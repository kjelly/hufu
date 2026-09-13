package team

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEvidenceManifestBuildFailurePreservesPublishedManifest(t *testing.T) {
	published := &EvidenceManifest{RunID: "published", Status: "accepted"}
	c := &Coordinator{lastEvidenceManifest: published}

	if _, err := c.buildEvidenceManifest(t.Context(), true); err == nil {
		t.Fatal("buildEvidenceManifest succeeded without a task tracker")
	}
	if c.lastEvidenceManifest != published {
		t.Fatal("failed build replaced the previously published manifest")
	}
}

func TestEvidenceManifestFinalizeFailureRetainsInPlaceMutation(t *testing.T) {
	workspaceFile := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(workspaceFile, []byte("workspace blocker"), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := &EvidenceManifest{RunID: "run-finalize-failure", Status: "accepted"}
	c := &Coordinator{
		session:              &TeamSession{Workspace: workspaceFile},
		lastEvidenceManifest: manifest,
	}

	if err := c.finalizeEvidenceManifest(t.Context(), nil); err == nil {
		t.Fatal("finalizeEvidenceManifest succeeded with an invalid workspace")
	}
	if c.lastEvidenceManifest != manifest {
		t.Fatal("failed finalize replaced the manifest pointer")
	}
	if manifest.Status != "unverified" {
		t.Fatalf("failed finalize status = %q, want in-place unverified mutation", manifest.Status)
	}
	if len(manifest.EvidenceResults) != 1 || manifest.EvidenceResults[0].RequirementID != "run:acceptance" {
		t.Fatalf("failed finalize did not retain acceptance mutation: %#v", manifest.EvidenceResults)
	}
}
