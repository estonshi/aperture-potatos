package peer_test

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/estonshi/aperture-potatos/ext/peer"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/providertest"
	"github.com/estonshi/aperture-potatos/store"
)

// TestForegroundReply: the model's a2a_send waits for the reply, which comes back wrapped and cleaned; the catalog and the enum list the peer.
func TestForegroundReply(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFake("translator", completed("r1", "c1", "Bonjour ", "tout le monde."))
		sp := providertest.NewScriptProvider(provider.APIAnthropicMessages,
			calls(call{"t1", "a2a_send", `{"agent":"translator","message":"Translate: hello everyone"}`}), reply("Done."))
		ag := newAgent(t, "Tater", store.NewMemory(), sp, kernel.Use(peer.New(f)))
		s, err := ag.NewSession(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		ask(t, s, "Say hello in French.")
		res := toolResults(t, s)["t1"]
		want := "<agent_reply agent=\"translator\" task_id=\"r1\">\nBonjour tout le monde.\n</agent_reply>"
		if res.IsError || resultText(res) != want {
			t.Fatalf("result %v %q", res.IsError, resultText(res))
		}
		sent := f.messages()
		if len(sent) != 1 || msgText(sent[0]) != "Translate: hello everyone" || sent[0].Role != "user" || sent[0].ContextID != "" || string(sent[0].Metadata["agent_call_chain"]) != `["Tater"]` {
			t.Fatalf("sent %+v", sent)
		}
		req := sp.Requests()[0]
		sys := systemText(req)
		if !strings.Contains(sys, "<name>translator</name>") || !strings.Contains(sys, "<description>Translates text.</description>") {
			t.Fatalf("no catalog in the system: %s", sys)
		}
		var tool *message.ToolDef
		for i := range req.Tools {
			if req.Tools[i].Name == "a2a_send" {
				tool = &req.Tools[i]
			}
		}
		if tool == nil || !strings.Contains(string(tool.InputSchema), `"enum":["translator"]`) || !strings.Contains(string(tool.InputSchema), `"background"`) {
			t.Fatalf("a2a_send: %+v", tool)
		}
		info := s.Tasks().List()[0]
		var st map[string]string
		_ = json.Unmarshal(info.State, &st)
		if info.Kind != "a2a" || info.Title != "translator: Translate: hello everyone" || st["peer"] != "translator" || st["context_id"] != "c1" || st["task_id"] != "r1" {
			t.Fatalf("task %+v state %s", info, info.State)
		}
		_ = ag.Close(t.Context())
	})
}

// TestNoPeersNoSurface: without peers there is neither a catalog nor a2a_send; a peer registered later brings both and is announced.
func TestNoPeersNoSurface(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := providertest.NewScriptProvider(provider.APIAnthropicMessages, reply("Hi."), reply("Ok."))
		ag := newAgent(t, "Tater", store.NewMemory(), sp, kernel.Use(peer.New()))
		s, err := ag.NewSession(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		ask(t, s, "Hello.")
		req := sp.Requests()[0]
		if strings.Contains(systemText(req), "available_agents") {
			t.Fatal("a catalog without peers")
		}
		for _, d := range req.Tools {
			if d.Name == "a2a_send" {
				t.Fatal("a2a_send without peers")
			}
		}
		reg, err := peer.Register(s.Registrar(), newFake("translator"))
		if err != nil {
			t.Fatal(err)
		}
		ask(t, s, "Again.")
		last := sp.Requests()[1]
		found := false
		for _, d := range last.Tools {
			found = found || d.Name == "a2a_send"
		}
		wire, _ := json.Marshal(last.Messages)
		if !found || !strings.Contains(string(wire), "New agents are available through a2a_send:") {
			t.Fatalf("after Register: tool %v, announcement in %s", found, wire)
		}
		reg.Remove()
		other := newAgent(t, "Other", store.NewMemory(), providertest.NewScriptProvider(provider.APIAnthropicMessages))
		os, _ := other.NewSession(t.Context())
		if _, err := peer.Register(os.Registrar(), newFake("x")); !errors.Is(err, peer.ErrNotInstalled) {
			t.Fatalf("Register without the extension: %v", err)
		}
		_ = other.Close(t.Context())
		_ = ag.Close(t.Context())
	})
}

