package kernel_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider/providertest"
)

type taskFunc func(ctx context.Context, rt kernel.TaskRuntime) (kernel.TaskResult, error)

func (f taskFunc) Run(ctx context.Context, rt kernel.TaskRuntime) (kernel.TaskResult, error) {
	return f(ctx, rt)
}

func funcKind(name string, caps kernel.TaskCaps, run taskFunc) kernel.TaskKind {
	return kernel.TaskKind{Name: name, Caps: caps, New: func(kernel.TaskSpec) (kernel.Task, error) { return run, nil }}
}

func hostStart(t *testing.T, s *kernel.Session, kind string) kernel.TaskInfo {
	t.Helper()
	info, err := s.Tasks().Start(t.Context(), kernel.TaskSpec{Kind: kind, Title: kind, Input: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	return info
}

// quiet keeps notifications from starting turns, so a test needs no model replies for them.
var quiet = kernel.WithHook(kernel.TaskNotify, "quiet", func(context.Context, kernel.TaskNotifyInput) (kernel.TaskNotifyOutput, error) {
	return kernel.TaskNotifyOutput{NoWake: true}, nil
})

func TestTaskAbandonedAfterGrace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		stuck := funcKind("stuck", kernel.TaskCaps{}, func(context.Context, kernel.TaskRuntime) (kernel.TaskResult, error) {
			<-release // ignores its context
			return kernel.TaskResult{Summary: "late"}, nil
		})
		h := newHarness(t, nil, kernel.WithTaskKind(stuck), kernel.WithLimits(kernel.Limits{ToolCancelGrace: 100 * time.Millisecond}), quiet)
		info := hostStart(t, h.sess, "stuck")
		if err := h.sess.Tasks().Stop(t.Context(), info.ID, "enough"); err != nil {
			t.Fatal(err)
		}
		time.Sleep(200 * time.Millisecond)
		synctest.Wait()
		got, _ := h.sess.Tasks().Get(info.ID)
		if got.Status != kernel.TaskCanceled || got.Error != "enough" || !got.Abandoned {
			t.Fatalf("after the grace: %+v", got)
		}
		if n := h.rec.count(event.TypeError); n != 1 {
			t.Fatalf("%d error events, want the one worker_leaked", n)
		}
		close(release)
		synctest.Wait()
		if n := h.rec.count(event.TypeTaskFinished); n != 1 {
			t.Fatalf("%d task.finished, want 1: the late return is dropped", n)
		}
		h.close()
	})
}

func TestTaskWaitsRefuseCycles(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		results := map[string]chan error{"task_01": make(chan error, 2), "task_02": make(chan error, 2)}
		targets := map[string]chan string{"task_01": make(chan string, 1), "task_02": make(chan string, 1)}
		waiter := funcKind("waiter", kernel.TaskCaps{}, func(ctx context.Context, rt kernel.TaskRuntime) (kernel.TaskResult, error) {
			_, err := rt.Session().Tasks().Wait(ctx, rt.ID()) // itself
			results[rt.ID()] <- err
			_, err = rt.Session().Tasks().Wait(ctx, <-targets[rt.ID()])
			results[rt.ID()] <- err
			return kernel.TaskResult{}, nil
		})
		h := newHarness(t, nil, kernel.WithTaskKind(waiter), quiet)
		a := hostStart(t, h.sess, "waiter")
		b := hostStart(t, h.sess, "waiter")
		for _, id := range []string{a.ID, b.ID} {
			if err := <-results[id]; !errors.Is(err, kernel.ErrReentrantWait) {
				t.Fatalf("%s waiting on itself: %v", id, err)
			}
		}
		targets[a.ID] <- b.ID
		synctest.Wait()
		targets[b.ID] <- a.ID // would close the loop
		if err := <-results[b.ID]; !errors.Is(err, kernel.ErrReentrantWait) {
			t.Fatalf("the cycle: %v", err)
		}
		if err := <-results[a.ID]; err != nil { // b returned, so a's wait ends
			t.Fatalf("a's wait: %v", err)
		}
		h.close()
	})
}

