package kernel_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/providertest"
)

// failing answers like a broken hook: an error, a panic, or no answer before the 10ms timeout the scenario sets.
func failing(kind string) func(ctx context.Context) error {
	return func(ctx context.Context) error {
		switch kind {
		case "panic":
			panic("hook bug")
		case "timeout":
			<-ctx.Done()
			return ctx.Err()
		}
		return errors.New("hook broke")
	}
}

type matrixRun struct {
	rec     *recorder
	lines   []message.Message
	err     error // registration
	taskErr error // what the host's task start returned
}

// runMatrix reaches every kernel point once: a switch, a command, a turn with one reviewed tool call and a final answer, a compaction, a task whose notice starts a turn, and close.
func runMatrix(t *testing.T, point kernel.AnyHookPoint, async bool, kind string, policy kernel.FailPolicy) matrixRun {
	var run matrixRun
	synctest.Test(t, func(t *testing.T) {
		scripts := func() *providertest.ScriptProvider {
			return providertest.NewScriptProvider(provider.APIAnthropicMessages, toolReply(call{"toolu_1", "echo", `{"x":1}`}), textReply("done"), textReply("noted"))
		}
		from, to := scripts(), scripts()
		opts := []kernel.HookOption{kernel.Timeout(10 * time.Millisecond), kernel.Priority(-1)}
		if policy != kernel.FailDefault {
			opts = append(opts, kernel.Fail(policy))
		}
		fail := failing(kind)
		ext := kernel.ExtensionFunc("matrix", func(r kernel.Registrar) error {
			if async {
				_, err := kernel.OnJSONAsync(r, point, "broken", func(ctx context.Context, _ json.RawMessage) (kernel.Injection, error) {
					return kernel.Injection{}, fail(ctx)
				}, opts...)
				return err
			}
			_, err := kernel.OnJSON(r, point, "broken", func(ctx context.Context, _ json.RawMessage) (json.RawMessage, error) {
				return nil, fail(ctx)
			}, opts...)
			return err
		})
		rec := &recorder{}
		ag, err := kernel.New(
			kernel.WithModel(provider.ModelRef{Provider: from, Model: "m"}),
			kernel.WithCapabilityResolver(nil),
			kernel.WithPanicOnInternalError(true),
			kernel.WithTool(&echoTool{name: "echo"}),
			kernel.WithCommand(kernel.Command{Name: "ping", Busy: kernel.RunNow, Run: func(context.Context, *kernel.CommandContext) (kernel.CommandResult, error) {
				return kernel.CommandResult{Text: "pong"}, nil
			}}),
			kernel.WithApprovalRules(kernel.ApprovalRule{ID: "review-echo", Tool: "echo", Action: kernel.RuleAsk}),
			kernel.WithApprover(func(context.Context, kernel.ApprovalRequest) (kernel.ApprovalDecision, error) {
				return kernel.ApprovalDecision{Verdict: kernel.VerdictApprove}, nil
			}),
			kernel.Use(ext),
			kernel.Use(kernel.ExtensionFunc("stub-compactor", func(r kernel.Registrar) error {
				_, err := r.Compactor(stubCompactor{})
				return err
			})),
			kernel.WithTaskKind(kernel.TaskKind{Name: "job", New: func(kernel.TaskSpec) (kernel.Task, error) { return doneTask{}, nil }}),
		)
		if err != nil {
			run.err = err
			return
		}
		sess, err := ag.NewSession(t.Context(), kernel.WithSessionFunc(rec.handle))
		if err != nil {
			t.Fatal(err)
		}
		if err := sess.SwitchModel(t.Context(), provider.ModelRef{Provider: to, Model: "m2"}); err != nil {
			t.Fatal(err)
		}
		cmd, err := sess.Command("ping", "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := cmd.Wait(t.Context()); err != nil {
			t.Fatal(err)
		}
		rc, err := sess.Send(kernel.Text("go"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := rc.Wait(t.Context()); err != nil {
			t.Fatal(err)
		}
		_, _ = sess.Compact(t.Context(), "") // pre_compact, session_start{compact} and post_compact; a blocking hook may refuse it
		_, run.taskErr = sess.Tasks().Start(t.Context(), kernel.TaskSpec{Kind: "job", Title: "job", Input: json.RawMessage(`{}`)})
		if _, err := sess.WaitQuiet(t.Context()); err != nil { // task_notify, and the turn its notice starts
			t.Fatal(err)
		}
		time.Sleep(time.Second) // async hooks of turn_end, and every async timeout, run out
		if err := ag.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if w := sess.Snapshot().Workers; len(w) != 0 {
			t.Fatalf("workers left: %v", w)
		}
		run.rec = rec
		for _, ev := range rec.all() {
			if d, ok := ev.Data.(*event.MessageCompleted); ok {
				run.lines = append(run.lines, d.Message)
			}
		}
	})
	return run
}

// closedEffect is what a failing hook does at a fail-closed point; points without one treat the failure as no opinion.
var closedEffect = map[string]func(t *testing.T, r matrixRun, kind string){
	"user_prompt_submit": func(t *testing.T, r matrixRun, _ string) {
		if !hasEvent(r.rec, event.TypeInputDropped, func(d event.Payload) bool { return d.(*event.InputDropped).Reason == "blocked" }) {
			t.Fatal("the input was not dropped as blocked")
		}
	},
	"command_invoke": func(t *testing.T, r matrixRun, _ string) {
		if !hasEvent(r.rec, event.TypeCommandCompleted, func(d event.Payload) bool { return d.(*event.CommandCompleted).Error == "blocked" }) {
			t.Fatal("the command was not blocked")
		}
	},
	"pre_tool_use": func(t *testing.T, r matrixRun, kind string) {
		if got := result(r.lines, "toolu_1"); got.Outcome != message.OutcomeDenied || !strings.Contains(textOf(got.Content), "policy hook failed ("+kind+")") {
			t.Fatalf("result %+v", got)
		}
	},
	"approval": func(t *testing.T, r matrixRun, kind string) {
		if got := result(r.lines, "toolu_1"); got.Outcome != message.OutcomeDenied || !strings.Contains(textOf(got.Content), "reviewer failed ("+kind+")") {
			t.Fatalf("result %+v", got)
		}
		want := "broken"
		if kind == "timeout" {
			want = "timeout"
		}
		if !hasEvent(r.rec, event.TypeApprovalResolved, func(d event.Payload) bool { return d.(*event.ApprovalResolved).By == want }) {
			t.Fatalf("approval.resolved.by is not %s", want)
		}
	},
	"task_start": func(t *testing.T, r matrixRun, kind string) {
		var de *kernel.TaskDeniedError
		if !errors.As(r.taskErr, &de) || !strings.Contains(de.Text, "task_start hook failed ("+kind+")") {
			t.Fatalf("start: %v", r.taskErr)
		}
	},
	"provider_switch": func(t *testing.T, r matrixRun, _ string) {
		if !hasEvent(r.rec, event.TypeProviderSwitchRejected, func(d event.Payload) bool { return d.(*event.ProviderSwitchRejected).Reason == "hook_blocked" }) {
			t.Fatal("the switch was not blocked")
		}
	},
}

func hasEvent(r *recorder, typ event.Type, pred func(event.Payload) bool) bool {
	for _, ev := range r.all() {
		if ev.Type == typ && pred(ev.Data) {
			return true
		}
	}
	return false
}

func result(lines []message.Message, id string) message.ToolResultBlock {
	for _, m := range lines {
		for _, b := range m.Content {
			if r, ok := b.(*message.ToolResultBlock); ok && r.CallID == id {
				return *r
			}
		}
	}
	return message.ToolResultBlock{}
}

func textOf(c message.Content) string {
	var b strings.Builder
	for _, x := range c {
		if tb, ok := x.(*message.TextBlock); ok {
			b.WriteString(tb.Text)
		}
	}
	return b.String()
}

// TestHookMatrix runs every kernel point × sync/async × timeout/panic/error × open/closed: the failure is reported once, a fail-closed point applies its closed output, and nothing else changes.
func TestHookMatrix(t *testing.T) {
	for _, point := range kernel.HookPoints() {
		for _, async := range []bool{false, true} {
			for _, kind := range []string{"error", "panic", "timeout"} {
				for _, policy := range []kernel.FailPolicy{kernel.FailOpen, kernel.FailClosed} {
					name := point.Name() + "/" + map[bool]string{false: "sync", true: "async"}[async] + "/" + kind + "/" + map[kernel.FailPolicy]string{kernel.FailOpen: "open", kernel.FailClosed: "closed"}[policy]
					t.Run(name, func(t *testing.T) {
						r := runMatrix(t, point, async, kind, policy)
						switch {
						case point.Name() == "approval" && async:
							if !errors.Is(r.err, kernel.ErrAsyncNotAllowed) {
								t.Fatalf("registration: %v", r.err)
							}
							return
						case point.Name() == "approval" && policy == kernel.FailOpen:
							if r.err == nil {
								t.Fatal("approval accepted a fail-open reviewer")
							}
							return
						case r.err != nil:
							t.Fatal(r.err)
						}
						var failed []*event.HookFailed
						for _, ev := range r.rec.all() {
							if d, ok := ev.Data.(*event.HookFailed); ok && d.Name == "broken" {
								failed = append(failed, d)
							}
						}
						if len(failed) == 0 {
							t.Fatal("no hook.failed")
						}
						f := failed[0]
						if f.Point != point.Name() || f.Timeout != (kind == "timeout") || f.Canceled {
							t.Fatalf("hook.failed %+v", f)
						}
						effect, closedPoint := closedEffect[point.Name()]
						wantClosed := !async && closedPoint && policy == kernel.FailClosed
						if want := map[bool]string{false: "open", true: "closed"}[wantClosed]; f.PolicyApplied != want {
							t.Fatalf("policy_applied %s, want %s", f.PolicyApplied, want)
						}
						if wantClosed {
							effect(t, r, kind)
							return
						}
						if got := result(r.lines, "toolu_1"); got.Outcome != message.OutcomeSuccess {
							t.Fatalf("a hook with no opinion changed the result: %+v", got)
						}
					})
				}
			}
		}
	}
}

func TestHookRegistrationRules(t *testing.T) {
	check := func(r kernel.Registrar) error {
		noop := func(context.Context, kernel.StopInput) (kernel.StopOutput, error) { return kernel.StopOutput{}, nil }
		if _, err := kernel.On(r, kernel.Stop, "x", noop, kernel.Match(kernel.MatchNames("a"))); !errors.Is(err, kernel.ErrNoMatcher) {
			t.Errorf("a matcher on a keyless point: %v", err)
		}
		if _, err := kernel.On(r, kernel.Stop, "bad name", noop); !errors.Is(err, kernel.ErrInvalidName) {
			t.Errorf("invalid name: %v", err)
		}
		if _, err := kernel.On(r, kernel.Stop, "x", noop); err != nil {
			t.Error(err)
		}
		if _, err := kernel.On(r, kernel.Stop, "x", noop); !errors.Is(err, kernel.ErrDuplicate) {
			t.Errorf("duplicate: %v", err)
		}
		pre := func(context.Context, kernel.PreToolUseInput) (kernel.PreToolUseOutput, error) {
			return kernel.PreToolUseOutput{}, nil
		}
		if _, err := kernel.On(r, kernel.PreToolUse, "re", pre, kernel.Match(kernel.MatchRegexp("(?i)x"))); err == nil {
			t.Error("an inline flag passed the portable subset")
		}
		reg, err := kernel.On(r, kernel.PreToolUse, "re", pre, kernel.Match(kernel.MatchRegexp(`sql_\w+`)))
		if err != nil {
			t.Error(err)
		}
		reg.Remove()
		reg.Remove() // idempotent
		if _, err := kernel.On(r, kernel.PreToolUse, "re", pre); err != nil {
			t.Errorf("the name is free again after Remove: %v", err)
		}
		approve := func(context.Context, kernel.ApprovalRequest) (kernel.ApprovalDecision, error) {
			return kernel.ApprovalDecision{}, nil
		}
		if _, err := kernel.On(r, kernel.Approval, "open", approve, kernel.Fail(kernel.FailOpen)); err == nil {
			t.Error("approval accepted a fail-open reviewer")
		}
		if _, err := kernel.OnAsync(r, kernel.Approval, "async", func(context.Context, kernel.ApprovalRequest) (kernel.Injection, error) {
			return kernel.Injection{}, nil
		}); !errors.Is(err, kernel.ErrAsyncNotAllowed) {
			t.Errorf("async reviewer: %v", err)
		}
		return nil
	}
	if _, err := kernel.New(kernel.WithModel(provider.ModelRef{Provider: providertest.NewScriptProvider(provider.APIAnthropicMessages), Model: "m"}),
		kernel.Use(kernel.ExtensionFunc("check", check))); err != nil {
		t.Fatal(err)
	}
}

func TestParseMatcher(t *testing.T) {
	for _, c := range []struct {
		form string
		hit  []string
		miss []string
		err  bool
	}{
		{form: "", hit: []string{"anything"}},
		{form: "*", hit: []string{"anything"}},
		{form: "echo|sql_exec", hit: []string{"echo", "sql_exec"}, miss: []string{"Echo", "sql"}},
		{form: "re:Edit.*", hit: []string{"Edit", "EditFile"}, miss: []string{"NotebookEdit"}},
		{form: "re:^x", err: true},
		{form: "a b", err: true},
	} {
		m, err := kernel.ParseMatcher(c.form)
		if (err != nil) != c.err {
			t.Fatalf("%q: %v", c.form, err)
		}
		for _, k := range c.hit {
			if m != nil && !m.Match(k) {
				t.Fatalf("%q misses %q", c.form, k)
			}
		}
		for _, k := range c.miss {
			if m == nil || m.Match(k) {
				t.Fatalf("%q matches %q", c.form, k)
			}
		}
	}
}

// TestHookOrderAndRewrite checks run order (Priority, then agent before session, then registration) and that each hook sees the previous one's rewrite.
func TestHookOrderAndRewrite(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var order []string
		mk := func(name, suffix string) kernel.HookFunc[kernel.CommandInvokeInput, kernel.CommandInvokeOutput] {
			return func(_ context.Context, in kernel.CommandInvokeInput) (kernel.CommandInvokeOutput, error) {
				order = append(order, name+":"+in.Args)
				args := in.Args + suffix
				return kernel.CommandInvokeOutput{UpdatedArgs: &args}, nil
			}
		}
		var got string
		h := newHarness(t, nil,
			kernel.WithHook(kernel.CommandInvoke, "late", mk("late", "L"), kernel.Priority(5)),
			kernel.WithHook(kernel.CommandInvoke, "agent", mk("agent", "A")),
			kernel.WithCommand(kernel.Command{Name: "say", Busy: kernel.RunNow, Run: func(_ context.Context, cc *kernel.CommandContext) (kernel.CommandResult, error) {
				got = cc.Args()
				return kernel.CommandResult{}, nil
			}}))
		if _, err := kernel.On(h.sess.Registrar(), kernel.CommandInvoke, "session", mk("session", "S")); err != nil {
			t.Fatal(err)
		}
		if _, err := kernel.On(h.sess.Registrar(), kernel.CommandInvoke, "first", mk("first", "F"), kernel.Priority(-1)); err != nil {
			t.Fatal(err)
		}
		rc, err := h.sess.Command("say", "x")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := rc.Wait(t.Context()); err != nil {
			t.Fatal(err)
		}
		if want := []string{"first:x", "agent:xF", "session:xFA", "late:xFAS"}; !slices.Equal(order, want) {
			t.Fatalf("order %v, want %v", order, want)
		}
		if got != "xFASL" {
			t.Fatalf("the command got %q", got)
		}
		h.close()
	})
}

