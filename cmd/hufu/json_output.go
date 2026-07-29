package main

import (
	"encoding/json"
	"os"
	"sort"

	"github.com/anomalyco/hufu/internal/team"
)

// jsonRunOutput is the machine-readable shape emitted by --output json.
type jsonRunOutput struct {
	Outcome         string                 `json:"outcome"`
	GoalSatisfied   bool                   `json:"goal_satisfied"`
	Result          string                 `json:"result"`
	Reason          string                 `json:"reason,omitempty"`
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

func aggregateOutcomes(outcomes []team.RunOutcome) team.RunOutcome {
	if len(outcomes) == 0 {
		return team.RunOutcomeCompleted
	}
	hasFailed := false
	hasBlocked := false
	hasPartial := false
	hasCancelled := false
	for _, o := range outcomes {
		switch o {
		case team.RunOutcomeFailed:
			hasFailed = true
		case team.RunOutcomeBlocked:
			hasBlocked = true
		case team.RunOutcomePartial:
			hasPartial = true
		case team.RunOutcomeCancelled:
			hasCancelled = true
		}
	}
	if hasFailed {
		return team.RunOutcomeFailed
	}
	if hasBlocked {
		return team.RunOutcomeBlocked
	}
	if hasPartial {
		return team.RunOutcomePartial
	}
	if hasCancelled {
		return team.RunOutcomeCancelled
	}
	return team.RunOutcomeCompleted
}

// printResultJSON writes the run result, per-team task/token data and skill
// usage to stdout as a single JSON object, for scripting and piping.
func printResultJSON(result string, loadedTeams map[string]*teamContext, skills []team.SkillUsageEntry) error {
	out := jsonRunOutput{Result: result}

	names := make([]string, 0, len(loadedTeams))
	for name := range loadedTeams {
		names = append(names, name)
	}
	sort.Strings(names)

	var teamOutcomes []team.RunOutcome
	var allItems []*team.TodoItem

	for _, name := range names {
		tc := loadedTeams[name]
		if tc == nil || tc.coordinator == nil {
			continue
		}
		if lastRes := tc.coordinator.LastRunResult(); lastRes != nil {
			teamOutcomes = append(teamOutcomes, lastRes.Outcome)
			if out.Reason == "" && lastRes.Reason != "" {
				out.Reason = lastRes.Reason
			}
			if out.ExitCode == 0 && lastRes.ExitCode != 0 {
				out.ExitCode = lastRes.ExitCode
			}
			if lastRes.Acceptance != nil {
				acceptance := *lastRes.Acceptance
				acceptance.State = acceptance.EffectiveState()
				if out.Acceptance == nil || acceptance.State != team.AcceptancePassed {
					out.Acceptance = &acceptance
				}
			}
			out.UnresolvedTasks = append(out.UnresolvedTasks, lastRes.UnresolvedTasks...)
		}
		jt := jsonRunTeam{Name: name, Tokens: tc.coordinator.TokensUsed()}
		var items []*team.TodoItem
		if tracker := tc.coordinator.TaskTracker(); tracker != nil && tracker.TodoList() != nil {
			items = tracker.TodoList().Items()
		}
		allItems = append(allItems, items...)
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

	topOutcome := aggregateOutcomes(teamOutcomes)
	out.Stats = team.SummarizeRunStats(allItems)
	if out.Stats.TasksUnresolved > 0 && topOutcome == team.RunOutcomeCompleted {
		topOutcome = team.RunOutcomePartial
	}
	out.Outcome = string(topOutcome)
	out.GoalSatisfied = topOutcome == team.RunOutcomeCompleted

	for _, s := range skills {
		out.Skills = append(out.Skills, jsonRunSkill{Name: s.Name, Count: s.Count, Agents: s.Agents})
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
