# hufu 規格書

## 1. 專案概述

**hufu** 是一個 CLI 工具，用於協調多個 LLM Agent 團隊來共同完成任務。透過團隊名稱（而非路徑）來載入團隊，支援在同一 prompt 中跨多個團隊切換、對特定 Agent 進行直接呼叫，以及互動式團隊選擇。

## 2. 技術棧

- **語言**: Go 1.26.2
- **CLI 框架**: `github.com/spf13/cobra`
- **LLM 框架**: `charm.land/fantasy`（Charm 的 Agent/LLM 抽象層）
- **MCP 用戶端**: `github.com/mark3labs/mcp-go`
- **樣式輸出**: `github.com/charmbracelet/lipgloss`
- **LLM Provider**: Ollama（透過 OpenAI 兼容 API）

## 3. 執行環境需求

- 運行時需有 Ollama 服務運行（預設 `http://localhost:11434/v1`）
- 系統需安裝 `rg`（ripgrep）和 `fd`（或 `find`）用於工具實現
- 建議安裝 GoReleaser 以支援發布流程

## 4. CLI 使用方式

### 4.1 基本用法

```bash
hufu [prompt]           # prompt 可選（無 prompt 時互動式詢問）
hufu --agent-team <name> [prompt]
hufu "@<team-name> <task>"           # prompt 中指定團隊
hufu "@<team-a> do A @<team-b> do B" # 多團隊切換
```

### 4.2 指令旗標

| 旗標 | 預設值 | 說明 |
|------|--------|------|
| `--ollama-url string` | `http://localhost:11434/v1` | Ollama API URL |
| `-v, --verbose` | `false` | 即時顯示 Agent 完整輸出 |
| `-w, --workspace string` | `<cwd>/workspace` | 工作區目錄 |
| `-n, --new` | `false` | 歸檔舊 session 並開始新 session |
| `-t, --temp` | `false` | 使用系統暫時目錄作為 workspace（執行後顯示路徑供重複使用） |
| `--agent-team string` | `""` | 直接指定團隊名稱（不需在 prompt 中指定） |
| `--agent-team-search-path string` | `""` | 團隊搜尋路徑（逗號分隔，預設：`./.agent-teams/`、`~/.agent-teams/`）|

### 4.3 Prompt 語法

- `@<team-name>` — 切換至指定團隊並處理後續任務
- `@<agent-name>` — 在當前團隊中直接呼叫指定 Agent
- 純文字 — 由當前團隊的協調者處理

### 4.5 Readline 整合

CLI 使用 `github.com/ergochat/readline` 封裝的 `internal/readline` 提供互動式輸入體驗：

**`PromptReader`：**

```go
type PromptReader struct {
    instance *ergoreadline.Instance
}

func NewPromptReader(historyFile string) (*PromptReader, error)
func (r *PromptReader) ReadLine(prompt string) (string, error)
func (r *PromptReader) Close() error
```

**全局讀取器管理：** `globalPromptReader` 指標儲存當前 `PromptReader` 實例。

**離開鉤子：**

```go
func exitInterrupt() { globalPromptReader.Close(); interruptExit() }
func exitError()    { globalPromptReader.Close(); errorExit() }
```

- 所有 `os.Exit(130)` 改為 `exitInterrupt()`，確保 readline 先行關閉
- 所有 `os.Exit(1)` 改為 `exitError()`
- `PromptReader` 需在程序退出前關閉以清除終端狀態

- **歷史記錄**：自動啟用，儲存於 `~/.hufu/prompt_history`（最多 1000 筆）
- **Ctrl+C / Ctrl+D**：分別觸發 `ErrInterrupt` 和 `io.EOF`
- **Fallback 機制**：若 readline 初始化失敗，自動降級至 `fmt.Scanln` 基礎輸入

### 4.6 互動式提示詞輸入

當未提供 prompt 且 stdin 無輸入時，程式會以互動式方式向使用者詢問團隊和任務（使用 readline）：

```
─── Select Team ───
  1. delegate
  2. tether
Team name or number:> 
```

## 5. 團隊探索與發現

### 5.1 團隊搜尋路徑

預設搜尋路徑：
- `./.agent-teams/`（相對於執行目錄）
- `~/.agent-teams/`（使用者家目錄）

可透過 `--agent-team-search-path` 自訂搜尋路徑。

### 5.2 TeamRegistry

`TeamRegistry` 負責探索和解析團隊：

```go
type TeamRegistry struct {
    searchPaths []string
    teams       map[string]string  // name -> absolute dir path
}
```

