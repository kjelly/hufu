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
   - Move `docs/hufu-future-improvement-roadmap.md` to `docs/hufu-future-improvement-roadmap.md`.
   - Move `tmp/hufu-strict-verification-workflow-improvement.md` to `docs/hufu-strict-verification-workflow-improvement.md`.
   - Update file path references across documentation and tests.

---

### Task 1: Wire Session Message Dual-Write in Coordinator Run & Integration Test (Issue A)

**Files:**
- Modify: `internal/team/coordinator_eventstore.go`
- Modify: `internal/team/coordinator_run.go:350,595,628,670,697`
- Modify: `internal/team/event_store_integration_test.go`

- [x] **Step 1: Add helper methods to `Coordinator` in `coordinator_eventstore.go`**

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

- [x] **Step 2: Replace direct `c.sessionData.AddEntry` calls in `coordinator_run.go`**

Replace lines 350, 595, 628, 670, 697 with `c.addSessionAssistantMessage` and `c.addSessionUserMessage`.

- [x] **Step 3: Add end-to-end integration test in `event_store_integration_test.go`**

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

- [x] **Step 4: Run test to verify it passes**

`go test ./internal/team/ -run 'TestCoordinatorRunEmitsSessionEvents' -v`

---

### Task 2: Discrete Task Event Deduplication & Distinct Task Statuses (Issue B & Minor Issue 1)

**Files:**
- Modify: `internal/team/coordinator.go` (add `emittedTaskTransitions map[string]bool`)
- Modify: `internal/team/coordinator_eventstore.go` (`emitTaskEventsFromCheckpoint`)
- Modify: `internal/team/event_reducers.go` (`ReduceToTodoList`)
- Modify: `internal/team/event_store_integration_test.go` (add deduplication test)

- [x] **Step 1: Write test verifying deduplicated task event emission**

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

- [x] **Step 2: Implement transition deduplication & separate `task_skipped` vs `task_blocked`**

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

- [x] **Step 3: Run test to verify it passes**

`go test ./internal/team/ -run 'TestDeduplicatedTaskEventsEmission' -v`

---

### Task 3: Dual-Write Error Logging & Failures Counter (Issue C)

**Files:**
- Modify: `internal/team/event_store.go`
- Modify: `internal/team/coordinator_eventstore.go`

- [x] **Step 1: Add `dualWriteFailures atomic.Int64` to `Coordinator` (EventStore 本體未加計數器)**

