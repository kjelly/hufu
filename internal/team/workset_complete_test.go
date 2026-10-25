package team

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newWorksetCompleteCoordinator builds a coordinator whose task list holds a
// two-child workset ("a" done with success, "b" done with completed_with_gaps)
// produced by task "prepare" in run-1, with the receipt attached to child a.
// The same manifest content was also published by an older run so producer
// occurrence checks can be exercised.
func newWorksetCompleteCoordinator(t *testing.T) (*Coordinator, *WorksetExpansionReceipt) {
	t.Helper()
	c := newTestCoordinator(t)
	c.executionRunID = "run-1"
	workspace := c.session.Workspace
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
		SourceSHA256: source.SHA256, SourceArtifact: source.ArtifactRef, ItemCount: 2, Children: map[string]string{},
	}
	binding := func(key string) *WorksetBinding {
		return &WorksetBinding{WorksetID: "workset-1", ParentTaskID: "prepare", ItemKey: key, SourceArtifactID: source.ID, SourceSHA256: source.SHA256, SourceArtifact: source.ArtifactRef}
	}
	items := c.taskTracker.TodoList().AddBatch([]TodoSpec{
		{PlanTaskID: "child-a", Agent: "worker", Desc: "a", WorksetBinding: binding("a")},
		{PlanTaskID: "child-b", Agent: "worker", Desc: "b", WorksetBinding: binding("b")},
	})
	for i, status := range []string{TaskResultStatusSuccess, TaskResultStatusCompletedWithGaps} {
		items[i].Status = TaskDone
		items[i].TypedResult = &TaskResult{TaskID: items[i].PlanTaskID, Status: status, Summary: items[i].Desc, Source: "submitted"}
		items[i].VerifyResult = &VerificationResult{ExitCode: 0}
		receipt.Children[items[i].Desc] = items[i].ID
	}
	items[0].WorksetReceipt = receipt
	c.taskResults = map[string]*TaskResult{
		"prepare": {TaskID: "prepare", Attempt: 2, Agent: "producer", Status: TaskResultStatusSuccess, Artifacts: []ArtifactRef{source.ArtifactRef}},
	}
	return c, receipt
}

func worksetCompleteSpec() VerificationSpec {
	return VerificationSpec{Type: VerifyWorksetComplete, WorksetSourceTask: "prepare"}
}

func TestWorksetCompleteVerificationPassesAllVerifiedChildren(t *testing.T) {
	c, _ := newWorksetCompleteCoordinator(t)
	spec := VerificationSpec{
		Type: VerifyWorksetComplete, WorksetSourceTask: " PREPARE ", WorksetRequireTerminal: true,
		WorksetRequireVerified: true, WorksetAcceptedStatuses: []string{TaskResultStatusSuccess, TaskResultStatusCompletedWithGaps},
	}
	result, err := c.executeWorksetCompleteVerification(context.Background(), spec)
	if err != nil || result == nil || result.ExitCode != 0 {
		t.Fatalf("workset_complete result=%#v err=%v", result, err)
	}
	states := c.WorksetGroupStates()
	if len(states) != 1 || states[0].State != "complete" || states[0].Expected != 2 || states[0].Completed != 2 || states[0].Verified != 2 {
		t.Fatalf("workset state = %#v", states)
	}

	// The same spec passes when it runs through the acceptance gate.
	c.acceptanceSpec = &AcceptanceSpec{Verifications: []VerificationSpec{spec}}
	acceptance, err := c.runAcceptance(context.Background())
	if err != nil || acceptance == nil || !acceptance.IsPassed() || len(acceptance.VerificationEvidence) != 1 || acceptance.VerificationEvidence[0].ExitCode != 0 {
		t.Fatalf("acceptance result=%#v err=%v", acceptance, err)
	}
}

