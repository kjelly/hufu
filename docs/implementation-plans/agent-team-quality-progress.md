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
| AT-02A structured results | 完成 | 9b4b1dc3；schemas／四角色接線、完整 gates 與 race 通過 |
| AT-04 context artifacts | 完成 | f604aeab；12 個精確大小邊界、長單行 fallback 與使用說明；focused/race/lint 通過 |
| AT-05 retrospective | 完成 | c5ba5d86；指南、draft 模板、固定合成快照；CLI／fixture／race 通過 |
| AT-01 semantic contracts | 完成 | 61b99600；既有契約對照與 10 個 typed fallback 組合；focused／race／lint 通過 |
| AT-06 research/review contracts | 完成 | 既有負面案例已覆蓋；提交測試對照與執行證據 |
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

## AT-02A：結構化結果

新增三份自含 Draft 2020-12 schema，runtime/integration 共用 analysis，implementation 與 verifier 各自使用其 schema。四角色 require-structured，維持 team.yaml、工具、side-effect、recovery、delegation 及 acceptance。

Verifier 新增 `VERDICT: UNVERIFIED`，對應 blocked／partial；修正原指示只允許 PASS／FAIL 與基礎設施未驗證情境的衝突。Check outcome 只表示 worker 觀察，不是 receipt；不新增 evidence_from、revision attestation 或 runtime policy。

新增 `hufu_dev_result_contract_test.go`，從實際 bundled team 複製到臨時目錄測試：四角色編譯／hash、缺 payload 拒絕後更正、success/failed/partial/blocked 均可合法提交、check outcome 與 exit code 條件、未知欄位／型別／長度／陣列上限，以及 worker PASS 自述無法繞過實際失敗的 acceptance。

有效工具集保留 loader 既有的 wait_for（AT-00 表列的是 frontmatter tools）；初次測試將有效工具誤比為原始 frontmatter，已依原本 golden 與 loader 修正比較，沒有擴張工具權限。Bundled compatibility golden 未變更。

| 檢查 | Exit／結果 |
|---|---|
| `go test ./internal/team -run '^TestHufuDev' -count=1` | 0；0.628s，包含所有巢狀子案例 |
| hufu-dev validate／lint --fail-on error | 0／0，無 findings |
| Bundled hufu-dev compatibility | 0，既有 golden 相同 |
| `go vet ./...` | 0 |
| `bin/check-docs`／`git diff --check` | 0／0 |
| `go test ./... -count=1` | 0；internal/team 286.610s |
| 最終 `golangci-lint run`（/tmp cache） | 0；0 issues，無 cache 寫入警告 |
| `go test -race ./internal/team` | 1；600.174s，預設 10m timeout；中止於仍執行中的 TestFreezeExecutionPolicyAtStartupPersistsPolicyForEachFreshRootBranch，無 DATA RACE 診斷 |
| `go test -race ./internal/team -count=1 -timeout=45m` | 0；1620.374s，完整同一測試集通過，無 DATA RACE 診斷 |
| `go build -o /dev/null ./cmd/hufu` | 0 |

回滾：還原本階段 frontmatter／schema／指示，在新的 session 執行；不更改既有 session 或歷史 payload。

## AT-04：Context artifacts 邊界

新增 `context_artifact_boundaries_test.go`，以完整 capture（含 exit footer）測試 production preview／min／cap 的等於及相鄰值，共 12 案例；成功發布後用 production view 分段重建完整 bytes，對照頭／中／尾、SHA256、byte size 與失敗 footer。另一個實際 bash 案例證明低於 preview 的 6000-byte 長單行仍會截斷、保留 exit 9、不發布 reference，寫入計數也只發生一次。

新增 hufu-dev README，說明臨時配置、適用來源、授權、配額、發布失敗、超限 fallback 與 attempt-local refs 的限制。未修改 bundled team.yaml；無預設啟用、核心行為或權限變更。原有 service／capture／policy 測試繼續覆蓋無 view、closed sequence、asserted result-contains、配額、保存／journal 失敗、越權／缺失／損壞與 snapshot drift。實際 structured handoff 與臨時啟用的組合於 AT-09 驗證。

