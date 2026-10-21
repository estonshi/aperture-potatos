package kernel_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"testing/synctest"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/providertest"
)

func TestSystemSectionPrecedence(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		sp := providertest.NewScriptProvider(provider.APIAnthropicMessages)
		ag, err := kernel.New(kernel.WithModel(provider.ModelRef{Provider: sp, Model: "m"}),
			kernel.WithNickname("Tater"), kernel.WithDescription("Plans trips."), kernel.WithInstructions("static"),
			kernel.WithInstructionsFunc(func(_ context.Context, info kernel.SessionInfo) (string, error) {
				return "for " + info.Labels["user"], nil
			}))
		if err != nil {
			t.Fatal(err)
		}
		systemOf := func(opts ...kernel.SessionOption) []string {
			var texts []string
			s, err := ag.NewSession(t.Context(), append(opts, kernel.WithSessionFunc(func(_ context.Context, ev event.Event) error {
				if d, ok := ev.Data.(*event.MessageCompleted); ok && d.Message.Role == message.RoleSystem {
					for _, b := range d.Message.Content {
						texts = append(texts, b.(*message.TextBlock).Text)
					}
					if d.Message.Boundary.Session.Labels["user"] == "" && len(opts) > 0 {
						t.Error("labels missing from the first line")
					}
				}
				return nil
			}))...)
			if err != nil {
				t.Fatal(err)
			}
			synctest.Wait()
			_ = s.Close(t.Context())
			return texts
		}
		fromFunc := systemOf(kernel.WithLabels(map[string]string{"user": "u_42"}))
		if fromFunc[1] != "<identity>\nYour name is Tater.\nPlans trips.\n</identity>" || !strings.HasSuffix(fromFunc[2], "for u_42") {
			t.Fatalf("system %q", fromFunc)
		}
		if got := systemOf(kernel.SessionInstructions("session wins"), kernel.WithLabels(map[string]string{"user": "x"})); !strings.HasSuffix(got[2], "session wins") {
			t.Fatalf("session instructions: %q", got[2])
		}
		if err := ag.SetNickname("Tudou"); err != nil || ag.Nickname() != "Tudou" {
			t.Fatal("rename")
		}
		if got := systemOf(kernel.WithLabels(map[string]string{"user": "y"})); !strings.Contains(got[1], "Tudou") {
			t.Fatalf("a new session uses the new name: %q", got[1])
		}
		if err := ag.SetNickname(""); !errors.Is(err, kernel.ErrInvalidName) {
			t.Fatal(err)
		}
		ag.Close(t.Context())
	})
}

func TestPromptSections(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		section := func(name string, slot kernel.Slot, order int, text string, err error) kernel.Option {
			return kernel.WithPromptSection(kernel.PromptSection{Name: name, Slot: slot, Order: order, Render: func(_ context.Context, rc kernel.RenderContext) (string, error) {
				if len(rc.Tools) != 1 || rc.Session == nil || rc.Epoch != 1 {
					return "", errors.New("bad render context")
				}
				return text, err
			}})
		}
		h := newHarness(t, nil, kernel.WithTool(&echoTool{name: "echo"}),
			section("env", kernel.SlotEnvironment, 0, "ENV", nil),
			section("b", kernel.SlotCapabilities, 1, "CAP-B", nil),
			section("a", kernel.SlotCapabilities, 1, "CAP-A", nil),
			section("first", kernel.SlotCapabilities, 0, "CAP-FIRST", nil),
			section("cat", kernel.SlotCatalog, 0, "", nil), // empty: left out
			section("broken", kernel.SlotCatalog, 1, "x", errors.New("no catalog")))
		var texts []string
		for _, b := range h.lines()[0].Content {
			texts = append(texts, b.(*message.TextBlock).Text)
		}
		got := strings.Join(texts[2:], "|") // after core and identity
		if got != "CAP-FIRST|CAP-A|CAP-B|ENV" {
			t.Fatalf("sections %q", got)
		}
		synctest.Wait() // events reach the recorder through its own pump, which may still hold the error when the line is read
		if !hasEvent(h.rec, event.TypeError, func(d event.Payload) bool { return d.(*event.Error).Code == "section_render_failed" }) {
			t.Fatal("a failing section is not reported")
		}
		for _, bad := range []kernel.PromptSection{
			{Name: "", Slot: kernel.SlotCatalog, Render: func(context.Context, kernel.RenderContext) (string, error) { return "", nil }},
			{Name: "x", Slot: 9, Render: func(context.Context, kernel.RenderContext) (string, error) { return "", nil }},
			{Name: "x", Slot: kernel.SlotCatalog},
		} {
			if _, err := h.sess.Registrar().PromptSection(bad); err == nil {
				t.Fatalf("accepted %+v", bad)
			}
		}
		h.close()
	})
}

