# Native Image Runtime 實作進度

> Status: active
> Authority: reference
> Verified-Commit: `515b95b9`
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
| PR-00 SDK/schema fixture 與回歸基線 | baseline 已確認；準備實作 | 測試草稿尚未加入工作樹 |
| PR-01 M0 private media/binding/policy/quota | 尚未開始 | — |
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
