package team

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadDecisionIndexEntriesReadOnlyDoesNotCreateMissingWorkspace(t *testing.T) {
	workspace := filepath.Join(t.TempDir(), "missing")
	entries, err := LoadDecisionIndexEntriesReadOnly(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("entries = %#v, want none", entries)
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatalf("read-only decision load created workspace: %v", err)
	}
}

func TestProjectDecisionEntriesForLineageUsesCanonicalFinalizedRecord(t *testing.T) {
	record := DecisionRecord{SchemaVersion: 1, ID: "decision-1", RunID: "run-1", TaskID: "task-1", EvidenceHash: "evidence-hash", FinalOption: "ship"}
	payload, err := json.Marshal(decisionEvent{
		DecisionID: record.ID, RunID: record.RunID, TaskID: record.TaskID,
		Record: &record, RecordRef: ArtifactRef{ID: "record-1", SHA256: "record-digest"},
	})
	if err != nil {
		t.Fatal(err)
	}
	entries, err := ProjectDecisionEntriesForLineage(t.Context(), []RunEvent{{
		Type: "decision_finalized", RunID: record.RunID, TaskID: record.TaskID, Actor: "decision-runtime", Payload: payload,
	}})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].DecisionID != record.ID || entries[0].EffectiveRecordRef().ID != "record-1" {
		t.Fatalf("projected entries = %#v", entries)
	}
}
