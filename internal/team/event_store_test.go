package team

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
)

func TestEventStoreSyncFailureIsObservable(t *testing.T) {
	dir := t.TempDir()
	es, err := NewEventStore(dir, "run-sync", "session-sync")
	if err != nil {
		t.Fatal(err)
	}
	defer es.Close()

	const uncertainKey = "run:started:run-sync"
	es.syncFile = func() error { return errors.New("injected sync failure") }
	if err := es.Append(RunEvent{Type: "run_started", Actor: "user", IdempotencyKey: uncertainKey}); err == nil {
		t.Fatal("Append returned nil for a failed fsync")
	}
	if !es.degraded {
		t.Fatal("event store was not marked degraded after uncertain durability")
	}

	// A subsequent append must reopen and rescan the bytes on disk before it
	// chooses the previous hash. It must not continue from stale in-memory state.
	if err := es.Append(RunEvent{Type: "run_started", Actor: "user", IdempotencyKey: uncertainKey}); err != nil {
		t.Fatalf("retry uncertain append: %v", err)
	}
	if err := es.Append(RunEvent{Type: "task_created", Actor: "coordinator"}); err != nil {
		t.Fatalf("Append after degraded recovery: %v", err)
	}
	if es.degraded {
		t.Fatal("event store remained degraded after successful reopen/rescan")
	}
	if err := es.VerifyHashChain(); err != nil {
		t.Fatalf("hash chain after recovery: %v", err)
	}
	events, err := es.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("event count = %d, want uncertain event plus one subsequent event", len(events))
	}
}

func TestLearningGapRepairReplaysEventWithoutWorker(t *testing.T) {
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repo.Close() }()
	if err := repo.Append(context.Background(), contextstore.ContextItem{ID: "memory-1", Kind: contextstore.ContextPattern, Content: "safe procedure", Scope: contextstore.Scope{ProjectID: "project"}, Lifecycle: contextstore.LifecycleConfirmed}); err != nil {
		t.Fatal(err)
	}
	store, err := NewEventStore(workspace, "run", "session")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	policy := agent.DefaultMemoryLearningPolicy()
	policy.Mode = agent.MemoryLearningObserve
	c := &Coordinator{eventStore: store, contextRepo: repo, session: &TeamSession{Workspace: workspace, Config: agent.TeamConfig{MemoryLearning: policy}}, sessionData: NewSession()}
	payload, _ := json.Marshal(memoryEventPayload{SchemaVersion: 1, RetrievalID: "retrieval-1", ContextItemID: "memory-1", PolicyVersion: policy.PolicyVersion})
	const key = "memory:retrieved:run:task:1:retrieval-1:memory-1"
	store.syncFile = func() error { return errors.New("uncertain sync") }
	if _, err := c.emitEventOnce(key, RunEvent{Type: "memory_retrieved", Actor: "runtime", TaskID: "task", Attempt: 1, Payload: payload}); err == nil {
		t.Fatal("expected injected append failure")
	}
	c.repairMemoryLearningGaps()
	if len(c.sessionData.LearningGaps) != 1 || c.sessionData.LearningGaps[0].PendingRepair {
		t.Fatalf("learning gaps = %+v", c.sessionData.LearningGaps)
	}
	aggregate, err := repo.ExperienceAggregate(context.Background(), "memory-1", policy.PolicyVersion)
	if err != nil {
		t.Fatal(err)
	}
	if aggregate.ExposureCount != 1 {
		t.Fatalf("repaired exposure count = %d, want 1", aggregate.ExposureCount)
	}
}

