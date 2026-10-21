package jsonl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/store"
)

func line(seq int64, text string) message.Message {
	m := message.UserText(text)
	m.V, m.ID, m.SessionID, m.Seq, m.Epoch = message.Version, "msg", "ses_1", seq, 1
	return m
}

func batch(id string, ms ...message.Message) []message.Message {
	for i := range ms {
		ms[i].Batch = id
	}
	ms[len(ms)-1].BatchEnd = true
	return ms
}

func texts(ms []message.Message) string {
	var out []string
	for _, m := range ms {
		out = append(out, m.Text())
	}
	return strings.Join(out, ",")
}

func TestAppendLoadRetry(t *testing.T) {
	ctx := context.Background()
	s := New(t.TempDir())
	if err := s.Append(ctx, "ses_1", batch("b_1", line(1, "a"), line(2, "b"))); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(ctx, "ses_1", batch("b_2", line(2, "b"), line(3, "c"))); err != nil {
		t.Fatalf("a retry repeating a stored line in another batch is skipped: %v", err)
	}
	got, err := s.Load(ctx, "ses_1")
	if err != nil || texts(got) != "a,b,c" {
		t.Fatalf("load: %v %q", err, texts(got))
	}
	if missing, err := s.Load(ctx, "ses_2"); err != nil || len(missing) != 0 {
		t.Fatalf("an unknown session loads empty: %v %v", missing, err)
	}
}

func TestSeqConflictWritesNothing(t *testing.T) {
	ctx := context.Background()
	s := New(t.TempDir())
	_ = s.Append(ctx, "ses_1", []message.Message{line(1, "a")})
	err := s.Append(ctx, "ses_1", []message.Message{line(1, "different"), line(2, "b")})
	if !errors.Is(err, store.ErrSeqConflict) {
		t.Fatalf("want ErrSeqConflict, got %v", err)
	}
	if got, _ := s.Load(ctx, "ses_1"); texts(got) != "a" {
		t.Fatalf("a failed Append writes nothing: %q", texts(got))
	}
}

func TestSecondWriterIsDetected(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	a, b := New(dir), New(dir)
	_ = a.Append(ctx, "ses_1", []message.Message{line(1, "a")})
	if err := b.Append(ctx, "ses_1", []message.Message{line(2, "from b")}); err != nil {
		t.Fatal(err)
	}
	// a still believes seq 1 is the last line, so its seq 2 must be compared with what b wrote
	if err := a.Append(ctx, "ses_1", []message.Message{line(2, "from a")}); !errors.Is(err, store.ErrSeqConflict) {
		t.Fatalf("want ErrSeqConflict, got %v", err)
	}
}

func TestTornTailIsCut(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := New(dir)
	_ = s.Append(ctx, "ses_1", []message.Message{line(1, "a")})
	_ = s.Append(ctx, "ses_1", batch("b_1", line(2, "b"), line(3, "c")))
	path := filepath.Join(dir, "ses_1.jsonl")
	raw, _ := os.ReadFile(path)
	lines := strings.SplitAfter(string(raw), "\n")
	// the batch lost its batch_end line, and half of the next line was written
	torn := lines[0] + lines[1] + `{"v":1,"id":"msg","ses`
	if err := os.WriteFile(path, []byte(torn), 0o644); err != nil {
		t.Fatal(err)
	}
	fresh := New(dir)
	got, err := fresh.Load(ctx, "ses_1")
	if err != nil || texts(got) != "a" {
		t.Fatalf("a batch without batch_end and a half line are dropped: %v %q", err, texts(got))
	}
	if err := fresh.Append(ctx, "ses_1", []message.Message{line(2, "b2")}); err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(path)
	if want := lines[0] + string(mustLine(line(2, "b2"))); string(after) != want {
		t.Fatalf("the next write cuts the torn tail first:\n%s", after)
	}
}

func mustLine(m message.Message) []byte {
	b, err := m.MarshalJSON()
	if err != nil {
		panic(err)
	}
	return append(b, '\n')
}

