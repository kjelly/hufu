# Hufu Outcome-driven Memory（L3／L4）實作規格

> 此文件位於 `docs/`，作為 L3/L4 的實作規格與驗收基準。

> 基準程式碼：`c9cb0a8f2f17fd491102c73a55869304da29ca24`
>
> 文件性質：依目前程式碼整理的下一階段實作計畫，不重做已完成的 HF-MEM3 canonical memory cutover。
>
> 核心原則：`RunEvent` 保存執行與使用結果的原始事實，`ContextItem` 保存可供模型使用的知識，experience aggregate 只是可由事件重建的排序投影。

## 1. 目標與完成定義

本計畫讓 Hufu 從「可保存、召回、審核記憶」進一步具備下列能力：

| 等級 | 完成能力 | 明確不包含 |
| --- | --- | --- |
| L3：outcome-driven reinforcement | 只有實際採用且具可歸因 outcome 的記憶會影響 utility、排序與 lifecycle；每次變動都可 replay、解釋與稽核 | 不修改模型權重；不因 retrieval、文字相似或 agent 自評就直接加分 |
| L4：consolidation and policy optimization | 從多筆已驗證經驗提出一般化 ContextItem、memory policy 或 Skill 候選，經固定 benchmark、review、promotion 與 rollback gate 後採用 | 不讓 agent 直接修改 production policy、發布 Skill、合併 PR 或對有副作用任務做線上探索 |

L3 完成點是 `HF-MEM4-005`；L4 完成點是 `HF-MEM4-008`。

## 2. 已驗證的程式碼現況

以下是本計畫的起點，不列為待實作功能：

| 能力 | 現況與 owner | 本計畫處置 |
| --- | --- | --- |
| Canonical store | `internal/context` 的 `context.sqlite` 已是 coordinator 建立時的必要元件 | 沿用；不新增第二套 LTM 或 experience DB |
| Canonical prompt path | normal DAG worker、direct-agent、coordinator 都由 `ContextCompiler` 產生 model-visible prompt | 沿用；不再設計 legacy/shadow/canonical mode |
| Stable memory identity | compiler 已把 canonical item render 為 `<!-- hufu-context ... id=context:<id> -->` | 沿用 `ContextItem.ID`，新增 retrieval manifest 綁定，不另造 ExperienceID |
| Shared/private lifecycle | shared/private、session/persistent、candidate/confirmed/rejected、superseded/expired 的隔離及 promotion gate 已存在 | reinforcement 只能在既有 lifecycle API 上提出 transition，不能直接改 row |
| Retrieval | private worker memory 已使用 `HybridRetrieve`；shared canonical bundle 仍以 projection query 取回，再由 compiler 排序與 token budget 篩選 | L3 將 shared persistent recall 收斂到 query-based retrieval；先 shadow，後 opt-in |
| Typed result | `TaskResult` 已有 evidence、commands、verification、decisions、findings、risks、receipt IDs | 擴充 `MemoryUses`，不要求 agent 額外呼叫另一個 attribution tool |
| Execution truth | hash-chained `RunEvent` 已有 run/session/branch/task/attempt/actor/payload/idempotency key | 作 outcome ledger；不把 aggregate 當原始事實 |
| Event durability | `EventStore.Append()` 在 write 後呼叫 `Sync()`，但目前忽略 `Sync()` 錯誤 | `HF-MEM4-000` 先修復，否則 learning evidence 不能宣稱 durable |
| Idempotency | task transition由 coordinator 記憶 key；`EventStore` 本身不保證所有 key 唯一 | 新 memory events 必須走通用 `emitEventOnce`，reducer 仍以 processed key 防重 |
| Maintenance CLI | 已有 `hufu context query/list/show/candidates/history/consolidate/explain/confirm/reject/supersede/repair/rebuild` | 擴充 `hufu context`；不新增重疊的 `hufu memory` namespace |
| Experiment | `internal/improve` 已有 immutable baseline/candidate snapshot、benchmark、compare gate 與 human-review eligibility | 擴充 memory metrics/gates；不重建 experiment framework |

已知註解債務也要在第一批 PR 修正：`internal/context/model.go` 與 `Coordinator.contextRepo` 仍有「shadow store／never read by prompt assembly」的歷史註解，與目前 authoritative read path 不符。

## 3. 不可破壞的 runtime invariant

