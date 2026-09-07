# Hufu Strategic Decision Discipline Runtime Specification
## 將《孫子兵法商學院》可實證化原則導入 Hufu Runtime

> Status: Draft implementation specification
> Target: `github.com/kjelly/hufu`
> Compatibility intent: **extend current execution semantics; do not replace scheduler/workflow/verification architecture**
> Design rule: **Runtime owns procedure; agents own judgment.**

---

# 1. Objective

把下列 10 個實證／理論支持較強的策略原則，轉成 Hufu 可執行的 runtime discipline：

```text
R1  Assess Before Commit
R2  Premortem Before High-Risk Action
R3  Define Exit Before Entry
R4  Preserve a No-Go Option
R5  Robustness Before Optimization
R6  Concentrate Comparative Advantage
R7  Replan When Assumptions Break
R8  Know Who Knows What
R9  Preserve Evidence Independence
R10 Share Objective, Preserve Dissent
```

**不要**在 runtime API、event type、CLI 或 schema 中使用「孫子」「兵法」「戰爭」等書籍語意。

這些規則應被抽象成一般化的：

- decision contract
- execution discipline
- evidence policy
- stop/replan policy
- capability routing
- dissent/isolation policy

---

# 2. Existing Hufu Architecture to Preserve

本規格假設並保留目前 Hufu 的核心結構：

```text
TaskDef
  - depends_on
  - pipeline
  - verify / verify_spec
  - max_retries
  - on_failure
  - escalate
  - adversarial_verify
  - side_effect
  - recovery
  - execution
  - phase
  - fact_refs
  - fan_out

runtime-owned workflow phases
DAG scheduler
typed TaskResult
objective verification
adversarial verification
event persistence / recovery
token / wall-clock budget
worker memory policy
deterministic fan-out
Artifact / Evidence / Acceptance
```

本規格 **不得**：

1. 再建立另一套 team-level DAG。
2. 取代 `PREPARE → AUDIT → EXECUTE → VERIFY`。
3. 把 decision loop 全寫進 coordinator prompt。
4. 讓 `team.yaml` 同時變成 workflow engine。
5. 把 `adversarial_verify` 改造成 decision formation。
6. 用 10 個「偏誤 specialist agent」取代 deterministic runtime policy。
7. 讓 LLM 算 arithmetic aggregation。
8. 把 Markdown report 當 canonical runtime state。

---

# 3. Architectural Placement

## 3.1 Team workflow 不變

```text
PREPARE
   ↓
AUDIT
   ↓
EXECUTE
   ↓
VERIFY
```

## 3.2 Decision Engine 保持 task-local sub-state machine

既有：

```text
PREPARE
  ↓
REFERENCE
  ↓
JUDGE
  ↓
AGGREGATE
  ↓
CHALLENGE
  ↓
REVISE
  ↓
FINALIZE
```

本規格不新增另一個 state machine，而是在 Decision Engine 與 task execution boundary 增加 **DisciplinePolicy**。

```text
Team Phase
   │
   └── Task
        │
        ├── optional DecisionEngine
        │     └── DisciplinePolicy
        │
        └── normal execution
              └── Execution Discipline / Stop / Replan gates
```

---

# 4. Mapping: 10 Principles → Hufu Primitives

| Rule | Runtime primitive | Enforcement |
|---|---|---|
| R1 Assess Before Commit | `RequestContract` + outside view + assumptions | hard for configured profiles |
| R2 Premortem | existing `PremortemPolicy` | profile-driven |
| R3 Define Exit Before Entry | `StopPolicy` + execution budget + checkpoints | hard |
| R4 Preserve No-Go | `AlternativesPolicy` | hard for decision tasks |
| R5 Robustness Before Optimization | `CommitGatePolicy` + side-effect/recovery/evidence | hard for mutations |
| R6 Comparative Advantage | `CapabilityResolver` / routing hint | heuristic, deterministic ranking |
| R7 Replan | typed assumptions + invalidation events + `ReplanPolicy` | hard trigger, policy-driven action |
| R8 Know Who Knows What | capability registry | runtime service |
| R9 Evidence Independence | sealed evidence + provenance `independence_group` | hard |
| R10 Preserve Dissent | context isolation + challenger + adversarial verify | hard for configured decision profile |

