package team

import (
	"strconv"
	"testing"
)

// receiptVerifyResult returns the verify result attached to the receipt for
// (runID, attempt), or nil when no such receipt exists.
func receiptVerifyResult(t *testing.T, tl *TodoList, todoID, runID string, attempt int) *VerificationResult {
	t.Helper()
	for _, item := range tl.Items() {
		if item.ID != todoID {
			continue
		}
		for _, r := range item.ExecutionReceipts {
			if r.RunID == runID && r.Attempt == attempt {
				return r.VerifyResult
			}
		}
	}
	return nil
}

func seedReceipts(t *testing.T, tl *TodoList, todoID string, receipts ...ExecutionReceipt) {
	t.Helper()
	for i := range receipts {
		exit := 0
		receipts[i].TaskID = todoID
		receipts[i].ExitCode = &exit
		tl.SetExecutionReceipt(todoID, &receipts[i])
	}
}

func TestTodoListUpdateReceiptVerifyResult(t *testing.T) {
	vr := &VerificationResult{Command: "test -f x", ExitCode: 1, Stderr: "missing"}
	tests := []struct {
		name     string
		receipts []ExecutionReceipt
		update   func(tl *TodoList, todoID string)
		// want maps "runID/attempt" to whether a verify result must be attached.
		want map[string]bool
	}{
		{
			name:     "attaches to the matching attempt only",
			receipts: []ExecutionReceipt{{RunID: "run-a", Attempt: 1}, {RunID: "run-a", Attempt: 2}},
			update: func(tl *TodoList, todoID string) {
				tl.UpdateReceiptVerifyResult("run-a", todoID, 1, vr)
			},
			want: map[string]bool{"run-a/1": true, "run-a/2": false},
		},
		{
			name:     "nil result, unknown attempt and unknown run are no-ops",
			receipts: []ExecutionReceipt{{RunID: "run-a", Attempt: 1}},
			update: func(tl *TodoList, todoID string) {
				tl.UpdateReceiptVerifyResult("run-a", todoID, 1, vr)
				tl.UpdateReceiptVerifyResult("run-a", todoID, 99, vr)
				tl.UpdateReceiptVerifyResult("run-a", todoID, 1, nil)
				tl.UpdateReceiptVerifyResult("nonexistent-run", todoID, 1, vr)
			},
			want: map[string]bool{"run-a/1": true},
		},
		{
			// After crash-resume a todo carries a prior run's attempt-1 receipt
			// next to the new run's; the match must include the run ID.
			name:     "matches run ID not just attempt",
			receipts: []ExecutionReceipt{{RunID: "run-old", Attempt: 1}, {RunID: "run-new", Attempt: 1}},
			update: func(tl *TodoList, todoID string) {
				tl.UpdateReceiptVerifyResult("run-new", todoID, 1, vr)
			},
			want: map[string]bool{"run-old/1": false, "run-new/1": true},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tl := NewTaskTracker().TodoList()
			todoID := tl.AddBatch([]TodoSpec{{Agent: "worker", Desc: "do work"}})[0].ID
			seedReceipts(t, tl, todoID, tt.receipts...)
			tt.update(tl, todoID)
			for _, r := range tt.receipts {
				key := r.RunID + "/" + strconv.Itoa(r.Attempt)
				got := receiptVerifyResult(t, tl, todoID, r.RunID, r.Attempt)
				if (got != nil) != tt.want[key] {
					t.Errorf("receipt %s verify result = %v, want attached=%v", key, got, tt.want[key])
				}
				if got != nil && (got.Command != vr.Command || got.ExitCode != vr.ExitCode) {
					t.Errorf("receipt %s verify result = %#v, want %#v", key, got, vr)
				}
			}
		})
	}
}

func TestTodoListItemsDeepCopyReceiptVerifyResult(t *testing.T) {
	tl := NewTaskTracker().TodoList()
	todoID := tl.AddBatch([]TodoSpec{{Agent: "worker", Desc: "do work"}})[0].ID
	seedReceipts(t, tl, todoID, ExecutionReceipt{RunID: "r", Attempt: 1})
	tl.UpdateReceiptVerifyResult("r", todoID, 1, &VerificationResult{Command: "test -f x", ExitCode: 1, Stderr: "original"})

	// Mutating a snapshot outside the todo lock must not reach the canonical
	// item through a shared pointer.
	snapshot := receiptVerifyResult(t, tl, todoID, "r", 1)
	if snapshot == nil {
		t.Fatal("snapshot does not carry the verify result")
	}
	snapshot.Stderr = "mutated-by-snapshot"
	if got := receiptVerifyResult(t, tl, todoID, "r", 1); got == nil || got.Stderr != "original" {
		t.Errorf("canonical receipt verify result = %#v, want Stderr %q", got, "original")
	}
}
