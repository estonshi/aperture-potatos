package compact_test

import (
	"testing"

	"github.com/estonshi/aperture-potatos/ext/compact"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/providertest"
)

// TestMalformedSummaryPromptFailsInstall: a caller's template is checked once, when the agent is built, rather than at every compaction.
func TestMalformedSummaryPromptFailsInstall(t *testing.T) {
	sp := providertest.NewScriptProvider(provider.APIAnthropicMessages)
	if _, err := kernel.New(kernel.WithModel(provider.ModelRef{Provider: sp, Model: "m"}), kernel.Use(compact.New(compact.Options{SummaryPrompt: "Summarize in {{target tokens."}))); err == nil {
		t.Fatal("a malformed summary prompt was accepted")
	}
}
