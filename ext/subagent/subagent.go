// Package subagent lets the model delegate work to a derived agent in a child session, as a task it can wait on or run in the background.
package subagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/spec"
)

// Name is the extension's name, which std.Without takes; it is also the task kind and the start tool.
const Name = "subagent"

const (
	askTool          = "ask_parent"
	generalName      = "general"
	defaultMaxSteps  = 50
	defaultMax       = 8
	maxStopContinues = 8
	settleWait       = 100 * time.Millisecond // an interrupted turn repairs its tail well within this, and it stays far under the stop grace period
	closeWait        = 10 * time.Second       // for the child to close
)

type Def struct {
	Name         string
	Description  string
	Instructions string
	Tools        []string // nil gives every tool the parent has; an empty slice gives none
	DenyTools    []string
	Model        *provider.ModelRef // nil uses the parent session's current model
	FixedModel   bool               // a call's model and effort are ignored even when Model is nil, so the type always follows the parent's
	MaxSteps     int
	AskTimeout   time.Duration // how long ask_parent waits for an answer; 0 waits until one arrives
}

type Options struct {
	Defs          []Def
	MaxConcurrent int // subagents of one session running at once, foreground and background; 0 means 8
	// ResolveModel turns a name from Models into the model a child starts with; nil leaves model out of the start
	// tool's schema, and a call that names one anyway fails.
	ResolveModel func(name string) (*provider.ModelRef, error)
	Models       []string // the start tool's model enum; empty exposes no model choice, and "inherit" runs on the parent's model
	Efforts      []string // the start tool's effort enum, values as in provider.ReasoningParams.Effort; empty exposes no effort choice
}

type StartInput struct {
	kernel.HookEnv
	TaskID    string `json:"task_id"`
	Attempt   int    `json:"attempt"`
	AgentType string `json:"agent_type"`
	Model     string `json:"model,omitempty"` // what the call asked for; empty when the type fixes its model or the call named none
	Effort    string `json:"effort,omitempty"`
	Prompt    string `json:"prompt"`
}

// StartOutput has no Common, because its context goes to the child session rather than to the session that fires it.
type StartOutput struct {
	Deny              bool   `json:"deny,omitempty"`
	Reason            string `json:"reason,omitempty"`
	UpdatedPrompt     string `json:"updated_prompt,omitempty"`
	Model             string `json:"model,omitempty"` // overrides the model the call asked for
	Effort            string `json:"effort,omitempty"`
	AdditionalContext string `json:"additional_context,omitempty"` // rides along with the child's first turn
}

type StopInput struct {
	kernel.HookEnv
	TaskID            string `json:"task_id"`
	AgentType         string `json:"agent_type"`
	Outcome           string `json:"outcome"`
	LastAssistantText string `json:"last_assistant_text"`
	Continuations     int    `json:"continuations"`
}

type StopOutput struct {
	Continue bool   `json:"continue,omitempty"`
	Reason   string `json:"reason,omitempty"` // sent to the child as the next instruction
}

var (
	Start = kernel.NewHookPoint(kernel.HookPointDef[StartInput, StartOutput]{
		Name: "subagent_start",
		Key:  func(in StartInput) string { return in.AgentType },
		Chain: func(in StartInput, out StartOutput) StartInput {
			if out.UpdatedPrompt != "" {
				in.Prompt = out.UpdatedPrompt
			}
			if out.Model != "" {
				in.Model = out.Model
			}
			if out.Effort != "" {
				in.Effort = out.Effort
			}
			return in
		},
		Merge: func(acc, out StartOutput) (StartOutput, bool) {
			if out.Deny {
				return out, true
			}
			if out.UpdatedPrompt != "" {
				acc.UpdatedPrompt = out.UpdatedPrompt
			}
			if out.Model != "" {
				acc.Model = out.Model
			}
			if out.Effort != "" {
				acc.Effort = out.Effort
			}
			acc.AdditionalContext = joinText(acc.AdditionalContext, out.AdditionalContext)
			return acc, false
		},
		Timeout: 10 * time.Second,
		Fail:    kernel.FailClosed,
		Closed: func(err error) StartOutput {
			kind := "error"
			var he *kernel.HookError
			if errors.As(err, &he) {
				kind = he.Kind
			}
			return StartOutput{Deny: true, Reason: "subagent_start hook failed (" + kind + ")"} // the kind alone, since the error text may carry what the model must not see
		},
	})
	Stop = kernel.NewHookPoint(kernel.HookPointDef[StopInput, StopOutput]{
		Name: "subagent_stop",
		Key:  func(in StopInput) string { return in.AgentType },
		Merge: func(acc, out StopOutput) (StopOutput, bool) {
			if out.Continue {
				acc.Continue, acc.Reason = true, joinText(acc.Reason, out.Reason)
			}
			return acc, false
		},
		Timeout: 30 * time.Second,
		Fail:    kernel.FailOpen,
	})
)

