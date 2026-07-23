🇺🇸 English | 🇪🇸 [Español](README.es.md)

# SecureOnce

`SecureOnce` is an adaptation of Go's standard library `sync.Once`, designed for cases where you need to run something **exactly once** — like the original `Once` — but where that "once" **isn't entirely final**: if the conditions that allowed the `Once` to close stop being true, `SecureOnce` can reopen itself automatically, without you having to manage that state by hand.

It keeps (almost) the same speed as native `sync.Once` on the fast path, in exchange for a bit more memory and background computation.

## Motivation

`sync.Once` is extremely fast, but it's **blind**: once `Do` runs successfully, it stays closed forever, regardless of whether the condition that justified it is still valid.

`SecureOnce` adds a safety layer for those cases, built around two core ideas:

1. **Retry on error**: if the function fails, `SecureOnce` doesn't close. The next call retries it (unlike `sync.Once`, which is considered "spent" even if the caller handles the error manually inside).
2. **Asynchronous revalidation**: once closed, the *next* call triggers a background goroutine that evaluates an escape function (`s func() bool`). If that function determines the conditions for staying closed no longer hold, `SecureOnce` reopens, and a future call to `Do` will run the original function again.
3. **Concurrent-call coalescing (single-flight)**: while it's open, if several goroutines call `Do` at the same time, only one of them (the "leader") actually executes `f`. The rest ("followers") don't call `f` themselves — they wait and receive the exact same result the leader got. This avoids redundant executions of `f` when several callers race in while a previous attempt hasn't finished (or keeps failing).

### Why revalidate on the next call instead of the same one?

You might think it'd be better to detect and fix the error within the same call. The problem is that this would break the native speed that is the whole point of `Once`: adding revalidation logic to the execution thread itself forces you to block or wait for a result before returning control.

With this approach, the call that detects the need to revalidate **returns control immediately** (just like a normal `Once`), and revalidation happens in parallel, in a goroutine. The real cost is:

- More memory/computation from the goroutine and the extra atomic fields.
- One extra atomic check on the fast path (the `CompareAndSwap` in `checkReset`).

In exchange, you gain the ability to self-correct without manual intervention, while keeping the fast path practically at `sync.Once` level.

## Installation

``` bash
go get <module-path>/secureonce
```

## API

``` go
func (o *SecureOnce) Do(f func(*error), s func() bool) error
```

- **`f func(*error)`**: the function that runs exactly once (while the `Once` is "open"). It must report its result by writing to the error pointer it receives. If it writes `nil`, `SecureOnce` is considered closed. If it writes an error, it stays open and the next call retries it.
- **`s func() bool`**: the escape function (optional, can be `nil`). It's evaluated in the background, after the `Once` is already closed, to decide whether it should reopen. If it returns `true`, it reopens.

The zero value of `SecureOnce` is ready to use, just like `sync.Once`. No constructor needed.

### Step-by-step behavior

