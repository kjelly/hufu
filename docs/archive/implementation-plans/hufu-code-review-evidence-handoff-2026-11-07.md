# hufu-code-review「局部未觀測」改善：停止與續作交接

> 前半為 2026-11-07 停止時的歷史交接；後續實作與驗證狀態見文末「2026-11-08 續作紀錄」。

## 狀態與最新指示

- 使用者已明確要求「停止所有工作，將相關報告與未完成工作寫入檔案」。本檔為停止時交接，不是完成或驗收報告。
- 已中止 `RequiredArtifactReads`、`GapSupplementFlow`；`ReviewInputProducer` 及其接續工作已完成／停止。最後 `proc://` 確認無仍在執行的任務子工作；`omp.lsp.mux` 為工具服務，未當作任務程序終止。
- **這輪 evidence 改善尚未完成、尚未驗證、尚未 commit。不得將工作目錄直接視為可用版本。**
- 停止時 index 為空；42 個 tracked 檔修改、6 個 untracked 新檔。下方清單不含本交接檔。
- 未執行 live recovery、resume 寫入、session checkout，未修改歷史 receipts／checkpoints／accepted payloads。
- 使用者要求每個實作階段驗證後獨立 commit；不得把未驗證的 runtime 與 team 修改混成一筆提交。尚未 push。
- 使用者另要求本次完成後即停止，不追加改善。續作不得自行啟動額外付費 LLM／線上 team workflow。

## 已有提交與原始診斷

停止時最近提交：

| Commit | Subject |
|---|---|
| `d86bac88` | `fix(team): harden Codex locks and review evidence scope` |
| `2aea4d23` | `fix(runtime): preserve collector claims and recover transient streams` |
| `a1fec596` | `fix(runtime): reconcile policy checks after image rebase` |

這些是目前 HEAD 歷史，不是本輪尚未提交 evidence 改善的驗證證明。本輪未重新執行其 checks；不要為了交接重做已有工作。

先前透過公開 CLI 得到的具體診斷：

- 使用者原始入口是 `mise exec -- go run ./cmd/hufu`，不是 PATH 上的 `hufu`。
- Workspace：`/home/ubuntu/.local/state/hufu/projects/hufu1--82ce3727173e95e7/teams/hufu-code-review`
- Run：`run-20261107T014359.917371456Z-2f257cbdac70`
- Branch：`session-1794015800976119432`
- Canonical run summary：22 tasks，done 2、unresolved 20。8 個 collector blocked，`invalid_schema`，各已使用 2 turns；其餘 blocked 不能混算成 collector。
- 觀察到多個 collector 將 array 輸出為 `{}`，遭 schema 拒絕。正確拒絕不等於 runtime bug；不能放寬 schema 來換取成功。
- PATH 舊 binary 的 policy hash mismatch 與原始執行版本不同。保留的相容 binary 與當時 checkout 的 `go run` 唯讀 preview 均通過 policy／canonical replay／frozen inputs／accepted sources 檢查。
- 該 preview 仍為 `blocked`：`workflow_recovery: failed workflow recovery requires its sole blocked collection task`。存在多個 blocked collection tasks，不符合單一 task result repair 前提。
- 已觀察的 preview fingerprint：`3c85d253cfb4b8c5f471e7c8bf0b9dfc19ce896d6bdaa274f4b1e02e0d55aa70`；accepted tasks 2、remaining turns 0。這是舊觀察，不能宣稱是目前 session 狀態。

已執行過的唯讀指令（**不是現在應直接重跑或執行 recovery 的授權**）：

```bash
mise exec -- go run -p 2 ./cmd/hufu resume \
  --execution-profile default \
  --workspace /home/ubuntu/.local/state/hufu/projects/hufu1--82ce3727173e95e7/teams/hufu-code-review \
  --team hufu-code-review \
  --run run-20261107T014359.917371456Z-2f257cbdac70 \
  --branch session-1794015800976119432 \
  --repair-result 3 --dry-run --output json
```

### 本輪 gap 調查：已保存完整逐項分類

本輪改善的實際調查對象與上方較早失敗的 run 不同：

- Invocation：`inv-20261107T175800.820093394Z-bd4e36bd2b4a`
- Run：`run-20261107T175924.974731290Z-917e1a7e1182`
- Branch：`session-1794074316546312647`
- 被 review 的 revision：`a1fec596e09bc31eb0e45dc8cb6152f698b86001`
- 保留的公開 status 觀察時間：`2026-11-07T20:57:21Z`。這不是目前 live 狀態。

