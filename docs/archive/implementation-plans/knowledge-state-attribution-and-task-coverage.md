# Knowledge-State Attribution + Task Knowledge Coverage：實作計畫

> Status: implemented and archived
> Priority: P1
> Baseline: `19b2d60b66f18062c954802c080430fd681b2a7d`
> Completed: 2026-09-14 (WP-1: `c67f12d`; WP-2: `41eaa93`; WP-3: `777c217`; completion-audit follow-up: `19e14ca`)
> Risk: medium — 新增純函式與 additive schema 欄位，不改變任何 completion/gate/retry 語意
> Normative: 本文件是 [AI 時代仍需記憶](../analyses/ai-era-memory-runtime-principles.md)
> §14（Knowledge Boundary）與 §11/§12（Internal
> Model Coverage）僅存缺口的唯一實作契約。`spec.md` 其餘章節已由既有系統覆蓋，不在本文件
> 重做（見 §0）。

## 0. 前置查核結論（為什麼只剩這兩項）

2026-09-14 對照 `spec.md` 18 節逐一核對現況：

| spec.md 章節 | 現況 |
|---|---|
| §3.3/§7 memory lifecycle、反證與淘汰 | `internal/context/model.go` 的 `ContextItem.Confidence`/`SupersededBy`/`ContextLifecycle` |
| §3.5/§6 outcome memory、promotion policy | `internal/context/experience.go` 的 `ExperienceAggregate`（`VerifiedSupportCount`/`CausalFailureCount`/`IndependentTaskCount`/`UtilityLowerBound`）+ `internal/promotion`（`docs/architecture/memory-promotion.md`） |
| §8 semantic regression 獨立於 test result | 已完成（`docs/archive/implementation-plans/semantic-regression-and-invariant-memory.md`，PR-1~5 全部落地） |
| §9/§10 architecture-aware、獨立於 producer 的 reviewer | `.agent-teams/hufu-code-review/`（coordinator/reviewer/critic + invariants.yaml）+ `internal/auditverify` |
| §12 escalation matrix | `docs/architecture/decision-runtime.md` 既有 `continue/replan/stop/request_information/escalate/needs_human` |

真正找不到既有對應的只剩：

1. §14 Known/Unknown/Assumed/Conflicting/Stale 顯式 knowledge-state；
2. §11/§12 Internal Model Coverage 指標。

`spec.md` 本身對這兩項都沒有給出可實作的資料模型或計算方式（§11 的
`architecture_schema: 1.0` 只是示意數字，沒有公式；§14 沒有說明狀態掛在哪個實體、由誰在
什麼時機轉換）。本文件把這兩項收斂成一個窄範圍、可重播、不影響現有 completion 語意的
runtime 功能；`spec.md` 其餘部分維持只是背景文件，不會被排入 archive/implementation-plans（除非
未來另有 PR 完整覆蓋，屆時應另立/更新 spec，而不是回頭對整份 `spec.md` 打勾）。

## 1. 決策摘要

第一版只實作一條純函式、可重播、不影響 completion 的窄路徑：

```text
existing ContextItem{Authority, Confidence, Freshness}
  + existing ExperienceAggregate（既有 ranking 已在讀的同一份資料，不新增查詢）
  + existing agent.MemoryLearningPolicy 門檻（MinConfirmedSupport/MinIndependentTasks，
    新增一個 StaleAfter 欄位）
    -> classifyKnowledgeState()（純函式）
    -> ContextManifestItem.KnowledgeState（additive 欄位，manifest schema 不升版）
    -> TaskKnowledgeCoverage（純函式，兩個維度：invariant coverage、outcome coverage）
    -> TaskResult 新增 report-only 欄位
    -> CLI/report/inspect 呈現
    -> 選配：hufu inspect 可對同一組已持久化資料重算比對，純粹展示用，不接 audit gate
```

關鍵邊界：

