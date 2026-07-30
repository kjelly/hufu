package team

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/anomalyco/hufu/internal/agent"
)

func TestNamedCriteriaPersistAndRespectDependencies(t *testing.T) {
	dir := t.TempDir()
	c := &Coordinator{session: &TeamSession{Config: agent.TeamConfig{Name: "test"}}, projectDir: dir, taskTracker: NewTaskTracker(), sessionData: NewSession()}
	c.acceptanceSpec = &AcceptanceSpec{Criteria: []AcceptanceCriterion{
		{ID: "build", Required: true, Verify: VerificationSpec{Type: VerifyCommandExit, Command: "true"}},
		{ID: "report", Required: true, DependsOn: []string{"build"}, Verify: VerificationSpec{Type: VerifyCommandExit, Command: "true"}},
	}}
	result, err := c.runAcceptance(context.Background())
	if err != nil || !result.Passed {
		t.Fatalf("named criteria should pass: result=%#v err=%v", result, err)
	}
	if len(result.CriterionResults) != 2 || len(c.sessionData.CriterionResults) != 2 {
		t.Fatalf("criterion results were not persisted: %#v / %#v", result.CriterionResults, c.sessionData.CriterionResults)
	}

	c.acceptanceSpec.Criteria[0].Verify.Command = "false"
	result, err = c.runAcceptance(context.Background())
	if err == nil || result.Passed {
		t.Fatalf("failed required criterion must fail acceptance: %#v", result)
	}
	if result.CriterionResults[1].State != CriterionBlocked {
		t.Fatalf("dependent criterion = %s, want blocked", result.CriterionResults[1].State)
	}
}

func TestCriterionFingerprintIncludesRevisionAndSecurityInputs(t *testing.T) {
	criteria := []AcceptanceCriterion{{ID: "ready", Required: true, Verify: VerificationSpec{Type: VerifyCommandExit, Command: "true"}}}
	newCoordinator := func(revision int, noNet bool) *Coordinator {
		return &Coordinator{
			session:    &TeamSession{Config: agent.TeamConfig{Name: "test", NoNet: noNet}},
			projectDir: dirForCriteriaTest(t), sessionData: NewSession(), acceptanceContractRevision: revision,
		}
	}
	c1 := newCoordinator(1, false)
	r1, err := c1.evaluateCriteria(context.Background(), criteria)
	if err != nil || len(r1) != 1 {
		t.Fatalf("first evaluation failed: %#v %v", r1, err)
	}
	c2 := newCoordinator(2, true)
	r2, err := c2.evaluateCriteria(context.Background(), criteria)
	if err != nil || len(r2) != 1 {
		t.Fatalf("second evaluation failed: %#v %v", r2, err)
	}
	if r1[0].InputFingerprint == "" || r1[0].InputFingerprint == r2[0].InputFingerprint {
		t.Fatalf("criterion fingerprint ignored revision/security inputs: %q == %q", r1[0].InputFingerprint, r2[0].InputFingerprint)
	}
}

func TestDiagnosticTaskRequiresUncertaintyDeclaration(t *testing.T) {
	c := &Coordinator{session: &TeamSession{Config: agent.TeamConfig{Name: "test", GoalMode: "outcome"}}}
	if err := c.validateTaskCriterionLinks([]TaskDef{{Kind: TaskKindDiagnostic, Goal: "inspect"}}); err == nil {
		t.Fatal("diagnostic task without expected state change should be rejected")
	}
	if err := c.validateTaskCriterionLinks([]TaskDef{{Kind: TaskKindDiagnostic, Goal: "inspect", ExpectedStateChange: "resolve whether artifact is stale"}}); err != nil {
		t.Fatalf("declared diagnostic uncertainty should be accepted: %v", err)
	}
}

func TestCriterionRetryTargetsFailedCriteria(t *testing.T) {
	tasks := []TaskDef{
		{Kind: TaskKindOutcome, Advances: []string{"build"}},
		{Kind: TaskKindDiagnostic, Advances: []string{"build"}, ExpectedStateChange: "inspect build failure"},
		{Kind: TaskKindRepair, Advances: []string{"build"}},
		{Kind: TaskKindRepair, Advances: []string{"test"}},
	}
	got := criterionRetryTargets(tasks, []string{"build"})
	if len(got) != 2 || got[0] != 0 || got[1] != 2 {
		t.Fatalf("criterion retry targets = %#v, want [0 2]", got)
	}
}

