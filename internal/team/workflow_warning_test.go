package team

import (
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

func TestShouldWarnPromptWorkflowDeprecation(t *testing.T) {
	tests := []struct {
		name string
		sess *TeamSession
		want bool
	}{
		{name: "nil session"},
		{name: "plain coordinator", sess: &TeamSession{}},
		{
			name: "runtime workflow",
			sess: &TeamSession{Config: agent.TeamConfig{Workflow: agent.WorkflowConfig{Phases: []string{"prepare", "audit", "verify"}}}},
		},
		{
			name: "static task contract",
			sess: &TeamSession{ContractTasks: []TaskDef{{ID: "review"}}},
			want: true,
		},
		{
			name: "delegation binding",
			sess: &TeamSession{Config: agent.TeamConfig{Delegation: agent.DelegationPolicy{BindTaskGoalContracts: true}}},
			want: true,
		},
		{
			name: "initial batch",
			sess: &TeamSession{Config: agent.TeamConfig{Delegation: agent.DelegationPolicy{InitialBatch: []string{"inventory"}}}},
			want: true,
		},
		{
			name: "goal invariant",
			sess: &TeamSession{Config: agent.TeamConfig{Delegation: agent.DelegationPolicy{TaskGoalInvariants: []agent.TaskGoalInvariant{{Agent: "reviewer"}}}}},
			want: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldWarnPromptWorkflowDeprecation(tt.sess); got != tt.want {
				t.Fatalf("shouldWarnPromptWorkflowDeprecation() = %v, want %v", got, tt.want)
			}
		})
	}
}
