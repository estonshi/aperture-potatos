package a2a

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"strconv"

	"github.com/estonshi/aperture-potatos/ext/peer"
	"github.com/estonshi/aperture-potatos/internal/sse"
)

type op int

const (
	opSend op = iota + 1
	opStream
	opGet
	opCancel
	opSubscribe
)

// wire is one binding: how an operation becomes an HTTP request and its answer a result or a stream of them.
type wire interface {
	unary(ctx context.Context, o op, taskID string, params any) (json.RawMessage, error)
	stream(ctx context.Context, o op, taskID string, params any) (frames, error)
}

// frames reads the results of one stream; io.EOF means the server closed it.
type frames interface {
	next() (json.RawMessage, error)
	close()
}

// wireError is an error the peer answered with, kept with what the client needs to classify it.
type wireError struct {
	status int    // HTTP status; 0 for a JSON-RPC error inside a 200
	code   int    // JSON-RPC error code
	reason string // v1.0 ErrorInfo reason
	text   string
	auth   string // WWW-Authenticate
}

var rpcNames = map[int]string{
	-32700: "JSONParseError", -32600: "InvalidRequestError", -32601: "MethodNotFoundError", -32602: "InvalidParamsError", -32603: "InternalError",
	-32001: "TaskNotFoundError", -32002: "TaskNotCancelableError", -32003: "PushNotificationNotSupportedError", -32004: "UnsupportedOperationError",
	-32005: "ContentTypeNotSupportedError", -32006: "InvalidAgentResponseError", -32007: "ExtendedAgentCardNotConfiguredError",
	-32008: "ExtensionSupportRequiredError", -32009: "VersionNotSupportedError",
}

func (e *wireError) Error() string {
	var where string
	switch {
	case e.code != 0:
		where = strconv.Itoa(e.code)
		if n := rpcNames[e.code]; n != "" {
			where = n + " " + where
		}
	case e.reason != "":
		where = e.reason + " HTTP " + strconv.Itoa(e.status)
	default:
		where = "HTTP " + strconv.Itoa(e.status)
	}
	if e.text == "" {
		return "a2a: " + where
	}
	return "a2a: " + where + ": " + e.text
}

// classify turns a peer's error into what the a2a task acts on: credentials, a task the peer no longer knows, or a failure.
func classify(err error) error {
	var we *wireError
	if !errors.As(err, &we) {
		return err
	}
	switch {
	case we.status == http.StatusUnauthorized || we.status == http.StatusForbidden || we.code == -31401 || we.code == -31403:
		st := we.status
		if st == 0 {
			st = -we.code - 31000 // -31401 and -31403 carry the HTTP status they stand for
		}
		detail := we.auth
		if detail == "" {
			detail = we.text
		}
		return &peer.AuthError{Status: st, Detail: detail}
	case we.code == -32001 || we.reason == "TASK_NOT_FOUND" || we.status == http.StatusNotFound:
		return fmt.Errorf("%w: %v", peer.ErrUnknownTask, err)
	}
	return err
}

func notCancelable(err error) bool {
	var we *wireError
	return errors.As(err, &we) && (we.code == -32002 || we.reason == "TASK_NOT_CANCELABLE" || we.code == 0 && we.reason == "" && we.status == http.StatusConflict)
}

// retryable is a failure a GetTask or a reconnect may try again: the network, or the server's own trouble.
func retryable(ctx context.Context, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	var we *wireError
	if errors.As(err, &we) {
		return we.status >= 500
	}
	var ae *peer.AuthError
	return !errors.As(err, &ae) && !errors.Is(err, peer.ErrUnknownTask) && !errors.Is(err, errUndecodable)
}

// rpcStatus is v1.0's google.rpc.Status error body; ErrorInfo's reason names the A2A error.
type rpcStatus struct {
	Error *struct {
		Code    int    `json:"code"`
		Status  string `json:"status"`
		Message string `json:"message"`
		Details []struct {
			Type   string `json:"@type"`
			Reason string `json:"reason"`
			Domain string `json:"domain"`
		} `json:"details"`
	} `json:"error"`
}

// httpError reads a non-2xx answer: the v1.0 status body when there is one, else the status code alone.
func httpError(resp *http.Response, body []byte) *wireError {
	we := &wireError{status: resp.StatusCode, auth: resp.Header.Get("WWW-Authenticate")}
	var st rpcStatus
	if json.Unmarshal(body, &st) == nil && st.Error != nil {
		we.text = st.Error.Message
		for _, d := range st.Error.Details {
			if d.Reason != "" {
				we.reason = d.Reason
			}
		}
		return we
	}
	if len(body) > 0 && len(body) < 512 && !bytes.ContainsAny(body, "{<") {
		we.text = string(bytes.TrimSpace(body))
	}
	return we
}

func readBody(resp *http.Response) ([]byte, error) {
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, maxBody))
}

func isEventStream(resp *http.Response) bool {
	mt, _, _ := mime.ParseMediaType(resp.Header.Get("Content-Type"))
	return mt == "text/event-stream"
}

// sseFrames reads the data of each event; parse turns one into a result or an error.
type sseFrames struct {
	resp   *http.Response
	r      *sse.Reader
	parse  func([]byte) (json.RawMessage, error)
	cancel context.CancelFunc
}

func (f *sseFrames) next() (json.RawMessage, error) {
	for {
		ev, err := f.r.Next()
		if err != nil {
			return nil, err
		}
		if len(bytes.TrimSpace(ev.Data)) == 0 {
			continue // a keep-alive
		}
		return f.parse(ev.Data)
	}
}

func (f *sseFrames) close() {
	f.cancel()
	_ = f.resp.Body.Close()
}

// oneFrame is a stream request a server answered with one JSON result instead of a stream.
type oneFrame struct {
	raw  json.RawMessage
	done bool
}

func (f *oneFrame) next() (json.RawMessage, error) {
	if f.done {
		return nil, io.EOF
	}
	f.done = true
	return f.raw, nil
}

func (f *oneFrame) close() {}