// TestNextMessageSameContext: the next message to the same agent continues its latest conversation; new_conversation starts another.
func TestNextMessageSameContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFake("translator", completed("r1", "c1", "A", "."), completed("r2", "c1", "B", "."), completed("r3", "c2", "C", "."))
		sp := providertest.NewScriptProvider(provider.APIAnthropicMessages,
			calls(call{"t1", "a2a_send", `{"agent":"translator","message":"one"}`}), reply("1"),
			calls(call{"t2", "a2a_send", `{"agent":"translator","message":"two"}`}), reply("2"),
			calls(call{"t3", "a2a_send", `{"agent":"translator","message":"three","new_conversation":true}`}), reply("3"))
		ag := newAgent(t, "Tater", store.NewMemory(), sp, kernel.Use(peer.New(f)))
		s, _ := ag.NewSession(t.Context())
		ask(t, s, "1")
		ask(t, s, "2")
		ask(t, s, "3")
		sent := f.messages()
		if len(sent) != 3 || sent[0].ContextID != "" || sent[1].ContextID != "c1" || sent[1].TaskID != "" || sent[2].ContextID != "" {
			t.Fatalf("contexts %q %q %q", sent[0].ContextID, sent[1].ContextID, sent[2].ContextID)
		}
		_ = ag.Close(t.Context())
	})
}

// TestInputRequiredRoundtrip: a question from the peer returns from the foreground call and leaves the task waiting; the answer goes to the same task.
func TestInputRequiredRoundtrip(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFake("translator",
			round{events: []peer.Event{
				{Kind: peer.EventTask, TaskID: "r1", ContextID: "c1", State: peer.StateSubmitted},
				{Kind: peer.EventStatus, TaskID: "r1", ContextID: "c1", State: peer.StateInputRequired, Message: agentMsg("Which language?")},
			}},
			completed("r1", "c1", "Hola", "."))
		sp := providertest.NewScriptProvider(provider.APIAnthropicMessages,
			calls(call{"t1", "a2a_send", `{"agent":"translator","message":"Translate hello"}`}), reply("It asks which language."))
		ag := newAgent(t, "Tater", store.NewMemory(), sp, kernel.Use(peer.New(f)), noWake)
		s, _ := ag.NewSession(t.Context())
		ask(t, s, "Translate hello.")
		res := toolResults(t, s)["t1"]
		if res.IsError || !strings.Contains(resultText(res), "Which language?") {
			t.Fatalf("foreground result %q", resultText(res))
		}
		info := s.Tasks().List()[0]
		if info.Status != kernel.TaskWaitingInput || info.Question != "Which language?" {
			t.Fatalf("task %s %q", info.Status, info.Question)
		}
		if err := s.Tasks().Send(t.Context(), info.ID, kernel.TaskInstruction{Text: "Spanish."}); err != nil {
			t.Fatal(err)
		}
		got, err := s.Tasks().Wait(t.Context(), info.ID)
		if err != nil || got.Status != kernel.TaskSucceeded || !strings.Contains(got.Result.Summary, "Hola.") {
			t.Fatalf("after the answer: %+v %v", got, err)
		}
		sent := f.messages()
		if len(sent) != 2 || msgText(sent[1]) != "Spanish." || sent[1].TaskID != "r1" || sent[1].ContextID != "c1" {
			t.Fatalf("answer sent as %+v", sent[1])
		}
		_ = ag.Close(t.Context())
	})
}

func hostSend(t *testing.T, s *kernel.Session, agent, msg string) kernel.TaskInfo {
	t.Helper()
	in, _ := json.Marshal(map[string]string{"agent": agent, "message": msg})
	info, err := s.Tasks().Start(t.Context(), kernel.TaskSpec{Kind: peer.Kind, Title: agent, Input: in})
	if err != nil {
		t.Fatal(err)
	}
	return info
}

