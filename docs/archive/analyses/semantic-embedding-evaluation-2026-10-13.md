# Embedding 模型 CPU 實測與語意檢索命中品質評估

> Status: archived
> Authority: reference（評估紀錄；決策與現行行為見 [實作紀錄](../implementation-plans/semantic-retrieval-runtime.md) 與 [Semantic memory retrieval](../../reference/semantic-retrieval.md)）
> Baseline: `b760831c`（命中品質）；CPU 實測在實作前
> Date: 2026-10-13

三項評估：(1) `qwen3-embedding:0.6b` 與 `embeddinggemma` 在本機 CPU 的效能；(2) 英文資料（hufu-code-review）的命中品質；(3) 中文資料（web-research）的模擬命中品質。資料全部來自 hufu 歷史，本文件只含統計；含內容的資料集、逐筆選取與評分在評估後已刪除。CPU 實測的腳本在 [`semantic-embedding-evaluation-2026-10-13/scripts/`](semantic-embedding-evaluation-2026-10-13/scripts/)。

## 1. CPU 實測

### 環境與資料

| 項目 | 值 |
|---|---|
| 機器 | VM，6 vCPU（Intel i7-6700K @ 4.00GHz，AVX2／FMA，無 AVX-512），31 GB RAM |
| Ollama | 0.40.3，CPU-only；實際用到約 3 顆核心 |
| hypervisor steal | 平均 8.4%，最高 29%（每 5 秒取樣） |
| qwen3-embedding:0.6b | Q8_0，digest `ac6da0dfba84…`，1024 維，`num_ctx` 4096 |
| embeddinggemma:latest | `300m-bf16`，768 維，`num_ctx` 2048（模型上限） |

Query 前綴：qwen 用 Instruct 指令（27 token）；gemma 用官方 `task: search result | query: `（10 token），文件前綴 `title: none | text: `。

- 文件：所有 workspace `context.sqlite` 的記憶，以 content hash 去重後 1,403 筆（149 筆含中文；長度 p50 219 字、p95 975 字、最長 2,003 字），其中 project／team 範圍、confirmed 或 candidate 的只有 35 筆。
- Query：`session.json`／`session_tree.json` 的 task 描述去重後 714 個（94 個含中文）。延遲測試隨機抽 300 個，照 `ContextRequest.RetrievalQuery()` 格式組成。
- 品質參考：以 `task_result_reducer` 記憶的 `run:task:attempt` 對回 task，得到 16 組「task 描述 → 該 task 的結果」配對（4 組中文）。

### 延遲與吞吐

| 項目 | qwen3-embedding:0.6b | embeddinggemma |
|---|---|---|
| 處理速度 | 約 70 token/s | 約 375 token/s |
| 預載（`input: []`）p50／p95 | 1.84／2.11 秒 | 1.84／2.10 秒 |
| 預載後第一個 query p50 | 2.66 秒 | 0.55 秒 |
| 不預載、直接冷查詢 p50／max | 5.49／6.25 秒 | 2.25／2.45 秒 |
| 真實 query p50／p90／p95／p99／max | 1.20／2.60／4.15／15.6／64.7 秒 | 0.20／0.46／0.71／1.87／3.08 秒 |
| 超過 3 秒 | 27／300（9%） | 1／297 |
| 超過模型上限而回 400 | 0 | 3／300（>2048 token） |
| query token p50／p95／max | 86／314／3,224 | 72／282／876 |

| query token | qwen p50／p95 | gemma p50／p95 |
|---|---|---|
| 0–128 | 0.98／1.63 秒（n=210） | 0.17／0.34 秒（n=244） |
| 128–256 | 1.99／3.42 秒（n=68） | 0.43／0.59 秒（n=35） |
| 256–512 | 4.17／6.26 秒（n=14） | 0.85／1.31 秒（n=13） |
| 512–1024 | 10.9／14.6 秒（n=5） | 1.98／3.03 秒（n=5） |
| >1024 | 50.1／63.2 秒（n=3） | 超過上限 |

| 併發 worker（48 個 query） | qwen 吞吐／p95 | gemma 吞吐／p95 |
|---|---|---|
| 1 | 0.64 q/s／2.8 秒 | 4.68 q/s／0.37 秒 |
| 2 | 0.72 q/s／5.4 秒 | 4.80 q/s／0.89 秒 |
| 4 | 0.70 q/s／13.0 秒 | 4.71 q/s／2.66 秒 |
| 8 | 0.71 q/s／19.3 秒 | 3.28 q/s／4.22 秒 |

吞吐量不隨 worker 數增加，多出來的請求只是排隊；batch 大小（1–32）也沒有影響。輸入長度：qwen 182／1,152／3,899 token 分別約 2.5／17.7／86.6 秒，gemma 111／1,107／1,848 token 約 0.48／3.19／6.31 秒。

### 建索引與資源

| 項目 | qwen | gemma |
|---|---|---|
| 1,403 筆全量（batch 8） | 35 分鐘（0.67 筆/秒） | 7 分鐘（3.33 筆/秒） |
| Ollama RSS 高峰／`/api/ps` 模型大小 | 3,629／2,261 MB | 2,649／649 MB |

### 品質參考（16 組，只看方向）

| 方法 | 中文 recall@5（n=4） | 英文 recall@5（n=12） | MRR@10 |
|---|---|---|---|
| ASCII BM25（模擬 `ftsQuery`） | 0.25 | 0.58 | 0.21 |
| qwen（RetrievalQuery 格式／只用 goal） | 1.00／1.00 | 0.33／0.33 | 0.32／0.37 |
| gemma（RetrievalQuery 格式／只用 goal） | 1.00／1.00 | 0.42／0.42 | 0.35／0.36 |

