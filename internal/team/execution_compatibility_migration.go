package team

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/kjelly/hufu/internal/execution"
	"github.com/kjelly/hufu/internal/executioncompat"
	"github.com/kjelly/hufu/internal/modelprofile"
)

const executionCompatibilityMigrationSchemaVersion = 1

// ReceiptBackendMigration is a content-free receipt identity patch applied by
// the append-only execution compatibility materializer.
type ReceiptBackendMigration struct {
	RunID            string `json:"run_id"`
	Attempt          int    `json:"attempt"`
	ModelExecutionID string `json:"model_execution_id,omitempty"`
	Backend          string `json:"backend"`
}

// ExecutionCompatibilityMigratedPayload is the self-contained canonical task
// identity derived exclusively from historical workspace evidence.
type ExecutionCompatibilityMigratedPayload struct {
	SchemaVersion     int                         `json:"schema_version"`
	BranchID          string                      `json:"branch_id"`
	TaskID            string                      `json:"task_id"`
	SourceKind        string                      `json:"source_kind"`
	SourceDigest      string                      `json:"source_digest"`
	EvidenceEventIDs  []string                    `json:"evidence_event_ids,omitempty"`
	ExecutionTarget   execution.ExecutionTarget   `json:"execution_target"`
	ExecutionTopology []execution.ExecutionTarget `json:"execution_topology"`
	BackendBinding    *BackendBinding             `json:"backend_binding,omitempty"`
	ReceiptBackends   []ReceiptBackendMigration   `json:"receipt_backends,omitempty"`
	CanonicalTask     json.RawMessage             `json:"canonical_task,omitempty"`
}

// ExecutionPolicySnapshotMigratedPayload is the analogous self-contained v4
// policy snapshot replacement for one historical v3 policy event.
type ExecutionPolicySnapshotMigratedPayload struct {
	SchemaVersion int                     `json:"schema_version"`
	BranchID      string                  `json:"branch_id"`
	RunID         string                  `json:"run_id,omitempty"`
	SourceKind    string                  `json:"source_kind"`
	SourceEventID string                  `json:"source_event_id,omitempty"`
	SourceDigest  string                  `json:"source_digest"`
	Snapshot      ExecutionPolicySnapshot `json:"snapshot"`
}

// ExecutionCompatibilityApplyResult records only content-free materializer
// effects. It deliberately does not expose source task text or model names.
type ExecutionCompatibilityApplyResult struct {
	SchemaVersion         int    `json:"schema_version"`
	Scope                 string `json:"scope"`
	TaskMigrationEvents   int    `json:"task_migration_events"`
	PolicyMigrationEvents int    `json:"policy_migration_events"`
	ProjectionRebuilt     bool   `json:"projection_rebuilt"`
}

type executionCompatibilityTaskPlan struct {
	branchID string
	runID    string
	payload  ExecutionCompatibilityMigratedPayload
}

type executionCompatibilityPolicyPlan struct {
	branchID string
	runID    string
	payload  ExecutionPolicySnapshotMigratedPayload
}