func joinText(a, b string) string {
	switch {
	case a == "":
		return b
	case b == "":
		return a
	}
	return a + "\n\n" + b
}

var (
	defsKey    = kernel.NewKey[[]Def]("subagent.defs")                          // on the scope that installed or defined them
	toolKey    = kernel.NewKey[bool]("subagent.session_tool")                   // the session registered its own start tool for its own definitions
	optsKey    = kernel.NewKey[startOptions]("subagent.start_options")          // the choices the start tool exposes
	slotsKey   = kernel.NewKey[int]("subagent.running")                         // on the owner session: attempts holding a slot
	childKey   = kernel.NewKey[map[string]*kernel.Session]("subagent.children") // on the owner session: task id -> its live child
	attemptKey = kernel.NewKey[*attempt]("subagent.attempt")                    // on a child session: the attempt it works for now
)

// startOptions is Options as the start tool and a session-scope Define read it.
type startOptions struct {
	models  []string
	efforts []string
	resolve func(string) (*provider.ModelRef, error)
}

// startOptionsFor reads the options installed at this scope, falling back to the agent's for a session's own tool.
func startOptionsFor(r kernel.Registrar) startOptions {
	if s := r.Session(); s != nil {
		if o, ok := kernel.Get(s, optsKey); ok {
			return o
		}
	}
	o, _ := kernel.Get(r.Agent(), optsKey)
	return o
}

var defName = regexp.MustCompile(`^[a-zA-Z0-9_:-]{1,64}$`)

// New registers the subagent kind together with its start tool, so the model can delegate in the foreground or the background.
func New(o Options) kernel.Extension {
	return kernel.ExtensionFunc(Name, func(r kernel.Registrar) error {
		so := startOptions{models: slices.Clone(o.Models), efforts: slices.Clone(o.Efforts), resolve: o.ResolveModel}
		if s := r.Session(); s != nil {
			kernel.Set(s, optsKey, so)
		} else {
			kernel.Set(r.Agent(), optsKey, so)
		}
		defs := o.Defs
		if len(defs) == 0 {
			defs = []Def{{Name: generalName, Description: text("general")}}
		}
		for _, d := range defs {
			if _, err := Define(r, d); err != nil {
				return err
			}
		}
		max := o.MaxConcurrent
		if max == 0 {
			max = defaultMax
		}
		if _, err := r.TaskKind(kernel.TaskKind{Name: Name, Description: "Runs a delegated task in a child session.", Caps: kernel.TaskCaps{Instructions: true, Continue: true, Restart: true},
			New: func(spec kernel.TaskSpec) (kernel.Task, error) {
				var in input
				if err := json.Unmarshal(spec.Input, &in); err != nil {
					return nil, fmt.Errorf("invalid subagent input: %w", err)
				}
				return &task{in: in, max: max, models: o.Models, efforts: o.Efforts, resolve: o.ResolveModel}, nil
			}}); err != nil {
			return err
		}
		if r.Session() != nil {
			return nil // Define gave the session its own start tool
		}
		_, err := r.Tool(newStartTool(r))
		return err
	})
}

