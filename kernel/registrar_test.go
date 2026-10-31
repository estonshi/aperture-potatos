package kernel_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/providertest"
)

func TestRuntimeRegistrationTakesEffectNextTurn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, [][]providertest.Step{textReply("a"), textReply("b"), textReply("c")})
		reg, err := h.sess.Registrar().Tool(&echoTool{name: "late"})
		if err != nil {
			t.Fatal(err)
		}
		rc, _ := h.sess.Send(kernel.Text("one"))
		rc.Wait(t.Context())
		reg.Remove()
		reg.Remove()
		rc, _ = h.sess.Send(kernel.Text("two"))
		rc.Wait(t.Context())
		if _, err := h.sess.Registrar().Tool(&echoTool{name: "bad name"}); !errors.Is(err, kernel.ErrInvalidName) {
			t.Fatalf("bad name: %v", err)
		}
		h.close()
		reqs := h.sp.Requests()
		if len(reqs[0].Tools) != 1 || len(reqs[1].Tools) != 0 {
			t.Fatalf("tools %d then %d", len(reqs[0].Tools), len(reqs[1].Tools))
		}
		var changes []string
		for _, ev := range h.rec.all() {
			if d, ok := ev.Data.(*event.RegistryChanged); ok {
				changes = append(changes, d.Op+":"+d.Effective)
			}
		}
		if !slices.Equal(changes, []string{"add:turn", "remove:turn"}) {
			t.Fatalf("changes %v", changes)
		}
	})
}

func TestAgentSubscribeAndAgentScopeRegistry(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var reg kernel.Registrar
		keep := kernel.ExtensionFunc("keeper", func(r kernel.Registrar) error { reg = r; return nil })
		sp := providertest.NewScriptProvider(provider.APIAnthropicMessages)
		ag, err := kernel.New(kernel.WithModel(provider.ModelRef{Provider: sp, Model: "m"}), kernel.Use(keep))
		if err != nil {
			t.Fatal(err)
		}
		first, _ := ag.NewSession(t.Context())
		var mu sync.Mutex
		seen := map[string][]string{}
		sub, err := ag.SubscribeFunc(func(_ context.Context, ev event.Event) error {
			mu.Lock()
			defer mu.Unlock()
			if ev.Type == event.TypeRegistryChanged || ev.Type == event.TypeSessionStarted {
				seen[ev.SessionID] = append(seen[ev.SessionID], string(ev.Type))
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		second, _ := ag.NewSession(t.Context())
		if reg.Scope() != kernel.ScopeAgent || reg.Session() != nil {
			t.Fatal("an agent-scope registrar has no session")
		}
		if _, err := reg.Tool(&echoTool{name: "shared"}); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		mu.Lock()
		got1, got2 := seen[first.Info().SessionID], seen[second.Info().SessionID]
		mu.Unlock()
		if !slices.Equal(got1, []string{"registry.changed"}) || !slices.Equal(got2, []string{"session.started", "registry.changed"}) {
			t.Fatalf("first %v, second %v", got1, got2)
		}
		sub.Unsubscribe()
		<-sub.Done()
		if _, err := kernel.New(kernel.WithModel(provider.ModelRef{Provider: sp, Model: "m"}), kernel.Use(kernel.ExtensionFunc("app", func(kernel.Registrar) error { return nil }))); !errors.Is(err, kernel.ErrInvalidName) {
			t.Fatalf("reserved extension name: %v", err)
		}
		if err := ag.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		if _, err := ag.NewSession(t.Context()); !errors.Is(err, kernel.ErrAgentClosed) {
			t.Fatalf("new session after close: %v", err)
		}
	})
}

func TestSessionScopeRegistration(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := providertest.NewScriptProvider(provider.APIAnthropicMessages, textReply("a"))
		ag, err := kernel.New(kernel.WithModel(provider.ModelRef{Provider: sp, Model: "m"}), kernel.WithTool(&echoTool{name: "echo"}),
			kernel.WithCommand(command("plan", kernel.WaitIdle, "agent plan")))
		if err != nil {
			t.Fatal(err)
		}
		local := kernel.ExtensionFunc("local", func(r kernel.Registrar) error {
			if r.Scope() != kernel.ScopeSession || r.Session() == nil || r.Agent() != ag {
				t.Error("session registrar")
			}
			if _, err := r.Tool(&echoTool{name: "echo"}); err != nil { // shadows the agent's tool of the same name
				return err
			}
			if _, err := r.Tool(&echoTool{name: "echo"}); !errors.Is(err, kernel.ErrDuplicate) {
				t.Errorf("same name twice in one scope: %v", err)
			}
			for _, bad := range []kernel.Command{{Name: "Bad", Run: command("x", "", "").Run}, {Name: "ok", Aliases: []string{"no space"}, Run: command("x", "", "").Run}, {Name: "norun"}, {Name: "busy", Busy: "sometimes", Run: command("x", "", "").Run}} {
				if _, err := r.Command(bad); err == nil {
					t.Errorf("command %+v registered", bad)
				}
			}
			_, err := r.Command(kernel.Command{Name: "plan", Aliases: []string{"p"}, Run: command("plan", "", "session plan").Run})
			return err
		})
		s, err := ag.NewSession(t.Context(), kernel.SessionUse(local))
		if err != nil {
			t.Fatal(err)
		}
		c, _ := s.Send(kernel.Text("/p now"))
		if res, _ := c.Wait(t.Context()); res.Command == nil || res.Command.Text != "session plan" {
			t.Fatalf("alias of the session command: %+v", res)
		}
		rc, _ := s.Send(kernel.Text("hi"))
		rc.Wait(t.Context())
		if tools := sp.Requests()[0].Tools; len(tools) != 1 || tools[0].Name != "echo" {
			t.Fatalf("tools %+v", tools)
		}
		if _, err := ag.NewSession(t.Context(), kernel.SessionUse(kernel.ExtensionFunc("broken", func(kernel.Registrar) error { return errors.New("nope") }))); err == nil {
			t.Fatal("a failing session extension must fail NewSession")
		}
		ag.Close(t.Context())
	})
}

