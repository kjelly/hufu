# 有界唯讀診斷 belief loop（提案）

> Status: draft
> Authority: normative
> Verified-Commit: `66293fafd8fbf208fc40d3be7320135e5045db96`
> Supersedes: —
> Superseded-By: —

本文件是 canonical 設計規格。階段 0–1 已提供離線 fixture/baseline、純計算與
準備契約；階段 2 已提供離線 fixture episode 的 journal/replay/唯讀 inspect 與 shadow。
真實執行中的 episode、runtime shadow、跨任務證據 admission 與 executor 尚未實作。
目前只能準備 `mode: off` 設定，不能啟用診斷派送。原始 PR #19 僅新增文件。
核對基準是 PR18 合併後的上述 `main`。以下「現況」是程式碼已存在的行為；
階段 0–2 已交付的名稱與邊界見第 7 節；其餘「提案」中的型別、事件、設定與驗收
案例仍是後續實作契約。

目標是讓一個小型、可重播的 episode 完成：

```text
客觀 evidence → 有限離散 belief → 經授權的診斷決策
             → 唯讀 action → 客觀 observation → 重算 belief → 下一輪或停止
```

先證明 identity、授權、停止與恢復正確，再評估選擇策略。此範圍不包含 factorized
belief、因果圖、線上訓練、自動修復、自由 shell、模型自行新增 hypothesis/action，
也不把 posterior 當作任務成功、權限或修復授權。

## 1. 已實作事實與不可變邊界

| 現況（已核對程式碼） | 對提案的限制 |
| --- | --- |
| [Go function: compileTeamBelief](../../internal/team/belief_config.go) 載入 bounded model，選定 verification experiment，並綁定可選 policy | 原有設定仍只支援既有 receipt adapter；不能偷偷改成多步模式 |
| [Go function: BeliefVerificationEpoch](../../internal/team/belief_source.go) 把 receipt identity 放進 epoch ID；subject 是 `RunInputSnapshotHash`，environment 是 verifier contract、workdir、target 等的摘要 | 此 identity 並非完整 source tree 或作業系統快照；不能拿相同輸入字串證明環境沒變 |
| [Go function: admitBeliefReceipt](../../internal/team/coordinator_belief.go) 為一個 receipt 開 epoch、admit、close | 後一個 receipt 不繼承前一個 posterior |
| [Go function: beliefVerificationOutcome](../../internal/team/belief_source.go) 只有 exit 0、非 timeout、非 overturned 為 `pass`，其餘 verifier result 為 `fail` | 保留 v1；新 adapter 必須把 timeout 與無效 evidence 分開處理 |
| [Go function: AdmitVerification](../../internal/team/belief_journal.go) 與 [Go function: applyBeliefLifecycle](../../internal/team/belief_events.go) 驗證 task/attempt、source、revision 與選定 experiment | 現有 API 不是跨 task 的 episode admission API；不可削弱 v1 驗證來接受新資料 |
| [Go function: Project](../../internal/belief/projector.go) 從原 prior 與有效 observations 重算；相同 source 或 correlation group 的不同 observation 產生 `dependence_unresolved` | 不把 rounded posterior 當新 prior，也不把重跑當成獨立樣本 |
| [Go function: SuggestPolicy](../../internal/team/belief_suggestion_journal.go) 綁定 snapshot/generation，透過 `ConditionalEventJournal` fence 全域 journal head | advisory generation 與 episode decision generation 分離；建議永遠不派送 |
| [action catalog](../../internal/team/team_action_dispatch.go)、[proposal](../../internal/team/team_action_proposal.go)、[binding](../../internal/team/team_action_binding.go) 已有 catalog/grant/schema/phase/budget 與 invocation identity | controller 必須走同一 admission/dispatch 路徑，不直接呼叫 provider |

原有 `max_posterior_v1` 只建議最高 Q9 hypothesis 對應的 candidate，不估算資訊增益、
成本或正確率。`belief.policy-file` 的 `mode: advisory` 不改；原本拒絕的 `mode: active`
繼續拒絕。既有功能詳見 [belief reference](../reference/belief-state.md)。

## 2. 單一 pilot：離線測試失敗分類 fixture

提案先使用 maintainer-authored 的離線 fixture provider。它讀取不可變、已登錄的
fixture bundle，針對一個失敗測試事件執行兩種有限檢查：

- `check_source`：以固定輸入向量檢查 fixture 函式輸出是否符合預期。
- `check_environment`：檢查 fixture 的 runtime manifest 是否符合 pinned requirement。

這不是在任意使用者專案執行 `go test`。一般測試能寫檔、連網或跑任意程式，不能只靠
`side-effect: none` 宣告就稱為唯讀。本 pilot provider 只解讀受限 fixture 格式、
做純函式比較，不執行 bundle 內程式、不載入 plugin、不連網、不讀取 host credentials。
只允許控制區內 runtime journal/artifact 的正常寫入；subject snapshot 不可修改。
fixture、domain schema 與 provider 屬於 team-owned 測試資源；core 不判斷 team 名稱。