---

# 5. Do Not Create a Parallel "Sun Tzu" Feature

禁止：

```go
type SunTzuMode bool
type ArtOfWarPolicy struct{}
type StrategyRule string // "avoid-strong-attack-weak"
```

建議：

```go
type DisciplinePolicy struct {
    Alternatives  AlternativesPolicy  `yaml:"alternatives,omitempty"`
    Stop          StopPolicy          `yaml:"stop,omitempty"`
    Commit        CommitGatePolicy    `yaml:"commit,omitempty"`
    Replan        ReplanPolicy        `yaml:"replan,omitempty"`
    Evidence      EvidenceDiscipline  `yaml:"evidence,omitempty"`
    Routing       RoutingPolicy       `yaml:"routing,omitempty"`
}
```

這些語意應可套用到：

- software architecture
- migration
- incident remediation
- research
- procurement
- deployment
- operations
- general multi-agent decision

---

# 6. RequestContract

Hufu 已有 task `goal` / `constraints`；不要把所有目的欄位重複塞進 `TaskDef`。

建議維持 **session-scoped / request-scoped contract**：

```go
type RequestContract struct {
    RawRequest      string
    DirectQuestion  string

    Objective       string
    SuccessCriteria []SuccessCriterion
    Constraints     []Constraint
    Assumptions     []DecisionAssumption

    Confidence      float64
    Revision        uint64
}
```

規則：

```text
Question → Objective → Success Criteria → Tasks
```

首次 dispatch 應攜帶 RequestContract 的 reference，不額外增加一次 LLM round-trip。

### Validation

在需要 structured execution 的 run：

```text
Objective != ""
SuccessCriteria >= 1
Task.Goal != ""
```

Coordinator 不能只用 prose 宣稱「已理解目標」。

---

# 7. R1 — Assess Before Commit

## 7.1 Existing DecisionEngine integration

沿用：

```go
DecisionEvidencePacket
BaseRateEvidence
OutsideViewPolicy
```

不要再造另一份 `AssessmentPacket`。

新增 typed assumption：

```go
type DecisionAssumption struct {
    ID          string
    Statement   string
    Status      string // unknown, supported, contradicted, stale
    EvidenceRefs []ArtifactRef
    Critical    bool
    CheckedAt   time.Time
}
```

## 7.2 Commit condition

對 `standard` / `high-stakes` decision：

```text
question exists
options valid
criteria valid
required facts present
critical assumptions declared
outside-view requirement satisfied
```

未達成不得進 `JUDGE` / `COMMIT`。

---

# 8. R2 — Premortem

沿用既有：

```go
PremortemPolicy
PremortemResult
FailureMode
```

不要建立第二套 risk agent graph。

建議擴充 `FailureMode`：

```go
type FailureMode struct {
    ID                  string
    Description         string
    Likelihood          float64
    Impact              float64
    EarlyWarningSignals []string
    Mitigations         []string
    EvidenceRefs        []ArtifactRef
}
```

規則：

```text
premortem discovers risk
premortem does NOT automatically reject option
```

High-stakes profile：

```yaml
premortem:
  enabled: true
  required-before-commit: true
```

---

# 9. R3 — Define Exit Before Entry

## 9.1 Problem

`max_retries` / token budget / wall-clock budget 已存在，但它們主要是 runtime resource guard。

策略層還需要：

> 「什麼情況下，從決策上不應繼續？」

## 9.2 StopPolicy

