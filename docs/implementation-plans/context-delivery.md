# Hufu Context Delivery Improvement Plan

> Status: active
> Authority: reference
> Verified-Commit: `7312757e0187d471f16347656e887207b9db96ef`
> Supersedes: —
> Superseded-By: —
> Execution-Status: implementation-ready — 可開始 PR-00；各階段有可由 Coding Agent 執行的驗收條件，尚未實作
> Type: bounded implementation plan / offline evaluation / explicit opt-in
> Repository: kjelly/hufu
> Reviewed baseline: `7312757e0187d471f16347656e887207b9db96ef`（2026-10-12；開工時重新確認 HEAD）
> Scope: context query selection、既有 eval harness、相容性與 attribution；共三個工作包
> Location: `docs/implementation-plans/context-delivery.md`；依使用者授權移至永久位置並提交
> Completion claim: contract correctness / synthetic fixture retrieval quality；不宣稱真實任務成功率提升

## 0. 執行範圍與順序

Coding Agent 依序執行 **PR-00 → PR-01 → PR-02**。每一階段包含實作、測試、故障注入與可重現報告，不依賴人工標註真實工作負載、外部模型帳號或 production 環境。PR-02 的前置條件是 PR-01 的機器可判定 gate 通過；無須另外向使用者索取任務、模型或預算。

本計畫的產品需求：在既有模型呼叫邊界，把已授權的 constraints、verification 與 workset/dependency metadata 用於記憶檢索，減少 goal-only 檢索的漏召回；預設行為保持 legacy，改善以離線配對評估證明後才提供 active opt-in。

### 包含

- 既有 `evals/context-routing/`、`internal/evalharness` 的資料 seed、provider request assertion 與成對指標。
- runtime-owned query hints、有限的補充 exact/lexical 檢索，以及既有 ranking 核心內的版本化合併。
- shared persistent 與 worker private recall 的整合；worker private 的 restricted-ancestor 路徑保持 legacy，不擴充其檢索。
- team-neutral opt-in 設定、frozen execution policy、manifest attribution、舊 session 相容測試。

### 範圍界線

- 不新增執行中 refresh、trigger、背景排程器、LLM 語意 resolver 或另一套 memory/router/compiler。
- 不執行真實模型 A/B，不建立 experiment infrastructure，不發布或自動採用 production 設定。
- 不修改 lineage 大型檢索策略、不更換 vector backend、不移除 worker 的 `Limit: 100000` workaround 或 final allowed-ID guard。
- 不改 embedding 策略；補充檢索不呼叫 embedding 模型。既有 primary query 的 vector 行為保持原樣。
- 不重寫中文 tokenizer。本案包含中文品質測試，並如實呈現無 embedding 時純中文自然語意的既有限制。
- 不改 memory reinforcement、credit、VERIFY 隔離、closed tool sequence、tool allowlist 或 artifact authorization。

## 1. 開工檢查與目前可重用的實作

先閱讀 `AGENTS.md` 與以下現行文件；code/tests 和 active normative 文件優先於本計畫：

- [Memory learning](../architecture/memory-learning.md)
- [Knowledge coverage](../architecture/knowledge-coverage.md)
- [Canonical context schema](../reference/context-sqlite-schema.md)
- [Execution runtime](../architecture/execution-runtime.md)

記錄 HEAD、Go/toolchain 版本和受影響檔案。若現況已實作某一工作，不重建；以回歸測試確認後把該項標為已存在。其他人的變更不可覆寫。修改 Go 時使用 Modern Go Guidelines skill；修改 runtime 時使用 `harden-hufu-runtime` skill，依其規則追蹤執行路徑與契約。

