# Native Image Runtime 實作進度

> Status: active
> Authority: reference
> Verified-Commit: `40bed236`（PR-04D2B 證據與程式碼同本次階段 commit；M0～M2 完成，M3／M4 未完成）
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
| PR-03F CLI／文字 synthesis／自動 worker retry | 完成；`7ebbf2f5` | wire／cost／acceptance、完整 suite、contracts、vet、lint 與定向 race 通過 |
| PR-03G result-only repair／verbatim wire | 完成；`49f8b0d6` | 實際 SDK repair／cost／transcript、完整 suite、contracts、vet、lint 與定向 race 通過 |
| PR-03 M1 CLI/direct/single native images | 完成；`49f8b0d6` | PR-03A～G；明示單 worker、CLI／API native wire、CAS retry/resume、文字 synthesis/repair、cost 與 acceptance 全部離線驗證 |
| PR-04A metadata-only provider 邊界 | 完成；`75dd2adc` | 固定最終 source 的 focused／lint／contracts／vet／race／full 全部通過；公開 M2 入口尚未開啟 |
| PR-04B coordinator 有序委派／續談／CLI | 完成；`5bee5767` | 離線 SDK／CLI／多輪／cost／audit，以及 lint、contracts、vet、race、完整 suite 全部通過 |
| PR-04C Chat pending／canonical admission | 完成；`2f98de86` | staging、plain／Chat TUI PTY、crash、reset／team，以及 lint、contracts、vet、race、完整 suite 全部通過 |
| PR-04D1 DAG／route fallback | 完成；`130eb3d7` | 候選授權、CAS wire、DAG 不重用／取消、audit／cost，以及 lint、contracts、vet、race、完整 suite 全部通過 |
| PR-04D2A model fanout 授權／結果保存 | 完成；`40bed236` | 雙 lease、獨立 SDK／audit／cost、accepted leaf results、拒絕／取消、文字 judge 與全部 required gates 通過 |
| PR-04D2B fanout retry／repair／resume | 完成；同本次階段 commit | 獨立 SDK retry／repair、新程序 CAS 恢復、blocked collector 拒絕、parent proxy ownership／文字 skeptic，以及最終 lint、contracts、vet、race、完整 suite 全部通過 |
| PR-04 M2 coordinator/chat/DAG/routing | 完成；同本次階段 commit | A／B／C／D1／D2A／D2B；支援明示的獨立 DAG、route 與 frozen LLM fanout；多候選 route＋fanout、blocked collector 追加 grant 維持明確拒絕 |
| PR-05 M3 MCP/result binding | 開發中，未驗收 | typed result／reference-only carrier／bounded Base64 staging 的未提交改動；尚未完成來源與消費授權、runtime-owned 結果或後續 wire E2E |
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


## PR-03G：同一 attempt 的文字 repair 與 transcript finalization

此子階段以 `7ebbf2f5` 為父，全部必要 gates 已通過。實際 SDK fixture 重現了 verbatim 的錯誤：原工作已產生觀察，result-only repair 也提交有效結果，但 transcript finalization 仍持有 `protocol_incomplete` transition 前的舊 lease，因此拒絕 provenance。這是同一 attempt 的合法 continuation，不能以重跑圖片分析解決。

修正集中在既有 `runRepair` 的授權邊界：建立 fresh repair lease 並綁定 `submit_result` context 時，同步更新 transcript finalization／error terminalization 的 captured occurrence。沒有在 finalization 時任意採納較新的 worker，也沒有修改 stale／rejected transcript 的拒絕條件；原有拒絕回歸一併通過。

新 fixture 使用真正 pinned SDK 與 loopback HTTP，以一個 step 的上限確實觸發 bounded result-only repair。第一次看圖後刪除來源 PNG，第二次 request 只有原始文字觀察與 `submit_result`，不帶圖片／Base64；summary 與 verbatim 都維持單一 run、task、attempt、原始 media digest／receipt／repair provenance。verbatim 另驗證 runtime 封存的 transcript artifact；原始圖片 history 保留兩個有序 refs，獨立 audit 通過，沒有重播圖片分析或創造 whole-run acceptance。最後一步的純文字提示可以重述目標；測試另外核對原 image-bearing user turn 仍唯一且 bytes／順序／重複值正確。

成本使用凍結 catalog：兩個獨立 reservation／settlement 的 purpose 依序為 `worker` 與 `protocol_repair`，綁定相同 run／task／attempt；fake SDK 未回報 usage，settlement 保留 `admission_bound` 與 unknown usage。開發測試只使用小型合成 PNG，沒有真實 inference、圖片外送、登入／憑證或文件搬移。

