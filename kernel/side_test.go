package kernel_test

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/providertest"
)

func TestSideCallPreambleDropsCalls(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		h := newHarness(t, [][]providertest.Step{textReply("hi"), toolReply(call{"c9", "echo", `{}`})}, kernel.WithTool(&echoTool{name: "echo"}))
		r, _ := h.sess.Send(kernel.Input{Text: "hello"})
		_, _ = r.Wait(ctx)
		m, _, err := h.sess.SideCall(ctx, kernel.SideRequest{IncludeHistory: true, Messages: []message.Message{message.UserText("side question")}, Preamble: "Answer briefly and call no tool.", NoTools: true, Bucket: "side", Hop: event.Hop{Kind: "side", ID: "q1"}, Emit: true})
		if err != nil {
			t.Fatal(err)
		}
		if len(m.ToolCalls()) != 0 {
			t.Fatalf("NoTools drops returned calls: %+v", m.Content)
		}
		req := h.sp.Requests()[1]
		last := req.Messages[len(req.Messages)-1]
		if rem, ok := last.Content[0].(*message.ReminderBlock); !ok || rem.Source != "side" || last.Text() != "side question" || len(req.Tools) != 1 || req.CacheTail != 2 {
			t.Fatalf("preamble first in the appended question, tools kept, cache tail at the prefix: %+v tail %d", last, req.CacheTail)
		}
		synctest.Wait()
		var hop bool
		for _, ev := range h.rec.all() {
			if ev.Type == event.TypeToolCallCompleted && len(ev.Path) == 1 && ev.Path[0].ID == "q1" {
				hop = true
			}
		}
		if !hop {
			t.Fatal("Emit publishes the side call's stream under its hop")
		}
		if h.sess.Snapshot().Usage.Buckets["side"].InputTokens == 0 {
			t.Fatal("booked in its bucket")
		}
		h.close()
	})
}

func TestSideCallRetries(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		failed := []providertest.Step{{Err: &provider.Error{Class: provider.ClassTransient, Status: 503, Message: "busy"}}}
		h := newHarness(t, [][]providertest.Step{failed, textReply("answer")},
			kernel.WithRetryPolicy(provider.RetryPolicy{MaxAttempts: 2, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}))
		m, _, err := h.sess.SideCall(ctx, kernel.SideRequest{Messages: []message.Message{message.UserText("q")}})
		if err != nil || m.Text() != "answer" {
			t.Fatalf("%v %+v", err, m)
		}
		if b := h.sess.Snapshot().Usage.Buckets["side"]; b.InputTokens == 0 {
			t.Fatal("the default bucket is side")
		}
		h.close()
	})
}
