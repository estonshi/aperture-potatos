package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/internal/hookrun"
	"github.com/estonshi/aperture-potatos/internal/prompt"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
)

// Slot is where an extension section sits in the system skeleton; core, identity and user are not open to extensions.
type Slot int

const (
	SlotCapabilities Slot = iota + 1 // usage notes of installed capabilities
	SlotCatalog                      // epoch-stable catalogs
	SlotEnvironment                  // epoch-stable environment facts
)

var slotNames = map[Slot]string{SlotCapabilities: prompt.SlotCapabilities, SlotCatalog: prompt.SlotCatalog, SlotEnvironment: prompt.SlotEnvironment}

// PromptSection is a part of the system prompt that an extension renders at every epoch start, so its text stays cacheable for the whole epoch.
type PromptSection struct {
	Name            string
	Slot            Slot
	Order           int                                                         // order inside the slot; ties sort by Name
	AnnounceChanges bool                                                        // a change inside an epoch reaches the model as a reminder instead of waiting for the next epoch
	Render          func(ctx context.Context, rc RenderContext) (string, error) // "" leaves the section out
}

// RenderContext is what a section's Render may read; Tools is the set the model sees this epoch, so a section can describe only installed tools.
type RenderContext struct {
	Agent   *Agent
	Session *Session
	Tools   []ToolSpec // the frozen tool set the model sees
	Epoch   int
}

// ProvideAt says how often a context provider is asked.
type ProvideAt int

const (
	AtTurnStart ProvideAt = iota + 1 // once, with the turn's first model call
	AtEveryStep                      // before every model call of the turn
)

// ContextProvider adds a reminder at delivery points; its Name is the reminder's source.
type ContextProvider struct {
	Name    string
	When    ProvideAt
	Provide func(ctx context.Context, pc ProvideContext) (string, error) // "" adds nothing this time
}

// ProvideContext is what a context provider's Provide may read at one delivery point.
type ProvideContext struct {
	Session *Session
	TurnID  string
	Step    int
	Inputs  []Input // AtTurnStart: this turn's inputs
}

// WithPromptSection registers ps at agent scope as part of the "app" extension.
func WithPromptSection(ps PromptSection) Option {
	return func(c *config) {
		c.app = append(c.app, func(r Registrar) error { _, err := r.PromptSection(ps); return err })
	}
}

// WithContextProvider registers p at agent scope as part of the "app" extension.
func WithContextProvider(p ContextProvider) Option {
	return func(c *config) {
		c.app = append(c.app, func(r Registrar) error { _, err := r.ContextProvider(p); return err })
	}
}

// sections lists the effective sections, session scope shadowing agent scope, in skeleton order.
func (s *Session) sections() []PromptSection {
	sess := s.reg.state()
	byName := map[string]PromptSection{}
	for n, r := range s.outer().sections {
		if !sess.hides(KindPromptSection, n) {
			byName[n] = r.ps
		}
	}
	for n, r := range sess.sections {
		byName[n] = r.ps
	}
	out := slices.Collect(maps.Values(byName))
	slices.SortFunc(out, func(a, b PromptSection) int {
		if a.Slot != b.Slot {
			return int(a.Slot - b.Slot)
		}
		if a.Order != b.Order {
			return a.Order - b.Order
		}
		return strings.Compare(a.Name, b.Name)
	})
	return out
}

// renderSection calls one section's Render off the actor; a failure leaves the section out and is reported, so one broken extension never blocks a session.
func (s *Session) renderSection(ctx context.Context, ps PromptSection, rc RenderContext) string {
	text, fail := hookrun.Call(ctx, s.wk, "section "+ps.Name, 0, s.limits.HookGrace, func(c context.Context) (string, error) { return ps.Render(c, rc) })
	if fail != nil {
		s.publish(&event.Error{Code: "section_render_failed", Message: "section " + ps.Name + ": " + fail.Err.Error()})
		return ""
	}
	return text
}

