# Native Image Runtime 實作進度

> Status: active
> Authority: reference
> Verified-Commit: `532a6c45`（PR-03F 證據與變更同本次階段 commit；M1 尚待 PR-03G repair wire）
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
| PR-01B M0 canonical binding/policy/quota | 完成 | `3143197f`；required gates、定向 race 與 Darwin media compile 通過；PR-01 完成，M0 仍待 PR-02 |
| PR-02 M0 history/compaction/recovery/audit | 完成 | `bb2301c2`；required gates、定向 race 通過；M0 資料基礎完成，圖片 transport 仍關閉 |
| PR-03A provider compiler／圖片能力基礎 | 完成 | `8ddd0f04`；required gates、SDK wire／生命週期定向 race 通過；未開啟 runtime 圖片執行 |
| PR-03B canonical provider admission／CAS compiler／token counter／audit | 完成 | `9484c5e7`；required gates、最終 source 定向 race 通過；runtime 與 CLI 送圖仍關閉 |
| PR-03C 最後 HTTP 授權／SDK stream 排空 | 完成；`a1ad9c8a` | 完整測試、contracts、vet、lint 與定向 race 通過；圖片執行入口仍關閉 |
| PR-03D 原生圖片 API／共用 worker 串接 | 完成；`cac9d373` | API wire、full、contracts、vet、lint 與定向 race 全部通過；M1 整體仍未完成 |
| PR-03E canonical 圖片恢復／重試 | 完成；`532a6c45` | 新程序 CAS wire、reset 授權、分支政策與所有 required gates 通過 |
| PR-03F CLI／文字 synthesis／自動 worker retry | 完成；同本次階段 commit | wire／cost／acceptance、完整 suite、contracts、vet、lint 與定向 race 通過 |
| PR-03G result-only repair／verbatim wire | 待驗證 | 補驗合法 repair 更新 lease 後的 transcript finalization；保持原圖不重播 |
| PR-03 M1 CLI/direct/single native images | 進行中 | PR-03F 完整 gates 與 PR-03G repair wire 通過後才宣告完成 |
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

Task scratch 清理前 6,060 KiB，觀察峰值約 133 MiB；fixture binaries 已自行清理。階段提交後移除當階段 logs／fixtures，保留 cache inventory 在同一私有 task 目錄供連續階段辨識輸出來源，不保留可丟棄二進位或大型 dump。GOTMPDIR 最終 11,532 KiB，保留其中既有 fixtures。驗證後 `/tmp` 可用約 7.3 GiB、cache/module volume 約 57 GiB、available RAM 約 12 GiB。

## PR-02：引用式 conversation 與獨立驗證

- Fantasy 的開放 `MessagePart` 介面承載 inert `media_ref`，保留原 role、part 順序、重複圖片、source snapshot／part 與 binding hash；decoder 不讀 CAS bytes。歷史 codec 在 JSON 編碼前拒絕 raw FilePart、tool media 與未知 part；在 SDK decode 前依 discriminator 拒絕 raw media，避免先配置 Base64 decode buffer。
- Canonical owner 仍是 conversation compaction state。含圖 state 使用 version 2，`native_input` 與 `context_reset` generation 明確沒有 model、summary、token usage 或宣稱已壓縮的 source ranges；文字 state version 1 的輸出格式與既有 digest 保留。相容 `session_history` 使用帶 checkpoint identity 的 envelope，不能替代遺失的 canonical state。
- 文字壓縮只替換一段完整、連續的非圖片訊息，保留引用相對順序、tool pairing 與原 source ranges；hard trim／squeeze 不得丟圖。內容無法縮至 fit 時保留引用並拒絕後續請求。
- 啟動只還原 refs 並核對 source／metadata，resume 前完整驗證 CAS。原始檔案移除不影響恢復；CAS 缺圖、內容竄改、未授權 source 或 branch cutoff 外的來源會拒絕。冪等輸入重試從 canonical checkpoint 還原 live history，不建立第二份 occurrence。
- `/reset` 先提交空 checkpoint 並完成 attestation，再清除相容 history；plain 與 TUI 都回報持久化錯誤。來源索引保持單調遞增，quota／admitted snapshots 不清除。中斷測試修正了既有 reconciliation 對「generation 收據已存在、checkpoint 收據尚缺」的誤判；仍拒絕實際位於其他分支的收據。
- 獨立 audit 使用已驗證 branch lineage、frozen policies、input／task binding events、canonical checkpoints 與 CAS，不信任 session cache。即使 run 尚無 terminal event，也驗證圖片完整性；宣告 media binding 卻缺 source events 的 task 不能跳過驗證。
- Provider observer 對 media request 只留 invocation／model 與 `media_body_omitted`，不呼叫 `GetBody`。debug export 在開檔前排除 private media data，即使遭竄改內容看起來像文字也不匯出。typed media bytes 不進入本階段的 history／events／checkpoint／debug 文件；任意文字中的編碼不屬於此保證。
- 中央 artifact path policy 封鎖整個 private media namespace，沿用 view／write 等 path-enforcing tools 的既有授權入口。receipt 測試涵蓋舊 data/meta 與新 media data/meta，在讀檔前回報 `path_out_of_scope` 且 `Executed=false`；native compiler 的 store access 與一般 path tool 保持不同 grant 邊界。
- M0 仍不 hydrate 或傳輸圖片。PR-03～07、真實模型驗收及圖片功能完整交付均未完成。

本階段以 `3143197f` 為父；以下是包含最後中央 media path 封鎖的完整 source 驗證。圖片均為離線合成 fixture，沒有真實模型呼叫、圖片外送、SDK 升級或 SQL schema migration。圖片 state version 2 不改寫舊文字 state／歷史 receipts，舊 binary 應拒絕未知版本而非猜測恢復。

