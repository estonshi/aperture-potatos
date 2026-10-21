package project

import (
	"encoding/json"
	"regexp"
	"testing"

	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
)

func native(api provider.API, raw string) *message.Native {
	return &message.Native{API: string(api), Raw: json.RawMessage(raw)}
}

func assistant(p *message.Producer, blocks ...message.Block) message.Message {
	return message.Message{Role: message.RoleAssistant, Origin: message.Origin{Kind: message.OriginModel}, Producer: p, Content: blocks}
}

func TestForTargetAnthropic(t *testing.T) {
	anth := &message.Producer{API: "anthropic.messages", Endpoint: "api.anthropic.com", Model: "claude-opus-5-5"}
	chat := &message.Producer{API: "openai.chat", Endpoint: "api.deepseek.com", Model: "deepseek-chat"}
	msgs := []message.Message{
		message.UserText("hi"),
		assistant(anth, &message.ReasoningBlock{Kind: message.ReasoningOmitted, Native: native(provider.APIAnthropicMessages, `{"type":"thinking","thinking":"","signature":"s"}`)},
			&message.OpaqueBlock{Kind: "anthropic.messages:server_tool_use", Native: native(provider.APIAnthropicMessages, `{"type":"server_tool_use"}`)},
			&message.TextBlock{Text: "a", Native: native(provider.APIAnthropicMessages, `{"type":"text","text":"a","citations":[]}`)}),
		message.UserText("next"),
		assistant(chat, &message.ReasoningBlock{Kind: message.ReasoningRaw, Text: "hmm", Native: native(provider.APIOpenAIChat, `{"field":"reasoning_content"}`)},
			&message.TextBlock{Text: "b"}, &message.ToolCallBlock{ID: "call_1", Name: "x", Input: json.RawMessage(`{}`), Native: native(provider.APIOpenAIResponses, `{"id":"fc_1"}`)}),
	}
	msgs[3].Native = native(provider.APIOpenAIChat, `{"reasoning_details":[]}`)
	got, st := ForTarget(msgs, Target{API: provider.APIAnthropicMessages, Endpoint: "api.anthropic.com", Family: "claude-opus-5-5"})
	if st.DroppedReasoning != 1 || st.DroppedOpaque != 0 {
		t.Fatalf("stats %+v", st)
	}
	if len(got[1].Content) != 3 {
		t.Fatalf("anthropic blocks are replayed whole and in order, even with empty text: %d", len(got[1].Content))
	}
	if len(got[3].Content) != 2 || got[3].Native != nil {
		t.Fatalf("foreign reasoning and message native must go: %+v", got[3])
	}
	if got[3].Content[1].(*message.ToolCallBlock).Native != nil || msgs[3].Content[2].(*message.ToolCallBlock).Native == nil {
		t.Fatal("a foreign native is stripped on a copy, never in the history")
	}
}

func TestForTargetChatNeedsSameEndpointAndFamily(t *testing.T) {
	r := func(p *message.Producer) message.Message {
		return assistant(p, &message.ReasoningBlock{Kind: message.ReasoningRaw, Text: "t", Native: native(provider.APIOpenAIChat, `{"field":"reasoning_content"}`)}, &message.TextBlock{Text: "x"})
	}
	tgt := Target{API: provider.APIOpenAIChat, Endpoint: "api.deepseek.com", Family: "deepseek-chat"}
	same := r(&message.Producer{API: "openai.chat", Endpoint: "api.deepseek.com", Model: "deepseek-chat"})
	otherHost := r(&message.Producer{API: "openai.chat", Endpoint: "openrouter.ai", Model: "deepseek-chat"})
	got, st := ForTarget([]message.Message{same, otherHost}, tgt)
	if len(got[0].Content) != 2 || len(got[1].Content) != 1 || st.DroppedReasoning != 1 {
		t.Fatalf("got %d/%d blocks, %+v", len(got[0].Content), len(got[1].Content), st)
	}
}

func TestHealDangling(t *testing.T) {
	calls := assistant(nil, &message.ToolCallBlock{ID: "a", Name: "x"}, &message.ToolCallBlock{ID: "b", Name: "y"})
	partial := message.Message{Role: message.RoleTool, Content: message.Content{&message.ToolResultBlock{CallID: "b", Outcome: message.OutcomeSuccess}, &message.ToolResultBlock{CallID: "zz", Outcome: message.OutcomeSuccess}}}
	orphan := message.Message{Role: message.RoleTool, Content: message.Content{&message.ToolResultBlock{CallID: "q"}}}
	msgs := []message.Message{message.UserText("go"), calls, partial, orphan, message.UserText("more"), calls}
	got, healed := HealDangling(msgs, "Not executed.")
	if len(healed) != 3 || healed[0] != "a" || healed[1] != "a" || healed[2] != "b" {
		t.Fatalf("healed %v", healed)
	}
	if len(got) != 6 {
		t.Fatalf("an orphan tool message is dropped and a missing one is added: %d messages", len(got))
	}
	first := got[2].Content
	if len(first) != 2 || first[0].(*message.ToolResultBlock).CallID != "a" || first[1].(*message.ToolResultBlock).CallID != "b" {
		t.Fatalf("results follow call order: %+v", first)
	}
	if r := first[0].(*message.ToolResultBlock); r.Outcome != message.OutcomeNotExecuted || !r.IsError {
		t.Fatalf("synthetic result: %+v", r)
	}
	if got[5].Role != message.RoleTool || len(got[5].Content) != 2 {
		t.Fatalf("a trailing call gets a tool message: %+v", got[5])
	}
	if len(partial.Content) != 2 {
		t.Fatal("history is not modified")
	}
}

func TestCheckTailAndRemap(t *testing.T) {
	msgs := []message.Message{message.UserText("x"), assistant(nil, &message.ToolCallBlock{ID: "call.1/x", Name: "n"}),
		{Role: message.RoleTool, Content: message.Content{&message.ToolResultBlock{CallID: "call.1/x"}}}}
	if CheckTail(msgs[:2], false) == nil || CheckTail(msgs[:2], true) != nil || CheckTail(msgs, false) != nil {
		t.Fatal("tail check")
	}
	valid := regexp.MustCompile(`^[a-zA-Z0-9_-]+$`).MatchString
	got := RemapIDs(msgs, valid)
	id := got[1].Content[0].(*message.ToolCallBlock).ID
	if !valid(id) || len(id) != 26 || got[2].Content[0].(*message.ToolResultBlock).CallID != id {
		t.Fatalf("remapped %q", id)
	}
	if msgs[1].Content[0].(*message.ToolCallBlock).ID != "call.1/x" {
		t.Fatal("history is not modified")
	}
}

func TestStripReasoning(t *testing.T) {
	th := func() message.Block { return &message.ReasoningBlock{Kind: message.ReasoningSummary} }
	msgs := []message.Message{assistant(nil, th(), &message.TextBlock{Text: "a"}), message.UserText("u"), assistant(nil, th(), &message.OpaqueBlock{Kind: "k"}, &message.TextBlock{Text: "b"})}
	got := StripReasoning(msgs, true)
	if len(got[0].Content) != 1 || len(got[2].Content) != 3 {
		t.Fatalf("keep latest: %d %d", len(got[0].Content), len(got[2].Content))
	}
	if got = StripReasoning(msgs, false); len(got[2].Content) != 1 {
		t.Fatal("strip all")
	}
}
