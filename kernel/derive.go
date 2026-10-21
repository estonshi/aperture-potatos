package kernel

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/internal/bus"
	"github.com/estonshi/aperture-potatos/internal/prompt"
	"github.com/estonshi/aperture-potatos/provider"
)

// DeriveOptions says how a derived agent differs from its parent; a zero field inherits.
type DeriveOptions struct {
	Nickname     string
	Description  string
	Instructions *string             // nil inherits the parent's user section
	Model        *provider.ModelRef  // nil inherits
	ToolFilter   func(ToolSpec) bool // also asked about each hosted tool, with a spec that carries only its name
	InheritHooks bool                // audit and other parent hooks keep applying; approval rules and reviewers are inherited regardless, so a child can never escape review
	Use          []Extension
	Limits       *Limits // its zero fields keep the parent's values
}

// derivation is what a derived agent does to the registrations it inherits.
type derivation struct {
	filter       func(ToolSpec) bool
	inheritHooks bool
}

// keeps asks the filter about an inherited tool; a Spec that panics leaves the tool out.
func (d derivation) keeps(t Tool) (ok bool) {
	if d.filter == nil {
		return true
	}
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	return d.filter(t.Spec())
}

// keepsName asks the filter about an inherited hosted tool, which has no Spec, by its name alone; a filter that panics leaves it out.
func (d derivation) keepsName(name string) (ok bool) {
	if d.filter == nil {
		return true
	}
	defer func() {
		if recover() != nil {
			ok = false
		}
	}()
	return d.filter(ToolSpec{Name: name})
}

// Derive makes an agent that shares this one's registrations as a read-only layer under its own, so later changes to the parent reach it.
func (a *Agent) Derive(o DeriveOptions) (*Agent, error) {
	cfg := a.cfg
	cfg.exts, cfg.app, cfg.sinks = nil, nil, nil // installed on the parent, whose registrations the child already sees
	nickname := a.Nickname()
	if o.Nickname != "" {
		if utf8.RuneCountInString(o.Nickname) > 64 {
			return nil, fmt.Errorf("%w: nickname %q", ErrInvalidName, o.Nickname)
		}
		nickname = o.Nickname
	}
	cfg.nickname = nickname
	if o.Description != "" {
		cfg.description = o.Description
	}
	if o.Instructions != nil {
		cfg.instructions, cfg.instructionsFunc = *o.Instructions, nil
	}
	if o.Model != nil {
		if o.Model.Provider == nil {
			return nil, errors.New("derived model has no provider")
		}
		cfg.model = o.Model
	}
	if o.Limits != nil {
		cfg.limits = overLimits(*o.Limits, a.cfg.limits).resolved()
	}
	d := &Agent{cfg: cfg, reg: newRegistry(ScopeAgent), base: a, derived: derivation{filter: o.ToolFilter, inheritHooks: o.InheritHooks},
		nickname: nickname, open: map[string]*Session{}, rules: a.rules}
	names, err := install(&registrar{scope: ScopeAgent, agent: d, reg: d.reg}, o.Use, nil)
	if err != nil {
		return nil, err
	}
	d.exts = slices.Concat(a.exts, names)
	notify := func(c regChange) {
		for _, s := range d.sessions() {
			s.announceRegistry(ScopeAgent, c)
		}
	}
	d.reg.notify.Store(&notify)
	return d, nil
}

// overLimits takes each non-zero field of l and the parent's value elsewhere.
func overLimits(l, parent Limits) Limits {
	lv, pv := reflect.ValueOf(&l).Elem(), reflect.ValueOf(parent)
	for i := range lv.NumField() {
		if lv.Field(i).IsZero() {
			lv.Field(i).Set(pv.Field(i))
		}
	}
	return l
}

// view is the agent-scope registrations its sessions see: its own, over its parent's for a derived agent.
func (a *Agent) view() *regState {
	own := a.reg.state()
	if a.base == nil {
		return own
	}
	base := a.base.view()
	return a.views.get(base, own, func() *regState { return layered(base, own, a.derived) })
}

