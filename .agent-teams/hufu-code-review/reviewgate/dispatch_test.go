package main

import (
	"context"
	"encoding/json"
	"errors"
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
	t.Run("accepted", func(t *testing.T) { testTeamAuditDispatch(t, false, false, false) })
	t.Run("rejected", func(t *testing.T) { testTeamAuditDispatch(t, true, false, false) })
	t.Run("invalid_source", func(t *testing.T) { testTeamAuditDispatch(t, false, true, false) })
}

func TestSupplementInventoryDispatchPreservesExplicitEvidenceRanges(t *testing.T) {
	testTeamAuditDispatch(t, false, false, true)
}

func testTeamAuditDispatch(t *testing.T, rejected, invalidSource, supplement bool) {
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
	if supplement {
		replacePayload(t, &fixture.Tasks[0], func(p *payload) {
			var source record
			if err := json.Unmarshal(p.Record, &source); err != nil {
				t.Fatal(err)
			}
			source.Gaps = []json.RawMessage{raw(t, sourceGap{Description: "Caller missing", MissingEvidence: "caller bytes", Kind: "missing_dependency", RequestedEvidence: []requestedEvidence{{Path: "caller.go", Ranges: []evidenceRange{{Start: 0, End: 5}}}}, Limitations: []sourceLimitation{}})}
			p.Record = raw(t, source)
		})
		requests, err := fixtureCollectSupplementRequests(fixture, "run-1")
		if err != nil {
			t.Fatal(err)
		}
		var outputs map[string]any
		if err := json.Unmarshal(raw(t, map[string]any{"review_supplement_requests": requests}), &outputs); err != nil {
			t.Fatal(err)
		}
		if _, _, err := team.CanonicalizeRuntimeOutputs(outputs); err != nil {
			t.Fatalf("native Go supplement serialization: %v", err)
		}
	}
	if invalidSource {
		fixture.Tasks[3].Result.Status = "success"
	}
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
	contractID := "verify-review-report"
	if supplement {
		contractID = "inventory-review-supplement-requests"
	}
	output, err := coordinator.ExecuteTasks(t.Context(), []team.TaskDef{{Agent: "reviewer", ContractID: contractID, Goal: "Audit all accepted review evidence"}})
	var audit *team.TodoItem
	for _, item := range coordinator.TaskTracker().TodoList().Items() {
		if item.ContractID == contractID {
			audit = item
		}
	}
	if invalidSource {
		if err == nil || audit == nil || audit.FailureEvent == nil || audit.FailureEvent.FailureClass == team.FailureVerify || !strings.Contains(audit.FailureEvent.Summary, "critic 6 concealed") {
			t.Fatalf("source failure could take report-only verification edge: audit=%+v err=%v", audit, err)
		}
		return
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
	if supplement {
		var requests supplementRequests
		if err := json.Unmarshal(raw(t, audit.TypedResult.RuntimeOutputs["review_supplement_requests"]), &requests); err != nil {
			t.Fatal(err)
		}
		if len(requests.Requests) != 1 || len(requests.Requests[0].RequestedEvidence) != 1 || len(requests.Requests[0].RequestedEvidence[0].Ranges) != 1 || requests.Requests[0].RequestedEvidence[0].Ranges[0] != (evidenceRange{Start: 0, End: 5}) {
			t.Fatalf("supplement inventory lost explicit ranges: %#v", requests)
		}
		return
	}
	if audit.TypedResult.RuntimeOutputs["review_report_verification"] == nil {
		t.Fatal("audit produced no verified runtime output")
	}
}

func TestTeamPrepareDispatchExecutesImmutableWorksetAndGoVerifier(t *testing.T) {
	for _, failing := range []bool{false, true} {
		t.Run(fmt.Sprint(failing), func(t *testing.T) { testPrepareDispatch(t, failing, false) })
	}
}

func TestTeamPrepareDispatchPreservesLargeSnapshotInventory(t *testing.T) {
	testPrepareDispatch(t, false, true)
}

func testPrepareDispatch(t *testing.T, failing, largeSnapshot bool) {
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
	if largeSnapshot {
		for i := range 4000 {
			write(fmt.Sprintf("docs/%04d-%s.md", i, strings.Repeat("inventory", 15)), "excluded documentation\n")
		}
		write("catalog.txt", strings.Repeat("原始碼", 48_000))
	}
	git("add", ".")
	git("commit", "-m", "feat: change value")
	// A failing dirty test must be ignored by the reviewed-revision verifier.
	write("calc/value_test.go", "package calc\n\nimport \"testing\"\n\nfunc TestValue(t *testing.T) { t.Fatal(\"dirty worktree\") }\n")
	loaded.Workspace = t.TempDir()
	if err := loaded.SetCompatibilityWorkspaceScope(project); err != nil {
		t.Fatal(err)
	}
	loaded.ExecutionRouteConfigs = map[string]config.ExecutionRouteConfig{"review": {Candidates: []string{"ollama/fixture"}}}
	coordinator, err := team.NewCoordinator(loaded, "", "", nil, nil, team.RoleModels{}, 0, false, false, false, nil, nil, nil, false, "", true, false, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = coordinator.Close() })
	scopeInput := `{"kind":"last_n","count":1,"history":"first_parent","head":"HEAD","commit_type":"feat"}`
	if largeSnapshot {
		scopeInput = `{"kind":"repository_snapshot","history":"first_parent","head":"HEAD"}`
	}
	snapshot, err := team.ResolveRunInputSnapshot(loaded.RunInputDefinitions, []team.RunInputAssignment{{Name: "review.scope", RawValue: []byte(scopeInput), Source: team.RunInputSourceCLI}}, "run-prepare", "invocation-prepare", loaded.Config.Name)
	if err != nil {
		t.Fatal(err)
	}
	coordinator.SetSessionData(&team.SessionData{ActiveRunInputSnapshotID: snapshot.ID, RunInputSnapshots: []team.RunInputSnapshot{*snapshot}, WorkflowState: team.PhasePrepare})
	coordinator.TaskTracker().TodoList().SetRunID("run-prepare")
	if largeSnapshot {
		if err := coordinator.PrepareContextPreflightContext(t.Context()); err != nil {
			t.Fatal(err)
		}
	}
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
	if largeSnapshot {
		var tests struct {
			Packages []string `json:"packages"`
			Passed   bool     `json:"passed"`
		}
		if err := json.Unmarshal(raw(t, items[1].TypedResult.RuntimeOutputs["targeted_go_tests"]), &tests); err != nil {
			t.Fatal(err)
		}
		if !tests.Passed || len(tests.Packages) != 1 || tests.Packages[0] != "./calc" {
			t.Fatalf("PREPARE did not verify the selected snapshot packages: %+v", tests)
		}
		var scope snapshotAttestation
		if err := json.Unmarshal(raw(t, items[0].TypedResult.RuntimeOutputs["scope"]), &scope); err != nil {
			t.Fatal(err)
		}
		if scope.Snapshot == nil || scope.Snapshot.Inventory == nil || len(scope.Snapshot.Files) != 0 || scope.Snapshot.Plan != nil || scope.Snapshot.FileCount != 4006 {
			t.Fatalf("missing compact inventory reference: %+v", scope.Snapshot)
		}
		store, err := team.NewFileArtifactStore(loaded.Workspace, loaded.Workspace)
		if err != nil {
			t.Fatal(err)
		}
		reader, err := store.Open(t.Context(), scope.Snapshot.Inventory.ID)
		if err != nil {
			t.Fatal(err)
		}
		var inventory snapshotIdentity
		err = json.NewDecoder(reader).Decode(&inventory)
		_ = reader.Close()
		if err != nil || len(inventory.Files) != scope.Snapshot.FileCount || inventory.ID != scope.Snapshot.ID || scope.Snapshot.Inventory.Bytes <= 900*1024 {
			t.Fatalf("durable inventory lost: files=%d err=%v", len(inventory.Files), err)
		}
		files, err := checkedSnapshotFiles(&snapshotAttestation{Satisfied: true, Resolved: scope.Resolved, Snapshot: &inventory})
		if err != nil {
			t.Fatal(err)
		}
		if inventory.Plan == nil {
			t.Fatal("native producer omitted the snapshot workset plan")
		}
		page, err := checkSnapshotPlan(&inventory, files)
		if err != nil || len(page.worksets) == 0 {
			t.Fatalf("native producer emitted an invalid plan: %v", err)
		}
		fragments := 0
		for _, workset := range page.worksets {
			if len(workset.files["catalog.txt"]) > 0 {
				fragments++
			}
		}
		if fragments < 2 {
			t.Fatal("native producer did not split the oversized line")
		}
		assertPrepareFanoutAdmission(t, coordinator, items[0])
		return
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

// Stop after the real admission/receipt boundary, before any model is called.
func assertPrepareFanoutAdmission(t *testing.T, coordinator *team.Coordinator, producer *team.TodoItem) {
	t.Helper()
	stop := errors.New("fan-out admitted; stop before model execution")
	var admitted []team.TaskDef
	originalReceipt := raw(t, producer.ExecutionReceipt)
	originalArtifacts := raw(t, producer.TypedResult.Artifacts)
	coordinator.SetStepConfirmFn(func(_ context.Context, tasks []team.TaskDef) (bool, error) {
		admitted = tasks
		receipts := map[string]*team.WorksetExpansionReceipt{}
		for _, item := range coordinator.TaskTracker().TodoList().Items() {
			if item.WorksetReceipt != nil {
				receipts[item.WorksetReceipt.WorksetID] = item.WorksetReceipt
			}
		}
		if len(receipts) != 3 {
			t.Fatalf("expected three route receipts, got %d", len(receipts))
		}
		children := 0
		for _, item := range coordinator.TaskTracker().TodoList().Items() {
			if item.WorksetBinding == nil {
				continue
			}
			children++
			receipt := receipts[item.WorksetBinding.WorksetID]
			if receipt == nil || receipt.Children[item.WorksetBinding.ItemKey] != item.ID || receipt.SourceArtifactID != item.WorksetBinding.SourceArtifactID {
				t.Fatal("fan-out receipt does not assign child")
			}
		}
		if children != len(tasks) {
			t.Fatalf("receipt children=%d admitted=%d", children, len(tasks))
		}
		return false, stop
	})
	_, err := coordinator.ExecuteTasks(t.Context(), []team.TaskDef{
		{Agent: "reviewer", ContractID: "review-primary-workset", Goal: "Review the primary workset."},
		{Agent: "documentation-reviewer", ContractID: "review-documentation-workset", Goal: "Review the documentation workset."},
		{Agent: "critic", ContractID: "review-documentation-escalation", Goal: "Review the documentation escalation workset."},
	})
	if !errors.Is(err, stop) {
		t.Fatalf("three-route fan-out did not reach model boundary: %v", err)
	}
	routes := map[string]int{}
	shared := map[string]int{}
	for _, task := range admitted {
		routes[task.ContractID]++
		if task.WorksetBinding == nil || task.WorksetBinding.SourceArtifactID == "" {
			t.Fatal("missing receipt-bound workset")
		}
		for _, ref := range task.WorksetBinding.Inputs {
			if ref.Kind != "review_evidence_inventory" || len(task.WorksetBinding.TouchedPaths) != 0 {
				continue
			}
			if ref.TaskID != producer.ID || ref.RunID != producer.TypedResult.Artifacts[0].RunID || ref.Attempt != producer.TypedResult.Attempt {
				t.Fatalf("producer occurrence changed: %+v", ref)
			}
			shared[ref.ID]++
		}
	}
	if routes["review-primary-workset"] == 0 || routes["review-documentation-workset"] != 1 || routes["review-documentation-escalation"] != 1 {
		t.Fatalf("routes=%v", routes)
	}
	collision := ""
	for id, count := range shared {
		if count == 2 {
			collision = id
		}
	}
	if collision == "" {
		t.Fatalf("fixture did not exercise shared empty inventory: %v", shared)
	}
	declarations := 0
	for _, ref := range producer.TypedResult.Artifacts {
		if ref.ID == collision {
			declarations++
		}
	}
	if declarations != 2 {
		t.Fatalf("original declarations were lost: %d", declarations)
	}
	if !equalJSON(originalReceipt, raw(t, producer.ExecutionReceipt)) || !equalJSON(originalArtifacts, raw(t, producer.TypedResult.Artifacts)) {
		t.Fatal("fan-out rewrote producer evidence")
	}
	if len(coordinator.TaskTracker().TodoList().Items()) != 2 {
		t.Fatal("stopped admission left pending children")
	}

}
