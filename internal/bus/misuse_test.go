//go:build !race

package bus

import (
	"errors"
	"testing"
	"testing/synctest"

	"github.com/estonshi/aperture-potatos/event"
)

// The owner closing its channel races with the pump by definition, so this case runs without the race detector and only checks that nothing crashes.
func TestOwnerClosedChannelDetaches(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := New(envelope)
		s := b.Subscribe(opts(Block))
		other := b.Subscribe(opts(Block))
		ch := make(chan event.Event)
		close(ch)
		go PumpChan(t.Context(), b, s, ch, true)
		b.Publish(notice())
		<-s.Done()
		if !errors.Is(s.Err(), ErrChannelClosed) {
			t.Fatalf("err = %v", s.Err())
		}
		got := drain(t, b, other)
		if got[len(got)-1].Data.(*event.Error).Code != "subscriber_detached" {
			t.Fatal("others were not told")
		}
	})
}
