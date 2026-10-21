package bus

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/estonshi/aperture-potatos/event"
)

func envelope(p event.Payload) *event.Event { return &event.Event{Type: p.EventType(), Data: p} }

func delta() *event.Event { return envelope(&event.MessageDelta{Text: "x"}) }

func notice() *event.Event { return envelope(&event.Notice{Source: "t", Text: "x"}) }

func opts(p Policy) Options {
	return Options{Policy: p, HighWater: 4, HardLimit: 16, StallTimeout: time.Minute, MaxDepth: -1}
}

func drain(t *testing.T, b *Bus, s *Sub) []*event.Event {
	t.Helper()
	var out []*event.Event
	for {
		s.mu.Lock()
		n := len(s.q)
		s.mu.Unlock()
		if n == 0 {
			return out
		}
		ev, _ := s.Pop(t.Context(), b)
		out = append(out, ev)
	}
}

func TestPublishOrderAndFilters(t *testing.T) {
	b := New(envelope)
	all := b.Subscribe(opts(Block))
	msgs := b.Subscribe(Options{Policy: Block, HighWater: 4, HardLimit: 16, Types: []string{"message."}, MaxDepth: -1})
	own := b.Subscribe(Options{Policy: Block, HighWater: 4, HardLimit: 16, MaxDepth: 0})
	b.Publish(notice())
	b.Publish(delta())
	nested := delta()
	nested.Path = []event.Hop{{Kind: "task", ID: "task_1"}}
	b.Publish(nested)
	got := drain(t, b, all)
	if len(got) != 3 || got[0].Seq != 1 || got[1].Seq != 2 || got[2].Seq != 3 {
		t.Fatalf("all: %+v", got)
	}
	if got := drain(t, b, msgs); len(got) != 2 || got[0].Seq != 2 {
		t.Fatalf("types filter: %d events", len(got))
	}
	if got := drain(t, b, own); len(got) != 2 || got[1].Seq != 2 {
		t.Fatalf("depth filter: %d events", len(got))
	}
}

func TestNativeVariantSharesSeq(t *testing.T) {
	b := New(envelope)
	plain := b.Subscribe(opts(Block))
	o := opts(Block)
	o.Native = true
	native := b.Subscribe(o)
	ev, nat := notice(), envelope(&event.Notice{Source: "native", Text: "x"})
	b.PublishNative(ev, nat)
	p, n := drain(t, b, plain), drain(t, b, native)
	if p[0].Data.(*event.Notice).Source != "t" || n[0].Data.(*event.Notice).Source != "native" || p[0].Seq != n[0].Seq {
		t.Fatalf("plain %+v native %+v", p[0], n[0])
	}
}

func TestBlockBackpressureReturnsCredit(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := New(envelope)
		s := b.Subscribe(opts(Block))
		for range 4 {
			b.Publish(delta())
		}
		done := make(chan error, 1)
		go func() { done <- b.WaitWritable(t.Context()) }()
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("producer did not wait on a full subscriber")
		default:
		}
		b.Publish(notice()) // non-bulk events are never held back
		s.Pop(t.Context(), b)
		s.Pop(t.Context(), b)
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("credit returned above the low water mark")
		default:
		}
		s.Pop(t.Context(), b)
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}

func TestDropDeltasDropsOnlyBulk(t *testing.T) {
	b := New(envelope)
	s := b.Subscribe(opts(DropDeltas))
	for range 6 {
		b.Publish(delta())
	}
	b.Publish(notice())
	if err := b.WaitWritable(t.Context()); err != nil {
		t.Fatal(err) // a lossy subscriber never holds producers back
	}
	got := drain(t, b, s)
	if len(got) != 5 || s.Dropped() != 2 || got[4].Seq != 7 {
		t.Fatalf("got %d events, dropped %d", len(got), s.Dropped())
	}
}

func TestHardLimitDetachesAndAnnounces(t *testing.T) {
	b := New(envelope)
	victim := b.Subscribe(Options{Policy: Block, HighWater: 2, HardLimit: 3, StallTimeout: -1, MaxDepth: -1})
	other := b.Subscribe(Options{Policy: Block, HighWater: 100, HardLimit: 100, StallTimeout: -1, MaxDepth: -1})
	for range 4 {
		b.Publish(notice())
	}
	if !errors.Is(victim.Err(), ErrOverflow) {
		t.Fatalf("victim err = %v", victim.Err())
	}
	if _, ok := victim.Pop(t.Context(), b); ok {
		t.Fatal("a detached subscriber still delivers")
	}
	got := drain(t, b, other)
	last := got[len(got)-1].Data.(*event.Error)
	if last.Code != "subscriber_detached" {
		t.Fatalf("last event %+v", last)
	}
}

