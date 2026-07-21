# HF-STATE-001 / HF-PR-104 Quality Fixes & Refinements Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Address code quality, dual-write completeness, deduplication of task events, error handling, roadmap file placement, and event store schema consistency for `HF-STATE-001`.

**Architecture:**
1. **Coordinator Session Dual-Write Wiring (Issue A):**
   - Add `c.addSessionUserMessage(content)` and `c.addSessionAssistantMessage(content)` to `Coordinator` (in `coordinator_eventstore.go`).
   - Replace all direct `c.sessionData.AddEntry` calls in `coordinator_run.go` (lines 350, 595, 628, 670, 697) with `c.addSessionUserMessage` / `c.addSessionAssistantMessage`.
   - Add end-to-end integration test (`TestCoordinatorRunEmitsSessionEvents`) that runs a coordinator workflow turn and verifies `user_message_added` and `assistant_message_added` events in `event_store.jsonl`.

2. **Discrete Task Events & Deduplication (Issue B & Minor Issue 1):**
   - Track emitted task state transitions in `Coordinator` (`emittedTaskTransitions map[string]bool`) protected by `mu`.
   - In `emitTaskEventsFromCheckpoint`, construct a transition key `taskID + ":" + status + ":" + retries`. Only emit when the key has not been seen.
   - Separate `TaskSkipped` (`task_skipped`) and `TaskBlocked` (`task_blocked`) into distinct event types, and update `ReduceToTodoList` accordingly.
   - Set `IdempotencyKey` on task events.
   - Clean up `ReduceToTodoList` filter logic (`if !strings.HasPrefix(e.Type, "task_") { continue }`).
   - Add unit test (`TestDeduplicatedTaskEventsEmission`) verifying that multiple checkpoints/status updates do NOT emit duplicate task completion events.

3. **Dual-Write Error Tracking & Metric (Issue C):**
   - Add `dualWriteFailures atomic.Int64` to `Coordinator` and `EventStore`.
   - Log warnings via `log.Printf` on append failures and increment the counter.

4. **EventStore RunID / SessionID Persistence on Open (Minor Issue 2):**
   - In `NewEventStore` / `OpenEventStore`, populate `es.runID` and `es.sessionID` from the last recorded event in existing files if they were passed as empty strings.

5. **Roadmap Location Refactoring (Issue D):**
   - Move `tmp/hufu-future-improvement-roadmap.md` to `docs/hufu-future-improvement-roadmap.md`.
   - Move `tmp/hufu-strict-verification-workflow-improvement.md` to `docs/hufu-strict-verification-workflow-improvement.md`.
   - Update file path references across documentation and tests.

---

### Task 1: Wire Session Message Dual-Write in Coordinator Run & Integration Test (Issue A)

**Files:**
- Modify: `internal/team/coordinator_eventstore.go`
- Modify: `internal/team/coordinator_run.go:350,595,628,670,697`
- Modify: `internal/team/event_store_integration_test.go`

- [ ] **Step 1: Add helper methods to `Coordinator` in `coordinator_eventstore.go`**

```go
func (c *Coordinator) addSessionUserMessage(content string) {
	if c == nil || c.sessionData == nil {
		return
	}
	RecordSessionUserMessage(c.sessionData, c.eventStore, content)
}

func (c *Coordinator) addSessionAssistantMessage(content string) {
	if c == nil || c.sessionData == nil {
		return
	}
	RecordSessionAssistantMessage(c.sessionData, c.eventStore, content)
}
```

- [ ] **Step 2: Replace direct `c.sessionData.AddEntry` calls in `coordinator_run.go`**

Replace lines 350, 595, 628, 670, 697 with `c.addSessionAssistantMessage` and `c.addSessionUserMessage`.

- [ ] **Step 3: Add end-to-end integration test in `event_store_integration_test.go`**

