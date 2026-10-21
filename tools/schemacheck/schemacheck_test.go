package schemacheck

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/message"
)

const base = "https://github.com/estonshi/aperture-potatos/spec/schema/"

var (
	specDir = filepath.Join("..", "..", "spec", "schema")
	extDir  = filepath.Join("..", "..", "spec", "ext")
)

func compiler(t *testing.T) *jsonschema.Compiler {
	t.Helper()
	c := jsonschema.NewCompiler()
	c.AssertFormat()
	c.AssertContent()
	files, err := filepath.Glob(filepath.Join(specDir, "*.schema.json"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no schemas in %s: %v", specDir, err)
	}
	ext, _ := filepath.Glob(filepath.Join(extDir, "*", "*.schema.json"))
	for _, f := range append(files, ext...) {
		doc := decode(t, readFile(t, f))
		if err := c.AddResource(schemaURL(f), doc); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
	return c
}

// schemaURL is a schema file's $id, which relative $refs between kernel and extension schemas resolve against.
func schemaURL(file string) string {
	if dir := filepath.Dir(file); filepath.Base(filepath.Dir(dir)) == "ext" {
		return strings.TrimSuffix(base, "schema/") + "ext/" + filepath.Base(dir) + "/" + filepath.Base(file)
	}
	return base + filepath.Base(file)
}

// schemaFor compiles the target an example names: <stem>/<def>.<case>.json checks against $defs/<def>, and def "root" against the whole schema; a stem with a slash, ext/<x>/<stem>, names an extension's schema.
func schemaFor(t *testing.T, c *jsonschema.Compiler, stem, def string) *jsonschema.Schema {
	t.Helper()
	loc := base + stem + ".schema.json"
	if strings.Contains(stem, "/") {
		loc = strings.TrimSuffix(base, "schema/") + stem + ".schema.json"
	}
	if def != "root" {
		loc += "#/$defs/" + def
	}
	sch, err := c.Compile(loc)
	if err != nil {
		t.Fatalf("compile %s: %v", loc, err)
	}
	return sch
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func decode(t *testing.T, b []byte) any {
	t.Helper()
	v, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("parse %s: %v", b, err)
	}
	return v
}

// docs splits a .jsonl example into lines and returns a .json example whole.
func docs(t *testing.T, path string) [][]byte {
	t.Helper()
	b := readFile(t, path)
	if !strings.HasSuffix(path, ".jsonl") {
		return [][]byte{b}
	}
	var out [][]byte
	sc := bufio.NewScanner(bytes.NewReader(b))
	sc.Buffer(nil, 1<<24)
	for sc.Scan() {
		if len(bytes.TrimSpace(sc.Bytes())) > 0 {
			out = append(out, append([]byte(nil), sc.Bytes()...))
		}
	}
	return out
}

type example struct {
	path, stem, def string
}

func examples(t *testing.T, kind string) []example {
	t.Helper()
	var out []example
	files, _ := filepath.Glob(filepath.Join(specDir, "examples", kind, "*", "*.json*"))
	for _, f := range files {
		def, _, _ := strings.Cut(filepath.Base(f), ".")
		out = append(out, example{path: f, stem: filepath.Base(filepath.Dir(f)), def: def})
	}
	ext, _ := filepath.Glob(filepath.Join(extDir, "*", "examples", kind, "*", "*.json*"))
	for _, f := range ext {
		def, _, _ := strings.Cut(filepath.Base(f), ".")
		x := filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(filepath.Dir(f)))))
		out = append(out, example{path: f, stem: "ext/" + x + "/" + filepath.Base(filepath.Dir(f)), def: def})
	}
	if len(out) == 0 {
		t.Fatalf("no %s examples", kind)
	}
	return out
}

func TestValidExamplesPass(t *testing.T) {
	c := compiler(t)
	for _, ex := range examples(t, "valid") {
		sch := schemaFor(t, c, ex.stem, ex.def)
		for i, d := range docs(t, ex.path) {
			if err := sch.Validate(decode(t, d)); err != nil {
				t.Errorf("%s line %d: %v", ex.path, i+1, err)
			}
		}
	}
}

func TestInvalidExamplesFail(t *testing.T) {
	c := compiler(t)
	for _, ex := range examples(t, "invalid") {
		sch := schemaFor(t, c, ex.stem, ex.def)
		for i, d := range docs(t, ex.path) {
			if err := sch.Validate(decode(t, d)); err == nil {
				t.Errorf("%s line %d: accepted, want rejected", ex.path, i+1)
			}
		}
	}
}

func canonical(t *testing.T, b []byte) any {
	t.Helper()
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		t.Fatalf("decode %s: %v", b, err)
	}
	return v
}

