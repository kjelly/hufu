# Hufu Learning Runtime Gap Closure：實作紀錄

> Status: partially implemented — HF-L2L-001、HF-L2L-002 與 stale gate 已實作（`73bb7059`、`c2b4cb0d`、`bda75f5b`）；其餘 work package 經評估不做或延後（§4）
> Authority: reference（實作紀錄；現行行為以 [memory learning](../../architecture/memory-learning.md)、[memory promotion](../../architecture/memory-promotion.md) 與 [unified observability inspector](../../architecture/unified-observability-inspector.md) §6.2.1–§6.2.2 為準）
> Verified-Commit: `bda75f5b`
> Review baseline: `64304069`（原計畫）
> Supersedes: —
> Superseded-By: 上述三份 architecture 文件
> Scope: 把《Learning How to Learn》中可工程化的原則補進既有 runtime，不另建 learning subsystem
> Date: 2026-10-04

原計畫是 gitignored 的草稿 `docs/tmp/spec.md`，以 `64304069`（2026-10-02）為 baseline，分成 HF-L2L-000～007 與 PR-01～13。2026-10-04 依維護者要求改寫成符合現況的紀錄，移入 archive。原計畫的非目標（不改模型權重、不建第二套 LTM、retrieval 不算 credit、promotion 與 consolidation 維持人工審核、stale 不自動重跑有 side effect 的程序）全部維持。

## 1. 結論

| Work package | 狀態 | Commit |
|---|---|---|
| HF-L2L-000 baseline correctness audit | 不需要新工作，對應測試已存在（§3.1） | — |
| HF-L2L-001 strong-evidence recency（PR-01～04） | 已實作（§3.2） | `73bb7059` |
| §9.5／§9.6／§10.5 的 stale gate（rollout Stage C 的 recency 部分） | 已實作（§3.3） | `bda75f5b` |
| HF-L2L-002 material strategy change（PR-08～09） | 已實作，預設只警告（§3.4） | `c2b4cb0d` |
| HF-L2L-003 task-shape diversity（PR-05～07） | 不做（§4.1） | — |
| HF-L2L-004 revalidation governance（PR-10） | 延後（§4.2） | — |
| HF-L2L-005 controlled memory ablation（PR-11） | 延後，要跑 A/B 時再做（§4.3） | — |
| HF-L2L-006 structured procedural chunks（PR-12） | 不做（§4.4） | — |
| HF-L2L-007 explain／inspect／observability | 只做了 001、002 與 stale gate 需要的部分（§4.5） | — |

所有實作都在本機 `main`；改寫時尚未 push。

## 2. 評估時的資料現況（2026-10-04）

決定做或不做時，本機的真實資料如下：

- 7 個真實 workspace 的 `experience_aggregates` 全是 0；consolidation 與 promotion proposal 都是 0；沒有任何 improve experiment。
- 83 次 run 裡，repair 類 task 0 次、`recovery_hypothesis_missing` 0 次；task 失敗被判 `replan_required` 約 12 次，之後都沒有接著產生 repair task。
- 只有 hufu-dev 開了 memory learning（`observe`）。observe 模式不把 aggregate 帶進知識狀態，所以 `stale` 只會在 shadow／active 出現。

因此已實作的部分目前都看不出效果，要等 learning 開啟的 team 累積出驗證過的記憶；也因此後續項目都以「有真實資料再說」為準。

## 3. 已實作

### 3.1 HF-L2L-000：baseline 已有測試

計畫要求的覆蓋都已有對應測試，沒有另開新檔：

- retrieval-only 只加 exposure、不加 credit：`TestRetrievedMemoryDoesNotReceiveReward`
- 重複事件不重複計：`TestOutcomeReducerIsIdempotent`
- 從事件重建 aggregate：`TestAggregateCanRebuildFromRunEvents`、`TestReduceExperienceAggregatesMatchesRepositoryRebuild`
- inspect replay 的比對與 drift：`TestInspectReplayComparesMemoryAggregatesInMemory`、`TestInspectReplayDetectsMemoryAggregateDrift`
- applied + verified 的端到端信用：eval `memory-learning/applied-memory-earns-verified-credit`

### 3.2 HF-L2L-001：strong-evidence recency（`73bb7059`）

問題：每次 retrieval 都會推進 `LastObservedAt`，而 stale 以它判斷，所以常被撈到的舊知識永遠不會 stale。

實作：

- context migration 12 新增 `experience_aggregates.last_strong_evidence_at`。舊 row 是 0（未知）；唯讀開啟尚未 migrate 的 store 時讀成沒有強證據。
- `ExperienceObservation.StrongEvidence` 只在以下情況成立：
  - `verification_passed` 或 run 的 `acceptance_passed`，且 `effective_weight > 0`；
  - `causal_confidence > 0` 的負向 outcome（objective verification failure、可歸因的 rollback）。
