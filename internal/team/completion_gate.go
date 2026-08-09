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
	if result == nil || result.Outcome != RunOutcomeCompleted || !result.GoalSatisfied {
		return result
	}
	// A coordinator without a workspace is only used by lightweight unit
	// callers; production sessions always have a workspace and therefore pass
	// through the durable manifest gate.
	if c == nil || c.session == nil || strings.TrimSpace(c.session.Workspace) == "" {
		return result
	}
	c.lastEvidenceManifestMu.RLock()
	manifest := c.lastEvidenceManifest
	c.lastEvidenceManifestMu.RUnlock()
	items := c.taskTracker.TodoList().Items()
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
	if decision.Accepted {
		if err := c.confirmWorkerMemoryCandidates(ctx, manifest); err != nil {
			c.rejectWorkerMemoryCandidates(ctx, manifest, "accepted manifest could not confirm private candidates: "+err.Error())
			result.Outcome = RunOutcomePartial
			result.GoalSatisfied = false
			result.StopReason = StopReasonEvidenceIncomplete
			result.ExitCode = 7
			result.Reason = "worker memory candidate promotion failed: " + err.Error()
			return result
		}
		if err := c.bindCandidateLessonsToManifest(manifest); err != nil {
			c.rejectWorkerMemoryCandidates(ctx, manifest, "legacy candidate manifest binding failed: "+err.Error())
			result.Outcome = RunOutcomePartial
			result.GoalSatisfied = false
			result.StopReason = StopReasonEvidenceIncomplete
			result.ExitCode = 7
			result.Reason = "candidate manifest binding failed: " + err.Error()
			return result
		}
		c.promoteCandidateLessons(manifest)
		return result
	}
	c.rejectWorkerMemoryCandidates(ctx, manifest, strings.Join(decision.Reasons, "; "))
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
	return nil
}

func (c *Coordinator) rejectWorkerMemoryCandidates(ctx context.Context, manifest *EvidenceManifest, reason string) {
	if c == nil || c.workerMemorySvc == nil {
		return
	}
	runID := c.executionRunID
	if manifest != nil && strings.TrimSpace(manifest.RunID) != "" {
		runID = manifest.RunID
	}
	if strings.TrimSpace(runID) == "" {
		return
	}
	scope := c.contextScope()
	scope.BranchID = c.activeBranchID()
	items, err := c.workerMemorySvc.RejectRun(ctx, WorkerMemoryRejectionRequest{Scope: scope, RunID: runID, Reason: reason})
	if err != nil {
		return
	}
	for _, item := range items {
		_ = c.emitEvent("worker_memory_rejected", "coordinator", item.Metadata["task_id"], map[string]interface{}{
			"item_id": item.ID, "worker_id": item.Scope.AgentID, "run_id": runID, "reason": contextstore.RedactSecrets(reason),
		})
	}
}

// completionGateState reads authoritative coordinator state immediately before
// certification. Observation errors are represented as findings so the gate
// fails closed rather than treating an unavailable state as clean.
func (c *Coordinator) completionGateState() (risks, terminalLeaks []string) {
	if c == nil {
		return []string{"coordinator is unavailable"}, nil
	}
	if c.taskTracker != nil && c.taskTracker.TodoList() != nil {
		for _, item := range c.taskTracker.TodoList().Items() {
			if item == nil || item.TypedResult == nil {
				continue
			}
			for _, risk := range item.TypedResult.Risks {
				if strings.TrimSpace(risk.Description) != "" {
					risks = append(risks, fmt.Sprintf("task %s: %s", item.ID, strings.TrimSpace(risk.Description)))
				}
			}
			for _, question := range item.TypedResult.OpenQuestions {
				if strings.TrimSpace(question) != "" {
					risks = append(risks, fmt.Sprintf("task %s open question: %s", item.ID, strings.TrimSpace(question)))
				}
			}
		}
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