| PR-03G gate | 目前結果 |
|---|---|
| SDK repair／text view／stale transcript／cost 定向 | PASS，internal/team 11.863s |
| `mise run lint` | PASS，0 issues |
| `mise run test-contracts` | PASS：team 133.207s、cmd 65.415s、reviewgate 12.883s、reviewprep 24.936s、web 0.293s |
| `mise run vet` | PASS，無診斷 |
| 定向 race | PASS，internal/team 51.822s；repair／resume／stale submission／transcript finalization |
| `mise run test` | PASS，exit 0：team 911.930s、cmd/hufu 192.789s、tools 49.229s、web 0.302s；全部 packages 通過 |

最終 gates 固定兩個 Go source／test 檔與四個設定檔的 SHA-256。所有 gates 使用相同凍結 source，M1 現在完成；M2～M4 尚未開始。此修正不改變 SQL schema、frozen policy 或 persisted payload 格式，原始 receipt／history 保持 canonical authority。


PR-03G 資源紀錄：沿用既有 caches 與同一 task scratch。normal exports 更新時可辨識 task 產物暫時約 2.02 GiB，更新完立即在下一個 heavyweight gate 前依來源證據清理。G 階段兩次共清除 65 個 obsolete archives、回收 1,179,717,632 bytes，自前期累計回收 20,523,057,152 bytes；目前 normal／race exports 都對應最終 source。

完整 gate 後共享 GOCACHE 20,299,264,000 bytes；可辨識本任務 archives 968,036,352、其他 project test cache 557,932,544，合計 1,525,968,896 bytes（約 1.42 GiB），低於 2 GiB。GOTMPDIR 約 14.1 MiB，task scratch 約 30 MiB，沒有保留 executable。/tmp 可用約 7.3 GiB、cache/module volume 約 61 GiB、available RAM 約 12 GiB。提交後清除本階段 disposable logs，保留 inventories、source/config hashes、cleanup manifest 與 M2 continuation checkpoint；未刪共享依賴、toolchain、未知來源資料或其他 session 的產物。

M1 支援範圍：`hufu run --team … --image … '@worker …'`、legacy root 的明示 direct worker，以及 `RunTaskWithMedia`／`RunDirectAgentWithMedia`／具非視覺 synthesis 的 direct API；重複圖片、順序、CAS retry／新 process resume、summary／verbatim 文字 repair 與既有驗收語意均有離線證據。`--dry-run --image` 仍只提供本機 preview，不宣稱實際模型能力或 durable admission。成功的 result-only repair 沿用同一份看圖工作、逐字稿與收據。Coordinator delegation、Chat、DAG／fanout／fallback、MCP image results 與 Codex localImage 仍屬後續里程碑，未交付的入口繼續拒絕圖片。真實模型驗收由使用者另行安排，沒有執行推論或圖片外送。


## PR-04A：明確授權的 coordinator metadata 投影

以 `49f8b0d6` 為父，先建立 M2 的 provider 控制流程基礎；此階段不開啟 CLI Coordinator、Chat、DAG、fanout 或 fallback 的圖片入口。

- 原始請求、history、admission、commit 與 accounting 保留 canonical typed refs。私有 wire projection 只能把每個引用替換成固定格式的文字，包含 turn／part、MIME、尺寸和 byte size；不含圖片 bytes、CAS ID、digest 或 source path。
- 文字投影需要獨立的 runtime 授權。scope 綁定 live public invocation lease、run／invocation／branch、coordinator、完整 immutable provider context 與目前 canonical history 的有序引用。模型或一般 compiler 不能自行把原生送圖降級為文字。
- 僅 coordinator purpose 與 coordinator task 可使用這份 scope；worker 與 auxiliary role 維持自己的 native admission。原生圖片的三層能力、frozen egress、task lease 與 buffer ownership 檢查保留。
- metadata 計數使用實際送出的完整文字 projection，無 image token upper bound。編譯與送出前重新驗證 canonical metadata、provider identity 與授權；HTTP 前另核對已提交的精確 profile。這條流程不讀 image blob，也不產生 `media_provider_admitted` 圖片外送許可；正常 token/cost reservation 與 settlement 照常適用。
- 離線測試使用明確不支援圖片的 text model 與 pinned SDK／本機 fake HTTP endpoint。覆蓋所有四種模型呼叫、三種 request shape、順序／重複、原始 history 不變、移除 blob 後的 metadata 行為、越權與失效 scope、HTTP 前 lease 撤銷，以及 worker 不繼承 metadata 權限而繼續原生送圖。