在全部 1,403 筆裡找的版本不可用：英文 task 有 37–50 筆 STM 進度紀錄逐字引用 task 描述，卻沒有被標成相關。

### Token 估算校準

120 個真實 task 描述（中英各 60），以 Ollama 的 `prompt_eval_count` 擬合 `token ≈ a × 中文字元 + b × 其他字元`：qwen 0.93／0.25（p95 保守值 1.31／0.36），gemma 0.85／0.26（1.22／0.37）。採共用的保守估算 `中文 × 1.3 + 其他 × 0.4`。

### 結論

- embeddinggemma 快約 5 倍；qwen 有 9% 的真實 query 超過 3 秒，需要 query 預算。預設模型改為 embeddinggemma。
- 輸入上限依模型：qwen 4096、gemma 2048（D4）；語意 query 只用 goal（D3）。
- 不同硬體要重測；建議 `query-max-tokens ≤ query-timeout × 實測 token/s × 0.5`。

## 2. 英文資料命中品質（hufu-code-review，離線重播）

hufu-code-review 使用 fresh-session、memory learning off，review task 都在 VERIFY phase，真實 run 不讀 canonical 記憶，所以改用正式 router 對 workspace 副本重播：

- 候選 35 筆 confirmed shared 記憶；200 個不重複歷史 task 描述（2 個含中文）；以 EXECUTE／task_dispatch 重播。
- 判斷：每組（task, 選入的記憶）由 minimax-m3:cloud 打分——2 直接有用（提供這個 task 會用到的事實、限制、決策或教訓）、1 同領域但不太會改變做法、0 無關。

| 設定 | useful@4 | related@4 | 平均分數 | 至少 1 筆有用的 task |
|---|---|---|---|---|
| off／legacy（當時預設） | 0.352 | 0.882 | 1.235 | 148 |
| active／legacy | 0.351 | 0.880 | 1.231 | 145 |
| off／score_normalized | 0.357 | 0.858 | 1.215 | 151 |
| active／score_normalized | 0.336 | 0.860 | 1.196 | 136 |

- legacy fusion 下語意只改變 25／200 個 task 的選取：carried 的 BM25 原始分數壓過向量清單。
- score_normalized 下語意改變 43 個 task，但換進來的 34 筆 useful 0.088、被換掉的 0.588；逐 task off 較好 17、active 較好 0。
- 結論：在幾乎全英文的資料上，embeddinggemma 的語意排序比 BM25 差。

## 3. 中文資料命中品質（web-research，模擬）

web-research 與 article-debate 的記憶全是 session 範圍，正式檢索不排序它們。在 workspace 副本中把 candidate／confirmed 記憶改成 project／team 範圍的 confirmed（內容不變），用正式 router 以歷史 task 描述重播，判斷方式同上；同一次 run 的記憶另外標記。

web-research：54 個 task（全部含中文），55 筆記憶（38 筆含中文）。

| 設定 | useful@4 | related@4 | 平均分數 | 至少 1 筆有用的 task |
|---|---|---|---|---|
| off／legacy | 0.472 | 0.880 | 1.352 | 47 |
| active／legacy | 0.472 | 0.880 | 1.352 | 47 |
| off／score_normalized | 0.495 | 0.894 | 1.389 | 46 |
| **active／score_normalized** | **0.694** | 0.963 | 1.657 | **54** |
| 只用語意前 4 名 | 0.699 | 0.991 | 1.690 | 51 |

- 排除同一次 run 的記憶後結論不變（0.468 vs 0.704）。
- legacy 下開語意，選取完全沒有改變。
- score_normalized 下開語意：40 個 task 選取改變，active 較好 25、off 較好 4；換入的 80 筆 useful 0.762，被換掉的 0.225。
- 中文 goal 的 FTS 退化成只比對 `phase:`／`trigger:`／`role:` 等 metadata 詞：16 個 task 選到的正是只用這些詞搜尋的前 4 名。
- 依非 ASCII 比例分組：< 0.15 的 5 個 task 兩種設定相同（0.70）；≥ 0.15 的 49 個 task 語意明顯較好。若用 `auto`（門檻 0.15）推算，web-research 為 0.694、hufu-code-review 為 0.352（全部走 lexical，等同 off）。

article-debate 無法評估：39 個 task 都含同一個識別符（被辯論文章的路徑），exact lane 的前綴佔滿 4 個名額，所有設定選取相同。這是刻意設計（識別符比對優先）。

## 4. 限制

- 單機 VM，有 hypervisor steal；gemma 用 BF16 而非 Q8。
- 單一 LLM 判斷、無人工標註；中文只有 54 個 task，且是把 session 記憶模擬成 shared。
- 判斷用的重播程式是一次性的 Go 測試，沒有保留；目前可以用 `hufu context ranking-replay --candidate-semantic-mode` 重做選取比較。

## 5. 重現 CPU 實測

```bash
cd docs/archive/analyses/semantic-embedding-evaluation-2026-10-13/scripts
python3 -I extract.py /path/to/private/dataset.json        # 從 ~/.local/state/hufu 建資料集，只印統計
python3 -I bench.py /path/to/private/dataset.json /path/to/out qwen3-embedding:0.6b embeddinggemma:latest
python3 -I proxy_results_only.py /path/to/private/dataset.json /path/to/out/proxy_results_only.json
python3 -I calibrate_tokens.py /path/to/private/dataset.json /path/to/out/token_calibration.json
```

資料集含記憶與 task 內容，請放在私有目錄、用完刪除。`BENCH_QUICK=1` 可先用縮小資料做 smoke test；完整執行約 90 分鐘。
