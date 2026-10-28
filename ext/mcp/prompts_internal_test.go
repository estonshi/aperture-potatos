package mcp

import (
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestCommandName(t *testing.T) {
	for in, want := range map[string]string{
		"Review.Code": "mcp__gh__review_code",
		"fix-bug_2":   "mcp__gh__fix-bug_2",
		"日本語":         "mcp__gh_____",
	} {
		if got := commandName("gh", in); got != want {
			t.Errorf("commandName(%q) = %q, want %q", in, got, want)
		}
	}
	long := commandName("gh", strings.Repeat("A", 80))
	if len(long) != 64 || !strings.HasPrefix(long, "mcp__gh__aaaa") || long[57] != '_' {
		t.Fatalf("long name %q", long)
	}
	if commandName("gh", strings.Repeat("A", 80)) == commandName("gh", strings.Repeat("A", 81)) {
		t.Fatal("two long names that differ past the cut share a command name")
	}
	if got := commandName("My_GitHub", "Review"); got != "mcp__my_github__review" {
		t.Fatalf("a server's capitals are lower-cased in the command: %q", got)
	}
}

func TestPromptArguments(t *testing.T) {
	pe := &promptEntry{name: "mcp__t__p", hint: "<a> [b]", args: []*sdk.PromptArgument{{Name: "a", Required: true}, {Name: "b"}}}
	got, err := pe.arguments([]string{"x", "y", "z"})
	if err != nil || got["a"] != "x" || got["b"] != "y z" {
		t.Fatalf("extra words join the last argument: %v %v", got, err)
	}
	if got, err := pe.arguments([]string{"x"}); err != nil || got["b"] != "" {
		t.Fatalf("an optional argument may be left out: %v %v", got, err)
	}
	if _, err := pe.arguments(nil); err == nil || !strings.Contains(err.Error(), "usage: /mcp__t__p <a> [b]") {
		t.Fatalf("missing required: %v", err)
	}
	none := &promptEntry{name: "mcp__t__q"}
	if _, err := none.arguments([]string{"x"}); err == nil {
		t.Fatal("words given to a prompt with no arguments")
	}
	if got, err := none.arguments(nil); err != nil || len(got) != 0 {
		t.Fatalf("no words, no arguments: %v %v", got, err)
	}
}
