package evalharness

import (
	"fmt"

	"github.com/kjelly/hufu/internal/team"
)

// assertRun compares one case's ExpectSpec against the RunResult and events
// a real run actually produced. Only fields the fixture set are checked --
// see ExpectSpec's zero-value convention.
func assertRun(expect ExpectSpec, result *team.RunResult, events []team.StatusEvent) []EvalFinding {
	var findings []EvalFinding

	if expect.RunOutcome != "" {
		actual := ""
		if result != nil {
			actual = string(result.Outcome)
		}
		if actual != expect.RunOutcome {
			findings = append(findings, EvalFinding{Dimension: "run-outcome", Expected: expect.RunOutcome, Actual: actual})
		}
	}

	if expect.Acceptance != "" {
		actual := string(team.AcceptanceNotConfigured)
		if result != nil && result.Acceptance != nil {
			actual = string(result.Acceptance.EffectiveState())
		}
		if actual != expect.Acceptance {
			findings = append(findings, EvalFinding{Dimension: "acceptance", Expected: expect.Acceptance, Actual: actual})
		}
	}

	if expect.TaskCount != nil {
		actual := 0
		if result != nil {
			actual = result.Stats.TasksTotal
		}
		if actual != *expect.TaskCount {
			findings = append(findings, EvalFinding{
				Dimension: "task-count",
				Expected:  fmt.Sprintf("%d", *expect.TaskCount),
				Actual:    fmt.Sprintf("%d", actual),
			})
		}
	}

	for _, want := range expect.Events.Required {
		if !hasEventType(events, want) {
			findings = append(findings, EvalFinding{Dimension: "events." + want, Expected: "observed", Actual: "not observed"})
		}
	}

	return findings
}

func hasEventType(events []team.StatusEvent, want string) bool {
	for _, e := range events {
		if e.Type == want {
			return true
		}
	}
	return false
}
