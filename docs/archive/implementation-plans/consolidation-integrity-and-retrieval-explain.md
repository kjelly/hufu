# Hufu consolidation 完整性修正與檢索解釋：實作計畫

> Status: implemented — moved 2026-09-27 from local scratch space (`docs/tmp/spec.md`); implemented on branch `fix/consolidation-integrity` through `b9e1b5c` (see §11 實作紀錄)
> Authority: reference（實作紀錄；現行行為以程式、測試、[memory learning](../../architecture/memory-learning.md) 與 [context SQLite schema](../../reference/context-sqlite-schema.md) 為準）
> Verified-Commit: `b9e1b5c`
> Supersedes: —
> Superseded-By: —
> Scope: consolidation 提案／審核交易化、通用 lifecycle 旁路封鎖、候選身份與 reject 黏性、來源失效連動降級與唯讀 doctor、`explain-memory` 重做、兩個讀取路徑 bug
> Code baseline: `kjelly/hufu` main, commit `489db51`（本文所有 `file:line` 以此 commit 為準；行號會漂移，以函式名稱為主）
> Date: 2026-09-27（v2：依查核結果與使用者決策改寫初稿）

本計畫只包含可由 coding agent 以程式、固定測試資料與自動驗證完成的工作。需要真實 workspace 資料或人工判斷的工作列在 §10，不屬於本次交付。

本檔是實作計畫，不是現行文件的權威來源。實作時以程式、測試與 accepted ADR 為準。實作完成後，依實際程式更新 §9 列出的正式文件；不要讓正式文件引用本檔。

## 0. 改寫紀錄

v1（「離線衍生 Observation 與檢索解釋」）經查核後原地改寫。原章節處置：

| v1 章節 | 處置 | 理由 |
| --- | --- | --- |
| §1 現有基礎 | 改寫為 §2 | 修正誤述：`AppendMany` 不存在；`consolidation show/approve/reject` 沒有 `--team`；`context doctor` 目前強制 `--learning`；persistent 判定另接受 legacy `memory_tier`（`persistent_knowledge.go:21`）。 |
| §2 固定邊界、§3 admission guard、§4 Observation 契約、§5／§6 中 Observation 專屬部分 | **移除** | 核准後的 Observation 永不進 prompt，也不能當 promotion／consolidation 來源，沒有任何消費者；admission guard 卻要改約 10 條讀取路徑（`context_get`、`memory_query`、worker recall、chromem、`stm.md`／`ltm-TEAM.md` 投影檔、FTS、semantic inventory 等），而且 `Append`／`UpsertCandidate` 的去重會改寫 `source_json`，保留標記本身可被改掉（BUG-10）。 |
| §5 交易原子性、審核旁路封鎖；§6 新鮮度狀態機 | **改套既有 consolidation**（WP-1～WP-4） | 既有 consolidation 的產出會進 prompt，且有 v1 只替 Observation 修的同一批問題（BUG-04～BUG-11、BUG-15）。 |
| §7 檢索解釋 | 保留並修正（WP-5） | 現有 `explain-memory` 越權可讀、分數與 runtime 不一致（BUG-02、BUG-03）；v1 未考慮 BUG-01。v1 的「持久化 trace DTO」移除（不需要）。 |
| §8 交付順序與驗收 | 改寫為 §6～§8 | — |
| — | 新增 §3 bug 清單、WP-6 | 查核中發現的讀取路徑 bug。 |

## 1. 決策

使用者已確認（2026-09-27）：

- **D1 BUG-01（`rrf` 夾帶原始分數）只記錄並在 explain 揭露。** 本計畫不改 production 排序。修正需要真實 workspace replay 評估，列入 §10。
- **D2 來源失效採寫入時連動降級。** 來源被 supersede 時，在同一個交易內把由它衍生、已核准的 consolidation 項目降回 `candidate`（因此離開 prompt），proposal 標為 `stale`；沒有寫入事件的失效（過期、有效期結束、open conflict）由唯讀 doctor 回報。
- **D3 操作者 reject 具黏性。** `hufu context reject` 與 `consolidation reject` 的結果不會被相同內容重開；run 失敗造成的自動 reject 仍可由之後成功的 run 重開（保留 `UpsertCandidate` 註解說明的原設計）。

實作決策（本計畫固定）：

- **D4** `consolidation_proposal` 是保留的 source type：只有 consolidation 專用 repository 方法可以建立、確認、拒絕或改寫它。
- **D5** 不新增 SQLite migration。`consolidation_proposals.status` 是沒有 CHECK 的 TEXT 欄位，新增值 `stale` 不需 migration。
- **D6** 審核紀錄以同一交易內的 `context_events` 為準。既有 RunEvent `memory_consolidation_proposed` 保留為 commit 後附加，沿用既有 idempotency key；event store 遇到重複 key 視為成功（`event_store.go:395-415`），所以重跑會補寫漏掉的事件。approve／reject／stale 不新增 RunEvent。
- **D7** 已核准的 proposal 不能 reject（D2 未選 retire 指令）。要替換已核准的合併知識，用 `hufu context supersede`，會觸發連動規則。
- **D8** `explain-memory` 是「以目前資料重算」，不持久化任何解釋資料；`--team` 改為必填（runtime 一律有 team scope）。
- **D9** 來源新鮮度判斷只有一份實作。create、approve、reject、show、doctor、improve handoff 全部呼叫它。

## 2. 已驗證的程式碼現況（baseline `489db51`）

### 2.1 Consolidation

- 指令：`hufu context consolidate [--apply-proposal --source <ids> (--proposal-text <t> | --draft --team <t>)]`；`hufu context consolidation show|approve|reject <id>` 只有 `--workspace --project --json --policy-version`（`cmd/hufu/context_consolidation_cmd.go:33-49`）。
- 建立：`persistConsolidationProposal`（`:125-167`）依序呼叫 `UpsertCandidate`、`AddEdges`、`SaveConsolidationProposal`、event store `Append`，四者各自獨立。ID 是 `sha256(排序後 IDs + "\x00" + 原始文字)` 切成 `[:10]`／`[10:20]`（`:126-128`），但 repository 的 `normalize` 會 `TrimSpace`、CRLF→LF 並 redaction 後才算 content hash（`internal/context/sqlite_repository.go:284-294`）。
- 來源驗證分散三處：`validateConsolidationSources`（`:228-251`：confirmed、未 supersede、同 scope 同 kind、project/team 相符、`contradicts_ids`）、`validateConsolidationConflicts`（`context_consolidation_conflicts.go:36-47`）、`validateConsolidationSupport`（`:215-226`：experience aggregate 門檻）。它們都在任何交易之外執行。
- 核准：`runContextConsolidationApprove`（`:305-325`）先檢查 status，再 `validateConsolidationProposalCurrent`（`:327-354`：來源、衝突、凍結 hash、aggregate revision；aggregate 不存在視為 revision 0），然後 `ConfirmCandidates`（自己一個交易），最後 `UpdateConsolidationProposal`（另一個交易）。
- 拒絕：`runContextConsolidationReject`（`:356-370`）不檢查 proposal status，先 `UpdateLifecycle(rejected)`（`:362`）再 `UpdateConsolidationProposal`（`:365`，要求 `status='proposed'`）。
- `--draft` 在模型呼叫前用 `FindProposedConsolidation` 重用同來源的 pending proposal（`context_consolidation_draft.go:58-65`）；`--proposal-text` 路徑沒有這個檢查。
- improve handoff 自有一份驗證 `validateConsolidationHandoffCurrent`（`cmd/hufu/improve_handoff.go:285-305`）：不查 open conflict，且 aggregate 不存在即視為已變更（與 approve 不同）。
- `consolidation_proposals`（migration 5）的 status 目前使用 `proposed`／`approved`／`rejected`／`failed`（`internal/context/consolidation.go:68`）。
- `derived_from` edge 唯一的寫入點是 `context_consolidation_cmd.go:144`；整個 repo 沒有讀取它的程式。

### 2.2 Repository 變更語意

