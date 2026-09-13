package team

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func TestLoadTeamContractTasksParsesInvariantVerification(t *testing.T) {
	dir := t.TempDir()
	manifest := `name: contract-team
tasks:
  - id: review
    agent: reviewer
    goal: review
    phase: VERIFY
    invariant-verification: report
    execution:
      requires_result: true
`
	if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte(manifest), 0o600); err != nil {
		t.Fatal(err)
	}
	tasks, err := loadTeamContractTasks(dir, nil)
	if err != nil {
		t.Fatalf("loadTeamContractTasks: %v", err)
	}
	if len(tasks) != 1 || tasks[0].InvariantVerification != InvariantVerificationReport {
		t.Fatalf("tasks = %#v", tasks)
	}
}

func TestInvariantVerificationIsConfigurationOnlyAndDurable(t *testing.T) {
	task := TaskDef{
		ID: "verify", Agent: "reviewer", Goal: "review",
		Phase: PhaseVerify, InvariantVerification: InvariantVerificationGate,
		Execution: ExecutionContract{RequiresResult: true},
	}
	encoded, err := json.Marshal(task)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "invariant_verification") {
		t.Fatalf("configuration-only mode leaked into coordinator JSON: %s", encoded)
	}

	item := todoItemFromSpec(TodoSpec{
		PlanTaskID: task.ID, Agent: task.Agent, Goal: task.Goal, Phase: task.Phase,
		InvariantVerification: task.InvariantVerification, Execution: task.Execution,
	}, "todo-1")
	if item.InvariantVerification != InvariantVerificationGate {
		t.Fatalf("Todo mode = %q", item.InvariantVerification)
	}
	rebuilt := taskDefFromTodoItem(item)
	if rebuilt.InvariantVerification != InvariantVerificationGate {
		t.Fatalf("rebuilt task mode = %q", rebuilt.InvariantVerification)
	}
	projection, err := newTaskOccurrenceProjection(item)
	if err != nil {
		t.Fatal(err)
	}
	if projection.InvariantVerification != InvariantVerificationGate {
		t.Fatalf("projection mode = %q", projection.InvariantVerification)
	}
}

func TestInvariantVerificationChangesStaticContractHash(t *testing.T) {
	base := TaskDef{ID: "verify", Agent: "reviewer", Execution: ExecutionContract{RequiresResult: true}}
	report := base
	report.InvariantVerification = InvariantVerificationReport
	baseHash, err := effectiveContractHash(base.ID, base.Agent, base.Execution, "", "", "", 0, nil, nil, false, base)
	if err != nil {
		t.Fatal(err)
	}
	reportHash, err := effectiveContractHash(report.ID, report.Agent, report.Execution, "", "", "", 0, nil, nil, false, report)
	if err != nil {
		t.Fatal(err)
	}
	if baseHash == reportHash {
		t.Fatal("invariant-verification mode is absent from static contract hash")
	}
}

func TestValidateInvariantVerificationContract(t *testing.T) {
	worker := &agent.AgentDef{Name: "reviewer", Role: "worker"}
	validTask := TaskDef{
		Agent: "reviewer", Phase: PhaseVerify,
		InvariantVerification: InvariantVerificationReport,
		Execution:             ExecutionContract{RequiresResult: true},
	}
	base := &TeamSession{
		Config:           agent.TeamConfig{Workflow: agent.WorkflowConfig{Phases: []string{string(PhaseVerify)}}},
		Agents:           map[string]*agent.AgentDef{"reviewer": worker},
		InvariantCatalog: []InvariantDefinition{{ID: "safe", Statement: "safe", Severity: InvariantSeverityError, AppliesTo: []string{"*"}}},
	}

	tests := []struct {
		name string
		edit func(*TeamSession, *TaskDef)
		code string
	}{
		{name: "bad mode", edit: func(_ *TeamSession, task *TaskDef) { task.InvariantVerification = "bad" }, code: FindingInvariantVerificationMode},
		{name: "catalog", edit: func(session *TeamSession, _ *TaskDef) { session.InvariantCatalog = nil }, code: FindingInvariantVerificationCatalog},
		{name: "phase", edit: func(_ *TeamSession, task *TaskDef) { task.Phase = PhaseExecute }, code: FindingInvariantVerificationPhase},
		{name: "result", edit: func(_ *TeamSession, task *TaskDef) { task.Execution.RequiresResult = false }, code: FindingInvariantVerificationResult},
		{name: "sidecar", edit: func(_ *TeamSession, task *TaskDef) { task.Sidecar = true }, code: FindingInvariantVerificationTopology},
		{name: "extra models", edit: func(_ *TeamSession, task *TaskDef) { task.ModelTopology = []string{"a", "b"} }, code: FindingInvariantVerificationTopology},
		{name: "optional gate", edit: func(_ *TeamSession, task *TaskDef) {
			task.InvariantVerification = InvariantVerificationGate
			task.Optional = true
		}, code: FindingInvariantVerificationOptional},
	}
	if findings := validateInvariantVerificationContract(base, 0, validTask); len(findings) != 0 {
		t.Fatalf("valid invariant verification contract findings = %#v", findings)
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session := *base
			session.InvariantCatalog = cloneInvariantCatalog(base.InvariantCatalog)
			task := validTask
			if test.edit != nil {
				test.edit(&session, &task)
			}
			findings := validateInvariantVerificationContract(&session, 0, task)
			if !hasContractFindingCode(findings, test.code) {
				t.Fatalf("findings = %#v, want code %q", findings, test.code)
			}
		})
	}
}

func TestCloneSessionDoesNotAliasInvariantCatalog(t *testing.T) {
	original := &TeamSession{InvariantCatalog: []InvariantDefinition{{ID: "safe", AppliesTo: []string{"internal/team/"}}}}
	cloned := cloneSession(original, t.TempDir())
	cloned.InvariantCatalog[0].AppliesTo[0] = "changed"
	if original.InvariantCatalog[0].AppliesTo[0] != "internal/team/" {
		t.Fatal("cloneSession aliases invariant catalog")
	}
}

func hasContractFindingCode(findings []ContractFinding, code string) bool {
	for _, finding := range findings {
		if finding.Code == code {
			return true
		}
	}
	return false
}
