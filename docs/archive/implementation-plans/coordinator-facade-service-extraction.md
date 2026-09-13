# Coordinator Façade / Service Extraction 實作紀錄

> Status: completed
> Priority: P2
> Baseline: `132a0c89d2b74c762a19c6082a110fa375d0c882`
> Baseline validated: 2026-09-13
> Completed: 2026-09-13
> Nature: architecture debt reduction，非新使用者功能

## 0. 完成摘要

本計畫已依序完成四個可獨立驗證、可反向 revert 的階段：

| 階段 | Commit | 結果 |
| --- | --- | --- |
| PR-A TaskCache ownership extraction | `d95c969` | cache state、locking、generation、restore/fork/invalidate 收斂至 `defaultTaskCache` |
| PR-B EvidenceService extraction | `f8485d7` | manifest build/finalize/persist 收斂至 `defaultEvidenceService` |
| PR-C Recovery ownership consolidation | `881a5b3` | `RepairController` 納入 composition bundle，一般重試與 targeted recovery 共用安全決策入口；crash-resume 保留既有 session recovery 語意 |
| PR-D Internal composition root | `27b9d6c` | exported constructor 相容，internal params/bundle composition 與 error cleanup 完成 |

實作期間未修改 event schema、CLI output、retry/disposition、acceptance、terminal、
memory 或 execution-target 語意。以下各節保留原始設計與驗收規格，供後續維護與
回歸調查使用。

## 0.1 開工時的 baseline 決策（歷史）

本計畫按上述 baseline 的實際程式碼重新盤點，並固定由 PR-A 開始逐階段實作。

如果開工時 `HEAD` 已不是上述 baseline，先執行：

```bash
git diff --stat 132a0c89d2b74c762a19c6082a110fa375d0c882..HEAD -- internal/team cmd/hufu internal/evalharness evals
```

若差異碰到本文件列出的 PR-A 檔案、cache policy、task journal、session restore、
extra-model clone 或 eval harness，必須先更新本文件的 baseline 與 impact map；
不能在未知新行為上直接套用本計畫。

實作完成後，本文件由被 gitignore 的 `docs/tmp/now` 移至受版控的
`docs/archive/implementation-plans`。

## 1. 目標

讓 `Coordinator` 漸進收斂為 orchestration façade：

- `Coordinator` 保留 run-level composition、狀態機與跨 service orchestration。
- subsystem 自己持有其 state、locking、演算法與 lifecycle ownership。
- production call site 一律經過 canonical service seam。
- tests 可以只替換被測 service，不需建置平行 runtime。
- 不做 big-bang rewrite，不在本計畫內改使用者可觀察語意。

成功不是單純減少 `coordinator.go` 行數，而是消除跨檔案直接操作 subsystem
state 的情況，並以既有 deterministic evals 證明行為相容。

## 2. Baseline 現況與 canonical owners

| Area | Baseline owner / seam | 本計畫處置 |
| --- | --- | --- |
| Planning | `Planner` / `defaultPlanner`，`internal/team/services.go` | 已存在，不重抽 |
| Session | `SessionStore` / `defaultSessionStore` | 已存在，不重抽 |
| Policy | `PolicyEngine` / `defaultPolicyEngine` | 已存在；TaskCache 只透過它讀 policy/freshness |
| Context | `ContextCompiler` / `defaultContextCompiler` | 已存在，不重抽 |
| Agents | `AgentPool` / `defaultAgentPool` | 已存在；TaskCache similarity adapter 透過它呼叫 sidecar |
| Workflow | `WorkflowEngine` / `defaultWorkflowEngine`；`runtimeWorkflow` 管 phase | 已存在，不新增第二個 engine |
| Events | `EventJournal` / `eventStoreJournal`；`EventStore` 管 canonical hash chain | 已存在，不新增第二個 façade |
| Terminal | `TerminalManager`、`TerminalCleanupManager`、`TerminalTransferManager` | 已按權限分離，不合併、不重抽 |
| Recovery | `RepairController`，搭配既有 recovery/disposition/commit gate | 強化既有 owner，不新增 `RecoveryManager` |
| Memory | `WorkerMemoryService`、`SharedMemoryService`；`MemoryStore` 僅為 legacy adapter | 保持分離，不新增泛化 `MemoryService` |
| Task cache | `Coordinator` fields + `coordinator_taskcache.go`，且 restore/clone/prune 分散 | PR-A 抽取單一 owner |
| Evidence | `evidence_manifest.go` methods 直接綁 `Coordinator` state | PR-B 抽取 build/finalize service |
| Composition | `RuntimeServices` 與 `setRuntimeServices` 已存在；`NewCoordinator` 仍為長 positional constructor | PR-D 建 internal composition root |

