# Hufu Runtime Lifecycle Hardening：可實作規格

> Status: implemented — archived 2026-09-26 from local scratch space (`docs/tmp/spec.md`); implemented on branch `feat/runtime-lifecycle-hardening` (see Implementation record)
> Authority: reference（實作紀錄；現行行為以 [execution runtime](../../architecture/execution-runtime.md)、[workspace versioning](../../architecture/workspace-versioning.md) 與 [operator experience](../../architecture/operator-experience.md) §9.1 為準）
> Verified-Commit: 2026-09-26
> Review baseline: `f6ebb4f39e6bf8abd5cfd37bc46190123874c1d8`
> Revision: 2026-09-26 修訂 PR 2 的身分與衝突範圍（見 PR 2 修訂說明）；PR 3 拆成 3a（檢查順序）與 3b（彙整結果）。
> Superseded-By: [execution runtime](../../architecture/execution-runtime.md)；PR 2 衝突的失敗處置由 [backend session 身分衝突的處置](session-identity-conflict-disposition.md) 補上
> Scope: 保護既有 runtime 的資源所有權、external-agent session binding 與啟動前置檢查；不引入新執行架構。

## Implementation record

實作於 `feat/runtime-lifecycle-hardening`，每個 PR 一個 commit，2026-09-26 fast-forward 進本機 `main`（未 push）。本文件原為 gitignored 的草稿；內文「留在 `docs/tmp/`、不得 commit 或移動」的指示只適用於草稿階段，實作完成後依維護者要求移入 `docs/archive/implementation-plans/`。

| Commit | 項目 | 內容 |
| --- | --- | --- |
| `15d9b90` | PR 1 | `teamContext` 擁有 MCP manager；`Close()` 依序停止 coordinator、關閉 MCP manager、釋放 workspace lease，保留每個錯誤；`loadTeamCommon` 的失敗路徑一律關閉 manager。coordinator 建構後的啟動步驟拆到 `startTeamCoordinator`，以符合 gocyclo 上限。 |
| `88a131e` | PR 3a | role 解析、execution-target preflight 與 effective-contract lint 移到 `bindRunWorkspaceVersioning` 之前。 |
| `4b8af58` | PR 2 | 依修訂後的規則實作：身分 `(backend, session ID)`、範圍 `(branch, task)`、per-task idempotency key、resume ID 檢查、attempt 啟動前讀取 canonical lineage。 |
| `26bed7c` | PR 3b | `runStartupStaticGate` 以 `TeamCheckItem` 收集結果；單一失敗保留原錯誤，多項失敗一次列出；`team check` 輸出不變。 |
| `2f3742f` | PR 2 後續 | 衝突的失敗分類與處置，以及以首次綁定為錨點的身分規則，見 [backend session 身分衝突的處置](session-identity-conflict-disposition.md)。 |

### 與原規格的偏離

- **PR 2：** 原稿以「branch、task ID、attempt」為衝突範圍並比對 execution world identity。實作前核對發現與本規格自己的驗收矛盾：Codex 的 world ID 每次 `RunAttempt` 都重新產生，attempt 計數每次 dispatch 從 1 開始。因此改為上表的身分與範圍（見 PR 2 修訂說明）。
- **PR 2：** 原稿沒有設計衝突之後的處置，實作後的處置取決於錯誤字串；由後續計畫補上型別化的失敗分類。
- **PR 3：** 拆成 3a 與 3b。`team check` 的 JSON 與 exit code 不變，run 啟動的新 check ID 只多了 `static.effective_contract`。

### Verification

每個 PR 各自通過 `go vet ./...`、`golangci-lint run ./...`（0 issues）、`go test ./...`，並以 race 測試相關 package。`go test -race ./internal/team/` 約需 14 分鐘，超過 Go 預設的 10 分鐘 test timeout，需加 `-timeout 45m`；這是環境限制，不是 data race。

## 目標與效益

