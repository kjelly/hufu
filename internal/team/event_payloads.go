package team

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/kjelly/hufu/internal/executioncompat"
)

// Event payloads intentionally contain references and redacted summaries,
// never raw transcripts or canonical context content. The structs only model
// fields required to validate terminal/reducer-consumed records; legacy v1
// records continue through the compatibility path below.
type SessionMessageEventPayload struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type TaskTransitionEventPayload struct {
	ID     string `json:"id"`
	Status string `json:"status"`
}

type RunFinishedEventPayload struct {
	Outcome              RunOutcome                   `json:"outcome"`
	GoalSatisfied        bool                         `json:"goal_satisfied,omitempty"`
	GoalMode             GoalMode                     `json:"goal_mode,omitempty"`
	Response             string                       `json:"response,omitempty"`
	Reason               string                       `json:"reason,omitempty"`
	StopReason           StopReason                   `json:"stop_reason,omitempty"`
	ExitCode             int                          `json:"exit_code,omitempty"`
	UnresolvedTasks      []TaskReference              `json:"unresolved_tasks,omitempty"`
	Acceptance           *AcceptanceResult            `json:"acceptance,omitempty"`
	Stats                *RunStats                    `json:"stats,omitempty"`
	Metrics              *RunMetrics                  `json:"metrics,omitempty"`
	RunInputs            *RunInputSnapshot            `json:"run_inputs,omitempty"`
	InputBoundAssertions []InputBoundAssertionSummary `json:"input_bound_assertions,omitempty"`
}

// RunCancellationRequestedPayload records the exact runtime boundary that
// changed a graceful wrap-up into context cancellation. It intentionally
// contains no task or model output.
type RunCancellationRequestedPayload struct {
	RunID                 string `json:"run_id"`
	BranchID              string `json:"branch_id"`
	RequestedAt           string `json:"requested_at"`
	Status                string `json:"status"`
	ReasonCode            string `json:"reason_code"`
	Source                string `json:"source"`
	GracefulTimeoutMillis int64  `json:"graceful_timeout_millis,omitzero"`
}

// ValidateEventPayload validates an event after EventStore has filled its
// identity and redacted its payload, but before it becomes durable. Schema v1
// is deliberately accepted as a read/replay compatibility format. Unknown
// event types are accepted so a newer writer cannot make an older reader lose
// an otherwise valid hash chain.
func ValidateEventPayload(event RunEvent) error {
	if event.SchemaVersion <= 0 {
		return fmt.Errorf("event schema version must be positive")
	}
	if strings.TrimSpace(event.Type) == "" {
		return fmt.Errorf("event type is empty")
	}
	// Schema v1 intentionally allowed sparse events. Preserve those old
	// workspaces (and their hash chains); v2 is the current event-first
	// contract and requires complete durable identity.
	if event.SchemaVersion >= eventStoreSchemaVersion && (strings.TrimSpace(event.ID) == "" || strings.TrimSpace(event.RunID) == "" || strings.TrimSpace(event.SessionID) == "" || strings.TrimSpace(event.Actor) == "" || strings.TrimSpace(event.Timestamp) == "") {
		return fmt.Errorf("event identity is incomplete")
	}
	if len(event.Payload) > 0 && !json.Valid(event.Payload) {
		return fmt.Errorf("event %q payload is not valid JSON", event.Type)
	}
	if IsTerminalEvent(event.Type) && IsEmptyPayload(event.Payload) {
		return fmt.Errorf("terminal event %q has empty payload", event.Type)
	}
	if event.SchemaVersion == eventStoreLegacySchemaVersion {
		return nil
	}

	switch EventType(event.Type) {
	case EventWrapUpPhase, EventRunCancellationRequested:
		return validateRunLifecycleControlPayload(event)
	case EventUserMessageAdded, EventAssistantMessageAdded:
		var payload SessionMessageEventPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return fmt.Errorf("decode %s payload: %w", event.Type, err)
		}
		if strings.TrimSpace(payload.Content) == "" {
			return fmt.Errorf("%s payload has empty content", event.Type)
		}
	case EventTaskCreated, EventTaskPlanned, EventTaskStarted, EventTaskVerifying, EventTaskPaused, EventTaskCompleted, EventTaskFailed, EventTaskBlocked, EventTaskSkipped, EventTaskProtocolIncomplete, EventTaskCancelled, EventTaskRemoved, EventTaskResolution:
		if strings.TrimSpace(event.TaskID) == "" {
			return fmt.Errorf("%s event has empty task id", event.Type)
		}
		var payload TaskTransitionEventPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return fmt.Errorf("decode %s payload: %w", event.Type, err)
		}
		// Old task_started records did not always carry id/status. Keep schema
		// v1 replayable while enforcing the complete v2 transition contract.
		if strings.TrimSpace(payload.ID) == "" || strings.TrimSpace(payload.Status) == "" {
			return fmt.Errorf("%s payload lacks task transition identity", event.Type)
		}
	case EventRunFinished:
		return validateRunFinishedEventPayload(event)
	case EventRunInputsResolved:
		var snapshot RunInputSnapshot
		decoder := json.NewDecoder(bytes.NewReader(event.Payload))
		decoder.DisallowUnknownFields()
		if err := decoder.Decode(&snapshot); err != nil {
			return fmt.Errorf("decode run_inputs_resolved payload: %w", err)
		}
		var trailing any
		if err := decoder.Decode(&trailing); err == nil {
			return fmt.Errorf("decode run_inputs_resolved payload: trailing JSON value")
		} else if err != io.EOF {
			return fmt.Errorf("decode run_inputs_resolved trailing JSON: %w", err)
		}
		if err := ValidateRunInputSnapshot(&snapshot); err != nil {
			return fmt.Errorf("invalid run_inputs_resolved payload: %w", err)
		}
		if snapshot.RunID != event.RunID {
			return fmt.Errorf("run_inputs_resolved payload run_id does not match event envelope")
		}
	case EventExecutionCompatibilityObserved:
		var payload ExecutionCompatibilityObservedPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return fmt.Errorf("decode execution compatibility observation payload: %w", err)
		}
		if err := validateExecutionCompatibilityObservedPayload(payload); err != nil {
			return err
		}
	case EventDecisionRunOpened, EventDecisionRunAttached,
		EventPrimaryDecisionPrepared, EventPrimaryDecisionAdmitted,
		EventDecisionRoleCallStarted, EventDecisionRoleCallUnconfirmed,
		EventDecisionRoleCallSettled, EventPrimaryDecisionBlocked,
		EventPrimaryDecisionBound, EventPrimaryDecisionInvalidated:
		return validateDecisionCorrectnessEvent(event)
	}
	return nil
}

