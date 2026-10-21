package kernel_test

import (
	"context"
	"encoding/json"
	"testing"
	"testing/synctest"
	"time"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider/providertest"
)

// stuckParallel is a parallel tool that ignores cancellation for a minute.
type stuckParallel struct{ name string }

func (s stuckParallel) Spec() kernel.ToolSpec {
	return kernel.ToolSpec{Name: s.name, Concurrency: kernel.Parallel}
}

func (stuckParallel) Call(context.Context, *kernel.ToolContext, json.RawMessage) (kernel.ToolResult, error) {
	time.Sleep(time.Minute)
	return kernel.TextResult("late"), nil
}

func TestParallelGroupInterruptedWaitsOneGrace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, [][]providertest.Step{toolReply(call{"toolu_1", "slow_a", `{}`}, call{"toolu_2", "slow_b", `{}`}), textReply("unused")},
			kernel.WithTool(stuckParallel{"slow_a"}), kernel.WithTool(stuckParallel{"slow_b"}),
			kernel.WithLimits(kernel.Limits{ToolCancelGrace: 3 * time.Second, CloseTimeout: time.Second}))
		rc, err := h.sess.Send(kernel.Text("go"))
		if err != nil {
			t.Fatal(err)
		}
		synctest.Wait() // both calls are running
		if n := h.rec.count(event.TypeToolStarted); n != 2 {
			t.Fatalf("%d calls started; a parallel group starts together", n)
		}
		start := time.Now()
		h.sess.Interrupt("stop")
		res, err := rc.Wait(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if took := time.Since(start); took != 3*time.Second || res.Outcome != kernel.OutcomeInterrupted {
			t.Fatalf("took %v, outcome %s: the group shares one grace period", took, res.Outcome)
		}
		results := toolResults(h.lines())
		if len(results) != 2 || results[0].Outcome != message.OutcomeInterrupted || results[1].Outcome != message.OutcomeInterrupted || h.rec.count(event.TypeToolAbandoned) != 2 {
			t.Fatalf("results %+v", results)
		}
		time.Sleep(time.Minute) // the abandoned calls return late, and their results are dropped
		h.close()
		if h.rec.count(event.TypeToolCompleted) != 0 {
			t.Fatal("an abandoned call never completes")
		}
	})
}
