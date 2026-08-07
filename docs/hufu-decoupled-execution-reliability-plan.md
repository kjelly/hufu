# Hufu 解偶合執行可靠性改善計畫

> 狀態：提案
> 範圍：任務語意、非同步執行、交付驗證、協議修復、狀態投影與完成判定
> 原則：production code 不得認識任何特定 CLI、外部系統、工作目錄、檔名、UI 文案或輸出格式。

## 1. 背景與問題定義

hufu 的工作單位可能啟動同步命令、長時間背景程序、互動式終端、MCP 呼叫或外部寫入。這些執行型態的共同需求是：系統必須能判斷「這一次執行」是否完成、是否產生可驗證交付物，以及在 agent 回報協議不完整時如何安全恢復。

目前部分決策仍由自然語言 task goal、工具輸出文字或散落的 status 檔案推導。這會讓模型措辭、過期產物、或一次中斷改變控制流程，並將下列本應獨立的問題混在一起：

- 任務實際執行是否成功。
- agent 是否正確提交結構化結果。
- 交付物是否由本輪執行產生。
- 非同步程序是否已結束及資源是否已回收。
- workspace 中顯示的狀態是否與 canonical task state 一致。
- run 是否達成使用者目標。

本計畫以通用型資料契約取代文字 heuristic。特定工具只能透過設定或測試 fixture 使用這些契約，不能被寫入 hufu 的核心流程。

## 2. 設計原則與非目標

### 原則

1. **結構優於推測**：執行語意由 `TaskDef` 的結構化欄位宣告，不能從 task 描述字串判斷。
2. **單一事實來源**：task/session event store 是 canonical state；YAML status、報表、TUI 都是可重建的 projection。
3. **副作用不可因協議失誤重播**：缺少 result 與執行失敗是不同 failure class。
4. **交付驗證必須有世代資訊**：檔案存在不等於是本次執行的產物。
5. **各工作包可獨立合併**：每一階段只新增明確介面，不能要求同一 PR 同時修改其他階段。
6. **向後相容**：未設定新欄位的 task 維持現有 shell/task 行為；strict profile 可逐步要求新欄位。

### 非目標

- 不新增任何特定工具、服務、腳本格式、檔名、畫面文字或 command 名稱的 special case。
- 不以 LLM 自由文字作為安全或完成判定的唯一依據。
- 不將 artifact verifier 綁定檔案；未來可實作 HTTP、資料庫或 MCP 型驗證器，但 core interface 不依賴它們。
- 不在本計畫中重寫 agent prompting、模型選擇或 TUI 視覺設計。

## 3. 目標架構

```text
TaskDef + ExecutionContract
          │
          ├── Task policy validation ─── 建立 task 前拒絕無效契約
          ├── Executor ──────────────── 回傳 ExecutionReceipt
          ├── Artifact verifier ─────── 驗證本次執行的交付物
          ├── Result protocol repair ── 僅補交結構化結果、禁止工具
          └── Event store ───────────── canonical lifecycle events
                                               │
                                               ├── Task/session state
                                               ├── status projection
                                               ├── TUI/report/JSON
                                               └── Run outcome evaluator
```

`Executor` 不決定 run outcome；`ArtifactVerifier` 不啟動程序；`StatusProjector` 不改寫 task lifecycle；`ResultProtocolRepairer` 不可呼叫具有副作用的工具。這些邊界是解偶合的核心。

## 4. 工作包 A：結構化 Execution Contract

### 目的

移除「由 task goal 文字推論執行風險、是否互動或是否需要驗證」的做法。

### 新介面

```go
type ExecutionKind string

const (
    ExecutionKindInline      ExecutionKind = "inline"
    ExecutionKindProcess     ExecutionKind = "process"
    ExecutionKindInteractive ExecutionKind = "interactive"
    ExecutionKindExternal    ExecutionKind = "external"
)

type ExecutionContract struct {
    Kind               ExecutionKind `json:"kind,omitempty" yaml:"kind,omitempty"`
    RequiresResult     bool          `json:"requires_result,omitempty" yaml:"requires-result,omitempty"`
    RequiresVerification bool        `json:"requires_verification,omitempty" yaml:"requires-verification,omitempty"`
    AllowsReplay       bool          `json:"allows_replay,omitempty" yaml:"allows-replay,omitempty"`
}
```

`TaskDef` 加入 `Execution ExecutionContract`。現有 `SideEffect` 和 `Recovery` 仍保留，分別回答「可能影響什麼」與「中斷後如何處理」；Execution Contract 只回答「如何執行及需要哪些完成證據」。

