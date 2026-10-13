# hufu Chat-first TUI：P0～P3 實作規格（可執行修訂版）

> Status: implemented — P0、PM、P1、P2、P3 已在 branch `feat/chat-first-tui` 完成，每個 Phase 一個 commit
> Authority: reference — code + tests 優先；穩定契約以 `docs/architecture/chat-tui.md` 為準
> Verified-Commit: `f077b622`（§1 的所有程式位置都以此版本查核過）
> Superseded-By: [`docs/architecture/chat-tui.md`](../../architecture/chat-tui.md)

> **文件類型**：Coding Agent 可直接執行的實作規格／工作拆解／驗收契約
> **專案**：`kjelly/hufu`（Go module `github.com/kjelly/hufu`）
> **範圍**：P0（Controller 與每輪生命週期）、PM（Bubble Tea v2 遷移）、P1（Chat 介面）、P2（任務工作台與 ask_user）、P3（歷史還原與命令）
> **順序**：P0 → PM → P1 → P2 → P3。PM 是使用者在 P0 完成後追加的階段，P1 起的 TUI 程式一律以 v2 API 撰寫。
> **文件位置**：`docs/archive/implementation-plans/chat-first-tui-p0-p3.md`（由 `docs/tmp/` 移入並納入版本控制）。實作完成後的穩定架構說明寫到 `docs/architecture/chat-tui.md`（WP-32）。
> **規格優先順序**：既有 Runtime invariants／安全與恢復契約 > `AGENTS.md`／`CLAUDE.md`／`internal/tui/CONSTITUTION.md` > 本規格 > UI 細節。

---

## 0. Coding Agent 執行規則（先讀）

1. 開工前先讀 `AGENTS.md`、`CLAUDE.md`、`internal/tui/CONSTITUTION.md`、`docs/architecture/execution-runtime.md`。逐條核對 §1 的程式位置在當前 HEAD 是否仍成立；若不成立，**先修改本文件 §1 與受影響的工作包**，再開始實作。
2. 在本地 branch `feat/chat-first-tui` 上實作。**每個 Phase（P0／PM／P1／P2／P3）完成並通過 Gate 後 commit 一次**。**不得 push、不得開 PR、不得 merge**。
3. 每個 Phase 的 Gate 全部通過後，以 §10 的格式記錄結果（寫在 commit message body），接著繼續下一個 Phase；全部 Phase 完成後再一併回報使用者。
4. 每個 WP 的流程：先寫會失敗的測試 → 做最小修改 → 該 package 測試與 `go vet ./...` 通過 → 進行下一個 WP（Phase 結束時才 commit）。
5. **所有驗收都是自動化測試**（unit、table-driven、`teatest`、`creack/pty` E2E 搭配假 provider）。本規格沒有任何人工驗收步驟；無法自動化驗證的條件不是需求。
6. 不改變 Coordinator 的驗證、授權、Recovery、BackendBinding、ExecutionTarget、EventStore 語意。本規格**不新增任何 canonical event type**，也不改 `internal/team` 的 production code（WP-21 只允許在 `internal/team` 新增測試）。
7. 最終回答與結果只能來自 `Coordinator.Run`／`ContinueWithPrompt` 的回傳值與 `Coordinator.LastRunResult()`；**不得**從 `StatusEvent.Message`、Tool Log 或模型文字推斷成功、驗證或完成。
8. 新增的 Bubble Tea 程式碼中，`Update()` 必須是純函式：不得做 channel 操作、I/O 或全域狀態修改；副作用一律放在回傳的 `tea.Cmd` 裡。既有 `ask_user.go` 的 ReplyCh 行為原樣重用，不在本計畫重構。
9. 新檔案 < 800 行；不得新增 `.golangci.yml` 的 exclusion。修改 `runChat` 時要降低它的複雜度，目標是移除 `cmd/hufu/chat\.go` 的 gocyclo exclusion（WP-01／WP-10）。
10. 錯誤一律以 `fmt.Errorf("doing X: %w", err)` 包裝；測試使用 table-driven 寫法。
11. 不新增 team-specific 旗標，也不硬編碼 team 名稱。
12. Phase 驗收命令：`go build ./cmd/hufu`、`go test ./...`、`go test -race ./internal/conversation/... ./internal/tui/... ./cmd/hufu/...`、`go vet ./...`、`golangci-lint run`。全部通過才算 Gate 通過。

### Non-goals（本計畫明確不做；不得自行加回）

| 不做的項目 | 原因 |
|---|---|
| Web UI、App Server、RPC／socket、多 process ownership | 留給 P4 |
| durable 的 command admission event（`conversation_turn_admitted`）、跨 crash 的 `command_id` 去重 | 單一 process 內沒有會重送同一個 `command_id` 的 client；成本是改動 EventStore 的 type、reducer、replay、hash chain |
| Event cursor／`resync_required`／原子 snapshot＋subscribe 協定 | 這是遠端重連 client 才需要的；process 內只有一個訂閱者 |
| 最終回答逐 token 串流（`assistant_answer_delta`） | 需要修改 Coordinator 產生回答的邊界。採用「即時進度＋完整最終回答」 |
| Markdown 渲染（glamour） | 已決議只顯示純文字；除了 PM 的 Charm v2 套件以外不新增依賴 |
| `chat_*` metrics | repo 沒有 metrics 框架 |
| `/model`、`/diff`、`/compact`、`/history`、`/sessions`、在 UI 內 checkout branch | 凍結的 policy 會讓 `/model` 幾乎都被拒絕；沒有「本輪 diff」來源；`/compact` 沒有安全的入口；branch 操作和 `hufu session` CLI 重疊 |
| `--continue`、`--workspace-exact` 旗標 | `hufu chat` 預設已經會還原既有 session（`resumeOrStartSession`）；`--workspace` 沿用現有語意 |
| 改變 `hufu run`、`hufu chat`（plain）、`hufu chat --tui` 的既有行為 | 唯一例外是 WP-00 的 silent drop 修正 |
| 淘汰既有 dashboard 的 chat 模式 | 需要產品決策，不在本計畫範圍 |
| 人工驗收、真實 provider smoke test、實體終端測試 | 已決議只做自動化測試 |

---

## 1. 現況（以 `f077b622` 查核）

