package team

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSessionTree_DefaultMain(t *testing.T) {
	st := NewSessionTree()
	if st.ActiveBranch != "main" {
		t.Fatalf("expected ActiveBranch to be main, got %q", st.ActiveBranch)
	}
	mainBranch := st.GetBranch("main")
	if mainBranch == nil {
		t.Fatalf("expected main branch to exist")
	}
	if mainBranch.Name != "main" {
		t.Errorf("expected main branch name to be main, got %q", mainBranch.Name)
	}
}

func TestSessionTree_CreateBranch(t *testing.T) {
	st := NewSessionTree()
	b1, err := st.CreateBranch("feature/fix-bug", "", nil)
	if err != nil {
		t.Fatalf("CreateBranch failed: %v", err)
	}
	if b1.Name != "feature/fix-bug" {
		t.Errorf("expected branch name feature/fix-bug, got %q", b1.Name)
	}
	if b1.ParentID != "main" {
		t.Errorf("expected parent ID main, got %q", b1.ParentID)
	}

	// Sub-branch
	st.ActiveBranch = b1.ID
	b2, err := st.CreateBranch("feature/sub-fix", "", nil)
	if err != nil {
		t.Fatalf("CreateBranch for sub-branch failed: %v", err)
	}
	if b2.ParentID != b1.ID {
		t.Errorf("expected parent ID %q, got %q", b1.ID, b2.ParentID)
	}

	// Duplicate error
	_, err = st.CreateBranch("feature/fix-bug", "", nil)
	if err == nil {
		t.Errorf("expected error when creating duplicate branch")
	}
}

func TestSessionTree_Checkout(t *testing.T) {
	st := NewSessionTree()
	b1, err := st.CreateBranch("experimental", "", nil)
	if err != nil {
		t.Fatalf("CreateBranch failed: %v", err)
	}

	cb, err := st.CheckoutBranch("experimental", nil)
	if err != nil {
		t.Fatalf("CheckoutBranch failed: %v", err)
	}
	if cb.ID != b1.ID {
		t.Errorf("expected checked out branch ID %q, got %q", b1.ID, cb.ID)
	}
	if st.ActiveBranch != b1.ID {
		t.Errorf("expected ActiveBranch %q, got %q", b1.ID, st.ActiveBranch)
	}

	// Checkout non-existent
	_, err = st.CheckoutBranch("non-existent", nil)
	if err == nil {
		t.Errorf("expected error checking out non-existent branch")
	}
}

func TestSessionTree_AddLabel(t *testing.T) {
	st := NewSessionTree()
	err := st.AddLabel("checkpoint-v1", "main")
	if err != nil {
		t.Fatalf("AddLabel failed: %v", err)
	}

	b := st.GetBranch("checkpoint-v1")
	if b == nil || b.ID != "main" {
		t.Errorf("expected label target main, got %v", b)
	}

	// Empty label validation
	if err := st.AddLabel("", "main"); err == nil {
		t.Errorf("expected error for empty label name")
	}
}

func TestSessionTree_RenderTree(t *testing.T) {
	st := NewSessionTree()
	_, _ = st.CreateBranch("feature-a", "", nil)
	_ = st.AddLabel("v1", "main")

	output := st.RenderTree(nil)
	if output == "" {
		t.Fatalf("rendered tree output is empty")
	}
	if !strings.Contains(output, "Session Tree:") || !strings.Contains(output, "main") || !strings.Contains(output, "feature-a") {
		t.Errorf("rendered tree output missing expected content:\n%s", output)
	}
}

