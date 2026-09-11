# hufu 任務執行可靠性修正計畫

## 1. 背景與目標

本計畫依據 `/home/ubuntu/nfs/github/kvmforge/workspace/default` 的 2026-07-11 執行紀錄制定。

該次執行共建立 10 個 worker task，全部記錄為 `done`，但所有 task 的 `Verify` 皆為空；執行過程實際出現 sudo 權限阻擋、SSH timeout、QEMU guest agent 失敗、ping 100% packet loss，最終 coordinator 又因 `max rounds (10) exceeded` 進入 error。這代表目前的「worker 成功產生文字」與「使用者要求確實完成」沒有被可靠區分。

本計畫目標如下：

1. 任務狀態必須準確反映執行與驗證結果，禁止假完成。
2. 在消耗大量模型與工具時間前，先檢查執行能力與先決條件。
3. round、timeout 與 retry 必須有整體預算，並保留收尾與清理空間。
4. journal、session、report 與 LTM 必須能區分 run、task、錯誤與驗證證據。
5. cleanup 必須在成功、失敗、timeout、max-rounds 與取消等路徑可靠執行。
6. 保持既有 team/agent 設定相容，採漸進式啟用與可回滾 rollout。

## 2. 已確認問題

### 2.1 狀態與成功語意混淆

- Worker 能完整回報「命令失敗」時，task 仍可能標記為 `done`。
- `validateTaskOutput` 只檢查空輸出或未完成語氣，無法判定命令是否達成目標。
- `TaskDone` 是不可逆終態，錯誤地進入 `done` 後無法由後續 acceptance 修正。
- `finish` 可以在存在失敗或未驗證 task 時輸出 `FINISHED:`。

### 2.2 驗證機制存在但不是安全預設

- `TaskDef.Verify` 與 `verifyTaskDeliverable` 已存在，但 coordinator 可以對可客觀驗證的工作完全不提供 `verify`。
- 沒有記錄 verify 的開始、結束、exit code、stdout/stderr 與耗時。
- Task journal 的用途偏向 semantic cache，只保存 `op/agent/desc/output/round/ts`，不能作為可靠的執行稽核來源。

### 2.3 執行能力未提前驗證

- Agent 執行後才發現 sudo、libvirt、OVS、guest agent 或 SSH 不可用。
- Agent 的工具宣告只代表工具存在，不代表執行環境具備權限、服務與憑證。
- 權限被阻擋後，模型可能自行嘗試規避方式，而非回報明確的 capability failure。

### 2.4 預算與 timeout 放大

- `MaxRounds` 在進入下一 round 時才拒絕任務，沒有預留 finish/cleanup round。
- 多個 VM readiness check 串行等待，各自使用完整 timeout，造成單一 task 長時間佔用。
- Retry 主要由 worker/verify error 驅動；「成功產生失敗報告」不會進入 retry。
- 缺少同類 failure signature 的熔斷機制，容易重複執行相同診斷。

### 2.5 Workspace 與記憶污染

- 同一 workspace 中存在多份命名不同、結論互相矛盾的報告。
- Journal 沒有 `run_id` 與 `todo_id`，難以精確還原單次執行。
- LTM 可能把未經 acceptance 證實的推測保存為已知事實。
- LLM log 體積遠大於 session/journal，缺少明確 retention policy。

### 2.6 Cleanup 不是不可跳過階段

- Acceptance rollback 只處理 unattended acceptance failure，並非通用 finally。
- max-rounds、interactive failure、context cancellation 等情況沒有統一 cleanup stack。
- 外部環境變更如 VM、OVS bridge、系統設定可能在失敗後殘留。

## 3. 設計原則

1. **Fail closed**：無法取得必要證據時，狀態為 `blocked` 或 `error`，不是 `done`。
2. **完成與驗證分離**：worker execution、objective verification、run acceptance 分別記錄。
3. **結構化優先**：狀態判定依賴 exit code 與 typed result，不解析自然語言報告。
4. **先便宜後昂貴**：preflight 與快速 probe 先於 LLM retry 和長 timeout。
5. **Cleanup always runs**：資源清理由 coordinator 控制，不依賴模型記得執行。
6. **向後相容**：既有 `team.yaml` 和 agent `.md` 在未啟用 strict mode 時維持行為。
7. **可觀測且可重播**：每個 run、task、attempt、verify 與 cleanup 都有穩定 ID 和事件。

