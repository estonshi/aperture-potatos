package conformance

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
)

func line(seq int64, epoch int, role message.Role, reason string) message.Message {
	m := message.Message{ID: "msg_" + string(rune('a'+seq)), Seq: seq, Epoch: epoch, Role: role}
	if reason != "" {
		m.Boundary = &message.Boundary{Reason: reason}
	}
	return m
}

func TestCheckEpochs(t *testing.T) {
	good := []message.Message{line(1, 1, message.RoleSystem, "new"), line(2, 1, message.RoleUser, ""), line(3, 2, message.RoleSystem, "compaction"), line(4, 2, message.RoleUser, ""), line(5, 2, message.RoleSystem, "dropped")}
	good[3].CopyOf = good[1].ID
	if err := checkEpochs(good); err != nil {
		t.Fatal(err)
	}
	sameEpochCopy := append([]message.Message(nil), good...)
	sameEpochCopy[3].CopyOf = good[2].ID
	for name, lines := range map[string][]message.Message{
		"epoch goes back":       {line(1, 2, message.RoleSystem, "new"), line(2, 1, message.RoleUser, "")},
		"epoch without system":  {line(1, 1, message.RoleSystem, "new"), line(2, 2, message.RoleUser, "")},
		"second system line":    {line(1, 1, message.RoleSystem, "new"), line(2, 1, message.RoleSystem, "")},
		"copy within its epoch": sameEpochCopy,
	} {
		if err := checkEpochs(lines); err == nil || !strings.HasPrefix(err.Error(), "I6") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

func TestCheckRequests(t *testing.T) {
	user := message.Message{Role: message.RoleUser, Seq: 2, TurnID: "turn_01", Epoch: 1}
	asst := message.Message{Role: message.RoleAssistant, Seq: 3, TurnID: "turn_01", Epoch: 1}
	tool := message.Message{Role: message.RoleTool, Seq: 4, TurnID: "turn_01", Epoch: 1}
	side := message.Message{Role: message.RoleUser} // a side call's own prompt
	req := func(model string, tools []string, prefixBound, pause bool, msgs ...message.Message) json.RawMessage {
		r := provider.Request{Model: model, Messages: msgs, PauseTail: pause, Caps: provider.Capabilities{ThinkingPrefixBound: prefixBound}}
		for _, n := range tools {
			r.Tools = append(r.Tools, message.ToolDef{Name: n})
		}
		b, _ := json.Marshal(r)
		return b
	}
	if err := checkRequests([]json.RawMessage{
		req("m", []string{"a"}, false, false, user),
		req("m", []string{"a"}, false, false, user, asst, tool),
		req("m", []string{"a"}, false, true, user, asst),        // pause_turn continuation
		req("small", nil, false, false, user, asst, tool, side), // a side call may differ
	}); err != nil {
		t.Fatal(err)
	}
	next := message.Message{Role: message.RoleUser, Seq: 6, TurnID: "turn_02", Epoch: 1}
	for name, sent := range map[string][]json.RawMessage{
		"ends with assistant":                  {req("m", nil, false, false, user, asst)},
		"model changes in turn":                {req("m", nil, false, false, user), req("n", nil, false, false, user, asst, tool)},
		"tools change in turn":                 {req("m", []string{"a"}, false, false, user), req("m", []string{"a", "b"}, false, false, user, asst, tool)},
		"tools change in a prefix-bound epoch": {req("m", []string{"a"}, true, false, user), req("m", []string{"a", "b"}, true, false, user, asst, tool, next)},
	} {
		err := checkRequests(sent)
		if err == nil || !(strings.HasPrefix(err.Error(), "I9") || strings.HasPrefix(err.Error(), "I10")) {
			t.Errorf("%s: %v", name, err)
		}
	}
	if err := checkRequests([]json.RawMessage{req("m", []string{"a"}, false, false, user), req("m", []string{"a", "b"}, false, false, user, asst, tool, next)}); err != nil {
		t.Errorf("tools may change at a new turn on a model that does not bind the prefix: %v", err)
	}
}
