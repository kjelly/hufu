package main

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/team"
)

// Dispatch the actual team audit through coordinator admission, materialization,
// checkpointing, embedded provider execution, and objective verification. The
// preceding accepted review inventory is a fixture; no model is called.
func TestTeamAuditDispatchBindsScopeAndVerifiesProviderResult(t *testing.T) {
	teamDir, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := team.LoadTeam(teamDir, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	loaded.Workspace = t.TempDir()
	if err := loaded.SetCompatibilityWorkspaceScope(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	loaded.ExecutionRouteConfigs = map[string]config.ExecutionRouteConfig{"review": {Candidates: []string{"ollama/fixture"}}}
	coordinator, err := team.NewCoordinator(loaded, "", "", nil, nil, nil, team.RoleModels{}, 0, false, false, false, nil, nil, nil, false, "", true, false, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	scope := []byte(`{"kind":"last_n","count":10,"history":"first_parent","head":"HEAD"}`)
	snapshot, err := team.ResolveRunInputSnapshot(loaded.RunInputDefinitions, []team.RunInputAssignment{{Name: "review.scope", RawValue: scope, Source: team.RunInputSourceCLI}}, "run-1", "invocation-1", loaded.Config.Name)
	if err != nil {
		t.Fatal(err)
	}
	fixture := gateFixture(t)
	fixture.SnapshotID = snapshot.ID
	for i := range fixture.Tasks {
		if fixture.Tasks[i].SnapshotID != "" {
			fixture.Tasks[i].SnapshotID = snapshot.ID
		}
	}
	var saved team.SessionData
	if err := json.Unmarshal(raw(t, fixture), &saved); err != nil {
		t.Fatal(err)
	}
	for _, item := range saved.Tasks {
		item.Agent = "reviewer"
		if item.ContractID == "critic-review" {
			item.Agent = "critic"
		} else if item.ContractID == "synthesize-review" {
			item.Agent = "synthesizer"
		}
		item.Goal = "accepted fixture evidence"
		item.Desc = item.Goal
	}
	saved.RunInputSnapshots = []team.RunInputSnapshot{*snapshot}
	saved.WorkflowState = team.PhaseVerify
	saved.RuntimeWorkspace = filepath.Join(loaded.Workspace, "runtime")
	saved.PhaseResults = map[team.Phase]team.PhaseResult{team.PhasePrepare: {Status: team.PhaseStatusSuccess}, team.PhaseVerify: {Status: team.PhaseStatusSuccess}}
	coordinator.SetSessionData(&saved)
	// These are current-run accepted fixture tasks, not history to be reused.
	coordinator.TaskTracker().TodoList().Restore(saved.Tasks)
	coordinator.TaskTracker().TodoList().SetRunID("run-1")
	output, err := coordinator.ExecuteTasks(t.Context(), []team.TaskDef{{Agent: "reviewer", ContractID: "verify-review-report", Goal: "Audit all accepted review evidence"}})
	if err != nil {
		t.Fatalf("audit dispatch: %s, %v", output, err)
	}
	var audit *team.TodoItem
	for _, item := range coordinator.TaskTracker().TodoList().Items() {
		if item.ContractID == "verify-review-report" {
			audit = item
		}
	}
	if audit == nil || audit.Status != team.TaskDone || audit.RunInputSnapshotID != snapshot.ID || audit.VerifyResult == nil || audit.VerifyResult.ExitCode != 0 || audit.ExecutionReceipt == nil || audit.ExecutionReceipt.RunInputSnapshotID != snapshot.ID {
		t.Fatalf("audit=%+v output=%s", audit, output)
	}
	if audit.TypedResult.RuntimeOutputs["review_report_verification"] == nil {
		t.Fatal("audit produced no verified runtime output")
	}
}

func TestTeamPrepareDispatchExecutesImmutableWorksetAndGoVerifier(t *testing.T) {
	teamDir, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := team.LoadTeam(teamDir, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	project := t.TempDir()
	git := func(args ...string) {
		t.Helper()
		command := exec.CommandContext(t.Context(), "git", args...)
		command.Dir = project
		command.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=Fixture", "GIT_AUTHOR_EMAIL=fixture@example.test", "GIT_COMMITTER_NAME=Fixture", "GIT_COMMITTER_EMAIL=fixture@example.test")
		if output, err := command.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %s, %v", args, output, err)
		}
	}
	write := func(path, content string) {
		t.Helper()
		fullPath := filepath.Join(project, path)
		if err := os.MkdirAll(filepath.Dir(fullPath), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(fullPath, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	git("init", "--initial-branch=main")
	write("go.mod", "module example.test/review\n\ngo 1.26.0\n")
	write("calc/value.go", "package calc\n\nfunc Value() int { return 1 }\n")
	write("calc/value_test.go", "package calc\n\nimport \"testing\"\n\nfunc TestValue(t *testing.T) { if Value() != 1 { t.Fatal(Value()) } }\n")
	git("add", ".")
	git("commit", "-m", "baseline")
	write("calc/value.go", "package calc\n\nfunc Value() int { return 2 }\n")
	write("calc/value_test.go", "package calc\n\nimport \"testing\"\n\nfunc TestValue(t *testing.T) { if Value() != 2 { t.Fatal(Value()) } }\n")
	git("add", ".")
	git("commit", "-m", "feat: change value")
	// A failing dirty test must be ignored by the reviewed-revision verifier.
	write("calc/value_test.go", "package calc\n\nimport \"testing\"\n\nfunc TestValue(t *testing.T) { t.Fatal(\"dirty worktree\") }\n")
	loaded.Workspace = t.TempDir()
	if err := loaded.SetCompatibilityWorkspaceScope(project); err != nil {
		t.Fatal(err)
	}
	loaded.ExecutionRouteConfigs = map[string]config.ExecutionRouteConfig{"review": {Candidates: []string{"ollama/fixture"}}}
	coordinator, err := team.NewCoordinator(loaded, "", "", nil, nil, nil, team.RoleModels{}, 0, false, false, false, nil, nil, nil, false, "", true, false, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	snapshot, err := team.ResolveRunInputSnapshot(loaded.RunInputDefinitions, []team.RunInputAssignment{{Name: "review.scope", RawValue: []byte(`{"kind":"last_n","count":1,"history":"first_parent","head":"HEAD","commit_type":"feat"}`), Source: team.RunInputSourceCLI}}, "run-prepare", "invocation-prepare", loaded.Config.Name)
	if err != nil {
		t.Fatal(err)
	}
	coordinator.SetSessionData(&team.SessionData{ActiveRunInputSnapshotID: snapshot.ID, RunInputSnapshots: []team.RunInputSnapshot{*snapshot}, WorkflowState: team.PhasePrepare})
	coordinator.TaskTracker().TodoList().SetRunID("run-prepare")
	output, err := coordinator.ExecuteTasks(t.Context(), []team.TaskDef{
		{Agent: "reviewer", ContractID: "produce-workset", Goal: "Produce immutable worksets"},
		{Agent: "reviewer", ContractID: "verify-targeted-go-tests", Goal: "Run snapshot-pinned tests"},
	})
	if err != nil {
		t.Fatalf("PREPARE dispatch: %s, %v", output, err)
	}
	items := coordinator.TaskTracker().TodoList().Items()
	if len(items) != 2 {
		t.Fatalf("items=%d", len(items))
	}
	for _, item := range items {
		if item.Status != team.TaskDone || item.ExecutionReceipt == nil || item.RunInputSnapshotID != snapshot.ID {
			t.Fatalf("prepare occurrence=%+v output=%s", item, output)
		}
	}
	if items[1].TypedResult.RuntimeOutputs["targeted_go_tests"] == nil || len(items[0].TypedResult.Artifacts) < 3 {
		t.Fatal("PREPARE omitted test receipt or routed workset artifacts")
	}
}
