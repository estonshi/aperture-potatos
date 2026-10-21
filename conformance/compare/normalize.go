package compare

import (
	"encoding/json"
	"fmt"
	"maps"
	"slices"
	"strings"
)

// concurrent sorts the events of each run that runs concurrently by call id, handing the run's seqs and times back out in their original order: a parallel tool group (its tool.* and per-call hook.* events) and a review batch of more than one call (its approval.resolved and reviewer hook.* events); a task's own events are placed by tasked first. Then it renames hook ids by first appearance, because workers take them in no fixed order.
func concurrent(lines []json.RawMessage) []json.RawMessage {
	type ev struct {
		raw  map[string]any
		call string
		unit string
	}
	parsed := make([]map[string]any, len(lines))
	for i, l := range lines {
		if json.Unmarshal(l, &parsed[i]) != nil {
			return lines
		}
	}
	parsed = tasked(parsed)
	evs := make([]ev, len(lines))
	groupOf := map[string]string{}
	groupSize := map[string]int{}
	batchOf := map[string]string{}
	for i, m := range parsed {
		evs[i].raw = m
		if hops, _ := m["path"].([]any); len(hops) > 0 {
			continue // a task's own events are ordered by tasked, not as part of the loop's groups
		}
		typ, _ := m["type"].(string)
		data, _ := m["data"].(map[string]any)
		call, _ := data["call_id"].(string)
		evs[i].call = call
		switch typ {
		case "tool.started":
			g := fmt.Sprint(m["turn_id"], "/", m["step"], "/", data["group"])
			groupOf[call] = g
			groupSize[g]++
		case "approval.requested":
			if n, _ := data["batch_size"].(float64); n > 1 {
				batchOf[call], _ = data["batch_id"].(string)
			}
		}
	}
	for i := range evs {
		m, call := evs[i].raw, evs[i].call
		typ, _ := m["type"].(string)
		data, _ := m["data"].(map[string]any)
		point, _ := data["point"].(string)
		switch {
		case call == "":
		case typ == "approval.resolved" || strings.HasPrefix(typ, "hook.") && point == "approval":
			if b := batchOf[call]; b != "" {
				evs[i].unit = "batch " + b
			}
		case strings.HasPrefix(typ, "tool.") || strings.HasPrefix(typ, "hook."):
			if g := groupOf[call]; g != "" && groupSize[g] > 1 {
				evs[i].unit = "group " + g
			}
		}
	}
	out := make([]map[string]any, len(evs))
	for i := 0; i < len(evs); {
		j := i + 1
		for evs[i].unit != "" && j < len(evs) && evs[j].unit == evs[i].unit {
			j++
		}
		run := slices.Clone(evs[i:j])
		slices.SortStableFunc(run, func(a, b ev) int { return strings.Compare(a.call, b.call) })
		for k := range run {
			m := maps.Clone(run[k].raw)
			m["seq"], m["time"] = evs[i+k].raw["seq"], evs[i+k].raw["time"]
			out[i+k] = m
		}
		i = j
	}
	names := map[string]string{}
	res := make([]json.RawMessage, len(out))
	for i, m := range out {
		if data, ok := m["data"].(map[string]any); ok {
			if id, ok := data["hook_id"].(string); ok {
				if names[id] == "" {
					names[id] = fmt.Sprintf("hook_#%d", len(names)+1)
				}
				data = maps.Clone(data)
				data["hook_id"] = names[id]
				m["data"] = data
			}
		}
		res[i], _ = json.Marshal(m)
	}
	return res
}

// tasked moves each event under a task hop to just after the latest task.started of that task, keeping each task's own order, since O4 lets a task's events interleave with the loop's in any order; a child session's own tasks are placed the same way within it, and seqs and times stay in their positions.
func tasked(ms []map[string]any) []map[string]any {
	idx := make([]int, len(ms))
	for i := range ms {
		idx[i] = i
	}
	out := make([]map[string]any, 0, len(ms))
	for _, k := range placed(ms, idx, 0) {
		m := maps.Clone(ms[k])
		m["seq"], m["time"] = ms[len(out)]["seq"], ms[len(out)]["time"]
		out = append(out, m)
	}
	relayed(ms, out)
	return out
}

// relayed keeps each relay's downstream seqs in their positions as well, since a child's bus orders its own tasks' events just as freely.
func relayed(ms, out []map[string]any) {
	each := func(hops []any, f func(key string, hop map[string]any)) {
		for d, h := range hops {
			if hop, ok := h.(map[string]any); ok && hop["seq"] != nil {
				f(fmt.Sprint(d, "/", hop["session_id"], "/", hop["id"]), hop)
			}
		}
	}
	seqs := map[string][]any{}
	for _, m := range ms {
		hops, _ := m["path"].([]any)
		each(hops, func(k string, hop map[string]any) { seqs[k] = append(seqs[k], hop["seq"]) })
	}
	for _, m := range out {
		hops, _ := m["path"].([]any)
		cp := make([]any, len(hops)) // the clone above shares its path with ms
		for i, h := range hops {
			if hop, ok := h.(map[string]any); ok {
				h = maps.Clone(hop)
			}
			cp[i] = h
		}
		each(cp, func(k string, hop map[string]any) { hop["seq"], seqs[k] = seqs[k][0], seqs[k][1:] })
		if len(cp) > 0 {
			m["path"] = cp
		}
	}
}

