package a2a

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/estonshi/aperture-potatos/internal/sse"
)

// httpjson maps every operation to its own method and path; v0.3's paths start with /v1, and a v1.0 tenant prefixes them.
type httpjson struct {
	r *remote
}

func (h *httpjson) contentType() string {
	if h.r.iface.version == v10 {
		return "application/a2a+json"
	}
	return "application/json"
}

func (h *httpjson) accept() string {
	if h.r.iface.version == v10 {
		return "application/a2a+json, application/json"
	}
	return "application/json"
}

func (h *httpjson) target(o op, taskID string) (method, path string) {
	base := strings.TrimRight(h.r.iface.url, "/")
	if t := h.r.iface.tenant; t != "" && h.r.iface.version == v10 {
		base += "/" + url.PathEscape(t)
	}
	if h.r.iface.version == v03 {
		base += "/v1"
	}
	task := base + "/tasks/" + url.PathEscape(taskID)
	switch o {
	case opSend:
		return http.MethodPost, base + "/message:send"
	case opStream:
		return http.MethodPost, base + "/message:stream"
	case opGet:
		return http.MethodGet, task
	case opCancel:
		return http.MethodPost, task + ":cancel"
	}
	if h.r.getSub.Load() {
		return http.MethodGet, task + ":subscribe"
	}
	return http.MethodPost, task + ":subscribe"
}

func (h *httpjson) do(ctx context.Context, o op, taskID string, params any, accept string) (*http.Response, error) {
	method, target := h.target(o, taskID)
	var body io.Reader
	switch {
	case o == opSend || o == opStream:
		b, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		body = bytes.NewReader(b)
	case method == http.MethodPost:
		body = strings.NewReader("{}")
	case o == opGet:
		if q := h.r.codec.query(historyOf(params)); q != "" {
			target += "?" + q
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return nil, err
	}
	ct := ""
	if body != nil {
		ct = h.contentType()
	}
	h.r.headers(req, ct, accept)
	resp, err := h.r.cfg.doer.Do(req)
	if err == nil && o == opSubscribe && method == http.MethodPost && resp.StatusCode == http.StatusMethodNotAllowed {
		_, _ = readBody(resp)
		h.r.getSub.Store(true) // the v0.3 proto and some servers take GET only; remembered for this peer
		return h.do(ctx, o, taskID, params, accept)
	}
	return resp, err
}

func (h *httpjson) unary(ctx context.Context, o op, taskID string, params any) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	resp, err := h.do(ctx, o, taskID, params, h.accept())
	if err != nil {
		return nil, err
	}
	body, err := readBody(resp)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, classify(httpError(resp, body))
	}
	return body, nil
}

func (h *httpjson) stream(ctx context.Context, o op, taskID string, params any) (frames, error) {
	ctx, cancel := context.WithCancel(ctx)
	resp, err := h.do(ctx, o, taskID, params, "text/event-stream")
	if err != nil {
		cancel()
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 || !isEventStream(resp) {
		defer cancel()
		body, err := readBody(resp)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return nil, classify(httpError(resp, body))
		}
		return &oneFrame{raw: body}, nil
	}
	return &sseFrames{resp: resp, r: sse.NewReader(resp.Body), parse: restEvent, cancel: cancel}, nil
}

// restEvent is one stream event; an error object in the stream is the peer's error.
func restEvent(data []byte) (json.RawMessage, error) {
	var st rpcStatus
	if json.Unmarshal(data, &st) == nil && st.Error != nil {
		we := &wireError{status: st.Error.Code, text: st.Error.Message}
		for _, d := range st.Error.Details {
			if d.Reason != "" {
				we.reason = d.Reason
			}
		}
		return nil, classify(we)
	}
	return json.RawMessage(data), nil
}

// historyOf reads the history length back from GetTask params, which over HTTP+JSON travels in the query.
func historyOf(params any) int {
	b, _ := json.Marshal(params)
	var p struct {
		HistoryLength int `json:"historyLength"`
	}
	_ = json.Unmarshal(b, &p)
	return p.HistoryLength
}