1. `RunEvent`、Todo transition、typed `TaskResult`、receipt、verification、acceptance 與 rollback 才能證明執行結果；模型文字不能。
2. `ContextItem` 是 knowledge truth；aggregate、Markdown、FTS、vector 與報表皆為可重建 projection/index。
3. Retrieved 不等於 used。只被選入 prompt 的 item 只能增加 exposure。
4. Used 不等於 causal。未知原因的成功或失敗不可完整歸功／歸咎於 memory。
5. 每個 outcome signal 的總 credit 有上限，不能因一次任務引用十筆 memory 而產生十倍 reward。
6. candidate、rejected、superseded、expired item 不得因 aggregate 高分重新進入 runtime recall。
7. reinforcement 不得繞過 scope、authority、trust、branch lineage、unattended policy 或 `CompletionGate`。
8. replay、resume、retry、fast-path upgrade、direct-agent 與 normal DAG 對同一 observation 必須得到相同 aggregate。
9. side effect 已完成後，learning/reducer 修復不得重跑 worker、tool、acceptance 或 rollback；只能重播事件與重建 projection。
10. telemetry/event payload 只保存 ID、reason code、score、policy version 與 opaque evidence ref；不得保存 memory content、prompt、tool arguments、secret 或完整輸出。

## 4. 目標資料流

```text
ContextItem (confirmed + scope-authorized)
             │
             ▼
Retrieval / compiler selection
  retrieval_id + selected ContextItem IDs + score components + policy version
             │
             ▼
Attempt-scoped MemoryInjectionManifest
             │
             ▼
TaskResult.MemoryUses
  applied / consulted / rejected
             │
             ▼
Objective runtime signals
  task transition / verification / skeptic / acceptance / retry / rollback
             │
             ▼
RunEvent memory_* events (append-only source truth)
             │
             ▼
Experience reducer (idempotent replay)
             │
             ▼
context.sqlite experience aggregate projection
             │
             ├── explain / outcomes / improve metrics
             └── second-stage reranker（shadow → active）
```

## 5. Canonical contracts

### 5.1 MemoryInjectionManifest

manifest 由 runtime 建立，agent 不可自行提供或修改：

```go
type MemoryInjectionManifest struct {
    RetrievalID  string
    RunID        string
    TaskID       string
    Attempt      int
    Agent        string
    PolicyVersion string
    Items        []MemoryInjectionItem
    Fingerprint  string
    CreatedAt    time.Time
}

type MemoryInjectionItem struct {
    ContextItemID string
    Source        string // shared_session, shared_persistent, worker_session, worker_persistent
    Rank          int
    BaseScore     float64
    FinalScore    float64
    ScoreParts    MemoryScoreParts
}
```

要求：

- manifest 只包含實際進入 `CompiledContext.IncludedItems` 的 canonical IDs；omitted item 不算 exposure。
- normal DAG、direct-agent 必須建立 manifest；coordinator context 先只記 exposure，不做 credit assignment。
- retry 若重用完全相同 prompt，沿用 retrieval ID，但每個 attempt 仍發出獨立、具 idempotency key 的 exposure event。
- manifest fingerprint 必須涵蓋有序 item IDs、policy version、task、agent 與 run；不得涵蓋原文。
- manifest 要綁定 attempt execution receipt，不能只存在記憶體；crash-resume 後仍可驗證 `MemoryUses`。

### 5.2 TaskResult.MemoryUses

```go
type MemoryUseRef struct {
    RetrievalID  string  `json:"retrieval_id"`
    ContextItemID string `json:"context_item_id"`
    Disposition  string  `json:"disposition"` // applied, consulted, rejected
    ReasonCode   string  `json:"reason_code,omitempty"`
    Confidence   float64 `json:"confidence"`
}

type TaskResult struct {
    // existing fields...
    MemoryUses []MemoryUseRef `json:"memory_uses,omitempty"`
}
```

runtime validation：

- `RetrievalID` 與 `ContextItemID` 必須存在於該 task/attempt 的 injection manifest。
- 同一 item 只能出現一次；disposition 必須是 enum；confidence 必須在 `[0,1]`。
- `applied` 表示做法或判斷實際影響執行；`consulted` 只記觀察；`rejected` 表示 agent 判定不適用或衝突。
- 未回報的 injected item 視為 exposure only，不推測 applied。
- free-text fallback 不產生 applied attribution。
- strict/`submit_result` path 若有 memory 注入，schema 必須公開 `memory_uses`；空陣列合法，避免逼 agent 虛構採用。
- runtime-owned manifest、receipt、outcome 與 causal confidence 不可由 model input 覆寫。

