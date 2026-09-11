# Hufu Decision-Aware Runtime Specification

> Status: active
> Authority: normative
> Verified-Commit: `6ab9951`
> Supersedes: the former decision-runtime drafts and the root `spec.md` decision draft
> Superseded-By: —

**Implementation status:** V1 phases 0–3.5 are wired to dispatch. Capability-aware
REFERENCE/JUDGE/CHALLENGE/REVISE routing, diversity reporting, and pinned
bindings are also implemented. Outcome learning/calibration remains deferred
until its documented evidence threshold is met.

**Target:** `github.com/kjelly/hufu`
**Audience:** Coding agents / maintainers
**Language:** English identifiers and API names; explanatory text in Traditional Chinese.

> **關於檔名**：`spec.md` 這個檔名在本 repo 被重複覆寫過多次，且
> `internal/improve/sqlite_analytics_error.go`、`internal/auditverify/archive.go`、
> `internal/tools/javascript.go` 等處的註解仍以「spec.md §N」指向**不同的舊規格**。
> 本規格一律以本檔名 `docs/architecture/decision-runtime.md` 被引用；
> 新增程式碼註解必須引用本檔名，不得再寫 bare `spec.md`。

---

## 0. 2026-09-05 修訂備註

本文件由 2940 行草稿修訂而成。草稿的職責切分是正確的，但有三類問題會使
coding agent 在實作中期卡死：假設了不存在的元件、關鍵門檻缺少可判定定義、
以及與 repo 既有 invariant 衝突。以下為對草稿的**實質修改**清單：

| # | 修訂項 | 原因 |
|---|---|---|
| 1 | `TaskDef.DecisionProfile` 改為 `json:"-"` | 草稿的 `json:"decision_profile"` 讓 coordinator（LLM）可自行把 `high-stakes` 降成 `off`。與 `Phase`／`Action` 既有的 `json:"-"` 保護模式一致（`internal/team/coordinator.go:85-91`） |
| 2 | 新增 §14「數值語意」 | 草稿的分數尺度、權重正規化、缺值、NaN、四捨五入、`dispersion-above` 單位全未定義，導致「deterministic aggregation」實際不可判定 |
| 3 | §15 定義 material evidence 欄位集合 | 草稿以「evidence 有變 → 全部 stale」實作會造成 replan 風暴；現在只有能改變判斷的欄位進入 hash |
| 4 | §28 證據獨立性 V1 降為 advisory | runtime 無法推導引用血緣；草稿同時要求「不信任自述」又要 LLM 宣告 parent，自相矛盾。V1 只做可推導的分組 + 警示 |
| 5 | §29 定義 checkpoint 單位與可計算的 kill criterion | 草稿未定義 checkpoint 是每個 tool call 還是每個 step；`expected_value` 無法計算，已移除 |
| 6 | 新增 Phase 0.5「Budget ownership extraction」 | 草稿的硬性 invariant「counters 由 BudgetManager 擁有」在現況不成立（只有 `Coordinator.budgetExceeded()`），且草稿又禁止無關重構，自相矛盾 |
| 7 | §30 commit gate 的每個 `require-*` 給出可判定條件與掛載點 | 草稿的 `require-observability` 無定義；`require-rollback` 在現有 `RecoveryPolicy` 沒有對應值 |
| 8 | §34 允許 `budget-degradation: explicit` | 草稿一律 fail closed，`high-stakes` 單一決策 8–12 次 LLM 呼叫，CLI 情境會大量硬失敗。降級仍永不靜默 |
| 9 | §42 套件配置改為 `internal/team/decision_*.go` | 草稿的 `decision/`、`policy/`、`routing/` 會與 `internal/team`（432 檔／16 萬行，持有 `TaskDef`、`EventStore`、`ArtifactStore`）循環相依 |
| 10 | Phase 4 routing and Phase 5 calibration were originally separated from V1 | Phase 4 is now implemented through the capability registry and role runners; Phase 5 remains data-gated because outcome resolution/calibration needs enough verified samples. |
| 11 | §4 新增「已驗證的現況基線」 | 草稿的 §3 是宣稱，本節逐項給出 file:line 依據，並列出**不存在**的元件 |
| 12 | 修正不存在的型別名 | 草稿的 `AgentRuntime` 型別不存在；`DecisionServices` 改對映實際介面（`EventJournal`、`ArtifactStore`、`ContextCompiler`、`AgentPool`） |

---

## 1. 執行摘要

Hufu 由「多代理任務編排器」演進為：

> **Decision-aware, policy-enforced, evidence-driven execution runtime**

責任邊界：

```text
LLM / Agent:
  understands objective
  proposes options
  produces judgment
  discovers risk
  explains trade-offs
  performs authorized work

Runtime:
  enforces information isolation
  seals evidence
  evaluates deterministic gates
  reads budget counters
  aggregates structured scores
  preserves no-go alternatives
  tracks assumptions
  gates side effects
  stops / replans by policy
  persists provenance
  performs recovery / replay

Scheduler / AgentPool:
  routes work
  enforces authorization
  owns task execution lifecycle
```

治理原則：

> **Runtime owns procedure; agents own judgment.**

執行模型：

> **Plan is a hypothesis. Commitment is conditional. Evidence can invalidate both.**

---

## 2. 目標

本規格引入兩個 V1 runtime 抽象，並由既有 capability registry 提供
REFERENCE/JUDGE/CHALLENGE/REVISE 的 capability-aware routing：

```text
DecisionEngine
    └── How should Hufu form a decision?

DisciplinePolicy
    └── Is Hufu allowed to commit or continue?
```

主要目標：

1. 讓決策形成成為 runtime 擁有的程序，而非 coordinator prompt 慣例。
2. 以資訊流隔離保全獨立判斷。
3. 數值聚合完全 deterministic。
4. 在配置要求時，強制 alternatives、assumptions、evidence、stop conditions。
5. 未滿足 commit 前提時，阻擋有副作用的執行。
6. 偵測假設或證據變動導致決策過期。
7. 支援 deterministic 的 replan / stop。
8. 持久化決策來源與執行理由。
9. 保留 crash / resume / replay 語意。
10. 舊 team 設定零行為變動。
11. 決策品質與結果品質分開看待。

---

## 3. 非目標

第一版**不得**：

- 建立第二個 team 層 DAG；
- 取代既有 scheduler；
- 取代 `TaskDef`；
- 取代 `PREPARE → AUDIT → EXECUTE → VERIFY`；
- 在 `team.yaml` 建立第二個 workflow engine；
- 把決策邏輯完全寫在 coordinator prompt；
- 建立十個 bias/strategy 專家 agent；
- 實作「Sun Tzu mode」、「Art of War mode」等書名化的 runtime 名稱；
- 讓 LLM 做算術聚合；
- 讓 LLM 擁有資源計數器；
- 讓 LLM 直接決定授權；
- 取代 `adversarial_verify`；
- 把 consensus 當成 verification 證據；
- 把 Markdown 報告當成 canonical runtime state；
- **靜默**降低已配置的決策嚴謹度（明示降級見 §34）；
- 由極小樣本自動推導專家權重；
- 自動把未驗證的軼事升級進 LTM；
- 沒有明確 policy 邊界就把任意任務歸類為 high-stakes；
- 改寫既有的 side-effect recovery 語意。

---

## 4. 已驗證的現況基線

以下為 2026-09-05 對 `internal/team`、`internal/agent` 的實際查核結果。
實作者可直接依賴，不需重新確認。

### 4.1 已存在，必須沿用（不得重建）

| 能力 | 位置 |
|---|---|
| `TaskDef`（`DependsOn`／`Pipeline`／`Verify`／`VerifySpec`／`AdversarialVerify`／`SideEffect`／`Recovery`／`ReconcileTool`／`Execution`／`Phase`／`FactRefs`／`FanOut`／`MaxRetries`／`OnFailure`／`Escalate`） | `internal/team/coordinator.go:79` |
| workflow phases + 合法轉移表 | `internal/team/phase.go:12`、`allowedTransitions` |
| `RunEvent`（含 `PreviousHash`／`Hash`／`IdempotencyKey`／`Attempt`）、append-only event store | `internal/team/event_store.go:36,55` |
| `EventJournal` 介面（`Append`／`ReadEvents`／`VerifyHashChain`） | `internal/team/services.go:18` |
| `ArtifactStore`（content-addressed、never overwritten） | `internal/team/evidence_store.go:30` |
| `ArtifactRef`（含 `SHA256`／`MediaType`／`RunID`／`TaskID`／`Attempt`） | `internal/team/task_result.go:18` |
| `EvidenceRequirement`／`EvidenceResult`／`EvidenceBinding`／`EvidenceManifest.Seal()` | `internal/team/evidence_store.go:323,332,345,355` |
| `SideEffectClass`（`none`／`workspace_write`／`external_write`／`infra_mutation`／`credential_mutation`／`unknown`） | `internal/team/recovery.go:10` |
| `RecoveryPolicy`（`retry`／`reconcile`／`manual`／`never`）、`RecoveryState*`、reconcile exit codes | `internal/team/recovery.go:30,45` |
| `ToolRecoverySpec`（`RetrySafe`／`IdempotencyKey`／`ReconcileTool`／`CompensateTool`） | `internal/team/recovery.go:38` |
| 工具呼叫前的 policy gate（commit gate 的掛載點） | `internal/team/tool_policy_gate.go:190`（`policyGatedTool.Run`）、`:545`（`authorizeToolInvocation`） |
| adversarial verification（skeptics）與 judge | `internal/team/coordinator_skeptic.go`、`coordinator_judge.go` |
| protocol repair（結構化輸出修復路徑） | `internal/team/protocol_repair_test.go` 對應實作 |
| no-progress 偵測 | `internal/team/no_progress_test.go` 對應實作 |
| YAML 嚴格解碼（未知欄位不靜默忽略） | `internal/team/parse.go:827,979`（`KnownFields(true)`） |
| secret redaction | `internal/team/workspace_redact.go` |
| `ContextCompiler`／`AgentPool`／`PolicyEngine`／`SessionStore` 介面 | `internal/team/services.go:82,100,71,62` |
| event type 命名慣例為 snake_case（`task_completed`、`artifact_created`、`memory_outcome_recorded`…） | `internal/team/coordinator_eventstore.go` |

**結論**：§35–§38 提出的事件命名慣例與現況相符，不需要額外映射層。

### 4.2 不存在，必須先建立或已延後

| 草稿假設 | 現況 | 處置 |
|---|---|---|
| `BudgetManager` 型別 | 不存在。只有 `Coordinator.budgetExceeded()`（`internal/team/coordinator.go:1009`）與 `BudgetSnapshot`（`internal/team/diagnosis.go:47`） | **Phase 0.5** 明確抽取 |
| `AgentRuntime` 型別 | 不存在（僅出現在測試函式名） | `DecisionServices` 改用 §43 Phase 1 的實際介面 |
| `RequestContract` | 不存在 | Phase 2 新建 |
| capability 記錄／評分／routing | Maintainer-authored capability registry and role runners now resolve authorized candidates for REFERENCE/JUDGE/CHALLENGE/REVISE. Capabilities still never grant tools or authorization. | **Implemented; keep the authorization-before-ranking invariant** |
| outcome resolution 觸發機制 | 不存在。hufu 為 CLI，run 結束即退出 | **延後至 §49 Phase 5** |
| `rollback` recovery policy | 不存在於 `RecoveryPolicy`。補償能力實際由 `ToolRecoverySpec.CompensateTool` 表示 | §30 重新定義 `require-rollback` |

### 4.3 影響實作方式的既有約束

1. `internal/team` 為 432 檔、約 160,913 行的單一套件，且持有 `TaskDef`、
   `EventStore`、`ArtifactStore`。任何放在套件外的決策型別若需引用這些型別
   即產生循環相依 → 見 §42。
2. `CLAUDE.md` 要求單檔 < 800 行。既有 `coordinator_task_run.go` 已 4671 行。
   本規格新增的每個檔案都必須自行守住 800 行 → 見 §42 的檔案切分。
3. `TaskDef` 的 JSON payload 來自 LLM（coordinator 工具參數），`UnmarshalJSON`
   會吃下 payload 中任何有 json tag 的欄位；工具 schema 是手寫的
   （`internal/team/coordinator.go:1613` 附近）但**不構成保護**。凡 coordinator
   不得選擇的欄位一律 `json:"-"`。

---

## 5. V1 範圍

```text
V1 = Phase 0   Typed schema and compatibility foundation
   + Phase 0.5 Budget ownership extraction
   + Phase 1   Minimal decision engine
   + Phase 2   Decision quality gates
   + Phase 3   Execution discipline
   + Phase 3.5 Dispatch integration
   + Capability-aware routing for REFERENCE/JUDGE/CHALLENGE/REVISE

Deferred (見 §49):
     Phase 5  Outcome learning and calibration
```

V1 完成後 runtime 必須能以持久化狀態回答：目標是什麼、考慮過哪些選項、
哪些假設是關鍵、可用證據為何、判斷是否獨立、如何聚合、提出過什麼反論、
為何允許執行、什麼會讓我們停止、什麼使決策失效。

「驗證後的結果教了我們什麼」仍屬 Phase 5，V1 不承諾自動校準或
自動調整 judge 權重；實際 routing binding 與選擇理由會被持久化。

---

## 6. 統一 runtime 架構

### 6.1 高階流程

```text
User Request
    │
    ▼
RequestContract
    │
    ▼
Effective Policy Resolution
    │
    ▼
Existing DAG Scheduler
    │
    ▼
Task
    │
    ├── normal execution
    │
    └── optional DecisionEngine
           │
           ▼
      DecisionRecord
           │
           ▼
      Commit / Discipline Gate
           │
           ▼
        EXECUTE
           │
       checkpoints
           │
   ┌───────┼────────┐
   ▼       ▼        ▼
continue  replan   stop
   │
   ▼
VERIFY
   │
   ▼
Evidence / Acceptance
   │
   ▼
Outcome
```

