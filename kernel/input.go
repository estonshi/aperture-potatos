package kernel

import (
	"cmp"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/internal/prompt"
	"github.com/estonshi/aperture-potatos/message"
)

// InputMode decides when an input reaches the model; next_loop is the default so a correction lands in the running turn.
type InputMode string

const (
	ModeQueue     InputMode = "queue"     // waits for the running query to return, then runs as a turn of its own
	ModeNextLoop  InputMode = "next_loop" // reaches the running turn before its next model call
	ModeInterrupt InputMode = "interrupt" // interrupts the running turn, then runs first
)

// Input is one piece of content handed to a session; it needs Text or Parts.
type Input struct {
	Text     string            `json:"text,omitempty"`
	Parts    message.Content   `json:"parts,omitempty"`    // images, documents
	Origin   message.Origin    `json:"origin,omitzero"`    // human (default), command, agent or reminder; Send rejects the other kinds with ErrInvalidOrigin
	Metadata map[string]string `json:"metadata,omitempty"` // opaque to the kernel; visible to ToolContext.InputMetadata
}

// Text is a human input with the given text, the common case for Send and Run.
func Text(s string) Input { return Input{Text: s, Origin: message.Origin{Kind: message.OriginHuman}} }

// SendOption configures one Send or Run.
type SendOption func(*sendConfig)

type sendConfig struct {
	mode    InputMode
	noWake  bool
	noParse bool
	origin  *message.Origin
	meta    map[string]string
}

// WithMode sets when this input reaches the model, overriding the session's default mode.
func WithMode(m InputMode) SendOption { return func(c *sendConfig) { c.mode = m } }

// NoWake never starts a turn: the input waits for the next safe point or the next turn.
func NoWake() SendOption { return func(c *sendConfig) { c.noWake = true } }

// NoCommandParsing sends a human text that starts with a slash as plain text, even when it names a command.
func NoCommandParsing() SendOption { return func(c *sendConfig) { c.noParse = true } }

// WithOrigin replaces the input's Origin, for example to mark it as coming from another agent.
func WithOrigin(o message.Origin) SendOption { return func(c *sendConfig) { c.origin = &o } }

// WithMetadata adds keys to the input's Metadata, overriding keys it already has.
func WithMetadata(md map[string]string) SendOption { return func(c *sendConfig) { c.meta = md } }

// WithInputMode sets the mode of inputs sent without WithMode; SessionInputMode wins over it.
func WithInputMode(m InputMode) Option { return func(c *config) { c.inputMode = m } }

// SessionInputMode sets the default mode of this session's inputs, overriding WithInputMode.
func SessionInputMode(m InputMode) SessionOption { return func(c *sessionConfig) { c.inputMode = m } }

var modeRank = map[InputMode]int{ModeQueue: 0, ModeNextLoop: 1, ModeInterrupt: 2}

// queueItem is one entry of the unified input queue; WaitIdle and InterruptFirst commands sit here too, with cmd set.
type queueItem struct {
	id       string
	seq      uint64 // arrival order; a command's prompt reuses the command's seq
	origin   message.Origin
	mode     InputMode
	wake     bool
	parts    []*itemPart // parts[0] is the item itself; later parts were folded in by a merge
	omitted  int         // reminder parts a merge dropped, announced when the item is delivered
	reserved bool        // holds a Send reservation until it is delivered or dropped
	tail     bool        // left by a turn's end, so it speaks of the context that turn ran in, which a clear or rewind drops
	cmd      *queuedCommand
}

type itemPart struct {
	id       string
	origin   message.Origin
	text     string
	content  message.Content
	metadata map[string]string
	receipt  *Receipt
	unpin    string    // the pin key this reminder retires, carried onto its block
	note     *taskNote // set on a task notification
}

func (it *queueItem) ids() []string {
	out := make([]string, len(it.parts))
	for i, p := range it.parts {
		out[i] = p.id
	}
	return out
}

func (it *queueItem) size() int {
	n := 0
	for _, p := range it.parts {
		n += len(p.text)
	}
	return n
}

// humanish marks input written by a person or another agent; only such input breaks a chain of automatic turns.
func humanish(o message.Origin) bool {
	return o.Kind == message.OriginHuman || o.Kind == message.OriginCommand || o.Kind == message.OriginAgent
}

// canWake reports whether it may start a turn by itself; reminder items lose that right at the automatic-turn cap, and in a child session while its task does not run.
func (st *state) canWake(it *queueItem, maxAuto int) bool {
	return it.cmd == nil && it.wake && (humanish(it.origin) || under(st.autoTurns, maxAuto) && !st.held())
}