1. `KnowledgeState` 與 `TaskKnowledgeCoverage` 全部是 pure function，只讀已經持久化/已經在
   既有 ranking 路徑讀取的資料；不新增蒐證機制、不呼叫模型、不新增使用者可設定的「知識狀態」
   輸入欄位。
2. `conflicting` 是保留的 enum 值；v1 分類器**永不**輸出它。內容層級的語意衝突偵測需要獨立
   設計（見 §8），本文件不冒充已經解決。
3. Coverage 是可各自追溯的結構化計數，不是單一 0–1 綜合分數——刻意不重蹈
   `docs/architecture/memory-promotion.md` §5.3 已經拒絕過的加權公式做法（該文件因為
   「沒有可靠的 reuse_value 與 stability 資料」而放棄類似設計）。
4. 第一版只有兩個維度可信計算：invariant coverage（用既有 invariant catalog
   applicability）與 outcome coverage（用既有 `ExperienceAggregate`）。architecture-schema
   coverage 與 failure-pattern coverage 因為沒有既有結構化 catalog，v1 不做（見 §8 對兩者
   個別列出的前置需求）。
5. 全部是 report-only projection：不影響 `CompletionGate`、`EvaluateSemanticRegression`、
   retry、escalation 或 autonomy。這不是「先做 report 之後再做 gate」的兩階段設計（不像
   invariant 那樣天生有 gate 語意）——coverage 本質上是診斷信號，不是正確性判準，沒有 gate
   形態。

## 2. 範圍與非目標

### 2.1 本系列交付

- `KnowledgeState` enum 與 `classifyKnowledgeState` 純函式；
- `agent.MemoryLearningPolicy` 新增 `StaleAfter time.Duration`（沿用既有 policy 載入/驗證
  路徑，不新建 policy 型別）；
- compiler-local `internal/team.ContextItem` 新增 `Aggregate *contextstore.ExperienceAggregate`
  透傳欄位（重用既有 ranking 已抓到的同一份資料，不新增查詢）；
- `ContextManifestItem.KnowledgeState`（additive，manifest schema 版本不變）；
- `TaskKnowledgeCoverage`（`InvariantCoverage` + `OutcomeCoverage`），`TaskResult` 新增
  `KnowledgeCoverage *TaskKnowledgeCoverage` 欄位；
- `internal/team` 匯出的 `ComputeTaskKnowledgeCoverage(manifest, catalog, touchedPaths)` 純函式；
- CLI `report`/`inspect` 呈現；
- clone/redaction/replay 測試。

### 2.2 不在第一版

- 不做 `conflicting` 語意衝突偵測；
- 不做 architecture-schema coverage、failure-pattern coverage（見 §8 前置需求）；
- 不做任何 coverage 驅動的 runtime 決策（gate、retry、escalate、autonomy 內縮）；
- 不新增「遇到 unknown 就自動蒐證」的行為（fetch docs/inspect code）——本文件只負責忠實
  回報，蒐證策略是另一個需要獨立設計與風險評估的功能；
- 不新增 mandatory audit dimension，不升版 `WitnessSchemaVersion`／`AuditSchemaVersion`；
- 不改變任何既有 ordinary/invariant-verifier task 的 completion 語意；
- 不做使用者可覆寫「知識狀態」的欄位或 CLI（避免變成可以被 prompt/memory_save 操縱的
  第二個 policy 入口）。

## 3. Domain model

### 3.1 KnowledgeState

新增 `internal/team/knowledge_state.go`：

```go
package team

type KnowledgeState string

const (
    // KnowledgeKnown: 內容來自 repository authority（例如 invariant），或有足夠獨立驗證
    // 支持且未過期。
    KnowledgeKnown KnowledgeState = "known"
    // KnowledgeAssumed: 內容已 confirmed 可被注入，但尚未累積足夠獨立驗證支持。
    KnowledgeAssumed KnowledgeState = "assumed"
    // KnowledgeStale: 曾經足夠驗證支持，但最後一次觀察已超過 policy 訂的新鮮度視窗。
    KnowledgeStale KnowledgeState = "stale"
    // KnowledgeConflicting is reserved. classifyKnowledgeState never returns
    // it in this version; producing it requires an independent
    // content-contradiction design (§8) that does not yet exist.
    KnowledgeConflicting KnowledgeState = "conflicting"
)

func validKnowledgeState(s KnowledgeState) bool {
    switch s {
    case KnowledgeKnown, KnowledgeAssumed, KnowledgeStale, KnowledgeConflicting:
        return true
    default:
        return false
    }
}
```