| 實作位置 | 已存在能力／限制 | 本案使用方式 |
|---|---|---|
| `internal/team/context_request.go` | v1 request identity；`RetrievalQuery()` 使用 goal、phase、trigger、role、capabilities、failure | 保留 legacy 方法、序列化與 fingerprint 算法 |
| `internal/team/context_request_runtime.go` | constraints、shell Verify、DependsOn 已帶入；TouchedPaths 只在 invariant verification 任務帶入 | 使用獨立 hints resolver，避免修改 legacy request |
| `internal/team/context_router.go` | lifecycle、expiry、activation、VERIFY gate；shared persistent allowed set | 沿用授權與 eligibility，補充項目同樣過 gate |
| `internal/team/memory_ranking_core.go` | 無寫入的 persistent ranking，query retrieval、normalization、relevance／reinforcement | 在這個核心整合兩個 query lane，不新增平行 ranker |
| `internal/team/task_evidence_sources.go`、`task_evidence_delivery.go` | `resultDependencyIDs` 合併 DependsOn/EvidenceFrom；已接受 payload 與 workset input 授權檢查 | 重用，提供有界 hints；不從 task prose 猜證據 |
| `internal/team/worker_memory.go` | private tier、scope/branch guard、items/tokens budget；restricted ancestor 仍使用 overfetch | 一般 recall 可接 hints；restricted-ancestor recall 整次保持 legacy |
| `internal/context/model.go`、`sqlite_repository.go` | SearchRequest 有 AllowedItemIDs；exact/lexical SQL 支援 allow-set predicate | shared 補充查詢重用 allow-set-before-LIMIT；不另造 SQL 授權 |
| `internal/context/retrieval.go` | exact/lexical/vector、fusion、MMR、file boost、source observation | 重用 bounded supplemental helper；既有 primary 行為不變 |
| `internal/team/context_compiler.go`、`context_manifest.go` | required context、budget、content-free manifest schema 2 | 沿用編譯與 preflight；版本化新增 query attribution |
| `internal/evalharness/types.go`、`provider_fake.go` | minimal context seed、scripted provider；substring match 不等於完整 prompt assertion | 擴充 seed 與逐 request 的 model-visible message 檢查 |
| `internal/context/sqlite_repository.go::ftsQuery` | 只保留 ASCII 詞；中文不拋 syntax error，但不保證中文語意召回 | 分開測 syntax safety、identifier recall 與 fake-vector recall |

現有 tool-failure recovery、`context_query`／`context_get`、skill disclosure、knowledge-state diagnostic 都直接沿用。現有 `ranking-replay` 比較 fusion policy，並不支援本計畫的 query-plan 比較；本案的配對報告放在 eval harness，不新增 CLI query-profile 功能。

## 2. 不變量與可驗收的完成定義

1. `context.sqlite` 的 canonical rows 與 runtime task/result/receipt/verification 是 authority；FTS、vector、Markdown、aggregates 和 comparison report 都是 projection。
2. Query hints 只影響 relevance，不能新增 scope、branch、worker、tool 或路徑權限。先使用既有授權 repository，再檢查 lifecycle/activation；optional context 仍作背景資料渲染。
3. 成功送出的 invocation 中，required oracle fragments 必須全部可見；forbidden context 必須為零。容量不足時應 preflight 拒絕、provider calls 為零，這是正確結果，不計為送達失敗。
4. legacy/default 的選取、排序、RequestID、持久化 fingerprint 與 provider input 保持原有契約。比較時固定時鐘與 runtime identity；不要求不同真實 run 的時間欄位相同。
5. 純 ranking comparison 不是 exposure，不建立 candidate memory usage/outcome。隔離 fake-provider integration run 可以產生測試專用 manifest/events，以驗證真正呼叫的 attribution；這些資料不匯入真實 workspace 或另一 arm。Production 只有實際送出的 selection 能產生 exposure。
6. 把 route selection、compiler inclusion、provider delivery 分開記錄；預算排除不能假裝成 retrieval miss，編譯結果也不能代替實際 provider request。
7. 只保留 Coding Agent 能用 repository fixtures、fake provider、fake embedder 和 benchmark 完成的驗證；synthetic oracle 不代表真實使用者工作負載的成效。

## 3. PR-00 — Baseline、fixture schema、provider assertion 與離線指標

### 3.1 Fixture seed

