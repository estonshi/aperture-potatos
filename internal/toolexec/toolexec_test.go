package toolexec

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/estonshi/aperture-potatos/internal/prompt"
	"github.com/estonshi/aperture-potatos/message"
)

type runner struct {
	mu        sync.Mutex
	completed []string
	running   int                                  // calls inside RunOne right now
	peak      int                                  // the most calls that ran at once
	behave    map[string]func(ctx context.Context) // how each call spends its time
}

func (r *runner) RunOne(ctx context.Context, c message.ToolCallBlock) message.ToolResultBlock {
	r.mu.Lock()
	r.running++
	r.peak = max(r.peak, r.running)
	r.mu.Unlock()
	r.behave[c.ID](ctx)
	r.mu.Lock()
	r.running--
	r.mu.Unlock()
	return message.ToolResultBlock{CallID: c.ID, Outcome: message.OutcomeSuccess}
}

func (r *runner) Parallel(name string) bool { return name == "p" }

func (r *runner) Completed(c message.ToolCallBlock, _ message.ToolResultBlock) {
	r.mu.Lock()
	r.completed = append(r.completed, c.ID)
	r.mu.Unlock()
}

func (r *runner) Go(_ string, fn func()) { go fn() }

func calls(ids ...string) []message.ToolCallBlock {
	var out []message.ToolCallBlock
	for _, id := range ids {
		out = append(out, message.ToolCallBlock{ID: id, Name: "t"})
	}
	return out
}

// parallel marks calls whose id starts with p as parallel ones.
func parallel(ids ...string) []message.ToolCallBlock {
	out := calls(ids...)
	for i := range out {
		if out[i].ID[0] == 'p' {
			out[i].Name = "p"
		}
	}
	return out
}

func noTimeout(message.ToolCallBlock) time.Duration { return -1 }

func TestInterruptAbandonsAfterGrace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := &runner{behave: map[string]func(context.Context){
			"a": func(context.Context) {},
			"b": func(context.Context) { time.Sleep(time.Hour) }, // ignores cancellation
			"c": func(context.Context) {},
		}}
		ctx, cancel := context.WithCancel(t.Context())
		go func() { time.Sleep(time.Second); cancel() }()
		var abandoned []string
		start := time.Now()
		res := Run(ctx, r, calls("a", "b", "c"), Options{Grace: 3 * time.Second, Timeout: noTimeout, OnAbandon: func(c message.ToolCallBlock) { abandoned = append(abandoned, c.ID) }})
		if len(res) != 3 || res[0].Outcome != message.OutcomeSuccess || res[1].Outcome != message.OutcomeInterrupted || res[2].Outcome != message.OutcomeInterrupted {
			t.Fatalf("results %+v", res)
		}
		if waited := time.Since(start); waited != 4*time.Second {
			t.Fatalf("returned after %v, want the interrupt plus the grace period", waited)
		}
		if len(abandoned) != 1 || abandoned[0] != "b" {
			t.Fatalf("abandoned %v", abandoned)
		}
		time.Sleep(time.Hour) // the abandoned call returns late; its result must be dropped
		synctest.Wait()
		if len(r.completed) != 1 || r.completed[0] != "a" {
			t.Fatalf("completed %v; a late result must not complete", r.completed)
		}
	})
}

func TestTimeoutWithoutInterrupt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := &runner{behave: map[string]func(context.Context){
			"slow": func(ctx context.Context) { <-ctx.Done() }, // honors cancellation, so it returns within the grace period
		}}
		res := Run(t.Context(), r, calls("slow"), Options{Grace: time.Second, Timeout: func(message.ToolCallBlock) time.Duration { return 2 * time.Second }})
		if res[0].Outcome != message.OutcomeSuccess || len(r.completed) != 1 {
			t.Fatalf("a call that returns within the grace period keeps its own result: %+v", res)
		}
	})
}

func TestTimedOutCallIsAbandoned(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := &runner{behave: map[string]func(context.Context){
			"stuck": func(context.Context) { time.Sleep(time.Hour) },
		}}
		res := Run(t.Context(), r, calls("stuck"), Options{Grace: time.Second, Timeout: func(message.ToolCallBlock) time.Duration { return 2 * time.Second }})
		if res[0].Outcome != message.OutcomeError || len(res[0].Content) == 0 {
			t.Fatalf("result %+v", res[0])
		}
		time.Sleep(time.Hour)
	})
}

