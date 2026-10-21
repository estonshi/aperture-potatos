package memory_test

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"testing/synctest"
	"time"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/ext/memory"
	"github.com/estonshi/aperture-potatos/internal/determ"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/providertest"
	"github.com/estonshi/aperture-potatos/store"
)

var usage = message.Usage{InputTokens: 10, OutputTokens: 2}

func producer() *message.Producer {
	return &message.Producer{API: string(provider.APIAnthropicMessages), Endpoint: "script.invalid", Model: "m"}
}

func reply(text string) []providertest.Step {
	u := usage
	m := &message.Message{Role: message.RoleAssistant, Origin: message.Origin{Kind: message.OriginModel}, Content: message.Content{&message.TextBlock{Text: text}}, Producer: producer(), Usage: &u, StopReason: string(provider.StopEndTurn)}
	return []providertest.Step{
		{Event: provider.StreamEvent{Kind: provider.SEStart, ResponseID: "resp"}},
		{Event: provider.StreamEvent{Kind: provider.SEBlockStart, Index: 0, BlockType: message.TypeText}},
		{Event: provider.StreamEvent{Kind: provider.SETextDelta, Index: 0, Channel: "text", Text: text}},
		{Event: provider.StreamEvent{Kind: provider.SEBlockStop, Index: 0, Block: &message.TextBlock{Text: text}}},
		{Event: provider.StreamEvent{Kind: provider.SEEnd, Message: m, Stop: &provider.Stop{Reason: provider.StopEndTurn, Raw: "end_turn"}, Usage: &u}},
	}
}

func call(id, name, input string) []providertest.Step {
	u := usage
	b := &message.ToolCallBlock{ID: id, Name: name, Input: json.RawMessage(input)}
	m := &message.Message{Role: message.RoleAssistant, Origin: message.Origin{Kind: message.OriginModel}, Content: message.Content{b}, Producer: producer(), Usage: &u, StopReason: string(provider.StopToolUse)}
	return []providertest.Step{
		{Event: provider.StreamEvent{Kind: provider.SEStart, ResponseID: "resp"}},
		{Event: provider.StreamEvent{Kind: provider.SEBlockStart, Index: 0, BlockType: message.TypeToolCall, ToolCallID: id, ToolName: name}},
		{Event: provider.StreamEvent{Kind: provider.SEToolArgsDelta, Index: 0, ToolCallID: id, Text: input}},
		{Event: provider.StreamEvent{Kind: provider.SEBlockStop, Index: 0, Block: b}},
		{Event: provider.StreamEvent{Kind: provider.SEEnd, Message: m, Stop: &provider.Stop{Reason: provider.StopToolUse, Raw: "tool_use"}, Usage: &u}},
	}
}

type recorder struct {
	mu     sync.Mutex
	events []event.Event
}

func (r *recorder) handle(_ context.Context, ev event.Event) error {
	r.mu.Lock()
	r.events = append(r.events, ev)
	r.mu.Unlock()
	return nil
}

func (r *recorder) of(typ event.Type) []event.Event {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []event.Event
	for _, ev := range r.events {
		if ev.Type == typ {
			out = append(out, ev)
		}
	}
	return out
}

type stubCompactor struct{}

func (stubCompactor) Compact(context.Context, kernel.CompactRequest) (kernel.CompactResult, error) {
	return kernel.CompactResult{Summary: "Earlier the user talked about editors.", Strategy: "stub"}, nil
}

var compactor = kernel.Use(kernel.ExtensionFunc("comp", func(r kernel.Registrar) error { _, err := r.Compactor(stubCompactor{}); return err }))

type harness struct {
	t    *testing.T
	sp   *providertest.ScriptProvider
	st   store.SessionStore
	ag   *kernel.Agent
	sess *kernel.Session
	rec  *recorder
}

