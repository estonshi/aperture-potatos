package kernel

import (
	"context"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/internal/tokens"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/store"
)

// sideChatName keeps the name rule of runtime.md §12.8 even though a command's name now allows ":".
var sideChatName = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

// ForkOption configures Fork and SideChat.
type ForkOption func(*forkConfig)

type forkConfig struct {
	at      string
	tools   []string
	limited bool
	sinks   []sinkSpec
}

// ForkAt forks before a line of the current epoch, named by an input it delivered or by its turn. An input a merged
// line delivered names the whole line, so the cut lands before all of it; the default is the current stable prefix.
func ForkAt(id string) ForkOption { return func(c *forkConfig) { c.at = id } }

// ForkTools is a call-time allow-list; the tool definitions sent stay the source's, so the cached prefix holds.
func ForkTools(names ...string) ForkOption {
	return func(c *forkConfig) { c.tools, c.limited = append(c.tools, names...), true }
}

// ForkSink attaches ch when the new session is created, so its session.started and copied lines are never missed.
func ForkSink(ch chan<- event.Event, opts ...SubscribeOption) ForkOption {
	return func(c *forkConfig) { c.sinks = append(c.sinks, sinkSpec{ch: ch, opts: newSubscribeConfig(opts)}) }
}

// ForkFunc is ForkSink with a callback, so fn sees the new session's first events too.
func ForkFunc(fn func(ctx context.Context, ev event.Event) error, opts ...SubscribeOption) ForkOption {
	return func(c *forkConfig) { c.sinks = append(c.sinks, sinkSpec{fn: fn, opts: newSubscribeConfig(opts)}) }
}

// forkSource is what a fork reads from the source's actor, so the new session never touches the source's state.
type forkSource struct {
	id       string
	lines    []message.Message // system line first
	system   []provider.SystemBlock
	model    modelState
	meter    tokens.Estimate
	learned  map[provider.ModelInfo]*learned
	main     *mainView // a side chat being opened: the main part it weighs from the start
	fromSide bool      // the source is a side chat, so the copied lines follow a main part other than the one their thinking was made on
	window   *int      // the source's own context window, which the new session keeps; nil follows the agent's
}

// Fork copies the current context into a new independent session of the same agent, stored under its own id and resumable like any other; a side chat's context is its main part followed by its own lines.
func (s *Session) Fork(ctx context.Context, opts ...ForkOption) (*Session, error) {
	return s.fork(ctx, s.agent.cfg.ids.NewID("ses"), nil, "", nil, forkOptions(opts))
}

func forkOptions(opts []ForkOption) forkConfig {
	var fc forkConfig
	for _, o := range opts {
		o(&fc)
	}
	return fc
}

// fork creates the session id from s: parent and name are set for a side chat, which copies no line, and stored is what its file already holds after a drop.
func (s *Session) fork(ctx context.Context, id string, parent *Session, name string, stored []message.Message, fc forkConfig) (*Session, error) {
	var main *mainView
	if s.parent != nil { // a fork of a side chat starts with its main part as it is now
		v, err := s.parent.view()
		if err != nil {
			return nil, err
		}
		main = v
	}
	var src forkSource
	var err error
	if e := s.exec(func(st *state) {
		cut := st.stableLen
		if fc.at != "" {
			hist := st.history[1:]
			at, _, _ := cutLine(hist, headLen(hist), fc.at)
			if at < 0 {
				err = unknownCutError{fc.at}
				return
			}
			cut = at + 1
		}
		var window *int
		if st.windowSet {
			w := st.policy.MaxContextTokens
			window = &w
		}
		lines := slices.Clone(st.history[:cut])
		src = forkSource{id: s.id, system: slices.Clone(st.system), model: st.model, meter: st.meter, learned: map[provider.ModelInfo]*learned{}, window: window}
		switch {
		case parent != nil:
			lines, src.main = lines[:1], st.view(s.agent.cfg.texts)
			src.meter.Reset() // the side chat's own lines start here; the main session's estimate stands for its part
		case main != nil:
			lines, src.fromSide = slices.Concat(lines[:1], main.lines[:main.stable], lines[1:]), true
			src.meter.Reset()
		}
		src.lines = lines
		for k, v := range st.learned {
			l := *v
			src.learned[k] = &l
		}
	}); e != nil {
		return nil, e
	}
	if err != nil {
		return nil, err
	}
	ns := s.agent.newSession(id, s.inputMode, sessionConfig{labels: maps.Clone(s.labelMap()), instructions: s.sessInstr, contextWindow: src.window})
	ns.nickname.Store(s.nickname.Load()) // the copied system was rendered with this name
	s.inherit(ns, parent, fc)
	ns.sideName = name
	ns.source = "fork"
	var meta *message.SessionMeta
	if len(stored) == 0 {
		meta = &message.SessionMeta{Agent: ns.nick(), SideChat: name, Labels: ns.labelMap(), SpecVersion: specVersion()}
		if parent != nil {
			meta.ParentSessionID = parent.id
		}
	}
	return s.agent.start(ctx, ns, sessionConfig{sinks: fc.sinks}, func() {
		ictx, cancel := bounded(ns.ctx, ns.limits.InitTimeout)
		defer cancel()
		_ = ns.mb.push(&initDoneMsg{err: ns.openFork(ictx, src, meta, stored)}, false)
	})
}

