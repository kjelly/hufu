package team

// Dry-run planning: predicting delegations without executing them.

import (
	"context"
	"strings"

	"github.com/anomalyco/hufu/internal/config"
)

type DryRunAgentInfo struct {
	Name   string
	Role   string
	Model  string
	Tools  []string
	Skills []string
}

type DryRunSkillInfo struct {
	Name        string
	Description string
}

type DryRunResult struct {
	UserPrompt         string
	TeamName           string
	Model              string
	SidecarModel       string
	Agents             []DryRunAgentInfo
	AllSkills          []DryRunSkillInfo
	MatchedSkillNames  []string
	OrchestratorPrompt string
	FirstRoundTasks    []TaskDef
	Error              string
}

func (c *Coordinator) DryRun(ctx context.Context, userPrompt string) (*DryRunResult, error) {
	orchDef := c.GetOrchestratorDef()

	_ = EnsureWorkspaceDirs(c.session.Workspace)

	result := &DryRunResult{
		UserPrompt: userPrompt,
	}
	if c.session != nil && c.session.Config.Name != "" {
		result.TeamName = c.session.Config.Name
	}
	if orchDef != nil {
		result.Model = c.resolveAgentModel(orchDef, "")
	}

	if c.session != nil {
		if c.session.Config.SidecarModel != "" {
			result.SidecarModel = c.session.Config.SidecarModel
		}
		if result.SidecarModel == "" {
			if resolved := config.LoadConfig(); resolved != nil {
				result.SidecarModel = resolved.ResolveSidecarModel(c.session.Config.SidecarModel)
			}
		}
	}

	// Skill matching: keyword-only, no LLM, no sidecar.
	allSkills := c.getSkills()
	matchedSet := map[string]bool{}
	for _, sk := range allSkills {
		if strings.Contains(strings.ToLower(userPrompt), strings.ToLower(sk.Name)) || SkillMatchesPrompt(sk, userPrompt) {
			matchedSet[strings.ToLower(sk.Name)] = true
		}
		result.AllSkills = append(result.AllSkills, DryRunSkillInfo{
			Name:        sk.Name,
			Description: sk.Description,
		})
	}
	for _, sk := range allSkills {
		if matchedSet[strings.ToLower(sk.Name)] {
			result.MatchedSkillNames = append(result.MatchedSkillNames, sk.Name)
		}
	}

	// Agent listing: derived from session config, not from an LLM.
	// Dedupe by def.Name so any agent registered under multiple map keys
	// (e.g. legacy aliases) still appears only once in the dry-run output.
	if c.session != nil {
		seenAgents := map[string]bool{}
		for _, def := range c.session.Agents {
			if def == nil {
				continue
			}
			if seenAgents[def.Name] {
				continue
			}
			seenAgents[def.Name] = true
			role := def.Role
			if role == "" {
				role = "worker"
			}
			model := c.resolveAgentModel(def, "")
			var tools []string
			if def.Tools != "" {
				tools = strings.Split(def.Tools, ",")
				for i, t := range tools {
					tools[i] = strings.TrimSpace(t)
				}
			}
			var skills []string
			if def.Skills != "" {
				skills = strings.Split(def.Skills, ",")
				for i, s := range skills {
					skills[i] = strings.TrimSpace(s)
				}
			}
			if role == "coordinator" || role == "orchestrator" {
				tools = []string{"agent", "finish", "load_skill", "save_skill", "ask_user"}
			}
			result.Agents = append(result.Agents, DryRunAgentInfo{
				Name:   def.Name,
				Role:   role,
				Model:  model,
				Tools:  tools,
				Skills: skills,
			})
		}
	}

	c.report(c.newEvent("done").withAgent("coordinator").withMessage("dry-run complete (no LLM calls)").withTodoID(CoordTodoID))

	return result, nil
}

func cloneTaskDef(td TaskDef) TaskDef {
	clone := td
	if td.ContextFiles != nil {
		clone.ContextFiles = make([]string, len(td.ContextFiles))
		copy(clone.ContextFiles, td.ContextFiles)
	}
	if td.DependsOn != nil {
		clone.DependsOn = make([]int, len(td.DependsOn))
		copy(clone.DependsOn, td.DependsOn)
	}
	if td.Requires != nil {
		clone.Requires = make([]string, len(td.Requires))
		copy(clone.Requires, td.Requires)
	}
	return clone
}
