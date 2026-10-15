# 假設與證據診斷：Coding Agent 實作規格

> 日期：2026-10-15
> 狀態：可開始實作；以下為待實作合約，不代表目前 CLI 已支援。
> 基準：`c03b785788fbe9b1f636e8b8baca745b6345114e`。
> 交付方式：依本文件完成程式、測試與使用指南；所有驗收皆可離線自動執行。
> Authority: reference；本文件為版本化實作計畫，程式與測試優先。使用者已授權移至本目錄並依 C1–C4 分階段提交。

## 1. 固定交付範圍

新增 `hufu inspect beliefs <run-id>`：在一次唯讀查詢中呈現已持久化的 decision 假設、檢查來源、狀態變更、來源統計，以及同一 run 的 evidence 驗證資訊。

產品需求是讓操作者從同一輸出追查「哪個假設、由何種來源檢查、對應哪些事件與證據」，並分清 worker 回報與 verification。以固定 fixture 驗證正確性與單次查詢覆蓋率；不承諾真人定位時間、任務成功率或 token 節省。

本次交付是獨立 inspect kind，名稱固定為 `beliefs`。保留既有命令與輸出合約。實作不再重新選擇產品方向，也不以真人評估、外部資料、live LLM 或研究實驗作為完成前提。

範圍只有查詢與投影。沒有新 runtime 設定、事件、資料表、observer、模型呼叫或執行控制；不修改 scheduler、prompt、acceptance、recovery、memory lifecycle 或既有 evidence hash。

## 2. 程式入口與責任

| 責任 | 既有入口 | 本次工作 |
| --- | --- | --- |
| journal 唯讀與完整性 | [event_store_stream.go](../../internal/team/event_store_stream.go)、[runtimestore/store.go](../../internal/runtimestore/store.go) | 沿用既有讀取 API，資料仍來自 `runtime.sqlite.events` |
| lineage、branch、run 選取 | [inspect/source.go](../../internal/inspect/source.go)、[inspect/run.go](../../internal/inspect/run.go) | 共用已載入 lineage，所有子投影使用同一份事件快照 |
| decision 狀態投影 | [decision_projection.go](../../internal/team/decision_projection.go)、[decision_store.go](../../internal/team/decision_store.go) | 新增窄的唯讀 adapter，重用 `projectDecision` 與 `readOnlyDecisionLineage` |
| assumption 合法來源與轉換 | [replan.go](../../internal/team/replan.go)、[decision_assumptions.go](../../internal/team/decision_assumptions.go) | 沿用既有狀態與驗證，額外保留事件錨點供呈現 |
| persisted provenance 統計 | [decision_engine_stages.go](../../internal/team/decision_engine_stages.go)、[decision_types.go](../../internal/team/decision_types.go) | 讀取 finalized record 內已記錄的統計，不重新推論來源獨立性 |
| evidence 驗證 | [inspect/evidence.go](../../internal/inspect/evidence.go) | 抽取接收已選定事件的內部 helper，舊 `InspectEvidence` 與新 kind 共用 |
| CLI、JSON、exit code | [inspectcmd.go](../../cmd/hufu/inspectcmd.go)、[inspect/types.go](../../internal/inspect/types.go) | 增加 kind、query validation、子命令與 renderer |
| 唯讀、receipt 關聯測試 | [required_contract_test.go](../../internal/inspect/required_contract_test.go)、[decision_index_readonly_test.go](../../internal/team/decision_index_readonly_test.go) | 沿用 fixture、檔案快照與無執行檢查模式 |

新增檔案固定以以下責任拆分，測試置於同 package：

- `internal/team/decision_inspection.go`：解析私有 decision payload、依 identity 分桶、重用 reducer、產出唯讀 DTO。
- `internal/inspect/beliefs.go`：查詢、scope 過濾、availability、與 evidence 組合。
- `cmd/hufu/inspectcmd_beliefs.go`：Cobra 命令與文字 renderer。

不建立 `internal/belief`。不匯出 `decisionState`、`decisionEvent` 或可寫入的 journal。adapter 不接受 Coordinator、provider、tool runner 或可寫 DB。

## 3. CLI 與查詢合約