// ApplyExecutionCompatibility appends canonical migration events after a
// complete read-only preflight. It never rewrites historical event bytes. A
// partial append is intentionally recoverable: deterministic idempotency keys
// make a later invocation continue from the exact durable chain head.
func ApplyExecutionCompatibility(ctx context.Context, workspace, requestedBranch string) (*ExecutionCompatibilityApplyResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	report, err := InspectExecutionCompatibility(ctx, workspace, requestedBranch)
	if err != nil {
		return nil, fmt.Errorf("apply execution compatibility inspect: %w", err)
	}
	if report.AmbiguousTasks != 0 || report.UnmigratableTasks != 0 || report.AmbiguousPolicySnapshots != 0 || report.UnmigratablePolicySnapshots != 0 {
		return nil, fmt.Errorf("apply execution compatibility: inspection has ambiguous or unmigratable subjects")
	}

	events, tree, session, branchIDs, scope, err := loadExecutionCompatibilityApplyInputs(ctx, workspace, requestedBranch)
	if err != nil {
		return nil, err
	}
	taskPlans, policyPlans, err := buildExecutionCompatibilityPlans(ctx, events, tree, session, branchIDs)
	if err != nil {
		return nil, err
	}
	result := &ExecutionCompatibilityApplyResult{SchemaVersion: executionCompatibilityMigrationSchemaVersion, Scope: scope}
	if len(taskPlans) == 0 && len(policyPlans) == 0 {
		return result, nil
	}

	// Opening the writer happens strictly after all inspection and derivation.
	// EventStore validates the existing chain and acquires its interprocess
	// exclusive lock for every atomic append; no prior byte can be rewritten.
	store, err := OpenEventStore(workspace)
	if err != nil {
		return nil, fmt.Errorf("apply execution compatibility open event store: %w", err)
	}
	defer func() { _ = store.Close() }()
	for _, plan := range taskPlans {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		data, err := json.Marshal(plan.payload)
		if err != nil {
			return nil, fmt.Errorf("marshal execution compatibility task %q: %w", plan.payload.TaskID, err)
		}
		key := fmt.Sprintf("execution-compatibility-migrated:%s:%s:v1:%s", plan.branchID, plan.payload.TaskID, plan.payload.SourceDigest)
		if _, err := store.AppendPersistedContext(ctx, RunEvent{
			Type: string(EventExecutionCompatibilityMigrated), Actor: "execution-compatibility-migrator",
			BranchID: plan.branchID, TaskID: plan.payload.TaskID, RunID: plan.runID,
			IdempotencyKey: key, Payload: data,
		}); err != nil {
			return nil, fmt.Errorf("append execution compatibility task %q: %w", plan.payload.TaskID, err)
		}
		result.TaskMigrationEvents++
	}
	for _, plan := range policyPlans {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		data, err := json.Marshal(plan.payload)
		if err != nil {
			return nil, fmt.Errorf("marshal execution compatibility policy %q: %w", plan.payload.SourceEventID, err)
		}
		source := plan.payload.SourceEventID
		if source == "" {
			source = "session"
		}
		key := fmt.Sprintf("execution-policy-snapshot-migrated:%s:%s:v1:%s", plan.branchID, source, plan.payload.SourceDigest)
		if _, err := store.AppendPersistedContext(ctx, RunEvent{
			Type: string(EventExecutionPolicySnapshotMigrated), Actor: "execution-compatibility-migrator",
			BranchID: plan.branchID, RunID: plan.runID, IdempotencyKey: key, Payload: data,
		}); err != nil {
			return nil, fmt.Errorf("append execution compatibility policy %q: %w", source, err)
		}
		result.PolicyMigrationEvents++
	}

	// The canonical append is now durable. Projection failure intentionally
	// leaves those events in place and returns an error so retry can rebuild
	// without duplicating the chain entries.
	if err := RebuildSessionForBranch(workspace, tree, store, tree.ActiveBranch); err != nil {
		return nil, fmt.Errorf("apply execution compatibility rebuild active projection: %w", err)
	}
	result.ProjectionRebuilt = true
	return result, nil
}

func loadExecutionCompatibilityApplyInputs(ctx context.Context, workspace, requestedBranch string) ([]RunEvent, *SessionTree, *SessionData, []string, string, error) {
	var events []RunEvent
	if err := StreamValidatedRunEvents(ctx, workspace, func(event RunEvent) error {
		events = append(events, event)
		return nil
	}); err != nil {
		return nil, nil, nil, nil, "", fmt.Errorf("apply execution compatibility event store: %w", err)
	}
	tree, err := LoadSessionTree(workspace)
	if err != nil {
		return nil, nil, nil, nil, "", fmt.Errorf("apply execution compatibility session tree: %w", err)
	}
	branchIDs, scope, err := compatibilityBranches(tree, requestedBranch)
	if err != nil {
		return nil, nil, nil, nil, "", err
	}
	session, err := loadCompatibilitySession(workspace)
	if err != nil {
		return nil, nil, nil, nil, "", err
	}
	return events, tree, session, branchIDs, scope, nil
}

