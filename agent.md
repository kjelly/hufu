# Agent 定義規格

## Agent 結構

### AgentDef

```go
type AgentDef struct {
    Name        string
    Description string
    Tools       string
    Role        string
    System      string
    Skills      string
    Timeout     int64
    MaxRetries  int
    Generation  GenerationParams
    ProviderURL string
}
```

| 欄位 | 說明 |
|------|------|
| `Name` | Agent 名稱（唯一識別符） |
| `Description` | Agent 描述 |
| `Tools` | 可用工具列表（逗號分隔，`all` 表示全部） |
| `Role` | Agent 角色：`worker`（預設）或 `coordinator` |
| `System` | 系統提示詞（從 .md 檔案內容解析） |
| `Skills` | 適用技能列表（逗號分隔） |
| `Timeout` | 執行逾時（秒），優先於團隊設定 |
| `MaxRetries` | 最大重試次數，優先於團隊設定 |
| `Generation` | LLM 生成參數（model、max-tokens、temperature） |
| `ProviderURL` | Provider API URL，優先於團隊/全域設定 |

### GenerationParams

```go
type GenerationParams struct {
    Model      string
    MaxTokens  int64
    Temperature float64
}
```

## TeamConfig

```go
type TeamConfig struct {
    Name          string
    Description   string
    MaxRounds     int
    WorkspaceDir  string
    Timeout       int64
    MaxRetries    int
    Generation    GenerationParams
    Skills        string
    SkillsExclude string
    ProviderURL   string
}
```

| 欄位 | 預設值 | 說明 |
|------|--------|------|
| `Name` | - | 團隊名稱 |
| `Description` | - | 團隊描述 |
| `MaxRounds` | 10 | 最大委派回合數 |
| `WorkspaceDir` | `workspace` | 工作區目錄（相對於團隊目錄） |
| `Timeout` | 600 | Agent 預設逾時（秒） |
| `MaxRetries` | 2 | Agent 預設重試次數 |
| `Generation` | - | 預設 LLM 參數（所有 Agent） |
| `Skills` | - | 包含的技能列表 |
| `SkillsExclude` | - | 排除的技能列表 |
| `ProviderURL` | - | 預設 Provider URL |

## Provider URL 優先順序

```
AgentDef.ProviderURL > TeamConfig.ProviderURL > CLI --ollama-url > 全域預設
```

透過 `config.ResolveProviderURL()` 解析。

## Agent 角色

### Coordinator

**可用工具：**
- `run_agents` — 委派任務給 worker
- `finish` — 完成任務並返回最終答案
- `load_skill` — 載入技能完整內容
- `ask_user` — 向使用者提問

**限制：**
- 不能執行 bash、read、write 等 worker 工具
- 主要職責是協調和合成結果

### Worker

**可用工具：** 由 `tools` 欄位指定

**內建工具：**
- `bash` — 執行 shell 命令
- `read` — 讀取檔案
- `write` — 寫入檔案
- `edit` — 編輯檔案
- `grep` — 搜尋內容
- `find` — 搜尋檔案
- `ls` — 列出目錄
- `ask_user` — 向使用者提問

**技能整合：**
- Worker 的 `skills` 欄位指定適用的技能
- 技能摘要自動注入任務 prompt
- 技能完整內容可透過 `load_skill` 載入

## Agent 檔案格式

Agent 定義檔案為 Markdown + YAML Frontmatter：

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

### Frontmatter 欄位

| 欄位 | 必要 | 預設 | 說明 |
|------|------|------|------|
| `name` | ✓ | - | Agent 名稱（唯一） |
| `description` | ✓ | - | Agent 描述 |
| `model` | ✗ | 團隊設定 | LLM 模型 |
| `max-tokens` | ✗ | 團隊設定 | 最大生成 token 數 |
| `temperature` | ✗ | 團隊設定 | 生成溫度（0-1） |
| `role` | ✗ | `worker` | Agent 角色 |
| `tools` | ✗ | - | 可用工具列表 |
| `skills` | ✗ | - | 適用技能列表 |
| `timeout` | ✗ | 團隊設定 | 執行逾時（秒） |
| `max-retries` | ✗ | 團隊設定 | 最大重試次數 |

### 系統提示詞

Frontmatter 後的 Markdown 內容作為系統提示詞。

**撰寫建議：**
- 明確說明 Agent 職責和邊界
- 定義輸出格式和期望行為
- 包含領域特定知識或約束
- 保持簡潔但完整

## Agent 快取

Coordinator 使用 `agentCache` 快取已建立的 Agent 實例：

```go
type Coordinator struct {
    agentCache   map[string]fantasy.Agent
    agentCacheMu sync.RWMutex
    // ...
}
```