func TestContextProviderFailure(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, [][]providertest.Step{textReply("ok")},
			kernel.WithContextProvider(kernel.ContextProvider{Name: "flaky", When: kernel.AtTurnStart, Provide: func(context.Context, kernel.ProvideContext) (string, error) { panic("boom") }}),
			kernel.WithContextProvider(kernel.ContextProvider{Name: "time", When: kernel.AtTurnStart, Provide: func(_ context.Context, pc kernel.ProvideContext) (string, error) {
				return "Inputs: " + pc.Inputs[0].Text, nil
			}}))
		rc, _ := h.sess.Send(kernel.Text("hi"))
		if res, _ := rc.Wait(t.Context()); res.Outcome != kernel.OutcomeCompleted {
			t.Fatalf("outcome %s", res.Outcome)
		}
		user := h.lines()[1]
		if r, ok := user.Content[0].(*message.ReminderBlock); !ok || r.Source != "time" || r.Text != "Inputs: hi" || len(user.Content) != 2 {
			t.Fatalf("delivery %+v", user.Content)
		}
		if !hasEvent(h.rec, event.TypeError, func(d event.Payload) bool { return d.(*event.Error).Code == "context_provider_failed" }) {
			t.Fatal("a failing provider is not reported")
		}
		if _, err := h.sess.Registrar().ContextProvider(kernel.ContextProvider{Name: "x", When: 7, Provide: func(context.Context, kernel.ProvideContext) (string, error) { return "", nil }}); err == nil {
			t.Fatal("accepted an unknown When")
		}
		h.close()
	})
}

// TestHideAnnouncesSection: hiding an announced agent section inside an epoch tells the model it is gone, and undoing the hide brings its text back.
func TestHideAnnouncesSection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, [][]providertest.Step{textReply("a"), textReply("b")},
			kernel.WithPromptSection(kernel.PromptSection{Name: "rules", Slot: kernel.SlotCatalog, AnnounceChanges: true,
				Render: func(context.Context, kernel.RenderContext) (string, error) { return "Be kind.", nil }}))
		hide, err := h.sess.Registrar().Hide(kernel.KindPromptSection, "rules")
		if err != nil {
			t.Fatal(err)
		}
		rc, _ := h.sess.Send(kernel.Text("one"))
		_, _ = rc.Wait(t.Context())
		hide.Remove()
		synctest.Wait()
		rc, _ = h.sess.Send(kernel.Text("two"))
		_, _ = rc.Wait(t.Context())
		var got []string
		for _, m := range h.lines() {
			for _, b := range m.Content {
				if r, ok := b.(*message.ReminderBlock); ok && r.Source == "registry" {
					got = append(got, r.Text)
				}
			}
		}
		if len(got) != 2 || !strings.Contains(got[0], `"rules" was removed`) || !strings.Contains(got[1], "Be kind.") {
			t.Fatalf("announcements %q", got)
		}
		h.close()
	})
}

type noted struct{ N int }

func (noted) EventType() event.Type { return "test.noted" }

// TestAgentAnnounce: an announcement starts no turn, rides with the next one as a reminder of the caller's source, and its event is published once per session.
func TestAgentAnnounce(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, [][]providertest.Step{textReply("a")})
		h.ag.Announce("ext:notes", "Two new notes.", noted{N: 2})
		synctest.Wait()
		if n := len(h.sp.Requests()); n != 0 {
			t.Fatalf("%d requests before any input", n)
		}
		rc, _ := h.sess.Send(kernel.Text("one"))
		_, _ = rc.Wait(t.Context())
		var got []string
		for _, m := range h.lines() {
			for _, b := range m.Content {
				if r, ok := b.(*message.ReminderBlock); ok && r.Source == "ext:notes" {
					got = append(got, r.Text)
				}
			}
		}
		if len(got) != 1 || got[0] != "Two new notes." || h.rec.count("test.noted") != 1 {
			t.Fatalf("announcements %q, %d events", got, h.rec.count("test.noted"))
		}
		h.close()
	})
}

