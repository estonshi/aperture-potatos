package kernel

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"unicode"

	"github.com/estonshi/aperture-potatos/event"
	"github.com/estonshi/aperture-potatos/message"
)

// BusyPolicy says what a command does when it arrives while a turn is running.
type BusyPolicy string

const (
	RunNow         BusyPolicy = "run_now"   // runs concurrently with a running turn; must not mutate history
	WaitIdle       BusyPolicy = "wait_idle" // FIFO with queue-mode inputs and a barrier to every later input; holds the history token while running
	InterruptFirst BusyPolicy = "interrupt" // interrupts the running turn, then runs first; holds the history token
	RejectBusy     BusyPolicy = "reject"    // fails fast with ErrBusy while a turn or a history command runs
)

// Command is a slash command that runs host code instead of reaching the model, unless its result carries a Prompt.
type Command struct {
	Name        string
	Aliases     []string
	Description string
	ArgHint     string
	Busy        BusyPolicy // "" is WaitIdle
	Hidden      bool       // excluded from Commands and session.started.commands
	Run         func(ctx context.Context, cc *CommandContext) (CommandResult, error)
}

func (c Command) busy() BusyPolicy {
	if c.Busy == "" {
		return WaitIdle
	}
	return c.Busy
}

// CommandResult is what a command hands back to the person and, through Prompt, to the model.
type CommandResult struct {
	Text   string `json:"text,omitempty"`   // for people, carried by command.completed.text
	Data   any    `json:"data,omitempty"`   // structured result, carried by command.completed.data
	Prompt *Input `json:"prompt,omitempty"` // non-nil turns into a normal input with origin command
	Pin    *Pin   `json:"pin,omitempty"`    // joins the in-memory pin set, not the session file; compaction re-injects it like a tool result's pin, queued at the command's place
}

// CommandInfo is the public, serializable view of a registered command.
type CommandInfo struct {
	Name        string     `json:"name"`
	Aliases     []string   `json:"aliases,omitempty"`
	Description string     `json:"description"`
	ArgHint     string     `json:"arg_hint,omitempty"`
	Busy        BusyPolicy `json:"busy"`
}

func (c Command) info() CommandInfo {
	return CommandInfo{Name: c.Name, Aliases: c.Aliases, Description: c.Description, ArgHint: c.ArgHint, Busy: c.busy()}
}

// CommandContext is the invocation a command's Run receives.
type CommandContext struct {
	s    *Session
	ctx  context.Context
	id   string
	name string
	args string
}

// ID is the command id that this invocation's command.* events carry.
func (cc *CommandContext) ID() string { return cc.id }

// Name is the registered name, even when the command was invoked by an alias.
func (cc *CommandContext) Name() string { return cc.name }

// Args is the text after the command name as one unsplit string, so each command parses it its own way.
func (cc *CommandContext) Args() string { return cc.args }

// Session is the session the command was sent to.
func (cc *CommandContext) Session() *Session { return cc.s }

// Output emits command.output, which waits for backpressure credit like any bulk event.
func (cc *CommandContext) Output(text string) {
	if cc.s.bus.WaitWritable(cc.ctx) == nil {
		cc.s.publish(&event.CommandOutput{CommandID: cc.id, Text: text})
	}
}

// queuedCommand is one invocation from Send or Command until command.completed.
type queuedCommand struct {
	id       string
	seq      uint64
	cmd      Command
	args     string
	receipt  *Receipt
	reserved bool
	token    bool                        // holds the history token, so the phase is maintenance while it runs
	op       func(context.Context) error // a history primitive (Compact, Clear, Rewind) queued from outside: no hook, no command events
}

// Command invokes without text parsing and never blocks; the receipt resolves with TurnResult.Command.
func (s *Session) Command(name, args string) (*Receipt, error) {
	c, ok := s.lookupCommand(name)
	if !ok {
		return nil, fmt.Errorf("%w: %s", ErrUnknownCommand, name)
	}
	return s.invoke(c, args)
}

