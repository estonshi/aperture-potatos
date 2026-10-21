// Package providertest holds test doubles for the provider layer: scripted providers for kernel tests, and HTTP fixtures that drive the real adapters.
package providertest

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// Response is one recorded HTTP response: status, headers and the raw body bytes (SSE for a stream).
type Response struct {
	Status int
	Header http.Header
	Body   []byte

	// HoldAfter, when positive, serves that many SSE events and then blocks the body, so a test can act while the stream is mid-flight.
	HoldAfter int
	// HoldFor is how long the body blocks; 0 blocks until the request is canceled.
	HoldFor time.Duration
}

// Request is what an adapter sent, kept for golden comparison of rendered bodies.
type Request struct {
	Method string
	URL    string
	Header http.Header
	Body   []byte
}

// FixtureTransport is a provider.Doer that answers each request with the next queued response.
type FixtureTransport struct {
	// ChunkSeed, when non-zero, splits every body into random small reads, so decoders are checked against arbitrary chunking.
	ChunkSeed uint64

	mu        sync.Mutex
	responses []Response
	requests  []Request
}

// NewFixtureTransport serves the responses in order, and a request beyond the last one fails.
func NewFixtureTransport(responses ...Response) *FixtureTransport {
	return &FixtureTransport{responses: responses}
}

// Do records the request, body included, and answers it with the next response.
func (f *FixtureTransport) Do(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		body = b
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, Request{Method: req.Method, URL: req.URL.String(), Header: req.Header.Clone(), Body: body})
	if len(f.responses) == 0 {
		return nil, errors.New("providertest: no fixture response left")
	}
	r := f.responses[0]
	f.responses = f.responses[1:]
	status := r.Status
	if status == 0 {
		status = http.StatusOK
	}
	var rd io.Reader = bytes.NewReader(r.Body)
	if f.ChunkSeed != 0 {
		rd = &chunked{data: r.Body, rng: rand.New(rand.NewPCG(f.ChunkSeed, uint64(len(f.requests))))}
	}
	if r.HoldAfter > 0 {
		cut := eventsEnd(r.Body, r.HoldAfter)
		rd = io.MultiReader(bytes.NewReader(r.Body[:cut]), &hold{ctx: req.Context(), d: r.HoldFor}, bytes.NewReader(r.Body[cut:]))
	}
	h := r.Header.Clone()
	if h == nil {
		h = http.Header{}
	}
	return &http.Response{StatusCode: status, Status: http.StatusText(status), Header: h, Body: io.NopCloser(rd), Request: req}, nil
}

// Requests returns every request the transport has served, in order.
func (f *FixtureTransport) Requests() []Request {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Request(nil), f.requests...)
}

// eventsEnd is the offset just past the n-th SSE event, or the whole body when it has fewer.
func eventsEnd(body []byte, n int) int {
	off := 0
	for range n {
		i := bytes.Index(body[off:], []byte("\n\n"))
		if i < 0 {
			return len(body)
		}
		off += i + 2
	}
	return off
}

// hold blocks one read: for d, or until the request is canceled when d is 0.
type hold struct {
	ctx  context.Context
	d    time.Duration
	done bool
}

func (h *hold) Read([]byte) (int, error) {
	if h.done {
		return 0, io.EOF
	}
	h.done = true
	var timer <-chan time.Time
	if h.d > 0 {
		t := time.NewTimer(h.d)
		defer t.Stop()
		timer = t.C
	}
	select {
	case <-timer:
		return 0, io.EOF
	case <-h.ctx.Done():
		return 0, h.ctx.Err()
	}
}

type chunked struct {
	data []byte
	rng  *rand.Rand
}

func (c *chunked) Read(p []byte) (int, error) {
	if len(c.data) == 0 {
		return 0, io.EOF
	}
	n := min(1+c.rng.IntN(17), len(p), len(c.data))
	copy(p, c.data[:n])
	c.data = c.data[n:]
	return n, nil
}

// headersFile is the shape of a fixture's headers.json.
type headersFile struct {
	Status  int               `json:"status"`
	Headers map[string]string `json:"headers"`
}

// LoadResponse reads a fixture directory: the body from response.sse (or response.json), status and headers from headers.json when present.
func LoadResponse(dir string) (Response, error) {
	r := Response{Header: http.Header{}}
	for name, ctype := range map[string]string{"response.sse": "text/event-stream", "response.json": "application/json"} {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err == nil {
			r.Body = b
			r.Header.Set("content-type", ctype)
			continue
		}
		if !errors.Is(err, os.ErrNotExist) {
			return r, err
		}
	}
	b, err := os.ReadFile(filepath.Join(dir, "headers.json"))
	switch {
	case errors.Is(err, os.ErrNotExist):
	case err != nil:
		return r, err
	default:
		var hf headersFile
		if err := json.Unmarshal(b, &hf); err != nil {
			return r, fmt.Errorf("%s/headers.json: %w", dir, err)
		}
		r.Status = hf.Status
		for k, v := range hf.Headers {
			r.Header.Set(k, v)
		}
	}
	return r, nil
}
