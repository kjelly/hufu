package auditverify

import (
	"context"
	"fmt"
	"strings"

	"github.com/kjelly/hufu/internal/team"
)

// runProjection is the intermediate, already-verified state one lineage scan
// produces. VerifyWorkspaceRun and explain.go's witness/explanation builder
// both need it, so it is computed once per invocation (spec.md §46) and
// shared rather than recomputed.
type runProjection struct {
	lineage          []team.RunEvent
	terminalEvent    team.RunEvent
	runResult        *team.RunResult
	tasks            []*team.TodoItem
	requiredCriteria map[string]bool
}

// VerifyWorkspaceRun is the core audit algorithm (spec.md §34). It never
// re-derives a completion policy: it only checks whether the run's own
// persisted terminal decision is backed by durable, tamper-evident evidence,
// and reports whether that evidence in fact justifies the outcome the run
// claims. A returned error means verification could not even be attempted
// (bad usage, e.g. an unknown run id); a returned result with Verdict
// pass/fail/incomplete means verification ran to completion.
func VerifyWorkspaceRun(ctx context.Context, workspace string, runID string, opts VerifyOptions) (*AuditVerificationResult, error) {
	result, _, err := runWorkspaceAudit(ctx, workspace, runID, opts)
	return result, err
}

// runWorkspaceAudit is VerifyWorkspaceRun's implementation. It additionally
// returns the runProjection so callers that need to build a DecisionWitness
// (hufu audit explain) do not have to re-scan the event log.
func runWorkspaceAudit(ctx context.Context, workspace string, runID string, opts VerifyOptions) (*AuditVerificationResult, *runProjection, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	runID = strings.TrimSpace(runID)
	if runID == "" {
		return nil, nil, fmt.Errorf("run id is required")
	}

	result := &AuditVerificationResult{SchemaVersion: AuditSchemaVersion, RunID: runID}

	// Phase A: canonical event integrity.
	lineage, err := canonicalLineage(workspace)
	if err != nil {
		result.Integrity = AuditDimensionResult{Status: AuditDimensionFail, Reason: err.Error()}
		result.addFinding(CodeEventChainBroken, FindingSeverityCritical, err.Error(), "", 0, "")
		result.finalizeVerdict()
		return result, nil, nil
	}

	chain := VerifyEventChain(lineage)
	if !chain.Valid {
		reason := strings.Join(chain.Findings, "; ")
		result.Integrity = AuditDimensionResult{Status: AuditDimensionFail, Reason: reason}
		result.addFinding(CodeEventHashMismatch, FindingSeverityCritical, reason, "", 0, "")
		result.finalizeVerdict()
		return result, nil, nil
	}

	terminals, runExists := runTerminalEvents(lineage, runID)
	if !runExists {
		return nil, nil, fmt.Errorf("run %q was not found in this workspace's event log", runID)
	}
	if terminalConflict(terminals) {
		reason := fmt.Sprintf("run %q has %d conflicting terminal run_finished events", runID, len(terminals))
		result.Integrity = AuditDimensionResult{Status: AuditDimensionFail, Reason: reason}
		result.addFinding(CodeTerminalConflict, FindingSeverityCritical, reason, "", 0, "")
		result.finalizeVerdict()
		return result, nil, nil
	}
	if len(terminals) == 0 {
		result.Integrity = AuditDimensionResult{Status: AuditDimensionPass, Reason: fmt.Sprintf("event chain verified (%d events); run has no terminal event yet", chain.Events)}
		reason := fmt.Sprintf("run %q has no canonical run_finished event yet", runID)
		result.Completion = AuditDimensionResult{Status: AuditDimensionIncomplete, Reason: reason}
		result.Evidence = AuditDimensionResult{Status: AuditDimensionSkipped, Reason: "no terminal event to bind evidence to"}
		result.Acceptance = AuditDimensionResult{Status: AuditDimensionSkipped, Reason: "no terminal event to bind acceptance to"}
		result.Provenance = AuditDimensionResult{Status: AuditDimensionSkipped, Reason: "no terminal event to bind provenance to"}
		result.Recheck = AuditDimensionResult{Status: AuditDimensionSkipped, Reason: "no terminal event"}
		result.addFinding(CodeTerminalMissing, FindingSeverityWarning, reason, "", 0, "")
		result.finalizeVerdict()
		return result, nil, nil
	}
	terminalEvent := terminals[0]
	result.Integrity = AuditDimensionResult{Status: AuditDimensionPass, Reason: fmt.Sprintf("event chain verified (%d events); terminal event %s (hash %s)", chain.Events, terminalEvent.ID, shortHash(terminalEvent.Hash))}

	// Phase B: canonical terminal projection. This replays the same lineage
	// slice through team.ReduceToSessionData -- the exact reducer
	// team.LoadCanonicalRunFinishedSnapshot and the coordinator's own
	// event-first projections use -- rather than a second evaluator. Doing it
	// inline (instead of calling that convenience wrapper) keeps this to one
	// lineage scan per spec.md §46 and also yields the replayed Tasks and
	// CriterionResults that phases D/E need, which the wrapper discards.
	terminalIndex := -1
	for i, e := range lineage {
		if e.ID == terminalEvent.ID {
			terminalIndex = i
			break
		}
	}
	if terminalIndex < 0 {
		return nil, nil, fmt.Errorf("internal error: terminal event %q not found in its own lineage", terminalEvent.ID)
	}
	session := team.ReduceToSessionData(lineage[:terminalIndex+1])
	if session == nil || session.RunResult == nil || session.RunResult.RunID != runID {
		reason := "canonical run_finished event did not reduce to a matching run result"
		result.Completion = AuditDimensionResult{Status: AuditDimensionFail, Reason: reason}
		result.Evidence = AuditDimensionResult{Status: AuditDimensionFail, Reason: reason}
		result.Acceptance = AuditDimensionResult{Status: AuditDimensionFail, Reason: reason}
		result.Provenance = AuditDimensionResult{Status: AuditDimensionFail, Reason: reason}
		result.Recheck = AuditDimensionResult{Status: AuditDimensionSkipped, Reason: reason}
		result.addFinding(CodeCompletionUnjustified, FindingSeverityCritical, reason, "", 0, "")
		result.finalizeVerdict()
		return result, nil, nil
	}
	runResult := session.RunResult
	result.ExpectedOutcome = runResult.Outcome
	requiredCriteria := requiredCriteriaIDs(lineage[:terminalIndex+1], runID)

	// Phase C: evidence manifest.
	evidenceValid, evidenceDim := verifyEvidenceDimension(ctx, workspace, runResult, result)
	result.Evidence = evidenceDim

	// Phase D: attempt provenance.
	result.Provenance = verifyProvenanceDimension(runID, runResult, session.Tasks, result)

	// Phase E: acceptance.
	result.Acceptance = verifyAcceptanceDimension(runResult, requiredCriteria, result)

	// Phase F: completion derivation.
	requiredTasksComplete := allRequiredTasksComplete(session.Tasks)
	completionDim := DeriveCompletionAudit(CompletionAuditInput{
		RunResult:              runResult,
		EvidenceValid:          evidenceValid,
		EvidenceStatus:         evidenceStatus(runResult),
		AcceptanceState:        acceptanceState(runResult),
		RequiredTasksComplete:  requiredTasksComplete,
		CompletionGateAccepted: result.Provenance.Status != AuditDimensionFail,
	})
	result.Completion = completionDim
	result.DerivedOutcome = runResult.Outcome
	if completionDim.Status == AuditDimensionFail {
		result.DerivedOutcome = team.RunOutcomePartial
		result.addFinding(CodeCompletionUnjustified, FindingSeverityCritical, completionDim.Reason, "", 0, "")
	}

	// Phase G: optional recheck. Recheck never participates in the overall
	// verdict (spec.md §35), so it is computed last and does not gate anything
	// above.
	if opts.Recheck {
		result.Recheck = recheckDimension(ctx, runResult, session.Tasks)
	} else {
		result.Recheck = AuditDimensionResult{Status: AuditDimensionSkipped, Reason: "use --recheck to execute deterministic verifiers"}
	}

	result.finalizeVerdict()
	projection := &runProjection{
		lineage: lineage, terminalEvent: terminalEvent, runResult: runResult,
		tasks: session.Tasks, requiredCriteria: requiredCriteria,
	}
	return result, projection, nil
}

