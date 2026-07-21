# HF-STATE-001: Append-Only Event Store & Atomic Persistence Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement `HF-STATE-001` (and `HF-PR-104` + `HF-PR-002` foundation) to introduce an append-only JSONL Event Store with hash chain validation, dual-write telemetry, state reducers, and atomic snapshot write helpers.

**Architecture:**
1. Create `internal/team/atomic_write.go` providing `AtomicWriteFile` (temp file write, `Sync()`, atomic `Rename()`, directory `Sync()`) to ensure no corrupt partial files occur during session snapshotting or compaction.
2. Refactor existing `SaveSession`, `SaveSessionMD`, `SaveCompactionRecord` to use `AtomicWriteFile`, with comprehensive tests for crash/corrupt recovery.
3. Create `internal/team/event_store.go` implementing `RunEvent` struct (schema version, ID, previous_id, run_id, session_id, branch_id, task_id, attempt, actor, type, timestamp, idempotency_key, payload, previous_hash, hash) and `EventStore` JSONL logger with SHA-256 hash chaining.
4. Implement dual-write logic in `Coordinator` and `SessionData` to emit all workflow lifecycle events (`run_started`, `user_message_added`, `task_started`, `task_completed`, `compaction_created`, etc.) while retaining legacy files.
5. Build state reducers `ReduceToSessionData` and `ReduceToTodoList` in `internal/team/event_reducers.go` to reconstruct session context and task state directly from `EventStore` logs, verified with unit tests.
6. Update documentation `docs/hufu-future-improvement-roadmap.md` marking `HF-STATE-001`, `HF-PR-104`, and `HF-PR-002` as `🟡 IMPLEMENTED (PENDING REVIEW)`.

**Tech Stack:** Go 1.26.2, standard library (`os`, `sync`, `crypto/sha256`, `encoding/json`, `path/filepath`), `internal/team`.

## Global Constraints

- Go module path: `github.com/anomalyco/hufu`
- Must maintain 100% backward compatibility with legacy `session.json`, `chat_history.md`, and `task_journal.jsonl` files (dual-write phase).
- All tests must pass: `go test ./...`
- Verification commands must be executed before marking tasks done.

---

### Task 1: Atomic Persistence Helper (HF-PR-002)

**Files:**
- Create: `internal/team/atomic_write.go`
- Create: `internal/team/atomic_write_test.go`
- Modify: `internal/team/session.go:57-67`
- Modify: `internal/team/session_md.go`
- Modify: `internal/team/compaction.go`

**Interfaces:**
- Produces: `AtomicWriteFile(path string, data []byte, perm os.FileMode) error`
- Produces: `SyncDir(dir string) error`

- [ ] **Step 1: Write failing unit test for `AtomicWriteFile` and corrupt session recovery**

