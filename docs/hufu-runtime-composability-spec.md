# Hufu Runtime Composability 可實作計畫

> 程式碼基準：`e36c550`（2026-08-14）
> 本文件取代原本偏概念性的 Runtime Composability 建議，以下工作項目均以目前 repository 已存在的型別、路徑與 runtime invariant 為基礎。

## 1. 目標與非目標

本計畫只處理四個可獨立交付的架構邊界：

1. 讓既有 `EventStore` 足以重建 runtime execution state，並逐步把 `session.json`、`task_journal.jsonl` 與 `execution-events.jsonl` 降級為 projection／compatibility output。
2. 把 worker tool assembly、model resolution 與 agent attempt execution 從 `Coordinator.executeTask` 拆成可替換服務。
3. 建立 Hufu-owned 的 `SubagentProvider` 邊界，但第一階段只提供目前 Fantasy agent 的 local adapter。
4. 把 LTM extraction 與 candidate confirm/reject 從 `finishTool`、direct-agent path 移到共同的 run finalization／experience processor。

以下不在本計畫內：

- 重寫 `internal/team` 或替換 Fantasy。
- 新增 DI framework。
- 重做 SQLite canonical context repository、`WorkerMemoryService` 或 promotion workflow。
- 實作 Firecracker、E2B、Kubernetes 或 remote execution world。
- 允許外部 agent 自行決定 Hufu task acceptance。
- 自動修改 skill、team policy 或 agent policy。
- 在尚未提供 Hufu-owned tool broker／隔離環境前啟用可任意操作 workspace 的 command provider。

所有 Go PR 必須分別通過：

```bash
go build ./cmd/hufu
go test ./...
go vet ./...
golangci-lint run
```

## 2. 目前程式碼事實

### 2.1 已存在，不應重做

| 能力 | 現有落點 | 本計畫處理方式 |
| --- | --- | --- |
| Append-only hash-chained event log | `internal/team/event_store.go` 的 `RunEvent`、`EventStore` | 擴充 catalog、reducer 與 commit boundary，不新增第二套 store |
| Session/task replay | `internal/team/event_reducers.go` 的 `ReduceToSessionData`、`ReduceToTodoList` | 補齊 parity，沿用 reducer |
| Branch time-travel replay | `internal/team/session_tree.go` 的 `RebuildSessionForBranch` | 改成 canonical replay 的驗收 consumer |
| Runtime service seams | `internal/team/services.go` | 擴充並改成 constructor-injected bundle，不另建 DI framework |
| Central tool authorization | `internal/team/tool_policy_gate.go` 的 `createGatedAgent` | 所有 local provider path 必須繼續走此 gate |
| Typed result、receipt、verification、recovery | `task_result.go`、`execution_receipt.go`、`verification.go`、`recovery.go` | 保留在 Hufu runtime，不下放給 provider |
| Canonical context + Markdown projection | `internal/context/sqlite_repository.go`、`internal/context/projection.go` | SQLite 維持內容權威；Markdown 維持 projection |
| Candidate lifecycle | `shared_memory.go`、`worker_memory.go`、`completion_gate.go` | 搬移 orchestration，不新增平行 candidate type |
| Experience aggregation | `memory_outcome.go`、`internal/context/experience.go` | 讓 processor 呼叫現有 reducer／repository |
| Human-reviewed promotion | `internal/promotion/`、`cmd/hufu/context_promotion_cmd.go` | 視為既有能力，只補 integration boundary |

### 2.2 尚未成立的邊界

