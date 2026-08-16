package team

import (
	"context"
	"fmt"
	"strings"

	contextstore "github.com/kjelly/hufu/internal/context"
)

// CompletionGateInput is the immutable evidence observed at the end of a run.
// A worker response is deliberately not part of this contract: prose cannot
// make a run accepted.
type CompletionGateInput struct {
	Result          *RunResult
	Acceptance      *AcceptanceResult
	Evidence        *EvidenceManifest
	RequiredTasks   []TaskReference
	UnresolvedRisks []string
	TerminalLeaks   []string
	ArtifactStore   ArtifactStore
}

type CompletionGateDecision struct {
	Accepted bool
	Reasons  []string
}

// CompletionGate is the sole policy that can certify an accepted run.
type CompletionGate struct{}

func (CompletionGate) Evaluate(ctx context.Context, input CompletionGateInput) CompletionGateDecision {
	decision := CompletionGateDecision{Accepted: true}
	reject := func(reason string) {
		decision.Accepted = false
		decision.Reasons = append(decision.Reasons, reason)
	}

	if input.Result == nil || input.Result.Outcome != RunOutcomeCompleted || !input.Result.GoalSatisfied {
		reject("run result is not a satisfied completed result")
	}
	acceptance := input.Acceptance
	if acceptance == nil && input.Result != nil {
		acceptance = input.Result.Acceptance
	}
	if acceptance == nil || !acceptance.IsPassed() {
		reject("acceptance gate did not pass")
	}

	if input.Evidence == nil {
		reject("required evidence manifest is missing")
	} else {
		if input.Evidence.Status != "accepted" {
			reject(fmt.Sprintf("evidence manifest status is %q", input.Evidence.Status))
		}
		if strings.TrimSpace(input.Evidence.ManifestHash) == "" {
			reject("evidence manifest has no digest")
		}
		if input.ArtifactStore != nil {
			if err := input.Evidence.Verify(ctx, input.ArtifactStore); err != nil {
				reject("evidence manifest verification failed: " + err.Error())
			}
		}
		for _, evidence := range input.Evidence.EvidenceResults {
			if evidence.Status != "passed" {
				reject(fmt.Sprintf("evidence requirement %q is %q", evidence.RequirementID, evidence.Status))
			}
		}
	}

	for _, task := range input.RequiredTasks {
		if task.Status != string(TaskDone) {
			reject(fmt.Sprintf("required task %q is not done (status %s)", task.ID, task.Status))
		}
	}
	for _, risk := range input.UnresolvedRisks {
		if strings.TrimSpace(risk) != "" {
			reject("unresolved risk: " + strings.TrimSpace(risk))
		}
	}
	for _, leak := range input.TerminalLeaks {
		if strings.TrimSpace(leak) != "" {
			reject("terminal leak: " + strings.TrimSpace(leak))
		}
	}
	return decision
}

func EvaluateCompletionGate(ctx context.Context, input CompletionGateInput) CompletionGateDecision {
	return (CompletionGate{}).Evaluate(ctx, input)
}

func (c *Coordinator) applyCompletionGate(ctx context.Context, result *RunResult, acceptance *AcceptanceResult) *RunResult {
	// A coordinator without a workspace is only used by lightweight unit
	// callers; production sessions always have a workspace and therefore pass
	// through the durable manifest gate.
	if c == nil || c.session == nil || strings.TrimSpace(c.session.Workspace) == "" {
		return result
	}
	c.lastEvidenceManifestMu.RLock()
	manifest := c.lastEvidenceManifest
	c.lastEvidenceManifestMu.RUnlock()
	var items []*TodoItem
	if c.taskTracker != nil && c.taskTracker.TodoList() != nil {
		items = c.taskTracker.TodoList().Items()
	}
	required := make([]TaskReference, 0, len(items))
	for _, item := range items {
		if item != nil {
			required = append(required, TaskReference{ID: item.ID, Desc: item.Desc, Agent: item.Agent, Status: string(item.Status)})
		}
	}
	riskFindings, terminalLeaks := c.completionGateState()
	var store ArtifactStore
	if c.session != nil {
		store, _ = NewFileArtifactStore(c.session.Workspace, c.session.Workspace)
	}
	decision := EvaluateCompletionGate(ctx, CompletionGateInput{
		Result: result, Acceptance: acceptance, Evidence: manifest,
		RequiredTasks: required, UnresolvedRisks: riskFindings,
		TerminalLeaks: terminalLeaks, ArtifactStore: store,
	})
	input := c.runFinalizationInput(result, acceptance)
	input.Evidence = manifest
	if err := c.ExperienceProcessor().Finalize(ctx, input, decision); err != nil {
		downgradeRunForFinalizationError(result, err)
		return result
	}
	// CompletionGate is only allowed to downgrade a claimed accepted run.
	// Explicit failed, cancelled, partial, and unverified outcomes remain
	// distinct for reports/recovery, while the processor above has still
	// rejected their pending learning candidates.
	if result == nil || result.Outcome != RunOutcomeCompleted || !result.GoalSatisfied {
		return result
	}
	if decision.Accepted {
		return result
	}
	result.Outcome = RunOutcomePartial
	result.GoalSatisfied = false
	result.StopReason = StopReasonEvidenceIncomplete
	result.ExitCode = 7
	result.Reason = strings.Join(decision.Reasons, "; ")
	return result
}

