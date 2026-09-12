# Execution Compatibility Sunset 實作計畫

> Status: archived
> Authority: reference
> Verified-Commit: `d5e50bd`（PR-1～PR-4 與 v2.0.0 warning-release evidence）
> Superseded-By: [execution compatibility deprecation record](../../deprecations/execution-compatibility.md)
> Priority: P1
> Historical-Baseline: `7815dc728ab79488de86ee7680c177b9347ac18a`
> Canonical authority: [execution runtime](../../architecture/execution-runtime.md)
> Scope: legacy `local` execution-backend alias、provider-era **durable execution identity** writers/readers

本文件是 PR-1～PR-4 的歷史實作計畫；這些階段及 v2.0.0 warning release 已完成。
尚未開放的 PR-5 removal gate 由正式 deprecation record 持續追蹤。本文件不得作為重新
實作既有機制或提前移除 legacy reader 的請求。

## 1. 結論與開始條件

本計畫可自 PR-1 開始實作。PR-1 至 PR-4 都維持舊 workspace 可讀；PR-5
是獨立、release-gated 的 removal PR，不得與前四個 PR 合併。

PR-4 分成兩個不同責任：coding PR 實作 warning／observer 與測試；release owner 在
實際發版時更新正式 deprecation record、發行說明與 tag。沒有已發布 tag 或 production
inventory 時，coding agent 不得宣告 PR-5 可開始，也不得把 placeholder 改成推測的版本。

正確順序：

```text
inventory current writers/readers
→ finish write-ban
→ read-only inspector + saved fixtures
→ append-only materializer
→ warning + one complete release window
→ runtime reader removal
→ durable struct cleanup
```

任何階段發現 canonical owner、branch lineage 或 migration evidence 無法唯一決定時，
停止實作並補設計；不得以 current config、CLI flags 或 provider availability 猜測舊
execution identity。

## 2. 已確認的 baseline 事實

Baseline 已具備：

- 新 task occurrence 在 admission 時產生 `ExecutionTarget` 與
  `ExecutionTopology`。
- canonical checkpoint marshal 會抑制 `Model`、`ModelTopology`、
  `SubagentProvider`、`ProviderBinding` shadow。
- canonical task lifecycle event 只在 target-less historical projection 才寫 legacy
  fields。
- `backend_session_bound` 是新 writer；`provider_session_bound` 只供 replay。
- target-less legacy task 已可由 `execution_target_migrated` v1 在 dispatch 前遷移。
- ambiguous qualified legacy model 會 fail closed，而且 current config 不是 evidence。

Baseline 尚未完成完整 write-ban：

- `ExecutionReceipt.SubagentProvider` 仍由新 attempt 寫入。
- `ExecutionPolicySnapshot.ModelRoutes[].LegacyProvider` 仍由新 run 寫入。
- `execution-events.jsonl` shadow 的 `ExecutionEvent.Provider` 仍由新 event 寫入。
- typed `ExecutionTarget{Backend: "local"}` 仍會原樣 replay；現有
  `migrateLegacyExecutionTarget` 因 target 非零而跳過。

因此 PR-1 是「完成 write-ban 並固化既有 invariant」，不是重新實作已存在的
task-event suppression。

## 3. Canonical 與 legacy boundary

### 3.1 Canonical durable execution identity

```text
ExecutionTarget{backend, model}
ExecutionTopology[]ExecutionTarget
BackendBinding
ExecutionReceipt.Backend
ExecutionPolicySnapshot.ModelRoutes[].{model, backend, provider_key}
ExecutionEvent.{execution_target, backend, backend_kind}
```

`provider_key` 是 LLM transport adapter 的 canonical configuration identity，不是本計畫
要移除的 provider-era task identity。

### 3.2 本計畫要 sunset 的 surface

| Surface | Legacy representation | PR-1～PR-4 | PR-5 後 |
| --- | --- | --- | --- |
| Typed target/topology/binding | backend `local` | read + inspect + migrate；不再寫 | runtime 拒絕；只有 migration CLI 可讀 |
| Todo/session/task events | `model`, `model_topology`, `subagent_provider`, `provider_binding` 作 execution identity | historical read only | runtime reducer 不讀 execution identity；migration CLI 可讀 |
| Session binding event | `provider_session_bound` | replay + migrate only | runtime unknown-event semantics；migration CLI 可讀 |
| Execution receipt | `subagent_provider` | dual-read；PR-1 起只寫 `backend` | runtime 只讀 `backend` |
| Execution policy snapshot | route `legacy_provider` | v3 read；PR-1 起 v4 只寫 canonical route | runtime 只讀 v4；舊 snapshot 必須先 materialize 或 fail actionable |
| Execution-event shadow | `provider` | old file read/export compatibility；PR-1 起只寫 `backend` | 不讀/不寫 `provider` |
| Authored alias | `local/<model>`、`default-llm-backend: local`、`backends.local`、`providers.local` | accept + canonicalize；PR-4 warning | reject with `use ollama` diagnostic |

### 3.3 明確不在本計畫移除的名稱

以下是 transport/config adapter 或非 execution-domain provider，不得因文字相同而刪除：

- `agent.ProviderManager`、`config.ProviderConfig`、`providers:`；
- `ExecutionBackendPolicySnapshot.ProviderKey`；
- transient `AttemptRequest.Provider` / `ProviderBinding` adapter fields；
- internal `SubagentProvider` protocol interface、`SubagentCapabilities`、
  `SubagentRegistry`、`HufuLocalSubagentProvider`；
- `subagent-provider-default`、`subagent-providers`、agent frontmatter
  `subagent-provider` authoring compatibility；
