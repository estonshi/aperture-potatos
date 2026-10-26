package kernel_test

import (
	"context"
	"errors"
	"slices"
	"testing"
	"testing/synctest"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/providertest"
)

// TestRuntimeApprovalRuleAppliesToTheNextGate: a registered rule denies from the next gate on, and Remove restores the old policy.
func TestRuntimeApprovalRuleAppliesToTheNextGate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, [][]providertest.Step{
			toolReply(call{"toolu_1", "echo", `{"x":1}`}), textReply("denied done"),
			toolReply(call{"toolu_2", "echo", `{"x":2}`}), textReply("allowed done"),
		}, kernel.WithTool(&echoTool{name: "echo"}))
		reg, err := h.sess.Registrar().ApprovalRule(kernel.ApprovalRule{ID: "late", Tool: "echo", Action: kernel.RuleDeny, Reason: "late deny"})
		if err != nil {
			t.Fatal(err)
		}
		ask(t, h.sess, "one")
		reg.Remove()
		ask(t, h.sess, "two")
		lines := h.lines()
		if r := result(lines, "toolu_1"); r.Outcome != message.OutcomeDenied || textOf(r.Content) != "Denied by rule late: late deny" {
			t.Fatalf("before remove: %+v", r)
		}
		if r := result(lines, "toolu_2"); r.Outcome != message.OutcomeSuccess {
			t.Fatalf("after remove: %+v", r)
		}
		if got := h.sess.ApprovalRules(); len(got) != 0 {
			t.Fatalf("rules after remove: %+v", got)
		}
		h.close()
		var changes []string
		for _, ev := range h.rec.all() {
			if rc, ok := ev.Data.(*event.RegistryChanged); ok && rc.Kind == "approval_rule" {
				changes = append(changes, rc.Scope+":"+rc.Op+":"+rc.Name+":"+rc.Effective)
			}
		}
		if want := []string{"session:add:late:now", "session:remove:late:now"}; !slices.Equal(changes, want) {
			t.Fatalf("rule registry.changed: %v, want %v", changes, want)
		}
	})
}

// TestApprovalRuleScopesConflictAndExport: one id may not cross scopes, and ApprovalRules reports each rule with its scope and whether it can be written to a file.
func TestApprovalRuleScopesConflictAndExport(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var agentReg kernel.Registrar
		sp := providertest.NewScriptProvider(provider.APIAnthropicMessages)
		ag, err := kernel.New(
			kernel.WithModel(provider.ModelRef{Provider: sp, Model: "m"}),
			keepRegistrar(&agentReg),
			kernel.WithApprovalRules(kernel.ApprovalRule{ID: "agent-rule", Tool: "echo", Action: kernel.RuleAsk}),
		)
		if err != nil {
			t.Fatal(err)
		}
		sess, err := ag.NewSession(t.Context(), kernel.SessionApprovalRules(kernel.ApprovalRule{ID: "session-rule", Tool: "echo", Action: kernel.RuleDeny}))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ag.NewSession(t.Context(), kernel.SessionApprovalRules(kernel.ApprovalRule{ID: "agent-rule", Tool: "x", Action: kernel.RuleDeny})); !errors.Is(err, kernel.ErrDuplicate) {
			t.Fatalf("session rule over an agent id: %v", err)
		}
		if _, err := sess.Registrar().ApprovalRule(kernel.ApprovalRule{ID: "agent-rule", Tool: "x", Action: kernel.RuleDeny}); !errors.Is(err, kernel.ErrDuplicate) {
			t.Fatalf("runtime session rule over an agent id: %v", err)
		}
		if _, err := agentReg.ApprovalRule(kernel.ApprovalRule{ID: "session-rule", Tool: "x", Action: kernel.RuleDeny}); !errors.Is(err, kernel.ErrDuplicate) {
			t.Fatalf("agent rule over a live session id: %v", err)
		}
		got := sess.ApprovalRules()
		if len(got) != 2 || got[0].ID != "agent-rule" || got[0].Scope != "agent" || got[1].ID != "session-rule" || got[1].Scope != "session" {
			t.Fatalf("rules %+v", got)
		}
		if !got[0].Portable || !got[1].Portable {
			t.Fatalf("a rule without a predicate is portable: %+v", got)
		}
		if _, err := sess.Registrar().ApprovalRule(kernel.ApprovalRule{ID: "native", Tool: "echo", Action: kernel.RuleAllow, Match: func(kernel.ToolCallView) bool { return true }}); err != nil {
			t.Fatal(err)
		}
		got = sess.ApprovalRules()
		if len(got) != 3 || got[2].ID != "native" || got[2].Portable {
			t.Fatalf("native rule: %+v", got)
		}
		if err := ag.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}

