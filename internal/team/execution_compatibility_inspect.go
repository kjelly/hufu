package team

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kjelly/hufu/internal/executioncompat"
)

// InspectExecutionCompatibility reads a workspace without taking the event
// store writer lock or changing any workspace file. branch is empty for every
// branch, otherwise it selects one branch ID or name.
func InspectExecutionCompatibility(ctx context.Context, workspace, branch string) (*executioncompat.InspectionReport, error) {
	if strings.TrimSpace(workspace) == "" {
		return nil, fmt.Errorf("inspect execution compatibility: workspace is empty")
	}
	info, err := os.Stat(workspace)
	if err != nil {
		return nil, fmt.Errorf("inspect execution compatibility workspace: %w", err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("inspect execution compatibility workspace %q is not a directory", workspace)
	}

	var events []RunEvent
	var raw eventCompatibilityCounts
	if err := StreamValidatedRunEvents(ctx, workspace, func(event RunEvent) error {
		events = append(events, event)
		raw.observeEvent(event)
		return nil
	}); err != nil {
		return nil, fmt.Errorf("inspect execution compatibility event store: %w", err)
	}
	if err := raw.observeExecutionEventProviders(workspace); err != nil {
		return nil, err
	}

	tree, err := LoadSessionTree(workspace)
	if err != nil {
		return nil, fmt.Errorf("inspect execution compatibility session tree: %w", err)
	}
	branchIDs, scope, err := compatibilityBranches(tree, branch)
	if err != nil {
		return nil, err
	}
	session, err := loadCompatibilitySession(workspace)
	if err != nil {
		return nil, err
	}

	report := &executioncompat.InspectionReport{
		SchemaVersion:                 executioncompat.InspectionReportSchemaVersion,
		Scope:                         scope,
		LegacyLocalAliasEvents:        raw.localAliasEvents,
		LegacyProviderShadowEvents:    raw.providerShadowEvents,
		LegacyExecutionEventProviders: raw.executionEventProviders,
		LegacyProviderBindings:        raw.providerBindings,
		LegacyProviderSessionEvents:   raw.providerSessionEvents,
		LegacyReceiptProviders:        raw.receiptProviders,
		LegacyPolicyRoutes:            raw.policyRoutes,
	}
	for _, branchID := range branchIDs {
		lineage, err := ProjectValidatedEventsForBranch(events, tree, branchID)
		if err != nil {
			return nil, fmt.Errorf("inspect execution compatibility branch %q: %w", branchID, err)
		}
		if err := inspectCompatibilityLineage(report, branchID, lineage); err != nil {
			return nil, err
		}
		if session != nil && branchID == tree.ActiveBranch {
			if err := inspectSessionOnlyCompatibilityTasks(report, branchID, lineage, session); err != nil {
				return nil, err
			}
		}
	}
	sortCompatibilityFindings(report.Findings)
	return report, nil
}

func compatibilityBranches(tree *SessionTree, requested string) ([]string, string, error) {
	if tree == nil || len(tree.Branches) == 0 {
		return nil, "", fmt.Errorf("inspect execution compatibility: session tree has no branches")
	}
	if requested != "" {
		branch := tree.GetBranch(requested)
		if branch == nil {
			return nil, "", fmt.Errorf("inspect execution compatibility: branch %q not found", requested)
		}
		return []string{branch.ID}, "branch:" + branch.ID, nil
	}
	ids := make([]string, 0, len(tree.Branches))
	for id := range tree.Branches {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	return ids, "all", nil
}

func loadCompatibilitySession(workspace string) (*SessionData, error) {
	data, err := os.ReadFile(filepath.Join(workspace, sessionFile))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("inspect execution compatibility session: %w", err)
	}
	var session SessionData
	if err := json.Unmarshal(data, &session); err != nil {
		return nil, fmt.Errorf("inspect execution compatibility session: decode session.json: %w", err)
	}
	return &session, nil
}

type compatibilityTaskPayload struct {
	ID                string                        `json:"id"`
	Model             string                        `json:"model"`
	SubagentProvider  string                        `json:"subagent_provider"`
	ExecutionTarget   executioncompat.Target        `json:"execution_target"`
	ExecutionTopology []executioncompat.Target      `json:"execution_topology"`
	ProviderBinding   *compatibilityProviderBinding `json:"provider_binding"`
	BackendBinding    *compatibilityBackendBinding  `json:"backend_binding"`
	ExecutionReceipt  *executioncompat.Receipt      `json:"execution_receipt"`
	ExecutionReceipts []executioncompat.Receipt     `json:"execution_receipts"`
}

type compatibilityProviderBinding struct {
	Provider string `json:"provider"`
}

type compatibilityBackendBinding struct {
	Backend string `json:"backend"`
}

type compatibilityTaskSubject struct {
	input    executioncompat.TaskInput
	runID    string
	evidence []string
}

func inspectCompatibilityLineage(report *executioncompat.InspectionReport, branchID string, events []RunEvent) error {
	tasks := make(map[string]*compatibilityTaskSubject)
	for _, event := range events {
		if isCompatibilityTaskEvent(event.Type) {
			taskID, payload, err := decodeCompatibilityTask(event)
			if err != nil {
				return fmt.Errorf("inspect execution compatibility task event %q: %w", event.ID, err)
			}
			subject := tasks[taskID]
			if subject == nil {
				subject = &compatibilityTaskSubject{}
				tasks[taskID] = subject
			}
			subject.merge(payload, event)
		}
	}
	ids := make([]string, 0, len(tasks))
	for id := range tasks {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		subject := tasks[id]
		appendTaskCompatibilityFinding(report, branchID, id, subject.runID, subject.evidence, executioncompat.ClassifyTask(subject.input))
	}
	for _, event := range events {
		if EventType(event.Type) != EventExecutionPolicySnapshot {
			continue
		}
		var input executioncompat.PolicyInput
		if err := json.Unmarshal(event.Payload, &input); err != nil {
			return fmt.Errorf("inspect execution compatibility policy event %q: %w", event.ID, err)
		}
		appendPolicyCompatibilityFinding(report, branchID, event.ID, executioncompat.ClassifyPolicySnapshot(input))
	}
	return nil
}

func isCompatibilityTaskEvent(eventType string) bool {
	switch EventType(eventType) {
	case EventTaskCreated, EventTaskPlanned, EventTaskStarted, EventTaskVerifying, EventTaskPaused, EventTaskCompleted, EventTaskFailed, EventTaskBlocked, EventTaskSkipped, EventTaskProtocolIncomplete, EventTaskCancelled, EventTaskRemoved, EventTaskResolution:
		return true
	default:
		return false
	}
}

func decodeCompatibilityTask(event RunEvent) (string, compatibilityTaskPayload, error) {
	var payload compatibilityTaskPayload
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return "", compatibilityTaskPayload{}, err
	}
	taskID := strings.TrimSpace(event.TaskID)
	if taskID == "" {
		taskID = strings.TrimSpace(payload.ID)
	}
	if taskID == "" {
		return "", compatibilityTaskPayload{}, fmt.Errorf("missing task ID")
	}
	return taskID, payload, nil
}

