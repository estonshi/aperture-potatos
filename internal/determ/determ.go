// Package determ provides the fixed clock and id sequence that conformance scripts ask for ("clock": {start, step_ms}, "ids": "sequential").
package determ

import (
	"fmt"
	"sync"
	"time"
)

// StepClock returns start on the first call and advances by step on every later call, so each timestamp in a run is distinct and reproducible.
type StepClock struct {
	mu   sync.Mutex
	next time.Time
	step time.Duration
}

func NewStepClock(start time.Time, step time.Duration) *StepClock {
	return &StepClock{next: start.UTC(), step: step}
}

func (c *StepClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	t := c.next
	c.next = c.next.Add(c.step)
	return t
}

// SequentialIDs numbers ids per prefix from 1 ("msg_01", "msg_02", "ses_01"), matching the ids in the spec's example files.
type SequentialIDs struct {
	mu sync.Mutex
	n  map[string]int
}

func NewSequentialIDs() *SequentialIDs { return &SequentialIDs{n: map[string]int{}} }

func (g *SequentialIDs) NewID(prefix string) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.n[prefix]++
	return fmt.Sprintf("%s_%02d", prefix, g.n[prefix])
}
