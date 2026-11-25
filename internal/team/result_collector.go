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
	message := fmt.Sprintf("result collection %s: %v", failure.Reason, failure.Cause)
	if failure.Reason == RepairFailureInvalidSchema {
		message += "\nHint: Invalid structured output may be caused by the model or an upstream provider/adapter. This error alone does not establish a Hufu runtime defect. Compare the sent schema and instructions with the raw provider response to identify the cause."
	}
	return message
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
		return &resultCollectionFailure{Reason: localResultRejectionReason(rejectNormalizationRecoveryRead(err)), Checkpoint: checkpoint, Cause: err}
	}
	return c.collectWorkResult(ctx, def, checkpoint)
}

func (c *Coordinator) collectionFailure(ctx context.Context, checkpoint *ResultCollectionCheckpoint, reason RepairFailureReason, cause error) error {
	if reason == RepairFailureCommit {
		return &resultCollectionFailure{Reason: reason, Checkpoint: checkpoint, Cause: cause}
	}
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
		return &resultCollectionFailure{Reason: localResultRejectionReason(rejectNormalizationRecoveryRead(err)), Checkpoint: checkpoint, Cause: err}
	}
	if err := c.validateInitialCollectionReads(ctx, checkpoint); err != nil {
		return err
	}
	// A local reference selection has no model fallback, including after restart.
	if acceptedReferenceProjectionFor(c.todoItemByID(checkpoint.TaskID), checkpoint) != nil {
		return c.commitAcceptedReferenceSelection(ctx, checkpoint)
	}
	receipt := collectionReceipt(c.todoItemByID(checkpoint.TaskID), checkpoint)
	sourceRecollection := grantedSourceFieldRecollection(receipt, checkpoint)
	diagnostic := collectionRepairDiagnostic(receipt, c.executionRunID, sourceRecollection)
	for {
		if err := ctx.Err(); err != nil {
			return &resultCollectionFailure{Reason: repairFailureCollectionUnavailable, Checkpoint: checkpoint, Cause: err}
		}
		switch checkpoint.State {
		case ResultCollectionCommitted:
			_, err := c.acceptedCollectionResult(ctx, c.todoItemByID(checkpoint.TaskID), checkpoint)
			return err
		case ResultCollectionPrepared:
			err = c.commitPreparedCollection(ctx, checkpoint)
			if err == nil {
				return nil
			}
			if localResultRejectionReason(err) != RepairFailureInvalidSchema {
				return c.collectionFailure(ctx, checkpoint, localResultRejectionReason(err), err)
			}
			diagnostic = err.Error()
			checkpoint, err = c.saveCollectionFormatFailure(ctx, checkpoint, err)
			if err != nil {
				return err
			}
		case ResultCollectionReceived:
			if sourceRecollection != nil {
				// This exact response was already rejected and independently
				// assessed. Recollect from it without attempting to commit it.
				sourceRecollection = nil
				break
			}
			prepared, err := c.prepareCollectionClaims(ctx, checkpoint)
			if err == nil {
				checkpoint = prepared
				continue
			}
			if reason := localResultRejectionReason(err); reason != RepairFailureInvalidSchema && reason != RepairFailureResultMissing {
				return c.collectionFailure(ctx, checkpoint, localResultRejectionReason(err), err)
			}
			diagnostic = err.Error()
			checkpoint, err = c.saveCollectionFormatFailure(ctx, checkpoint, err)
			if err != nil {
				return err
			}
		case ResultCollectionBlocked:
			if !collectionGrantedForRun(collectionReceipt(c.todoItemByID(checkpoint.TaskID), checkpoint), c.executionRunID) {
				return &resultCollectionFailure{Reason: checkpoint.FailureReason, Checkpoint: checkpoint, Cause: errResultCollectionBlocked}
			}
		case ResultCollectionInFlight:
			received, err := c.restoreCollectionResponse(ctx, checkpoint)
			if err != nil {
				return c.collectionFailure(ctx, checkpoint, localResultRejectionReason(rejectNormalizationRecoveryRead(err)), err)
			}
			if received != nil {
				checkpoint = received
				continue
			}
		}
		if checkpoint.TurnsUsed >= checkpoint.TurnLimit {
			if diagnostic != "" {
				return c.collectionFailure(ctx, checkpoint, RepairFailureInvalidSchema, errors.New(diagnostic))
			}
			return c.collectionFailure(ctx, checkpoint, repairFailureCollectionBudget, errResultCollectionBudget)
		}
		if acceptedRepairProof(checkpoint) != nil {
			return c.collectionFailure(ctx, checkpoint, RepairFailureInvalidSchema, errors.New("accepted-result repair requires an explicit observed reference selection"))
		}
		if err := c.canonicalJournalOwner().providerBoundaryRequired(ctx); err != nil {
			return c.collectionFailure(ctx, checkpoint, repairFailureCollectionUnavailable, err)
		}
		id := "result-collection-" + rand.Text()
		invocation, request, err := c.prepareCollectionInvocation(ctx, def, checkpoint, id, diagnostic)
		if err != nil {
			if _, ok := errors.AsType[*localResultAdmissionError](err); ok {
				return c.collectionFailure(ctx, checkpoint, localResultRejectionReason(err), err)
			}
			return c.collectionFailure(ctx, checkpoint, repairFailurePreparation, err)
		}
		checkpoint, err = c.executeFrozenCollectionTurn(ctx, invocation, def, checkpoint, id, request)
		if err != nil {
			return err
		}
	}
}