### 5.3 RunEvent payload

新增事件：

```text
memory_retrieved
memory_usage_recorded
memory_outcome_recorded
memory_aggregate_rebuilt
memory_policy_evaluated
memory_consolidation_proposed
```

不另建 `memory_status_changed` 真相；candidate/confirmed/rejected/superseded 仍由既有 context lifecycle event 表示。

所有新事件使用同一 metadata envelope；不適用的欄位省略，不得填入假 ID：

```text
schema_version
retrieval_id（retrieval/usage/outcome event）
context_item_id（逐 item event）
run_id / task_id / attempt / branch_id / actor
policy_version
reason_code
evidence_ref（opaque ID）
idempotency_key
```

retrieval／usage／outcome 的唯一鍵格式：

```text
memory:<signal>:<run_id>:<task_id>:<attempt>:<retrieval_id>:<context_item_id>
```

### 5.4 Outcome 與 attribution 規則

| Signal | 是否更新 utility | 初始 evidence weight |
| --- | ---: | ---: |
| injected/retrieved | 否，只增加 exposure | 0 |
| consulted | 否，只增加 consulted count | 0 |
| rejected，無客觀證據 | 否，只記 reason | 0 |
| applied + task terminal success，無 verification | 可記弱正向 | 0.2 |
| applied + objective verification passed | 正向 | 1.0 |
| applied + skeptic passed | 正向附加訊號 | 0.8 |
| applied + run acceptance passed | 正向；每個 task 的總 credit 仍受上限限制 | 1.0 |
| task failed，但 failure class 與 memory 無確定關係 | 否 | 0 |
| applied + objective verification failure + deterministic action match | 負向 | 1.0 |
| rollback，且 receipt 能證明採用的 action 造成失敗 | 負向 | 1.0 |
| agent 自稱 memory 有害，無 objective evidence | 只記 observation | 0.2，且不可單獨觸發 rejected |

有效權重：

```text
effective_weight = evidence_weight × attribution_confidence × causal_confidence
```

V1 的保守限制：

- positive attribution 可由 explicit `applied` + objective pass 建立。
- negative attribution 必須同時有 explicit `applied`、objective failure 與 deterministic action/evidence match；缺一即 `causal_confidence=unknown`，不扣 utility。
- V1 的 deterministic action match 僅接受 confirmed procedural item 的 `metadata["action_fingerprint"]`，且必須等於 runtime 從 typed `CommandResult`／`ExecutionReceipt` 正規化出的 fingerprint；未帶 fingerprint 的既有 item 不做自動負向歸因。
- agent prose similarity 不能建立 action match。
- 每個 outcome signal 的 `sum(effective_weight)` 上限為 `1.0`，依各 item attribution confidence 正規化分配。

### 5.5 Aggregate projection

不保存單一不可重算 reward。`context.sqlite` 新增可重建 projection tables：

```text
experience_aggregates
experience_processed_events
memory_policy_versions
consolidation_proposals       # HF-MEM4-006 才加入
```

`experience_aggregates` 最少欄位：

```go
type ExperienceAggregate struct {
    ContextItemID          string
    PolicyVersion          string
    PositiveWeight         float64
    NegativeWeight         float64
    ExposureCount          int
    ConsultedCount         int
    AppliedCount           int
    RejectedCount          int
    VerifiedSupportCount   int
    CausalFailureCount     int
    IndependentTaskCount   int
    IndependentProjectCount int
    UtilityLowerBound      float64
    LastObservedAt         time.Time
    Revision               int64
}
```

`experience_processed_events.idempotency_key` 是 reducer 防重依據。aggregate 必須能從 `event_store.jsonl` 清空後完整重建；不得依賴 `execution-events.jsonl`，後者只供 `hufu improve` 的 metadata report。

Utility 採 weighted Beta posterior 的保守下界：

```text
alpha = prior_alpha + positive_weight
beta  = prior_beta  + negative_weight
utility = Beta(alpha, beta) 的第 10 百分位
```

