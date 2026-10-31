package subagent_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/ext/subagent"
	"github.com/estonshi/aperture-potatos/internal/determ"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/providertest"
	"github.com/estonshi/aperture-potatos/store"
)

func reply(text string) []providertest.Step {
	u := message.Usage{InputTokens: 10, OutputTokens: 2}
	m := &message.Message{Role: message.RoleAssistant, Origin: message.Origin{Kind: message.OriginModel}, Content: message.Content{&message.TextBlock{Text: text}},
		Producer: &message.Producer{API: string(provider.APIAnthropicMessages), Endpoint: "script.invalid", Model: "m"}, Usage: &u, StopReason: string(provider.StopEndTurn)}
	return []providertest.Step{
		{Event: provider.StreamEvent{Kind: provider.SEStart, ResponseID: "resp"}},
		{Event: provider.StreamEvent{Kind: provider.SEBlockStart, Index: 0, BlockType: message.TypeText}},
		{Event: provider.StreamEvent{Kind: provider.SETextDelta, Index: 0, Channel: "text", Text: text}},
		{Event: provider.StreamEvent{Kind: provider.SEBlockStop, Index: 0, Block: &message.TextBlock{Text: text}}},
		{Event: provider.StreamEvent{Kind: provider.SEEnd, Message: m, Stop: &provider.Stop{Reason: provider.StopEndTurn, Raw: "end_turn"}, Usage: &u}},
	}
}

// children records every session the subagent's inherited hooks fire in, which is how a test reaches a child session.
type children struct {
	mu   sync.Mutex
	seen []*kernel.Session
}

func (c *children) hook() kernel.Option {
	return kernel.WithHook(kernel.UserPromptSubmit, "children", func(_ context.Context, in kernel.UserPromptSubmitInput) (kernel.UserPromptSubmitOutput, error) {
		if len(in.Path) > 0 {
			c.mu.Lock()
			if !slices.Contains(c.seen, in.Session) {
				c.seen = append(c.seen, in.Session)
			}
			c.mu.Unlock()
		}
		return kernel.UserPromptSubmitOutput{}, nil
	})
}

func (c *children) first() *kernel.Session {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.seen) == 0 {
		return nil
	}
	return c.seen[0]
}

var noWake = kernel.WithHook(kernel.TaskNotify, "no_wake", func(context.Context, kernel.TaskNotifyInput) (kernel.TaskNotifyOutput, error) {
	return kernel.TaskNotifyOutput{NoWake: true}, nil
})

