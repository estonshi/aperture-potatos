package skills_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"slices"
	"strings"
	"sync"
	"testing"
	"testing/fstest"
	"testing/synctest"
	"time"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/ext/skills"
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

func call(id, input string) []providertest.Step {
	u := usage
	b := &message.ToolCallBlock{ID: id, Name: "skill", Input: json.RawMessage(input)}
	m := &message.Message{Role: message.RoleAssistant, Origin: message.Origin{Kind: message.OriginModel}, Content: message.Content{b}, Producer: producer(), Usage: &u, StopReason: string(provider.StopToolUse)}
	return []providertest.Step{
		{Event: provider.StreamEvent{Kind: provider.SEStart, ResponseID: "resp"}},
		{Event: provider.StreamEvent{Kind: provider.SEBlockStart, Index: 0, BlockType: message.TypeToolCall, ToolCallID: id, ToolName: "skill"}},
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
	synctest.Wait() // the recorder's pump delivers the events asynchronously
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

type harness struct {
	t    *testing.T
	sp   *providertest.ScriptProvider
	st   store.SessionStore
	ag   *kernel.Agent
	sess *kernel.Session
	rec  *recorder
	logs *bytes.Buffer
	reg  kernel.Registrar // the agent scope's, kept from install, as a host adding sources later keeps it
}

func newHarness(t *testing.T, scripts [][]providertest.Step, opts ...kernel.Option) *harness {
	t.Helper()
	h := &harness{t: t, sp: providertest.NewScriptProvider(provider.APIAnthropicMessages, scripts...), st: store.NewMemory(), rec: &recorder{}, logs: &bytes.Buffer{}}
	base := []kernel.Option{
		kernel.WithModel(provider.ModelRef{Provider: h.sp, Model: "m"}),
		kernel.WithStore(h.st),
		kernel.WithClock(determ.NewStepClock(time.Date(2026, 9, 30, 0, 0, 0, 0, time.UTC), time.Millisecond)),
		kernel.WithIDGenerator(determ.NewSequentialIDs()),
		kernel.WithPanicOnInternalError(true),
		kernel.WithLogger(slog.New(slog.NewTextHandler(h.logs, nil))),
		kernel.Use(kernel.ExtensionFunc("keep", func(r kernel.Registrar) error { h.reg = r; return nil })),
	}
	ag, err := kernel.New(append(base, opts...)...)
	if err != nil {
		t.Fatal(err)
	}
	h.ag = ag
	if h.sess, err = ag.NewSession(t.Context(), kernel.WithSessionFunc(h.rec.handle)); err != nil {
		t.Fatal(err)
	}
	return h
}

func (h *harness) turn(text string) kernel.TurnResult {
	h.t.Helper()
	rc, err := h.sess.Send(kernel.Text(text))
	if err != nil {
		h.t.Fatal(err)
	}
	res, err := rc.Wait(h.t.Context())
	if err != nil {
		h.t.Fatal(err)
	}
	return res
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

// results lists every tool result of the session file, with its pin key when it has one.
func (h *harness) results() (texts []string, pins []string) {
	for _, m := range h.lines() {
		for _, b := range m.Content {
			if r, ok := b.(*message.ToolResultBlock); ok {
				texts = append(texts, message.Message{Content: r.Content}.Text())
				if r.Pin != nil {
					pins = append(pins, r.Pin.Key)
				}
			}
		}
	}
	return texts, pins
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

func example() skills.Options {
	return skills.Options{Sources: []skills.Source{skills.FS(os.DirFS("testdata/skills"))}}
}

// staticSource is a host source: it lists exactly the skills it was given, without the file-system rules.
type staticSource struct {
	list []skills.Frontmatter
	body map[string]string
}

func (s staticSource) List(context.Context) ([]skills.Frontmatter, error) {
	return slices.Clone(s.list), nil
}

func (s staticSource) Load(_ context.Context, name string) (*skills.Skill, error) {
	for _, fm := range s.list {
		if fm.Name == name {
			return &skills.Skill{Frontmatter: fm, Body: s.body[name]}, nil
		}
	}
	return nil, skills.ErrNotFound
}

func (s staticSource) Open(context.Context, string, string) (io.ReadCloser, error) {
	return nil, skills.ErrNotFound
}

func toolOf(req *provider.Request, name string) *message.ToolDef {
	for i := range req.Tools {
		if req.Tools[i].Name == name {
			return &req.Tools[i]
		}
	}
	return nil
}

func system(req *provider.Request) string {
	var parts []string
	for _, b := range req.System {
		parts = append(parts, b.Text)
	}
	return strings.Join(parts, "\n")
}

// TestModelLoadsSkill: the catalog lists only model-invocable skills, the tool's enum names them, and a load returns the body with its resources and pins it; loading it again only points back at it.
func TestModelLoadsSkill(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, [][]providertest.Step{
			call("toolu_1", `{"name":"pdf-processing"}`), call("toolu_2", `{"name":"pdf-processing"}`),
			call("toolu_3", `{"name":"pdf-processing","resource":"references/REFERENCE.md"}`), call("toolu_4", `{"name":"release-notes"}`), reply("done"),
		}, kernel.Use(skills.New(example())))
		h.turn("fill the form")
		req := h.sp.Requests()[0]
		sys := system(req)
		if !strings.Contains(sys, "<available_skills>\n<skill>\n<name>pdf-processing</name>\n<description>Extract PDF text, fill forms, merge files. Use when handling PDFs.</description>\n<location>pdf-processing/SKILL.md</location>\n</skill>\n</available_skills>") || strings.Contains(sys, "release-notes") {
			t.Fatalf("system:\n%s", sys)
		}
		if def := toolOf(req, "skill"); def == nil || !strings.Contains(string(def.InputSchema), `"enum":["pdf-processing"]`) {
			t.Fatalf("tool %+v", def)
		}
		texts, pins := h.results()
		if len(texts) != 4 || !strings.HasPrefix(texts[0], "<skill_content name=\"pdf-processing\" location=\"pdf-processing/SKILL.md\">\n# PDF processing") ||
			!strings.HasSuffix(texts[0], "</skill_content>\n<skill_resources>\n<file>references/REFERENCE.md</file>\n<file>scripts/extract.py</file>\n</skill_resources>") {
			t.Fatalf("results %q", texts)
		}
		if texts[1] != "Skill pdf-processing is already active; its instructions are above." || !strings.HasPrefix(texts[2], "# Form fields") ||
			!strings.HasPrefix(texts[3], `There is no skill named "release-notes" that you can load. Available skills: pdf-processing.`) {
			t.Fatalf("results %q", texts)
		}
		if !slices.Equal(pins, []string{"skill:pdf-processing"}) {
			t.Fatalf("pins %v", pins)
		}
		evs := h.rec.of("skills.activated")
		if len(evs) != 2 {
			t.Fatalf("%d activations", len(evs))
		}
		first, again := evs[0].Data.(*skills.Activated), evs[1].Data.(*skills.Activated)
		if first.Trigger != "model" || first.Deduplicated || first.Tokens == 0 || !slices.Equal(first.AllowedTools, []string{"Bash(pdftotext:*)", "Read"}) || !again.Deduplicated || evs[0].TurnID == "" {
			t.Fatalf("%+v %+v", first, again)
		}
		h.close()
	})
}

// TestCommandLoadsSkill: /release-notes expands the body with its arguments as a command input and pins it in memory, although the model cannot load it.
func TestCommandLoadsSkill(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, [][]providertest.Step{reply("notes")}, kernel.Use(skills.New(example())))
		if !slices.ContainsFunc(h.sess.Commands(), func(c kernel.CommandInfo) bool {
			return c.Name == "release-notes" && c.ArgHint == "[version]" && strings.HasPrefix(c.Description, "Draft release notes")
		}) {
			t.Fatalf("commands %+v", h.sess.Commands())
		}
		rc, err := h.sess.Command("release-notes", "2.1")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := rc.Wait(t.Context()); err != nil {
			t.Fatal(err)
		}
		_, _ = h.sess.WaitQuiet(t.Context())
		user := h.sp.Requests()[0].Messages[0]
		if txt := user.Text(); !strings.Contains(txt, "Write release notes for version 2.1, grouped") {
			t.Fatalf("input %q", txt)
		}
		if pins := h.sess.Pins(); len(pins) != 1 || pins[0].Key != "skill:release-notes" || pins[0].MaxTokens != 5000 {
			t.Fatalf("pins %+v", pins)
		}
		if evs := h.rec.of("skills.activated"); len(evs) != 1 || evs[0].Data.(*skills.Activated).Trigger != "user" {
			t.Fatalf("%+v", evs)
		}
		h.close()
	})
}

