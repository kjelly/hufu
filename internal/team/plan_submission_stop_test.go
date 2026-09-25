package team

import "testing"

// TestPlanSubmissionStopEndsOnlyThePlanningPhase guards E-27: a planning
// worker holds its full tool set, so its stream must end once its plan is
// submitted, while the execution of an approved plan runs to completion.
func TestPlanSubmissionStopEndsOnlyThePlanningPhase(t *testing.T) {
	cases := []struct {
		name       string
		task       TaskDef
		planStatus string
		wantStops  int
		wantStop   bool
	}{
		{name: "planning phase after submission", task: TaskDef{PlanFirst: true}, planStatus: "submitted", wantStops: 1, wantStop: true},
		{name: "planning phase before submission", task: TaskDef{PlanFirst: true}, wantStops: 1, wantStop: false},
		{name: "re-planning after rejection", task: TaskDef{PlanFirst: true}, planStatus: "rejected", wantStops: 1, wantStop: false},
		{name: "approved plan execution", task: TaskDef{PlanFirst: true, PlanID: "1"}, planStatus: "approved", wantStops: 0},
		{name: "task without plan-first", task: TaskDef{}, planStatus: "submitted", wantStops: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := &Coordinator{pendingPlans: map[string]*PlanEntry{}}
			if tc.planStatus != "" {
				c.pendingPlans["1"] = &PlanEntry{TodoID: "1", PlanText: "1. do it", Status: tc.planStatus}
			}
			stops := c.planSubmissionStop(tc.task, "1")
			if len(stops) != tc.wantStops {
				t.Fatalf("planSubmissionStop() returned %d conditions, want %d", len(stops), tc.wantStops)
			}
			if tc.wantStops == 1 {
				if got := stops[0](nil); got != tc.wantStop {
					t.Fatalf("stop condition = %v, want %v", got, tc.wantStop)
				}
			}
			if got := c.planSubmittedFor("1"); got != (tc.planStatus == "submitted") {
				t.Fatalf("planSubmittedFor() = %v, want %v", got, tc.planStatus == "submitted")
			}
		})
	}
}