| PR-02 gate | 結果 |
|---|---|
| 中央路徑／history／audit 定向測試 | PASS，exit 0；team 5.467s |
| `mise run test` | PASS，exit 0；team 888.353s、cmd/hufu 215.850s、auditverify 3.952s、tools 48.514s |
| `mise run test-contracts` | PASS，exit 0；team 180.745s、cmd/hufu 92.397s、reviewgate 15.882s、reviewprep 28.161s |
| `mise run vet` | PASS，exit 0，無診斷 |
| `mise run lint` | PASS，exit 0，`0 issues.` |
| 定向 race | PASS，exit 0；agent 1.144s、auditverify 1.400s、team 29.945s、cmd/hufu 2.700s |

定向命令：`mise exec -- go test -p 2 ./internal/team -run '^Test(MediaHistory|MediaAudit|ArtifactPathDenial|CompactionAttestation|ConversationCompaction)' -count=1`。

Race 命令：`mise exec -- go test -p 2 -race ./internal/agent ./internal/auditverify ./internal/team ./cmd/hufu -run '^Test(MediaHistory|MediaAudit|ArtifactPathDenial|AuditVerifiesMedia|PersistentMessages|Provider.*Media|ChatTeamHooks|DebugBundleOmits)' -count=1`。

完整 agent／auditverify 定向前檢查亦通過（0.372s／9.099s），chat reset／debug 邊界測試通過（cmd/hufu 0.361s）。最終 full suite 包含這些 cases；沒有以較早 source 的結果替代最後版本。36 個變更 Go 檔的 SHA-256 已在最後 full suite 期間核對不變，文件證據另於完成後更新。

早期 reset 測試抓到 source index 歸零造成 checkpoint regression，已修正為保持單調索引；不確定提交案例抓到 generation 收據已存在、checkpoint 收據尚缺的 reconciliation 誤判，已以重新開啟 EventStore 的恢復測試驗證。首輪 full regression 的 `TestChatTeamHooks` 使用沒有 session 的零值 Coordinator，新 checked reset 正確回報 session 不可用；已改用既有完整 fixture 並註冊 Close。第二輪 full suite 雖通過，但未包含最後中央 media path 封鎖，故在補上四種 path 的 policy-denied receipt cases 後，重新通過上表全部 gates。未通過／非最終 source 的結果不算完成證據。

Full tests、contracts、vet、lint 與 race 各命令依序執行。一輪 export 盤點與 lint 曾短暫重疊；盤點退出後才安排後續 gates，未重啟安靜的驗證工作。共享 cache 另有其他 checkout 的 consumer，清理守衛拒絕時保留輸出，待 consumer 退出後才依 task inventory、media owner symbols 及 current normal/race exports 刪除過期 archives。

GOCACHE 最後盤點（排除 GOTMPDIR，allocated bytes）為 30,716,428,288；相對 PR-01B 開始前 inventory 的新增 entries 為 6,874,546,176，包含其他 checkout 與必要依賴，不能當作本任務新增量。已識別的 media archives 912,551,936 bytes、task test-cache logs 253,132,800 bytes，另有前階段 baseline 380,006,400 bytes，合計約 1.44 GiB，低於 2 GiB；目前 normal/race exports 共 614,625,280 bytes。這些不是程序 RSS 的估算。

PR-02 期間已回收 2,807,439,360 bytes（約 2.6 GiB），連同 PR-01B 的累計回收為 3,776,954,368 bytes。PR-01A 剩餘 3 個 baseline archives 由 `15ce1e17` 首次新增的 media owner symbols 識別，因共享 consumer 保護暫留；確切路徑記於 `/tmp/codex-task.bY44PR/pr01a-retained-baseline.json`，後續階段待 consumer 退出再清理。未依時間戳刪除未知輸出，也未清除 toolchain、共享依賴或整個 cache。

Task scratch 最終盤點約 7.2 MiB，觀察峰值約 134 MiB；驗證二進位與 fixtures 已自行清理。提交後移除本階段 disposable logs，保留 cache inventories、cleanup manifest 與 PR-03 intake 在同一私有 task 目錄，供連續工作核對來源；不新增第二個暫存根目錄或保留大型 dump。GOTMPDIR 約 30 MiB 時仍有其他 checkout consumer，保留未知 work dirs；容量盤點 `/tmp` 可用約 7.3 GiB、cache/module volume 約 52 GiB、available RAM 約 10 GiB。

## PR-03A：provider compiler 與圖片能力基礎

此為 PR-03 的獨立前置子階段，以 `bb2301c2` 為父；不等同 M1 完成。Production team 尚未實作 compiler hook，M0 的圖片執行拒絕仍全部保留，沒有新增 `--image` 或呼叫真實模型。

- Provider introspection／model profile 新增獨立 `image_input` 證據，區分 absent、unknown、no 與 yes。一般 attachments／multimodal 不能證明圖片能力；runtime 的明示 no／unknown 覆蓋 metadata，同一 authority 的衝突拒絕樂觀推論。保留既有 attachments 欄位與 catalog 語意，僅在存在圖片證據時加入 telemetry capability。
- Generate、Stream、GenerateObject、StreamObject 共用 optional compiler，於 admission／provider slot 之後產生私有 wire projection。原引用保留給 commit、計費、重試與 callbacks；沒有 compiler 的引用不得靜默進入 SDK。
- Wire 驗證保留目標、call kind／參數、role、part 次序、文字與工具內容，逐一比對圖片 MIME、byte length 與 SHA-256；沒有 canonical refs 的請求不能被 compiler 改寫。原始 history 不被 hydrated parts 覆寫，含圖 compiler 必須提供 buffer cleanup。
- 取消、提交失敗與 stream 結束均沿同一 cleanup ownership。釋放前等待 pinned SDK encoder 返回、HTTP request body 的背景讀取結束，以及 response body 關閉；已關閉 body 不接受新 reader。含圖 scope 禁止 HTTP redirect，文字請求保留原 callback／預設 redirect 行為。
- 測試涵蓋真正 SDK → 本機 fake HTTP provider 的 Generate／Stream wire、重複圖片與 hash、history 未變、四種呼叫模式、竄改／缺漏、partial compile／commit failure、nil parts、取消期間 encoder／HTTP consumer 排空與 slot／buffer 恰一次釋放。圖片均為小型合成 fixtures，沒有真實 inference、外送、登入、SDK 升級或 SQL schema migration。