```go
type StopPolicy struct {
    MaxAttempts        int           `yaml:"max-attempts,omitempty"`
    MaxToolCalls       int           `yaml:"max-tool-calls,omitempty"`
    MaxTokens          int64         `yaml:"max-tokens,omitempty"`
    MaxDuration        time.Duration `yaml:"max-duration,omitempty"`

    CheckpointEvery    int           `yaml:"checkpoint-every,omitempty"`
    RequireKillCriteria bool         `yaml:"require-kill-criteria,omitempty"`

    KillCriteria []KillCriterion `yaml:"kill-criteria,omitempty"`
}

type KillCriterion struct {
    ID          string
    Kind        string // assumption_invalid, no_progress, budget, expected_value, repeated_failure
    Threshold   float64
    Description string
}
```

### Important boundary

既有 budget 仍是 canonical resource accounting。

`StopPolicy` 不重複計數，只引用／解釋 budget state：

```text
BudgetManager owns counters.
StopPolicy decides whether current state still permits continuation.
```

## 9.3 Required rule

在 `EXECUTE` 前持久化：

```text
entry decision
budget snapshot
kill criteria
checkpoint rule
```

不得在已消耗大量資源後才臨時發明退出條件。

---

# 10. R4 — Preserve a No-Go Option

## 10.1 DecisionOption extension

```go
type DecisionOptionKind string

const (
    OptionExecute      DecisionOptionKind = "execute"
    OptionDefer        DecisionOptionKind = "defer"
    OptionNegotiate    DecisionOptionKind = "negotiate"
    OptionRequestInfo  DecisionOptionKind = "request_information"
    OptionReduceScope  DecisionOptionKind = "reduce_scope"
    OptionAbandon      DecisionOptionKind = "abandon"
    OptionCustom       DecisionOptionKind = "custom"
)
```

`DecisionOption` 加：

```go
Kind DecisionOptionKind
```

## 10.2 AlternativesPolicy

```go
type AlternativesPolicy struct {
    RequireNoActionOption bool `yaml:"require-no-action-option,omitempty"`
    RequireInfoOption     bool `yaml:"require-information-option,omitempty"`
    MinOptions            int  `yaml:"min-options,omitempty"`
}
```

### Gate

```text
require-no-action-option = true
AND no defer/abandon/no-op equivalent exists
→ DecisionEngine MUST NOT enter JUDGE
```

### Exception

如果 objective 本身是：

```text
"已經發生事故，必須立即恢復服務"
```

runtime 可以使用 explicit policy override：

```yaml
alternatives:
  require-no-action-option: false
  reason-required: true
```

override reason 必須進 event log / DecisionRecord。

---

# 11. R5 — Robustness Before Optimization

## 11.1 Reuse existing Hufu safety semantics

優先重用：

```text
side_effect
recovery
EvidenceRequirement
EvidenceManifest
blocking acceptance
ArtifactStore
verification
terminal lifecycle
```

不要再創建第二套 safety framework。

## 11.2 CommitGatePolicy

```go
type CommitGatePolicy struct {
    RequiredForSideEffects []SideEffectClass `yaml:"required-for-side-effects,omitempty"`

    RequireRollback       bool `yaml:"require-rollback,omitempty"`
    RequireReconcile      bool `yaml:"require-reconcile,omitempty"`
    RequireObservability  bool `yaml:"require-observability,omitempty"`
    RequireVerification   bool `yaml:"require-verification,omitempty"`
    RequireEvidence       bool `yaml:"require-evidence,omitempty"`

    Invariants []InvariantRequirement `yaml:"invariants,omitempty"`
}
```

對 mutation task：

```text
side_effect != none
→ policy evaluates commit gate
```

若 `recovery-policy=reconcile`，不能要求虛假的 rollback；允許：

```text
rollback OR reconcile
```

### Fail closed

在 strict profile：

```text
missing required commit prerequisite
→ policy_blocked
→ tool process MUST NOT start
```

---

# 12. R6 / R8 — Comparative Advantage + Know Who Knows What

這兩條不應塞入 DecisionEngine。

應放在 AgentPool / routing service 邊界。

## 12.1 Capability Index