- Event type 仍是散落的 string；terminal event 以外缺少一致的 payload validation。
- `RecordSession*` 與 task checkpoint 仍是先改 mutable state，再 dual-write event；EventStore 尚未真正成為所有 transition 的 source of truth。
- Reducer 已可重建部分 `SessionData`／`TodoItem`，但尚未證明與 runtime checkpoint 完整等價。
- `execution-events.jsonl` 仍由 `executionEventLogger` 獨立寫入，不是 EventStore exporter。
- `services.go` 多數 default implementation 仍直接回呼 `Coordinator`，尚未降低 `executeTask` 的實際 coupling。
- `coordinator_task_run.go` 同時負責 tool assembly、Fantasy agent construction、attempt execution、result protocol、retry、verification 與 recovery。
- `AutoExtractLTM` 仍由 `finishTool` 與 `finalizeDirectRun` 直接呼叫。
- 外部 command provider 若直接取得 workspace，現階段無法證明它遵守 Hufu tool policy、side-effect classification、receipt 與 evidence contract。

## 3. 不可破壞的 Runtime Invariants

每個 PR 都要在描述與測試中指出它影響哪些 invariant：

1. `CompletionGate` 是唯一能把 run 認定為 accepted 的 policy；模型文字與 provider status 都不是完成證據。
2. Task transition 必須同步 checkpoint、task journal、event store、status projection、CLI/JSON/report/TUI consumer。
3. 所有 Hufu local agent tool 必須經 `createGatedAgent`／central policy gate，且 model-visible tools 必須等於 runtime allowlist。
4. Retry、repair、reconcile 與 crash-resume 不得重播已完成或可能已完成的 side effect。
5. Event payload、transcript、receipt、session、report 與 context store 在持久化前必須 redaction。
6. Artifact reference 必須維持 opaque/content-addressed ID 與授權檢查。
7. SQLite context repository 是 memory content 的權威；EventStore 只記 provenance/identity，不複製完整知識內容。
8. Skill/team/agent policy promotion 必須保留 analyze → review → approve → apply 的人工 gate。

## 4. 實作階段與 Merge 順序

```text
Phase 0  Replay baseline
   ↓
Phase 1  Event catalog → replay parity → event-first cutover
   ↓
Phase 2  Tool/model/attempt service extraction
   ↓
Phase 3  Local SubagentProvider migration
   ↓
Phase 4  Common run finalization + experience processor
   ↓
Phase 5  Projection exporter and legacy cleanup
   ↓
Gate X   External provider protocol（本計畫只產出設計與測試契約）
```

Phase 1 完成前不得宣稱 EventStore 是完整 source of truth。Phase 3 完成前不得加入 provider config。Phase 5 cleanup 只能在 shadow parity 連續通過後進行。

## 5. Phase 0 — 凍結現有行為

### PR-00：Runtime replay baseline

目標：以目前 production 路徑建立「live checkpoint 與 event replay 必須等價」的基準。

修改範圍：

- 擴充 `internal/team/phase0_test.go`，不要另建一套 fake runtime framework。
- 重用現有 deterministic agent/test seams、`EventStore`、`ReduceToSessionData`、`ReduceToTodoList`。
- 新增 test helper，把 unstable 欄位（timestamp、random event ID、hash）排除後比較 projection。

必須新增的案例：

1. task created → started → typed result → verification → done。
2. verify failure → retry → success，保留所有 `ExecutionReceipts` 與 retry count。
3. protocol incomplete → result-only repair → done。
4. cancellation／blocked 不被 replay 成 success。
5. direct-agent finalization 與 coordinator finish 產生相同的 acceptance/evidence outcome。
6. branch checkout 只 replay lineage，不混入 sibling branch task 或 memory event。
7. EventStore append/sync failure 後不得產生無 event 的 accepted runtime state。

驗收：

- 測試明確列出目前 reducer 尚未 round-trip 的欄位；缺口作為 Phase 1 backlog，不以模糊 assertion 隱藏。
- 不改 production behavior。

## 6. Phase 1 — 完成既有 EventStore

### PR-01：Typed event catalog 與 payload validation

新增：

```text
internal/team/event_types.go
internal/team/event_payloads.go
internal/team/event_payloads_test.go
```

實作：

