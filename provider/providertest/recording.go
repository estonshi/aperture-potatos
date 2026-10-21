package providertest

import (
	"bytes"
	"io"
	"net/http"
	"regexp"
	"sync"

	"github.com/estonshi/aperture-potatos/provider"
)

// Exchange is one request and the response it got, captured by a RecordingTransport.
type Exchange struct {
	Request  Request
	Response Response
}

// RecordingTransport wraps a real Doer and keeps every exchange with credentials removed, so it can be saved as a fixture.
type RecordingTransport struct {
	Next provider.Doer

	mu        sync.Mutex
	exchanges []*Exchange
}

var secretHeaders = []string{"Authorization", "X-Api-Key", "Api-Key", "Proxy-Authorization", "Cookie", "Set-Cookie"}

// keyLike catches credentials pasted into bodies, which header redaction alone would miss.
var keyLike = regexp.MustCompile(`\b(sk-[A-Za-z0-9_-]{16,}|sk-ant-[A-Za-z0-9_-]{16,})`)

func redact(h http.Header) http.Header {
	h = h.Clone()
	for _, k := range secretHeaders {
		if h.Get(k) != "" {
			h.Set(k, "REDACTED")
		}
	}
	return h
}

// Do forwards req to Next and records the exchange, keeping the response body as the caller reads it.
func (t *RecordingTransport) Do(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		body = b
		req.Body = io.NopCloser(bytes.NewReader(b))
	}
	ex := &Exchange{Request: Request{Method: req.Method, URL: req.URL.String(), Header: redact(req.Header), Body: keyLike.ReplaceAll(body, []byte("REDACTED"))}}
	resp, err := t.Next.Do(req)
	if err != nil {
		return nil, err
	}
	ex.Response = Response{Status: resp.StatusCode, Header: redact(resp.Header)}
	resp.Body = &tee{ReadCloser: resp.Body, ex: ex, mu: &t.mu}
	t.mu.Lock()
	t.exchanges = append(t.exchanges, ex)
	t.mu.Unlock()
	return resp, nil
}

// Exchanges returns what has been recorded so far; a body is complete once the adapter has read it to the end.
func (t *RecordingTransport) Exchanges() []Exchange {
	t.mu.Lock()
	defer t.mu.Unlock()
	out := make([]Exchange, len(t.exchanges))
	for i, e := range t.exchanges {
		out[i] = *e
		out[i].Response.Body = keyLike.ReplaceAll(e.Response.Body, []byte("REDACTED"))
	}
	return out
}

type tee struct {
	io.ReadCloser
	ex *Exchange
	mu *sync.Mutex
}

func (t *tee) Read(p []byte) (int, error) {
	n, err := t.ReadCloser.Read(p)
	t.mu.Lock()
	t.ex.Response.Body = append(t.ex.Response.Body, p[:n]...)
	t.mu.Unlock()
	return n, err
}
