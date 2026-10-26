package kernel

import (
	"bytes"
	"unicode/utf8"

	"github.com/estonshi/aperture-potatos/event"
)

// AnomalyConfig bounds the repetition detector that publishes model.anomaly; a zero field takes its default, and Disabled turns detection off.
type AnomalyConfig struct {
	Disabled           bool
	CheckEveryBytes    int // new bytes one scan needs
	WindowBytes        int // how much of a block the scanner keeps
	MaxUnitBytes       int // the longest period the suffix check looks for
	MinRepetitions     int // how often a suffix must repeat
	MinLineBytes       int // the shortest line the line check counts
	MinLineRepetitions int // how often a line must repeat
	BlockBytes         int // the size of one block the block check compares
}

// anomalyDefaults is deliberately conservative: code, tables and lists should not reach it.
var anomalyDefaults = AnomalyConfig{
	CheckEveryBytes:    512,
	WindowBytes:        8192,
	MaxUnitBytes:       16,
	MinRepetitions:     8,
	MinLineBytes:       8,
	MinLineRepetitions: 5,
	BlockBytes:         1024,
}

func (c AnomalyConfig) resolved() AnomalyConfig {
	out := anomalyDefaults
	if c.CheckEveryBytes > 0 {
		out.CheckEveryBytes = c.CheckEveryBytes
	}
	if c.WindowBytes > 0 {
		out.WindowBytes = max(c.WindowBytes, c.CheckEveryBytes)
	}
	if c.MaxUnitBytes > 0 {
		out.MaxUnitBytes = c.MaxUnitBytes
	}
	if c.MinRepetitions > 0 {
		out.MinRepetitions = c.MinRepetitions
	}
	if c.MinLineBytes > 0 {
		out.MinLineBytes = c.MinLineBytes
	}
	if c.MinLineRepetitions > 0 {
		out.MinLineRepetitions = c.MinLineRepetitions
	}
	if c.BlockBytes > 0 {
		out.BlockBytes = c.BlockBytes
	}
	out.Disabled = c.Disabled
	return out
}

// anomalyScan watches one block's text and reports each repetition mode at most once; it compares bytes, so a repeated multi-byte character is a repetition of its byte period.
type anomalyScan struct {
	cfg   AnomalyConfig
	tail  []byte // the last WindowBytes of the block
	total int    // bytes the block has emitted
	next  int    // total at which the next scan is due
	chars int    // characters the block has emitted, the event's offset
	seen  map[string]bool
	dead  bool // a comparison panicked, so this block is no longer checked
}

func newAnomalyScan(cfg AnomalyConfig) *anomalyScan {
	return &anomalyScan{cfg: cfg, next: cfg.CheckEveryBytes, seen: map[string]bool{}}
}

// write feeds one delta and scans once enough new text has arrived.
func (a *anomalyScan) write(text string) *event.ModelAnomalyDetail {
	if a.cfg.Disabled || a.dead {
		return nil
	}
	a.chars += utf8.RuneCountInString(text)
	a.total += len(text)
	a.tail = append(a.tail, text...)
	if n := len(a.tail); n > a.cfg.WindowBytes {
		a.tail = append(a.tail[:0], a.tail[n-a.cfg.WindowBytes:]...)
	}
	if a.total < a.next {
		return nil
	}
	a.next = a.total + a.cfg.CheckEveryBytes
	return a.detect()
}

// final scans what is left when a block ends, which catches a repetition shorter than one check interval.
func (a *anomalyScan) final() *event.ModelAnomalyDetail {
	if a.cfg.Disabled || a.dead || len(a.tail) == 0 {
		return nil
	}
	return a.detect()
}

func (a *anomalyScan) detect() *event.ModelAnomalyDetail {
	for _, check := range []func() *event.ModelAnomalyDetail{a.repeatedSuffix, a.repeatedLine, a.repeatedBlock} {
		if d := check(); d != nil {
			return d
		}
	}
	return nil
}

// repeatedSuffix finds a short unit that ends the text at least MinRepetitions times, such as "the the the" or one character repeated.
func (a *anomalyScan) repeatedSuffix() *event.ModelAnomalyDetail {
	if a.seen["suffix"] {
		return nil
	}
	for unit := 1; unit <= a.cfg.MaxUnitBytes; unit++ {
		need := unit * a.cfg.MinRepetitions
		if len(a.tail) < need {
			break
		}
		periodic := true
		for k := len(a.tail) - need + unit; k < len(a.tail); k++ {
			if a.tail[k] != a.tail[k-unit] {
				periodic = false
				break
			}
		}
		if !periodic {
			continue
		}
		u := a.tail[len(a.tail)-unit:]
		if blankBytes(u) {
			continue
		}
		reps := a.cfg.MinRepetitions
		for len(a.tail)-(reps+1)*unit >= 0 && bytes.Equal(a.tail[len(a.tail)-(reps+1)*unit:len(a.tail)-reps*unit], u) {
			reps++
		}
		a.seen["suffix"] = true
		return &event.ModelAnomalyDetail{Mode: "suffix", UnitChars: utf8.RuneCount(u), Repetitions: reps, Sample: a.sample()}
	}
	return nil
}

// repeatedLine finds the same whole line MinLineRepetitions times in a row at the end, such as a repeated paragraph line.
func (a *anomalyScan) repeatedLine() *event.ModelAnomalyDetail {
	if a.seen["line"] {
		return nil
	}
	lines := bytes.Split(a.tail, []byte("\n"))
	end := len(lines) - 1
	for end >= 0 && blankBytes(lines[end]) {
		end--
	}
	if end < 0 {
		return nil
	}
	last := lines[end]
	if len(last) < a.cfg.MinLineBytes {
		return nil
	}
	reps := 1
	for i := end - 1; i >= 0 && end-i <= 16; i-- {
		if !bytes.Equal(lines[i], last) {
			break
		}
		reps++
	}
	if reps < a.cfg.MinLineRepetitions {
		return nil
	}
	a.seen["line"] = true
	return &event.ModelAnomalyDetail{Mode: "line", UnitChars: utf8.RuneCount(last), Repetitions: reps, Sample: a.sample()}
}

// repeatedBlock finds two or more identical BlockBytes blocks at the end of the text.
func (a *anomalyScan) repeatedBlock() *event.ModelAnomalyDetail {
	if a.seen["block"] {
		return nil
	}
	n := a.cfg.BlockBytes
	if len(a.tail) < 2*n {
		return nil
	}
	last := a.tail[len(a.tail)-n:]
	if blankBytes(last) {
		return nil
	}
	reps := 1
	for len(a.tail)-(reps+1)*n >= 0 && bytes.Equal(a.tail[len(a.tail)-(reps+1)*n:len(a.tail)-reps*n], last) {
		reps++
	}
	if reps < 2 {
		return nil
	}
	a.seen["block"] = true
	return &event.ModelAnomalyDetail{Mode: "block", UnitChars: utf8.RuneCount(last), Repetitions: reps, Sample: a.sample()}
}

// sample is the tail of the block, at most 256 characters, cut at a rune boundary.
func (a *anomalyScan) sample() string {
	const limit = 256
	runes := []rune(string(a.tail))
	if len(runes) > limit {
		runes = runes[len(runes)-limit:]
	}
	return string(runes)
}

// blankBytes reports whether b holds only whitespace, which never counts as a repetition worth reporting.
func blankBytes(b []byte) bool {
	for _, c := range b {
		switch c {
		case ' ', '\t', '\r', '\n':
		default:
			return false
		}
	}
	return true
}
