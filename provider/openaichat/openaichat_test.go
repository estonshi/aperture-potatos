package openaichat

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

const okStream = "data: {\"id\":\"chatcmpl-1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"

func request() *provider.Request {
	return &provider.Request{Model: "deepseek-v4.1-flash", Messages: []message.Message{message.UserText("hello")}, CacheKey: "ses_1"}
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

func TestHeadersEndpointAndCompatibleServer(t *testing.T) {
	ft := providertest.NewFixtureTransport(providertest.Response{Body: []byte(okStream)})
	p := New(Config{APIKey: "k", BaseURL: "https://LiteLLM.example.com:443/v1/", Doer: ft})
	if m := final(t, p, request()); m.Text() != "hi" {
		t.Fatalf("text %q", m.Text())
	}
	r := ft.Requests()[0]
	if r.URL != "https://LiteLLM.example.com:443/v1/chat/completions" || p.Endpoint() != "litellm.example.com" || r.Header.Get("authorization") != "Bearer k" {
		t.Fatalf("url %s endpoint %s headers %v", r.URL, p.Endpoint(), r.Header)
	}
	if bytes.Contains(r.Body, []byte("prompt_cache_key")) || bytes.Contains(r.Body, []byte("include_obfuscation")) {
		t.Fatalf("fields only OpenAI accepts reached a compatible server: %s", r.Body)
	}
}

type closeSpy struct {
	io.Reader
	closed bool
}

func (c *closeSpy) Close() error { c.closed = true; return nil }

type spyDoer struct{ body *closeSpy }

func (d spyDoer) Do(*http.Request) (*http.Response, error) {
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

func TestResolveCapabilitiesFromListing(t *testing.T) {
	listing := `{"object":"list","data":[
		{"id":"deepseek-v4.1-flash","object":"model","model_info":{"max_input_tokens":128000,"max_output_tokens":8192}},
		{"id":"llama","object":"model","max_model_len":32768},
		{"id":"router","object":"model","context_length":200000,"max_completion_tokens":16000,"model_info":{"max_tokens":999}},
		{"model_name":"named-only","model_info":{"context_window":64000,"max_tokens":4096}},
		{"id":"bare","object":"model"}]}`
	ft := providertest.NewFixtureTransport(providertest.Response{Status: 500, Body: []byte(`{"error":{"message":"down"}}`)}, providertest.Response{Body: []byte(listing)})
	p := New(Config{Doer: ft})
	ctx := context.Background()
	if _, err := p.ResolveCapabilities(ctx, provider.ModelInfo{Model: "llama"}); err == nil {
		t.Fatal("a failed listing must report its error")
	}
	want := map[string][2]int{"deepseek-v4.1-flash": {128000, 8192}, "llama": {32768, 0}, "router": {200000, 16000}, "named-only": {64000, 4096}, "bare": {}, "unlisted": {}}
	for model, w := range want {
		c, err := p.ResolveCapabilities(ctx, provider.ModelInfo{Model: model})
		if err != nil || c.ContextWindow != w[0] || c.MaxOutputTokens != w[1] {
			t.Errorf("%s: %+v %v", model, c, err)
		}
	}
	if n := len(ft.Requests()); n != 2 || ft.Requests()[1].URL != "https://api.openai.com/v1/models" || ft.Requests()[1].Method != http.MethodGet {
		t.Fatalf("one retry after the failure, then one listing for every model; got %d requests", n)
	}
}

func TestMergeThinking(t *testing.T) {
	frags := []map[string]any{
		{"type": "thinking", "thinking": "a"}, {"type": "thinking", "thinking": "b", "signature": "s1"},
		{"type": "thinking", "thinking": "c"}, {"type": "redacted_thinking", "data": "x"}, {"type": "thinking", "thinking": "d"},
	}
	var acc []map[string]any
	for _, f := range frags {
		acc = mergeThinking(acc, []map[string]any{f})
	}
	b, _ := json.Marshal(acc)
	if string(b) != `[{"signature":"s1","thinking":"ab","type":"thinking"},{"thinking":"c","type":"thinking"},{"data":"x","type":"redacted_thinking"},{"thinking":"d","type":"thinking"}]` {
		t.Fatalf("merged %s", b)
	}
	if got := replayableThinking(acc); len(got) != 2 || got[0]["signature"] != "s1" || got[1]["type"] != "redacted_thinking" {
		t.Fatalf("replayable %v", got)
	}
}

// FuzzDecoder mutates the layer-1 fixtures; whatever arrives, the decoder must not panic.
func FuzzDecoder(f *testing.F) {
	files, _ := filepath.Glob("../../conformance/provider/openai.chat/*/response.sse")
	for _, file := range files {
		if b, err := os.ReadFile(file); err == nil {
			f.Add(b)
		}
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		d := newDecoder(request(), "api.openai.com", http.Header{}, 0)
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
