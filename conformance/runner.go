// Package conformance runs the language-neutral fixtures against the Go binding; it reaches adapters only through their exported constructors and knows no extension.
package conformance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/estonshi/aperture-potatos/conformance/compare"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/provider"
	"github.com/estonshi/aperture-potatos/provider/providertest"
	"github.com/estonshi/aperture-potatos/spec"
)

// Factory builds the adapter for one API over a Doer, which is how fixtures replace the network.
type Factory func(doer provider.Doer) provider.Provider

// Meta is a fixture's meta.json.
type Meta struct {
	API        provider.API `json:"api"`
	Model      string       `json:"model"`
	Source     string       `json:"source"` // recorded | synthetic
	RecordedAt string       `json:"recorded_at,omitempty"`
	Notes      string       `json:"notes,omitempty"`
}

// envelope lists the message fields the kernel fills at commit; an adapter's message has none of them yet, so they are not compared.
var envelope = []string{"v", "id", "session_id", "seq", "epoch", "ts"}

// MessageJSON is a message as fixtures compare it: without the commit envelope.
func MessageJSON(m *message.Message) (json.RawMessage, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(b, &obj); err != nil {
		return nil, err
	}
	for _, k := range envelope {
		delete(obj, k)
	}
	return json.Marshal(obj)
}

// Collect streams req and returns one JSON line per event, the terminal error as a final {"error": …} line, and the SEEnd message.
func Collect(ctx context.Context, p provider.Provider, req *provider.Request) ([]json.RawMessage, json.RawMessage, error) {
	var lines []json.RawMessage
	var final json.RawMessage
	for ev, err := range p.Stream(ctx, req) {
		if err != nil {
			line, merr := json.Marshal(map[string]any{"error": provider.AsError(err)})
			if merr != nil {
				return nil, nil, merr
			}
			return append(lines, line), final, nil
		}
		var msg json.RawMessage
		if ev.Message != nil {
			m, err := MessageJSON(ev.Message)
			if err != nil {
				return nil, nil, err
			}
			msg, ev.Message = m, nil
		}
		line, err := json.Marshal(ev)
		if err != nil {
			return nil, nil, err
		}
		if msg != nil {
			line = append(line[:len(line)-1], append([]byte(`,"message":`), append(msg, '}')...)...)
		}
		lines = append(lines, line)
		if ev.Kind == provider.SEEnd {
			final = msg
		}
	}
	return lines, final, nil
}

// Verify writes a case's outputs into a scratch directory with write and compares them with the case's expected files by the rules every binding shares, or with update accepts them; with POTATOS_CONFORM_OUT set, the outputs are also kept under that directory for the conform command.
func Verify(path string, update bool, write func(out string) error) error {
	out, err := os.MkdirTemp("", "conform")
	if err != nil {
		return err
	}
	defer os.RemoveAll(out)
	if err := write(out); err != nil {
		return errors.Join(err, record(path, "actual.fail", err.Error()))
	}
	if err := keep(path, out); err != nil {
		return err
	}
	if update {
		_, err := compare.Update(path, out)
		return err
	}
	if r := compare.Check(path, out); r.Status != compare.Passed {
		return fmt.Errorf("%s: %s", r.Status, r.Detail)
	}
	return nil
}

// record keeps a one-line actual.skip or actual.fail for a case under POTATOS_CONFORM_OUT, so conform reports why it has no outputs.
func record(path, name, reason string) error {
	if os.Getenv("POTATOS_CONFORM_OUT") == "" {
		return nil
	}
	out, err := os.MkdirTemp("", "conform")
	if err != nil {
		return err
	}
	defer os.RemoveAll(out)
	if err := os.WriteFile(filepath.Join(out, name), []byte(reason+"\n"), 0o644); err != nil {
		return err
	}
	return keep(path, out)
}

// errUnsupported skips a case whose spec_version this binding does not implement.
var errUnsupported = errors.New("spec_version not supported")

// supports reports whether a case for version, MAJOR.MINOR, holds for the embedded spec: before 1.0 every minor version may break, so it must match, and from 1.0 an older minor of the same major still holds.
func supports(version string) bool {
	have := strings.Split(strings.TrimSpace(spec.MustText("VERSION")), ".")
	want := strings.Split(version, ".")
	if len(want) != 2 || len(have) < 2 || want[0] != have[0] {
		return false
	}
	if have[0] == "0" {
		return want[1] == have[1]
	}
	w, err := strconv.Atoi(want[1])
	h, err2 := strconv.Atoi(have[1])
	return err == nil && err2 == nil && w <= h
}

