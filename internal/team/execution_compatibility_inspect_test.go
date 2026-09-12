package team

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/execution"
	"github.com/kjelly/hufu/internal/executioncompat"
	"github.com/kjelly/hufu/internal/modelprofile"
)

func appendCompatibilityEvent(t *testing.T, store *EventStore, event RunEvent) RunEvent {
	t.Helper()
	persisted, err := store.AppendPersisted(event)
	if err != nil {
		t.Fatalf("append event: %v", err)
	}
	return persisted
}

func compatibilityPayload(t *testing.T, value any) json.RawMessage {
	t.Helper()
	payload, err := json.Marshal(value)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return payload
}

func TestCompatibilityScannerCountsPhysicalEventsOnce(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-1", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	appendCompatibilityEvent(t, store, RunEvent{
		Type: string(EventTaskCreated), TaskID: "task-1", RunID: "run-1", SessionID: "session-1", Actor: "worker", Timestamp: "2026-01-01T00:00:00Z",
		Payload: compatibilityPayload(t, map[string]any{
			"id": "task-1", "status": "pending", "model": "local/qwen3:8b", "subagent_provider": "hufu-local",
			"execution_target": map[string]any{"backend": "local", "model": "qwen3:8b"},
			"provider_binding": map[string]any{"provider": "ollama"},
		}),
	})
	appendCompatibilityEvent(t, store, RunEvent{
		Type: string(EventTaskCompleted), TaskID: "task-1", RunID: "run-1", SessionID: "session-1", Actor: "worker", Timestamp: "2026-01-01T00:00:01Z",
		Payload: compatibilityPayload(t, map[string]any{
			"id": "task-1", "status": "done", "execution_receipt": map[string]any{"subagent_provider": "hufu-local"},
		}),
	})
	appendCompatibilityEvent(t, store, RunEvent{
		Type: string(EventProviderSessionBound), TaskID: "task-1", RunID: "run-1", SessionID: "session-1", Actor: "worker", Timestamp: "2026-01-01T00:00:02Z",
		Payload: compatibilityPayload(t, map[string]any{"task_id": "task-1", "provider": "ollama"}),
	})
	appendCompatibilityEvent(t, store, RunEvent{
		Type: string(EventExecutionPolicySnapshot), RunID: "run-1", SessionID: "session-1", Actor: "coordinator", Timestamp: "2026-01-01T00:00:03Z",
		Payload: compatibilityPayload(t, executioncompat.PolicyInput{Version: 3, Routes: []executioncompat.PolicyRoute{{Model: "qwen3:8b", Backend: "local", LegacyProvider: "local"}}}),
	})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, logsDir, executionEventsFile), []byte(`{"version":4,"provider":"ollama"}`+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := InspectExecutionCompatibility(context.Background(), workspace, "")
	if err != nil {
		t.Fatalf("InspectExecutionCompatibility: %v", err)
	}
	if report.LegacyLocalAliasEvents != 2 || report.LegacyProviderShadowEvents != 1 || report.LegacyProviderBindings != 1 || report.LegacyProviderSessionEvents != 1 || report.LegacyReceiptProviders != 1 || report.LegacyPolicyRoutes != 1 || report.LegacyExecutionEventProviders != 1 {
		t.Fatalf("raw counters = %#v", report)
	}
	if report.MigratableTasks != 1 || report.MigratablePolicySnapshots != 1 || len(report.Findings) != 2 {
		t.Fatalf("subject results = %#v", report)
	}
	first, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	second, err := json.Marshal(report)
	if err != nil || !bytes.Equal(first, second) {
		t.Fatalf("inspection JSON is not deterministic: %s / %s / %v", first, second, err)
	}
}