以下工作已在指定 baseline 前完成，不得再建立平行版本：

```text
TerminalManager extraction
Runtime Event façade
WorkflowEngine extraction
RuntimeServices bundle
WorkerMemoryService / SharedMemoryService extraction
```

## 3. 不可破壞的 runtime invariants

所有 PR 必須同時遵守：

1. `Coordinator` 是狀態機；typed task result、receipt、verification、evidence、
   acceptance 與 durable event 才是權威，model prose 不是。
2. 不改 `RunResult`、event schema/payload、event order/cardinality、task status、
   retry count、acceptance、`ExecutionTarget`、`BackendBinding` 或 stop reason。
3. 不新增 local retry loop；task repair/reconcile 必須經既有 recovery machinery，
   且 protocol repair 不得重播已完成的 side effect。
4. event append、session checkpoint、task journal 與 projection 的既有先後順序不變。
5. cache/evidence 錯誤的 fail-open、fail-closed 或 best-effort 行為保持原樣；
   介面不得僅因「較乾淨」而新增會往上傳播的 error。
6. artifact ref 仍是 opaque/content-addressed ID；service 不得把 ID 當路徑。
7. persistence 前維持既有 secret redaction、workspace scope、locking 與 write ordering。
8. normal worker、direct-agent、extra-model、sidecar、fast path、unattended、dry-run、
   wrap-up 與 crash-resume 都必須納入 impact tracing；不能只改 DAG happy path。
9. 不在同 PR 改 schema、CLI output、TUI message、team config 或公開 constructor API。
10. 不搬 package，不新建 `internal/runtime`，不做全域 rename。

## 4. 目標 composition

保留現有 `RuntimeServices`，只加入本計畫確實需要的 seams：

```go
type RuntimeServices struct {
    // Existing fields remain unchanged.
    Planner             Planner
    SessionStore        SessionStore
    PolicyEngine        PolicyEngine
    ContextCompiler     ContextCompiler
    AgentPool           AgentPool
    WorkflowEngine      WorkflowEngine
    EventJournal        EventJournal
    ToolResolver        ToolResolver
    ModelRuntime        ModelRuntime
    SubagentRegistry    *SubagentRegistry
    ExecutionRegistry   *ExecutionRegistry
    ExperienceProcessor ExperienceProcessor

    // Added by this plan.
    TaskCache        TaskCache
    Evidence         EvidenceService
    RepairController *RepairController
}
```

不把 `RuntimeServices` 變成 DI framework。nil field 永遠表示使用 production
default；現有 exported `NewCoordinator(...)` signature 在本計畫內保持不變。
所有 service setter 與 `setRuntimeServices` 都是 construction-time/test seams，
不是 hot-swap API；run 開始後不得替換 stateful service。

`Coordinator` 完成後仍可持有：

```text
run/session identity
task tracker and authoritative task state
conversation/compaction state
event store and durable projections
acceptance/run-finalization state
provider/tool/action composition
separate terminal and memory services
```

## 5. PR-A — TaskCache ownership extraction

### 5.1 Scope

PR-A 是第一個且目前唯一可立即開工的 PR。它只抽 TaskCache，不碰 Evidence、
Recovery、Terminal、Event、Workflow 或 Memory semantics。

目前必須收斂的 state：

```text
Coordinator.taskResultCache
Coordinator.taskResultCacheMu
Coordinator.cacheGeneration
maxTaskCacheEntries
cachedTaskEntry lifecycle
```

