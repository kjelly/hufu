package team

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"

	"github.com/kjelly/hufu/internal/tools"
)

// Attempt-world states derived from the durable lifecycle events.
const (
	attemptWorldStatePrepared   = "prepared"
	attemptWorldStateApplying   = "applying"
	attemptWorldStateApplied    = "applied"
	attemptWorldStateConflicted = "conflicted"
	attemptWorldStateDiscarded  = "discarded"
	attemptWorldStateRemoved    = "removed"
)

// attemptWorldRecord is one world's lifecycle as recorded in the journal.
type attemptWorldRecord struct {
	WorldID           string
	TaskID            string
	RunID             string
	BranchID          string
	Attempt           int
	OccurrenceAttempt int
	BaselineDigest    string
	DeltaDigest       string
	State             string
	// ApplyStarted is the attempt_workspace_apply_started payload, which
	// carries the verified result the task completes with.
	ApplyStarted json.RawMessage
	// order is the index of the world's prepared event, for choosing the
	// latest world of a task.
	order int
}

type attemptWorldEventPayload struct {
	WorldID           string `json:"world_id"`
	OccurrenceAttempt int    `json:"occurrence_attempt"`
	Attempt           int    `json:"attempt"`
	BaselineDigest    string `json:"baseline_digest"`
	DeltaDigest       string `json:"delta_digest"`
}

// attemptWorldRecordsFromEvents folds attempt-world events into per-world
// records. Callers pass the session's global events: a branch-filtered view
// would hide a world another branch left mid-apply.
func attemptWorldRecordsFromEvents(events []RunEvent) map[string]*attemptWorldRecord {
	records := make(map[string]*attemptWorldRecord)
	for i, event := range events {
		var state string
		switch EventType(event.Type) {
		case EventAttemptWorkspacePrepared:
			state = attemptWorldStatePrepared
		case EventAttemptWorkspaceApplyStarted:
			state = attemptWorldStateApplying
		case EventAttemptWorkspaceApplied:
			state = attemptWorldStateApplied
		case EventAttemptWorkspaceApplyConflicted:
			state = attemptWorldStateConflicted
		case EventAttemptWorkspaceDiscarded:
			state = attemptWorldStateDiscarded
		case EventAttemptWorkspaceOrphanRemoved:
			state = attemptWorldStateRemoved
		default:
			continue
		}
		var payload attemptWorldEventPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil || payload.WorldID == "" {
			continue
		}
		record := records[payload.WorldID]
		if record == nil {
			record = &attemptWorldRecord{WorldID: payload.WorldID, order: i}
			records[payload.WorldID] = record
		}
		if state == attemptWorldStatePrepared {
			record.TaskID, record.RunID, record.BranchID = event.TaskID, event.RunID, event.BranchID
			record.Attempt, record.OccurrenceAttempt = payload.Attempt, payload.OccurrenceAttempt
			record.BaselineDigest, record.order = payload.BaselineDigest, i
		}
		if state == attemptWorldStateApplying {
			record.DeltaDigest = payload.DeltaDigest
			record.ApplyStarted = append(json.RawMessage(nil), event.Payload...)
		}
		record.State = state
	}
	return records
}

// errIsolatedApplyPending means a task's latest world has an apply in
// flight, which only attempt-world recovery may finish.
var errIsolatedApplyPending = errors.New(workspaceApplyIncompleteCode + ": the isolated attempt's apply did not finish; it must be recovered before the task can continue")

// reopenIsolatedRepairWorld finds the world a protocol-incomplete isolated
// task's result-only repair must apply: the latest world of the task's
// current occurrence, still in the prepared state and present on disk. It
// returns nil when there is no such world, in which case the attempt's
// changes are gone and the task must be re-dispatched.
func (c *Coordinator) reopenIsolatedRepairWorld(ctx context.Context, item *TodoItem) (*isolatedAttempt, error) {
	if !c.hasDurableEventJournal() {
		return nil, nil
	}
	events, err := c.EventJournal().ReadEvents(ctx)
	if err != nil {
		return nil, fmt.Errorf("read attempt world events: %w", err)
	}
	var latest *attemptWorldRecord
	for _, record := range attemptWorldRecordsFromEvents(events) {
		if record.TaskID == item.ID && (latest == nil || record.order > latest.order) {
			latest = record
		}
	}
	if latest == nil || latest.OccurrenceAttempt != item.Retries+1 {
		return nil, nil
	}
	switch latest.State {
	case attemptWorldStatePrepared:
	case attemptWorldStateApplying, attemptWorldStateApplied:
		return nil, errIsolatedApplyPending
	default:
		return nil, nil
	}
	world := NewIsolatedCopyExecutionWorld()
	prepared, owner, err := world.Load(c.attemptWorldDirFor(latest.WorldID), latest.BaselineDigest)
	if err != nil {
		log.Printf("warning: attempt world %s of task %s cannot be reopened: %v", latest.WorldID, item.ID, err)
		return nil, nil
	}
	if owner.TaskID != item.ID {
		return nil, nil
	}
	return &isolatedAttempt{world: world, prepared: prepared, taskID: item.ID, attempt: latest.Attempt}, nil
}

