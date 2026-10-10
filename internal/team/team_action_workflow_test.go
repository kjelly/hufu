package team

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func workflowCatalogTask(id string, phase Phase, sideEffect SideEffectClass) TaskDef {
	return TaskDef{
		Agent: "executor", Goal: "run " + id, Phase: phase, SideEffect: sideEffect,
		Action:        &Action{Capability: "structured-actions", Type: "apply"},
		CatalogAction: &CatalogActionBinding{ActionID: id, ArgumentsHash: "sha256:" + id},
	}
}

// workflowAt returns an enabled workflow advanced to phase.
func workflowAt(t *testing.T, phase Phase) *runtimeWorkflow {
	t.Helper()
	session := workflowTestSession(t)
	w, err := newRuntimeWorkflow(session)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	steps := []TodoItem{
		{Agent: "preparer", ContractID: "prepare", Phase: PhasePrepare, Status: TaskDone},
		{Agent: "auditor", ContractID: "audit", Phase: PhaseAudit, Status: TaskDone},
		{Agent: "executor", ContractID: "execute", Phase: PhaseExecute, Status: TaskDone},
	}
	for _, step := range steps {
		if w.State() == phase {
			break
		}
		if err := w.observe([]*TodoItem{&step}); err != nil {
			t.Fatal(err)
		}
	}
	if w.State() != phase {
		t.Fatalf("workflow state = %s, want %s", w.State(), phase)
	}
	return w
}

func TestWorkflowAdmitsCatalogTasksByPhase(t *testing.T) {
	tests := []struct {
		phase      Phase
		sideEffect SideEffectClass
		want       bool
	}{
		{PhaseExecute, SideEffectNone, true},
		{PhaseExecute, SideEffectWorkspaceWrite, true},
		{PhasePrepare, SideEffectNone, true},
		{PhasePrepare, SideEffectWorkspaceWrite, false},
		{PhaseAudit, SideEffectNone, false},
		{PhaseVerify, SideEffectNone, true},
		{PhaseVerify, SideEffectWorkspaceWrite, false},
	}
	for _, tt := range tests {
		t.Run(fmt.Sprintf("%s/%s", tt.phase, tt.sideEffect), func(t *testing.T) {
			w := workflowAt(t, tt.phase)
			err := w.validateTasks([]TaskDef{workflowCatalogTask("probe", tt.phase, tt.sideEffect)})
			if (err == nil) != tt.want {
				t.Fatalf("validateTasks error = %v, want admitted %v", err, tt.want)
			}
		})
	}
	w := workflowAt(t, PhaseExecute)
	if err := w.validateTasks([]TaskDef{workflowCatalogTask("probe", PhasePrepare, SideEffectNone)}); err == nil {
		t.Fatal("a catalog task bound to another phase was admitted")
	}
}

func TestWorkflowCatalogBatchesAndStaticContracts(t *testing.T) {
	w := workflowAt(t, PhaseExecute)
	onlyCatalog := []TaskDef{workflowCatalogTask("probe", PhaseExecute, SideEffectNone), workflowCatalogTask("probe", PhaseExecute, SideEffectNone)}
	onlyCatalog[1].CatalogAction.ArgumentsHash = "sha256:other"
	if err := w.validateTasks(onlyCatalog); err != nil {
		t.Fatalf("catalog-only batch (same action, different arguments) = %v", err)
	}
	mixed := []TaskDef{workflowCatalogTask("probe", PhaseExecute, SideEffectNone), {Agent: "executor", Goal: "execute", Phase: PhaseExecute, ContractID: "other"}}
	if err := w.validateTasks(mixed); err == nil {
		t.Fatal("a mixed batch skipped the static contract checks")
	}
	withContract := []TaskDef{workflowCatalogTask("probe", PhaseExecute, SideEffectNone), {Agent: "executor", Goal: "execute", Phase: PhaseExecute, ContractID: "execute"}}
	if err := w.validateTasks(withContract); err != nil {
		t.Fatalf("catalog task alongside the phase contract = %v", err)
	}
}