```go
type CapabilityRecord struct {
    AgentID       string
    Capability    string
    Confidence    float64
    CostClass     string
    Freshness     time.Time
    EvidenceRefs  []ArtifactRef
}

type CapabilityQuery struct {
    Required []string
    Preferred []string
    RiskClass string
}

type CapabilityCandidate struct {
    AgentID      string
    Score        float64
    Explanation  []string
}

type CapabilityResolver interface {
    Resolve(ctx context.Context, q CapabilityQuery) ([]CapabilityCandidate, error)
}
```

## 12.2 Routing principle

Ranking inputs may include:

```text
capability match
historical verified outcome
cost
latency
tool access
scope authorization
freshness
```

不得：

```text
randomly assign
round-robin high-stakes specialist work
choose highest-cost model by default
infer expertise solely from self-description
```

## 12.3 Historical outcome

若未來使用 outcome memory：

```text
verified outcomes may update capability confidence
unverified self-claims must not
```

Capability metadata 必須有 provenance。

---

# 13. R7 — Replan When Assumptions Break

## 13.1 ReplanPolicy

```go
type ReplanPolicy struct {
    OnCriticalAssumptionContradicted string `yaml:"on-critical-assumption-contradicted"`
    OnEvidencePacketChanged          string `yaml:"on-evidence-packet-changed"`
    OnRepeatedFailure                string `yaml:"on-repeated-failure"`
    OnCapabilityInvalidated          string `yaml:"on-capability-invalidated"`
}
```

合法 action：

```text
continue
replan
stop
request_information
escalate
needs_human
```

## 13.2 Events

新增或映射到既有 event model：

```text
assumption.declared
assumption.supported
assumption.contradicted
assumption.stale
decision.invalidated
replan.requested
replan.completed
```

event 必須 append-only；不得覆寫過去 assumption state 造成 audit gap。

## 13.3 Evidence packet interaction

沿用既有規則：

```text
evidence packet hash changed
→ old first-round opinions stale
```

若 critical assumption 改變造成 decision input materially changed：

```text
DecisionRecord remains durable
new DecisionRevision / new DecisionRun created
```

不要原地改寫舊 DecisionRecord。

---

# 14. R9 — Preserve Evidence Independence

既有 sealed evidence / strict isolation 已處理 agent-opinion independence。

還需補 **source independence**。

## 14.1 Evidence metadata

```go
type EvidenceProvenance struct {
    SourceID          string
    SourceType        string
    ParentSourceIDs   []string
    IndependenceGroup string
    RetrievedAt       time.Time
    ContentHash       string
}
```

## 14.2 Independence policy

```go
type EvidenceIndependencePolicy struct {
    RequiredIndependentGroups int  `yaml:"required-independent-groups,omitempty"`
    RejectCircularCitation    bool `yaml:"reject-circular-citation,omitempty"`
    WarnSharedOrigin          bool `yaml:"warn-shared-origin,omitempty"`
}
```

High-impact claim：

```text
3 reports copied from same wire story
= 1 independence group
```

不是：

```text
3 independent confirmations
```

## 14.3 Aggregation

不要只存：

```text
source_count
```

至少存：

```text
source_count
independence_group_count
shared_origin_warnings
```

---

# 15. R10 — Share Objective, Preserve Dissent

沿用：

```text
sealed evidence
strict/sealed context isolation
N independent jurors
challenger
independent revision
adversarial_verify
```

責任邊界：

```text
challenger:
  decision formation 前找共同盲點

adversarial_verify:
  execution 完成後嘗試推翻「已完成/正確」的 claim
```

不要合併。

### Required rule

High-stakes：

```text
first-round jurors MUST NOT see:
- other juror opinions
- aggregate
- coordinator preference

challenger MUST see:
- sealed evidence
- anonymized opinions
- deterministic aggregate
```

Consensus 不是 verification evidence。

---

# 16. DisciplinePolicy

為避免 boolean soup，把新增策略放進 profile-scoped policy：

```go
type DisciplinePolicy struct {
    Alternatives AlternativesPolicy       `yaml:"alternatives,omitempty"`
    Stop         StopPolicy               `yaml:"stop,omitempty"`
    Commit       CommitGatePolicy         `yaml:"commit,omitempty"`
    Replan       ReplanPolicy             `yaml:"replan,omitempty"`
    Evidence     EvidenceIndependencePolicy `yaml:"evidence,omitempty"`
    Routing      RoutingPolicy            `yaml:"routing,omitempty"`
}
```

