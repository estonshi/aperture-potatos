package compare

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"math/big"
	"reflect"
	"slices"
	"strconv"
	"strings"
)

// Canonical decodes JSON with numbers kept as written, so an accepted output is stored as the binding wrote it.
func Canonical(b []byte) (any, error) {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return nil, err
	}
	return v, nil
}

// zeroMillis sets every integer *_ms field to 0, because durations depend on the machine; a value of another type is kept, so it still fails against the expected integer.
func zeroMillis(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, c := range x {
			if n, ok := c.(json.Number); ok && strings.HasSuffix(k, "_ms") {
				if r, ok := value(n); ok && r.IsInt() {
					x[k] = json.Number("0")
					continue
				}
			}
			x[k] = zeroMillis(c)
		}
	case []any:
		for i, c := range x {
			x[i] = zeroMillis(c)
		}
	}
	return v
}

// Same reports whether two JSON documents are equal as values once durations are zeroed.
func Same(a, b []byte) (bool, error) {
	ca, err := Canonical(a)
	if err != nil {
		return false, err
	}
	cb, err := Canonical(b)
	if err != nil {
		return false, err
	}
	return where(zeroMillis(ca), zeroMillis(cb), "$") == "", nil
}

// value reads a JSON number as an exact rational, so 1, 1.0 and 1e0 are one value whatever a binding's encoder writes; an exponent past float64's range is not read, since its expansion could exhaust memory.
func value(n json.Number) (*big.Rat, bool) {
	s := string(n)
	if i := strings.IndexAny(s, "eE"); i >= 0 {
		if e, err := strconv.Atoi(strings.TrimPrefix(s[i+1:], "+")); err != nil || e > 400 || e < -400 {
			return nil, false
		}
	}
	return new(big.Rat).SetString(s)
}

// sameNumber compares two numbers by value, or by their text when either cannot be read.
func sameNumber(a, b json.Number) bool {
	ra, oka := value(a)
	rb, okb := value(b)
	if oka && okb {
		return ra.Cmp(rb) == 0
	}
	return a == b
}

// differ names the first place two JSON documents differ once durations are zeroed, or returns "" when they are equal.
func differ(want, got []byte) string {
	w, err := Canonical(want)
	if err != nil {
		return "the expected document is not JSON: " + err.Error()
	}
	g, err := Canonical(got)
	if err != nil {
		return "not JSON: " + err.Error()
	}
	return where(zeroMillis(w), zeroMillis(g), "$")
}

// where walks two decoded values in step and describes the first difference at its path.
func where(want, got any, path string) string {
	switch w := want.(type) {
	case map[string]any:
		g, ok := got.(map[string]any)
		if !ok {
			return fmt.Sprintf("%s: got %s, want an object", path, show(got))
		}
		keys := slices.Collect(maps.Keys(w))
		for k := range g {
			if _, ok := w[k]; !ok {
				keys = append(keys, k)
			}
		}
		slices.Sort(keys)
		for _, k := range keys {
			wv, wok := w[k]
			gv, gok := g[k]
			switch p := path + "." + k; {
			case !gok:
				return fmt.Sprintf("%s: missing, want %s", p, show(wv))
			case !wok:
				return fmt.Sprintf("%s: unexpected, got %s", p, show(gv))
			default:
				if d := where(wv, gv, p); d != "" {
					return d
				}
			}
		}
		return ""
	case []any:
		g, ok := got.([]any)
		if !ok {
			return fmt.Sprintf("%s: got %s, want an array", path, show(got))
		}
		for i := range min(len(w), len(g)) {
			if d := where(w[i], g[i], fmt.Sprintf("%s[%d]", path, i)); d != "" {
				return d
			}
		}
		if len(w) != len(g) {
			return fmt.Sprintf("%s: %d items, want %d", path, len(g), len(w))
		}
		return ""
	}
	if wn, ok := want.(json.Number); ok {
		if gn, ok := got.(json.Number); ok && sameNumber(wn, gn) {
			return ""
		}
	} else if reflect.DeepEqual(want, got) {
		return ""
	}
	return fmt.Sprintf("%s: got %s, want %s", path, show(got), show(want))
}

// show prints a value for a difference, cut short so one long string does not bury the rest.
func show(v any) string {
	b, _ := json.Marshal(v)
	if r := []rune(string(b)); len(r) > 120 {
		return string(r[:120]) + "…"
	}
	return string(b)
}

// Pretty formats JSON for an expected file, with keys sorted and no HTML escaping.
func Pretty(v any) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	c, err := Canonical(b)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	e := json.NewEncoder(&buf)
	e.SetEscapeHTML(false)
	e.SetIndent("", "  ")
	if err := e.Encode(c); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// JSONL formats lines one compact document each, keys sorted.
func JSONL(lines []json.RawMessage) ([]byte, error) {
	var buf bytes.Buffer
	for _, l := range lines {
		c, err := Canonical(l)
		if err != nil {
			return nil, err
		}
		e := json.NewEncoder(&buf)
		e.SetEscapeHTML(false)
		if err := e.Encode(c); err != nil {
			return nil, err
		}
	}
	return buf.Bytes(), nil
}

// ReadJSONL splits a JSONL file into lines, skipping blank ones.
func ReadJSONL(b []byte) []json.RawMessage {
	var out []json.RawMessage
	for _, l := range bytes.Split(b, []byte("\n")) {
		if len(bytes.TrimSpace(l)) > 0 {
			out = append(out, json.RawMessage(l))
		}
	}
	return out
}