func TestSessionTree_FilterEventsForBranch(t *testing.T) {
	st := NewSessionTree()
	tempDir := t.TempDir()

	es, err := NewEventStore(tempDir, "run-1", "session-1")
	if err != nil {
		t.Fatalf("NewEventStore failed: %v", err)
	}

	_ = es.Append(RunEvent{ID: "evt-1", BranchID: "main", Type: "task_started"})
	_ = es.Append(RunEvent{ID: "evt-2", BranchID: "main", Type: "task_completed", Payload: []byte(`{"status":"done"}`)})
	_ = es.Append(RunEvent{ID: "evt-3", BranchID: "main", Type: "task_started"})

	b1, err := st.CreateBranch("feature-b", "evt-3", es)
	if err != nil {
		t.Fatalf("CreateBranch failed: %v", err)
	}

	_ = es.Append(RunEvent{ID: "evt-4", BranchID: "main", Type: "task_completed", Payload: []byte(`{"status":"done"}`)})
	_ = es.Append(RunEvent{ID: "evt-5", BranchID: b1.ID, Type: "task_started"})
	_ = es.Append(RunEvent{ID: "evt-6", BranchID: b1.ID, Type: "task_completed", Payload: []byte(`{"status":"done"}`)})

	events, err := es.ReadEvents()
	if err != nil {
		t.Fatalf("ReadEvents failed: %v", err)
	}

	// Filter for feature-b
	filteredB := FilterEventsForBranch(events, st, b1.ID)
	if len(filteredB) != 5 {
		t.Errorf("expected 5 events for feature-b lineage, got %d", len(filteredB))
	}
	for _, e := range filteredB {
		if e.ID == "evt-4" {
			t.Errorf("event evt-4 from main after fork point should not be in feature-b lineage")
		}
	}

	// Filter for main
	filteredMain := FilterEventsForBranch(events, st, "main")
	if len(filteredMain) != 4 {
		t.Errorf("expected 4 events for main, got %d", len(filteredMain))
	}
	for _, e := range filteredMain {
		if e.BranchID == b1.ID {
			t.Errorf("event %s from feature-b should not pollute main branch", e.ID)
		}
	}
}