func TestKeyState(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, nil)
		k := kernel.NewKey[[]string]("notes")
		other := kernel.NewKey[[]string]("notes") // same name, its own slot
		if _, ok := kernel.Get(h.sess, k); ok {
			t.Fatal("a fresh key holds a value")
		}
		kernel.Set(h.sess, k, []string{"a"})
		got := kernel.Update(h.sess, k, func(old []string, ok bool) []string { return append(old, "b") })
		if v, _ := kernel.Get(h.sess, k); len(v) != 2 || len(got) != 2 {
			t.Fatalf("update %v", v)
		}
		if _, ok := kernel.Get(h.sess, other); ok {
			t.Fatal("keys with one name share a slot")
		}
		if _, ok := kernel.Get(h.ag, k); ok {
			t.Fatal("agent and session share a slot")
		}
		h.close()
	})
}

func TestHide(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		noop := func(context.Context, *kernel.CommandContext) (kernel.CommandResult, error) {
			return kernel.CommandResult{}, nil
		}
		h := newHarness(t, [][]providertest.Step{textReply("a"), textReply("b")},
			kernel.WithTool(&echoTool{name: "echo"}), kernel.WithCommand(kernel.Command{Name: "ping", Run: noop}))
		r := h.sess.Registrar()
		hideTool, err := r.Hide(kernel.KindTool, "echo")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.Hide(kernel.KindTool, "echo"); !errors.Is(err, kernel.ErrDuplicate) {
			t.Fatalf("hide twice: %v", err)
		}
		hideCmd, _ := r.Hide(kernel.KindCommand, "ping")
		if _, err := h.sess.Command("ping", ""); !errors.Is(err, kernel.ErrUnknownCommand) {
			t.Fatalf("a hidden command runs: %v", err)
		}
		if len(h.sess.Commands()) != 0 {
			t.Fatal("a hidden command is listed")
		}
		rc, _ := h.sess.Send(kernel.Text("one"))
		_, _ = rc.Wait(t.Context())
		if tools := h.sp.Requests()[0].Tools; len(tools) != 0 {
			t.Fatalf("a hidden tool was sent: %v", tools)
		}
		hideTool.Remove()
		hideCmd.Remove()
		if _, err := h.sess.Command("ping", ""); err != nil {
			t.Fatalf("Remove did not restore the command: %v", err)
		}
		rc, _ = h.sess.Send(kernel.Text("two"))
		_, _ = rc.Wait(t.Context())
		if tools := h.sp.Requests()[1].Tools; len(tools) != 1 {
			t.Fatalf("Remove did not restore the tool: %v", tools)
		}
		if _, err := r.Hide("hook", "x"); err == nil {
			t.Fatal("hid an unknown kind")
		}
		for _, kind := range []kernel.Kind{kernel.KindApprovalRule, kernel.KindCompactor, kernel.KindTaskKind} {
			if _, err := r.Hide(kind, "x"); err == nil || !strings.Contains(err.Error(), "cannot hide") {
				t.Fatalf("Hide(%s): %v", kind, err)
			}
		}
		h.close()
	})
}

