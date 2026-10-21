package provider

import (
	"encoding/json"
	"testing"

	"github.com/estonshi/aperture-potatos/message"
)

func TestReplaysResponsesReasoning(t *testing.T) {
	item := func(kind message.ReasoningKind, raw string) *message.ReasoningBlock {
		return &message.ReasoningBlock{Kind: kind, Native: &message.Native{API: string(APIOpenAIResponses), Raw: json.RawMessage(raw)}}
	}
	encrypted := item(message.ReasoningSummary, `{"type":"reasoning","summary":[],"encrypted_content":"gAAAA"}`)
	raw := item(message.ReasoningRaw, `{"type":"reasoning","summary":[],"content":[{"type":"reasoning_text","text":"t"}]}`)
	summaryOnly := item(message.ReasoningSummary, `{"type":"reasoning","summary":[{"type":"summary_text","text":"s"}]}`)
	here := &message.Producer{API: string(APIOpenAIResponses), Endpoint: "gw.example", Model: "m"}
	elsewhere := &message.Producer{API: string(APIOpenAIResponses), Endpoint: "other.example", Model: "m"}
	otherFamily := &message.Producer{API: string(APIOpenAIResponses), Endpoint: "gw.example", Model: "n"}
	target := ReplayTarget{API: APIOpenAIResponses, Endpoint: "gw.example", Family: "m"}
	for _, c := range []struct {
		name  string
		block *message.ReasoningBlock
		p     *message.Producer
		want  bool
	}{
		{"encrypted from another endpoint", encrypted, elsewhere, true},
		{"encrypted from another family", encrypted, otherFamily, false},
		{"raw from the same endpoint", raw, here, true},
		{"raw from another endpoint", raw, elsewhere, false},
		{"raw from another family", raw, otherFamily, false},
		{"summary without encrypted content", summaryOnly, here, false},
	} {
		if got := Replays(c.block, c.p, target); got != c.want {
			t.Errorf("%s: %v", c.name, got)
		}
	}
}