// verifyEvidenceDimension re-verifies the run's evidence manifest by calling
// its own Seal/Verify contract against the workspace's artifact store --
// never a second artifact-hashing implementation.
func verifyEvidenceDimension(ctx context.Context, workspace string, runResult *team.RunResult, result *AuditVerificationResult) (bool, AuditDimensionResult) {
	manifest := runResult.EvidenceManifest
	if manifest == nil {
		if runResult.Outcome == team.RunOutcomeCompleted {
			reason := "run is completed but has no evidence manifest"
			result.addFinding(CodeManifestMissing, FindingSeverityCritical, reason, "", 0, "")
			return false, AuditDimensionResult{Status: AuditDimensionFail, Reason: reason}
		}
		return false, AuditDimensionResult{Status: AuditDimensionSkipped, Reason: fmt.Sprintf("no evidence manifest recorded for outcome %q", runResult.Outcome)}
	}
	if manifest.RunID != runResult.RunID {
		reason := "evidence manifest run_id does not match run_finished run_id"
		result.addFinding(CodeManifestHashMismatch, FindingSeverityCritical, reason, "", 0, manifest.ManifestHash)
		return false, AuditDimensionResult{Status: AuditDimensionFail, Reason: reason}
	}
	if strings.TrimSpace(manifest.ManifestHash) == "" {
		reason := "evidence manifest has no digest"
		result.addFinding(CodeManifestHashMismatch, FindingSeverityCritical, reason, "", 0, "")
		return false, AuditDimensionResult{Status: AuditDimensionFail, Reason: reason}
	}
	store, err := team.NewFileArtifactStore(workspace, workspace)
	if err != nil {
		reason := fmt.Sprintf("open artifact store: %v", err)
		return false, AuditDimensionResult{Status: AuditDimensionFail, Reason: reason}
	}
	if err := manifest.Verify(ctx, store); err != nil {
		code := CodeArtifactHashMismatch
		if strings.Contains(err.Error(), "manifest") {
			code = CodeManifestHashMismatch
		} else if strings.Contains(err.Error(), "read artifact") {
			code = CodeArtifactMissing
		}
		result.addFinding(code, FindingSeverityCritical, err.Error(), "", 0, manifest.ManifestHash)
		return false, AuditDimensionResult{Status: AuditDimensionFail, Reason: err.Error()}
	}
	reason := fmt.Sprintf("evidence manifest %s verified (%d artifacts)", shortHash(manifest.ManifestHash), len(manifest.ArtifactRefs))
	return true, AuditDimensionResult{Status: AuditDimensionPass, Reason: reason}
}

