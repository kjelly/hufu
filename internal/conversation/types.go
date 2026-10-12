// Package conversation owns the turn lifecycle of an interactive chat
// session: command admission, a bounded prompt queue, per-turn cancellation,
// and the events a front end renders. It never decides task success; turn
// outcomes come verbatim from the RuntimePort.
package conversation

import (
	"context"

	"github.com/kjelly/hufu/internal/team"
)

// CommandKind names one client command.
type CommandKind string

const (
	CommandSubmitPrompt CommandKind = "submit_prompt"
	CommandCancelTurn   CommandKind = "cancel_turn"
	CommandWrapUp       CommandKind = "wrap_up"
	CommandResetContext CommandKind = "reset_context"
	CommandSwitchTeam   CommandKind = "switch_team"
)

// Command is one request from the front end. ID is the in-process
// idempotency key: resubmitting an ID returns the original Ack.
type Command struct {
	ID     string
	Kind   CommandKind
	Text   string // submit_prompt only
	Target string // switch_team only
}

// AckStatus is the admission decision for a command. Admission is in-memory:
// it is not durable across process restarts.
type AckStatus string

const (
	AckAccepted AckStatus = "accepted"
	AckQueued   AckStatus = "queued"
	AckBusy     AckStatus = "busy"
	AckRejected AckStatus = "rejected"
)

// Ack reports how a command was admitted. Every Submit returns exactly one.
type Ack struct {
	CommandID string
	TurnID    string
	Status    AckStatus
	Reason    string
	Position  int // 1-based queue position for AckQueued
}

// TurnResult is the runtime's verdict for one turn, passed through unchanged.
type TurnResult struct {
	TurnID    string
	Answer    string
	Result    *team.RunResult // nil when the runtime produced no RunResult
	Err       error
	Cancelled bool
	RunIDs    []string
}

// RuntimePort is the narrow runtime surface the controller drives. RunTurn
// must honor ctx cancellation and return when the turn has settled.
type RuntimePort interface {
	RunTurn(ctx context.Context, turnID, prompt string) TurnResult
	RequestWrapUp()
	ResetContext(ctx context.Context) error
	SwitchTeam(ctx context.Context, name string) error
}

// Event is emitted by the controller in the order things happened.
type Event interface{ isEvent() }

// TurnStarted marks the moment a turn was handed to the runtime.
type TurnStarted struct {
	TurnID    string
	CommandID string
	Prompt    string
}

// TurnSettled carries the runtime's result for one turn.
type TurnSettled struct{ Result TurnResult }

// QueueDropped lists queued prompts discarded because the session closed.
type QueueDropped struct{ TurnIDs []string }

// ControlDone reports the completion of reset_context or switch_team.
type ControlDone struct {
	CommandID string
	Kind      CommandKind
	Target    string
	Err       error
}

// Closed is the final event; the events channel closes right after it.
type Closed struct{}

func (TurnStarted) isEvent()  {}
func (TurnSettled) isEvent()  {}
func (QueueDropped) isEvent() {}
func (ControlDone) isEvent()  {}
func (Closed) isEvent()       {}

// CloseReport describes how Close ended the session.
type CloseReport struct {
	DroppedTurnIDs []string
	// TimedOut is true when the active turn did not settle within the
	// timeout; the caller must run its emergency finalization.
	TimedOut bool
}

const (
	defaultQueueCapacity  = 16
	maxQueueCapacity      = 64
	defaultMaxPromptBytes = 256 << 10
	defaultDedupeWindow   = 256
	eventBuffer           = 64
)

// Config tunes admission. Zero values select safe defaults.
type Config struct {
	QueueCapacity  int
	MaxPromptBytes int
	DedupeWindow   int
	NewTurnID      func() string
}

func (c Config) normalized() Config {
	if c.QueueCapacity <= 0 {
		c.QueueCapacity = defaultQueueCapacity
	}
	c.QueueCapacity = min(c.QueueCapacity, maxQueueCapacity)
	if c.MaxPromptBytes <= 0 {
		c.MaxPromptBytes = defaultMaxPromptBytes
	}
	if c.DedupeWindow <= 0 {
		c.DedupeWindow = defaultDedupeWindow
	}
	if c.NewTurnID == nil {
		c.NewTurnID = team.NewInvocationID
	}
	return c
}

// MaxPromptBytes is the default submit_prompt size limit, exposed so front
// ends can show the same bound the controller enforces.
const MaxPromptBytes = defaultMaxPromptBytes
