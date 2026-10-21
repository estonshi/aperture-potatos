//go:build live

package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
)

// try sends req and returns the reply or the classified error, where run fails the test on any error.
func try(t *testing.T, p *Provider, req *provider.Request) (*message.Message, *provider.Error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	for ev, err := range p.Stream(ctx, req) {
		if err != nil {
			var pe *provider.Error
			if !errors.As(err, &pe) {
				t.Fatalf("stream: %v", err)
			}
			return nil, pe
		}
		if ev.Kind == provider.SEEnd {
			return ev.Message, nil
		}
	}
	t.Fatal("stream ended without an end event")
	return nil, nil
}

var clock = message.ToolDef{
	Name:        "get_time",
	Description: "Get the current time in a city.",
	InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}},"required":["city"],"additionalProperties":false}`),
}

// signed returns a tool loop ending in the tool result, whose signed thinking is always verified, and the whole turn followed by a new question.
func signed(t *testing.T, p *Provider, req *provider.Request) (loop, turns []message.Message) {
	t.Helper()
	var history []message.Message
	var first *message.Message
	for try := 0; ; try++ { // adaptive thinking skips some questions, and the model sometimes answers without the tool
		nonce := fmt.Sprintf(" (run %d)", time.Now().UnixNano()) // the gateway caches identical requests
		history = []message.Message{message.UserText("Is it warmer in Osaka or in Sapporo right now, and by how much? Think it through, and use the tool for each city." + nonce)}
		req.Messages = history
		if first = run(t, p, req); hasReasoning(first) && len(first.ToolCalls()) > 0 {
			break
		}
		if try == 4 {
			t.Fatalf("the first reply needs signed reasoning and a tool call to replay: %+v", first.Content)
		}
	}
	var results message.Content
	for _, c := range first.ToolCalls() {
		results = append(results, &message.ToolResultBlock{CallID: c.ID, Name: c.Name, Outcome: message.OutcomeSuccess, Content: message.Content{&message.TextBlock{Text: "Sunny, 24°C"}}})
	}
	history = append(history, *first, message.Message{Role: message.RoleTool, Origin: message.Origin{Kind: message.OriginTool}, Content: results})
	req.Messages = history
	second := run(t, p, req)
	return slices.Clone(history), append(history, *second, message.UserText("Thanks. Is that warm for this time of year? One sentence."))
}

// tampered copies the history with one character of the first signature changed, and returns the new signature.
func tampered(t *testing.T, history []message.Message) ([]message.Message, string) {
	t.Helper()
	out := slices.Clone(history)
	for i, m := range out {
		for j, b := range m.Content {
			rb, ok := b.(*message.ReasoningBlock)
			if !ok || rb.Native == nil {
				continue
			}
			var raw map[string]any
			if err := json.Unmarshal(rb.Native.Raw, &raw); err != nil {
				t.Fatal(err)
			}
			sig, _ := raw["signature"].(string)
			if len(sig) < 20 {
				continue
			}
			k := len(sig) / 2
			c := byte('A')
			if sig[k] == 'A' {
				c = 'B'
			}
			raw["signature"] = sig[:k] + string(c) + sig[k+1:]
			nr, _ := json.Marshal(raw)
			cp := *rb
			cp.Native = &message.Native{API: rb.Native.API, Raw: nr}
			content := slices.Clone(m.Content)
			content[j] = &cp
			out[i].Content = content
			return out, raw["signature"].(string)
		}
	}
	t.Fatal("no signature to tamper with")
	return nil, ""
}

// replaceSignature copies the history with every signature set to sig.
func replaceSignature(t *testing.T, history []message.Message, sig string) []message.Message {
	t.Helper()
	out := slices.Clone(history)
	for i, m := range out {
		content := slices.Clone(m.Content)
		for j, b := range content {
			if rb, ok := b.(*message.ReasoningBlock); ok && rb.Native != nil {
				var raw map[string]any
				if err := json.Unmarshal(rb.Native.Raw, &raw); err != nil {
					t.Fatal(err)
				}
				raw["signature"] = sig
				nr, _ := json.Marshal(raw)
				cp := *rb
				cp.Native = &message.Native{API: rb.Native.API, Raw: nr}
				content[j] = &cp
			}
		}
		out[i].Content = content
	}
	return out
}

// TestLiveSignatureReplay checks on the real API what the runtime assumes about signed thinking: signatures are verified, replay across turns holds, and how edits to the prefix are treated, both inside a tool loop and after the turn ended.
func TestLiveSignatureReplay(t *testing.T) {
	p, model := live(t)
	params := provider.Params{MaxOutputTokens: 8192, Reasoning: provider.ReasoningParams{Mode: provider.ReasoningOn}}
	base := provider.Request{Model: model, Params: params, Tools: []message.ToolDef{weather},
		System: []provider.SystemBlock{{Slot: "core", Text: "You are a brief assistant."}}}
	req := base
	loop, turns := signed(t, p, &req)

	t.Run("replay_across_turns", func(t *testing.T) {
		r := base
		r.Messages = turns
		m, err := try(t, p, &r)
		if err != nil || m.Text() == "" {
			t.Fatalf("the unchanged history replays: %+v", err)
		}
	})
	t.Run("drop_block_request", func(t *testing.T) {
		r := base
		r.Messages = loop
		r.Replay, r.Caps.ThinkingPrefixBound = provider.ReplayFallback, true
		rd, _ := p.render(&r)
		if !bytes.Contains(rd.body, []byte("drop_block")) || !slices.Contains(rd.header.Values("anthropic-beta"), betaBlockBinding) {
			t.Fatalf("the fallback asks for drop_block: %s %v", rd.body, rd.header)
		}
		if m, err := try(t, p, &r); err != nil || m.Text() == "" {
			t.Fatalf("the drop_block fallback that CompactUpTo relies on is refused: %+v", err)
		}
	})
	for _, at := range []struct {
		name    string
		history []message.Message
	}{{"in_loop", loop}, {"next_turn", turns}} {
		t.Run(at.name+"/tampered_signature", func(t *testing.T) {
			r := base
			var sig string
			r.Messages, sig = tampered(t, at.history)
			rd, rerr := p.render(&r)
			if rerr != nil || !bytes.Contains(rd.body, []byte(sig)) {
				t.Fatalf("the request must carry the changed signature: %v", rerr)
			}
			_, err := try(t, p, &r)
			if err == nil {
				t.Log("accepted: the signature here is not verified")
				return
			}
			t.Logf("refused: class=%s status=%d rule=%q message=%q", err.Class, err.Status, err.Rule, err.Message)
		})
		t.Run(at.name+"/garbage_signature", func(t *testing.T) {
			r := base
			r.Messages = replaceSignature(t, at.history, "not-a-signature")
			if _, err := try(t, p, &r); err == nil {
				t.Log("accepted: the thinking block does not reach a verifier")
			} else {
				t.Logf("refused: class=%s status=%d rule=%q message=%q", err.Class, err.Status, err.Rule, err.Message)
			}
		})
		edits := []struct {
			name string
			edit func(r *provider.Request)
		}{
			{"system_changed", func(r *provider.Request) {
				r.System = []provider.SystemBlock{{Slot: "core", Text: "You are a very brief assistant."}}
			}},
			{"tool_added", func(r *provider.Request) { r.Tools = []message.ToolDef{weather, clock} }},
			{"first_input_changed", func(r *provider.Request) {
				r.Messages = slices.Clone(r.Messages)
				r.Messages[0] = message.UserText("What is the weather in Osaka today? Use the tool.")
			}},
		}
		for _, e := range edits {
			t.Run(at.name+"/"+e.name, func(t *testing.T) {
				r := base
				r.Messages = at.history
				e.edit(&r)
				_, err := try(t, p, &r)
				if err == nil {
					t.Log("accepted: the signature is not bound to this part of the prefix")
					return
				}
				t.Logf("refused: class=%s status=%d rule=%q message=%q", err.Class, err.Status, err.Rule, err.Message)
				if err.Class != provider.ClassReasoningReplay {
					t.Fatalf("an edited prefix is refused only as reasoning_replay: %+v", err)
				}
				fb := r
				fb.Replay, fb.Caps.ThinkingPrefixBound = provider.ReplayFallback, true
				if _, err := try(t, p, &fb); err != nil {
					t.Errorf("the drop_block fallback that CompactUpTo relies on is refused: %+v", err)
				} else {
					t.Log("drop_block fallback accepted")
				}
				strip := r
				strip.Replay = provider.ReplayFallback
				if _, err := try(t, p, &strip); err != nil {
					t.Errorf("the fallback without reasoning is refused: %+v", err)
				} else {
					t.Log("fallback without reasoning accepted")
				}
			})
		}
	}
}