// TestActivateHookDenies: skill_activate keyed on the name refuses one load, and the model gets the reason.
func TestActivateHookDenies(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, [][]providertest.Step{call("toolu_1", `{"name":"pdf-processing"}`), reply("ok")}, kernel.Use(skills.New(example())),
			kernel.WithHook(skills.Activate, "policy", func(_ context.Context, in skills.ActivateInput) (skills.ActivateOutput, error) {
				return skills.ActivateOutput{Deny: true, Reason: "not in this workspace"}, nil
			}, kernel.Match(kernel.MatchRegexp("pdf-.*"))))
		h.turn("go")
		texts, pins := h.results()
		if len(texts) != 1 || texts[0] != "Loading skill pdf-processing was denied: not in this workspace" || len(pins) != 0 || len(h.rec.of("skills.activated")) != 0 {
			t.Fatalf("%q %v", texts, pins)
		}
		h.close()
	})
}

// TestAddSourceAnnounces: a source added later and removed again reaches the model as reminders with the next turn, changes the enum and commands, and is reported as catalog_changed.
func TestAddSourceAnnounces(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, [][]providertest.Step{reply("a"), reply("b"), reply("c")}, kernel.Use(skills.New(example())))
		h.turn("one")
		more := skills.FS(fstest.MapFS{
			"csv/SKILL.md":  {Data: []byte("---\nname: csv\ndescription: Clean CSV files.\n---\nUse the csv module.")},
			"memo/SKILL.md": {Data: []byte("---\nname: memo\ndescription: Private notes.\ndisable-model-invocation: true\n---\nM")},
		})
		reg, err := skills.AddSource(h.reg, more)
		if err != nil {
			t.Fatal(err)
		}
		synctest.Wait()
		if n := len(h.sp.Requests()); n != 1 {
			t.Fatalf("the announcement started a turn: %d requests", n)
		}
		h.turn("two")
		reg.Remove()
		h.turn("three")
		reqs := h.sp.Requests()
		if def := toolOf(reqs[1], "skill"); def == nil || !strings.Contains(string(def.InputSchema), `"enum":["csv","pdf-processing"]`) {
			t.Fatalf("tool %+v", def)
		}
		if def := toolOf(reqs[2], "skill"); !strings.Contains(string(def.InputSchema), `"enum":["pdf-processing"]`) {
			t.Fatalf("tool %s", def.InputSchema)
		}
		got := reminders(h.lines(), "ext:skills")
		want := []string{"New skills are available to load with the skill tool:\n- csv: Clean CSV files.", "These skills are no longer available: csv, memo."}
		if !slices.Equal(got, want) {
			t.Fatalf("reminders %q", got)
		}
		evs := h.rec.of("skills.catalog_changed")
		if len(evs) != 2 || !slices.Equal(evs[0].Data.(*skills.CatalogChanged).Added, []string{"csv", "memo"}) || !slices.Equal(evs[1].Data.(*skills.CatalogChanged).Removed, []string{"csv", "memo"}) {
			t.Fatalf("%+v", evs)
		}
		if slices.ContainsFunc(h.sess.Commands(), func(c kernel.CommandInfo) bool { return c.Name == "memo" }) {
			t.Fatal("memo's command outlived its source")
		}
		if strings.Contains(system(reqs[2]), "csv") {
			t.Fatal("the epoch's system changed")
		}
		h.close()
	})
}