刻意不新增 `unknown` enum 值掛在既有 item 上：manifest 裡的一個 item 一定「存在」，
「unknown」描述的是某個被要求的知識**不存在任何 item**，這是缺席而非某個 item 的屬性。
「unknown」在本文件裡只在 §4.2 的 `InvariantCoverage.UncoveredPathCount` 以計數形式呈現，
不勉強塞進這個 per-item enum；如果未來要對「完全查無資料的請求」做更細緻的分類，應在那個
計數的基礎上擴充，而不是回頭改這個 enum。

### 3.2 Classifier

```go
// classifyKnowledgeState is pure: same inputs always produce the same
// output, and it never mutates its arguments. ok is false when this
// authority is not classified at all in v1 (caller must leave
// ContextManifestItem.KnowledgeState empty rather than invent a value).
func classifyKnowledgeState(
    authority ContextAuthority,
    aggregate *contextstore.ExperienceAggregate,
    now time.Time,
    policy agent.MemoryLearningPolicy,
) (state KnowledgeState, ok bool) {
    switch authority {
    case ContextAuthorityNormative:
        // Repository-sourced / structural prompt sections are trusted by
        // construction (this already covers repository invariants, which
        // carry their own separate InvariantSeverity attribution).
        return KnowledgeKnown, true
    case ContextAuthorityExample:
        // Example/demonstration content does not assert a fact about the
        // world; it is not knowledge in the Oakley sense this feature
        // models. Left unclassified rather than forced into a state that
        // would misrepresent it.
        return "", false
    case ContextAuthorityHistorical:
        // The actual authority value carried by ranked worker/shared
        // persistent memory items (context_compiler.go:936 and the
        // reinforceSearchResults path) — this is the only case this
        // version evaluates against ExperienceAggregate evidence.
    default:
        return "", false
    }
    if aggregate == nil {
        return KnowledgeAssumed, true
    }
    if aggregate.VerifiedSupportCount < policy.MinConfirmedSupport ||
        aggregate.IndependentTaskCount < policy.MinIndependentTasks {
        return KnowledgeAssumed, true
    }
    if policy.StaleAfter > 0 && now.Sub(aggregate.LastObservedAt) > policy.StaleAfter {
        return KnowledgeStale, true
    }
    return KnowledgeKnown, true
}
```

門檻**重用**既有 `agent.DefaultMemoryLearningPolicy()` 的 `MinConfirmedSupport`/
`MinIndependentTasks`，不在這個新檔案裡另立一套常數——與 `memory-promotion.md` §5.1 的
既有原則一致（「預設 policy 的門檻來自 `agent.DefaultMemoryLearningPolicy()`，不要在
promotion package 複製另一套常數」）。

`classifyKnowledgeState` 只在 manifest builder 對「已經進入 `CompiledContext.IncludedItems`」
的 item 呼叫；被路由排除的 item（`ContextOmittedLifecycle`/`ContextOmittedExpired` 等）永遠
不會走到這裡，因為既有 eligibility 過濾已經保證能到這一步的 item 必為
`LifecycleConfirmed` 且 `SupersededBy == ""`——這正是為什麼分類器不需要重新檢查
`Lifecycle`/`SupersededBy`：它們已經是「能被選中」這件事本身的前提。