func TestCompatibilityScannerIsByteAndMtimeReadOnly(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, sessionFile)
	data := []byte(`{"tasks":[{"id":"legacy","model":"qwen3:8b","subagent_provider":"hufu-local"}]}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	beforeBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	report, err := InspectExecutionCompatibility(context.Background(), workspace, "")
	if err != nil {
		t.Fatal(err)
	}
	if report.MigratableTasks != 1 {
		t.Fatalf("report = %#v, want one session-only migratable task", report)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	afterBytes, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeBytes, afterBytes) || before.Mode() != after.Mode() || !before.ModTime().Equal(after.ModTime()) {
		t.Fatalf("inspector mutated session.json: before=%v after=%v", before, after)
	}
}

func TestCompatibilityScannerDoesNotUseSessionRuntimeShadowsAsEvidence(t *testing.T) {
	workspace := t.TempDir()
	canonical := &SessionData{Tasks: []*TodoItem{{
		ID: "canonical", Status: TaskPending,
		ExecutionTarget:   execution.ExecutionTarget{Backend: "ollama", Model: "qwen3:8b"},
		ExecutionTopology: []execution.ExecutionTarget{{Backend: "ollama", Model: "qwen3:8b"}},
		BackendBinding:    &BackendBinding{Backend: "ollama", EffectiveTarget: "qwen3:8b"},
	}}}
	if err := SaveSession(workspace, canonical); err != nil {
		t.Fatal(err)
	}
	report, err := InspectExecutionCompatibility(context.Background(), workspace, "")
	if err != nil {
		t.Fatal(err)
	}
	if report.CanonicalTasks != 1 || report.MigratableTasks != 0 || len(report.Findings) != 0 {
		t.Fatalf("canonical session was classified using runtime shadows: %#v", report)
	}
}

func TestCompatibilityScannerDoesNotUseHistoricalProfileToRetargetTypedTask(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-1", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	appendCompatibilityEvent(t, store, RunEvent{
		Type: string(EventTaskCreated), TaskID: "canonical", RunID: "run-1", SessionID: "session-1", Actor: "worker", Timestamp: "2026-01-01T00:00:00Z",
		Payload: compatibilityPayload(t, map[string]any{"id": "canonical", "status": "pending", "execution_target": map[string]any{"backend": "ollama", "model": "qwen3:8b"}, "execution_topology": []map[string]any{{"backend": "ollama", "model": "qwen3:8b"}}}),
	})
	appendCompatibilityEvent(t, store, RunEvent{
		Type: string(EventModelProfileResolved), RunID: "run-1", SessionID: "session-1", Actor: "coordinator", Timestamp: "2026-01-01T00:00:01Z",
		Payload: compatibilityPayload(t, modelprofile.TelemetryProjection{SchemaVersion: 1, InvocationID: "profile-1", ModelID: "qwen3:8b", Provider: "codex"}),
	})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	report, err := InspectExecutionCompatibility(t.Context(), workspace, "")
	if err != nil {
		t.Fatal(err)
	}
	if report.CanonicalTasks != 1 || report.MigratableTasks != 0 || report.AmbiguousTasks != 0 || len(report.Findings) != 0 {
		t.Fatalf("historical profile retargeted canonical task: %#v", report)
	}
}

func TestCompatibilityScannerRejectsCorruptHashChain(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-1", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	appendCompatibilityEvent(t, store, RunEvent{Type: string(EventTaskCreated), TaskID: "task-1", RunID: "run-1", SessionID: "session-1", Actor: "worker", Timestamp: time.Now().UTC().Format(time.RFC3339Nano), Payload: compatibilityPayload(t, map[string]any{"id": "task-1", "status": "pending"})})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(workspace, logsDir, eventStoreFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, bytes.Replace(data, []byte(`"status":"pending"`), []byte(`"status":"blocked"`), 1), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectExecutionCompatibility(context.Background(), workspace, ""); err == nil {
		t.Fatal("corrupt event chain was accepted")
	}
}

func TestCompatibilityScannerClassifiesBranchTaskOccurrences(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-1", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	mainEvent := appendCompatibilityEvent(t, store, RunEvent{
		Type: string(EventTaskCreated), TaskID: "inherited", RunID: "run-1", SessionID: "session-1", Actor: "worker", Timestamp: "2026-01-01T00:00:00Z",
		Payload: compatibilityPayload(t, map[string]any{"id": "inherited", "status": "pending", "model": "qwen3:8b", "subagent_provider": "hufu-local"}),
	})
	appendCompatibilityEvent(t, store, RunEvent{
		Type: string(EventTaskCreated), BranchID: "feature", TaskID: "child-only", RunID: "run-1", SessionID: "session-1", Actor: "worker", Timestamp: "2026-01-01T00:00:01Z",
		Payload: compatibilityPayload(t, map[string]any{"id": "child-only", "status": "pending", "execution_target": map[string]any{"backend": "ollama", "model": "qwen3:8b"}}),
	})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	tree := NewSessionTree()
	tree.Branches["feature"] = &SessionBranch{ID: "feature", Name: "feature", ParentID: "main", ForkEventID: mainEvent.ID}
	if err := SaveSessionTree(workspace, tree); err != nil {
		t.Fatal(err)
	}

	all, err := InspectExecutionCompatibility(context.Background(), workspace, "")
	if err != nil {
		t.Fatal(err)
	}
	if all.MigratableTasks != 2 || all.CanonicalTasks != 1 {
		t.Fatalf("all-branch task classifications = %#v", all)
	}
	feature, err := InspectExecutionCompatibility(context.Background(), workspace, "feature")
	if err != nil {
		t.Fatal(err)
	}
	if feature.Scope != "branch:feature" || feature.MigratableTasks != 1 || feature.CanonicalTasks != 1 {
		t.Fatalf("feature branch classifications = %#v", feature)
	}
}
