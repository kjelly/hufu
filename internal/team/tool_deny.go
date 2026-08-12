package team

import (
	"strings"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
)

// selectWorkerTools is the single worker-facing tool boundary. Team-level
// denials take precedence over both agent frontmatter and alwaysIncludeTools,
// so a collaboration/memory convenience cannot bypass a read-only contract.
func (c *Coordinator) selectWorkerTools(def *agent.AgentDef) []fantasy.AgentTool {
	if def == nil {
		return nil
	}
	return c.filterDeniedWorkerTools(agent.SelectTools(c.coreTools, def.Tools))
}

// selectWorkerToolsForTask preserves the team-wide deny list except for a
// tool explicitly granted by a goal-selected static template. The caller must
// pass the runtime-assigned contract ID: an arbitrary task execution object is
// never enough to bypass a team denial.
func (c *Coordinator) selectWorkerToolsForTask(def *agent.AgentDef, task TaskDef) []fantasy.AgentTool {
	if def == nil {
		return nil
	}
	return c.filterDeniedWorkerToolsWithGrants(agent.SelectTools(c.coreTools, def.Tools), templateGrantedToolNames(def, task))
}

// templateGrantedToolNames returns only a runtime-selected template's narrow
// grants. A model-supplied execution object has no ContractID and therefore
// cannot bypass the team deny policy.
func templateGrantedToolNames(def *agent.AgentDef, task TaskDef) map[string]bool {
	grants := make(map[string]bool, len(task.Execution.TemplateToolGrants))
	if def == nil || task.ContractID == "" {
		return grants
	}
	for _, name := range task.Execution.TemplateToolGrants {
		name = strings.TrimSpace(name)
		if name != "" && agentDeclaresTool(def, name) {
			grants[name] = true
		}
	}
	return grants
}

func agentDeclaresTool(def *agent.AgentDef, want string) bool {
	for _, name := range strings.Split(def.Tools, ",") {
		if strings.TrimSpace(name) == want {
			return true
		}
	}
	return false
}

// filterDeniedWorkerTools removes denied tools before they reach a worker
// model. Result-protocol tools are appended by the caller after this filter so
// a team cannot accidentally remove the only terminal reporting mechanism.
func (c *Coordinator) filterDeniedWorkerTools(candidate []fantasy.AgentTool) []fantasy.AgentTool {
	return c.filterDeniedWorkerToolsWithGrants(candidate, nil)
}

func (c *Coordinator) filterDeniedWorkerToolsWithGrants(candidate []fantasy.AgentTool, grants map[string]bool) []fantasy.AgentTool {
	if c == nil || c.session == nil {
		return candidate
	}
	denied := make(map[string]bool, len(c.session.Config.ToolsDenied))
	for _, name := range c.session.Config.ToolsDenied {
		if name = strings.TrimSpace(name); name != "" {
			denied[name] = true
		}
	}

	filtered := make([]fantasy.AgentTool, 0, len(candidate))
	for _, tool := range candidate {
		if tool == nil {
			continue
		}
		name := tool.Info().Name
		if denied[name] && !grants[name] {
			continue
		}
		if c.phaseWorkflow != nil && c.phaseWorkflow.Enabled() && c.phaseWorkflow.State() != PhaseExecute {
			if executionCapabilityTools[name] {
				continue
			}
		}
		filtered = append(filtered, tool)
	}
	return filtered
}

func (c *Coordinator) filterDeniedToolNamesWithGrants(candidate []string, grants map[string]bool) []string {
	if c == nil || c.session == nil {
		return candidate
	}
	denied := make(map[string]bool, len(c.session.Config.ToolsDenied))
	for _, name := range c.session.Config.ToolsDenied {
		if name = strings.TrimSpace(name); name != "" {
			denied[name] = true
		}
	}

	filtered := make([]string, 0, len(candidate))
	for _, name := range candidate {
		name = strings.TrimSpace(name)
		if name == "" {
			continue
		}
		if denied[name] && !grants[name] {
			continue
		}
		if c.phaseWorkflow != nil && c.phaseWorkflow.Enabled() && c.phaseWorkflow.State() != PhaseExecute {
			if executionCapabilityTools[name] {
				continue
			}
		}
		filtered = append(filtered, name)
	}
	return filtered
}

// filterDeniedCoordinatorTools applies the same team-level deny boundary to
// the orchestrator as to workers. Memory/collaboration conveniences are
// otherwise injected by buildOrchestratorTools after the worker filter and
// can bypass a read-only or no-memory team contract during wrap-up.
func (c *Coordinator) filterDeniedCoordinatorTools(candidate []fantasy.AgentTool) []fantasy.AgentTool {
	if c == nil || c.session == nil {
		return candidate
	}
	denied := make(map[string]bool, len(c.session.Config.ToolsDenied))
	for _, name := range c.session.Config.ToolsDenied {
		if name = strings.TrimSpace(name); name != "" {
			denied[name] = true
		}
	}

	filtered := make([]fantasy.AgentTool, 0, len(candidate))
	for _, tool := range candidate {
		if tool == nil {
			continue
		}
		name := tool.Info().Name
		if denied[name] {
			continue
		}
		if c.phaseWorkflow != nil && c.phaseWorkflow.Enabled() && c.phaseWorkflow.State() != PhaseExecute {
			if executionCapabilityTools[name] {
				continue
			}
		}
		filtered = append(filtered, tool)
	}
	return filtered
}

func (c *Coordinator) coordinatorToolDenied(name string) bool {
	if c == nil || c.session == nil {
		return false
	}
	name = strings.TrimSpace(name)
	for _, denied := range c.session.Config.ToolsDenied {
		if strings.TrimSpace(denied) == name {
			return true
		}
	}

	if c.phaseWorkflow != nil && c.phaseWorkflow.Enabled() && c.phaseWorkflow.State() != PhaseExecute {
		if executionCapabilityTools[name] {
			return true
		}
	}

	return false
}

var executionCapabilityTools = map[string]bool{
	"bash":               true,
	"sudo":               true,
	"ssh":                true,
	"write":              true,
	"edit":               true,
	"multiedit":          true,
	"golang":             true,
	"lua":                true,
	"scp":                true,
	"create_skill":       true,
	"wait_for":           true,
	"download":           true,
	"terminal":           true,
	"terminal_start":     true,
	"terminal_write":     true,
	"terminal_read":      true,
	"terminal_wait":      true,
	"terminal_close":     true,
	"terminal_list":      true,
	"terminal_reconcile": true,
}