- memory、decision 或 artifact schemas 中語意不同的 `provider` fields。

這些 authored settings 可以在 admission 時轉成 canonical target，但 PR-1 起不得再被寫入
durable task identity。若要移除它們，必須另開 public configuration deprecation。

## 4. Code-owned compatibility inventory

新增 dependency-light package：

```text
internal/executioncompat/
```

此 package：

- 可 import `internal/execution`；
- 不可 import `internal/team`，避免 cycle；
- 使用自己的 legacy wire structs 解析 `json.RawMessage`；
- 不做 I/O、不讀 config、不解析 team files；
- 提供 pure detection、derivation、classification。

核心型別：

```go
type Feature string

const (
    FeatureLocalAlias              Feature = "local_alias"
    FeatureProviderShadowFields    Feature = "provider_shadow_fields"
    FeatureLegacyProviderBinding   Feature = "legacy_provider_binding"
    FeatureProviderSessionEvent    Feature = "provider_session_event"
    FeatureLegacyReceiptProvider   Feature = "legacy_receipt_provider"
    FeatureLegacyPolicyRoute       Feature = "legacy_policy_route"
    FeatureLegacyResumeMigration   Feature = "legacy_resume_migration"
)

type Classification string

const (
    ClassificationCanonical     Classification = "canonical"
    ClassificationMigrated      Classification = "migrated"
    ClassificationMigratable    Classification = "migratable"
    ClassificationAmbiguous     Classification = "ambiguous"
    ClassificationUnmigratable  Classification = "unmigratable"
    ClassificationNotApplicable Classification = "not_applicable"
)
```

每一個 task subject `(branch_id, task_id)` 及 policy subject
`(branch_id, source_event_id)` 恰有一個 classification。Feature counts 可重疊；同類
subject 的 classification 不可重疊。

Policy subject 是該 branch 可見、仍會影響 replay 的每個 v3 snapshot event；若只有
`session.json` snapshot，subject key 改用 `(branch_id, "session", source_digest)`。
後來原生寫入的 v4 snapshot 是另一個 canonical subject，不會自動覆蓋舊 v3 subject；
舊 v3 只有被明確的 policy migration event涵蓋後才算 migrated。不得只掃「最新一筆」
而漏掉 canonical-only replay 仍會遇到的歷史 event。

`internal/team/execution_compatibility_bridge.go` 負責：

- 將 checked `RunEvent`/session projection 轉成 `executioncompat` input；
- 建立所有 branch lineage；
- append canonical migration event；
- 將 pure result 套用到 runtime projection。

`cmd/hufu` 不可自行重做 migration derivation。

## 5. Write-ban invariants（PR-1）

### 5.1 Durable writer invariants

1. 新 `task_created` 及後續 task lifecycle events不得寫 `local`、`model`、
   `model_topology`、`subagent_provider` 或 `provider_binding` 作 execution identity。
2. 新 `session.json` task projection 只寫 `ExecutionTarget`、
   `ExecutionTopology`、`BackendBinding`。
3. 新 `ExecutionReceipt` 新增：

   ```go
   Backend string `json:"backend,omitempty"`
   ```

   writer 必須從該 attempt 的 admitted `ExecutionTarget.Backend` 取得；不得從
   `TaskDef.SubagentProvider` 反推。`SubagentProvider` 暫留 dual-reader shadow，當
   `Backend != ""` 時 custom marshal 不得輸出 `subagent_provider`。
4. `ExecutionPolicySnapshot` bump 至 version 4。新 route 不寫
   `LegacyProvider`；同 model 的多 backend route 以 `(Model, Backend,
   ProviderKey)` 排序及計入 configuration hash。
5. `ExecutionEvent` bump schema version，停止寫 `Provider`；所有 projection consumer
   使用既有 `Backend`/`ExecutionTarget`。
6. 新 `backend_session_bound` 的 target、backend 與 task target 必須 strict canonical，
   不得是 `local`。
7. 新 durable backend 只能是 `ollama` 或 admission 時已在
   `ExecutionRegistry` 註冊的 named backend。

### 5.2 Routing invariants

1. scheduler、retry、direct-agent、extra-model、sidecar、fast path、unattended、resume
   都只能 dispatch frozen `ExecutionTarget`。
2. legacy authored config 只可在 admission boundary 影響第一次 target resolution；
   一旦 target 已 frozen，不得再成為 retry/resume input。
3. current config、CLI override、live provider list/availability 不可作 migration evidence。
4. ambiguous 或 conflicting legacy identity fail closed before model/process/tool call。

### 5.3 PR-1 不做的事

- 不移除任何 legacy reader。
- 不改舊 EventStore bytes/hash chain。
- 不讓 inspector 或 migration command 寫檔。
- 不改 `SubagentProvider` protocol adapter。

## 6. Read-only workspace inspector（PR-2）

### 6.1 CLI

```bash
hufu migrate inspect-execution --workspace <path> [--branch <id>] [--json]
```

- 新增 root `migrate` command；`cmd/hufu/root.go` 必須顯式註冊。
- `--workspace/-w` 使用 root persistent flag `opts.workspace`，child 不重複宣告。
- 未提供 `--workspace` 時使用既有 `getWorkspace()`。
- 預設掃所有 branches；`--branch` 只掃該 branch 的可見 lineage。
- `--json` 是 inspector 自有 stable JSON output；text 與 JSON 均只寫 stdout。
- diagnostics 寫 stderr；compatibility hits 本身不是 command error。

Exit contract：

| Exit | Meaning |
| --- | --- |
| 0 | 完整掃描成功，包括有 ambiguous/unmigratable findings |
| 1 | workspace/I/O、hash chain、event envelope、session tree 或參數錯誤；不得輸出看似完整的 partial report |

