// Package schemadrift compares a Go type's JSON fields with a definition in spec/schema or spec/ext, so a hand-written binding cannot drift from the spec.
package schemadrift

import (
	"encoding/json"
	"fmt"
	"path"
	"reflect"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/estonshi/aperture-potatos/spec"
)

var (
	rawType       = reflect.TypeFor[json.RawMessage]()
	marshalerType = reflect.TypeFor[json.Marshaler]()
	timeType      = reflect.TypeFor[time.Time]()
	contentName   = "github.com/estonshi/aperture-potatos/message.Content"
)

var (
	extraMu sync.Mutex
	extras  = map[reflect.Type][]string{}
)

// Extra declares fields a type's own MarshalJSON writes beyond its struct fields, such as ToolSpec's timeout_ms, wherever the type appears.
func Extra(typ reflect.Type, names ...string) {
	extraMu.Lock()
	defer extraMu.Unlock()
	extras[typ] = append(extras[typ], names...)
}

// Check fails t for every field name, requiredness or JSON type that differs between typ and the schema at loc ("message.schema.json", "common.schema.json#/$defs/usage", or a path under spec such as "ext/mcp/events.schema.json#/$defs/mcp_elicitation"); extra names fields the schema has but the struct carries some other way, such as a block's type discriminator.
func Check(t testing.TB, typ reflect.Type, loc string, extra ...string) {
	t.Helper()
	c := &checker{t: t, docs: map[string]any{}, seen: map[string]bool{}}
	node, file := c.resolve(loc, "")
	c.object(typ, node, file, loc, extra)
}

// Defs lists the $defs keys of a schema file, for tests that must cover every definition.
func Defs(t testing.TB, file string) []string {
	t.Helper()
	c := &checker{t: t, docs: map[string]any{}}
	defs, _ := c.doc(specPath(file, "")).(map[string]any)["$defs"].(map[string]any)
	keys := make([]string, 0, len(defs))
	for k := range defs {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}

type checker struct {
	t    testing.TB
	docs map[string]any
	seen map[string]bool
}

// specPath turns a schema reference into a path under spec: a bare file name is in spec/schema, anything else is relative to the referring file, or to spec itself at the top.
func specPath(ref, from string) string {
	switch {
	case from != "":
		return path.Join(path.Dir(from), ref)
	case !strings.Contains(ref, "/"):
		return "schema/" + ref
	}
	return ref
}

func (c *checker) doc(file string) any {
	if d, ok := c.docs[file]; ok {
		return d
	}
	b, err := spec.ReadFile(file)
	if err != nil {
		c.t.Fatalf("schemadrift: %v", err)
	}
	var d any
	if err := json.Unmarshal(b, &d); err != nil {
		c.t.Fatalf("schemadrift: %s: %v", file, err)
	}
	c.docs[file] = d
	return d
}

// resolve follows a $ref-style location relative to file and returns the node and the file it lives in.
func (c *checker) resolve(ref, file string) (map[string]any, string) {
	f, ptr, _ := strings.Cut(ref, "#")
	if f == "" {
		f = file
	} else {
		f = specPath(f, file)
	}
	var node any = c.doc(f)
	for seg := range strings.SplitSeq(strings.TrimPrefix(ptr, "/"), "/") {
		if seg == "" {
			continue
		}
		m, ok := node.(map[string]any)
		if !ok {
			c.t.Fatalf("schemadrift: %s: no %s", f, ptr)
		}
		node = m[seg]
	}
	m, ok := node.(map[string]any)
	if !ok {
		c.t.Fatalf("schemadrift: %s#%s is not a schema object", f, ptr)
	}
	return m, f
}

// deref follows $ref chains so a property's own keywords and its target's are both visible.
func (c *checker) deref(node map[string]any, file string) (map[string]any, string) {
	for {
		ref, ok := node["$ref"].(string)
		if !ok {
			return node, file
		}
		node, file = c.resolve(ref, file)
	}
}

// shape collects properties and required names from a node, its $ref target and unconditional allOf members.
func (c *checker) shape(node map[string]any, file string) (props map[string]any, propFile map[string]string, required map[string]bool) {
	props, propFile, required = map[string]any{}, map[string]string{}, map[string]bool{}
	var walk func(n map[string]any, f string)
	walk = func(n map[string]any, f string) {
		n, f = c.deref(n, f)
		if p, ok := n["properties"].(map[string]any); ok {
			for k, v := range p {
				props[k], propFile[k] = v, f
			}
		}
		if r, ok := n["required"].([]any); ok {
			for _, k := range r {
				required[k.(string)] = true
			}
		}
		all, _ := n["allOf"].([]any)
		for _, a := range all {
			if am, ok := a.(map[string]any); ok && am["if"] == nil {
				walk(am, f)
			}
		}
	}
	walk(node, file)
	return props, propFile, required
}

type goField struct {
	name     string
	typ      reflect.Type
	required bool
}

func fieldsOf(t reflect.Type) []goField {
	var out []goField
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, opts, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if f.Anonymous && name == "" && f.Type.Kind() == reflect.Struct {
			out = append(out, fieldsOf(f.Type)...)
			continue
		}
		if name == "" {
			name = f.Name
		}
		optional := slices.Contains(strings.Split(opts, ","), "omitempty") || slices.Contains(strings.Split(opts, ","), "omitzero")
		out = append(out, goField{name: name, typ: f.Type, required: !optional})
	}
	return out
}