func TestLearningGapCheckpointSurvivesRestart(t *testing.T) {
	workspace := t.TempDir()
	repo, err := contextstore.OpenSQLite(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = repo.Close() }()
	if err := repo.Append(context.Background(), contextstore.ContextItem{ID: "memory-1", Kind: contextstore.ContextPattern, Content: "safe procedure", Scope: contextstore.Scope{ProjectID: "project"}, Lifecycle: contextstore.LifecycleConfirmed}); err != nil {
		t.Fatal(err)
	}
	store, err := NewEventStore(workspace, "run", "session")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	policy := agent.DefaultMemoryLearningPolicy()
	policy.Mode = agent.MemoryLearningObserve
	c := &Coordinator{eventStore: store, contextRepo: repo, session: &TeamSession{Workspace: workspace, Config: agent.TeamConfig{MemoryLearning: policy}}, sessionData: NewSession()}
	payload, _ := json.Marshal(memoryEventPayload{SchemaVersion: 1, RetrievalID: "retrieval-1", ContextItemID: "memory-1", PolicyVersion: policy.PolicyVersion})
	const key = "memory:retrieved:run:task:1:retrieval-1:memory-1"
	// Inject a sync failure after the manifest checkpoint: the exposure append
	// fails while the manifest itself is already durable.
	store.syncFile = func() error { return errors.New("uncertain sync") }
	if _, err := c.emitEventOnce(key, RunEvent{Type: "memory_retrieved", Actor: "runtime", TaskID: "task", Attempt: 1, Payload: payload}); err == nil {
		t.Fatal("expected injected append failure")
	}

	// The gap must be durably checkpointed before the failed emission path
	// returns, so a crash cannot lose the repair record and its event.
	reloaded := LoadSession(workspace)
	if reloaded == nil || len(reloaded.LearningGaps) != 1 || !reloaded.LearningGaps[0].PendingRepair || reloaded.LearningGaps[0].IdempotencyKey != key {
		t.Fatalf("learning gap not durably checkpointed: %+v", reloaded)
	}

	// Simulate a crash: reconstruct the coordinator from session.json with a
	// fresh event store (sync restored). No worker run is involved.
	reopened, err := NewEventStore(workspace, "run", "session")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reopened.Close() }()
	restarted := &Coordinator{eventStore: reopened, contextRepo: repo, session: &TeamSession{Workspace: workspace, Config: agent.TeamConfig{MemoryLearning: policy}}, sessionData: reloaded}
	restarted.hydrateEmittedEventKeys()
	restarted.repairMemoryLearningGaps()

	// The retry must emit/reduce the original event without a worker run.
	events, err := reopened.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range events {
		if event.IdempotencyKey == key {
			found = true
		}
	}
	if !found {
		t.Fatal("repair did not emit the original event")
	}
	aggregate, err := repo.ExperienceAggregate(context.Background(), "memory-1", policy.PolicyVersion)
	if err != nil {
		t.Fatal(err)
	}
	if aggregate.ExposureCount != 1 {
		t.Fatalf("repaired exposure count = %d, want 1", aggregate.ExposureCount)
	}
	if len(reloaded.LearningGaps) != 1 || reloaded.LearningGaps[0].PendingRepair {
		t.Fatalf("gap not marked repaired after reopen: %+v", reloaded.LearningGaps)
	}
}

func TestEmitEventOnceSurvivesRestart(t *testing.T) {
	dir := t.TempDir()
	es, err := NewEventStore(dir, "run-once", "session-once")
	if err != nil {
		t.Fatal(err)
	}
	c := &Coordinator{eventStore: es, emittedTaskTransitions: make(map[string]bool)}
	event := RunEvent{Type: "memory_retrieved", Actor: "runtime", TaskID: "task-1"}
	if emitted, err := c.emitEventOnce("memory:retrieved:run-once:task-1:1:r1:context:1", event); err != nil || !emitted {
		t.Fatalf("first emit = %v, %v", emitted, err)
	}
	if emitted, err := c.emitEventOnce("memory:retrieved:run-once:task-1:1:r1:context:1", event); err != nil || emitted {
		t.Fatalf("duplicate emit = %v, %v", emitted, err)
	}
	if err := es.Close(); err != nil {
		t.Fatal(err)
	}

	reopened, err := OpenEventStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restarted := &Coordinator{eventStore: reopened}
	restarted.hydrateEmittedEventKeys()
	if emitted, err := restarted.emitEventOnce("memory:retrieved:run-once:task-1:1:r1:context:1", event); err != nil || emitted {
		t.Fatalf("restart duplicate emit = %v, %v", emitted, err)
	}
	events, err := reopened.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("event count = %d, want 1", len(events))
	}
}