### 6.2 邏輯平面

四個平面是**推理與維護用的邏輯邊界**，不要求四個新套件（見 §42）。

```text
Execution Plane     : agent 執行實體、tools、workers、model backends
Decision Control    : DecisionEngine, DisciplinePolicy, CommitGate, StopPolicy,
                      ReplanPolicy, EvidenceIndependencePolicy
Scheduling Plane    : DAG Scheduler, AgentPool, authorization, concurrency
Evidence/State      : EventStore, ArtifactStore, DecisionEvidencePacket,
                      DecisionRecord, EvidenceProvenance, typed assumptions
```

---

## 7. Team workflow 與決策子狀態機

Team 層 workflow 不變（`internal/team/phase.go`）：

```text
PREPARE → AUDIT → EXECUTE → VERIFY
```

決策型任務內部有一個**任務區域**的子狀態機：

```text
PREPARE → REFERENCE → JUDGE → AGGREGATE → CHALLENGE → REVISE → FINALIZE
```

coordinator **不得**手工建立 `judge-1`／`judge-2`／`aggregate`／`challenge`
之類的 DAG 節點；那些是 DecisionEngine 的任務區域工作項，不進入 team DAG。

---

## 8. Policy 啟用模型

決策能力是常駐 runtime 能力，決策**嚴謹度**由 profile 決定。

```text
off | light | standard | high-stakes
```

`decision-profile = off` 的意義是：

> 不執行結構化多代理決策形成。

它**不**代表關閉 runtime 正確性或安全性。以下在任何 profile 下都是 invariant：

- authorization；
- budget accounting；
- event integrity（hash chain）；
- side-effect classification；
- recovery / reconcile 語意；
- verification 語意；
- secret redaction；
- artifact / evidence 持久化；
- tool lifecycle safety。

優先序（高者勝）：

```text
CLI / request override      --decision-profile <name>
    >
Task contract override      TaskDef.DecisionProfile（configuration-only）
    >
Team default                decision.default-profile
    >
Runtime built-in default    off
```

最上層必須真的存在，否則優先序只有三層。CLI flag 定義：

```text
--decision-profile <name>   套用於本次 run 的所有 task。
                            值必須是 team 已定義的 profile 或保留字 off，
                            否則 run 在開始前失敗（decision_profile_unknown）。
                            它只能「指定」profile，不能繞過 §9 的保護——
                            coordinator 仍然無法選擇 profile。
```

**相容性**：未宣告 `decision` 區段的舊 `team.yaml`，其 effective profile 為
`off`，行為與升級前完全一致。

---

## 9. TaskDef 整合

決策是一種**執行模式**，不是新的 task kind。

```go
type TaskDef struct {
    // existing fields...

    // DecisionProfile selects the decision rigor profile for this task.
    // It is configuration-only: `json:"-"` keeps it out of the coordinator's
    // task payload so an LLM can never lower configured rigor (same protection
    // as Phase and Action).
    DecisionProfile string `json:"-" yaml:"decision-profile,omitempty"`
}
```

**硬性要求**

1. tag 必須是 `json:"-"`。若 payload 中出現 `decision_profile`，
   解碼後必須為零值，且必須有測試斷言此點。
2. 不得建立 `TaskKindDecision`，除非未來出現無法以執行模式表達的 lifecycle 語意。
3. `DecisionProfile` 必須是 team config 中已定義的 profile 名稱或空字串，
   否則 config 載入即失敗（reason code `decision_profile_unknown`）。

---

## 10. Team 層決策設定

```go
type TeamConfig struct {
    // existing fields...

    Decision DecisionConfig `yaml:"decision,omitempty"`
}

type DecisionConfig struct {
    DefaultProfile string                    `yaml:"default-profile,omitempty"`
    Profiles       map[string]DecisionPolicy `yaml:"profiles,omitempty"`
}
```

沿用 `parse.go` 既有的 `KnownFields(true)`：typed schema 宣稱支援的欄位，
未知鍵一律解碼失敗，不得靜默忽略。

### 10.1 內建 profile（V1 基準值）

```yaml
decision:
  default-profile: off

  profiles:
    light:
      independent-judgments: 2
      min-independent-judgments: 2
      context-isolation: strict
      score-scale: 0-10
      aggregation:
        method: mean-score
      outside-view:
        required: false
      challenge:
        enabled: false
      revision:
        enabled: false
      premortem:
        enabled: false
      forecast:
        required: false
      max-rounds: 1
      budget-degradation: forbidden
      discipline:
        alternatives:
          require-no-action-option: true
          min-options: 2
        stop:
          max-attempts: 2
          require-kill-criteria: false
        evidence:
          required-independent-groups: 0
          warn-shared-origin: true

    standard:
      independent-judgments: 3
      min-independent-judgments: 3
      context-isolation: strict
      score-scale: 0-10
      aggregation:
        method: mean-score
      outside-view:
        required: true
      challenge:
        enabled: true
        count: 1
        trigger:
          dispersion-above: 2.0
      revision:
        enabled: true
      premortem:
        enabled: true
        required-before-commit: false
      forecast:
        required: true
      max-rounds: 2
      budget-degradation: forbidden
      discipline:
        alternatives:
          require-no-action-option: true
          require-information-option: true
          min-options: 3
        stop:
          max-attempts: 3
          checkpoint-every: 1
          require-kill-criteria: true
        commit:
          require-verification: true
          require-evidence: true
        replan:
          on-critical-assumption-contradicted: replan
          on-material-evidence-changed: replan
          on-repeated-failure: replan
        evidence:
          required-independent-groups: 0
          warn-shared-origin: true

    high-stakes:
      independent-judgments: 5
      min-independent-judgments: 3
      context-isolation: sealed
      score-scale: 0-10
      aggregation:
        method: mean-score
      outside-view:
        required: true
      challenge:
        enabled: true
        count: 2
        trigger:
          dispersion-above: 1.5
      revision:
        enabled: true
      premortem:
        enabled: true
        required-before-commit: true
      forecast:
        required: true
      max-rounds: 2
      budget-degradation: forbidden
      discipline:
        alternatives:
          require-no-action-option: true
          require-information-option: true
          min-options: 3
        stop:
          checkpoint-every: 1
          require-kill-criteria: true
        commit:
          require-reconcile: true
          require-observability: true
          require-verification: true
          require-evidence: true
        replan:
          on-critical-assumption-contradicted: replan
          on-material-evidence-changed: replan
          on-repeated-failure: stop
        evidence:
          required-independent-groups: 0
          warn-shared-origin: true
```

> `required-independent-groups: 0` 是**刻意**的 V1 值，理由見 §28。
> `on-capability-invalidated` is not a V1 profile setting. Current capability
> routing is implemented by the registry/role runners; invalidation remains a
> runtime maintenance concern rather than an authorization bypass.

> **這些數值是未經驗證的起始值，不是建議值。**
> `independent-judgments`、`challenge.trigger.dispersion-above`（2.0 / 1.5）、
> `max-attempts`、以及 §34 的 `defaultJudgeTokenEstimate`（2000）都是在沒有
> 任何真實 run 資料的情況下選定的。它們的作用是讓 profile 可用，不是宣稱
> 這是對的參數。累積足夠的實際決策之後應回頭校準，並在此記錄依據。

---

## 11. RequestContract

Hufu 已有 task 層的 goal 與 constraints，不要把 intent 欄位複製到每個 `TaskDef`。
使用 request/session 範圍的契約：

```go
type RequestContract struct {
    ID              string
    RawRequest      string
    DirectQuestion  string

    Objective       string
    SuccessCriteria []SuccessCriterion
    Constraints     []Constraint
    Assumptions     []DecisionAssumption

    Revision        uint64
    CreatedAt       time.Time
}
```

> 草稿的 `Confidence float64` 已移除：它沒有定義來源、尺度、消費者，
> 且與 `DecisionOpinion.Confidence` 混淆。

概念轉換：

```text
Question → Objective → Success Criteria → Tasks
```

進入結構化決策的前提：

```text
Objective != ""
len(SuccessCriteria) >= 1
Task.Goal != ""
```

第一次 dispatch 攜帶 `RequestContract` 的參考；**不得**為了複述契約而多做一次
LLM round trip。`RequestContract` 以 artifact 形式持久化，`DecisionRecord`
只存其 ID 與 revision。

---

## 12. DecisionPolicy

```go
type DecisionPolicy struct {
    IndependentJudgments    int    `yaml:"independent-judgments"`
    MinIndependentJudgments int    `yaml:"min-independent-judgments,omitempty"`
    ContextIsolation        string `yaml:"context-isolation,omitempty"` // strict | sealed
    ScoreScale              string `yaml:"score-scale,omitempty"`       // V1: "0-10" only

    OutsideView OutsideViewPolicy   `yaml:"outside-view,omitempty"`
    Criteria    []DecisionCriterion `yaml:"criteria,omitempty"`

    Aggregation AggregationPolicy `yaml:"aggregation,omitempty"`
    Challenge   ChallengePolicy   `yaml:"challenge,omitempty"`
    Revision    RevisionPolicy    `yaml:"revision,omitempty"`
    Premortem   PremortemPolicy   `yaml:"premortem,omitempty"`
    Forecast    ForecastPolicy    `yaml:"forecast,omitempty"`

    Finalization FinalizationPolicy `yaml:"finalization,omitempty"`

    OptionProposal OptionProposalPolicy `yaml:"option-proposal,omitempty"`

    Discipline DisciplinePolicy `yaml:"discipline,omitempty"`

    MaxRounds         int    `yaml:"max-rounds,omitempty"`
    MaxTokens         int64  `yaml:"max-tokens,omitempty"`
    BudgetDegradation string `yaml:"budget-degradation,omitempty"` // forbidden | explicit
}

type DecisionCriterion struct {
    ID        string  `yaml:"id"`
    Statement string  `yaml:"statement"`
    Weight    float64 `yaml:"weight"`
    Direction string  `yaml:"direction,omitempty"` // higher-is-better (default) | lower-is-better
}
```

### 12.1 子 policy 型別

```go
type OutsideViewPolicy struct {
    Required bool `yaml:"required,omitempty"`
}

type AggregationPolicy struct {
    Method string `yaml:"method,omitempty"` // mean-score | median-score | mean-probability | majority
}

type ChallengePolicy struct {
    Enabled bool             `yaml:"enabled,omitempty"`
    Count   int              `yaml:"count,omitempty"`
    Trigger *ChallengeTrigger `yaml:"trigger,omitempty"`
}

type ChallengeTrigger struct {
    // DispersionAbove is expressed in score points on the 0-10 scale (§14.5).
    DispersionAbove float64 `yaml:"dispersion-above,omitempty"`
}

type RevisionPolicy struct {
    Enabled bool `yaml:"enabled,omitempty"`
}

type PremortemPolicy struct {
    Enabled              bool `yaml:"enabled,omitempty"`
    RequiredBeforeCommit bool `yaml:"required-before-commit,omitempty"`
}

type ForecastPolicy struct {
    Required bool `yaml:"required,omitempty"`
}

type OptionProposalPolicy struct {
    Enabled    bool `yaml:"enabled,omitempty"`
    MaxOptions int  `yaml:"max-options,omitempty"`
}

type FinalizationPolicy struct {
    Mode string `yaml:"mode,omitempty"` // aggregate (default) | coordinator | judge
    // JudgeID is required when Mode == "judge".
    JudgeID string `yaml:"judge-id,omitempty"`
}
```

`Trigger` 使用指標型別，用以區分「未設定 trigger（無條件執行 challenge）」
與「設定為 0（任何 dispersion 都觸發）」。其餘 policy 的零值即為關閉。

Config 載入時的驗證（全部必須有測試）：

```text
independent-judgments      >= 1
min-independent-judgments  >= 1 且 <= independent-judgments（未設時等於 independent-judgments）
max-rounds                 介於 1..2
context-isolation          ∈ {strict, sealed}
score-scale                == "0-10"（V1 唯一合法值，其他值一律拒絕）
aggregation.method         ∈ {mean-score, median-score, mean-probability, majority}
challenge.count            >= 0 且 <= 3
criteria[].id              非空且唯一
criteria[].weight          > 0 且為有限值
sum(criteria[].weight)     > 0
criteria[].direction       ∈ {"", higher-is-better, lower-is-better}
budget-degradation         ∈ {"", forbidden, explicit}（預設 forbidden）
option-proposal.max-options >= 0，且啟用時必須 >= alternatives.min-options
finalization.mode          ∈ {"", aggregate, coordinator, judge}（預設 aggregate）
finalization.mode == judge ⇒ judge-id 非空
forecast.required == true  ⇒ 最終 record 必須含合法機率（見 §14）
```

---

## 13. DisciplinePolicy

避免布林值散落在不相關的 runtime 元件中。

```go
type DisciplinePolicy struct {
    Alternatives AlternativesPolicy         `yaml:"alternatives,omitempty"`
    Stop         StopPolicy                 `yaml:"stop,omitempty"`
    Commit       CommitGatePolicy           `yaml:"commit,omitempty"`
    Replan       ReplanPolicy               `yaml:"replan,omitempty"`
    Evidence     EvidenceIndependencePolicy `yaml:"evidence,omitempty"`
}
```

不得引入書名化型別，例如 `type SunTzuMode bool`、`type ArtOfWarPolicy struct{}`。
`RoutingPolicy` is configured outside this V1 profile and is consumed by the
current capability-aware role runners; it does not grant authorization.