擴充既有 `ContextItemFixture`，所有新欄位選填、舊 fixture 保持語意。seed 使用既有 canonical repository API，不直接修改資料庫表：

- lifecycle、kind、有效起訖／expiry、superseded relation、typed activation、priority/confidence。
- 使用 fixture-local scope alias enum：`same_team_shared`、`same_worker_private`、`other_worker`、`other_team`、`active_branch`、`ancestor_before_fork`、`ancestor_after_fork`、`sibling_branch`。harness 建立實際 scope/lineage；fixture 不取得 runtime 授權權限。
- 用明確 task/session/event fixtures 建立 branch 與已驗收 dependency/evidence；不偽造 verification 或 receipt。
- seed 的 kind/lifecycle/activation 等固定詞彙有 lowercase/snake_case enum 與 strict YAML validation；未知值、未知欄位、錯誤時間順序要拒絕。
- 測試時鐘用固定 UTC instant，seed timestamps 和查詢 eligibility 時間一致；不要修改全域時鐘。新增必要的內部 clock seam，production 預設仍為真實時間。

Oracle 使用明確 case data：`required_fragment_ids`、`forbidden_item_ids`、`optional_relevant_ids`、`legacy_expected_ids`、`candidate_expected_ids`、期望 preflight outcome。Coding Agent 編寫帶已知正反例的 synthetic cases；report 標記 `oracle_source: synthetic_contract`。Oracle 不可由被測 ranking 的輸出自動生成。

### 3.2 真正的 provider request assertion

在 `provider_fake.go` 的接收邊界解碼每個 request 的 messages；不要把 tool schema、JSON arguments、已生成的模型回答中的 ID 誤算成注入內容。

- 逐 invocation 檢查 system/user model-visible text、runtime context marker 與對應 fixture sentinel；marker 存在但內容缺失要失敗。
- selector 使用 runtime-owned request/model-execution identity 和每個 invocation 的 step ordinal；不得只以 agent 名稱或全域 request 次序配對 parallel streams。重用 `agent.ProviderRequest.InvocationID`、team invocation metadata 與 provider admission seam，加入 test-only observer/transport 來配對；與 fake server 實際解碼 messages 比對。識別資訊留在測試程序內，不把 fixture ID 或額外追蹤文字注入 production prompt。
- 匹配到 forbidden content 時立即記 finding；每次 request 都檢查，不能只消耗一次 substring trap。
- 使用 in-memory request evidence；report 只保留 ID/hash/count/reason，不持久化原始 messages。
- provider-visible assertions 與該 invocation 的 manifest 對齊，覆蓋 PrepareStep shaping、history compaction、fallback 後真正送出的內容。
- 非 OpenAI-compatible agent backend 用既有 fake backend 在 dispatch 邊界捕捉 model-visible input；不為它啟動 OpenAI proxy 或額外 metadata call。
- tiny-budget case 檢查 typed preflight failure 及零 provider calls。

### 3.3 指標定義

| 指標 | 分子／分母或內容 |
|---|---|
| `required_delivery_recall` | 成功送出 request 的可見 required fragments／該 request required fragments |
| `forbidden_delivery_count` | 任一 provider request 內 forbidden item 的數量，必須為 0 |
| `retrieval_recall_at_k` | 授權後、compiler 前 Top-K 中 relevant optional IDs／eligible relevant optional IDs |
| `retrieval_precision_at_k` | 同一 Top-K 中 relevant optional IDs／Top-K optional IDs |
| `delivery_recall` | provider 可見 relevant optional IDs／eligible relevant optional IDs |
| `delivery_precision` | provider 可見 relevant optional IDs／provider 可見 optional IDs |
| `irrelevant_optional_tokens` | 真正送出但 oracle 標為無關的 optional content tokens |
| `preflight_rejections` | 預期及實際拒絕數；不放入 delivery recall 分母 |
| `retrieval_calls`、`embedding_calls` | 依 exact/lexical/vector 分類的實際呼叫數 |
| `selection_latency_ms` | routing/retrieval/compile/manifest persistence 的 p50/p95；分開報各段 |

