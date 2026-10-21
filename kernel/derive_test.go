package kernel_test

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/providertest"
)

func toolNames(sp *providertest.ScriptProvider) []string {
	reqs := sp.Requests()
	var names []string
	for _, d := range reqs[len(reqs)-1].Tools {
		names = append(names, d.Name)
	}
	return names
}

// keepRegistrar keeps the agent-scope Registrar, so a test can register on the parent after deriving from it.
func keepRegistrar(dst *kernel.Registrar) kernel.Option {
	return kernel.Use(kernel.ExtensionFunc("keep", func(r kernel.Registrar) error { *dst = r; return nil }))
}

// TestDeriveLayersOverParent: the derived agent reads the parent's registrations live through its filter, and stays under the parent's reviewers without InheritHooks.
func TestDeriveLayersOverParent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		var reviewed, audited []string
		reviewer := kernel.WithApprover(func(_ context.Context, req kernel.ApprovalRequest) (kernel.ApprovalDecision, error) {
			mu.Lock()
			reviewed = append(reviewed, req.Call.Name)
			mu.Unlock()
			return kernel.ApprovalDecision{Verdict: kernel.VerdictApprove}, nil
		})
		audit := kernel.WithHook(kernel.PreToolUse, "audit", func(_ context.Context, in kernel.PreToolUseInput) (kernel.PreToolUseOutput, error) {
			mu.Lock()
			audited = append(audited, in.Call.Name)
			mu.Unlock()
			return kernel.PreToolUseOutput{}, nil
		})
		rules, _ := kernel.ParseApprovalRules([]byte(`[{"id":"ask-a","tool":"a","action":"ask"}]`))
		var parent kernel.Registrar
		h := newHarness(t, [][]providertest.Step{toolReply(call{"c1", "a", `{}`}), textReply("done")},
			kernel.WithTool(&echoTool{name: "a"}), kernel.WithTool(&echoTool{name: "b"}), reviewer, audit, kernel.WithApprovalRules(rules...), keepRegistrar(&parent))
		d, err := h.ag.Derive(kernel.DeriveOptions{Nickname: "helper", ToolFilter: func(ts kernel.ToolSpec) bool { return ts.Name != "b" }, Limits: &kernel.Limits{MaxSteps: 3}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := parent.Tool(&echoTool{name: "c"}); err != nil {
			t.Fatal(err)
		}
		child, err := d.NewSession(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if res := ask(t, child, "go"); res.Outcome != kernel.OutcomeCompleted {
			t.Fatalf("outcome %s", res.Outcome)
		}
		if got := toolNames(h.sp); !slices.Equal(got, []string{"a", "c"}) {
			t.Fatalf("tools %v: b is filtered out, and c, registered on the parent afterwards, shows through", got)
		}
		mu.Lock()
		if !slices.Equal(reviewed, []string{"a"}) || len(audited) != 0 {
			t.Fatalf("reviewed %v, audited %v: reviewers always carry over, other hooks only with InheritHooks", reviewed, audited)
		}
		mu.Unlock()
		if err := child.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		h.close()
	})
}

// childKind makes a kind whose attempts open a ChildOf session through open and hand it to got.
func childKind(open func(ctx context.Context, rt kernel.TaskRuntime) (*kernel.Session, error), got chan<- *kernel.Session) kernel.TaskKind {
	return funcKind("child", kernel.TaskCaps{Instructions: true, Continue: true, Restart: true}, func(ctx context.Context, rt kernel.TaskRuntime) (kernel.TaskResult, error) {
		c, err := open(ctx, rt)
		if err != nil {
			return kernel.TaskResult{}, err
		}
		got <- c
		return kernel.TaskResult{Summary: "opened"}, nil
	})
}

// TestChildOfNeedsRunningAttempt: only the runtime of an attempt that is running can own a session.
func TestChildOfNeedsRunningAttempt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		kept := make(chan kernel.TaskRuntime, 1)
		k := funcKind("keep", kernel.TaskCaps{}, func(_ context.Context, rt kernel.TaskRuntime) (kernel.TaskResult, error) {
			kept <- rt
			return kernel.TaskResult{}, nil
		})
		h := newHarness(t, nil, kernel.WithTaskKind(k), quiet)
		info := hostStart(t, h.sess, "keep")
		rt := <-kept
		if _, err := h.sess.Tasks().Wait(t.Context(), info.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := h.ag.NewSession(t.Context(), kernel.ChildOf(rt)); err == nil {
			t.Fatal("a finished attempt owned a new session")
		}
		h.close()
	})
}

