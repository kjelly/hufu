# 唯讀 runtime observability 實作進度

對應 [實作計畫](readonly-runtime-observability.md)。本次只使用本機 fixture；不進行真實模型試跑，不宣稱模型品質或節費效果。

## 基準與文件提交

- 原始程式碼基準：`3715d61001b9122a3828e3202487f7446598f894`。
- 計畫移入正式文件目錄：`13aad2a2`。
- Baseline `go vet ./...`：exit 0。
- Baseline `golangci-lint run`：exit 0；有唯讀 cache 目錄警告。後續使用 `/tmp/hufu-observability-lint-cache`。
- Baseline sandbox `go test ./...`：exit 1；`httptest`／本機 fake server 因 socket 權限失敗。原始記錄：`/tmp/hufu-observability-baseline-test.log`。
- 允許本機 socket 的 baseline 重跑：exit 1；其餘套件通過，`internal/team` 有 6 個 Unix socket 測試因預設暫存路徑超過 socket 路徑限制而失敗。記錄於 `/tmp/hufu-observability-baseline-test-unrestricted.log`。
- 環境根因：本機 Go 1.27.2 的 `testing.(*common).makeTempDir` 使用 `GOTMPDIR`。原值是 `/home/ubuntu/.cache/go-build/tmp`；只設定 `TMPDIR=/tmp` 的完整重跑仍 exit 1，不能算通過。
- 使用 `GOTMPDIR=/tmp` 重跑上述 socket tests：exit 0。未修改產品程式碼或排除測試；後續完整測試使用此設定。

## WP-00：fixture 與既有契約盤點（完成）

新增 `internal/inspect/observability_fixture_test.go`：

- 使用 production event writer 建立 hash chain 與 canonical cost events。
- failed invocation 7 micros、成功 invocation 11 micros、coordinator overhead 3 micros：run usage 小計必須是 21，task 小計必須是 18。
- 混合 fixture 另有 unknown、local、subscription 與 100 micros open reservation；各類不能混為 usage 或零。
- 使用 CAS artifact、sealed manifest、receipt 與持久化 verifier 結果建立 completed fixture，透過唯讀 audit 驗證。
- 重用 `snapshotInspectWorkspace` 比較檔案內容、模式與 mtime，確認查詢不寫入；重播輸出必須相同。

以下既有測試繼續作為契約，不另複製 assertion：

| 契約 | 現有測試 |
|---|---|
| resume、重號 dispatch、重複事件 | `internal/inspect/workers_test.go` 的 `TestProjectWorkerAttemptsIdentityAcrossResumeAndDuplicates` |
| fallback 與 actual target | 同檔 `TestProjectWorkerAttemptsFallbackAndWorld`；`internal/inspect/run_test.go` 的 `TestInspectShowsFallbackAttemptTarget` |
| fallback 後一般 retry 回 primary | `internal/team/execution_fallback_test.go` 的 `TestNormalRetryOfARouteTaskReturnsToThePrimary` |
| extra-model receipt 身份 | `internal/team/execution_receipt_test.go` 的 `TestExecutionReceiptsKeepConcurrentModelExecutionsDistinct` |
| branch 繼承與 sibling 隔離 | `internal/inspect/source_test.go`、`run_test.go` |
| 壞 hash chain、矛盾／缺失 evidence | `internal/auditverify/integration_test.go`、`hardening_test.go` |
| 未記帳為 unavailable、不執行 verifier | `internal/inspect/required_contract_test.go` |
| 重複 settlement／reservation、unknown 類別 | `internal/cost/projection_test.go` |

後續 WP-01／02 仍須補新投影的正反向驗收：上述現有測試證明 canonical 元件行為，不代表 routes／economics 已完成。尤其 extra-model 分組、唯一 winner credit、零 verified 分母與成本完整性資格，須在新功能實作時驗證。

驗證：