func TestWorksetGroupStatesDistinguishesUnfinishedAndFailedChildren(t *testing.T) {
	tests := []struct {
		name      string
		statuses  []TaskStatus
		results   []string
		wantState string
		pending   int
		active    int
		completed int
		verified  int
		skipped   int
		failed    int
	}{
		{name: "pending", statuses: []TaskStatus{TaskPending, TaskPlanned}, wantState: "partial", pending: 2},
		{name: "active", statuses: []TaskStatus{TaskInProgress, TaskVerifying}, wantState: "in_progress", active: 2},
		{name: "partially completed", statuses: []TaskStatus{TaskDone, TaskPending}, results: []string{TaskResultStatusSuccess, ""}, wantState: "partial", pending: 1, completed: 1, verified: 1},
		{name: "terminal failure", statuses: []TaskStatus{TaskError, TaskPending}, results: []string{TaskResultStatusFailed, ""}, wantState: "failed", pending: 1, failed: 1},
		{name: "skipped is not failed", statuses: []TaskStatus{TaskSkipped, TaskPending}, wantState: "partial", pending: 1, skipped: 1},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, _ := newWorksetCompleteCoordinator(t)
			items := c.taskTracker.TodoList().Items()
			for i, status := range tt.statuses {
				items[i].Status = status
				items[i].TypedResult = nil
				items[i].VerifyResult = nil
				if i < len(tt.results) && tt.results[i] != "" {
					items[i].TypedResult = &TaskResult{TaskID: items[i].ID, Status: tt.results[i], Source: "submitted"}
					items[i].VerifyResult = &VerificationResult{ExitCode: 0}
				}
			}
			c.taskTracker.TodoList().Restore(items)

			states := c.WorksetGroupStates()
			if len(states) != 1 {
				t.Fatalf("workset states = %#v, want one", states)
			}
			got := states[0]
			if got.State != tt.wantState || got.Pending != tt.pending || got.Active != tt.active ||
				got.Completed != tt.completed || got.Verified != tt.verified || got.Skipped != tt.skipped || got.Failed != tt.failed {
				t.Fatalf("workset state = %#v, want state=%q pending=%d active=%d completed=%d verified=%d skipped=%d failed=%d",
					got, tt.wantState, tt.pending, tt.active, tt.completed, tt.verified, tt.skipped, tt.failed)
			}
		})
	}
}