// withoutIsolatedAttempt clears an execution-root binding from ctx, so a
// re-dispatched attempt cannot inherit a world it does not own.
func withoutIsolatedAttempt(ctx context.Context) context.Context {
	ctx = context.WithValue(ctx, tools.AgentExecutionRootKey, tools.AgentExecutionRoot{})
	return context.WithValue(ctx, isolatedAttemptKey{}, (*isolatedAttempt)(nil))
}

// redispatchIsolatedTask resets a protocol-incomplete isolated task and runs
// it again from the current canonical project. Its earlier attempt never
// touched the project, so the new attempt repeats no applied effect.
func (c *Coordinator) redispatchIsolatedTask(ctx context.Context, task TaskDef, item *TodoItem, reason string) (string, error) {
	_ = c.emitEvent(string(EventRecoveryDecision), "coordinator", item.ID, map[string]interface{}{
		"decision":      "isolated_attempt_redispatch",
		"reason":        reason,
		"worker_replay": true,
	})
	c.report(c.newEvent("step").withAgent(item.Agent).withMessage("re-dispatching isolated task: " + reason).withTodoID(item.ID))
	if err := c.prepareInterruptedTaskForResume(ctx, item.ID, "re-dispatched: "+reason); err != nil {
		return "", err
	}
	return c.executeTask(withoutIsolatedAttempt(ctx), task, item.ID)
}

// resumeIsolatedProtocolRepair decides how a protocol-incomplete isolated
// task resumes. With its world still prepared, the result-only repair runs
// and applies that world; without it, the task is re-dispatched.
func (c *Coordinator) resumeIsolatedProtocolRepair(ctx context.Context, task TaskDef, item *TodoItem) (context.Context, *isolatedAttempt, string, bool, error) {
	attempt, err := c.reopenIsolatedRepairWorld(ctx, item)
	if err != nil {
		detail := c.FailureDetail(err, FailureSourceError)
		c.PersistFailureWithClassAndStatusAndOutput(item.Agent, task.Goal, item.ID, detail, NeedsHuman, FailureExecution, TaskBlocked, item.Output)
		return ctx, nil, "", true, err
	}
	if attempt == nil {
		output, err := c.redispatchIsolatedTask(ctx, task, item, "the isolated attempt world is gone, so its changes can no longer be applied")
		return ctx, nil, output, true, err
	}
	return c.withIsolatedAttempt(ctx, attempt), attempt, "", false, nil
}

// applyIsolatedProtocolRepair applies the repaired attempt's world before
// TaskDone. A conflict re-dispatches the task from the current project; any
// other failure blocks it with the world kept.
func (c *Coordinator) applyIsolatedProtocolRepair(ctx context.Context, item *TodoItem, task TaskDef, agentName string, result *TaskResult, output string) (bool, string, error) {
	a := isolatedAttemptFromContext(ctx)
	if a == nil {
		return false, "", nil
	}
	var receipt *ExecutionReceipt
	if current := c.todoItemByID(item.ID); current != nil {
		receipt = current.ExecutionReceipt
	}
	err := c.integrateIsolatedAttempt(ctx, a, isolatedApplyEvidence{
		TypedResult: result, VerifyResult: verifyResultForTodo(c, item.ID), Receipt: receipt, CoordinatorOutput: output,
	})
	if err == nil {
		return false, "", nil
	}
	var conflict *AttemptWorkspaceConflictError
	if errors.As(err, &conflict) && !isAttemptWorkspaceApplyIncomplete(err) {
		redispatched, redispatchErr := c.redispatchIsolatedTask(ctx, task, item, conflict.Error())
		return true, redispatched, redispatchErr
	}
	class := FailureExecution
	if !isAttemptWorkspaceApplyIncomplete(err) {
		class = ClassifyTaskFailureStructured(FailureClassificationInput{Err: err})
	}
	detail := c.FailureDetail(fmt.Errorf("apply isolated attempt after protocol repair: %w", err), FailureSourceError)
	c.PersistFailureWithClassAndStatusAndOutput(agentName, task.Goal, item.ID, detail, NeedsHuman, class, TaskBlocked, output)
	return true, "", err
}