同分 tie-break 固定為：base relevance、scope distance、priority、confidence、updated_at、ContextItem ID，確保 replay 與 benchmark deterministic。

## 6. Runtime 設定與 rollout

新增 team-level config，預設完全不改排序：

```yaml
memory-learning:
  mode: off                # off | observe | shadow | active
  policy-version: memory-policy-v1
  prior-alpha: 1.0
  prior-beta: 1.0
  utility-percentile: 0.10
  max-credit-per-signal: 1.0
  min-confirmed-support: 2
  min-independent-tasks: 2
  max-harm-rate: 0.0
```

模式語意：

| mode | 行為 |
| --- | --- |
| `off` | 不建立 manifest，不發 memory observation event，不計 aggregate |
| `observe` | 建 manifest、驗證 `MemoryUses`、寫 events/aggregate，但不計算新 ranking |
| `shadow` | 同時計算 base 與 reinforced ranking，保存差異，不改 prompt selection |
| `active` | reinforced ranking 可改變 prompt selection；仍受 lifecycle/scope/authority/token budget gate |

設定 precedence 沿用現有規則：CLI explicit override（若日後加入）> profile > team config > default。第一版不必增加 CLI flag，以 team config 降低 surface area。

## 7. 工作包與 PR 拆分

### HF-MEM4-000 — Baseline、durability 與契約測試

**Priority：P0；行為改動前置。**

修改面：

- `internal/team/event_store.go`
- `internal/team/coordinator_eventstore.go`
- `internal/context/model.go`
- `internal/team/coordinator.go`
- 對應 tests

工作：

1. `EventStore.Append()` 必須回傳 `Sync()` 錯誤，不可靜默成功。
2. sync 結果不確定時將 store 標記 degraded；後續 append 前先 reopen/rescan hash chain，不可在未知 last hash 上繼續。
3. 新增通用 `emitEventOnce(idempotencyKey, ...)`，啟動時由既有 events hydrate；memory events 禁止各自維護另一個防重 map。
4. event append 失敗不回滾已完成 task，也不重跑 side effect；改記 learning gap、dual-write failure 與 pending reducer repair。
5. 修正 canonical/shadow 過時註解。
6. 建立本文件第 10 節前六項 failing contract tests。

驗收：sync failure 可被測試注入且不被吞掉；reopen 後 hash chain 有效；同 key 不產生第二筆事件。

### HF-MEM4-001 — Retrieval manifest 與 exposure telemetry

**Priority：P0；只觀察，不改 ranking。依賴 000。**

修改面：

- `internal/team/context_compiler.go`
- `internal/team/worker_memory.go`
- `internal/team/coordinator_task_run.go`
- `internal/team/coordinator_run.go`
- `internal/team/status.go`
- `internal/team/session.go`／receipt persistence

工作：

1. `CompiledContext` 保留每個 canonical included item 的 base score/source/rank。
2. normal DAG 與 direct-agent 在 model call 前建立 attempt-scoped manifest。
3. worker private recall trace 與 shared canonical items 合併為同一 manifest，不可重複 ID。
4. `memory_retrieved` 逐 item 發 event，只保存 metadata。
5. manifest 隨 receipt/session checkpoint 持久化並可在 resume/replay 還原。
6. coordinator、sidecar、judge、skeptic 先只計 exposure；本 PR 不做 usage attribution。

驗收：同一 prompt 重試與 crash-resume 不重複 exposure；未進 token budget 的 item 不算 retrieved。

### HF-MEM4-002 — TaskResult attribution 與 runtime validation

**Priority：P0。依賴 001。**

修改面：

- `internal/team/task_result.go`
- `internal/team/coordinator_tools_result.go`
- `internal/team/coordinator_step_receipts.go`
- `internal/team/event_reducers.go`
- task/session/report projection tests

工作：

1. 加入 `MemoryUseRef` 與 `TaskResult.MemoryUses`。
2. 更新 `submit_result` JSON schema、parser、validation、format/context projection。
3. 僅接受 manifest 中的 ID；拒絕跨 task、跨 attempt、跨 worker 或 lifecycle 已失效的 claim。
4. 將 validated claim 寫入 typed result/receipt，發 `memory_usage_recorded`。
5. free-text path 明確標記 attribution unavailable，不做語意猜測。
6. event/session replay 必須完整還原 `MemoryUses`。

