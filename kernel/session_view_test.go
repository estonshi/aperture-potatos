package kernel_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/providertest"
)

// staticTool reports a fixed spec, so a test can watch the bytes Tools hands out.
type staticTool struct {
	spec    kernel.ToolSpec
	panicky bool
}

func (t *staticTool) Spec() kernel.ToolSpec {
	if t.panicky {
		panic("spec panic")
	}
	return t.spec
}

func (t *staticTool) Call(context.Context, *kernel.ToolContext, json.RawMessage) (kernel.ToolResult, error) {
	return kernel.TextResult("ok"), nil
}

func TestSessionToolsListsVisibleSpecs(t *testing.T) {
	sp := providertest.NewScriptProvider(provider.APIAnthropicMessages)
	ag, err := kernel.New(
		kernel.WithModel(provider.ModelRef{Provider: sp, Model: "m"}),
		kernel.WithTool(&staticTool{spec: kernel.ToolSpec{
			Name: "agent_tool", InputSchema: json.RawMessage(`{"type":"object"}`), Tags: []string{"agent"},
		}}),
		kernel.WithTool(&staticTool{spec: kernel.ToolSpec{Name: "hidden", InputSchema: json.RawMessage(`{"type":"object"}`)}}),
	)
	if err != nil {
		t.Fatal(err)
	}
	local := kernel.ExtensionFunc("local", func(r kernel.Registrar) error {
		broken := &staticTool{spec: kernel.ToolSpec{Name: "broken", InputSchema: json.RawMessage(`{"type":"object"}`)}}
		for _, tool := range []*staticTool{
			{spec: kernel.ToolSpec{Name: "zeta", InputSchema: json.RawMessage(`{"type":"object"}`), Tags: []string{"local"}}},
			{spec: kernel.ToolSpec{Name: "alpha", InputSchema: json.RawMessage(`{"type":"object"}`)}},
			broken,
		} {
			if _, err := r.Tool(tool); err != nil {
				return err
			}
		}
		broken.panicky = true // registration already read the spec
		_, err := r.Hide(kernel.KindTool, "hidden")
		return err
	})
	sess, err := ag.NewSession(t.Context(), kernel.SessionUse(local))
	if err != nil {
		t.Fatal(err)
	}
	got := sess.Tools()
	want := []string{"agent_tool", "alpha", "zeta"}
	if len(got) != len(want) {
		t.Fatalf("tools %v, want %v", names(got), want)
	}
	for i, name := range want {
		if got[i].Name != name {
			t.Fatalf("tools %v, want %v", names(got), want)
		}
	}
	if got[0].Tags[0] != "agent" || string(got[0].InputSchema) != `{"type":"object"}` {
		t.Fatalf("first tool lost its spec: %+v", got[0])
	}

	got[0].Tags[0] = "mutated"
	got[0].InputSchema[0] = 'x'
	again := sess.Tools()
	if again[0].Tags[0] != "agent" || string(again[0].InputSchema) != `{"type":"object"}` {
		t.Fatalf("Tools shares spec bytes with the registry: %+v", again[0])
	}
	if err := ag.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestSessionHostedToolsListsVisible(t *testing.T) {
	sp := providertest.NewScriptProvider(provider.APIAnthropicMessages)
	ag, err := kernel.New(
		kernel.WithModel(provider.ModelRef{Provider: sp, Model: "m"}),
		kernel.WithHostedTool(message.HostedTool{Name: "web_search", WebSearch: &message.WebSearch{}}),
	)
	if err != nil {
		t.Fatal(err)
	}
	local := kernel.ExtensionFunc("local", func(r kernel.Registrar) error {
		if _, err := r.HostedTool(message.HostedTool{Name: "tool_search", ToolSearch: &message.ToolSearch{}}); err != nil {
			return err
		}
		_, err := r.Hide(kernel.KindHostedTool, "web_search")
		return err
	})
	sess, err := ag.NewSession(t.Context(), kernel.SessionUse(local))
	if err != nil {
		t.Fatal(err)
	}
	got := sess.HostedTools()
	if len(got) != 1 || got[0].Name != "tool_search" {
		t.Fatalf("hosted tools %v", hostedNames(got))
	}
	if err := ag.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func names(specs []kernel.ToolSpec) []string {
	out := make([]string, len(specs))
	for i, s := range specs {
		out[i] = s.Name
	}
	return out
}

func hostedNames(tools []message.HostedTool) []string {
	out := make([]string, len(tools))
	for i, t := range tools {
		out[i] = t.Name
	}
	return out
}
