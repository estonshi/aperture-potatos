// Package tokens estimates prompt size between provider reports: it anchors on the last reported usage and scales a byte heuristic for what came after.
package tokens

import (
	"encoding/json"
	"unicode/utf8"

	"github.com/estonshi/aperture-potatos/message"
)

// Per-item weights of the heuristic; an image or document costs about the same on every API.
const (
	perMessage = 4
	perMedia   = 1600
)

// The calibration ratio stays within these bounds, so one odd report never scales every later estimate far off.
const (
	minRatio = 0.5
	maxRatio = 2.0
)

// Text counts ASCII at four bytes a token and every other rune as one token.
func Text(s string) int64 {
	var ascii, other int64
	for _, r := range s {
		if r < utf8.RuneSelf {
			ascii++
		} else {
			other++
		}
	}
	return (ascii+3)/4 + other
}

func raw(b []byte) int64 { return int64(len(b)+3) / 4 }

// Message estimates one line as a request would carry it.
func Message(m message.Message) int64 {
	n := int64(perMessage)
	for _, b := range m.Content {
		n += Block(b)
	}
	return n
}

// Block estimates one block; provider data kept for replay counts too, because it is sent back.
func Block(b message.Block) int64 {
	switch v := b.(type) {
	case *message.TextBlock:
		return Text(v.Text)
	case *message.RefusalBlock:
		return Text(v.Text)
	case *message.ReminderBlock:
		return Text(v.Text) + 8
	case *message.ReasoningBlock:
		n := Text(v.Text)
		if v.Native != nil && v.Text == "" {
			n += raw(v.Native.Raw)
		}
		return n
	case *message.ToolCallBlock:
		return Text(v.Name) + raw(v.Input)
	case *message.ToolResultBlock:
		n := int64(8)
		for _, c := range v.Content {
			n += Block(c)
		}
		return n
	case *message.ImageBlock, *message.DocumentBlock:
		return perMedia
	case *message.OpaqueBlock:
		if v.Native != nil {
			return raw(v.Native.Raw)
		}
		return Text(v.Text)
	}
	b2, _ := json.Marshal(b)
	return raw(b2)
}

// Lines sums Message over lines.
func Lines(lines []message.Message) int64 {
	var n int64
	for _, m := range lines {
		n += Message(m)
	}
	return n
}

// Tools estimates the tool definitions a request carries.
func Tools(defs []message.ToolDef) int64 {
	b, _ := json.Marshal(defs)
	return raw(b)
}

// Estimate holds the last reported size and how far the heuristic was off, so the next estimate needs only the tail.
type Estimate struct {
	AnchorTokens int64   // input plus output tokens reported for the last committed step
	AnchorLen    int     // history length at that point; 0 means no anchor
	Ratio        float64 // EMA of reported / heuristic; 0 means 1
}

// Anchor records a report: reported tokens covered the first n lines, whose heuristic size was heuristic.
func (e *Estimate) Anchor(reported int64, n int, heuristic int64) {
	if heuristic > 0 {
		r := min(max(float64(reported)/float64(heuristic), minRatio), maxRatio)
		if e.Ratio == 0 {
			e.Ratio = r
		} else {
			e.Ratio = 0.5*e.Ratio + 0.5*r
		}
	}
	e.AnchorTokens, e.AnchorLen = reported, n
}

// Scale applies the calibration ratio to a heuristic count.
func (e *Estimate) Scale(heuristic int64) int64 { return int64(e.ratio() * float64(heuristic)) }

func (e *Estimate) ratio() float64 {
	if e.Ratio == 0 {
		return 1
	}
	return e.Ratio
}

// Next estimates the whole request: the anchor plus the scaled tail, or the scaled heuristic of everything when there is no anchor; base is the system and tools.
func (e *Estimate) Next(history []message.Message, base int64) (int64, bool) {
	if e.AnchorLen > 0 && e.AnchorLen <= len(history) {
		return e.AnchorTokens + int64(e.ratio()*float64(Lines(history[e.AnchorLen:]))), true
	}
	return int64(e.ratio() * float64(base+Lines(history))), false
}

// Reset drops the anchor, as after a new epoch; the ratio stays, since the model is the same.
func (e *Estimate) Reset() { e.AnchorTokens, e.AnchorLen = 0, 0 }