func (s *Session) invoke(c Command, args string) (*Receipt, error) {
	id := s.agent.cfg.ids.NewID("cmd")
	qc := &queuedCommand{id: id, cmd: c, args: args, receipt: s.newReceipt(id)}
	if c.busy() != RunNow {
		if !s.reserve() {
			qc.receipt.resolve(TurnResult{Outcome: OutcomeCommand, Err: ErrQueueFull})
			return nil, ErrQueueFull
		}
		qc.reserved = true
	}
	if err := s.mb.push(&commandMsg{qc: qc}, true); err != nil {
		s.releaseCommand(qc)
		qc.receipt.resolve(TurnResult{Outcome: OutcomeCommand, Err: err})
		return nil, err
	}
	return qc.receipt, nil
}

func (s *Session) releaseCommand(qc *queuedCommand) {
	if qc.reserved {
		qc.reserved = false
		s.queued.Add(-1)
	}
}

// onCommand gives every command a seq, so the prompt it returns keeps the command's place ahead of later input.
func (s *Session) onCommand(qc *queuedCommand) {
	st := &s.st
	st.seq++
	qc.seq = st.seq
	if st.phase == phaseClosing || st.phase == phaseClosed {
		s.dropCommand(qc)
		return
	}
	switch qc.cmd.busy() {
	case RunNow:
		if st.phase == phaseInitializing {
			st.runNowWait = append(st.runNowWait, qc)
			return
		}
		s.runCommand(qc, false)
		return
	case RejectBusy:
		if st.phase == phaseRunning || st.phase == phaseMaintenance {
			s.rejectBusy(qc)
			return
		}
	case InterruptFirst:
		it := &queueItem{id: qc.id, seq: qc.seq, origin: message.Origin{Kind: message.OriginCommand, Source: qc.cmd.Name}, mode: ModeInterrupt, cmd: qc}
		st.insert(it)
		s.preempt(it)
		s.flushPreempt()
		return
	}
	st.insert(&queueItem{id: qc.id, seq: qc.seq, origin: message.Origin{Kind: message.OriginCommand, Source: qc.cmd.Name}, mode: ModeQueue, cmd: qc})
}

// rejectBusy pairs command.started with command.completed{error:busy}, so every started command still completes.
func (s *Session) rejectBusy(qc *queuedCommand) {
	s.releaseCommand(qc)
	s.publish(&event.CommandStarted{CommandID: qc.id, Name: qc.cmd.Name, Args: qc.args, Busy: true})
	s.publish(&event.CommandCompleted{CommandID: qc.id, Name: qc.cmd.Name, Error: "busy"})
	qc.receipt.resolve(TurnResult{Outcome: OutcomeCommand, Command: &CommandResult{}, Err: ErrBusy})
}

// dropCommand settles a command that never started.
func (s *Session) dropCommand(qc *queuedCommand) {
	s.releaseCommand(qc)
	qc.receipt.resolve(TurnResult{Outcome: OutcomeCommand, Err: ErrSessionClosed})
}

// startCommand runs a command that holds the history token: the phase is maintenance until it returns.
func (s *Session) startCommand(qc *queuedCommand) {
	s.setPhase(phaseMaintenance)
	s.runCommand(qc, true)
}

func (s *Session) runCommand(qc *queuedCommand, token bool) {
	s.releaseCommand(qc)
	qc.token = token
	if qc.op != nil {
		ctx, cancel := context.WithCancel(s.ctx)
		s.st.commands[qc] = cancel
		mctx := withMarker(ctx, marker{chain: s.chain, kind: "command", token: true, seq: qc.seq})
		s.wk.Go("cmd "+qc.id, func() {
			err := callOp(qc.op, mctx)
			_ = s.mb.push(&commandExitMsg{qc: qc, err: err}, false)
		})
		return
	}
	busy := !token && (s.st.phase == phaseRunning || s.st.phase == phaseMaintenance)
	ctx, cancel := context.WithCancel(s.ctx)
	s.st.commands[qc] = cancel
	cc := &CommandContext{s: s, ctx: withMarker(ctx, marker{chain: s.chain, kind: "command", token: token, seq: qc.seq}), id: qc.id, name: qc.cmd.Name, args: qc.args}
	s.wk.Go("cmd "+qc.id, func() {
		f := fire(withHookScope(ctx, &hookScope{}), s, CommandInvoke, CommandInvokeInput{HookEnv: s.env("", 0), Name: qc.cmd.Name, Args: qc.args, Busy: busy}, nil)
		if f.out.Block {
			_ = s.mb.push(&commandExitMsg{qc: qc, res: CommandResult{Text: f.out.Reason}, err: ErrBlocked, blocked: true}, false)
			return
		}
		cc.args = f.in.Args
		s.publish(&event.CommandStarted{CommandID: qc.id, Name: qc.cmd.Name, Args: cc.args, Busy: busy})
		res, err := callCommand(qc.cmd, cc)
		_ = s.mb.push(&commandExitMsg{qc: qc, res: res, err: err}, false)
	})
}

