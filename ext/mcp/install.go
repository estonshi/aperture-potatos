package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/message"
)

const (
	searchTool = "mcp_search_tools"
	callTool   = "mcp_call_tool"

	maxInstructions = 2000 // characters of a server's instructions in the catalog
	maxSummary      = 120  // characters of a tool's one-line summary
)

// agentInstalls finds the agent-scope install of a session's agent, so a session's own discovery tools also reach the agent's servers.
var agentInstalls sync.Map // *kernel.Agent → *install

// install is the extension installed in one scope: its connections, the registrations they produced, and the mode.
type install struct {
	e       *Extension
	r       kernel.Registrar
	session *kernel.Session // nil at agent scope
	conns   []*conn

	mu     sync.Mutex // orders reconcile, so registrations follow catalogs one at a time
	regs   map[string]*kernel.Registration
	cmds   map[string]*kernel.Registration // one per prompt of a server the host enabled prompts on
	meta   []*kernel.Registration
	search *kernel.Registration // the neutral tool search, while Native
	native atomic.Bool          // Spec marks the tools deferred, read at every freeze
	mode   Mode                 // Direct, Search or Native: what is registered now
	ready  bool                 // Install returned; later changes are announced
	closed bool
	cat    atomic.Pointer[catalog]
}

// catalog is one consistent view of every server's tools and prompts.
type catalog struct {
	byName  map[string]*toolEntry
	prompts map[string]*promptEntry // by command name
	servers []serverView            // in configuration order
}

type serverView struct {
	name         string
	instructions string
	tools        []*toolEntry
}

func newInstall(e *Extension, r kernel.Registrar) *install {
	in := &install{e: e, r: r, session: r.Session(), regs: map[string]*kernel.Registration{}, cmds: map[string]*kernel.Registration{}, mode: Direct}
	in.cat.Store(&catalog{byName: map[string]*toolEntry{}, prompts: map[string]*promptEntry{}})
	for _, s := range e.o.Servers {
		in.conns = append(in.conns, newConn(in, s))
	}
	return in
}

func (in *install) register() error {
	section := Name
	if in.session != nil {
		section = Name + "_session" // the agent's catalog stays beside it rather than shadowed
	}
	if _, err := in.r.PromptSection(kernel.PromptSection{Name: section, Slot: kernel.SlotCatalog, Render: in.render}); err != nil {
		return err
	}
	if in.session != nil {
		_, err := kernel.On(in.r, kernel.SessionEnd, Name+"_close", func(context.Context, kernel.SessionEndInput) (kernel.SessionEndOutput, error) {
			in.close()
			return kernel.SessionEndOutput{}, nil
		})
		return err
	}
	agentInstalls.Store(in.r.Agent(), in)
	in.r.Agent().OnClose(func(context.Context) error {
		in.close() // so the connections, child processes and the agentInstalls entry end with the agent, not only with Extension.Close
		return nil
	})
	return nil
}

// start connects every server at once and waits up to ConnectTimeout for each first attempt; a server still down keeps retrying in the background.
func (in *install) start() {
	for _, c := range in.conns {
		go c.run()
	}
	deadline := time.After(in.e.o.ConnectTimeout)
	for _, c := range in.conns {
		select {
		case <-c.first:
		case <-deadline:
		}
	}
	in.reconcile()
	in.mu.Lock()
	in.ready = true
	in.mu.Unlock()
}

func (in *install) close() {
	in.mu.Lock()
	if in.closed {
		in.mu.Unlock()
		return
	}
	in.closed = true
	in.mu.Unlock()
	if in.session == nil {
		agentInstalls.CompareAndDelete(in.r.Agent(), in)
	}
	var wg sync.WaitGroup
	for _, c := range in.conns {
		wg.Go(c.close)
	}
	wg.Wait()
}

// reconcile turns the connections' current tool lists into registrations, the mode, and what the open sessions are told.
func (in *install) reconcile() {
	in.mu.Lock()
	defer in.mu.Unlock()
	if in.closed {
		return
	}
	prev := in.cat.Load()
	next := in.build()
	mode := in.decide(next)
	in.cat.Store(next) // before registering, since registration reads Spec
	if mode != Search {
		in.dropMeta()
		in.syncSearch(mode == Native)
		redo := in.native.Swap(mode == Native) != (mode == Native) // every definition changes with the deferral
		for name, te := range next.byName {
			reg := in.regs[name]
			if reg == nil {
				r, err := in.r.Tool(&serverTool{in: in, name: name})
				if err != nil {
					in.r.Agent().Logger().Warn("mcp: tool not registered", "tool", name, "error", err)
					continue
				}
				in.regs[name] = r
				continue
			}
			if old := prev.byName[name]; redo || old == nil || !sameSpec(old.spec, te.spec) {
				kernel.ToolChanged(in.r, name)
			}
		}
		for name, reg := range in.regs {
			if next.byName[name] == nil {
				reg.Remove()
				delete(in.regs, name)
			}
		}
	} else {
		in.syncSearch(false)
		in.native.Store(false)
		for name, reg := range in.regs {
			reg.Remove()
			delete(in.regs, name)
		}
		if in.meta == nil {
			for _, t := range []kernel.Tool{&searchMeta{in: in}, &callMeta{in: in}} {
				r, err := in.r.Tool(t)
				if err != nil {
					in.r.Agent().Logger().Warn("mcp: discovery tool not registered", "error", err)
					continue
				}
				in.meta = append(in.meta, r)
			}
		} else if !slices.Equal(serverNames(prev), serverNames(next)) {
			kernel.ToolChanged(in.r, searchTool) // its servers enum lists them
		}
	}
	in.syncCommands(prev, next)
	if in.ready {
		in.tell(prev, next, in.mode, mode)
	}
	in.mode = mode
}

