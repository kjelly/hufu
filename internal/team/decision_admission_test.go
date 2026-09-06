package team

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
)

func admissionForTest(t *testing.T, enabled bool) DecisionAdmission {
	t.Helper()
	task := decisionTask()
	digest, err := decisionTaskInputDigest(task)
	if err != nil {
		t.Fatal(err)
	}
	a := DecisionAdmission{SchemaVersion: DecisionAdmissionSchemaVersion, RunID: "run-a", TaskID: "todo-a", Attempt: 1, Profile: DecisionProfileOff, Source: DecisionProfileSourceDefault, TaskInputDigest: digest}
	if enabled {
		a.Profile = "standard"
		a.Source = DecisionProfileSourceTask
		a.Enabled = true
		a.DecisionID = "decision-a"
		policy := enginePolicy(2)
		a.Policy = &policy
	}
	return a
}

func TestDecisionAdmissionFreezesResolvedProfile(t *testing.T) {
	c := dispatchCoordinator(t, dispatchConfig())
	c.executionRunID = "run-frozen"
	task := decisionTask()
	if _, err := c.admitTaskOccurrence(context.Background(), task, "todo-frozen", 1); err != nil {
		t.Fatal(err)
	}
	// The task profile is empty: this admission came from the then-current
	// team default and must survive a later config mutation.
	c.session.Config.Decision.DefaultProfile = "standard"
	journal, err := c.decisionJournalFor()
	if err != nil {
		t.Fatal(err)
	}
	got, found, err := loadDecisionAdmission(context.Background(), journal, "todo-frozen", 1)
	if err != nil || !found || got.Profile != DecisionProfileOff {
		t.Fatalf("admission=%#v found=%v err=%v", got, found, err)
	}
}

func TestRetryCreatesNextDecisionAdmission(t *testing.T) {
	c := &Coordinator{
		sessionTime: time.Now(), eventJournal: &memoryJournal{}, taskTracker: NewTaskTracker(), reportStatus: func(StatusEvent) {},
		session: &TeamSession{Config: agent.TeamConfig{Decision: dispatchConfig()}},
	}
	item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "retry", DecisionOptions: decisionTask().DecisionOptions}})[0]
	if err := c.CommitTaskResetForRetry(context.Background(), item.ID, "retry"); err != nil {
		t.Fatal(err)
	}
	journal, err := c.decisionJournalFor()
	if err != nil {
		t.Fatal(err)
	}
	if _, found, err := loadDecisionAdmission(context.Background(), journal, item.ID, 2); err != nil || !found {
		t.Fatalf("retry admission found=%v err=%v", found, err)
	}
}
func TestDecisionAdmissionOffAndEnabled(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		j := &memoryJournal{}
		want := admissionForTest(t, enabled)
		got, err := appendDecisionAdmission(context.Background(), j, want)
		if err != nil {
			t.Fatal(err)
		}
		if got.Profile != want.Profile || got.Enabled != enabled {
			t.Fatalf("got %#v", got)
		}
		if j.count("decision_admitted") != 1 {
			t.Fatal("missing admission event")
		}
	}
}
func TestDecisionAdmissionIdempotentAndConflicting(t *testing.T) {
	j := &memoryJournal{}
	a := admissionForTest(t, true)
	if _, err := appendDecisionAdmission(context.Background(), j, a); err != nil {
		t.Fatal(err)
	}
	if _, err := appendDecisionAdmission(context.Background(), j, a); err != nil {
		t.Fatal(err)
	}
	if j.count("decision_admitted") != 1 {
		t.Fatalf("events=%d", j.count("decision_admitted"))
	}
	a.Profile = "other"
	if _, err := appendDecisionAdmission(context.Background(), j, a); err == nil || !strings.Contains(err.Error(), "conflicting") {
		t.Fatalf("err=%v", err)
	}
}
func TestDecisionAdmissionLookupByTaskAttempt(t *testing.T) {
	j := &memoryJournal{}
	a := admissionForTest(t, false)
	if _, err := appendDecisionAdmission(context.Background(), j, a); err != nil {
		t.Fatal(err)
	}
	got, found, err := loadDecisionAdmission(context.Background(), j, "todo-a", 1)
	if err != nil || !found || got.Profile != DecisionProfileOff {
		t.Fatalf("got=%#v found=%v err=%v", got, found, err)
	}
	if _, found, err := loadDecisionAdmission(context.Background(), j, "todo-a", 2); err != nil || found {
		t.Fatalf("found=%v err=%v", found, err)
	}
}

