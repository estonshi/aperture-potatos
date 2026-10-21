package peer_test

import (
	"encoding/json"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/ext/peer"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/providertest"
	"github.com/estonshi/aperture-potatos/store"
)

// TestLocalRoundTrip: an in-process peer answers in a session of its own; its events reach the caller under the task's hop, its calls the caller's turn, and the next message continues its session.
func TestLocalRoundTrip(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bsp := providertest.NewScriptProvider(provider.APIAnthropicMessages, reply("Bonjour."), reply("Au revoir."))
		b := newAgent(t, "Bee", store.NewMemory(), bsp, kernel.WithDescription("Speaks French."), otherIDs("b_"))
		asp := providertest.NewScriptProvider(provider.APIAnthropicMessages,
			calls(call{"t1", "a2a_send", `{"agent":"Bee","message":"Say hello"}`}), reply("Done."),
			calls(call{"t2", "a2a_send", `{"agent":"Bee","message":"Say goodbye"}`}), reply("Done again."))
		a := newAgent(t, "Tater", store.NewMemory(), asp, kernel.Use(peer.New(peer.Local(b))))
		var rec recorder
		s, err := a.NewSession(t.Context(), kernel.WithSessionFunc(rec.handle))
		if err != nil {
			t.Fatal(err)
		}
		first := ask(t, s, "Greet in French.")
		ask(t, s, "Now say goodbye.")
		res := toolResults(t, s)
		if r := resultText(res["t1"]); !strings.HasPrefix(r, `<agent_reply agent="Bee" task_id="b_in_`) || !strings.Contains(r, "\nBonjour.\n</agent_reply>") {
			t.Fatalf("first reply %q", r)
		}
		if !strings.Contains(resultText(res["t2"]), "Au revoir.") {
			t.Fatalf("second reply %q", resultText(res["t2"]))
		}
		if !strings.Contains(systemText(asp.Requests()[0]), "<description>Speaks French.</description>") {
			t.Fatal("the local peer's description is not in the catalog")
		}
		// the peer's second turn saw its first: one session, two turns
		second := bsp.Requests()[1]
		wire, _ := json.Marshal(second.Messages)
		if !strings.Contains(string(wire), "Say hello") || !strings.Contains(string(wire), "Bonjour.") || !strings.Contains(string(wire), `"source":"peer:Tater"`) {
			t.Fatalf("the peer's second request: %s", wire)
		}
		var relayed []string
		for _, ev := range rec.all() {
			if len(ev.Path) > 0 && ev.Path[0].Name == peer.Kind && ev.Type == event.TypeTurnCompleted {
				relayed = append(relayed, ev.SessionID)
			}
		}
		if len(relayed) != 2 || relayed[0] != relayed[1] || relayed[0] == s.Info().SessionID {
			t.Fatalf("relayed turn.completed from %v", relayed)
		}
		tc, err := s.TurnCalls(first.TurnID)
		if err != nil {
			t.Fatal(err)
		}
		buckets := map[string]int{}
		for _, c := range tc.Calls {
			buckets[c.Bucket]++
		}
		if buckets["peer"] != 1 || buckets["main"] != 2 {
			t.Fatalf("calls of the first turn by bucket: %v", buckets)
		}
		_ = a.Close(t.Context())
		_ = b.Close(t.Context())
	})
}

// TestLocalCycleRefused: A asks B, whose call back to A is refused because A is on the chain the message carried.
func TestLocalCycleRefused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bsp := providertest.NewScriptProvider(provider.APIAnthropicMessages,
			calls(call{"b1", "a2a_send", `{"agent":"Tater","message":"What do you mean?"}`}), reply("I cannot ask back."))
		b := newAgent(t, "Bee", store.NewMemory(), bsp, kernel.Use(peer.New(newFake("Tater"))), otherIDs("b_"))
		asp := providertest.NewScriptProvider(provider.APIAnthropicMessages, calls(call{"t1", "a2a_send", `{"agent":"Bee","message":"Help"}`}), reply("Ok."))
		a := newAgent(t, "Tater", store.NewMemory(), asp, kernel.Use(peer.New(peer.Local(b))))
		s, _ := a.NewSession(t.Context())
		ask(t, s, "Ask Bee.")
		if r := resultText(toolResults(t, s)["t1"]); !strings.Contains(r, "I cannot ask back.") {
			t.Fatalf("A's reply %q", r)
		}
		wire, _ := json.Marshal(bsp.Requests()[1].Messages)
		if !strings.Contains(string(wire), "Cyclic agent call Tater→Bee→Tater refused") {
			t.Fatalf("B's call back was not refused: %s", wire)
		}
		_ = a.Close(t.Context())
		_ = b.Close(t.Context())
	})
}

