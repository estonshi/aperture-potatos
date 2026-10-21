package mcp

import (
	"context"
	"encoding/json"
	"time"

	"github.com/estonshi/aperture-potatos/kernel"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// ElicitInput is a server's request for user input during a call, in the session that made the call.
type ElicitInput struct {
	kernel.HookEnv
	Server  string          `json:"server"`
	Tool    string          `json:"tool,omitempty"`
	Mode    string          `json:"mode"` // form or url
	Message string          `json:"message"`
	Schema  json.RawMessage `json:"requested_schema,omitempty"`
	URL     string          `json:"url,omitempty"`
}

// ElicitOutput is the user's answer; Content goes back to the server only, never to the model.
type ElicitOutput struct {
	Action  string         `json:"action"` // accept, decline or cancel
	Content map[string]any `json:"content,omitempty"`
}

// Elicitation is the mcp_elicitation hook point; with no hook, or a failing one, the request is declined.
var Elicitation = kernel.NewHookPoint(kernel.HookPointDef[ElicitInput, ElicitOutput]{
	Name: "mcp_elicitation",
	Key:  func(in ElicitInput) string { return in.Server },
	Merge: func(acc, out ElicitOutput) (ElicitOutput, bool) {
		if out.Action != "" {
			return out, true
		}
		return acc, false
	},
	Timeout:  10 * time.Minute, // a person answers
	Fail:     kernel.FailClosed,
	Closed:   func(error) ElicitOutput { return ElicitOutput{Action: "decline"} },
	SyncOnly: true,
})

// elicit answers a server's input request through the hook of the session whose call asked for it.
func (c *conn) elicit(ctx context.Context, req *sdk.ElicitRequest) (*sdk.ElicitResult, error) {
	decline := &sdk.ElicitResult{Action: "decline"}
	if req == nil || req.Params == nil {
		return decline, nil
	}
	st := c.caller(ctx)
	if st == nil {
		return decline, nil
	}
	p := req.Params
	mode := p.Mode
	if mode == "" {
		mode = "form"
		if p.URL != "" {
			mode = "url"
		}
	}
	if mode == "url" {
		if err := checkAuthURL(p.URL, false); err != nil {
			return decline, nil
		}
	}
	in := ElicitInput{Server: c.srv.Name, Tool: st.tool.name, Mode: mode, Message: p.Message, URL: p.URL}
	if p.RequestedSchema != nil {
		in.Schema, _ = json.Marshal(p.RequestedSchema)
	}
	s := st.tc.Session()
	info := s.Info()
	in.HookEnv = kernel.HookEnv{Session: s, SessionID: info.SessionID, Agent: info.Agent, Path: st.tc.Path()}
	out, _ := kernel.Fire(ctx, s, Elicitation, in)
	switch out.Action {
	case "accept", "cancel":
	default:
		out = ElicitOutput{Action: "decline"}
	}
	st.tc.Emit(&ElicitationEvent{Server: c.srv.Name, Tool: st.tool.name, Mode: mode, Action: out.Action})
	res := &sdk.ElicitResult{Action: out.Action}
	if out.Action == "accept" {
		res.Content = out.Content
	}
	return res, nil
}

// caller finds the call an input request belongs to: the call's context under multi round-trip requests, otherwise the only call in flight.
func (c *conn) caller(ctx context.Context) *callState {
	if st, ok := ctx.Value(callKey{}).(*callState); ok {
		return st
	}
	if ctx.Value(promptKey{}) != nil {
		return nil // a prompt a command fetches has no call to answer for it, so it is declined
	}
	var only *callState
	n := 0
	c.calls.Range(func(_, v any) bool {
		only = v.(*callState)
		n++
		return n < 2
	})
	if n == 1 {
		return only
	}
	return nil
}