// TestInstructionModes: while the peer works, next_loop instructions are merged into one message after the round, queue ones follow one by one, and an interrupt cancels the peer's task and goes out at once.
func TestInstructionModes(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		gate := make(chan struct{})
		first := completed("r1", "c1", "one", ".")
		first.gate = gate
		f := newFake("translator", first, completed("r2", "c1", "two", "."), completed("r3", "c1", "three", "."))
		ag := newAgent(t, "Tater", store.NewMemory(), providertest.NewScriptProvider(provider.APIAnthropicMessages), kernel.Use(peer.New(f)), noWake)
		s, _ := ag.NewSession(t.Context())
		info := hostSend(t, s, "translator", "start")
		synctest.Wait()
		for _, in := range []kernel.TaskInstruction{{Text: "a", Mode: kernel.ModeQueue}, {Text: "b"}, {Text: "c"}} {
			if err := s.Tasks().Send(t.Context(), info.ID, in); err != nil {
				t.Fatal(err)
			}
		}
		synctest.Wait()
		close(gate)
		got, _ := s.Tasks().Wait(t.Context(), info.ID)
		sent := f.messages()
		var texts []string
		for _, m := range sent {
			texts = append(texts, msgText(m))
		}
		if got.Status != kernel.TaskSucceeded || strings.Join(texts, "|") != "start|b\n\nc|a" {
			t.Fatalf("%s sent %q", got.Status, texts)
		}
		if strings.Count(got.Result.Summary, ".") != 3 || len(got.Result.Content) != 3 {
			t.Fatalf("result %+v", got.Result)
		}

		cut := round{events: []peer.Event{{Kind: peer.EventTask, TaskID: "r9", ContextID: "c9", State: peer.StateWorking}}, hold: true}
		g := newFake("writer", cut, completed("r10", "c9", "short", "."))
		if _, err := peer.Register(s.Registrar(), g); err != nil {
			t.Fatal(err)
		}
		info = hostSend(t, s, "writer", "long story")
		synctest.Wait()
		if err := s.Tasks().Send(t.Context(), info.ID, kernel.TaskInstruction{Text: "make it short", Mode: kernel.ModeInterrupt}); err != nil {
			t.Fatal(err)
		}
		got, _ = s.Tasks().Wait(t.Context(), info.ID)
		sent = g.messages()
		if got.Status != kernel.TaskSucceeded || len(sent) != 2 || msgText(sent[1]) != "make it short" || sent[1].ContextID != "c9" || strings.Join(g.canceled(), ",") != "r9" {
			t.Fatalf("interrupt: %s %+v canceled %v", got.Status, sent, g.canceled())
		}
		if len(got.Result.Content) != 1 { // the canceled round counts for nothing
			t.Fatalf("interrupt result %+v", got.Result)
		}
		_ = ag.Close(t.Context())
	})
}

// TestStopCancelsRemote: stopping the task cancels the peer's unfinished task, and what it held is unprocessed.
func TestStopCancelsRemote(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFake("translator", round{events: []peer.Event{{Kind: peer.EventTask, TaskID: "r1", ContextID: "c1", State: peer.StateWorking}}, hold: true})
		ag := newAgent(t, "Tater", store.NewMemory(), providertest.NewScriptProvider(provider.APIAnthropicMessages), kernel.Use(peer.New(f)), noWake)
		s, _ := ag.NewSession(t.Context())
		info := hostSend(t, s, "translator", "work")
		synctest.Wait()
		_ = s.Tasks().Send(t.Context(), info.ID, kernel.TaskInstruction{Text: "later"})
		synctest.Wait()
		if err := s.Tasks().Stop(t.Context(), info.ID, "enough"); err != nil {
			t.Fatal(err)
		}
		got, _ := s.Tasks().Wait(t.Context(), info.ID)
		if got.Status != kernel.TaskCanceled || strings.Join(f.canceled(), ",") != "r1" || len(got.UnprocessedInstructions) != 1 || got.UnprocessedInstructions[0].Text != "later" {
			t.Fatalf("%s canceled %v unprocessed %+v", got.Status, f.canceled(), got.UnprocessedInstructions)
		}
		_ = ag.Close(t.Context())
	})
}