// renderSystem evaluates the developer section and renders the skeleton; SessionInstructions wins over WithInstructionsFunc, which wins over WithInstructions.
func (s *Session) renderSystem(ctx context.Context, tools map[string]frozenTool, epoch int) ([]prompt.Part, error) {
	cfg := s.agent.cfg
	name := s.agent.Nickname() // a rename reaches an open session with its next epoch
	s.renderedName = name
	instructions := cfg.instructions
	switch {
	case s.sessInstr != nil:
		instructions = *s.sessInstr
	case cfg.instructionsFunc != nil:
		v, err := callInstructions(ctx, cfg.instructionsFunc, SessionInfo{SessionID: s.id, ParentSessionID: s.parentID(), Agent: name, Labels: maps.Clone(s.labelMap()), Epoch: epoch})
		if err != nil {
			return nil, fmt.Errorf("instructions: %w", err)
		}
		instructions = v
	}
	rc := RenderContext{Agent: s.agent, Session: s, Tools: toolSpecs(tools), Epoch: epoch}
	var sections []prompt.Part
	for _, ps := range s.sections() {
		if text := s.renderSection(ctx, ps, rc); text != "" {
			sections = append(sections, prompt.Part{Slot: slotNames[ps.Slot], Name: ps.Name, Text: text})
		}
	}
	return prompt.System(prompt.SystemInput{Nickname: name, Description: cfg.description, Instructions: instructions, Sections: sections}), nil
}

func toolSpecs(tools map[string]frozenTool) []ToolSpec {
	out := make([]ToolSpec, 0, len(tools))
	for _, n := range slices.Sorted(maps.Keys(tools)) {
		out = append(out, tools[n].spec)
	}
	return out
}

// announceSection tells the model about a section that changed inside an epoch, as a reminder that never wakes, because the system prompt stays frozen until the next epoch.
func (s *Session) announceSection(c regChange) {
	ps := *c.section
	if c.op == "remove" {
		s.post(func(*state) {
			s.enqueue(s.reminderItem("registry", prompt.Textf("section_removed", map[string]string{"name": ps.Name}), false))
		})
		return
	}
	s.wk.Go("section "+ps.Name, func() {
		_, tools := s.freezeTools()
		text := s.renderSection(s.ctx, ps, RenderContext{Agent: s.agent, Session: s, Tools: toolSpecs(tools), Epoch: s.snap.Load().Epoch})
		if text == "" {
			return
		}
		s.post(func(*state) {
			s.enqueue(s.reminderItem("registry", prompt.Textf("section_updated", map[string]string{"name": ps.Name, "text": text}), false))
		})
	})
}

// providers lists the effective context providers of one kind, session scope shadowing agent scope, by name.
func (s *Session) providers(when ProvideAt) []ContextProvider {
	sess := s.reg.state()
	byName := map[string]ContextProvider{}
	for n, r := range s.outer().providers {
		if !sess.hides(KindContextProvider, n) {
			byName[n] = r.p
		}
	}
	for n, r := range sess.providers {
		byName[n] = r.p
	}
	var out []ContextProvider
	for _, n := range slices.Sorted(maps.Keys(byName)) {
		if p := byName[n]; p.When == when {
			out = append(out, p)
		}
	}
	return out
}

// provide runs the providers of one kind off the actor; a failure injects nothing and is reported.
func (s *Session) provide(ctx context.Context, when ProvideAt, pc ProvideContext) []*message.ReminderBlock {
	var out []*message.ReminderBlock
	for _, p := range s.providers(when) {
		if ctx.Err() != nil {
			return out
		}
		text, fail := hookrun.Call(ctx, s.wk, "provider "+p.Name, 0, s.limits.HookGrace, func(c context.Context) (string, error) { return p.Provide(c, pc) })
		switch {
		case fail != nil && fail.Kind != hookrun.KindCanceled:
			s.publishAt(&event.Error{Code: "context_provider_failed", Message: "context provider " + p.Name + ": " + fail.Err.Error()}, pc.TurnID, pc.Step, 0)
		case fail == nil && text != "":
			out = append(out, &message.ReminderBlock{Source: p.Name, Text: text})
		}
	}
	return out
}

func callInstructions(ctx context.Context, fn func(context.Context, SessionInfo) (string, error), info SessionInfo) (out string, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("panic: %v", r)
		}
	}()
	return fn(ctx, info)
}

func systemContent(parts []prompt.Part) message.Content {
	out := make(message.Content, len(parts))
	for i, p := range parts {
		out[i] = &message.TextBlock{Text: p.Text}
	}
	return out
}