| PR-03A gate | 結果 |
|---|---|
| 最終 source 定向 race | PASS，exit 0；agent 1.372s、modelprofile 1.030s、providerintrospection 1.025s |
| `mise run test` | PASS，exit 0；team 1091.666s、cmd/hufu 239.956s、agent 0.474s、auditverify 19.739s、tools 48.882s |
| `mise run test-contracts` | PASS，exit 0；team 156.001s、cmd/hufu 89.046s、reviewgate 15.634s、reviewprep 26.760s、web-research 0.422s |
| `mise run vet` | PASS，exit 0，無診斷 |
| `mise run lint` | PASS，exit 0，`0 issues.` |

Race 命令：`mise exec -- go test -p 2 -race ./internal/agent ./internal/modelprofile ./internal/providerintrospection -run '^Test(ProviderCompilation|MediaDetection|NativeProviderRequests|ImageInput|PublicParsersPreserveImage|RuntimeImage)' -count=1`。

完整 gates 期間固定 20 個變更 Go 檔並核對 SHA-256，未以較早 source 的結果替代最後版本。最初 compiler 定向測試抓到 nil FilePart 在 reference predicate 被解參考的 panic，已改為 nil-safe、保守拒絕，並加入 nil text／file／reference／tool output cases；修正後的定向 race 與完整回歸均通過。

PR-03 的待辦仍包含：canonical task／當前 lease 與凍結 provider identity、三層能力、圖片 token 上界及計費、CAS bundle reservation／載圖、durable provider media admission／audit、CLI／direct／single 的 runtime wire E2E。上述 compiler fixture 不替代這些驗收；M2～M4 也尚未完成。

所有 heavyweight gates 依序執行，沒有同時啟動第二輪驗證。GOCACHE 在 PR-03 開始前為 18,145,693,696 allocated bytes；full tests 後 19,449,573,376、contracts 後 19,451,486,208、vet 後 19,451,854,848。這些是共享總量，期間其他 checkout 同樣增刪其輸出，不能把淨變化全歸本任務。

早期盤點只涵蓋大型 media archives，完整補算本任務來源的小型 project archives 後，識別留存量為 2,677,383,168 bytes，超過 2 GiB 預算；未再啟動新的 build／test gate。先從已驗證 source 的快取取得完整 normal／targeted-race dependency exports，再以絕對 source root 加 compile-unit 辨識 task-owned outputs，排除既存 inventory、目前 exports、open／mapped／hard-linked entries，才清理過期版本。其他 checkout 的 Go／team.test consumers 及其輸出保留，未依時間戳推測來源。

本階段刪除 322 個過期 owned archives，回收 2,189,729,792 bytes（約 2.04 GiB），含 PR-01A 暫留的三個 baseline；自 PR-01B 累計回收 5,966,684,160 bytes。清理後共享 GOCACHE 17,376,714,752 bytes；本任務 project archives 467,582,976 bytes、test-cache logs 20,070,400 bytes，合計 487,653,376 bytes（約 465 MiB），低於 2 GiB。configured caches、toolchain 與可重用 dependencies 未搬移或整批清除。

同一私有 task scratch 最終約 8.8 MiB，觀察峰值約 143 MiB；fixtures 自行清理，未留 disposable binary。GOTMPDIR 約 168 MiB，其中已確認大型 team.test binary 由其他 checkout 使用，未刪除。提交後移除本階段 logs，保留 cache inventories／cleanup manifest／source hashes 與後續設計筆記供連續階段辨識來源；不建立第二個暫存根目錄。最後 `/tmp` 可用約 7.3 GiB、cache/module volume 約 64 GiB、available RAM 約 12 GiB。

## PR-03B：canonical provider admission、CAS compiler 與 audit

此為 PR-03 的第二個前置子階段，以 `8ddd0f04` 為父，尚不代表 M1 完成。Universal provider admission、task／DAG／Codex／action／sidecar 的圖片拒絕仍保留；沒有新增 CLI `--image`，也沒有真實 inference 或圖片外送。

- Immutable provider admission snapshot 新增 configuration hash、圖片能力 state／source；backend 與 concrete adapter 分別宣告 transport 能力。metadata resolver 核對 current task／run／attempt／revision／dispatch lease、worker purpose、canonical target、frozen policy／identity、三層能力、有序 selections／refs 與保守上界；不讀 CAS 圖片 bytes。
- Team compiler 實作共用 hook。先預留整包 raw／encoding 額度，等待後重驗 lease，再從同一私有 CAS 驗證 bytes 並建立獨立 SDK projection。重複圖片保留每個 part，但可共用該次 request 的 raw buffer；成功與錯誤 cleanup 都只釋放一次。原始 history 保留 refs。
- `ReserveTransportFrames` 按每個 occurrence 計算 Base64 padding；private reserved reader 不巢狀取得額度。既有 reader 仍只扣一次 metadata read budget，新增 exact-budget positive control 防止 refactor 偷加讀取費用。
- Native counter 只在 private coordinator context 與 canonical metadata admission 下計算，完整文字／system／tool schema 加上每個圖片 occurrence 的 operator upper bound。SDK 的 bare model name 只能對應既有 bound target；未授權 raw media 在 JSON／Base64 serialization 前拒絕。處理缺少上界與整數溢位，不改動原始 messages。
- `media_provider_admitted` 保存 task lease、resolved target／configuration hash、binding／policy／input digest、有序圖片數與 token estimates。事件在 profile evidence 之後、cost reservation／transport 之前提交；cost manager 關閉仍必須保存。此事件表示 permission，不能推論 provider 已執行或有 actual usage。
- 獨立 media audit 從 preceding canonical lineage 核對 task state／revision、nonce 一致性、frozen backend／policy、profile capability、目標、digest、重複圖片估算與容量。缺少 profile、未知能力、目標／lease／binding／數量竄改或超額均拒絕；不信任 session cache。