func callOp(op func(context.Context) error, ctx context.Context) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("history operation panicked: %v", r)
		}
	}()
	return op(ctx)
}

// historyOp runs fn holding the history token: at once for a caller that already holds it, otherwise queued like a WaitIdle command, after the running turn.
func (s *Session) historyOp(ctx context.Context, name string, fn func(context.Context) error) error {
	if m, ok := ctx.Value(markerKey{}).(marker); ok && m.token && len(m.chain) > 0 && m.chain[len(m.chain)-1] == s.id {
		return fn(ctx)
	}
	if reentrant(ctx, s.id) {
		return ErrReentrantWait
	}
	id := s.agent.cfg.ids.NewID("op")
	qc := &queuedCommand{id: id, cmd: Command{Name: name, Busy: WaitIdle}, receipt: s.newReceipt(id), op: fn}
	if err := s.mb.push(&commandMsg{qc: qc}, true); err != nil {
		qc.receipt.resolve(TurnResult{Outcome: OutcomeCommand, Err: err})
		return err
	}
	res, err := qc.receipt.Wait(ctx)
	if err != nil {
		return err
	}
	return res.Err
}

func callCommand(c Command, cc *CommandContext) (res CommandResult, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("command %s panicked: %v", c.Name, r)
		}
	}()
	return c.Run(cc.ctx, cc)
}

func (s *Session) onCommandExit(m *commandExitMsg) {
	st := &s.st
	if cancel := st.commands[m.qc]; cancel != nil {
		cancel()
		delete(st.commands, m.qc)
	}
	if m.qc.op != nil {
		if m.qc.token && st.phase == phaseMaintenance {
			s.setPhase(phaseIdle)
		}
		m.qc.receipt.resolve(TurnResult{Outcome: OutcomeCommand, Err: m.err})
		return
	}
	done := &event.CommandCompleted{CommandID: m.qc.id, Name: m.qc.cmd.Name, Text: m.res.Text}
	switch {
	case m.blocked:
		done.Error = "blocked" // a command_invoke hook refused it before command.started
	case m.err != nil:
		done.Error = m.err.Error()
	}
	if m.res.Data != nil {
		done.Data, _ = json.Marshal(m.res.Data)
	}
	s.publish(done)
	if m.res.Prompt != nil && m.err == nil {
		s.enqueue(s.promptItem(m.qc, *m.res.Prompt))
	}
	if m.res.Pin != nil && m.err == nil {
		st.addPin(*m.res.Pin)
	}
	if m.qc.token && st.phase == phaseMaintenance {
		s.setPhase(phaseIdle)
	}
	res := m.res
	m.qc.receipt.resolve(TurnResult{Outcome: OutcomeCommand, Command: &res, Err: m.err})
}

// promptItem queues a command's prompt as one Send from the command, at the command's seq; it takes its slot unconditionally, so a full queue cannot lose it.
func (s *Session) promptItem(qc *queuedCommand, in Input) *queueItem {
	id := s.agent.cfg.ids.NewID("in")
	origin := message.Origin{Kind: message.OriginCommand, Source: qc.cmd.Name}
	s.queued.Add(1)
	return &queueItem{
		id: id, seq: qc.seq, origin: origin, mode: s.inputMode, wake: true, reserved: true,
		parts: []*itemPart{{id: id, origin: origin, text: in.Text, content: in.Parts, metadata: in.Metadata, receipt: s.newReceipt(id)}},
	}
}

// PromptOption configures PromptCommand.
type PromptOption func(*promptConfig)

type promptConfig struct {
	args []string
	busy BusyPolicy
}

// WithArgs names the positional arguments, so the template can say $name for them.
func WithArgs(names ...string) PromptOption {
	return func(c *promptConfig) { c.args = append(c.args, names...) }
}