- `go test ./internal/inspect -run TestObservability -count=1`：exit 0。
- `go vet ./...`：exit 0。
- `GOLANGCI_LINT_CACHE=/tmp/hufu-observability-lint-cache golangci-lint run`：exit 0，`0 issues`。
- `GOTMPDIR=/tmp go test ./...`：exit 0；記錄於 `/tmp/hufu-observability-wp00-test-gotmpdir.log`。測試需要允許本機 fake server 的 socket。

結論：原始產品程式未修改；baseline 的失敗可由本機 socket 權限與 `GOTMPDIR` 路徑長度解釋。在確認的環境設定下，含新增 fixture 的完整 suite、vet、lint 全部通過，可以開始 WP-01。沒有把 sandbox 失敗、被中止的重跑或無效的 `TMPDIR` 設定標成通過。

## WP-01：route／attempt 唯讀投影（完成）

新增 `hufu inspect routes <run-id>`，支援 exact workspace、session、branch 與 optional task，並提供 text／JSON。沿用 scope binding、validated lineage、task replay、worker lifecycle projector 與 offline audit。

- `internal/inspect/routes*.go`：task origin／creation event、receipt/model execution 身份、route digest、actual target、retry／fallback、完成證據、分組與資料缺口。
- 歷史資料在指定 run 的最後 event 截止；祖先事件只用來恢復身份及狀態，不受後續 run 改寫。重設 task 不建立新的統計 occurrence。
- Extra-model receipt 保留同一 dispatch key 及各自 execution key。歸因有歧義的 receipt 標為不計數，不重複增加既有 dispatch／完成 credit。
- 成功 credit 需唯一證據綁定、passing verifier、適用的 payload／contract／invariant checks；unknown 不升級為成功。artifact、binding、target 與 frozen route 的矛盾回 integrity error。
- 從 `internal/team/task_resolution.go` 抽出既有純 payload／receipt 驗證供 inspector 重用；原 resolution path 保留原有 required-contract 行為。
- 補齊 WP-00 證據 fixture 的 frozen topology：原 fixture 通過 audit，但嚴格 task replay 指出缺少 topology，不能用 audit 單獨證明 replay 可用。
- 同步 `docs/guides/inspect.md`、命令 metadata 產生的 reference 與 Nushell completion。抽出既有 overview renderer，以保持 lint 複雜度門檻；未增加 lint exclusion。

新增測試涵蓋：verified 正例、未驗證／缺證據、verifier failed／timed out／overturned、歷史 run、跨 run 身份、branch 繼承與隔離、artifact corruption、read-only、missing workspace、額外模型、dispatch 重號、歧義 receipt、retry／fallback、route candidate 矛盾，以及 CLI text／JSON 一致性。沿用既有 event-chain corruption／schema 檢查與 resolution regressions。

驗證（最後程式版本）：

- `GOTMPDIR=/tmp go test ./internal/inspect ./cmd/hufu ./internal/team -run 'Test(InspectRoutes|Route|Observability|ValidatePersistedResultPayloadReceipt|VerifiedResolution)' -count=1`：exit 0。
- `GOTMPDIR=/tmp go test ./...`：exit 0；`/tmp/hufu-observability-wp01-test-complete.log`。
- `GOTMPDIR=/tmp go vet ./...`：exit 0；`/tmp/hufu-observability-wp01-vet-complete.log`。
- `GOTMPDIR=/tmp GOLANGCI_LINT_CACHE=/tmp/hufu-observability-lint-cache golangci-lint run`：exit 0，`0 issues`；`/tmp/hufu-observability-wp01-lint-complete.log`。
- 完整 tests 使用允許本機 socket 的執行環境；沒有真實模型試跑，沒有新增 runtime 寫入、排程或選路政策。核心變更均為 team-neutral。

## 尚未完成

WP-02、WP-03、WP-04、WP-05 尚未實作。未執行的驗收不得視為通過。