// TestChildOfInheritsRulesAndRefusesDuplicates: the owner's rules arrive once, and a child rule that reuses one of their ids is an error rather than a silent drop.
func TestChildOfInheritsRulesAndRefusesDuplicates(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var ag *kernel.Agent
		kept := make(chan kernel.TaskRuntime, 1)
		release := make(chan struct{})
		h := newHarness(t, nil, kernel.WithTaskKind(funcKind("hold", kernel.TaskCaps{}, func(_ context.Context, rt kernel.TaskRuntime) (kernel.TaskResult, error) {
			kept <- rt
			<-release
			return kernel.TaskResult{}, nil
		})), quiet, kernel.WithApprovalRules(kernel.ApprovalRule{ID: "dup", Tool: "echo", Action: kernel.RuleAsk}))
		ag = h.ag
		hostStart(t, h.sess, "hold")
		rt := <-kept
		if _, err := ag.NewSession(t.Context(), kernel.ChildOf(rt), kernel.SessionApprovalRules(kernel.ApprovalRule{ID: "dup", Tool: "echo", Action: kernel.RuleDeny})); !errors.Is(err, kernel.ErrDuplicate) {
			t.Fatalf("duplicate child rule: %v", err)
		}
		child, err := ag.NewSession(t.Context(), kernel.ChildOf(rt))
		if err != nil {
			t.Fatal(err)
		}
		if got := child.ApprovalRules(); len(got) != 1 || got[0].ID != "dup" {
			t.Fatalf("child rules %+v", got)
		}
		close(release)
		h.close()
	})
}

// TestApprovalRequestedRulesCarryScope: a matched rule reports the scope it was registered in, both in the event and in what the reviewer is given, agent scope first.
func TestApprovalRequestedRulesCarryScope(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		seen := make(chan []kernel.MatchedRule, 1)
		h := newHarness(t, [][]providertest.Step{
			toolReply(call{"toolu_1", "echo", `{"x":1}`}), textReply("done"),
		}, kernel.WithTool(&echoTool{name: "echo"}),
			kernel.WithApprovalRules(kernel.ApprovalRule{ID: "agent-ask", Tool: "echo", Action: kernel.RuleAsk, Reason: "agent rule"}),
			kernel.WithApprover(func(_ context.Context, req kernel.ApprovalRequest) (kernel.ApprovalDecision, error) {
				seen <- req.Rules
				return kernel.ApprovalDecision{Verdict: kernel.VerdictApprove}, nil
			}))
		if _, err := h.sess.Registrar().ApprovalRule(kernel.ApprovalRule{ID: "session-ask", Tool: "echo", Action: kernel.RuleAsk, Reason: "session rule"}); err != nil {
			t.Fatal(err)
		}
		ask(t, h.sess, "go")
		var got []event.RuleRef
		for _, ev := range h.rec.all() {
			if ar, ok := ev.Data.(*event.ApprovalRequested); ok {
				got = ar.Rules
			}
		}
		want := []event.RuleRef{
			{ID: "agent-ask", Action: "ask", Reason: "agent rule", Scope: "agent"},
			{ID: "session-ask", Action: "ask", Reason: "session rule", Scope: "session"},
		}
		if !slices.Equal(got, want) {
			t.Fatalf("approval.requested rules: got %+v, want %+v", got, want)
		}
		reviewed := <-seen
		wantRules := []kernel.MatchedRule{
			{ID: "agent-ask", Action: kernel.RuleAsk, Reason: "agent rule", Scope: "agent"},
			{ID: "session-ask", Action: kernel.RuleAsk, Reason: "session rule", Scope: "session"},
		}
		if !slices.Equal(reviewed, wantRules) {
			t.Fatalf("reviewer rules: got %+v, want %+v", reviewed, wantRules)
		}
		h.close()
	})
}
