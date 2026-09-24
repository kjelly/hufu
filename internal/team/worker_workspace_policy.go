package team

import (
	"fmt"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/execution"
)

// WorkerWorkspacePolicy is the worker-workspace decision frozen into a task
// occurrence. EffectiveMode is what the task actually runs with: a read-only
// task of an isolated agent runs shared, because it writes nothing.
type WorkerWorkspacePolicy struct {
	Mode          agent.WorkerWorkspaceMode `json:"mode"`
	Integrate     string                    `json:"integrate,omitempty"`
	EffectiveMode agent.WorkerWorkspaceMode `json:"effective_mode"`
}

func (p *WorkerWorkspacePolicy) clone() *WorkerWorkspacePolicy {
	if p == nil {
		return nil
	}
	clone := *p
	return &clone
}

// isolated reports whether the occurrence runs in an isolated attempt world.
func (p *WorkerWorkspacePolicy) isolated() bool {
	return p != nil && p.EffectiveMode == agent.WorkerWorkspaceIsolated
}

// isolatedWorkerForbiddenTools are tools whose filesystem effects cannot be
// rebound to an attempt world: terminals start in the canonical root, lua
// and golang change the process-wide working directory, scp resolves local
// paths against the process cwd, and sudo escalates past every tool policy.
var isolatedWorkerForbiddenTools = []string{
	"sudo", "scp", "lua", "golang",
	"terminal", "terminal_start", "terminal_write", "terminal_wait", "terminal_close", "terminal_reconcile",
}

// workerWorkspaceSpecFor resolves an agent's worker-workspace setting: its
// own frontmatter first, then the team default. Coordinators never run in a
// worker workspace.
func workerWorkspaceSpecFor(session *TeamSession, def *agent.AgentDef) *agent.WorkerWorkspaceSpec {
	if def == nil {
		return nil
	}
	if role := strings.ToLower(strings.TrimSpace(def.Role)); role == "coordinator" || role == "orchestrator" {
		return nil
	}
	if def.WorkerWorkspace != nil {
		return def.WorkerWorkspace
	}
	if session != nil {
		return session.Config.WorkerWorkspace
	}
	return nil
}

// validateTeamWorkerWorkspaces rejects, at team load, every agent whose
// isolated setting the runtime cannot honor.
func validateTeamWorkerWorkspaces(session *TeamSession) error {
	if session == nil {
		return nil
	}
	if err := session.Config.WorkerWorkspace.Validate(); err != nil {
		return fmt.Errorf("team: %w", err)
	}
	for _, def := range uniqueSessionAgentDefs(session) {
		if role := strings.ToLower(strings.TrimSpace(def.Role)); (role == "coordinator" || role == "orchestrator") && def.WorkerWorkspace.Isolated() {
			return fmt.Errorf("%s: agent %q: worker-workspace applies only to workers, not %s agents", workspaceIsolationUnsupportedCode, def.Name, role)
		}
		if err := def.WorkerWorkspace.Validate(); err != nil {
			return fmt.Errorf("agent %q: %w", def.Name, err)
		}
		spec := workerWorkspaceSpecFor(session, def)
		if !spec.Isolated() {
			continue
		}
		if err := isolatedAgentUnsupportedReason(session, def); err != "" {
			return fmt.Errorf("%s: agent %q: %s", workspaceIsolationUnsupportedCode, def.Name, err)
		}
	}
	return nil
}

func isolatedAgentUnsupportedReason(session *TeamSession, def *agent.AgentDef) string {
	switch {
	case len(def.ExtraModels) > 0:
		return "isolated workspaces cannot be combined with extra-models"
	case agentUsesExternalBackend(session, def):
		return "isolated workspaces are not available for external agent backends"
	case len(def.MCPTools) > 0:
		return "isolated workspaces cannot be combined with MCP tools, whose effects cannot be confined to the attempt world"
	case len(session.Config.Workflow.Phases) > 0:
		return "isolated workspaces cannot be combined with a phase workflow"
	}
	for _, tool := range strings.Split(def.Tools, ",") {
		name := strings.ToLower(strings.TrimSpace(tool))
		if name == "all" {
			return "isolated workspaces cannot be combined with tools: all"
		}
		for _, forbidden := range isolatedWorkerForbiddenTools {
			if name == forbidden {
				return fmt.Sprintf("isolated workspaces cannot be combined with the %s tool", name)
			}
		}
	}
	return ""
}