// TestLocalConcurrentSameContext: two messages to one conversation in one batch reach the peer one after the other, each a turn of its own.
func TestLocalConcurrentSameContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		slow := append([]providertest.Step{{Hold: time.Second}}, reply("one")...)
		bsp := providertest.NewScriptProvider(provider.APIAnthropicMessages, reply("zero"), slow, reply("two"))
		b := newAgent(t, "Bee", store.NewMemory(), bsp, otherIDs("b_"))
		asp := providertest.NewScriptProvider(provider.APIAnthropicMessages,
			calls(call{"t0", "a2a_send", `{"agent":"Bee","message":"m0"}`}), reply("."),
			calls(call{"t1", "a2a_send", `{"agent":"Bee","message":"m1"}`}, call{"t2", "a2a_send", `{"agent":"Bee","message":"m2"}`}), reply(".."))
		a := newAgent(t, "Tater", store.NewMemory(), asp, kernel.Use(peer.New(peer.Local(b))))
		s, _ := a.NewSession(t.Context())
		ask(t, s, "first")
		ask(t, s, "two at once")
		reqs := bsp.Requests()
		if len(reqs) != 3 {
			t.Fatalf("%d peer requests", len(reqs))
		}
		wire, _ := json.Marshal(reqs[2].Messages)
		if !strings.Contains(string(wire), "m1") || !strings.Contains(string(wire), `"one"`) || !strings.Contains(string(wire), "m2") {
			t.Fatalf("the third turn did not follow the second's reply: %s", wire)
		}
		res := toolResults(t, s)
		got := resultText(res["t1"]) + resultText(res["t2"])
		if !strings.Contains(got, "\none\n") || !strings.Contains(got, "\ntwo\n") { // which call took the conversation first is up to the scheduler
			t.Fatalf("replies %q", got)
		}
		_ = a.Close(t.Context())
		_ = b.Close(t.Context())
	})
}

// TestLocalCancel: stopping the exchange interrupts the peer's turn, and the peer reports it canceled.
func TestLocalCancel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		bsp := providertest.NewScriptProvider(provider.APIAnthropicMessages, []providertest.Step{{Hold: -1}})
		b := newAgent(t, "Bee", store.NewMemory(), bsp, otherIDs("b_"))
		lp := peer.Local(b)
		a := newAgent(t, "Tater", store.NewMemory(), providertest.NewScriptProvider(provider.APIAnthropicMessages), kernel.Use(peer.New(lp)), noWake)
		s, _ := a.NewSession(t.Context())
		info := hostSend(t, s, "Bee", "think forever")
		synctest.Wait()
		if err := s.Tasks().Stop(t.Context(), info.ID, "enough"); err != nil {
			t.Fatal(err)
		}
		got, _ := s.Tasks().Wait(t.Context(), info.ID)
		synctest.Wait()
		var st struct {
			TaskID string `json:"task_id"`
		}
		_ = json.Unmarshal(got.State, &st)
		var last peer.Event
		for ev, err := range lp.Subscribe(t.Context(), st.TaskID) {
			if err != nil {
				t.Fatal(err)
			}
			last = ev
		}
		if got.Status != kernel.TaskCanceled || last.State != peer.StateCanceled {
			t.Fatalf("task %s, peer %s", got.Status, last.State)
		}
		_ = a.Close(t.Context())
		_ = b.Close(t.Context())
	})
}
