# Backend Session 身分衝突的處置與綁定事件語意：問題與建議做法

> Status: implemented — archived 2026-09-26 from local scratch space (`docs/tmp/session-identity-conflict-disposition.md`); implemented on branch `feat/runtime-lifecycle-hardening` (see Implementation record)
> Authority: reference（實作紀錄；現行行為以 [execution runtime](../../architecture/execution-runtime.md) 的 `BackendBinding` 段落為準）
> Verified-Commit: 2026-09-26
> Baseline: `26bed7c`（`feat/runtime-lifecycle-hardening`）
> 前置：[Runtime Lifecycle Hardening](runtime-lifecycle-hardening.md) PR 2（commit `4b8af58`）
> Revision: 2026-09-26 v2，採納外部評估：問題 3 改用「首次綁定為錨點」的嚴格規則；A 一律 `ReconcileOnly`；儲存故障與證據損壞分開處理。
> Superseded-By: [execution runtime](../../architecture/execution-runtime.md)
> Scope: 只調整 PR 2 新增的失敗如何分類與處置，以及 durable binding 的身分規則；不改 event schema、不新增 event type、不改 `ExecutionBackend` 介面。

## Implementation record

以單一 commit 實作於 `feat/runtime-lifecycle-hardening`（接在 `26bed7c` 之後），2026-09-26 fast-forward 進本機 `main`（未 push）。

- 身分規則放在新檔 `internal/team/subagent_session_identity.go`（`sessionIdentityFromLineage`、`resumableBackendSessionID`、`durableBackendSessionBinding`、`lineageReadFailure`），`subagent_binding.go` 只保留綁定寫入。
- 新增 `session_resume_mismatch` 與 `identity_conflict` 兩個 `TaskFailureClass`；`ExecutionIdentityConflictError` 與新的 `CodexSessionResumeMismatchError` 各自實作 `FailureClassOverride()`。
- blocked 訊息為 "backend session identity needs reconciliation (<class>); no turn started in this attempt"。
- 與本文件的偏離：無。

### Verification

- 新測試：`TestSessionIdentityRule`（規則表）、`TestSessionIdentityFailureClassification`（A、A′、B、C、D 損壞、D I/O、E、sync 不明，各走真實錯誤路徑）、`TestSessionIdentityFailuresBlockForReconciliation`、`TestSessionIdentityConflictBlocksLaunchButStillReplays`、`TestSessionIdentityLegacyHistoryResumesLastSession`、`TestCodexResumeMismatchAfterSideEffectBlocksTask`、`TestCodexIdentityConflictBlocksTaskWithoutLaunching`。
- Mutation 檢查：逐一拿掉七個防護（conflict 的 class override、狀態轉換事件的綁定觀察、每次 attempt 都查 durable 身分、`DecideRecovery` 的兩個新 case、repair 的 `protocol` override、錨點之後的衝突檢查、`*fs.PathError` 判定為 `environment`），每一個都至少有一個測試失敗。
- `go vet ./...`、`golangci-lint run ./...`（0 issues）、`bin/check-docs`、`go test ./...` 通過。
- `go test -race -timeout 45m ./internal/team/` 通過（793 s）；`go test -race ./cmd/hufu/` 通過（126 s）。

## 決策

- **D1：** A（resume 回傳不同 thread）一律 `ReconcileOnly`，不自動 replan。
- **D2：** 暫不提供「重設 session」操作（例如 `--new-session`）。它需要明確的重新綁定事件、舊 session 與未完成 turn 的處置，以及既有副作用的核對。
- **D3：** 儲存的暫時性 I/O 失敗標 `environment`，停止這個 task 的 worker 重試；hash chain 損壞、lineage 無法投影等證據損壞，標 `identity_conflict` 並阻止執行。

## 背景

PR 2 讓一個 task 在同一 branch 上只綁定一個 backend session，身分為 `(backend, session ID)`。為此新增了 per-task idempotency key `backend-session-bound:<task>`、綁定前的 lineage 檢查，以及 attempt 開始前從 canonical lineage 決定要 resume 的 session。實作後有三個問題要處理。

