package team

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newCompleteWorksetCoordinator(t *testing.T) (*Coordinator, *WorksetExpansionReceipt) {
	t.Helper()
	workspace := t.TempDir()
	store, err := NewFileArtifactStore(workspace, workspace)
	if err != nil {
		t.Fatal(err)
	}
	source, err := store.Put(context.Background(), PutArtifactRequest{Content: []byte(`{"schema_version":1,"items":[{"key":"a"},{"key":"b"} ]}`), Path: "manifest.json", Kind: "workset_manifest", RunID: "run-1", TaskID: "prepare"})
	if err != nil {
		t.Fatal(err)
	}
	receipt := &WorksetExpansionReceipt{
		WorksetID: "workset-1", RunID: "run-1", ParentTaskID: "prepare", SourceArtifactID: source.ID,
		SourceSHA256: source.SHA256, ItemCount: 2, Children: map[string]string{"a": "child-a", "b": "child-b"},
	}
	tracker := NewTaskTracker()
	items := tracker.TodoList().AddBatch([]TodoSpec{
		{PlanTaskID: "child-a", Agent: "worker", Desc: "a", WorksetBinding: &WorksetBinding{WorksetID: "workset-1", ParentTaskID: "prepare", ItemKey: "a", SourceArtifactID: source.ID, SourceSHA256: source.SHA256}},
		{PlanTaskID: "child-b", Agent: "worker", Desc: "b", WorksetBinding: &WorksetBinding{WorksetID: "workset-1", ParentTaskID: "prepare", ItemKey: "b", SourceArtifactID: source.ID, SourceSHA256: source.SHA256}},
	})
	items[0].Status = TaskDone
	items[1].Status = TaskDone
	receipt.Children["a"] = items[0].ID
	receipt.Children["b"] = items[1].ID
	items[0].TypedResult = &TaskResult{TaskID: "child-a", Status: TaskResultStatusSuccess, Summary: "a", Source: "submitted"}
	items[1].TypedResult = &TaskResult{TaskID: "child-b", Status: TaskResultStatusCompletedWithGaps, Summary: "b", Source: "submitted"}
	items[0].VerifyResult = &VerificationResult{ExitCode: 0}
	items[1].VerifyResult = &VerificationResult{ExitCode: 0}
	items[0].WorksetReceipt = receipt
	c := &Coordinator{session: &TeamSession{Workspace: workspace}, taskTracker: tracker, executionRunID: "run-1"}
	return c, receipt
}

func TestWorksetCompleteVerificationPassesAllVerifiedChildren(t *testing.T) {
	c, _ := newCompleteWorksetCoordinator(t)
	result, err := c.executeWorksetCompleteVerification(context.Background(), VerificationSpec{
		Type: VerifyWorksetComplete, WorksetSourceTask: "prepare", WorksetRequireTerminal: true,
		WorksetRequireVerified: true, WorksetAcceptedStatuses: []string{TaskResultStatusSuccess, TaskResultStatusCompletedWithGaps},
	})
	if err != nil || result == nil || result.ExitCode != 0 {
		t.Fatalf("workset_complete result=%#v err=%v", result, err)
	}
	states := c.WorksetGroupStates()
	if len(states) != 1 || states[0].State != "complete" || states[0].Expected != 2 || states[0].Completed != 2 || states[0].Verified != 2 {
		t.Fatalf("workset state = %#v", states)
	}
}

func TestWorksetCompleteVerificationRejectsFailureExtraChildAndStaleSource(t *testing.T) {
	t.Run("failed child", func(t *testing.T) {
		c, _ := newCompleteWorksetCoordinator(t)
		items := c.taskTracker.TodoList().Items()
		if err := c.taskTracker.TodoList().SetTypedResult(items[1].ID, &TaskResult{TaskID: items[1].ID, Status: TaskResultStatusPartial, Summary: "partial", Source: "submitted"}); err != nil {
			t.Fatal(err)
		}
		if result, err := c.executeWorksetCompleteVerification(context.Background(), NormalizeVerificationSpec(VerificationSpec{Type: VerifyWorksetComplete, WorksetSourceTask: "prepare", WorksetRequireTerminal: true, WorksetRequireVerified: true}, "", "")); err == nil || result.ExitCode == 0 {
			t.Fatalf("failed child unexpectedly passed: %#v err=%v", result, err)
		}
	})
	t.Run("extra child", func(t *testing.T) {
		c, _ := newCompleteWorksetCoordinator(t)
		item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{PlanTaskID: "child-extra", Agent: "worker", WorksetBinding: &WorksetBinding{WorksetID: "workset-1", ItemKey: "extra"}}})[0]
		item.Status = TaskDone
		item.TypedResult = &TaskResult{Status: TaskResultStatusSuccess, Source: "submitted"}
		if result, err := c.executeWorksetCompleteVerification(context.Background(), NormalizeVerificationSpec(VerificationSpec{Type: VerifyWorksetComplete, WorksetSourceTask: "prepare"}, "", "")); err == nil || result.ExitCode == 0 {
			t.Fatalf("extra child unexpectedly passed: %#v err=%v", result, err)
		}
	})
	t.Run("stale source", func(t *testing.T) {
		c, receipt := newCompleteWorksetCoordinator(t)
		path := filepath.Join(c.session.Workspace, logsDir, "artifacts", "data", receipt.SourceArtifactID)
		if err := os.WriteFile(path, []byte("changed"), 0o644); err != nil {
			t.Fatal(err)
		}
		if result, err := c.executeWorksetCompleteVerification(context.Background(), NormalizeVerificationSpec(VerificationSpec{Type: VerifyWorksetComplete, WorksetSourceTask: "prepare"}, "", "")); err == nil || result.ExitCode == 0 {
			t.Fatalf("stale source unexpectedly passed: %#v err=%v", result, err)
		}
	})
}

func TestWorksetCompleteVerificationRejectsZeroItemReceipt(t *testing.T) {
	c, receipt := newCompleteWorksetCoordinator(t)
	receipt.ItemCount = 0
	receipt.Children = map[string]string{}
	c.taskTracker.TodoList().Items()[0].WorksetReceipt = receipt
	result, err := c.executeWorksetCompleteVerification(context.Background(), VerificationSpec{Type: VerifyWorksetComplete, WorksetSourceTask: "prepare"})
	if err == nil || result == nil || !strings.Contains(result.Stderr, "cardinality") {
		t.Fatalf("zero-item workset result=%#v err=%v", result, err)
	}
}

func TestWorksetCompleteVerificationRunsThroughAcceptance(t *testing.T) {
	c, _ := newCompleteWorksetCoordinator(t)
	c.acceptanceSpec = &AcceptanceSpec{Verifications: []VerificationSpec{{
		Type: VerifyWorksetComplete, WorksetSourceTask: "prepare", WorksetRequireTerminal: true, WorksetRequireVerified: true,
	}}}
	result, err := c.runAcceptance(context.Background())
	if err != nil || result == nil || !result.IsPassed() || len(result.VerificationEvidence) != 1 || result.VerificationEvidence[0].ExitCode != 0 {
		t.Fatalf("acceptance result=%#v err=%v", result, err)
	}
}