`ContextAuthority` 目前有三個值（`context_compiler.go:21-27`）：`Normative`（結構化
prompt 段落，含 repository invariant）、`Historical`（既有 ranked worker/shared persistent
memory——真正對應 Oakley 意義下的「記憶」，也是唯一一個目前就有 `ExperienceAggregate` 可查
的類別）、`Example`（示範/few-shot 內容，不是對世界的斷言）。三者對應三種處置，而不是把
「非 Normative」都當同一種：`Normative` 恆 `known`、`Historical` 才進入 aggregate-based
分類、`Example` 不分類（`ok=false`，manifest item 的 `KnowledgeState` 留空）。

### 3.3 Policy 新增欄位

`internal/agent/agent.go`：

```go
type MemoryLearningPolicy struct {
    Mode                MemoryLearningMode `yaml:"mode" json:"mode"`
    PolicyVersion       string             `yaml:"policy-version" json:"policy_version"`
    PriorAlpha          float64            `yaml:"prior-alpha" json:"prior_alpha"`
    PriorBeta           float64            `yaml:"prior-beta" json:"prior_beta"`
    UtilityPercentile   float64            `yaml:"utility-percentile" json:"utility_percentile"`
    MaxCreditPerSignal  float64            `yaml:"max-credit-per-signal" json:"max_credit_per_signal"`
    MinConfirmedSupport int                `yaml:"min-confirmed-support" json:"min_confirmed_support"`
    MinIndependentTasks int                `yaml:"min-independent-tasks" json:"min_independent_tasks"`
    MaxHarmRate         float64            `yaml:"max-harm-rate" json:"max_harm_rate"`
    // StaleAfter marks a fully-verified item KnowledgeStale once its
    // ExperienceAggregate has not been observed for this long. Zero disables
    // staleness classification (never returns KnowledgeStale), which keeps
    // absent-config teams' behavior unchanged.
    StaleAfter time.Duration `yaml:"stale-after" json:"stale_after"`
}

func DefaultMemoryLearningPolicy() MemoryLearningPolicy {
    return MemoryLearningPolicy{
        Mode: MemoryLearningOff, PolicyVersion: "memory-policy-v1",
        PriorAlpha: 1, PriorBeta: 1, UtilityPercentile: 0.10,
        MaxCreditPerSignal: 1, MinConfirmedSupport: 2,
        MinIndependentTasks: 2, MaxHarmRate: 0,
        StaleAfter: 30 * 24 * time.Hour,
    }
}
```

`internal/team/parse.go` 的 `rawMemoryLearningPolicy`、`resolveMemoryLearningPolicy`、
`validateMemoryLearningPolicy` 依既有模式新增 `stale-after`（YAML duration 字串，例如
`"720h"`；空值時沿用 default，不得為負數）。這是既有 policy 載入路徑的加欄位，不是新
config 表面。

## 4. Manifest attribution 與 Task coverage

### 4.1 ContextManifestItem.KnowledgeState

`internal/team/context_manifest.go`：

```go
type ContextManifestItem struct {
    ID                string                `json:"id"`
    Kind              string                `json:"kind"`
    Source            string                `json:"source,omitempty"`
    Included          bool                  `json:"included"`
    Reason            ContextDecisionReason `json:"reason"`
    Tokens            int                   `json:"tokens"`
    Compressed        bool                  `json:"compressed,omitempty"`
    BaseScore         float64               `json:"base_score,omitempty"`
    FinalScore        float64               `json:"final_score,omitempty"`
    DisclosureLevel   string                `json:"disclosure_level,omitempty"`
    ContentHash       string                `json:"content_hash,omitempty"`
    InvariantSeverity InvariantSeverity     `json:"invariant_severity,omitempty"`
    // KnowledgeState is populated only when classifyKnowledgeState returns
    // ok=true for an Included item (currently: Normative and Historical
    // authority). Empty for every other item — Example-authority items,
    // omitted items, invariant items (which keep their own
    // InvariantSeverity attribution), and legacy replayed manifests — never
    // inferred retroactively.
    KnowledgeState KnowledgeState `json:"knowledge_state,omitempty"`
}
```

