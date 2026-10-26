package kernel

import (
	"encoding/json"
	"strings"
	"testing"
)

func mustRules(t *testing.T, src string) []compiledRule {
	t.Helper()
	rules, err := ParseApprovalRules([]byte(src))
	if err != nil {
		t.Fatal(err)
	}
	c, err := compileRules(rules)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestRuleDecide(t *testing.T) {
	rules := mustRules(t, `[
	  {"id": "no-drop", "tool": "sql_exec", "when": {"statement": {"regex": "\\s*[dD][rR][oO][pP]\\b[\\s\\S]*"}}, "action": "deny", "reason": "no DROP"},
	  {"id": "prod-writes", "tool": "sql_exec", "when": {"database": {"eq": "prod"}}, "action": "ask"},
	  {"id": "prod-read-only", "tool": "sql_exec", "when": {"database": {"eq": "prod"}, "read_only": {"eq": true}}, "action": "allow"},
	  {"id": "http-mutations", "tool": "http_*", "when": {"method": {"in": ["POST", "PUT", "DELETE"]}}, "action": "ask"},
	  {"id": "system-files", "tool": "write_file", "when": {"files.*.path": {"prefix": "/etc/", "normalize": "path"}}, "action": "deny"},
	  {"id": "first-file", "tool": "write_file", "when": {"files.0.mode": {"glob": "07?7"}}, "action": "ask"},
	  {"id": "limits", "tool": "fetch", "when": {"limit": {"eq": 1e2}}, "action": "deny"},
	  {"id": "shape", "tool": "fetch", "when": {"opts": {"eq": {"b": [1, 2], "a": null}}}, "action": "ask"},
	  {"id": "no-token", "tool": "call_api", "when": {"token": {"exists": false}}, "action": "ask"},
	  {"id": "everything", "tool": "*", "when": {"danger": {"exists": true}}, "action": "deny"}
	]`)
	for _, c := range []struct {
		tool, input string
		want        RuleAction
		deny        string
	}{
		{"sql_exec", `{"database":"dev","statement":"  drop table x"}`, RuleDeny, "no-drop"},
		{"sql_exec", `{"database":"prod","statement":"UPDATE t"}`, RuleAsk, ""},
		{"sql_exec", `{"database":"prod","statement":"SELECT 1","read_only":true}`, RuleAllow, ""},
		{"sql_exec", `{"database":"prod","statement":"DROP TABLE t","read_only":true}`, RuleDeny, "no-drop"}, // allow never lifts a deny
		{"sql_exec", `{"database":"dev","statement":"SELECT 1"}`, "", ""},
		{"sql_exec", `{"database":"prod","statement":"SELECT 1","read_only":"yes"}`, RuleAsk, ""}, // eq is typed
		{"http_post", `{"method":"POST"}`, RuleAsk, ""},
		{"http_get", `{"method":"GET"}`, "", ""},
		{"https_post", `{"method":"POST"}`, "", ""},
		{"write_file", `{"files":[{"path":"/tmp/a"},{"path":"/tmp/../etc/passwd"}]}`, RuleDeny, "system-files"},
		{"write_file", `{"files":[{"path":"/etcetera"}]}`, "", ""},
		{"write_file", `{"files":[]}`, "", ""},
		{"write_file", `{"files":{"0":{"path":"/etc/x"}}}`, "", ""}, // * needs an array
		{"write_file", `{"files":[{"path":"/tmp/a","mode":"0757"}]}`, RuleAsk, ""},
		{"write_file", `{"files":[{"path":"/tmp/a"},{"mode":"0757"}]}`, "", ""},
		{"fetch", `{"limit":100.0}`, RuleDeny, "limits"},
		{"fetch", `{"limit":"100"}`, "", ""},
		{"fetch", `{"opts":{"a":null,"b":[1,2.0]}}`, RuleAsk, ""},
		{"fetch", `{"opts":{"a":null,"b":[2,1]}}`, "", ""},
		{"call_api", `{}`, RuleAsk, ""},
		{"call_api", `{"token":null}`, "", ""},
		{"anything", `{"danger":0}`, RuleDeny, "everything"},
	} {
		v := decide(rules, ToolCallView{Name: c.tool, Input: json.RawMessage(c.input)})
		deny := ""
		if v.deny != nil {
			deny = v.deny.ID
		}
		if v.action != c.want || deny != c.deny {
			t.Errorf("%s %s: got %q (deny %q), want %q (deny %q)", c.tool, c.input, v.action, deny, c.want, c.deny)
		}
	}
}

// TestRulePredicate: the native predicate runs only after tool and when match, the three are ANDed, and a panic resolves toward the stricter action.
func TestRulePredicate(t *testing.T) {
	boom := func(ToolCallView) bool { panic("boom") }
	calls := 0
	rules, err := compileRules([]ApprovalRule{
		{ID: "deny-boom", Tool: "write_file", When: map[string]Condition{"path": {Prefix: "/etc"}}, Action: RuleDeny, Match: boom},
		{ID: "ask-boom", Tool: "write_file", When: map[string]Condition{"path": {Prefix: "/etc"}}, Action: RuleAsk, Match: boom},
		{ID: "allow-boom", Tool: "write_file", When: map[string]Condition{"path": {Prefix: "/etc"}}, Action: RuleAllow, Match: boom},
		{ID: "runs", Tool: "write_file", When: map[string]Condition{"path": {Prefix: "/etc"}}, Action: RuleAsk,
			Match: func(ToolCallView) bool { calls++; return true }},
	})
	if err != nil {
		t.Fatal(err)
	}
	v := decide(rules, ToolCallView{Name: "write_file", Input: json.RawMessage(`{"path":"/etc/passwd"}`)})
	if v.action != RuleDeny || v.deny == nil || v.deny.ID != "deny-boom" {
		t.Fatalf("got %q (deny %v)", v.action, v.deny)
	}
	if len(v.failures) != 3 || v.failures[0].id != "deny-boom" || v.failures[2].action != RuleAllow {
		t.Fatalf("failures %+v", v.failures)
	}
	if calls != 1 {
		t.Fatalf("predicate calls %d", calls)
	}
	if v := decide(rules, ToolCallView{Name: "write_file", Input: json.RawMessage(`{"path":"/tmp/x"}`)}); v.action != "" || calls != 1 {
		t.Fatalf("a miss ran the predicate: %q, calls %d", v.action, calls)
	}
	if v := decide(rules, ToolCallView{Name: "read_file", Input: json.RawMessage(`{"path":"/etc/passwd"}`)}); v.action != "" || calls != 1 {
		t.Fatalf("another tool ran the predicate: %q, calls %d", v.action, calls)
	}
}

// TestApprovalRuleSerialization: a rule carrying a native predicate cannot be written, and a rule file cannot name one.
func TestApprovalRuleSerialization(t *testing.T) {
	withPredicate := ApprovalRule{ID: "x", Tool: "*", Action: RuleDeny, Match: func(ToolCallView) bool { return true }}
	if _, err := json.Marshal(withPredicate); err == nil || !strings.Contains(err.Error(), "native predicate") {
		t.Fatalf("marshal with predicate: %v", err)
	}
	plain := ApprovalRule{ID: "x", Tool: "*", Action: RuleDeny}
	b, err := json.Marshal(plain)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "match") {
		t.Fatalf("marshal wrote a match field: %s", b)
	}
	if rs, err := ParseApprovalRules(append(append([]byte{'['}, b...), ']')); err != nil || len(rs) != 1 || rs[0].Match != nil {
		t.Fatalf("round trip: %v %+v", err, rs)
	}
	if _, err := ParseApprovalRules([]byte(`[{"id":"x","tool":"*","action":"deny","match":"always"}]`)); err == nil || !strings.Contains(err.Error(), "native predicate") {
		t.Fatalf("parse with match: %v", err)
	}
}