- `Discover()` — 掃描所有搜尋路徑，尋找包含 `team.yml` 或 `team.yaml` 的目錄
- `Resolve(name)` — 根據團隊名稱解析為絕對路徑
- `ListTeams()` — 列出所有發現的團隊名稱
- `HasTeam(name)` — 檢查團隊是否存在

### 5.3 團隊目錄結構

```
.agent-teams/
├── delegate/
│   ├── team.yaml
│   ├── coordinator.md
│   ├── researcher.md
│   ├── writer.md
│   ├── checker.md
│   └── .agents/
│       └── skills/
│           └── code-review/
│               └── SKILL.md
├── tether/
│   ├── team.yaml
│   └── ...
```

## 6. Prompt 解析與分段

### 6.1 PromptSegment 類型

```go
type PromptSegmentType string

const (
    SegmentSwitchTeam  = "switch_team"   // 切換團隊
    SegmentInvokeAgent = "invoke_agent"  // 直接呼叫 Agent
    SegmentText        = "text"          // 純文字
)

type PromptSegment struct {
    Type    PromptSegmentType
    Name    string  // 團隊名稱或 Agent 名稱
    Content string  // 任務內容
}
```

### 6.2 解析函式

- `HasAtName(s)` — 檢查字串是否包含 `@名稱` 引用
- `ParsePromptWithLazyAgents(prompt, registry, defaultTeam)` — 延遲解析：若有 `@team-name` 在 prompt 中，則識別為團隊切換；否則使用 `--agent-team` 作為預設團隊
- `ParsePrompt(prompt, registry, currentTeam, currentAgents)` — 完全解析：同時處理團隊切換和 Agent 呼叫
- `SplitSegmentByAgents(segment, registry, currentAgents)` — 將團隊段落拆分為多個 Agent 呼叫段落

### 6.3 解析流程

```
prompt
  │
  ▼
ParsePromptWithLazyAgents()
  │
  ├─ 有 @team-name？ → [switch_team: team-name, content=full-prompt]
  │
  └─ 無 @team-name 但有 --agent-team？
        → [switch_team: default-team, content=prompt]

[switch_team with content]
  │
  ▼
SplitSegmentByAgents()  ──→  [switch_team, text, invoke_agent, text, ...]
```

## 7. 多團隊執行流程

### 7.1 執行架構

```
使用者 prompt（含多個 @team-name / @agent-name）
    │
    ▼
ParsePromptWithLazyAgents() ──→ 初始 segments
    │
    ▼
載入所有涉及的團隊（TeamRegistry.Resolve → LoadTeam）
    │
    ▼
executeSegments() 逐一執行 segments
    │
    ├─ SegmentSwitchTeam ──→ coordinator.Run()
    │     (切換團隊，儲存舊團隊 session)
    │
    ├─ SegmentInvokeAgent ──→ coordinator.RunDirectAgent()
    │     (直接呼叫特定 Agent，支援合成)
    │
    └─ SegmentText ──→ coordinator.Run()
          (由當前團隊協調者處理)
    │
    ▼
收集結果，輸出至 stdout
```

### 7.2 團隊切換

- 切換團隊時自動儲存前一個團隊的 session（`SaveSessionMD`）
- CLI 輸出顯示團隊切換（`⇒ currentTeam → newTeam`）
- 工作區目錄各自獨立（每個團隊有獨立 workspace，如 `<workspace>/<team-name>/`）

### 7.3 直接 Agent 呼叫

當 prompt 包含 `@<agent-name>` 且該 Agent 存在於當前團隊時：

1. 呼叫 `coordinator.RunDirectAgent(agentName, task)`
2. 若團隊有協調者，則將結果傳給協調者合成
3. 若無協調者，直接輸出結果

### 7.4 閒置警告

30 秒無回應時自動顯示閒置警告至 stderr。

## 8. 團隊設定格式

### 8.1 team.yml

使用自訂簡化 YAML 解析器（僅支援扁平 `key: value` 結構）。

```yaml
name: my-team
description: "團隊描述"
max-rounds: 10        # 最大委派回合數，預設 10
timeout: 300           # Agent 逾時（秒），預設 600
max-retries: 2         # 最大重試次數，預設 2
model: ollama/qwen3:8b # 預設模型（所有 Agent）
workspace: workspace   # 工作區目錄，可為絕對路徑
skills: code-review,git-commit    # 包含的技能列表（可選）
skills-exclude: debug              # 排除的技能列表（可選）
```