### 6.2 Authoritative inputs

按順序：

1. 以既有 EventStore scanner read-only open 並驗證完整 hash chain；
2. 讀 `session_tree.json`，對每個 branch 建立 visible lineage；沒有 tree 時使用
   implicit `main`；
3. `session.json` 只補 event lineage 中完全不存在的 legacy task，並只歸入 active
   branch；
4. 同一 task 同時存在 session/event 且 immutable contract 不一致時，回 hard error，
   不選較新的檔案；
5. `task_journal.jsonl`、status、report、TUI output 都不是 migration evidence；
6. `execution-events.jsonl` 只可額外 read-only 掃描 `ExecutionEvent.Provider` 的 raw
   inventory counter，絕不可改變 task/policy classification、source digest 或 migration
   payload。檔案存在但 JSON malformed 時 command 回 exit 1；檔案不存在則 counter 為 0。

Raw event counters按 physical event 計數一次，不因 parent event 被多 branch 繼承而重複。
Task counters按 `(branch_id, task_id)` 計數，因此同一 inherited task 在不同 branch 可有
不同 migration 狀態。

### 6.3 Stable report schema

```go
type InspectionReport struct {
    SchemaVersion                 int       `json:"schema_version"` // 1
    Scope                         string    `json:"scope"`          // all or branch:<id>
    LegacyLocalAliasEvents        int       `json:"legacy_local_alias_events"`
    LegacyProviderShadowEvents   int       `json:"legacy_provider_shadow_events"`
    LegacyExecutionEventProviders int      `json:"legacy_execution_event_providers"`
    LegacyProviderBindings       int       `json:"legacy_provider_bindings"`
    LegacyProviderSessionEvents  int       `json:"legacy_provider_session_events"`
    LegacyReceiptProviders       int       `json:"legacy_receipt_providers"`
    LegacyPolicyRoutes           int       `json:"legacy_policy_routes"`
    CanonicalTasks               int       `json:"canonical_tasks"`
    MigratedTasks                int       `json:"migrated_tasks"`
    MigratableTasks              int       `json:"migratable_tasks"`
    AmbiguousTasks               int       `json:"ambiguous_tasks"`
    UnmigratableTasks            int       `json:"unmigratable_tasks"`
    CanonicalPolicySnapshots     int       `json:"canonical_policy_snapshots"`
    MigratedPolicySnapshots      int       `json:"migrated_policy_snapshots"`
    MigratablePolicySnapshots    int       `json:"migratable_policy_snapshots"`
    AmbiguousPolicySnapshots     int       `json:"ambiguous_policy_snapshots"`
    UnmigratablePolicySnapshots  int       `json:"unmigratable_policy_snapshots"`
    Findings                     []Finding `json:"findings"`
}

type Finding struct {
    SubjectKind     string         `json:"subject_kind"` // task|policy_snapshot
    BranchID        string         `json:"branch_id"`
    TaskID          string         `json:"task_id,omitempty"`
    RunID           string         `json:"run_id,omitempty"`
    SourceEventID   string         `json:"source_event_id,omitempty"`
    Classification  Classification `json:"classification"`
    Features        []Feature      `json:"features"`
    ReasonCode      string         `json:"reason_code,omitempty"`
    EvidenceEventIDs []string      `json:"evidence_event_ids,omitempty"`
}
```

禁止輸出 model、provider URL/key、workspace absolute path、task goal/output 或 raw payload。
`Features` 與 `EvidenceEventIDs` 排序，`Findings` 依
`subject_kind/branch_id/task_id/run_id/source_event_id` deterministic 排序。

Raw surface counters 與 subject counters 的意義不同：

| Counter | Exact unit |
| --- | --- |
| `legacy_local_alias_events` | EventStore 中至少含一個 durable `local` backend spelling 的 physical event；同 event 只算一次 |
| `legacy_provider_shadow_events` | EventStore 中至少含一個 legacy task identity shadow（`model`、`model_topology`、`subagent_provider`、`provider_binding`）的 physical event；同 event 只算一次 |
| `legacy_execution_event_providers` | `execution-events.jsonl` 中含非空 `provider` 的 physical JSONL record |
| `legacy_provider_bindings` | EventStore 或 session-only fallback 中每個非空 legacy `provider_binding` field occurrence |
| `legacy_provider_session_events` | EventStore 中每個 `provider_session_bound` physical event |
| `legacy_receipt_providers` | EventStore 或 session-only fallback 中每個非空 receipt `subagent_provider` occurrence |
| `legacy_policy_routes` | EventStore 或 session-only fallback 中每個非空 route `legacy_provider` occurrence |

Raw counters 是 append-only 歷史盤點，migration 後可以保持大於 0。Task 的 subject invariant
是 `applicable = canonical + migrated + migratable + ambiguous + unmigratable`；
`not_applicable` findings 不計入其中。Policy subject 使用相同 invariant，沒有
`not_applicable`。Report tests 必須逐 branch 驗證 invariant，不能用 raw counter 推論是否
已完成 migration。

Text output 必須逐行輸出上列 counters，最後每個非-canonical finding 只輸出
`subject_kind branch_id task_id-or-source-event-id classification reason_code`，不輸出
敏感內容。

### 6.4 Read-only proof

Inspector：

- EventStore只呼叫既有 read-only `StreamValidatedRunEvents`；不呼叫
  `OpenEventStore`/`NewEventStore`、`LoadTeam`、`NewCoordinator`、`SaveSession`；
