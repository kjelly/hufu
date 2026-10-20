package improver

import (
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func TestStageTasksNeverPromotesUnknownEvidence(t *testing.T) {
	for _, test := range []struct {
		name       string
		state      string
		settlement *Settlement
		want       team.TaskStatus
	}{
		{"planned", "planned", nil, team.TaskPending},
		{"started", "started", nil, team.TaskInProgress},
		{"missing_result", "completed", nil, team.TaskBlocked},
		{"unknown_usage", "completed", &Settlement{Outcome: "passed"}, team.TaskBlocked},
		{"passed", "completed", &Settlement{Outcome: "passed", UsageKnown: true}, team.TaskDone},
		{"functional_failure", "completed", &Settlement{Outcome: "failed", UsageKnown: true}, team.TaskError},
		{"conflict", "blocked", &Settlement{Outcome: "passed", UsageKnown: true}, team.TaskBlocked},
	} {
		t.Run(test.name, func(t *testing.T) {
			stage := &StageState{State: test.state, Attempts: []AttemptState{{Settlement: test.settlement}}}
			state := LedgerState{Stages: map[string]*StageState{"probe_0001": stage}}
			items := StageTasks(state)
			if len(items) != 1 || items[0].Status != test.want {
				t.Fatalf("status = %+v, want %s", items, test.want)
			}
			items[0].Status = team.TaskDone
			if stage.State != test.state {
				t.Fatal("presentation mutated canonical stage")
			}
		})
	}
}
