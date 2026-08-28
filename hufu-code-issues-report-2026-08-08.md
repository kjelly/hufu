# Hufu 程式碼問題獨立報告

日期：2026-08-08（UTC）
審查目標：`/home/ubuntu/nfs/github/agent-team-cli`
模組：`github.com/kjelly/hufu`
審查 revision：`e28e4df833f4a9ee6c8a88581d1d597699a4b21e`
審查 tree：`b1bb42c9a645d24c3d0a38d174e2dbd1d4ce1038`

本報告只涵蓋 Hufu runtime 原始碼，不涵蓋 Pilot、Pilot playbook 或
`.agent-teams/` team-local 文件。審查期間沒有修改 Hufu 程式碼。

## 摘要

Hufu 的 typed task result、transcript、receipt 與 structured execution 已有
不少防護，但目前仍存在會影響「重啟後續跑、證據可信度與跨 worker 結果交接」的
runtime 問題。最重要的是證據簽章使用 process-local secret，導致跨 process
resume 後既有簽章無法驗證。

## Findings

### P1：證據 HMAC 使用 process-local key，重啟後所有 persisted evidence 失效

位置：

- `internal/team/task_result.go:48-76`
- `internal/team/status.go:565-594`
- `internal/team/run_result.go:807-823`

`GetSystemSecret()` 以 `sync.Once` 在目前 process 內產生隨機 key，並且不保存到
session 或 run evidence。`finalizeVerbatimTaskResult()` 用這個 key 簽署 transcript
evidence；後續 `run_result.go` 又用同一 API 重新取 key 驗證。

因此，process A 產生並保存的 `EvidenceRef.SystemHMAC`，在 process B resume
同一個 workspace 後會用另一把 key 驗證並失敗。`TodoList.SetTypedResult()` 也會
把無法用新 key 驗證的簽章清空。這會讓 crash-resume 或跨 process 的 resolver
evidence 被判定為沒有有效證據，即使原始 transcript 與內容完全未被修改。

最小重現情境：

1. process A 呼叫 `finalizeVerbatimTaskResult()`，保存 `TaskResult` 與 session。
2. 結束 process A。
3. process B 載入同一 workspace，執行 `VerifyEvidenceSignature()`。
4. 驗證結果為 false，因 `GetSystemSecret()` 已產生不同 key。

修正方向：為每個 run 建立可安全復原的 key lifecycle，或改用可跨 process
驗證的 sealed evidence 設計。key 不應直接放進普通 session JSON；至少要有明確
的 key storage、權限、rotation 與失效策略，並增加真正的跨 process integration test。

### P1：提交 artifact 時固定寫入 `Attempt: 1`，重試後 provenance 錯誤

位置：`internal/team/coordinator_tools_result.go:357-371`

`materializeSubmittedArtifacts()` 將 worker 提交的 artifact snapshot 寫入
`FileArtifactStore` 時，固定使用：

```go
Attempt: 1,
```

但同一 task 可能在第 2 次或後續 retry 才提交結果。此時 artifact 的內容可能是
第 2 次執行產生的，metadata 卻宣稱 attempt 1。下游 evidence manifest、audit
與 recovery 依據這個欄位時會得到錯誤的因果關係。

修正方向：從目前 task execution context 傳入並驗證實際 attempt；若 context
缺失，應拒絕 materialization，而不是回退到 1。應增加 retry-attempt artifact
integration test，確認 snapshot、typed result、journal 與 manifest 使用同一個
attempt。

### P1：`task_result` 的「最近完成」查詢依 TODO 插入順序，不依完成時間

位置：`internal/team/coordinator_tool_teaminfo.go:292-318`

`completedTaskResultForAgent()` 由 `TodoList.Items()` 反向掃描，第一個符合的
item 就回傳。`TodoList.Items()` 的順序是 task 建立／插入順序；並行 worker 的
實際完成順序則記錄在 `TodoItem.EndedAt`（`internal/team/status.go:226-227`、
`503-506`）。兩者不一定相同。

重現情境：

1. 依序建立同一 agent 的 task A、task B。
2. B 先完成，A 後完成。
3. 呼叫不帶 `task_id`、不帶 selector 的 `team_info(action=task_result)`。
4. 目前實作仍依反向插入順序固定選到 B，而不是保證回傳最近完成的 A。

這會讓 auditor 或 downstream worker 取得錯誤 task 的 result/transcript。雖然新增
`task_id` 是正確方向，但相容的無 selector API 仍會被實際使用。

修正方向：無 selector 時依 `EndedAt` 或明確的 completion sequence 選擇最新完成
task；更好的做法是讓 caller 一律使用 stable task ID，並把 agent-only lookup
降級為明確的 ambiguous response。

### P1：typed result 與 durable Markdown 仍存在兩套結果來源

位置：`internal/team/coordinator_tool_teaminfo.go:227-273`

目前程式先從 in-memory `TodoList` 取 typed result；找不到符合項目後，仍會讀取
`workspace/tasks/.../*.md`，解析 `Status: done`、`## Task Description` 與
`## Result`，再以最多 8,000 字的 Markdown projection 回傳。

這代表以下情況仍可能回傳不同內容：

- typed result 已更新，但 Markdown 尚未發布或仍是舊摘要；
- process restart 後若 in-memory typed result 沒有被恢復，仍可退回 Markdown projection；
- result 含 verbatim transcript manifest，但 Markdown 只保留截斷後的模型文字。

因此 `task_result` 的結果不是真正單一權威來源；downstream auditor 仍可能拿到
摘要，而不是 sealed typed result/raw transcript manifest。這正是先前 evidence
handoff 問題的殘餘風險。

修正方向：以 task ID 查詢 sealed typed result／event-store record；Markdown 只作
人類閱讀用途。若 typed result 尚未 seal 或 persistence 不完整，應回傳明確的
`evidence_publication_failed`，不要靜默 fallback 成可能過時的 Markdown。

## 測試與驗證

已執行：

```text
go test ./internal/team -run 'TestTaskResultByIDSurvivesSessionReplayWithSealedManifest|TestReplayPreventionAcrossTasksAndRuns' -count=1
```

結果：PASS（`github.com/kjelly/hufu/internal/team`）。

未完成：

- `go test ./internal/team`
- `go test ./cmd/hufu`
- `go test ./internal/tools ./internal/agent ./internal/sidecar`

上述三個 broad test process 在審查環境長時間沒有輸出，為避免持續佔用執行資源已
停止；因此本報告不宣稱全 repo 測試通過。`git diff --check` 對本報告涉及的
Hufu runtime 檔案沒有發現 whitespace error。

## 建議修復順序

1. 先修復跨 process evidence key lifecycle，並加入真正的 restart/resume test。
2. 修正 artifact attempt provenance，讓 retry metadata 與 task execution context
   綁定。
3. 將 `task_result` 改為 task-ID-first，移除依賴 Markdown projection 的 runtime
   lookup fallback。
4. 為並行完成順序、selector ambiguity、Markdown 延遲與 transcript sealing 加入
   integration tests。

## 最終判定

**Hufu runtime 目前不可視為 evidence/restart-safe。** typed execution 的設計方向
是合理的，但上述 P1 問題尚未由跨 process、retry 與並行 worker 的完整 integration
test 證明已解決。