選此 domain 是因為真值與輸出可離線重現，能刻意製造失敗、timeout、相關證據與版本
變動，且不需機率準確率承諾即可驗證完整 runtime 路徑。timeout 使用受控測試 hook；
正常 provider 不做睡眠或無界工作。將來支援真實測試執行需另立 isolation 與模型校準
設計，不在本規格暗中擴大範圍。

模型 universe 僅限 manifest 明確宣告的三種、互斥且完備的 fixture 狀態：
`source_fault`、`environment_fault`、`other_fault`。前兩者為單一故障；雙重故障與
超出此 fixture universe 的輸入回報 `out_of_scope`，不是硬塞進低機率 hypothesis。
`other_fault` 是具名的第三種 fixture 故障，不代表所有真實世界未知原因。
起始失敗事件是 episode eligibility，不納入 likelihood；prior 明確條件於這個
「已失敗且符合 fixture universe」的母體，避免把失敗選樣重複算兩次。

### 2.1 可重現 fixture 格式

提案的 strict JSON bundle 固定 `schema_version: 1`、`fixture_id`、`source_vectors`
（最多 8 組 `{input, expected, actual}`，值為有界整數）、`environment`
（`required_version` 與 `observed_version`，各最多 32 bytes）、`failed_test_receipt_ref`
與 `scope: single_fault_fixture_v1`。拒絕 unknown fields、symlink、外部路徑與大於
64 KiB 的 bundle。`check_source` 比較所有 actual/expected；`check_environment` 比較
兩個 version；相等為 pass，否則 fail。這是測試輸出記錄的診斷，不執行任意 source。
latent truth 放在 harness 的獨立 oracle，不傳給 controller/provider。

固定 golden fixtures 用一組向量 `input=1, expected=2`：source fail 時 `actual=3`，
pass 時 `actual=2`；environment fail 時 required/observed 為 `v1`/`v2`，pass 時都為
`v1`。完整 paired synthetic corpus 可枚舉每個 latent hypothesis 下的
`seed_source, seed_environment ∈ [0,99]`：各 seed 小於對應 fail 百分比時產生 fail
bundle。再以 prior 比例 5:3:2 複製各 hypothesis 的完整 seed 格點，得到 100,000 個
加權案例；oracle/truth 僅由 harness 保存。兩個 seeds 獨立枚舉，使 synthetic likelihood
與 independence 假設可精確驗證，不必依賴隨機函式庫版本。故障注入 corpus 另列，
不混入這個機率生成母體；雙 probe fail 不等同雙重 latent fault，可能是有雜訊的測量。

輸出 strict typed evidence 至少包含 `fixture_ref`、`experiment_id`、`outcome`、
`measurement_digest`；runtime 重新驗證 artifact identity，objective verifier 重算比較
並核對該 outcome。provider 自報的 digest 或 outcome 不直接可信。版本與 seed manifest
亦保存為 immutable artifact，schema/generator 改變須更換 revision。

### 2.2 固定的 synthetic model 與 provenance

先驗與 likelihood 都標記 `synthetic: true`、`source: fixture_design_v1`，是手工 engine
測試數字，不是量測結果，不能宣稱診斷正確率、信心校準或實際節省。兩個 experiment
使用不同 correlation group；條件獨立僅是此 synthetic fixture generator 的假設。
模型既有 `prior_source`、`independence_basis`、`applicability` 記錄來源與限制；
額外 generator/version/seed 規則存在綁定的 provenance artifact，不能向 strict v1
model schema 隨意加欄位。experiment 的 `source` 保持 `verification`，由新版 adapter
驗證 typed measurement，而非改成未支援的 source enum。

| Hypothesis | Prior | P(`check_source` fail) | P(`check_environment` fail) |
| --- | ---: | ---: | ---: |
| `source_fault` | 0.50 | 0.90 | 0.10 |
| `environment_fault` | 0.30 | 0.10 | 0.90 |
| `other_fault` | 0.20 | 0.20 | 0.20 |

每欄的 pass likelihood 為 1 減 fail；落地 model 使用既有 1,000,000,000 integer units。
每個 hypothesis 的完整 outcome likelihood 加總為 scale，prior 也加總為 scale。
`timeout`、`unknown`、`invalid` 不在二元 likelihood 裡，不能被映射為 fail。
精確計算仍用既有 projector，展示才量化 Q9。例：source fail 後權重為
`(0.45, 0.03, 0.04)`；再 environment pass 後為 `(0.405, 0.003, 0.032)`，
正規化後 source 約 0.92045。此例只測算術與 admission，不是真實準確率證據。

任何 prior、likelihood、independence、generator、provider 或 verifier 版本變更都換
artifact digest；不在進行中的 episode 更新。正式 domain 必須另外提供代表性樣本、
保留集、混淆矩陣、機率校準與分布轉移評估；這些是將來工作的必要條件而非本 pilot 成果。

## 3. 提案契約：episode、revision 與 evidence

新增獨立的 team 設定概念 `diagnostic-loop`，缺省為 off；唯一執行模式為
`bounded_read_only`，另提供不派送的 `shadow`。本段不是現行可貼上的 YAML。
設定須列出固定 model/policy/adapter artifacts、允許的 catalog IDs、明確 actor、
啟用來源與以下硬上限。不可由 model prose 或既有 advisory recommendation 自動開啟。
同一 scope 不能同時由 v1 receipt adapter 與 episode adapter 消費成同一條 belief stream；
若兩者皆開，各有不同 adapter/epoch namespace，episode 不讀取 v1 posterior。

