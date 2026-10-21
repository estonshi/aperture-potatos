package a2a

import (
	"encoding/json"
	"strconv"

	"github.com/estonshi/aperture-potatos/ext/peer"
)

// codec03 is A2A v0.3: kind-tagged objects over JSON-RPC, the v0.3 proto's ProtoJSON over HTTP+JSON.
type codec03 struct {
	proto bool
}

type message03 struct {
	Kind      string                     `json:"kind,omitempty"`
	MessageID string                     `json:"messageId"`
	ContextID string                     `json:"contextId,omitempty"`
	TaskID    string                     `json:"taskId,omitempty"`
	Role      string                     `json:"role"`
	Parts     []any                      `json:"parts,omitempty"`
	Content   []any                      `json:"content,omitempty"`
	Metadata  map[string]json.RawMessage `json:"metadata,omitempty"`
}

func (c codec03) message(m peer.Message) message03 {
	out := message03{MessageID: m.ID, ContextID: m.ContextID, TaskID: m.TaskID, Metadata: m.Metadata}
	parts := make([]any, 0, len(m.Parts))
	for _, p := range m.Parts {
		parts = append(parts, c.part(p))
	}
	if c.proto {
		out.Role, out.Content = "ROLE_"+upper(m.Role), parts
		return out
	}
	out.Kind, out.Role, out.Parts = "message", m.Role, parts
	if out.Parts == nil {
		out.Parts = []any{}
	}
	return out
}

func (c codec03) part(p peer.Part) any {
	switch {
	case p.Raw != nil || p.URL != "":
		if c.proto {
			f := map[string]string{}
			if p.URL != "" {
				f["fileWithUri"] = p.URL
			} else {
				f["fileWithBytes"] = encodeBytes(p.Raw)
			}
			if p.MediaType != "" {
				f["mimeType"] = p.MediaType
			}
			return map[string]any{"file": f}
		}
		f := map[string]string{}
		if p.URL != "" {
			f["uri"] = p.URL
		} else {
			f["bytes"] = encodeBytes(p.Raw)
		}
		if p.MediaType != "" {
			f["mimeType"] = p.MediaType
		}
		if p.Filename != "" {
			f["name"] = p.Filename
		}
		return map[string]any{"kind": "file", "file": f}
	case p.Data != nil:
		data := objectData(p.Data)
		if c.proto {
			return map[string]any{"data": map[string]json.RawMessage{"data": data}}
		}
		return map[string]any{"kind": "data", "data": data}
	}
	if c.proto {
		return map[string]string{"text": p.Text}
	}
	return map[string]string{"kind": "text", "text": p.Text}
}

// objectData wraps a value that is not an object, since v0.3's data part holds only objects.
func objectData(raw json.RawMessage) json.RawMessage {
	var probe map[string]json.RawMessage
	if json.Unmarshal(raw, &probe) == nil {
		return raw
	}
	b, _ := json.Marshal(map[string]json.RawMessage{"value": raw})
	return b
}

// configuration03 always says blocking, because the two versions name the switch in opposite senses; a history length of 0 means unlimited in v0.3, so it is never sent.
type configuration03 struct {
	Blocking      *bool `json:"blocking,omitempty"`
	HistoryLength int   `json:"historyLength,omitempty"`
}

func (c codec03) send(m peer.Message, stream bool, historyLength int, _ string) any {
	p := struct {
		Message       message03        `json:"message"`
		Configuration *configuration03 `json:"configuration,omitempty"`
	}{Message: c.message(m)}
	conf := &configuration03{HistoryLength: historyLength}
	if !stream {
		no := false
		conf.Blocking = &no
	}
	if conf.Blocking != nil || conf.HistoryLength > 0 {
		p.Configuration = conf
	}
	return p
}

func (codec03) task(id string, historyLength int, _ string) any {
	return struct {
		ID            string `json:"id"`
		HistoryLength int    `json:"historyLength,omitempty"`
	}{id, max(historyLength, 0)}
}

func (codec03) query(historyLength int) string {
	if historyLength <= 0 {
		return ""
	}
	return "historyLength=" + strconv.Itoa(historyLength)
}

// decode accepts both v0.3 shapes on both bindings: the ProtoJSON wrapper, or a kind-tagged object, inferring a missing kind from its members.
func (c codec03) decode(raw json.RawMessage) (object, error) {
	if o, ok, err := unwrap(raw, true); ok {
		return o, err
	}
	var probe struct {
		Kind      string          `json:"kind"`
		ID        string          `json:"id"`
		MessageID string          `json:"messageId"`
		Status    json.RawMessage `json:"status"`
		Artifact  json.RawMessage `json:"artifact"`
	}
	if err := json.Unmarshal(raw, &probe); err != nil {
		return object{}, err
	}
	kind := probe.Kind
	if kind == "" {
		switch {
		case probe.Artifact != nil:
			kind = "artifact-update"
		case probe.Status != nil && probe.ID != "":
			kind = "task"
		case probe.Status != nil:
			kind = "status-update"
		case probe.MessageID != "":
			kind = "message"
		}
	}
	switch kind {
	case "task":
		return decodeTask(raw, c.proto)
	case "message":
		m, err := decodeMessage(raw, c.proto)
		if err != nil {
			return object{}, err
		}
		return object{kind: objMessage, taskID: m.TaskID, contextID: m.ContextID, message: m}, nil
	case "status-update":
		return decodeStatusUpdate(raw, c.proto)
	case "artifact-update":
		return decodeArtifactUpdate(raw, c.proto)
	}
	return object{}, errUndecodable
}

func upper(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'a' && c <= 'z' {
			b[i] = c - 32
		}
	}
	return string(b)
}
