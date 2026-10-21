package kernel

import (
	"slices"
	"strings"
	"unicode"

	"github.com/estonshi/aperture-potatos/internal/prompt"
	"github.com/estonshi/aperture-potatos/internal/tokens"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
)

// mainView is a side chat's main part: its main session's current context, taken once per turn so every step of the turn sends the same prefix (runtime.md §12.8).
type mainView struct {
	lines    []message.Message // the main session's lines after its system line, then the placeholder line when its last call has no result yet
	shared   int               // leading lines that are the main session's own, the placeholder left out
	stable   int               // leading lines within the main session's stable prefix, which a fork of the side chat copies
	seq      int64             // the main session's last line, recorded as ModelCall.ParentSeq
	tokens   int64             // the main session's estimate of these lines
	model    provider.ModelInfo
	system   []provider.SystemBlock
	tools    []message.ToolDef
	hosted   []message.HostedTool
	fallback bool // the main session's epoch renders with ReplayFallback
}

// view is the session's current context as a side chat carries it; committed lines are immutable, so the slice shares their backing array.
func (st *state) view() *mainView {
	v := &mainView{}
	n := len(st.history)
	if n == 0 {
		return v
	}
	v.lines = st.history[1:n:n]
	v.shared, v.stable, v.seq = n-1, max(st.stableLen-1, 0), st.history[n-1].Seq
	v.model, v.system, v.tools, v.hosted = st.model.info, st.system, st.history[0].Tools, st.history[0].HostedTools
	v.fallback = st.replayMode() == provider.ReplayFallback
	if st.stableLen < n {
		v.lines = append(v.lines, pendingLine(st.history[n-1].ToolCalls()))
	}
	v.tokens = st.meter.Scale(tokens.Lines(v.lines)) // the session's own calibration, without the system and tools the side chat sends itself
	return v
}

func (s *Session) view() (*mainView, error) {
	var v *mainView
	err := s.exec(func(st *state) { v = st.view() })
	return v, err
}

// pendingLine answers the calls that have no result yet in the main session, for a side chat's requests only; it is never stored.
func pendingLine(calls []message.ToolCallBlock) message.Message {
	m := message.Message{Role: message.RoleTool, Origin: message.Origin{Kind: message.OriginTool}}
	text := prompt.Text("main_call_pending")
	for _, c := range calls {
		m.Content = append(m.Content, &message.ToolResultBlock{CallID: c.ID, Name: c.Name, Outcome: message.OutcomeNotExecuted, Content: message.Content{&message.TextBlock{Text: text}}})
	}
	return m
}

// messages puts the main part before the side chat's own lines; on a prefix-bound model the reasoning of earlier turns goes, since its signatures were made on another main part.
func (v *mainView) messages(own []message.Message, turnID string, bound, verbatim bool) []message.Message {
	main := v.lines
	if bound {
		if !verbatim {
			main = withoutReasoning(main, "")
		}
		own = withoutReasoning(own, turnID)
	}
	return slices.Concat(main, own)
}

// verbatim says whether the main part's reasoning still holds: the same model, system and tools as the main session's, so the prefix its signatures cover is unchanged.
func (v *mainView) verbatim(model provider.ModelInfo, system []provider.SystemBlock, tools []message.ToolDef, hosted []message.HostedTool) bool {
	if v.model != model || len(v.system) != len(system) {
		return false
	}
	for i := range system {
		if v.system[i].Text != system[i].Text { // a resumed session keeps the text only, which is all the request carries
			return false
		}
	}
	return sameDefs(v.tools, tools) && sameDefs(v.hosted, hosted)
}

// sameDefs compares definitions as the request encodes them, so a copy read back from a file equals the one frozen in memory.
func sameDefs[T any](a, b []T) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b)
	}
	same, _ := sameJSON(a, b)
	return same
}

// withoutReasoning drops the reasoning blocks of the assistant lines outside turnID, copying only the lines it changes.
func withoutReasoning(lines []message.Message, turnID string) []message.Message {
	isReasoning := func(b message.Block) bool { _, ok := b.(*message.ReasoningBlock); return ok }
	out, copied := lines, false
	for i, m := range lines {
		if m.Role != message.RoleAssistant || turnID != "" && m.TurnID == turnID || !slices.ContainsFunc(m.Content, isReasoning) {
			continue
		}
		if !copied {
			out, copied = slices.Clone(lines), true
		}
		out[i].Content = slices.DeleteFunc(slices.Clone(m.Content), isReasoning)
	}
	return out
}

// sideHead puts the side chat note first in an epoch head, so every epoch of a side chat marks where its own lines start.
func (s *Session) sideHead(notes []*message.ReminderBlock) []*message.ReminderBlock {
	if s.parent == nil {
		return notes
	}
	return append([]*message.ReminderBlock{{Source: "side_chat", Text: prompt.Text("side_chat_head")}}, notes...)
}