## 問題 1：綁定事件變少（結論：保留）

key 從 `backend-session-bound:<task>:<attempt>:<session>` 改為 `backend-session-bound:<task>`。第一次綁定寫入一筆事件；之後同一 session 的 attempt，EventStore 在 writer lock 內命中 key，回傳既有事件，不再寫新事件。因此這筆事件 payload 的 `attempt` 與 `execution_world_id` 是第一次綁定時的值。

可以接受的理由：

- 讀 `backend_session_bound` 的只有 reducer、replay 雙寫驗證與 binding 邏輯本身，沒有 inspect、history 或報表逐筆使用。
- 每個 attempt 的 session 與 world 仍會落盤：execution receipt（`coordinator_task_run.go:1324-1327`），以及 task 狀態轉換事件的 `backend_binding`（`coordinator_eventstore.go:1542`、`event_reducers.go:886`）。
- per-task key 讓衝突比對發生在 EventStore 的 interprocess writer lock 內，不必修改 `event_store.go`。

替代方案（每 task 一筆 claim 加每 attempt 一筆 evidence；或在 EventStore 加唯一性約束）不是多出沒有讀者的事件，就是要改核心 append 邊界，都不採用。

要做的只有文件：`execution-runtime.md` 說明這筆事件是 task 的首次綁定紀錄，每個 attempt 的 session 與 world 以 receipt 和狀態轉換事件為準。

## 問題 2：衝突的處置靠字串比對

### 機制

- `CodexProviderError` 的 `Class` 在 `subagent_codex.go` 以外沒有任何地方讀取，只以字串前綴出現在錯誤訊息裡。
- 錯誤都沒有 `FailureClassOverride`，所以分類落到 `classifyTaskFailureByText`：訊息含 `"protocol"` 就判成 `protocol`（`failure_classify.go:297`），否則多半是 `execution`。
- `DecideRecovery` 對 `protocol` 回 `ReconcileOnly`（`disposition.go:192-196`）；例外條件 `ProtocolRetrySafe` 只在缺少 `submit_result` 時成立（`coordinator_task_run.go:2021`），這些錯誤永遠不符合。`execution` 在可 replay 的 task 上走 `RetryWorker`。

### 錯誤來源與目前的處置

| 代號 | 觸發條件 | 目前分類 | 目前處置 |
|---|---|---|---|
| A | `thread/resume` 回傳的 thread ID 不等於 durable session | `protocol` | `ReconcileOnly`，訊息寫 "protocol failure" |
| A′ | result repair 的 resume 回傳不同 thread | `protocol` | `ReconcileOnly` |
| B | 綁定時 durable binding 與要綁的 session 不同 | `protocol` | `ReconcileOnly`，訊息寫 "protocol failure" |
| C | durable binding 的 backend 與 task 的 frozen target 不同 | `execution` | 重試；指紋重複時轉 `ReplanRequired` |
| D | canonical lineage 讀不到（hash chain 重驗後仍失敗） | `execution` | 同 C |
| E | 綁定時 append 失敗 | `protocol` | `ReconcileOnly`，設計好的 canonical 恢復路徑不會執行 |

### 問題點

1. 處置取決於錯誤字串是否含 `"protocol"`，屬於巧合。
2. blocked 的原因寫成 "protocol failure; worker tools must not be replayed"，重試提示要模型「改變做法」；這些錯誤都發生在 turn 開始之前。
3. C 是每次都會重現的衝突，重試只會浪費 attempt。
4. D 把「暫時讀不到」和「hash chain 損壞」混在一起，後者應該阻止執行，而不是交給模型換方法。
5. E 被判成 `protocol` 後直接 block，下一次 attempt 先讀 canonical lineage 的恢復路徑沒有機會執行。

### 為什麼不回到舊行為（直接接受新 session）