func TestEventStoreAppendAndVerifyHashChain(t *testing.T) {
	dir := t.TempDir()
	es, err := NewEventStore(dir, "run-101", "session-202")
	if err != nil {
		t.Fatalf("NewEventStore failed: %v", err)
	}
	defer es.Close()

	payload1, _ := json.Marshal(map[string]string{"goal": "Build feature X"})
	e1 := RunEvent{
		Type:    "run_started",
		Actor:   "user",
		Payload: payload1,
	}
	if err := es.Append(e1); err != nil {
		t.Fatalf("Append e1 failed: %v", err)
	}

	payload2, _ := json.Marshal(map[string]string{"task_id": "task-1", "desc": "Write code"})
	e2 := RunEvent{
		Type:    "task_created",
		Actor:   "coordinator",
		TaskID:  "task-1",
		Payload: payload2,
	}
	if err := es.Append(e2); err != nil {
		t.Fatalf("Append e2 failed: %v", err)
	}

	events, err := es.ReadEvents()
	if err != nil {
		t.Fatalf("ReadEvents failed: %v", err)
	}

	if len(events) != 2 {
		t.Fatalf("expected 2 events, got %d", len(events))
	}

	if err := es.VerifyHashChain(); err != nil {
		t.Errorf("VerifyHashChain failed: %v", err)
	}
}

func TestEventStoreTamperDetection(t *testing.T) {
	dir := t.TempDir()
	es, err := NewEventStore(dir, "run-101", "session-202")
	if err != nil {
		t.Fatal(err)
	}
	_ = es.Append(RunEvent{Type: "run_started", Actor: "user"})
	_ = es.Append(RunEvent{Type: "run_finished", Actor: "coordinator", Payload: []byte(`{"outcome":"success"}`)})
	_ = es.Close()

	// Tamper with event log file directly
	path := filepath.Join(dir, logsDir, eventStoreFile)
	data, _ := os.ReadFile(path)
	tampered := bytes.Replace(data, []byte("run_started"), []byte("run_hacked"), 1)
	_ = os.WriteFile(path, tampered, 0o644)

	es2, err := OpenEventStore(dir)
	if err == nil {
		defer es2.Close()
		if err := es2.VerifyHashChain(); err == nil {
			t.Errorf("expected hash chain error on tampered file, got nil")
		}
	}
}

func TestCoordinatorStartupUsesSingleStrictEventStoreScan(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-history", "session-history")
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 512; i++ {
		if err := store.Append(RunEvent{
			Type:    "task_progress",
			Actor:   "worker",
			TaskID:  fmt.Sprintf("task-%d", i),
			Payload: []byte(`{"progress":"` + strings.Repeat("x", 1024) + `"}`),
		}); err != nil {
			t.Fatalf("append history event %d: %v", i, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	c, err := NewCoordinator(&TeamSession{
		Workspace: workspace,
		Config:    agent.TeamConfig{Name: "named-team"},
	}, "", "", nil, nil, nil, RoleModels{}, 0, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.PrepareContextPreflight(); err != nil {
		t.Fatal(err)
	}
	defer c.CloseContextPreflight()
	if c.eventStore == nil {
		t.Fatal("startup did not initialize the event store")
	}
	if c.eventStore.scanCount != 1 {
		t.Fatalf("event-store scans during startup = %d, want 1", c.eventStore.scanCount)
	}
	if c.eventStore.cacheHitCount == 0 {
		t.Fatal("coordinator startup did not read the validated event cache")
	}
}

func TestEventStoreReadEventsUsesValidatedCacheAndCopiesPayload(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-cache", "session-cache")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if err := store.Append(RunEvent{Type: "task_progress", Actor: "worker", Payload: []byte(`{"status":"original"}`)}); err != nil {
		t.Fatal(err)
	}
	if store.scanCount != 1 {
		t.Fatalf("initial scans = %d, want 1", store.scanCount)
	}

	first, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 1 {
		t.Fatalf("cached event count = %d, want 1", len(first))
	}
	first[0].Payload[0] = 'X'
	if store.scanCount != 1 {
		t.Fatalf("ReadEvents triggered a rescan: %d scans", store.scanCount)
	}
	if store.cacheHitCount != 1 {
		t.Fatalf("cache hits = %d, want 1", store.cacheHitCount)
	}

	second, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	if got := string(second[0].Payload); got != `{"status":"original"}` {
		t.Fatalf("cached payload = %s, want original payload", got)
	}
	if store.cacheHitCount != 2 {
		t.Fatalf("cache hits after second read = %d, want 2", store.cacheHitCount)
	}
}

func TestEventStoreVerifyRefreshDetectsTamperAfterOpen(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-refresh", "session-refresh")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if err := store.Append(RunEvent{Type: "task_progress", Actor: "worker", Payload: []byte(`{"status":"original"}`)}); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(workspace, logsDir, eventStoreFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.Replace(data, []byte("task_progress"), []byte("task_tampered"), 1)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := store.VerifyHashChain(); err == nil {
		t.Fatal("VerifyHashChain accepted tampered durable JSONL")
	}
	if _, err := store.ReadEvents(); err == nil {
		t.Fatal("ReadEvents exposed the pre-tamper cache after failed refresh")
	}
}

func TestEventStoreFailedRefreshInvalidatesStateAndBlocksAppend(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-refresh-failure", "session-refresh-failure")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if err := store.Append(RunEvent{Type: "run_started", Actor: "user", IdempotencyKey: "started"}); err != nil {
		t.Fatal(err)
	}
	originalPath := store.path
	store.path = filepath.Join(workspace, "missing-parent", eventStoreFile)

	if err := store.VerifyHashChain(); err == nil {
		t.Fatal("VerifyHashChain unexpectedly recovered a missing refresh path")
	}
	if _, err := store.ReadEvents(); err == nil {
		t.Fatal("ReadEvents exposed stale cache after refresh failure")
	}
	if err := store.Append(RunEvent{Type: "task_created", Actor: "coordinator"}); err == nil {
		t.Fatal("Append used stale chain state after refresh failure")
	}

	data, err := os.ReadFile(originalPath)
	if err != nil {
		t.Fatal(err)
	}
	if count := bytes.Count(data, []byte("\n")); count != 1 {
		t.Fatalf("original durable log gained an append after refresh failure: %d lines", count)
	}
}

func TestEventStoreFailedRefreshDoesNotPublishPartialState(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-partial", "session-partial")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if err := store.Append(RunEvent{Type: "run_started", Actor: "user", IdempotencyKey: "first"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(RunEvent{Type: "task_created", Actor: "coordinator", IdempotencyKey: "second"}); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(workspace, logsDir, eventStoreFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.Replace(data, []byte("task_created"), []byte("task_corrupt"), 1)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := store.VerifyHashChain(); err == nil {
		t.Fatal("VerifyHashChain accepted corruption after a valid prefix")
	}
	if store.lastEventID != "" || store.lastHash != "" || store.sequence != 0 || len(store.cachedEvents) != 0 || len(store.idempotencyKeys) != 0 {
		t.Fatalf("failed refresh published partial state: head=%q/%q sequence=%d events=%d keys=%d", store.lastEventID, store.lastHash, store.sequence, len(store.cachedEvents), len(store.idempotencyKeys))
	}
}

