package compare

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// tree writes files, keyed by slash path, under a fresh directory.
func tree(t *testing.T, files map[string]string) string {
	t.Helper()
	dir := t.TempDir()
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestCheck(t *testing.T) {
	const script = `{"clock":{"start":"2026-09-26T00:00:00.000Z","step_ms":1}}`
	const interleaved = `{"clock":{"interleaved":true}}`
	notice := func(detail string) string {
		return `{"seq":1,"type":"model.notice","data":{"kind":"clamped","detail":"` + detail + `"}}` + "\n"
	}
	group := func(first, second string) string {
		ev := func(seq int, typ, call string) string {
			return `{"seq":` + string(rune('0'+seq)) + `,"turn_id":"turn_01","step":1,"type":"` + typ + `","data":{"call_id":"` + call + `","group":0}}` + "\n"
		}
		return ev(1, "tool.started", first) + ev(2, "tool.started", second) + ev(3, "tool.completed", first) + ev(4, "tool.completed", second)
	}
	for _, tc := range []struct {
		name     string
		kase     map[string]string
		out      map[string]string // nil: no output directory
		want     Status
		inDetail string
	}{
		{"equal", map[string]string{"expected.a.json": `{"x":1,"y":[1,2]}`, "expected.b.jsonl": "{\"k\":1}\n"},
			map[string]string{"actual.a.json": "{\n \"y\": [1, 2], \"x\": 1}", "actual.b.jsonl": `{"k": 1}`}, Passed, ""},
		{"value differs", map[string]string{"expected.b.jsonl": "{\"k\":1}\n{\"k\":2}\n"},
			map[string]string{"actual.b.jsonl": "{\"k\":1}\n{\"k\":3}\n"}, Failed, "expected.b.jsonl line 2: $.k: got 3, want 2"},
		{"extra field", map[string]string{"expected.a.json": `{"x":1}`},
			map[string]string{"actual.a.json": `{"x":1,"y":2}`}, Failed, "expected.a.json: $.y: unexpected, got 2"},
		{"line count", map[string]string{"expected.b.jsonl": "{}\n{}\n"},
			map[string]string{"actual.b.jsonl": "{}\n"}, Failed, "1 lines, want 2"},
		{"missing json", map[string]string{"expected.a.json": `{}`},
			map[string]string{"actual.other.jsonl": ""}, Failed, "produced none"},
		{"unexpected json", map[string]string{"expected.a.json": `{}`},
			map[string]string{"actual.a.json": `{}`, "actual.b.json": `{}`}, Failed, "expected.b.json: missing, the case produced actual.b.json"},
		{"absent jsonl is empty", map[string]string{"expected.notices.jsonl": ""},
			map[string]string{"actual.other.jsonl": ""}, Passed, ""},
		{"durations zeroed", map[string]string{"expected.a.json": `{"took_ms":12,"d":{"wait_ms":-1}}`},
			map[string]string{"actual.a.json": `{"took_ms":3,"d":{"wait_ms":40}}`}, Passed, ""},
		{"duration of another type", map[string]string{"expected.a.json": `{"took_ms":12}`},
			map[string]string{"actual.a.json": `{"took_ms":"12"}`}, Failed, "expected.a.json"},
		{"numbers by value", map[string]string{"expected.a.json": `{"n":1000,"f":0.5,"took_ms":3}`},
			map[string]string{"actual.a.json": `{"n":1e3,"f":5.0E-1,"took_ms":1.0}`}, Passed, ""},
		{"fractional duration", map[string]string{"expected.a.json": `{"took_ms":12}`},
			map[string]string{"actual.a.json": `{"took_ms":1.5}`}, Failed, "$.took_ms"},
		{"skip", map[string]string{"expected.a.json": `{}`},
			map[string]string{"actual.skip": "spec_version 0.2 unsupported\n"}, Skipped, "spec_version 0.2 unsupported"},
		{"runner failure", map[string]string{"expected.a.json": `{}`},
			map[string]string{"actual.fail": "I1: call toolu_1 has two results"}, Failed, "runner: I1"},
		{"not run", map[string]string{"expected.a.json": `{}`}, nil, NotRun, ""},
		{"requests directory", map[string]string{"expected.requests/01.body.json": `{"a":1}`, "expected.requests/02.body.json": `{"a":2}`},
			map[string]string{"actual.requests/01.body.json": `{"a":1}`}, Failed, "02.body.json: the case produced none"},
		{"notice detail in a loop case", map[string]string{"script.json": script, "expected.events.jsonl": notice("cut to 4096")},
			map[string]string{"actual.events.jsonl": notice("clamped to 4096")}, Passed, ""},
		{"notice detail elsewhere", map[string]string{"expected.events.jsonl": notice("cut to 4096")},
			map[string]string{"actual.events.jsonl": notice("clamped to 4096")}, Failed, "line 1"},
		{"parallel group order", map[string]string{"script.json": script, "expected.events.jsonl": group("call_a", "call_b")},
			map[string]string{"actual.events.jsonl": group("call_b", "call_a")}, Passed, ""},
		{"times in an interleaved case", map[string]string{"script.json": interleaved, "expected.store.jsonl": "{\"seq\":1,\"ts\":\"2026-09-26T00:00:00.001Z\"}\n", "expected.ledger.json": `[{"started_at":""}]`},
			map[string]string{"actual.store.jsonl": "{\"seq\":1,\"ts\":\"2026-09-26T00:00:00.007Z\"}\n", "actual.ledger.json": `[{"started_at":"2026-09-26T00:00:00.002Z"}]`}, Passed, ""},
		{"times elsewhere", map[string]string{"script.json": script, "expected.store.jsonl": "{\"seq\":1,\"ts\":\"2026-09-26T00:00:00.001Z\"}\n"},
			map[string]string{"actual.store.jsonl": "{\"seq\":1,\"ts\":\"2026-09-26T00:00:00.007Z\"}\n"}, Failed, "line 1"},
		{"message envelope", map[string]string{"expected.message.json": `{"role":"assistant"}`, "expected.stream.jsonl": "{\"kind\":\"end\",\"message\":{\"role\":\"assistant\"}}\n"},
			map[string]string{"actual.message.json": `{"v":1,"id":"msg_1","role":"assistant"}`, "actual.stream.jsonl": `{"kind":"end","message":{"seq":3,"role":"assistant"}}`}, Passed, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := tree(t, tc.kase)
			out := filepath.Join(t.TempDir(), "absent")
			if tc.out != nil {
				out = tree(t, tc.out)
			}
			r := Check(dir, out)
			if r.Status != tc.want || !strings.Contains(r.Detail, tc.inDetail) {
				t.Fatalf("got %s %q, want %s with %q", r.Status, r.Detail, tc.want, tc.inDetail)
			}
		})
	}
}

