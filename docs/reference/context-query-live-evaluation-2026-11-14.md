# Context query：真實模型配對評測與校準實驗（2026-11-14）

> Status: active
> Authority: reference
> Verified-Commit: — (working tree; source baseline `0b4f4eac23c0bfa1d031ef3437ff59ee31512f27`; source hashes in the JSON report)
> Supersedes: —
> Superseded-By: —

## 結論

本次完成使用者授權的 bounded real-model A/B、代表性合成任務評測、推論成本比較及評測集自動校準實驗。模型固定為 `ollama/deepseek-v4.1-flash:cloud`。**修正版 active 答對 24/24，legacy 答對 9/24；校準候選未達採用門檻，未套用任何 production 設定。**

這是 context-query 邊界的模型品質 pilot：先由真實 Hufu runtime 在獨立合成 workspace 產生收到端 task／memory fragments，再用相同 benchmark system、兩欄 data-only tool contract 向真實模型推論。Coordinator、前置 producer、驗證與收據的生成維持 scripted；不是該模型執行完整 Hufu CLI／自由多代理工作流程的驗收。這些資料也不能代替正式 decision Phase 5 要求的真實已結案決策。

[機器可讀報告](context-query-live-evaluation-2026-11-14.json)包含兩批原始評分、每次呼叫的 token／latency／hash、事前 protocol、source hashes 與校準結果。原始 HTTP body 與私有驅動保留在 `/tmp/codex-task.yUSnZb`，沒有發布憑證。

## 範圍與事前契約

- 使用者先授權 48 次呼叫；失敗 pilot 的契約修正後，另行明確授權追加 48 次。總共 96 次，HTTP 200 共 96 次；沒有自動重試或額外探測。
- 每批 8 類 × 3 個獨立合成 instance × 2 arms = 48 次；每次最多 90 秒、2,048 輸出 tokens，每批預留上限 98,304 輸出 tokens。
- Legacy 使用既有 goal-only query；active 使用 team 的 `context-routing.query-mode: active`。兩組的 seed、gold answer、資源上限、生成設定與判分規則相同；先後順序交錯並平衡。
- Runtime 擷取時檢查目標 `current_task` 與 worker 身分，避免把 dependency／workset 的前置 worker 當成受測者。其他 worker 的 synthetic 記憶不得越過 provider 邊界；兩批準備均通過該檢查。
- 修正版將 runtime 的 task／memory fragments 原樣放入 JSON evidence；其中的 agent／memory reporting 片段是資料，輸出指令只來自共同 benchmark system，並與 `submit_eval_result(answer, confidence)` schema 一致。
- 嚴格接受單一 tool call、精確欄位、有限且介於 0～1 的 confidence；不補 JSON、不刪除多餘欄位、不改答案、不增加修補輪。
- Replicate 0、1 為訓練集，replicate 2 為保留集；一個 task pair 不跨 split。Calibration grid 與 gate 在修正版呼叫前固定。

## 修正版品質結果

| 類別 | Legacy 正確 / 3 | Active 正確 / 3 |
|---|---:|---:|
| Constraints 選取規範 | 0 | 3 |
| Verifier 選取規範 | 0 | 3 |
| 已接受依賴選取規範 | 0 | 3 |
| Workset 路徑選取規範 | 0 | 3 |
| Goal-only 正向控制 | 3 | 3 |
| 無 hint 相容控制 | 3 | 3 |
| 其他 worker 記憶隔離 | 3 | 3 |
| 中文精確規範名稱 | 0 | 3 |

整體精確答案成功率：legacy 9/24（37.5%），active 24/24（100.0%）。差值 +62.5 個百分點；以 task pair 為單位的 10,000 次 bootstrap 95% 區間為 +41.7～+79.2 個百分點。這是固定合成資料集條件下的區間，不代表自然工作負載母體。

Active 的 24 筆均通過格式契約；legacy 通過 23/24。`dependency-2-legacy` 用完 2,048 輸出 tokens，finish reason 為 `length`，沒有 tool call：它保留為任務失敗，不重試、不補答案；也不虛構 confidence 納入校準。

中文類別驗證的是固定中文名稱的 exact/lexical signal 與答案送達，不是純中文自然語意召回率。正向、無 hint 與授權控制均未回歸。

## 推論耗時與成本