### 3.1 `DiagnosticEpisodeV1`（提案）

| 欄位 | 契約 |
| --- | --- |
| `schema_version`, `adapter_version`, `reducer_version` | 固定版本；不支援就 unavailable 並停止 |
| `episode_id`, `session_id`, `branch_id`, `run_id`, `owner_task_id`, `owner_occurrence` | runtime 產生並保存；episode owner 為 durable 診斷控制 task，不冒充 child action task |
| `subject_id`, `subject_revision` | 失敗 fixture 的邏輯 ID 與完整 canonical bundle content digest |
| `source_revision` | fixture source snapshot digest；含被檢查檔案、manifest、輸入向量；Git SHA 僅輔助標籤 |
| `environment_revision` | immutable environment manifest、provider executable/source identity、verifier/adapter schema、受允許環境輸入與執行 target 的 digest |
| `model_ref`, `policy_ref`, `catalog_digest`, `execution_policy_digest` | 既有 immutable artifacts 與 frozen policy；不從現行 team 檔補歷史 |
| `seed_source` | 初始客觀 failure event/hash/receipt/artifact references，作 eligibility 證據 |
| `opened_at`, `deadline`, `limits`, `reserved`, `consumed` | durable 絕對 deadline 與成本帳；重啟不能重置 |
| `state`, `generation`, `head_cursor`, `stop_reason` | 有限狀態與 fence；終止 append-only，不原地 reopen |

固定來源為 immutable snapshot，禁止 live dirty tree；建立 snapshot 前後驗證一致性，
無法保證原子讀取就拒絕。episode 不宣稱已 fingerprint 整台 host；其適用範圍僅是
provider 確實使用的 pinned inputs。未列入 manifest 的環境依賴一律拒絕或 `out_of_scope`。
artifact 缺失、摘要不符、配置漂移不能用空字串或現在的檔案補足。

同一 episode 的每個 child receipt 必須符合上述全部 identity，且屬於該 episode
已持久化的 decision 與 child occurrence。subject/source/environment 任何一項變動都
停止 `revision_changed`；不自動開新 episode，不搬運 posterior。由使用者明確重新啟動
才建新 ID、重新從 prior 開始；run 的既有總預算仍不能歸零。

### 3.2 `DiagnosticObservationV1`（提案）

observation 記錄 `episode_id`、`decision_id`、`execution_id`、child task/occurrence、
`ActionInvocationID`、receipt key、terminal event ID/hash/cursor、experiment/outcome、
artifact refs/digests、三種 revision、`correlation_group` 與 adapter version。
`observation_id = H(branch_id, episode_id, execution_id, experiment_id)`；此處與下文
`H` 均為 domain-separated canonical serialization 的 content hash，不能拼接含糊字串。

admission 必須依序驗證：

1. 完整 journal hash chain 與 branch lineage；source 在可見 cursor 內且早於 admission。
2. decision 已提交、執行 mapping 唯一；receipt 對應確切 child occurrence 與 invocation。
3. frozen catalog/grants/provider/verifier identity 與 revisions 全部符合。
4. artifact 存在、hash 正確、typed result schema 與 objective verifier 通過。
5. adapter 根據 typed objective result 產生 `pass` 或 `fail`，而非 worker prose。
6. 去重與 independence preflight 後，journal 原子提交 admission 與更新游標，再重算分布。

檢查結果 `fail` 是被檢查 fixture 的結果；收集 action 仍須成功完成其 transport/schema/
artifact verifier。若收集 action 自己的 verifier 失敗，停止 `evidence_invalid`，不把它
當成 fixture fail。新 adapter 讀受驗證的 typed outcome，而非重用 v1 的
`beliefVerificationOutcome`。timeout/cancelled/provider failure、missing result、
unknown outcome 均不更新分布；保存拒絕原因後停止。

相同 observation ID 與相同內容為 no-op，不增加 round/generation/cost；同 ID 不同
內容視為 integrity conflict。receipt alias、重送 event 或同 artifact 的兩種 wrapper
不產生新 source。另以 canonical underlying evidence digest（probe ID、受檢資料與結果的摘要，
不是共用 bundle 的摘要）作 source alias index，
不同 execution ID 也不能重複計數同一 evidence。不同 observations 共用 source 或
correlation group 時，保存衝突記錄、分布標示 unavailable/null 並停止
`dependence_unresolved`；不選最有利樣本、不自行平均或假裝獨立。
撤回 append tombstone；有效 evidence 集合改變就從 original prior 重算。若已終止，
可顯示歷史分布被撤回後不可用，但不得因此復活 episode 或派送動作。

### 3.3 v1 與新 adapter 的隔離