- 不建立不存在的 workspace/logs/session tree；
- 不取得 writer lock；
- 測試前後比較 workspace recursive file list、bytes、mode、size、mtime；
- missing workspace 是 exit 1，不自動建立。

## 7. Deterministic classification algorithm

每個 branch lineage 先重建 raw task occurrence，再依下列 precedence 解析：

### 7.1 Existing typed target

- backend `local` → 同 model 的 `ollama` target；
- canonical `ollama` 或 named backend → 原 target；
- 空 backend/model、非法 backend/model → unmigratable；
- topology 每 leaf 獨立 canonicalize，且必須包含 primary；
- binding backend 必須與 canonical target 相同；`local` 與 `ollama` 只在 migration
  deriver 內視為同一 historical identity。

### 7.2 Target-less legacy occurrence

Evidence precedence：

1. durable `BackendBinding.Backend`；
2. durable `ProviderBinding.Provider`；
3. durable `SubagentProvider`（`hufu-local` → `ollama`，其餘為 named backend）；
4. 同 task run lineage 中 model 相符且唯一的 `model_profile_resolved.provider`；
5. historical model spelling：
   - `local/<model>` 或 `ollama/<model>` → `ollama/<model>`；
   - bare model → historical built-in `ollama`；
   - 其他 qualified spelling 若無 1～4 evidence → ambiguous。

兩個非空 durable sources canonicalize 後不同，分類為 ambiguous，永不依 precedence
靜默覆蓋 conflict。非法 JSON field、空 executable model、unsupported compatibility
schema 分類為 unmigratable。完全沒有 execution identity、binding 或 receipt 的 policy-only
task 分類為 not-applicable，不計入 task counters。

`ExecutionReceipt.subagent_provider` canonicalization：

- `hufu-local` → receipt 所屬 admitted target 的 backend；
- 其他非空值 → `CanonicalTargetBackendName(value)`；
- 與 admitted target backend 不一致 → ambiguous；
- 新 canonical field `backend` 與 legacy field 同時存在但不同 → ambiguous。

`ExecutionPolicySnapshot` v3 canonicalization：

- 每個 route 以既有 `Model`、canonicalized `Backend`、`ProviderKey` 建立 v4 route；
- `LegacyProvider` 非空時必須 canonicalize 後等於 route backend，否則 ambiguous；
- drop `LegacyProvider` 後，以 `(Model, Backend, ProviderKey)` 排序及 deduplicate；
- 保留其他 backend/world/network/environment policy fields，將 version設為 4，使用 v4
  encoding重算 `ConfigurationHash`；
- 不得重新 resolve model、讀 current config、查 backend registry或 provider metadata；
- invalid route、不同 legacy fields折疊成同 key但內容不一致，分類為 unmigratable。

### 7.3 Classification outcome

- canonical：沒有 legacy feature，所有 canonical fields valid；
- migrated：task 存在有效、self-contained 的
  `execution_compatibility_migrated` event，或 policy subject 存在有效的
  `execution_policy_snapshot_migrated` event，且其 canonical projection valid；
- migratable：尚無 migration event，algorithm 唯一決定完整 canonical projection；
- ambiguous：存在至少兩個可能 identity 或 evidence conflict；
- unmigratable：資料結構有效但缺少產生完整 canonical projection 的必要資料；
- malformed hash chain/event envelope/session tree：不是 finding，整個 inspection error。

Policy snapshot使用相同 classification vocabulary與獨立 counters；它不併入 task counts。
任何 classification 都不讀 current config 或要求 backend 現在可用。Backend registration
只在真正 resume/dispatch 的既有 preflight 檢查。

## 8. Append-only materialized migration（PR-3）

### 8.1 CLI

```bash
hufu migrate apply-execution --workspace <path> [--branch <id>] --apply [--json]
```

- 無 `--apply` 時 hard error，避免誤以為已修改。
- 預設處理所有 branches；依 branch ID 排序，branch 內先依 task ID、再依 policy
  `(run ID, source event ID, source digest)` 排序。
- 先完整 inspect；只要 task或policy subject存在 ambiguous/unmigratable，就不 append
  任何 event。
- command 必須取得 EventStore 的既有 exclusive writer lock；已有 runtime writer 時 fail。
- 不改寫、truncate 或 replace 舊 EventStore bytes。
- 每個 task 或 policy subject 各一個 atomic append；crash 後以 idempotency key 重跑。
- append 全部成功後才用既有 atomic session projection API rebuild active
  `session.json`；projection rebuild 失敗不回滾 canonical events，回 exit 1 並提示可重跑。

### 8.2 New canonical migration event

```go
const EventExecutionCompatibilityMigrated EventType =
    "execution_compatibility_migrated"

type ExecutionCompatibilityMigratedPayload struct {
    SchemaVersion       int                         `json:"schema_version"` // 1
    BranchID            string                      `json:"branch_id"`
    TaskID              string                      `json:"task_id"`
    SourceKind          string                      `json:"source_kind"` // event_lineage|session_snapshot
    SourceDigest        string                      `json:"source_digest"`
    EvidenceEventIDs    []string                    `json:"evidence_event_ids,omitempty"`
    ExecutionTarget     execution.ExecutionTarget   `json:"execution_target"`
    ExecutionTopology   []execution.ExecutionTarget `json:"execution_topology"`
    BackendBinding      *BackendBinding             `json:"backend_binding,omitempty"`
    ReceiptBackends     []ReceiptBackendMigration   `json:"receipt_backends,omitempty"`
    CanonicalTask       json.RawMessage             `json:"canonical_task,omitempty"`
}

type ReceiptBackendMigration struct {
    RunID            string `json:"run_id"`
    Attempt          int    `json:"attempt"`
    ModelExecutionID string `json:"model_execution_id,omitempty"`
    Backend          string `json:"backend"`
}
```

