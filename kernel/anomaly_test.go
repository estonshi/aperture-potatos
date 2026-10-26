package kernel_test

import (
	"strings"
	"testing"
	"testing/synctest"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/provider/providertest"
)

// TestModelAnomalyIsAdvisory: a reply that repeats a line raises model.anomaly once, and the turn completes with the whole text committed.
func TestModelAnomalyIsAdvisory(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		text := strings.Repeat("this line repeats\n", 6)
		h := newHarness(t, [][]providertest.Step{textReply(text)})
		res := ask(t, h.sess, "go")
		if res.Outcome != kernel.OutcomeCompleted {
			t.Fatalf("outcome %q, want completed: the event must not change the turn", res.Outcome)
		}
		var got []*event.ModelAnomaly
		for _, ev := range h.rec.all() {
			if a, ok := ev.Data.(*event.ModelAnomaly); ok {
				got = append(got, a)
			}
		}
		if len(got) != 1 {
			t.Fatalf("%d model.anomaly events, want 1", len(got))
		}
		a := got[0]
		if a.Kind != "repetition" || a.Channel != "text" || a.Detail == nil || a.Detail.Mode != "line" {
			t.Fatalf("anomaly %+v detail %+v", a, a.Detail)
		}
		if a.Offset != len([]rune(text)) {
			t.Fatalf("offset %d, want %d", a.Offset, len([]rune(text)))
		}
		lines := h.lines()
		if last := lines[len(lines)-1]; last.Text() != text {
			t.Fatalf("committed text changed: %q", last.Text())
		}
		h.close()
	})
}

// TestModelAnomalyDisabled: Disabled turns the detector off, so the same reply raises nothing.
func TestModelAnomalyDisabled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, [][]providertest.Step{textReply(strings.Repeat("this line repeats\n", 6))},
			kernel.WithOutputAnomalyDetection(kernel.AnomalyConfig{Disabled: true}))
		ask(t, h.sess, "go")
		for _, ev := range h.rec.all() {
			if _, ok := ev.Data.(*event.ModelAnomaly); ok {
				t.Fatal("a disabled detector published an event")
			}
		}
		h.close()
	})
}

// TestModelAnomalyNotBulk: DropDeltas may skip deltas, never the advisory event.
func TestModelAnomalyNotBulk(t *testing.T) {
	if _, ok := any(&event.ModelAnomaly{}).(event.Bulk); ok {
		t.Fatal("model.anomaly is bulk, so a subscriber that drops deltas would lose it")
	}
}