## 4. 目標狀態模型

### 4.1 TaskStatus

新增 `TaskBlocked`，完整狀態如下：

```text
pending -> planned -> in_progress -> verifying -> done
                              |          |
                              |          +-> error
                              +-> blocked
                              +-> error
                              +-> paused
pending/planned -> skipped
```

規則：

- `done`：worker 正常結束，且必要 verify 成功。
- `error`：執行、輸出驗證或 objective verify 失敗且 retry 已耗盡。
- `blocked`：缺少權限、工具、服務、憑證、人類輸入或外部依賴。
- `skipped`：使用者拒絕、dependency 失敗或策略明確略過。
- `paused`：取消或中斷，可在 resume 時重新驅動。
- `verifying`：worker 已交付結果，objective verify 尚未完成。

`done` 與 `skipped` 維持終態；`error`/`blocked` 只允許在明確 retry/resume 操作轉回 `in_progress`。

### 4.2 結構化結果

新增內部資料型別，不要求模型直接正確產出 JSON：

```go
type TaskAttemptResult struct {
    RunID         string
    TodoID        string
    Attempt       int
    Execution     ExecutionResult
    Verification *VerificationResult
    Failure       *TaskFailure
}

type ExecutionResult struct {
    Output       string
    StartedAt    time.Time
    EndedAt      time.Time
    ModelTime    time.Duration
    ToolTime     time.Duration
    LastTool     string
}

type VerificationResult struct {
    Command    string
    ExitCode   int
    Stdout     string
    Stderr     string
    Duration   time.Duration
    TimedOut   bool
}

type TaskFailure struct {
    Source    string
    Kind      string
    Retryable bool
    Message   string
    Signature string
}
```

對外的 coordinator tool response 改為每個 task 都包含 `todo_id/status/output/failure/verification`，避免只回傳摘要文字。

## 5. 分階段實作

## Phase 0：建立回歸測試基線

### 工作項目

1. 將本次事故抽象為 fixture，不直接依賴 kvmforge、libvirt 或 sudo。
2. 建立 fake worker，模擬以下情境：
   - 回傳「ping failed」文字但 agent API 無 error。
   - verify exit 1。
   - capability denied。
   - 兩個串行 timeout。
   - max-rounds 前仍有未終態 task。
3. 記錄現有行為測試，之後逐項反轉為期望行為。

### 涉及檔案

- `internal/team/coordinator_task_run_test.go` 或新增 `internal/team/task_outcome_test.go`
- `internal/team/improvements_test.go`
- `internal/team/dag_test.go`
- `internal/team/resume_test.go`
- `internal/team/unattended_test.go`

### 驗收條件

- 測試能重現「worker 回報失敗但 task done」。
- 測試能重現 max-rounds 後缺少可靠收尾狀態。
- 所有 fixture 都不需要外部 VM、sudo 或網路。

## Phase 1：修正 task outcome 與狀態機（P0）

### 1.1 新增狀態與 transition API

修改：

- `internal/team/status.go`
- `internal/team/todo_test.go`
- `internal/tui/tui.go`
- `internal/tui/*_test.go`
- `cmd/hufu/display.go`
- `cmd/hufu/report.go`

工作：

1. 新增 `TaskVerifying` 與 `TaskBlocked`。
2. 將散落的 transition 判斷集中為 `CanTransition(from, to TaskStatus) bool`。
3. `UpdateStatusAndOutput` 遇到非法 transition 時回傳 error，而不是靜默忽略。
4. 更新 TUI column 策略：短期可將 `verifying` 顯示在 in-progress、`blocked` 顯示在 error；若增加 column，必須同步更新 View/Update、resize、navigation 與測試。
5. `FinishedMsg` 不得把仍在執行或 paused 的 task 無條件改成 done；應依 coordinator 提供的終止原因轉成 paused/error/skipped。

### 1.2 統一 executeTask 成功路徑

修改：

- `internal/team/coordinator_task_run.go`
- `internal/team/coordinator_extra_models.go`
- `internal/team/dag_scheduler.go`
- `internal/team/coordinator_run.go`
- `internal/team/coordinator_tools_delegate.go`

工作：