---

## 14. 數值語意（新增，V1 必要）

草稿把聚合稱為 deterministic，但未定義尺度與缺值處理，實際上無法判定。
本節為 §21、§22、§23 的前提，**必須先實作**。

### 14.1 尺度

```text
criterion score : float64，閉區間 [0, 10]
option overall  : float64，閉區間 [0, 10]
probability     : float64，閉區間 [0, 1]
confidence      : float64，閉區間 [0, 1]
severity        : float64，閉區間 [0, 1]
likelihood/impact (FailureMode) : float64，閉區間 [0, 1]
```

`NaN`、`±Inf`、超出區間的值一律視為**無效輸出**，不得被截斷（clamp）為邊界值。
截斷會把模型的格式錯誤悄悄變成一個看似合理的判斷。

`direction: lower-is-better` 的 criterion，**由 judge 直接以「越高越好」的
語意給分**（即 judge 自行反向），runtime 不做二次轉換。`direction` 僅用於
提示詞生成與報表標示。這是為了避免「模型已反向、runtime 又反向」的雙重反轉。

### 14.2 Overall 的來源

```text
配置了 criteria  → Overall 由 runtime 計算，judge 提供的 Overall 一律忽略
                   （記錄 decision_judge_overall_ignored 事件，不視為錯誤）
未配置 criteria  → 使用 judge 提供的 Overall，必須落在 [0, 10]
```

計算式（權重先正規化）：

```text
w'_i     = w_i / Σ(w_j)          // Σ over all configured criteria
Overall  = Σ( w'_i × score_i )
```

`Σ(w_j)` 在 config 載入時已驗證 > 0，執行期不需再防除以零。

### 14.3 缺值與無效輸出

每位 judge 必須為**每個 option × 每個已配置 criterion** 給分。

```text
缺任一分數 / 值無效 / OptionID 不在 sealed packet 中
  → opinion 無效
  → 走既有 protocol repair 路徑重試一次（同一 judge、同一 sealed evidence）
  → 仍無效 → 丟棄該 opinion，emit decision_opinion_rejected（含 reason code）
  → 有效 opinion 數 < min-independent-judgments
      → 決策 fail closed（reason: decision_insufficient_valid_opinions）
```

被丟棄的 opinion 仍以事件持久化（供稽核），但不進入聚合。
Repair 最多一次：不得為了湊足人數而無限重試。

### 14.4 Determinism 規則

浮點加總順序會影響結果，因此：

```text
1. 聚合前，所有集合以 ID 的 byte-wise 升冪排序後迭代：
   options → criteria → judges（JudgeID）
2. 所有持久化的浮點數，以 half-away-from-zero 四捨五入到小數第 6 位後再寫入。
3. 比較與門檻判斷（如 dispersion-above）使用四捨五入後的值。
4. 聚合過程零 LLM 呼叫。
```

同一組 opinions 在任何平台重跑，必須逐位元得到同一個 `DecisionAggregate`。

### 14.5 Dispersion 的定義

```text
dispersion(option) = 母體標準差 σ（分母 N，N = 有效 opinion 數）
                     計算對象為該 option 的 per-judge Overall
                     單位與 score 相同，即 [0, 10] 尺度
```

同時持久化（同一尺度）：`mean`、`median`、`min`、`max`、`stddev`、`mad`
（median absolute deviation）、`judge_count`。

`challenge.trigger.dispersion-above: X` 的比較對象是**聚合勝出 option 的
σ**，單位為分數點。以 0-10 尺度，`2.0` 代表「judges 對勝出選項的評分
標準差超過 2 分」。

`N == 1` 時 σ 定義為 0。

### 14.6 勝出選項的 tie-break

必須完全 deterministic：

```text
1. 平均 Overall 最高者勝
2. 相同 → σ 最小者勝
3. 相同 → OptionID byte-wise 最小者勝
```

`majority` 聚合法的 tie-break 同理：票數 → 平均 Overall → OptionID。

---

## 15. DecisionEvidencePacket 與封存

第一個關鍵 runtime primitive 是**封存的證據**。

```go
type DecisionEvidencePacket struct {
    ID       string
    Hash     string   // hex-encoded lowercase SHA-256 of the canonical material form
    Question string

    Options  []DecisionOption
    Criteria []DecisionCriterion

    Facts       map[string]any
    Artifacts   []ArtifactRef
    BaseRates   []BaseRateEvidence
    Assumptions []DecisionAssumption

    RequestContractRef string
    CreatedAt          time.Time
}
```

### 15.1 封存規則

在第一輪判斷之前：

```text
canonicalize material fields
→ SHA-256
→ 以 artifact 形式持久化 packet 全文
→ emit decision_evidence_sealed
→ 標記 sealed
```

所有第一輪 judges：

```text
MUST 收到相同的 evidence hash
MUST NOT 看到其他 judge 的意見
MUST NOT 看到聚合結果
MUST NOT 看到 challenge
MUST NOT 看到 coordinator 的偏好
```

### 15.2 Material 欄位集合（新增，取代草稿的模糊定義）

草稿以「evidence 有任何變動就全部 stale」實作會造成 replan 風暴
（時間戳、路徑、attempt 編號都會改變 hash）。因此明確定義：

**進入 hash 的欄位（material）**

```text
Question                    trim + Unicode NFC 正規化後的字串
Options[]                   依 ID 升冪；每項取 {ID, Kind, Origin, Title, Description}
Criteria[]                  依 ID 升冪；每項取 {ID, Statement, NormalizedWeight, Direction}
Facts                       鍵名升冪；值以 canonical JSON 編碼
Artifacts[]                 依 SHA256 升冪；每項僅取 {SHA256, MediaType, Role}
BaseRates[]                 依 (ReferenceClass, Metric) 升冪；
                            每項取 {ReferenceClass, Metric, SampleSize, Distribution}
Assumptions[]               依 ID 升冪；每項僅取 {ID, Statement, Critical}
```

**不進入 hash 的欄位（non-material）**

```text
packet ID、CreatedAt、RequestContractRef
ArtifactRef 的 Path / RunID / TaskID / Attempt / Agent / Provider / Bytes / ID / Description
DecisionAssumption 的 Status / CheckedAt / EvidenceRefs
BaseRateEvidence 的 Source / Limitations
任何在正規化後不影響上述集合的空白、鍵序差異
```

理由：只有**能改變判斷本身的輸入**才算 material。artifact 內容以 SHA256
代表；同一份內容換路徑不是新證據。assumption 的 status 變動走 §31 的
ReplanPolicy，**不**改變 evidence hash——否則每次假設查核都會讓整輪意見過期。

### 15.3 Canonical 編碼

```text
1. 字串：UTF-8，Unicode NFC，前後空白 trim
2. 物件：鍵名 byte-wise 升冪，無多餘空白
3. 浮點：strconv.FormatFloat(v, 'f', 6, 64)（先依 §14.4 四捨五入）
4. 整數：十進位，無正號
5. nil / 空集合：省略該鍵（不得寫成 null 或 []，以免空與缺不一致）
6. SHA-256 → 小寫 hex
```

此編碼必須有 golden-file 測試，確保跨版本穩定。

### 15.3.1 編碼器版本

編碼規則改變時 hash 必然改變，這是對的。問題在於**單看 hash 無法分辨原因**：
是證據真的動了，還是編碼器升級了？對 runtime 的每一處比對來說兩者長得一樣，
因此一次動到編碼器的發行會看起來像是讓所有既存決策一夜之間全部失效。

```go
const CanonicalFormVersion = 1
```

- **納入雜湊輸入**（`canonical_version` 鍵）：兩個不同編碼器對同一份證據
  永遠不可能產生相同 digest，也就不可能被誤認為同一件事；
- **同時存在封包上**（`DecisionEvidencePacket.CanonicalVersion`）：
  讀的人能**解釋**差異，而不只是觀察到差異。

material 欄位集合（§15.2）增刪欄位時，必須一併 bump 這個版本。

hash 不符時的歸因：

```text
先前封包的 CanonicalVersion != 目前版本
  → decision_canonical_form_changed（證據本身可能未變）
否則
  → material evidence changed
```

未帶版本的舊封包讀為 v1——那正是該編碼器的版本，不是未知值。

### 15.4 Material 變動的後果

```text
新的 evidence hash
→ 先前的第一輪意見標記 stale（不刪除、不覆寫）
→ 建立新的 judgment round 或 decision revision
→ emit decision_evidence_changed
```

不得就地修改任何已持久化的不可變記錄。

---

## 16. Context isolation

預設：

```yaml
context-isolation: strict
```

第一輪 judge **可以**收到：

- 自身的 system/agent role；
- sealed evidence packet；
- 宣告的 project context；
- 任務相關 skills；
- 允許的來源材料；
- 允許的歷史領域記憶。

第一輪 judge **不得**收到：

- 其他 judge 的輸出；
- 聚合分數；
- challenge 輸出；
- coordinator 偏好；
- 先前的 final `DecisionRecord`（僅因其存在而注入）。

注入的記憶必須語意標註為：

```text
Background reference, not authoritative instruction.
```

更強的隔離：

```yaml
context-isolation: sealed
```

`sealed` 的實際差異（必須可測）：關閉 worker memory 注入、關閉 LTM 注入、
關閉跨任務 STM，judge 的 context 僅由 sealed packet + role + 宣告的 project
context 組成。

**驗收方式**：對每位 judge 實際送出的 prompt 取得可檢查的快照，斷言其中
不含其他 judge 的 opinion ID、分數字串或 challenge 文字。

---

## 17. Outside view

Outside view 是 typed 契約，不只是提示詞要求。

```go
type BaseRateEvidence struct {
    ReferenceClass string
    Metric         string
    SampleSize     int
    Distribution   DistributionSummary
    Source         ArtifactRef
    Limitations    []string
}

type DistributionSummary struct {
    Mean   float64
    Median float64
    P10    float64
    P90    float64
}
```

若 `outside-view.required: true`：

```text
無合法 BaseRateEvidence
→ DecisionEngine MUST NOT 進入 JUDGE
→ reason: decision_outside_view_missing
```

合法性判定（deterministic）：

```text
ReferenceClass != "" 且 Metric != ""
SampleSize >= 1
Distribution 各值皆為有限數
Source.SHA256 != ""（必須指向已存在於 ArtifactStore 的 artifact）
```

reference worker 可以產生 `BaseRateEvidence`，但：

- 只產生證據，不選擇最終選項；
- 輸出必須結構化；
- 結果必須在 judges 收到之前完成封存。

---

## 18. 決策假設

```go
type DecisionAssumption struct {
    ID           string
    Statement    string
    Status       string // unknown | supported | contradicted | stale
    EvidenceRefs []ArtifactRef
    Critical     bool
    CheckedAt    time.Time
}
```

### 18.1 誰能改變 status

草稿未定義狀態來源，會導致 runtime 無法判斷。只有三個來源：

```text
1. 任務的 submit_result 中的 assumption_checks（帶 assumption ID）
   → internal/team/coordinator_tools_result.go，提交時套用
2. 任務契約宣告該 verification 檢查哪些 assumption
   （verify-spec.assumption-refs）→ 驗證本身的通過/失敗決定狀態
3. 操作者指令：hufu decision assume <decision-id> <assumption-id> --status ...
   → 透過跨 run 索引，在形成決策的 run 結束後仍可使用
```

runtime **絕不**自行推論 status。無人查核的假設永遠停在 `unknown`。

**可回報的狀態只有 `supported` 與 `contradicted`**。`unknown` 是「沒有查核」
而不是一個發現；`stale` 是 runtime 自己的結論（先前的查核不再適用），
不是 worker 或 verification 能宣稱的東西。

**回報者只能回報決策已宣告的假設**，不能事後發明新的：指名未宣告 ID 的
查核會被拒絕（在 submit_result 路徑上是 contract violation，不是警告）。

第 2 個來源之所以獨立於第 1 個，是因為它不依賴 worker 對自己工作的說法：
契約指定哪個 verification 覆蓋哪些假設，該 verification 自己的通過或失敗
決定狀態。

### 18.2 status 對執行的影響

```text
unknown      → 不阻擋執行（否則所有未查核假設都會造成死結）
supported    → 不阻擋
contradicted → 若 Critical == true，觸發 §31 的 ReplanPolicy
stale        → 僅標示，由 §31 決定行為
```

狀態轉移一律以 append-only 事件表示。

重要：

```text
critical assumption contradicted
≠ 修改舊決策使它看起來仍然正確
```

正確做法：

```text
舊決策保持不變且可讀
→ 標記 stale / invalidated
→ 執行配置的 replan 動作
→ 建立新的 revision / decision run
```

---

## 19. 選項與 no-go 替代方案

```go
type DecisionOptionKind string

const (
    OptionExecute     DecisionOptionKind = "execute"
    OptionDefer       DecisionOptionKind = "defer"
    OptionNegotiate   DecisionOptionKind = "negotiate"
    OptionRequestInfo DecisionOptionKind = "request_information"
    OptionReduceScope DecisionOptionKind = "reduce_scope"
    OptionAbandon     DecisionOptionKind = "abandon"
    OptionCustom      DecisionOptionKind = "custom"
)

type DecisionOption struct {
    ID          string
    Kind        DecisionOptionKind
    Title       string
    Description string
}

type AlternativesPolicy struct {
    RequireNoActionOption bool `yaml:"require-no-action-option,omitempty"`
    RequireInfoOption     bool `yaml:"require-information-option,omitempty"`
    MinOptions            int  `yaml:"min-options,omitempty"`
}
```

Gate（deterministic）：