1. **While open** (`done == false`):

   - The internal `Mutex` is acquired briefly to decide the caller's role:
     - If no one else is currently executing `f` (`thereIsLeader` CAS from `false` to `true` succeeds), the caller becomes the **leader**: it releases the lock and runs `f` on its own, outside the mutex.
     - If someone else is already the leader, the caller becomes a **follower**: it registers a buffered channel in `channels`, releases the lock, and blocks waiting on that channel for the leader's result — it never calls `f` itself.
   - The leader's execution of `f` is wrapped in a `recover()`. If `f` panics, the panic is converted into the returned `error` (preserving it as-is if it was already an `error`, wrapping it otherwise).
   - If `f` reports `nil` (and didn't panic), it's marked as closed and, on this same success, the escape function `s` is stored (only the first time a non-nil one is received; later calls don't overwrite it).
   - If `f` reports an error (or panicked), that error is returned, but the `Once` stays open for the next attempt.
   - In all cases (success, error, or panic) the leader locks the mutex once more, broadcasts its result to every waiting follower's channel, clears `channels`, and releases the leader role (`thereIsLeader = false`) so a future call can become the new leader.

2. **Once closed** (`done == true`):

   - Subsequent calls take the fast path: a simple atomic read and `return nil`, without touching the mutex or running `f` again.
   - At that same step, if there isn't already a revalidation in progress, a goroutine is triggered (only once per cycle, thanks to the `CompareAndSwap` on `checkReset`) that runs the stored escape function.
   - If the escape function returns `true`, the `Once` reopens (`done` goes back to `false`) and a future call to `Do` will run `f` from scratch.

### Concurrency guarantees

- `SecureOnce` must not be copied after first use (just like `sync.Once`); it includes a `noCopy` field so `go vet` can catch this.
- The escape function is stored using `atomic.Pointer`, avoiding data races between whoever registers it and the goroutine that reads it.
- Only one revalidation goroutine can be in flight at a time, managed via `checkReset` (an `atomic.Bool` with `CompareAndSwap`).
- Only one goroutine can be the leader at a time, managed via `thereIsLeader` (an `atomic.Bool` with `CompareAndSwap`). Access to the `channels` slice (followers registering, leader broadcasting) is always done under the `Mutex`, so there's no data race there even though the leader runs `f` outside the lock.
- A panic inside `f` is recovered by the leader and turned into the returned `error`; it does not crash the caller, and `SecureOnce` stays open so the next call retries.
- A panic inside the escape function `s` is also recovered, but it is **not** turned into an error — it's only printed to stdout and swallowed. `SecureOnce` stays closed in that case. Keep `s` panic-free if you rely on it to trigger a reopen.

## Usage example

``` go
var once secureonce.SecureOnce

func loadConfig() error {
    return once.Do(
        func(err *error) {
            cfg, e := readConfigFromDisk()
            if e != nil {
                *err = e
                return
            }
            globalConfig = cfg
        },
        func() bool {
            // Escape logic: boolean only, lightweight.
            return configIsOutdated()
        },
    )
}
```

- As long as `readConfigFromDisk` fails, every call to `loadConfig` will retry loading the config.
- Once loaded successfully, subsequent calls are practically free.
- If at some point `configIsOutdated` returns `true`, the next call to `Do` (after the background revalidation) will run `readConfigFromDisk` again.

## ⚠️ Important: about the escape function (`s`)

The escape function runs in a dedicated goroutine, outside the main `Mutex`, every time a revalidation is triggered. Because of that, it's critical that it:

- **Contains only boolean logic**: a comparison, a flag read, an expiration check, etc.
- **Does not contain heavy business logic, blocking calls, or expensive I/O.**

If the escape function is too slow or expensive:

- The revalidation goroutine will take longer than necessary, delaying the moment the `Once` actually reopens.
- You can introduce **unpredictable desynchronization** between your system's real state and the state `SecureOnce` believes it has, since while the goroutine keeps evaluating, all concurrent calls keep taking the fast path as if nothing had changed.

In short: the escape function is a signal, not a place to do work.

## Trade-offs

|                                    | `sync.Once`         | `SecureOnce`                                                                 |
| ---------------------------------- | -------------------- | ----------------------------------------------------------------------------- |
| Fast path                          | 1 atomic read        | 1 atomic read + 1 `CompareAndSwap`                                             |
| Retry after error                  | No (stays "spent")   | Yes, on the next call                                                          |
| Self-recovery / reopening          | No                   | Yes, via async escape function                                                 |
| Concurrent calls while open        | Each blocks on the internal lock, only the first runs `f` | Coalesced via leader/follower: only the leader runs `f`, followers reuse its result |
| Panic inside `f`                    | Propagates to the caller | Recovered and returned as an `error`; `SecureOnce` stays open for retry        |
| Panic inside the escape function `s`| N/A                   | Recovered and logged to stdout, not surfaced as an error                       |
| Extra memory                       | —                     | Extra `atomic.Bool`s, `atomic.Pointer` for the escape function, and a slice of follower channels |
| Extra computation                   | —                     | Occasional revalidation goroutine                                              |

`SecureOnce` trades a bit of memory and background computation for safety and self-recovery, without giving up much of the fast-path speed that makes `Once` attractive in the first place.

## License

*Anzhi*
