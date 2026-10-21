// Package jsonl is the reference session store: one <session_id>.jsonl file per session, one message per line, <session_id>.calls.jsonl for its model call ledger and <session_id>.labels.json for its labels.
package jsonl

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/store"
)

type fileStore struct {
	dir string
	mu  sync.Mutex
	// good is where the last complete write of each session ended, so a mismatch means a torn write or another writer.
	good  map[string]tail
	calls map[string]tail // the same for each session's calls file
}

type tail struct {
	offset int64
	seq    int64
}

// New stores sessions under dir, which is created on the first write.
func New(dir string) store.SessionStore {
	return &fileStore{dir: dir, good: map[string]tail{}, calls: map[string]tail{}}
}

var errBadID = errors.New("jsonl: session id cannot name a file")

// callsSuffix names a session's calls file, so no session id may end with it.
const callsSuffix = ".calls"

func (s *fileStore) path(id string) (string, error) {
	if id == "" || id == "." || id == ".." || strings.ContainsAny(id, `/\`+"\x00") || strings.HasSuffix(id, callsSuffix) {
		return "", fmt.Errorf("%w: %q", errBadID, id)
	}
	return filepath.Join(s.dir, id+".jsonl"), nil
}

func (s *fileStore) Append(ctx context.Context, id string, msgs []message.Message) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(msgs) == 0 {
		return nil
	}
	path, err := s.path(id)
	if err != nil {
		return err
	}
	var out bytes.Buffer
	for _, m := range msgs {
		b, err := json.Marshal(m)
		if err != nil {
			return err
		}
		out.Write(b)
		out.WriteByte('\n')
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := lock(f, true); err != nil {
		return err
	}
	defer unlock(f)
	s.mu.Lock()
	defer s.mu.Unlock()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	t, known := s.good[id]
	var have []message.Message
	if !known || info.Size() != t.offset || msgs[0].Seq <= t.seq {
		// torn tail, another process, or a retry: read back, cut to the last complete batch and compare seqs
		raw, err := io.ReadAll(f)
		if err != nil {
			return err
		}
		lines, end, err := parse(raw)
		if err != nil {
			return err
		}
		if end != int64(len(raw)) {
			if err := f.Truncate(end); err != nil {
				return err
			}
		}
		have = lines
		t = tail{offset: end}
		if len(lines) > 0 {
			t.seq = lines[len(lines)-1].Seq
		}
	}
	add, err := newLines(id, have, t.seq, msgs)
	if err != nil {
		return err
	}
	if len(add) == 0 {
		s.good[id] = t
		return nil
	}
	if len(add) < len(msgs) { // part of a retry was already stored: write only the rest
		out.Reset()
		for _, m := range add {
			b, _ := json.Marshal(m)
			out.Write(b)
			out.WriteByte('\n')
		}
	}
	if _, err := f.WriteAt(out.Bytes(), t.offset); err != nil {
		delete(s.good, id) // the next write reads back and cuts whatever landed
		return err
	}
	if err := f.Sync(); err != nil {
		delete(s.good, id)
		return err
	}
	s.good[id] = tail{offset: t.offset + int64(out.Len()), seq: add[len(add)-1].Seq}
	return nil
}

// newLines drops the lines a retry already stored and fails on a seq that holds a different line or goes backwards.
func newLines(id string, have []message.Message, last int64, msgs []message.Message) ([]message.Message, error) {
	var add []message.Message
	for _, m := range msgs {
		if m.Seq <= last {
			i, found := slices.BinarySearchFunc(have, m.Seq, func(x message.Message, seq int64) int { return cmpSeq(x.Seq, seq) })
			if !found {
				return nil, fmt.Errorf("jsonl: session %s: seq %d is not after the last stored line", id, m.Seq)
			}
			same, err := store.SameLine(have[i], m)
			if err != nil {
				return nil, err
			}
			if !same {
				return nil, fmt.Errorf("%w: session %s seq %d", store.ErrSeqConflict, id, m.Seq)
			}
			continue
		}
		last = m.Seq
		add = append(add, m)
	}
	return add, nil
}

func cmpSeq(a, b int64) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

func (s *fileStore) Load(ctx context.Context, id string) ([]message.Message, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := s.path(id)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if err := lock(f, false); err != nil {
		return nil, err
	}
	defer unlock(f)
	raw, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	lines, _, err := parse(raw)
	return lines, err
}

func (s *fileStore) List(ctx context.Context, prefix string) ([]string, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), ".jsonl")
		if ok && !e.IsDir() && strings.HasPrefix(id, prefix) && !strings.HasSuffix(id, callsSuffix) {
			out = append(out, id)
		}
	}
	slices.Sort(out)
	return out, nil
}

// parse reads complete lines and returns where the last complete batch ends: a half line or a batch without batch_end at the end is left out (RULE-STORE-008).
func parse(raw []byte) ([]message.Message, int64, error) {
	var lines []message.Message
	var ends []int64 // byte offset after each line
	var off int64
	for len(raw) > 0 {
		i := bytes.IndexByte(raw, '\n')
		if i < 0 {
			break // a half line at the end
		}
		b := raw[:i]
		raw = raw[i+1:]
		off += int64(i + 1)
		if len(bytes.TrimSpace(b)) == 0 {
			continue
		}
		var v struct {
			V int `json:"v"`
		}
		if err := json.Unmarshal(b, &v); err != nil {
			return nil, 0, fmt.Errorf("jsonl: line %d: %w", len(lines)+1, err)
		}
		if v.V > message.Version {
			return nil, 0, fmt.Errorf("%w: line %d has v %d", store.ErrIncompatibleStore, len(lines)+1, v.V)
		}
		var m message.Message
		if err := json.Unmarshal(b, &m); err != nil {
			return nil, 0, fmt.Errorf("jsonl: line %d: %w", len(lines)+1, err)
		}
		lines = append(lines, m)
		ends = append(ends, off)
	}
	if n := len(lines); n > 0 && lines[n-1].Batch != "" && !lines[n-1].BatchEnd {
		batch := lines[n-1].Batch
		for n > 0 && lines[n-1].Batch == batch {
			n--
		}
		lines, ends = lines[:n], ends[:n]
	}
	var end int64
	if len(ends) > 0 {
		end = ends[len(ends)-1]
	}
	return lines, end, nil
}

func (s *fileStore) AppendCalls(ctx context.Context, id string, calls []json.RawMessage) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(calls) == 0 {
		return nil
	}
	path, err := s.path(id)
	if err != nil {
		return err
	}
	path = strings.TrimSuffix(path, ".jsonl") + callsSuffix + ".jsonl"
	seqs := make([]int64, len(calls))
	for i, c := range calls {
		if seqs[i], err = store.CallSeq(c); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := lock(f, true); err != nil {
		return err
	}
	defer unlock(f)
	s.mu.Lock()
	defer s.mu.Unlock()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	t, known := s.calls[id]
	if !known || info.Size() != t.offset || seqs[0] <= t.seq {
		raw, err := io.ReadAll(f)
		if err != nil {
			return err
		}
		recs, end := parseCalls(raw)
		if end != int64(len(raw)) {
			if err := f.Truncate(end); err != nil {
				return err
			}
		}
		t = tail{offset: end}
		if len(recs) > 0 {
			t.seq, _ = store.CallSeq(recs[len(recs)-1])
		}
	}
	var out bytes.Buffer
	last := t.seq
	for i, c := range calls {
		if seqs[i] <= last { // an earlier try stored it
			continue
		}
		last = seqs[i]
		out.Write(bytes.TrimSpace(c))
		out.WriteByte('\n')
	}
	if out.Len() == 0 {
		s.calls[id] = t
		return nil
	}
	if _, err := f.WriteAt(out.Bytes(), t.offset); err != nil {
		delete(s.calls, id)
		return err
	}
	if err := f.Sync(); err != nil {
		delete(s.calls, id)
		return err
	}
	s.calls[id] = tail{offset: t.offset + int64(out.Len()), seq: last}
	return nil
}

func (s *fileStore) LoadCalls(ctx context.Context, id string) ([]json.RawMessage, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := s.path(id)
	if err != nil {
		return nil, err
	}
	f, err := os.Open(strings.TrimSuffix(path, ".jsonl") + callsSuffix + ".jsonl")
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if err := lock(f, false); err != nil {
		return nil, err
	}
	defer unlock(f)
	raw, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	recs, _ := parseCalls(raw)
	return recs, nil
}

// parseCalls reads complete records in increasing seq order and returns where the last complete line ends; only a half line at the end is left for the next write to cut, and a line without a seq is skipped.
func parseCalls(raw []byte) ([]json.RawMessage, int64) {
	var recs []json.RawMessage
	var off, end, last int64
	for len(raw) > 0 {
		i := bytes.IndexByte(raw, '\n')
		if i < 0 {
			break
		}
		b := bytes.TrimSpace(raw[:i])
		raw = raw[i+1:]
		off += int64(i + 1)
		end = off
		if len(b) == 0 {
			continue
		}
		if seq, err := store.CallSeq(b); err == nil && seq > last {
			recs = append(recs, append(json.RawMessage(nil), b...))
			last = seq
		}
	}
	return recs, end
}

// labelsExt names a session's label record; it ends in .json, so List never takes it for a session.
const labelsExt = ".labels.json"

func (s *fileStore) labelsPath(id string) (string, error) {
	p, err := s.path(id)
	if err != nil {
		return "", err
	}
	return strings.TrimSuffix(p, ".jsonl") + labelsExt, nil
}

// SaveLabels writes the record to a temporary file and renames it over the old one, so a reader never sees half a record.
func (s *fileStore) SaveLabels(ctx context.Context, rec store.LabelRecord) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path, err := s.labelsPath(rec.SessionID)
	if err != nil {
		return err
	}
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(s.dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(s.dir, ".labels-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	_, err = f.Write(append(b, '\n'))
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		_ = os.Remove(tmp)
	}
	return err
}

func (s *fileStore) LoadLabels(ctx context.Context, id string) (store.LabelRecord, bool, error) {
	if err := ctx.Err(); err != nil {
		return store.LabelRecord{}, false, err
	}
	path, err := s.labelsPath(id)
	if err != nil {
		return store.LabelRecord{}, false, err
	}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return store.LabelRecord{}, false, nil
	}
	if err != nil {
		return store.LabelRecord{}, false, err
	}
	var rec store.LabelRecord
	if err := json.Unmarshal(raw, &rec); err != nil {
		return store.LabelRecord{}, false, fmt.Errorf("jsonl: %s: %w", filepath.Base(path), err)
	}
	return rec, true, nil
}

// FindLabels reads every label record in the directory; a store with many sessions indexes them instead.
func (s *fileStore) FindLabels(ctx context.Context, match map[string]string) ([]store.LabelRecord, error) {
	entries, err := os.ReadDir(s.dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []store.LabelRecord
	for _, e := range entries {
		id, ok := strings.CutSuffix(e.Name(), labelsExt)
		if !ok || e.IsDir() {
			continue
		}
		rec, found, err := s.LoadLabels(ctx, id)
		if err != nil {
			return nil, err
		}
		if found && store.MatchLabels(rec.Labels, match) {
			out = append(out, rec)
		}
	}
	slices.SortFunc(out, func(a, b store.LabelRecord) int { return strings.Compare(a.SessionID, b.SessionID) }) // file names sort "a.b.labels.json" before "a.labels.json"
	return out, nil
}
