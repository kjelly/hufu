package team

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"charm.land/fantasy"
)

// failingAppendJournal fails the first append of one event type, standing in
// for a crash at that point of an apply.
type failingAppendJournal struct {
	EventJournal
	failType EventType
	failed   bool
}

func (j *failingAppendJournal) Append(ctx context.Context, event RunEvent) (RunEvent, error) {
	if !j.failed && event.Type == string(j.failType) {
		j.failed = true
		return RunEvent{}, errors.New("injected crash")
	}
	return j.EventJournal.Append(ctx, event)
}

// interruptedApply runs an isolated task's apply up to a crash just before
// the named lifecycle event, then forgets the world as a restart would.
func (f *isolatedTeamFixture) interruptedApply(t *testing.T, goal string, crashBefore EventType) (*TodoItem, *isolatedAttempt) {
	t.Helper()
	item, world := f.startedIsolatedTask(t, goal)
	for rel, content := range map[string]string{"a.txt": "a after\n", "b.txt": "b after\n"} {
		if err := writeWorldFile(world.prepared.Root, rel, content); err != nil {
			t.Fatal(err)
		}
	}
	journal := f.c.EventJournal()
	f.c.SetEventJournal(&failingAppendJournal{EventJournal: journal, failType: crashBefore})
	result := &TaskResult{TaskID: item.ID, Agent: "worker", Status: TaskResultStatusSuccess, Summary: "applied before the crash", Source: "submitted"}
	err := f.c.integrateIsolatedAttempt(context.Background(), world, isolatedApplyEvidence{
		TypedResult: result, VerifyResult: &VerificationResult{Command: "true", ExitCode: 0},
		Receipt: &ExecutionReceipt{TaskID: item.ID, Attempt: 1, HandoffState: ResultHandoffSubmitted}, CoordinatorOutput: "applied before the crash",
	})
	f.c.SetEventJournal(journal)
	if crashBefore == "" {
		if err != nil {
			t.Fatalf("integrate: %v", err)
		}
	} else if !isAttemptWorkspaceApplyIncomplete(err) {
		t.Fatalf("integrate error = %v, want the injected crash to leave the apply incomplete", err)
	}
	simulateRestart(world)
	return f.c.todoItemByID(item.ID), world
}

