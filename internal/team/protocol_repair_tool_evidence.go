package team

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"strings"

	"charm.land/fantasy"
)

// Separate validation evidence from the stream's recorder. A result-only
// repair may read the original observations, but must never append its own
// tool calls or assistant output to that sealed transcript.
type protocolRepairToolEvidenceKey struct{}

func withProtocolRepairToolEvidence(ctx context.Context, transcript *taskTranscript) context.Context {
	// A nested dispatch can inherit its parent's recorder. Mask that too:
	// only the separate read-only validation channel belongs in repair.
	ctx = context.WithValue(ctx, taskTranscriptKey{}, (*taskTranscript)(nil))
	return context.WithValue(ctx, protocolRepairToolEvidenceKey{}, transcript)
}

func (c *Coordinator) protocolRepairUsesToolEvidence(todoID string) bool {
	_, contract, err := c.boundResultContract(todoID)
	return err == nil && contract != nil && contract.toolEvidence != nil
}

func (c *Coordinator) protocolRepairCanRetryFinalization(task TaskDef, todoID string, reason RepairFailureReason) bool {
	return task.SideEffect == SideEffectNone && c.protocolRepairUsesToolEvidence(todoID) &&
		(reason == repairFailurePreparation || reason == RepairFailureNoToolCall)
}

func (c *Coordinator) protocolRepairObservations(todoID string, transcript *taskTranscript, steps []fantasy.StepResult) (string, bool) {
	if c.protocolRepairUsesToolEvidence(todoID) {
		// The citation contract needs the actual source text, including late
		// observations. Its authenticated runner-owned evidence is already
		// bounded at 16 MiB. Admit the complete text through the normal context
		// gate instead of silently clipping it to the first page's navigation.
		return protocolRepairObservationEvidenceWithinBudget(protocolRepairTranscriptSteps(transcript), c.readOnlyToolCall, 16<<20)
	}
	return protocolRepairObservationEvidence(steps, c.readOnlyToolCall)
}

func protocolRepairTranscriptSteps(transcript *taskTranscript) []fantasy.StepResult {
	if transcript == nil {
		return nil
	}
	var content fantasy.ResponseContent
	for _, record := range transcript.evidenceRecords() {
		switch record.Event {
		case "tool_call":
			content = append(content, fantasy.ToolCallContent{ToolCallID: record.ToolCallID, ToolName: record.Tool, Input: record.Input})
		case "tool_result":
			result := fantasy.ToolResultContent{ToolCallID: record.ToolCallID, ToolName: record.Tool, Result: fantasy.ToolResultOutputContentText{Text: record.Output}}
			if record.Error {
				result.Result = fantasy.ToolResultOutputContentError{Error: errors.New(record.Output)}
			}
			content = append(content, result)
		}
	}
	return []fantasy.StepResult{{Response: fantasy.Response{Content: content}}}
}

func submissionToolEvidence(ctx context.Context, c *Coordinator, todoID string) ([]taskTranscriptRecord, error) {
	transcript, _ := ctx.Value(taskTranscriptKey{}).(*taskTranscript)
	if repairing, _ := ctx.Value(protocolRepairExecutionKey{}).(bool); repairing {
		transcript, _ = ctx.Value(protocolRepairToolEvidenceKey{}).(*taskTranscript)
	}
	if transcript == nil || transcript.todoID != todoID {
		return nil, nil
	}
	identity, err := submitResultRuntimeIdentityFromContext(ctx, c, todoID)
	if err != nil {
		return nil, err
	}
	if transcript.runID != identity.RunID || transcript.attempt != identity.Attempt || transcript.agent != identity.Agent {
		return nil, nil
	}
	return transcript.evidenceRecords(), nil
}

