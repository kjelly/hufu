package team

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// eventStoreExecutionEventsFile is the Phase-5 shadow projection. The legacy
// execution-events writer remains active until normalized parity is proven;
// this exporter is deliberately pure and never changes runtime state.
const eventStoreExecutionEventsFile = "execution-events.event-store.jsonl"

// ExecutionEventFromRunEvent maps canonical event-store records into the
// existing debug/telemetry schema. It intentionally omits transcript and raw
// output fields because EventStore holds only redacted metadata.
func ExecutionEventFromRunEvent(event RunEvent) (ExecutionEvent, bool) {
	out := ExecutionEvent{
		Version:      executionEventSchemaVersion,
		Timestamp:    event.Timestamp,
		InvocationID: event.InvocationID,
		RunID:        event.RunID,
		TaskID:       event.TaskID,
		Agent:        event.Actor,
	}
	switch event.Type {
	case string(EventRunStarted):
		out.Status = "run_started"
		var payload LifecycleEventPayload
		if json.Unmarshal(event.Payload, &payload) == nil {
			out.Team = payload.Team
		}
		return out, true
	case string(EventRunFinished):
		out.Status = "run_finished"
		var payload LifecycleEventPayload
		if json.Unmarshal(event.Payload, &payload) != nil {
			return ExecutionEvent{}, false
		}
		out.Team = payload.Team
		out.Outcome = payload.Outcome
		out.AcceptanceState = payload.AcceptanceState
		if payload.EvidenceManifest != nil {
			out.EvidenceManifestHash = payload.EvidenceManifest.ManifestHash
		}
		if payload.Telemetry != nil {
			out.DecisionChain = append([]string(nil), payload.Telemetry.DecisionChain...)
			out.RepairCost = payload.Telemetry.RepairCost
			out.RepairAttempts = payload.Telemetry.RepairCost.Attempts
			out.PlanRevision = payload.Telemetry.PlanRevision
		}
		return out, true
	case string(EventTaskPlanned):
		out.Status = "planned"
	case string(EventTaskStarted):
		out.Status = "in_progress"
	case string(EventTaskVerifying):
		out.Status = "verifying"
	case string(EventTaskCompleted):
		out.Status = "done"
	case string(EventTaskFailed), string(EventTaskBlocked), string(EventTaskProtocolIncomplete), string(EventTaskCancelled):
		out.Status = "error"
	case string(EventTaskSkipped):
		out.Status = "skipped"
	default:
		return ExecutionEvent{}, false
	}
	var payload struct {
		Agent            string     `json:"agent"`
		Attempt          int        `json:"attempt"`
		Phase            Phase      `json:"phase"`
		ContractID       string     `json:"contract_id"`
		ContractHash     string     `json:"contract_hash"`
		ContractRevision int        `json:"contract_revision"`
		Model            string     `json:"model"`
		Skills           []string   `json:"skills"`
		Outcome          RunOutcome `json:"outcome"`
		Retries          int        `json:"retries"`
	}
	if json.Unmarshal(event.Payload, &payload) == nil {
		if payload.Agent != "" {
			out.Agent = payload.Agent
		}
		if payload.Attempt > 0 {
			out.Attempt = payload.Attempt
		} else if payload.Retries >= 0 {
			out.Attempt = payload.Retries + 1
		}
		if payload.Phase != "" {
			out.Phase = payload.Phase
		}
		if payload.ContractID != "" {
			out.ContractID = payload.ContractID
		}
		if payload.ContractHash != "" {
			out.ContractHash = payload.ContractHash
		}
		if payload.ContractRevision > 0 {
			out.ContractRevision = payload.ContractRevision
		}
		if payload.Model != "" {
			out.Model = payload.Model
		}
		if len(payload.Skills) > 0 {
			out.Skills = append([]string(nil), payload.Skills...)
		}
	}
	if out.Attempt < 1 {
		out.Attempt = event.Attempt
	}
	if out.Attempt < 1 {
		out.Attempt = 1
	}
	return out, true
}

// projectedExecutionEvents is the legacy-compatible lifecycle projection.
// The event store deliberately records every durable state transition, while
// the legacy execution logger records one lifecycle event each time a task
// enters a status within an attempt. Collapse a task's repeated durable
// transitions into the status it is already in, but keep a status the task
// re-enters after another one (an approved --plan task starting again after
// planned). Attempts follow the in-dispatch counter the attempt-starting
// task_started event carries, which is the number the legacy logger records;
// logs without that counter fall back to the task's retry count.
func projectedExecutionEvents(events []RunEvent) []ExecutionEvent {
	type taskLifecycle struct {
		dispatchAttempt int
		status          string
		attempt         int
	}
	projected := make([]ExecutionEvent, 0, len(events))
	lifecycles := make(map[string]*taskLifecycle)
	for _, event := range events {
		mapped, ok := ExecutionEventFromRunEvent(event)
		if !ok {
			continue
		}
		key := mapped.RunID + "\x00" + mapped.TaskID
		lifecycle := lifecycles[key]
		if lifecycle == nil {
			lifecycle = &taskLifecycle{}
			lifecycles[key] = lifecycle
		}
		if attempt := eventDispatchAttempt(event); attempt > 0 {
			lifecycle.dispatchAttempt = attempt
		}
		if lifecycle.dispatchAttempt > 0 {
			mapped.Attempt = lifecycle.dispatchAttempt
		}
		if mapped.Status == lifecycle.status && mapped.Attempt == lifecycle.attempt {
			continue
		}
		lifecycle.status, lifecycle.attempt = mapped.Status, mapped.Attempt
		projected = append(projected, mapped)
	}
	return projected
}

