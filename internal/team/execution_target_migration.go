package team

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/kjelly/hufu/internal/execution"
	"github.com/kjelly/hufu/internal/modelprofile"
)

const executionTargetMigrationVersion = 1

// LegacyExecutionTargetAmbiguousError blocks resume rather than guessing a
// historical qualified provider identity from current configuration.
type LegacyExecutionTargetAmbiguousError struct {
	TaskID string
	Model  string
}

func (e *LegacyExecutionTargetAmbiguousError) Error() string {
	return fmt.Sprintf("legacy execution target ambiguous for task %q model %q", e.TaskID, e.Model)
}

type ExecutionTargetMigratedPayload struct {
	TaskID                 string                    `json:"task_id"`
	LegacyModel            string                    `json:"legacy_model"`
	LegacySubagentProvider string                    `json:"legacy_subagent_provider"`
	ExecutionTarget        execution.ExecutionTarget `json:"execution_target"`
	// ExecutionTopology is optional for migration version 1 so older durable
	// migration events remain replayable. New migrations always persist the
	// complete ordered topology, including every legacy ModelTopology leaf.
	ExecutionTopology []execution.ExecutionTarget `json:"execution_topology,omitempty"`
	MigrationVersion  int                         `json:"migration_version"`
	EvidenceEventIDs  []string                    `json:"evidence_event_ids"`
	// BranchID scopes the migration occurrence. Event-store idempotency keys
	// are global, while task IDs are inherited across forks, so a migration on
	// one branch must never satisfy the append on another branch.
	BranchID string `json:"branch_id,omitempty"`
}

func migratableLegacyExecutionTarget(item *TodoItem) (execution.ExecutionTarget, error) {
	if item == nil {
		return execution.ExecutionTarget{}, fmt.Errorf("legacy execution target requires task")
	}
	provider := strings.TrimSpace(item.SubagentProvider)
	if provider != "" && provider != localSubagentProviderName {
		return targetFromLegacyIdentity(item.Model, provider), nil
	}
	// A durable provider/backend session binding is stronger historical
	// evidence than the legacy hufu-local marker. It was appended in the task
	// lineage at execution time, so it may safely disambiguate a qualified
	// legacy model without consulting current configuration.
	if item.BackendBinding != nil && execution.CanonicalBackendName(item.BackendBinding.Backend) != localSubagentProviderName {
		return targetFromLegacyProviderEvidence(item.Model, item.BackendBinding.Backend), nil
	}
	if item.ProviderBinding != nil && execution.CanonicalBackendName(item.ProviderBinding.Provider) != localSubagentProviderName {
		return targetFromLegacyProviderEvidence(item.Model, item.ProviderBinding.Provider), nil
	}
	selector, err := execution.ParseExecutionSelector(item.Model)
	if err != nil {
		return execution.ExecutionTarget{}, err
	}
	if selector.Backend != "" && !execution.IsOllamaBackend(selector.Backend) {
		return execution.ExecutionTarget{}, &LegacyExecutionTargetAmbiguousError{TaskID: item.ID, Model: item.Model}
	}
	return targetFromLegacyIdentity(item.Model, provider), nil
}

// targetFromHistoricalProfileEvidence recovers a legacy qualified target only
// when the task's own historical run recorded exactly one matching
// model_profile_resolved provider. Current configuration is deliberately not
// an input: a resumed task must not be retargeted by today's backend choices.
func targetFromHistoricalProfileEvidence(item *TodoItem, events []RunEvent) (execution.ExecutionTarget, []string, bool) {
	if item == nil {
		return execution.ExecutionTarget{}, nil, false
	}
	var taskRunID string
	for _, event := range events {
		if event.TaskID == item.ID && event.RunID != "" {
			taskRunID = event.RunID
			break
		}
	}
	if taskRunID == "" {
		return execution.ExecutionTarget{}, nil, false
	}
	model := strings.TrimSpace(item.Model)
	if selector, err := execution.ParseExecutionSelector(model); err == nil && selector.Backend != "" {
		model = selector.Model
	}
	var recovered execution.ExecutionTarget
	evidenceIDs := make([]string, 0, 1)
	for _, event := range events {
		if event.Type != string(EventModelProfileResolved) || event.RunID != taskRunID {
			continue
		}
		var profile modelprofile.TelemetryProjection
		if json.Unmarshal(event.Payload, &profile) != nil || strings.TrimSpace(profile.ModelID) != model {
			continue
		}
		provider := execution.CanonicalTargetBackendName(profile.Provider)
		if provider == "" || provider == localSubagentProviderName {
			continue
		}
		candidate := targetFromLegacyProviderEvidence(item.Model, provider)
		if !recovered.IsZero() && recovered != candidate {
			return execution.ExecutionTarget{}, nil, false
		}
		recovered = candidate
		if event.ID != "" {
			evidenceIDs = append(evidenceIDs, event.ID)
		}
	}
	return recovered, evidenceIDs, !recovered.IsZero()
}

