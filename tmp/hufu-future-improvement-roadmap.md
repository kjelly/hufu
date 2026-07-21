# hufu 未來改進方向與工程 Roadmap

> 文件目的：整理 hufu 作為 Go 多代理協作與工作流引擎的整體演進方向，供未來規劃架構重整、功能開發、相容性遷移、測試與 agent team 生態。
>
> 適用範圍：本文件是 **hufu 全產品／全架構 roadmap**，不只處理特定 runbook 或 `pilot` 驗證情境。
>
> 伴隨文件：`hufu-strict-verification-workflow-improvement.md` 是高約束、可稽核 workflow 的**領域通用機制設計與驗收案例**（required skill lock、fail-closed policy、evidence gate、cache/recovery 語意等）。分工規則：**Go 子系統的 schema、詞彙與實作狀態以本文件工作卡為單一真相來源**；伴隨文件提供對應的詳細設計與驗收案例（各卡以「詳細設計見 strict 文件 §x.y」引用）。本文件則涵蓋 session、context、workflow kernel、extension、SDK/RPC、TUI、observability、memory 與通用 agent teams 的 backlog。
>
> 分析基準：**HEAD `b9b47557`（2026-07-21，`feat: add structured conversation compaction with token-aware history management`）**。所有工作項目已對照此 commit 逐一標記落地狀態。**每次相關 PR 合入後，必須更新本文件的狀態欄位與基準 commit**，避免將已修正的問題重做。

---

## 0. 如何使用本文件指派 agent 工作

**狀態圖例**：

| 符號 | 意義 | 可否指派 |
|---|---|---|
| ✅ DONE | 已在 HEAD 完成，僅供參考 | ❌ 勿重做 |
| 🟡 PARTIAL | 部分落地，卡上註明殘餘缺口 | ✅ 只能做殘餘部分 |
| 🟡 IMPLEMENTED (PENDING REVIEW) | 已實作完成，待 Review | ⏳ 待 Review |
| 🔴 OPEN | 未動工 | ✅ |

**指派流程**：

1. **每個 `HF-PR-xxx` 是一張自包含工作卡**（見 Part XII），內含：狀態、工作量、依賴、背景、既有基礎、任務、驗收條件、驗證指令、指派指令。agent 只需讀自己的那張卡加上引用的設計小節（`§N`），不需讀完整份文件。
2. **指派方式**：複製該卡的「指派指令」區塊餵給 agent（例如 `hufu "<指派指令>"`），或直接叫 agent 讀本文件的對應小節。
3. **依賴檢查**：依賴欄位列出的 PR 必須先 DONE 才能指派該卡。
4. **驗收**：agent 回報完成後，執行卡上的「驗證指令」確認，不要只信回報文字。合入後回本文件把狀態改為 ✅ 並更新基準 commit。
5. **工作量尺度**：S = 半天內（單一檔案、repo 內有現成模式）；M = 1–3 天（跨數檔案、需新測試）；L = 約 1–2 週（新子系統）；XL = 數週（新 kernel／協議）。
6. **文件結構**：§3 是問題總表（含狀態）；§3.1 是既有基礎（防止重複設計）；Part I–X 是各主題設計細節（每節開頭有落地狀態橫幅）；Part XI 是 Phase 總覽；Part XII 是工作卡本體；Part XVI 是建議指派順序。

---

## 1. 北極星定位

hufu 不應演進成另一個「單代理 terminal coding agent」，也不應只靠持續增加 coordinator 功能與內建工具擴張。

建議的長期定位是：

> **Pi-like kernel，hufu-like brain。**

（註：「Pi」指極簡 kernel + extension 架構的 agent runtime 風格——核心只負責 session／tool loop／RPC，其餘能力全部以可替換 extension 提供。若原意另有所指，請文件作者補上確切參照連結。）

意思是：

- 底層 kernel 應具備乾淨、可嵌入、可 replay、可分支、token-aware、extension-friendly 的 runtime。
- 上層保留 hufu 最有差異化的能力：
  - coordinator／worker teams
  - DAG task scheduling
  - objective verification
  - retry／model escalation
  - judge／skeptic／reflexion
  - unattended operation
  - durable task recovery
  - MCP 與基礎設施操作

最終產品可被描述為：

> **一個以 Go 實作、local-first、可驗證、可恢復、可擴充的 durable multi-agent workflow engine。**

---

## 2. hufu 現有優勢

應優先保留並強化，而不是重寫掉的能力：

### 2.1 原生多代理編排

- coordinator 與 worker team
- agent definition
- direct agent invocation
- team switching
- parallel worker dispatch
- agent-specific tools、skills、model、timeout、guard

### 2.2 任務層工作流

- `TaskDef`
- dependency／pipeline
- DAG scheduler
- bounded concurrency
- `on_failure` reset wave
- retry
- model escalation
- duplicate detection
- task result cache

### 2.3 客觀驗證與品質控制

- shell verification
- plan-first
- plan reviewer
- judge
- skeptic／adversarial verification
- reflexion hint
- acceptance command

### 2.4 操作可靠性

- task journal
- task checkpoint
- interrupted task resume
- status files
- audit logs
- timeout／token／wall-clock budget
- unattended mode

### 2.5 Local-first 與 Go 部署優勢

- 單一 binary
- 適合 Linux、DevOps、CI、Kubernetes 與自架環境
- 能直接整合 OS、SSH、MCP、Ollama 與 OpenAI-compatible providers
- 相較 Node extension runtime，部署與供應鏈範圍較容易控制

---

## 3. 目前最需要改善的核心問題

| ID | 優先級 | 狀態（基準 `b9b47557`） | 問題 |
|---|---:|---|---|
| HF-CTX-001 | P0 | ✅ 已修復（`5f97001`）；殘餘見 HF-PR-001R | session 恢復可能遺失最近內容或保留錯誤時間區段 |
| HF-CTX-002 | P0 | ✅ 已完成（工作區未合入） | context budget 已實作 model-aware token budget (TokenCounter, ModelContextSpec, ContextBudget)；report breakdown 已接線 `--report`、`estimated` 已 log、compaction token 計數已 model-aware |
| HF-CTX-003 | P0 | ✅ 已完成（工作區未合入） | compaction 對 tool call/result、artifact、verification 保留不足；殘餘 HF-PR-103 已實作完成（ValidateStructuredSummary 5 項 deterministic 檢查、UserCorrections/SourceEntryIDs 欄位、fallback 保留舊 summary），已 Review 通過 |
| HF-STATE-001 | P0 | 🟡 IMPLEMENTED (PENDING REVIEW) | session、history、STM、LTM、journal、audit 多份狀態缺少單一真相來源；HF-PR-104 已實作 append-only event store (RunEvent, SHA-256 hash chain, dual-write telemetry, state reducers)，待 Review |
| HF-STATE-002 | P0 | 🟡 IMPLEMENTED (PENDING REVIEW) | snapshot write 與 crash recovery 一致性不足；HF-PR-002 已實作 AtomicWriteFile (temp write + fsync + atomic rename + dir sync) 與 crash recovery 測試，待 Review |
| HF-CACHE-001 | P0 | 🟡 PARTIAL（identity 已含 verify+mode+generation；缺 CachePolicy 與環境指紋） | cache 缺少 workspace/source/environment freshness |
| HF-RECOVERY-001 | P0 | 🔴 OPEN（風險最高，建議升入下一批 P0） | interrupted task resume 未完整區分可安全重跑與非冪等副作用 |
| HF-ARCH-001 | P1 | 🟡 PARTIAL（檔案已拆分 `58e5a54`；struct 層面介面解耦未做） | `Coordinator` 聚合過多責任，難測試、難嵌入、難擴充 |
| HF-WF-001 | P1 | 🔴 OPEN | worker output 主要是自由文字，缺少 typed task result |
| HF-WF-002 | P1 | 🔴 OPEN（`--default` 已是 fast path 雛形，實作時應認領整合） | 簡單任務與複雜 team workflow 沒有明確 fast/team path |
| HF-PLUGIN-001 | P1 | 🔴 OPEN | hooks、skills、MCP 尚未形成穩定公開 extension contract |
| HF-SESSION-001 | P1 | 🔴 OPEN（大型投資，依賴 event store，建議補價值論證後再排程） | 缺少 session branch、fork、checkpoint、label 與 time travel |
| HF-MEM-001 | P1 | 🔴 OPEN | STM/LTM/RAG 缺少完整 provenance、confidence、supersession |
| HF-OBS-001 | P1 | 🟡 PARTIAL（`hufu improve` 已有 telemetry/A-B；缺 trace/replay/fault-injection/eval） | 缺少完整 trace、replay、context inspector 與 agent eval |
| HF-UX-001 | P2 | 🔴 OPEN | TUI 對 context、DAG、terminal lifecycle、e-paper 顯示支援不足 |
| HF-TEAM-001 | P2 | 🔴 OPEN（`hufu team` 目前只有 `generate`／`permissions`） | bundled teams 缺少 schema lint、版本、合約與標準輸出 |
| HF-WORKSPACE-001 | P1 | 🔴 OPEN（源自 strict 文件，詳細設計見其 §5.10） | control workspace 與 subject workspace 無強制隔離，ground-up cleanup 可能刪除 hufu 自己的 checkpoint/evidence |
| HF-SECRET-001 | P1 | 🔴 OPEN（源自 strict 文件，詳細設計見其 §5.11） | audit/tool input/MCP/report 缺少統一 secret-aware redaction |
| HF-TERM-001 | P1 | 🔴 OPEN（源自 strict 文件，詳細設計見其 §5.12；lifecycle event 依賴 HF-PR-104） | stateful terminal session 不是第一級 task resource，long-running process lifecycle 與 agent timeout/resume 混淆 |
| HF-PROFILE-001 | P2 | 🔴 OPEN（源自 strict 文件，詳細設計見其 §5.15、§7.2；依賴 HF-PR-003/006） | cache／acceptance／memory 開關散落各 flag，無具名 execution profile；fresh verification 無法一鍵隔離舊 memory |

---

## 3.1 既有基礎（指派前必讀，避免重複設計）

以下能力**已存在於 HEAD**。後續工作卡應在其上延伸，而非從零設計：

| 既有基礎 | 位置／來源 | 可重用之處 |
|---|---|---|
| Structured compaction | `internal/team/compaction.go`（`b9b4755`） | 13-section `StructuredSummary`、7 條 `EnforceCompactionInvariants`、`CompactionRecord`（tokens_before/after、SourceRange）、`compaction_history.json` 持久化、restart-safe source offsets |
| Session recency 修正 | `internal/team/session.go`（`5f97001`） | `ContextSummary()` 已改為保留最近 `maxSessionEntries`（40）筆 |
| Task journal atomic 寫法 | `internal/team/task_journal.go` | temp+rename compaction、tombstone 刪除語意——**HF-PR-002 直接照抄此模式** |
| Task cache identity | `internal/team/coordinator_taskcache.go` | identity 已含 agent + 正規化 desc + verify command + verifyMode + per-round generation；`on_failure` re-drive 會 invalidate；journal tombstone 防止重啟復活 |
| Capability preflight | `internal/team/capability.go` | `CapabilityResult`（scope/evidence/CheckedAt）——§12.4 的雛形，只差 TTL／generation／invalidation |
| Verification modes | `verify` + `VerifyMode`（success／expected_failure／observation，`d032426`） | §16 evidence／acceptance 設計應與此整合 |
| Unattended acceptance + rollback | `58792a0` | finish gate、self-healing retries、git rollback——HF-PR-006 在此基礎上加 advisory／blocking 語意 |
| Crash-resume checkpoint | `ResumeInterruptedTasks`、`TodoList.onChange → saveCheckpoint` | HF-PR-107 的 side-effect policy 掛在這裡 |
| Telemetry／A-B experiment | `internal/improve/`（`hufu improve`） | Part VII observability 與 Part XIV 成功指標的基線來源 |
| Coordinator 檔案拆分 | `internal/team/coordinator_*.go`（`58e5a54`） | HF-ARCH-001 的下一步是 struct 層面介面解耦，不是再拆檔案 |
| Fast path 雛形 | `--default`（內建 coordinator+Helper）、`--auto-team` | HF-PR-207 的 ExecutionRouter 從此長出 |

---

# Part I：Context 與 Session Kernel

## 4. 立即修正 session recency correctness

