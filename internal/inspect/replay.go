package inspect

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/team"
)

const maxReplayDiffPaths = 200

type ReplayData struct {
	RunID         string            `json:"run_id"`
	EventChain    string            `json:"event_chain"`
	Checks        []ProjectionCheck `json:"checks"`
	OverallStatus string            `json:"overall_status"`
}

func InspectReplay(ctx context.Context, query InspectQuery) (*Envelope, error) {
	if err := query.Validate(KindReplay); err != nil {
		return nil, err
	}
	lineage, err := LoadLineage(ctx, query)
	if err != nil {
		return nil, err
	}
	selected, err := selectRun(lineage, query)
	if err != nil {
		return nil, err
	}
	checks := make([]ProjectionCheck, 0, 5)
	checks = append(checks, replaySessionChecks(query, lineage, selected)...)
	decisionCheck, err := replayDecisionCheck(ctx, query, selected)
	if err != nil {
		return nil, err
	}
	checks = append(checks, decisionCheck)
	memoryCheck := replayMemoryCheck(ctx, query.Workspace, lineage.GlobalEvents)
	checks = append(checks, memoryCheck)
	terminalCheck, err := replayTerminalCheck(query, selected.runEvents)
	if err != nil {
		return nil, err
	}
	checks = append(checks, terminalCheck)
	data := ReplayData{RunID: query.RunID, EventChain: "verified", Checks: checks, OverallStatus: replayOverallStatus(checks)}
	envelope := envelope(KindReplay, query, lineage.BranchID, data)
	switch data.OverallStatus {
	case "drift":
		envelope.Integrity.Projection = "drift"
	case "unavailable":
		envelope.Integrity.Projection = "unavailable"
	}
	return envelope, nil
}

type sessionProjectionReaders struct {
	loadSession func(string) (*team.SessionData, bool, error)
	loadTree    func(string) (*team.SessionTree, error)
}

func replaySessionChecks(query InspectQuery, lineage Lineage, selected selectedRun) []ProjectionCheck {
	return replaySessionChecksWithReaders(query, lineage, selected, sessionProjectionReaders{
		loadSession: team.LoadSessionReadOnly,
		loadTree:    team.LoadSessionTree,
	})
}

func replaySessionChecksWithReaders(query InspectQuery, lineage Lineage, selected selectedRun, readers sessionProjectionReaders) []ProjectionCheck {
	runCheck := ProjectionCheck{Name: "session.run", Status: "unavailable"}
	taskCheck := ProjectionCheck{Name: "session.tasks", Status: "unavailable"}
	reason := sessionProjectionGate(lineage, selected)
	if reason != "" {
		runCheck.ReasonCode, taskCheck.ReasonCode = reason, reason
		return []ProjectionCheck{runCheck, taskCheck}
	}
	checkpoint, exists, err := readers.loadSession(query.Workspace)
	secondTree, treeErr := readers.loadTree(query.Workspace)
	if treeErr != nil || secondTree.ActiveBranch != lineage.ActiveBranchID || secondTree.ActiveBranch != lineage.BranchID {
		runCheck.ReasonCode, taskCheck.ReasonCode = ReasonProjectionChangedOnRead, ReasonProjectionChangedOnRead
		return []ProjectionCheck{runCheck, taskCheck}
	}
	if err != nil {
		runCheck.ReasonCode, taskCheck.ReasonCode = ReasonProjectionUnreadable, ReasonProjectionUnreadable
		return []ProjectionCheck{runCheck, taskCheck}
	}
	if !exists || checkpoint == nil {
		runCheck.ReasonCode, taskCheck.ReasonCode = ReasonOptionalProjectionAbsent, ReasonOptionalProjectionAbsent
		return []ProjectionCheck{runCheck, taskCheck}
	}
	canonical := team.ReduceToSessionData(selected.raw[:selected.terminalIndex+1])
	runDiffs := compareRunProjection(canonical.RunResult, checkpoint.RunResult)
	taskDiffs := compareTaskProjections(canonical.Tasks, checkpoint.Tasks)
	runCheck = projectionCheck("session.run", runDiffs, selected.terminal.ID)
	taskCheck = projectionCheck("session.tasks", taskDiffs, selected.terminal.ID)
	return []ProjectionCheck{runCheck, taskCheck}
}