func TestTaskStateMustBeJSON(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		saved := make(chan struct{})
		saver := funcKind("saver", kernel.TaskCaps{}, func(ctx context.Context, rt kernel.TaskRuntime) (kernel.TaskResult, error) {
			rt.SaveState(json.RawMessage(`{ "page": 3 }`))
			rt.SaveState(json.RawMessage(`{not json`))
			close(saved)
			<-ctx.Done()
			return kernel.TaskResult{}, ctx.Err()
		})
		h := newHarness(t, nil, kernel.WithTaskKind(saver), quiet)
		info := hostStart(t, h.sess, "saver")
		<-saved
		synctest.Wait()
		got, _ := h.sess.Tasks().Get(info.ID)
		if string(got.State) != `{"page":3}` {
			t.Fatalf("state %s, want the valid one, compacted", got.State)
		}
		if h.rec.count(event.TypeError) != 1 {
			t.Fatal("no error for the invalid state")
		}
		h.close()
	})
}

func TestFinishedTasksEvicted(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		done := funcKind("done", kernel.TaskCaps{}, func(context.Context, kernel.TaskRuntime) (kernel.TaskResult, error) {
			return kernel.TaskResult{Summary: "ok"}, nil
		})
		h := newHarness(t, nil, kernel.WithTaskKind(done), quiet)
		var first string
		for i := range 52 {
			info := hostStart(t, h.sess, "done")
			if i == 0 {
				first = info.ID
			}
			if _, err := h.sess.Tasks().Wait(t.Context(), info.ID); err != nil {
				t.Fatal(err)
			}
		}
		if n := len(h.sess.Tasks().List()); n != 50 {
			t.Fatalf("%d tasks kept, want 50", n)
		}
		if _, ok := h.sess.Tasks().Get(first); ok {
			t.Fatal("the oldest finished task is still there")
		}
		if err := h.sess.Tasks().Send(t.Context(), first, kernel.TaskInstruction{Text: "hi"}); !errors.Is(err, kernel.ErrTaskNotFound) {
			t.Fatalf("send to an evicted task: %v", err)
		}
		h.close()
	})
}

func TestTaskKindRegistration(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		mk := func(name string) kernel.TaskKind {
			return funcKind(name, kernel.TaskCaps{}, func(context.Context, kernel.TaskRuntime) (kernel.TaskResult, error) {
				return kernel.TaskResult{Summary: name}, nil
			})
		}
		h := newHarness(t, nil, kernel.WithTaskKind(mk("job")), quiet)
		reg := h.sess.Registrar()
		if _, err := reg.TaskKind(mk(kernel.ToolExecKind)); !errors.Is(err, kernel.ErrDuplicate) {
			t.Fatalf("the kernel's kind: %v", err)
		}
		if _, err := reg.TaskKind(mk("bad name")); !errors.Is(err, kernel.ErrInvalidName) {
			t.Fatalf("a bad name: %v", err)
		}
		if _, err := reg.TaskKind(kernel.TaskKind{Name: "empty"}); err == nil {
			t.Fatal("a kind without New")
		}
		shadow := funcKind("job", kernel.TaskCaps{}, func(context.Context, kernel.TaskRuntime) (kernel.TaskResult, error) {
			return kernel.TaskResult{Summary: "session"}, nil
		})
		r, err := reg.TaskKind(shadow)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := reg.TaskKind(shadow); !errors.Is(err, kernel.ErrDuplicate) {
			t.Fatalf("a duplicate: %v", err)
		}
		info, _ := h.sess.Tasks().Wait(t.Context(), hostStart(t, h.sess, "job").ID)
		if info.Result.Summary != "session" {
			t.Fatalf("ran %q, want the session's kind", info.Result.Summary)
		}
		r.Remove()
		info, _ = h.sess.Tasks().Wait(t.Context(), hostStart(t, h.sess, "job").ID)
		if info.Result.Summary != "job" {
			t.Fatalf("ran %q, want the agent's kind again", info.Result.Summary)
		}
		if _, err := h.sess.Tasks().Start(t.Context(), kernel.TaskSpec{Kind: "nope"}); err == nil {
			t.Fatal("started an unknown kind")
		}
		h.close()
	})
}