// eventDispatchAttempt returns the in-dispatch attempt counter that an
// attempt-starting task_started event carries (see attemptStartMetadata), or
// 0 for any other event.
func eventDispatchAttempt(event RunEvent) int {
	if event.Type != string(EventTaskStarted) {
		return 0
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal(event.Payload, &payload) != nil {
		return 0
	}
	var attempt int
	if json.Unmarshal(payload[taskStartedDispatchAttemptKey], &attempt) != nil {
		return 0
	}
	return attempt
}

// ReadExecutionEvents reads execution events from a JSONL file.
func ReadExecutionEvents(path string) ([]ExecutionEvent, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	var events []ExecutionEvent
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var ev ExecutionEvent
		if err := json.Unmarshal([]byte(line), &ev); err == nil {
			events = append(events, ev)
		}
	}
	return events, nil
}

// ExportExecutionEvents writes a stable JSONL compatibility projection from
// canonical RunEvents. An atomic replace ensures a crash cannot leave a
// partially regenerated debug bundle behind.
func ExportExecutionEvents(workspace string, events []RunEvent) error {
	var output bytes.Buffer
	for _, mapped := range projectedExecutionEvents(events) {
		line, err := json.Marshal(mapped)
		if err != nil {
			return fmt.Errorf("marshal execution-event projection: %w", err)
		}
		output.Write(line)
		output.WriteByte('\n')
	}
	return AtomicWriteFile(filepath.Join(workspace, logsDir, eventStoreExecutionEventsFile), output.Bytes(), 0o644)
}

// ExportAndVerifyExecutionEvents exports canonical events to the shadow file
// and verifies parity against the legacy execution-events.jsonl stream for runID.
func ExportAndVerifyExecutionEvents(workspace, runID string, events []RunEvent) (bool, error) {
	if err := ExportExecutionEvents(workspace, events); err != nil {
		return false, err
	}
	legacyPath := filepath.Join(workspace, logsDir, executionEventsFile)
	legacyEvents, err := ReadExecutionEvents(legacyPath)
	if err != nil {
		return false, fmt.Errorf("read legacy execution events: %w", err)
	}
	var legacyForRun []ExecutionEvent
	for _, ev := range legacyEvents {
		if runID == "" || ev.RunID == runID {
			legacyForRun = append(legacyForRun, ev)
		}
	}
	var exportedForRun []ExecutionEvent
	for _, ee := range projectedExecutionEvents(events) {
		if runID != "" && ee.RunID != runID {
			continue
		}
		exportedForRun = append(exportedForRun, ee)
	}
	if err := CompareExecutionEventsParity(legacyForRun, exportedForRun); err != nil {
		return false, err
	}
	return true, nil
}

// CompareExecutionEventsParity compares a legacy ExecutionEvent sequence with
// an exported ExecutionEvent sequence to verify shadow compatibility.
func CompareExecutionEventsParity(legacy, exported []ExecutionEvent) error {
	if len(legacy) != len(exported) {
		return fmt.Errorf("execution events length mismatch: legacy=%d exported=%d", len(legacy), len(exported))
	}
	for i := range legacy {
		leg := legacy[i]
		exp := exported[i]
		if leg.Status != exp.Status {
			return fmt.Errorf("event %d status mismatch: legacy=%s exported=%s", i, leg.Status, exp.Status)
		}
		if leg.TaskID != exp.TaskID {
			return fmt.Errorf("event %d task_id mismatch: legacy=%s exported=%s", i, leg.TaskID, exp.TaskID)
		}
		if leg.Agent != "" && exp.Agent != "" && !strings.EqualFold(leg.Agent, exp.Agent) {
			return fmt.Errorf("event %d agent mismatch: legacy=%s exported=%s", i, leg.Agent, exp.Agent)
		}
		if leg.Attempt > 0 && exp.Attempt > 0 && leg.Attempt != exp.Attempt {
			return fmt.Errorf("event %d attempt mismatch: legacy=%d exported=%d", i, leg.Attempt, exp.Attempt)
		}
		if leg.Status == "run_finished" {
			if leg.Outcome != "" && exp.Outcome != "" && leg.Outcome != exp.Outcome {
				return fmt.Errorf("run_finished outcome mismatch: legacy=%s exported=%s", leg.Outcome, exp.Outcome)
			}
			if leg.AcceptanceState != "" && exp.AcceptanceState != "" && leg.AcceptanceState != exp.AcceptanceState {
				return fmt.Errorf("run_finished acceptance mismatch: legacy=%s exported=%s", leg.AcceptanceState, exp.AcceptanceState)
			}
			if leg.EvidenceManifestHash != "" && exp.EvidenceManifestHash != "" && leg.EvidenceManifestHash != exp.EvidenceManifestHash {
				return fmt.Errorf("run_finished evidence hash mismatch: legacy=%s exported=%s", leg.EvidenceManifestHash, exp.EvidenceManifestHash)
			}
		}
	}
	return nil
}