func TestReconcileAttemptWorldsFinishesInterruptedApplies(t *testing.T) {
	tests := []struct {
		name string
		// crashBefore is the lifecycle event the crash prevented.
		crashBefore EventType
		// revert undoes part of the apply, as a crash between two writes would.
		revert bool
	}{
		{name: "crash mid-apply", crashBefore: EventAttemptWorkspaceApplied, revert: true},
		{name: "crash after apply before applied event", crashBefore: EventAttemptWorkspaceApplied},
		{name: "crash after applied before TaskDone"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			workerRuns := 0
			f := newIsolatedTeamFixture(t, func(c *Coordinator) fantasy.Agent {
				return &isolatedEditAgent{c: c, edit: func(string, string, int) error { workerRuns++; return nil }}
			})
			f.write(t, "a.txt", "a before\n")
			f.write(t, "b.txt", "b before\n")
			item, world := f.interruptedApply(t, "interrupted apply", tt.crashBefore)
			if tt.revert {
				f.write(t, "b.txt", "b before\n")
			}
			if _, err := os.Stat(world.worldDir()); err != nil {
				t.Fatalf("the interrupted world is gone before recovery: %v", err)
			}
			if err := f.c.reconcileAttemptWorlds(context.Background()); err != nil {
				t.Fatalf("reconcileAttemptWorlds: %v", err)
			}
			got := f.c.todoItemByID(item.ID)
			if got.Status != TaskDone {
				t.Fatalf("status = %s (%s), want done", got.Status, got.Detail)
			}
			if got.Output != "applied before the crash" || got.TypedResult == nil || got.TypedResult.Summary != "applied before the crash" {
				t.Fatalf("recovered completion = output %q, result %+v", got.Output, got.TypedResult)
			}
			if got.VerifyResult == nil || got.VerifyResult.Command != "true" {
				t.Fatalf("recovered verification = %+v", got.VerifyResult)
			}
			for rel, want := range map[string]string{"a.txt": "a after\n", "b.txt": "b after\n"} {
				if content := f.read(t, rel); content != want {
					t.Fatalf("%s = %q, want %q", rel, content, want)
				}
			}
			if workerRuns != 0 {
				t.Fatalf("recovery re-ran the worker %d time(s)", workerRuns)
			}
			applied := 0
			for _, eventType := range f.eventTypes(t) {
				if eventType == string(EventAttemptWorkspaceApplied) {
					applied++
				}
			}
			if applied != 1 {
				t.Fatalf("applied events = %d, want exactly 1 (events %v)", applied, f.eventTypes(t))
			}
			if dirs := f.worldDirs(t); len(dirs) != 0 {
				t.Fatalf("attempt worlds left after recovery: %v", dirs)
			}
			// A second pass finds nothing left to do.
			if err := f.c.reconcileAttemptWorlds(context.Background()); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestReconcileAttemptWorldsBlocksAnApplyItCannotProve(t *testing.T) {
	f := newIsolatedTeamFixture(t, nil)
	f.write(t, "a.txt", "a before\n")
	f.write(t, "b.txt", "b before\n")
	item, world := f.interruptedApply(t, "conflicting recovery", EventAttemptWorkspaceApplied)
	// Someone else changed a path after the crash: the apply can no longer
	// be proven, so recovery must not guess.
	f.write(t, "a.txt", "someone else\n")
	if err := f.c.reconcileAttemptWorlds(context.Background()); err != nil {
		t.Fatalf("reconcileAttemptWorlds: %v", err)
	}
	got := f.c.todoItemByID(item.ID)
	if got.Status != TaskBlocked {
		t.Fatalf("status = %s (%s), want blocked", got.Status, got.Detail)
	}
	if _, err := os.Stat(world.worldDir()); err != nil {
		t.Fatalf("the world was not kept for review: %v", err)
	}
	if content := f.read(t, "a.txt"); content != "someone else\n" {
		t.Fatalf("recovery overwrote a conflicting path: %q", content)
	}
}

func TestReconcileAttemptWorldsRemovesOnlyOrphans(t *testing.T) {
	f := newIsolatedTeamFixture(t, nil)
	ctx := context.Background()

	// A crash mid-attempt leaves a prepared world whose task never finished.
	_, midAttempt := f.startedIsolatedTask(t, "crashed mid-attempt")
	simulateRestart(midAttempt)
	// A protocol-incomplete task's latest world is kept for its repair.
	_, repairWorld := f.crashedProtocolIncompleteTask(t, "awaiting repair")
	// A world still in use by this process is never touched.
	_, live := f.startedIsolatedTask(t, "still running")
	// A world whose prepared event was never written.
	unrecordedDir := filepath.Join(attemptWorldsDir(f.control), "aw-0000000000000001")
	if err := os.MkdirAll(filepath.Join(unrecordedDir, attemptWorldRootDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeAttemptWorldOwner(unrecordedDir, attemptWorldOwner{Format: attemptWorldFormat, WorldID: "aw-0000000000000001", TaskID: "99", SourceRoot: f.subject, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	// A directory without an owner marker is not Hufu's.
	strangerDir := filepath.Join(attemptWorldsDir(f.control), "not-a-world")
	if err := os.MkdirAll(strangerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A world another session branch left mid-apply.
	foreignDir := filepath.Join(attemptWorldsDir(f.control), "aw-0000000000000002")
	if err := os.MkdirAll(filepath.Join(foreignDir, attemptWorldRootDir), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := writeAttemptWorldOwner(foreignDir, attemptWorldOwner{Format: attemptWorldFormat, WorldID: "aw-0000000000000002", TaskID: "98", SourceRoot: f.subject, CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	for _, eventType := range []EventType{EventAttemptWorkspacePrepared, EventAttemptWorkspaceApplyStarted} {
		payload, _ := json.Marshal(map[string]any{"world_id": "aw-0000000000000002"})
		if _, err := f.store.AppendPersistedContext(ctx, RunEvent{Type: string(eventType), BranchID: "experiment", TaskID: "98", Actor: attemptWorldEventActor, Payload: payload}); err != nil {
			t.Fatal(err)
		}
	}

	if err := f.c.reconcileAttemptWorlds(ctx); err != nil {
		t.Fatalf("reconcileAttemptWorlds: %v", err)
	}
	exists := func(dir string) bool {
		_, err := os.Stat(dir)
		return err == nil
	}
	for name, tc := range map[string]struct {
		dir  string
		want bool
	}{
		"mid-attempt orphan":      {midAttempt.worldDir(), false},
		"unrecorded world":        {unrecordedDir, false},
		"protocol repair world":   {repairWorld.worldDir(), true},
		"live world":              {live.worldDir(), true},
		"directory without owner": {strangerDir, true},
		"foreign branch world":    {foreignDir, true},
	} {
		if got := exists(tc.dir); got != tc.want {
			t.Errorf("%s exists = %v, want %v", name, got, tc.want)
		}
	}
	if got := f.c.Metrics().AttemptWorldsOrphanRemoved; got != 2 {
		t.Fatalf("attempt_worlds_orphan_removed = %d, want 2", got)
	}
	f.c.closeIsolatedAttempt(ctx, live, "test_exit")
}

func TestIsolatedAttemptMetrics(t *testing.T) {
	overlap := newBarrier(2)
	f := newIsolatedTeamFixture(t, func(c *Coordinator) fantasy.Agent {
		return &isolatedEditAgent{c: c, edit: func(root, goal string, attempt int) error {
			if attempt == 1 {
				if err := overlap.wait(); err != nil {
					return err
				}
			}
			existing, err := os.ReadFile(filepath.Join(root, "log.txt"))
			if err != nil {
				return err
			}
			return writeWorldFile(root, "log.txt", string(existing)+goal+"\n")
		}}
	})
	f.write(t, "log.txt", "base\n")
	if _, err := f.c.ExecuteTasks(context.Background(), []TaskDef{{Agent: "worker", Goal: "left"}, {Agent: "worker", Goal: "right"}}); err != nil {
		t.Fatalf("ExecuteTasks: %v", err)
	}
	metrics := f.c.Metrics()
	if metrics.IsolatedAttemptsTotal != 3 || metrics.AttemptWorkspaceConflictsTotal != 1 || metrics.AttemptWorldsOrphanRemoved != 0 {
		t.Fatalf("isolation metrics = attempts %d, conflicts %d, orphans %d; want 3, 1, 0",
			metrics.IsolatedAttemptsTotal, metrics.AttemptWorkspaceConflictsTotal, metrics.AttemptWorldsOrphanRemoved)
	}
	if !slices.Contains(f.eventTypes(t), string(EventAttemptWorkspaceApplyConflicted)) {
		t.Fatal("no conflict was recorded")
	}
}

func TestPublicEntryPointReconcilesAttemptWorldsBeforeDispatch(t *testing.T) {
	f := newIsolatedTeamFixture(t, func(c *Coordinator) fantasy.Agent {
		return &isolatedEditAgent{c: c, edit: func(root, _ string, _ int) error {
			return writeWorldFile(root, "next.txt", "next\n")
		}}
	})
	f.write(t, "a.txt", "a before\n")
	f.write(t, "b.txt", "b before\n")
	item, _ := f.interruptedApply(t, "applied before restart", "")
	if _, err := f.c.RunDirectAgent(context.Background(), "worker", "the next direct task"); err != nil {
		t.Fatalf("RunDirectAgent: %v", err)
	}
	if got := f.c.todoItemByID(item.ID); got.Status != TaskDone {
		t.Fatalf("interrupted task status = %s (%s), want done before the next dispatch", got.Status, got.Detail)
	}
	if got := f.read(t, "next.txt"); got != "next\n" {
		t.Fatalf("next.txt = %q", got)
	}
	if dirs := f.worldDirs(t); len(dirs) != 0 {
		t.Fatalf("attempt worlds left: %v", dirs)
	}
}

func TestIsolatedTaskBlocksWhenItsWorldCannotBePrepared(t *testing.T) {
	workerRuns := 0
	f := newIsolatedTeamFixture(t, func(c *Coordinator) fantasy.Agent {
		return &isolatedEditAgent{c: c, edit: func(string, string, int) error { workerRuns++; return nil }}
	})
	f.write(t, "a.txt", "a\n")
	// A symlink out of the project cannot be represented in a private copy.
	if err := os.Symlink(t.TempDir(), filepath.Join(f.subject, "escape")); err != nil {
		t.Fatal(err)
	}
	const goal = "cannot isolate"
	if _, err := f.c.ExecuteTasks(context.Background(), []TaskDef{{Agent: "worker", Goal: goal}}); err == nil {
		t.Fatal("ExecuteTasks succeeded without an attempt world")
	}
	item := f.item(t, goal)
	if item.Status != TaskBlocked || item.FailureEvent == nil || item.FailureEvent.FailureClass != FailureEnvironment || item.FailureEvent.RetryDisposition != NeedsHuman {
		t.Fatalf("task = status %s, failure %+v; want blocked environment/needs_human", item.Status, item.FailureEvent)
	}
	if workerRuns != 0 {
		t.Fatalf("the worker ran %d time(s) without an attempt world", workerRuns)
	}
	if dirs := f.worldDirs(t); len(dirs) != 0 {
		t.Fatalf("a failed prepare left worlds: %v", dirs)
	}
}