已從仍保留的分析狀態保存 73 筆逐 gap 分類，包含 task ID、gap index、subject、source artifact／range、transcript evidence、原因、補救建議、主類別與次類別：

[`hufu-code-review-evidence-gap-classification-2026-11-07.json`](hufu-code-review-evidence-gap-classification-2026-11-07.json)

| 主分類 | 筆數 |
|---|---:|
| 必要檔案未納入 `missing_file` | 53 |
| Producer 截斷 `producer_truncated` | 8 |
| 現有輸出不足以核實 `unverified` | 8 |
| 語意證據仍不足 `semantic_uncertainty` | 3 |
| 完整 artifact 只部分讀取 `partial_read` | 1 |

因此不能把全部「局部未觀測」歸因為 agent 沒讀完。主要缺口是 producer 沒提供必要檔案，其次是 producer 截斷。搜尋掃描不等於片段已回傳；未經核實的項目不能自動算成 unread 或完整讀取。JSON 保留次分類，總數為每 gap 一個主分類，不是 findings 或 task 數量。

需要保留的五類：artifact 完整但僅部分讀取；搜尋只回傳命中片段；producer 已截斷；必要檔案未納入；完整讀取但語意證據仍不足。實作將前兩類映射至 `unread` 的不同來源，另外三類為 `truncated`、`missing_dependency`、`semantic_uncertainty`；調查上的 `unverified` 則需保留為證據限制，不能虛構原因。

先前公開診斷亦已觀察本輪調查 run 為 `partial`／`workflow_complete`，其 acceptance 未通過與原 review 的 unit／integration test 證據失敗有關；不能把這種 partial 直接解讀成 evidence 改善程式的驗證失敗或成功。新改善尚未執行 workflow。

## 責任邊界與實作決定

- Team 決定必要 artifacts／範圍、primary source 分拆、相鄰內容／依賴、補證策略及 review 判斷。
- Runtime 只核對 content identity 與成功工具實際回傳的 byte ranges；搜尋掃描但未回傳的 bytes 不算讀取。
- 「資料已回傳給 LLM」不代表已理解，不代表 review 充分或程式正確。
- 現有只記 artifact identity 的 read receipts 不足以區分部分回傳、搜尋片段與完整回傳，因此正在增加通用 required-read 契約；不可加入 team-name 分支、review severity 或 primary-file 規則。
- Required ranges 為 half-open `[start,end)` source bytes。empty artifact 要求成功讀取身分證據，不能只因 ranges 為空就自動通過。
- 不以擴大 context window、改 prompt、降低 findings／severity、刪除 findings 作為唯一或替代修復。

## 已寫入但尚未驗證的修改

### A. 通用 runtime 與工具（階段一候選）

ArtifactReadSpans 子工作已交付：

- `internal/tools/view_artifact_reads.go`
- `internal/tools/view.go`
- `internal/tools/view_range.go`
- `internal/tools/view_artifact_search.go`
- `internal/tools/view_artifact_reads_test.go`
- `internal/golangruntime/runtime.go`
- `internal/golangruntime/runtime_test.go`

內容：實際回傳 source byte range metadata、source SHA256／byte size、response content binding、搜尋只記回傳片段、失敗／batch overflow 不授予 coverage、UTF-8 邊界；trusted-static interpreter export `tools.ArtifactByteRange`／`tools.ArtifactReadCoverage`。

該子工作回報新增 tests（**未執行**）：

- `TestViewArtifactReadCoverageTracksReturnedSourceBytes`
- `TestViewArtifactReadCoverageBindsResponseAndSource`
- `TestViewArtifactReadCoverageBatchOmitsFailedAndOverflowedItems`
- `TestViewArtifactReadCoverageUsesVerifiedReaderIdentity`
- `TestViewArtifactReadCoverageExcludesReadAndIdentityFailures`
- `TestExecuteTrustedProgramUsesSharedArtifactCoverageTypes`

RequiredArtifactReads 子工作被中途取消，**無最終交付、沒有新的 required-read 專用 test 檔出現在停止時清單**。下列修改只能視為進行中：