// TestHookRecursionCapped fires an extension point from its own hook; beyond the depth cap Fire returns nothing and reports it.
func TestHookRecursionCapped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		type in struct{ N int }
		type out struct{ Depth int }
		point := kernel.NewHookPoint(kernel.HookPointDef[in, out]{Name: "ext_point", Timeout: time.Second, Fail: kernel.FailOpen})
		h := newHarness(t, nil)
		deepest := 0
		if _, err := kernel.On(h.sess.Registrar(), point, "again", func(ctx context.Context, v in) (out, error) {
			deepest = max(deepest, v.N)
			o, _ := kernel.Fire(ctx, h.sess, point, in{N: v.N + 1})
			return out{Depth: o.Depth + 1}, nil
		}); err != nil {
			t.Fatal(err)
		}
		o, _ := kernel.Fire(t.Context(), h.sess, point, in{N: 1})
		if o.Depth != 4 || deepest != 4 {
			t.Fatalf("depth %d, deepest call %d, want 4", o.Depth, deepest)
		}
		synctest.Wait() // the recorder's pump delivers the error event asynchronously
		if !hasEvent(h.rec, event.TypeError, func(d event.Payload) bool { return d.(*event.Error).Code == "hook_recursion" }) {
			t.Fatal("no hook_recursion error")
		}
		h.close()
	})
}

