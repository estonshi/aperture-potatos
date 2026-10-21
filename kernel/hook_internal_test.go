package kernel

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/providertest"
)

// TestAsyncAnswerAfterClearIsDropped: an async hook whose context was cleared while it ran never reaches the queue, because what it answered no longer exists.
func TestAsyncAnswerAfterClearIsDropped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := providertest.NewScriptProvider(provider.APIAnthropicMessages, reply("one"))
		var s *Session
		ag, err := New(WithModel(provider.ModelRef{Provider: sp, Model: "m"}), WithCapabilityResolver(nil),
			WithAsyncHook(TurnEnd, "late", func(ctx context.Context, _ TurnEndInput) (Injection, error) {
				time.Sleep(time.Second)
				return Injection{AdditionalContext: "stale", Wake: true}, nil
			}))
		if err != nil {
			t.Fatal(err)
		}
		accepted := 0
		s, err = ag.NewSession(t.Context(), WithSessionFunc(func(_ context.Context, ev event.Event) error {
			if ev.Type == event.TypeInputAccepted {
				accepted++
			}
			return nil
		}))
		if err != nil {
			t.Fatal(err)
		}
		rc, _ := s.Send(Text("hi"))
		_, _ = rc.Wait(t.Context())
		s.clearGen.Add(1) // what Clear does, while the hook is still running
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if accepted != 1 {
			t.Fatalf("%d inputs accepted; the stale answer must be dropped", accepted)
		}
		if err := ag.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}

// TestAsyncHookGetsACopy: an async hook reads the input as the chain started, not the rewrites sync hooks make after it.
func TestAsyncHookGetsACopy(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := providertest.NewScriptProvider(provider.APIAnthropicMessages)
		seen := make(chan string, 1)
		ag, err := New(WithModel(provider.ModelRef{Provider: sp, Model: "m"}), WithCapabilityResolver(nil),
			WithAsyncHook(CommandInvoke, "watch", func(_ context.Context, in CommandInvokeInput) (Injection, error) {
				time.Sleep(time.Second)
				if in.Session == nil {
					seen <- "no session"
				} else {
					seen <- in.Args
				}
				return Injection{}, nil
			}),
			WithHook(CommandInvoke, "rewrite", func(_ context.Context, in CommandInvokeInput) (CommandInvokeOutput, error) {
				a := in.Args + "!"
				return CommandInvokeOutput{UpdatedArgs: &a}, nil
			}),
			WithCommand(Command{Name: "go", Busy: RunNow, Run: func(context.Context, *CommandContext) (CommandResult, error) { return CommandResult{}, nil }}))
		if err != nil {
			t.Fatal(err)
		}
		s, err := ag.NewSession(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		rc, _ := s.Command("go", "x")
		_, _ = rc.Wait(t.Context())
		if got := <-seen; got != "x" {
			t.Fatalf("the async hook saw %q", got)
		}
		if err := ag.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}

func TestParamsPatch(t *testing.T) {
	f := func(v float64) *float64 { return &v }
	n := func(v int) *int { return &v }
	a := &ParamsPatch{MaxOutputTokens: n(100), Temperature: f(0.1), Headers: map[string]string{"a": "1"}, Extra: map[string]any{"x": 1}}
	b := &ParamsPatch{Temperature: f(0.5), TopP: f(0.9), StopSequences: []string{"END"}, ToolChoice: &provider.ToolChoice{Mode: provider.ToolChoiceNone}, Headers: map[string]string{"b": "2"}, Extra: map[string]any{"y": 2}}
	m := a.merge(b)
	if *m.MaxOutputTokens != 100 || *m.Temperature != 0.5 || *m.TopP != 0.9 || m.StopSequences[0] != "END" || m.ToolChoice.Mode != provider.ToolChoiceNone || len(m.Headers) != 2 || len(m.Extra) != 2 {
		t.Fatalf("merge %+v", m)
	}
	if len(a.Headers) != 1 || len(a.Extra) != 1 {
		t.Fatal("merge changed its receiver")
	}
	if (*ParamsPatch)(nil).merge(b) != b || a.merge(nil) != a {
		t.Fatal("merging with nothing")
	}
	p := m.apply(provider.Params{MaxOutputTokens: 64000, Headers: map[string][]string{"C": {"3"}}})
	if p.MaxOutputTokens != 100 || *p.Temperature != 0.5 || *p.TopP != 0.9 || p.Headers.Get("a") != "1" || p.Headers.Get("C") != "3" || p.Extra["y"] != 2 || p.StopSequences[0] != "END" {
		t.Fatalf("apply %+v", p)
	}
	if got := (*ParamsPatch)(nil).apply(provider.Params{MaxOutputTokens: 7}); got.MaxOutputTokens != 7 {
		t.Fatal("a nil patch changed the params")
	}
}

func TestHookErrorAndDecisions(t *testing.T) {
	he := &HookError{Kind: "timeout", Err: context.DeadlineExceeded}
	if he.Error() == "" || he.Unwrap() != context.DeadlineExceeded || failReason("x", he) != "x failed (timeout)" || failReason("x", context.Canceled) != "x failed (error)" {
		t.Fatal("hook error")
	}
	s := "a"
	for want, d := range map[string]interface{ hookDecision() string }{
		"block":     UserPromptSubmitOutput{BlockIDs: []string{"x"}},
		"rewrite":   CommandInvokeOutput{UpdatedArgs: &s},
		"patch":     PreModelRequestOutput{ParamsPatch: &ParamsPatch{}},
		"reject":    PostModelResponseOutput{Reject: true},
		"synthetic": PreToolUseOutput{SyntheticResult: &SyntheticResult{}},
		"stop_turn": PostToolBatchOutput{StopTurn: true},
		"continue":  StopOutput{Continue: true},
		"deny":      ApprovalDecision{Verdict: VerdictDeny},
	} {
		if got := d.hookDecision(); got != want {
			t.Errorf("%T: %q, want %q", d, got, want)
		}
	}
	if (PostToolUseOutput{UpdatedResult: &SyntheticResult{}}).hookDecision() != "rewrite" || (ProviderSwitchOutput{Block: true}).hookDecision() != "block" || (StopOutput{}).hookDecision() != "" {
		t.Fatal("decisions")
	}
	if NewKey[int]("n").Name() != "n" {
		t.Fatal("key name")
	}
}
