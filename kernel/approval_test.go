package kernel_test

import (
	"context"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/providertest"
)

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