```go
// internal/team/atomic_write_test.go
package team

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAtomicWriteFileSuccess(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "test.json")
	data := []byte(`{"hello":"world"}`)

	if err := AtomicWriteFile(target, data, 0o644); err != nil {
		t.Fatalf("AtomicWriteFile failed: %v", err)
	}

	got, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("ReadFile failed: %v", err)
	}
	if string(got) != string(data) {
		t.Errorf("got %s, want %s", got, data)
	}
}

func TestLoadSessionCrashRecovery(t *testing.T) {
	dir := t.TempDir()
	// Create corrupt session file
	target := filepath.Join(dir, sessionFile)
	if err := os.WriteFile(target, []byte(`{"created_at":`), 0o644); err != nil {
		t.Fatal(err)
	}
	// Create leftover temp file
	tmpFile := target + ".tmp.12345"
	_ = os.WriteFile(tmpFile, []byte(`temp data`), 0o644)

	session := LoadSession(dir)
	if session != nil {
		t.Errorf("expected nil session for corrupt file, got %+v", session)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/team/ -run 'TestAtomicWriteFile|TestLoadSessionCrash' -v`
Expected: FAIL due to missing `AtomicWriteFile`.

- [ ] **Step 3: Implement `AtomicWriteFile` in `internal/team/atomic_write.go` and refactor `SaveSession`**

```go
// internal/team/atomic_write.go
package team

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
)

func SyncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

func AtomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("atomic write mkdir: %w", err)
	}

	randBytes := make([]byte, 4)
	_, _ = rand.Read(randBytes)
	tmpPath := fmt.Sprintf("%s.tmp.%s", path, hex.EncodeToString(randBytes))

	f, err := os.OpenFile(tmpPath, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return fmt.Errorf("atomic write open temp: %w", err)
	}

	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("atomic write write temp: %w", err)
	}

	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(tmpPath)
		return fmt.Errorf("atomic write sync temp: %w", err)
	}

	if err := f.Close(); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("atomic write close temp: %w", err)
	}

	if err := os.Rename(tmpPath, path); err != nil {
		_ = os.Remove(tmpPath)
		return fmt.Errorf("atomic write rename: %w", err)
	}

	_ = SyncDir(dir)
	return nil
}
```

Update `SaveSession` in `session.go`:
```go
func SaveSession(workspace string, session *SessionData) error {
	if session == nil {
		return errors.New("session is nil")
	}
	session.UpdatedAt = time.Now().Format(time.RFC3339)
	data, err := json.MarshalIndent(session, "", "  ")
	if err != nil {
		return err
	}
	return AtomicWriteFile(filepath.Join(workspace, sessionFile), data, 0o644)
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/team/ -run 'TestAtomicWriteFile|TestLoadSessionCrash' -v`
Expected: PASS.

- [ ] **Step 5: Commit Task 1**

```bash
git add internal/team/atomic_write.go internal/team/atomic_write_test.go internal/team/session.go
git commit -m "feat(team): add atomic write helper for session persistence (HF-PR-002)"
```

---

### Task 2: Append-Only Event Store Schema & Hash Chain Logger (HF-PR-104 Part 1)

**Files:**
- Create: `internal/team/event_store.go`
- Create: `internal/team/event_store_test.go`

**Interfaces:**
- Produces: `RunEvent` struct
- Produces: `EventStore` struct with `Append(event RunEvent) error`, `ReadEvents() ([]RunEvent, error)`, `VerifyHashChain() error`

- [ ] **Step 1: Write failing unit test for `EventStore` append, read, and hash chain validation**

```go
// internal/team/event_store_test.go
package team

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

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
	_ = es.Append(RunEvent{Type: "run_finished", Actor: "coordinator"})
	_ = es.Close()

	// Tamper with event log file directly
	path := filepath.Join(dir, logsDir, eventStoreFile)
	data, _ := os.ReadFile(path)
	tampered := bytes.Replace(data, []byte("run_started"), []byte("run_hacked"), 1)
	_ = os.WriteFile(path, tampered, 0o644)

	es2, err := OpenEventStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer es2.Close()

	if err := es2.VerifyHashChain(); err == nil {
		t.Errorf("expected hash chain error on tampered file, got nil")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/team/ -run 'TestEventStore' -v`
Expected: FAIL due to missing `NewEventStore`.

- [ ] **Step 3: Implement `RunEvent` and `EventStore` in `internal/team/event_store.go`**

```go
// internal/team/event_store.go
package team

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

const eventStoreFile = "event_store.jsonl"
const eventStoreSchemaVersion = 1

type RunEvent struct {
	SchemaVersion  int             `json:"schema_version"`
	ID             string          `json:"id"`
	PreviousID     string          `json:"previous_id,omitempty"`
	RunID          string          `json:"run_id"`
	SessionID      string          `json:"session_id"`
	BranchID       string          `json:"branch_id,omitempty"`
	TaskID         string          `json:"task_id,omitempty"`
	Attempt        int             `json:"attempt,omitempty"`
	Actor          string          `json:"actor"`
	Type           string          `json:"type"`
	Timestamp      string          `json:"timestamp"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
	Payload        json.RawMessage `json:"payload,omitempty"`
	PreviousHash   string          `json:"previous_hash,omitempty"`
	Hash           string          `json:"hash,omitempty"`
}

type EventStore struct {
	mu           sync.Mutex
	f            *os.File
	path         string
	runID        string
	sessionID    string
	lastEventID  string
	lastHash     string
	sequence     int
}

func ComputeEventHash(prevHash, id, eventType, timestamp string, payload json.RawMessage) string {
	h := sha256.New()
	_, _ = h.Write([]byte(prevHash))
	_, _ = h.Write([]byte(id))
	_, _ = h.Write([]byte(eventType))
	_, _ = h.Write([]byte(timestamp))
	_, _ = h.Write(payload)
	return hex.EncodeToString(h.Sum(nil))
}