func buildExecutionCompatibilityPlans(ctx context.Context, events []RunEvent, tree *SessionTree, session *SessionData, branchIDs []string) ([]executionCompatibilityTaskPlan, []executionCompatibilityPolicyPlan, error) {
	var taskPlans []executionCompatibilityTaskPlan
	var policyPlans []executionCompatibilityPolicyPlan
	for _, branchID := range branchIDs {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		lineage, err := ProjectValidatedEventsForBranch(events, tree, branchID)
		if err != nil {
			return nil, nil, fmt.Errorf("apply execution compatibility branch %q: %w", branchID, err)
		}
		tasks, eventTaskIDs, err := collectCompatibilityTaskSubjects(lineage)
		if err != nil {
			return nil, nil, fmt.Errorf("apply execution compatibility branch %q tasks: %w", branchID, err)
		}
		ids := make([]string, 0, len(tasks))
		for id := range tasks {
			ids = append(ids, id)
		}
		sort.Strings(ids)
		for _, taskID := range ids {
			plan, needed, err := buildEventTaskCompatibilityPlan(branchID, taskID, tasks[taskID], lineage)
			if err != nil {
				return nil, nil, err
			}
			if needed {
				taskPlans = append(taskPlans, plan)
			}
		}
		if session != nil && branchID == tree.ActiveBranch {
			for _, task := range session.Tasks {
				if task == nil || strings.TrimSpace(task.ID) == "" {
					continue
				}
				if _, exists := eventTaskIDs[task.ID]; exists {
					continue
				}
				plan, needed, err := buildSessionTaskCompatibilityPlan(branchID, task, lineage)
				if err != nil {
					return nil, nil, err
				}
				if needed {
					taskPlans = append(taskPlans, plan)
				}
			}
		}
		plans, err := buildPolicyCompatibilityPlans(branchID, lineage)
		if err != nil {
			return nil, nil, err
		}
		policyPlans = append(policyPlans, plans...)
		if session != nil && branchID == tree.ActiveBranch {
			plan, needed, err := buildSessionPolicyCompatibilityPlan(branchID, session, lineage)
			if err != nil {
				return nil, nil, err
			}
			if needed {
				policyPlans = append(policyPlans, plan)
			}
		}
	}
	sort.Slice(taskPlans, func(i, j int) bool {
		if taskPlans[i].branchID != taskPlans[j].branchID {
			return taskPlans[i].branchID < taskPlans[j].branchID
		}
		return taskPlans[i].payload.TaskID < taskPlans[j].payload.TaskID
	})
	sort.Slice(policyPlans, func(i, j int) bool {
		if policyPlans[i].branchID != policyPlans[j].branchID {
			return policyPlans[i].branchID < policyPlans[j].branchID
		}
		left, right := policyPlans[i].payload, policyPlans[j].payload
		if policyPlans[i].runID != policyPlans[j].runID {
			return policyPlans[i].runID < policyPlans[j].runID
		}
		if left.SourceEventID != right.SourceEventID {
			return left.SourceEventID < right.SourceEventID
		}
		return left.SourceDigest < right.SourceDigest
	})
	return taskPlans, policyPlans, nil
}

func collectCompatibilityTaskSubjects(events []RunEvent) (map[string]*compatibilityTaskSubject, map[string]struct{}, error) {
	tasks := make(map[string]*compatibilityTaskSubject)
	eventTaskIDs := make(map[string]struct{})
	for _, event := range events {
		if !isCompatibilityTaskEvent(event.Type) {
			continue
		}
		taskID, payload, err := decodeCompatibilityTask(event)
		if err != nil {
			return nil, nil, fmt.Errorf("task event %q: %w", event.ID, err)
		}
		eventTaskIDs[taskID] = struct{}{}
		subject := tasks[taskID]
		if subject == nil {
			subject = &compatibilityTaskSubject{}
			tasks[taskID] = subject
		}
		subject.merge(payload, event)
	}
	for _, event := range events {
		if EventType(event.Type) != EventProviderSessionBound {
			continue
		}
		var payload ProviderSessionBoundPayload
		if err := json.Unmarshal(event.Payload, &payload); err != nil {
			continue
		}
		taskID := strings.TrimSpace(event.TaskID)
		if taskID == "" {
			taskID = strings.TrimSpace(payload.TaskID)
		}
		subject := tasks[taskID]
		if subject == nil || strings.TrimSpace(payload.Provider) == "" {
			continue
		}
		subject.input.ProviderSessionBindings = append(subject.input.ProviderSessionBindings, payload.Provider)
		if subject.providerBinding == nil {
			subject.providerBinding = &ProviderBinding{
				Provider:         payload.Provider,
				Protocol:         payload.Protocol,
				SessionID:        payload.SessionID,
				ExecutionWorldID: payload.ExecutionWorldID,
				CWD:              payload.CWD,
			}
		}
		if event.ID != "" {
			subject.evidence = append(subject.evidence, event.ID)
		}
	}
	for _, event := range events {
		if EventType(event.Type) != EventModelProfileResolved {
			continue
		}
		var profile modelprofile.TelemetryProjection
		if json.Unmarshal(event.Payload, &profile) != nil || strings.TrimSpace(profile.Provider) == "" {
			continue
		}
		for _, subject := range tasks {
			if subject.runID != event.RunID || !compatibilityProfileMatchesTask(profile.ModelID, subject) {
				continue
			}
			subject.input.ProfileProviders = append(subject.input.ProfileProviders, profile.Provider)
			if event.ID != "" {
				subject.evidence = append(subject.evidence, event.ID)
			}
		}
	}
	return tasks, eventTaskIDs, nil
}

func compatibilityProfileMatchesTask(profileModel string, subject *compatibilityTaskSubject) bool {
	if subject == nil || subject.input.Target != (executioncompat.Target{}) || len(subject.input.Topology) != 0 {
		return false
	}
	model := strings.TrimSpace(subject.input.Model)
	selector, err := execution.ParseExecutionSelector(model)
	if err == nil && selector.Model != "" {
		model = selector.Model
	}
	return strings.TrimSpace(profileModel) != "" && strings.TrimSpace(profileModel) == model
}