- 定義 `type EventType string` 與目前實際使用的 constants；先涵蓋 `run_*`、`task_*`、message、artifact、criterion、memory、workflow、policy、recovery。
- 將 `RunEvent.Type` 保持為 `string` 以維持 JSON/source compatibility；producer 使用 `string(EventTaskCompleted)`，避免一次改完整 repository。
- 為 terminal task/run 與 reducer-consumed events 提供 typed payload struct 與 `ValidateEventPayload(event RunEvent) error`。
- 在 `EventStore.Append` redaction 後、write 前驗證 schema version、required identity 與 payload。
- 不把完整 tool output 或 memory content加入 event payload；只保留 hash、redacted preview、artifact/context item ID。

測試：

- legacy schema-v1 event 仍可 read/replay。
- malformed terminal/task payload fail closed。
- secret 在 hash 計算前已 redaction。
- unknown event 可保存供 forward compatibility，但 reducer 必須忽略而不能破壞 replay。

### PR-02：Reducer parity

修改：

```text
internal/team/event_reducers.go
internal/team/event_reducers_test.go
internal/team/session_tree.go
```

實作：

- 以現有 `SessionData` 與 `TodoItem` 作 projection model，不新增內容重疊的 `RuntimeProjection` mega-struct。
- 補齊 resume 必需欄位：task execution contract、side effect/recovery、verification spec/result、typed result、all receipts、memory manifests、failure event/fingerprints、criterion state、run result/evidence。
- 對同一 idempotency key 與同一 task transition做 deterministic dedup。
- 定義 reducer 對 duplicate、out-of-order、unknown schema event 的明確行為。
- `RebuildSessionForBranch` 只使用 reducer 結果；branch snapshot 只作沒有 canonical events 的 legacy fallback。

驗收：

- Phase 0 的 live vs replay parity table 全部通過。
- 同一 event slice replay 多次得到 deep-equal projection。
- sibling branch、duplicate event、truncated tail 都有測試。

### PR-03：EventJournal service 與 durable append result

修改：

```text
internal/team/services.go
internal/team/event_store.go
internal/team/coordinator_eventstore.go
internal/team/coordinator.go
```

新增最小介面：

```go
type EventJournal interface {
    Append(ctx context.Context, event RunEvent) (RunEvent, error)
    ReadEvents(ctx context.Context) ([]RunEvent, error)
    VerifyHashChain(ctx context.Context) error
}
```

實作注意：

- 保留既有 `EventStore.Append(RunEvent) error` 給現有 caller。
- 將 append 核心抽成可回傳已補齊 ID、timestamp、branch、hash 的內部函式，由 `eventStoreJournal` adapter 使用。
- `Coordinator` 透過 service 取得 journal；`NewCoordinator` 維持既有 signature，另以 internal `RuntimeServices` bundle 注入 default adapter。
- 測試可注入 failing/in-memory journal，不依賴實際 JSONL。

### PR-04：先切換 session message commit boundary

以低風險的 `user_message_added`、`assistant_message_added` 驗證 event-first 模式：

```text
validate event
→ durable append
→ Reduce/Apply to in-memory SessionData
→ SaveSession compatibility projection
→ notify observers
```

修改 `RecordSessionUserMessage`、`RecordSessionAssistantMessage` 與其 caller，使 append 失敗時不先修改 `SessionData`。

驗收：

- append failure 時 conversation history 與 `session.json` 都不前進。
- append success、projection write failure 時，restart 可由 EventStore 修復 projection。
- repair 必須 idempotent，不能重複 conversation entry。

### PR-05：Task transition event-first cutover

這是 Phase 1 風險最高的 PR，不可與 PR-04 合併。

實作：

- 新增 coordinator-owned `CommitTaskTransition`；輸入是 task ID、expected current status、next status、output/detail 與 transition metadata。
- 在不 mutate `TodoItem` 的情況下先產生完整 event payload；append 成功後才套用 canonical Todo transition API。
- 保留 `TodoList.onChange → saveCheckpoint`，但移除已切換 transition 的事後 `emitTaskEventsFromCheckpoint`，避免雙 event producer。
- 分批切換 pending/started/terminal status；每一批都保留 idempotency key 與 compare-and-transition guard。
- verification result、typed result、receipt 應先成為 transition payload 的一部分，再讓 checkpoint/projector 消費。