- `internal/team/required_artifact_reads.go`（新檔）
- `internal/team/artifact_access.go`
- `internal/team/coordinator_task_execution_envelope.go`
- `internal/team/coordinator_task_run.go`
- `internal/team/coordinator_tools_result.go`
- `internal/team/event_payloads.go`
- `internal/team/event_reducers.go`
- `internal/team/event_types.go`
- `internal/team/result_admission.go`
- `internal/team/result_collection_work.go`
- `internal/team/task_occurrence_projection.go`
- `internal/team/tool_policy_gate.go`
- `internal/team/workset.go`
- `internal/team/workset_fanout.go`
- `docs/reference/agent-format.md`

已看到的方向：Workset `required_reads`、durable artifact read observation event、submission coverage admission、collector 保留原 work occurrence／model execution identity、replay projection。**是否完整接線、是否編譯、是否保留原 receipts、是否支援修復與重播，均未證實。**

### B. Team producer（階段二候選的一部分）

ReviewInputProducer 子工作已交付，未跑 checks：

- 修改 `reviewprep/main.go`、`source_budget.go`、`main_test.go`、`source_budget_test.go`
- 新增 `reviewprep/source_evidence.go`、`source_evidence_test.go`

以上相對於 `.agent-teams/hufu-code-review/`。

其交付聲明（待驗證）：

- `required_reads: [{artifact_id,sha256,byte_size,ranges:[{start,end}]}]`，包括 diff、source bundle、inventory、per-item references。
- inventory 包含 path／role／status、原始 SHA256／bytes、required scopes、source bytes／lines 至 bundle bytes 的 available spans、truncated／omitted 資訊。
- 大 primary source 依 Go declaration／原始行邊界拆分，保留重疊上下文、caller／tests，總 cap 含 noop，超額 fail closed。
- 保存 frozen tree／manifest供補證；supplement 不重新 export 整棵 tree。

已寫入但未執行的 tests：

- `TestEmbeddedRuntimeEnforcesSourceSnapshotBudget`
- `TestLargePrimarySourcePartitionsPreserveCompleteCoverageAndDependencies`
- `TestProducerRequiredReadsIncludeReferencesAndNoop`
- `TestEvidenceInventoryUTF8TruncationEmptyAndUnavailable`
- `TestFrozenEvidencePersistsOriginalBytesAfterCheckoutChanges`
- `TestSupplementItemsDoNotRepublishFrozenTree`

獨立 native smoke 指令尚未完成。子工作提醒 supplement action 依賴正式 checkpoint／receipt，不能以任意 JSON 假充完整 workflow proof。

### C. Team gap／補證 workflow（中途停止）

GapSupplementFlow 子工作被取消，沒有最終驗證交付。停止時已寫入：

- `reviewgate/inventory.go`、`reviewgate/main.go`
- 新檔 `reviewgate/supplement.go`、`reviewgate/supplement_audit.go`、`reviewprep/supplement.go`
- `schemas/review-v1.json`、`documentation-review-v1.json`、`critique-v1.json`、`critique-v2.json`、`report-v1.json`、`report-v2.json`
- `coordinator.md`、`reviewer.md`、`documentation-reviewer.md`、`critic.md`、`synthesizer.md`
- `team.yaml`、`docs/contracts.md`

以上相對於 `.agent-teams/hufu-code-review/`。

已協議的 gap shape：

```json
{
  "description": "...",
  "missing_evidence": "...",
  "kind": "unread|truncated|missing_dependency|semantic_uncertainty",
  "requested_evidence": [{"path": "...", "ranges": [{"start": 0, "end": 123}]}],
  "limitations": [{"kind": "unavailable_path|out_of_scope|version_mismatch|budget_exhausted|invalid_request", "detail": "..."}]
}
```

上例 `|` 只是可選值說明，不是合法 enum 值。`requested_evidence`／`limitations` 必填、可為空；非 semantic gap 至少有 request 或 limitation。Request ranges 為 frozen source bytes，`[]` 代表 whole-file。Documentation／escalation 同 shape。

`report-v2` 新增 runtime-owned `supplement_outcomes`，由 `inventory-review-handoffs` action 同名 output 綁定；無補證也應為 `[]`。`report-v1` 沒有這個新欄位，不應直接修改兩者共用的 report fixture const。

父層已更新、但未執行驗證：