`cachePolicy` 仍由 `PolicyEngine` 擁有；task journal 檔案的 open/load/compact
lifecycle 仍由 `task_journal.go` 擁有。TaskCache 接收 restore seeds，並在 mutation
完成且釋放 cache lock 後，透過 callback 送出既有 journal record。TaskCache
不得自行開檔。

### 5.2 Contract

在 `internal/team/services.go` 加入下列完整 contract。名稱可因 Go lint 做小幅
調整，但 method semantics、scope 與 error behavior 不得改變：

```go
type TaskCacheLookupScope string

const (
    TaskCacheLookupExecution      TaskCacheLookupScope = "execution"
    TaskCacheLookupAllGenerations TaskCacheLookupScope = "all_generations"
    TaskCacheLookupCurrentRun     TaskCacheLookupScope = "current_run"
)

type TaskCacheLookupRequest struct {
    Scope      TaskCacheLookupScope
    AgentKey   string
    Task       string
    VerifySpec *VerificationSpec
    Verify     string
    VerifyMode string
}

type TaskCacheLookupResult struct {
    Output      string
    MatchedTask string
}

type TaskCacheStoreRequest struct {
    AgentKey     string
    Task         string
    Output       string
    VerifySpec   *VerificationSpec
    Verify       string
    VerifyMode   string
    Verification *VerificationResult
}

type TaskCacheInvalidateRequest struct {
    AgentKey   string
    Task       string
    VerifySpec *VerificationSpec
    Verify     string
    VerifyMode string
}

type TaskCacheSeed struct {
    AgentKey     string
    Task         string
    Output       string
    VerifySpec   *VerificationSpec
    Verify       string
    VerifyMode   string
    Verification *VerificationResult
    Identity     CacheIdentity
    Pinned       bool
    Deduplicate  bool
}

type TaskCache interface {
    Lookup(context.Context, TaskCacheLookupRequest) (TaskCacheLookupResult, bool)
    Store(TaskCacheStoreRequest)
    Invalidate(TaskCacheInvalidateRequest)
    AdvanceGeneration()
    Restore([]TaskCacheSeed)
    Fork(taskCacheDependencies) TaskCache
}
```

刻意不回傳 `error`：baseline 的 similarity failure 是 cache miss，journal append
failure 是 warning，不能因抽 interface 改成 run failure。

### 5.3 Scope semantics

`Lookup` 必須逐字保留三種既有模式：

| Scope | Exact candidates | Semantic candidates | Pinned entries |
| --- | --- | --- | --- |
| `execution` | current generation first，再檢查所有 generation | current generation only | 可命中 |
| `all_generations` | 所有 generation | 最近 100 筆符合 freshness 的 entries | 可命中 |
| `current_run` | 所有 non-pinned entries | 最近 100 筆 non-pinned entries | 必須排除 |

其他固定語意：

- `CacheBypass`：lookup miss，store no-op。
- `CacheRefresh`：lookup miss，但成功結果仍可 store。
- `CacheUse`：正常 lookup/store。
- 每個 agent 最多保留 `50` 筆，eviction order 與 baseline 相同。
- `ExecutionProfile.DisableTaskCache` 優先解析成 bypass。
- `[bypass-cache]`、`[no-cache]`、`[rerun]`、`[force-refresh]` 與既有
  non-idempotent keyword 清單保持不變。
- observation verification 與 task-result assertion 不可 cache/reuse。
- typed file/json/workset verification 必須通過現有 fingerprint freshness；
  workset 與 command-produced JSON 仍 fail closed。
- exact matching、verification normalization、semantic sidecar timeout（execution
  10 秒，其他 5 秒）及 think/status observation 保持不變。
- generation advance 保留 pinned entries；只有 contract-matching invalidation
  可以移除 pinned entry，並保留 delete tombstone。
- `Fork(deps)` 必須深拷貝 map、slice、`VerificationSpec` 與
  `VerificationResult`；fork 與 parent 之後的 cache mutation 不得互相可見。
- 為逐字保留 `cloneCoordinatorForModel` baseline，forked entries 保留各自原本的
  generation，但 fork 的 active generation 從 `0` 開始。dependencies 必須以
  clone coordinator 重新綁定，不可捕捉 parent coordinator。
