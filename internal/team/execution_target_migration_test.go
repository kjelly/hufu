package team

import (
	"encoding/json"
	"testing"

	"github.com/kjelly/hufu/internal/modelprofile"
)

func TestMigratableLegacyExecutionTargetUsesDurableProviderBindingEvidence(t *testing.T) {
	target, err := migratableLegacyExecutionTarget(&TodoItem{
		ID:               "legacy-1",
		Model:            "openrouter/meta/llama",
		SubagentProvider: localSubagentProviderName,
		ProviderBinding:  &ProviderBinding{Provider: "openrouter"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := target.String(), "openrouter/meta/llama"; got != want {
		t.Fatalf("target = %q, want %q", got, want)
	}
}

func TestMigratableLegacyExecutionTargetIgnoresLocalBindingMarker(t *testing.T) {
	target, err := migratableLegacyExecutionTarget(&TodoItem{
		ID:              "legacy-local",
		Model:           "qwen3:8b",
		BackendBinding:  &BackendBinding{Backend: localSubagentProviderName},
		ProviderBinding: &ProviderBinding{Provider: localSubagentProviderName},
	})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := target.String(), "local/qwen3:8b"; got != want {
		t.Fatalf("target = %q, want %q", got, want)
	}
}

func TestHistoricalProfileEvidenceRecoversMatchingTaskRunOnly(t *testing.T) {
	payload, err := json.Marshal(modelprofile.TelemetryProjection{ModelID: "meta/llama", Provider: "openrouter"})
	if err != nil {
		t.Fatal(err)
	}
	target, evidenceIDs, ok := targetFromHistoricalProfileEvidence(&TodoItem{ID: "legacy-2", Model: "openrouter/meta/llama"}, []RunEvent{
		{Type: string(EventTaskCreated), TaskID: "legacy-2", RunID: "run-a"},
		{ID: "wrong-run", Type: string(EventModelProfileResolved), RunID: "run-b", Payload: payload},
		{ID: "profile-a", Type: string(EventModelProfileResolved), RunID: "run-a", Payload: payload},
	})
	if !ok {
		t.Fatal("historical profile evidence was not recovered")
	}
	if got, want := target.String(), "openrouter/meta/llama"; got != want {
		t.Fatalf("target = %q, want %q", got, want)
	}
	if len(evidenceIDs) != 1 || evidenceIDs[0] != "profile-a" {
		t.Fatalf("evidence IDs = %#v, want profile-a", evidenceIDs)
	}
}