// outer is what a session inherits under its own scope: its agent's view or, for a ChildOf session, its owner's effective registrations with the agent's derivation applied.
func (s *Session) outer() *regState {
	if s.owner == nil {
		return s.agent.view()
	}
	base, own := s.owner.owner.effective(), s.agent.reg.state()
	return s.outerView.get(base, own, func() *regState { return layered(base, own, s.agent.derived) })
}

// effective is the registrations the session's model works with, which its ChildOf sessions start from.
func (s *Session) effective() *regState {
	outer, own := s.outer(), s.reg.state()
	return s.effView.get(outer, own, func() *regState { return layered(outer, own, derivation{inheritHooks: true}) })
}

// viewCache keeps the last layering, so a view is rebuilt only after one of its layers changed.
type viewCache struct{ last atomic.Pointer[cachedView] }

type cachedView struct{ base, own, out *regState }

func (c *viewCache) get(base, own *regState, build func() *regState) *regState {
	if v := c.last.Load(); v != nil && v.base == base && v.own == own {
		return v.out
	}
	out := build()
	c.last.Store(&cachedView{base: base, own: own, out: out})
	return out
}

// layered puts own over base: what own hides or the filter refuses drops out, only reviewers pass among hooks unless hooks are inherited, and own shadows base by name.
func layered(base, own *regState, d derivation) *regState {
	st := newRegistry(ScopeAgent).state()
	for n, r := range base.tools {
		if !own.hides(KindTool, n) && d.keeps(r.tool) {
			st.tools[n] = r
		}
	}
	maps.Copy(st.tools, own.tools)
	for n, r := range base.hosted {
		if !own.hides(KindHostedTool, n) && d.keepsName(n) {
			st.hosted[n] = r
		}
	}
	maps.Copy(st.hosted, own.hosted)
	for n, r := range base.commands {
		if !own.hides(KindCommand, n) {
			st.commands[n] = r
		}
	}
	for alias, n := range base.aliases {
		if r, ok := st.commands[n]; ok && r == base.commands[n] {
			st.aliases[alias] = n
		}
	}
	maps.Copy(st.commands, own.commands)
	maps.Copy(st.aliases, own.aliases)
	for n, r := range base.sections {
		if !own.hides(KindPromptSection, n) {
			st.sections[n] = r
		}
	}
	maps.Copy(st.sections, own.sections)
	for n, r := range base.providers {
		if !own.hides(KindContextProvider, n) {
			st.providers[n] = r
		}
	}
	maps.Copy(st.providers, own.providers)
	maps.Copy(st.taskKinds, base.taskKinds)
	maps.Copy(st.taskKinds, own.taskKinds)
	st.compactor = orElse(own.compactor, base.compactor)
	for id, hs := range base.hooks {
		if d.inheritHooks || id == Approval.meta.id {
			st.hooks[id] = slices.Clone(hs)
		}
	}
	for id, hs := range own.hooks {
		for _, h := range hs {
			if !slices.Contains(st.hooks[id], h) { // a ChildOf session of its owner's own agent sees that agent twice
				st.hooks[id] = append(st.hooks[id], h)
			}
		}
	}
	return st
}

func orElse[T any](a, b *T) *T {
	if a != nil {
		return a
	}
	return b
}

// reviewerChain is the reviewers of every layer a session descends from, outermost first, then its own in hook order (runtime.md §13.10).
func (s *Session) reviewerChain() []*hookEntry {
	var head []*hookEntry
	if s.owner != nil {
		head = s.owner.owner.reviewerChain()
	} else if s.agent.base != nil {
		head = s.agent.base.reviewerChain()
	}
	return withHead(head, s.hookEntries(Approval.meta))
}

func (a *Agent) reviewerChain() []*hookEntry {
	var head []*hookEntry
	if a.base != nil {
		head = a.base.reviewerChain()
	}
	own := slices.Clone(a.reg.state().hooks[Approval.meta.id])
	sortHooks(own)
	return withHead(head, own)
}

func withHead(head, rest []*hookEntry) []*hookEntry {
	return slices.Concat(head, slices.DeleteFunc(rest, func(h *hookEntry) bool { return slices.Contains(head, h) }))
}

