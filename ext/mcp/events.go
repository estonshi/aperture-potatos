package mcp

import "github.com/estonshi/aperture-potatos/event"

// ServerStatusEvent is mcp.server_status: a connection changed state; Error names only the kind of failure.
type ServerStatusEvent struct {
	Server string `json:"server"`
	State  string `json:"state"` // connecting, ready, failed or closed
	Tools  int    `json:"tools"`
	Error  string `json:"error,omitempty"`
}

func (*ServerStatusEvent) EventType() event.Type { return "mcp.server_status" }

// AuthRequiredEvent is mcp.auth_required: the user has to sign in at AuthURL, or signing in failed.
type AuthRequiredEvent struct {
	Server  string `json:"server"`
	State   string `json:"state"` // waiting or failed
	AuthURL string `json:"auth_url,omitempty"`
}

func (*AuthRequiredEvent) EventType() event.Type { return "mcp.auth_required" }

// ElicitationEvent is mcp.elicitation: a server asked the user for input during a call; it never carries what the user gave.
type ElicitationEvent struct {
	Server string `json:"server"`
	Tool   string `json:"tool,omitempty"`
	Mode   string `json:"mode"` // form or url
	Action string `json:"action"`
}

func (*ElicitationEvent) EventType() event.Type { return "mcp.elicitation" }

// Payloads teaches an event.Decoder this extension's events.
func Payloads() []event.Constructor {
	return []event.Constructor{
		{Type: "mcp.server_status", New: func() event.Payload { return &ServerStatusEvent{} }},
		{Type: "mcp.auth_required", New: func() event.Payload { return &AuthRequiredEvent{} }},
		{Type: "mcp.elicitation", New: func() event.Payload { return &ElicitationEvent{} }},
	}
}
