// Package skills lists Agent Skills in the system prompt and lets the model load one with the skill tool, or a person with /<name>; a loaded skill is pinned, so compaction keeps it.
package skills

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/message"
	"github.com/estonshi/aperture-potatos/spec"
)

// Name is the extension's name; it is also the tool's and the catalog section's.
const Name = "skills"

const (
	toolName        = "skill"
	pinPrefix       = "skill:"
	announceSource  = "ext:skills"
	defaultDescCap  = 1536
	defaultPinCap   = 5000
	maxResourceSize = 256 << 10
)

type Options struct {
	Sources             []Source
	MaxDescriptionChars int // default 1536
	PinTokens           int // default 5000 per activated skill
	Rewrite             RewriteFunc
}

// RewriteInput is what Rewrite receives: the body after $ARGUMENTS expansion.
type RewriteInput struct {
	Session   *kernel.Session
	Name      string
	Trigger   string // model or user
	Arguments string
	Body      string
}

// RewriteFunc may run the body's shell substitutions with the session's permissions before the model sees it; a model load calls it before deduplication, so a repeated load sees fresh text. An error ends the load with the denied text.
type RewriteFunc func(ctx context.Context, in RewriteInput) (string, error)

type ActivateInput struct {
	kernel.HookEnv
	Name         string   `json:"name"`
	Trigger      string   `json:"trigger"` // model or user
	AllowedTools []string `json:"allowed_tools,omitempty"`
}

type ActivateOutput struct {
	Deny   bool   `json:"deny,omitempty"`
	Reason string `json:"reason,omitempty"`
}

var Activate = kernel.NewHookPoint(kernel.HookPointDef[ActivateInput, ActivateOutput]{
	Name: "skill_activate",
	Key:  func(in ActivateInput) string { return in.Name },
	Merge: func(acc, out ActivateOutput) (ActivateOutput, bool) {
		if out.Deny {
			return out, true
		}
		return acc, false
	},
	Timeout: 10 * time.Second,
	Fail:    kernel.FailOpen,
})

// Activated reports a skill loaded by the model's tool call or a person's command.
type Activated struct {
	Name         string   `json:"name"`
	Trigger      string   `json:"trigger"`
	Tokens       int64    `json:"tokens"` // estimate of the body, by the heuristic of runtime.md §12.1
	Deduplicated bool     `json:"deduplicated"`
	AllowedTools []string `json:"allowed_tools,omitempty"`
}

func (*Activated) EventType() event.Type { return "skills.activated" }

// CatalogChanged reports skills added or removed after the extension was installed.
type CatalogChanged struct {
	Added   []string `json:"added"`
	Removed []string `json:"removed"`
}

func (*CatalogChanged) EventType() event.Type { return "skills.catalog_changed" }

// Payloads teaches an event.Decoder this extension's events.
func Payloads() []event.Constructor {
	return []event.Constructor{
		{Type: "skills.activated", New: func() event.Payload { return &Activated{} }},
		{Type: "skills.catalog_changed", New: func() event.Payload { return &CatalogChanged{} }},
	}
}

// instKey holds the extension on the agent or session that installed it, where AddSource finds it.
var instKey = kernel.NewKey[*ext]("skills.ext")

var ErrNotInstalled = errors.New("skills: the extension is not installed at this registrar's scope")

type entry struct {
	fm  Frontmatter
	src Source
}

type catalog struct {
	list   []Frontmatter // by name
	byName map[string]entry
}

type added struct {
	id  int
	src Source
}

type ext struct {
	o       Options
	r       kernel.Registrar
	mu      sync.Mutex // serializes catalog changes and the registrations they make
	sources []added
	nextID  int
	cat     atomic.Pointer[catalog]
	tool    *kernel.Registration
	cmds    map[string]*kernel.Registration
	ready   bool                 // commands may be registered: the host's own options are installed
	waiting *kernel.Registration // the session_start hook that sets ready
}