```go
func TestCoordinatorRunEmitsSessionEvents(t *testing.T) {
	dir := t.TempDir()
	teamSession := &TeamSession{Workspace: dir, Config: &agent.TeamConfig{Name: "test-team"}}
	coord := &Coordinator{
		session:     teamSession,
		sessionData: NewSession(),
		taskTracker: NewTaskTracker(),
	}
	coord.initEventStore()
	defer coord.EventStore().Close()

	coord.addSessionUserMessage("Analyze repository")
	coord.addSessionAssistantMessage("Task completed successfully")

	events, err := coord.EventStore().ReadEvents()
	if err != nil {
		t.Fatalf("ReadEvents error: %v", err)
	}

	hasUserMsg := false
	hasAssistantMsg := false
	for _, e := range events {
		if e.Type == "user_message_added" {
			hasUserMsg = true
		}
		if e.Type == "assistant_message_added" {
			hasAssistantMsg = true
		}
	}
	if !hasUserMsg || !hasAssistantMsg {
		t.Errorf("expected both user and assistant message events, got user=%v assistant=%v", hasUserMsg, hasAssistantMsg)
	}
}
```

- [ ] **Step 4: Run test to verify it passes**

`go test ./internal/team/ -run 'TestCoordinatorRunEmitsSessionEvents' -v`

---

### Task 2: Discrete Task Event Deduplication & Distinct Task Statuses (Issue B & Minor Issue 1)

**Files:**
- Modify: `internal/team/coordinator.go` (add `emittedTaskTransitions map[string]bool`)
- Modify: `internal/team/coordinator_eventstore.go` (`emitTaskEventsFromCheckpoint`)
- Modify: `internal/team/event_reducers.go` (`ReduceToTodoList`)
- Modify: `internal/team/event_store_integration_test.go` (add deduplication test)

- [ ] **Step 1: Write test verifying deduplicated task event emission**

```go
func TestDeduplicatedTaskEventsEmission(t *testing.T) {
	dir := t.TempDir()
	teamSession := &TeamSession{Workspace: dir}
	coord := &Coordinator{
		session:                 teamSession,
		sessionData:             NewSession(),
		taskTracker:             NewTaskTracker(),
		emittedTaskTransitions: make(map[string]bool),
	}

	coord.initEventStore()
	defer coord.EventStore().Close()

	items := coord.taskTracker.TodoList().AddBatch([]TodoSpec{
		{Agent: "worker1", Desc: "Task 1"},
	})
	coord.taskTracker.TodoList().UpdateStatus(items[0].ID, TaskDone, "completed")

	// Call saveCheckpoint twice
	coord.saveCheckpoint()
	coord.saveCheckpoint()

	events, err := coord.EventStore().ReadEvents()
	if err != nil {
		t.Fatal(err)
	}

	taskCompletedCount := 0
	for _, e := range events {
		if e.Type == "task_completed" && e.TaskID == items[0].ID {
			taskCompletedCount++
		}
	}

	if taskCompletedCount != 1 {
		t.Errorf("expected exactly 1 task_completed event, got %d", taskCompletedCount)
	}
}
```

- [ ] **Step 2: Implement transition deduplication & separate `task_skipped` vs `task_blocked`**

In `coordinator.go`: add `emittedTaskTransitions map[string]bool` to `Coordinator`.
In `coordinator_eventstore.go`:
```go
func (c *Coordinator) emitTaskEventsFromCheckpoint(tasks []*TodoItem) {
	if c == nil || c.eventStore == nil {
		return
	}
	c.mu.Lock()
	if c.emittedTaskTransitions == nil {
		c.emittedTaskTransitions = make(map[string]bool)
	}
	c.mu.Unlock()

	for _, item := range tasks {
		if item == nil {
			continue
		}
		var eventType string
		switch item.Status {
		case TaskPending:
			eventType = "task_created"
		case TaskInProgress:
			eventType = "task_started"
		case TaskDone:
			eventType = "task_completed"
		case TaskError:
			eventType = "task_failed"
		case TaskSkipped:
			eventType = "task_skipped"
		case TaskBlocked:
			eventType = "task_blocked"
		}
		if eventType == "" {
			continue
		}

		transitionKey := fmt.Sprintf("%s:%s:%d", item.ID, item.Status, item.Retries)
		c.mu.Lock()
		alreadyEmitted := c.emittedTaskTransitions[transitionKey]
		if !alreadyEmitted {
			c.emittedTaskTransitions[transitionKey] = true
		}
		c.mu.Unlock()

		if alreadyEmitted {
			continue
		}

		_ = c.eventStore.Append(RunEvent{
			Type:           eventType,
			Actor:          item.Agent,
			TaskID:         item.ID,
			IdempotencyKey: transitionKey,
			Payload: rawPayload(map[string]interface{}{
				"id":         item.ID,
				"desc":       item.Desc,
				"status":     string(item.Status),
				"output":     item.Output,
				"agent":      item.Agent,
				"depends_on": item.DependsOn,
			}),
		})
	}
}
```

