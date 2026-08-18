package team

import (
	"strings"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
)

var legacyMemoryMutationTools = map[string]bool{
	"stm_write":   true,
	"ltm_update":  true,
	"memory_save": true,
}

func isLegacyMemoryMutationTool(name string) bool {
	return legacyMemoryMutationTools[strings.TrimSpace(name)]
}

// explicitlyDeclaresTool accepts only an exact comma-separated literal.
// Empty and "all" deliberately do not grant deprecated mutation authority.
func explicitlyDeclaresTool(raw, want string) bool {
	if strings.TrimSpace(raw) == "" || strings.TrimSpace(raw) == "all" {
		return false
	}
	for _, name := range strings.Split(raw, ",") {
		if strings.TrimSpace(name) == want {
			return true
		}
	}
	return false
}

func (c *Coordinator) legacyMemoryToolGranted(def *agent.AgentDef, name string) bool {
	if !isLegacyMemoryMutationTool(name) || c == nil || c.session == nil || c.toolDeniedByTeam(name) {
		return false
	}
	for _, allowed := range c.session.Config.ToolsAllowed {
		if strings.TrimSpace(allowed) == name {
			return true
		}
	}
	return def != nil && explicitlyDeclaresTool(def.Tools, name)
}

func (c *Coordinator) filterLegacyMemoryMutationTools(def *agent.AgentDef, candidate []fantasy.AgentTool) []fantasy.AgentTool {
	filtered := make([]fantasy.AgentTool, 0, len(candidate))
	seen := make(map[string]bool, len(candidate))
	for _, tool := range candidate {
		if tool == nil {
			continue
		}
		name := tool.Info().Name
		if isLegacyMemoryMutationTool(name) && !c.legacyMemoryToolGranted(def, name) {
			continue
		}
		seen[name] = true
		filtered = append(filtered, tool)
	}
	// A team-level literal grant applies to every worker even when an agent's
	// own list is constrained. Registration stays global; exposure is decided
	// here per invocation.
	if c != nil {
		for _, tool := range c.coreTools {
			if tool == nil {
				continue
			}
			name := tool.Info().Name
			if !seen[name] && isLegacyMemoryMutationTool(name) && c.legacyMemoryToolGranted(def, name) {
				seen[name] = true
				filtered = append(filtered, tool)
			}
		}
	}
	return filtered
}

// selectWorkerTools is the single worker-facing tool boundary. Team-level
// denials take precedence over both agent frontmatter and alwaysIncludeTools,
// so a collaboration/memory convenience cannot bypass a read-only contract.
func (c *Coordinator) selectWorkerTools(def *agent.AgentDef) []fantasy.AgentTool {
	if def == nil {
		return nil
	}
	return c.filterDeniedWorkerTools(c.filterLegacyMemoryMutationTools(def, agent.SelectTools(c.coreTools, def.Tools)))
}

// selectWorkerToolsForTask preserves the team-wide deny list except for a
// tool explicitly granted by a goal-selected static template. The caller must
// pass the runtime-assigned contract ID: an arbitrary task execution object is
// never enough to bypass a team denial.
func (c *Coordinator) selectWorkerToolsForTask(def *agent.AgentDef, task TaskDef) []fantasy.AgentTool {
	if def == nil {
		return nil
	}
	return c.filterDeniedWorkerToolsWithGrants(c.filterLegacyMemoryMutationTools(def, agent.SelectTools(c.coreTools, def.Tools)), c.taskToolGrants(def, task))
}

// taskToolGrants returns only capabilities authorized by a trusted static task
// contract. Workflow phases normally hide execution tools outside EXECUTE;
// a bounded contract such as a PREPARE inventory may explicitly require bash
// to produce a run-scoped artifact. The coordinator cannot forge this grant:
// the contract ID and agent must match the loaded team definition, and the
// granted names come from that definition rather than model-authored input.
func (c *Coordinator) taskToolGrants(def *agent.AgentDef, task TaskDef) map[string]bool {
	grants := make(map[string]bool)
	if c == nil || c.session == nil || strings.TrimSpace(task.ContractID) == "" {
		return grants
	}
	for _, contract := range c.session.ContractTasks {
		if contract.ID != task.ContractID || !strings.EqualFold(strings.TrimSpace(contract.Agent), strings.TrimSpace(task.Agent)) {
			continue
		}
		// Only a loaded, agent-bound static contract may grant a capability
		// during PREPARE/AUDIT/VERIFY. A model-authored ContractID or template
		// grant is otherwise just untrusted task input and must not bypass the
		// phase gate.
		for _, name := range contract.Execution.TemplateToolGrants {
			name = strings.TrimSpace(name)
			if name != "" && agentDeclaresTool(def, name) {
				grants[name] = true
			}
		}
		break
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
		if denied[name] {
			continue
		}
		if c.phaseWorkflow != nil && c.phaseWorkflow.Enabled() && c.phaseWorkflow.State() != PhaseExecute {
			if executionCapabilityTools[name] && !grants[name] {
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
		if denied[name] {
			continue
		}
		if c.phaseWorkflow != nil && c.phaseWorkflow.Enabled() && c.phaseWorkflow.State() != PhaseExecute {
			if executionCapabilityTools[name] && !grants[name] {
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

func (c *Coordinator) toolDeniedByTeam(name string) bool {
	if c == nil || c.session == nil {
		return false
	}
	for _, denied := range c.session.Config.ToolsDenied {
		if strings.TrimSpace(denied) == strings.TrimSpace(name) {
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
	"fetch":              true,
	"agentic_fetch":      true,
	"terminal":           true,
	"terminal_start":     true,
	"terminal_write":     true,
	"terminal_read":      true,
	"terminal_wait":      true,
	"terminal_close":     true,
	"terminal_list":      true,
	"terminal_reconcile": true,
}