func (in *install) dropMeta() {
	for _, r := range in.meta {
		r.Remove()
	}
	in.meta = nil
}

// syncSearch registers the neutral tool search while Native; one the scope already has, such as the host's own, serves as well.
func (in *install) syncSearch(on bool) {
	switch {
	case on && in.search == nil:
		r, err := in.r.HostedTool(message.HostedTool{Name: "tool_search", ToolSearch: &message.ToolSearch{}})
		if err != nil {
			if !errors.Is(err, kernel.ErrDuplicate) {
				in.r.Agent().Logger().Warn("mcp: tool search not registered", "error", err)
			}
			return
		}
		in.search = r
	case !on && in.search != nil:
		in.search.Remove()
		in.search = nil
	}
}

// build gathers the tools every connection last listed; a disconnected server keeps its tools, so calls fail rather than the tool set changing.
func (in *install) build() *catalog {
	c := &catalog{byName: map[string]*toolEntry{}, prompts: map[string]*promptEntry{}}
	for _, cn := range in.conns {
		instr, tools, prompts, known := cn.snapshot()
		if !known {
			continue
		}
		for _, pe := range prompts {
			if c.prompts[pe.name] != nil {
				in.r.Agent().Logger().Warn("mcp: prompt name taken, prompt skipped", "server", cn.srv.Name, "prompt", pe.mcpName, "name", pe.name)
				continue
			}
			c.prompts[pe.name] = pe
		}
		sv := serverView{name: cn.srv.Name, instructions: instr}
		for _, te := range tools {
			if c.byName[te.name] != nil {
				in.r.Agent().Logger().Warn("mcp: tool name taken, tool skipped", "server", cn.srv.Name, "tool", te.mcpName, "name", te.name)
				continue
			}
			c.byName[te.name] = te
			sv.tools = append(sv.tools, te)
		}
		c.servers = append(c.servers, sv)
	}
	return c
}

// decide keeps discovery once the definitions outgrow DiscoveryRatio until they fall below half of it, so a tool or two never flips the mode back and forth.
func (in *install) decide(c *catalog) Mode {
	if in.e.o.Discovery != Auto {
		return in.e.o.Discovery
	}
	discover := Search
	if in.e.o.NativeSearch {
		discover = Native
	}
	size := 0
	for _, te := range c.byName {
		b, _ := json.Marshal(te.spec)
		size += len(b) / 4
	}
	limit := in.e.o.DiscoveryRatio * float64(in.window())
	if in.mode != Direct {
		if float64(size) < limit/2 {
			return Direct
		}
		return discover
	}
	if float64(size) > limit {
		return discover
	}
	return Direct
}

// window is the session's own context window at session scope; at agent scope the sessions' models may differ, so the option decides.
func (in *install) window() int {
	if in.session != nil {
		if w := in.session.Snapshot().Context.WindowTokens; w > 0 {
			return int(w)
		}
	}
	return in.e.o.ContextWindow
}

// tell queues what changed for the sessions whose catalog was rendered before it.
func (in *install) tell(prev, next *catalog, was, now Mode) {
	var parts []string
	switch {
	case was == now:
	case now == Search:
		lines := []string{text("mode_search")}
		for _, sv := range next.servers {
			for _, te := range sv.tools {
				lines = append(lines, text("tool_item", "name", te.name, "summary", te.summary))
			}
		}
		parts = append(parts, strings.Join(lines, "\n"))
	case now == Native:
		parts = append(parts, text("mode_native"))
	case was == Search:
		parts = append(parts, text("mode_direct")) // from Native the definitions simply load again, and calls by name never changed
	}
	old := map[string]serverView{}
	for _, sv := range prev.servers {
		old[sv.name] = sv
	}
	for _, sv := range next.servers {
		before, had := old[sv.name]
		if !had {
			block := []string{text("connected", "name", sv.name), serverItem(sv, now == Search)}
			if now != Search {
				block = append(block, text("connected_next_turn"))
			}
			parts = append(parts, strings.Join(block, "\n"))
			continue
		}
		if now == Search && was == Search {
			if d := toolDiff(sv.name, before.tools, sv.tools); d != "" {
				parts = append(parts, d)
			}
		}
	}
	if len(parts) > 0 {
		in.say(strings.Join(parts, "\n\n"))
	}
}