```text
hufu inspect beliefs <run-id> --workspace <workspace>
hufu inspect beliefs <run-id> --task <task-id> --format json
hufu inspect beliefs <run-id> --task <task-id> --attempt 2 --output json
hufu inspect beliefs <run-id> --branch <branch-id-or-name> --session <session-id>
```

- `<run-id>` 恰好一個；沿用 common workspace、branch、session、format/output flags 與 completion。
- 新增局部 `--task`、`--attempt`。attempt 未指定為 0，表示不過濾；明確指定時須為正整數且同時指定 task。`--attempt 0`、負數、只有 attempt 一律 usage error。
- 不提供 `--at-event`、`--factor`、`--show-content`、`--all-branches`、`--agent` 或 `--all-agents`。
- facade 的 query validation 同時拒絕非空 `ProjectID`、`TeamID`、`AgentID` 與 `AllBranches=true`，避免非 CLI caller 傳入看似生效、實際被忽略的 filter。
- 空 run ID、非法 flags／format 在讀 workspace 前拒絕。
- task 先由選定 run 的 `ReplayTodoList` 驗證存在；attempt 依現有 `projectTaskWithEvents` 所辨識的 occurrence 驗證存在。不存在時沿用 `ErrNotFound`。
- task 存在但沒有 decision，不是查詢失敗；回傳 `not_recorded`。
- task/attempt filter 只過濾 `decisions`。`run_evidence` 始終明示為整個 run 的資訊，不把全 run 成敗誤稱該 attempt 的結果。
- 未指定 branch 時只用 active branch；名稱模糊不得猜測。session 跨多個候選時沿用 `selectRun` 的 ambiguity 規則。

## 4. 讀取流程、範圍與資源

1. 驗證 query，呼叫 `loadLineageWithLimits`。上限固定為 1,000,000 events、256 MiB 累計 event payload，與現有歷史查詢使用相同規模。這是資源拒絕上限，不是效能承諾。
2. 必須完整驗證 journal 後才輸出。超限或取消立即返回錯誤；不輸出 prefix 或截斷後的「完整」報告。
3. `selectRun` 取得 `selected.raw` 與 `selected.runEvents`。保存 query scope 及選定 lineage 的最後事件作 `source_cursor`；另保存完整驗證 journal 的最後事件作 `validated_head`。
4. 對 `selected.runEvents` 的 canonical decision event types 建立 decision ID 清單。外部文字或任意 JSON 中的 `decision_id` 不構成 decision。
5. adapter 接收 `selected.raw`、目標 run ID，將同一 decision 的事件依 canonical order 分桶。先完成 identity 檢查，再讀假設、統計及 refs。每個分桶只投影一次，不能對每個 decision 重掃整份 journal。
6. 重用 task replay 驗證 filter，最後過濾 DTO。沒有 attempt identity 的 decision 在未指定 attempt 時可見、`attempt: null`；指定 attempt 時排除並附 `binding_unavailable` 診斷，不能掛到最新 attempt。
7. evidence helper 接收同一個 `selected`，沿用 `auditverify.ExplainLineageRun` 和既有 manifest 投影，不再次呼叫 public inspector 重新載入 journal。
8. 全部資料與錯誤確定後才呼叫 `finishInspect`。子投影不得回頭重新載入 active branch、目前 team config 或 context store。

完整 journal 驗證可以讀 sibling 事件以檢查 hash chain，但它們不能成為結果資料、統計、診斷 refs 或來源清單。fork 後的祖先事件仍受現有 fork boundary 限制。

唯讀 store 目前會在私人 temporary directory 建立 SQLite snapshot，這是允許且必須清理的實作細節。禁止的是改動目標 workspace：不能新增／變更 DB、WAL/SHM、lock、index、cache、session 或 artifact；不禁止既有安全讀取實作所需的暫存快照。

## 5. Decision adapter 的確定性規則

### 5.1 來源 allowlist 與 identity

