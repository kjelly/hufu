package team

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// DecisionTerminalSnapshot is a read-only presentation projection of the
// common terminal state. It contains immutable identities and artifact refs,
// never provider prompts or raw model output.
type DecisionTerminalSnapshot struct {
	Configured            bool
	LogicalRunID          string
	ExecutionRunID        string
	BranchID              string
	Generation            uint32
	Prepared              bool
	PrimaryBinding        *PrimaryBindingV1
	PrimaryBindingEventID *string
	ReasonCodes           []string
}

const (
	ReasonDecisionTerminalPreparationMissing = "decision_terminal_preparation_missing"
	ReasonDecisionPrimaryMissing             = "decision_primary_missing"
	ReasonDecisionTerminalStopped            = "decision_terminal_stopped"
	ReasonDecisionTerminalRecoveryRequired   = "decision_terminal_recovery_required"
)

type decisionTerminalRuntime struct {
	configured             bool
	logicalRunID           string
	branchID               string
	generation             uint32
	preparer               DecisionTerminalPreparer
	existingBinding        *PrimaryBindingV1
	existingBindingEventID *string

	executionRunID string
	preparing      bool
	prepared       bool
	done           chan struct{}
	cancel         context.CancelCauseFunc
	hardStop       bool
	stopCandidate  *RunResult
	preparation    TerminalPreparation
	err            error
	committing     bool
	commitDone     chan struct{}
	commitResult   *RunResult
}

// ConfigureDecisionTerminal enables decision-intent terminal preparation. It
// is a configuration operation and must be called before a public invocation.
func (c *Coordinator) ConfigureDecisionTerminal(config DecisionTerminalConfig) error {
	if c == nil {
		return errors.New("coordinator is nil")
	}
	if !decisionLogicalIDPattern.MatchString(strings.TrimSpace(config.LogicalRunID)) {
		return fmt.Errorf("invalid decision logical run id %q", config.LogicalRunID)
	}
	if !validDecisionIdentifier(strings.TrimSpace(config.BranchID)) || config.Generation == 0 {
		return errors.New("invalid decision terminal branch or generation")
	}
	if config.ExistingBinding != nil {
		if err := validatePrimaryBinding(*config.ExistingBinding); err != nil {
			return fmt.Errorf("validate existing primary binding: %w", err)
		}
		if config.ExistingBinding.LogicalRunID != config.LogicalRunID || config.ExistingBinding.BranchID != config.BranchID || config.ExistingBinding.Generation != config.Generation {
			return errors.New("existing primary binding identity does not match terminal config")
		}
		if config.ExistingBindingEventID == nil || !validDecisionIdentifier(*config.ExistingBindingEventID) {
			return errors.New("existing primary binding requires a valid binding event id")
		}
	}
	c.decisionTerminalMu.Lock()
	c.decisionTerminal = decisionTerminalRuntime{
		configured: true, logicalRunID: config.LogicalRunID, branchID: config.BranchID,
		generation: config.Generation, preparer: config.Preparer,
		existingBinding:        clonePrimaryBinding(config.ExistingBinding),
		existingBindingEventID: cloneStringPointer(config.ExistingBindingEventID),
	}
	c.decisionTerminalMu.Unlock()
	return nil
}

// DisableDecisionTerminal restores ordinary execute-intent terminal behavior.
func (c *Coordinator) DisableDecisionTerminal() {
	if c == nil {
		return
	}
	c.decisionTerminalMu.Lock()
	if c.decisionTerminal.cancel != nil {
		c.decisionTerminal.cancel(errors.New(ReasonDecisionTerminalStopped))
	}
	c.decisionTerminal = decisionTerminalRuntime{}
	c.decisionTerminalMu.Unlock()
}

func (c *Coordinator) resetDecisionTerminalInvocation(runID string) {
	c.decisionTerminalMu.Lock()
	defer c.decisionTerminalMu.Unlock()
	runtime := &c.decisionTerminal
	if !runtime.configured {
		return
	}
	runtime.executionRunID = runID
	runtime.preparing = false
	runtime.prepared = false
	runtime.done = nil
	runtime.cancel = nil
	runtime.hardStop = false
	runtime.stopCandidate = nil
	runtime.preparation = TerminalPreparation{}
	runtime.err = nil
	runtime.committing = false
	runtime.commitDone = nil
	runtime.commitResult = nil
}