func buildEventTaskCompatibilityPlan(branchID, taskID string, subject *compatibilityTaskSubject, lineage []RunEvent) (executionCompatibilityTaskPlan, bool, error) {
	if subject == nil {
		return executionCompatibilityTaskPlan{}, false, fmt.Errorf("task %q has no compatibility evidence", taskID)
	}
	classification := executioncompat.ClassifyTask(subject.input)
	if classification.Classification == executioncompat.ClassificationCanonical || classification.Classification == executioncompat.ClassificationNotApplicable {
		return executionCompatibilityTaskPlan{}, false, nil
	}
	if classification.Classification != executioncompat.ClassificationMigratable {
		return executionCompatibilityTaskPlan{}, false, fmt.Errorf("task %q is %s: %s", taskID, classification.Classification, classification.ReasonCode)
	}
	digest, err := compatibilityTaskSourceDigest(subject.input)
	if err != nil {
		return executionCompatibilityTaskPlan{}, false, err
	}
	derivation, err := executioncompat.DeriveTask(subject.input)
	if err != nil {
		return executionCompatibilityTaskPlan{}, false, fmt.Errorf("derive task %q: %w", taskID, err)
	}
	target := executionTargetFromCompatibility(derivation.Target)
	payload := ExecutionCompatibilityMigratedPayload{
		SchemaVersion: executionCompatibilityMigrationSchemaVersion,
		BranchID:      branchID, TaskID: taskID, SourceKind: "event_lineage", SourceDigest: digest,
		EvidenceEventIDs:  sortedUniqueStrings(subject.evidence),
		ExecutionTarget:   target,
		ExecutionTopology: executionTopologyFromCompatibility(derivation.Topology),
		BackendBinding:    canonicalCompatibilityBackendBinding(subject.backendBinding, subject.providerBinding, target),
		ReceiptBackends:   receiptBackendMigrations(subject.receipts, derivation.ReceiptBackend),
	}
	if exists, err := matchingTaskCompatibilityMigration(lineage, branchID, payload); err != nil {
		return executionCompatibilityTaskPlan{}, false, err
	} else if exists {
		return executionCompatibilityTaskPlan{}, false, nil
	}
	if err := validateExecutionCompatibilityMigrationPayload(payload); err != nil {
		return executionCompatibilityTaskPlan{}, false, fmt.Errorf("derive task %q migration: %w", taskID, err)
	}
	return executionCompatibilityTaskPlan{branchID: branchID, runID: subject.runID, payload: payload}, true, nil
}

func buildSessionTaskCompatibilityPlan(branchID string, task *TodoItem, lineage []RunEvent) (executionCompatibilityTaskPlan, bool, error) {
	input := compatibilityInputFromTodo(task)
	classification := executioncompat.ClassifyTask(input)
	if classification.Classification == executioncompat.ClassificationCanonical || classification.Classification == executioncompat.ClassificationNotApplicable {
		return executionCompatibilityTaskPlan{}, false, nil
	}
	if classification.Classification != executioncompat.ClassificationMigratable {
		return executionCompatibilityTaskPlan{}, false, fmt.Errorf("session task %q is %s: %s", task.ID, classification.Classification, classification.ReasonCode)
	}
	digest, err := compatibilityTaskSourceDigest(input)
	if err != nil {
		return executionCompatibilityTaskPlan{}, false, err
	}
	derivation, err := executioncompat.DeriveTask(input)
	if err != nil {
		return executionCompatibilityTaskPlan{}, false, fmt.Errorf("derive session task %q: %w", task.ID, err)
	}
	canonical, receipts, err := canonicalSessionCompatibilityTask(task, derivation)
	if err != nil {
		return executionCompatibilityTaskPlan{}, false, err
	}
	target := executionTargetFromCompatibility(derivation.Target)
	payload := ExecutionCompatibilityMigratedPayload{
		SchemaVersion: executionCompatibilityMigrationSchemaVersion,
		BranchID:      branchID, TaskID: task.ID, SourceKind: "session_snapshot", SourceDigest: digest,
		ExecutionTarget:   target,
		ExecutionTopology: executionTopologyFromCompatibility(derivation.Topology),
		BackendBinding:    canonicalCompatibilityBackendBinding(task.BackendBinding, task.ProviderBinding, target),
		ReceiptBackends:   receipts, CanonicalTask: canonical,
	}
	if exists, err := matchingTaskCompatibilityMigration(lineage, branchID, payload); err != nil {
		return executionCompatibilityTaskPlan{}, false, err
	} else if exists {
		return executionCompatibilityTaskPlan{}, false, nil
	}
	if err := validateExecutionCompatibilityMigrationPayload(payload); err != nil {
		return executionCompatibilityTaskPlan{}, false, fmt.Errorf("derive session task %q migration: %w", task.ID, err)
	}
	return executionCompatibilityTaskPlan{branchID: branchID, payload: payload}, true, nil
}

