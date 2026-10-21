package kernel

import (
	"context"
	"encoding/json"
	"slices"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/store"
)

// callStore is the configured store's call ledger, nil when it keeps none.
func (s *Session) callStore() store.CallStore {
	cs, _ := s.agent.cfg.store.(store.CallStore)
	return cs
}

// queueCall hands a booked call to the calls writer, which starts when none runs, so booking never waits on the store.
func (s *Session) queueCall(st *state, mc ModelCall) {
	if s.callStore() == nil {
		return
	}
	st.calls = append(st.calls, mc)
	if !st.writing && st.phase != phaseClosed {
		st.writing = true
		s.wk.Go("calls", s.writeCalls)
	}
}

// writeCalls appends the pending calls until none is left; a failed write keeps them for the next booked call and only warns, since the calls already happened.
func (s *Session) writeCalls() {
	cs := s.callStore()
	for {
		var batch []ModelCall
		if s.exec(func(st *state) {
			batch = slices.Clone(st.calls)
			st.writing = len(batch) > 0
		}) != nil || len(batch) == 0 {
			return
		}
		recs := make([]json.RawMessage, len(batch))
		for i, c := range batch {
			recs[i], _ = json.Marshal(c) // a ModelCall always encodes
		}
		err := cs.AppendCalls(s.ctx, s.id, recs)
		last := batch[len(batch)-1].Seq
		_ = s.exec(func(st *state) {
			if err != nil {
				st.writing = false
				s.publish(&event.Error{Code: "store_failed", Message: "calls: " + err.Error()})
				return
			}
			st.calls = slices.DeleteFunc(st.calls, func(c ModelCall) bool { return c.Seq <= last })
		})
		if err != nil {
			return
		}
	}
}

// loadCalls reads this session's stored calls; records that no longer decode are skipped.
func (s *Session) loadCalls(ctx context.Context) ([]ModelCall, error) {
	cs := s.callStore()
	if cs == nil {
		return nil, nil
	}
	recs, err := cs.LoadCalls(ctx, s.id)
	if err != nil {
		return nil, err
	}
	out := make([]ModelCall, 0, len(recs))
	for _, r := range recs {
		var mc ModelCall
		if json.Unmarshal(r, &mc) == nil {
			out = append(out, mc)
		}
	}
	return out, nil
}

// storedTurnCalls finds a turn in the calls file.
func (s *Session) storedTurnCalls(turnID string) (TurnCalls, error) {
	calls, err := s.loadCalls(s.ctx)
	if err != nil {
		return TurnCalls{}, err
	}
	calls = slices.DeleteFunc(calls, func(c ModelCall) bool { return c.TurnID != turnID })
	if turnID == "" || len(calls) == 0 {
		return TurnCalls{}, ErrTurnNotFound
	}
	return TurnCalls{TurnID: turnID, Calls: calls}, nil
}

// restoreCalls rebuilds the ledger and the usage totals of a resumed session from its calls file and its lines: turns are counted from the original lines, retries from calls past their first attempt.
func (st *state) restoreCalls(self string, calls []ModelCall, lines []message.Message) {
	turns := map[string]bool{}
	for _, m := range lines {
		if m.TurnID != "" && m.CopyOf == "" {
			turns[m.TurnID] = true
		}
	}
	st.usage.Turns = len(turns)
	for _, mc := range calls {
		st.usage.tally(mc, self)
		if mc.Bucket == "main" && mc.SessionID == self && mc.Attempt > 1 {
			st.usage.Retries++
		}
		st.ledger.add(mc)
		st.callSeq = max(st.callSeq, mc.Seq)
	}
}
