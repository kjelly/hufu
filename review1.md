# Hufu 兩週 commit 潛在問題審查報告

- 審查範圍：`2026-08-12` ~ `2026-08-16`（50 個 commit，橫跨 `internal/team`、`internal/tools`、`internal/context`、`internal/memory`、`internal/promotion`、`internal/improve`、`cmd/hufu`）
- 審查方法：hufu-runtime-code-review / go-code-reviewer 準則，逐 commit 追蹤執行路徑與授權/復原/持久化契約
- 驗證 gate：`go build ./...` ✅、`go vet ./...` ✅、`go test ./...`（4378 passed）✅、`golangci-lint run`（No issues）✅

---

## 摘要

這批改動品質整體良好，主要方向是：把「協調者/worker 的 result 契約」與「唯讀執行」做成可驗證、可復原、可 fail-closed 的執行契約，並把記憶體/上下文改為 SQLite canonical store + 審查制 promotion。絕大多數路徑有測試覆蓋，且所有既有 gate 通過。

然而，**唯讀 bash 政策（read-only bash policy）存在可被利用的變更繞過**，會讓聲稱 `side_effect:none`（唯讀）的任務實際寫入磁碟。這是本次審查唯一的高嚴重度發現，其餘為中/低等級的契約或邊界問題。

---

## 高嚴重度

### H1 — 唯讀 bash 政策允許「寫出檔案」的 inspect 指令，造成唯讀任務可變更磁碟

**位置**：`internal/tools/bash_policy.go`（`checkReadOnlyBashSegment` / `readOnlyGitCommand`，commit 5292f00、8f1cf1b）
**路徑**：`side_effect:none` 的 worker / 直接 agent / declared-tool runner，其 `bash` 在 `AgentReadOnlyExecutionKey` 下執行（`coordinator_task_run.go:572`、`coordinator_run.go:65`、`coordinator_declared_tool_runner.go:75`），以及用於 `protocolAttemptWasReadOnly()`（`protocol_capability.go`）與協調者唯讀錯誤復原（`coordinator_task_run.go`、`tool_policy_gate.go`）。

**問題**：白名單只檢查「指令名稱」與少數參數，未檢查「會把內容寫出到檔案的參數」。下列指令都被判定為唯讀，但實際會寫入磁碟（我已在本機以實測驗證）：

```bash
git diff --output=/tmp/out.patch          # 寫出 patch 檔（實測：產生 101 bytes 檔案）
git diff --no-index --output=/tmp/x a b
git diff --cached --output=/tmp/x.patch   # 同上
go test -c -o /tmp/bin .                  # 寫出編譯後的 binary
go test -coverprofile=/tmp/c.out .        # 寫出 coverage 檔
go env -w GOFLAGS=...                     # 寫入 ~/.config/go/env
sort -o /tmp/x.txt file.txt               # sort -o 就地寫檔
golangci-lint run ... --fix               # --fix 已被拒，但 -o/--out-format 未全列
```

違反的**不變量**：
- 「`side_effect:none` 不得變更狀態」的執行契約（`bash.go:99-106` 是唯一的執行期強制點，但被上述參數繞過）。
- `protocolAttemptWasReadOnly()` 宣稱「可證明該 attempt 沒有跑任何 mutating tool，因此可以安全地重試/復原」。若 bash 曾以 `git diff --output` 等寫檔，該證明是假的，會把一個**可能已寫入檔案的 attempt** 當成「可安全重放」——放大副作用（尤其結合 8f1cf1b 新增的「唯讀證明後一次乾淨重試」與 66bfe81 的「唯讀協調者錯誤可復原」）。

