package inspect

import (
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func TestEvidenceProjectionRetainsResolutionWithoutAliasing(t *testing.T) {
	manifest := &team.EvidenceManifest{EvidenceResults: []team.EvidenceResult{{
		RequirementID: "task:1", Status: "passed", Validator: "task-resolution",
		Resolution: &team.EvidenceResolution{Status: "superseded", OriginalStatus: "blocked", ResolvedBy: "2"},
		Binding:    &team.EvidenceBinding{TaskID: "2"},
	}}}
	var data EvidenceData
	projectEvidenceManifest(&data, manifest)
	if len(data.Requirements) != 1 || data.Requirements[0].Resolution == nil || data.Requirements[0].Resolution.OriginalStatus != "blocked" || data.Requirements[0].Binding.TaskID != "2" {
		t.Fatalf("projection lost resolution: %#v", data.Requirements)
	}
	manifest.EvidenceResults[0].Resolution.ResolvedBy = "changed"
	if data.Requirements[0].Resolution.ResolvedBy != "2" {
		t.Fatal("projection aliases the mutable manifest resolution")
	}
}