- `SQLiteRepository` 只開一條連線（`sqlite_repository.go:74` `SetMaxOpenConns(1)`）。**交易內絕不能呼叫任何走 `r.db` 的方法，否則會 deadlock**；交易內的讀取必須綁定 `*sql.Tx`。`conflictQuerier`（`conflict.go:131-134`）已可接受 tx；`queryPairJudgments`／`conflictViews` 以它為參數。
- `normalize` 在 lifecycle 為空時預設為 **confirmed**（`:301-303`）。
- `Append` 去重命中時，不論既有列的 lifecycle 或 source type，都執行 `UPDATE ... SET updated_at=?, source_json=?`（`:404-406`）。
- `UpsertCandidate`（`:333-390`）以 (project, kind, content_hash, 完整 scope) 找既有列：confirmed 原樣回傳；其餘（含 rejected）覆寫 `source_json`、`evidence_json`、`metadata_json`、confidence，並強制 `lifecycle='candidate'`，不看既有列的 source type。
- `UpdateLifecycle`（`:998-1029`）不檢查目前 lifecycle，接受 candidate／confirmed／rejected 任一目標。production 呼叫者全都只傳 candidate（`worker_memory.go:878`、`shared_memory.go:409`、`completion_gate.go:301`、`contextcmd.go:462` 呼叫前有檢查、`context_consolidation_cmd.go:362` 例外）。測試 `coordinator_tools_memory_test.go:139` 用它把 candidate 改成 confirmed；`improve_handoff_test.go:187,221` 用它 reject confirmed 來源。
- `ConfirmCandidates`（`:1095-1176`）、`BindCandidates`（`:1035-1093`）都不檢查 source type；`ConfirmCandidates` 會處理 `supersedes_ids`，並在同一交易內 supersede 舊項目。
- `MarkSuperseded`（`:970-996`）一個交易；呼叫者：`contextcmd.go:340`（migrate-memory）、`:500`（`context supersede`）。
- `hufu context confirm/reject`（`contextcmd.go:413-470`）接受專案內任何 candidate；reject 會以 evidence type `operator_rejection` 呼叫 `BindCandidates`，再呼叫 `UpdateLifecycle`。
- shared memory `Propose`（`internal/team/shared_memory.go:118-189`）以 `Source.Type="shared_memory_candidate"` 呼叫 `UpsertCandidate`；`ConfirmRun`（`:285-320`）以 `SourceTypes: ["shared_memory_candidate"]` 加 `OriginRunID` 找候選並確認。`memory_save`／`ltm_update` 把 `Propose` 錯誤轉成非空的 tool error（`coordinator_tools_memory.go:116-118, 478-480`）；auto-extract 只記 warning（`coordinator_reflexion.go:118-123`）。

### 2.3 檢索與 explain

- `HybridRetrieveWithOptions`（`internal/context/retrieval.go:164-248`）：exact 結果當前綴；`rrf(lexical, vector)`；`applyMMR(λ=0.75)`；file-path boost `+0.15`；最後依 `Limit` 截斷。
- `rrf`（`:338-368`）在 `:344` 以 `current = result` 起算，**第一個含該 item 的清單的原始分數會被帶進 fused score**，再加上 `1/(61+rank0)`，最後依 content hash 去重。lexical 原始分數是 `-bm25`（`sqlite_repository.go:1303`）。
- runtime persistent ranking `rankSharedPersistentMemoryAllowed`（`internal/team/memory_ranking.go:91-176`）：vector 固定傳 nil；`Limit = max(CandidateTopK, len(allowed))`；結果為空時退回 goal line 重查；以 allowed set 過濾後截到 `CandidateTopK`；把 `0<s<1` 的分數 ×61（`:152-153`）；依 mode 用 relevance entries（`:180-196`）或 reinforced entries（`:217-280`）；會寫 memory ranking trace（副作用）。
- `Route`（`context_router.go:230-330`）以 `EvaluateContextEligibility`（`:160`）算出 allowed set，排序後把沒被選到的 `MustKeep` 項目強制加回。runtime mode 以已採用的 policy snapshot 為準（`memory_policy_runtime.go:134`）。
- `explain-memory`（`cmd/hufu/context_learning_cmd.go:170-222`）：`repo.Get` 不限 scope（`:179`）；`HybridRetrieve(vector=nil, Limit=100)`（`:196`）；直接用原始分數，沒有 ×61、goal-line fallback 或 allowed set；`--team` 可省略。
- `ContextDecisionReason` enum 在 `context_router.go:17-32`。`GetScoped`（`sqlite_repository.go:608-648`）對越權回 `ErrReadScopeDenied`，對不存在回 `sql.ErrNoRows`。

### 2.4 其他

- `GetAuthorizedContextItem`（`internal/team/coordinator_tools_context.go:188-219`，`context_get` tool）先 `Query(Limit: 200)` 才在 Go 比對 ID。
- `VectorStore.SearchVector`（`internal/context/vector.go:112-133`）與 `SearchSimilarTo`（`:141-163`）先向 chromem 取 top-`limit`，才 hydrate 並用 `isRetrievable` 過濾。測試可用 `NewVectorStore(dir, "test-v1", testEmbedding)` 注入假 embedder（`vector_test.go:61`）。
- `docs/reference/operator-command-reference.md` 是生成檔，由 `TestGeneratedOperatorCommandReferenceIsCurrent` 檢查；重新生成：`go run ./cmd/hufu examples --format markdown`。
- 檔案大小：`sqlite_repository.go` 1440 行、`contextcmd.go` 1003 行，都已超過 CLAUDE.md 的 800 行上限。**新程式放新檔案**，只在既有方法內插入必要的呼叫。

## 3. 已知 bug 清單

「實測」表示以暫時性測試跑過（已刪除）；「讀碼」表示逐行讀程式確認。

