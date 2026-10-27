//go:build live

package kernel_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/anthropic"
	"github.com/estonshi/aperture-potatos/provider/openaichat"
	"github.com/estonshi/aperture-potatos/store"
)

// TestLiveRewindInput cuts a real conversation at an input id through an OpenAI-compatible gateway, forks at the same
// input, and cuts again after a resume, so the lookup runs on lines a real model produced.
func TestLiveRewindInput(t *testing.T) {
	model, key := os.Getenv("OPENAI_MODEL"), os.Getenv("OPENAI_API_KEY")
	if model == "" || key == "" {
		t.Skip("set OPENAI_API_KEY and OPENAI_MODEL (and OPENAI_BASE_URL for a gateway)")
	}
	p := openaichat.New(openaichat.Config{APIKey: key, BaseURL: os.Getenv("OPENAI_BASE_URL")})
	liveRewindInput(t, p, model)
}

// TestLiveRewindInputAnthropic is the same through the Messages API, from ANTHROPIC_BASE_URL, ANTHROPIC_API_KEY and
// ANTHROPIC_MODEL.
func TestLiveRewindInputAnthropic(t *testing.T) {
	model, key := os.Getenv("ANTHROPIC_MODEL"), os.Getenv("ANTHROPIC_API_KEY")
	if model == "" || key == "" {
		t.Skip("set ANTHROPIC_API_KEY and ANTHROPIC_MODEL (and ANTHROPIC_BASE_URL for a gateway)")
	}
	p := anthropic.New(anthropic.Config{APIKey: key, BaseURL: os.Getenv("ANTHROPIC_BASE_URL")})
	liveRewindInput(t, p, model)
}

func liveRewindInput(t *testing.T, p provider.Provider, model string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	st := store.NewMemory()
	agent := func() *kernel.Agent {
		t.Helper()
		ag, err := kernel.New(
			kernel.WithModel(provider.ModelRef{Provider: p, Model: model, Params: provider.Params{MaxOutputTokens: 256}}),
			kernel.WithCapabilityResolver(nil),
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
	ask(sess, "Say hello in three words.")
	ask(sess, "Name one fruit.")
	turns := sess.Turns()
	if len(turns) != 2 || turns[0].InputID == "" || turns[1].InputID == "" {
		t.Fatalf("turns: %+v", turns)
	}
	firstID, secondID := turns[0].InputID, turns[1].InputID

	fork, err := sess.Fork(ctx, kernel.ForkAt(secondID))
	if err != nil {
		t.Fatal(err)
	}
	if got := roles(fork.Messages()); got != "system,user,assistant" {
		t.Fatalf("the fork copied beyond the input: %s", got)
	}
	if strings.Contains(strings.ToLower(fork.Messages()[1].Text()), "fruit") {
		t.Fatalf("the fork saw the dropped turn: %q", fork.Messages()[1].Text())
	}
	ask(fork, "Name one city.")
	if err := fork.Close(ctx); err != nil {
		t.Fatal(err)
	}

	rep, err := sess.Rewind(ctx, secondID)
	if err != nil {
		t.Fatal(err)
	}
	if rep.InputID != secondID || rep.TurnID != turns[1].ID || rep.Epoch != 2 || rep.Dropped != 2 {
		t.Fatalf("report: %+v", rep)
	}
	if got := roles(sess.Messages()); got != "system,user,assistant" {
		t.Fatalf("after the rewind: %s", got)
	}
	ask(sess, "Name one vegetable.") // a real request on the truncated history

	id := sess.Info().SessionID
	if err := first.Close(ctx); err != nil {
		t.Fatal(err)
	}
	second := agent()
	resumed, err := second.Resume(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if turns := resumed.Turns(); len(turns) == 0 || turns[0].InputID != firstID {
		t.Fatalf("the resumed turns: %+v", turns)
	}
	rep, err = resumed.Rewind(ctx, firstID)
	if err != nil {
		t.Fatal(err)
	}
	got := roles(resumed.Messages())
	if rep.InputID != firstID || got != "system" {
		t.Fatalf("the resumed cut: %+v, lines %s", rep, got)
	}
	ask(resumed, "Say goodbye in two words.")
	if err := resumed.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err := second.Close(ctx); err != nil {
		t.Fatal(err)
	}
}