新增 versioned episode event envelope、source validator 與 projection 分支，重用
`belief.Project`、artifact service、journal/replay infrastructure。保留 `belief.Scope`
為 owner task/occurrence 的計算 scope；child receipt mapping 存在 episode envelope，
不得偽造 child receipt 的 `TaskID` 來通過 v1 validator。episode events 使用新 type
namespace（建議 `diagnostic_episode_*`），不把跨 task 資料寫成 v1
`belief_observation_admitted`。新版 reducer 必須先完整驗證 mapping 才把標準
`belief.Observation` 送給純 projector。這是新 admission 能力，不是放寬既有 journal。

## 4. 決策、授權、停止與 baseline

每輪從有效 state、未執行 experiment、frozen catalog、當前授權與剩餘預算建立候選集。
每種 experiment 最多執行一次，序列執行、最多一個 outstanding action。pilot 不用 LLM
選參數；typed fixture ref 與 experiment ID 由固定 mapping 填入，不接受命令字串。

選擇策略提案為 `expected_gini_gain_per_cost_v1`：對每個候選 action，以模型算
`P(o)=Σ p(h)L(o|h)`、`G(p)=1−Σp(h)^2`，評分
`[G(p)−Σ P(o)G(p|o)] / declared_cost_units`。P(o)=0 的項略過；全部為零則
`model_inconsistent`。使用 deterministic rational arithmetic；比較有理數，不依賴
浮點誤差；同分依 catalog ID 字典序。每個 candidate 保存分數分子/分母、成本及排除
原因。成本為正整數 synthetic work units，兩個 pilot action 各 1，不是金額。
既有 `max_posterior_v1` 不改寫成此策略。首輪 golden score 為
`check_source = 7009/31200`、`check_environment = 5521/28800`，故選 source。

baseline 為 `fixed_order_v1`：`check_environment` 然後 `check_source`，略過已做或
不合法項。使用完全相同 prior、fixture、授權、budget、stop rule、adapter 與 executor；
唯一變因是候選排序。此模型任一單次 pass/fail 都不會到達 0.90；正常且資源
充足時兩種策略都執行兩次、消耗 2 units。預期成本差為零，順序不同而最終有效
evidence 集合與 posterior 相同，這也是必須 assert 的 golden baseline。它不是繞過授權的 fallback：策略無法算出分數就停止，不偷換
baseline。shadow 只記錄「會選什麼」，不消耗 execution quota；bounded mode 才可能派送。

### 4.1 權限與執行路徑

opt-in 表示允許 controller 在所列界限內提出執行要求，不提供新權限。必須分別滿足
catalog discover/propose grant、catalog invocation/proposal 要求、coordinator dispatch
權限、agent/capability/phase、unattended、tool denial 與 artifact path policy。
若 catalog 要求 proposal，必須有真正被授權 actor/occurrence 產生的 durable proposal；
controller 不冒充 source event actor、不補造 grant 或偽造模型呼叫。pilot 預設選用不要求
額外 proposal 的 catalog entry；其他 entry 缺 proposal 就 blocked。

實作將 catalog dispatch 的共用 admission service 抽出給 controller 與現有 tool 使用，
不繞過 `CatalogActionBinding` 或 `validateCatalogActionIntegrity`。既有 catalog
action 強制 `DecisionProfileOff` 並拒絕 enabled profile override；episode controller
不改此規則，也不透過 active DecisionPrimitive profile 派送 catalog action。decision commit 前、
child task admission 時及實際 provider 啟動前都重驗授權、identity 與 budget ledger。
新的 reservation 檢查未保留額度；已保留的 action 則核對自己的有效 reservation，
不得再次扣額或因該 reservation 已把可用額度降為零而拒絕啟動（詳見下一節）。
無法取得有效授權資訊就 `authorization_unavailable`；deny 就 `unauthorized_action`。
可用候選被明確 deny 時停止，而非透過替代 action 試探權限。
`side-effect: none` 只是必要 metadata；pilot provider 的唯讀實作與 path 邊界還需測試。

### 4.2 有限性與終止優先序

pilot 硬上限：2 輪、2 次 action attempts（含不確定 dispatch）、每 action 5 秒、
episode 20 秒（含重啟停機時間）、總成本 2 units、無額外模型 calls、最多 32 個 episode
事件、每 observation payload 16 KiB。不得調高 runtime/團隊原有更嚴格的上限。
artifact/output 大小另受既有 runtime 限制。兩輪之間不重新給 token/cost/execution quota。
持久化 reservation 在執行前扣除可用額度；`remaining = limit − consumed − reserved`。
reservation 以 execution ID 綁定該 action 的 attempt/cost，從 reserved 結算至 consumed
不再次扣額。失敗/timeout/不確定執行也佔 attempt 與成本；只有可證明尚未 dispatch 的
取消能釋放 reservation，且不能抹除 audit history。

區分「不能再保留新的工作」與「已授權工作的 reservation 失效」：remaining 為零只
禁止新的 reservation/dispatch，不阻止已持有有效 reservation 的 action 啟動、完成或
admit 最後一份有效 evidence。provider-start 必須找到自己的 execution ID reservation、
核對未重複執行、額度足夠且未失效；不能拿同一 action 的成本再與 remaining 比較。
此規則同樣適用共用 runtime ledger；不能透過 episode reservation 繞過更嚴格的 runtime
限制。reservation 缺失、額度不足、撤銷或 ledger 不一致立即停止；授權撤回、revision
變動、disabled/cancellation、deadline 到期等硬限制也不因已有 reservation 而豁免。

