package team

import (
	"context"
	jsonv2 "encoding/json/v2"
	"strings"
	"time"
	"unicode"

	"github.com/kjelly/hufu/internal/utils"
)

const actionProgressPrefix = "HUFU_ACTION_PROGRESS "

// ActionProgress is an observational adapter frame, never an execution result
// or evidence that an acceptance criterion advanced.
type ActionProgress struct {
	Version int    `json:"version"`
	Kind    string `json:"kind"`
	Stage   string `json:"stage"`
	Message string `json:"message"`
	Status  string `json:"status,omitempty"`
}

type ActionProgressObservation struct {
	ActionProgress
	ActionID  string    `json:"action_id"`
	Attempt   int       `json:"attempt"`
	Sequence  int       `json:"sequence"`
	UpdatedAt time.Time `json:"updated_at"`
}

func normalizedActionProgress(value ActionProgress) (ActionProgress, bool) {
	if value.Version != 1 || (value.Kind != "heartbeat" && value.Kind != "stage") || len(value.Stage) == 0 || len(value.Stage) > 128 || len(value.Message) > 1024 {
		return ActionProgress{}, false
	}
	for _, r := range value.Stage {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') && r != '_' && r != '-' && r != '.' {
			return ActionProgress{}, false
		}
	}
	switch value.Status {
	case "", "started", "running", "completed", "failed":
	default:
		return ActionProgress{}, false
	}
	value.Message = strings.Join(strings.Fields(strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return ' '
		}
		return r
	}, utils.RedactSecrets(value.Message))), " ")
	return value, true
}

type actionProgressReporterKey struct{}

// WithActionProgressReporter scopes observations to the runtime's immutable
// action identity. Providers cannot supply task or attempt IDs in wire frames.
func WithActionProgressReporter(ctx context.Context, report func(ActionProgress)) context.Context {
	return context.WithValue(ctx, actionProgressReporterKey{}, report)
}

func ReportActionProgress(ctx context.Context, value ActionProgress) bool {
	if ctx.Err() != nil {
		return false
	}
	value, valid := normalizedActionProgress(value)
	if !valid {
		return false
	}
	if report, ok := ctx.Value(actionProgressReporterKey{}).(func(ActionProgress)); ok && report != nil {
		report(value)
		return true
	}
	return false
}

type actionProgressWriter struct {
	ctx        context.Context
	diagnostic *boundedBuffer
	pending    []byte
	oversized  bool
}

func newActionProgressWriter(ctx context.Context) *actionProgressWriter {
	return &actionProgressWriter{ctx: ctx, diagnostic: newBoundedBuffer(16 * 1024)}
}

func (w *actionProgressWriter) Write(data []byte) (int, error) {
	_, _ = w.diagnostic.Write(data)
	for _, b := range data {
		if b == '\n' {
			if !w.oversized && strings.HasPrefix(string(w.pending), actionProgressPrefix) {
				var frame ActionProgress
				if jsonv2.Unmarshal(w.pending[len(actionProgressPrefix):], &frame, jsonv2.RejectUnknownMembers(true)) == nil {
					ReportActionProgress(w.ctx, frame)
				}
			}
			w.pending, w.oversized = w.pending[:0], false
		} else if len(w.pending) < 4096 && !w.oversized {
			w.pending = append(w.pending, b)
		} else {
			w.pending, w.oversized = w.pending[:0], true
		}
	}
	return len(data), nil
}

func (c *Coordinator) recordActionProgress(task TaskDef, todoID, actionID string, attempt int, value ActionProgress) {
	var valid bool
	if value, valid = normalizedActionProgress(value); !valid {
		return
	}
	c.taskProjectionMu.Lock()
	defer c.taskProjectionMu.Unlock()
	item := c.todoItemByID(todoID)
	if item == nil || item.Status != TaskInProgress || c.currentTaskAttempt(todoID) != attempt {
		return
	}
	sequence := 1
	if item.ActionProgress != nil && item.ActionProgress.ActionID == actionID {
		sequence = item.ActionProgress.Sequence + 1
	}
	observed := &ActionProgressObservation{ActionProgress: value, ActionID: actionID, Attempt: attempt, Sequence: sequence, UpdatedAt: time.Now().UTC()}
	if err := c.emitEvent("action_progress", "runtime", todoID, map[string]any{"action_progress": observed}); err != nil {
		return
	}
	if err := c.taskTracker.TodoList().SetActionProgress(todoID, observed); err != nil {
		return
	}
	c.journalAppend(journalRecord{Op: "progress", Agent: task.Agent, TaskID: todoID, RunID: coordinatorRuntimeRunID(c), TS: observed.UpdatedAt.Format(time.RFC3339Nano), ActionProgress: observed})
	c.report(c.newEvent("action_progress").withAgent(task.Agent).withTodoID(todoID).withMessage(value.Stage + ": " + value.Message).withData(map[string]any{"action_id": actionID, "action_type": task.Action.Type, "action_progress": observed}))
	c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
}
