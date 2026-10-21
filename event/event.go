// Package event holds the event envelope and the kernel payloads; subscribers receive these values and decode them with a Decoder.
package event

import (
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type Type string

type Event struct {
	Type      Type      `json:"type"` // "domain.action", e.g. message.delta
	Seq       uint64    `json:"seq"`  // per root session, starts at 1; no order across sessions
	Time      time.Time `json:"time"`
	SessionID string    `json:"session_id"` // emitting session
	Agent     string    `json:"agent"`      // emitting agent's nickname
	TurnID    string    `json:"turn_id,omitempty"`
	Step      int       `json:"step,omitempty"`
	Attempt   int       `json:"attempt,omitempty"`
	Path      []Hop     `json:"path,omitempty"` // nesting from the subscriber's session to the emitter; empty = own loop
	Data      Payload   `json:"data"`
}

// Hop describes one level of nesting; the task manager writes kind "task", other kinds such as "side" are labels chosen by the caller.
type Hop struct {
	Kind      string `json:"kind"`
	ID        string `json:"id"`
	Name      string `json:"name,omitempty"`
	SessionID string `json:"session_id"`        // session this hop hangs from
	CallID    string `json:"call_id,omitempty"` // parent tool call that spawned it
	TurnID    string `json:"turn_id,omitempty"` // parent turn that spawned it
	Seq       uint64 `json:"seq,omitempty"`     // seq on the downstream bus before relay
}

// Payload is implemented by every concrete event data struct.
type Payload interface{ EventType() Type }

// Bulk marks high-frequency payloads that take backpressure credit and may be dropped by DropDeltas.
type Bulk interface{ Bulk() bool }

// Unknown holds a payload whose type this binding does not know; consumers must ignore it.
type Unknown struct {
	TypeName Type
	Raw      json.RawMessage
}

func (u *Unknown) EventType() Type { return u.TypeName }

// MarshalJSON writes the payload back verbatim, so relaying an unknown event loses nothing.
func (u *Unknown) MarshalJSON() ([]byte, error) {
	if len(u.Raw) == 0 {
		return []byte("{}"), nil
	}
	return u.Raw, nil
}

// Constructor lets an extension teach a Decoder its payload types; there is no process-wide registry.
type Constructor struct {
	Type Type
	New  func() Payload
}

// Decoder turns event JSON back into typed payloads.
type Decoder struct {
	types map[Type]func() Payload
}

// NewDecoder knows every kernel payload plus extra; kernel types win, so an extension cannot change how kernel events decode.
func NewDecoder(extra ...Constructor) *Decoder {
	d := &Decoder{types: make(map[Type]func() Payload, len(kernelPayloads)+len(extra))}
	for _, c := range extra {
		d.types[c.Type] = c.New
	}
	for _, c := range kernelPayloads {
		d.types[c.Type] = c.New
	}
	return d
}

func (d *Decoder) Decode(b []byte) (Event, error) {
	var env struct {
		Event
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(b, &env); err != nil {
		return Event{}, err
	}
	ev := env.Event
	if ev.Type == "" {
		return Event{}, errors.New("event: missing type")
	}
	newPayload, ok := d.types[ev.Type]
	if !ok {
		ev.Data = &Unknown{TypeName: ev.Type, Raw: append(json.RawMessage(nil), env.Data...)}
		return ev, nil
	}
	p := newPayload()
	if len(env.Data) > 0 && string(env.Data) != "null" {
		if err := json.Unmarshal(env.Data, p); err != nil {
			return Event{}, fmt.Errorf("event: %s data: %w", ev.Type, err)
		}
	}
	ev.Data = p
	return ev, nil
}