- `internal/team/hufu_code_review_fixed_collection_test.go`：舊 gap fixture 補 typed semantic uncertainty；native receipt fixture 的 inventory output 加 `supplement_outcomes: []`；model-facing runtime-owned 欄位檢查加此欄。
- `internal/team/hufu_code_review_scope_characterization_test.go`：舊 gap fixture 補 typed semantic uncertainty，未虛構缺少依賴或補證成功。

仍須確認 bounded workflow 真正接線、補證後重新分派、保留既有 findings／accepted results、無法補齊時保留 limitation，以及新 schema 與 model prompts／repair examples 一致。新增補證功能的專用 behavior tests 尚未交付。

## 未完成工作：續作順序

1. **先檢查中途取消的 runtime 與 team code 完整性，不直接提交。** 遵守 `harden-hufu-runtime`／`team-builder`／`hufu-diagnose-invocation` skills；commit 前讀 `git-commit` skill。
2. 補齊通用 required-read admission／event／persistence／replay／collector repair 接線與 team-independent regression tests；檢查各 worker execution path，不只一般 worker。
3. 必須涵蓋：empty artifacts、重疊區間 union、UTF-8 起終點、讀取失敗、artifact identity／content 變更、搜尋僅片段、partial read 觸發補讀、wrong task occurrence／attempt／model identity、persist/replay 與 result-only repair 不重播原工作。
4. 階段一只提交 generic runtime＋tools＋interpreter exports＋generic docs/tests。兩個 `internal/team/hufu_code_review_*` fixture 變更屬 team 階段，不能依目錄一概納入 runtime commit。
5. 原擬使用**單一** `git archive HEAD` 加 staged patch 的隔離 snapshot 驗證 runtime 階段，避免未完成 team schema 混入測試；尚未建立 snapshot／stage patch。若採此方案，不要另開多份 baseline、不要更換 caches 或 toolchain。
6. runtime 階段 formatter、相關 tests／smoke、適用的 contract gate、`mise run lint` 成功後才 commit。不能因 runtime 已編譯就視為 complete。
7. 完成 team 補證／補讀／有界 redispatch＋behavior tests；保留已完成有效結果與 findings，semantic uncertainty 可維持，不強迫得出結論。
8. 更新必要既有 docs；跑 team native offline smoke、相關 integration tests、`mise run test-contracts`、`mise run lint`，成功後另 commit team 階段。
9. 最後分開報告：來源修正、離線驗證、實際 workflow 結果、剩餘證據缺口。若未跑 LLM workflow，只能回報未執行；不能把 native smoke／tests 當成實際模型 workflow 成功。
10. 完成既定範圍後停止，不自行開新工作。Live recovery 仍需使用者另外授權。

建議檢查入口（**均尚未執行；不是通過紀錄**）：

```bash
mise exec -- go test -p 2 ./internal/tools ./internal/golangruntime
mise exec -- go test -p 2 ./internal/team -run 'RequiredArtifact|ArtifactRead|Workset|HufuCodeReview'
mise exec -- go test -p 2 ./.agent-teams/hufu-code-review/reviewprep ./.agent-teams/hufu-code-review/reviewgate
mise run test-contracts
mise run lint
```

以上 targeted regex 不保證涵蓋尚未新增的所有 tests，須依完成的修改補齊。Heavyweight checks 必須序列執行；無權繼續時不要執行。

## 驗證、實際 workflow 與資源

- **本輪未執行 formatter、build、tests、lint、contract gate 或新的 smoke；未觀察新的 LLM workflow 結果。** 子工作均被要求不跑 checks。
- 原 session 的唯讀 preflight 已執行，結果如前；不代表新 evidence code 通過驗證或原 workflow 已恢復。
- 所有 repository Go／lint 指令必須由 repository root 使用 mise；Go 1.27.2，`GOTOOLCHAIN=local`，不要替換 toolchain。
- 最後觀察 `GOCACHE=/home/ubuntu/.cache/go-build`、`GOMODCACHE=/home/ubuntu/gohome/pkg/mod`、`GOTMPDIR` 未設定；cache 約 146,520 KiB，磁碟可用約 79 GiB，`/tmp` 可用約 7.1 GiB。此後未跑 validation，不能推算未測的成長。未清除 shared caches。
- 已建立尚未使用的階段暫存根目錄：`/tmp/hufu-evidence-phase-utcfjxg3`。尚無 archive snapshot／smoke deliverable。
- 保留原診斷資料：`/tmp/hufu-inv-fix-EglZF6fb`（約 3.2 MiB）、`/tmp/hufu-inv-fix-8wgNpa7S`（約 134 MiB，含相容 binary／debug exports）。不要當作程式來源提交；不要上傳 debug bundles。這些是暫存路徑，重開機後可能消失。
- 先前公開 debug exports 包含 `hufu-debug-run-20261107T014359.917371456Z-2f257cbdac70.tar.gz`；應先讀 manifest 再使用其 evidence，不回查 live storage 補洞。
- 本輪 gap 診斷 scratch：`/tmp/hufu-gap-yl12Owqw`；內有公開匯出的 `public-gaps.json` 與 `hufu-debug-run-20261107T175924.974731290Z-917e1a7e1182.tar.gz`。逐項分類已另存永久 JSON，不必為了恢復分類重做整段調查。

