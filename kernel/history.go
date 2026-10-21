package kernel

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"unicode"

	"github.com/estonshi/aperture-potatos/message"
)

// HistoryQuery is a word search over the session's lines, including those compaction took out of the context.
type HistoryQuery struct {
	Words   []string // every word must appear, case-insensitive
	Limit   int      // default 10, at most 50
	Current bool     // only lines the current context holds, as a side chat sees its main session
}

// HistoryHit is one matching line; pass Seq to HistoryLine to read it whole.
type HistoryHit struct {
	Seq     int64        `json:"seq"`
	Epoch   int          `json:"epoch"`
	Role    message.Role `json:"role"`
	TurnID  string       `json:"turn_id,omitempty"`
	Current bool         `json:"current"` // the line is in the current context
	Snippet string       `json:"snippet"`
}

var (
	ErrEmptyQuery  = errors.New("a history query needs at least one word") // from SearchHistory when every word is empty once trimmed
	ErrUnknownLine = errors.New("no committed line with this seq")         // from HistoryLine
)

// snippetRunes is how much a hit shows on each side of the first matched word.
const snippetRunes = 200

// SearchHistory searches the lines of this session that the current context holds or that compaction took out of it, newest first; a copy of a line in this file is skipped, so each line is found once.
func (s *Session) SearchHistory(ctx context.Context, q HistoryQuery) ([]HistoryHit, error) {
	var words [][]rune
	for _, w := range q.Words {
		if w = strings.TrimSpace(w); w != "" {
			words = append(words, lowerRunes(w))
		}
	}
	if len(words) == 0 {
		return nil, ErrEmptyQuery
	}
	limit := q.Limit
	if limit <= 0 {
		limit = 10
	}
	limit = min(limit, 50)
	lines, current, err := s.committed(ctx)
	if err != nil {
		return nil, err
	}
	found := reachable(lines, current, q.Current)
	var hits []HistoryHit
	for i := len(lines) - 1; i >= 0 && len(hits) < limit; i-- {
		m := lines[i]
		if !found[m.ID] {
			continue
		}
		text := []rune(searchText(m))
		at := matchAll(lowerRunes(string(text)), words)
		if at < 0 {
			continue
		}
		lo, hi := max(at-snippetRunes, 0), min(at+snippetRunes, len(text))
		hits = append(hits, HistoryHit{Seq: m.Seq, Epoch: m.Epoch, Role: m.Role, TurnID: m.TurnID, Current: current[m.ID], Snippet: string(text[lo:hi])})
	}
	return hits, nil
}

// HistoryLine returns a line a search can find by seq, and whether the current context holds it.
func (s *Session) HistoryLine(ctx context.Context, seq int64) (message.Message, bool, error) {
	lines, current, err := s.committed(ctx)
	if err != nil {
		return message.Message{}, false, err
	}
	i, ok := slices.BinarySearchFunc(lines, seq, func(m message.Message, seq int64) int { return cmp.Compare(m.Seq, seq) })
	if !ok || !reachable(lines, current, false)[lines[i].ID] {
		return message.Message{}, false, fmt.Errorf("%w: %d", ErrUnknownLine, seq)
	}
	return lines[i], current[lines[i].ID], nil
}

// committed is the stored lines plus the committed ones the store does not have yet, in seq order, and the ids the current context holds or copies; a line whose calls still run is left out, so a search never finds the call that runs it.
func (s *Session) committed(ctx context.Context) ([]message.Message, map[string]bool, error) {
	var pending []message.Message
	current, running := map[string]bool{}, map[string]bool{}
	if err := s.exec(func(st *state) {
		pending = st.unsynced()
		for _, m := range st.history[min(st.stableLen, len(st.history)):] {
			running[m.ID] = true
		}
		for _, m := range st.history {
			current[m.ID] = true
			if m.CopyOf != "" {
				current[m.CopyOf] = true
			}
		}
	}); err != nil {
		return nil, nil, err
	}
	lines, err := s.stored.load(ctx, s)
	if err != nil {
		return nil, nil, err
	}
	var last int64
	if len(lines) > 0 {
		last = lines[len(lines)-1].Seq
	}
	for _, m := range pending {
		if m.Seq > last {
			lines = append(lines, m)
		}
	}
	lines = slices.DeleteFunc(lines, func(m message.Message) bool { return running[m.ID] })
	return lines, current, nil
}

