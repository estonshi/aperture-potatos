package kernel_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/providertest"
	"github.com/estonshi/aperture-potatos/store"
)

func TestCompactManualAndAuto(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		comp := &keepTurnCompactor{}
		big := message.Usage{InputTokens: 5000, OutputTokens: 10}
		reply := func(text string, u message.Usage) []providertest.Step {
			s := textReply(text)
			s[len(s)-1].Event.Usage = &u
			s[len(s)-1].Event.Message.Usage = &u
			return s
		}
		h := newHarness(t, [][]providertest.Step{
			reply("one", usage),     // turn 1
			textReply("Summary A."), // manual compaction
			reply("two", big),       // turn 2, which reports a context past the threshold
			textReply("Summary B."), // auto compaction before turn 3's first request
			reply("three", usage),   // turn 3
		},
			kernel.Use(kernel.ExtensionFunc("comp", func(r kernel.Registrar) error { _, err := r.Compactor(comp); return err })),
			kernel.WithContextPolicy(kernel.ContextPolicy{MaxContextTokens: 64000 + 3000, MinSummaryTokens: 10, MaxSummaryTokens: 50}),
		)
		send := func(text string) kernel.TurnResult {
			r, err := h.sess.Send(kernel.Input{Text: text})
			if err != nil {
				t.Fatal(err)
			}
			res, err := r.Wait(ctx)
			if err != nil {
				t.Fatal(err)
			}
			return res
		}
		send("first")
		rep, err := h.sess.Compact(ctx, "focus on names")
		if err != nil || rep.Epoch != 2 || rep.Strategy != "test" {
			t.Fatalf("manual: %v %+v", err, rep)
		}
		send("second")
		send("third")
		if comp.calls != 2 {
			t.Fatalf("compactions: %d", comp.calls)
		}
		lines := h.lines()
		cur := store.Current(lines)
		if cur[0].Boundary.Reason != "compaction" || cur[0].Boundary.Trigger != "auto" || cur[0].Epoch != 3 {
			t.Fatalf("current epoch: %+v", cur[0].Boundary)
		}
		if roles(cur) != "system,user,user,assistant" {
			t.Fatalf("epoch 3 is system, head, the kept turn input (which needs no continue reminder), reply: %s", roles(cur))
		}
		head := cur[1].Content[0].(*message.ReminderBlock)
		if cur[1].Origin.Kind != message.OriginEpochHead || head.Source != "compaction" || head.Text != "Summary B." {
			t.Fatalf("head: %+v", cur[1])
		}
		if cur[2].CopyOf == "" || cur[2].Origin.Kind != message.OriginHuman {
			t.Fatalf("the kept input is a copy: %+v", cur[2])
		}
		reqs := h.sp.Requests()
		sum := reqs[1]
		if n := len(sum.Messages); sum.CacheTail != n-1 || sum.Messages[n-1].Text() != "Summarize the conversation." {
			t.Fatalf("the summary request appends its instruction to the cached prefix: tail %d of %d", sum.CacheTail, n)
		}
		if b := h.sess.Snapshot().Usage.Buckets["compaction"]; b.InputTokens == 0 {
			t.Fatalf("summary calls are booked in the compaction bucket: %+v", h.sess.Snapshot().Usage.Buckets)
		}
		h.close()
	})
}

func TestHistoryOpsInlineAndReentrant(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		wipe := &clearTool{}
		h := newHarness(t, [][]providertest.Step{toolReply(call{"c1", "wipe", `{}`}), textReply("ok")},
			kernel.WithTool(wipe),
			kernel.WithCommand(kernel.Command{Name: "fresh", Busy: kernel.WaitIdle, Run: func(ctx context.Context, cc *kernel.CommandContext) (kernel.CommandResult, error) {
				return kernel.CommandResult{Text: "cleared"}, cc.Session().Clear(ctx) // inline: this runner holds the history token
			}}))
		r, _ := h.sess.Send(kernel.Input{Text: "wipe it"})
		_, _ = r.Wait(ctx)
		if len(wipe.errs) != 2 || !errors.Is(wipe.errs[0], kernel.ErrReentrantWait) || !errors.Is(wipe.errs[1], kernel.ErrReentrantWait) {
			t.Fatalf("history primitives from a tool: %v", wipe.errs)
		}
		c, _ := h.sess.Command("fresh", "")
		res, err := c.Wait(ctx)
		if err != nil || res.Err != nil || h.sess.Snapshot().Epoch != 2 {
			t.Fatalf("clear inline from a WaitIdle command: %v %v epoch %d", err, res.Err, h.sess.Snapshot().Epoch)
		}
		if err := h.sess.Clear(ctx); err != nil || h.sess.Snapshot().Epoch != 3 {
			t.Fatalf("clear from the host queues and runs: %v", err)
		}
		if _, err := h.sess.Compact(ctx, ""); !errors.Is(err, kernel.ErrNoCompactor) {
			t.Fatalf("no compactor: %v", err)
		}
		if err := h.sess.Unpin(ctx, "none"); !errors.Is(err, kernel.ErrUnknownPin) {
			t.Fatalf("unknown pin: %v", err)
		}
		h.close()
	})
}