## 最短接手提示

> 從本檔續作，不重做已交付 producer／view coverage 實作。先完成被取消的 generic required-read 與 team bounded supplement 接線及測試。工作目錄尚未驗證且未提交，index 原為空。按 runtime、team 分階段驗證並 commit；不得自動 live resume，不得啟動額外付費 LLM workflow，不得宣稱已讀等於已理解。使用者目前要求停止，須有後續續作指示才繼續。

## 2026-11-08 續作紀錄（實作與離線驗證完成）

- `b3f4de4b`：通用 artifact byte coverage、`required_reads` admission、durable observations、workset／collection replay 與 trusted-static 共用型別。已在單一隔離 snapshot 通過 targeted tests、`mise run lint`、`mise run test-contracts`，再獨立提交。
- `417f65a5`：實際 worker → collector 邊界補上原 worker metadata。新離線回歸在修正前重現 `artifact_read_coverage_identity_invalid`，修正後整組 required-read tests 通過，且 work／collection 各一次、可重播。
- Team producer 已加入來源 inventory、primary declarations／lines 分拆、frozen tree 與同版補證；supplement 至多一輪、八個項目、2 MiB，並限制原始與補充 reviews 合計 32 個。無可派發項目時保留 typed limitations 並略過空 manifest fan-out。
- 原始 accepted findings／gaps 保留；supplement 為增量 source，納入 critic／report inventory。`supplement_outcomes` 由 native receipt 綁定，不能由模型覆寫。`report-v1` fixture 與 `report-v2` 新 runtime-owned 欄位分開。
- 完整 native suites、最終 `mise run lint`（0 issues）、最終 `mise run test-contracts` 均已通過。
- 完整 tests 第一輪唯一失敗為 bundled-team compatibility golden 未同步；已確認 diff 僅新增一個 capability 與三個 contract（23 行），更新後 targeted normalize test 與完整 `mise run test` 重跑均通過（`internal/team` 1208.982s）。`mise run vet` 亦通過。較早因版本已被修正而停止的 snapshot full-test 不列為通過證明。
- 使用目前 checkout 編譯的單一暫存 CLI，`team validate`、`team explain`、`list hufu-code-review` 與指定 `review.scope` 的 LLM-free dry-run 均成功。`team lint` 預設 gate 成功；`--fail-on warning` 為退出 1，唯一警告是本次未修改的 `subagent-providers` 舊格式（`legacy_execution_provider_field`）。未新增 lint 忽略或順帶遷移 provider 設定。
- Dry-run 使用 private task workspace、明確 scope 及 localhost port 1 provider endpoint；輸出確認 no tasks were executed。此結果只驗證預覽，不代表補證模型 workflow 完成。
- 資源：保留既有 GOCACHE／GOTMPDIR／GOMODCACHE 路徑；GOCACHE 從約 150 MiB 增至約 1.9 GiB（含可重用依賴），峰值約 2.4 GiB 時依精確來源 provenance 清除過時輸出。逐檔記錄的 cache 回收共 1,561,608,192 bytes（約 1.45 GiB），另有較早未逐檔記錄的回收未計入。單一隔離 snapshot 約 58 MiB 與暫存 CLI 約 129 MiB 均已刪除；任務 scratch 餘約 4 MiB，提交後由本輪清除。不清除共享依賴 cache 或原 session 診斷／recovery checkpoints。
- 未執行付費／線上 LLM team workflow、live recovery、resume 寫入、session checkout 或 push；未修改歷史 receipts、checkpoints、accepted payloads。原 run 的證據缺口未重新觀察。
