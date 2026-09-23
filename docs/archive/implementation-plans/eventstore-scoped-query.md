# Hufu EventStore 範圍查詢實作計畫

> Status: Historical — implemented
> Authority: reference
> Verified-Commit: `5ee0246`
> Supersedes: —
> Superseded-By: —
> Completed: 2026-09-22

基準提交：`6b865ed99a4e0167f92ec8aadcca1b90c9fab763`
實作範圍：`internal/team` 內部程式碼與測試

> 本文件原先位於未納入版本控制的 `docs/tmp/now/` scratch space；實作完成後依文件生命週期移至 `docs/archive/implementation-plans/`。程式碼與測試才是 runtime 交付物。

## 1. 目標

在不改變 EventStore 持久化、事件 schema、CLI 或公開輸出契約的前提下，提供一個可依 `RunID` 與事件類型篩選既有記憶體快取的查詢 API，並把 `Coordinator.Metrics()` 中兩個會複製整份事件歷史的讀取點改用該 API。

完成後應具備以下結果：

1. metrics 查詢不再為無關事件複製 `RunEvent` 與 `Payload`。
2. 查詢仍只讀取 EventStore 已驗證的 `cachedEvents`，不重新讀檔、不建立第二份持久化狀態。
3. retry suppression 與 failure metrics 的結果和目前行為完全相同。
4. EventStore 的 hash chain、append、rescan、idempotency、branch lineage 與錯誤語意完全不變。

## 2. 已核對的現況

本計畫以目前程式碼為準：

- `internal/team/event_store.go`
  - `EventStore.ReadEvents()` 在鎖內讀取已驗證的 `cachedEvents`。
  - 每次呼叫都以 `cloneRunEvents` 複製整份事件切片及每筆非 nil `Payload`。
  - append 成功並完成 `Sync` 後，才把事件加入 `cachedEvents`。
- `internal/team/metrics.go`
  - `retrySuppressionsFromEvents()` 讀取全部事件，再依 `executionRunID` 與 `retry_suppressed` 篩選。
  - `failureEventsForMetrics()` 讀取全部事件，再依 `executionRunID` 與三種 task failure 事件篩選。
  - 讀取事件失敗時，既有 fallback 行為不同：
    - retry suppression 保留 coordinator 記憶體計數。
    - failure metrics 改從 todo items 的 `FailureEvent` 聚合。
- `EventJournal` 目前只有 `Append`、`ReadEvents`、`VerifyHashChain`。本次不修改該介面，避免波及 production adapters 與大量測試 doubles。

## 3. 固定範圍

### 3.1 本次必做

只實作以下四項：

1. 新增內部 typed query：`EventQuery`。
2. 新增 `(*EventStore).QueryEvents(EventQuery)`。
3. 遷移 `metrics.go` 的兩個全量讀取點。
4. 新增單元測試、metrics regression tests 與一個無門檻 benchmark。

### 3.2 明確不做

下列主題已從本實作計畫移除，不是後續工作包，coding agent 不得順手實作：

- Projection Registry 或通用 reducer/plugin framework。
- activation budget、`runtime:` 或 `execution-backends:` manifest 欄位。
- backend/agent publication boundary 重構。
- 通用 Runtime Component State / lifecycle state machine。
- stdout/stderr、`--output`、`--event-format` 或 JSONL protocol 變更。
- PTC-like program runtime、sandbox、seccomp、container 或 subprocess isolation。
- startup readiness domain、`team check` schema 或 exit-code 變更。
- EventJournal 介面變更。
- EventStore 檔案格式、事件 envelope、schema version 或 hash 計算變更。
- branch lineage、session tree、decision journal 或 event replay 語意變更。
- 將其他 `ReadEvents()` call sites 一併遷移。

移除原因：這些項目不是完成本次內部查詢優化的必要條件，且會引入產品介面、安全政策、相容性或跨模組架構決策。它們不得成為本次實作的阻塞條件。

## 4. 不可破壞的契約

### 4.1 Canonical truth

- `event_store.jsonl` 仍是唯一 durable event truth。
- `cachedEvents` 仍只能由既有嚴格 scan/rescan 與 durable append 路徑建立或更新。
- `QueryEvents` 只能讀取 `cachedEvents`，不得維護另一份索引、快取、checkpoint 或 sidecar 檔案。