- 使用 `agent.DecisionEventTypes` 中目前 reducer 支援的事件與其既有常數；不能以 `strings.Contains` 或自由文字決定事件意義。
- 目標 decision 必須有可綁定到目標 run 的 canonical 事件。明確不同 run/session 的同名 decision 不可合併。
- 同 decision 的 lifecycle event 若缺 run/task/attempt，只能從同一合法 lineage 中已持久化的該 decision identity 補足缺值。非空值衝突視為 integrity error；不以 query filter 覆蓋。
- 缺 run 且找不到唯一 owning decision 的事件不掛入結果。已選定 decision 如有無法歸屬的相關歷史資料，標 `binding_unavailable`，不猜測。
- identity consistency 重用 `decisionState.applyEventIdentity` 所維護的規則。已選定 decision 的必要 JSON 無法解碼、缺 decision ID、或不支援 record schema 時，回傳有分類的 adapter error，由 inspect 包成 `ErrIntegrity`；不能默默當成空資料。
- 已知事件的冪等性沿用 branch-qualified key。相同 branch/key 的相同 material payload 不重複列 history；同 key 不同 material payload 回報 integrity error。不要把 query branch label 當原 event branch ID。

分桶後傳入 reducer 的必須是深拷貝。`projectDecision` 本身的 seen-key 使用字串 key，因此 adapter 在內存副本把非空 key 編成無碰撞的 `(原 event branch ID, 原 key)` 表示；空 branch 沿用既有 main 規則。原事件、hash、輸出 event ref 與 journal 不變，不將這份副本拿去重新驗證 hash chain。

### 5.2 finalized 與進行中資料

- finalized：以 `projectDecision` 的 `Record.Assumptions` 作目前狀態來源；保留 `Record.Stale` 與 `Invalidated` 的合法投影。state 重播錯誤直接回報，不能繞過既有驗證。
- 未 finalized：`lifecycle: in_progress`，只顯示已持久化 identity、事件 refs 與 `decision_not_finalized`；`assumptions: []`、`availability: partial`。不另建進行中 assumption reducer。
- 沒有任何 decision：`decisions: []`、整體 `availability: not_recorded`。仍可呈現 run evidence。
- finalized 但沒有 recorded assumptions：該 decision 的 `assumptions: []`、`availability: not_recorded`。不推論「已檢查所有前提」。
- 歷史 schema 以目前 `projectDecision` 的實際支援為準，不為此修改寫入端或放寬 reader。schema 不支援走 integrity error，stderr 標示 `unsupported_version`。

### 5.3 assumption 狀態與 history

每筆 finalized assumption 顯示 ID、critical、`EffectiveStatus()`、目前累積 evidence refs、以及同 decision／assumption 的狀態事件 history。狀態沿用 `unknown | supported | contradicted | stale`。

history 只來自 `assumption_supported`、`assumption_contradicted`、`assumption_stale` 的 typed payload，保留 event ID/hash/order、from/to、source、該事件自己的 evidence refs。不得解析 `Reason` 或 `Note`。

- 以 `ValidAssumptionSource`、`ValidAssumptionStatus` 和 `ApplyAssumptionTransition` 的既有條件確認 source、target status、declared assumption；不改變原 reducer 的語意。
- finalization 前的狀態事件只能列為 `application: not_applied`，不據此覆寫 finalized record；finalization 後且由 reducer 接受者列為 `application: applied`。adapter 以 finalized event 原始 assumptions 的內存副本逐筆驗證後置轉換，再與 `projectDecision` 最終狀態比對；不改 runtime reducer。前置事件的 source/status 與 declared ID 仍須合法，但不加入這次重播的 applied checks。
- 無法通過合法轉換驗證的事件不得稱為 applied。已選定 decision 出現此類非法資料時回報 integrity error；不以顯示層修復資料。
- `from` 缺省時輸出 null，不根據目前狀態倒推；有效的既有狀態可以原樣呈現。未知 enum 或事件 type 與 `to` 不一致為 integrity error。
- source 映射固定：`task_result → worker_claim`、`verification → verification`、`operator → operator_check`。
- `latest_check` 是最後一筆 applied history 的 event ref；沒有可綁定檢查即 null，即使 record 本身已有 supported 狀態。
- supported → contradicted 是歷史變化；不產生新的 `conflicted` 狀態。整個 decision stale 與個別 assumption stale 分欄顯示。

### 5.4 來源統計：只讀已記錄值

`applyProvenance` 使用 runtime trusted provenance 寫入 `DecisionRecord.SourceCount`、`IndependenceGroupCount`。封存 packet 的 `Provenance` 可包含 request/model 宣告，不能重新送進 `GroupEvidence` 當成可信來源。