// TestAgentSessionCut: with a catalog section after them, core through user carry the agent/session breakpoint only while every session of the agent renders them alike.
func TestAgentSessionCut(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		catalog := kernel.WithPromptSection(kernel.PromptSection{Name: "agents", Slot: kernel.SlotCatalog,
			Render: func(context.Context, kernel.RenderContext) (string, error) { return "CATALOG", nil }})
		h := newHarness(t, [][]providertest.Step{textReply("a"), textReply("b"), textReply("c"), textReply("d")}, kernel.WithInstructions("Be brief."), catalog)
		marks := func(s *kernel.Session) []bool {
			t.Helper()
			rc, _ := s.Send(kernel.Text("hi"))
			_, _ = rc.Wait(t.Context())
			reqs := h.sp.Requests()
			var out []bool
			for _, b := range reqs[len(reqs)-1].System {
				out = append(out, b.Cache)
			}
			return out
		}
		if got := marks(h.sess); !slices.Equal(got, []bool{false, false, true, false}) {
			t.Fatalf("plain session: %v, want the user block marked", got)
		}
		own, _ := h.ag.NewSession(t.Context(), kernel.SessionInstructions("Mine."))
		if got := marks(own); slices.Contains(got, true) {
			t.Fatalf("session instructions: %v, want no mark", got)
		}
		_, _ = h.sess.Registrar().PromptSection(kernel.PromptSection{Name: "local", Slot: kernel.SlotCapabilities,
			Render: func(context.Context, kernel.RenderContext) (string, error) { return "LOCAL", nil }})
		if err := h.sess.Clear(t.Context()); err != nil {
			t.Fatal(err)
		}
		if got := marks(h.sess); slices.Contains(got, true) {
			t.Fatalf("session capabilities section: %v, want no mark", got)
		}
		h.close()
	})
}

// TestRenderEpoch: a section and the instructions func see the epoch they render for.
func TestRenderEpoch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var sections, funcs []int
		h := newHarness(t, nil,
			kernel.WithPromptSection(kernel.PromptSection{Name: "e", Slot: kernel.SlotEnvironment, Render: func(_ context.Context, rc kernel.RenderContext) (string, error) {
				sections = append(sections, rc.Epoch)
				return "E", nil
			}}),
			kernel.WithInstructionsFunc(func(_ context.Context, info kernel.SessionInfo) (string, error) {
				funcs = append(funcs, info.Epoch)
				return "I", nil
			}))
		for range 2 {
			if err := h.sess.Clear(t.Context()); err != nil {
				t.Fatal(err)
			}
		}
		if !slices.Equal(sections, []int{1, 2, 3}) || !slices.Equal(funcs, []int{1, 2, 3}) {
			t.Fatalf("sections %v, instructions %v", sections, funcs)
		}
		h.close()
	})
}

// TestRenameAtNextEpoch: an open session keeps the agent's old name until its next epoch renders the new one.
func TestRenameAtNextEpoch(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		h := newHarness(t, nil)
		if err := h.ag.SetNickname("Tudou"); err != nil {
			t.Fatal(err)
		}
		if got := h.sess.Info().Agent; got != "Tater" {
			t.Fatalf("before the next epoch: %s", got)
		}
		if err := h.sess.Clear(t.Context()); err != nil {
			t.Fatal(err)
		}
		lines := h.lines()
		sys := lines[len(lines)-1]
		if got := h.sess.Info().Agent; got != "Tudou" || sys.Boundary == nil || !strings.Contains(sys.Content[1].(*message.TextBlock).Text, "Tudou") {
			t.Fatalf("after clear: %s, system %+v", got, sys)
		}
		h.close()
	})
}