| 位置 | 事實 | 影響 |
|---|---|---|
| `cmd/hufu/signal.go:109-114` | `promptInjector.enqueue` 用 `select { case ch <- p: default: }`，滿了就靜默丟棄 | WP-00 |
| `internal/tui/dashboard_update.go:346-375` | `updatePromptInput` 按 Enter 時先清空輸入框，再在 `Update()` 內非阻塞送入 `PromptInjectCh`；送不進去時草稿直接消失（也違反「Update 不做 channel 操作」） | WP-00 |
| `cmd/hufu/display.go:1950-1958` | `PromptInjectCh` → goroutine → `injector.enqueue` 是第二段 silent drop | WP-00 |
| `internal/tui/tui.go:275-278`、`:304` | prompt 的 `textinput` 只有單行，`CharLimit = 500`；`PromptInjectCh` buffer 16 | P1 用 textarea 取代 |
| `internal/tui/dashboard_view.go:236` | 寬度不到 60 欄就不顯示任務 | P1 版面 |
| `cmd/hufu/chat.go:159-219` | `--tui` 時整個 session 共用一個 `signal.NotifyContext`，沒有每輪的 `SetInvocationID`；第一次 Ctrl+C 觸發 wrap-up，第二次 cancel 整個 session | P0 |
| `cmd/hufu/run.go:385-447` | `runWithInjection` 把每輪結果用 `result += "\n\n---\n\n"` 串起來；`opts.isChatTUI && turn == 0` 時呼叫 `Run`，否則呼叫 `ContinueWithPrompt` | P0 |
| `cmd/hufu/display.go:2018-2044` | `ResultMsg`／`FinishedMsg` 只在 `executeSegments` 全部結束時送一次 → 每輪的回答要等離開 TUI 才看得到 | P0 |
| `cmd/hufu/chat.go:225-353` | plain REPL 每輪都有：`injectFileContexts`、project context、`NewInvocationID`+`SetInvocationID`、獨立的 `signal.NotifyContext`、`turn==0` 時 `Run` 否則 `Continue`（第一輪被取消也算一輪；非取消錯誤不算）、`presentExploratoryDelivery`、run identity、`SaveSession`+`SaveSessionMD`、`savePromptToHistory`，以及 `/reset /team /agents /skills /config /save /help /exit` | WP-01 抽成共用程式 |
| `cmd/hufu/chat.go:159-219` | TUI chat 路徑**沒有** `#file` 注入、project context、slash commands | P1 用共用 turn runner 補齊 |
| `cmd/hufu/chat.go:42-83` | `chat` 子命令沒有綁定 `--theme`、`--display-preset`、`--no-spinner`（`root.go:160-164` 是非 persistent 的 `Flags()`；只有 `--no-color` 是 persistent） | WP-10 |
| `cmd/hufu/team_loader.go:80-200` | 沒有 `--new` 時，`resumeOrStartSession` 會載入既有的 `session.json`；`SetSessionData` 會還原 `LastRunResult`（`internal/team/coordinator_session.go:494`） | P3 只需要「顯示」，不需要新旗標 |
| `cmd/hufu/display.go:1236-1270` | `newCoordDisplay(tc)` 在 `activeTUIProgram` 有值時接上 `makeTUIReporter`，並以 `O_TRUNC` 開啟 `execution_trace.log` | Chat UI 每個 session（每個 team）只呼叫一次，避免每輪截斷 log |
| `cmd/hufu/display.go:80-110` | `ask_user` 經由 `activeTUIProgram` 送出 `tuipkg.AskUserMsg{ReplyCh}`，並用 `askUserMu` 序列化（同一時間只有一個問題） | P2 直接重用 |
| `internal/team/status.go:82-96` | `StatusEvent.withTool`／`withToolResult` 建立事件時已經做 `utils.RedactSecrets` | P2 不需要另外 redact，但要測試 |
| `internal/team/status.go:147-156` | TaskStatus：`pending in_progress verifying done error blocked skipped planned paused protocol_incomplete` | P1／P2 |
| `internal/team/run_result.go:21-27` | RunOutcome：`completed unverified partial blocked failed cancelled stalled`；`FormatCanonicalStatus(*RunResult)` | P1 |
| `internal/tui/theme.go` | `styleSet`、`cycleTheme`、`canSwitchTheme`、`watchThemeCmd`、`probeThemeCmd` 都是 `package tui` 內未匯出的成員 | Chat 程式放在 `package tui` |
| `internal/tui/sanitize.go` | `sanitizeTerminalText`、`wrapCells`（以 cell 寬度換行） | 重用 |
| `go.mod` | bubbletea v1.3.10、bubbles v1.0.0（內含 `textarea`）、lipgloss v1.1.0、`teatest`、`creack/pty` v1.1.24 | PM 升級到 `charm.land/bubbletea/v2` v2.1.0、`charm.land/bubbles/v2` v2.2.1、`charm.land/lipgloss/v2` v2.0.6、`teatest/v2` |
| `cmd/hufu/display.go:685-709` | CLI 樣式來自 `tuipkg.NewCLIStyleSet`；no-color 時以 `lipgloss.SetColorProfile(termenv.Ascii)` 讓 v1 renderer 輸出零 ANSI；stdout 不是 TTY 時 v1 renderer 也自動不輸出 ANSI | PM 必須在 v2（`Render()` 一律輸出完整 ANSI）下保留這個行為 |
| `cmd/hufu/process_exit_contract_test.go` | 已經有假的 OpenAI-compatible `/v1/chat/completions` server 測試寫法 | WP-15 E2E |
| `internal/inspect/cost.go:16`、`cmd/hufu/inspectcmd.go:584` | `inspectpkg.InspectCost`、`renderInspectCostText` | `/cost` |
| `internal/tui/dashboard_update.go:160,237` | dashboard 已經用 Tab／Shift+Tab 切換欄位 | Chat↔Tasks 切換改用 `Ctrl+T` |

---

## 2. 目標行為

- `hufu chat --ui chat [--agent-team NAME | --default]` 開啟 Chat-first TUI；整個 session 共用同一個已載入的 team、MCP、workspace lease 和 Coordinator。
- 多行輸入；`#file`、project context、`@name` 的處理**與 plain REPL 完全相同**（同一份 turn runner；`@name` 不另外解析，原文交給 coordinator）。
- Transcript 依輪次顯示：使用者訊息、進度卡片（live，該輪結束後凍結）、最終回答、outcome 標籤。
- 每輪結束後（不論哪種 outcome 或錯誤），輸入框都恢復可用；錯誤在 transcript 中保持可見。
- `Ctrl+T` 切換到既有的 Tasks dashboard，切回來時捲動位置、草稿、目前輪次狀態都保留。
- 在 120×30、80×24、40×20 都能操作；寬度 0 不會 panic；epaper 模式下沒有 spinner 和游標閃爍，FPS 為 1。
- 預設 `--ui plain`，Chat UI 必須明確指定才會啟用。

### 2.1 版面

```text
┌─ hufu · dev-team · ws:dev-team · idle ──────────────────────────────┐
│ You                                                                  │
│ 請檢查 auth module 的安全性                                            │
│                                                                      │
│  ┌─ Turn 3 · running ───────────────────────────────────────────┐    │
│  │ Security  done          local/qwen3                          │    │
│  │ Coder     in_progress   codex/gpt-5   edit internal/auth.go  │    │
│  │ Verifier  planned                                            │    │
│  └──────────────────────────────────── [Ctrl+T: tasks] ─────────┘    │
│                                                                      │
│ Coordinator · unverified                                             │
│ 修正結果與限制...（純文字，code fence 原樣保留）                          │
├──────────────────────────────────────────────────────────────────────┤
│ > 輸入訊息、#file、/command                                           │
│   Enter 送出 · Ctrl+J／Alt+Enter 換行 · Ctrl+P 命令                     │
├──────────────────────────────────────────────────────────────────────┤
│ queued:0 · last: unverified · theme:auto · Ctrl+T tasks · ? help     │
└──────────────────────────────────────────────────────────────────────┘
```

---

## 3. 架構與套件責任

```text
cmd/hufu/chat.go ── --ui selector ─┬─ plain REPL ──────────┐
                                   ├─ dashboard (legacy)    │
                                   └─ cmd/hufu/chat_ui.go   │
                                          │                 │
                         tea.Program(ChatModel)             │
                                          │ tea.Cmd         │
                         internal/conversation.Controller   │
                                          │ RuntimePort     │
                         cmd/hufu/chat_runtime_adapter.go   │
                                          │                 │
                         cmd/hufu/chat_turn.go (runChatTurn) ◄┘  ← plain REPL 共用
                                          │
                         team.Coordinator Run / ContinueWithPrompt / SetWrapUp
```

狀態事件的路徑**沿用既有程式**：`Coordinator.SetStatusReporter` ← `newCoordDisplay` → `makeTUIReporter(p)` → `p.Send(tuipkg.*Msg)` → `ChatModel`（再轉給內嵌的 dashboard `Model`）。Controller **不經手** StatusEvent。

| 位置 | 可以持有 | 不可以持有 |
|---|---|---|
| `internal/conversation/` | Command／Ack／Event 型別、Controller actor、queue、dedupe、每輪狀態機、`RuntimePort` 介面 | Bubble Tea、stdin/stdout、CLI `opts`、`cmd/hufu` 型別、Tool 權限決策 |
| `cmd/hufu/chat_turn.go` | 從 plain REPL 抽出的單輪執行邏輯、Run／Continue 選擇規則 | 任何直接寫 stdout／stderr 的程式碼（交給呼叫端） |
| `cmd/hufu/chat_runtime_adapter.go` | 用 `teamContext` 實作 `RuntimePort`，處理切換 team、重設對話 | 新的 scheduler 或 policy |
| `cmd/hufu/chat_ui.go` | 建立 program、activate／deactivate TUI、Controller 生命週期、事件轉送、關閉順序 | 判定任務成功或失敗 |
| `internal/tui/chat_*.go`（`package tui`） | `ChatModel`、composer、transcript、版面、按鍵、palette、內嵌 dashboard | 呼叫模型、執行 Tool、寫入 workspace |

