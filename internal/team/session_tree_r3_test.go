package team

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/anomalyco/hufu/internal/agent"
)

// saveCheckpoint must snapshot the coordinator's live state (task plan, active
// model, selected team, compaction) into the active session branch so branch
// state survives process restarts (R3).
func TestCoordinatorSaveCheckpoint_UpdatesBranchState(t *testing.T) {
	tempDir := t.TempDir()
	st := NewSessionTree()
	exp, err := st.CreateBranch("exp", "", nil)
	if err != nil {
		t.Fatalf("CreateBranch failed: %v", err)
	}
	st.ActiveBranch = exp.ID
	if err := SaveSessionTree(tempDir, st); err != nil {
		t.Fatalf("SaveSessionTree failed: %v", err)
	}

	c := &Coordinator{
		session: &TeamSession{
			Workspace: tempDir,
			Config: agent.TeamConfig{
				Name:       "team-x",
				Generation: agent.GenerationParams{Model: "m-1"},
			},
		},
		sessionData:           NewSession(),
		taskTracker:           NewTaskTracker(),
		lastCompactionSummary: &StructuredSummary{Goal: "g", CompletedTasks: []string{"x"}},
	}
	c.taskTracker.TodoList().AddBatch([]TodoSpec{{Agent: "dev", Desc: "t1"}})

	c.saveCheckpoint()

	loaded, err := LoadSessionTree(tempDir)
	if err != nil {
		t.Fatalf("LoadSessionTree failed: %v", err)
	}
	b := loaded.Branches[exp.ID]
	if b == nil {
		t.Fatalf("expected branch %q in saved tree", exp.ID)
	}
	if b.State.ActiveModel != "m-1" {
		t.Errorf("expected ActiveModel m-1, got %q", b.State.ActiveModel)
	}
	if b.State.SelectedTeam != "team-x" {
		t.Errorf("expected SelectedTeam team-x, got %q", b.State.SelectedTeam)
	}
	if len(b.State.TaskPlan) != 1 || b.State.TaskPlan[0].Desc != "t1" {
		t.Errorf("expected TaskPlan snapshot with t1, got %+v", b.State.TaskPlan)
	}
	if b.State.Compaction == nil || b.State.Compaction.Goal != "g" {
		t.Errorf("expected Compaction snapshot, got %+v", b.State.Compaction)
	}

	// The stored snapshot must be detached from the coordinator's live objects.
	c.lastCompactionSummary.CompletedTasks[0] = "mutated"
	if b.State.Compaction.CompletedTasks[0] != "x" {
		t.Errorf("stored Compaction aliases coordinator state: %q", b.State.Compaction.CompletedTasks[0])
	}
}

