package team

import (
	"context"
	"encoding/json/v2"
	"fmt"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/tools"
)

// commitPreparedResultCandidate consumes only durable model-authored bytes.
// It does not create a provider, spend a repair turn, or replay worker tools.
func (c *Coordinator) commitPreparedResultCandidate(ctx context.Context, item *TodoItem) (fantasy.ToolResponse, error) {
	if item == nil || item.ExecutionReceipt == nil || item.ResultCandidate == nil || !item.ResultCandidate.Prepared {
		return submissionFailureResponse(RepairFailureSource, "prepared candidate requires its original receipt"), nil
	}
	value, err := c.loadResultCandidate(ctx, item, item.ResultCandidate)
	if err != nil {
		return submissionFailureResponse(RepairFailureSource, err.Error()), nil
	}
	canonical, err := c.canonicalWorkerAttemptContextForID(item.ID)
	if err != nil {
		return submissionFailureResponse(RepairFailureContract, err.Error()), nil
	}
	phase := PhaseExecute
	enabled := c.phaseWorkflow != nil && c.phaseWorkflow.Enabled()
	if enabled {
		phase = c.phaseWorkflow.State()
	}
	_, err = ResolveStaticWorkerTools(StaticToolResolutionInput{
		Session: c.session, Agent: canonical.Agent, Task: canonical.Task,
		LifecycleMode: WorkerToolResolutionResume, WorkflowEnabled: enabled, WorkflowPhase: phase,
		Policy: EffectiveTeamContractContext{NoNet: c.noNet || canonical.Agent.NoNet, ForceMCP: c.forceMCP || canonical.Agent.ForceMCP},
	})
	if err != nil {
		return submissionFailureResponse(RepairFailureContract, err.Error()), nil
	}
	evidence, err := c.restoreProtocolRepairToolEvidence(ctx, item)
	if err != nil {
		return submissionFailureResponse(RepairFailureSource, err.Error()), nil
	}
	ctx = withProtocolRepairToolEvidence(ctx, evidence)
	ctx = context.WithValue(ctx, protocolRepairExecutionKey{}, true)
	ctx = context.WithValue(ctx, todoIDKey{}, item.ID)
	ctx = context.WithValue(ctx, tools.AgentNameKey, item.Agent)
	ctx = context.WithValue(ctx, tools.AgentToolsAllowedKey, []string{submitResultToolName})
	if item.ExecutionReceipt != nil && item.ExecutionReceipt.ArtifactScope != nil {
		ctx = context.WithValue(ctx, artifactAccessScopeKey, cloneArtifactAccessScope(item.ExecutionReceipt.ArtifactScope))
	}
	if item.ExecutionReceipt != nil && item.ExecutionReceipt.ModelExecutionID != "" {
		manifest, err := authoritativeTaskContextManifest(item, item.ResultCandidate.Attempt, item.ExecutionReceipt.ModelExecutionID)
		if err != nil {
			return submissionFailureResponse(RepairFailureSource, err.Error()), nil
		}
		ctx = withInvocationMetadata(ctx, invocationMetadataFromManifest(manifest))
	}
	c.setCurrentTaskAttempt(item.ID, item.ResultCandidate.Attempt)
	identity, ok := c.activeTaskResultOccurrence(item.ID)
	if !ok {
		return submissionFailureResponse(RepairFailureSource, "candidate occurrence unavailable"), nil
	}
	ctx = withSubmitResultRuntimeIdentity(ctx, identity)
	ctx = context.WithValue(ctx, resultPatchBaseKey{}, item.ResultCandidate.SHA256)
	data, err := json.Marshal(value, json.Deterministic(true))
	if err != nil {
		return fantasy.ToolResponse{}, err
	}
	tool := &policyGatedTool{coordinator: c, inner: &submitResultTool{coordinator: c, todoID: item.ID}}
	return tool.Run(ctx, fantasy.ToolCall{ID: "candidate-" + item.ResultCandidate.SHA256, Name: submitResultToolName, Input: string(data)})
}

func (c *Coordinator) resumePreparedResultCandidate(ctx context.Context, task TaskDef, item *TodoItem) (string, error) {
	response, err := c.commitPreparedResultCandidate(ctx, item)
	if err == nil && response.IsError {
		err = fmt.Errorf("prepared candidate rejected: %s", response.Content)
	}
	if err != nil {
		if receiptErr := c.recordPreparedCandidateFailure(item, response, err); receiptErr != nil {
			return "", receiptErr
		}
		c.PersistFailureWithClassAndStatusAndOutput(item.Agent, task.Goal, item.ID,
			c.FailureDetail(err, FailureSourceError), NeedsHuman, FailureProtocol, TaskBlocked, item.Output)
		return "", err
	}
	result := c.GetTaskResult(item.ID)
	if result == nil {
		return "", fmt.Errorf("prepared candidate did not commit a result")
	}
	if err := validateCompletedTaskResult(result); err != nil {
		c.PersistFailureWithClassAndStatusAndOutput(item.Agent, task.Goal, item.ID, c.FailureDetail(err, FailureSourceError), NeedsHuman, FailureProtocol, TaskBlocked, item.Output)
		return "", err
	}
	if err := c.materializePreparedCandidateReceipt(item, result); err != nil {
		return "", err
	}
	return c.finishProtocolRepair(ctx, item, task, item.Agent, task.Model, result, item.Output)
}

func (c *Coordinator) materializePreparedCandidateReceipt(item *TodoItem, result *TaskResult) error {
	if item.ExecutionReceipt == nil {
		return fmt.Errorf("prepared result requires original execution receipt")
	}
	receipt := cloneExecutionReceipt(item.ExecutionReceipt)
	if receipt.RepairProvenance == nil {
		receipt.RepairProvenance = &RepairProvenance{}
	}
	p := receipt.RepairProvenance
	p.Success, p.SubmittedResult = true, result
	p.FailureReason, p.Error = "", ""
	p.Attempted = true
	p.PreparedCandidateSHA256 = item.ResultCandidate.SHA256
	c.applyResultContractReceipt(item.ID, result, &receipt)
	if err := c.setAttemptReceipt(item.ID, &receipt); err != nil {
		return err
	}
	return c.emitTaskEventsFromCheckpoint(c.taskTracker.TodoList().Items())
}

func (c *Coordinator) recordPreparedCandidateFailure(item *TodoItem, response fantasy.ToolResponse, failure error) error {
	if item.ExecutionReceipt == nil {
		return nil
	}
	receipt := cloneExecutionReceipt(item.ExecutionReceipt)
	if receipt.RepairProvenance == nil {
		receipt.RepairProvenance = &RepairProvenance{}
	}
	reason := submissionFailureMetadata(response.Metadata)
	if reason == "" {
		reason = RepairFailureCommit
	}
	receipt.RepairProvenance.FailureReason = reason
	receipt.RepairProvenance.Error = c.FailureDetail(failure, FailureSourceError)
	receipt.RepairProvenance.PreparedCandidateSHA256 = item.ResultCandidate.SHA256
	return c.setAttemptReceipt(item.ID, &receipt)
}