分母為零輸出 null 並附 `not_applicable`；未知數據輸出 null 並附 `unavailable`。token counter/spec 必須一致，使用 estimated counter 時標記 estimated，不能報成精確 tokenizer 成本。

### 3.4 交付與驗收

- baseline report 帶 schema version、commit、fixture digest、query policy、learning policy、clock/spec、case ID、retrieved/included/omitted IDs、reason 與上述指標。
- 新的改善 case 可以明確記錄 legacy miss，不要求 baseline 已經解決 candidate 的目標；既有 passing cases、安全 gate 與 required delivery 必須維持。
- coverage matrix 每列有 case ID；不要以任意 case 數取代覆蓋證據。基本場景見 §7。
- 使用既有 eval report 輸出；執行產物寫到暫存目錄，不提交含實際使用者資料的 baseline。

## 4. PR-01 — Query hints、版本化排序與離線配對評估

本階段只有內部 builder、純 ranking seam、query attribution helpers 與 eval；production 仍走 legacy。harness 以選填 `query-profile-override: legacy|candidate` 指定 test-only selector，舊 fixtures 預設 legacy；不得將 override 暴露成 production flag。沒有線上 shadow 設定、comparison goroutine 或額外 production 檢索。

### 4.1 Runtime-owned hints resolver

建立 `context_query_plan.go`，把兩個責任分開：

- resolver 從 durable TodoItem、已接受結果和已綁定 workset 收集 immutable hints，做授權檢查。
- builder 接收 v1 ContextRequest 與 resolver 的 typed hints，正規化、去重、redact、限制長度並生成 plan；builder 不查 repository、不呼叫模型。

Plan 至少包含 policy version、未改寫的 legacy primary query、supplemental query、授權 paths、來源 ID/content hash、plan hash 和固定 reason enum。原文只存在 invocation-local memory，不落庫。

| 訊號 | 唯一可用來源與上限 |
|---|---|
| primary | 原封不動使用 `ContextRequest.RetrievalQuery()`；不以新增 hints 改寫 Goal/RequestID |
| constraints | admitted `ContextRequest.Constraints`；redact 後最多 384 runes |
| verification | admitted `VerificationCriteria`，目前是 shell Verify；最多 256 runes；只作 literal retrieval text，不猜命令語意 |
| paths | durable TodoItem.WorksetBinding 與其有效 receipt；重用 workset 授權/正規化；最多 8 個，每個最多 128 runes；保持路徑大小寫，不截斷單一路徑 |
| dependencies | `resultDependencyIDs(TodoItem)`，包含 DependsOn/EvidenceFrom；只讀既有 gate 已接受的 TaskResult.Summary；最多 4 個摘要、合計 256 runes |

Dependency lookup 重用 `validateAcceptedDependencyPayload` 與既有 dependency delivery 檢查；不把 ID 當自然語言、不讀取任意 task、原始 tool output 或任意 structured payload。尚未完成／未被接受的結果不產 hint；需要的 evidence 缺失仍由既有必需證據 gate 拒絕，resolver 不能掩蓋它。

Constraints/summary 是已入場的文字資料，仍非執行授權。只做機械式文字正規化和檢索；不加 regex/keyword 意圖分類器，不從文字推測新 scope 或 verified status。

Supplemental text 順序固定為 constraints → verification → dependency summaries → paths；去重後最多 1024 runes。paths 為 bytewise stable order；dependency 按既有 canonical todo order；先 redact 再截文字，超過剩餘預算的路徑整個省略。primary + supplemental 最多 4096 runes；supplemental 可用長度為 `min(1024, max(0, 4096 - primary_runes))`，包含分隔符。primary 自己已超限時，candidate 使用完整 legacy selection 並記 `input_over_limit`，不截掉 required goal。

**不修改** `newTaskContextRequest` 的 TouchedPaths/DependencyIDs、v1 JSON、Fingerprint 或 AssignRequestID。新增 query metadata 使用獨立 plan hash。沒有 hints、memory off、reviewer／repair 禁止歷史記憶時，直接保持 legacy selection。