// admittedWorkerWorkspace freezes an occurrence's worker-workspace policy.
// It returns nil for the shared default and an error when an isolated agent's
// task cannot run isolated. It runs after side-effect and target resolution.
func (c *Coordinator) admittedWorkerWorkspace(task TaskDef, def *agent.AgentDef) (*WorkerWorkspacePolicy, error) {
	if c == nil || c.session == nil || task.Sidecar {
		return nil, nil
	}
	spec := workerWorkspaceSpecFor(c.session, def)
	if !spec.Isolated() {
		return nil, nil
	}
	policy := &WorkerWorkspacePolicy{Mode: spec.Mode, Integrate: spec.Integrate, EffectiveMode: agent.WorkerWorkspaceIsolated}
	switch task.SideEffect {
	case "", SideEffectNone:
		policy.EffectiveMode = agent.WorkerWorkspaceShared
		return policy, nil
	case SideEffectWorkspaceWrite:
	default:
		return nil, fmt.Errorf("%s: task side effect %q cannot be contained by an isolated workspace", workspaceIsolationUnsupportedCode, task.SideEffect)
	}
	if !task.ResolvedExecutionTarget.IsZero() {
		if backend, err := c.ExecutionRegistry().ResolveBackend(task.ResolvedExecutionTarget.Backend); err == nil && backend.Kind() == execution.BackendKindAgent {
			return nil, fmt.Errorf("%s: backend %q is an external agent backend", workspaceIsolationUnsupportedCode, task.ResolvedExecutionTarget.Backend)
		}
	}
	if len(task.Execution.Steps) > 0 || task.Action != nil {
		return nil, fmt.Errorf("%s: structured steps and actions run in the canonical project and cannot be isolated", workspaceIsolationUnsupportedCode)
	}
	if len(task.ModelTopology) > 1 || len(task.ExecutionTopology) > 1 {
		return nil, fmt.Errorf("%s: extra-model fan-out cannot be isolated", workspaceIsolationUnsupportedCode)
	}
	if err := c.requireIsolatedWorkspaceScope(); err != nil {
		return nil, err
	}
	return policy, nil
}

// requireIsolatedWorkspaceScope requires a managed workspace whose control
// root and project do not overlap, since attempt worlds live under the
// control root and must never be observed as project files.
func (c *Coordinator) requireIsolatedWorkspaceScope() error {
	scope := c.session.Scope
	if !scope.Managed {
		return fmt.Errorf("%s: isolated workspaces require a managed workspace (see hufu workspace)", workspaceIsolationUnsupportedCode)
	}
	control, subject := strings.TrimSpace(scope.ControlRoot), strings.TrimSpace(scope.SubjectRoot)
	if control == "" || subject == "" || isWithinRoot(subject, control) || isWithinRoot(control, subject) {
		return fmt.Errorf("%s: the control root and the project overlap", workspaceIsolationUnsupportedCode)
	}
	return nil
}

// requireWorkerWorkspaceIsolationAvailable refuses a team that configures
// isolated workspaces while attempts are not yet run in isolated worlds, so
// an isolated setting is never silently ignored.
func requireWorkerWorkspaceIsolationAvailable(session *TeamSession) error {
	if session == nil {
		return nil
	}
	for _, def := range uniqueSessionAgentDefs(session) {
		if workerWorkspaceSpecFor(session, def).Isolated() {
			return fmt.Errorf("%s: agent %q configures an isolated worker workspace, which this build does not run yet", workspaceIsolationUnsupportedCode, def.Name)
		}
	}
	return nil
}

// isolatedResolvedToolsUnsupported checks an isolated task's resolved tool
// surface, which can include tools the agent file does not name (team MCP
// servers, profile grants).
func isolatedResolvedToolsUnsupported(resolved ResolvedWorkerTools) error {
	if len(resolved.DynamicTargets) > 0 {
		return fmt.Errorf("%s: MCP tools cannot be confined to an isolated attempt world", workspaceIsolationUnsupportedCode)
	}
	for _, name := range resolved.AuthorizedNames {
		for _, forbidden := range isolatedWorkerForbiddenTools {
			if strings.EqualFold(name, forbidden) {
				return fmt.Errorf("%s: the %s tool cannot be confined to an isolated attempt world", workspaceIsolationUnsupportedCode, name)
			}
		}
	}
	return nil
}