> **落地狀態：✅ 已修復（`5f97001`）**。`SessionData.ContextSummary()`（`internal/team/session.go`）已改為保留最近 `maxSessionEntries`（40）筆，附「older exchanges omitted」標記。
> **殘餘缺口（見工作卡 HF-PR-001R）**：`ContextSummary` 無 head 保留——超過 40 筆後最初 user goal 會從 resume context 消失，目前僅靠 structured compaction 的 Goal invariant 間接保存。以下 §4.1–4.3 為原始分析，保留存檔。

### 4.1 問題

先前檢視的 `SessionData.ContextSummary()` 會由 entries 開頭開始輸出，超過上限時停止。由於 entries 是持續 append，長 session 恢復時可能保留最舊資料、遺失最近幾輪。

這會造成：

- 最新 user correction 消失
- 最近架構決策消失
- 最近失敗原因與修復方式消失
- coordinator 重新委派已完成工作
- resume 與不中斷執行產生不同結果

### 4.2 修正原則

```go
func (s *SessionData) RecentEntries(limit int) []SessionEntry {
    if limit <= 0 || len(s.Entries) == 0 {
        return nil
    }
    start := len(s.Entries) - limit
    if start < 0 {
        start = 0
    }
    out := make([]SessionEntry, len(s.Entries)-start)
    copy(out, s.Entries[start:])
    return out
}
```

若仍需要保留最初 goal，應明確採：

```text
head: 原始 user goal、核心 constraints
tail: 最近 N exchanges
```

而不是不透明地只取最舊或最新。

### 4.3 驗收條件

- 41、80、200、1,000 筆 entry 測試。
- 最新 user correction 必須存在。
- 最初 user goal 必須存在或由 structured summary 保存。
- resumed session 的核心 context 與 uninterrupted session 等價。

---

## 5. Model-aware token budget

> **落地狀態：✅ 已完成（工作區未合入）**。`internal/team/token_counter.go` 已落地 `TokenCounter` interface、`ModelContextSpec` registry（內建 gpt-4o／claude／qwen／llama 等 context window，provider 前綴剝離與未知模型 fallback）、`DefaultTokenCounter`（CJK／code／ascii 密度估算 + 保守 margin）、`CalculateContextBudget`、`ContextUsageBreakdown`／`BreakdownReport`、`IsContextOverflowError`。`context_budget.go` 的 `capStepMessages` 已改為 `CapStepMessagesWithCounter`（token 預算，雙 pass squeeze），固定 `120_000` chars 預算已移除；`coordinator_task_run.go` 的 `PrepareStep` 改用 model spec token budget，並在 `Stream` 回傳 context overflow 時自動 compact＋retry 一次。`compaction.go` 的 `countTokensInText`／`countTokensInMessages` 改走 `defaultCounter`。
> **殘餘缺口**：無。原三項殘餘已全數修復——(1) `BreakdownReport` 已接線至 `cmd/hufu/report.go` 的 `--report` 輸出（`Coordinator.RenderContextUsageSection` → `reportData.ContextUsageSection`，§5.4 breakdown 於報告中渲染，estimated 模型加註）；(2) `estimated` 已 log（`warnEstimatedOnce` 每個 model 每行程序警告一次，`estimatedModelLogger` 可替換）；(3) compaction 的 `countTokensInText`／`countTokensInMessages` 已改為 model-aware（`coordinatorModelID()` 傳入），`CompactionRecord` 的 tokens_before/after 採同一 estimator。以下 §5.1–5.4 為原始設計，實作以 `token_counter.go`／`coordinator_context_report.go` 為準。

### 5.1 問題

固定字元數無法可靠代表 token：

- 中文與英文比例不同
- 程式碼、JSON、YAML token 密度不同
- tokenizer 因 provider/model 而異
- tool schemas 與 system prompt 也占 context
- output reserve 必須依 model 設定
- image／reasoning／cache usage 也可能影響實際限制

### 5.2 建議介面

```go
type TokenCounter interface {
    CountText(ctx context.Context, modelID, text string) (int, error)
    CountMessages(ctx context.Context, modelID string, messages []Message) (int, error)
    CountTools(ctx context.Context, modelID string, tools []ToolSchema) (int, error)
}

type ModelContextSpec struct {
    ModelID            string
    ContextWindow      int
    MaxOutputTokens    int
    SafetyMarginTokens int
    Estimator          string
}

type ContextBudget struct {
    Window        int
    System        int
    Tools         int
    ReservedReply int
    SafetyMargin  int
    Available     int
}
```

### 5.3 不支援 tokenizer 的模型

可提供估算 fallback，但必須：

- 依 model family 使用不同 estimator
- 提供 conservative margin
- 記錄 estimated 而非 exact
- 允許 provider 回傳 context spec
- context overflow 後能自動縮減並 retry 一次

### 5.4 Context budget report

TUI/report 顯示：

```text
Context usage: 47,820 / 65,536

System instructions       8,210
Tool schemas              4,540
Recent conversation      18,200
Compacted history         5,430
Project context           3,210
STM/LTM/RAG               4,870
Task dependency results   3,360
Reply reserve            12,000
```

---

## 6. Structured compaction

> **落地狀態：✅ 已完成（工作區未合入）**。`internal/team/compaction.go` 已完整實作 `StructuredSummary` (含 `UserCorrections`、`SourceEntryIDs`)、7 條 Invariants、`ValidateStructuredSummary` (5 項 post-compaction deterministic 檢查: goal 不為空、active task ID 存在、modified artifact Traceable、user correction 存在、failed task 不被標 done；失敗時 fallback 不覆寫舊 summary)、`CompactionRecord` 持久化與 restart-safe counts/offsets。已 Review 通過。
> **殘餘缺口**：無。以下 schema 與 invariants 為設計原稿，實作以 `compaction.go` 為準。

### 6.1 問題

若 compaction 只串接自然語言 `TextPart`，會遺失：

- tool call 名稱與 arguments
- tool result
- command exit code
- read/modified files
- verification failure
- artifact path
- task state
- model／agent attribution

模型可能記得「做過」，卻失去「做了什麼、證據在哪裡」。

### 6.2 建議 summary schema

```go
type ConversationSummary struct {
    Goal              string
    Constraints       []string
    UserCorrections   []string

    CompletedTasks    []TaskSummary
    InProgressTasks   []TaskSummary
    BlockedTasks      []TaskSummary

    Decisions         []DecisionSummary
    Findings          []FindingSummary
    ErrorsAndFixes    []ErrorFixSummary
    OpenQuestions     []string
    NextActions       []string

    FilesRead         []FileRef
    FilesModified     []FileRef
    Artifacts         []ArtifactRef
    Verification      []VerificationSummary

    SourceEntryIDs    []string
    TokensBefore      int
    TokensAfter       int
}
```

### 6.3 Compaction invariants

- 不拆開 tool call/result pairing。
- 不壓縮掉最初 goal。
- 不壓縮掉最新 user correction。
- 保留未完成與 blocked task。
- 保留 verification failure。
- 保留 modified files 與 artifact refs。
- 下一次 compaction 以舊 summary 為 input，不能無限累積失真。
- 完整原始 history 仍保留於 durable session log。

### 6.4 Summary validation

compaction 完成後，可由 deterministic validator 檢查：

- 所有 active task ID 是否仍存在
- 所有 modified artifact 是否仍可追溯
- 最新 user correction 是否存在
- summary 是否超出 budget
- 是否錯把 failed task 標成 done

---

## 7. ContextCompiler

### 7.1 目的

將目前分散於 coordinator、worker injection、STM/LTM、skills、AGENTS.md 的 prompt 拼接邏輯抽成一個獨立子系統。

### 7.2 Pipeline

```text
Collect
  → Normalize
  → Scope
  → Deduplicate
  → Retrieve
  → Rank
  → Compact
  → Budget
  → Validate
  → Emit
```

### 7.3 Context item

```go
type ContextItem struct {
    ID           string
    Kind         string
    Content      string
    Source       string
    Scope        ContextScope
    Priority     int
    TokenCount   int
    Confidence   float64
    Freshness    time.Time
    Provenance   []string
    DedupKey     string
    Compressible bool
    Required     bool
}
```

### 7.4 Worker context 優先順序

建議：

1. user goal
2. hard constraints
3. approved plan
4. agent core instructions
5. project instructions
6. dependency task results
7. current task verification criteria
8. recent STM findings/decisions/errors
9. relevant LTM/RAG
10. concurrent task summaries
11. general historical context

超過 budget 時：

- 不可移除 1–7
- 先移除／壓縮 11、10、9
- 不按檔案順序或單純字串尾端裁切

### 7.5 Context provenance

TUI 可顯示：

```text
Why is this in context?
- Source: task 17 / researcher
- Category: finding
- Confidence: 0.91
- Last confirmed: 2026-07-17
- Files: internal/team/coordinator.go
```

---

## 8. Session tree、branch、fork 與 checkpoint

> **落地狀態：🔴 OPEN**。`SessionEntry` 無 `BranchID`／`ParentID`，session 歷史僅線性 archive。依賴 HF-PR-104（event store）。對應工作卡 HF-PR-201；屬大型投資，建議補上對 hufu 主要場景（unattended／CI）的價值論證後再排程。

### 8.1 需求

多代理系統的錯誤常發生在「任務拆解或架構選擇」，而不是單一訊息。因此 hufu 應支援：

- 從任一 coordinator round 建立 branch
- fork session 使用不同 team/model
- checkpoint label
- 回到 verification 前重新嘗試
- branch summary
- 比較兩個 branch 的 task/artifact/verification

### 8.2 Session entry 基礎

```go
type SessionEntry struct {
    ID        string
    ParentID  string
    BranchID  string
    Type      string
    Timestamp time.Time
    Payload   json.RawMessage
}
```

### 8.3 Branch-aware state

應 branch-scoped：

- active model
- thinking level
- active tools
- selected team
- task plan
- memory additions
- compaction
- labels
- artifacts

### 8.4 CLI／TUI

```text
hufu session list
hufu session tree
hufu session fork <entry-id>
hufu session checkout <entry-id>
hufu session label <entry-id> <name>
hufu session diff <branch-a> <branch-b>
```

---

# Part II：Durable State 與 Recovery

## 9. Append-only Session Event Store

> **落地狀態：🟡 IMPLEMENTED (PENDING REVIEW)**。已於 `internal/team/event_store.go` 實作 `RunEvent`（SchemaVersion, ID, PreviousID, RunID, SessionID, BranchID, TaskID, Attempt, Actor, Type, Timestamp, IdempotencyKey, Payload, PreviousHash, Hash）、`EventStore` (SHA-256 hash chain 驗證)、dual-write 機制、`event_reducers.go` (`ReduceToSessionData`, `ReduceToTodoList`)，單元測試與整合測試全綠，待 Review。對應工作卡 HF-PR-104。

### 9.1 問題

目前可能同時存在：

- session JSON
- conversation history
- chat history Markdown
- STM
- LTM
- vector memory
- task journal
- task files
- status files
- audit log
- report

這些各自有價值，但缺少統一 ordering、event ID、schema version 與 replay semantics。

### 9.2 目標

```text
Append-only Event Log
        │
        ├── Session projection
        ├── Conversation projection
        ├── Task/Todo projection
        ├── STM projection
        ├── LTM candidate projection
        ├── TUI projection
        ├── Audit projection
        └── Report projection
```

### 9.3 Event schema

```go
type RunEvent struct {
    SchemaVersion int             `json:"schema_version"`
    ID            string          `json:"id"`
    PreviousID    string          `json:"previous_id,omitempty"`
    RunID         string          `json:"run_id"`
    SessionID     string          `json:"session_id"`
    BranchID      string          `json:"branch_id,omitempty"`
    TaskID        string          `json:"task_id,omitempty"`
    Attempt       int             `json:"attempt,omitempty"`
    Actor         string          `json:"actor"`
    Type          string          `json:"type"`
    Timestamp     time.Time       `json:"timestamp"`
    IdempotencyKey string         `json:"idempotency_key,omitempty"`
    Payload       json.RawMessage `json:"payload"`
    PreviousHash  string          `json:"previous_hash,omitempty"`
    Hash          string          `json:"hash,omitempty"`
}
```

### 9.4 事件類型

```text
run_started
run_interrupted
run_finished

user_message_added
assistant_message_added
model_changed
tool_set_changed

task_created
task_started
task_completed
task_failed
task_blocked
task_reset
verification_started
verification_finished

tool_call_started
tool_call_finished

compaction_created
branch_created
leaf_changed
label_changed

memory_candidate_created
memory_confirmed
memory_superseded

artifact_created
evidence_validated
```

