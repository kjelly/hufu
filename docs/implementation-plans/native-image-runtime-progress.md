# Native Image Runtime 實作進度

> Status: active
> Authority: reference
> Verified-Commit: `15ce1e17`（PR-01B 證據與變更同本次階段 commit）
> Supersedes: —
> Superseded-By: —

依 [實作規格](native-image-runtime.md) 分階段交付，每階段驗證後獨立 commit。未通過驗收的階段不標示完成。

## Baseline

- 原始碼基準：`b97ef843f440ab0cca85a8a453892ba2c4eef560`；工作樹起始無 tracked changes。
- Toolchain：mise Go 1.27.2、golangci-lint 2.14.0；GOMAXPROCS=2，Go commands 使用 `-p 2`。
- 2026-11-04 驗證前：GOCACHE 22,107,688 KiB、GOMODCACHE 16,897,080 KiB；沿用既有配置，未建立新快取。這是共享既存總量，非本工作新增。
- `/tmp` 可用 7.4 GiB、available RAM 12 GiB；cache/module volume 可用 61 GiB。
- 初次 sandbox `mise run test`：既有 cmd/hufu、evalharness 的 loopback fixtures 遭 `socket: operation not permitted`；已終止確切工作程序，不能視為程式失敗或測試通過。
- 清理本次 abandoned Go work directory（456 MiB）及 CLI fixture binary（128 MiB），未刪共享快取或其他 session 資料。
- Baseline 執行於文件 commit `515b95b9`，Go source 與 `b97ef843` 相同，尚未加入 PR-00 測試。

| Baseline gate | 結果 | 主要證據 |
|---|---|---|
| `mise run test` | PASS, exit 0 | cmd/hufu 187.350s、internal/team 809.511s、internal/tools 48.400s；其餘 package 通過（包含既有 cache 命中） |
| `mise run test-contracts` | PASS, exit 0 | team 122.228s、cmd/hufu 73.997s、reviewgate 13.290s、reviewprep 25.267s，其餘 contracts packages 通過 |
| `mise run vet` | PASS, exit 0 | repository 與指定 agent-team packages 無診斷 |
| `mise run lint` | PASS, exit 0 | `0 issues.` |

- 全部 heavyweight gates 依序執行，未呼叫真實模型。自動審核曾誤判 contracts 仍在執行而拒絕啟動 vet；確認 owned go/mise/test processes 為空後重試，未繞過序列化。
- Baseline gates 完成後 GOCACHE（排除 GOTMPDIR）22,669,032 KiB，較最初含 TMP 的總量淨增約 548 MiB，未超過本工作 2 GiB 預算；既有 cache 與 dependencies 保留。GOTMPDIR 約 11 MiB（包含既有 fixtures），task scratch 約 6 MiB。

## 階段狀態

| 階段 | 狀態 | 證據／commit |
|---|---|---|
| 文件移入正式目錄 | 已提交 | `515b95b9`；文件 map、規格與本紀錄 |
| PR-00 SDK/schema fixture 與回歸基線 | 完成 | `82b0884a`；SDK HTTP wire／tool media、pinned Codex schema；全部 required gates 與 targeted race 通過 |
| PR-01A 私有圖片 storage 與前置拒絕 | 完成 | `15ce1e17`；格式／CAS／資源限制與 universal provider guard；required gates、targeted race 與 Darwin package cross-build 通過 |
| PR-01B M0 canonical binding/policy/quota | 完成；同本次階段 commit | required gates、定向 race 與 Darwin media compile 通過；PR-01 完成，M0 仍待 PR-02 |
| PR-02 M0 history/compaction/recovery/audit | 尚未開始 | — |
| PR-03 M1 CLI/direct/single native images | 尚未開始 | — |
| PR-04 M2 coordinator/chat/DAG/routing | 尚未開始 | — |
| PR-05 M3 MCP/result binding | 尚未開始 | — |
| PR-06 M3 inspect/export/docs | 尚未開始 | — |
| PR-07 M4 Codex localImage | 尚未開始 | — |

## PR-00 entrypoint impact map

以下是 `b97ef843` 程式碼的靜態定位，非圖片功能已完成的聲明：