定向測試包含 definite／uncertain media append failure，兩者都不能返回傳輸許可。最初 event 測試抓到 `image_tokens`／`text_tokens`／`request_tokens` 被 redactor 改成字串，已加入 numeric-only telemetry 例外，並驗證相同 key 下的非數值仍遮蔽。使用 canonical atomic journal fault seam 測持久化失敗，不靠未命中該 adapter 的 store hook 冒充證據。

| PR-03B gate | 結果 |
|---|---|
| Metadata／compiler／counter／CAS 定向測試 | PASS；最新 counter／compiler group team 10.066s；CAS group media 0.006s、team 6.808s |
| Media event／audit／redactor 定向測試 | PASS；utils 0.008s、team 16.389s |
| Helper 定向 race | PASS；media 1.021s、agent 1.098s、team 68.461s；此輪早於 event／audit／redactor 變更，不替代下方最終 source race |
| `mise run lint` | PASS，exit 0，`0 issues.`；28 個 Go 變更固定後啟動完整 gates |
| `mise run test` | PASS，exit 0；team 936.763s、cmd/hufu 226.080s、agent 0.485s、auditverify 15.693s、tools 48.435s |
| `mise run test-contracts` | PASS，exit 0；team 124.349s、cmd/hufu 73.516s、reviewgate 19.393s、reviewprep 27.100s、web-research 0.259s |
| `mise run vet` | PASS，exit 0，無診斷 |
| 最終 source 定向 race | PASS，exit 0；media 1.023s、agent 1.079s、utils 1.045s、team 90.579s；包含 event／audit／redactor 變更 |

最終 race 命令：`mise exec -- go test -p 2 -race ./internal/media ./internal/agent ./internal/utils ./internal/team -run '^Test(Transport|NativeProvider|NativeImageCounter|ImageAdmissionSnapshot|NativeImageAdapterCapability|NativeImageBackendCapability|MediaReservedReader|MediaStoreRespects|MediaProviderAdmission|MediaAudit|PairedAllocationLimitsRemainNumericOnly)' -count=1`。

完整 gates 期間固定 28 個 Go 檔，反覆核對 SHA-256 未變；lint、full regression、contracts、vet、最終 race 均使用同一份 source。文件摘要在 gates 完成後更新，不以早期 helper race 替代 event／audit 變更的最終證據。

M1 尚待：受支援 ingress／mode 的 canonical binding、RunTask／RunDirectAgent worker message／PrepareStep 串接、修復與 synthesis 的 metadata-only 路徑、最後 transport lease／identity 驗證、CLI／legacy／dry-run／binary context guard，以及完整 runtime wire E2E／retry／resume 驗收。M2～M4 未完成。

本階段全程沿用既有 mise toolchain／GOCACHE／GOTMPDIR／GOMODCACHE。Full gates 前共享 GOCACHE 17,400,979,456 allocated bytes；full tests 後 17,554,939,904、contracts 後 17,556,852,736、vet 後 17,557,405,696。這些包含其他 checkout 的活動，不以總量淨變化推定本任務使用量。

開發期間盤點本任務 project archives 到 2,065,514,496 bytes，接近 2 GiB，加上 test-cache logs 後須在下一個 heavyweight phase 前清理。保留當時已驗證 normal helper dependency exports；事件／redactor 修改後過期的 project race outputs 不假稱仍是目前 source。依先前相同的 source-root／compile-unit／current-exports／open-and-mapped-file guards 刪除 106 個過期 owned archives，回收 1,890,402,304 bytes（約 1.76 GiB），再啟動 lint 與完整 gates。其他 session 的 processes、temporary test binary、共用 dependencies 與 hard-linked／open entries 保留。

最終 full-source normal／race exports 完成後再清理，沒有額外 obsolete archive 可刪。盤點共享 GOCACHE 17,891,102,720 bytes；本任務 project archives 732,622,848 bytes、test-cache logs 23,683,072 bytes，合計 756,305,920 bytes（約 721 MiB），低於 2 GiB。從 PR-01B 至此累計回收 7,857,086,464 bytes；沒有整批清 cache 或改 cache 路徑。一次 cleanup 在 exports job 尚未退出時被 own-consumer guard 拒絕，待退出後才重試，未刪除 active outputs。

Task scratch 約 10.1 MiB，觀察峰值約 144 MiB；CLI fixtures 自行清理，沒有保留 disposable binary。GOTMPDIR 約 203 MiB，保留其他 checkout 與未知 ownership 的工作目錄。提交後刪除本階段 disposable logs，保留 inventories、cleanup manifest、source hashes 與 M1 continuation notes 在同一私有 task 目錄；不建立第二個暫存根。最後 `/tmp` 可用約 7.3 GiB、cache/module volume 約 64 GiB、available RAM 約 11 GiB。

## PR-03C：最後 HTTP 授權與 SDK stream 排空

此子階段以 `9484c5e7` 為父。圖片執行入口仍關閉；M1 的 CLI／direct／single-task 串接與完整 runtime wire 驗收尚未完成。

- 共用 optional `ProviderTransportAuthorizer` 在 durable invocation commit 與 SDK encoding 之後、每次 native HTTP send 之前重新授權。接收原始 canonical refs 與已提交的 invocation ID，不接收 hydrated bytes；文字請求維持原路徑。
- Team adapter 從 hash-chain-verified branch lineage 尋找相同 invocation 的圖片 permission，核對 task／run／branch／actor、revision／attempt／dispatch、binding／policy／目標／configuration hash 與 token estimates，再從 preceding lineage 驗證 profile／policy evidence。journal read 後重驗 live lease，不沿用等待前的權限。
- 權限撤銷、context 取消或 scope 已關閉時，HTTP body 在未讀取／未交給 underlying transport 前關閉。SDK fixture 覆蓋 Generate／Stream／GenerateObject／StreamObject：native positive control 通過，授權拒絕為零 fake-endpoint requests，文字相容路徑不新增 native gate。
- 真正 pinned SDK 的 iterator 提早 break、尚未迭代就取消、迭代中取消，都驗證 HTTP disconnect、buffer cleanup 與 provider slot 釋放，且不重送。這些證據只支持已驗證的 SDK 生命週期，不宣稱任意 lazy encoder 相容。
- 沿用既有 media admission payload，commit 與最後 transport check 共用同一 estimates builder；沒有 SQL／event schema migration、SDK 升級、文件搬移或真實 inference。

