package kernel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/internal/prompt"
	"github.com/estonshi/aperture-potatos/internal/toolexec"
	"github.com/estonshi/aperture-potatos/message"
)

// toolExecInput is tool_exec's input: the registered tool and its own input, never with the background switch.
type toolExecInput struct {
	Tool  string          `json:"tool"`
	Input json.RawMessage `json:"input"`
}

func parseToolExec(raw json.RawMessage) (toolExecInput, error) {
	var in toolExecInput
	if err := json.Unmarshal(raw, &in); err != nil || in.Tool == "" {
		return in, errors.New(`tool_exec input must be {"tool": <name>, "input": {...}}`)
	}
	if len(in.Input) == 0 {
		in.Input = json.RawMessage(`{}`)
	}
	norm, err := normalizeInput(in.Input)
	if err != nil {
		return in, fmt.Errorf("tool_exec input for %s: %s", in.Tool, err.(*InputError).Detail)
	}
	in.Input = norm
	return in, nil
}

func toolExecSpec(tool string, input json.RawMessage) json.RawMessage {
	b, _ := json.Marshal(toolExecInput{Tool: tool, Input: input})
	return b
}

// toolExecKind is the kernel's own kind; it takes no instructions, because a running tool call cannot take any.
func (s *Session) toolExecKind() TaskKind {
	return TaskKind{Name: ToolExecKind, Description: "Runs a registered tool in the background.", Caps: TaskCaps{Restart: true}, New: func(spec TaskSpec) (Task, error) {
		in, err := parseToolExec(spec.Input)
		if err != nil {
			return nil, err
		}
		return &toolExec{s: s, in: in}, nil
	}}
}

type toolExec struct {
	s  *Session
	in toolExecInput
}

// Run reviews a host start as the model's background call would have been, runs the tool under the task's hop, and ends as the tool's result says.
func (x *toolExec) Run(ctx context.Context, rt TaskRuntime) (TaskResult, error) {
	s, r := x.s, rt.(*taskRuntime)
	tool, spec, ok := s.liveTool(x.in.Tool)
	switch {
	case !ok:
		return TaskResult{}, fmt.Errorf("no tool named %s is registered", x.in.Tool)
	case s.allowed != nil && !s.allowed[x.in.Tool]:
		return TaskResult{}, errors.New(s.textf("tool_not_allowed", map[string]string{"name": x.in.Tool}))
	}
	call := message.ToolCallBlock{ID: s.agent.cfg.ids.NewID("tcall"), Name: x.in.Tool, Input: x.in.Input}
	var approved bool
	_ = s.exec(func(st *state) {
		if e := r.current(st); e != nil {
			approved = e.approved
		}
	})
	if !approved {
		gated, res, run := s.gateRun(ctx, r, call, spec)
		if !run {
			return toolExecResult(res)
		}
		_, call.Input, _ = takeBackground(gated.Input)
		if bytes.Equal(call.Input, x.in.Input) { // a rewritten input was not what the task records, so a restart must review it again
			s.post(func(st *state) {
				if e := r.current(st); e != nil {
					e.approved = true
				}
			})
		}
	}
	return toolExecResult(s.execTool(ctx, r, call, tool, spec))
}

// toolExecResult ends the task as the tool's result says: an error result fails it with the result's text as the error, which would only repeat as a summary.
func toolExecResult(res message.ToolResultBlock) (TaskResult, error) {
	if !res.IsError {
		return TaskResult{Content: res.Content, Data: res.Structured}, nil
	}
	text := message.Message{Content: res.Content}.Text()
	if text == "" {
		text = string(res.Outcome)
	}
	return TaskResult{Data: res.Structured}, errors.New(text)
}

// liveTool is the tool a name means now, session scope shadowing agent scope, as the next freeze would see it.
func (s *Session) liveTool(name string) (Tool, ToolSpec, bool) {
	sess := s.reg.state()
	r, ok := sess.tools[name]
	if !ok {
		if r, ok = s.outer().tools[name]; !ok || sess.hides(KindTool, name) {
			return nil, ToolSpec{}, false
		}
	}
	spec, ok := s.toolSpec(r.tool)
	if !ok {
		return nil, ToolSpec{}, false
	}
	spec.Name = name
	if spec.Background {
		if bg, err := withBackground(spec.InputSchema, spec.Strict); err == nil {
			spec.InputSchema = bg
		}
	}
	return r.tool, spec, true
}

