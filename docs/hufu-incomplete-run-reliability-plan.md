# Hufu 未完成任務可靠性改善計畫

> 狀態：提案
> 範圍：完成狀態語意、run-level acceptance、step-limit continuation、typed result、retry/reconcile、context 壓縮
> 相關文件：`docs/hufu-strict-verification-workflow-improvement.md`、`docs/hufu-future-improvement-roadmap.md`

## 1. 問題摘要

hufu 目前可能把兩種不同結果混在一起：

1. 使用者要求已完整達成。
2. coordinator 已無法或不打算繼續，並揭露未完成項目。

兩者最後都會呈現為 `FINISHED`，使人類、CI、cron 或上層 orchestrator 容易誤判。

## 2. 改善目標

完成本計畫後，hufu 應滿足：

1. 未通過 run-level acceptance 的 run 絕不能回報 `completed`。
2. `acknowledge_failed_tasks` 只能產生 `partial`，不能產生成功語意。
3. coordinator step budget 用完時可自動 checkpoint、壓縮 context 並續跑。
4. worker 只因漏交 typed result 時，不可重做已有副作用的工具操作。
5. 已由後續任務取代或修復的失敗 task 可被 reconcile，不再永久污染 finish gate。
6. task 成功與 user goal 達成必須分開計算。
7. CLI、JSON、event store、session 與通知對 run outcome 使用一致語意。

### 非目標

- 不為特定工具、skill、任務名稱或產物格式內建完成判定與 verification adapter。
- 不從自然語言 prompt 猜測 required artifact、acceptance command 或目標是否完成。
- 不以本次或任何單一 incident 的工作目錄、檔名、外部系統流程作為核心機制。

## 3. 建議的 Run Outcome 模型

新增明確的 run-level outcome：

```go
type RunOutcome string

const (
    RunOutcomeCompleted RunOutcome = "completed"
    RunOutcomePartial   RunOutcome = "partial"
    RunOutcomeBlocked   RunOutcome = "blocked"
    RunOutcomeFailed    RunOutcome = "failed"
    RunOutcomeCancelled RunOutcome = "cancelled"
)

type RunResult struct {
    Outcome          RunOutcome        `json:"outcome"`
    GoalSatisfied    bool              `json:"goal_satisfied"`
    Response         string            `json:"response"`
    Acceptance       *AcceptanceResult `json:"acceptance,omitempty"`
    UnresolvedTasks  []TaskReference   `json:"unresolved_tasks,omitempty"`
    Continuation     *ContinuationInfo `json:"continuation,omitempty"`
}
```

語意：

| Outcome | Goal satisfied | Acceptance | CLI exit |
|---|---:|---|---:|
| `completed` | true | pass 或明確不需要 | 0 |
| `partial` | false | fail／missing／not run | 非 0 |
| `blocked` | false | not run 或 blocked | 非 0 |
| `failed` | false | fail | 非 0 |
| `cancelled` | false | not run | 130 或既有取消碼 |

`finish(acknowledge_failed_tasks=true)` 最多只能產生 `partial` 或 `blocked`。

### 驗收條件

- JSON output 可穩定區分 `completed` 與 `partial`。
- text output 不再對 partial 使用單獨的 `FINISHED:` 前綴。
- partial run 的 process exit code 非 0。
- event store、session、notification 與 report 使用相同 outcome。

## 4. P0：強化 Finish Gate

目前 `finishTool.Run` 在 default profile 下，只要
`acknowledge_failed_tasks=true` 就容許結束。建議修改為：

1. 檢查 unresolved tasks。
2. 執行或讀取 run-level acceptance。
3. 依 finish gate 的客觀結果設定 goal satisfaction。
4. 建立 `RunResult`，不再只回傳文字。
5. 只有 `GoalSatisfied && AcceptancePassed` 才能回傳 `completed`。

建議完成規則：

```text
failed tasks == 0
AND no pending/planned/in_progress tasks
AND acceptance passes
= completed
```

若 coordinator 設定 `acknowledge_failed_tasks=true`：

```text
return partial result
append unresolved task manifest
do not mark goal satisfied
do not return success exit code
```

### 建議實作位置

- `internal/team/coordinator_tools.go`
- coordinator run-result state
- `cmd/hufu` final output 與 exit handling
- notification payload
- session/event persistence

### 必要測試

