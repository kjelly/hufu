package team

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/kjelly/hufu/internal/execution"
)

// ExecutionIdentityConflictError means durable execution identity evidence
// disagrees. Callers must not resume or dispatch the affected task.
type ExecutionIdentityConflictError struct {
	TaskID  string
	EventID string
	Reason  string
}

func (e *ExecutionIdentityConflictError) Error() string {
	if e.EventID != "" {
		return fmt.Sprintf("execution identity conflict for task %q at event %q: %s", e.TaskID, e.EventID, e.Reason)
	}
	return fmt.Sprintf("execution identity conflict for task %q: %s", e.TaskID, e.Reason)
}

type executionIdentityEventPayload struct {
	ID                string                       `json:"id"`
	Model             string                       `json:"model"`
	ModelTopology     []string                     `json:"model_topology"`
	SubagentProvider  string                       `json:"subagent_provider"`
	ProviderBinding   *ProviderBinding             `json:"provider_binding"`
	ExecutionTarget   *execution.ExecutionTarget   `json:"execution_target"`
	ExecutionTopology *[]execution.ExecutionTarget `json:"execution_topology"`
	BackendBinding    *BackendBinding              `json:"backend_binding"`
}

// ReplayTodoList reconstructs durable tasks only after every canonical target
// agrees with its legacy dual-write fields and frozen topology. The historic
// no-error ReduceToTodoList helper remains available for narrow projections.
func ReplayTodoList(events []RunEvent) ([]*TodoItem, error) {
	if err := validateReplayExecutionIdentities(events); err != nil {
		return nil, err
	}
	replay := reduceToTodoList(events)
	if err := todoReplayWorksetConflictError(replay.worksetConflicts); err != nil {
		return nil, err
	}
	for _, item := range replay.tasks {
		if item == nil {
			continue
		}
		if err := validateExecutionIdentity(item.ExecutionTarget, item.ExecutionTopology, item.BackendBinding); err != nil {
			return nil, &ExecutionIdentityConflictError{TaskID: item.ID, Reason: err.Error()}
		}
	}
	return replay.tasks, nil
}

// ReadCheckedActiveExecutionTaskEvidence reads the active branch's durable
// event lineage without creating or mutating any workspace files, validates
// the event-store hash chain, and replays its task projection with the checked
// execution-identity reducer. The returned lineage is the evidence used by
// legacy-target preflight to recover a historical provider identity without
// consulting current configuration.
//
// Startup resume preflight uses this alongside session.json because a crash
// can append a task transition before its checkpoint is written. A missing
// event store is a valid legacy/first-run state and returns no tasks/evidence.
// Any existing but malformed or conflicting durable evidence fails closed so
// runtime initialization cannot proceed from an unverified target.
func ReadCheckedActiveExecutionTaskEvidence(workspace string) ([]*TodoItem, []RunEvent, error) {
	workspace = strings.TrimSpace(workspace)
	if workspace == "" {
		return nil, nil, nil
	}
	path := filepath.Join(workspace, logsDir, eventStoreFile)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil, nil
		}
		return nil, nil, fmt.Errorf("open event store read-only: %w", err)
	}
	defer func() { _ = f.Close() }()

	reader := &EventStore{}
	state, err := reader.scanFile(f)
	if err != nil {
		return nil, nil, fmt.Errorf("validate event store: %w", err)
	}
	if len(state.events) == 0 {
		return nil, nil, nil
	}
	tree, err := LoadSessionTree(workspace)
	if err != nil {
		return nil, nil, fmt.Errorf("load session tree for event replay: %w", err)
	}
	activeBranch := "main"
	if tree != nil && strings.TrimSpace(tree.ActiveBranch) != "" {
		activeBranch = tree.ActiveBranch
	}
	lineage, err := projectEventsForBranch(state.events, tree, activeBranch)
	if err != nil {
		return nil, nil, fmt.Errorf("project active event lineage: %w", err)
	}
	tasks, err := ReplayTodoList(lineage)
	if err != nil {
		return nil, nil, fmt.Errorf("replay active event lineage: %w", err)
	}
	return tasks, lineage, nil
}

