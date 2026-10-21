package toolexec

import (
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/estonshi/aperture-potatos/message"
)

// moveRunner hands each movable call's Move to the test and records what its tool saw.
type moveRunner struct {
	runner
	moves  chan *Move
	mu     sync.Mutex
	ended  map[string]error // the call's context error when its tool returned
	moved  map[string]bool
	took   map[string]time.Duration
	tookAt time.Time
}

func (r *moveRunner) RunOne(ctx context.Context, c message.ToolCallBlock) message.ToolResultBlock {
	m := MoveOf(ctx)
	if m != nil {
		r.moves <- m
	}
	res := r.runner.RunOne(ctx, c)
	r.mu.Lock()
	r.ended[c.ID] = ctx.Err()
	r.took[c.ID] = time.Since(r.tookAt)
	r.mu.Unlock()
	if m != nil && m.Returned() {
		r.mu.Lock()
		r.moved[c.ID] = true
		r.mu.Unlock()
		return message.ToolResultBlock{}
	}
	return res
}

func newMoveRunner(behave map[string]func(context.Context)) *moveRunner {
	return &moveRunner{runner: runner{behave: behave}, moves: make(chan *Move, 4), ended: map[string]error{}, moved: map[string]bool{}, took: map[string]time.Duration{}, tookAt: time.Now()}
}

func movable(c message.ToolCallBlock) bool { return c.ID == "m" }

var standIn = message.ToolResultBlock{CallID: "m", Outcome: message.OutcomeSuccess, Content: message.Content{&message.TextBlock{Text: "moved"}}}

func TestMoveDetachesFromTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newMoveRunner(map[string]func(context.Context){
			"m": func(ctx context.Context) { <-ctx.Done() },
			"b": func(context.Context) {},
		})
		ctx, cancel := context.WithCancel(t.Context())
		go func() {
			m := <-r.moves
			time.Sleep(time.Second)
			if !m.Claim() {
				t.Error("claim failed on a running call")
			}
			m.Commit(standIn, 0)
			time.Sleep(time.Second)
			cancel() // the turn ends; the moved call must not
			time.Sleep(time.Second)
			m.Cancel()
		}()
		res := Run(ctx, r, calls("m", "b"), Options{Grace: time.Second, Timeout: func(message.ToolCallBlock) time.Duration { return time.Hour }, Movable: movable})
		if res[0].Content == nil || res[1].Outcome != message.OutcomeSuccess {
			t.Fatalf("results %+v", res)
		}
		synctest.Wait()
		time.Sleep(3 * time.Second)
		synctest.Wait()
		r.mu.Lock()
		defer r.mu.Unlock()
		if !r.moved["m"] || r.ended["m"] != context.Canceled || r.took["m"] != 3*time.Second { // a task's stop looks like any cancellation
			t.Fatalf("moved %v ended %v after %v", r.moved, r.ended["m"], r.took["m"])
		}
		if len(r.completed) != 2 || r.completed[0] != "m" {
			t.Fatalf("completed %v", r.completed)
		}
	})
}

func TestMoveReleaseAppliesHeldCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newMoveRunner(map[string]func(context.Context){"m": func(ctx context.Context) { <-ctx.Done() }})
		ctx, cancel := context.WithCancel(t.Context())
		go func() {
			m := <-r.moves
			m.Claim()
			cancel()
			time.Sleep(time.Second) // the interrupt waits for the move's outcome
			m.Release()
		}()
		res := Run(ctx, r, calls("m"), Options{Grace: time.Hour, Timeout: noTimeout, Movable: movable})
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.moved["m"] || !errors.Is(r.ended["m"], context.Canceled) || r.took["m"] != time.Second || res[0].Outcome != message.OutcomeSuccess {
			t.Fatalf("moved %v ended %v after %v, result %+v", r.moved, r.ended["m"], r.took["m"], res[0])
		}
	})
}

