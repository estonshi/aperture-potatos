// Package workers registers every goroutine a session starts, so close can wait for them and report the ones that never exit.
package workers

import (
	"context"
	"slices"
	"strings"
	"sync"
	"time"
)

// Info names one live goroutine and when it started.
type Info struct {
	Name    string
	Started time.Time
}

// Set is safe for concurrent use.
type Set struct {
	mu      sync.Mutex
	next    uint64
	live    map[uint64]Info
	changed chan struct{} // closed and replaced on every exit, so each waiter re-checks its own condition
}

func New() *Set {
	return &Set{live: map[uint64]Info{}, changed: make(chan struct{})}
}

// Go starts fn on a new goroutine that stays registered until fn returns.
func (s *Set) Go(name string, fn func()) {
	s.mu.Lock()
	s.next++
	id := s.next
	s.live[id] = Info{Name: name, Started: time.Now()}
	s.mu.Unlock()
	go func() {
		defer s.exit(id)
		fn()
	}()
}

func (s *Set) exit(id uint64) {
	s.mu.Lock()
	delete(s.live, id)
	close(s.changed)
	s.changed = make(chan struct{})
	s.mu.Unlock()
}

// Live lists the running goroutines, oldest first.
func (s *Set) Live() []Info {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.liveLocked(func(Info) bool { return true })
}

func (s *Set) liveLocked(match func(Info) bool) []Info {
	out := make([]Info, 0, len(s.live))
	for _, i := range s.live {
		if match(i) {
			out = append(out, i)
		}
	}
	slices.SortFunc(out, func(a, b Info) int {
		if c := a.Started.Compare(b.Started); c != 0 {
			return c
		}
		return strings.Compare(a.Name, b.Name)
	})
	return out
}

// Wait returns nil once no goroutine is left, or the stragglers when ctx ends first.
func (s *Set) Wait(ctx context.Context) []Info {
	return s.waitNone(ctx, func(Info) bool { return true })
}

// WaitPrefix waits until no goroutine whose name starts with one of prefixes is left.
func (s *Set) WaitPrefix(ctx context.Context, prefixes ...string) []Info {
	return s.waitNone(ctx, func(i Info) bool {
		return slices.ContainsFunc(prefixes, func(p string) bool { return strings.HasPrefix(i.Name, p) })
	})
}

// WaitExcept waits until only goroutines named exactly as one of names are left; the caller passes its own name.
func (s *Set) WaitExcept(ctx context.Context, names ...string) []Info {
	return s.waitNone(ctx, func(i Info) bool { return !slices.Contains(names, i.Name) })
}

func (s *Set) waitNone(ctx context.Context, match func(Info) bool) []Info {
	for {
		s.mu.Lock()
		left := s.liveLocked(match)
		changed := s.changed
		s.mu.Unlock()
		if len(left) == 0 {
			return nil
		}
		select {
		case <-changed:
		case <-ctx.Done():
			return left
		}
	}
}
