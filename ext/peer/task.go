package peer

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"iter"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/estonshi/aperture-potatos/kernel"
	"github.com/estonshi/aperture-potatos/message"
)

const (
	maxAuthRounds = 3
	reportEvery   = 500 * time.Millisecond
	progressChars = 200
	cancelWait    = 2 * time.Second // the remote cancel of a stop, inside the default tool_cancel_grace_ms
	joiner        = "\n\n"
)

// saved is the exchange's state: where the conversation is, so a continuation or a resumed session picks it up.
type saved struct {
	Peer            string `json:"peer"`
	ContextID       string `json:"context_id,omitempty"`
	TaskID          string `json:"task_id,omitempty"`
	ProtocolVersion string `json:"protocol_version,omitempty"`
}

var (
	contextsKey = kernel.NewKey[map[string]string]("peer.contexts") // on a session: peer -> the context of its latest exchange
	locksKey    = kernel.NewKey[*locks]("peer.locks")               // on a session: one exchange per conversation at a time
)

// runtimeKey hands an in-process peer the task that drives it, so it can relay events and book calls.
type runtimeKey struct{}

func withRuntime(ctx context.Context, rt kernel.TaskRuntime) context.Context {
	return context.WithValue(ctx, runtimeKey{}, rt)
}

func runtimeFrom(ctx context.Context) kernel.TaskRuntime {
	rt, _ := ctx.Value(runtimeKey{}).(kernel.TaskRuntime)
	return rt
}

// newMessageID makes message ids, which the client keeps across its own retries.
var newMessageID = func() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6], b[8] = b[6]&0x0f|0x40, b[8]&0x3f|0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:]
}

func newTask(spec kernel.TaskSpec) (kernel.Task, error) {
	var in input
	if err := json.Unmarshal(spec.Input, &in); err != nil {
		return nil, fmt.Errorf("invalid a2a input: %w", err)
	}
	return &task{in: in}, nil
}

// task is one attempt; Inspect reads its exchange while Run drives it.
type task struct {
	in input
	mu sync.Mutex
	x  *exchange
}

func (t *task) Run(ctx context.Context, rt kernel.TaskRuntime) (kernel.TaskResult, error) {
	owner := rt.Session()
	name := t.in.Agent
	var st saved
	cont := rt.Spec().Continue
	if cont != nil {
		_ = json.Unmarshal(cont.State, &st)
		if st.Peer != "" {
			name = st.Peer
		}
	}
	en, ok := view(owner)[name]
	if !ok {
		return kernel.TaskResult{}, errors.New(text("agent_gone", "agent", name))
	}
	chain := t.in.Chain
	if len(chain) == 0 {
		chain = []string{owner.Info().Agent} // a restart with a new input has no chain
	}
	if slices.Contains(chain, name) {
		return kernel.TaskResult{}, errors.New(cyclic(chain, name))
	}
	st.Peer = name
	if v, ok := en.p.(Versioned); ok {
		st.ProtocolVersion = v.ProtocolVersion()
	}
	x := &exchange{rt: rt, owner: owner, p: en.p, name: name, chain: chain, st: st}
	t.mu.Lock()
	t.x = x
	t.mu.Unlock()
	return x.run(ctx, t.in, cont) // the state is saved once the peer names the conversation, since the peer alone is already in the input
}

// Inspect asks the peer where its task is now, or gives the last report before there is one.
func (t *task) Inspect(ctx context.Context) (kernel.TaskProgress, error) {
	t.mu.Lock()
	x := t.x
	t.mu.Unlock()
	if x == nil {
		return kernel.TaskProgress{}, nil
	}
	st, last := x.where()
	if st.TaskID == "" {
		return last, nil
	}
	ictx, cancel := context.WithCancel(ctx)
	defer cancel()
	for ev, err := range x.p.Subscribe(ictx, st.TaskID) {
		if err != nil {
			return last, err
		}
		return x.progress(ev.State, statusText(ev), "", st), nil
	}
	return last, nil
}