### 9.5 遷移策略

不要一次重寫所有 persistence：

1. 現有程式照常寫舊格式，同時 dual-write event log。
2. 寫 reducers 從 event log 重建 session/task/STM。
3. 在測試與實際 run 比對 legacy state 與 replay state。
4. 將 event log 升為 source of truth。
5. 舊檔案改為 projection/export。
6. 最後移除不必要的 duplicate state。

---

## 10. Atomic snapshot 與 schema migration

> **落地狀態：🟡 IMPLEMENTED (PENDING REVIEW)**。已實作 `internal/team/atomic_write.go` (`AtomicWriteFile`: temp write + `Sync()` + `os.Rename` + directory `SyncDir`)，`SaveSession`、`SaveSessionMD`、`SaveCompactionRecord` 已全數改用此 helper，並於 `atomic_write_test.go` 涵蓋 crash 恢復測試，待 Review。對應工作卡 HF-PR-002。

即使 event log 成為真相來源，仍可能需要 snapshot 加速啟動。

### 10.1 Atomic write

```text
write temp
fsync temp
rename
fsync directory
```

### 10.2 Snapshot metadata

```go
type SnapshotHeader struct {
    SchemaVersion int
    CreatedAt     time.Time
    LastEventID   string
    LastEventHash string
    Checksum      string
}
```

### 10.3 Migration

每個持久格式需要：

```go
type Migrator interface {
    FromVersion() int
    ToVersion() int
    Migrate(ctx context.Context, raw []byte) ([]byte, error)
}
```

不可靜默忽略不相容資料；應：

- 備份舊檔
- migration log
- dry-run
- rollback
- explicit error

---

## 11. Side-effect-aware recovery

> **落地狀態：🔴 OPEN（建議升入下一批 P0）**。`ResumeInterruptedTasks` 對所有非終態 task 一律原 ID 重跑，不區分副作用等級。對擁有 `ssh`／`sudo`／`bash` 工具且跑 unattended 的 hufu，這是唯一可能導致「自動重跑非冪等基礎設施操作」的缺口，風險實際高於多數 P0。對應工作卡 HF-PR-107。

### 11.1 問題

自動 resume 適合：

- read-only
- pure computation
- idempotent verification

但不一定適合：

- VM create/delete
- deploy
- credential change
- external API write
- database migration
- Git push

### 11.2 Task side-effect

```go
type SideEffectClass string

const (
    SideEffectNone           SideEffectClass = "none"
    SideEffectWorkspaceWrite SideEffectClass = "workspace_write"
    SideEffectExternalWrite  SideEffectClass = "external_write"
    SideEffectInfraMutation  SideEffectClass = "infra_mutation"
    SideEffectCredential     SideEffectClass = "credential_mutation"
)
```

### 11.3 Recovery policy

```go
type RecoveryPolicy string

const (
    RecoveryRetry     RecoveryPolicy = "retry"
    RecoveryReconcile RecoveryPolicy = "reconcile"
    RecoveryManual    RecoveryPolicy = "manual"
    RecoveryNever     RecoveryPolicy = "never"
)
```

### 11.4 Reconcile

每個非冪等 tool 可宣告：

```go
type ToolRecoverySpec struct {
    RetrySafe      bool
    IdempotencyKey string
    ReconcileTool  string
}
```

恢復時：

1. replay durable events
2. 找到 unfinished operation
3. 執行 read-only reconciliation
4. 判斷 complete／not_started／partial／unknown
5. 依 policy 決定完成、重試、補償或人工介入

---

## 12. Cache freshness 與明確語意

> **落地狀態：🟡 PARTIAL**。cache identity 已含 verify command + verifyMode + per-round generation，journal 有 tombstone、`on_failure` re-drive 會 invalidate（見 `coordinator_taskcache.go`、`dag_scheduler.go`）。仍缺：`CachePolicy`（use/refresh/bypass）明確語意、repo commit／file fingerprint／skill hash／model family 環境指紋、§12.4 capability TTL。對應工作卡 HF-PR-003、HF-PR-004。

### 12.1 Cache policy

```go
type CachePolicy string

const (
    CacheUse     CachePolicy = "use"
    CacheRefresh CachePolicy = "refresh"
    CacheBypass  CachePolicy = "bypass"
)
```

### 12.2 Cache identity

至少包含：

```text
agent identity/version
task goal
constraints
verify command/mode
repo commit
workspace generation
project file fingerprint
tool registry version
skill hashes
policy version
model family
dependency result hashes
```

### 12.3 何時禁止 cache

- 使用者明確說重新執行
- idempotency test
- live environment verification
- VM/deploy/credential mutation
- security check
- benchmark
- freshness-sensitive research
- source revision已變更

### 12.4 Capability cache

加入：

- TTL
- scope
- environment generation
- mutation-triggered invalidation
- `always-fresh` probe

---

# Part III：Workflow Engine

## 13. Typed TaskResult

> **落地狀態：🔴 OPEN**。worker output 仍是 free text（`TodoItem.Output string`），無 `submit_result` tool。對應工作卡 HF-PR-105。

### 13.1 問題

自由文字 output 對人類可讀，但難以：

- 驗證
- compaction
- retry
- judge
- report
- dependency injection
- cache identity
- artifact tracking

### 13.2 建議 schema

```go
type TaskResult struct {
    TaskID       string
    Agent        string
    Status       TaskStatus
    Summary      string

    Artifacts    []ArtifactRef
    Evidence     []EvidenceRef
    FilesRead    []FileRef
    FilesModified []FileRef

    Commands     []CommandResult
    Verification []VerificationResult

    Decisions    []Decision
    Findings     []Finding
    Risks        []Risk
    OpenQuestions []string
    SuggestedNextTasks []TaskProposal

    RetryHint    string
    RawOutputRef *ArtifactRef
}
```

### 13.3 相容性

Agent 可繼續輸出 Markdown，但在工具層加入：

```text
submit_result
```

讓 agent 提交 typed result。若沒有提交，compatibility parser 可從 final text 產生低信心結果：

```go
result.Confidence = 0.4
result.Source = "parsed_free_text"
```

嚴格 task 可要求：

```text
typed result mandatory
```

---

## 14. Workflow definition 與 plan contract

> **落地狀態：🔴 OPEN**。DAG scheduler 僅依 dependency 排程，無 `ResourceClaim`／resource lock 概念。對應工作卡 HF-PR-106。

### 14.1 需求

目前 coordinator 每一輪動態產生 tasks 很靈活，但缺少穩定、可檢查的 plan contract。

### 14.2 WorkflowPlan

```go
type WorkflowPlan struct {
    ID          string
    Goal        string
    Constraints []string
    Tasks       []TaskDef
    CreatedBy   string
    ApprovedBy  string
    Version     int
    Hash        string
}
```

### 14.3 Plan validation

Deterministic checks：

- unknown agent
- dependency cycle
- missing verification
- incompatible side effects/concurrency
- protected resource conflict
- no stop condition
- impossible timeout
- required skills/tools unavailable
- duplicate artifact outputs
- unsafe recovery policy

### 14.4 Resource locks

Task 可宣告：

```go
type ResourceClaim struct {
    Resource string
    Mode     string // read, write, exclusive
}
```

例如：

```text
vm/freeipa-server       exclusive
workspace/inventory     exclusive
repo/source             read
terminal/pty             exclusive
```

Scheduler 依 dependency 之外，也要依 resource claim 避免不安全並行。

---

## 15. Fast path 與 Team path

> **落地狀態：🔴 OPEN**。無 `ExecutionRouter`；`--auto-team` 只選 team 不選執行路徑。注意 `--default`（內建 coordinator+Helper）已是事實上的 fast path 雛形，實作時應認領整合而非另起爐灶。對應工作卡 HF-PR-207（新增）。

### 15.1 Fast path

```text
User → single capable agent → tools → typed result
```

適合：

- 單檔案修改
- 簡單查詢
- 小型重構
- 執行單一測試
- 明確、低風險命令

優點：

- latency 低
- token 低
- context 簡單
- 不需 coordinator overhead

### 15.2 Team path

```text
User → coordinator → plan → DAG → workers → verification → synthesis
```

適合：

- 跨模組修改
- research + implementation + review
- 基礎設施
- 長時間 unattended
- 多模型判定
- 有 acceptance criteria
- 需要 evidence chain

### 15.3 自動選擇

新增 `ExecutionRouter`：

```go
type ExecutionRoute string

const (
    RouteFast ExecutionRoute = "fast"
    RouteTeam ExecutionRoute = "team"
)

type RouteDecision struct {
    Route      ExecutionRoute
    Team       string
    Confidence float64
    Reasons    []string
}
```

先採 deterministic signals：

- 是否要求多角色
- 是否有多個 artifact
- 是否跨多檔案／模組
- 是否有 deploy、infra、security
- 是否需要 verify/acceptance
- 是否要求 debate/judge
- 預估 steps

LLM classifier 只作補充。

---

## 16. Verification、Evidence 與 Acceptance

> **落地狀態：🟡 PARTIAL**。`verify` command、`VerifyMode`（success／expected_failure／observation，`d032426`）、unattended acceptance + self-healing + rollback（`58792a0`）已存在。仍缺：evidence requirement schema、advisory／blocking 分級、evidence manifest。對應工作卡 HF-PR-006、HF-PR-108。

通用 hufu 也應將 evidence 視為第一級資料，而不只用於 pilot。

### 16.1 Evidence requirements

```go
type EvidenceRequirement struct {
    ID        string
    Kind      string
    Required  bool
    Validator string
    Expected  map[string]any
}
```

### 16.2 常用 evidence kind

```text
file_exists
file_hash
command_exit
test_report
coverage_report
git_diff
http_response
json_assertion
screenshot
trec_cast
transcript
artifact_schema
manual_approval
```

### 16.3 Acceptance

區分：

```text
advisory
blocking
```

Blocking acceptance 失敗時：

- run 不得 success
- final response 不得假裝完成
- exit code 非零
- 保留可重試與 evidence

---

# Part IV：架構模組化

## 17. 拆分 Coordinator God Object

> **落地狀態：🟡 PARTIAL**。檔案層面拆分已完成（`58e5a54`：`coordinator.go` 降至約 691 行 + 25 個職責檔如 `coordinator_task_run.go`、`coordinator_session.go` 等），但 `Coordinator` struct 仍聚合全部狀態與職責、無服務介面。下一步是**介面解耦**（Planner／WorkflowEngine／SessionStore 等），不是再拆檔案。無獨立 PR 卡——隨 HF-PR-104（SessionStore/EventStore）、HF-PR-102（ContextCompiler）等抽取自然演進，每次抽取遵循 §17.3 的漸進式原則。

### 17.1 建議服務

```go
type Coordinator struct {
    planner       Planner
    workflow      WorkflowEngine
    context       ContextCompiler
    sessions      SessionStore
    events        EventStore
    agents        AgentPool
    policies      PolicyEngine
    evidence      EvidenceService
    memory        MemoryService
    terminal      TerminalManager
    observer      EventSink
}
```

### 17.2 建議 package layout

```text
internal/
├── kernel/
│   ├── events/
│   ├── session/
│   ├── snapshot/
│   └── migration/
├── runtime/
│   ├── agentloop/
│   ├── provider/
│   ├── tools/
│   ├── terminal/
│   └── resources/
├── workflow/
│   ├── plan/
│   ├── scheduler/
│   ├── task/
│   ├── recovery/
│   └── cache/
├── context/
│   ├── compiler/
│   ├── tokenizer/
│   ├── compaction/
│   └── retrieval/
├── memory/
│   ├── records/
│   ├── retrieval/
│   └── projection/
├── policy/
│   ├── engine/
│   ├── command/
│   ├── filesystem/
│   └── secrets/
├── evidence/
│   ├── artifacts/
│   ├── validators/
│   └── manifest/
├── extension/
│   ├── registry/
│   ├── rpc/
│   └── wasm/
├── teams/
│   ├── loader/
│   ├── schema/
│   └── lint/
└── ui/
    ├── tui/
    ├── theme/
    └── rpc/
```

### 17.3 漸進式抽取

1. 保留 `Coordinator` public API。
2. 先抽 `TaskCacheService`。
3. 再抽 `ContextCompiler`。
4. 抽 `SessionStore/EventStore`。
5. 抽 `WorkflowEngine`。
6. Coordinator 最後變成 façade。
7. 每次抽取都有 legacy adapter 與 regression test。

