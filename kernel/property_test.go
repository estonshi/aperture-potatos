package kernel_test

import (
	"context"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/providertest"
)

// paced puts a short hold after every event, so an interruption can land between any two of them.
func paced(steps []providertest.Step) []providertest.Step {
	var out []providertest.Step
	for _, s := range steps {
		out = append(out, s, providertest.Step{Hold: time.Millisecond})
	}
	return out
}

func loopScripts() [][]providertest.Step {
	return [][]providertest.Step{
		paced(toolReply(call{"toolu_1", "echo", `{"n":1}`}, call{"toolu_2", "echo", `{"n":2}`})),
		paced(toolReply(call{"toolu_3", "echo", `{"n":3}`})),
		paced(textReply("done")),
	}
}

// checkRun asserts what every exit path must keep: paired history (I1), at most one run per committed call (I2) and one turn.completed per turn.
func checkRun(t *testing.T, h *harness, echo *echoTool) {
	t.Helper()
	lines := h.lines()
	if err := pairedHistory(lines); err != nil {
		t.Fatal(err)
	}
	committed := map[string]bool{}
	for _, m := range lines {
		for _, c := range m.ToolCalls() {
			committed[c.ID] = true
		}
	}
	for _, id := range []string{"toolu_1", "toolu_2", "toolu_3"} {
		if n := echo.runsOf(id); n > 1 || n == 1 && !committed[id] {
			t.Fatalf("%s ran %d times, committed %v", id, n, committed[id])
		}
	}
	if s, c := h.rec.count(event.TypeTurnStarted), h.rec.count(event.TypeTurnCompleted); s != c {
		t.Fatalf("%d turns started, %d completed", s, c)
	}
}

func TestPropertyInterruptAnywhere(t *testing.T) {
	var total int
	synctest.Test(t, func(t *testing.T) {
		echo := &echoTool{name: "echo", delay: 5 * time.Millisecond}
		h := newHarness(t, loopScripts(), kernel.WithTool(echo))
		rc, _ := h.sess.Send(kernel.Text("go"))
		if res, _ := rc.Wait(t.Context()); res.Outcome != kernel.OutcomeCompleted {
			t.Fatalf("baseline %+v", res)
		}
		h.close()
		total = len(h.rec.all())
	})
	for k := 1; k <= total; k++ {
		t.Run(fmt.Sprint(k), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				echo := &echoTool{name: "echo", delay: 5 * time.Millisecond}
				h := newHarness(t, loopScripts(), kernel.WithTool(echo))
				var seen atomic.Int64
				_, err := h.sess.SubscribeFunc(func(ctx context.Context, ev event.Event) error {
					if seen.Add(1) == int64(k) {
						h.sess.Interrupt("property")
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
				rc, _ := h.sess.Send(kernel.Text("go"))
				rc.Wait(t.Context())
				h.close()
				checkRun(t, h, echo)
			})
		})
	}
}

// failAt keeps the first j events of an attempt and then breaks the stream with a retryable error.
func failAt(steps []providertest.Step, j int) []providertest.Step {
	out := append([]providertest.Step(nil), steps[:j]...)
	return append(out, providertest.Step{Err: &provider.Error{Class: provider.ClassTransient, Message: "overloaded", MidStream: j > 0}})
}

func TestPropertyStreamErrorAnywhere(t *testing.T) {
	first := toolReply(call{"toolu_1", "echo", `{"n":1}`}, call{"toolu_2", "echo", `{"n":2}`})
	second := toolReply(call{"toolu_3", "echo", `{"n":3}`})
	for step := range 2 {
		frames := len(first)
		if step == 1 {
			frames = len(second)
		}
		for j := range frames {
			t.Run(fmt.Sprintf("step%d/frame%d", step+1, j), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					scripts := [][]providertest.Step{failAt(first, j), first, second, textReply("done")}
					if step == 1 {
						scripts = [][]providertest.Step{first, failAt(second, j), second, textReply("done")}
					}
					echo := &echoTool{name: "echo"}
					h := newHarness(t, scripts, kernel.WithTool(echo))
					rc, _ := h.sess.Send(kernel.Text("go"))
					res, _ := rc.Wait(t.Context())
					h.close()
					if res.Outcome != kernel.OutcomeCompleted {
						t.Fatalf("result %+v", res)
					}
					checkRun(t, h, echo)
					for _, id := range []string{"toolu_1", "toolu_2", "toolu_3"} {
						if echo.runsOf(id) != 1 {
							t.Fatalf("%s ran %d times; every call of a committed attempt runs exactly once", id, echo.runsOf(id))
						}
					}
					if n := h.rec.count(event.TypeModelRetry); n != 1 {
						t.Fatalf("%d retries", n)
					}
					if got := roles(h.lines()); got != "system,user,assistant,tool,assistant,tool,assistant" {
						t.Fatalf("lines %s", got)
					}
					var discarded int
					for _, ev := range h.rec.all() {
						if d, ok := ev.Data.(*event.MessageDiscarded); ok && d.Reason == "retry" {
							discarded++
						}
					}
					if discarded != 1 {
						t.Fatalf("%d discarded attempts", discarded)
					}
				})
			})
		}
	}
}