```text
require-no-action-option: true
  且不存在 Kind ∈ {defer, abandon} 的選項
  → MUST NOT 進入 JUDGE（reason: decision_no_no_go_option）

require-information-option: true
  且不存在 Kind == request_information 的選項
  → MUST NOT 進入 JUDGE（reason: decision_missing_alternative）

len(Options) < min-options
  → MUST NOT 進入 JUDGE（reason: decision_missing_alternative）
```

緊急情況可明確覆寫，但覆寫理由必須持久化
（`decision_alternatives_override` 事件，含理由字串與操作者身分）。

> **2026-09-07 狀態**：V1 **未實作**這個緊急覆寫。事件名稱保留，但沒有任何
> production 路徑會發出它——要發出它就必須先實作一條繞過 alternatives 門檻的
> 路徑，那不是為了讓事件名稱有人使用而該加的東西。
> `TestDeclaredDecisionEventsAreEmittedBySomeProductionPath` 明確把它列為
> 保留未實作，其餘每一個宣告的生命週期事件都必須有 production 發出點。

### 19.1 選項提案（option proposal）

手寫 options 是可用但不好用的：每個決策任務都要在 YAML 裡列出三個以上的
選項。提案階段讓 runtime 在任務只宣告目標時自動產生候選選項。

**核心風險**：如果「受審視的判斷」同時決定「什麼算是替代方案」，
no-go 門檻就什麼都沒檢查到——提案者只要不提「不做」，門檻就永遠通過。

**解法不是要求提案者記得，而是由 runtime 保證。** 提案完成後，
runtime 依 `AlternativesPolicy` **deterministic 地補齊**缺少的必要選項：

```text
require-no-action-option: true 且提案沒有 defer/abandon
  → runtime 注入一個 defer 選項
require-information-option: true 且提案沒有 request_information
  → runtime 注入一個 request_information 選項
```

這比「提案缺了就擋下」更強：擋下只會造成重試迴圈，而注入讓「不做」
真的出現在判斷桌上、真的被每位 judge 評分、也真的可能勝出。門檻的實質
問題從「提案者有沒有記得」變成「判斷發生時，不作為是否在選項中」——
後者才是原本要保障的性質。

**每個選項必須帶來源（provenance）**：

```go
type DecisionOptionOrigin string

const (
    OptionOriginDeclared DecisionOptionOrigin = "declared" // 任務契約宣告
    OptionOriginProposed DecisionOptionOrigin = "proposed" // 提案階段產生
    OptionOriginRuntime  DecisionOptionOrigin = "runtime"  // runtime 依門檻補齊
)
```

沒有來源標記的話，之後讀 `DecisionRecord` 的人會看到「考慮了 3 個選項」
而以為那三個都是有人想過的。`Origin` 進入 sealed evidence 的 material
欄位集合（§15.2），因為它會改變一位 judge 該如何看待這個選項。

**優先序**：任務宣告了 options 就直接使用，提案階段不執行。提案只在
任務沒有宣告任何 option 時運行。設定是明確的，不做隱式魔法。

**提案階段的隔離**：提案者只看到目標與宣告的 project context，
看不到任何偏好、任何 judge、任何先前決策。

**Resume**：提案結果以 `decision_options_proposed` 事件持久化，
resume 時重用而**不重新提案**。options 是 material 欄位，重新提案會產生
不同的 evidence hash，讓整輪判斷失效（§15.4）。

**設定**：

```yaml
option-proposal:
  enabled: true
  max-options: 5        # 上限，含 runtime 補齊的選項；0 = 用內建上限
```

未啟用且任務未宣告 options → 設定錯誤（`decision_missing_alternative`），
與提案前的行為相同。

---

## 20. 獨立判斷的 schema

```go
type DecisionOpinion struct {
    ID           string
    JudgeID      string
    EvidenceHash string
    Round        int   // 1 = first-round, 2 = revision

    OptionScores []OptionScore

    PreferredOption    string
    SuccessProbability float64

    KeyAssumptions        []string
    DisconfirmingEvidence []string
    MissingInformation    []string

    Confidence float64
    Valid      bool
    RejectedReason string
}

type OptionScore struct {
    OptionID string
    Criteria map[string]float64 // key = DecisionCriterion.ID
    Overall  float64            // runtime-computed when criteria are configured
}
```

Judge 必須對**每個**選項評分，而非只投票給一個贏家。
`PreferredOption` 必須是 sealed packet 中存在的 OptionID，否則 opinion 無效（§14.3）。

---

## 21. Deterministic 聚合

聚合**不得**呼叫 LLM。

```go
type DecisionAggregate struct {
    ID           string
    EvidenceHash string
    Round        int
    Method       string
    JudgeCount   int

    MeanScores   map[string]float64
    MedianScores map[string]float64
    MinScores    map[string]float64
    MaxScores    map[string]float64
    StdDev       map[string]float64
    MAD          map[string]float64

    MeanProbability map[string]float64

    PreferredOption string
}
```

V1 支援的聚合法：

```text
mean-score        數值分數取算術平均
median-score      數值分數取中位數（偶數個取中間兩值平均）
mean-probability  機率取算術平均
majority          以 PreferredOption 計票
```

預設對應：

```text
numeric score → arithmetic mean
probability   → arithmetic mean
ordinal score → median
```

沒有具統計可信度的歷史校準時：**不得自行發明專家權重**。
V1 一律等權重（每位有效 judge 權重相同）。

所有迭代順序、四捨五入、tie-break 依 §14.4、§14.6。

---

## 22. Dispersion 是一等資訊

不得只持久化贏家。必須持久化 §14.5 列出的完整分佈統計。

Policy 可依 dispersion 觸發 challenge：

```yaml
challenge:
  trigger:
    dispersion-above: 2.0   # 分數點，見 §14.5
```

`challenge.enabled: true` 且未設 `trigger` 時，challenge 無條件執行。
設了 `trigger` 時，僅在門檻滿足時執行，且未執行的事實必須記錄
（`decision_challenge_skipped`，含實際 σ 值）。

---

## 23. Challenge

Challenge 發生在 deterministic 聚合**之後**。

challenger 可以收到：sealed evidence、匿名化的 opinions、deterministic aggregate。
challenger 不得收到：judge 身分（預設）、coordinator 偏好。

「匿名化」的定義：opinion 中的 `JudgeID` 替換為穩定的序號別名
（`judge-a`、`judge-b`…），別名對映持久化於事件中但不進入 challenger prompt。

```go
type DecisionChallenge struct {
    ID                   string
    EvidenceHash         string
    TargetOption         string
    StrongestCountercase string
    FragileAssumptions   []string
    MissingEvidence      []string
    FalsificationTests   []string
    Severity             float64 // [0, 1]
}
```

若 team 已有不同視角的角色（security、operations、cost、migration），
runtime 可優先選擇一個已授權的不同視角來執行 challenge。
**不得**為了滿足此階段而建立大量 bias 專家 agent。

challenger 不得修改 sealed evidence；任何試圖寫入的行為視為工具違規。

---

## 24. Premortem

重用 premortem 概念，不要另建風險圖。

```go
type PremortemResult struct {
    ID             string
    AssumedOutcome string // "failure"
    FailureModes   []FailureMode
}

type FailureMode struct {
    ID                  string
    Description         string
    Likelihood          float64 // [0, 1]
    Impact              float64 // [0, 1]
    EarlyWarningSignals []string
    Mitigations         []string
    EvidenceRefs        []ArtifactRef
}
```

Premortem **發現風險**，不直接否決選項。它可以：產生 challenge 輸入、
降低估計成功機率、提出緩解任務、揭露關鍵假設。

```yaml
premortem:
  enabled: true
  required-before-commit: true   # high-stakes
```

`required-before-commit: true` 且無合法 `PremortemResult`（至少一個
`FailureMode`，各數值皆在 [0,1]）→ commit gate 阻擋，
reason `decision_premortem_required`。

---

## 25. Revision

Challenge 之後，原本的 judges 可獨立修訂一次。

```go
type DecisionRevision struct {
    ID                string
    OriginalOpinionID string
    JudgeID           string
    EvidenceHash      string

    RevisedScores      []OptionScore
    RevisedProbability float64

    Changed bool
    Reason  string
}
```

V1 硬上限：

```text
MaxRounds <= 2
```

意義：

```text
Round 1 獨立判斷
→ challenge / 結構化資訊交換
→ Round 2 獨立修訂
```

修訂輪的 judge 可以看到：sealed evidence、聚合結果、challenge、自己的原始意見。
**不得**看到其他 judge 的個別意見原文。

不得執行「一直討論到有共識」的開放式迴圈。

---

## 26. Finalization

```yaml
finalization:
  mode: aggregate     # 預設
```

```text
aggregate    最終選項 = 最後一輪聚合的 PreferredOption（§14.6 tie-break）
coordinator  由 coordinator 決定，但只能收到結構化物件
judge        由指定的單一 judge 決定
```

`coordinator` 模式時，coordinator 收到的是：sealed evidence、aggregate、
challenge、revision aggregate 這四個結構化物件，**不是**原始多代理對話歷程。
coordinator 若選擇與聚合勝出者不同的選項，必須提供理由字串，
並持久化 `decision_finalization_override` 事件。

---

## 27. DecisionRecord

決策必須成為持久化 artifact，而非散文。

```go
type DecisionRecord struct {
    ID           string
    RunID        string
    TaskID       string
    Profile      string
    EvidenceHash string
    SchemaVersion int

    RequestContractRef string

    Options     []DecisionOption
    Assumptions []DecisionAssumption

    Aggregates []DecisionAggregate  // 每輪一筆
    Challenges []DecisionChallenge
    Revisions  []DecisionRevision
    Premortem  *PremortemResult

    FinalOption      string
    FinalizationMode string
    Probability      float64

    AlternativesChecked bool
    NoGoOptionID        string

    StopPolicySnapshot StopPolicy

    SourceCount             int
    IndependenceGroupCount  int
    SharedOriginWarnings    []string

    ReplanTriggers []string
    Degradations   []DecisionDegradation  // §34

    KeyAssumptions          []string
    FalsificationConditions []string
    ReviewTriggers          []string

    Stale       bool
    StaleReason string

    CreatedAt time.Time
}
```

不得只保存勝出選項。
`ResolutionDate` 與 outcome 相關欄位屬 Phase 5（§49），V1 不寫入。

---

## 28. 證據來源與獨立性（V1 為 advisory）

代理意見的獨立性與**來源**的獨立性是兩件事。

```go
type EvidenceProvenance struct {
    SourceID                string
    SourceType              string   // artifact | url | tool_output | memory | declared
    ParentSourceIDs         []string // runtime-derived only
    DeclaredParentSourceIDs []string // model-declared, advisory only
    IndependenceGroup       string
    RetrievedAt             time.Time
    ContentHash             string
}

type EvidenceIndependencePolicy struct {
    RequiredIndependentGroups int  `yaml:"required-independent-groups,omitempty"`
    RejectCircularCitation    bool `yaml:"reject-circular-citation,omitempty"`
    WarnSharedOrigin          bool `yaml:"warn-shared-origin,omitempty"`
}
```

### 28.1 V1 的分組規則（只做可推導的部分）

草稿要求 runtime 判定「B 引用 A、C 鏡像 A → 1 組」，但 runtime 無法推導引用
血緣；唯一來源是模型自述，而規格同時規定不信任自述。V1 因此只做**可由
runtime 自行推導**的分組：

```text
規則 1  ContentHash 相同 → 同一組（完全相同的內容不是兩個確認）
規則 2  SourceType == url 且 registrable domain 相同 → 同一組
規則 3  retrieval adapter 在擷取時已知的 ParentSourceIDs → union-find 合併
（DeclaredParentSourceIDs 僅記錄，V1 不參與分組）
```

分組結果以 union-find 計算，對固定輸入必須 deterministic。

### 28.2 V1 的行為

```text
required-independent-groups  V1 內建 profile 一律為 0（不阻擋）
reject-circular-citation     V1 只警示，不阻擋
warn-shared-origin: true     → emit decision_evidence_shared_origin
```

必須持久化：`source_count`、`independence_group_count`、`shared_origin_warnings`。

**升級為阻擋門檻的進入條件**（未滿足前不得開啟）：
retrieval adapter 能對至少一種來源型別可靠產生 `ParentSourceIDs`，
且有測試證明分組結果不依賴模型自述。

---

## 29. StopPolicy 與 checkpoint

既有預算機制仍是 canonical 的資源記帳（Phase 0.5 抽出的 `BudgetManager`）。
`StopPolicy` 只**解讀**是否應繼續。

```go
type StopPolicy struct {
    MaxAttempts  int           `yaml:"max-attempts,omitempty"`
    MaxToolCalls int           `yaml:"max-tool-calls,omitempty"`
    MaxTokens    int64         `yaml:"max-tokens,omitempty"`
    MaxDuration  time.Duration `yaml:"max-duration,omitempty"`

    CheckpointEvery     int  `yaml:"checkpoint-every,omitempty"`
    RequireKillCriteria bool `yaml:"require-kill-criteria,omitempty"`

    KillCriteria []KillCriterion `yaml:"kill-criteria,omitempty"`
}

type KillCriterion struct {
    ID          string  `yaml:"id"`
    Kind        string  `yaml:"kind"`
    Threshold   float64 `yaml:"threshold"`
    Description string  `yaml:"description,omitempty"`
}
```

`MaxDuration` 以 Go duration 字串解析（`"15m"`、`"90s"`）；非法字串在 config
載入時失敗。

### 29.1 Checkpoint 的定義（新增）

```text
checkpoint = 在一次工具呼叫「完成之後」由 runtime 評估的一個同步點。
掛載點：internal/team/tool_policy_gate.go 的 policyGatedTool.Run 回傳路徑。
checkpoint-every: N 代表每 N 次已完成的工具呼叫評估一次（N >= 1）。
計數範圍：per task attempt，attempt 重置時歸零。
```

