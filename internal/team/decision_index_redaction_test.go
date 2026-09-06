package team

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestDecisionIndexEntryRedactedDeeplySanitizesPresentationStrings(t *testing.T) {
	entry := DecisionIndexEntry{
		DecisionID:              "api_key=redacted-decision-id",
		RunID:                   "api_key=redacted-run-id",
		TaskID:                  "api_key=redacted-task-id",
		Profile:                 "ordinary-profile",
		EvidenceHash:            "api_key=redacted-evidence-hash",
		Question:                "api_key=redacted-question",
		FinalOption:             "api_key=redacted-option",
		FinalizationMode:        "api_key=redacted-mode",
		FinalizationIdentity:    "api_key=redacted-identity",
		FinalizationReason:      "api_key=redacted-reason",
		FinalizationWarnings:    []string{"api_key=redacted-warning"},
		FinalizationOutcome:     "api_key=redacted-finalization-outcome",
		FalsificationConditions: []string{"api_key=redacted-falsification"},
		RecordDigest:            "api_key=redacted-record-digest",
		RecordPath:              "api_key=redacted-record-path",
		StaleReason:             "api_key=redacted-stale-reason",
		AssumptionNotes:         []string{"api_key=redacted-assumption-note"},
		RecordRef: ArtifactRef{
			ID: "api_key=redacted-record-id", Kind: "api_key=redacted-record-kind",
			Role: "api_key=redacted-record-role", Path: "api_key=redacted-record-ref-path",
			Description: "api_key=redacted-record-description", Type: "api_key=redacted-record-type",
			SHA256: "api_key=redacted-record-sha", MediaType: "api_key=redacted-record-media",
			RunID: "api_key=redacted-record-run", TaskID: "api_key=redacted-record-task",
			Agent: "api_key=redacted-record-agent", Provider: "api_key=redacted-record-provider",
			ToolCallID: "api_key=redacted-record-tool",
		},
		FinalizationResultRef: &ArtifactRef{
			ID: "api_key=redacted-finalization-result-id", Kind: "api_key=redacted-finalization-result-kind",
			Role: "api_key=redacted-finalization-result-role", Path: "api_key=redacted-finalization-result-path",
			Description: "api_key=redacted-finalization-result-description", Type: "api_key=redacted-finalization-result-type",
			SHA256: "api_key=redacted-finalization-result-sha", MediaType: "api_key=redacted-finalization-result-media",
			RunID: "api_key=redacted-finalization-result-run", TaskID: "api_key=redacted-finalization-result-task",
			Agent: "api_key=redacted-finalization-result-agent", Provider: "api_key=redacted-finalization-result-provider",
			ToolCallID: "api_key=redacted-finalization-result-tool",
		},
		Assumptions: []DecisionAssumption{{
			ID: "api_key=redacted-assumption-id", Statement: "api_key=redacted-assumption-statement",
			Status:       "api_key=redacted-assumption-status",
			EvidenceRefs: []ArtifactRef{{ID: "api_key=redacted-assumption-ref"}},
		}},
		Outcome: &DecisionOutcomeRecord{
			DecisionID: "api_key=redacted-outcome-id", ResolvedOutcome: "succeeded",
			SuccessCriteria:  []string{"api_key=redacted-success-criteria"},
			ObservedEvidence: []ArtifactRef{{Path: "api_key=redacted-observed-path"}},
			Lessons:          []string{"api_key=redacted-lesson"}, Notes: "api_key=redacted-notes",
			ResolvedBy:          "api_key=redacted-resolved-by",
			UnverifiedEvidence:  []string{"api_key=redacted-unverified"},
			VerificationSummary: "api_key=redacted-verification-summary",
		},
	}
	redacted := entry.Redacted()
	data, err := json.Marshal(redacted)
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range []string{
		"redacted-decision-id", "redacted-run-id", "redacted-task-id", "redacted-evidence-hash",
		"redacted-question", "redacted-option", "redacted-mode", "redacted-identity", "redacted-reason",
		"redacted-warning", "redacted-finalization-outcome", "redacted-falsification", "redacted-record-digest",
		"redacted-record-path", "redacted-stale-reason", "redacted-assumption-note", "redacted-record-id",
		"redacted-record-kind", "redacted-record-role", "redacted-record-ref-path", "redacted-record-description",
		"redacted-record-type", "redacted-record-sha", "redacted-record-media", "redacted-record-run",
		"redacted-record-task", "redacted-record-agent", "redacted-record-provider", "redacted-record-tool",
		"redacted-finalization-result-id", "redacted-finalization-result-kind", "redacted-finalization-result-role",
		"redacted-finalization-result-path", "redacted-finalization-result-description", "redacted-finalization-result-type",
		"redacted-finalization-result-sha", "redacted-finalization-result-media", "redacted-finalization-result-run",
		"redacted-finalization-result-task", "redacted-finalization-result-agent", "redacted-finalization-result-provider",
		"redacted-finalization-result-tool",
		"redacted-assumption-id", "redacted-assumption-statement", "redacted-assumption-status", "redacted-assumption-ref",
		"redacted-outcome-id", "redacted-success-criteria", "redacted-observed-path", "redacted-lesson",
		"redacted-notes", "redacted-resolved-by", "redacted-unverified", "redacted-verification-summary",
	} {
		if strings.Contains(string(data), secret) {
			t.Errorf("redacted entry retained %q: %s", secret, data)
		}
	}
	if !strings.Contains(string(data), "ordinary-profile") || !strings.Contains(string(data), "succeeded") {
		t.Fatalf("redaction removed ordinary values: %s", data)
	}
	if entry.RecordRef.ID == "[REDACTED]" || entry.FinalizationResultRef == nil || entry.FinalizationResultRef.ID == "[REDACTED]" || entry.Outcome.Notes == "[REDACTED]" {
		t.Fatal("Redacted mutated the source entry")
	}
	if redacted.FinalizationResultRef == nil {
		t.Fatal("Redacted dropped the finalization result reference")
	}
	if redacted.FinalizationResultRef == entry.FinalizationResultRef {
		t.Fatal("Redacted retained the finalization result reference alias")
	}
	if redacted.Outcome == entry.Outcome || &redacted.Assumptions[0] == &entry.Assumptions[0] ||
		&redacted.Assumptions[0].EvidenceRefs[0] == &entry.Assumptions[0].EvidenceRefs[0] ||
		&redacted.Outcome.ObservedEvidence[0] == &entry.Outcome.ObservedEvidence[0] {
		t.Fatal("Redacted retained nested aliases")
	}

	redacted.RecordRef.ID = "mutated-record-ref"
	redacted.FinalizationResultRef.ID = "mutated-finalization-result-ref"
	redacted.Outcome.Notes = "mutated-outcome"
	redacted.Outcome.ObservedEvidence[0].Path = "mutated-outcome-evidence"
	redacted.Assumptions[0].EvidenceRefs[0].ID = "mutated-assumption-evidence"
	if entry.RecordRef.ID != "api_key=redacted-record-id" ||
		entry.FinalizationResultRef.ID != "api_key=redacted-finalization-result-id" ||
		entry.Outcome.Notes != "api_key=redacted-notes" ||
		entry.Outcome.ObservedEvidence[0].Path != "api_key=redacted-observed-path" ||
		entry.Assumptions[0].EvidenceRefs[0].ID != "api_key=redacted-assumption-ref" {
		t.Fatal("mutating redacted nested values changed the source entry")
	}
}

