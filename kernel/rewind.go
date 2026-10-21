package kernel

import (
	"context"
	"fmt"
	"time"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/message"
)

// TurnInfo is one rewind target: a turn whose first line is in the current epoch after the epoch head.
type TurnInfo struct {
	ID      string          `json:"id"`
	Seq     int64           `json:"seq"`   // first line of the turn
	Input   message.Content `json:"input"` // content of that line, so a UI can offer it for editing
	Started time.Time       `json:"started"`
}

// RewindReport is what a rewind left behind; Input is the dropped turn's input, so a UI can offer it for editing and resending.
type RewindReport struct {
	TurnID  string          `json:"turn_id"`
	Epoch   int             `json:"epoch"`
	Dropped int             `json:"dropped"` // lines left behind
	Input   message.Content `json:"input"`
}

// Turns lists the rewind targets, oldest first.
func (s *Session) Turns() []TurnInfo {
	var out []TurnInfo
	_ = s.exec(func(st *state) {
		if len(st.history) == 0 {
			return
		}
		hist := st.history[1:]
		seen := map[string]bool{}
		for _, m := range hist[headLen(hist):] {
			if m.TurnID == "" || seen[m.TurnID] {
				continue
			}
			seen[m.TurnID] = true
			out = append(out, TurnInfo{ID: m.TurnID, Seq: m.Seq, Input: m.Content, Started: m.CreatedAt})
		}
	})
	return out
}

// headLen counts the leading epoch-head lines of an epoch's lines after its system line.
func headLen(hist []message.Message) int {
	n := 0
	for n < len(hist) && hist[n].Origin.Kind == message.OriginEpochHead {
		n++
	}
	return n
}

// Rewind drops turnID and everything after it from the context; like Compact it runs inline for a history-token holder and queues otherwise.
func (s *Session) Rewind(ctx context.Context, turnID string) (RewindReport, error) {
	var rep RewindReport
	err := s.historyOp(ctx, "rewind", func(ctx context.Context) error {
		r, err := s.rewind(ctx, turnID)
		rep = r
		return err
	})
	return rep, err
}

// rewind opens an epoch whose system line and kept lines are the current ones verbatim, so the cached prefix and prefix-bound signatures still hold (runtime.md §12.7).
func (s *Session) rewind(ctx context.Context, turnID string) (RewindReport, error) {
	var seq uint64
	if m, ok := ctx.Value(markerKey{}).(marker); ok {
		seq = m.seq
	}
	found := false
	_ = s.exec(func(st *state) {
		if len(st.history) > 0 {
			hist := st.history[1:]
			found = turnStart(hist, headLen(hist), turnID) >= 0
		}
	})
	if !found {
		return RewindReport{}, fmt.Errorf("%w: %s", ErrUnknownTurn, turnID)
	}
	s.clearGen.Add(1) // async hooks started in the dropped turns answer a context that no longer exists
	sink := &hookSink{}
	fire(withHookScope(ctx, &hookScope{sink: sink}), s, SessionStart, SessionStartInput{HookEnv: s.env("", 0), Source: "rewind", Model: s.Snapshot().Model}, nil)
	notes := sink.take()
	var rep RewindReport
	var unsynced []message.Message
	if err := s.exec(func(st *state) {
		old := st.history
		cut := turnStart(old[1:], headLen(old[1:]), turnID) + 1
		s.dropQueued("rewound", func(it *queueItem) bool {
			return it.cmd != nil || it.origin.Kind != message.OriginReminder || it.seq > seq && !it.tail
		})
		st.epoch++
		st.history, st.stableLen = nil, 0
		st.executed = map[string]*message.ToolResultBlock{}
		st.meter.Reset()
		sys := &message.Message{Role: message.RoleSystem, Origin: message.Origin{Kind: message.OriginSystem}, Content: old[0].Content, Tools: old[0].Tools, HostedTools: old[0].HostedTools,
			Boundary: &message.Boundary{Reason: "rewind", TurnID: turnID, ReplayFallback: old[0].Boundary != nil && old[0].Boundary.ReplayFallback}} // kept lines that already followed a changed prefix still do
		s.appendLine(st, sys, "")
		s.publishCommitted(st, sys, nil)
		for _, m := range old[1:cut] {
			s.appendCopy(st, m, nil)
		}
		st.pins = rebuildPins(st.history[1:])
		for _, n := range notes { // the epoch head was copied, so this context rides the next turn
			s.enqueue(s.reminderItem(n.Source, n.Text, false))
		}
		rep = RewindReport{TurnID: turnID, Epoch: st.epoch, Dropped: len(old) - cut, Input: old[cut].Content}
		st.context.UsedTokens, st.context.Basis = st.estimate(), "estimated"
		s.publishSnapshot()
		s.publish(&event.ContextRewound{TurnID: turnID, Epoch: rep.Epoch, Dropped: rep.Dropped})
		unsynced = st.unsynced()
	}); err != nil {
		return RewindReport{}, err
	}
	return rep, s.persist(s.ctx, unsynced)
}

// appendCopy appends a copy of m, which keeps its turn and points back at it.
func (s *Session) appendCopy(st *state, m message.Message, le *lineEvents) {
	cp := m
	cp.ID, cp.CopyOf, cp.Batch, cp.BatchEnd = "", m.ID, "", false
	s.appendLine(st, &cp, m.TurnID)
	s.publishCommitted(st, &cp, le)
}