// PromptBusy sets the busy policy; the default is WaitIdle.
func PromptBusy(p BusyPolicy) PromptOption { return func(c *promptConfig) { c.busy = p } }

// PromptCommand expands the template with the command's arguments into a normal input with origin command, by the placeholder rules of spec/runtime.md §6.
func PromptCommand(name, description, template string, opts ...PromptOption) Command {
	var pc promptConfig
	for _, o := range opts {
		o(&pc)
	}
	return Command{
		Name:        name,
		Description: description,
		Busy:        pc.busy,
		Run: func(_ context.Context, cc *CommandContext) (CommandResult, error) {
			in := Text(expandTemplate(template, cc.Args(), pc.args))
			in.Origin = message.Origin{Kind: message.OriginCommand, Source: name}
			return CommandResult{Prompt: &in}, nil
		},
	}
}

// expandTemplate replaces $ARGUMENTS, $ARGUMENTS[N], $N and the named $name; $$ is a literal $, and a template without placeholders gets the arguments appended.
func expandTemplate(tmpl, args string, names []string) string {
	words := SplitArgs(args)
	word := func(i int) string {
		if i >= 0 && i < len(words) {
			return words[i]
		}
		return ""
	}
	var b strings.Builder
	used := false
	for i := 0; i < len(tmpl); i++ {
		if tmpl[i] != '$' || i+1 == len(tmpl) {
			b.WriteByte(tmpl[i])
			continue
		}
		rest := tmpl[i+1:]
		switch {
		case rest[0] == '$':
			b.WriteByte('$')
			i++
		case strings.HasPrefix(rest, "ARGUMENTS"):
			n := len("ARGUMENTS")
			if idx, width, ok := bracketIndex(rest[n:]); ok {
				b.WriteString(word(idx))
				n += width
			} else {
				b.WriteString(args)
			}
			used = true
			i += n
		case rest[0] >= '0' && rest[0] <= '9':
			j := 0
			for j < len(rest) && rest[j] >= '0' && rest[j] <= '9' {
				j++
			}
			idx, _ := strconv.Atoi(rest[:j])
			b.WriteString(word(idx))
			used = true
			i += j
		default:
			j := 0
			for j < len(rest) && (rest[j] == '_' || rest[j] >= 'a' && rest[j] <= 'z' || rest[j] >= 'A' && rest[j] <= 'Z' || j > 0 && rest[j] >= '0' && rest[j] <= '9') {
				j++
			}
			if k := slices.Index(names, rest[:j]); j > 0 && k >= 0 {
				b.WriteString(word(k))
				used = true
				i += j
				continue
			}
			b.WriteByte('$')
		}
	}
	out := b.String()
	if !used && args != "" {
		out += "\n\nARGUMENTS: " + args
	}
	return out
}

// bracketIndex reads "[N]" at the start of s.
func bracketIndex(s string) (idx, width int, ok bool) {
	end := strings.IndexByte(s, ']')
	if !strings.HasPrefix(s, "[") || end < 2 {
		return 0, 0, false
	}
	n, err := strconv.Atoi(s[1:end])
	if err != nil || n < 0 {
		return 0, 0, false
	}
	return n, end + 1, true
}

// SplitArgs splits command arguments like a shell, as prompt commands do (runtime.md §6): whitespace separates words, single quotes keep everything literally, double quotes honor \" and \\, and a backslash outside quotes escapes the next character.
func SplitArgs(s string) []string {
	var words []string
	var cur strings.Builder
	in, quote, escaped := false, rune(0), false
	for _, r := range s {
		switch {
		case escaped:
			cur.WriteRune(r)
			escaped = false
		case quote == '\'':
			if r == '\'' {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case quote == '"':
			switch r {
			case '"':
				quote = 0
			case '\\':
				escaped = true
			default:
				cur.WriteRune(r)
			}
		case r == '\\':
			escaped, in = true, true
		case r == '\'' || r == '"':
			quote, in = r, true
		case unicode.IsSpace(r):
			if in {
				words = append(words, cur.String())
				cur.Reset()
				in = false
			}
		default:
			cur.WriteRune(r)
			in = true
		}
	}
	if in {
		words = append(words, cur.String())
	}
	return words
}
