package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/estonshi/aperture-potatos/internal/sse"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/providertest"
)

const okStream = "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n\n" +
	"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
	"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
	"event: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\n" +
	"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":2}}\n\n" +
	"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"

func request() *provider.Request {
	return &provider.Request{Model: "claude-opus-5-5", Messages: []message.Message{message.UserText("hello")}}
}

func final(t *testing.T, p *Provider, req *provider.Request) *message.Message {
	t.Helper()
	for ev, err := range p.Stream(context.Background(), req) {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Kind == provider.SEEnd {
			return ev.Message
		}
	}
	t.Fatal("no end event")
	return nil
}

// cloudDoer stands in for a cloud backend: it moves the model into the URL, edits the body, and re-encodes the reply, all without adapter changes.
type cloudDoer struct {
	next     *providertest.FixtureTransport
	sawURL   string
	sawModel bool
}

func (d *cloudDoer) Do(req *http.Request) (*http.Response, error) {
	var body map[string]any
	b, _ := io.ReadAll(req.Body)
	_ = json.Unmarshal(b, &body)
	model, _ := body["model"].(string)
	delete(body, "model")
	body["anthropic_version"] = "vertex-2023-10-16"
	nb, _ := json.Marshal(body)
	out, _ := http.NewRequestWithContext(req.Context(), http.MethodPost, "https://cloud.example/models/"+model+":streamRawPredict", bytes.NewReader(nb))
	out.Header = req.Header.Clone()
	d.sawURL, d.sawModel = out.URL.String(), strings.Contains(string(nb), `"model"`)
	resp, err := d.next.Do(out)
	if err != nil {
		return nil, err
	}
	frames, _ := io.ReadAll(resp.Body) // a real Doer would transcode AWS eventstream frames here
	resp.Body = io.NopCloser(bytes.NewReader(frames))
	return resp, nil
}

func TestDoerRewritesRequestAndResponse(t *testing.T) {
	ft := providertest.NewFixtureTransport(providertest.Response{Body: []byte(okStream)})
	d := &cloudDoer{next: ft}
	m := final(t, New(Config{Doer: d}), request())
	if m.Text() != "hi" || d.sawModel || d.sawURL != "https://cloud.example/models/claude-opus-5-5:streamRawPredict" {
		t.Fatalf("text %q, model left in body %v, url %s", m.Text(), d.sawModel, d.sawURL)
	}
	if got := ft.Requests()[0].Body; !bytes.Contains(got, []byte(`"anthropic_version":"vertex-2023-10-16"`)) {
		t.Fatalf("body not rewritten: %s", got)
	}
}

func TestHeadersAndEndpoint(t *testing.T) {
	ft := providertest.NewFixtureTransport(providertest.Response{Body: []byte(okStream)})
	p := New(Config{APIKey: "k", BaseURL: "https://LiteLLM.example.com:443/", Doer: ft, Betas: []string{"a-beta"}})
	final(t, p, request())
	r := ft.Requests()[0]
	if r.URL != "https://LiteLLM.example.com:443/v1/messages" || p.Endpoint() != "litellm.example.com" {
		t.Fatalf("url %s endpoint %s", r.URL, p.Endpoint())
	}
	if r.Header.Get("x-api-key") != "k" || r.Header.Get("anthropic-beta") != "a-beta" || r.Header.Get("anthropic-version") != apiVersion {
		t.Fatalf("headers %v", r.Header)
	}
}

type closeSpy struct {
	io.Reader
	closed bool
}

func (c *closeSpy) Close() error { c.closed = true; return nil }

type spyDoer struct{ body *closeSpy }

func (d spyDoer) Do(req *http.Request) (*http.Response, error) {
	return &http.Response{StatusCode: 200, Header: http.Header{}, Body: d.body}, nil
}

func TestBreakClosesBody(t *testing.T) {
	spy := &closeSpy{Reader: strings.NewReader(okStream)}
	for ev := range New(Config{Doer: spyDoer{spy}}).Stream(context.Background(), request()) {
		if ev.Kind == provider.SEStart {
			break
		}
	}
	if !spy.closed {
		t.Fatal("breaking out of the loop must close the body")
	}
}

func TestCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, err := range New(Config{Doer: failDoer{}}).Stream(ctx, request()) {
		if e := provider.AsError(err); err == nil || e.Class != provider.ClassCanceled {
			t.Fatalf("want canceled, got %v", err)
		}
	}
}

type failDoer struct{}

func (failDoer) Do(req *http.Request) (*http.Response, error) { return nil, req.Context().Err() }