// TestGoReadsBackExamples is the cross-language contract in one direction: every valid example decodes in Go and re-encodes semantically unchanged, and the re-encoding still validates.
func TestGoReadsBackExamples(t *testing.T) {
	c := compiler(t)
	dec := event.NewDecoder()
	for _, ex := range examples(t, "valid") {
		if ex.def != "root" || (ex.stem != "message" && ex.stem != "event") {
			continue
		}
		sch := schemaFor(t, c, ex.stem, "root")
		for i, d := range docs(t, ex.path) {
			var out []byte
			var err error
			if ex.stem == "message" {
				var m message.Message
				if err = json.Unmarshal(d, &m); err == nil {
					out, err = json.Marshal(m)
				}
			} else {
				var ev event.Event
				if ev, err = dec.Decode(d); err == nil {
					out, err = json.Marshal(ev)
				}
			}
			if err != nil {
				t.Errorf("%s line %d: %v", ex.path, i+1, err)
				continue
			}
			if !reflect.DeepEqual(canonical(t, d), canonical(t, out)) {
				t.Errorf("%s line %d changed in a Go round trip\n in %s\nout %s", ex.path, i+1, d, out)
			}
			if err := sch.Validate(decode(t, out)); err != nil {
				t.Errorf("%s line %d: Go output fails the schema: %v", ex.path, i+1, err)
			}
		}
	}
}

// TestGoWrittenLinesValidate builds lines with the Go types, so a field the binding writes but the schema lacks shows up here.
func TestGoWrittenLinesValidate(t *testing.T) {
	sch := schemaFor(t, compiler(t), "message", "root")
	ts := time.Date(2026, 9, 27, 8, 0, 0, 0, time.FixedZone("JST", 9*3600))
	raw := json.RawMessage(`{"type":"thinking","signature":"sig"}`)
	usage := message.Usage{InputTokens: 10, OutputTokens: 5, ReasoningTokens: 2, Estimated: true}
	lines := []message.Message{
		{Role: message.RoleSystem, Origin: message.Origin{Kind: message.OriginSystem}, Content: message.Content{&message.TextBlock{Text: "<identity>…</identity>"}},
			Boundary: &message.Boundary{Reason: "new", Session: &message.SessionMeta{Agent: "Tater", SpecVersion: "0.1.0", Labels: map[string]string{"env": "test"}}},
			Tools:    []message.ToolDef{{Name: "get_weather", Description: "d", InputSchema: json.RawMessage(`{"type":"object"}`)}}},
		message.UserText("hi"),
		{Role: message.RoleUser, Origin: message.Origin{Kind: message.OriginEpochHead}, Content: message.Content{
			&message.ReminderBlock{Source: "pin:plan", Text: "step 2"},
			&message.ImageBlock{MediaType: "image/png", Data: []byte{0x89, 'P', 'N', 'G'}},
			&message.DocumentBlock{MediaType: "application/pdf", URL: "https://example.com/a.pdf", Filename: "a.pdf"},
		}, CopyOf: "msg_01", Batch: "b_01", BatchEnd: true},
		{Role: message.RoleAssistant, Origin: message.Origin{Kind: message.OriginModel}, Content: message.Content{
			&message.ReasoningBlock{Kind: message.ReasoningSummary, Text: "think", Native: &message.Native{API: "anthropic.messages", Raw: raw}},
			&message.RefusalBlock{Text: "no"},
			&message.OpaqueBlock{Kind: "anthropic.messages:fallback", Native: &message.Native{API: "anthropic.messages", Raw: json.RawMessage(`{"type":"fallback"}`)}},
			&message.ToolCallBlock{ID: "toolu_01", Name: "get_weather"},
		}, Producer: &message.Producer{API: "anthropic.messages", Endpoint: "api.anthropic.com", Model: "claude-opus-5-5", ResponseID: "msg_1", RequestID: "req_1", Headers: map[string]string{"x-call": "1"}},
			Usage: &usage, StopReason: "tool_use", Partial: true},
		{Role: message.RoleTool, Origin: message.Origin{Kind: message.OriginTool}, Content: message.Content{&message.ToolResultBlock{
			CallID: "toolu_01", Content: message.Content{&message.TextBlock{Text: "Sunny"}}, Outcome: message.OutcomeSuccess,
			Pin: &message.Pin{Key: "w", Content: "sunny"}, Structured: json.RawMessage(`{"temp":24}`)}},
			Tasks: []message.TaskRef{{ID: "task_01", Kind: "tool_exec", Status: "running", Attempt: 1, Spec: json.RawMessage(`{"kind":"tool_exec","title":"t","input":{}}`)}}},
		message.AssistantText("done"),
	}
	for i := range lines {
		m := &lines[i]
		m.V, m.ID, m.SessionID, m.Seq, m.Epoch, m.CreatedAt = message.Version, "msg_0"+string(rune('1'+i)), "ses_01", int64(i+1), 1, ts
		if m.Role != message.RoleSystem {
			m.TurnID = "turn_01"
		}
		out, err := json.Marshal(m)
		if err != nil {
			t.Fatal(err)
		}
		if err := sch.Validate(decode(t, out)); err != nil {
			t.Errorf("line %d fails the schema: %v\n%s", i+1, err, out)
		}
		var back message.Message
		if err := json.Unmarshal(out, &back); err != nil {
			t.Fatal(err)
		}
		again, _ := json.Marshal(back)
		if !bytes.Equal(out, again) {
			t.Errorf("line %d does not read back unchanged\n%s\n%s", i+1, out, again)
		}
	}
}