// TestPathPrefix: path prefixes match whole segments, and a relative path is never under an absolute prefix, except that one climbing out with .. is denied.
func TestPathPrefix(t *testing.T) {
	rules := mustRules(t, `[
	  {"id": "system", "tool": "write_file", "when": {"path": {"prefix": "/etc/", "normalize": "path"}}, "action": "deny"},
	  {"id": "workspace", "tool": "write_file", "when": {"path": {"prefix": "/workspace", "normalize": "path"}}, "action": "allow"},
	  {"id": "src", "tool": "write_file", "when": {"path": {"prefix": "src", "normalize": "path"}}, "action": "allow"},
	  {"id": "writes", "tool": "write_file", "action": "ask"}
	]`)
	for _, c := range []struct {
		path string
		want RuleAction
	}{
		{"/workspace", RuleAllow},
		{"/workspace/a.go", RuleAllow},
		{"/workspace2/a.go", RuleAsk},
		{"/workspace-secrets/x", RuleAsk},
		{"/etc", RuleDeny},
		{"/etc/hosts", RuleDeny},
		{"/etcetera", RuleAsk},
		{"../../../etc/hosts", RuleDeny},
		{"..", RuleDeny},
		{"workspace/a.go", RuleAsk},
		{"src/a.go", RuleAllow},
		{"./src/../src/a.go", RuleAllow},
		{"srcx/a.go", RuleAsk},
		{"notes.md", RuleAsk},
	} {
		input, _ := json.Marshal(map[string]string{"path": c.path})
		if v := decide(rules, ToolCallView{Name: "write_file", Input: input}); v.action != c.want {
			t.Errorf("%s: got %q, want %q", c.path, v.action, c.want)
		}
	}
}

