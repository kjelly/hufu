package team

import (
	"testing"

	"github.com/kjelly/hufu/internal/workspace/versionstore"
)

func TestWorkspaceSnapshotProvenanceDistinguishesDerivedPublication(t *testing.T) {
	f := newVersionFixture(t)
	f.write("version.txt", "A")
	base, _, err := f.ops().SnapshotNow(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	cutoff := f.event("main", "test_marker")
	f.write("version.txt", "B")
	if _, _, err := f.ops().SnapshotNow(t.Context()); err != nil {
		t.Fatal(err)
	}
	fork := mustFork(t, f, "historical", cutoff)
	provenance, err := f.ops().SnapshotProvenance(t.Context(), fork.Snapshot)
	if err != nil || provenance.CaptureState != "known" || provenance.CaptureSnapshot != string(base.ID) || provenance.CaptureEventID != base.CommitEventID || provenance.SnapshotEventID != fork.Snapshot.CommitEventID || provenance.SnapshotReason != string(versionstore.SnapshotFork) {
		t.Fatalf("historical fork provenance=%+v err=%v", provenance, err)
	}
	if provenance.SnapshotEventID == provenance.CaptureEventID || f.read("version.txt") != "A" {
		t.Fatal("derived publication was confused with the source capture")
	}
	restored, _, err := f.ops().Restore(t.Context(), base.ID)
	if err != nil {
		t.Fatal(err)
	}
	provenance, err = f.ops().SnapshotProvenance(t.Context(), restored)
	if err != nil || provenance.CaptureState != "unavailable" || provenance.CaptureEventID != "" || provenance.SnapshotReason != string(versionstore.SnapshotRestore) {
		t.Fatalf("restore fabricated a capture origin: %+v err=%v", provenance, err)
	}
}
