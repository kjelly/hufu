package main

import (
	"encoding/json"
	"os"
	"sort"
	"time"

	"github.com/anomalyco/hufu/internal/team"
)

// jsonRunOutput is the machine-readable shape emitted by --output json.
type jsonRunOutput struct {
	Outcome         string                 `json:"outcome"`
	GoalSatisfied   bool                   `json:"goal_satisfied"`
	GoalMode        string                 `json:"goal_mode,omitempty"`
	Result          string                 `json:"result"`
	Reason          string                 `json:"reason,omitempty"`
	StopReason      string                 `json:"stop_reason,omitempty"`
	ExitCode        int                    `json:"exit_code,omitempty"`
	Acceptance      *team.AcceptanceResult `json:"acceptance,omitempty"`
	UnresolvedTasks []team.TaskReference   `json:"unresolved_tasks,omitempty"`
	Stats           team.RunStats          `json:"stats"`
	Teams           []jsonRunTeam          `json:"teams"`
	Skills          []jsonRunSkill         `json:"skills,omitempty"`
}

type jsonRunTeam struct {
	Name   string        `json:"name"`
	Tokens int64         `json:"tokens"`
	Tasks  []jsonRunTask `json:"tasks,omitempty"`
}

type jsonRunTask struct {
	ID     string `json:"id"`
	Agent  string `json:"agent"`
	Desc   string `json:"desc"`
	Status string `json:"status"`
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
		jt := jsonRunTeam{Name: name, Tokens: tc.coordinator.TokensUsed()}
		var items []*team.TodoItem
		if tracker := tc.coordinator.TaskTracker(); tracker != nil && tracker.TodoList() != nil {
			items = tracker.TodoList().Items()
		}
		allItems = append(allItems, items...)
		for _, item := range items {
			if item == nil || !isHistoricalUnresolvedTask(name, item, priorUnresolved) {
				currentItems = append(currentItems, item)
			}
		}
		for _, it := range items {
			jt.Tasks = append(jt.Tasks, jsonRunTask{
				ID:     it.ID,
				Agent:  it.Agent,
				Desc:   it.Desc,
				Status: string(it.Status),
			})
		}
		out.Teams = append(out.Teams, jt)
	}

	out.UnresolvedTasks = appendUniqueTaskReferences(out.UnresolvedTasks, team.UnresolvedTaskReferences(currentItems))
	out.Stats = team.SummarizeRunStats(allItems)
	canonical := team.AggregateRunResults(runResults, out.UnresolvedTasks, out.Stats)
	out.Stats = canonical.Stats
	out.Outcome = string(canonical.Outcome)
	out.GoalSatisfied = canonical.GoalSatisfied
	out.GoalMode = string(canonical.GoalMode)
	out.Reason = canonical.Reason
	out.StopReason = string(canonical.StopReason)
	out.ExitCode = canonical.ExitCode
	if canonical.Acceptance != nil {
		acceptance := *canonical.Acceptance
		acceptance.State = acceptance.EffectiveState()
		out.Acceptance = &acceptance
	}

	for _, s := range skills {
		out.Skills = append(out.Skills, jsonRunSkill{Name: s.Name, Count: s.Count, Agents: s.Agents})
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
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
	seen := make(map[string]struct{}, len(existing)+len(additional))
	key := func(ref team.TaskReference) string { return ref.ID + "\x00" + ref.Status }
	for _, ref := range existing {
		seen[key(ref)] = struct{}{}
	}
	for _, ref := range additional {
		if _, ok := seen[key(ref)]; ok {
			continue
		}
		seen[key(ref)] = struct{}{}
		existing = append(existing, ref)
	}
	return existing
}
