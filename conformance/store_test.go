package conformance

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/estonshi/aperture-potatos/conformance/compare"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/store"
	"github.com/estonshi/aperture-potatos/store/jsonl"
)

// TestStore runs store/: a case with script.json resumes its session.jsonl and runs like a loop case; any other checks how the file reads.
func TestStore(t *testing.T) {
	entries, err := os.ReadDir("store")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		dir := filepath.Join("store", e.Name())
		t.Run(e.Name(), func(t *testing.T) {
			if _, err := os.Stat(filepath.Join(dir, "script.json")); err == nil {
				res := RunLoop(t, dir)
				if t.Failed() {
					return
				}
				if err := res.Expected(dir, *update); err != nil {
					t.Fatal(err)
				}
				return
			}
			got, err := storeFormat(dir)
			if err != nil {
				t.Fatal(err)
			}
			if err := Verify(dir, *update, func(out string) error { return writeJSON(out, "actual.json", got) }); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// storeResult is expected.json of a format case.
type storeResult struct {
	Lines       []int64 `json:"lines"`                  // seqs a reader returns
	CurrentFrom *int64  `json:"current_from,omitempty"` // seq where the current context starts
	Error       string  `json:"error,omitempty"`        // reading failed
	AppendError string  `json:"append_error,omitempty"` // appending append.jsonl failed
}

func storeFormat(dir string) (storeResult, error) {
	raw, err := os.ReadFile(filepath.Join(dir, "session.jsonl"))
	if err != nil {
		return storeResult{}, err
	}
	id := "ses_01"
	var first struct {
		SessionID string `json:"session_id"`
	}
	if line, _, _ := bytes.Cut(raw, []byte("\n")); json.Unmarshal(line, &first) == nil && first.SessionID != "" {
		id = first.SessionID
	}
	tmp, err := os.MkdirTemp("", "store")
	if err != nil {
		return storeResult{}, err
	}
	defer os.RemoveAll(tmp)
	if err := os.WriteFile(filepath.Join(tmp, id+".jsonl"), raw, 0o644); err != nil {
		return storeResult{}, err
	}
	st := jsonl.New(tmp)
	ctx := context.Background()
	res := storeResult{Lines: []int64{}}
	lines, err := st.Load(ctx, id)
	if err != nil {
		res.Error = errCode(err)
		return res, nil
	}
	bySeq := map[int64][]byte{}
	for _, l := range bytes.Split(raw, []byte("\n")) {
		var s struct {
			Seq int64 `json:"seq"`
		}
		if json.Unmarshal(l, &s) == nil {
			bySeq[s.Seq] = l
		}
	}
	for _, m := range lines {
		res.Lines = append(res.Lines, m.Seq)
		out, err := json.Marshal(m)
		if err != nil {
			return res, err
		}
		if ok, err := compare.Same(out, bySeq[m.Seq]); err != nil || !ok {
			return res, fmt.Errorf("line %d does not round-trip:\n in  %s\n out %s", m.Seq, bySeq[m.Seq], out)
		}
	}
	if cur := store.Current(lines); len(cur) > 0 {
		seq := cur[0].Seq
		res.CurrentFrom = &seq
	}
	if add, err := os.ReadFile(filepath.Join(dir, "append.jsonl")); err == nil {
		var msgs []message.Message
		for _, l := range bytes.Split(add, []byte("\n")) {
			if len(bytes.TrimSpace(l)) == 0 {
				continue
			}
			var m message.Message
			if err := json.Unmarshal(l, &m); err != nil {
				return res, err
			}
			msgs = append(msgs, m)
		}
		if err := st.Append(ctx, id, msgs); err != nil {
			res.AppendError = errCode(err)
		}
	}
	return res, nil
}

func errCode(err error) string {
	switch {
	case errors.Is(err, store.ErrSeqConflict):
		return "seq_conflict"
	case errors.Is(err, store.ErrIncompatibleStore):
		return "incompatible_store"
	}
	return "other"
}