### 4.2 Durability 與 publication

- 不修改 `AppendPersistedContext` 的順序。
- 不得在 `Write` / `Sync` 成功前讓任何新查詢看見事件。
- append、sync 或 rescan 失敗後，`QueryEvents` 必須和 `ReadEvents` 一樣拒絕從 invalid state 回傳資料。

### 4.3 相容性

- `ReadEvents()` 的 signature 與行為保持不變。
- `EventJournal` 保持不變。
- `RunEvent`、event type 字串、payload 內容與序列化保持不變。
- CLI flags、stdout、stderr、JSON output 與 process exit contract 保持不變。
- metrics 的 JSON 欄位、零值、計數與 fallback 行為保持不變。

### 4.4 Ownership 與防禦性複製

- caller 不得取得 `cachedEvents` 或其中 `Payload` 的共享可變參照。
- `QueryEvents` 只複製符合條件的事件。
- 回傳結果的 durable order 必須和 `cachedEvents` 相同。

## 5. API 設計

在新檔案 `internal/team/event_query.go` 新增：

```go
package team

// EventQuery selects validated events from EventStore's in-memory cache.
// Empty fields are wildcards. Matching is exact and preserves durable order.
type EventQuery struct {
	RunID string
	Types []string
}

func (es *EventStore) QueryEvents(query EventQuery) ([]RunEvent, error)
```

### 5.1 精確語意

| 欄位 | 空值 | 非空值 |
|---|---|---|
| `RunID` | 不依 run 篩選 | `event.RunID == query.RunID` |
| `Types` | 不依 type 篩選 | `event.Type` 必須等於其中任一字串 |

其他規則：

1. `Types` 中重複值不得造成重複結果。
2. 不 trim、不大小寫轉換、不接受 prefix 或 pattern matching。
3. 未知 event type 是合法條件；沒有符合項目時回傳空結果，不回錯。
4. `EventQuery{}` 等同 `ReadEvents()` 的選取範圍，但仍由新方法自行完成鎖定與複製。
5. 篩選後結果依原始 durable order 回傳。
6. 沒有符合項目時可回傳 nil slice；caller 不得依賴 nil 與空 slice 的差異。
7. 每筆回傳事件必須使用既有 `cloneRunEvent`，包含 payload defensive copy。

### 5.2 鎖定與錯誤語意

`QueryEvents` 必須沿用 `ReadEvents` 的同步與 state gate：

1. 先把 `Types` 建成唯讀 membership set，再取得 EventStore lock。
2. 取得 lock 後，依目前 `ReadEvents` 的規則處理：
   - `path == ""`：回傳 `nil, nil`。
   - `stateValid == false` 且有 `stateErr`：回傳包住該錯誤的 `event store state invalid`。
   - `stateValid == false` 且無 `stateErr`：回傳 `event store state invalid`。
3. valid state 才能增加既有 `cacheHitCount`。
4. 在持有 lock 時走訪 `cachedEvents`，只 clone matching events。
5. 不呼叫 `ReadEvents()`，避免先複製全部事件。
6. 不呼叫 scan、rescan、open、stat 或任何 filesystem API。

為避免 `ReadEvents` 與 `QueryEvents` 的 invalid-state 訊息日後分岔，可抽出一個只在 EventStore lock 內呼叫的 private validation helper。若抽 helper，`ReadEvents` 的外部行為必須由既有與新增測試證明未改變。

### 5.3 不加入 BranchID 的理由

本次兩個 consumer 的既有語意只以 `executionRunID` 篩選，沒有再以 branch 篩選。擅自加入 branch 條件可能改變目前跨 session tree 的 metrics 結果。因此本次 query contract 不提供或推導 branch lineage；不得從 `EventStore.branchID` 隱式過濾。

## 6. Consumer 遷移

只修改 `internal/team/metrics.go` 的以下兩個函式。

### 6.1 `retrySuppressionsFromEvents`

以以下等價查詢取代 `ReadEvents()` 後的 envelope 篩選：

