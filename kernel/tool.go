package kernel

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/estonshi/aperture-potatos/message"
)

// Concurrency says whether a call may overlap its neighbours in one batch of calls.
type Concurrency string

const (
	Parallel Concurrency = "parallel" // may run with adjacent parallel calls (read-only tools)
	Serial   Concurrency = "serial"   // runs alone, keeping order relative to other calls (side effects)
)

// ToolSpec is a tool's definition for the model and its execution policy for the kernel.
type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
	Strict      bool            `json:"strict,omitempty"`     // sent only when the schema meets strict mode, otherwise dropped with a notice
	Concurrency Concurrency     `json:"concurrency"`          // "" is Serial
	Timeout     time.Duration   `json:"-"`                    // 0 uses Limits.ToolTimeout; any negative value is NoTimeout
	Tags        []string        `json:"tags,omitempty"`       // for the host and extensions; the kernel does not read them
	Background  bool            `json:"background,omitempty"` // lets the model run this tool as a tool_exec task instead of waiting for it
	Deferred    bool            `json:"deferred,omitempty"`   // loaded on demand when the request carries a tool search; an ordinary definition otherwise

	taskKind string // set by TaskTool, so a restart is reviewed under the name of the kind's start tool
}

// MarshalJSON writes the timeout as timeout_ms, -1 for any negative value, so the wire never carries nanoseconds.
func (s ToolSpec) MarshalJSON() ([]byte, error) {
	type plain ToolSpec
	v := struct {
		plain
		TimeoutMS int64 `json:"timeout_ms,omitempty"`
	}{plain: plain(s), TimeoutMS: s.Timeout.Milliseconds()}
	if s.Timeout < 0 {
		v.TimeoutMS = -1
	}
	return json.Marshal(v)
}

// UnmarshalJSON reads timeout_ms back; any negative value is NoTimeout.
func (s *ToolSpec) UnmarshalJSON(b []byte) error {
	type plain ToolSpec
	var v struct {
		plain
		TimeoutMS int64 `json:"timeout_ms"`
	}
	if err := json.Unmarshal(b, &v); err != nil {
		return err
	}
	*s = ToolSpec(v.plain)
	s.Timeout = time.Duration(v.TimeoutMS) * time.Millisecond
	if v.TimeoutMS < 0 {
		s.Timeout = NoTimeout
	}
	return nil
}

// NoTimeout is the canonical negative Timeout, which disables the per-call timeout for tools that wait on a person or another agent; interrupt, stop and close still cancel them.
const NoTimeout time.Duration = -1

// Tool is a function the model can call; Call's error reaches the model as an error result, so write it for the model.
type Tool interface {
	Spec() ToolSpec // called at every freeze point, so enums may be dynamic
	Call(ctx context.Context, tc *ToolContext, input json.RawMessage) (ToolResult, error)
}

// ToolResult is what a tool returns; IsError marks a failure the model should see as one.
type ToolResult struct {
	Content    message.Content `json:"content,omitempty"` // shown to the model: text / image / document
	IsError    bool            `json:"is_error,omitempty"`
	Structured any             `json:"structured,omitempty"` // machine-readable output for events and hooks; never sent to the model
	Pin        *Pin            `json:"pin,omitempty"`        // content the kernel re-injects after compaction
}

// Pin is an alias, so what a tool declares and what the session file stores are one definition.
type Pin = message.Pin

// TextResult is a successful result holding one text block.
func TextResult(s string) ToolResult {
	return ToolResult{Content: message.Content{&message.TextBlock{Text: s}}}
}

// ErrorResult is an error result with a formatted message, for a failure the model can act on.
func ErrorResult(format string, args ...any) ToolResult {
	return ToolResult{Content: message.Content{&message.TextBlock{Text: fmt.Sprintf(format, args...)}}, IsError: true}
}

// InputError is what NewTool returns when the arguments do not decode; the kernel turns it into an invalid_input result without calling the tool.
type InputError struct {
	Detail string
}

// Error includes the decoding detail.
func (e *InputError) Error() string { return "invalid tool input: " + e.Detail }

// NewTool decodes strictly (keys must match the json tags exactly, unknown and repeated keys are rejected) so fn acts on exactly the fields rules and reviewers saw.
func NewTool[In any](spec ToolSpec, fn func(ctx context.Context, tc *ToolContext, in In) (ToolResult, error)) Tool {
	return &typedTool[In]{spec: spec, fn: fn}
}

type typedTool[In any] struct {
	spec ToolSpec
	fn   func(context.Context, *ToolContext, In) (ToolResult, error)
}

func (t *typedTool[In]) Spec() ToolSpec { return t.spec }

