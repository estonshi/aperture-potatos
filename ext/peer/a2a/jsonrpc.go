package a2a

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"

	"github.com/estonshi/aperture-potatos/internal/sse"
)

// jsonrpc posts every operation to the interface's url as a JSON-RPC 2.0 call; a stream's events are whole JSON-RPC responses.
type jsonrpc struct {
	r *remote
}

var rpcMethods = map[string]map[op]string{
	v03: {opSend: "message/send", opStream: "message/stream", opGet: "tasks/get", opCancel: "tasks/cancel", opSubscribe: "tasks/resubscribe"},
	v10: {opSend: "SendMessage", opStream: "SendStreamingMessage", opGet: "GetTask", opCancel: "CancelTask", opSubscribe: "SubscribeToTask"},
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

func (j *jsonrpc) post(ctx context.Context, o op, params any, accept string) (*http.Response, error) {
	body, err := json.Marshal(rpcRequest{JSONRPC: "2.0", ID: j.r.rpcID.Add(1), Method: rpcMethods[j.r.iface.version][o], Params: params})
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, j.r.iface.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	j.r.headers(req, "application/json", accept)
	return j.r.cfg.doer.Do(req)
}

func (j *jsonrpc) unary(ctx context.Context, o op, _ string, params any) (json.RawMessage, error) {
	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	resp, err := j.post(ctx, o, params, "application/json")
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
	return rpcResult(body)
}

func (j *jsonrpc) stream(ctx context.Context, o op, _ string, params any) (frames, error) {
	ctx, cancel := context.WithCancel(ctx)
	resp, err := j.post(ctx, o, params, "text/event-stream")
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
		raw, err := rpcResult(body)
		if err != nil {
			return nil, err
		}
		return &oneFrame{raw: raw}, nil
	}
	return &sseFrames{resp: resp, r: sse.NewReader(resp.Body), parse: rpcResult, cancel: cancel}, nil
}

// rpcResult is a JSON-RPC response's result, or its error classified.
func rpcResult(body []byte) (json.RawMessage, error) {
	var r struct {
		Result json.RawMessage `json:"result"`
		Error  *struct {
			Code    int    `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &r); err != nil {
		return nil, errUndecodable
	}
	if r.Error != nil {
		return nil, classify(&wireError{code: r.Error.Code, text: r.Error.Message})
	}
	if len(r.Result) == 0 || string(r.Result) == "null" {
		return nil, errUndecodable
	}
	return r.Result, nil
}