最初 structured-object positive fixture 回傳文字而非該 adapter 所需的 tool call，測試正確失敗；修正 fixture 後，以下 gates 使用固定的 8 個 Go 檔，SHA-256 未變。

| PR-03C gate | 結果 |
|---|---|
| Native transport／durable permission 定向測試 | PASS；agent 0.131s、team 13.233s |
| SDK stream early-stop 定向測試 | PASS；agent 0.142s |
| `mise run lint` | PASS，exit 0，`0 issues.` |
| 定向 race | PASS，exit 0；agent 1.473s、team 42.513s |
| `mise run test` | PASS，exit 0；team 863.834s、cmd/hufu 238.988s、agent 0.481s、auditverify 47.462s、tools 48.489s |
| `mise run test-contracts` | PASS，exit 0；team 116.453s、cmd/hufu 67.335s、reviewgate 13.278s、reviewprep 23.419s、web-research 0.284s |
| `mise run vet` | PASS，exit 0，無診斷 |

Race 命令：`mise exec -- go test -p 2 -race ./internal/agent ./internal/team -run '^Test(ProviderCompilation|NativeProviderRequests|MediaProviderTransport|MediaProviderAdmission)' -count=1`。

Lint、race、完整測試、contracts、vet 依序執行；全部結束後再次核對 8 個 Go 檔的 SHA-256。文件證據在驗證期間及完成後更新，沒有改動固定的 Go source，也沒有以較早版本的結果替代。

M1 尚待受支援 ingress／mode、RunTask／direct／worker／PrepareStep 串接、repair／synthesis 的文字 view、CLI／legacy／dry-run／binary guard，以及 runtime wire／retry／新 process resume 驗收。啟用前還需檢查已完成 iterator 保留 cleanup 閉包時的 buffer references，避免已清零的 raw arrays 長期留存；目前 production media guards 仍全部關閉。M2～M4 未完成。

本階段前共享 GOCACHE 18,098,425,856 allocated bytes；lint 後 18,512,576,512、race 後 18,728,443,904。已識別的本任務留存接近 2 GiB 前，先取得同一 source 的 normal／race exports，再清除 51 個過期 owned archives，回收 696,315,904 bytes，才開始完整 gates。完整測試後 GOCACHE 18,286,063,616、contracts 後 18,287,976,448、vet 後 18,288,308,224；共享總量包含其他工作的活動，不能全歸本任務。

盤點補含先前未完整計入的大型 task test-cache logs。Gates 結束後依確切清單、task 開始前 inventory、test-log header、絕對 source root 與 open／mapped／hard-link guards 清除 33 個舊 logs，回收 495,165,440 bytes；保留本階段結果及目前編譯輸出。兩次清理共回收 1,191,481,344 bytes（約 1.11 GiB），自 PR-01B 累計 9,048,567,808 bytes。最終 GOCACHE 17,793,142,784 bytes；本任務 archives 730,820,608、其他可辨識 task 輸出 126,222,336，合計 857,042,944 bytes（約 817 MiB），低於 2 GiB。

清理守衛曾保留仍有 consumer 的輸出；追查確認其中一個是使用者 nu／tmux 啟動的閒置 hufu，而非本任務。以確切 PID／start identity 辨識後仍保護其 open／mapped outputs，沒有停止該程序、刪除其 binary 或改動共享 dependencies。當時沒有 socket／runtime store handle 且 I/O 未變；完整 gates 沒有與本任務的 live agent-team run 重疊。

Task scratch 最終約 15.6 MiB，觀察峰值約 150 MiB；fixtures 自行清理，沒有留下 disposable binary。GOTMPDIR 約 12.5 MiB，保留既有／未知 ownership 的資料；沿用原 cache 路徑。階段提交後清理本階段 disposable logs，保留 inventories、cleanup manifest、source hashes 與 M1 continuation checkpoint 於同一私有 task 目錄。最後 `/tmp` 可用約 7.2 GiB、cache/module volume 約 64 GiB、available RAM 約 11 GiB。

## PR-03D：原生圖片 API 與共用 worker 執行

此子階段以 `a1ad9c8a` 為父，串接 `RunTaskWithMedia` 與 `RunDirectAgentWithMedia` 到既有 task／worker／provider pipeline。CLI `--image` 尚未交付，M1 整體仍未完成。

- Trusted ingress 設定 `single_task`／`direct_agent` invocation mode，參與 immutable binding hash；舊 binding 省略此欄位，v1 JSON／hash golden fixture 保持相容。每個輸入 turn 僅綁定一個 root LLM worker，保留完整圖片順序；拒絕子集、重排、複製 owner、降級 mode、DAG／fanout／多 candidate routing。Codex、sidecar 與 runtime action 不取得 native 圖片權限。
- `executeTask`／scheduler／Hufu-local 使用同一 canonical owner 與 live attempt lease。送出前重驗 model／backend／adapter 三層能力、frozen provider identity／grant、完整 system／tools／text／image token budget；metadata 拒絕先於 provider boundary，CAS byte read 與最後 HTTP 授權沿用 PR-03B/C。
- Worker 建立 task-owned refs 的完整 Messages，SDK Prompt 留空以免重複當前 user turn；不使用 Files 捷徑。內容切片獨立複製，不能在綁定時改寫呼叫端歷史。PrepareStep 與逐 step 預算包含每次圖片 occurrence 的保守 token 上限。
- Result-only repair 使用明確文字 view，不重讀／重送原圖，也不改寫 durable history。一般 attempt receipt 與 runtime collection 的 immutable work／checkpoint 從 canonical Todo 綁定 media digest，拒絕衝突值；collection replay 與獨立 media audit 重驗相同 digest。
- 新 invocation 沿原始 creation／binding 事件驗證 owner，不將舊圖片改綁到新 run 或要求來源檔重現。此階段只驗證跨 invocation metadata 與移除來源檔後的 canonical owner；真正新 process 的恢復 wire E2E 仍列為 M1 未完成項目。
- 取消發生於圖片已收件、worker 尚未開始時，保留標準 TaskError／FailureCancelled 與 `task_cancelled` 事件，不記重試指紋；一般媒體拒絕仍以既有失敗分類停止。取消回歸已確認修正前失敗、修正後通過。
- Pending task 新增 failure fingerprints 時不再 backfill 第二個 `task_created`；live／rehydrated 回歸驗證 creation 唯一且終態證據完整。已完成 provider compilation 在 consumers 排空後清除 cleanup／authorizer callbacks 及 buffer map；弱引用回歸確認保留 scope 不再保留其 private buffer owner。