- baseline 的 extra-model clone 沒有 task journal handle，因此 production clone
  傳入的 `AppendJournal` 必須為 nil；forked cache mutation 不寫入 parent journal。

### 5.4 Default implementation dependencies

`defaultTaskCache` 擁有 entries、mutex、generation 與 matching algorithm。
它可接收 unexported dependency callbacks：

```go
type taskCacheDependencies struct {
    PolicyEngine  func() PolicyEngine
    Identity      func(TaskCacheLookupRequest) CacheIdentity
    Forbidden     func(task, verify string) bool
    SimilarTask   func(context.Context, string, []string, time.Duration) (int, error)
    ObserveThink  func(scope TaskCacheLookupScope, task string)
    AppendJournal func(journalRecord)
}
```

Production callbacks 只能轉接既有 `PolicyEngine`、`AgentPool`、think reporter 與
task journal；不得重做 policy、sidecar 或 journal persistence。callback nil 時
使用安全的既有 fallback：沒有 similarity 即 miss，沒有 journal 即 memory-only。

`Coordinator` 只新增一個 `taskCache TaskCache` field，以及 `TaskCache()` /
`SetTaskCache(TaskCache)`。`NewCoordinator` 必須 eager-install production default；
針對 tests 中的手工 `Coordinator{}`，getter 可在 `c.mu` 保護下 lazy-install，
但同一 coordinator 的後續 getter 必須回傳同一 instance，不能每次建立空 cache。
`RuntimeServices()` 與 `setRuntimeServices()` 同步納入 TaskCache。

### 5.5 Production call-site inversion

下列 production paths 全部改經 `c.TaskCache()`：

| Path | Required service operation |
| --- | --- |
| `dag_scheduler.go` | execution lookup、store |
| `coordinator_tools_delegate.go` | all-generations lookup、store |
| `coordinator_plan.go` | store |
| `coordinator_taskcache.go` duplicate checking | current-run lookup |
| `coordinator_execute.go` | `AdvanceGeneration` |
| `coordinator_session.go` | `Restore` with pinned seeds |
| `task_journal.go` | `Restore` with pinned seeds；open/load/compact 不搬 |
| `coordinator_extra_models.go` | `Fork(taskCacheDependenciesFor(clone))`；clone journal callback 為 nil |
| DAG on-failure reset | contract-specific `Invalidate` |

`cloneCoordinatorForModel` 必須先建立 clone value，再以該 clone 建 dependencies
並指定 forked cache；不可在 struct literal 中讓 callback 捕捉 parent。

舊的 helper names 可暫留為 package-internal compatibility wrappers供既有 tests
使用，但 production files 不得再直接讀寫 cache fields。PR-A 完成時執行：

```bash
rg -n 'taskResultCache|taskResultCacheMu|cacheGeneration' internal/team --glob '*.go' --glob '!*_test.go'
```

預期只命中 `defaultTaskCache` implementation；不得命中 `Coordinator`、scheduler、
session、journal 或 extra-model code。

### 5.6 Files

修改：

```text
internal/team/services.go
internal/team/coordinator.go
internal/team/coordinator_taskcache.go
internal/team/coordinator_execute.go
internal/team/coordinator_session.go
internal/team/task_journal.go
internal/team/coordinator_extra_models.go
internal/team/dag_scheduler.go
internal/team/coordinator_tools_delegate.go
internal/team/coordinator_plan.go
internal/team/services_test.go
internal/team/cache_test.go
internal/team/cache_freshness_test.go
internal/team/task_journal_test.go
internal/team/check_duplicate_test.go
```

允許新增：

```text
internal/team/task_cache_service_test.go
```

不要新增 `service_task_cache.go` 與現有 `coordinator_taskcache.go` 形成平行 owner；
若檔案過大，只能在 state ownership 完成後把 canonical implementation rename
成 `task_cache_service.go`，且 rename 必須留在同一 PR。

### 5.7 Characterization and acceptance tests

先寫會在 baseline 通過的 characterization tests，再搬 state。至少覆蓋：