> 放在 `package tui` 而不是 `internal/tui/chat` 子 package，是為了直接重用未匯出的 `styleSet`、theme 偵測、`askState`、`sanitizeTerminalText`／`wrapCells`，並內嵌 dashboard `Model`，不需要為此匯出內部實作。所有新檔案都以 `chat_` 為前綴。

---

## 4. 身分與資料權威

| 名稱 | 來源 | 用途 |
|---|---|---|
| `turn_id` | 由 Controller 在 admission 時用 `team.NewInvocationID()` 產生（可注入） | 同時作為該輪的 invocation ID：adapter 會呼叫 `SetInvocationID(turn_id)` 並設定 `opts.invocationID` |
| `command_id` | ChatModel 每次送出時產生（隨機 hex） | **只在 process 內**去重；不持久化 |
| run identities | `snapshotExecutionRunCounts`（該輪開始前）+ `collectExecutionRunIdentities`（結束後） | 顯示在 `/status`；不寫到 stderr |
| 最終回答 | `Run`／`ContinueWithPrompt` 經 `presentExploratoryDelivery` 後的回傳值 | transcript 的 Assistant item |
| 該輪結果 | 該輪開始前記下 `prev := LastRunResult()`；結束後若 `LastRunResult() != prev`，就是本輪的 `*RunResult`，否則為 `nil` | outcome 標籤。**`nil` 時顯示 `no result`，不得沿用前一輪的 outcome** |
| 任務列表 | 既有的 `TasksUpdatedMsg`（canonical `TodoItem` projection） | 進度卡片、Tasks view；唯讀 |
| 開啟時的歷史 | `tc.sessionData.Entries`（`Role`／`Content`／`Timestamp`）與還原後的 `LastRunResult()` | P3 顯示；只顯示，不觸發執行 |
| UI 狀態 | 捲動、草稿、focus、目前 view、render cache | 可以遺失，**不持久化** |

---

## 5. `internal/conversation` 協定（實作契約）

```go
package conversation

type CommandKind string

const (
	CommandSubmitPrompt CommandKind = "submit_prompt"
	CommandCancelTurn   CommandKind = "cancel_turn"
	CommandWrapUp       CommandKind = "wrap_up"
	CommandResetContext CommandKind = "reset_context" // P3
	CommandSwitchTeam   CommandKind = "switch_team"   // P3
)

type Command struct {
	ID     string
	Kind   CommandKind
	Text   string // submit_prompt only
	Target string // switch_team only
}

type AckStatus string

const (
	AckAccepted AckStatus = "accepted" // 已開始（或已執行的控制命令）；in-memory admission，非 durable
	AckQueued   AckStatus = "queued"   // 已排入佇列
	AckBusy     AckStatus = "busy"     // 佇列已滿；未排入
	AckRejected AckStatus = "rejected" // 驗證失敗或狀態不允許；Reason 必填
)

type Ack struct {
	CommandID string
	TurnID    string // submit_prompt 時有值
	Status    AckStatus
	Reason    string
	Position  int // queued 時在佇列中的位置（從 1 開始）
}

type TurnResult struct {
	TurnID    string
	Answer    string
	Result    *team.RunResult // 本輪沒有產生 RunResult 時為 nil
	Err       error
	Cancelled bool
	RunIDs    []string
}

type RuntimePort interface {
	RunTurn(ctx context.Context, turnID, prompt string) TurnResult
	RequestWrapUp()
	ResetContext(ctx context.Context) error           // P3
	SwitchTeam(ctx context.Context, name string) error // P3；切換成功後的 UI 更新由 adapter 的 callback 處理（WP-31）
}

// Event 是 sealed interface（未匯出的 marker method）。
type Event interface{ isEvent() }

type TurnStarted struct{ TurnID, CommandID, Prompt string }
type TurnSettled struct{ Result TurnResult }
type QueueDropped struct{ TurnIDs []string } // 關閉時被丟棄的排隊 prompt
type ControlDone struct {
	CommandID string
	Kind      CommandKind
	Target    string // switch_team 的目標
	Err       error
}
type Closed struct{ Err error }

type Config struct {
	QueueCapacity  int           // 預設 16，上限 64
	MaxPromptBytes int           // 預設 256 KiB
	DedupeWindow   int           // 預設 256 個最近的 command_id
	NewTurnID      func() string // 預設 team.NewInvocationID
}

func New(port RuntimePort, cfg Config) *Controller
func (c *Controller) Start(sessionCtx context.Context)
func (c *Controller) Submit(cmd Command) Ack // 不會阻塞在 runtime 上
func (c *Controller) Events() <-chan Event
func (c *Controller) Close(timeout time.Duration) CloseReport // CloseReport{DroppedTurnIDs []string; TimedOut bool}
```

### 5.1 Submit 規則

| 情況 | Ack |
|---|---|
| Controller 正在關閉或已關閉 | `rejected`：`session closing` |
| `ID` 為空、`Kind` 未知 | `rejected` |
| `ID` 在 dedupe window 內出現過 | 回傳**當初那個** Ack（不會再執行一次） |
| `submit_prompt`：`TrimSpace` 後為空、不是合法 UTF-8、超過 `MaxPromptBytes` | `rejected`，Reason 註明原因與上限 |
| `submit_prompt`：沒有進行中的輪次且佇列為空 | `accepted`，馬上開始 |
| `submit_prompt`：有進行中的輪次，且佇列還沒滿 | `queued`，帶 `Position` |
| `submit_prompt`：佇列已滿 | `busy` |
| `cancel_turn`：有進行中的輪次 | `accepted`；cancel 該輪的 ctx，狀態轉為 `cancelling` |
| `cancel_turn`：沒有進行中的輪次 | `rejected`：`no active turn` |
| `wrap_up`：有進行中的輪次 | `accepted`；呼叫 `port.RequestWrapUp()` |
| `wrap_up`：沒有進行中的輪次 | `rejected` |
| `reset_context`／`switch_team`：idle 且佇列為空 | `accepted`；由 actor 依序執行，完成時送出 `ControlDone` |
| `reset_context`／`switch_team`：有輪次進行中或佇列非空 | `rejected`：`busy: finish or cancel the active turn first` |

**不變式**：每一筆 Submit 都會得到上表其中一種 Ack；沒有任何路徑會靜默丟棄命令。

### 5.2 每輪狀態機

```text
idle ──submit──► running ──RunTurn 回傳──► idle（若佇列有下一筆 → running）
running ──cancel_turn──► cancelling ──RunTurn 回傳──► idle
任何狀態 ──Close──► closing ──► closed
```

- 每輪使用 `context.WithCancel(sessionCtx)`。取消某一輪不會影響 session ctx。
- 同一時間只有一輪在執行（serial admission）；worker 的並行仍由既有 DAG scheduler 負責。
- 每輪剛好送出一次 `TurnStarted`、一次 `TurnSettled`。
- `Events()` channel 的 buffer 為 64；actor 用阻塞式 send，並同時 `select` 關閉訊號。事件數量很少（每輪 2～3 筆），消費端（`chat_ui.go` 的轉送 goroutine）會持續讀取。

### 5.3 Close 順序

1. 標記為 closing，之後的 Submit 一律 `rejected`。
2. 清空佇列，送出 `QueueDropped`（ChatModel 會把這些項目標成 `dropped`）。
3. 如果有輪次在執行，就 cancel 它，並等 `RunTurn` 回傳，最多等 `timeout`。
4. actor 結束 → 送出 `Closed` → 關閉 `Events()`。
5. 回傳 `CloseReport`。`TimedOut == true` 時，由 `chat_ui.go` 呼叫既有的 `waitWithTimeoutAndFinalize`／`emergencyFinalizeCoordinator`。

---

## 6. 工作包

### Phase 0 — 控制層與每輪生命週期

#### WP-00｜修正舊路徑的 silent drop（`run --tui`、`chat --tui`）