// TestHideRefusesApprovalRules: a session cannot hide an inherited rule, since hiding the agent's deny would let the session escape it.
func TestHideRefusesApprovalRules(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, [][]providertest.Step{
			toolReply(call{"toolu_1", "echo", `{"x":1}`}), textReply("done"),
		}, kernel.WithTool(&echoTool{name: "echo"}),
			kernel.WithApprovalRules(kernel.ApprovalRule{ID: "no-echo", Tool: "echo", Action: kernel.RuleDeny, Reason: "no echo"}))
		if _, err := h.sess.Registrar().Hide(kernel.KindApprovalRule, "no-echo"); err == nil {
			t.Fatal("a session hid an agent rule")
		}
		ask(t, h.sess, "go")
		if r := result(h.lines(), "toolu_1"); r.Outcome != message.OutcomeDenied {
			t.Fatalf("the agent rule no longer applied: %+v", r)
		}
		h.close()
	})
}

func TestProviderRegistration(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, [][]providertest.Step{textReply("a"), textReply("b")})
		p := kernel.ContextProvider{Name: "clock", When: kernel.AtEveryStep, Provide: func(context.Context, kernel.ProvideContext) (string, error) { return "tick", nil }}
		reg, err := h.sess.Registrar().ContextProvider(p)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := h.sess.Registrar().ContextProvider(p); !errors.Is(err, kernel.ErrDuplicate) {
			t.Fatalf("duplicate: %v", err)
		}
		for _, bad := range []kernel.ContextProvider{{Name: "", When: kernel.AtTurnStart, Provide: p.Provide}, {Name: "x", When: kernel.AtTurnStart}} {
			if _, err := h.sess.Registrar().ContextProvider(bad); err == nil {
				t.Fatalf("accepted %+v", bad)
			}
		}
		rc, _ := h.sess.Send(kernel.Text("one"))
		_, _ = rc.Wait(t.Context())
		reg.Remove()
		rc, _ = h.sess.Send(kernel.Text("two"))
		_, _ = rc.Wait(t.Context())
		lines := h.lines()
		if _, ok := lines[1].Content[0].(*message.ReminderBlock); !ok {
			t.Fatal("the provider did not run")
		}
		if len(lines[3].Content) != 1 {
			t.Fatalf("a removed provider still ran: %+v", lines[3].Content)
		}
		h.close()
	})
}