func (c *Coordinator) decisionTerminalEnabled() bool {
	if c == nil {
		return false
	}
	c.decisionTerminalMu.Lock()
	defer c.decisionTerminalMu.Unlock()
	return c.decisionTerminal.configured
}

func (c *Coordinator) DecisionTerminalSnapshot() DecisionTerminalSnapshot {
	if c == nil {
		return DecisionTerminalSnapshot{}
	}
	c.decisionTerminalMu.Lock()
	defer c.decisionTerminalMu.Unlock()
	runtime := c.decisionTerminal
	binding := runtime.preparation.PrimaryBinding
	bindingEventID := runtime.existingBindingEventID
	if runtime.preparation.Proof != nil && runtime.preparation.Proof.PrimaryBindingEventID != nil {
		bindingEventID = runtime.preparation.Proof.PrimaryBindingEventID
	}
	if binding == nil {
		binding = runtime.existingBinding
	}
	return DecisionTerminalSnapshot{
		Configured: runtime.configured, LogicalRunID: runtime.logicalRunID, ExecutionRunID: runtime.executionRunID,
		BranchID: runtime.branchID, Generation: runtime.generation, Prepared: runtime.prepared,
		PrimaryBinding: clonePrimaryBinding(binding), PrimaryBindingEventID: cloneStringPointer(bindingEventID),
		ReasonCodes: slices.Clone(runtime.preparation.ReasonCodes),
	}
}

// ActiveBranchID returns the branch identity used by durable runtime events.
func (c *Coordinator) ActiveBranchID() string { return c.activeBranchID() }

// ResolvePrimaryDecisionRecord verifies and decodes the immutable record ref.
func (c *Coordinator) ResolvePrimaryDecisionRecord(ctx context.Context, ref DecisionArtifactRef) (DecisionRecord, error) {
	artifact, err := resolveDecisionArtifactRef(ctx, c.decisionArtifactStore(), ref, "primary decision record")
	if err != nil {
		return DecisionRecord{}, err
	}
	record, err := FetchDecisionArtifact[DecisionRecord](ctx, c.decisionArtifactStore(), artifact)
	if err != nil {
		return DecisionRecord{}, err
	}
	if err := record.ValidateSchemaVersion(); err != nil {
		return DecisionRecord{}, err
	}
	return record, nil
}

// ResolvePrimaryDecisionEvidence verifies and decodes the immutable bounded
// evidence packet used by the bound primary decision.
func (c *Coordinator) ResolvePrimaryDecisionEvidence(ctx context.Context, ref DecisionArtifactRef) (PrimaryDecisionEvidenceV1, error) {
	artifact, err := resolveDecisionArtifactRef(ctx, c.decisionArtifactStore(), ref, "primary decision evidence")
	if err != nil {
		return PrimaryDecisionEvidenceV1{}, err
	}
	evidence, err := FetchDecisionArtifact[PrimaryDecisionEvidenceV1](ctx, c.decisionArtifactStore(), artifact)
	if err != nil {
		return PrimaryDecisionEvidenceV1{}, err
	}
	if err := ValidatePrimaryDecisionEvidence(&evidence); err != nil {
		return PrimaryDecisionEvidenceV1{}, err
	}
	return evidence, nil
}

func (c *Coordinator) authorizeTerminalPreparation(candidate *RunResult, proof *TerminalPreparationProof) {
	if c == nil || candidate == nil || proof == nil {
		return
	}
	c.decisionTerminalMu.Lock()
	c.decisionTerminal.preparation.Candidate = candidate
	c.decisionTerminal.preparation.Proof = proof
	c.decisionTerminalMu.Unlock()
}

func (c *Coordinator) terminalPreparationAuthorized(candidate *RunResult) bool {
	if c == nil || candidate == nil {
		return false
	}
	c.decisionTerminalMu.Lock()
	runtime := c.decisionTerminal
	c.decisionTerminalMu.Unlock()
	proof := runtime.preparation.Proof
	if runtime.preparation.Candidate != candidate || proof == nil {
		return false
	}
	if proof.SchemaVersion != 1 || proof.Kind != "terminal_preparation_proof" || proof.Action != TerminalPreparationCommitTerminal {
		return false
	}
	if proof.LogicalRunID != runtime.logicalRunID || proof.ExecutionRunID != runtime.executionRunID || proof.BranchID != runtime.branchID || proof.Generation != runtime.generation {
		return false
	}
	if proof.SupportRevisionDigest != nil && !validDecisionDigest(*proof.SupportRevisionDigest) {
		return false
	}
	if len(proof.ReasonCodes) > 64 {
		return false
	}
	for _, reason := range proof.ReasonCodes {
		if !validDecisionIdentifier(reason) {
			return false
		}
	}
	if terminalCandidateWantsSuccess(candidate) {
		return runtime.preparation.PrimaryBinding != nil && proof.PrimaryBindingEventID != nil && validDecisionIdentifier(*proof.PrimaryBindingEventID)
	}
	return true
}