func sessionProjectionGate(lineage Lineage, selected selectedRun) string {
	if lineage.BranchID == "" || lineage.BranchID != lineage.ActiveBranchID || selected.terminal == nil || selected.terminalIndex < 0 {
		return ReasonProjectionNotRunScoped
	}
	terminalSeen := false
	for _, indexed := range lineage.Events {
		if indexed.Event.ID == selected.terminal.ID {
			terminalSeen = true
			continue
		}
		if terminalSeen && indexed.Event.RunID != "" && indexed.Event.RunID != selected.terminal.RunID {
			return ReasonProjectionNotRunScoped
		}
	}
	if !terminalSeen {
		return ReasonProjectionNotRunScoped
	}
	return ""
}

func projectionCheck(name string, diffPaths []string, refs ...string) ProjectionCheck {
	check := ProjectionCheck{Name: name, Status: "match", ComparedRefs: normalizeOpaqueRefs(refs), DiffPaths: []string{}}
	if len(diffPaths) > 0 {
		check.Status = "drift"
		check.DiffPaths = boundedDiffPaths(diffPaths)
	}
	return check
}

func compareRunProjection(expected, actual *team.RunResult) []string {
	if expected == nil && actual == nil {
		return nil
	}
	if expected == nil || actual == nil {
		return []string{"run_result"}
	}
	var diffs []string
	compareField(&diffs, "run_result.run_id", expected.RunID, actual.RunID)
	compareField(&diffs, "run_result.outcome", expected.Outcome, actual.Outcome)
	compareField(&diffs, "run_result.goal_satisfied", expected.GoalSatisfied, actual.GoalSatisfied)
	compareField(&diffs, "run_result.stop_reason", expected.StopReason, actual.StopReason)
	compareField(&diffs, "run_result.acceptance", acceptanceState(expected), acceptanceState(actual))
	compareField(&diffs, "run_result.evidence_manifest.hash", manifestHash(expected), manifestHash(actual))
	compareField(&diffs, "run_result.evidence_manifest.status", manifestStatus(expected), manifestStatus(actual))
	return diffs
}

func acceptanceState(result *team.RunResult) team.AcceptanceState {
	if result == nil || result.Acceptance == nil {
		return team.AcceptanceNotConfigured
	}
	return result.Acceptance.EffectiveState()
}

func manifestHash(result *team.RunResult) string {
	if result == nil || result.EvidenceManifest == nil {
		return ""
	}
	return result.EvidenceManifest.ManifestHash
}

func manifestStatus(result *team.RunResult) string {
	if result == nil || result.EvidenceManifest == nil {
		return ""
	}
	return result.EvidenceManifest.Status
}

type replayTaskProjection struct {
	Status            team.TaskStatus
	Phase             team.Phase
	Agent             string
	ExecutionTarget   string
	ExecutionTopology []string
	RecoveryState     string
	Receipts          []string
}

func compareTaskProjections(expected, actual []*team.TodoItem) []string {
	expectedByID, expectedIDs := replayTasksByID(expected)
	actualByID, actualIDs := replayTasksByID(actual)
	ids := normalizeRefs(append(expectedIDs, actualIDs...))
	var diffs []string
	for index, id := range ids {
		left, leftOK := expectedByID[id]
		right, rightOK := actualByID[id]
		prefix := fmt.Sprintf("tasks[%d]", index)
		if !leftOK || !rightOK {
			diffs = append(diffs, prefix)
			continue
		}
		compareField(&diffs, prefix+".status", left.Status, right.Status)
		compareField(&diffs, prefix+".phase", left.Phase, right.Phase)
		compareField(&diffs, prefix+".agent", left.Agent, right.Agent)
		compareField(&diffs, prefix+".execution_target", left.ExecutionTarget, right.ExecutionTarget)
		compareField(&diffs, prefix+".execution_topology", left.ExecutionTopology, right.ExecutionTopology)
		compareField(&diffs, prefix+".recovery_state", left.RecoveryState, right.RecoveryState)
		compareField(&diffs, prefix+".execution_receipts", left.Receipts, right.Receipts)
	}
	return diffs
}

func replayTasksByID(items []*team.TodoItem) (map[string]replayTaskProjection, []string) {
	byID := make(map[string]replayTaskProjection, len(items))
	ids := make([]string, 0, len(items))
	for _, item := range items {
		if item == nil || item.ID == "" {
			continue
		}
		topology := make([]string, 0, len(item.ExecutionTopology))
		for _, target := range item.ExecutionTopology {
			topology = append(topology, target.String())
		}
		receipts := make([]string, 0, len(item.ExecutionReceipts))
		for _, receipt := range item.ExecutionReceipts {
			exitCode := "unavailable"
			if receipt.ExitCode != nil {
				exitCode = fmt.Sprintf("%d", *receipt.ExitCode)
			}
			verification := "not_run"
			if receipt.VerifyResult != nil {
				verification = fmt.Sprintf("%d:%s", receipt.VerifyResult.ExitCode, safeOpaqueRef(receipt.VerifyResult.Fingerprint))
			}
			receipts = append(receipts, fmt.Sprintf("%d:%s:%s:%s:%s:%s", receipt.Attempt, safeOpaqueRef(receipt.ModelExecutionID), safeOpaqueRef(receipt.ProducerID), boundedCode(receipt.Backend), exitCode, verification))
		}
		slices.Sort(receipts)
		byID[item.ID] = replayTaskProjection{Status: item.Status, Phase: item.Phase, Agent: item.Agent, ExecutionTarget: item.ExecutionTarget.String(), ExecutionTopology: topology, RecoveryState: item.RecoveryState, Receipts: receipts}
		ids = append(ids, item.ID)
	}
	return byID, ids
}