func compatibilityTaskSourceDigest(input executioncompat.TaskInput) (string, error) {
	// A struct rather than a map fixes field ordering. These are precisely the
	// durable identity fields consumed by ClassifyTask/DeriveTask.
	evidence := struct {
		Target                  executioncompat.Target    `json:"target"`
		Topology                []executioncompat.Target  `json:"topology,omitempty"`
		Model                   string                    `json:"model,omitempty"`
		SubagentProvider        string                    `json:"subagent_provider,omitempty"`
		ProviderBinding         string                    `json:"provider_binding,omitempty"`
		BackendBinding          string                    `json:"backend_binding,omitempty"`
		ProviderBindings        []string                  `json:"provider_bindings,omitempty"`
		BackendBindings         []string                  `json:"backend_bindings,omitempty"`
		ProviderSessionBindings []string                  `json:"provider_session_bindings,omitempty"`
		ProfileProviders        []string                  `json:"profile_providers,omitempty"`
		Receipts                []executioncompat.Receipt `json:"receipts,omitempty"`
	}{
		Target:                  input.Target,
		Topology:                input.Topology,
		Model:                   input.Model,
		SubagentProvider:        input.SubagentProvider,
		ProviderBinding:         input.ProviderBinding,
		BackendBinding:          input.BackendBinding,
		ProviderBindings:        input.ProviderBindings,
		BackendBindings:         input.BackendBindings,
		ProviderSessionBindings: input.ProviderSessionBindings,
		ProfileProviders:        input.ProfileProviders,
		Receipts:                input.Receipts,
	}
	return executionCompatibilityDigest(evidence)
}

func executionCompatibilityDigest(value any) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("marshal compatibility source evidence: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func executionTargetFromCompatibility(target executioncompat.Target) execution.ExecutionTarget {
	return execution.ExecutionTarget{Backend: target.Backend, Model: target.Model}
}

func executionTopologyFromCompatibility(topology []executioncompat.Target) []execution.ExecutionTarget {
	result := make([]execution.ExecutionTarget, 0, len(topology))
	for _, target := range topology {
		result = append(result, executionTargetFromCompatibility(target))
	}
	return result
}

func receiptBackendMigrations(receipts []compatibilityReceipt, backends []string) []ReceiptBackendMigration {
	result := make([]ReceiptBackendMigration, 0, len(receipts))
	for index, receipt := range receipts {
		if index >= len(backends) || strings.TrimSpace(backends[index]) == "" {
			continue
		}
		result = append(result, ReceiptBackendMigration{RunID: receipt.RunID, Attempt: receipt.Attempt, ModelExecutionID: receipt.ModelExecutionID, Backend: backends[index]})
	}
	return result
}

func canonicalCompatibilityBackendBinding(binding *BackendBinding, provider *ProviderBinding, target execution.ExecutionTarget) *BackendBinding {
	canonical := cloneBackendBinding(binding)
	if canonical == nil {
		canonical = backendBindingFromProviderBinding(provider, target)
	}
	if canonical == nil {
		canonical = &BackendBinding{}
	}
	canonical.Backend = target.Backend
	if canonical.EffectiveTarget == "" {
		canonical.EffectiveTarget = target.Model
	}
	return canonical
}

func canonicalSessionCompatibilityTask(task *TodoItem, derivation executioncompat.TaskDerivation) (json.RawMessage, []ReceiptBackendMigration, error) {
	if task == nil {
		return nil, nil, fmt.Errorf("canonical session task is nil")
	}
	canonical := cloneTodoItem(task)
	canonical.ExecutionTarget = executionTargetFromCompatibility(derivation.Target)
	canonical.ExecutionTopology = executionTopologyFromCompatibility(derivation.Topology)
	canonical.BackendBinding = canonicalCompatibilityBackendBinding(canonical.BackendBinding, canonical.ProviderBinding, canonical.ExecutionTarget)
	canonical.Model = ""
	canonical.ModelTopology = nil
	canonical.SubagentProvider = ""
	canonical.ProviderBinding = nil
	var receipts []compatibilityReceipt
	if canonical.ExecutionReceipt != nil {
		receipts = append(receipts, compatibilityReceipt{RunID: canonical.ExecutionReceipt.RunID, Attempt: canonical.ExecutionReceipt.Attempt, ModelExecutionID: canonical.ExecutionReceipt.ModelExecutionID, Backend: canonical.ExecutionReceipt.Backend, SubagentProvider: canonical.ExecutionReceipt.SubagentProvider})
	}
	for _, receipt := range canonical.ExecutionReceipts {
		receipts = append(receipts, compatibilityReceipt{RunID: receipt.RunID, Attempt: receipt.Attempt, ModelExecutionID: receipt.ModelExecutionID, Backend: receipt.Backend, SubagentProvider: receipt.SubagentProvider})
	}
	for index, backend := range derivation.ReceiptBackend {
		if index >= len(receipts) {
			break
		}
		if index == 0 && canonical.ExecutionReceipt != nil {
			canonical.ExecutionReceipt.Backend = backend
			canonical.ExecutionReceipt.SubagentProvider = ""
			continue
		}
		manyIndex := index
		if canonical.ExecutionReceipt != nil {
			manyIndex--
		}
		if manyIndex >= 0 && manyIndex < len(canonical.ExecutionReceipts) {
			canonical.ExecutionReceipts[manyIndex].Backend = backend
			canonical.ExecutionReceipts[manyIndex].SubagentProvider = ""
		}
	}
	data, err := json.Marshal(taskTransitionPayload(canonical))
	if err != nil {
		return nil, nil, fmt.Errorf("marshal canonical session task: %w", err)
	}
	if hasLegacyTaskIdentityJSON(data) {
		return nil, nil, fmt.Errorf("canonical session task still contains legacy execution identity")
	}
	return data, receiptBackendMigrations(receipts, derivation.ReceiptBackend), nil
}