func TestTaskToolSchema(t *testing.T) {
	spec := kernel.ToolSpec{Name: "start", InputSchema: json.RawMessage(`{"type": "object", "properties": {"q": {"type": "string"}}, "required": ["q"]}`), Strict: true}
	tool, err := kernel.TaskTool("job", spec, nil)
	if err != nil {
		t.Fatal(err)
	}
	got := string(tool.Spec().InputSchema)
	want := `{"type":"object","properties":{"q":{"type":"string"},"background":{"type":"boolean","default":false,"description":"Run in the background: return a task_id now and get a notification when it finishes."}},"required":["q","background"]}`
	if got != want {
		t.Fatalf("schema\n got %s\nwant %s", got, want)
	}
	for _, schema := range []string{`{"type":"object","properties":{"background":{"type":"string"}}}`, `[]`} {
		if _, err := kernel.TaskTool("job", kernel.ToolSpec{Name: "start", InputSchema: json.RawMessage(schema)}, nil); err == nil {
			t.Errorf("schema %s was accepted", schema)
		}
	}
}

func TestAskInputNeedsInstructions(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		asked := make(chan struct{})
		mute := funcKind("mute", kernel.TaskCaps{}, func(ctx context.Context, rt kernel.TaskRuntime) (kernel.TaskResult, error) {
			rt.AskInput("anyone?")
			close(asked)
			<-ctx.Done()
			return kernel.TaskResult{}, ctx.Err()
		})
		h := newHarness(t, nil, kernel.WithTaskKind(mute), quiet)
		info := hostStart(t, h.sess, "mute")
		<-asked
		synctest.Wait()
		if got, _ := h.sess.Tasks().Get(info.ID); got.Status != kernel.TaskRunning {
			t.Fatalf("status %s: nobody could answer, so the question is ignored", got.Status)
		}
		if h.rec.count(event.TypeError) != 1 {
			t.Fatal("the ignored question was not reported")
		}
		h.close()
	})
}

func TestCloseCancelsTasksSilently(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		waiting := funcKind("waiting", kernel.TaskCaps{}, func(ctx context.Context, rt kernel.TaskRuntime) (kernel.TaskResult, error) {
			<-ctx.Done()
			return kernel.TaskResult{}, ctx.Err()
		})
		h := newHarness(t, nil, kernel.WithTaskKind(waiting))
		info := hostStart(t, h.sess, "waiting")
		h.close()
		got, _ := h.sess.Tasks().Get(info.ID)
		if got.Status != kernel.TaskCanceled {
			t.Fatalf("status %s after close", got.Status)
		}
		for _, ev := range h.rec.all() {
			if d, ok := ev.Data.(*event.InputAccepted); ok && d.Origin.Kind == "task" {
				t.Fatal("a closing session queued a task notification")
			}
		}
	})
}

func TestStopTasksOnClear(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		waiting := funcKind("waiting", kernel.TaskCaps{}, func(ctx context.Context, rt kernel.TaskRuntime) (kernel.TaskResult, error) {
			<-ctx.Done()
			return kernel.TaskResult{}, ctx.Err()
		})
		h := newHarness(t, [][]providertest.Step{textReply("It was stopped.")}, kernel.WithTaskKind(waiting), kernel.WithContextPolicy(kernel.ContextPolicy{StopTasksOnClear: true}))
		info := hostStart(t, h.sess, "waiting")
		if err := h.sess.Clear(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, err := h.sess.WaitQuiet(t.Context()); err != nil {
			t.Fatal(err)
		}
		got, _ := h.sess.Tasks().Get(info.ID)
		if got.Status != kernel.TaskCanceled || got.Error != "the conversation was cleared" {
			t.Fatalf("after clear: %+v", got)
		}
		last := h.lines()[len(h.lines())-2]
		if len(last.Tasks) != 1 || !strings.Contains(last.Text(), "was stopped: the conversation was cleared") {
			t.Fatalf("the policy's stop was not notified: %+v", last)
		}
		h.close()
	})
}