驗收：偽造 ID、重複 ID、錯誤 retrieval ID 與越權 scope 均 fail closed；空陣列不影響既有 worker。

### HF-MEM4-003 — Outcome reducer 與 aggregate rebuild

**Priority：P0；完成後仍不改 retrieval。依賴 002。**

修改面：

- `internal/team/memory_outcome.go`（新增，runtime signal mapping）
- `internal/team/event_reducers.go`
- `internal/context/experience.go`（新增 projection model/repository operations）
- `internal/context/sqlite_repository.go`（append-only migration）
- completion、verification、skeptic、rollback call sites

工作：

1. 在 task terminal、verification、skeptic、completion gate、acceptance、retry rescue、rollback 後發 `memory_outcome_recorded`。
2. 以 structured failure class、verification result、receipt 與 manifest 計算 attribution/causal confidence；禁止文字相似 fallback。
3. reducer 以 processed idempotency key 原子更新 aggregate。
4. 提供 `RebuildExperienceAggregates(events []RunEvent)`；先建新 projection，再 transaction swap，失敗保留舊 projection。
5. aggregate failure 不改 task outcome；留下可重播錯誤與 degraded learning 狀態。
6. secret redaction、workspace scope、branch/run/task identity 必須測試。

驗收：同 events replay N 次結果相同；刪除 aggregate 後可完全重建；未知失敗不扣分。

### HF-MEM4-004 — Reinforced reranker 與 lifecycle policy

**Priority：P1。依賴 003。**

修改面：

- `internal/context/retrieval.go`
- `internal/team/context_shadow.go`
- `internal/team/context_compiler.go`
- `internal/team/worker_memory.go`
- config parse/validation

工作：

1. shared persistent memory 由「全量 projection + compiler budget」改為依目前 goal 的 `HybridRetrieve`；shared session operational context 維持 projection query。
2. base relevance 仍由 exact/lexical/vector/RRF/MMR 負責；utility 只做第二階段 rerank。
3. `shadow` 保存 base/new rank、score parts、selected IDs 與 policy version，不保存內容。
4. `active` 才改變 selection；低 relevance 不得被高 utility 反轉。
5. lifecycle 建議由 deterministic policy 產生 proposal，再走既有 confirm/reject/supersede API；不得直接 SQL update。

V1 score：

```text
final_score =
    base_relevance
  × applicability
  × (0.75 + 0.50 × utility_lower_bound)
  × freshness
  × trust_factor
  - harmful_use_penalty
  - stale_environment_penalty
```

驗收：observe/shadow 的 model-visible prompt 與 base 完全一致；active 下正向 evidence 可提升相近 item，高 utility 無關 item仍不可入選。

### HF-MEM4-005 — Explainability、CLI、improve 與 L3 benchmark

**Priority：P1；L3 gate。依賴 004。**

擴充既有 CLI：

```text
hufu context outcomes <id>
hufu context explain-memory <id> --query <goal>
hufu context rebuild --aggregates
hufu context doctor --learning
```

`explain-memory` 至少輸出 base relevance、scope/applicability、utility lower bound、freshness、trust、harm penalty、final score、support/failure counts、policy version 與 retrieval ID；預設不顯示原文。

擴充 `internal/improve.Metrics`：

```text
memory_retrieval_count
memory_exposure_count
memory_applied_count
memory_attribution_coverage
memory_verified_assist_rate
memory_harmful_use_rate
memory_stale_retrieval_rate
memory_token_overhead
memory_assisted_retry_rate
memory_unassisted_retry_rate
```

L3 benchmark 固定三組：positive transfer、irrelevant high-utility memory、stale/harmful memory。通過條件：

- retrieved-only item 的 positive/negative weight 均為 0。
- replay/rebuild deterministic。
- verified applied memory 排名可觀察地上升。
- causal verification failure 排名下降；unknown failure 不下降。
- rejected/superseded/expired item 永不注入。
- active completion/error 不劣於 base；harmful memory rate 必須為 0。
- memory token overhead 相對 baseline 不超過 10%。
- CLI、JSON output、Markdown report、TUI status、execution telemetry 對同一 policy/version/count 一致。

全部成立才可宣稱 L3。

### HF-MEM4-006 — Consolidation proposal

**Priority：P1；L4 第一階段。依賴 L3。**

擴充現有 `hufu context consolidate`，預設 dry-run：