func replayDecisionCheck(ctx context.Context, query InspectQuery, selected selectedRun) (ProjectionCheck, error) {
	expected, err := team.ProjectDecisionEntriesForLineage(ctx, selected.runEvents)
	if err != nil {
		return ProjectionCheck{}, fmt.Errorf("%w: replay decision lineage: %v", ErrIntegrity, err)
	}
	actual, err := team.LoadDecisionIndexEntriesReadOnly(query.Workspace)
	if err != nil {
		return ProjectionCheck{Name: "decision_index", Status: "unavailable", ReasonCode: ReasonProjectionUnreadable}, nil
	}
	actual = filterDecisionEntries(actual, query.RunID)
	if len(expected) == 0 && len(actual) == 0 {
		return ProjectionCheck{Name: "decision_index", Status: "skipped", ReasonCode: ReasonOptionalProjectionAbsent}, nil
	}
	return projectionCheck("decision_index", compareDecisionEntries(expected, actual)), nil
}

func filterDecisionEntries(entries []team.DecisionIndexEntry, runID string) []team.DecisionIndexEntry {
	out := make([]team.DecisionIndexEntry, 0, len(entries))
	for _, entry := range entries {
		if entry.RunID == runID {
			out = append(out, entry)
		}
	}
	return out
}

func compareDecisionEntries(expected, actual []team.DecisionIndexEntry) []string {
	left := make(map[string]team.DecisionIndexEntry, len(expected))
	right := make(map[string]team.DecisionIndexEntry, len(actual))
	var ids []string
	for _, entry := range expected {
		left[entry.DecisionID], ids = entry, append(ids, entry.DecisionID)
	}
	for _, entry := range actual {
		right[entry.DecisionID], ids = entry, append(ids, entry.DecisionID)
	}
	ids = normalizeRefs(ids)
	var diffs []string
	for index, id := range ids {
		leftEntry, leftOK := left[id]
		rightEntry, rightOK := right[id]
		prefix := fmt.Sprintf("decisions[%d]", index)
		if !leftOK || !rightOK {
			diffs = append(diffs, prefix)
			continue
		}
		compareField(&diffs, prefix+".run_id", leftEntry.RunID, rightEntry.RunID)
		compareField(&diffs, prefix+".task_id", leftEntry.TaskID, rightEntry.TaskID)
		compareField(&diffs, prefix+".record_ref", safeArtifactMetadata(leftEntry.EffectiveRecordRef()), safeArtifactMetadata(rightEntry.EffectiveRecordRef()))
		compareField(&diffs, prefix+".finalization_result_ref", safeArtifactPointer(leftEntry.FinalizationResultRef), safeArtifactPointer(rightEntry.FinalizationResultRef))
	}
	return diffs
}

func safeArtifactPointer(ref *team.ArtifactRef) *ArtifactMetadata {
	if ref == nil {
		return nil
	}
	safe := safeArtifactMetadata(*ref)
	return &safe
}

func replayMemoryCheck(ctx context.Context, workspace string, indexed []IndexedEvent) ProjectionCheck {
	events := make([]team.RunEvent, len(indexed))
	for index := range indexed {
		events[index] = indexed[index].Event
	}
	observations := team.ExperienceObservationsFromEvents(events, agent.DefaultMemoryLearningPolicy())
	if len(observations) == 0 {
		return ProjectionCheck{Name: "memory_aggregates", Status: "skipped", ReasonCode: ReasonOptionalProjectionAbsent}
	}
	expected, err := contextstore.ReduceExperienceAggregates(observations)
	if err != nil {
		return ProjectionCheck{Name: "memory_aggregates", Status: "unavailable", ReasonCode: ReasonLegacySchemaUnsupported}
	}
	repo, err := contextstore.OpenSQLiteReadOnly(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ProjectionCheck{Name: "memory_aggregates", Status: "unavailable", ReasonCode: ReasonOptionalProjectionAbsent}
		}
		return ProjectionCheck{Name: "memory_aggregates", Status: "unavailable", ReasonCode: ReasonProjectionUnreadable}
	}
	defer func() { _ = repo.Close() }()
	actual, err := loadExperienceAggregates(ctx, repo, expected)
	if err != nil {
		return ProjectionCheck{Name: "memory_aggregates", Status: "unavailable", ReasonCode: ReasonProjectionUnreadable}
	}
	return projectionCheck("memory_aggregates", compareExperienceAggregates(expected, actual))
}