func TestNewerVersionRejected(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "ses_1.jsonl"), []byte(`{"v":2,"id":"x","seq":1}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := New(dir).Load(context.Background(), "ses_1"); !errors.Is(err, store.ErrIncompatibleStore) {
		t.Fatalf("want ErrIncompatibleStore, got %v", err)
	}
}

func TestListAndIDs(t *testing.T) {
	ctx := context.Background()
	s := New(t.TempDir())
	if ids, err := s.List(ctx, ""); err != nil || len(ids) != 0 {
		t.Fatalf("an absent directory lists nothing: %v %v", ids, err)
	}
	for _, id := range []string{"ses_1", "ses_1.side.notes", "ses_2"} {
		if err := s.Append(ctx, id, []message.Message{line(1, id)}); err != nil {
			t.Fatal(err)
		}
	}
	if ids, _ := s.List(ctx, "ses_1."); strings.Join(ids, ",") != "ses_1.side.notes" {
		t.Fatalf("list by prefix: %v", ids)
	}
	for _, bad := range []string{"", "..", "a/b", `a\b`} {
		if err := s.Append(ctx, bad, []message.Message{line(1, "x")}); !errors.Is(err, errBadID) {
			t.Fatalf("id %q: want errBadID, got %v", bad, err)
		}
	}
}

func TestUnknownFieldsSurvive(t *testing.T) {
	dir := t.TempDir()
	raw := `{"v":1,"id":"m1","session_id":"ses_1","seq":1,"epoch":1,"role":"user","origin":{"kind":"human"},"content":[{"type":"text","text":"hi","future":1},{"type":"hologram","x":2}],"ts":"2026-09-29T00:00:00Z","later":"kept"}` + "\n"
	if err := os.WriteFile(filepath.Join(dir, "ses_1.jsonl"), []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := New(dir).Load(context.Background(), "ses_1")
	if err != nil || len(got) != 1 {
		t.Fatal(err)
	}
	out, _ := got[0].MarshalJSON()
	for _, want := range []string{`"future":1`, `"hologram"`, `"later":"kept"`} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("%s lost from %s", want, out)
		}
	}
}

func rec(seq int64) json.RawMessage {
	return json.RawMessage(fmt.Sprintf(`{"seq":%d,"bucket":"main"}`, seq))
}

func seqs(t *testing.T, recs []json.RawMessage) string {
	t.Helper()
	var out []string
	for _, r := range recs {
		n, err := store.CallSeq(r)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, fmt.Sprint(n))
	}
	return strings.Join(out, ",")
}

func TestCallsRetryTornTailAndList(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	st := New(dir).(store.CallStore)
	if err := st.AppendCalls(ctx, "ses_1", []json.RawMessage{rec(1), rec(2)}); err != nil {
		t.Fatal(err)
	}
	if err := st.AppendCalls(ctx, "ses_1", []json.RawMessage{rec(2), rec(3)}); err != nil { // a retry that repeats seq 2
		t.Fatal(err)
	}
	path := filepath.Join(dir, "ses_1.calls.jsonl")
	f, _ := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	_, _ = f.WriteString(`{"seq":4,"buck`) // a torn write
	f.Close()
	got, err := New(dir).(store.CallStore).LoadCalls(ctx, "ses_1")
	if err != nil || seqs(t, got) != "1,2,3" {
		t.Fatalf("load: %v %s", err, seqs(t, got))
	}
	other := New(dir).(store.CallStore) // another process: reads back and cuts the half record first
	if err := other.AppendCalls(ctx, "ses_1", []json.RawMessage{rec(4)}); err != nil {
		t.Fatal(err)
	}
	got, _ = other.LoadCalls(ctx, "ses_1")
	raw, _ := os.ReadFile(path)
	if seqs(t, got) != "1,2,3,4" || strings.Count(string(raw), "\n") != 4 {
		t.Fatalf("after the cut: %s\n%s", seqs(t, got), raw)
	}
	if err := New(dir).Append(ctx, "ses_1", []message.Message{line(1, "a")}); err != nil {
		t.Fatal(err)
	}
	if ids, _ := New(dir).List(ctx, "ses_"); strings.Join(ids, ",") != "ses_1" {
		t.Fatalf("the calls file is not a session: %v", ids)
	}
	if err := New(dir).Append(ctx, "ses_1.calls", []message.Message{line(1, "a")}); !errors.Is(err, errBadID) {
		t.Fatalf("an id that would share the calls file: %v", err)
	}
	if got, err := st.LoadCalls(ctx, "ses_none"); err != nil || len(got) != 0 {
		t.Fatalf("no file: %v %v", got, err)
	}
}

func TestLabelRecords(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	s := New(dir)
	ls := s.(store.LabelStore)
	if _, ok, err := ls.LoadLabels(ctx, "ses_1"); ok || err != nil {
		t.Fatalf("no record yet: %v %v", ok, err)
	}
	if found, err := ls.FindLabels(ctx, nil); len(found) != 0 || err != nil {
		t.Fatalf("an empty directory finds nothing: %v %v", found, err)
	}
	if err := s.Append(ctx, "a", []message.Message{line(1, "x")}); err != nil {
		t.Fatal(err)
	}
	for _, rec := range []store.LabelRecord{
		{SessionID: "a.labels", Labels: map[string]string{"team": "x"}},
		{SessionID: "a", Labels: map[string]string{"team": "x"}},
		{SessionID: "a.b", ParentSessionID: "a", Labels: map[string]string{"team": "x"}},
		{SessionID: "c", Labels: map[string]string{"team": "y"}},
	} {
		if err := ls.SaveLabels(ctx, rec); err != nil {
			t.Fatal(err)
		}
	}
	if err := ls.SaveLabels(ctx, store.LabelRecord{SessionID: "a", Labels: map[string]string{"team": "x", "tier": "gold"}}); err != nil {
		t.Fatal(err)
	}
	rec, ok, err := ls.LoadLabels(ctx, "a")
	if !ok || err != nil || rec.Labels["tier"] != "gold" {
		t.Fatalf("a save replaces the record: %+v %v", rec, err)
	}
	found, err := ls.FindLabels(ctx, map[string]string{"team": "x"})
	var ids []string
	for _, f := range found {
		ids = append(ids, f.SessionID)
	}
	if err != nil || strings.Join(ids, ",") != "a,a.b,a.labels" || found[1].ParentSessionID != "a" {
		t.Fatalf("matches in session id order, parents kept: %v %v", ids, err)
	}
	if listed, _ := s.List(ctx, ""); strings.Join(listed, ",") != "a" {
		t.Fatalf("label files are not sessions: %v", listed)
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".labels-") {
			t.Fatalf("a temporary file is left behind: %s", e.Name())
		}
	}
	if err := ls.SaveLabels(ctx, store.LabelRecord{SessionID: "../x"}); !errors.Is(err, errBadID) {
		t.Fatalf("an id that names no file: %v", err)
	}
}