// TestAsyncHookPoolFull drops an async call when every slot is busy and reports it.
func TestAsyncHookPoolFull(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		release := make(chan struct{})
		h := newHarness(t, [][]providertest.Step{textReply("one")},
			kernel.WithLimits(kernel.Limits{MaxAsyncHooks: 1}),
			kernel.WithAsyncHook(kernel.TurnEnd, "slow", func(ctx context.Context, _ kernel.TurnEndInput) (kernel.Injection, error) {
				select {
				case <-release:
				case <-ctx.Done():
				}
				return kernel.Injection{}, nil
			}),
			kernel.WithAsyncHook(kernel.TurnEnd, "second", func(context.Context, kernel.TurnEndInput) (kernel.Injection, error) {
				return kernel.Injection{}, nil
			}))
		rc, _ := h.sess.Send(kernel.Text("hi"))
		_, _ = rc.Wait(t.Context())
		synctest.Wait()
		if !hasEvent(h.rec, event.TypeError, func(d event.Payload) bool { return d.(*event.Error).Code == "async_hook_dropped" }) {
			t.Fatal("no async_hook_dropped")
		}
		close(release)
		h.close()
	})
}

// TestTurnEndSharesOneBudget ends the turn within one turn_end timeout however many slow hooks there are.
func TestTurnEndSharesOneBudget(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		slow := func(ctx context.Context, _ kernel.TurnEndInput) (kernel.TurnEndOutput, error) {
			select {
			case <-time.After(8 * time.Second):
			case <-ctx.Done():
			}
			return kernel.TurnEndOutput{}, nil
		}
		ran := 0
		h := newHarness(t, [][]providertest.Step{textReply("one")},
			kernel.WithHook(kernel.TurnEnd, "a", slow),
			kernel.WithHook(kernel.TurnEnd, "b", slow),
			kernel.WithHook(kernel.TurnEnd, "c", func(context.Context, kernel.TurnEndInput) (kernel.TurnEndOutput, error) {
				ran++
				return kernel.TurnEndOutput{}, nil
			}))
		start := time.Now()
		rc, _ := h.sess.Send(kernel.Text("hi"))
		_, _ = rc.Wait(t.Context())
		if took := time.Since(start); took > 12*time.Second {
			t.Fatalf("the turn ended after %v, want within one 10s budget plus grace", took)
		}
		if ran != 0 {
			t.Fatal("a hook ran after the budget was spent")
		}
		h.close()
	})
}