### 8.2 Agent 定義檔案

格式為 Markdown + YAML Frontmatter：

```markdown
---
name: developer
description: 實作專家
model: ollama/qwen3:8b
max-tokens: 8192
temperature: 0.2
role: worker
tools: read,write,edit,bash,grep,find,ls
skills: code-review
timeout: 300
max-retries: 3
---
你的系統提示詞在這裡。
```

- **role**：Agent 角色，`worker`（預設）或 `coordinator`
- **tools**：可用工具列表（逗號分隔），`all` 表示全部工具
- **skills**：此 Agent 適用的技能名稱列表

### 8.3 MCP 伺服器設定

```yaml
mcp-servers:
  my-server:
    type: local        # "local" 或 "remote"，自動偵測
    command: ["npx", "-y", "@modelcontextprotocol/server-filesystem", "/path"]
```

### 8.4 技能設定

技能從以下位置自動探索：

1. `team-dir/.agents/skills/{skill-name}/SKILL.md`
2. `~/.agents/skills/{skill-name}/SKILL.md`

## 9. 內建工具

### 9.1 bash

在系統 shell 中執行命令。

```json
{"command": "ls -la", "timeout": 60}
```

- 預設逾時 120 秒，最大 600 秒
- 禁止執行的命令：shell 內建指令
- 輸出截断至最後 2000 行或 50KB

### 9.2 read

讀取檔案內容。

```json
{"path": "path/to/file.go", "offset": 1, "limit": 100}
```

- `offset`：起始行號（1-indexed）
- `limit`：最大行數
- 截断至 2000 行或 50KB

### 9.3 write

寫入檔案內容。

```json
{"path": "path/to/file.go", "content": "檔案內容"}
```

- 自動建立父目錄

### 9.4 edit

取代檔案中的文字。

```json
{"path": "file.go", "old_text": "舊文字", "new_text": "新文字"}
```

或批次編輯：

```json
{"path": "file.go", "edits": [{"old_text": "...", "new_text": "..."}]}
```

- 精確匹配 `old_text`；若無精確匹配，嘗試模糊匹配
- 多重匹配會返回錯誤
- CRLF 自動轉換為 LF

### 9.5 grep

搜尋檔案內容。

```json
{"pattern": "func.*Error", "path": ".", "glob": "*.go", "ignore_case": false, "literal": false, "context": 3, "limit": 100}
```

- 支援正規表達式（預設）和純文字（`literal: true`）
- 限制 100 個匹配結果
- 尊重 `.gitignore`

### 9.6 find

依 glob 模式搜尋檔案。

```json
{"pattern": "**/*.go", "path": ".", "limit": 1000}
```

- 預設使用 `fd`，備援使用 `find`
- 限制 1000 個結果

### 9.7 ls

列出目錄內容。

```json
{"path": ".", "limit": 500}
```

- 目錄顯示 `/` 後綴，隱藏檔案包含在內

### 9.8 ask_user

向使用者提問並等待回應。

```json
{"question": "選擇一個選項", "type": "single_choice", "options": [{"label": "選項 A"}]}
```

- **type**：`single_choice`、`multiple_choice`、`free_text`、`mixed`
- 回應格式：`{"answers": ["value"], "free_text": "text"}`
- **輸入鎖定**：使用 `tools.StdinMu` 序列化 stdin 讀取，避免與信號處理器衝突
- **狀態管理**：執行期間設定 `askUserActive` 標誌，完成後觸發 `onAskUserDone` 回調
- **CLI 整合**：ask_user 活躍時暫停 status 輸出和 TODO 顯示，完成後重新整理

### 9.9 load_skill

載入技能完整內容（僅協調者可用）。

```json
{"name": "code-review"}
```

### 9.10 finish

標記協調完成並提供最終答案（僅協調者可用）。

```json
{"response": "這是給使用者的最終答案"}
```

### 9.11 run_agents

委派任務給 worker agents（僅協調者可用）。

```json
{"tasks": [{"agent": "researcher", "task": "find bugs", "context_files": ["shared/spec.md"]}]}
```

- **agent**：必須是團隊中存在的 worker 名稱（使用 enum 限制）
- **task**：任務描述
- **context_files**：可選，從 workspace/shared/ 目錄提供上下文檔案
- **批次委派**：支援同時委派多個任務（並發執行，最多 8 個）

## 10. 協調流程

### 10.1 啟動序列