// exchange is the state of one attempt's conversation with the peer.
type exchange struct {
	rt    kernel.TaskRuntime
	owner *kernel.Session
	p     Peer
	name  string
	chain []string

	mu   sync.Mutex
	st   saved
	last kernel.TaskProgress

	next    []kernel.TaskInstruction // next_loop and interrupt instructions, merged into one message
	queue   []kernel.TaskInstruction // queue instructions, one message each
	cut     []kernel.TaskInstruction // interrupts whose cancel is on its way
	replies []string
	blocks  message.Content
	auths   int
	lock    *held     // used by the Run goroutine only
	sent    time.Time // of the last throttled report
	state   TaskState // how the last finished round ended
	remote  TaskState // where the peer's current task is
}

func (x *exchange) where() (saved, kernel.TaskProgress) {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.st, x.last
}

// note records what the peer said about where the conversation is; the peer's ids always win.
func (x *exchange) note(ctx context.Context, contextID, taskID string) {
	if x.ids(contextID, taskID) {
		x.adopt(ctx, contextID)
	}
}

// ids keeps the peer's ids and saves them; it runs where the peer's events are read, so the progress it precedes carries them.
func (x *exchange) ids(contextID, taskID string) bool {
	x.mu.Lock()
	changed := false
	if contextID != "" && contextID != x.st.ContextID {
		x.st.ContextID, changed = contextID, true
	}
	if taskID != "" && taskID != x.st.TaskID {
		x.st.TaskID, changed = taskID, true
	}
	x.mu.Unlock()
	if changed {
		x.save()
	}
	return changed
}

// adopt makes the conversation the session's latest with the peer and holds its lock, on the Run goroutine that owns the lock.
func (x *exchange) adopt(ctx context.Context, contextID string) {
	if contextID == "" {
		return
	}
	if x.lock == nil || x.lock.key != lockKey(x.name, contextID) {
		remember(x.owner, x.name, contextID)
		_ = x.relock(ctx, contextID, true) // a new conversation nobody else knows yet, so this never waits
	}
}

func (x *exchange) save() {
	x.mu.Lock()
	b, _ := json.Marshal(x.st)
	x.mu.Unlock()
	x.rt.SaveState(b)
}

// run drives the attempt: the first message, or a continuation, then every pending instruction in turn.
func (x *exchange) run(ctx context.Context, in input, cont *kernel.TaskContinuation) (kernel.TaskResult, error) {
	defer x.unlock()
	var msg *Message
	var follow iter.Seq2[Event, error]
	switch {
	case cont != nil:
		m, f, err := x.resume(ctx, cont.Instruction)
		if err != nil {
			return x.fail(ctx, err)
		}
		msg, follow = m, f
	default:
		ctxID := ""
		if !in.NewConversation && x.rt.Attempt() == 1 {
			ctxID = contextFor(x.owner, x.name)
		}
		msg = x.message(in.Message, ctxID, "", nil)
	}
	for {
		end, err := x.round(ctx, msg, follow)
		follow = nil
		if err != nil {
			return x.fail(ctx, err)
		}
		switch {
		case end.state == StateCompleted || end.only:
			x.reply(end)
		case end.state == StateCanceled && end.cut:
			// the interrupt's message goes out next, merged after what was pending
		case end.state == StateInputRequired:
			ans, err := x.ask(ctx, end)
			if err != nil {
				return x.fail(ctx, err)
			}
			msg = ans
			continue
		case end.state == StateAuthRequired:
			m, f, err := x.authorized(ctx, end)
			if err != nil {
				return x.fail(ctx, err)
			}
			msg, follow = m, f
			continue
		default:
			return x.fail(ctx, x.ended(end))
		}
		x.take()
		if msg = x.pending(); msg == nil {
			return x.result(), nil
		}
	}
}