不要同時重寫 scheduler、session、context 與 TUI。

---

# Part V：Extension、SDK 與平台化

## 18. 公開 extension contract

> **落地狀態：🔴 OPEN**。`internal/hooks` 存在，但無 versioned manifest／RPC／WASM。對應工作卡 HF-PR-203（依賴 HF-PR-109 policy engine）。

### 18.1 建議 extension points

```text
Provider
Tool
ContextSource
ContextTransformer
MemoryStore
Planner
SchedulerPolicy
Reviewer
Verifier
Policy
SessionStore
EventSink
ArtifactValidator
TeamResolver
```

### 18.2 不建議以 Go plugin 為主要方案

原因：

- Go ABI/version 限制
- 跨平台困難
- 編譯器版本耦合
- 安全隔離差
- 發布與依賴管理不易

### 18.3 建議組合

| 類型 | 技術 |
|---|---|
| 外部工具 | MCP |
| 跨語言 extension | JSON-RPC／stdio RPC |
| sandbox policy/context transform | WASM |
| 官方高效 extension | 編譯期 Go interface |
| prompt workflow | Skills |
| UI／產品整合 | RPC／event stream |

### 18.4 Versioned extension manifest

```yaml
apiVersion: hufu.io/v1alpha1
kind: Extension
metadata:
  name: example
spec:
  protocol: stdio-jsonrpc
  capabilities:
    - context-transform
    - artifact-validator
  permissions:
    filesystem:
      read: ["$PROJECT/**"]
      write: []
    network: false
```

---

## 19. SDK 與 RPC

> **落地狀態：🔴 OPEN**。無 SDK／RPC mode。對應工作卡 HF-PR-202（event subscription 依賴 HF-PR-104）。

### 19.1 Go SDK

目標：

```go
engine, err := hufu.NewEngine(hufu.EngineOptions{
    SessionStore: store,
    EventStore: eventStore,
    Providers: providers,
})

run, err := engine.Run(ctx, hufu.RunRequest{
    Goal: "Refactor auth module",
    Route: hufu.RouteAuto,
})
```

### 19.2 RPC mode

```text
hufu serve --stdio
hufu serve --socket /run/user/1000/hufu.sock
hufu serve --http 127.0.0.1:8080
```

RPC 功能：

- start run
- subscribe events
- submit user input
- approve policy
- abort/pause/resume
- list sessions
- fetch artifacts
- inspect context
- query task DAG

### 19.3 Event protocol

使用穩定 JSON schema：

```json
{
  "type": "task.updated",
  "run_id": "...",
  "task_id": "17",
  "sequence": 128,
  "payload": {}
}
```

sequence 必須 deterministic，client 可斷線重連。

---

# Part VI：Memory 系統

## 20. STM/LTM 從 Markdown truth 降為 projection

> **落地狀態：🔴 OPEN**。`internal/memory` 無 `Confidence`／`Supersedes`／`SourceEventIDs` 等欄位，無 candidate→confirmed→superseded 生命週期。無獨立 PR 卡（視需求新增）；`SourceEventIDs` 依賴 event store（HF-PR-104）的 event ID 才有意義，建議排在 HF-PR-104 之後。

### 20.1 目標記錄

```go
type MemoryRecord struct {
    ID              string
    Content         string
    Category        string
    Project         string
    Team            string
    SourceEventIDs  []string
    SourceTaskID    string
    SourceAgent     string
    FilePaths       []string
    CommitHash      string
    CreatedAt       time.Time
    LastConfirmedAt time.Time
    Confidence      float64
    ExpiresAt       *time.Time
    Supersedes      []string
    Status          string
}
```

### 20.2 Memory lifecycle

```text
candidate
confirmed
superseded
expired
rejected
```

不是每個 STM finding 都應直接進 LTM。

### 20.3 寫入條件

- 有 verification evidence
- 重複出現
- 使用者明確確認
- reviewer 確認
- failure lesson 被後續成功驗證

### 20.4 Retrieval

Hybrid ranking：

```text
vector similarity
+ lexical match
+ recency
+ same-file relevance
+ same-task relevance
+ confidence
+ verification bonus
- superseded penalty
- expired penalty
```

### 20.5 Instruction/data boundary

LTM 注入時明確標示：

```text
Background reference, not authoritative instruction.
```

避免歷史 memory 覆蓋當前 user 或 project rules。

---

# Part VII：Observability、Replay 與 Eval

> **落地狀態：🟡 PARTIAL**。`hufu improve`（`internal/improve/`）已提供跨 run telemetry 分析、A/B experiment、controlled team promotion——**Part XIV 成功指標的基線應從這裡建立，不要另起量測管線**。仍缺：統一 trace model、deterministic replay、fault injection harness、agent eval suite。trace 的 event ID／sequence 依賴 HF-PR-104；eval dashboard 屬 Phase F（未編號 backlog）。

## 21. Trace model

每個事件帶：

```text
run_id
session_id
branch_id
task_id
attempt
agent
model
tool_call_id
parent_span_id
sequence
```

### 21.1 指標

```text
task completion rate
verification pass rate
false-success rate
duplicate delegation rate
retry rescue rate
cache hit usefulness
recovery correctness rate
context retention rate
context duplication rate
tokens per successful task
wall time per successful task
tool error rate
policy denial rate
human intervention rate
```

### 21.2 Context quality metrics

建立 probe：

- 原始 goal retention
- latest correction retention
- completed task retention
- file/artifact retention
- decision consistency
- stale memory injection
- compaction hallucination

---

## 22. Deterministic replay

### 22.1 Replay modes

```text
state replay
provider replay
tool replay
full simulation
```

### 22.2 Golden run

保存：

```text
input prompt
locked resources
provider scripted responses
tool calls/results
events
expected task state
expected context snapshots
expected final output
```

### 22.3 CLI

```text
hufu replay <run-id>
hufu replay <run-id> --until-event <id>
hufu replay <run-id> --replace-model <model>
hufu replay <run-id> --dry-tools
```

---

## 23. Fault injection

需測試：

- session append 中斷
- snapshot rename 中斷
- provider stream 中斷
- tool call side effect 後 crash
- verification完成前 crash
- compaction 中斷
- branch navigation 中斷
- terminal session leak
- event consumer failure
- policy service timeout
- disk full
- corrupted final JSONL line

Fault injection 應內建 test harness，而非只靠手動 kill。

---

## 24. Agent eval

建立 scenario suite：

```text
simple coding
multi-file refactor
research + implementation
CI failure
infra verification
long-running deployment
interrupted recovery
ambiguous user requirement
unsafe tool request
context overflow
stale memory
duplicate delegation
```

每個 scenario 定義：

```text
success criteria
forbidden behavior
expected tools
maximum attempts
verification command
expected artifacts
cost/token budget
```

---

# Part VIII：TUI 與使用者體驗

> **落地狀態：🔴 OPEN**。`internal/tui` 無 Theme 抽象（僅個別 lipgloss style）、無 DAG view、無 context inspector、無 e-paper mode。近期 TUI commit（compact layout、spinner、result viewer、progress bar）未觸及本節主題。對應工作卡 HF-PR-205（依賴 HF-PR-102）、HF-PR-206。

## 25. TUI 功能方向

### 25.1 Task DAG view

顯示：

- dependency
- resource lock
- status
- retry count
- verification
- evidence
- current agent/model

### 25.2 Context Inspector

顯示：

- context item
- token count
- source
- priority
- why included
- dropped/compacted reason

### 25.3 Session tree

- branch
- checkpoint
- compaction
- labels
- active leaf
- branch diff

### 25.4 Terminal lifecycle

- running terminal sessions
- command label
- owner task
- elapsed
- last output
- exit code
- close/abort control

### 25.5 Evidence browser

- artifacts
- hashes
- validators
- failure reason
- report link

---

## 26. Theme 與電子紙模式

建議 theme runtime abstraction：

```go
type Theme interface {
    Background() Color
    Foreground() Color
    Muted() Color
    Success() Color
    Warning() Color
    Error() Color
    Border() Color
}
```

內建：

```text
dark
light
high-contrast
monochrome
e-paper
```

### 26.1 e-paper mode

- 停用 spinner 與高頻動畫
- 不逐 token repaint
- 只在 state change 更新
- 支援 batch refresh interval
- 減少反白區域與大面積閃爍
- 使用圖形符號與文字，不只依賴顏色
- 可動態切換 background/theme
- 保留完整 plain-text fallback

---

# Part IX：建議內建 Agent Teams

## 27. `fast`

### 用途

小型、低風險、單一目標任務。

### Agents

```text
helper
```

### 特性

- 不啟動 coordinator DAG
- typed result
- 基本 verification
- 自動升級到 team path 的能力

---

## 28. `software-delivery`

### 用途

一般 feature、bug fix、跨檔案重構。

### Agents

| Agent | 職責 |
|---|---|
| planner | 讀需求與程式碼，提出可驗證 plan |
| implementer | 實作 |
| test-runner | 執行單元／整合測試 |
| reviewer | review correctness/security/maintainability |
| release-auditor | 核對 diff、tests、artifacts、changelog |

### Workflow

```text
inspect → plan → implement → test → review → fix loop → final audit
```

---

## 29. `repository-maintainer`

### 用途

issue triage、dependency update、CI 維護、repository hygiene。

### Agents

```text
issue-triager
dependency-analyst
ci-debugger
maintainer
reviewer
```

### 特性

- 不自動大量更新依賴
- dependency changes 有逐項 verification
- 支援 GitHub connector／CLI
- PR evidence 與 changelog

---

## 30. `research-design`

### 用途

架構選型、技術研究、ADR、prototype decision。

### Agents

```text
researcher
codebase-explorer
architect
skeptic
adr-writer
```

### Workflow

```text
question decomposition
→ primary-source research
→ current-code constraints
→ options/tradeoffs
→ adversarial review
→ ADR
```

### 完成條件

不是「提出方案」而已，還要：

- alternatives
- decision criteria
- risks
- migration path
- rollback
- unresolved questions

---

## 31. `incident-response`

### 用途

production incident、效能退化、間歇性故障。

### Agents

```text
signal-collector
timeline-builder
hypothesis-investigator
mitigator
verifier
postmortem-writer
```

### 規則

- diagnosis 與 mitigation 分離
- 一次一個變因
- 所有 mutation 有 rollback
- observation evidence first
- postmortem 不將假設寫成事實

---

## 32. `infra-operator`

### 用途

一般基礎設施部署、migration、cluster operation。

### Agents

```text
runbook-analyst
operator
environment-observer
evidence-verifier
rollback-controller
final-auditor
```

### 特性

- resource locks
- side-effect-aware recovery
- blocking acceptance
- command/file policy
- terminal lifecycle
- evidence manifests

---

## 33. `pilot-reverify`（已移除，改為通用機制）

原規劃依附於伴隨文件的 pilot/FreeIPA/trec 專用 scaffold 已隨伴隨文件一般化而移除（該附件從未提供）。此類高約束驗證團隊未來由 `infra-operator`（§32）加上 strict-verification 機制（HF-PR-005/006/107/108/109/110/111/112/113）衍生，不再維護專用 scaffold。

---

## 34. `documentation-verifier`

### 用途

驗證 runbook、README、部署說明是否能真實重現。

### Agents

```text
doc-reader
environment-builder
procedure-runner
evidence-verifier
doc-editor
final-reviewer
```

### 完成條件

- 每個命令有真實輸出
- 文件不使用 predicted output
- 操作步驟與目前 CLI 一致
- 失敗與 workaround 有 provenance
- 文件更新後再跑最小 smoke verification

---

# Part X：Team Schema 與治理

> **落地狀態：🔴 OPEN**。`hufu team` 目前只有 `generate` 與 `permissions` 子命令；無 lint、無 apiVersion／schema。對應工作卡 HF-PR-007（先落地 linter，不帶 schema version）、HF-PR-204（schema version 與進階檢查）。

## 35. Team schema version

```yaml
apiVersion: hufu.io/v1alpha1
kind: AgentTeam
metadata:
  name: software-delivery
spec:
  ...
```

### 35.1 Linter

```text
hufu team lint <path>
```

檢查：

- unknown tools
- unknown skills
- duplicate agents
- coordinator 缺失
- prompt 參照不存在的 tool
- timeout 不合理
- unsafe concurrency
- invalid model
- missing required verification
- cyclic resource/dependency
- unsupported schema field

### 35.2 Team contract tests

每個 bundled team 至少有：