func targetFromLegacyProviderEvidence(model, provider string) execution.ExecutionTarget {
	provider = execution.CanonicalTargetBackendName(provider)
	model = strings.TrimSpace(model)
	if selector, err := execution.ParseExecutionSelector(model); err == nil && selector.Backend != "" {
		model = selector.Model
	}
	return execution.ExecutionTarget{Backend: provider, Model: model}
}

// RestoredExecutionTargetsForPreflight derives the immutable execution
// targets that a resumable task would use, without consulting live
// configuration or mutating the task/event store. Canonical occurrences use
// their typed target/topology directly. Legacy occurrences are resolved from
// their durable fields and, only when a qualified selector is ambiguous, the
// task's own historical model-profile evidence.
//
// The coordinator still appends EventExecutionTargetMigrated at its existing
// just-before-dispatch boundary. This function is intentionally a read-only
// admission plan so startup can reject an unavailable or ambiguous backend
// before workspace/session lifecycle mutation.
func RestoredExecutionTargetsForPreflight(item *TodoItem, events []RunEvent) ([]execution.ExecutionTarget, error) {
	if item == nil {
		return nil, nil
	}

	if item.ExecutionTarget.IsZero() {
		targets, _, err := resolveLegacyExecutionTopology(item, events)
		if err != nil {
			return nil, err
		}
		return targets, nil
	}
	primary := item.ExecutionTarget
	if err := primary.Validate(); err != nil {
		return nil, err
	}

	targets := []execution.ExecutionTarget{primary}
	if len(item.ExecutionTopology) > 0 {
		targets = slices.Clone(item.ExecutionTopology)
		if err := validateExecutionIdentity(primary, targets, item.BackendBinding); err != nil {
			return nil, err
		}
		return validateRestoredExecutionTargets(targets)
	}
	if len(item.ModelTopology) == 0 {
		return targets, nil
	}

	// Legacy topology leaves carry the same durable provider/binding evidence
	// as the primary occurrence. Resolve each leaf independently so a
	// qualified legacy selector cannot silently become a local target merely
	// because the current runtime defaults to local. The migration and
	// preflight paths share this resolver so the read-only plan is exactly what
	// the dispatch-boundary freeze will persist.
	legacyTargets, _, err := resolveLegacyExecutionTopology(item, events)
	if err != nil {
		return nil, err
	}
	return validateRestoredExecutionTargets(legacyTargets)
}

// resolveLegacyExecutionTopology resolves the primary legacy target and every
// ordered ModelTopology leaf. It returns the target list that migration must
// freeze plus the historical profile event IDs used to disambiguate any
// qualified legacy selectors.
func resolveLegacyExecutionTopology(item *TodoItem, events []RunEvent) ([]execution.ExecutionTarget, []string, error) {
	if item == nil {
		return nil, nil, fmt.Errorf("legacy execution topology requires task")
	}
	primary := item.ExecutionTarget
	var evidenceEventIDs []string
	if primary.IsZero() {
		var err error
		primary, evidenceEventIDs, err = resolveLegacyExecutionTargetWithEvidence(item, events)
		if err != nil {
			return nil, nil, err
		}
	}
	if primary.IsZero() {
		return nil, evidenceEventIDs, nil
	}
	if len(item.ModelTopology) == 0 {
		targets, err := validateRestoredExecutionTargets([]execution.ExecutionTarget{primary})
		return targets, evidenceEventIDs, err
	}

	topology := make([]execution.ExecutionTarget, 0, len(item.ModelTopology)+1)
	for index, model := range item.ModelTopology {
		leaf := *item
		leaf.Model = model
		leaf.ExecutionTarget = execution.ExecutionTarget{}
		leaf.ExecutionTopology = nil
		leaf.ID = item.ID
		leafTarget, leafEvidence, leafErr := resolveLegacyExecutionTargetWithEvidence(&leaf, events)
		if leafErr != nil {
			return nil, nil, fmt.Errorf("legacy topology leaf %d: %w", index, leafErr)
		}
		if leafTarget.IsZero() {
			continue
		}
		topology = append(topology, leafTarget)
		for _, eventID := range leafEvidence {
			if eventID != "" && !slices.Contains(evidenceEventIDs, eventID) {
				evidenceEventIDs = append(evidenceEventIDs, eventID)
			}
		}
	}
	if len(topology) == 0 {
		topology = []execution.ExecutionTarget{primary}
	} else if !slices.Contains(topology, primary) {
		return nil, nil, fmt.Errorf("legacy execution topology omits primary target %q", primary)
	}
	validated, err := validateRestoredExecutionTargets(topology)
	return validated, evidenceEventIDs, err
}

