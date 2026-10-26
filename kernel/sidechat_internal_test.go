package kernel

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/internal/prompt"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/providertest"
)

func reasoned(id, turn, text string) message.Message {
	return message.Message{ID: id, TurnID: turn, Role: message.RoleAssistant, Content: message.Content{&message.ReasoningBlock{Kind: message.ReasoningSummary, Text: "why " + text}, &message.TextBlock{Text: text}}}
}

func hasReasoning(m message.Message) bool {
	for _, b := range m.Content {
		if _, ok := b.(*message.ReasoningBlock); ok {
			return true
		}
	}
	return false
}

func TestSideChatReasoningOnBoundModels(t *testing.T) {
	main := []message.Message{{ID: "m1", Role: message.RoleUser, Content: message.Content{&message.TextBlock{Text: "hi"}}}, reasoned("m2", "turn_01", "hello")}
	own := []message.Message{{ID: "s1", Role: message.RoleUser}, reasoned("s2", "turn_01", "earlier"), {ID: "s3", Role: message.RoleUser, TurnID: "turn_02"}, reasoned("s4", "turn_02", "now")}
	v := &mainView{lines: main}
	got := v.messages(own, "turn_02", true, true)
	if len(got) != 6 || !hasReasoning(got[1]) || hasReasoning(got[3]) || !hasReasoning(got[5]) {
		t.Fatalf("bound and verbatim: the main part and the running turn keep theirs, an earlier turn loses it: %v %v %v", hasReasoning(got[1]), hasReasoning(got[3]), hasReasoning(got[5]))
	}
	if !hasReasoning(own[1]) || !hasReasoning(main[1]) {
		t.Fatal("the committed lines are never changed in place")
	}
	if got := v.messages(own, "turn_02", true, false); hasReasoning(got[1]) || !hasReasoning(got[5]) {
		t.Fatal("a main part on another system, tools or model loses its reasoning too")
	}
	if got := v.messages(own, "", true, true); hasReasoning(got[5]) {
		t.Fatal("a side call outside any turn has no running turn to keep")
	}
	if got := v.messages(own, "turn_02", false, false); !hasReasoning(got[1]) || !hasReasoning(got[3]) {
		t.Fatal("a model that is not prefix-bound replays everything")
	}
}

