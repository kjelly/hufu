package main

import (
	"encoding/json"
	"os"
	"slices"
	"sort"
	"time"

	"github.com/kjelly/hufu/internal/team"
	"github.com/kjelly/hufu/internal/utils"
)

// jsonRunOutput is the machine-readable shape emitted by --output json.
type jsonRunOutput struct {
	Outcome             string                     `json:"outcome"`
	GoalSatisfied       bool                       `json:"goal_satisfied"`
	GoalMode            string                     `json:"goal_mode,omitempty"`
	Result              string                     `json:"result"`
	Reason              string                     `json:"reason,omitempty"`
	StopReason          string                     `json:"stop_reason,omitempty"`
	RecoveryDisposition team.RetryDisposition      `json:"recovery_disposition,omitempty"`
	ExitCode            int                        `json:"exit_code,omitempty"`
	Acceptance          *team.AcceptanceResult     `json:"acceptance,omitempty"`
	Worksets            []team.WorksetGroupState   `json:"worksets,omitempty"`
	CompletedReview     bool                       `json:"completed_review,omitempty"`
	FindingsPresent     bool                       `json:"findings_present,omitempty"`
	FixedAndVerified    bool                       `json:"fixed_and_verified,omitempty"`
	AcceptanceAdvisory  bool                       `json:"acceptance_advisory,omitempty"`
	UnresolvedTasks     []team.TaskReference       `json:"unresolved_tasks,omitempty"`
	Stats               team.RunStats              `json:"stats"`
	Metrics             team.RunMetrics            `json:"metrics,omitempty"`
	Teams               []jsonRunTeam              `json:"teams"`
	Skills              []jsonRunSkill             `json:"skills,omitempty"`
	Failures            []team.FailureEventPayload `json:"failures,omitempty"`
}

type jsonRunTeam struct {
	Name                 string                            `json:"name"`
	Tokens               int64                             `json:"tokens"`
	Tasks                []jsonRunTask                     `json:"tasks,omitempty"`
	MemoryLearning       team.MemoryLearningReport         `json:"memory_learning,omitempty"`
	DeprecatedMemory     []team.DeprecatedMemoryToolUsage  `json:"deprecated_memory_tools,omitempty"`
	ContextRouting       team.ContextManifestSummary       `json:"context_routing"`
	Decisions            []team.DecisionIndexEntry         `json:"decisions,omitempty"`
	RunInputs            *jsonRunInputs                    `json:"run_inputs,omitempty"`
	InputBoundAssertions []team.InputBoundAssertionSummary `json:"input_bound_assertions,omitempty"`
}

type jsonRunInputs struct {
	SnapshotID   string                  `json:"snapshot_id"`
	SnapshotHash string                  `json:"snapshot_hash"`
	Inputs       []team.ResolvedRunInput `json:"inputs"`
}

type jsonRunTask struct {
	ID                  string                          `json:"id"`
	Agent               string                          `json:"agent"`
	Desc                string                          `json:"desc"`
	Status              string                          `json:"status"`
	NoObjectiveVerifier bool                            `json:"no_objective_verifier,omitempty"`
	CompletedReview     bool                            `json:"completed_review,omitempty"`
	FindingsPresent     bool                            `json:"findings_present,omitempty"`
	ResourceScope       *team.TaskResourceScopeSnapshot `json:"resource_scope,omitempty"`
	RetryDisposition    team.RetryDisposition           `json:"retry_disposition,omitempty"`
	NextAction          string                          `json:"next_action,omitempty"`
}

type jsonRunSkill struct {
	Name   string   `json:"name"`
	Count  int      `json:"count"`
	Agents []string `json:"agents"`
}

// printResultJSON writes the run result, per-team task/token data and skill
// usage to stdout as a single JSON object, for scripting and piping.
func printResultJSON(result string, loadedTeams map[string]*teamContext, skills []team.SkillUsageEntry) error {
	return printResultJSONWithPrior(result, loadedTeams, skills, nil)
}