// ReadCheckedActiveExecutionTasks is the task-only compatibility wrapper for
// callers that do not need historical evidence.
func ReadCheckedActiveExecutionTasks(workspace string) ([]*TodoItem, error) {
	tasks, _, err := ReadCheckedActiveExecutionTaskEvidence(workspace)
	return tasks, err
}

func validateReplayExecutionIdentities(events []RunEvent) error {
	seenTargets := make(map[string]execution.ExecutionTarget)
	seenTopologies := make(map[string][]execution.ExecutionTarget)
	migrationExpectations := make(map[string]legacyExecutionMigrationExpectation)
	normalizedEvents := normalizeReplayEvents(events)
	for index, event := range normalizedEvents {
		if event.Type == string(EventExecutionTargetMigrated) && event.TaskID != "" {
			if err := validateReplayExecutionTargetMigration(event, normalizedEvents[:index], migrationExpectations, seenTargets, seenTopologies); err != nil {
				return err
			}
			continue
		}
		if !strings.HasPrefix(event.Type, "task_") {
			continue
		}
		var payload executionIdentityEventPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			continue // Existing replay semantics tolerate unrelated malformed payloads.
		}
		taskID := event.TaskID
		if taskID == "" {
			taskID = payload.ID
		}
		if taskID == "" {
			continue
		}
		if payload.ExecutionTarget == nil || payload.ExecutionTarget.IsZero() {
			continue // legacy-only event
		}
		target := *payload.ExecutionTarget
		if err := target.Validate(); err != nil {
			return executionIdentityConflict(event, taskID, err.Error())
		}
		// A canonical event may intentionally omit the legacy provider marker.
		// In that form a bare model is ambiguous (for example, it can be a
		// Codex model or an LLM model), so only compare legacy identity when the
		// provider marker is present. Historical dual-write events include both.
		if payload.SubagentProvider != "" && payload.Model != "" {
			legacy := targetFromLegacyIdentity(payload.Model, payload.SubagentProvider)
			if legacy != target {
				return executionIdentityConflict(event, taskID, "legacy model/provider fields disagree with execution target")
			}
		}
		if payload.ExecutionTopology == nil || len(*payload.ExecutionTopology) == 0 {
			return executionIdentityConflict(event, taskID, "execution target is missing a frozen topology")
		}
		topology := *payload.ExecutionTopology
		if err := validateExecutionIdentity(target, topology, payload.BackendBinding); err != nil {
			return executionIdentityConflict(event, taskID, err.Error())
		}
		if payload.SubagentProvider != "" && payload.ModelTopology != nil {
			legacyTopology := topologyFromLegacyIdentity(payload.ModelTopology, target, payload.SubagentProvider)
			if !slices.Equal(legacyTopology, topology) {
				return executionIdentityConflict(event, taskID, "legacy model topology disagrees with execution topology")
			}
		}
		if payload.ProviderBinding != nil && payload.BackendBinding != nil && !equivalentBackendBinding(payload.ProviderBinding, payload.BackendBinding, target) {
			return executionIdentityConflict(event, taskID, "provider binding disagrees with backend binding")
		}
		if previous, ok := seenTargets[taskID]; ok && previous != target {
			return executionIdentityConflict(event, taskID, "execution target changed after task creation")
		}
		seenTargets[taskID] = target
		if previous, ok := seenTopologies[taskID]; ok && !slices.Equal(previous, topology) {
			return executionIdentityConflict(event, taskID, "execution topology changed after task creation")
		}
		seenTopologies[taskID] = cloneExecutionTopology(topology)
	}
	return validateReplayBackendSessionBindings(events, seenTargets)
}

