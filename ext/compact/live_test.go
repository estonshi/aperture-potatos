//go:build live

package compact_test

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/estonshi/aperture-potatos/ext/compact"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/openaichat"
	"github.com/estonshi/aperture-potatos/store"
)

// TestLiveCompaction summarizes a real conversation through OPENAI_BASE_URL, OPENAI_API_KEY and OPENAI_MODEL, then checks the model still knows what the summary had to keep.
func TestLiveCompaction(t *testing.T) {
	model := os.Getenv("OPENAI_MODEL")
	if model == "" || os.Getenv("OPENAI_API_KEY") == "" {
		t.Skip("set OPENAI_API_KEY and OPENAI_MODEL (and OPENAI_BASE_URL for a gateway)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	p := openaichat.New(openaichat.Config{APIKey: os.Getenv("OPENAI_API_KEY"), BaseURL: os.Getenv("OPENAI_BASE_URL")})
	st := store.NewMemory()
	ag, err := kernel.New(
		kernel.WithModel(provider.ModelRef{Provider: p, Model: model, Params: provider.Params{MaxOutputTokens: 1024}}),
		kernel.WithCapabilityResolver(nil),
		kernel.WithStore(st),
		kernel.WithInstructions("Answer in one short sentence."),
		kernel.WithContextPolicy(kernel.ContextPolicy{MaxContextTokens: 8000, MinSummaryTokens: 1500}), // a 1500-token target, above what a three-turn conversation needs
		kernel.Use(compact.New(compact.Options{})),
	)
	if err != nil {
		t.Fatal(err)
	}
	defer ag.Close(context.Background())
	sess, err := ag.NewSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	nonce := time.Now().Format(time.RFC3339Nano)
	ask := func(text string) kernel.TurnResult {
		t.Helper()
		r, err := sess.Send(kernel.Input{Text: text})
		if err != nil {
			t.Fatal(err)
		}
		res, err := r.Wait(ctx)
		if err != nil || res.Err != nil {
			t.Fatalf("turn: %v %v", err, res.Err)
		}
		t.Logf("%q -> %q", text, res.FinalText)
		return res
	}
	ask("Remember this: the secret word is PERSIMMON and the meeting is in room 42. (" + nonce + ")")
	ask("Here are some notes on Japanese fruit, only for context, reply OK: " + strings.Repeat("Persimmons, loquats, yuzu and nashi pears are grown across Japan, each in its own season and region. ", 60))
	ask("Name one fruit that grows in Japan.")
	ask("Name one city in Japan.")
	rep, err := sess.Compact(ctx, "")
	if err != nil {
		t.Fatalf("compact: %v", err)
	}
	t.Logf("compacted %+v", rep)
	lines, _ := st.Load(ctx, sess.Info().SessionID)
	cur := store.Current(lines)
	var summary string
	for _, b := range cur[1].Content {
		if r, ok := b.(*message.ReminderBlock); ok && r.Source == "compaction" {
			summary = r.Text
		}
	}
	t.Logf("summary: %s", summary)
	if cur[0].Boundary.Reason != "compaction" || summary == "" {
		t.Fatalf("no summary in the new epoch head: %+v", cur[1])
	}
	res := ask("What was the secret word, and which room is the meeting in?")
	if !strings.Contains(strings.ToUpper(res.FinalText), "PERSIMMON") || !strings.Contains(res.FinalText, "42") {
		t.Fatalf("the summary lost what the next turn needs: %q", res.FinalText)
	}
	if b := sess.Snapshot().Usage.Buckets["compaction"]; b.InputTokens == 0 {
		t.Fatalf("the summary call is booked in the compaction bucket: %+v", sess.Snapshot().Usage.Buckets)
	}
}