不升版 `ContextManifestSchemaVersion`：這個欄位是純粹 additive 的診斷屬性，不像 invariant
attestation 需要嚴格版本相等性檢查才能保證 trust boundary；舊回放的 v1/v2 manifest 這個
欄位天生是空字串，消費端一律當作「這份 manifest 建立時本功能尚未存在」，不得回填猜測值。

`BuildContextInjectionManifest` 在既有 `appendCompiled` 迴圈裡，對
`included && item.Kind != string(contextstore.ContextInvariant)` 的 item 呼叫
`classifyKnowledgeState(item.Authority, item.Aggregate, createdAt, policy)`；只在回傳
`ok=true` 時把 `state` 寫進 `manifestItem.KnowledgeState`，`ok=false`（目前是
`ContextAuthorityExample`）就維持空字串。`policy` 由呼叫端（既有取得
`agent.MemoryLearningPolicy` 的路徑，例如 `LoadAdoptedMemoryPolicy`/session 既有欄位）傳入，
不在這個函式內部重新載入。

compiler-local `internal/team.ContextItem`（`context_compiler.go:50`）新增：

```go
Aggregate *contextstore.ExperienceAggregate
```

在既有 `reinforceSearchResults`/`rankSharedPersistentMemory` 已經呼叫
`repo.ExperienceAggregate(ctx, result.Item.ID, policy.PolicyVersion)` 的地方，把取得的值直接
帶進對應的編譯後 item，manifest builder 端不得為同一個 item 重新查詢一次。沒有走過這條
ranking 路徑的 item（結構化 prompt 段落、invariant）維持 `Aggregate == nil`，不影響它們
既有的 `known`（Normative）或 invariant 專用屬性。

### 4.2 TaskKnowledgeCoverage

`internal/team/knowledge_state.go` 新增：

```go
type InvariantCoverageSignal struct {
    TouchedPathCount         int `json:"touched_path_count"`
    ApplicableInvariantCount int `json:"applicable_invariant_count"`
    // UncoveredPathCount counts touched paths matched by zero catalog
    // invariant — the structural form of "unknown" this version supports:
    // not a per-item state, but an explicit count of requested surface area
    // with no documented invariant at all.
    UncoveredPathCount int `json:"uncovered_path_count"`
}

type OutcomeCoverageSignal struct {
    IncludedItemCount int `json:"included_item_count"`
    KnownCount        int `json:"known_count"`
    AssumedCount      int `json:"assumed_count"`
    StaleCount        int `json:"stale_count"`
}

type TaskKnowledgeCoverage struct {
    InvariantCoverage InvariantCoverageSignal `json:"invariant_coverage"`
    OutcomeCoverage   OutcomeCoverageSignal   `json:"outcome_coverage"`
}
```

刻意不提供單一 0–1 分數欄位；每個數字都可以回頭對到 manifest 裡具體的 item 或 touched
path，符合既有 `ExperienceAggregate` 「揭露可追溯 metrics，而非捏造 composite confidence」
的原則（`memory-promotion.md` §5.3）。

```go
// ComputeTaskKnowledgeCoverage is pure: it only reads the already-persisted
// manifest and catalog; it performs no I/O and calls no model.
func ComputeTaskKnowledgeCoverage(
    manifest *ContextInjectionManifest,
    catalog []InvariantDefinition,
    touchedPaths []string,
) TaskKnowledgeCoverage
```

計算規則：

- `OutcomeCoverage`：掃 `manifest.Items`，對 `Included && KnowledgeState != ""` 的 item 累加
  `KnownCount`/`AssumedCount`/`StaleCount`；`IncludedItemCount` 是這個子集的總數。
  `conflicting` 不出現在 v1，不需要對應的計數欄位膨脹本結構（保留 enum 是為了未來擴充時
  這個結構也要跟著加一個 `ConflictingCount`，屆時一併處理）。