| 指標（每 arm 24 次，含失敗） | Legacy | Active |
|---|---:|---:|
| Input tokens | 31,370 | 31,449 |
| Cached input tokens（包含於 input） | 9,728 | 10,112 |
| Output tokens | 7,584 | 3,244 |
| 推論 p50 | 1.412 s | 0.853 s |
| 推論 p95 | 4.258 s | 1.923 s |
| Peak 標價估算 | US$0.01565177 | US$0.01035457 |
| Off-peak 標價估算 | US$0.00782588 | US$0.00517729 |

修正版的 active 推論總估算成本較 legacy 低 33.8%；這包含失敗嘗試與實際 cache 用量，不能直接稱為 production 節費。耗時只量單次 HTTP 推論，沒有把 scripted runtime 準備或完整 live coordinator 成本算進來。

依 [Ollama 官方價格表](https://ollama.com/pricing)於 2026-11-14 核對：peak 每百萬 input／cached input／output tokens 為 US$0.30／0.006／1.20，off-peak 為 US$0.15／0.003／0.60。報告以 API 回傳 usage 計算，cached input 從總 input 扣除後分開計價；未回報 cache 的部分保守按未快取計。這是標價估算，不是帳戶扣款或帳單證據。

兩批合計 141,499 input tokens、38,854 output tokens；估算合計 off-peak US$0.04162077，peak US$0.08324154。失敗 pilot 的成本單獨保留，不混入修正版的 A/B 指標。

## 自動校準結果

對兩個 arms 的有效 confidence 混合資料做 temperature scaling；訓練集 32 筆，保留集 15/16 筆。只在訓練集比較事前固定的 temperature 0.25、0.5、1、2、4、8，選出 **temperature 4**；沒有用保留集選參數。

| 保留集指標 | 原始 confidence | 校準候選 |
|---|---:|---:|
| Brier（越低越好） | 0.239633 | 0.201261 |
| ECE（5 bins） | 0.215333 | 0.048710 |

Brier 差值（候選－原始）的 paired case bootstrap 95% 區間為 **[-0.129547, 0.062866]**，跨過零，未通過事前 gate。因此結果是 **`candidate_not_adopted`**。雖然點估計變好，資料仍不足以認定改善穩定。

這是混合兩 arms、有效格式回應條件下的評測集候選，不能直接套用到 active 或其他模型／任務。未輸出的那一筆仍算任務失敗，不補假 confidence；保留集校準覆蓋率是 15/16。自動選參、保留集檢查與拒絕採用均已實際執行；沒有改 confidence 原始值、正式路由、模型權重或 production 設定。正式 decision outcome 校準仍依使用者選擇另案累積真實結果。

## 失敗 pilot：保留且不改判

第一批 48 次呼叫的原始參數 JSON 都可解析，但只有 2 筆符合事前的縮窄 schema。46 筆帶有未允許的 `memory_uses`（部分另含 details／findings 等欄位）。原因是評測驅動保留 runtime 要求回報記憶使用資訊的 system 指示，卻把工具 schema 縮成 status／summary／confidence，造成契約衝突。這是評測設計問題，不能由這批主評分推論模型品質。

修正只作用於下一批的 benchmark 契約；原 protocol、requests、responses、主評分、費用與 hashes 全部保留。修正版沒有刪除模型回傳的額外欄位或放寬接受條件來讓舊結果通過。

## 驗證與限制

- 48 組原始 runtime capture 的離線 fixture 驗證通過；修正版事前用 Draft 2020-12 schema 檢查正反例與額外欄位拒絕。
- 兩批 live 驅動皆完成：第一批 119.60 秒，修正版 71.20 秒；共 96 次 HTTP 200，所有 usage 都在單次及總輸出 token 上限內。
- 每次請求與回應有 SHA-256；dataset、driver、source manifest 與完成 ledger 分別保存。重算比較與校準不呼叫模型。
- 獨立重算 96 次呼叫的 usage、固定 seed 的 gold、配對與校準選參均一致；`bin/check-docs`、`mise run lint`（0 issues）及 `git diff --check` 通過。來源未變更的完整 tests／contracts／vet 沿用前輪結果。
- 相較前一輪已驗證來源，本次沒有修改 production Go、agent team 或既有設定。私有 Go overlays 僅作實驗，不把 offline `hufu eval` 改成可連外入口。
- 樣本小、答案人工可判定、任務是 synthetic context-query proxy；不證明 production 任務成功率、整體 agent 品質、完整端到端效能或所有中文情境。
- 模型是 cloud alias，不是不可變權重快照；token／cache／延遲與價格均可能隨環境變動。
- 本紀錄驗證的是實驗已執行及其觀察結果，不把未達 gate 的校準宣稱為已驗收。
