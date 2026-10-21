package mcp

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"unicode"

	"github.com/estonshi/aperture-potatos/kernel"
)

const maxHits = 20

// searchMeta is mcp_search_tools: reads definitions by name or finds tools by keyword.
type searchMeta struct{ in *install }

func (m *searchMeta) Spec() kernel.ToolSpec {
	spec := metaDefs[searchTool]
	spec.Concurrency = kernel.Parallel
	var names []string
	for _, in := range installsFor(m.in, m.in.session) {
		names = append(names, serverNames(in.cat.Load())...)
	}
	spec.InputSchema = withServerEnum(spec.InputSchema, names)
	return spec
}

// withServerEnum lists the servers in the servers parameter; tools.json fixes the schema's shape.
func withServerEnum(schema json.RawMessage, names []string) json.RawMessage {
	var s map[string]any
	if json.Unmarshal(schema, &s) != nil || len(names) == 0 {
		return schema
	}
	props, _ := s["properties"].(map[string]any)
	servers, _ := props["servers"].(map[string]any)
	items, _ := servers["items"].(map[string]any)
	if items == nil {
		return schema
	}
	items["enum"] = names
	b, err := json.Marshal(s)
	if err != nil {
		return schema
	}
	return b
}

type searchInput struct {
	Names   []string `json:"names"`
	Query   string   `json:"query"`
	Servers []string `json:"servers"`
	Detail  string   `json:"detail"`
}

func (m *searchMeta) Call(ctx context.Context, tc *kernel.ToolContext, raw json.RawMessage) (kernel.ToolResult, error) {
	var q searchInput
	if err := json.Unmarshal(raw, &q); err != nil {
		return kernel.ToolResult{}, &kernel.InputError{Detail: err.Error()}
	}
	if len(q.Names) == 0 && strings.TrimSpace(q.Query) == "" {
		return errResult(text("need_query")), nil
	}
	pool := m.pool(tc.Session(), q.Servers)
	var hits []*toolEntry
	var notes []string
	for _, n := range q.Names {
		if te := find(pool, n); te != nil {
			hits = append(hits, te)
		} else {
			notes = append(notes, text("unknown_tool", "name", n))
		}
	}
	if strings.TrimSpace(q.Query) != "" {
		found := m.search(ctx, q.Query, pool)
		for _, te := range found {
			if !slices.Contains(hits, te) {
				hits = append(hits, te)
			}
		}
		if len(found) == 0 {
			notes = append(notes, text("no_matches", "query", q.Query))
		}
	}
	detail := q.Detail
	if detail == "" {
		detail = "descriptions"
		if len(q.Names) > 0 {
			detail = "schemas"
		}
	}
	out := format(hits, detail)
	if len(notes) > 0 {
		out = strings.TrimSpace(out + "\n\n" + strings.Join(notes, "\n"))
	}
	return kernel.TextResult(out), nil
}

// pool is every tool this session reaches through discovery, limited to servers when given.
func (m *searchMeta) pool(s *kernel.Session, servers []string) []*toolEntry {
	return discoverable(m.in, s, servers)
}

func discoverable(own *install, s *kernel.Session, servers []string) []*toolEntry {
	var out []*toolEntry
	for _, in := range installsFor(own, s) {
		for _, sv := range in.cat.Load().servers {
			if len(servers) > 0 && !slices.Contains(servers, sv.name) {
				continue
			}
			out = append(out, sv.tools...)
		}
	}
	return out
}

func find(pool []*toolEntry, name string) *toolEntry {
	for _, te := range pool {
		if te.name == name {
			return te
		}
	}
	return nil
}

func (m *searchMeta) search(ctx context.Context, query string, pool []*toolEntry) []*toolEntry {
	if fn := m.in.e.o.Search; fn != nil {
		infos := make([]ToolInfo, len(pool))
		for i, te := range pool {
			infos[i] = ToolInfo{Name: te.name, Server: te.server, Title: te.title, Description: te.desc}
		}
		var out []*toolEntry
		for _, info := range fn(ctx, query, infos) {
			if te := find(pool, info.Name); te != nil && !slices.Contains(out, te) {
				out = append(out, te)
			}
			if len(out) == maxHits {
				break
			}
		}
		return out
	}
	return keyword(query, pool)
}

// keyword scores each tool by the query's words in its name, title and description, in that order of weight.
func keyword(query string, pool []*toolEntry) []*toolEntry {
	words := strings.FieldsFunc(strings.ToLower(query), func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
	type hit struct {
		te    *toolEntry
		score int
	}
	var hits []hit
	for _, te := range pool {
		name, title, desc := strings.ToLower(te.name), strings.ToLower(te.title), strings.ToLower(te.desc)
		score := 0
		for _, w := range words {
			if strings.Contains(name, w) {
				score += 3
			}
			if strings.Contains(title, w) {
				score += 2
			}
			if strings.Contains(desc, w) {
				score++
			}
		}
		if score > 0 {
			hits = append(hits, hit{te, score})
		}
	}
	slices.SortStableFunc(hits, func(a, b hit) int {
		if a.score != b.score {
			return b.score - a.score
		}
		return strings.Compare(a.te.name, b.te.name)
	})
	var out []*toolEntry
	for i, h := range hits {
		if i == maxHits {
			break
		}
		out = append(out, h.te)
	}
	return out
}

// format groups the hits by server at the asked level of detail.
func format(hits []*toolEntry, detail string) string {
	var order []string
	byServer := map[string][]*toolEntry{}
	for _, te := range hits {
		if _, ok := byServer[te.server]; !ok {
			order = append(order, te.server)
		}
		byServer[te.server] = append(byServer[te.server], te)
	}
	var groups []string
	for _, server := range order {
		lines := []string{text("search_server", "name", server)}
		for _, te := range byServer[server] {
			switch detail {
			case "names":
				lines = append(lines, text("tool_name_item", "name", te.name))
			case "schemas":
				b, _ := json.Marshal(struct {
					Name        string          `json:"name"`
					Description string          `json:"description"`
					InputSchema json.RawMessage `json:"input_schema"`
				}{te.name, te.spec.Description, te.input})
				lines = append(lines, string(b))
			default:
				lines = append(lines, te.name+"\n"+te.spec.Description)
			}
		}
		groups = append(groups, strings.Join(lines, "\n"))
	}
	return strings.Join(groups, "\n\n")
}

// callMeta is mcp_call_tool: the one stable entry that runs a server tool in discovery mode.
type callMeta struct{ in *install }

func (m *callMeta) Spec() kernel.ToolSpec {
	spec := metaDefs[callTool]
	spec.Concurrency = kernel.Serial
	return spec
}

type callInput struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

func (m *callMeta) Call(ctx context.Context, tc *kernel.ToolContext, raw json.RawMessage) (kernel.ToolResult, error) {
	var q callInput
	if err := json.Unmarshal(raw, &q); err != nil {
		return kernel.ToolResult{}, &kernel.InputError{Detail: err.Error()}
	}
	te := find(discoverable(m.in, tc.Session(), nil), q.Name)
	if te == nil {
		return errResult(text("unknown_tool", "name", q.Name)), nil
	}
	if len(q.Arguments) == 0 || string(q.Arguments) == "null" {
		q.Arguments = json.RawMessage(`{}`)
	}
	if why := te.argsError(q.Arguments); why != "" {
		return errResult(text("bad_arguments", "name", te.name, "detail", why)), nil
	}
	return te.c.call(ctx, tc, te, q.Arguments)
}
