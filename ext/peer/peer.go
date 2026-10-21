// Package peer lets the model talk to other agents, in this process or over A2A, each exchange a task it can wait on or run in the background.
package peer

import (
	"context"
	"encoding/json"
	"errors"
	"iter"
	"regexp"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/kernel"
)

// Name is the extension's name, which std.Without takes.
const Name = "peer"

// Kind is the task kind of one exchange with a peer.
const Kind = "a2a"

type TaskState string

const (
	StateSubmitted     TaskState = "submitted"
	StateWorking       TaskState = "working"
	StateInputRequired TaskState = "input_required"
	StateAuthRequired  TaskState = "auth_required"
	StateCompleted     TaskState = "completed"
	StateFailed        TaskState = "failed"
	StateCanceled      TaskState = "canceled"
	StateRejected      TaskState = "rejected"
)

func (s TaskState) Terminal() bool {
	return s == StateCompleted || s == StateFailed || s == StateCanceled || s == StateRejected
}

// EndsRound reports whether an exchange is over for now: terminal, or waiting on the caller.
func (s TaskState) EndsRound() bool {
	return s.Terminal() || s == StateInputRequired || s == StateAuthRequired
}

type Card struct {
	Name        string      `json:"name"`
	Description string      `json:"description"`
	Version     string      `json:"version,omitempty"`
	Skills      []CardSkill `json:"skills,omitempty"`
	Streaming   bool        `json:"streaming"`
	URL         string      `json:"url,omitempty"` // empty for in-process peers
}

type CardSkill struct {
	ID          string   `json:"id"`
	Name        string   `json:"name"`
	Description string   `json:"description"`
	Tags        []string `json:"tags,omitempty"`
	Examples    []string `json:"examples,omitempty"`
}