審查補驗曾重現兩個授權缺口：journal 讀取後 consumer 不再適用 metadata view，以及 run 身分消失時，舊實作都可能回傳 `nil`。兩個新測試在修正前均失敗（1.170s）；修正後要求最後重驗同時滿足「仍適用且沒有錯誤」，已存在的 metadata scope 若失去 run 身分或 context 取消則明確拒絕。初版完整 suite 雖通過（team 939.897s、cmd 198.787s），僅為初版證據，不作最終 source 的通過依據。

| PR-04A gate | 最終修正版本結果 |
|---|---|
| metadata/native focused regression | PASS, exit 0；agent 0.225s、team 34.357s，含修正前失敗的兩個授權邊界 |
| `mise run lint` | PASS, exit 0；`0 issues.` |
| `mise run test-contracts` | PASS, exit 0；team 121.517s、cmd 67.214s、reviewgate 12.189s、reviewprep 23.112s、web 0.254s |
| `mise run vet` | PASS, exit 0；無診斷 |
| metadata/compiler/transport 定向 race | PASS, exit 0；agent 1.515s、team 114.930s |
| `mise run test` | PASS, exit 0；team 935.100s、cmd 201.192s、tools 48.738s、web 0.465s |

M2 尚需有序 `image_inputs` 解析與 scope 綁定、Coordinator 完整委派／完成契約、Chat pending state、DAG／fanout／fallback，以及各入口生命週期回歸。metadata 本身不代表已看過圖片或已完成含圖工作，未交付的入口繼續拒絕圖片。沒有呼叫真實模型或外送圖片。


PR-04A 最終驗證固定 11 個 Go source／test 檔與四個設定檔的 SHA-256；修正前的初版證據另行標記，最終 gates 全部使用相同修正版。此階段不改 SQL schema、既有 receipt 或 persisted media binding 格式；原生 worker 的三層能力與逐請求 byte 授權保留。效益是讓 coordinator 在明確授權下以純文字處理圖片清單與順序，為不必接收像素的控制流程建立共同邊界；完整委派、Chat 與圖形路由仍需後續 M2 實作，不能視為已交付。

資源紀錄：沿用原有 caches 與同一 private task scratch；normal/race 清單均對應最終 source。階段內清除 147 個過期 archives 與三份經測試案例版本證明已過期的 test-input logs，共回收 2,095,947,776 bytes；前期累計回收 22,619,004,928 bytes。清理前先確認無執行中的驗證，保護 open／mapped／linked entries 與初始共享資料；最終修正版 test-input log 保留。各次編譯選取的 task 產物峰值約 1.98 GiB，未超過 2 GiB 保留預算。

最終共享 GOCACHE 20,628,586,496 bytes；可辨識 task archives 969,195,520、其他 project test cache 618,790,912，合計 1,587,986,432 bytes（約 1.48 GiB）。GOTMPDIR 約 14.3 MiB、task scratch 約 32.5 MiB；沒有保留 executable。提交後清除本階段 disposable logs，保留 source/settings hashes、inventories、cleanup manifests 與 continuation checkpoint 以接續 M2；未刪共享 dependencies、toolchain、未知來源資料或其他 session 的產物。所有測試只使用離線 fixtures／loopback HTTP，沒有真實模型推論、圖片外送、登入／憑證操作或文件搬移。


## PR-04B：Coordinator 有序圖片委派與多輪輸入

- `RunWithMedia`／`ContinueWithMedia` 沿既有 public invocation、workspace、policy 與 history 生命週期接收圖片。協調者只收到經授權的文字中繼資料，以 `agent.tasks[].image_inputs` 選擇 `(turn_id, part_index)`；runtime 解析來源、建立新 task binding，並在 cache／dedup 前固定有序 digest。重複選取保留，模型不能提交 CAS ID、路徑或 runtime-owned binding。
- worker 只取得自己明示選取的圖片；子 worker 的選取範圍限於父 task 的 grant。原有 direct/single worker 的完整來源與 owner 檢查保留。未知來源、文字 part、過期 lease、binding override 在建立 task 前拒絕；選取不會自動授權 auxiliary role。
- canonical 與 legacy CLI 的單一協調者入口支援 repeatable `--image`。普通 segment dispatcher 使用共用 runner，避免旗標通過解析後遺失圖片。dry-run 只做本機有界驗證與凍結 provider 身分比對，不建立 CAS／事件，也不要求文字協調者具有視覺能力。
- 新圖片 turn 的 user text 與有序 refs 只提交一次；純文字續談仍保留新的訊息。模型切換重新建立候選模型的 metadata grant，不能沿用上一個模型的 grant。worker 執行邊界明確建立自己的 cost purpose，避免繼承協調者的 purpose。
- `finish` 要求當前圖片有完成的 native worker task、相符 media receipt 與 accepted result，後續仍經既有 whole-run acceptance；僅有中繼資料不能完成圖片工作。
- 多輪測試揭露並修正兩個一般 runtime 問題：checkpoint 保留策略移除過期內容後，只在同分支存在完整且精確驗證的後繼 checkpoint 時，允許略過更早的確定性 attestation；目前 snapshot、branch、digest 與 generation 檢查保留。新 invocation 同時重新綁定 decision journal，重用原 CAS/index，舊 invocation 的已關閉 journal 不會轉接成新權限。

