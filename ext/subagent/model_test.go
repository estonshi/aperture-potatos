package subagent_test

import (
	"context"
	"encoding/json"
	"maps"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/estonshi/aperture-potatos/ext/subagent"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/providertest"
	"github.com/estonshi/aperture-potatos/store"
)

// choices is Options with a resolver that turns each alias into a model on the script provider.
func choices(sp *providertest.ScriptProvider, defs ...subagent.Def) subagent.Options {
	return subagent.Options{
		Defs:    defs,
		Models:  []string{"small", "large", "inherit"},
		Efforts: []string{"low", "high"},
		ResolveModel: func(name string) (*provider.ModelRef, error) {
			return &provider.ModelRef{Provider: sp, Model: name}, nil
		},
	}
}

// startWith starts the general type as the host, with fields the model would have sent.
func startWith(t *testing.T, s *kernel.Session, fields map[string]string) kernel.TaskInfo {
	t.Helper()
	in := map[string]string{"agent_type": "general", "description": "job", "prompt": "work"}
	maps.Copy(in, fields)
	raw, _ := json.Marshal(in)
	info, err := s.Tasks().Start(t.Context(), kernel.TaskSpec{Kind: subagent.Name, Title: "job", Input: raw})
	if err != nil {
		t.Fatal(err)
	}
	return info
}

type toolProp struct {
	Enum []string `json:"enum"`
}

// toolProps is the start tool's input schema as the model sees it.
func toolProps(t *testing.T, s *kernel.Session) map[string]toolProp {
	t.Helper()
	for _, tool := range s.Tools() {
		if tool.Name != subagent.Name {
			continue
		}
		var v struct {
			Properties map[string]toolProp `json:"properties"`
		}
		if err := json.Unmarshal(tool.InputSchema, &v); err != nil {
			t.Fatal(err)
		}
		return v.Properties
	}
	t.Fatal("no subagent start tool")
	return nil
}

