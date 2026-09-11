是的。你描述的現況代表 **v2 spec 的 Phase 0 做完也不會改變任何實際 runtime 行為**：

```text
CapabilityResolver
    ↓
AgentBinding 產生了
    ↓
但 DecisionEngine 根本沒用 binding
    ↓
仍然直接 call judge-model sidecar
```

所以真正需要改的是 **DecisionEngine → LLM 的呼叫邊界**。

## 正確的 execution path

現在大概是：

```text
REFERENCE ─┐
JUDGE ─────┤
CHALLENGE ─┼─> JudgeSidecar(model = judge-model)
REVISE ────┘
```

應改成：

```text
DecisionEngine
      │
      │ RoleInvocation
      ▼
┌───────────────────┐
│ DecisionRoleRunner │
└─────────┬─────────┘
          │
          ├─ RoleSpec
          ▼
   CapabilityResolver
          │
          ▼
      AgentBinding
          │
          ▼
   EffectiveAgentConfig
          │
          ▼
      AgentRuntime
          │
          ▼
 model + tools + skills + allowed-paths
```

關鍵是：

> **DecisionEngine 不再直接知道 `judge-model`。**

它只知道：

```go
role = reference
role = juror
role = challenger
```

---

# 1. 最重要的新 interface

我會加一層很薄的 interface：

```go
type DecisionRoleRunner interface {
    Bind(
        ctx context.Context,
        req RoleBindingRequest,
    ) ([]AgentBinding, error)

    Invoke(
        ctx context.Context,
        binding AgentBinding,
        req RoleInvocationRequest,
    ) (*RoleInvocationResult, error)
}
```

其中：

```go
type RoleBindingRequest struct {
    DecisionID string
    Stage      DecisionStage
    Role       string

    Count int

    RequiredCapabilities  []string
    PreferredCapabilities []string

    DomainTags []string
    RiskTags   []string

    Constraints RoleConstraints
    Diversity   DiversityPolicy
}
```

Invocation：

```go
type RoleInvocationRequest struct {
    DecisionID  string
    Stage       DecisionStage

    EvidencePacketRef ArtifactRef
    EvidenceHash      string

    AggregateRef *ArtifactRef
    ChallengeRef *ArtifactRef

    OutputSchema string
}
```

---

# 2. `Invoke()` 不能再呼叫 sidecar

這裡是實作成敗的核心。

錯誤：

```go
func (d *DecisionEngine) runJudge(...) {
    return d.judgeClient.Complete(
        d.cfg.JudgeModel,
        prompt,
    )
}
```

應該變成：

```go
bindings, err := d.roles.Bind(ctx, RoleBindingRequest{
    Role:  "juror",
    Count: policy.IndependentJudgments,
    RequiredCapabilities: []string{
        "decision-analysis",
    },
    PreferredCapabilities: domainCapabilities(req),
    Diversity: policy.Routing.Juror.Diversity,
})
```

然後：

```go
for _, binding := range bindings {
    result, err := d.roles.Invoke(
        ctx,
        binding,
        invocation,
    )
}
```

真正的：

```text
provider
model
tools
skills
filesystem scope
network
memory
```

全部由 `AgentBinding → AgentRuntime` 決定。

---

# 3. Concrete agent 必須經過正常 AgentRuntime

這點我會特別堅持。

不要另外做：

```text
CapabilityResolver
   ↓
找到 agent.model
   ↓
DecisionEngine 直接 call 那個 model
```

這只是把：

```text
judge-model
```

換成：

```text
selected-agent-model
```

**還是沒有真正執行 concrete agent。**

因為 concrete agent 的價值不只是 model：

```text
model
skills
tools
allowed paths
MCP tools
memory policy
system role
capabilities
provider
```

正確：

```text
AgentBinding
     ↓
base agent definition
     +
role overlay
     +
team policy
     +
task authorization
     ↓
EffectiveAgentConfig
     ↓
AgentRuntime
```

---

# 4. Effective agent 必須是 intersection

例如 resolver 選到：

```yaml
agent: kubernetes-architect

model: gpt-5.6-terra

tools:
  - view
  - grep
  - shell
  - kubectl
```