func TestWorkflowPhaseIgnoresCatalogTaskFailures(t *testing.T) {
	w := workflowAt(t, PhaseExecute)
	failed := &TodoItem{Agent: "executor", Phase: PhaseExecute, Status: TaskError, Action: &Action{Capability: "structured-actions", Type: "apply"}, CatalogAction: &CatalogActionBinding{ActionID: "probe"}}
	if err := w.observe([]*TodoItem{failed}); err != nil {
		t.Fatalf("a failed catalog task failed the phase: %v", err)
	}
	if w.State() != PhaseExecute {
		t.Fatalf("state after a catalog failure = %s, want EXECUTE", w.State())
	}
	if err := w.observe([]*TodoItem{failed, {Agent: "executor", ContractID: "execute", Phase: PhaseExecute, Status: TaskDone}}); err != nil {
		t.Fatal(err)
	}
	if w.State() != PhaseVerify {
		t.Fatalf("state after the static contract = %s, want VERIFY", w.State())
	}
}

func workflowCatalogCoordinator(t *testing.T, entries int) *Coordinator {
	t.Helper()
	session := workflowTestSession(t)
	catalog := &ActionCatalogSnapshot{Version: actionCatalogSnapshotVersion}
	for i := range entries {
		catalog.Entries = append(catalog.Entries, ActionCatalogEntry{
			ID: fmt.Sprintf("action-%02d", i), Capability: "structured-actions", Type: "apply", Agent: "catalog-runner",
			SideEffect: SideEffectNone, Recovery: RecoveryRetry, MaxInvocations: 1,
		})
	}
	session.ActionCatalog = catalog
	session.Agents["catalog-runner"] = &agent.AgentDef{Name: "catalog-runner", Role: "worker"}
	return &Coordinator{session: session, taskTracker: NewTaskTracker(), sessionData: NewSession(), phaseWorkflow: workflowAt(t, PhaseExecute)}
}

func TestWorkflowAgentToolSchemaWithCatalog(t *testing.T) {
	c := workflowCatalogCoordinator(t, maxCatalogSchemaEnum)
	info := (&runAgentsTool{coordinator: c}).Info()
	properties := info.Parameters["tasks"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
	enum, _ := properties["agent"].(map[string]any)["enum"].([]string)
	if !slices.Contains(enum, "catalog-runner") || !slices.Contains(enum, "executor") || !slices.IsSorted(enum) {
		t.Fatalf("workflow agent enum = %v, want the phase worker and the catalog agent", enum)
	}
	if _, ok := properties["catalog_action"]; !ok {
		t.Fatal("workflow schema lacks catalog_action in EXECUTE")
	}
	encoded, err := json.Marshal(info.Parameters)
	if err != nil {
		t.Fatal(err)
	}
	if len(encoded) >= 16000 {
		t.Fatalf("workflow schema with %d catalog entries = %d bytes, want < 16000", maxCatalogSchemaEnum, len(encoded))
	}
	if !strings.Contains(string(encoded), `"action-31"`) {
		t.Fatal("32-entry catalog schema should still enumerate IDs")
	}

	c.phaseWorkflow = workflowAt(t, PhaseVerify)
	info = (&runAgentsTool{coordinator: c}).Info()
	properties = info.Parameters["tasks"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
	if _, ok := properties["catalog_action"]; !ok {
		t.Fatal("verify-phase schema omits read-only catalog actions")
	}
	if enum, _ := properties["agent"].(map[string]any)["enum"].([]string); !slices.Contains(enum, "catalog-runner") {
		t.Fatalf("verify-phase agent enum %v omits the read-only catalog agent", enum)
	}

	c.phaseWorkflow = workflowAt(t, PhaseAudit)
	info = (&runAgentsTool{coordinator: c}).Info()
	properties = info.Parameters["tasks"].(map[string]any)["items"].(map[string]any)["properties"].(map[string]any)
	if _, ok := properties["catalog_action"]; ok {
		t.Fatal("audit-phase schema exposes catalog_action")
	}
	if enum, _ := properties["agent"].(map[string]any)["enum"].([]string); slices.Contains(enum, "catalog-runner") {
		t.Fatalf("audit-phase agent enum %v includes the catalog agent", enum)
	}
}

func TestCompileSetsTheWorkflowPhase(t *testing.T) {
	c, _ := dispatchTestCoordinator(t, "", nil)
	c.phaseWorkflow = &runtimeWorkflow{enabled: true, state: PhaseExecute}
	compiled, err := c.compileCatalogActionTasks([]TaskDef{catalogRequest("runtime-engineer", "collect-debug-bundle", `{"service":"api"}`)})
	if err != nil || compiled[0].Phase != PhaseExecute {
		t.Fatalf("compiled phase = %#v, err %v", compiled, err)
	}
}