func (t *typedTool[In]) Call(ctx context.Context, tc *ToolContext, input json.RawMessage) (ToolResult, error) {
	var in In
	if err := decodeStrict(input, &in); err != nil {
		return ToolResult{}, err
	}
	return t.fn(ctx, tc, in)
}

func decodeStrict(data []byte, v any) error {
	if err := checkKeys(data, reflect.TypeOf(v).Elem(), ""); err != nil {
		return err
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(v); err != nil {
		var te *json.UnmarshalTypeError
		if errors.As(err, &te) {
			return &InputError{Detail: fmt.Sprintf("field %q must be %s, got %s", te.Field, jsonKind(te.Type), te.Value)}
		}
		return &InputError{Detail: "the arguments are not valid JSON"}
	}
	return nil
}

// checkKeys walks the value against t, because encoding/json matches keys case-insensitively and keeps the last of repeated keys.
func checkKeys(data []byte, t reflect.Type, path string) error {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 || string(trimmed) == "null" {
		return nil
	}
	switch t.Kind() {
	case reflect.Struct:
		if t == reflect.TypeFor[time.Time]() || trimmed[0] != '{' {
			return nil // type mismatches are reported by the decoder
		}
		fields := jsonFields(t)
		keys, values, err := objectEntries(trimmed)
		if err != nil {
			return &InputError{Detail: "the arguments are not valid JSON"}
		}
		seen := map[string]bool{}
		for i, k := range keys {
			if seen[k] {
				return &InputError{Detail: fmt.Sprintf("field %q appears more than once", join(path, k))}
			}
			seen[k] = true
			ft, ok := fields[k]
			if !ok {
				return &InputError{Detail: fmt.Sprintf("unknown field %q", join(path, k))}
			}
			if err := checkKeys(values[i], ft, join(path, k)); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		if trimmed[0] != '[' || t.Elem().Kind() == reflect.Uint8 {
			return nil
		}
		var items []json.RawMessage
		if json.Unmarshal(trimmed, &items) != nil {
			return nil
		}
		for i, it := range items {
			if err := checkKeys(it, t.Elem(), fmt.Sprintf("%s[%d]", path, i)); err != nil {
				return err
			}
		}
	case reflect.Map:
		if trimmed[0] != '{' {
			return nil
		}
		keys, values, err := objectEntries(trimmed)
		if err != nil {
			return &InputError{Detail: "the arguments are not valid JSON"}
		}
		for i, k := range keys {
			if err := checkKeys(values[i], t.Elem(), join(path, k)); err != nil {
				return err
			}
		}
	}
	return nil
}

func join(path, key string) string {
	if path == "" {
		return key
	}
	return path + "." + key
}

// objectEntries lists an object's keys in order, repeats included, which map decoding would hide.
func objectEntries(data []byte) ([]string, []json.RawMessage, error) {
	d := json.NewDecoder(bytes.NewReader(data))
	if tok, err := d.Token(); err != nil {
		return nil, nil, err
	} else if tok != json.Delim('{') {
		return nil, nil, fmt.Errorf("got %v, want an object", tok)
	}
	var keys []string
	var values []json.RawMessage
	for d.More() {
		tok, err := d.Token()
		if err != nil {
			return nil, nil, err
		}
		k, _ := tok.(string)
		var v json.RawMessage
		if err := d.Decode(&v); err != nil {
			return nil, nil, err
		}
		keys = append(keys, k)
		values = append(values, v)
	}
	return keys, values, nil
}

// jsonFields maps each exact JSON key of a struct, embedded structs flattened, to its field type.
func jsonFields(t reflect.Type) map[string]reflect.Type {
	out := map[string]reflect.Type{}
	for i := range t.NumField() {
		f := t.Field(i)
		tag := f.Tag.Get("json")
		if tag == "-" {
			continue
		}
		name, _, _ := strings.Cut(tag, ",")
		if f.Anonymous && name == "" {
			ft := f.Type
			if ft.Kind() == reflect.Pointer {
				ft = ft.Elem()
			}
			if ft.Kind() == reflect.Struct {
				for k, v := range jsonFields(ft) {
					if _, ok := out[k]; !ok {
						out[k] = v
					}
				}
				continue
			}
		}
		if !f.IsExported() {
			continue
		}
		if name == "" {
			name = f.Name
		}
		out[name] = f.Type
	}
	return out
}

func jsonKind(t reflect.Type) string {
	for t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	switch t.Kind() {
	case reflect.Bool:
		return "a boolean"
	case reflect.String:
		return "a string"
	case reflect.Slice, reflect.Array:
		return "an array"
	case reflect.Struct, reflect.Map:
		return "an object"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64, reflect.Float32, reflect.Float64:
		return "a number"
	}
	return "a different type"
}