func (subject *compatibilityTaskSubject) merge(payload compatibilityTaskPayload, event RunEvent) {
	if subject.runID == "" {
		subject.runID = event.RunID
	}
	if event.ID != "" {
		subject.evidence = append(subject.evidence, event.ID)
	}
	if subject.input.Target == (executioncompat.Target{}) && payload.ExecutionTarget != (executioncompat.Target{}) {
		subject.input.Target = payload.ExecutionTarget
	}
	if len(payload.ExecutionTopology) > 0 {
		subject.input.Topology = append(subject.input.Topology, payload.ExecutionTopology...)
	}
	if subject.input.Model == "" {
		subject.input.Model = payload.Model
	}
	if subject.input.SubagentProvider == "" {
		subject.input.SubagentProvider = payload.SubagentProvider
	}
	if payload.ProviderBinding != nil && subject.input.ProviderBinding == "" {
		subject.input.ProviderBinding = payload.ProviderBinding.Provider
	}
	if payload.BackendBinding != nil && subject.input.BackendBinding == "" {
		subject.input.BackendBinding = payload.BackendBinding.Backend
	}
	if payload.ExecutionReceipt != nil {
		subject.input.Receipts = append(subject.input.Receipts, *payload.ExecutionReceipt)
	}
	subject.input.Receipts = append(subject.input.Receipts, payload.ExecutionReceipts...)
}