1. 所有執行路徑回傳 `TaskAttemptResult` 或等價內部 outcome。
2. worker 完成後先進入 `verifying`，只有驗證成功才能進入 `done`。
3. cache hit 只能重用先前已通過相同 verify fingerprint 的結果。
4. extra-model/judge 路徑不得繞過 objective verify。
5. direct agent、delegate 與 resume 使用同一套完成判定。
6. `TaskDone` 更新集中到單一函式，禁止各檔案直接寫入。

### 1.3 明確處理「診斷任務」與「交付任務」

新增 `completion_policy`：

```yaml
completion-policy: report   # 成功產生調查報告即可完成
completion-policy: verify   # 必須有 verify 且通過
completion-policy: auto     # 預設；依 task 類型與 strict mode 決定
```

行為：

- `report` 適用於「讀取文件、列出現況、分析原因」。命令失敗可以是有效觀察，但輸出必須明確包含證據。
- `verify` 適用於「建立、修復、測試、部署、確認可用」。缺少 verify 時拒絕 delegation 或降為 blocked。
- `auto` 在 strict mode 下，含建立/修復/測試/部署語意且沒有 verify 時回傳 schema validation error，提示 coordinator 補上 verify。

為避免用脆弱關鍵字決定正確性，長期應由 coordinator 在 task tool call 明確指定 policy；關鍵字只用於 warning。

### 驗收條件

- verify 非零時，任何執行路徑都不能產生 `done`。
- capability failure 最終為 `blocked`，並包含可機器判斷的 failure kind。
- illegal state transition 會被測試捕捉並記錄。
- 舊設定在 strict mode 關閉時仍可執行。

## Phase 2：Verify 成為可稽核的一級流程（P0）

### 2.1 擴充 verify 執行結果

修改：

- `internal/team/coordinator_task_run.go`
- `internal/team/failure_source.go`
- `internal/team/status.go`
- `internal/team/improvements_test.go`

工作：

1. `verifyTaskDeliverable` 改回傳 `VerificationResult`。
2. 分離 stdout/stderr，保存 exit code、timeout、duration。
3. 增加 verify 專用 timeout，不能沿用過大的 agent timeout。
4. 發出 `verify_start`、`verify_done`、`verify_error` StatusEvent。
5. verify 命令、工作目錄、shell 與環境來源必須記錄，但敏感環境變數不得寫入 log。

### 2.2 Verify fingerprint 與 cache 安全

修改：

- `internal/team/coordinator_taskcache.go`
- `internal/team/task_journal.go`
- `internal/team/cache_test.go`
- `internal/team/task_journal_test.go`

fingerprint 至少包含：

- 正規化 task description
- agent/model identity
- verify command
- project/workspace identity
- relevant input file digest（若有 context files）
- completion policy

沒有 verify 或 fingerprint 不同時，不得把舊結果當成已驗證完成。

### 2.3 Coordinator prompt 與 schema 約束

修改：

- `internal/team/coordinator.go`
- `internal/team/coordinator_prompt.go`
- coordinator tool schema tests

工作：

1. 在 agent tool schema 說明何時 `verify` 必填。
2. Coordinator prompt 要求對客觀任務提供 verify，不接受「請 report all outputs」替代 acceptance。
3. 若 task 沒有 verify，tool result 回傳明確 warning，並在 strict mode 拒絕。

### 驗收條件

- session/report 能顯示每個 task 的 verify 命令、exit code、耗時與摘要。
- cache 不會重用 verify 不同或未驗證的結果。
- verify timeout 能被分類並依策略 retry。

## Phase 3：Capability preflight 與阻擋分類（P1）

### 3.1 新增通用 capability 模型

新增：

- `internal/team/capability.go`
- `internal/team/capability_test.go`

建議型別：

```go
type CapabilityRequirement struct {
    Name    string
    Probe   string
    Timeout time.Duration
    Scope   string
}

type CapabilityResult struct {
    Name      string
    Available bool
    Reason    string
    Evidence  string
}
```

支援來源：

- task 的 `requires` 欄位。
- agent 工具自動推導的基本 requirement。
- team-level `preflight` 設定。

範例：

```yaml
preflight:
  - name: passwordless-sudo
    probe: sudo -n true
    timeout: 5
  - name: libvirt
    probe: virsh list --all >/dev/null
    timeout: 10
```

