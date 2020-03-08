package goroutine

import (
	"math"
	"sync"

	"github.com/xaionaro-go/rand/mathrand"

	"github.com/xaionaro-go/spinlock"
)

const (
	blockedByWriter = -math.MaxInt64 + 1
)

type RWMutex struct {
	lazyInitOnce sync.Once

	usedDone         chan struct{}
	monopolizedDone  chan struct{}
	monopolizedDepth int
	monopolizedBy    *g

	state          int64
	backendLocker  sync.Mutex
	internalLocker spinlock.Locker
	usedBy         map[*g]*int64
}

func (m *RWMutex) lazyInit() {
	m.lazyInitOnce.Do(func() {
		m.usedBy = map[*g]*int64{}
	})
}

func (m *RWMutex) Lock() {
	m.lazyInit()
	me := GetG()

	for {
		var monopolizedBy *g
		m.internalLocker.Lock()
		if m.monopolizedBy == nil {
			m.monopolizedBy = me
			monopolizedBy = me
			m.monopolizedDepth++
			m.internalLocker.Unlock()
			m.backendLocker.Lock()
			m.setStateBlockedByWriter(me)
			return
		} else {
			monopolizedBy = m.monopolizedBy
		}
		monopolizedByMe := monopolizedBy == me
		var ch chan struct{}
		if !monopolizedByMe {
			if m.monopolizedDone == nil {
				m.monopolizedDone = make(chan struct{})
			}
			ch = m.monopolizedDone
		}
		if monopolizedByMe {
			m.monopolizedDepth++
		}
		m.internalLocker.Unlock()
		if monopolizedByMe {
			return
		}
		select {
		case <-ch:
		}
	}
}

func (m *RWMutex) Unlock() {
	me := GetG()

	m.internalLocker.Lock()
	if me != m.monopolizedBy {
		m.internalLocker.Unlock()
		panic("I'm not the one, who locked this mutex")
	}
	m.monopolizedDepth--
	if m.monopolizedDepth == 0 {
		m.monopolizedBy = nil
		m.backendLocker.Unlock()
		m.state -= blockedByWriter
	}

	chPtr := m.monopolizedDone
	m.monopolizedDone = nil
	m.internalLocker.Unlock()
	if chPtr == nil {
		return
	}
	close(chPtr)

}

func (m *RWMutex) LockDo(fn func()) {
	m.Lock()
	defer m.Unlock()

	fn()
}

func (m *RWMutex) setStateBlockedByWriter(me *g) {
	m.internalLocker.Lock()
	defer m.internalLocker.Unlock()
	for {
		m.state += blockedByWriter
		if m.state == blockedByWriter {
			return
		}
		if myReadersCountPtr, _ := m.usedBy[me]; myReadersCountPtr != nil {
			if m.state-*myReadersCountPtr == blockedByWriter {
				return
			}
		}
		m.state -= blockedByWriter

		if m.usedDone == nil {
			m.usedDone = make(chan struct{})
		}
		ch := m.usedDone
		m.internalLocker.Unlock()
		select {
		case <-ch:
		}
		m.internalLocker.Lock()
	}
}

type int64PoolT []*int64

func (pool *int64PoolT) Put(v *int64) {
	*v = 1
	*pool = append(*pool, v)
}

func (pool *int64PoolT) Get() *int64 {
	if len(*pool) == 0 {
		for i := 0; i < 100; i++ {
			pool.Put(&[]int64{1}[0])
		}
	}

	idx := len(*pool) - 1
	v := (*pool)[idx]
	*pool = (*pool)[:idx]

	return v
}

var int64Pool = &int64PoolT{}

func (m *RWMutex) incMyReaders(me *g) {
	if v := m.usedBy[me]; v == nil {
		m.usedBy[me] = int64Pool.Get()
	} else {
		*v++
	}
}

var prng = mathrand.New()

func (m *RWMutex) gc() {
	if prng.Uint32MultiplyAdd()>>24 != 0 {
		return
	}

	for k, v := range m.usedBy {
		if *v != 0 {
			continue
		}
		delete(m.usedBy, k)
	}
}

func (m *RWMutex) decMyReaders(me *g) {
	v := m.usedBy[me]
	*v--
	if *v != 0 {
		return
	}
	m.gc()
	ch := m.usedDone
	if ch == nil {
		return
	}
	close(ch)
	m.usedDone = nil
	int64Pool.Put(v)
}

func (m *RWMutex) RLock() {
	m.lazyInit()
	me := GetG()

	for {
		m.internalLocker.Lock()
		m.state++
		isOK := m.state > 0
		if isOK {
			break
		}
		monopolizedBy := m.monopolizedBy
		if monopolizedBy == me {
			break
		}

		m.state--
		if m.monopolizedDone == nil {
			m.monopolizedDone = make(chan struct{})
		}
		ch := m.monopolizedDone
		m.internalLocker.Unlock()

		select {
		case <-ch:
		}
	}

	m.incMyReaders(me)
	m.internalLocker.Unlock()

}

func (m *RWMutex) RUnlock() {
	me := GetG()

	m.internalLocker.Lock()
	m.state--
	m.decMyReaders(me)
	m.internalLocker.Unlock()
}

func (m *RWMutex) RLockDo(fn func()) {
	m.RLock()
	defer m.RUnlock()

	fn()
}