func TestWP6DiffBranchesShowsMemoryItemIDsWithoutContent(t *testing.T) {
	workspace := t.TempDir()
	es, err := NewEventStore(workspace, "run-1", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	defer es.Close()
	if err := es.Append(RunEvent{ID: "fork", BranchID: "main", Type: "worker_memory_confirmed", Payload: []byte(`{"item_id":"main-memory"}`)}); err != nil {
		t.Fatal(err)
	}
	tree := NewSessionTree()
	feature, err := tree.CreateBranch("feature", "fork", es)
	if err != nil {
		t.Fatal(err)
	}
	if err := es.Append(RunEvent{ID: "feature-memory", BranchID: feature.ID, Type: "worker_memory_confirmed", Payload: []byte(`{"item_id":"feature-private-memory","content":"must never render"}`)}); err != nil {
		t.Fatal(err)
	}
	diff, err := DiffBranches(workspace, tree, es, "main", feature.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(diff.MemoryDiffs) != 1 || diff.MemoryDiffs[0].ItemID != "feature-private-memory" || diff.MemoryDiffs[0].DiffType != "only_in_b" {
		t.Fatalf("memory diffs = %#v", diff.MemoryDiffs)
	}
	rendered := diff.RenderText()
	if !strings.Contains(rendered, "feature-private-memory") || strings.Contains(rendered, "must never render") {
		t.Fatalf("memory diff rendering leaked content or omitted ID: %s", rendered)
	}
}

func TestSessionTree_DiffBranches(t *testing.T) {
	tempDir := t.TempDir()
	st := NewSessionTree()

	b1, err := st.CreateBranch("feature-x", "", nil)
	if err != nil {
		t.Fatalf("CreateBranch failed: %v", err)
	}

	es, err := NewEventStore(tempDir, "run-1", "session-1")
	if err != nil {
		t.Fatalf("NewEventStore failed: %v", err)
	}

	payloadMain, _ := json.Marshal(map[string]string{
		"id":     "task-1",
		"desc":   "Initial task",
		"status": "done",
	})
	_ = es.Append(RunEvent{ID: "evt-m1", BranchID: "main", Type: "task_completed", Payload: payloadMain})

	payloadB1, _ := json.Marshal(map[string]string{
		"id":     "task-2",
		"desc":   "Feature X task",
		"status": "in_progress",
	})
	_ = es.Append(RunEvent{ID: "evt-f1", BranchID: b1.ID, Type: "task_started", Payload: payloadB1})

	diff, err := DiffBranches(tempDir, st, es, "main", b1.ID)
	if err != nil {
		t.Fatalf("DiffBranches failed: %v", err)
	}

	if diff.BranchA != "main" || diff.BranchB != b1.Name {
		t.Errorf("unexpected branch names in diff: %s vs %s", diff.BranchA, diff.BranchB)
	}
	if len(diff.TaskDiffs) == 0 {
		t.Errorf("expected task diffs between main and feature-x")
	}

	textOutput := diff.RenderText()
	if textOutput == "" || !strings.Contains(textOutput, "Session Diff:") {
		t.Errorf("expected valid rendered diff text, got:\n%s", textOutput)
	}
}

func TestSessionTree_Persistence(t *testing.T) {
	tempDir := t.TempDir()
	st := NewSessionTree()
	_, _ = st.CreateBranch("branch-persist", "", nil)
	_ = st.AddLabel("v2.0", "branch-persist")

	if err := SaveSessionTree(tempDir, st); err != nil {
		t.Fatalf("SaveSessionTree failed: %v", err)
	}

	loaded, err := LoadSessionTree(tempDir)
	if err != nil {
		t.Fatalf("LoadSessionTree failed: %v", err)
	}

	if loaded.ActiveBranch != st.ActiveBranch {
		t.Errorf("expected active branch %q, got %q", st.ActiveBranch, loaded.ActiveBranch)
	}
	if loaded.GetBranch("branch-persist") == nil {
		t.Errorf("expected branch-persist in loaded tree")
	}
	if loaded.Labels["v2.0"] != "branch-persist" {
		t.Errorf("expected label v2.0 -> branch-persist, got %q", loaded.Labels["v2.0"])
	}
}

// Events written by the coordinator currently carry no BranchID tag; they must
// resolve to the main lineage so `hufu session checkout <event-id>` works.
func TestSessionTree_CheckoutLegacyEventID(t *testing.T) {
	tempDir := t.TempDir()
	st := NewSessionTree()
	es, err := NewEventStore(tempDir, "run-1", "session-1")
	if err != nil {
		t.Fatalf("NewEventStore failed: %v", err)
	}
	_ = es.Append(RunEvent{ID: "evt-1", Type: "run_started"}) // BranchID empty, like production events

	b, err := st.CheckoutBranch("evt-1", es)
	if err != nil {
		t.Fatalf("checkout by legacy event ID failed: %v", err)
	}
	if b.ID != "main" {
		t.Errorf("expected legacy event to resolve to main, got %q", b.ID)
	}
}

func TestSessionTree_ForkUnresolvableTargetErrors(t *testing.T) {
	st := NewSessionTree()
	if _, err := st.CreateBranch("x", "does-not-exist", nil); err == nil {
		t.Errorf("expected error forking from unresolvable target, got silent fallback")
	}
}

// A forked branch must not alias the parent's mutable state: mutating the
// parent after the fork must leave the child's TaskPlan/Compaction untouched.
func TestSessionTree_CopyBranchStateDeepCopy(t *testing.T) {
	st := NewSessionTree()
	main := st.GetBranch("main")
	main.State.TaskPlan = []*TodoItem{{ID: "1", Desc: "t1", Status: TaskInProgress}}
	main.State.Compaction = &StructuredSummary{
		Goal:            "g",
		CompletedTasks:  []string{"a"},
		UserCorrections: []string{"keep-this"},
	}

	child, err := st.CreateBranch("child", "", nil)
	if err != nil {
		t.Fatalf("CreateBranch failed: %v", err)
	}

	main.State.TaskPlan[0].Status = TaskDone
	main.State.Compaction.CompletedTasks[0] = "mutated"
	main.State.Compaction.UserCorrections[0] = "mutated"

	if child.State.TaskPlan[0].Status != TaskInProgress {
		t.Errorf("child TaskPlan aliases parent: status=%q", child.State.TaskPlan[0].Status)
	}
	if child.State.Compaction.CompletedTasks[0] != "a" {
		t.Errorf("child Compaction.CompletedTasks aliases parent: %q", child.State.Compaction.CompletedTasks[0])
	}
	if child.State.Compaction.UserCorrections[0] != "keep-this" {
		t.Errorf("child Compaction.UserCorrections aliases parent: %q", child.State.Compaction.UserCorrections[0])
	}
}
