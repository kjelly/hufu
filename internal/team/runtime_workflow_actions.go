package team

import (
	"fmt"
	"path/filepath"
	"slices"
	"strings"
)

// phaseAllowsAction keeps static validation, catalog discovery, and execution
// on the same boundary. Read-only verification providers may inspect accepted
// evidence in VERIFY; mutation remains confined to EXECUTE.
func phaseAllowsAction(phase Phase, sideEffect string) bool {
	return phase == PhaseExecute || (phase == PhasePrepare || phase == PhaseVerify) && strings.EqualFold(strings.TrimSpace(sideEffect), string(SideEffectNone))
}

// ActionsEnabled reports whether this workflow can run structured actions:
// either its phase workflow is enabled, or a team without phases declares an
// action catalog. Phase dispatch, validation, and gating still follow
// Enabled.
func (w *runtimeWorkflow) ActionsEnabled() bool {
	return w != nil && (w.enabled || w.actionsEnabled)
}

// runtimeWorkspace returns the action runtime workspace. Unlike
// executionContext it does not require the phase workflow, so catalog
// actions in a dynamic team can allocate staging directories and receipts.
func (w *runtimeWorkflow) runtimeWorkspace() RuntimeWorkspace {
	if !w.ActionsEnabled() {
		return RuntimeWorkspace{}
	}
	w.mu.RLock()
	defer w.mu.RUnlock()
	return w.workspace
}

// enableCatalogActions prepares the action runtime for a team without phases
// whose action catalog is non-empty. It never enables phase dispatch.
func (w *runtimeWorkflow) enableCatalogActions(session *TeamSession) error {
	root := filepath.Join(session.Workspace, "runtime")
	if err := ensureRuntimeWorkspace(root); err != nil {
		return err
	}
	w.actionsEnabled = true
	w.team = session.Config.Name
	w.repositoryRoot = session.Dir
	if strings.TrimSpace(w.repositoryRoot) == "" {
		w.repositoryRoot = session.Workspace
	}
	if strings.TrimSpace(w.repositoryRoot) == "" {
		w.repositoryRoot = "."
	}
	w.workspace = RuntimeWorkspace{Root: root}
	w.registry = session.ProviderRegistry
	w.retryPolicy = session.Config.Retry
	return nil
}

// runtimeActionEventPhase is the phase recorded on an action lifecycle event:
// the workflow state, or "" for a catalog action in a team without phases.
func runtimeActionEventPhase(w *runtimeWorkflow) string {
	if !w.Enabled() {
		return ""
	}
	return string(w.State())
}

// validateCatalogTaskLocked admits a catalog task into the current phase:
// every entry in EXECUTE, only side-effect-free entries in PREPARE or VERIFY. Catalog
// tasks are not phase contracts, so the phase agent, static contract, and
// dispatch-once checks do not apply. The caller holds w.mu.
func (w *runtimeWorkflow) validateCatalogTaskLocked(task TaskDef) error {
	if task.Phase != w.state {
		return fmt.Errorf("workflow phase %s only accepts %s tasks; catalog action %q is bound to %s", w.state, w.state, task.CatalogAction.ActionID, task.Phase)
	}
	if phaseAllowsAction(w.state, string(task.SideEffect)) {
		return nil
	}
	return fmt.Errorf("catalog action %q (%s) cannot run in workflow phase %s", task.CatalogAction.ActionID, task.SideEffect, w.state)
}

// catalogWorkflowAgents returns the executing agents of the catalog entries
// the current phase may dispatch, for the agent tool's agent enum.
func (c *Coordinator) catalogWorkflowAgents() []string {
	if c == nil || c.session == nil || c.session.ActionCatalog == nil || c.phaseWorkflow == nil || !c.phaseWorkflow.Enabled() {
		return nil
	}
	var agents []string
	for _, entry := range c.session.ActionCatalog.Entries {
		if _, ok := c.catalogDispatchPhase(entry); ok && !slices.Contains(agents, entry.Agent) {
			agents = append(agents, entry.Agent)
		}
	}
	return agents
}

// unionAgentEnum adds names to the enum of an agent schema property,
// keeping it sorted and unique.
func unionAgentEnum(property any, names []string) {
	agentProperty, ok := property.(map[string]any)
	if !ok || len(names) == 0 {
		return
	}
	current, _ := agentProperty["enum"].([]string)
	merged := slices.Clone(current)
	for _, name := range names {
		if !slices.Contains(merged, name) {
			merged = append(merged, name)
		}
	}
	slices.Sort(merged)
	agentProperty["enum"] = merged
}