輸出 `provenance.source_count`、`provenance.known_group_count`，直接讀 finalized event record 內的對應原始 JSON member；以 raw member presence 或 pointer DTO 區分「沒記錄」和 0，不能只讀已解碼 int 的預設零值。欄位不存在為 null，存在且為 0 就輸出 0；非整數或負值為 integrity error。

`known_group_count` 只是原本記錄的分組數，不是新的獨立性判定。沒有群組成員資料就不提供成員、parent graph 或推算的樣本數。SharedOriginWarnings 的自由文字不輸出；只輸出該 decision 已記錄的 `decision_evidence_shared_origin` event refs。

不得把多個 judge opinion、同一 artifact 多次引用或不同 provider 次數加進來源統計。第一版也不新增「總引用次數」「總 occurrence 數」等需要跨歷史格式重新猜測的全域計數。

## 6. Evidence 與資料安全

`run_evidence` 重用現有 `EvidenceData`，外包 `scope: run`。它包含 manifest、requirements、verification、acceptance 與 findings；不為 beliefs 建立第二套成功判定。

- 對完整 run 與 task/attempt filter 都呼叫同一個已選定 run 的 evidence helper；helper 抽取須保持舊 `InspectEvidence` bytes／語意相容。
- 沒有 `run_finished`／manifest 的行為沿用現有 unavailable 結果。其缺失不改 decision 的 availability。
- validator 判定失敗可以是成功查詢的資料，exit code 仍為 0；讀取或資料完整性失敗才是查詢錯誤。
- assumption refs 與 requirement refs 只並列展示；不做檔名、digest 相似度、task 名稱或文字語意 join。這樣也不會把一個 run requirement 的驗證結果錯算成某 assumption 已驗證。
- refs 是「事件記錄了這個引用」，不宣稱該 artifact 已通過驗證。實際完整性狀態由 evidence 區塊說明；belief adapter 不另讀任意 artifact path 或網路。
- 不讀 `context.sqlite`、private worker memory、prompt、task output、belief statement、judge rationale 或當前設定。沒有 show-content 入口。
- JSON/text DTO 僅含 allowlisted metadata，不 marshal `DecisionRecord`、`decisionEvent` 或 evidence packet 全文。
- IDs 與 refs 在內部比對使用原值；顯示副本沿用既有 secret redaction 與 opaque-ref 處理，文字 renderer 必須處理 ANSI/control characters。不要 redaction 後再拿去做 identity join。
- `ArtifactRef.Path`、URL、自由文字 reason/note、command、raw payload 不可從新 DTO 輸出；不可在診斷/error 中回顯整個壞 payload。

## 7. JSON 資料合約

沿用 `Envelope` schema version 1，新增 `KindBeliefs = "beliefs"`。新增 data 的 `projection_version` 固定為 1，不修改舊 kind 的 schema version。

所有集合輸出 `[]`，不輸出 null。以下明訂 nullable scalar／object 必須輸出 null，不以 0 或空字串代替。既有 `EvidenceData`、`ArtifactMetadata` 的 encoding 維持原狀。

| 結構 | 必備欄位 |
| --- | --- |
| `BeliefsData` | `projection_version`、`run_id`、`availability`、`source_cursor`、`validated_head`、`decisions`、`run_evidence` |
| event cursor/ref | `event_id`、`event_hash`、`ordinal`；ordinal 為完整 journal 的 1-based position |
| decision | `decision_id`、`run_id`、`task_id`（nullable）、`attempt`（nullable）、`lifecycle`、`availability`、`stale`、`evidence_hash`（nullable）、`record_ref`（nullable ArtifactMetadata）、`event_refs`（該 decision 的已接納 event ref[]）、`assumptions`、`provenance`、`reason_codes` |
| assumption | `assumption_id`、`critical`、`status`、`evidence_refs`（ArtifactMetadata[]）、`history`、`latest_check`（nullable event ref） |
| history | `event`、`from`（nullable）、`to`、`source`、`source_kind`、`application`、`evidence_refs` |
| provenance | `source_count`（nullable integer）、`known_group_count`（nullable integer）、`shared_origin_events`（event ref[]） |
| run evidence | `scope`（固定 `run`）、`data`（既有 EvidenceData） |

