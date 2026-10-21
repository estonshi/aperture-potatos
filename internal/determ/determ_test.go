package determ

import (
	"testing"
	"time"

	"github.com/estonshi/aperture-potatos/kernel"
)

var (
	_ kernel.Clock       = (*StepClock)(nil)
	_ kernel.IDGenerator = (*SequentialIDs)(nil)
)

func TestStepClock(t *testing.T) {
	start := time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)
	c := NewStepClock(start, time.Millisecond)
	if a, b := c.Now(), c.Now(); !a.Equal(start) || b.Sub(a) != time.Millisecond {
		t.Fatalf("got %v then %v", a, b)
	}
}

func TestSequentialIDs(t *testing.T) {
	g := NewSequentialIDs()
	got := []string{g.NewID("msg"), g.NewID("ses"), g.NewID("msg")}
	want := []string{"msg_01", "ses_01", "msg_02"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
}
