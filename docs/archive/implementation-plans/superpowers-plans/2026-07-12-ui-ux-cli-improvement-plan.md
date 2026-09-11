# hufu UI/UX 與 CLI 改善計畫

> **STATUS: DRAFT - NOT YET VERIFIED**
> 本文件中的命令與介面為提案，並非目前可執行的操作程序；每個項目實作後必須以實際輸出補齊驗證證據。

> 日期：2026-07-12
> 實作狀態：已完成非終端安全降級、色彩控制、JSONL events、CLI 摘要、config/status/version、init templates、list JSON、REPL inspection/export、TUI compact view、task log 上限、progress bar、result viewer 與 overlay 文件同步；其餘項目仍待實作與驗證。
> 範圍：CLI 介面設計、TUI 互動體驗、chat REPL、顯示系統、子命令 UX
> 依據：對 `cmd/hufu/`（main.go 2,054 行、display.go 1,671 行、chat.go 420 行）與 `internal/tui/`（tui.go 2,674 行、ask_user.go、overlay.go）的原始碼審查

---

## 一、現狀診斷摘要

| 面向 | 現狀 | 問題 |
|------|------|------|
| CLI flags 數量 | 60+ 個 root flags | 🟡 新手門檻高，無分群 |
| flag 與子命令共用 | root flag 綁在全域 var | 🟡 chat 子命令手動 re-bind，易漂移 |
| TUI 程式碼 | tui.go 單檔 2,674 行 / 71 函式 | 🟡 維護困難 |
| TUI 欄位 | Model struct 30+ 欄位 | 🟡 overlay 管理靠 9 個 bool + enum |
| 非終端輸出 | 用 ANSI 游標操作重繪 TODO 面板 | 🔴 管道/重定向時崩壞 |
| 退出碼 | 僅 0/1/130 | 🟡 無法腳本化判斷失敗類型 |
| 顏色控制 | 無 `--no-color` / `NO_COLOR` | 🟡 CI 環境 ANSI 殘留 |
| chat REPL | 4 個指令（/exit /reset /team /help） | 🟡 功能不足 |
| 子命令覆蓋 | 無 `config`、`status`、跨執行 session 瀏覽 | 🟡 設定與可觀測性不足 |

---

## 二、CLI 介面改善

### 2.1 Flag 分群與漸進式揭露

**問題**：60+ 個 root flag 扁平排列，`hufu --help` 輸出一面文字牆。

**方案**：

1. **分群顯示**：在 `rootCmd` 的 `Long` 中按語意分區（不靠 cobra 的 flag grouping，而是自定义 `UsageTemplate`）：
   - `Core`：`--model`、`--agent-team`、`--default`、`--workspace`
   - `Execution`：`--plan`、`--auto-skills`、`--steps`、`--dry-run`、`--timeout`、`--max-rounds`
   - `Model/Tuning`：`--temperature`、`--max-tokens`、`--top-p`、`--top-k`、`--sidecar-model`、`--guard-model`、`--judge-model`
   - `Security`：`--rbash`、`--no-net`、`--force-mcp`、`--allow-path`、`--direnv`
   - `Unattended`：`--unattended`、`--max-duration`、`--max-total-tokens`、`--auto-approve`
   - `Output`：`--verbose`、`--quiet`、`--output`、`--report`、`--think`、`--tui`
   - `Advanced`：`--provider-url`、`--provider-api-key`、`--profile`、`--var`、`--var-file`、`--memory`、`--template`

2. **`hufu --help-flags <group>`** 子命令：只顯示該群組的 flag 詳細說明。

3. **`hufu --examples`**：顯示按場景分類的常用命令範例（快速上手、互動模式、CI/CD 等）。

**預期效果**：新手只需看 Core 群即可上手；進階使用者可快速定位特定 flag。

**實作位置**：`cmd/hufu/main.go` — 新增自定義 `cobra.UsageTemplate` 或 `SetHelpFunc`。

### 2.2 Flag 與子命令共用機制重構

**問題**：root flag 綁定到 package-level `var`，`chat` 子命令在 `init()` 中手動 re-bind 25 個 flag 到相同的全域變數。新增 flag 需要在兩處同步修改，容易遺漏。

