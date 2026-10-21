package toolexec

import (
	"context"
	"sync"
	"time"

	"github.com/estonshi/aperture-potatos/message"
)

type moveKey struct{}

// MoveOf is the move handle of the movable call ctx belongs to, nil for any other call.
func MoveOf(ctx context.Context) *Move {
	m, _ := ctx.Value(moveKey{}).(*Move)
	return m
}

type moveState int

const (
	moveRunning moveState = iota
	moveClaimed
	moveMoved
)

// Move lets a running call leave its batch: the batch settles it with a stand-in result while the tool runs on, detached from the turn.
type Move struct {
	mu       sync.Mutex
	changed  *sync.Cond
	state    moveState
	returned bool
	pending  error // the turn's cancellation or the timeout, held while the call is claimed
	stop     bool  // the task's stop, held while the call is claimed
	ctx      *detached
	stopTurn func() bool
	timer    *time.Timer
	began    time.Time
	r        Runner
	call     message.ToolCallBlock
	sl       *slot
}

// newMove forwards the turn's cancellation and the call's timeout to a context of its own, so a move can stop forwarding them.
func newMove(ctx context.Context, r Runner, call message.ToolCallBlock, sl *slot, limit time.Duration) *Move {
	m := &Move{began: time.Now(), r: r, call: call, sl: sl}
	m.changed = sync.NewCond(&m.mu)
	m.ctx = &detached{Context: context.WithoutCancel(ctx), done: make(chan struct{}), move: m}
	m.stopTurn = context.AfterFunc(ctx, func() { m.forward(context.Canceled) })
	if ctx.Err() != nil { // AfterFunc runs on its own goroutine, so a turn already over must end the call before the tool starts
		m.ctx.end(context.Canceled)
	}
	if limit >= 0 {
		m.ctx.deadline = m.began.Add(limit)
		m.timer = time.AfterFunc(limit, func() { m.forward(context.DeadlineExceeded) })
	}
	return m
}

func (m *Move) forward(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	switch m.state {
	case moveRunning:
		m.ctx.end(err)
	case moveClaimed:
		if m.pending == nil {
			m.pending = err
		}
	}
}

// Claim holds the call for a move; false when the tool has returned or the call is already ending.
func (m *Move) Claim() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state != moveRunning || m.returned || m.ctx.Err() != nil {
		return false
	}
	m.state = moveClaimed
	return true
}

// Release gives a claimed call back to its batch, and an end that arrived meanwhile takes effect.
func (m *Move) Release() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state = moveRunning
	if m.pending != nil {
		m.ctx.end(m.pending)
		m.pending = nil
	}
	m.changed.Broadcast()
}

// Commit settles the claimed call with res and detaches it: the turn no longer ends it, and only keep, a timeout the tool declares itself, still does.
func (m *Move) Commit(res message.ToolResultBlock, keep time.Duration) {
	if m.sl.settled.CompareAndSwap(false, true) { // nothing ends a claimed call, so the collector cannot have settled it
		m.sl.release()
		m.r.Completed(m.call, res)
		m.sl.res <- res
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.state, m.pending = moveMoved, nil
	m.stopTurn()
	if m.timer != nil {
		m.timer.Stop()
	}
	m.ctx.setDeadline(time.Time{})
	if keep > 0 {
		at := m.began.Add(keep)
		m.ctx.setDeadline(at)
		m.timer = time.AfterFunc(time.Until(at), func() { m.ctx.end(context.DeadlineExceeded) })
	}
	if m.stop {
		m.ctx.end(context.Canceled)
	}
	m.changed.Broadcast()
}

// Returned reports, once the tool has returned, whether its result belongs to the task the call moved to; it waits out a claim.
func (m *Move) Returned() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.returned = true
	for m.state == moveClaimed {
		m.changed.Wait()
	}
	return m.state == moveMoved
}

// Cancel ends a moved call, as its task's stop does; the tool sees context.Canceled, as the Context contract requires.
func (m *Move) Cancel() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.state == moveClaimed {
		m.stop = true
		return
	}
	m.ctx.end(context.Canceled)
}

// Moved reports that the call now belongs to a task.
func (m *Move) Moved() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.state == moveMoved
}

// close is the worker's cleanup once the tool returned.
func (m *Move) close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.stopTurn()
	if m.timer != nil {
		m.timer.Stop()
	}
	if m.state != moveMoved {
		m.ctx.end(context.Canceled)
	}
}

// detached carries the call's values but ends only when its move ends it, so a moved call outlives its turn.
type detached struct {
	context.Context
	move     *Move
	done     chan struct{}
	mu       sync.Mutex
	err      error
	deadline time.Time
}

func (d *detached) Deadline() (time.Time, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.deadline, !d.deadline.IsZero()
}

func (d *detached) Done() <-chan struct{} { return d.done }

func (d *detached) Err() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.err
}

func (d *detached) Value(key any) any {
	if key == (moveKey{}) {
		return d.move
	}
	return d.Context.Value(key)
}

func (d *detached) end(err error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.err == nil {
		d.err = err
		close(d.done)
	}
}

func (d *detached) setDeadline(t time.Time) {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.deadline = t
}