// TestNoSkillsNoSurface: without a skill there is no catalog, no tool and no command; the first source added brings the tool.
func TestNoSkillsNoSurface(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, [][]providertest.Step{reply("a"), reply("b")}, kernel.Use(skills.New(skills.Options{})))
		h.turn("one")
		req := h.sp.Requests()[0]
		if toolOf(req, "skill") != nil || strings.Contains(system(req), "available_skills") {
			t.Fatal("a skill surface without skills")
		}
		if _, err := skills.AddSource(h.reg, skills.FS(os.DirFS("testdata/skills"))); err != nil {
			t.Fatal(err)
		}
		h.turn("two")
		if toolOf(h.sp.Requests()[1], "skill") == nil {
			t.Fatal("no tool after the first skill")
		}
		if _, err := skills.AddSource(h.sess.Registrar(), skills.FS(os.DirFS("testdata/skills"))); err != skills.ErrNotInstalled {
			t.Fatalf("session scope: %v", err)
		}
		h.close()
	})
}

// TestCommandNameTaken: a skill named like a command of the host's options or of an earlier extension keeps the tool but gets no command, with a warning in the agent's log, and New does not fail.
func TestCommandNameTaken(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		earlier := kernel.ExtensionFunc("mine", func(r kernel.Registrar) error {
			_, err := r.Command(kernel.PromptCommand("tsv", "Earlier.", "tsv"))
			return err
		})
		h := newHarness(t, nil, kernel.WithCommand(kernel.PromptCommand("csv", "Host.", "csv")), kernel.Use(earlier, skills.New(skills.Options{Sources: []skills.Source{
			skills.FS(fstest.MapFS{
				"csv/SKILL.md": {Data: []byte("---\nname: csv\ndescription: Clean CSV files.\n---\nC")},
				"tsv/SKILL.md": {Data: []byte("---\nname: tsv\ndescription: Clean TSV files.\n---\nT")},
			}),
		}})))
		var got []string
		for _, c := range h.sess.Commands() {
			got = append(got, c.Name+":"+c.Description)
		}
		if strings.Join(got, " ") != "csv:Host. tsv:Earlier." || strings.Count(h.logs.String(), "no command for this skill") != 2 {
			t.Fatalf("%v\n%s", got, h.logs.String())
		}
		h.close()
	})
}