1. MCP manager 在正常結束與各種啟動失敗路徑都被關閉，避免連線、子程序或檔案描述符殘留。
2. Codex 類 backend 的 session binding 以既有 canonical event 為唯一發布點；重試與恢復只使用已持久化的 identity，避免同一 task attempt 形成互相矛盾的 session。
3. 一次呈現可獨立檢查的前置失敗，減少操作者修好一項後再啟動、才發現下一項錯誤的循環。

三項工作互相獨立，可以依下列順序分成三個 PR。每個 PR 必須單獨通過驗證並可合併。只需 coding agent 修改程式、測試及正式文件；此草稿留在 `docs/tmp/`，不得移動或加入版本控制。

## 現有契約

- `internal/team/event_store.go` 的 `EventStore` 是 canonical append-only history，具有 hash chain、branch、idempotency 與 durability-unknown 恢復。不得新增第二份可寫 history。
- `internal/team/execution_backend.go` 的 `ExecutionRegistry` 與 `ExecutionBackend` 仍是唯一 backend 路由；不新增 parallel registry 或全面改寫介面。
- `internal/team/subagent_binding.go` 已使用 `backend_session_bound` event；`internal/team/subagent_codex.go` 已在 `turn/start` 前呼叫持久化 callback。本計畫補強故障窗口，不另造 publication event。
- `internal/team/coordinator_close.go` 明訂 `Coordinator.Close()` 不關閉 caller 提供的資源。`cmd/hufu/team_setup.go` 建立 MCP manager 並傳入 coordinator，因此 CLI caller 必須擁有其關閉責任。
- `cmd/hufu/teamcheckcmd.go` 已有 `TeamCheckDocument`、required／warning 與 `not_checked` 類型語意。下述前置檢查沿用這些語意；不建立第二套 doctor 或通用 `ComponentState` 狀態機。
- 既有 execution policy、tool authorization、verification、receipt、acceptance 與 recovery ownership 不變。診斷資訊不能成為授權依據。

## PR 1：修正 MCP manager 所有權與關閉路徑

### 變更

1. `teamContext` 明確持有 `loadTeamCommon` 建立的 MCP manager。成功回傳後，由 `teamContext.Close()` 在 coordinator 停止後關閉 manager，最後釋放 workspace lease。各關閉錯誤以 `errors.Join` 保留，不能因第一項失敗略過其餘清理。
2. `loadTeamCommon` 在 manager 建立後、`teamContext` 成功交付前負責清理；涵蓋 `NewCoordinator` 失敗、policy freeze 失敗、capability validation 失敗及後續 setup 失敗。所有權轉移必須只有一次；正常路徑不可提前關閉。
3. `buildMCPManager` 部分載入失敗仍維持目前 warning／degraded 語意，不把既有 optional MCP 載入警告升級成啟動失敗；已建立的 manager 仍須關閉。
4. 對 default team、沒有 MCP 的 team、既有 `teamContext.Close()` 重複呼叫情境保持安全。若需要測試 seam，限定在 manager 建立／關閉介面，避免為此新增通用 lifecycle framework。

### 驗收

- 使用可觀察的 fake manager 或局部測試 seam，證明成功、constructor 失敗、constructor 後失敗與部分 MCP 載入失敗皆恰好關閉一次。
- coordinator 關閉失敗時仍關閉 manager 並釋放 workspace lease；回傳錯誤包含各失敗原因。
- 現有 MCP tool 使用、default team 與無 MCP 路徑測試通過。

### 主要檔案

`cmd/hufu/team_setup.go`、`cmd/hufu/team_loader.go`、相關 `cmd/hufu/*_test.go`；必要時只增加極小的 manager close seam。

## PR 2：補強既有 session binding 發布與恢復

### 修訂說明（2026-09-26）

原稿以「同一 branch、task ID、attempt」為衝突範圍，並把 execution world identity 納入比對。實作前核對發現這與現況衝突：

