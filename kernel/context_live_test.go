//go:build live

package kernel_test

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/anthropic"
	"github.com/estonshi/aperture-potatos/provider/openaichat"
	"github.com/estonshi/aperture-potatos/store"
)

// TestLiveContextWindow runs the window checks through an OpenAI-compatible gateway, from OPENAI_BASE_URL,
// OPENAI_API_KEY and OPENAI_MODEL.
func TestLiveContextWindow(t *testing.T) {
	model, key := os.Getenv("OPENAI_MODEL"), os.Getenv("OPENAI_API_KEY")
	if model == "" || key == "" {
		t.Skip("set OPENAI_API_KEY and OPENAI_MODEL (and OPENAI_BASE_URL for a gateway)")
	}
	p := openaichat.New(openaichat.Config{APIKey: key, BaseURL: os.Getenv("OPENAI_BASE_URL")})
	liveWindow(t, p, model)
}

// TestLiveContextWindowAnthropic is the same through the Messages API, from ANTHROPIC_BASE_URL, ANTHROPIC_API_KEY
// and ANTHROPIC_MODEL.
func TestLiveContextWindowAnthropic(t *testing.T) {
	model, key := os.Getenv("ANTHROPIC_MODEL"), os.Getenv("ANTHROPIC_API_KEY")
	if model == "" || key == "" {
		t.Skip("set ANTHROPIC_API_KEY and ANTHROPIC_MODEL (and ANTHROPIC_BASE_URL for a gateway)")
	}
	p := anthropic.New(anthropic.Config{APIKey: key, BaseURL: os.Getenv("ANTHROPIC_BASE_URL")})
	liveWindow(t, p, model)
}

// liveWindow: a window named for the session overrides the model's, a real request after it still works, the file
// records it, and a resume restores it.
func liveWindow(t *testing.T, p provider.Provider, model string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	st := store.NewMemory()
	agent := func() *kernel.Agent {
		t.Helper()
		ag, err := kernel.New(
			kernel.WithModel(provider.ModelRef{Provider: p, Model: model, Params: provider.Params{MaxOutputTokens: 256}}),
			kernel.WithCapabilityResolver(nil), // no lookup: the default window stands until the session names one
			kernel.WithStore(st),
			kernel.WithInstructions("Answer in one short sentence."),
		)
		if err != nil {
			t.Fatal(err)
		}
		return ag
	}
	ask := func(s *kernel.Session, text string) string {
		t.Helper()
		r, err := s.Send(kernel.Text(text))
		if err != nil {
			t.Fatal(err)
		}
		res, err := r.Wait(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if res.Err != nil {
			t.Fatalf("turn %q: %v", text, res.Err)
		}
		t.Logf("%s %q -> %q", model, text, res.FinalText)
		return res.FinalText
	}

	first := agent()
	sess, err := first.NewSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	id := sess.Info().SessionID
	if rep := sess.Context(); rep.Window != provider.DefaultContextWindow || rep.Threshold <= 0 {
		t.Fatalf("the default window: %+v", rep)
	}
	if answer := ask(sess, "Say hello in three words."); answer == "" {
		t.Fatal("no answer")
	}
	if err := sess.SetContextWindow(1_000_000); err != nil {
		t.Fatal(err)
	}
	rep := sess.Context()
	if rep.Window != 1_000_000 || rep.Usable != 1_000_000-256 || rep.Threshold != 899_769 {
		t.Fatalf("the named window: %+v", rep) // 0.9 of 1M less the 256-token output cap
	}
	t.Logf("%s window %d usable %d threshold %d", model, rep.Window, rep.Usable, rep.Threshold)
	ask(sess, "Name one fruit.") // a real request on the widened window

	lines, err := st.Load(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	recorded := -1
	for _, m := range lines {
		if m.CopyOf == "" && m.ContextWindow != nil {
			recorded = *m.ContextWindow
		}
	}
	if recorded != 1_000_000 {
		t.Fatalf("the file recorded %d", recorded)
	}
	if err := first.Close(ctx); err != nil {
		t.Fatal(err)
	}

	second := agent()
	resumed, err := second.Resume(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if rep := resumed.Context(); rep.Window != 1_000_000 || rep.Threshold != 899_769 {
		t.Fatalf("the resumed window: %+v", rep)
	}
	ask(resumed, "Name one city.")
	if err := resumed.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := second.Close(ctx); err != nil {
		t.Fatal(err)
	}
}