func TestHostedToolRegistration(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		search := message.HostedTool{Name: "web_search", WebSearch: &message.WebSearch{}}
		h := newHarness(t, [][]providertest.Step{textReply("a"), textReply("b"), textReply("c")}, kernel.WithHostedTool(search), kernel.WithTool(&echoTool{name: "echo"}))
		reg := h.sess.Registrar()
		for _, bad := range []message.HostedTool{
			{Name: "bad name", Raw: map[string]json.RawMessage{"anthropic.messages": json.RawMessage(`{}`)}},
			{Name: "both", WebSearch: &message.WebSearch{}, Raw: map[string]json.RawMessage{"anthropic.messages": json.RawMessage(`{}`)}},
			{Name: "neither"},
			{Name: "search", WebSearch: &message.WebSearch{}},
			{Name: "web_search", WebSearch: &message.WebSearch{AllowedDomains: []string{"a.test"}, BlockedDomains: []string{"b.test"}}},
			{Name: "web_search", WebSearch: &message.WebSearch{ContextSize: "huge"}},
			{Name: "fetch", Raw: map[string]json.RawMessage{"anthropic.messages": json.RawMessage(`[1]`)}},
		} {
			if _, err := reg.HostedTool(bad); err == nil {
				t.Errorf("%+v: registered", bad)
			}
		}
		if _, err := reg.HostedTool(message.HostedTool{Name: "echo", Raw: map[string]json.RawMessage{"openai.responses": json.RawMessage(`{"type":"x"}`)}}); err != nil {
			t.Fatalf("a session hosted tool may take the agent tool's name, which the freeze then refuses: %v", err)
		}
		if _, err := reg.Tool(&echoTool{name: "fetch"}); err != nil {
			t.Fatal(err)
		}
		if _, err := reg.HostedTool(message.HostedTool{Name: "fetch", Raw: map[string]json.RawMessage{"openai.responses": json.RawMessage(`{"type":"x"}`)}}); !errors.Is(err, kernel.ErrDuplicate) {
			t.Fatalf("same scope, tool's name: %v", err)
		}
		ask(t, h.sess, "one")
		hide, err := reg.Hide(kernel.KindHostedTool, "web_search")
		if err != nil {
			t.Fatal(err)
		}
		ask(t, h.sess, "two")
		hide.Remove()
		ask(t, h.sess, "three")
		h.close()
		var got []string
		for _, r := range h.sp.Requests() {
			var names []string
			for _, ht := range r.HostedTools {
				names = append(names, ht.Name)
			}
			got = append(got, strings.Join(names, ","))
		}
		if !slices.Equal(got, []string{"web_search", "", "web_search"}) {
			t.Fatalf("hosted tools per request %q", got)
		}
		if sys := h.lines()[0]; len(sys.HostedTools) != 1 || sys.HostedTools[0].Name != "web_search" {
			t.Fatalf("system line %+v", sys.HostedTools)
		}
	})
}

func TestDerivedAgentFiltersHostedTools(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := providertest.NewScriptProvider(provider.APIAnthropicMessages, textReply("a"))
		parent, err := kernel.New(kernel.WithModel(provider.ModelRef{Provider: sp, Model: "m"}),
			kernel.WithHostedTool(message.HostedTool{Name: "web_search", WebSearch: &message.WebSearch{}}),
			kernel.WithHostedTool(message.HostedTool{Name: "web_fetch", Raw: map[string]json.RawMessage{"anthropic.messages": json.RawMessage(`{"type":"web_fetch_20250910","name":"web_fetch"}`)}}))
		if err != nil {
			t.Fatal(err)
		}
		child, err := parent.Derive(kernel.DeriveOptions{ToolFilter: func(ts kernel.ToolSpec) bool { return ts.Name != "web_fetch" }})
		if err != nil {
			t.Fatal(err)
		}
		s, err := child.NewSession(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		ask(t, s, "hi")
		_ = parent.Close(t.Context())
		_ = child.Close(t.Context())
		hosted := sp.Requests()[0].HostedTools
		if len(hosted) != 1 || hosted[0].Name != "web_search" {
			t.Fatalf("child hosted tools %+v", hosted)
		}
	})
}

// deferredTool is an echo tool that loads only on demand.
type deferredTool struct{ echoTool }

func (d *deferredTool) Spec() kernel.ToolSpec {
	spec := d.echoTool.Spec()
	spec.Deferred = true
	return spec
}