// TestPropertyTaskCompletionAnywhere finishes a background task at every event of a turn, so its end lands before, at and after each safe point and the turn's exit: the notice reaches history exactly once, and no turn starts without something to deliver.
func TestPropertyTaskCompletionAnywhere(t *testing.T) {
	scripts := func() [][]providertest.Step {
		return [][]providertest.Step{paced(toolReply(call{"toolu_1", "echo", `{"n":1}`})), paced(textReply("done")), paced(textReply("noted")), textReply("an extra turn")}
	}
	var total int // events the probe sees up to the turn's end, counted as it counts them
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, scripts()[:2], kernel.WithTool(&echoTool{name: "echo", delay: 5 * time.Millisecond}))
		var seen atomic.Int64
		if _, err := h.sess.SubscribeFunc(func(context.Context, event.Event) error { seen.Add(1); return nil }); err != nil {
			t.Fatal(err)
		}
		rc, _ := h.sess.Send(kernel.Text("go"))
		rc.Wait(t.Context())
		synctest.Wait()
		total = int(seen.Load())
		h.close()
	})
	for k := 1; k <= total; k++ {
		t.Run(fmt.Sprint(k), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				finish := make(chan struct{})
				job := funcKind("job", kernel.TaskCaps{}, func(ctx context.Context, rt kernel.TaskRuntime) (kernel.TaskResult, error) {
					select {
					case <-finish:
						return kernel.TaskResult{Summary: "finished"}, nil
					case <-ctx.Done():
						return kernel.TaskResult{}, ctx.Err()
					}
				})
				h := newHarness(t, scripts(), kernel.WithTool(&echoTool{name: "echo", delay: 5 * time.Millisecond}), kernel.WithTaskKind(job))
				hostStart(t, h.sess, "job")
				var seen atomic.Int64
				if _, err := h.sess.SubscribeFunc(func(context.Context, event.Event) error {
					if seen.Add(1) == int64(k) {
						close(finish)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				rc, _ := h.sess.Send(kernel.Text("go"))
				rc.Wait(t.Context())
				if _, err := h.sess.WaitQuiet(t.Context()); err != nil {
					t.Fatal(err)
				}
				h.close()
				notes := 0
				for _, m := range h.lines() {
					if strings.Count(m.Text(), `<task-notification task_id="task_01"`) > 0 {
						notes++
					}
				}
				if notes != 1 {
					t.Fatalf("the notice reached history %d times", notes)
				}
				if n := len(h.sp.Requests()); n > 3 {
					t.Fatalf("%d model requests: a turn started with nothing to deliver", n)
				}
				for _, ev := range h.rec.all() {
					if d, ok := ev.Data.(*event.TurnStarted); ok && len(d.InputIDs) == 0 {
						t.Fatalf("turn %s started without input", d.TurnID)
					}
				}
			})
		})
	}
}