**方案**：

1. 提取 `commonFlags()` 函式回傳 `[]FlagSpec`（名稱、型別、預設值、說明），root 和 chat 都從此函式註冊。
2. 或更激進：將 root flag 改為 `PersistentFlags()`，讓子命令自動繼承（需注意某些 flag 不適合所有子命令）。

**實作位置**：`cmd/hufu/main.go` + `cmd/hufu/chat.go`。

### 2.3 退出碼細化

**問題**：退出碼只有 0（成功）、1（一般錯誤）、130（中斷），腳本無法區分失敗類型。

**方案**：

| 退出碼 | 含義 | 觸發條件 |
|--------|------|---------|
| 0 | 成功 | 正常完成 |
| 1 | 未知錯誤 | 未分類的 panic/error |
| 2 | 設定錯誤 | flag 衝突、team 找不到、config 解析失敗 |
| 3 | provider 不可達 | LLM API 連線失敗 |
| 4 | 超時 | agent/coordinator 逾時 |
| 5 | 預算耗盡 | `--max-duration` / `--max-total-tokens` 觸發 |
| 6 | 驗收失敗 | acceptance command 非零退出 |
| 7 | 部分成功 | 有 task error 但整體完成 |
| 130 | 使用者中斷 | Ctrl+C / SIGINT |

**實作前提**：先在 `internal/` 與 `cmd/` 邊界定義可用 `errors.Is` 辨識的 typed/sentinel errors（設定、provider、timeout、budget、acceptance、partial success）。不可在 `os.Exit(exitCode)` 前以錯誤字串分類，因為跨層包裝會使分類不可靠。

**實作位置**：錯誤來源 package + `cmd/hufu/main.go` 的最上層錯誤對應。先以整合測試固定每一類別的退出碼契約。

### 2.4 顏色控制與 `NO_COLOR` 支援

**問題**：lipgloss 預設自動偵測終端，但沒有 `--no-color` flag 或 `NO_COLOR` 環境變數支援。CI 環境中 ANSI 序列殘留在日誌中。

**方案**：