- `promptInjector.enqueue(prompt string) bool`：滿了回傳 `false`。
- 移除 `Model.PromptInjectCh` 與 `display.go` 的轉送 goroutine，改成 `Model.SubmitPrompt func(string) bool`（由 `runWithTUI` 注入 `injector.enqueue`；沒有 injector 時為 `nil`）。
- `updatePromptInput` 按 Enter：**不清空**輸入，設定 `submitting=true`，回傳一個會呼叫 `SubmitPrompt` 的 `tea.Cmd`，結果以 `promptSubmitResultMsg{Prompt, OK}` 回傳。
  - `OK`：清空輸入、離開輸入模式、套用既有的 `IsChat` 狀態更新。
  - `!OK`：保留草稿、留在輸入模式、狀態列顯示 `queue full — prompt not sent`。
- 更新受影響的測試：`internal/tui/teatest_integration_test.go`、`internal/tui/l1_overlay_unit_test.go`。
- **測試**：injector 容量滿時回傳 false；Model 在 OK／!OK 時的草稿與狀態（table-driven）；`Update` 內沒有 channel 操作（以「回傳 Cmd、在 Cmd 被執行之前 `SubmitPrompt` 尚未被呼叫」來驗證）。
- 其餘行為不變。

#### WP-01｜抽出共用的 turn runner（`cmd/hufu/chat_turn.go`）

- 從 `chat.go:309-353` 抽出：

```go
type chatTurnSequencer struct{ turn int } // Run/Continue 規則的唯一來源
func (s *chatTurnSequencer) first() bool
func (s *chatTurnSequencer) record(o chatTurnOutcome) // 成功或取消 → turn++；非取消錯誤 → 不變
func (s *chatTurnSequencer) reset()

type chatTurnOutcome struct {
	Answer        string
	Err           error
	Cancelled     bool
	Result        *team.RunResult // 依 §4 的 prev 比較規則
	RunIdentities []cliRunIdentity
}

func runChatTurn(ctx context.Context, rt chatTurnRuntime, seq *chatTurnSequencer, invocationID, line string) chatTurnOutcome
```

- `runChatTurn` 依序做：`injectFileContexts` → project context（沿用三個條件）→ `opts.invocationID`＋`SetInvocationID` → run count snapshot → `prev := LastRunResult()` → `Run` 或 `ContinueWithPrompt` → `presentExploratoryDelivery` → 收集 identities → 成功時 `SaveSession`＋`SaveSessionMD` → `go savePromptToHistory(...)` → `seq.record`。
- `chatTurnRuntime` 是窄介面（`Run`、`ContinueWithPrompt`、`SetInvocationID`、`LastRunResult`，以及包裝 `presentExploratoryDelivery` 與存檔的函式欄位），讓測試可以使用 fake。**不得**因此改變任何既有 helper 的行為。
- 顯示相關的副作用（`newCoordDisplay`、identity heartbeat、印出結果、`emitExecutionIdentity`）留在 plain REPL 呼叫端。
- plain REPL 改成呼叫 `runChatTurn`，輸出與行為完全不變。
- **測試**（fake runtime，table-driven）：第一輪 Run、第二輪 Continue；第一輪被取消 → 下一輪 Continue；第一輪非取消錯誤 → 下一輪仍是 Run；`reset()` 之後回到 Run；每輪設定不同的 invocation ID；`LastRunResult` 沒變時 `Result == nil`；`#file` 注入確實被套用；`runChatTurn` 不寫 stdout／stderr（擷取 `os.Stdout`／`os.Stderr` 驗證）。既有的 chat 測試全部通過。

#### WP-02｜`internal/conversation` Controller

- 依 §5 實作 `types.go`、`controller.go`、`queue.go`（含 dedupe），以及對應的 `_test.go`。
- **測試**（fake `RuntimePort`，可控制何時回傳）：
  - C01 單次送出 → `accepted`，`RunTurn` 被呼叫一次，`TurnStarted`／`TurnSettled` 各一筆
  - C02 同一個 ID 重送 → 回傳原本的 Ack，`RunTurn` 不會再被呼叫
  - C03 容量 1：執行中＋1 筆排隊 → 下一筆 `busy`
  - C04 burst 100 筆（容量 16）：`accepted + queued + busy + rejected == 100`，執行次數等於 `accepted + queued`
  - C05 空字串、非法 UTF-8、超過上限 → `rejected` 並帶 Reason
  - C06 cancel 只取消當輪的 ctx，sessionCtx 仍然有效，佇列中的下一筆接著執行
  - C07 沒有進行中的輪次時 cancel／wrap_up → `rejected`
  - C08 wrap_up 時呼叫 `RequestWrapUp` 一次，且不 cancel ctx
  - C09 Close 時有 2 筆排隊 → `QueueDropped` 列出這 2 筆，進行中的輪次被 cancel，`Events()` 被關閉
  - C10 Close 逾時（fake 不回傳）→ `TimedOut == true`；fake 放行之後 actor 會結束，沒有 goroutine leak（用 `runtime.NumGoroutine` 前後比較並設定逾時重試；**不新增 goleak 依賴**）
  - C11 Close 之後 Submit → `rejected`，不會 panic，也沒有 send on closed channel
  - C12 Submit 由多個 goroutine 同時呼叫（`-race`）
- 必須通過 `go test -race ./internal/conversation/...`。

#### WP-03｜RuntimePort adapter（`cmd/hufu/chat_runtime_adapter.go`）

- `RunTurn` 呼叫 `runChatTurn(ctx, rt, seq, turnID, prompt)`，再轉成 `TurnResult`（`Cancelled` 依 `ctx.Err() == context.Canceled` 判斷；`RunIDs` 取自 identities）。
- `RequestWrapUp` 呼叫目前 `tc.coordinator.SetWrapUp()`；不額外排入 `ContinueWithPrompt("")`。
- `ResetContext`／`SwitchTeam` 先回傳 `errors.New("not implemented")`，WP-31 再實作。
- **測試**：用 fake `chatTurnRuntime` 驗證 `turnID` 被當成 invocation ID；cancel 的結果；`RunIDs` 的對應。

**P0 Gate**：WP-00～03 的測試全部通過；Controller 可以在沒有終端機的環境下由 fake 完整驅動；`go test ./...`、`-race`（§0-12）、`go vet ./...`、`golangci-lint run` 都通過；plain／dashboard／`hufu run` 的既有測試沒有被修改（WP-00 列出的兩個測試檔除外）。

---

### Phase M — Bubble Tea v2 遷移

目標：把整個 repo 從 Charm v1 套件換成 v2，作為 P1 的基礎。**功能不變；畫面允許些許差異**（使用者決定）。**能用 v2 現成功能就用現成的**：宣告式 `tea.View`（AltScreen、MouseMode、KeyboardEnhancements、Cursor）、`colorprofile` 的偵測與色彩轉換、bubbles v2 元件（`viewport`、`textarea`、`textinput`）的內建行為，不要為了保留 v1 外觀而自行重寫。

#### WP-M1｜依賴與 import

- `go get charm.land/bubbletea/v2@v2.1.0 charm.land/bubbles/v2@v2.2.1 charm.land/lipgloss/v2@v2.0.6 github.com/charmbracelet/x/exp/teatest/v2@latest`，改寫所有 import，`go mod tidy` 後 `go.mod` 不再直接依賴 `github.com/charmbracelet/{bubbletea,bubbles,lipgloss}` 與 v1 `teatest`。
- 驗收：`grep -rE '"github.com/charmbracelet/(bubbletea|bubbles|lipgloss)' --include=*.go .` 沒有結果。

#### WP-M2｜TUI（`internal/tui`）

- `Model.View()` 改回傳 `tea.View`：原本的字串渲染改名為 `render() string`；`View()` 設定 `AltScreen = true`，`MouseMode` 由 `mouseEnabled` 決定（取代 `tea.WithAltScreen()`、`tea.EnableMouseCellMotion`／`tea.DisableMouse` 命令）。
- 按鍵：`tea.KeyMsg` → `tea.KeyPressMsg`；`" "` → `"space"`。滑鼠：改用 `MouseClickMsg`／`MouseReleaseMsg`／`MouseMotionMsg`／`MouseWheelMsg` 與 `msg.Mouse()`。
- bubbles：`viewport`／`textinput` 的 `Width`／`Height`／`YOffset` 欄位改用 getter／setter；`textinput` 樣式改用 `SetStyles`；epaper 下游標不閃爍。
- 色彩：`lipgloss.AdaptiveColor` 改成 hufu 自己的 light／dark 字串對，由既有 `resolveColor` 依 `ThemeMode` 選擇；主題偵測（`detectRuntimeTheme`、tmux 背景切換）邏輯不變。
- `--no-color`／`NO_COLOR` 時 TUI 使用 mono 樣式（不出現任何色碼）。