```text
routing test
plan extraction test
tool availability test
blocked-task test
verification test
finish gate test
prompt-tool consistency test
```

### 35.3 Version pinning

Team 可鎖定：

```text
skill version/hash
extension version
tool contract version
minimum hufu version
```

---

# Part XI：實作 Roadmap（Phase 總覽，基準 `b9b47557`）

| Phase | 目標 | 狀態 | 包含工作卡 |
|---|---|---|---|
| A. Correctness Foundation | 長 session 不丟 context、crash 不毀狀態、cache／acceptance／side-effect 語意明確 | 🟡 進行中（1/9 已完成） | ~~HF-PR-001~~ ✅、HF-PR-002、HF-PR-003、HF-PR-004、HF-PR-005、HF-PR-006、HF-PR-007、HF-PR-107（自 P1 升入）、HF-PR-110 |
| B. Context Kernel | token 為主的 budget、可解釋的 context、不丟 artifact 的 compaction | 🟡 進行中（budget＋compaction 主體＋殘餘 validator 已完成） | ~~HF-PR-101~~ ✅、~~HF-PR-103~~ ✅、HF-PR-102、HF-PR-001R |
| C. Durable Session Kernel | event log 可重建一切、branch 不互相污染 | 🔴 未開始 | HF-PR-104、HF-PR-201 |
| D. Workflow Reliability | typed result、resource lock、evidence 綁定、policy engine、terminal/secret/profile | 🔴 未開始 | HF-PR-105、HF-PR-106、HF-PR-108、HF-PR-109、HF-PR-111、HF-PR-112、HF-PR-113 |
| E. Platformization | hufu 可作 library／daemon、extension 不需 fork core | 🔴 未開始 | HF-PR-202、HF-PR-203 |
| F. Product／Ecosystem | fast/team 分流、team catalog、TUI inspector、eval | 🔴 未開始 | HF-PR-204、HF-PR-205、HF-PR-206、HF-PR-207（新增）、built-in team catalog／eval dashboard／package 簽章（未編號 backlog） |

**Phase 完成判準**改由各工作卡的「驗收條件」承擔（Part XII）；Phase A 的整體判準：

- ~~長 session 不失去最近 correction~~ ✅（`5f97001`）
- crash 不會產生半份 session.json（HF-PR-002）
- fresh verification 可完全 bypass cache（HF-PR-003）
- capability probe 有 TTL／invalidation（HF-PR-004）
- required skill 缺失時 fail fast（HF-PR-005）
- acceptance failure 不會被當 success（HF-PR-006）
- 非冪等 task 不盲目重跑（HF-PR-107）

---

# Part XII：PR Backlog（工作卡）

> 每張卡自包含：狀態／工作量／依賴／背景／既有基礎／任務／驗收條件／驗證指令／指派指令。
> 指派方式與工作量尺度見 §0。完成後回本文件更新狀態與基準 commit。

## 42. P0 工作卡（依建議指派順序排列）

### HF-PR-002 Atomic persistence【🟡 IMPLEMENTED (PENDING REVIEW)｜S｜依賴：無】

- **背景**：`SaveSession`（`internal/team/session.go`）用 `os.WriteFile` 直接覆寫 session.json，crash 會留下半份 JSON；全 repo 無 `fsync`。`task_journal.go` 的註解已明確承認此缺口。設計細節見 §10。
- **既有基礎**：`internal/team/task_journal.go` 的 compaction 已是 temp+rename 模式，直接照抄。
- **任務**：
  1. 新增共用 helper（建議 `internal/team/atomic_write.go`）：write temp → `f.Sync()` → `os.Rename` → fsync 目錄（§10.1）。
  2. `SaveSession`、`SaveSessionMD`、`SaveCompactionRecord`、session_history 寫入一律改用 helper。
  3. 新增測試：temp 殘留、主檔缺失、主檔截斷三種 crash 狀態下 `LoadSession` 行為明確（fallback 或 warning，不 panic）。
- **驗收條件**：三種 crash 狀態有測試覆蓋；rename 邏輯只在 helper 出現一次；不重複實作。
- **驗證指令**：
  ```bash
  go build ./cmd/hufu && go vet ./... && go test ./internal/team/ -run 'TestSession|TestCompaction' -count=1
  ```
- **指派指令**：
  ```text
  HF-PR-002：為 session 持久化實作 atomic write。先讀 tmp/hufu-future-improvement-roadmap.md 的 HF-PR-002 工作卡與 §10，參考 internal/team/task_journal.go 的 temp+rename 模式，抽共用 helper 套用到 SaveSession/SaveSessionMD/SaveCompactionRecord，補三種 crash 狀態的 fallback 測試，再跑卡上的驗證指令。
  ```

---

### HF-PR-001R Session recency 殘餘：head/goal retention【🟡 PARTIAL 殘餘｜S｜依賴：無】

- **背景**：`5f97001` 已把 `ContextSummary()` 改為保留最近 40 筆（tail），但 §4.2 的 head/tail 原則未落地——超過 40 筆後最初 user goal 從 resume context 消失（目前僅靠 structured compaction 的 Goal invariant 間接保存，未 compaction 的 session 會丟）。
- **既有基礎**：`ContextSummary()`（`internal/team/session.go`）；compaction Goal invariant（`compaction.go`）。
- **任務**：resume context 明確採 head（第一則 user goal，加標註）+ tail（最近 N 筆）結構；head 取自 `Entries[0]`，若已有 compaction summary 則以其 Goal 取代。
- **驗收條件**：200 筆 entry 的 session resume 後，最初 user goal 與最新 user correction 同時存在於 context；§4.3 的 41/80/200/1000 筆測試補齊。
- **驗證指令**：
  ```bash
  go test ./internal/team/ -run 'TestSessionDataContextSummary' -count=1 -v
  ```
- **指派指令**：
  ```text
  HF-PR-001R：為 session resume 加上 head/goal retention。讀 tmp/hufu-future-improvement-roadmap.md 的 HF-PR-001R 工作卡與 §4，修改 internal/team/session.go 的 ContextSummary 採 head（最初 user goal）+ tail（最近 40 筆）結構，補 41/80/200/1000 筆測試，跑卡上的驗證指令。
  ```

---

### HF-PR-003 CachePolicy【🟡 PARTIAL｜M｜依賴：無】

- **背景**：cache identity 已含 verify command + verifyMode + per-round generation（§12 落地狀態橫幅），但沒有明確的 use/refresh/bypass 語意——使用者要求重跑、live verification、source 變更等場景無法可靠繞過 cache。設計見 §12；**詳細語意表與驗收案例見 strict 文件 §5.6**。
- **既有基礎**：`internal/team/coordinator_taskcache.go`（identity、generation、tombstone）；`TaskDef`（`internal/team/coordinator.go`）。
- **Schema 決策**：TaskDef 的執行語意欄位統一收進 `TaskExecutionPolicy`（`TaskDef.Execution`，strict 文件 §7.1），不直接加在頂層，避免欄位爆炸。HF-PR-105/106/107 同此原則。
- **任務**：
  1. `TaskDef` 新增 `cache_policy`（`use`／`refresh`／`bypass`，預設 `use`）：`refresh` = 重跑並覆寫 cache；`bypass` = 不讀不寫。
  2. journal 記錄 policy；lookup 與 store 路徑都遵守。
  3. 明確文件化 §12.3「何時禁止 cache」場景與 policy 的對應。
- **驗收條件**：`bypass` 的 task 永不命中 cache 也不寫入；`refresh` 重跑後覆寫舊 entry；既有 dedup／lookup 測試全綠。
- **驗證指令**：
  ```bash
  go test ./internal/team/ -run 'TestCache|TestDedup|TestLookupTaskCache' -count=1
  ```
- **指派指令**：
  ```text
  HF-PR-003：為 task cache 加上明確 CachePolicy（use/refresh/bypass）。讀 tmp/hufu-future-improvement-roadmap.md 的 HF-PR-003 工作卡與 §12，在 TaskDef 加 cache_policy 欄位並讓 coordinator_taskcache.go 的 lookup/store 遵守，journal 記錄 policy，補測試並跑卡上的驗證指令。
  ```

---

### HF-PR-004 Capability freshness【🟡 PARTIAL｜M｜依賴：無】

- **背景**：`internal/team/capability.go` 已有 preflight probe（`CapabilityResult`：scope/evidence/CheckedAt），但結果無 TTL、無 mutation invalidation——環境變更後可能用過期的 capability 判斷。設計見 §12.4；**驗收案例（環境重建後重新 probe）見 strict 文件 §5.7**。
- **既有基礎**：`capability.go` 的 probe 機制；team.yml `preflight` 設定。
- **任務**：
  1. capability 結果快取加 TTL（team.yml 可配）與 scope。
  2. mutation 類工具（bash/ssh/sudo/write/edit）成功執行後 invalidate 相關 scope 的 capability 結果。
  3. 支援 `always-fresh` 標記：每次都重新 probe。
- **驗收條件**：TTL 內不重複 probe；mutation 後相關 capability 重新 probe；`always-fresh` 每次 probe；probe timeout 行為不變。
- **驗證指令**：
  ```bash
  go test ./internal/team/ -run 'TestCapab' -count=1 -v
  ```
- **指派指令**：
  ```text
  HF-PR-004：為 capability preflight 加 TTL 與 mutation invalidation。讀 tmp/hufu-future-improvement-roadmap.md 的 HF-PR-004 工作卡與 §12.4，修改 internal/team/capability.go 加快取 TTL、scope invalidation 與 always-fresh 支援，補測試並跑卡上的驗證指令。
  ```

---

### HF-PR-005 Required skills【🔴 OPEN｜M｜依賴：無】

- **背景**：skill 目前有 discovery、`--skill`、auto-skills，但沒有「required + hash lock + 缺失 fail fast」——team 依賴的 skill 缺失時要到執行中才發現。**`RequiredSkillSpec` schema（SHA-256 lock、`inject-into`、snapshot、`policy-lock.json`）與驗收案例見 strict 文件 §5.1，以該 schema 為準**。
- **既有基礎**：`internal/skill/`（discovery、parsing）；`hufu doctor`（preflight 檢查框架）。
- **任務**：
  1. team.yml 支援 `required-skills`（每項可附 hash）。
  2. team 載入時解析：skill 缺失或 hash 不符 → 在任何 LLM call 前 fatal，明確指出缺哪個。
  3. `hufu doctor` 納入 required-skill 檢查。
- **驗收條件**：缺失 required skill 時 fail fast 且訊息指出名稱；hash 不符明確報錯；非 required skill 行為不變。
- **驗證指令**：
  ```bash
  go test ./internal/skill/ ./internal/team/ -run 'TestSkill' -count=1 && go run ./cmd/hufu doctor
  ```
- **指派指令**：
  ```text
  HF-PR-005：實作 required skill 鎖定。讀 tmp/hufu-future-improvement-roadmap.md 的 HF-PR-005 工作卡，team.yml 加 required-skills（可附 hash），載入時缺失或不符即 fatal，並納入 hufu doctor，補測試並跑卡上的驗證指令。
  ```

---

### HF-PR-006 Blocking acceptance【🟡 PARTIAL｜M｜依賴：無】

- **背景**：unattended self-healing acceptance + rollback 已存在（`58792a0`），但 acceptance 只有一種語意；缺 §16.3 的 advisory/blocking 分級與 strict exit semantics。**`completed_with_exceptions` 終態與 strict profile 規則見 strict 文件 §5.9**。
- **既有基礎**：finish gate 的 acceptance 執行路徑（`internal/team/` finish tool）；rollback 機制。
- **任務**：
  1. team.yml `acceptance` 支援 `mode: advisory|blocking`（預設 `advisory` 維持向後相容）。
  2. blocking 失敗：run 不得 success、exit code 非零、final response 不得假裝完成；互動模式同樣遵守（不只是 unattended）。
  3. 保留可重試與失敗輸出。
- **驗收條件**：blocking 失敗 → exit≠0 且結果明確標示未通過；advisory 失敗 → 附註但不改 exit code；既有 unattended 測試全綠。
- **驗證指令**：
  ```bash
  go test ./internal/team/ -run 'TestUnattended|TestAcceptance|TestFinish' -count=1
  ```
- **指派指令**：
  ```text
  HF-PR-006：為 acceptance 加 advisory/blocking 分級。讀 tmp/hufu-future-improvement-roadmap.md 的 HF-PR-006 工作卡與 §16.3，team.yml acceptance 支援 mode，blocking 失敗時 exit code 非零且結果不得假裝完成（互動模式同），補測試並跑卡上的驗證指令。
  ```

