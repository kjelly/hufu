# Hufu 建議：把「AI 時代仍需記憶」轉成 Agent Runtime 設計

> Status: archived
> Authority: reference
> Baseline: `623964ab12b067a9614615fdbf2d46eaceaa8660`
> Superseded-By: §8 semantic regression is implemented —
> [semantic regression and invariant memory](../implementation-plans/semantic-regression-and-invariant-memory.md).
> §11/§12/§14 (Internal Model Coverage, Knowledge Boundary) have a narrow
> implementation-ready follow-up spec at `docs/tmp/now/knowledge-coverage-signal.md`
> (not yet landed). §3.3/§6/§7 (memory lifecycle, promotion, evidence/demotion) are
> covered by [memory learning](../../architecture/memory-learning.md) and
> [memory promotion](../../architecture/memory-promotion.md). §9/§10 (independent,
> architecture-aware review) are covered by `.agent-teams/hufu-code-review/` and
> `internal/auditverify`. §12 (escalation matrix) is covered by
> [decision runtime](../../architecture/decision-runtime.md).
> Still untracked anywhere: §3.4/§17 item 13 (a persisted, cross-run failure-pattern
> catalog — today's `FailureSignature` is per-run only), §17 item 14 (automatic
> stale-memory demotion), §17 item 15 (cross-task reinforcement), and §5/§17 item 10
> (a concrete scope/confidence/outcome-quality retrieval-scoring formula). Retained
> for this rationale and for that remaining backlog, not as an active spec.

## 實作邊界

本文件保留長期方向與設計動機；型別、信任邊界、失敗語義、檔案範圍、PR
順序與驗收條件，以 `semantic-regression-and-invariant-memory.md` 為唯一 normative
規格。兩份文件衝突時，以該實作計畫為準。

第一版明確採用以下限制：

1. Invariant 的 canonical source 是 team 目錄內、隨 team 設定載入的
   `invariants.yaml`。LLM 或 `memory_save` 不得建立可阻擋 completion 的
   invariant。
2. Model 產生的 finding/assessment 只是 claim；只有 runtime 能依據實際注入的
   context manifest 產生 `InvariantAssessment` attestation。
3. Semantic regression 的適用方式由靜態 task contract 指定：`report` 只呈現
   被審查對象的結果，`gate` 才阻擋該 run。發現缺陷不代表 review 工作本身失敗。
4. 第一版不實作自動 invariant 推導、agent-driven promotion、AST 分析或數值化
   Internal Model Coverage。
5. 第一版只使用 `error`、`warning`、`info` 一套 severity；是否阻擋以 invariant
   的 policy severity 為準，不以模型自行填寫的 finding severity 為準。

這些限制是信任與重播契約，不是暫時性的 prompt 慣例。未來若放寬 invariant
authoring 或 promotion，必須另寫設計並維持 runtime-owned attestation 邊界。

## 目標

Barbara Oakley 的核心論點可直接轉成一個 agent architecture 原則：

> **LLM 可以是 external intelligence，但 Hufu 必須持有足夠強的 internal model，才能約束、驗證與修正 LLM。**

對 Hufu 而言，真正重要的不是「讓 agent 記更多」，而是把：

- memory
- state
- architecture knowledge
- invariants
- policy
- validation
- outcome history

變成 **runtime 可操作的結構化 internal model**。

---

# 1. 認知模型 → Hufu Runtime 對照

| 認知概念 | Hufu 對應 | 建議 |
|---|---|---|
| Working memory | active context / prompt context | 僅放當前決策真正需要的 context |
| Long-term memory | SQLite canonical memory / LTM | 保存可重用、可驗證的 knowledge |
| Chunk | reusable knowledge unit | 把成功 pattern、failure lesson 壓縮成小型可檢索單位 |
| Schema | architecture/domain model | 顯式保存 component、contract、dependency、invariant |
| Pattern recognition | retrieval + classifier | 依 task/failure/semantic risk 找相似經驗 |
| Retrieval practice | repeated recall/use | 以實際任務反覆驗證 memory 是否仍有效 |
| Automaticity | deterministic runtime | retry、fallback、budget、tool policy 不交給 LLM 臨場決定 |
| Error detection | verifier / reviewer | 對 proposal 做獨立 semantic verification |
| Independent verification | separate model/path | producer 與 validator 不共享單一路徑 |
| Cognitive offloading | tool/agent delegation | 只 offload computation，不 offload core control model |

---

# 2. Hufu 應明確區分「Generator」與「Control Plane」

