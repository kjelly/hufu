package team

import (
	"context"
	"testing"

	"github.com/kjelly/hufu/internal/workspace/versionstore"
)

func issueCodes(report WorkspaceDoctorReport) map[string]int {
	codes := make(map[string]int)
	for _, issue := range report.Issues {
		codes[issue.Code]++
	}
	return codes
}

func TestDoctorWorkspaceChecksLineageAndRepairsHeads(t *testing.T) {
	ctx := context.Background()
	f := newVersionFixture(t)
	f.write("a.txt", "v1")
	first, _, err := f.ops().SnapshotNow(ctx)
	if err != nil {
		t.Fatal(err)
	}
	f.write("a.txt", "v2")
	second, _, err := f.ops().SnapshotNow(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.tree.CreateBranch("legacy", "", f.events); err != nil {
		t.Fatal(err)
	}
	report, err := DoctorWorkspace(ctx, f.ops(), false)
	if err != nil {
		t.Fatal(err)
	}
	if report.HasErrors() || issueCodes(report)["legacy_branch"] != 1 {
		t.Fatalf("clean report = %#v", report)
	}

	// Simulate a head that no longer matches the canonical event lineage.
	if err = f.store.RepairBranchHead(ctx, f.version.WorkspaceID, "main", first.ID); err != nil {
		t.Fatal(err)
	}
	f.write("a.txt", "drifted")
	report, err = DoctorWorkspace(ctx, f.ops(), false)
	if err != nil {
		t.Fatal(err)
	}
	codes := issueCodes(report)
	if codes["head_lineage_mismatch"] != 1 || codes["live_drift"] != 1 || !report.HasErrors() {
		t.Fatalf("report = %#v", report)
	}
	f.write("a.txt", "v2")
	report, err = DoctorWorkspace(ctx, f.ops(), true)
	if err != nil {
		t.Fatal(err)
	}
	if report.HasErrors() || len(report.Repaired) == 0 || f.head("main").ID != second.ID {
		t.Fatalf("repair report = %#v, head = %s", report, f.head("main").ID)
	}
}

func TestDoctorWorkspaceReportsMissingCommitEvents(t *testing.T) {
	ctx := context.Background()
	f := newVersionFixture(t)
	f.write("a.txt", "v1")
	if _, _, err := f.ops().SnapshotNow(ctx); err != nil {
		t.Fatal(err)
	}
	// A different control workspace (fresh event log) sees the same store.
	other := newVersionFixture(t)
	other.closeStores()
	other.subject, other.version.SubjectRoot, other.version.StateDir = f.subject, f.subject, f.version.StateDir
	other.version.WorkspaceID = f.version.WorkspaceID
	other.reopen()
	report, err := DoctorWorkspace(ctx, other.ops(), false)
	if err != nil {
		t.Fatal(err)
	}
	if issueCodes(report)["commit_event_missing"] != 1 {
		t.Fatalf("report = %#v", report)
	}
}

func TestWorkspaceStatusAndPinnedLabels(t *testing.T) {
	ctx := context.Background()
	f := newVersionFixture(t)
	f.write("a.txt", "v1")
	first, _, err := f.ops().SnapshotNow(ctx)
	if err != nil {
		t.Fatal(err)
	}
	marker := f.event("main", "test_marker")
	if err = f.tree.AddLabel("before-v2", marker); err != nil {
		t.Fatal(err)
	}
	f.write("a.txt", "v2")
	if _, _, err = f.ops().SnapshotNow(ctx); err != nil {
		t.Fatal(err)
	}
	pinned, err := PinnedWorkspaceSnapshots(f.ops())
	if err != nil || len(pinned) != 1 || pinned[0] != first.ID {
		t.Fatalf("pinned = %v, %v; want [%s]", pinned, err, first.ID)
	}
	status, err := f.ops().Status(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if status.ActiveBranchID != "main" || status.ModeFloor != "required" || status.SnapshotCounts["published"] != 2 ||
		status.LiveDrift == nil || *status.LiveDrift || status.CASObjects == 0 || status.Commits[string(versionstore.SnapshotBaseline)] != 1 {
		t.Fatalf("status = %#v", status)
	}
	f.write("a.txt", "live edit")
	status, err = f.ops().Status(ctx)
	if err != nil || status.LiveDrift == nil || !*status.LiveDrift {
		t.Fatalf("drifted status = %#v, %v", status, err)
	}
}
