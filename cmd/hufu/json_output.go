package main

import (
	"encoding/json"
	"os"
	"sort"

	"github.com/anomalyco/hufu/internal/team"
)

// jsonRunOutput is the machine-readable shape emitted by --output json.
type jsonRunOutput struct {
	Result string         `json:"result"`
	Teams  []jsonRunTeam  `json:"teams"`
	Skills []jsonRunSkill `json:"skills,omitempty"`
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
	out := jsonRunOutput{Result: result}

	names := make([]string, 0, len(loadedTeams))
	for name := range loadedTeams {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		tc := loadedTeams[name]
		if tc == nil || tc.coordinator == nil {
			continue
		}
		jt := jsonRunTeam{Name: name, Tokens: tc.coordinator.TokensUsed()}
		for _, it := range tc.coordinator.TaskTracker().TodoList().Items() {
			jt.Tasks = append(jt.Tasks, jsonRunTask{
				ID:     it.ID,
				Agent:  it.Agent,
				Desc:   it.Desc,
				Status: string(it.Status),
			})
		}
		out.Teams = append(out.Teams, jt)
	}
	for _, s := range skills {
		out.Skills = append(out.Skills, jsonRunSkill{Name: s.Name, Count: s.Count, Agents: s.Agents})
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