```go
events, err := c.eventStore.QueryEvents(EventQuery{
	RunID: c.executionRunID,
	Types: []string{"retry_suppressed"},
})
```

保留現有 payload decode 與計數規則：

- JSON decode 失敗：忽略該事件。
- `reason_code` 為空：忽略該事件。
- 至少一筆有效 reason 才回傳 `found == true`。
- store/query error：回傳 `nil, false`，讓 `Metrics()` 保留 in-memory counters。
- `executionRunID == ""`：維持 legacy unscoped 行為，讀取所有 run 的 matching type。

不得把 malformed payload 提升成 `Metrics()` error；`Metrics()` 目前沒有 error return，本次不改 API。

### 6.2 `failureEventsForMetrics`

以以下等價查詢取代 `ReadEvents()` 後的 envelope 篩選：

```go
events, err := c.eventStore.QueryEvents(EventQuery{
	RunID: c.executionRunID,
	Types: []string{
		"task_failed",
		"task_blocked",
		"task_protocol_incomplete",
	},
})
```

保留現有 payload merge 與 fallback：

- 每筆仍交給 `mergeFailureEventJSON(nil, event.Payload)`。
- malformed / absent failure payload 仍忽略。
- query 成功時，即使零筆符合，也回傳空 failure result，不得改從 todo fallback。
- query 失敗時，才從傳入的 todo items 收集 `item.FailureEvent`。
- `executionRunID == ""`：維持 legacy unscoped 行為。

### 6.3 禁止順手重構

- 不合併上述兩個函式。
- 不改 `RunMetrics` 結構。
- 不改 `Metrics()` 的鎖定範圍。
- 不把 event type 字串提升為新的全域 enum；這會擴大本次 diff。
- 不修改其他 package 或其他 `ReadEvents()` call sites。

## 7. 工作包與實作順序

### WP-1：範圍查詢 primitive

檔案：

- 新增 `internal/team/event_query.go`
- 新增 `internal/team/event_query_test.go`
- 必要時小幅修改 `internal/team/event_store.go` 以共用 private state-validation helper

完成條件：

- API 與第 5 節一致。
- 不新增持久狀態或背景 goroutine。
- query 僅 clone matching events。
- unit tests 全部通過。

### WP-2：metrics consumer migration

檔案：

- 修改 `internal/team/metrics.go`
- 修改或補強 `internal/team/reliability_metrics_checkpoint_test.go`
- 若既有 retry suppression fixture 更適合，可補強 `internal/team/retry_suppression_test.go`

完成條件：

- 兩個指定函式不再呼叫 `ReadEvents()`。
- metrics 成功、空結果、malformed payload、不同 run 與 store error 行為均保持相容。

### WP-3：效能證據與完整驗證

檔案：

- benchmark 放在 `internal/team/event_query_test.go`

完成條件：

- 新增 `BenchmarkEventStoreQueryEventsSparse`。
- fixture 至少包含大量無關事件與少量符合事件。
- benchmark 同時提供 `ReadEvents()+caller filter` 與 `QueryEvents` sub-benchmark，並呼叫 `b.ReportAllocs()`。
- benchmark 只提供比較證據，不設硬性時間或 allocation threshold，避免 CI 噪音造成不穩定 gate。
- 完成第 10 節所有驗證命令。

工作包必須依序完成；WP-2 依賴 WP-1，WP-3 驗證兩者。

## 8. 必要測試案例

### 8.1 `EventStore.QueryEvents`

至少涵蓋：

1. `EventQuery{}` 回傳和 `ReadEvents()` 等值且同順序的事件。
2. 只指定 `RunID` 時，不同 run 的事件不會混入。
3. 只指定單一 type 時，只回傳該 type。
4. 指定多個 types 時，結果仍按 durable order，而不是依 `Types` 順序分組。
5. 同時指定 `RunID` 與 types 時採 AND 語意。
6. `Types` 有重複值時不重複回傳事件。
7. unknown type 與無符合項目回傳成功的空結果。
8. 修改回傳事件的 `Payload` 不得改變下一次 `ReadEvents()` 或 `QueryEvents()` 的內容。
9. query 不增加 `scanCount`，但 valid query 依既有 cache read 定義增加 `cacheHitCount`。
10. invalid state 回傳和 `ReadEvents()` 同類的 fail-closed error，不回傳 stale cached data。
11. zero-path store 保留 `nil, nil` 行為。