func inspectSessionOnlyCompatibilityTasks(report *executioncompat.InspectionReport, branchID string, events []RunEvent, session *SessionData) error {
	eventTaskIDs := make(map[string]struct{})
	for _, event := range events {
		if isCompatibilityTaskEvent(event.Type) && event.TaskID != "" {
			eventTaskIDs[event.TaskID] = struct{}{}
		}
	}
	for _, task := range session.Tasks {
		if task == nil || strings.TrimSpace(task.ID) == "" {
			continue
		}
		if _, exists := eventTaskIDs[task.ID]; exists {
			continue
		}
		input := compatibilityInputFromTodo(task)
		appendTaskCompatibilityFinding(report, branchID, task.ID, "", nil, executioncompat.ClassifyTask(input))
	}
	return nil
}

func compatibilityInputFromTodo(task *TodoItem) executioncompat.TaskInput {
	input := executioncompat.TaskInput{
		Target:           executioncompat.Target{Backend: task.ExecutionTarget.Backend, Model: task.ExecutionTarget.Model},
		Model:            task.Model,
		SubagentProvider: task.SubagentProvider,
	}
	for _, target := range task.ExecutionTopology {
		input.Topology = append(input.Topology, executioncompat.Target{Backend: target.Backend, Model: target.Model})
	}
	if task.ProviderBinding != nil {
		input.ProviderBinding = task.ProviderBinding.Provider
	}
	if task.BackendBinding != nil {
		input.BackendBinding = task.BackendBinding.Backend
	}
	if task.ExecutionReceipt != nil {
		input.Receipts = append(input.Receipts, executioncompat.Receipt{Backend: task.ExecutionReceipt.Backend, SubagentProvider: task.ExecutionReceipt.SubagentProvider})
	}
	for _, receipt := range task.ExecutionReceipts {
		input.Receipts = append(input.Receipts, executioncompat.Receipt{Backend: receipt.Backend, SubagentProvider: receipt.SubagentProvider})
	}
	return input
}

func appendTaskCompatibilityFinding(report *executioncompat.InspectionReport, branchID, taskID, runID string, evidence []string, result executioncompat.ClassificationResult) {
	switch result.Classification {
	case executioncompat.ClassificationCanonical:
		report.CanonicalTasks++
	case executioncompat.ClassificationMigrated:
		report.MigratedTasks++
	case executioncompat.ClassificationMigratable:
		report.MigratableTasks++
	case executioncompat.ClassificationAmbiguous:
		report.AmbiguousTasks++
	case executioncompat.ClassificationUnmigratable:
		report.UnmigratableTasks++
	default:
		return
	}
	if result.Classification == executioncompat.ClassificationCanonical {
		return
	}
	report.Findings = append(report.Findings, executioncompat.Finding{SubjectKind: "task", BranchID: branchID, TaskID: taskID, RunID: runID, Classification: result.Classification, Features: result.Features, ReasonCode: result.ReasonCode, EvidenceEventIDs: sortedUniqueStrings(evidence)})
}

func appendPolicyCompatibilityFinding(report *executioncompat.InspectionReport, branchID, sourceEventID string, result executioncompat.ClassificationResult) {
	switch result.Classification {
	case executioncompat.ClassificationCanonical:
		report.CanonicalPolicySnapshots++
	case executioncompat.ClassificationMigrated:
		report.MigratedPolicySnapshots++
	case executioncompat.ClassificationMigratable:
		report.MigratablePolicySnapshots++
	case executioncompat.ClassificationAmbiguous:
		report.AmbiguousPolicySnapshots++
	case executioncompat.ClassificationUnmigratable:
		report.UnmigratablePolicySnapshots++
	}
	if result.Classification == executioncompat.ClassificationCanonical {
		return
	}
	report.Findings = append(report.Findings, executioncompat.Finding{SubjectKind: "policy_snapshot", BranchID: branchID, SourceEventID: sourceEventID, Classification: result.Classification, Features: result.Features, ReasonCode: result.ReasonCode, EvidenceEventIDs: []string{sourceEventID}})
}