// New lists the sources once when installed; AddSource changes the catalog later.
func New(o Options) kernel.Extension {
	if o.MaxDescriptionChars <= 0 {
		o.MaxDescriptionChars = defaultDescCap
	}
	if o.PinTokens <= 0 {
		o.PinTokens = defaultPinCap
	}
	return kernel.ExtensionFunc(Name, func(r kernel.Registrar) error {
		e := &ext{o: o, r: r, cmds: map[string]*kernel.Registration{}}
		for _, src := range o.Sources {
			e.sources = append(e.sources, added{id: e.nextID, src: src})
			e.nextID++
		}
		e.cat.Store(&catalog{byName: map[string]entry{}})
		if r.Session() != nil {
			e.ready = true // a session's scope has no option-form registrations after its extensions
		} else {
			reg, err := kernel.On(r, kernel.SessionStart, Name, e.start)
			if err != nil {
				return err
			}
			e.waiting = reg
		}
		if _, err := r.PromptSection(kernel.PromptSection{Name: Name, Slot: kernel.SlotCatalog, Render: e.render}); err != nil {
			return err
		}
		if err := e.refresh(context.Background(), false); err != nil {
			return err
		}
		kernel.Set(holder(r), instKey, e)
		return nil
	})
}

// start registers the commands when the first session starts, after the host's WithCommand options, so a name the host took is skipped instead of failing its New.
func (e *ext) start(context.Context, kernel.SessionStartInput) (kernel.SessionStartOutput, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.ready {
		e.ready = true
		e.syncCommands(e.cat.Load())
		e.waiting.Remove()
	}
	return kernel.SessionStartOutput{}, nil
}

func holder(r kernel.Registrar) kernel.StateHolder {
	if s := r.Session(); s != nil {
		return s
	}
	return r.Agent()
}

// AddSource adds src after the others, so it never shadows a skill already listed; the new skills reach the open sessions with their next turn.
func AddSource(r kernel.Registrar, src Source) (*kernel.Registration, error) {
	e, ok := kernel.Get(holder(r), instKey)
	if !ok {
		return nil, ErrNotInstalled
	}
	e.mu.Lock()
	id := e.nextID
	e.nextID++
	e.sources = append(e.sources, added{id: id, src: src})
	e.mu.Unlock()
	if err := e.refresh(context.Background(), true); err != nil {
		e.drop(id)
		return nil, err
	}
	return kernel.NewRegistration(func() {
		e.drop(id)
		if err := e.refresh(context.Background(), true); err != nil {
			e.r.Agent().Logger().Warn("skills: the catalog keeps the removed source's skills until the next change", "error", err)
		}
	}), nil
}

func (e *ext) drop(id int) {
	e.mu.Lock()
	e.sources = slices.DeleteFunc(e.sources, func(a added) bool { return a.id == id })
	e.mu.Unlock()
}

// refresh rebuilds the catalog and the tool and commands that follow it, announcing the difference under the lock so two changes are announced in order.
func (e *ext) refresh(ctx context.Context, announce bool) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	srcs := make([]Source, len(e.sources))
	for i, a := range e.sources {
		srcs[i] = a.src
	}
	list, owners, shadows, err := mergeLists(ctx, srcs)
	if err != nil {
		return err
	}
	for _, sh := range shadows {
		e.r.Agent().Logger().Warn("skills shadowed", "name", sh.name, "kept", sh.winner, "shadowed", sh.loser)
	}
	next := &catalog{list: list, byName: map[string]entry{}}
	for _, fm := range list {
		next.byName[fm.Name] = entry{fm: fm, src: owners[fm.Name]}
	}
	prev := e.cat.Swap(next)
	diff := &CatalogChanged{Added: []string{}, Removed: []string{}}
	for _, fm := range list {
		if _, ok := prev.byName[fm.Name]; !ok {
			diff.Added = append(diff.Added, fm.Name)
		}
	}
	for _, fm := range prev.list {
		if _, ok := next.byName[fm.Name]; !ok {
			diff.Removed = append(diff.Removed, fm.Name)
		}
	}
	had := e.tool != nil
	e.syncTool(next)
	if had && e.tool != nil && !slices.Equal(prev.modelNames(), next.modelNames()) {
		kernel.ToolChanged(e.r, toolDef.Name)
	}
	e.syncCommands(next)
	if !announce || len(diff.Added)+len(diff.Removed) == 0 {
		return nil
	}
	text := announcement(next, diff, e.o.MaxDescriptionChars)
	if s := e.r.Session(); s != nil {
		s.Announce(announceSource, text, diff)
	} else {
		e.r.Agent().Announce(announceSource, text, diff)
	}
	return nil
}