| ID | 問題 | 證據 | 驗證 | 處置 |
| --- | --- | --- | --- | --- |
| BUG-01 | `rrf` 把第一個清單的原始分數帶進 fused score。6 筆語料時 BM25≈3e-6 看不出影響；302 筆語料時 fused≈11.77，RRF 本身只有 0.016，排序實際上等於 BM25。runtime 的 ×61 只處理 `<1` 的分數，所以 `BaseRelevance` 可 >1，`MinimumRelevance=0.05` 幾乎沒有作用；vector-only 項目只有 cosine+RRF，幾乎一定排在 lexical 命中之後。 | `retrieval.go:344`、`memory_ranking.go:152` | 實測 | D1：不修；WP-5 揭露 `carried_score`；§10 |
| BUG-02 | `explain-memory` 以 `repo.Get` 讀 item，不限 scope；其他 project 的 item 一樣輸出曝光、套用、驗證次數與分數，等於可探測存在性。 | `context_learning_cmd.go:179` | 讀碼 | WP-5 |
| BUG-03 | `explain-memory` 的 `base_relevance`／`final_score` 與 runtime 不同：沒有 ×61 正規化、Limit 100（runtime 20）、沒有 goal-line fallback、沒有 allowed set；不在前 100 名時顯示 0，與「不相關」無法區分。 | `context_learning_cmd.go:196-205` 對照 `memory_ranking.go:113-154` | 讀碼 | WP-5 |
| BUG-04 | `consolidation reject` 不檢查 proposal status，而 `UpdateLifecycle` 允許 confirmed→rejected：reject 已核准的 proposal 會把已確認的知識改成 rejected，然後 proposal 更新失敗，最終 proposal=`approved`、候選=`rejected`。 | `context_consolidation_cmd.go:356-370`、`sqlite_repository.go:1015` | 讀碼 | WP-1、WP-2 |
| BUG-05 | `consolidation approve` 分兩個交易：確認候選後若 proposal 更新失敗，候選已 confirmed 但 proposal 仍 `proposed`；之後 approve 永遠失敗（不是 candidate），改用 reject 又會觸發 BUG-04。 | `:317`、`:320` | 讀碼 | WP-1 |
| BUG-06 | `persistConsolidationProposal` 四段獨立寫入且不檢查 `UpsertCandidate` 回傳值：(a) 文字與同 scope 既有 confirmed 項目相同時，proposal 會指向那筆既有知識，之後 approve 必失敗、reject 會把既有知識（可能就是來源）改成 rejected；(b) 同文字但來源集合不同（或只差首尾空白）會得到不同 proposal ID 卻共用同一候選，第二次 upsert 會覆寫第一個的 `Source.Ref`／metadata；(c) proposal 列寫入後若 event 附加失敗，重跑會卡在主鍵衝突，永遠無法成功；(d) reject 後重跑，會把候選翻回 candidate，proposal 卻仍是 `rejected`。 | `:125-167` | 讀碼 | WP-1 |
| BUG-07 | `hufu context confirm <consolidation 候選>` 可直接確認，完全跳過 approve 的來源 revision／衝突檢查，proposal 留在 `proposed`；`hufu context reject` 同樣會留下不一致狀態。 | `contextcmd.go:413-470` | 讀碼 | WP-2 |
| BUG-08 | `UpsertCandidate` 以內容找既有列、不看 source type：model 用 `memory_save` 存入與 consolidation 候選完全相同的文字時，會把它改寫成 `shared_memory_candidate`、加上 `run_id`，之後在 run acceptance 被 `ConfirmRun` 確認，不經人工核准；反方向則讓 shared 候選脫離 `ConfirmRun`。 | `sqlite_repository.go:378`、`shared_memory.go:285-320` | 讀碼推導，未端到端實測 | WP-3 |
| BUG-09 | `UpsertCandidate` 會把操作者 reject 過的項目重開成 candidate，並清掉 `operator_rejection` 證據與 `rejection_reason`。 | `sqlite_repository.go:378` | 讀碼 | WP-3（D3） |
| BUG-10 | `Append` 去重命中時，不論既有列 lifecycle 或 source type 都改寫 `source_json`（包括 confirmed 列）。這使 source type 無法當可靠的治理標記。 | `sqlite_repository.go:404-406` | 讀碼 | WP-2 只保護保留 source type；一般行為見 §10 |
| BUG-11 | `derived_from` edge 只寫不讀；已核准的合併項目在來源被 supersede、過期或出現衝突後，仍留在 prompt，而且沒有任何指令能下架它。 | `context_consolidation_cmd.go:144` | 讀碼 | WP-4（D2） |
| BUG-12 | `context_get` 先 `Limit 200` 再比對 ID：資料量大時，合法可讀的 item 會被回報成找不到。 | `coordinator_tools_context.go:192-202` | 讀碼 | WP-6 |
| BUG-13 | chromem `SearchVector`／`SearchSimilarTo` 先取 top-K 才做授權過濾：top-K 中有越權或已失效項目時，回傳筆數少於 limit。只影響 CLI（`context query`、`conflicts --vector`）。 | `vector.go:121-132, 159-163` | 讀碼 | WP-6 |
| BUG-14 | `UpdateLifecycle` 可把任何列直接改成 confirmed（含 rejected→confirmed），完全不綁證據。production 沒有呼叫者，只有測試使用。 | `sqlite_repository.go:998-1029` | 讀碼 | WP-2 |
| BUG-15 | improve handoff 的 consolidation 驗證與 approve 不一致：不查 open conflict；aggregate 不存在時判為已變更（approve 視為 revision 0）。 | `improve_handoff.go:285-305` | 讀碼 | WP-1／WP-4（D9 共用判斷） |

觀察到但本計畫不處理（非目標，見 §10）：

| ID | 觀察 | 證據 | 驗證 |
| --- | --- | --- | --- |
| OBS-1 | run finalization 依序在三個獨立交易確認 worker、shared、run-shared 候選；後段失敗時，前段已確認的不會被補償（補償只 reject candidate）。 | `experience_processor.go:45-82` | 讀碼 |
| OBS-2 | task result reducer 寫入的 findings（kind `observation`）直接 confirmed，失敗 run 的 findings 仍在同 session 的 prompt 可見。 | `shared_working_memory.go:84-100` | 讀碼 |
| OBS-3 | `memory_query` 與 worker memory recall 不經過 `EvaluateContextEligibility`（沒有 activation／phase 檢查）。 | `coordinator_tools_memory.go:235-287`、`worker_memory.go:233-310` | agent 讀碼，未逐行複核 |
| OBS-4 | `migrate-memory --apply` 每筆 supersede 各一個交易，中途失敗會留下部分 supersede。 | `contextcmd.go:315-345` | agent 讀碼，未逐行複核 |
| OBS-5 | `hufu context supersede` 在交易外檢查、交易內寫入（check-then-act）。 | `contextcmd.go:472-505` | agent 讀碼，未逐行複核 |

## 4. 目標與非目標

目標：

1. consolidation 的建立、核准、拒絕各自是單一 SQLite 交易；可安全重跑；並行時只有一方成功，且最終狀態一致。
2. 除了 consolidation 專用方法，沒有任何 API 或 CLI 能建立、確認、拒絕或改寫 `consolidation_proposal` 項目。
3. 相同內容不能跨 source type 劫持候選；操作者的 reject 不會被推翻。
4. 來源被 supersede 時，由它衍生的合併知識在同一交易內離開 prompt；其他失效由唯讀 doctor 回報。
5. `explain-memory` 重算的結果與 runtime 的實際選取一致，不越權、不輸出原文、不改變 production 排序，並揭露 BUG-01。
6. 修掉 BUG-12、BUG-13。

非目標：修正 BUG-01；衍生 Observation 功能；改變 `Append` 的一般去重行為；OBS-1～OBS-5；新增 migration；自動修復既有不一致資料（doctor 只回報）；新增 retire 指令；持久化檢索解釋。

## 5. 工作項目

### WP-1 Consolidation 交易化

新增檔案 `internal/context/consolidation_tx.go`（交易方法）與 `internal/context/consolidation_freshness.go`（新鮮度判斷）。`internal/context/consolidation.go` 保留型別與讀取方法。

#### 5.1.1 型別與常數

```go
const SourceTypeConsolidationProposal = "consolidation_proposal"

const (
    ConsolidationStatusProposed = "proposed"
    ConsolidationStatusApproved = "approved"
    ConsolidationStatusRejected = "rejected"
    ConsolidationStatusFailed   = "failed" // 既有值，只讀
    ConsolidationStatusStale    = "stale"  // WP-4 寫入
)

const (
    EvidenceTypeOperatorApproval  = "operator_approval"
    EvidenceTypeOperatorRejection = "operator_rejection" // contextcmd.go:459 改用此常數
)

type ConsolidationSupportPolicy struct {
    MinConfirmedSupport int
    MinIndependentTasks int
}

type ConsolidationCreateInput struct {
    ProjectID, TeamID string
    SourceIDs         []string // 方法內去重並排序；至少 2 筆
    Text              string
    Origin            string // "operator" | "model"
    DraftModel        string // Origin=="model" 時必填
    PolicyVersion     string
    Support           ConsolidationSupportPolicy
}

type ConsolidationReviewInput struct {
    ProposalID, ProjectID string
    PolicyVersion         string
    Actor                 string // 例如 "hufu context consolidation approve"
    Reason                string // reject 必填
}
```

`internal/context` 不 import `internal/agent`；CLI 以 `agent.DefaultMemoryLearningPolicy()` 組出 `ConsolidationSupportPolicy`。

錯誤（皆可用 `errors.Is` 判別，訊息不含記憶內容）：`ErrConsolidationNotFound`（不存在與不在該 project 使用同一錯誤與同一訊息）、`ErrConsolidationNotPending`（附目前 status）、`ErrConsolidationProposalExists`（同 ID 已存在且不是 proposed）、`ErrConsolidationPending`（同來源集合已有其他 proposed proposal，附其 ID）、`ErrConsolidationCandidateDuplicate`（候選內容已存在於同 scope／kind，附既有 item ID）、`ErrConsolidationSourceInvalid`（附 §5.1.5 reason codes）、`ErrConsolidationInconsistent`（proposal 與候選的連結損壞，訊息提示執行 `hufu context doctor --consolidation`）。

#### 5.1.2 內容正規化與身份

