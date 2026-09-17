package team

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"slices"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/utils"
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
	if c != nil && result != nil && c.decisionTerminalEnabled() && terminalCandidateWantsSuccess(result) && !c.terminalPreparationAuthorized(result) {
		result = blockDecisionTerminalCandidate(result, ReasonDecisionTerminalPreparationMissing)
	}
	return c.finalizeRunPrepared(ctx, result, acceptance)
}

// finalizeRunPrepared is the existing single terminal writer. Decision-intent
// callers reach it through RequestRunTermination after runtime proof has been
// registered; FinalizeRun retains a fail-closed compatibility seam for direct
// embedders.
func (c *Coordinator) finalizeRunPrepared(ctx context.Context, result *RunResult, acceptance *AcceptanceResult) *RunResult {
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
		commitCtx, cancel := terminalFinalizationContext(ctx)
		defer cancel()
		if _, err := c.commitTerminalLifecycle(commitCtx, candidate); err == nil {
			c.SetLastRunResult(candidate)
			c.reconcileTerminalStatusProjection(candidate)
		}
		return candidate
	}
	if !activeLifecycle {
		candidate = result
	}
	result = candidate
	// Cancellation stops worker execution, but terminal cleanup owns a separate
	// bounded lifetime. Detach even while the caller context is still live:
	// evidence/experience preparation may otherwise consume its final moments
	// and hand an already-expired deadline to the run_finished append.
	finalCtx, cancelFinalization := terminalFinalizationContext(ctx)
	defer cancelFinalization()
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
		// Persistence receives a fresh full window; preparation time must not
		// reduce the durability boundary's budget.
		commitCtx, cancelCommit := terminalFinalizationContext(ctx)
		_, commitErr := c.commitTerminalLifecycle(commitCtx, result)
		cancelCommit()
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

func terminalFinalizationContext(parent context.Context) (context.Context, context.CancelFunc) {
	base := context.Background()
	if parent != nil {
		base = context.WithoutCancel(parent)
	}
	return context.WithTimeout(base, terminalFinalizationTimeout)
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
	result.RunInputs = c.RunInputSnapshot()
	result.InputBoundAssertions = SummarizeInputBoundAssertions(result.Acceptance)
	result.Metrics.TypedRunInputsResolved = 0
	result.Metrics.InputBoundActions = 0
	result.Metrics.InputBoundAssertionsPassed = 0
	result.Metrics.InputBoundAssertionsFailed = 0
	if result.RunInputs != nil {
		result.Metrics.TypedRunInputsResolved = len(result.RunInputs.Inputs)
	}
	if c.taskTracker != nil && c.taskTracker.TodoList() != nil {
		for _, item := range c.taskTracker.TodoList().Items() {
			if item != nil && len(item.BoundInputs) > 0 {
				result.Metrics.InputBoundActions++
			}
		}
	}
	for _, assertion := range result.InputBoundAssertions {
		if assertion.State == "passed" {
			result.Metrics.InputBoundAssertionsPassed++
		} else {
			result.Metrics.InputBoundAssertionsFailed++
		}
	}
	c.annotateRunCompletionSemantics(result)
}

// SummarizeInputBoundAssertions derives the bounded terminal projection of
// task_output_assert evidence. Offline audit uses the same deterministic
// projection after independently replaying every assertion.
func SummarizeInputBoundAssertions(acceptance *AcceptanceResult) []InputBoundAssertionSummary {
	if acceptance == nil {
		return nil
	}
	var summaries []InputBoundAssertionSummary
	for index, verification := range acceptance.VerificationEvidence {
		if verification == nil || verification.Spec == nil || verification.Spec.Type != VerifyTaskOutputAssert {
			continue
		}
		parts := make([]string, 0, len(verification.Spec.Assertions))
		for _, assertion := range verification.Spec.Assertions {
			part := assertion.Pointer + " " + assertion.Op
			if assertion.Input != "" {
				part += " `" + assertion.Input + "`"
			}
			parts = append(parts, part)
		}
		state := "failed"
		if verification.ExitCode == 0 {
			state = "passed"
		}
		summaries = append(summaries, InputBoundAssertionSummary{
			Criterion: fmt.Sprintf("task_output_assert:%d", index+1), SourceTask: verification.Spec.WorksetSourceTask,
			Output: verification.Spec.TaskOutputName, Assertion: strings.Join(parts, "; "), State: state,
			Evidence: slices.Clone(verification.TaskOutputAssertions),
		})
	}
	return summaries
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
	payload.RunInputs = CloneRunInputSnapshot(result.RunInputs)
	payload.InputBoundAssertions = slices.Clone(result.InputBoundAssertions)
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
		if err := c.terminalizeEmergencyInterruptedTasks(); err != nil {
			c.markTerminalRecoveryState("emergency task cancellation persistence failed: " + utils.RedactSecrets(err.Error()))
			if persistErr := c.persistSession("persist emergency task cancellation recovery"); persistErr != nil {
				return errors.Join(err, persistErr)
			}
			return err
		}
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
		entryPoint := TerminalEntryEmergency
		if errors.Is(context.Cause(ctx), context.Canceled) {
			entryPoint = TerminalEntrySignal
		}
		final := c.requestTerminalResult(ctx, entryPoint, "emergency", false, result, result.Acceptance)
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

func (c *Coordinator) terminalizeEmergencyInterruptedTasks() error {
	if c == nil || c.taskTracker == nil || c.taskTracker.TodoList() == nil {
		return nil
	}
	detail := c.FailureDetail(context.Canceled, FailureSourceContextCanceled)
	var transitionErr error
	changed := false
	for _, item := range c.taskTracker.TodoList().Items() {
		if item == nil {
			continue
		}
		switch item.Status {
		case TaskInProgress, TaskPaused, TaskVerifying, TaskProtocolIncomplete:
			if err := c.PersistFailureWithClassAndStatusError(item.Agent, item.Desc, item.ID, detail, RetryNone, FailureCancelled, TaskError); err != nil {
				transitionErr = errors.Join(transitionErr, err)
				continue
			}
			changed = true
		}
	}
	if changed {
		c.reconcileTaskStatusProjection()
		c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
	}
	return transitionErr
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
