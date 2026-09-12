package team

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/kjelly/hufu/internal/execution"
)

func TestApplyExecutionCompatibilityMaterializesEventTaskIdempotently(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-1", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	appendCompatibilityEvent(t, store, RunEvent{
		Type: string(EventTaskCreated), TaskID: "task-1", RunID: "run-1", SessionID: "session-1", Actor: "worker", Timestamp: "2026-01-01T00:00:00Z",
		Payload: compatibilityPayload(t, map[string]any{"id": "task-1", "status": "pending", "model": "qwen3:8b", "subagent_provider": "hufu-local"}),
	})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(workspace, logsDir, eventStoreFile)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)

	}
	result, err := ApplyExecutionCompatibility(context.Background(), workspace, "")
	if err != nil {
		t.Fatalf("ApplyExecutionCompatibility: %v", err)
	}
	if result.TaskMigrationEvents != 1 || !result.ProjectionRebuilt {
		t.Fatalf("apply result = %#v", result)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(after, before) {
		t.Fatal("materializer rewrote pre-existing event-store bytes")
	}

	report, err := InspectExecutionCompatibility(context.Background(), workspace, "")
	if err != nil {
		t.Fatal(err)
	}
	if report.MigratedTasks != 1 || report.MigratableTasks != 0 {
		t.Fatalf("post-apply inspection = %#v", report)
	}
	reader, err := OpenEventStore(workspace)
	if err != nil {
		t.Fatal(err)
	}
	events, err := reader.ReadEvents()
	_ = reader.Close()
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := ReplayTodoList(events)
	if err != nil {
		t.Fatalf("ReplayTodoList: %v", err)
	}
	if len(tasks) != 1 || tasks[0].ExecutionTarget != (execution.ExecutionTarget{Backend: "ollama", Model: "qwen3:8b"}) {
		t.Fatalf("replayed tasks = %#v", tasks)
	}
	beforeRepeat := append([]byte(nil), after...)
	second, err := ApplyExecutionCompatibility(context.Background(), workspace, "")
	if err != nil {
		t.Fatalf("repeat apply: %v", err)
	}
	if second.TaskMigrationEvents != 0 || second.PolicyMigrationEvents != 0 {
		t.Fatalf("repeat result = %#v", second)
	}
	afterRepeat, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(beforeRepeat, afterRepeat) {
		t.Fatal("repeat apply appended a duplicate migration")
	}
}