func printResultJSONWithPrior(result string, loadedTeams map[string]*teamContext, skills []team.SkillUsageEntry, priorUnresolved map[string]map[string]time.Time) error {
	out := jsonRunOutput{Result: result}

	names := make([]string, 0, len(loadedTeams))
	for name := range loadedTeams {
		names = append(names, name)
	}
	sort.Strings(names)

	var runResults []*team.RunResult
	var allItems []*team.TodoItem
	var currentItems []*team.TodoItem

	for _, name := range names {
		tc := loadedTeams[name]
		if tc == nil || tc.coordinator == nil {
			continue
		}
		if lastRes := tc.coordinator.LastRunResult(); lastRes != nil {
			runResults = append(runResults, lastRes)
			out.UnresolvedTasks = append(out.UnresolvedTasks, lastRes.UnresolvedTasks...)
		}
		jt := jsonRunTeam{Name: name, Tokens: tc.coordinator.TokensUsed(), MemoryLearning: tc.coordinator.MemoryLearningReport(), DeprecatedMemory: tc.coordinator.DeprecatedMemoryToolReport(), ContextRouting: tc.coordinator.ContextManifestReport()}
		if lastRes := tc.coordinator.LastRunResult(); lastRes != nil {
			jt.RunInputs = redactedJSONRunInputs(lastRes.RunInputs)
			jt.InputBoundAssertions = slices.Clone(lastRes.InputBoundAssertions)
		}
		jt.Decisions, _ = tc.coordinator.DecisionIndexEntries()
		jt.Decisions = team.RedactedDecisionIndexEntries(jt.Decisions)
		var items []*team.TodoItem
		if tracker := tc.coordinator.TaskTracker(); tracker != nil && tracker.TodoList() != nil {
			items = tracker.TodoList().Items()
		}
		if lastRes := tc.coordinator.LastRunResult(); lastRes != nil && lastRes.EvidenceManifest != nil && lastRes.EvidenceManifest.RunID != "" {
			items, _ = latestRunTodos(items, lastRes.EvidenceManifest)
		}
		allItems = append(allItems, items...)
		for _, item := range items {
			if item == nil || !isHistoricalUnresolvedTask(name, item, priorUnresolved) {
				currentItems = append(currentItems, item)
			}
		}
		for _, it := range items {
			var retryDisposition team.RetryDisposition
			var nextAction string
			if it != nil && it.FailureEvent != nil {
				out.Failures = append(out.Failures, team.FailureEventsFromTodos([]*team.TodoItem{it})...)
				retryDisposition = it.FailureEvent.RetryDisposition
				nextAction = team.RecoveryNextAction(retryDisposition)
			}
			jt.Tasks = append(jt.Tasks, jsonRunTask{
				ID: it.ID, Agent: it.Agent, Desc: it.Desc, Status: string(it.Status),
				NoObjectiveVerifier: it.Status == team.TaskDone && it.Verify == "" && it.VerifySpec == nil,
				CompletedReview:     it.Kind == team.TaskKindDiagnostic && it.Status == team.TaskDone,
				FindingsPresent:     it.TypedResult != nil && len(it.TypedResult.Findings) > 0,
				ResourceScope:       it.ResourceScopeSnapshot,
				RetryDisposition:    retryDisposition,
				NextAction:          nextAction,
			})
		}
		out.Teams = append(out.Teams, jt)
	}

	out.UnresolvedTasks = appendUniqueTaskReferences(out.UnresolvedTasks, team.UnresolvedTaskReferences(currentItems))
	out.Stats = team.SummarizeRunStats(allItems)
	canonical := team.AggregateRunResults(runResults, out.UnresolvedTasks, out.Stats)
	out.Stats = canonical.Stats
	out.Metrics = canonical.Metrics
	out.Outcome = string(canonical.Outcome)
	out.GoalSatisfied = canonical.GoalSatisfied
	out.GoalMode = string(canonical.GoalMode)
	out.Reason = canonical.Reason
	out.StopReason = string(canonical.StopReason)
	out.RecoveryDisposition = team.RunRecoveryDisposition(canonical.UnresolvedTasks)
	out.ExitCode = canonical.ExitCode
	out.CompletedReview = canonical.CompletedReview
	out.FindingsPresent = canonical.FindingsPresent
	out.FixedAndVerified = canonical.FixedAndVerified
	out.AcceptanceAdvisory = canonical.AcceptanceAdvisory
	if canonical.Acceptance != nil {
		acceptance := *canonical.Acceptance
		acceptance.State = acceptance.EffectiveState()
		out.Acceptance = &acceptance
	}
	out.Worksets = append([]team.WorksetGroupState(nil), canonical.Worksets...)

	for _, s := range skills {
		out.Skills = append(out.Skills, jsonRunSkill{Name: s.Name, Count: s.Count, Agents: s.Agents})
	}

	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(out)
}

func redactedJSONRunInputs(snapshot *team.RunInputSnapshot) *jsonRunInputs {
	if snapshot == nil {
		return nil
	}
	result := &jsonRunInputs{SnapshotID: snapshot.ID, SnapshotHash: snapshot.SnapshotHash, Inputs: slices.Clone(snapshot.Inputs)}
	for index := range result.Inputs {
		value := result.Inputs[index].CanonicalValue
		if redacted, err := utils.RedactJSONCompact(value); err == nil {
			result.Inputs[index].CanonicalValue = redacted
		} else {
			result.Inputs[index].CanonicalValue = json.RawMessage(`"[REDACTED:invalid-json]"`)
		}
		result.Inputs[index].Evidence = slices.Clone(result.Inputs[index].Evidence)
	}
	return result
}

func isHistoricalUnresolvedTask(teamName string, item *team.TodoItem, priorUnresolved map[string]map[string]time.Time) bool {
	if item == nil || (item.Status != team.TaskError && item.Status != team.TaskBlocked) {
		return false
	}
	prior := priorUnresolved[teamName]
	endedAt, ok := prior[item.ID]
	return ok && endedAt.Equal(item.EndedAt)
}

func appendUniqueTaskReferences(existing, additional []team.TaskReference) []team.TaskReference {
	seen := make(map[string]int, len(existing)+len(additional))
	key := func(ref team.TaskReference) string { return ref.ID + "\x00" + ref.Status }
	for index, ref := range existing {
		seen[key(ref)] = index
	}
	for _, ref := range additional {
		if index, ok := seen[key(ref)]; ok {
			mergeTaskReferenceRecovery(&existing[index], ref)
			continue
		}
		seen[key(ref)] = len(existing)
		existing = append(existing, ref)
	}
	return existing
}

func mergeTaskReferenceRecovery(target *team.TaskReference, source team.TaskReference) {
	if target == nil {
		return
	}
	if target.FailureClass == "" {
		target.FailureClass = source.FailureClass
	}
	if target.RetryDisposition == "" {
		target.RetryDisposition = source.RetryDisposition
	}
	if target.NextAction == "" {
		target.NextAction = source.NextAction
	}
	if target.Error == "" {
		target.Error = source.Error
	}
}
