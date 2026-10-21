package a2a

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"testing/synctest"

	"github.com/estonshi/aperture-potatos/conformance"
	"github.com/estonshi/aperture-potatos/ext/peer"
)

var update = flag.Bool("update", false, "rewrite expected files from the Go reference implementation")

var root = filepath.Join("..", "..", "..", "conformance", "a2a")

// fixtureCall is one operation of call.json.
type fixtureCall struct {
	Op            string        `json:"op"` // send | subscribe | cancel
	Message       *peer.Message `json:"message,omitempty"`
	HistoryLength int           `json:"history_length,omitempty"`
	TaskID        string        `json:"task_id,omitempty"`
}

type fixtureCase struct {
	BaseURL string `json:"base_url"`
	Options struct {
		ProtocolVersion string `json:"protocol_version"`
		Binding         string `json:"binding"`
	} `json:"options"`
	Calls []fixtureCall `json:"calls"`
}

// exchange is one HTTP round trip after the card fetch: the request the client must send and the response it gets.
type exchange struct {
	Request struct {
		Method  string            `json:"method"`
		URL     string            `json:"url"`
		Headers map[string]string `json:"headers"`
		Body    json.RawMessage   `json:"body"`
	} `json:"request"`
	Response struct {
		Status  int               `json:"status"`
		Headers map[string]string `json:"headers"`
		Body    json.RawMessage   `json:"body"` // a JSON document
		SSE     string            `json:"sse"`  // a raw event-stream body
		Hold    bool              `json:"hold"` // after the body, the stream stays open until the request is canceled
	} `json:"response"`
}

// replay answers the card fetch and then the recorded exchanges in order, failing on any request that differs.
type replay struct {
	t     *testing.T
	card  []byte
	base  string
	mu    sync.Mutex
	steps []exchange
	n     int
	cardN int
}

func (r *replay) Do(req *http.Request) (*http.Response, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if req.URL.String() == strings.TrimRight(r.base, "/")+cardPath {
		r.cardN++
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(bytes.NewReader(r.card)), Request: req}, nil
	}
	if r.n >= len(r.steps) {
		r.t.Errorf("request %d %s %s: the fixture has no more exchanges", r.n+1, req.Method, req.URL)
		return nil, errors.New("no more exchanges")
	}
	ex := r.steps[r.n]
	r.n++
	where := fmt.Sprintf("exchange %d", r.n)
	if req.Method != ex.Request.Method || req.URL.String() != ex.Request.URL {
		r.t.Errorf("%s: sent %s %s, want %s %s", where, req.Method, req.URL, ex.Request.Method, ex.Request.URL)
	}
	for k, v := range ex.Request.Headers {
		if got := req.Header.Get(k); got != v {
			r.t.Errorf("%s: header %s is %q, want %q", where, k, got, v)
		}
	}
	var body []byte
	if req.Body != nil {
		body, _ = io.ReadAll(req.Body)
	}
	if !sameJSON(body, ex.Request.Body) {
		r.t.Errorf("%s: body\n got %s\nwant %s", where, body, ex.Request.Body)
	}
	resp := &http.Response{StatusCode: ex.Response.Status, Header: http.Header{}, Request: req}
	for k, v := range ex.Response.Headers {
		resp.Header.Set(k, v)
	}
	var b io.Reader = bytes.NewReader(ex.Response.Body)
	if ex.Response.SSE != "" {
		b = strings.NewReader(ex.Response.SSE)
	}
	if ex.Response.Hold {
		b = io.MultiReader(b, holdReader{req.Context()})
	}
	resp.Body = io.NopCloser(b)
	return resp, nil
}

// holdReader blocks until the request is canceled, as an open stream does.
type holdReader struct{ ctx context.Context }

func (h holdReader) Read([]byte) (int, error) {
	<-h.ctx.Done()
	return 0, h.ctx.Err()
}

func sameJSON(a, b []byte) bool {
	if len(bytes.TrimSpace(a)) == 0 || len(bytes.TrimSpace(b)) == 0 || string(bytes.TrimSpace(b)) == "null" {
		return len(bytes.TrimSpace(a)) == 0 && (len(bytes.TrimSpace(b)) == 0 || string(bytes.TrimSpace(b)) == "null")
	}
	var x, y any
	if json.Unmarshal(a, &x) != nil || json.Unmarshal(b, &y) != nil {
		return false
	}
	return reflect.DeepEqual(x, y)
}

// end is what closes one call in expected.events.jsonl: ok, or the error's class.
type end struct {
	End   string `json:"end"` // ok | error
	Kind  string `json:"kind,omitempty"`
	Code  int    `json:"status,omitempty"`
	Error string `json:"detail,omitempty"`
}

func endOf(err error) end {
	if err == nil {
		return end{End: "ok"}
	}
	var ae *peer.AuthError
	switch {
	case errors.As(err, &ae):
		return end{End: "error", Kind: "auth", Code: ae.Status, Error: ae.Detail}
	case errors.Is(err, peer.ErrUnknownTask):
		return end{End: "error", Kind: "unknown_task"}
	case errors.Is(err, ErrNoSupportedInterface):
		return end{End: "error", Kind: "no_supported_interface"}
	case errors.Is(err, ErrInterfaceHost):
		return end{End: "error", Kind: "interface_host"}
	}
	return end{End: "error", Kind: "failed"}
}

