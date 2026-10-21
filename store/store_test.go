package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"

	"github.com/estonshi/aperture-potatos/message"
)

func line(seq int64, text string) message.Message {
	m := message.UserText(text)
	m.V, m.ID, m.SessionID, m.Seq, m.Epoch = message.Version, "msg", "ses_1", seq, 1
	return m
}

func TestMemoryAppendLoad(t *testing.T) {
	ctx := context.Background()
	s := NewMemory()
	if err := s.Append(ctx, "ses_1", []message.Message{line(1, "a"), line(2, "b")}); err != nil {
		t.Fatal(err)
	}
	retry := []message.Message{line(2, "b"), line(3, "c")}
	retry[0].Batch, retry[1].Batch, retry[1].BatchEnd = "b_2", "b_2", true
	if err := s.Append(ctx, "ses_1", retry); err != nil {
		t.Fatalf("a retry that repeats a stored line in another batch must be skipped: %v", err)
	}
	got, err := s.Load(ctx, "ses_1")
	if err != nil || len(got) != 3 || got[2].Text() != "c" {
		t.Fatalf("load: %v %+v", err, got)
	}
	if other, _ := s.Load(ctx, "ses_2"); len(other) != 0 {
		t.Fatalf("sessions must not share lines: %+v", other)
	}
}

func TestMemorySeqConflictIsAtomic(t *testing.T) {
	ctx := context.Background()
	s := NewMemory()
	_ = s.Append(ctx, "ses_1", []message.Message{line(1, "a")})
	err := s.Append(ctx, "ses_1", []message.Message{line(2, "b"), line(1, "different")})
	if !errors.Is(err, ErrSeqConflict) {
		t.Fatalf("want ErrSeqConflict, got %v", err)
	}
	if got, _ := s.Load(ctx, "ses_1"); len(got) != 1 {
		t.Fatalf("a failed Append must write nothing, got %d lines", len(got))
	}
	if err := s.Append(ctx, "ses_1", []message.Message{line(3, "c"), line(2, "b")}); err == nil {
		t.Fatal("want an error for a seq that goes backwards")
	}
}

func TestMemoryHonorsContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := NewMemory().Append(ctx, "s", []message.Message{line(1, "a")}); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestCurrent(t *testing.T) {
	sys := func(seq int64, reason string) message.Message {
		m := line(seq, "")
		m.Role, m.Boundary = message.RoleSystem, &message.Boundary{Reason: reason}
		return m
	}
	msgs := []message.Message{sys(1, "new"), line(2, "a"), sys(3, "clear"), line(4, "b"), line(5, "c")}
	if cur := Current(msgs); len(cur) != 3 || cur[0].Seq != 3 {
		t.Fatalf("current starts at the last boundary: %+v", cur)
	}
	if cur := Current(msgs[1:2]); len(cur) != 1 {
		t.Fatal("without a boundary every line is current")
	}
}

func TestMemoryList(t *testing.T) {
	ctx := context.Background()
	s := NewMemory()
	for _, id := range []string{"ses_2", "ses_1.side.a", "ses_1"} {
		_ = s.Append(ctx, id, []message.Message{line(1, id)})
	}
	if ids, _ := s.List(ctx, "ses_1"); len(ids) != 2 || ids[0] != "ses_1" || ids[1] != "ses_1.side.a" {
		t.Fatalf("sorted by id, filtered by prefix: %v", ids)
	}
}

func TestMemoryCalls(t *testing.T) {
	ctx := context.Background()
	st := NewMemory().(CallStore)
	recs := func(n ...int) []json.RawMessage {
		var out []json.RawMessage
		for _, k := range n {
			out = append(out, json.RawMessage(fmt.Sprintf(`{"seq":%d}`, k)))
		}
		return out
	}
	_ = st.AppendCalls(ctx, "s", recs(1, 2))
	_ = st.AppendCalls(ctx, "s", recs(2, 3)) // a retry that repeats seq 2
	got, _ := st.LoadCalls(ctx, "s")
	if len(got) != 3 || string(got[2]) != `{"seq":3}` {
		t.Fatalf("got %s", got)
	}
	if err := st.AppendCalls(ctx, "s", []json.RawMessage{json.RawMessage(`{}`)}); err == nil {
		t.Fatal("a record without a seq is refused")
	}
}

func TestMemoryLabelRecords(t *testing.T) {
	ctx := context.Background()
	ls := NewMemory().(LabelStore)
	labels := map[string]string{"team": "x"}
	if err := ls.SaveLabels(ctx, LabelRecord{SessionID: "b", Labels: labels}); err != nil {
		t.Fatal(err)
	}
	labels["team"] = "changed"
	_ = ls.SaveLabels(ctx, LabelRecord{SessionID: "a", ParentSessionID: "b", Labels: map[string]string{"team": "x"}})
	_ = ls.SaveLabels(ctx, LabelRecord{SessionID: "c"})
	rec, ok, _ := ls.LoadLabels(ctx, "b")
	if !ok || rec.Labels["team"] != "x" {
		t.Fatalf("the store keeps its own copy: %+v", rec)
	}
	rec.Labels["team"] = "mutated"
	found, _ := ls.FindLabels(ctx, map[string]string{"team": "x"})
	if len(found) != 2 || found[0].SessionID != "a" || found[1].SessionID != "b" || found[1].Labels["team"] != "x" {
		t.Fatalf("matches in id order, untouched by callers: %+v", found)
	}
	if found, _ := ls.FindLabels(ctx, map[string]string{"team": ""}); len(found) != 1 || found[0].SessionID != "c" {
		t.Fatalf("an empty value matches a missing key: %+v", found)
	}
}
