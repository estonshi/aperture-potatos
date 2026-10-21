package kernel

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/estonshi/aperture-potatos/internal/bus"
	"github.com/estonshi/aperture-potatos/store"
)

var (
	ErrSessionClosed       = errors.New("session closed")                                                // a method was called after Close
	ErrSessionClosing      = errors.New("session closing")                                               // cancel cause of the turn that Close interrupts
	ErrAgentClosed         = errors.New("agent closed")                                                  // NewSession or Resume after Agent.Close
	ErrQueueFull           = errors.New("input queue full")                                              // MaxQueuedInputs or a task inbox is full; reminders merge instead, so only human, command and agent inputs meet it
	ErrNotQueued           = errors.New("no queued input with this id")                                  // Withdraw and InterruptWith act only on the queue, so an input a turn has taken is out of their reach
	ErrReentrantWait       = errors.New("re-entrant wait on a session that is running the caller")       // a callback waited on the session running it, which would deadlock
	ErrBusy                = errors.New("session busy")                                                  // in TurnResult.Err when a RejectBusy command finds a turn running
	ErrDuplicate           = errors.New("duplicate registration")                                        // a name already registered in the same scope
	ErrInvalidName         = errors.New("invalid name")                                                  // a nickname or registration name outside the allowed form
	ErrInvalidOrigin       = errors.New("Send accepts only human, command, agent and reminder origins")  // the other kinds, such as task notifications, are written only by the kernel
	ErrEmptyInput          = errors.New("input has neither text nor parts")                              // Send refuses it, so an empty input never starts a turn
	ErrInvalidMode         = errors.New("unknown input mode")                                            // an InputMode outside the declared constants, from any option or call that takes one
	ErrUnknownCommand      = errors.New("no such command")                                               // Session.Command named no registered command
	ErrCapability          = errors.New("model capability does not allow this configuration")            // the declared capabilities cannot serve the session, such as tools on a model that takes none
	ErrNoMatcher           = errors.New("hook point does not support matchers")                          // Match given on a hook point that has no key to match
	ErrAsyncNotAllowed     = errors.New("hook point accepts only synchronous hooks")                     // OnAsync on a point whose output the kernel must wait for
	ErrBlocked             = errors.New("blocked by a hook")                                             // a pre_compact hook blocked Compact, or command_invoke blocked a command
	ErrSubscriberStalled   = bus.ErrStalled                                                              // Subscription.Err: a blocking subscriber took no event within its stall timeout
	ErrSubscriberOverflow  = bus.ErrOverflow                                                             // Subscription.Err: the subscriber's queue reached its hard limit
	ErrChannelClosed       = bus.ErrChannelClosed                                                        // Subscription.Err: the owner closed the channel it subscribed with
	ErrIncompatibleStore   = store.ErrIncompatibleStore                                                  // re-exported like ErrSeqConflict, because store/jsonl reports it and cannot import the kernel
	ErrSessionOpen         = errors.New("session is already open")                                       // NewSession or Resume of an id this agent already has open, so one session never has two writers
	ErrSessionNotFound     = errors.New("no stored session with this id")                                // Resume of an id the store does not hold
	ErrSideChat            = errors.New("a side chat is opened by SideChat on its main session")         // Resume would skip its parent and the drop check
	ErrSideChatUnsupported = errors.New("not supported for a side chat")                                 // SideChat on a side chat, or ForkAt given to SideChat: a side chat always carries its main session's current context
	ErrNoLabelStore        = errors.New("the store keeps no labels; it must implement store.LabelStore") // SetLabels and FindSessions need the labels apart from the append-only file
	ErrChildSession        = errors.New("a task's child session is resumed only with ChildOf")           // alone it would run with no owner, ledger or nesting limit
	ErrTurnNotFound        = errors.New("turn is not in the call ledger")                                // older than the 100 turns kept, from before a Resume, or unknown
	ErrNoCostSource        = errors.New("no cost source configured; use WithCostSource")                 // from TurnCost, since the kernel has no prices of its own
	ErrNotMovable          = errors.New("no movable call with this id is running in the foreground")     // BackgroundCall: unknown, already settled, ending, or of a tool not declared Background
	ErrSeqConflict         = store.ErrSeqConflict                                                        // re-exported so errors.Is works without importing store; always fatal to the session
)

// InterruptError is the cancel cause of a turn stopped by Interrupt or by a preempting input.
type InterruptError struct {
	Reason string
}

// Error includes the reason given to Interrupt.
func (e *InterruptError) Error() string { return "turn interrupted: " + e.Reason }

// WorkerInfo names a goroutine that is still running, as Snapshot and LeakError report it.
type WorkerInfo struct {
	Name string
	Age  time.Duration
}

// LeakError is returned by Close when some workers did not exit within CloseTimeout.
type LeakError struct {
	Workers []WorkerInfo
}

// Error names the workers that were still running.
func (e *LeakError) Error() string {
	names := make([]string, len(e.Workers))
	for i, w := range e.Workers {
		names[i] = w.Name
	}
	return fmt.Sprintf("%d workers did not exit: %s", len(e.Workers), strings.Join(names, ", "))
}
