package evalharness

import (
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func TestEvalEvidenceAssertionsVerifyManifestAndMembership(t *testing.T) {
	workspace := t.TempDir()
	store, err := team.NewFileArtifactStore(workspace, workspace)
	if err != nil {
		t.Fatalf("NewFileArtifactStore: %v", err)
	}
	task := &team.TodoItem{ID: "opaque-task"}
	put, err := store.Put(t.Context(), team.PutArtifactRequest{
		Kind: "task_transcript", Path: "transcript.jsonl", Content: []byte("complete"),
		RunID: "run-eval", TaskID: task.ID, Attempt: 1, Agent: "worker",
	})
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	manifest := &team.EvidenceManifest{
		RunID: "run-eval", Status: "accepted", ArtifactRefs: []team.ArtifactRef{put.ArtifactRef},
		EvidenceResults: []team.EvidenceResult{{
			RequirementID: "run:fixture", Status: "passed", ArtifactRefs: []team.ArtifactRef{put.ArtifactRef},
		}},
	}
	if err := manifest.Seal(); err != nil {
		t.Fatalf("Seal: %v", err)
	}
	expect := &EvidenceExpect{
		ManifestExists:  new(true),
		HashValid:       new(true),
		Status:          "accepted",
		MinArtifactRefs: new(1),
		RequiredResults: []EvidenceResultExpect{{
			RequirementID: "run:fixture", Status: "passed", MinArtifactRefs: new(1),
		}},
		RequiredArtifactRefs: []ArtifactRefExpect{{Kind: "task_transcript", TaskIndex: new(0), MinCount: 1}},
	}
	result := &team.RunResult{RunID: "run-eval", EvidenceManifest: manifest}
	if findings := assertEvidence(t.Context(), workspace, expect, result, []*team.TodoItem{task}); len(findings) != 0 {
		t.Fatalf("valid evidence produced findings: %+v", findings)
	}

	manifest.ManifestHash = "tampered"
	findings := assertEvidence(t.Context(), workspace, expect, result, []*team.TodoItem{task})
	if len(findings) != 1 || findings[0].Dimension != "evidence.hash-valid" {
		t.Fatalf("tampered manifest findings = %+v", findings)
	}
}

func TestEvalEvidenceAssertionsRequireConfiguredManifest(t *testing.T) {
	expect := &EvidenceExpect{HashValid: new(true)}
	findings := assertEvidence(t.Context(), t.TempDir(), expect, &team.RunResult{}, nil)
	if len(findings) != 1 || findings[0].Dimension != "evidence.manifest" {
		t.Fatalf("missing manifest findings = %+v", findings)
	}
}
