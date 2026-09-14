# Unified Trace / Replay / Context / Evidence Inspector 實作規格

> Status: active
> Authority: normative
> Verified-Commit: `b35e578`
> Supersedes: —
> Superseded-By: —
> Priority: P2

> 本規格已由 §10 所列的分階段提交實作。程式與測試若與本文衝突，依
> [文件權威順序](../README.md#authority-order)以程式與測試為準。

## 1. 決策與目標

建立一個唯讀的 `hufu inspect` facade，將 operator 目前需要分別查詢的資料
投影成同一套穩定輸出：

```text
hufu audit explain          → run outcome / evidence / attempt witness
hufu context explain        → context compiler trace
hufu context explain-memory → memory ranking / retrieval binding
hufu decision explain       → decision stage bindings
hufu team explain            → effective team configuration
EventStore / receipts / terminal lifecycle → durable execution facts
```

Inspector 的職責是查詢、排序、格式化與指出 projection drift；它不能改變
任何 runtime 決策，也不能新增另一份 truth store。

第一版只支援單一 workspace 內的 run/task/attempt 查詢，不引入 tracing backend、
OTel collector 或外部 observability service。

新增的 `overview` kind、Operator Snapshot、active-run selection 與 deterministic Next Action
由 [Operator experience](operator-experience.md) 定義。它是 additive extension：既有
run/task/trace/evidence/context/replay/storage payload、exit code 與 read-only invariant 不變。

## 2. 權威來源與 projection matrix

| 顯示內容 | canonical source | 允許使用的 projection | 禁止信任的來源 |
| --- | --- | --- | --- |
| run outcome | `workspace/logs/event_store.jsonl` 的 `run_finished` | `ReduceToSessionData`、`auditverify` | `session.json`、`report.md`、worker prose |
| task status/contract | event store task events | `ReplayTodoList` | TUI/log text |
| attempt/receipt/verification | task transition event payload 中的 receipt snapshot | `ReplayTodoList`、`ExecutionReceipt` | 任意 task ID 與 receipt 的自由 join |
| evidence | `EvidenceManifest` 與 artifact store | `auditverify` 的驗證與 findings | 自行重算 completion policy |
| decision | decision event lineage + DecisionRecord artifact | decision projection / `DecisionIndex` addressing | 直接把 DecisionIndex 當 truth |
| context item | `context.sqlite` 的 `context_items` | read-only repository query | FTS/vector/Markdown projection |
| context injection | task/session 的 `ContextInjectionManifest` | task replay | prompt 內容或 shadow trace 推測 |
| memory retrieval | `MemoryInjectionManifest`、`memory_*` events | task replay、memory event reducer | 以目前 policy 重新 retrieval |
| memory aggregate | `context.sqlite` experience aggregate | event-derived in-memory aggregate | 直接修改或重建 SQLite |
| terminal state | terminal lifecycle events + `terminal_sessions.json` | in-memory lifecycle reducer、read-only file read | 由 terminal 狀態推論 task 完成 |
| legacy telemetry | `execution-events.jsonl` | 僅 diagnostic comparison | 把 legacy telemetry 升格為 canonical |

目前 `audit explain` 已經提供 run witness、task attempt history，應由 inspector
直接呼叫或共用其底層純函式；不得複製 evidence 判定邏輯。[auditcmd.go](../../cmd/hufu/auditcmd.go)、
[auditverify explain](../../internal/auditverify/explain.go)

若 query 指定非 active branch，不得直接呼叫只讀取 active branch 的高階 helper；
必須先抽出接受已選定 lineage 的純函式，或以 branch-scoped input 呼叫同一套邏輯。

## 3. 查詢身份與 branch 規則

### 3.1 Common query

所有 inspector handler 先建立同一個 immutable query：

```go
type InspectQuery struct {
    Workspace string
    RunID     string
    TaskID    string
    Attempt   int
    BranchID  string
    SessionID string
    ProjectID string
    TeamID    string
    AgentID   string
}
```

規則：

1. `Workspace` 由 `--workspace` 或既有 workspace resolver 決定；空值使用既有預設。
2. `run`、`trace`、`evidence`、`replay` 的 positional ID 是 `RunID`。
3. `task`、`context` 的 positional ID 是 `TaskID`，必須同時提供 `--run`；
   `--attempt` 可選，未指定時顯示該 task 的所有 attempts。
4. `--branch` 未指定時只查詢 `session_tree.json` 的 active branch；指定時只查詢
   該 branch。第一版不得自動搜尋 sibling branches。若選定 lineage 內仍有多個
   candidate 同時符合 positional ID 與 filters，命令必須回傳 ambiguity，不得猜測。
5. `--session` 是額外的 exact filter；若指定，所有 matching event 的 `SessionID`
   必須一致。
6. context 查詢必須沿用現有 `--project`；私有 context 必須提供匹配的 `--agent`
   或明確的 `--all-agents` maintenance 權限。
7. ID 只作 lookup key，不作 authorization token；scope/branch/agent authorization
   在資料讀取前執行。

### 3.2 Canonical lineage loading

新增一個唯讀 lineage loader，流程固定為：

```text
StreamValidatedRunEvents
→ 驗證完整 global hash chain
→ LoadSessionTree
→ resolve branch
→ FilterEventsForBranch
→ filter run/session/task
```

`resolve branch` 必須在 run/task lookup 前完成。未指定 `--branch` 時，不得為了尋找
`RunID` 或 `TaskID` 掃描其他 branch；指定不存在的 branch、active branch 不存在，
或選定 lineage 內 lookup 結果不唯一，均回傳 exit 2。未來若需要跨 branch discovery，
必須以獨立的明示 flag（例如 `--all-branches`）另行設計，不得改變本版預設語意。

不得使用 `OpenEventStore` 作為 inspector 的第一層 reader，因為該 constructor 在
缺少檔案時會建立 `event_store.jsonl`。缺少 event store 應回傳 empty/unavailable
狀態，不得建立任何檔案。

branch lineage 的第一筆 event 可以有 predecessor；global chain 必須先驗證完整，
再驗證 branch slice，與現有 `auditverify` 語意一致。

## 4. 唯讀與副作用契約

Inspector 及其呼叫的 library 必須符合：

- 不呼叫 provider、LLM、MCP server、shell、terminal command 或 network。
- 不執行 `--recheck` 類 verifier；只做已持久化 metadata/hash 的驗證。
- 不建立、修改、刪除或 rename workspace 檔案。
- 不執行 SQLite migration、`PRAGMA journal_mode=WAL` 或任何 mutation。
- 不 append EventStore、decision index、context event 或 memory event。
- 不持久化 trace cache、report、replay snapshot 或 drift result。
- 可使用記憶體內的 slices/maps 建立 projection，命令結束即丟棄。

為滿足上述契約，實作需提供：

1. `team` 的 read-only event lineage reader（沿用 `StreamValidatedRunEvents`）。
2. `team` 的 read-only artifact store opener；不得使用會 `MkdirAll` 的寫入 constructor。
3. `auditverify` 的 read-only verification/projection entry point；它不得建立 artifact
   directories 或執行 deterministic verifier。
4. `context` 的 `OpenSQLiteReadOnly`，使用 SQLite read-only/query-only 模式，
   不套用 migration；missing DB 應明確回報 unavailable。

若上述 seam 尚未存在，先在 PR-0 實作 seam；不得以「呼叫既有 API」為理由破壞
Inspector 的唯讀契約。

## 5. Projection data contract

### 5.1 TraceRef

`TraceRef` 是輸出 projection identity，不嵌入或修改任何 persisted type：

```go
type TraceRef struct {
    RunID           string `json:"run_id,omitempty"`
    SessionID       string `json:"session_id,omitempty"`
    BranchID        string `json:"branch_id,omitempty"`
    TaskID          string `json:"task_id,omitempty"`
    Attempt         int    `json:"attempt,omitempty"`
    AgentID         string `json:"agent_id,omitempty"`
    ExecutionTarget string `json:"execution_target,omitempty"` // backend/model
    ToolCallID      string `json:"tool_call_id,omitempty"`
    EventID         string `json:"event_id,omitempty"`
    EventHash       string `json:"event_hash,omitempty"`
    EventOrdinal    int64  `json:"event_ordinal,omitempty"`
    ParentEventID   string `json:"parent_event_id,omitempty"`
    Source          string `json:"source"`
}
```

規則：

- `ExecutionTarget` 必須來自 canonical `ExecutionTarget{Backend, Model}` 或已驗證的
  receipt/payload，不得由目前 CLI model flag 推測。
- `EventOrdinal` 是本次完整 event stream 的 1-based file order；它不是 persisted
  sequence，也不得在輸出中稱為 durable sequence。
- 非 event source 的 `EventOrdinal` 為零；不得讓零值參與 event timeline 排序，必須依
  6.3 節的 anchor/unanchored 規則排序。
- 沒有 persisted parent relation 時 `ParentEventID` 留空；不得依時間或 task ID 猜 parent。
- `EventHash` 可輸出完整 hash 或固定長度 prefix，但同一 format 必須固定。

### 5.2 TraceEntry

```go
type TraceEntry struct {
    Ref                TraceRef `json:"ref"`
    Kind               string   `json:"kind"`
    AnchorEventID      string   `json:"anchor_event_id,omitempty"`
    AnchorEventOrdinal int64    `json:"anchor_event_ordinal,omitempty"`
    Timestamp          string   `json:"timestamp,omitempty"`
    Status             string   `json:"status,omitempty"`
    ReasonCode         string   `json:"reason_code,omitempty"`
    Refs               []string `json:"refs,omitempty"` // opaque IDs/digests only
}
```

`AnchorEventID` 與 `AnchorEventOrdinal` 只供 projection ordering 使用，不寫回
persisted type，也不是新的 durable sequence：

- event entry 的 anchor 是自身 event。
- receipt 的 anchor 是持久化或引用該 receipt 的 task/tool/attempt event。
- evidence manifest 的 anchor 是引用它的 evidence/completion event。
- decision/context/memory/terminal projection 的 anchor 是引用該 record 的 lifecycle event。
- 找不到 persisted reference 時不得依 timestamp、task ID 或鄰近位置猜測；anchor 留空、
  `AnchorEventOrdinal` 為零，並設定 `ReasonCode: missing_anchor`。

`ReasonCode` 只能使用既有 bounded failure/recovery/policy/verifier code，或本 schema
版本定義的 inspector code。第一版 inspector code 至少固定包含：

```text
missing_anchor
projection_not_run_scoped
projection_changed_during_read
optional_projection_missing
projection_unreadable
legacy_schema_unsupported
```

不得把 raw error、command、stdout、stderr、prompt、tool args 或完整 model response
放入 `TraceEntry` 或 `ProjectionCheck`。需要人類可讀說明時，只能使用固定模板加上
已 redacted、長度受限的 metadata。

### 5.3 Common JSON envelope

所有 `--format json` 輸出使用以下 envelope；欄位順序由 struct 定義固定，陣列必須
明確排序：

```json
{
  "schema_version": 1,
  "kind": "run|task|trace|evidence|context|replay",
  "query": {"run_id": "", "task_id": "", "branch_id": ""},
  "integrity": {
    "event_chain": "verified|invalid|unavailable",
    "projection": "consistent|drift|unavailable"
  },
  "data": {},
  "diagnostics": []
}
```

JSON contract 規則：

- `schema_version` 必填；新增欄位只能向後相容地加入。
- `data` 不因 text/json 改變語意；JSON 不輸出 ANSI。
- `diagnostics` 只含 code、severity、message、ref；不得含 raw content。
- map 一律轉為 sorted key/value array 或先排序後 marshal。
- 不使用當前時間、指標記憶體位址或 map iteration order 造成不穩定輸出。

各 `kind` 的 `data` 至少包含以下欄位；額外欄位只能是同一 canonical source 的
metadata projection：

| kind | required data fields |
| --- | --- |
| `run` | `run_id`, `terminal_event_id`, `outcome`, `acceptance`, `task_summary`, `attempt_summary`, `evidence_refs` |
| `task` | `run_id`, `task_id`, `status`, `phase`, `agent_id`, `execution_target`, `attempts`, `artifact_refs`, `context_refs`, `memory_refs` |
| `trace` | `run_id`, `entries` |
| `evidence` | `run_id`, `manifest`, `requirements`, `artifact_refs`, `verification`, `acceptance`, `findings` |
| `context` | `run_id`, `task_id`, `attempts`, `manifests`, `items`, `authorization` |
| `replay` | `run_id`, `event_chain`, `checks`, `overall_status` |

## 6. CLI contract

```bash
hufu inspect run <run-id> [--branch <branch>] [--format text|json]
hufu inspect task <task-id> --run <run-id> [--attempt <n>] [--branch <branch>] [--format text|json]
hufu inspect trace <run-id> [--branch <branch>] [--format text|json]
hufu inspect evidence <run-id> [--branch <branch>] [--format text|json]
hufu inspect context <task-id> --run <run-id> [--attempt <n>] [context flags]
hufu inspect replay <run-id> [--branch <branch>] [--format text|json]
```

`inspect` 的 flags：

```text
--workspace, -w
--branch
--session
--run              # task/context 子命令必填
--attempt
--format           # text (default) 或 json
--project          # context 子命令必填
--team
--agent
--all-agents
--show-content     # 僅 context；需通過 scope/authorization
```

Process exit code 固定為：

```text
0  projection 成功；run 的 outcome 可以是 failed/partial
1  canonical integrity 失敗，或 replay 發現 drift
2 參數錯誤、run/task 不存在、結果 ambiguity、scope/authorization 拒絕
```

optional projection 缺失而沒有證據顯示 drift 時，輸出 `unavailable/skipped` 並回傳
0；canonical event store 不可讀或 required payload 不合法則回傳 1。

不得另造 `--json` 語意；若要相容既有 CLI，可接受 `--json` 作為 `--format json`
的 deprecated alias，但 help 與文件以 `--format` 為準。

### 6.1 `inspect run`

顯示：canonical `run_finished`、expected/derived outcome、acceptance state、
completion state、task summary、attempt summary、evidence manifest reference、
integrity diagnostics。run 本身即使 outcome 是 failed/partial，也不代表 inspect
命令失敗；只要資料成功讀取，命令 exit 0。

### 6.2 `inspect task`

顯示該 run/branch/task 的：

- canonical task status、phase、agent、execution target/topology
- retry/reset/recovery decision 與 reason code
- 每個 attempt 的 receipt identity、exit code、verification status
- artifact/evidence/context/memory opaque references

不得顯示 task output、transcript、tool args 或 verifier stdout/stderr。

### 6.3 `inspect trace`

輸出該 run lineage 的 TraceEntry。event 與具有可信 persisted reference 的 supplemental
entry 依 anchor 排序，排序鍵固定為：

```text
anchored group
→ anchor event ordinal
→ entry class（event entry 在前，supplemental entry 在後）
→ timestamp
→ source
→ kind-specific stable key
```

沒有可信 anchor 的 supplemental entries 一律排在完整 event timeline 之後，並以
`source → timestamp → kind-specific stable key` 排序。所有 stable key 都必須由 persisted
opaque ID/digest 組成；不得使用 map iteration order 或目前時間。

若 entry 來自 receipt/manifest 等非 event source，必須保留 `Source`，其
`Ref.EventOrdinal` 維持零，且不得假裝它具備 event hash 或 global ordinal。它只能透過
`AnchorEventOrdinal` 靠近引用它的 event；無 anchor 時必須輸出 `missing_anchor`。

### 6.4 `inspect evidence`

只投影既有 `auditverify` 結果與 manifest metadata：

```text
manifest hash/status
requirements and statuses
artifact IDs/digests/metadata
verification refs and exit status
acceptance state
missing/invalid refs
```

必須使用 auditverify 的既有 verification primitive。不得在 inspector 重寫
manifest hash、artifact verification 或 completion gate。artifact bytes 永不由
此命令輸出。

### 6.5 `inspect context`

預設 metadata-only，顯示：

```text
context ID, source type/ref, authority, trust, scope, lifecycle
included/omitted, token count, omission reason
retrieval ID, rank, score parts, policy version  # 僅 memory manifest 有值時
request/manifest fingerprint, attempt, model execution ID
```

`ContextInjectionManifest` 與 `MemoryInjectionManifest` 必須分開標示；若只有一般
context manifest，不得捏造 retrieval ID/rank/policy version。

`--show-content` 只有在 `--project` scope、agent authorization 與既有 context
redaction 通過後才可輸出 redacted content。授權失敗時 fail closed，不回傳原文。

### 6.6 `inspect replay`

不重新執行 agent、provider、verifier 或 acceptance。它只在記憶體中重播並比較
projection，輸出見下一節。

## 7. Replay consistency contract

### 7.1 Replay phases

```text
讀取並驗證完整 EventStore hash chain
→ 選定 branch lineage
→ replay SessionData / TodoList
→ project run outcome / evidence / decisions / memory / terminal
→ 讀取既有 projections（read-only）
→ 以 projection-specific equivalence 比較
→ 輸出 match/drift/unavailable
```

### 7.2 比較對象

| Projection | replay input | stored projection | 比較規則 |
| --- | --- | --- | --- |
| task | task events | `session.json` task fields | 只比較 canonical durable fields；忽略 checkpoint-only timestamps |
| run | `run_finished` reducer | `session.json` run result/report metadata | run result 以 canonical event 為準；report 只報 drift |
| decision | decision events + record refs | DecisionIndex latest row | index 是可重建 projection；outcome fields 依其獨立 lifecycle event 比較 |
| memory | `memory_*` events | experience aggregate rows | 只建立 in-memory expected aggregate；不得寫 SQLite |
| terminal | terminal lifecycle events | `terminal_sessions.json` | 比較 lifecycle identity/state；process completion 不轉換成 task status |

`execution-events.jsonl`、TUI、STM/LTM Markdown 與 report 可列為 diagnostic-only，
第一版 replay 不得以它們作 canonical comparison input。

#### `session.json` applicability gate

`session.json` 是 active branch head 的可變 checkpoint projection，不是任意歷史 run
的 run-scoped truth。task/run projection 只有在下列條件全部成立時才可比較：

1. query 選定的是讀取開始時 `session_tree.json` 所記錄的 active branch。
2. 目標 run 在該 branch lineage 中恰有一個合法的 `run_finished`。
3. 在目標 `run_finished` 之後，不存在 `RunID` 非空且不同於目標 run 的 event；也就是
   目標 run 仍是 active branch head 所代表的最新 run，而不是僅為最新 completed run。
4. 讀取 `session.json` 後再次讀取 `session_tree.json`；active branch 必須仍與第一次
   相同。若不同，該 projection read 不具一致 snapshot，不得比較。

只要任何條件無法證明，canonical replay 仍須正常輸出，但 task/run check 必須回傳：

```text
status: unavailable
reason_code: projection_not_run_scoped
```

此狀態不是 drift，且不使命令回傳 exit 1。若兩次 session tree 讀取之間 active branch
改變，使用 `projection_changed_during_read`，同樣視為 unavailable。不得僅因
`session.json` 中碰巧存在相同 task ID、相同狀態或相似時間，就推定它屬於目標 run。
若未來 persisted projection 加入可驗證的 immutable run/branch provenance，可用該
provenance 取代上述 head gate；第一版不得修改 persisted schema 只為支援 inspector。

### 7.3 Drift result

```go
type ProjectionCheck struct {
    Name         string   `json:"name"`
    Status       string   `json:"status"` // match, drift, unavailable, skipped
    ComparedRefs []string `json:"compared_refs,omitempty"`
    DiffPaths    []string `json:"diff_paths,omitempty"`
    ReasonCode   string   `json:"reason_code,omitempty"`
}
```

`DiffPaths` 只能是 bounded field paths，例如 `tasks[0].status`；不得放入左右兩邊
的 raw value。未知 event type、legacy schema 或缺少 optional projection 應標為
`unavailable/skipped`，除非 canonical integrity 本身失敗。

Replay 必須區分：

- `invalid`：canonical event chain 或 required payload 不合法。
- `drift`：canonical replay 與既有 projection 在可比較欄位不一致。
- `unavailable`：缺 projection 或缺 optional legacy data，不能證明 drift。
- `match`：比較範圍內完全一致。

## 8. Security and redaction

- 預設 metadata-only。
- secret names、secret refs、provider keys 與 credentials 保持 opaque。
- 不輸出 raw tool args/output、command、environment dump、prompt、transcript 或 chain-of-thought。
- `ReasonCode` 優先於自由文字；自由文字若為必要診斷，必須先通過既有 JSON/text redactor 並限制長度。
- `--show-content` 只適用 context content，不能解鎖 artifact bytes 或 transcript。
- artifact/context authorization 必須在 adapter 邊界執行；CLI handler 不得繞過。
- 所有資料錯誤、redaction error、authorization error 均 fail closed。

## 9. Package boundary and files

### 9.1 New package

```text
internal/inspect/types.go       # public projection/query/output structs
internal/inspect/source.go      # read-only source loading and identity resolution
internal/inspect/run.go         # run/task projections
internal/inspect/trace.go       # event/receipt/manifest adapters and ordering
internal/inspect/context.go     # context/memory metadata projection
internal/inspect/evidence.go    # auditverify-backed safe evidence projection
internal/inspect/replay.go      # pure replay and projection comparison
cmd/hufu/inspectcmd.go          # Cobra command and rendering only
```

### 9.2 Required supporting seams

```text
internal/team/                 # read-only lineage/receipt/terminal adapters if needed
internal/auditverify/           # read-only verification and safe evidence view
internal/context/               # OpenSQLiteReadOnly and read-only aggregate queries
```

Dependency direction：

```text
cmd/hufu → internal/inspect
internal/inspect → internal/team, internal/auditverify, internal/context
internal/auditverify → internal/team
internal/team ↛ internal/inspect
```

`internal/inspect` 不可被 runtime coordinator import，避免 inspection logic 反向
成為執行路徑或造成 import cycle。不得在 `internal/inspect` 維護任何 append-only
file、SQLite table、package-global cache 或 second truth store。

## 10. Implementation commits

### PR-0：contract and read-only seams（`5663358`）

- 加入 inspect output schema/version、query validation、exit code contract。
- 實作 read-only event/artifact/SQLite readers。
- 補測試確認缺檔時不建立 workspace 檔案。

### PR-1：run/task inspector（`889e89c`）

- 以 branch-aware canonical lineage 為唯一輸入。
- 未指定 `--branch` 時只解析 active branch，不掃描 sibling branches。
- 重用 `ReduceToSessionData`、`ReplayTodoList` 與 `auditverify.ExplainRun` 的純 projection。
- 完成 `inspect run/task` text/json。

### PR-2：evidence/context/memory/decision/terminal adapters（`86cb4bb`）

- evidence 只呼叫 auditverify seam。
- context/memory 分離一般 injection 與 memory retrieval。
- decision index 僅作 addressing；terminal 僅投影 process facts。

### PR-3：unified trace（`612063c`）

- 實作所有 source adapters、anchor-based stable ordering、opaque refs 與 bounded diagnostics。
- unanchored supplemental entry 排在 event timeline 後並標示 `missing_anchor`。
- 不修改 persisted schema。

### PR-4：replay consistency（`95f8ac7`）

- 實作 in-memory reducers 與 projection-specific equivalence。
- 在比較 `session.json` 前套用 active-branch/latest-run applicability gate。
- 輸出 field-path drift，不輸出 raw values。

### PR-5：CLI/docs/completion（`b35e578`）

- 加入 help、completion、正式文件與 migration note。
- 若保留既有 `audit/context/decision` commands，明確把 inspect 定位為 facade，
  不重複或取代既有 command 的 canonical semantics。

## 11. Required tests

### Query and identity

```text
TestInspectRequiresRunForTaskAndContext
TestInspectRejectsAmbiguousTaskAcrossRuns
TestInspectWithoutBranchUsesOnlyActiveBranch
TestInspectExplicitBranchUsesOnlySelectedLineage
TestInspectDoesNotSearchSiblingBranches
TestInspectRejectsAmbiguityWithinSelectedLineage
TestInspectSessionFilter
```

### Canonical projection

```text
TestInspectRunUsesCanonicalRunFinished
TestInspectTaskUsesReplayTodoList
TestInspectTaskUsesFrozenExecutionTarget
TestInspectReceiptDoesNotJoinArbitraryTaskID
TestInspectEvidenceDelegatesToAuditverify
TestInspectMemorySeparatesGeneralAndMemoryManifest
TestInspectTerminalDoesNotInferTaskCompletion
```

### Ordering and JSON

```text
TestInspectTraceOrderingUsesEventOrdinal
TestInspectTraceAnchorsSupplementalEntryAfterReferencingEvent
TestInspectTracePlacesUnanchoredEntryAfterTimeline
TestInspectTraceMarksMissingAnchor
TestInspectTraceDoesNotInventParentEvent
TestInspectJSONStable
TestInspectJSONHasSchemaVersion
TestInspectJSONDoesNotExposeRawValues
TestInspectTextAndJSONHaveSameProjection
```

### Replay and read-only safety

```text
TestInspectReplayDetectsTaskProjectionDrift
TestInspectReplayDetectsMemoryAggregateDrift
TestInspectReplayReportsUnavailableOptionalProjection
TestInspectReplayComparesSessionOnlyForActiveBranchLatestRun
TestInspectReplayReportsHistoricalSessionProjectionUnavailable
TestInspectReplayReportsNonActiveSessionProjectionUnavailable
TestInspectReplayReportsProjectionChangedDuringRead
TestInspectReplayRejectsBrokenGlobalHashChain
TestInspectReplayNeverExecutesProvider
TestInspectReplayNeverExecutesVerifier
TestInspectDoesNotCreateMissingEventStore
TestInspectDoesNotCreateMissingArtifactDirectories
TestInspectDoesNotRunSQLiteMigration
TestInspectDoesNotPersistNewTruth
```

### Authorization and redaction

```text
TestInspectContextHidesContentByDefault
TestInspectContextRequiresProjectScope
TestInspectContextRejectsPrivateContentWithoutAgent
TestInspectContextRedactsExplicitContent
TestInspectContextFailsClosedOnRedactionError
TestInspectTraceDoesNotExposeTerminalCommand
TestInspectTraceDoesNotExposeReceiptTranscript
```

## 12. Definition of done

完成後，operator 可以在不 grep 多個檔案、不中斷或重新執行任何 agent 的情況下回答：

```text
為何 retry？                 → task attempt/recovery reason code
用了哪個 backend/model？      → frozen ExecutionTarget
哪個 verifier 擋住？         → persisted verification status/ref
缺哪個 evidence？            → auditverify-backed manifest findings
為何注入某 memory？          → persisted MemoryInjectionManifest/rank/policy
resume 後 projection 有無 drift？ → inspect replay ProjectionCheck
```

以下任一情況不算完成：

- 使用 `session.json`、report 或 current configuration 作為 canonical outcome。
- Inspector 執行 provider、shell、MCP、verifier、migration 或 rollback。
- Inspector 寫入任何 workspace state。
- JSON 輸出未版本化、排序不穩定或包含 raw secret/content。
- 未提供 task/context 所需的 run，或在選定 branch lineage 內結果不唯一時自動猜測。
- 未指定 `--branch` 時掃描 sibling branches，或拿不具 run-scoped provenance 的
  `session.json` 對歷史 run 判定 drift。
- 以零值 `EventOrdinal` 把非 event entry 排在 timeline 前面，或替它猜測 anchor。