func runCase(t *testing.T, dir string) []json.RawMessage {
	var fc fixtureCase
	readJSON(t, filepath.Join(dir, "call.json"), &fc)
	card, err := os.ReadFile(filepath.Join(dir, "card.json"))
	if err != nil {
		t.Fatal(err)
	}
	rp := &replay{t: t, card: card, base: fc.BaseURL, steps: readExchanges(t, filepath.Join(dir, "exchange.jsonl"))}
	var out []json.RawMessage
	synctest.Test(t, func(t *testing.T) {
		opts := []Option{WithDoer(rp)}
		if v := fc.Options.ProtocolVersion; v != "" {
			opts = append(opts, WithProtocolVersion(v))
		}
		if b := fc.Options.Binding; b != "" {
			opts = append(opts, WithBinding(Binding(b)))
		}
		p, err := NewRemotePeer(t.Context(), fc.BaseURL, opts...)
		if err != nil {
			out = append(out, mustJSON(endOf(err)))
			return
		}
		for _, c := range fc.Calls {
			var seq func(func(peer.Event, error) bool)
			switch c.Op {
			case "send":
				seq = p.Send(t.Context(), *c.Message, peer.SendOptions{HistoryLength: c.HistoryLength})
			case "subscribe":
				seq = p.Subscribe(t.Context(), c.TaskID)
			case "cancel":
				out = append(out, mustJSON(endOf(p.Cancel(t.Context(), c.TaskID))))
				continue
			default:
				t.Fatalf("unknown op %q", c.Op)
			}
			var last error
			for ev, err := range seq {
				if err != nil {
					last = err
					break
				}
				out = append(out, mustJSON(ev))
			}
			out = append(out, mustJSON(endOf(last)))
		}
	})
	if rp.n != len(rp.steps) {
		t.Errorf("%d of %d exchanges were used", rp.n, len(rp.steps))
	}
	return out
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}

func readJSON(t *testing.T, path string, v any) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, v); err != nil {
		t.Fatalf("%s: %v", path, err)
	}
}

func readExchanges(t *testing.T, path string) []exchange {
	t.Helper()
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	var out []exchange
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<24)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) == 0 {
			continue
		}
		var ex exchange
		if err := json.Unmarshal(sc.Bytes(), &ex); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		out = append(out, ex)
	}
	return out
}

// expect compares one output of a case with its expected file by the conformance rules every binding shares.
func expect(t *testing.T, dir, name string, lines []json.RawMessage) {
	t.Helper()
	var b []byte
	for _, l := range lines {
		b = append(append(b, l...), '\n')
	}
	if err := conformance.Verify(dir, *update, func(out string) error { return os.WriteFile(filepath.Join(out, name), b, 0o644) }); err != nil {
		t.Error(err)
	}
}

// TestConformance runs conformance/a2a/{v03,v10}/{jsonrpc,httpjson}: the requests the client sends are compared with the fixture's, and the events it yields with the expected ones.
func TestConformance(t *testing.T) {
	for _, version := range []string{"v03", "v10"} {
		for _, binding := range []string{"jsonrpc", "httpjson"} {
			dir := filepath.Join(root, version, binding)
			entries, err := os.ReadDir(dir)
			if err != nil {
				t.Fatalf("%s: %v", dir, err)
			}
			for _, e := range entries {
				if !e.IsDir() {
					continue
				}
				c := filepath.Join(dir, e.Name())
				t.Run(version+"/"+binding+"/"+e.Name(), func(t *testing.T) {
					got := runCase(t, c)
					expect(t, c, "actual.events.jsonl", got)
				})
			}
		}
	}
}

// TestDiscovery runs conformance/a2a/discovery: a card and the options choose an interface, or none.
func TestDiscovery(t *testing.T) {
	dir := filepath.Join(root, "discovery")
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		c := filepath.Join(dir, e.Name())
		t.Run(e.Name(), func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(c, "card.json"))
			if err != nil {
				t.Fatal(err)
			}
			var o struct {
				ProtocolVersion string   `json:"protocol_version"`
				Binding         string   `json:"binding"`
				BaseURL         string   `json:"base_url"`
				AllowedHosts    []string `json:"allowed_hosts"`
			}
			if b, err := os.ReadFile(filepath.Join(c, "options.json")); err == nil {
				if err := json.Unmarshal(b, &o); err != nil {
					t.Fatal(err)
				}
			}
			type chosen struct {
				URL     string `json:"url,omitempty"`
				Binding string `json:"binding,omitempty"`
				Version string `json:"protocol_version,omitempty"`
				Tenant  string `json:"tenant,omitempty"`
				Error   string `json:"error,omitempty"`
			}
			var got chosen
			_, cands, err := parseCard(raw)
			if err == nil && o.BaseURL != "" {
				cands, err = onAllowedHosts(cands, o.BaseURL, o.AllowedHosts)
			}
			if err == nil {
				var pick candidate
				if pick, err = choose(cands, o.ProtocolVersion, Binding(o.Binding)); err == nil {
					got = chosen{URL: pick.url, Binding: string(pick.binding), Version: pick.version, Tenant: pick.tenant}
				}
			}
			if err != nil {
				got = chosen{Error: endOf(err).Kind}
			}
			expect(t, c, "actual.json", []json.RawMessage{mustJSON(got)})
		})
	}
}
