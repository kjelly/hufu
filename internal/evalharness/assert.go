package evalharness

import (
	"fmt"
	"regexp"
	"sort"
	"strings"

	"github.com/kjelly/hufu/internal/team"
)

// runIDPattern matches team.RunResult.RunID's opaque timestamp+random-hex
// suffix (e.g. "run-20260912T093923.493930048Z-97f23d1c6c58"), the one
// genuinely non-deterministic identifier a completed deterministic case
// produces (TodoItem.ID is a plain sequence counter, not opaque).
var runIDPattern = regexp.MustCompile(`run-\d{8}T\d{6}\.\d+Z-[0-9a-f]+`)

// normalizeOpaqueID redacts a RunID (wherever it appears in a larger string)
// so two runs of the same deterministic case report byte-identical
// findings/metrics instead of differing solely by run identity.
func normalizeOpaqueID(s string) string {
	return runIDPattern.ReplaceAllString(s, "<run-id>")
}

// assertRun compares one case's ExpectSpec against everything a real run
// actually produced: the RunResult, its reported events, and the durable
// task list in creation order. Only fields/entries the fixture set are
// checked -- see ExpectSpec's and TaskExpect's zero-value convention.
func assertRun(expect ExpectSpec, result *team.RunResult, events []team.StatusEvent, tasks []*team.TodoItem) []EvalFinding {
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

	if expect.StopReason != "" {
		actual := ""
		if result != nil {
			actual = string(result.StopReason)
		}
		if actual != expect.StopReason {
			findings = append(findings, EvalFinding{Dimension: "stop-reason", Expected: expect.StopReason, Actual: actual})
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

	findings = append(findings, assertTasks(expect.Tasks, tasks)...)

	for _, want := range expect.Events.Required {
		if !hasEventType(events, want) {
			findings = append(findings, EvalFinding{Dimension: "events." + want, Expected: "observed", Actual: "not observed"})
		}
	}
	findings = append(findings, assertEventOrder(expect.Events.Order, events)...)

	if expect.ExecutionTargetFrozen != nil && *expect.ExecutionTargetFrozen {
		findings = append(findings, assertExecutionTargetFrozen(events)...)
	}

	return findings
}

// assertExecutionTargetFrozen checks that every StatusEvent carrying a
// non-empty ExecutionTarget across the run reports the same value.
func assertExecutionTargetFrozen(events []team.StatusEvent) []EvalFinding {
	seen := map[string]bool{}
	for _, e := range events {
		if e.ExecutionTarget != "" {
			seen[e.ExecutionTarget] = true
		}
	}
	if len(seen) <= 1 {
		return nil
	}
	distinct := make([]string, 0, len(seen))
	for target := range seen {
		distinct = append(distinct, target)
	}
	sort.Strings(distinct)
	return []EvalFinding{{
		Dimension: "execution-target-frozen",
		Expected:  "a single ExecutionTarget across the whole run",
		Actual:    fmt.Sprintf("%d distinct values: %s", len(distinct), strings.Join(distinct, ", ")),
	}}
}

func hasEventType(events []team.StatusEvent, want string) bool {
	for _, e := range events {
		if e.Type == want {
			return true
		}
	}
	return false
}

// assertTasks matches Tasks[i] against the i-th durable task in creation
// order (see TaskExpect's doc comment for why position, not ID).
func assertTasks(expectTasks []TaskExpect, tasks []*team.TodoItem) []EvalFinding {
	var findings []EvalFinding
	for i, want := range expectTasks {
		dim := fmt.Sprintf("tasks[%d]", i)
		if i >= len(tasks) || tasks[i] == nil {
			findings = append(findings, EvalFinding{Dimension: dim, Expected: "a task at this position", Actual: fmt.Sprintf("only %d task(s) observed", len(tasks))})
			continue
		}
		actual := tasks[i]
		if want.Agent != "" && actual.Agent != want.Agent {
			findings = append(findings, EvalFinding{Dimension: dim + ".agent", Expected: want.Agent, Actual: actual.Agent})
		}
		if want.Status != "" && string(actual.Status) != want.Status {
			findings = append(findings, EvalFinding{Dimension: dim + ".status", Expected: want.Status, Actual: string(actual.Status)})
		}
		if want.FailureClass != "" {
			actualClass := ""
			if actual.FailureEvent != nil {
				actualClass = string(actual.FailureEvent.FailureClass)
			}
			if actualClass != want.FailureClass {
				findings = append(findings, EvalFinding{Dimension: dim + ".failure-class", Expected: want.FailureClass, Actual: actualClass})
			}
		}
		if want.RetryDisposition != "" {
			actualDisposition := ""
			if actual.FailureEvent != nil {
				actualDisposition = string(actual.FailureEvent.RetryDisposition)
			}
			if actualDisposition != want.RetryDisposition {
				findings = append(findings, EvalFinding{Dimension: dim + ".retry-disposition", Expected: want.RetryDisposition, Actual: actualDisposition})
			}
		}
		if want.SideEffect != "" && string(actual.SideEffect) != want.SideEffect {
			findings = append(findings, EvalFinding{Dimension: dim + ".side-effect", Expected: want.SideEffect, Actual: string(actual.SideEffect)})
		}
		if want.Attempts != nil && len(actual.ExecutionReceipts) != *want.Attempts {
			findings = append(findings, EvalFinding{
				Dimension: dim + ".attempts",
				Expected:  fmt.Sprintf("%d", *want.Attempts),
				Actual:    fmt.Sprintf("%d", len(actual.ExecutionReceipts)),
			})
		}
	}
	return findings
}

// assertEventOrder checks that order appears, in that relative order, as a
// subsequence of events -- other event types may be interleaved between
// them (§5 "Event": only critical type/order/cardinality/fields, never a
// full golden comparison).
func assertEventOrder(order []string, events []team.StatusEvent) []EvalFinding {
	if len(order) == 0 {
		return nil
	}
	next := 0
	for _, e := range events {
		if e.Type == order[next] {
			next++
			if next == len(order) {
				return nil
			}
		}
	}
	return []EvalFinding{{
		Dimension: "events.order",
		Expected:  strings.Join(order, " -> "),
		Actual:    fmt.Sprintf("matched only %d/%d in order; never observed %q after the preceding ones", next, len(order), order[next]),
	}}
}