// inherit gives ns what a fork keeps of s: the session-scope registrations, rules included, as they are now, and the fork's own options.
func (s *Session) inherit(ns *Session, parent *Session, fc forkConfig) {
	ns.exts = slices.Clone(s.exts[len(s.agent.exts):])
	ns.parent = parent
	s.reg.mu.Lock()
	ns.reg.cur.Store(s.reg.cur.Load()) // copy-on-write, so sharing the state is a copy
	ns.reg.next = s.reg.next
	s.reg.mu.Unlock()
	if fc.limited {
		ns.allowed = map[string]bool{}
		for _, n := range fc.tools {
			ns.allowed[n] = true
		}
	}
}

// openFork writes the fork's epoch: the source's system line and lines verbatim, so the new session's first request hits the source's cache; a side chat's epoch has its head in place of lines (runtime.md §12.8).
func (s *Session) openFork(ctx context.Context, src forkSource, meta *message.SessionMeta, stored []message.Message) error {
	last := src.lines[len(src.lines)-1]
	b := &message.Boundary{Reason: "fork", From: &message.ForkFrom{SessionID: src.id, Seq: last.Seq}, Session: meta}
	b.ReplayFallback = src.lines[0].Boundary != nil && src.lines[0].Boundary.ReplayFallback // copied lines that followed a changed prefix still do
	if src.fromSide && (src.model.caps.ThinkingPrefixBound || src.learned[src.model.info] != nil && src.learned[src.model.info].prefixBound) {
		b.ReplayFallback = true // the side chat's thinking was made on the main part of its own turns
	}
	if src.main != nil {
		b.Reason, b.From.Seq, b.ReplayFallback = "side_chat", src.main.seq, false
	}
	if s.allowed != nil {
		names := append([]string{}, slices.Sorted(maps.Keys(s.allowed))...) // never nil, since null would read back as no limit
		b.ToolsAllowed = &names
	}
	var unsynced []message.Message
	if err := s.exec(func(st *state) {
		st.model, st.learned, st.system, st.meter = src.model, src.learned, src.system, src.meter
		st.setWindow()
		if len(stored) > 0 {
			st.epoch, st.lineSeq = stored[len(stored)-1].Epoch, stored[len(stored)-1].Seq
		}
		st.epoch++
		sys := &message.Message{Role: message.RoleSystem, Origin: message.Origin{Kind: message.OriginSystem}, Content: src.lines[0].Content, Tools: src.lines[0].Tools, HostedTools: src.lines[0].HostedTools, Boundary: b}
		s.appendLine(st, sys, "")
		s.publishCommitted(st, sys, nil)
		for _, m := range src.lines[1:] {
			s.appendCopy(st, m, nil)
		}
		if src.main != nil {
			head, info := delivery(s.agent.cfg.texts, nil, nil, s.sideHead(nil))
			head.Origin = message.Origin{Kind: message.OriginEpochHead}
			s.appendLine(st, head, "")
			s.publishCommitted(st, head, &lineEvents{delivery: &info})
			st.mainTokens, st.mainLines = src.main.tokens, len(src.main.lines)
		}
		st.pins = rebuildPins(st.history[1:])
		st.context.UsedTokens = st.estimate()
		s.publishSnapshot()
		unsynced = st.unsynced()
	}); err != nil {
		return err
	}
	if err := s.persist(ctx, unsynced); err != nil {
		return err
	}
	sink := &hookSink{}
	fire(withHookScope(ctx, &hookScope{sink: sink}), s, SessionStart, SessionStartInput{HookEnv: s.env("", 0), Source: "fork", Model: src.model.info}, nil)
	notes := sink.take() // the epoch head is the copied one, so this context rides the first turn
	if s.allowed != nil {
		names := slices.Sorted(maps.Keys(s.allowed))
		list := strings.Join(names, ", ")
		if list == "" {
			list = "none"
		}
		notes = append(notes, &message.ReminderBlock{Source: "tools", Text: s.textf("tools_allowed", map[string]string{"names": list})})
	}
	for _, n := range notes {
		s.post(func(*state) { s.enqueue(s.reminderItem(n.Source, n.Text, false)) })
	}
	return s.saveLabels(ctx)
}