// principal names the turn and the delivered message: the first human, command or agent item, else the first task notification, else the first item.
func principal(items []*queueItem) *queueItem {
	if i := slices.IndexFunc(items, func(it *queueItem) bool { return humanish(it.origin) }); i >= 0 {
		return items[i]
	}
	if i := slices.IndexFunc(items, func(it *queueItem) bool { return it.origin.Kind == message.OriginTask }); i >= 0 {
		return items[i]
	}
	return items[0]
}

// barrier is the seq of the earliest queued command; nothing behind it may be delivered before that command has run.
func (st *state) barrier() uint64 {
	if i := slices.IndexFunc(st.queue, func(it *queueItem) bool { return it.cmd != nil }); i >= 0 {
		return st.queue[i].seq
	}
	return math.MaxUint64
}

func (st *state) first(pick func(*queueItem) bool) *queueItem {
	if i := slices.IndexFunc(st.queue, pick); i >= 0 {
		return st.queue[i]
	}
	return nil
}

// insert keeps st.queue in seq order; a command's prompt, which reuses the command's seq, lands ahead of input that arrived after the command.
func (st *state) insert(it *queueItem) {
	i, _ := slices.BinarySearchFunc(st.queue, it.seq, func(q *queueItem, seq uint64) int { return cmp.Compare(q.seq, seq) })
	st.queue = slices.Insert(st.queue, i, it)
}

// takeIf removes the matching items and returns them in arrival order.
func (st *state) takeIf(pick func(*queueItem) bool) []*queueItem {
	var out, keep []*queueItem
	for _, it := range st.queue {
		if pick(it) {
			out = append(out, it)
		} else {
			keep = append(keep, it)
		}
	}
	st.queue = keep
	return out
}

func (st *state) removeAt(i int) *queueItem {
	it := st.queue[i]
	st.queue = slices.Delete(st.queue, i, i+1)
	return it
}

type turnPlan struct {
	id         string
	items      []*queueItem
	trigger    TurnTrigger
	humanInput bool
	model      modelState  // the model when the turn starts
	switchTo   *modelState // a pending SwitchModel, which takes effect at this turn's start
}

func (p *turnPlan) inputIDs() []string {
	var out []string
	for _, it := range p.items {
		out = append(out, it.ids()...)
	}
	return out
}

// nextWork applies the idle rule of spec/runtime.md to the items ahead of the barrier and returns nothing while only passengers wait there.
func (st *state) nextWork(maxAuto int) (*turnPlan, *queuedCommand) {
	if i := slices.IndexFunc(st.queue, func(it *queueItem) bool { return it.cmd != nil && it.mode == ModeInterrupt }); i >= 0 {
		return nil, st.removeAt(i).cmd
	}
	bar := st.barrier()
	wakes := func(it *queueItem) bool { return it.seq < bar && st.canWake(it, maxAuto) }
	lead := st.first(func(it *queueItem) bool { return wakes(it) && humanish(it.origin) && it.mode != ModeQueue })
	if lead == nil {
		lead = st.first(func(it *queueItem) bool { return wakes(it) && humanish(it.origin) }) // a person's queue item goes before automatic turns
	}
	if lead == nil {
		lead = st.first(func(it *queueItem) bool { return wakes(it) && it.mode != ModeQueue })
	}
	if lead == nil {
		lead = st.first(wakes)
	}
	if lead == nil {
		if i := slices.IndexFunc(st.queue, func(it *queueItem) bool { return it.cmd != nil }); i >= 0 {
			return nil, st.removeAt(i).cmd // a WaitIdle command with nothing that can start a turn ahead of it
		}
		return nil, nil
	}
	items := st.takeIf(func(it *queueItem) bool {
		switch {
		case it == lead:
			return true
		case it.seq >= bar:
			return false // commands and everything that arrived after the first of them wait
		case !wakes(it):
			return true // passengers ride along with whatever turn starts
		case lead.mode != ModeQueue:
			return it.mode != ModeQueue // waking next_loop and interrupt items share one turn
		default:
			return humanish(lead.origin) && !humanish(it.origin) // reminder items ride along with a person's queue item
		}
	})
	p := principal(items)
	return &turnPlan{items: items, trigger: TurnTrigger(p.origin.Kind), humanInput: humanish(p.origin)}, nil
}