效益：文字協調者能指派視覺工作並整理已接受的結果；各 worker 只接收指定圖片，降低不必要的圖片傳輸與 token 預留。對話可再附新圖或接續文字追問，原始檔案移除後仍由 CAS 提供已提交圖片，且不重複提交舊 turn。

離線 SDK 測試包含兩輪圖片分析與一輪文字追問、合計七次 provider HTTP 呼叫及成本結算；每輪 worker 僅收到選定的原始 bytes，協調者始終只有 metadata。CLI 程序測試涵蓋 canonical／legacy、依序與重複選取、既有 M1 direct 路徑、canonical evidence 與 acceptance。checkpoint 測試另驗證缺少後繼證明、錯誤身分、錯誤分支或目前 payload 不符仍拒絕。

| PR-04B gate | 結果 |
|---|---|
| `mise run lint` | PASS，exit 0；`0 issues.`；首輪唯一 complexity 診斷已以保留順序的 workspace 檢查抽取修正 |
| `mise run test-contracts` | PASS，exit 0；team 121.049s、cmd 67.298s、reviewgate 14.053s、reviewprep 24.648s、web 0.548s |
| `mise run vet` | PASS，exit 0；無診斷 |
| 定向 race | PASS，exit 0；media 1.019s、team 147.582s、cmd 16.449s；範圍為圖片選取、metadata、續談、checkpoint/journal 與 CLI |
| `mise run test` | PASS，exit 0；team 961.825s、cmd 203.254s、tools 48.527s、TUI 6.731s、web 0.320s |

本階段以 `75dd2adc` 為父；上述最終 gates 固定同一份 32 個 Go 檔及四個設定檔，完成後核對 SHA-256 與檔案集合一致，驗證紀錄與變更一併提交。Chat plain/TUI 的 pending commands、staging 與 commit ACK、DAG／fanout／route fallback 的完整驗證及 phase workflow 尚待後續階段；多模型、leaf、action、sidecar 與 phase 圖片組合繼續明確拒絕，M2 整體未完成。所有服務均為 fake provider／loopback，沒有呼叫真實模型或外送圖片。


PR-04B 資源紀錄：全程沿用同一 task scratch、GOCACHE、GOTMPDIR 與 module cache。最終 normal／race export 清單保護目前需要的版本；本階段清除 185 個過期 task archives 與一份由新／舊測試特徵證明過期的 test-input log，回收 3,496,677,376 bytes，累計回收 26,115,682,304 bytes。編譯新版本及保留已驗證基準時，可辨識 task 輸出曾暫時達 2,303,897,600 bytes（約 2.15 GiB，超過預設保留預算約 149 MiB）；在下一 heavyweight gate 前即清除過期版本。最終可辨識 task 產物 1,653,784,576 bytes（約 1.54 GiB），低於 2 GiB；共享 GOCACHE 總量 20,970,385,408 bytes，非全部由本工作新增。

GOTMPDIR 約 14.4 MiB、private task scratch 約 34 MiB；沒有保留 executable。保留 source/settings hashes、資源與清理 manifests、必要驗證摘要和尚未接入的下一階段小型草稿，供後續工作續行；草稿不算已交付程式碼或測試證據。清理只處理確切來源已證明且沒有 consumer 的本工作輸出，沒有清除共享依賴、toolchain、未確認來源的既有 fixtures 或其他 session 資料。

## PR-04C：Chat pending 圖片與提交邊界