- `InvariantCoverage`：`TouchedPathCount = len(touchedPaths)`；對每個 touched path，用既有
  `invariantAppliesToTouchedPaths` 對整個 catalog 判斷是否至少一筆 applicable；
  `ApplicableInvariantCount` 是「至少對一個 touched path 適用」的 catalog invariant 去重計數；
  `UncoveredPathCount` 是零筆 applicable invariant 的 touched path 數。`touchedPaths` 為空時
  （既有 §5.2 fail-safe 語意：空值視為全部 applicable）全部欄位為 0，不視為
  「完全沒被覆蓋」——這與既有 invariant selection 的 fail-safe 規則一致，避免這個新指標
  自相矛盾地把「沒有限定範圍」誤報成「毫無知識」。

`TaskResult` 新增：

```go
// KnowledgeCoverage is a runtime-computed, report-only diagnostic. It never
// participates in CompletionGate, EvaluateSemanticRegression, retry, or
// escalation decisions. Absent (nil) for any task whose persisted context
// manifest predates this feature or carries no included memory/invariant
// items worth reporting.
KnowledgeCoverage *TaskKnowledgeCoverage `json:"knowledge_coverage,omitempty"`
```

計算時機：與既有 `attestInvariantClaims` 同一個呼叫點（`storeSubmittedTaskResult`/
`beginTaskResultSubmission` 前），對**每一個**有 `ModelCalled=true` context manifest 的 task
呼叫（不限定 invariant-verifier task）；`touchedPaths` 對非 workset-bound task 傳空 slice。
失敗（例如 manifest 缺失）不得讓 task 失敗——這是純粹的盡力而為診斷欄位，計算錯誤只記
`nil` 並可選擇性記一則低嚴重度 log，不得升級成 execution failure、不得走
`withFailureClassOverride`。這與 invariant attestation 刻意相反：attestation 失敗必須
fail-closed，coverage 計算失敗必須 fail-open（因為它不是 trust boundary，是診斷信號）。

## 5. Persistence、replay 與 clone

不新增 event type 或 SQLite migration。透過既有 `TypedResult`/`ContextInjectionManifest` JSON
持久化：

- `cloneTaskResult` 深拷貝 `KnowledgeCoverage`（值型別，直接複製即可，內部沒有 slice/pointer
  需要額外處理，除非未來擴充；現在只需確認 clone 沒有共享底層資料——目前欄位全是基本型別，
  複製 struct 即安全）；
- `ContextManifestItem` clone 路徑（既有 clone 已逐欄複製，新增欄位需要一併確認）；
- 舊 JSON 缺少新欄位時 `KnowledgeState`/`KnowledgeCoverage` 為空值，不得回填或推測。

## 6. CLI / report / inspect

- `cmd/hufu/report.go`：task 區塊新增（存在時才顯示）knowledge coverage 摘要行，例如
  `knowledge: 5 known, 2 assumed, 0 stale · invariants: 3/3 paths covered`；
- `internal/inspect/evidence.go`：既有 per-task 投影新增對應唯讀欄位；
- 不新增獨立子命令；沿用既有 `report`/`inspect` 的 per-task 視圖。

## 7. 選配：獨立重算（非 mandatory audit dimension）

`internal/auditverify` **不**新增 mandatory dimension、不升版 witness/audit schema。因為
coverage 不影響 completion，把它塞進 audit 的 pass/fail 語意只會製造一個「看起來像 gate
但其實不是」的混淆欄位。改為：

- `internal/team` 匯出 `ComputeTaskKnowledgeCoverage` 供 `internal/auditverify`（或未來的
  `hufu inspect`）在需要時對同一組已持久化 `ContextInjectionManifest` 重算，並可选择性
  比對 `TaskResult.KnowledgeCoverage` 是否一致（純粹展示「這個數字是否可從 event 重新推導」，
  不是新的 `AUDIT-*` failure code）；
- 若要在未來把 coverage 升級成真正的 audit dimension，需要另外走一次完整設計（含
  witness schema 升版與 mandatory dimension 決策），本文件不預先承諾。

## 8. 明確排除項的前置需求（回答「如果要做，卡在哪裡」）