| 檢查 | Exit／結果 |
|---|---|
| `go test ./internal/team -run '^Test(ContextArtifact\|ContextArtifacts\|BashOutputOffload\|BashSmallLongLineHonorsLegacyFallback)' -count=1` | 0；2.704s，含既有拒絕與 fallback 案例 |
| hufu-dev validate／lint --fail-on error | 0／0，無 findings |
| `bin/check-docs`／`git diff --check` | 0／0 |

回滾：撤回新增測試／說明即可；未啟用任何 persistent policy，不需資料遷移。

AT-04 的新增案例與真實 team 載入邊界另以 `go test -race ./internal/team -run '^Test(ContextArtifactProductionSizeBoundaries|BashSmallLongLineHonorsLegacyFallback|HufuDevResultContractsPreserveWorkerBoundaries)' -count=1 -timeout=10m` 通過（exit 0，3.557s）；`golangci-lint run` 通過（exit 0，0 issues）。新增 fake-worker 組合在隔離 overlay 的預先驗證也通過，最後仍須於 AT-09 對實際工作樹驗證。

## AT-05：離線回顧指南與固定示例

新增 `docs/guides/agent-team-retrospective.md`，保留 explicit workspace/team/branch 的既有 CLI 語法、unavailable／lineage／不可信資料的判讀方式、draft 候選模板與已填寫示例；不新增 helper、查詢 API、核准／套用流程，也未讀取真實歷史。

`TestRetrospectiveSyntheticSnapshotProvenance` 以固定事件 IDs／timestamp，在兩個新建 SQLite workspace 重建相同 report bytes，固定 source hash 為 `fee2f15e0b0c7ea9d31cfb20e7845fa129cfea8299b3fbb616bbcd87521b89a9`。Worker 自稱通過，但 run 保持 partial，receipt／revision／metrics／成本不可得；快照完整性不被當成測試證明。

既有 CLI 正反例 `go test ./cmd/hufu -run '^Test(ResultsReport|ResultsPickReport|RequestedReports)' -count=1` 通過（exit 0，0.733s），涵蓋指定 snapshot、缺失／失配、匯出及 branch lineage。新增 fixture 同時列入最終全量測試與 focused race；指南相對連結通過 check-docs。回滾只移除新指南／fixture，不影響既有歷史讀取。

AT-05 新增 fixture 已於實際工作樹的 focused race 執行成功；該批 team 案例 exit 0（74.099s），context 案例 exit 0（2.254s）。本輪 `go vet ./...`、`golangci-lint run`（0 issues）、build 及 check-docs 皆 exit 0；後續最終全量結果集中於 AT-09 記錄。

## AT-01：Semantic 離線契約

不修改任何 bundled semantic 配置。盤點發現既有測試分別驗證「typed error → reason」與泛用失敗 fallback，但沒有逐一把五種錯誤與實際 lexical 結果／trace 結合。因此新增 `internal/context/semantic_fallback_contract_test.go`，對 warming／busy／timeout／profile mismatch／projection incomplete × active／shadow 共 10 案例，要求結果與 lexical baseline 相同、content-free reason 正確且不洩漏 query、item ID／content 或原始錯誤。