| Path | 現行呼叫鏈／canonical owner | 後續媒體改動邊界 |
|---|---|---|
| CLI / legacy | canonical `RunE` → `runTeam` → `executeAndReport` → `executeSegmentsWithRunners` | flags/options 與單 segment builder；不得在 router preflight 前漏掉圖片 |
| direct / fast | `dispatchSegmentContent` / explicit agent → `RunDirectAgent`；需要時 `synthesizeDirectAgentResult` | 明示 direct grant；synthesis 使用文字結果 view，保持 acceptance |
| coordinator | `Coordinator.Run` → coordinator rounds → `runAgentWithStatusAndHistory` | typed ref history、metadata-only delegation、逐回合載圖 gate |
| single task | `RunTask` → `beginPublicInvocationExecutionRun` / admission → `ExecuteTasks` | 公開 invocation 身分、canonical task binding、非新建 resume |
| DAG / local worker | `ExecuteTasks` / scheduler → canonical Todo → backend `RunAttempt` → `HufuLocalSubagentProvider.RunAttempt` → `runAgentWithStatusAndHistory` | worker assertion、scope、lease、cache/inflight、native message compiler |
| chat | `runChatTurn` → `Run` / `ContinueWithPrompt` → canonical checkpoint | plain/TUI 同一 turn sequencer 與 pending state |
| fallback / fanout / repair | route/fanout occurrence 與 `coordinator_task_run.go` 的 ordinary/result-only repair paths | 同一 frozen media binding；逐 candidate admission，不重播副作用 |
| provider | bound invocation context → gated agent → admitted model → `AdmitProviderRequest` / `CommitProviderInvocation` → transport | provider-bound profile、limits、egress、wire identity、settlement |
| MCP worker | direct/gateway descriptor/authorizer → `ExecuteAuthorizedTool` → 現行 text-only `executeMCPTool` | 不可讓 string wrapper 靜默丟圖；新增 ordered typed result |
| MCP action | `ExecuteRuntimeTool` → `RuntimeToolResult` → `mcpActionResult` → canonical ActionResult/receipt | 保留 structured/text/error，runtime publisher 持有圖片 refs |
| Codex | `CodexSubagentProvider.RunAttempt` → execution world/process → `codexRunTurn` | pinned union、projection、effective world、cancel/resume；text-only 相容 |
| durable / replay | `SaveConversationHistory`、compaction state、event reducer、policy/receipt、branch lineage、audit | ref codec 與 version/hash 一致；不能把 hydrated SDK bytes 寫回紀錄 |
| bypass inventory | sidecar、runtime actions、unattended、dry-run、direct synthesis、old session/result reuse | 未授權者不 hydrate；尚未支援組合明確拒絕 |

## PR-00 SDK/schema 證據

- `internal/agent/native_image_transport_test.go`：使用真正 loopback HTTP request 驗證 Stream 的 Messages/Prompt precedence、有序／重複 image parts、空輸入前置拒絕，以及 NewImageResponse 工具結果進入下一次 provider request；比對原始 fixture bytes 的 SHA-256，未呼叫真實 provider。
- `internal/team/codex_input_schema_test.go` 與 `testdata/codex/0.166.0/`：固定原始 UserInput definition digest 及其 transitive definitions，驗證 text producer 相容與 localImage 的必要欄位／detail／variant case。
- `mise exec -- go test -p 2 ./internal/agent ./internal/team -run '^(TestNativeImage|TestPinnedCodexUserInputSchema|TestCodexTextInputMatchesPinnedSchema)' -count=1`：PASS（agent 0.055s、team 0.035s）。

| PR-00 gate | 結果 |
|---|---|
| targeted SDK/schema tests | PASS，agent 0.055s、team 0.035s |
| `mise run test` | PASS，exit 0；cmd/hufu 229.249s、agent 0.364s、team 830.858s |
| `mise run vet` | PASS，exit 0 |
| `mise run lint` | PASS，exit 0，0 issues |
| `mise run test-contracts` | PASS，exit 0；team 118.987s、cmd/hufu 67.663s、reviewgate 12.742s、reviewprep 24.233s |
| 新增測試 targeted race | PASS，exit 0；agent 1.116s、team 1.129s |

上述驗證針對以 `5b160410` 為父的同一份 Go 變更；PR-00 已完成。

Race 命令：`mise exec -- go test -p 2 -race ./internal/agent ./internal/team -run '^(TestNativeImage|TestPinnedCodexUserInputSchema|TestCodexTextInputMatchesPinnedSchema)' -count=1`。此階段只新增測試／fixture，race 覆蓋新增 HTTP callbacks 與 schema 測試；未宣稱整個 repository 的全量 race 已重跑。

