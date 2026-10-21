package provider

import (
	"encoding/json"

	"github.com/estonshi/aperture-potatos/message"
)

// ReplayTarget is where history is about to be sent.
type ReplayTarget struct {
	API      API
	Endpoint string // normalized host
	Family   string // resolved Capabilities.Family, or the request model when none is known
}

// Replays reports whether a reasoning, opaque or hosted block produced by p reaches t, by the replay matrix of spec/mappings/projection.md; the kernel counts what a switch drops with the same rule the adapters render with.
func Replays(b message.Block, p *message.Producer, t ReplayTarget) bool {
	switch v := b.(type) {
	case *message.ReasoningBlock:
		if v.Native == nil || API(v.Native.API) != t.API {
			return false
		}
		switch t.API {
		case APIAnthropicMessages:
			return true // the API filters blocks it cannot read and does not bill them
		case APIOpenAIChat:
			return p != nil && API(p.API) == t.API && p.Endpoint == t.Endpoint && family(p) == t.Family
		case APIOpenAIResponses:
			var item struct {
				EncryptedContent string `json:"encrypted_content"`
			}
			_ = json.Unmarshal(v.Native.Raw, &item)
			if family(p) != t.Family {
				return false
			}
			// Raw reasoning_text is plain text only its own server is known to read back, so unlike an encrypted item it also needs the same endpoint.
			return item.EncryptedContent != "" || v.Kind == message.ReasoningRaw && p != nil && p.Endpoint == t.Endpoint
		}
		return false
	case *message.OpaqueBlock:
		return v.Native != nil && API(v.Native.API) == t.API
	case *message.HostedCallBlock:
		return v.Native != nil && API(v.Native.API) == t.API
	case *message.HostedResultBlock:
		return v.Native != nil && API(v.Native.API) == t.API
	}
	return true
}

// family is the producer's family, which defaults to its model name.
func family(p *message.Producer) string {
	if p == nil {
		return ""
	}
	if p.Family != "" {
		return p.Family
	}
	return p.Model
}