// TestSessionEndWaitsForAsyncHooks lets an async session_end hook finish before the session context is canceled.
func TestSessionEndWaitsForAsyncHooks(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		done := false
		h := newHarness(t, nil, kernel.WithAsyncHook(kernel.SessionEnd, "ingest", func(ctx context.Context, in kernel.SessionEndInput) (kernel.Injection, error) {
			time.Sleep(time.Second)
			done = ctx.Err() == nil && in.Reason == "closed"
			return kernel.Injection{}, nil
		}))
		h.close()
		if !done {
			t.Fatal("the async session_end hook was canceled before it finished")
		}
	})
}

// stubCompactor summarizes without a model call and keeps nothing.
type stubCompactor struct{}

func (stubCompactor) Compact(_ context.Context, req kernel.CompactRequest) (kernel.CompactResult, error) {
	return kernel.CompactResult{Summary: "Earlier: the user said go.", Strategy: "stub"}, nil
}

func TestBlockedInputAndCommandReceipts(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, nil,
			kernel.WithHook(kernel.UserPromptSubmit, "no", func(context.Context, kernel.UserPromptSubmitInput) (kernel.UserPromptSubmitOutput, error) {
				return kernel.UserPromptSubmitOutput{Block: true}, nil
			}),
			kernel.WithHook(kernel.CommandInvoke, "no", func(context.Context, kernel.CommandInvokeInput) (kernel.CommandInvokeOutput, error) {
				return kernel.CommandInvokeOutput{Block: true, Reason: "frozen"}, nil
			}),
			kernel.WithCommand(kernel.Command{Name: "go", Run: func(context.Context, *kernel.CommandContext) (kernel.CommandResult, error) {
				return kernel.CommandResult{}, nil
			}}))
		rc, _ := h.sess.Send(kernel.Text("hi"))
		if res, _ := rc.Wait(t.Context()); res.Outcome != kernel.OutcomeDropped {
			t.Fatalf("blocked input outcome %s", res.Outcome)
		}
		rc, _ = h.sess.Command("go", "")
		res, _ := rc.Wait(t.Context())
		if !errors.Is(res.Err, kernel.ErrBlocked) || res.Command.Text != "frozen" {
			t.Fatalf("blocked command %+v", res)
		}
		if len(h.lines()) != 1 {
			t.Fatal("a blocked input reached history")
		}
		h.close()
	})
}