- 從 `normalize` 抽出 `NormalizeContent(string) string`（TrimSpace、CRLF→LF、`RedactSecrets`），`normalize` 改呼叫它，行為不變。
- `text := NormalizeContent(in.Text)`；若 redaction 改變了文字（即含 secret-like 內容）或結果為空 → 錯誤，不寫入任何東西。CLI 既有的檢查保留。
- ID 格式不變，但輸入改用正規化後文字：`sum := sha256(strings.Join(ids, "\x00") + "\x00" + text)`，`proposalID = "consolidation-" + hex(sum[:10])`，`candidateID = "ctx-consolidated-" + hex(sum[10:20])`。修掉 BUG-06(b) 的首尾空白分歧。

#### 5.1.3 `CreateConsolidationProposal(ctx, in) (ConsolidationProposal, bool, error)`

以 `withBusyRetry` 包住單一交易；第二個回傳值表示本次是否新建。交易內依序：

1. 依 ID 讀 proposal（tx）。已存在且為 `proposed`：驗證它與候選的連結（候選存在、`Source.Type` 為保留值、`Source.Ref == proposal.ID`），通過就回傳 `(existing, false, nil)`，否則 `ErrConsolidationInconsistent`。已存在但不是 `proposed`：`ErrConsolidationProposalExists`。
2. 同 project／team 且 `source_ids_json` 相同的其他 `proposed` proposal → `ErrConsolidationPending`。
3. 在 tx 內讀來源，以 §5.1.5 的判斷（`mode=create`）驗證；非 fresh → `ErrConsolidationSourceInvalid`。
4. 候選內容重複檢查：同 (project, kind, content_hash, 完整 scope) 有任何 lifecycle 的既有列，或 `candidateID` 已被使用 → `ErrConsolidationCandidateDuplicate`。修掉 BUG-06(a)(b)。
5. 插入候選：Kind 取自來源（來源必同 kind）；Scope、Authority、Priority 取自 `sources[0]`（沿用現況）；`TrustLevel=internal`；Confidence 取來源最小值；`Lifecycle=candidate`；`Source={Type: consolidation_proposal, Ref: proposalID}`；Metadata 與現況相同（`derived_from`、`consolidation_proposal`、`proposal_origin`，必要時加 `draft_model`）。插入 FTS 列與 `candidate_append` 事件。插入邏輯從 `appendOnce` 抽成 tx helper 共用，不另寫一份 SQL。
6. `INSERT OR IGNORE` 每一來源的 `derived_from` edge。
7. 插入 proposal：`SourceRevisions` 為凍結的 content hash；`AggregateRevisions` 只記有 aggregate 的來源（比較時缺值視為 0，與現況 approve 相同）。
8. `insertEvent("consolidation_proposed", candidateID, scope, {proposal_id, source_ids, origin, policy_version})`，不含內容。

提供 unexported 測試掛鉤 `consolidationTxTestHook func(stage string) error`（production 為 nil），在步驟 5、6、7 之後呼叫，用來驗證「任一步失敗都完全不留痕跡」。

CLI（`persistConsolidationProposal` 改寫）：呼叫 Create；不論 created 為 true 或 false，都以既有 payload 與 idempotency key 附加 RunEvent（D6），然後輸出。created=false 時輸出既有的「already pending」訊息。`--draft` 保留模型呼叫前的 `FindProposedConsolidation` 檢查；Create 因競態回 `ErrConsolidationPending` 時，輸出該 pending proposal，行為與預檢相同。

#### 5.1.4 `ApproveConsolidationProposal` 與 `RejectConsolidationProposal`

兩者都是以 `withBusyRetry` 包住的單一交易，回傳更新後的 proposal。

Approve：

1. 在 tx 內讀 proposal；不存在或 `ProjectID` 不符 → `ErrConsolidationNotFound`。status 必須是 `proposed`，否則 `ErrConsolidationNotPending`。
2. 候選必須存在、`Lifecycle=candidate`、未 supersede、`Source.Type` 為保留值、`Source.Ref == proposal.ID`、scope 的 project／team 與 proposal 相同；否則 `ErrConsolidationInconsistent`。
3. §5.1.5 判斷（`mode=approve`，含 aggregate revision 相等檢查）必須是 fresh，否則 `ErrConsolidationSourceInvalid`，交易回滾。
4. 以 `confirmCandidateTx`（從 `ConfirmCandidates` 迴圈主體抽出的 unexported helper，含 `supersedes_ids` 處理；公開的 `ConfirmCandidates` 改為呼叫它）確認候選；binding 為 `{Type: operator_approval, Ref: proposal.ID}`，metadata `approved_by=<Actor>`。
5. `UPDATE consolidation_proposals SET status='approved', reason=?, reviewed_at=? WHERE id=? AND status='proposed'`，影響列數必須為 1。
6. `insertEvent("consolidation_approved", ...)`。

Reject：

1. 讀 proposal 的規則同上。status 必須是 `proposed` 或 `stale`。`approved` → `ErrConsolidationNotPending`，訊息指向 `hufu context supersede`（D7）。
2. 候選必須存在、`Lifecycle=candidate`、`Source.Type` 為保留值、`Source.Ref == proposal.ID`；否則 `ErrConsolidationInconsistent`。
3. 在候選上附加 evidence `{Type: operator_rejection, Ref: proposal.ID}` 與 metadata `rejection_reason=<Reason>`，把 lifecycle 改成 rejected，並寫入 `lifecycle` 事件。
4. `UPDATE ... SET status='rejected' ... WHERE id=? AND status IN ('proposed','stale')`，影響列數必須為 1。
5. `insertEvent("consolidation_rejected", ...)`。

CLI：approve／reject 改為呼叫這兩個方法；刪除 `validateConsolidationProposalCurrent`。`loadConsolidationRepo` 對不存在與不在該 project 回同一訊息（`consolidation proposal %q was not found in project %q`）。reject 的 reason 沿用現有固定字串 `explicit operator rejection`。

移除 `SaveConsolidationProposal`、`UpdateConsolidationProposal` 與 `ConsolidationRepository` interface，避免繞過交易。測試 fixture（`internal/context/consolidation_test.go`、`cmd/hufu/improve_handoff_test.go`）改用 Create／Approve／Reject；若 fixture 必須直接造出損壞狀態，就在 `_test.go` 內以 SQL 直接寫入。保留 `GetConsolidationProposal`、`FindProposedConsolidation`；新增 `ListConsolidationProposals(ctx, projectID, teamID string, allTeams bool)` 供 doctor 使用。

#### 5.1.5 來源新鮮度判斷（D9 的唯一實作）

```go
type ConsolidationFreshnessState string // "fresh" | "stale" | "blocked" | "invalid"
type ConsolidationReason string

type ConsolidationFreshness struct {
    ProposalID    string                           `json:"proposal_id"`
    Status        string                           `json:"status"`
    CandidateID   string                           `json:"candidate_id"`
    State         ConsolidationFreshnessState      `json:"state"`
    Reasons       []ConsolidationReason            `json:"reasons,omitempty"` // 去重、排序
    SourceReasons map[string][]ConsolidationReason `json:"source_reasons,omitempty"`
}

type freshnessMode int // create | approve | inspect

func evaluateConsolidationFreshness(ctx context.Context, q conflictQuerier, p ConsolidationProposal, mode freshnessMode, opts freshnessOptions, now time.Time) (ConsolidationFreshness, error)
```

`q` 可為 `*sql.DB` 或 `*sql.Tx`；交易內一律傳 tx。open conflict 查詢從 `OpenConflictsForItems` 抽出以 `conflictQuerier` 為參數的 helper，公開方法改呼叫它；schema <11 時沿用現況回空結果。create 模式沒有 proposal，改以輸入的來源集合評估。

Reason codes（固定 enum；不得以自由文字控制行為）：