func TestMoveKeepsDeclaredTimeout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newMoveRunner(map[string]func(context.Context){"m": func(ctx context.Context) { <-ctx.Done() }})
		go func() {
			m := <-r.moves
			time.Sleep(time.Second)
			m.Claim()
			m.Commit(standIn, 5*time.Second) // the tool's own timeout still counts from the call's start
		}()
		Run(t.Context(), r, calls("m"), Options{Grace: time.Second, Timeout: func(message.ToolCallBlock) time.Duration { return 2 * time.Second }, Movable: movable})
		time.Sleep(time.Hour)
		synctest.Wait()
		r.mu.Lock()
		defer r.mu.Unlock()
		if !r.moved["m"] || !errors.Is(r.ended["m"], context.DeadlineExceeded) || r.took["m"] != 5*time.Second {
			t.Fatalf("moved %v ended %v after %v", r.moved, r.ended["m"], r.took["m"])
		}
	})
}

func TestMoveTimeoutLooksAsBefore(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newMoveRunner(map[string]func(context.Context){"m": func(ctx context.Context) {
			if _, ok := ctx.Deadline(); !ok {
				t.Error("a movable call lost its deadline")
			}
			<-ctx.Done()
		}})
		go func() { <-r.moves }()
		Run(t.Context(), r, calls("m"), Options{Grace: time.Second, Timeout: func(message.ToolCallBlock) time.Duration { return 2 * time.Second }, Movable: movable})
		r.mu.Lock()
		defer r.mu.Unlock()
		if r.moved["m"] || r.ended["m"] != context.DeadlineExceeded || r.took["m"] != 2*time.Second {
			t.Fatalf("moved %v ended %v after %v", r.moved, r.ended["m"], r.took["m"])
		}
	})
}

func TestClaimFailsOnceReturnedOrEnding(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newMoveRunner(map[string]func(context.Context){"m": func(context.Context) {}})
		Run(t.Context(), r, calls("m"), Options{Grace: time.Second, Timeout: noTimeout, Movable: movable})
		if m := <-r.moves; m.Claim() {
			t.Fatal("claimed a call whose tool returned")
		}
		r = newMoveRunner(map[string]func(context.Context){"m": func(ctx context.Context) { <-ctx.Done(); time.Sleep(time.Minute) }}) // still running, but ending
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan struct{})
		go func() {
			m := <-r.moves
			cancel()
			synctest.Wait()
			if m.Claim() {
				t.Error("claimed a call whose turn was interrupted")
			}
			close(done)
		}()
		Run(ctx, r, calls("m"), Options{Grace: time.Hour, Timeout: noTimeout, Movable: movable})
		<-done
	})
}

func TestMoveStartsEndedWhenTurnIsOver(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		cancel()
		sl := &slot{res: make(chan message.ToolResultBlock, 1), expired: make(chan struct{}), release: func() {}}
		m := newMove(ctx, &runner{}, message.ToolCallBlock{ID: "m"}, sl, -1)
		defer m.close()
		if m.ctx.Err() != context.Canceled { // checked before the AfterFunc goroutine could run
			t.Fatalf("a call of a turn already over starts with err %v", m.ctx.Err())
		}
		if m.Claim() {
			t.Fatal("claimed a call of a turn already over")
		}
	})
}

func TestCancelWhileClaimedAppliesAtCommit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := newMoveRunner(map[string]func(context.Context){"m": func(ctx context.Context) { <-ctx.Done() }})
		go func() {
			m := <-r.moves
			m.Claim()
			m.Cancel() // the task was stopped before the call settled
			time.Sleep(time.Second)
			if m.Moved() {
				t.Error("moved before Commit")
			}
			m.Commit(standIn, 0)
		}()
		Run(t.Context(), r, calls("m"), Options{Grace: time.Second, Timeout: noTimeout, Movable: movable})
		synctest.Wait()
		r.mu.Lock()
		defer r.mu.Unlock()
		if !r.moved["m"] || r.ended["m"] != context.Canceled || r.took["m"] != time.Second {
			t.Fatalf("moved %v ended %v after %v", r.moved, r.ended["m"], r.took["m"])
		}
	})
}
