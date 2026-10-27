package team

import (
	"context"
	"crypto/rand"
	"encoding/json/v2"
	"errors"
	"fmt"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/execution"
	"github.com/kjelly/hufu/internal/tools"
)

type resultCollectionInvocationKey struct{}
type resultCollectionWorkInputKey struct{}
type resultCollectionGenerationKey struct{}
type workerRuntimeResultKey struct{}
type resultCollectionStopKey struct{}
type resultCollectionTimeoutKey struct{}

var errDeprecatedResultTransport = errors.New("deprecated_result_transport")
var errCollectionToolCall = errors.New("collector_tool_call")

const runtimeWorkerResultInstructions = "\n\n## Runtime Result Collection\nFinish the authorized work and return its complete report, including findings, decisions, evidence references, checks and limitations. The runtime collects typed result claims after this work turn. Do not call submit_result or reserve a tool step for reporting. Do not repeat completed work to correct reporting format. A report does not bypass contract validation, objective verification, or acceptance.\n"

func (c *Coordinator) stopDeprecatedResultReport(ctx context.Context, call fantasy.ToolCallContent) error {
	runtime, _ := ctx.Value(workerRuntimeResultKey{}).(bool)
	collector, _ := ctx.Value(resultCollectionInvocationKey{}).(string)
	if !runtime || collector != "" || call.ToolName != submitResultToolName {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	agentName, _ := ctx.Value(tools.AgentNameKey).(string)
	denial, err := c.authorizeToolInvocation(ctx, agentName, call.ToolName)
	if err != nil {
		return err
	}
	if denial == "" {
		return fmt.Errorf("runtime result transport was not denied")
	}
	tools.ReportToolExecutionDisposition(ctx, tools.ToolExecutionDisposition{Kind: "policy_denied", ReasonCode: "deprecated_result_transport", ToolName: call.ToolName, ToolCallID: call.ToolCallID, Executed: false})
	c.setToolPolicyVerdict(call.ToolCallID, "denied")
	if transcript, _ := ctx.Value(taskTranscriptKey{}).(*taskTranscript); transcript != nil {
		if err := transcript.RecordToolResult(call.ToolCallID, call.ToolName, denial, true); err != nil {
			return err
		}
	}
	return errDeprecatedResultTransport
}

type resultCollectionFailure struct {
	Reason     RepairFailureReason
	Checkpoint *ResultCollectionCheckpoint
	Cause      error
}

func (failure *resultCollectionFailure) Error() string {
	return fmt.Sprintf("result collection %s: %v", failure.Reason, failure.Cause)
}
func (failure *resultCollectionFailure) Unwrap() error { return failure.Cause }

// collectReturnedWork is the sole bridge from a completed local work turn to
// collection. Every error is result-only: callers must never retry worker tools.
func (c *Coordinator) collectReturnedWork(ctx context.Context, def *agent.AgentDef, receipt ExecutionReceipt, report, input string) error {
	if item := c.todoItemByID(receipt.TaskID); item != nil {
		// These are assigned by admission, never reconstructed by the model.
		receipt.RunInputSnapshotID, receipt.RunInputSnapshotHash = item.RunInputSnapshotID, item.RunInputSnapshotHash
	}
	ctx = context.WithValue(ctx, resultCollectionWorkInputKey{}, input)
	generation := def.Generation
	generation.Model = receipt.Model
	ctx = context.WithValue(ctx, resultCollectionGenerationKey{}, generation)
	timeout := c.session.Config.Timeout
	if def.Timeout > 0 {
		timeout = def.Timeout
	}
	if timeout <= 0 {
		timeout = 600
	}
	ctx = context.WithValue(ctx, resultCollectionTimeoutKey{}, timeout)
	checkpoint, err := c.beginResultCollection(ctx, receipt.TaskID, receipt, report)
	if err != nil {
		return &resultCollectionFailure{Reason: RepairFailureCommit, Cause: err}
	}
	return c.collectWorkResult(ctx, def, checkpoint)
}

func (c *Coordinator) collectionFailure(ctx context.Context, checkpoint *ResultCollectionCheckpoint, reason RepairFailureReason, cause error) error {
	blocked, err := c.blockResultCollection(ctx, checkpoint, reason)
	if err == nil {
		checkpoint = blocked
	}
	return &resultCollectionFailure{Reason: reason, Checkpoint: checkpoint, Cause: errors.Join(cause, err)}
}

// collectWorkResult consumes durable received/prepared bytes before considering
// a provider. Ordinary resume cannot reset the work binding or model-turn budget.
func (c *Coordinator) collectWorkResult(ctx context.Context, def *agent.AgentDef, checkpoint *ResultCollectionCheckpoint) error {
	if err := ctx.Err(); err != nil {
		return &resultCollectionFailure{Reason: repairFailureCollectionUnavailable, Checkpoint: checkpoint, Cause: err}
	}
	if checkpoint != nil && checkpoint.State == ResultCollectionCommitted {
		item := c.todoItemByID(checkpoint.TaskID)
		result, err := c.acceptedCollectionResult(ctx, item, checkpoint)
		if err != nil {
			return err
		}
		c.storeSubmittedTaskResult(item.ID, result)
		return nil
	}
	var err error
	ctx, err = c.restoreCollectionContext(ctx, checkpoint)
	if err != nil {
		return &resultCollectionFailure{Reason: RepairFailureSource, Checkpoint: checkpoint, Cause: err}
	}
	var diagnostic string
	for {
		if err := ctx.Err(); err != nil {
			return &resultCollectionFailure{Reason: repairFailureCollectionUnavailable, Checkpoint: checkpoint, Cause: err}
		}
		switch checkpoint.State {
		case ResultCollectionCommitted:
			_, err := c.acceptedCollectionResult(ctx, c.todoItemByID(checkpoint.TaskID), checkpoint)
			return err
		case ResultCollectionPrepared:
			if err := c.commitPreparedCollection(ctx, checkpoint); err != nil {
				return c.collectionFailure(ctx, checkpoint, localResultRejectionReason(err), err)
			}
			return nil
		case ResultCollectionReceived:
			prepared, err := c.prepareCollectionClaims(ctx, checkpoint)
			if err == nil {
				checkpoint = prepared
				continue
			}
			if reason := localResultRejectionReason(err); reason != RepairFailureInvalidSchema && reason != RepairFailureResultMissing {
				return c.collectionFailure(ctx, checkpoint, localResultRejectionReason(err), err)
			}
			diagnostic = err.Error()
			if checkpoint.TurnsUsed >= checkpoint.TurnLimit {
				return c.collectionFailure(ctx, checkpoint, localResultRejectionReason(err), err)
			}
			checkpoint, err = c.recordCollectionSchemaFailure(ctx, checkpoint)
			if err != nil {
				return &resultCollectionFailure{Reason: RepairFailureCommit, Cause: err}
			}
		case ResultCollectionBlocked:
			if !collectionGrantedForRun(collectionReceipt(c.todoItemByID(checkpoint.TaskID), checkpoint), c.executionRunID) {
				return &resultCollectionFailure{Reason: checkpoint.FailureReason, Checkpoint: checkpoint, Cause: errResultCollectionBlocked}
			}
		}
		if checkpoint.TurnsUsed >= checkpoint.TurnLimit {
			return c.collectionFailure(ctx, checkpoint, repairFailureCollectionBudget, errResultCollectionBudget)
		}
		id := "result-collection-" + rand.Text()
		invocation, request, err := c.prepareCollectionInvocation(ctx, def, checkpoint, id, diagnostic)
		if err != nil {
			return c.collectionFailure(ctx, checkpoint, repairFailurePreparation, err)
		}
		checkpoint, err = c.reserveResultCollection(ctx, checkpoint, id, request)
		if err != nil {
			return &resultCollectionFailure{Reason: RepairFailureCommit, Checkpoint: checkpoint, Cause: err}
		}
		response, usage, err := c.runCollectionInvocation(invocation, def, checkpoint)
		if errors.Is(err, errCollectionToolCall) {
			// The callback rejected it before any handler could run. Preserve
			// the denial as invalid JSON evidence for the same bounded repair.
			response += "\ncollector_tool_call: no tools are authorized"
			err = nil
		}
		if err != nil {
			return c.collectionFailure(ctx, checkpoint, repairFailureCollectionUnavailable, err)
		}
		checkpoint, err = c.recordCollectionResponseUsage(ctx, checkpoint, []byte(response), usage)
		if err != nil {
			return &resultCollectionFailure{Reason: RepairFailureCommit, Checkpoint: checkpoint, Cause: err}
		}
	}
}

type frozenCollectionRequest struct {
	Version             int                    `json:"version"`
	InvocationID        string                 `json:"invocation_id"`
	Model               string                 `json:"model"`
	System              string                 `json:"system"`
	Prompt              string                 `json:"prompt"`
	ManifestFingerprint string                 `json:"manifest_fingerprint"`
	Generation          agent.GenerationParams `json:"generation"`
	TeamGeneration      agent.GenerationParams `json:"team_generation"`
	Tools               []any                  `json:"tools"`
}

func decodeFrozenCollectionRequest(data []byte, checkpoint *ResultCollectionCheckpoint) (frozenCollectionRequest, error) {
	var request frozenCollectionRequest
	if err := json.Unmarshal(data, &request, json.RejectUnknownMembers(true)); err != nil {
		return request, err
	}
	selector, err := execution.ParseExecutionSelector(request.Model)
	if err != nil || selector.Model != checkpoint.ExecutionTarget.Model || (selector.Backend != "" && selector.Backend != checkpoint.ExecutionTarget.Backend) || request.Generation.Model != request.Model || request.ManifestFingerprint == "" {
		return request, errResultCollectionStale
	}
	if request.Version != resultCollectionVersion || request.InvocationID != checkpoint.InvocationID || request.Prompt == "" || len(request.Tools) != 0 {
		return request, errResultCollectionStale
	}
	return request, nil
}

// A task-transition event can persist this native failure even when a prior
// collection checkpoint append failed or the work context was cancelled. It
// preserves the earlier CAS progress rather than forging a new model result.
func (c *Coordinator) recordCollectionStopReceipt(taskID string, cause error) {
	item := c.todoItemByID(taskID)
	checkpoint := currentResultCollection(item)
	if checkpoint == nil {
		return
	}
	receipt := cloneExecutionReceipt(item.ExecutionReceipt)
	if receipt.RepairProvenance == nil {
		receipt.RepairProvenance = &RepairProvenance{}
	}
	provenance := receipt.RepairProvenance
	if provenance.Success {
		return
	}
	reason := RepairFailureSource
	if failure, ok := errors.AsType[*resultCollectionFailure](cause); ok {
		reason = failure.Reason
	}
	provenance.Attempted, provenance.RepairAttempts, provenance.FailureReason = true, checkpoint.TurnsUsed, reason
	_ = c.setAttemptReceipt(taskID, &receipt)
}