func newHarness(t *testing.T, scripts [][]providertest.Step, labels map[string]string, opts ...kernel.Option) *harness {
	t.Helper()
	h := &harness{t: t, sp: providertest.NewScriptProvider(provider.APIAnthropicMessages, scripts...), st: store.NewMemory(), rec: &recorder{}}
	base := []kernel.Option{
		kernel.WithModel(provider.ModelRef{Provider: h.sp, Model: "m"}),
		kernel.WithStore(h.st),
		kernel.WithClock(determ.NewStepClock(time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC), time.Millisecond)),
		kernel.WithIDGenerator(determ.NewSequentialIDs()),
		kernel.WithPanicOnInternalError(true),
	}
	ag, err := kernel.New(append(base, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	h.ag = ag
	t.Cleanup(func() { _ = ag.Close(context.Background()) }) // a test that fails before h.close would otherwise leave the pumps blocked, which synctest reports as a deadlock panic
	if h.sess, err = ag.NewSession(t.Context(), kernel.WithSessionFunc(h.rec.handle), kernel.WithLabels(labels)); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *harness) turn(text string) {
	h.t.Helper()
	rc, err := h.sess.Send(kernel.Text(text))
	if err != nil {
		h.t.Fatal(err)
	}
	if _, err := rc.Wait(h.t.Context()); err != nil {
		h.t.Fatal(err)
	}
}

func (h *harness) close() {
	h.t.Helper()
	if err := h.ag.Close(context.Background()); err != nil {
		h.t.Fatal(err)
	}
	synctest.Wait()
}

func (h *harness) lines() []message.Message {
	h.t.Helper()
	lines, err := h.st.Load(context.Background(), h.sess.Info().SessionID)
	if err != nil {
		h.t.Fatal(err)
	}
	return lines
}

func reminders(msgs []message.Message, source string) []string {
	var out []string
	for _, m := range msgs {
		for _, b := range m.Content {
			if r, ok := b.(*message.ReminderBlock); ok && r.Source == source {
				out = append(out, r.Text)
			}
		}
	}
	return out
}

var docs = fstest.MapFS{"AGENTS.md": {Data: []byte("\nUse tabs.\n")}, "team/STYLE.md": {Data: []byte("Short commit messages.")}}

func str(s string) *string { return &s }

// seeded holds one memory for each combination of tags, and two that sessions of u1 in p1 must not see.
func seeded(t *testing.T) memory.Store {
	st := memory.NewInMemory()
	var adds []memory.Change
	for _, it := range []struct{ user, project, content string }{
		{"u1", "p1", "In this project the user wants answers in Chinese."},
		{"", "p1", "The project builds with make."},
		{"u1", "", "The user's editor is Helix."},
		{"u2", "p1", "Another user's editor is Emacs."},
		{"u1", "p2", "In another project the user writes Rust."},
	} {
		adds = append(adds, memory.Change{Op: "add", Content: str(it.content), User: str(it.user), Project: str(it.project)})
	}
	if _, err := st.Apply(t.Context(), memory.Scope{}, adds); err != nil {
		t.Fatal(err)
	}
	return st
}

var u1p1 = map[string]string{"user_id": "u1", "project_id": "p1"}

const loaded = "Long-term memory for this conversation (user=u1 project=p1). Follow it like the instructions above; when the user now says otherwise, what the user says wins.\n\n" +
	"- [mem_1 user=u1 project=p1] In this project the user wants answers in Chinese.\n- [mem_2 user= project=p1] The project builds with make.\n- [mem_3 user=u1 project=] The user's editor is Helix."

// TestLoadedInEveryNewEpoch: the instruction files and the memories whose tags match ride in the epoch head of a new session and of each compaction's epoch, and a resume adds none.
func TestLoadedInEveryNewEpoch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, [][]providertest.Step{reply("a"), reply("b")}, u1p1, compactor,
			kernel.Use(memory.New(memory.Options{Store: seeded(t), ConsolidateOn: []string{}, Instructions: memory.Files(docs, "AGENTS.md", "missing.md", "team/STYLE.md")})))
		h.turn("one")
		if _, err := h.sess.Compact(t.Context(), ""); err != nil {
			t.Fatal(err)
		}
		id := h.sess.Info().SessionID
		if err := h.sess.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		again, err := h.ag.Resume(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		h.sess = again
		h.turn("two")
		got := reminders(h.lines(), "hook:memory")
		want := "Contents of AGENTS.md:\n\nUse tabs.\n\nContents of team/STYLE.md:\n\nShort commit messages.\n\n" + loaded
		if len(got) != 2 || got[0] != want || got[1] != want {
			t.Fatalf("%q", got)
		}
		for _, m := range h.lines() {
			if len(reminders([]message.Message{m}, "hook:memory")) > 0 && m.Origin.Kind != message.OriginEpochHead {
				t.Fatalf("memory outside an epoch head: %+v", m.Origin)
			}
		}
		synctest.Wait() // closing the first session does not wait for its pump to deliver
		if evs := h.rec.of("memory.loaded"); len(evs) != 2 || !slices.Equal(evs[0].Data.(*memory.Loaded).IDs, []string{"mem_1", "mem_2", "mem_3"}) {
			t.Fatalf("%+v", evs)
		}
		if names := []string{h.sp.Requests()[0].Tools[0].Name, h.sp.Requests()[0].Tools[1].Name}; !slices.Equal(names, []string{"memory_forget", "memory_save"}) {
			t.Fatalf("tools %v", names)
		}
		h.close()
	})
}