func TestFindWorksetReceiptMatchesNormalizedParentAndFailsClosed(t *testing.T) {
	// withSecondReceipt attaches a derived receipt to child b and returns the
	// lookup error for the (case-insensitive) parent.
	withSecondReceipt := func(t *testing.T, mutate func(*WorksetExpansionReceipt)) (*Coordinator, error) {
		t.Helper()
		c, receipt := newWorksetCompleteCoordinator(t)
		second := cloneWorksetReceipt(receipt)
		mutate(second)
		items := c.taskTracker.TodoList().Items()
		items[1].WorksetReceipt = second
		c.taskTracker.TodoList().Restore(items)
		_, err := c.findWorksetReceipt("prepare")
		return c, err
	}

	t.Run("unknown", func(t *testing.T) {
		c, _ := newWorksetCompleteCoordinator(t)
		if _, err := c.findWorksetReceipt("missing"); err == nil {
			t.Fatal("unknown source-task unexpectedly found a receipt")
		}
	})

	t.Run("multiple distinct receipts", func(t *testing.T) {
		if _, err := withSecondReceipt(t, func(r *WorksetExpansionReceipt) { r.WorksetID = "workset-2" }); err == nil || !strings.Contains(err.Error(), "ambiguous") {
			t.Fatalf("multiple receipts error = %v", err)
		}
	})

	t.Run("equivalent replayed receipts", func(t *testing.T) {
		c, err := withSecondReceipt(t, func(*WorksetExpansionReceipt) {})
		if err != nil {
			t.Fatalf("equivalent receipts unexpectedly conflicted: %v", err)
		}
		items := c.taskTracker.TodoList().Items()
		projected, err := c.worksetReceiptsFromTasks(items)
		if err != nil || len(projected) != 1 || projected[0].WorksetID != items[0].WorksetReceipt.WorksetID {
			t.Fatalf("equivalent checkpoint receipts = %#v, err=%v; want one deduplicated receipt", projected, err)
		}
	})

	for name, mutate := range map[string]func(*WorksetExpansionReceipt){
		"parent other":       func(r *WorksetExpansionReceipt) { r.ParentTaskID = "other" },
		"parent OTHER":       func(r *WorksetExpansionReceipt) { r.ParentTaskID = "OTHER" },
		"run ID":             func(r *WorksetExpansionReceipt) { r.RunID = "run-2" },
		"source artifact ID": func(r *WorksetExpansionReceipt) { r.SourceArtifactID = "artifact-2" },
		"source SHA256":      func(r *WorksetExpansionReceipt) { r.SourceSHA256 = strings.Repeat("b", 64) },
		"item count":         func(r *WorksetExpansionReceipt) { r.ItemCount++ },
		"item keys SHA256":   func(r *WorksetExpansionReceipt) { r.ItemKeysSHA256 = "keys-2" },
		"child mapping":      func(r *WorksetExpansionReceipt) { r.Children["a"] = "child-conflict" },
	} {
		t.Run("conflicting "+name, func(t *testing.T) {
			if _, err := withSecondReceipt(t, mutate); err == nil || !strings.Contains(err.Error(), "conflicting") {
				t.Fatalf("conflicting receipt error = %v", err)
			}
		})
	}

	t.Run("replayed receipt", func(t *testing.T) {
		c, _ := newWorksetCompleteCoordinator(t)
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
	c, receipt := newWorksetCompleteCoordinator(t)
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

	events := make([]RunEvent, 0, len(items))
	for _, item := range items {
		payload, err := json.Marshal(item)
		if err != nil {
			t.Fatal(err)
		}
		events = append(events, RunEvent{Type: string(EventTaskCreated), TaskID: item.ID, Payload: payload})
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

// TestWorksetCompleteVerificationSourceAndReceiptIntegrity mutates the
// producer occurrence, the receipt, or the children before running the
// workset_complete verifier. A non-empty wantStderr means the verifier must
// fail with that diagnostic; an empty one means it must pass.
func TestWorksetCompleteVerificationSourceAndReceiptIntegrity(t *testing.T) {
	strict := func() VerificationSpec {
		return NormalizeVerificationSpec(VerificationSpec{Type: VerifyWorksetComplete, WorksetSourceTask: "prepare", WorksetRequireTerminal: true, WorksetRequireVerified: true}, "", "")
	}
	accepting := func() VerificationSpec {
		spec := worksetCompleteSpec()
		spec.WorksetAcceptedStatuses = []string{TaskResultStatusSuccess, TaskResultStatusCompletedWithGaps}
		return spec
	}
	// unrelatedProducer registers a different task that published the same
	// artifact content.
	unrelatedProducer := func(c *Coordinator, receipt *WorksetExpansionReceipt) {
		unrelated := receipt.SourceArtifact
		unrelated.TaskID = "unrelated"
		c.taskResults["unrelated"] = &TaskResult{
			TaskID: "unrelated", Attempt: unrelated.Attempt, Agent: unrelated.Agent,
			Status: TaskResultStatusSuccess, Artifacts: []ArtifactRef{unrelated},
		}
	}

	tests := []struct {
		name       string
		mutate     func(t *testing.T, c *Coordinator, receipt *WorksetExpansionReceipt)
		spec       func() VerificationSpec
		wantPass   bool
		wantStderr string
	}{
		{
			name: "failed child",
			mutate: func(t *testing.T, c *Coordinator, _ *WorksetExpansionReceipt) {
				items := c.taskTracker.TodoList().Items()
				if err := c.taskTracker.TodoList().SetTypedResult(items[1].ID, &TaskResult{TaskID: items[1].ID, Status: TaskResultStatusPartial, Summary: "partial", Source: "submitted"}); err != nil {
					t.Fatal(err)
				}
			},
			spec: strict,
		},
		{
			name: "extra child",
			mutate: func(_ *testing.T, c *Coordinator, _ *WorksetExpansionReceipt) {
				item := c.taskTracker.TodoList().AddBatch([]TodoSpec{{PlanTaskID: "child-extra", Agent: "worker", WorksetBinding: &WorksetBinding{WorksetID: "workset-1", ItemKey: "extra"}}})[0]
				item.Status = TaskDone
				item.TypedResult = &TaskResult{Status: TaskResultStatusSuccess, Source: "submitted"}
			},
		},
		{
			name: "stale source content",
			mutate: func(t *testing.T, c *Coordinator, receipt *WorksetExpansionReceipt) {
				path := filepath.Join(c.session.Workspace, logsDir, "artifacts", "data", receipt.SourceArtifactID)
				if err := os.WriteFile(path, []byte("changed"), 0o644); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "old occurrence remains valid for old run",
			mutate: func(_ *testing.T, c *Coordinator, receipt *WorksetExpansionReceipt) {
				oldRef := c.taskResults["prepare"].Artifacts[0]
				oldRef.RunID, oldRef.Attempt = "old-run", 1
				c.taskResults["prepare"].Artifacts[0] = oldRef
				c.taskResults["prepare"].Attempt = 1
				c.executionRunID = "old-run"
				receipt.RunID = "old-run"
				receipt.SourceArtifact = oldRef
			},
			spec:     accepting,
			wantPass: true,
		},
		{
			name: "current receipt rejects old producer occurrence",
			mutate: func(_ *testing.T, c *Coordinator, _ *WorksetExpansionReceipt) {
				oldRef := c.taskResults["prepare"].Artifacts[0]
				oldRef.RunID, oldRef.Attempt = "old-run", 1
				c.taskResults["prepare"].Artifacts[0] = oldRef
			},
			wantStderr: "invalid current producer occurrence",
		},
		{
			name:       "old receipt rejects current run",
			mutate:     func(_ *testing.T, _ *Coordinator, receipt *WorksetExpansionReceipt) { receipt.RunID = "old-run" },
			wantStderr: "workset receipt belongs to run",
		},
		{
			name: "intended producer missing despite same-content producer",
			mutate: func(_ *testing.T, c *Coordinator, receipt *WorksetExpansionReceipt) {
				delete(c.taskResults, "prepare")
				unrelatedProducer(c, receipt)
			},
			wantStderr: "intended canonical producer task",
		},
		{
			name: "intended producer stale despite same-content producer",
			mutate: func(_ *testing.T, c *Coordinator, receipt *WorksetExpansionReceipt) {
				stale := receipt.SourceArtifact
				stale.RunID = "old-run"
				c.taskResults["prepare"].Artifacts[0] = stale
				unrelatedProducer(c, receipt)
			},
			wantStderr: "invalid current producer occurrence",
		},
		{
			name: "producer byte claims conflict with CAS metadata",
			mutate: func(_ *testing.T, c *Coordinator, receipt *WorksetExpansionReceipt) {
				conflicting := receipt.SourceArtifact
				conflicting.Bytes++
				conflicting.ByteSize++
				c.taskResults["prepare"].Artifacts[0] = conflicting
				receipt.SourceArtifact = conflicting
			},
			wantStderr: "byte count conflicts with immutable metadata",
		},
		{
			name: "cleared receipt size claims",
			mutate: func(_ *testing.T, _ *Coordinator, receipt *WorksetExpansionReceipt) {
				receipt.SourceArtifact.Bytes = 0
				receipt.SourceArtifact.ByteSize = 0
			},
			wantStderr: "byte count claims",
		},
		{
			name: "zero-item receipt",
			mutate: func(_ *testing.T, c *Coordinator, receipt *WorksetExpansionReceipt) {
				receipt.ItemCount = 0
				receipt.Children = map[string]string{}
				c.taskTracker.TodoList().Items()[0].WorksetReceipt = receipt
			},
			wantStderr: "cardinality",
		},
		{
			name: "structured action artifact output is an accepted source",
			mutate: func(_ *testing.T, c *Coordinator, _ *WorksetExpansionReceipt) {
				producer := c.taskResults["prepare"]
				ref := producer.Artifacts[0]
				ref.Provider = "fake-action-adapter"
				producer.Artifacts = nil
				producer.Outputs = map[string]StructuredOutputValue{"manifest": {Kind: ExecutionOutputArtifact, Artifact: &ref}}
			},
			spec: func() VerificationSpec {
				spec := accepting()
				spec.WorksetRequireTerminal = true
				return spec
			},
			wantPass: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c, receipt := newWorksetCompleteCoordinator(t)
			tt.mutate(t, c, receipt)
			spec := worksetCompleteSpec()
			if tt.spec != nil {
				spec = tt.spec()
			}
			result, err := c.executeWorksetCompleteVerification(context.Background(), spec)
			if tt.wantPass {
				if err != nil || result == nil || result.ExitCode != 0 {
					t.Fatalf("workset_complete result=%#v err=%v, want pass", result, err)
				}
				return
			}
			if err == nil || result == nil || result.ExitCode == 0 {
				t.Fatalf("workset_complete unexpectedly passed: %#v err=%v", result, err)
			}
			if tt.wantStderr != "" && !strings.Contains(result.Stderr, tt.wantStderr) {
				t.Fatalf("workset_complete stderr = %q, want %q", result.Stderr, tt.wantStderr)
			}
		})
	}
}