func loadExperienceAggregates(ctx context.Context, repo contextstore.ReadOnlyRepository, expected []contextstore.ExperienceAggregate) ([]contextstore.ExperienceAggregate, error) {
	policies := make([]string, 0)
	for _, aggregate := range expected {
		policies = append(policies, aggregate.PolicyVersion)
	}
	policies = normalizeRefs(policies)
	var actual []contextstore.ExperienceAggregate
	for _, policy := range policies {
		items, err := repo.ListExperienceAggregates(ctx, policy)
		if err != nil {
			return nil, err
		}
		actual = append(actual, items...)
	}
	return actual, nil
}

func compareExperienceAggregates(expected, actual []contextstore.ExperienceAggregate) []string {
	left := make(map[string]contextstore.ExperienceAggregate, len(expected))
	right := make(map[string]contextstore.ExperienceAggregate, len(actual))
	var keys []string
	for _, aggregate := range expected {
		key := aggregate.PolicyVersion + "/" + aggregate.ContextItemID
		left[key], keys = aggregate, append(keys, key)
	}
	for _, aggregate := range actual {
		key := aggregate.PolicyVersion + "/" + aggregate.ContextItemID
		right[key], keys = aggregate, append(keys, key)
	}
	keys = normalizeRefs(keys)
	var diffs []string
	for index, key := range keys {
		leftAggregate, leftOK := left[key]
		rightAggregate, rightOK := right[key]
		prefix := fmt.Sprintf("memory[%d]", index)
		if !leftOK || !rightOK {
			diffs = append(diffs, prefix)
			continue
		}
		compareField(&diffs, prefix+".positive_weight", leftAggregate.PositiveWeight, rightAggregate.PositiveWeight)
		compareField(&diffs, prefix+".negative_weight", leftAggregate.NegativeWeight, rightAggregate.NegativeWeight)
		compareField(&diffs, prefix+".exposure_count", leftAggregate.ExposureCount, rightAggregate.ExposureCount)
		compareField(&diffs, prefix+".consulted_count", leftAggregate.ConsultedCount, rightAggregate.ConsultedCount)
		compareField(&diffs, prefix+".applied_count", leftAggregate.AppliedCount, rightAggregate.AppliedCount)
		compareField(&diffs, prefix+".rejected_count", leftAggregate.RejectedCount, rightAggregate.RejectedCount)
		compareField(&diffs, prefix+".verified_support_count", leftAggregate.VerifiedSupportCount, rightAggregate.VerifiedSupportCount)
		compareField(&diffs, prefix+".causal_failure_count", leftAggregate.CausalFailureCount, rightAggregate.CausalFailureCount)
		compareField(&diffs, prefix+".independent_task_count", leftAggregate.IndependentTaskCount, rightAggregate.IndependentTaskCount)
		compareField(&diffs, prefix+".independent_project_count", leftAggregate.IndependentProjectCount, rightAggregate.IndependentProjectCount)
		if math.Abs(leftAggregate.UtilityLowerBound-rightAggregate.UtilityLowerBound) > 1e-12 {
			diffs = append(diffs, prefix+".utility_lower_bound")
		}
		compareField(&diffs, prefix+".last_observed_at", leftAggregate.LastObservedAt, rightAggregate.LastObservedAt)
		compareField(&diffs, prefix+".revision", leftAggregate.Revision, rightAggregate.Revision)
	}
	return diffs
}

type replayTerminalProjection struct {
	RunID            string
	OwnerTaskID      string
	ControllerTaskID string
	Agent            string
	State            team.TerminalSessionState
	OutputRefs       []string
}

