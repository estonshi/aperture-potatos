//go:build live

package mcp_test

import (
	"context"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/ext/mcp"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/openaichat"
	"github.com/estonshi/aperture-potatos/store"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// TestLiveServerNameCase runs through OPENAI_BASE_URL, OPENAI_API_KEY and OPENAI_MODEL a server the host names as Claude Code names one — capitals and underscores — so a real model calls mcp__<server>__<tool> with the case kept, and a person runs the prompt it offers as the lower-cased command.
func TestLiveServerNameCase(t *testing.T) {
	model := os.Getenv("OPENAI_MODEL")
	if model == "" || os.Getenv("OPENAI_API_KEY") == "" {
		t.Skip("set OPENAI_API_KEY and OPENAI_MODEL (and OPENAI_BASE_URL for a gateway)")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	ts := promptServer(t)
	ts.add("echo", "Repeat the text it is given.", func(_ context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		return textResult(argText(req)), nil
	})
	p := openaichat.New(openaichat.Config{APIKey: os.Getenv("OPENAI_API_KEY"), BaseURL: os.Getenv("OPENAI_BASE_URL")})
	ag, err := kernel.New(
		kernel.WithStore(store.NewMemory()),
		kernel.WithModel(provider.ModelRef{Provider: p, Model: model, Params: provider.Params{MaxOutputTokens: 512}}),
		kernel.WithCapabilityResolver(nil),
		kernel.WithInstructions("Do what the user asks with the tools you are given. Do not ask for confirmation."),
		use(t, mcp.Options{Servers: []mcp.Server{{Name: "claude_ai_Gmail", URL: ts.URL(), Prompts: true}}}),
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
	var called []string
	if _, err := sess.SubscribeFunc(func(_ context.Context, ev event.Event) error {
		mu.Lock()
		defer mu.Unlock()
		if d, ok := ev.Data.(*event.ToolStarted); ok {
			called = append(called, d.Name)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// The prompt commands carry the server's name in lower case.
	if got := commandNames(sess); !slices.Equal(got, []string{"mcp__claude_ai_gmail__few_shot", "mcp__claude_ai_gmail__review_code"}) {
		t.Fatalf("commands %v", got)
	}

	// The model sees the tool under the name the host's own case produces, and calls it.
	rc, err := sess.Send(kernel.Text(`Call the tool named mcp__claude_ai_Gmail__echo exactly once with {"text": "hello"}, then answer with the single word it returned.`))
	if err != nil {
		t.Fatal(err)
	}
	res, err := rc.Wait(ctx)
	if err != nil || res.Err != nil {
		t.Fatalf("turn: %v %v", err, res.Err)
	}
	t.Logf("answer %q", res.FinalText)
	mu.Lock()
	got := slices.Clone(called)
	mu.Unlock()
	if !slices.Contains(got, "mcp__claude_ai_Gmail__echo") {
		t.Fatalf("the model called %v, not mcp__claude_ai_Gmail__echo", got)
	}
	echoed := false
	for _, r := range toolResults(sess) {
		echoed = echoed || strings.Contains(resultText(r), "hello")
	}
	if !echoed {
		t.Fatal("no tool result carried the text back")
	}

	// A person runs the command the prompt became, and its input reaches the model.
	if _, err := sess.Command("mcp__claude_ai_gmail__review_code", `"x = 1" go`); err != nil {
		t.Fatal(err)
	}
	cres, err := sess.WaitQuiet(ctx)
	if err != nil || cres.Err != nil {
		t.Fatalf("command turn: %v %v", err, cres.Err)
	}
	t.Logf("review %q", cres.FinalText)
	if !slices.ContainsFunc(sess.Messages(), func(m message.Message) bool {
		return m.Role == message.RoleUser && strings.Contains(m.Text(), "Review this go code: x = 1")
	}) {
		t.Fatalf("the prompt's input never reached the model")
	}
}
