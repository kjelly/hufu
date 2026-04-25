# agent-team-cli 規格書

## 1. 專案概述

**agent-team-cli** 是一個 CLI 工具，用於協調多個 LLM Agent 團隊來共同完成任務。團隊由一個協調者（Coordinator）和多個工作者（Worker）組成。協調者接收使用者任務，利用 `run_agents` 工具將工作委派給各 Agent，Agent 間可平行執行任務，最終由協調者整合結果回傳給使用者。

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
agent-team-cli <team-dir> [prompt]
```

- `<team-dir>`：必填，團隊定義目錄（包含 `team.yml` 和 Agent 定義檔案）
- `[prompt]`：選填，任務提示（可從 stdin 讀取；若兩者皆無，則以互動式方式向使用者詢問）

### 4.2 指令旗標

| 旗標 | 預設值 | 說明 |
|------|--------|------|
| `--ollama-url string` | `http://localhost:11434/v1` | Ollama API URL |
| `-v, --verbose` | `false` | 即時顯示 Agent 完整輸出 |
| `-w, --workspace string` | `<cwd>/workspace` | 工作區目錄 |
| `-n, --new` | `false` | 歸檔舊 session 並開始新 session |

### 4.3 互動式提示詞輸入

當未提供 prompt 且 stdin 無輸入時，程式會以互動式方式向使用者詢問：

```
─── Enter Prompt ───
Please describe the task you want the team to perform:
> 
```

若直接按 Enter 提交空白 prompt，程式會顯示錯誤並退出。

## 5. 團隊設定格式

### 5.1 team.yml

使用自訂簡化 YAML 解析器（僅支援扁平 `key: value` 結構）。

```yaml
name: my-team
description: "團隊描述"
max-rounds: 10        # 最大委派回合數，預設 10
timeout: 300           # Agent 逾時（秒），預設 600
max-retries: 2         # 最大重試次數，預設 2
model: ollama/qwen3:8b # 預設模型（所有 Agent）
workspace: workspace   # 工作區目錄，可為絕對路徑
temperature: 0.3      # 生成溫度
max-tokens: 8192      # 最大 token 數
top-p: 0.9            # Top-p 採樣
top-k: 40             # Top-k 採樣
skills: code-review,git-commit    # 包含的技能列表（可選）
skills-exclude: debug              # 排除的技能列表（可選）
```

### 5.2 Agent 定義檔案

格式為 Markdown + YAML Frontmatter：

```markdown
---
name: developer
description: 實作專家
model: ollama/qwen3:8b
max-tokens: 8192
temperature: 0.2
top-p: 0.9
top-k: 40
role: worker
tools: read,write,edit,bash,grep,find,ls
skills: code-review    # 此 Agent 適用的技能（可選）
timeout: 300
max-retries: 3
---
你的系統提示詞在這裡。
```

- **role**：Agent 角色，`worker`（預設）或 `coordinator`
- **tools**：可用工具列表（逗號分隔），`all` 表示全部工具，`glob` 別名為 `find`
- **skills**：此 Agent 適用的技能名稱列表，會在任務執行時自動注入技能摘要至 prompt
- **系統提示詞**：第二個 `---` 之後的所有內容

### 5.3 MCP 伺服器設定

MCP 伺服器在 `team.yml` 中宣告：

```yaml
mcp-servers:
  my-server:
    type: local        # "local" 或 "remote"，自動偵測
    command: ["npx", "-y", "@modelcontextprotocol/server-filesystem", "/path"]
    allowedTools: []    # 允許的工具列表，空表示全部
    excludedTools: []   # 排除的工具列表
```

- `type: local`：執行 stdio MCP 伺服器
- `type: remote`：連接 HTTP MCP 伺服器（需 `url` 欄位）

### 5.4 技能設定

技能從以下位置自動探索：

1. `team-dir/.agents/skills/{skill-name}/SKILL.md`
2. `~/.agents/skills/{skill-name}/SKILL.md`

技能透過 `team.yml` 中的 `skills` 和 `skills-exclude` 欄位進行包含或排除過濾。

## 6. 內建工具

### 6.1 bash

在系統 shell 中執行命令。

```json
{"command": "ls -la", "timeout": 60}
```

