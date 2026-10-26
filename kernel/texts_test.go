package kernel_test

import (
	"errors"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/estonshi/aperture-potatos/internal/prompt"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/providertest"
)

// TestTextOverrides: WithTexts replaces the system file, the fixed texts the model sees, and the texts an adapter writes.
func TestTextOverrides(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, [][]providertest.Step{
			toolReply(call{id: "tc1", name: "missing", input: `{}`}),
			textReply("done"),
		}, kernel.WithTexts(map[string]string{
			"identity":              "<identity>\ncall me {{nickname}}.\n</identity>",
			"unknown_tool":          "No tool {{name}} here.",
			"dangling_tool_call":    "This call never ran.",
			"tool_result_truncated": "[cut {{omitted}} of {{total}}]",
		}))
		ask(t, h.sess, "hi")

		req := h.sp.Requests()[0]
		if len(req.System) < 2 || req.System[1].Text != "<identity>\ncall me Tater.\n</identity>" {
			t.Fatalf("system: %+v", req.System)
		}
		if got := prompt.TextFrom(req.Texts, "dangling_tool_call"); got != "This call never ran." {
			t.Fatalf("the adapter source: %q", got)
		}
		if prompt.TextFrom(req.Texts, "resume_lost") != prompt.Text("resume_lost") {
			t.Fatal("a key without an override must fall back to the built-in text")
		}

		found := false
		for _, m := range h.lines() {
			for _, b := range m.Content {
				if r, ok := b.(*message.ToolResultBlock); ok && r.Name == "missing" {
					found = true
					if text(r.Content) != "No tool missing here." {
						t.Fatalf("tool result: %q", text(r.Content))
					}
				}
			}
		}
		if !found {
			t.Fatal("no result for the unknown tool")
		}
		h.close()
	})
}

// TestWithTextsRejectsBadKeys: a key that names no text or prompt file fails at New, as does a core override that drops a runtime tag.
func TestWithTextsRejectsBadKeys(t *testing.T) {
	newAgent := func(texts map[string]string) error {
		sp := providertest.NewScriptProvider(provider.APIAnthropicMessages)
		_, err := kernel.New(
			kernel.WithModel(provider.ModelRef{Provider: sp, Model: "m"}),
			kernel.WithTexts(texts),
		)
		return err
	}
	if err := newAgent(map[string]string{"unknown_tool": "x"}); err != nil {
		t.Fatalf("a known key must be accepted: %v", err)
	}
	if err := newAgent(map[string]string{"identity": "x"}); err != nil {
		t.Fatalf("a prompt file name must be accepted: %v", err)
	}
	for _, c := range []struct {
		name  string
		texts map[string]string
		want  string
	}{
		{name: "typo", texts: map[string]string{"unknown_toool": "x"}, want: "no fixed text or prompt file"},
		{name: "core drops tags", texts: map[string]string{"system.core": "<runtime-conventions>hi</runtime-conventions>"}, want: "runtime tag"},
		{name: "core unclosed", texts: map[string]string{"system.core": "<runtime-conventions> <system-reminder> <task-notification> <agent-message> hi"}, want: "closes runtime-conventions"},
	} {
		t.Run(c.name, func(t *testing.T) {
			err := newAgent(c.texts)
			if !errors.Is(err, kernel.ErrInvalidName) || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want ErrInvalidName mentioning %q", err, c.want)
			}
		})
	}
}