### 3.2 執行時機

1. Team load 後執行 team preflight。
2. DAG 排程 task 前執行尚未檢查或已過 TTL 的 task requirements。
3. 同一 capability result 在單一 run 內快取，避免每個 task 重複 probe。
4. preflight failure 直接產生 `TaskBlocked`，不消耗 worker LLM call。

### 3.3 安全行為

- 不允許模型透過命令包裝規避 denied tool policy。
- sudo capability 只代表非互動 sudo 是否可用，不自動放寬 unattended allowlist。
- probe 命令需通過與 verify 相同的 shell/path/network policy。
- 缺少 capability 時發送 `needs_human` notification，附精簡修復指引。

### 涉及檔案

- `internal/team/parse.go`
- `internal/agent/agent.go`
- `internal/team/coordinator_execute.go`
- `internal/team/dag_scheduler.go`
- `internal/notify/notify.go`
- `cmd/hufu/doctor.go`（若現有 doctor 實作檔名不同則整合到對應檔案）

### 驗收條件

- sudo/libvirt 不可用時，在第一次 worker LLM call 前終止相關 task。
- 多個 task 共享 capability 時只執行一次 probe。
- blocked 原因會保存到 session、journal、TUI 與 report。

## Phase 4：Round、retry 與 timeout 預算治理（P1）

### 4.1 保留收尾預算

修改：

- `internal/team/coordinator_execute.go`
- `internal/team/coordinator_run.go`
- `internal/team/coordinator_prompt.go`

新增概念：

```yaml
max-rounds: 10
wrap-up-reserve-rounds: 2
```

規則：

- 當 `round >= max-rounds - reserve`，拒絕新的探索性 task。
- 仍允許 verify、cleanup、acceptance 與 finish。
- 進入 reserve 時向 coordinator 注入剩餘預算與未完成 task 清單。
- max-rounds 到達時，把未終態 task 轉為 paused/error，保存 checkpoint，再執行 finalizers。

### 4.2 整體 deadline

新增 task batch deadline，避免每個 probe 各自使用完整 timeout：

```yaml
timeout: 600
batch-timeout: 600
probe-timeout: 15
verify-timeout: 120
```

規則：

- child context deadline 不得晚於 run/batch deadline。
- 並行 readiness probe 共用整體 deadline。
- 每次 retry 前先檢查剩餘時間是否足夠完成最小一次 attempt。

### 4.3 Failure signature 熔斷

對 failure 建立 signature，例如：

```text
capability:sudo:permission-denied
network:ssh:timeout:<target-class>
verify:exit-1:<command-hash>
```

當同一 signature 在同一 run 連續出現達門檻：

- 停止相同策略的 retry。
- 將相依 task 標記 blocked/skipped。
- 要求 coordinator 改用另一診斷方法，或進入 wrap-up。

### 4.4 Retry policy

區分：

- retryable：暫時性網路、provider timeout、服務啟動等待。
- non-retryable：permission denied、command not found、schema error、缺少必要檔案且 agent 無權建立。
- human-required：sudo 密碼、憑證、破壞性操作批准。

### 驗收條件

- 兩個各 5 分鐘的 probe 在 batch timeout 5 分鐘時，不會串行耗時 10 分鐘。
- max-rounds 前一定有機會執行 cleanup 與 finish。
- 相同 non-retryable failure 不會重複呼叫 worker。

## Phase 5：通用 finalizer 與 cleanup stack（P1）

### 5.1 新增 finalizer 模型

不要把 cleanup 等同於 git rollback。新增 coordinator-owned finalizer stack：

```go
type Finalizer struct {
    ID        string
    Command   string
    Verify    string
    Timeout   time.Duration
    CreatedAt time.Time
    Status    string
}
```

需求：

- 資源建立成功後立即註冊對應 cleanup。
- LIFO 執行。
- 寫入 checkpoint，crash-resume 後仍可執行。
- command 必須 idempotent。
- 每個 finalizer 有自己的 verify 與結果。

### 5.2 觸發條件

Finalizer 在以下情況執行：

- 正常 finish。
- acceptance failure。
- max-rounds/budget exceeded。
- coordinator error。
- context cancellation/Ctrl+C wrap-up。
- resume 發現前次 run 尚有 pending finalizer。

