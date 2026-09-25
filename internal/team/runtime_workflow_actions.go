package team

import (
	"path/filepath"
	"strings"
)

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
