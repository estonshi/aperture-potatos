package bus

import (
	"context"
	"fmt"
	"runtime"

	"github.com/estonshi/aperture-potatos/event"
)

// PumpChan delivers to a caller-owned channel until the subscription ends; closing the channel early is misuse that only detaches instead of crashing.
func PumpChan(ctx context.Context, b *Bus, s *Sub, ch chan<- event.Event, closeOnDone bool) {
	defer s.Finish()
	defer func() {
		ownerClosed := false
		if r := recover(); r != nil {
			if !isClosedChannelPanic(r) {
				panic(r)
			}
			ownerClosed = true
			b.Detach(s, ErrChannelClosed)
		}
		if closeOnDone && !ownerClosed { // every exit path closes ch, so a range loop over it always ends
			close(ch)
		}
	}()
	for {
		ev, ok := s.Pop(ctx, b)
		if !ok {
			return
		}
		select {
		case ch <- *ev:
		case <-s.quit:
			return
		case <-ctx.Done():
			return
		}
	}
}

func isClosedChannelPanic(r any) bool {
	err, ok := r.(runtime.Error)
	return ok && err.Error() == "send on closed channel"
}

// PumpFunc calls fn for each event on the pump goroutine; ctx is what fn receives, so the caller puts its re-entrancy marker there.
func PumpFunc(ctx context.Context, b *Bus, s *Sub, fn func(context.Context, event.Event) error) {
	defer s.Finish()
	for {
		ev, ok := s.Pop(ctx, b)
		if !ok {
			return
		}
		if err := callHandler(ctx, fn, *ev); err != nil {
			b.Detach(s, err) // a handler error ends this subscription only
			return
		}
	}
}

func callHandler(ctx context.Context, fn func(context.Context, event.Event) error, ev event.Event) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("event handler panic: %v", r)
		}
	}()
	return fn(ctx, ev)
}
