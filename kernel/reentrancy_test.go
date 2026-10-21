package kernel_test

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/provider/providertest"
)

// probe calls one public method from inside a callback, with the callback's context.
type probe func(ctx context.Context, s *kernel.Session, rc *kernel.Receipt, self *kernel.Subscription) string

func outcome(err error) string {
	switch {
	case err == nil:
		return "ok"
	case errors.Is(err, kernel.ErrReentrantWait):
		return "reentrant"
	}
	return err.Error()
}

var probes = map[string]probe{
	"Send": func(ctx context.Context, s *kernel.Session, _ *kernel.Receipt, _ *kernel.Subscription) string {
		_, err := s.Send(kernel.Text("later"), kernel.NoWake())
		return outcome(err)
	},
	"Receipt.Wait": func(ctx context.Context, _ *kernel.Session, rc *kernel.Receipt, _ *kernel.Subscription) string {
		_, err := rc.Wait(ctx)
		return outcome(err)
	},
	"Run": func(ctx context.Context, s *kernel.Session, _ *kernel.Receipt, _ *kernel.Subscription) string {
		for _, err := range s.Run(ctx, kernel.Text("run me")) {
			if err != nil {
				return outcome(err)
			}
		}
		return "ok"
	},
	"Close": func(ctx context.Context, s *kernel.Session, _ *kernel.Receipt, _ *kernel.Subscription) string {
		return outcome(s.Close(ctx))
	},
	"Unsubscribe": func(_ context.Context, _ *kernel.Session, _ *kernel.Receipt, self *kernel.Subscription) string {
		self.Unsubscribe()
		return "ok"
	},
	"Interrupt": func(_ context.Context, s *kernel.Session, _ *kernel.Receipt, _ *kernel.Subscription) string {
		s.Interrupt("from a callback")
		return "ok"
	},
	"Command": func(_ context.Context, s *kernel.Session, _ *kernel.Receipt, _ *kernel.Subscription) string {
		_, err := s.Command("noop", "")
		return outcome(err)
	},
	"Compact": func(ctx context.Context, s *kernel.Session, _ *kernel.Receipt, _ *kernel.Subscription) string {
		_, err := s.Compact(ctx, "")
		return outcome(err)
	},
	"Tasks().Wait": func(ctx context.Context, s *kernel.Session, _ *kernel.Receipt, _ *kernel.Subscription) string {
		_, err := s.Tasks().Wait(ctx, "task_01") // the finished task runProbe starts first
		return outcome(err)
	},
}

// wants is the re-entrancy table: waiting on the session chain from a callback that the session is waiting on fails fast, and so does a lossless handler waiting on a task whose events wait for its credit; a WaitIdle command holds the history token, so its Compact runs in place (D25).
var wants = map[string]map[string]string{
	"block handler": {"Send": "ok", "Receipt.Wait": "reentrant", "Run": "reentrant", "Close": "reentrant", "Compact": "reentrant", "Unsubscribe": "ok", "Interrupt": "ok", "Command": "ok", "Tasks().Wait": "reentrant"},
	"drop handler":  {"Send": "ok", "Receipt.Wait": "ok", "Run": "ok", "Close": "ok", "Compact": "ok", "Unsubscribe": "ok", "Interrupt": "ok", "Command": "ok", "Tasks().Wait": "ok"},
	"hook":          {"Send": "ok", "Receipt.Wait": "reentrant", "Run": "reentrant", "Close": "reentrant", "Compact": "reentrant", "Unsubscribe": "ok", "Interrupt": "ok", "Command": "ok", "Tasks().Wait": "ok"},
	"tool":          {"Send": "ok", "Receipt.Wait": "reentrant", "Run": "reentrant", "Close": "reentrant", "Compact": "reentrant", "Unsubscribe": "ok", "Interrupt": "ok", "Command": "ok", "Tasks().Wait": "ok"},
	"command":       {"Send": "ok", "Receipt.Wait": "reentrant", "Run": "reentrant", "Close": "reentrant", "Compact": "ok", "Unsubscribe": "ok", "Interrupt": "ok", "Command": "ok", "Tasks().Wait": "ok"},
	"task":          {"Send": "ok", "Receipt.Wait": "reentrant", "Run": "reentrant", "Close": "reentrant", "Compact": "reentrant", "Unsubscribe": "ok", "Interrupt": "ok", "Command": "ok", "Tasks().Wait": "ok"},
}

// probeTool runs the probe inside its own call.
type probeTool struct {
	run func(ctx context.Context) string
}

func (p *probeTool) Spec() kernel.ToolSpec {
	return kernel.ToolSpec{Name: "probe", Concurrency: kernel.Serial}
}

