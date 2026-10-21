//go:build live

package memory_test

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/ext/memory"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/openaichat"
	"github.com/estonshi/aperture-potatos/store"
)

// TestLiveMemory runs through OPENAI_BASE_URL, OPENAI_API_KEY and OPENAI_MODEL a session in which the user states a standing preference, which the model saves or the consolidation at close keeps, then a new session of the same user and project that follows it from its epoch head.
func TestLiveMemory(t *testing.T) {
	model := os.Getenv("OPENAI_MODEL")
	if model == "" || os.Getenv("OPENAI_API_KEY") == "" {
		t.Skip("set OPENAI_API_KEY and OPENAI_MODEL (and OPENAI_BASE_URL for a gateway)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	p := openaichat.New(openaichat.Config{APIKey: os.Getenv("OPENAI_API_KEY"), BaseURL: os.Getenv("OPENAI_BASE_URL")})
	st := memory.NewInMemory()
	ag, err := kernel.New(
		kernel.WithStore(store.NewMemory()),
		kernel.WithModel(provider.ModelRef{Provider: p, Model: model, Params: provider.Params{MaxOutputTokens: 1024}}),
		kernel.WithCapabilityResolver(nil),
		kernel.WithInstructions("Be brief."),
		kernel.WithLimits(kernel.Limits{CloseTimeout: 60 * time.Second}), // the consolidation at close is a model call
		kernel.Use(memory.New(memory.Options{Store: st, ConsolidateOn: []string{"session_end", "pre_compact"}})),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer ag.Close(context.Background())
	var mu sync.Mutex
	var triggers []string
	open := func() *kernel.Session {
		t.Helper()
		s, err := ag.NewSession(ctx, kernel.WithLabels(map[string]string{"user_id": "u1", "project_id": "p1"}), kernel.WithSessionFunc(func(_ context.Context, ev event.Event) error {
			if d, ok := ev.Data.(*memory.Saved); ok {
				mu.Lock()
				triggers = append(triggers, d.Trigger)
				mu.Unlock()
			}
			return nil
		}))
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	ask := func(s *kernel.Session, text string) string {
		t.Helper()
		rc, err := s.Send(kernel.Text(text))
		if err != nil {
			t.Fatal(err)
		}
		res, err := rc.Wait(ctx)
		if err != nil || res.Err != nil {
			t.Fatalf("turn: %v %v", err, res.Err)
		}
		t.Logf("%q -> %q", text, res.FinalText)
		return res.FinalText
	}

	first := open()
	ask(first, "From now on, in this project, end every answer with the word OVER. Please remember this for our future conversations.")
	if err := first.Close(ctx); err != nil {
		t.Fatal(err)
	}
	items, _ := st.List(ctx, memory.Scope{User: "u1", Project: "p1"})
	for _, it := range items {
		t.Logf("memory %s user=%s project=%s: %s", it.ID, it.User, it.Project, it.Content)
	}
	mu.Lock()
	t.Logf("saved by %v", triggers)
	mu.Unlock()
	if len(items) == 0 || len(items) > 2 {
		t.Fatalf("%d memories after the first session", len(items))
	}

	second := open()
	got := ask(second, "What is 2 + 3?")
	if !strings.HasSuffix(strings.TrimRight(got, ".!\n "), "OVER") {
		t.Fatalf("the new session did not follow its memory: %q", got)
	}
}