`media_native_execution_test.go` 以真正 provider proxy／pinned SDK 連本機 fake HTTP endpoint，涵蓋 single-task／direct × tool／runtime result transport。兩張不同 PNG 加上第一張重複，核對原生 image parts 的 role／順序／解碼 SHA、user text 唯一、provider admission 次數、receipt 與獨立 audit；runtime collector 不收到圖片 bytes／refs。能力 unknown／no、context 不足與 CAS 替換均驗證零 HTTP；metadata 拒絕亦不啟動 boundary。未設定 whole-run acceptance 的執行仍是 unverified，沒有把 worker 完成當作 run acceptance。

| PR-03D gate | 結果 |
|---|---|
| 最終 native API／worker／取消／checkpoint 定向測試 | PASS，team 13.608s |
| `mise run lint` | PASS，exit 0，`0 issues.` |
| `mise run test` | PASS，exit 0；team 1094.955s、cmd/hufu 246.707s、tools 49.831s |
| `mise run test-contracts` | PASS，exit 0；team 170.836s、cmd/hufu 88.969s、reviewgate 26.691s、reviewprep 28.636s、web-research 0.448s |
| `mise run vet` | PASS，exit 0，無診斷 |
| 定向 race | PASS，exit 0；agent 1.665s、media 1.044s、team 270.417s |

完整 gates 凍結 34 個 Go／JSON source／fixture 檔，持續核對 SHA-256。較早的完整測試因確認取消投影錯誤而中斷，沒有算作通過；修正後重新執行 lint、定向測試與完整測試，以上只列最終 source 的證據。所有模型請求僅進 fake provider，沒有真實 inference、圖片外送或文件搬移。

M1 尚待 CLI／legacy／profile／dry-run／binary context guard、明確非視覺 direct synthesis、禁止含圖工作自動 replan 降級，以及 retry／真正新 process resume 的完整 wire／cost／acceptance 驗收。M2～M4 未完成。

Race 命令：`mise exec -- go test -p 2 -race -timeout 20m ./internal/agent ./internal/media ./internal/team -run '^(TestNative|TestMedia|TestDirectMedia|TestSingleTaskMedia|TestPendingMetadata|TestProviderCompilation|TestClassifyTaskFailure)' -count=1`。全部 required gates 與最終 source 的定向 race 依序完成，沒有並行啟動本工作的 heavyweight commands。

本階段沿用原 mise toolchain／GOCACHE／GOTMPDIR／GOMODCACHE，未清除共享依賴。完整測試後共享 GOCACHE 20,059,353,088 allocated bytes，contracts 後 20,130,680,832、vet 後 20,131,061,760；其中包含其他 checkout 活動，不以總量淨變化推定本任務成長。

依目前 normal／race exports、初始 inventory、source-root／compile-unit／test-log header 與 open／mapped／hard-link guards，分段清理本工作的舊 archives 與已完成的 test-input logs，本階段回收 4,518,031,360 bytes（約 4.21 GiB），自 PR-01B 累計 13,566,599,168 bytes。中斷測試的確切程序退出後，其已記錄 work roots 已不存在，未以時間或廣泛 glob 刪除其他目錄；其他 checkout 的工作與未明 ownership 的資料保留。

最終共享 GOCACHE 19,928,477,696 bytes；可辨識本任務 archives 733,970,432、其他輸出 9,265,152，合計 743,235,584 bytes（約 709 MiB），低於 2 GiB。Task scratch 約 22.5 MiB，沒有保留 executable；GOTMPDIR 約 13.3 MiB。提交後清理本階段 disposable logs，保留同一私有 task 目錄中的 source hashes、inventories、cleanup manifest 與後續 M1 recovery checkpoint，供下一階段接續使用。

## PR-03E：保留 canonical 圖片 owner 的恢復與重試

此子階段以 `cac9d373` 為父，修正真正恢復執行時的 reset 與政策驗證。CLI 與非視覺 synthesis 尚未交付，M1 整體仍未完成。

- 原有 `task_created` 的 resume／retry reset 延續既有 owner，不建立第二份圖片 binding。持久化前依同一 verified journal head 驗證原始成對 creation／binding、前一狀態、revision、重試計數、immutable execution contract 與 frozen media policy，再 conditional append；成功後才更新 live projection。
- Independent media audit 以相同規則驗證 reset；拒絕移除 binding／digest、同時宣告兩種 reset、偽造來源狀態／revision／dispatch／attempt，以及改寫 agent／goal／圖片綁定。原始唯一 owner、輸入摘要與歷史 receipts 不重寫。
- 執行政策的 authority 是分支 lineage 中的有效快照，並非每次 invocation 各有一份快照。媒體 ingress、provider admission、reset 與 audit 共用既有 canonical policy resolver，保留 snapshot migration／contract recovery／derived fork 的一般規則；逐次 profile／provider invocation／lease 的身分驗證仍照原規則執行。
- 新程序離線測試先保存 interrupted task，關閉原 coordinator、刪除來源 PNG，再由獨立 test executable 透過 `ResumeInterruptedTasks` 執行恢復或明確 retry reset。驗證實際 SDK HTTP model、圖片 bytes／MIME／順序／重複 occurrence、唯一 user turn、原始 owner／binding hash、正確 retry counter、完成 receipt digest 與獨立 audit。
- Fixture 必須保留 task execution contract；本次修正既有 media helper 遺漏該欄位造成 `submit_result` 缺席的問題。這不是 production provider 的重試修正。