// TestPeerFailure: a failed exchange fails the task with the peer's message.
func TestPeerFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFake("translator", round{events: []peer.Event{
			{Kind: peer.EventTask, TaskID: "r1", ContextID: "c1", State: peer.StateSubmitted},
			{Kind: peer.EventStatus, TaskID: "r1", ContextID: "c1", State: peer.StateFailed, Message: agentMsg("quota exceeded")},
		}})
		ag := newAgent(t, "Tater", store.NewMemory(), providertest.NewScriptProvider(provider.APIAnthropicMessages), kernel.Use(peer.New(f)), noWake)
		s, _ := ag.NewSession(t.Context())
		info := hostSend(t, s, "translator", "work")
		got, _ := s.Tasks().Wait(t.Context(), info.ID)
		if got.Status != kernel.TaskFailed || got.Error != "translator ended the exchange as failed: quota exceeded" {
			t.Fatalf("%s %q", got.Status, got.Error)
		}
		_ = ag.Close(t.Context())
	})
}

// TestCycleRefused: an exchange that would reach an agent already on the call chain is refused before anything is sent.
func TestCycleRefused(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFake("Upstream")
		sp := providertest.NewScriptProvider(provider.APIAnthropicMessages,
			calls(call{"t1", "a2a_send", `{"agent":"Upstream","message":"hi"}`}), reply("Refused."))
		ag := newAgent(t, "Tater", store.NewMemory(), sp, kernel.Use(peer.New(f)))
		s, _ := ag.NewSession(t.Context())
		rc, err := s.Send(kernel.Input{Text: "relay this", Origin: message.Origin{Kind: message.OriginAgent, Source: "a2a:Upstream"}, Metadata: map[string]string{"agent_call_chain": `["Upstream"]`}})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := rc.Wait(t.Context()); err != nil {
			t.Fatal(err)
		}
		res := toolResults(t, s)["t1"]
		if !res.IsError || resultText(res) != "Cyclic agent call Upstream→Tater→Upstream refused: Upstream is already waiting on this conversation." || len(f.messages()) != 0 {
			t.Fatalf("%v %q, %d sent", res.IsError, resultText(res), len(f.messages()))
		}
		_ = ag.Close(t.Context())
	})
}

// TestAuth: a rejected request goes again once a hook says the credentials are fixed; without a hook the task fails.
func TestAuth(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFake("bank", round{err: &peer.AuthError{Status: 401, Detail: `Bearer realm="bank"`}}, completed("r1", "c1", "ok", "."), round{err: &peer.AuthError{Status: 401}})
		var seen []peer.AuthInput
		hook := kernel.WithHook(peer.AuthRequired, "login", func(_ context.Context, in peer.AuthInput) (peer.AuthOutput, error) {
			seen = append(seen, in)
			return peer.AuthOutput{Retry: len(seen) == 1}, nil
		})
		ag := newAgent(t, "Tater", store.NewMemory(), providertest.NewScriptProvider(provider.APIAnthropicMessages), kernel.Use(peer.New(f)), noWake, hook)
		s, _ := ag.NewSession(t.Context())
		info := hostSend(t, s, "bank", "balance?")
		got, _ := s.Tasks().Wait(t.Context(), info.ID)
		sent := f.messages()
		if got.Status != kernel.TaskSucceeded || len(sent) != 2 || sent[0].ID != sent[1].ID || len(seen) != 1 || seen[0].Status != 401 || seen[0].Peer != "bank" {
			t.Fatalf("%s %d sent, ids %v, hook %+v", got.Status, len(sent), sent, seen)
		}
		info = hostSend(t, s, "bank", "again")
		got, _ = s.Tasks().Wait(t.Context(), info.ID)
		if got.Status != kernel.TaskFailed || got.Error != "bank requires authorization that was not provided." {
			t.Fatalf("given up: %s %q", got.Status, got.Error)
		}
		_ = ag.Close(t.Context())
	})
}