#### WP-M3｜CLI 與 program 啟動（`cmd/hufu`）

- `tea.NewProgram` 移除 `tea.WithAltScreen()`（改由 View 宣告）；保留 `tea.WithoutSignalHandler()`、epaper 的 `tea.WithFPS(1)`。
- CLI 樣式：以 `colorprofile.Detect(os.Stdout, os.Environ())`（與 v1 renderer 相同以 stdout 判斷）取得 profile，色彩以 `profile.Convert` 降階；profile 為 `NoTTY`／`Ascii`，或 `--no-color`、`NO_COLOR`、`--display-mode plain`、`--output json` 時，`NewCLIStyleSet` 回傳不輸出任何 ANSI 的純文字樣式；移除 `termenv` 的直接使用。
- `ReleaseTerminal`／`RestoreTerminal`（path consent）、`tea.ExecProcess`（PTY attach）、`p.Send` 等行為不變。

#### WP-M4｜測試

- 既有測試做機械式遷移（按鍵建構方式、`teatest/v2`、bubbles getter）。行為類斷言不放寬；只有因 v2 現成元件造成的外觀差異（例如游標、viewport 換行、space 的字串名稱）可以調整對應斷言，並在 commit body 中逐一列出。
- 新增測試：stdout 非 TTY 時 CLI 樣式輸出不含 ESC；`View()` 的 `AltScreen` 與 `MouseMode` 依狀態正確；epaper 下 `Init`／`Focus` 不排程閃爍。

**PM Gate**：§0-12 的驗收命令全部通過；`go test ./...` 的結果與 P0 相同（沒有新的失敗）；WP-M1 的 grep 驗收通過。

---

### Phase 1 — Chat 介面

#### WP-10｜CLI 選擇器與旗標（`cmd/hufu/chat.go`、`cli_metadata.go`）

- 新增 `--ui plain|dashboard|chat`，預設 `plain`，並以 `registerStaticFlagCompletion` 提供補全。
- `--tui` 是 `--ui dashboard` 的別名。同時指定 `--tui` 與 `--ui chat`（或 `--ui plain`）→ 錯誤：`--tui conflicts with --ui <value>`。
- 在 `chat` 子命令綁定 `--theme`、`--display-preset`、`--no-spinner`，寫入既有的 `opts.themeMode`、`opts.displayPreset`、`opts.noSpinner`（`resolvedTUIOptions` 會讀取它們）。
- `--ui chat` 時 stdin 與 stdout 必須都是 TTY（`term.IsTerminal`）；否則回傳錯誤 `--ui chat requires an interactive terminal; use --ui plain`。**不自動降級**。
- 把 `runChat` 拆成 `runChatPlain`／`runChatDashboard`／`runChatUI`，讓 `runChat` 的 gocyclo 降到 40 以下，並移除 `.golangci.yml` 中 `cmd/hufu/chat\.go` 的 exclusion。
- 更新 `cli_metadata.go`（`display` 群組加入 `ui`）與 `chatHelpText`。
- **測試**：旗標解析 table（plain／dashboard／chat／`--tui`／衝突／非法值）；非 TTY 時的錯誤；`cli_metadata_test.go`；既有的 chat 測試通過。

#### WP-11｜`ChatModel` 骨架與內嵌 dashboard（`internal/tui/chat_model.go`、`chat_keys.go`）

- `ChatModel` 持有：內嵌的 dashboard `Model`（以 `NewWithOptions` 建立，並新增 `embedded bool` 欄位）、composer、transcript、focus（`composer`／`transcript`）、view（`chat`／`tasks`）、目前輪次狀態、佇列計數、overlay 堆疊。
- `internal/tui` 不能 import `cmd/hufu`，因此 cmd 端的功能一律透過注入的 `ChatActions` 結構（函式欄位：`Submit func(conversation.Command) conversation.Ack`，P3 再加 `Cost`、`Save`）存取，且只在 `tea.Cmd` 中呼叫。`internal/tui` 可以 import `internal/conversation`（反向不行）。
- `embedded` 時 dashboard 的所有離開路徑（`q`、`Esc` 在最上層、`handleCtrlC`）改成送出 `embeddedExitMsg{}`，不回傳 `tea.Quit`；`c`（dashboard 的 prompt input 鍵；`i` 是 info panel，維持原樣）改成送出 `embeddedComposeMsg{}`。非 embedded 時行為不變。
- 所有 dashboard 的訊息（`TasksUpdatedMsg`、`TaskLogMsg`、`StatusBarMsg`、`OperatorSnapshotMsg`、`TerminalSessionMsg`、`AskUserMsg`、`AskUserCancelMsg`、`TeamInfoMsg`、theme 訊息等）一律轉給內嵌的 `Model`；ChatModel 只從中讀取進度卡片需要的資料。
- 按鍵依 §7 分派。`View()` 在寬度或高度為 0、或尚未收到 `WindowSizeMsg` 時，回傳安全的 placeholder。
- **測試**：teatest 驗證 `Ctrl+T` 進出 Tasks view 後草稿與捲動位置保留；embedded dashboard 的 `q`／`Esc` 不會讓程式結束；寬度 0 不 panic。

#### WP-12｜Composer（`internal/tui/chat_composer.go`）

- 使用 `charm.land/bubbles/v2/textarea`，以內建 `DynamicHeight` 依內容自動長高（上限隨終端高度 6／3／1 列）。`Enter` 送出；`Ctrl+J`、`Alt+Enter` 一律插入換行；`View()` 另外請求 keyboard enhancements，終端支援時 `Shift+Enter` 也插入換行。貼上（`tea.PasteMsg`）的內容（多行、Unicode）原樣保留。
- 位元組上限與 Controller 的 `MaxPromptBytes` 相同。接近或超過上限時顯示 `n / 256 KiB`；超過上限時 Enter 不送出，並顯示原因。
- 送出流程：Enter → 回傳呼叫 `Controller.Submit` 的 `tea.Cmd` → 收到 `ackMsg`。
  - `accepted`／`queued`：清空草稿，在 transcript 加入 User item（queued 時標註 `queued #n`）。
  - `busy`／`rejected`：**保留草稿**，顯示 Reason。
- Slash command 判斷：第一個 token 必須符合 `^/[a-z][a-z-]*$` 才算命令。
  - 符合且在命令表中 → 執行命令。
  - 符合但不在命令表中 → 顯示 `unknown command /x — use //x to send literally`，不送出。
  - 以 `//` 開頭 → 去掉一個 `/` 後當成一般 prompt 送出。
  - 其他（例如 `/home/x 壞了`）→ 一般 prompt。
- `Tab`：cursor 所在 token 以 `/` 開頭時，從命令表補全；以 `@` 開頭時，從 `TeamInfo.Agents`／`TeamInfo.AvailableTeams` 補全；其他情況 no-op（不插入 tab 字元）。
- **測試**：table-driven 的命令判斷（含 `//`、路徑、大寫）；100 KB 貼上不截斷；中文、emoji、寬字元；busy／rejected 時保留草稿；Tab 補全。

#### WP-13｜Transcript 與進度卡片（`internal/tui/chat_transcript.go`、`chat_progress.go`）