---

### HF-PR-007 Prompt/tool linter【🔴 OPEN｜M｜依賴：無】

- **背景**：`hufu team` 目前只有 `generate`／`permissions` 子命令；§35.1 的檢查可先不帶 schema version 落地（schema version 屬 HF-PR-204）。**已確認 3 個內建 team（`operator`、`delegate`、`dev-team`）prompt 引用不存在的 `run_agents`——這是 linter 的第一個真實案例，見 strict 文件 §5.16**。
- **既有基礎**：`cmd/hufu/teamcmd.go`（子命令框架）；`cmd/hufu/list.go` 的 `readAgentFrontmatter`（無 side-effect 讀 frontmatter）。
- **任務**：
  1. 新增 `hufu team lint <path>`：unknown tools、unknown skills、duplicate agents、coordinator 缺失、prompt 引用不存在的 tool、timeout 不合理、unsupported frontmatter field。
  2. lint 不得有 workspace side effect（比照 `hufu list` 直接讀 frontmatter）。
  3. CI gate 掃 `.agent-teams/`。
- **驗收條件**：repo 內建 teams lint 全綠；故意注入的每類錯誤都能逐項抓出；exit code 反映結果；lint 後不產生任何 workspace 目錄。
- **驗證指令**：
  ```bash
  go test ./cmd/hufu/ -run 'TestTeamLint' -count=1 && go run ./cmd/hufu team lint .agent-teams/
  ```
- **指派指令**：
  ```text
  HF-PR-007：新增 hufu team lint 子命令。讀 tmp/hufu-future-improvement-roadmap.md 的 HF-PR-007 工作卡與 §35.1，在 cmd/hufu/teamcmd.go 掛新子命令，比照 list.go 直接讀 frontmatter（無 workspace side effect），實作 unknown tools/skills、duplicate agents、缺 coordinator、prompt 引用不存在 tool 等檢查，補測試並跑卡上的驗證指令。
  ```

---

### HF-PR-107 Side-effect recovery【🔴 OPEN｜L｜依賴：無｜自 P1 升入 P0，風險最高】

- **背景**：`ResumeInterruptedTasks` 對所有非終態 task 一律原 ID 重跑，不區分副作用（§11）。agent 擁有 bash/ssh/sudo 工具且跑 unattended 時，crash-resume 可能盲目重跑 VM create/delete、deploy、credential change 等非冪等操作。**reconcile probe 配置範例與恢復流程驗收見 strict 文件 §5.8**。
- **詞彙決策**：`SideEffectClass` 以本文件 §11.2 為準（superset）；strict 文件 §5.8 的 `local_mutation` 對應本文件的 `workspace_write`。
- **既有基礎**：crash-resume checkpoint 機制（`TodoList.onChange → saveCheckpoint`、`ResumeInterruptedTasks`）；task journal。
- **任務**：
  1. `TaskDef` 新增 `side_effect` 分級（`none`／`workspace_write`／`external_write`／`infra_mutation`／`credential_mutation`）與 `recovery` policy（`retry`／`reconcile`／`manual`／`never`）；預設值依分級推導（§11.2–11.3），未標分級的 task 維持現行行為（向後相容）。
  2. resume 時按 policy 分流：`retry` 照現行重跑；`manual`／`never` 標 blocked 等人工；`reconcile` 先跑 read-only 探查再決定 complete／not_started／partial／unknown（§11.4–11.5）。
  3. checkpoint 記錄分級與 policy 供重啟後使用。
- **驗收條件**：fault-injection 測試——task 標 `in_progress` + `side_effect=infra_mutation` 重啟後**不**直接重跑；`external_write` 在 unattended 下預設不盲目重跑；§11.5 五步流程有測試覆蓋。
- **驗證指令**：
  ```bash
  go test ./internal/team/ -run 'TestResume|TestRecovery|TestUnattended' -count=1
  ```
- **指派指令**：
  ```text
  HF-PR-107：實作 side-effect-aware crash recovery。讀 tmp/hufu-future-improvement-roadmap.md 的 HF-PR-107 工作卡與 §11，TaskDef 加 side_effect 分級與 recovery policy，修改 ResumeInterruptedTasks 按 policy 分流（retry/reconcile/manual/never），補 fault-injection 測試並跑卡上的驗證指令。
  ```

---

### HF-PR-101 Token counter【✅ DONE（工作區未合入）｜L｜依賴：無｜解鎖 Phase B 其餘工作】

- **背景**：`internal/team/context_budget.go` 以固定 `120_000` chars 估算（自承「Roughly 30K tokens」）——中英文混合、程式碼/JSON 密度差異使估算嚴重失真（§5.1）。
- **既有基礎**：`context_budget.go` 的 squeeze/cap 機制（budget 改以 token 計後仍需要）；compaction 的 tokens_before/after 記錄。
- **任務**：
  1. `TokenCounter` interface + `ModelContextSpec` registry（§5.2）：內建常見 model 的 context window；provider 可回報 spec。
  2. 無 tokenizer 的模型：依 model family 的 estimator fallback + conservative margin，log 標記 `estimated`（§5.3）。
  3. `PrepareStep`／`capStepMessages` 改用 token budget；context overflow 時自動 compact 並 retry 一次。
  4. report 輸出 §5.4 的 budget breakdown。
- **驗收條件**：budget 以 token 為主；無 tokenizer 時保守且標記 estimated；overflow 能 deterministic compact/retry；report 有 breakdown。
- **Review 結果（2026-07-21）**：核心交付物已達成——`TokenCounter`／`ModelContextSpec` registry／`DefaultTokenCounter`（CJK+code+ascii 密度估算＋保守 margin）／`CalculateContextBudget`／`CapStepMessagesWithCounter`（token 預算、雙 pass squeeze）／`PrepareStep` 改用 model spec budget／overflow 自動 compact+retry 一次（`IsContextOverflowError`）。測試全綠（`TestModelSpecRegistry`／`TestEstimatorTokenCounts`／`TestContextBudgetAndReport`／`TestCapStepMessagesWithCounter`／`TestIsContextOverflowError`／`TestCapStepMessages`），`go vet`／`go build` 通過，gofmt 已清理。
  - **殘餘修復（2026-07-21 第二輪）**：原三項殘餘已全數修復並補測試——
    - (a) `BreakdownReport`／`ContextUsageBreakdown` 已接線至 `--report`：`buildSystemPrompt` 捕獲 core/project/memory 三段文字，`Coordinator.recordContextBreakdown`（`coordinator_context_report.go`）結合 tool schemas、conversation、compacted summary、已完成 task output 與 reply reserve 算出 `ContextUsageBreakdown`，`RenderContextUsageSection` 在 `cmd/hufu/report.go` 渲染 §5.4 breakdown（estimated 模型加註 `_estimated_`）。
    - (b) `estimated` 已 log：`warnEstimatedOnce`（`token_counter.go`）對每個 fallback model 於程式生命週期警告一次（`log.Printf` 至 stderr），`estimatedModelLogger` 可替換以便測試（`TestWarnEstimatedOnceDedup`）。
    - (c) compaction token 計數已 model-aware：`countTokensInText(modelID, …)`／`countTokensInMessages(modelID, …)` 接受 model 參數，`compactMessages` 以 `coordinatorModelID()` 傳入，使 `CompactionRecord` 的 `TokensBefore`／`TokensAfter` 採與 context budget 相同之 estimator。
    - 新增測試：`TestRecordContextBreakdownAndReport`、`TestRenderContextUsageSectionEmptyWhenNotReady`、`TestEstimatedContextModelFallback`、`TestWarnEstimatedOnceDedup`、`TestCountTokensInTextModelAware`、`TestCountTokensInMessagesModelAware`；`go test ./... -count=1`、`go vet`、`gofmt` 全綠。
- **驗證指令**：
  ```bash
  go test ./internal/team/ -run 'TestContextBudget|TestToken|TestCapStep' -count=1
  ```
- **指派指令**：
  ```text
  HF-PR-101：實作 model-aware token budget。讀 tmp/hufu-future-improvement-roadmap.md 的 HF-PR-101 工作卡與 §5，建立 TokenCounter interface 與 ModelContextSpec registry（含 estimator fallback），把 context_budget.go 的固定字元預算改為 token 預算，overflow 自動 compact+retry 一次，report 加 breakdown，補測試並跑卡上的驗證指令。
  ```

---

## 43. P1 工作卡

### HF-PR-102 Context compiler【🔴 OPEN｜L｜依賴：HF-PR-101】

- **背景**：prompt 拼接邏輯分散於 coordinator、worker injection、STM/LTM、skills、AGENTS.md（§7.1）。
- **任務**：抽 `ContextCompiler` 子系統（§7.2 pipeline、§7.3 `ContextItem`、§7.4 worker 優先順序與 §7.5 provenance）；先以 legacy adapter 包住現有拼接，逐呼叫點遷移（§17.3 原則）。
- **驗收條件**：超過 budget 時 1–7 優先級不可移除；可解釋每個 context item 為何存在；既有 worker prompt 行為有 regression test。
- **驗證指令**：`go test ./internal/team/ -run 'TestContext|TestBuildOrchestrator|TestWorker' -count=1`
- **指派指令**：
  ```text
  HF-PR-102：抽 ContextCompiler 子系統。讀 tmp/hufu-future-improvement-roadmap.md 的 HF-PR-102 工作卡與 §7，以 legacy adapter 漸進抽取 prompt 拼接邏輯，實作 ContextItem/priority/budget/provenance，補 regression test 並跑卡上的驗證指令。
  ```

---

### HF-PR-103 Structured compaction 殘餘【✅ DONE｜M｜依賴：無】

- **背景**：主體已落地（`b9b4755`，見 §6 橫幅）；剩 §6.4 validator 與兩個欄位。
- **任務**：
  1. §6.4 deterministic post-compaction validator：active task ID 仍存在、modified artifact 可追溯、最新 user correction 存在、failed task 不被標 done；失敗時保留舊 summary 並 log。
  2. `StructuredSummary` 加 `UserCorrections`、`SourceEntryIDs` 欄位。
- **驗收條件**：validator 對 §6.4 五項檢查各有測試；失敗時 fallback 不覆寫舊 summary。
- **驗證指令**：`go test ./internal/team/ -run 'TestCompaction' -count=1 -v`
- **指派指令**：
  ```text
  HF-PR-103：補齊 structured compaction 殘餘。讀 tmp/hufu-future-improvement-roadmap.md 的 HF-PR-103 工作卡與 §6.4，在 internal/team/compaction.go 加 deterministic validator 與 UserCorrections/SourceEntryIDs 欄位，補測試並跑卡上的驗證指令。
  ```

---

### HF-PR-104 Event store【🟡 IMPLEMENTED (PENDING REVIEW)｜XL｜依賴：HF-PR-002】

- **背景**：狀態分散於 6+ 份檔案，無統一 ordering/event ID/replay semantics（§9.1）。
- **Schema 決策**：`RunEvent` 以本文件 §9.3 為準（含 `SessionID`／`BranchID`／`IdempotencyKey`）；event 類型取 §9.4 與 strict 文件 §7.3 的**聯集**（strict 版多出 `source_locked`／`skill_locked`／`task_authorized`／`terminal_started`／`terminal_closed`／`recovery_decided`／`acceptance_finished`）。
- **任務**：按 §9.3 schema 實作 append-only JSONL event store + §9.4 事件類型 + hash chain；**嚴格遵守 §9.5 dual-write 遷移順序**（先雙寫、再 reducers 比對、最後才升為 source of truth）。
- **驗收條件**：可從 event log 重建 session/task/STM（reducer 比對測試）；hash chain 可驗證；legacy 檔案行為不變直到切換。
- **驗證指令**：`go test ./internal/team/ -run 'TestEvent|TestReplay|TestReducer' -count=1`
- **指派指令**：
  ```text
  HF-PR-104：實作 append-only event store（dual-write 階段）。讀 tmp/hufu-future-improvement-roadmap.md 的 HF-PR-104 工作卡與 §9，按 §9.3 schema 建立 JSONL event store + hash chain，現有寫入路徑雙寫，實作 session/task reducers 並加比對測試，跑卡上的驗證指令。不要在本 PR 移除 legacy state。
  ```

---

### HF-PR-105 Typed TaskResult【🔴 OPEN｜L｜依賴：無｜HF-PR-108 的前置】

