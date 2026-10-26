package kernel_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/providertest"
)

// TestNamedApproversRunInPriorityOrder: named reviewers form one chain, ordered by Priority, and one scope refuses the same name twice.
func TestNamedApproversRunInPriorityOrder(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var order []string
		sp := providertest.NewScriptProvider(provider.APIAnthropicMessages, toolReply(call{"toolu_1", "echo", `{"x":1}`}), textReply("done"))
		ag, err := kernel.New(
			kernel.WithModel(provider.ModelRef{Provider: sp, Model: "m"}),
			kernel.WithTool(&echoTool{name: "echo"}),
			kernel.WithApprovalRules(kernel.ApprovalRule{ID: "review-echo", Tool: "echo", Action: kernel.RuleAsk}),
			kernel.WithApproverNamed("policy", func(context.Context, kernel.ApprovalRequest) (kernel.ApprovalDecision, error) {
				order = append(order, "policy")
				return kernel.ApprovalDecision{Verdict: kernel.VerdictApprove}, nil
			}, kernel.Priority(-1)),
			kernel.WithApproverNamed("human", func(context.Context, kernel.ApprovalRequest) (kernel.ApprovalDecision, error) {
				order = append(order, "human")
				return kernel.ApprovalDecision{Verdict: kernel.VerdictApprove}, nil
			}),
		)
		if err != nil {
			t.Fatal(err)
		}
		sess, err := ag.NewSession(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		rc, err := sess.Send(kernel.Text("go"))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := rc.Wait(t.Context()); err != nil {
			t.Fatal(err)
		}
		if want := []string{"policy", "human"}; !slices.Equal(order, want) {
			t.Fatalf("order %v, want %v", order, want)
		}
		if err := ag.Close(context.Background()); err != nil {
			t.Fatal(err)
		}

		_, err = kernel.New(kernel.WithModel(provider.ModelRef{Provider: sp, Model: "m"}),
			kernel.WithApproverNamed("same", func(context.Context, kernel.ApprovalRequest) (kernel.ApprovalDecision, error) {
				return kernel.ApprovalDecision{Verdict: kernel.VerdictApprove}, nil
			}),
			kernel.WithApproverNamed("same", func(context.Context, kernel.ApprovalRequest) (kernel.ApprovalDecision, error) {
				return kernel.ApprovalDecision{Verdict: kernel.VerdictApprove}, nil
			}))
		if !errors.Is(err, kernel.ErrDuplicate) {
			t.Fatalf("same name twice: %v", err)
		}
	})
}

// TestSessionRulesAndMatchFunc: a session's own rules add to the agent's, and a Go matcher picks the tools its hook sees.
func TestSessionRulesAndMatchFunc(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := providertest.NewScriptProvider(provider.APIAnthropicMessages, toolReply(call{"toolu_1", "echo", `{"x":1}`}, call{"toolu_2", "other", `{}`}), textReply("done"))
		var seen []string
		ag, err := kernel.New(kernel.WithModel(provider.ModelRef{Provider: sp, Model: "m"}), kernel.WithCapabilityResolver(nil),
			kernel.WithTool(&echoTool{name: "echo"}), kernel.WithTool(&echoTool{name: "other"}),
			kernel.WithHook(kernel.PreToolUse, "watch", func(_ context.Context, in kernel.PreToolUseInput) (kernel.PreToolUseOutput, error) {
				seen = append(seen, in.Call.Name)
				return kernel.PreToolUseOutput{}, nil
			}, kernel.Match(kernel.MatchFunc(func(k string) bool { return k == "other" }))))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ag.NewSession(t.Context(), kernel.SessionApprovalRules(kernel.ApprovalRule{ID: "", Tool: "x", Action: kernel.RuleDeny})); err == nil {
			t.Fatal("a session accepted a rule without an id")
		}
		rec := &recorder{}
		sess, err := ag.NewSession(t.Context(), kernel.WithSessionFunc(rec.handle), kernel.SessionApprovalRules(kernel.ApprovalRule{ID: "no-echo", Tool: "echo", Action: kernel.RuleDeny, Reason: "not here"}))
		if err != nil {
			t.Fatal(err)
		}
		rc, _ := sess.Send(kernel.Text("go"))
		_, _ = rc.Wait(t.Context())
		synctest.Wait()
		var lines []message.Message
		for _, ev := range rec.all() {
			if d, ok := ev.Data.(*event.MessageCompleted); ok {
				lines = append(lines, d.Message)
			}
		}
		if r := result(lines, "toolu_1"); r.Outcome != message.OutcomeDenied || !strings.Contains(textOf(r.Content), "Denied by rule no-echo: not here") {
			t.Fatalf("echo %+v", r)
		}
		if r := result(lines, "toolu_2"); r.Outcome != message.OutcomeSuccess || len(seen) != 1 || seen[0] != "other" {
			t.Fatalf("other %+v, hook saw %v", r, seen)
		}
		if err := ag.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
}
