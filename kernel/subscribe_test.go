package kernel_test

import (
	"context"
	"encoding/json"
	"testing"
	"testing/synctest"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/providertest"
)

func TestRunYieldsTheConsumingTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, [][]providertest.Step{textReply("one"), textReply("two")})
		rc, _ := h.sess.Send(kernel.Text("first"))
		rc.Wait(t.Context())
		var types []event.Type
		var turn string
		for ev, err := range h.sess.Run(t.Context(), kernel.Text("second")) {
			if err != nil {
				t.Fatal(err)
			}
			if turn == "" {
				turn = ev.TurnID
			}
			if ev.TurnID != turn {
				t.Fatalf("event of another turn: %+v", ev)
			}
			types = append(types, ev.Type)
		}
		if types[0] != event.TypeTurnStarted || types[len(types)-1] != event.TypeTurnCompleted || turn != "turn_02" {
			t.Fatalf("types %v turn %s", types, turn)
		}
		h.close()
	})
}

func TestEventsIteratorBreakUnsubscribes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, [][]providertest.Step{textReply("pong")})
		done := make(chan int)
		go func() {
			n := 0
			for ev, err := range h.sess.Events(t.Context()) {
				if err != nil {
					t.Error(err)
					break
				}
				n++
				if ev.Type == event.TypeTurnCompleted {
					break
				}
			}
			done <- n
		}()
		synctest.Wait()
		h.sess.Send(kernel.Text("ping"))
		if n := <-done; n == 0 {
			t.Fatal("no events")
		}
		h.close()
	})
}

func TestAgentSinkSpansSessionsAndClosesOnDone(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ch := make(chan event.Event, 8)
		sp := providertest.NewScriptProvider(provider.APIAnthropicMessages, textReply("a"), textReply("b"))
		ag, err := kernel.New(kernel.WithModel(provider.ModelRef{Provider: sp, Model: "m"}), kernel.WithEventSink(ch, kernel.WithCloseOnDone(), kernel.WithTypes("session.")))
		if err != nil {
			t.Fatal(err)
		}
		sessions := map[string]int{}
		collected := make(chan struct{})
		go func() {
			for ev := range ch {
				if ev.Type == event.TypeSessionClosed {
					sessions[ev.SessionID]++
				}
			}
			close(collected)
		}()
		for range 2 {
			s, err := ag.NewSession(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			rc, _ := s.Send(kernel.Text("hi"))
			rc.Wait(t.Context())
		}
		if err := ag.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		<-collected
		if len(sessions) != 2 {
			t.Fatalf("closed sessions seen: %v", sessions)
		}
	})
}

// TestSnapshotIsCurrentWhenEventsArrive covers O3: a snapshot taken on an event already shows the change it reports.
func TestSnapshotIsCurrentWhenEventsArrive(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, [][]providertest.Step{textReply("pong")})
		var phases []string
		h.sess.SubscribeFunc(func(ctx context.Context, ev event.Event) error {
			if ev.Type == event.TypeTurnCompleted {
				phases = append(phases, h.sess.Snapshot().Phase)
			}
			return nil
		})
		rc, _ := h.sess.Send(kernel.Text("ping"))
		rc.Wait(t.Context())
		if p := h.sess.Snapshot().Phase; p != "idle" {
			t.Fatalf("after Wait the phase is %s", p)
		}
		h.close()
		if len(phases) != 1 || phases[0] == "running" {
			t.Fatalf("phase seen with turn.completed: %v", phases)
		}
	})
}

func TestNativeAndRawEvents(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		u := usage
		thinking := &message.ReasoningBlock{Kind: message.ReasoningSummary, Text: "hmm", Native: &message.Native{API: "anthropic.messages", Raw: json.RawMessage(`{"type":"thinking","signature":"sig"}`)}}
		m := &message.Message{Role: message.RoleAssistant, Origin: message.Origin{Kind: message.OriginModel}, Content: message.Content{thinking, &message.TextBlock{Text: "hi"}}, Producer: producer(), Usage: &u, StopReason: "end_turn"}
		script := []providertest.Step{
			{Event: provider.StreamEvent{Kind: provider.SEStart}},
			{Event: provider.StreamEvent{Kind: provider.SERaw, Text: "message_start", Raw: json.RawMessage(`{"type":"message_start"}`)}},
			{Event: provider.StreamEvent{Kind: provider.SEBlockStart, Index: 0, BlockType: message.TypeReasoning, ReasoningKind: message.ReasoningSummary}},
			{Event: provider.StreamEvent{Kind: provider.SEReasoningDelta, Index: 0, ReasoningKind: message.ReasoningSummary, Text: "hmm"}},
			{Event: provider.StreamEvent{Kind: provider.SEBlockStop, Index: 0, Block: thinking}},
			{Event: provider.StreamEvent{Kind: provider.SEEnd, Message: m, Stop: &provider.Stop{Reason: provider.StopEndTurn}, Usage: &u}},
		}
		h := newHarness(t, [][]providertest.Step{script}, kernel.WithRawProviderEvents())
		native := &recorder{}
		h.sess.SubscribeFunc(native.handle, kernel.WithNative())
		rc, _ := h.sess.Send(kernel.Text("think"))
		rc.Wait(t.Context())
		h.close()
		find := func(r *recorder) *message.ReasoningBlock {
			for _, ev := range r.all() {
				if d, ok := ev.Data.(*event.MessageCompleted); ok && d.Message.Role == message.RoleAssistant {
					return d.Message.Content[0].(*message.ReasoningBlock)
				}
			}
			return nil
		}
		if b := find(h.rec); b == nil || b.Native != nil || b.Text != "hmm" {
			t.Fatalf("the public view must drop the signature: %+v", b)
		}
		if b := find(native); b == nil || b.Native == nil {
			t.Fatal("WithNative keeps the signature")
		}
		if h.rec.count(event.TypeProviderRaw) != 1 || h.rec.count(event.TypeReasoningCompleted) != 1 || !h.sp.Requests()[0].EmitRaw {
			t.Fatal("raw events were not published")
		}
		if got := h.lines()[2].Content[0].(*message.ReasoningBlock); got.Native == nil {
			t.Fatal("the session file keeps the signature")
		}
	})
}
