package kernel

import (
	"context"
	"fmt"
	"slices"

	"github.com/estonshi/aperture-potatos/internal/prompt"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/store"
)

// Resume reopens a stored session: its current epoch, the system it was rendered with and, for a prefix-bound model, the frozen tools; the model is the agent's current one.
func (a *Agent) Resume(ctx context.Context, id string, opts ...SessionOption) (*Session, error) {
	var sc sessionConfig
	for _, o := range opts {
		o(&sc)
	}
	mode := a.cfg.inputMode
	if sc.inputMode != "" {
		mode = sc.inputMode
	}
	if _, ok := modeRank[mode]; !ok {
		return nil, ErrInvalidMode
	}
	if sc.contextWindow != nil && *sc.contextWindow < 0 {
		return nil, fmt.Errorf("%w: %d", ErrInvalidWindow, *sc.contextWindow)
	}
	if _, err := compileRules(sc.rules); err != nil {
		return nil, err
	}
	lines, err := a.cfg.store.Load(ctx, id)
	if err != nil {
		return nil, err
	}
	if !live(lines) {
		return nil, ErrSessionNotFound // never stored, or a dropped side chat
	}
	if meta := lines[0].Boundary.Session; meta != nil && meta.SideChat != "" {
		return nil, ErrSideChat
	} else if meta != nil && meta.ParentSessionID != "" && sc.owner == nil {
		return nil, ErrChildSession
	}
	return a.resume(ctx, id, lines, sc, nil)
}

// resume reopens stored lines as session id; prep adjusts the session before it starts.
func (a *Agent) resume(ctx context.Context, id string, lines []message.Message, sc sessionConfig, prep func(*Session)) (*Session, error) {
	mode := a.cfg.inputMode
	if sc.inputMode != "" {
		mode = sc.inputMode
	}
	var created map[string]string
	if meta := lines[0].Boundary.Session; meta != nil {
		created = meta.Labels
	}
	labels, save, err := a.resumedLabels(ctx, id, created, sc.labels)
	if err != nil {
		return nil, err
	}
	sc.labels = labels
	if sc.contextWindow == nil { // the host's option wins over what the file recorded
		if w, ok := storedWindow(lines); ok {
			sc.contextWindow = &w
		}
	}
	s := a.newSession(id, mode, sc)
	for _, rule := range sc.rules {
		compiled, err := compileRules([]ApprovalRule{rule})
		if err != nil {
			return nil, err
		}
		if _, err := s.reg.addCompiledRule(compiled[0], ScopeSession); err != nil {
			return nil, err
		}
	}
	s.source = "resume"
	if meta := lines[0].Boundary.Session; meta != nil && meta.Agent != "" {
		s.nickname.Store(&meta.Agent) // the stored system was rendered with this name
	}
	s.allowed = storedAllowed(lines) // before prep, so a side chat reopened with its own ForkTools replaces it
	if prep != nil {
		prep(s)
	}
	s, err = a.start(ctx, s, sc, func() { s.resumeInit(lines) })
	if err == nil && save {
		if err = s.saveLabels(ctx); err != nil {
			_ = s.Close(ctx)
			return nil, err
		}
	}
	return s, err
}

// resumeInit restores the current epoch, heals its tail and fires session_start{resume}, whose context waits for the next turn.
func (s *Session) resumeInit(lines []message.Message) {
	ctx, cancel := bounded(s.ctx, s.limits.InitTimeout)
	defer cancel()
	_ = s.mb.push(&initDoneMsg{err: s.restore(ctx, lines)}, false)
}

func (s *Session) restore(ctx context.Context, lines []message.Message) error {
	cur := slices.Clone(store.Current(lines))
	ms, err := s.prepareModel(ctx, *s.agent.cfg.model, len(cur[0].Tools))
	if err != nil {
		return err
	}
	calls, err := s.loadCalls(ctx)
	if err != nil {
		return err
	}
	var unsynced []message.Message
	if err := s.exec(func(st *state) {
		st.model = ms
		st.setWindow()
		st.epoch = cur[0].Epoch
		st.lineSeq = lines[len(lines)-1].Seq
		st.history = cur
		st.stableLen = stablePrefix(cur)
		st.pins = rebuildPins(cur[1:])
		st.system = storedSystem(cur[0])
		st.restoreCalls(s.id, calls, lines)
		s.sides.cut.Store(sideCut(lines))
		for _, m := range cur {
			if m.Role != message.RoleTool {
				continue
			}
			for _, b := range m.Content {
				if r, ok := b.(*message.ToolResultBlock); ok {
					st.executed[r.CallID] = r
				}
			}
		}
		if lost := st.lostTail(s.agent.cfg.texts); lost != nil {
			s.appendLine(st, lost, "")
			s.publishCommitted(st, lost, nil)
		}
		s.restoreTasks(st, lines)
		s.publishSnapshot()
		unsynced = st.unsynced()
	}); err != nil {
		return err
	}
	if err := s.persist(ctx, unsynced); err != nil {
		return err
	}
	sink := &hookSink{}
	fire(withHookScope(ctx, &hookScope{sink: sink}), s, SessionStart, SessionStartInput{HookEnv: s.env("", 0), Source: "resume", Model: ms.info}, nil)
	for _, n := range sink.take() { // the epoch head is already in the file, so this context rides the next turn
		s.post(func(*state) { s.enqueue(s.reminderItem(n.Source, n.Text, false)) })
	}
	return nil
}

// stablePrefix is the longest prefix without a call awaiting its result, the same rule appendLine keeps.
func stablePrefix(lines []message.Message) int {
	n := 0
	for i, m := range lines {
		if !(m.Role == message.RoleAssistant && len(m.ToolCalls()) > 0) {
			n = i + 1
		}
	}
	return n
}

// storedSystem rebuilds the request's system from the stored line, which keeps each section's text but not its slot, so only the first breakpoint applies until the next epoch.
func storedSystem(line message.Message) []provider.SystemBlock {
	var out []provider.SystemBlock
	for _, b := range line.Content {
		if tb, ok := b.(*message.TextBlock); ok {
			out = append(out, provider.SystemBlock{Text: tb.Text})
		}
	}
	return out
}

// storedWindow is the last window a line of this file recorded. Copies repeat an older line's value, so the scan
// takes originals only; a recorded 0 means the session cleared its window and follows the model again.
func storedWindow(lines []message.Message) (int, bool) {
	for i := len(lines) - 1; i >= 0; i-- {
		if m := lines[i]; m.CopyOf == "" && m.ContextWindow != nil {
			return *m.ContextWindow, true
		}
	}
	return 0, false
}

// lostTail pairs the calls the file left without results with lost results; they are never re-run (I2).
func (st *state) lostTail(texts prompt.Source) *message.Message {
	if len(st.history) == 0 {
		return nil
	}
	last := st.history[len(st.history)-1]
	calls := last.ToolCalls()
	if last.Role != message.RoleAssistant || len(calls) == 0 {
		return nil
	}
	text := prompt.TextFrom(texts, "resume_lost")
	tool := &message.Message{Role: message.RoleTool, Origin: message.Origin{Kind: message.OriginTool}}
	for _, c := range calls {
		tool.Content = append(tool.Content, &message.ToolResultBlock{CallID: c.ID, Name: c.Name, IsError: true, Outcome: message.OutcomeLost, Content: message.Content{&message.TextBlock{Text: text}}})
	}
	return tool
}