1. use/refresh/bypass 與 profile override。
2. 三種 lookup scope 的 exact/semantic/pinned 差異。
3. sidecar nil、error、timeout 都只產生 miss。
4. typed verification normalization、fresh/stale fingerprint、observation、
   task-result assertion 與 workset 禁用。
5. generation prune、pinned restore、contract-specific invalidation與 tombstone。
6. session restore 的 seed count/order與 baseline 相同；task-journal restore 保留
   現有的 identity dedup 行為。
7. `Fork(...)` 深拷貝、active-generation reset、clone-specific dependencies、
   parent/child mutation isolation 與 no-journal behavior。
8. concurrent lookup/store/restore/invalidate 無 race。
9. normal DAG、direct delegation fast path、extra-model clone、crash-resume 各至少
   一條 integration path。
10. cache hit 的 `cache_hit` status/event 內容與次數不變。

PR-A targeted tests：

```bash
go test ./internal/team -run 'Test.*(Cache|Duplicate|Journal|Session|ExtraModel)' -count=1
go test -race ./internal/team -run 'Test.*Cache' -count=1
```

## 6. PR-B — EvidenceService extraction

PR-B 只能在 PR-A 合併且全綠後開始。它不改 evidence schema、artifact membership、
acceptance semantics 或 `auditverify`。

### 6.1 Contract

只暴露 coordinator 真正使用的 run-level API；task builder 保持 default
implementation 的 unexported method，避免無第二個 consumer 的寬 interface。

```go
type EvidenceBuildRequest struct {
    RunID     string
    Workspace string
    Items     []*TodoItem
    Strict    bool
}

type EvidenceFinalizeRequest struct {
    Workspace  string
    Manifest   *EvidenceManifest
    Acceptance *AcceptanceResult
}

type EvidenceService interface {
    BuildRunManifest(context.Context, EvidenceBuildRequest) (*EvidenceManifest, error)
    FinalizeRunManifest(context.Context, EvidenceFinalizeRequest) (*EvidenceManifest, error)
}
```

Preconditions：`Workspace`、task list 與 finalize 的 `Manifest` 必須有效；
`Coordinator` 負責從 authoritative run state 建 request。若 finalize 時尚無
manifest，`Coordinator` 先以 `Strict:false` 呼叫 `BuildRunManifest`，不要把
coordinator state lookup 藏進 service。

### 6.2 Ownership

`defaultEvidenceService` 擁有：

```text
per-task evidence construction
receipt/transcript/artifact binding
artifact occurrence projection
manifest status calculation
acceptance evidence replacement
Seal + Verify
logs/evidence_manifest.json serialization/write
```

`Coordinator` 仍擁有：

```text
executionRunID
TaskTracker and authoritative TodoItems
lastEvidenceManifest pointer + lock
決定 strict/non-strict call timing
run finalization and CompletionGate ordering
```

`BuildRunManifest` 只有完整成功後才把回傳值指定給 `lastEvidenceManifest`，這與
baseline 相同。注意 baseline 的 `finalizeEvidenceManifest` 會先 in-place 修改既有
manifest，再執行 Seal/Verify/write；PR-B 必須先用 failure-injection test 固定這個
現況，並保持相同的 call ordering。若要改成 copy-on-finalize/成功後原子 swap，
那是 failure-semantics 改善，必須另開 PR，不可藏在本次 extraction。

offline `internal/auditverify` 仍是獨立 verifier，只重用 `EvidenceManifest.Verify`
與 artifact store；不得呼叫 runtime `EvidenceService`，也不得複製一套 verifier。

### 6.3 Call sites and files

修改：

```text
internal/team/services.go
internal/team/coordinator.go
internal/team/evidence_manifest.go
internal/team/coordinator_tools.go
internal/team/coordinator_run.go
internal/team/run_finalizer.go
internal/team/completion_gate.go（只在需要改 service getter 時）
internal/team/services_test.go
internal/team/evidence_binding_test.go
internal/team/coordinator_terminal_receipt_test.go
internal/team/phase0_test.go
internal/team/verification_integration_test.go
```

