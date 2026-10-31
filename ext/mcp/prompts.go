package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/message"
)

// promptTimeout bounds prompts/get when the server sets no call timeout, since a command holds the session while it waits.
const promptTimeout = time.Minute

// promptEntry is one server prompt under the command name people type.
type promptEntry struct {
	name    string // mcp__<server>__<prompt>, lower case
	mcpName string
	server  string
	desc    string
	hint    string
	args    []*sdk.PromptArgument
	c       *conn
}

// promptKey marks a prompts/get request, so an input request it raises is never routed to a tool call in flight.
type promptKey struct{}

var notCommandChar = regexp.MustCompile(`[^a-z0-9_-]`)

// commandName maps a server's prompt name onto the kernel's command-name rule, hashing the tail of a name too long to keep; the server may carry capitals, so its part is lower-cased as the prompt's is.
func commandName(server, prompt string) string {
	n := "mcp__" + strings.ToLower(server) + "__" + notCommandChar.ReplaceAllString(strings.ToLower(prompt), "_")
	if len(n) <= 64 {
		return n
	}
	sum := sha256.Sum256([]byte(prompt))
	return n[:57] + "_" + hex.EncodeToString(sum[:])[:6]
}

// listPrompts reads every page of the server's prompts when the host enabled them and the server offers any; the ttl is when to list again.
func (c *conn) listPrompts(cs *sdk.ClientSession) ([]*promptEntry, time.Duration) {
	if !c.srv.Prompts {
		return nil, 0
	}
	if r := cs.InitializeResult(); r == nil || r.Capabilities == nil || r.Capabilities.Prompts == nil {
		return nil, 0
	}
	var out []*promptEntry
	cursor, ttl := "", time.Duration(0)
	for {
		res, err := cs.ListPrompts(c.ctx, &sdk.ListPromptsParams{Cursor: cursor})
		if err != nil {
			c.in.r.Agent().Logger().Warn("mcp: prompts not listed", "server", c.srv.Name, "error", err)
			return out, 0
		}
		if cursor == "" && res.TTLMs > 0 {
			ttl = time.Duration(res.TTLMs) * time.Millisecond
		}
		for _, p := range res.Prompts {
			if p != nil && p.Name != "" {
				out = append(out, c.promptEntry(p))
			}
		}
		if res.NextCursor == "" || res.NextCursor == cursor {
			return out, ttl
		}
		cursor = res.NextCursor
	}
}

func (c *conn) promptEntry(p *sdk.Prompt) *promptEntry {
	pe := &promptEntry{name: commandName(c.srv.Name, p.Name), mcpName: p.Name, server: c.srv.Name, args: p.Arguments, c: c}
	pe.desc = clip(strings.TrimSpace(p.Title), maxSummary)
	if pe.desc == "" {
		pe.desc = summary(p.Description, "")
	}
	var hint []string
	for _, a := range p.Arguments {
		if a == nil {
			continue
		}
		if a.Required {
			hint = append(hint, "<"+a.Name+">")
		} else {
			hint = append(hint, "["+a.Name+"]")
		}
	}
	pe.hint = strings.Join(hint, " ")
	return pe
}

// samePrompt says a registered command still describes the prompt, so it is not registered again.
func samePrompt(a, b *promptEntry) bool {
	return a != nil && b != nil && a.mcpName == b.mcpName && a.desc == b.desc && a.hint == b.hint
}

// syncCommands registers a command per listed prompt, again when one changed, and removes those gone; only people invoke them, so nothing is announced to the model.
func (in *install) syncCommands(prev, next *catalog) {
	for name, pe := range next.prompts {
		reg := in.cmds[name]
		if reg != nil && samePrompt(prev.prompts[name], pe) {
			continue
		}
		if reg != nil {
			reg.Remove()
			delete(in.cmds, name)
		}
		r, err := in.r.Command(in.promptCommand(pe))
		if err != nil {
			in.r.Agent().Logger().Warn("mcp: prompt command not registered", "server", pe.server, "prompt", pe.mcpName, "name", name, "error", err)
			continue
		}
		in.cmds[name] = r
	}
	for name, reg := range in.cmds {
		if next.prompts[name] == nil {
			reg.Remove()
			delete(in.cmds, name)
		}
	}
}