func systemText(r *provider.Request) string {
	var b strings.Builder
	for _, sb := range r.System {
		b.WriteString(sb.Text)
	}
	return b.String()
}

// TestResumeRebuildsContext: after a resume, the next message to the agent continues the conversation the task table remembers.
func TestResumeRebuildsContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st := store.NewMemory()
		f := newFake("translator", completed("r1", "c1", "A", "."))
		sp := providertest.NewScriptProvider(provider.APIAnthropicMessages, calls(call{"t1", "a2a_send", `{"agent":"translator","message":"one"}`}), reply("1"))
		ag := newAgent(t, "Tater", st, sp, kernel.Use(peer.New(f)))
		s, _ := ag.NewSession(t.Context())
		ask(t, s, "1")
		id := s.Info().SessionID
		_ = ag.Close(t.Context())

		g := newFake("translator", completed("r2", "c1", "B", "."))
		sp2 := providertest.NewScriptProvider(provider.APIAnthropicMessages, calls(call{"t2", "a2a_send", `{"agent":"translator","message":"two"}`}), reply("2"))
		ag2 := newAgent(t, "Tater", st, sp2, kernel.Use(peer.New(g)))
		s2, err := ag2.Resume(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		ask(t, s2, "2")
		if sent := g.messages(); len(sent) != 1 || sent[0].ContextID != "c1" {
			t.Fatalf("after the resume: %+v", sent)
		}
		_ = ag2.Close(t.Context())
	})
}

// TestContinueChecksThePeer: a continuation asks the peer where its task is first: a question is answered on that task, a task it no longer knows gets a new message in the same conversation.
func TestContinueChecksThePeer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFake("translator", completed("r1", "c1", "A", "."), completed("r1", "c1", "B", "."), completed("r2", "c1", "C", "."))
		ag := newAgent(t, "Tater", store.NewMemory(), providertest.NewScriptProvider(provider.APIAnthropicMessages), kernel.Use(peer.New(f)), noWake)
		s, _ := ag.NewSession(t.Context())
		info := hostSend(t, s, "translator", "one")
		if got, _ := s.Tasks().Wait(t.Context(), info.ID); got.Status != kernel.TaskSucceeded {
			t.Fatalf("first: %s", got.Status)
		}
		f.mu.Lock()
		f.subs["r1"] = []peer.Event{{Kind: peer.EventTask, TaskID: "r1", ContextID: "c1", State: peer.StateInputRequired, Message: agentMsg("More?")}}
		f.mu.Unlock()
		if err := s.Tasks().Send(t.Context(), info.ID, kernel.TaskInstruction{Text: "yes"}); err != nil {
			t.Fatal(err)
		}
		got, _ := s.Tasks().Wait(t.Context(), info.ID)
		f.mu.Lock()
		delete(f.subs, "r1")
		f.mu.Unlock()
		if err := s.Tasks().Send(t.Context(), info.ID, kernel.TaskInstruction{Text: "again"}); err != nil {
			t.Fatal(err)
		}
		got2, _ := s.Tasks().Wait(t.Context(), info.ID)
		sent := f.messages()
		if got.Status != kernel.TaskSucceeded || got2.Status != kernel.TaskSucceeded || len(sent) != 3 || sent[1].TaskID != "r1" || sent[1].ContextID != "c1" || sent[2].TaskID != "" || sent[2].ContextID != "c1" {
			t.Fatalf("%s %s sent %+v", got.Status, got2.Status, sent)
		}
		_ = ag.Close(t.Context())
	})
}

