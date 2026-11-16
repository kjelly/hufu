package main

import (
	"bytes"
	"context"
	json "encoding/json/v2"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/runtimestore"
	"github.com/kjelly/hufu/internal/taskdistill"
	"github.com/kjelly/hufu/internal/team"
	workspacepkg "github.com/kjelly/hufu/internal/workspace"
)

func distillationRefinementFixture(t *testing.T, count int) (workspaceCLIFixture, workspacepkg.Resolution, workspaceDistillationPlan, string) {
	t.Helper()
	f := newWorkspaceCLIFixture(t)
	f.run(t, "register")
	f.deps.registryOptions = []workspacepkg.RegistryOption{workspacepkg.WithIDGenerator(workspacepkg.NewIDGenerator(bytes.NewReader(fixtureIDs(0x21, 0x22, 0x23))))}
	registry, err := workspacepkg.OpenReadWrite(f.stateRoot, f.deps.registryOptions...)
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := registry.CreateWorkspace(t.Context(), f.projectID, "default")
	_ = registry.Close()
	if err != nil {
		t.Fatal(err)
	}
	destination := workspacepkg.Resolution{ProjectID: workspace.ProjectID, WorkspaceID: workspace.ID, ContextScopeID: workspace.ContextScopeID, ControlRoot: workspace.ControlRoot, TeamName: "default"}
	session := team.NewSession()
	session.Tasks = []*team.TodoItem{{ID: "old", Agent: "worker", Desc: "Completed historical review", Status: team.TaskDone, Output: "Frozen source evidence; never resend it for refinement."}}
	if err := team.SaveSession(workspace.ControlRoot, session); err != nil {
		t.Fatal(err)
	}
	snapshot, err := team.SnapshotTasksForDistillation(t.Context(), workspace.ControlRoot, []string{"old"})
	if err != nil {
		t.Fatal(err)
	}
	parent := workspaceDistillationPlan{ProjectID: destination.ProjectID, WorkspaceID: destination.WorkspaceID, ContextScopeID: destination.ContextScopeID, Team: "default", Model: "test-model", Policy: "Keep reusable knowledge.", Tasks: &snapshot, Sources: snapshot.Sources, MaxModelCalls: 10, Draft: taskdistill.Draft{Decisions: []taskdistill.Decision{{SourceID: "old", Action: "distill", Reason: "useful"}}, Knowledge: []taskdistill.Knowledge{}}}
	for i := range count {
		parent.Draft.Knowledge = append(parent.Draft.Knowledge, taskdistill.Knowledge{Text: fmt.Sprintf("Reusable procedure %d; only verified results count.", i), SourceIDs: []string{"old"}})
	}
	parent = persistRefinementTestPlan(t, destination.ControlRoot, parent)
	search := t.TempDir()
	helperSetupTeam(t, search, "default")
	return f, destination, parent, search
}