| State | Reason | 條件 |
| --- | --- | --- |
| invalid | `proposal_decode_failed` | proposal 列無法解碼 |
| invalid | `source_set_invalid` | 來源少於 2 筆，或任一來源缺凍結 hash |
| invalid | `candidate_missing` | 候選列不存在 |
| invalid | `candidate_link_mismatch` | 候選 `Source.Type`／`Source.Ref`／project／team 與 proposal 不符 |
| invalid | `candidate_shared` | 同一候選被多個 proposal 引用 |
| invalid | `lifecycle_mismatch` | status 與候選 lifecycle 不一致：`proposed`／`stale` 要求 candidate；`approved` 要求 confirmed；`rejected` 要求 rejected |
| invalid | `edge_mismatch` | `derived_from` edge 集合 ≠ proposal 來源集合 |
| blocked | `source_conflict_open` | 任一來源有 open conflict |
| stale | `marked_stale` | proposal status 為 `stale` |
| stale | `candidate_superseded` | 已核准的候選已被 supersede |
| stale | `source_missing`、`source_not_confirmed`、`source_superseded`、`source_expired`、`source_outside_validity`、`source_revision_changed`、`source_scope_mismatch`、`source_contradiction` | 依字面：來源不存在、非 confirmed、已 supersede、`expires_at ≤ now`、不在 `valid_from`／`valid_until` 區間、hash ≠ 凍結值、scope／kind 不一致或 project／team 不符、`contradicts_ids` 互指 |
| stale | `aggregate_revision_changed` | 只在 approve／handoff 模式，aggregate revision ≠ 凍結值 |
| stale | `support_insufficient` | 只在 create 模式，未達 `ConsolidationSupportPolicy` 或 `CausalFailureCount>0` |

多種原因並存時，State 取優先序 `invalid` > `blocked` > `stale` > `fresh`，Reasons 全部列出。查詢失敗一律回傳 error，絕不判為 fresh。`rejected`／`failed` 的 proposal 只做連結與 lifecycle 一致性檢查，不做來源檢查。

`validateConsolidationSources` 的規則搬進此判斷後，在 `cmd/hufu` 刪除；dry-run cluster builder 不變。

#### 5.1.6 WP-1 測試（`internal/context/consolidation_tx_test.go`，table-driven）

- 建立成功：候選、edges、proposal、`context_events` 全部存在；test hook 在各階段注入錯誤後，Revision 不變且沒有任何新列。
- 相同輸入重跑回 `created=false`、不新增列；CLI 重跑會再次附加 RunEvent，event store 內仍只有一筆（冪等）。
- 文字只差首尾空白 → 同一 proposal ID。
- 文字等於同 scope 既有 confirmed 項目 → `ErrConsolidationCandidateDuplicate`，既有項目不變（BUG-06a 回歸）。
- 同文字、不同來源集合 → duplicate 錯誤（BUG-06b 回歸）。
- 同來源、不同文字的第二個 pending → `ErrConsolidationPending`。
- 在 create 與 approve 之間 supersede 來源 → approve 回 `ErrConsolidationSourceInvalid`（WP-4 之後改為 NotPending／stale，見 WP-4 測試），候選仍是 candidate。
- 以 hook 讓 approve 的 proposal 更新失敗 → 候選沒被確認（BUG-05 回歸）。
- reject 已核准的 proposal → 錯誤，候選仍 confirmed（BUG-04 回歸）；reject 兩次 → 第二次錯誤且狀態不變。
- 舊資料連結損壞（候選 `Source.Ref` 指向另一個 proposal）→ approve／reject 回 `ErrConsolidationInconsistent`。
- 並行（`-race`）：兩個獨立 `OpenSQLite` handle 同時 Create 相同輸入 → 恰好一個 `created=true`；同時 approve 與 reject → 恰好一個成功，最終 proposal 與候選一致。
- `evaluateConsolidationFreshness` 對每個 reason code 至少一個 fixture。

### WP-2 保留 source type 與通用 lifecycle 旁路封鎖

新增 `internal/context/reserved_source.go`：`IsReservedSourceType(string) bool`（目前只有 `consolidation_proposal`），以及 `ErrReservedSourceType`、`ErrLifecycleTransition`、`ErrCandidateIdentityConflict`（WP-3 沿用並推廣）。既有方法內只插入檢查呼叫。

| 方法 | 新規則 |
| --- | --- |
| `Append`、`AppendReducer` | 任一輸入項為保留 source type → `ErrReservedSourceType`，整批不寫。去重命中的既有列若是保留 source type，不改寫 `source_json`，只寫 `deduplicate` 事件。 |
| `UpsertCandidate` | 輸入為保留 source type → `ErrReservedSourceType`。既有列為保留 source type → `ErrCandidateIdentityConflict`（WP-3 將此規則推廣到所有 source type）。 |
| `ConfirmCandidates`、`BindCandidates` | 任一目標列為保留 source type → `ErrReservedSourceType`，訊息指向 `hufu context consolidation approve` 與 `reject`，整批不寫。 |
| `UpdateLifecycle` | 目標只允許 `rejected`（其他 → `ErrLifecycleTransition`，修 BUG-14）；目前 lifecycle 必須是 `candidate`（否則 `ErrLifecycleTransition`，不存在仍回 `sql.ErrNoRows`）；保留 source type → `ErrReservedSourceType`。在 tx 內先讀列再更新。 |
| `MarkSuperseded` | 允許（操作者可 supersede 合併知識），並觸發 WP-4 連動。 |

WP-1 的專用方法使用 unexported tx helper，不經過這些公開檢查。

CLI：`hufu context confirm|reject` 在呼叫 repository 前，若任一項目為保留 source type，就回錯誤 `context item %q belongs to consolidation proposal %q; use "hufu context consolidation approve|reject %s"`（proposal ID 取自 `Source.Ref`）。`context repair` 重播 pending JSON 遇到保留 source type 會回錯，屬預期行為。

測試（`internal/context/reserved_source_test.go`，table-driven：方法 × 保留列／一般列）：錯誤時 Revision 不變；對保留列的去重不改 `source_json`；`UpdateLifecycle` 的 confirmed 目標、confirmed 來源都被拒，candidate→rejected 可行。更新 `coordinator_tools_memory_test.go:139`（改用 `ConfirmCandidates`）與 `improve_handoff_test.go:187,221`（改用 `MarkSuperseded` 使來源失效，並依 WP-4 行為調整斷言）。worker／shared／completion gate 既有測試必須不改斷言就通過。

### WP-3 候選身份衝突與 reject 黏性（D3）

`UpsertCandidate` 找到非 confirmed 的同身份列時，依序：

1. `existing.Source.Type != item.Source.Type` → `ErrCandidateIdentityConflict{ExistingID, ExistingSourceType}`，不改任何資料（修 BUG-08）。
2. `existing.Lifecycle == rejected` 且 Evidence 含 `operator_rejection` → `ErrOperatorRejected{ExistingID}`，不改任何資料（修 BUG-09）。
3. 其他情況維持現況（含重開 run 失敗造成的 rejected）。

confirmed 列維持原樣回傳。兩個錯誤的訊息只含 item ID 與 source type，不含內容。呼叫端不需改：`memory_save`／`ltm_update` 已把錯誤轉成非空的 tool error，auto-extract 只記 warning。

測試（table-driven，既有列 × 輸入）：同 type candidate 會刷新；不同 type candidate → identity conflict；操作者 rejected → `ErrOperatorRejected`；run rejected → 重開；confirmed → 原樣回傳。錯誤案例的 Revision 不變。另加一個 `internal/team` 測試：先建 consolidation 候選，再以 `memory_save` 存相同文字並跑 `ConfirmRun`，候選的 source type 與 lifecycle 都不變（BUG-08 端到端回歸）。

### WP-4 失效連動降級與唯讀檢查（D2）

#### 5.4.1 連動降級

新增 unexported `demoteDerivedConsolidationsTx(ctx, tx, invalidated []string, reason ConsolidationReason, now time.Time) error`（放在 `consolidation_tx.go`）：

1. 以 BFS 處理 queue，visited set 防止循環。對每個失效 ID，查：
   `SELECT p.id, p.status, p.candidate_context_item_id FROM consolidation_proposals p, json_each(p.source_ids_json) s WHERE s.value = ? AND p.project_id = ? AND p.status IN ('proposed','approved')`。以 proposal 的來源集合為準，不依賴 edge（legacy 資料的 edge 可能缺漏）。