**最小修復方向**：
1. `readOnlyGitCommand()` 對 `diff`/`show`/`log`/`format-patch` 等拒絕 `--output=`, `-o`, `--output=<file>`，以及任何會指定輸出檔的旗標；僅允許 stdout 輸出。
2. `go` 子命令除了子命令名白名單外，拒絕 `-o`, `--coverprofile=`, `-coverprofile`, `-c`(compile-only 寫 binary), `env -w` / `env -u`。
3. `sort`/`uniq`/`cut` 等拒絕 `-o`。
4. 補測試：每個「宣稱唯讀但含寫檔參數」的變體都要被 `checkReadOnlyBashCommand` 拒絕，且 `IsReadOnlyBashCommand` 回傳 false。

---

## 中嚴重度

### M1 — 唯讀白名單指令本身的潛在副作用未窮盡

**位置**：`bash_policy.go:209-221`
`echo` 被列為唯讀，但 `echo x > /path`（重導向）已由 `hasUnsafeReadOnlyShellSyntax` 的 `<`/`>` 攔截，故單獨 `echo` 安全；但 `echo >file` 外的組合需持續審視。更值得注意的是 `sed` 的 `-i` 檢查只針對 `-i` 單一 token，`sed -i.bak`（就地備份寫入）不會被 `containsField(...,"-i")` 命中——`sed -i.bak s/x/y/ file` 會實際改檔。這是與 H1 同族、但走另一參數形變的繞過，建議一併於 H1 修復中補齊。

### M2 — 唯讀政策的「證明」與「執行」共用同一份脆弱的 grammar，缺獨立性

**位置**：`IsReadOnlyBashCommand()`（bash_policy.go）與 `checkReadOnlyBashCommand()` 共用同一份邏輯。
當兩者共用同一套規則時，「執行期強制」與「事後證明 attempt 為唯讀」的信任同源。若未來有人放寬執行期規則以容許某個實際會寫檔的指令，`protocolAttemptWasReadOnly()` 會同步「證明」該 attempt 無副作用，導致復原路徑誤判可安全重放。建議維持 H1 修復後仍明確註記：復原用的唯讀證明應**獨立於、且比執行期更嚴格**，並加入測試防止兩者耦合。

### M3 — `go test` / `golangci-lint` 的唯讀允許過寬（與 H1 部分重疊）

**位置**：`checkReadOnlyBashSegment` 的 `go` / `golangci-lint` 分支。
`go list`/`go vet`/`go test` 本身多為唯讀，但 `go test -c` 寫 binary、`go test -coverprofile` 寫檔、`go list -export` 等也可能有輸出副作用。目前只檢查子命令名。建議明確允許的子命令 + 明確拒絕的旗標集合（見 H1 修復）。

---

## 低嚴重度 / 設計層面觀察

### L1 — 協調者「deterministic finish」把 `TaskSkipped` 視為成功完成

**位置**：`coordinator_run.go:986-998`（`canDeterministicallyFinishCompletedTasks` / `allTasksCompletedSuccessfully`）
當 coordinator 未呼叫 `finish` 且**所有** task 皆為 `TaskDone` **或** `TaskSkipped` 時，會以 `completedTasksSummary()` 直接組裝結果、設 `finishCalled=true` 並跳過 run 的接受度（acceptance）gate（該 gate 在其後的 `if c.LastRunResult()==nil` 區塊才執行，而 summary 路徑已提前返回）。`TaskSkipped` 通常代表「使用者拒絕執行」或「任務被跳過」，不必然是「成功完成」。若一個任務因使用者拒絕而 skipped、其餘皆 done，此路徑仍會把整個 run 報為完成並**可能繞過 acceptance**。這在非 unattended 互動下影響有限，但在 unattended / 自動化下可能把「部分任務未執行」當成「全部完成」。建議：要嘛只以 `TaskDone` 判定、要嘛在 summary 中明確標示 skipped 數量並讓 acceptance 仍可介入。

### L2 — 唯讀協調者錯誤復原依賴「工具是否唯讀」而非「該次呼叫是否有副作用」

