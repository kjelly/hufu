package team

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"time"
)

type terminalLifecycleState uint8

const (
	terminalLifecycleOpen terminalLifecycleState = iota
	terminalLifecycleCandidateElected
	terminalLifecycleCommitting
	terminalLifecycleCommitted
	terminalLifecycleRecoveryRequired
)

var errTerminalPersistenceUnconfirmed = errors.New("terminal persistence unconfirmed; recovery required")

const terminalFinalizationTimeout = 10 * time.Second

// RunFinalizationInput is an immutable snapshot of the facts a terminal run
// decision may use. It deliberately carries references to evidence/context,
// never transcripts or memory content.
type RunFinalizationInput struct {
	RunID      string
	Result     *RunResult
	Acceptance *AcceptanceResult
	Evidence   *EvidenceManifest
	Tasks      []TodoItem
	BranchID   string
}

// FinalizeRun is the common terminal path for coordinator finish, direct
// agents, and other non-tool completion paths. CompletionGate remains the
// only acceptance authority; the experience processor only proposes or
// confirms/rejects candidates based on that decision.
func (c *Coordinator) FinalizeRun(ctx context.Context, result *RunResult, acceptance *AcceptanceResult) *RunResult {
	if c == nil || result == nil {
		return result
	}
	candidate, elected := c.electTerminalCandidate(result)
	if candidate == nil {
		return result
	}
	c.terminalLifecycleMu.Lock()
	activeLifecycle := c.terminalLifecycleRunID != ""
	c.terminalLifecycleMu.Unlock()
	// An active terminal decision is single-owner. A concurrent or later
	// compatibility caller must wait for the elected owner; it must never run a
	// second builder against the elected business pointer. In particular, do not
	// rely on a state snapshot taken before election: the other goroutine may
	// have elected and started preparation in between those two operations.
	if activeLifecycle && !elected {
		if _, err := c.commitTerminalLifecycle(ctx, candidate); err == nil {
			c.SetLastRunResult(candidate)
			c.reconcileTerminalStatusProjection(candidate)
		}
		return candidate
	}
	if !activeLifecycle {
		candidate = result
	}
	result = candidate
	finalCtx := ctx
	if finalCtx == nil || finalCtx.Err() != nil {
		// Cancellation stops worker execution, but terminal cleanup still needs a
		// bounded context to reject run-bound candidates and either append the
		// complete snapshot or mark recovery. The append path never emits a
		// snapshot unless preparation has completed.
		var cancel context.CancelFunc
		finalCtx, cancel = context.WithTimeout(context.Background(), terminalFinalizationTimeout)
		defer cancel()
	}
	c.drainAsyncTasks()
	result.Acceptance = acceptance
	if err := c.recordContextAcceptanceObservations(acceptance); err != nil {
		downgradeRunForFinalizationError(result, err)
	}
	// Some terminal paths have no finish tool (for example cancellation and
	// LLM-free unresolved-task fallback). They must still receive the same
	// immutable evidence boundary before CompletionGate and experience policy.
	// Interactive finish/direct paths may have sealed it already; do not create
	// a second manifest for the same terminal decision.
	c.lastEvidenceManifestMu.RLock()
	manifest := c.lastEvidenceManifest
	c.lastEvidenceManifestMu.RUnlock()
	if manifest == nil && c.session != nil && c.session.Workspace != "" {
		if err := c.finalizeEvidenceManifest(finalCtx, acceptance); err != nil {
			downgradeRunForFinalizationError(result, fmt.Errorf("finalize evidence manifest: %w", err))
		} else {
			c.lastEvidenceManifestMu.RLock()
			manifest = c.lastEvidenceManifest
			c.lastEvidenceManifestMu.RUnlock()
		}
	}
	result.EvidenceManifest = manifest
	input := c.runFinalizationInput(result, acceptance)
	if err := c.ExperienceProcessor().Prepare(finalCtx, input); err != nil {
		downgradeRunForFinalizationError(result, fmt.Errorf("prepare experience: %w", err))
	}
	finalized := c.applyCompletionGate(finalCtx, result, acceptance)
	if finalized != nil && finalized != result {
		// CompletionGate may return a replacement value for compatibility with
		// older callers. Preserve the elected business pointer instead of
		// allowing that clone to become a competing candidate.
		*result = *finalized
	}
	c.prepareTerminalResult(result)
	if activeLifecycle {
		if evalReport, evalErr := c.PersistReliabilityEvaluation(result); evalErr != nil {
			downgradeReliabilityResultForError(result, evalErr)
		} else {
			_ = c.emitEvent("reliability_eval", "coordinator", "", LifecycleEventPayload{
				ReliabilityMetrics:    &evalReport.Metrics,
				ProductionObservation: evalReport.ProductionObservation,
			})
		}
	}
	if telemetry := c.buildRunTelemetry(result); result.Telemetry == nil {
		result.Telemetry = &telemetry
	}
	if activeLifecycle {
		c.finishTerminalPreparation()
		_, commitErr := c.commitTerminalLifecycle(finalCtx, result)
		// Event-first: no active-run result or downstream projection is
		// published until the exact immutable run_finished snapshot is confirmed.
		if commitErr != nil {
			// Keep only an ephemeral diagnostic outcome for callers handling the
			// error. Session/status/workset/report projections remain untouched.
			c.setLastRunResultInMemory(result)
			return result
		}
		c.SetLastRunResult(result)
		c.reconcileTerminalStatusProjection(result)
		return result
	}
	// Compatibility callers have no active terminal event owner. They still
	// receive the complete re-evaluated result, but no stale candidate state is
	// retained between finish calls.
	c.SetLastRunResult(result)
	return result
}

