package hookrun

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/estonshi/aperture-potatos/internal/workers"
)

func TestCall(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		wk := workers.New()
		v, f := Call(t.Context(), wk, "ok", time.Second, time.Second, func(context.Context) (int, error) { return 7, nil })
		if v != 7 || f != nil {
			t.Fatalf("ok: %d %v", v, f)
		}
		if _, f := Call(t.Context(), wk, "err", time.Second, time.Second, func(context.Context) (int, error) { return 0, errors.New("no") }); f == nil || f.Kind != KindError {
			t.Fatalf("error: %v", f)
		}
		if _, f := Call(t.Context(), wk, "panic", time.Second, time.Second, func(context.Context) (int, error) { panic("bug") }); f == nil || f.Kind != KindPanic {
			t.Fatalf("panic: %v", f)
		}
		start := time.Now()
		if _, f := Call(t.Context(), wk, "timeout", time.Second, time.Second, func(ctx context.Context) (int, error) { <-ctx.Done(); return 1, nil }); f == nil || f.Kind != KindTimeout || time.Since(start) != time.Second {
			t.Fatalf("an answer after the deadline counts as a timeout: %v after %v", f, time.Since(start))
		}
		ctx, cancel := context.WithCancel(t.Context())
		go func() { time.Sleep(time.Second); cancel() }()
		if _, f := Call(ctx, wk, "canceled", 0, time.Second, func(ctx context.Context) (int, error) { <-ctx.Done(); return 0, ctx.Err() }); f == nil || f.Kind != KindCanceled {
			t.Fatalf("canceled: %v", f)
		}
		start = time.Now()
		release := make(chan struct{})
		if _, f := Call(t.Context(), wk, "stuck", time.Second, 3*time.Second, func(context.Context) (int, error) { <-release; return 0, nil }); f == nil || f.Kind != KindTimeout || time.Since(start) != 4*time.Second {
			t.Fatalf("abandoned after the grace: %v after %v", f, time.Since(start))
		}
		if len(wk.Live()) != 1 {
			t.Fatal("the abandoned goroutine is still counted")
		}
		close(release)
		synctest.Wait()
		if len(wk.Live()) != 0 {
			t.Fatal("the late goroutine did not exit")
		}
	})
}

func TestPool(t *testing.T) {
	p := NewPool(1)
	if !p.TryAcquire() || p.TryAcquire() {
		t.Fatal("a pool of one holds one")
	}
	p.Release()
	if !p.TryAcquire() {
		t.Fatal("release frees the slot")
	}
	u := NewPool(-1)
	for range 100 {
		if !u.TryAcquire() {
			t.Fatal("an unbounded pool refused")
		}
	}
	u.Release()
}