// TestPropertyChildTaskCompletionAnywhere finishes a task that owns a child session at every event of the owner's turn, the child running one more turn before Run returns: that turn reaches the owner before task.finished, and the notice still reaches history exactly once.
func TestPropertyChildTaskCompletionAnywhere(t *testing.T) {
	scripts := func() [][]providertest.Step {
		return [][]providertest.Step{paced(toolReply(call{"toolu_1", "echo", `{"n":1}`})), paced(textReply("done")), paced(textReply("noted")), textReply("an extra turn")}
	}
	var total int
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, scripts()[:2], kernel.WithTool(&echoTool{name: "echo", delay: 5 * time.Millisecond}))
		var seen atomic.Int64
		if _, err := h.sess.SubscribeFunc(func(context.Context, event.Event) error { seen.Add(1); return nil }); err != nil {
			t.Fatal(err)
		}
		rc, _ := h.sess.Send(kernel.Text("go"))
		rc.Wait(t.Context())
		synctest.Wait()
		total = int(seen.Load())
		h.close()
	})
	for k := 1; k <= total; k++ {
		t.Run(fmt.Sprint(k), func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				childModel := provider.ModelRef{Provider: providertest.NewScriptProvider(provider.APIAnthropicMessages, paced(textReply("working")), paced(textReply("wrapped up"))), Model: "m"}
				ready, finish := make(chan struct{}), make(chan struct{})
				var derived *kernel.Agent
				var child *kernel.Session
				job := funcKind("job", kernel.TaskCaps{}, func(ctx context.Context, rt kernel.TaskRuntime) (kernel.TaskResult, error) {
					var err error
					if child, err = derived.NewSession(ctx, kernel.ChildOf(rt)); err != nil {
						return kernel.TaskResult{}, err
					}
					talk := func(text string) (kernel.TurnResult, error) {
						rc, err := child.Send(kernel.Text(text))
						if err != nil {
							return kernel.TurnResult{}, err
						}
						return rc.Wait(ctx)
					}
					if _, err := talk("work"); err != nil {
						return kernel.TaskResult{}, err
					}
					close(ready)
					select {
					case <-finish:
					case <-ctx.Done():
						return kernel.TaskResult{}, ctx.Err()
					}
					res, err := talk("wrap up")
					return kernel.TaskResult{Summary: res.FinalText}, err
				})
				h := newHarness(t, scripts(), kernel.WithTool(&echoTool{name: "echo", delay: 5 * time.Millisecond}), kernel.WithTaskKind(job))
				var err error
				if derived, err = h.ag.Derive(kernel.DeriveOptions{Model: &childModel}); err != nil {
					t.Fatal(err)
				}
				hostStart(t, h.sess, "job")
				<-ready
				var seen atomic.Int64
				if _, err := h.sess.SubscribeFunc(func(_ context.Context, ev event.Event) error {
					if len(ev.Path) == 0 && seen.Add(1) == int64(k) {
						close(finish)
					}
					return nil
				}); err != nil {
					t.Fatal(err)
				}
				rc, _ := h.sess.Send(kernel.Text("go"))
				rc.Wait(t.Context())
				if _, err := h.sess.WaitQuiet(t.Context()); err != nil {
					t.Fatal(err)
				}
				synctest.Wait()
				before := h.rec.all() // the child's own close comes later, with the owner's
				h.close()
				notes := 0
				for _, m := range h.lines() {
					if strings.Contains(m.Text(), `<task-notification task_id="task_01"`) {
						notes++
					}
				}
				if notes != 1 {
					t.Fatalf("the notice reached history %d times", notes)
				}
				if n := len(h.sp.Requests()); n > 3 {
					t.Fatalf("%d model requests: a turn started with nothing to deliver", n)
				}
				finished, lastChild, wrapped := -1, -1, false
				for i, ev := range before {
					switch d := ev.Data.(type) {
					case *event.TaskFinished:
						if len(ev.Path) == 0 && d.TaskID == "task_01" {
							finished = i
						}
					case *event.MessageCompleted:
						wrapped = wrapped || len(ev.Path) > 0 && d.Message.Text() == "wrapped up"
					}
					if len(ev.Path) > 0 && ev.Path[0].ID == "task_01" {
						lastChild = i
					}
				}
				if !wrapped || finished < lastChild {
					t.Fatalf("task.finished at %d, the child's last event at %d, wrap-up relayed %v", finished, lastChild, wrapped)
				}
				if sn := child.Snapshot(); sn.Phase != "closed" || len(sn.Workers) != 0 {
					t.Fatalf("child %s with workers %v after the owner closed", sn.Phase, sn.Workers)
				}
			})
		})
	}
}