func TestSideChatVerbatim(t *testing.T) {
	model := provider.ModelInfo{API: provider.APIAnthropicMessages, Model: "m"}
	tools := []message.ToolDef{{Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`)}}
	v := &mainView{model: model, system: []provider.SystemBlock{{Slot: "core", Text: "a", Cache: true}}, tools: tools}
	if !v.verbatim(model, []provider.SystemBlock{{Text: "a"}}, tools, nil) {
		t.Fatal("a resumed system keeps only its text, which is all the request carries")
	}
	spaced := []message.ToolDef{{Name: "echo", InputSchema: json.RawMessage(`{ "type": "object" }`)}}
	if !v.verbatim(model, []provider.SystemBlock{{Text: "a"}}, spaced, []message.HostedTool{}) {
		t.Fatal("definitions read back from a file encode the same, and no hosted tools is no hosted tools")
	}
	if v.verbatim(model, []provider.SystemBlock{{Text: "b"}}, tools, nil) || v.verbatim(provider.ModelInfo{Model: "other"}, []provider.SystemBlock{{Text: "a"}}, tools, nil) || v.verbatim(model, []provider.SystemBlock{{Text: "a"}}, nil, nil) {
		t.Fatal("another system, model or tool set changes the prefix")
	}
}

func TestSideChatClosesWhenItsMainSessionIsGone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := providertest.NewScriptProvider(provider.APIAnthropicMessages, reply("main"))
		ag, err := New(WithModel(provider.ModelRef{Provider: sp, Model: "m"}))
		if err != nil {
			t.Fatal(err)
		}
		main, err := ag.NewSession(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		r, _ := main.Send(Text("hello"))
		_, _ = r.Wait(t.Context())
		side, err := main.SideChat(t.Context(), "s")
		if err != nil {
			t.Fatal(err)
		}
		var reason string
		side.SubscribeFunc(func(_ context.Context, ev event.Event) error {
			if c, ok := ev.Data.(*event.SessionClosed); ok {
				reason = c.Reason
			}
			return nil
		})
		main.post(func(*state) { panic("kernel bug") }) // a crash skips closing the side chats
		<-main.done
		r, _ = side.Send(Text("still there?"))
		if res, _ := r.Wait(t.Context()); res.Outcome != OutcomeDropped && !errors.Is(res.Err, ErrSessionClosed) {
			t.Fatalf("the turn cannot take its main part, so the input goes: %+v", res)
		}
		<-side.done
		synctest.Wait()
		if reason != "main_closed" {
			t.Fatalf("closed for %q", reason)
		}
		_ = ag.Close(t.Context())
	})
}

func TestSideChatNamesTheFirstNewMainLine(t *testing.T) {
	line := func(seq int64, role message.Role, blocks ...message.Block) message.Message {
		return message.Message{Seq: seq, Role: role, Content: message.Content(blocks)}
	}
	text := func(s string) message.Block { return &message.TextBlock{Text: s} }
	call := func(name string) message.Block { return &message.ToolCallBlock{ID: name, Name: name} }
	result := func(name string) message.Block { return &message.ToolResultBlock{CallID: name, Name: name} }
	main := []message.Message{
		line(2, message.RoleUser, text("hello")),
		line(3, message.RoleAssistant, call("lookup"), call("fetch"), call("lookup")),
		line(4, message.RoleTool, result("lookup")),
		line(5, message.RoleTool, result("lookup"), result("fetch")),
		line(6, message.RoleAssistant, &message.ReasoningBlock{Text: "hm"}, text("  Sunny,\n24C.  ")),
		line(7, message.RoleUser, &message.ReminderBlock{Text: "Task task_01 finished."}),
		line(8, message.RoleUser, &message.ReminderBlock{Text: "note"}, text("Use the lookup tool for the weather in Paris, then tell me")),
		line(9, message.RoleUser, &message.ImageBlock{}),
		line(10, message.RoleUser, text("天气怎么样？请用工具查一下巴黎今天和明天的天气，然后把结果整理成一张表格发给我，谢谢你的帮助。再补充一句：")),
	}
	v := &mainView{lines: append(slices.Clone(main), pendingLine(nil, nil)), shared: len(main)}
	for seq, want := range map[int64]string{
		2:  "the calls to lookup, fetch",
		3:  "the result of lookup",
		4:  "the results of lookup, fetch",
		5:  `the reply "Sunny, 24C."`,
		6:  `the message "Task task_01 finished."`,
		7:  `the message "Use the lookup tool for the weather in Paris, th…"`, // 48 code points, then the mark
		8:  "",                                                                // nothing to quote
		9:  `the message "天气怎么样？请用工具查一下巴黎今天和明天的天气，然后把结果整理成一张表格发给我，谢谢你的帮助。再…"`,
		10: "", // only the placeholder is newer, and it is no line of the main session
		0:  "", // the seq of the latest reply is unknown
		1:  "", // every line is newer: the main session compacted or rewound since
	} {
		if got := v.firstNew(seq); got != want {
			t.Errorf("after seq %d: %q, want %q", seq, got, want)
		}
	}
	if got := prompt.Textf("side_chat_main_changed", map[string]string{"first": v.firstNew(3)}); !strings.Contains(got, "side conversation: everything from the result of lookup on is new. Answer") {
		t.Fatalf("named: %q", got)
	}
	if got := prompt.Textf("side_chat_main_changed", map[string]string{"first": ""}); !strings.Contains(got, "side conversation. Answer from the main conversation") {
		t.Fatalf("unnamed: %q", got)
	}
}
