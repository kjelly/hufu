# Runtime 字串契約：驗證版本與 derived fork 調查

> Status: historical
> Authority: reference
> Verified-Commit: `39756d921c5b847ccf8a4a0087da52287113c9c3`
> Supersedes: —
> Superseded-By: —

本紀錄補充 [runtime 字串契約文件](../../reference/runtime-string-contracts.md)
的驗證來源。歷史交付敘述、保存的審查證據與本次複驗分開記錄；
任何一次通過都只適用於該次固定版本、命令及環境。

## 歷史修正與證據界線

- 原始 runtime 變更為
  [`2100b1b2`](https://github.com/kjelly/hufu/commit/2100b1b2)。
- 四個 fixture 的修正為
  [`8ae68eda281654273981a23717a343a91174bac7`](https://github.com/kjelly/hufu/commit/8ae68eda281654273981a23717a343a91174bac7)。
  Git diff 可核對來源 receipt、snapshot coverage、同批 verifier 契約及
  `scope_context` 輸出的修正；commit 本身沒有附上當時 test／vet／lint 的原始輸出。
- 因此保留 2026-10-30「通過」的交付敘述作為歷史紀錄，
  不補造當時的執行收據，也不把後來的複驗當成當時已通過的證明。

| Fixture | 來源與修正範圍 |
| --- | --- |
| Gap identity | [Go function: TestHufuCodeReviewRuntimeOwnsAll51GapIdentities](../../../internal/team/hufu_code_review_fixed_collection_test.go)：inventory action 的 snapshot coverage 與來源 receipt。 |
| Empty critique | [Go function: TestHufuCodeReviewEmptyCritiqueOwnsSource](../../../internal/team/hufu_code_review_fixed_collection_test.go)：相同的來源綁定要求。 |
| Static scope | [Go function: TestHufuCodeReviewCharacterizesPromptCannotRewriteStaticScope](../../../internal/team/hufu_code_review_scope_characterization_test.go)：同批 `verify-targeted-go-tests` 契約。 |
| Synthesis input | [Go function: TestHufuCodeReviewSynthesisReceivesCompactPrepareStatusWithoutCoordinatorConstraints](../../../internal/team/runtime_output_refs_test.go)：`scope_context` runtime output。 |

## 2026-11-02 審查保存的歷史失敗

審查範圍為 `5736a4a2a2020e3e67ee0170b8f8fbb5c0a2e714`，
base 為 `0c66c8bcb206418e1fa48d14017c6138a216bc46`，僅一份文件的變更。
它不涵蓋後來的 `8223aebc`、`5eb057bb` 或 `39756d92`。

- Team：`hufu-code-review`
- Invocation：`inv-20261102T135117.813059952Z-681839b9b308`
- Run：`run-20261102T135247.932609182Z-9f8e89a92b7f`
- Branch：`session-1793627517011962326`
- Accepted report task：`7`
- Result：`res-7485f4b250ddd2a3282ebfdf804b44f33b15db0fdcfab3b4d6c4c6bd21973ee4`
- Canonical payload SHA-256：`912e322dceb89413363749c71add44f99a809f7dd2bca97c62bfc22d59dc7eba`

完整 payload 經公開的 `results show --payload` 介面匯出為
[historical-report.json](runtime-string-contracts-validation-2026-11-02/historical-report.json)，
保留 `test_verification` 的完整原始 diagnostic、scope、來源審查及 gap resolutions。
JSON 檔案為介面排版後的輸出；檔案 SHA-256
`879cf1b623b7790bb83dd269064e601adaa307458c33316b03f0074ec06b8245`
與上列 canonical payload hash 的計算範圍不同。
以 runtime 的 compact、排序及 HTML escaping 規則重新計算 payload hash，
結果與保存的 `912e322d…` 相同。
若原 workspace 仍保留結果，可設定其路徑後以唯讀命令再次查閱：

```bash
mise run run -- results show \
  res-7485f4b250ddd2a3282ebfdf804b44f33b15db0fdcfab3b4d6c4c6bd21973ee4 \
  --team hufu-code-review --workspace "$hufu_review_workspace" \
  --branch session-1793627517011962326 --payload
```

正式報告保存的 verifier 命令為 `go test -p 2 -mod=readonly -count=1 ./...`，
`completed=true`、`passed=false`、`exit_code=1`、`timed_out=false`，輸出未截斷。
失敗為：

```text
TestHistoricalResultRunBinaryAdmissionAndDelivery/derived_fork
historical CLI run: exit status 7, researcher=0 verifier=0
recovery required: event-store projection mismatch: execution policy snapshot missing from event store
```

這是一次有保存證據的歷史失敗，不是審查模型的缺陷推測。
三份來源審查共有零個 findings、三個保留缺口；其中兩個缺口都涉及
缺少歷史驗證收據，另一個涉及 fixture 的來源路徑。
審查流程本身完成並通過報告稽核，報告 coverage 仍為 `completed_with_gaps`。

## `39756d92` 固定快照複驗

複驗從 Git archive 建立隔離快照，未包含工作樹尚未提交的 recovery 修改。
使用 Linux amd64、mise 固定的 Go 1.27.2、`GOMAXPROCS=2`、`-p 2`，
保留既有 `GOCACHE`、`GOMODCACHE` 與 `GOTMPDIR`，以私有 task 目錄作為 `TMPDIR`。
本地 fixture 使用 loopback socket；沒有呼叫真實模型或重試原審查 run。

建立快照的可重現方式（從 repository root 執行）：

```bash
hufu_validation_root=$(mktemp -d /tmp/codex-task.XXXXXX)
trap 'rm -rf "$hufu_validation_root"' EXIT
mkdir "$hufu_validation_root/snapshot"
git archive 39756d921c5b847ccf8a4a0087da52287113c9c3 |
  tar -x -C "$hufu_validation_root/snapshot"
TMPDIR="$hufu_validation_root" mise exec -- go \
  -C "$hufu_validation_root/snapshot" test -p 2 -timeout 20m \
  ./cmd/hufu -run '^TestHistoricalResultRunBinaryAdmissionAndDelivery$' -count=1 -v
```

初次在限制 socket 的 sandbox 中執行，於第一個 `httptest.NewServer` 即因
`socket: operation not permitted` 中止，尚未進入待調查的 derived-fork 情境。
取得本地 fixture socket 執行權限後，原封不動重跑上述測試；
這是測試環境限制，與歷史的 policy projection 錯誤不同。

下表命令皆在該固定快照中執行；工具由 repository root 的 `mise exec --`
選定，Go 以 `-C` 指向快照。退出碼由執行程序取得，輸出存於相鄰檔案。

| 命令 | 退出碼與結果 | 執行輸出 |
| --- | --- | --- |
| `go test -p 2 -timeout 20m ./cmd/hufu -run '^TestHistoricalResultRunBinaryAdmissionAndDelivery$' -count=1 -v` | 0；四種情境全部通過，套件 46.234 秒。 | [derived-fork-head.txt](runtime-string-contracts-validation-2026-11-02/derived-fork-head.txt) |
| `go test -p 2 -timeout 20m ./...` | 0；67 個套件通過、1 個套件無測試。`cmd/hufu` 208.017 秒，`internal/team` 786.282 秒。 | [test-head.txt](runtime-string-contracts-validation-2026-11-02/test-head.txt) |
| `go vet -p 2 ./... ./.agent-teams/hufu-code-review/reviewprep/... ./.agent-teams/hufu-code-review/reviewgate/...` | 0；無診斷輸出。 | [vet-head.txt](runtime-string-contracts-validation-2026-11-02/vet-head.txt) |
| `golangci-lint run ./... ./.agent-teams/hufu-code-review/reviewprep/... ./.agent-teams/hufu-code-review/reviewgate/...`（mise 固定的 2.14.0） | 0；`0 issues`。 | [lint-head.txt](runtime-string-contracts-validation-2026-11-02/lint-head.txt) |

上述輸出與歷史 JSON 的檔案雜湊列於
[SHA256SUMS](runtime-string-contracts-validation-2026-11-02/SHA256SUMS)。
`vet-head.txt` 是包含命令、版本及退出碼的執行紀錄，其 stdout／stderr 為空；
其餘檔案保留公開介面或命令的完整輸出。

完整測試結束後，以 Git blob hash 核對快照內的 3,414 個 tracked files，
內容全部仍與 `39756d92` 相同；測試沒有改寫受驗來源。

`./...` 的全套測試只涵蓋 Go module 自動發現的套件，不包含隱藏的
`.agent-teams/` 目錄；上述四個 fixture 位於 `internal/team`，屬於此範圍。
此紀錄不宣稱已重跑所有隱藏 agent team 的測試。

## 調查判斷

[真實 binary 測試](../../../cmd/hufu/historical_result_process_test.go)
涵蓋 `single`、`fallback`、`extra_model` 與 `derived_fork`。
`derived_fork` 會從失敗的 parent 建立新任務分支，再以獨立 CLI process
執行新請求，檢查來源授權、parent 事件及報告不變、task ID 不重用，以及 child 完成狀態。

[投影校驗](../../../internal/team/projection_shadow.go) 的該錯誤表示
checkpoint 有 execution policy，而事件重播沒有對應 policy。
錯誤發生在 worker 派送前；歷史輸出中的兩個 worker 呼叫計數皆為零。
這只能定位拒絕邊界，不能單憑訊息認定事件寫入、fixture 或重播的哪一層有錯。

本次沒有修改測試或放寬投影校驗。固定版本的單獨測試與完整 module suite 均通過，
未重現歷史錯誤，因此沒有變更產品程式，也不宣稱已修復歷史根因。
fixture、當時 runtime 與測試間狀態的責任仍待可重現證據。
agent team 已正確保存失敗，沒有證據支持把這次測試失敗歸因於模型或團隊契約。