func TestCheckErrorCase(t *testing.T) {
	dir := tree(t, map[string]string{"errors/x.api/auth_401.json": `{"status":401,"body":{},"expected":{"class":"auth","status":401}}`})
	file := filepath.Join(dir, "errors", "x.api", "auth_401.json")
	if r := Check(file, tree(t, map[string]string{"actual.json": `{"status":401,"class":"auth"}`})); r.Status != Passed {
		t.Fatalf("got %s %s", r.Status, r.Detail)
	}
	if r := Check(file, tree(t, map[string]string{"actual.json": `{"class":"rate_limited","status":401}`})); r.Status != Failed {
		t.Fatalf("a different class passed")
	}
}

func TestCheckCacheControlLimit(t *testing.T) {
	cc := `{"cache_control":{"type":"ephemeral"}}`
	body := `{"system":[` + strings.Repeat(cc+",", 4) + cc + `]}`
	root := tree(t, map[string]string{"render/anthropic.messages/many/expected.body.json": body, "render/openai.chat/many/expected.body.json": body})
	out := tree(t, map[string]string{"actual.body.json": body})
	if r := Check(filepath.Join(root, "render", "anthropic.messages", "many"), out); r.Status != Failed || !strings.Contains(r.Detail, "5 cache_control") {
		t.Fatalf("got %s %q, want the breakpoint limit to fail", r.Status, r.Detail)
	}
	if r := Check(filepath.Join(root, "render", "openai.chat", "many"), out); r.Status != Passed {
		t.Fatalf("the limit applied to another API: %s", r.Detail)
	}
	if _, err := Update(filepath.Join(root, "render", "anthropic.messages", "many"), out); err == nil || !strings.Contains(err.Error(), "5 cache_control") {
		t.Fatalf("update accepted a body over the limit: %v", err)
	}
}