執行前與 evidence admission 前先檢查硬限制，再在有效 reservation 範圍內完成執行或
admission。不得因額度恰好用完，先把 episode 設成 terminal 而遺失已授權工作的最後
observation。每次有效 observation durable admit 並重算後，依下列順序決定是否停止
（第一個 reason 為主，其餘同時成立的原因保留供 audit）：

1. integrity/artifact/revision/authorization/policy/version 不符，或 reservation/ledger
   無效：blocked；失效證據不得更新 belief。
2. cancellation、disabled、deadline 或 runtime 強制中止：停止；這與僅 remaining=0
   不同，後者不是取消有效 reservation 的理由。
3. unknown/invalid/timeout evidence、correlation unresolved、分布 unavailable：停止。
4. 至少一個有效新 observation 且最大 posterior ≥ 0.90：`threshold_reached`。
5. 連續一個已完成 round 沒有新增有效 evidence 或分布 Q9 未變：`no_progress`。
6. 已完成兩輪：`round_limit`。
7. 未達以上條件、但 runtime/episode 剩餘可用額度不足以保留下一個 action：
   `budget_exhausted`；不建立下一份 reservation。
8. 無未使用且合法候選或最佳 gain ≤ 0：分別 `no_candidate`、`no_information_gain`。

第 7 項以合法候選的實際宣告成本檢查；沒有候選則走第 8 項，不捏造成本。初始 ready
且沒有 pending reservation 時也先檢查硬限制，再套用適用的選擇/停止規則，確保零額度
不會派送。恢復 pending action 則先按 execution ID 核對既有 reservation，不套用「下一個
action」的額度條件。故正常第二輪耗完 2 units 後，達門檻者以 `threshold_reached` 結束，
未達門檻且有進展者以 `round_limit` 結束；`budget_exhausted` 可作次要 audit reason，
兩者都不會產生第三次 dispatch。較小預算在第一輪後耗完且未達其他停止條件時，則以
`budget_exhausted` 結束。

deadline 包含驗證與 bookkeeping；到期不新增 dispatch，取消並 drain 已啟動工作。
收尾事件預留容量；達事件上限時只能提交 terminal 記錄。fence 衝突最多重算 3 次，
也受 deadline 約束，否則 `concurrent_change`。duplicate delivery 不觸發下一輪；
重試輸入流也不能使 deadline 延後。系統時鐘倒退或 deadline 無法可信重建時 fail closed。

`threshold_reached` 只代表 synthetic 模型下已到停止門檻。它不證明找到根因，更不代表
原始失敗任務成功；episode completion、execution status、belief status 三者分開顯示。

## 5. Durable identity、crash recovery 與競爭

狀態機提案：`opened → ready → decision_committed → dispatch_intent → running →
observed → ready`，任何非 terminal 狀態都可到 `stopped`；`observed` 必須已 durable
admit 才能進下一輪。generation 只在新的有效 decision input 發生時增加；其 namespace
獨立於既有 advisory `decision_generation` 與 active decision engine。

`decision_id = H(branch_id, episode_id, generation, state_digest, policy_digest)`；
`execution_id = H(decision_id, catalog_entry_hash, arguments_hash)`。decision event 固定
選擇、所有候選評分、source cursor/hash、revision、budget reservation 與 authorization
snapshot。durable mapping 把 execution ID 連到既有 task ID、occurrence 與
`ActionInvocationID`；重啟查 mapping，不重造新 invocation ID。

使用現有 `runtime.sqlite` event journal 為 authority，新增 versioned payload；episode
index 如需增加只是可重建 projection。decision+reservation 同一 event 原子 append；
dispatch intent+child mapping 的持久化必須具備唯一鍵與原子 admission。若跨現有 task
transition 邊界，採先持久化唯一 intent，再讓 canonical task creation 以 execution ID
冪等 materialize；必須補齊 pending intent 查詢與 replay，不能用「先 dispatch 再補 event」。
這項 atomic bridge 尚不存在，需在第 3 階段實作並驗收後才啟用 executor。

| crash 時點 | 恢復契約 |
| --- | --- |
| decision commit 前 | 無 intent 無執行；重讀 state/head 與 budget 後重算 |
| decision 已 commit、尚無 dispatch intent | 沿用 decision ID；重新授權與檢查 deadline，合法才建立唯一 intent |
| intent 已落盤、child task 尚未建立 | 依 execution ID 冪等完成 canonical task admission；不增加第二份 reservation |
| task 已建、provider 啟動是否發生不明 | 由既有 recovery 查 receipt/task journal；不能證明未執行則停止 `execution_uncertain`，pilot 不自動重跑 |
| result 已回傳但 terminal receipt 尚未 durable | 不採信記憶體結果；若無可驗證的 durable receipt，停止 uncertain |
| terminal receipt durable、observation 尚未 admit | 重建 observation 並去重 admission；絕不重呼 provider/verifier |
| admission durable、projection/cache 尚未更新 | 依原 prior 重播；budget/generation 不重置 |
| observation 已更新、下一 decision 尚未 commit | 重新檢查所有 stop rules 後提交下一 decision |
| stop 已 commit | 只讀重建，禁止恢復派送 |

