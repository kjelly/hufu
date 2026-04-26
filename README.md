# Hufu — Agent Team Orchestration CLI

**hufu** 是一座命令列工具，用於協調 LLM 代理團隊協作完成任務。透過 Ollama 支援多代理協作，可根據指令自動切換團隊或指定特定代理執行任務。

## 快速開始

### 前置條件

- **Go 1.26+**: [安裝 Go](https://golang.org/dl/)
- **[Ollama](https://ollama.com/)**: 正在運行且已拉取模型（如 `qwen3:8b`）

### 安裝與建置

```bash
# 建置
go build ./cmd/hufu          # 產生 hufu 二進位檔案
go run ./cmd/hufu [prompt]   # 直接使用

# 驗證
go vet ./...                 # 程式碼檢查
go test ./...                # 執行測試
go mod tidy                  # 整理相依性
```

### 基本使用

```bash
# 最簡單的用法：直接提供提示詞
hufu "調查這個專案的效能瓶頸"

# 指定要使用的代理團隊
hufu --agent-team delegate "檢查程式庫的 bugs"

# 使用提示詞中的團隊標記
hufu "@delegate 調查这个问题 @tether 分析架構"

# 無需提示詞，進入互動模式
hufu
```

## 命令列選項

```
Usage:
  hufu [prompt]

Flags:
  --ollama-url string       Ollama API 位址（預設：http://localhost:11434/v1）
  -v, --verbose            即時顯示完整代理文字輸出
  -w, --workspace string   工作區目錄（預設：<目前目錄>/workspace）
  -n, --new                歸檔舊會話並開始全新工作
  -t, --temp               使用暫存目錄作為工作區
  --agent-team string      指定要載入的代理團隊名稱
  --agent-team-search-path string
                           搜尋路徑（逗號分隔，預設：.agent-teams/,~/.agent-teams/）
```

## 提示詞語法

### 團隊切換與任務指派

使用 `@團隊名稱` 格式來切換團隊並指派任務：

```bash
# 切換到 delegate 團隊
@delegate 分析專案結構

# 指定代理
@developer 寫一個新的 API endpoint
```

### 多團隊協作

在同一個提示詞中切換多個團隊：

```bash
@delegate 負責研究 @tether 撰寫文件 @delegate 總結結果
```

### 直接呼叫代理

指定當前團隊中的特定代理執行任務：

```bash
@developer 執行具體的編碼工作
@researcher 負責資料蒐集
```

### 提示傳入團隊協調者

純文字提示會傳入當前團隊的協調者：

```bash
根據之前的調查結果，生成一份簡報大綱
```

### 互動模式

當未提供提示詞時，進入互動模式：

1. 顯示可用團隊清單
2. 若未指定團隊，則顯示選擇選單
3. 輸入任務描述

### 輸入提示規則

- 使用 `@name` 標記：支援 `@team-name` 或 `@agent-name`
- 支援中文字：完全支援 Unicode，可輸入中文任務
- 語法解析：`@` 後接字母、數字、底線或連字號

## 代理團隊搜尋路徑

### 預設搜尋路徑

程式會自動在以下路徑搜尋團隊定義：

- `.agent-teams/`
- `~/.agent-teams/`

### 自訂搜尋路徑

使用 `--agent-team-search-path` 指定：

```bash
hufu --agent-team-search-path "./my-teams/,~/.custom-teams/" "執行任務"
```

## 團隊結構

### 團隊目錄範例

```
.agent-teams/
├── delegate/
│   ├── team.yaml              # 團隊配置
│   ├── coordinator.md         # 協調器代理
│   ├── researcher.md          # 研究員代理
│   ├── writer.md              # 作家代理
│   └── .agents/
│       └── skills/            # 技能定義
├── dev-team/
│   └── ...
└── tether/
    └── ...
```

### team.yaml 格式

```yaml
name: my-team
description: "團隊描述"
max-rounds: 10
timeout: 300
max-retries: 2
model: ollama/qwen3:8b
workspace: workspace
skills: code-review,git-commit
skills-exclude: debug
```

#### 配置欄位

| 欄位 | 說明 | 預設值 |
|------|------|--------|
| `name` | 團隊名稱 | 自動從目錄名稱推斷 |
| `description` | 團隊描述 | 無 |
| `max-rounds` | 最大輪數 | 10 |
| `timeout` | 超時秒數 | 600 |
| `max-retries` | 最大重試次數 | 2 |
| `model` | 使用的模型 | 無 |
| `workspace` | 工作區目錄名 | `workspace` |
| `skills` | 需要的技能（逗號分隔） | 無 |
| `skills-exclude` | 排除的技能 | 無 |

### 代理檔案格式

代理檔案使用 Markdown 格式，開頭需包含 YAML Frontmatter：

```markdown
---
name: developer
description: 實現專家 — 撰寫生產程式碼
role: worker
tools: read,write,edit,bash,grep,find,ls
model: ollama/qwen3:8b
max-tokens: 8192
temperature: 0.2
---

你的系統提示詞內容...
```

#### Frontmatter 欄位

| 欄位 | 說明 | 預設 |
|------|------|------|
| `name` | 代理名稱（必填） | — |
| `description` | 代理描述 | — |
| `role` | 角色（worker/orchestrator/coordinator） | worker |
| `tools` | 可用工具清單 | — |
| `skills` | 相關技能 | — |
| `model` | 專屬模型設定 | 繼承團隊設定 |
| `max-tokens` | 最大 Token 數 | — |
| `temperature` | 溫度參數 | — |
| `timeout` | 超時秒數 | 繼承團隊設定 |
| `max-retries` | 最大重試次數 | 繼承團隊設定 |

## 代理工具

### 協調器工具

- **`run_agents`** — 指派任務給工作代理
- **`load_skill`** — 載入技能內容
- **`finish`** — 完成並輸出最終答案
- **`ask_user`** — 要求使用者輸入

### 工作代理工具

- `bash` — 執行 Shell 指令
- `read` — 讀取檔案內容
- `write` — 寫入檔案
- `edit` — 編輯檔案
- `grep` — 搜尋檔案內容
- `find` — 搜尋檔案
- `ls` — 列出目錄內容
- `ask_user` — 要求使用者輸入

## 工作區結構

每個代理團隊都會建立獨立的工作區：

```
workspace/
├── inbox/                  # 代理任務指派檔案
├── outbox/                 # 代理任務結果
├── shared/
│   └── skills/            # 已複製的技能檔案
├── status/                 # 代理狀態檔案
├── history/                # 歸檔的會話檔案
├── session.json           # 結構化會話資料
└── session.md             # 人類可讀的會話日誌
```

## 會話管理

### 會話歸檔

- 每次切換團隊時，前一個團隊的會話會自動歸檔
- 使用 `--new` 選項可強制歸檔並開始全新會話
- 中斷（Ctrl+C）時會自動儲存當前狀態

### 會話恢復

- 下次執行時，程式會自動恢復之前的會話
- 顯示上次會話的交換次數和開始時間
- 可讀取 `session.md` 檢視歷史記錄

## 即時互動操作

### 信號注入

在協調運作時，可透過信號注入額外提示：

- **Ctrl+Z** (SIGTSTP) — 注入額外提示
- **SIGUSR1** — 透過 kill 命令發送

```bash
# 發送 SIGUSR1
kill -USR1 $(pgrep hufu)
```

### 優雅結東

- **Ctrl+C** 第一次 — 進入收尾模式（不再開始新任務）
- **Ctrl+C** 第二次 — 強制終止
- **Ctrl+Z** 後輸入指令 — 注入額外提示而不中斷進行中的工作

### 忙碌狀態警示

- 30 秒無活動時顯示等待警示
- 恢復活動時顯示恢復提示
- 警示會自動重置

## TODO 追蹤系統

程式提供結構化的任務追蹤功能：

### TODO 顯示格式

```
─── TODO ───
  ○ 1. researcher 尋找問題
  ◑ 2. writer 撰寫文件
  ● 3. checker 驗證測試結果
  ✗ 4. researcher 第 1 次嘗試失敗：錯誤訊息
```

### 任務狀態

| 狀態 | 圖示 | 說明 |
|------|------|------|
| `TaskPending` | ○ | 等待處理 |
| `TaskInProgress` | ◑ | 處理中 |
| `TaskDone` | ● | 已完成 |
| `TaskError` | ✗ | 錯誤 |

### 狀態工作流程

```
新增任務 (TaskPending)
    ↓
開始處理 (TaskInProgress)
    ↓
完成 (TaskDone)
    或
失敗 (TaskError)
```

## MCP 伺服器支援

### MCPServer 功能

可集成 MCP（Model Context Protocol）伺服器以擴展能力：

```yaml
# team.yaml 配置
mcp:
  server-name:
    command: npx -y @modelcontextprotocol/server-filesystem
    args:
      - /path/to/directory
    env:
      KEY: value
```

### 使用範例

```yaml
mcp:
  filesystem:
    command: npx
    args:
      - -y
      - @modelcontextprotocol/server-filesystem
      - /data
  todoist:
    command: /usr/local/bin/todoist-sync
    env:
      TODOIST_API_KEY: ${TODOIST_API_KEY}
```

## 技能系統

### 技能目錄結構

```
{團隊目錄}/.agents/skills/{skill-name}/SKILL.md
~/.agents/skills/{skill-name}/SKILL.md
```

### SKILL.md 格式

每個技能檔案需要包含：

- **name**: 技能名稱
- **description**: 技能描述
- **content**: 技能內容
- **summary**: 簡短摘要

技能會在任務期間自動注入到相關代理的提示詞中。

## 錯誤處理

### 常見錯誤訊息

- `no team found in search paths` — 檢查搜尋路徑和團隊結構
- `@name` — 團隊尚未載入 — 先指定團隊再呼叫代理
- `@name — no active team` — 需要指定團隊
- `agent team name not valid` — 代理人員呼叫錯誤
- `team "name" failed` — 團隊任務失敗，查看協作錯誤

### 除錯建議

1. 使用 `-v` 選項查看完整輸出
2. 檢視 `workspace/session.md` 歷史記錄
3. 確認 Ollama 伺服器是否運行
4. 檢查模型是否已正確拉取
5. 驗證團隊結構和檔案語法

## 進階用法

### 工作區重用

```bash
# 建立暫存工作區
hufu -t "執行任務"

# 之後重用相同工作區
hufu -w /path/to/workspace "繼續任務"
```

### 多團隊同時執行

```bash
# 單一提示詞中使用多個團隊
hufu "@team-a 研究 @team-b 撰寫 @team-a 總結"
```

### 自訂 Ollama 端點

```bash
hufu --ollama-url "http://remote-host:11434/v1" "執行任務"
```

## 與 Ollama 整合

### 模型設定

```bash
# 拉取必要模型
ollama pull qwen3:8b
ollama pull qwen3:32b

# 檢查已安裝模型
ollama list

# 執行模型测试
ollama run qwen3:8b "你好"
```

### 模型選用

- 可在全體配置 `team.yaml` 中設定預設模型
- 可在個別代理檔案中覆寫模型設定
- 支援不同的 Ollama 實例端點

## 效能提示

- **預載團隊**：多個任務前載入團隊可加速處理
- **工作區分類**：為不同專案使用不同工作區
- **策略性歸檔**：定期歸檔舊會話保持工作區整潔
- **監控資源**：注意 Ollama 的服務器資源使用情况

## 範例工作流程

### 開發團隊工作

```bash
# 使用開發團隊
hufu "@dev-team 建立新功能 @reviewer 審查程式碼"

# 或直接使用開發團隊
hufu --agent-team dev-team "實現 API 端點"
```

### 多階段任務

```bash
# 複雜的多階段任務
hufu "@delegate 調查問題 @researcher 搜集資料 \
     @tether 撰寫報告 @delegate 提出建議"
```

### 持續開發

```bash
# 階段 1：調查
hufu @delegate "調查專案結構"

# 階段 2：開發
hufu @developer "實現新功能"

# 階段 3：測試
hufu @checker "驗證測試"
```

## 注意事項

1. **無需團隊目錄參數**：新版本的 CLI 不再接受團隊目錄位置參數
2. **團隊與代理消歧**：`@name` 先檢查團隊，再檢查當前團隊的代理
3. **提示詞解析**：先識別團隊，再解析代理呼叫
4. **@example.com** 特殊情況：電子郵件格式的位址也會匹配
5. **跨團隊工作區共享**：`--workspace` 設定適用於所有團隊

## 故障排除

### 無法載入團隊

```bash
# 檢查搜尋路徑
ls .agent-teams/
ls ~/.agent-teams/

# 檢查團隊結構
ls .agent-teams/team-name/

# 檢查 team.yaml 語法
cat .agent-teams/team-name/team.yaml
```

### Ollama 連線問題

```bash
# 檢查 Ollama 服務
curl http://localhost:11434/api/tags

# 測試連線
ollama list
```

### 工作區問題

```bash
# 清理並重新建立
rm -rf workspace/
hufu --new "重新建立工作區"
```

## 相關資源

- **AGENTS.md**: 詳細的系統架構與開發者文件
- **SPEC.md**: 系統規格說明
- **Ollama GitHub**: https://github.com/ollama/ollama
- **Charm Fantasy**: https://charm.land/fantasy (使用的 LLM 框架)

## License

MIT License - 依專案授權條款使用。

---

**Hufu** 是一個強大靈活的代理協調工具，可讓 LLM 代理團隊協作完成複雜任務。從簡單的使用者介面到進階的自訂能力，為您的 AI 代理工作流提供完整支援。
