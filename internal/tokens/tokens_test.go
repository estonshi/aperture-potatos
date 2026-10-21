package tokens

import (
	"testing"

	"github.com/estonshi/aperture-potatos/message"
)

func TestText(t *testing.T) {
	if got := Text("abcdefgh"); got != 2 {
		t.Fatalf("ascii at four bytes a token: %d", got)
	}
	if got := Text("日本語"); got != 3 {
		t.Fatalf("one token per other rune: %d", got)
	}
}

func TestEstimateAnchorsAndClamps(t *testing.T) {
	lines := []message.Message{message.UserText("abcdefgh"), message.UserText("abcdefgh")}
	var e Estimate
	if n, anchored := e.Next(lines, 100); anchored || n != 100+Lines(lines) {
		t.Fatalf("no anchor: the whole request at ratio 1: %d", n)
	}
	e.Anchor(1000, 1, 10) // a report 100 times the heuristic is clamped
	if e.Ratio != maxRatio {
		t.Fatalf("ratio %v", e.Ratio)
	}
	if n, anchored := e.Next(lines, 100); !anchored || n != 1000+int64(maxRatio*float64(Message(lines[1]))) {
		t.Fatalf("anchor plus the scaled tail: %d", n)
	}
	e.Anchor(10, 2, 1000)
	if e.Ratio != (maxRatio+minRatio)/2 {
		t.Fatalf("the mean of the old ratio and the clamped new one: %v", e.Ratio)
	}
	e.Reset()
	if e.AnchorLen != 0 || e.Scale(10) != int64(e.Ratio*10) {
		t.Fatal("reset keeps the ratio and drops the anchor")
	}
}

func TestBlocks(t *testing.T) {
	m := message.Message{Content: message.Content{
		&message.ToolCallBlock{Name: "echo", Input: []byte(`{"a":1}`)},
		&message.ToolResultBlock{Content: message.Content{&message.TextBlock{Text: "abcd"}}},
		&message.ImageBlock{},
		&message.ReasoningBlock{Native: &message.Native{Raw: []byte(`"abcdefgh"`)}},
		&message.ReminderBlock{Text: "abcd"},
	}}
	if got := Message(m); got < perMedia {
		t.Fatalf("an image counts as media: %d", got)
	}
	if Tools([]message.ToolDef{{Name: "x"}}) == 0 {
		t.Fatal("tools count")
	}
}
