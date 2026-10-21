// Package history provides search_history, which lets the model find what compaction took out of its own context, or read its parent's current context.
package history

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/spec"
)

// Name is the extension's name, which std.Without takes.
const Name = "history"

// maxLine is how many code points reading one line returns before cutting it.
const maxLine = 20000

type input struct {
	Query  string `json:"query,omitempty"`
	Seq    int64  `json:"seq,omitempty"`
	Parent bool   `json:"parent,omitempty"`
	Limit  int    `json:"limit,omitempty"`
}

// New registers search_history at agent scope, so forks and side chats send the same definition as their source.
func New() kernel.Extension {
	return kernel.ExtensionFunc(Name, func(r kernel.Registrar) error {
		var def struct {
			Name        string          `json:"name"`
			Description string          `json:"description"`
			InputSchema json.RawMessage `json:"input_schema"`
		}
		if err := json.Unmarshal([]byte(spec.MustText("ext/history/search_history.json")), &def); err != nil {
			return err
		}
		_, err := r.Tool(kernel.NewTool(kernel.ToolSpec{Name: def.Name, Description: def.Description, InputSchema: def.InputSchema, Concurrency: kernel.Parallel}, call))
		return err
	})
}

// text renders one of the extension's text files, given as name-value pairs; a malformed file is a build defect.
func text(name string, vars ...string) string {
	m := map[string]string{}
	for i := 0; i+1 < len(vars); i += 2 {
		m[vars[i]] = vars[i+1]
	}
	s, err := kernel.RenderTemplate(strings.TrimSpace(spec.MustText("ext/history/"+name)), m)
	if err != nil {
		panic("ext/history: " + name + ": " + err.Error())
	}
	return s
}

func call(ctx context.Context, tc *kernel.ToolContext, in input) (kernel.ToolResult, error) {
	if (strings.TrimSpace(in.Query) == "") == (in.Seq == 0) {
		return kernel.ErrorResult("%s", text("need_one.md")), nil
	}
	s := tc.Session()
	if in.Parent { // a child session sees only what its parent still holds, so an old epoch there is not found
		if s.Info().SideChat != "" { // every request of a side chat already carries its main session's current context
			return kernel.ErrorResult("%s", text("side_chat_parent.md")), nil
		}
		if s = s.Parent(); s == nil {
			return kernel.ErrorResult("%s", text("no_parent.md")), nil
		}
	}
	if in.Seq != 0 {
		m, current, err := s.HistoryLine(ctx, in.Seq)
		if errors.Is(err, kernel.ErrUnknownLine) || err == nil && in.Parent && !current {
			return kernel.ErrorResult("%s", text("no_line.md", "seq", strconv.FormatInt(in.Seq, 10))), nil
		}
		if err != nil {
			return kernel.ToolResult{}, err
		}
		return kernel.TextResult(header(m.Seq, m.Epoch, m.Role, m.TurnID, "") + "\n" + body(m)), nil
	}
	hits, err := s.SearchHistory(ctx, kernel.HistoryQuery{Words: strings.Fields(in.Query), Limit: in.Limit, Current: in.Parent})
	if err != nil {
		return kernel.ToolResult{}, err
	}
	if len(hits) == 0 {
		return kernel.TextResult(text("no_matches.md")), nil
	}
	lines := make([]string, len(hits))
	for i, h := range hits {
		in := "no"
		if h.Current {
			in = "yes"
		}
		lines[i] = header(h.Seq, h.Epoch, h.Role, h.TurnID, in) + " " + strings.Join(strings.Fields(h.Snippet), " ")
	}
	return kernel.TextResult(strings.Join(lines, "\n")), nil
}

func header(seq int64, epoch int, role message.Role, turn, current string) string {
	h := fmt.Sprintf("seq=%d epoch=%d role=%s", seq, epoch, role)
	if turn != "" {
		h += " turn=" + turn
	}
	if current != "" {
		h += " in_context=" + current
	}
	return h + ":"
}

// body is the line's searchable text, one block per line, cut at maxLine code points.
func body(m message.Message) string {
	var parts []string
	var add func(c message.Content)
	add = func(c message.Content) {
		for _, b := range c {
			switch v := b.(type) {
			case *message.TextBlock:
				parts = append(parts, v.Text)
			case *message.RefusalBlock:
				parts = append(parts, v.Text)
			case *message.ReminderBlock:
				parts = append(parts, v.Text)
			case *message.ToolCallBlock:
				parts = append(parts, v.Name+" "+string(v.Input))
			case *message.ToolResultBlock:
				add(v.Content)
			}
		}
	}
	add(m.Content)
	out := []rune(strings.Join(parts, "\n"))
	if len(out) <= maxLine {
		return string(out)
	}
	return string(out[:maxLine]) + "\n" + text("truncated.md", "limit", strconv.Itoa(maxLine))
}