1. 探索團隊（`TeamRegistry.Discover`）
2. 解析 prompt（`ParsePromptWithLazyAgents`）
3. 載入所有涉及的團隊（並行）
4. 設定信號處理（Ctrl+Z / SIGUSR1）
5. 執行 segments（`executeSegments`）
6. 輸出結果

### 10.2 Status 輸出管理

**`lineWriter`：** 自定義輸出緩衝器，支援 ask_user 期間暫停輸出。

```go
type lineWriter struct {
    mu  sync.Mutex
    buf []string  // 暫存輸出
}

func (w *lineWriter) write(s string)
func (w *lineWriter) flush()  // 釋放暫存輸出
```

- 當 `tools.IsAskUserActive()` 為 true 時，輸出暫存至 `buf`
- ask_user 完成後，`onAskUserDone` 回調觸發 `flush()` 釋放輸出

**`taskDisplay`：** TODO 顯示管理器。

```go
type taskDisplay struct {
    mu      sync.Mutex
    w       *lineWriter
    tracker *team.TaskTracker
    lines   int
    dirty   bool  // 標記是否需要重新整理
}

func (d *taskDisplay) update()
func (d *taskDisplay) refreshIfDirty()  // 若 dirty 則重新渲染
```

- ask_user 活躍時設定 `dirty = true`，暫停渲染
- ask_user 完成後，`refreshIfDirty()` 檢查並重新渲染 TODO 顯示

**`activeStatusFlusher`：** 全域指標，供 `onAskUserDone` 回調存取。

```go
var activeStatusFlusher struct {
    mu       sync.Mutex
    w        *lineWriter
    taskDisp *taskDisplay
}

func setStatusFlusher(w *lineWriter, taskDisp *taskDisplay)
```

### 10.3 Prompt 注入（Signal 機制）

在協調者執行期間，使用者可透過信號注入額外 prompt：

**支援的信號：**
- `SIGTSTP`（Ctrl+Z）
- `SIGUSR1`

**實作架構：**

```go
type promptInjector struct {
    ch              chan string          // buffered, capacity 16
    wrapUpCh        chan struct{}        // buffered, capacity 1
    wrapUpRequested atomic.Bool           // atomic flag
    mu              sync.Mutex
    promptReader    *readline.PromptReader
}
```

- `enqueue(prompt)` — 非阻塞寫入 `ch` channel，channel 滿時丟棄
- `poll()` — 從 `ch` 和 `wrapUpCh` 讀取，支援 `select`
- `injectWrapUp()` — 設定 `wrapUpRequested` 並寫入 `wrapUpCh`
- `IsWrapUpRequested()` — 原子讀取標記
- `promptAndEnqueue()` — 鎖住 `StdinMu` 後從 stdin 讀取一行並加入佇列

**StdinMutex：** `tools.StdinMu` 是共用的 `sync.Mutex`，確保 `ask_user` 工具和信號處理器不會同時讀取 stdin。

**處理流程：**

```
使用者按下 Ctrl+Z 或收到 SIGUSR1
    │
    ▼
signal handler 觸發 injector.promptAndEnqueue()
    │
    ├─► 鎖住 StdinMu
    ├─► 檢查是否為終端機模式
    ├─► 顯示 "─── Additional Prompt ───" 提示
    ├─► 從 stdin 讀取一行
    └─► 加入 injector channel（緩衝大小 16）
              │
              ▼
每個 Segment 執行完後呼叫 runWithInjection()
    │
    ▼
injector.poll() ──► 有 prompt？ ──► Coordinator.ContinueWithPrompt()
injector.wrapUpCh ──► 優雅終止？ ──► ContinueWithPrompt(wrapUp=true)
                              │
                              └─► 無 prompt ──► 繼續執行
```

**`ContinueWithPrompt()`：** 與 `Run()` 不同，會保留並傳遞 `conversationHistory`，確保協調者有完整對話上下文。

**`projectDir` 變更：** `Coordinator` 新增 `projectDir` 欄位（`os.Getwd()`），用於設定 Agent 的 `WorkDir`。工作區路徑（`session.Workspace`）現在僅用於 session 儲存，不再作為 Agent 的工作目錄。

### 10.4 TODO 任務追蹤系統

`TaskTracker` 內建 `TodoList`，提供結構化的任務追蹤：

```go
type TodoItem struct {
    ID     string      // 自動遞增 ID（如 "1", "2", "3"）
    Agent  string      // 負責的 Agent 名稱
    Desc   string      // 任務描述
    Status TaskStatus  // TaskPending / TaskInProgress / TaskDone / TaskError
    Detail string      // 錯誤詳情（當 Status == TaskError 時）
}

type TodoList struct {
    mu    sync.Mutex
    items []*TodoItem
    next  int  // 下一個 ID 計數器
}
```