// unionRules keeps the outer rules first and adds the inner ones it lacks, since the rules of every layer apply.
func unionRules(outer, inner []compiledRule) []compiledRule {
	out := slices.Clone(outer)
	for _, r := range inner {
		if !slices.ContainsFunc(outer, func(o compiledRule) bool { return o.ID == r.ID }) {
			out = append(out, r)
		}
	}
	return out
}

// Model is the model the session's next turn uses, so a derived agent can start from where its parent is.
func (s *Session) Model() provider.ModelRef {
	ref := *s.agent.cfg.model
	_ = s.exec(func(st *state) {
		ref = st.model.ref
		if st.pendingSwitch != nil {
			ref = st.pendingSwitch.ref
		}
	})
	return ref
}

// ---- ChildOf ----

// ChildOf makes a child session owned by a task: its events are relayed under the task hop, its model calls reach the owner's call ledger, and waits on its ancestors are re-entrant.
func ChildOf(rt TaskRuntime) SessionOption { return func(c *sessionConfig) { c.owner = rt } }

// childSet is the child sessions one task owns across its attempts, and the attempt they follow.
type childSet struct {
	owner   *Session
	mu      sync.Mutex
	kids    []*childLink
	rt      atomic.Pointer[taskRuntime] // the attempt their hop and calls follow, kept between attempts
	running atomic.Bool                 // an attempt runs, so the children may start turns of their own
}

// childLink relays one child's events to the owner's bus.
type childLink struct {
	s    *Session
	sub  *bus.Sub
	fwd  atomic.Uint64 // seq of the child's last event handed to the owner
	tick chan struct{}
	done chan struct{}
}

// adopt ties s to the task before its actor starts, so no event of it goes unrelayed and no call unbooked.
func (s *Session) adopt(o TaskRuntime) error {
	rt, ok := o.(*taskRuntime)
	if !ok || !rt.live.Load() {
		return errors.New("ChildOf takes the runtime of an attempt that is running")
	}
	s.chain = append(slices.Clone(rt.s.chain), s.id)
	s.owner = rt.kids
	s.rules = unionRules(rt.s.rules, s.rules)
	s.st.owned = rt.kids
	rt.kids.add(s)
	return nil
}

func (c *childSet) add(s *Session) {
	d := defaults().Subscription
	l := &childLink{s: s, tick: make(chan struct{}, 1), done: make(chan struct{})}
	l.fwd.Store(s.bus.Seq())
	l.sub = s.bus.Subscribe(bus.Options{Policy: bus.Block, HighWater: d.HighWater, HardLimit: d.HardLimit, StallTimeout: -1, MaxDepth: -1})
	c.mu.Lock()
	c.kids = append(c.kids, l)
	c.mu.Unlock()
	go c.relay(l)
}

// relay republishes the child's events under the task hop; a bulk event waits for the owner's credit, so a slow owner subscriber slows the child down.
func (c *childSet) relay(l *childLink) {
	defer close(l.done)
	defer c.remove(l)
	defer l.sub.Finish()
	for {
		ev, ok := l.sub.Pop(context.Background(), l.s.bus)
		if !ok {
			return
		}
		out := *ev
		hop := c.rt.Load().hop()
		hop.Seq = ev.Seq
		out.Path = append([]event.Hop{hop}, ev.Path...)
		if bp, ok := ev.Data.(event.Bulk); ok && bp.Bulk() {
			_ = c.owner.bus.WaitWritable(c.owner.ctx)
		}
		c.owner.bus.Publish(&out)
		l.fwd.Store(ev.Seq)
		select {
		case l.tick <- struct{}{}:
		default:
		}
	}
}

func (c *childSet) remove(l *childLink) {
	c.mu.Lock()
	c.kids = slices.DeleteFunc(c.kids, func(k *childLink) bool { return k == l })
	c.mu.Unlock()
}

func (c *childSet) list() []*childLink {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.kids)
}

// flush waits until what the child has published so far reached the owner.
func (l *childLink) flush(ctx context.Context) {
	target := l.s.bus.Seq()
	for l.fwd.Load() < target {
		select {
		case <-l.tick:
		case <-l.done:
			return
		case <-ctx.Done():
			return
		}
	}
}