// TestContinueAuth: a continuation that finds the task waiting for credentials asks the host, sends its metadata to the task and follows it; a refusal cancels the task.
func TestContinueAuth(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFake("bank", completed("r1", "c1", "A", "."), completed("r1", "c1", "B", "."), completed("r2", "c1", "C", "."))
		var seen []peer.AuthInput
		refuse := false
		hook := kernel.WithHook(peer.AuthRequired, "login", func(_ context.Context, in peer.AuthInput) (peer.AuthOutput, error) {
			seen = append(seen, in)
			if refuse {
				return peer.AuthOutput{}, nil
			}
			if in.Status == 401 {
				return peer.AuthOutput{Retry: true}, nil
			}
			return peer.AuthOutput{Retry: true, Metadata: map[string]json.RawMessage{"token": json.RawMessage(`"t"`)}}, nil
		})
		ag := newAgent(t, "Tater", store.NewMemory(), providertest.NewScriptProvider(provider.APIAnthropicMessages), kernel.Use(peer.New(f)), noWake, hook)
		s, _ := ag.NewSession(t.Context())
		info := hostSend(t, s, "bank", "one")
		_, _ = s.Tasks().Wait(t.Context(), info.ID)
		f.mu.Lock()
		f.subs["r1"] = []peer.Event{{Kind: peer.EventTask, TaskID: "r1", ContextID: "c1", State: peer.StateAuthRequired, Message: agentMsg("Log in.")}}
		f.subErr = &peer.AuthError{Status: 401}
		f.mu.Unlock()
		if err := s.Tasks().Send(t.Context(), info.ID, kernel.TaskInstruction{Text: "go"}); err != nil {
			t.Fatal(err)
		}
		got, _ := s.Tasks().Wait(t.Context(), info.ID)
		sent := f.messages()
		if got.Status != kernel.TaskSucceeded || len(seen) != 2 || seen[0].Status != 401 || seen[1].Detail != "Log in." || len(sent) != 3 ||
			sent[1].TaskID != "r1" || len(sent[1].Parts) != 0 || string(sent[1].Metadata["token"]) != `"t"` || msgText(sent[2]) != "go" || sent[2].TaskID != "" {
			t.Fatalf("%s hook %+v sent %+v", got.Status, seen, sent)
		}

		refuse = true
		f.mu.Lock()
		f.subs["r2"] = f.subs["r1"]
		f.mu.Unlock()
		if err := s.Tasks().Send(t.Context(), info.ID, kernel.TaskInstruction{Text: "again"}); err != nil {
			t.Fatal(err)
		}
		got, _ = s.Tasks().Wait(t.Context(), info.ID)
		if got.Status != kernel.TaskFailed || !slices.Contains(f.canceled(), "r2") {
			t.Fatalf("given up: %s canceled %v", got.Status, f.canceled())
		}
		_ = ag.Close(t.Context())
	})
}

// TestRestartNewContext: a restart starts a new conversation with the original message.
func TestRestartNewContext(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFake("translator", completed("r1", "c1", "A", "."), completed("r2", "c2", "B", "."))
		ag := newAgent(t, "Tater", store.NewMemory(), providertest.NewScriptProvider(provider.APIAnthropicMessages), kernel.Use(peer.New(f)), noWake)
		s, _ := ag.NewSession(t.Context())
		info := hostSend(t, s, "translator", "one")
		_, _ = s.Tasks().Wait(t.Context(), info.ID)
		if err := s.Tasks().Restart(t.Context(), info.ID, nil); err != nil {
			t.Fatal(err)
		}
		got, _ := s.Tasks().Wait(t.Context(), info.ID)
		sent := f.messages()
		if got.Status != kernel.TaskSucceeded || got.Attempt != 2 || len(sent) != 2 || sent[1].ContextID != "" || msgText(sent[1]) != "one" {
			t.Fatalf("%s attempt %d sent %+v", got.Status, got.Attempt, sent)
		}
		_ = ag.Close(t.Context())
	})
}