func toolDiff(server string, before, after []*toolEntry) string {
	had := map[string]bool{}
	for _, te := range before {
		had[te.name] = true
	}
	var added []string
	for _, te := range after {
		if !had[te.name] {
			added = append(added, text("tool_item", "name", te.name, "summary", te.summary))
		}
		delete(had, te.name)
	}
	if len(added) == 0 && len(had) == 0 {
		return ""
	}
	lines := []string{text("tools_changed", "name", server)}
	if len(added) > 0 {
		lines = append(lines, text("tools_added"))
		lines = append(lines, added...)
	}
	if len(had) > 0 {
		lines = append(lines, text("tools_removed", "names", strings.Join(slices.Sorted(maps.Keys(had)), ", ")))
	}
	return strings.Join(lines, "\n")
}

// say reaches the sessions this install serves with their next turn.
func (in *install) say(msg string) {
	if in.session != nil {
		in.session.Announce(Name, msg, nil)
		return
	}
	in.r.Agent().Announce(Name, msg, nil)
}

// emit publishes an event on the sessions this install serves.
func (in *install) emit(p event.Payload) {
	if in.session != nil {
		in.session.Emit(p)
		return
	}
	in.r.Agent().Announce(Name, "", p)
}

// render is the catalog section; it follows the tool set frozen for this epoch, so it lists tools exactly when they are reached through discovery.
func (in *install) render(_ context.Context, rc kernel.RenderContext) (string, error) {
	c := in.cat.Load()
	if len(c.servers) == 0 {
		return "", nil
	}
	mode := in.epochMode(rc.Tools)
	search := mode == Search
	full := in.items(c, mode, nil)
	if full == "" {
		return "", nil
	}
	key := "catalog"
	if mode == Native {
		key = "catalog_native"
	}
	if search {
		key = "catalog_search"
		budget := int(in.e.o.CatalogRatio * float64(in.window()))
		bare := map[string]bool{}
		order := slices.Clone(c.servers)
		slices.SortStableFunc(order, func(a, b serverView) int { return len(b.tools) - len(a.tools) })
		for _, sv := range order {
			if len(full)/4 <= budget {
				break
			}
			bare[sv.name] = true
			full = in.items(c, Search, bare)
		}
	}
	return text(key, "servers", full), nil
}

// epochMode is how this epoch reaches the install's tools: Native when its own frozen tools are deferred, Direct when they are frozen as they are, Search when only the call tool is.
func (in *install) epochMode(frozen []kernel.ToolSpec) Mode {
	c := in.cat.Load()
	call := false
	for _, t := range frozen {
		if c.byName[t.Name] != nil {
			if t.Deferred {
				return Native
			}
			return Direct
		}
		call = call || t.Name == callTool
	}
	if call {
		return Search
	}
	return Direct
}

// items lists the servers; Direct leaves out those without instructions, since their tools speak for themselves, and only Search lists tools.
func (in *install) items(c *catalog, mode Mode, bare map[string]bool) string {
	var out []string
	for _, sv := range c.servers {
		if mode == Direct && sv.instructions == "" {
			continue
		}
		if bare[sv.name] {
			out = append(out, serverItemBare(sv))
			continue
		}
		out = append(out, serverItem(sv, mode == Search))
	}
	return strings.Join(out, "\n")
}

func serverItem(sv serverView, withTools bool) string {
	lines := []string{text("server_open", "name", attr(sv.name))}
	if sv.instructions != "" {
		lines = append(lines, text("server_instructions", "instructions", sv.instructions))
	}
	if withTools && len(sv.tools) > 0 {
		var items []string
		for _, te := range sv.tools {
			if te.summary == "" {
				items = append(items, text("tool_name_item", "name", te.name))
				continue
			}
			items = append(items, text("tool_item", "name", te.name, "summary", te.summary))
		}
		lines = append(lines, text("server_tools", "tools", strings.Join(items, "\n")))
	}
	lines = append(lines, text("server_close"))
	return strings.Join(lines, "\n")
}

func serverItemBare(sv serverView) string {
	lines := []string{text("server_open", "name", attr(sv.name))}
	if sv.instructions != "" {
		lines = append(lines, text("server_instructions", "instructions", sv.instructions))
	}
	var items []string
	for _, te := range sv.tools {
		items = append(items, text("tool_name_item", "name", te.name))
	}
	if len(items) > 0 {
		lines = append(lines, text("server_tools", "tools", strings.Join(items, "\n")))
	}
	lines = append(lines, text("server_close"))
	return strings.Join(lines, "\n")
}

// attr writes a tag attribute value as a JSON string, as the runtime's other tags do.
func attr(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func serverNames(c *catalog) []string {
	var out []string
	for _, sv := range c.servers {
		out = append(out, sv.name)
	}
	return out
}

func sameSpec(a, b kernel.ToolSpec) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y) && a.Timeout == b.Timeout
}

// installsFor is what a session's discovery tools search: the session's own install first, then its agent's.
func installsFor(own *install, s *kernel.Session) []*install {
	out := []*install{own}
	if own.session != nil && s != nil {
		if v, ok := agentInstalls.Load(s.Registrar().Agent()); ok && v.(*install) != own {
			out = append(out, v.(*install))
		}
	}
	return out
}
