package kernel_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/providertest"
	"github.com/estonshi/aperture-potatos/store"
)

func TestRewindKeepsPrefixByteForByte(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		h := newHarness(t, [][]providertest.Step{textReply("one"), textReply("two"), textReply("three"), textReply("two again")})
		ask(t, h.sess, "first")
		second := ask(t, h.sess, "second")
		ask(t, h.sess, "third")
		turns := h.sess.Turns()
		if len(turns) != 3 || turns[1].ID != second.TurnID || text(turns[1].Input) != "second" {
			t.Fatalf("turns: %+v", turns)
		}
		if _, err := h.sess.Rewind(ctx, "turn_nope"); !errors.Is(err, kernel.ErrUnknownTurn) {
			t.Fatalf("unknown turn: %v", err)
		}
		rep, err := h.sess.Rewind(ctx, second.TurnID)
		if err != nil {
			t.Fatal(err)
		}
		if rep.Epoch != 2 || rep.Dropped != 4 || text(rep.Input) != "second" {
			t.Fatalf("report: %+v", rep)
		}
		ask(t, h.sess, "second, rephrased")
		reqs := h.sp.Requests()
		before, after := reqs[1], reqs[3] // the second turn's request and the one after the rewind
		if wire(t, &provider.Request{System: before.System, Tools: before.Tools}) != wire(t, &provider.Request{System: after.System, Tools: after.Tools}) {
			t.Fatal("the rewind changed the system or the tools")
		}
		if messagesJSON(t, after.Messages[:2]) != messagesJSON(t, before.Messages[:2]) {
			t.Fatalf("the kept lines differ from what the second turn sent:\n%s\n%s", messagesJSON(t, before.Messages[:2]), messagesJSON(t, after.Messages[:2]))
		}
		if after.Messages[2].Text() != "second, rephrased" || len(after.Messages) != 3 {
			t.Fatalf("after the rewind: %+v", after.Messages)
		}
		h.close()
		lines := h.lines()
		cur := store.Current(lines)
		if b := cur[0].Boundary; b.Reason != "rewind" || b.TurnID != second.TurnID || cur[1].CopyOf == "" || cur[0].Epoch != 2 {
			t.Fatalf("new epoch: %+v %+v", b, cur[1])
		}
		if h.rec.count(event.TypeContextRewound) != 1 {
			t.Fatal("no context.rewound")
		}
	})
}