**位置**：`coordinator_task_run.go`、`tool_policy_gate.go`（commit 66bfe81）、`protocol_capability.go`
把「協調者對唯讀工具（view/grep/ls/team_info/math/random 及唯讀 bash）的錯誤」從「終止」改成「可復原」。設計上是刻意且合理的窄例外。風險在於：`team_info`、`grep`、`view` 等被視為「無副作用」是基於工具類別，而非該次呼叫內容；只要該工具類別未來加入任何非唯讀 action，此處的唯讀假設就會悄悄失效。`team_info` 目前所有 action（`coordinator_tool_teaminfo.go:70-96`）皆唯讀，故現況安全；建議加註 + 測試，避免未來往 `team_info` 加入會變更狀態的 action。

### L3 — 協調者 prompt 與 delegation allowlist 對齊（正向）

**位置**：`coordinator_prompt.go`（commit 92eaaac）
`BuildOrchestratorPrompt` 改用 `workerNameList()`（policy 過濾後）而非 `uniqueWorkerDefs()`，解決了 prompt 列出被 allowlist 排除的 Helper 而觸發協調者 protocol repair 的問題。`workerDefsForNames` 以 lowercase map 查表，處理了大小寫不一致，設計正確。

### L4 — protocol repair 的一次 redirect 機制（正向，附帶提醒）

**位置**：`coordinator_tools_repair.go`（commit 34eef90）
在 tool-argument repair pending 期間，第一次誤呼叫其他工具不再立刻 fail-closed，而是給一次 redirect；第二次才終止。redirect 期間不會執行任何工具。`maxProtocolRepairRedirects=1` 的狀態記錄在 `protocolRepairAttempt` 內，且 redirect 後若 model 又呼叫正確工具仍會走正常 repair。設計審慎、有測試覆蓋（含「不執行」與「第二次失敗」）。唯一提醒：redirect 計數器放在 `pending`（每次新 repair 重建），不會跨多次獨立的 schema repair 累積誤判，是正確的語意。

### L5 — completion gate 移除「worker 自報 Risks/OpenQuestions」的降級（正向）

**位置**：`completion_gate.go`（commit 5fa52ad）
移除把 worker `TypedResult.Risks`/`OpenQuestions` 當作 run 阻擋的邏輯。論述合理：這些是報告/交接資料，而真正的未完成仍由 task status、objective evidence、acceptance、terminal-leak 檢查 fail-closed。`runSharedContextCandidateIDs` 的 terminal-leak 檢查保留。改動方向正確，且測試已更新為「成功的 task 不會因揭露 caveat 而降級」。

---

## 驗證執行摘要

| Gate | 結果 |
|------|------|
| `go build ./...` | ✅ 通過 |
| `go vet ./...` | ✅ 通過 |
| `go test ./...` | ✅ 4378 passed / 20 packages |
| `golangci-lint run` | ✅ No issues found |

H1/M1 的驗證方式是撰寫臨時測試（`internal/tools/ro_test.go`）以確認 `git diff --output`、`go test -c -o`、`go env -w`、`sort -o`、`sed -i.bak` 等被 `IsReadOnlyBashCommand` 誤判為唯讀；測試後已刪除，未改動原始碼。並以實際指令確認 `git diff --output` 會寫出內容檔。

---

## 建議優先處理順序

1. **H1 + M1**：修補唯讀 bash 政策對「寫檔參數」（`--output`/`-o`/`-c`/`-coverprofile`/`env -w`/`-i.bak`/`sort -o`）的繞過，並補齊測試。這是唯一會讓唯讀任務實際變更磁碟的高風險點，且會連帶影響 `protocolAttemptWasReadOnly()` 的安全重放證明。
2. **M2**：確保復原用的「唯讀證明」與執行期政策保持獨立、更嚴格，並以測試防止耦合。
3. **L1**：評估 `TaskSkipped` 是否該進入 deterministic-finish 的「成功」判定，以及是否會繞過 acceptance gate。