// RequestRunTermination is the single boundary for normal, exceptional, and
// compatibility terminal requests. It prepares a decision-intent run before
// delegating exactly one immutable candidate to the existing FinalizeRun
// writer.
func (c *Coordinator) RequestRunTermination(ctx context.Context, intent TerminalIntent, candidate *RunResult, acceptance *AcceptanceResult) (TerminalPreparation, error) {
	if c == nil || candidate == nil {
		return TerminalPreparation{Action: TerminalPreparationCommitTerminal, Candidate: candidate}, nil
	}
	policy, ok := TerminalEntryPolicyFor(intent.EntryPoint)
	if !ok {
		return TerminalPreparation{}, fmt.Errorf("unknown terminal entry point %q", intent.EntryPoint)
	}
	if !c.decisionTerminalEnabled() {
		final := c.finalizeRunPrepared(ctx, candidate, acceptance)
		return TerminalPreparation{Action: TerminalPreparationCommitTerminal, Candidate: final}, nil
	}

	prepared, err := c.prepareDecisionTermination(ctx, policy, intent, candidate)
	if prepared.Action != TerminalPreparationCommitTerminal {
		return prepared, err
	}
	if prepared.Proof != nil {
		c.authorizeTerminalPreparation(prepared.Candidate, prepared.Proof)
	}
	prepared.Candidate = c.commitDecisionTerminalPreparation(ctx, prepared.Candidate, acceptance)
	return prepared, err
}

func (c *Coordinator) requestTerminalResult(ctx context.Context, entryPoint TerminalEntryPoint, cause string, canContinue bool, candidate *RunResult, acceptance *AcceptanceResult) *RunResult {
	prepared, err := c.RequestRunTermination(ctx, TerminalIntent{
		EntryPoint: entryPoint, Cause: cause, WantsSuccess: terminalCandidateWantsSuccess(candidate), CanContinueSupporting: canContinue,
	}, candidate, acceptance)
	if err != nil && c != nil {
		c.report(c.newEvent("error").withMessage("terminal preparation failed: " + err.Error()))
	}
	if prepared.Candidate != nil {
		return prepared.Candidate
	}
	return candidate
}

func (c *Coordinator) commitDecisionTerminalPreparation(ctx context.Context, candidate *RunResult, acceptance *AcceptanceResult) *RunResult {
	c.decisionTerminalMu.Lock()
	runtime := &c.decisionTerminal
	if runtime.commitResult != nil {
		result := runtime.commitResult
		c.decisionTerminalMu.Unlock()
		return result
	}
	if runtime.committing {
		done := runtime.commitDone
		c.decisionTerminalMu.Unlock()
		if ctx == nil {
			ctx = context.Background()
		}
		select {
		case <-done:
			c.decisionTerminalMu.Lock()
			result := runtime.commitResult
			c.decisionTerminalMu.Unlock()
			return result
		case <-ctx.Done():
			return candidate
		}
	}
	runtime.committing = true
	runtime.commitDone = make(chan struct{})
	done := runtime.commitDone
	c.decisionTerminalMu.Unlock()

	result := c.finalizeRunPrepared(ctx, candidate, acceptance)
	c.decisionTerminalMu.Lock()
	runtime.commitResult = result
	runtime.committing = false
	close(done)
	c.decisionTerminalMu.Unlock()
	return result
}

// PrepareDecisionForTerminal exposes the preparation half for embedders that
// need to inspect continue_work without committing. Normal callers should use
// RequestRunTermination so the proof cannot be separated from its candidate.
func (c *Coordinator) PrepareDecisionForTerminal(ctx context.Context, intent TerminalIntent, candidate *RunResult) (TerminalPreparation, error) {
	policy, ok := TerminalEntryPolicyFor(intent.EntryPoint)
	if !ok {
		return TerminalPreparation{}, fmt.Errorf("unknown terminal entry point %q", intent.EntryPoint)
	}
	if !c.decisionTerminalEnabled() {
		return TerminalPreparation{Action: TerminalPreparationCommitTerminal, Candidate: candidate}, nil
	}
	return c.prepareDecisionTermination(ctx, policy, intent, candidate)
}