// TestToolSearchAndDeferredTools: the neutral tool search registers under its own name only, deferred definitions freeze as such, and the estimate leaves them out while a tool search goes with them.
func TestToolSearchAndDeferredTools(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		search := message.HostedTool{Name: "tool_search", ToolSearch: &message.ToolSearch{}}
		h := newHarness(t, [][]providertest.Step{textReply("a"), textReply("b")}, kernel.WithHostedTool(search),
			kernel.WithTool(&deferredTool{echoTool{name: "mcp__gh__create_issue"}}), kernel.WithTool(&echoTool{name: "echo"}))
		reg := h.sess.Registrar()
		for _, bad := range []message.HostedTool{
			{Name: "search", ToolSearch: &message.ToolSearch{}},
			{Name: "tool_search", ToolSearch: &message.ToolSearch{}, WebSearch: &message.WebSearch{}},
			{Name: "tool_search", ToolSearch: &message.ToolSearch{}, Raw: map[string]json.RawMessage{"anthropic.messages": json.RawMessage(`{}`)}},
		} {
			if _, err := reg.HostedTool(bad); err == nil {
				t.Errorf("%+v: registered", bad)
			}
		}
		parts := func() map[string]bool {
			out := map[string]bool{}
			for _, p := range h.sess.Context().Parts {
				if p.Kind == "tool" {
					out[p.Name] = true
				}
			}
			return out
		}
		if got := parts(); got["mcp__gh__create_issue"] || !got["echo"] {
			t.Fatalf("with a tool search the deferred definition is not counted: %v", got)
		}
		ask(t, h.sess, "one")
		hide, err := reg.Hide(kernel.KindHostedTool, "tool_search")
		if err != nil {
			t.Fatal(err)
		}
		if got := parts(); !got["mcp__gh__create_issue"] {
			t.Fatalf("without a tool search the deferred definition is sent and counted: %v", got)
		}
		ask(t, h.sess, "two")
		hide.Remove()
		h.close()
		reqs := h.sp.Requests()
		deferred := func(r *provider.Request) []string {
			var out []string
			for _, d := range r.Tools {
				if d.Deferred {
					out = append(out, d.Name)
				}
			}
			return out
		}
		if got := deferred(reqs[0]); !slices.Equal(got, []string{"mcp__gh__create_issue"}) || len(reqs[0].HostedTools) != 1 || reqs[0].HostedTools[0].ToolSearch == nil {
			t.Fatalf("first request: deferred %v, hosted %+v", got, reqs[0].HostedTools)
		}
		if len(reqs[1].HostedTools) != 0 {
			t.Fatalf("hidden tool search still sent: %+v", reqs[1].HostedTools)
		}
		if sys := h.lines()[0]; !slices.ContainsFunc(sys.Tools, func(d message.ToolDef) bool { return d.Name == "mcp__gh__create_issue" && d.Deferred }) {
			t.Fatalf("system line tools %+v", sys.Tools)
		}
	})
}

// TestAgentOnClose: close callbacks run after every session closed, the last registered first, and their errors join Close's.
func TestAgentOnClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, nil)
		var order []string
		h.ag.OnClose(func(context.Context) error {
			order = append(order, "first:"+h.sess.Snapshot().Phase)
			return nil
		})
		boom := errors.New("boom")
		h.ag.OnClose(func(context.Context) error {
			order = append(order, "second")
			return boom
		})
		if err := h.ag.Close(t.Context()); !errors.Is(err, boom) {
			t.Fatalf("close: %v", err)
		}
		if len(order) != 2 || order[0] != "second" || order[1] != "first:closed" {
			t.Fatalf("order %v", order)
		}
	})
}

// TestCommandNameAllowsColon: a command and its alias may carry ":", which runtime registration and /name parsing both accept.
func TestCommandNameAllowsColon(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, nil)
		run := func(context.Context, *kernel.CommandContext) (kernel.CommandResult, error) {
			return kernel.CommandResult{Text: "deployed"}, nil
		}
		if _, err := h.sess.Registrar().Command(kernel.Command{Name: "plugin:deploy", Aliases: []string{"plugin:d"}, Run: run}); err != nil {
			t.Fatal(err)
		}
		for _, text := range []string{"/plugin:deploy prod", "/plugin:d prod"} {
			rc, err := h.sess.Send(kernel.Text(text))
			if err != nil {
				t.Fatal(err)
			}
			res, err := rc.Wait(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if res.Command == nil || res.Command.Text != "deployed" {
				t.Fatalf("%s: %+v", text, res)
			}
		}
		synctest.Wait()
		var names []string
		for _, ev := range h.rec.all() {
			if ev.Type == event.TypeCommandStarted {
				names = append(names, ev.Data.(*event.CommandStarted).Name)
			}
		}
		if !slices.Equal(names, []string{"plugin:deploy", "plugin:deploy"}) {
			t.Fatalf("command.started names %v", names)
		}
		for _, bad := range []kernel.Command{
			{Name: ":deploy", Run: run},
			{Name: "Plugin:deploy", Run: run},
			{Name: "plugin/deploy", Run: run},
			{Name: "plugin:deploy", Aliases: []string{":bad"}, Run: run},
		} {
			if _, err := h.sess.Registrar().Command(bad); !errors.Is(err, kernel.ErrInvalidName) {
				t.Fatalf("%q: %v", bad.Name, err)
			}
		}
		h.close()
	})
}