func TestTokenCounterDecidesNearThreshold(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		near := message.Usage{InputTokens: 8600, OutputTokens: 10} // threshold 9000, within the 10% band
		first := textReply("one")
		first[len(first)-1].Event.Usage = &near
		counter := &fixedCounter{n: 20000}
		h := newHarness(t, [][]providertest.Step{first, textReply("two")},
			kernel.Use(kernel.ExtensionFunc("comp", func(r kernel.Registrar) error { _, err := r.Compactor(stubCompactor{}); return err })),
			kernel.WithContextPolicy(kernel.ContextPolicy{MaxContextTokens: 64000 + 10000}),
			kernel.WithTokenCounter(counter))
		for _, text := range []string{"first", "second"} {
			r, _ := h.sess.Send(kernel.Input{Text: text})
			if _, err := r.Wait(ctx); err != nil {
				t.Fatal(err)
			}
		}
		if counter.calls != 1 || h.sess.Snapshot().Epoch != 2 {
			t.Fatalf("the exact count, above the threshold, compacts: calls %d epoch %d", counter.calls, h.sess.Snapshot().Epoch)
		}
		if len(h.sess.Pins()) != 0 {
			t.Fatalf("no pin was declared: %+v", h.sess.Pins())
		}
		h.close()
	})
}

// clearTool calls a history primitive from inside a turn, which must refuse instead of deadlocking.
type clearTool struct{ errs []error }

func (c *clearTool) Spec() kernel.ToolSpec {
	return kernel.ToolSpec{Name: "wipe", Description: "Wipe.", Concurrency: kernel.Serial}
}

func (c *clearTool) Call(ctx context.Context, tc *kernel.ToolContext, _ json.RawMessage) (kernel.ToolResult, error) {
	c.errs = append(c.errs, tc.Session().Clear(ctx))
	_, err := tc.Session().Compact(ctx, "")
	c.errs = append(c.errs, err)
	return kernel.TextResult("done"), nil
}

type fixedCounter struct{ n, calls int64 }

func (c *fixedCounter) CountTokens(context.Context, *provider.Request) (int64, error) {
	c.calls++
	return c.n, nil
}

// TestClearDropsInterruptedTail: what an interrupted turn leaves for the next one, its note and its turn_end context, speaks of the context a clear drops, so it goes with it.
func TestClearDropsInterruptedTail(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		held := []providertest.Step{
			{Event: provider.StreamEvent{Kind: provider.SEStart}},
			{Event: provider.StreamEvent{Kind: provider.SEBlockStart, Index: 0, BlockType: message.TypeText}},
			{Event: provider.StreamEvent{Kind: provider.SETextDelta, Index: 0, Channel: "text", Text: "half"}},
			{Hold: -1},
		}
		h := newHarness(t, [][]providertest.Step{held, textReply("fresh")},
			kernel.WithHook(kernel.TurnEnd, "audit", func(context.Context, kernel.TurnEndInput) (kernel.TurnEndOutput, error) {
				return kernel.TurnEndOutput{Common: kernel.Common{AdditionalContext: "the last turn ran long"}}, nil
			}))
		rc, _ := h.sess.Send(kernel.Text("go"))
		synctest.Wait()
		cleared := make(chan error, 1)
		go func() { cleared <- h.sess.Clear(t.Context()) }() // queued behind the running turn, so the turn's tail arrives after it
		synctest.Wait()
		h.sess.Interrupt("")
		_, _ = rc.Wait(t.Context())
		if err := <-cleared; err != nil {
			t.Fatal(err)
		}
		rc, _ = h.sess.Send(kernel.Text("again"))
		_, _ = rc.Wait(t.Context())
		reqs := h.sp.Requests()
		for _, m := range reqs[len(reqs)-1].Messages {
			for _, b := range m.Content {
				if r, ok := b.(*message.ReminderBlock); ok {
					t.Fatalf("the cleared context's tail reached the new one: %s %q", r.Source, r.Text)
				}
			}
		}
		h.close()
	})
}

// TestAsyncAnswerAfterRealClearIsDropped: an async hook that answers after a clear speaks of the context the clear dropped, so its answer never reaches the queue.
func TestAsyncAnswerAfterRealClearIsDropped(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, [][]providertest.Step{textReply("one"), textReply("two")},
			kernel.WithAsyncHook(kernel.TurnEnd, "late", func(context.Context, kernel.TurnEndInput) (kernel.Injection, error) {
				time.Sleep(time.Second)
				return kernel.Injection{AdditionalContext: "stale", Wake: true}, nil
			}))
		rc, _ := h.sess.Send(kernel.Text("hi"))
		_, _ = rc.Wait(t.Context())
		if err := h.sess.Clear(t.Context()); err != nil {
			t.Fatal(err)
		}
		time.Sleep(2 * time.Second)
		synctest.Wait()
		if n := h.rec.count(event.TypeInputAccepted); n != 1 {
			t.Fatalf("%d inputs accepted; the stale answer must be dropped", n)
		}
		h.close()
	})
}