// TestInspectAsksThePeer: inspecting a running exchange reads the peer's task now.
func TestInspectAsksThePeer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFake("translator", round{events: []peer.Event{{Kind: peer.EventTask, TaskID: "r1", ContextID: "c1", State: peer.StateWorking}}, hold: true})
		f.subs["r1"] = []peer.Event{{Kind: peer.EventTask, TaskID: "r1", ContextID: "c1", State: peer.StateWorking, Message: agentMsg("Half done.")}}
		ag := newAgent(t, "Tater", store.NewMemory(), providertest.NewScriptProvider(provider.APIAnthropicMessages), kernel.Use(peer.New(f)), noWake)
		s, _ := ag.NewSession(t.Context())
		info := hostSend(t, s, "translator", "work")
		synctest.Wait()
		got, err := s.Tasks().Inspect(t.Context(), info.ID)
		if err != nil || got.Progress.Text != "Half done." || !strings.Contains(string(got.Progress.Data), `"state":"working"`) {
			t.Fatalf("inspect: %+v %v", got.Progress, err)
		}
		_ = s.Tasks().Stop(t.Context(), info.ID, "")
		_, _ = s.Tasks().Wait(t.Context(), info.ID)
		_ = ag.Close(t.Context())
	})
}

// TestNamed: a card name the model cannot use gets the host's name, in the catalog, the enum and the exchange.
func TestNamed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFake("接龙大师", completed("r1", "c1", "好", "。"))
		sp := providertest.NewScriptProvider(provider.APIAnthropicMessages,
			calls(call{"t1", "a2a_send", `{"agent":"jielong","message":"开始接龙"}`}), reply("Done."))
		ag := newAgent(t, "Tater", store.NewMemory(), sp, kernel.Use(peer.New(peer.Named(f, "jielong"), newFake("另一个"))))
		s, _ := ag.NewSession(t.Context())
		ask(t, s, "Play.")
		req := sp.Requests()[0]
		if !strings.Contains(systemText(req), "<name>jielong</name>") {
			t.Fatalf("catalog: %s", systemText(req))
		}
		for _, d := range req.Tools {
			if d.Name == "a2a_send" && !strings.Contains(string(d.InputSchema), `"enum":["___","jielong"]`) {
				t.Fatalf("enum: %s", d.InputSchema)
			}
		}
		if r := toolResults(t, s)["t1"]; r.IsError || !strings.Contains(resultText(r), `agent="jielong"`) || len(f.messages()) != 1 {
			t.Fatalf("result %q, %d sent", resultText(r), len(f.messages()))
		}
		_ = ag.Close(t.Context())
	})
}

// TestAuthHookFailures: a failing peer_auth_required hook (open) gives no retry, so the exchange fails as unauthorized.
func TestAuthHookFailures(t *testing.T) {
	for _, kind := range []string{"error", "panic", "timeout"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				f := newFake("bank", round{err: &peer.AuthError{Status: 401}})
				hook := kernel.WithHook(peer.AuthRequired, "broken", func(ctx context.Context, _ peer.AuthInput) (peer.AuthOutput, error) {
					switch kind {
					case "panic":
						panic("hook bug")
					case "timeout":
						<-ctx.Done()
						return peer.AuthOutput{}, ctx.Err()
					}
					return peer.AuthOutput{}, errors.New("hook broke")
				}, kernel.Timeout(10*time.Millisecond))
				ag := newAgent(t, "Tater", store.NewMemory(), providertest.NewScriptProvider(provider.APIAnthropicMessages), kernel.Use(peer.New(f)), noWake, hook)
				s, _ := ag.NewSession(t.Context())
				got, _ := s.Tasks().Wait(t.Context(), hostSend(t, s, "bank", "balance?").ID)
				if got.Status != kernel.TaskFailed || got.Error != "bank requires authorization that was not provided." || len(f.messages()) != 1 {
					t.Fatalf("%s %q, %d sent", got.Status, got.Error, len(f.messages()))
				}
				_ = ag.Close(t.Context())
			})
		})
	}
}
