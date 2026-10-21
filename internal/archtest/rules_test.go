package archtest

import "testing"

func TestCheckImportTable(t *testing.T) {
	m := func(rel string) string { return module + "/" + rel }
	cases := []struct {
		from, to string
		ok       bool
	}{
		{"message", "encoding/json", true},
		{"message", m("event"), false},
		{"event", m("message"), true},
		{"event", m("store"), false},
		{"store", m("message"), true},
		{"store/jsonl", m("store"), true},
		{"store/jsonl", m("event"), false},
		{"provider", m("message"), true},
		{"provider", m("kernel"), false},
		{"provider/anthropic", m("provider/internal/httpx"), true},
		{"provider/anthropic", m("internal/sse"), true},
		{"provider/anthropic", m("internal/prompt"), true},
		{"provider/internal/project", m("internal/prompt"), false},
		{"provider/anthropic", m("spec"), true},
		{"provider/anthropic", m("provider/openaichat"), false},
		{"provider/anthropic", m("kernel"), false},
		{"provider/anthropic", m("internal/bus"), false},
		{"kernel", m("provider"), true},
		{"kernel", m("internal/bus"), true},
		{"kernel", m("provider/anthropic"), false},
		{"kernel", m("store/jsonl"), false},
		{"kernel", m("ext/tasks"), false},
		{"kernel", m("std"), false},
		{"internal/bus", m("event"), true},
		{"internal/bus", m("kernel"), false},
		{"ext/tasks", m("kernel"), true},
		{"ext/tasks", m("spec"), true},
		{"ext/tasks", m("ext/subagent"), false},
		{"ext/tasks", m("internal/bus"), false},
		{"ext/peer/a2a", m("ext/peer"), true},
		{"ext/peer/a2a", m("internal/sse"), true},
		{"ext/peer", m("ext/peer/a2a"), false},
		{"ext/cmd/btwcmd", m("ext/cmd/usagecmd"), false},
		{"ext/litellm", m("kernel"), true},
		{"std", m("ext/tasks"), true},
		{"std", m("provider/anthropic"), false},
		{"spec", m("message"), false},
		{"conformance", m("provider/anthropic"), true},
		{"conformance/loop", m("ext/subagent"), false},
		{"tools/cmd/conform", m("conformance"), true},
		{"tools/cmd/conform", m("ext/tasks"), false},
		{"examples/full", m("ext/subagent"), true},
		{"kernel", "github.com/other/lib", false},
		{"ext/mcp", "github.com/modelcontextprotocol/go-sdk/mcp", true},
		{"ext/mcp", "golang.org/x/oauth2", true},
		{"ext/mcp", "github.com/modelcontextprotocol/go-sdk-fork/mcp", false},
		{"ext/mcp", "github.com/other/lib", false},
		{"ext/mcp", m("internal/bus"), false},
		{"ext/tasks", "github.com/modelcontextprotocol/go-sdk/mcp", false},
		{"kernel", "golang.org/x/oauth2", false},
		{"nowhere", m("message"), false},
	}
	for _, c := range cases {
		err := checkImport(c.from, c.to)
		if (err == nil) != c.ok {
			t.Errorf("%q -> %q: got %v, want ok=%v", c.from, c.to, err, c.ok)
		}
	}
}