固定 enum：

- `availability`: `available | partial | not_recorded`。
- `lifecycle`: `in_progress | finalized`；stale 用獨立 boolean。
- `source_kind`: `worker_claim | verification | operator_check`。
- `application`: `applied | not_applied`。
- `reason_codes`: `decision_not_finalized | binding_unavailable | assumptions_not_recorded | provenance_not_recorded | check_source_not_recorded`。
- adapter error categories: `invalid_payload | identity_conflict | invalid_transition | unsupported_version | idempotency_conflict`。只出現在錯誤分類，不能偽裝成功 data。

新 enum 定義為 typed constants，測試核對全集；沿用的 assumption/source/evidence statuses 使用既有 constants 與 validator，不另造同義字。

availability 的計算順序：

1. 單筆 in-progress decision 為 partial。
2. finalized 無 assumptions 為 not_recorded。
3. finalized 有 assumptions，但 task/attempt binding 缺失、兩項 provenance 統計任一未記錄，或 supported/contradicted/stale assumption 找不到 applied check，為 partial，附對應 reason code。
4. 其餘為 available；unknown 本身合法且不要求 applied check。
5. 整體沒有 decision 或所有 decision 均為 not_recorded，為 not_recorded；有任何 partial、混合 available/not_recorded，或 filter 排除了 binding 不足的同 task decision，為 partial；其餘 available。若所有候選都因缺 attempt 而排除，整體 partial 而非 not_recorded。

排序固定：decisions 依 task ID（null 先）、attempt（null 先）、decision ID；assumptions 依 ID；history、event_refs、shared-origin events 依 ordinal；artifact refs 依 `(ID, Digest, Kind, MediaType, Bytes)` 排序並去重；reason codes 依 byte order。不以時間或 map iteration 決定順序。

不增加 snapshot/state digest、數值信心或生成時間。重播穩定性以固定來源下 JSON bytes 一致驗收；查詢期間有新事件時 cursor 可以不同。

`Envelope.Integrity` 只表示 journal／投影的完整性，不等於 evidence 驗證通過或任務成功。`availability: partial` 不自動改成 projection corruption。

## 8. 文字輸出與錯誤

文字輸出依序為 scope/cursor、availability、decision、assumption、history、provenance、run evidence。數值 null 顯示 `not_recorded`，零值顯示 `0`；task/attempt identity null 顯示 `unavailable`。清楚標註 `read-only diagnostic` 與 `Run evidence (whole run)`。

新增 renderer 使用既有輸出路徑，不強制 spinner 或 ANSI 顏色。plain、NO_COLOR、mono、epaper 下仍可讀；文字與 JSON 含相同核心事實及 reason codes。

| 結果 | 輸出與 exit code |
| --- | --- |
| 成功查詢，包括 partial/not_recorded、failed verification | 完整 envelope／text，exit 0 |
| 損壞 journal、已選定 decision payload/reducer/schema 錯誤 | stdout 不輸出成功報告，stderr 使用安全訊息，`ErrIntegrity`，exit 1 |
| run/task/attempt 不存在、branch/session 模糊、非法 query | 沿用 `ErrNotFound`／`ErrAmbiguous`／`ErrInvalidQuery`，exit 2 |
| 事件讀取超限 | `ErrInvalidQuery`，exit 2，不輸出部分結果 |
| query context 取消／deadline 到期 | 新 facade 在各 I/O 邊界檢查 `ctx.Err()`，保留原 cause 並包成 `ErrInvalidQuery`，exit 2，無部分 stdout |
| workspace／runtime store 不存在 | `ErrNotFound`，exit 2；不得建立 workspace |
| store 權限、snapshot、schema 或 legacy-workspace 讀取失敗 | `ErrIntegrity`，exit 1；沿用 store 既有有界 snapshot retry，不新增 retry loop 或 migration |

evidence helper 已定義的錯誤分類維持原契約，不用錯誤文字解析出新的業務狀態。JSON 模式失敗時仍沿用 `finishInspect` 的 stdout/stderr 合約，不另造 error envelope。

## 9. 工作事項與順序