- plain 與 Chat TUI 共用 `/image add/list/remove/clear`、有序 staging、quota 與 turn sequencer。圖片在 add 時驗證並保存 process-local bytes；來源檔之後變更或刪除不影響本次輸入。只顯示安全名稱及 metadata，圖片控制指令不進 readline history。
- canonical input checkpoint 的 admission 通知與 provider 成功分開。確認提交後立即消費 pending／釋放 staging，provider 失敗或取消也不加回；提交前失敗保留有效圖片；無法確認提交狀態時凍結並要求恢復。已排隊的下一個 prompt 不會自動採用前一回合保留的圖片。
- reset／切 team 僅在成功後釋放 pending；正常退出清理並回報數量。未提交 staging 不承諾跨程序恢復。已提交圖片沿原 TurnID／snapshot／CAS 恢復，不重讀來源檔或建立新圖片 turn。`finish` 同時檢查目前 conversation 尚未完成的圖片，避免在提交後失敗、下一輪純文字續談時僅憑 metadata 宣稱完成。
- staging 與 transport 各自使用有界 logical-byte quota，避免巢狀取得同一 semaphore；不宣稱它們等於 RSS 上限。legacy dashboard／prompt injection 明確拒絕 `/image`，支援入口為 `chat --ui plain` 與 `chat --ui chat`。

效益：兩種 Chat 前端的圖片提交與重試行為一致，避免原始路徑重讀、失敗後意外重送和路徑歷史洩漏；圖片提交、worker 完成及整體 acceptance 仍是不同狀態。

定向證據：core staging／admission／history／native invocation 回歸通過（media 0.011s、team 17.463s）；完整 TUI package 通過（6.150s），含實際 Bubble Tea program 的指令、resize 與退出。plain／Chat TUI PTY 驗證刪除來源後的原生 bytes、只有 worker 收到圖片、canonical evidence 與歷史隱私；程序崩潰測試驗證提交前 pending 為空、提交後 snapshot 不變且 CAS 可恢復。Chat lifecycle 定向測試通過（8.707s）。

| PR-04C gate | 結果 |
|---|---|
| `mise run lint` | PASS，exit 0；`0 issues.`；已修正 helper 的 error 回傳位置 |
| `mise run test-contracts` | PASS，exit 0；team 119.222s、cmd 65.426s、reviewgate 13.519s、reviewprep 23.916s、web 0.275s |
| `mise run vet` | PASS，exit 0；無診斷 |
| staging／Chat／admission 定向 race | PASS，exit 0；media 1.039s、team 61.401s、conversation 1.082s、readline 1.016s、TUI 4.363s、cmd 14.604s |
| `mise run test` | PASS，exit 0；team 1004.466s、cmd 212.729s、tools 50.000s、TUI 6.682s、web 0.338s |

最終驗證固定 46 個 Go source／test 檔及四個設定檔的 SHA-256。contracts 完成後僅補強 TUI program 測試的失敗清理；該測試不在 contracts 的依賴中，正式程式碼相同。補強後已重跑 lint／vet 與上述 race，並以同一份最終 source 執行完整 suite。

本階段清除 165 個過期 archives 與一份已被本次完整回歸取代的基準 test-input log，回收 1,548,144,640 bytes；累計回收 27,663,826,944 bytes。清理前量測的 task 產物最高為 2,146,942,976 bytes，低於 2 GiB；每輪 heavyweight gate 前均檢查容量，並保護目前 normal／race outputs、共享依賴與其他 session 的 open／mapped 檔案。

最終可辨識 task 產物 1,722,208,256 bytes（約 1.60 GiB），共享 GOCACHE 21,276,950,528 bytes；GOTMPDIR 約 14.5 MiB、private task scratch 約 35 MiB。沒有保留 executable；續作所需的 source hashes、快取來源／清理 manifests 與驗證 checkpoint 留在同一 private task directory。未呼叫真實模型或搬移文件。M2 整體仍待 DAG／fanout／fallback；M3、M4 尚未開始。

## PR-04D1：DAG 與 route fallback

- 圖片 route 綁定須符合 frozen worker 的 route owner／digest／primary。每次 request 與獨立 audit 共用同一個 canonical attempt 重放：從既有 fallback decision 與帶 `dispatch_attempt` 的 task checkpoint 求出當次 target；候選清單成員身分本身不授權送圖。一般 retry 回到 primary，fallback 必須有前一 attempt 的正式決策；來源 run、revision、candidate 順序與 fallback 次數都受檢查。
- 每個實際候選仍獨立檢查 model／backend／adapter 的圖片能力、provider identity、egress 與 context／cost 預留。切換模型保留相同 ordered binding／CAS bytes；不重讀 source path、不擴張 owner，也不新增 persisted media schema。
- 真正 SDK／loopback 封包測試涵蓋 vision→vision、vision→text／unknown／未授權、auth failure 不 fallback 與 primary 成功；第一次 HTTP 收到圖後刪除原始檔案。合法 fallback 的原圖順序／重複次數、成本、receipt 與 audit 通過；替換成同清單的另一個模型仍拒絕。
- DAG 使用相同文字目標、不同圖片與相依邊，驗證兩個 occurrence 不命中自動 reuse，各自取得自己的圖片與 completion receipt。取消第一個 worker 後，不啟動 dependent worker，也不產生其結果或 execution receipt。