DecisionPolicy 擴充：

```go
type DecisionPolicy struct {
    // existing fields:
    IndependentJudgments int
    ContextIsolation string
    OutsideView OutsideViewPolicy
    Criteria []DecisionCriterion
    Aggregation AggregationPolicy
    Challenge ChallengePolicy
    Revision RevisionPolicy
    Premortem PremortemPolicy
    Forecast ForecastPolicy
    MaxRounds int
    MaxTokens int64

    Discipline DisciplinePolicy `yaml:"discipline,omitempty"`
}
```

---

# 17. Proposed `team.yaml` Target Schema

> 下列為 target schema；若 parser 尚未支援，必須先以 schema migration / typed config 實作，不能默默忽略 unknown fields。

```yaml
decision:
  default-profile: standard

  profiles:
    light:
      independent-judgments: 2
      context-isolation: strict
      aggregation: mean-score

      outside-view:
        required: false

      challenge:
        enabled: false

      revision:
        enabled: false

      forecast:
        required: false

      discipline:
        alternatives:
          require-no-action-option: true
          min-options: 2

        stop:
          max-attempts: 2
          require-kill-criteria: false

        evidence:
          required-independent-groups: 1

        routing:
          capability-aware: true

    standard:
      independent-judgments: 3
      context-isolation: strict
      aggregation: mean-score

      outside-view:
        required: true

      challenge:
        enabled: true
        count: 1

      revision:
        enabled: true

      premortem:
        enabled: true
        required-before-commit: false

      forecast:
        required: true

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
          on-evidence-packet-changed: replan
          on-repeated-failure: replan
          on-capability-invalidated: replan

        evidence:
          required-independent-groups: 2
          reject-circular-citation: true
          warn-shared-origin: true

        routing:
          capability-aware: true

      max-rounds: 2

    high-stakes:
      independent-judgments: 5
      context-isolation: sealed
      aggregation: mean-score

      outside-view:
        required: true

      challenge:
        enabled: true
        count: 2

      premortem:
        enabled: true
        required-before-commit: true

      revision:
        enabled: true

      forecast:
        required: true

      discipline:
        alternatives:
          require-no-action-option: true
          require-information-option: true
          min-options: 3

        stop:
          checkpoint-every: 1
          require-kill-criteria: true

        commit:
          require-rollback: false
          require-reconcile: true
          require-observability: true
          require-verification: true
          require-evidence: true

        replan:
          on-critical-assumption-contradicted: replan
          on-evidence-packet-changed: replan
          on-repeated-failure: stop
          on-capability-invalidated: replan

        evidence:
          required-independent-groups: 2
          reject-circular-citation: true
          warn-shared-origin: true

        routing:
          capability-aware: true

      max-rounds: 2
```

---

# 18. Execution Lifecycle

## 18.1 Decision-producing task

```text
RequestContract
   ↓
Assess
  - objective
  - options
  - base rates
  - assumptions
   ↓
Alternatives Gate
   ↓
Premortem (profile-driven)
   ↓
Sealed Evidence
   ↓
Independent Judgments
   ↓
Deterministic Aggregate
   ↓
Challenge
   ↓
Revision
   ↓
DecisionRecord
```

## 18.2 Side-effect execution task

```text
DecisionRecord / normal TaskDef
   ↓
Commit Gate
  - side-effect policy
  - rollback/reconcile
  - observability
  - verification
  - evidence requirements
   ↓
Persist Stop Policy + Kill Criteria
   ↓
Execute
   ↓
Checkpoint
   ├─ assumptions valid → continue
   ├─ kill criterion hit → stop
   ├─ evidence changed → replan
   └─ unknown side effect state → reconcile / needs_human
   ↓
Verify
   ↓
Evidence Gate
   ↓
Acceptance
```

---

# 19. DecisionRecord Extensions