// gateRun reviews a host start under the real tool name with "background": true last, the view the model's own call has; run reports that the tool may run.
func (s *Session) gateRun(ctx context.Context, rt *taskRuntime, call message.ToolCallBlock, spec ToolSpec) (gated message.ToolCallBlock, res message.ToolResultBlock, run bool) {
	call.Input, _ = withKeyLast(call.Input, "background", json.RawMessage("true"))
	path := []event.Hop{rt.hop()}
	env := s.env(rt.turnID, 0)
	env.Path = append(env.Path, path...)
	scope := func(id string) *hookScope { return &hookScope{turnID: rt.turnID, callID: id, path: path} }
	hctx := withHookScope(ctx, scope(call.ID))
	sc := s.screen(ctx, hctx, env, call, spec)
	switch sc.kind {
	case screenRun:
		return sc.call, res, true
	case screenInterrupted:
		return call, toolexec.NotStarted(call, s.agent.cfg.texts), false // the attempt was asked to stop, so it ends canceled whatever this says
	case screenInvalid:
		return call, sc.result, false
	case screenSettled:
		res, _ = postToolUse(hctx, s, env, call, sc.result, 0)
		return call, res, false
	}
	rs := reviewScope{env: env, publish: func(p event.Payload) { s.publishUnder(p, path) }, hooks: scope}
	outs, _ := s.review(ctx, rs, []askCall{sc.ask})
	if o := outs[0]; o.approved {
		c := sc.ask.call
		c.Input = o.input
		return c, res, true
	}
	res = outs[0].result
	if res.Outcome == message.OutcomeDenied {
		res, _ = postToolUse(hctx, s, env, call, res, 0)
	}
	return call, res, false
}

// execTool runs the wrapped tool under the task's hop: only the tool's own explicit timeout applies, and post_tool_use sees the real result with nothing that ends a turn.
func (s *Session) execTool(ctx context.Context, rt *taskRuntime, call message.ToolCallBlock, tool Tool, spec ToolSpec) message.ToolResultBlock {
	path := []event.Hop{rt.hop()}
	scope := &hookScope{turnID: rt.turnID, callID: call.ID, path: path}
	env := s.env(rt.turnID, 0)
	env.Path = append(env.Path, path...)
	cctx := ctx
	if spec.Timeout > 0 {
		var cancel context.CancelFunc
		cctx, cancel = context.WithTimeout(ctx, spec.Timeout)
		defer cancel()
	}
	tctx := withHookScope(cctx, scope) // the task's own marker stays, so the tool's waits on this session are re-entrant as the task's are
	began, started := time.Now(), s.agent.cfg.clock.Now()
	s.publishUnder(&event.ToolStarted{CallID: call.ID, Name: call.Name, Input: call.Input}, path)
	tc := &ToolContext{s: s, ctx: tctx, callID: call.ID, turnID: rt.turnID}
	tc.task.Store(rt)
	tc.live.Store(true)
	res, err := callTool(tctx, tool, tc, append(json.RawMessage(nil), call.Input...))
	tc.live.Store(false)
	conv := &toolRunner{s: s, turnCtx: ctx, tools: map[string]frozenTool{call.Name: {tool: tool, spec: spec}}}
	out := conv.convert(cctx, call, res, err)
	out, _ = postToolUse(withHookScope(ctx, scope), s, env, call, out, time.Since(began))
	pub := publicMessage(message.Message{Content: out.Content})
	s.publishUnder(&event.ToolCompleted{CallID: call.ID, Name: call.Name, Outcome: out.Outcome, IsError: out.IsError, Content: pub.Content, Structured: out.Structured, DurationMs: s.agent.cfg.clock.Now().Sub(started).Milliseconds(), OriginalChars: conv.cutChars(call.ID)}, path)
	return out
}

