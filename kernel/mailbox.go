package kernel

import "sync"

type msg interface{ isMsg() }

type mailbox struct {
	mu       sync.Mutex
	internal []msg
	external []msg
	maxExt   int           // < 0 means unbounded
	ready    chan struct{} // cap 1, level-triggered
	closed   bool
}

func newMailbox(maxExternal int) *mailbox {
	return &mailbox{maxExt: maxExternal, ready: make(chan struct{}, 1)}
}

// push never blocks; only external traffic is bounded, so worker exits can always be delivered.
func (m *mailbox) push(x msg, external bool) error {
	m.mu.Lock()
	switch {
	case m.closed:
		m.mu.Unlock()
		return ErrSessionClosed
	case external && m.maxExt >= 0 && len(m.external) >= m.maxExt:
		m.mu.Unlock()
		return ErrQueueFull
	case external:
		m.external = append(m.external, x)
	default:
		m.internal = append(m.internal, x)
	}
	m.mu.Unlock()
	select {
	case m.ready <- struct{}{}:
	default:
	}
	return nil
}

// pop prefers internal messages so commits and exits are never starved by a flooding caller.
func (m *mailbox) pop() (msg, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, q := range []*[]msg{&m.internal, &m.external} {
		if len(*q) > 0 {
			x := (*q)[0]
			(*q)[0] = nil
			*q = (*q)[1:]
			return x, true
		}
	}
	return nil, false
}

func (m *mailbox) close() {
	m.mu.Lock()
	m.closed = true
	m.mu.Unlock()
}