建議 architectural split：

```text
                     ┌──────────────────┐
                     │      Hufu        │
                     │  Runtime / State │
                     └────────┬─────────┘
                              │
        ┌─────────────────────┼─────────────────────┐
        │                     │                     │
        ▼                     ▼                     ▼
   Canonical Memory      Policy / Invariants    Outcome History
        │                     │                     │
        └──────────────┬──────┴──────────────┬─────┘
                       ▼                     ▼
                  Context Builder        Verifier
                       │                     ▲
                       ▼                     │
                    LLM / Agent ──proposal───┘
                       │
                       ▼
                   Tool Execution
                       │
                       ▼
                 Observed Outcome
                       │
                       └────→ Runtime State
```

核心原則：

```text
model generates
runtime governs
verifier challenges
memory constrains
outcome updates state
```

LLM 不應同時負責：

- 產生方案
- 定義驗收條件
- 宣告自己成功
- 決定是否升級 memory
- 決定 fallback policy

否則會形成：

```text
producer == validator
```

---

# 3. 把 Canonical Memory 從「文字歷史」提升成「Internal Model」

Hufu 的 memory 建議至少區分以下類型。

## 3.1 Fact Memory

可驗證事實。

```yaml
type: fact
subject: project.runtime
key: default_timeout
value: 30s
source: config
confidence: high
```

用途：

- deterministic lookup
- 不需要 LLM 重建

---

## 3.2 Architecture Memory

保存系統結構。

```yaml
type: architecture
component: memory
depends_on:
  - sqlite
contracts:
  - canonical_source_of_truth
  - transactional_update
invariants:
  - promotion_must_be_idempotent
```

比自然語言摘要更重要的是可直接供 runtime / verifier 使用。

---

## 3.3 Invariant Memory

**Invariant（不變條件：任何修改後仍必須成立的系統約束）** 應成為一級實體。

例如：

```yaml
schema-version: 1
invariants:
  - id: memory-canonical-source
    statement: SQLite is the canonical memory source
    severity: error
    applies-to:
      - internal/context/
```

Agent 修改程式時，runtime 可以明確把相關 invariant 注入 review。

---

## 3.4 Failure Pattern Memory

```yaml
type: failure_pattern
signal:
  - repeated_tool_call
  - no_state_change
classification: stall
recommended_action:
  - stop_current_attempt
  - compact_context
  - change_strategy
```

這相當於 human expert 的 pattern recognition。

---

## 3.5 Outcome Memory

不要只記「做了什麼」，而要記：

```text
task
→ strategy
→ environment
→ actions
→ observed outcome
→ verifier result
→ semantic impact
```

建議資料結構：

```yaml
task_signature: ...
strategy: ...
result: success
tests: pass
review: pass
semantic_regression: false
follow_up_failures: 0
```

這才能支援真正的 outcome-driven reinforcement。

---

# 4. Context Window 應視為 Working Memory，而不是 Knowledge Base

LLM context window 應類比：

**Working memory（工作記憶：當前有限的 active information）**

而不是 long-term memory。

因此 context builder 應遵守：

```text
retrieve → compress → rank → inject
```

而不是：

```text
dump all memory → prompt
```

建議引入：

```text
Context Budget
├─ task state
├─ architecture constraints
├─ relevant invariants
├─ top failure patterns
├─ recent outcome
└─ optional historical examples
```

每一類都有 hard budget。

---

# 5. Retrieval 必須以「可驗證相關性」為目標

目前很多 agent memory 的問題是：

```text
semantic similarity
≈ relevance
```

但對 runtime 不足。

建議 retrieval ranking：

```text
score =
semantic_similarity
× scope_match
× recency_weight
× confidence
× outcome_quality
× architecture_relevance
```

並對某些 knowledge 提供 deterministic key lookup。

例如：

```text
task touches memory promotion
→ invariant lookup(memory.*)
→ architecture lookup(memory)
→ failure pattern lookup(promotion)
→ semantic retrieval(relevant outcomes)
```

這比單純 embedding top-k 更可靠。

---

# 6. Memory Promotion 應模擬「真正學會」，而不是「看過一次」

Oakley 的 retrieval-practice 概念可以轉成 memory promotion policy。

不要：

```text
agent said useful
→ promote to LTM
```

建議：

```text
observation
→ candidate memory
→ reuse
→ verifier confirms usefulness
→ reuse again
→ no contradiction
→ promote
```

也就是：

```text
promotion confidence
↑ with repeated successful retrieval/use
↓ with contradiction / stale outcome / semantic failure
```

