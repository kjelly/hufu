package team

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/kjelly/hufu/internal/execution"
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
			out.TeamRevision = payload.TeamRevision
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
	case string(EventTaskProtocolIncomplete):
		// This is a recoverable checkpoint, not a terminal legacy error.
		// Raw canonical events retain it for inspection and crash recovery;
		// the compatibility stream records the eventual repair outcome.
		return ExecutionEvent{}, false
	case string(EventTaskFailed), string(EventTaskBlocked), string(EventTaskCancelled):
		out.Status = "error"
	case string(EventTaskSkipped):
		out.Status = "skipped"
	case string(EventExecutionFallbackDecided):
		// The failed provider attempt is real telemetry even though the
		// occurrence continues on its admitted route instead of terminalizing.
		var payload struct {
			Agent       string                    `json:"agent"`
			FromAttempt int                       `json:"from_attempt"`
			FromTarget  execution.ExecutionTarget `json:"from_target"`
		}
		if json.Unmarshal(event.Payload, &payload) != nil || payload.FromAttempt < 1 || payload.FromTarget.Validate() != nil {
			return ExecutionEvent{}, false
		}
		out.Status, out.Attempt = "error", payload.FromAttempt
		out.ExecutionTarget, out.Backend = payload.FromTarget.String(), payload.FromTarget.Backend
		out.Model = telemetryModelForTarget(payload.FromTarget)
		if payload.Agent != "" {
			out.Agent = payload.Agent
		}
		return out, true
	default:
		return ExecutionEvent{}, false
	}
	var payload struct {
		Agent            string                    `json:"agent"`
		Attempt          int                       `json:"attempt"`
		Phase            Phase                     `json:"phase"`
		ContractID       string                    `json:"contract_id"`
		ContractHash     string                    `json:"contract_hash"`
		ContractRevision int                       `json:"contract_revision"`
		Model            string                    `json:"model"`
		ExecutionTarget  execution.ExecutionTarget `json:"execution_target"`
		Skills           []string                  `json:"skills"`
		InjectedSkills   []string                  `json:"injected_skills"`
		LoadedSkills     []string                  `json:"loaded_skills"`
		Source           string                    `json:"source"`
		Outcome          RunOutcome                `json:"outcome"`
		Retries          int                       `json:"retries"`
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
		} else if !payload.ExecutionTarget.IsZero() {
			// A typed occurrence records its frozen primary target, not a
			// model string. The attempt's receipt names the model it actually
			// ran on; ExecutionEventProjector prefers that one.
			out.Model = telemetryModelForTarget(payload.ExecutionTarget)
		}
		out.TaskType = payload.Source
		out.Skills = unionSkills(payload.Skills, payload.InjectedSkills, payload.LoadedSkills)
	}
	if out.Attempt < 1 {
		out.Attempt = event.Attempt
	}
	if out.Attempt < 1 {
		out.Attempt = 1
	}
	return out, true
}

// telemetryModelForTarget names target the way execution telemetry does for
// a built-in or named LLM backend (executionModelIDForTarget): the local
// backend keeps its leaf model and every other backend its qualified
// selector. Agent backends report their leaf model there; without the
// registry this pure projection cannot tell them apart, which is why the
// attempt receipt records the resolved model.
func telemetryModelForTarget(target execution.ExecutionTarget) string {
	if execution.IsOllamaBackend(target.Backend) {
		return target.Model
	}
	return target.String()
}

// unionSkills merges declared, injected, and loaded skills in that order,
// dropping blanks and repeats, as TodoList.ExecutionMetadata does.
func unionSkills(groups ...[]string) []string {
	var skills []string
	seen := make(map[string]struct{})
	for _, group := range groups {
		for _, skill := range group {
			if skill == "" {
				continue
			}
			if _, ok := seen[skill]; ok {
				continue
			}
			seen[skill] = struct{}{}
			skills = append(skills, skill)
		}
	}
	return skills
}