效益：備援模型切換不會遺失原圖或沿用另一個候選的圖片授權；DAG 的相同文字任務能保留不同圖片的執行證據，取消後不再消耗依賴節點的模型呼叫。

固定最終 8 個 Go 檔與四個設定檔後的定向回歸通過（team 35.389s），涵蓋上述案例與既有 metadata coordinator／native retry／provider media 邊界。

| PR-04D1 gate | 結果 |
|---|---|
| `mise run lint` | PASS，exit 0；`0 issues.` |
| `mise run test-contracts` | PASS，exit 0；team 118.146s、cmd 64.600s、reviewgate 12.518s、reviewprep 25.124s、web 0.250s |
| `mise run vet` | PASS，exit 0；無診斷 |
| route／DAG／native lifecycle 定向 race | PASS，exit 0；team 146.636s |
| `mise run test` | PASS，exit 0；team 975.388s、cmd 211.840s、tools 48.421s、TUI 6.504s、web 0.303s |

上述 gates 全部使用相同最終 source／settings，完成後再次核對 SHA-256 一致；沒有修改 schema、歷史 receipt 或 acceptance 契約。D1 已完成，M2 整體仍待 model fanout 的 leaf 授權與獨立 audit，不能視為 M2 已完成。

本階段清除 66 個過期 archives 與一份已被新測試特徵證明取代的 C team test-input log，回收 1,398,792,192 bytes，累計回收 29,062,619,136 bytes。fixture 迭代與保留已驗證基準時，量測 task 產物曾達 2,225,729,536 bytes（約 2.07 GiB）；在下一 heavyweight command 前即清理過期版本。最終可辨識 task 產物 1,787,002,880 bytes（約 1.66 GiB），低於 2 GiB，沒有保留預算例外。

共享 GOCACHE 21,524,844,544 bytes，GOTMPDIR 約 14.6 MiB、private task scratch 約 37 MiB；沿用既有 caches，沒有保留 standalone executable。保留目前 normal／race exports、source hashes、清理來源與驗證 checkpoint 供下一階段續作；未知來源、共享依賴與使用中的檔案不刪除。沒有真實模型呼叫、圖片外送或文件搬移。

## PR-04D2A：model fanout 授權與結果保存

- 每個 leaf 以 frozen topology slot、stable model-execution ID、原有 ordered binding 和 parent／private leaf 雙 lease 取得自己的 provider admission；逐請求能力、egress、token／cost 與最後 HTTP 授權均保留。獨立 audit 驗證 parent／slot 身分與 leaf 間的 parent lease 一致性。
- 原始成功 leaf receipt 保存完整 runtime-owned `accepted_result`，不以非終態 `submitted_result` 或合併文字替代。Coordinator finish 與單 task 的 fanout 完成都須驗證所有已啟動 leaf 的成功 receipt、模型／圖片／attempt 身分與 accepted result。
- 失敗回歸重現 private leaf checkpoint 發布成 parent lifecycle、阻擋同伴合法授權的問題；改由 parent 單獨發布 task/session checkpoint，result-collection 保留專用 durable events。另補齊單 task fanout 的 parent judge provider 邊界初始化，未授權的 auxiliary role 仍只消費文字結果。

效益：同一圖片工作可由多個獲授權模型獨立分析並保留完整來源與限制；某個 leaf 拒絕／失敗不會抹去另一個 leaf 的成功證據，也不會被誤判為整體完成。

固定 25 個 team Go 檔與四個設定檔後的定向回歸通過（15.998s），包含原圖刪除後 CAS wire／順序／重複、Coordinator 完成、雙 lease 撤銷、leaf 拒絕、雙 leaf 取消、accepted result／payload hash 重播與文字 judge 的獨立呼叫／成本。typed choice resolver 依既有規則記為 sidecar 成本，綁定被評選的 parent task；judge 失敗後保留兩個原始 leaf 結果。本階段 D2A 已完成；D2B 的 native fanout retry／result-only repair／新程序 resume 組合仍待獨立驗證，不宣告整個 M2 完成。