// takeAtSafePoint hands the running turn the next_loop items and passengers ahead of the barrier; waking queue and interrupt items wait for the turn to end.
func (st *state) takeAtSafePoint(maxAuto int) []*queueItem {
	bar := st.barrier()
	items := st.takeIf(func(it *queueItem) bool {
		return it.seq < bar && (it.mode == ModeNextLoop || !st.canWake(it, maxAuto))
	})
	if slices.ContainsFunc(items, func(it *queueItem) bool { return humanish(it.origin) }) {
		st.turn.humanInput = true
		st.autoTurns = 0
	}
	return items
}

// enqueue admits a new item; Send, command prompts and kernel notes all pass here.
func (s *Session) enqueue(it *queueItem) {
	st := &s.st
	if st.phase == phaseClosing || st.phase == phaseClosed {
		s.drop(it, "session_closing")
		return
	}
	if it.seq == 0 {
		st.seq++
		it.seq = st.seq
	}
	if !s.coalesce(it) {
		st.insert(it)
		s.preempt(it)
	}
	s.publish(&event.InputAccepted{InputID: it.id, Mode: string(it.mode), Origin: it.origin, Text: it.parts[0].text, QueueLen: len(st.queue)})
	s.flushPreempt()
}

// coalesce merges reminders while the queue is at capacity; it never merges across a queued command, so no input moves past a barrier.
func (s *Session) coalesce(it *queueItem) bool {
	st, limit := &s.st, s.limits.MaxQueuedInputs
	if it.origin.Kind != message.OriginReminder || limit < 0 {
		return false
	}
	for len(st.queue) >= limit && s.mergeOldestPair() {
	}
	if len(st.queue) < limit {
		return false
	}
	after := st.afterLastCommand()
	dst := after(func(q *queueItem) bool {
		return q.origin.Kind == message.OriginReminder && q.origin.Source == it.origin.Source
	})
	if dst == nil {
		dst = after(func(q *queueItem) bool { return q.origin.Kind == message.OriginReminder })
	}
	if dst == nil {
		return false // nothing to merge into, so the reminder is queued above the limit
	}
	s.absorb(dst, it)
	return true
}

// afterLastCommand finds the oldest matching item that no queued command separates from a new arrival.
func (st *state) afterLastCommand() func(func(*queueItem) bool) *queueItem {
	start := 0
	for i, q := range st.queue {
		if q.cmd != nil {
			start = i + 1
		}
	}
	return func(pick func(*queueItem) bool) *queueItem {
		for _, q := range st.queue[start:] {
			if pick(q) {
				return q
			}
		}
		return nil
	}
}

// mergeOldestPair folds the oldest two reminders of one source together, as long as no command sits between them.
func (s *Session) mergeOldestPair() bool {
	st := &s.st
	for i, a := range st.queue {
		if a.origin.Kind != message.OriginReminder {
			continue
		}
		for _, b := range st.queue[i+1:] {
			if b.cmd != nil {
				break
			}
			if b.origin.Kind == message.OriginReminder && b.origin.Source == a.origin.Source {
				st.queue = slices.DeleteFunc(st.queue, func(q *queueItem) bool { return q == b })
				s.absorb(a, b)
				return true
			}
		}
	}
	return false
}

// absorb folds it into dst; wake is ORed and the strictest mode wins, so merging never delays or silences an input.
func (s *Session) absorb(dst, it *queueItem) {
	dst.wake = dst.wake || it.wake
	if modeRank[it.mode] > modeRank[dst.mode] {
		dst.mode = it.mode
	}
	dst.parts = append(dst.parts, it.parts...)
	dst.omitted += it.omitted
	d := defaults().Input
	for len(dst.parts) > 1 && (len(dst.parts) > d.MergedMaxBlocks || dst.size() > d.MergedMaxBytes) {
		gone := dst.parts[0]
		dst.parts = dst.parts[1:]
		dst.omitted++
		s.dropPart(gone, "merged_overflow")
	}
	s.preempt(dst)
}

// preempt stops the running turn for an InterruptFirst command or a waking interrupt item; it runs after every admission and merge.
func (s *Session) preempt(it *queueItem) {
	st := &s.st
	if st.phase != phaseRunning || st.turn.preempted || it.mode != ModeInterrupt || (it.cmd == nil && !st.canWake(it, s.limits.MaxConsecutiveAutoTurns)) {
		return
	}
	st.turn.preempted = true
	reason := "superseded by input " + it.id
	if it.cmd != nil {
		reason = "superseded by command " + it.cmd.id
	}
	st.preemptCancel = s.ctl.stopper(st.turn.id, reason, &InterruptError{Reason: reason}, true)
}

