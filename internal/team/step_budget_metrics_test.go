package team

import (
	"slices"
	"testing"
)

// TestAccumulateStepBudgetMetricsCountsExhaustedAttempts covers the counter that
// separates "these tasks needed more tool calls" from "this model ignored the
// result contract". Both land in failures_by_class as protocol failures, so
// without this counter the run report points at the wrong fix.
func TestAccumulateStepBudgetMetricsCountsExhaustedAttempts(t *testing.T) {
	tests := []struct {
		name     string
		receipts []ExecutionReceipt
		want     int
	}{
		{
			name: "no step budget recorded",
			receipts: []ExecutionReceipt{
				{RunID: "r", TaskID: "1", Attempt: 1},
			},
		},
		{
			name: "attempt stopped short of the budget",
			receipts: []ExecutionReceipt{
				{RunID: "r", TaskID: "1", Attempt: 1, StepBudget: &StepBudgetUsage{Used: 4, Limit: 30}},
			},
		},
		{
			name: "both attempts exhausted",
			receipts: []ExecutionReceipt{
				{RunID: "r", TaskID: "1", Attempt: 1, StepBudget: &StepBudgetUsage{Used: 30, Limit: 30, Exhausted: true}},
				{RunID: "r", TaskID: "1", Attempt: 2, StepBudget: &StepBudgetUsage{Used: 30, Limit: 30, Exhausted: true}},
			},
			want: 2,
		},
		{
			name: "a duplicated receipt is counted once",
			receipts: []ExecutionReceipt{
				{RunID: "r", TaskID: "1", Attempt: 1, StepBudget: &StepBudgetUsage{Used: 30, Limit: 30, Exhausted: true}},
				{RunID: "r", TaskID: "1", Attempt: 1, StepBudget: &StepBudgetUsage{Used: 30, Limit: 30, Exhausted: true}},
			},
			want: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			metrics := &RunMetrics{}
			accumulateStepBudgetMetrics(metrics, &TodoItem{ExecutionReceipts: tc.receipts})
			if metrics.StepBudgetExhaustions != tc.want {
				t.Fatalf("StepBudgetExhaustions = %d, want %d", metrics.StepBudgetExhaustions, tc.want)
			}
		})
	}
}

// TestDecisionChainReportsStepBudgetExhaustion checks the run's decision chain
// surfaces exhaustion alongside the failure classes it hides inside. A chain
// showing failure:protocol=15 on its own blames the model; the same chain with
// budget_exhausted=9 blames the budget.
func TestDecisionChainReportsStepBudgetExhaustion(t *testing.T) {
	c := &Coordinator{session: &TeamSession{}}

	withExhaustion := c.buildRunTelemetry(&RunResult{
		Outcome:    RunOutcomePartial,
		StopReason: StopReasonUnresolvedTasks,
		Metrics: RunMetrics{
			FailuresByClass:       map[TaskFailureClass]int{FailureProtocol: 15},
			StepBudgetExhaustions: 9,
		},
	})
	if !slices.Contains(withExhaustion.DecisionChain, "budget_exhausted=9") {
		t.Errorf("decision chain should report exhaustion: %v", withExhaustion.DecisionChain)
	}
	if !slices.Contains(withExhaustion.DecisionChain, "failure:protocol=15") {
		t.Errorf("decision chain should keep the failure class: %v", withExhaustion.DecisionChain)
	}

	without := c.buildRunTelemetry(&RunResult{
		Outcome:    RunOutcomeCompleted,
		StopReason: StopReasonCompleted,
		Metrics:    RunMetrics{FailuresByClass: map[TaskFailureClass]int{}},
	})
	for _, entry := range without.DecisionChain {
		if entry == "budget_exhausted=0" {
			t.Errorf("a run with no exhaustion should not carry the entry: %v", without.DecisionChain)
		}
	}
}

func TestToolDispositionMetricsUseOnlyActiveRun(t *testing.T) {
	items := []*TodoItem{{ExecutionReceipts: []ExecutionReceipt{
		{RunID: "old", TaskID: "1", Attempt: 1, ToolDispositions: []ToolExecutionDisposition{{Kind: ToolExecutionPolicyDenied, RetrySafety: RetrySafetySafeFreshAttempt}}},
		{RunID: "active", TaskID: "1", Attempt: 1, ToolDispositions: []ToolExecutionDisposition{
			{Kind: ToolExecutionPolicyDenied, RetrySafety: RetrySafetySafeFreshAttempt},
			{Kind: ToolExecutionBudgetExceeded, ReasonCode: "step_budget_wrap_up"},
			{Kind: ToolExecutionSchemaRepair},
		}},
	}}}
	metrics := RunMetrics{}
	accumulateToolDispositionMetrics(&metrics, items, "active")
	if metrics.PolicyDeniedToolCalls != 1 || metrics.SafeFreshAttempts != 1 || metrics.StepBudgetWrapUps != 1 || metrics.SchemaRepairDenials != 1 {
		t.Fatalf("active disposition metrics = %#v", metrics)
	}
}
