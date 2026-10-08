package team

import (
	"fmt"
	"reflect"
	"strings"
)

func evidenceProducerTask(manifest EvidenceManifest, result EvidenceResult) (string, error) {
	if result.Resolution == nil {
		return strings.TrimPrefix(result.RequirementID, "task:"), nil
	}
	if err := verifyEvidenceResolution(manifest, result); err != nil {
		return "", err
	}
	return result.Resolution.ResolvedBy, nil
}

func applyEvidenceResolutions(manifest *EvidenceManifest, items []*TodoItem) {
	for _, item := range items {
		resolver := VerifiedTaskResolution(item, items, manifest.RunID)
		if resolver == nil {
			continue
		}
		original := taskManifestEvidence(manifest, item.ID)
		replacement := taskManifestEvidence(manifest, resolver.ID)
		if original == nil || replacement == nil || replacement.Status != "passed" || replacement.Binding == nil || replacement.Resolution != nil {
			continue
		}
		original.Status = "passed"
		original.Validator = "task-resolution"
		original.Binding = replacement.Binding
		original.ArtifactRefs = replacement.ArtifactRefs
		original.Resolution = &EvidenceResolution{
			Status: item.Resolution.Status, ResolvedBy: resolver.ID, OriginalStatus: string(item.Status),
		}
	}
}

func taskManifestEvidence(manifest *EvidenceManifest, taskID string) *EvidenceResult {
	if manifest == nil {
		return nil
	}
	var found *EvidenceResult
	for i := range manifest.EvidenceResults {
		result := &manifest.EvidenceResults[i]
		if result.RequirementID == "task:"+taskID {
			if found != nil {
				return nil // ambiguous membership cannot prove satisfaction
			}
			found = result
		}
	}
	return found
}

func verifyEvidenceResolution(manifest EvidenceManifest, result EvidenceResult) error {
	resolution := result.Resolution
	taskID := strings.TrimPrefix(result.RequirementID, "task:")
	if resolution == nil || (resolution.Status != "superseded" && resolution.Status != "reconciled") ||
		(resolution.OriginalStatus != string(TaskError) && resolution.OriginalStatus != string(TaskBlocked)) ||
		resolution.ResolvedBy == "" || resolution.ResolvedBy == taskID {
		return fmt.Errorf("evidence requirement %q has an invalid resolution", result.RequirementID)
	}
	replacement := taskManifestEvidence(&manifest, resolution.ResolvedBy)
	if replacement == nil || replacement.Status != "passed" || replacement.Resolution != nil ||
		replacement.Binding == nil || !reflect.DeepEqual(result.Binding, replacement.Binding) ||
		!reflect.DeepEqual(result.ArtifactRefs, replacement.ArtifactRefs) {
		return fmt.Errorf("evidence requirement %q has no matching verified replacement", result.RequirementID)
	}
	// Check the replacement as an ordinary execution; never recurse through
	// another resolution or accept an evidence-reference cycle.
	return verifyEvidenceBinding(manifest, *replacement)
}
