package compact

import (
	"context"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/message"
)

// TestOverflowClearsCurrentTurn: after an overflow, the running turn's largest results are cleared until the turn fits, both in what is summarized and in what is kept.
func TestOverflowClearsCurrentTurn(t *testing.T) {
	result := func(id, text string) message.Message {
		return message.Message{Role: message.RoleTool, Content: message.Content{&message.ToolResultBlock{CallID: id, Name: "read", Outcome: message.OutcomeSuccess, Content: message.Content{&message.TextBlock{Text: text}}}}}
	}
	call := func(id string) message.Message {
		return message.Message{Role: message.RoleAssistant, Content: message.Content{&message.ToolCallBlock{ID: id, Name: "read"}}}
	}
	hist := []message.Message{
		message.UserText("earlier"), {Role: message.RoleAssistant, Content: message.Content{&message.TextBlock{Text: "ok"}}},
		message.UserText("read the logs"), call("big"), result("big", strings.Repeat("x", 4000)), call("small"), result("small", "tiny"),
	}
	var summarized []message.Message
	req := kernel.CompactRequest{
		Trigger: kernel.TriggerOverflow, History: hist, CurrentTurn: 2, Tokens: 1100, Target: 100, Threshold: 400,
		Estimate: func(msgs []message.Message) int64 {
			n := 0
			for _, m := range msgs {
				n += utf8.RuneCountInString(m.Text())
				for _, b := range m.Content {
					if r, ok := b.(*message.ToolResultBlock); ok {
						n += utf8.RuneCountInString(message.Message{Content: r.Content}.Text())
					}
				}
			}
			return int64(n / 4)
		},
		Summarize: func(_ context.Context, msgs []message.Message, _ string) (string, message.Usage, error) {
			summarized = append(summarized, msgs...)
			return "summary", message.Usage{}, nil
		},
	}
	res, err := (&strategy{}).Compact(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	cleared := func(msgs []message.Message, id string) bool {
		for _, m := range msgs {
			for _, b := range m.Content {
				if r, ok := b.(*message.ToolResultBlock); ok && r.CallID == id {
					return r.Cleared
				}
			}
		}
		t.Fatalf("no result %s", id)
		return false
	}
	if len(res.Replace) != 5 || !cleared(res.Replace, "big") || cleared(res.Replace, "small") {
		t.Fatalf("kept %+v", res.Replace)
	}
	if !cleared(summarized, "big") {
		t.Fatal("the summary request still carries the oversized result")
	}
	if cleared(hist, "big") {
		t.Fatal("the request's history was changed in place")
	}
}