- Item 種類：`user`、`assistant`（最終回答）、`progress`（每輪一張）、`notice`（系統訊息，例如 context cleared、dropped）、`error`。
- 所有文字都先經過 `sanitizeTerminalText`，再依 cell 寬度換行（`ansi.Wrap`；code fence 內只做 `ansi.Hardwrap`）。**不做 Markdown 渲染**。
- 每個 item 依 `(width, theme, version)` 快取 render 結果；resize 或切換 theme 時重算。捲動與可視範圍交給 bubbles v2 `viewport`（`SetContentLines`、`AtBottom`、`GotoBottom`）。
- 捲動：在最底部時自動跟隨新內容；使用者往上捲後停止跟隨，並顯示 `↓ new` 提示；`G` 回到底部並恢復跟隨。
- 進度卡片：
  - `TurnStarted` 時記錄 baseline（當下最後一份 `TasksUpdatedMsg` 中各 task 的 `ID→Status`）。
  - 輪次進行中，只顯示 ID 不在 baseline、或 Status 與 baseline 不同的 task（排除 `team.CoordTodoID`）。
  - 每一列顯示 `Agent`、canonical status 原字串、`ExecutionTarget`（取自 `TodoItem`；沒有就留空，**不從 model alias 猜測**），以及最後一筆 `TaskLogMsg`（已經 redact 過）。
  - `TurnSettled` 時凍結成快照；之後到達的 `TasksUpdatedMsg` 不會修改已凍結的卡片。
- 最終回答 item 的標題列：`Coordinator · <FormatCanonicalStatus(result)>`；`Result == nil` 時顯示 `no result`；`Err != nil` 時加上 error item；`Cancelled` 時顯示 `cancelled`。**不顯示任何本規格自行合成的 "verified"**。
- **測試**：10 輪連續對話，每輪各有自己的 User／Progress／Assistant；遲到的 `TasksUpdatedMsg` 不改已凍結卡片；ANSI／OSC 注入被清除；`Result == nil` 不會沿用前一輪的 outcome；向上捲動時不會被拉回底部。

#### WP-14｜版面、主題、epaper（`internal/tui/chat_layout.go`）

- 寬度斷點：`>=110` 時顯示 transcript 與右側 agent 摘要欄；`60–109` 單欄；`40–59` 精簡單欄（省略次要欄位）；`<40` 只保留 transcript、composer 和一行狀態列，仍可送出、取消、看 help。
- 主題使用既有的 `styleSet`、`cycleTheme`／`canSwitchTheme`、`watchThemeCmd`／`probeThemeCmd`。`T` 只在 transcript focus 時切換主題（composer focus 時 `T` 是一般字元）。
- epaper（`DisplayEpaper`）：program 加上 `tea.WithFPS(1)`；textarea 不閃爍；不排程 spinner 或 tick Cmd。
- **測試**：teatest 在 120×30、80×24、40×20、30×10、0×0 下 render，不 panic，composer 一定可見，任何一行都不超過寬度；epaper 下 `Init`／`Update` 不回傳 blink 或 tick Cmd；切換主題後捲動位置與草稿不變。

#### WP-15｜Chat UI 啟動與關閉（`cmd/hufu/chat_ui.go`）

- 把 `chat.go:161-197` 建立 `tuipkg.TeamInfo` 的程式抽成 `buildChatTeamInfo(tc *teamContext, registry *team.TeamRegistry) tuipkg.TeamInfo`，dashboard 路徑與 Chat UI 共用；dashboard 的輸出不變。
- 建立 `tea.NewProgram(chatModel, tea.WithAltScreen(), tea.WithoutSignalHandler(), [epaper: tea.WithFPS(1)])`，接著 `activateTUIProgram(p)`。
- **每個 team 只呼叫一次** `newCoordDisplay(tc)`（不要每輪都呼叫）；關閉時呼叫 `stopTimer()`。
- 建立 Controller（adapter 來自 WP-03），`Start(sessionCtx)`。轉送 goroutine：`for ev := range ctrl.Events() { p.Send(chatControllerEventMsg{ev}) }`。
- 以 `ChatActions{Submit: ctrl.Submit}` 注入 ChatModel（WP-11）。
- 外部送來的 SIGINT／SIGTERM（`signal.NotifyContext` 只用在 session 層級）→ 送出 `requestCloseMsg` → 走正常的關閉流程。
- 關閉順序：ChatModel 確認離開 → `ctrl.Close(gracefulTimeout)`（使用 `resolvedGracefulWrapUpTimeout`）→ 逾時時 `emergencyFinalizeCoordinator` → program 結束 → `deactivateTUI` → `disp.stopTimer()` → 既有的 `defer tc.Close()`。
- 啟動時，如果 `tc.sessionData.Entries` 非空，transcript 先加入一筆 notice：`Resumed session (N exchanges)`（完整歷史在 WP-30）。
- **E2E 測試**（`cmd/hufu/chat_ui_e2e_test.go`）：
  - build 測試用 binary；用既有的假 provider 寫法啟動 `/v1/chat/completions` server；在 `creack/pty`（80×24）中執行 `hufu chat --ui chat --default --provider-url <fake>`，使用暫存的 workspace 與 HOME。
  - 腳本：送出 prompt A → 等到畫面出現 A 的回答 → 送出 prompt B → 等到 B 的回答 → `/exit` → exit code 0。
  - 斷言：假 server 收到的 coordinator 請求中，第二輪帶有第一輪的歷史；兩個回答都在畫面上出現過。
  - 取消：假 server 對某個 prompt 延遲回應 → 送出 Ctrl+C → 畫面出現 `cancelled` → 再送出下一輪 prompt 能成功。
  - 如果既有的假 provider 寫法無法讓 coordinator 完成一輪，**只能擴充測試用的假 server**；不得在 production code 中加入測試後門。

**P1 Gate**：WP-10～15 的測試全部通過，包括 10 輪的 teatest 與 PTY E2E；`hufu chat`（plain）與 `hufu chat --tui` 的既有測試不需修改就通過；§0-12 的驗收命令全部通過。

---

### Phase 2 — 任務工作台與 ask_user

#### WP-20｜從進度卡片連到 Tasks detail

- 在 transcript focus 下選取某張進度卡片的某一列（`n`／`p` 移動選取——`j`／`k` 保留給 viewport 捲動；`Enter` 開啟；`Esc` 取消選取）→ 切換到 Tasks view，並開啟該 task 的 detail（`package tui` 內新增 `openTaskDetail`）。
- 從 detail 返回（dashboard 原本的 `Esc`）→ 回到 Tasks 主畫面；`Ctrl+T` 回到 Chat 時，選取狀態與捲動位置都保留。
- 已凍結的卡片如果指向已經被新一輪覆寫的 task ID，開啟時要顯示 `task has newer activity` 提示（比較目前 Status 與凍結時的 Status）。
- **測試**：teatest 驗證選取 → detail → 返回 → 切回 Chat 的整條路徑；同一個 task ID 跨輪出現時有提示。

#### WP-21｜工具活動摘要與 redaction 回歸測試

- 進度卡片每一列顯示該 task 最後一筆 `TaskLogMsg` 的單行摘要（截斷到欄寬）。
- **測試**：
  - team 端：`internal/team/status_test.go` 已經有 `TestStatusToolResultRedactsSecretsBeyondCollapsedPreview` 涵蓋 `withToolResult`。若沒有涵蓋 `withTool`（ToolArgs）的測試，就在同一個檔案新增一個（只新增測試）。fixture 使用 `utils.RedactSecrets` 會處理的格式：`ghp_` 後接 36 個英數字、`sk-` 後接 48 個英數字（見 `internal/utils/redact.go:66-73`）。
  - chat 端：在 `cmd/hufu` 建立 `ToolArgs`／`ToolResult` 已經過 `utils.RedactSecrets` 的 `StatusEvent`，並在其中混入 ANSI／OSC 控制碼，經 `makeTUIReporter` 送到 ChatModel。斷言 `View()` 不含原始 secret 與控制碼；顯示這些內容不會呼叫任何 runtime 函式（fake 的呼叫數為 0）。

#### WP-22｜ask_user 在 Chat 中的行為