// TestCallTimeModelAndEffort: a call picks a model and an effort from the host's enums, and the child runs on them.
func TestCallTimeModelAndEffort(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var kids children
		sp := providertest.NewScriptProvider(provider.APIAnthropicMessages, reply("done"))
		ag := newAgent(t, store.NewMemory(), sp, kernel.Use(subagent.New(choices(sp))), kids.hook())
		sess, err := ag.NewSession(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		info := startWith(t, sess, map[string]string{"model": "small", "effort": "high"})
		if got, _ := sess.Tasks().Wait(t.Context(), info.ID); got.Status != kernel.TaskSucceeded {
			t.Fatalf("task: %+v", got)
		}
		if req := sp.Requests()[0]; req.Model != "small" || req.Params.Reasoning.Effort != "high" {
			t.Fatalf("child ran on %q effort %q", req.Model, req.Params.Reasoning.Effort)
		}
		props := toolProps(t, sess)
		if !slices.Equal(props["model"].Enum, []string{"small", "large", "inherit"}) || !slices.Equal(props["effort"].Enum, []string{"low", "high"}) {
			t.Fatalf("schema enums: %+v", props)
		}
		if err := ag.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}

// TestStartSchemaHidesChoices: a host that offers no choices leaves model and effort out of the schema.
func TestStartSchemaHidesChoices(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := providertest.NewScriptProvider(provider.APIAnthropicMessages)
		ag := newAgent(t, store.NewMemory(), sp, kernel.Use(subagent.New(subagent.Options{})))
		sess, err := ag.NewSession(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		props := toolProps(t, sess)
		for _, name := range []string{"model", "effort"} {
			if _, ok := props[name]; ok {
				t.Fatalf("%s is in the schema without options", name)
			}
		}
		if _, ok := props["prompt"]; !ok {
			t.Fatalf("prompt left out: %+v", props)
		}
		if err := ag.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}

// TestFixedModelIgnoresCallChoice: a type that names its model, or fixes it, runs on that model whatever the call asks.
func TestFixedModelIgnoresCallChoice(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var kids children
		sp := providertest.NewScriptProvider(provider.APIAnthropicMessages, reply("a"), reply("b"))
		defs := []subagent.Def{
			{Name: "pinned", Description: "A fixed model.", Model: &provider.ModelRef{Provider: sp, Model: "pinned"}},
			{Name: "inherited", Description: "The parent's model.", FixedModel: true},
		}
		ag := newAgent(t, store.NewMemory(), sp, kernel.Use(subagent.New(choices(sp, defs...))), kids.hook())
		sess, err := ag.NewSession(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		for _, name := range []string{"pinned", "inherited"} {
			raw, _ := json.Marshal(map[string]string{"agent_type": name, "description": "job", "prompt": "work", "model": "small", "effort": "high"})
			info, err := sess.Tasks().Start(t.Context(), kernel.TaskSpec{Kind: subagent.Name, Title: "job", Input: raw})
			if err != nil {
				t.Fatal(err)
			}
			if got, _ := sess.Tasks().Wait(t.Context(), info.ID); got.Status != kernel.TaskSucceeded {
				t.Fatalf("%s: %+v", name, got)
			}
		}
		reqs := sp.Requests()
		if reqs[0].Model != "pinned" || reqs[0].Params.Reasoning.Effort != "" {
			t.Fatalf("a fixed model took the call's choice: %q %q", reqs[0].Model, reqs[0].Params.Reasoning.Effort)
		}
		if reqs[1].Model != "m" || reqs[1].Params.Reasoning.Effort != "" { // the agent's own model, from newAgent
			t.Fatalf("FixedModel took the call's choice: %q %q", reqs[1].Model, reqs[1].Params.Reasoning.Effort)
		}
		if err := ag.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}

// TestStartHookChoosesModel: the hook sees what the call asked for and its model and effort win.
func TestStartHookChoosesModel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var kids children
		var mu sync.Mutex
		var seen subagent.StartInput
		hook := kernel.WithHook(subagent.Start, "pick", func(_ context.Context, in subagent.StartInput) (subagent.StartOutput, error) {
			mu.Lock()
			seen = in
			mu.Unlock()
			return subagent.StartOutput{Model: "large", Effort: "low"}, nil
		})
		sp := providertest.NewScriptProvider(provider.APIAnthropicMessages, reply("done"))
		ag := newAgent(t, store.NewMemory(), sp, kernel.Use(subagent.New(choices(sp))), hook, kids.hook())
		sess, err := ag.NewSession(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		info := startWith(t, sess, map[string]string{"model": "small", "effort": "high"})
		if got, _ := sess.Tasks().Wait(t.Context(), info.ID); got.Status != kernel.TaskSucceeded {
			t.Fatalf("task: %+v", got)
		}
		mu.Lock()
		got := seen
		mu.Unlock()
		if got.Model != "small" || got.Effort != "high" {
			t.Fatalf("the hook saw %q %q", got.Model, got.Effort)
		}
		if req := sp.Requests()[0]; req.Model != "large" || req.Params.Reasoning.Effort != "low" {
			t.Fatalf("the hook's choice lost: %q %q", req.Model, req.Params.Reasoning.Effort)
		}
		if err := ag.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}

// TestUnknownModelChoiceFails: a name outside the enum fails the attempt with the enum, instead of running the default.
func TestUnknownModelChoiceFails(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := providertest.NewScriptProvider(provider.APIAnthropicMessages)
		ag := newAgent(t, store.NewMemory(), sp, kernel.Use(subagent.New(choices(sp))))
		sess, err := ag.NewSession(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		info := startWith(t, sess, map[string]string{"model": "huge"})
		got, _ := sess.Tasks().Wait(t.Context(), info.ID)
		if got.Status != kernel.TaskFailed || !strings.Contains(got.Error, `"huge"`) || !strings.Contains(got.Error, "small, large, inherit") {
			t.Fatalf("unknown model: %+v", got)
		}
		if n := len(sp.Requests()); n != 0 {
			t.Fatalf("%d requests before the choice was checked", n)
		}
		if err := ag.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}

// TestContinueKeepsChosenModel: a continuation that resumes the child from the store runs on the model the attempt chose.
func TestContinueKeepsChosenModel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var kids children
		sp := providertest.NewScriptProvider(provider.APIAnthropicMessages, reply("first"), reply("second"))
		ag := newAgent(t, store.NewMemory(), sp, kernel.Use(subagent.New(choices(sp))), kids.hook())
		sess, err := ag.NewSession(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		info := startWith(t, sess, map[string]string{"model": "small"})
		if _, err := sess.Tasks().Wait(t.Context(), info.ID); err != nil {
			t.Fatal(err)
		}
		child := kids.first()
		if child == nil {
			t.Fatal("no child session")
		}
		if err := child.Close(t.Context()); err != nil { // the continuation resumes it from the store
			t.Fatal(err)
		}
		if err := sess.Tasks().Send(t.Context(), info.ID, kernel.TaskInstruction{Text: "again"}); err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		got, _ := sess.Tasks().Wait(t.Context(), info.ID)
		if got.Status != kernel.TaskSucceeded || got.Attempt != 2 {
			t.Fatalf("continuation: %+v", got)
		}
		reqs := sp.Requests()
		if len(reqs) != 2 || reqs[1].Model != "small" {
			t.Fatalf("the resumed child ran on %q", reqs[len(reqs)-1].Model)
		}
		if err := ag.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
	})
}
