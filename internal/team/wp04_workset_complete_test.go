package team

import (
	"context"
	"encoding/json"
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
	manifest := []byte(`{"schema_version":1,"items":[{"key":"a"},{"key":"b"} ]}`)
	oldSource, err := store.Put(context.Background(), PutArtifactRequest{Content: manifest, Path: "manifest.json", Kind: "workset_manifest", RunID: "old-run", TaskID: "prepare", Attempt: 1, Agent: "producer"})
	if err != nil {
		t.Fatal(err)
	}
	source, err := store.Put(context.Background(), PutArtifactRequest{Content: manifest, Path: "manifest.json", Kind: "workset_manifest", RunID: "run-1", TaskID: "prepare", Attempt: 2, Agent: "producer"})
	if err != nil {
		t.Fatal(err)
	}
	if oldSource.ID != source.ID {
		t.Fatalf("identical source IDs differ: old=%q current=%q", oldSource.ID, source.ID)
	}
	receipt := &WorksetExpansionReceipt{
		WorksetID: "workset-1", RunID: "run-1", ParentTaskID: "prepare", SourceArtifactID: source.ID,
		SourceSHA256: source.SHA256, SourceArtifact: source.ArtifactRef, ItemCount: 2, Children: map[string]string{"a": "child-a", "b": "child-b"},
	}
	tracker := NewTaskTracker()
	items := tracker.TodoList().AddBatch([]TodoSpec{
		{PlanTaskID: "child-a", Agent: "worker", Desc: "a", WorksetBinding: &WorksetBinding{WorksetID: "workset-1", ParentTaskID: "prepare", ItemKey: "a", SourceArtifactID: source.ID, SourceSHA256: source.SHA256, SourceArtifact: source.ArtifactRef}},
		{PlanTaskID: "child-b", Agent: "worker", Desc: "b", WorksetBinding: &WorksetBinding{WorksetID: "workset-1", ParentTaskID: "prepare", ItemKey: "b", SourceArtifactID: source.ID, SourceSHA256: source.SHA256, SourceArtifact: source.ArtifactRef}},
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
	c := &Coordinator{
		session:        &TeamSession{Workspace: workspace},
		taskTracker:    tracker,
		executionRunID: "run-1",
		taskResults: map[string]*TaskResult{
			"prepare": {TaskID: "prepare", Attempt: 2, Agent: "producer", Status: TaskResultStatusSuccess, Artifacts: []ArtifactRef{source.ArtifactRef}},
		},
	}
	return c, receipt
}

func TestWorksetCompleteVerificationPassesAllVerifiedChildren(t *testing.T) {
	c, _ := newCompleteWorksetCoordinator(t)
	result, err := c.executeWorksetCompleteVerification(context.Background(), VerificationSpec{
		Type: VerifyWorksetComplete, WorksetSourceTask: " PREPARE ", WorksetRequireTerminal: true,
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

func TestFindWorksetReceiptMatchesNormalizedParentAndFailsClosed(t *testing.T) {
	t.Run("unknown", func(t *testing.T) {
		c, _ := newCompleteWorksetCoordinator(t)
		if _, err := c.findWorksetReceipt("missing"); err == nil {
			t.Fatal("unknown source-task unexpectedly found a receipt")
		}
	})

	t.Run("multiple distinct receipts", func(t *testing.T) {
		c, receipt := newCompleteWorksetCoordinator(t)
		second := cloneWorksetReceipt(receipt)
		second.WorksetID = "workset-2"
		items := c.taskTracker.TodoList().Items()
		items[1].WorksetReceipt = second
		c.taskTracker.TodoList().Restore(items)
		if _, err := c.findWorksetReceipt("pRePaRe"); err == nil || !strings.Contains(err.Error(), "ambiguous") {
			t.Fatalf("multiple receipts error = %v", err)
		}
	})

	t.Run("equivalent replayed receipts", func(t *testing.T) {
		c, receipt := newCompleteWorksetCoordinator(t)
		items := c.taskTracker.TodoList().Items()
		items[1].WorksetReceipt = cloneWorksetReceipt(receipt)
		c.taskTracker.TodoList().Restore(items)
		if _, err := c.findWorksetReceipt("prepare"); err != nil {
			t.Fatalf("equivalent receipts unexpectedly conflicted: %v", err)
		}
		projected, err := c.worksetReceiptsFromTasks(items)
		if err != nil || len(projected) != 1 || projected[0].WorksetID != receipt.WorksetID {
			t.Fatalf("equivalent checkpoint receipts = %#v, err=%v; want one deduplicated receipt", projected, err)
		}
	})

	for _, parentTaskID := range []string{"other", "OTHER"} {
		t.Run("conflicting parent before source filtering/"+parentTaskID, func(t *testing.T) {
			c, receipt := newCompleteWorksetCoordinator(t)
			second := cloneWorksetReceipt(receipt)
			second.ParentTaskID = parentTaskID
			items := c.taskTracker.TodoList().Items()
			items[1].WorksetReceipt = second
			c.taskTracker.TodoList().Restore(items)
			if _, err := c.findWorksetReceipt("prepare"); err == nil || !strings.Contains(err.Error(), "conflicting") {
				t.Fatalf("conflicting parent receipt error = %v", err)
			}
		})
	}

	for name, mutate := range map[string]func(*WorksetExpansionReceipt){
		"run ID":             func(receipt *WorksetExpansionReceipt) { receipt.RunID = "run-2" },
		"source artifact ID": func(receipt *WorksetExpansionReceipt) { receipt.SourceArtifactID = "artifact-2" },
		"source SHA256":      func(receipt *WorksetExpansionReceipt) { receipt.SourceSHA256 = strings.Repeat("b", 64) },
		"item count":         func(receipt *WorksetExpansionReceipt) { receipt.ItemCount++ },
		"item keys SHA256":   func(receipt *WorksetExpansionReceipt) { receipt.ItemKeysSHA256 = "keys-2" },
		"child mapping":      func(receipt *WorksetExpansionReceipt) { receipt.Children["a"] = "child-conflict" },
	} {
		t.Run("conflicting "+name, func(t *testing.T) {
			c, receipt := newCompleteWorksetCoordinator(t)
			second := cloneWorksetReceipt(receipt)
			mutate(second)
			items := c.taskTracker.TodoList().Items()
			items[1].WorksetReceipt = second
			c.taskTracker.TodoList().Restore(items)
			if _, err := c.findWorksetReceipt("prepare"); err == nil || !strings.Contains(err.Error(), "conflicting") {
				t.Fatalf("conflicting receipt error = %v", err)
			}
		})
	}

	t.Run("replayed receipt", func(t *testing.T) {
		c, _ := newCompleteWorksetCoordinator(t)
		item := c.taskTracker.TodoList().Items()[0]
		payload, err := json.Marshal(item)
		if err != nil {
			t.Fatal(err)
		}
		replayed := ReduceToTodoList([]RunEvent{{Type: string(EventTaskCreated), TaskID: item.ID, Payload: payload}})
		tracker := NewTaskTracker()
		tracker.TodoList().Restore(replayed)
		replayedCoordinator := &Coordinator{taskTracker: tracker}
		got, err := replayedCoordinator.findWorksetReceipt("PREPARE")
		if err != nil {
			t.Fatalf("replayed receipt lookup failed: %v", err)
		}
		if got.WorksetID != item.WorksetReceipt.WorksetID || got.ParentTaskID != item.WorksetReceipt.ParentTaskID {
			t.Fatalf("replayed receipt = %#v, want %#v", got, item.WorksetReceipt)
		}
	})
}

func TestWorksetReceiptConflictsFailClosedInProjectionPaths(t *testing.T) {
	c, receipt := newCompleteWorksetCoordinator(t)
	second := cloneWorksetReceipt(receipt)
	second.ParentTaskID = "OTHER"
	items := c.taskTracker.TodoList().Items()
	items[1].WorksetReceipt = second
	c.taskTracker.TodoList().Restore(items)

	states := c.WorksetGroupStates()
	if len(states) != 1 || states[0].State != "failed" || states[0].Failed == 0 {
		t.Fatalf("conflicting workset state = %#v, want failed", states)
	}
	projectedReceipts, err := c.worksetReceiptsFromTasks(items)
	if err == nil || !strings.Contains(err.Error(), "conflicting") {
		t.Fatalf("checkpoint receipt projection error = %v, want conflict", err)
	}
	if projectedReceipts != nil {
		t.Fatalf("conflicting checkpoint projection returned receipts: %#v", projectedReceipts)
	}

	payloads := make([][]byte, 0, len(items))
	for _, item := range items {
		payload, marshalErr := json.Marshal(item)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		payloads = append(payloads, payload)
	}
	events := []RunEvent{
		{Type: string(EventTaskCreated), TaskID: items[0].ID, Payload: payloads[0]},
		{Type: string(EventTaskCreated), TaskID: items[1].ID, Payload: payloads[1]},
	}
	reduced := ReduceToTodoList(events)
	if len(reduced) != 2 || reduced[0].Status != TaskError || reduced[1].Status != TaskError {
		t.Fatalf("reduced conflicting tasks = %#v, want both error", reduced)
	}
	projected := ReduceToSessionData(events)
	if !projected.RecoveryRequired || !strings.Contains(projected.RecoveryReason, "conflicting workset receipts") {
		t.Fatalf("reduced session recovery = required:%v reason:%q", projected.RecoveryRequired, projected.RecoveryReason)
	}
	if len(projected.WorksetReceipts) != 0 {
		t.Fatalf("conflicting session projection retained receipts: %#v", projected.WorksetReceipts)
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

func TestWorksetCompleteVerificationBindsProducerOccurrence(t *testing.T) {
	t.Run("old occurrence remains valid for old run", func(t *testing.T) {
		c, receipt := newCompleteWorksetCoordinator(t)
		oldRef := c.taskResults["prepare"].Artifacts[0]
		oldRef.RunID = "old-run"
		oldRef.Attempt = 1
		c.taskResults["prepare"].Artifacts[0] = oldRef
		c.taskResults["prepare"].Attempt = 1
		c.executionRunID = "old-run"
		receipt.RunID = "old-run"
		receipt.SourceArtifact = oldRef
		result, err := c.executeWorksetCompleteVerification(context.Background(), VerificationSpec{
			Type: VerifyWorksetComplete, WorksetSourceTask: "prepare",
			WorksetAcceptedStatuses: []string{TaskResultStatusSuccess, TaskResultStatusCompletedWithGaps},
		})
		if err != nil || result == nil || result.ExitCode != 0 {
			t.Fatalf("old occurrence result=%#v err=%v", result, err)
		}
	})

	t.Run("current receipt rejects old producer occurrence", func(t *testing.T) {
		c, _ := newCompleteWorksetCoordinator(t)
		oldRef := c.taskResults["prepare"].Artifacts[0]
		oldRef.RunID = "old-run"
		oldRef.Attempt = 1
		c.taskResults["prepare"].Artifacts[0] = oldRef
		result, err := c.executeWorksetCompleteVerification(context.Background(), VerificationSpec{Type: VerifyWorksetComplete, WorksetSourceTask: "prepare"})
		if err == nil || result == nil || result.ExitCode == 0 || !strings.Contains(result.Stderr, "invalid current producer occurrence") {
			t.Fatalf("old producer occurrence unexpectedly passed: %#v err=%v", result, err)
		}
	})

	t.Run("old receipt rejects current run", func(t *testing.T) {
		c, receipt := newCompleteWorksetCoordinator(t)
		receipt.RunID = "old-run"
		result, err := c.executeWorksetCompleteVerification(context.Background(), VerificationSpec{Type: VerifyWorksetComplete, WorksetSourceTask: "prepare"})
		if err == nil || result == nil || result.ExitCode == 0 || !strings.Contains(result.Stderr, "workset receipt belongs to run") {
			t.Fatalf("old receipt unexpectedly passed in current run: %#v err=%v", result, err)
		}
	})
}

func TestWorksetCompleteVerificationRejectsUnrelatedSameContentProducer(t *testing.T) {
	t.Run("intended producer missing", func(t *testing.T) {
		c, receipt := newCompleteWorksetCoordinator(t)
		delete(c.taskResults, "prepare")
		unrelated := receipt.SourceArtifact
		unrelated.TaskID = "unrelated"
		c.taskResults["unrelated"] = &TaskResult{
			TaskID: "unrelated", Attempt: unrelated.Attempt, Agent: unrelated.Agent,
			Status: TaskResultStatusSuccess, Artifacts: []ArtifactRef{unrelated},
		}
		result, err := c.executeWorksetCompleteVerification(context.Background(), VerificationSpec{Type: VerifyWorksetComplete, WorksetSourceTask: "prepare"})
		if err == nil || result == nil || result.ExitCode == 0 || !strings.Contains(result.Stderr, "intended canonical producer task") {
			t.Fatalf("missing intended producer unexpectedly passed: %#v err=%v", result, err)
		}
	})

	t.Run("intended producer stale", func(t *testing.T) {
		c, receipt := newCompleteWorksetCoordinator(t)
		stale := receipt.SourceArtifact
		stale.RunID = "old-run"
		c.taskResults["prepare"].Artifacts[0] = stale
		unrelated := receipt.SourceArtifact
		unrelated.TaskID = "unrelated"
		c.taskResults["unrelated"] = &TaskResult{
			TaskID: "unrelated", Attempt: unrelated.Attempt, Agent: unrelated.Agent,
			Status: TaskResultStatusSuccess, Artifacts: []ArtifactRef{unrelated},
		}
		result, err := c.executeWorksetCompleteVerification(context.Background(), VerificationSpec{Type: VerifyWorksetComplete, WorksetSourceTask: "prepare"})
		if err == nil || result == nil || result.ExitCode == 0 || !strings.Contains(result.Stderr, "invalid current producer occurrence") {
			t.Fatalf("stale intended producer unexpectedly passed: %#v err=%v", result, err)
		}
	})
}

func TestWorksetCompleteVerificationPassesProducerClaimsToCASResolution(t *testing.T) {
	c, receipt := newCompleteWorksetCoordinator(t)
	conflicting := receipt.SourceArtifact
	conflicting.Bytes++
	conflicting.ByteSize++
	c.taskResults["prepare"].Artifacts[0] = conflicting
	receipt.SourceArtifact = conflicting
	result, err := c.executeWorksetCompleteVerification(context.Background(), VerificationSpec{Type: VerifyWorksetComplete, WorksetSourceTask: "prepare"})
	if err == nil || result == nil || result.ExitCode == 0 || !strings.Contains(result.Stderr, "byte count conflicts with immutable metadata") {
		t.Fatalf("conflicting producer size claim was accepted: %#v err=%v", result, err)
	}
}

func TestWorksetCompleteVerificationRejectsClearedReceiptSizeClaims(t *testing.T) {
	c, receipt := newCompleteWorksetCoordinator(t)
	receipt.SourceArtifact.Bytes = 0
	receipt.SourceArtifact.ByteSize = 0
	result, err := c.executeWorksetCompleteVerification(context.Background(), VerificationSpec{Type: VerifyWorksetComplete, WorksetSourceTask: "prepare"})
	if err == nil || result == nil || result.ExitCode == 0 || !strings.Contains(result.Stderr, "byte count claims") {
		t.Fatalf("cleared receipt size claims result=%#v err=%v", result, err)
	}
}

func TestWorksetCompleteVerificationAcceptsStructuredActionArtifactOutput(t *testing.T) {
	c, _ := newCompleteWorksetCoordinator(t)
	producerResult := c.taskResults["prepare"]
	ref := producerResult.Artifacts[0]
	ref.Provider = "fake-action-adapter"
	producerResult.Artifacts = nil
	producerResult.Outputs = map[string]StructuredOutputValue{
		"manifest": {Kind: ExecutionOutputArtifact, Artifact: &ref},
	}
	verificationResult, err := c.executeWorksetCompleteVerification(context.Background(), VerificationSpec{
		Type: VerifyWorksetComplete, WorksetSourceTask: "prepare", WorksetRequireTerminal: true,
		WorksetAcceptedStatuses: []string{TaskResultStatusSuccess, TaskResultStatusCompletedWithGaps},
	})
	if err != nil || verificationResult == nil || verificationResult.ExitCode != 0 {
		t.Fatalf("structured action workset result=%#v err=%v", verificationResult, err)
	}
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
