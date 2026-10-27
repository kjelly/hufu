package team

import (
	"encoding/json/jsontext"
	"encoding/json/v2"
)

func resultCollectionFailureDiagnostic(item *TodoItem) (string, bool) {
	checkpoint := currentResultCollection(item)
	if checkpoint == nil || checkpoint.State != ResultCollectionBlocked || item.ExecutionReceipt.RepairProvenance == nil {
		return "", false
	}
	provenance := item.ExecutionReceipt.RepairProvenance
	if !provenance.Attempted || provenance.Success || provenance.FailureReason != checkpoint.FailureReason || provenance.Error == "" {
		return "", false
	}
	return schemaRepairDiagnostic(provenance.Error), true
}

// The published schema permits strings as file entries. Convert only that
// declared shorthand; all other claims retain their strict DTO boundary.
func normalizeCollectionFileRefs(raw []byte) ([]byte, error) {
	var fields map[string]jsontext.Value
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	for _, name := range []string{"files_read", "files_modified"} {
		value, exists := fields[name]
		if !exists {
			continue
		}
		var entries []jsontext.Value
		if err := json.Unmarshal(value, &entries); err != nil {
			return nil, err
		}
		changed := false
		for i, entry := range entries {
			var path *string
			if err := json.Unmarshal(entry, &path); err != nil || path == nil {
				continue
			}
			encoded, err := json.Marshal(FileRef{Path: *path})
			if err != nil {
				return nil, err
			}
			entries[i], changed = encoded, true
		}
		if changed {
			encoded, err := json.Marshal(entries)
			if err != nil {
				return nil, err
			}
			fields[name] = encoded
		}
	}
	return json.Marshal(fields, json.Deterministic(true))
}

// Only duplicate representations of findings and attested invariant claims
// qualify as format repair. Other assertion failures remain contract failures.
// Never retry admission that may already have materialized artifact declarations.
func collectionProjectionFormatFailure(contract taskResultSubmissionContract, result *TaskResult) bool {
	if result == nil || len(result.Artifacts) != 0 {
		return false
	}
	document, err := canonicalTaskResultDocument(result)
	if err != nil {
		return false
	}
	found := false
	for _, assertion := range contract.SubmissionTaskResultAssertions {
		if len(evaluateTaskResultAssertions(document, []TaskResultAssertion{assertion})) == 0 {
			continue
		}
		if assertion.Op != "equals_projection" || (assertion.Pointer != "/findings" && assertion.Pointer != "/invariant_verification/assessments") {
			return false
		}
		projection, err := decodeTaskResultProjection(assertion.Value)
		if err != nil || taskResultAssertionRootField(projection.Pointer) != "structured_payload" {
			return false
		}
		found = true
	}
	return found
}