func newAgent(t *testing.T, st store.SessionStore, sp *providertest.ScriptProvider, opts ...kernel.Option) *kernel.Agent {
	t.Helper()
	base := []kernel.Option{
		kernel.WithModel(provider.ModelRef{Provider: sp, Model: "m"}),
		kernel.WithStore(st),
		kernel.WithClock(determ.NewStepClock(time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC), time.Millisecond)),
		kernel.WithIDGenerator(determ.NewSequentialIDs()),
		kernel.WithPanicOnInternalError(true),
		noWake,
	}
	ag, err := kernel.New(append(base, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	return ag
}

func start(t *testing.T, s *kernel.Session, prompt string) kernel.TaskInfo {
	t.Helper()
	in, _ := json.Marshal(map[string]string{"agent_type": "general", "description": "job", "prompt": prompt})
	info, err := s.Tasks().Start(t.Context(), kernel.TaskSpec{Kind: subagent.Name, Title: "job", Input: in})
	if err != nil {
		t.Fatal(err)
	}
	return info
}

// TestHostStartInChildFails: a subagent's child session cannot start a subagent even by the host, whose attempt fails without a child.
func TestHostStartInChildFails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var kids children
		sp := providertest.NewScriptProvider(provider.APIAnthropicMessages, reply("done"))
		ag := newAgent(t, store.NewMemory(), sp, kernel.Use(subagent.New(subagent.Options{})), kids.hook())
		sess, err := ag.NewSession(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		first := start(t, sess, "work")
		if got, _ := sess.Tasks().Wait(t.Context(), first.ID); got.Status != kernel.TaskSucceeded {
			t.Fatalf("first: %s %s", got.Status, got.Error)
		}
		child := kids.first()
		if child == nil {
			t.Fatal("no child session")
		}
		nested := start(t, child, "again")
		got, _ := child.Tasks().Wait(t.Context(), nested.ID)
		if got.Status != kernel.TaskFailed || !strings.Contains(got.Error, "cannot start subagents") || len(sp.Requests()) != 1 {
			t.Fatalf("nested: %s %q after %d requests", got.Status, got.Error, len(sp.Requests()))
		}
		if err := ag.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}

// TestContinueResumesClosedChild: a continuation whose child was closed resumes it from the store, so the child keeps its history.
func TestContinueResumesClosedChild(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var kids children
		sp := providertest.NewScriptProvider(provider.APIAnthropicMessages, reply("Paris."), reply("Berlin."))
		ag := newAgent(t, store.NewMemory(), sp, kernel.Use(subagent.New(subagent.Options{})), kids.hook())
		sess, err := ag.NewSession(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		info := start(t, sess, "Capital of France?")
		if _, err := sess.Tasks().Wait(t.Context(), info.ID); err != nil {
			t.Fatal(err)
		}
		child := kids.first()
		if err := child.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		if err := sess.Tasks().Send(t.Context(), info.ID, kernel.TaskInstruction{Text: "And Germany?"}); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		got, _ := sess.Tasks().Wait(t.Context(), info.ID)
		if got.Status != kernel.TaskSucceeded || got.Attempt != 2 || got.Result.Summary != "Berlin." {
			t.Fatalf("continuation: %+v", got)
		}
		reqs := sp.Requests()
		if n := len(reqs[1].Messages); n != 3 {
			t.Fatalf("the resumed child sent %d messages; it keeps the first exchange", n)
		}
		if err := ag.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}

// TestDefineAtSessionScope: a type defined on one session joins the agent's types in that session's start tool, and leaves it when removed.
func TestDefineAtSessionScope(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := providertest.NewScriptProvider(provider.APIAnthropicMessages, reply("a"), reply("b"))
		ag := newAgent(t, store.NewMemory(), sp, kernel.Use(subagent.New(subagent.Options{})))
		sess, err := ag.NewSession(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		reg, err := subagent.Define(sess.Registrar(), subagent.Def{Name: "coder", Description: "Writes code."})
		if err != nil {
			t.Fatal(err)
		}
		enum := func() string {
			reqs := sp.Requests()
			for _, d := range reqs[len(reqs)-1].Tools {
				if d.Name == subagent.Name {
					var s struct {
						Properties struct {
							AgentType struct {
								Enum []string `json:"enum"`
							} `json:"agent_type"`
						} `json:"properties"`
					}
					_ = json.Unmarshal(d.InputSchema, &s)
					return strings.Join(s.Properties.AgentType.Enum, ",")
				}
			}
			return ""
		}
		send := func() {
			r, err := sess.Send(kernel.Text("hi"))
			if err != nil {
				t.Fatal(err)
			}
			if _, err := r.Wait(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		send()
		if got := enum(); got != "coder,general" {
			t.Fatalf("enum %q", got)
		}
		reg.Remove()
		send()
		if got := enum(); got != "general" {
			t.Fatalf("enum %q after Remove", got)
		}
		if err := ag.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}

// TestDefineAnnouncesToolChange: the first session type registers the session's start tool, and each later type defined or removed changes its enum, which registry.changed{op:update} reports.
func TestDefineAnnouncesToolChange(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := providertest.NewScriptProvider(provider.APIAnthropicMessages)
		ag := newAgent(t, store.NewMemory(), sp, kernel.Use(subagent.New(subagent.Options{})))
		sess, err := ag.NewSession(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		var mu sync.Mutex
		var got []string
		if _, err := sess.SubscribeFunc(func(_ context.Context, ev event.Event) error {
			if c, ok := ev.Data.(*event.RegistryChanged); ok {
				mu.Lock()
				got = append(got, c.Scope+" "+c.Kind+" "+c.Name+" "+c.Op+" "+c.Effective)
				mu.Unlock()
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := subagent.Define(sess.Registrar(), subagent.Def{Name: "coder", Description: "Writes code."}); err != nil {
			t.Fatal(err)
		}
		reg, err := subagent.Define(sess.Registrar(), subagent.Def{Name: "tester", Description: "Runs tests."})
		if err != nil {
			t.Fatal(err)
		}
		reg.Remove()
		synctest.Wait()
		mu.Lock()
		defer mu.Unlock()
		want := []string{"session tool subagent add turn", "session tool subagent update turn", "session tool subagent update turn"}
		if !slices.Equal(got, want) {
			t.Fatalf("got %q", got)
		}
		if err := ag.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}

// TestHookFailures: a failing subagent_start hook (closed) denies the start with the kind of failure only, never its error text; a failing subagent_stop hook (open) lets the child finish.
func TestHookFailures(t *testing.T) {
	broken := func(kind string) func(ctx context.Context) error {
		return func(ctx context.Context) error {
			switch kind {
			case "panic":
				panic("db password is hunter2")
			case "timeout":
				<-ctx.Done()
				return ctx.Err()
			}
			return errors.New("db password is hunter2")
		}
	}
	for _, kind := range []string{"error", "panic", "timeout"} {
		t.Run("start/"+kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fail := broken(kind)
				hook := kernel.WithHook(subagent.Start, "broken", func(ctx context.Context, _ subagent.StartInput) (subagent.StartOutput, error) {
					return subagent.StartOutput{}, fail(ctx)
				}, kernel.Timeout(10*time.Millisecond))
				ag := newAgent(t, store.NewMemory(), providertest.NewScriptProvider(provider.APIAnthropicMessages), kernel.Use(subagent.New(subagent.Options{})), hook)
				sess, err := ag.NewSession(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				got, _ := sess.Tasks().Wait(t.Context(), start(t, sess, "work").ID)
				if got.Status != kernel.TaskFailed || !strings.Contains(got.Error, "subagent_start hook failed ("+kind+")") || strings.Contains(got.Error, "hunter2") {
					t.Fatalf("%s %q", got.Status, got.Error)
				}
				_ = ag.Close(t.Context())
			})
		})
		t.Run("stop/"+kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				fail := broken(kind)
				hook := kernel.WithHook(subagent.Stop, "broken", func(ctx context.Context, _ subagent.StopInput) (subagent.StopOutput, error) {
					return subagent.StopOutput{}, fail(ctx)
				}, kernel.Timeout(10*time.Millisecond))
				sp := providertest.NewScriptProvider(provider.APIAnthropicMessages, reply("done"))
				ag := newAgent(t, store.NewMemory(), sp, kernel.Use(subagent.New(subagent.Options{})), hook)
				sess, err := ag.NewSession(t.Context())
				if err != nil {
					t.Fatal(err)
				}
				got, _ := sess.Tasks().Wait(t.Context(), start(t, sess, "work").ID)
				if got.Status != kernel.TaskSucceeded || len(sp.Requests()) != 1 {
					t.Fatalf("%s %q after %d requests", got.Status, got.Error, len(sp.Requests()))
				}
				_ = ag.Close(t.Context())
			})
		})
	}
}

// TestDefineAllowsColonName: a subagent type may carry ":", and the start tool's enum lists it.
func TestDefineAllowsColonName(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := providertest.NewScriptProvider(provider.APIAnthropicMessages)
		ag := newAgent(t, store.NewMemory(), sp, kernel.Use(subagent.New(subagent.Options{})))
		sess, err := ag.NewSession(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := subagent.Define(sess.Registrar(), subagent.Def{Name: "plugin:db-agent", Description: "Database work."}); err != nil {
			t.Fatal(err)
		}
		var enum []string
		for _, d := range sess.Tools() {
			if d.Name == subagent.Name {
				var s struct {
					Properties struct {
						AgentType struct {
							Enum []string `json:"enum"`
						} `json:"agent_type"`
					} `json:"properties"`
				}
				_ = json.Unmarshal(d.InputSchema, &s)
				enum = s.Properties.AgentType.Enum
			}
		}
		if !slices.Equal(enum, []string{"general", "plugin:db-agent"}) {
			t.Fatalf("enum %v", enum)
		}
		if _, err := subagent.Define(sess.Registrar(), subagent.Def{Name: ":bad name", Description: "Bad."}); !errors.Is(err, kernel.ErrInvalidName) {
			t.Fatalf("invalid name: %v", err)
		}
		if err := ag.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}
