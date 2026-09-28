package team

import (
	"strings"
	"testing"
)

// threeContractExecuteWorkflow returns a workflow in EXECUTE whose phase has
// three required contracts, like a review team's parallel review phase.
func threeContractExecuteWorkflow(t *testing.T) *runtimeWorkflow {
	t.Helper()
	session := workflowTestSession(t)
	session.ContractTasks = []TaskDef{
		{ID: "prepare", Agent: "preparer", Phase: PhasePrepare},
		{ID: "audit", Agent: "auditor", Phase: PhaseAudit},
		{ID: "review-primary", Agent: "executor", Phase: PhaseExecute},
		{ID: "review-docs", Agent: "auditor", Phase: PhaseExecute},
		{ID: "review-escalation", Agent: "verifier", Phase: PhaseExecute},
		{ID: "verify", Agent: "verifier", Phase: PhaseVerify},
	}
	w, err := newRuntimeWorkflow(session)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Start(); err != nil {
		t.Fatal(err)
	}
	for _, item := range []*TodoItem{
		{Agent: "preparer", ContractID: "prepare", Phase: PhasePrepare, Status: TaskDone},
		{Agent: "auditor", ContractID: "audit", Phase: PhaseAudit, Status: TaskDone},
	} {
		if err := w.observe([]*TodoItem{item}); err != nil {
			t.Fatal(err)
		}
	}
	if w.State() != PhaseExecute {
		t.Fatalf("workflow state = %s, want EXECUTE", w.State())
	}
	return w
}

func TestPhaseDispatchRejectionsListEveryContract(t *testing.T) {
	primary := TaskDef{Agent: "executor", ContractID: "review-primary", Phase: PhaseExecute}
	docs := TaskDef{Agent: "auditor", ContractID: "review-docs", Phase: PhaseExecute}
	escalation := TaskDef{Agent: "verifier", ContractID: "review-escalation", Phase: PhaseExecute}
	guide := `phase EXECUTE dispatches these static contracts together in one batch: ` +
		`review-docs (agent auditor); review-escalation (agent verifier); review-primary (agent executor)`
	tests := []struct {
		name  string
		tasks []TaskDef
		want  []string
	}{
		{name: "complete batch", tasks: []TaskDef{primary, docs, escalation}},
		{name: "two contracts missing", tasks: []TaskDef{docs}, want: []string{
			`must dispatch static contract review-escalation (agent verifier); review-primary (agent executor) in the same batch`,
			guide,
		}},
		{name: "task bound to another phase", tasks: []TaskDef{primary, {Agent: "executor", ContractID: "prepare", Phase: PhasePrepare}}, want: []string{
			`task for agent "executor" is bound to PREPARE`, guide,
		}},
		{name: "agent outside the phase", tasks: []TaskDef{{Agent: "preparer", ContractID: "review-docs", Phase: PhaseExecute}}, want: []string{
			`agent "preparer" is not authorized for workflow phase EXECUTE`, guide,
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			w := threeContractExecuteWorkflow(t)
			first := w.validateTasks(tt.tasks)
			if len(tt.want) == 0 {
				if first != nil {
					t.Fatalf("validateTasks: %v", first)
				}
				return
			}
			if first == nil {
				t.Fatal("dispatch was accepted")
			}
			for _, want := range tt.want {
				if !strings.Contains(first.Error(), want) {
					t.Fatalf("rejection = %q, want it to contain %q", first, want)
				}
			}
			// Map iteration once chose which missing contract to name; the
			// guidance must not depend on it.
			for range 20 {
				if again := w.validateTasks(tt.tasks); again == nil || again.Error() != first.Error() {
					t.Fatalf("rejection changed between identical dispatches: %v vs %v", again, first)
				}
			}
		})
	}
}
