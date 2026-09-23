package team

import (
	"context"
	"errors"
	"log"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/workspace/versionstore"
)

// runtimeCoordinator is a coordinator bound to f's control workspace, with
// its own event store like a real process.
func runtimeCoordinator(t *testing.T, f *versionFixture) *Coordinator {
	t.Helper()
	c := &Coordinator{
		session:        &TeamSession{Workspace: f.control, Config: agent.TeamConfig{Name: "team"}, WorkspaceVersion: f.version},
		executionRunID: "run-runtime",
	}
	c.initEventStore()
	if c.eventStore == nil {
		t.Fatal("coordinator event store was not initialized")
	}
	t.Cleanup(func() { _ = c.eventStore.Close() })
	return c
}

func snapshotCount(t *testing.T, f *versionFixture) int {
	t.Helper()
	snapshots, err := f.store.ListSnapshots(context.Background(), versionstore.SnapshotFilter{WorkspaceID: f.version.WorkspaceID})
	if err != nil {
		t.Fatal(err)
	}
	return len(snapshots)
}

func subjectState(t *testing.T, f *versionFixture) versionstore.SubjectState {
	t.Helper()
	state, _, err := f.store.GetSubjectState(context.Background(), f.subject)
	if err != nil {
		t.Fatal(err)
	}
	return state
}

// TestAdmitRunRecordsBaselineAndExternalDrift covers §22.1 and IT6: the
// first admission records a baseline, a user edit between runs becomes
// external drift (never overwritten), and an unchanged tree adds nothing.
func TestAdmitRunRecordsBaselineAndExternalDrift(t *testing.T) {
	ctx := context.Background()
	f := newVersionFixture(t)
	f.write("a.go", "v1")
	c := runtimeCoordinator(t, f)
	if err := c.admitWorkspaceVersion(ctx); err != nil {
		t.Fatal(err)
	}
	if head := f.head("main"); head.Reason != versionstore.SnapshotBaseline {
		t.Fatalf("first admission head = %#v", head)
	}
	f.write("a.go", "edited by the user between runs")
	if err := c.admitWorkspaceVersion(ctx); err != nil {
		t.Fatal(err)
	}
	head := f.head("main")
	if head.Reason != versionstore.SnapshotExternalDrift || f.read("a.go") != "edited by the user between runs" {
		t.Fatalf("drift head = %#v, a.go = %q", head, f.read("a.go"))
	}
	before := snapshotCount(t, f)
	if err := c.admitWorkspaceVersion(ctx); err != nil {
		t.Fatal(err)
	}
	if snapshotCount(t, f) != before {
		t.Fatal("an unchanged admission recorded a snapshot")
	}
}

// TestCheckpointRunAndDeferral covers §22.2 and IT14: a run's changes become
// a run_checkpoint; a failed checkpoint is deferred without failing the run
// and is picked up by the next admission.
func TestCheckpointRunAndDeferral(t *testing.T) {
	ctx := context.Background()
	f := newVersionFixture(t)
	f.write("a.go", "v1")
	c := runtimeCoordinator(t, f)
	if err := c.admitWorkspaceVersion(ctx); err != nil {
		t.Fatal(err)
	}
	f.write("a.go", "written by a worker")
	result := &RunResult{}
	c.checkpointWorkspaceVersion(ctx, result)
	if head := f.head("main"); head.Reason != versionstore.SnapshotRunCheckpoint || len(result.Warnings) != 0 {
		t.Fatalf("checkpoint head = %#v warnings = %v", head, result.Warnings)
	}

	fifo := filepath.Join(f.subject, "pipe")
	if err := syscall.Mkfifo(fifo, 0o600); err != nil {
		t.Skipf("mkfifo: %v", err)
	}
	f.write("a.go", "more work")
	result = &RunResult{}
	c.checkpointWorkspaceVersion(ctx, result)
	if len(result.Warnings) != 1 || !strings.Contains(result.Warnings[0], "deferred") {
		t.Fatalf("warnings = %v", result.Warnings)
	}
	if !subjectState(t, f).CheckpointDeferred {
		t.Fatal("failed checkpoint was not marked deferred")
	}
	if err := os.Remove(fifo); err != nil {
		t.Fatal(err)
	}
	if err := c.admitWorkspaceVersion(ctx); err != nil {
		t.Fatal(err)
	}
	if head := f.head("main"); head.Reason != versionstore.SnapshotExternalDrift || subjectState(t, f).CheckpointDeferred {
		t.Fatalf("after admission head = %#v deferred = %v", head, subjectState(t, f).CheckpointDeferred)
	}
}

