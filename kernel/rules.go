package kernel

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"math/big"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// RuleAction is what a matching rule asks for; any deny wins, then an allow exempts the call from every ask.
type RuleAction string

const (
	RuleAllow RuleAction = "allow" // run without review, unless a pre_tool_use hook asks
	RuleAsk   RuleAction = "ask"   // send the call to the approval reviewers
	RuleDeny  RuleAction = "deny"  // refuse the call, and the model sees Reason
)

// ApprovalRule matches calls by tool name and input fields; spec/schema/approval_rule.schema.json is its file form.
type ApprovalRule struct {
	ID     string                       `json:"id"`
	Tool   string                       `json:"tool"` // exact name or whole-string glob
	When   map[string]Condition         `json:"when,omitempty"`
	Action RuleAction                   `json:"action"`
	Reason string                       `json:"reason,omitempty"`
	Match  func(call ToolCallView) bool `json:"-"` // Go only, so a rule that sets it cannot be shared with other languages
}

// Condition sets exactly one operator, Normalize only qualifying Prefix, so a rule file means the same thing in every language.
type Condition struct {
	Eq        json.RawMessage   `json:"eq,omitempty"`
	Glob      string            `json:"glob,omitempty"`
	Regex     string            `json:"regex,omitempty"`
	Prefix    string            `json:"prefix,omitempty"`
	Normalize string            `json:"normalize,omitempty"`
	In        []json.RawMessage `json:"in,omitempty"`
	Exists    *bool             `json:"exists,omitempty"`
}

// ToolCallView is what rules and reviewers see of a call: the normalized input after pre_tool_use rewrites, which the tool receives byte for byte.
type ToolCallView struct {
	CallID string          `json:"call_id"`
	Name   string          `json:"name"`
	Input  json.RawMessage `json:"input"`
}

// WithApprovalRules adds agent-scope rules; New fails when one is malformed or two share an id.
func WithApprovalRules(rules ...ApprovalRule) Option {
	return func(c *config) { c.rules = append(c.rules, rules...) }
}

// SessionApprovalRules adds session-scope rules, after the agent's; NewSession fails when one is malformed.
func SessionApprovalRules(rules ...ApprovalRule) SessionOption {
	return func(c *sessionConfig) { c.rules = append(c.rules, rules...) }
}

var conditionOps = []string{"eq", "glob", "regex", "prefix", "in", "exists"}

// ParseApprovalRules reads a rule file; unknown operators, a condition with two operators and regexes outside the portable subset are errors.
func ParseApprovalRules(data []byte) ([]ApprovalRule, error) {
	var raw []struct {
		ID     string                                `json:"id"`
		Tool   string                                `json:"tool"`
		When   map[string]map[string]json.RawMessage `json:"when"`
		Action RuleAction                            `json:"action"`
		Reason string                                `json:"reason"`
	}
	d := json.NewDecoder(bytes.NewReader(data))
	if err := d.Decode(&raw); err != nil {
		return nil, fmt.Errorf("approval rules: %w", err)
	}
	rules := make([]ApprovalRule, len(raw))
	for i, r := range raw {
		rules[i] = ApprovalRule{ID: r.ID, Tool: r.Tool, Action: r.Action, Reason: r.Reason}
		for field, ops := range r.When {
			var c Condition
			for op, v := range ops {
				if !slices.Contains(conditionOps, op) && op != "normalize" {
					return nil, fmt.Errorf("approval rule %q: unknown operator %q on %q", r.ID, op, field)
				}
				if err := json.Unmarshal(v, conditionField(&c, op)); err != nil {
					return nil, fmt.Errorf("approval rule %q: %s on %q: %w", r.ID, op, field, err)
				}
			}
			if c.Eq == nil && ops["eq"] != nil {
				c.Eq = json.RawMessage("null") // eq null is a real operator, not an absent one
			}
			if rules[i].When == nil {
				rules[i].When = map[string]Condition{}
			}
			rules[i].When[field] = c
		}
	}
	if _, err := compileRules(rules); err != nil {
		return nil, err
	}
	return rules, nil
}

func conditionField(c *Condition, op string) any {
	switch op {
	case "eq":
		return &c.Eq
	case "glob":
		return &c.Glob
	case "regex":
		return &c.Regex
	case "prefix":
		return &c.Prefix
	case "normalize":
		return &c.Normalize
	case "in":
		return &c.In
	}
	return &c.Exists
}

// compiledRule is a validated rule with its regexes compiled.
type compiledRule struct {
	ApprovalRule
	conds []compiledCond
}

