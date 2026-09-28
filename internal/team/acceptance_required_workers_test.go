package team

import (
	"strings"
	"testing"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
)

func TestFinishRejectsZeroTaskRunWithRequiredWorkers(t *testing.T) {
	c := newBudgetCoordinator(t)
	if err := c.SetAcceptanceSpec(AcceptanceSpec{RequiredWorkers: []string{"worker"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := (&finishTool{coordinator: c}).Run(t.Context(), fantasy.ToolCall{Input: `{"response":"done"}`}); err != nil {
		t.Fatal(err)
	}
	result := c.LastRunResult()
	if result == nil || result.Outcome == RunOutcomeCompleted || result.GoalSatisfied || result.Acceptance == nil || result.Acceptance.EffectiveState() != AcceptanceFailed {
		t.Fatalf("zero-task finish accepted: %#v", result)
	}
}

func TestAcceptanceRequiredWorkersRejectsEmptyAndMissingStages(t *testing.T) {
	c := &Coordinator{
		session:        &TeamSession{Config: agent.TeamConfig{Name: "staged"}},
		projectDir:     t.TempDir(),
		taskTracker:    NewTaskTracker(),
		acceptanceSpec: &AcceptanceSpec{RequiredWorkers: []string{"analyst", "implementer"}},
	}
	if !AcceptanceSpecHasChecks(*c.acceptanceSpec) {
		t.Fatal("required workers must make acceptance non-vacuous")
	}
	result, err := c.runAcceptance(t.Context())
	if err == nil || result.State != AcceptanceFailed || len(result.Errors) != 2 {
		t.Fatalf("empty run acceptance = %#v, %v", result, err)
	}

	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "analyst", Desc: "analyze"}, {Agent: "implementer", Desc: "implement"}})
	c.taskTracker.TodoList().UpdateStatus(items[0].ID, TaskDone, "done")
	if err := c.taskTracker.TodoList().SetTypedResult(items[0].ID, &TaskResult{Status: TaskResultStatusSuccess, Source: "submitted"}); err != nil {
		t.Fatal(err)
	}
	result, err = c.runAcceptance(t.Context())
	if err == nil || result.State != AcceptanceFailed || len(result.Errors) != 1 || !strings.Contains(result.Errors[0], "implementer") {
		t.Fatalf("missing implementer acceptance = %#v, %v", result, err)
	}

	c.taskTracker.TodoList().UpdateStatus(items[1].ID, TaskDone, "done")
	if err := c.taskTracker.TodoList().SetTypedResult(items[1].ID, &TaskResult{Status: TaskResultStatusCompletedWithGaps, Source: "submitted"}); err != nil {
		t.Fatal(err)
	}
	result, err = c.runAcceptance(t.Context())
	if err == nil || result.State != AcceptanceFailed {
		t.Fatalf("completed_with_gaps stage satisfied acceptance: %#v, %v", result, err)
	}
	if err := c.taskTracker.TodoList().SetTypedResult(items[1].ID, &TaskResult{Status: TaskResultStatusSuccess, Source: "submitted"}); err != nil {
		t.Fatal(err)
	}
	result, err = c.runAcceptance(t.Context())
	if err != nil || !result.IsPassed() || len(result.RequiredWorkers) != 2 {
		t.Fatalf("completed stages acceptance = %#v, %v", result, err)
	}
}

func TestAcceptanceRequiredWorkersRejectsRuntimeOwnedResult(t *testing.T) {
	c := &Coordinator{
		session:        &TeamSession{Config: agent.TeamConfig{Name: "staged"}},
		projectDir:     t.TempDir(),
		taskTracker:    NewTaskTracker(),
		acceptanceSpec: &AcceptanceSpec{RequiredWorkers: []string{"worker"}},
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "runtime action"}})[0]
	c.taskTracker.TodoList().UpdateStatus(item.ID, TaskDone, "done")
	if err := c.taskTracker.TodoList().SetTypedResult(item.ID, &TaskResult{Source: "runtime"}); err != nil {
		t.Fatal(err)
	}
	result, err := c.runAcceptance(t.Context())
	if err == nil || result.State != AcceptanceFailed {
		t.Fatalf("runtime-owned result satisfied worker gate: %#v, %v", result, err)
	}
}

func TestAcceptanceRequiredWorkersCloned(t *testing.T) {
	original := AcceptanceSpec{RequiredWorkers: []string{"worker"}}
	cloned := cloneAcceptanceSpec(original)
	original.RequiredWorkers[0] = "other"
	if cloned.RequiredWorkers[0] != "worker" {
		t.Fatalf("acceptance clone retained caller slice: %#v", cloned.RequiredWorkers)
	}
}

func TestAcceptanceRequiredWorkersCountsAsConfiguredForReliability(t *testing.T) {
	session := &TeamSession{Config: agent.TeamConfig{AcceptanceSpec: &agent.AcceptanceSpec{RequiredWorkers: []string{"worker"}}}}
	if !acceptanceContractConfigured(session, nil) {
		t.Fatal("required workers acceptance was omitted from reliability reporting")
	}
}

func TestAcceptanceRequiredWorkersRejectsInvalidTeamReferences(t *testing.T) {
	session := policyTestSession()
	session.Config.AcceptanceSpec = &agent.AcceptanceSpec{RequiredWorkers: []string{"worker", "worker", "missing", "coordinator"}}
	findings := validateAcceptanceRequiredWorkers(session)
	if len(findings) != 3 {
		t.Fatalf("required worker findings = %#v, want duplicate, unknown, and coordinator", findings)
	}
}