- **背景**：worker output 是 free text（`TodoItem.Output string`），難驗證/compaction/retry/judge/cache（§13.1）。
- **Schema 決策**：TaskDef 擴充欄位統一收進 `TaskExecutionPolicy`（`TaskDef.Execution`，strict 文件 §7.1），同 HF-PR-003 的原則。
- **任務**：
  1. §13.2 `TaskResult` schema。
  2. 新增 `submit_result` tool 讓 agent 提交 typed result；無提交時以 compatibility parser 從 final text 產生低信心結果（`Confidence=0.4, Source=parsed_free_text`，§13.3）。
  3. dependency injection 改用 typed result。
- **驗收條件**：strict task 可要求 typed result mandatory；free-text fallback 有 confidence 標記；既有 task 流程 regression test 全綠。
- **驗證指令**：`go test ./internal/team/ -run 'TestTaskResult|TestSubmitResult|TestExecute' -count=1`
- **指派指令**：
  ```text
  HF-PR-105：實作 typed TaskResult 與 submit_result tool。讀 tmp/hufu-future-improvement-roadmap.md 的 HF-PR-105 工作卡與 §13，建立 TaskResult schema、submit_result tool 與 free-text compatibility parser（低信心標記），dependency injection 改用 typed result，補測試並跑卡上的驗證指令。
  ```

---

### HF-PR-106 Resource locks【🔴 OPEN｜M｜依賴：無（與 HF-PR-105 有綜效）】

- **背景**：DAG scheduler 僅依 dependency 排程，共享資源（VM、workspace、terminal）可能被不安全並行（§14.4）。
- **任務**：`TaskDef` 加 `ResourceClaim`（resource + read/write/exclusive）；scheduler 排程時檢查 claim 衝突。
- **驗收條件**：exclusive 衝突的 task 不並行；read/read 可並行；既有 DAG 測試全綠。
- **驗證指令**：`go test ./internal/team/ -run 'TestDAG|TestResource|TestSchedule' -count=1`
- **指派指令**：
  ```text
  HF-PR-106：為 DAG scheduler 加 resource lock。讀 tmp/hufu-future-improvement-roadmap.md 的 HF-PR-106 工作卡與 §14.4，TaskDef 加 ResourceClaim，scheduler 依 claim 避免不安全並行，補測試並跑卡上的驗證指令。
  ```

---

### HF-PR-108 Artifact/evidence【🔴 OPEN｜L｜依賴：HF-PR-105】

- **背景**：task 完成與 evidence 未綁定；report 文字是唯一證據儲存（§16）。**`EvidenceManifest` schema（含 hash chain）、錄影/transcript/secret-scan validator 檢查項、report 引用格式見 strict 文件 §5.5 與 §5.14，以該 schema 為準**。
- **任務**：artifact store（路徑+hash+metadata）、§16.2 evidence validators、final manifest、report 引用 artifact/event ID。
- **驗收條件**：strict task 的 evidence completeness = 100%；manifest 可追溯每個 artifact 的 hash 與 validator 結果。
- **驗證指令**：`go test ./internal/team/ -run 'TestArtifact|TestEvidence|TestManifest' -count=1`
- **指派指令**：
  ```text
  HF-PR-108：實作 artifact/evidence store。讀 tmp/hufu-future-improvement-roadmap.md 的 HF-PR-108 工作卡與 §16，建立 artifact store（hash+metadata）、evidence validators 與 final manifest，report 加引用，補測試並跑卡上的驗證指令。
  ```

---

### HF-PR-109 Policy engine【🔴 OPEN｜L｜依賴：無｜HF-PR-203 的前置】

- **背景**：工具權限分散於 allowlist、guard、path-consent；無統一 fail-closed policy 中介層。**詳細設計見 strict 文件：§5.2（hook failure-mode，已確認 `internal/hooks/shell.go` 三條 fail-open 路徑）、§5.3（command/file provenance 三層實作）、§5.4（MCP middleware 與 argv/AST 解析，防 `sh -c` 繞過）**。
- **任務**：tool/MCP middleware 形式的 policy engine；fail-closed；secret redaction；policy decision 可稽核。
- **驗收條件**：policy 錯誤/timeout 時 deny（fail-closed）；決策有 audit 記錄；既有 guard/allowlist 行為有 regression test。
- **驗證指令**：`go test ./internal/tools/ ./internal/team/ -run 'TestPolicy|TestGuard|TestPermission' -count=1`
- **指派指令**：
  ```text
  HF-PR-109：實作 policy engine middleware。讀 tmp/hufu-future-improvement-roadmap.md 的 HF-PR-109 工作卡，建立 fail-closed 的 tool/MCP policy 中介層（含 secret redaction 與決策稽核），整合現有 guard/allowlist，補 regression test 並跑卡上的驗證指令。
  ```

---

### HF-PR-110 Workspace separation【🔴 OPEN｜M｜依賴：無｜源自 strict 文件 §5.10】

- **背景**：strict 文件 HF-WORKSPACE-001。ground-up cleanup 類任務可能刪除 hufu 自己的 checkpoint／evidence；control workspace（session/journal/manifests）與 subject workspace（被操作對象）無強制隔離。
- **既有基礎**：`allowed-paths`／`restricted-path` 機制（`internal/tools/`）。
- **任務**：
  1. `ValidateWorkspaceSeparation(control, subject)`：canonical absolute path、symlink resolution、ancestor/descendant 檢查、禁止 root/home 刪除；subject 必須匹配 task policy allowlist。
  2. run 啟動時檢查，不符即 fail fast。
  3. cleanup 操作保存刪除前後 path state 作為 evidence。
- **驗收條件**：subject 包含 control 時啟動失敗；指向 control 的 symlink 也被識別；cleanup evidence 完整。
- **驗證指令**：`go test ./internal/team/ ./internal/tools/ -run 'TestWorkspace' -count=1`
- **指派指令**：
  ```text
  HF-PR-110：實作 control/subject workspace 分離驗證。讀 tmp/hufu-future-improvement-roadmap.md 的 HF-PR-110 工作卡與 strict 文件 §5.10，實作 ValidateWorkspaceSeparation（含 symlink/ancestor 檢查）並在 run 啟動時強制執行，補測試並跑卡上的驗證指令。
  ```

---

### HF-PR-111 Secret registry/redaction【🔴 OPEN｜L｜依賴：無｜與 HF-PR-109 有綜效｜源自 strict 文件 §5.11】

- **背景**：strict 文件 HF-SECRET-001。tool args、MCP args、command argv、env dump、錄影輸入、audit JSONL、task output、report、transcript、model prompt/history 都可能洩漏 secret，目前無統一防線。
- **任務**：
  1. `SecretRef` registry（Name/Source/ExactValue，value 僅存 process memory）+ `Redactor` interface（`RedactText`/`RedactJSON`）。
  2. model 只取得 secret 名稱；tool adapter 在執行邊界解析；audit 序列化前 redact；command label 取代完整 command。
  3. redactor failure 為 fail-closed；report 顯示 redaction coverage 而非 secret。
- **驗收條件**：測試 secret 經 MCP、tool result、stderr、report 各路径後，所有持久化檔案 exact-value scan = 0；redactor failure 阻擋而非放行。
- **驗證指令**：`go test ./internal/tools/ ./internal/audit/ -run 'TestSecret|TestRedact' -count=1`
- **指派指令**：
  ```text
  HF-PR-111：實作 secret registry 與統一 redaction。讀 tmp/hufu-future-improvement-roadmap.md 的 HF-PR-111 工作卡與 strict 文件 §5.11，建立 SecretRef/Redactor，audit/report 序列化前強制 redact 且 fail-closed，補掃描測試並跑卡上的驗證指令。
  ```

---

## 44. P2 工作卡

### HF-PR-201 Session tree【🔴 OPEN｜XL｜依賴：HF-PR-104｜建議補價值論證後再指派】

- **背景**：見 §8（branch/fork/label/time travel）。對 hufu 主要場景（unattended／CI）價值未論證，屬大型投資。
- **任務**：§8.2 `SessionEntry` 加 ParentID/BranchID；§8.3 branch-scoped state；§8.4 CLI/TUI。
- **驗收條件**：branch 不污染其他 branch；可 diff 兩 branch 的 task/artifact/verification。
- **驗證指令**：`go test ./internal/team/ -run 'TestBranch|TestSessionTree' -count=1`
- **指派指令**：
  ```text
  HF-PR-201：實作 session branch/fork。讀 tmp/hufu-future-improvement-roadmap.md 的 HF-PR-201 工作卡與 §8，基於 event store 建立 branch-scoped session tree 與 CLI，補測試並跑卡上的驗證指令。
  ```

---

### HF-PR-202 SDK/RPC【🔴 OPEN｜XL｜依賴：HF-PR-104（event subscription）】

- **背景**：見 §19。
- **任務**：Go SDK（§19.1）、`hufu serve --stdio/--socket/--http`（§19.2）、穩定 JSON event protocol（§19.3，sequence deterministic、client 可斷線重連）。
- **驗收條件**：外部程式可 start run／subscribe events／fetch artifacts；斷線重連不丟 event。
- **驗證指令**：`go test ./... -run 'TestRPC|TestSDK|TestServe' -count=1`
- **指派指令**：
  ```text
  HF-PR-202：實作 Go SDK 與 RPC serve mode。讀 tmp/hufu-future-improvement-roadmap.md 的 HF-PR-202 工作卡與 §19，建立 engine API、stdio/socket serve 與可重連的 event subscription，補測試並跑卡上的驗證指令。
  ```

---

### HF-PR-203 Extension API【🔴 OPEN｜XL｜依賴：HF-PR-109】

- **背景**：見 §18（extension points、不用 Go plugin 的理由、§18.3 技術組合、§18.4 manifest）。
- **任務**：versioned extension manifest、version negotiation、permissions、stdio JSON-RPC transport、WASM policy/context transform。
- **驗收條件**：extension 不需 fork core；permission 不因 transport 被繞過。
- **驗證指令**：`go test ./... -run 'TestExtension' -count=1`
- **指派指令**：
  ```text
  HF-PR-203：實作 versioned extension API。讀 tmp/hufu-future-improvement-roadmap.md 的 HF-PR-203 工作卡與 §18，建立 extension manifest/permissions/stdio JSON-RPC transport，補測試並跑卡上的驗證指令。
  ```

---

### HF-PR-204 Team schema/lint v2【🔴 OPEN｜L｜依賴：HF-PR-007】

- **背景**：見 §35（apiVersion、version pinning、contract tests）。
- **任務**：team.yml 加 `apiVersion`；JSON schema；§35.2 contract tests；§35.3 version pinning；存量 team.yml 的相容／遷移策略（不得直接 break）。
- **驗收條件**：無 apiVersion 的舊 team 仍能載入（預設版本）；lint 能抓 unsupported field；contract tests 覆蓋 bundled teams。
- **驗證指令**：`go test ./cmd/hufu/ ./internal/team/ -run 'TestTeamSchema|TestTeamLint|TestLoadTeam' -count=1`
- **指派指令**：
  ```text
  HF-PR-204：實作 team schema version 與 contract tests。讀 tmp/hufu-future-improvement-roadmap.md 的 HF-PR-204 工作卡與 §35，加 apiVersion/JSON schema/version pinning 並提供舊 team 相容策略，補 contract tests 並跑卡上的驗證指令。
  ```

---

### HF-PR-205 Context/evidence TUI【🔴 OPEN｜L｜依賴：HF-PR-102】

- **背景**：見 §25（DAG view、context inspector、session tree、terminal lifecycle、evidence browser）。**注意 TUI 紅線**：`View()` 的 9 層 overlay 優先序不可亂、`Update()` 保持純函數（AGENTS.md TUI 章節）。
- **任務**：§25.1 DAG view、§25.2 context inspector、§25.5 evidence browser；遵守 TUI testing guidelines（state machine test + view rendering test）。
- **驗收條件**：新 overlay 插入正確優先序位置；`tea.KeyMsg` 測試覆蓋新 key binding；`Update()` 無 I/O。
- **驗證指令**：`go test ./internal/tui/ -count=1`
- **指派指令**：
  ```text
  HF-PR-205：為 TUI 加 DAG view 與 context inspector。讀 tmp/hufu-future-improvement-roadmap.md 的 HF-PR-205 工作卡與 §25，並遵守 AGENTS.md 的 TUI 安全規則（View 優先序、Update 純函數、key binding 測試），實作後跑卡上的驗證指令。
  ```