不承諾 external exactly-once execution。可保證的是 durable intent/mapping 唯一、
evidence admission 冪等、已知完成的動作不重播；外部程序啟動與 SQLite commit 間仍有
不確定窗口。pilot 選擇停止而非冒險重跑。唯讀不代表可免費無限重試。

### 5.1 Lock、fence 與 branch

重用 [DecisionWriterLease](../../internal/team/decision_writer_lease.go) 的 branch
跨程序 lease；同 branch 只允許一個 episode writer，不新增 TTL 搶佔。若 host 已持有
此 lease，傳遞 ownership，不遞迴取得。in-process episode mutex 只序列化短小 state
準備；不在 `taskProjectionMu` 或 `beliefMu` 下執行 provider、artifact I/O 或等網路。
使用既有 [conditional journal](../../internal/team/event_store_conditional.go) 的 head
CAS；snapshot 讀完後 head 變動，必須重新讀取、重算及檢查，不能只替換 expected head。

建議鎖順序：workspace/session ownership → branch writer lease → episode state mutex
→ 短期 journal transaction；釋放 episode mutex 後才呼叫 task transition 或 dispatch。
同 branch 的其他 task 仍能 append，因此 lease 不能取代 head fence。執行回報需再次驗證
occurrence fence；遵循 transition commit 後才取得 occurrence lease 的現有順序。
停止/關閉時取消並 drain 已啟動 goroutine，再關 journal；不安排下一輪。

所有 idempotency key 包含 branch ID。fork/checkout 不繼承執行中的 episode 或 budget
reservation 為可執行權；祖先事件僅用於 read-only history。child 明確開始新 episode，
以自己的 branch/ID/prior/budget ledger 開始；seed 可引用 fork cursor 以前且重新授權的
祖先 evidence。禁止 sibling source、fork cursor 以後的 ancestor event 與跨 branch
receipt mapping。舊 branch 的遲到結果不能改寫新 branch。總 run/工作區限制仍由既有
owner 管理；fork 不能作為繞過全域額度的方式。

## 6. Audit、inspect 與相容性

擴充既有 `hufu inspect beliefs` 的 text/JSON 投影，新增 versioned `episodes` 區段；
CLI 拼字維持既有命令，新欄位需文件化。至少展示：

- mode/opt-in 來源、synthetic 標記、model/policy/adapter/reducer digest；
- episode scope、三種 revisions、branch/cursor、source receipt 與 artifact reference；
- 每輪 prior/input digest、有效/拒絕/duplicate/retracted observation 與 correlation；
- 各候選 score/cost/排除原因、decision/execution/task/occurrence/invocation mapping；
- budget reserved/consumed/remaining、deadline、fence conflict、recovery disposition；
- execution status、belief distribution/status、terminal reason 與 validation availability。

不展示原始 worker/verifier output、命令、工作目錄、秘密或未授權 artifact 內容。
正規化/遮罩在既有 receipt canonicalization 時完成，不在保存或重播時再次改寫 receipt
bytes。read-only inspect 不修復、不遷移 DB、不讀 current team 檔、不執行 action。
artifact 缺失時保留歷史 choice 與 source references，標示 validation unavailable；
不能顯示為當前可用的建議。JSON unknown distribution 用 null，不以零或 uniform 代替。

本文件不需要資料遷移。未來實作新增 event schema/version 與可選 projection table 時，
由顯式 runtime upgrade 路徑執行 additive migration，先備份與測試 rollback；inspect 不做
migration。舊 event、舊 epoch、既有未啟用設定的 policy hash 與 replay 結果須維持相同。
未知 episode event 的舊 binary 不可 resume 執行；新 binary 對不支援版本也 fail closed。
不要求舊 binary 能解讀新 episode，發布時需標示最低可讀版本。

disable 的控制先禁止新 dispatch，再取消/drain 現有 action，持久化 `disabled`；可保存
已完成的 evidence 但不再選下一步。關閉不刪 artifacts/events，不退款已發生的成本，
不修改原任務結果。設定模式改變會觸發 policy drift，須新 session；獨立 stop/disable
控制可停止既有 episode 而不修改 frozen policy。降版前停止所有 episodes、保留 journal
與 artifacts，只在相容版本做 inspect；禁止刪事件或重設 workspace 以掩蓋不相容。

## 7. 實作順序與具體 integration points