// placed orders events whose paths are at least depth long: those under a deeper task hop, and a task's task.progress, follow that task's latest task.started at this depth, recursively.
func placed(ms []map[string]any, idx []int, depth int) []int {
	last := map[string]int{} // task id -> position of its latest task.started at this depth
	after := map[int][]int{}
	var loop []int
	for _, i := range idx {
		hops, _ := ms[i]["path"].([]any)
		if len(hops) > depth {
			hop, _ := hops[depth].(map[string]any)
			id, _ := hop["id"].(string)
			if at, ok := last[id]; ok {
				after[at] = append(after[at], i)
				continue
			}
		}
		if ms[i]["type"] == "task.progress" && len(hops) == depth { // published by the task, not with a state change (O3), so it is the task's own too
			data, _ := ms[i]["data"].(map[string]any)
			id, _ := data["task_id"].(string)
			if at, ok := last[id]; ok {
				after[at] = append(after[at], i)
				continue
			}
		}
		if ms[i]["type"] == "task.started" && len(hops) == depth {
			data, _ := ms[i]["data"].(map[string]any)
			id, _ := data["task_id"].(string)
			last[id] = i
		}
		loop = append(loop, i)
	}
	var out []int
	for _, i := range loop {
		out = append(out, i)
		out = append(out, placed(ms, after[i], depth+1)...)
	}
	return out
}

// untimed blanks every timestamp string, for a case whose tasks or async hooks read the step clock while the loop does: which read comes first is free (runtime.md O4, §10.6), and each read moves every later timestamp.
func untimed(lines []json.RawMessage) []json.RawMessage {
	out := make([]json.RawMessage, len(lines))
	for i, l := range lines {
		v, err := Canonical(l)
		if err != nil {
			out[i] = l
			continue
		}
		out[i], _ = json.Marshal(blankTimes(v))
	}
	return out
}

func blankTimes(v any) any {
	switch x := v.(type) {
	case map[string]any:
		for k, c := range x {
			if _, ok := c.(string); ok && (k == "time" || k == "ts" || k == "started_at" || k == "ended_at") {
				x[k] = ""
				continue
			}
			x[k] = blankTimes(c)
		}
	case []any:
		for i, c := range x {
			x[i] = blankTimes(c)
		}
	}
	return v
}

// concurrentHooks sorts each run of consecutive approval or post_tool_use calls by call id, since those run concurrently.
func concurrentHooks(lines []json.RawMessage) []json.RawMessage {
	type entry struct {
		raw   json.RawMessage
		point string
		call  string
	}
	es := make([]entry, len(lines))
	for i, l := range lines {
		var e struct {
			Point string `json:"point"`
			Input struct {
				Call struct {
					ID     string `json:"id"`
					CallID string `json:"call_id"`
				} `json:"call"`
			} `json:"input"`
		}
		_ = json.Unmarshal(l, &e)
		es[i] = entry{raw: l, point: e.Point, call: e.Input.Call.ID + e.Input.Call.CallID}
	}
	out := make([]json.RawMessage, 0, len(es))
	for i := 0; i < len(es); {
		j := i + 1
		conc := es[i].point == "approval" || es[i].point == "post_tool_use"
		for conc && j < len(es) && es[j].point == es[i].point {
			j++
		}
		run := slices.Clone(es[i:j])
		slices.SortStableFunc(run, func(a, b entry) int { return strings.Compare(a.call, b.call) })
		for _, e := range run {
			out = append(out, e.raw)
		}
		i = j
	}
	return out
}

// noticeKinds drops the detail of every model.notice event, which is compared by kind only.
func noticeKinds(lines []json.RawMessage) []json.RawMessage {
	out := make([]json.RawMessage, len(lines))
	for i, l := range lines {
		out[i] = l
		var ev map[string]any
		if json.Unmarshal(l, &ev) != nil || ev["type"] != "model.notice" {
			continue
		}
		if data, ok := ev["data"].(map[string]any); ok {
			delete(data, "detail")
		}
		out[i], _ = json.Marshal(ev)
	}
	return out
}

// envelope lists the message fields the kernel fills at commit; an adapter's message has none of them yet, so they are not compared.
var envelope = []string{"v", "id", "session_id", "seq", "epoch", "ts"}

// unenveloped drops the commit envelope from a message document, or from the message a stream event carries.
func unenveloped(doc json.RawMessage, inEvent bool) json.RawMessage {
	var obj map[string]json.RawMessage
	if json.Unmarshal(doc, &obj) != nil {
		return doc
	}
	if inEvent {
		msg, ok := obj["message"]
		if !ok {
			return doc
		}
		obj["message"] = unenveloped(msg, false)
	} else {
		for _, k := range envelope {
			delete(obj, k)
		}
	}
	out, _ := json.Marshal(obj)
	return out
}