func (e *ext) syncTool(c *catalog) {
	want := len(c.modelNames()) > 0
	switch {
	case want && e.tool == nil:
		reg, err := e.r.Tool(&skillTool{e: e})
		if err != nil {
			e.r.Agent().Logger().Warn("skills: the skill tool is not registered", "error", err)
			return
		}
		e.tool = reg
	case !want && e.tool != nil:
		e.tool.Remove()
		e.tool = nil
	}
}

func (e *ext) syncCommands(c *catalog) {
	if !e.ready {
		return
	}
	for name, reg := range e.cmds {
		if en, ok := c.byName[name]; !ok || !en.fm.UserInvocable {
			reg.Remove()
			delete(e.cmds, name)
		}
	}
	for _, fm := range c.list {
		if !fm.UserInvocable || e.cmds[fm.Name] != nil {
			continue
		}
		reg, err := e.r.Command(e.command(fm))
		if err != nil { // a name taken by another command, or one no command may have
			e.r.Agent().Logger().Warn("skills: no command for this skill", "name", fm.Name, "error", err)
			continue
		}
		e.cmds[fm.Name] = reg
	}
}

func (c *catalog) modelNames() []string {
	var out []string
	for _, fm := range c.list {
		if fm.ModelInvocable {
			out = append(out, fm.Name)
		}
	}
	return out
}

// ---- texts ----

var text = kernel.SpecTexts("ext/skills/texts.md")

var xmlEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

// attr writes an attribute value as a JSON string, as runtime.md §5.7 writes tag attributes.
func attr(s string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s)
	return strings.TrimSuffix(b.String(), "\n")
}

func cut(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// estimate is the kernel's text heuristic: ASCII at four bytes a token, any other code point one.
func estimate(s string) int64 {
	var ascii, other int64
	for _, r := range s {
		if r < utf8.RuneSelf {
			ascii++
		} else {
			other++
		}
	}
	return (ascii+3)/4 + other
}

func (e *ext) render(context.Context, kernel.RenderContext) (string, error) {
	var items []string
	for _, fm := range e.cat.Load().list {
		if fm.ModelInvocable {
			items = append(items, text("catalog_item", "name", xmlEscaper.Replace(fm.Name),
				"description", xmlEscaper.Replace(cut(fm.Description, e.o.MaxDescriptionChars)), "location", xmlEscaper.Replace(fm.Location)))
		}
	}
	if len(items) == 0 {
		return "", nil
	}
	return text("catalog", "skills", strings.Join(items, "\n")), nil
}

func announcement(c *catalog, d *CatalogChanged, descCap int) string {
	var parts, items []string
	for _, n := range d.Added {
		if fm := c.byName[n].fm; fm.ModelInvocable {
			items = append(items, text("added_item", "name", n, "description", cut(fm.Description, descCap)))
		}
	}
	if len(items) > 0 {
		parts = append(parts, text("added")+"\n"+strings.Join(items, "\n"))
	}
	if len(d.Removed) > 0 { // a removed skill's visibility is gone with it, so every one is named
		parts = append(parts, text("removed", "names", strings.Join(d.Removed, ", ")))
	}
	return strings.Join(parts, "\n\n")
}

// ---- activation ----

func env(s *kernel.Session, path []event.Hop) kernel.HookEnv {
	info := s.Info()
	return kernel.HookEnv{Session: s, SessionID: info.SessionID, Agent: info.Agent, Path: path}
}

// active reports whether the session's pin set already holds this delivered body, which compaction re-injects, so loading it again would only repeat it.
func active(s *kernel.Session, name, body string) (kernel.Pin, bool) {
	for _, p := range s.Pins() {
		if p.Key == pinPrefix+name && p.Content == body {
			return p, true
		}
	}
	return kernel.Pin{}, false
}

func (e *ext) rewrite(ctx context.Context, s *kernel.Session, name, trigger, args, body string) (string, error) {
	if e.o.Rewrite == nil {
		return body, nil
	}
	return e.o.Rewrite(ctx, RewriteInput{Session: s, Name: name, Trigger: trigger, Arguments: args, Body: body})
}

func (e *ext) pin(name, body string) *kernel.Pin {
	return &kernel.Pin{Key: pinPrefix + name, Content: body, MaxTokens: e.o.PinTokens}
}

// ---- the skill tool ----

type skillTool struct{ e *ext }

type toolInput struct {
	Name     string  `json:"name"`
	Args     *string `json:"args,omitempty"`
	Resource string  `json:"resource,omitempty"`
}

var toolDef = func() (d struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}) {
	if err := json.Unmarshal([]byte(spec.MustText("ext/skills/skill.json")), &d); err != nil {
		panic("ext/skills: skill.json: " + err.Error())
	}
	return d
}()

