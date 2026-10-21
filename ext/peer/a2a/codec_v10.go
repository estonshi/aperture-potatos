package a2a

import (
	"encoding/json"
	"strconv"

	"github.com/estonshi/aperture-potatos/ext/peer"
)

// codec10 is A2A v1.0, the same ProtoJSON on both bindings.
type codec10 struct{}

type message10 struct {
	MessageID string                     `json:"messageId"`
	ContextID string                     `json:"contextId,omitempty"`
	TaskID    string                     `json:"taskId,omitempty"`
	Role      string                     `json:"role"`
	Parts     []part10                   `json:"parts"`
	Metadata  map[string]json.RawMessage `json:"metadata,omitempty"`
}

type part10 struct {
	Text      *string         `json:"text,omitempty"`
	Raw       string          `json:"raw,omitempty"`
	URL       string          `json:"url,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
	MediaType string          `json:"mediaType,omitempty"`
	Filename  string          `json:"filename,omitempty"`
}

func (codec10) message(m peer.Message) message10 {
	out := message10{MessageID: m.ID, ContextID: m.ContextID, TaskID: m.TaskID, Role: "ROLE_" + upper(m.Role), Parts: []part10{}, Metadata: m.Metadata}
	for _, p := range m.Parts {
		q := part10{MediaType: p.MediaType, Filename: p.Filename}
		switch {
		case p.Raw != nil:
			q.Raw = encodeBytes(p.Raw)
		case p.URL != "":
			q.URL = p.URL
		case p.Data != nil:
			q.Data = p.Data
		default:
			t := p.Text
			q.Text = &t
		}
		out.Parts = append(out.Parts, q)
	}
	return out
}

// configuration10 always says returnImmediately for a send, and sends a history length of 0, which in v1.0 means none.
type configuration10 struct {
	ReturnImmediately *bool `json:"returnImmediately,omitempty"`
	HistoryLength     int   `json:"historyLength"`
}

func (c codec10) send(m peer.Message, stream bool, historyLength int, tenant string) any {
	conf := &configuration10{HistoryLength: historyLength}
	if !stream {
		yes := true
		conf.ReturnImmediately = &yes
	}
	return struct {
		Tenant        string           `json:"tenant,omitempty"`
		Message       message10        `json:"message"`
		Configuration *configuration10 `json:"configuration"`
	}{tenant, c.message(m), conf}
}

func (codec10) task(id string, historyLength int, tenant string) any {
	p := struct {
		Tenant        string `json:"tenant,omitempty"`
		ID            string `json:"id"`
		HistoryLength *int   `json:"historyLength,omitempty"`
	}{Tenant: tenant, ID: id}
	if historyLength >= 0 {
		p.HistoryLength = &historyLength
	}
	return p
}

func (codec10) query(historyLength int) string { return "historyLength=" + strconv.Itoa(historyLength) }

func (codec10) decode(raw json.RawMessage) (object, error) {
	if o, ok, err := unwrap(raw, false); ok {
		return o, err
	}
	return codec03{}.decode(raw) // a v1.0 server answering in v0.3's shape, as one that took the request for 0.3 would
}