func (c *Coordinator) confirmWorkerMemoryCandidates(ctx context.Context, manifest *EvidenceManifest) error {
	if c == nil || c.workerMemorySvc == nil || manifest == nil {
		return nil
	}
	scope := c.contextScope()
	scope.BranchID = c.activeBranchID()
	items, err := c.workerMemorySvc.Confirm(ctx, WorkerMemoryPromotionRequest{Scope: scope, Manifest: manifest})
	if err != nil {
		return err
	}
	for _, item := range items {
		_ = c.emitEvent("worker_memory_confirmed", "coordinator", item.Metadata["task_id"], map[string]interface{}{
			"item_id": item.ID, "worker_id": item.Scope.AgentID, "run_id": manifest.RunID, "manifest_hash": manifest.ManifestHash,
		})
	}
	if len(items) > 0 {
		if err := c.rebuildLegacyContextProjections(ctx); err != nil {
			return fmt.Errorf("rebuild shared memory projection: %w", err)
		}
	}
	return nil
}

func (c *Coordinator) rejectWorkerMemoryCandidates(ctx context.Context, manifest *EvidenceManifest, reason string) error {
	if c == nil || c.workerMemorySvc == nil {
		return nil
	}
	runID := c.executionRunID
	if manifest != nil && strings.TrimSpace(manifest.RunID) != "" {
		runID = manifest.RunID
	}
	if strings.TrimSpace(runID) == "" {
		return nil
	}
	scope := c.contextScope()
	scope.BranchID = c.activeBranchID()
	items, err := c.workerMemorySvc.RejectRun(ctx, WorkerMemoryRejectionRequest{Scope: scope, RunID: runID, Reason: reason})
	if err != nil {
		return err
	}
	for _, item := range items {
		_ = c.emitEvent("worker_memory_rejected", "coordinator", item.Metadata["task_id"], map[string]interface{}{
			"item_id": item.ID, "worker_id": item.Scope.AgentID, "run_id": runID, "reason": contextstore.RedactSecrets(reason),
		})
	}
	if len(items) > 0 {
		if err := c.rebuildLegacyContextProjections(ctx); err != nil {
			_ = c.emitEvent("shared_memory_projection_error", "coordinator", "", map[string]interface{}{"error": contextstore.RedactSecrets(err.Error()), "run_id": runID})
		}
	}
	return nil
}

func (c *Coordinator) confirmSharedMemoryCandidates(ctx context.Context, manifest *EvidenceManifest) error {
	if c == nil || c.contextRepo == nil || manifest == nil {
		return nil
	}
	items, err := c.sharedMemoryService().ConfirmRun(ctx, SharedMemoryPromotion{Scope: c.contextScope(), Manifest: manifest})
	if err != nil {
		return err
	}
	for _, item := range items {
		_ = c.emitEvent("shared_memory_confirmed", "coordinator", item.Metadata["task_id"], map[string]interface{}{
			"item_id": item.ID, "run_id": manifest.RunID, "manifest_hash": manifest.ManifestHash, "kind": item.Kind,
		})
	}
	return nil
}

func (c *Coordinator) rejectSharedMemoryCandidates(ctx context.Context, manifest *EvidenceManifest, reason string) error {
	if c == nil || c.contextRepo == nil {
		return nil
	}
	runID := c.executionRunID
	if manifest != nil && strings.TrimSpace(manifest.RunID) != "" {
		runID = manifest.RunID
	}
	items, err := c.sharedMemoryService().RejectRun(ctx, SharedMemoryRejection{Scope: c.contextScope(), RunID: runID, Reason: reason})
	if err != nil {
		return err
	}
	for _, item := range items {
		_ = c.emitEvent("shared_memory_rejected", "coordinator", item.Metadata["task_id"], map[string]interface{}{
			"item_id": item.ID, "run_id": runID, "reason": contextstore.RedactSecrets(reason), "kind": item.Kind,
		})
	}
	return nil
}