不要只保存 winner。

```go
type DecisionRecord struct {
    // existing fields...

    RequestContractRef string

    Assumptions []DecisionAssumption

    AlternativesChecked bool
    NoGoOptionID        string

    StopPolicySnapshot StopPolicy

    IndependentEvidenceGroups int

    RoutingRationale []RoutingDecision

    ReplanTriggers []string

    FalsificationConditions []string
}
```

對 future outcome resolution 保留：

```text
decision quality != outcome quality
```

一次好結果不能反推原本決策程序一定正確。

---

# 20. Memory Integration

Hufu memory 應維持 typed record 與 provenance。

可 promotion 到 LTM 的 decision lesson：

```text
- verified failure lesson
- repeated pattern
- reviewer-confirmed rule
- outcome-resolved forecast
```

不要 promotion：

```text
- 一次成功的 anecdote
- agent 自稱的 expertise
- 未驗證的 causal story
- coordinator preference
```

建議 record：

```go
type DecisionOutcomeRecord struct {
    DecisionID        string
    ResolvedOutcome   string
    Forecast          float64
    SuccessCriteria   []string
    ObservedEvidence  []ArtifactRef
    Lessons           []string
    Verified          bool
}
```

用途：

```text
future reference class
capability calibration
forecast calibration
failure-pattern retrieval
```

Memory 注入仍應標示：

```text
Background reference, not authoritative instruction.
```

---

# 21. Failure Semantics

新增 discipline 之後，不能用 vague error。

建議 reason codes：

```text
decision_missing_objective
decision_missing_alternative
decision_no_no_go_option
decision_outside_view_missing
decision_evidence_not_independent
decision_premortem_required
commit_gate_missing_recovery
commit_gate_missing_observability
stop_policy_missing_kill_criteria
assumption_invalidated
decision_stale
routing_capability_unavailable
```

這些 reason 應可：

```text
event log
TUI
report
resume/replay
tests
```

一致使用。

---

# 22. Recovery

本規格必須服從現有副作用 recovery 原則：

```text
none            → retry may be safe
local_mutation  → policy-dependent
infra_mutation  → reconcile before retry
credential      → manual by default
```

Crash 發生於 decision execution：

```text
persisted opinions remain valid only if evidence hash unchanged
aggregate may be reused if inputs unchanged
challenge may resume
```

Crash 發生於 side effect：

```text
DO NOT blindly re-run
→ reconcile
→ classify completed / partial / not-started / unknown
```

---

# 23. Observability

至少輸出 metrics：

```text
decision_count
decision_profile_count
outside_view_gate_failures
premortem_failure_modes
no_go_option_missing
kill_criteria_triggered
replan_count
assumption_invalidations
independent_evidence_group_count
shared_origin_warnings
capability_routing_fallbacks
decision_dispersion
decision_revision_rate
```

以及 trace：

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
→ evidence
→ acceptance
```

---

# 24. Security / Safety Boundaries

1. Decision workers 預設 read-only。
2. Decision worker 不因需要研究就自動取得 shell。
3. External research tool 依 team policy 最小授權。
4. DecisionEngine 不直接知道 Ollama/OpenAI/Lemonade。
5. DecisionEngine 不自行組 shell command。
6. DecisionEngine 不自行讀 team files。
7. Commit gate 必須在 tool process start 前完成。
8. Unknown policy state 在 strict profile fail closed。
9. Evidence provenance 不可信時不得假裝成 independent evidence。
10. Secret handling 沿用全域 Redactor / SecretRef policy。

---

# 25. Implementation Plan

## Phase 0 — Typed records, no behavior change

低風險先加入：

```text
DecisionAssumption
DecisionOptionKind
EvidenceProvenance.independence_group
DecisionRecord extensions
reason codes
events
```

Acceptance:

- old team config 行為完全相同；
- unknown new enum fail validation；
- old DecisionRecord 可 migration/read；
- event replay deterministic。

## Phase 1 — Hard decision gates

加入：

```text
AlternativesPolicy
StopPolicy
CommitGatePolicy
EvidenceIndependencePolicy
```

Acceptance:

- no-go required 時缺 option 必須 block；
- kill criteria 在 execution 前持久化；
- side-effect commit gate 失敗時 tool 不啟動；
- 2 個相同 origin 的 source 不算 2 independent groups。

## Phase 2 — Replan / assumption invalidation

加入：

```text
assumption lifecycle
replan triggers
stale decision behavior
```

Acceptance:

- critical assumption contradicted 會觸發 configured action；
- old DecisionRecord 不被覆寫；
- evidence hash changed 會 stale first-round opinions。

## Phase 3 — Capability routing

加入：

```text
CapabilityResolver
verified outcome signals
routing rationale
freshness
```

Acceptance:

- unauthorized agent 永遠不因 capability score 被選中；
- stale capability 可被 invalidated；
- self-claimed capability 不可自動提高 trusted score；
- routing deterministic under fixed inputs。

## Phase 4 — Outcome calibration

加入：

```text
DecisionOutcomeRecord
forecast resolution
Brier/calibration metrics
reference-class retrieval
```

---

# 26. Test Matrix

## A. No-Go

```text
profile requires no-go
options = [A, B]
→ blocked

