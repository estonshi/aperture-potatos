package kernel

import (
	"testing"

	"github.com/estonshi/aperture-potatos/message"
)

// TestStoredWindow: a resume takes the last line the session itself wrote, since a copy repeats an older value.
func TestStoredWindow(t *testing.T) {
	first, second := 100, 200
	lines := []message.Message{
		{ID: "a", ContextWindow: &first},
		{ID: "b", CopyOf: "a", ContextWindow: &first},
		{ID: "c", ContextWindow: &second},
		{ID: "d", CopyOf: "c", ContextWindow: &first}, // newer, but a copy of an older value
	}
	if w, ok := storedWindow(lines); !ok || w != 200 {
		t.Fatalf("stored window %d %v", w, ok)
	}
	zero := 0
	lines = append(lines, message.Message{ID: "e", ContextWindow: &zero})
	if w, ok := storedWindow(lines); !ok || w != 0 {
		t.Fatalf("a cleared window %d %v", w, ok)
	}
	if _, ok := storedWindow([]message.Message{{ID: "x"}}); ok {
		t.Fatal("a file that recorded no window")
	}
}