// Part mirrors A2A v1.0: exactly one of Text, Raw, URL, Data is set.
type Part struct {
	Text      string          `json:"text,omitempty"`
	Raw       []byte          `json:"raw,omitempty"`
	URL       string          `json:"url,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
	MediaType string          `json:"media_type,omitempty"`
	Filename  string          `json:"filename,omitempty"`
}

type Message struct {
	ID        string                     `json:"message_id"`
	ContextID string                     `json:"context_id,omitempty"`
	TaskID    string                     `json:"task_id,omitempty"`
	Role      string                     `json:"role"` // user | agent
	Parts     []Part                     `json:"parts"`
	Metadata  map[string]json.RawMessage `json:"metadata,omitempty"`
}

type Artifact struct {
	ID    string `json:"artifact_id"`
	Name  string `json:"name,omitempty"`
	Parts []Part `json:"parts"`
}

type EventKind string

const (
	EventTask     EventKind = "task"
	EventMessage  EventKind = "message"
	EventStatus   EventKind = "status"
	EventArtifact EventKind = "artifact"
)

// Event mirrors the four A2A StreamResponse variants; a round ends at a message, or at a status whose state ends it, so a task snapshot is followed by its artifacts and, when it has ended, a status.
type Event struct {
	Kind      EventKind `json:"kind"`
	TaskID    string    `json:"task_id,omitempty"`
	ContextID string    `json:"context_id,omitempty"`
	State     TaskState `json:"state,omitempty"`
	Message   *Message  `json:"message,omitempty"`
	Artifact  *Artifact `json:"artifact,omitempty"`
	Append    bool      `json:"append,omitempty"`
	LastChunk bool      `json:"last_chunk,omitempty"`
}

type SendOptions struct {
	HistoryLength int
}

type Peer interface {
	Card() Card
	Send(ctx context.Context, msg Message, opt SendOptions) iter.Seq2[Event, error]
	Subscribe(ctx context.Context, taskID string) iter.Seq2[Event, error]
	Cancel(ctx context.Context, taskID string) error
}

// Versioned is implemented by a peer that speaks a chosen protocol version, which an exchange saves with its state.
type Versioned interface {
	ProtocolVersion() string
}

// Named gives p the name the model uses instead of its card's, for a card whose name is not made of [a-zA-Z0-9_-].
func Named(p Peer, name string) Peer { return named{Peer: p, name: name} }

type named struct {
	Peer
	name string
}

func (n named) Card() Card {
	c := n.Peer.Card()
	c.Name = n.name
	return c
}

// ProtocolVersion passes the wrapped peer's on, so a named remote peer still saves its version with the state.
func (n named) ProtocolVersion() string {
	if v, ok := n.Peer.(Versioned); ok {
		return v.ProtocolVersion()
	}
	return ""
}

// AuthError ends a Send or Subscribe that the peer rejected for credentials, so the a2a task can ask the host before retrying.
type AuthError struct {
	Status int
	Detail string
}

func (e *AuthError) Error() string {
	if e.Detail == "" {
		return "peer: authorization required"
	}
	return "peer: authorization required: " + e.Detail
}

var (
	ErrNotInstalled = errors.New("peer: the extension is not installed in this scope or its agent")
	// ErrUnknownTask is a peer that no longer knows the task, expired or cleaned up.
	ErrUnknownTask = errors.New("peer: the peer does not know this task")
)

// AuthInput.Status is the HTTP status of a rejected request, and 0 when the task itself entered AUTH_REQUIRED.
type AuthInput struct {
	kernel.HookEnv
	Peer      string          `json:"peer"`
	TaskID    string          `json:"task_id,omitempty"`
	ContextID string          `json:"context_id,omitempty"`
	Status    int             `json:"status,omitempty"`
	Detail    string          `json:"detail,omitempty"`
	Data      json.RawMessage `json:"data,omitempty"`
	Attempt   int             `json:"attempt"`
}

// AuthOutput.Retry means the host fixed the credentials out of band, usually inside the Doer.
type AuthOutput struct {
	Retry    bool                       `json:"retry,omitempty"`
	Metadata map[string]json.RawMessage `json:"metadata,omitempty"`
	Reason   string                     `json:"reason,omitempty"`
}

var AuthRequired = kernel.NewHookPoint(kernel.HookPointDef[AuthInput, AuthOutput]{
	Name: "peer_auth_required",
	Key:  func(in AuthInput) string { return in.Peer },
	Merge: func(acc, out AuthOutput) (AuthOutput, bool) {
		if out.Retry {
			return out, true
		}
		return acc, false
	},
	Timeout: 10 * time.Minute, // a person may have to finish a login
	Fail:    kernel.FailOpen,
})

// AuthRequiredEvent is peer.auth_required, published under the task's hop before the hook runs.
type AuthRequiredEvent struct {
	Peer      string `json:"peer"`
	TaskID    string `json:"task_id,omitempty"`
	ContextID string `json:"context_id,omitempty"`
	Status    int    `json:"status,omitempty"`
	Detail    string `json:"detail,omitempty"`
	Attempt   int    `json:"attempt"`
}

func (*AuthRequiredEvent) EventType() event.Type { return "peer.auth_required" }

// Payloads teaches an event.Decoder this extension's events.
func Payloads() []event.Constructor {
	return []event.Constructor{{Type: "peer.auth_required", New: func() event.Payload { return &AuthRequiredEvent{} }}}
}

// ---- registry ----

// entry is one registered peer under the name the model uses.
type entry struct {
	name string
	p    Peer
	card Card
}

// scope is the peers one agent or session registered, and the start tool it registered for them; mu orders changes, readers take the snapshot.
type scope struct {
	mu    sync.Mutex
	peers atomic.Pointer[[]entry]
	tool  *kernel.Registration
}

func (sc *scope) list() []entry {
	if p := sc.peers.Load(); p != nil {
		return *p
	}
	return nil
}

var (
	instKey  = kernel.NewKey[*ext]("peer.ext")
	scopeKey = kernel.NewKey[*scope]("peer.scope")
)

var validName = regexp.MustCompile(`^[a-zA-Z0-9_-]{1,64}$`)

// ext is what New installed in one scope.
type ext struct {
	r kernel.Registrar
}

// New registers the a2a kind and the peers; a2a_send and the catalog appear only while some peer is registered.
func New(peers ...Peer) kernel.Extension {
	return kernel.ExtensionFunc(Name, func(r kernel.Registrar) error {
		e := &ext{r: r}
		if _, err := r.TaskKind(kernel.TaskKind{Name: Kind, Description: "Exchanges messages with another agent.",
			Caps: kernel.TaskCaps{Instructions: true, Continue: true, Restart: true, Inspect: true}, New: newTask}); err != nil {
			return err
		}
		if _, err := r.PromptSection(kernel.PromptSection{Name: Name, Slot: kernel.SlotCatalog, Render: render}); err != nil {
			return err
		}
		kernel.Set(holder(r), instKey, e)
		for _, p := range peers {
			if _, err := add(r, p, false); err != nil {
				return err
			}
		}
		return nil
	})
}

// Register adds p in r's scope under its card's name made safe for the model; a session's peer shadows its agent's of the same name.
func Register(r kernel.Registrar, p Peer) (*kernel.Registration, error) {
	if _, ok := kernel.Get(holder(r), instKey); !ok {
		if r.Session() == nil {
			return nil, ErrNotInstalled
		}
		if _, ok := kernel.Get(r.Agent(), instKey); !ok {
			return nil, ErrNotInstalled
		}
	}
	return add(r, p, true)
}

func add(r kernel.Registrar, p Peer, announce bool) (*kernel.Registration, error) {
	card := p.Card()
	name := safeName(card.Name)
	sc := scopeOf(r)
	sc.mu.Lock()
	if slices.ContainsFunc(sc.list(), func(x entry) bool { return x.name == name }) {
		sc.mu.Unlock()
		return nil, kernel.ErrDuplicate
	}
	next := append(slices.Clone(sc.list()), entry{name: name, p: p, card: card})
	sc.peers.Store(&next)
	had := sc.tool != nil
	err := syncTool(r, sc)
	sc.mu.Unlock()
	if err != nil {
		remove(r, sc, name, false)
		return nil, err
	}
	if had {
		kernel.ToolChanged(r, toolDef.Name)
	}
	if announce {
		publish(r, text("added")+"\n"+text("added_item", "name", name, "description", oneLine(card.Description)))
	}
	return kernel.NewRegistration(func() { remove(r, sc, name, true) }), nil
}

func remove(r kernel.Registrar, sc *scope, name string, announce bool) {
	sc.mu.Lock()
	n := len(sc.list())
	next := slices.DeleteFunc(slices.Clone(sc.list()), func(x entry) bool { return x.name == name })
	sc.peers.Store(&next)
	gone := len(next) < n
	_ = syncTool(r, sc)
	kept := sc.tool != nil
	sc.mu.Unlock()
	if gone && kept {
		kernel.ToolChanged(r, toolDef.Name)
	}
	if gone && announce {
		publish(r, text("removed", "names", name))
	}
}

// publish tells the open sessions of the scope about a change, since their catalog was rendered when their epoch began.
func publish(r kernel.Registrar, msg string) {
	if s := r.Session(); s != nil {
		s.Announce(Name, msg, nil)
		return
	}
	r.Agent().Announce(Name, msg, nil)
}

// syncTool registers the scope's a2a_send while it has peers; a session's own tool shadows its agent's and lists both.
func syncTool(r kernel.Registrar, sc *scope) error {
	switch want := len(sc.list()) > 0; {
	case want && sc.tool == nil:
		reg, err := r.Tool(&sendTool{agent: r.Agent(), session: r.Session()})
		if err != nil {
			return err
		}
		sc.tool = reg
	case !want && sc.tool != nil:
		sc.tool.Remove()
		sc.tool = nil
	}
	return nil
}

func holder(r kernel.Registrar) kernel.StateHolder {
	if s := r.Session(); s != nil {
		return s
	}
	return r.Agent()
}

func scopeOf(r kernel.Registrar) *scope {
	var sc *scope
	kernel.Update(holder(r), scopeKey, func(old *scope, ok bool) *scope {
		if !ok || old == nil {
			old = &scope{}
		}
		sc = old
		return old
	})
	return sc
}

func peersOf(h kernel.StateHolder) []entry {
	sc, _ := kernel.Get(h, scopeKey)
	if sc == nil {
		return nil
	}
	return sc.list()
}

// view is the peers a session can reach now, by name, its own shadowing its agent's.
func view(s *kernel.Session) map[string]entry {
	out := map[string]entry{}
	for _, en := range slices.Concat(peersOf(s.Registrar().Agent()), peersOf(s)) {
		out[en.name] = en
	}
	return out
}

// safeName keeps a card name the model can use as an enum value, replacing anything else with an underscore.
func safeName(name string) string {
	if validName.MatchString(name) {
		return name
	}
	b := []rune(name)
	if len(b) > 64 {
		b = b[:64]
	}
	for i, c := range b {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-') {
			b[i] = '_'
		}
	}
	if len(b) == 0 {
		return "agent"
	}
	return string(b)
}

// ---- catalog ----

var (
	xmlText = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")
	xmlAttr = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;")
)

// foreign is card text from the peer, which could carry runtime tags or turn markers.
func foreign(s string) string { return xmlText.Replace(kernel.SanitizeForeign(s)) }

func render(_ context.Context, rc kernel.RenderContext) (string, error) {
	if rc.Session == nil {
		return "", nil
	}
	v := view(rc.Session)
	if len(v) == 0 {
		return "", nil
	}
	var items []string
	for _, name := range sortedNames(v) {
		c := v[name].card
		skills := ""
		if len(c.Skills) > 0 {
			var lines []string
			for _, sk := range c.Skills {
				n := sk.Name
				if n == "" {
					n = sk.ID
				}
				lines = append(lines, text("catalog_skill", "name", `"`+xmlAttr.Replace(kernel.SanitizeForeign(n))+`"`, "description", foreign(oneLine(sk.Description))))
			}
			skills = text("catalog_skills", "skills", strings.Join(lines, "\n")) + "\n"
		}
		items = append(items, text("catalog_item", "name", name, "description", foreign(c.Description), "skills", skills))
	}
	return text("catalog", "agents", strings.Join(items, "\n")), nil
}

func sortedNames(v map[string]entry) []string {
	names := make([]string, 0, len(v))
	for n := range v {
		names = append(names, n)
	}
	slices.Sort(names)
	return names
}

func oneLine(s string) string { return strings.Join(strings.Fields(s), " ") }

// ---- texts ----

var text = kernel.SpecTexts("ext/peer/texts.md")