必測路徑：normal worker、direct agent、protocol repair、resume、sidecar、extra-model、cancellation、failed verification。

驗收：

- EventStore append 失敗時 task status 不變。
- Event append 成功而 process 在 checkpoint 前 crash 時，restart replay 得到 next status。
- side effect 已執行但 terminal event 未確定時，resume 進 reconciliation，不直接 replay worker。

### PR-06：Startup replay 與 shadow parity

- 啟動時 verify hash chain，再 replay canonical session/task projection。
- 與現有 `session.json` checkpoint 做 normalized shadow compare；不一致時 fail closed 或標記 recovery-required，不默默選一份。
- `session.json` 仍寫出供 compatibility/debug 使用，但不覆蓋較新的 canonical event state。
- 提供明確 metric/event：`projection_rebuilt`、`projection_mismatch`、`projection_write_failed`。

Phase 1 完成條件：刪除 `session.json` 與 task projection後，只靠 EventStore + domain stores 可重建可 resume 的 session/task/run state；artifact bytes 與 context content仍由各自 store提供。

## 7. Phase 2 — 抽出實際 Capability Services

### PR-07：RuntimeServices bundle 與 ToolResolver

擴充 `internal/team/services.go`：

```go
type ResolvedWorkerTools struct {
    Tools        []fantasy.AgentTool
    Names        []string
    Capabilities []string
}

type ToolResolver interface {
    ResolveTaskTools(ctx context.Context, def *agent.AgentDef, task TaskDef, extras []fantasy.AgentTool) (ResolvedWorkerTools, error)
}

type ModelRuntime interface {
    ResolveTaskModel(def *agent.AgentDef, task TaskDef) (string, error)
    ProviderFor(modelID string) (*agent.OllamaProvider, error)
}
```

實作：

- 把 `selectWorkerToolsForTask`、MCP loading、phase filtering、force-MCP、deny filtering與 tool-name extraction集中在 default resolver。
- Resolver 回傳的 `Names` 是 model-visible/runtime allowlist 的唯一來源。
- central authorization仍由 `createGatedAgent`套在 concrete tools外層；resolver不得自行放寬 policy。
- 將既有 Planner/SessionStore/PolicyEngine/ContextCompiler/AgentPool/WorkflowEngine 與新 service放入 `RuntimeServices`。
- `NewCoordinator`建立 default bundle；測試使用 internal constructor注入 fake bundle，避免繼續增加公開 constructor參數。

驗收：

- coordinator task tests可使用 fake resolver/model runtime而不啟動 MCP/Ollama。
- phase capability同時影響 model-visible names與 execution gate。
- `TestAgentsAreCreatedThroughTheGatedConstructor`持續通過。

### PR-08：TaskResultSink 與 attempt runner seam

目前 `submitResultTool`直接寫入 Coordinator。先抽出 task-scoped sink：

```go
type TaskResultSink interface {
    Submit(ctx context.Context, taskID string, result TaskResult) error
}
```

再定義只代表「一次 worker attempt」的 runner；retry、verification、acceptance不在 runner內：

```go
type AttemptRequest struct {
    RunID, BranchID, TaskID string
    Attempt                 int
    Agent                   *agent.AgentDef
    Task                    TaskDef
    Prompt                  string
    ModelID                 string
    MaxSteps                int
    Timeout                 time.Duration
    Tools                   ResolvedWorkerTools
}

type AttemptResult struct {
    Output            string
    TypedResult       *TaskResult
    Usage             ExecutionUsage
    StepsUsed         int
    StopReason        string
    ProviderSessionID string
    TranscriptRef     string
}

type AttemptRunner interface {
    RunAttempt(context.Context, AttemptRequest) (AttemptResult, error)
}
```

