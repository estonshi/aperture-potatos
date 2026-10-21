//go:build live

package skills_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/ext/skills"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/openaichat"
	"github.com/estonshi/aperture-potatos/store"
)

// TestLiveSkills runs through OPENAI_BASE_URL, OPENAI_API_KEY and OPENAI_MODEL a model that picks a skill from the catalog, loads it and reads one of its resource files, and a person's /release-notes command.
func TestLiveSkills(t *testing.T) {
	model := os.Getenv("OPENAI_MODEL")
	if model == "" || os.Getenv("OPENAI_API_KEY") == "" {
		t.Skip("set OPENAI_API_KEY and OPENAI_MODEL (and OPENAI_BASE_URL for a gateway)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	p := openaichat.New(openaichat.Config{APIKey: os.Getenv("OPENAI_API_KEY"), BaseURL: os.Getenv("OPENAI_BASE_URL")})
	ag, err := kernel.New(
		kernel.WithStore(store.NewMemory()),
		kernel.WithModel(provider.ModelRef{Provider: p, Model: model, Params: provider.Params{MaxOutputTokens: 1024}}),
		kernel.WithCapabilityResolver(nil),
		kernel.WithInstructions("Be brief."),
		kernel.Use(skills.New(example())),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer ag.Close(context.Background())
	sess, err := ag.NewSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var calls []string // skill tool inputs
	var loaded []*skills.Activated
	if _, err := sess.SubscribeFunc(func(_ context.Context, ev event.Event) error {
		mu.Lock()
		defer mu.Unlock()
		switch d := ev.Data.(type) {
		case *event.ToolStarted:
			if d.Name == "skill" {
				calls = append(calls, string(d.Input))
			}
		case *skills.Activated:
			loaded = append(loaded, d)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	wait := func(rc *kernel.Receipt, err error) string {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		res, err := rc.Wait(ctx)
		if err != nil || res.Err != nil {
			t.Fatalf("turn: %v %v", err, res.Err)
		}
		return res.FinalText
	}

	got := wait(sess.Send(kernel.Text("I have a fillable PDF form. Which field types can its fields have? Answer from the reference file of the matching skill.")))
	t.Logf("answer %q, skill calls %v", got, calls)
	mu.Lock()
	var named, read bool
	for _, c := range calls {
		var in struct{ Name, Resource string }
		_ = json.Unmarshal([]byte(c), &in)
		named = named || in.Name == "pdf-processing" && in.Resource == ""
		read = read || in.Name == "pdf-processing" && in.Resource == "references/REFERENCE.md"
	}
	mu.Unlock()
	if !named || !read || !strings.Contains(strings.ToLower(got), "checkbox") {
		t.Fatalf("loaded the skill %v, read its reference %v, answer %q", named, read, got)
	}
	if pins := sess.Pins(); len(pins) != 1 || pins[0].Key != "skill:pdf-processing" {
		t.Fatalf("pins %+v", pins)
	}

	if _, err := sess.Command("release-notes", "3.0"); err != nil {
		t.Fatal(err)
	}
	res, err := sess.WaitQuiet(ctx)
	if err != nil || res.Err != nil {
		t.Fatalf("command turn: %v %v", err, res.Err)
	}
	t.Logf("release notes %q", res.FinalText)
	mu.Lock()
	defer mu.Unlock()
	if n := len(loaded); n < 2 || loaded[n-1].Name != "release-notes" || loaded[n-1].Trigger != "user" || !strings.Contains(res.FinalText, "3.0") {
		t.Fatalf("activations %+v, answer %q", loaded, res.FinalText)
	}
}