// storedAllowed is the allow-list the latest fork epoch of stored lines recorded, or nil for none.
func storedAllowed(lines []message.Message) map[string]bool {
	for i := len(lines) - 1; i >= 0; i-- {
		if b := lines[i].Boundary; b != nil && (b.Reason == "fork" || b.Reason == "side_chat") {
			if b.ToolsAllowed == nil {
				return nil
			}
			allowed := map[string]bool{}
			for _, n := range *b.ToolsAllowed {
				allowed[n] = true
			}
			return allowed
		}
	}
	return nil
}

// Parent is a side chat's main session, or the session whose task owns a ChildOf session; nil for a root session or a fork.
func (s *Session) Parent() *Session {
	if s.owner != nil {
		return s.owner.owner
	}
	return s.parent
}

// sideChats are a session's open side chats by name; closed marks that the session closed them and opens no more.
type sideChats struct {
	mu     sync.Mutex // guards the maps and closed only, never held across store I/O or a close
	open   map[string]*Session
	names  map[string]*nameLock // the calls opening, dropping or closing a name, one at a time, so a slow store holds up only that name
	closed bool
	cut    atomic.Int64 // seq of the last line that dropped them; atomic because the actor sets it while a name may wait on a closing side chat
}

type nameLock struct {
	ch   chan struct{} // holds one token while a call works on the name
	refs int
}

// lockName waits its turn for name and returns the release.
func (sc *sideChats) lockName(ctx context.Context, name string) (func(), error) {
	sc.mu.Lock()
	if sc.names == nil {
		sc.names = map[string]*nameLock{}
	}
	l := sc.names[name]
	if l == nil {
		l = &nameLock{ch: make(chan struct{}, 1)}
		sc.names[name] = l
	}
	l.refs++
	sc.mu.Unlock()
	leave := func() {
		sc.mu.Lock()
		if l.refs--; l.refs == 0 {
			delete(sc.names, name)
		}
		sc.mu.Unlock()
	}
	select {
	case l.ch <- struct{}{}:
		return func() { <-l.ch; leave() }, nil
	case <-ctx.Done():
		leave()
		return nil, context.Cause(ctx)
	}
}

// busy is every name open or being worked on, so a sweep also waits for an open still in flight.
func (sc *sideChats) busy() []string {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	names := maps.Clone(sc.open)
	if names == nil {
		names = map[string]*Session{}
	}
	for n := range sc.names {
		names[n] = nil
	}
	return slices.Sorted(maps.Keys(names))
}

func (sc *sideChats) get(name string) (*Session, bool) {
	sc.mu.Lock()
	defer sc.mu.Unlock()
	return sc.open[name], sc.closed
}

func (s *Session) sideID(name string) string { return s.id + ".side." + name }

