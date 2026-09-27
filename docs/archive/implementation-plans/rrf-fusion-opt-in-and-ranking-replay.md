# RRF fusion opt-in 與 ranking replay：實作計畫

> Status: in progress — implementing on branch `feat/rrf-fusion-replay`
> Authority: reference（實作計畫；現行行為以程式、測試與 [memory learning](../../architecture/memory-learning.md) 為準）
> Verified-Commit: `38e7bf1`
> Supersedes: —
> Superseded-By: —
> Scope: [consolidation integrity 計畫](consolidation-integrity-and-retrieval-explain.md) §10 的 BUG-01 後續：opt-in 的正規化 RRF、`hufu context ranking-replay`
> Date: 2026-09-27

## 1. 背景與決策

BUG-01：`rrf` 以第一個清單的原始分數起算（`current = result`），lexical 命中時 fused score 由原始 BM25 主導。runtime shared persistent 排序對 `0<s<1` 的分數 ×61，只在小語料（BM25 ≈ 0）時剛好等於「依 rank 正規化」，大語料時 base relevance 是原始 BM25（可 >1）。所以排序的尺度會依語料大小而不同。

查核（合成語料，未使用使用者資料）：

- 純 RRF（不正規化）讓分數約 0.016，MMR（λ=0.75）變成幾乎只看多樣性，排序偏離相關度，不可採用。
- 正規化 RRF（最佳名次為 1）在 lexical-only 時，順序與現行相同；runtime 的注入路徑都不用向量，所以 relevance 選取不變。
- 差異在 reinforced（active）排序：現行 base 是 BM25 強度，正規化後 base 只看名次，utility 乘數比較容易壓過弱的 lexical 命中。這可能改善 positive transfer，也可能違反 L3 的 irrelevant-high-utility gate，必須以 replay 證據判斷。
- 本機真實 workspace 的 shared persistent 記憶都只有 0–5 筆，沒有 aggregate、manifest 或 ranking trace，目前無法以真實資料判斷優劣（使用者資料的讀取也不在本次權限內）。

決策（使用者確認）：

- **D1** 加入 opt-in 的 fusion 參數，預設沿用現行行為；production 排序不變。
- **D2** 正規化 RRF 的 runtime base relevance 不再 ×61，只截在 [0,1]。
- **D3** 新增唯讀、不輸出內容的 `hufu context ranking-replay`，比較現行與候選 fusion 的選取差異；有 aggregate 時另比較結果指標。以合成／benchmark fixture 驗證。
- **D4** 是否切換預設，由使用者在自己的 workspace 累積足夠記憶後執行 replay 再決定；本計畫不切換，也不新增建立 policy candidate 的 CLI。

## 2. 設計

### 2.1 Fusion mode（`internal/context`）

```go
type FusionMode string

const (
    FusionLegacy        FusionMode = "legacy"         // 預設（空字串同義）：BUG-01 行為
    FusionRRFNormalized FusionMode = "rrf_normalized" // 不夾帶原始分數，依非空清單數正規化到 (0,1]
)
```

- `HybridRetrievalOptions.Fusion`；未知值由 `validateHybridRetrievalOptions` 拒絕。
- 正規化 RRF：`score = Σ_list 61/(61+rank₀) / nonEmptyLists`；所有非空清單都排第一的項目得 1.0。lexical-only 時 rank 1 = 1.0、rank 2 = 0.984。只出現在一個清單、另一清單非空時，第一名為 0.5。
- 公開的 `rrf(lists...)` 行為不變（legacy）。observer 在正規化模式下記錄 `carried_score = 0`，恆等式 `fused = carried + lexical_rrf + vector_rrf` 仍成立。
- exact 前綴、MMR、file-path boost 不變。

### 2.2 Runtime 與 policy（`internal/team`、`internal/improve`）

- `MemoryRuntimeRankingPolicy.Fusion`；`LoadMemoryPolicy` 讀 snapshot 的 `retrieval.fusion`，未知值使整個 policy 無效（與其他參數相同）。
- `rankPersistentMemory`：把 fusion 傳給 `HybridRetrieveWithOptions`；legacy 保留 ×61 規則，正規化只把分數截在 [0,1]。
- `effectiveRankingPolicy` 的 fallback 保留 fusion。
- `explain-memory` 輸出 `fusion`。
- `improve.MemoryRetrievalPolicy.Fusion`（`json:"fusion,omitempty"`，既有 snapshot 的 revision hash 不變），並驗證其值。
- worker memory recall、`memory_query` tool 與 `hufu context query` 不在範圍內，維持 legacy（非目標，見 §4）。

### 2.3 `hufu context ranking-replay`

```text
hufu context ranking-replay --workspace <w> --project <p> --team <t>
    (--query <q> ... | --queries-file <f> | --from-session)
    [--candidate-fusion rrf_normalized] [--policy-version <v>] [--json]
```

- 以 `OpenSQLiteReadOnly`（`mode=ro`、`query_only`）開啟，永不寫入 context store，也不寫 trace。
- eligible 集合與 `explain-memory` 相同（shared persistent projection 加上與 request 無關的 eligibility）。
- 每個查詢各跑兩種 ranker：`relevance`（observe 語意）與 `reinforced`（active 語意），並比較現行 fusion（policy 或預設）與候選 fusion 的選取結果。
- 每個查詢、每個 ranker 輸出：兩邊選取的 ID（依名次）、Jaccard、新增／移除、兩邊最大 base relevance。
- 有任何 eligible item 帶 aggregate 時，另輸出結果指標：選取中有 verified support、causal failure、negative weight 的筆數，以及平均 utility lower bound。
- `--from-session`：以 `session.json` 每個 task 的 goal（無 goal 時用 description），依 dispatch 格式組出 `ContextRequest.RetrievalQuery()`，是近似重建。
- 只輸出 ID、query hash、數值與 enum，不輸出查詢文字或記憶內容。

## 3. 交付與驗收

1. **Stage 1 fusion 機制**：context、team、improve 三處與測試；`TestPersistentRankingBehaviourIsLocked` 在預設下必須不變。
2. **Stage 2 replay**：`internal/team` 的 replay 核心與 CLI；測試涵蓋唯讀（檔案 hash 與 Revision 不變）、不輸出內容、合成 L3 情境（positive transfer、irrelevant high utility、stale／harmful）。
3. **Stage 3 文件**：`docs/architecture/memory-learning.md`，並在本檔加上實作紀錄與合成 replay 結果。

每階段都要通過 `go test ./...`、`go vet ./...`、`golangci-lint run ./...`。

## 4. 非目標

- 切換預設 fusion 或建立／採用 fusion policy candidate。
- worker memory recall、`memory_query`、`hufu context query` 的 fusion。
- 改變 MMR λ、exact 前綴或 `MinimumRelevance` 的語意。
- 讀取使用者真實 workspace 資料（由使用者自行執行 replay）。