type compiledCond struct {
	path []string
	c    Condition
	re   *regexp.Regexp
	eq   any
	in   []any
	deny bool // the rule denies, so a relative path that escapes the working directory matches an absolute path prefix
}

// compileRules validates a rule set: every id present and unique, a known action, and each condition with exactly one operator.
func compileRules(rules []ApprovalRule) ([]compiledRule, error) {
	seen := map[string]bool{}
	out := make([]compiledRule, 0, len(rules))
	for _, r := range rules {
		switch {
		case r.ID == "":
			return nil, errors.New("approval rule without an id")
		case seen[r.ID]:
			return nil, fmt.Errorf("approval rule id %q is used twice", r.ID)
		case r.Tool == "":
			return nil, fmt.Errorf("approval rule %q has no tool", r.ID)
		case r.Action != RuleAllow && r.Action != RuleAsk && r.Action != RuleDeny:
			return nil, fmt.Errorf("approval rule %q: unknown action %q", r.ID, r.Action)
		}
		seen[r.ID] = true
		cr := compiledRule{ApprovalRule: r}
		for _, field := range slices.Sorted(maps.Keys(r.When)) {
			cc, err := compileCond(field, r.When[field])
			if err != nil {
				return nil, fmt.Errorf("approval rule %q: %q: %w", r.ID, field, err)
			}
			cc.deny = r.Action == RuleDeny
			cr.conds = append(cr.conds, cc)
		}
		out = append(out, cr)
	}
	return out, nil
}

func compileCond(field string, c Condition) (compiledCond, error) {
	cc := compiledCond{path: strings.Split(field, "."), c: c}
	if slices.Contains(cc.path, "") {
		return cc, errors.New("empty path segment")
	}
	n := 0
	if c.Eq != nil {
		n++
		v, err := decodeValue(c.Eq)
		if err != nil {
			return cc, err
		}
		cc.eq = v
	}
	if c.Glob != "" {
		n++
	}
	if c.Regex != "" {
		n++
		re, err := compilePortable(c.Regex)
		if err != nil {
			return cc, err
		}
		cc.re = re
	}
	if c.Prefix != "" {
		n++
	}
	if c.In != nil {
		n++
		for _, raw := range c.In {
			v, err := decodeValue(raw)
			if err != nil {
				return cc, err
			}
			cc.in = append(cc.in, v)
		}
	}
	if c.Exists != nil {
		n++
	}
	switch {
	case n != 1:
		return cc, fmt.Errorf("a condition sets exactly one operator, not %d", n)
	case c.Normalize != "" && (c.Normalize != "path" || c.Prefix == ""):
		return cc, errors.New(`normalize must be "path" and qualifies prefix only`)
	}
	return cc, nil
}

