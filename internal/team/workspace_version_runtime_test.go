package team

import (
	"context"
	"database/sql"
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
	c.markWorkspaceUnauthorizedMutation(AttemptRequest{RunID: "run-1", TaskID: "t1", Attempt: 2}, delta, errors.New("outside every authorized writable root"), nil)
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

func TestCodexRecoveryWriteFailureKeepsCheckpointProhibited(t *testing.T) {
	for _, code := range []string{"unauthorized_mutation", "provider_process_unsettled", "provider_effects_unverified"} {
		t.Run(code, func(t *testing.T) {
			if code == "provider_effects_unverified" {
				skipUnsupportedCodexIntegration(t)
			}
			ctx := t.Context()
			f := newVersionFixture(t)
			c := runtimeCoordinator(t, f)
			if err := c.admitWorkspaceVersion(ctx); err != nil {
				t.Fatal(err)
			}
			before := f.head("main")
			f.write("unchecked.txt", "unknown provider effects")
			dsn := filepath.Join(versionstore.StorageRoot(f.version.StateDir), "workspace_versions.db")
			db, err := sql.Open("sqlite", dsn)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = db.Close() })
			if _, err := db.ExecContext(ctx, `CREATE TRIGGER reject_recovery_marker BEFORE UPDATE ON subject_state
				WHEN NEW.recovery_required = 1 BEGIN SELECT RAISE(ABORT, 'fixture recovery write failure'); END`); err != nil {
				t.Fatal(err)
			}
			if _, err := db.ExecContext(ctx, "UPDATE subject_state SET recovery_required=1"); err == nil {
				t.Fatal("fixture did not block SQLite recovery writes")
			}
			request := AttemptRequest{RunID: "run-runtime", TaskID: "affected-task", Attempt: 1}
			switch code {
			case "unauthorized_mutation":
				delta := WorkspaceDelta{Added: []WorkspaceFileState{{Path: "unchecked.txt"}}}
				c.markWorkspaceUnauthorizedMutation(request, delta, errors.New("outside authorized roots"), nil)
			case "provider_process_unsettled":
				leaf := newTestCoordinator(t)
				leaf.session.WorkspaceVersion = f.version
				leaf.modelFanout = &modelFanoutBinding{parent: c}
				leaf.markWorkspaceProviderUnsettled(request)
			default:
				provider := NewCodexSubagentProvider(c, "codex", agent.SubagentProviderConfig{})
				world := &codexStartupSnapshotProbe{}
				result, err := provider.settleFailedCodexWorkspace(ctx, request, world, &PreparedExecutionWorld{},
					AttemptResult{}, errors.New("disconnected"), nil)
				failure, failed := errors.AsType[*CodexProviderError](err)
				if !failed || failure.Class != CodexFailureWorkspaceViolation || world.calls != 1 || len(result.WorkspaceDelta.Added) != 0 {
					t.Fatalf("snapshot failure reached settlement as %+v, %v", result, err)
				}
			}
			var sqliteRecovery bool
			if err := db.QueryRowContext(ctx, "SELECT recovery_required FROM subject_state").Scan(&sqliteRecovery); err != nil {
				t.Fatal(err)
			}
			if state := subjectState(t, f); sqliteRecovery || !state.RecoveryRequired || state.RecoveryCode != code || c.pendingWorkspaceRecovery != nil {
				t.Fatalf("recovery did not persist independently: state %+v", state)
			}
			terminalCtx, cancel := context.WithCancel(ctx)
			cancel()
			c.checkpointWorkspaceVersion(terminalCtx, &RunResult{})
			if head := f.head("main"); head.ID != before.ID {
				t.Fatalf("checkpoint advanced while recovery storage was failing: %+v", head)
			}
			if err := c.eventStore.Close(); err != nil {
				t.Fatal(err)
			}
			f.reopen()
			restarted := runtimeCoordinator(t, f)
			if err := restarted.admitWorkspaceVersion(ctx); !errors.Is(err, versionstore.ErrWorkspaceRecoveryRequired) {
				t.Fatalf("cold admission ignored the durable recovery record: %v", err)
			}
			state := subjectState(t, f)
			if head := f.head("main"); head.ID != before.ID || !state.RecoveryRequired || state.RecoveryCode != code {
				t.Fatalf("reopened head %s (before %s), recovery state %+v", head.ID, before.ID, state)
			}
			if _, err := db.ExecContext(ctx, "DROP TRIGGER reject_recovery_marker"); err != nil {
				t.Fatal(err)
			}
			restarted.markWorkspaceProviderRecovery(f.version, "provider_effects_unverified", "latest provider disposition")
			f.reopen()
			if err := runtimeCoordinator(t, f).admitWorkspaceVersion(ctx); !errors.Is(err, versionstore.ErrWorkspaceRecoveryRequired) ||
				!strings.Contains(err.Error(), "latest provider disposition") {
				t.Fatalf("cold admission did not report the latest disposition: %v", err)
			}
			issues, err := f.store.Doctor(ctx)
			if err != nil {
				t.Fatal(err)
			}
			var recoveryIssues []string
			for _, issue := range issues {
				if issue.Code == "recovery_required" {
					recoveryIssues = append(recoveryIssues, issue.Detail)
				}
			}
			if len(recoveryIssues) != 1 || !strings.Contains(recoveryIssues[0], "latest provider disposition") || f.head("main").ID != before.ID {
				t.Fatalf("doctor recovery and original head: %v", recoveryIssues)
			}
			if _, _, err := f.ops().Restore(ctx, before.ID); err != nil {
				t.Fatal(err)
			}
			f.reopen()
			if state := subjectState(t, f); state.RecoveryRequired {
				t.Fatalf("explicit restore retained recovery state: %+v", state)
			}
			if err := restarted.admitWorkspaceVersion(ctx); err != nil {
				t.Fatalf("admission after restore: %v", err)
			}
		})
	}
}

