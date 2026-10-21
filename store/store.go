// Package store persists sessions as append-only sequences of message lines; store/jsonl is the reference file implementation.
package store

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"

	"github.com/estonshi/aperture-potatos/message"
)

// SessionStore persists one session as an append-only sequence of message lines.
type SessionStore interface {
	Append(ctx context.Context, sessionID string, msgs []message.Message) error // atomic; a Seq already stored with the same content is skipped, so a retry is safe
	Load(ctx context.Context, sessionID string) ([]message.Message, error)      // Seq order, without a torn batch or half line at the end
	List(ctx context.Context, prefix string) ([]string, error)                  // ids of stored sessions starting with prefix, sorted, so side chats are found after a restart
}

// CallStore is implemented by a SessionStore that also keeps each session's model call ledger; without it the ledger lives in memory only.
type CallStore interface {
	AppendCalls(ctx context.Context, sessionID string, calls []json.RawMessage) error // each record is a JSON object with a strictly increasing "seq"; a seq already stored is skipped, so a retry is safe
	LoadCalls(ctx context.Context, sessionID string) ([]json.RawMessage, error)       // seq order, without a half record at the end
}

// LabelStore is implemented by a SessionStore that keeps each session's labels in a record of its own, so they can change and be queried; its calls may overlap a session's other store calls.
type LabelStore interface {
	SaveLabels(ctx context.Context, rec LabelRecord) error                          // replaces the session's record
	LoadLabels(ctx context.Context, sessionID string) (LabelRecord, bool, error)    // false when the session has no record
	FindLabels(ctx context.Context, match map[string]string) ([]LabelRecord, error) // records whose labels hold every pair of match, in session id order
}

// LabelRecord is a session's current labels, kept apart from its append-only file.
type LabelRecord struct {
	SessionID       string            `json:"session_id"`
	ParentSessionID string            `json:"parent_session_id,omitempty"` // a side chat's main session or a child session's owner, so a query can keep to top-level sessions
	Labels          map[string]string `json:"labels,omitempty"`
}

// MatchLabels reports whether labels hold every pair of match; an empty value matches only a missing key, since no label has an empty value.
func MatchLabels(labels, match map[string]string) bool {
	for k, v := range match {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// CallSeq reads a call record's seq, which is all a store needs to know of it.
func CallSeq(rec json.RawMessage) (int64, error) {
	var v struct {
		Seq int64 `json:"seq"`
	}
	if err := json.Unmarshal(rec, &v); err != nil {
		return 0, err
	}
	if v.Seq <= 0 {
		return 0, errors.New("store: call record without a seq")
	}
	return v.Seq, nil
}

var (
	// ErrSeqConflict reports a second writer instead of letting two histories interleave in one session.
	ErrSeqConflict = errors.New("store: seq already holds a different line")
	// ErrIncompatibleStore stops a reader at a line format newer than it knows, instead of dropping fields it cannot see.
	ErrIncompatibleStore = errors.New("store: line has an unsupported format version")
)

// Current returns the lines from the last boundary on, which is the context the next turn sees.
func Current(msgs []message.Message) []message.Message {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Boundary != nil {
			return msgs[i:]
		}
	}
	return msgs
}

// SameLine reports whether a retried line repeats a stored one, ignoring batch markers, so every store implementation skips the same retries.
func SameLine(a, b message.Message) (bool, error) {
	x, err := sameness(a)
	if err != nil {
		return false, err
	}
	y, err := sameness(b)
	if err != nil {
		return false, err
	}
	return bytes.Equal(x, y), nil
}
