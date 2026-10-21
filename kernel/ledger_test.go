package kernel_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/providertest"
	"github.com/estonshi/aperture-potatos/store"
	"github.com/estonshi/aperture-potatos/store/jsonl"
)

func TestLedgerKeepsEveryAttempt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		failed := []providertest.Step{{Err: &provider.Error{Class: provider.ClassTransient, Status: 503, RequestID: "req_fail", Message: "busy"}}}
		began := textReply("cut")[:3]
		began = append(began, providertest.Step{Err: &provider.Error{Class: provider.ClassTransient, Message: "reset", MidStream: true}})
		var seen []kernel.RequestInfo
		h := newHarness(t, [][]providertest.Step{failed, began, textReply("ok")},
			kernel.WithRetryPolicy(provider.RetryPolicy{MaxAttempts: 3, BaseDelay: time.Millisecond, MaxDelay: time.Millisecond}),
			kernel.WithRequestHeaders(func(ri kernel.RequestInfo) map[string]string {
				seen = append(seen, ri)
				return map[string]string{"X-Session": ri.RootSessionID}
			}),
			kernel.WithCaptureHeaders("x-gateway-id"))
		r, _ := h.sess.Send(kernel.Input{Text: "hi"})
		res, err := r.Wait(ctx)
		if err != nil || res.FinalText != "ok" {
			t.Fatalf("%v %+v", err, res)
		}
		tc, err := h.sess.TurnCalls(res.TurnID)
		if err != nil {
			t.Fatal(err)
		}
		var outcomes []string
		for _, c := range tc.Calls {
			outcomes = append(outcomes, c.Outcome)
		}
		if got := strings.Join(outcomes, ","); got != "failed,discarded,committed" || tc.Pending {
			t.Fatalf("one entry per HTTP request: %s pending=%v", got, tc.Pending)
		}
		if tc.Calls[0].RequestID != "req_fail" || tc.Calls[2].ResponseID != "resp" || tc.Calls[2].MessageID == "" || tc.Calls[1].MessageID != "" {
			t.Fatalf("ids: %+v", tc.Calls)
		}
		sid := h.sess.Info().SessionID
		if len(seen) != 3 || seen[0].RootSessionID != sid || seen[0].TurnID != res.TurnID || seen[0].Bucket != "main" {
			t.Fatalf("request headers asked once per request: %+v", seen)
		}
		reqs := h.sp.Requests()
		if reqs[2].Params.Headers.Get("X-Session") != sid || strings.Join(reqs[2].CaptureHeaders, ",") != "x-gateway-id" {
			t.Fatalf("request: %+v %v", reqs[2].Params.Headers, reqs[2].CaptureHeaders)
		}
		if _, err := h.sess.TurnCalls("turn_nope"); !errors.Is(err, kernel.ErrTurnNotFound) {
			t.Fatalf("unknown turn: %v", err)
		}
		h.sess.RecordCalls("peer", res.TurnID, []kernel.ModelCall{{SessionID: "ses_other", API: "x", Model: "y", Outcome: "committed", Usage: message.Usage{InputTokens: 5}}})
		synctest.Wait()
		tc, _ = h.sess.TurnCalls(res.TurnID)
		snap := h.sess.Snapshot()
		if len(tc.Calls) != 4 || tc.Calls[3].Bucket != "peer" || snap.Usage.Buckets["peer"].InputTokens != 5 {
			t.Fatalf("RecordCalls books into the turn and the buckets: %+v %+v", tc.Calls, snap.Usage.Buckets)
		}
		h.close()
	})
}

// withResponseID gives a scripted reply its own response id.
func withResponseID(steps []providertest.Step, id string) []providertest.Step {
	steps[0].Event.ResponseID = id
	return steps
}

