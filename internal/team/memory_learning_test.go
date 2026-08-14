package team

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
)

func TestOnlyCompilerIncludedItemsBecomeExposures(t *testing.T) {
	policy := agent.DefaultMemoryLearningPolicy()
	policy.Mode = agent.MemoryLearningObserve
	compiled := CompiledContext{IncludedItems: []ContextItem{
		{ID: "current_task", Source: "task"},
		{ID: "context:included", Source: "shared_persistent", BaseScore: 0.8},
	}, OmittedItems: []ContextItem{{ID: "context:omitted", Source: "shared_persistent", BaseScore: 0.9}}}
	manifest := buildMemoryInjectionManifest(compiled, "run-1", "task-1", 1, "worker", "goal", policy)
	if manifest == nil || len(manifest.Items) != 1 || manifest.Items[0].ContextItemID != "included" {
		t.Fatalf("manifest = %+v", manifest)
	}
}

func TestMemoryEventsAndReportsRedactContentAndSecrets(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run", "session")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	payload, err := json.Marshal(map[string]any{
		"schema_version":  1,
		"context_item_id": "memory-1",
		"policy_version":  "v1",
		"content":         "do not persist this memory",
		"api_key":         "sk-secret-value-that-must-be-redacted",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(RunEvent{Type: "memory_retrieved", Actor: "runtime", IdempotencyKey: "memory:redaction", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	events, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	encoded := string(events[0].Payload)
	if strings.Contains(encoded, "sk-secret-value") || !strings.Contains(encoded, "[REDACTED]") {
		t.Fatalf("memory telemetry was not redacted: %s", encoded)
	}
	manifestPayload, err := json.Marshal(memoryEventPayload{SchemaVersion: 1, ContextItemID: "memory-1", PolicyVersion: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(manifestPayload), "content") {
		t.Fatalf("runtime memory payload exposes content field: %s", manifestPayload)
	}
}

func TestMemoryManifestSurvivesSessionReplay(t *testing.T) {
	workspace := t.TempDir()
	es, err := NewEventStore(workspace, "run-1", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	defer es.Close()
	manifest := MemoryInjectionManifest{RetrievalID: "retrieval-1", RunID: "run-1", TaskID: "task-1", Attempt: 1, Agent: "worker", PolicyVersion: "v1", Items: []MemoryInjectionItem{{ContextItemID: "memory-1"}}}
	payload := []byte(`{"id":"task-1","status":"done","memory_manifests":[{"retrieval_id":"retrieval-1","run_id":"run-1","task_id":"task-1","attempt":1,"agent":"worker","policy_version":"v1","items":[{"context_item_id":"memory-1","source":"shared_persistent","rank":1,"base_score":0.5,"final_score":0.5,"score_parts":{"base_relevance":0.5}}],"fingerprint":"fp","created_at":"2026-01-01T00:00:00Z"}]}`)
	if err := es.Append(RunEvent{Type: "task_completed", TaskID: "task-1", Actor: "worker", Payload: payload}); err != nil {
		t.Fatal(err)
	}
	events, _ := es.ReadEvents()
	tasks := ReduceToTodoList(events)
	if len(tasks) != 1 || len(tasks[0].MemoryManifests) != 1 || tasks[0].MemoryManifests[0].RetrievalID != manifest.RetrievalID {
		t.Fatalf("replayed tasks = %+v", tasks)
	}
}

func TestUnknownMemoryIDFailsClosed(t *testing.T) {
	c, manifest := memoryValidationCoordinator(t)
	result := &TaskResult{TaskID: manifest.TaskID, Attempt: 1, Source: "submitted", MemoryUses: []MemoryUseRef{{RetrievalID: manifest.RetrievalID, ContextItemID: "forged", Disposition: MemoryUseApplied, Confidence: 1}}}
	if err := c.validateMemoryUseClaims(context.Background(), manifest.TaskID, result); err == nil || !strings.Contains(err.Error(), "was not injected") {
		t.Fatalf("validation error = %v", err)
	}
}

func TestMemoryUseSealsRuntimeAttempt(t *testing.T) {
	c, manifest := memoryValidationCoordinator(t)
	result := &TaskResult{TaskID: manifest.TaskID, Source: "submitted", MemoryUses: []MemoryUseRef{{RetrievalID: manifest.RetrievalID, ContextItemID: "memory-1", Disposition: MemoryUseApplied, Confidence: 1}}}
	if err := c.validateMemoryUseClaims(context.Background(), manifest.TaskID, result); err != nil {
		t.Fatal(err)
	}
	if result.Attempt != manifest.Attempt {
		t.Fatalf("sealed attempt = %d, want %d", result.Attempt, manifest.Attempt)
	}
}

func TestMemoryUseReasonCodeCannotCarryProseOrSecrets(t *testing.T) {
	c, manifest := memoryValidationCoordinator(t)
	result := &TaskResult{TaskID: manifest.TaskID, Source: "submitted", MemoryUses: []MemoryUseRef{{RetrievalID: manifest.RetrievalID, ContextItemID: "memory-1", Disposition: MemoryUseRejected, ReasonCode: "the full prompt said sk-secret", Confidence: 1}}}
	if err := c.validateMemoryUseClaims(context.Background(), manifest.TaskID, result); err == nil {
		t.Fatal("free-form reason_code was accepted into memory telemetry")
	}
}

func TestMemoryUseCannotCrossTaskOrAttempt(t *testing.T) {
	c, manifest := memoryValidationCoordinator(t)
	result := &TaskResult{TaskID: manifest.TaskID, Attempt: 2, Source: "submitted", MemoryUses: []MemoryUseRef{{RetrievalID: manifest.RetrievalID, ContextItemID: "memory-1", Disposition: MemoryUseApplied, Confidence: 1}}}
	if err := c.validateMemoryUseClaims(context.Background(), manifest.TaskID, result); err == nil {
		t.Fatal("cross-attempt memory claim was accepted")
	}
}

func TestFreeTextResultCannotClaimAppliedMemory(t *testing.T) {
	c, manifest := memoryValidationCoordinator(t)
	result := ParseFreeTextResult("done")
	result.MemoryUses = []MemoryUseRef{{RetrievalID: manifest.RetrievalID, ContextItemID: "memory-1", Disposition: MemoryUseApplied, Confidence: 1}}
	if err := c.validateMemoryUseClaims(context.Background(), manifest.TaskID, result); err == nil {
		t.Fatal("free-text memory claim was accepted")
	}
}

func memoryValidationCoordinator(t *testing.T) (*Coordinator, MemoryInjectionManifest) {
	t.Helper()
	repo, err := contextstore.OpenSQLite(filepath.Join(t.TempDir(), "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	if err := repo.Append(context.Background(), contextstore.ContextItem{ID: "memory-1", Kind: contextstore.ContextPattern, Content: "procedure", Scope: contextstore.Scope{ProjectID: "project"}, Lifecycle: contextstore.LifecycleConfirmed}); err != nil {
		t.Fatal(err)
	}
	tracker := NewTaskTracker()
	tasks := tracker.TodoList().AddBatch([]TodoSpec{{Agent: "worker", Desc: "task"}})
	manifest := MemoryInjectionManifest{RetrievalID: "retrieval-1", RunID: "run-1", TaskID: tasks[0].ID, Attempt: 1, Agent: "worker", PolicyVersion: "v1", Items: []MemoryInjectionItem{{ContextItemID: "memory-1"}}}
	if err := tracker.TodoList().SetMemoryManifest(tasks[0].ID, &manifest); err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{contextRepo: repo, taskTracker: tracker, executionRunID: "run-1", taskAttempts: map[string]int{tasks[0].ID: 1}}
	return c, manifest
}
