package team

import (
	"slices"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/execution"
)

func recoveryTestReceipt(attempt int, model string) ExecutionReceipt {
	exitCode := 1
	return ExecutionReceipt{
		RunID: "run-1", TaskID: "1", Attempt: attempt, ModelExecutionID: contextModelExecutionID("1", "worker", model),
		ExecutionTarget: execution.ExecutionTarget{Backend: "ollama", Model: model},
		StartedAt:       time.Unix(int64(attempt), 0).UTC(), FinishedAt: time.Unix(int64(attempt)+1, 0).UTC(), ExitCode: &exitCode,
		ContextManifest: &ContextInjectionManifest{Items: []ContextManifestItem{
			{ID: "current_task", Included: true, ContentHash: "goal"},
			{ID: "context:ctx-a", Included: true, ContentHash: "memory-a"},
		}},
	}
}

func TestCompareRecoverySignatures(t *testing.T) {
	item := &TodoItem{ID: "1", DependsOn: []string{"a", "b"}}
	base := recoveryTestReceipt(1, "minimax")
	cases := []struct {
		name        string
		item        func() *TodoItem
		receipt     func() ExecutionReceipt
		want        RecoveryComparison
		wantChanged []RecoveryDimension
		wantUnknown []RecoveryDimension
	}{
		{name: "same model and inputs", want: RecoveryNoStructuralChange},
		{
			name: "fallback to another model",
			receipt: func() ExecutionReceipt {
				r := recoveryTestReceipt(2, "glm")
				r.FallbackFrom = &execution.ExecutionTarget{Backend: "ollama", Model: "minimax"}
				return r
			},
			want: RecoveryChangeDetected, wantChanged: []RecoveryDimension{RecoveryDimensionExecutionTarget, RecoveryDimensionModelExecution},
		},
		{
			name: "retry feedback alone is not a change",
			receipt: func() ExecutionReceipt {
				r := recoveryTestReceipt(2, "minimax")
				r.ContextManifest.Items = append(r.ContextManifest.Items, ContextManifestItem{ID: "retry_failure_context", Included: true, ContentHash: "verify failed"}, ContextManifestItem{ID: "runtime_context", Included: true, ContentHash: "attempt 2"})
				return r
			},
			want: RecoveryNoStructuralChange,
		},
		{
			name: "different memory injected",
			receipt: func() ExecutionReceipt {
				r := recoveryTestReceipt(2, "minimax")
				r.ContextManifest.Items[1].ContentHash = "memory-a-revised"
				return r
			},
			want: RecoveryChangeDetected, wantChanged: []RecoveryDimension{RecoveryDimensionContextManifest},
		},
		{
			name:    "reordered dependencies are the same graph",
			item:    func() *TodoItem { return &TodoItem{ID: "1", DependsOn: []string{"b", "a"}} },
			receipt: func() ExecutionReceipt { return recoveryTestReceipt(2, "minimax") },
			want:    RecoveryNoStructuralChange,
		},
		{
			name: "truncated dynamic tool record",
			receipt: func() ExecutionReceipt {
				r := recoveryTestReceipt(2, "minimax")
				r.ToolInvocationsTruncated = 3
				return r
			},
			want: RecoveryComparisonUnknown, wantUnknown: []RecoveryDimension{RecoveryDimensionDynamicTools},
		},
		{
			name: "coordinator-run receipt has no target",
			receipt: func() ExecutionReceipt {
				r := recoveryTestReceipt(2, "minimax")
				r.ExecutionTarget = execution.ExecutionTarget{}
				return r
			},
			want: RecoveryComparisonUnknown, wantUnknown: []RecoveryDimension{RecoveryDimensionExecutionTarget},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			current, receipt := item, recoveryTestReceipt(2, "minimax")
			if tc.item != nil {
				current = tc.item()
			}
			if tc.receipt != nil {
				receipt = tc.receipt()
			}
			got, changed, unknown := compareRecoverySignatures(recoveryChangeSignature(item, base), recoveryChangeSignature(current, receipt))
			if got != tc.want || !slices.Equal(changed, tc.wantChanged) || !slices.Equal(unknown, tc.wantUnknown) {
				t.Fatalf("comparison = %s changed=%v unknown=%v, want %s changed=%v unknown=%v", got, changed, unknown, tc.want, tc.wantChanged, tc.wantUnknown)
			}
		})
	}
}