- unresolved task + `acknowledge_failed_tasks=false`：finish 被拒絕。
- unresolved task + `acknowledge_failed_tasks=true`：得到 `partial`。
- acceptance fail + 無 failed task：不能得到 `completed`。
- pending task 存在：不能得到 `completed`。
- strict 與 default profile 都不得把 partial 輸出成 completed。

## 5. P0：Run-Level Acceptance Contract

task-level `verify` 只能證明單一 task；它不能證明原始使用者目標已達成。

應在每次 run 建立 acceptance contract。來源僅限明確、可重現的宣告：

1. team YAML 的 `acceptance` 設定。
2. 由呼叫端以結構化 run 設定明確提供的 acceptance。

acceptance 應以命令的退出狀態與輸出作為證據；檔案、服務或其他交付物的
檢查應包含在該命令中，而不是由 hufu 對特定工具、檔名或 prompt 文字作推測。

Acceptance contract 在 run 開始後應固定。若呼叫端明確要求變更，必須建立新的
contract revision，並在 event/session 中保留舊值、新值與變更來源；coordinator
不得自行降低 acceptance 標準。

### 必要測試

- acceptance 所驗證的交付條件不成立：run 仍為 failed/partial。
- acceptance command 未執行：不得推定 pass。
- acceptance 定義被變更：event store 記錄 revision 與變更來源。

## 6. P0：Coordinator Step-Limit 自動續跑

目前 coordinator 預設只有 20 steps。模型接近上限時容易：

- 呼叫 finish 要求使用者輸入 `continue`。
- 在尚未驗證修正前中止。
- 將執行框架限制暴露給使用者。

建議新增 coordinator continuation loop：

```text
run coordinator turn
  ├─ completed/partial/blocked/cancelled → return
  ├─ budget exceeded → forced wrap-up
  └─ step limit reached
       ├─ checkpoint session/tasks
       ├─ compact coordinator context
       ├─ emit coordinator_continuation event
       └─ start next coordinator turn automatically
```

必須另外設定 run-level 防護：

- `max-coordinator-turns`
- `max-duration`
- `max-total-tokens`
- cancellation propagation

step limit 是單次模型呼叫限制，不應被視為 user-visible blocker。

### Continuation context 最小集合

- 原始 user goal。
- 固定後的 acceptance contract。
- pending/in-progress/error task graph。
- 最近一次有效 tool/worker result。
- unresolved blockers。
- STM 中的決策與必要 artifact 路徑。
- 已完成 task 的摘要與 transcript reference，不攜帶完整 transcript。

### 必要測試

- 以極低 coordinator max steps 模擬跨 turn 完成。
- continuation 不建立重複 task。
- cancellation 可停止 continuation loop。
- duration/token budget 可終止 loop 並回傳 partial。
- checkpoint 後程序重啟可繼續同一 run。

## 7. P1：Typed Result Protocol Error 與執行失敗分流

目前 strict task 在 worker 未呼叫 `submit_result` 時，即使工具已成功執行，
仍會得到：

```text
typed task result mandatory ..., but agent did not submit structured result
```

若自動 retry，可能重做 workspace、external 或 infrastructure side effects。

建議分類：

```go
type TaskFailureClass string

const (
    FailureExecution TaskFailureClass = "execution"
    FailureProtocol  TaskFailureClass = "protocol"
    FailureVerify    TaskFailureClass = "verification"
    FailurePolicy    TaskFailureClass = "policy"
    FailureTimeout   TaskFailureClass = "timeout"
)
```

對 `FailureProtocol`：

1. 保留原 tool transcript 與 side-effect evidence。
2. 不重新執行 worker tools。
3. 要求原 worker進行 result-only repair，或由 deterministic parser 建立
   低信心 typed result。
4. 再執行 objective verify。
5. verify 通過後可標為 done，但需記錄 typed result 是 recovered。

對 `external_write`、`infra_mutation`、`credential_mutation`，禁止因純 protocol
error 直接 retry。

### 必要測試

- bash/write 已成功但漏 submit_result：工具只執行一次。
- recovered result 通過 verify：task done 且記錄 provenance。
- recovered result 無法確認：task 進入 reconcile/blocked，不可盲目 retry。
- read-only task 可依 policy 選擇安全重跑。

## 8. P1：Task Supersede 與 Reconcile

長 run 中，早期失敗往往已被後續任務修復或取代。若舊 task 永久維持
`error`，finish gate 與統計都會失真。

建議新增狀態或 resolution metadata：

```go
type TaskResolution struct {
    Status     string // unresolved, superseded, reconciled, waived
    ResolvedBy string // task ID
    Reason     string
    Evidence   []EvidenceRef
}
```

