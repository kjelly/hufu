package team

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestStaticToolResolutionMatchesRuntimeSurface(t *testing.T) {
	tests := []struct {
		name     string
		tools    string
		denied   []string
		noNet    bool
		forceMCP bool
		task     TaskDef
		mode     WorkerToolResolutionMode
	}{
		{name: "preset-like grant", tools: "view,bash", task: TaskDef{}},
		{name: "alias", tools: "read,find", task: TaskDef{}},
		{name: "team deny", tools: "view,bash", denied: []string{"bash"}, task: TaskDef{}},
		{name: "no net", tools: "view,fetch", noNet: true, task: TaskDef{}},
		{name: "force mcp", tools: "view,bash", forceMCP: true, task: TaskDef{}},
		{name: "closed sequence", tools: "view,bash", task: TaskDef{Execution: ExecutionContract{ToolSequence: []string{"view"}}}},
		{name: "typed result", tools: "view", task: TaskDef{Execution: ExecutionContract{RequiresResult: true}}},
		{name: "initial plan", tools: "view", task: TaskDef{PlanFirst: true}, mode: WorkerToolResolutionInitialPlan},
		{name: "result repair", tools: "view", task: TaskDef{Execution: ExecutionContract{RequiresResult: true}}, mode: WorkerToolResolutionResultRepair},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newDirectTypedCoordinator(t, tt.tools, nil, tt.denied)
			c.noNet = tt.noNet
			c.forceMCP = tt.forceMCP
			def := c.session.Agents["worker"]
			todoID := ""
			if tt.task.Execution.RequiresResult || tt.mode == WorkerToolResolutionInitialPlan || tt.mode == WorkerToolResolutionResultRepair {
				todoID = resolverTodoID(t, c, "parity")
			}
			runtime, err := c.ToolResolver().ResolveTaskTools(t.Context(), def, WorkerToolResolutionRequest{Task: tt.task, TodoID: todoID, Mode: tt.mode})
			if err != nil {
				t.Fatalf("runtime resolution: %v", err)
			}
			base := agentToolNames(c.selectWorkerToolsForTask(def, tt.task))
			static, err := ResolveStaticWorkerTools(StaticToolResolutionInput{
				Session: c.session, Agent: def, Task: tt.task, LifecycleMode: tt.mode,
				Policy:    EffectiveTeamContractContext{NoNet: tt.noNet, ForceMCP: tt.forceMCP},
				BaseTools: base, WorkflowPhase: PhaseExecute,
			})
			if err != nil {
				t.Fatalf("static resolution: %v", err)
			}
			if !slices.Equal(runtime.Names, static.Names) {
				t.Fatalf("runtime names = %v, static names = %v", runtime.Names, static.Names)
			}
		})
	}
}

func TestTeamSourceIndexLocationsAndBody(t *testing.T) {
	dir := t.TempDir()
	writeLintTestFile(t, filepath.Join(dir, "team.yaml"), "apiVersion: hufu.io/v1alpha1\nkind: AgentTeam\nmetadata:\n  name: source-test\nspec:\n  timeout: 12\n")
	writeLintTestFile(t, filepath.Join(dir, "coordinator.md"), "---\nname: coordinator\nrole: coordinator\ntools: view\n---\nUse `view` tool.\n")
	index, err := BuildTeamSourceIndex(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	if loc := index.Location("timeout"); loc.File != "team.yaml" || loc.Line != 6 || loc.Status != LocationExact {
		t.Fatalf("timeout location = %#v", loc)
	}
	source := index.Agents["coordinator"]
	if source.Body != "Use `view` tool.\n" || source.BodyLine != 6 {
		t.Fatalf("agent body source = %#v", source)
	}
}

func TestInspectTeamCollectsTopologyDiagnostics(t *testing.T) {
	dir := t.TempDir()
	writeLintTestFile(t, filepath.Join(dir, "a.md"), "---\nname: duplicate\nrole: worker\n---\nA\n")
	writeLintTestFile(t, filepath.Join(dir, "b.md"), "---\nname: duplicate\nrole: worker\n---\nB\n")
	inspection, err := InspectTeam(dir, nil, nil, DefaultProviderRegistry, TeamCompileLint)
	if err != nil {
		t.Fatal(err)
	}
	if !hasFindingCode(inspection.Diagnostics, FindingDuplicateAgent) || !hasFindingCode(inspection.Diagnostics, FindingMissingCoordinator) {
		t.Fatalf("diagnostics = %#v", inspection.Diagnostics)
	}
	if inspection.Complete {
		t.Fatal("inspection with topology errors reported complete")
	}
}

func writeLintTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