Rules：

- `SourceDigest` 是 canonical JSON of all consumed identity evidence 的 SHA-256；不包含
  current config。
- `EvidenceEventIDs` 只列 active lineage 中真正使用的 event IDs，sorted/unique。
- idempotency key：
  `execution-compatibility-migrated:<branch>:<task>:v1:<source_digest>`。
- Event-backed task 的 `CanonicalTask` 必須為空；event 只 patch execution identity。
- Session-only task 的 `CanonicalTask` 必須是完整 canonical task transition payload，
  不含任何 legacy execution field。Reducer 可由此單一 event 建立 task，避免兩次 append
  的 crash window。
- 若 event-backed canonical task immutable contract 與 migration payload task 不相符，
  fail closed。
- migration event 是 self-contained canonical truth。PR-5 後 runtime reducer只驗證其
  canonical schema/identity；legacy source re-derivation 留在 migration CLI audit path。

### 8.3 Policy snapshot migration event

Policy snapshot是 branch/session subject，不可塞入 task migration payload：

```go
const EventExecutionPolicySnapshotMigrated EventType =
    "execution_policy_snapshot_migrated"

type ExecutionPolicySnapshotMigratedPayload struct {
    SchemaVersion  int                     `json:"schema_version"` // 1
    BranchID       string                  `json:"branch_id"`
    RunID          string                  `json:"run_id,omitempty"`
    SourceKind     string                  `json:"source_kind"` // event_lineage|session_snapshot
    SourceEventID  string                  `json:"source_event_id,omitempty"`
    SourceDigest   string                  `json:"source_digest"`
    Snapshot       ExecutionPolicySnapshot `json:"snapshot"` // version 4
}
```

上述 event payload 定義在 `internal/team`；`internal/executioncompat` 只回傳不依賴 team
package 的 canonical snapshot wire/result，避免 import cycle。

- 每個 `(branch_id, visible v3 snapshot)` subject產生一個 branch-scoped v4 migration
  event；session-only snapshot的
  `SourceEventID` 可空，但 `SourceDigest` 必須是 checkpoint中該 snapshot canonical JSON
  的 SHA-256；
- idempotency key：
  `execution-policy-snapshot-migrated:<branch>:<source-event-or-session>:v1:<source_digest>`；
- migration event必須排在其所取代的 v3 event之後；latest valid v4 snapshot或migration
  event是該 branch的 canonical policy projection；
- repeated identical event idempotent；同 source不同 digest/snapshot conflict；
- PR-5 後 runtime不解析 v3 `LegacyProvider`，但可直接驗證並使用 payload中的 v4
  snapshot。

### 8.4 Replay ordering

PR-3/PR-4 compatibility runtime 使用 two-pass replay：

1. migration bridge以 preceding branch lineage 驗證 migration event 的 source digest 與
   derived canonical projection；
2. canonical reducer忽略被 migration event覆蓋的 legacy execution fields，task 套用
   target、topology、binding、receipt backends；policy reducer套用完整 v4 snapshot；
3. migration 後任何 event 改變 frozen target/topology/backend 都是
   `ExecutionIdentityConflictError`；
4. 只有 fork point以前的 parent migration event才對 child可見。Apply執行時新 append到
   parent、且位於既有 child fork point之後的 event不會回溯進 child lineage；因此每個
   尚未看見 migration event的 `(branch_id, task_id)` 都要寫自己的 branch-scoped event，
   即使 legacy source是 inherited parent event；
5. repeated identical event idempotent；不同 digest或projection conflict。

既有 `execution_target_migrated` v1 在 PR-3/PR-4 保持可 replay。它本身仍屬
`FeatureLegacyResumeMigration`；apply必須為這些 task另寫新的 self-contained
`execution_compatibility_migrated` event。PR-3 不改 v1 schema；task migration event涵蓋
typed `local`、receipt及 session-only task，policy-route則由 §8.3 的獨立 event涵蓋。

## 9. Warning 與 local telemetry（PR-4）

### 9.1 Runtime warning

Compatibility preflight可執行 read-only detection，但不得 append。Detection result 放入
in-memory observer；EventStore/coordinator 完成既有初始化後才 flush。

每個 CLI invocation 最多各一個 workspace-state warning與 authored-alias warning：

```text
warning: workspace contains deprecated execution identity state; run
`hufu migrate inspect-execution --workspace <path>` and then
`hufu migrate apply-execution --workspace <path> --apply`
```

若 runtime/config loader在 canonicalize前看見 raw `local` backend spelling：

```text
warning: execution backend alias `local` is deprecated; use `ollama`
```

- warning 不含 task/model/provider 或 resolved absolute path detail；若使用者明確提供
  workspace argument，command只原樣引用該 argument；若未提供則輸出不帶
  `--workspace` 的 command，不展開 default workspace path；
- `--quiet` 與 `--output json` 仍可寫 stderr，但 stdout contract 不變；
- fresh/canonical workspace不發 workspace-state warning；dry-run與doctor若載入 raw
  authored `local` alias仍發 alias warning；list與不解析 execution config的team commands
  不警告；
- inspector/apply command 不透過 runtime observer再產生 warning/event。

### 9.2 Metadata-only event

```go
const EventExecutionCompatibilityObserved EventType =
    "execution_compatibility_observed"

type ExecutionCompatibilityObservedPayload struct {
    SchemaVersion int             `json:"schema_version"` // 1
    Counts        map[Feature]int `json:"counts"`
}
```