| 契約 | 既有覆蓋與結果 |
|---|---|
| off／shadow 選取相同，auto 依實際 query 與 0.15 門檻路由 | TestContextRouterSemanticLaneByMode、TestContextRouterAutoRoutesByQueryScript、TestRoutesSemantic |
| 英文 lexical／private recall／授權先於 top-K | TestWorkerRecallUsesSemanticLaneForPrivateMemory、TestWorkerRecallAutoRoutesOncePerRecall、TestSemanticIndexAuthorizesBeforeDeterministicTopK、TestSemanticIndexAllowedIDsOnlyNarrowCanonicalAuthorization；包含其他 agent、candidate、expired 記錄 |
| timeout／busy／cancellation、逐 item freshness、profile mismatch | TestSemanticRuntimeTimeoutBusyAndCancellation、TestSemanticRuntimeSearchAndPerItemRefresh、TestSemanticIndexFreshnessIsPerItem；新增 typed fallback 組合 |
| 不下載模型、no-net／remote endpoint | TestSemanticRuntimeMissingModelFailsFast、TestSemanticRuntimeRefusesRemoteEndpointUnderNoNet、TestResolveValidation；只有本地 fake encoder |
| off／shadow 不 pin selecting hash；active／auto 才 pin | TestExecutionPolicySnapshotPinsOnlySelectingSemanticRetrieval、TestCompileAndManifestPropagationKeepsShadowNilAndActiveConsistent |

Focused 執行：`go test ./internal/semantic -count=1` exit 0（0.006s）；team 的 semantic／routing／wiring／recall／snapshot／replay focused suite exit 0（9.645s）；`go test ./internal/context -run '^Test(SemanticIndex|HybridRetrieve|SemanticFallbackReason)' -count=1` exit 0（1.472s）。新增 10 案例預驗證 exit 0（0.084s），最後再納入實際工作樹全量／race／lint。無真實召回率或 latency 結論；回滾只移除新增測試。

實際工作樹新增 semantic 案例已於 focused race 通過（context exit 0，2.254s）；共用最終 vet／lint／build 均 exit 0，0 lint issues。

## AT-06：研究／審查既有契約證據

本階段沒有發現需要新增的契約測試，不為產生 diff 重複現有案例；只提交覆蓋與執行結果。本輪未修改 web-research、hufu-coding、hufu-code-review 或其 action providers。

| 要求 | 既有測試證據 |
|---|---|
| gaps 需 team 明示允許，缺階段／partial／blocked／failed 不補成成功 | TestAcceptanceRequiredWorkersAcceptGapsOnlyWhenTeamOptsIn、TestAcceptanceRequiredWorkersRejectsEmptyAndMissingStages、TestFinishRejectsZeroTaskRunWithRequiredWorkers |
| 真實內容、錯引文、工具失敗、空內容與 search snippet 維持來源降級 | TestResultContractToolEvidence 的 success／wrong_quote／failed／empty／search 等案例；TestResultContractTextEvidence 的 truncation／metadata_only／wrong_tool／重複 call/result；無 browser 啟動 |
| 雙方 files_read、ownership、hash、缺 evidence_from 拒絕 | TestResultReferenceOwnershipAndReads、TestReplayedResultReferenceChecksWithoutRequiredDelivery、TestResultEvidenceInputBinding、TestResultEvidenceInputReplayRejectsSourceDrift、TestRequiredEvidenceRejectsMissingAndModifiedContext、TestRequiredEvidenceManifestIntegrityAndScope、TestEvidenceDispatchRejectsCorruptOrMissingPayload |
| 不用截斷 preview 代替來源，重播保留證據 | TestRequiredEvidenceNeverUsesTruncatedCoordinatorPreview、TestRequiredEvidenceDeliverySurvivesReplayAndBlocksLegacyFinish |
| immutable report 保留原 finding／gap，缺 attestation 不造成功 | reviewgate 的 TestFinalReportPreservesSourceFindingAndOriginalGap、TestCriticAndReportSchemasRequireCompleteOrderedSourceCoverage、TestReportRequiresCompleteRuntimeAttestations、TestHumanReportEmptySourcesRemainHonest、TestTeamResultSchemasRejectFalseCompletion |

Team 的 required-workers／tool evidence／references／evidence input/delivery／report focused suite exit 0（1.323s），最終全量檢查也覆蓋這些案例。Reviewgate 的上述報告／缺口／coverage focused suite exit 0（1.510s）；AT-00 已完整執行 reviewgate／reviewprep 兩 hidden packages，exit 0。沒有新的 hidden Go 變更，故未新增其 lint／vet gate。無 source 文字 heuristic、來源品質實機比較或人工 promotion 工作。
