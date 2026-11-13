# Agent runtime 功能補齊實作紀錄

> Status: active
> Authority: reference
> Verified-Commit: `5bf54fd7`
> Supersedes: —
> Superseded-By: —

規格：[Agent runtime 功能補齊](agent-runtime-completion.md)。

## 基線

- 原始程式版本：`5bf54fd7a9e3beac08ae4c5c165ca24ce98dc5ff`。
- 文件晉升前 tracked working tree 乾淨；`docs/tmp/spec.md` 為未追蹤的 ignored 文件。
- 文件晉升 commit：`ba8fa0a9`；`bin/check-docs` 通過（106 active files、689 relative links）。
- sandbox 內 baseline `mise exec -- env GOMAXPROCS=2 mise run test` 已確認至少 cmd/hufu、internal/agent、internal/evalharness 因本機 TCP socket `operation not permitted` 失敗；已終止該輪自有程序，不能將此結果解讀為程式回歸。
- 在允許本機 fixture socket 的執行環境重新執行完整 baseline：全套通過（exit 0）；cmd/hufu 201.793s、internal/improver 144.402s、internal/team 1208.543s、web-research/tests 0.544s。
- `mise run vet`、`mise run lint` 均通過（exit 0）；完整 baseline 已確認為綠燈。
- 全套 baseline 結束後 GOCACHE 約 9.7 GiB（相對初始 8.3 GiB 成長約 1.4 GiB）；task scratch 約 1.3 MiB，已完成 Go 工作目錄由工具清理。
- 重量驗證前容量：`/tmp` 8 GiB／可用約 7.6 GiB，available RAM 約 9.3 GiB；cache volume 可用約 69 GiB。既有 GOCACHE 約 8.3 GiB、GOTMPDIR 約 981 MiB、GOMODCACHE 約 17 GiB。這些是既有共享資料，不視為本任務獨佔產物。
- 本次只授權本地程式實作與離線 fixture；不執行真實模型實驗。

- 已清理中斷的 sandbox baseline 自有 Go 工作目錄約 484 MiB；以其中 testlog 的私有 TMPDIR 確認來源，清除前檢查無程序消費者。未清除共享 cache。

- 實測 mise task 會重新套用 `.mise.toml` 的 `GOMAXPROCS=4`，外層 `env GOMAXPROCS=2` 不會覆寫巢狀 task。Baseline 實際為 4、Go `-p 2`；當時 available RAM 超過 9 GiB，符合提高並行的容量條件。後續 focused 命令直接使用 `mise exec -- env GOMAXPROCS=2 go ...`。

## 階段狀態

| 階段 | 狀態 | 證據 |
| --- | --- | --- |
| 1 決策設定繼承 | 已完成，`e3c94b8b` | 下節 before/after、完整測試、vet、lint |
| 2 Step 執行歸因 | 已完成（本次提交） | 下節 before/after、CAS/replay、投影、完整測試與所有 gate |
| 3 中斷恢復回歸 | 待實作 | — |
| 4 跨 run economics 比較 | 待實作 | — |

## 階段 1：共用決策 transport

### 行為與完成界線

- Trigger：僅設定 legacy control transport，對 `runtime_browser_page` 與新 runtime choice 呼叫決策。
- Before：[Go function: TestRuntimeDecisionLegacySharedTransport](../../internal/team/runtime_semantic_decision_test.go) 兩案例均回報 `decision model unavailable`（exit 1）。
- 實作：每個設定檔／team 層先正規化 legacy transport，同層衝突 fail closed，再逐欄位合併。共用解析器涵蓋 startup、runtime、plan review、maintenance、preflight、snapshot 與 recovery；legacy control 保留 mode／threshold 政策。
- Sidecar timeout 使用獨立 canonical 欄位；inactive System One 設定不影響 sidecar。
- 相同政策可在 store close／reopen 後 resume；backend、model、endpoint、timeout 與 credential revision drift 被拒絕，歷史 snapshot 不改寫。
- 完成界線為離線設定分流修復及來源驗證；不宣稱原外部研究工作負載已全程驗收。