func replayTerminalCheck(query InspectQuery, events []team.RunEvent) (ProjectionCheck, error) {
	expected, projectionErr := terminalProjectionFromEvents(events)
	if projectionErr != nil {
		return ProjectionCheck{}, fmt.Errorf("%w: replay terminal lineage: %v", ErrIntegrity, projectionErr)
	}
	actualFacts, err := LoadTerminalFacts(query)
	if err != nil {
		return ProjectionCheck{Name: "terminal_sessions", Status: "unavailable", ReasonCode: ReasonProjectionUnreadable}, nil
	}
	actual := make(map[string]replayTerminalProjection, len(actualFacts))
	for _, fact := range actualFacts {
		refs := make([]string, 0, len(fact.OutputRefs)*2)
		for _, ref := range fact.OutputRefs {
			refs = append(refs, ref.ID, ref.Digest)
		}
		actual[fact.SessionID] = replayTerminalProjection{RunID: fact.RunID, OwnerTaskID: fact.OwnerTaskID, ControllerTaskID: fact.ControllerTaskID, Agent: fact.Agent, State: fact.State, OutputRefs: normalizeOpaqueRefs(refs)}
	}
	if len(expected) == 0 && len(actual) == 0 {
		return ProjectionCheck{Name: "terminal_sessions", Status: "skipped", ReasonCode: ReasonOptionalProjectionAbsent}, nil
	}
	return projectionCheck("terminal_sessions", compareTerminalProjections(expected, actual)), nil
}

func terminalProjectionFromEvents(events []team.RunEvent) (map[string]replayTerminalProjection, error) {
	out := make(map[string]replayTerminalProjection)
	for _, event := range events {
		if !strings.HasPrefix(event.Type, "terminal_") {
			continue
		}
		var payload struct {
			SessionID        string                    `json:"session_id"`
			RunID            string                    `json:"run_id"`
			OwnerTaskID      string                    `json:"owner_task_id"`
			ControllerTaskID string                    `json:"controller_task_id"`
			Agent            string                    `json:"agent"`
			State            team.TerminalSessionState `json:"state"`
			OutputRefs       []team.ArtifactRef        `json:"output_refs"`
		}
		if err := jsonUnmarshalMetadata(event.Payload, &payload); err != nil {
			return nil, fmt.Errorf("terminal event %q payload is invalid", event.ID)
		}
		if payload.SessionID == "" {
			return nil, fmt.Errorf("terminal event %q has no session identity", event.ID)
		}
		refs := make([]string, 0, len(payload.OutputRefs)*2)
		for _, ref := range payload.OutputRefs {
			refs = append(refs, ref.ID, ref.SHA256)
		}
		out[payload.SessionID] = replayTerminalProjection{RunID: payload.RunID, OwnerTaskID: payload.OwnerTaskID, ControllerTaskID: payload.ControllerTaskID, Agent: payload.Agent, State: payload.State, OutputRefs: normalizeOpaqueRefs(refs)}
	}
	return out, nil
}

func compareTerminalProjections(expected, actual map[string]replayTerminalProjection) []string {
	ids := make([]string, 0, len(expected)+len(actual))
	for id := range expected {
		ids = append(ids, id)
	}
	for id := range actual {
		ids = append(ids, id)
	}
	ids = normalizeRefs(ids)
	var diffs []string
	for index, id := range ids {
		left, leftOK := expected[id]
		right, rightOK := actual[id]
		prefix := fmt.Sprintf("terminals[%d]", index)
		if !leftOK || !rightOK {
			diffs = append(diffs, prefix)
			continue
		}
		compareField(&diffs, prefix+".run_id", left.RunID, right.RunID)
		compareField(&diffs, prefix+".owner_task_id", left.OwnerTaskID, right.OwnerTaskID)
		compareField(&diffs, prefix+".controller_task_id", left.ControllerTaskID, right.ControllerTaskID)
		compareField(&diffs, prefix+".agent", left.Agent, right.Agent)
		compareField(&diffs, prefix+".state", left.State, right.State)
		compareField(&diffs, prefix+".output_refs", left.OutputRefs, right.OutputRefs)
	}
	return diffs
}

func compareField[T any](diffs *[]string, path string, expected, actual T) {
	if !reflect.DeepEqual(expected, actual) {
		*diffs = append(*diffs, path)
	}
}

func boundedDiffPaths(paths []string) []string {
	slices.Sort(paths)
	paths = slices.Compact(paths)
	if len(paths) > maxReplayDiffPaths {
		paths = paths[:maxReplayDiffPaths]
	}
	return paths
}

func replayOverallStatus(checks []ProjectionCheck) string {
	hasUnavailable := false
	for _, check := range checks {
		switch check.Status {
		case "drift":
			return "drift"
		case "unavailable":
			hasUnavailable = true
		}
	}
	if hasUnavailable {
		return "unavailable"
	}
	return "match"
}

func jsonUnmarshalMetadata(data []byte, destination any) error {
	if len(data) == 0 {
		return fmt.Errorf("empty metadata")
	}
	return json.Unmarshal(data, destination)
}
