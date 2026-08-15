package context

import (
	"context"
	"path/filepath"
	"testing"
)

func TestActivationProjectionBackfillsAndTracksMetadata(t *testing.T) {
	repo, err := OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	item := ContextItem{ID: "activated", Kind: ContextPattern, Content: "recovery guidance", Scope: Scope{ProjectID: "p"}, Metadata: map[string]string{"activation.phases": "VERIFY", "activation.triggers": "tool_failure", "activation.roles": "worker", "activation.environment": "linux"}}
	if err := repo.Append(context.Background(), item); err != nil {
		t.Fatal(err)
	}
	record, err := repo.ActivationForItem(context.Background(), item.ID)
	if err != nil {
		t.Fatal(err)
	}
	if record.Phases != "VERIFY" || record.Triggers != "tool_failure" || record.Roles != "worker" || record.Environment != "linux" {
		t.Fatalf("unexpected typed activation projection: %+v", record)
	}
}

func TestContextOutcomeObservationIsDimensionedAndIdempotent(t *testing.T) {
	repo, err := OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer repo.Close()
	obs := ContextOutcomeObservation{IdempotencyKey: "obs-1", ContextItemID: "memory-1", Phase: "EXECUTE", Trigger: "retry", AgentRole: "worker", Environment: "linux", Outcome: "selected", PolicyRevision: "v1", RequestID: "request-1", ManifestFingerprint: "manifest-1", RunID: "run-1", TaskID: "task-1", Attempt: 2, ModelExecutionID: "model-1", VerificationOutcome: "passed", AcceptanceOutcome: "not_assessed", JudgeOutcome: "not_assessed", SkepticOutcome: "not_assessed"}
	applied, err := repo.RecordContextOutcomeObservation(context.Background(), obs)
	if err != nil || !applied {
		t.Fatalf("first observation: applied=%v err=%v", applied, err)
	}
	applied, err = repo.RecordContextOutcomeObservation(context.Background(), obs)
	if err != nil || applied {
		t.Fatalf("duplicate observation: applied=%v err=%v", applied, err)
	}
	count, err := repo.ContextOutcomeCount(context.Background(), "memory-1", "EXECUTE", "retry", "worker", "linux", "selected")
	if err != nil || count != 1 {
		t.Fatalf("dimension count=%d err=%v", count, err)
	}
	var requestID, fingerprint, runID, taskID, modelID, verify string
	var attempt int
	err = repo.db.QueryRowContext(context.Background(), `SELECT request_id,manifest_fingerprint,run_id,task_id,attempt,model_execution_id,verification_outcome FROM context_outcome_observations WHERE idempotency_key='obs-1'`).Scan(&requestID, &fingerprint, &runID, &taskID, &attempt, &modelID, &verify)
	if err != nil || requestID != "request-1" || fingerprint != "manifest-1" || runID != "run-1" || taskID != "task-1" || attempt != 2 || modelID != "model-1" || verify != "passed" {
		t.Fatalf("execution linkage was not stored: request=%q manifest=%q run=%q task=%q attempt=%d model=%q verify=%q err=%v", requestID, fingerprint, runID, taskID, attempt, modelID, verify, err)
	}
}