// Spec lists the names the model may load at every freeze point; Call checks the current catalog, so a name added since is loadable too.
func (t *skillTool) Spec() kernel.ToolSpec {
	return kernel.ToolSpec{Name: toolDef.Name, Description: toolDef.Description, InputSchema: withEnum(toolDef.InputSchema, t.e.cat.Load().modelNames()), Concurrency: kernel.Parallel}
}

func (t *skillTool) Call(ctx context.Context, tc *kernel.ToolContext, raw json.RawMessage) (kernel.ToolResult, error) {
	var in toolInput
	if err := json.Unmarshal(raw, &in); err != nil {
		return kernel.ToolResult{}, err
	}
	c := t.e.cat.Load()
	en, ok := c.byName[in.Name]
	if !ok || !en.fm.ModelInvocable {
		return kernel.ErrorResult("%s", text("unknown_skill", "name", in.Name, "names", strings.Join(c.modelNames(), ", "))), nil
	}
	if in.Resource != "" {
		return t.e.resource(ctx, en, in.Name, in.Resource)
	}
	s := tc.Session()
	out, _ := kernel.Fire(ctx, s, Activate, ActivateInput{HookEnv: env(s, tc.Path()), Name: in.Name, Trigger: "model", AllowedTools: en.fm.AllowedTools})
	if out.Deny {
		return kernel.ErrorResult("%s", text("denied", "name", in.Name, "reason", out.Reason)), nil
	}
	sk, err := en.src.Load(ctx, in.Name)
	if errors.Is(err, ErrNotFound) {
		return kernel.ErrorResult("%s", text("unknown_skill", "name", in.Name, "names", strings.Join(c.modelNames(), ", "))), nil
	}
	if err != nil {
		return kernel.ToolResult{}, err
	}
	delivered := sk.Body
	args := ""
	if in.Args != nil {
		args = *in.Args
		delivered = kernel.ExpandPrompt(sk.Body, args)
	}
	delivered, err = t.e.rewrite(ctx, s, in.Name, "model", args, delivered)
	if err != nil {
		return kernel.ErrorResult("%s", text("denied", "name", in.Name, "reason", err.Error())), nil
	}
	if p, ok := active(s, in.Name, delivered); ok {
		tc.Emit(&Activated{Name: in.Name, Trigger: "model", Tokens: estimate(p.Content), Deduplicated: true, AllowedTools: sk.AllowedTools})
		return kernel.TextResult(text("already_active", "name", in.Name)), nil
	}
	body := strings.ReplaceAll(delivered, "</skill_content>", `<\/skill_content>`)
	out2 := text("content", "name", attr(in.Name), "location", attr(sk.Location), "body", body)
	if len(sk.Resources) > 0 {
		files := make([]string, len(sk.Resources))
		for i, r := range sk.Resources {
			files[i] = text("resource_file", "path", xmlEscaper.Replace(r))
		}
		out2 += "\n" + text("resources", "files", strings.Join(files, "\n"))
	}
	tc.Emit(&Activated{Name: in.Name, Trigger: "model", Tokens: estimate(delivered), AllowedTools: sk.AllowedTools})
	res := kernel.TextResult(out2)
	res.Pin = t.e.pin(in.Name, delivered)
	return res, nil
}