- 預設逾時 120 秒，最大 600 秒
- 禁止執行的命令：shell 內建指令（`alias`, `bg`, `bind`, `builtin`, `caller`, `command`, `compgen`, `complete`, `compopt`, `coproc`, `dirs`, `disown`, `enable`, `fc`, `fg`, `hash`, `help`, `history`, `jobs`, `kill`, `logout`, `mapfile`, `popd`, `pushd`, `readonly`, `select`, `set`, `shopt`, `source`, `suspend`, `times`, `trap`, `type`, `typeset`, `ulimit`, `umask`, `unalias`, `wait`）
- 輸出截断至最後 2000 行或 50KB

### 6.2 read

讀取檔案內容。

```json
{"path": "path/to/file.go", "offset": 1, "limit": 100}
```

- `offset`：起始行號（1-indexed）
- `limit`：最大行數
- 截断至 2000 行或 50KB
- 目錄會返回錯誤

### 6.3 write

寫入檔案內容。

```json
{"path": "path/to/file.go", "content": "檔案內容"}
```

- 自動建立父目錄
- 覆寫已存在的檔案

### 6.4 edit

取代檔案中的文字。

```json
{"path": "file.go", "old_text": "舊文字", "new_text": "新文字"}
```

或批次編輯：

```json
{"path": "file.go", "edits": [{"old_text": "...", "new_text": "..."}]}
```

- 精確匹配 `old_text`
- 若無精確匹配，嘗試模糊匹配（正規化空白和智能引號）
- 多重匹配會返回錯誤（需提供更多上下文）
- 編輯間不可重疊
- CRLF 自動轉換為 LF

### 6.5 grep

搜尋檔案內容。

```json
{"pattern": "func.*Error", "path": ".", "glob": "*.go", "ignore_case": false, "literal": false, "context": 3, "limit": 100}
```

- 支援正規表達式（預設）和純文字（`literal: true`）
- 限制 100 個匹配結果
- 尊重 `.gitignore`
- 備援使用系統 `grep`

### 6.6 find

依 glob 模式搜尋檔案。

```json
{"pattern": "**/*.go", "path": ".", "limit": 1000}
```

- 預設使用 `fd`（含 `.gitignore` 支援）
- 備援使用 `find`
- 限制 1000 個結果

### 6.7 ls

列出目錄內容。

```json
{"path": ".", "limit": 500}
```

- 目錄顯示 `/` 後綴
- 包含隱藏檔案
- 依字母排序
- 限制 500 個項目

### 6.8 ask_user

向使用者提問並等待回應。

```json
{"question": "選擇一個選項", "type": "single_choice", "options": [{"label": "選項 A"}, {"label": "選項 B"}], "allow_any": false}
```

- **type**：`single_choice`（單選）、`multiple_choice`（多選）、`free_text`（自由輸入）、`mixed`（混合）
- `options`：選項列表（`label` 必填，`value` 選填）
- `allow_any`：單/多選時是否允許自由輸入
- 回應格式：`{"answers": ["value"], "free_text": "text"}`

### 6.9 load_skill

載入技能完整內容（僅協調者可用）。

```json
{"name": "code-review"}
```

- 根據技能名稱載入對應的技能內容
- 回應格式：`Skill: {name}\n\n{content}`
- 找不到時返回錯誤，並列出可用技能

### 6.10 finish

標記協調完成並提供最終答案（僅協調者可用）。

```json
{"response": "這是給使用者的最終答案"}
```

- 協調者必須調用此工具作為最終回應
- 回應前綴 `FINISHED:` 用於識別完成訊息

## 7. 協調流程

### 7.1 啟動序列

1. 解析命令列參數和 stdin 輸入；若無則以互動式詢問
2. 載入團隊設定（`team.yml` 和 Agent 定義）
3. 探索並過濾技能（從 `team-dir/.agents/skills` 和 `~/.agents/skills`）
4. 建立或恢復 session
5. 連接 Ollama provider
6. 載入 MCP 伺服器（如有）
7. 將技能複製到工作區（`workspace/shared/skills/`）
8. 執行協調流程

### 7.2 Session 管理