// storedLines caches what the store holds for this session, so repeated searches read the file once; persist extends it, since this session is the file's only writer.
type storedLines struct {
	mu     sync.Mutex
	loaded bool
	lines  []message.Message
}

// load returns a copy the caller may change.
func (c *storedLines) load(ctx context.Context, s *Session) ([]message.Message, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.loaded {
		lines, err := s.agent.cfg.store.Load(ctx, s.id)
		if err != nil {
			return nil, err
		}
		c.lines, c.loaded = lines, true
	}
	return slices.Clone(c.lines), nil
}

func (c *storedLines) extend(lines []message.Message) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.loaded {
		return
	}
	for _, m := range lines {
		if len(c.lines) == 0 || m.Seq > c.lines[len(c.lines)-1].Seq {
			c.lines = append(c.lines, m)
		}
	}
}

// reachable is the ids of the lines a search looks at: the current context and, unless currentOnly, what compaction took out of it; what a rewind took out and everything before a clear or a side chat's latest opening stay out, since the user discarded them.
func reachable(lines []message.Message, current map[string]bool, currentOnly bool) map[string]bool {
	own := map[string]bool{}
	for _, m := range lines {
		own[m.ID] = true
	}
	orig := func(m message.Message) string { // a fork's copy stands for itself, since its original is in the source's file
		if m.CopyOf != "" && own[m.CopyOf] {
			return m.CopyOf
		}
		return m.ID
	}
	var epochs []map[string]bool
	var reasons []string
	for _, m := range lines {
		if m.Role == message.RoleSystem && m.Boundary != nil {
			epochs, reasons = append(epochs, map[string]bool{}), append(reasons, m.Boundary.Reason)
		} else if len(epochs) > 0 {
			epochs[len(epochs)-1][orig(m)] = true
		}
	}
	compacted := map[string]bool{}
	for k := 1; k < len(epochs) && !currentOnly; k++ {
		switch reasons[k] {
		case "compaction":
			for id := range epochs[k-1] {
				if !epochs[k][id] {
					compacted[id] = true
				}
			}
		case "rewind", "switch":
		default: // clear, dropped, fork and side_chat start over
			clear(compacted)
		}
	}
	out := map[string]bool{}
	for _, m := range lines {
		if id := orig(m); id == m.ID && m.Role != message.RoleSystem && (current[id] || compacted[id]) {
			out[id] = true
		}
	}
	return out
}

// searchText is what a line offers to search: text, reminders, tool calls with their input and tool results; reasoning is mostly opaque, so it is left out.
func searchText(m message.Message) string {
	var parts []string
	var add func(c message.Content)
	add = func(c message.Content) {
		for _, b := range c {
			switch v := b.(type) {
			case *message.TextBlock:
				parts = append(parts, v.Text)
			case *message.RefusalBlock:
				parts = append(parts, v.Text)
			case *message.ReminderBlock:
				parts = append(parts, v.Text)
			case *message.ToolCallBlock:
				parts = append(parts, v.Name+" "+string(v.Input))
			case *message.ToolResultBlock:
				add(v.Content)
			}
		}
	}
	add(m.Content)
	return strings.Join(parts, "\n")
}

// lowerRunes lowercases rune by rune, so indexes into the result are indexes into the original.
func lowerRunes(s string) []rune {
	r := []rune(s)
	for i, c := range r {
		r[i] = unicode.ToLower(c)
	}
	return r
}

// matchAll is the index of the earliest matched word when every word occurs in text, or -1.
func matchAll(text []rune, words [][]rune) int {
	first := -1
	for _, w := range words {
		i := indexRunes(text, w)
		if i < 0 {
			return -1
		}
		if first < 0 || i < first {
			first = i
		}
	}
	return first
}

func indexRunes(text, w []rune) int {
	for i := 0; i+len(w) <= len(text); i++ {
		if slices.Equal(text[i:i+len(w)], w) {
			return i
		}
	}
	return -1
}