func TestTaskNotifyRewritesAndHolds(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		done := funcKind("done", kernel.TaskCaps{}, func(context.Context, kernel.TaskRuntime) (kernel.TaskResult, error) {
			return kernel.TaskResult{Summary: "ok"}, nil
		})
		hold := kernel.WithHook(kernel.TaskNotify, "hold", func(_ context.Context, in kernel.TaskNotifyInput) (kernel.TaskNotifyOutput, error) {
			return kernel.TaskNotifyOutput{UpdatedText: "rewritten: " + in.Info.Title, NoWake: true}, nil
		})
		h := newHarness(t, [][]providertest.Step{textReply("Seen.")}, kernel.WithTaskKind(done), hold)
		info := hostStart(t, h.sess, "done")
		if _, err := h.sess.Tasks().Wait(t.Context(), info.ID); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if n := h.rec.count(event.TypeTurnStarted); n != 0 {
			t.Fatalf("%d turns: no_wake must hold the notice", n)
		}
		ask(t, h.sess, "Anything new?")
		user := h.lines()[len(h.lines())-2]
		if !strings.Contains(user.Text(), "rewritten: done") || !strings.Contains(user.Text(), "Anything new?") {
			t.Fatalf("the held notice did not ride the next input: %s", user.Text())
		}
		h.close()
	})
}

func TestBlockHandlerCannotWaitOnTasks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		waiting := funcKind("waiting", kernel.TaskCaps{}, func(ctx context.Context, rt kernel.TaskRuntime) (kernel.TaskResult, error) {
			<-ctx.Done()
			return kernel.TaskResult{}, ctx.Err()
		})
		h := newHarness(t, nil, kernel.WithTaskKind(waiting))
		got := make(chan error, 1)
		if _, err := h.sess.SubscribeFunc(func(ctx context.Context, ev event.Event) error {
			if d, ok := ev.Data.(*event.TaskStarted); ok {
				_, err := h.sess.Tasks().Wait(ctx, d.TaskID)
				got <- err
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		hostStart(t, h.sess, "waiting")
		if err := <-got; !errors.Is(err, kernel.ErrReentrantWait) {
			t.Fatalf("a lossless handler waiting on its session's task: %v", err)
		}
		h.close()
	})
}