func (p *probeTool) Call(ctx context.Context, _ *kernel.ToolContext, _ json.RawMessage) (kernel.ToolResult, error) {
	return kernel.TextResult(p.run(ctx)), nil
}

// TestReentrancyMatrix covers I3: user code that calls back into its own session never deadlocks, which it would if the session's owner ran it.
func TestReentrancyMatrix(t *testing.T) {
	for caller, byMethod := range wants {
		for method, want := range byMethod {
			t.Run(caller+"/"+method, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					got := runProbe(t, caller, probes[method])
					if got != want {
						t.Fatalf("got %s, want %s", got, want)
					}
				})
			})
		}
	}
}

func runProbe(t *testing.T, caller string, p probe) string {
	var (
		mu     sync.Mutex
		result string
		rc     *kernel.Receipt
		self   *kernel.Subscription
		h      *harness
	)
	record := func(s string) {
		mu.Lock()
		result = s
		mu.Unlock()
	}
	get := func() (*kernel.Receipt, *kernel.Subscription) {
		mu.Lock()
		defer mu.Unlock()
		return rc, self
	}
	noop := command("noop", kernel.WaitIdle, "noop")
	tool := &probeTool{run: func(ctx context.Context) string {
		r, sub := get()
		out := p(ctx, h.sess, r, sub)
		record(out)
		return out
	}}
	probeCmd := kernel.Command{Name: "probe", Busy: kernel.WaitIdle, Run: func(ctx context.Context, cc *kernel.CommandContext) (kernel.CommandResult, error) {
		r, sub := get()
		record(p(ctx, cc.Session(), r, sub))
		return kernel.CommandResult{}, nil
	}}
	quick := funcKind("quick", kernel.TaskCaps{}, func(context.Context, kernel.TaskRuntime) (kernel.TaskResult, error) {
		return kernel.TaskResult{}, nil
	})
	probeTask := funcKind("probe", kernel.TaskCaps{}, func(ctx context.Context, rt kernel.TaskRuntime) (kernel.TaskResult, error) {
		r, sub := get()
		record(p(ctx, rt.Session(), r, sub))
		return kernel.TaskResult{}, nil
	})
	scripts := [][]providertest.Step{textReply("one"), textReply("two"), textReply("three")}
	if caller == "tool" {
		scripts = [][]providertest.Step{toolReply(call{"toolu_1", "probe", `{}`}), textReply("two"), textReply("three")}
	}
	receipted := make(chan struct{})
	var hookOnce sync.Once
	stopHook := kernel.WithHook(kernel.Stop, "probe", func(ctx context.Context, in kernel.StopInput) (kernel.StopOutput, error) {
		if caller == "hook" {
			<-receipted // the hook runs in the turn the receipt belongs to
			hookOnce.Do(func() {
				r, sub := get()
				record(p(ctx, in.Session, r, sub))
			})
		}
		return kernel.StopOutput{}, nil
	})
	compactor := kernel.Use(kernel.ExtensionFunc("comp", func(r kernel.Registrar) error { _, err := r.Compactor(stubCompactor{}); return err }))
	h = newHarness(t, scripts, kernel.WithTool(tool), kernel.WithCommand(noop), kernel.WithCommand(probeCmd), kernel.WithTaskKind(quick), kernel.WithTaskKind(probeTask), quiet, stopHook, compactor)
	if _, err := h.sess.Tasks().Wait(t.Context(), hostStart(t, h.sess, "quick").ID); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	handler := func(ctx context.Context, ev event.Event) error {
		if ev.Type == event.TypeMessageDelta {
			once.Do(func() {
				r, sub := get()
				record(p(ctx, h.sess, r, sub))
			})
		}
		return nil
	}
	switch caller {
	case "block handler", "drop handler":
		var opts []kernel.SubscribeOption
		if caller == "drop handler" {
			opts = append(opts, kernel.WithDropDeltas())
		}
		sub, err := h.sess.SubscribeFunc(handler, opts...)
		if err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		self = sub
		mu.Unlock()
	default:
		sub, _ := h.sess.SubscribeFunc(func(context.Context, event.Event) error { return nil })
		mu.Lock()
		self = sub
		mu.Unlock()
	}
	first, _ := h.sess.Send(kernel.Text("start"))
	mu.Lock()
	rc = first
	mu.Unlock()
	close(receipted)
	first.Wait(context.Background())
	switch caller {
	case "command":
		c, _ := h.sess.Command("probe", "")
		c.Wait(context.Background())
	case "task":
		h.sess.Tasks().Wait(context.Background(), hostStart(t, h.sess, "probe").ID)
	}
	synctest.Wait()
	h.close()
	mu.Lock()
	defer mu.Unlock()
	return result
}
