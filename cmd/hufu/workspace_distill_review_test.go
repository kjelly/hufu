package main

import (
	"context"
	json "encoding/json/v2"
	"path/filepath"
	"slices"
	"testing"

	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/taskdistill"
	"github.com/kjelly/hufu/internal/team"
)

func TestWorkspaceDistillationOperatorReviewThenApply(t *testing.T) {
	fixture, destination, parent, _ := distillationRefinementFixture(t, 3)
	original := taskDistillationGeneratorFactory
	t.Cleanup(func() { taskDistillationGeneratorFactory = original })
	taskDistillationGeneratorFactory = func(context.Context, string, string, string) (maintenanceTextGenerator, string, func(), error) {
		t.Fatal("operator review must not admit or call a model")
		return nil, "", nil, nil
	}
	args := []string{"distill", "review", parent.ID, "--team", "default", "--keep-knowledge", "1"}
	out, err := runRefinementTestCommand(t, fixture, args...)
	if err != nil {
		t.Fatal(err)
	}
	var reviewed workspaceDistillationPlan
	if err := json.Unmarshal([]byte(out), &reviewed); err != nil {
		t.Fatal(err)
	}
	if len(reviewed.Draft.Knowledge) != 1 || reviewed.Draft.Knowledge[0].Text != parent.Draft.Knowledge[1].Text || reviewed.Draft.Decisions[0].Action != taskdistill.ActionExtractOnly || reviewed.MaxModelCalls != 0 || !slices.Equal(reviewed.Refinement.OperatorSelection, []int{1}) {
		t.Fatalf("review widened selection or cleanup: %+v", reviewed)
	}
	if reviewed.Refinement.ParentPlanID != parent.ID || taskdistill.Digest(reviewed.Sources) != taskdistill.Digest(parent.Sources) {
		t.Fatal("review lost frozen provenance")
	}
	loaded, err := loadWorkspaceDistillationPlan(t.Context(), destination, reviewed.ID, 0)
	if err != nil || taskdistill.Digest(loaded) != taskdistill.Digest(reviewed) {
		t.Fatalf("new-process review: %v", err)
	}
	repeated, err := runRefinementTestCommand(t, fixture, args...)
	if err != nil || repeated != out {
		t.Fatal("same selection did not produce an identical saved plan")
	}
	for range 2 {
		if _, err := runRefinementTestCommand(t, fixture, "distill", "--team", "default", "--apply", reviewed.ID, "--yes"); err != nil {
			t.Fatal(err)
		}
	}
	repo, err := contextstore.OpenSQLiteReadOnly(filepath.Join(destination.ControlRoot, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	items, err := repo.Query(t.Context(), contextstore.RepositoryQuery{Scope: contextstore.Scope{ProjectID: destination.ContextScopeID, TeamID: "default"}})
	_ = repo.Close()
	if err != nil || len(items) != 1 || items[0].Content != parent.Draft.Knowledge[1].Text || items[0].Source.Ref != reviewed.ID || len(items[0].Evidence) != 1 {
		t.Fatalf("apply saved unselected or unbound knowledge: %+v %v", items, err)
	}
	if len(team.LoadSession(destination.ControlRoot).Tasks) != 1 {
		t.Fatal("omitting knowledge authorized source cleanup")
	}
}

func TestWorkspaceDistillationOperatorReviewRejectsInvalidSelections(t *testing.T) {
	fixture, destination, parent, _ := distillationRefinementFixture(t, 3)
	for _, value := range []string{"", "-1", "3", "1,1", "not-an-index"} {
		t.Run(value, func(t *testing.T) {
			args := []string{"distill", "review", parent.ID, "--team", "default"}
			if value != "" {
				args = append(args, "--keep-knowledge", value)
			}
			if _, err := runRefinementTestCommand(t, fixture, args...); err == nil {
				t.Fatal("invalid selection accepted")
			}
		})
	}
	_, draft, selection, err := selectReviewedKnowledge(parent, []int{2, 0})
	if err != nil || !slices.Equal(selection, []int{0, 2}) || len(draft.Knowledge) != 2 {
		t.Fatalf("valid explicit order not canonicalized: %v %v", selection, err)
	}
	session := team.LoadSession(destination.ControlRoot)
	session.Tasks[0].Output = "Changed source evidence."
	if err := team.SaveSession(destination.ControlRoot, session); err != nil {
		t.Fatal(err)
	}
	if _, err := runRefinementTestCommand(t, fixture, "distill", "review", parent.ID, "--team", "default", "--keep-knowledge", "1"); err == nil {
		t.Fatal("stale source accepted")
	}
}

func TestWorkspaceDistillationOperatorReviewRejectsAlteredScope(t *testing.T) {
	fixture, destination, parent, _ := distillationRefinementFixture(t, 3)
	evidence, draft, selection, err := selectReviewedKnowledge(parent, []int{1})
	if err != nil {
		t.Fatal(err)
	}
	child := parent
	child.Policy, child.MaxModelCalls, child.Draft = operatorKnowledgeSelectionPolicy, 0, draft
	child.Refinement = &workspaceDistillationRefinement{ParentPlanID: parent.ID, Evidence: evidence, OperatorSelection: selection}
	for _, mode := range []string{"selection", "missing selection", "text", "budget", "policy", "retention scope"} {
		t.Run(mode, func(t *testing.T) {
			raw, _ := json.Marshal(child)
			var changed workspaceDistillationPlan
			if err := json.Unmarshal(raw, &changed); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "selection":
				changed.Refinement.OperatorSelection = []int{0}
			case "missing selection":
				changed.Refinement.OperatorSelection = nil
			case "text":
				changed.Refinement.Evidence.Knowledge[0].Text = "Unreviewed replacement."
				changed.Draft.Knowledge[0].Text = "Unreviewed replacement."
			case "budget":
				changed.MaxModelCalls = 1
			case "policy":
				changed.Policy = "Unreviewed policy."
			case "retention scope":
				changed.ExtractOnly = !parent.ExtractOnly
			}
			changed = persistRefinementTestPlan(t, destination.ControlRoot, changed)
			if _, err := loadWorkspaceDistillationPlan(t.Context(), destination, changed.ID, 0); err == nil {
				t.Fatal("altered operator review accepted")
			}
			if _, err := runRefinementTestCommand(t, fixture, "distill", "--team", "default", "--apply", changed.ID, "--yes"); err == nil {
				t.Fatal("altered operator review applied")
			}
		})
	}
}

func TestOperatorReviewSelectsFromLargeParentInventory(t *testing.T) {
	_, _, parent, _ := distillationRefinementFixture(t, 1200)
	evidence, draft, selection, err := selectReviewedKnowledge(parent, []int{1199, 0})
	if err != nil || len(evidence.Batches) != 38 || len(draft.Knowledge) != 2 || !slices.Equal(selection, []int{0, 1199}) {
		t.Fatalf("large parent selection: %v %v", selection, err)
	}
	if draft.Knowledge[0].Text != parent.Draft.Knowledge[0].Text || draft.Knowledge[1].Text != parent.Draft.Knowledge[1199].Text {
		t.Fatal("selection rewrote approved text")
	}
}
