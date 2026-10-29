package team

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"strconv"
	"strings"

	"github.com/kjelly/hufu/internal/runtimestore"
	"github.com/kjelly/hufu/internal/utils"
)

type sessionResumeKey struct{}

// WithSessionResume distinguishes checkpoint recovery from a new user request.
// Recovery must never reopen a completed workflow or replay its work.
func WithSessionResume(ctx context.Context) context.Context {
	return context.WithValue(ctx, sessionResumeKey{}, true)
}

// WorkflowCycleStartedPayload marks a new execution cycle in the same
// conversation. Task IDs remain monotonic across cycles so historical receipts,
// artifact grants, and task idempotency keys retain their original owners.
type WorkflowCycleStartedPayload struct {
	SchemaVersion   int             `json:"schema_version,omitzero"`
	Kind            string          `json:"kind,omitempty"`
	DerivedFork     *DerivedForkRef `json:"derived_fork,omitempty"`
	PreviousCycleID string          `json:"previous_cycle_id,omitempty"`
	TaskIDFloor     int             `json:"task_id_floor"`
	Request         string          `json:"request"`
}

func decodeWorkflowCycleStarted(event RunEvent) (WorkflowCycleStartedPayload, error) {
	var payload WorkflowCycleStartedPayload
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return payload, fmt.Errorf("decode workflow cycle: %w", err)
	}
	if event.RunID == "" || payload.TaskIDFloor < 0 || strings.TrimSpace(payload.Request) == "" || payload.PreviousCycleID == event.RunID {
		return payload, fmt.Errorf("invalid workflow cycle identity or task ID floor")
	}
	if payload.Kind == "derived_fork" {
		if err := json.Unmarshal(event.Payload, &payload, json.RejectUnknownMembers(true)); err != nil {
			return payload, fmt.Errorf("%w: %v", ErrDerivedForkIntent, err)
		}
		ref := payload.DerivedFork
		if payload.SchemaVersion != 1 || payload.PreviousCycleID != "" || ref == nil || !ref.AwaitingNewRequest || ref.FirstCycleID != "" ||
			!historicalGrantIdentity(ref.BranchID) || !historicalGrantIdentity(ref.EventID) || !historicalGrantSHA256(ref.EventHash) ||
			!historicalGrantIdentity(ref.SnapshotID) || !historicalGrantCanonicalName(ref.Team) {
			return payload, ErrDerivedForkIntent
		}
	} else if payload.Kind != "" || payload.SchemaVersion != 0 || payload.DerivedFork != nil {
		return payload, ErrDerivedForkIntent
	}
	return payload, nil
}

// validateWorkflowCycleLineage prevents a malformed boundary from discarding
// unfinished work or reusing historical numeric task identities during replay.
func validateWorkflowCycleLineage(events []RunEvent) error {
	var state Phase
	cycleID, floor := "", 0
	var pending *DerivedForkRef
	startedRuns := make(map[string]string)
	for i, event := range events {
		switch EventType(event.Type) {
		case EventRunStarted:
			startedRuns[event.RunID] = effectiveEventBranchID(event)
		case EventDerivedForkCreated:
			payload, err := decodeDerivedForkCreated(event)
			if err != nil {
				return err
			}
			session := NewSession()
			resetDerivedForkProjection(session, event, payload)
			pending = session.DerivedFork
			clear(startedRuns)
			state, cycleID, floor = PhaseInit, "", payload.Intent.TaskIDFloor
		case EventWorkflowStateChanged:
			var payload WorkflowStateChangedPayload
			if json.Unmarshal(event.Payload, &payload) == nil {
				state = payload.State
			}
		case EventWorkflowCycleStarted:
			payload, err := decodeWorkflowCycleStarted(event)
			if err != nil {
				return err
			}
			// Run-scoped inspection can start at a cycle boundary without
			// its predecessor. A complete lineage has a state to validate.
			derived := payload.Kind == "derived_fork"
			if derived {
				if pending == nil || *pending != *payload.DerivedFork || payload.TaskIDFloor != floor || startedRuns[event.RunID] != effectiveEventBranchID(event) {
					return ErrDerivedForkIntent
				}
			} else if pending != nil || (state != "" && (state != PhaseDone || payload.PreviousCycleID != cycleID)) || payload.TaskIDFloor < floor {
				return fmt.Errorf("workflow cycle %q does not follow its completed predecessor", event.RunID)
			}
			if payload.TaskIDFloor < workflowTaskIDHighWater(events[:i]) {
				return fmt.Errorf("workflow cycle %q reuses a historical task identity", event.RunID)
			}
			for _, item := range reduceToTodoList(events[:i]).tasks {
				if item == nil || isInterruptedStatus(item.Status) {
					return fmt.Errorf("workflow cycle %q would discard unfinished work", event.RunID)
				}
				id, err := strconv.Atoi(item.ID)
				if err == nil && (id < 1 || id > payload.TaskIDFloor) {
					return fmt.Errorf("workflow cycle %q has an invalid task ID floor", event.RunID)
				}
			}
			state, cycleID, floor = PhaseInit, event.RunID, payload.TaskIDFloor
			pending = nil
		}
	}
	return nil
}