func resolveLegacyExecutionTargetWithEvidence(item *TodoItem, events []RunEvent) (execution.ExecutionTarget, []string, error) {
	target, err := migratableLegacyExecutionTarget(item)
	if err == nil {
		return target, nil, nil
	}
	if _, ambiguous := errors.AsType[*LegacyExecutionTargetAmbiguousError](err); !ambiguous {
		return execution.ExecutionTarget{}, nil, err
	}
	recovered, evidenceEventIDs, ok := targetFromHistoricalProfileEvidence(item, events)
	if !ok {
		return execution.ExecutionTarget{}, nil, err
	}
	return recovered, evidenceEventIDs, nil
}

func legacyExecutionTopologyNeedsHistoricalEvidence(item *TodoItem) (bool, error) {
	if item == nil {
		return false, fmt.Errorf("legacy execution topology requires task")
	}
	check := func(candidate *TodoItem) (bool, error) {
		_, err := migratableLegacyExecutionTarget(candidate)
		if err == nil {
			return false, nil
		}
		if _, ambiguous := errors.AsType[*LegacyExecutionTargetAmbiguousError](err); ambiguous {
			return true, nil
		}
		return false, err
	}
	needs, err := check(item)
	if err != nil || needs {
		return needs, err
	}
	for _, model := range item.ModelTopology {
		leaf := *item
		leaf.Model = model
		leaf.ExecutionTarget = execution.ExecutionTarget{}
		leaf.ExecutionTopology = nil
		needs, err := check(&leaf)
		if err != nil || needs {
			return needs, err
		}
	}
	return false, nil
}

func (c *Coordinator) readLegacyMigrationEvidence(ctx context.Context) ([]RunEvent, error) {
	events, err := c.EventJournal().ReadEvents(context.WithoutCancel(ctx))
	if err != nil {
		return nil, fmt.Errorf("read historical profile evidence: %w", err)
	}
	if c == nil || c.session == nil || strings.TrimSpace(c.session.Workspace) == "" {
		return events, nil
	}
	tree, err := LoadSessionTree(c.session.Workspace)
	if err != nil {
		return nil, fmt.Errorf("load session tree for historical profile evidence: %w", err)
	}
	lineage, err := projectEventsForBranch(events, tree, c.activeBranchID())
	if err != nil {
		return nil, fmt.Errorf("project active branch historical profile evidence: %w", err)
	}
	return lineage, nil
}

func executionTargetMigrationIdempotencyKey(branchID, taskID string) string {
	branchID = strings.TrimSpace(branchID)
	if branchID == "" {
		branchID = "main"
	}
	return fmt.Sprintf("execution-target-migrated:%s:%s:%d", branchID, taskID, executionTargetMigrationVersion)
}

func validateRestoredExecutionTargets(targets []execution.ExecutionTarget) ([]execution.ExecutionTarget, error) {
	for _, target := range targets {
		if err := target.Validate(); err != nil {
			return nil, err
		}
	}
	return targets, nil
}

// migrateLegacyExecutionTarget appends the one-time durable freeze before a
// resumed legacy task is allowed to reach worker dispatch.
func (c *Coordinator) migrateLegacyExecutionTarget(ctx context.Context, item *TodoItem) error {
	if item == nil || !item.ExecutionTarget.IsZero() {
		return nil
	}
	// Historical policy-only checkpoints may never have selected a worker
	// model. They are not executable occurrences and retain their existing
	// recovery semantics rather than manufacturing an identity.
	if strings.TrimSpace(item.Model) == "" {
		return nil
	}
	needsEvidence, err := legacyExecutionTopologyNeedsHistoricalEvidence(item)
	if err != nil {
		return err
	}
	var events []RunEvent
	if needsEvidence {
		events, err = c.readLegacyMigrationEvidence(ctx)
		if err != nil {
			return err
		}
	}
	topology, evidenceEventIDs, err := resolveLegacyExecutionTopology(item, events)
	if err != nil {
		return err
	}
	if len(topology) == 0 {
		return nil
	}
	target := topology[0]
	branchID := c.activeBranchID()
	payload, err := json.Marshal(ExecutionTargetMigratedPayload{TaskID: item.ID, LegacyModel: item.Model, LegacySubagentProvider: item.SubagentProvider, ExecutionTarget: target, ExecutionTopology: topology, MigrationVersion: executionTargetMigrationVersion, EvidenceEventIDs: evidenceEventIDs, BranchID: branchID})
	if err != nil {
		return err
	}
	if _, err := c.EventJournal().Append(context.WithoutCancel(ctx), RunEvent{Type: string(EventExecutionTargetMigrated), Actor: "coordinator", BranchID: branchID, TaskID: item.ID, IdempotencyKey: executionTargetMigrationIdempotencyKey(branchID, item.ID), Payload: payload}); err != nil {
		return err
	}
	item.ExecutionTarget = target
	item.ExecutionTopology = cloneExecutionTopology(topology)
	return c.taskTracker.TodoList().TryApplyProjectedItem(item)
}