### 額外確認並修復的缺陷

- Credential transport 的來源 map 使用 credential-shaped JSON key，journal redaction 會改寫來源而造成 canonical payload persistence conflict。[Go function: TestTransportProvenanceSurvivesRedaction](../../internal/decisionrt/settings/config_test.go) 修正前失敗；新來源 key 為 `authentication`，歷史 map 原樣解碼，未放寬 secret redaction 或 hash 檢查。
- YAML 語法錯誤與重複 decision-runtime block 原先被載入路徑略過，可被後續合法設定掩蓋；對應 table cases 修正前失敗。現在推論解析器保留每層讀取／解析錯誤並 fail closed；只有 optional file 不存在可使用預設。

### 驗證證據

- decisionrt 全部子套件與 config 全測試通過；runtime focused tests 涵蓋 5 種 transport／credential drift、新 choice、store reopen resume 與既有控制點。
- 第一輪完整測試 exit 1：只有 improver schema 清單缺少新增 Sidecar／Timeout；team 1126.839s、cmd/hufu 261.267s 均通過。透過既有受控更新旗標補登兩個型別，保留固定 DecisionRuntimeHash 分類；新增 [Go function: TestPolicyDecisionTimeoutIsFixedDuringComparison](../../internal/improver/policy_test.go)，證明 timeout 改變會改變比較 digest。兩項 targeted tests 通過。
- 第二輪完整 `mise run test` 通過（exit 0）：team 1218.124s、cmd/hufu 211.635s、improver 152.149s；隨後 `mise run vet` 通過。
- 首輪 lint 回報 snapshot 建構函式 gocyclo 41（上限 40）。將共用解析／fingerprint 抽為 `decisionRuntimeFingerprint`，未新增 nolint 或變更上限；config 測試 helper 改用完整 `Coordinator.Close()` 關閉自有資源。
- 最後調整後 `mise run lint` 通過（0 issues）、`mise run vet` 通過、runtime focused tests 通過（team 7.375s）。
- 最後調整後完整 `mise run test` 通過（exit 0）：team 1133.522s、cmd/hufu 262.771s、improver 179.645s、inspect 6.715s；沒有略過測試。階段 1 的 source validation 完成。
- `bin/check-docs` 通過（106 active files、692 relative links、50 Go function anchors）。

### 資源與清理

- 曾捕獲 754 個現行正常／測試 export；當時 Hufu archives 約 508.8 MiB。以新增唯一符號、修訂邊界、依賴圖與無程序消費者檢查確認來源，清理過時 cache 約 944.8 MiB；另清理中斷 baseline 自有工作目錄約 484 MiB。
- 最後完整測試前 GOCACHE 約 11 GiB（共享總量，非全數本任務產物），GOMODCACHE 約 17 GiB；task scratch 約 4 MiB。保留 configured cache／GOTMPDIR、共享依賴與未知來源既有產物；驗證完成後再盤點清理。

- 最後完整測試後 cache volume 可用約 67 GiB、`/tmp` 可用約 7.7 GiB、available RAM 約 11 GiB。共享 GOCACHE 約 11 GiB、task scratch 約 5.2 MiB；已完成的 Go 工作目錄由工具自動清理。另有其他工作目錄的測試程序，未干預其程序或產物。尚需保留本任務的驗證紀錄與後續階段草稿，沒有新增大型交付物。

## 階段 2：step 執行歸因

- Trigger：structured step 已取得非空模型提案，但 declared-tool runner 只執行工具；現有 receipt 沒有區分提案與實際 invocation。
- 預期：工具呈現 `tool_call`，未觀測的自訂 runner 呈現 `unknown`；只有 runtime provider 邊界實際記錄、同 run／branch／task／attempt 且可證明屬於該 step 的呼叫可呈現 `model_generation` 與完整 invocation ID 陣列。提案不得變成 actual target 或重複成本。
- 影響邊界：structured engine／ordinary tool receipt、provider commit／settle、canonical receipt persistence／selected-lineage replay、task inspect／trace／report；沿用原 cost ledger，不新增 generative runner。
- 完成證據：新提案／實際執行 fixture、錯誤來源與跨 scope 拒絕、歷史 unknown 與原 bytes/hash、close/reopen／branch replay、成本總額不變，以及完整 test／contracts／vet／lint 與對應 race gate。現階段尚未宣稱完成。