```text
hufu context consolidate --project <id> [--apply-proposal]
hufu context consolidation show <proposal-id>
hufu context consolidation approve <proposal-id>
hufu context consolidation reject <proposal-id>
```

pipeline：deterministic pre-cluster（scope、kind、tool/action signature、file evidence、semantic similarity）→ LLM proposal（只產候選文字）→ source coverage／contradiction／authority／secret／scope widening／benchmark validation → candidate `ContextItem`。

規則：

- proposal 必須列出所有 source ContextItem IDs 與 aggregate revision。
- 原始 item/event 永不刪除；以 `derived_from/supports/contradicts/supersedes/applies_to/failed_with` edge 連結。
- 單一 task 不得自動升 project；跨 project/team/global scope 必須人工批准。
- LLM 不能直接建立 confirmed item，也不能直接 supersede source。

### HF-MEM4-007 — Versioned memory policy experiment

**Priority：P1。依賴 006 與既有 `hufu improve`。**

工作：

1. policy snapshot 包含 retrieval、reinforcement、attribution、consolidation 參數及 revision hash。
2. 擴充既有 baseline/candidate compare，不另建 A/B framework。
3. 每次 candidate 只能改一類變數；若 model、team prompt 或 skills 同時改變，experiment 無效。
4. 新 gates：harmful rate=0、attribution coverage 達門檻、completion/error/retry 不退步、token overhead 不超限、stale rate 不上升。
5. passing candidate 只成為 `eligible_for_review`；採用需 explicit approval，並保留前一版以供 rollback。

### HF-MEM4-008 — Safe optimizer 與 Skill proposal

**Priority：P2；L4 gate。依賴 007。**

optimizer 只產 candidate，可調整 top-k、utility/freshness weight、threshold、memory prompt 排版與適用 agent category。

controlled exploration 只允許 read-only、sandbox、無 SSH/sudo/deployment/irreversible side effect 的 task；任何 `SideEffectClass != none` 或 recovery 不可自動化時 exploration 固定為 0。

穩定 procedural experience 可產生 `.agents/skills/<name>/SKILL.md` proposal，但必須走 candidate snapshot → benchmark → gates → PR/review → adoption → monitoring/rollback。不得呼叫自動發布或自動合併流程。

全部成立才可宣稱安全、可稽核的 L4。

## 8. 執行路徑覆蓋矩陣

| 路徑 | Retrieval manifest | Usage attribution | Outcome | Ranking |
| --- | --- | --- | --- | --- |
| normal DAG worker | 必須 | `TaskResult` | task/verify/acceptance | shadow/active |
| direct-agent | 必須 | `TaskResult`，無 submit 時 unavailable | task/verify/run gate | shadow/active |
| retry/resume | 還原或 deterministic 重建 | 綁定原 attempt | 不重複計分 | 同 policy revision |
| fast-path upgrade | 以 run/task/attempt/retrieval key 防重 | 不跨 path 借用 claim | 不重複 outcome | 一致 |
| extra-model/judge | exposure only（V1） | 不支援 | 不計 credit | base only |
| skeptic | 不採用 memory | 不支援 | 作客觀 outcome signal | 不適用 |
| coordinator | exposure only（V1） | 不支援 | 不計 credit | shadow only |
| unattended | 同 normal | 禁止 scope spoofing | acceptance/rollback 可作 signal | active 仍受 policy gate |
| dry-run | 只產 deterministic ranking/trace | 無 | 無 | 不寫 events/aggregate |

## 9. 失敗與 recovery 語意

| 失敗 | runtime 結果 | learning 結果 | 修復方式 |
| --- | --- | --- | --- |
| manifest 無法持久化 | model call 前 fail preflight，避免無法驗證 attribution | 不產 exposure | 修 store 後重試 task，尚未有 side effect |
| usage event append 失敗 | 不撤銷已完成 worker | claim 留在 receipt/session，但不計分 | repair/replay event，不重跑 worker |
| outcome event append 失敗 | task/acceptance outcome 保持 authoritative | 不更新 aggregate並標 learning gap | 由既有 task/receipt/manifest deterministic 補事件 |
| aggregate update 失敗 | 不影響 task | 舊 ranking projection繼續使用並標 degraded | rebuild aggregates |
| shadow ranker 失敗 | 使用 base ranking | 記 telemetry | 修 policy/reducer |
| active ranker 失敗 | fail closed 回 base ranking並顯式標 degraded；不得靜默宣稱 active | 不寫虛假 policy success | doctor/rebuild |
| consolidation/optimizer 失敗 | production policy/context 不變 | proposal failed | 修 candidate，禁止自動 retry side effect |

