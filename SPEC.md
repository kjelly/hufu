# agent-team-cli 規格書

## 1. 專案概述

**agent-team-cli** 是一個 CLI 工具，用於協調多個 LLM Agent 團隊來共同完成任務。透過團隊名稱（而非路徑）來載入團隊，支援在同一 prompt 中跨多個團隊切換、對特定 Agent 進行直接呼叫，以及互動式團隊選擇。

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
agent-team-cli [prompt]           # prompt 可選（無 prompt 時互動式詢問）
agent-team-cli --agent-team <name> [prompt]
agent-team-cli "@<team-name> <task>"           # prompt 中指定團隊
agent-team-cli "@<team-a> do A @<team-b> do B" # 多團隊切換
```

### 4.2 指令旗標

| 旗標 | 預設值 | 說明 |
|------|--------|------|
| `--ollama-url string` | `http://localhost:11434/v1` | Ollama API URL |
| `-v, --verbose` | `false` | 即時顯示 Agent 完整輸出 |
| `-w, --workspace string` | `<cwd>/workspace` | 工作區目錄 |
| `-n, --new` | `false` | 歸檔舊 session 並開始新 session |
| `--agent-team string` | `""` | 直接指定團隊名稱（不需在 prompt 中指定） |
| `--agent-team-search-path string` | `""` | 團隊搜尋路徑（逗號分隔，預設：`./.agent-teams/`、`~/.agent-teams/`）|

### 4.3 Prompt 語法

- `@<team-name>` — 切換至指定團隊並處理後續任務
- `@<agent-name>` — 在當前團隊中直接呼叫指定 Agent
- 純文字 — 由當前團隊的協調者處理

### 4.4 互動式提示詞輸入

當未提供 prompt 且 stdin 無輸入時，程式會以互動式方式向使用者詢問團隊和任務：

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
- 工作區目錄各自獨立（每個團隊有獨立 workspace）

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

## 10. 協調流程

### 10.1 啟動序列

1. 探索團隊（`TeamRegistry.Discover`）
2. 解析 prompt（`ParsePromptWithLazyAgents`）
3. 載入所有涉及的團隊（並行）
4. 執行 segments（`executeSegments`）
5. 輸出結果

### 10.2 Session 管理

- 每個團隊有獨立的工作區，各自維護獨立的 session
- 團隊切換時自動儲存舊 session
- `--new` 參數歸檔舊 session 並開始新 session
- 中斷處理：Ctrl+C 時自動儲存當前團隊 session

## 11. 工作區結構

每個團隊的工作區獨立：

```
workspace-{team-name}/
├── inbox/
├── outbox/
├── shared/
│   └── skills/
├── status/
├── history/
├── session.json
└── session.md
```

## 12. Agent 角色與權限

| 角色 | 可用工具 | 說明 |
|------|----------|------|
| `coordinator` | `run_agents`, `finish`, `load_skill`, `ask_user` | 只能協調，不能自行執行任務 |
| `worker`（預設） | 指定的工具集 | 執行實際工作 |

## 13. 建構與發布

### 13.1 建構

```bash
go build ./cmd/agent-team-cli
```

### 13.2 CI 工作流

觸發條件：`push` 或 `pull_request` 到 `main` 分支。
執行：`go vet` → `go build` → `go test`

### 13.3 發布工作流

觸發條件：推送 `v*` 標籤。
使用 GoReleaser 自動發布至 GitHub Releases。

## 14. 限制與約束

- **路徑解析**：相對路徑以工作區目錄為基準
- **模型識別**：`ollama/` 前綴會被自動移除
- **YAML 限制**：`team.yml` 僅支援扁平結構
- **MCP 工具命名**：MCP 工具名稱前綴為 `{server}__`
- **Session 大小**：單一 session 最多保留 40 筆交換記錄
- **技能探索**：`skill.DiscoverSkills` 僅探索目錄層級（不遞迴深入）
- **輸出截断**：bash 使用尾端截断；read 使用前端截断