// TestResumeKnowsLoadedSkills: after a resume the pin set comes back from the file, so a skill the model loaded before is not sent again.
func TestResumeKnowsLoadedSkills(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, [][]providertest.Step{call("toolu_1", `{"name":"pdf-processing"}`), reply("ok"), call("toolu_2", `{"name":"pdf-processing"}`), reply("still")}, kernel.Use(skills.New(example())))
		h.turn("load it")
		id := h.sess.Info().SessionID
		if err := h.sess.Close(t.Context()); err != nil {
			t.Fatal(err)
		}
		again, err := h.ag.Resume(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		h.sess = again
		h.turn("again")
		if texts, _ := h.results(); len(texts) != 2 || !strings.HasPrefix(texts[1], "Skill pdf-processing is already active") {
			t.Fatalf("%q", texts)
		}
		h.close()
	})
}

// TestActivateHookFailures: a failing skill_activate hook (open) has no opinion, so the skill loads.
func TestActivateHookFailures(t *testing.T) {
	for _, kind := range []string{"error", "panic", "timeout"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				h := newHarness(t, [][]providertest.Step{call("toolu_1", `{"name":"pdf-processing"}`), reply("ok")}, kernel.Use(skills.New(example())),
					kernel.WithHook(skills.Activate, "broken", func(ctx context.Context, _ skills.ActivateInput) (skills.ActivateOutput, error) {
						switch kind {
						case "panic":
							panic("hook bug")
						case "timeout":
							<-ctx.Done()
							return skills.ActivateOutput{}, ctx.Err()
						}
						return skills.ActivateOutput{}, errors.New("hook broke")
					}, kernel.Timeout(10*time.Millisecond)))
				h.turn("go")
				if len(h.rec.of("skills.activated")) != 1 || len(h.rec.of("hook.failed")) != 1 {
					t.Fatalf("activated %d, failed %d", len(h.rec.of("skills.activated")), len(h.rec.of("hook.failed")))
				}
				h.close()
			})
		})
	}
}