// resume continues a finished task: it checks first where the peer's task is, since a crash may have lost the last saved state.
func (x *exchange) resume(ctx context.Context, ins kernel.TaskInstruction) (*Message, iter.Seq2[Event, error], error) {
	st, _ := x.where()
	if st.TaskID == "" {
		return x.message(ins.Text, st.ContextID, "", nil), nil, nil
	}
	var check iter.Seq2[Event, error] // the stream that answers the host's metadata, which shows where the task went
	for {
		src := check
		if src == nil {
			src = x.p.Subscribe(withRuntime(ctx, x.rt), st.TaskID)
		}
		check = nil
		snap, rest, stop, err := first(src)
		var ae *AuthError
		switch {
		case errors.Is(err, ErrUnknownTask):
			return x.message(ins.Text, st.ContextID, "", nil), nil, nil
		case errors.As(err, &ae):
			if _, err := x.authorize(ctx, ae.Status, ae.Detail, nil); err != nil {
				return nil, nil, err
			}
			continue
		case err != nil:
			return nil, nil, err
		}
		x.note(ctx, snap.ContextID, snap.TaskID)
		if snap.State == StateSubmitted || snap.State == StateWorking {
			x.hold(ins) // its round is still going: the instruction waits for it to end
			return nil, prepend(snap, rest), nil
		}
		stop()
		switch {
		case snap.State == StateInputRequired:
			return x.message(ins.Text, st.ContextID, st.TaskID, nil), nil, nil
		case snap.State == StateAuthRequired:
			out, err := x.authorize(ctx, 0, statusText(snap), nil)
			if err != nil {
				x.cancelRemote(ctx, st.TaskID)
				return nil, nil, err
			}
			if len(out.Metadata) > 0 {
				m := x.message("", st.ContextID, st.TaskID, out.Metadata)
				m.Parts = nil
				check = x.p.Send(withRuntime(ctx, x.rt), *m, SendOptions{})
			}
			continue
		}
		return x.message(ins.Text, st.ContextID, "", nil), nil, nil
	}
}

// roundEnd is how one round ended.
type roundEnd struct {
	state   TaskState
	only    bool   // the peer answered with a message and no task
	text    string // the reply: artifacts in order, then the final status message
	message string // the final status message alone
	taskID  string
	cut     bool // canceled by an interrupt from this side
}

// round sends msg, or follows a stream already open, and reads the peer's events and the inbox until the round ends.
func (x *exchange) round(ctx context.Context, msg *Message, follow iter.Seq2[Event, error]) (roundEnd, error) {
	st, _ := x.where()
	if msg != nil {
		st.ContextID = msg.ContextID
	}
	if st.ContextID != "" {
		if err := x.relock(ctx, st.ContextID, false); err != nil {
			return roundEnd{}, err
		}
	}
	x.setRemote(StateSubmitted)
	rctx, cancel := context.WithCancel(withRuntime(ctx, x.rt))
	defer cancel()
	seq := follow
	if seq == nil {
		seq = x.p.Send(rctx, *msg, SendOptions{})
	}
	asm := &assembly{}
	// the progress is reported where the events are read, in order with what an in-process peer relays there
	events := pump(rctx, seq, func(ev Event) (bool, roundEnd) {
		x.ids(ev.ContextID, ev.TaskID)
		done, end := asm.add(ev)
		if asm.state != "" {
			x.setRemote(asm.state)
		}
		if end.only {
			x.setRemote(StateCompleted)
		}
		if !done {
			x.report(asm.state, asm.status, asm.tail(), asm.changed)
			return false, end
		}
		st, _ := x.where()
		end.taskID = st.TaskID
		end.cut = end.state == StateCanceled && x.cutting()
		x.report(end.state, end.message, "", true)
		return true, end
	})
	got := false
	for {
		select {
		case it, ok := <-events:
			if !ok {
				if !got {
					return roundEnd{}, errors.New("the peer closed the exchange without an answer")
				}
				return roundEnd{}, errors.New("the peer's stream ended before the round did")
			}
			if it.err != nil {
				var ae *AuthError
				if errors.As(it.err, &ae) {
					m, f, err := x.rejected(ctx, msg, ae)
					if err != nil {
						return roundEnd{}, err
					}
					cancel()
					return x.round(ctx, m, f)
				}
				return roundEnd{}, it.err
			}
			got = true
			x.adopt(ctx, it.ev.ContextID)
			if it.done {
				return it.end, nil
			}
		case ins := <-x.rt.Inbox():
			x.instruct(ctx, ins)
		case <-ctx.Done():
			return roundEnd{}, ctx.Err()
		}
	}
}