2. 對每個命中的 proposal：`UPDATE ... SET status='stale', reason=<reason code> WHERE id=? AND status IN ('proposed','approved')`；`reviewed_at` 不變。寫 `insertEvent("consolidation_stale", candidateID, scope, {proposal_id, invalidated_source_id, reason})`。
3. 原 status 為 `approved`，且候選為 confirmed、`Source.Ref == proposal.ID`：`UPDATE context_items SET lifecycle='candidate', updated_at=?`，寫 `lifecycle` 事件（payload 含 `reason: consolidation_stale`），並把候選 ID 以 `source_not_confirmed` 加入 queue（處理合併的合併）。
4. 候選連結不符（legacy）：只標 proposal stale，不碰候選；事件 payload 註明 `candidate_link_mismatch`。

呼叫點（在既有交易內）：`MarkSuperseded` 每個 old ID（reason `source_superseded`）；`confirmCandidateTx` 的 supersede 迴圈每個 oldID（同上，涵蓋 run finalization 與 approve）。`UpdateLifecycle` 經 WP-2 後只能 reject candidate，而來源必為 confirmed，所以不需呼叫。

效果：降級後的項目不再是 confirmed，因此自動離開所有 confirmed-only 讀取路徑（projection、lexical、HybridRetrieve、LTM Markdown），不必改任何讀取路徑。`stale` proposal 不能 approve；reject 允許（WP-1），會把候選改成 rejected。

#### 5.4.2 `consolidation show`

新增 freshness（inspect 模式）。文字輸出加一行 `freshness: <state> reasons=<r1,r2>`；JSON 以嵌入 struct 保留原有頂層欄位，再加 `freshness` 物件（純新增，不破壞既有欄位）。

#### 5.4.3 `hufu context doctor --consolidation`

- 旗標：`--learning` 與 `--consolidation` 必須恰好擇一，錯誤訊息 `one of --learning or --consolidation is required`。`--consolidation` 要求 `--project`，`--team` 可省略（省略時涵蓋該 project 所有 team，與 dry-run 的 subtree 語意一致）。
- 唯讀：只做 SELECT；測試斷言執行前後 Revision 相同。
- 評估範圍內所有 proposal（含 rejected／failed 的一致性檢查），另列 orphan：`Source.Type` 為保留值、但沒有 proposal 指回的候選（reason `orphan_candidate`，state invalid）。
- 文字輸出：一行摘要 `context doctor --consolidation: fresh=N stale=N blocked=N invalid=N orphans=N`，之後每個非 fresh 項目一行 `proposal=<id> status=<s> candidate=<id> state=<st> reasons=<...>`。
- JSON：`{"schema_version":1,"status":"ok"|"attention","counts":{...},"proposals":[ConsolidationFreshness...],"orphans":[{"item_id","reason"}]}`。只輸出 ID、狀態與 reason code，不輸出內容。
- 能產出報告就 exit 0（`status` 表示有無問題）；讀取失敗才回非零。
- 放在新檔 `cmd/hufu/context_consolidation_doctor.go`。

#### 5.4.4 improve handoff

`validateConsolidationHandoffCurrent` 改呼叫共用判斷（approve 模式），修 BUG-15。handoff 其餘 status／candidate 檢查不變。

#### 5.4.5 WP-4 測試

- supersede 已核准 proposal 的來源 → 候選降為 candidate、proposal 變 stale、`QuerySharedPersistentProjection` 不再回傳它；`hufu context supersede` 之後重建的 `ltm-TEAM.md` 不含它。
- 經 `ConfirmCandidates` 的 `supersedes_ids` 使來源失效，同樣觸發連動。
- 遞移：A 是 C 的來源、C 是 D 的來源，supersede A → C、D 都降級。
- 循環安全；legacy 連結不符只標 stale。
- supersede proposed proposal 的來源 → proposal stale，approve 回 `ErrConsolidationNotPending`，reject 可行。
- doctor 對每個 reason code 至少一個 fixture，並包含 BUG-04／BUG-06 造成的舊不一致狀態（以 SQL 直接造）；doctor 唯讀；show 的 JSON 同時保有舊欄位與新欄位。
- handoff 在來源有 open conflict 時拒絕。

### WP-5 `explain-memory` 重做

#### 5.5.1 檢索觀測（`internal/context/retrieval_explain.go`）

`HybridRetrievalOptions` 新增 `Observer *RetrievalObservation`。production 一律為 nil；觀測資料只存在於記憶體，任何路徑都不得持久化。

```go
type RetrievalObservation struct {
    Paths      []RetrievalPathObservation      // 順序固定：exact, lexical, vector
    Candidates []RetrievalCandidateObservation // 依 RetrievalRank，再依 ItemID
}

type RetrievalPathObservation struct {
    Path              string                 `json:"path"` // "exact" | "lexical" | "vector"
    Executed          bool                   `json:"executed"`
    UnavailableReason SemanticFallbackReason `json:"unavailable_reason,omitempty"`
    ResultCount       int                    `json:"result_count"`
}

type RetrievalCandidateObservation struct {
    ItemID        string  `json:"item_id"`
    ExactRank     int     `json:"exact_rank,omitempty"` // 1-based；0 = 該途徑未回傳
    LexicalRank   int     `json:"lexical_rank,omitempty"`
    LexicalScore  float64 `json:"lexical_score,omitempty"` // SearchLexical 的原始分數（-bm25）
    VectorRank    int     `json:"vector_rank,omitempty"`
    VectorScore   float64 `json:"vector_score,omitempty"`
    CarriedScore  float64 `json:"carried_score"` // BUG-01：rrf 起算的原始分數
    LexicalRRF    float64 `json:"lexical_rrf,omitempty"` // 1/(60+rank)
    VectorRRF     float64 `json:"vector_rrf,omitempty"`
    FusedScore    float64 `json:"fused_score"` // == CarriedScore + LexicalRRF + VectorRRF
    DuplicateOf   string  `json:"duplicate_of,omitempty"` // rrf content-hash 去重的勝出者
    ExactPrefix   bool    `json:"exact_prefix,omitempty"`
    PreMMRRank    int     `json:"pre_mmr_rank,omitempty"`
    MMRRank       int     `json:"mmr_rank,omitempty"`
    MMRPenalty    float64 `json:"mmr_penalty,omitempty"`
    MMRScore      float64 `json:"mmr_score,omitempty"`
    FilePathBoost float64 `json:"file_path_boost,omitempty"`
    RetrievalRank int     `json:"retrieval_rank,omitempty"` // Limit 截斷前的最終順位
    CutByLimit    bool    `json:"cut_by_limit,omitempty"`
}
```

實作限制：`rrf` 與 `applyMMR` 的簽章與輸出不變（v1 的「不得為本需求重寫 fusion 演算法」維持）。改為共用迴圈的 `rrfObserved(obs, lists...)`／`applyMMRObserved(obs, candidates, lambda)`，公開版本傳 nil。只出現在 exact 的項目不經 rrf，`FusedScore` 為其 exact 分數。

#### 5.5.2 抽出 runtime 排序核心（`internal/team/memory_ranking_core.go`）

把 `rankSharedPersistentMemoryAllowed` 與 `reinforceSearchResults` 的計算抽成不依賴 `*Coordinator`、沒有副作用的函式：