// verifyProvenanceDimension re-derives, for every task-level evidence
// binding, the same winning-attempt selection the runtime made
// (team.LatestSuccessfulExecutionReceipt) and rejects a binding the audit
// cannot uniquely justify from persisted receipts (spec.md §26-27).
func verifyProvenanceDimension(runID string, runResult *team.RunResult, tasks []*team.TodoItem, result *AuditVerificationResult) AuditDimensionResult {
	manifest := runResult.EvidenceManifest
	if manifest == nil {
		return AuditDimensionResult{Status: AuditDimensionSkipped, Reason: "no evidence manifest to bind provenance to"}
	}
	tasksByID := make(map[string]*team.TodoItem, len(tasks))
	for _, item := range tasks {
		if item != nil {
			tasksByID[item.ID] = item
		}
	}

	checked := 0
	for _, evidenceResult := range manifest.EvidenceResults {
		taskID := strings.TrimPrefix(evidenceResult.RequirementID, "task:")
		if taskID == evidenceResult.RequirementID {
			continue // not a "task:<id>" requirement
		}
		if !strings.EqualFold(evidenceResult.Status, "passed") || evidenceResult.Binding == nil {
			continue
		}
		binding := evidenceResult.Binding
		item := tasksByID[taskID]
		if item == nil {
			reason := fmt.Sprintf("task %q evidence binding has no corresponding replayed task state", taskID)
			result.addFinding(CodeReceiptMissing, FindingSeverityCritical, reason, taskID, binding.Attempt, "")
			return AuditDimensionResult{Status: AuditDimensionFail, Reason: reason}
		}
		checked++

		if ambiguous, identities := ambiguousSuccessfulReceipts(item, runID); ambiguous {
			reason := fmt.Sprintf("task %q has %d distinct successful producer identities; the evidence binding cannot uniquely justify a winner", taskID, identities)
			result.addFinding(CodeReceiptAmbiguous, FindingSeverityCritical, reason, taskID, binding.Attempt, "")
			return AuditDimensionResult{Status: AuditDimensionFail, Reason: reason}
		}

		winner := team.LatestSuccessfulExecutionReceipt(item, runID)
		if winner == nil {
			reason := fmt.Sprintf("task %q evidence binding has no matching successful execution receipt", taskID)
			result.addFinding(CodeReceiptMissing, FindingSeverityCritical, reason, taskID, binding.Attempt, "")
			return AuditDimensionResult{Status: AuditDimensionFail, Reason: reason}
		}
		if winner.Attempt != binding.Attempt || winner.ModelExecutionID != binding.ModelExecutionID || winner.ProducerID != binding.ProducerID {
			reason := fmt.Sprintf("task %q evidence binding (attempt %d, producer %s) does not match the winning execution receipt (attempt %d, producer %s)",
				taskID, binding.Attempt, binding.ProducerID, winner.Attempt, winner.ProducerID)
			result.addFinding(CodeBindingConflict, FindingSeverityCritical, reason, taskID, binding.Attempt, "")
			return AuditDimensionResult{Status: AuditDimensionFail, Reason: reason}
		}
	}
	if checked == 0 {
		return AuditDimensionResult{Status: AuditDimensionSkipped, Reason: "no task-level evidence bindings to verify"}
	}
	return AuditDimensionResult{Status: AuditDimensionPass, Reason: fmt.Sprintf("%d task evidence binding(s) match their winning execution receipt", checked)}
}