// takeView gives a side chat's turn its main part and, when the main session moved since the side chat's latest reply, the reminder saying so.
func (t *turn) takeView() (note string, err error) {
	if t.s.parent == nil {
		return "", nil
	}
	v, err := t.s.parent.view()
	if err != nil {
		return "", err
	}
	t.view = v
	_ = t.s.exec(func(st *state) {
		if seq, replied := st.repliedAt(); replied && seq != v.seq { // an unknown seq, after a restart without a calls file, counts as a change
			note = prompt.Textf("side_chat_main_changed", map[string]string{"first": v.firstNew(seq)})
		}
		st.mainTokens, st.mainLines = v.tokens, len(v.lines)
		if st.turn != nil {
			st.turn.view = v
		}
	})
	return note, nil
}

// firstNew names the first main line newer than seq, since the side chat's own earlier replies sit below the main part and would otherwise read as the newer ones; "" when there is no such line to point at (runtime.md §12.8).
func (v *mainView) firstNew(seq int64) string {
	own := v.lines[:v.shared]
	if seq == 0 || len(own) == 0 || own[0].Seq > seq {
		return ""
	}
	if i := slices.IndexFunc(own, func(m message.Message) bool { return m.Seq > seq }); i >= 0 {
		return lineName(own[i])
	}
	return ""
}

// lineName points at a line by what the model sees of it in every API's rendering: a quote of its text, or the tools it calls or answers.
func lineName(m message.Message) string {
	var texts, reminders, tools []string
	for _, b := range m.Content {
		switch b := b.(type) {
		case *message.TextBlock:
			texts = append(texts, b.Text)
		case *message.ReminderBlock:
			reminders = append(reminders, b.Text)
		case *message.ToolCallBlock:
			tools = append(tools, b.Name)
		case *message.ToolResultBlock:
			tools = append(tools, b.Name)
		}
	}
	text := snippet(strings.Join(texts, " "))
	switch m.Role {
	case message.RoleTool:
		return toolsName("the result of ", "the results of ", tools)
	case message.RoleAssistant:
		if text != "" {
			return `the reply "` + text + `"`
		}
		return toolsName("the call to ", "the calls to ", tools)
	}
	if text == "" {
		text = snippet(strings.Join(reminders, " "))
	}
	if text == "" {
		return ""
	}
	return `the message "` + text + `"`
}

func toolsName(one, many string, names []string) string {
	var seen []string
	for _, n := range names {
		if !slices.Contains(seen, n) {
			seen = append(seen, n)
		}
	}
	switch len(seen) {
	case 0:
		return ""
	case 1:
		return one + seen[0]
	}
	return many + strings.Join(seen, ", ")
}

// snippet is text short enough to quote in a reminder, with its white space made single spaces.
func snippet(s string) string {
	r := []rune(strings.Join(strings.Fields(s), " "))
	if len(r) <= 48 {
		return string(r)
	}
	return strings.TrimRightFunc(string(r[:48]), unicode.IsSpace) + "…"
}

// repliedAt is the main session's last line that the side chat's latest reply in history was written against, from the ledger of that reply's turn; 0 when the ledger no longer has it.
func (st *state) repliedAt() (seq int64, replied bool) {
	for i := len(st.history) - 1; i > 0; i-- {
		m := st.history[i]
		if m.Role != message.RoleAssistant {
			continue
		}
		for _, c := range st.ledger.turns[m.TurnID] {
			if c.Bucket == "main" && c.ParentSeq > 0 { // a turn's steps all carry the main part it took at its start
				return c.ParentSeq, true
			}
		}
		return 0, true
	}
	return 0, false
}

// withMain is what a turn's request sends; a side chat's turn puts its main part first.
func (t *turn) withMain(own []message.Message, system []provider.SystemBlock, bound bool) []message.Message {
	if t.view == nil {
		return own
	}
	return t.view.messages(own, t.plan.id, bound, t.view.verbatim(t.model.info, system, t.defs, t.hosted))
}

// orphaned closes a side chat whose main session is gone without closing it first, as after a crash, since no turn of it can run again.
func (s *Session) orphaned() {
	s.post(func(st *state) {
		if st.phase != phaseClosing && st.phase != phaseClosed {
			s.onClose(&closeMsg{reason: "main_closed"})
		}
	})
}

// noteMain weighs the main part before the side chat's first turn, so Context and the estimate count it from the start.
func (s *Session) noteMain() error {
	v, err := s.parent.view()
	if err != nil {
		return err
	}
	return s.exec(func(st *state) {
		st.mainTokens, st.mainLines = v.tokens, len(v.lines)
		st.context.UsedTokens = st.estimate()
		s.publishSnapshot()
	})
}