不新增 `service_evidence.go` 與 `evidence_manifest.go` 平行。若需要 rename，規則
同 PR-A：canonical owner 只能剩一份。

### 6.4 Acceptance

至少固定：

- strict / non-strict missing evidence 結果不變。
- zero-task、no-completed-task、failed-task manifest status 不變。
- successful verifier、typed evidence、transcript fallback 與 artifact membership 不變。
- receipt binding仍含 run/task/attempt/model execution/producer/transcript。
- acceptance not-configured/passed/failed 分別維持 unverified/accepted/failed。
- repeated finalize 只保留一筆 `run:acceptance` evidence。
- manifest seal/hash/verify 與 persisted JSON 保持有效。
- finish、emergency finalizer、unattended acceptance recovery、terminal evidence path
  皆經 service，event ordering 不變。
- injected failure 不得發布成功 outcome；`lastEvidenceManifest` 的 build/finalize
  failure state 必須分別符合新增的 baseline characterization tests。

Targeted tests：

```bash
go test ./internal/team -run 'Test.*(Evidence|Manifest|Acceptance|Finaliz|TerminalReceipt)' -count=1
```

## 7. PR-C — Recovery ownership consolidation

不新增 `RecoveryManager`。Canonical decision owner 固定為既有
`RepairController`；`recovery.go`、failure classification、disposition、commit
gate、anti-thrashing 與 StopPolicy 保留各自既有責任。

### 7.1 Scope

- 將 `RepairController *RepairController` 加入 `RuntimeServices` 與
  `setRuntimeServices`。
- inventory task-level retry/reconcile/escalate/replan call sites；只有「選擇下一步」
  的地方要經 `RepairController.Decide/Execute`。
- mechanical callback implementation 可留在原檔案，但不得自行重新判斷
  replayability、side-effect class、attempt budget 或 rollback authorization。
- run-level acceptance self-healing/rollback仍由 finish/run finalization owner；
  不硬塞入 task-level `RepairController`。
- operator targeted reconciliation 必須保留顯式 authorization；controller 不能授權
  side effect。
- StopPolicy、commit gate、verification、receipt、checkpoint/event append ordering不變。

主要 impact files：

```text
internal/team/coordinator.go
internal/team/services.go
internal/team/repair_controller.go
internal/team/coordinator_task_run.go
internal/team/coordinator_session.go
internal/team/targeted_recovery.go
internal/team/coordinator_failure.go
internal/team/disposition.go
internal/team/coordinator_tools.go
internal/team/repair_controller_test.go
internal/team/recovery_test.go
internal/team/coordinator_repair_wiring_test.go
```

### 7.2 Acceptance

固定以下 decision/callback matrix：

| Condition | Expected action |
| --- | --- |
| replayable + no side effect + budget available | retry，或 task 已要求時 escalate |
| manual / never recovery | block |
| non-replayable external/infra/credential/unknown side effect | reconcile 或 block；不得直接 retry |
| reconcile proves not-started | attempt budget內才可 retry/escalate |
| reconcile unknown/partial/error | block |
| replayable task或 reconcile 已證明 not-started，且 attempt budget exhausted | replan |
| rollback未顯式授權 | block |
| checkpoint callback失敗 | 不執行任何 action callback |
| protocol result repair | 不重播 worker side effect |

每個 action callback 最多一次。crash-resume 保持原 Todo ID、dependency order、
completed result reuse；error task 不因 refactor 自動跨 run retry。

若 inventory 發現某個現行 bypass 其行為與上述 matrix 不同，立即停止 PR-C：
先把它記成獨立 semantic-change proposal，不能藉 service extraction 偷改行為。

Targeted tests：

```bash
go test ./internal/team -run 'Test.*(Repair|Recovery|Retry|Reconcile|Resume|Disposition)' -count=1
```

## 8. PR-D — Internal composition root

PR-D 在前述 service contracts 穩定後才做。目的只是不再為新增 service 擴張
positional constructor；不在本計畫移除或修改 exported `NewCoordinator(...)`。

### 8.1 Internal API

新增 unexported config value，完整承接現有 constructor 參數：

