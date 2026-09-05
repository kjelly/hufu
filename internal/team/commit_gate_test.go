package team

import (
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func mutatingTask() TaskDef {
	return TaskDef{
		ID:                  "t1",
		Agent:               "deployer",
		Goal:                "create the bridge",
		SideEffect:          SideEffectInfraMutation,
		Recovery:            RecoveryReconcile,
		ReconcileTool:       "probe-state",
		ExpectedStateChange: "ovs bridge br0 exists",
		Verify:              "test -n \"$(ovs-vsctl br-exists br0)\"",
		VerifySpec: &VerificationSpec{
			Type:       agent.VerifyTaskResultAssert,
			Assertions: []agent.JSONAssertion{{Path: "/status", Equals: "ok"}},
		},
	}
}

// A task whose side effect is not guarded is not the commit gate's business.
func TestCommitGateOnlyGuardsConfiguredClasses(t *testing.T) {
	policy := CommitGatePolicy{RequireVerification: true, RequireEvidence: true, RequireReconcile: true}

	for _, class := range []SideEffectClass{SideEffectNone, SideEffectWorkspaceWrite} {
		task := TaskDef{ID: "t1", SideEffect: class}
		decision := EvaluateCommitGate(CommitGateInput{Task: task, Policy: policy})
		if decision.Applicable || !decision.Allowed {
			t.Fatalf("class %q: decision = %#v, want not applicable", class, decision)
		}
	}
	for _, class := range []SideEffectClass{SideEffectExternalWrite, SideEffectInfraMutation, SideEffectCredential, SideEffectUnknown} {
		task := TaskDef{ID: "t1", SideEffect: class}
		decision := EvaluateCommitGate(CommitGateInput{Task: task, Policy: policy})
		if !decision.Applicable {
			t.Fatalf("class %q was not guarded by default", class)
		}
	}

	// An explicit class list replaces the default rather than adding to it.
	narrow := policy
	narrow.RequiredForSideEffects = []string{string(SideEffectCredential)}
	decision := EvaluateCommitGate(CommitGateInput{
		Task: TaskDef{ID: "t1", SideEffect: SideEffectInfraMutation}, Policy: narrow,
	})
	if decision.Applicable {
		t.Fatal("infra_mutation was guarded despite an explicit credential-only list")
	}
}

func TestCommitGatePrerequisites(t *testing.T) {
	tests := []struct {
		name    string
		policy  CommitGatePolicy
		mutate  func(*TaskDef)
		wantAll bool
		want    string
	}{
		{
			name:    "fully specified task passes",
			policy:  CommitGatePolicy{RequireVerification: true, RequireEvidence: true, RequireReconcile: true, RequireObservability: true},
			wantAll: true,
		},
		{
			name:   "missing verification",
			policy: CommitGatePolicy{RequireVerification: true},
			mutate: func(task *TaskDef) { task.Verify = ""; task.VerifySpec = nil },
			want:   ReasonCommitGateMissingVerification,
		},
		{
			name:    "a plain verify command satisfies verification",
			policy:  CommitGatePolicy{RequireVerification: true},
			mutate:  func(task *TaskDef) { task.VerifySpec = nil },
			wantAll: true,
		},
		{
			name:   "missing evidence",
			policy: CommitGatePolicy{RequireEvidence: true},
			mutate: func(task *TaskDef) { task.VerifySpec = nil },
			want:   ReasonCommitGateMissingEvidence,
		},
		{
			name:   "a verify_spec without assertions is not evidence",
			policy: CommitGatePolicy{RequireEvidence: true},
			mutate: func(task *TaskDef) {
				task.VerifySpec = &VerificationSpec{Type: agent.VerifyCommandExit, Command: "true"}
			},
			want: ReasonCommitGateMissingEvidence,
		},
		{
			name:   "missing reconcile policy",
			policy: CommitGatePolicy{RequireReconcile: true},
			mutate: func(task *TaskDef) { task.Recovery = RecoveryRetry },
			want:   ReasonCommitGateMissingReconcile,
		},
		{
			name:   "reconcile policy without a probe",
			policy: CommitGatePolicy{RequireReconcile: true},
			mutate: func(task *TaskDef) { task.ReconcileTool = "" },
			want:   ReasonCommitGateMissingReconcile,
		},
		{
			name:   "missing observability",
			policy: CommitGatePolicy{RequireObservability: true},
			mutate: func(task *TaskDef) { task.ExpectedStateChange = "" },
			want:   ReasonCommitGateMissingObservability,
		},
		{
			name:   "missing compensating operation",
			policy: CommitGatePolicy{RequireRollback: true},
			want:   ReasonCommitGateMissingRecovery,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			task := mutatingTask()
			if tt.mutate != nil {
				tt.mutate(&task)
			}
			decision := EvaluateCommitGate(CommitGateInput{Task: task, Policy: tt.policy})
			if tt.wantAll {
				if !decision.Allowed {
					t.Fatalf("decision = %#v, want allowed", decision)
				}
				return
			}
			if decision.Allowed {
				t.Fatalf("decision = %#v, want blocked with %s", decision, tt.want)
			}
			found := false
			for _, reason := range decision.Missing {
				if reason == tt.want {
					found = true
				}
			}
			if !found {
				t.Fatalf("missing reasons = %v, want %s", decision.Missing, tt.want)
			}
		})
	}
}

// The gate reports every unsatisfied prerequisite so an operator fixes the
// contract once rather than iterating.
func TestCommitGateReportsAllMissingPrerequisites(t *testing.T) {
	task := TaskDef{ID: "t1", SideEffect: SideEffectInfraMutation}
	decision := EvaluateCommitGate(CommitGateInput{
		Task: task,
		Policy: CommitGatePolicy{
			RequireVerification: true, RequireEvidence: true,
			RequireReconcile: true, RequireObservability: true, RequireRollback: true,
		},
	})
	if decision.Allowed || len(decision.Missing) != 5 {
		t.Fatalf("missing = %v, want all five prerequisites", decision.Missing)
	}
	if !strings.Contains(decision.Error(), decision.Reason) {
		t.Fatalf("Error() = %q, want it to name the reason code", decision.Error())
	}
}

// A compensating tool satisfies require-rollback: this repo has no rollback
// recovery policy, only ToolRecoverySpec.CompensateTool (spec §30.1).
func TestCommitGateRollbackUsesCompensateTool(t *testing.T) {
	decision := EvaluateCommitGate(CommitGateInput{
		Task:         mutatingTask(),
		Policy:       CommitGatePolicy{RequireRollback: true},
		ToolRecovery: ToolRecoverySpec{CompensateTool: "delete-bridge"},
	})
	if !decision.Allowed {
		t.Fatalf("decision = %#v, want a compensating tool to satisfy require-rollback", decision)
	}
}