// divert starts each approved background:true call of a Background tool as a tool_exec task, so it takes no worker and its call returns at once.
func (r *toolRunner) divert(ctx context.Context, results []message.ToolResultBlock, runnable []message.ToolCallBlock, index []int) ([]message.ToolCallBlock, []int) {
	var keep []message.ToolCallBlock
	var at []int
	for k, c := range runnable {
		if !r.tools[c.Name].spec.Background || ctx.Err() != nil {
			keep, at = append(keep, c), append(at, index[k])
			continue
		}
		bg, rest, err := takeBackground(c.Input)
		var ie *InputError
		switch {
		case errors.As(err, &ie):
			results[index[k]] = kernelResult(c, message.OutcomeInvalidInput, r.s.textf("invalid_input_detail", map[string]string{"detail": ie.Detail}))
		case !bg:
			keep, at = append(keep, c), append(at, index[k])
		default:
			results[index[k]] = r.startBackground(ctx, c, rest, index[k])
		}
	}
	return keep, at
}

// startBackground is a diverted call: tool events like any call's, around a start made with the call's context so the task is recorded against it.
func (r *toolRunner) startBackground(ctx context.Context, c message.ToolCallBlock, input json.RawMessage, group int) message.ToolResultBlock {
	s := r.s
	sink := &hookSink{}
	r.mu.Lock()
	r.starts[c.ID] = s.agent.cfg.clock.Now()
	r.sinks[c.ID] = sink
	r.mu.Unlock()
	r.publish(&event.ToolStarted{CallID: c.ID, Name: c.Name, Input: c.Input, Group: group})
	scope := &hookScope{turnID: r.turnID, step: r.step, attempt: r.attempt, callID: c.ID, sink: sink}
	cctx := withMarker(withHookScope(ctx, scope), marker{chain: s.chain, kind: "tool", call: c.ID})
	info, _, err := s.tasks.start(cctx, TaskSpec{Kind: ToolExecKind, Title: c.Name, Input: toolExecSpec(c.Name, input)}, true)
	res := startedResult(s.agent.cfg.texts, info, false)
	if err != nil {
		res = s.TaskErrorResult(err)
	}
	out := r.convert(ctx, c, res, nil)
	r.Completed(c, out)
	return out
}

// restartView is what a restart is reviewed as: the kind's start tool, or for tool_exec the wrapped tool, and the input that tool takes.
func (s *Session) restartView(kind string, input json.RawMessage) (string, json.RawMessage) {
	if kind == ToolExecKind {
		if in, err := parseToolExec(input); err == nil {
			return in.Tool, in.Input
		}
	}
	if f := s.frozen.Load(); f != nil && f.starts[kind] != "" {
		return f.starts[kind], input
	}
	return "task:" + kind, input
}

// restartInput is a restart's new spec input; a tool_exec restart takes the wrapped tool's own input and cannot change the tool.
func restartInput(texts prompt.Source, spec TaskSpec, input json.RawMessage) (json.RawMessage, error) {
	if spec.Kind != ToolExecKind {
		return input, nil
	}
	cur, err := parseToolExec(spec.Input)
	if err != nil {
		return nil, err
	}
	norm, err := normalizeInput(input)
	if err != nil {
		return nil, &TaskDeniedError{Text: prompt.TextfFrom(texts, "invalid_input_detail", map[string]string{"detail": err.(*InputError).Detail})}
	}
	_, rest, _ := takeBackground(norm)
	return toolExecSpec(cur.Tool, rest), nil
}

// waitsForTask says whether a call of a kind's start tool waits for its task, which makes it movable.
func waitsForTask(spec ToolSpec, input json.RawMessage) bool {
	if spec.taskKind == "" {
		return false
	}
	bg, _, err := takeBackground(input)
	return err == nil && !bg
}

// movableCall is a running call of a Background tool that BackgroundCall can hand to a task.
type movableCall struct {
	r       *toolRunner
	call    message.ToolCallBlock
	input   json.RawMessage // the final input without background, which the task records
	ft      frozenTool
	tc      *ToolContext
	tctx    context.Context // the tool's context, whose end says why the tool stopped
	scope   *hookScope
	mv      *toolexec.Move
	began   time.Time
	started chan struct{}   // closed once tool.started is out, so a move never completes the call before it started
	done    chan toolReturn // the tool's return values once the call has moved
}