// keep copies a case's outputs to its id under POTATOS_CONFORM_OUT, the case's path below conformance/ without an error case's .json.
func keep(path, out string) error {
	root := os.Getenv("POTATOS_CONFORM_OUT")
	if root == "" {
		return nil
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	abs = filepath.ToSlash(abs)
	i := strings.LastIndex(abs, "/conformance/")
	if i < 0 {
		return fmt.Errorf("%s is not under conformance/", path)
	}
	dst := filepath.Join(root, filepath.FromSlash(strings.TrimSuffix(abs[i+len("/conformance/"):], ".json")))
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	return os.CopyFS(dst, os.DirFS(out))
}

// writeJSON writes v as one output file of a case.
func writeJSON(out, name string, v any) error {
	b, err := compare.Pretty(v)
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(out, name), b, 0o644)
}

// Cases lists the case directories under dir in name order.
func Cases(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if e.IsDir() {
			out = append(out, filepath.Join(dir, e.Name()))
		}
	}
	sort.Strings(out)
	return out, nil
}

// DecodeResult is what a layer-1 decode case produces.
type DecodeResult struct {
	Stream  []json.RawMessage
	Message json.RawMessage
}

// RunDecode replays response.sse (and headers.json) through the real adapter; request.json, when present, is the request the stream answers.
func RunDecode(ctx context.Context, dir string, f Factory, chunkSeed uint64) (DecodeResult, error) {
	var meta Meta
	if err := readJSON(filepath.Join(dir, "meta.json"), &meta); err != nil {
		return DecodeResult{}, err
	}
	req := &provider.Request{Model: meta.Model, Messages: []message.Message{message.UserText("hi")}}
	if err := readJSON(filepath.Join(dir, "request.json"), req); err != nil && !errors.Is(err, os.ErrNotExist) {
		return DecodeResult{}, err
	}
	resp, err := providertest.LoadResponse(dir)
	if err != nil {
		return DecodeResult{}, err
	}
	ft := providertest.NewFixtureTransport(resp)
	ft.ChunkSeed = chunkSeed
	lines, final, err := Collect(ctx, f(ft), req)
	return DecodeResult{Stream: lines, Message: final}, err
}

// RenderResult is what a layer-1 render case produces: the body and the headers that matter, or the local error, plus notices.
type RenderResult struct {
	Body    json.RawMessage
	Headers map[string]string
	Notices []json.RawMessage
	Error   json.RawMessage
}

// comparedHeaders are the request headers a render case checks; credentials and transport headers are left out.
var comparedHeaders = []string{"anthropic-beta", "anthropic-version", "x-test"}

// RunRender streams input.json against a canned reply and captures what the adapter sent.
func RunRender(ctx context.Context, dir string, f Factory, reply providertest.Response) (RenderResult, error) {
	req := &provider.Request{}
	if err := readJSON(filepath.Join(dir, "input.json"), req); err != nil {
		return RenderResult{}, err
	}
	ft := providertest.NewFixtureTransport(reply)
	var res RenderResult
	for ev, err := range f(ft).Stream(ctx, req) {
		if err != nil {
			if reqs := ft.Requests(); len(reqs) == 0 {
				res.Error, _ = json.Marshal(provider.AsError(err))
			}
			break
		}
		if ev.Kind == provider.SENotice {
			b, _ := json.Marshal(ev.Notice)
			res.Notices = append(res.Notices, b)
		}
	}
	if reqs := ft.Requests(); len(reqs) > 0 {
		res.Body = reqs[0].Body
		res.Headers = pick(reqs[0].Header)
	}
	return res, nil
}

func pick(h http.Header) map[string]string {
	out := map[string]string{}
	for _, k := range comparedHeaders {
		if v := h.Get(k); v != "" {
			out[k] = v
		}
	}
	return out
}

// ErrorCase is one errors/<api>/<case>.json: an HTTP error response and the classified error it must produce.
type ErrorCase struct {
	Status   int               `json:"status"`
	Headers  map[string]string `json:"headers,omitempty"`
	Body     json.RawMessage   `json:"body"` // a JSON document, or a JSON string holding a non-JSON body
	Expected json.RawMessage   `json:"expected,omitempty"`
}

// RunError sends one request whose reply is the case's error response and returns the classified error.
func RunError(ctx context.Context, c ErrorCase, model string, f Factory) (json.RawMessage, error) {
	body := []byte(c.Body)
	var s string
	if json.Unmarshal(c.Body, &s) == nil {
		body = []byte(s)
	}
	h := http.Header{}
	for k, v := range c.Headers {
		h.Set(k, v)
	}
	ft := providertest.NewFixtureTransport(providertest.Response{Status: c.Status, Header: h, Body: body})
	req := &provider.Request{Model: model, Messages: []message.Message{message.UserText("hi")}}
	for _, err := range f(ft).Stream(ctx, req) {
		if err != nil {
			return json.Marshal(provider.AsError(err))
		}
	}
	return nil, fmt.Errorf("status %d produced no error", c.Status)
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}