// decodeValue reads JSON keeping numbers exact, so eq compares them by value.
func decodeValue(b []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// ruleVerdict is what a rule set says about one call: its own conclusion and every rule that matched.
type ruleVerdict struct {
	action  RuleAction // "" when no rule matched
	matched []compiledRule
	deny    *compiledRule // the first matching deny, in rule order
}

// decide applies the rule order: any deny wins, then an allow exempts from ask, then any ask.
func decide(rules []compiledRule, view ToolCallView) ruleVerdict {
	var v ruleVerdict
	input, err := decodeValue(view.Input)
	if err != nil {
		input = nil
	}
	var allow, ask bool
	for i := range rules {
		r := &rules[i]
		if !r.matches(view, input) {
			continue
		}
		v.matched = append(v.matched, *r)
		switch r.Action {
		case RuleDeny:
			if v.deny == nil {
				v.deny = r
			}
		case RuleAllow:
			allow = true
		case RuleAsk:
			ask = true
		}
	}
	switch {
	case v.deny != nil:
		v.action = RuleDeny
	case allow:
		v.action = RuleAllow
	case ask:
		v.action = RuleAsk
	}
	return v
}

func (r *compiledRule) matches(view ToolCallView, input any) bool {
	if r.Tool != "*" && r.Tool != view.Name && !matchGlob(r.Tool, view.Name) {
		return false
	}
	for _, c := range r.conds {
		if !c.holds(input) {
			return false
		}
	}
	return r.Match == nil || r.Match(view)
}

// holds evaluates the condition over every value its path reaches; no value means the field does not exist.
func (c compiledCond) holds(input any) bool {
	values := resolvePath(input, c.path)
	if c.c.Exists != nil {
		return (len(values) > 0) == *c.c.Exists
	}
	for _, v := range values {
		if c.test(v) {
			return true
		}
	}
	return false
}

func (c compiledCond) test(v any) bool {
	s, isString := v.(string)
	switch {
	case c.c.Eq != nil:
		return jsonEqual(c.eq, v)
	case c.c.Glob != "":
		return isString && matchGlob(c.c.Glob, s)
	case c.re != nil:
		return isString && c.re.MatchString(s)
	case c.c.Prefix != "":
		if !isString {
			return false
		}
		if c.c.Normalize == "path" {
			return pathPrefix(path.Clean(s), c.c.Prefix, c.deny)
		}
		return strings.HasPrefix(s, c.c.Prefix)
	case c.in != nil:
		return slices.ContainsFunc(c.in, func(w any) bool { return jsonEqual(w, v) })
	}
	return false
}

// pathPrefix compares whole segments, so /workspace never matches /workspace2; a relative path cannot be placed under an absolute prefix, so it never satisfies one, except that one climbing out with .. counts against a deny.
func pathPrefix(s, prefix string, deny bool) bool {
	p := path.Clean(prefix)
	if path.IsAbs(p) && !path.IsAbs(s) {
		return deny && (s == ".." || strings.HasPrefix(s, "../"))
	}
	return p == "/" && path.IsAbs(s) || s == p || strings.HasPrefix(s, p+"/")
}

// resolvePath follows dot segments: a number indexes an array, * takes every element, and anything else looks up an object key.
func resolvePath(v any, segs []string) []any {
	if len(segs) == 0 {
		return []any{v}
	}
	seg, rest := segs[0], segs[1:]
	switch x := v.(type) {
	case map[string]any:
		child, ok := x[seg]
		if !ok {
			return nil
		}
		return resolvePath(child, rest)
	case []any:
		if seg == "*" {
			var out []any
			for _, e := range x {
				out = append(out, resolvePath(e, rest)...)
			}
			return out
		}
		i, err := strconv.Atoi(seg)
		if err != nil || i < 0 || i >= len(x) || strconv.Itoa(i) != seg {
			return nil
		}
		return resolvePath(x[i], rest)
	}
	return nil
}

// jsonEqual is JSON equality: numbers by value, objects regardless of key order.
func jsonEqual(a, b any) bool {
	switch x := a.(type) {
	case json.Number:
		y, ok := b.(json.Number)
		if !ok {
			return false
		}
		rx, ok1 := new(big.Rat).SetString(string(x))
		ry, ok2 := new(big.Rat).SetString(string(y))
		return ok1 && ok2 && rx.Cmp(ry) == 0
	case map[string]any:
		y, ok := b.(map[string]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for k, v := range x {
			w, ok := y[k]
			if !ok || !jsonEqual(v, w) {
				return false
			}
		}
		return true
	case []any:
		y, ok := b.([]any)
		if !ok || len(x) != len(y) {
			return false
		}
		for i := range x {
			if !jsonEqual(x[i], y[i]) {
				return false
			}
		}
		return true
	}
	return a == b
}

// normalizeInput is the one form rules, hooks, reviewers and the tool all read: a JSON object without repeated keys at any depth and without insignificant whitespace; key order and string escapes stay as sent.
func normalizeInput(raw json.RawMessage) (json.RawMessage, error) {
	t := bytes.TrimSpace(raw)
	if len(t) == 0 || t[0] != '{' {
		return nil, &InputError{Detail: "the arguments are not a JSON object"}
	}
	if !json.Valid(t) {
		return nil, &InputError{Detail: "the arguments are not valid JSON"}
	}
	if err := checkRepeated(t, ""); err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, t); err != nil {
		return nil, &InputError{Detail: "the arguments are not valid JSON"}
	}
	return buf.Bytes(), nil
}

// checkRepeated rejects an object that names a key twice, at any depth, because decoders disagree on which one wins.
func checkRepeated(data []byte, at string) error {
	t := bytes.TrimSpace(data)
	if len(t) == 0 {
		return nil
	}
	switch t[0] {
	case '{':
		keys, values, err := objectEntries(t)
		if err != nil {
			return &InputError{Detail: "the arguments are not valid JSON"}
		}
		seen := map[string]bool{}
		for i, k := range keys {
			if seen[k] {
				return &InputError{Detail: fmt.Sprintf("field %q appears more than once", join(at, k))}
			}
			seen[k] = true
			if err := checkRepeated(values[i], join(at, k)); err != nil {
				return err
			}
		}
	case '[':
		var items []json.RawMessage
		if err := json.Unmarshal(t, &items); err != nil {
			return &InputError{Detail: "the arguments are not valid JSON"}
		}
		for i, it := range items {
			if err := checkRepeated(it, join(at, strconv.Itoa(i))); err != nil {
				return err
			}
		}
	}
	return nil
}
