package evalharness

import (
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"

	"github.com/kjelly/hufu/internal/execution"
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
func assertRun(expect ExpectSpec, result *team.RunResult, terminalLifecycleConfirmed bool, events []team.StatusEvent, tasks []*team.TodoItem) []EvalFinding {
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

	if expect.TerminalLifecycle != nil && terminalLifecycleConfirmed != *expect.TerminalLifecycle {
		findings = append(findings, EvalFinding{
			Dimension: "terminal-lifecycle-confirmed",
			Expected:  fmt.Sprintf("%t", *expect.TerminalLifecycle),
			Actual:    fmt.Sprintf("%t", terminalLifecycleConfirmed),
		})
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
	findings = append(findings, assertStatusEvents(expect.Events, events, tasks)...)

	if expect.ExecutionTargetFrozen != nil && *expect.ExecutionTargetFrozen {
		findings = append(findings, assertExecutionTargetFrozen(events)...)
	}
	if expect.NoUnauthorizedFallback != nil && *expect.NoUnauthorizedFallback {
		findings = append(findings, assertNoUnauthorizedFallback(tasks)...)
	}

	return findings
}

func assertNoUnauthorizedFallback(tasks []*team.TodoItem) []EvalFinding {
	var findings []EvalFinding
	for i, item := range tasks {
		dimension := fmt.Sprintf("tasks[%d].no-unauthorized-fallback", i)
		if item == nil || item.ExecutionTarget.IsZero() {
			findings = append(findings, EvalFinding{Dimension: dimension, Expected: "an immutable execution target", Actual: "missing"})
			continue
		}
		wantBackend := execution.CanonicalBackendName(item.ExecutionTarget.Backend)
		if item.BackendBinding == nil {
			findings = append(findings, EvalFinding{Dimension: dimension + ".backend-binding", Expected: wantBackend, Actual: "missing"})
		} else if got := execution.CanonicalBackendName(item.BackendBinding.Backend); got != wantBackend {
			findings = append(findings, EvalFinding{Dimension: dimension + ".backend-binding", Expected: wantBackend, Actual: got})
		}
		if len(item.ExecutionReceipts) == 0 {
			findings = append(findings, EvalFinding{Dimension: dimension + ".receipts", Expected: "at least one backend-bound receipt", Actual: "none"})
			continue
		}
		for receiptIndex, receipt := range item.ExecutionReceipts {
			got := execution.CanonicalBackendName(receipt.Backend)
			if got != wantBackend {
				findings = append(findings, EvalFinding{
					Dimension: fmt.Sprintf("%s.receipts[%d].backend", dimension, receiptIndex),
					Expected:  wantBackend,
					Actual:    got,
				})
			}
		}
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
	slices.Sort(distinct)
	return []EvalFinding{{
		Dimension: "execution-target-frozen",
		Expected:  "a single ExecutionTarget across the whole run",
		Actual:    fmt.Sprintf("%d distinct values: %s", len(distinct), strings.Join(distinct, ", ")),
	}}
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
		if want.Verification != "" {
			actualVerification := verificationState(actual.VerifyResult)
			if actualVerification != want.Verification {
				findings = append(findings, EvalFinding{Dimension: dim + ".verification", Expected: want.Verification, Actual: actualVerification})
			}
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
		if want.ExecutionTarget != "" && actual.ExecutionTarget.String() != want.ExecutionTarget {
			findings = append(findings, EvalFinding{
				Dimension: dim + ".execution-target",
				Expected:  want.ExecutionTarget,
				Actual:    actual.ExecutionTarget.String(),
			})
		}
		if want.BackendBinding != nil {
			findings = append(findings, assertBackendBinding(dim+".backend-binding", want.BackendBinding, actual.BackendBinding)...)
		}
	}
	return findings
}

func verificationState(result *team.VerificationResult) string {
	if result == nil {
		return "not-run"
	}
	if result.ExitCode == 0 && !result.TimedOut {
		return "passed"
	}
	return "failed"
}

func assertBackendBinding(dimension string, expect *BackendBindingExpect, actual *team.BackendBinding) []EvalFinding {
	if actual == nil {
		return []EvalFinding{{Dimension: dimension, Expected: "present", Actual: "missing"}}
	}
	var findings []EvalFinding
	compare := func(field, want, got string) {
		if want != "" && got != want {
			findings = append(findings, EvalFinding{Dimension: dimension + "." + field, Expected: want, Actual: got})
		}
	}
	compare("backend", expect.Backend, actual.Backend)
	compare("session-id", expect.SessionID, actual.SessionID)
	compare("turn-id", expect.TurnID, actual.TurnID)
	compare("backend-version", expect.BackendVersion, actual.BackendVersion)
	compare("effective-target", expect.EffectiveTarget, actual.EffectiveTarget)
	compare("execution-world-id", expect.ExecutionWorldID, actual.ExecutionWorldID)
	compare("cwd", expect.CWD, actual.CWD)
	compare("sandbox-mode", expect.SandboxMode, actual.SandboxMode)
	if expect.ResumeSupported != nil && actual.ResumeSupported != *expect.ResumeSupported {
		findings = append(findings, EvalFinding{Dimension: dimension + ".resume-supported", Expected: fmt.Sprintf("%t", *expect.ResumeSupported), Actual: fmt.Sprintf("%t", actual.ResumeSupported)})
	}
	return findings
}

func assertDurableEvents(expect EventsExpect, events []team.RunEvent, tasks []*team.TodoItem) []EvalFinding {
	types := make([]string, 0, len(events))
	for _, event := range events {
		types = append(types, event.Type)
	}
	findings := assertEventTypes("durable-events", expect, types)
	stable := make([]map[string]any, 0, len(events))
	for _, event := range events {
		fields := map[string]any{
			"schema_version": event.SchemaVersion,
			"run_id":         normalizeOpaqueID(event.RunID),
			"actor":          event.Actor,
			"type":           event.Type,
			"attempt":        event.Attempt,
			"task_index":     taskIndex(tasks, event.TaskID),
		}
		if len(event.Payload) > 0 {
			var payload any
			if err := json.Unmarshal(event.Payload, &payload); err == nil {
				fields["payload"] = payload
			}
		}
		stable = append(stable, fields)
	}
	return append(findings, assertEventMatches("durable-events", expect.Matches, types, stable)...)
}

func assertStatusEvents(expect EventsExpect, events []team.StatusEvent, tasks []*team.TodoItem) []EvalFinding {
	types := make([]string, 0, len(events))
	stable := make([]map[string]any, 0, len(events))
	for _, event := range events {
		types = append(types, event.Type)
		stable = append(stable, map[string]any{
			"type":             event.Type,
			"team":             event.TeamName,
			"agent":            event.Agent,
			"tool_name":        event.ToolName,
			"step":             event.Step,
			"task_index":       taskIndex(tasks, event.TodoID),
			"execution_target": event.ExecutionTarget,
			"backend":          event.Backend,
			"backend_kind":     event.BackendKind,
			"data":             event.Data,
		})
	}
	findings := assertEventTypes("events", expect, types)
	return append(findings, assertEventMatches("events", expect.Matches, types, stable)...)
}

func taskIndex(tasks []*team.TodoItem, taskID string) int {
	if taskID == "" {
		return -1
	}
	for index, task := range tasks {
		if task != nil && task.ID == taskID {
			return index
		}
	}
	return -1
}

func assertEventTypes(dimension string, expect EventsExpect, eventTypes []string) []EvalFinding {
	var findings []EvalFinding
	for _, want := range expect.Required {
		if !hasString(eventTypes, want) {
			findings = append(findings, EvalFinding{Dimension: dimension + "." + want, Expected: "observed", Actual: "not observed"})
		}
	}
	for _, want := range expect.Counts {
		actual := 0
		for _, eventType := range eventTypes {
			if eventType == want.Type {
				actual++
			}
		}
		if actual != want.Count {
			findings = append(findings, EvalFinding{Dimension: dimension + ".count." + want.Type, Expected: fmt.Sprintf("%d", want.Count), Actual: fmt.Sprintf("%d", actual)})
		}
	}
	if len(expect.Order) == 0 {
		return findings
	}
	next := 0
	for _, eventType := range eventTypes {
		if eventType == expect.Order[next] {
			next++
			if next == len(expect.Order) {
				return findings
			}
		}
	}
	return append(findings, EvalFinding{
		Dimension: dimension + ".order",
		Expected:  strings.Join(expect.Order, " -> "),
		Actual:    fmt.Sprintf("matched only %d/%d in order; never observed %q after the preceding ones", next, len(expect.Order), expect.Order[next]),
	})
}

func assertEventMatches(dimension string, matches []EventMatchExpect, eventTypes []string, events []map[string]any) []EvalFinding {
	var findings []EvalFinding
	for _, match := range matches {
		occurrence := match.Occurrence
		if occurrence == 0 {
			occurrence = 1
		}
		seen := 0
		selected := -1
		for index, eventType := range eventTypes {
			if eventType != match.Type {
				continue
			}
			seen++
			if seen == occurrence {
				selected = index
				break
			}
		}
		matchDimension := fmt.Sprintf("%s.match.%s[%d]", dimension, match.Type, occurrence)
		if selected < 0 {
			findings = append(findings, EvalFinding{Dimension: matchDimension, Expected: "event occurrence present", Actual: fmt.Sprintf("only %d occurrence(s)", seen)})
			continue
		}
		for _, path := range slices.Sorted(maps.Keys(match.Fields)) {
			want := match.Fields[path]
			actual, ok := fieldAtPath(events[selected], path)
			if !ok {
				findings = append(findings, EvalFinding{Dimension: matchDimension + "." + path, Expected: canonicalValue(want), Actual: "field missing"})
				continue
			}
			if canonicalValue(actual) != canonicalValue(want) {
				findings = append(findings, EvalFinding{Dimension: matchDimension + "." + path, Expected: canonicalValue(want), Actual: canonicalValue(actual)})
			}
		}
	}
	return findings
}

func fieldAtPath(fields map[string]any, path string) (any, bool) {
	var current any = fields
	for part := range strings.SplitSeq(path, ".") {
		mapped, ok := current.(map[string]any)
		if !ok {
			return nil, false
		}
		current, ok = mapped[part]
		if !ok {
			return nil, false
		}
	}
	return current, true
}

func canonicalValue(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprintf("%v", value)
	}
	return string(encoded)
}

func hasString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