// TestSetExecutionReceiptKeepsOneReceiptPerAttempt pins the attempt unit the
// observer relies on (Step 0): a retry adds a receipt, a repair of the same
// attempt replaces it.
func TestSetExecutionReceiptKeepsOneReceiptPerAttempt(t *testing.T) {
	list := NewTaskTracker().TodoList()
	item := list.AddBatch([]TodoSpec{{Agent: "worker", Desc: "task"}})[0]
	first, second := recoveryTestReceipt(1, "minimax"), recoveryTestReceipt(2, "minimax")
	first.TaskID, second.TaskID = item.ID, item.ID
	repaired := first
	repaired.TranscriptRef = "repaired"
	for _, receipt := range []ExecutionReceipt{first, second, repaired} {
		if err := list.SetExecutionReceipt(item.ID, &receipt); err != nil {
			t.Fatal(err)
		}
	}
	stored := list.Items()[0].ExecutionReceipts
	if len(stored) != 2 || stored[0].Attempt != 1 || stored[0].TranscriptRef != "repaired" || stored[1].Attempt != 2 {
		t.Fatalf("receipts = %+v, want attempts 1 (repaired) and 2", stored)
	}
}

// TestRecoveryChangeObservedPerFinishedAttempt drives the coordinator path:
// one observation per finished attempt, none for an unfinished receipt or a
// repeated write, and a resumed coordinator still compares with the attempt
// before it.
func TestRecoveryChangeObservedPerFinishedAttempt(t *testing.T) {
	workspace := t.TempDir()
	store, err := OpenEventStore(workspace)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	newCoordinator := func(list *TaskTracker) *Coordinator {
		return &Coordinator{taskTracker: list, eventStore: store, emittedTaskTransitions: map[string]bool{}, session: &TeamSession{Workspace: workspace}, executionRunID: "run-1"}
	}
	tracker := NewTaskTracker()
	item := tracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "task"}})[0]
	c := newCoordinator(tracker)
	write := func(c *Coordinator, receipt ExecutionReceipt) {
		t.Helper()
		receipt.TaskID = item.ID
		if err := c.setAttemptReceipt(item.ID, &receipt); err != nil {
			t.Fatal(err)
		}
	}
	started := recoveryTestReceipt(1, "minimax")
	started.FinishedAt = time.Time{}
	write(c, started)
	write(c, recoveryTestReceipt(1, "minimax"))
	write(c, recoveryTestReceipt(1, "minimax"))
	write(c, recoveryTestReceipt(2, "minimax"))

	resumed := newCoordinator(tracker)
	write(resumed, recoveryTestReceipt(3, "glm"))

	events, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	observations := RecoveryChangeObservations(events)
	if len(observations) != 3 {
		t.Fatalf("observations = %+v, want one per finished attempt", observations)
	}
	first, second, third := observations[0], observations[1], observations[2]
	if first.Comparison != RecoveryComparisonUnknown || first.Reason != recoveryReasonNoPriorAttempt {
		t.Fatalf("first attempt = %+v, want unknown with no prior attempt", first)
	}
	if second.Comparison != RecoveryNoStructuralChange || second.PreviousAttempt != 1 {
		t.Fatalf("second attempt = %+v, want no structural change against attempt 1", second)
	}
	if third.Comparison != RecoveryChangeDetected || third.PreviousAttempt != 2 || !slices.Contains(third.ChangedDimensions, RecoveryDimensionExecutionTarget) {
		t.Fatalf("resumed attempt = %+v, want an execution target change against attempt 2", third)
	}
	if got := resumed.retriesWithoutStructuralChange(); got != 1 {
		t.Fatalf("retries without structural change = %d, want 1 (attempt 2), counted again after resume", got)
	}
	if first.OccurrenceID != "run-1/"+item.ID || !slices.Equal(first.NotTracked, recoveryNotTracked) {
		t.Fatalf("observation identity = %+v", first)
	}
	for _, event := range events {
		if event.Type == string(EventRecoveryChangeObserved) {
			if err := validateCurrentEventPayload(event); err != nil {
				t.Fatalf("observation event fails validation: %v", err)
			}
		}
	}
}
