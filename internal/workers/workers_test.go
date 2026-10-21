package workers

import (
	"context"
	"testing"
	"testing/synctest"
	"time"
)

func TestWaitReturnsOnceEmpty(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := New()
		release := make(chan struct{})
		s.Go("turn t1", func() { <-release })
		s.Go("cmd c1", func() { <-release })
		if n := len(s.Live()); n != 2 {
			t.Fatalf("live = %d, want 2", n)
		}
		close(release)
		if left := s.Wait(t.Context()); left != nil {
			t.Fatalf("left = %v", left)
		}
	})
}

func TestWaitReportsStragglers(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := New()
		stop := make(chan struct{})
		s.Go("tool x", func() { <-stop })
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)
		defer cancel()
		left := s.Wait(ctx)
		if len(left) != 1 || left[0].Name != "tool x" {
			t.Fatalf("left = %v", left)
		}
		close(stop)
		s.Wait(t.Context())
	})
}

func TestWaitPrefixAndExcept(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		s := New()
		turn, other := make(chan struct{}), make(chan struct{})
		s.Go("turn t1", func() { <-turn })
		s.Go("closer", func() { <-other })
		done := make(chan []Info, 1)
		go func() { done <- s.WaitPrefix(t.Context(), "turn ", "cmd ") }()
		synctest.Wait()
		select {
		case <-done:
			t.Fatal("WaitPrefix returned while a turn was live")
		default:
		}
		close(turn)
		if left := <-done; left != nil {
			t.Fatalf("left = %v", left)
		}
		if left := s.WaitExcept(t.Context(), "closer"); left != nil {
			t.Fatalf("WaitExcept left = %v", left)
		}
		close(other)
		s.Wait(t.Context())
	})
}