// ambiguousSuccessfulReceipts mirrors cmd/hufu/report.go's
// itemHasAmbiguousCurrentRunReceipts: it counts distinct successful producer
// identities (model_execution_id + transcript_ref) for a task within one run.
// More than one means no completion order or manifest binding can honestly
// claim a single winner (spec.md §27).
func ambiguousSuccessfulReceipts(item *team.TodoItem, runID string) (bool, int) {
	seen := make(map[string]bool)
	add := func(modelExecutionID, transcriptRef string) {
		seen[modelExecutionID+"\x00"+transcriptRef] = true
	}
	for _, receipt := range item.ExecutionReceipts {
		if receipt.RunID != runID || (receipt.ExitCode != nil && *receipt.ExitCode != 0) {
			continue
		}
		if strings.TrimSpace(receipt.TranscriptRef) == "" {
			continue
		}
		add(receipt.ModelExecutionID, receipt.TranscriptRef)
	}
	if r := item.ExecutionReceipt; r != nil && r.RunID == runID &&
		(r.ExitCode == nil || *r.ExitCode == 0) && strings.TrimSpace(r.TranscriptRef) != "" {
		add(r.ModelExecutionID, r.TranscriptRef)
	}
	return len(seen) > 1, len(seen)
}

// verifyAcceptanceDimension checks that the persisted AcceptanceResult is
// internally consistent, and that a completed run's acceptance state is
// actually Passed (spec.md §14, §40).
//
// A non-passed criterion does not by itself contradict an overall Passed
// state: coordinator_tools.go only fails acceptance over criteria whose
// AcceptanceCriterion.Required is true (an optional criterion may legitimately
// still be failed/pending). requiredIDs (from requiredCriteriaIDs) tells us,
// for ids it has an entry for, whether that criterion was required; an id
// with no entry has an unknown Required flag and is never treated as
// "confirmed required" -- this must not be flagged as a contradiction on
// evidence this thin.
func verifyAcceptanceDimension(runResult *team.RunResult, requiredIDs map[string]bool, result *AuditVerificationResult) AuditDimensionResult {
	acceptance := runResult.Acceptance
	state := team.AcceptanceNotConfigured
	if acceptance != nil {
		state = acceptance.EffectiveState()
	}
	if runResult.Outcome == team.RunOutcomeCompleted && state != team.AcceptancePassed {
		reason := fmt.Sprintf("run is completed but acceptance state is %q", state)
		result.addFinding(CodeAcceptanceNotPassed, FindingSeverityCritical, reason, "", 0, "")
		return AuditDimensionResult{Status: AuditDimensionFail, Reason: reason}
	}
	if acceptance == nil || state == team.AcceptanceNotConfigured {
		return AuditDimensionResult{Status: AuditDimensionSkipped, Reason: "acceptance was not configured for this run"}
	}
	if state == team.AcceptancePassed {
		for _, cr := range acceptance.CriterionResults {
			if !requiredIDs[cr.ID] {
				continue
			}
			if cr.State != "" && cr.State != team.CriterionPassed {
				reason := fmt.Sprintf("acceptance is passed but required criterion %q is %q", cr.ID, cr.State)
				result.addFinding(CodeCriterionEvidenceMissing, FindingSeverityCritical, reason, "", 0, cr.ID)
				return AuditDimensionResult{Status: AuditDimensionFail, Reason: reason}
			}
		}
	}
	return AuditDimensionResult{Status: AuditDimensionPass, Reason: fmt.Sprintf("acceptance state %q is internally consistent", state)}
}

// allRequiredTasksComplete mirrors the exact check EvaluateCompletionGate
// makes over its RequiredTasks input: every replayed task must be TaskDone.
func allRequiredTasksComplete(tasks []*team.TodoItem) bool {
	for _, item := range tasks {
		if item != nil && item.Status != team.TaskDone {
			return false
		}
	}
	return true
}