// Define adds a subagent type in r's scope; at session scope the session gets its own start tool, whose enum also lists the session's types.
func Define(r kernel.Registrar, d Def) (*kernel.Registration, error) {
	if !defName.MatchString(d.Name) {
		return nil, fmt.Errorf("%w: subagent type %q", kernel.ErrInvalidName, d.Name)
	}
	h := holder(r)
	var dup bool
	kernel.Update(h, defsKey, func(old []Def, _ bool) []Def {
		if dup = slices.ContainsFunc(old, func(x Def) bool { return x.Name == d.Name }); dup {
			return old
		}
		return append(slices.Clone(old), d)
	})
	if dup {
		return nil, fmt.Errorf("%w: subagent type %q", kernel.ErrDuplicate, d.Name)
	}
	added := false
	if s := r.Session(); s != nil {
		if had, _ := kernel.Get(s, toolKey); !had {
			kernel.Set(s, toolKey, true)
			if _, err := r.Tool(newStartTool(r)); err != nil {
				return nil, err
			}
			added = true
		}
	}
	if !added {
		kernel.ToolChanged(r, toolDef.Name)
	}
	return kernel.NewRegistration(func() {
		kernel.Update(h, defsKey, func(old []Def, _ bool) []Def {
			return slices.DeleteFunc(slices.Clone(old), func(x Def) bool { return x.Name == d.Name })
		})
		kernel.ToolChanged(r, toolDef.Name)
	}), nil
}

func holder(r kernel.Registrar) kernel.StateHolder {
	if s := r.Session(); s != nil {
		return s
	}
	return r.Agent()
}

// defs are the types a session can use now, its own shadowing its agent's by name.
func defs(s *kernel.Session) map[string]Def {
	out := map[string]Def{}
	agent, _ := kernel.Get(s.Registrar().Agent(), defsKey)
	own, _ := kernel.Get(s, defsKey)
	for _, d := range slices.Concat(agent, own) {
		out[d.Name] = d
	}
	return out
}

type input struct {
	AgentType   string `json:"agent_type"`
	Description string `json:"description"`
	Model       string `json:"model,omitempty"`
	Effort      string `json:"effort,omitempty"`
	Prompt      string `json:"prompt"`
}

// startTool recomputes the agent_type enum at every freeze point and checks the type against the current definitions when called.
type startTool struct {
	agent   *kernel.Agent
	session *kernel.Session // set for a session's own start tool
	base    kernel.ToolSpec
	opts    startOptions
}