### 4.2 兩個 query lane 的明確算法

在既有 retrieval/ranking 核心增加以下版本 `context-query-v2.1`，不建立另一個記憶服務：

1. **primary lane**：執行現有 legacy query，保留 exact/lexical/vector、fusion、MMR、file boost、goal-line fallback。在同一次純 ranking comparison 中，candidate 和 legacy 共用 primary 的 immutable 授權結果，不再執行一次 vector query。每個 arm 的 result slice、item metadata 和 observation buffer 必須分開，避免 normalization 或 sorting 改寫另一 arm。
2. **supplemental lane**：使用另一個 query 獨立搜尋同一 scope 的 canonical repository，不能只重排 primary 已找到的 ID。使用現有 exact/lexical helper、file boost，vector 關閉；每 scope 最多 8 個 exact terms、1 次 lexical、64 個授權結果，超出 terms 按 stable order 捨棄並記 reason。限制只作用於新 lane。
3. shared persistent supplemental 使用 router 已計算的 eligible allowed set，傳入現有 SQL allow-set predicate；仍做 final scope/activation recheck。worker supplemental 使用相同 worker scope 與現有 eligibility，不能把 coordinator 的 shared allowed set 當 private 權限。
4. 每個 lane 以授權後的順序給 item 1-based rank；只取現有 CandidateTopK 與 64 的較小值供兩 lane 合併。legacy 自己的 candidate/inject limits 不改。
5. 同 ID 合併，合併 relevance 定義為 `5 * (0.6 / (4 + primary_rank) + 0.4 / (4 + supplemental_rank))`；某 lane 缺少該 ID 時該項為 0。以合併分數排序、使用第 8 項 tie-breaker 後，套回原有 CandidateTopK；union 不是提高最終候選上限的理由。primary 在相同 rank 下貢獻較高；固定 rank constant 為 4，使 supplemental-only 的第一名能超過 primary-only 的第四名，避免新召回項永遠排在已滿的 InjectTopK 外。這不是保證所有 primary 命中永遠優於 supplemental。
6. 此分數已在 [0,1]；active candidate 不可再套 legacy 的乘 61 normalization。保留各 lane 原本 exact prefix/source ranks，不把 supplemental exact hit 自動提升為全域第一名。
7. 將此 BaseRelevance 接回既有 relevance／reinforced ranking、trust/harmful/stale penalty、content dedup、InjectTopK 與 compiler token budget。不得抹除 experience score，也不得額外 append 未通過排名的 optional item。Required/MustKeep 保持原有路徑。
8. private recall 保持既有 tier precedence，再在同 tier 使用上述 relevance 與既有 reranker。完全同分以 scope distance、priority、confidence、UpdatedAt、opaque ID 決定；不使用 map iteration order。
9. 若 hints 空、supplemental 無授權命中或不可用，直接返回原有 legacy selection/score，不重新縮放 primary；記 `no_hints`、`no_matches` 或對應 fallback reason。

上述權重和上限是本版本固定、可測的 policy constants，不新增一串可自由調整的使用者旗標。只有 §6 gate 通過才交付 active；若算法需變更，更新 policy version、golden assertions 和 paired report，不用放寬 gate 或調大 Top-K 掩蓋失敗。

### 4.3 範圍、成本與失敗