// rejected handles a request refused for credentials: once the host fixed them, the same message goes again, or a task that exists is followed.
func (x *exchange) rejected(ctx context.Context, msg *Message, ae *AuthError) (*Message, iter.Seq2[Event, error], error) {
	if _, err := x.authorize(ctx, ae.Status, ae.Detail, nil); err != nil {
		return nil, nil, err
	}
	st, _ := x.where()
	if st.TaskID == "" && msg != nil {
		return msg, nil, nil // the refused request made no task, so sending it again duplicates nothing
	}
	return nil, x.p.Subscribe(withRuntime(ctx, x.rt), st.TaskID), nil
}

// authorized handles a task waiting for credentials: the host's metadata goes to it, then its stream is followed again.
func (x *exchange) authorized(ctx context.Context, end roundEnd) (*Message, iter.Seq2[Event, error], error) {
	out, err := x.authorize(ctx, 0, end.message, nil)
	if err != nil {
		x.cancelRemote(ctx, end.taskID)
		return nil, nil, err
	}
	st, _ := x.where()
	if len(out.Metadata) > 0 {
		m := x.message("", st.ContextID, st.TaskID, out.Metadata)
		m.Parts = nil
		return m, nil, nil
	}
	return nil, x.p.Subscribe(withRuntime(ctx, x.rt), st.TaskID), nil
}

// authorize asks the host through peer_auth_required; without a hook, a refusal or a third failure in this attempt, the exchange fails.
func (x *exchange) authorize(ctx context.Context, status int, detail string, data json.RawMessage) (AuthOutput, error) {
	x.mu.Lock()
	x.auths++
	n := x.auths
	st := x.st
	x.mu.Unlock()
	if n > maxAuthRounds {
		return AuthOutput{}, errors.New(text("auth_failed", "agent", x.name))
	}
	x.rt.Emit(&AuthRequiredEvent{Peer: x.name, TaskID: st.TaskID, ContextID: st.ContextID, Status: status, Detail: detail, Attempt: n})
	info := x.owner.Info()
	out, _ := kernel.Fire(ctx, x.owner, AuthRequired, AuthInput{HookEnv: kernel.HookEnv{Session: x.owner, SessionID: info.SessionID, Agent: info.Agent, TurnID: x.rt.TurnID()},
		Peer: x.name, TaskID: st.TaskID, ContextID: st.ContextID, Status: status, Detail: detail, Data: data, Attempt: n})
	if !out.Retry || ctx.Err() != nil {
		return out, errors.New(text("auth_failed", "agent", x.name))
	}
	return out, nil
}

// instruct files an instruction that arrives while the peer works: an interrupt cancels the peer's task, the rest wait for the round to end.
func (x *exchange) instruct(ctx context.Context, ins kernel.TaskInstruction) {
	st, _ := x.where()
	x.mu.Lock()
	switch {
	case ins.Mode == kernel.ModeQueue:
		x.queue = append(x.queue, ins)
	case ins.Mode == kernel.ModeInterrupt && st.TaskID != "":
		x.next = append(x.next, ins)
		x.cut = append(x.cut, ins)
	default:
		x.next = append(x.next, ins)
	}
	x.mu.Unlock()
	x.holding()
	if ins.Mode == kernel.ModeInterrupt && st.TaskID != "" {
		go func() { _ = x.p.Cancel(withRuntime(context.WithoutCancel(ctx), x.rt), st.TaskID) }()
	}
}

