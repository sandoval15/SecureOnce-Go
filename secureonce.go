package secureonce

import (
	"fmt"
	"sync"
	"sync/atomic"
)

type SecureOnce struct {
	_	noCopy

	done atomic.Bool
	checkReset atomic.Bool
	thereIsLeader atomic.Bool
	channels []chan(error)
	m sync.Mutex
	escapeFunc atomic.Pointer[func() bool]
}

func (o *SecureOnce) Do(f func(*error), s func() bool) (err error) {
	if o.done.Load() {
		if o.checkReset.CompareAndSwap(false, true) { go o.reset() } 
		return nil 
	}
	o.m.Lock()
	if o.channels == nil {
		o.channels = []chan(error){}
	}
	if !o.thereIsLeader.CompareAndSwap(false, true) {
		c := make(chan error, 1)
		o.channels = append(o.channels, c)
		o.m.Unlock()
		r := <- c
		return r
	}
	o.m.Unlock()
	defer func() {
		if r := recover(); r != nil {
			switch _r := r.(type) {
			case error:
				err = _r
			case string:
				err = fmt.Errorf("panic recovered: %v", _r)
			default:
				err = fmt.Errorf("Error inesperado en la funcion principal de Do: %v", _r)
			}
		}
		o.m.Lock()
		for _, c := range o.channels {
			c <- err
		}
		o.channels = []chan(error){}
		o.thereIsLeader.Store(false)
		o.m.Unlock()
	}()
	if !o.done.Load() {
		f(&err)
		if err == nil {
			if s != nil && o.escapeFunc.Load() == nil { o.escapeFunc.Store(&s) }
			o.done.Store(true)
		}
	}
	return err
}

func (o *SecureOnce) reset() {
	defer func() {
		o.checkReset.Store(false)
		if r := recover(); r != nil { fmt.Println("Error inesperado en funcion de escape: ", r) }
	}()
	if f := o.escapeFunc.Load(); f != nil {
		if fn := *f; fn() { o.escapeFunc.Store(nil); o.done.Store(false) }
	}
}

type noCopy struct {}

func (*noCopy) Lock() {}
func (*noCopy) Unlock() {}