func workflowTaskIDHighWater(events []RunEvent) int {
	var highest int
	for _, event := range events {
		if id, err := strconv.Atoi(event.TaskID); err == nil {
			highest = max(highest, id)
		}
	}
	return highest
}

// resetWorkflowCycleProjection clears execution evidence, never conversation
// history, immutable execution policy, run input history, or stored results.
func resetWorkflowCycleProjection(sd *SessionData, cycleID string, floor int) {
	sd.WorkflowCycleID, sd.WorkflowTaskIDFloor = cycleID, floor
	sd.HistoricalResultGrants = nil
	if sd.DerivedFork != nil {
		sd.DerivedFork = cloneDerivedForkRef(sd.DerivedFork)
		if sd.DerivedFork.AwaitingNewRequest {
			sd.DerivedFork.FirstCycleID = cycleID
		}
		sd.DerivedFork.AwaitingNewRequest = false
	}
	sd.Tasks, sd.WorksetReceipts, sd.WorksetStates = nil, nil, nil
	sd.WorkflowState, sd.PhaseResults, sd.RetryState = PhaseInit, nil, NewRetryState()
	sd.RunResult, sd.ContinuationCheckpoint = nil, nil
	sd.ActiveRunInputSnapshotID = ""
	sd.CriterionResults, sd.CriterionCheckpoints, sd.LastCriterionProgressAt = nil, nil, ""
	sd.DiagnosticPackets, sd.PlanRevisions, sd.PlanReviews = nil, nil, nil
	sd.DelegationPhase = DelegationPhaseInitialPending
}

func (c *Coordinator) workflowCycleID() string {
	var id string
	c.viewSessionData(func(sd *SessionData) { id = sd.WorkflowCycleID })
	return id
}

// initialContextWithheld protects a fresh session's first delegation without
// discarding the conversation that supplies the meaning of a follow-up request.
func (c *Coordinator) initialContextWithheld() bool {
	return c.initialDelegationPending() && (c.workflowCycleID() == "" || c.initialDerivedCycle())
}