- 不算強證據：retrieval、consulted／applied 回報、沒有 verifier 的 `task_terminal_success`、`retry_rescued`、skeptic 的 `skeptic_passed`。計畫 §7.2 把 skeptic 列為強證據；實作不採用，因為 skeptic 是模型投票，不是客觀驗證。
- SQL reducer 與 in-memory reducer 同步更新；lineage 承接的 observation 一併帶上這個標記。
- 知識狀態：`stale-after` 為 0 時不判 stale；否則以 `LastStrongEvidenceAt` 判斷，沒有紀錄的 row 維持 `assumed`，不從 `LastObservedAt` 推測。
- `hufu context outcomes` 與 `explain-memory` 顯示兩個時間；`inspect replay` 比對新欄位；eval harness 新增 `strong-evidence` 斷言。
- 排序公式不變（計畫 §7.7）。

附帶修正：event timestamp 帶奈秒，projection 只存毫秒，in-memory reducer 原本用奈秒計算，所以每個有記憶事件的真實 run 跑 `inspect replay` 都會誤報 `last_observed_at` drift。reducer 改以毫秒計算。

### 3.3 Stale gate：promotion 與 consolidation（`bda75f5b`）

對應計畫 §9.5 的「strong evidence fresh enough」、§9.6 的 reason `strong_evidence_stale` 與 §10.5 的 stale behavior，但不含 task-shape diversity。

- 共用判斷 `context.StrongEvidenceFresh`：最後一次強證據要在 `stale-after` 內（預設 30 天，0 不檢查）；沒有紀錄視為不新鮮。
- promotion：analyze 排除並回報 diagnostic `strong_evidence_stale`；approve 與 apply 每次重新檢查，過期時 proposal 轉為 `stale`，要重新 analyze。
- consolidation：create、approve、`consolidation show`、`doctor --consolidation` 與 improve handoff 都以 reason `strong_evidence_stale` 拒絕或回報。已核准的合併知識不撤銷，doctor 只回報為 stale（對應計畫「review-required diagnostic，不自動 rollback」）。
- 證據會隨時間過期而沒有任何寫入，所以建立時新鮮的 proposal 到審核時仍可能被拒。

與計畫的差異：`ConsolidationSupportPolicy` 用 `StaleAfter` 而不是 `RequireFreshEvidence bool`，門檻與知識狀態共用；CLI 路徑一律用 `agent.DefaultMemoryLearningPolicy()` 的門檻，與既有的 support 門檻一致。

### 3.4 HF-L2L-002：material strategy change（`c2b4cb0d`）

計畫假設 runtime 能辨認「`replan_required` 之後的下一個 repair／replan」。實際上 coordinator 只能用新的 todo 繼續，沒有欄位說明新 task 在重做哪個失敗的 task；`kind`、`advances`、`recovery_hypothesis` 也不在 coordinator 的 tool schema 裡，criteria 只有明確的 acceptance contract 才有。實作因此改為：

- **連結**：新 task 與一個尚未被 `reconcile_task` 取代、狀態為 error／blocked、disposition 為 `replan_required` 的 task 用同一個驗證證明成功，或推進該 task 失敗的 criterion，就視為重做它。兩者都沒有就不讀 goal 文字、不比對。範圍是所有 `replan_required`，不只 repeated fingerprint，因為這個 disposition 的 next action 本來就要求實質不同的計畫。
- **`StrategyExecutionFingerprint`**：`task_shape`、`execution_target`、`dependency_shape`、`evidence_shape`、`tool_sequence` 五個 digest，只含 identity、數量與 hash。model 宣告的 recovery strategy 只作對照、不進 digest；任一側未知的維度不能證明改變。
- **兩個時間點**：派工時（`ExecuteTasks` 綁定 identity、target 與 DAG 邊之後、建立 task 之前）比較，兩側都不含 tool 順序；替代 task 第一個完成的 attempt 再與失敗 task 最後一個完成的 attempt 比一次，這次包含 tool 順序，只作診斷。
- **receipt 新增 `tool_sequence`**：attempt 實際執行的 tool 名稱順序，最多 256 個；外部 agent provider 自己執行 tool，記為未知。
- **模式** `reliability.material-replan`：`warn`（預設）記錄並發 `loop_warning`；`enforce` 經 policy repair 退回整批派工（reason `replan_not_materially_different`）；`off` 不比對。`warn-only` 把 `enforce` 降為 `warn`；沒設定時保持空值，既有 team 設定不變。
- 事件 `strategy_change_evaluated`（phase `planned`／`executed`）、`strategy_change_rejected`；metrics `ReplanMaterialChangeAccepted`、`ReplanWithoutMaterialChange`、`ReplanMaterialChangeRejected`；`hufu inspect task` 與 `hufu report` 顯示。
- eval：`retry-recovery/replan-repeating-failed-strategy-is-flagged`（warn）與新 suite `material-replan/replan-must-change-strategy`（enforce）。

與計畫的差異：

- 計畫 §8.4 把 tool sequence 列為派工時的 material change。task 還沒跑就沒有這個資料，所以只在執行後比較；計畫的 eval `repeated-failure-material-tool-change-admitted` 無法照原意實作。
- 沒有 `strategy_fingerprint_recorded` 事件：指紋可從 durable 的 todo 與 receipt 重算，兩側 digest 都在比較事件裡。
- `repeated-failure-model-change-admitted` 改為換 agent 的 eval；`model` 欄位只在有 model list 時才給 coordinator，換模型由單元測試涵蓋。
- rollout Stage D 只做到 warn；`enforce` 是 opt-in。改成預設強制前，應先用 `strategy_change_evaluated` 累積的資料確認「沒有結構改變的替代 task」的實際失敗率。