- `AttemptRequest.Attempt` 是 `coordinator_task_run.go` retry 迴圈的區域計數，每次 dispatch 從 1 開始，不是 occurrence identity。crash 後 resume 會重複使用相同數字。
- Codex 的 `ExecutionWorldID`（`execution_world_local.go`，`localworld-<UnixNano>-<seq>`）每次 `RunAttempt` 都重新產生。若納入比對，合法的 crash 後 resume 一律被拒，與本 PR 自己的驗收項目矛盾。
- 既有語意是 binding 綁在 task 上：`BackendBinding` 寫入後不會被清除，之後每次 attempt／dispatch 都 resume 同一 session。現況沒有「同一 task 換新 session」的合法路徑。
- `codexStartOrResumeThread` 沒有檢查 `thread/resume` 回傳的 thread ID 是否等於要求的 ID。idempotency key 與 replay 衝突檢查的 key 都含 session ID，所以兩個不同 session 不會被擋。

### 身分與範圍

- **Binding identity** = `(backend, session_id)`。`execution_world_id`、`cwd`、turn、effective model 屬診斷資訊，或已由 Codex effective-state 驗證處理，不參與衝突比對。
- **衝突範圍** = `(branch, task ID)`：在 active branch 可見 lineage 上，一個 task 只能有一個 binding identity。不同 attempt 以相同 identity 重複發布屬冪等。
- 比對對象是該 task 在 active lineage 上**最後一筆** `backend_session_bound`（含 legacy `provider_session_bound`），與 reducer 的 last-writer-wins 一致。舊 workspace 即使歷史上出現過兩個 session，也不會因此無法 replay 或 resume。

### 變更

1. 保留 `persistProviderSessionBinding` 的順序：驗證 frozen target 與有效 session identity → append 既有 `backend_session_bound` event 並確認 durability → 更新 Todo／session projection → 允許 `turn/start`。
2. `thread/resume` 回傳的 thread ID 必須等於要求 resume 的 durable session ID，否則 fail closed，且不呼叫 binding callback。
3. idempotency key 改為 `backend-session-bound:<task>`；branch 已由 EventStore 的 idempotency identity 區分。EventStore 在 interprocess writer lock 內比對 key，key 已存在時回傳既有 durable event。`persistProviderSessionBinding` 解碼回傳的 event：identity 不同就回傳 `ExecutionIdentityConflictError`，不更新 projection。這是同一 branch 並行寫入或重試寫入的原子保證，不需要修改 `event_store.go`。
4. append 前另外讀取 active lineage 上的 durable binding，涵蓋 fork 繼承的 binding 與舊格式 key 的 event。identity 不同就拒絕，不 append。在同一 process 內，同一 task 由 occurrence controller 序列化；跨 process 由 workspace lease 保障。這一步是補充檢查，不是唯一保證。
5. `RunAttempt` 在啟動 app-server 之前決定要 resume 哪個 session：projection 有 binding 就用 projection；沒有就讀 canonical lineage，有 durable binding 就 resume 它；讀取失敗（event store degraded、lineage 無法投影）就 fail closed，不開新 session。這涵蓋三種情況：append 結果 durability unknown、event 已寫入但 projection 更新失敗、crash 後 projection 落後。
6. append 明確失敗時不啟動 turn，並停止 app-server 與 execution world。這部分現況已實作（`RunAttempt` 的 defer），只需補測試。
7. 不改 event payload schema、不新增 event type、不改 `ExecutionBackend` 介面。replay 維持既有的 last-writer-wins 與 legacy／canonical 雙寫驗證，不新增 replay 拒絕規則。Stateless LLM backend 不受影響。

### 驗收

- resume 回傳不同 thread ID 時失敗，且沒有 binding event、沒有 `turn/start`。
- 同一 identity 在不同 attempt（含 attempt 計數重置）重複發布時冪等，只留下一筆新格式 event。
- 已有 durable binding S1 時發布 S2 會得到 `ExecutionIdentityConflictError`：event log 沒有 S2，projection 也沒有被改。
- projection 沒有 binding、但 canonical 有時（模擬 projection 更新失敗或 crash window），`RunAttempt` 走 `thread/resume` S1，不呼叫 `thread/start`。
- canonical lineage 無法讀取時，`RunAttempt` 在啟動 app-server 前失敗。
- branch A 在 fork 之後才寫入的 binding，不影響 branch B 的同名 task。
- 舊格式 key（`backend-session-bound:<task>:<attempt>:<session>`）的 event 仍會被 lineage 檢查看見。
- 既有 Codex、replay 與 stateless backend 測試通過。