type toolReturn struct {
	res ToolResult
	err error
}

// offerMove makes the call findable by BackgroundCall until the tool returns.
func (s *Session) offerMove(r *toolRunner, call message.ToolCallBlock, input json.RawMessage, ft frozenTool, tc *ToolContext, tctx context.Context, scope *hookScope, mv *toolexec.Move, began time.Time) *movableCall {
	mc := &movableCall{r: r, call: call, input: input, ft: ft, tc: tc, tctx: tctx, scope: scope, mv: mv, began: began, started: make(chan struct{}), done: make(chan toolReturn, 1)}
	s.movable.Store(call.ID, mc)
	return mc
}

// returned withdraws the offer and reports whether the tool's return values went to the task the call moved to.
func (mc *movableCall) returned(res ToolResult, err error) bool {
	mc.r.s.movable.CompareAndDelete(mc.call.ID, mc)
	if !mc.mv.Returned() {
		return false
	}
	mc.done <- toolReturn{res: res, err: err}
	return true
}

// BackgroundCall moves a running foreground call of this session to the background: the call returns task_started at once and the work goes on as a task that notifies when it ends.
func (s *Session) BackgroundCall(ctx context.Context, callID string) (TaskInfo, error) {
	if callID == "" { // a host-started foreground task waits under no call, and must not be moved by a missing id
		return TaskInfo{}, ErrNotMovable
	}
	if v, ok := s.movable.Load(callID); ok {
		return s.adoptCall(ctx, v.(*movableCall))
	}
	return s.tasks.detach(callID)
}

// adoptCall hands a running call to a new tool_exec task whose first attempt waits for it instead of calling the tool again.
func (s *Session) adoptCall(ctx context.Context, mc *movableCall) (TaskInfo, error) {
	select {
	case <-mc.started:
	case <-ctx.Done():
		return TaskInfo{}, ctx.Err()
	}
	if !mc.mv.Claim() {
		return TaskInfo{}, ErrNotMovable
	}
	x := &adoptedRun{mc: mc, ready: make(chan struct{})}
	sctx := withMarker(withHookScope(ctx, mc.scope), marker{chain: s.chain, kind: "tool", call: mc.call.ID})
	info, _, err := s.tasks.startWith(sctx, TaskSpec{Kind: ToolExecKind, Title: mc.call.Name, Input: toolExecSpec(mc.call.Name, mc.input)}, true, x)
	if err != nil {
		mc.mv.Release()
		return TaskInfo{}, err
	}
	select {
	case <-x.ready: // the task took the call's progress over before the call settles, so none reaches the main loop after its tool.completed
	case <-s.ctx.Done():
	}
	mc.mv.Commit(mc.r.convert(mc.tctx, mc.call, startedResult(mc.r.s.agent.cfg.texts, info, true), nil), max(mc.ft.spec.Timeout, 0))
	return info, nil
}

// adoptedRun is the first attempt of a task a moved call became: it waits for the running call, so the tool never runs twice.
type adoptedRun struct {
	mc    *movableCall
	ready chan struct{}
}

func (x *adoptedRun) Run(ctx context.Context, rt TaskRuntime) (TaskResult, error) {
	r, mc := rt.(*taskRuntime), x.mc
	mc.tc.task.Store(r)
	close(x.ready)
	var ret toolReturn
	select {
	case ret = <-mc.done:
	case <-ctx.Done():
		mc.mv.Cancel()
		ret = <-mc.done // a tool that ignores it is abandoned after the grace period like any task
	}
	s := r.s
	path := []event.Hop{r.hop()}
	env := s.env(r.turnID, 0)
	env.Path = append(env.Path, path...)
	conv := &toolRunner{s: s, turnCtx: ctx, tools: map[string]frozenTool{mc.call.Name: mc.ft}}
	out := conv.convert(mc.tctx, mc.call, ret.res, ret.err)
	out, _ = postToolUse(withHookScope(ctx, &hookScope{turnID: r.turnID, callID: mc.call.ID, path: path}), s, env, mc.call, out, time.Since(mc.began))
	return toolExecResult(out)
}
