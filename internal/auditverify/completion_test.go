package auditverify

import (
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func TestDeriveCompletionAuditNonCompletedIsAlwaysPass(t *testing.T) {
	for _, outcome := range []team.RunOutcome{team.RunOutcomeFailed, team.RunOutcomePartial, team.RunOutcomeCancelled, team.RunOutcomeBlocked} {
		dim := DeriveCompletionAudit(CompletionAuditInput{RunResult: &team.RunResult{Outcome: outcome}})
		if dim.Status != AuditDimensionPass {
			t.Fatalf("outcome %s: dimension = %#v, want pass", outcome, dim)
		}
	}
}

func TestDeriveCompletionAuditCompletedRequiresAllInvariants(t *testing.T) {
	base := func() CompletionAuditInput {
		return CompletionAuditInput{
			RunResult:              &team.RunResult{Outcome: team.RunOutcomeCompleted, GoalSatisfied: true},
			EvidenceValid:          true,
			EvidenceStatus:         "accepted",
			AcceptanceState:        team.AcceptancePassed,
			RequiredTasksComplete:  true,
			CompletionGateAccepted: true,
		}
	}
	if dim := DeriveCompletionAudit(base()); dim.Status != AuditDimensionPass {
		t.Fatalf("fully justified completed run = %#v, want pass", dim)
	}

	cases := []struct {
		name   string
		mutate func(*CompletionAuditInput)
	}{
		{"invalid evidence", func(in *CompletionAuditInput) { in.EvidenceValid = false }},
		{"acceptance not passed", func(in *CompletionAuditInput) { in.AcceptanceState = team.AcceptanceFailed }},
		{"tasks incomplete", func(in *CompletionAuditInput) { in.RequiredTasksComplete = false }},
		{"provenance rejected", func(in *CompletionAuditInput) { in.CompletionGateAccepted = false }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			input := base()
			tc.mutate(&input)
			dim := DeriveCompletionAudit(input)
			if dim.Status != AuditDimensionFail {
				t.Fatalf("dimension = %#v, want fail", dim)
			}
		})
	}
}

func TestDeriveCompletionAuditNilRunResultIsIncomplete(t *testing.T) {
	dim := DeriveCompletionAudit(CompletionAuditInput{})
	if dim.Status != AuditDimensionIncomplete {
		t.Fatalf("nil run result dimension = %#v, want incomplete", dim)
	}
}