func TestResolveCapabilities(t *testing.T) {
	body := `{"type":"model","id":"claude-opus-5-5","max_input_tokens":1000000,"max_tokens":128000,
		"capabilities":{"thinking":{"supported":true,"types":{"adaptive":{"supported":true},"enabled":{"supported":false}}},
		"effort":{"supported":true,"low":{"supported":true},"medium":{"supported":true},"high":{"supported":true},"max":{"supported":true}},
		"image_input":{"supported":true}}}`
	ft := providertest.NewFixtureTransport(providertest.Response{Body: []byte(body)})
	c, err := New(Config{Doer: ft}).ResolveCapabilities(context.Background(), provider.ModelInfo{API: api, Model: "claude-opus-5-5"})
	if err != nil {
		t.Fatal(err)
	}
	if c.ContextWindow != 1000000 || c.MaxOutputTokens != 128000 || len(c.Reasoning.Modes) != 1 || c.Reasoning.Modes[0] != "adaptive" || len(c.Reasoning.Efforts) != 4 {
		t.Fatalf("got %+v", c)
	}
	if got := ft.Requests()[0].URL; got != "https://api.anthropic.com/v1/models/claude-opus-5-5" {
		t.Fatalf("url %s", got)
	}
	if c.NoWebSearch {
		t.Fatal("an absent server_tools entry leaves web search unknown")
	}
	for raw, want := range map[string]bool{`{"web_search":{"supported":false}}`: true, `{"web_search":{"supported":true}}`: false, `{}`: false} {
		if c, _ := parseModel([]byte(`{"capabilities":{"server_tools":` + raw + `}}`)); c.NoWebSearch != want {
			t.Errorf("server_tools %s: NoWebSearch %v", raw, c.NoWebSearch)
		}
	}
	if c, err := parseModel([]byte(`{"id":"x","capabilities":{"thinking":"weird"}}`)); err != nil || c.ContextWindow != 0 || c.Reasoning.Modes != nil {
		t.Fatalf("unknown shapes stay unknown: %+v %v", c, err)
	}
	ft = providertest.NewFixtureTransport(providertest.Response{Status: 404, Body: []byte(`{"type":"error","error":{"type":"not_found_error","message":"no"}}`)})
	if _, err := New(Config{Doer: ft}).ResolveCapabilities(context.Background(), provider.ModelInfo{Model: "x"}); provider.AsError(err).Class != provider.ClassNotFound {
		t.Fatalf("got %v", err)
	}
}

// FuzzDecoder mutates the layer-1 fixtures; whatever arrives, the decoder must not panic.
func FuzzDecoder(f *testing.F) {
	files, _ := filepath.Glob("../../conformance/provider/anthropic.messages/*/response.sse")
	for _, file := range files {
		if b, err := os.ReadFile(file); err == nil {
			f.Add(b)
		}
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		d := newDecoder(request(), "api.anthropic.com", "", http.Header{})
		r := sse.NewReader(bytes.NewReader(b))
		for !d.done {
			ev, err := r.Next()
			if err != nil {
				return
			}
			if _, err := d.handle(ev); err != nil {
				return
			}
		}
	})
}

// countDoer answers count_tokens and records what it was sent.
type countDoer struct {
	url  string
	body map[string]any
}

func (d *countDoer) Do(req *http.Request) (*http.Response, error) {
	d.url = req.URL.String()
	b, _ := io.ReadAll(req.Body)
	_ = json.Unmarshal(b, &d.body)
	return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"input_tokens":1234}`)), Request: req}, nil
}

func TestCountTokens(t *testing.T) {
	d := &countDoer{}
	p := New(Config{APIKey: "k", Doer: d})
	var _ provider.TokenCounter = p
	req := request()
	req.System = []provider.SystemBlock{{Slot: "core", Text: "Be brief."}}
	n, err := p.CountTokens(context.Background(), req)
	if err != nil || n != 1234 {
		t.Fatalf("%d %v", n, err)
	}
	if !strings.HasSuffix(d.url, "/v1/messages/count_tokens") {
		t.Fatalf("url %s", d.url)
	}
	for _, k := range []string{"stream", "max_tokens"} {
		if _, ok := d.body[k]; ok {
			t.Fatalf("count_tokens rejects %s: %v", k, d.body)
		}
	}
	if d.body["model"] != "claude-opus-5-5" || d.body["messages"] == nil || d.body["system"] == nil {
		t.Fatalf("the body is the request as Stream renders it: %v", d.body)
	}
}
