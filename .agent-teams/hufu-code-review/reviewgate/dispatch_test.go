package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/team"
)

// Dispatch the actual team audit through coordinator admission, materialization,
// checkpointing, embedded provider execution, and objective verification. The
// preceding accepted review inventory is a fixture; no model is called.
func TestTeamAuditDispatchBindsScopeAndVerifiesProviderResult(t *testing.T) {
	t.Run("accepted", func(t *testing.T) { testTeamAuditDispatch(t, false) })
	t.Run("rejected", func(t *testing.T) { testTeamAuditDispatch(t, true) })
}

func testTeamAuditDispatch(t *testing.T, rejected bool) {
	t.Helper()
	teamDir, err := filepath.Abs("..")
	if err != nil {
		t.Fatal(err)
	}
	loaded, err := team.LoadTeam(teamDir, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Isolate the audit action against an already accepted synthesis fixture.
	// The complete declarative batch and repair edges are tested in internal/team.
	for i := range loaded.ContractTasks {
		loaded.ContractTasks[i].DependsOnContracts = nil
		loaded.ContractTasks[i].OnFailureContract = ""
	}
	loaded.Workspace = t.TempDir()
	if err := loaded.SetCompatibilityWorkspaceScope(t.TempDir()); err != nil {
		t.Fatal(err)
	}
	loaded.ExecutionRouteConfigs = map[string]config.ExecutionRouteConfig{"review": {Candidates: []string{"ollama/fixture"}}}
	coordinator, err := team.NewCoordinator(loaded, "", "", nil, nil, team.RoleModels{Sidecar: "ollama/fixture"}, 0, false, false, false, nil, nil, nil, false, "", true, false, nil, false, false)
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
	if rejected {
		replacePayload(t, &fixture.Tasks[4], func(p *payload) { p.GapResolutions[0].ResolvingTaskID = "3" })
	}
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
	var audit *team.TodoItem
	for _, item := range coordinator.TaskTracker().TodoList().Items() {
		if item.ContractID == "verify-review-report" {
			audit = item
		}
	}
	if rejected {
		if err == nil || audit == nil || audit.Status != team.TaskError || audit.FailureEvent == nil || audit.FailureEvent.FailureClass != team.FailureVerify || audit.VerifyResult == nil || !strings.Contains(audit.VerifyResult.Stderr, "lacks observed cross-workset evidence") {
			t.Fatalf("report rejection lost classified diagnostic: audit=%+v err=%v output=%s", audit, err, output)
		}
		return
	}
	if err != nil {
		t.Fatalf("audit dispatch: %s, %v", output, err)
	}
	if audit == nil || audit.Status != team.TaskDone || audit.RunInputSnapshotID != snapshot.ID || audit.VerifyResult == nil || audit.VerifyResult.ExitCode != 0 || audit.ExecutionReceipt == nil || audit.ExecutionReceipt.RunInputSnapshotID != snapshot.ID {
		t.Fatalf("audit=%+v output=%s", audit, output)
	}
	if audit.TypedResult.RuntimeOutputs["review_report_verification"] == nil {
		t.Fatal("audit produced no verified runtime output")
	}
}

func TestTeamPrepareDispatchExecutesImmutableWorksetAndGoVerifier(t *testing.T) {
	for _, failing := range []bool{false, true} {
		t.Run(fmt.Sprint(failing), func(t *testing.T) { testPrepareDispatch(t, failing) })
	}
}

func testPrepareDispatch(t *testing.T, failing bool) {
	t.Helper()
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
	if failing {
		write("calc/value_test.go", "package calc\n\nimport \"testing\"\n\nfunc TestValue(t *testing.T) { t.Fatal(\"committed failure\") }\n")
	}
	write(".gitignore", "docs/tmp\n")
	write("docs/reference/belief-state-validation.md", "The residual local scratch proposal was compared with the integrated plan and\nremoved from `docs/tmp`; that directory contains no remaining proposal files\nand no path under it is tracked.\n")
	git("add", ".")
	git("commit", "-m", "feat: change value")
	// A failing dirty test must be ignored by the reviewed-revision verifier.
	write("calc/value_test.go", "package calc\n\nimport \"testing\"\n\nfunc TestValue(t *testing.T) { t.Fatal(\"dirty worktree\") }\n")
	loaded.Workspace = t.TempDir()
	if err := loaded.SetCompatibilityWorkspaceScope(project); err != nil {
		t.Fatal(err)
	}
	loaded.ExecutionRouteConfigs = map[string]config.ExecutionRouteConfig{"review": {Candidates: []string{"ollama/fixture"}}}
	coordinator, err := team.NewCoordinator(loaded, "", "", nil, nil, team.RoleModels{Sidecar: "ollama/fixture"}, 0, false, false, false, nil, nil, nil, false, "", true, false, nil, false, false)
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
	var tests struct {
		Passed     bool   `json:"passed"`
		ExitCode   int    `json:"exit_code"`
		Diagnostic string `json:"diagnostic"`
	}
	if err := json.Unmarshal(raw(t, items[1].TypedResult.RuntimeOutputs["targeted_go_tests"]), &tests); err != nil {
		t.Fatal(err)
	}
	if tests.Passed == failing || (failing && (tests.ExitCode != 1 || !strings.Contains(tests.Diagnostic, "committed failure"))) {
		t.Fatalf("PREPARE changed the reviewed revision's test outcome: %+v", tests)
	}
	var verification struct {
		Passed     bool `json:"passed"`
		Candidates []struct {
			ID       string `json:"id"`
			Document string `json:"document"`
			Target   string `json:"target"`
		} `json:"reference_candidates"`
	}
	if err := json.Unmarshal(raw(t, items[0].TypedResult.RuntimeOutputs["documentation_verification"]), &verification); err != nil {
		t.Fatal(err)
	}
	if !verification.Passed || len(verification.Candidates) != 1 || verification.Candidates[0].ID != "ref-000000" || verification.Candidates[0].Document != "docs/reference/belief-state-validation.md" || verification.Candidates[0].Target != "docs/tmp" {
		t.Fatalf("PREPARE lost the cleanup reference classification obligation: %+v", verification)
	}
	// Read the reports produced by the actual embedded action, rather than only
	// checking the compiled producer's structs. Interpreter JSON encoding and
	// artifact materialization are part of the worker's real input boundary.
	store, err := team.NewFileArtifactStore(loaded.Workspace, loaded.Workspace)
	if err != nil {
		t.Fatal(err)
	}
	empty, assigned := 0, 0
	for _, artifact := range items[0].TypedResult.Artifacts {
		if artifact.Kind != "documentation_verification" {
			continue
		}
		reader, err := store.Open(t.Context(), artifact.ID)
		if err != nil {
			t.Fatal(err)
		}
		var report struct {
			Key        string   `json:"workset_key"`
			IDs        []string `json:"reference_candidate_ids"`
			Candidates []struct {
				ID string `json:"id"`
			} `json:"reference_candidates"`
		}
		err = json.NewDecoder(reader).Decode(&report)
		_ = reader.Close()
		if err != nil {
			t.Fatal(err)
		}
		if report.Key == "" { // The aggregate report remains coordinator evidence.
			continue
		}
		if report.IDs == nil || report.Candidates == nil || len(report.IDs) != len(report.Candidates) {
			t.Fatalf("embedded report lost its explicit inventory: %+v", report)
		}
		if len(report.IDs) == 0 {
			empty++
		} else {
			assigned++
			if len(report.IDs) != 1 || report.IDs[0] != "ref-000000" || report.Candidates[0].ID != report.IDs[0] {
				t.Fatalf("embedded report substituted candidates: %+v", report)
			}
		}
	}
	if empty == 0 || assigned == 0 {
		t.Fatalf("embedded reports missed empty or assigned scopes: empty=%d assigned=%d", empty, assigned)
	}
}