### 主要檔案

`internal/team/subagent_binding.go`（主要邏輯）、`internal/team/codex_appserver_protocol.go`（resume ID 檢查）、`internal/team/subagent_codex.go`（只改呼叫點；該檔已超過 1100 行，不擴充邏輯）、相關測試。

## PR 3：彙整現有、可獨立的啟動前置檢查

### 拆分（2026-09-26）

- **3a**：只做第 1 點的順序調整，讓三個唯讀檢查在 `bindRunWorkspaceVersioning` 之前執行；不彙整結果。
- **3b**：其餘項目，即以 check ID 收集結果、依賴檢查標記 skipped、共用 `team check` 語意。
- `cmd/hufu/team_setup.go` 接近 800 行上限，3b 的聚合邏輯放在新檔案。

### 變更

1. 僅針對 `loadTeamCommon` 中可純讀或只修改記憶體設定的檢查：execution role／target 解析、`preflightExecutionTargets` 與 `LintEffectiveTeamContracts`。先完成 `applyConfiguredBackends` 等純記憶體設定，再把這組檢查移到 `bindRunWorkspaceVersioning` 之前；後者在 required mode 可能執行 workspace recovery。以穩定的 check ID 和原因碼收集結果，按 ID 排序後回報所有已執行的 required failure。角色解析失敗時，依賴其結果的 target check 標記為 skipped／dependency unavailable。
2. `preflightHistoricalExecutionTargets` 仍依其現有 workspace／replay 前提單獨 fail closed；EventStore、MCP、provider 的初始化或線上探測亦保留各自既有 gate。本 PR 不為了湊齊清單而在失敗後繼續有副作用的初始化。
3. 共用 `team check` 的 required／warning／not_checked 語意與安全文字截斷／遮罩規則，但不得讓 `team check` 建立 coordinator 或寫 workspace。CLI 的現有退出碼與 `--output json` 文件格式維持相容；run 啟動失敗時在人類可讀錯誤中列出多個原因。
4. 若其中某一檢查的實作會產生副作用，先保留它的原有執行位置，不將其納入聚合；不能為了聚合而放寬啟動 gate。

### 驗收

- 兩個互相獨立的 static failure 同時出現時一次列出，順序穩定；有依賴關係的 check 正確 skipped。
- required failure 時不觸發其後的 workspace versioning recovery、MCP／provider 建立或 session lifecycle 寫入；warning-only 仍可啟動。進入 `loadTeamCommon` 前既有的 workspace 路徑解析不在此保證範圍內。
- `hufu team check --output json` 的既有 schema、排序、redaction 與退出碼回歸測試通過；多 team prompt 不混用不同 team 的檢查結果。

### 主要檔案

`cmd/hufu/team_setup.go`、`cmd/hufu/execution_target_preflight.go`、`cmd/hufu/teamcheckcmd.go`、相關測試。若要共用結果型別，放在現有 operator／CLI 檢查層，避免核心 runtime 為 UI 建立新狀態機。

## 共通驗證與完成條件

每個 PR：

1. 檢查 `git diff -- cmd/hufu internal`，確認沒有 team 名稱分支或 team-specific policy 滲入核心。
2. 執行相關 package 的 focused tests、`go test ./...`、`go test -race ./...` 與 `golangci-lint run`；lint 必須成功。
3. 只更新受變更影響的正式架構／reference 文件；不得 stage、commit 或移動 `docs/tmp/` 下的檔案。

三個 PR 合併後，MCP manager 在所有 CLI 所有權路徑都會關閉；Codex binding 的 durable publication 與恢復不會產生歧義；啟動前置檢查能在不增加副作用的情況下一次回報多項獨立問題。