func (x *exchange) hold(ins kernel.TaskInstruction) {
	x.mu.Lock()
	if ins.Mode == kernel.ModeQueue {
		x.queue = append(x.queue, ins)
	} else {
		x.next = append(x.next, ins)
	}
	x.mu.Unlock()
	x.holding()
}

func (x *exchange) cutting() bool {
	x.mu.Lock()
	defer x.mu.Unlock()
	return len(x.cut) > 0
}

// holding reports the instructions taken and not yet sent, so an early end lists them as unprocessed.
func (x *exchange) holding() {
	x.mu.Lock()
	all := slices.Concat(x.next, x.queue)
	x.mu.Unlock()
	x.rt.Holding(all)
}

// take reads what is already in the inbox before the attempt decides it is done.
func (x *exchange) take() {
	for {
		select {
		case ins := <-x.rt.Inbox():
			x.hold(ins)
		default:
			return
		}
	}
}

// pending is the next message: the merged next_loop and interrupt instructions first, then one queue instruction.
func (x *exchange) pending() *Message {
	x.mu.Lock()
	var text string
	switch {
	case len(x.next) > 0:
		text = merge(x.next)
		x.next, x.cut = nil, nil
	case len(x.queue) > 0:
		text = x.queue[0].Text
		x.queue = x.queue[1:]
	default:
		x.mu.Unlock()
		return nil
	}
	ctxID := x.st.ContextID
	x.mu.Unlock()
	x.holding()
	return x.message(text, ctxID, "", nil)
}