- Before：[Go function: TestStructuredStepDistinguishesProposalFromExecution](../../internal/team/structured_step_attribution_test.go) 三個案例均在歸因斷言失敗（exit 1，team 0.133s）：工具實際呼叫一次、未觀測 runner 零次 generation、模型 runner 實際呼叫兩次；receipt 的 kind／proposal／IDs 皆缺少。最初工具 fixture 未宣告既有 artifact-path enforcement marker，已改用現有 `artifactAwareStructuredTestTool`，沒有放寬產品授權。

- 已實作核心歸因：sealed runtime scope 在 provider commit／settle 綁定已持久化的 profile invocation；工具／未觀測 runner／實際 generation 分別為 tool_call／unknown／model_generation。沒有新增模型 runner 或計價紀錄。
- 已實作新 receipt 的完整 CAS blob 與 append-only journal 引用、嚴格來源驗證及 selected-lineage hydration。Registry 依 run／branch／task／attempt／occurrence revision 區分歷史；重複的原生 call ID 不會取代其他 occurrence，未選 scope 的模糊查詢拒絕選取。
- Codec 將具名 artifact digest 編碼為 name／sha256 陣列，避免把 `api_key` 等輸出名稱當成 credential；human diagnostics 先遮罩，再計算完整 stdout／stderr digest。超過 64 MiB 明確拒絕儲存，不截斷診斷或放寬 redactor。
- 核心 focused tests 通過（team 0.385s）；加入完整 CAS round-trip 與新 store handle 重播後，相關 lifecycle／cost focused tests 通過（0.229s）。
- 來源替換、重複綁定、240 KiB 長診斷與 artifact digest 保留、固定金額 23 USD micros 的成本不變測試通過（0.659s）。加入 revision 範圍、重複 call ID、clone 隔離、unjoined／closed scope 與取消後，最新 focused gate 通過（0.320s）。
- 診斷投影、歷史 unknown、分支／損毀 evidence 與 target provenance 已補齊，驗證見下列紀錄。尚待對應 race 與完整 test／contracts／vet／lint；focused 結果不是階段完成證據，階段 2 尚未提交。
- 資源：以本任務新增符號及本 checkout 的絕對來源路徑確認可歸屬 cache 約 1535.2 MiB（38 entries；盤點時仍在 2 GiB 預算內）；共享 GOCACHE 約 12 GiB、GOTMPDIR 約 986 MiB、task scratch 約 5.2 MiB。沒有清理未知來源或其他工作的產物；下一輪重量驗證前需重新盤點並淘汰過時的自有 variants。