驗收：

- Fake runner可回傳 success、partial、malformed result、timeout、cancellation。
- `executeTask`仍負責 receipt finalization、typed-result validation、terminal evidence、verify、retry/recovery。
- runner error保留 partial output/usage，不能遺失 retry evidence。

本階段不新增空殼 `ExecutionWorld`；在沒有第二個安全 backend前，只有 `ID()/Root()` 的介面無法約束任何能力，反而會製造錯誤安全感。

## 8. Phase 3 — Local SubagentProvider

### PR-09：Provider contract 與 registry

新增：

```text
internal/team/subagent_provider.go
internal/team/subagent_registry.go
```

介面沿用 Phase 2 的 neutral attempt DTO：

```go
type SubagentProvider interface {
    Name() string
    Capabilities() SubagentCapabilities
    RunAttempt(context.Context, AttemptRequest) (AttemptResult, error)
}
```

`SubagentCapabilities` 第一版只描述可驗證能力：supports Hufu tools、typed result、streaming activities、resume token。不可用 capability宣告繞過 runtime policy。

Registry規則：

- default key固定為 `hufu-local`。
- unknown provider在 model call與side effect前 fail closed。
- 本 PR 不增加 `team.yaml`／agent frontmatter欄位。

### PR-10：HufuLocalSubagentProvider adapter

新增 `internal/team/subagent_hufu.go`，封裝目前的：

- `createTaskAgent`／`createTaskAgentWithResultTool`
- `createGatedAgent`
- `runAgentWithStatusAndHistory`
- Fantasy step → neutral `AttemptResult` usage/activity mapping

Provider不得執行：

- retry/escalation decision
- deliverable verification
- terminal failure acceptance
- completion gate
- memory candidate confirmation
- task status terminal transition

驗收：在未改任何 team config時，baseline tests證明 local provider的 output、receipt、tool authorization與retry classification與舊路徑相同。

### PR-11：`executeTask` 切換 provider

將每次 attempt內的 agent construction/stream call替換為：

```text
resolve agent/task contract
→ ToolResolver
→ ModelRuntime
→ build AttemptRequest
→ SubagentRegistry.Resolve("hufu-local")
→ provider.RunAttempt
→ Hufu result/receipt/verification/recovery pipeline
```

只移動 attempt execution；不要搬動 retry loop與 completion policy。

必測：plan-first、escalated model、protocol repair、sidecar fallback、resume、direct-agent fast path。若某路徑不是 task attempt，必須明確保留原路徑並在文件中說明，不得假裝已由 provider涵蓋。

## 9. Gate X — 外部／Command Provider 前置契約

原草案的 `CommandSubagentProvider` 不能直接排在 local adapter之後實作。外部 process若能自行執行 shell或修改 workspace，Hufu無法只靠 `ToolNames` 證明它遵守 policy、closed sequence、receipt與artifact scope。

在加入任何 provider config前，先完成一份 ADR與 executable contract tests，至少回答：

1. 外部 provider只能透過 Hufu tool broker執行 tool，還是執行於受限 execution world？
2. cancellation、timeout、stdout/stderr上限與process-tree cleanup如何保證？
3. JSON framing、typed result schema、activity stream與resume token如何版本化？
4. side-effect receipt如何由 Hufu簽發，避免 provider自報成功？
5. workspace、secret、network與MCP capability如何授權？
6. provider crash後如何判定 reconcile或replay？

只有上述 tests能 fail closed後，才新增：

```yaml
subagent-providers:
  external-coder:
    type: command
    command: ["some-agent", "run"]
```

在此之前 repository可保留 registry與fake provider，但 production只註冊 `hufu-local`。

## 10. Phase 4 — Common Finalization 與 Experience Processor

### PR-12：不可變的 run finalization input

新增 `internal/team/run_finalizer.go`：

