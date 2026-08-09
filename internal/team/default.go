package team

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/mcp"
	"github.com/kjelly/hufu/internal/skill"
)

// LoadDefaultTeam returns an in-memory TeamSession containing only the
// built-in default coordinator and Helper worker. No on-disk team files
// are required. workspace is the absolute path used for session
// persistence; the caller is responsible for creating it.
//
// The default team has no teamDir, so it discovers skills from both the
// current project (.agents/skills) and global skills under $HOME/.agents/skills/.
// forcedSkills is merged into the discovered list (missing skills print a
// warning to stderr).
//
// helperTools is a comma-separated list of extra tools appended to
// Helper's default read-only toolset (e.g. "bash" or "bash,sudo,ssh").
// Whitespace around each entry is trimmed; empty entries are dropped.
// Pass "" to keep Helper's default toolset.
func LoadDefaultTeam(workspace string, forcedSkills []string, helperTools string) (*TeamSession, error) {
	cfg := agent.TeamConfig{
		Name: "default",
		// A default runbook often needs more than ten coordinator turns just to
		// establish and verify a multi-phase environment. Explicit team/CLI
		// limits still take precedence; this avoids a needless manual resume for
		// ordinary default-team runs.
		MaxRounds: 30,
		// DefaultMaxSteps (30) is sized for text-level work. A default-team
		// worker driving infrastructure — bringing hosts up, walking a wizard,
		// waiting on services — routinely needs several times that many tool
		// calls, and a worker cut off mid-task cannot report a usable result.
		// The task deadline, no-progress budgets and anti-thrashing limits are
		// the real stopping conditions; the step cap only decides whether a task
		// can finish at all. --max-steps still overrides this.
		MaxSteps:      120,
		WorkspaceDir:  workspace,
		Timeout:       600,
		VerifyTimeout: 120,
		MaxRetries:    2,
		Generation: agent.GenerationParams{
			Temperature: agent.DefaultTemperature,
			MaxTokens:   agent.DefaultMaxTokens,
			TopP:        agent.DefaultTopP,
		},
		Reliability: agent.DefaultReliabilityConfig(),
	}
	cfg.Vars = map[string]interface{}{
		"TEAM_NAME":   "default",
		"AGENT_COUNT": "1",
		"AGENT_NAMES": "Helper",
	}

	coord := &agent.AgentDef{
		Name:        "coordinator",
		FileAlias:   "coordinator",
		Description: "Default team coordinator",
		Role:        "coordinator",
		Tools:       "ask_user",
		// Leave the per-agent field unset so the team/CLI max-steps override
		// reaches the coordinator too. A non-zero value here would take
		// precedence in stepBudget and silently defeat --max-steps.
		MaxSteps:    0,
		MaxRetries:  -1,
		Generation:  cfg.Generation,
		ProviderURL: cfg.ProviderURL,
		Memory:      agent.WorkerMemoryPolicy{Mode: agent.WorkerMemoryOff},
	}
	helperBaseTools := "view,write,edit,multiedit,grep,glob,ls,random,math"
	helperToolList := helperBaseTools
	if t := strings.TrimSpace(helperTools); t != "" {
		var extras []string
		for _, p := range strings.Split(t, ",") {
			p = strings.TrimSpace(p)
			if p != "" {
				extras = append(extras, p)
			}
		}
		if len(extras) > 0 {
			helperToolList = helperBaseTools + "," + strings.Join(extras, ",")
		}
	}
	helperToolList = agent.ExpandImpliedTools(helperToolList)
	helper := &agent.AgentDef{
		Name:        "Helper",
		FileAlias:   "helper",
		Description: "Versatile worker for text processing, string comparison, file I/O, calculations, and miscellaneous tasks",
		Role:        "worker",
		Tools:       helperToolList,
		System:      "You are a versatile helper agent. You handle text processing, string comparisons, file reading/writing, mathematical calculations via the math tool, and miscellaneous tasks that don't require specialized domain knowledge. Be thorough and precise.",
		MaxRetries:  -1,
		Generation:  cfg.Generation,
		ProviderURL: cfg.ProviderURL,
		Memory:      agent.WorkerMemoryPolicy{Mode: agent.WorkerMemoryOff},
	}

	sess := &TeamSession{
		Config:    cfg,
		Dir:       workspace,
		Workspace: workspace,
		Agents: map[string]*agent.AgentDef{
			"coordinator": coord,
			"helper":      helper,
		},
		MCPServers: map[string]mcp.MCPServerConfig{},
	}

	home, _ := os.UserHomeDir()
	skillDirs := make([]string, 0, 2)
	if cwd, err := os.Getwd(); err == nil && cwd != "" {
		skillDirs = append(skillDirs, filepath.Join(cwd, ".agents", "skills"))
	}
	skillDirs = append(skillDirs, filepath.Join(home, ".agents", "skills"))
	allSkills := skill.DiscoverSkills(skillDirs, false)
	includeSkills := skill.ParseSkillList(sess.Config.Skills)
	excludeSkills := skill.ParseSkillList(sess.Config.SkillsExclude)
	sess.Skills = skill.FilterSkills(allSkills, includeSkills, excludeSkills)

	if len(forcedSkills) > 0 {
		forcedSet := map[string]bool{}
		for _, name := range forcedSkills {
			forcedSet[strings.ToLower(strings.TrimSpace(name))] = true
		}
		existingSet := map[string]bool{}
		for _, s := range sess.Skills {
			existingSet[strings.ToLower(s.Name)] = true
		}
		knownSkills := map[string]bool{}
		for _, sk := range allSkills {
			knownSkills[strings.ToLower(sk.Name)] = true
		}
		for _, name := range forcedSkills {
			trimmed := strings.TrimSpace(name)
			if trimmed != "" && !knownSkills[strings.ToLower(trimmed)] {
				fmt.Fprintf(os.Stderr, "warning: forced skill %q not found in discovered skills\n", trimmed)
			}
		}
		for _, sk := range allSkills {
			lowerName := strings.ToLower(sk.Name)
			if forcedSet[lowerName] && !existingSet[lowerName] {
				sess.Skills = append(sess.Skills, sk)
				existingSet[lowerName] = true
			}
		}
	}
	sess.Skills = skill.ExpandSkillDependenciesForSet(sess.Skills, allSkills, excludeSkills)

	return sess, nil
}