func NewEventStore(workspace, runID, sessionID string) (*EventStore, error) {
	if workspace == "" {
		return nil, fmt.Errorf("new event store: empty workspace")
	}
	path := filepath.Join(workspace, logsDir, eventStoreFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("new event store mkdir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("new event store open: %w", err)
	}

	es := &EventStore{
		f:         f,
		path:      path,
		runID:     runID,
		sessionID: sessionID,
	}

	// Read existing events to initialize lastEventID, lastHash, sequence
	events, _ := es.ReadEvents()
	if len(events) > 0 {
		last := events[len(events)-1]
		es.lastEventID = last.ID
		es.lastHash = last.Hash
		es.sequence = len(events)
	}

	return es, nil
}

func OpenEventStore(workspace string) (*EventStore, error) {
	return NewEventStore(workspace, "", "")
}

func generateEventID() string {
	buf := make([]byte, 8)
	_, _ = rand.Read(buf)
	return fmt.Sprintf("evt-%s-%s", time.Now().UTC().Format("20060102150405"), hex.EncodeToString(buf))
}

func (es *EventStore) Append(event RunEvent) error {
	es.mu.Lock()
	defer es.mu.Unlock()

	if es.f == nil {
		return fmt.Errorf("event store closed")
	}

	es.sequence++
	if event.SchemaVersion == 0 {
		event.SchemaVersion = eventStoreSchemaVersion
	}
	if event.ID == "" {
		event.ID = generateEventID()
	}
	if event.RunID == "" {
		event.RunID = es.runID
	}
	if event.SessionID == "" {
		event.SessionID = es.sessionID
	}
	if event.Timestamp == "" {
		event.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	}

	event.PreviousID = es.lastEventID
	event.PreviousHash = es.lastHash
	event.Hash = ComputeEventHash(event.PreviousHash, event.ID, event.Type, event.Timestamp, event.Payload)

	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal run event: %w", err)
	}

	if _, err := es.f.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write run event: %w", err)
	}

	_ = es.f.Sync()

	es.lastEventID = event.ID
	es.lastHash = event.Hash
	return nil
}

func (es *EventStore) ReadEvents() ([]RunEvent, error) {
	if es.path == "" {
		return nil, nil
	}
	f, err := os.Open(es.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var events []RunEvent
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var e RunEvent
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		events = append(events, e)
	}
	return events, sc.Err()
}

func (es *EventStore) VerifyHashChain() error {
	events, err := es.ReadEvents()
	if err != nil {
		return fmt.Errorf("read events for verification: %w", err)
	}
	var prevHash string
	var prevID string
	for i, e := range events {
		if i > 0 {
			if e.PreviousID != prevID {
				return fmt.Errorf("event %d (%s) previous_id mismatch: got %s, want %s", i, e.ID, e.PreviousID, prevID)
			}
			if e.PreviousHash != prevHash {
				return fmt.Errorf("event %d (%s) previous_hash mismatch: got %s, want %s", i, e.ID, e.PreviousHash, prevHash)
			}
		}
		expectedHash := ComputeEventHash(e.PreviousHash, e.ID, e.Type, e.Timestamp, e.Payload)
		if e.Hash != expectedHash {
			return fmt.Errorf("event %d (%s) hash invalid: got %s, calculated %s", i, e.ID, e.Hash, expectedHash)
		}
		prevHash = e.Hash
		prevID = e.ID
	}
	return nil
}