func (c *Coordinator) prepareDecisionTermination(ctx context.Context, policy TerminalEntryPolicy, intent TerminalIntent, candidate *RunResult) (TerminalPreparation, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	wantsSuccess := terminalCandidateWantsSuccess(candidate) || decisionCandidateAwaitingPrimary(candidate)

	c.decisionTerminalMu.Lock()
	runtime := &c.decisionTerminal
	if runtime.executionRunID == "" {
		runtime.executionRunID = strings.TrimSpace(c.executionRunID)
	}
	if runtime.prepared {
		prepared, err := cloneTerminalPreparation(runtime.preparation), runtime.err
		c.decisionTerminalMu.Unlock()
		return prepared, err
	}
	if policy.PrimaryMode == TerminalPrimaryResumeOnly && runtime.existingBinding == nil {
		blocked := blockDecisionTerminalCandidate(candidate, ReasonDecisionPrimaryMissing)
		request := DecisionTerminalPreparationRequest{
			Intent: intent, Candidate: blocked, LogicalRunID: runtime.logicalRunID,
			ExecutionRunID: runtime.executionRunID, BranchID: runtime.branchID, Generation: runtime.generation,
		}
		proof := terminalProofFor(request, TerminalPreparationCommitTerminal, nil, nil, []string{ReasonDecisionPrimaryMissing})
		prepared := TerminalPreparation{
			Action: TerminalPreparationCommitTerminal, Candidate: blocked, Proof: proof,
			ReasonCodes: []string{ReasonDecisionPrimaryMissing},
		}
		runtime.prepared = true
		runtime.preparation = prepared
		c.decisionTerminalMu.Unlock()
		return cloneTerminalPreparation(prepared), nil
	}
	if policy.PrimaryMode == TerminalPrimaryForbidden || !wantsSuccess {
		runtime.hardStop = true
		runtime.stopCandidate = candidate
		if runtime.cancel != nil {
			runtime.cancel(errors.New(ReasonDecisionTerminalStopped))
		}
		if runtime.preparing {
			done := runtime.done
			c.decisionTerminalMu.Unlock()
			select {
			case <-done:
				c.decisionTerminalMu.Lock()
				prepared, err := cloneTerminalPreparation(runtime.preparation), runtime.err
				c.decisionTerminalMu.Unlock()
				return prepared, err
			case <-ctx.Done():
				return TerminalPreparation{Action: TerminalPreparationRecoverOnly, Candidate: candidate, ReasonCodes: []string{ReasonDecisionTerminalRecoveryRequired}}, ctx.Err()
			}
		}
		prepared := c.buildStoppedDecisionPreparationLocked(intent, candidate)
		runtime.prepared = true
		runtime.preparation = prepared
		c.decisionTerminalMu.Unlock()
		return cloneTerminalPreparation(prepared), nil
	}
	if runtime.preparing {
		done := runtime.done
		c.decisionTerminalMu.Unlock()
		select {
		case <-done:
			c.decisionTerminalMu.Lock()
			prepared, err := cloneTerminalPreparation(runtime.preparation), runtime.err
			c.decisionTerminalMu.Unlock()
			return prepared, err
		case <-ctx.Done():
			return TerminalPreparation{Action: TerminalPreparationRecoverOnly, Candidate: candidate, ReasonCodes: []string{ReasonDecisionTerminalRecoveryRequired}}, ctx.Err()
		}
	}
	runtime.preparing = true
	runtime.done = make(chan struct{})
	prepareCtx, cancel := context.WithCancelCause(ctx)
	runtime.cancel = cancel
	request := DecisionTerminalPreparationRequest{
		Intent: intent, Candidate: candidate, LogicalRunID: runtime.logicalRunID,
		ExecutionRunID: runtime.executionRunID, BranchID: runtime.branchID, Generation: runtime.generation,
	}
	preparer := runtime.preparer
	existingBinding := clonePrimaryBinding(runtime.existingBinding)
	existingEventID := cloneStringPointer(runtime.existingBindingEventID)
	c.decisionTerminalMu.Unlock()

	prepared, prepareErr := c.runDecisionTerminalPreparer(prepareCtx, request, preparer, existingBinding, existingEventID)
	c.decisionTerminalMu.Lock()
	runtime.cancel = nil
	if runtime.hardStop {
		prepared = c.buildStoppedDecisionPreparationLocked(intent, runtime.stopCandidate)
		prepareErr = nil
	}
	runtime.preparing = false
	runtime.prepared = prepared.Action != TerminalPreparationContinueWork
	runtime.preparation = cloneTerminalPreparation(prepared)
	runtime.err = prepareErr
	close(runtime.done)
	c.decisionTerminalMu.Unlock()
	return prepared, prepareErr
}

