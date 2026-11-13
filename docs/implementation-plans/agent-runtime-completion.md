# Agent runtime 功能補齊實作規格

> Status: active
> Authority: reference
> Verified-Commit: `5bf54fd7`
> Supersedes: —
> Superseded-By: —
> 實作狀態：可開始分項實作；不是已完成的驗證報告。
> 更新日期：2026-11-13
> 程式查核基準：`5bf54fd7a9e3beac08ae4c5c165ca24ce98dc5ff`（本地 HEAD）。
> 範圍：決策設定繼承、structured step 執行歸因、離線恢復回歸、跨 run economics 比較。
> 進度與驗證：[實作紀錄](agent-runtime-completion-progress.md)。

## 1. 目標與實作順序

先補齊可由 coding agent 在本地完成、以離線測試驗收的 runtime 功能。沿用既有 coordinator、event journal、receipt、recovery、inspect 與 cost ledger，不建立平行實作。

| 順序 | 工作 | 依賴 | 交付效益 |
| --- | --- | --- | --- |
| 1 | 統一 Decision Transport 設定繼承 | 無 | 新增 runtime choice 自然繼承共用設定，避免靜默選錯 backend |
| 2 | 區分 step 選模提案與實際執行 | 無；在第 1 項之後交付 | 診斷能辨認工具執行、模型生成及未知歷史資料，避免錯算選模效果 |
| 3 | Structured step 中斷與恢復回歸 | 第 2 項的 receipt 格式 | 證明恢復不重播未獲授權的副作用，保留已有結果與失敗證據 |
| 4 | 唯讀跨 run economics 比較 | 第 2、3 項完成後整合 | 用既有資料比較成本與驗收結果，供後續真實實驗使用 |

每項可獨立交付；不得以後續工作尚未完成為由放寬當項驗收。實作前若 HEAD 已改變，先重新核對本項涉及的程式與既有測試，保留已存在的能力。

本次不包含隔離工程；隔離未列為 P0，未選定內建 sandbox 或外部隔離工具。既有隔離保障仍須維持。真實模型實驗由使用者另行提供模型、任務及費用上限後規劃，不是本文件的工作項或完成門檻。

## 2. 共通工程約束

- 核心能力須與 team 無關；不得為 `hufu-coding` 或其他具名 team 新增 runtime 分支、專用啟動器或策略。
- 執行 target、route、授權及 recovery 由既有 admission 和 frozen policy 決定；本次不增加動態選模、品質失敗自動換模或自動校準。
- 保留 multi-candidate route 與 `escalate-on-retry` 的既有互斥限制。不得為了通過測試而增加重試、弱化 schema、改寫判斷或重播已完成副作用。
- Receipt、來源身分、原始失敗診斷與 canonical payload 維持完整且不可變；新增投影不取代持久化證據。舊資料缺欄位時明確回報未知，不補造歷史事件。
- 有行為修改時先依 [fix evidence workflow](../reference/fix-evidence.md) 定義 trigger、預期結果、影響邊界及 before/after 證據；遵守 `harden-hufu-runtime`。寫 Go 時使用 Modern Go Guidelines skill。
- 測試使用既有 coordinator／scripted worker／provider fixture；新場景優先擴充行為所屬測試。所有故障注入只操作測試程序、測試資料庫及本機 fixture。
- 測試資料、程序、socket 與暫存輸出須清理；依 AGENTS.md 使用私有 task TMPDIR，保留既有 GOTMPDIR 與 cache，socket 使用既有短路徑 helper。

## 3. 統一 Decision Transport 設定繼承

### 3.1 已確認的 trigger 與邊界

`internal/team/runtime_semantic_decision.go` 的 `runtimeControlTransport()` 只對 `runtimeControlPoint()` 對應的四種名稱套用 legacy transport。`runtime_browser_page` 等其他名稱只取得 `decision-runtime` 預設，因此只配置 `control-decisions` transport 時可能使用非預期 sidecar。