```go
type RunFinalizationInput struct {
    RunID       string
    Result      *RunResult
    Acceptance  *AcceptanceResult
    Evidence    *EvidenceManifest
    Tasks       []TodoItem
    BranchID    string
}
```

建立一個共同 finalization流程，依序：

1. finalize/verify evidence manifest。
2. evaluate canonical run outcome。
3. apply `CompletionGate`。
4. prepare/finalize experience candidates。
5. persist reliability evaluation與telemetry。
6. append `run_finished`。

`finishTool`只提出 finish response與處理interactive self-healing；direct-agent與non-tool terminal path都呼叫同一 finalizer。

### PR-13：ExperienceProcessor adapter，不新增平行 storage

新增 `internal/team/experience_processor.go`：

```go
type ExperienceProcessor interface {
    Prepare(context.Context, RunFinalizationInput) error
    Finalize(context.Context, RunFinalizationInput, CompletionGateDecision) error
}
```

default implementation重用：

- `AutoExtractLTM`／`autoExtractCanonicalLTM`的 extraction規則。
- `SharedMemoryService`與`WorkerMemoryService`的 candidate lifecycle。
- `memoryObservationFromEvent`與`ExperienceRepository`的 idempotent aggregation。
- SQLite `ContextItem`、`ExperienceAggregate`、`PromotionProposal`；不要再新增 `ExperienceCandidate` table/type。

順序要求：

- `Prepare`只建立 candidate，不能 confirm。
- `Finalize`只在 accepted manifest與completion decision通過時 confirm；其他結果reject或保留明確的diagnostic candidate。
- candidate identity由 run/task/source content hash決定，重跑processor不得重複建立。
- processor只讀 immutable finalization input、EventStore、EvidenceManifest與canonical context repo；不能讀 mutable conversation scratch state決策。

### PR-14：移除 finish/direct path 的 learning responsibility

- 從 `finishTool.Run`與`finalizeDirectRun`移除直接 `AutoExtractLTM`呼叫。
- 讓 common finalizer在所有 terminal path呼叫 ExperienceProcessor。
- 保留 `memory_save`與`ltm_update`「直接寫 canonical candidate + emit identity/provenance event」的行為；不要改成把完整 memory content寫進 EventStore後再異步還原。
- legacy無SQLite模式繼續透過adapter支援，直到明確宣布migration policy。

驗收：

- coordinator finish、direct-agent、cancelled/failed run都不留下未決candidate。
- accepted run只confirm一次；processor重跑不產生duplicate candidate/aggregate。
- failed acceptance可留下diagnostic/negative experience，但不能產生confirmed instruction或promotion proposal。
- `context-stm.md`與`context-ltm.md`只由`SQLiteRepository.RebuildProjection`產生。

### PR-15：Promotion integration contract

不新增 self-improvement subsystem。補一條 integration test證明：

```text
confirmed context item
→ repeated verified experience aggregates
→ internal/promotion Analyzer proposes draft
→ operator approve
→ apply with target/base hash check
```

並證明 failed/stale evidence、private memory跨agent/team、未批准proposal都不能修改 skill/team/agent文件。

## 11. Phase 5 — Exporter 與 Legacy Cleanup

### PR-16：`execution-events.jsonl` exporter

- 先定義 EventStore event → `ExecutionEvent`的純函式mapper。
- 保持現有 JSON schema與debug bundle consumer相容。
- runtime只append EventStore；exporter生成`execution-events.jsonl`。
- shadow模式同時產生舊/new output並做normalized compare；不一致時保留舊writer並回報metric。

### PR-17：Task journal與session projection ownership

逐一分類：

| 檔案 | 最終角色 |
| --- | --- |
| `logs/event_store.jsonl` | execution transition/provenance source of truth |
| `session.json` | resume compatibility projection |
| `logs/task_journal.jsonl` | task-result recovery/index projection，非第二套狀態機 |
| `logs/execution-events.jsonl` | telemetry/export projection |
| `context.sqlite` | canonical context、experience與promotion content/state |
| `context-stm.md`、`context-ltm.md` | human-readable projection |
| artifact store | artifact bytes/integrity source of truth |