func (c *Coordinator) runDecisionTerminalPreparer(ctx context.Context, request DecisionTerminalPreparationRequest, preparer DecisionTerminalPreparer, existing *PrimaryBindingV1, existingEventID *string) (TerminalPreparation, error) {
	if existing != nil {
		proof := terminalProofFor(request, TerminalPreparationCommitTerminal, &existing.SupportRevisionDigest, existingEventID, nil)
		return TerminalPreparation{Action: TerminalPreparationCommitTerminal, Candidate: request.Candidate, PrimaryBinding: existing, Proof: proof}, nil
	}
	if preparer == nil {
		candidate := blockDecisionTerminalCandidate(request.Candidate, ReasonDecisionPrimaryMissing)
		proof := terminalProofFor(request, TerminalPreparationCommitTerminal, nil, nil, []string{ReasonDecisionPrimaryMissing})
		return TerminalPreparation{Action: TerminalPreparationCommitTerminal, Candidate: candidate, Proof: proof, ReasonCodes: []string{ReasonDecisionPrimaryMissing}}, nil
	}
	result, err := preparer.PrepareDecisionForTerminal(ctx, request)
	if err != nil {
		candidate := blockDecisionTerminalCandidate(request.Candidate, utilsSafeError(err))
		proof := terminalProofFor(request, TerminalPreparationCommitTerminal, nil, nil, []string{ReasonDecisionPrimaryMissing})
		return TerminalPreparation{Action: TerminalPreparationCommitTerminal, Candidate: candidate, Proof: proof, ReasonCodes: []string{ReasonDecisionPrimaryMissing}}, err
	}
	if result.Action == "" {
		result.Action = TerminalPreparationCommitTerminal
	}
	if result.Action == TerminalPreparationContinueWork {
		return TerminalPreparation{Action: result.Action, Candidate: request.Candidate, ReasonCodes: slices.Clone(result.ReasonCodes)}, nil
	}
	if result.Action == TerminalPreparationRecoverOnly {
		return TerminalPreparation{Action: result.Action, Candidate: request.Candidate, ReasonCodes: append(slices.Clone(result.ReasonCodes), ReasonDecisionTerminalRecoveryRequired)}, nil
	}
	if result.Action != TerminalPreparationCommitTerminal || result.PrimaryBinding == nil || result.PrimaryBindingEventID == nil {
		candidate := blockDecisionTerminalCandidate(request.Candidate, ReasonDecisionPrimaryMissing)
		proof := terminalProofFor(request, TerminalPreparationCommitTerminal, nil, nil, []string{ReasonDecisionPrimaryMissing})
		return TerminalPreparation{Action: TerminalPreparationCommitTerminal, Candidate: candidate, Proof: proof, ReasonCodes: []string{ReasonDecisionPrimaryMissing}}, errors.New("decision terminal preparer did not bind a primary decision")
	}
	if err := validatePrimaryBinding(*result.PrimaryBinding); err != nil {
		return TerminalPreparation{}, fmt.Errorf("validate terminal primary binding: %w", err)
	}
	if result.PrimaryBinding.LogicalRunID != request.LogicalRunID || result.PrimaryBinding.BranchID != request.BranchID || result.PrimaryBinding.Generation != request.Generation || !validDecisionIdentifier(*result.PrimaryBindingEventID) {
		return TerminalPreparation{}, errors.New("terminal primary binding identity mismatch")
	}
	supportDigest := result.SupportRevisionDigest
	if supportDigest == nil {
		supportDigest = &result.PrimaryBinding.SupportRevisionDigest
	}
	proof := terminalProofFor(request, result.Action, supportDigest, result.PrimaryBindingEventID, result.ReasonCodes)
	candidate := request.Candidate
	if decisionCandidateAwaitingPrimary(candidate) {
		candidate = promoteDecisionCandidateWithPrimary(candidate)
	}
	return TerminalPreparation{
		Action: result.Action, Candidate: candidate, PrimaryBinding: clonePrimaryBinding(result.PrimaryBinding),
		Proof: proof, ReasonCodes: slices.Clone(result.ReasonCodes),
	}, nil
}