func (c *checker) object(typ reflect.Type, node map[string]any, file, where string, extra []string) {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	key := typ.String() + "@" + where
	if c.seen[key] {
		return
	}
	c.seen[key] = true
	props, propFile, required := c.shape(node, file)
	goNames := map[string]bool{}
	extraMu.Lock()
	extra = append(slices.Clone(extra), extras[typ]...)
	extraMu.Unlock()
	for _, e := range extra {
		goNames[e] = true
		if props[e] == nil {
			c.t.Errorf("%s: schema lacks %q", where, e)
		}
	}
	for _, f := range fieldsOf(typ) {
		goNames[f.name] = true
		p, ok := props[f.name].(map[string]any)
		if !ok {
			c.t.Errorf("%s: %s.%s (%q) is not in the schema", where, typ.Name(), fieldName(typ, f.name), f.name)
			continue
		}
		if required[f.name] != f.required {
			c.t.Errorf("%s: %q required in schema=%v, in Go=%v", where, f.name, required[f.name], f.required)
		}
		c.value(f.typ, p, propFile[f.name], where+"."+f.name)
	}
	for name := range props {
		if !goNames[name] {
			c.t.Errorf("%s: schema property %q has no Go field in %s", where, name, typ.Name())
		}
	}
}

func fieldName(t reflect.Type, jsonName string) string {
	for i := range t.NumField() {
		if n, _, _ := strings.Cut(t.Field(i).Tag.Get("json"), ","); n == jsonName {
			return t.Field(i).Name
		}
	}
	return "?"
}

// value checks the JSON type of one property and descends into nested objects and arrays.
func (c *checker) value(typ reflect.Type, node map[string]any, file, where string) {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ == rawType || typ.Kind() == reflect.Interface {
		return
	}
	target, tfile := c.deref(node, file)
	if typ.Kind() != reflect.Struct && typ.Implements(marshalerType) {
		return // a type with its own encoding, such as an enum written as a name, declares its JSON type itself
	}
	want := jsonType(typ)
	if got := schemaType(node, target); got != "" && got != want {
		c.t.Errorf("%s: schema type %s, Go %s is %s", where, got, typ, want)
	}
	if _, declared := target["properties"]; !declared && target["allOf"] == nil && typ.Kind() == reflect.Struct && typ != timeType {
		return // an object without declared properties is free-form in the schema
	}
	switch {
	case typ.PkgPath()+"."+typ.Name() == contentName: // blocks are checked one by one against their own $defs
	case typ.Kind() == reflect.Struct && typ != timeType:
		c.object(typ, target, tfile, where, nil)
	case typ.Kind() == reflect.Slice && typ.Elem().Kind() != reflect.Uint8:
		if items, ok := target["items"].(map[string]any); ok {
			c.value(typ.Elem(), items, tfile, where+"[]")
		}
	}
}

func jsonType(t reflect.Type) string {
	switch {
	case t == timeType:
		return "string"
	case t.Kind() == reflect.Slice && t.Elem().Kind() == reflect.Uint8:
		return "string"
	}
	switch t.Kind() {
	case reflect.String:
		return "string"
	case reflect.Bool:
		return "boolean"
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return "integer"
	case reflect.Float32, reflect.Float64:
		return "number"
	case reflect.Slice, reflect.Array:
		return "array"
	case reflect.Map, reflect.Struct:
		return "object"
	}
	return fmt.Sprint(t.Kind())
}

// schemaType is the JSON type a property declares directly, through its $ref, or through a const; "" means any.
func schemaType(node, target map[string]any) string {
	for _, n := range []map[string]any{node, target} {
		if s, ok := n["type"].(string); ok {
			return s
		}
		if v, ok := n["const"]; ok {
			switch x := v.(type) {
			case string:
				return "string"
			case bool:
				return "boolean"
			case float64:
				if x == float64(int64(x)) {
					return "integer"
				}
				return "number"
			}
		}
		if e, ok := n["enum"].([]any); ok && len(e) > 0 {
			if _, ok := e[0].(string); ok {
				return "string"
			}
		}
	}
	return ""
}