強制退出可略過 finalizer，但必須在 session/report 標記 `cleanup_incomplete`。

### 5.3 與既有 rollback 的關係

- `rollback` 保留為 unattended acceptance failure 的 project-level recovery。
- `finalizer` 用於外部資源與系統狀態清理。
- 執行順序：停止新增 task -> task cancellation -> finalizers -> acceptance/rollback policy -> finish report。
- 是否先 acceptance 再 cleanup 需依 team 設定；預設 acceptance 在 cleanup 前，確保測試環境仍存在。

### 涉及檔案

- 新增 `internal/team/finalizer.go`
- `internal/team/coordinator.go`
- `internal/team/coordinator_run.go`
- `internal/team/coordinator_session.go`
- `internal/team/coordinator_tools.go`
- `internal/team/session.go` 或 session data 定義檔
- 對應 tests

### 驗收條件

- 模擬 max-rounds、timeout、verify failure 與 Ctrl+C，finalizer 都會執行。
- crash 後 resume 能重新執行未完成 finalizer。
- finalizer failure 不覆蓋原始 failure，兩者都保存在報告中。

## Phase 6：Journal、Session 與事件 schema 升級（P1）

### 6.1 引入 run identity

每次新 run 建立 UUID/ULID `run_id`，所有資料包含：

- `run_id`
- `team`
- `todo_id`
- `attempt`
- `event_id`
- `timestamp`

### 6.2 Journal v2

目前 task journal 同時承擔 cache 與事故紀錄，應拆分：

1. `task_cache.jsonl`：只保存可重用且已驗證的結果。
2. `runs/<run_id>/events.jsonl`：完整 append-only lifecycle event。

事件範例：

```json
{
  "schema_version": 2,
  "run_id": "...",
  "event": "task_verify_failed",
  "todo_id": "9",
  "attempt": 1,
  "status": "error",
  "failure_kind": "ssh_timeout",
  "exit_code": 3,
  "duration_ms": 300012
}
```

### 6.3 Migration

- Reader 同時支援 journal v1 與 v2。
- v1 `put` 可載入為 legacy unverified cache，但 strict mode 不可直接命中為 done。
- Writer 只寫 v2。
- 提供一次性 compact/migrate 函式與測試。

### 6.4 Session schema

`TodoItem` 增加：

- `CompletionPolicy`
- `Attempt`
- `FailureKind`
- `FailureSource`
- `VerifyResult`
- `BlockedReason`
- `RunID`

新增 `schema_version`，避免未來無法安全演進。

### 驗收條件

- 可由 events.jsonl 精確重建 task 最終狀態。
- error event 不會被 cache loader 當作成功結果。
- 舊 session/journal 可讀，新的資料不丟失 verify/failure 資訊。

## Phase 7：Run-scoped workspace、報告與記憶治理（P2）

### 7.1 目錄布局

建議：

```text
workspace/default/
  session.json
  runs/
    <run_id>/
      events.jsonl
      report.md
      artifacts/
      logs/
  latest -> runs/<run_id>
  cache/
    task_cache.jsonl
  memory/
    stm.md
    ltm.md
```

若跨平台 symlink 不合適，可用 `latest.json` 保存最新 run metadata。

### 7.2 報告生成

Report 必須區分：

- `PASS`：全部 required acceptance 通過。
- `FAIL`：已執行且 objective check 失敗。
- `BLOCKED`：缺少能力或外部依賴，無法完成檢驗。
- `INCOMPLETE`：被取消、預算耗盡或 cleanup 未完成。

報告不得把「VM running」呈現為整體驗證成功，也不得用 checkbox emoji 取代 typed status。

### 7.3 LTM 寫入門檻

修改 `AutoExtractLTM`：

- acceptance 通過的結論可寫入 `verified_fact`。
- 未通過但有證據的觀察寫入 `observation`，帶 run_id 和有效期。
- 模型推測寫入 `hypothesis`，不可在後續 prompt 中呈現為已知事實。
- 新 run 與舊 observation 衝突時，不覆寫；建立 supersedes 關係。

### 7.4 Log retention

新增設定：

```yaml
log-retention-days: 14
log-max-run-mb: 50
log-max-total-mb: 500
```