func hasLegacyTaskIdentityJSON(data []byte) bool {
	var value map[string]json.RawMessage
	if json.Unmarshal(data, &value) != nil {
		return true
	}
	return hasAnyKey(value, "model", "model_topology", "subagent_provider", "provider_binding") || countLegacyReceiptProviders(value) != 0
}

func matchingTaskCompatibilityMigration(lineage []RunEvent, branchID string, want ExecutionCompatibilityMigratedPayload) (bool, error) {
	for _, event := range lineage {
		if EventType(event.Type) != EventExecutionCompatibilityMigrated || effectiveEventBranchID(event) != branchID || event.TaskID != want.TaskID {
			continue
		}
		var got ExecutionCompatibilityMigratedPayload
		if err := json.Unmarshal(event.Payload, &got); err != nil {
			return false, fmt.Errorf("task %q migration event %q is invalid: %w", want.TaskID, event.ID, err)
		}
		if got.BranchID != branchID || got.TaskID != want.TaskID {
			return false, fmt.Errorf("task %q migration event %q has conflicting scope", want.TaskID, event.ID)
		}
		if got.SourceDigest != want.SourceDigest {
			return false, fmt.Errorf("task %q has conflicting execution compatibility migration", want.TaskID)
		}
		if !sameTaskMigrationProjection(got, want) {
			return false, fmt.Errorf("task %q has conflicting execution compatibility migration projection", want.TaskID)
		}
		return true, nil
	}
	return false, nil
}

func sameTaskMigrationProjection(left, right ExecutionCompatibilityMigratedPayload) bool {
	left.EvidenceEventIDs = sortedUniqueStrings(left.EvidenceEventIDs)
	right.EvidenceEventIDs = sortedUniqueStrings(right.EvidenceEventIDs)
	left.CanonicalTask = compactCompatibilityJSON(left.CanonicalTask)
	right.CanonicalTask = compactCompatibilityJSON(right.CanonicalTask)
	return left.SchemaVersion == right.SchemaVersion && left.BranchID == right.BranchID && left.TaskID == right.TaskID && left.SourceKind == right.SourceKind && left.SourceDigest == right.SourceDigest &&
		execution.TargetsEqual(left.ExecutionTarget, right.ExecutionTarget) && execution.TargetSlicesEqual(left.ExecutionTopology, right.ExecutionTopology) &&
		equivalentBackendMigrationBinding(left.BackendBinding, right.BackendBinding) && receiptBackendMigrationSlicesEqual(left.ReceiptBackends, right.ReceiptBackends) && string(left.CanonicalTask) == string(right.CanonicalTask) && stringSlicesEqual(left.EvidenceEventIDs, right.EvidenceEventIDs)
}

func compactCompatibilityJSON(data json.RawMessage) json.RawMessage {
	if len(data) == 0 {
		return nil
	}
	var value any
	if json.Unmarshal(data, &value) != nil {
		return data
	}
	canonical, err := json.Marshal(value)
	if err != nil {
		return data
	}
	return canonical
}

func equivalentBackendMigrationBinding(left, right *BackendBinding) bool {
	if left == nil || right == nil {
		return left == nil && right == nil
	}
	return *left == *right
}

