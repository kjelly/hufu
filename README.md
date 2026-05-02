# hufu

> 透過 Ollama 協調 LLM Agent 團隊，協作完成任務的 Go CLI 工具

`hufu` 是一個以 Go 撰寫的命令列工具，能夠協調由多個 LLM Agent 組成的團隊（透過 Ollama），讓它們以分工合作的方式完成複雜任務。團隊透過名稱從設定的搜尋路徑中發現，單一 prompt 可以在多個團隊之間切換，或直接呼叫特定 Agent。

- **Module**: `github.com/anomalyco/hufu`
- **Go 版本**: 1.26.2
- **CLI 框架**: [cobra](https://github.com/spf13/cobra)
- **LLM 框架**: [charm.land/fantasy](https://charm.land/fantasy)
- **MCP 客戶端**: [github.com/mark3labs/mcp-go](https://github.com/mark3labs/mcp-go)

---

## 目錄

- [功能特色](#功能特色)
- [安裝與建置](#安裝與建置)
- [快速開始](#快速開始)
- [CLI Flags 參考](#cli-flags-參考)
- [Prompt 語法](#prompt-語法)
- [互動模式](#互動模式)
- [團隊配置 (team.yml)](#團隊配置-teamyml)
- [Agent .md 檔案格式](#agent-md-檔案格式)
- [MaxSteps 優先順序](#maxsteps-優先順序)
- [團隊目錄結構](#團隊目錄結構)
- [Skills 系統](#skills-系統)
- [Workspace 佈局](#workspace-佈局)
- [Memory 系統 (RAG)](#memory-系統-rag)
- [MCP 配置](#mcp-配置)
- [Worker Tools 參考](#worker-tools-參考)
- [Signal 處理](#signal-處理)
- [Loop 偵測](#loop-偵測)
- [Session 管理](#session-管理)
- [Idle 警告](#idle-警告)
- [配置檔 (hufu.yaml)](#配置檔-hufuyaml)
- [預設值參考](#預設值參考)

---

## 功能特色

- 🤖 **多 Agent 協作** — Coordinator 分配任務給 Worker，自動協調多輪對話
- 🔄 **多團隊切換** — 單一 prompt 中可切換不同團隊或直接呼叫特定 Agent
- 🧠 **長期記憶 (RAG)** — 透過 ChromaDB 向量搜尋，自動注入相關記憶到系統提示
- 🛠️ **16 種 Worker Tools** — 涵蓋 bash、檔案操作、遠端執行、程式碼直譯器等
- 🔌 **MCP 整合** — 支援 local 與 remote MCP server，擴充 Agent 能力
- 📋 **Skills 系統** — 可重用的技能定義，跨團隊共享
- ⚡ **Signal 控制** — Ctrl+C 優雅結束、Ctrl+Z 注入額外 prompt

---

## 安裝與建置

```bash
# 建置 binary
go build ./cmd/hufu

# 直接執行
go run ./cmd/hufu [prompt]
```

---

## 快速開始

### 1. 啟動 Ollama

確保 Ollama 已啟動並運行在預設位址：

```bash
ollama serve
```

### 2. 建立團隊

在專案根目錄建立 `.agent-teams/` 資料夾，並加入團隊定義：

```bash
mkdir -p .agent-teams/my-team
```

建立 `team.yaml`：

```yaml
name: my-team
description: "我的開發團隊"
model: ollama/qwen3:8b
temperature: "0.7"
max-tokens: "4096"
skills: code-review,git-commit
```

建立 `coordinator.md`：

```markdown
---
name: coordinator
description: "任務協調者"
role: coordinator
model: ollama/qwen3:8b
---
你是團隊的協調者，負責分配任務給團隊成員並整合結果。
```

建立 Worker Agent，例如 `developer.md`：

```markdown
---
name: developer
description: "實作專家"
role: worker
tools: view,write,edit,bash,grep,glob,ls
model: ollama/qwen3:8b
---
你是一位資深開發者，擅長撰寫高品質程式碼。
```

### 3. 執行任務

```bash
# 直接指定 prompt
go run ./cmd/hufu "重構 auth 模組的錯誤處理邏輯"

# 指定團隊
go run ./cmd/hufu --agent-team my-team "重構 auth 模組"

# 互動模式（不提供 prompt 時進入）
go run ./cmd/hufu
```

---

## CLI Flags 參考

以下是所有 10 個 CLI flags 的完整參考：

| Flag | 短旗標 | 類型 | 預設值 | 說明 |
|------|--------|------|--------|------|
| `--provider-url` | — | `string` | `http://localhost:11434/v1` | Ollama API base URL |
| `--verbose` | `-v` | `bool` | `false` | 即時顯示完整的 Agent 文字輸出 |
| `--workspace` | `-w` | `string` | `""` (cwd/workspace) | Workspace 目錄路徑 |
| `--new` | `-n` | `bool` | `false` | 封存舊 session 並重新開始 |
| `--temp` | `-t` | `bool` | `false` | 使用臨時目錄作為 workspace |
| `--agent-team` | — | `string` | `""` | 要載入的 Agent 團隊名稱 |
| `--agent-team-search-path` | — | `string` | `""` | 團隊搜尋路徑（逗號分隔），預設為 `.agent-teams/,~/.agent-teams/` |
| `--memory` | — | `bool` | `true` | 啟用長期記憶（RAG 向量搜尋） |
| `--memory-model` | — | `string` | `""` | Memory 使用的 embedding model（預設：`qwen3-embedding:4b`，覆蓋 hufu.yaml） |
| `--archive-memory` | — | `bool` | `false` | 將 session 摘要封存至 memory 後退出 |

### 使用範例

```bash
# 使用自訂 Ollama 位址
go run ./cmd/hufu --provider-url http://192.168.1.100:11434/v1 "分析程式碼"

# 啟用 verbose 模式觀察 Agent 運作
go run ./cmd/hufu -v "重構模組"

# 指定 workspace 目錄
go run ./cmd/hufu -w /path/to/project "修復 bug"

# 重新開始新 session
go run ./cmd/hufu -n "新任務"

# 使用臨時 workspace
go run ./cmd/hufu -t "快速測試"

# 指定團隊與搜尋路徑
go run ./cmd/hufu --agent-team dev-team --agent-team-search-path "./teams,~/teams" "開發功能"

# 停用 memory
go run ./cmd/hufu --memory=false "不需要記憶的任務"

# 指定 embedding model
go run ./cmd/hufu --memory-model mxbai-embed-large "分析文件"

# 封存 session 記憶後退出
go run ./cmd/hufu --archive-memory
```

---

## Prompt 語法

`hufu` 支援在 prompt 中使用特殊語法來切換團隊或呼叫特定 Agent：

| 語法 | 說明 | 範例 |
|------|------|------|
| `@<team-name> <task>` | 切換至指定團隊並委派任務 | `@research-team 調查 API 設計` |
| `@<agent-name> <task>` | 直接呼叫特定 Agent | `@developer 實作登入功能` |
| 純文字 | 傳遞給目前團隊的 coordinator | `重構 auth 模組` |

### 多團隊切換範例

你可以在單一 prompt 中切換多個團隊：

```bash
go run ./cmd/hufu "@research-team 調查 API 設計 @dev-team 實作功能 @research-team 驗證結果"
```

這會依序：
1. 切換到 `research-team` 進行調查
2. 切換到 `dev-team` 進行實作
3. 切回 `research-team` 驗證結果

---

## 互動模式

當未提供 prompt 且 stdin 為空時，`hufu` 會進入互動模式：

1. **若無法推斷團隊** — 顯示團隊選擇選單供使用者挑選
2. **選擇團隊後** — 提示使用者輸入任務描述

```bash
# 進入互動模式
go run ./cmd/hufu

# 輸出範例：
# ? 選擇團隊:
#   > my-team
#     research-team
#     dev-team
#
# ? 請輸入任務描述:
```

---

## 團隊配置 (team.yml)

團隊配置檔定義了團隊的整體行為和預設參數。以下是完整的配置參考：

```yaml
# === 必要欄位 ===
name: my-team                    # 團隊名稱（必要）

# === 選填欄位 ===
description: "我的開發團隊"       # 團隊描述

# === 執行控制 ===
max-rounds: 10                   # 最大協調輪數（預設：10）
max-steps: 30                    # Agent 預設最大步數（預設：30）
timeout: 600                     # 逾時秒數（預設：600）
max-retries: 2                   # 最大重試次數（預設：2）

# === Workspace ===
workspace: workspace             # Workspace 目錄（預設："workspace"）

# === Model 設定 ===
model: ollama/qwen3:8b           # 預設 model 名稱
temperature: "0.7"               # Temperature 值
max-tokens: "4096"               # 最大 output tokens
top-p: "0.9"                     # Top P 值
top-k: "40"                      # Top K 值

# === Skills ===
skills: code-review,git-commit    # 要包含的 skills
skills-exclude: debug             # 要排除的 skills

# === Provider ===
provider-url: http://localhost:11434/v1  # Provider URL 覆寫

# === MCP Servers ===
mcp-servers:
  filesystem:
    type: local
    command: ["npx", "-y", "@modelcontextprotocol/server-filesystem", "/path"]
  remote-api:
    type: remote
    url: "https://mcp-server.example.com/api"
    allowedTools: ["search", "query"]
```

> **注意**：`temperature`、`max-tokens`、`top-p`、`top-k` 的值在 YAML 中以字串形式表示。

---

## Agent .md 檔案格式

每個 Agent 以 Markdown 檔案定義，frontmatter 使用 YAML 格式，正文為系統提示詞：

```markdown
---
name: developer
description: Implementation specialist
role: worker
tools: view,write,edit,multiedit,bash,grep,glob,ls
skills: code-review
model: ollama/qwen3:8b
temperature: "0.7"
max-tokens: "4096"
top-p: "0.9"
top-k: "40"
timeout: 300
max-retries: 2
max-steps: 50
provider-url: http://localhost:11434/v1
---
你是一位資深開發者，擅長撰寫高品質、可維護的程式碼。
請遵循最佳實踐，並確保程式碼有適當的錯誤處理。
```

### Frontmatter 欄位說明

| 欄位 | 必要 | 預設值 | 說明 |
|------|------|--------|------|
| `name` | ✅ | — | Agent 名稱，用於 `@<name>` 呼叫 |
| `description` | ❌ | — | Agent 描述 |
| `role` | ❌ | `worker` | Agent 角色（`worker` 或 `coordinator`） |
| `tools` | ❌ | — | 可用工具列表（逗號分隔） |
| `skills` | ❌ | — | 要載入的 skills（逗號分隔） |
| `model` | ❌ | 團隊預設 | 使用的 LLM model |
| `temperature` | ❌ | 團隊預設 | Temperature 值 |
| `max-tokens` | ❌ | 團隊預設 | 最大 output tokens |
| `top-p` | ❌ | 團隊預設 | Top P 值 |
| `top-k` | ❌ | 團隊預設 | Top K 值 |
| `timeout` | ❌ | 團隊預設 | 逾時秒數 |
| `max-retries` | ❌ | `-1`（使用團隊預設） | 最大重試次數 |
| `max-steps` | ❌ | 團隊預設 | 最大執行步數 |
| `provider-url` | ❌ | 團隊預設 | Provider URL 覆寫 |

> **重要**：`max-retries` 的預設值為 `-1`，表示使用團隊預設值；若明確設定則覆寫團隊預設。

---

## MaxSteps 優先順序

當多處設定了 `max-steps` 時，以下為優先順序（由高到低）：

| 優先順序 | 來源 | 說明 |
|----------|------|------|
| 1 | `AgentConfig.MaxSteps` | 程式中的明確覆寫 |
| 2 | `AgentDef.MaxSteps` | Agent `.md` 檔案中的設定 |
| 3 | `TeamConfig.MaxSteps` | `team.yml` 中的設定 |
| 4 | `DefaultMaxSteps` | 預設值：worker 為 30，coordinator 為 20 |

---

## 團隊目錄結構

團隊檔案組織在搜尋路徑下的子目錄中。預設搜尋路徑為 `.agent-teams/` 和 `~/.agent-teams/`：

```
.agent-teams/
├── my-team/
│   ├── team.yaml              # 團隊配置
│   ├── coordinator.md         # Coordinator Agent 定義
│   ├── researcher.md          # Worker Agent 定義
│   ├── writer.md              # Worker Agent 定義
│   └── .agents/
│       └── skills/
│           └── code-review/
│               └── SKILL.md   # Skill 定義
```

### 搜尋路徑

團隊搜尋路徑由 `--agent-team-search-path` 控制：

```bash
# 預設搜尋路徑
# .agent-teams/
# ~/.agent-teams/

# 自訂搜尋路徑
go run ./cmd/hufu --agent-team-search-path "./teams,~/teams" "任務"
```

---

## Skills 系統

Skills 是可重用的技能定義，讓 Agent 能夠遵循特定的操作流程。

### SKILL.md 格式

```markdown
---
name: code-review
description: 執行系統化的程式碼審查
allowed-tools: view,grep,glob,bash
---
# Code Review

## 步驟

1. 使用 `glob` 找出所有相關的原始碼檔案
2. 使用 `view` 逐一閱讀每個檔案
3. 使用 `grep` 搜尋潛在問題模式
4. 整理發現並提出改善建議
```

### Skill 探索路徑

Skills 依序從以下路徑搜尋：

1. `<teamDir>/.agents/skills/<skill-name>/SKILL.md` — 團隊專屬 skills
2. `~/.agents/skills/<skill-name>/SKILL.md` — 全域 skills

### 在團隊中使用 Skills

```yaml
# team.yml
skills: code-review,git-commit    # 包含 skills
skills-exclude: debug             # 排除 skills
```

```markdown
---
name: developer
tools: view,write,edit,bash
skills: code-review               # Agent 層級的 skill
---
```

> **注意**：`allowed-tools` 定義了 skill 可使用的工具子集，會與 Agent 本身的 `tools` 取交集。

---

## Workspace 佈局

`hufu` 在 workspace 目錄中維護結構化的檔案系統，用於 Agent 間的通訊和狀態追蹤：

```
workspace/
├── inbox/               # 各 Agent 的任務分配
├── outbox/              # 各 Agent 的執行結果
├── shared/
│   └── skills/          # 複製的 SKILL.md 檔案
├── status/              # Agent 狀態檔案
├── history/             # 封存的 session 檔案
├── session.json         # 結構化 session 資料
└── session.md           # 人類可讀的 session 日誌
```

### 目錄說明

| 目錄/檔案 | 說明 |
|-----------|------|
| `inbox/` | 每個 Agent 的任務分配檔案 |
| `outbox/` | 每個 Agent 的執行結果檔案 |
| `shared/skills/` | 從 skill 定義複製過來的 SKILL.md |
| `status/` | Agent 狀態追蹤檔案 |
| `history/` | 封存的歷史 session 檔案 |
| `session.json` | 結構化的 session 資料（機器可讀） |
| `session.md` | 人類可讀的 session 日誌 |

---

## Memory 系統 (RAG)

`hufu` 內建長期記憶系統，使用 RAG（Retrieval-Augmented Generation）技術，讓 Agent 能夠跨 session 保留和查詢重要資訊。

### 架構

| 元件 | 說明 |
|------|------|
| **Vector Store** | ChromaDB（持久化、檔案型） |
| **Embedding** | Ollama embeddings |
| **儲存位置** | `~/.local/share/hufu/memory/<projectHash>/` |
| **預設 Embedding Model** | `qwen3-embedding:4b` |

### 運作機制

- **自動查詢**：相關的記憶會自動注入到 Agent 的系統提示中
- **封存**：Session 摘要會在結束時儲存至 memory

### Memory Tools

Agent 可使用以下兩個 memory 工具：

#### `memory_save`

儲存知識到長期記憶，供後續查詢使用。

```
memory_save(
  content: "重要的發現或決策",    # 必要
  category: "architecture"        # 選填，用於分類
)
```

#### `memory_query`

搜尋長期記憶中與查詢相關的知識。

```
memory_query(
  query: "API 設計決策",          # 必要
  n: 5,                           # 選填，回傳結果數（預設 5，最大 20）
  category: "architecture"        # 選填，分類篩選
)
```

### 配置優先順序

Memory 相關配置的優先順序：

```
CLI flag > hufu.yaml > 預設值
```

### 使用範例

```bash
# 啟用 memory（預設已啟用）
go run ./cmd/hufu --memory "分析程式碼架構"

# 停用 memory
go run ./cmd/hufu --memory=false "一次性任務"

# 指定 embedding model
go run ./cmd/hufu --memory-model mxbai-embed-large "分析文件"

# 封存 session 記憶後退出
go run ./cmd/hufu --archive-memory
```

---

## MCP 配置

`hufu` 支援 Model Context Protocol (MCP) server，讓 Agent 能夠使用外部工具和資源。在 `team.yml` 中加入 `mcp-servers` 區段：

```yaml
mcp-servers:
  filesystem:
    type: local
    command: ["npx", "-y", "@modelcontextprotocol/server-filesystem", "/path/to/dir"]
  remote-api:
    type: remote
    url: "https://mcp-server.example.com/api"
    allowedTools: ["search", "query"]
```

### MCPServerConfig 欄位

| 欄位 | 類型 | 說明 |
|------|------|------|
| `type` | `string` | `"local"` 或 `"remote"`（可從 `command`/`url` 自動推斷） |
| `command` | `[]string` | Local server 的啟動命令 |
| `environment` | `map[string]string` | 環境變數 |
| `url` | `string` | Remote server 的 URL |
| `allowedTools` | `[]string` | 工具白名單 |
| `excludedTools` | `[]string` | 工具黑名單 |
| `noOAuth` | `bool` | 停用 OAuth |

### MCP 工具命名

MCP 工具在 `hufu` 中使用以下前綴格式：

```
<serverName>__<toolName>
```

例如：`filesystem__read_file`、`remote-api__search`

### 安全限制

以下環境變數會被封鎖，無法透過 `environment` 設定：

- `LD_PRELOAD`
- `LD_LIBRARY_PATH`
- `DYLD_INSERT_LIBRARIES`
- `DYLD_LIBRARY_PATH`
- `__AFL_PRELOAD`

### Local MCP Server 範例

```yaml
mcp-servers:
  filesystem:
    command: ["npx", "-y", "@modelcontextprotocol/server-filesystem", "/home/user/projects"]
    environment:
      NODE_ENV: production
  git:
    command: ["npx", "-y", "@modelcontextprotocol/server-git", "/home/user/repo"]
```

### Remote MCP Server 範例

```yaml
mcp-servers:
  search-api:
    url: "https://search.example.com/mcp"
    allowedTools: ["search", "query", "suggest"]
  analytics:
    url: "https://analytics.example.com/mcp"
    excludedTools: ["admin_delete"]
```

---

## Worker Tools 參考

`hufu` 提供 16 種 Worker Tools，以及 4 種 Always-included Tools 和 5 種 Coordinator Tools：

### 16 種 Worker Tools

| Tool | 說明 |
|------|------|
| `bash` | 執行 bash 命令（逾時：120s，最大 600s） |
| `sudo` | 透過 sudo 以 root 權限執行命令 |
| `ssh` | 透過 SSH 在遠端主機執行命令 |
| `view` | 讀取檔案內容（含行號） |
| `write` | 寫入檔案內容，自動建立目錄 |
| `edit` | 透過替換精確文字來編輯檔案 |
| `multiedit` | 原子性地套用多個編輯操作 |
| `grep` | 使用正規表達式搜尋檔案內容 |
| `glob` | 使用 glob 模式搜尋檔案 |
| `ls` | 以樹狀結構列出目錄內容 |
| `lua` | 在沙盒中執行 Lua 程式碼 |
| `golang` | 透過 yaegi 直譯器執行 Go 程式碼 |
| `ask_user` | 向使用者提問（多選/自由文字） |
| `download` | 從 URL 下載檔案 |
| `fetch` | 取得 URL 內容（text/markdown/html） |
| `agentic_fetch` | 取得並分析 URL 內容 |

### Always-included Tools（所有 Agent 皆可使用）

| Tool | 說明 |
|------|------|
| `agent` | 委派任務給其他 Agent |
| `todo` | 管理任務清單 |
| `memory_save` | 儲存知識到長期記憶 |
| `memory_query` | 搜尋長期記憶 |

### Coordinator Tools

| Tool | 說明 |
|------|------|
| `agent` | 委派任務給 Worker Agent |
| `finish` | 結束協調流程 |
| `load_skill` | 載入 skill 定義 |
| `save_skill` | 儲存 skill 定義 |
| `ask_user` | 向使用者提問 |

### Agent .md 中指定 Tools

```markdown
---
name: developer
tools: view,write,edit,bash,grep,glob,ls
---
```

> **注意**：Always-included Tools（`agent`、`todo`、`memory_save`、`memory_query`）無需在 `tools` 中指定，會自動包含。

---

## Signal 處理

`hufu` 支援以下 Signal 操作：

| Signal | 按鍵 | 行為 |
|--------|------|------|
| `SIGINT` | `Ctrl+C` | 第一次：進入 wrap-up 模式（優雅結束）；第二次：強制退出 |
| `SIGTSTP` | `Ctrl+Z` | 透過 readline 注入額外 prompt |
| `SIGUSR1` | — | 注入額外 prompt（替代方案） |

### 使用情境

```bash
# 執行中按 Ctrl+C：
# 第一次按下 → Agent 會嘗試完成目前工作並整理結果
# 第二次按下 → 立即強制結束

# 執行中按 Ctrl+Z：
# 進入 prompt 輸入，可注入額外指示
# 例如：「請專注在效能優化上」

# 發送 SIGUSR1：
kill -USR1 <hufu-pid>
# 同樣可注入額外 prompt
```

---

## Loop 偵測

`hufu` 具備迴圈偵測機制，防止 Agent 重複委派相同任務給同一個 Agent：

- **追蹤機制**：以 `agent:task` 為 key 追蹤已委派的任務
- **偵測條件**：當相同任務被委派給同一個 Agent 多次時觸發
- **警告行為**：發出 `loop_warning` 狀態事件
- **警告內容**：包含任務描述、Agent 名稱和重複次數

---

## Session 管理

`hufu` 為每個團隊維護獨立的 session 資料：

| 情境 | 行為 |
|------|------|
| 切換團隊 | 儲存前一個團隊的 session（`SaveSessionMD`） |
| `Ctrl+C` 結束 | 儲存目前 session |
| `--new` 旗標 | 封存舊 session 並開始新的 session |
| `--temp` 旗標 | 使用臨時目錄作為 workspace |

### Session 檔案

- `session.json` — 結構化的 session 資料（機器可讀）
- `session.md` — 人類可讀的 session 日誌
- `history/` — 封存的歷史 session 檔案

---

## Idle 警告

`hufu` 內建閒置偵測機制：

- **計時器**：30 秒 idle 計時器，每次收到狀態事件時重置
- **觸發條件**：30 秒內無任何活動時，向 stderr 輸出閒置警告
- **目的**：提醒使用者 Agent 可能卡住或等待輸入

---

## 配置檔 (hufu.yaml)

`hufu` 支援 YAML 配置檔，從以下位置依優先順序載入：

| 優先順序 | 路徑 | 說明 |
|----------|------|------|
| 1 | `~/.config/hufu/hufu.yaml` | 全域配置 |
| 2 | `./hufu.yaml` | 專案配置 |

### 配置檔範例

```yaml
provider-url: http://localhost:11434/v1
embedding-model: qwen3-embedding:4b
```

### 配置優先順序

整體配置的優先順序為：

```
CLI flag > hufu.yaml > 預設值
```

---

## 預設值參考

以下是所有預設值的完整參考，包含其來源檔案：

### 一般設定

| 設定 | 預設值 | 來源 |
|------|--------|------|
| Provider URL | `http://localhost:11434/v1` | `agent.go` |
| Embedding Model | `qwen3-embedding:4b` | `config.go` |

### Agent 設定

| 設定 | 預設值 | 來源 |
|------|--------|------|
| Max Steps (workers) | 30 | `agent.go` |
| Max Steps (coordinators) | 20 | `agent.go` |
| Agent Default Role | `worker` | `parse.go` |

### 團隊設定

| 設定 | 預設值 | 來源 |
|------|--------|------|
| Team Max Rounds | 10 | `parse.go` |
| Team Timeout | 600s | `parse.go` |
| Team Max Retries | 2 | `parse.go` |

### Tool 逾時與限制

| 設定 | 預設值 | 來源 |
|------|--------|------|
| Bash Timeout | 120s | `bash.go` |
| Max Bash Timeout | 600s | `bash.go` |
| SSH Timeout | 30s | `ssh.go` |
| Lua Timeout | 120s | `lua.go` |
| Golang Timeout | 120s | `golang.go` |
| Download Timeout | 300s | `download.go` |
| Fetch Timeout | 30s | `fetch.go` |
| MCP Timeout | 30s | `manager.go` |

### Tool 輸出限制

| 設定 | 預設值 | 來源 |
|------|--------|------|
| View Limit | 2000 行 | `view.go` |
| Grep Limit | 100 筆符合 | `grep.go` |
| Glob Limit | 100 筆結果 | `glob.go` |
| LS Limit | 1000 筆項目 | `ls.go` |

---

## 完整使用範例

### 基本任務執行

```bash
# 使用預設團隊執行任務
go run ./cmd/hufu "重構 auth 模組的錯誤處理邏輯"
```

### 指定團隊與 Verbose 模式

```bash
# 指定團隊並觀察 Agent 運作過程
go run ./cmd/hufu --agent-team dev-team -v "實作使用者登入功能"
```

### 多團隊協作

```bash
# 先讓研究團隊調查，再讓開發團隊實作
go run ./cmd/hufu "@research-team 調查最佳認證方案 @dev-team 根據研究結果實作"
```

### 直接呼叫特定 Agent

```bash
# 直接呼叫 developer Agent
go run ./cmd/hufu "@developer 修復 login.go 的記憶體洩漏"
```

### 使用記憶系統

```bash
# 啟用記憶並指定 embedding model
go run ./cmd/hufu --memory --memory-model mxbai-embed-large "分析專案架構"

# 封存 session 記憶
go run ./cmd/hufu --archive-memory
```

### 使用臨時 Workspace

```bash
# 快速實驗，不影響現有 workspace
go run ./cmd/hufu -t "測試新的演算法想法"
```

### 重新開始 Session

```bash
# 封存舊 session 並開始新的
go run ./cmd/hufu -n "全新的任務"
```

### 自訂搜尋路徑

```bash
# 指定多個團隊搜尋路徑
go run ./cmd/hufu --agent-team-search-path "./teams,~/projects/teams,/opt/teams" "任務"
```

---

## 授權

請參閱專案原始碼庫中的授權檔案。

---

## 相關連結

- **Ollama**: [https://ollama.com](https://ollama.com)
- **Cobra**: [https://github.com/spf13/cobra](https://github.com/spf13/cobra)
- **ChromaDB**: [https://www.trychroma.com](https://www.trychroma.com)
- **MCP**: [https://modelcontextprotocol.io](https://modelcontextprotocol.io)