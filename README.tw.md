# hufu

> ⚠️ **Fork notice** — This repository (`kjelly/hufu`) is a personal fork of [`anomalyco/hufu`](https://github.com/anomalyco/hufu). The Go module path remains `github.com/anomalyco/hufu` so existing consumers keep resolving dependencies. Releases are published from this fork via GoReleaser.


> 透過 Ollama 協調 LLM Agent 團隊，協作完成任務的 Go CLI 工具

`hufu` 是一個以 Go 撰寫的命令列工具，能夠協調由多個 LLM Agent 組成的團隊（透過 Ollama），讓它們以分工合作的方式完成複雜任務。團隊透過名稱從設定的搜尋路徑中發現，單一 prompt 可以在多個團隊之間切換，或直接呼叫特定 Agent。

- **Module**: `github.com/anomalyco/hufu` (fork at `github.com/kjelly/hufu`)
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
- [團隊配置](#團隊配置)
- [Agent .md 檔案格式](#agent-md-檔案格式)
- [MaxSteps 優先順序](#maxsteps-優先順序)
- [團隊目錄結構](#團隊目錄結構)
- [Skills 系統](#skills-系統)
- [Workspace 佈局](#workspace-佈局)
- [Memory 系統 (RAG)](#memory-系統-rag)
- [MCP 配置](#mcp-配置)
- [多 Provider 支援](#多-provider-支援)
- [Sidecar 系統](#sidecar-系統)
- [Guard 系統](#guard-系統)
- [Worker Tools 參考](#worker-tools-參考)
- [Signal 處理](#signal-處理)
- [Loop 偵測](#loop-偵測)
- [Session 管理](#session-管理)
- [TUI 模式](#tui-模式)
- [Dry Run 模式](#dry-run-模式)
- [Plan-First 模式](#plan-first-模式)
- [報表產生](#報表產生)
- [Idle 警告](#idle-警告)
- [配置檔 (hufu.yaml)](#配置檔-hufuyaml)
- [預設值參考](#預設值參考)

---

## 功能特色

- 🤖 **多 Agent 協作** — Coordinator 分配任務給 Worker，自動協調多輪對話
- 🔄 **多團隊切換** — 單一 prompt 中可切換不同團隊或直接呼叫特定 Agent
- 🧠 **長期記憶 (RAG)** — 透過 chromem-go 向量搜尋，自動注入相關記憶到系統提示
- 🛠️ **18 種 Worker Tools** — 涵蓋 bash、檔案操作、遠端執行、程式碼直譯器、亂數、數學運算等
- 🔌 **MCP 整合** — 支援 local 與 remote MCP server，擴充 Agent 能力
- 📋 **Skills 系統** — 可重用的技能定義，跨團隊共享，支援自動技能偵測
- 🎯 **Sidecar 系統** — 輕量輔助 LLM，用於技能比對與 Guard 審核
- 🛡️ **Guard 系統** — 規則導向的輸出審核（例如：要求測試、禁止不雅內容）
- ⚡ **Signal 控制** — Ctrl+C 優雅結束、Ctrl+Z 注入額外 prompt
- 📺 **TUI 模式** — 即時 Bubble Tea 終端 UI，追蹤任務執行狀態
- 🔍 **Dry Run 模式** — 預覽執行計畫，不實際執行 Agent
- 📝 **Plan-First 模式** — 強制要求 Agent 先提交計畫再執行
- 📊 **報表產生** — 產生完整的 markdown 執行報表
- 🌐 **SSH 工具** — 增強會話管理、錯誤診斷、SCP 支援、SSH 配置整合、連接複用和審計日誌

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

建立 `team.yaml`（此檔案為選擇性；若未建立，會以目錄名稱作為 team 名稱）：

```yaml
name: my-team
description: "我的開發團隊"
model: ollama/qwen3:8b
temperature: "0.2"
max-tokens: "16384"
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

| Flag | 短旗標 | 類型 | 預設值 | 說明 |
|------|--------|------|--------|------|
| `--provider-url` | — | `string` | "" (hufu.yaml 或 `http://localhost:11434/v1`) | Ollama 或 OpenAI-compatible API base URL |
| `--provider-api-key` | — | `string` | "" | Provider API key |
| `--verbose` | `-v` | `bool` | `false` | 即時顯示完整的 Agent 文字輸出 |
| `--workspace` | `-w` | `string` | `""` (`<cwd>/workspace`) | Workspace 目錄路徑 |
| `--new` | `-n` | `bool` | `false` | 封存舊 session 並重新開始 |
| `--temp` | `-t` | `bool` | `false` | 使用臨時目錄作為 workspace |
| `--steps` | `-s` | `bool` | `false` | 在執行每批 Worker 任務前暫停並要求確認 |
| `--agent-team` | — | `string` | `""` | 要載入的 Agent 團隊名稱 |
| `--agent-team-search-path` | — | `string` | `""` | 團隊搜尋路徑（逗號分隔），預設為 `.agent-teams/,~/.agent-teams/` |
| `--memory` | — | `bool` | `false` | 啟用長期記憶（RAG 向量搜尋） |
| `--memory-model` | — | `string` | `""` | Memory 使用的 embedding model（預設：`ollama/nomic-embed-text:latest`） |
| `--archive-memory` | — | `bool` | `false` | 將 session 摘要封存至 memory 後退出 |
| `--show-history` | — | `bool` | `false` | 恢復時顯示先前的 session 歷史 |
| `--dry-run` | — | `bool` | `false` | 不呼叫 LLM 的預覽，列出技能比對與可用 agents（不執行 agent） |
| `--tui` | — | `bool` | `false` | 顯示 Bubble Tea TUI 即時任務追蹤 |
| `--rbash` | — | `bool` | `false` | 對 bash tool 使用 restricted bash (rbash) |
| `--no-net` | — | `bool` | `false` | 封鎖 Agent 子程序的所有網路存取 |
| `--force-mcp` | — | `bool` | `false` | 強制 MCP 模式：停用內建執行/網路工具（bash, sudo, ssh, golang, lua, download, fetch, agentic_fetch），需使用 MCP servers |
| `--direnv` | — | `bool` | `false` | 為 bash tool 載入 `.envrc` / `.env` 環境 |
| `--think` | — | `bool` | `false` | 顯示 Coordinator 決策推理 |
| `--plan` | — | `bool` | `false` | 強制 plan-first 模式：Agent 必須先提交計畫 |
| `--auto-skills` | — | `bool` | `false` | 啟用 sidecar / LLM 自動技能偵測 |
| `--report` | — | `bool` | `false` | 產生完整的 markdown 執行報表 |
| `--default` | — | `bool` | `false` | 使用內建預設團隊（coordinator + Helper）；不需要 `.agent-teams/` 目錄（與 `--agent-team` 互斥）。會自動探索目前目錄 `.agents/skills/` 與 `~/.agents/skills/` 的技能並支援 `--skill` 強制載入。 |
| `--helper-tools` | — | `string` | `""` | 為預設 Helper worker 啟用額外的工具列表（逗號分隔），需搭配 `--default` 使用（例如 `bash` 或 `bash,sudo,ssh`）。會自動 trim 空白，忽略空項目。空字串 = 預設唯讀工具集。 |
| `--auto-approve` | — | `bool` | `false` | 自動選擇 `ask_user` 中明顯安全的選項；危險或不明確的選項仍會詢問使用者 |
| `--model` | — | `string` | `""` | 覆寫目前團隊的預設模型（最高優先權） |
| `--temperature` | — | `string` | `""` | 覆寫取樣溫度 |
| `--max-tokens` | — | `string` | `""` | 覆寫最大輸出 token 數 |
| `--top-p` | — | `string` | `""` | 覆寫 top-p 值 |
| `--top-k` | — | `string` | `""` | 覆寫 top-k 值 |
| `--sidecar-model` | — | `string` | `""` | 覆寫用於技能配對的 sidecar 模型（未指定時 fallback 到 `--model`） |
| `--guard-model` | — | `string` | `""` | 覆寫用於輸出審查的 guard 模型（未指定時 fallback 到 `--model`） |
| `--timeout` | — | `int64` | `0` | 覆寫 agent / coordinator 的 timeout（秒），例如 `1800` 表示 30 分鐘。`0` = 使用 team / agent 預設值。 |
| `--fix` | — | `string` | `""` | 分析前次執行資料並提出改善建議 |
| `--skill` | — | `[]string` | `nil` | 強制載入特定 skill（可重複） |
| `--var` | — | `[]string` | `nil` | 設定模板變數 `key=value`（可重複） |
| `--var-file` | — | `[]string` | `nil` | 從檔案讀取模板變數（可重複） |

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

# 啟用 memory
go run ./cmd/hufu --memory "需要記憶的任務"

# 指定 embedding model
go run ./cmd/hufu --memory-model mxbai-embed-large "分析文件"

# 封存 session 記憶後退出
go run ./cmd/hufu --archive-memory

# TUI 模式
go run ./cmd/hufu --tui "重構 auth 模組"

# Dry-run 預覽
go run ./cmd/hufu --dry-run "重構模組"

# Plan-first 模式
go run ./cmd/hufu --plan "實作功能"

# 產生報表
go run ./cmd/hufu --report "建構功能"

# 逐步確認
go run ./cmd/hufu -s "重構模組"
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

### 進階 Agentic 模式 (Advanced Patterns)

`hufu` 的設計理念是保持核心引擎的輕量，並透過靈活的語法與工具組合來達成複雜的 Agentic 工作流。搭配 `create_skill` 工具與 Prompt Chaining (`|` 串聯)，你可以輕鬆實現以下進階模式：

#### 1. 動態階層委派 (Hierarchical Delegation)
允許 Agent 在執行過程中，動態地委派子任務或生成專屬的助手。
與其讓 Coordinator 在一開始就把所有計畫寫死，你可以指示 Agent 遇到困難時自行發布子任務：
```bash
go run ./cmd/hufu "@coder 你正在開發一項複雜的功能。如果需要特定領域的協助，請使用 create_skill 工具寫一個包含 'hufu @specialist' 指令的 shell script，然後執行它來獲取答案。"
```
這使得 Agent 具備了遞迴呼叫與動態委派的能力。

#### 2. 多 Agent 辯論與交叉審查 (Multi-Agent Debate)
對於高風險任務（如程式碼審查、架構設計），你可以強迫兩個不同的 Agent 進行辯論，直到得出最佳解。
透過 Prompt Chaining 語法 (`|`) 與 `{{PREV_RESULT}}` 變數，可以輕鬆串聯出一個辯論迴圈：
```bash
go run ./cmd/hufu "@generator 請提出一個系統架構方案 | @auditor 嚴厲地審查上一個人的提案並指出漏洞：{{PREV_RESULT}} | @generator 針對審查意見：{{PREV_RESULT}}，請提出修正後的最終架構"
```

---

## 常見使用情境

### 1. 使用多模型評委進行 Code Review

當你有多個模型可供使用時，可以使用 judge 來挑選最佳的程式碼審查結果：

```yaml
# team.yaml
model-list:
  - name: qwen3:1b
    provider: ollama
  - name: qwen3:8b
    provider: ollama
judge-model: qwen3:1b  # 用較便宜的模型進行評判
```

```bash
# 執行 code review 並使用多個模型，評委會挑選最佳結果
go run ./cmd/hufu --agent-team code-review "Review the authentication module"
```

### 2. 使用 Skeptic 進行高風險決策

針對關鍵決策，使用對抗性驗證 (adversarial verification) 來質疑結果：

```bash
# 在 prompt 中明確要求 coordinator 啟用對抗性驗證
go run ./cmd/hufu --agent-team arch-team \
  "Design the database schema. 請確保結果經過 3 位 skeptic (評審) 嚴格驗證。"
```

### 3. 對於頑固任務進行升級重試

啟用升級機制，當任務需要更強大的模型進行重試時：

```yaml
# team.yaml
escalate-on-retry: true
model-list:
  - name: qwen3:1b
    provider: ollama
  - name: qwen3:8b
    provider: ollama
  - name: qwen3:30b
    provider: ollama
```

### 4. 使用 Shell 指令驗證交付成果

確保任務產生實際的產出，而不僅僅是宣稱完成：

```bash
# 在 prompt 中提供明確的指令與驗證條件
go run ./cmd/hufu --agent-team dev-team \
  "Implement the login feature. 請設定 verify 指令為 'go test ./tests/login/...' 來確保它能運作。"
```

### 5. 盲目重試時的 Reflexion (反思)

即使沒有 sidecar，重試時也會獲得結構化的提示：

```bash
# Reflexion 在沒有 sidecar 時也能運作 - 具有確定性的錯誤分類
go run ./cmd/hufu --agent-team dev-team "Implement complex feature"
# 如果失敗，reflexion 會進行分類：timeout / missing file / permission error
```

### 6. 具備記憶的開發 (Memory-Augmented)

在不同 session 之間保留學習到的經驗：

```bash
# 啟用 memory 以搜尋過去的 session
go run ./cmd/hufu --memory "Refactor the API layer"

# 之後的 session 可以查詢相關記憶
go run ./cmd/hufu --memory "How did we handle auth errors previously?"
```

### 7. 無人值守的批次處理 (Unattended)

執行不需要人工監控的作業：

```bash
# 完整的 unattended 設定
go run ./cmd/hufu \
  --unattended \
  --max-duration 3600 \
  --max-total-tokens 500000 \
  --no-journal \
  --profile batch \
  --agent-team pipeline \
  "Process all pending pull requests"
```

### 8. 複雜功能的計畫優先 (Plan-First) 模式

要求 Agent 在採取行動前必須先提交計畫：

```bash
# Agent 必須先提出計畫
go run ./cmd/hufu --plan "Implement a distributed caching layer"
```

### 9. 安全探索的 Dry Run 模式

預覽會發生的事情而不會實際執行：

```bash
# 不會呼叫 LLM，也不會執行 Agent - 純預覽
go run ./cmd/hufu --dry-run --agent-team dev-team "Refactor the auth module"
```

### 10. 確保程式碼品質的 Guardrails

為每個 Agent 加上輸出的防護欄：

```markdown
# researcher.md
---
name: researcher
guard:
  - require-tests
  - no-profanity
---
```

### 11. 用於即時監控的 TUI 介面

即時監看任務的進度：

```bash
# TUI 模式會顯示任務看板、日誌、技能使用情況
go run ./cmd/hufu --tui "Implement the new feature"
```

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

## 團隊配置

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
max-concurrent: 8                # 最大並行 Worker 任務數（預設：8）

# === Workspace ===
workspace: workspace             # Workspace 目錄（預設："workspace"）

# === Model 設定 ===
model: ollama/qwen3:8b           # 預設 model 名稱
temperature: "0.2"               # Temperature 值
max-tokens: "16384"               # 最大 output tokens
top-p: "0.9"                     # Top P 值
top-k: "40"                      # Top K 值

# === Provider ===
provider-url: http://localhost:11434/v1  # Provider URL 覆寫
provider-api-key: ""                      # Provider API key 覆寫

# === 多 Provider 池 ===
providers:
  openai:
    url: https://api.openai.com/v1
    key: $OPENAI_API_KEY
    models: [gpt-4o, gpt-4-turbo]
    aliases:
      gpt-4: gpt-4o

# === Model 清單 ===
model-list:
  - name: qwen3:8b
    provider: ollama
  - name: gpt-4o
    provider: openai

# === Sidecar / Guard 模型 ===
sidecar-model: qwen3:1b          # 輕量模型，用於技能比對
guard-model: qwen3:8b            # Guard / 審核用模型

# === Skills ===
skills: code-review,git-commit  # 要包含的 skills
skills-exclude: debug            # 要排除的 skills
auto-skills: false              # 啟用自動技能偵測

# === 安全性 ===
allowed-paths: ["/home/user/projects", "/tmp"]  # 允許的檔案路徑
restricted-path: "/etc"                           # 限制的檔案路徑
no-net: false                                     # 封鎖網路存取
force-mcp: false                                  # 強制 MCP 模式
auto-approve: false                               # 自動選擇明顯安全的 ask_user 選項
shell: bash                                       # MCP tools 預設 shell（從 PATH 搜尋）

# === 模板變數 ===
vars:
  project_name: "hufu"
  author: "anomalyco"

# === 通知 ===
notify:
  type: webhook
  url: "https://hooks.example.com/agent"

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
temperature: "0.2"
max-tokens: "16384"
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
| `shell` | ❌ | 團隊預設 | Agent 的 MCP tools 預設 shell（例如 `bash`、`zsh`、`nu` 或完整路徑） |
| `mcp-tools` | ❌ | — | 自訂 MCP tools（dict 格式：`{tool-name: {cmd, desc, inputs, shell, dir}}`） |

> **重要**：`max-retries` 的預設值為 `-1`，表示使用團隊預設值；若明確設定則覆寫團隊預設。

### MCP Tools (`mcp-tools`)

為 Agent 定義自訂 MCP tools。每個 tool 執行 shell 命令並支援參數替換：

```yaml
mcp-tools:
  run-tests:
    cmd: go test ./...
    desc: 執行 Go 測試
    inputs: [package]

  build:
    cmd: go build -o /tmp/app ./...
    desc: 構建應用程式

  calc:
    cmd: print ($env.V1 + $env.V2)
    desc: 使用 nushell 計算總和
    inputs: [a, b]
    shell: nu
```

**參數映射：**
- **命名參數**：`url` → `$URL` 環境變數
- **位置參數**：第 1 個輸入 → `$V1`，第 2 個 → `$V2`，依此類推
- **Shell 優先順序**：`tool.shell` > `agent.shell` > `team.shell` > `hufu.yaml shell` > `bash`（預設）

**支援的 shells：** `bash`、`sh`、`zsh`、`fish`、`nu`（nushell），或 PATH 中的任何 shell。

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

1. `<teamDir>/skills/<skill-name>/SKILL.md` — 團隊專屬 skills（從 `<teamDir>/.agents/skills/` 更改）
2. `<current-directory>/.agents/skills/<skill-name>/SKILL.md` — 專案 skills
3. `~/.agents/skills/<skill-name>/SKILL.md` — 全域 skills（不變）

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
├── tasks/               # 統一的任務檔案（合併 inbox + outbox）
│   └── {team-name}/
│       └── {agent-name}/
│           └── {timestamp}.md
├── compaction_history.json # Structured compaction 摘要歷史（13 個區段）
├── shared/
│   └── skills/          # 複製的 SKILL.md 檔案
├── status/              # Agent 狀態檔案
├── history/             # 封存的 session 檔案
├── session.json         # 結構化 session 資料
├── chat_history.md      # 人類可讀的對話歷史紀錄
└── execution_trace.log  # 詳細的執行軌跡日誌
```

### 目錄說明

| 目錄/檔案 | 說明 |
|-----------|------|
| `tasks/{team-name}/{agent-name}/` | 每個 Agent 的任務檔案（含任務描述、狀態、結果） |
| `compaction_history.json` | Structured compaction 摘要歷史（13 個區段） |
| `shared/skills/` | 從 skill 定義複製過來的 SKILL.md |
| `status/` | Agent 狀態追蹤檔案 |
| `history/` | 封存的歷史 session 檔案 |
| `session.json` | 結構化的 session 資料（機器可讀） |
| `chat_history.md` | 人類可讀的對話歷史紀錄 |
| `execution_trace.log` | 詳細的執行軌跡日誌（僅在 TUI 模式下產生） |

---

## Memory 系統 (RAG)

`hufu` 內建長期記憶系統，使用 RAG（Retrieval-Augmented Generation）技術，讓 Agent 能夠跨 session 保留和查詢重要資訊。

### 架構

| 元件 | 說明 |
|------|------|
| **Vector Store** | chromem-go（程序內、檔案型） |
| **Embedding** | Ollama embeddings |
| **儲存位置** | `~/.local/share/hufu/memory/<projectHash>/` |
| **預設 Embedding Model** | `ollama/nomic-embed-text:latest` |

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
# 啟用 memory
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

## 多 Provider 支援

`hufu` 可同時支援多個 LLM Provider，透過 `team.yml` 中的 `providers` 欄位設定：

```yaml
providers:
  openai:
    url: https://api.openai.com/v1
    key: $OPENAI_API_KEY
    models: [gpt-4o, gpt-4-turbo]
    aliases:
      gpt-4: gpt-4o
  local:
    url: http://localhost:11434/v1
    key: ollama
    models: [qwen3:8b]
```

Agent 可在 `.md` frontmatter 中個別指定 `provider-url` 與 `provider-api-key`，或使用團隊預設值。

---

## Sidecar 系統

Sidecar 是輕量的輔助 LLM，用於不應消耗主 model context window 的任務。

### 使用情境

- **技能比對** — 將 prompt 配對到相關 skills
- **Guard 審核** — 審核 Agent 輸出是否符合 guard 規則
- **計畫審核** — 執行 multi-step 任務前的自主計畫審核

### 配置

```yaml
sidecar-model: qwen3:1b   # 輕量模型，用於技能比對
guard-model: qwen3:8b      # Guard / 審核用模型
```

---

## Guard 系統

Guard 系統審核 Agent 輸出是否符合可配置規則。規則透過 `.md` frontmatter 中的 `guard` 欄位定義：

```yaml
guard:
  - require-tests
  - no-profanity
```

當 guard 被觸發時，輸出會傳給 `guard-model` sidecar 進行審核。若審核失敗，Agent 會被要求修正輸出。

### 支援的規則

| 規則 | 說明 |
|------|------|
| `require-tests` | 確保程式碼變更包含測試檔案 |
| `no-profanity` | 禁止不雅內容 |

你可以透過在團隊目錄的 `skills/guard-<name>/SKILL.md` 建立 guard skill 定義來實作自訂規則。

---

## Worker Tools 參考

`hufu` 提供 18 種 Worker Tools，以及 4 種 Always-included Tools 和 5 種 Coordinator Tools：

### 18 種 Worker Tools

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
| `random` | 產生亂數 / UUID |
| `math` | 評估數學運算式 |

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
- `chat_history.md` — 人類可讀的對話歷史紀錄
- `execution_trace.log` — 詳細的執行軌跡日誌（僅在 TUI 模式下產生）
- `history/` — 封存的歷史 session 檔案

---

## TUI 模式

啟用即時 Bubble Tea 終端 UI：

```bash
# TUI 模式，即時追蹤任務
go run ./cmd/hufu --tui "重構 auth 模組"
```

TUI 顯示：
- **任務清單** — Pending、進行中、完成、錯誤等狀態
- **工具呼叫 / 結果** — 即時 Agent 工具執行日誌
- **技能使用** — 追蹤本 session 載入的技能
- **Wrap-up 指示器** — Coordinator 即將結束時的視覺提示

> **注意**：`--tui` 與 `--steps` 不可同時使用。

---

## Dry Run 模式

預覽執行計畫，不呼叫 LLM：

```bash
# 預覽技能比對與委派
go run ./cmd/hufu --dry-run "重構模組"
```

輸出（純粹由 team config 衍生，不涉及 LLM）：
- Team 名稱、模型、sidecar 模型
- 使用者 prompt
- 所有可用的 agents（名稱、角色、模型、工具、技能）
- 所有已發現的 skills
- 名稱/描述關鍵字與使用者 prompt 相符的 skills
- 註：實際的任務委派**不會**在此規劃（那需要 LLM）。Dry-run 只列出*可能*被使用的 agents。

---

## Plan-First 模式

強制要求 Agent 先提交計畫再執行：

```bash
go run ./cmd/hufu --plan "實作功能"
```

---

## 報表產生

執行後產生完整的 markdown 報表：

```bash
go run ./cmd/hufu --report "重構模組"
```

報表包含：
- 任務委派摘要
- Agent 執行日誌
- 工具與技能使用統計
- 效能指標

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
embedding-model: ollama/nomic-embed-text:latest
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
| Embedding Model | `ollama/nomic-embed-text:latest` | `config.go` |

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

## DAG 任務排程

DAG 任務排程是完全由你的 Prompt 動態驅動的。你不需要手寫任何設定檔，只需要在對話或指令中清楚描述任務的先後順序、依賴關係或驗證條件，Coordinator 就會自動將其轉譯為底層的執行圖 (Execution Graph)。

例如，你可以透過自然語言強制指定執行順序與客觀驗證：

```bash
# 指示 coordinator 建立依賴關係與 verify 指令
go run ./cmd/hufu --agent-team dev-team \
  "請先指派 researcher 尋找 auth 最佳實踐。完成後，再指派 coder 實作 login 功能。請為 coder 設定 verify 指令 'go test ./tests/login/...' 以確保程式碼通過測試。"
```

### Verify 指令

在 Prompt 中要求設定 `verify`，會讓 Coordinator 在該任務綁定一段 shell 指令。這段指令會在 Agent 報告成功之後，但在任務標記為 done 之前執行。非零的退出狀態碼將導致任務失敗，並觸發重試路徑，能有效防止 Agent 謊稱任務已完成。

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

本專案採用 Apache License 2.0 授權 — 詳見 [LICENSE](LICENSE) 檔案。

---

## 相關連結

- **Ollama**: [https://ollama.com](https://ollama.com)
- **Cobra**: [https://github.com/spf13/cobra](https://github.com/spf13/cobra)
- **MCP**: [https://modelcontextprotocol.io](https://modelcontextprotocol.io)