- run 結束後壓縮原始 LLM log。
- 保留 structured events 與 report。
- 清理不得刪除目前 active run。
- 日誌中的 API key、token、密碼必須經 redaction。

### 涉及檔案

- `cmd/hufu/report.go`
- `internal/team/session.go`
- `internal/team/session_mgmt.go` 或相關 session 管理檔
- `internal/team/coordinator_session.go`
- LTM extraction 相關檔案
- log package 與 tests

### 驗收條件

- 同一天多次執行不會產生互相覆蓋或無法判斷新舊的報告。
- 最新報告可以追溯到完整 run events。
- 未通過 acceptance 的假設不會成為 verified LTM fact。

## Phase 8：CLI、設定與操作介面（P2）

### 新增設定

建議 team.yaml：

```yaml
strict-completion: false
require-verify: auto
wrap-up-reserve-rounds: 2
batch-timeout: 0
probe-timeout: 15
verify-timeout: 120
failure-circuit-breaker: 2
cleanup-on-exit: true
preflight: []
```

建議 CLI flags：

- `--strict-completion`
- `--verify-timeout`
- `--probe-timeout`
- `--wrap-up-reserve-rounds`
- `--no-cleanup`，僅在明確要求保留測試環境時使用
- `--run-id`，主要供 resume/automation 定位

### Doctor 擴充

`hufu doctor` 增加：

- team preflight 結果。
- agent tool 與 capability mismatch。
- strict mode 下缺少 acceptance/verify 的警告。
- workspace schema 與 journal migration 狀態。
- pending finalizer 警告。

### TUI/文字輸出

- 顯示 `VERIFYING`、`BLOCKED`、`CLEANING_UP`。
- Status bar 顯示 round、剩餘 rounds、wall-clock/token budget。
- Detail view 顯示 attempt 與 verify evidence。
- 不改變既有 overlay priority；新增狀態只進入既有資料流。

### 驗收條件

- `hufu list/doctor/dry-run` 不產生 workspace side effect。
- 新 flags 遵守「CLI > profile > team.yaml > default」優先順序。
- completion 與 shell completion 文件同步更新。

## 6. 測試策略

### 6.1 Unit tests

必測：

- 所有合法與非法 TaskStatus transition。
- verify success、exit failure、timeout、context cancellation。
- completion policy validation。
- failure classification 與 signature。
- round reserve 計算。
- batch deadline 與 retry remaining-time 判斷。
- finalizer LIFO、idempotency、resume。
- journal v1/v2 migration、corrupt line、partial write、compaction。
- LTM fact/observation/hypothesis 分類。

### 6.2 Integration tests

使用 fake shell script 與 fake provider：

1. Worker 回傳成功文字，verify exit 1，期望 task error。
2. Preflight permission denied，期望零次 worker model call。
3. 兩個 timeout probe，期望總耗時受 batch deadline 限制。
4. max-rounds 進入 reserve，期望拒絕探索 task但允許 cleanup/finish。
5. 執行中 crash，resume 後 task 與 finalizer 正確恢復。
6. acceptance failure，同時保留 acceptance、rollback、cleanup 三種結果。
7. cache 中有舊未驗證結果，strict mode 不得命中為 done。

### 6.3 TUI tests

- `TasksUpdatedMsg` 顯示 verifying/blocked。
- `FinishedMsg` 不把失敗或 paused task 改為 done。
- Window resize、detail view、search、report 保持一致。
- 新狀態的鍵盤操作使用 `tea.KeyMsg` 測試。

### 6.4 End-to-end smoke test

建立不需要 root 的模擬驗證 team：

- 建立暫存資源。
- 一個 readiness probe 失敗。
- verify 失敗後 retry 成功。
- 無論成功失敗都移除暫存資源。
- 最終 report 與 events.jsonl 可重建一致結果。

## 7. 相容性與 migration

### 相容策略

- 第一版 `strict-completion` 預設 `false`，但對缺少 verify 發出 warning。
- 一個 minor release 後，built-in default team 對副作用任務預設 strict。
- 再下一個 major release 才考慮全域預設 strict。
- `TaskBlocked` 對舊 UI/report reader 可降級映射為 error。
- Journal v1 至少保留兩個 release 的唯讀支援。

### 風險