// beginFollowUpWorkflow is called only by coordinator user-request entry
// points, after admission and before any auxiliary/model preparation. Interrupted
// and failed workflows keep their existing recovery semantics.
func (c *Coordinator) beginFollowUpWorkflow(ctx context.Context, prompt string) (bool, error) {
	derived := c.AwaitingDerivedForkRequest()
	if derived {
		if err := c.ValidateDerivedForkRequest(ctx, prompt); err != nil {
			return false, err
		}
	}
	if c.phaseWorkflow != nil && c.phaseWorkflow.Enabled() && c.phaseWorkflow.State() == PhaseFailed {
		return false, fmt.Errorf("workflow is failed; preserve this session and use targeted reconciliation or fork an earlier checkpoint before resuming")
	}
	if c.phaseWorkflow == nil || !c.phaseWorkflow.Enabled() || (!derived && c.phaseWorkflow.State() != PhaseDone) {
		if pendingHistoricalResultRequest(ctx) != nil {
			return false, fmt.Errorf("historical result intake cannot attach to an unfinished workflow")
		}
		return false, nil
	}
	if resume, _ := ctx.Value(sessionResumeKey{}).(bool); resume || c.IsWrapUp() {
		return false, fmt.Errorf("workflow is already complete; use run or chat with a follow-up request to start another workflow in this session")
	}
	if strings.TrimSpace(prompt) == "" {
		return false, fmt.Errorf("a completed workflow requires a new user request")
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if !c.hasDurableEventJournal() || c.taskTracker == nil {
		return false, fmt.Errorf("starting a follow-up workflow requires a durable event journal and task tracker")
	}
	payload := WorkflowCycleStartedPayload{Request: utils.RedactSecrets(prompt)}
	c.viewSessionData(func(sd *SessionData) {
		payload.PreviousCycleID, payload.TaskIDFloor = sd.WorkflowCycleID, sd.WorkflowTaskIDFloor
		if derived {
			payload.SchemaVersion, payload.Kind, payload.DerivedFork = 1, "derived_fork", cloneDerivedForkRef(sd.DerivedFork)
		}
	})
	for _, item := range c.taskTracker.TodoList().Items() {
		if item == nil {
			continue
		}
		if isInterruptedStatus(item.Status) {
			return false, fmt.Errorf("completed workflow still has unfinished task %s; recover it before starting another cycle", item.ID)
		}
		id, err := strconv.Atoi(item.ID)
		if err != nil {
			// Opaque primary/legacy occurrence IDs cannot alias the numeric
			// allocator used for new worker tasks.
			continue
		}
		payload.TaskIDFloor = max(payload.TaskIDFloor, id)
	}
	events, err := c.EventJournal().ReadEvents(ctx)
	if err != nil {
		return false, fmt.Errorf("read workflow cycle history: %w", err)
	}
	tree, err := LoadSessionTree(c.session.Workspace)
	if err != nil {
		return false, err
	}
	lineage, err := ProjectValidatedEventsForBranch(events, tree, tree.ActiveBranch)
	if err != nil {
		return false, err
	}
	canonical := ReduceToSessionData(lineage)
	if derived {
		if canonical.DerivedFork == nil || *canonical.DerivedFork != *payload.DerivedFork || canonical.WorkflowState != PhaseInit {
			return false, ErrDerivedForkIntent
		}
	} else if canonical.WorkflowState != PhaseDone || canonical.WorkflowCycleID != payload.PreviousCycleID {
		return false, fmt.Errorf("completed workflow has no matching durable completion checkpoint")
	}
	payload.TaskIDFloor = max(payload.TaskIDFloor, workflowTaskIDHighWater(lineage))
	data, err := json.Marshal(payload)
	if err != nil {
		return false, err
	}
	// The append is the commit point. If projection persistence fails, restart
	// rebuilds this boundary without rerunning the completed cycle's workers.
	boundary := RunEvent{
		Type: string(EventWorkflowCycleStarted), Actor: "coordinator", RunID: c.executionRunID, Payload: data,
		IdempotencyKey: "workflow-cycle:" + c.executionRunID,
	}
	durable, grant, err := c.appendWorkflowCycle(ctx, boundary, payload, events)
	if err != nil {
		return false, fmt.Errorf("start follow-up workflow: %w", err)
	}
	payload, err = decodeWorkflowCycleStarted(durable)
	if err != nil {
		return false, err
	}
	if err := c.mutateSessionData(func(sd *SessionData) error {
		resetWorkflowCycleProjection(sd, c.executionRunID, payload.TaskIDFloor)
		sd.HistoricalResultGrants = cloneHistoricalGrantCycleRef(grant)
		sd.addEntryAt("user", payload.Request, durable.Timestamp)
		return nil
	}); err != nil {
		return false, err
	}
	if err := c.phaseWorkflow.restore(PhaseInit, nil, "", NewRetryState()); err != nil {
		return false, err
	}
	c.taskTracker.TodoList().restoreWorkflowCycle(nil, payload.TaskIDFloor)
	c.SetTaskCache(newDefaultTaskCache(taskCacheDependenciesFor(c)))
	c.setRestoredTodoIDs(nil)
	c.initialDelegationAttempted.Store(false)
	c.continuationResume = nil
	c.diagnosticPacketsMu.Lock()
	c.diagnosticPackets = nil
	c.diagnosticPacketsMu.Unlock()
	c.planRevisionsMu.Lock()
	c.planRevisions = nil
	c.planRevisionsMu.Unlock()
	c.planReviewsMu.Lock()
	clear(c.planReviews)
	c.planReviewsMu.Unlock()
	c.initialPrompt = prompt
	c.requestContractMu.Lock()
	c.requestContract, c.requestContractRef, c.requestContractInput = nil, "", ""
	c.requestContractArtifact = ArtifactRef{}
	c.requestContractMu.Unlock()
	c.rebuildAntiThrashingState()
	if err := c.persistSession("persist follow-up workflow cycle"); err != nil {
		return false, err
	}
	c.reconcileTaskStatusProjection()
	if err := c.phaseWorkflow.Start(); err != nil {
		return false, err
	}
	c.saveCheckpoint()
	c.report(c.newEvent("todos_updated").withTodos([]*TodoItem{}))
	return true, nil
}

// appendWorkflowCycle keeps every cycle's durable commit on the canonical
// journal, including the atomic head fence required by derived requests.
func (c *Coordinator) appendWorkflowCycle(ctx context.Context, boundary RunEvent, payload WorkflowCycleStartedPayload, events []RunEvent) (RunEvent, *runtimestore.HistoricalGrantCycleRef, error) {
	if pendingHistoricalResultRequest(ctx) != nil {
		return c.appendHistoricalCycle(ctx, boundary, payload)
	}
	if payload.Kind != "derived_fork" {
		durable, err := c.EventJournal().Append(ctx, boundary)
		return durable, nil, err
	}
	journal, ok := c.EventJournal().(AtomicEventJournal)
	if !ok || len(events) == 0 {
		return RunEvent{}, nil, fmt.Errorf("derived workflow requires an atomic event journal")
	}
	batch, err := journal.AppendBatchAtHead(ctx, []RunEvent{boundary}, events[len(events)-1].ID)
	if err != nil {
		return RunEvent{}, nil, err
	}
	if len(batch) != 1 {
		return RunEvent{}, nil, ErrEventBatchConflict
	}
	return batch[0], nil, nil
}