- 避免重複建立相同 Agent
- 使用 `agentCacheMu` 保護並發存取
- 在 `getOrCreateAgent()` 中實現 lazy initialization

## 工具選擇

### 工具過濾

```go
func FilterTools(all []fantasy.AgentTool, allowed map[string]bool) []fantasy.AgentTool
```

根據 Agent 的 `tools` 欄位過濾可用工具。

### 工具命名

- 內建工具：直接使用名稱（`bash`、`read`、`write` 等）
- MCP 工具：前綴為 `{server}__{tool}`
- 技能工具：前綴為 `skill__{name}`

## Agent 執行流程

```
coordinator.Run()
    │
    ├─► BuildOrchestratorPrompt() — 建立提示（含 agent 列表、技能）
    │
    ├─► getOrCreateAgent("coordinator") — 取得/建立 coordinator agent
    │
    ├─► fantasy.Agent.Run() — 執行對話
    │     │
    │     ├─► 呼叫 run_agents — 委派任務
    │     │     │
    │     │     ├─► TodoList.AddBatch() — 建立 TODO 項目
    │     │     ├─► ExecuteTasks() — 並發執行（最多 8 個）
    │     │     └─► UpdateStatus() — 更新 TODO 狀態
    │     │
    │     ├─► 呼叫 finish — 完成任務
    │     └─► 呼叫 load_skill — 載入技能
    │
    └─► 返回最終結果
```

## 對話歷史

Coordinator 維護 `conversationHistory` 以保留完整上下文：

```go
type Coordinator struct {
    conversationHistory   []fantasy.Message
    conversationHistoryMu sync.Mutex
    // ...
}
```

- 最多保留 100 筆訊息（`maxConversationHistory`）
- 在 `ContinueWithPrompt()` 中重用歷史記錄
- 使用 `conversationHistoryMu` 保護並發存取

## Wrap-up 機制

Coordinator 支援優雅終止：

```go
type Coordinator struct {
    wrapUp atomic.Int32
    // ...
}
```

- `SetWrapUp()` — 設定 wrap-up 標誌
- `IsWrapUp()` — 檢查是否請求 wrap-up
- `ExecuteTasks()` — 偵測 wrap-up 後拒絕新任務
- `ContinueWithPrompt(wrapUp=true)` — 使用 `wrapUpPromptTemplate` 強制總結

## LLM 日誌記錄

Coordinator 在執行 Agent 時記錄 LLM 對話，用於除錯和審計。

### 日誌位置

```
{workspace}/{team-name}/{agent-name}/llm.log
```

例如：
- `workspace/delegate/coordinator/llm.log`
- `workspace/delegate/researcher/llm.log`

使用三層目錄結構區分不同團隊和 Agent。

### Stream Callbacks

`runAgentWithStatusAndHistory()` 註冊以下 callbacks：

```go
AgentStreamCall{
    PrepareStep:    llmLogRequest,         // 記錄每次 step 的請求
    OnToolCall:     llmLogStreamEvent,      // 記錄工具呼叫
    OnToolResult:   llmLogStreamEvent,      // 記錄工具結果
    OnTextDelta:    writeLLMLog,            // 記錄文字輸出
    OnReasoningDelta: writeLLMLog,          // 記錄思考過程
    OnStreamFinish: llmLogStreamFinish,     // 記錄完成狀態
}
```

### 訊息格式化

`formatMessagePart()` 將 `fantasy.MessagePart` 轉換為 XML 標記：

| Part Type | 輸出格式 |
|-----------|----------|
| `ContentTypeText` | 直接文字 |
| `ContentTypeReasoning` | `<reasoning>...</reasoning>` |
| `ContentTypeToolCall` | `<tool_call name="..." id="...">...</tool_call>` |
| `ContentTypeToolResult` | `<tool_result id="...">...</tool_result>` |

### 日誌內容範例

```
[2024-01-15T10:30:00Z] === REQUEST step=1 model=qwen3:8b ===
[2024-01-15T10:30:00Z] system
You are the coordinator...
[2024-01-15T10:30:00Z] user
Build a REST API...

[2024-01-15T10:30:01Z] <tool_call name="run_agents" id="abc">...</tool_call>
[2024-01-15T10:30:02Z] <tool_result>...</tool_result>
[2024-01-15T10:30:05Z] === RESPONSE finish_reason=stop tokens_in=1500 tokens_out=250 ===
```

## 重要提示

**Agent 名稱強制匹配：**

在 `BuildOrchestratorPrompt()` 中明確提示：

```
IMPORTANT: You MUST use these exact agent names in run_agents: <names>. Do NOT invent or modify agent names.
```

確保 coordinator 使用正確的 agent 名稱進行委派。
