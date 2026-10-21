package memory

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
)

type inMemory struct {
	mu    sync.Mutex
	items []Item
	next  int
}

// NewInMemory is the reference store, in memory, for tests and examples.
func NewInMemory() Store { return &inMemory{} }

func (m *inMemory) List(_ context.Context, scope Scope) ([]Item, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Item
	for _, it := range m.items {
		if matches(it, scope) {
			out = append(out, it)
		}
	}
	return out, nil
}

func (m *inMemory) Apply(_ context.Context, scope Scope, changes []Change) ([]Change, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	next, n := slices.Clone(m.items), m.next
	done := make([]Change, 0, len(changes))
	for _, c := range changes {
		i := slices.IndexFunc(next, func(it Item) bool { return it.ID == c.ID && matches(it, scope) })
		switch c.Op {
		case "add":
			n++
			c.ID = fmt.Sprintf("mem_%d", n)
			next = append(next, Item{ID: c.ID, Content: strings.TrimSpace(deref(c.Content)), User: deref(c.User), Project: deref(c.Project), Author: scope.Agent, Timestamp: time.Now()})
		case "update":
			if i < 0 {
				return nil, fmt.Errorf("%w: %s", ErrNotFound, c.ID)
			}
			if c.Content != nil {
				next[i].Content = strings.TrimSpace(*c.Content)
			}
			if c.User != nil {
				next[i].User = *c.User
			}
			if c.Project != nil {
				next[i].Project = *c.Project
			}
			next[i].Timestamp = time.Now()
		case "delete":
			if i < 0 {
				return nil, fmt.Errorf("%w: %s", ErrNotFound, c.ID)
			}
			next = slices.Delete(next, i, i+1)
		default:
			return nil, fmt.Errorf("memory: unknown op %q", c.Op)
		}
		done = append(done, c)
	}
	m.items, m.next = next, n
	return done, nil
}