1. 在 `main()` 早期檢查 `NO_COLOR` 環境變數（[no-color.org 標準](https://no-color.org)）。
2. 新增 `--no-color` flag，與 `NO_COLOR` 環境變數做 OR。
3. 使用 `lipgloss.SetDefaultRenderer(lipgloss.NewRenderer(io.Writer, lipgloss.WithColorLevel(...)))` 全域控制。
4. `--output json` 時自動停用顏色。

**實作位置**：`cmd/hufu/main.go` — `main()` 最早期 + `cmd/hufu/display.go` 的全域 style 定義。

### 2.5 `hufu config` 命令

**問題**：使用者無法在不手動編輯 YAML 的情況下查看或修改 `hufu.yaml`。profile 系統隱藏，無法列出可用 profile。

**方案**：

```
hufu config                 # 顯示合併後的有效設定（hufu.yaml + flags + env）
hufu config --json          # JSON 格式輸出
hufu config list-profiles   # 列出所有 profile 名稱
hufu config show <profile>  # 顯示某 profile 的內容
hufu config init            # 互動式初始化 hufu.yaml
hufu config set <key> <val> # 設定單一值（寫入 ~/.config/hufu/hufu.yaml）
hufu config get <key>       # 讀取單一值
```

**實作位置**：新增 `cmd/hufu/configcmd.go`。

### 2.6 `hufu status` 命令

**問題**：無法快速查看當前 workspace 的 session 狀態（上次執行時間、任務完成度、token 用量等）。

**方案**：

```
hufu status                 # 概覽當前 workspace session
hufu status --json          # 機器可讀格式
```

輸出包含：
- Team 名稱、模型
- 上次執行時間、持續時間
- 任務統計（done/error/skipped/pending）
- Token 用量摘要
- Session 檔案路徑

**實作位置**：新增 `cmd/hufu/statuscmd.go`，讀取 `workspace/session.json`。

### 2.7 `hufu runs`（或 `sessions`）命令

**現況**：既有 `hufu history [query]` 用於語意搜尋 prompt history，不能改變其語意。

**問題**：過去的執行 session 記錄散落在各 workspace 目錄，無法跨 session 瀏覽。

**方案**：

```
hufu runs                    # 列出近期執行（從所有 workspace 的 session.json 彙總）
hufu runs --team <name>      # 篩選特定 team
hufu runs --limit 10         # 限制筆數
```

**實作位置**：新增 `cmd/hufu/runscmd.go`，掃描已知 workspace 與當前 workspace。需先明確定義可安全掃描的根目錄，避免無界限地遍歷家目錄。

---

## 三、TUI 改善

### 3.1 終端寬度適應與最小尺寸提示

**問題**：6 欄 Kanban 在寬度 < 80 時每欄只剩 ~12 字元，任務描述幾乎不可讀。沒有任何提示告知使用者終端太窄。

**方案**：

1. **寬度 < 60**：自動切換為 compact view，並顯示「terminal too narrow」提示；不可提示 `c` 切換，因為 `c` 已用於 prompt injection。
2. **Compact 模式**：將 6 欄合併為 3 欄（Pending+Planned、In Progress+Verifying、Done+Skipped+Error），用顏色區分。
3. **自動切換**：偵測到寬度變化時自動在 6-col / 3-col 間切換（可用 `--tui-compact` flag 固定）。
4. **高度不足**：當行高 < 20 時，prompt widget 和 activity feed 自動收合。

**實作位置**：`internal/tui/tui.go` — `columnsView()`、`renderCol()`、`WindowSizeMsg` handler。

### 3.2 任務篩選與排序

**問題**：dashboard 只能按狀態欄瀏覽，無法篩選特定 agent、按時間排序、或隱藏已完成任務。

**方案**：

1. **`f` 鍵開啟篩選面板**：
   - By agent：選擇一或多個 agent
   - By status：顯示/隱藏特定狀態欄
   - By skill：只顯示使用了特定 skill 的任務
   - 時間排序：建立時間 / 完成時間 / 持續時間
2. **`H` 鍵隱藏 Done/Skip 欄**：隱藏已完成欄位讓焦點集中在進行中任務。
3. 篩選狀態顯示在 footer 上（如 `[filter: agent=dev, sort=time]`）。

**實作位置**：`internal/tui/tui.go` — 新增 `filterState` struct + filter overlay。

### 3.3 Detail View 改善

**問題**：detail view 的 log 會無限累積，長時間執行時記憶體和渲染效能下降。沒有匯出功能。`enter` 在 detail view 沒有作用。

**方案**：

1. **Log 行數上限**：每個 task 的 `m.logs[todoID]` 超過 5000 行時只保留最後 5000 行；完整事件仍寫入既有 journal 或專用 log 檔，避免資料遺失。
2. **`e` 鍵匯出**：從完整事件來源匯出當前 task log 至 `workspace/logs/tui_export_<taskID>.log`，而非從已截斷的 UI 快取匯出。
3. **`enter` 展開/收合**：在 detail view 中，按 `enter` 可展開/收合摺疊的 log 區段（如 tool_result）。
4. **時間戳顯示**：在每行 log 前顯示相對時間戳（如 `+12s`），可用 `t` 鍵開關。
5. **摺疊 tool 輸出**：tool_result 預設只顯示前 5 行，按 `Tab` 展開全部。

**實作限制**：`Model.Update()` 必須維持純函式；按鍵僅發送 export request，檔案 I/O 由 `cmd/hufu` 的 reporter/callback 執行並回傳結果訊息。

**實作位置**：`internal/tui/` 的 detail 狀態與訊息 + `cmd/hufu/` 的 export handler。

### 3.4 進度概覽列

**問題**：dashboard 底部只有 footer 操作提示，沒有整體進度概覽。使用者不知道「還要多久」。

**方案**：

1. 在 prompt widget 和 status area 之間插入一行進度列：
   ```
   ████████░░░░░░░░░░░░  8/20 tasks (40%) · 3 in progress · 2 errors · 1m32s elapsed
   ```
2. 進度列只在有 ≥ 3 個 task 時顯示。
3. 用顏色標示示狀態：綠=done、黃=in progress、紅=error、灰=pending。

**實作位置**：`internal/tui/tui.go` — `columnsView()` 新增 `renderProgressBar()`。

### 3.5 Result Box 可展開

**問題**：完成時 result box 最多顯示 8 行，截斷的內容無法在 TUI 內查看。

**方案**：

1. 在 result box 上按 `enter` 進入全螢幕 result viewer（viewport 模式，同 memory view）。
2. `esc` 返回 dashboard。
3. Footer 顯示 `enter expand · esc back`。

**實作位置**：`internal/tui/tui.go` — 新增 `inResultView` overlay。

### 3.6 等待動畫改善

**問題**：LLM 等待時只有文字狀態列 `waiting for LLM… 15s`，缺乏視覺回饋。

**方案**：

1. 在狀態列加入 spinner 動畫（`⠋⠙⠹⠸⠼⠴⠦⠧⠇⠏`）。
2. Spinner 只在非 finished 且有 in_progress task 時啟動。
3. 用 `tea.Tick` 每 100ms 更新 spinner frame。
4. 可用 `--no-spinner` 或 `NO_SPINNER` env 停用。

**實作位置**：`internal/tui/tui.go` — 新增 `spinner` frame + `spinnerTickMsg`。

### 3.7 鍵盤快捷鍵一致性

**問題**：不同 overlay 的鍵綁不一致：
- Activity log 用 `space/b` 翻頁，但 detail view 用 `ctrl+d/u`。
- Info panel 的 `q` 可以關閉，但 search 的 `q` 不行。
- Help 的 `?` 可以開啟，但沒有在所有 overlay 中一致地關閉。

**方案**：

| 按鍵 | 全域行為（所有 overlay） |
|------|-------------------------|
| `esc` | 關閉當前 overlay（回到 columns） |
| `ctrl+c` | wrap-up / force quit（全域） |
| `?` | 開啟/關閉 help |
| `q` | 如果 finished → quit；否則如果不在 columns → 回 columns |

**實作位置**：`internal/tui/tui.go` — 統一所有 `update*` 函式的 `esc`/`q`/`ctrl+c` 行為。

### 3.8 Overlay 文件同步

**問題**：`inHelp` overlay 已加入程式碼（`overlay.go` 的 `OverlayHelp`），但 AGENTS.md 的 TUI Model Fields 表格和 View Priority Order 未更新。`inHelp` 不在 Model 欄位清單中。

**方案**：

1. 更新 AGENTS.md：在 Model Fields 表格新增 `inHelp`、`helpView()` 等。
2. 更新 View Priority Order：在 `inAskUser` 之後插入 `inHelp`。
3. 建立 lint 規則：`overlay.go` 的 `Overlay` enum 與 AGENTS.md 表格交叉檢查。

**實作位置**：文件更新 + 可選的 `go generate` 檢查腳本。

### 3.9 TUI 設定檔

**問題**：TUI 行為（是否啟用 mouse、log 行數上限、spinner 速度等）無法持久化，每次都要手動調整。

**方案**：

```yaml
# ~/.config/hufu/hufu.yaml
tui:
  mouse: true           # 預設啟用 mouse
  max-log-lines: 5000  # 每 task log 行數上限
  spinner: true         # 啟用 spinner
  compact: false        # 預設 6-col 模式
  auto-scroll: true     # detail view 自動滾到底
```

**實作位置**：`internal/config/` 新增 `TUIConfig` struct + `cmd/hufu/display.go` 傳遞至 TUI。

### 3.10 TUI 拆檔

**問題**：`tui.go` 單檔 2,674 行包含所有 overlay、渲染、鍵綁、viewport 邏輯。

**方案**：按 overlay 拆分（與 refactoring-plan.md 一致）：

| 檔案 | 內容 |
|------|------|
| `tui.go` | Model struct、`New()`、`Init()`、`Update()` 分派 |
| `columns.go` | 6-col dashboard 渲染、`renderCol()`、`itemLines()` |
| `detail.go` | Detail view、`buildDetailContent()`、visual mode |
| `search.go` | Search overlay |
| `ask_user.go` | (已存在) |
| `overlay.go` | (已存在) |
| `memory.go` | Memory view |
| `activity.go` | Activity log |
| `info.go` | Team info panel |
| `confirm.go` | Quit confirmation |
| `prompt_input.go` | Prompt injection dialog |
| `help.go` | Help screen |
| `styles.go` | 全域 lipgloss style |
| `helpers.go` | `wrapLine()`、`copyToClipboard()` 等 |

**實作位置**：`internal/tui/` — 純檔案搬移，不改介面。

---

## 四、CLI 顯示系統改善

### 4.1 非終端環境安全降級

**問題**：`taskDisplay` 使用 `\033[%dA\033[J` ANSI 游標操作重繪 TODO 面板。在非終端環境（管道、重導向、CI log）中，這些序列直接寫入輸出，造成亂碼。

**方案**：

1. 在 `newTaskDisplay()` 中偵測 `os.Stderr` 是否為終端（`term.IsTerminal(os.Stderr.Fd())`）。
2. 非終端模式：改為「追加模式」—— 不清行、不重繪，只在狀態變化時輸出一行摘要。
3. 第一階段提供 `--display-mode {auto,terminal,plain}`：
   - `auto`（預設）：終端 → 游標重繪；非終端 → plain
   - `terminal`：強制游標重繪
   - `plain`：純文字追加，無 ANSI
4. `jsonl` 屬於 4.2 的獨立功能，不應和終端降級綁在同一個 P0 變更中。

**實作位置**：`cmd/hufu/display.go` — `taskDisplay.clear()` 和 `taskDisplay.update()`。

### 4.2 結構化事件輸出

**問題**：CLI 模式的事件輸出是人類可讀的文字，無法被機器解析。`--output json` 只影響最終結果，不影響過程事件。

**方案**：

1. 新增 `--event-format {text,jsonl}` flag（預設 `text`）。
2. `jsonl` 模式：每個 `StatusEvent` 輸出一行 JSON 到 stderr：
   ```json
   {"type":"start","agent":"dev","todo":"t3","model":"qwen3:8b","ts":"2026-07-12T10:30:00Z"}
   ```
3. 可與 `--quiet` 組合：stdout 只有最終結果，stderr 有結構化事件。

**實作位置**：`cmd/hufu/display.go` — `setupStatusReporter()` 中的 `statusWriter` 介面新增 jsonl 實作。

### 4.3 執行結束摘要

**問題**：CLI 模式執行結束後沒有摘要，使用者要自己翻 stdout/stderr 找 token 用量和耗時。

**方案**：

執行完成後（非 `--quiet` 模式）自動輸出摘要到 stderr：

```
─── Summary ───
  Team:       dev-team
  Tasks:      12 done · 1 error · 2 skipped (15 total)
  Tokens:     45,230 (model: 38,100 · tools: 7,130)
  Duration:   3m42s (model: 2m51s · tools: 51s)
  Skills:     code-review ×3, git-commit ×1
  Session:    workspace/session.json
  Report:     workspace/report.md (if --report)
```

可用 `--no-summary` 停用。

**實作位置**：`cmd/hufu/main.go` — `executeAndReport()` 結束後。

### 4.4 `--think` 輸出視覺分離

**問題**：`--think` 模式的推理輸出與正常事件輸出交錯，難以區分。

**方案**：

1. think 輸出加固定前綴 `💭 ` 和縮排（已有，但可改進）。
2. 用左邊欄虛線分隔 think 區塊：
   ```
   ▶ developer — reviewing code
   │
   💆 ┊ Analyzing the function signature...
   💆 ┊ The return type should be error, not string.
   │
   ⟹ view
   ```
3. 或在 `--think` 模式下，think 輸出用暗色背景行渲染。

**實作位置**：`cmd/hufu/display.go` — `flushThink()`。

---

## 五、Chat REPL 改善

### 5.1 擴充 REPL 指令

**問題**：chat REPL 只有 4 個指令（/exit、/reset、/team、/help），功能不足。

**方案**：新增以下指令：

| 指令 | 功能 |
|------|------|
| `/agents` | 列出當前 team 的所有 agent 及其角色/模型 |
| `/skills` | 列出已載入的 skills |
| `/config` | 顯示當前 team 的有效設定 |
| `/model <name>` | 動態切換模型（不需退出重啟） |
| `/verbose` | 切換 verbose 模式 |
| `/save <path>` | 匯出對話歷史到檔案 |
| `/undo` | 回滾上一輪對話（刪除最後一輪的歷史） |
| `/retry` | 用相同 prompt 重試上一輪 |
| `/edit` | 編輯上一個 prompt 並重新提交 |

**實作位置**：`cmd/hufu/chat.go` — REPL switch 增加 case。

### 5.2 多行輸入支援

**問題**：chat REPL 只能單行輸入，貼上多行 prompt 時會提前送出。

**方案**：

1. 偵測貼上（bracketed paste mode）：`\e[200~...\e[201~` 之間的內容視為單一輸入。
2. 或用 `\` 結尾表示續行。
3. `readline` 庫 `ergochat/readline` 已支援 bracketed paste，需確認是否已啟用。

**實作位置**：`internal/readline/` — 確認 paste mode 設定。

### 5.3 互動式團隊切換

**問題**：`/team` 指令用 `promptui.Select`，但 readline 已接管 stdin，可能衝突。

**方案**：

1. `/team <name>` 直接切換（不需選單）。
2. `/team` 不帶參數時用 readline 的 tab completion 列出可用 team。
3. 移除 `askUserForTeamWithPromptUI` 在 chat 中的使用。

**實作位置**：`cmd/hufu/chat.go` — `switchChatTeam()`。

### 5.4 回應格式化

**問題**：coordinator 的回應直接 `fmt.Println(result)` 到 stdout，沒有語法高亮或 markdown 渲染。

**方案**：

1. 偵測終端能力，如果支援：
   - 用 [glamour](https://github.com/charmbracelet/glamour) 渲染 markdown 回應。
   - 程式碼區塊加語法高亮。
2. `--no-render` flag 停用渲染（原始 markdown 輸出）。
3. 非 chat 模式也可用 `--render` flag 啟用。

**實作位置**：`cmd/hufu/chat.go` + `cmd/hufu/main.go` — result 輸出前。

---

## 六、子命令 UX 改善

### 6.1 `hufu list` 改善

**問題**：list 輸出是純文字，無 JSON 格式，無表格對齊。skill 和 tool 資訊顯示不一致。

**方案**：

1. `--output json` 支援：機器可讀的 team/agent 結構。
2. `--output table`：用 `lipgloss.Table` 或自定義對齊渲染。
3. `--verbose` 顯示完整 agent .md frontmatter（含 guard、mcp-tools 等）。
4. `--skills` flag 只顯示 skill 相關資訊。
5. 加上 team description 顯示（目前只顯示名稱和目錄）。

**實作位置**：`cmd/hufu/list.go`。

### 6.2 `hufu doctor` 改善

**問題**：doctor 只檢查 provider 和 workspace，不檢查磁碟空間、模型大小、記憶體等。

**方案**：

新增檢查項：

| 檢查項 | 說明 |
|--------|------|
| 磁碟空間 | workspace 所在分區可用空間 < 1GB 時警告 |
| 模型大小 | 列出每個可用模型的大小（Ollama API `ollama show --size`） |
| 記憶體 | 系統可用 RAM vs 模型需求粗估 |
| 環境變數 | 檢查 `HUFU_*` 環境變數 |
| 衝突設定 | 偵測 hufu.yaml 中衝突的設定（如同時設 `no-net` 和 `force-mcp`） |
| MCP 伺服器 | 如果 team 配置了 MCP servers，檢查其可連性 |

**實作位置**：`cmd/hufu/doctor.go`。

### 6.3 `hufu init` 模板擴充

**問題**：`hufu init` 只有一個 `default` 模板（coordinator + 1 helper）。

**方案**：

| 模板名 | 內容 |
|--------|------|
| `default` | 現有：1 helper worker |
| `dev` | coordinator + developer + reviewer + tester（含 guard rules） |
| `research` | coordinator + researcher + writer |
| `ops` | coordinator + operator（bash/sudo/ssh tools）+ monitor |
| `minimal` | 只有 team.yaml，無 agent .md（使用者自建） |

**實作位置**：`cmd/hufu/initcmd.go` — 新增模板字串 + switch case。

### 6.4 `hufu team generate` LLM 輔助生成

**問題**：`team generate` 使用硬編碼的 task category 分類，生成的 team 可能不符合需求。

**方案**：

1. `--use-llm` flag：用 sidecar model 分析 prompt 並生成 team.yaml + agent .md。
2. 生成的 team 經過 `validateGeneratedTeam` 驗證後才寫入。
3. 不帶 `--use-llm` 時維持現有行為（向下相容）。

**實作位置**：`cmd/hufu/teamcmd.go` — `runTeamGenerate()`。

### 6.5 Shell Completion flag 值補全

**問題**：tab completion 只補全 flag 名稱和 `@team`/`@agent`，不補全 flag 值（如 `--output text|json`）。

**方案**：

1. 為關鍵 flag 註冊 `RegisterFlagCompletionFunc`：
   - `--output`：`text`, `json`
   - `--model`：從 provider API 動態取得
   - `--agent-team`：已有
   - `--profile`：從 hufu.yaml 讀取
   - `--template`：從 `.hufu-templates/` 掃描
2. nushell completion 腳本同步更新。

**實作位置**：`cmd/hufu/main.go` — flag registration 區段 + `cmd/hufu/completion.go`。

---

## 七、跨面向改善

### 7.1 stdout/stderr 一致性審計

**問題**：結果與狀態輸出分散在多條路徑；應以測試固定 stdout/stderr 契約。現有 `--output json` 已走專用輸出路徑，尚未證實有非 JSON 洩漏。

**方案**：

1. 確立規則：**結果 → stdout，所有其他輸出 → stderr**。
2. 全專案審計 `fmt.Print` / `fmt.Println` / `fmt.Printf`，確認狀態輸出不誤入 stdout。
3. 以整合測試驗證 `--output json` 的 stdout 恰有一份可解析 JSON，且 status/event 僅在 stderr。

**實作位置**：全域審計 + 修復。

### 7.2 `hufu version` 子命令

**問題**：版本只能用 `--version` flag 取得，不符合 POSIX 慣例的 `hufu version` 子命令。

**方案**：

新增 `hufu version` 子命令，輸出版本、build time、Go version、commit hash（如有）。

**實作位置**：`cmd/hufu/main.go`。

### 7.3 互動式 prompt 改善

**問題**：`askUserForPrompt()` 在無 prompt 參數時顯示簡單的 `> ` 提示，缺乏引導。

**方案**：

1. 多行提示：
   ```
   ─── Enter Prompt ───

   Describe the task for your agent team.

   Tips:
     · @team-name <task>     run a specific team
     · @agent-name <task>    target a specific agent
     · #filename             inject file contents
     · Type your task and press Enter

   >
   ```
2. 如果有已發現的 team，顯示可用 team 名稱。
3. 支援 `Ctrl+R` 搜尋歷史 prompt。

**實作位置**：`cmd/hufu/main.go` — `askUserForPrompt()`。

### 7.4 錯誤訊息改善

**問題**：部分錯誤訊息只說「failed to do X」而不提示如何修復。

**方案**：

為常見錯誤加上修復提示：

| 錯誤 | 附加提示 |
|------|---------|
| `no teams found` | `Run 'hufu init <team>' to create one, or use --default` |
| `provider unreachable` | `Is Ollama running? Try 'ollama serve'. Check --provider-url` |
| `model not in provider's list` | `Pull it first: 'ollama pull <model>'. Or use --model <available-model>` |
| `team not found: X` | `Available: A, B, C. Run 'hufu list' for details.` |
| `workspace not writable` | `Check permissions on <path>, or use --workspace <writable-path>` |

**實作位置**：全域 — 搜尋 `return fmt.Errorf` 和 `fmt.Fprintf(os.Stderr, ...)` 加上 hint。

---

## 八、優先級與排序

| 優先級 | 項目 | 預期投入 | 影響面 |
|--------|------|---------|--------|
| 🔴 P0 | 4.1 非終端環境安全降級（auto/plain） | 2-3h | CI/管道使用者 |
| 🔴 P0 | 2.4 `NO_COLOR` / `--no-color` 支援 | 1-2h | 所有 CI 使用者 |
| 🔴 P0 | 7.1 stdout/stderr 契約測試與審計 | 2-3h | 腳本整合可靠性 |
| 🟡 P1 | 3.1 終端寬度適應 | 3-4h | TUI 可用性 |
| 🟡 P1 | 3.3 Detail log 上限與完整匯出來源 | 3-4h | TUI 穩定性 |
| 🟡 P1 | 3.8 overlay 文件同步 | 1h | 文件正確性 |
| 🟡 P1 | 4.3 執行結束摘要 | 2h | CLI 體驗 |
| 🟡 P1 | 2.3 typed errors 與退出碼細化 | 1-2d | 腳本整合 |
| 🟡 P1 | 3.10 TUI 拆檔 | 4-6h | 維護性 |
| 🟡 P1 | 5.1 REPL 指令擴充 | 3h | chat 使用者 |
| 🟡 P1 | 2.5 `hufu config` 命令 | 4h | 設定管理 |
| 🟢 P2 | 2.1 Flag 分群顯示 | 3h | 新手體驗 |
| 🟢 P2 | 3.4 進度概覽列 | 2h | TUI 體驗 |
| 🟢 P2 | 3.6 等待動畫改善 | 1h | TUI 體驗 |
| 🟢 P2 | 6.2 doctor 擴充 | 3h | 診斷體驗 |
| 🟢 P2 | 6.3 init 模板擴充 | 2h | 快速上手 |
| 🟢 P2 | 5.4 回應 markdown 渲染 | 2h | chat 體驗 |
| 🟢 P2 | 2.6 `hufu status` 命令 | 2h | 可觀測性 |
| 🟢 P2 | 4.2 結構化事件 JSONL | 3-4h | CI/事件整合 |
| ⚪ P3 | 2.7 `hufu runs` / `sessions` 命令 | 4-6h | 跨執行可觀測性 |
| ⚪ P3 | 2.2 flag 共用機制重構 | 4h | 維護性 |
| ⚪ P3 | 3.2 任務篩選與排序 | 6h | TUI 進階 |
| ⚪ P3 | 3.5 Result box 展開 | 2h | TUI 體驗 |
| ⚪ P3 | 3.7 鍵盤快捷鍵一致性 | 2h | TUI 一致性 |
| ⚪ P3 | 3.9 TUI 設定檔 | 3h | 個人化 |
| ⚪ P3 | 4.4 `--think` 視覺分離 | 1h | debug 體驗 |
| ⚪ P3 | 5.2 多行輸入 | 2h | chat 體驗 |
| ⚪ P3 | 5.3 互動式團隊切換 | 1h | chat 體驗 |
| ⚪ P3 | 6.1 `hufu list` 改善 | 2h | 列表體驗 |
| ⚪ P3 | 6.4 team generate LLM 輔助 | 4h | 進階生成 |
| ⚪ P3 | 6.5 Shell completion flag 值補全 | 2h | shell 整合 |
| ⚪ P3 | 7.2 `hufu version` 子命令 | 0.5h | 慣例 |
| ⚪ P3 | 7.3 互動式 prompt 改善 | 1h | 新手體驗 |
| ⚪ P3 | 7.4 錯誤訊息改善 | 2h | 除錯體驗 |

---

## 九、驗證方式

1. **CLI 改善**：新增 `cmd/hufu/usability_test.go` 測試案例覆蓋每項變更。
2. **TUI 改善**：用 `tea.KeyMsg` / `tea.WindowSizeMsg` 模擬按鍵和視窗變化，assert `View()` 輸出。
3. **非終端降級**：用 pipe 重導向執行 `hufu --dry-run 2>&1 | cat` 確認無 ANSI 殘留。
4. **退出碼**：用 shell script 驗證每個退出碼路徑。
5. **一致性審計**：`grep -rn 'fmt.Print' cmd/hufu/*.go | grep -v Stderr` 確認無 stdout 洩漏。

---

## 十、向後相容性

- 所有新增 flag 預設值不改變現有行為。
- 新增子命令不影響現有子命令。
- TUI 拆檔為純檔案重組，`Model` struct 介面不變。
- 退出碼細化：原有的 0/1/130 保持，新增 2-7 為更細分類。
- `NO_COLOR` 環境變數為業界標準，不影響未設定的使用者。