// prepareTerminalResult fills the bounded fields that must be identical in
// the durable terminal event and the externally visible result. It deliberately
// does not persist the result; the terminal event is committed first.
func (c *Coordinator) prepareTerminalResult(result *RunResult) {
	if c == nil || result == nil {
		return
	}
	if strings.TrimSpace(result.RunID) == "" {
		result.RunID = strings.TrimSpace(c.executionRunID)
	}
	if result.EvidenceManifest != nil && strings.TrimSpace(result.RunID) == "" {
		result.RunID = result.EvidenceManifest.RunID
	}
	result.Worksets = c.WorksetGroupStates()
	c.annotateRunCompletionSemantics(result)
}

func cloneRunResult(result *RunResult) (*RunResult, error) {
	if result == nil {
		return nil, nil
	}
	data, err := json.Marshal(result)
	if err != nil {
		return nil, fmt.Errorf("marshal terminal result snapshot: %w", err)
	}
	var clone RunResult
	if err := json.Unmarshal(data, &clone); err != nil {
		return nil, fmt.Errorf("unmarshal terminal result snapshot: %w", err)
	}
	return &clone, nil
}

func terminalLifecyclePayload(c *Coordinator, result *RunResult) LifecycleEventPayload {
	payload := LifecycleEventPayload{}
	if c != nil && c.session != nil {
		payload.Team = c.session.Config.Name
	}
	if result == nil {
		payload.Outcome = RunOutcomeFailed
		return payload
	}
	payload.RunID = result.RunID
	payload.Outcome = result.Outcome
	payload.GoalSatisfied = result.GoalSatisfied
	payload.GoalMode = result.GoalMode
	payload.StopReason = result.StopReason
	payload.ExitCode = result.ExitCode
	payload.Reason = result.Reason
	payload.Response = result.Response
	payload.UnresolvedTasks = append([]TaskReference(nil), result.UnresolvedTasks...)
	payload.CompletedReview = result.CompletedReview
	payload.FindingsPresent = result.FindingsPresent
	payload.FixedAndVerified = result.FixedAndVerified
	payload.AcceptanceAdvisory = result.AcceptanceAdvisory
	if result.Acceptance != nil {
		payload.AcceptanceState = result.Acceptance.EffectiveState()
		payload.AcceptancePassed = result.Acceptance.IsPassed()
		payload.Acceptance = result.Acceptance
	}
	payload.Worksets = append([]WorksetGroupState(nil), result.Worksets...)
	payload.Stats = &result.Stats
	payload.Metrics = &result.Metrics
	payload.Telemetry = result.Telemetry
	if result.EvidenceManifest != nil {
		payload.EvidenceManifest = result.EvidenceManifest
	}
	return payload
}