// TestRewriteModel: a rewriter runs after $ARGUMENTS expansion, the tool result and pin hold its text, and a later load without args is already active without calling it again.
func TestRewriteModel(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var calls []skills.RewriteInput
		rewrite := func(_ context.Context, in skills.RewriteInput) (string, error) {
			calls = append(calls, in)
			return strings.ReplaceAll(in.Body, "!`git status`", "working tree clean"), nil
		}
		files := fstest.MapFS{
			"notes/SKILL.md": &fstest.MapFile{Data: []byte("---\nname: notes\ndescription: Draft notes.\n---\nDraft notes for $ARGUMENTS.\n\n!`git status`\n")},
		}
		h := newHarness(t, [][]providertest.Step{
			call("toolu_1", `{"name":"notes","args":"2.1"}`),
			call("toolu_2", `{"name":"notes"}`),
			call("toolu_3", `{"name":"notes","args":"2.2"}`),
			reply("ok"),
		}, kernel.Use(skills.New(skills.Options{Sources: []skills.Source{skills.FS(files)}, Rewrite: rewrite})))
		h.turn("go")
		texts, pins := h.results()
		if len(texts) != 3 || !strings.Contains(texts[0], "Draft notes for 2.1.") || !strings.Contains(texts[0], "working tree clean") || strings.Contains(texts[0], "!`git status`") || !strings.HasPrefix(texts[1], "Skill notes is already active") || !strings.Contains(texts[2], "Draft notes for 2.2.") || !strings.Contains(texts[2], "working tree clean") {
			t.Fatalf("results %q", texts)
		}
		ps := h.sess.Pins()
		if len(pins) != 2 || len(ps) != 1 || !strings.Contains(ps[0].Content, "2.2") || !strings.Contains(ps[0].Content, "working tree clean") || strings.Contains(ps[0].Content, "!`git status`") {
			t.Fatalf("pins %v %+v", pins, ps)
		}
		if len(calls) != 2 || calls[0].Trigger != "model" || calls[0].Arguments != "2.1" || !strings.Contains(calls[0].Body, "Draft notes for 2.1.") || !strings.Contains(calls[0].Body, "!`git status`") || calls[1].Arguments != "2.2" || !strings.Contains(calls[1].Body, "Draft notes for 2.2.") {
			t.Fatalf("calls %+v", calls)
		}
		acts := h.rec.of("skills.activated")
		if len(acts) != 3 || !acts[1].Data.(*skills.Activated).Deduplicated || acts[2].Data.(*skills.Activated).Deduplicated {
			t.Fatalf("activations %+v", acts)
		}
		h.close()
	})
}

// TestRewriteUser: a command expands $ARGUMENTS first, the rewriter sees the expanded body, and the command input and pin hold its text.
func TestRewriteUser(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var got skills.RewriteInput
		rewrite := func(_ context.Context, in skills.RewriteInput) (string, error) {
			got = in
			return strings.ReplaceAll(in.Body, "!`git status`", "working tree clean"), nil
		}
		files := fstest.MapFS{
			"notes/SKILL.md": &fstest.MapFile{Data: []byte("---\nname: notes\ndescription: Draft notes.\n---\nDraft notes for $ARGUMENTS.\n\n!`git status`\n")},
		}
		h := newHarness(t, [][]providertest.Step{reply("ok")}, kernel.Use(skills.New(skills.Options{Sources: []skills.Source{skills.FS(files)}, Rewrite: rewrite})))
		rc, err := h.sess.Command("notes", "2.1")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := rc.Wait(t.Context()); err != nil {
			t.Fatal(err)
		}
		_, _ = h.sess.WaitQuiet(t.Context())
		req := h.sp.Requests()[0]
		if text := req.Messages[0].Text(); !strings.Contains(text, "Draft notes for 2.1.") || !strings.Contains(text, "working tree clean") || strings.Contains(text, "!`git status`") {
			t.Fatalf("input %q", text)
		}
		if ps := h.sess.Pins(); len(ps) != 1 || !strings.Contains(ps[0].Content, "working tree clean") || strings.Contains(ps[0].Content, "!`git status`") {
			t.Fatalf("pins %+v", ps)
		}
		if got.Trigger != "user" || got.Arguments != "2.1" || !strings.Contains(got.Body, "Draft notes for 2.1.") {
			t.Fatalf("rewrite %+v", got)
		}
		if acts := h.rec.of("skills.activated"); len(acts) != 1 || acts[0].Data.(*skills.Activated).Tokens == 0 {
			t.Fatalf("activations %+v", acts)
		}
		h.close()
	})
}