func TestUpdate(t *testing.T) {
	dir := tree(t, map[string]string{
		"expected.same.json":             "{ \"b\": 1,\n\"a\": 2 }", // equal but formatted differently: left alone
		"expected.changed.jsonl":         "{\"k\":1}\n",
		"expected.gone.json":             `{}`,
		"expected.requests/01.body.json": `{}`,
		"script.json":                    `{}`,
	})
	out := tree(t, map[string]string{
		"actual.same.json":      `{"a":2,"b":1}`,
		"actual.changed.jsonl":  "{\"k\": 2, \"a\": \"<x>\"}\n",
		"actual.new.json":       `{"z":1,"a":[1]}`,
		"actual.notices.jsonl":  "",
		"actual.took_ms.json":   `{"took_ms":5}`,
		"actual.unrelated.json": `{"took_ms":7}`,
	})
	if err := os.WriteFile(filepath.Join(dir, "expected.took_ms.json"), []byte(`{"took_ms":1}`), 0o644); err != nil {
		t.Fatal(err)
	}
	changed, err := Update(dir, out)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range changed {
		r, _ := filepath.Rel(dir, f)
		names = append(names, filepath.ToSlash(r))
	}
	slices.Sort(names)
	if want := []string{"expected.changed.jsonl", "expected.gone.json", "expected.new.json", "expected.requests/01.body.json", "expected.unrelated.json"}; !slices.Equal(names, want) {
		t.Fatalf("changed %v, want %v", names, want)
	}
	read := func(name string) string {
		b, err := os.ReadFile(filepath.Join(dir, name))
		if err != nil {
			return "<none>"
		}
		return string(b)
	}
	for name, want := range map[string]string{
		"expected.same.json":     "{ \"b\": 1,\n\"a\": 2 }",
		"expected.took_ms.json":  `{"took_ms":1}`,
		"expected.changed.jsonl": "{\"a\":\"<x>\",\"k\":2}\n",
		"expected.new.json":      "{\n  \"a\": [\n    1\n  ],\n  \"z\": 1\n}\n",
		"expected.gone.json":     "<none>",
		"expected.notices.jsonl": "<none>",
	} {
		if got := read(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "expected.requests")); err == nil {
		t.Error("the emptied expected.requests directory was kept")
	}
	if r := Check(dir, out); r.Status != Passed {
		t.Fatalf("after update: %s %s", r.Status, r.Detail)
	}
}

func TestUpdateErrorCase(t *testing.T) {
	dir := tree(t, map[string]string{"auth_401.json": `{"status":401,"body":{"error":"no"}}`})
	file := filepath.Join(dir, "auth_401.json")
	if _, err := Update(file, tree(t, map[string]string{"actual.json": `{"class":"auth"}`})); err != nil {
		t.Fatal(err)
	}
	if r := Check(file, tree(t, map[string]string{"actual.json": `{"class":"auth"}`})); r.Status != Passed {
		t.Fatalf("after update: %s %s", r.Status, r.Detail)
	}
	b, _ := os.ReadFile(file)
	if !strings.Contains(string(b), `"status": 401`) {
		t.Fatalf("the case's other fields were lost:\n%s", b)
	}
}

func TestUpdateRefusesRunnerFailure(t *testing.T) {
	dir := tree(t, map[string]string{"expected.a.json": `{}`})
	if _, err := Update(dir, tree(t, map[string]string{"actual.fail": "I4: line 3 differs"})); err == nil {
		t.Fatal("accepted the outputs of a failed run")
	}
}

func TestCases(t *testing.T) {
	root := tree(t, map[string]string{
		"loop/simple/script.json":                     `{}`,
		"loop/simple/expected.events.jsonl":           "",
		"loop/simple/expected.requests/01.body.json":  `{}`,
		"loop/ext/x/case/expected.store.jsonl":        "",
		"loop/fresh/script.json":                      `{}`,
		"errors/a.api/auth_401.json":                  `{}`,
		"errors/a.api/new_429.json":                   `{}`,
		"render/a.api/basic/input.json":               `{}`,
		"render/a.api/basic/expected.notices.jsonl":   "",
		"render/a.api/basic/expected.requests/x.json": `{}`,
	})
	out := tree(t, map[string]string{"loop/fresh/actual.events.jsonl": "", "loop/simple/actual.events.jsonl": ""})
	cases, err := Cases(root, out)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, c := range cases {
		ids = append(ids, c.ID)
		if want := filepath.Join(out, filepath.FromSlash(c.ID)); c.Out != want {
			t.Errorf("%s: out %s, want %s", c.ID, c.Out, want)
		}
	}
	if want := []string{"errors/a.api/auth_401", "errors/a.api/new_429", "loop/ext/x/case", "loop/fresh", "loop/simple", "render/a.api/basic"}; !slices.Equal(ids, want) {
		t.Fatalf("cases %v, want %v", ids, want)
	}
	if !strings.HasSuffix(cases[0].Path, filepath.FromSlash("errors/a.api/auth_401.json")) {
		t.Errorf("error case path %s", cases[0].Path)
	}
	one, err := Cases(filepath.Join(root, "loop", "simple"), filepath.Join(out, "loop", "simple"))
	if err != nil || len(one) != 1 || one[0].ID != "." || one[0].Out != filepath.Join(out, "loop", "simple") {
		t.Fatalf("one case: %+v %v", one, err)
	}
}