- `AskUserMsg` 抵達時，ChatModel 把它轉給內嵌的 `Model`，並以最高優先順序 overlay 顯示既有的 ask 對話框（重用既有的 render 函式）。overlay 開啟期間，所有按鍵都交給 ask 對話框；composer 草稿保留。
- `AskUserCancelMsg` 或該輪被 cancel 時，overlay 關閉，草稿保留。
- 在 Tasks view 時收到 `AskUserMsg` → 自動切回 Chat 並顯示 overlay。
- **測試**：teatest 驗證回答的內容送到正確的 `ReplyCh`；兩次依序到達的 ask（`askUserMu` 會序列化）各自拿到正確的回答；ask 進行中 cancel 該輪 → overlay 關閉，下一輪可以送出；PTY attach：既有的 `terminal_attach_test.go` 在 embedded 模式下仍然通過（只新增測試，不改 `internal/team`）。

**P2 Gate**：WP-20～22 的測試全部通過；§0-12 的驗收命令全部通過。

---

### Phase 3 — 歷史還原與命令

#### WP-30｜開啟時顯示既有歷史

- 啟動時把 `tc.sessionData.Entries` 轉成 transcript items（`Role` 為 `user`／`assistant`，其他 role 顯示為 notice），每一則加上 `restored` 標記，**只顯示一次**。
- 如果 `LastRunResult()` 不是 nil：在歷史最後加一筆 notice `last run: <FormatCanonicalStatus>`。如果 `Continuation != nil` 或 `len(UnresolvedTasks) > 0`，再加一筆 notice：`previous run has unresolved work — run 'hufu session status' or 'hufu resume' outside chat`。**不自動執行任何 recovery**。
- 開啟 Chat 本身不呼叫 `RunTurn`；第一則新訊息依 `chatTurnSequencer` 規則（process 內第一輪 → `Run`），與 plain REPL 一致。
- `--new` 沿用既有的 archive 行為；不新增任何旗標。
- **測試**：預先寫好含 3 輪 entries 的 `session.json` → 開啟後每則各出現一次；fake runtime 在開啟時的呼叫數為 0；帶有 `UnresolvedTasks` 時顯示提示；`--new` 時不顯示舊歷史。

#### WP-31｜Slash commands 與 `Ctrl+P` palette（`internal/tui/chat_commands.go`、`chat_palette.go`）

| 命令 | 動作 | 限制 |
|---|---|---|
| `/help` | 顯示按鍵與命令（overlay） | 無副作用 |
| `/agents`、`/skills`、`/config` | 顯示 `TeamInfo` 的內容（與 plain REPL 相同的資訊） | 唯讀 |
| `/status` | 上一輪的 `FormatCanonicalStatus`、turn ID、RunIDs、佇列數量 | 唯讀 |
| `/tasks` | 等同 `Ctrl+T` | 唯讀 |
| `/theme [auto\|light\|dark\|mono]` | 不帶參數時等同 `T`；帶參數時直接切換 | 只影響 UI |
| `/cost` | 在 `tea.Cmd` 中呼叫 `ChatActions.Cost(ctx, runID)`。`cmd/hufu` 的實作：以上一輪的 RunID 與目前 workspace 呼叫 `inspectpkg.InspectCost`，把 `Envelope.Data` 斷言為 `inspectpkg.CostData`，用 `renderInspectCostText` 寫入 buffer 後回傳文字；ChatModel 以 overlay 顯示。不得改變 `hufu inspect cost` 的輸出 | 唯讀；還沒有任何一輪時顯示 `no run yet` |
| `/save <path>` | 在 `tea.Cmd` 中呼叫 `ChatActions.Save(path)`。`cmd/hufu` 的實作與 plain REPL 相同：`GenerateSessionMD` → `MkdirAll` → `WriteFile 0644`（抽成共用函式，plain REPL 一起改用） | 缺少路徑時顯示 usage |
| `/cancel` | `CommandCancelTurn` | 依 §5.1 |
| `/wrap` | `CommandWrapUp` | 依 §5.1 |
| `/reset` | `CommandResetContext` → adapter 呼叫 `coordinator.ResetConversation()` 並 `seq.reset()`；transcript 加入 `context cleared` notice（保留舊的 items） | 只在 idle 且佇列為空時 |
| `/team` | 列出 `AvailableTeams` | 唯讀 |
| `/team <name>` | `CommandSwitchTeam` → adapter：`switchChatTeamByName` → 成功時停止舊的 display、`Close()` 舊的 tc、對新的 tc 呼叫 `newCoordDisplay`、`seq.reset()`，再呼叫 `chat_ui.go` 注入的 `onTeamSwitched(newTC)` callback，由它送出 `TeamInfoMsg{buildChatTeamInfo(newTC, registry)}`；失敗時保留原本的 team，並由 `ControlDone.Err` 顯示錯誤。`--default` 模式下沒有 registry，回傳錯誤 `team switching is unavailable with --default` | 只在 idle 且佇列為空時 |
| `/exit`、`/quit` | 有進行中的輪次或排隊的 prompt 時，先跳出確認（列出會被 cancel 或丟棄的數量）；否則直接進入關閉流程 | 依 §5.3 |

- 補全的命令表與 `/help` 都由同一份命令表產生。
- `Ctrl+P` palette：列出命令並可輸入文字篩選（`↑`／`↓`／`Ctrl+J`／`Ctrl+K` 移動）；`Enter` 只會把命令**插入 composer**，不會直接執行。
- 實作 adapter 的 `ResetContext`、`SwitchTeam`（WP-03 留下的部分）。
- 切換 team 之後，session 結束時的 `Close()` 必須作用在 adapter **目前**持有的 tc。`runChat` 既有的 `defer` 會閉包捕捉變數 `tc`，要讓它取得 adapter 的目前值，例如透過 `adapter.Current()`。
- **測試**：每個命令的 table-driven 測試；`/team` 在有輪次進行時被拒絕；`/team` 切換到不存在的 team 時保留原本的 team；切換成功後舊的 tc 只 Close 一次，下一輪呼叫 `Run`，離開時 Close 的是新的 tc；`/cost` 與 `hufu inspect cost` 對同一個 workspace 的輸出一致；palette 的 Enter 不觸發 Submit；未知命令的提示。

#### WP-32｜文件

- 新增 `docs/architecture/chat-tui.md`：§3 架構、§4 資料權威、§5 協定、§7 按鍵，以**實際實作**為準。
- 更新 `README.md`、`README.tw.md`：說明 `--ui`、按鍵、命令。
- 更新 `chatHelpText` 與 `cli_metadata.go` 的 examples。
- 不 commit 本文件。

**P3 Gate**：WP-30～32 的測試全部通過；§0-12 的驗收命令全部通過；§11 的 DoD 全部打勾。

---

## 7. 按鍵規格

| 狀態／位置 | 按鍵 | 動作 |
|---|---|---|
| Composer | `Enter` | 送出（依 §5.1）；ACK 為 accepted／queued 時才清空 |
| Composer | `Ctrl+J`、`Alt+Enter`、`Shift+Enter`（終端支援 key disambiguation 時；Bubble Tea v2 預設會請求） | 換行 |
| Composer | `Tab` | 補全 `/cmd`、`@name`；沒有候選時不做任何事 |
| Composer | `Esc` | focus 移到 transcript（草稿保留） |
| Composer | `PgUp`／`PgDn` | 捲動 transcript |
| Transcript | `g`／`G`；其餘捲動鍵（`j`／`k`／`↑`／`↓`／`PgUp`／`PgDn`／`Ctrl+U`／`Ctrl+D` 等）沿用 bubbles `viewport` 的 key map | 捲動，或移動進度卡片的選取（WP-20） |
| Transcript | `i`、`a` | focus 回到 composer |
| Transcript | `n`／`p` | 選取下一／上一個進度卡片列（WP-20） |
| Transcript | `Enter` | 有選取的卡片列時開啟 detail（WP-20）；否則 focus 回到 composer |
| Transcript | `Esc` | 取消卡片列選取 |
| Transcript | `T` | 切換主題 |
| 沒有 overlay 時 | `Ctrl+T` | Chat ↔ Tasks |
| 沒有 overlay 時 | `Ctrl+P` | palette |
| 沒有 overlay 時 | `?` | help（只在 transcript focus 時；composer focus 時是一般字元） |
| idle，草稿非空 | `Ctrl+C` | 清空草稿 |
| idle，草稿為空 | `Ctrl+C` | 跳出離開確認；再按一次 `Ctrl+C` 或 `y` 確認 |
| running | `Ctrl+C` | `CommandCancelTurn`；狀態列顯示 `cancelling — Ctrl+C again to quit` |
| cancelling | `Ctrl+C` | 跳出離開確認（警告會強制結束）→ §5.3 |
| 任何 overlay | `Esc` | 關閉最上層的 overlay，沒有其他副作用 |
| ask_user overlay | 既有的按鍵 | 所有按鍵都交給 ask 對話框 |
| Tasks view | dashboard 既有的按鍵 | 但 `q`／`Ctrl+C`／最上層的 `Esc`／`c` 依 WP-11 被攔截 |

