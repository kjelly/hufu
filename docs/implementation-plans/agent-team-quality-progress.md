# Agent Team 品質改善：實作與驗證紀錄

> Status: active
> Authority: reference
> Verified-Commit: `2c188afe7c7078db43b84514cbaf305e4ffa9cf9`
> Supersedes: —
> Superseded-By: —

依 [實作計畫](agent-team-quality.md) 執行。僅使用本地合成資料與 fake models；不操作真實模型或歷史 workspace。每個階段完成後獨立提交。

| 階段 | 狀態 | 證據 |
|---|---|---|
| 計畫搬移 | 完成 | docs/tmp/spec.md 搬入正式實作計畫目錄，更新路徑與逐階段提交規則 |
| AT-00 baseline | 完成 | 本節全量與五團隊靜態檢查均通過 |
| AT-02A structured results | 待執行 | — |
| AT-04 context artifacts | 待執行 | — |
| AT-05 retrospective | 待執行 | — |
| AT-01 semantic contracts | 待執行 | — |
| AT-06 research/review contracts | 待執行 | — |
| AT-09 integration | 待執行 | — |

## AT-00：Baseline 與覆蓋盤點

測試基準為計畫提交 `c3ef8625`（程式碼與 `2c188afe` 相同），開工時 worktree 為乾淨。Go 1.27.2，golangci-lint 2.14.0。全量檢查以 `GOMAXPROCS=4` 執行，測試使用短路徑 `GOTMPDIR=/tmp TMPDIR=/tmp`，避免 Unix socket 路徑限制。

### hufu-dev before

| Worker | tools | side_effect／recovery | Result contract |
|---|---|---|---|
| runtime-engineer | view,grep,glob,ls,bash | none／retry | 無 |
| integration-engineer | view,grep,glob,ls,bash | none／retry | 無 |
| implementation-engineer | view,write,edit,multiedit,bash,grep,glob,ls,lua | workspace_write／manual | 無 |
| verifier | view,bash,grep,glob,ls | none／retry | 無 |

`goal-mode: outcome`、`memory-learning: observe`、allowed-workers 四人、implementation 的 require-task-contract、implementation/verifier 的 share-request-with、四人的 no-redispatch-after-success，以及 blocking acceptance 的 test/vet/lint 均需維持。Semantic/context-artifacts 尚未在此 bundled team 顯式啟用。

### 案例與既有覆蓋

| 項目 | 既有證據入口 | 本輪缺口／處置 |
|---|---|---|
| AT-02A schema 與提交 | TestValidateStructuredResultPayload、TestSubmitResultToolValidatesStructuredPayload、TestCanonicalExternalStructuredPayload | 新增真實 hufu-dev schemas、四角色接線及合法負面結果；不重寫通用驗證 |
| repair／持久化 | TestStructuredPayloadRejectionLoopUsesResultOnlyRepair、TestResultContractRefIsDurable、TestExecutionPolicySnapshotPinsResultContracts | AT-09 補真實 bundled schema 與完整 fake-worker 組合 |
| AT-04 大型輸出 | TestBashOutputOffloadEndToEnd、TestContextArtifactEligibility、TestContextArtifactFailuresFallBackWithoutReference、TestContextArtifactsChangeOnResumeFailsClosedOnPolicyDrift | 補 production limits 的精確邊界與小於 preview 的長行截斷，說明既有 fallback |
| AT-05 快照 | TestResultsReportUsesSelectedRunAndExportsSavedSnapshot、TestResultsReportMissingSnapshotFailsWithoutExport、TestResultsReportFollowsBranchLineage、TestRunReportCapturedWithoutExportAndRemainsRunBound | 交付可重建的合成示例、指南及候選模板 |
| AT-01 semantic | TestContextRouterSemanticLaneByMode、TestContextRouterAutoRoutesByQueryScript、TestWorkerRecallUsesSemanticLaneForPrivateMemory、TestSemanticRuntimeTimeoutBusyAndCancellation、TestExecutionPolicySnapshotPinsOnlySelectingSemanticRetrieval | 核對執行結果及剩餘錯誤情境，無缺口則不加同義測試 |
| AT-06 research/review | TestAcceptanceRequiredWorkersAcceptGapsOnlyWhenTeamOptsIn、TestAcceptanceRequiredWorkersRejectsEmptyAndMissingStages、TestResultContractToolEvidence、TestResultReferenceOwnershipAndReads、TestRequiredEvidenceDeliverySurvivesReplayAndBlocksLegacyFinish | 使用 typed transcript 檢查來源降級、scope、hash 與 gaps；保留既有 team 行為 |

### 檢查結果

| 指令／範圍 | Exit | 結果 |
|---|---|---|
| `go test ./... -count=1` | 0 | 全部通過；cmd/hufu 194.039s、internal/team 268.895s |
| `go vet ./...` | 0 | 無診斷 |
| `golangci-lint run` | 0 | 0 issues；唯讀 cache 造成 7 筆寫入警告，後續將 cache 指向 /tmp |
| `go build -o /tmp/hufu-quality-baseline/hufu ./cmd/hufu` | 0 | 由此 binary 執行靜態檢查 |
| 五團隊 validate／lint --fail-on error／explain | 全部 0 | 15 項通過；hufu-coding、hufu-code-review 各有既存 legacy provider warning，其餘無 warning |
| `go test ./.agent-teams/hufu-code-review/reviewgate/... ./.agent-teams/hufu-code-review/reviewprep/... -count=1` | 0 | 兩 package 通過（GOMAXPROCS=2） |
| `bin/check-docs` | 0 | 正式計畫與相對連結通過 |

Baseline 無程式測試／lint 失敗，可以開始 AT-02A。原始本地輸出暫存於 `/tmp/hufu-quality-baseline/`；此表保存命令、範圍、exit code 與結果，臨時 log 不是版本化權威。