func (c *Coordinator) buildStoppedDecisionPreparationLocked(intent TerminalIntent, candidate *RunResult) TerminalPreparation {
	if candidate == nil {
		candidate = &RunResult{Outcome: RunOutcomeCancelled, ExitCode: 130, Reason: ReasonDecisionTerminalStopped}
	} else if terminalCandidateWantsSuccess(candidate) {
		candidate = blockDecisionTerminalCandidate(candidate, ReasonDecisionTerminalStopped)
	}
	request := DecisionTerminalPreparationRequest{
		Intent: intent, Candidate: candidate, LogicalRunID: c.decisionTerminal.logicalRunID,
		ExecutionRunID: c.decisionTerminal.executionRunID, BranchID: c.decisionTerminal.branchID, Generation: c.decisionTerminal.generation,
	}
	proof := terminalProofFor(request, TerminalPreparationCommitTerminal, nil, nil, []string{ReasonDecisionTerminalStopped})
	return TerminalPreparation{Action: TerminalPreparationCommitTerminal, Candidate: candidate, Proof: proof, ReasonCodes: []string{ReasonDecisionTerminalStopped}}
}

func terminalProofFor(request DecisionTerminalPreparationRequest, action TerminalPreparationAction, supportDigest, bindingEventID *string, reasons []string) *TerminalPreparationProof {
	return &TerminalPreparationProof{
		SchemaVersion: 1, Kind: "terminal_preparation_proof", LogicalRunID: request.LogicalRunID,
		ExecutionRunID: request.ExecutionRunID, BranchID: request.BranchID, Generation: request.Generation,
		SupportRevisionDigest: cloneStringPointer(supportDigest), Action: action,
		PrimaryBindingEventID: cloneStringPointer(bindingEventID), ReasonCodes: slices.Clone(reasons),
	}
}

func terminalCandidateWantsSuccess(candidate *RunResult) bool {
	return candidate != nil && (candidate.GoalSatisfied || candidate.Outcome == RunOutcomeCompleted)
}

func decisionCandidateAwaitingPrimary(candidate *RunResult) bool {
	if candidate == nil || candidate.Outcome != RunOutcomeUnverified || candidate.GoalSatisfied || candidate.ExitCode != 7 {
		return false
	}
	return candidate.Acceptance == nil || candidate.Acceptance.EffectiveState() == AcceptanceNotConfigured
}

func promoteDecisionCandidateWithPrimary(candidate *RunResult) *RunResult {
	if candidate == nil {
		return nil
	}
	clone := *candidate
	clone.Outcome = RunOutcomeCompleted
	clone.GoalSatisfied = true
	clone.ExitCode = 0
	clone.StopReason = StopReasonCompleted
	clone.Reason = ""
	return &clone
}

func blockDecisionTerminalCandidate(candidate *RunResult, detail string) *RunResult {
	if candidate == nil {
		candidate = &RunResult{}
	} else {
		clone := *candidate
		candidate = &clone
	}
	candidate.Outcome = RunOutcomeBlocked
	candidate.GoalSatisfied = false
	candidate.ExitCode = 7
	candidate.StopReason = StopReasonPolicyViolation
	if detail == "" {
		detail = ReasonDecisionPrimaryMissing
	}
	if candidate.Reason == "" {
		candidate.Reason = detail
	} else if !strings.Contains(candidate.Reason, detail) {
		candidate.Reason += "; " + detail
	}
	return candidate
}

func cloneTerminalPreparation(value TerminalPreparation) TerminalPreparation {
	value.PrimaryBinding = clonePrimaryBinding(value.PrimaryBinding)
	if value.Proof != nil {
		proof := *value.Proof
		proof.SupportRevisionDigest = cloneStringPointer(value.Proof.SupportRevisionDigest)
		proof.PrimaryBindingEventID = cloneStringPointer(value.Proof.PrimaryBindingEventID)
		proof.ReasonCodes = slices.Clone(value.Proof.ReasonCodes)
		value.Proof = &proof
	}
	value.ReasonCodes = slices.Clone(value.ReasonCodes)
	return value
}

func clonePrimaryBinding(binding *PrimaryBindingV1) *PrimaryBindingV1 {
	if binding == nil {
		return nil
	}
	copyBinding := *binding
	return &copyBinding
}

func utilsSafeError(err error) string {
	if err == nil {
		return ReasonDecisionPrimaryMissing
	}
	return ReasonDecisionPrimaryMissing + ": " + strings.TrimSpace(err.Error())
}