---

### HF-PR-206 Theme/e-paper【🔴 OPEN｜M｜依賴：無】

- **背景**：見 §26。`internal/tui` 目前只有硬編碼 lipgloss style。
- **任務**：`Theme` interface（§26）+ dark/light/high-contrast/monochrome/e-paper 內建主題；§26.1 e-paper mode（停高頻動畫、state-change 才更新、batch refresh、plain-text fallback）。
- **驗收條件**：可 runtime 切換 theme；e-paper mode 下無逐 token repaint；plain-text fallback 完整。
- **驗證指令**：`go test ./internal/tui/ -count=1`
- **指派指令**：
  ```text
  HF-PR-206：為 TUI 加 runtime theme 與 e-paper mode。讀 tmp/hufu-future-improvement-roadmap.md 的 HF-PR-206 工作卡與 §26，建立 Theme interface 與五種內建主題，e-paper mode 停高頻動畫並保留 plain-text fallback，補測試並跑卡上的驗證指令。
  ```

---

### HF-PR-207 Fast/team router（新增）【🔴 OPEN｜L｜依賴：無】

- **背景**：見 §15。`--default` 已是事實上的 fast path 雛形、`--auto-team` 已有 sidecar match + keyword fallback（`cmd/hufu/autoteam.go`），實作時應認領整合。
- **任務**：`ExecutionRouter`（§15.3）：deterministic signals 優先、LLM classifier 為輔；fast path 直送單一 agent（不啟動 coordinator DAG）；可自動升級 team path。
- **驗收條件**：簡單任務不經多代理 overhead（可用 step/token 數證明）；路由決策有 reasons 可解釋。
- **驗證指令**：`go test ./cmd/hufu/ -run 'TestRoute|TestAutoTeam' -count=1`
- **指派指令**：
  ```text
  HF-PR-207：實作 fast/team ExecutionRouter。讀 tmp/hufu-future-improvement-roadmap.md 的 HF-PR-207 工作卡與 §15，整合 --default 與 autoteam.go 的既有機制，以 deterministic signals 為主做路由，補測試並跑卡上的驗證指令。
  ```

---

### HF-PR-112 Terminal session manager【🔴 OPEN｜L｜依賴：HF-PR-104（lifecycle event 入 log）｜源自 strict 文件 §5.12】

- **背景**：strict 文件 HF-TUI-001。stateful terminal session（互動式 wizard、long-running deploy）不是第一級 task resource——session 所有權、關閉時機、與 agent timeout 的關係都沒有建模，resume 時容易與執行中的 child process 混淆。
- **任務**：
  1. `TerminalSession` + `TerminalSessionManager`（Start/Write/Read/Close/List，schema 見 strict 文件 §5.12）。
  2. session 只能由 owner task 使用；task 結束前必須 closed 或 child natural exit；run finish 前 session list 必須為空（leaked-session gate）。
  3. long-running child timeout 與 agent model timeout 分離；lifecycle event 寫 event log。
- **驗收條件**：coordinator timeout 不把執行中的 long-running child 誤判為可 retry；leaked session 阻擋 final acceptance；session ID 不跨 task 重用。
- **驗證指令**：`go test ./internal/team/ -run 'TestTerminal' -count=1`
- **指派指令**：
  ```text
  HF-PR-112：實作 terminal session manager。讀 tmp/hufu-future-improvement-roadmap.md 的 HF-PR-112 工作卡與 strict 文件 §5.12，把 stateful terminal 建模為第一級 task resource（owner 綁定、leaked gate、timeout 分離、lifecycle event），補測試並跑卡上的驗證指令。
  ```

---

### HF-PR-113 Execution profiles【🔴 OPEN｜M｜依賴：HF-PR-003、HF-PR-006｜源自 strict 文件 §5.15、§7.2】

- **背景**：strict 文件 HF-MEMORY-001。cache／acceptance／memory／hook failure-mode 等開關散落在各 CLI flag 與 team.yml，無具名 profile；「fresh verification」（不信任任何舊 cache/memory/journal）目前無法一鍵達成。
- **任務**：
  1. `ExecutionProfile`（schema 見 strict 文件 §7.2）；內建 `default`／`unattended`／`strict-verification`／`fresh-verification`。
  2. profile 統一控制 StrictPolicy、HookFailureMode、AcceptanceMode、DisableHistoricalMemory、DisableTaskCache、DisableJournalRestore、RequireEvidenceManifest、RequireClosedTerminals、RequireWorkspaceIsolation。
  3. team.yml `execution-profile` 指定；與既有 CLI flag 衝突時的優先規則明確文件化。
- **驗收條件**：`fresh-verification` 下 session-resume／LTM／RAG／journal-restore 全停且 control evidence 保留；profile 與 CLI flag 衝突行為有測試。
- **驗證指令**：`go test ./internal/team/ -run 'TestProfile' -count=1`
- **指派指令**：
  ```text
  HF-PR-113：實作具名 execution profiles。讀 tmp/hufu-future-improvement-roadmap.md 的 HF-PR-113 工作卡與 strict 文件 §7.2，建立 ExecutionProfile 與四個內建 profile（含 fresh-verification 隔離舊 memory/cache/journal），定義與 CLI flag 的優先規則，補測試並跑卡上的驗證指令。
  ```

---

## 44.1 已完成工作卡（存檔，勿重做）

| 工作卡 | 完成 commit | 內容 |
|---|---|---|
| HF-PR-001 Session recency | `5f97001` | `ContextSummary()` 改為保留最近 40 筆；殘餘 head retention 見 HF-PR-001R |
| HF-PR-103 主體 Structured compaction | `b9b4755` | 13-section summary、7 invariants、CompactionRecord 持久化、restart-safe offsets；殘餘 validator 見下方 |
| HF-PR-103 殘餘 Validator | 工作區未合入 | `ValidateStructuredSummary` 5 項 deterministic 檢查（goal/activeTask/artifact/userCorrection/failedTask）、`UserCorrections`/`SourceEntryIDs` 欄位、fallback 保留舊 summary、coordinator_session 接線 task IDs |
| HF-PR-101 Token counter | 工作區未合入 | `TokenCounter`／`ModelContextSpec` registry／`DefaultTokenCounter`（密度估算+保守 margin）／`CalculateContextBudget`／`CapStepMessagesWithCounter`（token 預算）／overflow compact+retry／report breakdown 接線 `--report`（`coordinator_context_report.go`）／`estimated` log（`warnEstimatedOnce`）／compaction token 計數 model-aware |
| HF-PR-002 Atomic persistence | 工作區未合入 | `AtomicWriteFile`（temp write + `Sync()` + `os.Rename` + directory `SyncDir`），`SaveSession`、`SaveSessionMD`、`SaveCompactionRecord` 已全數改用此 helper，並於 `atomic_write_test.go` 涵蓋 crash 恢復測試 |
| HF-PR-104 Event store | 工作區未合入 | `RunEvent`（SchemaVersion, ID, PreviousID, RunID, SessionID, BranchID, TaskID, Attempt, Actor, Type, Timestamp, IdempotencyKey, Payload, PreviousHash, Hash）、`EventStore`（SHA-256 hash chain 驗證）、dual-write 機制、`event_reducers.go` (`ReduceToSessionData`, `ReduceToTodoList`) |

---

# Part XIII：測試與驗收策略

## 45. Unit tests

- recency/head-tail
- tokenizer fallback
- context budget
- context dedup
- compaction serialization
- event migration
- event hash
- cache policy
- capability invalidation
- resource lock
- side-effect recovery table
- typed task result validation
- team schema lint
- secret redaction

---

## 46. Integration tests

- multi-agent dependency workflow
- verification failure → retry
- model escalation
- branch and replay
- context overflow
- task cache bypass
- interrupted VM/deploy-style task reconcile
- extension permission denial
- SDK/RPC reconnect
- TUI terminal lifecycle

---

## 47. Fault injection

每個 critical boundary 都應有 kill/restart test：

```text
event append 前後
snapshot rename 前後
tool call start/result
artifact write/hash
verification完成
compaction summary
branch leaf change
terminal child exit
policy decision
final acceptance
```

---

## 48. Eval gate

每次大型 kernel 變更，執行固定 eval suite，至少比較：

```text
completion
verification
false success
token usage
latency
duplicate delegation
context retention
recovery correctness
```

不能只以 unit test 綠燈判斷 agent 行為沒有退化。

---

# Part XIV：成功指標

> **量測基線前提**：以下指標必須先以 `hufu improve` 的 telemetry（`internal/improve/`）建立**現況基線**，再設定目標值；無基線的指標無法驗收。每個指標應註明量測來源與資料集。

## 49. Reliability

```text
false-success rate → 趨近 0
interrupted recovery correctness > 99%
verification evidence completeness = 100%（strict tasks）
duplicate re-execution 降低
cache stale-result incidents = 0
```

## 50. Context

```text
latest-user-correction retention = 100%
original-goal retention = 100%
active-task retention = 100%
context duplication 顯著下降
compaction artifact-loss = 0
```

## 51. Developer experience

```text
新增 provider/tool/context extension 不需修改 Coordinator
team lint 可在啟動前發現配置問題
SDK 能以少量程式建立 run
replay 能重現失敗
report 能直接定位 artifact/event/task
```

## 52. Product experience

```text
簡單任務不經多代理 overhead
複雜任務能顯示 DAG 與 evidence
session 可以 fork/time travel
TUI 可解釋 context
dark/light/e-paper 動態切換
```

---

# Part XV：非目標與設計原則

## 53. 非目標

- 不以新增更多內建工具取代 extension contract。
- 不將所有 workflow hardcode 進 Coordinator。
- 不以 Go `plugin` 作為唯一 extension 機制。
- 不讓 RAG memory 覆蓋當前 user/project instructions。
- 不讓自動 retry 重跑未知副作用。
- 不將 report 文字視為唯一證據儲存。
- 不以一次大爆炸重寫取代漸進 migration。

## 54. 設計原則

1. **Durable before autonomous**
2. **Evidence before success**
3. **Explicit policy before prompt convention**
4. **Typed state before free-form interpretation**
5. **Replayable before optimizable**
6. **Fast path for simple work**
7. **Team path for complex work**
8. **Transport is not security**
9. **Memory is reference, not authority**
10. **One source of truth, many projections**

---

# Part XVI：建議下一步（基準 `b9b47557` 更新版）

**下一批可直接指派**（全部 🔴/🟡、依賴皆已滿足，按成本遞增排序）：

```text
1. HF-PR-002  Atomic persistence（S）— 最便宜，直接消除 crash 毀損 session 的風險
2. HF-PR-001R Session head/goal retention（S）
3. HF-PR-003  CachePolicy（M）
4. HF-PR-004  Capability freshness（M）
5. HF-PR-005  Required skills（M）
6. HF-PR-006  Blocking acceptance（M）
7. HF-PR-007  Prompt/tool linter（M）
8. HF-PR-107  Side-effect recovery（L）— 風險最高，M 卡完成後立即做
9. HF-PR-101  Token counter（L）— 解鎖 Phase B 其餘工作
```

**之後的 kernel 工程**（依賴鏈：HF-PR-104 → 201/202/112；HF-PR-105 → 108；HF-PR-101 → 102；HF-PR-109 → 203；HF-PR-003+006 → 113）：

```text
10. HF-PR-105 Typed TaskResult（L）
11. HF-PR-104 Event store dual-write（XL）
~~12. HF-PR-103 Structured compaction 殘餘（M）~~ ✅
13. HF-PR-102 Context compiler（L）
14. HF-PR-106 Resource locks（M）
15. HF-PR-108 Artifact/evidence（L）
16. HF-PR-109 Policy engine（L）
17. HF-PR-110 Workspace separation（M）
18. HF-PR-111 Secret registry（L）
19. HF-PR-113 Execution profiles（M）
20. HF-PR-112 Terminal session manager（L，待 HF-PR-104）
```

**平台與生態**（Phase E/F）：HF-PR-201/202/203/204/205/206/207，時機到了再從工作卡指派。

第一批（1–9）完成後，hufu 獲得 correctness：crash 不毀狀態、cache／acceptance／side-effect 語意明確、context budget 以 token 計；第二批建立乾淨 kernel；第三批才擴張成平台與生態。

最終判斷：

> hufu 最有價值的未來，不是成為功能更多的 CLI，而是成為一個能把多代理決策、工具操作、任務狀態、驗證證據與中斷恢復統一管理的 Go workflow kernel。
