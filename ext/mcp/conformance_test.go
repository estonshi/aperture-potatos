package mcp_test

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/estonshi/aperture-potatos/conformance"
	"github.com/estonshi/aperture-potatos/ext/mcp"
	"github.com/estonshi/aperture-potatos/kernel"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

var update = flag.Bool("update", false, "rewrite expected files from the Go reference implementation")

// caseOptions is a case's `mcp` options: the servers it describes, served in process, then the extension's settings.
type caseOptions struct {
	Servers        []caseServer `json:"servers"`
	Discovery      string       `json:"discovery"`
	DiscoveryRatio float64      `json:"discovery_ratio"`
	CatalogRatio   float64      `json:"catalog_ratio"`
	ContextWindow  int          `json:"context_window"`
}

type caseServer struct {
	Name         string       `json:"name"`
	Instructions string       `json:"instructions"`
	Tools        []caseTool   `json:"tools"`
	Prompts      []casePrompt `json:"prompts"`
}

type caseTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
	Echo        bool            `json:"echo"`     // answer with the text argument
	Text        string          `json:"text"`     // the text an answer carries otherwise
	IsError     bool            `json:"is_error"` // answer with an error result
	Error       string          `json:"error"`    // fail the call with a protocol error of this text
	Elicit      *caseElicit     `json:"elicit"`   // ask the client for input, then answer with its action and content
}

// caseElicit is a server's request for input: a form of one message and schema, or a URL.
type caseElicit struct {
	Mode    string          `json:"mode"` // form or url; empty infers it
	Message string          `json:"message"`
	Schema  json.RawMessage `json:"schema"` // the form's requested_schema
	URL     string          `json:"url"`
}

type casePrompt struct {
	Name        string          `json:"name"`
	Title       string          `json:"title"`
	Description string          `json:"description"`
	Arguments   []casePromptArg `json:"arguments"`
	Text        string          `json:"text"` // {{name}} is replaced by the argument's value
}

type casePromptArg struct {
	Name     string `json:"name"`
	Required bool   `json:"required"`
}

var textSchema = json.RawMessage(`{"type":"object","properties":{"text":{"type":"string"}},"required":["text"]}`)

// inProcess serves a stateful Streamable HTTP MCP server through the client's own HTTP client, so a case exchanges
// messages without a socket: a loop case runs in a synctest bubble, whose goroutines wait on a pipe only until fake
// time passes a deadline. A stateful server streams its responses and its standalone SSE stream, which a stateless
// JSON response cannot: a server asks the client for input (mcp_elicitation) over those streams.
type inProcess struct{ h http.Handler }

func (t inProcess) RoundTrip(r *http.Request) (*http.Response, error) {
	pr, pw := io.Pipe()
	ctx, cancel := context.WithCancel(r.Context())
	sw := &streamWriter{hdr: make(http.Header), pw: pw, ready: make(chan struct{})}
	req := r.WithContext(ctx)
	go func() {
		defer func() {
			sw.WriteHeader(http.StatusOK) // a handler that wrote nothing still answers 200
			_ = pw.Close()
		}()
		t.h.ServeHTTP(sw, req)
	}()
	select {
	case <-sw.ready:
	case <-ctx.Done():
		cancel()
		return nil, ctx.Err()
	}
	sw.mu.Lock()
	hdr, status := sw.hdr.Clone(), sw.status
	sw.mu.Unlock()
	return &http.Response{StatusCode: status, Status: fmt.Sprintf("%d %s", status, http.StatusText(status)),
		Header: hdr, Body: &streamBody{PipeReader: pr, cancel: cancel}, Request: r}, nil
}

// streamWriter is the response writer of one in-process request: headers signal RoundTrip, and every write goes
// straight into the pipe so an event stream reaches the client as it is written.
type streamWriter struct {
	mu     sync.Mutex
	hdr    http.Header
	status int
	once   sync.Once
	ready  chan struct{}
	pw     *io.PipeWriter
}

func (w *streamWriter) Header() http.Header { return w.hdr }

func (w *streamWriter) WriteHeader(code int) {
	w.mu.Lock()
	if w.status == 0 {
		w.status = code
	}
	w.mu.Unlock()
	w.once.Do(func() { close(w.ready) })
}

func (w *streamWriter) Write(p []byte) (int, error) {
	w.WriteHeader(http.StatusOK)
	return w.pw.Write(p)
}

func (w *streamWriter) Flush() {}

// streamBody ends the handler when the client closes the response, as it does for a stream that outlives no more.
type streamBody struct {
	*io.PipeReader
	cancel context.CancelFunc
}

