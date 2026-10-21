package kernel_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider/providertest"
)

func TestSearchFindsWhatCompactionTookOut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		h := newHarness(t, [][]providertest.Step{textReply("apples are red"), textReply("Summary."), textReply("pears are green"), textReply("plums are purple")},
			kernel.Use(kernel.ExtensionFunc("comp", func(r kernel.Registrar) error { _, err := r.Compactor(&keepTurnCompactor{}); return err })),
			kernel.WithContextPolicy(kernel.ContextPolicy{MinSummaryTokens: 1, MaxSummaryTokens: 50}),
		)
		first := ask(t, h.sess, "tell me about apples")
		if _, err := h.sess.Compact(ctx, ""); err != nil {
			t.Fatal(err)
		}
		second := ask(t, h.sess, "tell me about pears")
		if _, err := h.sess.Rewind(ctx, second.TurnID); err != nil {
			t.Fatal(err)
		}
		third := ask(t, h.sess, "tell me about plums")
		hits, err := h.sess.SearchHistory(ctx, kernel.HistoryQuery{Words: []string{"APPLES"}})
		if err != nil {
			t.Fatal(err)
		}
		if len(hits) != 2 || hits[0].Role != message.RoleAssistant || hits[0].Current || hits[0].TurnID != first.TurnID || hits[0].Epoch != 1 {
			t.Fatalf("what the compaction took out is found, newest first, outside the context: %+v", hits)
		}
		m, current, err := h.sess.HistoryLine(ctx, hits[0].Seq)
		if err != nil || m.Text() != "apples are red" || current {
			t.Fatalf("line: %v %v %+v", err, current, m)
		}
		if hits, _ := h.sess.SearchHistory(ctx, kernel.HistoryQuery{Words: []string{"apples"}, Current: true}); len(hits) != 0 {
			t.Fatalf("Current leaves out what the compaction took out: %+v", hits)
		}
		if hits, _ := h.sess.SearchHistory(ctx, kernel.HistoryQuery{Words: []string{"pears"}}); len(hits) != 0 {
			t.Fatalf("what the rewind took out is not found: %+v", hits)
		}
		lines, _ := h.store.Load(ctx, h.sess.Info().SessionID)
		for _, l := range lines {
			if l.TurnID == second.TurnID {
				if _, _, err := h.sess.HistoryLine(ctx, l.Seq); !errors.Is(err, kernel.ErrUnknownLine) {
					t.Fatalf("a rewound line is not read by seq: %v", err)
				}
			}
		}
		hits, _ = h.sess.SearchHistory(ctx, kernel.HistoryQuery{Words: []string{"plums"}})
		if len(hits) != 2 || !hits[0].Current || hits[0].TurnID != third.TurnID {
			t.Fatalf("the current context is found: %+v", hits)
		}
		if _, _, err := h.sess.HistoryLine(ctx, 999); !errors.Is(err, kernel.ErrUnknownLine) {
			t.Fatalf("unknown seq: %v", err)
		}
		if _, err := h.sess.SearchHistory(ctx, kernel.HistoryQuery{Words: []string{" "}}); !errors.Is(err, kernel.ErrEmptyQuery) {
			t.Fatalf("empty query: %v", err)
		}
		if err := h.sess.Clear(ctx); err != nil {
			t.Fatal(err)
		}
		if hits, _ := h.sess.SearchHistory(ctx, kernel.HistoryQuery{Words: []string{"apples"}}); len(hits) != 0 {
			t.Fatalf("nothing before a clear is found: %+v", hits)
		}
		h.close()
	})
}