// systemBlocks opens a rendered system: the session takes the name it was rendered with, and the end of the agent-level part is marked (B4) when session-level sections follow it and it is the same for every session of the agent (runtime.md §7).
func (s *Session) systemBlocks(parts []prompt.Part) []provider.SystemBlock {
	out := make([]provider.SystemBlock, len(parts))
	cut := -1
	for i, p := range parts {
		out[i] = provider.SystemBlock{Slot: p.Slot, Name: p.Name, Text: p.Text}
		if p.Slot != prompt.SlotCatalog && p.Slot != prompt.SlotEnvironment {
			cut = i
		}
	}
	if cut >= 0 && cut < len(out)-1 && s.agentLevelSystem() {
		out[cut].Cache = true
	}
	name := s.renderedName
	s.nickname.Store(&name)
	return out
}

// agentLevelSystem reports whether core through user render alike in every session of the agent: no session instructions, no instructions func, and no capabilities section added or hidden at session scope.
func (s *Session) agentLevelSystem() bool {
	if s.sessInstr != nil || s.agent.cfg.instructionsFunc != nil {
		return false
	}
	sess := s.reg.state()
	for _, r := range sess.sections {
		if r.ps.Slot == SlotCapabilities {
			return false
		}
	}
	for n, r := range s.outer().sections {
		if r.ps.Slot == SlotCapabilities && sess.hides(KindPromptSection, n) {
			return false
		}
	}
	return true
}

func systemHash(texts []string) string { return prompt.Hash(texts) }

func systemRendered(epoch int, parts []prompt.Part) *event.SystemRendered {
	ev := &event.SystemRendered{Epoch: epoch}
	texts := make([]string, len(parts))
	for i, p := range parts {
		ev.Sections = append(ev.Sections, event.Section{Slot: p.Slot, Name: p.Name, Chars: utf8.RuneCountInString(p.Text)})
		texts[i] = p.Text
	}
	ev.Hash = systemHash(texts)
	return ev
}

type frozenTool struct {
	tool Tool
	spec ToolSpec
}

// emptySchema stands in for a tool that declares no input, because every API requires an input schema.
var emptySchema = json.RawMessage(`{"type":"object"}`)

// freezeTools reads the effective tool set, session scope shadowing agent scope, in name order so the request bytes stay stable.
func (s *Session) freezeTools() ([]message.ToolDef, map[string]frozenTool) {
	byName := map[string]Tool{}
	sess := s.reg.state()
	for n, t := range s.outer().tools {
		if !sess.hides(KindTool, n) {
			byName[n] = t.tool
		}
	}
	for n, t := range sess.tools {
		byName[n] = t.tool
	}
	defs := make([]message.ToolDef, 0, len(byName))
	tools := make(map[string]frozenTool, len(byName))
	for _, name := range slices.Sorted(maps.Keys(byName)) {
		spec, ok := s.toolSpec(byName[name])
		if !ok {
			continue
		}
		spec.Name = name
		if spec.Background {
			bg, err := withBackground(spec.InputSchema, spec.Strict)
			if err != nil {
				s.agent.cfg.logger.Error("tool left out of this freeze", "tool", name, "error", err)
				continue
			}
			spec.InputSchema = bg
		}
		schema := spec.InputSchema
		if len(schema) == 0 {
			schema = emptySchema
		}
		defs = append(defs, message.ToolDef{Name: name, Description: spec.Description, InputSchema: schema, Strict: spec.Strict, Deferred: spec.Deferred})
		tools[name] = frozenTool{tool: byName[name], spec: spec}
	}
	s.noteFrozen(defs, tools)
	return defs, tools
}

// freeze fixes the turn's tool set (I10); a model whose thinking signatures bind the prefix keeps its epoch's definitions, while calls still run against the live registry.
func (t *turn) freeze() {
	t.defs, t.tools = t.s.freezeTools()
	t.hosted = t.s.freezeHosted(t.defs)
	if t.model.caps.ThinkingPrefixBound {
		_ = t.s.exec(func(st *state) { t.defs, t.hosted = st.history[0].Tools, st.history[0].HostedTools })
		t.s.noteFrozen(t.defs, t.tools)
	}
}

// toolSpec calls a tool's Spec, which is caller code; a tool whose Spec panics is left out of this freeze.
func (s *Session) toolSpec(t Tool) (spec ToolSpec, ok bool) {
	defer func() {
		if r := recover(); r != nil {
			s.agent.cfg.logger.Error("tool Spec panicked", "panic", r)
			ok = false
		}
	}()
	return t.Spec(), true
}