- **Session 資料**：`session.json`（JSON 格式）記錄所有對話
- **Session 日誌**：`session.md`（Markdown 格式）人性化日誌
- **新 Session** (`--new`)：歸檔舊 session 至 `history/` 並開始新 session
- **恢復 Session**：載入舊 session，注入上下文摘要至協調者提示詞（最近 20 筆交換，每筆限制 500 字元）
- **中斷處理**：Ctrl+C 時自動儲存 session

### 7.3 委派流程

1. 協調者接收使用者任務
2. 評估是否需要使用技能，呼叫 `load_skill` 取得詳細指示
3. 分析任務並決定需要的 Agent
4. 使用 `run_agents` 工具委派任務（可平行委派多個獨立任務）
5. 對於具有技能標記的 Worker Agent，自動將技能摘要注入任務 prompt
6. Agent 接收任務、執行工具、返回結果
7. 協調者整合結果並回應使用者，呼叫 `finish` 工具

### 7.4 技能注入機制

當 Worker Agent 被賦予技能時（如 `skills: code-review`）：

1. 在任務執行前，自動將對應技能的摘要和工作區路徑注入 prompt 前綴
2. 技能內容被複製至 `workspace/shared/skills/{skill-name}.md`
3. Worker 可透過 `read` 工具讀取完整技能指示

### 7.5 重試機制

- 任務失敗時自動重試（最多 `max-retries` 次）
- 重試時會傳遞對話歷史，讓 LLM 從上次進度繼續
- 達到最大重試次數後返回錯誤

### 7.6 回合限制

- `max-rounds` 限制委派回合數（每次 `run_agents` 呼叫為一回合）
- 超過限制時停止執行

## 8. 工作區結構

```
workspace/
├── inbox/
│   └── {agent}/
│       └── task-{timestamp}.md     # Agent 收到的任務
├── outbox/
│   └── {agent}/
│       └── result-{timestamp}.md   # Agent 產出的結果
├── shared/
│   └── skills/
│       └── {skill-name}.md        # 技能檔案（自動複製至此）
├── status/
│   └── {agent}.yml                # Agent 狀態檔案
├── history/
│   └── {date}-{slug}.md           # 歸檔的 session
├── session.json                    # Session 資料
└── session.md                     # Session 日誌
```

## 9. Agent 角色與權限

| 角色 | 可用工具 | 說明 |
|------|----------|------|
| `coordinator` | `run_agents`, `finish`, `load_skill`, `ask_user` | 只能協調，不能自行執行任務 |
| `worker`（預設） | 指定的工具集 | 執行實際工作 |

- `role` 欄位用於識別協調者；任何名稱含 "coordinat" 或 "orchestr" 的 Agent 也會被視為協調者
- 協調者不可被委派（防禦性檢查）

## 10. 輸出格式

### 10.1 狀態報告

使用 lipgloss 樣式化輸出至 stderr：

- `▶` — Agent 開始執行
- `│` — 步驟進度
- `⟹` — 工具呼叫
- `✓` — 工具結果/完成
- `💬` — Agent 回應文字
- `✗` — 錯誤
- `⚠` — 警告

### 10.2 最終輸出

協調結果輸出至 stdout，其餘診斷資訊輸出至 stderr。

## 11. 建構與發布

### 11.1 建構

```bash
go build ./cmd/agent-team-cli
```

### 11.2 CI 工作流

- 觸發條件：`push` 或 `pull_request` 到 `main` 分支
- 執行：`go vet` → `go build` → `go test`

### 11.3 發布工作流

- 觸發條件：推送 `v*` 標籤
- 使用 GoReleaser 自動發布至 GitHub Releases
- 支援平台：Linux、macOS、Windows（amd64、arm64）
- 歸檔格式：tar.gz（Linux/macOS）、zip（Windows）
- 產生 SHA256 校驗和檔案

## 12. 限制與約束

- **路徑解析**：相對路徑以工作區目錄為基準
- **模型識別**：`ollama/` 前綴會被自動移除
- **YAML 限制**：`team.yml` 僅支援扁平結構，不支援巢狀 YAML
- **MCP 工具命名**：MCP 工具名稱前綴為 `{server}__` 以避免衝突
- **Session 大小**：單一 session 最多保留 40 筆交換記錄用於日誌顯示
- **技能探索**：`skill.DiscoverSkills` 僅探索目錄層級（不遞迴深入），每個技能需有 `SKILL.md` 檔案
