package kernel

import (
	"regexp"
	"testing"
	"time"
)

func TestRandomIDs(t *testing.T) {
	g := RandomIDs()
	re := regexp.MustCompile(`^msg_[0-9a-hjkmnp-tv-z]{26}$`)
	seen := map[string]bool{}
	var prev string
	for range 1000 {
		id := g.NewID("msg")
		if !re.MatchString(id) || seen[id] {
			t.Fatalf("bad or repeated id %q", id)
		}
		seen[id] = true
		if prev != "" && id[:14] < prev[:14] {
			t.Fatalf("timestamp part went backwards: %s after %s", id, prev)
		}
		prev = id
	}
}

func TestSystemClockIsUTC(t *testing.T) {
	if SystemClock().Now().Location() != time.UTC {
		t.Fatal("wire times are UTC")
	}
}
