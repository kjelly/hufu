# Agent 定義規格

> Status: active
> Authority: reference
> Verified-Commit: `6ab9951`
> Supersedes: —
> Superseded-By: —

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

team.yaml 可選用 `decision-primitives:` catalog，並在 worker `tools` 明訂
`decision_primitive`。設定、授權及恢復規則見
[Agent-team DecisionPrimitives](decision-primitives.md)。這不改變既有 `decision:` contract。

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

team.yaml 的 `worker-workspace` 是所有 worker 的預設值，agent frontmatter 可以
覆寫（見下方「Worker workspace」）；coordinator 不會套用。team.yaml 的
`execution-route` 是沒有自己 `model` 與 route 的 worker 的預設 route（見下方
「Execution route」）。

### Context artifacts（context-artifacts）

選用的 team 層級區塊，預設關閉。開啟後，worker attempt 中很大的 `bash` 或
`use_dynamic_tool` 結果會完整（已 redaction）存進 artifact store，模型只收到一段
有上限的 preview 和一個不透明的 `artifact_ref`，需要時再用 `view` 的
`byte_offset`/`byte_limit` 分段讀回，不必重跑命令。

```yaml
context-artifacts:
  enabled: false                 # 預設關閉
  min-bytes: 51200               # 超過這個大小才 offload（等於 bash 既有的 50 KiB 上限）
  preview-bytes: 8192            # 模型看到的 preview 大小（開頭 3/4 + 結尾 1/4）
  max-artifact-bytes: 1048576    # 超過就維持既有的截斷行為；上限 4194304
  max-read-bytes: 32768          # 單次 view 分段讀取上限；不得超過 524288
  max-artifacts-per-attempt: 32
```

- 省略的欄位使用上面的預設值；明確寫出的值必須是正數。
- 必須滿足 `1024 <= preview-bytes < min-bytes <= max-artifact-bytes`，
  且 `max-read-bytes <= max-artifact-bytes`；不合法的區塊在 team 載入時就失敗。
- 即使結果小於 `min-bytes`，只要既有截斷會丟掉或切掉內容，而且大於
  `preview-bytes`，也會 offload。
- 生效值會固定在 execution-policy snapshot；resume 或 `hufu retry` 時設定不同
  會以 policy drift 失敗。
- worker 的工具必須包含 `view`；coordinator、structured step、closed tool
  sequence、`result-contains` 斷言所指的工具都不會 offload。

## Provider URL 優先順序

```
AgentDef.ProviderURL > TeamConfig.ProviderURL > CLI --ollama-url > 全域預設
```

透過 `config.ResolveProviderURL()` 解析。

## Agent 角色

### Coordinator

**可用工具：**
- `agent` — 委派任務給 worker
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
| `model` | ✗ | 團隊設定 | LLM 模型（執行期覆寫見下方） |
| `max-tokens` | ✗ | 團隊設定 | 最大生成 token 數 |
| `temperature` | ✗ | 團隊設定 | 生成溫度（0-1） |
| `role` | ✗ | `worker` | Agent 角色 |
| `tools` | ✗ | - | 可用工具列表 |
| `skills` | ✗ | - | 適用技能列表 |
| `timeout` | ✗ | 團隊設定 | 執行逾時（秒） |
| `max-retries` | ✗ | 團隊設定 | 最大重試次數 |
| `result-contract` | ✗ | - | 結果合約：`schema`（相對於團隊目錄的 JSON Schema 檔）與 `require-structured`（見下方） |
| `worker-workspace` | ✗ | team 設定，否則 `shared` | worker 的寫入位置：`mode: shared`，或 `mode: isolated` 搭配 `integrate: on-verified`（見下方） |
| `execution-route` | ✗ | team 設定 | 引用 hufu.yaml `execution-routes` 中的 route 名稱；不能與 `model` 同時設定（見下方） |

### 執行期模型覆寫

Worker 的 `model` 是 Git 追蹤的團隊契約；操作者要換模型時不需要改這個檔案。
執行期覆寫的優先順序（高到低）：CLI `--worker-model <agent>=<target>` >
CLI `-m/--model` > 所選 profile 的 `worker-model` 項目 > 所選 profile 的 `model`
> 本欄位 `model` > team `worker-model` > `hufu.yaml` `worker-model`。
`--worker-model` 以 agent 的 `name` 或檔名（不分大小寫）指定 worker，不能指定
coordinator/orchestrator（改用 `--coordinator-model`），也不會改變 prompt、
`role`、`tools` 或其他權限。已 admit 的任務在 retry 時保留凍結的
`ExecutionTarget`；每個 worker 的 target 也屬於 run 的 execution-policy
snapshot，因此 resume 必須沿用原 run 的相同覆寫（或相同 profile），改變 target
會因 snapshot drift 被拒絕；要換 target 請用 `--new` 開新 session。
`--plan`（plan-first）的 task 不能在 external agent backend（例如 Codex）上
規劃：這類 backend 無法呼叫 `submit_plan`，規劃的那一輪會在 plan 審查前就做完
工作，因此 admission 會以 `plan_first_external_backend` 拒絕，extra-model 的
leaf 也一樣。

### 結果合約（result-contract）

```yaml
result-contract:
  schema: schemas/code-review-v1.json   # 相對於團隊目錄
  require-structured: true
```

宣告後，worker 以 `submit_result` 的 `structured_payload` 欄位（external
agent backend 則為 `structured_payload_json`，內容是 JSON 字串）提交符合 schema
的 payload。runtime 會驗證並計算雜湊，驗證通過的 payload 會以
`structured_payload` 保存在 typed result，並附在交給 coordinator 與下游 task 的
結果中。team.yaml 的 static contract task 也可以宣告 `result-contract`，它會覆寫
該 contract 的 agent 預設值；coordinator 的 task payload 不能設定或覆寫它。

