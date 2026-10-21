package a2a

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"

	"github.com/estonshi/aperture-potatos/ext/peer"
)

// codec turns peer values into one version's request bodies and that version's results and stream events back into objects.
type codec interface {
	// send is the params of a send; a non-streaming send asks to return at once, since the client polls.
	send(m peer.Message, stream bool, historyLength int, tenant string) any
	// task is the params of a request about one task; a negative history length leaves it out, as cancel and subscribe take none.
	task(id string, historyLength int, tenant string) any
	// query is the history length of a GetTask over HTTP+JSON, empty when it is not sent.
	query(historyLength int) string
	decode(raw json.RawMessage) (object, error)
}

type objectKind int

const (
	objTask objectKind = iota + 1
	objMessage
	objStatus
	objArtifact
)

// object is one result or stream event in the client's terms.
type object struct {
	kind      objectKind
	taskID    string
	contextID string
	state     peer.TaskState
	message   *peer.Message // the status message, or the message of an objMessage
	artifacts []peer.Artifact
	artifact  peer.Artifact
	append    bool
	lastChunk bool
	final     bool // v0.3's last event of the interaction
}

var errUndecodable = errors.New("a2a: the peer sent an object the client does not understand")

// state maps a wire state to the peer's; unknown, unspecified and unrecognized values fail the task, the raw value kept in the message.
func state(raw string) (peer.TaskState, bool) {
	s := strings.ToLower(strings.TrimPrefix(strings.TrimSpace(raw), "TASK_STATE_"))
	switch strings.ReplaceAll(s, "-", "_") {
	case "submitted":
		return peer.StateSubmitted, true
	case "working":
		return peer.StateWorking, true
	case "input_required":
		return peer.StateInputRequired, true
	case "auth_required":
		return peer.StateAuthRequired, true
	case "completed":
		return peer.StateCompleted, true
	case "canceled", "cancelled":
		return peer.StateCanceled, true
	case "failed":
		return peer.StateFailed, true
	case "rejected":
		return peer.StateRejected, true
	}
	return peer.StateFailed, false
}

// statusOf reads a status object; a state the client does not know fails the task, saying what the peer reported.
func statusOf(raw json.RawMessage, msg func(json.RawMessage) (*peer.Message, error)) (peer.TaskState, *peer.Message, error) {
	var s struct {
		State   string          `json:"state"`
		Message json.RawMessage `json:"message"`
		Update  json.RawMessage `json:"update"`
	}
	if len(raw) == 0 {
		return "", nil, nil
	}
	if err := json.Unmarshal(raw, &s); err != nil {
		return "", nil, err
	}
	body := s.Message
	if len(body) == 0 {
		body = s.Update
	}
	var m *peer.Message
	if len(body) > 0 && string(body) != "null" {
		var err error
		if m, err = msg(body); err != nil {
			return "", nil, err
		}
	}
	st, ok := state(s.State)
	if !ok {
		note := `the peer reported the unknown task state "` + s.State + `"`
		if m == nil {
			m = &peer.Message{Role: "agent"}
		}
		m.Parts = append(m.Parts, peer.Part{Text: note})
	}
	return st, m, nil
}

// partJSON accepts every part shape both versions use: v1.0's members, v0.3's kind objects and v0.3's ProtoJSON oneofs.
type partJSON struct {
	Kind      string          `json:"kind"`
	Text      *string         `json:"text"`
	Raw       string          `json:"raw"`
	URL       string          `json:"url"`
	Data      json.RawMessage `json:"data"`
	MediaType string          `json:"mediaType"`
	Filename  string          `json:"filename"`
	File      *struct {
		Bytes         string `json:"bytes"`
		URI           string `json:"uri"`
		FileWithBytes string `json:"fileWithBytes"`
		FileWithURI   string `json:"fileWithUri"`
		MimeType      string `json:"mimeType"`
		Name          string `json:"name"`
	} `json:"file"`
}

func decodePart(raw json.RawMessage, protoData bool) (peer.Part, error) {
	var p partJSON
	if err := json.Unmarshal(raw, &p); err != nil {
		return peer.Part{}, err
	}
	switch {
	case p.Text != nil:
		return peer.Part{Text: *p.Text, MediaType: p.MediaType}, nil
	case p.File != nil:
		out := peer.Part{MediaType: p.File.MimeType, Filename: p.File.Name}
		b64 := p.File.Bytes + p.File.FileWithBytes
		if uri := p.File.URI + p.File.FileWithURI; uri != "" {
			out.URL = uri
		} else if b, err := base64.StdEncoding.DecodeString(b64); err == nil {
			out.Raw = b
		}
		return out, nil
	case p.Raw != "":
		b, err := base64.StdEncoding.DecodeString(p.Raw)
		if err != nil {
			return peer.Part{}, err
		}
		return peer.Part{Raw: b, MediaType: p.MediaType, Filename: p.Filename}, nil
	case p.URL != "":
		return peer.Part{URL: p.URL, MediaType: p.MediaType, Filename: p.Filename}, nil
	case len(p.Data) > 0:
		data := p.Data
		if protoData && p.Kind == "" { // v0.3 ProtoJSON wraps the value: {"data":{"data":{…}}}
			var inner struct {
				Data json.RawMessage `json:"data"`
			}
			if json.Unmarshal(p.Data, &inner) == nil && len(inner.Data) > 0 {
				data = inner.Data
			}
		}
		return peer.Part{Data: data, MediaType: p.MediaType}, nil
	}
	return peer.Part{Text: ""}, nil
}