**checkpoint 評估必須是純 deterministic，零 LLM 呼叫。** 其輸入只有：

```text
BudgetManager 的計數器（tokens、duration、tool calls、attempts）
KillCriteria 門檻
event log 中已記錄的 assumption status（§18.1）
主導此任務的 DecisionRecord 的 evidence hash
既有的 no-progress 偵測結果
side-effect recovery state
```

由 LLM 重新查核假設**不在 V1 範圍**：那會使每個 checkpoint 都變成一次模型
呼叫，成本無法接受。假設狀態只能由 §18.1 的三個來源更新。

### 29.2 可計算的 kill criterion kinds

```text
budget_tokens      Threshold = 已用 token 上限（絕對值）
budget_duration    Threshold = 已耗時秒數上限
tool_calls         Threshold = 本次 attempt 的工具呼叫次數上限
attempts           Threshold = 任務嘗試次數上限
repeated_failure   Threshold = 連續失敗次數上限
no_progress        Threshold = 連續無進展 checkpoint 次數上限（沿用既有偵測器）
assumption_invalid Threshold 忽略；任一 Critical 假設為 contradicted 即觸發
```

草稿的 `expected_value` 已**移除**：V1 沒有可計算的期望值來源。
config 中出現未知 kind 一律載入失敗（reason `stop_policy_unknown_kill_kind`）。

### 29.3 進入 EXECUTE 前必須持久化

```text
entry decision（DecisionRecord ID 或「無決策」的明示標記）
budget snapshot 參考
kill criteria（完整快照，寫入 DecisionRecord.StopPolicySnapshot）
checkpoint 規則
```

不得在大量消耗資源之後才發明停止條件。
`require-kill-criteria: true` 且 `KillCriteria` 為空 → 任務在執行前失敗，
reason `stop_policy_missing_kill_criteria`。

---

## 30. CommitGatePolicy

重用既有安全概念：`side_effect`、`recovery`、`EvidenceRequirement`、
`EvidenceManifest`、blocking acceptance、`ArtifactStore`、verification、
terminal lifecycle。

```go
type CommitGatePolicy struct {
    RequiredForSideEffects []SideEffectClass `yaml:"required-for-side-effects,omitempty"`

    RequireRollback      bool `yaml:"require-rollback,omitempty"`
    RequireReconcile     bool `yaml:"require-reconcile,omitempty"`
    RequireObservability bool `yaml:"require-observability,omitempty"`
    RequireVerification  bool `yaml:"require-verification,omitempty"`
    RequireEvidence      bool `yaml:"require-evidence,omitempty"`
}
```

`RequiredForSideEffects` 未設定時的預設為
`{external_write, infra_mutation, credential_mutation, unknown}`
（與 `nonReplayableSideEffect` 一致，`internal/team/recovery.go:21`）。

### 30.1 每個 require-* 的可判定條件（新增）

草稿未定義滿足條件，且 `require-rollback` 在現有 `RecoveryPolicy` 沒有對應值
（只有 retry／reconcile／manual／never）。V1 定義：

```text
RequireVerification   task.Verify != "" 或 task.VerifySpec != nil
RequireEvidence       task.VerifySpec 帶有至少一條 assertion
                      （Assertions / ToolCallAssertions / TaskResultAssertions）
                      — 現況沒有「每個 task 的 EvidenceRequirement」來源，
                      而只有帶 assertion 的 verify_spec 會產出 EvidenceResult。
                      require-verification 與 require-evidence 因此刻意不同：
                      `verify: test -f out.pdf` 證明有東西跑過，
                      帶 assertion 的 verify_spec 才產出可供 acceptance 判定的結構化證據
RequireReconcile      task.Recovery == RecoveryReconcile 且 task.ReconcileTool != ""
RequireRollback       該任務將使用的工具具備 ToolRecoverySpec.CompensateTool != ""
                      （即存在補償操作；本 repo 沒有 rollback recovery policy）
RequireObservability  task.ExpectedStateChange != "" 且 task.ReconcileTool != ""
                      （能描述預期狀態變化，且有只讀探針可觀測實際狀態）
```

### 30.2 掛載點與阻擋語意

```text
side_effect ∈ RequiredForSideEffects
→ 在該任務的第一次「可能產生副作用的工具呼叫」之前評估 commit gate
→ 掛載於 internal/team/tool_policy_gate.go 的 authorizeToolInvocation（:545）
```

嚴格行為：

```text
缺少任一必要 commit 前提
→ policy_blocked
→ 工具程序 MUST NOT 啟動（零 process start）
→ emit commit_gate_blocked（含缺少的具體前提 reason code）
```

`recovery-policy = reconcile` 不得被迫宣稱擁有假的 rollback 能力。
接受 rollback **或** reconcile，依既有 recovery model 決定。

---

## 31. ReplanPolicy

```go
type ReplanPolicy struct {
    OnCriticalAssumptionContradicted string `yaml:"on-critical-assumption-contradicted,omitempty"`
    OnMaterialEvidenceChanged        string `yaml:"on-material-evidence-changed,omitempty"`
    OnRepeatedFailure                string `yaml:"on-repeated-failure,omitempty"`
}
```

允許的動作：

```text
continue | replan | stop | request_information | escalate | needs_human
```

未設定時預設 `continue`（保持舊行為）。未知動作在 config 載入時失敗。

> 草稿的 `on-evidence-packet-changed` 改名為 `on-material-evidence-changed`，
> 以強調觸發條件是 §15.2 的 material 欄位變動，而非任何欄位變動。
> Capability-aware routing is implemented outside the V1 profile shape; the
> profile still fails closed when its declared decision contract is invalid.

重要 invariant：

```text
決策輸入實質改變
→ 舊 DecisionRecord 保持可讀且不變
→ 建立新的 DecisionRevision / DecisionRun
```

絕不改寫歷史來讓過去看起來與現在一致。

---

## 32. 執行 checkpoint 迴圈

```text
DecisionRecord / TaskDef
   ↓
Commit Gate
   ↓
Persist StopPolicy + KillCriteria
   ↓
Execute
   ↓
Checkpoint（每 N 次工具呼叫，deterministic）
   ├─ 假設仍有效        → continue
   ├─ kill criterion 觸發 → stop
   ├─ material evidence 改變 → 依 ReplanPolicy
   ├─ critical assumption contradicted → 依 ReplanPolicy
   └─ side-effect 狀態未知 → reconcile / needs_human
   ↓
Verify
   ↓
Evidence Gate
   ↓
Acceptance
```

---

## 33. 與 `adversarial_verify` 的關係

兩者職責必須分開：

```text
Decision challenge     決策定案「之前」→ 攻擊候選判斷 → 可能觸發修訂
adversarial_verify     執行「之後」   → 攻擊「工作正確／完整」的主張 → 可能觸發修復/重試
```

因此：

```text
decision challenge != adversarial verification
consensus 不是 verification 證據
```

V1 不得把 challenge 的結果餵入 `adversarial_verify` 的判定，反之亦然。

---

## 34. 預算語意

DecisionProfile 可配置 `max-tokens`、`independent-judgments`、
`challenge.count`、`max-rounds`；全部仍受既有 team/global 上限約束
（`MaxTotalTokens`、`MaxWallClock`、`MaxConcurrent`）。

### 34.1 預算不足時的行為

```yaml
budget-degradation: forbidden   # 預設
```

```text
forbidden  預算不足以支撐配置的嚴謹度 → 決策 fail closed
           reason: decision_budget_insufficient
```

```yaml
budget-degradation: explicit    # 必須由使用者明確開啟
```

`explicit` 時，runtime 依**固定順序**降級，每一步都 emit
`decision_budget_degraded` 並寫入 `DecisionRecord.Degradations`：

```text
步驟 1  challenge.count 降至 1（若原本 > 1）
步驟 2  停用 revision 輪（max-rounds → 1）
步驟 3  independent-judgments 降至 min-independent-judgments
步驟 4  仍不足 → fail closed
```

```go
type DecisionDegradation struct {
    Step      string
    From      string
    To        string
    Reason    string
    Timestamp time.Time
}
```

### 34.2 永不可降級的項目

```text
outside-view 必要性
premortem 必要性
no-go / information option 門檻
context isolation 等級
commit gate 前提
kill criteria 要求
```

品質門檻不因執行不便而放寬。降級**永不靜默**：使用者必須能在事件與
`DecisionRecord` 中看到降了什麼、為什麼。

---

## 35. 不可變的決策物件

一旦持久化，以下視為不可變：

```text
DecisionEvidencePacket(hash)
DecisionOpinion(id)
DecisionAggregate(id)
DecisionChallenge(id)
DecisionRevision(id)
PremortemResult(id)
DecisionRecord(id)
```

Resume 由 event log 與已持久化的 artifacts 重建狀態。
不得靜默重新產生已完成的階段。
`DecisionRecord.Stale` 與 `StaleReason` 是唯二允許在既有 record 上被設定的
欄位，且只能由 `unset → set` 單向轉移，並以事件記錄。

---

## 36. Runtime 事件

命名沿用既有 snake_case 慣例（已驗證，§4.1）。所有事件經
`EventJournal.Append`（`internal/team/services.go:18`）寫入，納入既有 hash chain。

```text
decision_started
decision_evidence_sealed
decision_evidence_changed
decision_reference_completed
decision_opinion_submitted
decision_opinion_rejected
decision_judge_overall_ignored
decision_aggregate_computed
decision_challenge_submitted
decision_challenge_skipped
decision_premortem_submitted
decision_revision_submitted
decision_finalized
decision_finalization_override
decision_alternatives_override
decision_budget_degraded
decision_evidence_shared_origin
decision_invalidated

assumption_declared
assumption_supported
assumption_contradicted
assumption_stale

replan_requested
replan_completed

commit_gate_blocked
kill_criterion_triggered
```

每個事件的 payload 至少包含：

```text
run_id
task_id
decision_id（適用時）
evidence_hash（適用時）
timestamp
reason code（適用時）
```

`RunEvent` 的 `RunID`／`TaskID`／`Timestamp` 由既有 store 填入，不需重複放進 payload。

**不得持久化 chain-of-thought。** 只持久化結構化欄位與明確的理由字串。

---

## 37. 失敗原因碼

避免模糊錯誤。以下為 canonical 集合，必須以常數定義於單一檔案
（`internal/team/decision_reason.go`）並在測試中斷言：

```text
decision_profile_unknown
decision_missing_objective
decision_missing_alternative
decision_no_no_go_option
decision_outside_view_missing
decision_premortem_required
decision_budget_insufficient
decision_opinion_invalid
decision_insufficient_valid_opinions
decision_evidence_not_sealed
decision_stale

commit_gate_missing_recovery
commit_gate_missing_reconcile
commit_gate_missing_observability
commit_gate_missing_verification
commit_gate_missing_evidence

stop_policy_missing_kill_criteria
stop_policy_unknown_kill_kind
kill_criterion_reached

assumption_invalidated

reconcile_required
reconcile_unknown_state
```

原因碼與事件名的**唯一**定義處是 `internal/agent/decision_reason.go`；
`internal/team/decision_reason.go` 以常數別名再匯出，讓 runtime 呼叫端讀起來自然。
定義放在 agent 套件是因為 config 驗證（`internal/agent`）與 runtime
（`internal/team`）都要引用同一份清單，而 agent 不能反向 import team。

同一組原因碼必須被 event log、TUI、reports、resume/replay、tests 一致重用。
`decision_evidence_not_independent`、`routing_*` 原因碼屬延後範圍，V1 不定義。

---

## 38. Crash / Resume 語意

### 38.1 決策過程 crash

```text
3 位 judge 中已持久化 2 份意見，evidence hash 未變
→ resume 只 dispatch 缺少的那一位
```

不得重新產生前兩份。

```text
evidence hash 已改變
→ 舊意見標記 stale
→ 開啟新的合法判斷輪
```

### 38.2 聚合後 crash

```text
已持久化的輸入未變
→ 重用已持久化的 opinions 與 aggregate
→ 依需要續跑 challenge / revision / finalization
```

### 38.3 副作用執行中 crash

絕不盲目重跑。沿用既有 recovery 語意與 reconcile exit codes
（`internal/team/recovery.go:45`）：

```text
reconcile → complete / partial / not_started / unknown
```

`unknown` 且 policy 要求人工檢視 → `needs_human`。

---

## 39. 安全邊界

1. 決策 worker 預設為只讀。
2. 研究需求不自動蘊含 shell 存取。
3. 外部研究存取遵循 team policy 與最小權限。
4. `DecisionEngine` 不直接相依 Ollama／OpenAI／Lemonade 等 provider。
5. `DecisionEngine` 不直接組裝 shell 命令。
6. `DecisionEngine` 不直接讀取任意 team 檔案。
7. commit gate 在工具程序啟動前完成。
8. 未知的 policy 狀態在嚴格 profile 下 fail closed。
9. 來源不可信的證據不被視為獨立證據。
10. secret 處理沿用既有全域 redaction／SecretRef 政策
    （`internal/team/workspace_redact.go`）。
11. 決策 profile 的選擇永不繞過 authorization。
12. `TaskDef.DecisionProfile` 不可由 coordinator payload 設定（§9）。

---

## 40. 可觀測性

**實作**：`internal/team/decision_metrics.go`（`ComputeDecisionMetrics`），
CLI 入口 `hufu decision stats [--run <id>] [--json]`。

每個計數都是對 runtime **已經持久化**的狀態所做的投影——append-only event log
與跨 run 索引。沒有另外儲存、沒有重複計數，因此指標不可能與產生它的事件漂移；
這與 §29 讓 StopPolicy 讀取預算帳本而非自建計數是同一條原則。