### 8.1 Conflicting 狀態

需要先有：

1. 一個「兩個 confirmed item 在同一 scope 內談的是同一件事，但內容彼此矛盾」的**判定演算法**
   ——目前 `ConflictKey`（`context_compiler.go`）只解決結構化 prompt 段落的「誰是唯一版本」
   （precedence），不解決「兩份記憶內容互相矛盾」這個語意問題；
2. 判定演算法要嘛是 deterministic（例如同一個 dedup key 下出現兩個 `LifecycleConfirmed`
   且 `SupersededBy==""` 的相異 content hash——這是可以做的窄範圍規則，但目前的 promotion/
   supersede 流程理論上會讓一份取代另一份，需要先確認是否真的存在「兩份同時 confirmed 卻
   互斥」的資料狀態，還是這個狀態在現有寫入路徑下根本不會發生），要嘛需要模型參與語意比對
   （非 deterministic，需要獨立的可信度與誤報率設計）。
3. 在演算法明確前，任何「conflicting」輸出都只是猜測；本文件選擇保留 enum 值但不猜。

### 8.2 Architecture-schema coverage

需要先有一個結構化的「component → contract/dependency/invariant」登記表——目前
`docs/architecture/*.md` 是給人看的 prose，不是可查詢的資料。`spec.md` §3.2 給的
`architecture memory` YAML 範例目前沒有任何對應的 runtime 型別或 repository；要先有這樣
一個 catalog（性質上很像本文件用的 invariant catalog，但描述的是 component/contract 而不是
不變條件），coverage 才有分母可以算。

### 8.3 Failure-pattern coverage

需要先有一個持久化的 failure-pattern catalog。目前 `FailureSignature`/`Signature()`
（`internal/team/phase.go`、`execution_events.go`）只是單次 run 內用於 retry 去重與
anti-thrashing 的**瞬時**識別碼，沒有跨 run 累積、也沒有「建議動作」欄位（spec.md §3.4 的
`recommended_action`）。要做 coverage，需要先把它提升成一個像 `ExperienceAggregate` 一樣
可查詢、可累積的持久化型別——這本身就是一個獨立、不窄的功能，不適合塞進本文件。

## 9. 完整檔案範圍

新增：

```text
internal/team/knowledge_state.go
internal/team/knowledge_state_test.go
```

修改：

```text
internal/agent/agent.go
internal/team/parse.go
internal/team/context_compiler.go
internal/team/context_manifest.go
internal/team/memory_ranking.go
internal/team/task_result.go
internal/team/task_occurrence.go
internal/team/coordinator_task_run.go
cmd/hufu/report.go
internal/inspect/evidence.go
```

## 10. WP 順序

### WP-1 — KnowledgeState 分類 + manifest attribution（無 task-level coverage）

- `KnowledgeState` enum、`classifyKnowledgeState`；
- `MemoryLearningPolicy.StaleAfter` + parse/validate；
- compiler item 新增 `Aggregate` 透傳欄位，`memory_ranking.go` 填值；
- `ContextManifestItem.KnowledgeState` 填值（manifest schema 不升版）；
- clone/replay/legacy-manifest 測試。

### WP-2 — TaskKnowledgeCoverage

- `InvariantCoverageSignal`/`OutcomeCoverageSignal`/`TaskKnowledgeCoverage`；
- `ComputeTaskKnowledgeCoverage` 純函式；
- `TaskResult.KnowledgeCoverage` 填值時機（與 attestation 同一呼叫點，fail-open）；
- clone 測試、legacy `TaskResult` 相容測試。

### WP-3 — CLI/report/inspect + 選配重算

- `cmd/hufu/report.go`、`internal/inspect/evidence.go` 呈現；
- `internal/auditverify` 選配的重算/比對 helper（非 mandatory、非 gate）；
- 文件：`docs/architecture/` 新增一篇簡短的參考頁（比照 `run-outcome.md` 的簡潔程度），
  說明這是診斷用途、明確不是 completion 判準。

