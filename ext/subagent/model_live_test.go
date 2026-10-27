//go:build live

package subagent_test

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/estonshi/aperture-potatos/ext/subagent"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/openaichat"
	"github.com/estonshi/aperture-potatos/store"
)

// TestLiveCallTimeModel starts subagents through a real OpenAI-compatible gateway: one on OPENAI_ALT_MODEL, chosen
// by the call, and one on inherit, and checks the child session ran on each.
func TestLiveCallTimeModel(t *testing.T) {
	model, alt, key := os.Getenv("OPENAI_MODEL"), os.Getenv("OPENAI_ALT_MODEL"), os.Getenv("OPENAI_API_KEY")
	if model == "" || key == "" {
		t.Skip("set OPENAI_API_KEY, OPENAI_MODEL and OPENAI_ALT_MODEL (and OPENAI_BASE_URL for a gateway)")
	}
	if alt == "" {
		t.Skip("set OPENAI_ALT_MODEL to a second model on the same endpoint")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	p := openaichat.New(openaichat.Config{APIKey: key, BaseURL: os.Getenv("OPENAI_BASE_URL")})
	var mu sync.Mutex
	var childModels []string
	watch := kernel.WithHook(kernel.UserPromptSubmit, "child_model", func(_ context.Context, in kernel.UserPromptSubmitInput) (kernel.UserPromptSubmitOutput, error) {
		if len(in.Path) == 0 {
			return kernel.UserPromptSubmitOutput{}, nil
		}
		mu.Lock()
		childModels = append(childModels, in.Session.Model().Model)
		mu.Unlock()
		return kernel.UserPromptSubmitOutput{}, nil
	})
	opts := subagent.Options{
		Models: []string{alt, "inherit"},
		ResolveModel: func(name string) (*provider.ModelRef, error) {
			return &provider.ModelRef{Provider: p, Model: name, Params: provider.Params{MaxOutputTokens: 256}}, nil
		},
	}
	ag, err := kernel.New(
		kernel.WithModel(provider.ModelRef{Provider: p, Model: model, Params: provider.Params{MaxOutputTokens: 256}}),
		kernel.WithCapabilityResolver(nil),
		kernel.WithStore(store.NewMemory()),
		kernel.WithInstructions("Answer in one short sentence."),
		kernel.Use(subagent.New(opts)),
		watch,
	)
	if err != nil {
		t.Fatal(err)
	}
	defer ag.Close(context.Background())
	sess, err := ag.NewSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	start := func(chosen string) {
		t.Helper()
		in, _ := json.Marshal(map[string]string{"agent_type": "general", "description": "probe", "prompt": "Reply with the single word KO.", "model": chosen})
		info, err := sess.Tasks().Start(ctx, kernel.TaskSpec{Kind: subagent.Name, Title: "probe", Input: in})
		if err != nil {
			t.Fatal(err)
		}
		got, err := sess.Tasks().Wait(ctx, info.ID)
		if err != nil || got.Status != kernel.TaskSucceeded {
			t.Fatalf("%s: %+v %v", chosen, got, err)
		}
		t.Logf("%s -> %q", chosen, got.Result.Summary)
	}
	start(alt)
	start("inherit")
	mu.Lock()
	got := slices.Clone(childModels)
	mu.Unlock()
	if !slices.Equal(got, []string{alt, model}) {
		t.Fatalf("children ran on %q, want [%s %s]", got, alt, model)
	}
}