// TestChildHeldBetweenAttempts: a child session starts no turn of its own while its task has no running attempt, a continuation lets it go on, and the owner's close closes it.
func TestChildHeldBetweenAttempts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var ag *kernel.Agent
		var child *kernel.Session
		got := make(chan *kernel.Session, 2)
		k := childKind(func(ctx context.Context, rt kernel.TaskRuntime) (*kernel.Session, error) {
			if child != nil {
				_, err := child.WaitQuiet(ctx) // the kernel bound it to this attempt, so what it held back runs now
				return child, err
			}
			return ag.NewSession(ctx, kernel.ChildOf(rt))
		}, got)
		h := newHarness(t, [][]providertest.Step{textReply("later")}, kernel.WithTaskKind(k), quiet)
		ag = h.ag
		info := hostStart(t, h.sess, "child")
		child = <-got
		if p := child.Info().ParentSessionID; p != h.sess.Info().SessionID {
			t.Fatalf("parent %q", p)
		}
		if _, err := h.sess.Tasks().Wait(t.Context(), info.ID); err != nil {
			t.Fatal(err)
		}
		if _, err := child.Send(kernel.Input{Text: "note", Origin: message.Origin{Kind: message.OriginReminder}}); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if n := len(h.sp.Requests()); n != 0 {
			t.Fatalf("%d requests while the task had no running attempt", n)
		}
		if err := h.sess.Tasks().Send(t.Context(), info.ID, kernel.TaskInstruction{Text: "go on"}); err != nil {
			t.Fatal(err)
		}
		<-got
		if n := len(h.sp.Requests()); n != 1 {
			t.Fatalf("%d requests once the continuation ran", n)
		}
		h.close()
		if ph := child.Snapshot().Phase; ph != "closed" {
			t.Fatalf("child %s after its owner closed", ph)
		}
	})
}

// TestRestartClosesChildren: a restart starts from nothing, so the children of the replaced attempts are closed.
func TestRestartClosesChildren(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var ag *kernel.Agent
		got := make(chan *kernel.Session, 2)
		k := childKind(func(ctx context.Context, rt kernel.TaskRuntime) (*kernel.Session, error) {
			return ag.NewSession(ctx, kernel.ChildOf(rt))
		}, got)
		h := newHarness(t, nil, kernel.WithTaskKind(k), quiet)
		ag = h.ag
		info := hostStart(t, h.sess, "child")
		first := <-got
		if _, err := h.sess.Tasks().Wait(t.Context(), info.ID); err != nil {
			t.Fatal(err)
		}
		if err := h.sess.Tasks().Restart(t.Context(), info.ID, nil); err != nil {
			t.Fatal(err)
		}
		second := <-got
		synctest.Wait()
		if ph := first.Snapshot().Phase; ph != "closed" || second.Snapshot().Phase != "idle" {
			t.Fatalf("first child %s, second %s", ph, second.Snapshot().Phase)
		}
		h.close()
	})
}

// TestChildResumedOnlyWithChildOf: a child's file names its owner, so a plain Resume of it is refused.
func TestChildResumedOnlyWithChildOf(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var ag *kernel.Agent
		got := make(chan *kernel.Session, 2)
		k := childKind(func(ctx context.Context, rt kernel.TaskRuntime) (*kernel.Session, error) {
			return ag.NewSession(ctx, kernel.ChildOf(rt))
		}, got)
		h := newHarness(t, nil, kernel.WithTaskKind(k), quiet)
		ag = h.ag
		info := hostStart(t, h.sess, "child")
		first := <-got
		if _, err := h.sess.Tasks().Wait(t.Context(), info.ID); err != nil {
			t.Fatal(err)
		}
		if err := h.sess.Tasks().Restart(t.Context(), info.ID, nil); err != nil {
			t.Fatal(err)
		}
		<-got
		synctest.Wait()
		if _, err := ag.Resume(t.Context(), first.Info().SessionID); !errors.Is(err, kernel.ErrChildSession) {
			t.Fatalf("plain Resume of a closed child: %v", err)
		}
		h.close()
	})
}