```go
type persistentRankingInput struct {
    Query        string
    RequestScope contextstore.Scope         // 原 c.contextScope()，用於 tie-break
    Base         []contextstore.ContextItem // 空 query 的 MustKeep／Pinned 分支用
    Allowed      map[string]bool
    Learning     agent.MemoryLearningPolicy
    Ranking      MemoryRuntimeRankingPolicy // 已套 effectiveMemoryRankingPolicy
    Observer     *contextstore.RetrievalObservation
}

type persistentRankingResult struct {
    Results           []contextstore.SearchResult // allowed 過濾、CandidateTopK 截斷、×61 之後
    RelevanceEntries  []MemoryRankingEntry
    ReinforcedEntries []MemoryRankingEntry // 只在 shadow／active 計算
    Aggregates        map[string]*contextstore.ExperienceAggregate
    Selected          []contextstore.ContextItem
    Scores            map[string]MemoryScoreParts
    FinalScores       map[string]float64
    UsedGoalFallback  bool
    CandidateCutIDs   []string // 在 allowed 內、被 CandidateTopK 截掉
}

func rankPersistentMemory(ctx context.Context, repo contextstore.RetrievalRepository, experience contextstore.ExperienceRepository, in persistentRankingInput) (persistentRankingResult, error)
```

`rankSharedPersistentMemoryAllowed` 改為呼叫它，再依 mode 寫 memory ranking trace，回傳值不變。核心內以 `HybridRetrieveWithOptions(Vector: nil, Mode: RetrievalActive, UnavailableReason: SemanticFallbackProjectionMissing, Observer: in.Observer)` 取代 `HybridRetrieve(nil)`（兩者等價）；goal-line fallback 時先清空 observer 再重查。

**先寫行為鎖定測試再重構**：以固定 fixture（≥30 筆，含重複內容、不同 priority、MustKeep、有害 aggregate）在重構前記錄各 mode 下的 selected IDs、rank、分數，重構後必須完全相同。

`EvaluateContextEligibility` 拆出與 request 無關的前段 `evaluateLifecycleEligibility(item, runID, now)`（lifecycle、superseded、有效期、stale environment）；原函式改為呼叫它再做 activation 檢查，行為不變。

#### 5.5.3 解釋組裝（`internal/team/memory_explain.go`）與 CLI

```go
type MemoryExplainInput struct {
    Workspace, ItemID, ProjectID, TeamID, Query, PolicyVersion string
    Now                                                        time.Time
}

func ExplainPersistentMemory(ctx context.Context, repo *contextstore.SQLiteRepository, in MemoryExplainInput) (MemoryExplanation, error)
```

1. `GetScoped(ItemID, {ProjectID, TeamID})`（不含內容）。`ErrReadScopeDenied` 與 `sql.ErrNoRows` 一律轉成 `ErrMemoryExplainNotFound`，訊息 `context item %q was not found in project %q team %q`（修 BUG-02）。
2. `LoadMemoryPolicy(repo, version)` 取得 learning 與 ranking policy；另回報 `mode_source`：`policy_snapshot`（有記錄的 snapshot）或 `default`。
3. allowed set：`QuerySharedPersistentProjection({ProjectID, TeamID})` 經 `evaluateLifecycleEligibility(runID="")` 過濾。activation 閘門需要 phase／role／trigger 等 request 狀態，CLI 沒有，所以不評估，並在輸出 `not_evaluated` 列出 `activation`；token budget 在 compile 階段，同樣列出 `token_budget`。
4. 呼叫 `rankPersistentMemory`（附 Observer），mode 取自步驟 2。
5. 依下表為被解釋的 item 產生 reasons（`MemoryExplainReason` enum，固定順序，列出所有成立的條件）：

| Reason | 條件 |
| --- | --- |
| `selected` | 依 mode 的有效 entry 為 selected |
| `must_keep_forced` | `MustKeep` 且排序未選到（`Route` 會強制加回） |
| `outside_persistent_scope` | item 帶 session／branch／agent／task／attempt scope，不參與 persistent 排序 |
| `lifecycle_ineligible` / `expired` / `environment_mismatch` | `evaluateLifecycleEligibility` 的對應結果 |
| `outside_observed_candidates` | 在 allowed 內，但沒有任何檢索途徑回傳它；不得解讀為「不相關」 |
| `duplicate_content` | `DuplicateOf` 非空 |
| `retrieval_limit` | `CutByLimit` |
| `candidate_limit` | 在 `CandidateCutIDs` 內 |
| `below_relevance` | normalized base relevance < `MinimumRelevance` |
| `harmful_use` | active mode 且 `HarmfulUsePenalty > 0` |
| `non_positive_score` | active mode 且 `FinalScore ≤ 0` |
| `inject_limit` | 其餘條件都通過，但名次超過 `InjectTopK` |

6. 輸出 `MemoryExplanation`：嵌入既有 `MemoryScoreExplanation`，保留原有頂層欄位（`context_item_id`、`policy_version`、`retrieval_id`、`score_parts`、`final_score`、各計數）。`score_parts.base_relevance` 改為 runtime 正規化後的值（BUG-03 修正，文件須註明語意變更）。新增欄位：`schema_version: 2`、`recomputed: true`、`scope`、`mode`、`mode_source`、`query_hash`、`used_goal_fallback`、`retrieval.paths`、`retrieval.candidate`（該 item 的觀測；不在候選內則省略）、`retrieval.observed_candidate_count`、`ranking.{base_rank, relevance_rank, relevance_selected, reinforced_rank, reinforced_selected, selected}`、`reasons`、`not_evaluated`。`retrieval_id` 沿用 `RetrievalIDForItem`，只有 manifest 相符時才非空。
7. 任何路徑都不輸出 query 原文、item 內容、embedding 或 source 文字。

CLI：`explain-memory` 要求 `--project`、`--team`、`--query`；呼叫 `ExplainPersistentMemory`；文字輸出在既有行之後加上 scope、mode（source）、selected、reasons、not_evaluated、各途徑 rank／分數、`carried_score`、`fused_score`、MMR、boost。vector 途徑顯示 `executed=false unavailable_reason=projection_missing`，與 runtime 一致；不開啟 Ollama。解釋邏輯放新檔案，`context_learning_cmd.go` 只留旗標與輸出。

#### 5.5.4 WP-5 測試

- 觀測不影響結果：以 `testdata/retrieval_golden.json` 與隨機 fixture，`HybridRetrieveWithOptions` 有無 Observer 的 `[]SearchResult` 與 `RetrievalTrace` 完全相同。
- 等價：同一 fixture 與多組 query（含帶 `\n` 的 goal-line fallback），bare `&Coordinator{...}` 的 `rankSharedPersistentMemoryAllowed` 與 `ExplainPersistentMemory` 對每個 item 的 selected、rank、base relevance 完全一致；off／observe／shadow／active 各跑一次。
- scope：其他 project、其他 team、不存在的 ID 得到相同錯誤文字。
- 每個 reason 至少一個 fixture。
- BUG-01 揭露：302 筆 fixture 下 `CarriedScore + LexicalRRF + VectorRRF == FusedScore`（誤差 1e-12 內），且 `CarriedScore > 1`。
- 無副作用：執行後沒有新增 memory ranking trace，Revision 不變。
- CLI JSON 仍含所有既有欄位。

### WP-6 讀取路徑 bug

- **6a（BUG-12）**：`RepositoryQuery` 新增 `IDs []string`。語意與 `SearchRequest.AllowedItemIDs` 相同：nil 表示不限制，非 nil 空切片表示不匹配任何列。在 `compileRepositoryPredicates` 編成 `id IN (...)`。`GetAuthorizedContextItem` 改用 `Query{Scope, Ancestors, IncludeCandidates: true, IDs: []string{id}, Limit: 1}`，其餘授權與 activation 邏輯不變。測試：250 筆較新、較高 priority 的項目加 1 筆最舊的目標 → 找得到；越權 ID 仍找不到。
- **6b（BUG-13）**：`SearchVector`／`SearchSimilarTo` 改為逐步放大：`n := min(limit(+1), count)`；每輪 query n、hydrate、過濾；已湊滿 limit 或 `n == count` 就截到 limit 回傳；否則 `n = min(2n, count)`。測試（假 embedder）：30 筆文件中前 10 名相似度的都在其他 scope → 仍回傳 limit 筆可讀結果；結果順序與舊版在「沒有被過濾項目」時相同。

## 6. 交付順序

每個 WP 一個 commit，可單獨驗證：