- 已接上 task inspect／trace 的文字與 JSON、安全摘要及 immutable report。Report schema 2 保存 native step 摘要；schema 1 以原格式驗證並讀取，不改寫舊 snapshot。新的摘要會與完整 CAS receipt 交叉核對；query／report 使用 canonical task occurrence，保留原始 source event／hash。
- 投影與既有 report focused tests 通過：team 1.290s、inspect 0.291s、CLI 0.112s；包含透過公開 preflight／structured execution 產生的持久化資料，沒有真實模型請求。
- Target before：`TestStructuredStepTargetUsesBoundProviderInsteadOfNamedDefault` 以實際 provider binding 證明 `gpt` 綁定 local，但舊 cost admission 重新套用 named default，歸因為 `remote/gpt`（exit 1，0.127s）。修正後在 provider commit 由 immutable provider identity 與 selector 語法綁定 target，cost／protocol 使用該目標；拒絕 profile input 覆寫。新 profile 與 ledger 不一致時 fail closed，無法證明的 legacy target 保持 unavailable，不改寫歷史金額。
- Target／分支／損毀 CAS／provider／protocol focused gate 通過：agent 0.500s、team 9.310s、inspect 0.280s、CLI 0.083s。包含 retained ancestor 的原 receipt bytes 相同、sibling 不可見，以及缺失／損毀 CAS 導致 recovery-required 與 admission 拒絕。
- 六個併行 provider invocation、runtime target 覆寫拒絕（含相同值）、projection／report 等 focused tests 通過：team 2.517s、inspect 0.221s、CLI 0.057s。
- Lint 首輪只有事件驗證器 gocyclo 42（上限 40）；將新增事件放到上層 current-event 分派，未放寬規則或新增 nolint。Durable claim 同時拒絕無來源的舊記憶體 receipt；所有有 store 的原生工具 receipt 都帶 runtime occurrence。包含 result-only collection 的最新 focused gate 通過（team 53.735s）。Lint 重跑已通過（0 issues）。
- 已檢查 /proc exe／fd／maps 無消費者，清理 14 個確定過時的自有測試 artifacts，共 1171.6 MiB；以 checkout 絕對來源、新增符號及現行 occurrence 測試識別修訂，保留現行測試與所有 normal dependency archives。加上先前 944.8 MiB，已清理自有過時 cache 共 2116.4 MiB；中斷 baseline 的 484 MiB 工作目錄另計。

- Focused race gate 通過：`mise exec -- env GOMAXPROCS=2 go test -race -p 2 ./internal/team -run '^Test(StructuredStep|DurableReceiptClaims|OrdinaryToolReceipt|ProviderProfileInput)'`（exit 0，team 3.937s）。`bin/check-docs` 通過；contracts gate 已開始，完整 test／vet 尚待執行。
- Race 後盤點：可由來源根目錄與新增符號確認的自有 cache 約 1866.7 MiB（62 entries，仍在 2 GiB 預算內）；GOTMPDIR 約 986 MiB。下一重量階段完成後重新盤點；不可將共享 cache 的總成長全算成本任務，亦不清除未知來源依賴。

- `mise run test-contracts` 全部通過（exit 0）：team 132.039s、CLI 56.490s、reviewgate 9.960s、reviewprep 39.723s，以及 inspect／operator／auditverify／web-research fixtures。已開始第二階段完整 `mise run test`；沒有真實服務請求。驗證前可歸屬 cache 1892.6 MiB、scratch 5.3 MiB、cache volume 可用約 65 GiB、available RAM 9.6 GiB。

- 首輪完整測試發現兩項確認回歸後停止剩餘自有測試（本輪不是通過證據）。第一項：provider commit 新增 runtime target，但 reference-only media transport 仍以舊投影比較，合法請求被拒絕；既有 `TestCoordinatorReferenceMetadataCountsAndCompilesWithoutImageAccess` 三種呼叫形態均重現（exit 1，team 0.748s）。將完整 profile 建構抽為共用 helper，寫入與 transport 授權使用相同投影，仍精確比對全部欄位並拒絕 model 輸入覆寫。媒體 metadata、target 與 override focused 測試通過（team 5.300s）。
- 第二項：action-catalog 原評測的全部任務完成，但原先第 1 筆 coordinator `action_completed` 消失，導致第 2 筆不是 catalog action。診斷透過既有公開 Run fixture 讀取實際 journal，確認 coordinator 沒有 worker occurrence lease，不能套用 worker step receipt 檢查；修正後 coordinator 工具沿用其既有 workflow action／observation evidence，worker receipt scope 仍嚴格驗證。臨時診斷測試已移除，保留原評測斷言。
- 修正後 focused gate：team 4.993s、圖片 CLI 19.582s 均通過；第一個 pattern 未選到 eval 子測試（明確回報 no tests to run），已改用獨立精確子測試命令補跑。
- 停止失敗完整測試後，經 testlog 的私有 TMPDIR 與 /proc 無消費者檢查，清理自有 abandoned Go 工作目錄約 121 MiB。未清除既有或未知來源工作目錄與共享依賴。