func validateRunLifecycleControlPayload(event RunEvent) error {
	switch EventType(event.Type) {
	case EventWrapUpPhase:
		var payload PendingWrapUp
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return fmt.Errorf("decode wrap_up_phase payload: %w", err)
		}
		if strings.TrimSpace(payload.RunID) == "" || payload.RunID != event.RunID || strings.TrimSpace(payload.BranchID) == "" || strings.TrimSpace(payload.RequestedAt) == "" {
			return fmt.Errorf("wrap_up_phase payload has invalid run binding")
		}
	case EventRunCancellationRequested:
		var payload RunCancellationRequestedPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			return fmt.Errorf("decode run_cancellation_requested payload: %w", err)
		}
		if strings.TrimSpace(payload.RunID) == "" || payload.RunID != event.RunID ||
			strings.TrimSpace(payload.BranchID) == "" || strings.TrimSpace(payload.RequestedAt) == "" ||
			payload.Status != "cancellation_requested" || strings.TrimSpace(payload.ReasonCode) == "" ||
			strings.TrimSpace(payload.Source) == "" || payload.GracefulTimeoutMillis < 0 {
			return fmt.Errorf("run_cancellation_requested payload is invalid")
		}
	}
	return nil
}

func validateRunFinishedEventPayload(event RunEvent) error {
	var payload RunFinishedEventPayload
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return fmt.Errorf("decode run_finished payload: %w", err)
	}
	if payload.Outcome == "" {
		return fmt.Errorf("run_finished payload lacks outcome")
	}
	if payload.RunInputs != nil {
		if err := ValidateRunInputSnapshot(payload.RunInputs); err != nil {
			return fmt.Errorf("run_finished payload has invalid run inputs: %w", err)
		}
		if payload.RunInputs.RunID != event.RunID {
			return fmt.Errorf("run_finished run input snapshot does not match event run")
		}
	}
	if len(payload.InputBoundAssertions) > maxRunInputEvidenceItems {
		return fmt.Errorf("run_finished payload has too many input-bound assertions")
	}
	if len(payload.InputBoundAssertions) > 0 && payload.RunInputs == nil {
		return fmt.Errorf("run_finished payload has input-bound assertions without run inputs")
	}
	for index, assertion := range payload.InputBoundAssertions {
		if strings.TrimSpace(assertion.Criterion) == "" || strings.TrimSpace(assertion.SourceTask) == "" || strings.TrimSpace(assertion.Output) == "" || len(assertion.Assertion) > maxRunInputResolverDiagnosticBytes {
			return fmt.Errorf("run_finished input-bound assertion %d is invalid", index)
		}
		if assertion.State != "passed" && assertion.State != "failed" {
			return fmt.Errorf("run_finished input-bound assertion %d has invalid state %q", index, assertion.State)
		}
		if len(assertion.Evidence) > maxRunInputEvidenceItems {
			return fmt.Errorf("run_finished input-bound assertion %d has too much evidence", index)
		}
	}
	return nil
}

func validateExecutionCompatibilityObservedPayload(payload ExecutionCompatibilityObservedPayload) error {
	if payload.SchemaVersion != executionCompatibilityObservationSchemaVersion {
		return fmt.Errorf("execution compatibility observation has unsupported schema version %d", payload.SchemaVersion)
	}
	if len(payload.Counts) == 0 {
		return fmt.Errorf("execution compatibility observation has no feature counts")
	}
	for feature, count := range payload.Counts {
		if !isExecutionCompatibilityFeature(feature) {
			return fmt.Errorf("execution compatibility observation has unknown feature %q", feature)
		}
		if count <= 0 {
			return fmt.Errorf("execution compatibility observation feature %q has non-positive count", feature)
		}
	}
	return nil
}

func isExecutionCompatibilityFeature(feature executioncompat.Feature) bool {
	switch feature {
	case executioncompat.FeatureLocalAlias,
		executioncompat.FeatureProviderShadowFields,
		executioncompat.FeatureLegacyProviderBinding,
		executioncompat.FeatureProviderSessionEvent,
		executioncompat.FeatureLegacyReceiptProvider,
		executioncompat.FeatureLegacyPolicyRoute,
		executioncompat.FeatureLegacyResumeMigration:
		return true
	default:
		return false
	}
}