func TestStallTimeoutDetachesAndWakesProducer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := New(envelope)
		s := b.Subscribe(opts(Block))
		for range 4 {
			b.Publish(delta())
		}
		start := time.Now()
		if err := b.WaitWritable(t.Context()); err != nil {
			t.Fatal(err)
		}
		if waited := time.Since(start); waited != time.Minute {
			t.Fatalf("waited %v, want the stall timeout", waited)
		}
		if !errors.Is(s.Err(), ErrStalled) {
			t.Fatalf("err = %v", s.Err())
		}
	})
}

func TestIdleThenBurstIsNotStalled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := New(envelope)
		s := b.Subscribe(opts(Block))
		time.Sleep(2 * time.Minute) // longer than the stall timeout without a pop
		for range 4 {
			b.Publish(delta())
		}
		go func() {
			time.Sleep(30 * time.Second) // slow but within the timeout, counted from the wait
			for range 3 {
				s.Pop(t.Context(), b)
			}
		}()
		if err := b.WaitWritable(t.Context()); err != nil {
			t.Fatal(err)
		}
		if s.Err() != nil {
			t.Fatalf("healthy subscriber detached: %v", s.Err())
		}
	})
}

func TestWaitWritableHonorsContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := New(envelope)
		b.Subscribe(Options{Policy: Block, HighWater: 1, HardLimit: 8, StallTimeout: -1, MaxDepth: -1})
		b.Publish(delta())
		ctx, cancel := context.WithCancel(t.Context())
		go func() { time.Sleep(time.Second); cancel() }()
		if err := b.WaitWritable(ctx); !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	})
}

func TestCloseDrainsThenEnds(t *testing.T) {
	b := New(envelope)
	s := b.Subscribe(opts(Block))
	b.Publish(notice())
	b.Publish(notice())
	b.Close()
	b.Publish(notice()) // a late publish from an abandoned goroutine is a no-op
	if err := b.WaitWritable(t.Context()); !errors.Is(err, ErrClosed) {
		t.Fatalf("WaitWritable after close: %v", err)
	}
	var n int
	for {
		if _, ok := s.Pop(t.Context(), b); !ok {
			break
		}
		n++
	}
	if n != 2 || !errors.Is(s.Err(), ErrClosed) {
		t.Fatalf("drained %d, err %v", n, s.Err())
	}
}

func TestPumpChanCloseOnDone(t *testing.T) {
	for name, end := range map[string]func(*Bus, *Sub){
		"unsubscribe": func(b *Bus, s *Sub) { b.Unsubscribe(s) },
		"bus close":   func(b *Bus, s *Sub) { b.Close() },
		"detach":      func(b *Bus, s *Sub) { b.Detach(s, ErrStalled) },
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				b := New(envelope)
				s := b.Subscribe(opts(Block))
				ch := make(chan event.Event, 8)
				go PumpChan(t.Context(), b, s, ch, true)
				b.Publish(notice())
				synctest.Wait()
				end(b, s)
				<-s.Done()
				n := 0
				for range ch {
					n++
				}
				if n != 1 {
					t.Fatalf("received %d events before close", n)
				}
			})
		})
	}
}

func TestPumpChanUnblocksOnUnsubscribe(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := New(envelope)
		s := b.Subscribe(opts(Block))
		ch := make(chan event.Event) // never read
		go PumpChan(t.Context(), b, s, ch, false)
		b.Publish(notice())
		synctest.Wait()
		b.Unsubscribe(s) // must not block, and must free the pump stuck on the send
		<-s.Done()
		if s.Err() != nil {
			t.Fatalf("err = %v", s.Err())
		}
	})
}

func TestPumpFuncHandlerErrorAndPanic(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		b := New(envelope)
		boom := errors.New("boom")
		failing := b.Subscribe(opts(Block))
		go PumpFunc(t.Context(), b, failing, func(context.Context, event.Event) error { return boom })
		panicking := b.Subscribe(opts(Block))
		go PumpFunc(t.Context(), b, panicking, func(context.Context, event.Event) error { panic("bad handler") })
		b.Publish(notice())
		<-failing.Done()
		<-panicking.Done()
		if !errors.Is(failing.Err(), boom) || panicking.Err() == nil {
			t.Fatalf("errs %v / %v", failing.Err(), panicking.Err())
		}
	})
}