## 4. 沒有實作的部分

### 4.1 HF-L2L-003 task-shape diversity：不做

- 計畫的 `TaskShapeID` 由 agent、驗證方式、side effect、recovery、tool 集組成，並排除 goal 文字。它分得出「誰做、怎麼驗」，分不出計畫要的「不同問題類型」。
- 會卡住升級：hufu-dev 只有帶 verify 的 task 拿得到驗證信用，試點時幾乎全來自實作 task，也就是同一個形狀。`min-independent-task-shapes: 2` 會讓程式慣例類記憶永遠升不上去。
- 「同一個 task 被算很多次」這個較大的問題，已由 occurrence identity（`1c27f839`）處理。

連帶不做：`IndependentTaskShapeCount`、`experience_observation_shapes`、`min-independent-task-shapes`、`task_shape_diversity_insufficient`、`MemoryTaskShapeCount`。要重新考慮，需要一個不讀 goal 文字也能分辨問題類型的 deterministic 訊號，以及真實資料顯示 promotion 來自重複的同一範本。

### 4.2 HF-L2L-004 revalidation governance：延後

已涵蓋的部分：passive revalidation（正常 task 用到記憶並通過客觀驗證）就是強證據更新；stale 擋 promotion 與 consolidation 已實作（§3.3）。

沒做：`context_validation_state` projection、`memory_revalidation_due|recorded|failed` 事件、`hufu context revalidate --evidence-ref`、`MemoryStaleDue` 與 `MemoryRevalidationPassed|Failed` metrics。目前沒有任何驗證過的記憶，這些只會是空轉的機制。等真實 workspace 出現 stale 的已驗證記憶、operator 需要以外部證據重新背書時再做。

### 4.3 HF-L2L-005 controlled memory ablation：延後

缺口仍然存在：`CreateCandidateSnapshot` 會拒絕內容與 baseline 相同的 candidate（`internal/improve/experiment.go`），所以只比「開記憶 vs 關記憶」時仍需要假的 team 改動；`experiment compare` 的證據限制已寫在 [improvement artifact schemas](../../reference/improvement-artifact-schemas.md)（`dd4fdfe8`）。

這是取得「記憶真的有幫助」因果證據的唯一方法，但本機沒有任何 experiment，每個 case 都要兩組真實模型 run。打算實際跑 memory A/B 時再做 `ExperimentMutation`、同 team revision 實驗與 `causal_evidence_eligible`。

### 4.4 HF-L2L-006 structured procedural chunks：不做

到目前沒有產生過任何 consolidation 或 promotion proposal，這等於替不存在的候選定格式；改變 `ContextItem.Content` 的格式也會影響全文檢索、embedding 與 token 預算。等 consolidation proposal 實際出現、且 Skill 產生確實需要結構化欄位時再評估。

### 4.5 HF-L2L-007 的剩餘部分

- 已做：explain-memory 的 `last_observed_at`／`last_strong_evidence_at`、inspect replay 比對 `last_strong_evidence_at`、inspect task 的替代 task 比較。
- 沒做：explain-memory 的 `knowledge_state`（要與 runtime 一致，必須依 mode 與衝突判斷重算一次）；`independent_task_shape_count`（隨 003 不做）；validation projection（隨 004 延後）。

### 4.6 Metrics 與 rollout

- §20 metrics：只做了 replan 的三個計數；`MemoryStrongEvidenceUpdates`、`MemoryStaleDue`、`MemoryRevalidationPassed|Failed`、`MemoryTaskShapeCount` 沒做。
- §18 rollout：Stage A 完成；Stage B 不做（003）；Stage C 只有 recency gate，沒有 diversity gate；Stage D 只有 warn，`enforce` 為 opt-in；Stage E 延後（005）。
- §19 eval matrix：memory-learning 有「只被撈到」與「驗證過」的 `strong-evidence` 斷言，「只被 consulted」由單元測試涵蓋；diversity 與 stale gate 的 eval case 沒有（stale gate 由 repository、service 與 CLI 測試涵蓋）。retry-recovery 有 same-strategy 與 prose-only 的情境，tool-change 與 model-change 見 §3.4。

## 5. 相關的前置工作

同一系列查核中較早的一版計畫（baseline `b624d27`）已處理的項目，本計畫沿用其結果：

| Commit | 內容 |
|---|---|
| `1c27f839` | 真實 run 能拿到記憶 outcome 信用；以 occurrence identity 計算 independent task |
| `640035e4` | inspect 顯示 attempt 實際使用的 execution target |
| `dd4fdfe8` | 寫明 experiment compare 的證據限制 |
| `19619bda` | promotion approve 前檢查證據 |
| `79439051` | 拒絕來源在起草期間改變的 consolidation |
| `b1d509e1` | `recovery_change_observed`：同一 task 每次重試與前一次 attempt 的比較 |