options = [A, B, DEFER]
→ continue
```

## B. Premortem

```text
high-stakes + premortem missing
→ blocked before commit
```

## C. Sunk-cost prevention

```text
attempts consumed > 0
remaining expected value below threshold
kill criterion hit
→ stop
```

Past consumption must not appear as positive continuation evidence.

## D. Assumption invalidation

```text
A1 critical = true
A1 supported
→ execute
new evidence contradicts A1
→ decision stale
→ replan
```

## E. Evidence independence

```text
source A
source B cites A
source C mirrors A
→ independence_group_count = 1
```

## F. Isolation

```text
juror A prompt MUST NOT contain juror B/C opinion
```

## G. Routing

```text
security task
candidate generalist score < security specialist
specialist authorized
→ specialist selected
```

## H. Commit Gate

```text
infra_mutation
recovery required
no reconcile / rollback
→ policy_blocked
→ zero tool process starts
```

## I. Crash / Resume

```text
2/3 judgments persisted
same evidence hash
→ dispatch only missing judgment
```

## J. Strict Finish

```text
required evidence missing
→ run must not be success
```

---

# 27. Non-Goals

第一版不要做：

- automatic strategy generation from Sun Tzu quotes
- 47-rule prompt injection
- 10 bias/strategy specialist agents
- LLM-controlled stop counters
- LLM arithmetic aggregation
- adaptive juror weights without historical calibration
- unlimited Delphi rounds
- automatic high-stakes classification without policy boundary
- rewriting scheduler
- replacing TaskDef
- replacing existing verification/evidence architecture

---

# 28. Definition of Done

本規格完成時，Hufu 應具備：

```text
[ ] 重大決策先 assessment，而非直接 commit
[ ] profile 可強制 premortem
[ ] 開始前持久化 stop / kill criteria
[ ] decision 可強制包含 no-go option
[ ] mutation 前有 runtime-owned commit gate
[ ] capability routing 可解釋且有 provenance
[ ] critical assumption 改變可觸發 replan
[ ] evidence 可區分 source count 與 independent group count
[ ] first-round judgments 保持隔離
[ ] challenger 與 adversarial verification 責任分離
[ ] crash/resume 不破壞上述語意
[ ] 所有 hard gate 可 deterministic test
```

---

# 29. Final Design Principle

```text
LLM:
  understands objective
  proposes options
  produces judgment
  discovers risk
  explains trade-offs

Runtime:
  enforces isolation
  counts budget
  preserves no-go
  gates commitment
  tracks assumptions
  verifies evidence independence
  stops / replans by policy
  persists provenance
  performs deterministic aggregation

Scheduler / AgentPool:
  routes work by capability
  enforces authorization
  owns execution lifecycle
```

最重要的抽象：

> **Plan is a hypothesis. Commitment is conditional. Evidence can invalidate both.**