**核心方法：**

| 方法 | 說明 |
|------|------|
| `AddBatch([]{Agent, Desc})` | 批次新增任務，返回 `[]*TodoItem`（含自動 ID） |
| `UpdateStatus(id, status, detail)` | 更新指定 ID 的任務狀態 |
| `Items()` | 返回所有任務的副本（thread-safe） |
| `Clear()` | 清空所有任務並重置 ID 計數器 |

**狀態流程：**
```
AddBatch() → TaskPending → UpdateStatus(TaskInProgress) → TaskDone
                                          ↓
                                     TaskError
```

**Coordinator 整合：**

- `ExecuteTasks()` 呼叫 `TodoList.AddBatch()` 為每個委派任務建立 TODO 項目
- `executeTask()` 和 `RunDirectAgent()` 在生命週期的每個階段呼叫 `UpdateStatus()`
- 每當 TODO 狀態變更時，回報 `StatusEvent{Type: "todos_updated", Todos: ...}` 事件
- CLI 接收 `todos_updated` 事件後重新渲染 TODO 顯示

**CLI 顯示格式：**
```
─── TODO ───
  ◑ 1. researcher find bugs
  ○ 2. writer write docs
  ● 3. checker verify tests
  ✗ 4. researcher attempt 1 failed: ...
```

### 10.5 Session 管理

- 每個團隊有獨立的工作區，各自維護獨立的 session
- 團隊切換時自動儲存舊 session
- `--new` 參數歸檔舊 session 並開始新 session
- 中斷處理：Ctrl+C 時自動儲存當前團隊 session

## 12. 工作區結構

每個團隊的工作區位於 `<workspace>/<team-name>/`（由 `--workspace` 參數與團隊名稱組合）：

```
<workspace>/
└── {team-name}/
    ├── inbox/
    ├── outbox/
    ├── shared/
    │   └── skills/
    ├── status/
    ├── history/
    ├── session.json
    └── session.md
```

**`CleanRunDirs()`：** `--new` 時自動清理 `inbox/`、`outbox/`、`status/` 目錄，保留 `shared/`、`history/`、`session.json`、`session.md`。

### 12.1 優雅關機（Graceful Shutdown）

支援 Ctrl+C 兩階段終止：

****第一次 Ctrl+C：** 發送 `SIGINT` → 呼叫 `SetWrapUp()` 設定 `wrapUp=1` 並回報 `StatusEvent{Type: "wrap_up"}` → CLI 顯示 `─── WRAP UP ───` 提示 → `ExecuteTasks` 偵測 `IsWrapUp()` 後拒絕委派新任務並回傳錯誤 → 協調者收到錯誤後呼叫 `ContinueWithPrompt("")` 使用 `wrapUpPromptTemplate` 指示立即總結並 finish

**第二次 Ctrl+C：** 強制取消 context (`cancel()`) → 立即退出

**相關常數：**

```go
wrapUpPromptTemplate = "The user has requested that you wrap up immediately. IMPORTANT: Do NOT delegate any new tasks. Immediately summarize what has been accomplished so far based on all results you have received. Call the finish tool RIGHT NOW..."
```

**`activeCoordinator`**：儲存當前活躍的協調者指標，供 signal handler 呼叫 `SetWrapUp()`。

## 12.2 LLM 日誌記錄

`internal/team/llm_log.go` 提供 LLM 對話記錄功能，用於除錯和審計。

### 資料流

```
Agent.Run() → AgentStreamCall callbacks → llm_log functions → workspace/{agent-name}/llm.log
```

### 主要函式

| 函式 | 說明 |
|------|------|
| `llmLogRequest(workspace, agentName, opts)` | 記錄每個 step 的請求（messages、model、step number） |
| `llmLogStreamEvent(workspace, agentName, eventType, content)` | 記錄 streaming 事件（tool_call、tool_result） |
| `llmLogStreamFinish(workspace, agentName, finishReason, usage)` | 記錄完成原因和 token 用量 |
| `writeLLMLog(workspace, agentName, entry)` | 寫入單一 log 項目到檔案 |

### 輸出格式