// electTerminalCandidate is the only operation allowed to choose a business
// result. The pointer selected in Open is retained for the entire lifecycle;
// serialized event data is detached later and is never written to LastRunResult.
func (c *Coordinator) electTerminalCandidate(result *RunResult) (*RunResult, bool) {
	if c == nil || result == nil {
		return result, false
	}
	c.terminalLifecycleMu.Lock()
	defer c.terminalLifecycleMu.Unlock()
	if c.terminalLifecycleRunID == "" {
		return result, true
	}
	if c.terminalLifecycleState == terminalLifecycleOpen {
		c.terminalLifecycleCandidate = result
		if strings.TrimSpace(result.RunID) == "" {
			result.RunID = strings.TrimSpace(c.executionRunID)
		}
		c.terminalLifecycleState = terminalLifecycleCandidateElected
		if c.terminalLifecycleDone == nil {
			c.terminalLifecycleDone = make(chan struct{})
		}
		if c.terminalLifecyclePrepareDone == nil {
			c.terminalLifecyclePrepareDone = make(chan struct{})
		}
		return result, true
	}
	return c.terminalLifecycleCandidate, false
}

func (c *Coordinator) finishTerminalPreparation() {
	if c == nil {
		return
	}
	c.terminalLifecycleMu.Lock()
	if c.terminalLifecyclePrepared {
		c.terminalLifecycleMu.Unlock()
		return
	}
	c.terminalLifecyclePrepared = true
	prepareDone := c.terminalLifecyclePrepareDone
	c.terminalLifecycleMu.Unlock()
	if prepareDone != nil {
		close(prepareDone)
	}
}

func (c *Coordinator) terminalCandidate() *RunResult {
	if c == nil {
		return nil
	}
	c.terminalLifecycleMu.Lock()
	defer c.terminalLifecycleMu.Unlock()
	if c.terminalLifecycleRunID == "" {
		return nil
	}
	return c.terminalLifecycleCandidate
}

// TerminalLifecycleConfirmed gates legacy/shadow/report projections. A
// coordinator with no active invocation is retained as a compatibility seam
// for test and embedding callers that set a result directly.
func (c *Coordinator) TerminalLifecycleConfirmed() bool {
	if c == nil {
		return false
	}
	c.terminalLifecycleMu.Lock()
	defer c.terminalLifecycleMu.Unlock()
	if c.terminalLifecycleRunID == "" {
		return true
	}
	return c.terminalLifecycleState == terminalLifecycleCommitted
}

