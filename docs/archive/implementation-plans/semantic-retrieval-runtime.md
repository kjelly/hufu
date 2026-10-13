# Semantic retrieval runtime（Ollama embedding）：實作紀錄

> Status: implemented — P1–P4、語言自動路由、可觀測性、explain／replay 重播與 doctor 檢查已實作（`d39ca72c..3ea2749c`，已 push）；P0、P5 的 eval suite、P6 的正式 benchmark、P7 canary 手冊與 P8 release 不做或延後（§6）
> Authority: reference（實作紀錄；現行行為以 [Semantic memory retrieval](../../reference/semantic-retrieval.md) 與 [context.sqlite schema](../../reference/context-sqlite-schema.md) 為準）
> Verified-Commit: `3ea2749c`
> Review baseline: `2319d2a1`（原計畫）、`25ccee12`（實作起點）
> Supersedes: —
> Superseded-By: [Semantic memory retrieval](../../reference/semantic-retrieval.md)
> Scope: 讓 coordinator 與 worker 的 canonical 記憶檢索在 exact＋FTS5 之外，選用本機 Ollama embedding 模型的語意 lane
> Date: 2026-10-13

原計畫是 gitignored 的草稿 `docs/tmp/spec.md`（「Qwen3 Embedding 語意記憶檢索與 Just-in-Time Context Delivery 改善計畫」，P0～P8），以 `2319d2a1` 為 baseline。2026-10-13 依維護者要求改寫成符合現況的紀錄並移入 archive。評估數據見 [semantic embedding evaluation](../analyses/semantic-embedding-evaluation-2026-10-13.md)。

原計畫的不變原則全部維持：`context.sqlite` 是唯一 source of truth、向量只是可重建的 projection；off 不送任何請求；shadow 不改變選取；授權在 top-K 之前；取消與 canonical 錯誤不當成 fallback；trace 不含內容。

## 1. 結論

- 問題成立：production 沒有 vector lane（`NewWorkerMemoryService(repo, nil)`、shared ranking 不帶 `VectorSearcher`），而 `ftsQuery` 只保留 ASCII 詞，中文 goal 讀不到中文記憶。
- 計畫縮小後實作：P1（adapter）、P2（builder／index lifecycle 與 B1 修正）、P3–P4（shared 與 worker 接線、shadow／active）。P5 以 Go 整合測試取代 eval YAML；P6–P8 未照原樣做。
- 預設模型改為 `embeddinggemma`（CPU 上比 `qwen3-embedding:0.6b` 快約 5 倍），legacy `embedding-model` 預設同步更改。
- 命中品質：語意對中文明顯有效（useful@4 0.472 → 0.694），對英文無益或略差（0.352 → 0.336），而且只在 `score_normalized` fusion 下有作用。因此新增 `mode: auto`：依 query 的文字種類逐請求決定是否使用語意 lane。
- 全域預設仍是 `off`；web-research team 設為 `active`＋`score_normalized`。

## 2. 決策

| ID | 決策 |
|---|---|
| D1 | 冷啟動另外處理：背景以 `input: []` 預載並用 `/api/ps` 驗證 digest 與 context length（cold → warming → ready／failed）；未就緒直接走 exact＋FTS5（`embedding_warming`），載入時間不算進 query timeout；ready 後逾時以 singleflight 重新預載。 |
| D2 | 每個請求明確送 `options.num_ctx` 與 `truncate: false`；超長回 typed `ErrEmbeddingInputTooLong`，文件記為 oversize exclusion 只走 lexical，不讓 index 判為損壞。 |
| D3 | Query 長度預算 `goal-head-v1`：語意 lane 只嵌入 goal（不含 `phase:`／`trigger:`／`role:` 行），以「中文字元 × 1.3 + 其他 × 0.4」估算並在句子邊界截短；排隊時間計入 query timeout，不足時 `embedding_busy`。 |
| D4 | `max-input-tokens = min(4096, 模型上限)`：qwen 4096、embeddinggemma 2048，以 `/api/show` 驗證。 |
| B1 | 阻擋點：原本以全庫 canonical revision 判斷新鮮度，run 中任何寫入都讓 index stale。改為逐項 `(item_id, content_hash)` 新鮮度、增量 refresh 與 copy-forward、`source_revision` 單調啟用；授權候選有未覆蓋項目時整個請求走 lexical（`projection_incomplete`）並通知 refresher。 |
| O1 | 新設定與 legacy `embedding-model` 的預設都改為 `ollama/embeddinggemma:latest`。 |
| O2 | worker private 記憶納入 v1（推翻原計畫建議）：inventory policy `retrievable-v1` 涵蓋同 project 所有 scope 的 candidate／confirmed、未 superseded 項目。 |
| O3 | `mode: auto` 與 `fusion` 設定（§3.5）；web-research 設為 active＋`score_normalized`；使用者的全域 hufu.yaml 設為 `auto`（repo 預設仍為 off）。 |