### C1：新增 team 唯讀 adapter

建立 `decision_inspection.go` 與對應單元測試。輸入只有 context、已驗證且選定 lineage 的 events、target run identity；輸出不含全文的 decision DTO 與 typed errors。

依 §5 完成 event allowlist、identity 分桶、reducer 重用、transition history、原始 JSON member presence 與 persisted provenance。使用現有 decision event builders／fixture helper 製作資料。不得更動 write-side、runtime reducer 狀態機或原 record schema 來配合 inspector。

完成條件：T01–T06、T09、T12；同一 input slice 在投影前後深度相等；adapter 無檔案／DB／LLM／tool 存取。

### C2：新增 inspect facade

在 `types.go` 加 kind 與 validation，在 `beliefs.go` 依 §4 組合結果。將 `InspectEvidence` 的資料形成部分抽為接收 selected snapshot 的 helper，供兩種命令共用；不改既有 public API。

完成條件：T07–T11、T13–T15；固定 scope 下只載入一次 lineage；兩個子視圖來自同一 source cursor；未綁定資料不猜測。

### C3：接上 CLI 與 renderer

新增命令、flags、completion、render dispatch。固定使用 §7 JSON shape 與 §8 exit contract，不增加其他 UI 或 TUI 功能。

完成條件：T16–T18，CLI fixture 必須經實際 SQLite reader 及 facade，不只手工組 DTO。更新 `docs/guides/inspect.md`，提供 runnable examples、null／partial 語意與整個 run evidence 的 filter 說明；指南不得宣稱改善真人效率。

### C4：完整驗收與效能紀錄

執行 §10 的自動化檢查，完成 100／1,000 decisions 的離線 benchmark。回報硬體、Go 版本、fixture 規模、ns/op、B/op、allocs/op，分開列 adapter 和包含 journal 完整性驗證的 end-to-end 查詢。

Benchmark 是可重現的工程紀錄，不設依賴真人或生產資料的 adoption gate。用測試 spy 確認一次查詢只載入一次 lineage、每個 decision 分桶只 reducer 一次；避免以不穩定 wall-clock 門檻驗收。

完成條件：全部 Txx、必要命令通過、文件與 CLI 一致；每個階段驗收後建立獨立 commit。回報實際執行命令及 exit codes。

## 10. 自動化驗收矩陣

所有 fixture 都由 repository 測試 helper 在 `t.TempDir()` 內建立，不使用使用者 workspace、真人輸入、live provider 或外部服務。