但現在 role 是：

```text
juror
```

Juror constraint：

```yaml
read-only: true
side-effect: none
forbidden-tools:
  - shell
  - kubectl
```

最後：

```text
EffectiveAgent
─────────────────
model: gpt-5.6-terra

tools:
  view
  grep
```

公式：

```text
effective capability
=
base agent
∩ team policy
∩ role overlay
∩ task authorization
```

**Role routing 不能因為選到更強 agent 就升權。**

---

# 5. 每個 stage 不應有相同 tool policy

這也是目前「全部 judge-model 無 tool」改造時容易犯的錯。

## REFERENCE

這個 stage 最需要 concrete agent routing。

```text
REFERENCE
→ evidence researcher / domain specialist
```

可以有：

```text
read files
grep
repo search
read-only web/research tool
domain-specific read-only tool
```

因為它的目的就是：

```text
收集 base rate
reference class
evidence
```

流程：

```text
reference binding
     ↓
research tools
     ↓
BaseRateEvidence
     ↓
EvidencePacket
     ↓
SEAL
```

---

# 6. JUROR 反而不應任意使用 research tools

這點很重要。

一旦：

```text
EvidencePacket sealed
```

三個 juror 應該基於：

```text
完全相同 evidence
```

做獨立判斷。

假設：

```text
Juror A 自己上網搜了資料 X
Juror B 沒搜
Juror C 搜到資料 Y
```

那就破壞：

```text
same evidence packet
```

所以第一版建議：

```text
JUROR:
  live research: false
  side effect: none

  allowed:
    sealed artifacts
    scoped read-only artifact tools
```

也就是 concrete agent 的價值主要來自：

```text
domain expertise
model
skill
reasoning behavior
historical calibration
```

而不是各自出去找不同資料。

---

# 7. CHALLENGER 可以比 Juror 稍微寬鬆

Challenger 收到：

```text
sealed evidence
+
anonymized opinions
+
aggregate
```

它可以：

```text
找 assumptions
找 countercase
做 premortem
找 falsification conditions
```

我建議 MVP 仍然：

```text
live research = false
```

但未來可以允許：

```text
challenge discovers new evidence
```

此時不能直接塞進 revision。

必須：

```text
Challenger
   ↓
new evidence candidate
   ↓
Evidence validation
   ↓
accepted?
   │
   ├─ NO → original flow
   │
   └─ YES
       ↓
   EvidencePacket v2
       ↓
   new hash
       ↓
   old opinions stale
       ↓
   restart JUDGE
```

否則 sealed evidence contract 就失去意義。

---

# 8. REVISE 不應重新 routing

這也是需要在 implementation spec 裡寫死的。

第一輪：

```text
Juror bindings:

J1 = database-agent
J2 = architecture-agent
J3 = operations-agent
```

Challenge 後：

```text
REVISE
```

應該仍然：

```text
J1'
J2'
J3'
```

即：

> **使用原本的 AgentBinding。**

不能重新跑：

```text
CapabilityResolver
```

再選三個人。

否則：

```text
before/after revision
```

就失去 paired comparison 的意義。

因此：

```go
DecisionRevision {
    OriginalOpinionID
    OriginalBindingID
}
```

---

# 9. AGGREGATE 完全不要 routing

```text
AGGREGATE
```

仍然 deterministic：

```go
aggregate(opinions)
```

不要：

```text
CapabilityResolver
→ aggregator agent
```

也不要：

```text
judge-model 做 summary → winner
```

這部分現有 spec 是對的。

---

# 10. FINALIZE 第一版也不用 capability routing

第一版建議：

```text
FINALIZE
→ deterministic DecisionRecord
```

Coordinator 最後負責把它解釋成人話。

不要急著加入：

```text
finalizer role
```

除非未來有真正需求。

所以最終只有三種 capability-routed role：

```text
REFERENCE
JUDGE
CHALLENGE
```

以及：

```text
REVISE = reuse JUDGE bindings
```

這樣最乾淨。

---

# 11. Current `judge-model` 不必立刻刪掉

可以把現有 sidecar 包成 compatibility agent：