func (in *install) promptCommand(pe *promptEntry) kernel.Command {
	name := pe.name
	return kernel.Command{
		Name:        name,
		Description: pe.desc,
		ArgHint:     pe.hint,
		Run: func(ctx context.Context, cc *kernel.CommandContext) (kernel.CommandResult, error) {
			pe := in.cat.Load().prompts[name] // the latest listing, as tools read theirs
			if pe == nil {
				return kernel.CommandResult{}, fmt.Errorf("mcp: %s is no longer offered by its server", name)
			}
			return pe.run(ctx, cc.Args(), in.e.o.MaxResultChars)
		},
	}
}

// run fetches the prompt with the typed words as its arguments and turns its messages into one input.
func (pe *promptEntry) run(ctx context.Context, raw string, maxChars int) (kernel.CommandResult, error) {
	args, err := pe.arguments(kernel.SplitArgs(raw))
	if err != nil {
		return kernel.CommandResult{}, err
	}
	cs := pe.c.current()
	if cs == nil {
		return kernel.CommandResult{}, fmt.Errorf("mcp: the %s server cannot be reached right now", pe.server)
	}
	timeout := pe.c.srv.Timeout
	if timeout <= 0 {
		timeout = promptTimeout
	}
	ctx, cancel := context.WithTimeout(context.WithValue(ctx, promptKey{}, true), timeout)
	defer cancel()
	res, err := cs.GetPrompt(ctx, &sdk.GetPromptParams{Name: pe.mcpName, Arguments: args})
	if err != nil {
		return kernel.CommandResult{}, fmt.Errorf("mcp: %s failed on the %s server (%s): %w", pe.name, pe.server, errorClass(err), err)
	}
	in := promptInput(res.Messages, maxChars, pe.c.in.e.o.MaxMediaBytes)
	if in.Text == "" && len(in.Parts) == 0 {
		return kernel.CommandResult{}, fmt.Errorf("mcp: %s returned nothing to send", pe.name)
	}
	return kernel.CommandResult{Prompt: &in}, nil
}

// arguments gives the words to the prompt's arguments in order; words past the last argument join it, so free text needs no quotes.
func (pe *promptEntry) arguments(words []string) (map[string]string, error) {
	var names []string
	for _, a := range pe.args {
		if a != nil {
			names = append(names, a.Name)
		}
	}
	out := map[string]string{}
	for i, w := range words {
		switch {
		case len(names) == 0:
			return nil, fmt.Errorf("mcp: %s takes no arguments", pe.name)
		case i < len(names):
			out[names[i]] = w
		default:
			last := names[len(names)-1]
			out[last] += " " + w
		}
	}
	for _, a := range pe.args {
		if a != nil && a.Required && out[a.Name] == "" {
			return nil, fmt.Errorf("mcp: %s needs %s; usage: /%s %s", pe.name, a.Name, pe.name, pe.hint)
		}
	}
	return out, nil
}

// promptInput keeps a lone user message as it is and labels each message by role otherwise, since a command yields one user input.
func promptInput(msgs []*sdk.PromptMessage, maxChars, maxMedia int) kernel.Input {
	msgs = slices.DeleteFunc(slices.Clone(msgs), func(m *sdk.PromptMessage) bool { return m == nil })
	labeled := len(msgs) > 1 || len(msgs) == 1 && msgs[0].Role != "user"
	var texts []string
	var parts message.Content
	for _, m := range msgs {
		var t string
		if b := block(m.Content); b != nil {
			if tb, ok := media(b, maxMedia).(*message.TextBlock); ok {
				t = tb.Text
			} else {
				parts = append(parts, b)
			}
		}
		if labeled {
			label := text("prompt_user")
			if m.Role == "assistant" {
				label = text("prompt_assistant")
			}
			t = strings.TrimRight(label+"\n"+t, "\n")
		}
		if t != "" {
			texts = append(texts, t)
		}
	}
	var body []string
	for _, b := range truncate(message.Content{&message.TextBlock{Text: strings.Join(texts, "\n\n")}}, maxChars) {
		if t := b.(*message.TextBlock).Text; t != "" {
			body = append(body, t)
		}
	}
	return kernel.Input{Text: strings.Join(body, "\n"), Parts: parts}
}