```
[2024-01-15T10:30:00Z] === REQUEST step=1 model=qwen3:8b ===
[2024-01-15T10:30:00Z] user
<tool_call name="bash" id="abc123">{"command": "ls"}</tool_call>

[2024-01-15T10:30:01Z] <tool_call>...</tool_call>
[2024-01-15T10:30:02Z] <tool_result>...</tool_result>
[2024-01-15T10:30:05Z] === RESPONSE finish_reason=stop tokens_in=1500 tokens_out=250 ===
```

### Stream Callbacks

`runAgentWithStatusAndHistory()` 註冊以下 callbacks：

```go
AgentStreamCall{
    PrepareStep:   llmLogRequest,           // 記錄請求
    OnToolCall:    llmLogStreamEvent,        // 記錄工具呼叫
    OnToolResult:  llmLogStreamEvent,        // 記錄工具結果
    OnTextDelta:   writeLLMLog,              // 記錄文字輸出
    OnReasoningDelta: writeLLMLog,           // 記錄思考過程
    OnStreamFinish: llmLogStreamFinish,      // 記錄完成狀態
}
```

### 訊息格式化

`formatMessagePart()` 將 `fantasy.MessagePart` 轉換為 XML 標記：

- `ContentTypeText` → 直接輸出文字
- `ContentTypeReasoning` → `<reasoning>...</reasoning>`
- `ContentTypeToolCall` → `<tool_call name="..." id="...">...</tool_call>`
- `ContentTypeToolResult` → `<tool_result id="...">...</tool_result>`

## 13. StatusEvent 結構

```go
type StatusEvent struct {
    Type       string     // "start","step","tool_call","tool_result","done","error","text","todos_updated","wrap_up"
    TeamName   string     // 團隊名稱（新增）
    Agent      string
    Message    string
    ToolName  string
    ToolArgs  string
    ToolResult string
    Step      int
    Todos     []*TodoItem
}
```

使用 builder 模式鏈式呼叫：`c.newEvent("start").withAgent(name).withMessage(msg)`

## 14. Agent 角色與權限

| 角色 | 可用工具 | 說明 |
|------|----------|------|
| `coordinator` | `run_agents`, `finish`, `load_skill`, `ask_user` | 只能協調，不能自行執行任務 |
| `worker`（預設） | 指定的工具集 | 執行實際工作 |

## 15. 建構與發布

### 13.1 建構

```bash
go build ./cmd/hufu
```

### 13.2 CI 工作流

觸發條件：`push` 或 `pull_request` 到 `main` 分支。
執行：`go vet` → `go build` → `go test`

### 13.3 發布工作流

觸發條件：推送 `v*` 標籤。
使用 GoReleaser 自動發布至 GitHub Releases。

## 16. 限制與約束

- **路徑解析**：相對路徑以工作區目錄為基準
- **模型識別**：`ollama/` 前綴會被自動移除
- **YAML 限制**：`team.yml` 僅支援扁平結構
- **MCP 工具命名**：MCP 工具名稱前綴為 `{server}__`
- **Session 大小**：單一 session 最多保留 40 筆交換記錄（`maxSessionEntries`）
- **對話歷史**：`conversationHistory` 最多保留 100 筆訊息（`maxConversationHistory`），由 `conversationHistoryMu` 保護
- **並發限制**：`ExecuteTasks` 同時最多執行 8 個任務（`maxConcurrentTasks`）
- **MCP 逾時**：`ExecuteTool` 預設 30 秒逾時（`mcpDefaultTimeout`），ctx 有 deadline 時優先使用
- **所有任務失敗**：`ExecuteTasks` 若 `successCount == 0` 且有任務結果時，回傳錯誤而非空結果
- **@名稱匹配**：`atNamePattern` 為 `\B@([\w][\w-]*)`，需 @ 前一個字元為非單詞邊界，避免匹配 email 位址
- **Agent 檔案解析警告**：`parseAgentFile` 在失敗時輸出警告至 stderr（無效檔案不中斷載入）
- **技能探索**：`skill.DiscoverSkills` 僅探索目錄層級（不遞迴深入）
- **輸出截断**：bash 使用尾端截断；read 使用前端截断
- **工作區隔離**：每個團隊的工作區為 `<workspace>/<team-name>/`，`CleanRunDirs` 僅清理 inbox/outbox/status
- **ask_user 輸出管理**：ask_user 活躍時暫存 status 輸出和 TODO 顯示，完成後重新整理
- **LLM 日誌隔離**：每個 agent 的 log 位於 `workspace/{agent-name}/llm.log`，自動建立目錄
- **LLM reasoning 記錄**：支援記錄模型的思考過程（`<reasoning>` 標籤）