func TestFilesCapped(t *testing.T) {
	big := strings.Repeat("é", 10000) // 20000 bytes
	fsys := fstest.MapFS{"A.md": {Data: []byte(big)}, "B.md": {Data: []byte(big)}, "C.md": {Data: []byte("never")}}
	got, err := memory.Files(fsys, "A.md", "B.md", "C.md").Load(t.Context(), memory.Scope{})
	note := "\n[Cut here: the instruction files together are longer than 25600 bytes.]"
	if err != nil || len(got) != 2 || got[0].Content != big || !strings.HasSuffix(got[1].Content, note) || len(strings.TrimSuffix(got[1].Content, note)) != 25600-20000 {
		t.Fatalf("%d docs %v", len(got), err)
	}
}

func results(lines []message.Message) []string {
	var out []string
	for _, m := range lines {
		for _, b := range m.Content {
			if r, ok := b.(*message.ToolResultBlock); ok {
				out = append(out, message.Message{Content: r.Content}.Text())
			}
		}
	}
	return out
}

// TestModelWritesStayOwn: by default the model's writes land under the session's user, and a shared memory can be neither changed nor deleted by a tool or a consolidation.
func TestModelWritesStayOwn(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st := seeded(t)
		consolidation := `{"changes": [{"op": "add", "content": "The user likes tea.", "user": "", "project": ""}, {"op": "delete", "id": "mem_2"}]}`
		h := newHarness(t, [][]providertest.Step{
			call("toolu_1", "memory_save", `{"content":"Everyone must obey this.","user":"","project":""}`),
			call("toolu_2", "memory_forget", `{"id":"mem_2"}`),
			call("toolu_3", "memory_save", `{"content":"The project builds with nix.","user":"","project":"p1","replaces":"mem_2"}`),
			reply("done"), reply(consolidation),
		}, u1p1, kernel.Use(memory.New(memory.Options{Store: st, ConsolidateOn: []string{}})))
		h.turn("remember things")
		want := []string{"Saved as mem_6.", "Memory mem_2 is shared with other users, so it cannot be changed or deleted from this conversation.",
			"Memory mem_2 is shared with other users, so it cannot be changed or deleted from this conversation."}
		if got := results(h.lines()); !slices.Equal(got, want) {
			t.Fatalf("%q", got)
		}
		if err := memory.Consolidate(t.Context(), h.sess); err != nil {
			t.Fatal(err)
		}
		items, _ := st.List(t.Context(), memory.Scope{User: "u1", Project: "p1"})
		var contents []string
		for _, it := range items {
			contents = append(contents, it.ID+"@"+it.User+"/"+it.Project+":"+it.Content)
		}
		if strings.Join(contents, "|") != "mem_1@u1/p1:In this project the user wants answers in Chinese.|mem_2@/p1:The project builds with make.|mem_3@u1/:The user's editor is Helix.|mem_6@u1/:Everyone must obey this.|mem_7@u1/:The user likes tea." {
			t.Fatalf("%q", contents)
		}
		h.close()
	})
}

