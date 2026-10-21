package kernel

import (
	"context"
	"encoding/json"
	"maps"
	"sync/atomic"

	"github.com/estonshi/aperture-potatos/event"
)

// ToolContext accessors stay valid for the session's lifetime; Progress and Emit only while Call runs.
type ToolContext struct {
	s       *Session
	ctx     context.Context
	callID  string
	turnID  string
	step    int
	attempt int
	meta    map[string]string
	task    atomic.Pointer[taskRuntime] // the tool_exec attempt running this call, if any; set mid-call when the host moves the call to the background
	live    atomic.Bool
}

// CallID is the model's id for this call, as the tool.* events and the history carry it.
func (tc *ToolContext) CallID() string { return tc.callID }

// Session is the session the call runs in; only its non-waiting methods are safe here.
func (tc *ToolContext) Session() *Session { return tc.s }

// Path is the hops from the root session to the call: the tasks that own this session, then the task's hop for a tool_exec run.
func (tc *ToolContext) Path() []event.Hop {
	path := tc.s.rootPath()
	if rt := tc.task.Load(); rt != nil {
		path = append(path, rt.hop())
	}
	return path
}

// InputMetadata is the merged Metadata of the inputs that started this turn, later ones winning a shared key, and nil for a background run.
func (tc *ToolContext) InputMetadata() map[string]string { return maps.Clone(tc.meta) }

// Progress emits tool.progress; it waits for backpressure credit like any bulk event, bounded by the call's context.
func (tc *ToolContext) Progress(text string, data any) {
	if !tc.live.Load() {
		return
	}
	var raw json.RawMessage
	if data != nil {
		raw, _ = json.Marshal(data)
	}
	if rt := tc.task.Load(); rt != nil {
		rt.Report(TaskProgress{Text: text, Data: raw}) // a background run reports through its task, which is what the model is told about
		return
	}
	if tc.s.bus.WaitWritable(tc.ctx) != nil {
		return
	}
	tc.s.publishAt(&event.ToolProgress{CallID: tc.callID, Text: text, Data: raw}, tc.turnID, tc.step, tc.attempt)
}

// Emit publishes an extension event attributed to this call.
func (tc *ToolContext) Emit(p event.Payload) {
	rt := tc.task.Load()
	switch {
	case !tc.live.Load():
	case rt != nil:
		tc.s.publishUnder(p, []event.Hop{rt.hop()})
	default:
		tc.s.publishAt(p, tc.turnID, tc.step, tc.attempt)
	}
}
