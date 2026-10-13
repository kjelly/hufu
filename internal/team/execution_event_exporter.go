package team

import (
	"encoding/json"

	"github.com/kjelly/hufu/internal/execution"
)

// ExecutionEventFromRunEvent maps one canonical event-store record into the
// execution telemetry schema; ExecutionEventProjector assembles the stream.
// It intentionally omits transcript and raw output fields because EventStore
// holds only redacted metadata.
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