In `event_reducers.go`:
```go
		switch e.Type {
		case "task_created":
			if payload.Status != "" {
				item.Status = TaskStatus(payload.Status)
			}
		case "task_started":
			item.Status = TaskInProgress
		case "task_completed":
			item.Status = TaskDone
			if payload.Output != "" {
				item.Output = payload.Output
			}
		case "task_failed":
			item.Status = TaskError
			if payload.Output != "" {
				item.Output = payload.Output
			}
		case "task_skipped":
			item.Status = TaskSkipped
		case "task_blocked":
			item.Status = TaskBlocked
		case "task_reset":
			item.Status = TaskPending
		}
```

- [ ] **Step 3: Run test to verify it passes**

`go test ./internal/team/ -run 'TestDeduplicatedTaskEventsEmission' -v`

---

### Task 3: Dual-Write Error Logging & Failures Counter (Issue C)

**Files:**
- Modify: `internal/team/event_store.go`
- Modify: `internal/team/coordinator_eventstore.go`

- [ ] **Step 1: Add `dualWriteFailures atomic.Int64` to `Coordinator` and `EventStore`**

In `EventStore`: add `failures atomic.Int64` and `Failures() int64`.
In `coordinator_eventstore.go`:
```go
func (c *Coordinator) emitEvent(eventType, actor, taskID string, payload map[string]interface{}) {
	if c == nil || c.eventStore == nil {
		return
	}
	var rawPayload json.RawMessage
	if payload != nil {
		data, err := json.Marshal(payload)
		if err == nil {
			rawPayload = data
		}
	}
	if err := c.eventStore.Append(RunEvent{
		Type:    eventType,
		Actor:   actor,
		TaskID:  taskID,
		Payload: rawPayload,
	}); err != nil {
		log.Printf("[WARN] dual-write event store append failed: %v", err)
		c.dualWriteFailures.Add(1)
	}
}
```

- [ ] **Step 2: Run test suite**

`go test ./internal/team/ -run 'TestEventStore' -v`

---

### Task 4: EventStore RunID / SessionID Persistence on Open (Minor Issue 2)

**Files:**
- Modify: `internal/team/event_store.go`
- Modify: `internal/team/event_store_test.go`

- [ ] **Step 1: Update `NewEventStore` to extract runID and sessionID from existing events if passed empty**

```go
	events, _ := es.ReadEvents()
	if len(events) > 0 {
		last := events[len(events)-1]
		es.lastEventID = last.ID
		es.lastHash = last.Hash
		es.sequence = len(events)
		if es.runID == "" && last.RunID != "" {
			es.runID = last.RunID
		}
		if es.sessionID == "" && last.SessionID != "" {
			es.sessionID = last.SessionID
		}
	}
```

- [ ] **Step 2: Add test verifying `OpenEventStore` retains runID/sessionID**

`go test ./internal/team/ -run 'TestEventStore' -v`

---

### Task 5: Relocate Roadmap Files to `docs/` (Issue D)

**Files:**
- Move: `tmp/hufu-future-improvement-roadmap.md` → `docs/hufu-future-improvement-roadmap.md`
- Move: `tmp/hufu-strict-verification-workflow-improvement.md` → `docs/hufu-strict-verification-workflow-improvement.md`
- Remove `tmp/` git tracking workaround.

- [ ] **Step 1: Move files using git mv**

```bash
git mv tmp/hufu-future-improvement-roadmap.md docs/hufu-future-improvement-roadmap.md
git mv tmp/hufu-strict-verification-workflow-improvement.md docs/hufu-strict-verification-workflow-improvement.md
```

- [ ] **Step 2: Update references in documentation & roadmap file headers**

---

### Task 6: Comprehensive Verification & Commit

- [ ] **Step 1: Run full test suite & lint**

```bash
go build ./cmd/hufu && go vet ./... && go test ./... -count=1
```

- [ ] **Step 2: Commit all fixes**

```bash
git commit -m "fix(team): resolve event store dual-write, deduplication, error logging, and roadmap location (HF-STATE-001)"
```
