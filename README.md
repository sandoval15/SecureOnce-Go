# SecureOnce

`SecureOnce` es una adaptación de `sync.Once` de la librería estándar de Go, pensada para los casos en los que necesitás ejecutar algo **una sola vez**, como con el `Once` original, pero donde ese "una sola vez" **no es del todo definitivo**: si las condiciones que permitieron cerrar el `Once` dejan de cumplirse, `SecureOnce` puede reabrirse solo, sin que tengas que gestionar ese estado a mano.

Mantiene (casi) la misma velocidad del `sync.Once` nativo en el fast path, a cambio de un poco más de memoria y cómputo en segundo plano.

## Motivación

`sync.Once` es rapidísimo, pero es **ciego**: una vez que `Do` se ejecuta con éxito, queda cerrado para siempre, sin importar si la condición que lo justificaba sigue siendo válida.

`SecureOnce` agrega una capa de seguridad para esos casos, con dos ideas centrales:

1. **Reintento en caso de error**: si la función a ejecutar falla, el `SecureOnce` no se cierra. La próxima llamada lo vuelve a intentar (a diferencia de `sync.Once`, que se considera "gastado" incluso si el usuario maneja el error manualmente adentro).
2. **Revalidación asíncrona**: una vez cerrado, en la *siguiente* llamada se dispara una goroutine en segundo plano que evalúa una función de escape (`s func() bool`). Si esa función determina que ya no se dan las condiciones para seguir cerrado, `SecureOnce` se reabre, y una futura llamada volverá a ejecutar la función original.

### ¿Por qué revalidar en la llamada siguiente y no en la misma?

Se podría pensar que sería mejor detectar el error y solucionarlo en la misma llamada. El problema es que eso rompería la velocidad nativa que es justamente la razón de ser de `Once`: agregar lógica de revalidación en el propio hilo de ejecución te obliga a bloquear o esperar un resultado antes de devolver el control.

Con este enfoque, la llamada que detecta la necesidad de revalidar **devuelve el control inmediatamente** (igual que un `Once` normal) y la revalidación ocurre en paralelo, en una goroutine. El costo real es:

- Más memoria/cómputo por la goroutine y por los campos atómicos extra.
- Una comprobación atómica adicional en el fast path (el `CompareAndSwap` de `checkReset`).

A cambio, se gana la capacidad de autocorregirse sin intervención manual, manteniendo el fast path prácticamente al nivel de `sync.Once`.

## Instalación

```bash
go get <ruta-del-modulo>/secureonce
```

## API

```go
func (o *SecureOnce) Do(f func(*error), s func() bool) error
```

- **`f func(*error)`**: la función que se ejecuta una única vez (mientras el `Once` esté "abierto"). Debe reportar el resultado escribiendo en el puntero a error que recibe. Si escribe `nil`, `SecureOnce` se considera cerrado. Si escribe un error, se mantiene abierto y la próxima llamada lo vuelve a intentar.
- **`s func() bool`**: la función de escape (opcional, puede ser `nil`). Se evalúa en segundo plano, después de que el `Once` ya está cerrado, para decidir si corresponde reabrirlo. Si devuelve `true`, se reabre.

El valor cero de `SecureOnce` ya está listo para usarse, igual que `sync.Once`. No necesita constructor.

### Comportamiento paso a paso

1. **Mientras está abierto** (`done == false`):
   - Se toma el `Mutex` interno.
   - Si todavía sigue abierto (doble chequeo dentro del lock), se guarda la función de escape `s` (solo la primera vez que se recibe una no nula; llamadas posteriores no la sobrescriben) y se ejecuta `f`.
   - Si `f` reporta `nil`, se marca como cerrado.
   - Si `f` reporta un error, ese error se devuelve, pero el `Once` sigue abierto para el próximo intento.

2. **Una vez cerrado** (`done == true`):
   - Las llamadas siguientes toman el fast path: una simple lectura atómica y `return nil`, sin tocar el mutex ni volver a ejecutar `f`.
   - En ese mismo paso, si no hay ya una revalidación en curso, se dispara (una sola vez por ciclo, gracias al `CompareAndSwap` sobre `checkReset`) una goroutine que ejecuta la función de escape guardada.
   - Si la función de escape devuelve `true`, el `Once` se reabre (`done` vuelve a `false`) y una futura llamada a `Do` volverá a ejecutar `f` desde cero.

### Garantías de concurrencia

- `SecureOnce` no se debe copiar después de usarse (igual que `sync.Once`); incluye un campo `noCopy` para que `go vet` lo detecte.
- La función de escape se guarda con `atomic.Pointer`, evitando data races entre quien la registra y la goroutine que la lee.
- Solo una goroutine de revalidación puede estar en vuelo a la vez, gestionada mediante `checkReset` (un `atomic.Bool` con `CompareAndSwap`).

## Ejemplo de uso

```go
var once secureonce.SecureOnce

func cargarConfiguracion() error {
    return once.Do(
        func(err *error) {
            cfg, e := leerConfigDesdeDisco()
            if e != nil {
                *err = e
                return
            }
            configGlobal = cfg
        },
        func() bool {
            // Lógica de escape: solo booleana, liviana.
            return configuracionDesactualizada()
        },
    )
}
```

- Mientras `leerConfigDesdeDisco` falle, cada llamada a `cargarConfiguracion` reintentará cargar la config.
- Una vez cargada con éxito, las llamadas siguientes son prácticamente gratis.
- Si en algún momento `configuracionDesactualizada` devuelve `true`, la próxima llamada a `Do` (después de la revalidación en segundo plano) volverá a ejecutar `leerConfigDesdeDisco`.

## ⚠️ Importante: sobre la función de escape (`s`)

La función de escape se ejecuta en una goroutine dedicada, fuera del `Mutex` principal, cada vez que se detecta que corresponde revalidar. Por eso es crítico que:

- **Contenga únicamente lógica booleana**: una comparación, una lectura de un flag, un chequeo de expiración, etc.
- **No contenga lógica de negocio pesada, llamadas bloqueantes o I/O costoso.**

Si la función de escape es muy lenta o costosa:

- La goroutine de revalidación demorará más de lo necesario, retrasando el momento en que el `Once` realmente se reabre.
- Puedes introducir **desincronizaciones impredecibles** entre el estado real de tu sistema y el estado que `SecureOnce` cree tener, ya que mientras la goroutine sigue evaluando, todas las llamadas concurrentes siguen tomando el fast path como si nada hubiera cambiado.

En resumen: la función de escape es una señal, no un lugar para trabajar.

## Trade-offs

| | `sync.Once` | `SecureOnce` |
| --- | --- | --- |
| Fast path | 1 lectura atómica | 1 lectura atómica + 1 `CompareAndSwap` |
| Reintento tras error | No (queda "gastado") | Sí, en la próxima llamada |
| Autorrecuperación / reapertura | No | Sí, vía función de escape asíncrona |
| Memoria extra | — | `atomic.Bool` extra + `atomic.Pointer` para la función de escape |
| Cómputo extra | — | Goroutine ocasional de revalidación |

`SecureOnce` cambia algo de memoria y cómputo en segundo plano por seguridad y autorrecuperación, sin resignar en gran medida la velocidad del fast path que hace atractivo a `Once` en primer lugar.

## Licencia

_Anzhi_"# SecureOnce-Go" 