1. **WP-1**（交易方法、新鮮度判斷、CLI 改用）
2. **WP-2**（依賴 WP-1：approve 已不經 `ConfirmCandidates`）
3. **WP-3**
4. **WP-4**（依賴 WP-1 的判斷與 WP-2 的 lifecycle 規則）
5. **WP-5**（與 WP-1～4 無依賴，可並行）
6. **WP-6**（無依賴，可並行）

## 7. 測試矩陣摘要

| 面向 | WP | 關鍵斷言 |
| --- | --- | --- |
| 原子性 | 1 | hook 注入任一階段失敗 → Revision 不變、無殘留 |
| 冪等與重跑 | 1 | 重跑 created=false；RunEvent 只一筆 |
| 並行 | 1 | 雙 handle 同時 create／approve+reject → 恰好一方成功（`-race`） |
| 旁路 | 2 | 每個通用方法 × 保留列 → 錯誤且 Revision 不變 |
| 身份與黏性 | 3 | 跨 type 不劫持；操作者 reject 不重開；run reject 可重開 |
| 失效 | 4 | supersede → 同交易降級＋stale；遞移；prompt 排除 |
| 唯讀檢查 | 4 | 每個 reason code；doctor 不寫入 |
| explain 等價 | 5 | 四種 mode 下與 runtime 選取一致 |
| explain 安全 | 5 | 越權與不存在同錯誤；不輸出內容；無副作用 |
| 讀取路徑 | 6 | 大資料量 `context_get` 找得到；vector 回滿 limit |

## 8. 驗收

- `go test ./...`、`go vet ./...`、`golangci-lint run` 全部通過。
- `go test -race ./internal/context -run 'Consolidation|Reserved|Upsert'`；涉及 `internal/team` 的 race 測試需 `-timeout 45m`。
- 若新增或變更的旗標影響 `docs/reference/operator-command-reference.md` 的生成內容，執行 `go run ./cmd/hufu examples --format markdown` 重新生成。
- `git diff -- cmd/hufu internal` 不得出現為特定 agent team 名稱寫的 core policy。
- 沒有新增 SQLite migration；沒有新增 store。
- 新程式遵守 800 行上限；`sqlite_repository.go` 與 `contextcmd.go` 的淨增行數只限必要的呼叫點。

完成定義：consolidation 的建立、核准、拒絕都是單一交易且可重跑；沒有任何通用 API 或 CLI 能旁路保留 source type；相同內容不能跨 type 劫持候選，操作者 reject 不被推翻；來源 supersede 時合併知識在同一交易內離開 prompt；doctor 唯讀回報所有 reason code；`explain-memory` 與 runtime 選取一致、不越權、不輸出內容、不改排序，並揭露 BUG-01；BUG-12、BUG-13 已修；上述驗證全部通過。

## 9. 實作後的文件更新

- `docs/architecture/memory-learning.md`：HF-MEM4-005（explain 新欄位、`base_relevance` 語意、`not_evaluated`、BUG-01 說明）；HF-MEM4-006（交易語意、`stale` 狀態與連動規則、D7、doctor）。
- `docs/reference/context-sqlite-schema.md`：`consolidation_proposals.status` 的值（新增 `stale`，不需 migration）與 `consolidation_*` context events。
- `docs/reference/operator-command-reference.md`：如 §8 需要則重新生成。
- 正式文件不得引用本檔。

## 10. 後續工作（需要人工或真實資料，不在本計畫）

- **BUG-01 修正（ranking 實驗）**：coding agent 無法單獨判斷改成純 RRF 後的排序品質，需要真實 workspace 的 replay 資料。建議做法：WP-5 完成後，在 2～3 個真實 workspace 上以 `explain-memory` 取樣記錄現行排序；另開計畫，以 memory policy snapshot 的 fusion 參數提供純 RRF 選項（預設舊行為），用既有 `hufu improve` replay／experiment 流程比較選取差異與任務結果；有證據後再決定是否採用新 policy。
- **BUG-10 一般行為**：`Append` 去重改寫 `source_json` 的行為自初版（`85c8499`）即存在，沒有記錄理由，可能有呼叫端依賴。改動前需要盤點所有 `Append` 呼叫端並決定 provenance 語意。
- **OBS-1～OBS-5**：需要先決定期望語意（例如失敗 run 的 findings 是否應在同 session 可見），再各自立項。

## 11. 實作紀錄

Baseline（`3fae664`，即 `489db51` 加上本計畫文件）：`go vet ./...` 通過；`golangci-lint run ./...` 0 issues；`go test ./...` 43 個 package 通過。`cmd/hufu` 在 scratchpad git worktree 中因子程序 `go build` 讀不到 VCS 狀態（`error obtaining VCS status`）失敗，以 `GOFLAGS=-buildvcs=false` 重跑後通過，屬環境問題，不是邏輯失敗。在主 checkout 執行時不需要該旗標。

| WP | Commit | 內容 |
| --- | --- | --- |
| WP-1 | `b1cefdd` | `CreateConsolidationProposal`／`ApproveConsolidationProposal`／`RejectConsolidationProposal` 單一交易；`evaluateConsolidationQ` 共用新鮮度判斷；CLI 與 improve handoff 改用；移除 `SaveConsolidationProposal`、`UpdateConsolidationProposal`、`ConsolidationRepository` |
| WP-2 | `8007aaa` | 保留 source type、`UpdateLifecycle` 只允許 candidate→rejected、CLI `confirm`／`reject` 指向 consolidation 指令 |
| WP-3 | `00563cb` | `UpsertCandidate` 跨 source type 回 `ErrCandidateIdentityConflict`、操作者 reject 回 `ErrOperatorRejected` |
| WP-4 | `bc18d79` | `demoteDerivedConsolidationsTx`（接在 `MarkSuperseded` 與 `confirmCandidateTx` 的 supersede 迴圈）、`consolidation show` freshness、`hufu context doctor --consolidation`、handoff 把 `stale` 視為已變更 |
| WP-5 | `a3d3de8` | `RetrievalObservation`、`rankPersistentMemory` 排序核心、`evaluateLifecycleEligibility`、`ExplainPersistentMemory` 與 CLI |
| WP-6 | `b9e1b5c` | `RepositoryQuery.IDs` 與 `context_get` 修正；chromem 搜尋逐步放大鄰居視窗 |

與計畫的差異：

- BUG-15 的 handoff 驗證在 WP-1 就改用共用判斷（WP-1 移除了 `validateConsolidationSources`，不能等到 WP-4）；WP-4 只補上 `stale` 狀態處理。
- `ConsolidationSupportPolicy` 的零值代表不檢查 support 門檻。production 一律傳入 memory learning policy 的非零門檻；improve handoff 測試 fixture 的來源只有曝光證據，因此用零值。
- `ConfirmCandidates` 的迴圈主體抽成 `confirmCandidateTx`，公開方法在呼叫前檢查保留 source type；三處重複的 INSERT 抽成 `insertItemRowTx`，`normalize` 的內容正規化抽成 `NormalizeContent`。
- WP-5 另加 `ranking.injected`（`selected` 或 router 會強制加回的 must-keep）。off／observe mode 的 `score_parts`／`final_score` 另以同一組候選計算 reinforced 分數作顯示，不影響選取判斷。
- 為了證明重構不改變 prompt 選取，新增行為鎖定測試 `TestPersistentRankingBehaviourIsLocked`（`internal/team/testdata/memory_ranking_lock.golden`），在重構前以舊程式產生，涵蓋四種 mode、goal-line fallback、allowed subset 與 content 去重。
- `hufu context doctor --consolidation` 的 JSON 另含 `orphans`；能產出報告就 exit 0，讀取失敗才回非零（與計畫相同）。

驗證：每個 WP commit 前都跑過 `go test ./...`、`go vet ./...`、`golangci-lint run ./...`，全部通過；`go test -race ./internal/context -run 'Consolidation|Reserved|Upsert|Demot|Supersed'` 通過。`docs/reference/operator-command-reference.md` 的生成內容不受影響（`TestGeneratedOperatorCommandReferenceIsCurrent` 通過）。正式文件已依 §9 更新 `docs/architecture/memory-learning.md` 與 `docs/reference/context-sqlite-schema.md`。