| PR-04D2A gate | 結果 |
|---|---|
| `mise run lint` | PASS，exit 0；`0 issues.` |
| `mise run test-contracts` | PASS，exit 0；team 130.966s、cmd 57.093s、reviewgate 13.319s、reviewprep 23.109s、web 0.428s；inspect／operator／auditverify 通過 |
| `mise run vet` | PASS，exit 0；無診斷 |
| fanout／native lifecycle 定向 race | PASS，exit 0；team 162.746s，無競態診斷 |
| native／Coordinator／Chat CLI 定向 race | PASS，exit 0；cmd 34.561s，含 ancestor Git 變化、PTY 與 crash recovery |
| `mise run test` | 最終 PASS，exit 0；team 927.771s、cmd 187.762s、tools 48.490s、TUI cache 命中、web 0.303s |

相容性：新增可選 `model_fanout` admission grant 與 receipt `accepted_result`，零值不寫出；不改 SQL schema，不重寫歷史 receipts／hashes。既有單 worker 路徑不需 migration；歷史上未獲支援的圖片 fanout 不能憑缺少的新證據宣稱完成。真實模型驗收仍另行安排。

首輪完整 suite 的 team 通過（979.406s），但 CLI legacy fixture 因 project-root policy drift 失敗，該輪整體不算通過。根因是 `t.TempDir()` 位於既有 `GOTMPDIR` 下，空 project fixture 沒有自己的 Git marker；已用重疊 sandbox／host 唯讀程序確認暫時建立的 build-cache 祖先 `.git` 可被外部 subprocess 看見，改變 project discovery。新增受控祖先 marker 變化的回歸，修正前穩定失敗（6.471s、零模型呼叫）；為 fixture 建立自己的 Git 邊界後，native／Coordinator／Chat PTY／crash 定向測試全數通過（20.947s）。execution-world fingerprint 與授權檢查保持有效。

最終固定 26 個 Go 檔與四個設定檔，全部 gates 完成後再次核對 SHA-256 相同。後加入的唯一 Go 檔變更是 CLI 測試 fixture；原先 25 個 team Go 檔與設定檔未變，contracts 與 team race 證據對應相同 source。CLI 修正後重跑 lint／vet、CLI race 與完整 suite。最終 lint 為 `0 issues.`、exit 0；sandbox 曾對原有 golangci cache 的寫入產生警告，未影響分析完成，未變更或另建快取。

支援範圍：frozen LLM topology 的各 slot 獨立授權；與多候選 execution-route 同時使用的組合仍在 provider 前拒絕。D2B 將補 native fanout 的 retry／repair／新程序恢復封包驗收；本節不將它們標為完成，也不把 MCP／Codex 圖片入口視為已交付。

資源紀錄：本階段清除 137 個過期 task archives 與一份已被 D2 新測試特徵證明取代的 D1 full-team test log，回收 2,602,061,824 bytes；累計回收 31,664,680,960 bytes。清理保護目前 normal／race exports、open／mapped／linked 檔案、共享依賴與未知來源輸出。量測的 task 產物峰值為 2,126,966,784 bytes，低於 2 GiB，沒有保留預算例外。

最終可辨識 task 產物 1,797,165,056 bytes（約 1.67 GiB），共享 GOCACHE 21,694,033,920 bytes；GOTMPDIR 約 14.8 MiB、同一 private task scratch 約 39.4 MiB。未保留 standalone executable；續作需要的 hashes、清理來源 manifests、失敗／通過 gate logs 與 recovery checkpoint 保留。沒有真實模型呼叫、圖片外送或文件搬移。


## PR-04D2B：fanout 生命週期與 provider ownership

- 實際 fake HTTP／SDK 覆蓋各 leaf 的 worker retry，以及 summary／verbatim 的 tool-result repair。retry 保留原 CAS、有序重複圖片、獨立 model-execution ID 與所有失敗／成功 receipts；repair 只消費自己的文字證據，不重新送圖或取得執行工具。
- 新程序從原 canonical CAS 恢復工作前的 fanout，並驗證 genuine reset 後的 occurrence provenance。blocked runtime collector 保留 sealed work／已用 budget，普通 resume 與 operator 追加 grant 都不能重播 worker；兩次新程序 admission 後 receipts 維持原值。
- 修正 private leaf 將 failure fingerprint 以 parent task ID 發布的問題。事件重播只恢復 parent 的 canonical 指紋；leaf 原始診斷保留在各自 receipts，不刪歷史紀錄或繞過 parity 檢查。
- public parent 在建立 fanout clients 前取得 provider proxy；啟動失敗時所有 worker transport 為零。以 fixture provider 分開計數，避免將 fallback proxy 計入同一個 provider 的啟動次數。
- configured skeptic 經 parent-owned boundary／sidecar 執行，保留 leaf identity，只讀文字結果；HTTP 401 導致全數 abstain 時沿用 fail-closed 驗收。沒有將不可用 reviewer 當成功。
- 原路徑的 proxy-startup failure 回歸實際觀察到 2 個 worker HTTP（1.641s，FAIL），修正後完整定向組合通過（20.973s）。早期未按 provider 區分的總次數 probe 不作缺陷證據。
- 最終驗證綁定 11 個 Go 檔與 4 個設定／依賴檔的 SHA-256；不包含真實模型呼叫、登入、部署、圖片外送或文件搬移。