func receiptBackendMigrationSlicesEqual(left, right []ReceiptBackendMigration) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func stringSlicesEqual(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func validateExecutionCompatibilityMigrationPayload(payload ExecutionCompatibilityMigratedPayload) error {
	if payload.SchemaVersion != executionCompatibilityMigrationSchemaVersion || strings.TrimSpace(payload.BranchID) == "" || strings.TrimSpace(payload.TaskID) == "" || (payload.SourceKind != "event_lineage" && payload.SourceKind != "session_snapshot") || strings.TrimSpace(payload.SourceDigest) == "" {
		return fmt.Errorf("migration payload identity is incomplete")
	}
	if err := validateExecutionIdentity(payload.ExecutionTarget, payload.ExecutionTopology, payload.BackendBinding); err != nil {
		return err
	}
	seenReceipts := make(map[string]string, len(payload.ReceiptBackends))
	for _, receipt := range payload.ReceiptBackends {
		backend := execution.CanonicalTargetBackendName(receipt.Backend)
		if backend == "" {
			return fmt.Errorf("receipt backend migration is incomplete")
		}
		identity := fmt.Sprintf("%s:%d:%s", receipt.RunID, receipt.Attempt, receipt.ModelExecutionID)
		if previous, exists := seenReceipts[identity]; exists && previous != backend {
			return fmt.Errorf("receipt backend migrations disagree for %q", identity)
		}
		seenReceipts[identity] = backend
	}
	if payload.SourceKind == "event_lineage" && len(payload.CanonicalTask) != 0 {
		return fmt.Errorf("event-lineage migration must not contain canonical task")
	}
	if payload.SourceKind == "session_snapshot" {
		if len(payload.CanonicalTask) == 0 {
			return fmt.Errorf("session snapshot migration lacks canonical task")
		}
		var canonical TodoItem
		if err := json.Unmarshal(payload.CanonicalTask, &canonical); err != nil {
			return fmt.Errorf("decode canonical task: %w", err)
		}
		if canonical.ID != payload.TaskID || !execution.TargetsEqual(canonical.ExecutionTarget, payload.ExecutionTarget) || !execution.TargetSlicesEqual(canonical.ExecutionTopology, payload.ExecutionTopology) {
			return fmt.Errorf("canonical task execution identity disagrees with migration")
		}
		if hasLegacyTaskIdentityJSON(payload.CanonicalTask) {
			return fmt.Errorf("canonical task contains legacy execution identity")
		}
	}
	return nil
}

func buildPolicyCompatibilityPlans(branchID string, lineage []RunEvent) ([]executionCompatibilityPolicyPlan, error) {
	var plans []executionCompatibilityPolicyPlan
	for _, event := range lineage {
		if EventType(event.Type) != EventExecutionPolicySnapshot {
			continue
		}
		var snapshot ExecutionPolicySnapshot
		if err := json.Unmarshal(event.Payload, &snapshot); err != nil {
			return nil, fmt.Errorf("policy event %q is invalid: %w", event.ID, err)
		}
		plan, needed, err := buildPolicyCompatibilityPlan(branchID, event.ID, event.RunID, "event_lineage", &snapshot, lineage)
		if err != nil {
			return nil, err
		}
		if needed {
			plans = append(plans, plan)
		}
	}
	return plans, nil
}

func buildSessionPolicyCompatibilityPlan(branchID string, session *SessionData, lineage []RunEvent) (executionCompatibilityPolicyPlan, bool, error) {
	if session == nil || session.ExecutionPolicySnapshot == nil {
		return executionCompatibilityPolicyPlan{}, false, nil
	}
	for _, event := range lineage {
		if EventType(event.Type) == EventExecutionPolicySnapshot {
			return executionCompatibilityPolicyPlan{}, false, nil
		}
	}
	return buildPolicyCompatibilityPlan(branchID, "", "", "session_snapshot", session.ExecutionPolicySnapshot, lineage)
}

func buildPolicyCompatibilityPlan(branchID, sourceEventID, runID, sourceKind string, snapshot *ExecutionPolicySnapshot, lineage []RunEvent) (executionCompatibilityPolicyPlan, bool, error) {
	if snapshot == nil {
		return executionCompatibilityPolicyPlan{}, false, fmt.Errorf("policy snapshot is nil")
	}
	if snapshot.Version >= executionPolicySnapshotVersion {
		return executionCompatibilityPolicyPlan{}, false, nil
	}
	if err := validateExecutionPolicySnapshot(snapshot); err != nil {
		return executionCompatibilityPolicyPlan{}, false, fmt.Errorf("policy snapshot %q: %w", sourceEventID, err)
	}
	input := executioncompat.PolicyInput{Version: snapshot.Version, Routes: make([]executioncompat.PolicyRoute, 0, len(snapshot.ModelRoutes))}
	for _, route := range snapshot.ModelRoutes {
		input.Routes = append(input.Routes, executioncompat.PolicyRoute{Model: route.Model, Backend: route.Backend, ProviderKey: route.ProviderKey, LegacyProvider: route.LegacyProvider})
	}
	classification := executioncompat.ClassifyPolicySnapshot(input)
	if classification.Classification != executioncompat.ClassificationMigratable {
		return executionCompatibilityPolicyPlan{}, false, fmt.Errorf("policy snapshot %q is %s: %s", sourceEventID, classification.Classification, classification.ReasonCode)
	}
	digest, err := executionCompatibilityDigest(snapshot)
	if err != nil {
		return executionCompatibilityPolicyPlan{}, false, err
	}
	canonical, err := canonicalExecutionCompatibilityPolicySnapshot(snapshot)
	if err != nil {
		return executionCompatibilityPolicyPlan{}, false, err
	}
	payload := ExecutionPolicySnapshotMigratedPayload{SchemaVersion: executionCompatibilityMigrationSchemaVersion, BranchID: branchID, RunID: runID, SourceKind: sourceKind, SourceEventID: sourceEventID, SourceDigest: digest, Snapshot: *canonical}
	if exists, err := matchingPolicyCompatibilityMigration(lineage, branchID, payload); err != nil {
		return executionCompatibilityPolicyPlan{}, false, err
	} else if exists {
		return executionCompatibilityPolicyPlan{}, false, nil
	}
	if err := validateExecutionPolicyCompatibilityMigrationPayload(payload); err != nil {
		return executionCompatibilityPolicyPlan{}, false, err
	}
	return executionCompatibilityPolicyPlan{branchID: branchID, runID: runID, payload: payload}, true, nil
}

func canonicalExecutionCompatibilityPolicySnapshot(snapshot *ExecutionPolicySnapshot) (*ExecutionPolicySnapshot, error) {
	canonical := cloneExecutionPolicySnapshot(snapshot)
	canonical.Version = executionPolicySnapshotVersion
	canonical.DefaultLLMBackend = execution.CanonicalTargetBackendName(canonical.DefaultLLMBackend)
	for index := range canonical.Backends {
		canonical.Backends[index].Backend = execution.CanonicalTargetBackendName(canonical.Backends[index].Backend)
	}
	for index := range canonical.ModelRoutes {
		canonical.ModelRoutes[index].Backend = execution.CanonicalTargetBackendName(canonical.ModelRoutes[index].Backend)
		canonical.ModelRoutes[index].LegacyProvider = ""
	}
	for index := range canonical.ExecutionWorlds {
		canonical.ExecutionWorlds[index].Backend = execution.CanonicalTargetBackendName(canonical.ExecutionWorlds[index].Backend)
	}
	var err error
	canonical.ConfigurationHash, err = executionPolicyConfigurationHash(canonical)
	if err != nil {
		return nil, err
	}
	if err := validateExecutionPolicySnapshot(canonical); err != nil {
		return nil, err
	}
	return canonical, nil
}

func matchingPolicyCompatibilityMigration(lineage []RunEvent, branchID string, want ExecutionPolicySnapshotMigratedPayload) (bool, error) {
	for _, event := range lineage {
		if EventType(event.Type) != EventExecutionPolicySnapshotMigrated || effectiveEventBranchID(event) != branchID {
			continue
		}
		var got ExecutionPolicySnapshotMigratedPayload
		if err := json.Unmarshal(event.Payload, &got); err != nil {
			return false, fmt.Errorf("policy migration event %q is invalid: %w", event.ID, err)
		}
		if got.BranchID != branchID || got.SourceEventID != want.SourceEventID || got.SourceKind != want.SourceKind {
			continue
		}
		if got.SourceDigest != want.SourceDigest || !samePolicyMigrationProjection(got, want) {
			return false, fmt.Errorf("policy snapshot %q has conflicting execution compatibility migration", want.SourceEventID)
		}
		return true, nil
	}
	return false, nil
}

func samePolicyMigrationProjection(left, right ExecutionPolicySnapshotMigratedPayload) bool {
	return left.SchemaVersion == right.SchemaVersion && left.BranchID == right.BranchID && left.RunID == right.RunID && left.SourceKind == right.SourceKind && left.SourceEventID == right.SourceEventID && left.SourceDigest == right.SourceDigest && left.Snapshot.ConfigurationHash == right.Snapshot.ConfigurationHash
}

func validateExecutionPolicyCompatibilityMigrationPayload(payload ExecutionPolicySnapshotMigratedPayload) error {
	if payload.SchemaVersion != executionCompatibilityMigrationSchemaVersion || strings.TrimSpace(payload.BranchID) == "" || (payload.SourceKind != "event_lineage" && payload.SourceKind != "session_snapshot") || strings.TrimSpace(payload.SourceDigest) == "" {
		return fmt.Errorf("policy migration payload identity is incomplete")
	}
	if payload.SourceKind == "event_lineage" && strings.TrimSpace(payload.SourceEventID) == "" {
		return fmt.Errorf("event-lineage policy migration lacks source event")
	}
	if payload.Snapshot.Version != executionPolicySnapshotVersion {
		return fmt.Errorf("policy migration snapshot is not v%d", executionPolicySnapshotVersion)
	}
	return validateExecutionPolicySnapshot(&payload.Snapshot)
}