// flushPreempt cancels the turn preempt stopped, after the input that caused it is announced, because the canceled tools publish their own events at once.
func (s *Session) flushPreempt() {
	if c := s.st.preemptCancel; c != nil {
		s.st.preemptCancel = nil
		c()
	}
}

// drop settles an item that will never be delivered: input.dropped for each of its parts, receipts resolve as dropped, its reservation is returned.
func (s *Session) drop(it *queueItem, reason string) {
	if it.cmd != nil {
		s.dropCommand(it.cmd)
		return
	}
	for _, p := range it.parts {
		s.dropPart(p, reason)
	}
	s.release(it)
}

func (s *Session) dropPart(p *itemPart, reason string) {
	s.publish(&event.InputDropped{InputID: p.id, Reason: reason})
	var err error
	if reason == "session_closing" {
		err = ErrSessionClosed
	}
	p.receipt.resolve(TurnResult{Outcome: OutcomeDropped, Err: err})
}

func (s *Session) release(it *queueItem) {
	if it.reserved {
		it.reserved = false
		s.queued.Add(-1)
	}
}

// giveBack returns items a turn took but never committed; they never reached the model, so requeueing cannot deliver anything twice.
func (s *Session) giveBack(items []*queueItem, ownInputsStopped bool) {
	st := &s.st
	for _, it := range items {
		switch {
		case st.phase == phaseClosing:
			s.drop(it, "session_closing")
		case humanish(it.origin) && (st.turn.dropInputs || ownInputsStopped && !st.turn.preempted):
			s.drop(it, "interrupted") // Interrupt ends the request it stopped; a preempting input keeps it for the next turn
		default:
			st.insert(it) // back at its old seq, so it stays ahead of any barrier it was ahead of
		}
	}
}

// Withdraw takes back an input that no turn has taken yet; task notifications stay, since only their task may withdraw them.
func (s *Session) Withdraw(inputID string) error {
	err := ErrNotQueued
	if e := s.execAfterInputs(func(st *state) {
		it, p := st.queuedInput(inputID)
		if it == nil || it.origin.Kind == message.OriginTask || p.unpin != "" { // an Unpin's reminder must reach history, or a Resume would bring the pin back
			return
		}
		err = nil
		if len(it.parts) == 1 {
			st.queue = slices.DeleteFunc(st.queue, func(q *queueItem) bool { return q == it })
			s.publishSnapshot() // the caller may read QueuedInputs as soon as Withdraw returns
			s.drop(it, "withdrawn")
			return
		}
		it.parts = slices.DeleteFunc(it.parts, func(q *itemPart) bool { return q == p }) // only reminders merge, so no reservation moves with the part
		it.id = it.parts[0].id
		s.dropPart(p, "withdrawn")
	}); e != nil {
		return e
	}
	return err
}

// InterruptWith turns a queued input into a waking interrupt item, so it behaves as if it had been sent with ModeInterrupt.
func (s *Session) InterruptWith(inputID string) error {
	err := ErrNotQueued
	if e := s.execAfterInputs(func(st *state) {
		it, _ := st.queuedInput(inputID)
		if it == nil {
			return
		}
		err = nil
		it.mode, it.wake = ModeInterrupt, true
		s.preempt(it)
		s.flushPreempt()
	}); e != nil {
		return e
	}
	return err
}

// execAfterInputs is exec through the inputs' own lane, so a Withdraw never overtakes an input sent before it.
func (s *Session) execAfterInputs(fn func(*state)) error {
	o := &opMsg{fn: fn, done: make(chan struct{})}
	if err := s.mb.push(o, true); err != nil {
		return err
	}
	select {
	case <-o.done:
		return nil
	case <-s.done:
		return ErrSessionClosed
	}
}

// queuedInput finds the queued item holding a part with this id; a queued command has no parts, so it never matches.
func (st *state) queuedInput(id string) (*queueItem, *itemPart) {
	for _, it := range st.queue {
		if i := slices.IndexFunc(it.parts, func(p *itemPart) bool { return p.id == id }); i >= 0 {
			return it, it.parts[i]
		}
	}
	return nil, nil
}

// dropQueued drops every queued item that keep rejects, in queue order.
func (s *Session) dropQueued(reason string, keep func(*queueItem) bool) {
	for _, it := range s.st.takeIf(func(it *queueItem) bool { return !keep(it) }) {
		s.drop(it, reason)
	}
}