## 3. 實作內容

### 3.1 Encoder 與設定（`d39ca72c`、`ebd9257b`、`3de58196`）

- `internal/embedding`：Ollama `/api/embed` adapter（document／query 分離、batch、取消、L2 normalize、digest pin、`Preload`／`VerifyLoaded`、typed errors）；`EncoderProfile`（document 側欄位決定 generation 身分）與 `QueryPolicy`（query 前綴與預算，不進 generation）；embeddinggemma 與 qwen3-embedding:0.6b 的實測 preset。`ModelIdentity` 新增 identity scheme，Ollama 模型不冒充 static table hash。
- `internal/semantic`：`semantic-retrieval` 設定（mode、model、endpoint、預算、timeouts、並行數），hufu.yaml 全域、team.yaml 覆寫，非 loopback endpoint 需 `allow-remote-endpoint`。

### 3.2 Projection 與 builder（`ba24f50b`）

Migration 13：`identity_scheme`、`inventory_policy`、`context_embedding_exclusions`、新的 active 唯一鍵。`SemanticBuilder` 增量 build、copy-forward、oversize 逐筆重試；`VerifyActiveGeneration` 檢查完整性與覆蓋。

### 3.3 Runtime 接線（`31de16e1`）

`SemanticRetrievalRuntime`：背景預載、共用 embedding slot、query deadline、refresher（每 2 秒以全庫 revision 為觸發訊號、debounce 250ms、失敗指數退避）。Shared persistent ranking 與 worker recall 都接上語意 lane；active 時把授權 ID 帶入每條 lane 再做 top-K。Execution policy snapshot 只釘 active（後來加上 auto）的設定 hash，切換後舊 session 須 `--new`。Manifest 與 receipt 記錄 `semantic_retrieval` 身分。

### 3.4 維運指令（`4abc0bc4`、`b760831c`、`f00a2a8e`、`3ea2749c`）

`hufu context semantic status|build|verify`：status 與 `build --dry-run` 離線唯讀；build 不會 pull 模型；verify 檢查完整性、覆蓋與 encoder profile。設定採用一次 run 會用的設定（hufu.yaml 合併 `--team` 或 project 唯一 team 的區塊），並顯示來源。

### 3.5 語言自動路由（`15dde5a2`、`b0f316e5`）

- `mode: auto`：goal（worker recall 用其 query）中 lexical 會丟掉的部分——非 ASCII 字母與數字，以 token 估算加權——比例 ≥ 0.15 時走語意 lane，否則完全等同 off。只看字元的 script，不解讀文字。門檻依歷史資料校準：200 個英文 review goal 全部 < 0.15；54 個中文研究 goal 有 49 個 ≥ 0.15。
- `fusion`（預設 `score_normalized`）只用在使用語意 lane 的請求；shadow 與走 lexical 的請求維持 memory policy 的 fusion。
- web-research team 設為 `active`＋`score_normalized`。

### 3.6 可觀測性（`0aea7bc8`、`3b1396ac`、`c01ed7fb`）

- Trace 加上 run／task／attempt／context request／phase／trigger／role／agent／source、route 與 route share，以及 encoder 細節（覆蓋數、query tokens、截短、排隊、embedding 時間，經 context 中的 `SemanticQueryProbe` 帶回）；auto 走 lexical 的請求也寫 trace。
- Worker recall 改為只嵌入 task goal（原本含 metadata 行）。
- 每個 run 結束時寫 `semantic-runtime-status.jsonl`（計數為該 run 的差值）；`hufu inspect semantic-retrieval [run-id] [--task]` 與 report 的 Semantic Retrieval Runtime 區段。統計單位是 retrieval（goal 行重試會算兩次）。

