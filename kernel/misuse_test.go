//go:build !race

package kernel_test

import (
	"errors"
	"testing"
	"testing/synctest"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/provider/providertest"
)

// Closing a subscribed channel races with its pump by definition, so this runs without the race detector and checks only that nothing crashes (I8).
func TestOwnerClosesChannelEarly(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, [][]providertest.Step{textReply("pong")})
		ch := make(chan event.Event)
		close(ch)
		sub, _ := h.sess.Subscribe(ch)
		rc, _ := h.sess.Send(kernel.Text("ping"))
		if res, _ := rc.Wait(t.Context()); res.Outcome != kernel.OutcomeCompleted {
			t.Fatalf("result %+v", res)
		}
		<-sub.Done()
		if !errors.Is(sub.Err(), kernel.ErrChannelClosed) {
			t.Fatalf("err %v", sub.Err())
		}
		h.close()
	})
}