### 驗收

- task 建立時由純函式 `ValidateExecutionContract(TaskDef)` 驗證。
- `interactive` 或 `external` 且 `RequiresVerification=true` 時，缺少 verifier contract 必須在執行前拒絕。
- 驗證邏輯不得讀取 `Goal` 或 `Desc` 的內容。
- 所有既有 task 未設定 contract 時仍可執行，並使用相容預設值。

### 邊界

只修改 task schema、parser、validator 與單元測試；不修改 terminal manager、artifact 實作或 run outcome。

## 5. 工作包 B：通用 Execution Receipt 與 Artifact Verification

### 目的

讓「本輪是否完成」與「某個路徑曾存在」分離，避免過期產物被當成本輪成功。

### 新介面

```go
type ExecutionReceipt struct {
    RunID       string    `json:"run_id"`
    TaskID      string    `json:"task_id"`
    Attempt     int       `json:"attempt"`
    StartedAt   time.Time `json:"started_at"`
    FinishedAt  time.Time `json:"finished_at,omitempty"`
    ExitCode    *int      `json:"exit_code,omitempty"`
    ProducerID  string    `json:"producer_id,omitempty"`
}

type ArtifactExpectation struct {
    Name             string `json:"name" yaml:"name"`
    Locator          string `json:"locator" yaml:"locator"`
    MustBeFresh      bool   `json:"must_be_fresh,omitempty" yaml:"must-be-fresh,omitempty"`
    Required         bool   `json:"required,omitempty" yaml:"required,omitempty"`
    VerificationMode string `json:"verification_mode,omitempty" yaml:"verification-mode,omitempty"`
}

type ArtifactVerifier interface {
    Verify(ctx context.Context, receipt ExecutionReceipt, expectation ArtifactExpectation) VerificationResult
}
```

`Locator` 與 `VerificationMode` 是通用識別字串，由 registry dispatch；檔案型 verifier 只是第一個 adapter。`MustBeFresh` 至少比較 receipt 的開始時間與 artifact metadata；若 producer metadata 可用，應優先比對 run/task/attempt identity。

### 驗收

- 既有 artifact 在 receipt 開始前就存在時，freshness 驗證不得通過。
- 同一 task 的 retry 不能接受前一 attempt 的 artifact。
- verifier 回傳可序列化 `VerificationResult`，寫入 task state 與 event store。
- verifier 不可呼叫 agent 或建立 terminal session。

### 邊界

只新增 receipt、verifier registry、file adapter 與測試。Execution Contract 可先不要求使用它；後續由 profile 漸進啟用。

## 6. 工作包 C：Protocol-only Repair

### 目的

把「agent 漏交 typed result」從真實 execution failure 分離，避免重播已產生副作用的工作。

### 流程

1. executor 完成後寫入 immutable `ExecutionReceipt` 與 tool transcript reference。
2. 若 required result 缺失，task 進入 `protocol_incomplete`，不是 `error`。
3. 系統建立一個 **無工具、單步** repair prompt，只允許 `submit_result`。
4. repair 成功後執行原有 verifier；驗證成功才轉 `done`。
5. repair 失敗時：可重播任務才依 recovery policy retry；不可重播任務轉 `blocked` 或 `reconcile`。

### 驗收

- 缺 result 的 task 不會再次呼叫原 worker tool。
- repair prompt 的 allowed-tools 僅包含 `submit_result`。
- `ExecutionReceipt`、原 transcript 與 repair provenance 全部保留。
- non-replayable task 在 repair 失敗後不會被自動重跑。

### 邊界

只修改 task lifecycle 與 tool allowlist；不要求 Artifact verifier 的新介面。若 verifier 尚未導入，現有 `Verify` command 可作為 adapter。

## 7. 工作包 D：Terminal 與非同步程序生命週期

### 目的

把「程序已啟動」「程序已退出」「輸出已觀察」「資源已回收」分為不同事件，避免存在檔案或一次讀取被誤當成完成。

### 設計

terminal/session manager 只管理 process identity、owner、run、狀態與輸出 reference，並發出 lifecycle event：

```text
process_started → process_observed → process_exited → process_reconciled
```

等待行為由獨立 waiter 消費 `ExecutionReceipt` 或 terminal session ID；它必須指定等待目標，例如 `exit`、`artifact_verified` 或 `resource_released`。不得以任意 shell condition 自動宣告整個 task 完成。

### 驗收

- 一個已存在的輸出檔不會滿足「等待本次 process 結果」的條件。
- process 已退出但 task 尚未驗證時，task 狀態不可為 `done`。
- crash/restart 後，process identity 無法確認時進入 `unknown`，並經既有 recovery policy 處理。
- terminal manager 不讀取或理解任何特定 command 的語意。

