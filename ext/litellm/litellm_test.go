package litellm_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/estonshi/aperture-potatos/ext/litellm"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/providertest"
)

// proxy is a fake LiteLLM that serves /spend/logs/v2 from rows by session and /spend/logs from rows by request id.
type proxy struct {
	mu       sync.Mutex
	sessions map[string][]map[string]any
	requests map[string]float64
	status   int
	seen     []*http.Request
}

func (p *proxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.seen = append(p.seen, r)
	if p.status != 0 {
		http.Error(w, "nope", p.status)
		return
	}
	switch r.URL.Path {
	case "/spend/logs/v2":
		rows := p.sessions[r.URL.Query().Get("session_id")]
		page := r.URL.Query().Get("page")
		half := (len(rows) + 1) / 2 // two pages, so paging is exercised
		if page == "1" {
			rows = rows[:half]
		} else {
			rows = rows[half:]
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": rows, "total_pages": 2})
	case "/spend/logs":
		id := r.URL.Query().Get("request_id")
		var out []map[string]any
		if v, ok := p.requests[id]; ok {
			out = append(out, map[string]any{"request_id": id, "spend": v})
		}
		_ = json.NewEncoder(w).Encode(out)
	default:
		http.NotFound(w, r)
	}
}

func (p *proxy) paths() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for _, r := range p.seen {
		out = append(out, r.URL.Path)
	}
	return out
}

func TestPriceBySessionThenByRequest(t *testing.T) {
	started := time.Date(2026, 9, 29, 8, 30, 0, 0, time.FixedZone("JST", 9*3600))
	p := &proxy{
		sessions: map[string][]map[string]any{"ses_a": {
			{"request_id": "r1", "spend": 0.25},
			{"request_id": "someone_else", "spend": 9.0}, // session_id matches loosely
			{"request_id": "r2", "spend": 0.5},
		}},
		requests: map[string]float64{"r3": 0.125},
	}
	srv := httptest.NewServer(p)
	defer srv.Close()
	src := litellm.CostSource(litellm.Options{BaseURL: srv.URL + "/", APIKey: "k", SessionHeader: "x-litellm-session-id"})
	calls := []kernel.ModelCall{
		{RootSession: "ses_a", ResponseID: "r1", StartedAt: started.Add(time.Minute)},
		{RootSession: "ses_a", ResponseID: "r2", StartedAt: started},
		{RootSession: "ses_b", ResponseID: "r3", StartedAt: started}, // another tree, found only one request at a time
		{RootSession: "ses_a", ResponseID: "r4", StartedAt: started}, // not logged yet
	}
	got, err := src.Price(context.Background(), calls)
	if err != nil {
		t.Fatal(err)
	}
	want := map[int]kernel.Cost{0: {Amount: 0.25, Currency: "USD"}, 1: {Amount: 0.5, Currency: "USD"}, 2: {Amount: 0.125, Currency: "USD"}}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] || got[2] != want[2] {
		t.Fatalf("got %+v", got)
	}
	var v2 []*http.Request
	for _, r := range p.seen {
		if r.URL.Path == "/spend/logs/v2" {
			v2 = append(v2, r)
		}
	}
	if len(v2) != 4 { // two sessions, two pages each
		t.Fatalf("v2 queries: %v", p.paths())
	}
	q := v2[0].URL.Query()
	if q.Get("session_id") != "ses_a" || q.Get("start_date") != "2026-09-28 23:29:00" || q.Get("page_size") != "100" || q.Get("page") != "1" || v2[0].Header.Get("Authorization") != "Bearer k" {
		t.Fatalf("query %v, auth %q", q, v2[0].Header.Get("Authorization"))
	}
	if _, err := time.Parse("2006-01-02 15:04:05", q.Get("end_date")); err != nil {
		t.Fatalf("end_date %q", q.Get("end_date"))
	}
	var one []string
	for _, r := range p.seen {
		if r.URL.Path == "/spend/logs" {
			one = append(one, r.URL.Query().Get("request_id"))
		}
	}
	slices.Sort(one)
	if !slices.Equal(one, []string{"r3", "r4"}) {
		t.Fatalf("what the session query missed is looked up one request at a time: %v", one)
	}
}

