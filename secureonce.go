package secureonce

import (
	"sync"
	"sync/atomic"
)

type SecureOnce struct {
	_	noCopy

	done atomic.Bool
	checkReset atomic.Bool
	m    sync.Mutex
	escapeFunc atomic.Pointer[func() bool]
}

func (o *SecureOnce) Do(f func(*error), s func()(bool)) (err error) {
	if o.done.Load() {
		if o.checkReset.CompareAndSwap(false, true) { go o.reset() } 
		return nil 
	}
	o.m.Lock()
	defer o.m.Unlock()
	if !o.done.Load() {
		if s != nil && o.escapeFunc.Load() == nil { o.escapeFunc.Store(&s) }
		f(&err)
		if err == nil { o.done.Store(true) }
	}
	return err
}

func (o *SecureOnce) reset() {
	defer o.checkReset.Store(false)
	if f := o.escapeFunc.Load(); f != nil {
		if fn := *f; fn() { o.done.Store(false) }
	}
}

type noCopy struct {}

func (*noCopy) Lock() {}
func (*noCopy) Unlock() {}