可實作：

```yaml
promotion:
  minimum_successful_reuse: 2
  minimum_distinct_tasks: 2
  require_verifier_confirmation: true
  contradiction_penalty: high
```

這相當於把：

```text
encode
→ retrieve
→ correct
→ retrieve again
```

轉成 runtime learning loop。

---

# 7. Memory 應具備「反證與淘汰」能力

Long-term memory 不是 append-only truth。

建議 memory state：

```text
candidate
→ validated
→ promoted
→ challenged
→ superseded / deprecated
```

每條 memory 應至少有：

```yaml
confidence:
evidence_count:
successful_reuse_count:
failure_count:
last_verified_at:
supersedes:
contradictions:
```

否則 Hufu 會累積：

```text
old success
→ stale rule
→ repeatedly retrieved
→ systemic semantic regression
```

---

# 8. Semantic Regression 必須獨立於 Test Result

Oakley 論點對 coding agent 最重要的對應：

> 沒有 architecture model，就很難察覺「看起來合理但語意已變」的答案。

因此 Hufu 不應使用：

```text
tests_passed == task_success
```

建議：

```text
task_success =
tests_passed
AND contract_preserved
AND invariants_preserved
AND requested_semantics_satisfied
AND no_forbidden_scope_expansion
```

需要獨立欄位：

```yaml
validation:
  compile: pass
  unit_tests: pass
  integration_tests: pass
  contract_check: pass
  invariant_check: pass
  semantic_review: pass
```

---

# 9. 建立 Architecture / Invariant-Aware Reviewer

Reviewer 不應只拿 diff + prompt。

至少提供：

```text
Task
+
Diff
+
Affected components
+
Architecture schema
+
Applicable invariants
+
Historical regressions
+
Test evidence
```

Review prompt 也不要問：

```text
Is this code good?
```

而要問：

```text
Which documented invariants could this change violate?

Which externally observable semantics changed?

What assumptions are introduced but not validated?

Can tests pass while the original contract is broken?

Is scope expanded beyond the requested behavior?
```

---

# 10. Reviewer 與 Coder 需要真正的 Independence

若 coder / reviewer：

- 相同 context
- 相同 memory ranking
- 相同 model
- 相同 prompt framing

則容易得到 correlated errors。

建議至少提供一種 independence：

```text
different role prompt
different context selection
different model
different reasoning path
different validation tool
```

高 semantic-risk 任務可升級：

```text
Coder
→ deterministic tests
→ Reviewer A
→ architecture/invariant checker
→ optional Reviewer B
```

---

# 11. 把「Internal Model 強度」變成 Runtime 可觀測指標

可以定義：

```text
Internal Model Coverage
```

例如：

```yaml
task:
  affected_components:
    - memory
    - runtime

coverage:
  architecture_schema: 1.0
  invariants: 0.8
  known_failure_patterns: 0.6
  related_outcomes: 0.7
```

當 coverage 太低：

```text
do not increase agent autonomy
```

反而應：

```text
fetch docs
inspect code
ask verifier
expand deterministic analysis
```

而不是只把更強模型叫進來。

---

# 12. Model Escalation 不應只看 Failure

你目前考慮的：

- reviewer reject
- test failure
- scope expansion
- semantic risk

很合理。

可再加入：

```text
internal_model_coverage_low
```

升級矩陣：

| Signal | Runtime 動作 |
|---|---|
| test failure | retry / inspect |
| reviewer reject | strategy change |
| repeated failure | model escalation |
| semantic-risk high | stronger reviewer |
| scope expansion | tighten policy |
| internal model coverage low | gather evidence first |
| invariant ambiguity | block execution / inspect architecture |
| conflicting memory | resolve contradiction before action |

重點：

> **模型升級不能取代 architecture knowledge 缺失。**

如果 runtime 自己不知道 contract，再強的 LLM 也只是更有說服力地猜。

---

# 13. Tool Execution 應比 Reasoning 更 Deterministic

對應 Oakley：

```text
internalize model
externalize computation
```

Hufu 版可改成：

```text
LLM decides intent / hypothesis
Runtime controls execution semantics
```

例如：

```text
LLM:
  "run tests for memory package"

Runtime:
  maps intent
  → approved tool
  → bounded command
  → timeout
  → retry policy
  → stdout capture
  → result classification
```

不要讓 LLM 每次重新發明：

- retry
- timeout
- fallback
- cancellation
- state transition
- success criteria

這些應是 runtime automaticity。