func TestEventStoreSuccessfulRefreshSwapsCacheAndAppendHandle(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-replacement", "session-replacement")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	if err := store.Append(RunEvent{ID: "old-event", Type: "run_started", Actor: "user"}); err != nil {
		t.Fatal(err)
	}

	replacementWorkspace := t.TempDir()
	replacement, err := NewEventStore(replacementWorkspace, "run-replacement", "session-replacement")
	if err != nil {
		t.Fatal(err)
	}
	if err := replacement.Append(RunEvent{ID: "new-event", Type: "run_started", Actor: "user"}); err != nil {
		t.Fatal(err)
	}
	if err := replacement.Close(); err != nil {
		t.Fatal(err)
	}
	replacementPath := filepath.Join(replacementWorkspace, logsDir, eventStoreFile)
	targetPath := filepath.Join(workspace, logsDir, eventStoreFile)
	if err := os.Rename(replacementPath, targetPath); err != nil {
		t.Fatal(err)
	}

	if err := store.VerifyHashChain(); err != nil {
		t.Fatalf("refresh replacement: %v", err)
	}
	events, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].ID != "new-event" {
		t.Fatalf("refreshed cache = %#v, want replacement event", events)
	}

	durable, err := store.AppendPersisted(RunEvent{ID: "after-refresh", Type: "task_progress", Actor: "worker", TaskID: "task-after-refresh", Payload: []byte(`{"progress":"after refresh"}`)})
	if err != nil {
		t.Fatalf("append after refresh: %v", err)
	}
	if durable.PreviousID != "new-event" {
		t.Fatalf("append previous_id = %q, want refreshed chain head", durable.PreviousID)
	}
	if err := store.VerifyHashChain(); err != nil {
		t.Fatalf("verify appended refreshed chain: %v", err)
	}
	events, err = store.ReadEvents()
	if err != nil {
		t.Fatalf("read appended refreshed chain: %v", err)
	}
	if len(events) != 2 || events[0].ID != "new-event" || events[1].ID != "after-refresh" {
		t.Fatalf("durable refreshed events = %#v, want new-event followed by after-refresh", events)
	}
}