func TestTurnCostRetriesMissing(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		src := &scriptedCosts{after: map[string]int{"r1": 1, "r2": 3}, errs: map[int]error{2: &kernel.CostError{Status: 503, Message: "later"}}}
		h := newHarness(t, [][]providertest.Step{withResponseID(toolReply(call{"c1", "echo", `{}`}), "r1"), withResponseID(textReply("x"), "r2")},
			kernel.WithCostSource(src), kernel.WithTool(&echoTool{name: "echo"}))
		r, _ := h.sess.Send(kernel.Input{Text: "hi"})
		res, _ := r.Wait(ctx)
		began := time.Now()
		cost, err := h.sess.TurnCost(ctx, res.TurnID)
		if err != nil {
			t.Fatal(err)
		}
		if len(cost.Priced) != 2 || len(cost.Missing) != 0 || cost.Total.Amount != 0.5 || cost.Total.Currency != "USD" {
			t.Fatalf("priced after retries: %+v", cost)
		}
		// round 1 finds r1; round 2 is a 503 and is retried; round 3 finds r2, after waits of 1s and 2s
		if len(src.calls) != 3 || strings.Join(src.calls[1], ",") != "r2" || time.Since(began) != 3*time.Second {
			t.Fatalf("rounds %v after %v", src.calls, time.Since(began))
		}

		src2 := &scriptedCosts{errs: map[int]error{1: &kernel.CostError{Status: 401, Message: "no"}}}
		h2 := newHarness(t, [][]providertest.Step{textReply("y")}, kernel.WithCostSource(src2))
		r2, _ := h2.sess.Send(kernel.Input{Text: "hi"})
		res2, _ := r2.Wait(ctx)
		out, err := h2.sess.TurnCost(ctx, res2.TurnID)
		var ce *kernel.CostError
		if !errors.As(err, &ce) || ce.Status != 401 || len(out.Missing) != 1 || len(src2.calls) != 1 {
			t.Fatalf("a 401 stops at once: %v %+v", err, out)
		}
		short, cancel := context.WithTimeout(ctx, 1500*time.Millisecond)
		src3 := &scriptedCosts{}
		h3 := newHarness(t, [][]providertest.Step{textReply("z")}, kernel.WithCostSource(src3))
		r3, _ := h3.sess.Send(kernel.Input{Text: "hi"})
		res3, _ := r3.Wait(ctx)
		out, err = h3.sess.TurnCost(short, res3.TurnID)
		cancel()
		if err != nil || len(out.Missing) != 1 || out.Missing[0] != "resp" || len(src3.calls) != 2 {
			t.Fatalf("the ctx deadline returns what is still missing: %v %+v rounds %d", err, out, len(src3.calls))
		}
		h.close()
		h2.close()
		h3.close()
		h4 := newHarness(t, nil)
		if _, err := h4.sess.TurnCost(ctx, "turn_x"); !errors.Is(err, kernel.ErrNoCostSource) {
			t.Fatalf("no source: %v", err)
		}
		h4.close()
	})
}

func TestLedgerKeepsTheLatestTurns(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		scripts := make([][]providertest.Step, 101)
		for i := range scripts {
			scripts[i] = textReply("ok")
		}
		lines := linesOnly{store.NewMemory()}
		h := newHarness(t, scripts, kernel.WithStore(lines))
		var first, last string
		for i := range scripts {
			r, _ := h.sess.Send(kernel.Input{Text: "hi"})
			res, err := r.Wait(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if i == 0 {
				first = res.TurnID
			}
			last = res.TurnID
		}
		if _, err := h.sess.TurnCalls(first); !errors.Is(err, kernel.ErrTurnNotFound) {
			t.Fatalf("the 101st turn evicts the first from memory, and a store without calls cannot give it back: %v", err)
		}
		if tc, err := h.sess.TurnCalls(last); err != nil || len(tc.Calls) != 1 {
			t.Fatalf("latest turn: %v %+v", err, tc)
		}
		if got := h.sess.Snapshot().Usage.Buckets["main"].InputTokens; got != 101*usage.InputTokens {
			t.Fatalf("eviction never changes the totals: %d", got)
		}
		h.close()
	})
}

// linesOnly hides a store's call ledger, as a store that keeps only lines would.
type linesOnly struct{ store.SessionStore }

func TestLedgerSurvivesRestart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		dir := t.TempDir()
		failed := []providertest.Step{{Err: &provider.Error{Class: provider.ClassTransient, Status: 503, Message: "busy"}}}
		first := resumeAgent(t, jsonl.New(dir), failed, textReply("one"), textReply("two"))
		sess, err := first.NewSession(ctx)
		if err != nil {
			t.Fatal(err)
		}
		t1 := ask(t, sess, "first")
		ask(t, sess, "second")
		before, _ := sess.TurnCalls(t1.TurnID)
		usage := sess.Snapshot().Usage
		id := sess.Info().SessionID
		_ = first.Close(ctx)

		second := resumeAgent(t, jsonl.New(dir), textReply("three"))
		back, err := second.Resume(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		after, err := back.TurnCalls(t1.TurnID)
		if err != nil || messagesJSONAny(t, after) != messagesJSONAny(t, before) {
			t.Fatalf("the turn's calls come back as they were:\n%s\n%s (%v)", messagesJSONAny(t, before), messagesJSONAny(t, after), err)
		}
		if got := back.Snapshot().Usage; messagesJSONAny(t, got) != messagesJSONAny(t, usage) || got.Retries != 1 || got.Turns != 2 || got.Steps != 2 {
			t.Fatalf("the usage totals are rebuilt:\n%s\n%s", messagesJSONAny(t, usage), messagesJSONAny(t, got))
		}
		t3 := ask(t, back, "third")
		tc, _ := back.TurnCalls(t3.TurnID) // sequential test ids restart with the process, so this turn shares the first one's id
		if c := tc.Calls[len(tc.Calls)-1]; c.Seq != 4 || c.Attempt != 1 || c.Step != 1 || c.Outcome != "committed" {
			t.Fatalf("the ledger goes on from the stored seq: %+v", c)
		}
		_ = second.Close(ctx)
		synctest.Wait()
		recs, _ := jsonl.New(dir).(store.CallStore).LoadCalls(ctx, id)
		if len(recs) != 4 || !strings.Contains(string(recs[0]), `"outcome":"failed"`) {
			t.Fatalf("calls file: %s", recs)
		}
	})
}