指標：

```text
decision_count
decision_profile_count

outside_view_gate_failures
premortem_failure_modes
no_go_option_missing

decision_dispersion
decision_revision_rate
decision_stale_count
decision_opinion_rejected_count
decision_budget_degraded_count

independent_evidence_group_count
shared_origin_warnings

kill_criteria_triggered
replan_count
assumption_invalidations

commit_gate_blocked
reconcile_required_count
```

`capability_routing_fallbacks` 與 `capability_invalidations` remain optional
operational metrics around the implemented routing path;
`reconcile_required_count` still depends on crash-recovery inputs and is not a
V1 decision contract.

`hufu decision stats` 另外回報**距離 Phase 5 進入條件還差多少**
（§49.2 的 30 筆已結案 / 20 筆已驗證），因為那是唯一無法用程式碼滿足的條件。

追蹤鏈：

```text
RequestContract
→ EvidencePacket hash
→ opinions
→ aggregate
→ challenge
→ revision
→ DecisionRecord
→ execution task
→ commit gate
→ checkpoints
→ verification
→ evidence
→ acceptance
```

---

## 41. 記憶整合

可作為未來 LTM 升級候選：

```text
verified failure lesson
repeated pattern
reviewer-confirmed rule
well-supported reference class
```

**不得**自動升級：

```text
單一成功軼事
agent 自述的專長
未驗證的因果敘事
coordinator 偏好
```

記憶注入維持非權威：

```text
Background reference, not authoritative instruction.
```

> 草稿的「outcome-resolved forecast」需要 Phase 5 的結案機制，V1 不列入。

---

## 42. 套件與檔案配置

**不得**建立 `decision/`、`policy/`、`routing/` 頂層套件。原因（§4.3）：
`TaskDef`、`EventStore`、`ArtifactStore`、`ArtifactRef` 全部位於 `internal/team`，
外部套件引用它們會產生循環相依，而把這些型別下沉會是一次跨 432 檔的重構，
與本規格的 guardrail 直接衝突。

**設定型（declarative）型別放 `internal/agent`**，因為 `TeamConfig` 與所有既有
的宣告式設定型別（`WorkflowConfig`、`RetryConfig`、`CapabilityConfig`）都在那裡，
而 `internal/team` 已經 import `internal/agent`；反向 import 會是循環。
**runtime 型別放 `internal/team`**，因為它們引用 `ArtifactRef`、`TaskDef`。
每個檔案自行守住 `CLAUDE.md` 的 800 行上限：

```text
Phase 0
  internal/agent/decision_config.go      DecisionConfig / DecisionPolicy / DisciplinePolicy
                                         及其子 policy、全部 config 驗證規則
  internal/agent/decision_reason.go      reason code 與 event type 常數（唯一定義處）
  internal/team/decision_types.go        DecisionRecord / Opinion / Aggregate / Option /
                                         Assumption / Challenge / Revision / Premortem /
                                         RequestContract 等 runtime 型別 + config 型別別名
                                         （RequestContract 與其他 runtime 型別同檔，
                                          不另開 request_contract.go）
  internal/team/decision_config.go       profile 解析（precedence chain）與 task profile 驗證
  internal/team/decision_reason.go       reason code / event type 別名再匯出

Phase 0.5
  internal/team/budget_manager.go        BudgetManager（自 Coordinator 抽出的計數所有權）

Phase 1
  internal/team/decision_numeric.go      §14 數值語意：尺度驗證、權重正規化、
                                         四捨五入、mean/median/σ/MAD
  internal/team/decision_canonical.go    §15.3 canonical 編碼器
  internal/team/decision_evidence.go     packet、material 欄位集合、SHA-256 封存
  internal/team/decision_isolation.go    judge context 組裝與隔離斷言用的快照
  internal/team/decision_engine.go       階段編排（Run / Resume）
  internal/team/decision_aggregate.go    opinion 驗證、deterministic 聚合與 dispersion
  internal/team/decision_budget.go       §34 預算准入與降級階梯
  internal/team/decision_store.go        事件持久化與 resume 投影

Phase 2
  internal/team/decision_options.go      §19.1 選項提案、slug 正規化、
                                         必要替代方案的 runtime 注入
  internal/team/decision_gates.go        alternatives / outside-view / premortem /
                                         forecast / request-contract gates、finalization
  internal/team/decision_challenge.go    challenge 與 premortem 的提示、匿名化、驗證
  internal/team/decision_revision.go     單輪獨立修訂與 round 2 投影
  internal/team/decision_provenance.go   union-find 分組與警示
  internal/team/decision_assumptions.go  §18.1 三個假設狀態來源的套用與驗證
  internal/team/decision_metrics.go      §40 指標投影與 Phase 5 進入條件評估
  internal/team/decision_engine_stages.go  premortem / challenge / revision /
                                         provenance 的階段編排與 resume

Phase 3
  internal/team/stop_policy.go           checkpoint 評估與 kill criterion（純函式）
  internal/team/commit_gate.go           commit 前提判定（純函式）
  internal/team/replan.go                assumption lifecycle、stale 標記、replan 事件
  internal/team/decision_discipline.go   runtime 掛載：per-task arm / 兩個 hook
```

Phase 3 的掛載點在 `internal/team/tool_policy_gate.go` 的 `policyGatedTool.Run`：
commit gate 在 `t.inner.Run` 之前、checkpoint 在其之後。未 arm discipline 的
task（即預設的 `off` profile）兩個 hook 皆為 no-op。

每個檔案都必須有對應的 `_test.go`。
若某檔逼近 800 行，先橫向切分（例如 `decision_aggregate_stats.go`），
不得把邏輯塞回 `coordinator_task_run.go`（已 4671 行）。

`DisciplinePolicy` 不另開 `decision_discipline.go`：它與 `DecisionPolicy`
同屬設定層，合併在 `internal/agent/decision_config.go` 內仍遠低於 800 行。

**唯一允許的例外**：若某組型別確定既不引用 `internal/team` 也不引用
`internal/agent` 的任何識別字，可放入新的 `internal/decision`。
V1 不預先這麼做，避免無收益的搬遷。

---

## 43. 實作順序

Coding agent 必須依相依順序實作，並遵守 §44 的 phase barrier。

---

### Phase 0 — Typed schema 與相容性基礎

**目標**：導入共用資料模型與設定介面，**完全不改變既有 runtime 行為**。

**前置**：無。

**必須實作**

- `TaskDef.DecisionProfile`（`json:"-"`，§9）；
- `DecisionConfig`、`DecisionPolicy`、`DisciplinePolicy` 及其子 policy；
- `DecisionOptionKind`、`DecisionOption`、`DecisionCriterion`、`DecisionAssumption`；
- `EvidenceProvenance`；
- 初版 `DecisionRecord`（含 `SchemaVersion`）；
- §36 事件名與 §37 原因碼常數；
- §12 的全部 config 驗證規則；
- 持久化 / 遷移相容性。

**尚不得實作**

DecisionEngine 編排、多 judge dispatch、challenge／revision、
stop／replan 執行期行為、commit gate 行為、capability routing。

**相容性要求**

```text
舊 team.yaml                → 行為完全相同
未設 decision-profile 的 task → 行為完全相同
舊持久化記錄               → 仍可讀
未知 enum 值               → 驗證失敗
typed schema 宣稱支援的未知欄位 → 不得靜默忽略
```

**驗收測試**

1. 舊 team 設定原封不動載入成功。
2. 既有執行路徑行為不變（跑既有回歸套件）。
3. 新 config enum 拒絕非法值，錯誤訊息含 reason code。
4. 新記錄型別的序列化／反序列化 deterministic（golden file）。
5. Event replay 仍 deterministic。
6. 涵蓋前一版持久化格式的讀取測試。
7. **`decision_profile` 出現在 task payload JSON 時解碼後為零值**。
8. diff 中沒有無關的 scheduler 改寫。

**完成條件**：Phase 0 測試與既有回歸測試全數通過。

---

### Phase 0.5 — Budget ownership extraction

**目標**：把預算計數的所有權自 `Coordinator` 抽出為明確元件，使 Phase 3 的
`StopPolicy` 能「只讀不記帳」。這是本規格**唯一**授權的既有程式重構
（§45 guardrail 6 的明確例外）。

**前置**：Phase 0 完成。

**必須實作**

```go
type BudgetManager interface {
    TokensUsed() int64
    Reserved() int64
    Limits() BudgetLimits
    Exceeded(elapsed time.Duration) (bool, string)
    Snapshot(elapsed time.Duration) BudgetSnapshot
}
```

> 介面已依實際 scoping 修正。既有帳本是 **run 範圍**（由
> `tokenBudgetRoot()` 決定唯一擁有者），不是 task 範圍，因此不接受
> `runID`／`taskID`／`attempt` 參數——那會是純粹的雜訊。`elapsed` 由呼叫端傳入，
> 因為帳本擁有的是「計數器」，session 的起始時間仍屬 `Coordinator.sessionTime`。

- 將 `Coordinator.budgetExceeded()`（`internal/team/coordinator.go:1009`）的
  計數與判定移入 `internal/team/budget_manager.go`；
- `budgetLedger` 以**內嵌（embedded）**方式放進 `Coordinator`，使既有的
  `owner.tokensUsed`／`owner.tokenBudget` 等引用（含測試中的讀取）
  透過欄位提升解析到同一份儲存；
- `Coordinator` 的 budget 方法改為薄委派；
- 沿用既有 `BudgetSnapshot`（`internal/team/diagnosis.go:47`），
  **不得**變更其 JSON 欄位或既有的 `[REDACTED]` 相容處理。

**硬性 invariant**

```text
本階段行為零變化：同樣的輸入必須產生同樣的 budget_exceeded 判定與訊息
計數器只有一個擁有者（BudgetManager）
沒有第二套平行記帳
```

**驗收測試**

1. 既有 budget 相關測試（含 `coordinator_terminal_test.go` 的
   `budget_exceeded` 事件斷言）不做**行為**修改即通過。
   唯一允許的例外是 composite literal 的路徑調整：
   `Coordinator{maxWallClock: x}` → `Coordinator{budgetLedger: budgetLedger{maxWallClock: x}}`
   （Go 不允許在 composite literal 中設定提升欄位）。此類調整不得改變任何斷言。
2. 新增測試：同一輸入下 `BudgetManager.Exceeded` 與抽取前的判定一致，
   **包含訊息字串**（其他層會把它呈現給使用者）。
3. `BudgetSnapshot` 的序列化與遷移測試不變。
4. 單一擁有者測試：掃描套件內非測試 `.go` 檔，斷言 `tokensUsed.Add(`／
   `tokensUsed.Store(`／`tokenReservations` 的寫入只出現在 `budget_manager.go`。

**完成條件**：既有測試零修改通過，且 `grep` 顯示 token/duration 計數只在
`budget_manager.go` 內被更新。

---

### Phase 1 — 最小決策引擎

**目標**：讓「獨立結構化判斷」成為真正的 runtime primitive。

**前置**：Phase 0、Phase 0.5 完成。

**必須實作**

- §14 的全部數值語意（尺度、正規化、缺值、determinism、dispersion）；
- `DecisionEvidencePacket` 與 §15.2／§15.3 的 canonical 編碼與封存；
- strict 第一輪 context isolation（§16）；
- N 份獨立結構化判斷、`DecisionOpinion`；
- deterministic mean/median 聚合、`DecisionAggregate`；
- 持久化的 `DecisionRecord`；
- DecisionEngine 的 resume／replay；
- 依 §34 的判斷人數預算驗證。

**介面**

```go
type DecisionEngine interface {
    Run(ctx context.Context, req DecisionRequest) (*DecisionRecord, error)
    Resume(ctx context.Context, decisionID string) (*DecisionRecord, error)
}

type DecisionRequest struct {
    RunID   string
    TaskID  string
    Profile string

    Question string
    Options  []DecisionOption

    Facts     map[string]any
    Artifacts []ArtifactRef
}

// 對映本 repo 實際存在的介面（§4.2：草稿的 AgentRuntime 型別不存在）。
type DecisionServices struct {
    Pool     AgentPool        // internal/team/services.go:100
    Journal  EventJournal     // internal/team/services.go:18
    Store    ArtifactStore    // internal/team/evidence_store.go:30
    Context  ContextCompiler  // internal/team/services.go:82
    Budget   BudgetManager    // Phase 0.5
}
```

**硬性 invariant**

```text
第一輪 judges：同一 evidence hash、看不到其他意見、看不到聚合、
              看不到 challenge、看不到 coordinator 偏好
聚合：deterministic、零 LLM 呼叫
```

**尚不得實作**

outside-view 硬門檻、premortem、challenge、revision、
證據獨立性門檻、execution stop/replan。

**驗收測試**

隔離
- judge A 的實際 prompt 不含 judge B/C 的 opinion；
- 所有第一輪 judge 的 evidence hash 相同；
- coordinator 偏好不在 sealed evidence 內。

數值（§14）
- 分數超出 [0,10]／NaN／Inf → opinion 無效，且**不被 clamp**；
- 缺一個 criterion 分數 → repair 一次 → 仍缺 → 丟棄並記錄；
- 有效 opinion 少於 `min-independent-judgments` → fail closed；
- 權重 `{2,1,1}` 正規化為 `{0.5,0.25,0.25}` 並正確算出 Overall；
- 打亂 judge/option/criterion 的輸入順序，聚合結果逐位元相同；
- tie-break 三層規則各有一個案例。

封存（§15）
- material 欄位改變 → hash 改變；
- 只改 `CreatedAt`／`ArtifactRef.Path`／`Assumption.Status` → hash **不變**；
- canonical 編碼的 golden file。