func TestCodexUnauthorizedMutationRedactsRecoveryDetail(t *testing.T) {
	skipUnsupportedCodexIntegration(t)
	const credential = "opaque-workspace-mutation-68001"
	f := newVersionFixture(t)
	turn := fakeCodexTurnCompletedStep(t, "work", validProposalJSON(""))
	turn.WriteFile = "outside-" + credential + ".txt"
	turn.Notifications = append([]fakeCodexNotification{{
		Method: "error", Params: rawJSON(t, map[string]string{"api_key": credential}),
	}}, turn.Notifications...)
	c, provider, item, _ := newCodexHarness(t, f.subject, []fakeCodexStep{
		codexInitializeStep(t), fakeCodexThreadStartStep("read-only-thread", f.subject, "gpt-5-codex", codexSandboxReadOnly, t), turn,
	})
	c.session.WorkspaceVersion = f.version
	request := codexAttemptRequest(item, "inspect only")
	request.Task.SideEffect = SideEffectNone
	result, err := provider.RunAttempt(t.Context(), request)
	failure, ok := errors.AsType[*CodexProviderError](err)
	if !ok || failure.Class != CodexFailureWorkspaceViolation || result.CanonicalResult != nil {
		t.Fatalf("read-only violation did not block settlement: %v", err)
	}
	if result.WorkspaceDelta.Added[0].Path != turn.WriteFile {
		t.Fatal("workspace validation lost the original path")
	}
	_, transcript := readCodexTranscriptArtifact(t, f.subject, result.TranscriptRef)
	f.reopen()
	state := subjectState(t, f)
	if !state.RecoveryRequired || state.RecoveryCode != "unauthorized_mutation" || state.Generation != 0 {
		t.Fatal("reopened version store lost the recovery disposition")
	}
	issues, doctorErr := f.store.Doctor(t.Context())
	if doctorErr != nil || len(issues) != 1 || issues[0].Code != "recovery_required" {
		t.Fatalf("doctor did not report recovery before SQLite subject registration: %v, %v", issues, doctorErr)
	}
	for sink, detail := range map[string]string{
		"error": err.Error(), "sealed transcript": string(transcript),
		"reopened recovery marker": state.RecoveryDetail, "doctor": issues[0].Detail,
	} {
		if strings.Contains(detail, credential) || !strings.Contains(detail, "outside-[REDACTED].txt") {
			t.Fatalf("%s did not retain the redacted path", sink)
		}
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