// TestConformanceFixturesValidate checks the language-neutral conformance files against the schemas they claim to follow, so other bindings can rely on their shape.
func TestConformanceFixturesValidate(t *testing.T) {
	c := compiler(t)
	root := filepath.Join("..", "..", "conformance")
	errSch := schemaFor(t, c, "provider", "error")
	checked := 0
	for glob, target := range map[string][2]string{
		"render/*/*/input.json":                   {"provider", "request"},
		"provider/*/*/request.json":               {"provider", "request"},
		"render/*/*/expected.error.json":          {"provider", "error"},
		"render/*/*/expected.notices.jsonl":       {"provider", "notice"},
		"provider/*/*/expected.stream.jsonl":      {"provider", "stream_event"},
		"loop/*/responses/*.jsonl":                {"provider", "stream_event"},
		"loop/*/expected.requests/*.request.json": {"provider", "request"},
		"loop/*/expected*.events.jsonl":           {"event", "root"},
		"loop/*/expected*.store.jsonl":            {"message", "root"},
		"store/*/expected.events.jsonl":           {"event", "root"},
		"loop/ext/*/*/expected*.store.jsonl":      {"message", "root"},
		"store/*/expected.store.jsonl":            {"message", "root"},
		"store/*/calls.jsonl":                     {"common", "model_call"},
		"store/*/expected.calls.jsonl":            {"common", "model_call"},
		"loop/*/expected.calls.jsonl":             {"common", "model_call"},
	} {
		files, _ := filepath.Glob(filepath.Join(root, glob))
		def := target[1]
		sch := schemaFor(t, c, target[0], def)
		for _, f := range files {
			checked++
			for i, d := range docs(t, f) {
				v := decode(t, d)
				target := sch
				if m, ok := v.(map[string]any); ok && def == "stream_event" && m["error"] != nil {
					v, target = m["error"], errSch // a stream that fails ends with {"error": …}
				}
				if err := target.Validate(v); err != nil {
					t.Errorf("%s line %d: %v", f, i+1, err)
				}
			}
		}
	}
	extEvents, _ := filepath.Glob(filepath.Join(root, "loop", "ext", "*", "*", "expected*.events.jsonl"))
	for _, f := range extEvents {
		checked++
		x := filepath.Base(filepath.Dir(filepath.Dir(f)))
		sch := schemaFor(t, c, "event", "root")
		if _, err := os.Stat(filepath.Join(extDir, x, "events.schema.json")); err == nil {
			sch = schemaFor(t, c, "ext/"+x+"/events", "root")
		}
		for i, d := range docs(t, f) {
			if err := sch.Validate(decode(t, d)); err != nil {
				t.Errorf("%s line %d: %v", f, i+1, err)
			}
		}
	}
	hooks, _ := filepath.Glob(filepath.Join(root, "*", "*", "expected.hooks.jsonl"))
	for _, f := range hooks {
		checked++
		for i, d := range docs(t, f) {
			var e struct {
				Point string          `json:"point"`
				Input json.RawMessage `json:"input"`
			}
			if err := json.Unmarshal(d, &e); err != nil {
				t.Fatalf("%s line %d: %v", f, i+1, err)
			}
			if err := schemaFor(t, c, "hook", e.Point+"_input").Validate(decode(t, e.Input)); err != nil {
				t.Errorf("%s line %d: %v", f, i+1, err)
			}
		}
	}
	ledgers, _ := filepath.Glob(filepath.Join(root, "*", "*", "expected.ledger.json"))
	for _, f := range ledgers {
		checked++
		var entries []map[string]any
		if err := json.Unmarshal(readFile(t, f), &entries); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		for i, e := range entries {
			stem, def := "common", "turn_calls"
			switch {
			case e["error"] != nil:
				continue // a portable error code, not a ledger document
			case e["priced"] != nil:
				def = "turn_cost"
			case e["total"] != nil:
				stem, def = "command", "usage_report" // a ledger action that reads the usage totals
			}
			if err := schemaFor(t, c, stem, def).Validate(e); err != nil {
				t.Errorf("%s entry %d: %v", f, i+1, err)
			}
		}
	}
	errs, _ := filepath.Glob(filepath.Join(root, "errors", "*", "*.json"))
	if checked+len(errs) < 50 {
		t.Fatalf("only %d conformance files found; is the path right?", checked+len(errs))
	}
	for _, f := range errs {
		v, _ := decode(t, readFile(t, f)).(map[string]any)
		if err := errSch.Validate(v["expected"]); err != nil {
			t.Errorf("%s: %v", f, err)
		}
	}
}
