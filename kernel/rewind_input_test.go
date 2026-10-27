package kernel_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/provider/providertest"
	"github.com/estonshi/aperture-potatos/store"
)

// injected runs one turn whose tool takes a second, sends a second input while it runs, and returns the turn, the
// first input's id and the second input's receipt.
func injected(t *testing.T, h *harness) (turnID, firstID string, second *kernel.Receipt) {
	t.Helper()
	first, err := h.sess.Send(kernel.Input{Text: "find a hotel"})
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond) // the turn starts and blocks in the tool
	second, err = h.sess.Send(kernel.Input{Text: "skip the expensive ones"})
	if err != nil {
		t.Fatal(err)
	}
	res, err := first.Wait(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.Wait(context.Background()); err != nil {
		t.Fatal(err)
	}
	return res.TurnID, first.InputID(), second
}

func TestRewindToInputMidTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		h := newHarness(t, [][]providertest.Step{toolReply(call{"c1", "slow", `{}`}), textReply("here is one")},
			kernel.WithTool(&echoTool{name: "slow", delay: time.Second}))
		turnID, firstID, second := injected(t, h)
		msgs := h.sess.Messages()
		if len(msgs) != 6 || !slices.Equal(msgs[4].InputIDs, []string{second.InputID()}) || msgs[4].TurnID != turnID {
			t.Fatalf("the injected line: %+v", msgs[4])
		}
		turns := h.sess.Turns()
		if len(turns) != 1 || turns[0].ID != turnID || turns[0].InputID != firstID {
			t.Fatalf("turns: %+v", turns)
		}
		rep, err := h.sess.Rewind(ctx, second.InputID())
		if err != nil {
			t.Fatal(err)
		}
		if rep.InputID != second.InputID() || rep.TurnID != turnID || rep.Epoch != 2 || rep.Dropped != 2 || !strings.Contains(text(rep.Input), "expensive") {
			t.Fatalf("report: %+v", rep)
		}
		if got := roles(h.sess.Messages()); got != "system,user,assistant,tool" {
			t.Fatalf("after the rewind: %s", got)
		}
		var rewound *event.ContextRewound
		for _, ev := range h.rec.all() {
			if r, ok := ev.Data.(*event.ContextRewound); ok {
				rewound = r
			}
		}
		if rewound == nil || rewound.InputID != second.InputID() || rewound.TurnID != turnID {
			t.Fatalf("context.rewound: %+v", rewound)
		}
		h.close()
		cur := store.Current(h.lines())
		if b := cur[0].Boundary; b.TurnID != turnID || b.Reason != "rewind" {
			t.Fatalf("boundary: %+v", b)
		}
		if roles(cur) != "system,user,assistant,tool" {
			t.Fatalf("stored lines: %s", roles(cur))
		}
	})
}

func TestForkAtInputMidTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		h := newHarness(t, [][]providertest.Step{toolReply(call{"c1", "slow", `{}`}), textReply("here is one")},
			kernel.WithTool(&echoTool{name: "slow", delay: time.Second}))
		turnID, firstID, second := injected(t, h)
		seq := h.sess.Messages()[3].Seq // the tool line, the last kept one
		fork, err := h.sess.Fork(ctx, kernel.ForkAt(second.InputID()))
		if err != nil {
			t.Fatal(err)
		}
		if got := roles(fork.Messages()); got != "system,user,assistant,tool" {
			t.Fatalf("fork lines: %s", got)
		}
		if turns := fork.Turns(); len(turns) != 1 || turns[0].ID != turnID || turns[0].InputID != firstID {
			t.Fatalf("fork turns: %+v", turns)
		}
		fid := fork.Info().SessionID
		h.close()
		lines, err := h.store.Load(ctx, fid)
		if err != nil {
			t.Fatal(err)
		}
		if b := lines[0].Boundary; b.Reason != "fork" || b.From == nil || b.From.Seq != seq {
			t.Fatalf("fork boundary: %+v", b)
		}
		if len(lines) != 4 || !slices.Equal(lines[1].InputIDs, []string{firstID}) {
			t.Fatalf("fork copies: %+v", lines)
		}
	})
}

func TestRewindToMergedInput(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		h := newHarness(t, [][]providertest.Step{textReply("both")})
		one, err := h.sess.Send(kernel.Input{Text: "first"})
		if err != nil {
			t.Fatal(err)
		}
		two, err := h.sess.Send(kernel.Input{Text: "second"})
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range []*kernel.Receipt{one, two} {
			if _, err := r.Wait(ctx); err != nil {
				t.Fatal(err)
			}
		}
		msgs := h.sess.Messages()
		if len(msgs) != 3 || len(msgs[1].InputIDs) != 2 {
			t.Fatalf("merged line: %+v", msgs)
		}
		rep, err := h.sess.Rewind(ctx, two.InputID())
		if err != nil {
			t.Fatal(err)
		}
		if rep.InputID != two.InputID() || rep.Dropped != 2 || !strings.Contains(text(rep.Input), "first") || !strings.Contains(text(rep.Input), "second") {
			t.Fatalf("report: %+v", rep)
		}
		if got := roles(h.sess.Messages()); got != "system" {
			t.Fatalf("after the rewind: %s", got)
		}
		h.close()
	})
}

func TestUnknownInputOrTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		h := newHarness(t, [][]providertest.Step{textReply("hi")})
		ask(t, h.sess, "hello")
		if _, err := h.sess.Rewind(ctx, "in_nope"); !errors.Is(err, kernel.ErrUnknownInput) || !errors.Is(err, kernel.ErrUnknownTurn) || !strings.Contains(err.Error(), "in_nope") {
			t.Fatalf("unknown rewind: %v", err)
		}
		if _, err := h.sess.Fork(ctx, kernel.ForkAt("in_nope")); !errors.Is(err, kernel.ErrUnknownInput) || !errors.Is(err, kernel.ErrUnknownTurn) {
			t.Fatalf("unknown fork: %v", err)
		}
		h.close()
	})
}

func TestRewindToInputAfterResume(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		h := newHarness(t, [][]providertest.Step{toolReply(call{"c1", "slow", `{}`}), textReply("here is one")},
			kernel.WithTool(&echoTool{name: "slow", delay: time.Second}))
		_, _, second := injected(t, h)
		id := h.sess.Info().SessionID
		h.close()
		ag := resumeAgent(t, h.store)
		defer func() { _ = ag.Close(ctx) }()
		s, err := ag.Resume(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		rep, err := s.Rewind(ctx, second.InputID())
		if err != nil {
			t.Fatal(err)
		}
		got := roles(s.Messages())
		if rep.InputID != second.InputID() || rep.Epoch != 2 || got != "system,user,assistant,tool" {
			t.Fatalf("report %+v, lines %s", rep, got)
		}
	})
}