---

## 8. Runtime 安全不變式（每個 Phase 都要成立）

1. UI 不寫入 `TodoItem.Status`，也不合成任何 terminal status；所有狀態都來自 `TasksUpdatedMsg` 與 `LastRunResult()`。
2. `RunResult.Outcome`／`GoalSatisfied` 只由 Runtime 決定；spinner、模型文字都不是完成的證據。
3. 不改動 ExecutionTarget／BackendBinding／policy snapshot；UI 沒有任何能修改這些東西的命令。
4. 開啟 Chat、重新送出同一個 command、切換 view，都不會重跑有副作用的工作；recovery 只能透過 Chat 外部既有的 `hufu resume`／`hufu session` 進行。
5. 不新增 canonical event type，也不改 `internal/team` 的 production code。
6. 顯示的內容一律經過既有的 redaction（`StatusEvent` 建立時）與 `sanitizeTerminalText`。
7. 關閉順序：Controller drain → program 結束 → display 停止 → `tc.Close()`（coordinator → MCP → lease）。
8. `--ui chat` 遇到非 TTY 時明確報錯；`--output json`、`--event-format jsonl`、`--quiet` 這些 `hufu run` 的路徑完全不受影響。

---

## 9. 測試矩陣總表

| ID | 情境 | WP |
|---|---|---|
| T01 | 舊路徑佇列滿時草稿保留並顯示提示 | 00 |
| T02 | Run／Continue 選擇規則（成功、取消、錯誤、reset） | 01 |
| T03 | 每輪不同的 invocation ID | 01／03 |
| T04 | `LastRunResult` 沒變 → `Result == nil` | 01 |
| T05 | C01～C12（Controller） | 02 |
| T06 | `--ui` 旗標、衝突、非 TTY | 10 |
| T07 | Ctrl+T 前後草稿、捲動保留；embedded dashboard 不會 quit | 11 |
| T08 | 命令判斷、`//` escape、100 KB 貼上、寬字元、busy 保留草稿 | 12 |
| T09 | 10 輪 transcript、凍結卡片、ANSI 注入、`no result` | 13 |
| T10 | 120×30／80×24／40×20／30×10／0×0、epaper 沒有 tick | 14 |
| T11 | PTY E2E：兩輪對話、取消後還能繼續、`/exit` | 15 |
| T12 | 卡片 → detail → 返回 | 20 |
| T13 | secret／ANSI 不出現在 View | 21 |
| T14 | ask_user 路由、序列化、取消、PTY attach 回歸 | 22 |
| T15 | 歷史只顯示一次、開啟時不執行、unresolved 提示 | 30 |
| T16 | 每個 slash command、`/team` 的成功／失敗／忙碌、`/cost` 一致性 | 31 |
| T17 | plain／dashboard／`hufu run` 的既有測試不需修改就通過 | 每個 Phase |

---

## 10. Commit 與 Phase 回報格式

每個 Phase 一個 commit。Commit message 範例：

```text
feat(chat): P0 turn controller, shared turn runner, prompt admission    # WP-00～03
feat(chat): P1 chat-first TUI with --ui selector                        # WP-10～15
feat(chat): P2 task workbench and ask_user integration                  # WP-20～22
feat(chat): P3 restored history, slash commands, and docs               # WP-30～32
```

Commit message body 使用下列格式記錄該 Phase 的驗收結果：

```markdown
## Phase Pn
- 已完成的 WP：…
- 與本規格不同之處，以及原因：…
- §8 Runtime invariants 是否受影響：…
- go build ./cmd/hufu: PASS/FAIL
- go test ./...: PASS/FAIL（與 baseline 的差異）
- go test -race (§0-12): PASS/FAIL
- go vet ./...: PASS/FAIL
- golangci-lint run: PASS/FAIL
- 新增的測試與對應的 T 編號：…
- BLOCKED／已知缺口：…
```

無法通過的項目標註 **BLOCKED**，並寫明原因；不得宣稱完成。

---

## 11. 完成定義（DoD）

- [ ] `hufu chat --ui chat` 可以連續進行 10 輪對話，每輪都看得到自己的回答與 outcome（T09、T11）。
- [ ] 不會靜默丟棄任何 prompt；Chat 與舊路徑都有測試（T01、T05）。
- [ ] Ctrl+C 只取消當輪，session 仍可繼續使用（T05、T11）。
- [ ] 每輪有獨立的 invocation ID，Run／Continue 規則與 plain REPL 共用同一份程式碼（T02、T03）。
- [ ] Tasks view、進度卡片、ask_user、PTY attach 都能使用（T07、T12、T14）。
- [ ] 開啟時顯示既有歷史，而且不會執行任何東西（T15）。
- [ ] §6 WP-31 表中的命令全部可用（T16）。
- [ ] 版面在各種尺寸與 epaper 下都通過測試（T10）。
- [ ] 沒有新增依賴、canonical event、`internal/team` production 修改，或 `.golangci.yml` exclusion；`cmd/hufu/chat\.go` 的 gocyclo exclusion 已移除。
- [ ] 新檔案都在 800 行以內。
- [ ] `docs/architecture/chat-tui.md`、`README.md`、`README.tw.md`、help 文字都已更新。
- [ ] §0-12 的驗收命令全部通過。

---

## 12. 風險與對策

| 風險 | 對策 |
|---|---|
| 內嵌 dashboard 的 quit／prompt-input 路徑漏攔截 | WP-11 新增 `embedded` 欄位，並為每一條離開路徑寫測試 |
| `newCoordDisplay` 每輪呼叫會截斷 `execution_trace.log` | 每個 team 只呼叫一次（WP-15）；切換 team 時重建（WP-31） |
| `LastRunResult` 是上一輪的舊結果 | 依 §4 比較 `prev` 指標 |
| 舊的 `TasksUpdatedMsg` 晚到，覆寫新一輪的卡片 | baseline 比對＋凍結（WP-13） |
| 抽出 turn runner 時改變了 plain REPL 的行為 | WP-01 先寫 characterization 測試，再重構 |
| 假 provider 無法驅動完整的 coordinator 一輪 | 只擴充測試用的假 server（WP-15）；仍無法完成時標註 BLOCKED 並回報 |
| PTY E2E 在 CI 上不穩定 | 用等待特定畫面文字（有逾時）取代固定 sleep；逾時時把畫面內容寫進測試輸出 |

---

## 13. 參考檔案（本地，`f077b622`）

`cmd/hufu/chat.go`、`cmd/hufu/chat_completer.go`、`cmd/hufu/run.go`、`cmd/hufu/signal.go`、`cmd/hufu/display.go`、`cmd/hufu/segments.go`、`cmd/hufu/team_loader.go`、`cmd/hufu/team_setup.go`、`cmd/hufu/run_identity.go`、`cmd/hufu/exploratory_delivery.go`、`cmd/hufu/prompt_input.go`、`cmd/hufu/interrupt.go`、`cmd/hufu/inspectcmd.go`、`cmd/hufu/cli_metadata.go`、`cmd/hufu/process_exit_contract_test.go`、`internal/tui/tui.go`、`internal/tui/dashboard_update.go`、`internal/tui/dashboard_view.go`、`internal/tui/ask_user.go`、`internal/tui/theme.go`、`internal/tui/sanitize.go`、`internal/tui/CONSTITUTION.md`、`internal/team/status.go`、`internal/team/run_result.go`、`internal/team/coordinator_run.go`、`internal/team/coordinator_session.go`、`internal/inspect/cost.go`、`AGENTS.md`、`CLAUDE.md`、`docs/architecture/execution-runtime.md`。
