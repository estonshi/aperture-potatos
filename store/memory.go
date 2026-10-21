package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"

	"github.com/estonshi/aperture-potatos/message"
)

type memory struct {
	mu       sync.Mutex
	sessions map[string][]stored
	calls    map[string][]callRec
	labels   map[string]LabelRecord
}

type callRec struct {
	seq int64
	raw json.RawMessage
}

type stored struct {
	msg  message.Message
	line []byte // canonical encoding without batch markers, compared on retry
}

// NewMemory keeps sessions in process memory; it is the default when no store is configured, and it keeps calls and labels too.
func NewMemory() SessionStore {
	return &memory{sessions: map[string][]stored{}, calls: map[string][]callRec{}, labels: map[string]LabelRecord{}}
}

func (s *memory) SaveLabels(ctx context.Context, rec LabelRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	rec.Labels = maps.Clone(rec.Labels) // the caller keeps its map
	s.mu.Lock()
	defer s.mu.Unlock()
	s.labels[rec.SessionID] = rec
	return nil
}

func (s *memory) LoadLabels(ctx context.Context, sessionID string) (LabelRecord, bool, error) {
	if err := ctx.Err(); err != nil {
		return LabelRecord{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	rec, ok := s.labels[sessionID]
	rec.Labels = maps.Clone(rec.Labels)
	return rec, ok, nil
}

func (s *memory) FindLabels(ctx context.Context, match map[string]string) ([]LabelRecord, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []LabelRecord
	for _, id := range slices.Sorted(maps.Keys(s.labels)) {
		if rec := s.labels[id]; MatchLabels(rec.Labels, match) {
			rec.Labels = maps.Clone(rec.Labels)
			out = append(out, rec)
		}
	}
	return out, nil
}

func (s *memory) Append(ctx context.Context, sessionID string, msgs []message.Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	in := make([]stored, len(msgs))
	for i, m := range msgs {
		line, err := sameness(m)
		if err != nil {
			return err
		}
		in[i] = stored{msg: m, line: line}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	have := s.sessions[sessionID]
	var last int64
	if len(have) > 0 {
		last = have[len(have)-1].msg.Seq
	}
	var add []stored
	for _, m := range in {
		i, found := slices.BinarySearchFunc(have, m.msg.Seq, func(x stored, seq int64) int { return compareSeq(x.msg.Seq, seq) })
		switch {
		case found && bytes.Equal(have[i].line, m.line):
		case found:
			return fmt.Errorf("%w: session %s seq %d", ErrSeqConflict, sessionID, m.msg.Seq)
		default:
			if m.msg.Seq <= last {
				return fmt.Errorf("store: session %s: seq %d is not after the last stored line", sessionID, m.msg.Seq)
			}
			last = m.msg.Seq
			add = append(add, m)
		}
	}
	s.sessions[sessionID] = append(have[:len(have):len(have)], add...)
	return nil
}

func (s *memory) Load(ctx context.Context, sessionID string) ([]message.Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	have := s.sessions[sessionID]
	s.mu.Unlock()
	out := make([]message.Message, len(have))
	for i, st := range have {
		out[i] = st.msg
	}
	return out, nil
}

func (s *memory) List(ctx context.Context, prefix string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []string
	for id, lines := range s.sessions {
		if strings.HasPrefix(id, prefix) && len(lines) > 0 {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out, nil
}

func (s *memory) AppendCalls(ctx context.Context, sessionID string, calls []json.RawMessage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	in := make([]callRec, len(calls))
	for i, c := range calls {
		seq, err := CallSeq(c)
		if err != nil {
			return err
		}
		in[i] = callRec{seq: seq, raw: append(json.RawMessage(nil), c...)}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	have := s.calls[sessionID]
	var last int64
	if len(have) > 0 {
		last = have[len(have)-1].seq
	}
	for _, c := range in {
		if c.seq > last { // a lower seq was stored by an earlier try
			have = append(have, c)
			last = c.seq
		}
	}
	s.calls[sessionID] = have
	return nil
}

func (s *memory) LoadCalls(ctx context.Context, sessionID string) ([]json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]json.RawMessage, len(s.calls[sessionID]))
	for i, c := range s.calls[sessionID] {
		out[i] = append(json.RawMessage(nil), c.raw...)
	}
	return out, nil
}

func compareSeq(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// sameness encodes a line for the retry comparison, leaving out batch markers because a retry may regroup lines into another batch.
func sameness(m message.Message) ([]byte, error) {
	m.Batch, m.BatchEnd = "", false
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	var v any
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}