// commitTerminalLifecycle is the one canonical owner of run_finished. State
// selection and publication are separate: no terminal lifecycle mutex is held
// while cloning, appending, syncing, or updating session projections.
func (c *Coordinator) commitTerminalLifecycle(ctx context.Context, result *RunResult) (*RunResult, error) {
	if c == nil || result == nil {
		return result, nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	candidate, _ := c.electTerminalCandidate(result)
	if candidate == nil {
		return result, nil
	}
	c.terminalLifecycleMu.Lock()
	activeLifecycle := c.terminalLifecycleRunID != ""
	c.terminalLifecycleMu.Unlock()
	if !activeLifecycle {
		return candidate, nil
	}
	if strings.TrimSpace(candidate.RunID) == "" {
		err := errors.New("terminal result has no run id")
		c.markTerminalRecovery(err.Error())
		return candidate, err
	}

	c.terminalLifecycleMu.Lock()
	if c.terminalLifecycleDone == nil {
		c.terminalLifecycleDone = make(chan struct{})
	}
	done := c.terminalLifecycleDone
	prepareDone := c.terminalLifecyclePrepareDone
	switch c.terminalLifecycleState {
	case terminalLifecycleCommitted:
		c.terminalLifecycleMu.Unlock()
		return candidate, nil
	case terminalLifecycleRecoveryRequired:
		err := c.terminalLifecycleErr
		c.terminalLifecycleMu.Unlock()
		if err == nil {
			err = errTerminalPersistenceUnconfirmed
		}
		return candidate, err
	case terminalLifecycleCandidateElected:
		if !c.terminalLifecyclePrepared {
			c.terminalLifecycleMu.Unlock()
			select {
			case <-prepareDone:
				return c.commitTerminalLifecycle(ctx, candidate)
			case <-ctx.Done():
				c.markTerminalRecoveryForCandidate("terminal preparation wait exceeded: "+ctx.Err().Error(), candidate)
				return candidate, fmt.Errorf("%w: %v", errTerminalPersistenceUnconfirmed, ctx.Err())
			}
		}
		c.terminalLifecycleState = terminalLifecycleCommitting
		c.terminalLifecycleMu.Unlock()
		return c.appendTerminalCandidate(ctx, candidate, done)
	case terminalLifecycleCommitting:
		c.terminalLifecycleMu.Unlock()
		select {
		case <-done:
			c.terminalLifecycleMu.Lock()
			err := c.terminalLifecycleErr
			state := c.terminalLifecycleState
			c.terminalLifecycleMu.Unlock()
			if state == terminalLifecycleCommitted {
				return candidate, nil
			}
			if err == nil {
				err = errTerminalPersistenceUnconfirmed
			}
			return candidate, err
		case <-ctx.Done():
			c.markTerminalRecoveryForCandidate("terminal commit wait exceeded: "+ctx.Err().Error(), candidate)
			return candidate, fmt.Errorf("%w: %v", errTerminalPersistenceUnconfirmed, ctx.Err())
		}
	default:
		c.terminalLifecycleMu.Unlock()
		return candidate, errTerminalPersistenceUnconfirmed
	}
}

func (c *Coordinator) appendTerminalCandidate(ctx context.Context, candidate *RunResult, done chan struct{}) (*RunResult, error) {
	branchID := c.activeBranchID()
	idempotencyKey := terminalFinishedIdempotencyKey(candidate.RunID)
	snapshot, err := cloneRunResult(candidate)
	if err == nil {
		var payload []byte
		payload, err = json.Marshal(terminalLifecyclePayload(c, snapshot))
		if err == nil {
			if c.eventStore == nil {
				err = errors.New("canonical event store is unavailable")
			} else {
				_, err = c.eventStore.AppendPersistedBoundedContext(ctx, RunEvent{
					Type: "run_finished", Actor: "coordinator", RunID: candidate.RunID, BranchID: branchID,
					IdempotencyKey: idempotencyKey, Payload: payload,
				})
			}
		}
	}
	c.terminalLifecycleMu.Lock()
	if err != nil {
		c.terminalLifecycleErr = err
		c.terminalLifecycleState = terminalLifecycleRecoveryRequired
		c.terminalLifecycleWaitTimedOut = errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled)
	} else {
		c.terminalLifecycleSnapshot = snapshot
		c.terminalLifecycleState = terminalLifecycleCommitted
		c.terminalLifecycleErr = nil
	}
	close(done)
	c.terminalLifecycleMu.Unlock()
	if err != nil {
		c.markTerminalRecoveryPending("terminal persistence failure: "+err.Error(), &PendingTerminalCommit{
			RunID: candidate.RunID, IdempotencyKey: idempotencyKey, BranchID: branchID,
		})
	} else {
		// The event store is the canonical terminal owner. Only after its append
		// returns successfully may the exact run-bound recovery marker be
		// reconciled and the canonical session projection be persisted.
		c.clearTerminalRecoveryAfterCommit(snapshot, &PendingTerminalCommit{
			RunID: candidate.RunID, IdempotencyKey: idempotencyKey, BranchID: branchID,
		})
	}
	return candidate, err
}