func TestDecisionAdmissionDigestBindsExecutableTaskContract(t *testing.T) {
	base := decisionTask()
	base.PlanFirst = true
	base.PlanID = "plan-1"
	base.Constraints = "preserve the old API"
	base.MaxRetries = 2
	base.SideEffect = SideEffectWorkspaceWrite
	base.Recovery = RecoveryRetry
	base.RecoveryHypothesis = &RecoveryHypothesis{
		ObservedFailure: "the first attempt failed", HypothesizedCause: "invalid input",
		ProposedChange: "validate input", DifferenceFromPrior: "new validation", ExpectedChange: "exit 0",
		Strategy: RecoveryStrategyToolChange,
	}
	base.Execution = ExecutionContract{RequiresResult: true}
	want, err := decisionTaskInputDigest(base)
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*TaskDef){
		"plan first":  func(task *TaskDef) { task.PlanFirst = false },
		"max retries": func(task *TaskDef) { task.MaxRetries = 3 },
		"recovery hypothesis": func(task *TaskDef) {
			task.RecoveryHypothesis = &RecoveryHypothesis{
				ObservedFailure: "the first attempt failed", HypothesizedCause: "different cause",
				ProposedChange: "change input", DifferenceFromPrior: "new hypothesis", ExpectedChange: "exit 0",
				Strategy: RecoveryStrategyToolChange,
			}
		},
		"goal":        func(task *TaskDef) { task.Goal = "a different migration" },
		"constraints": func(task *TaskDef) { task.Constraints = "do not change the schema" },
		"side effect": func(task *TaskDef) { task.SideEffect = SideEffectExternalWrite },
		"execution":   func(task *TaskDef) { task.Execution.RequiresVerification = true },
	} {
		t.Run(name, func(t *testing.T) {
			changed := base
			mutate(&changed)
			got, err := decisionTaskInputDigest(changed)
			if err != nil {
				t.Fatal(err)
			}
			if got == want {
				t.Fatalf("digest did not bind changed %s", name)
			}
		})
	}
	planIDOnly := base
	planIDOnly.PlanID = "plan-2"
	planIDDigest, err := decisionTaskInputDigest(planIDOnly)
	if err != nil {
		t.Fatal(err)
	}
	if planIDDigest != want {
		t.Fatalf("PlanID-only digest changed: got %q, want %q", planIDDigest, want)
	}
}

func TestProtocolRepairRejectsDurableTaskWithoutAdmission(t *testing.T) {
	c := dispatchCoordinator(t, dispatchConfig())
	item := todoItemFromSpec(TodoSpec{Agent: "worker", Desc: "repair only", Goal: "repair only", Execution: ExecutionContract{RequiresResult: true}}, "todo-protocol")
	item.Status, item.Output = TaskProtocolIncomplete, "checkpointed worker output"
	c.taskTracker.TodoList().Restore([]*TodoItem{item})
	repairCalls := 0
	c.repairAgentOverride = &scriptedRepairAgent{calls: &repairCalls}

	if _, err := c.executeTask(context.Background(), taskDefFromTodoItem(item), item.ID); err == nil || !strings.Contains(err.Error(), "has no decision admission") {
		t.Fatalf("executeTask = %v, want missing durable admission", err)
	}
	if repairCalls != 0 {
		t.Fatalf("repair calls = %d, want none before admission validation", repairCalls)
	}
}

func TestResumeProtocolRepairRejectsDurableTaskWithoutAdmission(t *testing.T) {
	c := dispatchCoordinator(t, dispatchConfig())
	item := todoItemFromSpec(TodoSpec{Agent: "worker", Desc: "repair only", Goal: "repair only", Execution: ExecutionContract{RequiresResult: true}}, "todo-resume-protocol")
	item.Status, item.Output = TaskProtocolIncomplete, "checkpointed worker output"
	c.taskTracker.TodoList().Restore([]*TodoItem{item})
	repairCalls := 0
	c.repairAgentOverride = &scriptedRepairAgent{calls: &repairCalls}

	count, err := c.ResumeInterruptedTasks(context.Background())
	if count != 1 || err == nil || !strings.Contains(err.Error(), "has no decision admission") {
		t.Fatalf("ResumeInterruptedTasks count=%d err=%v, want blocked markerless protocol repair", count, err)
	}
	if repairCalls != 0 {
		t.Fatalf("repair calls = %d, want none before admission validation", repairCalls)
	}
}