func decodeParts(list []json.RawMessage, protoData bool) ([]peer.Part, error) {
	out := make([]peer.Part, 0, len(list))
	for _, raw := range list {
		p, err := decodePart(raw, protoData)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, nil
}

// messageJSON accepts both versions' messages; v0.3 ProtoJSON names the parts content.
type messageJSON struct {
	MessageID string                     `json:"messageId"`
	ContextID string                     `json:"contextId"`
	TaskID    string                     `json:"taskId"`
	Role      string                     `json:"role"`
	Parts     []json.RawMessage          `json:"parts"`
	Content   []json.RawMessage          `json:"content"`
	Metadata  map[string]json.RawMessage `json:"metadata"`
}

func decodeMessage(raw json.RawMessage, protoData bool) (*peer.Message, error) {
	var m messageJSON
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, err
	}
	list := m.Parts
	if len(list) == 0 {
		list = m.Content
	}
	parts, err := decodeParts(list, protoData)
	if err != nil {
		return nil, err
	}
	role := strings.ToLower(strings.TrimPrefix(m.Role, "ROLE_"))
	return &peer.Message{ID: m.MessageID, ContextID: m.ContextID, TaskID: m.TaskID, Role: role, Parts: parts, Metadata: m.Metadata}, nil
}

type artifactJSON struct {
	ArtifactID string            `json:"artifactId"`
	Name       string            `json:"name"`
	Parts      []json.RawMessage `json:"parts"`
}

func decodeArtifact(raw json.RawMessage, protoData bool) (peer.Artifact, error) {
	var a artifactJSON
	if err := json.Unmarshal(raw, &a); err != nil {
		return peer.Artifact{}, err
	}
	parts, err := decodeParts(a.Parts, protoData)
	return peer.Artifact{ID: a.ArtifactID, Name: a.Name, Parts: parts}, err
}

// taskJSON accepts both versions' tasks.
type taskJSON struct {
	ID        string            `json:"id"`
	ContextID string            `json:"contextId"`
	Status    json.RawMessage   `json:"status"`
	Artifacts []json.RawMessage `json:"artifacts"`
}

func decodeTask(raw json.RawMessage, protoData bool) (object, error) {
	var t taskJSON
	if err := json.Unmarshal(raw, &t); err != nil {
		return object{}, err
	}
	st, msg, err := statusOf(t.Status, func(b json.RawMessage) (*peer.Message, error) { return decodeMessage(b, protoData) })
	if err != nil {
		return object{}, err
	}
	o := object{kind: objTask, taskID: t.ID, contextID: t.ContextID, state: st, message: msg}
	for _, a := range t.Artifacts {
		art, err := decodeArtifact(a, protoData)
		if err != nil {
			return object{}, err
		}
		o.artifacts = append(o.artifacts, art)
	}
	return o, nil
}

type statusUpdateJSON struct {
	TaskID    string          `json:"taskId"`
	ContextID string          `json:"contextId"`
	Status    json.RawMessage `json:"status"`
	Final     bool            `json:"final"`
}

func decodeStatusUpdate(raw json.RawMessage, protoData bool) (object, error) {
	var s statusUpdateJSON
	if err := json.Unmarshal(raw, &s); err != nil {
		return object{}, err
	}
	st, msg, err := statusOf(s.Status, func(b json.RawMessage) (*peer.Message, error) { return decodeMessage(b, protoData) })
	if err != nil {
		return object{}, err
	}
	return object{kind: objStatus, taskID: s.TaskID, contextID: s.ContextID, state: st, message: msg, final: s.Final}, nil
}

type artifactUpdateJSON struct {
	TaskID    string          `json:"taskId"`
	ContextID string          `json:"contextId"`
	Artifact  json.RawMessage `json:"artifact"`
	Append    bool            `json:"append"`
	LastChunk bool            `json:"lastChunk"`
}

func decodeArtifactUpdate(raw json.RawMessage, protoData bool) (object, error) {
	var a artifactUpdateJSON
	if err := json.Unmarshal(raw, &a); err != nil {
		return object{}, err
	}
	art, err := decodeArtifact(a.Artifact, protoData)
	if err != nil {
		return object{}, err
	}
	return object{kind: objArtifact, taskID: a.TaskID, contextID: a.ContextID, artifact: art, append: a.Append, lastChunk: a.LastChunk}, nil
}

// unwrap reads the one-member wrapper of ProtoJSON results and stream events: {task}, {message}, {statusUpdate} or {artifactUpdate}.
func unwrap(raw json.RawMessage, protoData bool) (object, bool, error) {
	var w map[string]json.RawMessage
	if json.Unmarshal(raw, &w) != nil {
		return object{}, false, nil
	}
	for key, body := range w {
		switch key {
		case "task":
			o, err := decodeTask(body, protoData)
			return o, true, err
		case "message", "msg":
			m, err := decodeMessage(body, protoData)
			if err != nil {
				return object{}, true, err
			}
			return object{kind: objMessage, taskID: m.TaskID, contextID: m.ContextID, message: m}, true, nil
		case "statusUpdate", "status_update":
			o, err := decodeStatusUpdate(body, protoData)
			return o, true, err
		case "artifactUpdate", "artifact_update":
			o, err := decodeArtifactUpdate(body, protoData)
			return o, true, err
		}
	}
	return object{}, false, nil
}

func encodeBytes(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
