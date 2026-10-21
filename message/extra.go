package message

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"sync"
)

var keyCache sync.Map // reflect.Type -> map[string]bool

// jsonKeys is the set of JSON names a struct declares, used to route every other key into extra.
func jsonKeys(t reflect.Type) map[string]bool {
	if v, ok := keyCache.Load(t); ok {
		return v.(map[string]bool)
	}
	keys := map[string]bool{}
	for i := range t.NumField() {
		f := t.Field(i)
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		keys[name] = true
	}
	keyCache.Store(t, keys)
	return keys
}

// decodeKnown decodes only exactly-named keys into v, because encoding/json matches names case-insensitively and a key like "Text" must stay an unknown field.
func decodeKnown(b []byte, v any, drop map[string]bool) (map[string]json.RawMessage, error) {
	obj, err := parseObject(b)
	if err != nil {
		return nil, err
	}
	return splitKnown(obj, v, drop)
}

func parseObject(b []byte) (map[string]json.RawMessage, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(b, &obj); err != nil {
		return nil, err
	}
	if obj == nil {
		return nil, fmt.Errorf("message: expected a JSON object, got %s", bytes.TrimSpace(b))
	}
	return obj, nil
}

func splitKnown(obj map[string]json.RawMessage, v any, drop map[string]bool) (map[string]json.RawMessage, error) {
	known := jsonKeys(reflect.TypeOf(v).Elem())
	fields := make(map[string]json.RawMessage, len(obj))
	var extra map[string]json.RawMessage
	for k, raw := range obj {
		switch {
		case known[k]:
			fields[k] = raw
		case drop[k]:
		default:
			if extra == nil {
				extra = map[string]json.RawMessage{}
			}
			extra[k] = raw
		}
	}
	kb, err := json.Marshal(fields)
	if err != nil {
		return nil, err
	}
	return extra, json.Unmarshal(kb, v)
}

var typeKey = map[string]bool{"type": true}

func decodeBlockFields(b []byte, want BlockType, v any) (map[string]json.RawMessage, error) {
	obj, err := parseObject(b)
	if err != nil {
		return nil, err
	}
	if raw, ok := obj["type"]; ok {
		var typ string
		if err := json.Unmarshal(raw, &typ); err != nil || typ != string(want) {
			return nil, fmt.Errorf("message: block type %s is not %q", raw, want)
		}
	}
	return splitKnown(obj, v, typeKey)
}

// marshalBlock writes the type discriminator first, then the declared fields, then the unknown ones.
func marshalBlock(typ BlockType, v any, extra map[string]json.RawMessage) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, len(b)+len(typ)+12)
	out = append(out, `{"type":`...)
	out = append(out, jsonString(string(typ))...)
	if len(b) > 2 {
		out = append(out, ',')
		out = append(out, b[1:]...)
	} else {
		out = append(out, '}')
	}
	return appendExtra(out, extra), nil
}

// appendExtra adds unknown fields in sorted key order so the same value always encodes to the same bytes.
func appendExtra(obj []byte, extra map[string]json.RawMessage) []byte {
	if len(extra) == 0 {
		return obj
	}
	keys := make([]string, 0, len(extra))
	for k := range extra {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	out := obj[:len(obj)-1]
	empty := len(bytes.TrimSpace(out)) == 1
	for _, k := range keys {
		if !empty {
			out = append(out, ',')
		}
		empty = false
		out = append(out, jsonString(k)...)
		out = append(out, ':')
		out = append(out, extra[k]...)
	}
	return append(out, '}')
}

func jsonString(s string) []byte {
	b, _ := json.Marshal(s)
	return b
}
