// Package bus is a session's event bus: one queue per subscriber, a pump per sink, and credit-based backpressure on bulk producers.
package bus

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/estonshi/aperture-potatos/event"
)

var (
	ErrStalled       = errors.New("subscriber stalled")
	ErrOverflow      = errors.New("subscriber queue overflow")
	ErrChannelClosed = errors.New("subscriber channel closed by its owner")
	ErrClosed        = errors.New("bus closed")
)

type Policy uint8

const (
	Block      Policy = iota // lossless; bulk producers wait for this subscriber
	DropDeltas               // bulk events are dropped while the queue is at or above HighWater
)

// Options are complete: the caller fills every default before subscribing.
type Options struct {
	Policy       Policy
	HighWater    int
	HardLimit    int
	StallTimeout time.Duration // < 0 disables stall detection
	Types        []string      // event type prefixes; empty means all
	MaxDepth     int           // max len(Path); < 0 means unlimited
	Native       bool          // receive the native variant of an event that has one
}

type Bus struct {
	mu       sync.Mutex
	seq      uint64
	subs     []*Sub
	credit   chan struct{} // closed and replaced whenever a blocking subscriber drains to its low water mark
	waiting  bool
	closed   bool
	envelope func(event.Payload) *event.Event
}

// New takes the envelope builder the bus uses for the events it emits itself, such as subscriber_detached.
func New(envelope func(event.Payload) *event.Event) *Bus {
	return &Bus{credit: make(chan struct{}), envelope: envelope}
}

type Sub struct {
	opts     Options
	mu       sync.Mutex
	q        []*event.Event
	notify   chan struct{} // cap 1
	lastPop  time.Time
	dropped  uint64
	closing  bool  // the bus closed: drain the queue, then stop
	failed   error // set once the subscription is cut off; nil after a plain unsubscribe
	stopped  bool
	quit     chan struct{} // closed when the subscription is cut off, so a pump blocked on its sink gives up
	done     chan struct{}
	doneOnce sync.Once
}

// Subscribe adds a subscriber that receives every event published from now on.
func (b *Bus) Subscribe(o Options) *Sub {
	s := &Sub{opts: o, notify: make(chan struct{}, 1), lastPop: time.Now(), quit: make(chan struct{}), done: make(chan struct{})}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		s.closing = true
		return s
	}
	b.subs = append(b.subs, s)
	return s
}

// Seq is the seq of the last event published, so a relay can tell when it has caught up.
func (b *Bus) Seq() uint64 {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.seq
}

// Publish assigns the next seq and enqueues ev for every matching subscriber; it never blocks.
func (b *Bus) Publish(ev *event.Event) { b.publish(ev, nil) }

// PublishNative is Publish for an event with a second, native form that subscribers with Options.Native receive under the same seq.
func (b *Bus) PublishNative(ev, native *event.Event) { b.publish(ev, native) }

func (b *Bus) publish(ev, native *event.Event) {
	var victims []*Sub
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.seq++
	ev.Seq = b.seq
	if native != nil {
		native.Seq = b.seq
	}
	bulk := isBulk(ev.Data)
	for _, s := range b.subs {
		e := ev
		if native != nil && s.opts.Native {
			e = native
		}
		if s.matches(e) && !s.offer(e, bulk) {
			victims = append(victims, s)
		}
	}
	b.mu.Unlock()
	for _, s := range victims {
		b.Detach(s, ErrOverflow)
	}
}

func isBulk(p event.Payload) bool {
	bp, ok := p.(event.Bulk)
	return ok && bp.Bulk()
}

func (s *Sub) matches(ev *event.Event) bool {
	if s.opts.MaxDepth >= 0 && len(ev.Path) > s.opts.MaxDepth {
		return false
	}
	return len(s.opts.Types) == 0 || slices.ContainsFunc(s.opts.Types, func(p string) bool { return strings.HasPrefix(string(ev.Type), p) })
}

// offer reports false only when the hard limit is exceeded.
func (s *Sub) offer(ev *event.Event, bulk bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return true
	}
	n := len(s.q)
	switch {
	case bulk && s.opts.Policy == DropDeltas && n >= s.opts.HighWater:
		s.dropped++
		return true
	case n >= s.opts.HardLimit:
		return false
	}
	s.q = append(s.q, ev)
	s.signal()
	return true
}