func evidenceStatus(runResult *team.RunResult) string {
	if runResult.EvidenceManifest == nil {
		return ""
	}
	return runResult.EvidenceManifest.Status
}

func acceptanceState(runResult *team.RunResult) team.AcceptanceState {
	if runResult.Acceptance == nil {
		return team.AcceptanceNotConfigured
	}
	return runResult.Acceptance.EffectiveState()
}

// CompletionAuditInput is the immutable, already-verified evidence
// DeriveCompletionAudit checks a persisted "completed" claim against.
type CompletionAuditInput struct {
	RunResult *team.RunResult

	EvidenceValid  bool
	EvidenceStatus string

	AcceptanceState team.AcceptanceState

	RequiredTasksComplete bool

	CompletionGateAccepted bool
}

// DeriveCompletionAudit is a pure function that checks whether a persisted
// terminal RunResult claiming "completed" is justified by the other
// already-verified dimensions (spec.md §15). It is not a new completion
// evaluator: it does not decide what should have happened, only whether the
// invariants CompletionGate itself requires (spec.md §2.3) hold for what was
// actually persisted. A non-completed persisted outcome makes no claim to
// justify and is always Pass here; whether IT was the right outcome is out of
// scope (spec.md §50).
func DeriveCompletionAudit(input CompletionAuditInput) AuditDimensionResult {
	result := input.RunResult
	if result == nil {
		return AuditDimensionResult{Status: AuditDimensionIncomplete, Reason: "no run result to derive completion from"}
	}
	if result.Outcome != team.RunOutcomeCompleted || !result.GoalSatisfied {
		return AuditDimensionResult{Status: AuditDimensionPass, Reason: fmt.Sprintf("persisted outcome %q makes no completed claim to justify", result.Outcome)}
	}

	var unmet []string
	if !input.EvidenceValid {
		unmet = append(unmet, fmt.Sprintf("evidence manifest is not valid (status %q)", input.EvidenceStatus))
	}
	if input.AcceptanceState != team.AcceptancePassed {
		unmet = append(unmet, fmt.Sprintf("acceptance state is %q, not passed", input.AcceptanceState))
	}
	if !input.RequiredTasksComplete {
		unmet = append(unmet, "not all required tasks are done")
	}
	if !input.CompletionGateAccepted {
		unmet = append(unmet, "task evidence provenance does not verify")
	}
	if len(unmet) > 0 {
		return AuditDimensionResult{Status: AuditDimensionFail, Reason: "persisted completed result is not justified: " + strings.Join(unmet, "; ")}
	}
	return AuditDimensionResult{Status: AuditDimensionPass, Reason: "persisted completed result is fully justified by evidence, acceptance, and task completion"}
}

// recheckDimension re-executes every persisted verification result reachable
// from the run that is safe to recheck read-only, and reports whether
// current on-disk state still reproduces the persisted pass/fail outcome.
func recheckDimension(ctx context.Context, runResult *team.RunResult, tasks []*team.TodoItem) AuditDimensionResult {
	targets := collectRecheckTargets(runResult.Acceptance, tasks)
	if len(targets) == 0 {
		return AuditDimensionResult{Status: AuditDimensionIncomplete, Reason: "no persisted verification evidence available to recheck"}
	}
	var attempted, reproduced, unsupported int
	var failures []string
	for _, target := range targets {
		outcome := recheckVerification(ctx, target)
		if !outcome.attempted {
			unsupported++
			continue
		}
		attempted++
		if outcome.reproduced {
			reproduced++
		} else {
			failures = append(failures, outcome.detail)
		}
	}
	switch {
	case len(failures) > 0:
		return AuditDimensionResult{Status: AuditDimensionFail, Reason: strings.Join(failures, "; ")}
	case attempted == 0:
		return AuditDimensionResult{Status: AuditDimensionIncomplete, Reason: fmt.Sprintf("recheck unsupported for all %d persisted verification(s) in this schema version", unsupported)}
	case unsupported > 0:
		return AuditDimensionResult{Status: AuditDimensionIncomplete, Reason: fmt.Sprintf("%d/%d persisted verification(s) reproduced; %d unsupported for recheck", reproduced, attempted, unsupported)}
	default:
		return AuditDimensionResult{Status: AuditDimensionPass, Reason: fmt.Sprintf("%d/%d persisted verification(s) reproduced under recheck", reproduced, attempted)}
	}
}

func shortHash(hash string) string {
	if len(hash) <= 12 {
		return hash
	}
	return hash[:12]
}