```go
type coordinatorParams struct {
    Session               *TeamSession
    DefaultProviderURL    string
    DefaultProviderAPIKey string
    MCPManager            *mcp.MCPToolManager
    MemoryStore           *memory.MemoryStore
    ModelList             []config.ModelEntry
    RoleModels            RoleModels
    MaxConcurrent         int
    Verbose               bool
    Think                 bool
    Direnv                bool
    AllowedPaths          []string
    PathConsent           *tools.PathConsent
    HookRegistry          *hooks.HookRegistry
    RestrictedBash        bool
    RestrictedPath        string
    NoNet                 bool
    ForceMCP              bool
    ForcedSkillNames      []string
    PlanMode              bool
    AutoSkillsMode        bool
}

func newCoordinator(params coordinatorParams, services RuntimeServices) (*Coordinator, error)
```

現有 exported constructor 只負責把 positional arguments 映射到
`coordinatorParams`，再用空 `RuntimeServices` 呼叫 `newCoordinator`。所有既有
callers、source compatibility 與 defaults 不變。

### 8.2 Initialization order

固定順序：

1. validate constructor inputs and build coordinator base state。
2. 建立所有 nil-field 對應的 production default services。
3. 套用 injected non-nil services。
4. 才建立會捕捉/使用 service 的 dependent tools、reviewers 與 runtime callbacks。
5. 初始化途中失敗時，清理本次 constructor 已建立的 repo/resource；不得關閉
   caller 注入且仍由 caller 擁有的 dependency。

本 PR 不把 context repository、terminal manager、worker/shared memory 強行塞入
`RuntimeServices`。它們有不同 resource ownership/authority；只有另立設計並有
測試 seam value 時才可處理。

Files：

```text
internal/team/coordinator.go
internal/team/services.go
internal/team/services_test.go
internal/team/coordinator_constructor_test.go（允許新增）
```

Acceptance：

- existing `NewCoordinator` compile/runtime behavior不變。
- empty bundle 的所有 service getter 非 nil。
- partial injection 只替換指定 field，其餘使用 default。
- injected service 在第一個 dependent call 前生效。
- constructor error path 不洩漏本次建立的 context/resource。
- no production caller 使用 `setRuntimeServices` 進行 late mutation。

## 9. Explicitly deferred work

以下不屬於本計畫，也不是完成條件：

- 把 `Coordinator` 所有欄位收進單一 `services` field。
- 改 exported `NewCoordinator` 為 options API。
- 合併 `WorkerMemoryService` 與 `SharedMemoryService`。
- 移除 legacy `MemoryStore` migration adapter。
- 重寫 terminal subsystem或合併 worker/cleanup/transfer authority。
- 新建 event façade或替換 `EventJournal`/`EventStore`。
- 搬 DAG/phase orchestration到新的 workflow god object。
- 改 cache、recovery、evidence、acceptance 或 event semantics。
- 把 evidence finalize 改成 copy-on-write/atomic state swap；先另立
  failure-semantics 修正與 recovery test。
- 檔案/package 大搬家與全面 method rename。

若上述工作後來有明確 second implementation、test seam 或 ownership value，另寫
新計畫並重新 baseline，不能擴張本 PR scope。

## 10. Compatibility gates

### 10.1 每個 PR 的共同 gates

任何 Go source/test 變更都必須依序通過：

```bash
go test ./...
go vet ./...
golangci-lint run
go build ./cmd/hufu
```

`golangci-lint run` 必須零錯誤；不得以「既有問題」結束。

### 10.2 Deterministic runtime evals

workflow regression harness 已完成且是 blocking CI，不再使用「若完成」條件。
每個 PR 先 build 一次本地 binary，再跑受影響 suite：

```bash
go build -o /tmp/hufu-service-extraction ./cmd/hufu
/tmp/hufu-service-extraction eval run ./evals/core-lifecycle --format json
```

附加 suites：