// TestForegroundFinishRace lets a foreground task's end and the interrupt of the call waiting on it land in either order: the call reports what really happened, the task ends once, and nothing is notified either way.
func TestForegroundFinishRace(t *testing.T) {
	for _, order := range []string{"finish_first", "interrupt_first", "together"} {
		for i := range 20 {
			t.Run(fmt.Sprintf("%s/%d", order, i), func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					finish := make(chan struct{})
					started := make(chan struct{})
					job := funcKind("job", kernel.TaskCaps{}, func(ctx context.Context, rt kernel.TaskRuntime) (kernel.TaskResult, error) {
						close(started)
						<-finish // ignores its context, so the order is the test's
						return kernel.TaskResult{Summary: "finished"}, nil
					})
					start, _ := kernel.TaskTool("job", kernel.ToolSpec{Name: "start_job", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(in json.RawMessage) (kernel.TaskSpec, error) {
						return kernel.TaskSpec{Title: "job", Input: in}, nil
					})
					h := newHarness(t, [][]providertest.Step{toolReply(call{"toolu_1", "start_job", `{}`}), textReply("next")},
						kernel.WithTaskKind(job), kernel.WithTool(start), kernel.WithLimits(kernel.Limits{ToolCancelGrace: time.Second}))
					rc, _ := h.sess.Send(kernel.Text("go"))
					<-started
					switch order {
					case "finish_first":
						close(finish)
						synctest.Wait()
						h.sess.Interrupt("race")
					case "interrupt_first":
						h.sess.Interrupt("race")
						synctest.Wait()
						close(finish)
					default:
						close(finish)
						h.sess.Interrupt("race")
					}
					rc.Wait(t.Context())
					synctest.Wait()
					info, _ := h.sess.Tasks().Get("task_01")
					var res *message.ToolResultBlock
					for _, m := range h.lines() {
						for _, b := range m.Content {
							if r, ok := b.(*message.ToolResultBlock); ok && r.CallID == "toolu_1" {
								res = r
							}
						}
					}
					switch {
					case res == nil:
						t.Fatal("the call has no result")
					case info.Status == kernel.TaskSucceeded && res.Outcome == message.OutcomeSuccess && text(res.Content) == "finished":
					case info.Status == kernel.TaskCanceled && res.Outcome == message.OutcomeInterrupted:
					default:
						t.Fatalf("task %s, call %s %q: they disagree", info.Status, res.Outcome, text(res.Content))
					}
					if order == "interrupt_first" && info.Status != kernel.TaskCanceled {
						t.Fatalf("stopped before it returned, yet %s", info.Status)
					}
					if order == "finish_first" && info.Status != kernel.TaskSucceeded {
						t.Fatalf("returned before the stop, yet %s", info.Status)
					}
					if n := h.rec.count(event.TypeTaskFinished); n != 1 {
						t.Fatalf("%d task.finished", n)
					}
					for _, ev := range h.rec.all() {
						if d, ok := ev.Data.(*event.InputAccepted); ok && d.Origin.Kind == "task" {
							t.Fatal("a foreground task was notified")
						}
					}
					h.close()
				})
			})
		}
	}
}

// TestInterruptedForegroundTaskNotLostOnResume: an interrupt stops a foreground task silently, so its call's line must record it canceled, or Resume would report it lost.
func TestInterruptedForegroundTaskNotLostOnResume(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		finish := make(chan struct{})
		started := make(chan struct{})
		job := funcKind("job", kernel.TaskCaps{}, func(ctx context.Context, rt kernel.TaskRuntime) (kernel.TaskResult, error) {
			close(started)
			<-finish // ignores its context, so the tool line commits before the attempt ends
			return kernel.TaskResult{Summary: "finished"}, nil
		})
		start, _ := kernel.TaskTool("job", kernel.ToolSpec{Name: "start_job", InputSchema: json.RawMessage(`{"type":"object"}`)}, func(in json.RawMessage) (kernel.TaskSpec, error) {
			return kernel.TaskSpec{Title: "job", Input: in}, nil
		})
		h := newHarness(t, [][]providertest.Step{toolReply(call{"toolu_1", "start_job", `{}`})},
			kernel.WithTaskKind(job), kernel.WithTool(start), kernel.WithLimits(kernel.Limits{ToolCancelGrace: time.Hour}))
		rc, _ := h.sess.Send(kernel.Text("go"))
		<-started
		h.sess.Interrupt("stop")
		rc.Wait(t.Context())
		synctest.Wait()
		var ref *message.TaskRef
		for _, m := range h.lines() {
			if m.Role == message.RoleTool && len(m.Tasks) > 0 {
				ref = &m.Tasks[0]
			}
		}
		if info, _ := h.sess.Tasks().Get("task_01"); ref == nil || ref.Status != string(kernel.TaskCanceled) || info.Status != kernel.TaskRunning {
			t.Fatalf("line %+v, task %s: the line should record the only end left while the task still runs", ref, info.Status)
		}
		close(finish)
		synctest.Wait()
		id := h.sess.Info().SessionID
		h.close()
		second := resumeAgent(t, h.store)
		back, err := second.Resume(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if info, _ := back.Tasks().Get("task_01"); info.Status != kernel.TaskCanceled || back.Snapshot().QueuedInputs != 0 {
			t.Fatalf("after resume the task is %s with %d queued inputs; it was canceled, not lost", info.Status, back.Snapshot().QueuedInputs)
		}
		_ = second.Close(t.Context())
		synctest.Wait()
	})
}