func TestRuleMatchFunc(t *testing.T) {
	c, err := compileRules([]ApprovalRule{{ID: "go", Tool: "x", Action: RuleDeny, Match: func(v ToolCallView) bool { return strings.Contains(string(v.Input), "bad") }}})
	if err != nil {
		t.Fatal(err)
	}
	if decide(c, ToolCallView{Name: "x", Input: json.RawMessage(`{"a":"bad"}`)}).action != RuleDeny || decide(c, ToolCallView{Name: "x", Input: json.RawMessage(`{}`)}).action != "" {
		t.Fatal("Match must hold together with tool and when")
	}
}

func TestParseApprovalRulesRejects(t *testing.T) {
	for name, src := range map[string]string{
		"unknown operator":   `[{"id":"a","tool":"x","when":{"f":{"like":"y"}},"action":"deny"}]`,
		"two operators":      `[{"id":"a","tool":"x","when":{"f":{"eq":1,"glob":"y"}},"action":"deny"}]`,
		"no operator":        `[{"id":"a","tool":"x","when":{"f":{}},"action":"deny"}]`,
		"normalize alone":    `[{"id":"a","tool":"x","when":{"f":{"glob":"y","normalize":"path"}},"action":"deny"}]`,
		"normalize unknown":  `[{"id":"a","tool":"x","when":{"f":{"prefix":"/","normalize":"url"}},"action":"deny"}]`,
		"inline flag":        `[{"id":"a","tool":"x","when":{"f":{"regex":"(?i)drop"}},"action":"deny"}]`,
		"anchor":             `[{"id":"a","tool":"x","when":{"f":{"regex":"^drop"}},"action":"deny"}]`,
		"lookahead":          `[{"id":"a","tool":"x","when":{"f":{"regex":"a(?=b)"}},"action":"deny"}]`,
		"backreference":      `[{"id":"a","tool":"x","when":{"f":{"regex":"(a)\\1"}},"action":"deny"}]`,
		"unicode class":      `[{"id":"a","tool":"x","when":{"f":{"regex":"\\p{L}"}},"action":"deny"}]`,
		"empty id":           `[{"id":"","tool":"x","action":"deny"}]`,
		"duplicate id":       `[{"id":"a","tool":"x","action":"deny"},{"id":"a","tool":"y","action":"ask"}]`,
		"unknown action":     `[{"id":"a","tool":"x","action":"maybe"}]`,
		"no tool":            `[{"id":"a","action":"deny"}]`,
		"empty path segment": `[{"id":"a","tool":"x","when":{"a..b":{"exists":true}},"action":"deny"}]`,
		"not an array":       `{"id":"a"}`,
	} {
		if _, err := ParseApprovalRules([]byte(src)); err == nil {
			t.Errorf("%s: accepted %s", name, src)
		}
	}
	if r, err := ParseApprovalRules([]byte(`[{"id":"a","tool":"x","when":{"f":{"eq":null}},"action":"deny"}]`)); err != nil || r[0].When["f"].Eq == nil {
		t.Fatalf("eq null is an operator: %v %+v", err, r)
	}
}