func persistRefinementTestPlan(t *testing.T, workspace string, plan workspaceDistillationPlan) workspaceDistillationPlan {
	t.Helper()
	plan.ID = ""
	plan.ID = taskdistill.Digest(plan)
	store, err := runtimestore.Open(t.Context(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(plan)
	if err == nil {
		err = store.PutDocument(t.Context(), "task_distillation_plan:"+plan.ID, raw)
	}
	_ = store.Close()
	if err != nil {
		t.Fatal(err)
	}
	return plan
}

func runRefinementTestCommand(t *testing.T, fixture workspaceCLIFixture, args ...string) (string, error) {
	t.Helper()
	cmd := newWorkspaceCommand(fixture.deps)
	cmd.SetContext(t.Context())
	cmd.SilenceErrors, cmd.SilenceUsage = true, true
	var out, progress bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&progress)
	cmd.SetArgs(args)
	err := cmd.Execute()
	return out.String(), err
}

func refinementTestGenerator(t *testing.T, prompt string) string {
	t.Helper()
	_, raw, _ := strings.Cut(prompt, "\nDATA: ")
	var input struct {
		Sources []taskdistill.Source `json:"sources"`
	}
	if err := json.Unmarshal([]byte(raw), &input); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "Frozen source evidence;") {
		t.Fatal("refinement sent the original source instead of parent knowledge")
	}
	draft := taskdistill.Draft{Decisions: []taskdistill.Decision{}, Knowledge: []taskdistill.Knowledge{}}
	var ids []string
	for _, source := range input.Sources {
		action := "distill"
		if strings.Contains(prompt, "Plan refinement v1") && source.ID == "fact_0" {
			action = "discard"
		}
		draft.Decisions = append(draft.Decisions, taskdistill.Decision{SourceID: source.ID, Action: action, Reason: "Remove a transient report; keep the reusable rule."})
		if action == "distill" {
			ids = append(ids, source.ID)
		}
	}
	if len(ids) > 0 {
		draft.Knowledge = append(draft.Knowledge, taskdistill.Knowledge{Text: "Only claim success when verified evidence supports it.", SourceIDs: ids})
	}
	encoded, err := json.Marshal(draft)
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func TestWorkspaceDistillationRefineReviewAndApply(t *testing.T) {
	fixture, destination, parent, search := distillationRefinementFixture(t, 3)
	original := taskDistillationGeneratorFactory
	t.Cleanup(func() { taskDistillationGeneratorFactory = original })
	calls := 0
	taskDistillationGeneratorFactory = func(_ context.Context, _, audit, model string) (maintenanceTextGenerator, string, func(), error) {
		if audit == destination.ControlRoot || model != parent.Model {
			t.Fatal("incorrect audit workspace or model default")
		}
		return identityTextGenerator{identity: "test-admitted-profile", generate: func(_ context.Context, prompt string) (string, error) {
			calls++
			return refinementTestGenerator(t, prompt), nil
		}}, model, func() {}, nil
	}
	args := []string{"distill", "refine", parent.ID, "--team", "default", "--team-search-path", search, "--max-model-calls", "5"}
	output, err := runRefinementTestCommand(t, fixture, args...)
	if err != nil {
		t.Fatal(err)
	}
	var child workspaceDistillationPlan
	if err := json.Unmarshal([]byte(output), &child); err != nil {
		t.Fatal(err)
	}
	if child.ID == parent.ID || child.Refinement.ParentPlanID != parent.ID || len(child.Draft.Knowledge) != 1 || child.Draft.Decisions[0].Action != taskdistill.ActionExtractOnly || calls != 1 {
		t.Fatalf("unexpected child: id=%s draft=%+v calls=%d", child.ID, child.Draft, calls)
	}
	if _, err := os.Stat(filepath.Join(destination.ControlRoot, "context.sqlite")); !os.IsNotExist(err) {
		t.Fatal("refinement applied knowledge")
	}
	if _, err := runRefinementTestCommand(t, fixture, args...); err != nil || calls != 1 {
		t.Fatalf("accepted checkpoint resent: calls=%d err=%v", calls, err)
	}
	for _, full := range []bool{false, true} {
		args := []string{"distill", "show", child.ID, "--team", "default"}
		if full {
			args = append(args, "--full")
		}
		out, err := runRefinementTestCommand(t, fixture, args...)
		if err != nil || strings.Contains(out, "Frozen source evidence;") != full || !strings.Contains(out, parent.ID) || !strings.Contains(out, "fact_1") {
			t.Fatalf("review lost provenance/full boundary: full=%t err=%v", full, err)
		}
	}
	for range 2 {
		if _, err := runRefinementTestCommand(t, fixture, "distill", "--team", "default", "--apply", child.ID, "--yes"); err != nil {
			t.Fatal(err)
		}
	}
	if calls != 1 || len(team.LoadSession(destination.ControlRoot).Tasks) != 1 {
		t.Fatal("apply generated or deleted the retained source")
	}
	repo, err := contextstore.OpenSQLiteReadOnly(filepath.Join(destination.ControlRoot, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repo.Close() }()
	items, err := repo.Query(t.Context(), contextstore.RepositoryQuery{Scope: contextstore.Scope{ProjectID: destination.ContextScopeID, TeamID: "default"}})
	if err != nil || len(items) != 1 || items[0].Source.Ref != child.ID || len(items[0].Evidence) != 1 || !strings.HasPrefix(items[0].Evidence[0].Ref, "old@sha256:") {
		t.Fatalf("durable knowledge lost provenance: %+v err=%v", items, err)
	}
	loaded, err := loadWorkspaceDistillationPlan(t.Context(), destination, parent.ID, 0)
	if err != nil || taskdistill.Digest(loaded) != taskdistill.Digest(parent) {
		t.Fatal("refinement changed the parent plan")
	}
}

func TestWorkspaceDistillationRefinementResumesAndRejectsStaleSources(t *testing.T) {
	fixture, destination, parent, search := distillationRefinementFixture(t, 33)
	original := taskDistillationGeneratorFactory
	t.Cleanup(func() { taskDistillationGeneratorFactory = original })
	fail := true
	calls := 0
	taskDistillationGeneratorFactory = func(context.Context, string, string, string) (maintenanceTextGenerator, string, func(), error) {
		return identityTextGenerator{identity: "test-profile", generate: func(_ context.Context, prompt string) (string, error) {
			calls++
			if fail && calls == 2 {
				return "", errors.New("offline provider failure")
			}
			return refinementTestGenerator(t, prompt), nil
		}}, "test-model", func() {}, nil
	}
	args := []string{"distill", "refine", parent.ID, "--team", "default", "--team-search-path", search, "--max-model-calls", "5"}
	if out, err := runRefinementTestCommand(t, fixture, args...); err == nil || out != "" {
		t.Fatal("partial refinement returned a plan")
	}
	calls, fail = 0, false
	out, err := runRefinementTestCommand(t, fixture, args...)
	if err != nil || calls != 1 {
		t.Fatalf("resume regenerated completed work: calls=%d err=%v", calls, err)
	}
	var child workspaceDistillationPlan
	if err := json.Unmarshal([]byte(out), &child); err != nil {
		t.Fatal(err)
	}
	// Reopen under a new command without ever invoking the model again.
	session := team.LoadSession(destination.ControlRoot)
	session.Tasks[0].Output = "Changed after the snapshot."
	if err := team.SaveSession(destination.ControlRoot, session); err != nil {
		t.Fatal(err)
	}
	calls = 0
	if _, err := runRefinementTestCommand(t, fixture, args...); err == nil || calls != 0 {
		t.Fatal("stale source admitted inference")
	}
	if _, err := runRefinementTestCommand(t, fixture, "distill", "--team", "default", "--apply", child.ID, "--yes"); err == nil || calls != 0 {
		t.Fatal("stale refinement applied")
	}
	if _, err := os.Stat(filepath.Join(destination.ControlRoot, "context.sqlite")); !os.IsNotExist(err) {
		t.Fatal("stale apply saved knowledge")
	}
}

func TestWorkspaceDistillationRejectsChangedRefinementLineage(t *testing.T) {
	fixture, destination, parent, _ := distillationRefinementFixture(t, 2)
	filter := taskdistill.Draft{Decisions: []taskdistill.Decision{{SourceID: "fact_0", Action: "discard", Reason: "transient"}, {SourceID: "fact_1", Action: "distill", Reason: "useful"}}, Knowledge: []taskdistill.Knowledge{{Text: parent.Draft.Knowledge[1].Text, SourceIDs: []string{"fact_1"}}}}
	evidence := taskdistill.Refinement{Batches: []taskdistill.Draft{filter}, Knowledge: filter.Knowledge}
	draft, err := taskdistill.RebuildRefinement(parent.Draft, parent.Sources, taskdistill.DefaultRefinementPolicy, evidence)
	if err != nil {
		t.Fatal(err)
	}
	child := parent
	child.Policy, child.Draft = taskdistill.DefaultRefinementPolicy, draft
	child.Refinement = &workspaceDistillationRefinement{ParentPlanID: parent.ID, Evidence: evidence}
	for _, mode := range []string{"source", "draft", "parent", "scope"} {
		t.Run(mode, func(t *testing.T) {
			raw, _ := json.Marshal(child)
			var changed workspaceDistillationPlan
			if err := json.Unmarshal(raw, &changed); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "source":
				changed.Sources[0].Content = "Substituted frozen evidence."
			case "draft":
				changed.Draft.Decisions[0].Action = "distill"
			case "parent":
				changed.Refinement.ParentPlanID = "missing-parent"
			case "scope":
				changed.ContextScopeID = "other-scope"
			}
			changed = persistRefinementTestPlan(t, destination.ControlRoot, changed)
			if _, err := runRefinementTestCommand(t, fixture, "distill", "show", changed.ID, "--team", "default"); err == nil {
				t.Fatal("review admitted substituted provenance")
			}
			if _, err := runRefinementTestCommand(t, fixture, "distill", "--team", "default", "--apply", changed.ID, "--yes"); err == nil {
				t.Fatal("apply admitted substituted provenance")
			}
		})
	}
}