// bind points the children at a new attempt and lets them look for the work they held back meanwhile.
func (c *childSet) bind(rt *taskRuntime) {
	c.rt.Store(rt)
	c.running.Store(true)
	for _, l := range c.list() {
		l.s.post(func(*state) {})
	}
}

// release holds the children once an attempt has returned: their tasks stop, and what they published reaches the owner before task.finished.
func (c *childSet) release(timeout time.Duration) {
	c.running.Store(false)
	kids := c.list()
	if len(kids) == 0 {
		return
	}
	ctx, cancel := bounded(context.Background(), timeout)
	defer cancel()
	for _, l := range kids {
		_ = l.s.exec(func(st *state) { l.s.stopAll(st, "the task that owns this session ended") })
	}
	for _, l := range kids {
		l.flush(ctx)
	}
}

// closeAll closes the children and waits for their last events to reach the owner.
func (c *childSet) closeAll(ctx context.Context) { closeLinks(ctx, c.take()) }

// take detaches the children from the set, so the ones a next attempt opens are not among them.
func (c *childSet) take() []*childLink {
	c.mu.Lock()
	defer c.mu.Unlock()
	kids := c.kids
	c.kids = nil
	return kids
}

func closeLinks(ctx context.Context, kids []*childLink) {
	var wg sync.WaitGroup
	for _, l := range kids {
		wg.Go(func() {
			_ = l.s.Close(ctx)
			select {
			case <-l.done:
			case <-ctx.Done():
			}
		})
	}
	wg.Wait()
}

// hosts reports whether the caller runs in one of the task's child sessions or below them, where waiting on the task waits on the caller.
func (e *taskEntry) hosts(ctx context.Context) bool {
	m, ok := ctx.Value(markerKey{}).(marker)
	if !ok || e.kids == nil {
		return false
	}
	return slices.ContainsFunc(e.kids.list(), func(l *childLink) bool { return slices.Contains(m.chain, l.s.id) })
}

// rootPath is the hops from the root session to this one: each owning task's hop, outermost first.
func (s *Session) rootPath() []event.Hop {
	if s.owner == nil {
		return nil
	}
	return append(s.owner.owner.rootPath(), s.owner.rt.Load().hop())
}

// held says the owning task has no running attempt, so the child starts no turn of its own.
func (st *state) held() bool { return st.owned != nil && !st.owned.running.Load() }

// stoppedByReviewer fails the owning attempt once the turn a reviewer stopped here has ended, so that turn still ends as the reviewer said.
func (s *Session) stoppedByReviewer(reason string) {
	if s.owner == nil {
		return
	}
	s.post(func(st *state) {
		if st.turn == nil {
			s.ownerDenied(reason)
			return
		}
		st.deniedStop = &reason
	})
}

// ownerDenied fails the owning attempt after a reviewer's deny_and_stop here, unless a stop came first.
func (s *Session) ownerDenied(reason string) {
	c := s.owner
	if c == nil {
		return
	}
	rt := c.rt.Load()
	text := prompt.Textf("denied_by_reviewer", map[string]string{"reason": reason})
	c.owner.post(func(st *state) {
		e := rt.current(st)
		if e == nil || e.cur.stopReq || e.cur.denied != "" {
			return
		}
		e.cur.denied = text
		e.cur.cancel(errDeniedAndStop)
		c.owner.graceTimer(e.id, e.cur)
	})
}

// closeChildren closes the child sessions of a task that restarts or is evicted, off the actor.
func (s *Session) closeChildren(e *taskEntry) {
	if e.kids == nil {
		return
	}
	kids := e.kids.take() // now, on the actor, since the worker may run after the next attempt opened children
	if len(kids) == 0 {
		return
	}
	s.wk.Go("children "+e.id, func() {
		ctx, cancel := bounded(context.Background(), s.limits.CloseTimeout)
		defer cancel()
		closeLinks(ctx, kids)
	})
}

var errDeniedAndStop = errors.New("a reviewer stopped the task")