Codex 的對話脈絡會悄悄遺失；同一 task 在 log 上留下兩個互相矛盾的 session；也違反 PR 2 的原則：只有已持久化的 identity 才能作為 resume 的依據。

## 問題 3：durable binding 的身分規則不完整

目前 `durableBackendSessionBinding`（`subagent_binding.go:198`）只看綁定事件，取最後一筆；但 reducer 也會從 `task_` 開頭的狀態轉換事件套用 `backend_binding`（`event_reducers.go:886`）。只把查詢改成 reducer 的結果也不夠，會一樣卡住：

1. 綁定事件記錄 S1，占用 `backend-session-bound:<task>`。
2. 之後的狀態轉換事件帶 S2，reducer 得到 S2。
3. resume S2 後呼叫 `Append`，取回既有的 S1 事件。
4. 比對 S1 與 S2，仍然失敗；每次、每個 process 都一樣。

另外，checked replay 對 target 有跨事件的不可變檢查（`execution_target_replay.go:190`），對 session 則只比對同一個 `task:attempt:session` key 之內（`:404-407`）和同一筆事件的雙寫欄位（`:187`），S1 換成 S2 不會被發現。

目前沒有已知路徑會產生這種歷史，但 resume、綁定與診斷必須依同一套規則判斷。

## 設計

### 身分規則

- **不可變：** `(backend, session ID)`。
- **可更新的執行資訊：** attempt、execution world、cwd、turn、effective model。
- **錨點（claim）：** active lineage 上第一筆以新格式 key `backend-session-bound:<task>` 寫入的 `backend_session_bound` 事件。
- **有錨點的 task：** 錨點之後的每一個綁定觀察，包括綁定事件，以及帶 `backend_binding`（或只帶 `provider_binding`）的 `task_` 狀態轉換事件，身分都必須等於錨點；不符就是 `identity_conflict`，錯誤附上錨點事件 ID 與衝突事件 ID。不以「最後一筆勝出」消除衝突。
- **沒有錨點的舊 task（只有舊格式 key 的事件）：** 維持最後一筆觀察為準，與 reducer 一致。新程式第一次綁定時寫入錨點，從此不可變。舊版程式在失敗窗口留下的「attempt 1 用 S1、attempt 2 用 S2」歷史不需遷移，仍會 resume S2。

### 強制點

- `resumableBackendSessionID`：每次 attempt 啟動 app-server 之前都讀取 active lineage 並套用規則。有 durable 身分時，projection 的 session 必須與它相同，否則回報 `identity_conflict`；沒有 durable 身分時才使用 projection（相容只存在於 checkpoint 的 binding）。lineage 讀不到時 fail closed。
- `persistProviderSessionBinding`：綁定前以同一規則預檢；append 後比對 writer lock 內回傳的錨點。
- **啟動時的 checked replay 不加入 session 不可變的拒絕。** `hufu reconcile --task` 與 `hufu retry --task` 也經過同一個啟動預檢（`recoverycmd.go:74` → `loadTeamCommon` → `preflightHistoricalExecutionTargets`）。若在那裡拒絕，一個 task 的矛盾就會讓整個 run 無法啟動，操作者連處理那個 task 的指令都跑不了。矛盾只在 resume 或綁定該 task 時回報，並阻止該 task。

### 失敗分類

照 `workspace_conflict` 的既有做法，新增兩個 `TaskFailureClass`：

| Class | 來源 | 處置 |
|---|---|---|
| `session_resume_mismatch` | A | `ReconcileOnly` |
| `identity_conflict` | B、C、問題 3 的矛盾、證據無法驗證（hash chain 損壞、session tree 格式錯誤、lineage 無法投影） | `ReconcileOnly` |
| 既有 `environment` | 讀取 lineage 時的檔案 I/O 失敗（`*fs.PathError`）；綁定時的 append 失敗 | 既有對應 `ReplanRequired`：只結束該 task 的 retry loop，不停止整個 run |
| 既有 `protocol`（維持） | A′ | `ReconcileOnly`；repair 發生在 turn 改過 workspace 之後，保留部分結果交給 reconcile |