// TestToolsChangeMemory: memory_save adds, rewrites with replaces and refuses a foreign tag or a full budget, memory_forget deletes, and another open session that loads a changed memory hears what its memory now reads.
func TestToolsChangeMemory(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st := seeded(t)
		h := newHarness(t, [][]providertest.Step{
			call("toolu_1", "memory_save", `{"content":"The user prefers dark themes.","user":"u1","project":""}`),
			call("toolu_2", "memory_save", `{"content":"Someone else's.","user":"u2","project":""}`),
			call("toolu_3", "memory_save", `{"content":"The user prefers light themes.","user":"u1","project":"p1","replaces":"mem_6"}`),
			call("toolu_4", "memory_forget", `{"id":"mem_2"}`),
			call("toolu_5", "memory_forget", `{"id":"mem_4"}`),
			call("toolu_6", "memory_save", `{"content":"`+strings.Repeat("word ", 60)+`","user":"u1","project":""}`),
			reply("done"), reply("other"), reply("stranger"),
		}, u1p1, kernel.Use(memory.New(memory.Options{Store: st, ConsolidateOn: []string{}, UserTokens: 80, SharedWrites: true})))
		other, err := h.ag.NewSession(t.Context(), kernel.WithLabels(u1p1))
		if err != nil {
			t.Fatal(err)
		}
		stranger, err := h.ag.NewSession(t.Context(), kernel.WithLabels(map[string]string{"user_id": "u9", "project_id": "p9"}))
		if err != nil {
			t.Fatal(err)
		}
		h.turn("remember things")
		want := []string{"Saved as mem_6.", `The user tag must be "u1" (this conversation's) or empty.`, "Saved as mem_6.", "Forgot mem_2.", "There is no memory mem_4.",
			"Memory is full: memories with a user tag would use 103 of 80 tokens. Merge related memories with replaces, or forget one, first."}
		if got := results(h.lines()); !slices.Equal(got, want) {
			t.Fatalf("%q", got)
		}
		items, _ := st.List(t.Context(), memory.Scope{User: "u1", Project: "p1"})
		var contents []string
		for _, it := range items {
			contents = append(contents, it.ID+"@"+it.User+"/"+it.Project+":"+it.Content)
		}
		if strings.Join(contents, "|") != "mem_1@u1/p1:In this project the user wants answers in Chinese.|mem_3@u1/:The user's editor is Helix.|mem_6@u1/p1:The user prefers light themes." {
			t.Fatalf("%q", contents)
		}
		synctest.Wait() // the recorder's pump delivers the events
		if n := len(h.rec.of("memory.saved")); n != 3 {
			t.Fatalf("%d saves", n)
		}
		for _, s := range []*kernel.Session{other, stranger} {
			rc, _ := s.Send(kernel.Text("next"))
			_, _ = rc.Wait(t.Context())
		}
		lines := func(s *kernel.Session) []message.Message {
			l, _ := h.st.Load(t.Context(), s.Info().SessionID)
			return l
		}
		heard := reminders(lines(other), "memory")
		if len(heard) != 3 || !strings.HasSuffix(heard[2], "- [mem_6 user=u1 project=p1] The user prefers light themes.") || strings.Contains(heard[2], "make") {
			t.Fatalf("other session heard %q", heard)
		}
		if got := reminders(lines(stranger), "memory"); len(got) != 0 {
			t.Fatalf("a stranger heard %q", got)
		}
		h.close()
	})
}

const noChange = `{"changes": []}`

// TestConsolidateOnClose: closing asks the model over the conversation for the changes, with the limits and what is used, and applies what it answers.
func TestConsolidateOnClose(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st := seeded(t)
		h := newHarness(t, [][]providertest.Step{reply("Noted."),
			reply("```json\n{\"changes\": [{\"op\": \"add\", \"content\": \"The user indents with tabs.\", \"user\": \"u1\", \"project\": \"\"}, {\"op\": \"update\", \"id\": \"mem_2\", \"content\": \"The project builds with make and Go 1.25.\"}]}\n```"),
		}, u1p1, kernel.Use(memory.New(memory.Options{Store: st, ConsolidateOn: []string{"session_end"}, SharedWrites: true})))
		h.turn("From now on I indent with tabs. We need Go 1.25.")
		h.close()
		reqs := h.sp.Requests()
		if len(reqs) != 2 {
			t.Fatalf("%d requests", len(reqs))
		}
		last := reqs[1].Messages[len(reqs[1].Messages)-1].Text()
		if !strings.Contains(last, "Limits: memories with a user tag use 20 of 10000 tokens (about 7500 words); the others use 8 of 10000 tokens (about 7500 words).") ||
			!strings.Contains(last, "- [mem_2 user= project=p1] The project builds with make.") || strings.Contains(last, "Emacs") || !slices.ContainsFunc(reqs[1].Messages, func(m message.Message) bool { return strings.Contains(m.Text(), "indent with tabs") }) {
			t.Fatalf("consolidation request:\n%s", last)
		}
		items, _ := st.List(t.Context(), memory.Scope{User: "u1", Project: "p1"})
		if len(items) != 4 || items[1].Content != "The project builds with make and Go 1.25." || items[3].ID != "mem_6" || items[3].User != "u1" {
			t.Fatalf("%+v", items)
		}
		saved := h.rec.of("memory.saved")
		if len(saved) != 1 || saved[0].Data.(*memory.Saved).Trigger != "session_end" || saved[0].Data.(*memory.Saved).Changes[0].ID != "mem_6" {
			t.Fatalf("%+v", saved)
		}
		for _, m := range h.lines() {
			if strings.Contains(m.Text(), "Review the conversation above") {
				t.Fatal("the consolidation prompt reached the history")
			}
		}
	})
}

