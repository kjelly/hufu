package team

import (
	"errors"
	"testing"

	"github.com/kjelly/hufu/internal/workspace/versionstore"
)

func TestResumeActivationPlanIsReadOnlyAndPinsTargetGeneration(t *testing.T) {
	f := resumeVersionFixture(t)
	source, err := ReadResumeSource(f.control, "main", "")
	if err != nil {
		t.Fatal(err)
	}
	before := snapshotRuntimeState(t, f.control)
	state, _, err := f.store.GetSubjectState(t.Context(), f.subject)
	if err != nil {
		t.Fatal(err)
	}
	head := f.head("main")
	plan, err := PlanResumeActivation(t.Context(), f.control, source, f.version)
	if err != nil || plan.MetadataOnly || plan.TargetSnapshotID != head.ID || plan.TargetTreeHash != head.RootTreeHash || plan.SnapshotEventID != head.CommitEventID || plan.CaptureEventID == "" {
		t.Fatalf("plan=%+v err=%v", plan, err)
	}
	after, _, err := f.store.GetSubjectState(t.Context(), f.subject)
	if err != nil || state != after || !before.equal(snapshotRuntimeState(t, f.control)) {
		t.Fatalf("planning changed durable state: %v", err)
	}
	// Doctor/head repair can change generation without changing the event ID
	// or snapshot. A stale preview cannot authorize a later activation.
	if err := f.store.RepairBranchHead(t.Context(), f.version.WorkspaceID, "main", head.ID); err != nil {
		t.Fatal(err)
	}
	if err := ActivateResumePlan(t.Context(), f.control, source, f.version, plan); !errors.Is(err, ErrStaleResumeSource) {
		t.Fatalf("stale target generation accepted: %v", err)
	}
	if !before.equal(snapshotRuntimeState(t, f.control)) || f.read("version.txt") != "B" {
		t.Fatal("stale plan activated branch/files")
	}
	plan, err = PlanResumeActivation(t.Context(), f.control, source, f.version)
	if err != nil {
		t.Fatal(err)
	}
	if err := ActivateResumePlan(t.Context(), f.control, source, f.version, plan); err != nil {
		t.Fatal(err)
	}
}

func TestResumeActivationRefusesForeignMaterializedWorkspace(t *testing.T) {
	f := resumeVersionFixture(t)
	if err := f.store.UpdateSubjectState(t.Context(), f.subject, func(state *versionstore.SubjectState) { state.MaterializedWorkspaceID = "another-workspace" }); err != nil {
		t.Fatal(err)
	}
	source, err := ReadResumeSource(f.control, "main", "")
	if err != nil {
		t.Fatal(err)
	}
	before := snapshotRuntimeState(t, f.control)
	if err := ActivateVersionedResumeSource(t.Context(), f.control, source, f.version); !errors.Is(err, versionstore.ErrWorkspaceRecoveryRequired) {
		t.Fatalf("foreign materialized workspace: %v", err)
	}
	if !before.equal(snapshotRuntimeState(t, f.control)) || f.read("version.txt") != "B" {
		t.Fatal("foreign materialized files were captured as this workspace's branch")
	}
}

func TestResumeActivationRefusesExistingMetadataFileMismatch(t *testing.T) {
	f := resumeVersionFixture(t)
	// Simulate a workspace left by the old metadata-only resume path.
	if _, _, err := CheckoutSessionBranch(t.Context(), f.control, f.tree, f.events, "main"); err != nil {
		t.Fatal(err)
	}
	source, err := ReadResumeSource(f.control, "main", "")
	if err != nil {
		t.Fatal(err)
	}
	before := snapshotRuntimeState(t, f.control)
	if err := ActivateVersionedResumeSource(t.Context(), f.control, source, f.version); !errors.Is(err, versionstore.ErrWorkspaceRecoveryRequired) {
		t.Fatalf("existing branch/files mismatch: %v", err)
	}
	if !before.equal(snapshotRuntimeState(t, f.control)) || f.read("version.txt") != "B" {
		t.Fatal("existing mismatch was recorded as new main drift")
	}
}