var toolDef = func() kernel.ToolSpec {
	var def struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"input_schema"`
	}
	if err := json.Unmarshal([]byte(spec.MustText("ext/subagent/subagent.json")), &def); err != nil {
		panic(err)
	}
	return kernel.ToolSpec{Name: def.Name, Description: def.Description, InputSchema: def.InputSchema, Concurrency: kernel.Parallel}
}()

func newStartTool(r kernel.Registrar) *startTool {
	return &startTool{agent: r.Agent(), session: r.Session(), base: toolDef, opts: startOptionsFor(r)}
}

func toSpec(raw json.RawMessage) (kernel.TaskSpec, error) {
	var in input
	if err := json.Unmarshal(raw, &in); err != nil {
		return kernel.TaskSpec{}, err
	}
	return kernel.TaskSpec{Title: in.Description, Input: raw}, nil
}

// start builds the kind's start tool; its schema comes from the spec files, so an error is a build defect.
func start(def kernel.ToolSpec) kernel.Tool {
	t, err := kernel.TaskTool(Name, def, toSpec)
	if err != nil {
		panic(err)
	}
	return t
}

func (t *startTool) Spec() kernel.ToolSpec {
	byName := map[string]Def{}
	agent, _ := kernel.Get(t.agent, defsKey)
	var own []Def
	if t.session != nil {
		own, _ = kernel.Get(t.session, defsKey)
	}
	for _, d := range slices.Concat(agent, own) {
		byName[d.Name] = d
	}
	names := slices.Sorted(maps.Keys(byName))
	spec := t.base
	spec.InputSchema = withEnums(spec.InputSchema, names, t.opts.models, t.opts.efforts)
	lines := []string{text("types")}
	for _, n := range names {
		lines = append(lines, text("type", "name", n, "description", byName[n].Description))
	}
	spec.Description += "\n\n" + strings.Join(lines, "\n")
	return start(spec).Spec()
}

func (t *startTool) Call(ctx context.Context, tc *kernel.ToolContext, raw json.RawMessage) (kernel.ToolResult, error) {
	var in input
	_ = json.Unmarshal(raw, &in)
	if _, ok := defs(tc.Session())[in.AgentType]; !ok {
		return kernel.ErrorResult("%s", unknownType(tc.Session(), in.AgentType)), nil
	}
	return start(t.base).Call(ctx, tc, raw)
}

func unknownType(s *kernel.Session, name string) string {
	return text("unknown_type", "agent_type", name, "types", strings.Join(slices.Sorted(maps.Keys(defs(s))), ", "))
}

// withEnums sets the agent_type enum and the model and effort enums the host exposes; the schema comes from
// subagent.json, so its shape is known. An empty list leaves that property out, so the model never sees a choice
// the host does not offer.
func withEnums(schema json.RawMessage, names, models, efforts []string) json.RawMessage {
	var s map[string]json.RawMessage
	var props map[string]json.RawMessage
	var at map[string]json.RawMessage
	if json.Unmarshal(schema, &s) != nil || json.Unmarshal(s["properties"], &props) != nil || json.Unmarshal(props["agent_type"], &at) != nil {
		return schema
	}
	enum, _ := json.Marshal(names)
	at["enum"] = enum
	props["agent_type"] = ordered(at, "type", "enum", "description")
	order := []string{"agent_type", "description"}
	for _, p := range []struct {
		name   string
		values []string
	}{{"model", models}, {"effort", efforts}} {
		if len(p.values) == 0 {
			delete(props, p.name)
			continue
		}
		var prop map[string]json.RawMessage
		if json.Unmarshal(props[p.name], &prop) != nil {
			continue
		}
		values, _ := json.Marshal(p.values)
		prop["enum"] = values
		props[p.name] = ordered(prop, "type", "enum", "description")
		order = append(order, p.name)
	}
	order = append(order, "prompt")
	s["properties"] = ordered(props, order...)
	return ordered(s, "type", "properties", "required", "additionalProperties")
}

// ordered writes an object with its members in the given order, so the definition's bytes stay what subagent.json says.
func ordered(m map[string]json.RawMessage, keys ...string) json.RawMessage {
	var b strings.Builder
	b.WriteByte('{')
	first := true
	for _, k := range keys {
		v, ok := m[k]
		if !ok {
			continue // a property the host exposes no choice for
		}
		if !first {
			b.WriteByte(',')
		}
		first = false
		name, _ := json.Marshal(k)
		b.Write(name)
		b.WriteByte(':')
		b.Write(v)
	}
	b.WriteByte('}')
	return json.RawMessage(b.String())
}

// ---- the task ----

type task struct {
	in      input
	max     int
	models  []string
	efforts []string
	resolve func(string) (*provider.ModelRef, error)
}

// saved is the attempt's state, which a continuation after a restart of the process resumes the child from; the
// model and effort are the ones this attempt settled on, so a resumed child keeps them.
type saved struct {
	ChildSessionID string `json:"child_session_id"`
	AgentType      string `json:"agent_type"`
	Model          string `json:"model,omitempty"`
	Effort         string `json:"effort,omitempty"`
}

// attempt is what ask_parent in the child needs from the attempt it works for.
type attempt struct {
	rt      kernel.TaskRuntime
	timeout time.Duration
	mu      sync.Mutex
	waiting chan string // set while ask_parent waits, so the answer goes to it instead of to the child as input
}

func (t *task) Run(ctx context.Context, rt kernel.TaskRuntime) (kernel.TaskResult, error) {
	owner := rt.Session()
	if _, nested := kernel.Get(owner, attemptKey); nested {
		return kernel.TaskResult{}, errors.New(text("nested"))
	}
	def, ok := defs(owner)[t.in.AgentType]
	if !ok {
		return kernel.TaskResult{}, errors.New(unknownType(owner, t.in.AgentType))
	}
	if !t.take(owner) {
		return kernel.TaskResult{}, errors.New(text("too_many", "max", strconv.Itoa(t.max)))
	}
	defer kernel.Update(owner, slotsKey, func(n int, _ bool) int { return n - 1 })
	child, first, extra, err := t.open(ctx, rt, owner, def)
	if err != nil {
		return kernel.TaskResult{}, err
	}
	at := &attempt{rt: rt, timeout: def.AskTimeout}
	kernel.Set(child, attemptKey, at)
	return t.drive(ctx, rt, owner, child, at, def, first, extra)
}

// take holds one of the owner's slots; a full set fails at once rather than queueing, since a queued call could wait on a subagent that waits on it.
func (t *task) take(owner *kernel.Session) bool {
	ok := false
	kernel.Update(owner, slotsKey, func(n int, _ bool) int {
		if t.max >= 0 && n >= t.max {
			return n
		}
		ok = true
		return n + 1
	})
	return ok
}

// open makes a new child for a first attempt or a restart, and finds or resumes the one a continuation goes on with.
func (t *task) open(ctx context.Context, rt kernel.TaskRuntime, owner *kernel.Session, def Def) (child *kernel.Session, first kernel.Input, mode kernel.InputMode, err error) {
	from := message.Origin{Kind: message.OriginAgent, Source: "agent:" + owner.Info().Agent}
	if cont := rt.Spec().Continue; cont != nil {
		var st saved
		_ = json.Unmarshal(cont.State, &st)
		model, effort := t.fixedChoice(def, st.Model, st.Effort)
		if child = liveChild(owner, rt.ID(), st.ChildSessionID); child == nil {
			ref, err := t.modelRef(owner, model, effort)
			if err != nil {
				return nil, first, mode, err
			}
			derived, err := derive(owner, def, ref)
			if err != nil {
				return nil, first, mode, err
			}
			if child, err = derived.Resume(ctx, st.ChildSessionID, childOptions(rt, def)...); err != nil {
				if errors.Is(err, kernel.ErrSessionNotFound) || st.ChildSessionID == "" {
					return nil, first, mode, errors.New(text("cannot_continue"))
				}
				return nil, first, mode, err
			}
			remember(owner, rt.ID(), child)
		}
		return child, kernel.Input{Text: cont.Instruction.Text, Origin: from}, cont.Instruction.Mode, nil
	}
	model, effort := t.fixedChoice(def, t.in.Model, t.in.Effort)
	if err := t.check(model, effort); err != nil {
		return nil, first, mode, err
	}
	out, in := kernel.Fire(ctx, owner, Start, StartInput{HookEnv: env(owner, rt), TaskID: rt.ID(), Attempt: rt.Attempt(), AgentType: def.Name, Model: model, Effort: effort, Prompt: t.in.Prompt})
	if out.Deny {
		return nil, first, mode, errors.New(text("denied", "reason", out.Reason))
	}
	if out.Model != "" {
		model = out.Model // the hook chose, so it is not held to the enum the model sees
	}
	if out.Effort != "" {
		effort = out.Effort
	}
	ref, err := t.modelRef(owner, model, effort)
	if err != nil {
		return nil, first, mode, err
	}
	derived, err := derive(owner, def, ref)
	if err != nil {
		return nil, first, mode, err
	}
	if child, err = derived.NewSession(ctx, childOptions(rt, def)...); err != nil {
		return nil, first, mode, err
	}
	state, _ := json.Marshal(saved{ChildSessionID: child.Info().SessionID, AgentType: def.Name, Model: model, Effort: effort})
	rt.SaveState(state)
	remember(owner, rt.ID(), child)
	if out.AdditionalContext != "" {
		_, _ = child.Send(kernel.Input{Text: out.AdditionalContext, Origin: message.Origin{Kind: message.OriginReminder, Source: "hook:subagent_start"}}, kernel.NoWake())
	}
	return child, kernel.Input{Text: in.Prompt, Origin: from}, "", nil
}

func env(s *kernel.Session, rt kernel.TaskRuntime) kernel.HookEnv {
	info := s.Info()
	return kernel.HookEnv{Session: s, SessionID: info.SessionID, Agent: info.Agent, TurnID: rt.TurnID()}
}

// inheritModel is the name that runs a child on the parent's current model, so a host that lists it in Models need
// not resolve it itself.
const inheritModel = "inherit"

// fixedChoice drops a call's choice when the definition fixes the model, as a type that always runs on the parent's
// or its own model must.
func (t *task) fixedChoice(def Def, model, effort string) (string, string) {
	if def.Model != nil || def.FixedModel {
		return "", ""
	}
	return model, effort
}

// check rejects a name outside the enums the host declared, so the model learns instead of running on the default.
func (t *task) check(model, effort string) error {
	if model != "" && model != inheritModel {
		switch {
		case len(t.models) == 0:
			return errors.New(text("no_model_choice"))
		case !slices.Contains(t.models, model):
			return errors.New(text("unknown_model", "model", model, "models", strings.Join(t.models, ", ")))
		}
	}
	if effort != "" {
		switch {
		case len(t.efforts) == 0:
			return errors.New(text("no_effort_choice"))
		case !slices.Contains(t.efforts, effort):
			return errors.New(text("unknown_effort", "effort", effort, "efforts", strings.Join(t.efforts, ", ")))
		}
	}
	return nil
}

// modelRef is the base the child's agent starts from: the parent's current model, the one this attempt named
// (inherit included) or the host resolved, with the effort this attempt asked for.
func (t *task) modelRef(owner *kernel.Session, model, effort string) (*provider.ModelRef, error) {
	ref := owner.Model()
	if model != "" && model != inheritModel {
		if t.resolve == nil {
			return nil, errors.New(text("no_model_resolver", "model", model))
		}
		resolved, err := callResolve(t.resolve, model)
		if err != nil {
			return nil, err
		}
		if resolved == nil || resolved.Provider == nil {
			return nil, errors.New(text("no_model_provider", "model", model))
		}
		ref = *resolved
	}
	if effort != "" {
		ref.Params.Reasoning.Effort = effort
	}
	return &ref, nil
}

// callResolve runs caller code, so a panic fails the attempt instead of the task manager.
func callResolve(resolve func(string) (*provider.ModelRef, error), name string) (ref *provider.ModelRef, err error) {
	defer func() {
		if p := recover(); p != nil {
			err = fmt.Errorf("model resolver panicked: %v", p)
		}
	}()
	return resolve(name)
}

// derive makes the child's agent: the parent's tools as the definition narrows them, never the start tool, the
// definition's user section and model, and the parent's hooks; ref is what this attempt chose, nil for the default.
func derive(owner *kernel.Session, def Def, ref *provider.ModelRef) (*kernel.Agent, error) {
	model := owner.Model()
	if ref != nil {
		model = *ref
	}
	if def.Model != nil {
		model = *def.Model // the definition named its model, and the attempt's choice was dropped already
	}
	steps := def.MaxSteps
	if steps == 0 {
		steps = defaultMaxSteps
	}
	instr := def.Instructions
	return owner.Registrar().Agent().Derive(kernel.DeriveOptions{Nickname: def.Name, Description: def.Description, Instructions: &instr, Model: &model,
		ToolFilter: func(ts kernel.ToolSpec) bool { return ts.Name != Name && allowed(def, ts.Name) }, InheritHooks: true, Limits: &kernel.Limits{MaxSteps: steps}})
}

func allowed(def Def, tool string) bool {
	return (def.Tools == nil || slices.Contains(def.Tools, tool)) && !slices.Contains(def.DenyTools, tool)
}

// childOptions own the child by the attempt and give it ask_parent before its first epoch, since only a subagent's child session has that tool.
func childOptions(rt kernel.TaskRuntime, def Def) []kernel.SessionOption {
	opts := []kernel.SessionOption{kernel.ChildOf(rt)}
	if allowed(def, askTool) {
		opts = append(opts, kernel.SessionUse(kernel.ExtensionFunc(askTool, func(r kernel.Registrar) error {
			_, err := r.Tool(kernel.NewTool(askDef, ask))
			return err
		})))
	}
	return opts
}

// remember records the child under its task, so a continuation finds it while it is open.
func remember(owner *kernel.Session, taskID string, child *kernel.Session) {
	kernel.Update(owner, childKey, func(old map[string]*kernel.Session, _ bool) map[string]*kernel.Session {
		m := maps.Clone(old)
		if m == nil {
			m = map[string]*kernel.Session{}
		}
		m[taskID] = child
		return m
	})
}

// liveChild is the task's child when it is still open, which the kernel has already bound to the new attempt.
func liveChild(owner *kernel.Session, taskID, id string) *kernel.Session {
	m, _ := kernel.Get(owner, childKey)
	c := m[taskID]
	if c == nil || c.Info().SessionID != id {
		return nil
	}
	if ph := c.Snapshot().Phase; ph == "closing" || ph == "closed" {
		return nil
	}
	return c
}

// drive feeds the child until it is quiet with nothing left to say, answering ask_parent from the inbox, and closes it when the attempt is stopped.
func (t *task) drive(ctx context.Context, rt kernel.TaskRuntime, owner, child *kernel.Session, at *attempt, def Def, first kernel.Input, mode kernel.InputMode) (kernel.TaskResult, error) {
	send := func(in kernel.Input, m kernel.InputMode) error {
		var opts []kernel.SendOption
		if m != "" {
			opts = append(opts, kernel.WithMode(m))
		}
		_, err := child.Send(in, opts...)
		return err
	}
	from := first.Origin
	if err := send(first, mode); err != nil {
		return kernel.TaskResult{}, err
	}
	quiet := t.waitQuiet(ctx, child)
	continues := 0
	for {
		select {
		case in := <-rt.Inbox():
			if in.Answer && at.answer(in.Text) {
				continue
			}
			if err := send(kernel.Input{Text: in.Text, Origin: from}, in.Mode); err != nil {
				return kernel.TaskResult{}, err
			}
		case q := <-quiet:
			if q.err != nil {
				return kernel.TaskResult{}, t.stop(ctx, child, q.err)
			}
			if len(rt.Inbox()) > 0 {
				quiet = t.waitQuiet(ctx, child)
				continue
			}
			if continues < maxStopContinues {
				out, _ := kernel.Fire(ctx, owner, Stop, StopInput{HookEnv: env(owner, rt), TaskID: rt.ID(), AgentType: def.Name, Outcome: string(q.res.Outcome), LastAssistantText: q.res.FinalText, Continuations: continues})
				if out.Continue && ctx.Err() == nil {
					continues++
					if err := send(kernel.Input{Text: out.Reason, Origin: from}, ""); err != nil {
						return kernel.TaskResult{}, err
					}
					quiet = t.waitQuiet(ctx, child)
					continue
				}
			}
			return result(child, def, q.res)
		case <-ctx.Done():
			return kernel.TaskResult{}, t.stop(ctx, child, context.Cause(ctx))
		}
		quiet = t.waitQuiet(ctx, child)
	}
}

type quietResult struct {
	res kernel.TurnResult
	err error
}

// waitQuiet waits off the loop, so the inbox is still read while the child works; the channel is buffered, so a replaced wait never blocks.
func (t *task) waitQuiet(ctx context.Context, child *kernel.Session) <-chan quietResult {
	ch := make(chan quietResult, 1)
	go func() {
		r, err := child.WaitQuiet(ctx)
		ch <- quietResult{res: r, err: err}
	}()
	return ch
}

// stop interrupts the child's turn and gives it a moment to wind down before closing the child, so its history records an interruption; a later continuation resumes it from the store.
func (t *task) stop(ctx context.Context, child *kernel.Session, cause error) error {
	if child.Interrupt("the task was stopped") {
		time.Sleep(settleWait)
	}
	cctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), closeWait)
	defer cancel()
	_ = child.Close(cctx)
	if cause == nil {
		cause = ctx.Err()
	}
	return cause
}

func result(child *kernel.Session, def Def, r kernel.TurnResult) (kernel.TaskResult, error) {
	if r.Outcome == kernel.OutcomeError {
		if r.Err != nil {
			return kernel.TaskResult{}, r.Err
		}
		return kernel.TaskResult{}, errors.New(string(r.Outcome))
	}
	answer := kernel.SanitizeForeign(r.FinalText)
	body := answer
	if body == "" {
		body = text("no_answer")
	}
	if r.Outcome != kernel.OutcomeCompleted {
		body += "\n\n" + text("not_completed", "outcome", string(r.Outcome))
	}
	body = strings.ReplaceAll(body, "</subagent_result>", "<\\/subagent_result>")
	content := text("result", "agent_type", def.Name, "outcome", string(r.Outcome), "steps", strconv.Itoa(r.Steps), "text", body)
	data, _ := json.Marshal(map[string]any{"child_session_id": child.Info().SessionID, "outcome": r.Outcome, "steps": r.Steps, "usage": r.Usage})
	return kernel.TaskResult{Summary: answer, Content: message.Content{&message.TextBlock{Text: content}}, Data: data}, nil
}

// ---- ask_parent ----

var askDef = func() kernel.ToolSpec {
	var def struct {
		Name        string          `json:"name"`
		Description string          `json:"description"`
		InputSchema json.RawMessage `json:"input_schema"`
	}
	if err := json.Unmarshal([]byte(spec.MustText("ext/subagent/ask_parent.json")), &def); err != nil {
		panic(err)
	}
	return kernel.ToolSpec{Name: def.Name, Description: def.Description, InputSchema: def.InputSchema, Concurrency: kernel.Serial, Timeout: kernel.NoTimeout}
}()

type askInput struct {
	Question string `json:"question"`
}

// ask puts the question to the task's owner through the task protocol and blocks this batch until the answer, the definition's timeout or a stop.
func ask(ctx context.Context, tc *kernel.ToolContext, in askInput) (kernel.ToolResult, error) {
	at, _ := kernel.Get(tc.Session(), attemptKey)
	if at == nil || !at.running() {
		return kernel.TextResult(text("no_one_to_ask")), nil
	}
	ch := make(chan string, 1)
	at.mu.Lock()
	at.waiting = ch
	at.mu.Unlock()
	defer at.clear(ch)
	at.rt.AskInput(in.Question)
	var timeout <-chan time.Time
	if at.timeout > 0 {
		tm := time.NewTimer(at.timeout)
		defer tm.Stop()
		timeout = tm.C
	}
	select {
	case a := <-ch:
		return kernel.TextResult(a), nil
	case <-timeout:
		at.clear(ch)
		at.rt.AskInput("") // withdraws the question and its undelivered notification
		select {
		case a := <-ch: // answered while it timed out
			return kernel.TextResult(a), nil
		default:
		}
		return kernel.TextResult(text("ask_timeout", "duration", seconds(at.timeout))), nil
	case <-ctx.Done():
		return kernel.ToolResult{}, context.Cause(ctx)
	}
}

// running is false once the attempt ended, when nobody would read an answer.
func (at *attempt) running() bool {
	st := at.rt.Session().Tasks()
	info, ok := st.Get(at.rt.ID())
	return ok && info.Attempt == at.rt.Attempt() && !info.Status.Terminal()
}

// answer hands an answer to the waiting ask_parent; false means none waits, so it goes to the child as input.
func (at *attempt) answer(text string) bool {
	at.mu.Lock()
	defer at.mu.Unlock()
	if at.waiting == nil {
		return false
	}
	at.waiting <- text
	at.waiting = nil
	return true
}

func (at *attempt) clear(ch chan string) {
	at.mu.Lock()
	if at.waiting == ch {
		at.waiting = nil
	}
	at.mu.Unlock()
}

// seconds renders a duration as whole seconds rounded up, the same in every binding.
func seconds(d time.Duration) string {
	return strconv.FormatInt(int64((d+time.Second-1)/time.Second), 10) + "s"
}

// ---- texts ----

var text = kernel.SpecTexts("ext/subagent/results.md")