### 8.2 Metrics regression

至少涵蓋：

1. active run 只計算該 run 的 `retry_suppressed`。
2. 空 `executionRunID` 保留跨 run legacy 計數。
3. malformed JSON 與空 `reason_code` 不計數。
4. active run 只計算該 run 的 `task_failed`、`task_blocked`、`task_protocol_incomplete`。
5. 無關 event type 不影響 failure metrics。
6. query 成功但零筆符合時，不誤用 todo fallback。
7. EventStore invalid/query error 時，failure metrics 使用 todo fallback。
8. EventStore invalid/query error 時，retry suppression 保留 in-memory counter。
9. 既有 `FailuresByClass`、`FailuresByPhase`、`RetryAttemptsAvoidedByDisposition` 與 `CancelledTasksExcludedFromRetries` assertions 持續通過。

測試不得只驗證事件數量；必須驗證選取 identity、順序與最終 metrics 值。

## 9. 驗收矩陣

| 驗收項 | 證據 | 必須結果 |
|---|---|---|
| Query correctness | `event_query_test.go` | run/type/AND/order 語意通過 |
| Defensive ownership | payload mutation test | canonical cache 不變 |
| Failure semantics | invalid-state tests | fail closed，不回 stale data |
| No extra I/O | `scanCount` assertion | query 前後不變 |
| Metrics parity | regression tests | 和既有計數及 fallback 相同 |
| Allocation direction | sparse benchmark | 可比較 matching-only 與 full-copy；不作硬 gate |
| Scope control | `git diff -- cmd/hufu internal` | 只包含第 7 節列出的 `internal/team` 檔案 |
| Full repository health | test/vet/lint | 全部成功 |

## 10. 驗證命令

coding agent 完成程式碼後，必須依序執行：

```bash
gofmt -w internal/team/event_query.go internal/team/event_query_test.go internal/team/metrics.go internal/team/reliability_metrics_checkpoint_test.go internal/team/retry_suppression_test.go
go test ./internal/team -run 'TestEventStoreQueryEvents|Test.*Metrics|Test.*RetrySuppression' -count=1
go test ./internal/team -run '^$' -bench BenchmarkEventStoreQueryEventsSparse -benchmem
go test ./...
go vet ./...
golangci-lint run
git diff --check
git diff -- cmd/hufu internal
```

若某個列出的測試檔沒有修改，不應為了符合命令而 touch；`gofmt` 可只帶實際存在且有修改的 Go 檔案。

任何命令失敗都必須修正並重跑。依根目錄 `AGENTS.md`，`golangci-lint run` 是程式碼變更的必要完成 gate。

## 11. Coding agent 停止條件

以下情況才停止並回報，不自行擴張設計：

1. 目前 HEAD 已改變，使第 2 節描述的兩個 metrics call sites 不再存在。
2. 實作需要改動 `EventJournal`、event schema、CLI 或公開輸出契約才能完成。
3. 既有測試證明 current metrics 有不同於第 6 節的 branch 或 fallback 契約。
4. workspace 有會和第 7 節檔案重疊的未提交使用者變更，且無法安全保留。

其他一般編譯、測試或 lint 問題由 coding agent 直接修正，不需要產品、設計、安全或維運人員介入。

## 12. Definition of Done

只有同時符合以下條件才算完成：

- `EventQuery` 與 `QueryEvents` 已依第 5 節實作。
- 只有兩個指定 metrics consumer 改用新 API。
- 第 8 節測試完整且通過。
- sparse benchmark 可執行並輸出 allocation 資料。
- 第 10 節 test、vet、lint 與 diff checks 全部成功。
- 沒有修改 CLI、event schema、EventJournal、durability、branch 或輸出契約。
- 本文件已移至 `docs/archive/implementation-plans/`，不再由 `docs/tmp/` 作為 runtime 依據。

達成以上條件後，coding agent 可直接交付程式碼，不需要額外的人工作業或外部系統變更。
