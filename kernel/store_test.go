package kernel_test

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider/providertest"
	"github.com/estonshi/aperture-potatos/store"
)

func TestStoreStrictNeverRunsAnUnpersistedCall(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		echo := &echoTool{name: "echo"}
		st := &flakyStore{SessionStore: store.NewMemory(), fail: func(m message.Message) error {
			if m.Role == message.RoleAssistant {
				return errors.New("disk full")
			}
			return nil
		}}
		h := newHarness(t, [][]providertest.Step{toolReply(call{"toolu_1", "echo", `{}`})}, kernel.WithTool(echo), kernel.WithStore(st), kernel.WithStorePolicy(kernel.StoreStrict))
		rc, _ := h.sess.Send(kernel.Text("go"))
		res, _ := rc.Wait(t.Context())
		if res.Outcome != kernel.OutcomeError || echo.runsOf("toolu_1") != 0 {
			t.Fatalf("result %+v, runs %d", res, echo.runsOf("toolu_1"))
		}
		h.close()
		if h.rec.count(event.TypeError) == 0 {
			t.Fatal("no store_failed event")
		}
	})
}

func TestSeqConflictIsFatal(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st := &flakyStore{SessionStore: store.NewMemory(), fail: func(m message.Message) error {
			if m.Role == message.RoleUser {
				return store.ErrSeqConflict
			}
			return nil
		}}
		h := newHarness(t, [][]providertest.Step{textReply("never")}, kernel.WithStore(st))
		rc, _ := h.sess.Send(kernel.Text("go"))
		rc.Wait(t.Context())
		synctest.Wait()
		if _, err := h.sess.Send(kernel.Text("again")); !errors.Is(err, kernel.ErrSessionClosed) {
			t.Fatalf("send after the conflict: %v", err)
		}
		var fatal, closed bool
		for _, ev := range h.rec.all() {
			switch d := ev.Data.(type) {
			case *event.Error:
				fatal = fatal || d.Code == "store_failed" && d.Fatal
			case *event.SessionClosed:
				closed = d.Reason == "store_failed"
			}
		}
		if !fatal || !closed {
			t.Fatalf("fatal %v closed %v", fatal, closed)
		}
		h.close()
	})
}

// flakyStore fails the append of every line that matches.
type flakyStore struct {
	store.SessionStore
	fail func(m message.Message) error
}

func (f *flakyStore) Append(ctx context.Context, id string, msgs []message.Message) error {
	for _, m := range msgs {
		if err := f.fail(m); err != nil {
			return err
		}
	}
	return f.SessionStore.Append(ctx, id, msgs)
}