func TestDecisionIndexAppendReloadRedactsNestedProjection(t *testing.T) {
	tests := []struct {
		name  string
		entry DecisionIndexEntry
	}{
		{name: "resolved outcome", entry: DecisionIndexEntry{
			DecisionID: "append-reload", Profile: "ordinary-profile",
			Outcome: &DecisionOutcomeRecord{ResolvedOutcome: "succeeded", Notes: "api_key=append-reload-notes", ObservedEvidence: []ArtifactRef{{Path: "api_key=append-reload-path"}}},
		}},
		{name: "finalization", entry: DecisionIndexEntry{
			DecisionID: "append-finalization", Profile: "ordinary-profile",
			FinalizationMode: "api_key=append-reload-mode", FinalizationWarnings: []string{"api_key=append-reload-warning"},
		}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			index := newIndex(t)
			if err := index.Append(tt.entry); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(index.Path())
			if err != nil {
				t.Fatal(err)
			}
			if strings.Contains(string(data), "append-reload-") {
				t.Fatalf("index file retained sentinel: %s", data)
			}
			entries, err := index.List()
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || entries[0].Profile != "ordinary-profile" {
				t.Fatalf("reloaded projection = %#v", entries)
			}
			if entries[0].Outcome != nil && entries[0].Outcome.ResolvedOutcome != "succeeded" {
				t.Fatalf("ordinary outcome changed: %#v", entries[0].Outcome)
			}
		})
	}
}

// A finalized decision is listed so it can be found after the run exits. A
// decision blocked by a gate was never made and must not appear.