| PR | Required suites |
| --- | --- |
| PR-A | `core-lifecycle`、`retry-recovery`、`execution-target`、`legacy-execution-replay` |
| PR-B | `core-lifecycle`、`strict-verification`、`terminal-leak`、`memory-learning` |
| PR-C | `retry-recovery`、`side-effect-recovery`、`execution-target`、`legacy-execution-replay` |
| PR-D | 全部 `evals/*` suites |

最後一個 PR 必須執行與 CI 相同的全部 suite loop；結果中任何 failed case 都是
blocker。不能更新 fixture/golden 來掩蓋 extraction 造成的差異。

### 10.3 Observable compatibility oracle

比較時忽略 nondeterministic timestamp/opaque ID 的字面值，但固定：

```text
RunResult outcome/stop reason/acceptance
task count/status/attempts/verification/failure class
ExecutionTarget and BackendBinding
event critical type/order/cardinality/fields
receipt run/task/attempt/backend/producer/transcript binding
EvidenceManifest status/hash validity/artifact membership
cache hit count and selected output
session/task-journal restore behavior
unattended and crash-resume disposition
```

## 11. PR discipline

每個 PR 內的 commit order：

```text
1. characterization tests
2. interface/types
3. production default implementation
4. production call-site inversion
5. state/locking ownership move
6. compatibility cleanup
7. targeted + full validation
```

Rules：

- 一次只抽一個 subsystem。
- 不做 opportunistic semantic fix；發現真 bug 時另開 issue/PR。
- 不先刪舊路徑再補 projection/persistence。
- 每個 PR 必須可獨立 build、test、部署；相依 PR 只能按反向順序 revert。
- 不允許 production call site 繞過新 interface，同時保留第二份 state owner。
- 不為沒有 second implementation/test seam 的元件硬抽 interface。

## 12. Stop conditions

遇到下列任一情況，停止該 PR 並先修訂設計：

- canonical owner 或 state transition API 不明。
- 必須改 event/schema/result/cache/retry/acceptance semantics 才能完成 extraction。
- 新路徑會繞過 policy、receipt、verification、persistence 或 authorization。
- repair/retry 可能重播 external/infra/credential side effect。
- test plan 無法區分 success、partial、blocked、verification failure。
- 必須同時抽兩個以上 subsystem 才能讓單一 PR compile。
- deterministic eval 差異無法用純結構搬移解釋。

## 13. Done（已達成）

本計畫全部完成的標準：

- TaskCache 是 cache state、generation、matching、restore/fork/invalidate 的單一 owner。
- EvidenceService 是 runtime evidence build/finalize/publish 的單一 owner；
  `Coordinator` 只組 request、保存成功結果並控制 run ordering。
- 一般 task retry 與 targeted recovery 的安全決策經既有 `RepairController`；
  crash-resume 保留原 session recovery/disposition 語意，且沒有新增
  `RecoveryManager` 或平行 retry policy。
- `RuntimeServices` 可替換 TaskCache、EvidenceService、RepairController；nil
  injection 穩定回退 production defaults。
- exported `NewCoordinator` 相容，internal construction 不再因 service 增加
  positional parameter。
- production call sites 不直接操作已抽取 service 的 private state。
- 所有 targeted tests、`go test ./...`、`go vet ./...`、`golangci-lint run`、
  build 與 required eval suites 全綠。
- 沒有 event/schema/result/retry/acceptance/ExecutionTarget observable drift。

最終驗證結果：

```text
PR-A targeted tests + cache race tests: passed
PR-B targeted tests: passed
PR-C targeted tests: passed
go test ./...: passed
go vet ./...: passed
golangci-lint run: 0 issues
go build ./cmd/hufu: passed
evals/* (10 suites, CI-equivalent loop): all cases passed
```

## 14. Coding-agent 起始指派（歷史）

```text
只實作 PR-A TaskCache ownership extraction。
先在 baseline 行為上補 characterization tests，再加入 TaskCache contract、
defaultTaskCache、getter/setter 與 RuntimeServices field，最後逐一 inversion
第 5.5 節所有 production call sites並搬移 state。不可修改 cache semantics、
event payload、retry/recovery、evidence、terminal、workflow 或 memory。
完成第 5.7、10.1 與 PR-A eval gates 後才可宣告完成。
```