WP 之間沒有 PR-3/PR-4 那種「不得平行實作」的順序限制，因為沒有 trust boundary 或 gate
語意需要保護；但 WP-2 依賴 WP-1 的欄位，WP-3 依賴 WP-2 的欄位。

## 11. 測試矩陣

### Classifier

- `Authority == ContextAuthorityNormative` → `(known, true)`，忽略 aggregate；
- `Authority == ContextAuthorityExample` → `("", false)`，忽略 aggregate；
- `Authority` 為未知/未來新增值 → `("", false)`（fail-safe：寧可不分類，不得猜測）；
- `Authority == ContextAuthorityHistorical` 且 `aggregate == nil` → `(assumed, true)`；
- 同上但低於門檻 → `(assumed, true)`；達門檻但 `StaleAfter <= 0`（未設定）→ `(known, true)`，
  永不 `stale`；
- 同上且達門檻且超過 `StaleAfter` → `(stale, true)`；
- 同上且達門檻且未超過 → `(known, true)`；
- 函式對相同輸入永遠回傳相同輸出（無隱藏狀態）。

### Manifest attribution

- Included 的 invariant item 不設 `KnowledgeState`（維持既有 `InvariantSeverity` 專用路徑）；
- Included 的 `ContextAuthorityExample` item 一律 `KnowledgeState == ""`；
- Omitted item 一律 `KnowledgeState == ""`；
- 舊（本功能之前建立）manifest 回放時 `KnowledgeState` 全空，不回填；
- `Aggregate` 透傳不引發第二次 repository 查詢（以 fake repository 計數呼叫次數驗證）。

### Coverage

- 空 `touchedPaths` → `InvariantCoverage` 全零，不視為 uncovered；
- 每個 touched path 都被至少一個 invariant 覆蓋 → `UncoveredPathCount == 0`；
- 部分 touched path 無 catalog 對應 → 精確計數落在 `UncoveredPathCount`；
- `OutcomeCoverage` 計數與 manifest 裡逐一手算的 `KnowledgeState` 分佈一致；
- manifest 缺失或損毀 → `KnowledgeCoverage` 為 `nil`，task 本身不失敗、不重試、不改變
  `TaskResult.Findings`/completion 结果；
- 對已完成 invariant-verifier task（既有 semantic-regression 測試 fixture）疊加本功能，
  確認 `EvaluateSemanticRegression`/`HasBlockingInvariantAssessment` 行為完全不變。

### Clone/replay

- `cloneTaskResult`、`ContextManifestItem` clone 對新欄位 mutation-safe；
- crash-resume/branch checkout 對新欄位的既有測試（沿用既有 fixture 擴充，不新建一整套
  fixture 機制）。

## 12. Validation gate

```bash
go test ./...
go vet ./...
golangci-lint run
```

不需要額外的 `-race` 專門要求（沒有新增共享可變狀態或並行寫入路徑）；沿用既有套件測試的
race 設定即可。

## 13. 完成定義

1. `KnowledgeState`/`TaskKnowledgeCoverage` 只讀既有持久化資料，不新增查詢成本（每個
   included item 至多一次既有的 `ExperienceAggregate` 讀取，且是複用而非新增）；
2. 沒有任何 completion/gate/retry/escalation 路徑讀取這兩個新欄位；
3. 舊 team/run/manifest/report 資料回放時新欄位安全地為空，行為不變；
4. `conflicting`、architecture-schema coverage、failure-pattern coverage 明確未實作，且
   §8 列出的前置需求對任何想繼續做的人是可執行的下一步，而不是重新從零查一次現況；
5. 三個 validation gate 全數通過。

可對外描述為：

```text
Hufu attaches a structural, deterministic knowledge-state to every injected
memory item (known/assumed/stale, reusing existing promotion evidence and
authority signals) and reports two coverage dimensions per task (invariant
coverage, outcome coverage) as diagnostics. Neither participates in
completion; both are fully reproducible from already-persisted context
manifests.
```