// TestHaltAndNextTurnReasoning: a reasoning change from pre_model_request reaches the next turn only, and a halt ends the turn before its model call.
func TestHaltAndNextTurnReasoning(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		calls := 0
		h := newHarness(t, [][]providertest.Step{textReply("one"), textReply("two"), textReply("four")},
			kernel.WithHook(kernel.PreModelRequest, "gate", func(_ context.Context, in kernel.PreModelRequestInput) (kernel.PreModelRequestOutput, error) {
				calls++
				switch calls {
				case 1:
					return kernel.PreModelRequestOutput{NextTurnReasoning: &provider.ReasoningParams{Effort: "high"}}, nil
				case 3:
					return kernel.PreModelRequestOutput{Common: kernel.Common{Halt: true}}, nil
				}
				return kernel.PreModelRequestOutput{}, nil
			}))
		for _, text := range []string{"a", "b", "c", "d"} {
			rc, _ := h.sess.Send(kernel.Text(text))
			_, _ = rc.Wait(t.Context())
		}
		synctest.Wait()
		var efforts []string
		for _, r := range h.sp.Requests() {
			efforts = append(efforts, r.Params.Reasoning.Effort)
		}
		if strings.Join(efforts, ",") != ",high," {
			t.Fatalf("efforts %q", efforts)
		}
		var outcomes []string
		for _, ev := range h.rec.all() {
			if d, ok := ev.Data.(*event.TurnCompleted); ok {
				outcomes = append(outcomes, d.Outcome)
			}
		}
		h.close()
		if strings.Join(outcomes, ",") != "completed,completed,hook_stopped,completed" {
			t.Fatalf("outcomes %v", outcomes)
		}
	})
}