// TestConsolidateBeforeCompaction: the consolidation runs before the compaction takes lines out, so the model sees them.
func TestConsolidateBeforeCompaction(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st := memory.NewInMemory()
		h := newHarness(t, [][]providertest.Step{reply("ok"), reply(`{"changes":[{"op":"add","content":"The user's editor is Helix.","user":"u1","project":""}]}`)}, u1p1, compactor,
			kernel.Use(memory.New(memory.Options{Store: st, ConsolidateOn: []string{"pre_compact"}})))
		h.turn("I use Helix.")
		if _, err := h.sess.Compact(t.Context(), ""); err != nil {
			t.Fatal(err)
		}
		if req := h.sp.Requests()[1]; !slices.ContainsFunc(req.Messages, func(m message.Message) bool { return m.Text() == "I use Helix." }) {
			t.Fatalf("consolidation did not see the conversation: %+v", req.Messages)
		}
		if items, _ := st.List(t.Context(), memory.Scope{User: "u1"}); len(items) != 1 {
			t.Fatalf("%+v", items)
		}
		synctest.Wait() // the recorder's pump delivers the event
		if saved := h.rec.of("memory.saved"); len(saved) != 1 || saved[0].Data.(*memory.Saved).Trigger != "pre_compact" {
			t.Fatalf("%+v", saved)
		}
		h.close()
	})
}

// TestConsolidationAllOrNothing: one bad change drops the whole reply, which costs a hook failure and changes nothing.
func TestConsolidationAllOrNothing(t *testing.T) {
	for name, answer := range map[string]string{
		"foreign tag": `{"changes":[{"op":"add","content":"A.","user":"u1","project":""},{"op":"add","content":"B.","user":"u7","project":""}]}`,
		"unknown id":  `{"changes":[{"op":"add","content":"A.","user":"u1","project":""},{"op":"delete","id":"mem_99"}]}`,
		"not json":    "Nothing to save.",
	} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				st := memory.NewInMemory()
				h := newHarness(t, [][]providertest.Step{reply("ok"), reply(answer)}, u1p1, kernel.Use(memory.New(memory.Options{Store: st, ConsolidateOn: []string{"session_end"}})))
				h.turn("hello")
				h.close()
				if items, _ := st.List(t.Context(), memory.Scope{User: "u1", Project: "p1"}); len(items) != 0 || len(h.rec.of(event.TypeHookFailed)) != 1 {
					t.Fatalf("%+v, %d failures", items, len(h.rec.of(event.TypeHookFailed)))
				}
			})
		})
	}
}

// TestOnlyRootSessionsConsolidate: a side chat never consolidates, and a session in which no person spoke asks nothing.
func TestOnlyRootSessionsConsolidate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, [][]providertest.Step{reply("side"), reply(noChange)}, u1p1, kernel.Use(memory.New(memory.Options{Store: memory.NewInMemory(), ConsolidateOn: []string{"session_end"}})))
		side, err := h.sess.SideChat(t.Context(), "aside")
		if err != nil {
			t.Fatal(err)
		}
		rc, _ := side.Send(kernel.Text("aside question"))
		_, _ = rc.Wait(t.Context())
		if err := side.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		h.close()
		if n := len(h.sp.Requests()); n != 1 {
			t.Fatalf("%d requests: the side chat or the silent main session consolidated", n)
		}
	})
}

type ingesting struct {
	memory.Store
	mu   sync.Mutex
	seen []message.Message
}

func (i *ingesting) Ingest(_ context.Context, _ memory.Scope, lines []message.Message) ([]memory.Change, error) {
	i.mu.Lock()
	i.seen = lines
	i.mu.Unlock()
	return []memory.Change{{Op: "add", ID: "ext_1"}}, nil
}