func TestRebuildSessionForBranchRestoresCriterionProgressTimestamp(t *testing.T) {
	tempDir := t.TempDir()
	es, err := NewEventStore(tempDir, "run-1", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = es.Close() }()
	progressedAt := time.Now().Add(-2 * time.Minute).UTC().Format(time.RFC3339Nano)
	payload, err := json.Marshal(map[string]any{"progress": ProgressAdvanced, "progressed_at": progressedAt, "after": []CriterionResult{{ID: "ready", State: CriterionPassed}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := es.Append(RunEvent{ID: "criterion-1", BranchID: "main", Type: "criterion_re_evaluated", Timestamp: progressedAt, Payload: payload}); err != nil {
		t.Fatal(err)
	}
	st := NewSessionTree()
	if err := RebuildSessionForBranch(tempDir, st, es, "main"); err != nil {
		t.Fatal(err)
	}
	sd := LoadSession(tempDir)
	if sd == nil || sd.LastCriterionProgressAt != progressedAt {
		t.Fatalf("progress timestamp not restored: %#v", sd)
	}
	c := &Coordinator{sessionData: sd}
	if got := c.Metrics().TimeSinceCriterionProgressSeconds; got < 60 {
		t.Fatalf("replayed time since progress = %d, want non-zero based on original timestamp", got)
	}
}

// Checkout-time travel: rebuilding a branch must rewrite session.json from the
// branch's event lineage so the next run resumes that branch's state (R3).
func TestRebuildSessionForBranch(t *testing.T) {
	tempDir := t.TempDir()
	es, err := NewEventStore(tempDir, "run-1", "session-1")
	if err != nil {
		t.Fatalf("NewEventStore failed: %v", err)
	}
	defer func() { _ = es.Close() }()

	msgPayload, _ := json.Marshal(map[string]string{"role": "user", "content": "hello main"})
	_ = es.Append(RunEvent{ID: "evt-1", BranchID: "main", Type: "user_message_added", Payload: msgPayload})
	task1, _ := json.Marshal(map[string]string{"id": "1", "desc": "main task", "status": "done"})
	_ = es.Append(RunEvent{ID: "evt-2", BranchID: "main", Type: "task_completed", TaskID: "1", Payload: task1})

	st := NewSessionTree()
	exp, err := st.CreateBranch("exp", "evt-2", es)
	if err != nil {
		t.Fatalf("CreateBranch failed: %v", err)
	}

	task2, _ := json.Marshal(map[string]string{"id": "2", "desc": "exp task", "status": "done"})
	_ = es.Append(RunEvent{ID: "evt-3", BranchID: exp.ID, Type: "task_completed", TaskID: "2", Payload: task2})

	// Live session currently reflects exp work.
	live := NewSession()
	live.Tasks = []*TodoItem{{ID: "2", Desc: "exp task", Status: TaskDone}}
	live.AddEntry("user", "hello main")
	live.AddEntry("user", "exp follow-up")
	if err := SaveSession(tempDir, live); err != nil {
		t.Fatalf("SaveSession failed: %v", err)
	}

	// Snapshot the outgoing branch, then travel back to main.
	SnapshotBranchState(tempDir, st, exp.ID)
	if got := st.Branches[exp.ID].State.TaskPlan; len(got) != 1 || got[0].ID != "2" {
		t.Fatalf("expected exp snapshot to keep task 2, got %+v", got)
	}

	if err := RebuildSessionForBranch(tempDir, st, es, "main"); err != nil {
		t.Fatalf("RebuildSessionForBranch(main) failed: %v", err)
	}
	sd := LoadSession(tempDir)
	if sd == nil {
		t.Fatalf("expected session.json to be rebuilt")
	}
	if len(sd.Tasks) != 1 || sd.Tasks[0].ID != "1" || sd.Tasks[0].Status != TaskDone {
		t.Errorf("expected main lineage tasks [1(done)], got %+v", sd.Tasks)
	}
	if len(sd.Entries) != 1 || sd.Entries[0].Content != "hello main" {
		t.Errorf("expected main lineage entries, got %+v", sd.Entries)
	}

	// Travel forward to exp: lineage = main-up-to-fork + exp.
	if err := RebuildSessionForBranch(tempDir, st, es, exp.ID); err != nil {
		t.Fatalf("RebuildSessionForBranch(exp) failed: %v", err)
	}
	sd2 := LoadSession(tempDir)
	if len(sd2.Tasks) != 2 {
		t.Fatalf("expected exp lineage tasks [1,2], got %+v", sd2.Tasks)
	}
	if sd2.Tasks[1].ID != "2" || sd2.Tasks[1].Status != TaskDone {
		t.Errorf("expected task 2 done in exp lineage, got %+v", sd2.Tasks[1])
	}
}

// A branch with no events and no snapshot must leave session.json untouched.
func TestRebuildSessionForBranch_NoDataNoop(t *testing.T) {
	tempDir := t.TempDir()
	st := NewSessionTree()
	b, err := st.CreateBranch("empty", "", nil)
	if err != nil {
		t.Fatalf("CreateBranch failed: %v", err)
	}

	live := NewSession()
	live.AddEntry("user", "keep me")
	if err := SaveSession(tempDir, live); err != nil {
		t.Fatalf("SaveSession failed: %v", err)
	}

	if err := RebuildSessionForBranch(tempDir, st, nil, b.ID); err != nil {
		t.Fatalf("RebuildSessionForBranch failed: %v", err)
	}
	sd := LoadSession(tempDir)
	if len(sd.Entries) != 1 || sd.Entries[0].Content != "keep me" {
		t.Errorf("session.json must be untouched for dataless branch, got %+v", sd.Entries)
	}
}