- Action-catalog 原評測補跑通過（exit 0，evalharness 0.714s），沒有改動 fixture 或斷言。
- 新一輪驗證前可歸屬 cache 2396.6 MiB；辨識本 checkout 與新增 structured 測試符號、缺少現行 profile helper 的過時 artifacts，檢查無 /proc 消費者後清理 21 entries、1090.0 MiB，降至約 1306.6 MiB。保留現行 normal 產物及共享依賴；舊 source 的 race artifact 已過時，會針對最新 source 重跑 race。

- 最新 source 的 `mise run lint` 通過（0 issues），擴大到 CoordinatorReferenceMetadata 的 focused race gate 通過（team 42.276s）；已重新開始 contracts gate。驗證前 GOTMPDIR 約 986 MiB、scratch 6.7 MiB、cache volume 可用約 65 GiB、available RAM 約 10 GiB。

- 最新 source 的 contracts 全數通過（exit 0）：team／inspect／operator／auditverify／CLI／reviewgate／reviewprep／web-research；CLI 44.489s、reviewgate 11.513s、reviewprep 27.000s。已重新開始完整測試。驗證前可歸屬 cache 1530.5 MiB（61 entries）、scratch 6.7 MiB、cache volume 可用 65 GiB、available RAM 約 10 GiB，符合預算。

- 第二輪完整測試：runtime/team 835.695s、evalharness 56.988s、improver 120.585s、inspect 4.617s 通過；只有 maintenance fixture 失敗，因此整輪 exit 1。失敗的第二份 project-root hash 精確等於 GOCACHE 根目錄；正常 sandbox 可見該處暫態 `.git`，sandbox 外不存在。Fixture 的 project 在 configured GOTMPDIR 下、未宣告自己的 Git marker，因此 subject-root discovery 會受祖先環境影響。
- 在私有 fixture 的祖先目錄、兩次 generator 載入之間確定性新增 `.git`，既有 configured 子測試重現同類 hash 漂移（exit 1，CLI 0.447s）。修正只為 fixture project 建立自有 `.git` 邊界，保留祖先變動注入與原 hash／identity 斷言；production discovery、scope、policy hash／drift 驗證均維持原邏輯。
- 第二輪完整 runtime 通過後沒有修改 internal source；後續完整 gate 可重用 Go 判定仍有效的 runtime 測試 cache，仍必須讓整套命令 exit 0。

- Maintenance fixture 全部案例通過（exit 0，CLI 9.878s），包含祖先 marker 變動注入與原 checkpoint identity 斷言。最終完整測試已開始；可歸屬 cache 約 1689.4 MiB、cache volume 可用 65 GiB、available RAM 約 10 GiB。Production source 未再變更，保留已通過的 race／contracts 證據。

- 最終完整 `mise run test` 全數通過（exit 0）：team 869.265s、CLI 180.741s、improver 132.164s、inspect 有效 cache；沒有略過測試。隨後 `mise run vet` 通過（exit 0）。最終 lint 進行中。
- 完整測試後可歸屬 cache 約 1765.2 MiB（65 entries，低於 2 GiB）；共享 GOCACHE 總量約 13 GiB、GOTMPDIR 約 987 MiB、scratch 約 9.3 MiB、cache volume 可用約 64 GiB、available RAM 約 11 GiB。完成的 Go 工作目錄由工具清理；沒有新增大型交付物。

- 最終 `mise run lint` 通過（0 issues），`bin/check-docs` 通過（106 active files、204 total files、693 relative links、51 Go function anchors）。Source snapshot 確認全部 Go 與規範檔自完整測試開始後沒有變更，只有進度紀錄新增驗證結果。第二階段 source validation 完成，可提交。
- 第二階段完成界線為離線歸因、完整持久化／replay、scope／來源拒絕及診斷投影；沒有執行真實模型實驗，亦不宣稱外部操作 exactly-once 或原外部工作負載已完成。第三、四階段尚待實作。
