package team

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/kjelly/hufu/internal/runtimestore"
)

// branchSwitchFixture is a workspace with main and a sibling branch, a live
// session, compaction state, and events on main.
func branchSwitchFixture(t *testing.T) (string, *SessionTree, *EventStore) {
	t.Helper()
	workspace := t.TempDir()
	es, err := NewEventStore(workspace, "run-switch", "session-switch")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = es.Close() })
	if err := es.Append(RunEvent{Type: "run_started", Actor: "user", Payload: []byte(`{"goal":"switch"}`)}); err != nil {
		t.Fatal(err)
	}
	st := NewSessionTree()
	if _, err := st.CreateRootBranch("sibling"); err != nil {
		t.Fatal(err)
	}
	if err := SaveSessionTree(workspace, st); err != nil {
		t.Fatal(err)
	}
	session := NewSession()
	session.Tasks = []*TodoItem{{ID: "live-task", Desc: "live work", Status: TaskInProgress}}
	if err := SaveSession(workspace, session); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadSessionTree(workspace)
	if err != nil {
		t.Fatal(err)
	}
	return workspace, loaded, es
}

// runtimeStateSnapshot captures everything a branch switch may write.
type runtimeStateSnapshot struct {
	session, compaction []byte
	active              string
	branches            map[string]string
}

func snapshotRuntimeState(t *testing.T, workspace string) runtimeStateSnapshot {
	t.Helper()
	snapshot := runtimeStateSnapshot{branches: make(map[string]string)}
	snapshot.session, _ = getRuntimeDocument(t, workspace, runtimestore.DocumentSession)
	snapshot.compaction, _ = getRuntimeDocument(t, workspace, runtimestore.DocumentConversationCompactionState)
	withJournal(t, workspace, func(q runtimestore.Queryer) {
		ctx := context.Background()
		if err := q.QueryRowContext(ctx, `SELECT active_branch FROM session_tree_head WHERE id = 1`).Scan(&snapshot.active); err != nil {
			t.Fatal(err)
		}
		rows, err := q.QueryContext(ctx, `SELECT id, name || parent_id || fork_event_id || created_at || CAST(state AS TEXT) FROM session_branches`)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var id, row string
			if err := rows.Scan(&id, &row); err != nil {
				t.Fatal(err)
			}
			snapshot.branches[id] = row
		}
	})
	return snapshot
}

func (s runtimeStateSnapshot) equal(other runtimeStateSnapshot) bool {
	if string(s.session) != string(other.session) || string(s.compaction) != string(other.compaction) || s.active != other.active || len(s.branches) != len(other.branches) {
		return false
	}
	for id, row := range s.branches {
		if other.branches[id] != row {
			return false
		}
	}
	return true
}

// TestBranchSwitchCommitsAllOrNothing pins D9: when the transaction fails
// after the documents were written, no branch row, document, or active
// branch changes, and the caller's tree is restored.
func TestBranchSwitchCommitsAllOrNothing(t *testing.T) {
	tests := []struct {
		name string
		run  func(workspace string, st *SessionTree, es *EventStore) error
	}{
		{name: "fork", run: func(workspace string, st *SessionTree, es *EventStore) error {
			_, err := ForkSessionBranch(context.Background(), workspace, st, es, "child", "")
			return err
		}},
		{name: "checkout", run: func(workspace string, st *SessionTree, es *EventStore) error {
			_, _, err := CheckoutSessionBranch(context.Background(), workspace, st, es, "sibling")
			return err
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workspace, st, es := branchSwitchFixture(t)
			before := snapshotRuntimeState(t, workspace)
			treeBefore := cloneSessionTree(st)
			injected := errors.New("injected commit failure")
			previous := runtimeBatchCommitStage
			runtimeBatchCommitStage = func() error { return injected }
			t.Cleanup(func() { runtimeBatchCommitStage = previous })

			if err := tt.run(workspace, st, es); !errors.Is(err, injected) {
				t.Fatalf("switch error = %v, want the injected failure", err)
			}
			if after := snapshotRuntimeState(t, workspace); !before.equal(after) {
				t.Fatalf("failed switch changed runtime state:\nbefore %+v\nafter  %+v", before, after)
			}
			if st.ActiveBranch != treeBefore.ActiveBranch || len(st.Branches) != len(treeBefore.Branches) {
				t.Fatalf("failed switch left the caller's tree changed: active %q with %d branches", st.ActiveBranch, len(st.Branches))
			}

			runtimeBatchCommitStage = previous
			if err := tt.run(workspace, st, es); err != nil {
				t.Fatalf("switch after the failure: %v", err)
			}
			if after := snapshotRuntimeState(t, workspace); after.active == before.active {
				t.Fatalf("switch did not change the active branch from %q", before.active)
			}
		})
	}
}