func TestPortableRegex(t *testing.T) {
	for expr, ok := range map[string]bool{
		`abc`: true, `a.c`: true, `[a-z0-9_]+`: true, `[^\s]*`: true, `\d{2,4}`: true, `x{3}`: true, `x{2,}?`: true, `(?:ab|cd)+`: true,
		`(a)(b)`: true, `\bword\b`: true, `\.\*\+\?\(\)\[\]\{\}\|\\\^\$\-`: true, `[\]\[-]`: true, `[\w.-]`: true, `a|`: true, `日本`: true,
		`^a`: false, `a$`: false, `(?i)a`: false, `(?s).`: false, `(?P<n>a)`: false, `(?<n>a)`: false, `a(?!b)`: false, `(?<=a)b`: false,
		`\1`: false, `\p{L}`: false, `[[:alpha:]]`: false, `[]a]`: false, `[^]`: false, `*a`: false, `a**`: false, `a{`: false, `a{2,1}`: false,
		`a{1001}`: false, `\n`: false, `\Qx\E`: false, `[\b]`: false, `[z-a]`: false, `[\d-z]`: false, `(a`: false, `a)`: false, `a}`: false, `\`: false,
	} {
		if _, err := compilePortable(expr); (err == nil) != ok {
			t.Errorf("%q: accepted %v, want %v (%v)", expr, err == nil, ok, err)
		}
	}
	re, _ := compilePortable(`\s+`)
	if re.MatchString("\v") || !re.MatchString("\t\n\f\r ") {
		t.Fatal(`\s is [\t\n\f\r ] in ASCII`)
	}
	re, _ = compilePortable(`.`)
	if re.MatchString("\n") || !re.MatchString("\r") {
		t.Fatal(". matches everything but \\n")
	}
	re, _ = compilePortable(`Edit.*`)
	if re.MatchString("NotebookEdit") {
		t.Fatal("the expression is anchored")
	}
}

func TestNormalizeInput(t *testing.T) {
	for in, want := range map[string]string{
		`{ "b" : 1 , "a" : [ 1, { "c" : "x y" } ] }`: `{"b":1,"a":[1,{"c":"x y"}]}`,
		`{"s":"é\n"}`:               `{"s":"é\n"}`, // escapes stay as sent
		`{"a":{"a":1},"b":{"a":2}}`: `{"a":{"a":1},"b":{"a":2}}`,
	} {
		got, err := normalizeInput(json.RawMessage(in))
		if err != nil || string(got) != want {
			t.Errorf("%s: got %s %v, want %s", in, got, err, want)
		}
	}
	for in, detail := range map[string]string{
		`{"a":1,"a":2}`:                 `field "a" appears more than once`,
		`{"x":{"b":1,"b":1}}`:           `field "x.b" appears more than once`,
		`{"l":[{"k":1},{"k":1,"k":2}]}`: `field "l.1.k" appears more than once`,
		`[1]`:                           "not a JSON object",
		`{"a":`:                         "not valid JSON",
	} {
		_, err := normalizeInput(json.RawMessage(in))
		if ie, ok := err.(*InputError); !ok || !strings.Contains(ie.Detail, detail) {
			t.Errorf("%s: %v, want %q", in, err, detail)
		}
	}
}

func TestExpandTemplate(t *testing.T) {
	for _, c := range []struct{ tmpl, args, want string }{
		{"Plan $city for $days days: $ARGUMENTS", `Kyoto 3`, "Plan Kyoto for 3 days: Kyoto 3"},
		{"$0|$1|$2|$ARGUMENTS[1]|$9", `a "b c" 'd e'`, "a|b c|d e|b c|"},
		{"Cost $$5 and $nope for $city", "x", "Cost $5 and $nope for x"},
		{"No placeholders.", "some args", "No placeholders.\n\nARGUMENTS: some args"},
		{"No placeholders.", "", "No placeholders."},
		{"Only $$ here", "x", "Only $ here\n\nARGUMENTS: x"},
		{`$0`, `a\ b "c\"d"`, `a b`},
		{`$1`, `a\ b "c\"d"`, `c"d`},
		{"$ARGUMENTS[x] end$", "q", "q[x] end$"},
	} {
		if got := expandTemplate(c.tmpl, c.args, []string{"city", "days"}); got != c.want {
			t.Errorf("%q with %q: got %q, want %q", c.tmpl, c.args, got, c.want)
		}
	}
}

func FuzzApprovalRules(f *testing.F) {
	f.Add([]byte(`[{"id":"a","tool":"x*","when":{"f.*.g":{"regex":"[a-z]+\\d{2,}"}},"action":"ask"}]`), []byte(`{"f":[{"g":"ab12"}]}`))
	f.Add([]byte(`[{"id":"b","tool":"*","when":{"p":{"prefix":"/etc","normalize":"path"}},"action":"deny"}]`), []byte(`{"p":"/tmp/../etc"}`))
	f.Fuzz(func(t *testing.T, rules, input []byte) {
		rs, err := ParseApprovalRules(rules)
		if err != nil {
			return
		}
		c, err := compileRules(rs)
		if err != nil {
			t.Fatalf("parsed rules failed to compile: %v", err)
		}
		_ = decide(c, ToolCallView{Name: "x1", Input: input})
	})
}

func FuzzPortableRegex(f *testing.F) {
	for _, s := range []string{`a|b`, `[\w-]+`, `(?:x){2,3}?`, `\b\d+\b`} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, expr string) {
		if err := checkPortable(expr); err == nil {
			if _, err := compilePortable(expr); err != nil {
				t.Fatalf("%q passed the subset but does not compile: %v", expr, err)
			}
		}
	})
}