- 兩個新 class 在 `DecideRecovery` 的 class 判斷中直接回傳 `ReconcileOnly`。它位於 replay 檢查之前，但 `ReconcileOnly` 本身就是不重跑 worker 的最保守結果，不會繞過任何安全檢查。
- 無法歸類為檔案 I/O 的 lineage 讀取失敗，一律當成證據無法驗證（`identity_conflict`），寧可 block 也不交給模型重試。
- append 失敗標 `environment`：之後以 `hufu retry --task` 恢復時，`resumableBackendSessionID` 會先重驗 canonical lineage，已落盤的 binding 會被 resume，損壞的證據會被 block。
- 型別化來源：`ExecutionIdentityConflictError` 實作 `FailureClassOverride()` 回傳 `identity_conflict`（它在 task 執行路徑上只由 session 綁定產生；`session_tree.go` 的 replay 呼叫不做 task 分類）。A 由新的 `CodexSessionResumeMismatchError` 產生，實作 `FailureClassOverride()` 回傳 `session_resume_mismatch`，並記錄 requested 與 returned 的 thread。A′ 在 repair 呼叫點外層包 `withFailureClassOverride(err, FailureProtocol)`，`errors.As` 先找到最外層的 override。
- `ReconcileOnly` 分支為這兩個 class 產生專屬訊息，不再沿用 "protocol failure" 或 "automatic replay is not allowed"。
- `SystemicDispositionForClass` 把兩個新 class 列入 `needs_human`。
- `knownTaskFailureClasses` 加入兩個新 class，讓 `on-failure-classes` 可以引用。

## 驗收

- 每種來源都得到上表的 class 與處置（`DecideRecovery` 的 table-driven 測試，以及用實際錯誤鏈呼叫 `ClassifyTaskFailureStructured` 的分類測試）。
- **A，前次 attempt 已有副作用：** 第一個 attempt 寫入檔案並綁定 S1；第二個 attempt 的 resume 回傳不同 thread。task 變成 blocked，class 為 `session_resume_mismatch`，沒有第三個 attempt，`thread/resume` 只呼叫一次，第一個 attempt 的檔案保留。
- **C：** durable binding 的 backend 不符時，task 在第一個 attempt 後 blocked，app-server 從未啟動。
- **問題 3：** 錨點 S1 之後的狀態轉換事件帶 S2 時，`RunAttempt` 在啟動 app-server 前回報 `identity_conflict`，錯誤附上兩個事件 ID；而且這個 lineage 仍能通過 `ReplayTodoList` 與 `ReadCheckedActiveExecutionTaskEvidence`，恢復指令不會被啟動預檢擋住。
- **相容：** 舊格式 key 的「attempt 1 用 S1、attempt 2 用 S2」歷史 resume S2；綁定 S2 後寫入錨點，之後出現 S1 就是衝突。
- **E 與 sync 結果不明：** append 的 sync 失敗分類為 `environment`；之後 resume 的是已落盤的原 session，不會開新 session。
- **D：** hash chain 損壞分類為 `identity_conflict`；檔案 I/O 失敗分類為 `environment`。
- `validateOnFailureClasses` 接受兩個新 class。
- 既有測試、`go test ./...`、`go test -race ./...`（`internal/team` 需 `-timeout 45m`）與 `golangci-lint run` 通過。

## 主要檔案

`internal/team/run_result.go`、`internal/team/disposition.go`、`internal/team/contract_compile.go`、`internal/team/systemic_scope.go`、`internal/team/execution_target_replay.go`（`ExecutionIdentityConflictError`）、`internal/team/codex_appserver_protocol.go`、`internal/team/subagent_codex.go`（只改呼叫點）、`internal/team/subagent_binding.go`（身分規則，必要時拆出新檔）、`internal/team/coordinator_task_run.go`（只改 `ReconcileOnly` 訊息分支）、相關測試，以及 `docs/architecture/execution-runtime.md`。
