# Agent Team 持續改善流程計畫

## 目標

建立一個以 workspace 執行資料為依據的持續改善閉環，讓 agent team 的角色、提示詞、模型、技能、工具與驗收方式能被有證據地調整。初期採 **human-in-the-loop**：系統只能提出可審核的候選變更，不可直接覆寫正式 team。

## 核心閉環

```text
執行任務
  → 蒐集 workspace telemetry
  → 偵測異常與趨勢
  → 診斷根因
  → 產生可審核改善提案
  → 以 benchmark 實驗驗證候選 team
  → 採納或拒絕，保留決策與學習
  └────────────────────────────────────→ 下一次執行
```

| 階段 | 工作 | 主要產物 |
|---|---|---|
| 1. 觀測 | 蒐集每次任務的 agent、重試、token、工具錯誤、驗證與 acceptance 結果 | 結構化執行資料 |
| 2. 偵測 | 以規則與跨執行趨勢尋找異常 | findings 清單 |
| 3. 診斷 | 對照 team 定義、prompt、skills、工具與任務結果提出根因假設 | 診斷報告 |
| 4. 提案 | 生成 versioned candidate patch，不改動正式 team | hypothesis 與 candidate |
| 5. 實驗 | 用固定 benchmark 對照 baseline 與 candidate | A/B 評估報告 |
| 6. 決策 | 依品質、安全與成本門檻採納、再測或拒絕 | decision record / PR |
| 7. 學習 | 保存有效與無效的假設、適用情境與證據 | 改善知識庫 |

## 可直接利用的既有資料

hufu 已經產生或支援下列基礎；改善流程應優先整合，而不是建立另一套互相矛盾的紀錄。

- `workspace/logs/execution-events.jsonl`：任務狀態、attempt、使用量等結構化事件。
- `workspace/logs/task_journal.jsonl`：可復用的任務結果紀錄。
- `workspace/logs/audit/`：工具呼叫與工具錯誤彙總。
- `workspace/session.json`、`stm.md`、`ltm.md`：執行與記憶上下文。
- `hufu --report`：執行報告與 verification evidence。
- `hufu improve`：不傳送 prompt／output／tool arguments 的 deterministic 改善報告。
- `--fix`：有上下文的 LLM 根因分析，應設為 opt-in 並經過資料遮罩。

## 建議的 workspace 產物結構

```text
workspace/
  logs/
    execution-events.jsonl
    task_journal.jsonl
    audit/
  reports/
    execution/
    improve/
  improvement/
    baselines/
    findings/
    hypotheses/
    candidates/
    experiments/
    decisions/
    benchmarks/
```

每個改善假設、候選版本、實驗結果與採納決策都應有不可變 ID，並連結到 run ID、team revision 與 benchmark revision。

## 偵測規則：第一批高價值訊號

先採用可解釋、可重現的規則，再逐步導入模型輔助診斷。

| 訊號 | 可能根因 | 優先檢查項目 |
|---|---|---|
| 重試率高 | 任務定義或完成條件模糊 | task 拆解、prompt、`verify`、agent 選擇 |
| 特定 agent 工具錯誤多 | tool 設定或指令使用不匹配 | tools、allowed paths、輸入格式、範例 |
| 逾時或 token 異常 | 模型不合適、任務過大、上下文過長 | model routing、`max-steps`、任務拆分 |
| 已完成但 verify / acceptance 失敗 | 交付物與驗收要求不一致 | acceptance、verify、review gate |
| 某類任務成功率低 | team 未依工作類型分流 | task taxonomy、skills、專責 worker |
| coordinator 重複派工或耗盡 rounds | 協調規則缺少停止與去重條件 | coordinator prompt、todo/依賴規則 |
| 高風險工具沒有具體 guard | 最小權限與安全規則不足 | tools、guard、`no-net`、`force-mcp` |

既有 `hufu improve` 已可偵測短 agent prompt、敏感工具未設 guard、工具錯誤偏高與 retry rate。下一步應將它由「最新一次 run」擴充為「最近 N 次 run 的分組趨勢」，並依 agent、任務類型、模型與 skill 分群。

## 指標與決策門檻

不要用單一成功率決定是否採納。每次實驗都應套用下列門檻，並先比較同一組 benchmark。

1. **硬性 gate**：安全違規為零；acceptance / verification 通過率不得下降。
2. **品質**：完成率、reviewer 缺陷數、人工抽樣評分、交付物完整度。
3. **效率**：重試率、端到端耗時、token、工具錯誤數。
4. **穩定性**：同類任務的結果方差，以及失敗是否集中於特定 agent。