- 每 invocation 最多三個 supplemental scopes：shared persistent、active private session、private persistent；未啟用的 memory scope 不查詢。最多新增 27 次 exact/lexical 呼叫、192 個補充授權候選；candidate buffer 有界。
- coordinator 只使用 admitted goal 與已有明確 constraints；不能自行拆解使用者 prose。direct agent 缺少 task hints 時保持 legacy；不偽造它沒有的 workset/dependency。
- **private recall 若包含 restricted ancestor，整個 private bundle 使用 legacy**，記 `restricted_lineage_legacy`；shared persistent 的改善可獨立使用。不是 lineage 完整性修正。
- 新 supplemental I/O 以 invocation context 和總共 100ms 子 deadline 控制，循序執行、不另開背景 goroutine、不重試。三個 scope 共用 invocation-local budget 和 staged selection，不各自重設 100ms；保留每個 source 的 immutable legacy bundle。超時／搜尋失敗／source revision 在擴充期間變動則丟棄全部 supplemental 結果，以保留的 legacy bundles 編譯，並在實際受影響的 sources 記固定 fallback enum。
- 100ms 只限制新工作；不得聲稱已限制 legacy overfetch 或所有 DB 掃描。bench 同時報 legacy 成本與新增成本；取消必須傳到底層搜尋。
- 外層 invocation 已取消時傳回 cancellation，不用 legacy fallback 繼續呼叫模型。授權、必需證據、compiler required fit 或實際 manifest persistence 失敗仍依原契約 fail closed。
- 補充項目在編譯前做 canonical scope/lifecycle/activation recheck；revision 檢查不取代 authorization，也不自動重跑檢索。

### 4.4 離線配對評估

legacy/candidate 使用相同 fixture、scope、time、canonical snapshot、experience aggregates、learning policy、Top-K、model spec、token budget。

- 純 ranking comparison 共用 primary 結果，但 supplemental 獨立檢索同一授權資料 universe；legacy miss 必須在至少一個 case 被 candidate 召回。
- 每個 arm 用獨立 ephemeral run identity；comparison 透過 fixture invocation key 配對，不要求兩 arm 的真實 RequestID 相同。
- 使用 fixture SQLite snapshot／固定 seed，避免兩次檢索之間寫入；fake embedding vectors 固定在 fixture，不呼叫外部服務。這只驗證向量路徑的契約。
- 純 ranking comparison 共用一次 primary；end-to-end 配對則是兩個隔離 integration runs，每個 run 各做正常 primary 呼叫，candidate 不得比同一條 baseline 路徑增加 embedding 呼叫。兩 arm 各經 compiler、真正 fake provider request boundary 和各自 manifest；candidate arm 是離線測試執行，不是 production shadow exposure。
- offline 配對期間不讓某 arm 的學習事件更新另一 arm 的 aggregates；報告只有 ID/hash/count/reason。
- 重跑同一 arm 能得到相同選入順序、plan hash、semantic fingerprints；run/time identity 在 comparison report 單獨表示，不改寫 durable receipt。

### 4.5 PR-01 的 attribution seam

先完成 §5.2 的版本化 query metadata、manifest builder/validator 和 composite hash 純 helper，供 candidate fake-provider run 使用；production legacy 不使用新欄位。保留舊 schema/JSON round-trip 測試。PR-02 再把這些 helper 接入正式 config、snapshot 與所有 runtime 路徑。

## 5. PR-02 — Active opt-in、持久化契約與整合

### 5.1 唯一新增設定

新增 team-neutral team.yaml 設定 `context-routing.query-mode`，enum 為 `legacy`、`active`；未指定為 legacy。沒有線上 shadow mode、新 CLI flag 或 per-team branch。

- 在 `internal/agent/agent.go`、`internal/team/team_manifest.go`、`parse.go` 的既有 schema/parse/validation 流程加入，涵蓋 legacy YAML 和 versioned manifest；錯字及未知值開工前拒絕。
- query mode 與 `memory-learning.mode`、worker memory policy 相互獨立；不得為了 active query 啟用 memory 或 credit。
- 在 `execution_policy_snapshot.go` freeze active query policy/version/limits/hash。legacy/default 不加新非空欄位，保持既有 configuration hash；舊 snapshots 一律解讀為 legacy。
- active run resume 時 mode/version/hash 必須一致；換模式要使用新 run／workspace，不重跑已完成的外部 side effect。

### 5.2 Runtime 與 attribution

正常 DAG、direct、coordinator continuation、nested delegation、extra-model 各沿用現有 compiler/provider 邊界；以 invocation-local plan/候選資料整合，不使用 coordinator 全域 query buffer。