驗證後共享 GOCACHE（排除 GOTMPDIR）23,863,644 KiB，較工作起始總量淨增約 1.67 GiB（包含必要的 normal/race dependency outputs），低於 2 GiB；保留當前可重用輸出，不清共享 cache。一次只有一個 heavyweight gate。階段 scratch 約 7 MiB，摘要記錄後清理；無保留 CLI executable 或大型 schema dump。

接續 PR-01：純 media 型別／驗證、同一 CAS 的私有 namespace、canonical task binding／policy／lease、資源配額與自動結果重用隔離；PR-02～07 與 M0～M4 尚未完成，不縮減原規格範圍。
- 此階段只建立 SDK/protocol 回歸證據，Hufu CLI 的 native image ingress、media grants／codec 與 backend image admission 仍未實作；尚不能宣稱原生圖片 runtime 可用。

## PR-01A：私有圖片 storage 與前置拒絕

- Canonical storage owner 仍為 `FileArtifactStore`。新增 `media/{data,meta}`，目錄 0700／檔案 0600；ID 由 version、SHA-256 與 canonical MIME 決定。metadata 只含 immutable content identity／尺寸，沒有 first-writer task／run authority；舊 data/meta 目錄、artifact IDs 與 permissions 不改寫。
- 新增 `internal/media`：PNG／JPEG／static WebP magic detection、bounded header/config validation、完整 decode、APNG／animated WebP 拒絕、byte／pixel／side／bundle ceiling。JPEG 包含 APP1 metadata 的原 bytes 保留；不做 resize、EXIF 移除或格式轉換。
- Linux／Darwin 的 file ingress 以 directory handles 逐層拒絕 symlink，以 nonblocking file handle 排除 FIFO／device／directory，再對同一 handle 限量讀取。其他平台明確 unsupported；沒有宣稱已執行 Darwin runtime 測試。
- Process-wide decode concurrency 為 1；storage 的讀寫持有 raw-byte reservation，reader Close 才釋放。raw ceiling 64 MiB，另外按每個 frame 的 Base64 padding 預留三份 encoding/frame copies；碎片化 reservation 也有界，並不宣稱這些數值就是程序 RSS 上限。必須避免巢狀取得 reservation，後續 compiler 要沿用相同資源 ownership。
- `Put/Get/Resolve/Verify/Open/TrustedArtifactMetadata` 走同一 namespace routing；media 不接受 digest-only lookup，嚴格驗證 metadata、MIME、hash、尺寸與 canonical claims。read-only opener 不建目錄；檢查 aggregate artifact read budget。data 已落地但 metadata 尚未建立的情形由相同 trusted publication 補完，不因未提交 occurrence 刪除 blob。
- Universal provider admission 在文字 token counting／transport 前拒絕 FilePart／tool media；Generate、Stream、GenerateObject、StreamObject 共用相同 boundary。文字 artifact opener 明確拒絕 media IDs，避免將 raw image 當文字內容返回模型。
- 此子階段沒有新增 task/session/receipt schema 或擴張 task 授權。尚未交付 Todo binding、media events、frozen target policy、branch quota、cache/reuse 隔離、history codec、CLI ingress 或 native image transport；PR-01B／PR-02 的驗收不能由本子階段測試替代。migration 僅新增 private namespace，不搬移既有 artifacts 或修改歷史 receipts。
- 新增 tests 涵蓋同圖多 publisher、immutable collision、mode／namespace／symlink、FIFO／device、MIME 偽裝、截斷／動畫、EXIF byte identity、decode 取消、raw/encoding reservation、reader 釋放、aggregate read budget、metadata 中斷後重試與零 provider transport。所有圖片均為小型合成 fixture，沒有真實模型呼叫。

本次 Go 變更以 `82b0884a` 為父，驗證後與此紀錄一併提交；PR-01A 完成，PR-01／M0 尚未完成。`golang.org/x/image/webp` 固定 v0.47.0；Fantasy SDK 未升級，既有 x/sync v0.24.0 改為直接依賴。

| PR-01A gate | 結果 |
|---|---|
| `mise run lint` | PASS，exit 0，`0 issues.`；首輪兩項診斷已修正後重跑 |
| `mise run test` | PASS，exit 0；cmd/hufu 208.680s、team 846.082s、media 0.008s、tools 48.344s |
| `mise run test-contracts` | PASS，exit 0；team 121.027s、cmd/hufu 65.327s、reviewgate 23.428s、reviewprep 22.679s |
| `mise run vet` | PASS，exit 0 |
| 新圖片路徑 targeted race | PASS，exit 0；media 1.042s、team 1.453s |
| Darwin arm64 `internal/media` compile-only | PASS，exit 0；`-o /dev/null`，未執行 Darwin runtime／測試或真實模型 |