func TestApplyExecutionCompatibilityWritesOneSelfContainedSessionTask(t *testing.T) {
	workspace := t.TempDir()
	legacy := &SessionData{Tasks: []*TodoItem{{ID: "session-only", Status: TaskPending, Model: "qwen3:8b", SubagentProvider: "hufu-local"}}}
	if err := SaveSession(workspace, legacy); err != nil {
		t.Fatal(err)
	}
	result, err := ApplyExecutionCompatibility(context.Background(), workspace, "")
	if err != nil {
		t.Fatalf("ApplyExecutionCompatibility: %v", err)
	}
	if result.TaskMigrationEvents != 1 || !result.ProjectionRebuilt {
		t.Fatalf("apply result = %#v", result)
	}
	store, err := OpenEventStore(workspace)
	if err != nil {
		t.Fatal(err)
	}
	events, err := store.ReadEvents()
	_ = store.Close()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || EventType(events[0].Type) != EventExecutionCompatibilityMigrated {
		t.Fatalf("events = %#v", events)
	}
	var payload ExecutionCompatibilityMigratedPayload
	if err := json.Unmarshal(events[0].Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.SourceKind != "session_snapshot" || len(payload.CanonicalTask) == 0 || hasLegacyTaskIdentityJSON(payload.CanonicalTask) {
		t.Fatalf("session migration payload = %#v", payload)
	}
	report, err := InspectExecutionCompatibility(context.Background(), workspace, "")
	if err != nil {
		t.Fatal(err)
	}
	if report.MigratedTasks != 1 {
		t.Fatalf("post-apply inspection = %#v", report)
	}
}

func TestApplyExecutionCompatibilityMaterializesV3PolicyWithoutLiveConfig(t *testing.T) {
	workspace := t.TempDir()
	snapshot := &ExecutionPolicySnapshot{
		Version:           executionPolicyLegacySnapshotVersion,
		DefaultLLMBackend: "local",
		Backends:          []ExecutionBackendPolicySnapshot{{Backend: "local", Kind: "llm", IdentityHash: "identity"}},
		ModelRoutes:       []ExecutionModelRouteSnapshot{{Model: "qwen3:8b", Backend: "local", LegacyProvider: "local"}},
	}
	var err error
	snapshot.ConfigurationHash, err = executionPolicyConfigurationHash(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	store, err := NewEventStore(workspace, "run-1", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	appendCompatibilityEvent(t, store, RunEvent{Type: string(EventExecutionPolicySnapshot), RunID: "run-1", SessionID: "session-1", Actor: "coordinator", Timestamp: "2026-01-01T00:00:00Z", Payload: compatibilityPayload(t, snapshot)})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	result, err := ApplyExecutionCompatibility(context.Background(), workspace, "")
	if err != nil {
		t.Fatalf("ApplyExecutionCompatibility: %v", err)
	}
	if result.PolicyMigrationEvents != 1 || !result.ProjectionRebuilt {
		t.Fatalf("apply result = %#v", result)
	}
	store, err = OpenEventStore(workspace)
	if err != nil {
		t.Fatal(err)
	}
	events, err := store.ReadEvents()
	_ = store.Close()
	if err != nil {
		t.Fatal(err)
	}
	projection := ReduceToSessionData(events)
	if projection.ExecutionPolicySnapshot == nil || projection.ExecutionPolicySnapshot.Version != executionPolicySnapshotVersion || projection.ExecutionPolicySnapshot.ModelRoutes[0].LegacyProvider != "" || projection.ExecutionPolicySnapshot.ModelRoutes[0].Backend != "ollama" {
		t.Fatalf("canonical policy projection = %#v", projection.ExecutionPolicySnapshot)
	}
	report, err := InspectExecutionCompatibility(context.Background(), workspace, "")
	if err != nil {
		t.Fatal(err)
	}
	if report.MigratedPolicySnapshots != 1 || report.MigratablePolicySnapshots != 0 {
		t.Fatalf("post-apply inspection = %#v", report)
	}
}

func TestApplyExecutionCompatibilityMaterializesSessionOnlyV3Policy(t *testing.T) {
	workspace := t.TempDir()
	snapshot := &ExecutionPolicySnapshot{
		Version:           executionPolicyLegacySnapshotVersion,
		DefaultLLMBackend: "local",
		Backends:          []ExecutionBackendPolicySnapshot{{Backend: "local", Kind: "llm", IdentityHash: "identity"}},
		ModelRoutes:       []ExecutionModelRouteSnapshot{{Model: "qwen3:8b", Backend: "local", LegacyProvider: "local"}},
	}
	var err error
	snapshot.ConfigurationHash, err = executionPolicyConfigurationHash(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if err := SaveSession(workspace, &SessionData{ExecutionPolicySnapshot: snapshot}); err != nil {
		t.Fatal(err)
	}
	before, err := InspectExecutionCompatibility(context.Background(), workspace, "")
	if err != nil {
		t.Fatal(err)
	}
	if before.MigratablePolicySnapshots != 1 {
		t.Fatalf("pre-apply inspection = %#v", before)
	}
	result, err := ApplyExecutionCompatibility(context.Background(), workspace, "")
	if err != nil {
		t.Fatal(err)
	}
	if result.PolicyMigrationEvents != 1 {
		t.Fatalf("apply result = %#v", result)
	}
	after, err := InspectExecutionCompatibility(context.Background(), workspace, "")
	if err != nil {
		t.Fatal(err)
	}
	if after.MigratedPolicySnapshots != 1 || after.MigratablePolicySnapshots != 0 {
		t.Fatalf("post-apply inspection = %#v", after)
	}
}

func TestApplyExecutionCompatibilityRefusesAmbiguousTaskWithoutAppending(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-1", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	appendCompatibilityEvent(t, store, RunEvent{
		Type: string(EventTaskCreated), TaskID: "ambiguous", RunID: "run-1", SessionID: "session-1", Actor: "worker", Timestamp: "2026-01-01T00:00:00Z",
		Payload: compatibilityPayload(t, map[string]any{"id": "ambiguous", "status": "pending", "model": "qwen3:8b", "subagent_provider": "hufu-local", "provider_binding": map[string]any{"provider": "codex"}}),
	})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(workspace, logsDir, eventStoreFile)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyExecutionCompatibility(context.Background(), workspace, ""); err == nil {
		t.Fatal("ambiguous task was materialized")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("ambiguous preflight appended an event")
	}
}

func TestApplyExecutionCompatibilityMaterializesBackendBindingAndReceipt(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-1", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	appendCompatibilityEvent(t, store, RunEvent{
		Type: string(EventTaskCreated), TaskID: "task-1", RunID: "run-1", SessionID: "session-1", Actor: "worker", Timestamp: "2026-01-01T00:00:00Z",
		Payload: compatibilityPayload(t, map[string]any{
			"id": "task-1", "status": "pending", "model": "qwen3:8b", "subagent_provider": "hufu-local",
			"provider_binding":  map[string]any{"provider": "hufu-local", "session_id": "thread-1"},
			"execution_receipt": map[string]any{"run_id": "run-1", "task_id": "task-1", "attempt": 1, "subagent_provider": "hufu-local"},
		}),
	})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyExecutionCompatibility(context.Background(), workspace, ""); err != nil {
		t.Fatal(err)
	}
	store, err = OpenEventStore(workspace)
	if err != nil {
		t.Fatal(err)
	}
	events, err := store.ReadEvents()
	_ = store.Close()
	if err != nil {
		t.Fatal(err)
	}
	tasks, err := ReplayTodoList(events)
	if err != nil {
		t.Fatal(err)
	}
	if len(tasks) != 1 || tasks[0].BackendBinding == nil || tasks[0].BackendBinding.Backend != "ollama" || tasks[0].BackendBinding.SessionID != "thread-1" || tasks[0].ExecutionReceipt == nil || tasks[0].ExecutionReceipt.Backend != "ollama" || tasks[0].ExecutionReceipt.SubagentProvider != "" {
		t.Fatalf("canonical task = %#v", tasks)
	}
}