- 實際候選選取、Compiler Included/Omitted、provider-visible content 必須對應同一 query identity。
- manifest 使用 query attribution block：policy version、plan hash、各 source 的 effective mode/fallback reason/primary hash/supplemental hash，以及整體 effective mode。Source enum 為 `shared_persistent`、`private_session`、`private_persistent`；source mode 為 `legacy`、`active`、`not_applicable`；整體 mode 為 `legacy`、`active`、`mixed`。若 shared 改善但 private 保持 legacy，記 mixed，不能宣稱全部 source 使用 candidate。
- 固定 fallback enum 為 `none`、`no_hints`、`no_matches`、`input_over_limit`、`supplemental_timeout`、`supplemental_search_failed`、`source_revision_changed`、`restricted_lineage_legacy`、`memory_disabled`、`history_not_allowed`、`hint_source_unavailable`。額外 term/path 截斷以計數記錄，不偽裝成授權放寬。
- 新 query attribution 使用 manifest schema 3；legacy/default 繼續產 schema 2 且不加新欄位。保留 schema 2 的 fingerprint validator；active（包括 mixed 或整體 fallback）使用 schema 3 validator。不可用新版算法重算並覆寫舊 fingerprint。
- memory injection 的 query hash：legacy/default 保持原 query hash；active run 使用 policy version、plan hash、各 source 的實際 mode/query hashes 和授權 hint provenance hash 的 canonical composite hash。排序與序列化固定；把 mixed/各 source fallback 也納入，不能讓 memory manifest 記成未執行的 query。
- explain/inspect 對 active hash 顯示 query policy 和 `query_plan_required`，不能把單一 `--query` 重算結果冒充歷史 active selection。讀取持久化 manifest 不重新查 memory、不要求 query 明文。
- 保留既有 occurrence-scoped outcome cap、retry/extra-model identity 和 idempotency；選入不等於 applied。
- 真正 manifest persistence 仍在 provider call 前且 fail closed。額外 comparison/benchmark report 寫入失敗只讓該報告失敗，不改 task 狀態或重跑工具。
- active 設定但 supplemental fallback 時，對應 source 必須標示 legacy 實際選取與固定 fallback reason；不能記成 v2 成功改善。

## 6. Coding Agent 可自行執行的 gates

PR-01 必須通過下列第 1～4、6～7 項才進入 PR-02。第 5 項的 snapshot/resume/runtime integration gates 在 PR-02 完成；每階段遵守第 8 項失敗語意，不依賴人工 approval evidence：

1. 所有既有 passing fixtures 維持原結果；legacy/default 的 identity、選取和 provider request parity 通過。
2. 成功送出的 required delivery recall 為 100%，forbidden delivery 為 0；預期容量拒絕案例有 typed error、零 provider calls。
3. 在 constraints、verification、paths、accepted dependency 四類中，每類至少一個明確 legacy miss 在相同 K/token budget 被修復；既有 positive-control cases 的 delivery recall 不下降。
4. 每個 paired quality case 的 irrelevant optional token count 不增加，aggregate retrieval/delivery recall 不下降，aggregate delivery precision 不下降；不以擴大 K/budget 通過。
5. 相同 fixture 重播結果 deterministic；resume、parallel、extra-model 不串 identity、不重複 credit。
6. supplemental 增加的 embedding/model/tool calls 都為 0；搜尋數和候選數不超 §4.3 上限；deadline/cancel/revision-change 故障測試通過。
7. benchmark 使用至少 10／1,000／10,000 個 canonical rows 的 synthetic stores，包含 private/shared、強干擾資料、超時案例；報 p50/p95、allocations、queries/candidates。沒有實測數字不能寫「成本可接受」。
8. 若品質 gate 失敗，修正實作/版本後重跑；不得改 oracle relevant/forbidden labels 來配合輸出。新算法仍無法通過時，交付 PR-00/01 的 failing report，保持 production legacy，不交付 active 為已驗收。

## 7. 回歸與品質矩陣