- 每 `(run_id, branch_id)` 最多一個，idempotency key含 run/branch/schema；
- 不含 task IDs、event IDs、models、provider names、paths、prompts或outputs；
- append failure是 telemetry gap：記既有 dual-write diagnostic，不阻止 task、不改
  migration classification；
- Hufu 不新增外送 telemetry。Release evidence由 operator 執行 inspector取得。

### 9.3 Deprecation record

PR-1 新增 `docs/deprecations/execution-compatibility.md` 並加入 `docs/README.md`。PR-4
發布時必須把以下 placeholder 換成實際 tag，placeholder 不得進 release branch：

```text
Warning-Introduced-In: <release-tag>
Earliest-Removal: first semver-appropriate release after one later published
                  release has carried the warning
```

若移除 public `local` input alias或讓未遷移 workspace不再直接啟動被視為 breaking，PR-5
必須進 semver-appropriate breaking release；release notes要列 migration commands 與
rollback。

### 9.4 Release-owner handoff checklist

Coding PR 合併後，release owner 依下列順序提供 PR-5 可重跑 evidence：

1. 在 warning release 的同一個 release branch 將 `Warning-Introduced-In` 替換為實際
   tag，並發布該 tag；
2. 發布至少一個後續版本，確認它仍帶有相同 warning；
3. 在 release notes 列出 `inspect-execution`、`apply-execution --apply` 與 append-only
   rollback semantics；
4. 以 maintainer 指定的 production workspace 執行 inspector，只保存 §10 所列的
   aggregate counters 作 evidence；
5. 將 tag、後續 release、release-notes URL/identifier 與 sanitized counter evidence
   附到 PR-5 description。

這些步驟需要 release／production authority；它們不是 coding agent 可藉由本地文件或測試
自行滿足的條件。

## 10. Removal gates（PR-5）

PR-5 開始前，PR description 必須附可重跑 evidence，全部成立：

1. PR-1 writer tests證明所有 execution paths 100% canonical；
2. repository fixtures不再由 current writer產生 legacy fields；
3. saved legacy fixtures全部可 inspect、materialize、用 canonical-only replay重建；
4. scanner可區分 ambiguous/unmigratable，且 apply對任一存在時 zero append；
5. 正式 deprecation record已有實際 warning release tag；
6. 至少一個後續 published release完整攜帶 warning；
7. release notes已公告 removal與 migration command；
8. 由 maintainer指定的 production workspace inventory：
   `migratable_tasks=0`、`ambiguous_tasks=0`、`unmigratable_tasks=0`，且三個對應
   policy snapshot counters也都是 0；
9. runtime production code不再讀 legacy durable execution identity；只有
   `internal/executioncompat`、team migration bridge與 migrate CLI保留 legacy wire reader；
10. canonical-only full tests、race、vet、build、lint全部通過。

第 8 項只要求 metadata counters，不把 production workspace或 report提交 repository。
因為 migration 是 append-only，`legacy_*` raw surface counters 可保持大於 0，不能作
removal blocker；真正的 blocker 是尚未 materialize 或無法唯一 materialize 的 subject
counters。

### 10.1 Static enforcement

新增 AST-based test掃描 production `.go`（排除 `_test.go`），禁止在 migration allowlist
外出現：

- durable JSON tags `subagent_provider`、`provider_binding`、`legacy_provider`；
- task/session/event replay對 legacy fields的 selector access；
- `EventProviderSessionBound` runtime dispatch/reducer case；
- `LegacyLocalBackendName`、`IsOllamaBackend`、alias-aware equality在 scheduler/replay。

Allowlist只可包含：

```text
internal/executioncompat/**
internal/team/execution_compatibility_bridge.go
internal/team/execution_compatibility_inspect.go
internal/team/execution_compatibility_migration.go
cmd/hufu/migrate_execution_cmd.go
```

Transient adapter/config fields在 AST test以具體 type+field allowlist列出，不可用整個 package
或模糊字串排除。Test failure須列 file:line 與 forbidden surface。

### 10.2 Runtime after removal

- canonical replay不 import legacy derivation helper；
- unmigrated workspace在 preflight 回 actionable error，指出 inspector/apply command，且在
  任何 workspace mutation、LLM、provider process或tool call前失敗；
- valid migration event可由 canonical reducer重建 task，不需重讀 legacy payload；
- unknown historical events仍依 EventStore forward-compat規則保存/忽略；
- migration CLI仍可離線處理未遷移 workspace；不得要求安裝舊 Hufu binary。

## 11. PR 切分與檔案 ownership

### PR-1 — Inventory + complete write-ban

主要新增：

```text
internal/executioncompat/types.go
internal/executioncompat/detect.go
internal/executioncompat/detect_test.go
internal/team/execution_compatibility_bridge.go
internal/team/execution_compatibility_test.go
docs/deprecations/execution-compatibility.md
```

主要修改：

```text
internal/team/execution_receipt.go
internal/team/coordinator_task_run.go
internal/team/coordinator_run.go
internal/team/coordinator_terminal_receipt.go
internal/team/execution_policy_snapshot.go
internal/team/execution_events.go
internal/team/execution_event_exporter.go
internal/team/status.go
internal/team/coordinator_eventstore.go
internal/team/task_occurrence_projection.go
internal/team/projection_shadow.go
cmd/hufu/report.go
cmd/hufu/display.go
docs/README.md
docs/architecture/execution-runtime.md
docs/roadmap.md
```

PR-1 只完成 writer/canonical consumer；legacy reader仍在。

### PR-2 — Read-only inspector + saved fixtures