## 10. 最低測試矩陣

```text
TestEventStoreSyncFailureIsObservable
TestEmitEventOnceSurvivesRestart
TestOnlyCompilerIncludedItemsBecomeExposures
TestMemoryManifestSurvivesSessionReplay
TestRetrievedMemoryDoesNotReceiveReward
TestUnknownMemoryIDFailsClosed
TestMemoryUseCannotCrossTaskOrAttempt
TestFreeTextResultCannotClaimAppliedMemory
TestExplicitAppliedMemoryReceivesVerifiedCredit
TestUnknownFailureDoesNotPenalizeMemory
TestCausalVerificationFailureDemotesMemory
TestOutcomeCreditIsCappedAndNormalized
TestOutcomeReducerIsIdempotent
TestAggregateCanRebuildFromRunEvents
TestResumeDoesNotDoubleCountOutcome
TestFastPathUpgradeDoesNotDoubleCountOutcome
TestUtilityCannotOverrideLowRelevance
TestCandidateExpiredSupersededRejectedAreNeverInjected
TestUntrustedMemoryCannotOverrideNormativeContext
TestShadowModeDoesNotChangePrompt
TestActiveRankingIsDeterministic
TestConsolidationPreservesSourceEvidence
TestContradictoryExperiencesAreNotMerged
TestCrossProjectWideningRequiresReview
TestMemoryPolicyCandidateCannotAutoAdopt
TestSideEffectTaskDisablesExploration
TestProductionRegressionKeepsPreviousPolicyAvailable
TestMemoryEventsAndReportsRedactContentAndSecrets
```

每個 runtime PR 必須各自執行：

```bash
go test ./...
go vet ./...
golangci-lint run
```

不得以 live Ollama、外部 API、Pilot checkout、真實 GitHub PR 或真實 infra mutation作為 unit/integration test 必要條件。

## 11. 建議交付順序

```text
PR 1  HF-MEM4-000  durability + baseline contracts
PR 2  HF-MEM4-001  retrieval manifest + exposure events
PR 3  HF-MEM4-002  TaskResult.MemoryUses + validation/replay
PR 4  HF-MEM4-003  outcome reducer + aggregate rebuild
PR 5  HF-MEM4-004  shadow/active reranker
PR 6  HF-MEM4-005  CLI + improve + L3 benchmark
PR 7  HF-MEM4-006  consolidation proposals
PR 8  HF-MEM4-007  policy experiments/promotion/rollback
PR 9  HF-MEM4-008  safe optimizer + Skill proposals
```

PR 1–4 不改 model-visible memory selection；PR 5 先 shadow，只有通過 PR 6 的 L3 gate 才允許 active。L4 的所有自動化都止於 proposal/candidate，production adoption 保持人工或明確 policy gate。

## 12. 明確非目標

- 不保存或 replay 完整 worker conversation。
- 不讓 memory 取代 dependency result、artifact store、receipt、verification、acceptance 或 rollback truth。
- 不建立 remote memory service、多使用者 ACL、加密系統或新的 graph database。
- 不讓 model 指定任意 run/task/attempt/agent/branch identity。
- 不把 Hufu core 寫成 Pilot 或其他 consumer-specific workflow。
- 不在 V1 用 contextual bandit/Thompson sampling；先證明 deterministic attribution/replay 正確。
- 不從歷史成功狀態反推未曾記錄的 memory usage。
- 不因 aggregate 或 benchmark passing 自動修改 team、Skill、policy 或 Git repository。

## 13. 最終驗收

L3：同一組 event 可重建相同 aggregate 與 ranking；只有 manifest 中實際 applied 且有客觀 outcome 的 memory 得到 credit；unknown failure 不誤傷；每個 score 可解釋；active benchmark 不退步且 harmful rate 為 0。

L4：多筆已驗證經驗可產生保留 provenance 的 consolidation/policy/Skill proposal；candidate 經固定 experiment gates；production 採用需要明確批准並可 rollback；任何有副作用任務都不做線上 exploration。