```text
__legacy_decision_judge
```

概念：

```yaml
name: __legacy_decision_judge

model: ${judge-model}

capabilities:
  declared:
    - decision-analysis
    - adversarial-analysis
    - decision-review

tools: []

side-effect: none
```

然後 compatibility mode：

```yaml
routing-policy:
  compatibility-legacy-sidecar: true
```

Resolver 找不到其他合法 agent 時：

### legacy profile

可以：

```text
→ __legacy_decision_judge
```

### v2 strict / high-stakes

必須：

```text
routing_requirement_failed
```

不能 silent fallback。

這會讓 migration 平順很多。

---

# 12. 我會把 implementation 分成這四個 PR

### PR-1 — Role invocation boundary

先把：

```text
DecisionEngine → JudgeSidecar
```

抽成：

```text
DecisionEngine → DecisionRoleRunner
```

這個 PR **完全不改行為**。

內建一個：

```text
LegacySidecarRoleRunner
```

因此 regression 最容易控制。

---

### PR-2 — Concrete Agent Invocation

實作：

```text
AgentBinding
→ base AgentDefinition
→ role overlay
→ effective config
→ AgentRuntime
```

先讓：

```text
REFERENCE
```

真的 capability-route。

因為 Reference：

* 最需要 tools；
* 不涉及 N-way aggregation；
* 最容易驗證 concrete agent 是否真的工作。

---

### PR-3 — Juror capability routing

改：

```text
JUDGE
```

成：

```text
CapabilityResolver
→ diversity selector
→ AgentBinding[N]
→ isolated AgentRuntime invocations
```

驗證：

```text
same EvidenceHash
different bindings
no peer output leakage
```

這是 v2 真正完成的里程碑。

---

### PR-4 — Challenger + Revision

加入：

```text
CHALLENGE
→ risk-aware binding

REVISE
→ reuse original juror bindings
```

以及：

```text
binding persistence
replacement rules
resume
routing explain
```

---

# 13. Runtime state 最終應長這樣

```text
REFERENCE
   │
   ├─ ResolveRole(reference)
   │      ↓
   │   AgentBinding R1
   │
   └─ AgentRuntime(R1)
          ↓
     BaseRateEvidence
          ↓
        SEAL
          ↓
JUDGE
   │
   ├─ ResolveRole(juror, count=3)
   │
   ├─ J1: DB specialist
   ├─ J2: Architect
   └─ J3: Operations
          ↓
   isolated invoke ×3
          ↓
AGGREGATE
   deterministic code
          ↓
CHALLENGE
   │
   ├─ inspect primary risk
   ├─ ResolveRole(challenger)
   └─ Security / Reliability / ...
          ↓
REVISE
   │
   └─ reuse J1/J2/J3 bindings
          ↓
FINALIZE
   deterministic
```

---

## 最關鍵的驗收測試

我會用這個測試判斷 capability-routing 是否「真的完成」：

```text
Given:

agent A:
  model = luna
  capabilities = [decision-analysis, database]

agent B:
  model = terra
  capabilities = [decision-analysis, operations]

agent C:
  model = sol
  capabilities = [decision-analysis, architecture]

judge-model = legacy-model
```

執行 database architecture decision：

```text
JUDGE count = 3
```

期望 trace：

```text
juror-1 → agent A → luna
juror-2 → agent B → terra
juror-3 → agent C → sol
```

而且 provider trace 中：

```text
legacy judge-model calls = 0
```

這才叫：

> **Capability routing 已經接到 execution path。**

如果最後 trace 還是：

```text
judge-model
judge-model
judge-model
```

即使 `AgentBinding`、`RoleSpec`、`routing score` 全部存在，也只是 **shadow routing**，不是 capability routing。

所以你現在描述的 Hufu 狀態，精確來說就是：**v2 Phase 0 / shadow-routing-ready，但尚未進入 Phase 1 execution binding。** 下一個真正有價值的改動不是再補 schema，而是把 `DecisionEngine -> judge-model sidecar` 這條 dependency 斬掉，改成 `DecisionEngine -> DecisionRoleRunner -> AgentRuntime`。