func TestSuccessfulNoProgressWithFailedCriterionIsRetryEligible(t *testing.T) {
	if !criterionRetryEligible(ProgressNoChange, []string{"build"}) {
		t.Fatal("successful task with no criterion progress should route to remediation")
	}
	if criterionRetryEligible(ProgressAdvanced, []string{"build"}) {
		t.Fatal("advanced criterion must not route as no-progress")
	}
	if criterionRetryEligible(ProgressNoChange, nil) {
		t.Fatal("no failed criterion must not route")
	}
}

func dirForCriteriaTest(t *testing.T) string {
	t.Helper()
	return t.TempDir()
}

func TestCriteriaUseConfiguredVerificationShell(t *testing.T) {
	dir := t.TempDir()
	c := &Coordinator{
		session:     &TeamSession{Config: agent.TeamConfig{Name: "test", Shell: "bash"}},
		projectDir:  dir,
		taskTracker: NewTaskTracker(),
		sessionData: NewSession(),
	}
	c.acceptanceSpec = &AcceptanceSpec{Criteria: []AcceptanceCriterion{{
		ID: "bash-expression", Required: true,
		Verify: VerificationSpec{Type: VerifyCommandExit, Command: "[[ 1 -eq 1 ]]"},
	}}}
	result, err := c.runAcceptance(context.Background())
	if err != nil || !result.Passed {
		t.Fatalf("configured shell should evaluate criterion: result=%#v err=%v", result, err)
	}
}

func TestCriterionAndTaskProgressPersistInSession(t *testing.T) {
	dir := t.TempDir()
	session := NewSession()
	session.CriterionResults = []CriterionResult{{ID: "artifact", State: CriterionPassed, EvaluatedAt: time.Now().UTC(), InputFingerprint: "vfp_test"}}
	session.Tasks = []*TodoItem{{ID: "1", Kind: TaskKindRepair, Advances: []string{"artifact"}, ExpectedStateChange: "artifact exists", Progress: ProgressAdvanced}}
	if err := SaveSession(dir, session); err != nil {
		t.Fatal(err)
	}
	loaded := LoadSession(dir)
	if loaded == nil || len(loaded.CriterionResults) != 1 || loaded.CriterionResults[0].State != CriterionPassed {
		t.Fatalf("criterion result did not persist: %#v", loaded)
	}
	if len(loaded.Tasks) != 1 || loaded.Tasks[0].Kind != TaskKindRepair || loaded.Tasks[0].Progress != ProgressAdvanced || len(loaded.Tasks[0].Advances) != 1 {
		t.Fatalf("task progress metadata did not persist: %#v", loaded.Tasks)
	}
}

func TestCriterionAndTaskProgressReplayFromEvents(t *testing.T) {
	encode := func(value any) json.RawMessage {
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return data
	}
	events := []RunEvent{
		{Type: "task_created", TaskID: "1", Payload: encode(map[string]any{"id": "1", "desc": "repair", "kind": TaskKindRepair, "advances": []string{"artifact"}, "progress": ProgressAdvanced})},
		{Type: "criterion_re_evaluated", Payload: encode(map[string]any{"after": []CriterionResult{{ID: "artifact", State: CriterionPassed}}})},
	}
	items := ReduceToTodoList(events)
	if len(items) != 1 || items[0].Kind != TaskKindRepair || items[0].Progress != ProgressAdvanced || len(items[0].Advances) != 1 {
		t.Fatalf("task metadata replay mismatch: %#v", items)
	}
	session := ReduceToSessionData(events)
	if len(session.CriterionResults) != 1 || session.CriterionResults[0].State != CriterionPassed {
		t.Fatalf("criterion replay mismatch: %#v", session.CriterionResults)
	}
}

func TestOutcomeTaskReevaluatesAdvancedCriterion(t *testing.T) {
	dir := t.TempDir()
	c := &Coordinator{session: &TeamSession{Config: agent.TeamConfig{Name: "test"}}, projectDir: dir, taskTracker: NewTaskTracker(), sessionData: NewSession()}
	c.acceptanceSpec = &AcceptanceSpec{Criteria: []AcceptanceCriterion{{ID: "artifact", Required: true, Verify: VerificationSpec{Type: VerifyFileExists, Path: "report.txt"}}}}
	if _, err := c.evaluateCriteria(context.Background(), c.acceptanceSpec.Criteria); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "report.txt"), []byte("done"), 0o644); err != nil {
		t.Fatal(err)
	}
	item := &TodoItem{ID: "1", Kind: TaskKindOutcome, Advances: []string{"artifact"}}
	c.reEvaluateAffectedCriteria(context.Background(), item)
	if item.Progress != ProgressAdvanced {
		t.Fatalf("progress = %s, want advanced", item.Progress)
	}
	if got := c.sessionData.CriterionResults[0].State; got != CriterionPassed {
		t.Fatalf("criterion state = %s, want passed", got)
	}
}