// SideChat opens the named side chat of this session, which sends this session's current context ahead of its own lines every turn; later opens return the same chat until DropSideChat.
func (s *Session) SideChat(ctx context.Context, name string, opts ...ForkOption) (*Session, error) {
	if !sideChatName.MatchString(name) {
		return nil, fmt.Errorf("%w: side chat %q", ErrInvalidName, name)
	}
	fc := forkOptions(opts)
	switch {
	case s.parent != nil:
		return nil, fmt.Errorf("%w: a side chat opens no side chat", ErrSideChatUnsupported)
	case fc.at != "":
		return nil, fmt.Errorf("%w: ForkAt", ErrSideChatUnsupported)
	}
	unlock, err := s.sides.lockName(ctx, name)
	if err != nil {
		return nil, err
	}
	defer unlock()
	c, closed := s.sides.get(name)
	if closed {
		return nil, ErrSessionClosed
	}
	if c != nil {
		if ph := phase(c.snap.Load().Phase); ph != phaseClosing && ph != phaseClosed {
			return c, nil
		}
		select { // closed by its host: reopen it from the store once its id is free
		case <-c.done:
		case <-ctx.Done():
			return nil, context.Cause(ctx)
		}
	}
	id := s.sideID(name)
	lines, err := s.agent.cfg.store.Load(ctx, id)
	if err != nil {
		return nil, err
	}
	if live(lines) && (forkedAt(lines) < s.sides.cut.Load() || copiedMain(lines)) { // a rewind or clear came after it while it was closed or before its mark landed, or it holds an old copy of this session
		if lines, err = s.markDropped(ctx, id, lines); err != nil {
			return nil, err
		}
	}
	if live(lines) {
		c, err = s.agent.resume(ctx, id, lines, sessionConfig{inputMode: s.inputMode, instructions: s.sessInstr, sinks: fc.sinks}, func(c *Session) { s.inherit(c, s, fc); c.sideName = name })
		if err == nil {
			if err = c.noteMain(); err != nil {
				_ = c.Close(ctx)
			}
		}
	} else {
		c, err = s.fork(ctx, id, s, name, lines, fc)
	}
	if err != nil {
		return nil, err
	}
	s.sides.mu.Lock()
	if closed = s.sides.closed; !closed {
		if s.sides.open == nil {
			s.sides.open = map[string]*Session{}
		}
		s.sides.open[name] = c
	}
	s.sides.mu.Unlock()
	if closed { // the main session began closing while this one opened
		_ = c.Close(ctx)
		return nil, ErrSessionClosed
	}
	return c, nil
}

// live says whether stored lines hold a session whose current epoch was not dropped.
func live(lines []message.Message) bool {
	if len(lines) == 0 || lines[0].Boundary == nil {
		return false
	}
	return store.Current(lines)[0].Boundary.Reason != "dropped"
}

// DropSideChat discards the named side chat, so the next open starts it anew; its lines stay in its file for audit.
func (s *Session) DropSideChat(ctx context.Context, name string) error {
	unlock, err := s.sides.lockName(ctx, name)
	if err != nil {
		return err
	}
	defer unlock()
	if c, _ := s.sides.get(name); c != nil {
		if err := c.Close(ctx); err != nil {
			return err
		}
		s.sides.mu.Lock()
		delete(s.sides.open, name)
		s.sides.mu.Unlock()
	}
	id := s.sideID(name)
	lines, err := s.agent.cfg.store.Load(ctx, id)
	if err != nil {
		return err
	}
	if !live(lines) {
		return fmt.Errorf("%w: side chat %q", ErrSessionNotFound, name)
	}
	_, err = s.markDropped(ctx, id, lines)
	return err
}

// markDropped appends the dropped mark to a side chat's stored lines and returns them with it.
func (s *Session) markDropped(ctx context.Context, id string, lines []message.Message) ([]message.Message, error) {
	last := lines[len(lines)-1]
	mark := message.Message{V: message.Version, ID: s.agent.cfg.ids.NewID("msg"), SessionID: id, Seq: last.Seq + 1, Epoch: last.Epoch + 1, Role: message.RoleSystem,
		Origin: message.Origin{Kind: message.OriginSystem}, Content: message.Content{}, Boundary: &message.Boundary{Reason: "dropped"}, CreatedAt: s.agent.cfg.clock.Now()}
	if err := s.agent.cfg.store.Append(ctx, id, []message.Message{mark}); err != nil {
		return nil, err
	}
	return append(lines, mark), nil
}

// dropsSides is the side_chat.dropped reason when m opens an epoch after which this session's side chats talk about what is gone; a compaction or a switch only reshapes the context they carry, so neither is one.
func dropsSides(m *message.Message) (string, bool) {
	if m.Role != message.RoleSystem || m.Boundary == nil {
		return "", false
	}
	switch m.Boundary.Reason {
	case "clear":
		return "cleared", true
	case "rewind":
		return "rewound", true
	}
	return "", false
}

// sideCut is the seq of the last stored line that dropped side chats, which a resumed session starts from.
func sideCut(lines []message.Message) int64 {
	for i := len(lines) - 1; i >= 0; i-- {
		if _, ok := dropsSides(&lines[i]); ok {
			return lines[i].Seq
		}
	}
	return 0
}