func sortedUniqueStrings(values []string) []string {
	if len(values) == 0 {
		return nil
	}
	set := make(map[string]struct{}, len(values))
	for _, value := range values {
		if value != "" {
			set[value] = struct{}{}
		}
	}
	result := make([]string, 0, len(set))
	for value := range set {
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func sortCompatibilityFindings(findings []executioncompat.Finding) {
	sort.Slice(findings, func(i, j int) bool {
		left, right := findings[i], findings[j]
		if left.SubjectKind != right.SubjectKind {
			return left.SubjectKind < right.SubjectKind
		}
		if left.BranchID != right.BranchID {
			return left.BranchID < right.BranchID
		}
		if left.TaskID != right.TaskID {
			return left.TaskID < right.TaskID
		}
		if left.RunID != right.RunID {
			return left.RunID < right.RunID
		}
		return left.SourceEventID < right.SourceEventID
	})
}

type eventCompatibilityCounts struct {
	localAliasEvents, providerShadowEvents, providerBindings, providerSessionEvents, receiptProviders, policyRoutes, executionEventProviders int
}

func (counts *eventCompatibilityCounts) observeEvent(event RunEvent) {
	if EventType(event.Type) == EventProviderSessionBound {
		counts.providerSessionEvents++
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal(event.Payload, &payload) != nil {
		return
	}
	if hasLegacyLocalAlias(payload) {
		counts.localAliasEvents++
	}
	if hasAnyKey(payload, "model", "model_topology", "subagent_provider", "provider_binding") {
		counts.providerShadowEvents++
	}
	counts.providerBindings += countNonEmptyProviderBindings(payload)
	counts.receiptProviders += countLegacyReceiptProviders(payload)
	counts.policyRoutes += countLegacyPolicyRoutes(payload)
}

func (counts *eventCompatibilityCounts) observeExecutionEventProviders(workspace string) error {
	path := filepath.Join(workspace, logsDir, executionEventsFile)
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("inspect execution compatibility execution events: %w", err)
	}
	defer func() { _ = file.Close() }()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		var record map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			return fmt.Errorf("inspect execution compatibility execution events: decode JSONL: %w", err)
		}
		if raw := record["provider"]; nonEmptyJSONString(raw) {
			counts.executionEventProviders++
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("inspect execution compatibility execution events: scan JSONL: %w", err)
	}
	return nil
}

func hasLegacyLocalAlias(value map[string]json.RawMessage) bool {
	for key, raw := range value {
		if key == "backend" {
			var backend string
			if json.Unmarshal(raw, &backend) == nil && executioncompat.IsLegacyLocalAlias(backend) {
				return true
			}
		}
		var nested map[string]json.RawMessage
		if json.Unmarshal(raw, &nested) == nil && hasLegacyLocalAlias(nested) {
			return true
		}
		var items []map[string]json.RawMessage
		if json.Unmarshal(raw, &items) == nil {
			for _, item := range items {
				if hasLegacyLocalAlias(item) {
					return true
				}
			}
		}
	}
	return false
}

func hasAnyKey(value map[string]json.RawMessage, keys ...string) bool {
	for _, key := range keys {
		if _, ok := value[key]; ok {
			return true
		}
	}
	return false
}

func countNonEmptyProviderBindings(value map[string]json.RawMessage) int {
	raw := value["provider_binding"]
	var binding compatibilityProviderBinding
	if json.Unmarshal(raw, &binding) == nil && strings.TrimSpace(binding.Provider) != "" {
		return 1
	}
	return 0
}

func countLegacyReceiptProviders(value map[string]json.RawMessage) int {
	count := 0
	var single executioncompat.Receipt
	if json.Unmarshal(value["execution_receipt"], &single) == nil && strings.TrimSpace(single.SubagentProvider) != "" {
		count++
	}
	var many []executioncompat.Receipt
	if json.Unmarshal(value["execution_receipts"], &many) == nil {
		for _, receipt := range many {
			if strings.TrimSpace(receipt.SubagentProvider) != "" {
				count++
			}
		}
	}
	return count
}

func countLegacyPolicyRoutes(value map[string]json.RawMessage) int {
	var policy executioncompat.PolicyInput
	if json.Unmarshal(marshalCompatibilityRawObject(value), &policy) != nil {
		return 0
	}
	count := 0
	for _, route := range policy.Routes {
		if strings.TrimSpace(route.LegacyProvider) != "" {
			count++
		}
	}
	return count
}

func marshalCompatibilityRawObject(value map[string]json.RawMessage) []byte {
	data, _ := json.Marshal(value)
	return data
}

func nonEmptyJSONString(raw json.RawMessage) bool {
	var value string
	return json.Unmarshal(raw, &value) == nil && strings.TrimSpace(value) != ""
}
