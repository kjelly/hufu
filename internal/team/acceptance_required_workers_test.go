package team

import (
	"fmt"
	"path/filepath"
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

func TestAcceptanceRequiredWorkersAcceptGapsOnlyWhenTeamOptsIn(t *testing.T) {
	cases := []struct {
		status     string
		acceptGaps bool
		passed     bool
	}{
		{TaskResultStatusSuccess, false, true},
		{TaskResultStatusCompletedWithGaps, false, false},
		{TaskResultStatusPartial, false, false},
		{TaskResultStatusSuccess, true, true},
		{TaskResultStatusCompletedWithGaps, true, true},
		{TaskResultStatusPartial, true, false},
		{TaskResultStatusFailed, true, false},
		{TaskResultStatusBlocked, true, false},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%s/accept_gaps=%t", tc.status, tc.acceptGaps), func(t *testing.T) {
			c := &Coordinator{
				session:        &TeamSession{Config: agent.TeamConfig{Name: "research"}},
				projectDir:     t.TempDir(),
				taskTracker:    NewTaskTracker(),
				acceptanceSpec: &AcceptanceSpec{RequiredWorkers: []string{"researcher"}, RequiredWorkersAcceptGaps: tc.acceptGaps},
			}
			item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "researcher", Desc: "research"}})[0]
			c.taskTracker.TodoList().UpdateStatus(item.ID, TaskDone, "done")
			if err := c.taskTracker.TodoList().SetTypedResult(item.ID, &TaskResult{Status: tc.status, Source: "submitted"}); err != nil {
				t.Fatal(err)
			}
			result, err := c.runAcceptance(t.Context())
			if result.IsPassed() != tc.passed || (err == nil) != tc.passed {
				t.Fatalf("acceptance passed = %t, err = %v, want passed %t: %#v", result.IsPassed(), err, tc.passed, result)
			}
		})
	}
}

func TestAcceptanceRequiredWorkersAcceptGapsWithoutWorkersWarns(t *testing.T) {
	cases := []struct {
		name     string
		workers  []string
		warnings int
	}{
		{"no required workers", nil, 1},
		{"required workers", []string{"worker"}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			session := policyTestSession()
			session.Config.AcceptanceSpec = &agent.AcceptanceSpec{RequiredWorkers: tc.workers, RequiredWorkersAcceptGaps: true}
			warnings := 0
			for _, finding := range validateAcceptanceRequiredWorkers(session) {
				if finding.Field == "acceptance.required-workers-accept-gaps" && finding.Severity == FindingSeverityWarning {
					warnings++
				}
			}
			if warnings != tc.warnings {
				t.Fatalf("accept-gaps warnings = %d, want %d", warnings, tc.warnings)
			}
		})
	}
}

// Research stages may finish while recording an inaccessible source; staged
// coding stages must report success.
func TestBundledTeamsRequiredWorkerGapPolicy(t *testing.T) {
	cases := []struct {
		team       string
		acceptGaps bool
	}{
		{"web-research", true},
		{"hufu-coding", false},
	}
	for _, tc := range cases {
		t.Run(tc.team, func(t *testing.T) {
			session, err := LoadTeam(filepath.Join("..", "..", ".agent-teams", tc.team), nil, nil, DefaultProviderRegistry)
			if err != nil {
				t.Fatalf("LoadTeam(%s): %v", tc.team, err)
			}
			acceptance := session.Config.AcceptanceSpec
			if acceptance == nil || len(acceptance.RequiredWorkers) == 0 {
				t.Fatalf("%s declares no required workers: %#v", tc.team, acceptance)
			}
			if acceptance.RequiredWorkersAcceptGaps != tc.acceptGaps {
				t.Fatalf("%s required-workers-accept-gaps = %t, want %t", tc.team, acceptance.RequiredWorkersAcceptGaps, tc.acceptGaps)
			}
		})
	}
}