func (c *Coordinator) validateProtocolRepairEvidenceReceipt(item *TodoItem, ref *ResultContractRef) error {
	receipt := item.ExecutionReceipt
	if receipt == nil || receipt.TaskID != item.ID || receipt.RunID == "" || receipt.Attempt < 1 ||
		!strings.EqualFold(receipt.ProducerID, item.Agent) || !validArtifactID(receipt.TranscriptRef) {
		return fmt.Errorf("protocol repair has no bound original tool transcript")
	}
	if receipt.RunInputSnapshotID != item.RunInputSnapshotID || receipt.RunInputSnapshotHash != item.RunInputSnapshotHash ||
		receipt.ResultContractID != ref.ID || receipt.ResultContractSHA256 != ref.SchemaSHA256 {
		return fmt.Errorf("protocol repair transcript does not match the frozen inputs or result contract")
	}
	if scope := receipt.ArtifactScope; scope != nil &&
		(scope.RunID != receipt.RunID || scope.TaskID != item.ID || scope.Attempt != receipt.Attempt) {
		return fmt.Errorf("protocol repair artifact scope does not match the original receipt")
	}
	if snapshot := c.RunInputSnapshot(); snapshot != nil &&
		(snapshot.ID != receipt.RunInputSnapshotID || snapshot.SnapshotHash != receipt.RunInputSnapshotHash) {
		return fmt.Errorf("protocol repair transcript belongs to different run inputs")
	}
	if receipt.RunID != c.executionRunID {
		snapshot := c.RunInputSnapshot()
		if snapshot == nil || snapshot.RunID != receipt.RunID {
			return fmt.Errorf("protocol repair transcript belongs to another run")
		}
	}
	return nil
}

// restoreProtocolRepairToolEvidence rehydrates only the transcript pinned by
// the interrupted occurrence's receipt. Never trust its writable diagnostic
// path, a model-supplied reference, or a newer attempt's observations.
func (c *Coordinator) restoreProtocolRepairToolEvidence(ctx context.Context, item *TodoItem) (*taskTranscript, error) {
	ref, compiled, err := c.boundResultContract(item.ID)
	if err != nil || compiled == nil || compiled.toolEvidence == nil {
		return nil, err
	}
	if err := c.validateProtocolRepairEvidenceReceipt(item, ref); err != nil {
		return nil, err
	}
	receipt := item.ExecutionReceipt
	store, err := OpenFileArtifactStoreReadOnly(c.artifactStoreRootPath(), c.artifactStoreRootPath())
	if err != nil {
		return nil, err
	}
	artifact, err := store.Get(ctx, receipt.TranscriptRef)
	if err != nil {
		return nil, err
	}
	const maxBytes = 16 << 20
	if artifact.Bytes < 0 || artifact.Bytes > maxBytes || artifact.ByteSize != artifact.Bytes || artifact.SHA256 == "" || artifact.MediaType != taskTranscriptMediaType {
		return nil, fmt.Errorf("protocol repair transcript has invalid size or digest")
	}
	body, err := store.Open(ctx, receipt.TranscriptRef)
	if err != nil {
		return nil, err
	}
	data, readErr := io.ReadAll(io.LimitReader(body, maxBytes+1))
	if err := errors.Join(readErr, body.Close()); err != nil {
		return nil, err
	}
	digest := sha256.Sum256(data)
	if int64(len(data)) != artifact.Bytes || hex.EncodeToString(digest[:]) != artifact.SHA256 || receipt.TranscriptRef != "sha256-"+artifact.SHA256 {
		return nil, fmt.Errorf("protocol repair transcript content does not match its receipt")
	}
	// The receipt retains the original execution identity. Validation is
	// authorized for this resumed invocation only after the checks above.
	transcript := &taskTranscript{todoID: item.ID, runID: c.executionRunID, attempt: receipt.Attempt, agent: strings.ToLower(item.Agent)}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 4096), (1<<20)+1)
	for scanner.Scan() {
		var record taskTranscriptRecord
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			return nil, fmt.Errorf("decode protocol repair transcript: %w", err)
		}
		if record.Event == "tool_call" || record.Event == "tool_result" || record.Event == "historical_observation" {
			transcript.retainEvidence(record, len(scanner.Bytes()))
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if transcript.evidenceInvalid {
		return nil, fmt.Errorf("protocol repair tool evidence exceeds its retention limit")
	}
	return transcript, nil
}