### 邊界

只修改 terminal/session manager、waiter abstraction 與事件模型；不修改 agent task prompt 或 artifact verifier adapter。

## 8. 工作包 E：Canonical State 與 Status Projection

### 目的

避免 `status/<agent>.yml` 與 session task state 分歧。

### 設計

新增純函式：

```go
func ProjectAgentStatuses(items []*TodoItem, sessions []TerminalSession) map[string]AgentStatus
```

它以 todo state 與 terminal state 為輸入，計算 `idle`、`working`、`paused`、`error`，然後由單一 writer 原子更新 status files。所有散落的 `writeStatus` 呼叫保留為短期相容層，逐步改成 emit event。

### 驗收

- 正常結束、partial、cancel、crash resume 後皆可重建一致 status。
- status 重建不改變 TodoItem、terminal session 或 run result。
- 重建為 idempotent；連續執行兩次產生相同輸出。

### 邊界

僅新增 projection/reconciliation job 與測試；不改 task executor 或 finish policy。

## 9. 工作包 F：三態 Acceptance 與 Run Outcome

### 目的

避免「沒有設定 acceptance」被呈現為「acceptance passed」，同時保留既有 completed/partial 語意。

### 新模型

```go
type AcceptanceState string

const (
    AcceptanceNotConfigured AcceptanceState = "not_configured"
    AcceptancePassed        AcceptanceState = "passed"
    AcceptanceFailed        AcceptanceState = "failed"
)
```

`RunOutcomeEvaluator` 應為純函式，輸入 unresolved tasks、acceptance state、cancellation/budget state，輸出 `RunResult`。它是唯一能決定 `GoalSatisfied` 與 process exit code 的元件。

### 驗收

- `not_configured` 在 JSON、report、event store 中可辨識，不能序列化成 passed。
- 有 failed/pending task 時，一律不會得到 `completed`。
- evaluator 的 table-driven tests 覆蓋 completed、partial、blocked、failed、cancelled。
- finish tool 只收集輸入與呼叫 evaluator，不內嵌 outcome 判定規則。

### 邊界

僅修改 run-result/finish/output/report；不需要修改 terminal 或 task execution。

## 10. 推進順序與獨立合併條件

| 順序 | 工作包 | 可獨立合併條件 | 不依賴 |
|---:|---|---|---|
| 1 | A Execution Contract | schema 與 validator 完整、相容預設測試通過 | B–F |
| 2 | B Receipt + Artifact Verification | file adapter 與 freshness 測試通過 | C–F |
| 3 | C Protocol Repair | repair 無工具保證與 non-replay test 通過 | B、D–F |
| 4 | D Async Lifecycle | process/session state 可於重啟後重建 | A–C、E–F |
| 5 | E Status Projection | projection idempotence 與所有終止路徑測試通過 | A–D、F |
| 6 | F Acceptance/Outcome | evaluator table tests 與 CLI contract tests 通過 | A–E |

每個 PR 必須：

1. 僅引入一個工作包的新 public contract。
2. 對新 contract 提供 migration/compatibility 行為。
3. 以假 executor、假 clock、假 artifact verifier、假 terminal session 測試；不得以外部系統或特定 CLI 作為單元測試前提。
4. 不得新增 `strings.Contains(task.Goal, ...)` 類型的策略分支。

## 11. 回歸測試矩陣

| 情境 | 預期結果 |
|---|---|
| 過期 artifact 存在，新的 process 尚未結束 | 不可完成或驗證成功 |
| process 成功、agent 漏 result | `protocol_incomplete`，只進行無工具 repair |
| process 有外部副作用、repair 失敗 | blocked/reconcile，不可自動重播 |
| process crash 後 PID 無法辨識 | unknown，依 recovery policy 處理 |
| task done 但 agent status 檔仍為 working | projector 修正為 canonical 狀態 |
| acceptance 未設定、所有任務成功 | outcome 可 completed，但 acceptance 顯示 not_configured |
| acceptance passed、仍有未解任務 | outcome 必為 partial/blocked，goal_satisfied=false |

## 12. 成功標準

完成後，hufu 對所有執行器使用相同的可靠性語意：它能識別本次執行、區分協議失誤與執行失敗、避免重播副作用、從 canonical state 重建觀測狀態，並以可解釋的 run outcome 呈現結果。任何具體工具或專案只需提供 task contract 與 verifier adapter，不需要 hufu core 增加特例。