func validateReplayExecutionTargetMigration(event RunEvent, preceding []RunEvent, expectations map[string]legacyExecutionMigrationExpectation, seenTargets map[string]execution.ExecutionTarget, seenTopologies map[string][]execution.ExecutionTarget) error {
	var payload ExecutionTargetMigratedPayload
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return nil
	}
	if payload.TaskID != "" && payload.TaskID != event.TaskID {
		return executionIdentityConflict(event, event.TaskID, "execution target migration task identity disagrees with event task")
	}
	if payload.MigrationVersion != executionTargetMigrationVersion {
		return executionIdentityConflict(event, event.TaskID, "unsupported execution target migration version")
	}
	eventBranch := effectiveEventBranchID(event)
	migrationBranch := strings.TrimSpace(payload.BranchID)
	if migrationBranch == "" {
		// Pre-branch migration payloads did not carry a branch field; their event
		// lineage still supplies the historical branch identity for replay.
		migrationBranch = eventBranch
	}
	if migrationBranch != eventBranch {
		return executionIdentityConflict(event, event.TaskID, "execution target migration branch disagrees with event branch")
	}
	if err := payload.ExecutionTarget.Validate(); err != nil {
		return executionIdentityConflict(event, event.TaskID, err.Error())
	}
	topology := payload.ExecutionTopology
	if len(topology) == 0 {
		// Backward-compatible decode for migration version 1 events written
		// before complete topology persistence.
		topology = []execution.ExecutionTarget{payload.ExecutionTarget}
	}
	if err := validateExecutionIdentity(payload.ExecutionTarget, topology, nil); err != nil {
		return executionIdentityConflict(event, event.TaskID, err.Error())
	}
	expectation, ok := expectations[event.TaskID]
	if !ok {
		var err error
		expectation, err = deriveLegacyExecutionMigrationExpectation(preceding, event.TaskID)
		if err != nil {
			return executionIdentityConflict(event, event.TaskID, err.Error())
		}
		expectations[event.TaskID] = expectation
	}
	if err := validateLegacyExecutionMigrationPayload(payload, expectation); err != nil {
		return executionIdentityConflict(event, event.TaskID, err.Error())
	}
	if previous, ok := seenTargets[event.TaskID]; ok && previous != payload.ExecutionTarget {
		return executionIdentityConflict(event, event.TaskID, "repeated execution target migrations disagree")
	}
	seenTargets[event.TaskID] = payload.ExecutionTarget
	if previous, ok := seenTopologies[event.TaskID]; ok && !slices.Equal(previous, topology) {
		return executionIdentityConflict(event, event.TaskID, "repeated execution target migrations disagree on topology")
	}
	seenTopologies[event.TaskID] = cloneExecutionTopology(topology)
	return nil
}

type legacyExecutionMigrationExpectation struct {
	legacyModel            string
	legacySubagentProvider string
	target                 execution.ExecutionTarget
	topology               []execution.ExecutionTarget
	evidenceEventIDs       []string
}

// deriveLegacyExecutionMigrationExpectation reconstructs the target that the
// migration boundary was authorized to freeze. It intentionally consumes only
// the preceding checked lineage, never the migration event itself or live
// configuration. A migration without a preceding target-less occurrence is
// malformed durable evidence, not a new way to introduce a target.
func deriveLegacyExecutionMigrationExpectation(events []RunEvent, taskID string) (legacyExecutionMigrationExpectation, error) {
	var expectation legacyExecutionMigrationExpectation
	for _, item := range reduceToTodoList(events).tasks {
		if item == nil || item.ID != taskID {
			continue
		}
		if !item.ExecutionTarget.IsZero() {
			return expectation, fmt.Errorf("execution target migration follows a canonical occurrence")
		}
		topology, evidenceEventIDs, err := resolveLegacyExecutionTopology(item, events)
		if err != nil {
			return expectation, err
		}
		if len(topology) == 0 {
			return expectation, fmt.Errorf("legacy execution target migration has no executable topology")
		}
		return legacyExecutionMigrationExpectation{
			legacyModel:            item.Model,
			legacySubagentProvider: item.SubagentProvider,
			target:                 topology[0],
			topology:               cloneExecutionTopology(topology),
			evidenceEventIDs:       slices.Clone(evidenceEventIDs),
		}, nil
	}
	return expectation, fmt.Errorf("execution target migration has no preceding legacy occurrence")
}