func TestPartitionKeepsCallOrder(t *testing.T) {
	got := Partition(parallel("p1", "p2", "s1", "p3", "s2", "s3", "p4", "p5"), func(n string) bool { return n == "p" })
	want := "[[0 1] [2] [3] [4] [5] [6 7]]"
	if fmt.Sprint(got) != want {
		t.Fatalf("groups %v, want %s", got, want)
	}
}

func TestParallelGroupThenSerial(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sleep := func(d time.Duration) func(context.Context) { return func(context.Context) { time.Sleep(d) } }
		r := &runner{behave: map[string]func(context.Context){"pa": sleep(3 * time.Second), "pb": sleep(time.Second), "pc": sleep(2 * time.Second), "s": sleep(time.Second)}}
		start := time.Now()
		res := Run(t.Context(), r, parallel("pa", "pb", "pc", "s"), Options{MaxParallel: 8, Grace: time.Second, Timeout: noTimeout})
		if took := time.Since(start); took != 4*time.Second {
			t.Fatalf("took %v: the group runs at once, then the serial call", took)
		}
		if fmt.Sprint(r.completed) != "[pb pc pa s]" || r.peak != 3 {
			t.Fatalf("completed %v, peak %d", r.completed, r.peak)
		}
		for i, id := range []string{"pa", "pb", "pc", "s"} {
			if res[i].CallID != id {
				t.Fatalf("results are in call order: %+v", res)
			}
		}
	})
}

func TestMaxParallelBoundsTheGroup(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sleep := func(context.Context) { time.Sleep(time.Second) }
		r := &runner{behave: map[string]func(context.Context){"p1": sleep, "p2": sleep, "p3": sleep, "p4": sleep, "p5": sleep}}
		start := time.Now()
		Run(t.Context(), r, parallel("p1", "p2", "p3", "p4", "p5"), Options{MaxParallel: 2, Grace: time.Second, Timeout: noTimeout})
		if took := time.Since(start); took != 3*time.Second || r.peak != 2 {
			t.Fatalf("took %v with peak %d, want 3s and 2", took, r.peak)
		}
	})
}

// Two calls of one group that both ignore cancellation share one grace period, so the turn waits the grace once.
func TestTwoIgnoreCtxToolsInterrupted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		stubborn := func(context.Context) { time.Sleep(time.Hour) }
		r := &runner{behave: map[string]func(context.Context){"p1": stubborn, "p2": stubborn, "p3": func(context.Context) {}, "s": func(context.Context) {}}}
		ctx, cancel := context.WithCancel(t.Context())
		go func() { time.Sleep(time.Second); cancel() }()
		var abandoned []string
		start := time.Now()
		res := Run(ctx, r, parallel("p1", "p2", "p3", "s"), Options{MaxParallel: 8, Grace: 3 * time.Second, Timeout: noTimeout, OnAbandon: func(c message.ToolCallBlock) { abandoned = append(abandoned, c.ID) }})
		if took := time.Since(start); took != 4*time.Second {
			t.Fatalf("returned after %v, want the interrupt plus one grace period", took)
		}
		if fmt.Sprint(abandoned) != "[p1 p2]" || res[2].Outcome != message.OutcomeSuccess || res[3].Content[0].(*message.TextBlock).Text != prompt.Text("tool_not_started") {
			t.Fatalf("abandoned %v, results %+v", abandoned, res)
		}
		time.Sleep(time.Hour)
	})
}

// A call that times out and ignores it is abandoned and gives its slot back, so the calls queued behind it still run.
func TestAbandonedCallFreesItsSlot(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		r := &runner{behave: map[string]func(context.Context){"p1": func(context.Context) { time.Sleep(time.Hour) }, "p2": func(context.Context) {}}}
		start := time.Now()
		res := Run(t.Context(), r, parallel("p1", "p2"), Options{MaxParallel: 1, Grace: time.Second, Timeout: func(message.ToolCallBlock) time.Duration { return 2 * time.Second }})
		if took := time.Since(start); took != 3*time.Second || res[0].Outcome != message.OutcomeError || res[1].Outcome != message.OutcomeSuccess {
			t.Fatalf("took %v, results %+v", took, res)
		}
		time.Sleep(time.Hour)
	})
}
