package providertest

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/estonshi/aperture-potatos/provider"
)

func TestScriptProvider(t *testing.T) {
	boom := errors.New("boom")
	sp := NewScriptProvider(provider.APIAnthropicMessages,
		[]Step{{Event: provider.StreamEvent{Kind: provider.SEStart}}, {Event: provider.StreamEvent{Kind: provider.SEEnd}}},
		[]Step{{Event: provider.StreamEvent{Kind: provider.SEStart}}, {Err: boom}})
	var kinds []provider.StreamEventKind
	for ev, err := range sp.Stream(context.Background(), &provider.Request{Model: "a"}) {
		if err != nil {
			t.Fatal(err)
		}
		kinds = append(kinds, ev.Kind)
	}
	var got error
	for _, err := range sp.Stream(context.Background(), &provider.Request{Model: "b"}) {
		got = err
	}
	if len(kinds) != 2 || !errors.Is(got, boom) || len(sp.Requests()) != 2 || sp.Requests()[1].Model != "b" {
		t.Fatalf("kinds %v err %v", kinds, got)
	}
	for _, err := range sp.Stream(context.Background(), &provider.Request{}) {
		if err == nil {
			t.Fatal("an exhausted script must fail the call")
		}
	}
}

func TestRecordingRedacts(t *testing.T) {
	next := NewFixtureTransport(Response{Header: http.Header{"Set-Cookie": {"s=1"}}, Body: []byte("data: sk-ant-abcdefghijklmnopqrstuv\n\n")})
	rt := &RecordingTransport{Next: next}
	req, _ := http.NewRequest(http.MethodPost, "https://x/v1/messages", strings.NewReader(`{"key":"sk-abcdefghijklmnopqrstuvwx"}`))
	req.Header.Set("x-api-key", "secret")
	resp, err := rt.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.ReadAll(resp.Body)
	ex := rt.Exchanges()[0]
	if ex.Request.Header.Get("x-api-key") != "REDACTED" || bytes.Contains(ex.Request.Body, []byte("sk-")) {
		t.Fatalf("request not redacted: %v %s", ex.Request.Header, ex.Request.Body)
	}
	if bytes.Contains(ex.Response.Body, []byte("sk-ant")) || ex.Response.Header.Get("Set-Cookie") != "REDACTED" {
		t.Fatalf("response not redacted: %s", ex.Response.Body)
	}
	if got := next.Requests()[0].Header.Get("x-api-key"); got != "secret" {
		t.Fatal("redaction must not touch what is actually sent")
	}
}
