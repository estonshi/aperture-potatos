package mcp

import (
	"testing"

	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/providertest"
)

// TestAgentCloseEndsInstall: closing the agent closes its agent-scope install and forgets it, though the host never called Extension.Close.
func TestAgentCloseEndsInstall(t *testing.T) {
	sp := providertest.NewScriptProvider(provider.APIAnthropicMessages)
	ag, err := kernel.New(kernel.WithModel(provider.ModelRef{Provider: sp, Model: "m"}), kernel.Use(New(Options{Servers: []Server{{Name: "down", URL: "http://127.0.0.1:1/mcp"}}})))
	if err != nil {
		t.Fatal(err)
	}
	v, ok := agentInstalls.Load(ag)
	if !ok {
		t.Fatal("no agent install")
	}
	if err := ag.Close(t.Context()); err != nil {
		t.Fatal(err)
	}
	in := v.(*install)
	in.mu.Lock()
	closed := in.closed
	in.mu.Unlock()
	if _, still := agentInstalls.Load(ag); still || !closed {
		t.Fatalf("after Agent.Close: still registered %v, closed %v", still, closed)
	}
	for _, c := range in.conns {
		<-c.stopped // the reconnect loop has ended
	}
}
