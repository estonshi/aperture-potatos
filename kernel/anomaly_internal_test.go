package kernel

import (
	"strings"
	"testing"
)

// TestAnomalyModes: the three checks find their repetition, report the unit and the count, and leave ordinary prose alone.
func TestAnomalyModes(t *testing.T) {
	for _, c := range []struct {
		name string
		text string
		mode string // "" when nothing may be reported
		unit int
		reps int
	}{
		{name: "suffix bytes", text: "the answer is " + strings.Repeat("abc", 10), mode: "suffix", unit: 3, reps: 10},
		{name: "suffix rune", text: "it is funny " + strings.Repeat("哈", 20), mode: "suffix", unit: 1, reps: 20},
		{name: "line", text: "intro\n" + strings.Repeat("this line repeats\n", 6), mode: "line", unit: len("this line repeats"), reps: 6},
		{name: "block", text: strings.Repeat(strings.Repeat("x", 1023)+"\n", 3), mode: "block", unit: 1024, reps: 2},
		{name: "prose", text: "The quick brown fox jumps over the lazy dog.\nSecond line of ordinary text.\n"},
		{name: "code", text: "func main() {\n\tif x {\n\t\ty()\n\t}\n}\n"},
		{name: "blank lines", text: "text\n" + strings.Repeat("\n", 20)},
	} {
		t.Run(c.name, func(t *testing.T) {
			scan := newAnomalyScan(anomalyDefaults)
			d := scan.write(c.text)
			if d == nil {
				d = scan.final()
			}
			if c.mode == "" {
				if d != nil {
					t.Fatalf("false positive: %+v", d)
				}
				return
			}
			if d == nil || d.Mode != c.mode || d.UnitChars != c.unit || d.Repetitions < c.reps {
				t.Fatalf("got %+v, want %s unit %d at least %d times", d, c.mode, c.unit, c.reps)
			}
			if d.Sample == "" || !strings.HasSuffix(c.text, d.Sample) {
				t.Fatalf("sample %q is not the tail of the text", d.Sample)
			}
		})
	}
}

// TestAnomalyOncePerMode: a block reports each mode once, however often the text repeats afterwards.
func TestAnomalyOncePerMode(t *testing.T) {
	scan := newAnomalyScan(anomalyDefaults)
	text := "start\n" + strings.Repeat("this line repeats\n", 6)
	scan.write(text)
	if d := scan.final(); d == nil || d.Mode != "line" {
		t.Fatalf("first: %+v", d)
	}
	for range 3 {
		scan.write(text)
		if d := scan.final(); d != nil && d.Mode == "line" {
			t.Fatalf("line reported twice: %+v", d)
		}
	}
}

// TestAnomalyCheckInterval: a delta shorter than CheckEveryBytes waits for the block's end, so short replies are still checked once.
func TestAnomalyCheckInterval(t *testing.T) {
	scan := newAnomalyScan(anomalyDefaults.resolved())
	text := strings.Repeat("this line repeats\n", 6)
	if d := scan.write(text); d != nil {
		t.Fatalf("scanned before the interval: %+v", d)
	}
	if d := scan.final(); d == nil || d.Mode != "line" {
		t.Fatalf("final: %+v", d)
	}
	if scan.chars != len([]rune(text)) {
		t.Fatalf("offset %d, want %d characters", scan.chars, len([]rune(text)))
	}
}

// TestAnomalySampleLimit: the sample keeps the tail and cut at a rune boundary.
func TestAnomalySampleLimit(t *testing.T) {
	scan := newAnomalyScan(anomalyDefaults)
	scan.tail = []byte(strings.Repeat("好", 300))
	got := []rune(scan.sample())
	if len(got) != 256 || got[0] != '好' {
		t.Fatalf("sample has %d characters", len(got))
	}
}