// TestIngesterGetsTheConversation: a store that extracts memories itself gets the conversation, and no consolidation request is sent.
func TestIngesterGetsTheConversation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st := &ingesting{Store: memory.NewInMemory()}
		h := newHarness(t, [][]providertest.Step{reply("ok")}, u1p1, kernel.Use(memory.New(memory.Options{Store: st, ConsolidateOn: []string{"session_end"}})))
		h.turn("hello")
		h.close()
		if len(h.sp.Requests()) != 1 || len(st.seen) != 2 || st.seen[0].Text() != "hello" || len(h.rec.of("memory.saved")) != 1 {
			t.Fatalf("%d requests, %d lines", len(h.sp.Requests()), len(st.seen))
		}
	})
}

func TestUnknownConsolidationPoint(t *testing.T) {
	if _, err := kernel.New(kernel.WithModel(provider.ModelRef{Provider: providertest.NewScriptProvider(provider.APIAnthropicMessages), Model: "m"}),
		kernel.Use(memory.New(memory.Options{Store: memory.NewInMemory(), ConsolidateOn: []string{"turn_end"}}))); err == nil {
		t.Fatal("an unknown consolidation point was accepted")
	}
}

// TestConsolidateOnDemand: a host consolidates an open session when it likes, and by default a session closes without a model call.
func TestConsolidateOnDemand(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st := memory.NewInMemory()
		h := newHarness(t, [][]providertest.Step{reply("ok"), reply(`{"changes":[{"op":"add","content":"The user's editor is Helix.","user":"u1","project":""}]}`)}, u1p1,
			kernel.Use(memory.New(memory.Options{Store: st})))
		h.turn("I use Helix.")
		if err := memory.Consolidate(t.Context(), h.sess); err != nil {
			t.Fatal(err)
		}
		if items, _ := st.List(t.Context(), memory.Scope{User: "u1"}); len(items) != 1 {
			t.Fatalf("%+v", items)
		}
		synctest.Wait() // the recorder's pump delivers the event
		if saved := h.rec.of("memory.saved"); len(saved) != 1 || saved[0].Data.(*memory.Saved).Trigger != "host" {
			t.Fatalf("%+v", saved)
		}
		h.close()
		if n := len(h.sp.Requests()); n != 2 {
			t.Fatalf("%d requests: closing consolidated again", n)
		}
		plain := newHarness(t, nil, nil)
		if err := memory.Consolidate(t.Context(), plain.sess); err != memory.ErrNotInstalled {
			t.Fatalf("without the extension: %v", err)
		}
		plain.close()
	})
}

// TestIngesterAnnounces: what an ingesting store changed reaches the other open sessions that load it, as a change through the tools does.
func TestIngesterAnnounces(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st := &ingesting{Store: memory.NewInMemory()}
		h := newHarness(t, [][]providertest.Step{reply("ok"), reply("next")}, u1p1, kernel.Use(memory.New(memory.Options{Store: st, ConsolidateOn: []string{}})))
		other, err := h.ag.NewSession(t.Context(), kernel.WithLabels(u1p1))
		if err != nil {
			t.Fatal(err)
		}
		h.turn("hello")
		if err := memory.Consolidate(t.Context(), h.sess); err != nil {
			t.Fatal(err)
		}
		rc, _ := other.Send(kernel.Text("next"))
		_, _ = rc.Wait(t.Context())
		l, _ := h.st.Load(t.Context(), other.Info().SessionID)
		if heard := reminders(l, "memory"); len(heard) != 1 {
			t.Fatalf("the other session heard %q", heard)
		}
		h.close()
	})
}

// TestScopeFollowsChangedLabels: after SetLabels the next memory call checks tags against the new user.
func TestScopeFollowsChangedLabels(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		st := seeded(t)
		h := newHarness(t, [][]providertest.Step{
			call("toolu_1", "memory_save", `{"content":"Prefers tea.","user":"u2","project":""}`), reply("no"),
			call("toolu_2", "memory_save", `{"content":"Prefers tea.","user":"u2","project":""}`), reply("yes"),
		}, u1p1, kernel.Use(memory.New(memory.Options{Store: st, ConsolidateOn: []string{}})))
		h.turn("remember")
		if err := h.sess.SetLabels(t.Context(), map[string]string{"user_id": "u2"}); err != nil {
			t.Fatal(err)
		}
		h.turn("remember again")
		if got := results(h.lines()); len(got) != 2 || !strings.Contains(got[0], `must be "u1"`) || !strings.HasPrefix(got[1], "Saved as ") {
			t.Fatalf("%q", got)
		}
		h.close()
	})
}