| ID | Fixture／入口 | 可自動判定的預期 |
| --- | --- | --- |
| T01 | finalized assumption：unknown → supported → contradicted | reducer 最終 contradicted；history 依事件順序且有 refs；latest_check 指向最後 applied event |
| T02 | record 已 supported 但沒有 check event | 保留 supported、latest_check null、partial + check_source_not_recorded |
| T03 | in-progress 與完全沒有 decision 的 run | 前者 partial 且不推論 assumptions；後者 not_recorded；兩者不混淆 |
| T04 | 三種合法 transition source | 映射 worker_claim／verification／operator_check；operator 不冒充 verifier |
| T05 | 同 decision 出現壞 payload、非法 source/status、錯 identity 或未支援 schema | typed integrity error，不輸出成功報告；錯誤不含 raw payload |
| T06 | provenance member 缺省、明確 0、正值；packet 填假的 parent；三個 judges 重複同源 | null 與 0 區分，僅保留 persisted counts；不讀 model-declared lineage 增加計數 |
| T07 | fork ancestor、fork boundary 後事件、sibling 同 ID | 只使用選定 lineage；sibling 或 boundary 後資料不進輸出及聚合 |
| T08 | same task 的 attempt 1/2 與 legacy missing attempt | 精確 filter；不以新 attempt 補 legacy；排除時 partial 診斷；不存在 attempt 是 exit 2 |
| T09 | 重複 branch/key 相同／不同 payload | 相同資料不重複 history；不同 payload integrity error；不同 branch 不互相去重 |
| T10 | journal 最後一筆損壞 | 不得輸出前面合法 prefix；exit 1 |
| T11 | 缺 manifest、verification 失敗、completed_with_gaps | 沿用 EvidenceData 結果；驗證失敗不改 assumption，不重新判定 acceptance；成功查詢 exit 0 |
| T12 | finalized 後 invalidated；finalization 前 transition | 原 stale 與 lifecycle 正確；前置事件標 not_applied，不用來改 record |
| T13 | 同一 query 重複呼叫、map/ref 順序不同 | canonical 排序後 JSON 一致；canonical input 不變；新事件僅影響下一次快照 |
| T14 | 停止 writer 的 SQLite/WAL、缺 sidecar、唯讀目錄、missing workspace | before/after 檔案清單與內容一致，無新增 lock/cache/DB；temp snapshot 清理；不比較 atime |
| T15 | persisted verifier command 有 marker、provider/tool runner spy、private context sentinel | marker 不存在，provider/tool calls=0；不開 context DB、不讀私有內容；新 DTO 不輸出全文或路徑 |
| T16 | CLI text/JSON、--output alias/conflict、illegal attempt、help/completion | 固定欄位、null/0 語意、格式與 exit codes；非法格式在 I/O 前失敗 |
| T17 | NO_COLOR/plain/mono/epaper、ID 帶控制字元／已知 secret | 可讀且核心事實一致；無終端控制注入或 secret/raw reason 洩漏 |
| T18 | 舊 InspectEvidence 及其他 inspect commands golden | 舊 JSON/text、exit codes、workspace 副作用保持相容 |
| T19 | 小型注入讀取上限、取消、完整驗證前失敗 | bounded error／取消回傳，無部分 stdout、無重試執行；不用建 256 MiB 測試資料 |
| T20 | 完整整合診斷 fixture：假設、三種檢查、refs、persisted groups、失敗驗證、缺資料 | 單次 beliefs JSON 的所有 expected IDs/status/source/cursors 精確匹配 oracle；不得靠模型評分 |

T20 的 oracle 由固定 fixture 建立時宣告的事件 ID 與預期狀態組成，不能呼叫同一個新 projector 產生 expected。至少包含兩種 team identity，證明 core 沒有 team-name 分支；測試資料不依賴任何特定 team 的自然語言。

「單次查詢覆蓋率」定義為 T20 中列出的可取得 expected facts 在一次輸出正確呈現的比例，必須 100%；刻意未記錄的欄位必須正確為 null／not_recorded。這只驗收資料整合，不宣稱人類理解或實務診斷成功率。

## 11. 必跑命令與 Definition of Done

程式實作時使用 repository 既有 Go toolchain，不升級 dependencies。依適用 Go/runtime 技能實作，依序執行：

```bash
go test ./internal/team/... ./internal/inspect/... ./cmd/hufu/...
go test -race ./internal/team/... ./internal/inspect/...
go test ./...
go vet ./...
golangci-lint run
```

新增 benchmark 固定命名 `BenchmarkDecisionInspection`（team）及 `BenchmarkInspectBeliefs`（inspect），並執行：

```bash
go test ./internal/team -run '^$' -bench '^BenchmarkDecisionInspection$' -benchmem
go test ./internal/inspect -run '^$' -bench '^BenchmarkInspectBeliefs$' -benchmem
```

- [ ] C1–C4 全部完成，T01–T20 通過。
- [ ] 命令實際可用，schema、flags、renderer、exit codes 已接線。
- [ ] 所有新資料可追到持久化事件；無猜測、無補造 probability／attempt／source。
- [ ] 原 runtime outcomes、gates、prompt bytes、封存 hash、寫入與 recovery owner 不變。
- [ ] 所有必要測試、vet、lint 成功，保留實際命令與 exit codes；不得把未執行寫成通過。
- [ ] benchmark 數據及環境資訊已回報；固定 fixture 覆蓋率 100%。
- [ ] 只修改本規格列出的程式責任與必要測試／指南；沒有新 team 專用 core 分支。
- [ ] 本文件與階段驗證紀錄保留在正式 docs 目錄，`docs/tmp/` 不增加產物；測試暫存資料使用測試框架 temp dir。

本次文件修訂僅完成實作規格，沒有執行上述程式驗收。使用者已決定移除真人評估；本文件沒有需要真人測試、校準資料、外部採用審批或額外產品決策才能完成的工作項目。
