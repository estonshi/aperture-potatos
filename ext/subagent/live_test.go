//go:build live

package subagent_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/ext/history"
	"github.com/estonshi/aperture-potatos/ext/subagent"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/openaichat"
	"github.com/estonshi/aperture-potatos/store"
)

// TestLiveSubagents runs through OPENAI_BASE_URL, OPENAI_API_KEY and OPENAI_MODEL a foreground subagent, a background one that reports back by notification, and one that finds a fact in the parent conversation with search_history.
func TestLiveSubagents(t *testing.T) {
	model := os.Getenv("OPENAI_MODEL")
	if model == "" || os.Getenv("OPENAI_API_KEY") == "" {
		t.Skip("set OPENAI_API_KEY and OPENAI_MODEL (and OPENAI_BASE_URL for a gateway)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	p := openaichat.New(openaichat.Config{APIKey: os.Getenv("OPENAI_API_KEY"), BaseURL: os.Getenv("OPENAI_BASE_URL")})
	st := store.NewMemory()
	ag, err := kernel.New(
		kernel.WithStore(st),
		kernel.WithModel(provider.ModelRef{Provider: p, Model: model, Params: provider.Params{MaxOutputTokens: 1024}}),
		kernel.WithCapabilityResolver(nil),
		kernel.WithInstructions("Be brief."),
		kernel.Use(subagent.New(subagent.Options{}), history.New()),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer ag.Close(context.Background())
	main, err := ag.NewSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var childCalls []string         // tool calls made in child sessions, as name plus input
	foreground := map[string]bool{} // by task id, as its first attempt started; a finished task's info no longer says
	if _, err := main.SubscribeFunc(func(_ context.Context, ev event.Event) error {
		mu.Lock()
		defer mu.Unlock()
		switch d := ev.Data.(type) {
		case *event.ToolStarted:
			if len(ev.Path) > 0 {
				childCalls = append(childCalls, d.Name+" "+string(d.Input))
			}
		case *event.TaskStarted:
			if len(ev.Path) == 0 && d.Attempt == 1 {
				foreground[d.TaskID] = d.Foreground
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	ask := func(text string) string {
		t.Helper()
		r, err := main.Send(kernel.Input{Text: text})
		if err != nil {
			t.Fatal(err)
		}
		res, err := r.Wait(ctx)
		if err != nil || res.Err != nil {
			t.Fatalf("turn: %v %v", err, res.Err)
		}
		t.Logf("%q -> %q", text, res.FinalText)
		return res.FinalText
	}
	subagents := func() []kernel.TaskInfo {
		var out []kernel.TaskInfo
		for _, info := range main.Tasks().List() {
			if info.Kind == subagent.Name {
				out = append(out, info)
			}
		}
		return out
	}

	ask("Remember this: the secret word is PERSIMMON. Reply OK.")

	got := ask("Use the subagent tool, in the foreground, to have a helper compute 17 * 23. Then tell me the number.")
	fg := subagents()
	isFg := func(id string) bool { mu.Lock(); defer mu.Unlock(); return foreground[id] }
	if len(fg) != 1 || fg[0].Status != kernel.TaskSucceeded || !isFg(fg[0].ID) || !strings.Contains(got, "391") {
		t.Fatalf("foreground subagent %+v, answer %q", fg, got)
	}

	ask("Use the subagent tool with background set to true to have a helper write a two-line poem about autumn. Do not wait for it; reply 'started'.")
	if _, err := main.WaitQuiet(ctx); err != nil {
		t.Fatal(err)
	}
	all := subagents()
	if len(all) != 2 || isFg(all[1].ID) || all[1].Status != kernel.TaskSucceeded {
		t.Fatalf("background subagent %+v", all)
	}
	lines, err := st.Load(ctx, main.Info().SessionID)
	if err != nil {
		t.Fatal(err)
	}
	var notified bool
	for _, m := range lines {
		notified = notified || strings.Contains(m.Text(), `<task-notification task_id="`+all[1].ID+`"`)
	}
	if !notified {
		t.Fatal("the background subagent's notification never reached the conversation")
	}

	got = ask("Use the subagent tool in the foreground and tell the helper exactly this: \"Find the secret word mentioned earlier in the parent conversation by calling search_history with parent set to true, and reply with just the word.\" Then tell me the word it found.")
	mu.Lock()
	var searched bool
	for _, c := range childCalls {
		name, input, _ := strings.Cut(c, " ")
		var in struct {
			Parent bool `json:"parent"`
		}
		searched = searched || name == "search_history" && json.Unmarshal([]byte(input), &in) == nil && in.Parent
	}
	t.Logf("child calls: %v", childCalls)
	mu.Unlock()
	if !searched || !strings.Contains(strings.ToUpper(got), "PERSIMMON") {
		t.Fatalf("searched the parent %v, answer %q", searched, got)
	}
	if b := main.Snapshot().Usage.Buckets[subagent.Name]; b.InputTokens == 0 {
		t.Fatalf("the children's calls count in the parent's subagent bucket: %+v", main.Snapshot().Usage.Buckets)
	}
}