func validateLegacyExecutionMigrationPayload(payload ExecutionTargetMigratedPayload, expectation legacyExecutionMigrationExpectation) error {
	if payload.LegacyModel != "" && strings.TrimSpace(payload.LegacyModel) != strings.TrimSpace(expectation.legacyModel) {
		return fmt.Errorf("legacy migration model disagrees with preceding occurrence")
	}
	if payload.LegacySubagentProvider != "" && strings.TrimSpace(payload.LegacySubagentProvider) != strings.TrimSpace(expectation.legacySubagentProvider) {
		return fmt.Errorf("legacy migration provider disagrees with preceding occurrence")
	}
	if payload.ExecutionTarget != expectation.target {
		return fmt.Errorf("migrated execution target %q disagrees with preceding legacy occurrence target %q", payload.ExecutionTarget, expectation.target)
	}
	if len(payload.ExecutionTopology) > 0 && !slices.Equal(payload.ExecutionTopology, expectation.topology) {
		return fmt.Errorf("migrated execution topology disagrees with preceding legacy occurrence")
	}
	if !slices.Equal(payload.EvidenceEventIDs, expectation.evidenceEventIDs) {
		return fmt.Errorf("migrated execution evidence does not match the preceding legacy lineage")
	}
	return nil
}

// validateReplayBackendSessionBindings keeps the legacy and canonical session
// event names dual-readable while rejecting incompatible dual-write evidence.
func validateReplayBackendSessionBindings(events []RunEvent, targets map[string]execution.ExecutionTarget) error {
	type observedBinding struct {
		event   RunEvent
		binding BackendBinding
	}
	seen := make(map[string]observedBinding)
	for _, event := range normalizeReplayEvents(events) {
		if event.TaskID == "" || (event.Type != string(EventProviderSessionBound) && event.Type != string(EventBackendSessionBound)) {
			continue
		}
		target, ok := targets[event.TaskID]
		if !ok {
			continue // legacy-only tasks remain inspectable until migrated.
		}
		var binding BackendBinding
		switch event.Type {
		case string(EventProviderSessionBound):
			var payload ProviderSessionBoundPayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				continue
			}
			binding = *backendBindingFromProviderBinding(&ProviderBinding{
				Provider: payload.Provider, Protocol: payload.Protocol, SessionID: payload.SessionID,
				ExecutionWorldID: payload.ExecutionWorldID, CWD: payload.CWD,
			}, target)
		case string(EventBackendSessionBound):
			var payload BackendSessionBoundPayload
			if err := json.Unmarshal(event.Payload, &payload); err != nil {
				continue
			}
			if !payload.ExecutionTarget.IsZero() && payload.ExecutionTarget != target {
				return executionIdentityConflict(event, event.TaskID, "backend session target disagrees with task execution target")
			}
			binding = BackendBinding{Backend: payload.Backend, SessionID: payload.SessionID, ExecutionWorldID: payload.ExecutionWorldID, CWD: payload.CWD}
		}
		if execution.CanonicalBackendName(binding.Backend) != target.Backend {
			return executionIdentityConflict(event, event.TaskID, "backend session binding disagrees with task execution target")
		}
		key := fmt.Sprintf("%s:%d:%s", event.TaskID, event.Attempt, binding.SessionID)
		if previous, exists := seen[key]; exists && previous.binding != binding {
			return executionIdentityConflict(event, event.TaskID, "legacy and canonical backend session events disagree")
		}
		seen[key] = observedBinding{event: event, binding: binding}
	}
	return nil
}

func executionIdentityConflict(event RunEvent, taskID, reason string) error {
	return &ExecutionIdentityConflictError{TaskID: taskID, EventID: event.ID, Reason: reason}
}

func equivalentBackendBinding(provider *ProviderBinding, backend *BackendBinding, target execution.ExecutionTarget) bool {
	if provider == nil || backend == nil {
		return provider == nil && backend == nil
	}
	expected := backendBindingFromProviderBinding(provider, target)
	return expected.Backend == execution.CanonicalBackendName(backend.Backend) &&
		expected.SessionID == backend.SessionID &&
		expected.TurnID == backend.TurnID &&
		expected.BackendVersion == backend.BackendVersion &&
		expected.EffectiveTarget == backend.EffectiveTarget &&
		expected.ExecutionWorldID == backend.ExecutionWorldID &&
		expected.CWD == backend.CWD &&
		expected.SandboxMode == backend.SandboxMode &&
		expected.ResumeSupported == backend.ResumeSupported
}