// countBranchWrites records the session_branches rows the shared connection
// inserts or updates until the returned function reports them.
func countBranchWrites(t *testing.T, workspace string) func() []string {
	t.Helper()
	store, err := runtimestore.Open(context.Background(), workspace)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	for _, statement := range []string{
		`CREATE TEMP TABLE branch_writes(id TEXT)`,
		`CREATE TEMP TRIGGER branch_inserted AFTER INSERT ON session_branches BEGIN INSERT INTO branch_writes VALUES(new.id); END`,
		`CREATE TEMP TRIGGER branch_updated AFTER UPDATE ON session_branches BEGIN INSERT INTO branch_writes VALUES(new.id); END`,
	} {
		if _, err := store.DB().Exec(statement); err != nil {
			t.Fatal(err)
		}
	}
	return func() []string {
		rows, err := store.DB().Query(`SELECT DISTINCT id FROM branch_writes ORDER BY id`)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = rows.Close() }()
		var ids []string
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				t.Fatal(err)
			}
			ids = append(ids, id)
		}
		return ids
	}
}

// TestBranchStateWritesTouchOnlyTheirRows pins that a fork writes only the
// forked-from and new branch rows, and a checkpoint only the active row.
func TestBranchStateWritesTouchOnlyTheirRows(t *testing.T) {
	tests := []struct {
		name  string
		write func(workspace string, st *SessionTree, es *EventStore) error
		want  []string
	}{
		{name: "fork", write: func(workspace string, st *SessionTree, es *EventStore) error {
			_, err := ForkSessionBranch(context.Background(), workspace, st, es, "child", "")
			return err
		}, want: []string{"child", "main"}},
		{name: "checkpoint", write: func(workspace string, _ *SessionTree, _ *EventStore) error {
			return updateActiveBranchState(workspace, func(state *BranchState) { state.ActiveModel = "next-model" })
		}, want: []string{"main"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workspace, st, es := branchSwitchFixture(t)
			written := countBranchWrites(t, workspace)
			if err := tt.write(workspace, st, es); err != nil {
				t.Fatal(err)
			}
			if got := written(); !slices.Equal(got, tt.want) {
				t.Fatalf("written branch rows = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestForkSessionBranchUsesSelectedTaskHistory(t *testing.T) {
	for _, target := range []string{"empty", "before_task", "active_head"} {
		t.Run(target, func(t *testing.T) {
			workspace := t.TempDir()
			es, err := NewEventStore(workspace, "run-fork", "session-fork")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = es.Close() })
			st := NewSessionTree()
			active, err := st.CreateRootBranch("work")
			if err != nil {
				t.Fatal(err)
			}
			st.ActiveBranch = active.ID
			es.SetBranchID(active.ID)
			before, err := es.AppendPersisted(RunEvent{Type: string(EventUserMessageAdded), Actor: "user", Payload: []byte(`{"role":"user","content":"start"}`)})
			if err != nil {
				t.Fatal(err)
			}
			appendBranchEvent(t, es, RunEvent{Type: string(EventTaskCompleted), TaskID: "1", Actor: "worker", Payload: []byte(`{"id":"1","agent":"worker","desc":"accepted work","status":"done"}`)})
			live := ReduceToSessionData(FilterEventsForBranch(mustReadEvents(t, es), st, active.ID))
			if err := SaveSession(workspace, live); err != nil {
				t.Fatal(err)
			}
			if err := SaveSessionTree(workspace, st); err != nil {
				t.Fatal(err)
			}
			forkTarget := map[string]string{"empty": "main", "before_task": before.ID, "active_head": active.ID}[target]
			branch, err := ForkSessionBranch(t.Context(), workspace, st, es, "child", forkTarget)
			if err != nil {
				t.Fatal(err)
			}
			got := LoadSession(workspace)
			want := 0
			if target == "active_head" {
				want = 1
			}
			if got == nil || len(got.Tasks) != want {
				t.Fatalf("forked checkpoint = %+v, want %d tasks", got, want)
			}
			if len(branch.State.TaskPlan) != want {
				t.Fatalf("forked branch snapshot retained %d tasks, want %d", len(branch.State.TaskPlan), want)
			}
			canonical, err := ReplayTodoList(FilterEventsForBranch(mustReadEvents(t, es), st, branch.ID))
			if err != nil || len(canonical) != len(got.Tasks) {
				t.Fatalf("fork disagrees with replay: tasks=%d replay=%d err=%v", len(got.Tasks), len(canonical), err)
			}
			c := newTestCoordinator(t)
			c.session.Workspace = workspace
			c.SetSessionData(got)
			c.initEventStore()
			if c.sessionData.RecoveryRequired {
				t.Fatalf("fork cannot resume: %s", c.sessionData.RecoveryReason)
			}
			if len(st.Branches[active.ID].State.TaskPlan) != 1 {
				t.Fatal("fork lost the outgoing branch's work")
			}
			if _, _, err := CheckoutSessionBranch(t.Context(), workspace, st, es, active.ID); err != nil {
				t.Fatal(err)
			}
			if restored := LoadSession(workspace); len(restored.Tasks) != 1 || restored.Tasks[0].Status != TaskDone {
				t.Fatalf("returning to original branch lost accepted work: %+v", restored.Tasks)
			}
		})
	}
}

func TestForkBeforeCompactionClearsFutureSummary(t *testing.T) {
	workspace := t.TempDir()
	es, err := NewEventStore(workspace, "run-fork", "session-fork")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = es.Close() })
	before, err := es.AppendPersisted(RunEvent{Type: string(EventUserMessageAdded), Actor: "user", Payload: []byte(`{"role":"user","content":"before compaction"}`)})
	if err != nil {
		t.Fatal(err)
	}
	state := testCompactionState(t, workspace)
	checkpoint := state.Branches["main"]
	generation := state.Generations[checkpoint.GenerationID]
	generationEvent, err := compactionGenerationAttestationEvent(generation)
	if err != nil {
		t.Fatal(err)
	}
	appendBranchEvent(t, es, generationEvent)
	checkpointEvent, err := compactionCheckpointAttestationEvent(checkpoint, generation)
	if err != nil {
		t.Fatal(err)
	}
	appendBranchEvent(t, es, checkpointEvent)
	st := NewSessionTree()
	if err := SaveSession(workspace, NewSession()); err != nil {
		t.Fatal(err)
	}
	branch, err := ForkSessionBranch(t.Context(), workspace, st, es, "before-summary", before.ID)
	if err != nil {
		t.Fatal(err)
	}
	if branch.State.Compaction != nil || st.Branches["main"].State.Compaction == nil {
		t.Fatal("fork must clear the future summary while preserving the parent's summary")
	}
}

func TestSnapshotBranchStateClearsEmptyTaskPlan(t *testing.T) {
	workspace := t.TempDir()
	st := NewSessionTree()
	st.Branches["main"].State.TaskPlan = []*TodoItem{{ID: "stale", Status: TaskDone}}
	if err := SaveSession(workspace, NewSession()); err != nil {
		t.Fatal(err)
	}
	SnapshotBranchState(workspace, st, "main")
	if len(st.Branches["main"].State.TaskPlan) != 0 {
		t.Fatal("empty checkpoint retained stale tasks in its branch snapshot")
	}
}

// TestSessionTreeRoundTripsAndDropsRemovedBranches pins the table form of the
// tree: labels and branch fields survive a reload, a branch removed from a
// loaded tree is deleted, and a tree built in memory replaces the stored one.
func TestSessionTreeRoundTripsAndDropsRemovedBranches(t *testing.T) {
	workspace := t.TempDir()
	st := NewSessionTree()
	child, err := st.CreateBranch("child", "", nil)
	if err != nil {
		t.Fatal(err)
	}
	child.ForkEventID = "evt-fork"
	child.State.ActiveModel = "model-a"
	if err := st.AddLabel("release", child.ID); err != nil {
		t.Fatal(err)
	}
	st.ActiveBranch = child.ID
	if err := SaveSessionTree(workspace, st); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadSessionTree(workspace)
	if err != nil {
		t.Fatal(err)
	}
	got := loaded.Branches[child.ID]
	if loaded.ActiveBranch != child.ID || got == nil || got.ParentID != "main" || got.ForkEventID != "evt-fork" || got.State.ActiveModel != "model-a" || loaded.Labels["release"] != child.ID {
		t.Fatalf("reloaded tree = %+v (child %+v)", loaded, got)
	}

	delete(loaded.Branches, child.ID)
	loaded.ActiveBranch = "main"
	if err := SaveSessionTree(workspace, loaded); err != nil {
		t.Fatal(err)
	}
	if reloaded, err := LoadSessionTree(workspace); err != nil || len(reloaded.Branches) != 1 {
		t.Fatalf("tree after dropping a branch = %+v, %v", reloaded, err)
	}

	fresh := NewSessionTree()
	if _, err := fresh.CreateRootBranch("other"); err != nil {
		t.Fatal(err)
	}
	if err := SaveSessionTree(workspace, fresh); err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadSessionTree(workspace)
	if err != nil || len(reloaded.Branches) != 2 || reloaded.Branches["other"] == nil {
		t.Fatalf("tree after saving an in-memory tree = %+v, %v", reloaded, err)
	}
}
