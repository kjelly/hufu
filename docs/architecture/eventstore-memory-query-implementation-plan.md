# EventStore 記憶事件範圍查詢實作計畫

> Status: draft
> Authority: guide
> Verified-Commit: `ccde722f`
> Supersedes: —
> Superseded-By: —
> Implementation-Ready: yes

本文件只規劃 coding agent 能在本 repository 內完成的工作。實作時以當下
程式碼、測試與正式架構文件為準；本計畫不取代現行 runtime 契約。

## Baseline 驗證（2026-10-04）

在文件提交 `982b2030`、尚未修改 Go 程式碼時，使用 Go 1.26.6 執行：

| 命令 | 結果 |
| --- | --- |
| `go test ./...` | exit 0；所有套件通過 |
| `go vet ./...` | exit 0 |
| `golangci-lint run` | exit 0，0 issues；曾提示一個已不存在的相鄰 worktree 檔案快取警告 |

後續階段以此測試結果及下列修改前的 consumer benchmark 作 baseline。

Consumer benchmark 已於遷移前執行（`go test ./internal/team -run '^$'
-bench '^BenchmarkMemoryEventConsumersSparse$' -benchtime=10x -benchmem
-count=1`，Intel i7-6700K）：

| 事件數 | Consumer | ns/op | B/op | allocs/op |
| ---: | --- | ---: | ---: | ---: |
| 1,000 | credit | 608,972 | 822,198 | 1,107 |
| 1,000 | report | 3,294,181 | 1,195,923 | 4,133 |
| 10,000 | credit | 4,409,704 | 8,149,144 | 10,901 |
| 10,000 | report | 27,859,773 | 11,887,020 | 41,201 |
| 50,000 | credit | 20,235,809 | 40,734,700 | 54,501 |
| 50,000 | report | 132,864,770 | 59,426,718 | 206,001 |

這是合成稀疏事件資料的單次 benchmark 結果，不代表實際 workspace 的負載。

## 目標與效益

把記憶結果計算中的兩個全量 `ReadEvents()` 呼叫，改用已存在的
`EventStore.QueryEvents(EventQuery)` 挑出所需事件。現有 `ReadEvents()` 會在
每次呼叫時複製所有已驗證事件及其 payload；`QueryEvents` 先篩選再複製。
因此，在長期運作且多數事件與記憶無關的 workspace，可減少結果記帳及最終
報告時的配置量與複製成本。這項修改仍需走訪記憶體中的事件，並不承諾常數
時間查詢或改善開啟 EventStore 時的完整驗證掃描。

完成範圍只有：

1. `internal/team/memory_outcome.go` 的 `memoryOutcomeWeightForSignal`。
2. `internal/team/memory_learning.go` 的 `MemoryLearningReport`。
3. 針對上述兩個 consumer 的語意測試與可重現 benchmark。

## 已核對的接縫

- `internal/team/event_store.go`：`ReadEvents()` 對已驗證的 `cachedEvents`
  做 defensive copy；沒有在每次讀取時重新掃描檔案。
- `internal/team/event_query.go`：`QueryEvents` 支援精確的 `RunID`、`Types`
  篩選，保留 durable order，且只複製符合的事件；現有測試涵蓋無效狀態與
  defensive copy。
- `internal/team/event_query_test.go` 已有稀疏事件的 API benchmark，可作
  比較基線，但不能替代本次兩個 consumer 的 benchmark。
- `memoryOutcomeWeightForSignal` 由 `recordMemoryOutcomeSignal` 呼叫，會以
  todo ID、manifest 的 retrieval ID、signal、direction 計算既有 credit。
- `MemoryLearningReport` 由 CLI 報告與 JSON 最終輸出呼叫，目前統計整個
  EventStore 的記憶事件，不限當前 run。

## 實作步驟

### 1. 先鎖定現有語意

在修改 production code 前，為兩個 consumer 加入或補齊下列 fixture：

- 不同 run 使用相同 todo ID，只有屬於該 item manifest 的 retrieval ID
  才計入 credit；不要新增 `RunID` 或 branch 隱式篩選。
- `memoryOutcomeWeightForSignal` 只計入 `memory_outcome_recorded`，並保留
  `TaskID`、`RetrievalID`、`Signal`、`Direction` 與 `EffectiveWeight` 的原有規則。
  malformed payload 仍忽略，store 讀取失敗仍回傳 0。
- `MemoryLearningReport` 對跨 run 的 `memory_retrieved`、
  `memory_usage_recorded`、`memory_outcome_recorded` 保留原本的曝光數、
  去重 retrieval 數、applied 數與 outcome 數。空或 malformed payload
  的計數方式、mode、policy version 與 pending repair gaps 不變；store
  讀取失敗仍回傳既有的部分報告。
- 交錯插入大量其他類型事件，確認兩個 consumer 的結果與無雜訊時一致。

測試要斷言公開結果及 fallback，不要複製 production reducer 寫一份平行實作。

### 2. 只遷移兩個讀取點

在 `memoryOutcomeWeightForSignal` 使用：

```go
events, err := c.eventStore.QueryEvents(EventQuery{
	Types: []string{"memory_outcome_recorded"},
})
```

保留後續 `TaskID` 與 payload 條件，且不設定 `RunID`。在
`MemoryLearningReport` 使用：

```go
events, err := c.eventStore.QueryEvents(EventQuery{
	Types: []string{
		"memory_retrieved",
		"memory_usage_recorded",
		"memory_outcome_recorded",
	},
})
```

保留原本的報告欄位、錯誤 fallback 與跨 run 統計。可以把 payload 解碼移到
各事件類型內，但不得改變 malformed payload 的既有計數結果。

本工作不修改 `EventStore`、`QueryEvents`、事件格式、hash chain、
`EventJournal` 介面、branch lineage、記憶政策或 CLI 輸出 schema；也不建立
新的索引、projection registry、資料庫或快取。

### 3. 量測與決定是否保留修改

新增 consumer 層 benchmark，使用相同 fixture 分別在修改前後量測兩個函式。
fixture 在計時區外建立，包含 1,000、10,000、50,000 筆事件；大部分為
與記憶無關、帶固定大小 payload 的事件，約 1% 為目標記憶事件，並交錯
不同 run 與 todo。報告每組的 `ns/op`、`B/op`、`allocs/op`，記錄 Go 版本與
執行命令。沿用現有 `BenchmarkEventStoreQueryEventsSparse` 作 API 對照。

10,000 與 50,000 筆 fixture 的 `B/op` 均須低於修改前，語意測試須全數通過；
若實測沒有減少配置，coding agent 停止這項遷移並回報數據，不擴大成通用
projection 重構。效能數字是此工作項的驗收證據，不以單次 `ns/op` 波動
推導整體 runtime 的改善幅度。

## 驗收與交付

- 僅上述兩個 production 呼叫點改用 `QueryEvents`；跨 run credit 與報告
  結果、錯誤 fallback、durable order 及 defensive copy 契約不變。
- 新增的 fixture 覆蓋跨 run 同 todo ID、雜訊事件、malformed payload
  與讀取失敗；consumer benchmark 可重現，並附修改前後結果。
- 執行相關 `internal/team` 測試與 benchmark；所有 Go 程式碼改動完成後
  執行 `golangci-lint run`，必須無錯誤。
- 交付摘要記錄修改的呼叫點、效能結果與未改善的部分。