// TestRestartTwiceBeforeTheFirstStarts replaces an attempt that never began: it gets no task.started, its instance still runs once, and the latest attempt starts when the first returns.
func TestRestartTwiceBeforeTheFirstStarts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		var runs atomic.Int64
		slow := kernel.TaskKind{Name: "slow", Caps: kernel.TaskCaps{Restart: true}, New: func(kernel.TaskSpec) (kernel.Task, error) {
			return taskFunc(func(ctx context.Context, rt kernel.TaskRuntime) (kernel.TaskResult, error) {
				runs.Add(1)
				if rt.Attempt() == 1 {
					<-release // ignores its context, so the restarts pile up behind it
				}
				return kernel.TaskResult{Summary: fmt.Sprint("attempt ", rt.Attempt())}, nil
			}), nil
		}}
		h := newHarness(t, nil, kernel.WithTaskKind(slow), kernel.WithLimits(kernel.Limits{ToolCancelGrace: time.Hour}), quiet)
		info := hostStart(t, h.sess, "slow")
		for range 2 {
			if err := h.sess.Tasks().Restart(t.Context(), info.ID, nil); err != nil {
				t.Fatal(err)
			}
		}
		close(release)
		got, err := h.sess.Tasks().Wait(t.Context(), info.ID)
		if err != nil || got.Attempt != 3 || got.Result.Summary != "attempt 3" {
			t.Fatalf("%+v, %v", got, err)
		}
		synctest.Wait()
		if n := runs.Load(); n != 3 {
			t.Fatalf("%d runs, want 3: every constructed instance runs once", n)
		}
		if s, f := h.rec.count(event.TypeTaskStarted), h.rec.count(event.TypeTaskFinished); s != 2 || f != 2 {
			t.Fatalf("%d started, %d finished: the attempt that never began has neither", s, f)
		}
		h.close()
	})
}

func TestTaskHoldingIsUnprocessed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		took := make(chan struct{})
		holder := funcKind("holder", kernel.TaskCaps{Instructions: true}, func(ctx context.Context, rt kernel.TaskRuntime) (kernel.TaskResult, error) {
			in := <-rt.Inbox()
			rt.Holding([]kernel.TaskInstruction{in})
			close(took)
			<-ctx.Done()
			return kernel.TaskResult{}, ctx.Err()
		})
		h := newHarness(t, nil, kernel.WithTaskKind(holder), quiet)
		info := hostStart(t, h.sess, "holder")
		for _, text := range []string{"first", "second"} {
			if err := h.sess.Tasks().Send(t.Context(), info.ID, kernel.TaskInstruction{Text: text}); err != nil {
				t.Fatal(err)
			}
			if text == "first" {
				<-took
			}
		}
		synctest.Wait()
		if err := h.sess.Tasks().Stop(t.Context(), info.ID, "enough"); err != nil {
			t.Fatal(err)
		}
		got, err := h.sess.Tasks().Wait(t.Context(), info.ID)
		if err != nil {
			t.Fatal(err)
		}
		var texts []string
		for _, in := range got.UnprocessedInstructions {
			texts = append(texts, in.Text)
		}
		if strings.Join(texts, ",") != "first,second" {
			t.Fatalf("unprocessed %v, want the held one before the unread one", texts)
		}
		h.close()
	})
}