階段 0–1 的實作與使用方式見 [準備契約與離線驗證](../reference/diagnostic-preparation.md)。
fixture/provider/schema 固定放在 `.agent-teams/hufu-dev/diagnostic-pilot/`。
`diagnostic-loop` 的 `mode` 目前必須明寫 `off`；`shadow` 與 `bounded_read_only`
均 fail closed。model/policy/adapter 的語義 digest、actor 與 catalog digest 綁入
既有 execution policy snapshot；該設定本身不建立 episode。
階段 2 的離線 harness 另以 `diagnostic_episode_opened/shadowed/observed` schema v1
事件與 `diagnostic_fixture` immutable artifact 保存 synthetic model/policy/revisions/
measurements。它不使用假的 receipt、child task 或 dispatch intent，也不呼叫 provider。
所有決策以凍結 inputs 重算，inspect 沿用 canonical branch lineage，缺 artifact 時
保留歷史引用並顯示 unavailable/null。具體使用方式見上述 reference。
證據檢查只驗證 typed mapping/revisions，不代替未來 journal/receipt/artifact 授權。
階段 2 目前只涵蓋離線 fixture；階段 3–5 仍未實作。直到第 4 階段才允許
`bounded_read_only` 的顯式 opt-in。
既有 `belief` advisory 不因任一階段自動升級。

| 階段 | 修改位置與工作 | 必須通過的 acceptance |
| --- | --- | --- |
| 0：fixture 與 baseline | team-owned fixture/provider/schema、offline eval；沿用 [action provider](../../internal/team/action_provider.go) 邊界 | 固定 seed/output 可重現；沒有網路/host reads/subject writes；列出 synthetic 數字來源；baseline golden trace |
| 1：契約與純計算 | [belief projector](../../internal/belief/projector.go)、[belief config](../../internal/team/belief_config.go)、[policy snapshot](../../internal/team/execution_policy_snapshot.go)；新增 episode config/model/policy artifact binding 與 source validator | 嚴格 schema/enum/size、revision、Q9、unknown/correlation/retraction 測試；舊設定/hash golden 不變；新模式還不能 dispatch |
| 2：journal/replay/inspect（離線範圍已實作） | [episode journal](../../internal/team/diagnostic_journal.go)、[events/replay](../../internal/team/diagnostic_events.go)、[frozen fixture](../../internal/team/diagnostic_fixture.go)、[inspect](../../cmd/hufu/inspect_beliefs.go)；shadow 僅限 team-owned 離線 fixture | observation 去重、fork isolation、缺損 artifact、每個離線 commit 邊界與 lost acknowledgment 重播；全域 head CAS；inspect 無寫入；shadow 與 inspect replay digest 一致；不宣稱真實 cross-task admission |
| 3：授權決策與 durable bridge | [dispatch](../../internal/team/team_action_dispatch.go)、[proposal](../../internal/team/team_action_proposal.go)、[binding](../../internal/team/team_action_binding.go)、[budget](../../internal/team/budget_manager.go)、[conditional journal](../../internal/team/event_store_conditional.go)；真實 evidence admission 與 runtime shadow | 共用 admission 未繞過 grants/phase/denial；cross-task receipt mapping 與 provenance；decision/reservation/intent/mapping 唯一；同 branch 競爭、head drift、restart 不重置 budget；實際 dispatch crash gaps 與第 4 階段整合驗證；仍預設不執行 |
| 4：bounded executor | [coordinator belief](../../internal/team/coordinator_belief.go) 外獨立 controller、[runtime actions](../../internal/team/runtime_workflow_actions.go)、[recovery](../../internal/team/recovery.go)、[task transitions](../../internal/team/coordinator_eventstore.go) | 完整下列矩陣、race/CGO-free/replay；停止時 drain；action count 可驗證；明確 opt-in 才可執行 |
| 5：比較與發佈審查 | offline eval artifacts、inspect/reference/guide、設定驗證與最低版本文件 | paired baseline 報告、無準確率宣稱、disable/upgrade 測試；未達條件維持 shadow/off |

每個 runtime 階段需要單元、整合與故障注入測試，以及 repository 的 `go test ./...`、
`go vet ./...`、`golangci-lint run` 和 CI gates。不能因 pilot 很小略過 canonical
transition、task journal、session persistence、CLI/JSON/report/TUI 的投影傳播。
原始文件 PR 只驗證 docs lifecycle/links、diff hygiene 與既有 PR CI；階段 0–1 的
與第 2 階段離線範圍的實作另須通過上列 code gates，不能宣稱已跑 runtime shadow、
dispatch bridge 與第 3–5 階段尚未實作的測試。

## 8. 端到端驗收矩陣

測試 harness 使用假的 clock、bounded deterministic provider 與呼叫計數器；真實 journal
使用暫存 SQLite 與 immutable artifact store，重啟另開 process/handle。assert 不只看
最後文字，還核對 event chain、receipt identity、budget、task status 與 replay digest。