// scriptedCosts prices a call from the response id once its round has come, and can fail a round.
type scriptedCosts struct {
	after map[string]int // response id -> the round it appears in
	errs  map[int]error  // round -> error
	round int
	calls [][]string
}

func (c *scriptedCosts) Price(ctx context.Context, calls []kernel.ModelCall) (map[int]kernel.Cost, error) {
	c.round++
	var ids []string
	for _, x := range calls {
		ids = append(ids, x.ResponseID)
	}
	c.calls = append(c.calls, ids)
	if err := c.errs[c.round]; err != nil {
		return nil, err
	}
	out := map[int]kernel.Cost{}
	for i, x := range calls {
		if r, ok := c.after[x.ResponseID]; ok && c.round >= r {
			out[i] = kernel.Cost{Amount: 0.25, Currency: "USD"}
		}
	}
	return out, nil
}

// TestSideCallsBookedAndTagged: a side call made inside a turn is booked in that turn's ledger, and it and a ChildOf session's requests carry the request headers with the root session.
func TestSideCallsBookedAndTagged(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		var mu sync.Mutex
		var seen []kernel.RequestInfo
		aside := kernel.NewTool(kernel.ToolSpec{Name: "aside", Description: "Ask aside.", InputSchema: json.RawMessage(`{"type":"object"}`), Concurrency: kernel.Serial},
			func(ctx context.Context, tc *kernel.ToolContext, _ struct{}) (kernel.ToolResult, error) {
				m, _, err := tc.Session().SideCall(ctx, kernel.SideRequest{Messages: []message.Message{{Role: message.RoleUser, Content: message.Content{&message.TextBlock{Text: "aside"}}}}})
				if err != nil {
					return kernel.ToolResult{}, err
				}
				return kernel.TextResult(m.Text()), nil
			})
		var ag *kernel.Agent
		child := make(chan *kernel.Session, 1)
		k := childKind(func(ctx context.Context, rt kernel.TaskRuntime) (*kernel.Session, error) {
			c, err := ag.NewSession(ctx, kernel.ChildOf(rt))
			if err != nil {
				return nil, err
			}
			rc, _ := c.Send(kernel.Text("child"))
			_, err = rc.Wait(ctx)
			return c, err
		}, child)
		h := newHarness(t, [][]providertest.Step{toolReply(call{"c1", "aside", `{}`}), textReply("side"), textReply("done"), textReply("from child")}, kernel.WithTool(aside), kernel.WithTaskKind(k), quiet,
			kernel.WithRequestHeaders(func(ri kernel.RequestInfo) map[string]string {
				mu.Lock()
				seen = append(seen, ri)
				mu.Unlock()
				return map[string]string{"X-Root": ri.RootSessionID}
			}))
		ag = h.ag
		res := turnOnce(t, h.sess, "go")
		tc, _ := h.sess.TurnCalls(res.TurnID)
		var buckets []string
		for _, c := range tc.Calls {
			buckets = append(buckets, c.Bucket)
		}
		if strings.Join(buckets, ",") != "main,side,main" {
			t.Fatalf("the turn's calls: %v", buckets)
		}
		hostStart(t, h.sess, "child")
		<-child
		root := h.sess.Info().SessionID
		reqs := h.sp.Requests()
		if len(reqs) != 4 || len(seen) != 4 || seen[1].Bucket != "side" || seen[1].TurnID != res.TurnID || seen[3].SessionID == root || seen[3].RootSessionID != root {
			t.Fatalf("%d requests, seen %+v", len(reqs), seen)
		}
		for i, r := range reqs {
			if r.Params.Headers.Get("X-Root") != root {
				t.Fatalf("request %d: %v", i, r.Params.Headers)
			}
		}
		h.close()
	})
}