func TestTaskRelay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		done := make(chan kernel.TaskRuntime, 1)
		relayer := funcKind("relayer", kernel.TaskCaps{}, func(ctx context.Context, rt kernel.TaskRuntime) (kernel.TaskResult, error) {
			ev := event.Event{Type: event.TypeMessageDelta, Seq: 7, SessionID: "elsewhere", Path: []event.Hop{{Kind: "task", ID: "inner"}}, Data: &event.MessageDelta{Text: "hi"}}
			if err := rt.Relay(ctx, ev); err != nil {
				return kernel.TaskResult{}, err
			}
			done <- rt
			return kernel.TaskResult{Summary: "ok"}, nil
		})
		h := newHarness(t, nil, kernel.WithTaskKind(relayer), quiet)
		info := hostStart(t, h.sess, "relayer")
		rt := <-done
		if _, err := h.sess.Tasks().Wait(t.Context(), info.ID); err != nil {
			t.Fatal(err)
		}
		if err := rt.Relay(t.Context(), event.Event{Type: event.TypeMessageDelta, Data: &event.MessageDelta{Text: "late"}}); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		var got []event.Event
		for _, ev := range h.rec.all() {
			if ev.Type == event.TypeMessageDelta {
				got = append(got, ev)
			}
		}
		if len(got) != 1 {
			t.Fatalf("%d relayed deltas, want 1: a relay after the end does nothing", len(got))
		}
		p := got[0].Path
		if got[0].SessionID != "elsewhere" || len(p) != 2 || p[0].Kind != "task" || p[0].ID != info.ID || p[0].Name != "relayer" || p[0].Seq != 7 || p[1].ID != "inner" {
			t.Fatalf("relayed %+v under %+v", got[0], p)
		}
		h.close()
	})
}

// inspected is a task whose live state comes from Inspect.
type inspected struct {
	answer func() (kernel.TaskProgress, error)
	calls  atomic.Int32
}

func (i *inspected) Run(ctx context.Context, rt kernel.TaskRuntime) (kernel.TaskResult, error) {
	rt.Report(kernel.TaskProgress{Text: "reported"})
	<-ctx.Done()
	return kernel.TaskResult{}, ctx.Err()
}

func (i *inspected) Inspect(context.Context) (kernel.TaskProgress, error) {
	i.calls.Add(1)
	return i.answer()
}

func TestTaskInspect(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		answers := []func() (kernel.TaskProgress, error){
			func() (kernel.TaskProgress, error) {
				return kernel.TaskProgress{Text: "live", Data: json.RawMessage(`{ "n": 1 }`)}, nil
			},
			func() (kernel.TaskProgress, error) { return kernel.TaskProgress{}, errors.New("backend down") },
			func() (kernel.TaskProgress, error) { panic("boom") },
		}
		inst := &inspected{}
		kind := func(name string, caps kernel.TaskCaps) kernel.TaskKind {
			return kernel.TaskKind{Name: name, Caps: caps, New: func(kernel.TaskSpec) (kernel.Task, error) { return inst, nil }}
		}
		h := newHarness(t, nil, kernel.WithTaskKind(kind("remote", kernel.TaskCaps{Inspect: true})), kernel.WithTaskKind(kind("plain", kernel.TaskCaps{})), quiet)
		info := hostStart(t, h.sess, "remote")
		synctest.Wait()
		inst.answer = answers[0]
		got, err := h.sess.Tasks().Inspect(t.Context(), info.ID)
		if err != nil || got.Progress.Text != "live" || string(got.Progress.Data) != `{"n":1}` {
			t.Fatalf("inspect: %+v %v", got.Progress, err)
		}
		if g, _ := h.sess.Tasks().Get(info.ID); g.Progress.Text != "live" {
			t.Fatalf("the inspection is not the latest progress: %+v", g.Progress)
		}
		inst.answer = answers[1]
		if got, err := h.sess.Tasks().Inspect(t.Context(), info.ID); err == nil || got.Progress.Text != "live" {
			t.Fatalf("a failed inspection: %+v %v, want the error and the stored progress", got.Progress, err)
		}
		inst.answer = answers[2]
		if _, err := h.sess.Tasks().Inspect(t.Context(), info.ID); err == nil || !strings.Contains(err.Error(), "boom") {
			t.Fatalf("a panicking inspection: %v", err)
		}
		plain := hostStart(t, h.sess, "plain")
		synctest.Wait()
		before := inst.calls.Load()
		if got, err := h.sess.Tasks().Inspect(t.Context(), plain.ID); err != nil || got.Progress.Text != "reported" || inst.calls.Load() != before {
			t.Fatalf("a kind without Caps.Inspect: %+v %v, want Get", got.Progress, err)
		}
		if _, err := h.sess.Tasks().Inspect(t.Context(), "task_nope"); !errors.Is(err, kernel.ErrTaskNotFound) {
			t.Fatalf("unknown task: %v", err)
		}
		_ = h.sess.Tasks().Stop(t.Context(), info.ID, "")
		if _, err := h.sess.Tasks().Wait(t.Context(), info.ID); err != nil {
			t.Fatal(err)
		}
		before = inst.calls.Load()
		if _, err := h.sess.Tasks().Inspect(t.Context(), info.ID); err != nil || inst.calls.Load() != before {
			t.Fatalf("a finished task was inspected: %v", err)
		}
		if n := h.rec.count(event.TypeTaskProgress); n != 3 {
			t.Fatalf("%d task.progress, want two reports and one inspection", n)
		}
		h.close()
	})
}