> **實際落地差異（2026-07-21 第二輪 Review）：** 原計畫宣稱「於 `EventStore` 及 `Coordinator` 加入計數器」，實際只在 `Coordinator` 加上 `dualWriteFailures atomic.Int64` 與 `DualWriteFailures() int64`；`EventStore` 本體無計數器。計數器目前僅在 `emitEvent`（run 事件）與 `emitTaskEventsFromCheckpoint`（task 事件）失敗時遞增；`RecordSessionUserMessage` / `RecordSessionAssistantMessage`（message 事件）失敗時只 `log.Printf` 警告、**未遞增計數器**。殘留 follow-up 見文末「Residual Follow-ups」§F1。

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
		log.Printf("warning: dual-write event emit failed for type %s: %v", eventType, err)
		c.dualWriteFailures.Add(1)
	}
}
```

- [x] **Step 2: Run test suite**

`go test ./internal/team/ -run 'TestEventStore' -v`

---

### Task 4: EventStore RunID / SessionID Persistence on Open (Minor Issue 2)

**Files:**
- Modify: `internal/team/event_store.go`
- Modify: `internal/team/event_store_test.go`

- [x] **Step 1: Update `NewEventStore` to extract runID and sessionID from existing events if passed empty**

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

- [x] **Step 2: Add test verifying `OpenEventStore` retains runID/sessionID**（未新增獨立測試）

> **實際落地差異（2026-07-21 第二輪 Review）：** 未為 runID/sessionID 繼承新增獨立測試；既有 `TestEventStoreTamperDetection` 會走 `OpenEventStore` 但未斷言繼承值。殘留 follow-up 見文末「Residual Follow-ups」§F3。

`go test ./internal/team/ -run 'TestEventStore' -v`

---

### Task 5: Relocate Roadmap Files to `docs/` (Issue D)

**Files:**
- Move: `docs/hufu-future-improvement-roadmap.md` → `docs/hufu-future-improvement-roadmap.md`
- Move: `tmp/hufu-strict-verification-workflow-improvement.md` → `docs/hufu-strict-verification-workflow-improvement.md`
- Remove `tmp/` git tracking workaround.

- [x] **Step 1: Move roadmap file using git mv**

> **實際落地差異（2026-07-21 第二輪 Review）：** 僅搬移 `hufu-future-improvement-roadmap.md`（`tmp/` → `docs/`，git rename `ee3333f`）。`hufu-strict-verification-workflow-improvement.md` 在工作區並不存在，未搬移；`tmp/` 下仍殘留一份 gitignore 的 roadmap 複本（disk clutter，非 git 追蹤）。

```bash
git mv docs/hufu-future-improvement-roadmap.md docs/hufu-future-improvement-roadmap.md
# git mv tmp/hufu-strict-verification-workflow-improvement.md docs/hufu-strict-verification-workflow-improvement.md  # 檔案不存在，未執行
```

- [x] **Step 2: Update references in documentation & roadmap file headers**

---

### Task 6: Comprehensive Verification & Commit

- [x] **Step 1: Run full test suite & lint**

```bash
go build ./cmd/hufu && go vet ./... && go test ./... -count=1
```

- [x] **Step 2: Commit all fixes**

```bash
git commit -m "fix(team): resolve event store dual-write, deduplication, error logging, and roadmap location (HF-STATE-001)"
```

---

## Residual Follow-ups（2026-07-21 第二輪 Review 認定，結案前/後處理）

下列三項為第二輪 Review 確認的殘留事項。**F1 為建議結案前補上的小修**，F2/F3 可文件註記、於後續 PR 處理。

### F1. Message 事件 dual-write 失敗未計入 `DualWriteFailures()`（建議結案前修）

- **現況：** `RecordSessionUserMessage` / `RecordSessionAssistantMessage`（`coordinator_eventstore.go:27`、`:48`）在 `es.Append` 失敗時只 `log.Printf` 警告，**未** `c.dualWriteFailures.Add(1)`。`emitEvent`（run 事件）與 `emitTaskEventsFromCheckpoint`（task 事件）則有遞增。
- **影響：** `DualWriteFailures()` 只反映 task/run 事件失敗，**漏掉訊息事件失敗**，作為雙寫健康度指標會低報。
- **修法：** 讓 `RecordSession*` 改回傳 `error`（或由 `addSessionUserMessage` / `addSessionAssistantMessage` 在 coordinator 方法層捕獲），失敗時 `c.dualWriteFailures.Add(1)`。約 5–10 行改動。補一個測試：注入會失敗的 EventStore（或關閉後再寫）斷言計數器 > 0。

### F2. `emittedTaskTransitions` 跨 session resume 不持久化（文件註記，後續 PR）

- **現況：** 去重 map 為 coordinator 實例生命週期內的記憶體狀態，不寫入 `session.json`。Resume 時 coordinator 重建、map 為空，第一次 `saveCheckpoint()` 會對已 `done` 的回復任務**重新發射一次 `task_completed`**。
- **影響：** 對 dual-write telemetry 階段有限（hash chain 不毀損、reducer 仍收斂到正確終態），但與「同一狀態轉移只發射一次」的嚴格承諾有落差。
- **處理：** 現階段以本註記記錄限制；日後要把 event store 升為真相來源時，需把已發射鍵持久化到 `session.json`，或在 resume 時依既有 event log 重建此 map（掃描已存在的 `task_*` 事件填入）。

### F3. `OpenEventStore` runID/sessionID 繼承缺獨立測試（後續 PR）

- **現況：** `NewEventStore` 已實作「空 runID/sessionID 時繼承 `last.RunID/SessionID`」，但未新增斷言繼承值的測試；既有 `TestEventStoreTamperDetection` 雖走 `OpenEventStore` 但未驗證此行為。
- **修法：** 補一個測試：先以 `run-1`/`sess-1` 寫入事件並 `Close`，再以 `OpenEventStore` 重開並 `Append` 一筆，斷言新事件的 `RunID == "run-1"` 且 `SessionID == "sess-1"`。

---

## 結案狀態

- HF-PR-002（Atomic Persistence）：✅ 完成。
- HF-PR-104（Event Store）：核心資料結構 + 雙寫接線 + 離散去重完成；**殘留 F1（message 計量）/F2（resume 去重持久化）/F3（繼承測試）**。
- Roadmap 狀態：維持 🟡 IMPLEMENTED (PENDING REVIEW)；待 **F1** 補完後再評估推進至 🟢。