func TestEventStoreRejectsNonEmptyFirstLink(t *testing.T) {
	cases := []struct {
		name  string
		event RunEvent
	}{
		{name: "previous_id", event: RunEvent{PreviousID: "prior-event"}},
		{name: "previous_hash", event: RunEvent{PreviousHash: "prior-hash"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			workspace := t.TempDir()
			event := tc.event
			event.SchemaVersion = eventStoreSchemaVersion
			event.ID = "first-event"
			event.RunID = "run-first-link"
			event.SessionID = "session-first-link"
			event.Type = "run_started"
			event.Actor = "user"
			event.Timestamp = "2026-01-01T00:00:00Z"
			event.Payload = json.RawMessage(`{"started":true}`)
			event.Hash = ComputeEventHash(event.PreviousHash, event.ID, event.Type, event.Timestamp, event.Payload)
			path := filepath.Join(workspace, logsDir, eventStoreFile)
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			data, err := json.Marshal(event)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, append(data, '\n'), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := NewEventStore(workspace, "run-first-link", "session-first-link"); err == nil {
				t.Fatal("NewEventStore accepted a non-empty first link")
			}
		})
	}
}

func TestCoordinatorStartupMarksCorruptEventStoreForRecovery(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-corrupt", "session-corrupt")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(RunEvent{Type: "run_started", Actor: "user"}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(workspace, logsDir, eventStoreFile)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	data = bytes.Replace(data, []byte("run_started"), []byte("run_tampered"), 1)
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	c, err := NewCoordinator(&TeamSession{
		Workspace: workspace,
		Config:    agent.TeamConfig{Name: "named-team"},
	}, "", "", nil, nil, nil, RoleModels{}, 0, false, false, false, nil, nil, nil, false, "", false, false, nil, false, false)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.PrepareContextPreflight(); err == nil {
		t.Fatal("corrupt event store was accepted during startup")
	}
	if c.eventStore != nil {
		t.Fatal("corrupt event store remained attached")
	}
	if c.SessionData() == nil || !c.SessionData().RecoveryRequired {
		t.Fatalf("corrupt event store did not mark recovery required: %#v", c.SessionData())
	}
	if c.contextRepo != nil {
		_ = c.contextRepo.Close()
	}
}

func TestWP14_RejectTerminalEventsWithEmptyPayload(t *testing.T) {
	dir := t.TempDir()
	es, err := NewEventStore(dir, "run-101", "session-202")
	if err != nil {
		t.Fatal(err)
	}
	defer es.Close()

	// 1. All empty payload variations (nil, empty string, whitespace, null, {}, { }, {\n}, [], [ ]) for terminal event must be rejected
	emptyCases := [][]byte{
		nil,
		[]byte(""),
		[]byte("   "),
		[]byte("null"),
		[]byte("{}"),
		[]byte("{ }"),
		[]byte("{\n  \t\n}"),
		[]byte("[]"),
		[]byte("[ ]"),
		[]byte("[\n  \t\n]"),
	}
	for _, tc := range emptyCases {
		if err := es.Append(RunEvent{Type: "run_finished", Actor: "coordinator", Payload: tc}); err == nil {
			t.Errorf("expected error appending run_finished with empty payload %q, got nil", string(tc))
		}
	}

	// 2. Non-terminal event with nil payload is allowed
	if err := es.Append(RunEvent{Type: "run_started", Actor: "user", Payload: nil}); err != nil {
		t.Errorf("unexpected error appending non-terminal run_started with nil payload: %v", err)
	}

	// 3. Terminal event with valid non-empty payload must succeed
	if err := es.Append(RunEvent{Type: "run_finished", Actor: "coordinator", Payload: []byte(`{"outcome":"success","goal_satisfied":true}`)}); err != nil {
		t.Errorf("unexpected error appending run_finished with valid payload: %v", err)
	}

	// 4. Coordinator.emitEvent with empty payload must fail and increment dualWriteFailures
	coord := &Coordinator{
		eventStore: es,
	}
	if err := coord.emitEvent("run_finished", "coordinator", "", nil); err == nil {
		t.Error("expected emitEvent error for run_finished with nil payload, got nil")
	}
	if coord.DualWriteFailures() != 1 {
		t.Errorf("DualWriteFailures = %d, want 1", coord.DualWriteFailures())
	}
}