Resume
- 2/3 意見後 crash → 只 dispatch 第 3 位；
- 已持久化的**有效**意見不被靜默重新產生；
- 被拒絕的意見保留供稽核，但**不**算已完成工作：該 judge 在後續 run 仍會被
  重新派工（run 內的一次性 repair 上限仍防止湊人數）；
- evidence hash 改變 → 舊意見標記 stale；
- 已 finalize 的決策不重算。

預算
- profile 要 5 位 judge 但預算不足且 `budget-degradation: forbidden` → fail closed；
- runtime 不靜默減少 judge 數。

**完成條件**：Hufu 能產生並重播一份持久化的多 judge `DecisionRecord`，
且非決策執行路徑行為不變。

---

### Phase 2 — 決策品質門檻

**目標**：建立完整的 commit 前決策形成迴圈。

**前置**：Phase 1 完成。

**必須實作**

`RequestContract`；outside-view 硬門檻與 `BaseRateEvidence`；
`AlternativesPolicy` 與 no-go 驗證；typed assumptions（§18，含 status 來源）；
premortem；evidence provenance 與 §28.1 的分組；challenge；一輪獨立修訂；
dispersion 觸發；forecast 驗證；finalization policy。

**目標流程**

```text
RequestContract → Alternatives Gate → Outside View → Premortem
→ Seal Evidence → Independent Judgments → Deterministic Aggregate
→ Challenge → Independent Revision → DecisionRecord
```

**硬性 invariant**

- 必要的 no-go 選項缺失 → 進入 JUDGE 前阻擋；
- 必要的 outside view 缺失 → 進入 JUDGE 前阻擋；
- high-stakes 必要的 premortem 缺失 → commit／finalization 前阻擋；
- challenge 只能在聚合後開始；
- challenger 不能修改 sealed evidence；
- 修訂輪數有界（`MaxRounds <= 2`）；
- `source_count` 與 `independence_group_count` 是不同的數。

**驗收測試**

```text
no-go 必要 + options=[A,B]        → blocked（decision_no_no_go_option）
options=[A,B,DEFER]               → 繼續
outside-view 必要 + 無 base rate  → blocked（decision_outside_view_missing）
high-stakes + premortem 必要 + 缺 → blocked（decision_premortem_required）
內容雜湊相同的三份來源           → independence_group_count = 1
同一 registrable domain 的三個 URL → independence_group_count = 1
模型自述的 parent 關係            → 不影響分組，僅記錄
challenge 只在聚合後執行、看到匿名化意見、預設看不到 judge 身分
dispersion 未達門檻               → challenge 跳過且記錄實際 σ
revision：原 judge 獨立修訂、MaxRounds <= 2、無開放式共識迴圈
forecast 必要 → probability ∈ [0,1] 且 resolution condition 非空
```

**尚不得實作**

StopPolicy checkpoint 行為、CommitGate 強制、runtime replan。

**完成條件**：Hufu 能產生一份符合 policy 的 `DecisionRecord`，
含結構化替代方案、證據、challenge 與有界修訂。

---

### Phase 3 — 執行紀律

**目標**：讓「承諾」成為有條件的，並讓執行可被 policy 中斷。

**前置**：Phase 2 完成。

**必須實作**

`StopPolicy` 與 §29.2 的 `KillCriterion` kinds；§29.1 的 checkpoint 掛載；
`CommitGatePolicy` 與 §30.1 的可判定條件；assumption lifecycle 事件；
`ReplanPolicy`；stale decision 語意；配置的 stop／replan／escalate 動作；
副作用工具啟動前的 commit gate；與既有 recovery 語意的 reconcile 整合。

**硬性 invariant**

1. commit gate 在有副作用的程序啟動前執行。
2. 缺少嚴格前提 → 零工具程序啟動。
3. 預算計數器仍由 `BudgetManager` 擁有。
4. `StopPolicy` 只讀取／解讀計數器，不重複記帳。
5. checkpoint 評估零 LLM 呼叫。
6. critical assumption 被推翻時，絕不就地修改舊 `DecisionRecord`。
7. 副作用 crash 絕不盲目重跑。
8. 副作用狀態未知時依 policy 進入 reconcile / needs_human。

**驗收測試**

```text
commit gate
  infra_mutation + 需要 reconcile + 無 reconcile tool
  → policy_blocked → 零 tool process start → commit_gate_blocked 事件

  require-observability + ExpectedStateChange 為空 → blocked
  require-evidence + 無 Required 的 EvidenceRequirement → blocked

stop
  kill criterion 達成 → stop（已消耗的資源不得被當成繼續的正面理由）
  require-kill-criteria: true + KillCriteria 為空 → 執行前失敗
  未知 kill kind → config 載入失敗

checkpoint
  checkpoint-every: 2 → 第 2、4、6 次工具呼叫後各評估一次
  checkpoint 評估過程零 LLM 呼叫（以 mock provider 斷言呼叫次數為 0）
  attempt 重置後計數歸零

assumption
  A1 critical 且 supported → 執行
  新證據使 A1 contradicted → 舊決策標記 stale → 執行配置的 replan
  → 舊 DecisionRecord 內容不變（逐位元比對）

evidence
  material 變動 → 舊第一輪意見 stale → 新 revision/run
  non-material 變動 → 不觸發 replan

crash / recovery
  副作用狀態未知 → reconcile → complete/partial/not_started/unknown
  unknown 不得自動觸發盲目重試
```

**完成條件**：Hufu 能阻止不安全的承諾、依預先宣告的條件停止執行，
並在假設失效時建立新的決策譜系。

---

### Phase 3.5 — Dispatch integration

**目標**：把決策子系統接到實際執行路徑。在此之前，`decision-profile` 只是
一個會通過驗證但不產生任何行為的設定。

**前置**：Phase 3 完成。

**為什麼需要這個 phase**：Phase 0–3 的 Must Implement 清單描述的是子系統，
沒有任何一項要求「接到 coordinator 的 dispatch path」。實作照做之後，
`grep` 顯示 `NewDecisionEngine`、`ResolveDecisionProfile`、`armDiscipline`
的 production 呼叫點都是 0，四個 stage runner 也只有 interface 沒有實作。
子系統測試齊全，但通電沒有。本 phase 補上這一段。

**必須實作**

- `--decision-profile` CLI flag，並貫穿到 run 範圍的 override（§8）；
- team 載入時呼叫 `ValidateTaskDecisionProfiles`，未知 profile 在 run 開始前失敗；
- 四個 stage runner 的 production 實作。它們建構在既有的 judge sidecar
  （`AgentPool().JudgeSidecar()` + `Sidecar.ExecuteProfile`）之上：
  sidecar 沒有工具，因此「決策 worker 預設為只讀」（§38.1）由構造保證，
  不需要額外的執行期強制；
- dispatch path：解析 profile → 建 `DecisionEngine` → 形成決策 →
  arm discipline → 執行 → disarm；
- 把 `DecisionIndex` 傳入 `DecisionServices`，使實際 run 的決策可被
  `hufu decision resolve` 定址；
- 補完 `CheckpointState` 在實際掛載中未填的欄位：
  `Attempt`、`NoProgressStreak`、`MaterialEvidenceChanged`、`SideEffectState`；
- checkpoint 回傳 stop / replan 時的實際作用：
  `RequestReplan`、`MarkDecisionStale` 必須真的被呼叫。

**假設狀態的來源**：三個來源皆已實作，見 §18.1。它們解鎖了
`assumption_invalid` kill criterion 與 `on-critical-assumption-contradicted`
這條 replan 路徑——在此之前兩者的邏輯與測試都完整，只是沒有輸入。

**選項從哪裡來（本 phase 的範圍決定）**

`DecisionOptions` 由**任務契約宣告**（`decision-options:`，`json:"-"`）。
自動提案階段見 §19.1，於本 phase 之後加入：它保留了同一個安全性質，
但改用「runtime 依門檻 deterministic 補齊必要選項 + 每個選項帶來源標記」
而不是靠提案者自律。宣告的 options 仍然優先，提案只在未宣告時運行。

**本 phase 未接上的 checkpoint 輸入（明確記錄，非遺漏）**

```text
Attempt           ✅ 由 TodoItem.Retries + 1 提供
NoProgressStreak  ✅ 由既有的 noProgressCounters().Turns 提供
ConsecutiveFailures / ToolCalls / TokensUsed / Elapsed  ✅
MaterialEvidenceChanged  ❌ 單一任務執行期間 sealed evidence 不會變；
                            它的觸發點在跨任務的證據更新，由假設／verify 路徑
                            （§18.1）承擔，該路徑已實作
SideEffectState          ✅ arm 時由 TodoItem.RecoveryState 帶入。
                            正常執行期間為空是正確的（沒有存疑的變更）；
                            中斷後恢復的任務若無法分類其結果，checkpoint 會
                            以 reconcile_unknown_state 停止，而不是在無人能
                            交代的狀態上繼續執行
```

**已知缺陷（本 phase 必須修掉）**

```text
internal/team/decision_discipline.go  attempt: task.MaxRetries * 0
  → 恆為 0，attempts kill criterion 永遠不會觸發
```

**硬性 invariant**

1. `decision-profile` 未設定或為 `off` 的 task，行為與接線前逐位元相同。
2. 決策形成失敗時，任務依既有失敗路徑處理，不得靜默當成成功。
3. 決策 worker 不得取得工具。
4. arm 過的 discipline 必須在任務結束（含失敗與 panic 路徑）被 disarm。
5. 未 arm 的任務，兩個 tool hook 仍為 no-op。

**驗收測試**

> **2026-09-07 更正**：「零決策事件」在 Stage 3 引入 occurrence admission 之後
> 不再成立，而且**不應該**成立。`decision_admitted` 已不只是決策簿記：它同時
> 把任務的不可變輸入綁定到一個 digest，`validateTaskCreationAdmission` 靠它
> 拒絕「admit 之後才被竄改」的任務。對 off task 略過這個 marker，等於把這層
> 竄改偵測從所有既有 run 上拿掉——那比事件日誌多一列糟得多。
>
> 因此相容性承諾的精確形式是：off task 產生**零決策生命週期事件**
> （started／sealed／opinion／aggregate／challenge／revision／finalization／
> assumption／replan 全部為 0）、零 judge 派工、不 arm discipline、執行行為不變；
> 它唯一寫入的決策事件是記錄 `enabled=false` 的 admission marker。
> 由 `TestDecisionV1OffProfileIsInert` 逐一斷言，該測試列出全部生命週期事件名稱，
> 而不是只檢查其中兩個。

```text
未設 profile 的 task → 零決策生命週期事件、零 judge 派工、行為不變
                       （admission marker 除外，見上方更正）
--decision-profile 未定義的名稱 → run 開始前失敗
task 的 decision-profile 未定義 → team 載入失敗
設了 profile 的 task → 產生 DecisionRecord、寫入索引、可被 decision show 讀到
決策 worker 的工具集為空
commit gate 在實際 dispatch 中擋下缺前提的副作用任務 → 零 tool process start
checkpoint 在實際 dispatch 中填入 attempt / no-progress / side-effect state
attempts kill criterion 在真實重試下會觸發（迴歸上述缺陷）
task 結束後 discipline 已 disarm
```

**完成條件**

在 team.yaml 設定 `decision-profile: standard` 且任務宣告 `decision-options`
之後，一次真實 run 會形成、持久化並列出一筆決策，且 `hufu decision list`
能看到它。

---

## 44. Phase barrier

Coding agent 在 Phase N 完成前不得開始 Phase N+1。

每個 phase 的通過條件：

```text
1. 該 phase 的驗收測試全數通過。
2. 既有回歸套件通過（go test ./...）。
3. 沒有殘留的相容性違規。
4. Event／持久化 replay 仍 deterministic。
5. 本 phase 的必要項目沒有未經維護者明確同意就被延後。
6. 沒有引入無關的 scheduler/runtime 架構改寫（Phase 0.5 是唯一例外）。
7. git diff --check 通過。
8. go vet ./... 通過。
9. 新增的每個檔案 < 800 行。
10. 新的 public config/schema 行為都有測試。
11. 失敗原因碼穩定，且在適用測試中被斷言。
```

每個 phase 結束時必須回報：

```text
Implemented
Tests added
Tests executed
Compatibility impact
Known limitations
Deferred items
```

---

## 45. Coding agent 守則

1. 決定套件／檔案位置前，先檢查現有程式碼。
2. 重用既有的 scheduler、event、artifact、budget、recovery、verification、
   context 邊界。
3. 偏好 typed 結構，而非以 Markdown／散文作為 canonical 狀態。
4. 聚合／門檻／計數一律使用 deterministic 的 Go 程式碼。
5. 不要把 model provider 知識放進 `DecisionEngine`。
6. 避免無關的重構（**唯一例外**：Phase 0.5）。
7. 除非該 phase 明確引入遷移，否則保持向後相容。
8. 宣稱 phase 完成前先補測試。
9. 絕不因為執行不便而靜默削弱已配置的 policy。
10. 除非為了讓當前抽象正確，否則不得順手實作未來 phase 的功能。
11. 新程式碼註解引用本規格時，使用完整檔名
    `docs/architecture/decision-runtime.md §N`，不得寫 bare `spec.md`。

---

## 46. 統一測試矩陣