// confirmRunSharedContextCandidates promotes the current run's run-produced
// shared session candidates (written by appendCanonicalContext) to confirmed
// knowledge, bound to the accepted evidence manifest. This is the only path
// that makes run-produced shared context prompt-visible.
func (c *Coordinator) confirmRunSharedContextCandidates(ctx context.Context, manifest *EvidenceManifest) error {
	if c == nil || c.contextRepo == nil || manifest == nil || strings.TrimSpace(manifest.RunID) == "" {
		return nil
	}
	ids, err := c.runSharedContextCandidateIDs(ctx, manifest.RunID)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	if err := c.contextRepo.ConfirmCandidates(ctx, ids, contextstore.CandidateBinding{
		Evidence: contextstore.EvidenceRef{Type: "evidence_manifest", Ref: manifest.ManifestHash},
		Metadata: map[string]string{"manifest_hash": manifest.ManifestHash},
	}); err != nil {
		return err
	}
	for _, id := range ids {
		_ = c.emitEvent("run_shared_context_confirmed", "coordinator", "", map[string]interface{}{
			"item_id": id, "run_id": manifest.RunID, "manifest_hash": manifest.ManifestHash,
		})
	}
	return c.rebuildLegacyContextProjections(ctx)
}

// rejectRunSharedContextCandidates rejects the current run's run-produced
// shared session candidates so a failed run's records never become
// prompt-visible knowledge.
func (c *Coordinator) rejectRunSharedContextCandidates(ctx context.Context, manifest *EvidenceManifest, reason string) error {
	if c == nil || c.contextRepo == nil {
		return nil
	}
	runID := c.executionRunID
	if manifest != nil && strings.TrimSpace(manifest.RunID) != "" {
		runID = manifest.RunID
	}
	if strings.TrimSpace(runID) == "" {
		return nil
	}
	ids, err := c.runSharedContextCandidateIDs(ctx, runID)
	if err != nil {
		return err
	}
	if len(ids) == 0 {
		return nil
	}
	if err := c.contextRepo.UpdateLifecycle(ctx, ids, contextstore.LifecycleRejected); err != nil {
		return err
	}
	for _, id := range ids {
		_ = c.emitEvent("run_shared_context_rejected", "coordinator", "", map[string]interface{}{
			"item_id": id, "run_id": runID, "reason": contextstore.RedactSecrets(reason),
		})
	}
	return c.rebuildLegacyContextProjections(ctx)
}

// runSharedContextCandidateIDs selects the session-scoped candidates produced
// by appendCanonicalContext for the given run. Only run_shared_context source
// items are eligible; persistent shared-memory candidates are owned by
// SharedMemoryService.
func (c *Coordinator) runSharedContextCandidateIDs(ctx context.Context, runID string) ([]string, error) {
	items, err := c.contextRepo.Query(ctx, contextstore.RepositoryQuery{
		Scope:             c.contextScope(),
		Visibility:        contextstore.VisibilityExact,
		IncludeCandidates: true,
		Limit:             100000,
	})
	if err != nil {
		return nil, err
	}
	var ids []string
	for _, item := range items {
		if item.Lifecycle == contextstore.LifecycleCandidate && item.Source.Type == "run_shared_context" && item.Metadata["run_id"] == runID {
			ids = append(ids, item.ID)
		}
	}
	return ids, nil
}

// completionGateState reads authoritative runtime state immediately before
// certification. Worker-supplied Risks and OpenQuestions intentionally do not
// participate here: they are report and handoff data, and the task-result
// contract explicitly permits them on a successful or completed-with-gaps
// task. Treating that model-authored prose as a run blocker lets an otherwise
// verified PASS be downgraded merely for disclosing a non-blocking caveat.
//
// Actual incompleteness remains fail-closed through required task status,
// objective evidence, acceptance, and terminal-leak checks in
// EvaluateCompletionGate. Those are runtime-owned facts rather than a model's
// classification of a finding.
func (c *Coordinator) completionGateState() (risks, terminalLeaks []string) {
	if c == nil {
		return []string{"coordinator is unavailable"}, nil
	}
	if c.terminalSessionMgr != nil {
		// A new run may resume in a workspace containing a child from an older
		// run. Completion must inspect all run IDs; narrowing this query to the
		// current run would make the old child invisible and unsafe to replay.
		if err := c.terminalSessionMgr.RequireNoLeaks(""); err != nil {
			terminalLeaks = append(terminalLeaks, err.Error())
		}
	}
	return risks, terminalLeaks
}