Race 命令：`mise exec -- go test -p 2 -race ./internal/media ./internal/team -run '^Test(Validate$|ReadFile|CheckBundle|DecodeLimiter|TransportReservation|MediaArtifact|MediaPublication|MediaReader|MediaStoreRespects|TaskArtifactOpener|ProviderRejectsMedia)' -count=1`。

Darwin compile 命令：`GOOS=darwin GOARCH=arm64 mise exec -- go build -p 2 -o /dev/null ./internal/media`。

早期 targeted regex 誤匹配既有 provider model-validation HTTP tests，sandbox 阻擋 loopback，該次不是通過證據；後續改用精確範圍，完整回歸在允許 loopback 的環境通過。lint 初次 sandbox 的既有 cache 為唯讀，後續沿用同一 cache 正常完成，未新建或搬移快取。

全部 heavyweight gates 序列執行。已確認無 Go consumer 後，根據本工作開始前的 cache inventory、新 media owner symbols 與目前 export 清單，刪除 7 個過期的 owned archives，回收 910,897,152 bytes（約 869 MiB）。驗證後共享 GOCACHE（排除 GOTMPDIR）23,283,948 KiB；本階段開始後新 cache entries 留存 901,072 KiB（包含目前 normal/race outputs 與必要 Darwin/decode dependency artifacts），低於 2 GiB。未清除其他 session、共享依賴、toolchain 或既有 fixtures。

Task scratch 最終約 4.5 MiB，峰值觀察約 139 MiB；CLI fixtures 已自行清理。階段提交後刪除 task-owned logs／inventory，不留二進位或大型 dump。GOTMPDIR 約 12 MiB，包含既有共享 fixtures，未依時間戳猜測 ownership 刪除。

## PR-01B：canonical media admission

- `internal/media` 定義有序 turn parts、input snapshot、source occurrence／selection 與 immutable task binding。snapshot 綁定 run／branch／invocation／turn；task binding 另綁定 runtime-reserved Todo ID、agent、purpose、policy 與 input digest。重複圖片保留順序，同內容的不同 task 有各自的 owner hash；不能憑 CAS ID 或 SHA 取得另一個 task 的 grant。
- operator 全域／專案與 team policy 取交集，預設停用；limits 只能收緊、targets 必須 exact match，token upper bound 取較大值。未知欄位、重複 targets、wildcard、負值、超過 ceiling 以及可能被 YAML 截斷的浮點值均拒絕。frozen policy 綁定 backend identity 與 model routes；舊 policy version 不能啟用圖片，未啟用時保留原文字 policy hash。
- `media_input_admitted` 是 branch quota 的 canonical source。quota 去重 immutable blobs，保留每個 turn 的 repeated parts 計數；全域 event head 的 compare-and-append fence 防止不同 EventStore handles 同時超額。fork 繼承祖先 cutoff 內的 admitted 使用量，新 run 不重置；提交失敗不發布 live snapshot，也不刪除已入庫內容，uncertain commit 可冪等重建 projection。
- task creation 與 `task_media_bound` 使用同一 atomic event batch。圖片 binding 進入 Todo、task projection、decision admission、worker assertion、session、journal 與 receipt；transition metadata 不能覆寫。attempt 必須有當前 task／revision／dispatch 的明示 lease，且仍在 `in_progress`；transition 後舊 lease 失效。執行前重新檢查已提交 policy 與 source lineage。
- 新增 host API `RunTaskWithMedia`，沿用公開 invocation admission 與 `ExecuteTasks`。此階段會入庫／綁定並明確保存 blocked occurrence；local worker、Codex、DAG、sidecar 與 runtime action 仍拒絕含圖執行，未新增 `--image`，也未送圖給 provider。這不是 M1 transport 已完成的聲明。
- 含圖 task 不查詢／寫入自動 cache、不從 journal／completed-task seeds 自動重用、不參與 duplicate／inflight 合併；文字 cache 的 positive control 仍命中。`--no-journal` 不影響 durable event admission；測試證明仍建立兩個獨立 blocked 圖片 occurrence，沒有 cache lookup／store。
- `ListByTask` 從當前 branch 的 canonical task binding 取得 media refs，不依 blob 第一位 publisher 的 TaskID。receipt 的 media digest 由 canonical Todo 派生，拒絕衝突值。SessionData 的 media snapshots 隨 target branch replay／checkout／refresh，resume preview 使用 defensive clone；沒有跨分支 carry。
- improver 將 `Media` 分類為 fixed policy，recursive schema fixture 新增 36 個欄位且不移除舊欄位。comparison digest 保留 target identity 與 token 上界，不能將授權變更混成 prompt variant。數值 `image_token_upper_bound`／`image-token-upper-bound` 加入既有 numeric telemetry redaction 例外，非數值仍遮蔽，避免合法 policy 被持久化成字串而無法 replay。
- Migration：只新增 optional JSON fields／events，沒有 SQL schema migration，不搬移既有 artifacts、不改寫歷史 receipt。正式 team manifest 參考文件已說明目前 API／config 支援與 M0 限制；history codec、完整 recovery 與獨立 audit 仍待 PR-02，M0 整體尚未完成。