func TestApplyExecutionCompatibilityIsBranchScopedAfterFork(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-1", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	created := appendCompatibilityEvent(t, store, RunEvent{
		Type: string(EventTaskCreated), TaskID: "inherited", RunID: "run-1", SessionID: "session-1", Actor: "worker", Timestamp: "2026-01-01T00:00:00Z",
		Payload: compatibilityPayload(t, map[string]any{"id": "inherited", "status": "pending", "model": "qwen3:8b", "subagent_provider": "hufu-local"}),
	})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	tree := NewSessionTree()
	tree.Branches["feature"] = &SessionBranch{ID: "feature", Name: "feature", ParentID: "main", ForkEventID: created.ID}
	if err := SaveSessionTree(workspace, tree); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyExecutionCompatibility(context.Background(), workspace, "main"); err != nil {
		t.Fatalf("main apply: %v", err)
	}
	if _, err := ApplyExecutionCompatibility(context.Background(), workspace, "feature"); err != nil {
		t.Fatalf("feature apply: %v", err)
	}
	store, err = OpenEventStore(workspace)
	if err != nil {
		t.Fatal(err)
	}
	events, err := store.ReadEvents()
	_ = store.Close()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, event := range events {
		if EventType(event.Type) == EventExecutionCompatibilityMigrated && event.TaskID == "inherited" {
			seen[event.BranchID]++
		}
	}
	if seen["main"] != 1 || seen["feature"] != 1 {
		t.Fatalf("branch migration events = %#v", seen)
	}
}

func TestCanonicalReplayRejectsPostMigrationRetarget(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-1", "session-1")
	if err != nil {
		t.Fatal(err)
	}
	appendCompatibilityEvent(t, store, RunEvent{
		Type: string(EventTaskCreated), TaskID: "task-1", RunID: "run-1", SessionID: "session-1", Actor: "worker", Timestamp: "2026-01-01T00:00:00Z",
		Payload: compatibilityPayload(t, map[string]any{"id": "task-1", "status": "pending", "model": "qwen3:8b", "subagent_provider": "hufu-local"}),
	})
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := ApplyExecutionCompatibility(context.Background(), workspace, ""); err != nil {
		t.Fatal(err)
	}
	store, err = OpenEventStore(workspace)
	if err != nil {
		t.Fatal(err)
	}
	retarget := &TodoItem{ID: "task-1", Status: TaskDone, ExecutionTarget: execution.ExecutionTarget{Backend: "codex", Model: "gpt-5.6-luna"}, ExecutionTopology: []execution.ExecutionTarget{{Backend: "codex", Model: "gpt-5.6-luna"}}}
	appendCompatibilityEvent(t, store, RunEvent{Type: string(EventTaskCompleted), TaskID: "task-1", RunID: "run-1", SessionID: "session-1", Actor: "worker", Timestamp: "2026-01-01T00:00:01Z", Payload: compatibilityPayload(t, taskTransitionPayload(retarget))})
	events, err := store.ReadEvents()
	_ = store.Close()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ReplayTodoList(events); err == nil {
		t.Fatal("post-migration retarget was accepted")
	}
}