效益：同一圖片工作能保留各模型的原始證據，結果修補不重跑分析；新程序恢復不再依賴原始檔案，proxy 啟動失敗與 reviewer 不可用也不會被誤當完成。沒有宣稱模型的視覺判斷已通過真實服務驗收。

| PR-04D2B 最終 gate | 結果 |
|---|---|
| 定向 SDK／生命週期／boundary 回歸 | PASS，team 20.973s |
| `mise run lint` | PASS，exit 0，`0 issues.`；既有 lint cache save 警告不影響分析完成 |
| `mise run test-contracts` | PASS，team 115.046s、cmd/hufu 58.845s、reviewgate 19.619s、reviewprep 24.018s、web 0.256s；inspect／operator／auditverify 通過 |
| `mise run vet` | PASS，exit 0，無診斷 |
| 定向 `-race` | PASS，team 120.787s |
| `mise run test` | PASS，exit 0；team 1059.414s、cmd/hufu 207.373s、tools 49.784s、evalharness 44.300s、improver 153.936s、modelcatalog 0.653s、web 0.303s；其他套件通過，包含既有 cache 命中，TUI 為 cached |

定向／race 範圍包括 native fanout worker retry、tool-result repair、工作前新程序 resume/reset、blocked collector 新程序拒絕、正常／失敗 proxy startup、各 leaf 授權與證據，以及既有一般 worker fingerprint／collection／skeptic 回歸。contracts、vet、race、full 依序執行，前後比對同一份 15 檔 SHA-256。

較早的 13 檔版本完成 lint／contracts／vet／race，但其 full run 在發現 proxy ownership 需補驗後停止；該次只有已完成的 CLI 等套件結果，不列為完整 suite 通過。補測與修正後重跑本表的所有最終 gates，未縮減測試範圍或更換工具鏈。

資源紀錄：本階段按來源清除過期 task archives／已被新版測試取代的 test logs，回收 3,029,663,744 bytes；本工作累計回收 34,694,344,704 bytes。另清理已退出之診斷／中止測試的確切暫存根與 private-task engine 目錄，回收 154,062,848 bytes。開啟中、mapped／linked、來源不明及其他專案使用中的輸出都保留，未清除共享 caches 或變更 GOCACHE／GOTMPDIR。

最終可辨識本工作產物 1,762,033,664 bytes（約 1.64 GiB），低於 2 GiB；共享 GOCACHE 21,445,267,456 bytes，GOTMPDIR 約 15.2 MiB，private task scratch 約 18.5 MiB。續作保留當前 normal／race exports、必要 hashes／清理來源 manifests、gate logs 與 recovery checkpoint；沒有保留 standalone executable。M3／M4 尚未開始，不把此階段完成視為整個圖片 I/O 計畫完成。

## PR-05：開發中，尚未達成階段驗收

2026-11-06 核對工作區已有未提交的 typed MCP result、trusted publisher hook、reference-only tool carrier、四種 provider API 的 message projection，以及 bounded Base64 staging。現存定向測試紀錄包含 agent／media／MCP 套件通過；最新 publisher 定向紀錄為 MCP 0.023s。這些是局部開發證據，不是 M3 的後續 wire E2E、完整 required gates 或已交付聲明。

接續工作以[規格第 8.2 節](native-image-runtime.md#82-mcp-mixed-content-與-output-artifacts)為準：direct／gateway 共用 publication、canonical producer／consumer 授權、token/preflight 與完整 pairing、history／repair／replay、runtime-owned result binding，以及 fake MCP → fake provider 的完整回歸。PR-06／PR-07 未開始。現有未提交程式改動不能沿用 M2 的綠燈當作驗收結果。

本次文件修訂移除 raw SDK image response 與 reference-only 保存契約之間的矛盾，補齊上述交付邊界及預期效益；未移動文件。使用者指定的 Ollama `deepseek-v4.1-flash:cloud` 僅記為另行安排的真實驗收背景，未發出模型請求或將帳戶／外送設定列入 coding agent 待辦。