```text
internal/executioncompat/derive.go
internal/executioncompat/classify.go
internal/team/testdata/execution-compat/**
internal/team/execution_compatibility_inspect.go
cmd/hufu/migrate_execution_cmd.go
cmd/hufu/migrate_execution_cmd_test.go
cmd/hufu/root.go
```

### PR-3 — Append-only materializer + replay projection

```text
internal/executioncompat/migration.go
internal/team/execution_compatibility_migration.go
internal/team/execution_target_replay.go
internal/team/execution_policy_snapshot.go
internal/team/event_types.go
internal/team/event_reducers.go
internal/team/session.go
internal/team/session_tree.go
cmd/hufu/migrate_execution_cmd.go
```

### PR-4 — Warning/observer + deprecation-release handoff

```text
internal/team/execution_compatibility_observer.go
internal/team/execution_events.go
cmd/hufu/execution_compatibility_warning.go
cmd/hufu/run.go
cmd/hufu/team_runner.go
cmd/hufu/doctor.go
docs/deprecations/execution-compatibility.md
```

本 PR 的 coding scope 到 warning／observer 與正式 record 的 pending release marker
為止。實際 tag、published release、release notes 與 production inventory 是 release
owner 的外部 evidence，不能以本地測試或文件編輯替代。

若 warning需要新的 status event，必須同步 trace CLI plain/JSON、report與TUI reporter；若只走
stderr deprecation warning，不新增 `tea.Msg` 或 TUI overlay。

### PR-5 — Runtime reader removal + durable struct cleanup

實際檔案由 PR-1 inventory產生，不以原始短清單為限。至少包含：

```text
internal/execution/target.go
internal/team/execution_target_{state,migration,replay}.go
internal/team/{status,projection_shadow,event_reducers}.go
internal/team/{subagent_binding,execution_backend,execution_registry}.go
internal/team/{coordinator_session,coordinator_task_run}.go
cmd/hufu/{backend_config,execution_target_preflight,report}.go
```

`internal/agent` authoring config與 transient provider adapter依 §3.3 保留。

## 12. Fixture contract

Fixtures放在 `internal/team/testdata/execution-compat/`；integration test 由 `team`
package 執行，因為它需驗證 EventStore、branch lineage、materialization 與 replay。每個
fixture目錄包含：

```text
README.md                 producer version/commit、scenario、expected class
workspace/session.json    若 scenario需要
workspace/session_tree.json       若 scenario需要 branch lineage
workspace/logs/event_store.jsonl  若 scenario需要 durable events
expected-report.json      successful inspection case
expected-error.json       intentional hard-error case (mutually exclusive with report)
```

`README.md` 與**恰好一個** expected result 一律必備：正常掃描使用
`expected-report.json`，hash-chain／envelope 等預期 hard error 使用
`expected-error.json`（stable error code 或無敏感資訊的 diagnostic fragment）。workspace
inputs 只放入該 case 的 authoritative input。test 必須先複製 fixture 到 temp workspace，
永不在 `testdata` 上執行 apply。case 1--16 的完整矩陣是 PR-2/PR-3 acceptance
requirement，不因某個 case只需要 session snapshot 而省略。

必備 cases：

1. canonical current workspace，zero compatibility hits；
2. target-less bare model + `hufu-local`；
3. target-less named `SubagentProvider`；
4. durable `ProviderBinding` 可唯一恢復；
5. `provider_session_bound` → `BackendBinding`；
6. typed target/topology/binding 使用 `local`；
7. qualified legacy model無 evidence，ambiguous；
8. binding/profile evidence conflict，ambiguous；
9. invalid/empty executable identity，unmigratable；
10. legacy receipt provider + canonical receipt conflict；
11. policy snapshot v3 `legacy_provider` → v4 canonical routes；
12. parent/child branch在 fork前後各有 legacy state；
13. session-only task；
14. corrupt hash chain（command hard error）；
15. partial prior materialization後 idempotent rerun；
16. unknown unrelated event preserved/ignored。

「saved legacy fixture」必須來自已知舊 producer或由固定 legacy fixture builder產生；不得
手改 hashed JSONL。若需要 redact，只能改 sensitive payload後用 repository fixture builder
重建完整 hash chain，README要記錄原 producer commit與重建方式。Fixture不可依賴 live
provider、外部 binary、consumer checkout或 current config。

## 13. Required tests

### PR-1 writer tests

```text
TestNewTaskNeverPersistsLocalAlias
TestCanonicalTaskDurabilityOmitsRetiredIdentityFields        (existing; extend)
TestNewReceiptPersistsBackendNotSubagentProvider
TestEveryAttemptPathWritesCanonicalReceiptBackend
TestExecutionPolicySnapshotV4OmitsLegacyProvider
TestExecutionEventOmitsProviderWhenBackendPresent
TestNewTaskNeverUsesLegacyProviderAsRoutingInputAfterAdmission
```

`TestEveryAttemptPathWritesCanonicalReceiptBackend` table必須涵蓋 normal、direct、
extra-model、sidecar/auxiliary、retry、terminal/runtime receipt；不允許只測 struct marshal。

### PR-2 inspector tests

```text
TestCompatibilityScannerCountsPhysicalEventsOnce
TestCompatibilityScannerClassifiesBranchTaskOccurrences
TestCompatibilityScannerDoesNotReadLiveConfigAsEvidence
TestCompatibilityScannerDoesNotUseShadowProjectionsAsEvidence
TestCompatibilityScannerCountsExecutionEventProviderInventoryOnly
TestCompatibilityScannerIsByteAndMtimeReadOnly
TestCompatibilityScannerRejectsCorruptHashChain
TestCompatibilityScannerJSONIsDeterministic
TestCanonicalWorkspaceHasZeroCompatibilityHits
```