// resource reads one file of the skill, at most 256 KiB of text; it neither activates nor pins.
func (e *ext) resource(ctx context.Context, en entry, name, rel string) (kernel.ToolResult, error) {
	rc, err := en.src.Open(ctx, name, rel)
	switch {
	case errors.Is(err, ErrInvalidPath):
		return kernel.ErrorResult("%s", text("invalid_resource", "path", rel)), nil
	case errors.Is(err, ErrNotFound):
		return kernel.ErrorResult("%s", text("no_resource", "name", name, "path", rel)), nil
	case err != nil:
		return kernel.ToolResult{}, err
	}
	defer rc.Close()
	buf, err := io.ReadAll(io.LimitReader(rc, maxResourceSize+1))
	if err != nil {
		return kernel.ToolResult{}, err
	}
	over := len(buf) > maxResourceSize
	if over {
		buf = buf[:maxResourceSize]
		for len(buf) > 0 && !utf8.Valid(buf) && len(buf) > maxResourceSize-utf8.UTFMax { // a rune cut at the limit is not a sign of binary
			buf = buf[:len(buf)-1]
		}
	}
	if bytes.IndexByte(buf, 0) >= 0 || !utf8.Valid(buf) {
		return kernel.ErrorResult("%s", text("resource_binary", "path", rel)), nil
	}
	out := string(buf)
	if over {
		out += "\n" + text("resource_truncated", "limit", strconv.Itoa(maxResourceSize))
	}
	return kernel.TextResult(out), nil
}

// withEnum sets the name enum; the schema comes from skill.json, so its shape is known.
func withEnum(schema json.RawMessage, names []string) json.RawMessage {
	var s, props, nm map[string]json.RawMessage
	if json.Unmarshal(schema, &s) != nil || json.Unmarshal(s["properties"], &props) != nil || json.Unmarshal(props["name"], &nm) != nil {
		return schema
	}
	if names == nil {
		names = []string{}
	}
	enum, _ := json.Marshal(names)
	nm["enum"] = enum
	props["name"] = ordered(nm, "type", "enum", "description")
	s["properties"] = ordered(props, "name", "args", "resource")
	return ordered(s, "type", "properties", "required", "additionalProperties")
}

// ordered writes an object with its members in the given order, so the definition's bytes stay what skill.json says.
func ordered(m map[string]json.RawMessage, keys ...string) json.RawMessage {
	var b strings.Builder
	b.WriteByte('{')
	for i, k := range keys {
		if i > 0 {
			b.WriteByte(',')
		}
		name, _ := json.Marshal(k)
		b.Write(name)
		b.WriteByte(':')
		b.Write(m[k])
	}
	b.WriteByte('}')
	return json.RawMessage(b.String())
}

// ---- the /<name> command ----

func (e *ext) command(fm Frontmatter) kernel.Command {
	name := fm.Name
	hint, _ := fm.Extra["argument-hint"].(string)
	return kernel.Command{
		Name:        name,
		Description: cut(fm.Description, e.o.MaxDescriptionChars),
		ArgHint:     hint,
		Run: func(ctx context.Context, cc *kernel.CommandContext) (kernel.CommandResult, error) {
			en, ok := e.cat.Load().byName[name]
			if !ok {
				return kernel.CommandResult{}, ErrNotFound
			}
			s := cc.Session()
			out, _ := kernel.Fire(ctx, s, Activate, ActivateInput{HookEnv: env(s, nil), Name: name, Trigger: "user", AllowedTools: en.fm.AllowedTools})
			if out.Deny {
				return kernel.CommandResult{Text: text("denied", "name", name, "reason", out.Reason)}, nil
			}
			sk, err := en.src.Load(ctx, name)
			if err != nil {
				return kernel.CommandResult{}, err
			}
			args := cc.Args()
			delivered := kernel.ExpandPrompt(sk.Body, args) // the body expands like any prompt command
			delivered, err = e.rewrite(ctx, s, name, "user", args, delivered)
			if err != nil {
				return kernel.CommandResult{Text: text("denied", "name", name, "reason", err.Error())}, nil
			}
			in := kernel.Text(delivered)
			in.Origin = message.Origin{Kind: message.OriginCommand, Source: name}
			s.Emit(&Activated{Name: name, Trigger: "user", Tokens: estimate(delivered), AllowedTools: sk.AllowedTools})
			return kernel.CommandResult{Prompt: &in, Pin: e.pin(name, delivered)}, nil
		},
	}
}