| 場景 | 必須檢查 |
|---|---|
| 同 goal、不同 constraints | 有關記憶能改善，無關高 utility 記憶不擠入 |
| shell Verify、其他 verification contracts | literal Verify hint 改善；沒有文字的 typed contract 不猜語意，原本 required verification 完整 |
| 已綁定 paths／無 binding／大小寫不同的 paths | 有效 receipt 才產 hints；缺失時不推測；大小寫不混淆 |
| DependsOn／EvidenceFrom／未完成／wrong-run 依賴 | 已接受 Summary 可用；跨批次來源不遺漏；未授權不查詢 |
| expired、superseded、rejected、activation mismatch | 高分也不送出；metadata 設定不是權限 |
| VERIFY／judge／skeptic／guard／result-only repair | 原有記憶隔離與 evidence contract 不變 |
| active private、persistent private、other worker | scope、tier、去重和 token limit 不變 |
| restricted ancestors／siblings／fork cutoff | private legacy parity 與零越權；不宣稱修復大型 lineage 截斷 |
| 純中文、不含 ASCII identifiers、無 embedding | syntax 安全；純中文語意召回不足如實報 unavailable/legacy limit，不當成品質成功 |
| 中文 goal＋ASCII path／command＋relevant memory | 有明確 identifier 的補充召回與無關項排除 |
| 固定 fake-vector 中文相關性 | primary 向量路徑契約；candidate 不增加 embedding calls |
| tiny budget／required over budget | optional omission 理由；required 不足則不呼叫 provider |
| memory off／fresh session／fresh verification | 不增加查詢、archive 讀取或曝光 |
| DAG／direct／nested／extra-model／coordinator continuation | 實際呼叫邊界 assertion；沒有 hint 的路徑為 legacy |
| retry／tool failure／cancel／resume／parallel | 原有 recovery、manifest 順序、identity 與 credit 完整 |
| semantic unavailable／supplemental error／revision change | bounded fallback，合法 legacy 繼續；外層 cancel 停止 |
| stage shaping／compaction／provider fallback | manifest 與最後 provider request 對應，不能只驗 compiler |
| non-LLM runtime action／agent backend | runtime action 沒有 model exposure；fake backend 可檢查實際 input |

## 8. 交付、驗證與可完成範圍

| 工作包 | Coding Agent 交付物 | 下一階段條件 |
|---|---|---|
| PR-00 | 相容 fixture schema、逐 request assertion、baseline report 與 coverage matrix | 原有行為和安全 gates 通過 |
| PR-01 | hints resolver/builder、bounded lane merge、query attribution helpers、offline paired report、failure tests/bench | §6 的 PR-01 gates 通過 |
| PR-02 | explicit active opt-in、snapshot/manifest 相容、runtime 整合與 operator attribution | §6 所有適用 gates、完整回歸與 lint 通過；default 仍 legacy |

每一程式碼工作包執行：

```bash
git status --short
git rev-parse HEAD
go test ./internal/context/... ./internal/team/... ./internal/evalharness/... ./cmd/hufu/...
go run ./cmd/hufu eval run evals/context-routing
go run ./cmd/hufu eval run evals/memory-learning
go run ./cmd/hufu eval run evals/retry-recovery
go test ./...
go vet ./...
golangci-lint run
```

涉及 invocation state、clock、provider capture、shared buffers 的改動另執行受影響 package 的 `go test -race`。本機 socket 沙箱阻擋 httptest 時，依環境工具權限流程執行本機 fake-provider 測試，不能以外部真模型替代。實際 benchmark 命令和輸出隨工作包提供。

修改程式後更新對應現行 reference/architecture 中的已實作行為，並在本計畫記錄每一階段的實作與驗證狀態；每階段驗收後分別 commit。不得因本計畫改寫尚未核准的 lineage 架構文件。

完成報告分開列出：已實作工作包、passing/failing gates、fixture 品質差異、fallback 頻率、成本數據、尚未證明的真實成效。允許 truthful 的部分交付；不能把程式編譯成功或 fake provider 回答成功宣稱為真實任務效益。