func (b *streamBody) Close() error {
	b.cancel()
	return b.PipeReader.Close()
}

// serve turns one case server into the extension's server: an MCP server of the SDK in process, stateful so it may
// ask the client for input, with responses and the standalone SSE stream carried by inProcess pipes.
func serve(t *testing.T, cs caseServer) mcp.Server {
	t.Helper()
	caps := &sdk.ServerCapabilities{Tools: &sdk.ToolCapabilities{}}
	if len(cs.Prompts) > 0 {
		caps.Prompts = &sdk.PromptCapabilities{}
	}
	srv := sdk.NewServer(&sdk.Implementation{Name: "conformance", Version: "1"}, &sdk.ServerOptions{Instructions: cs.Instructions, Capabilities: caps})
	for _, ct := range cs.Tools {
		schema := ct.InputSchema
		if len(schema) == 0 {
			schema = textSchema
		}
		srv.AddTool(&sdk.Tool{Name: ct.Name, Description: ct.Description, InputSchema: schema}, toolHandler(ct))
	}
	for _, cp := range cs.Prompts {
		var args []*sdk.PromptArgument
		for _, a := range cp.Arguments {
			args = append(args, &sdk.PromptArgument{Name: a.Name, Required: a.Required})
		}
		srv.AddPrompt(&sdk.Prompt{Name: cp.Name, Title: cp.Title, Description: cp.Description, Arguments: args}, promptHandler(cp))
	}
	h := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return srv }, nil)
	return mcp.Server{Name: cs.Name, URL: "http://conformance.invalid/mcp", HTTPClient: &http.Client{Transport: inProcess{h}},
		Prompts: len(cs.Prompts) > 0}
}

func toolHandler(ct caseTool) sdk.ToolHandler {
	return func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		if ct.Error != "" {
			return nil, errors.New(ct.Error)
		}
		if e := ct.Elicit; e != nil {
			res, err := req.Session.Elicit(ctx, &sdk.ElicitParams{Mode: e.Mode, Message: e.Message, RequestedSchema: e.Schema, URL: e.URL})
			if err != nil {
				return nil, err
			}
			b, err := json.Marshal(map[string]any{"action": res.Action, "content": res.Content})
			if err != nil {
				return nil, err
			}
			return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: string(b)}}}, nil
		}
		text := ct.Text
		if ct.Echo {
			var in struct {
				Text string `json:"text"`
			}
			_ = json.Unmarshal(req.Params.Arguments, &in)
			text = in.Text
		}
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: text}}, IsError: ct.IsError}, nil
	}
}

func promptHandler(cp casePrompt) func(context.Context, *sdk.GetPromptRequest) (*sdk.GetPromptResult, error) {
	return func(_ context.Context, req *sdk.GetPromptRequest) (*sdk.GetPromptResult, error) {
		text := cp.Text
		for _, a := range cp.Arguments {
			text = strings.ReplaceAll(text, "{{"+a.Name+"}}", req.Params.Arguments[a.Name])
		}
		return &sdk.GetPromptResult{Messages: []*sdk.PromptMessage{{Role: "user", Content: &sdk.TextContent{Text: text}}}}, nil
	}
}

// build turns a case's options into the extension, serving each server it describes in process.
func build(t *testing.T, name string, raw json.RawMessage) (kernel.Extension, error) {
	t.Helper()
	if name != mcp.Name {
		return nil, fmt.Errorf("unknown extension %q", name)
	}
	var o caseOptions
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &o); err != nil {
			return nil, err
		}
	}
	servers := make([]mcp.Server, len(o.Servers))
	for i, cs := range o.Servers {
		servers[i] = serve(t, cs)
	}
	return mcp.New(mcp.Options{Servers: servers, Discovery: mcp.Mode(o.Discovery),
		DiscoveryRatio: o.DiscoveryRatio, CatalogRatio: o.CatalogRatio, ContextWindow: o.ContextWindow}), nil
}

// TestConformance runs conformance/loop/ext/mcp, the cases this extension owns and deletes with itself.
func TestConformance(t *testing.T) {
	root := filepath.Join("..", "..", "conformance", "loop", "ext", "mcp")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Skip("no cases")
	}
	for _, e := range entries {
		dir := filepath.Join(root, e.Name())
		t.Run(e.Name(), func(t *testing.T) {
			res := conformance.RunLoopWithPoints(t, dir, func(name string, raw json.RawMessage) (kernel.Extension, error) {
				return build(t, name, raw)
			}, mcp.Elicitation)
			if t.Failed() {
				return
			}
			if err := res.Expected(dir, *update); err != nil {
				t.Fatal(err)
			}
		})
	}
}