```text
A  向後相容      舊 config → 行為相同
B  隔離          judge A 的 prompt → 不含 judge B/C 的意見
C  Deterministic 固定意見 → 固定聚合 → 零 LLM 呼叫 → 順序無關
D  數值          越界/NaN 拒絕且不 clamp；權重正規化；tie-break 三層
E  封存          material 變 → hash 變；non-material 變 → hash 不變
F  No-Go         必要且缺 → blocked
G  Outside view  必要且缺 → blocked
H  Premortem     high-stakes 必要且缺 → blocked
I  獨立性        同雜湊/同網域的三份來源 → 一組；自述不影響分組
J  Dispersion    高 σ → 觸發 challenge；低 σ → 跳過並記錄
K  Revision      round 1 → challenge → 一輪獨立修訂 → finalize
L  Commit gate   有副作用且缺必要前提 → policy blocked → 零工具啟動
M  Stop          kill criterion 達成 → stop
N  Checkpoint    每 N 次工具呼叫評估一次；零 LLM 呼叫
O  假設失效      critical assumption 被推翻 → 舊決策 stale → replan → 舊記錄不變
P  Crash/Resume  2/3 意見 → 只 dispatch 缺少的那份
Q  副作用復原    mutation 中 crash → 先 reconcile 再重試
R  嚴格收尾      必要證據缺失 → run 不得判定為 success
S  降級          forbidden → fail closed；explicit → 依固定順序降級且留下事件
```

---

## 47. Definition of Done（V1）

> **2026-09-05 更正**：先前這裡把全部項目標記為完成，那是錯的。
> Phase 0–3 交付的是一個測試完整但**尚未接上執行路徑**的子系統：
> `NewDecisionEngine`、`ResolveDecisionProfile`、`armDiscipline` 的
> production 呼叫點都是 0。設定 `decision-profile` 會通過驗證，然後什麼
> 都不會發生。因此下列項目分為兩種狀態：
>
> ```text
> [x] 子系統與端到端都完成
> [~] 子系統完成且有 deterministic 測試，但缺少 production 輸入
> ```
>
> 2026-09-05 後續：Phase 3.5 已接線，§18.1 的三個假設狀態來源、§40 的指標
> 投影、以及未分類副作用狀態的 checkpoint 停止皆已實作。
>
> **2026-09-06 更正（第二次）**：上一行「V1 的 DoD 全數完成」在當時仍然不正確。
> 端到端測試在本次補上之後立刻證明：八個決策 stage 的 auxiliary purpose
> 只有 `decision-reference-evidence` 註冊在 §context purpose registry 中，
> 其餘七個（judge／options／challenge／premortem／revision／兩種 finalization）
> 都會在呼叫 sidecar 之前被 `unsupported context invocation purpose` 拒絕。
> 也就是說：**在 production 路徑上，任何決策都無法派出第一個 judge**，
> 而子系統的 264 個單元測試全數通過。這正是 §45 所警告的「子系統完整但
> 沒有通電」，只是這一次它藏在一層更深的地方。
>
> 缺陷已修復（purpose 常數與註冊表共用同一份清單，
> `TestEveryDecisionStagePurposeIsRegistered` 防止再次漂移），
> 並補上 §46 的端到端證明。下列項目的狀態以**具名測試**為依據，
> 不以「子系統有測試」為依據：
>
> ```text
> [x] 有 production 路徑，且有具名的端到端或整合測試
> ```
>
> §46 每一列的具名覆蓋由 `TestDecisionV1MatrixIsFullyAttributed` 維護；
> 該測試會驗證它引用的每一個測試確實存在，因此一列失去覆蓋時會失敗，
> 而不是安靜地消失。

```text
[x] 決策能力以任務區域 runtime 行為整合
[x] profile 驅動的決策嚴謹度
[x] 舊的非決策工作負載完全相容
[x] DecisionProfile 無法由 coordinator payload 設定
[x] 數值語意（尺度／正規化／缺值／determinism／dispersion）完整且有測試
[x] sealed evidence 與明確的 material 欄位集合
[x] 獨立的第一輪判斷
[x] deterministic 聚合（零 LLM 呼叫、順序無關）
[x] dispersion 追蹤
[x] outside-view 門檻
[x] no-go 替代方案門檻
[x] premortem 支援
[x] challenge 與有界修訂
[x] 持久化的 DecisionRecord
[x] typed assumptions，狀態來源明確且為 append-only（三個來源皆已接線，§18.1）
[x] evidence provenance 與可推導的獨立性分組（advisory）
[x] 執行前持久化 StopPolicy
[x] runtime 擁有的 commit gate，每個前提都可判定
[x] 前提未滿足時零副作用工具啟動
[x] deterministic 的 checkpoint 驅動 stop / replan
[x] stale decision 語意
[x] 單一預算所有者（BudgetManager）
[x] 明示且有事件記錄的預算降級（或 fail closed）
[x] crash/resume 保留決策語意
[x] 副作用 crash 先 reconcile 再重試（未分類狀態於 checkpoint 停止）
[x] adversarial verification 與 decision challenge 保持分離
[x] 記憶升級需要已驗證且有來源的證據（V1 未改動既有記憶升級路徑；§41 為約束而非新機制）
[x] 所有硬門檻都有 deterministic 測試
[x] 八個決策 stage 的 auxiliary purpose 全數註冊且不得降級
     （`TestEveryDecisionStagePurposeIsRegistered`）
[x] 自帶 fixture 的端到端證明：載入 team → 解析 profile → preflight →
     封存證據 → 獨立判斷 → 聚合 → challenge／revision → finalization → 索引
     （`TestDecisionV1StandardProfileFormsCompleteRecord` 等，見 §46 對照表）
[x] §36 宣告的每一個生命週期事件都有 production 發出點
     （`TestDeclaredDecisionEventsAreEmittedBySomeProductionPath`）；
     2026-09-07 補上 `decision_finalization_override`、`assumption_declared`、
     `replan_completed`，並移除從未被發出也無人消費的 `decision_contract_bound`
     （contract 綁定由 `request_contract_committed` 與 admission marker 記錄）
[x] arm 失敗時不留下已 armed 的 discipline
     （`TestArmDisciplineLeavesNothingArmedWhenPersistenceFails`）
[x] §44 item 9 的單檔 800 行上限，涵蓋本功能新增的每個檔案
     （`TestDecisionRuntimeFilesRespectTheSizeLimit`）
```

**V1 端到端證明的所在位置**

```text
internal/team/decision_v1_fixture_test.go   light / standard / high-stakes fixture（寫入磁碟後以 LoadTeam 載入）
internal/team/decision_fake_judge_test.go   deterministic fake judge sidecar（依 prompt 標記分派八個 stage）
internal/team/decision_v1_e2e_test.go       完整鏈路、隔離、門檻阻擋、determinism、off profile 相容
internal/team/decision_v1_matrix_test.go    §46 A–S 對照表與其具名覆蓋
internal/team/commitment_boundary_test.go   commit gate 的真實工具邊界（零 tool process）
```

---

## 48. 架構 invariants

以下強於實作便利性。

```text
48.1 決策    相同的 sealed evidence + 獨立的 judge context + deterministic 聚合
48.2 承諾    決策不蘊含執行許可；commit gate 擁有該邊界
48.3 執行    只有在假設、預算、kill criteria、recovery 狀態都允許時才能繼續
48.4 歷史    過去的決策是不可變的歷史記錄；新證據產生新狀態，而非改寫歷史
48.5 授權    能力可以為已授權的候選者排序；能力不能授予授權
48.6 驗證    consensus 不是 verification；decision challenge 不是 adversarial_verify
48.7 學習    結果品質不等於決策品質；提高 runtime 信任前必須有已驗證證據
```

---

## 49. 延後範圍（不屬目前已交付的 V1）

### 49.1 Capability-aware routing（已實作；本段只保留歷史進入條件）

Capability-aware routing is no longer an open implementation item. The
current runtime resolves authorized, capability-matched candidates for
REFERENCE/JUDGE/CHALLENGE/REVISE, records the concrete `AgentID`, reports
binding diversity, and reuses the original durable binding for REVISE. Pins
are still subject to authorization and required-capability checks.

The invariant remains:

```text
authorization eligibility → eligible candidates → capability ranking
```

Capabilities never grant authorization or tools, and an agent's self-described
skill is not treated as verified evidence. The old greenfield design notes in
this section are retained only to explain why the implementation uses the
existing registry and role runners rather than introducing a second resolver.

### 49.2 Phase 5 — Outcome learning and calibration

**延後理由**

1. 沒有結案觸發機制：hufu 是 CLI，run 結束即退出。`resolution date` 到期時
   沒有任何元件會被喚醒。這需要一個新的入口（例如
   `hufu decision resolve <id>`）與跨 run 的決策索引，兩者都不存在。
2. 樣本數不足：單一專案能解析的決策數量遠低於 Brier score 或校準分桶
   所需的量，過早導入會產生看似精確、實則無意義的數字。

**進入條件**

進入條件分兩類，性質不同，不可混為一談：

**實作前置（程式碼可滿足，已全部完成）**

```text
[x] 存在跨 run 的決策索引與明確的結案入口   §49.3
[x] 已明確定義「已驗證結果」的判定方式       VerifyOutcomeEvidence
[x] 條件本身可被量測與回報                   Phase5Entry / hufu decision stats
```

**資料前置（程式碼無法滿足，只能靠實際使用累積）**

```text
至少 30 筆已結案決策，其中 >= 20 筆 Verified == true
```

這一行**不是待實作項目**，而是一個關於世界的事實：它要求真的有 30 個決策
被做出來並結案。沒有任何程式碼能讓「已經發生過 30 次決策」成立，而偽造樣本
會讓後續算出的每一個校準數字都是假的。

可以做、也已經做了的是**量測它**：`DecisionMetrics.Phase5Entry()` 依這兩個
門檻評估目前樣本，`hufu decision stats` 每次都會回報還差多少。門檻成立那天，
Phase 5 就能開始；在那之前，依 §49 本節的規定，Phase 5 不得開始——
此處的「未完成」正是規格要求的狀態，不是缺口。

> N = 30（其中 20 筆已驗證）的依據：低於這個量時，Brier score 的信賴區間會比
> 它要用來偵測的差異還寬，算出來的校準數字看似精確、實則無意義；20 筆已驗證
> 是為了讓「已驗證/未驗證」兩組能分開看，而不是被未驗證的自述稀釋。
> 這是啟用**記錄與報表**的門檻；adaptive judge weighting 不在此門檻內，
> 它需要另一次明確決定（§41、本節 V1 行為）。

> 2026-09-05：前兩項已實作（見下方 §49.3）。第三項只能隨實際使用累積，
> 無法用程式碼滿足；在維護者確認樣本量足夠之前，Phase 5 本體仍不得開始。

### 49.3 已實作的 Phase 5 前置（不是 Phase 5 本體）

**跨 run 決策索引**：`internal/team/decision_index.go`。
每個 workspace 一份 append-only 的 `logs/decisions/index.jsonl`，
列出任何 run 形成過的決策。它是**投影**，不是事實來源——canonical 仍是
run 內的 `DecisionRecord` artifact 與 event log；索引只負責「去哪裡找」
以及「是否已結案」。後寫的列覆蓋先寫的，讀取時投影為每個決策一列；
尾端被截斷的行會被跳過，一次 crash 最多損失最後一次寫入，
不會讓先前所有決策失去定址能力。

決策 finalize 時由 `DecisionEngine` 寫入索引；**被門檻擋下的決策不入索引**
（它根本沒被做出來，也就無從結案）。

**結案入口**：`cmd/hufu/decisioncmd.go`。

```text
hufu decision list [--all]        列出待結案（或全部）決策
hufu decision show <id>           顯示單一決策與其結果
hufu decision resolve <id> --outcome <o> [--evidence <sha256>]...
                                  記錄實際結果
```

`--outcome` ∈ `succeeded | failed | mixed | superseded | unresolved`。
它描述**發生了什麼**，不是決策好壞——壞結果不必然證明當時的判斷不合理。
結案**永不修改** `DecisionRecord`：結果是獨立的
`DecisionOutcomeRecord`，且每個決策只能結案一次。

**「已驗證結果」的判定**（`VerifyOutcomeEvidence`，
`internal/team/decision_outcome.go`）：

```text
Verified = true  ⟺  outcome 引用了至少一個 artifact digest
                    且其引用的每一個 digest 都能在 workspace 的
                    content-addressed artifact store 中解析
```

- 完全沒引用證據 → 記錄下來，`Verified = false`（不是拒絕：
  知道發生了什麼但沒有證據，仍值得保存，只要下游不把它當成已證實）。
- 引用的 digest 有任一個解析不到 → `Verified = false`，並列出解析不到的 digest。
- `--resolved-by` 是 provenance，**不是** justification：操作者的宣稱
  無論多有把握都不會讓結果變成 verified。這與 §41「自述的專長不是已驗證的專長」
  是同一條規則。

**保留的設計要點**

```go
type DecisionOutcomeRecord struct {
    DecisionID       string
    ResolvedOutcome  string
    Forecast         float64
    SuccessCriteria  []string
    ObservedEvidence []ArtifactRef
    Lessons          []string
    Verified         bool
}
```

即使實作，V1 行為仍應是**記錄與報表**，不得自動套用激進的歷史 judge 加權。

```text
decision quality != outcome quality
```

好結果不證明過程健全；壞結果也不必然證明當時的判斷不合理。

---

## 50. Canonical 設計原則

```text
LLM:
  understands objective
  proposes options
  produces judgment
  discovers risk
  explains trade-offs

Runtime:
  enforces isolation
  seals evidence
  reads budget
  preserves alternatives
  aggregates deterministically
  gates commitment
  tracks assumptions
  records evidence provenance
  stops / replans by policy
  persists provenance
  verifies execution lifecycle

Scheduler / AgentPool:
  enforces authorization
  owns dispatch lifecycle
```

最終原則：

> **Plan is a hypothesis. Commitment is conditional. Evidence can invalidate both.**
