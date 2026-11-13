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
| 1 決策設定繼承 | 已完成 | 下節 before/after、完整測試、vet、lint |
| 2 Step 執行歸因 | 待實作 | — |
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