// TestStopAndCloseCascade: once a stopped attempt returns, the tasks of its child sessions are stopped, and closing the owner closes the children, canceling the tool a child's turn runs.
func TestStopAndCloseCascade(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		childModel := provider.ModelRef{Provider: providertest.NewScriptProvider(provider.APIAnthropicMessages, toolReply(call{"c1", "echo", `{}`})), Model: "m"}
		opened := make(chan *kernel.Session, 1)
		var derived *kernel.Agent
		job := funcKind("job", kernel.TaskCaps{}, func(ctx context.Context, rt kernel.TaskRuntime) (kernel.TaskResult, error) {
			child, err := derived.NewSession(ctx, kernel.ChildOf(rt))
			if err != nil {
				return kernel.TaskResult{}, err
			}
			opened <- child
			<-ctx.Done()
			return kernel.TaskResult{}, ctx.Err()
		})
		echo := &echoTool{name: "echo", delay: time.Hour}
		h := newHarness(t, nil, kernel.WithTaskKind(job), kernel.WithTool(echo), kernel.WithTool(fetchTool(0, time.Hour)), quiet)
		var err error
		if derived, err = h.ag.Derive(kernel.DeriveOptions{Model: &childModel}); err != nil {
			t.Fatal(err)
		}
		info := hostStart(t, h.sess, "job")
		child := <-opened
		inner, err := hostExec(t, child, `{"url":"u"}`)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := child.Send(kernel.Text("work")); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if echo.runsOf("c1") != 1 {
			t.Fatal("the child's turn is not in its tool")
		}
		if err := h.sess.Tasks().Stop(t.Context(), info.ID, "enough"); err != nil {
			t.Fatal(err)
		}
		if got, _ := h.sess.Tasks().Wait(t.Context(), info.ID); got.Status != kernel.TaskCanceled {
			t.Fatalf("task %s", got.Status)
		}
		synctest.Wait() // the stop is requested as the attempt ends, and lands when the child's task returns
		if got, _ := child.Tasks().Get(inner.ID); got.Status != kernel.TaskCanceled {
			t.Fatalf("the child's task is %s once its owner's attempt ended", got.Status)
		}
		h.close()
		if sn := child.Snapshot(); sn.Phase != "closed" || len(sn.Workers) != 0 {
			t.Fatalf("child %s with workers %v after the owner closed", sn.Phase, sn.Workers)
		}
		lines, err := h.store.Load(t.Context(), child.Info().SessionID)
		if err != nil {
			t.Fatal(err)
		}
		if err := pairedHistory(lines); err != nil || roles(lines) != "system,user,assistant,tool" {
			t.Fatalf("child history %s: %v", roles(lines), err)
		}
	})
}

// TestChildWaitsOnAncestorsReentrant: a child's tool waiting on its owner session or the task that owns it would wait on itself, so it fails fast.
func TestChildWaitsOnAncestorsReentrant(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var owner *kernel.Session
		got := map[string]error{}
		tool := &probeTool{run: func(ctx context.Context) string {
			_, got["WaitQuiet"] = owner.WaitQuiet(ctx)
			_, got["Tasks().Wait"] = owner.Tasks().Wait(ctx, "task_01")
			return "probed"
		}}
		childModel := provider.ModelRef{Provider: providertest.NewScriptProvider(provider.APIAnthropicMessages, toolReply(call{"c1", "probe", `{}`}), textReply("done")), Model: "m"}
		var derived *kernel.Agent
		job := funcKind("job", kernel.TaskCaps{}, func(ctx context.Context, rt kernel.TaskRuntime) (kernel.TaskResult, error) {
			child, err := derived.NewSession(ctx, kernel.ChildOf(rt))
			if err != nil {
				return kernel.TaskResult{}, err
			}
			rc, err := child.Send(kernel.Text("probe"))
			if err != nil {
				return kernel.TaskResult{}, err
			}
			_, err = rc.Wait(ctx)
			return kernel.TaskResult{}, err
		})
		h := newHarness(t, nil, kernel.WithTaskKind(job), kernel.WithTool(tool), quiet)
		owner = h.sess
		var err error
		if derived, err = h.ag.Derive(kernel.DeriveOptions{Model: &childModel}); err != nil {
			t.Fatal(err)
		}
		if res, _ := h.sess.Tasks().Wait(t.Context(), hostStart(t, h.sess, "job").ID); res.Status != kernel.TaskSucceeded {
			t.Fatalf("task %s %s", res.Status, res.Error)
		}
		if len(got) != 2 {
			t.Fatalf("probed %v", got)
		}
		for method, err := range got {
			if !errors.Is(err, kernel.ErrReentrantWait) {
				t.Fatalf("%s from the child's tool: %v", method, err)
			}
		}
		h.close()
	})
}

// TestChildSeesOwnerReviewer: a child session reviews with its owner's reviewers, so it does not warn that nobody can review its ask rules.
func TestChildSeesOwnerReviewer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var ag *kernel.Agent
		childRec := &recorder{}
		got := make(chan *kernel.Session, 1)
		k := childKind(func(ctx context.Context, rt kernel.TaskRuntime) (*kernel.Session, error) {
			return ag.NewSession(ctx, kernel.ChildOf(rt), kernel.WithSessionFunc(childRec.handle))
		}, got)
		h := newHarness(t, nil, kernel.WithTaskKind(k), quiet, kernel.WithApprovalRules(kernel.ApprovalRule{ID: "review-echo", Tool: "echo", Action: kernel.RuleAsk}))
		ag = h.ag
		if _, err := kernel.On(h.sess.Registrar(), kernel.Approval, "owner", func(context.Context, kernel.ApprovalRequest) (kernel.ApprovalDecision, error) {
			return kernel.ApprovalDecision{Verdict: kernel.VerdictApprove}, nil
		}); err != nil {
			t.Fatal(err)
		}
		hostStart(t, h.sess, "child")
		<-got
		synctest.Wait()
		for _, ev := range childRec.all() {
			if n, ok := ev.Data.(*event.Notice); ok && n.Source == "approval" {
				t.Fatalf("the child warned: %s", n.Text)
			}
		}
		h.close()
	})
}
