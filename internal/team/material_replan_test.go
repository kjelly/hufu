package team

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/execution"
)

var replanTestTarget = execution.ExecutionTarget{Backend: "ollama", Model: "minimax"}

func replanTestCoordinator(t *testing.T, mode string, store *EventStore, tracker *TaskTracker) *Coordinator {
	t.Helper()
	session := &TeamSession{Workspace: t.TempDir()}
	session.Config.Reliability.MaterialReplan = mode
	return &Coordinator{taskTracker: tracker, eventStore: store, emittedTaskTransitions: map[string]bool{}, session: session, executionRunID: "run-1"}
}

func replanTestStore(t *testing.T) *EventStore {
	t.Helper()
	store, err := OpenEventStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// addFailedReplanTask adds a task that failed with replan_required after an
// attempt that viewed a file and submitted a result.
func addFailedReplanTask(list *TodoList, spec TodoSpec) *TodoItem {
	if spec.ExecutionTarget.IsZero() {
		spec.ExecutionTarget = replanTestTarget
	}
	item := list.AddBatch([]TodoSpec{spec})[0]
	item.Status = TaskError
	item.FailureEvent = &FailureEventPayload{TaskID: item.ID, RetryDisposition: ReplanRequired}
	item.ExecutionReceipts = []ExecutionReceipt{{RunID: "run-1", TaskID: item.ID, Attempt: 2, StartedAt: time.Unix(4, 0), FinishedAt: time.Unix(5, 0),
		ToolSequence: &ToolSequenceRecord{Tools: []string{"view", "submit_result"}}}}
	return item
}

func TestReplanPredecessorsLinkOnlyByVerificationOrCriterion(t *testing.T) {
	cases := []struct {
		name      string
		failed    TodoSpec
		edit      func(*TodoItem)
		candidate TodoSpec
		wantLink  string
	}{
		{name: "same verification", failed: TodoSpec{Agent: "worker", Verify: "go test ./x"}, candidate: TodoSpec{Agent: "worker", Verify: "go test ./x"}, wantLink: strategyLinkVerification},
		{name: "other verification", failed: TodoSpec{Agent: "worker", Verify: "go test ./x"}, candidate: TodoSpec{Agent: "worker", Verify: "go test ./y"}},
		{name: "neither task verifies", failed: TodoSpec{Agent: "worker"}, candidate: TodoSpec{Agent: "worker"}},
		{name: "advances the failed criterion", failed: TodoSpec{Agent: "worker", Verify: "go test ./x", Advances: []string{"build"}}, candidate: TodoSpec{Agent: "worker", Advances: []string{"build"}}, wantLink: strategyLinkCriterion},
		{name: "failure that only needs a retry", failed: TodoSpec{Agent: "worker", Verify: "go test ./x"}, edit: func(item *TodoItem) { item.FailureEvent.RetryDisposition = RetryWorker }, candidate: TodoSpec{Agent: "worker", Verify: "go test ./x"}},
		{name: "failure already superseded", failed: TodoSpec{Agent: "worker", Verify: "go test ./x"}, edit: func(item *TodoItem) { item.Resolution = &TaskResolution{Status: "superseded", ResolvedBy: "9"} }, candidate: TodoSpec{Agent: "worker", Verify: "go test ./x"}},
		{name: "task that later succeeded", failed: TodoSpec{Agent: "worker", Verify: "go test ./x"}, edit: func(item *TodoItem) { item.Status = TaskDone }, candidate: TodoSpec{Agent: "worker", Verify: "go test ./x"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tracker := NewTaskTracker()
			c := replanTestCoordinator(t, agent.MaterialReplanWarn, nil, tracker)
			failed := addFailedReplanTask(tracker.TodoList(), tc.failed)
			if tc.edit != nil {
				tc.edit(failed)
			}
			links := c.replanPredecessors(todoItemFromSpec(tc.candidate, "99"), tracker.TodoList().Items())
			switch {
			case tc.wantLink == "" && len(links) != 0:
				t.Fatalf("links = %+v, want none", links)
			case tc.wantLink != "" && (len(links) != 1 || links[0].link != tc.wantLink || links[0].previous.ID != failed.ID):
				t.Fatalf("links = %+v, want one %s link to %s", links, tc.wantLink, failed.ID)
			}
		})
	}
}

// TestRepeatedFailureRejectsSameExecutionStrategy covers the dispatch check
// in each mode: a replacement that rewords the goal of a replan_required
// task but keeps its agent, target, dependencies, and verification.
func TestRepeatedFailureRejectsSameExecutionStrategy(t *testing.T) {
	repeat := TodoSpec{Agent: "worker", Goal: "Fix it another way", Verify: "go test ./x", ExecutionTarget: replanTestTarget}
	other := TodoSpec{Agent: "debugger", Goal: "Fix it", Verify: "go test ./x", ExecutionTarget: replanTestTarget}
	cases := []struct {
		name          string
		mode          string
		candidate     TodoSpec
		wantRefused   bool
		wantEvaluated []bool // materially_different per recorded comparison
		wantCounts    replanStrategyCounts
	}{
		{name: "off skips the check", mode: agent.MaterialReplanOff, candidate: repeat},
		{name: "warn records and dispatches", mode: agent.MaterialReplanWarn, candidate: repeat, wantEvaluated: []bool{false}, wantCounts: replanStrategyCounts{unchanged: 1}},
		{name: "enforce refuses a repeat", mode: agent.MaterialReplanEnforce, candidate: repeat, wantRefused: true, wantCounts: replanStrategyCounts{rejected: 1}},
		{name: "enforce admits another agent", mode: agent.MaterialReplanEnforce, candidate: other, wantEvaluated: []bool{true}, wantCounts: replanStrategyCounts{changed: 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := replanTestStore(t)
			tracker := NewTaskTracker()
			c := replanTestCoordinator(t, tc.mode, store, tracker)
			addFailedReplanTask(tracker.TodoList(), TodoSpec{Agent: "worker", Goal: "Fix it", Verify: "go test ./x"})
			evaluations, err := c.evaluateReplanBatch([]TodoSpec{tc.candidate}, []string{"5"})
			var violation *delegationPolicyViolation
			if refused := errors.As(err, &violation); refused != tc.wantRefused || (err != nil && !refused) {
				t.Fatalf("err = %v, want refused=%v", err, tc.wantRefused)
			}
			if tc.wantRefused && !strings.Contains(err.Error(), reasonReplanNotMateriallyDifferent) {
				t.Fatalf("refusal %q does not name the reason", err)
			}
			c.recordPlannedStrategyChanges(evaluations)
			var got []bool
			for _, payload := range evaluations {
				got = append(got, payload.MateriallyDifferent)
				if payload.TaskID != "5" || payload.Phase != strategyPhasePlanned || !slices.Contains(payload.UnknownDimensions, StrategyDimensionToolSequence) {
					t.Fatalf("planned comparison = %+v", payload)
				}
			}
			if !slices.Equal(got, tc.wantEvaluated) {
				t.Fatalf("materially different = %v, want %v", got, tc.wantEvaluated)
			}
			if counts := c.replanStrategyCounts(); counts != tc.wantCounts {
				t.Fatalf("counts = %+v, want %+v", counts, tc.wantCounts)
			}
			events, err := store.ReadEvents()
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range events {
				if event.Type == string(EventStrategyChangeEvaluated) || event.Type == string(EventStrategyChangeRejected) {
					if err := validateCurrentEventPayload(event); err != nil {
						t.Fatalf("%s fails validation: %v", event.Type, err)
					}
				}
			}
		})
	}
}

// TestExecutedStrategyChangeComparesToolSequencesOnce records the post-run
// comparison for a replacement's first finished attempt only, also after a
// resumed coordinator rebuilds its state from the event log.
func TestExecutedStrategyChangeComparesToolSequencesOnce(t *testing.T) {
	store := replanTestStore(t)
	tracker := NewTaskTracker()
	c := replanTestCoordinator(t, agent.MaterialReplanWarn, store, tracker)
	list := tracker.TodoList()
	failed := addFailedReplanTask(list, TodoSpec{Agent: "worker", Verify: "go test ./x"})
	spec := TodoSpec{Agent: "worker", Verify: "go test ./x", ExecutionTarget: replanTestTarget}
	evaluations, err := c.evaluateReplanBatch([]TodoSpec{spec}, []string{"2"})
	if err != nil {
		t.Fatal(err)
	}
	replacement := list.AddBatch([]TodoSpec{spec})[0]
	if replacement.ID != "2" {
		t.Fatalf("replacement id = %s", replacement.ID)
	}
	c.recordPlannedStrategyChanges(evaluations)
	attempt := func(c *Coordinator, n int, tools ...string) {
		t.Helper()
		receipt := ExecutionReceipt{RunID: "run-1", TaskID: replacement.ID, Attempt: n, StartedAt: time.Unix(int64(10*n), 0), FinishedAt: time.Unix(int64(10*n+1), 0), ToolSequence: &ToolSequenceRecord{Tools: tools}}
		if err := c.setAttemptReceipt(replacement.ID, &receipt); err != nil {
			t.Fatal(err)
		}
	}
	attempt(c, 1, "grep", "view", "submit_result")
	attempt(c, 2, "view", "submit_result")
	attempt(replanTestCoordinator(t, agent.MaterialReplanWarn, store, tracker), 3, "view", "submit_result")

	events, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	var executed []StrategyChangePayload
	for _, payload := range StrategyChangeObservations(events) {
		if payload.Phase == strategyPhaseExecuted {
			executed = append(executed, payload)
		}
	}
	if len(executed) != 1 {
		t.Fatalf("executed comparisons = %+v, want one", executed)
	}
	got := executed[0]
	if got.Attempt != 1 || got.PreviousTaskID != failed.ID || got.PreviousAttempt != 2 || !got.MateriallyDifferent || !slices.Equal(got.ChangedDimensions, []StrategyDimension{StrategyDimensionToolSequence}) {
		t.Fatalf("executed comparison = %+v, want a tool sequence change on attempt 1 against attempt 2 of %s", got, failed.ID)
	}
}

func TestMaterialReplanModeConfiguration(t *testing.T) {
	cases := []struct {
		name        string
		reliability string
		want        string
		wantErr     bool
	}{
		{name: "default warns", want: agent.MaterialReplanWarn},
		{name: "enforce", reliability: "  material-replan: enforce\n", want: agent.MaterialReplanEnforce},
		{name: "off", reliability: "  material-replan: off\n", want: agent.MaterialReplanOff},
		{name: "warn-only turns enforce into warn", reliability: "  material-replan: enforce\n  warn-only: true\n", want: agent.MaterialReplanWarn},
		{name: "unknown mode", reliability: "  material-replan: strict\n", wantErr: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "team.yaml"), []byte("name: replan-team\nreliability:\n  max-same-failure-fingerprint: 2\n"+tc.reliability), 0o644); err != nil {
				t.Fatal(err)
			}
			cfg, err := parseTeamYML(dir, nil)
			if tc.wantErr {
				if err == nil || !strings.Contains(err.Error(), "material-replan") {
					t.Fatalf("err = %v, want a material-replan error", err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			c := &Coordinator{session: &TeamSession{Config: cfg}}
			if got := c.materialReplanMode(); got != tc.want {
				t.Fatalf("mode = %q, want %q", got, tc.want)
			}
			if tc.reliability == "" && cfg.Reliability.MaterialReplan != "" {
				t.Fatalf("an unset mode was stored as %q; teams that do not set it must keep an unchanged configuration", cfg.Reliability.MaterialReplan)
			}
		})
	}
}