---

# 14. 建議新增「Knowledge Boundary」概念

Hufu 可以顯式區分：

```text
Known
Unknown
Assumed
Conflicting
Stale
```

例如：

```yaml
knowledge_state:
  architecture_contract: known
  performance_requirement: unknown
  API_backward_compatibility: assumed
  memory_policy: conflicting
```

若重要資訊是 `unknown`：

```text
runtime should gather evidence
```

而不是：

```text
LLM fills gap with plausible text
```

這是降低 hallucination 對 system behavior 影響的核心。

---

# 15. 建議的 Hufu Learning Loop

```text
            ┌──────────────┐
            │     Task     │
            └──────┬───────┘
                   ▼
           Retrieve Internal Model
                   │
                   ▼
        Build Minimal Active Context
                   │
                   ▼
              Agent Proposal
                   │
                   ▼
            Tool / Code Action
                   │
                   ▼
          Deterministic Validation
                   │
                   ▼
          Semantic / Architecture
                Verification
                   │
          ┌────────┴────────┐
          │                 │
        Pass               Fail
          │                 │
          ▼                 ▼
     Record Outcome    Classify Failure
          │                 │
          ▼                 ▼
 Candidate Memory      Retry / Fallback /
          │             Escalate / HITL
          ▼
 Reuse on Future Tasks
          │
          ▼
 Repeatedly Validated?
          │
       Yes│
          ▼
       Promote
```

---

# 16. 建議增加的 Runtime Principles

可加入 Hufu architecture principles：

## Principle 1

> **External intelligence requires an internal control model.**

LLM 能力越強，runtime 越需要保存 architecture、state、policy 與 invariants。

---

## Principle 2

> **Context is working memory, not long-term memory.**

Context window 只放當下需要的資訊；canonical knowledge 應存於外部可管理 memory。

---

## Principle 3

> **Successful retrieval is stronger evidence than successful storage.**

一條 memory 被寫入沒有價值；被多次正確取用並產生成功 outcome 才有價值。

---

## Principle 4

> **Tests prove examples; invariants protect semantics.**

Test pass 不能取代 architecture / contract / invariant verification。

---

## Principle 5

> **The generator must not be the sole validator.**

Coder、planner、LLM output 都必須有 independent validation path。

---

## Principle 6

> **Unknown knowledge must remain explicit.**

不要讓 plausible generation 把 unknown 偷偷轉成 assumed fact。

---

## Principle 7

> **Repeated success promotes memory; contradiction demotes it.**

Memory lifecycle 應依 outcome 更新，而非 append-only。

---

## Principle 8

> **Deterministic mechanisms should absorb repetitive cognition.**

Retry、fallback、timeout、state transition、budget、policy 應逐步 runtime 化。

---

## Principle 9

> **Model escalation cannot compensate for missing system knowledge.**

缺 architecture / invariant 時，優先補 evidence，而不是只升級模型。

---

## Principle 10

> **Semantic regressions are first-class failures.**

即使：

```text
compile ✓
tests ✓
lint ✓
```

只要受信任 static contract 宣告為 `gate`，且 runtime-attested assessment 證明 contract /
observable semantics 被非預期改變：

```text
run_completion = rejected
```

---

# 17. 優先實作順序

## P0：直接提高可靠度

1. **Invariant memory**
2. **Architecture-aware reviewer**
3. **Semantic regression result type**
4. **Producer / verifier separation**
5. **Explicit unknown / assumption tracking**

## P1：改善 Memory

6. Memory candidate → validated → promoted lifecycle
7. Successful-reuse count
8. Contradiction tracking
9. Outcome-linked memory
10. Retrieval scoring 加入 scope / confidence / outcome quality

## P2：Runtime Intelligence

11. Internal model coverage metric
12. Coverage-aware escalation
13. Failure-pattern retrieval
14. Automatic stale-memory demotion
15. Cross-task reinforcement

---

# 18. 最值得導入 Hufu 的一句話

Human learning：

```text
External intelligence is useful
only if
internal model is strong enough to supervise it.
```

Hufu 版：

```text
LLM capability is useful
only if
runtime state + memory + invariants + verification
are strong enough to govern it.
```

最終 architectural boundary：

```text
LLM
= probabilistic proposal engine

Hufu Runtime
= deterministic control plane

Canonical Memory
= persistent internal model

Reviewer / Verifier
= independent error-detection path

Outcome System
= learning signal
```

這比「讓 agent 記更多對話」更接近真正能提升長期可靠性的 memory architecture。