// TestUnauthorizedMutationBlocksRequiredAdmission covers §22.3 (IT14).
func TestUnauthorizedMutationBlocksRequiredAdmission(t *testing.T) {
	ctx := context.Background()
	f := newVersionFixture(t)
	f.write("a.go", "v1")
	c := runtimeCoordinator(t, f)
	if err := c.admitWorkspaceVersion(ctx); err != nil {
		t.Fatal(err)
	}
	f.write("outside.txt", "written outside the writable roots")
	delta := WorkspaceDelta{Added: []WorkspaceFileState{{Path: "outside.txt"}}}
	c.markWorkspaceUnauthorizedMutation(ctx, AttemptRequest{RunID: "run-1", TaskID: "t1", Attempt: 2}, delta, errors.New("outside every authorized writable root"))
	state := subjectState(t, f)
	if !state.RecoveryRequired || state.RecoveryCode != "unauthorized_mutation" || !strings.Contains(state.RecoveryDetail, "outside.txt") || !strings.Contains(state.RecoveryDetail, "attempt 2") {
		t.Fatalf("marker = %#v", state)
	}
	before := snapshotCount(t, f)
	c.checkpointWorkspaceVersion(ctx, &RunResult{})
	if snapshotCount(t, f) != before {
		t.Fatal("the violating state was recorded as a run checkpoint")
	}
	if err := c.admitWorkspaceVersion(ctx); !errors.Is(err, versionstore.ErrWorkspaceRecoveryRequired) {
		t.Fatalf("admission err = %v, want ErrWorkspaceRecoveryRequired", err)
	}
	if _, err := f.ops().Adopt(ctx); err != nil {
		t.Fatal(err)
	}
	if err := c.admitWorkspaceVersion(ctx); err != nil {
		t.Fatalf("admission after adopt: %v", err)
	}
}

// TestAdmitRunRequiresHufuignoreOutsideGit covers IT13 (D7).
func TestAdmitRunRequiresHufuignoreOutsideGit(t *testing.T) {
	ctx := context.Background()
	f := newVersionFixture(t)
	if err := os.Remove(filepath.Join(f.subject, versionstore.HufuignoreFile)); err != nil {
		t.Fatal(err)
	}
	f.write("a.go", "v1")
	c := runtimeCoordinator(t, f)
	if err := c.admitWorkspaceVersion(ctx); !errors.Is(err, versionstore.ErrHufuignoreRequired) {
		t.Fatalf("required admission err = %v, want ErrHufuignoreRequired", err)
	}
	// The required admission recorded the floor; lower it like `downgrade`.
	if err := f.store.Downgrade(ctx, f.subject, f.version.WorkspaceID, versionstore.ModeObserve); err != nil {
		t.Fatal(err)
	}
	c.session.WorkspaceVersion.Mode = versionstore.ModeObserve
	c.session.WorkspaceVersion.HoldsProjectLock = false
	var logged strings.Builder
	log.SetOutput(&logged)
	t.Cleanup(func() { log.SetOutput(os.Stderr) })
	if err := c.admitWorkspaceVersion(ctx); err != nil {
		t.Fatalf("observe admission must not block: %v", err)
	}
	if !strings.Contains(logged.String(), "has no .hufuignore") {
		t.Fatalf("observe admission did not warn about the missing .hufuignore: %q", logged.String())
	}
}

// TestAdmitRunModeGuards: required mode needs the project lock; observe mode
// skips (never blocks) when another holder has it.
func TestAdmitRunModeGuards(t *testing.T) {
	ctx := context.Background()
	f := newVersionFixture(t)
	f.write("a.go", "v1")
	c := runtimeCoordinator(t, f)
	c.session.WorkspaceVersion.HoldsProjectLock = false
	if err := c.admitWorkspaceVersion(ctx); err == nil || !strings.Contains(err.Error(), "project lock") {
		t.Fatalf("required without lock: err = %v", err)
	}
	c.session.WorkspaceVersion.Mode = versionstore.ModeObserve
	lock, err := versionstore.TryLockProject(f.version.StateDir, versionstore.LockOwner{WorkspaceID: "ws_other"})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = lock.Close() }()
	if err = c.admitWorkspaceVersion(ctx); err != nil {
		t.Fatalf("observe with a busy lock: %v", err)
	}
	if snapshotCount(t, f) != 0 {
		t.Fatal("observe admission wrote a snapshot without the lock")
	}
}

// TestAdmitRunSharedSubjectAcrossTeams covers IT10's last line (D2): another
// team's changes appear in this team's lineage as external drift.
func TestAdmitRunSharedSubjectAcrossTeams(t *testing.T) {
	ctx := context.Background()
	teamA := newVersionFixture(t)
	teamB := newVersionFixture(t)
	teamB.closeStores()
	teamB.subject, teamB.version.SubjectRoot, teamB.version.StateDir = teamA.subject, teamA.subject, teamA.version.StateDir
	teamB.version.WorkspaceID = "ws_fixture_b"
	teamB.reopen()

	a := runtimeCoordinator(t, teamA)
	b := runtimeCoordinator(t, teamB)
	teamA.write("x.go", "from team A")
	if err := a.admitWorkspaceVersion(ctx); err != nil {
		t.Fatal(err)
	}
	if err := b.admitWorkspaceVersion(ctx); err != nil {
		t.Fatal(err)
	}
	teamA.write("y.go", "team A's run")
	a.checkpointWorkspaceVersion(ctx, &RunResult{})
	if err := b.admitWorkspaceVersion(ctx); err != nil {
		t.Fatal(err)
	}
	head := teamB.head("main")
	leaves, err := teamB.store.DiffTrees(ctx, teamA.head("main").RootTreeHash, head.RootTreeHash)
	if err != nil || !leaves.Empty() || head.Reason != versionstore.SnapshotExternalDrift || head.WorkspaceID != "ws_fixture_b" {
		t.Fatalf("team B head = %#v diff = %#v err = %v", head, leaves, err)
	}
}