func (es *EventStore) Close() error {
	es.mu.Lock()
	defer es.mu.Unlock()
	if es.f != nil {
		err := es.f.Close()
		es.f = nil
		return err
	}
	return nil
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/team/ -run 'TestEventStore' -v`
Expected: PASS.

- [ ] **Step 5: Commit Task 2**

```bash
git add internal/team/event_store.go internal/team/event_store_test.go
git commit -m "feat(team): add append-only event store with hash chain validation (HF-PR-104)"
```

---

### Task 3: State Reducers for Rebuilding Session & Task State (HF-PR-104 Part 2)

**Files:**
- Create: `internal/team/event_reducers.go`
- Create: `internal/team/event_reducers_test.go`

**Interfaces:**
- Produces: `ReduceToSessionData(events []RunEvent) *SessionData`
- Produces: `ReduceToTodoList(events []RunEvent) []*TodoItem`

- [ ] **Step 1: Write failing unit test for state reducers**

```go
// internal/team/event_reducers_test.go
package team

import (
	"encoding/json"
	"testing"
)

func TestReducersReconstructState(t *testing.T) {
	var events []RunEvent

	p1, _ := json.Marshal(map[string]string{"role": "user", "content": "Build website"})
	events = append(events, RunEvent{Type: "user_message_added", Payload: p1, Timestamp: "2026-07-21T06:00:00Z"})

	p2, _ := json.Marshal(map[string]string{"role": "assistant", "content": "Starting build"})
	events = append(events, RunEvent{Type: "assistant_message_added", Payload: p2, Timestamp: "2026-07-21T06:01:00Z"})

	p3, _ := json.Marshal(map[string]interface{}{"id": "1", "description": "Create HTML", "status": "pending"})
	events = append(events, RunEvent{Type: "task_created", TaskID: "1", Payload: p3})

	p4, _ := json.Marshal(map[string]interface{}{"id": "1", "status": "in_progress"})
	events = append(events, RunEvent{Type: "task_started", TaskID: "1", Payload: p4})

	p5, _ := json.Marshal(map[string]interface{}{"id": "1", "status": "done", "output": "HTML created"})
	events = append(events, RunEvent{Type: "task_completed", TaskID: "1", Payload: p5})

	session := ReduceToSessionData(events)
	if len(session.Entries) != 2 {
		t.Fatalf("expected 2 session entries, got %d", len(session.Entries))
	}
	if session.Entries[0].Role != "user" || session.Entries[0].Content != "Build website" {
		t.Errorf("unexpected entry 0: %+v", session.Entries[0])
	}

	todos := ReduceToTodoList(events)
	if len(todos) != 1 {
		t.Fatalf("expected 1 todo item, got %d", len(todos))
	}
	if todos[0].ID != "1" || todos[0].Status != TodoDone || todos[0].Output != "HTML created" {
		t.Errorf("unexpected todo item: %+v", todos[0])
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/team/ -run 'TestReducers' -v`
Expected: FAIL due to missing `ReduceToSessionData`.

- [ ] **Step 3: Implement `ReduceToSessionData` and `ReduceToTodoList` in `internal/team/event_reducers.go`**

```go
// internal/team/event_reducers.go
package team

import (
	"encoding/json"
	"strings"
)

func ReduceToSessionData(events []RunEvent) *SessionData {
	session := NewSession()
	for _, e := range events {
		switch e.Type {
		case "run_started":
			if session.CreatedAt == "" && e.Timestamp != "" {
				session.CreatedAt = e.Timestamp
			}
		case "user_message_added", "assistant_message_added":
			var payload struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			}
			if err := json.Unmarshal(e.Payload, &payload); err == nil && payload.Content != "" {
				role := payload.Role
				if role == "" {
					if strings.HasPrefix(e.Type, "user") {
						role = "user"
					} else {
						role = "assistant"
					}
				}
				session.AddEntry(role, payload.Content)
			}
		}
	}
	return session
}

func ReduceToTodoList(events []RunEvent) []*TodoItem {
	taskMap := make(map[string]*TodoItem)
	var taskOrder []string

	for _, e := range events {
		if e.TaskID == "" && !strings.HasPrefix(e.Type, "task_") {
			continue
		}

		var payload struct {
			ID          string   `json:"id"`
			Description string   `json:"description"`
			Status      string   `json:"status"`
			Output      string   `json:"output"`
			Agent       string   `json:"agent"`
			DependsOn   []string `json:"depends_on"`
		}
		_ = json.Unmarshal(e.Payload, &payload)

		taskID := e.TaskID
		if taskID == "" {
			taskID = payload.ID
		}
		if taskID == "" {
			continue
		}

		item, exists := taskMap[taskID]
		if !exists {
			item = &TodoItem{
				ID:          taskID,
				Description: payload.Description,
				Status:      TodoPending,
				Agent:       payload.Agent,
				DependsOn:   payload.DependsOn,
			}
			taskMap[taskID] = item
			taskOrder = append(taskOrder, taskID)
		}

		if payload.Description != "" {
			item.Description = payload.Description
		}
		if payload.Agent != "" {
			item.Agent = payload.Agent
		}
		if len(payload.DependsOn) > 0 {
			item.DependsOn = payload.DependsOn
		}

		switch e.Type {
		case "task_created":
			if payload.Status != "" {
				item.Status = TodoStatus(payload.Status)
			}
		case "task_started":
			item.Status = TodoInProgress
		case "task_completed":
			item.Status = TodoDone
			if payload.Output != "" {
				item.Output = payload.Output
			}
		case "task_failed":
			item.Status = TodoError
			if payload.Output != "" {
				item.Output = payload.Output
			}
		case "task_blocked":
			item.Status = TodoSkipped
		case "task_reset":
			item.Status = TodoPending
		}
	}

	result := make([]*TodoItem, 0, len(taskOrder))
	for _, id := range taskOrder {
		result = append(result, taskMap[id])
	}
	return result
}
```

- [ ] **Step 4: Run test to verify it passes**

Run: `go test ./internal/team/ -run 'TestReducers' -v`
Expected: PASS.

- [ ] **Step 5: Commit Task 3**

```bash
git add internal/team/event_reducers.go internal/team/event_reducers_test.go
git commit -m "feat(team): add state reducers for session and todo list from event store (HF-PR-104)"
```

---

### Task 4: Dual-Write Integration into Coordinator & Session (HF-PR-104 Part 3)

**Files:**
- Modify: `internal/team/coordinator.go`
- Modify: `internal/team/coordinator_run.go`
- Modify: `internal/team/coordinator_session.go`
- Create: `internal/team/event_store_integration_test.go`

**Interfaces:**
- Consumes: `EventStore`
- Produces: Dual-write event emission when adding messages, starting runs, executing tasks, and creating compaction.

- [ ] **Step 1: Write integration test verifying dual-write behavior**

```go
// internal/team/event_store_integration_test.go
package team

import (
	"os"
	"testing"
)

func TestDualWriteEventStoreIntegration(t *testing.T) {
	dir := t.TempDir()
	session := NewSession()
	session.Workspace = dir

	es, err := NewEventStore(dir, "run-test", "sess-test")
	if err != nil {
		t.Fatalf("NewEventStore error: %v", err)
	}
	defer es.Close()

	// Verify recording user message emits event AND updates session
	RecordSessionUserMessage(session, es, "Hello agent")
	if len(session.Entries) != 1 {
		t.Fatalf("expected 1 session entry, got %d", len(session.Entries))
	}

	events, err := es.ReadEvents()
	if err != nil {
		t.Fatalf("ReadEvents error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(events))
	}
	if events[0].Type != "user_message_added" {
		t.Errorf("unexpected event type: %s", events[0].Type)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `go test ./internal/team/ -run 'TestDualWriteEventStoreIntegration' -v`
Expected: FAIL due to missing helper.

- [ ] **Step 3: Implement helper `RecordSessionUserMessage` / `RecordSessionAssistantMessage` and attach `eventStore` to `Coordinator`**

In `internal/team/coordinator.go`:
Add `eventStore *EventStore` field to `Coordinator`.
Provide helper methods on `Coordinator` / `SessionData` for emitting events during:
1. `AddEntry`
2. `beginExecutionRun` / `Run`
3. `ExecuteTasks` (task creation, start, completion, failure)
4. `compaction`

- [ ] **Step 4: Run test to verify it passes and execute full test suite**

Run: `go test ./internal/team/ -run 'TestDualWrite|TestEvent' -v`
Expected: PASS.

- [ ] **Step 5: Commit Task 4**

```bash
git add internal/team/coordinator.go internal/team/coordinator_run.go internal/team/event_store_integration_test.go internal/team/event_reducers.go
git commit -m "feat(team): integrate dual-write event store into coordinator execution (HF-PR-104)"
```

---

### Task 5: Document Update & Verification (HF-STATE-001)

**Files:**
- Modify: `docs/hufu-future-improvement-roadmap.md`

- [ ] **Step 1: Update status in `docs/hufu-future-improvement-roadmap.md`**

Mark `HF-STATE-001` in Section 3 as `🟡 IMPLEMENTED (PENDING REVIEW)`.
Mark `HF-PR-104` and `HF-PR-002` as `🟡 IMPLEMENTED (PENDING REVIEW)`.

- [ ] **Step 2: Run full verification suite**

Run: `go build ./cmd/hufu && go vet ./... && go test ./... -count=1`
Expected: PASS with 0 errors.

- [ ] **Step 3: Commit documentation updates**

```bash
git add docs/hufu-future-improvement-roadmap.md
git commit -m "docs: mark HF-STATE-001 (HF-PR-104, HF-PR-002) as implemented pending review"
```