// TestTaskInspectWaitsForCredit: an inspection's task.progress is bulk, so with a subscriber over its high water Inspect waits on the caller's context, not on the actor.
func TestTaskInspectWaitsForCredit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		inst := &inspected{answer: func() (kernel.TaskProgress, error) { return kernel.TaskProgress{Text: "live"}, nil }}
		h := newHarness(t, nil, kernel.WithTaskKind(kernel.TaskKind{Name: "remote", Caps: kernel.TaskCaps{Inspect: true}, New: func(kernel.TaskSpec) (kernel.Task, error) { return inst, nil }}), quiet)
		info := hostStart(t, h.sess, "remote")
		synctest.Wait()
		stuck := make(chan event.Event) // never read
		sub, _ := h.sess.Subscribe(stuck, kernel.WithBuffer(1), kernel.WithStallTimeout(time.Hour))
		var err error
		for range 5 { // the pump holds one event and the queue the next
			ctx, cancel := context.WithTimeout(t.Context(), time.Second)
			_, err = h.sess.Tasks().Inspect(ctx, info.ID)
			cancel()
			if err != nil {
				break
			}
		}
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("over the high water: %v, want the caller's deadline", err)
		}
		if _, ok := h.sess.Tasks().Get(info.ID); !ok {
			t.Fatal("the session stopped answering")
		}
		sub.Unsubscribe()
		_ = h.sess.Tasks().Stop(t.Context(), info.ID, "")
		_, _ = h.sess.Tasks().Wait(t.Context(), info.ID)
		h.close()
	})
}

func TestSnapshotRunningTasks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		block := funcKind("block", kernel.TaskCaps{}, func(ctx context.Context, _ kernel.TaskRuntime) (kernel.TaskResult, error) {
			<-ctx.Done()
			return kernel.TaskResult{}, ctx.Err()
		})
		h := newHarness(t, nil, kernel.WithTaskKind(block), quiet)
		info := hostStart(t, h.sess, "block")
		synctest.Wait()
		if n := h.sess.Snapshot().RunningTasks; n != 1 {
			t.Fatalf("%d running, want 1", n)
		}
		_ = h.sess.Tasks().Stop(t.Context(), info.ID, "")
		_, _ = h.sess.Tasks().Wait(t.Context(), info.ID)
		if n := h.sess.Snapshot().RunningTasks; n != 0 {
			t.Fatalf("%d running after the stop, want 0", n)
		}
		h.close()
	})
}