- schema 必須是自給自足的 Draft 2020-12：不允許 `$id`，`$ref` 只能指向同一份
  文件的 fragment（`#...`），`$schema` 只能出現在根節點。
- 屬性名稱若會被 durable event 的 secret redaction 改寫（例如含 `token`、
  `secret`、`api_key`），team 載入時會被拒絕；payload 的值若含 credential，
  提交時也會被拒絕。
- `require-structured: true` 時，缺少或不合法的 payload 都不能完成 task，
  free-text promotion 也不會套用；worker 可以在 result-only repair turn 補交。
- 修改 schema 會改變 execution policy snapshot，resume 時 fail closed；
  要使用新 schema 請用 `--new`。
- 不能與 `extra-models` 同時使用，也不能用在可能被 decision runtime 綁定為
  角色的 agent。
- `hufu team explain` 會列出每個 agent 綁定的 contract ID 與 hash。

### Execution route（execution-route）

route 定義在 hufu.yaml，team.yaml 與 agent frontmatter 只引用名稱：

```yaml
# hufu.yaml
execution-routes:
  coding:
    candidates:
      - ollama/qwen3:32b
      - openai/gpt-5.6-luna
    fallback-on: [rate_limited, provider_unavailable, model_unavailable]
  review:
    candidates:
      - openai/gpt-5.6-terra
```

```yaml
# agent frontmatter（或 team.yaml 的 worker 預設）
execution-route: coding
```

- route 名稱為小寫英文、數字與 `-`，以字母開頭。每個 candidate 都必須寫出
  backend（例如 `ollama/qwen3:32b`；`local/...` 會轉成 `ollama/...`），而且
  backend 必須是 language-model backend，不能是 Codex 這類 agent backend。
- 每個 worker 的優先順序：CLI `--worker-model`、CLI `-m`（兩者都會讓該 worker
  變成單一 target，忽略 route）> agent 的 `execution-route` > agent 的 `model` >
  team 的 `execution-route` > team 的 `worker-model` 或 `model`。coordinator 與
  sidecar、guard、judge、plan-reviewer 不使用 route。
- task 在 admission 時凍結 route 的名稱、digest 與 candidates，第一個 candidate
  就是 task 的 execution target。route 也會納入 execution policy snapshot：
  修改 hufu.yaml 的 route 後 resume 會 fail closed，要用 `--new`。
- 綁定 route 的 worker 不會套用 model-list 的複雜度選擇；每個 candidate 都要通過
  agent `requires` 的 model capability 檢查（已知不相容會報錯，unknown 只警告）。
- 多 candidate 的 route 必須設定 `fallback-on`，值只能是 `rate_limited`、
  `provider_unavailable`、`model_unavailable`、`transport_timeout`。attempt 因
  這些 provider 錯誤失敗、而且沒有留下會被重複的副作用時，下一個 attempt 會改用
  下一個 candidate（不占用 `max-retries`）；一般 retry 仍從第一個 candidate
  開始。完整規則見 [execution runtime](../architecture/execution-runtime.md#execution-routes-and-fallback)。
- 多 candidate 的 route 不能與 `extra-models`、`escalate`、team 的
  `escalate-on-retry`，或可能被 decision runtime 綁定為角色的 agent 同時使用；
  coordinator 也不能替這種 agent 指定其他 model。

### Worker workspace（worker-workspace）

```yaml
worker-workspace:
  mode: isolated
  integrate: on-verified
```

`isolated` 讓 worker 的每個寫入 attempt 在 control root 底下的一份專案副本
（attempt world）中執行，project（subject root）在 attempt 成功之前完全不變。
verification 在副本中執行；verification 與其他完成檢查都通過後，變更會在
`TaskDone` 之前逐 path 檢查前置條件後套用回 project，所以下游 task 看得到。
若 project 中同一個 path 已經被其他工作改掉，這次套用不寫入任何東西
（`workspace_conflict`），worker 會在目前的 project 上重試。失敗的 attempt 的
副本直接丟棄。完整語意見 [execution runtime](../architecture/execution-runtime.md#isolated-worker-workspaces)。

- 只有 `side-effect: workspace_write` 的 task 會 isolated；唯讀 task 照常在
  project 中執行。
- 需要 managed workspace（control root 與 project 不重疊）。
- 不能與 `extra-models`、external agent backend（例如 Codex）、MCP tools、
  phase workflow、`tools: all`，以及 `sudo`、`scp`、`lua`、`golang`、terminal
  系列 tool 同時使用；team 載入時就會拒絕（`workspace_isolation_unsupported`）。
- 帶 structured steps 或 action 的 task，以及 `request_agent` 的 sub-agent，
  都不能 isolated。
- 無法建立副本時（巢狀 repository 或 submodule、特殊檔案、指向 project 外的
  symlink），task 會被 block，不會退回 shared 模式。
- bash 以絕對路徑寫入 project 無法被阻擋，這不是安全沙箱。
- 鍵名採嚴格解析，拼錯的鍵會直接報錯，不會靜默退回 shared。

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
    │     ├─► 呼叫 agent — 委派任務
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

[2024-01-15T10:30:01Z] <tool_call name="agent" id="abc">...</tool_call>
[2024-01-15T10:30:02Z] <tool_result>...</tool_result>
[2024-01-15T10:30:05Z] === RESPONSE finish_reason=stop tokens_in=1500 tokens_out=250 ===
```

## 重要提示

**Agent 名稱強制匹配：**

在 `BuildOrchestratorPrompt()` 中明確提示：

```
IMPORTANT: You MUST use these exact agent names in agent: <names>. Do NOT invent or modify agent names.
```

確保 coordinator 使用正確的 agent 名稱進行委派。