// TestRewriteDenies: a rewriter error fails the load with the denied text on both paths, without a pin or activation.
func TestRewriteDenies(t *testing.T) {
	deny := func(context.Context, skills.RewriteInput) (string, error) { return "", errors.New("no shell here") }
	files := fstest.MapFS{
		"notes/SKILL.md": &fstest.MapFile{Data: []byte("---\nname: notes\ndescription: Draft notes.\n---\nBody.\n")},
	}
	t.Run("model", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := newHarness(t, [][]providertest.Step{call("toolu_1", `{"name":"notes"}`), reply("ok")},
				kernel.Use(skills.New(skills.Options{Sources: []skills.Source{skills.FS(files)}, Rewrite: deny})))
			h.turn("go")
			texts, pins := h.results()
			if len(texts) != 1 || texts[0] != "Loading skill notes was denied: no shell here" || len(pins) != 0 {
				t.Fatalf("results %q %v", texts, pins)
			}
			if len(h.sess.Pins()) != 0 || len(h.rec.of("skills.activated")) != 0 {
				t.Fatalf("pin %v, activations %d", h.sess.Pins(), len(h.rec.of("skills.activated")))
			}
			h.close()
		})
	})
	t.Run("user", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			h := newHarness(t, [][]providertest.Step{reply("ok")},
				kernel.Use(skills.New(skills.Options{Sources: []skills.Source{skills.FS(files)}, Rewrite: deny})))
			rc, err := h.sess.Command("notes", "")
			if err != nil {
				t.Fatal(err)
			}
			res, err := rc.Wait(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if res.Command == nil || res.Command.Text != "Loading skill notes was denied: no shell here" {
				t.Fatalf("result %+v", res.Command)
			}
			if len(h.sess.Pins()) != 0 || len(h.rec.of("skills.activated")) != 0 {
				t.Fatalf("pin %v, activations %d", h.sess.Pins(), len(h.rec.of("skills.activated")))
			}
			h.close()
		})
	})
}

// TestHostSkillNameWithColon: a host Source's name may carry ":"; it reaches the catalog, the tool's enum and /<name>.
func TestHostSkillNameWithColon(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		src := staticSource{
			list: []skills.Frontmatter{{Name: "plugin:notes", Description: "Draft notes.", UserInvocable: true, ModelInvocable: true, Location: "plugin:notes/SKILL.md"}},
			body: map[string]string{"plugin:notes": "Body of the plugin skill."},
		}
		h := newHarness(t, [][]providertest.Step{reply("ok"), reply("ok")},
			kernel.Use(skills.New(skills.Options{Sources: []skills.Source{src}})))
		h.turn("hi")
		req := h.sp.Requests()[0]
		if sys := system(req); !strings.Contains(sys, "<name>plugin:notes</name>") {
			t.Fatalf("system %q", sys)
		}
		if def := toolOf(req, "skill"); def == nil || !strings.Contains(string(def.InputSchema), `"plugin:notes"`) {
			t.Fatalf("tool %+v", def)
		}
		if !slices.ContainsFunc(h.sess.Commands(), func(c kernel.CommandInfo) bool { return c.Name == "plugin:notes" }) {
			t.Fatalf("commands %+v", h.sess.Commands())
		}
		rc, err := h.sess.Command("plugin:notes", "")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := rc.Wait(t.Context()); err != nil {
			t.Fatal(err)
		}
		_, _ = h.sess.WaitQuiet(t.Context())
		reqs := h.sp.Requests()
		if len(reqs) != 2 || !slices.ContainsFunc(reqs[1].Messages, func(m message.Message) bool {
			return strings.Contains(m.Text(), "Body of the plugin skill.")
		}) {
			t.Fatalf("second request %+v", reqs)
		}
		h.close()
	})
}