func terminalFinishedIdempotencyKey(runID string) string {
	return "run_finished:" + strings.TrimSpace(runID)
}

func (c *Coordinator) markTerminalRecovery(reason string) {
	if c == nil {
		return
	}
	c.terminalLifecycleMu.Lock()
	runID := c.terminalLifecycleRunID
	state := c.terminalLifecycleState
	c.terminalLifecycleMu.Unlock()
	if runID != "" && state != terminalLifecycleCommitted {
		c.markTerminalRecoveryForPending(reason, &PendingTerminalCommit{
			RunID: runID, IdempotencyKey: terminalFinishedIdempotencyKey(runID), BranchID: c.activeBranchID(),
		})
		return
	}
	c.markTerminalRecoveryState(reason)
	if err := c.persistSession("persist terminal recovery state"); err != nil {
		log.Printf("warning: persist terminal recovery state failed: %v", err)
	}
}

func (c *Coordinator) markTerminalRecoveryForCandidate(reason string, candidate *RunResult) {
	if c == nil || candidate == nil {
		return
	}
	c.terminalLifecycleMu.Lock()
	runID := c.terminalLifecycleRunID
	c.terminalLifecycleMu.Unlock()
	if strings.TrimSpace(candidate.RunID) == "" || runID != candidate.RunID {
		return
	}
	c.markTerminalRecoveryForPending(reason, &PendingTerminalCommit{
		RunID: candidate.RunID, IdempotencyKey: terminalFinishedIdempotencyKey(candidate.RunID), BranchID: c.activeBranchID(),
	})
}

// markTerminalRecoveryForPending is used by an active lifecycle waiter. The
// session write is serialized with the lifecycle state check so a waiter that
// wakes after the owner commits cannot recreate recovery for the same run (or
// for a later run after the lifecycle has been replaced).
func (c *Coordinator) markTerminalRecoveryForPending(reason string, pending *PendingTerminalCommit) {
	if c == nil || pending == nil {
		return
	}
	store := c.SessionStore()
	c.sessionMu.Lock()
	defer c.sessionMu.Unlock()
	c.terminalLifecycleMu.Lock()
	if c.terminalLifecycleRunID != pending.RunID || c.terminalLifecycleState == terminalLifecycleCommitted {
		c.terminalLifecycleMu.Unlock()
		return
	}
	if c.sessionData == nil {
		c.sessionData = NewSession()
	}
	copyPending := *pending
	c.sessionData.RecoveryRequired = true
	c.sessionData.RecoveryReason = reason
	c.sessionData.PendingTerminalCommit = &copyPending
	c.terminalLifecycleMu.Unlock()
	if c.session != nil && c.session.Workspace != "" {
		if err := store.SaveSession(c.session.Workspace, c.sessionData); err != nil {
			log.Printf("warning: persist pending terminal recovery state failed: %v", err)
		}
	}
}

func (c *Coordinator) markTerminalRecoveryPending(reason string, pending *PendingTerminalCommit) {
	if c == nil {
		return
	}
	if err := c.mutateSessionData(func(sd *SessionData) error {
		sd.RecoveryRequired = true
		sd.RecoveryReason = reason
		if pending == nil {
			sd.PendingTerminalCommit = nil
		} else {
			copyPending := *pending
			sd.PendingTerminalCommit = &copyPending
		}
		return nil
	}); err != nil {
		return
	}
	if err := c.persistSession("persist pending terminal recovery state"); err != nil {
		log.Printf("warning: persist pending terminal recovery state failed: %v", err)
	}
}