對應 [Issue #54](https://github.com/kjelly/hufu/issues/54)。Issue 中的 13 次記錄來自帶未提交修改的執行版本，不能宣稱是乾淨基準 commit 的實測。本項以離線 fixture 重現相同設定分流，不依賴重跑該外部工作負載。

主要修改面：

- `internal/decisionrt/settings/`：共用設定正規化、合併、來源及 fingerprint。
- `internal/decisionrt/control/`、`internal/decisionrt/catalog/`：使用同一 transport resolver，政策與 transport 分開。
- `internal/config/`、team 設定載入及 policy snapshot：設定來源、衝突檢查與 drift。
- `internal/team/runtime_semantic_decision.go`、decision sidecar 初始化及相關 inspect 投影。

### 3.2 固定設計

保留現有 `decision-runtime` 作為 canonical 共用 transport；不新增第三個 `decisions:` 區塊。

```yaml
decision-runtime:
  backend: systemone
  systemone:
    model: clef:cloud
    endpoint: http://decision-service:11434/v1/systemone
    timeout: 20s
control-decisions:
  mode: active
  points:
    guard-reviewer:
      min-confidence: 0.99
```

1. 在 global、team 各自的設定層先將 legacy `control-decisions` transport 正規化，再依 `global → team` 逐欄位覆寫。所有 runtime choice 和 control service 共用合併結果。
2. `control-decisions` 的 backend、model、endpoint、api-key-env、timeout 保留相容讀取，輸出遷移提示；同一設定層若與 canonical 欄位重複且值相同，接受並提示；值衝突則在 inference 前拒絕，指出欄位與設定來源，不輸出 credential 值。跨 global/team 的差異是合法覆寫。
3. 只有 mode／threshold 不視為顯式共用 transport。沒有任何 transport 宣告時，保留既有各入口預設行為，包括 active legacy control 的預設 backend；不得將 `mode: active` 擴張成所有 runtime choice 的啟用政策。
4. Legacy block 有 transport 欄位但省略 backend 時，依該 block 原有 backend 語義正規化；若 canonical 已明確指定另一 backend，視為同層衝突。顯式 `sidecar` 不得因存在 inactive systemone 欄位而改成 systemone。
5. 已有 catalog entry 顯式 transport 的優先序維持為 `合併後共用設定 → entry override`。本版不新增任意 runtime point 的 YAML override 機制。
6. 四種 legacy 名稱映射僅可服務既有 mode／confidence 政策，不再決定是否繼承共用 transport。新 runtime 名稱無須加入 transport 名稱表。
7. Canonical `decision-runtime.sidecar.timeout` 保存 sidecar 專屬 timeout；legacy sidecar timeout 正規化到此欄位，不能借用 inactive systemone timeout。Sidecar 的實際模型由現有 sidecar model precedence 決定，不把 systemone.model／endpoint／credential 套到 sidecar。保留兩個 backend 的 confidence、校準及安全失敗語義；不新增 fallback inference。
8. 有效 backend、model、timeout、endpoint identity 與逐欄位來源沿用現有 Transport／inspect 輸出；修正 legacy 正規化後的來源，禁止將繼承值全標成 entry。沿用既有 secret redaction。
9. Policy snapshot 必須釘選有效 transport 與既有 credential revision。若改變解析語義需要新 snapshot 版本，保留舊版解碼；歷史事件不重算、不覆寫。舊執行的模型／有效政策不同時，普通 resume 拒絕並說明差異及既有 recovery／新執行途徑，不自動升級或繞過 drift。

### 3.3 驗收

以 table-driven tests 覆蓋：無設定、僅 canonical、僅 legacy、同層相同／衝突、global/team 部分覆寫、顯式 sidecar、inactive backend 欄位、entry override、無效值、YAML 語法／重複欄位錯誤、設定檔無法讀取與 credential 不可用。可選設定檔不存在仍保留預設，其餘讀取／解析錯誤不能被後續層覆寫而靜默使用模型預設。

以 runtime 整合 fixture 驗證：

- 僅 legacy 共用 systemone transport 時，`runtime_browser_page` 及另一個測試用新 runtime 名稱都取得相同有效 transport，無新增名稱特例。
- Guard／path／matcher／ask_user 的 mode、threshold、安全失敗及呼叫上限維持契約；sidecar 不被套用 systemone 設定。
- 記錄的 transport、來源、policy hash 與實際 fixture 收到的 invocation 一致；錯誤與診斷無 credential 值。
- 相同新政策可 replay／resume；backend、model、endpoint、timeout 或 credential revision 漂移被拒絕。舊 snapshot 不被靜默改寫。
- 更新既有設定參考、範例與診斷說明，描述相容讀取及同層衝突規則。

完成界線：離線重現與修復設定分流，通過來源驗證；不宣稱原外部研究流程已全程驗收。

## 4. Structured step 執行歸因

### 4.1 已確認的現況

`coordinator_structured_execution.go` 會填入 `StructuredStepRequest.Model`，但預設 `coordinatorDeclaredToolRunner` 直接執行工具，不以該值啟動模型。選模提案不是實際 invocation 證據。Task-level `ExecutionReceipt` 已有 `ExecutionTarget`、`ModelExecutionID` 等資料，應重用。

本項不建立新的 generative runner，不變更模型選擇結果，也不擴張為全域 routing framework。

### 4.2 交付物

在現有 step receipt 與其 inspect 投影補上以下語義；實際 Go 型別沿用既有 execution／receipt 型別：

| 欄位 | 契約 |
| --- | --- |
| `execution_kind` | 固定值 `tool_call`、`model_generation`、`unknown`；缺欄位的歷史資料投影為 unknown |
| `proposed_model` | 原有 step selection 的提案；可空，不能當成實際模型或費用依據 |
| `model_execution_ids` | 由 runtime 綁定的既有 invocation ID 陣列；無法證明關聯則為空，不由模型／任意 runner 字串自證 |

- 預設 declared-tool runner 記為 `tool_call`。這只描述 step 的直接執行方式，不宣稱工具內部必然沒有 LLM 呼叫；工具內部已記帳的 generation 仍屬原 cost ledger。
- `model_generation` 必須有 runtime 已記錄、同 run／branch／task／attempt 且可證明屬於該 step 的 invocation。只有模型字串不能滿足條件；現有自訂 runner 未提供可驗證歸因時記 unknown。
- 實際 target 由綁定 invocation 投影取得，不再另存另一份可互相矛盾的 target。多個 invocation 保留全數關聯與各自 target，不任選一個代表全部。
- 沒有 step 關聯證據的 task-level invocation 仍保留在 task-level，不依時間相近或模型相同猜配。既有 cost 歸帳維持原規則，不因新增關聯而重複計價。
- 新欄位採相容讀取；不得修改歷史 receipt bytes 或 hash 來補欄位。更新受影響的 codec、clone、event／session 持久化、replay、JSON／文字診斷及既有報告消費端。

### 4.3 驗收

- Declared-tool step 即使 proposed_model 非空，也不產生虛構的 model invocation，投影呈現 tool_call。
- Fixture 的已記錄 generation 可綁定正確 step；多 invocation 保留所有 ID。跨 run／branch／attempt、未知 ID、重複或衝突綁定被拒絕。
- 舊 receipt 仍可解碼、驗證與 replay，缺欄位呈現 unknown，沒有補造 actual target。
- 保存、載入、分支 replay 後的關聯一致；既有 receipt 雜湊與 cost ledger 金額不因投影改變。
- 使用 fixture 的確定金額驗證新增歸因前後總費用一致；無 generation 的 tool step 不增加 invocation 計數。

## 5. Structured step 中斷與恢復回歸

### 5.1 交付範圍

本項是可直接實作的離線故障注入與回歸測試，驗收對象為既有 recovery 安全性。Process-local receipt registry 不是「必然重播外部操作」的證據，因此本版不預先新增 StepStarted／StepSettled store 或承諾逐 step 自動續跑。

主要檢查 `structured_execution.go`、`coordinator_step_receipts.go`、`coordinator_structured_execution.go`、`coordinator_session.go`、既有 recovery／protocol collection 及其測試。使用公開執行／resume 邊界與持久化 fixture；禁止只重新建立記憶體 map 冒充程序恢復。

### 5.2 故障矩陣

測試 child process 使用本機 fixture 表示外部系統，以獨立於 coordinator 記憶體的 durable 計數器記錄副作用。測試程序只在握手確認指定邊界後結束自有 child，再以新 coordinator／新 store handle 恢復。

| 中斷位置 | 預期結果 |
| --- | --- |
| 副作用執行前 | 依原 recovery 契約重試或阻擋；未獲 admission 不得呼叫工具 |
| A 的外部副作用已完成、coordinator 尚無終端證據 | 不得盲目重播；不可證明安全時進入既有 reconcile／blocked／needs_human 流程 |
| A 返回 receipt、B 尚在執行，程序中斷 | 依 durable evidence 及原 side-effect／recovery 契約處理，不能僅憑遺失的記憶體 receipt 判定可重跑 |
| 已有 durable result candidate／collection checkpoint，尚未 TaskDone | 沿用結果完成／result-only repair；不得重跑 producer／mutation |
| 已完成任務後重啟 | 復用既有完成結果，不再次呼叫副作用工具 |

同時覆蓋：取消、失敗 validator、repair 預算耗盡、policy drift、損毀／不匹配 evidence、另一分支的 receipt，以及 read-only 任務可安全重試。檢查實際副作用計數、狀態、失敗分類、保留的 payload／receipt，不只比對錯誤訊息。

### 5.3 驗收與缺陷處理

- 每個矩陣場景可重複離線執行；沒有時間猜測式 startup sleep 或真實服務依賴。
- 無足夠證據時拒絕重播，是有效的安全驗收；不得因此把狀態改成 success，也不得宣稱能自動續跑。
- 輸出精確的原有保障與限制：哪些會復用、哪些會安全重試、哪些必須 reconcile。
- 若測試證實存在違反上述契約的可達路徑，先記錄 failing test 與 cause，再於既有 recovery 邊界做最小修正並通過同一矩陣。不擴建通用外部 exactly-once 機制。
- 外部操作完成到本地 settlement 之間的窗口不能靠本地 receipt 單獨消除。本次 fixture 驗收的是不盲目重播；真實外部 adapter 的冪等鍵／核對契約不在本項範圍。

完成界線：離線恢復安全回歸完成；不是所有外部操作具備 exactly-once 或可無人工介入恢復。

## 6. 唯讀跨 run economics 比較

### 6.1 交付範圍與入口

擴充 `internal/inspect/` 與 `cmd/hufu/inspecteconomics.go` 所屬 CLI，新增：

```text
hufu inspect economics-compare --manifest <file> --format json
hufu inspect economics-compare --manifest <file> --format text
```

入口只讀已存在的工作區；不呼叫模型、不跑 verifier、不建立 session、不遷移資料庫或啟動 recovery。以既有 `InspectEconomics`、run outcome 與 receipt 投影為來源，不建立另一套 ledger。

Manifest 採嚴格 JSON、拒絕未知欄位；相對 workspace 路徑以 manifest 所在目錄解析。第一版上限 100 runs、10 groups、manifest 1 MiB，超限在讀 workspace 前拒絕。Schema version 固定為 1：

```json
{
  "schema_version": 1,
  "workload_id": "fixture-workload-v1",
  "runs": [
    {"group": "baseline", "workspace": "./a", "branch_id": "branch-a", "run_id": "run-a"},
    {"group": "candidate", "workspace": "./b", "branch_id": "branch-b", "run_id": "run-b"}
  ]
}
```

Manifest 是 workspace／branch／run 選取的唯一來源；若同時顯式傳入父命令的 `--workspace`、`--branch` 或 `--session`，回報 usage 錯誤，不默默忽略。

至少兩個 group，各至少一筆 run。Group／workload_id 是操作者宣告的比較標籤，不能證明 workload 等價；不以關鍵字解讀 group 的策略。使用完整 branch ID，拒絕重複的 canonical workspace／branch／run 身分（含跨 group 重複）。

### 6.2 比較語義

1. 每筆 run 都經現有 lineage／hash／讀取授權檢查。輸出保留 run、branch、source head、policy／input snapshot 可取得的識別；所有來源以同一個已綁定讀取視圖計算。來源變動不能讓同一筆比較混用不同 head，應固定視圖或拒絕並提示重試。
2. Manifest 的 workload_id 僅標記 `operator_declared`。輸出 observed policy／input 差異及 unavailable 欄位；不把不同 run 的完整 snapshot hash 不同直接當成 workload 不同，也不自行推斷任務語義等價。
3. 報表定位為「宣告分組的觀測比較」，不輸出自動勝出策略或因果節費結論。操作者日後以固定任務、基準版本及驗收條件組織真實實驗。
4. 每組回報 run 數、terminal／nonterminal 數、accepted run 數及 accepted rate。Accepted 必須是現有 run 投影可證明終端完成且 acceptance 通過；partial、blocked、failed、尚未完成或未知均不算 accepted，分別列出數量。分母為所有選定 runs，不能只保留成功樣本。
5. Task verified rate 使用既有 verified task 總數除以 task 總數，不平均各 run 百分比，也不等同整體 accepted rate。
6. 各 run 沿用既有 known cost coverage／unavailable reason。所有選定 run 的成本完整性檢查通過才提供 group total cost 與 `total cost / accepted runs`；包含失敗 run 的花費。任一 run 成本不完整，兩者為 null，另列 known subtotal、缺漏數與原因；accepted runs 為 0 時除值為 null。不可將 unknown、訂閱制或本地成本當成零。金額以既有 USD micros 整數累加並檢查溢位，除值沿用既有 economics 向上取整規則。
7. 第一版不提供 end-to-end p95，不把 attempt latency 改名為任務或 run wall-clock latency。保留各 run 既有 attempt p50／p95 及 sample count，不平均分位數，不輸出假的 group p95。
8. JSON 提供 schema version、scope、各 run 證據與 group aggregate；文字輸出呈現相同語義及 unavailable 原因，沿用 CLI exit code 與敏感資料遮罩慣例。格式／manifest 錯誤用 usage 錯誤；來源缺失、授權或 integrity 失敗沿用 inspect 錯誤規則，不默默略過該 run。

### 6.3 驗收

使用既有 economics／run fixture 建立至少兩組資料，不要求使用者提供真實工作區：

- 完整成本、混合成功與失敗時，分母與手算結果一致，失敗成本沒有被排除。
- 混合已知／未知成本、零 accepted、零 task、nonterminal、缺 acceptance 時，null、計數及 unavailable 原因符合契約。
- Run 重複、跨 group 重複、symlink workspace 別名、未知欄位、超限 manifest、branch 不匹配、損毀 hash chain、來源改變及取消均有明確結果。
- 驗證多次讀取前後 workspace 檔案／資料庫內容沒有被修改，沒有 provider／verifier 呼叫。
- JSON／text 一致；既有單 run economics CLI 與金額不變。更新 inspect command help、參考文件與受影響的命令清單／operator 投影。

完成界線：工具能對 fixture 與既有資料做可追溯的觀測比較；不宣稱任何真實模型已證明節費。

## 7. 驗證與交付

實作者先跑當項的 focused tests，再依專案要求循序執行以下 gate。遵守 cache／磁碟／記憶體預算，保留既有 cache 與 GOTMPDIR；focused Go 驗證從 `GOMAXPROCS=2`、Go `-p 2` 開始，不平行啟動重量驗證。完整 mise tasks 會套用專案的 `GOMAXPROCS=4`；只在記憶體 reserve 足夠時執行，不以巢狀 mise 指令假設外層環境可覆寫 task 設定。

```bash
mise exec -- env GOMAXPROCS=2 go test -p 2 ./internal/decisionrt/... ./internal/config ./internal/team ./internal/inspect ./cmd/hufu
mise run test
mise run vet
mise run lint
```

涉及 receipt／result contract、binding、repair 或 replay 的第 2、3 項另須執行 `mise run test-contracts`。修改共享 receipt 或並行狀態的項目補對應 package／測試的 focused race 檢查，以 `mise exec -- env GOMAXPROCS=2 go test -race -p 2` 執行並指定實際受影響的 package／test pattern。

交付包含：實際修改範圍、before/after 回歸證據、已執行命令與結果、剩餘限制、cache 成長與清理情況。非文件 PR 依 fix evidence workflow 提供結構化 evidence block，清楚標示 offline／source validation；不得將 fixture 成功寫成 full-target acceptance。

本規格本身僅修改文件，未執行上述程式驗證；這些命令是後續程式實作的驗收要求。