定向恢復／拒絕／public API wire 測試通過（internal/team 16.272s），lint 通過（0 issues）。兩輪完整測試均未通過整包 20 分鐘上限：team 分別在 1200.138s 與 1200.111s timeout，當時不同 case 僅開始約 1 秒與 0 秒，沒有 assertion failure；其他 packages 通過。降低同期 I/O 爭用未解除第二次 timeout，因此不再把環境負載當成唯一原因。

以相同 source 列出測試清單，確認共有 4,769 個頂層測試；兩次分別到第 4,752（剩 17 個）與第 4,678（剩 91 個）才被整包預算截斷。據此將 `.mise.toml` 完整 suite 的 package timeout 改為 30 分鐘，提供整包執行餘裕；保留所有測試與既有個別 fixture deadlines，contracts／race 等定向 gates 的預算不變。這是明確記錄的驗證設定調整。最終完整 gate 已通過，team 實際 942.516s；期間沒有改動 Go source，執行時間的變動與前兩輪 timeout 分開記錄，不將前兩輪視為通過。未執行真實模型推論或圖片外送。

| PR-03E gate | 最終結果 | 證據 |
|---|---|---|
| 定向恢復／拒絕／public API wire | PASS | internal/team 16.272s；包含新 process 恢復與明確 retry reset |
| `mise run lint` | PASS | 0 issues |
| `mise run test` | PASS，exit 0 | team 942.516s、cmd/hufu 215.787s、tools 48.689s、web 0.329s；全部 packages 通過 |
| `mise run test-contracts` | PASS | team 147.913s、cmd/hufu 90.385s、reviewgate 14.764s、reviewprep 28.066s、web 0.454s |
| `mise run vet` | PASS | 無診斷 |
| 定向 race | PASS | internal/team 276.644s；native／media 與 timeout 時的 workspace case |

上述各輪驗證固定同一份 13 個 Go 檔的 SHA-256。兩輪 timeout 後只調整完整 suite 的總預算，另記錄 `.mise.toml` 的 SHA-256；lint／contracts／vet／race 的命令與 Go source 未改，沿用已通過的結果，沒有排除測試或放寬 assertion。

PR-03E 資源紀錄：刷新同一 source 的 normal exports 後，已識別的本任務編譯輸出短暫約 2.07 GiB；在下一個 heavyweight gate 前依 source／compile-unit provenance 清除過期 owned archives。兩次共回收 1,740,419,072 bytes，自前期累計回收 15,307,018,240 bytes；目前 normal／race exports 都保留本階段 source。30m 完整 gate 前的可辨識 task 輸出為 861,483,008 bytes（約 822 MiB），低於 2 GiB；共享 GOCACHE 19,076,440,064 bytes，包含其他工作的使用量，不能全歸本任務。

沿用既有 GOCACHE／GOTMPDIR／GOMODCACHE；只使用同一私有 task scratch，約 26 MiB，另含約 120 KiB 的下一階段未套用 CLI 草稿。兩個 timeout active-case 的確切 fixture 路徑在最終 gate 通過、確認沒有 cwd／open FD／mapping consumer 後清理，回收 294,912 bytes。未刪共享快取、未知 ownership 資料或另一 checkout 的程序。

最終 normal／race exports 對應同一 source；已無過期 owned archives 可清。共享 GOCACHE 19,228,086,272 bytes；本任務 archives 734,478,336、其他可辨識輸出 247,971,840，合計 982,450,176 bytes（約 937 MiB），低於 2 GiB。GOTMPDIR 約 13.8 MiB，task scratch 約 27 MiB，沒有保留 executable；/tmp 可用約 7.3 GiB、cache/module volume 約 62 GiB、available RAM 約 12 GiB。提交後清除本階段 disposable logs，保留 source/config hashes、inventories、cleanup manifest 與下一階段草稿／checkpoint，供同一目標接續使用。

M1 尚待 CLI／legacy／profile／dry-run／binary context guard、同一 direct lifecycle 的明確非視覺 synthesis、禁止含圖工作自動 replan，以及自動 retry／cost／acceptance 驗收；M2～M4 未完成。


## PR-03F：CLI、非視覺結果整理與自動重試

此子階段以 `532a6c45` 為父；必要 gates 已通過。M1 尚待 PR-03G result-only repair／verbatim 的完整 wire 驗證。