// ask hands the peer's question to the session and waits for its answer, which goes out with the instructions pending before it.
func (x *exchange) ask(ctx context.Context, end roundEnd) (*Message, error) {
	q := end.message
	if q == "" {
		q = text("needs_input", "agent", x.name)
	}
	x.unlock() // nobody may send into the conversation, but waiting here must not block the other exchanges
	x.rt.AskInput(kernel.SanitizeForeign(q))
	for {
		select {
		case ins := <-x.rt.Inbox():
			if !ins.Answer {
				x.hold(ins)
				continue
			}
			x.mu.Lock()
			parts := slices.Concat(x.next, []kernel.TaskInstruction{ins})
			x.next, x.cut = nil, nil
			st := x.st
			x.mu.Unlock()
			x.holding()
			return x.message(merge(parts), st.ContextID, st.TaskID, nil), nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

func merge(ins []kernel.TaskInstruction) string {
	texts := make([]string, len(ins))
	for i, in := range ins {
		texts[i] = in.Text
	}
	return strings.Join(texts, joiner)
}

func (x *exchange) message(body, contextID, taskID string, meta map[string]json.RawMessage) *Message {
	chain, _ := json.Marshal(x.chain)
	m := map[string]json.RawMessage{chainKey: chain}
	maps.Copy(m, meta)
	return &Message{ID: newMessageID(), ContextID: contextID, TaskID: taskID, Role: "user", Parts: []Part{{Text: body}}, Metadata: m}
}

func (x *exchange) reply(end roundEnd) {
	body := kernel.SanitizeForeign(end.text)
	shown := body
	if shown == "" {
		shown = text("no_reply")
	}
	shown = strings.ReplaceAll(shown, "</agent_reply>", "<\\/agent_reply>")
	block := text("reply", "agent", `"`+xmlAttr.Replace(x.name)+`"`, "task_id", `"`+xmlAttr.Replace(end.taskID)+`"`, "text", shown)
	x.mu.Lock()
	x.replies = append(x.replies, body)
	x.blocks = append(x.blocks, &message.TextBlock{Text: block})
	x.state = end.state
	if end.only {
		x.state = StateCompleted
	}
	x.mu.Unlock()
}

func (x *exchange) result() kernel.TaskResult {
	x.mu.Lock()
	defer x.mu.Unlock()
	data, _ := json.Marshal(map[string]any{"peer": x.name, "context_id": x.st.ContextID, "task_id": x.st.TaskID, "state": x.state})
	return kernel.TaskResult{Summary: strings.Join(x.replies, joiner), Content: slices.Clone(x.blocks), Data: data}
}

// fail ends the attempt: a stop cancels the peer's unfinished task first, and the replies so far stay as a partial result.
func (x *exchange) fail(ctx context.Context, err error) (kernel.TaskResult, error) {
	if ctx.Err() != nil {
		st, _ := x.where()
		x.cancelRemote(ctx, st.TaskID)
		if c := context.Cause(ctx); c != nil {
			err = c
		}
		return x.result(), err
	}
	return x.result(), err
}

func (x *exchange) setRemote(s TaskState) {
	x.mu.Lock()
	x.remote = s
	x.mu.Unlock()
}

func (x *exchange) cancelRemote(ctx context.Context, taskID string) {
	x.mu.Lock()
	done := x.remote.Terminal()
	x.mu.Unlock()
	if taskID == "" || done {
		return
	}
	cctx, cancel := context.WithTimeout(withRuntime(context.WithoutCancel(ctx), x.rt), cancelWait)
	defer cancel()
	_ = x.p.Cancel(cctx, taskID)
}

func (x *exchange) ended(end roundEnd) error {
	x.mu.Lock()
	x.state = end.state
	x.mu.Unlock()
	if end.message == "" {
		return errors.New(text("ended", "agent", x.name, "state", string(end.state)))
	}
	return errors.New(text("ended_with", "agent", x.name, "state", string(end.state), "message", kernel.SanitizeForeign(end.message)))
}

// report turns the peer's status and artifacts into task progress: a change of state at once, the rest at most every half second.
func (x *exchange) report(state TaskState, status, tail string, now bool) {
	st, _ := x.where()
	p := x.progress(state, status, tail, st)
	x.mu.Lock()
	x.last = p
	if !now && time.Since(x.sent) < reportEvery {
		x.mu.Unlock()
		return
	}
	x.sent = time.Now()
	x.mu.Unlock()
	x.rt.Report(p)
}

func (x *exchange) progress(state TaskState, status, tail string, st saved) kernel.TaskProgress {
	t := status
	if t == "" {
		t = tail
	}
	data, _ := json.Marshal(struct {
		State     TaskState `json:"state,omitempty"`
		ContextID string    `json:"context_id,omitempty"`
		TaskID    string    `json:"task_id,omitempty"`
	}{state, st.ContextID, st.TaskID})
	return kernel.TaskProgress{Text: kernel.SanitizeForeign(t), Data: data}
}

// ---- reading a round ----

// assembly builds the reply of one round from the peer's events.
type assembly struct {
	order   []string
	parts   map[string][]Part
	state   TaskState
	status  string
	changed bool // the last event changed the state
	recent  string
}

// add takes one event; true means the round is over.
func (a *assembly) add(ev Event) (bool, roundEnd) {
	if a.parts == nil {
		a.parts = map[string][]Part{}
	}
	a.changed = false
	switch ev.Kind {
	case EventMessage:
		body := ""
		if ev.Message != nil {
			body = partsText(ev.Message.Parts)
		}
		return true, roundEnd{state: StateCompleted, only: true, text: body}
	case EventArtifact:
		if ev.Artifact == nil {
			return false, roundEnd{}
		}
		id := ev.Artifact.ID
		if _, ok := a.parts[id]; !ok {
			a.order = append(a.order, id)
		}
		if ev.Append {
			a.parts[id] = appendParts(a.parts[id], ev.Artifact.Parts)
		} else {
			a.parts[id] = slices.Clone(ev.Artifact.Parts)
		}
		a.recent = id
		return false, roundEnd{}
	case EventTask, EventStatus:
		if ev.State != "" && ev.State != a.state {
			a.state, a.changed = ev.State, true
		}
		if ev.Message != nil {
			a.status = partsText(ev.Message.Parts)
		} else if ev.Kind == EventStatus {
			a.status = ""
		}
		if ev.Kind == EventStatus && a.state.EndsRound() { // a task snapshot's artifacts and its closing status follow it
			return true, a.end()
		}
	}
	return false, roundEnd{}
}

func (a *assembly) end() roundEnd {
	var texts []string
	for _, id := range a.order {
		if t := partsText(a.parts[id]); t != "" {
			texts = append(texts, t)
		}
	}
	body := strings.Join(texts, "\n\n")
	if a.status != "" && a.status != body && a.state == StateCompleted {
		body = strings.TrimSpace(body + "\n\n" + a.status)
	}
	return roundEnd{state: a.state, text: body, message: a.status}
}

// tail is the end of the artifact being received, which progress shows while there is no status message.
func (a *assembly) tail() string {
	t := []rune(partsText(a.parts[a.recent]))
	if len(t) > progressChars {
		t = t[len(t)-progressChars:]
	}
	return string(t)
}

// appendParts adds a chunk, joining adjacent text parts into one.
func appendParts(have, more []Part) []Part {
	out := slices.Clone(have)
	for _, p := range more {
		if n := len(out); n > 0 && isText(out[n-1]) && isText(p) {
			out[n-1].Text += p.Text
			continue
		}
		out = append(out, p)
	}
	return out
}

func isText(p Part) bool { return p.Raw == nil && p.URL == "" && p.Data == nil && p.Filename == "" }

func partsText(parts []Part) string {
	var b strings.Builder
	for _, p := range parts {
		var s string
		switch {
		case p.Raw != nil || p.Filename != "" && p.URL == "":
			s = "[file " + p.Filename + " (" + p.MediaType + ", " + strconv.Itoa(len(p.Raw)) + " bytes)]"
		case p.URL != "":
			s = "[url " + p.URL + "]"
		case p.Data != nil:
			s = string(p.Data)
		default:
			s = p.Text
		}
		if s == "" {
			continue
		}
		if b.Len() > 0 && !isText(p) {
			b.WriteString("\n")
		}
		b.WriteString(s)
	}
	return b.String()
}

func statusText(ev Event) string {
	if ev.Message == nil {
		return ""
	}
	return partsText(ev.Message.Parts)
}

// ---- streams ----

type item struct {
	ev   Event
	err  error
	done bool // the round ended with this event
	end  roundEnd
}

// pump reads a peer's events off the loop, so the inbox is still read while the peer works; see takes each event first.
func pump(ctx context.Context, seq iter.Seq2[Event, error], see func(Event) (bool, roundEnd)) <-chan item {
	ch := make(chan item)
	go func() {
		defer close(ch)
		for ev, err := range seq {
			it := item{ev: ev, err: err}
			if err == nil {
				it.done, it.end = see(ev)
			}
			select {
			case ch <- it:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()
	return ch
}

// first reads the first event of a stream and hands back the rest, which stop releases when it is not read.
func first(seq iter.Seq2[Event, error]) (Event, iter.Seq2[Event, error], func(), error) {
	next, stop := iter.Pull2(seq)
	ev, err, ok := next()
	if !ok {
		stop()
		return Event{}, nil, nil, errors.New("the peer sent nothing")
	}
	if err != nil {
		stop()
		return Event{}, nil, nil, err
	}
	rest := func(yield func(Event, error) bool) {
		defer stop()
		for {
			ev, err, ok := next()
			if !ok || !yield(ev, err) || err != nil {
				return
			}
		}
	}
	return ev, rest, stop, nil
}

func prepend(ev Event, rest iter.Seq2[Event, error]) iter.Seq2[Event, error] {
	return func(yield func(Event, error) bool) {
		if !yield(ev, nil) {
			for range rest {
				break
			}
			return
		}
		for ev, err := range rest {
			if !yield(ev, err) {
				return
			}
		}
	}
}

// ---- the session's conversations ----

func remember(s *kernel.Session, peer, contextID string) {
	kernel.Update(s, contextsKey, func(old map[string]string, _ bool) map[string]string {
		m := make(map[string]string, len(old)+1)
		maps.Copy(m, old)
		m[peer] = contextID
		return m
	})
}

// contextFor is the conversation the next message to peer continues: the latest one, rebuilt from the task table after a resume.
func contextFor(s *kernel.Session, peer string) string {
	if m, _ := kernel.Get(s, contextsKey); m != nil {
		if id, ok := m[peer]; ok {
			return id
		}
	}
	var best kernel.TaskInfo
	found := ""
	for _, info := range s.Tasks().List() {
		if info.Kind != Kind || len(info.State) == 0 {
			continue
		}
		var st saved
		if json.Unmarshal(info.State, &st) != nil || st.Peer != peer || st.ContextID == "" {
			continue
		}
		if found == "" || !info.StartedAt.Before(best.StartedAt) {
			best, found = info, st.ContextID
		}
	}
	if found != "" {
		remember(s, peer, found)
	}
	return found
}

// locks lets one exchange at a time talk in a conversation, the others waiting in order.
type locks struct {
	mu   sync.Mutex
	byID map[string]*convLock
}

type convLock struct {
	holder  string // task id of the exchange holding it
	waiters []waiter
}

type waiter struct {
	id string
	ch chan struct{}
}

// held is the lock an exchange holds.
type held struct {
	key string
	ls  *locks
}

func lockKey(peer, contextID string) string { return peer + "\x00" + contextID }

func locksOf(s *kernel.Session) *locks {
	var ls *locks
	kernel.Update(s, locksKey, func(old *locks, ok bool) *locks {
		if !ok || old == nil {
			old = &locks{byID: map[string]*convLock{}}
		}
		ls = old
		return old
	})
	return ls
}

// relock holds the conversation's lock, waiting behind the exchanges that came first; try fails quietly instead of waiting.
func (x *exchange) relock(ctx context.Context, contextID string, try bool) error {
	key := lockKey(x.name, contextID)
	if x.lock != nil && x.lock.key == key {
		return nil
	}
	x.unlock()
	ls := locksOf(x.owner)
	me := x.rt.ID()
	for {
		ls.mu.Lock()
		l := ls.byID[key]
		if l == nil {
			ls.byID[key] = &convLock{holder: me}
			ls.mu.Unlock()
			x.lock = &held{key: key, ls: ls}
			return nil
		}
		if try {
			ls.mu.Unlock()
			return nil
		}
		ch := make(chan struct{})
		l.waiters = append(l.waiters, waiter{id: me, ch: ch})
		holder := l.holder
		ls.mu.Unlock()
		x.rt.Report(kernel.TaskProgress{Text: text("waiting", "task_id", holder, "agent", x.name)})
		select {
		case <-ch:
			ls.mu.Lock()
			if l := ls.byID[key]; l != nil && l.holder == me {
				ls.mu.Unlock()
				x.lock = &held{key: key, ls: ls}
				return nil
			}
			ls.mu.Unlock()
		case <-ctx.Done():
			ls.mu.Lock()
			if l := ls.byID[key]; l != nil {
				if l.holder == me { // handed over while it gave up
					ls.mu.Unlock()
					x.lock = &held{key: key, ls: ls}
					x.unlock()
					return ctx.Err()
				}
				l.waiters = slices.DeleteFunc(l.waiters, func(w waiter) bool { return w.ch == ch })
			}
			ls.mu.Unlock()
			return ctx.Err()
		}
	}
}

// unlock hands the conversation to the exchange that waited longest.
func (x *exchange) unlock() {
	h := x.lock
	if h == nil {
		return
	}
	x.lock = nil
	h.ls.mu.Lock()
	defer h.ls.mu.Unlock()
	l := h.ls.byID[h.key]
	if l == nil {
		return
	}
	if len(l.waiters) == 0 {
		delete(h.ls.byID, h.key)
		return
	}
	next := l.waiters[0]
	l.waiters = l.waiters[1:]
	l.holder = next.id
	close(next.ch)
}