1. 強制 verify 可能增加 coordinator tool schema error 與 rounds。
2. 狀態增加會影響 TUI 固定六欄假設。
3. Finalizer 執行外部命令具有安全風險，必須套用現有 tool policy。
4. Run-scoped workspace 可能影響使用者既有腳本路徑。
5. Cache fingerprint 更嚴格會降低 cache hit，但可避免錯誤重用。

### 緩解

- feature flag 漸進啟用。
- blocked/verifying 先映射到既有欄位，不立即增加 TUI column。
- 提供 `latest.json` 與舊路徑相容層。
- 比較 rollout 前後的成功率、耗時、token 與 cache hit。

## 8. 可觀測性指標

每次 run 應產生以下 metrics：

- `tasks_total{status}`
- `tasks_unverified_total`
- `task_attempts_total{failure_kind}`
- `verify_duration_seconds`
- `preflight_failures_total{capability}`
- `rounds_used` / `rounds_remaining_at_finish`
- `worker_model_calls_avoided_total`
- `failure_signature_repeats_total`
- `cleanup_total{status}`
- `run_duration_seconds`
- `model_time_seconds`
- `tool_time_seconds`
- `cache_hits_total{verified}`
- `journal_replay_errors_total`

初期不必導入外部 metrics backend，可先寫入 run summary JSON 與 report。

## 9. Definition of Done

本計畫完成需同時滿足：

1. 任一 required verify 失敗時，task 和 run 不可能顯示 PASS/done。
2. 缺少 sudo/libvirt 等必要 capability 時，不呼叫 worker model 即能回報 blocked。
3. max-rounds/budget/timeout/cancel 都會保存 checkpoint 並執行 finalizer。
4. 每個完成 task 都能追溯 execution attempt 與 verification evidence。
5. Journal error 不會被 cache 或 session 恢復成成功結果。
6. 同一 workspace 多次 run 的報告與 artifact 不互相污染。
7. LTM 能區分 verified fact、observation 與 hypothesis。
8. `go test ./...`、`go vet ./...`、race-sensitive team tests 全部通過。
9. 新舊 team.yaml、session 與 journal 相容性測試通過。
10. 使用本次 kvmforge 類型的 fixture 重跑時，結果應是 `BLOCKED` 或 `FAIL`，不得是 10/10 done。

## 10. 建議交付切分

### PR 1：狀態正確性

- TaskVerifying/TaskBlocked。
- transition API。
- 統一 task outcome。
- verify evidence。
- 回歸測試。

### PR 2：Preflight 與 failure classification

- capability schema/probe/cache。
- blocked notification。
- failure signature 與 retryability。
- doctor 整合。

### PR 3：預算治理與 finalizer

- wrap-up reserve。
- batch/probe/verify deadline。
- circuit breaker。
- persistent cleanup stack。

### PR 4：Journal v2 與 run workspace

- run_id/events schema。
- cache/event journal 拆分。
- migration。
- run-scoped report/artifacts。

### PR 5：記憶、retention 與 UX

- LTM evidence levels。
- log retention/redaction。
- TUI/report/CLI 完整呈現。
- metrics summary。

每個 PR 必須可獨立測試與回滾；不得在同一 PR 同時重寫狀態機、workspace layout 與 TUI layout。

## 11. 建議優先順序與預估工作量

| 優先級 | 項目 | 預估 | 依賴 |
|---|---|---:|---|
| P0 | Phase 0 回歸 fixture | 1–2 天 | 無 |
| P0 | Phase 1 狀態與 outcome | 3–5 天 | Phase 0 |
| P0 | Phase 2 Verify 稽核化 | 2–4 天 | Phase 1 |
| P1 | Phase 3 Capability preflight | 3–5 天 | Phase 1 |
| P1 | Phase 4 預算與熔斷 | 3–5 天 | Phase 1、3 |
| P1 | Phase 5 Finalizer | 4–6 天 | Phase 1、4 |
| P1 | Phase 6 Journal/session v2 | 4–6 天 | Phase 1、2 |
| P2 | Phase 7 Workspace/LTM/log | 4–7 天 | Phase 6 |
| P2 | Phase 8 CLI/TUI/doctor | 3–5 天 | 前述 phases |

單人完整交付約 4–7 週；若先處理最關鍵的 P0，約 1–2 週即可消除「失敗卻顯示 done」的主要風險。