### 3.7 explain／replay 與 doctor（`b9a3525f`、`bc18798a`、`1b2d338a`）

- `explain-memory` 與 `ranking-replay` 以唯讀的 `SemanticReplay`（不建 builder、不跑 refresher、不寫 trace，向量結果 memoize）重算語意 lane 與 auto 路由；`--semantic-mode` 覆寫模式，replay 的 `--candidate-semantic-mode` 可單獨比較語意 lane。
- `hufu doctor`：`semantic.config`、`semantic.encoder`（只讀 metadata，不載入也不 pull）、`semantic.legacy-embedding-model`。

## 4. 與原計畫的偏離

1. Generation 身分只含影響文件向量的欄位（digest、維度、document prefix、normalization、input limit、adapter version）；query 前綴與預算屬於 `QueryPolicy`，調整不需重新嵌入。
2. 沒有新增 `artifact_digest`／`profile_fingerprint` 欄位：以 `identity_scheme` 區分，`ollama-encoder-v1` 下 `model_hash` 是 Ollama digest、`model_revision` 是 profile fingerprint。
3. 設定照 `control-decisions` 模式（hufu.yaml＋team.yaml），沒有新增 `hufu run` 旗標。
4. 覆蓋不足時整個請求走 lexical，不做部分語意，避免新記憶在 fusion 中吃虧。
5. P5 以 Go 整合測試（概念向量 fake encoder）取代 `evals/context-jit` YAML case。
6. 原計畫 §4.1 寫「active 不得透過 `auto` 自動啟用」；`auto` 由操作者在設定中明確開啟、不會因模型可用而啟用，符合該條用意，但字面上不同，屬維護者決策。
7. `fusion` 預設 `score_normalized` 改變了 active 原本沿用 policy fusion（legacy）的行為；理由是 legacy 讓語意 lane 對選取沒有作用。

## 5. 驗證

- 每個 commit 經 pre-commit golangci-lint 與 `bin/check-docs`；最後一輪 `internal/team` 7,275 個測試、cmd/hufu 等全過，相關測試在 `-race` 下通過。
- 真實資料：11 個 workspace migrate 到 schema 13 並建立索引（約 530 筆，verify 全過）；embeddinggemma 對 20 個真實 goal p50 112ms、p95 346ms、0 fallback。
- 真實 LLM run（default team）：manifest 記錄 `semantic_retrieval`；refresher 自動補上 run 中新寫入的記憶；`inspect semantic-retrieval` 與 report 新區段在實機輸出正確。
- explain／replay 實機：在 hufu-code-review 的 35 筆 shared 記憶上，英文 query 走 lexical、中文 query 走語意；replay auto vs off 時英文選取不變、中文選取完全不同。

## 6. 未做與不做

| 項目 | 狀態 |
|---|---|
| P0 baseline 產出物 | 不做：以實作前 `go test ./...` 全綠為基線 |
| P5 `evals/context-jit` 與 §10.2 hard gates 逐項驗證 | 未做：以 Go 整合測試代替 |
| P6 正式 benchmark（200 筆人工標註 gold、Recall／MRR／nDCG、Nomic 對照） | 未做：以 CPU 實測與 LLM 評分重播代替 |
| P7 canary 與回滾手冊 | 未做：監測工具已具備（inspect、report、trace、explain／replay、doctor） |
| P8 release decision、每晚真實模型 CI | 未做；repo 預設維持 off |
| §9.2 剩餘 telemetry（compile／manifest／prompt 就緒耗時、模型載入與 refresh 耗時、每次查詢 oversize 數、trace 與 manifest 對帳） | 未做：等真實 run 有語意命中再評估 |
| `hufu run --semantic-mode`（原計畫 SHOULD） | 未做 |
| worker 私有記憶召回的 explain 工具 | 不在範圍內（lexical 也沒有） |

尚未在真實 run 驗證：真實 shared 記憶上的語意命中（web-research 的記憶仍是 session 範圍），以及 worker private 記憶走語意的路徑。