// TestHookSeesSessionAndCannotWaitOnIt: a typed hook gets the session handle, and waiting on its own session from the hook fails fast.
func TestHookSeesSessionAndCannotWaitOnIt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var waitErr error
		var h *harness
		h = newHarness(t, [][]providertest.Step{textReply("ok")},
			kernel.WithHook(kernel.TurnEnd, "waiter", func(ctx context.Context, in kernel.TurnEndInput) (kernel.TurnEndOutput, error) {
				rc, err := in.Session.Send(kernel.Text("later"), kernel.NoWake())
				if err != nil {
					return kernel.TurnEndOutput{}, err
				}
				_, waitErr = rc.Wait(ctx)
				return kernel.TurnEndOutput{}, nil
			}, kernel.Match(kernel.MatchNames("completed"))))
		rc, _ := h.sess.Send(kernel.Text("hi"))
		_, _ = rc.Wait(t.Context())
		if !errors.Is(waitErr, kernel.ErrReentrantWait) {
			t.Fatalf("wait from a hook: %v", waitErr)
		}
		h.close()
	})
}

func TestJSONForms(t *testing.T) {
	b, _ := json.Marshal(kernel.ToolSpec{Name: "x", Timeout: kernel.NoTimeout})
	var back kernel.ToolSpec
	if err := json.Unmarshal(b, &back); err != nil || !strings.Contains(string(b), `"timeout_ms":-1`) || back.Timeout != kernel.NoTimeout {
		t.Fatalf("%s → %+v %v", b, back, err)
	}
	b, _ = json.Marshal(kernel.ToolSpec{Name: "x", Timeout: 1500 * time.Millisecond})
	if !strings.Contains(string(b), `"timeout_ms":1500`) {
		t.Fatalf("%s", b)
	}
	b, _ = json.Marshal(kernel.TurnResult{TurnID: "turn_1", Outcome: kernel.OutcomeError, Err: errors.New("boom")})
	if !strings.Contains(string(b), `"error":"boom"`) || !strings.Contains(string(b), `"outcome":"error"`) {
		t.Fatalf("%s", b)
	}
}

// TestInvalidJSONHookOutput counts an output that does not decode as the hook failing, so a closed point applies its closed output.
func TestInvalidJSONHookOutput(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ext := kernel.ExtensionFunc("bad", func(r kernel.Registrar) error {
			_, err := kernel.OnJSON(r, kernel.PreToolUse, "garbled", func(context.Context, json.RawMessage) (json.RawMessage, error) {
				return json.RawMessage(`{"decision":`), nil
			})
			return err
		})
		h := newHarness(t, [][]providertest.Step{toolReply(call{"toolu_1", "echo", `{}`}), textReply("ok")}, kernel.WithTool(&echoTool{name: "echo"}), kernel.Use(ext))
		rc, _ := h.sess.Send(kernel.Text("go"))
		_, _ = rc.Wait(t.Context())
		if r := result(h.lines(), "toolu_1"); r.Outcome != message.OutcomeDenied {
			t.Fatalf("result %+v", r)
		}
		h.close()
	})
}

// doneTask succeeds at once, which is all a hook point around it needs.
type doneTask struct{}

func (doneTask) Run(context.Context, kernel.TaskRuntime) (kernel.TaskResult, error) {
	return kernel.TaskResult{Summary: "done"}, nil
}