func TestWithoutSessionHeaderOnlyByRequest(t *testing.T) {
	p := &proxy{requests: map[string]float64{"r1": 1}}
	srv := httptest.NewServer(p)
	defer srv.Close()
	src := litellm.CostSource(litellm.Options{BaseURL: srv.URL, APIKey: "k"})
	got, err := src.Price(context.Background(), []kernel.ModelCall{{RootSession: "ses_a", ResponseID: "r1"}, {RootSession: "ses_a"}})
	if err != nil || len(got) != 1 || got[0].Amount != 1 {
		t.Fatalf("%v %+v", err, got)
	}
	if paths := p.paths(); !slices.Equal(paths, []string{"/spend/logs"}) {
		t.Fatalf("paths %v", paths)
	}
	if h := litellm.RequestHeaders(litellm.Options{})(kernel.RequestInfo{RootSessionID: "ses_a"}); h != nil {
		t.Fatalf("no header without SessionHeader: %v", h)
	}
}

func TestStatusIsACostError(t *testing.T) {
	p := &proxy{status: http.StatusServiceUnavailable}
	srv := httptest.NewServer(p)
	defer srv.Close()
	src := litellm.CostSource(litellm.Options{BaseURL: srv.URL, SessionHeader: "x-litellm-session-id"})
	_, err := src.Price(context.Background(), []kernel.ModelCall{{RootSession: "ses_a", ResponseID: "r1"}})
	var ce *kernel.CostError
	if !errors.As(err, &ce) || ce.Status != http.StatusServiceUnavailable || !strings.Contains(ce.Message, "nope") {
		t.Fatalf("%v", err)
	}
}

func TestTurnCostThroughTheProxy(t *testing.T) {
	var tagged []string
	var tmu sync.Mutex
	u := message.Usage{InputTokens: 100, OutputTokens: 10}
	m := &message.Message{Role: message.RoleAssistant, Origin: message.Origin{Kind: message.OriginModel}, Content: message.Content{&message.TextBlock{Text: "hi"}}, Usage: &u, StopReason: string(provider.StopEndTurn)}
	sp := providertest.NewScriptProvider(provider.APIAnthropicMessages, []providertest.Step{
		{Event: provider.StreamEvent{Kind: provider.SEStart, ResponseID: "msg_1"}},
		{Event: provider.StreamEvent{Kind: provider.SEBlockStart, Index: 0, BlockType: message.TypeText}},
		{Event: provider.StreamEvent{Kind: provider.SETextDelta, Index: 0, Channel: "text", Text: "hi"}},
		{Event: provider.StreamEvent{Kind: provider.SEBlockStop, Index: 0, Block: &message.TextBlock{Text: "hi"}}},
		{Event: provider.StreamEvent{Kind: provider.SEEnd, Message: m, Stop: &provider.Stop{Reason: provider.StopEndTurn, Raw: "end_turn"}, Usage: &u}},
	})
	p := &proxy{sessions: map[string][]map[string]any{}}
	srv := httptest.NewServer(p)
	defer srv.Close()
	lo := litellm.Options{BaseURL: srv.URL, APIKey: "k", SessionHeader: "x-litellm-session-id"}
	tag := litellm.RequestHeaders(lo)
	ag, err := kernel.New(kernel.WithModel(provider.ModelRef{Provider: sp, Model: "m"}), kernel.WithCapabilityResolver(nil),
		kernel.WithRequestHeaders(func(ri kernel.RequestInfo) map[string]string {
			h := tag(ri)
			tmu.Lock()
			tagged = append(tagged, h["x-litellm-session-id"])
			tmu.Unlock()
			return h
		}),
		kernel.WithCostSource(litellm.CostSource(lo)))
	if err != nil {
		t.Fatal(err)
	}
	defer ag.Close(context.Background())
	sess, err := ag.NewSession(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	sid := sess.Info().SessionID
	p.mu.Lock()
	p.sessions[sid] = []map[string]any{{"request_id": "msg_1", "spend": 0.0042}}
	p.mu.Unlock()
	rc, _ := sess.Send(kernel.Text("hello"))
	res, err := rc.Wait(t.Context())
	if err != nil || res.Err != nil {
		t.Fatal(err, res.Err)
	}
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	tc, err := sess.TurnCost(ctx, res.TurnID)
	if err != nil || len(tc.Priced) != 1 || tc.Total.Amount != 0.0042 || tc.Total.Currency != "USD" || len(tc.Missing) != 0 {
		t.Fatalf("%v %+v", err, tc)
	}
	if !slices.Equal(tagged, []string{sid}) {
		t.Fatalf("every request carries the root session id: %v", tagged)
	}
}