// forkedAt is the seq of the main session's last line when a side chat last opened, or of the last line an older version's fork copied.
func forkedAt(lines []message.Message) int64 {
	for i := len(lines) - 1; i >= 0; i-- {
		if b := lines[i].Boundary; b != nil && (b.Reason == "side_chat" || b.Reason == "fork") && b.From != nil {
			return b.From.Seq
		}
	}
	return 0
}

// copiedMain says a side chat last opened as a copy of its main session, as before every request carried the main part; sending both would repeat the main session.
func copiedMain(lines []message.Message) bool {
	for i := len(lines) - 1; i >= 0; i-- {
		if b := lines[i].Boundary; b != nil && (b.Reason == "side_chat" || b.Reason == "fork") {
			return b.Reason == "fork"
		}
	}
	return false
}

// dropSides runs in the history-token holder, so side chats a written line dropped are gone before this session does anything next.
func (s *Session) dropSides(lines []message.Message) {
	for i := len(lines) - 1; i >= 0; i-- {
		if reason, ok := dropsSides(&lines[i]); ok {
			s.sides.cut.Store(lines[i].Seq)
			s.dropOpenSides(reason, lines[i].Seq)
			return
		}
	}
}

// dropOpenSides closes and marks every open side chat opened before cut; one opened after it talks about the new epoch and stays.
func (s *Session) dropOpenSides(reason string, cut int64) {
	ctx := context.WithoutCancel(s.ctx) // the mark must land even when this session is closing
	for _, name := range s.sides.busy() {
		s.dropOpenSide(ctx, name, reason, cut)
	}
}

func (s *Session) dropOpenSide(ctx context.Context, name, reason string, cut int64) {
	unlock, _ := s.sides.lockName(ctx, name) // ctx is never canceled
	defer unlock()
	c, _ := s.sides.get(name)
	if c == nil {
		return
	}
	lines, err := s.agent.cfg.store.Load(ctx, c.id)
	if err != nil {
		s.post(func(*state) {
			s.publish(&event.Error{Code: "store_failed", Message: "side chat " + name + ": " + err.Error()})
		})
		return
	}
	if !live(lines) || forkedAt(lines) >= cut {
		return
	}
	_ = c.Close(ctx)
	s.sides.mu.Lock()
	delete(s.sides.open, name)
	s.sides.mu.Unlock()
	if lines, err = s.agent.cfg.store.Load(ctx, c.id); err == nil && live(lines) { // reloaded, since the closing turn wrote more
		_, err = s.markDropped(ctx, c.id, lines)
	}
	s.post(func(*state) {
		if err != nil {
			s.publish(&event.Error{Code: "store_failed", Message: "side chat " + name + ": " + err.Error()}) // SideChat marks it on the next open
		}
		s.publish(&event.SideChatDropped{Name: name, Reason: reason})
	})
}

// SideChats lists the names of this session's side chats that are open, or stored and not dropped.
func (s *Session) SideChats(ctx context.Context) ([]string, error) {
	prefix := s.sideID("")
	ids, err := s.agent.cfg.store.List(ctx, prefix)
	if err != nil {
		return nil, err
	}
	names := map[string]bool{}
	for _, id := range ids {
		name := strings.TrimPrefix(id, prefix)
		if !sideChatName.MatchString(name) { // a side chat of a side chat
			continue
		}
		lines, err := s.agent.cfg.store.Load(ctx, id)
		if err != nil {
			return nil, err
		}
		if live(lines) && forkedAt(lines) >= s.sides.cut.Load() && !copiedMain(lines) {
			names[name] = true
		}
	}
	s.sides.mu.Lock()
	for name, c := range s.sides.open {
		if ph := phase(c.snap.Load().Phase); ph != phaseClosing && ph != phaseClosed {
			names[name] = true
		}
	}
	s.sides.mu.Unlock()
	return slices.Sorted(maps.Keys(names)), nil
}

// closeSides closes the open side chats before their main session, and opens no more.
func (s *Session) closeSides(ctx context.Context) {
	s.sides.mu.Lock()
	s.sides.closed = true
	s.sides.mu.Unlock()
	for _, name := range s.sides.busy() { // an open in flight sees closed and closes its own chat
		if unlock, err := s.sides.lockName(ctx, name); err == nil {
			unlock()
		}
	}
	s.sides.mu.Lock()
	open := slices.Collect(maps.Values(s.sides.open))
	s.sides.mu.Unlock()
	var wg sync.WaitGroup
	for _, c := range open {
		wg.Go(func() { _ = c.Close(ctx) })
	}
	wg.Wait()
}