func (c *Coordinator) markTerminalRecoveryState(reason string) {
	if c == nil {
		return
	}
	if err := c.mutateSessionData(func(sd *SessionData) error {
		sd.RecoveryRequired = true
		sd.RecoveryReason = reason
		return nil
	}); err != nil {
		return
	}
}

// EmergencyFinalizeRun starts the same commit operation as normal finalization
// but bounds only the coordinator wait. If the kernel write/sync is already in
// progress, the caller returns recovery-required without claiming durability.
func (c *Coordinator) EmergencyFinalizeRun(ctx context.Context) error {
	if c == nil {
		return errors.New("coordinator is nil")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	result := c.terminalCandidate()
	if result == nil {
		result = c.LastRunResult()
	}
	if result == nil {
		items := []*TodoItem(nil)
		if c.taskTracker != nil && c.taskTracker.TodoList() != nil {
			items = c.taskTracker.TodoList().Items()
		}
		evaluated := EvaluateRunOutcome(RunEvaluationInput{
			UnresolvedTasks: UnresolvedTaskReferences(items), Cancelled: true,
			Response: "run interrupted before normal finalization", Reason: "emergency finalization",
			Stats: SummarizeRunStats(items), Metrics: c.Metrics(), GoalMode: c.GoalMode(),
		})
		result = &evaluated
	}

	// If normal finalization has not claimed the active terminal yet, the
	// emergency path uses the exact same full builder: acceptance, evidence,
	// candidate disposition, reliability, worksets, and telemetry all precede
	// its one canonical append. It never marks preparation complete by itself.
	c.terminalLifecycleMu.Lock()
	activeLifecycle := c.terminalLifecycleRunID != ""
	state := c.terminalLifecycleState
	c.terminalLifecycleMu.Unlock()
	if !activeLifecycle || state == terminalLifecycleOpen {
		final := c.FinalizeRun(ctx, result, result.Acceptance)
		if !activeLifecycle {
			return nil
		}
		if final != nil && c.TerminalLifecycleConfirmed() {
			return nil
		}
		return errTerminalPersistenceUnconfirmed
	}

	// Normal finalization owns an already-elected candidate. Wait only on the
	// existing commit; do not replace it or run a second, divergent builder.
	candidate := c.terminalCandidate()
	if candidate == nil {
		candidate = result
	}
	_, err := c.commitTerminalLifecycle(ctx, candidate)
	if err != nil {
		if ctx.Err() != nil {
			c.markTerminalRecoveryForCandidate("emergency terminal persistence unconfirmed: "+ctx.Err().Error(), candidate)
		}
		return err
	}
	if c.TerminalLifecycleConfirmed() {
		c.SetLastRunResult(candidate)
		c.reconcileTerminalStatusProjection(candidate)
		return nil
	}
	return errTerminalPersistenceUnconfirmed
}

func (c *Coordinator) runFinalizationInput(result *RunResult, acceptance *AcceptanceResult) RunFinalizationInput {
	input := RunFinalizationInput{Result: result, Acceptance: acceptance}
	if c == nil {
		return input
	}
	input.RunID = c.executionRunID
	input.BranchID = c.activeBranchID()
	c.lastEvidenceManifestMu.RLock()
	input.Evidence = c.lastEvidenceManifest
	c.lastEvidenceManifestMu.RUnlock()
	if c.taskTracker != nil && c.taskTracker.TodoList() != nil {
		for _, item := range c.taskTracker.TodoList().Items() {
			if item != nil {
				input.Tasks = append(input.Tasks, *item)
			}
		}
	}
	return input
}

func downgradeRunForFinalizationError(result *RunResult, err error) {
	if result == nil || err == nil {
		return
	}
	result.Outcome = RunOutcomePartial
	result.GoalSatisfied = false
	result.StopReason = StopReasonEvidenceIncomplete
	result.ExitCode = 7
	result.Reason = err.Error()
}