本階段以 `15ce1e17` 為父；以下 gates 全部通過後與本紀錄一併提交。圖片均為離線合成 fixture，沒有真實模型呼叫或圖片外送。

| PR-01B gate | 結果 |
|---|---|
| 修正後定向測試 | PASS；media 0.005s、config 0.008s、team 9.942s、improver 0.031s |
| `mise run test` | PASS，exit 0；team 844.522s、cmd/hufu 198.999s、improver 147.864s、tools 48.555s |
| `mise run test-contracts` | PASS，exit 0；team 126.330s、cmd/hufu 55.924s、reviewgate 24.400s、reviewprep 25.267s |
| `mise run vet` | PASS，exit 0，無診斷 |
| `mise run lint` | PASS，exit 0，`0 issues.` |
| 定向 race | PASS，exit 0；media 1.042s、config 1.026s、utils 1.109s、team 22.612s、improver 1.094s |
| Darwin arm64 media compile-only | PASS，exit 0，`-o /dev/null`；未執行 Darwin runtime |

定向命令：`mise exec -- go test -p 2 ./internal/media ./internal/config ./internal/team ./internal/improver -run '^Test(Media|TaskMedia|OperatorMedia|SingleTaskMedia|SessionDataFields|Policy)' -count=1`。

Race 命令：`mise exec -- go test -p 2 -race ./internal/media ./internal/config ./internal/utils ./internal/team ./internal/improver -run '^Test(Media|TaskMedia|OperatorMedia|SingleTaskMedia|SessionDataFieldsAreClassifiedForBranchSwitch$|PolicyRetainsMediaAuthorization$|Paired|Redact)' -count=1`。

Darwin 命令：`GOOS=darwin GOARCH=arm64 mise exec -- go build -p 2 -o /dev/null ./internal/media`。

首輪 full regression 抓到 improver 的 policy field classification 與 SessionData branch classification 未更新，相關 CLI／improver／team 測試失敗，不能算通過證據。已補上 fixed／replay 分類、schema fixture、branch refresh 與實際 checkout 測試後重跑，完整測試第二輪通過。早期 lint 的三項 complexity 診斷已抽出媒體 owner helpers 修正，最終 source 的 lint 亦通過。更新 schema 的旗標只屬於 improver，誤套到多個 package 的定向命令不列為通過證據；schema 更新後已使用無該旗標的命令通過全部定向 packages。

所有 heavyweight gates 依序執行，沿用 mise、GOCACHE／GOTMPDIR／GOMODCACHE。GOCACHE（排除 GOTMPDIR，allocated bytes）由 23,842,762,752 增至 25,379,885,056；本階段新增且保留 1,537,122,304 bytes（約 1.43 GiB），包含目前 normal/race 與必要 Darwin dependency outputs，低於 2 GiB 預算。根據階段開始前 inventory、team compile-unit／新增 media method provenance 及 current normal/race exports，在無 Go consumer 後刪除 9 個過期 owned archives，回收 969,515,008 bytes（約 925 MiB）。未刪共享依賴、toolchain 或其他 session 的輸出。

Task scratch 清理前 6,060 KiB，觀察峰值約 133 MiB；fixture binaries 已自行清理。階段提交後移除 task-owned logs／inventory，不保留可丟棄二進位或大型 dump。GOTMPDIR 最終 11,532 KiB，保留其中既有 fixtures。驗證後 `/tmp` 可用約 7.3 GiB、cache/module volume 約 57 GiB、available RAM 約 12 GiB。
