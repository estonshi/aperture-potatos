package mcp_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/estonshi/aperture-potatos/ext/mcp"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/providertest"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// askServer has a tool that asks the user for a name before answering.
func askServer(t *testing.T) *testServer {
	ts := newTestServer(t, "", nil)
	ts.add("greet", "Greet someone.", func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		res, err := req.Session.Elicit(ctx, &sdk.ElicitParams{Message: "Whom should I greet?",
			RequestedSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`)})
		if err != nil {
			return nil, err
		}
		if res.Action != "accept" {
			return textResult("nobody (" + res.Action + ")"), nil
		}
		return textResult("hello " + res.Content["name"].(string)), nil
	})
	return ts
}

// TestElicitation: the server's question reaches the mcp_elicitation hook of the calling session, and the answer goes back to the server only.
func TestElicitation(t *testing.T) {
	ts := askServer(t)
	var asked mcp.ElicitInput
	sp := providertest.NewScriptProvider(provider.APIAnthropicMessages, calls(call{"e1", "mcp__t__greet", `{"text":""}`}), reply("ok"))
	ag := newAgent(t, sp, use(t, mcp.Options{Servers: []mcp.Server{{Name: "t", URL: ts.URL()}}}),
		kernel.WithHook(mcp.Elicitation, "user", func(_ context.Context, in mcp.ElicitInput) (mcp.ElicitOutput, error) {
			asked = in
			return mcp.ElicitOutput{Action: "accept", Content: map[string]any{"name": "Ann"}}, nil
		}))
	s, err := ag.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	rec := record(t, s)
	ask(t, s, "greet")
	if r := toolResults(s)["e1"]; r.IsError || resultText(r) != "hello Ann" {
		t.Fatalf("result %+v %q", r, resultText(r))
	}
	if asked.Server != "t" || asked.Tool != "mcp__t__greet" || asked.Mode != "form" || asked.Message != "Whom should I greet?" || len(asked.Schema) == 0 {
		t.Fatalf("hook input %+v", asked)
	}
	evs := rec.of("mcp.elicitation")
	if len(evs) != 1 || evs[0].Data.(*mcp.ElicitationEvent).Action != "accept" {
		t.Fatalf("events %+v", evs)
	}
}

// TestElicitationWithoutHook: with nobody to ask, the request is declined.
func TestElicitationWithoutHook(t *testing.T) {
	ts := askServer(t)
	sp := providertest.NewScriptProvider(provider.APIAnthropicMessages, calls(call{"e1", "mcp__t__greet", `{"text":""}`}), reply("ok"))
	ag := newAgent(t, sp, use(t, mcp.Options{Servers: []mcp.Server{{Name: "t", URL: ts.URL()}}}))
	s, err := ag.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ask(t, s, "greet")
	if r := toolResults(s)["e1"]; resultText(r) != "nobody (decline)" {
		t.Fatalf("result %+v %q", r, resultText(r))
	}
}