### PR-3 migration/replay tests

```text
TestLegacyLocalTypedReplayRequiresCompatibilityMigration
TestLegacyLocalMigrationCanonicalizesTargetTopologyAndBinding
TestLegacyProviderBindingMigrationMaterializesBackendBinding
TestLegacyReceiptMigrationMaterializesBackend
TestSessionOnlyMigrationWritesOneSelfContainedCanonicalEvent
TestLegacyPolicySnapshotMigrationProducesV4WithoutLiveConfig
TestMigrationNeverUsesLiveConfigOrProviderAvailability
TestAmbiguousLegacyIdentityAppendsNothing
TestAmbiguousLegacyPolicySnapshotAppendsNothing
TestMigrationIsBranchScopedAndIdempotent
TestParentMigrationAppendedAfterForkDoesNotSatisfyChild
TestMigrationAppendFailureDoesNotAdvanceProjection
TestCanonicalReplayRejectsPostMigrationRetarget
TestSavedLegacyFixturesReplayCanonicalAfterMaterialization
```

### PR-4 warning/telemetry tests

```text
TestCompatibilityWarningOncePerInvocation
TestCompatibilityWarningDoesNotPolluteJSONStdout
TestCompatibilityObserverPayloadContainsOnlyCounts
TestCompatibilityObserverFailureDoesNotFailTask
TestInspectorDoesNotEmitCompatibilityObservation
```

### PR-5 removal tests

```text
TestNoProductionReadOfLegacyDurableExecutionFields
TestUnmigratedWorkspaceFailsBeforeLifecycleMutation
TestCanonicalOnlyReplayAllSavedFixtures
TestMigrationCLIStillReadsPreSunsetWorkspace
TestPublicLocalAliasReturnsUseOllamaDiagnostic
```

## 14. Validation gates

每個有 Go/code change 的 PR 必須分別執行並成功：

```bash
go test ./...
go test -race ./...
go vet ./...
go build ./cmd/hufu
golangci-lint run
```

另外執行：

```bash
go test ./internal/executioncompat ./internal/team ./cmd/hufu -run 'ExecutionCompat|LegacyLocal|CanonicalReceipt|ExecutionPolicySnapshot'
```

PR-2/PR-3 必須對每個 fixture執行 inspector，且全程確認原 fixture bytes未被測試覆寫。
PR-3 在 temp copy 依預期分類驗證：

- `migratable` fixture：apply 後重新 inspect 為 `migrated`，並作 canonical-only replay；
- `canonical`／`migrated`／unknown-event fixture：apply 不追加 migration event，且 replay
  保持成功；
- `ambiguous`／`unmigratable` fixture：apply 回 actionable error 且 EventStore bytes 完全
  不變；
- hard-error fixture：inspector 失敗且 workspace bytes、mode、size、mtime 完全不變；不呼叫
  apply。

## 15. Failure、rollback 與 recovery semantics

### Before PR-5

- inspector failure不改 workspace；修正 input/fixture後重跑。
- apply在 append前發現 ambiguity/unmigratable：zero append。
- append途中 crash：已 append event保留；以 idempotency key重跑，不 truncate。
- append完成但 `session.json` rebuild失敗：event truth保留，回 error並重跑 projection；
  不重複 migration、不回滾 hash chain。
- runtime warning/observer失敗不改 dispatch outcome。

### PR-5 rollback

若 removal暴露 production workspace問題：

1. revert PR-5；
2. 保留 PR-1 canonical writers、inspector、materializer與warning；
3. 不重新允許 legacy writes；
4. 保存失敗 workspace的 metadata-only classification與新增 sanitized fixture；
5. 修正 migration後重新滿足全部 removal gates。

不得用 `git reset --hard`、刪 workspace或重寫 EventStore作 compatibility recovery。

## 16. Definition of done

PR-1～PR-3 code done：

```text
all new durable execution identity is canonical
inspector is deterministic and provably read-only
every supported legacy fixture has a deterministic append-only migration
ambiguity appends nothing and fails closed
```

PR-4 code handoff done：

```text
runtime warning and metadata-only observer are implemented and tested
formal deprecation record exists with a pending release marker
release owner handoff is specified in §9.4 and §10
```

PR-1～PR-4 release evidence is complete only after the warning has actually shipped with a
formal deprecation record. A pending marker, local build, or untagged commit is not release
evidence and does not satisfy any PR-5 gate.

PR-5 done：

```text
runtime scheduler/replay/session no longer reads legacy durable execution identity
unmigrated workspaces fail before mutation with an actionable migration command
migration CLI remains capable of upgrading old workspaces
all branches, receipts, policy snapshots and projections rebuild canonically
release and production gates are evidenced
```

不是只刪 struct field，也不是讓 tests compile；完成條件是 canonical writers、deterministic
migration、observable deprecation、canonical-only replay與可回復的 release boundary同時成立。

## 17. Coding-agent 指派順序

一次只實作一個 PR boundary：

1. PR-1：先建立 inventory tests，封閉 receipt/policy/execution-event writers。
2. PR-2：只做 pure scanner、fixtures與 read-only CLI。
3. PR-3：先寫 ambiguity/append-failure tests，再加 migration event與apply command。
4. PR-4：接 runtime warning/observer；交由 release owner發布 deprecation release，並把
   實際 tag、後續 release、release notes 與 production inventory 納入 PR-5 evidence。
5. PR-5：只有 §10 evidence齊全才移除 runtime readers/fields。

禁止 coding agent在 PR-1～PR-4「順手」刪 reader；禁止在 PR-3 為通過 fixture而讀
current team/provider config；禁止在 PR-5 移除 migration CLI。