只有符合以下條件才移除對應dual writer：

- new producer已有至少一個failure injection test。
- restart/replay、branch checkout、TUI、JSON output、report、debug bundle全部改讀canonical/projection owner。
- shadow parity覆蓋success、partial、blocked、cancelled、acceptance failed。
- migration可讀既有workspace，不要求使用者刪除session。

## 12. 最終 Integration Test Matrix

不要只建立一個過大的 E2E；建立共享 deterministic fixture，再分成以下 tests：

| Test | 主要證明 |
| --- | --- |
| `TestRuntimeReplayAcceptedRun` | task/result/receipt/verify/acceptance/evidence可由events重建 |
| `TestRuntimeReplayFailedRun` | failure class、retry、blocked與negative experience不被洗成success |
| `TestRuntimeCrashAfterEventBeforeProjection` | durable event可修復projection |
| `TestRuntimeEventAppendFailureBeforeTransition` | append失敗不修改runtime state |
| `TestLocalSubagentProviderParity` | local adapter不改變tool/result/usage語意 |
| `TestProviderCannotAcceptTask` | provider success仍需Hufu verification/completion gate |
| `TestProviderCancellationPreservesEvidence` | partial output/usage/receipt與process cleanup |
| `TestExperienceProcessorIdempotent` | 同run重跑不重複candidate或aggregate |
| `TestFailedRunCannotPromoteMemory` | failed/partial run不confirm、不產生eligible promotion |
| `TestPromotionRequiresApprovalAndFreshTarget` | 未批准或stale target無法修改文件 |
| `TestExecutionEventExporterParity` | exporter維持既有JSON consumer相容 |

每個測試必須使用 repository內 fake agent/provider/repository；不得依賴 live Ollama、外部 command、Pilot checkout或網路。

## 13. 每個 PR 的完成定義

每個 PR 描述必須包含：

- 變更的 canonical owner與受影響 execution paths。
- preserved invariants與新增 tests。
- migration/compatibility行為。
- fault injection結果。
- 實際執行的 build/test/vet/lint commands。
- 未切換的legacy path與後續PR編號。

若遇到以下任一情況，停止實作並先修正設計：

- 新 path繞過 policy gate、receipt、verification、completion gate或event commit。
- reducer無法區分success、partial、blocked、cancelled與verification failure。
- protocol repair或resume可能重播external side effect。
- external provider需要直接信任它自報的tool/receipt/acceptance。
- 同一資料同時存在兩個authoritative writer且無shadow parity/migration方案。

## 14. 完成後的邊界

```text
Coordinator
  ├── RuntimeServices
  │     ├── EventJournal
  │     ├── SessionStore (projection)
  │     ├── PolicyEngine
  │     ├── ContextCompiler
  │     ├── ToolResolver
  │     ├── ModelRuntime
  │     ├── SubagentRegistry
  │     └── ExperienceProcessor
  ├── task contract / scheduling
  ├── retry / recovery / reconciliation
  ├── verification / acceptance / completion gate
  └── projection notifications

EventStore
  └── session/task/run replay

context.sqlite
  ├── canonical context content
  ├── experience aggregates
  └── review-gated promotion proposals

SubagentProvider
  └── one attempt execution result（不能接受task）
```

完成標準不是「Coordinator不知道任何 implementation」，而是以下更可驗證的界線成立：

- Coordinator不直接組裝worker tools或呼叫Fantasy stream。
- Provider不擁有retry、verification、recovery、acceptance與memory promotion。
- 每個runtime transition先有durable event，再有projection。
- 每個learning/promotion結果都能追溯到run、task、event與evidence manifest。
- 未經人工批准的proposal永遠不能修改skill或agent-team文件。