func (s *Sub) signal() {
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// WaitWritable blocks a bulk producer until every blocking subscriber is below its high water mark.
func (b *Bus) WaitWritable(ctx context.Context) error {
	waitStart := time.Now()
	for {
		b.mu.Lock()
		if b.closed {
			b.mu.Unlock()
			return ErrClosed
		}
		full := b.firstFullLocked()
		if full == nil {
			b.mu.Unlock()
			return nil
		}
		b.waiting = true
		credit := b.credit
		b.mu.Unlock()

		// The stall clock starts at the later of the last pop and this wait, so an idle-then-bursty subscriber is not detached.
		budget, bounded := full.stallBudget(time.Now(), waitStart)
		if bounded && budget <= 0 {
			b.Detach(full, ErrStalled)
			continue
		}
		var stall <-chan time.Time
		var timer *time.Timer
		if bounded {
			timer = time.NewTimer(budget)
			stall = timer.C
		}
		select {
		case <-credit:
		case <-stall:
		case <-ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return context.Cause(ctx)
		}
		if timer != nil {
			timer.Stop()
		}
	}
}

func (b *Bus) firstFullLocked() *Sub {
	for _, s := range b.subs {
		if s.opts.Policy != Block {
			continue
		}
		s.mu.Lock()
		full := len(s.q) >= s.opts.HighWater
		s.mu.Unlock()
		if full {
			return s
		}
	}
	return nil
}

func (s *Sub) stallBudget(now, waitStart time.Time) (time.Duration, bool) {
	if s.opts.StallTimeout < 0 {
		return 0, false
	}
	s.mu.Lock()
	from := s.lastPop
	s.mu.Unlock()
	if waitStart.After(from) {
		from = waitStart
	}
	return s.opts.StallTimeout - now.Sub(from), true
}

// Pop is called by exactly one consumer per subscriber; it reports false once the subscription has ended and, after a bus close, its queue is drained.
func (s *Sub) Pop(ctx context.Context, b *Bus) (*event.Event, bool) {
	for {
		s.mu.Lock()
		if s.stopped {
			s.mu.Unlock()
			return nil, false
		}
		if len(s.q) > 0 {
			ev := s.q[0]
			s.q[0] = nil
			s.q = s.q[1:]
			n := len(s.q)
			s.lastPop = time.Now()
			s.mu.Unlock()
			if s.opts.Policy == Block && n <= s.opts.HighWater/2 {
				b.returnCredit()
			}
			return ev, true
		}
		closing := s.closing
		s.mu.Unlock()
		if closing {
			return nil, false
		}
		select {
		case <-s.notify:
		case <-ctx.Done():
			return nil, false
		}
	}
}

func (b *Bus) returnCredit() {
	b.mu.Lock()
	b.returnCreditLocked()
	b.mu.Unlock()
}

func (b *Bus) returnCreditLocked() {
	if b.waiting {
		close(b.credit)
		b.credit = make(chan struct{})
		b.waiting = false
	}
}

// Unsubscribe stops delivery without an error; it never blocks.
func (b *Bus) Unsubscribe(s *Sub) { b.cut(s, nil, false) }

// Detach cuts s off with cause and tells the remaining subscribers, so nobody keeps waiting on a subscriber that is gone.
func (b *Bus) Detach(s *Sub, cause error) { b.cut(s, cause, true) }

func (b *Bus) cut(s *Sub, cause error, announce bool) {
	b.mu.Lock()
	n := len(b.subs)
	b.subs = slices.DeleteFunc(b.subs, func(x *Sub) bool { return x == s })
	removed := len(b.subs) < n
	b.returnCreditLocked()
	b.mu.Unlock()
	if !s.stop(cause) {
		return
	}
	if announce && removed && b.envelope != nil {
		b.Publish(b.envelope(&event.Error{Code: "subscriber_detached", Message: cause.Error()}))
	}
}

// stop reports whether this call ended the subscription.
func (s *Sub) stop(cause error) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopped {
		return false
	}
	s.stopped, s.failed, s.q = true, cause, nil
	close(s.quit)
	s.signal()
	return true
}

// Close lets every subscriber drain its queue and then end; publishing afterwards is a no-op.
func (b *Bus) Close() {
	b.mu.Lock()
	b.closed = true
	subs := b.subs
	b.subs = nil
	b.returnCreditLocked()
	b.mu.Unlock()
	for _, s := range subs {
		s.mu.Lock()
		s.closing = true
		s.signal()
		s.mu.Unlock()
	}
}

// Err is nil after an unsubscribe, ErrClosed after the bus closed, or why the subscriber was detached.
func (s *Sub) Err() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.failed != nil:
		return s.failed
	case s.closing && !s.stopped:
		return ErrClosed
	}
	return nil
}

func (s *Sub) Dropped() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.dropped
}

// Done is closed by Finish, which the consumer calls once it will never touch the sink again.
func (s *Sub) Done() <-chan struct{} { return s.done }

func (s *Sub) Finish() { s.doneOnce.Do(func() { close(s.done) }) }
