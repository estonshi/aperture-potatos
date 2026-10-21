package openairesponses

import (
	"bytes"
	"context"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/estonshi/aperture-potatos/internal/sse"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/providertest"
)

const okStream = "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\"}}\n\n" +
	"data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"id\":\"msg_1\",\"type\":\"message\"}}\n\n" +
	"data: {\"type\":\"response.output_text.delta\",\"item_id\":\"msg_1\",\"delta\":\"hi\"}\n\n" +
	"data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"id\":\"msg_1\",\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"hi\"}]}}\n\n" +
	"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"usage\":{\"input_tokens\":3,\"output_tokens\":1}}}\n\n"

func request() *provider.Request {
	return &provider.Request{Model: "gpt-5.6", Messages: []message.Message{message.UserText("hello")}}
}

func TestHeadersAndEndpoint(t *testing.T) {
	ft := providertest.NewFixtureTransport(providertest.Response{Body: []byte(okStream)}, providertest.Response{Body: []byte(`{"data":[{"id":"gpt-5.6","model_info":{"max_input_tokens":400000,"max_output_tokens":128000}}]}`)})
	p := New(Config{APIKey: "k", BaseURL: "https://gw.example/v1", Doer: ft})
	var text string
	for ev, err := range p.Stream(context.Background(), request()) {
		if err != nil {
			t.Fatal(err)
		}
		if ev.Kind == provider.SEEnd {
			text = ev.Message.Text()
		}
	}
	r := ft.Requests()[0]
	if text != "hi" || r.URL != "https://gw.example/v1/responses" || p.Endpoint() != "gw.example" || r.Header.Get("authorization") != "Bearer k" {
		t.Fatalf("text %q url %s endpoint %s", text, r.URL, p.Endpoint())
	}
	c, err := p.ResolveCapabilities(context.Background(), provider.ModelInfo{Model: "gpt-5.6"})
	if err != nil || c.ContextWindow != 400000 || c.MaxOutputTokens != 128000 || ft.Requests()[1].URL != "https://gw.example/v1/models" {
		t.Fatalf("caps %+v %v", c, err)
	}
}

// FuzzDecoder mutates the layer-1 fixtures; whatever arrives, the decoder must not panic.
func FuzzDecoder(f *testing.F) {
	files, _ := filepath.Glob("../../conformance/provider/openai.responses/*/response.sse")
	for _, file := range files {
		if b, err := os.ReadFile(file); err == nil {
			f.Add(b)
		}
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		d := newDecoder(request(), "api.openai.com", http.Header{})
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