// projectedExecutionEvents projects a complete event slice; see
// ExecutionEventProjector.
func projectedExecutionEvents(events []RunEvent) []ExecutionEvent {
	projector := NewExecutionEventProjector()
	for _, event := range events {
		projector.Add(event)
	}
	return projector.Rows()
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
// an exported ExecutionEvent sequence to verify shadow compatibility. Run
// boundaries retain their global positions, while task lifecycle events are
// compared in order per task. The two writers observe concurrent task
// goroutines at different points, so the global interleaving of independent
// tasks is not a stable parity signal.
func CompareExecutionEventsParity(legacy, exported []ExecutionEvent) error {
	if len(legacy) != len(exported) {
		return fmt.Errorf("execution events length mismatch: legacy=%d exported=%d", len(legacy), len(exported))
	}

	legacyPartition := partitionExecutionEvents(legacy)
	exportedPartition := partitionExecutionEvents(exported)
	if len(legacyPartition.boundaries) != len(exportedPartition.boundaries) {
		return fmt.Errorf("run boundary count mismatch: legacy=%d exported=%d", len(legacyPartition.boundaries), len(exportedPartition.boundaries))
	}
	for i := range legacyPartition.boundaries {
		leg := legacyPartition.boundaries[i]
		exp := exportedPartition.boundaries[i]
		if leg.index != exp.index {
			return fmt.Errorf("run boundary %d position mismatch: legacy=%d exported=%d", i, leg.index, exp.index)
		}
		if err := compareExecutionEvent(fmt.Sprintf("event %d", leg.index), leg.event, exp.event); err != nil {
			return err
		}
	}

	for _, key := range legacyPartition.taskOrder {
		if _, ok := exportedPartition.tasks[key]; !ok {
			return fmt.Errorf("task_id mismatch: legacy task stream run_id=%q task_id=%q has no exported counterpart", key.runID, key.taskID)
		}
	}
	for _, key := range exportedPartition.taskOrder {
		if _, ok := legacyPartition.tasks[key]; !ok {
			return fmt.Errorf("task_id mismatch: exported task stream run_id=%q task_id=%q has no legacy counterpart", key.runID, key.taskID)
		}
	}
	for _, key := range legacyPartition.taskOrder {
		legacyTaskEvents := legacyPartition.tasks[key]
		exportedTaskEvents := exportedPartition.tasks[key]
		if len(legacyTaskEvents) != len(exportedTaskEvents) {
			return fmt.Errorf("task %q event count mismatch: legacy=%d exported=%d", key.taskID, len(legacyTaskEvents), len(exportedTaskEvents))
		}
		for i := range legacyTaskEvents {
			scope := fmt.Sprintf("task %q event %d", key.taskID, i)
			if err := compareExecutionEvent(scope, legacyTaskEvents[i].event, exportedTaskEvents[i].event); err != nil {
				return err
			}
		}
		if err := compareExecutionTaskTelemetry(key, legacyTaskEvents, exportedTaskEvents); err != nil {
			return err
		}
	}
	return nil
}

// compareExecutionTaskTelemetry compares what telemetry aggregates per task:
// total token usage and the last reported model. Rows may split usage
// differently (a fallback attempt's receipt arrives with the next terminal
// transition), but the task totals must match.
func compareExecutionTaskTelemetry(key executionEventTaskKey, legacy, exported []indexedExecutionEvent) error {
	sum := func(events []indexedExecutionEvent) (usage ExecutionUsage, model string) {
		for _, indexed := range events {
			usage = addExecutionUsage(usage, indexed.event.Usage)
			if indexed.event.Model != "" {
				model = indexed.event.Model
			}
		}
		return usage, model
	}
	legacyUsage, legacyModel := sum(legacy)
	exportedUsage, exportedModel := sum(exported)
	if legacyUsage != exportedUsage {
		return fmt.Errorf("task %q usage mismatch: legacy=%+v exported=%+v", key.taskID, legacyUsage, exportedUsage)
	}
	if legacyModel != exportedModel {
		return fmt.Errorf("task %q last model mismatch: legacy=%q exported=%q", key.taskID, legacyModel, exportedModel)
	}
	return nil
}

type executionEventTaskKey struct {
	runID  string
	taskID string
}

type indexedExecutionEvent struct {
	index int
	event ExecutionEvent
}

type executionEventPartition struct {
	boundaries []indexedExecutionEvent
	tasks      map[executionEventTaskKey][]indexedExecutionEvent
	taskOrder  []executionEventTaskKey
}

func partitionExecutionEvents(events []ExecutionEvent) executionEventPartition {
	partition := executionEventPartition{tasks: make(map[executionEventTaskKey][]indexedExecutionEvent)}
	for i, event := range events {
		indexed := indexedExecutionEvent{index: i, event: event}
		if event.TaskID == "" {
			partition.boundaries = append(partition.boundaries, indexed)
			continue
		}
		key := executionEventTaskKey{runID: event.RunID, taskID: event.TaskID}
		if _, ok := partition.tasks[key]; !ok {
			partition.taskOrder = append(partition.taskOrder, key)
		}
		partition.tasks[key] = append(partition.tasks[key], indexed)
	}
	return partition
}

func compareExecutionEvent(scope string, legacy, exported ExecutionEvent) error {
	if legacy.Status != exported.Status {
		return fmt.Errorf("%s status mismatch: legacy=%s exported=%s", scope, legacy.Status, exported.Status)
	}
	if legacy.TaskID != exported.TaskID {
		return fmt.Errorf("%s task_id mismatch: legacy=%s exported=%s", scope, legacy.TaskID, exported.TaskID)
	}
	if legacy.Agent != "" && exported.Agent != "" && !strings.EqualFold(legacy.Agent, exported.Agent) {
		return fmt.Errorf("%s agent mismatch: legacy=%s exported=%s", scope, legacy.Agent, exported.Agent)
	}
	if legacy.Attempt > 0 && exported.Attempt > 0 && legacy.Attempt != exported.Attempt {
		return fmt.Errorf("%s attempt mismatch: legacy=%d exported=%d", scope, legacy.Attempt, exported.Attempt)
	}
	if legacy.Team != exported.Team {
		return fmt.Errorf("%s team mismatch: legacy=%q exported=%q", scope, legacy.Team, exported.Team)
	}
	if legacy.TeamRevision != exported.TeamRevision {
		return fmt.Errorf("%s team_revision mismatch: legacy=%q exported=%q", scope, legacy.TeamRevision, exported.TeamRevision)
	}
	if legacy.TaskType != exported.TaskType {
		return fmt.Errorf("%s task_type mismatch: legacy=%q exported=%q", scope, legacy.TaskType, exported.TaskType)
	}
	if !slices.Equal(legacy.Skills, exported.Skills) {
		return fmt.Errorf("%s skills mismatch: legacy=%v exported=%v", scope, legacy.Skills, exported.Skills)
	}
	if legacy.Status != "run_finished" {
		return nil
	}
	if legacy.Outcome != "" && exported.Outcome != "" && legacy.Outcome != exported.Outcome {
		return fmt.Errorf("run_finished outcome mismatch: legacy=%s exported=%s", legacy.Outcome, exported.Outcome)
	}
	if legacy.AcceptanceState != "" && exported.AcceptanceState != "" && legacy.AcceptanceState != exported.AcceptanceState {
		return fmt.Errorf("run_finished acceptance mismatch: legacy=%s exported=%s", legacy.AcceptanceState, exported.AcceptanceState)
	}
	if legacy.EvidenceManifestHash != "" && exported.EvidenceManifestHash != "" && legacy.EvidenceManifestHash != exported.EvidenceManifestHash {
		return fmt.Errorf("run_finished evidence hash mismatch: legacy=%s exported=%s", legacy.EvidenceManifestHash, exported.EvidenceManifestHash)
	}
	return nil
}