// delivered settles the items a commit just put into history: their reservations are returned and their receipts wait for the turn's end.
func (s *Session) delivered(items []*queueItem) {
	for _, it := range items {
		s.release(it)
	}
}

// deliveryInfo is what a delivery point reports next to its message.
type deliveryInfo struct {
	ids       []string
	sources   []string
	reminders int
	truncated bool
	meta      map[string]string
	tasks     []string // tasks whose notifications the message carries
}

// delivery builds the one user message a delivery point commits: provider reminders, queued reminders, hook context, then task notifications, then messages from other agents, then commands and human input, each group in arrival order.
func delivery(texts prompt.Source, items []*queueItem, pre, post []*message.ReminderBlock) (*message.Message, deliveryInfo) {
	var info deliveryInfo
	var reminders, notes, agents, humans message.Content
	add := func(source, text string, cut bool) {
		reminders = append(reminders, &message.ReminderBlock{Source: source, Text: text})
		info.reminders++
		info.truncated = info.truncated || cut
		if !slices.Contains(info.sources, source) {
			info.sources = append(info.sources, source)
		}
	}
	remind := func(source, text string) {
		text, cut := truncateReminder(texts, text)
		add(source, text, cut)
	}
	for _, r := range pre {
		remind(r.Source, r.Text)
	}
	for _, it := range items {
		info.ids = append(info.ids, it.ids()...)
		for i, p := range it.parts {
			info.meta = mergeMeta(info.meta, p.metadata)
			switch p.origin.Kind {
			case message.OriginReminder:
				text, cut := truncateReminder(texts, p.text)
				if i == 0 && it.omitted > 0 {
					text = prompt.TextfFrom(texts, "merged_omitted", map[string]string{"count": strconv.Itoa(it.omitted)}) + "\n" + text
				}
				add(p.origin.Source, text, cut)
				if p.unpin != "" {
					reminders[len(reminders)-1].(*message.ReminderBlock).Unpin = p.unpin
				}
			case message.OriginTask:
				notes = append(notes, noteBlock(p))
				info.tasks = append(info.tasks, p.note.task)
			case message.OriginAgent:
				body := prompt.FileFrom(texts, "agent_message") + "\n" + prompt.EscapeRuntimeTags(p.text)
				agents = append(agents, &message.TextBlock{Text: "<agent-message from=" + prompt.QuoteAttr(p.origin.Source) + ">\n" + body + "\n</agent-message>"})
				agents = append(agents, escapeContent(p.content)...)
			default:
				if p.text != "" {
					humans = append(humans, &message.TextBlock{Text: prompt.EscapeRuntimeTags(p.text)})
				}
				humans = append(humans, escapeContent(p.content)...)
			}
		}
	}
	for _, r := range post {
		remind(r.Source, r.Text)
	}
	origin := message.Origin{Kind: message.OriginReminder}
	if len(items) > 0 {
		if p := principal(items); humanish(p.origin) || p.origin.Kind == message.OriginTask {
			origin = p.origin
		}
	}
	content := slices.Concat(reminders, notes, agents, humans)
	m := &message.Message{Role: message.RoleUser, Origin: origin, Content: content}
	m.InputIDs = slices.Clone(info.ids) // a rewind or fork names one of these to cut before this line
	return m, info
}

func mergeMeta(into, from map[string]string) map[string]string {
	if len(from) == 0 {
		return into
	}
	if into == nil {
		into = map[string]string{}
	}
	maps.Copy(into, from)
	return into
}

// truncateReminder caps a reminder at the spec's character limit, counted in Unicode code points.
func truncateReminder(texts prompt.Source, s string) (string, bool) {
	limit := defaults().Input.ReminderMaxChars
	n := utf8.RuneCountInString(s)
	if n <= limit {
		return s, false
	}
	cut := 0
	for i := range s {
		if cut == limit {
			return s[:i] + "\n" + prompt.TextfFrom(texts, "reminder_truncated", map[string]string{"count": strconv.Itoa(n - limit)}), true
		}
		cut++
	}
	return s, false
}

// parseCommand recognizes "/name args" when name is a registered command or alias.
func parseCommand(text string, lookup func(string) bool) (string, string, bool) {
	rest, ok := strings.CutPrefix(text, "/")
	if !ok {
		return "", "", false
	}
	end := strings.IndexFunc(rest, unicode.IsSpace)
	if end < 0 {
		end = len(rest)
	}
	name := rest[:end]
	if name == "" || !lookup(name) {
		return "", "", false
	}
	return name, strings.TrimSpace(rest[end:]), true
}