規則：

- `superseded`：後續 task 明確取代舊方法，不代表舊工作成功。
- `reconciled`：客觀檢查證明所需狀態已達成。
- `waived`：只能由 policy/user authorization 使用，不應由模型自行靜默設定。
- resolved error task 保留歷史，但不計入 unresolved finish gate。

### 必要測試

- task B verify 通過並 resolve task A：A 不再出現在 unresolved。
- 只有文字聲稱「已修復」：不可 reconcile。
- resolution cycle 或不存在的 task ID 被拒絕。
- report 同時呈現歷史失敗與目前 unresolved 數量。

## 9. P2：Context 壓縮與成本控制

建議活躍 coordinator context 只包含：

- goal + acceptance。
- active task graph。
- 每個 completed task 的 typed summary。
- unresolved error 的最新原因。
- artifact/evidence reference。
- 最近 N 個 coordinator decisions。

以下內容移至按需讀取：

- 完整 worker transcript。
- 已 supersede error 的詳細輸出。
- 重複 tool-call schema 與歷史。
- 舊 retry 的完整 message。

加入 observability：

```text
coordinator_context_bytes
coordinator_context_tokens
context_compaction_count
task_retry_count_by_failure_class
run_outcome
goal_satisfied
acceptance_status
```

usage 缺失不可寫成 0 並混入總計；應標記 `unknown` 或 `incomplete`。

## 10. 統計與報告一致性

統計應明確區分：

- logical tasks。
- attempts。
- historical failures。
- currently unresolved tasks。
- resolved/superseded failures。

建議 report：

```json
{
  "tasks_total": 26,
  "tasks_done": 17,
  "tasks_unresolved": 9,
  "attempts_total": 40,
  "attempts_failed": 23
}
```

所有摘要必須從同一個 canonical aggregation function 產生，避免模型自行計數。

## 11. 建議實作順序

### Phase 1：完成語意（P0）

1. 新增 `RunOutcome`／`RunResult`。
2. 修改 finish gate。
3. JSON/text/exit code 支援 partial。
4. event/session/notification 一致化。
5. 加入 finish outcome tests。

### Phase 2：Acceptance（P0）

1. 建立 immutable run acceptance contract。
2. finish 前執行 acceptance。
3. acceptance 與 unresolved task gate。
4. acceptance provenance/event tests。

### Phase 3：自動 continuation（P0）

1. 偵測 coordinator step-limit termination。
2. checkpoint + compact。
3. 自動啟動下一 turn。
4. run-level duration/token/turn guards。

### Phase 4：安全重試（P1）

1. failure classification。
2. protocol-only result recovery。
3. side-effect-aware retry。
4. supersede/reconcile model。

### Phase 5：效率與可觀測性（P2）

1. active-context projection。
2. transcript reference loading。
3. canonical run statistics。
4. context/token/outcome metrics。

## 12. Definition of Done

本改善計畫完成的最低標準：

- [ ] acceptance 未通過或未執行時，hufu 不可能回傳 `completed`。
- [ ] `acknowledge_failed_tasks=true` 會回傳 `partial` 與非零 exit code。
- [ ] coordinator step limit 可自動跨 turn 繼續。
- [ ] protocol-only typed-result failure不會重做副作用。
- [ ] resolved/superseded task 不再列入 unresolved。
- [ ] acceptance、goal satisfaction 與 task completion 分開呈現。
- [ ] text、JSON、session、event store、report、notification outcome 一致。
- [ ] logical task 與 attempt 統計不再混用。
- [ ] context usage 有上限、壓縮事件與可查 metrics。
- [ ] 新行為均有 unit、integration 及 resume/recovery tests。

## 13. 通用回歸案例

應以與工具、檔名及工作目錄無關的 integration fixture 覆蓋：

1. 有未解決 task 時，`finish(acknowledge_failed_tasks=true)` 回傳 `partial`，
   並以非零 exit code 結束。
2. 所有 task 均完成但 acceptance 失敗時，不得回傳 `completed`。
3. coordinator 在單次 step limit 用盡後，會在仍有 run-level budget 時 checkpoint、
   壓縮 context 並繼續執行。
4. 已有副作用證據的 protocol failure 不會重跑工具；verify 通過後可恢復 task。
5. 後續 task 以客觀證據 reconcile 早期失敗後，歷史失敗仍可查，但不列入
   unresolved finish gate。
