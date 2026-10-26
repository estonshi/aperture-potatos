package kernel

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/internal/prompt"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/store"
)

// appendLine stamps m with its line identity and appends it; the stable prefix and the executed-call ledger follow.
func (s *Session) appendLine(st *state, m *message.Message, turnID string) {
	st.lineSeq++
	m.V = message.Version
	if m.ID == "" {
		m.ID = s.agent.cfg.ids.NewID("msg")
	}
	m.SessionID, m.Seq, m.Epoch, m.TurnID = s.id, st.lineSeq, st.epoch, turnID
	m.CreatedAt = s.agent.cfg.clock.Now()
	st.history = append(st.history, *m)
	st.pending = append(st.pending, *m)
	if !(m.Role == message.RoleAssistant && len(m.ToolCalls()) > 0) {
		st.stableLen = len(st.history) // an assistant call holds the prefix back until its tool line pairs it
	}
	if m.Role == message.RoleTool {
		for _, b := range m.Content {
			if r, ok := b.(*message.ToolResultBlock); ok {
				st.executed[r.CallID] = r
				if r.Pin != nil {
					st.addPin(*r.Pin)
				}
			}
		}
	}
}

// unsynced hands the committed lines the store does not have yet to the history-token holder, who persists them off the actor.
func (st *state) unsynced() []message.Message {
	return append([]message.Message(nil), st.pending...)
}

// persist writes lines as one Append, with batch markers when there is more than one, and keeps them pending on failure so the next commit retries.
func (s *Session) persist(ctx context.Context, lines []message.Message) error {
	if len(lines) == 0 {
		return nil
	}
	if len(lines) > 1 {
		batch := s.agent.cfg.ids.NewID("b")
		for i := range lines {
			lines[i].Batch = batch
		}
		lines[len(lines)-1].BatchEnd = true
	}
	err := s.agent.cfg.store.Append(ctx, s.id, lines)
	s.dropSides(lines) // after the append, so a crash between the two leaves the side chat for SideChat to mark
	if err == nil {
		s.stored.extend(lines)
		last := lines[len(lines)-1].Seq
		_ = s.exec(func(st *state) {
			for len(st.pending) > 0 && st.pending[0].Seq <= last {
				st.pending = st.pending[1:]
			}
		})
		return nil
	}
	switch {
	case errors.Is(err, store.ErrSeqConflict):
		s.fatalStore(err)
		return err
	case ctx.Err() != nil:
		return nil // interrupted mid-write: the lines stay pending and the turn's exit writes them
	}
	s.post(func(*state) { s.publish(&event.Error{Code: "store_failed", Message: err.Error()}) })
	if s.agent.cfg.storePolicy == StoreStrict {
		return err
	}
	return nil
}

// lineEvents says how a committed line is announced beyond message.completed.
type lineEvents struct {
	step, attempt int
	delivery      *deliveryInfo // a user line that delivered queue items
	safePoint     bool          // the delivery happened at a safe point, which input.injected reports
	turnID        string        // the turn publishing a line that is not its own, such as a carried-over copy
}

// publishCommitted announces a committed line: message.completed, then context.injected for reminders and input.injected for items a safe point delivered.
func (s *Session) publishCommitted(st *state, m *message.Message, le *lineEvents) {
	var step, attempt int
	turnID := m.TurnID
	if le != nil {
		step, attempt = le.step, le.attempt
		if le.turnID != "" {
			turnID = le.turnID
		}
	}
	pub := s.envelope(&event.MessageCompleted{Message: publicMessage(*m), Partial: m.Partial}, turnID, step, attempt)
	nat := *pub
	nat.Data = &event.MessageCompleted{Message: *m, Partial: m.Partial}
	s.bus.PublishNative(pub, &nat)
	if le == nil || le.delivery == nil {
		return
	}
	if d := le.delivery; d.reminders > 0 {
		s.publishAt(&event.ContextInjected{MessageID: m.ID, Sources: d.sources, Count: d.reminders, Truncated: d.truncated}, m.TurnID, step, 0)
	}
	if le.safePoint {
		s.publishAt(&event.InputInjected{InputIDs: le.delivery.ids, TurnID: m.TurnID, Step: step}, m.TurnID, step, 0)
	}
}

// publicMessage is the event view of a line: provider wire data is left out, and an opaque block keeps only its API.
func publicMessage(m message.Message) message.Message {
	m.Native = nil
	content := make(message.Content, len(m.Content))
	for i, b := range m.Content {
		content[i] = publicBlock(b)
	}
	m.Content = content
	return m
}

var emptyRaw = json.RawMessage(`{}`)

func publicBlock(b message.Block) message.Block {
	switch v := b.(type) {
	case *message.TextBlock:
		if v.Native != nil {
			c := *v
			c.Native = nil
			return &c
		}
	case *message.RefusalBlock:
		if v.Native != nil {
			c := *v
			c.Native = nil
			return &c
		}
	case *message.ReasoningBlock:
		if v.Native != nil {
			c := *v
			c.Native = nil
			return &c
		}
	case *message.ToolCallBlock:
		if v.Native != nil {
			c := *v
			c.Native = nil
			return &c
		}
	case *message.OpaqueBlock:
		if v.Native != nil {
			c := *v
			c.Native = &message.Native{API: v.Native.API, Raw: emptyRaw}
			return &c
		}
	case *message.HostedCallBlock:
		if v.Native != nil {
			c := *v
			c.Native = nil
			return &c
		}
	case *message.HostedResultBlock:
		if v.Native != nil {
			c := *v
			c.Native = nil
			return &c
		}
	}
	return b
}

// repairTail pairs every call of the last assistant line that has no result with a not_executed result, so history stays valid for the API on every exit path (I1).
func (st *state) repairTail(outcome Outcome, texts prompt.Source) *message.Message {
	if len(st.history) == 0 {
		return nil
	}
	last := st.history[len(st.history)-1]
	calls := last.ToolCalls()
	if last.Role != message.RoleAssistant || len(calls) == 0 {
		return nil
	}
	text := prompt.TextfFrom(texts, "turn_ended_not_executed", map[string]string{"outcome": string(outcome)})
	tool := &message.Message{Role: message.RoleTool, Origin: message.Origin{Kind: message.OriginTool}}
	for _, c := range calls {
		tool.Content = append(tool.Content, notExecuted(c, text))
	}
	return tool
}

func notExecuted(c message.ToolCallBlock, text string) *message.ToolResultBlock {
	return &message.ToolResultBlock{CallID: c.ID, Name: c.Name, IsError: true, Outcome: message.OutcomeNotExecuted, Content: message.Content{&message.TextBlock{Text: text}}}
}
