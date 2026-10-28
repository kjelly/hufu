package team

import (
	"context"
	"errors"
	"testing"

	"github.com/kjelly/hufu/internal/workspace/versionstore"
)

func TestVersionedResumeActivation(t *testing.T) {
	for _, tc := range []struct {
		name       string
		sameBranch bool
		prepare    func(*testing.T, *versionFixture, *WorkspaceVersionContext)
		wantErr    bool
		stale      bool
	}{
		{name: "switch files and projection"},
		{name: "same branch preserves live drift", sameBranch: true},
		{name: "missing project lock", wantErr: true, prepare: func(_ *testing.T, _ *versionFixture, v *WorkspaceVersionContext) { v.HoldsProjectLock = false }},
		{name: "foreign control root", wantErr: true, prepare: func(t *testing.T, _ *versionFixture, v *WorkspaceVersionContext) { v.ControlRoot = t.TempDir() }},
		{name: "source head changed", wantErr: true, stale: true, prepare: func(_ *testing.T, f *versionFixture, _ *WorkspaceVersionContext) { f.event("b", "test_marker") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := resumeVersionFixture(t)
			target := "main"
			if tc.sameBranch {
				target = "b"
				f.write("version.txt", "live drift")
				checkpoint := LoadSession(f.control)
				checkpoint.ContinuationCheckpoint = &ContinuationCheckpoint{TurnCount: 1, MaxTurns: 3, Status: "pending", Reason: "continue after accepted work"}
				if err := SaveSession(f.control, checkpoint); err != nil {
					t.Fatal(err)
				}
			}
			source, err := ReadResumeSource(f.control, target, "")
			if err != nil {
				t.Fatal(err)
			}
			version := f.version
			if tc.prepare != nil {
				tc.prepare(t, f, &version)
			}
			before := snapshotRuntimeState(t, f.control)
			beforeEvents := len(mustReadEvents(t, f.events))
			err = ActivateVersionedResumeSource(t.Context(), f.control, source, version)
			if (err != nil) != tc.wantErr || (tc.stale && !errors.Is(err, ErrStaleResumeSource)) {
				t.Fatalf("activation error=%v", err)
			}
			if tc.wantErr || tc.sameBranch {
				if !before.equal(snapshotRuntimeState(t, f.control)) || len(mustReadEvents(t, f.events)) != beforeEvents {
					t.Fatal("refusal or same-branch activation changed durable state")
				}
				want := "B"
				if tc.sameBranch {
					want = "live drift"
				}
				if got := f.read("version.txt"); got != want {
					t.Fatalf("live files = %q, want %q", got, want)
				}
				return
			}
			tree, err := LoadSessionTree(f.control)
			if err != nil || tree.ActiveBranch != "main" || f.read("version.txt") != "A" {
				t.Fatalf("branch/files mismatch after resume: tree=%+v err=%v", tree, err)
			}
			state, ok, err := f.store.GetSubjectState(t.Context(), f.subject)
			if err != nil || !ok || state.MaterializedBranchID != "main" || state.MaterializedSnapshotID != f.head("main").ID {
				t.Fatalf("materialized head mismatch: state=%+v err=%v", state, err)
			}
			fresh, err := ReadResumeSource(f.control, "main", "")
			if err != nil || len(fresh.Session.Tasks) != 1 || fresh.Session.Tasks[0].ID != "1" {
				t.Fatalf("canonical task changed: source=%+v err=%v", fresh, err)
			}
			if err := ActivateVersionedResumeSource(t.Context(), f.control, fresh, version); err != nil {
				t.Fatalf("repeated activation: %v", err)
			}
		})
	}
}

func resumeVersionFixture(t *testing.T) *versionFixture {
	t.Helper()
	f := newVersionFixture(t)
	f.write("version.txt", "A")
	appendBranchEvent(t, f.events, RunEvent{BranchID: "main", Type: string(EventTaskCompleted), TaskID: "1", Actor: "worker", Payload: []byte(`{"id":"1","agent":"worker","desc":"accepted work","status":"done"}`)})
	mustFork(t, f, "b", "")
	f.write("version.txt", "B")
	if _, _, err := f.ops().SnapshotNow(t.Context()); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestVersionedResumeActivationRecoversInterruptedCheckout(t *testing.T) {
	f := resumeVersionFixture(t)
	source, err := ReadResumeSource(f.control, "main", "")
	if err != nil {
		t.Fatal(err)
	}
	oldHook := workspaceVersionStageHook
	t.Cleanup(func() { workspaceVersionStageHook = oldHook })
	injected := errors.New("crash after materialization")
	workspaceVersionStageHook = func(stage string) error {
		if stage == "checkout:materialized" {
			return injected
		}
		return nil
	}
	if err := ActivateVersionedResumeSource(t.Context(), f.control, source, f.version); !errors.Is(err, injected) {
		t.Fatalf("fault was not reached: %v", err)
	}
	workspaceVersionStageHook = oldHook
	f.reopen()
	source, err = ReadResumeSource(f.control, "main", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := ActivateVersionedResumeSource(t.Context(), f.control, source, f.version); !errors.Is(err, ErrStaleResumeSource) {
		t.Fatalf("forward recovery should require fresh selection: %v", err)
	}
	source, err = ReadResumeSource(f.control, "main", "")
	if err != nil {
		t.Fatal(err)
	}
	if err := ActivateVersionedResumeSource(t.Context(), f.control, source, f.version); err != nil {
		t.Fatal(err)
	}
	state, ok, err := f.store.GetSubjectState(t.Context(), f.subject)
	if err != nil || !ok || state.MaterializedBranchID != "main" || f.read("version.txt") != "A" {
		t.Fatalf("forward recovery diverged: state=%+v err=%v", state, err)
	}
	if f.head("b").Reason != versionstore.SnapshotManual {
		t.Fatal("recovery recaptured A files onto outgoing b")
	}
}

func TestVersionedResumeActivationRefusesMissingTargetSnapshot(t *testing.T) {
	f := resumeVersionFixture(t)
	branch, err := f.tree.CreateRootBranch("legacy")
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveSessionTree(f.control, f.tree); err != nil {
		t.Fatal(err)
	}
	appendBranchEvent(t, f.events, RunEvent{BranchID: branch.ID, Type: string(EventTaskCompleted), TaskID: "2", Actor: "worker", Payload: []byte(`{"id":"2","agent":"worker","desc":"legacy work","status":"done"}`)})
	source, err := ReadResumeSource(f.control, branch.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	before := snapshotRuntimeState(t, f.control)
	if err := ActivateVersionedResumeSource(t.Context(), f.control, source, f.version); !errors.Is(err, versionstore.ErrWorkspaceSnapshotUnavailable) {
		t.Fatalf("missing snapshot activation: %v", err)
	}
	if !before.equal(snapshotRuntimeState(t, f.control)) || f.read("version.txt") != "B" {
		t.Fatal("missing snapshot changed branch or files")
	}
}

func TestVersionedResumeActivationHonorsCancellation(t *testing.T) {
	f := resumeVersionFixture(t)
	source, err := ReadResumeSource(f.control, "main", "")
	if err != nil {
		t.Fatal(err)
	}
	before := snapshotRuntimeState(t, f.control)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if err := ActivateVersionedResumeSource(ctx, f.control, source, f.version); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled activation: %v", err)
	}
	if !before.equal(snapshotRuntimeState(t, f.control)) || f.read("version.txt") != "B" {
		t.Fatal("cancelled activation changed state")
	}
}