// TestFSRejectsColonName: the Agent Skills naming rule still skips a file-system skill whose name holds ":".
func TestFSRejectsColonName(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		files := fstest.MapFS{
			"plugin:notes/SKILL.md": &fstest.MapFile{Data: []byte("---\nname: plugin:notes\ndescription: Hidden.\n---\nBody.\n")},
		}
		h := newHarness(t, [][]providertest.Step{reply("ok")},
			kernel.Use(skills.New(skills.Options{Sources: []skills.Source{skills.FS(files)}})))
		h.turn("hi")
		req := h.sp.Requests()[0]
		if sys := system(req); strings.Contains(sys, "plugin:notes") {
			t.Fatalf("system %q", sys)
		}
		if toolOf(req, "skill") != nil {
			t.Fatal("the skill tool is registered without a model-invocable skill")
		}
		if slices.ContainsFunc(h.sess.Commands(), func(c kernel.CommandInfo) bool { return c.Name == "plugin:notes" }) {
			t.Fatal("the command is registered")
		}
		h.close()
	})
}

// TestArgsWithoutRewrite: args expand the body without a rewriter; the same args load again as already active, and different args reload.
func TestArgsWithoutRewrite(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		files := fstest.MapFS{
			"notes/SKILL.md": &fstest.MapFile{Data: []byte("---\nname: notes\ndescription: Draft notes.\n---\nVersion $ARGUMENTS.\n")},
		}
		h := newHarness(t, [][]providertest.Step{
			call("toolu_1", `{"name":"notes","args":"2.1"}`),
			call("toolu_2", `{"name":"notes","args":"2.1"}`),
			call("toolu_3", `{"name":"notes","args":"2.2"}`),
			reply("ok"),
		}, kernel.Use(skills.New(skills.Options{Sources: []skills.Source{skills.FS(files)}})))
		h.turn("go")
		texts, _ := h.results()
		if len(texts) != 3 || !strings.Contains(texts[0], "Version 2.1.") || !strings.HasPrefix(texts[1], "Skill notes is already active") || !strings.Contains(texts[2], "Version 2.2.") {
			t.Fatalf("results %q", texts)
		}
		if ps := h.sess.Pins(); len(ps) != 1 || !strings.Contains(ps[0].Content, "Version 2.2.") || strings.Contains(ps[0].Content, "2.1") {
			t.Fatalf("pins %+v", ps)
		}
		if acts := h.rec.of("skills.activated"); len(acts) != 3 || !acts[1].Data.(*skills.Activated).Deduplicated {
			t.Fatalf("activations %+v", acts)
		}
		h.close()
	})
}

// TestArgsAbsentLeavesBody: a model call without args leaves $ARGUMENTS in the body, as before args existed.
func TestArgsAbsentLeavesBody(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		files := fstest.MapFS{
			"notes/SKILL.md": &fstest.MapFile{Data: []byte("---\nname: notes\ndescription: Draft notes.\n---\nVersion $ARGUMENTS.\n")},
		}
		h := newHarness(t, [][]providertest.Step{call("toolu_1", `{"name":"notes"}`), reply("ok")},
			kernel.Use(skills.New(skills.Options{Sources: []skills.Source{skills.FS(files)}})))
		h.turn("go")
		texts, _ := h.results()
		if len(texts) != 1 || !strings.Contains(texts[0], "Version $ARGUMENTS.") {
			t.Fatalf("results %q", texts)
		}
		if ps := h.sess.Pins(); len(ps) != 1 || !strings.Contains(ps[0].Content, "$ARGUMENTS") {
			t.Fatalf("pins %+v", ps)
		}
		h.close()
	})
}