建議採納條件：硬性 gate 全數通過，且品質提升或持平；若品質持平，必須有明顯的成本、速度或穩定性收益。任何安全或驗收退化都自動拒絕候選變更。

## 改善提案格式

改善系統不應只輸出泛泛建議；每項提案都必須可實驗、可追溯。

```markdown
# Hypothesis: H-023

症狀：developer 在重構類任務的重試率為 62%。
證據：最近 20 次執行共 13 次重試，主要失敗於 verify。
假設：任務缺少相容性與測試交付條件。

變更：
- developer.md：要求先列出受影響 API，再執行指定測試。
- coordinator.md：重構任務必須委派 reviewer。
- team.yaml：新增驗證，不增加 retry 預算。

預期：重試率低於 30%，acceptance 不下降。
風險：任務平均時間可能增加。
實驗：以 10 個保留的重構 benchmark，和 baseline A/B 比較。
```

## 實驗與變更管理

1. 將每個 team definition 納入 Git 版本控制，baseline 必須是可重建 revision。
2. 建立 candidate team 或 Git branch；candidate 必須以 patch 表示，不能直接修改正式設定。
3. 每個 task category 保留固定 benchmark，包括成功、失敗、邊界與安全案例。
4. 在相同模型版本、預算與 benchmark 下執行 baseline/candidate；記錄隨機性與環境差異。
5. 產出比較報告，列出 hard gate、指標差異、失敗樣本與成本。
6. 僅當明確門檻通過時建立 PR；PR 需附 hypothesis、evidence、實驗報告與 rollback revision。
7. 採納後持續監測 production runs；若品質退化，回退至 baseline 並建立反例。

## 改善專用 agent team

改善流程可由一個獨立 team 執行，但此 team 的權限預設應為唯讀，只有 candidate-editor 可以在指定候選目錄產生 patch。

| Agent | 責任 |
|---|---|
| `improvement-coordinator` | 排程分析、整合證據、提出採納/再測/拒絕決策 |
| `telemetry-analyst` | 分析 workspace telemetry、找出異常與趨勢 |
| `team-auditor` | 審視 team.yaml、roles、prompts、skills、tools 與 guards |
| `experiment-designer` | 將診斷轉成可驗證假設與 benchmark |
| `candidate-editor` | 僅在 candidate 範圍產生設定與 prompt patch |
| `evaluator` | 執行 baseline/candidate 比較並檢查決策門檻 |

## 隱私與安全

- `hufu improve` 的純 telemetry 分析應作為預設，因為其報告不含 prompt、輸出或工具參數。
- 將 LLM 根因分析（如 `--fix`）設為明確 opt-in；傳入模型前需遮罩 secrets、個資、憑證與不必要的工具輸出。
- 記錄資料來源、遮罩版本、保留期限與誰可讀取原始 workspace 資料。
- 改善 team 採最小權限：預設 `view`、`grep`、`glob`、`ls`；candidate 寫入僅限指定目錄；正式設定變更透過 Git PR。

## 推進路線圖

### Phase 1：可觀測性與 deterministic 改善（MVP）

- [x] 擴充 `hufu improve`：支援最近 N 次執行、依 agent/任務類型/模型/skill 分組，以及趨勢報告。
- [x] 定義 findings、hypothesis 與 experiment report 的 JSON / Markdown schema（見 `improvement-artifact-schemas.md`）。
- [x] 建立第一批 deterministic 規則與人工審核輸入：短 prompt、敏感工具未設 guard、工具錯誤偏高與 retry rate；候選變更仍須人工審核。

### Phase 2：候選變更與 benchmark

- 建立 versioned benchmark fixtures 與 baseline snapshot。
- 由改善 team 生成可審核 patch 與 candidate team。
- 提供 baseline/candidate A/B runner 與明確的 gate 判定。

### Phase 3：受控自動化

- 合格候選自動建立 PR，禁止自動合併。
- 於 production 持續監測已採納變更；安全或驗收退化時自動建立 rollback 建議。
- 累積「問題類型 → 有效改變 → 適用條件」的改善知識庫，提升後續建議品質。

## 初始驗收標準

- 能從指定 workspace 產生跨 run 的改善報告，且不洩漏 prompt、輸出或工具參數。
- 每個 finding 都能連回匿名化的 telemetry evidence 與 team revision。
- 每個 candidate 都有 hypothesis、patch、benchmark、baseline、判定門檻與 rollback revision。
- 未通過 acceptance / verification / 安全 gate 的 candidate 不可進入採納流程。
- 每次採納與拒絕都可在 workspace 與 Git 歷史中追溯。