func (c *Coordinator) saveCollectionFormatFailure(ctx context.Context, checkpoint *ResultCollectionCheckpoint, rejection error) (*ResultCollectionCheckpoint, error) {
	next, err := c.recordCollectionFormatFailure(ctx, checkpoint, rejection)
	if errors.Is(err, errResultCollectionNoProgress) {
		return nil, c.collectionFailure(ctx, next, RepairFailureInvalidSchema, errors.Join(err, rejection))
	}
	if err != nil {
		return nil, c.collectionFailure(ctx, checkpoint, localResultRejectionReason(rejectNormalizationRecoveryRead(err)), err)
	}
	return next, nil
}

func collectionRepairDiagnostic(receipt *ExecutionReceipt, runID string, sourceRecollection *ResultCollectionRecoveryAssessment) string {
	var diagnostic string
	for _, grant := range collectionRepairGrants(receipt) {
		if grant.Recovery != nil && (grant.RunID == runID || grant.Recovery == sourceRecollection) {
			for _, check := range grant.Recovery.Checks {
				if !check.Passed {
					diagnostic += check.Diagnostic + "\n"
				}
			}
		}
	}
	return diagnostic
}

type frozenCollectionRequest struct {
	BoundSourceFields   bool                           `json:"bound_source_fields,omitempty"`
	ResponseChannel     agent.CollectorResponseChannel `json:"response_channel,omitempty"`
	Version             int                            `json:"version"`
	InvocationID        string                         `json:"invocation_id"`
	Model               string                         `json:"model"`
	System              string                         `json:"system"`
	Prompt              string                         `json:"prompt"`
	ManifestFingerprint string                         `json:"manifest_fingerprint"`
	Generation          agent.GenerationParams         `json:"generation"`
	TeamGeneration      agent.GenerationParams         `json:"team_generation"`
	Tools               []any                          `json:"tools"`
	ClaimsSchema        map[string]any                 `json:"claims_schema,omitempty"`
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
	if request.ClaimsSchema != nil && request.ClaimsSchema["type"] != "object" {
		return request, errResultCollectionStale
	}
	if request.ResponseChannel != "" && request.ResponseChannel != agent.CollectorResponseFunction && request.ResponseChannel != agent.CollectorResponseTextJSON {
		return request, errResultCollectionStale
	}
	if request.ResponseChannel == agent.CollectorResponseFunction && request.ClaimsSchema == nil {
		return request, errResultCollectionStale
	}
	return request, nil
}

func (c *Coordinator) validateInitialCollectionReads(ctx context.Context, checkpoint *ResultCollectionCheckpoint) error {
	if checkpoint.State != ResultCollectionPending || checkpoint.TurnsUsed != 0 || checkpoint.LegacySubmission != nil || checkpoint.ContractUpgrade != nil {
		return nil
	}
	ctx = context.WithValue(ctx, resultCollectionAdmissionKey{}, checkpoint)
	err := c.validateRequiredArtifactReadSubmission(ctx, checkpoint.TaskID)
	if err == nil {
		return nil
	}
	return c.collectionFailure(ctx, checkpoint, localResultRejectionReason(rejectNormalizationRecoveryRead(err)), err)
}

// A task-transition event can persist this native failure even when a prior
// collection checkpoint append failed or the work context was cancelled.
func (c *Coordinator) recordCollectionStopReceipt(taskID string, cause error) {
	item := c.todoItemByID(taskID)
	if item == nil || item.ExecutionReceipt == nil {
		return
	}
	checkpoint := currentResultCollection(item)
	failure, classified := errors.AsType[*resultCollectionFailure](cause)
	if checkpoint == nil && !classified {
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
	if classified {
		reason = failure.Reason
	}
	if checkpoint != nil {
		provenance.Attempted, provenance.RepairAttempts = true, checkpoint.TurnsUsed
	}
	provenance.FailureReason = reason
	if cause != nil {
		provenance.Error = schemaRepairDiagnostic(cause.Error())
	}
	_ = c.setAttemptReceipt(taskID, &receipt)
}