| Fixture / 故障注入 | 預期結果與必須斷言 |
| --- | --- |
| source fail → environment pass | 第二個 action 使用最後 reservation；兩份 evidence 均 admit，posterior 約 0.92045，主原因 `threshold_reached`，budget exhaustion 為次要原因；不改原失敗 task、不派送第三次 |
| source pass → environment fail | 第二份 evidence admit 後 environment posterior 為 `243/280`（約 0.86786），主原因 `round_limit`，budget exhaustion 為次要原因；不為提高 confidence 派送第三次 |
| 兩項 pass | `other_fault` 可上升但未達門檻也停止；沒有自動修復 |
| action timeout / cancellation | receipt 與 timeout 分開可見；不當 fail likelihood；無下一 action，已用額度保留 |
| duplicate event / receipt alias / duplicate admission | 同 observation 只生效一次；digest/generation/cost 不變；不觸發新 dispatch |
| 不同 wrapper 共用 source / 相同 correlation group | unavailable/null、`dependence_unresolved`，沒有虛假 confidence 增加 |
| 相同 observation ID 不同 outcome | integrity error、停止；不 last-write-wins |
| source、subject 或 environment digest 改變 | admission 或 dispatch 前拒絕；舊 epoch 不繼承；顯式新 episode 從 prior 開始 |
| 缺 model/policy/fixture/result artifact 或 hash 不符 | unavailable/blocked；保留 references；不讀現行 team file 代替 |
| unauthorized catalog、缺 proposal、denied tool、phase/unattended 拒絕 | provider 呼叫數 0，標準 gate error 可追蹤，無替代路徑 |
| budget 0、只餘 1 unit（沒有 pending reservation） | 分別 0 或至多 1 次 action；最後有效 evidence 仍 admit，未達門檻/其他優先條件才 `budget_exhausted`；restart/fork 不補額度 |
| 最後 1 unit 已 reserve，provider 尚未啟動或此時重啟 | remaining=0 仍以自己的有效 reservation 啟動一次；不 double-charge、不建立第二份 mapping；reserved→consumed 後總額不變 |
| 最後 action 結果已 durable、admission 前重啟 | 不重跑 action；最後 evidence admit 一次並依上述 threshold/round priority 停止；零新 reservation、零第三次 dispatch |
| 最後 reservation 後撤權、revision 改變或 deadline 到期 | reservation 不豁免硬 fence；尚未啟動則呼叫數 0，已執行則拒絕失效 evidence、取消/drain；不得以 threshold 覆蓋硬停止 |
| event capacity | 預留最後 admission/terminal 所需容量，不能保留無法完成 durable bookkeeping 的 action；達上限停止，不新增派送 |
| unknown/out-of-scope、schema invalid、收集 verifier fail | 無 posterior update，停止且標示具體 evidence failure |
| 合法 observation 使 Q9 不變、gain 0、候選耗盡 | 分別 no_progress/no_information_gain/no_candidate，有限步結束 |
| decision/intent/task/result/admission/update 各邊界 kill | 逐列符合第 5 節表；最多一筆 mapping/admission；uncertain 停止，沒有 exactly-once 假設 |
| 兩程序同 branch、無關 task append、撤權/配置變動 | lease 排他；head CAS stale decision 不 dispatch；有界重算或停止；沒有持鎖等待 provider |
| fork、sibling evidence、舊 worker 遲到 | branch keys 不碰撞；越界 evidence 拒絕；不在 child resume ancestor action |
| 撤回後重播、terminal 後遲到 event | tombstone 生效、不復活、不派送；歷史結果仍可稽核 |
| off、原有 advisory、shadow、disabled restart | off/advisory/shadow provider 次數為 0；v1 golden 不變；disabled 不續跑 |
| paired baseline 比較 | 同一 fixture/seed 的 observation streams 與規則，僅 action order 不同；記錄 rounds/cost/stop reason/不確定停止數與誤判，不省略較差案例 |

比較 corpus 至少涵蓋三類 synthetic latent state、所有 pass/fail 組合與上述故障注入；
固定 corpus hash、seed 與版本。分別報告完整 corpus 與有效樣本子集的 action 數/成本/
停止率，禁止刪掉 timeout 或 unresolved 後才宣稱改善。升級準則先是安全 invariant
100% 通過、replay 一致、資源上限不超標；選擇策略即使沒有比 baseline 少步也可證明
mechanism 正確，但不能聲稱有診斷效益。若聲稱成本改善，須提供 paired 原始 trace 與
差值、清楚標示只適用此 synthetic corpus；真實世界 accuracy 不在此驗收範圍。

## 9. 待決事項與接受提案前的界線

以下不影響先交付文件，但在對應實作階段開始前需明確決議並更新本文件：

階段 0–1 已定案：準備設定使用 `mode: off`，adapter 為
`diagnostic_measurement_v1`，純排序另用 `DiagnosticPolicy`，不修改 v1 advisory
policy。fixture 資源位置見第 7 節；typed evidence schema 與設定使用方式見
[準備契約 reference](../reference/diagnostic-preparation.md)。以下尚未完成的決議仍保留：

- 設定/事件/type 名稱與 schema 版本最終拼字（本文件名稱為提案）；v1 adapter 絕不混用。
- 第 3 階段 durable dispatch bridge 採 journal intent 冪等 materialization 或同 transaction
  task admission 的具體實作；必須滿足相同 crash matrix，不能僅依 in-memory mutex。
- 後續 consumer 的 team-owned domain/provider 配置；core 必須保持 team-neutral。
- 發佈時最低可讀 binary 版本與 additive projection migration 是否必要；先證明 replay
  可重建，才決定是否需要 table，避免建立第二套 authority。

門檻 0.90、2 輪與 synthetic likelihood 是 pilot 的固定測試選擇，不待模型自行調整。
擴大 hypothesis/domain、增加 experiment、真實測試 sandbox、校準與自動修復都需要另案，
不能以「完成本規格」為由在本 PR 或第一個 pilot 實作中一併啟用。