- canonical `run --image` 與 legacy root 使用 repeatable array，保留含逗號路徑、次序及重複值；canonical options 與 profile 不跨命令洩漏，尚未支援的子命令拒絕圖片 profile。含圖輸入在 router 前限制為明示 team/default、單一非空 direct worker；多 team、多 worker、pipe、自動選 team、fast routing 與 decision 等組合拒絕。`#path` 依內容辨識 UTF-8／binary，避免將圖片注入文字 context。
- `--dry-run --image` 只做 bounded 本機格式／大小與凍結目標政策檢查，明示 model vision capability 尚未驗證；不查 provider、不入 CAS、不建 task/event/conversation，也不將 preview 當成 durable admission。
- CLI 呼叫新的 `RunDirectAgentWithMediaAndSynthesis`，在原 direct invocation finalize 前，以 canonical worker result 與文字目標整理答案。結果消費者沒有圖片、原 worker grant、conversation 或工具，使用自己的 bound text profile、成本與完整 context admission；一個 step 的限制同時接入共用 stream runner。HTTP failure、工具呼叫、取消及容量不足均保留已完成 worker 與 receipt，不重跑看圖或建立第二個 run。
- CLI 直接使用原 invocation 的終態，禁止含圖工作落入舊的 coordinator synthesis／自動 replan。實際 CLI process 驗證未設定驗收及失敗驗收回傳非零，明確通過驗收才回傳成功；三者均保留答案、receipt 與稽核證據。
- built-in Ollama 的 profile 按既有 resolver 使用 model leaf；native profile verifier 現在允許這個既有身分，同時保留 named backend namespace、provider key、configuration hash、能力來源與 context 驗證。實際 canonical／legacy CLI fake-Ollama wire 驗證 model、兩張不同圖加重複第一張的 SHA／role／次序，圖片 flag 的原始路徑不進 durable events。
- 自動重試揭露三個共用 lifecycle 缺口：reducer 將失敗狀態永久封閉、transition 去重鍵忽略 lease、attempt activation 將 revision 重設為 1。修正後只有 runtime attempt marker、新 revision、空 dispatch 且一般狀態機允許的 retry start 才能重開；舊 checkpoint、stale lease、done/skipped 不重開。去重鍵綁定 occurrence revision/dispatch，無 lease 的 legacy key 保留；activation 沿用已提交的 revision，再發新 dispatch。沒有放寬 media admission。延伸的 verbatim SDK fixture 又重現 transcript finalization 持有舊 lease；現在 retry transition 前不開新 lease，提交後只開一次並刷新 finalization／error terminalization 的 captured identity。
- 真正 worker retry fixture 在第一次 HTTP rate-limit 後刪除原始 PNG；第二個 attempt 從 CAS 取得原圖，保留失敗證據、有序重複圖、receipt input digest 與獨立 audit。新的純文字 retry instruction 受 current-prompt compaction 保護，原 image-bearing turn 維持位置，SDK 不重複加入 Prompt。
- 成本 fixture 使用凍結 metered catalog 與 unknown-price deny：每次 image reservation 的完整 input tokens 對應 durable media admission、重複圖逐張計數、attempt 一致；文字整理有獨立 coordinator reservation。未回報 usage 的 fake SDK settlement 保留 `admission_bound`／usage unknown。預算不足測試證明零 HTTP、durable denial，沒有 reservation、settlement 或假造 acceptance。

| PR-03F gate | 目前結果 |
|---|---|
| 圖片／一般 retry 定向離線回歸 | PASS，最終 source internal/team 62.810s（含 summary／verbatim retry 與一般 fault matrix） |
| CLI／legacy／profile／dry-run／binary context／acceptance | 最終完整 suite 的 cmd/hufu PASS，212.436s；先前定向 11.277s |
| `mise run lint` | 最終 source PASS，0 issues |
| `mise run test-contracts` | PASS：team 115.990s、cmd 55.005s、reviewgate 23.134s、reviewprep 22.588s、web 0.251s |
| `mise run vet` | PASS，無診斷 |
| 定向 race | PASS，internal/team 285.362s |
| `mise run test` | PASS，exit 0：team 912.990s、cmd/hufu 212.436s、tools 48.334s、web 0.339s；全部 packages 通過 |

最終 gates 凍結 38 個 Go source／test 檔與四個驗證設定檔的 SHA-256。測試只連線 loopback fake provider；沒有真實 inference、圖片外送、登入／憑證或文件搬移。M2～M4 尚未開始。

首輪完整 gate 的 team 執行 925.761s，只有既有 fault-matrix 測試失敗：其三個 persistence 投影預期 revision 2，實際在修正後單調遞增至 4。此輪不算通過。已將預期明確寫成 start(1) → failure(2) → retry start(3) → done(4)，並新增完整 journal 的 revision 單調性與 attempts `[1,2]` 檢查，保留原有 receipt／context／task-journal 驗證。其他 packages 通過（cmd/hufu 198.558s、tools 48.614s、web 0.310s）；最終凍結 source 已重跑整個 required gate 並成功；沒有將首輪失敗視為通過。

Review 補充：ordinary retry 的 captured identity 已修正並通過 summary／verbatim wire；result-only repair 亦有同 attempt 的合法 lease 更新，尚需獨立的實際 SDK repair→transcript finalization 組合測試。既有 media text-view unit 只證明不帶圖片與不改寫 history，不足以替代這條完整 wire 驗收。此項列入 PR-03G，在完成前 M1 保持進行中。


PR-03F 資源紀錄（最終完整 gate 前）：沿用原 GOCACHE／GOTMPDIR／GOMODCACHE，沒有新 cache 或第二個 task scratch。多輪診斷與 normal exports 更新期間，可辨識本任務產物觀察到的暫時峰值約 2.10 GiB；取得當前 exports 後，在下一個 heavyweight gate 前依 source／compile-unit provenance 清理過期 archives。F 階段已回收 4,036,321,280 bytes，自前期累計 19,343,339,520 bytes；沒有刪除共用依賴或其他工作的產物。

最後 normal／race exports 對應同一份 38 個 Go source。完整 gate 前可辨識本任務輸出 1,281,441,792 bytes（archives 967,839,744、其他 project test cache 313,602,048），約 1.19 GiB；共享 GOCACHE 19,901,906,944 bytes，不能全歸本任務。GOTMPDIR 約 13.9 MiB，單一 task scratch 約 28 MiB，CLI fixture 的觀察峰值約 162 MiB，執行後自行清理 binary。沒有保留 executable；最終完整 suite 已通過，最終盤點如下。最近 /tmp 可用約 7.3 GiB、cache/module volume 約 61 GiB、available RAM 約 12 GiB。


最終完整 gate 後再次按 provenance 清理，沒有過期 owned archives 可刪。共享 GOCACHE 20,025,905,152 bytes；可辨識本任務 archives 967,839,744、其他 project test cache 434,417,664，合計 1,402,257,408 bytes（約 1.31 GiB），低於 2 GiB。此為目前 normal／race 與可重用測試證據所需產物，未清除共享依賴、toolchain 或未知來源資料。GOTMPDIR 約 14 MiB，task scratch 約 29 MiB，沒有保留 executable。階段提交後清除本階段 disposable logs，保留同一 task 目錄的 inventories、source/config hashes、cleanup manifest 與 PR-03G checkpoint，以接續同一目標。

本階段效益：可用 `--image` 明示送圖給單一 worker，先做不外送的本機 preview，保留圖片次序與重複值；答案整理沿用原本驗收結果，重試從 CAS 恢復並保留完整 receipt／成本證據。此證據支持離線 protocol 與 runtime 行為，不代表真實模型的辨識準確率或尚未執行的服務驗收。
