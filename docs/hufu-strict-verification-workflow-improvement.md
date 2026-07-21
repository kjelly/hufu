# hufu 嚴格驗證工作流能力改善設計

> 文件目的：專注整理 **hufu 本身** 在執行高約束、長時間、可稽核工作流時的能力缺口、Go 架構調整、資料模型、測試策略與建議 agent team。
>
> 本版已移除特定外部軟體的操作步驟、命令、選單、錄影格式與 runbook 細節。外部系統只被視為 hufu 要承載的工作負載，不是本文件的設計對象。
>
> 與 roadmap 的分工：本文件是 strict-verification 場景的**機制設計與驗收案例**；Go 子系統的 schema、詞彙、實作狀態與排程以 `docs/hufu-future-improvement-roadmap.md` 的工作卡（HF-PR-xxx）為**單一真相來源**。§6 問題總表與 Part XIX 均已映射至工作卡；修改任一邊的 schema／詞彙時必須同步檢查另一邊。
>
> - 本文件：聚焦 hufu 的 **strict verification／compliance workflow** 能力。
> - Future roadmap：涵蓋 hufu 整體 session、context、SDK、extension、TUI 與產品演進。
>
> 文件狀態：設計提案。凡標示「目標 schema」的 YAML/Go 欄位，目前不一定已被 hufu parser 支援，需先實作再使用。
>
> 分析基準：**HEAD `b9b47557`（2026-07-21）**，與 roadmap 相同。實作前應重新核對當時主分支，避免重做已完成的修正。
>
> 實作狀態速記（以 roadmap 工作卡為準）：HF-PR-104 event store（§20）與 HF-PR-002 atomic persistence 已 🟡 IMPLEMENTED (PENDING REVIEW)；HF-PR-101 token counter、HF-PR-103 compaction 殘餘已 ✅ DONE；HF-PR-003 cache（§13）、HF-PR-004 capability（§14）、HF-PR-006 acceptance（§16）為 🟡 PARTIAL。本文件各章節描述的是**目標狀態**，不代表現況。
>
> 變更紀錄：
>
> - 2026-07-21：以 2400 行重寫版取代舊版（1737 行）。恢復 roadmap 單一真相來源分工；§6 加回 HF-PR-xxx 映射欄；原 Part XIX 獨立 PR backlog（PR-01…PR-23）降格為 HF-PR 映射表，不再作為指派依據；釘回分析基準 commit 並補實作狀態速記。

---

## 1. 執行摘要

hufu 現在已具備優秀的多代理工作流骨架：

- coordinator／worker team
- `TaskDef`
- DAG dependency 與 bounded concurrency
- task verification
- retry、model escalation、judge、skeptic、reflexion
- task journal 與中斷恢復
- preflight capability probe
- hooks、MCP、audit、report、TUI
- unattended mode 與執行 budget

這些能力已足以執行一般多代理任務，也可在有人監督時執行高約束工作流。

目前不足的不是「能否呼叫工具」，而是以下保證：

1. 必要規則是否真的完整載入。
2. 禁止操作是否在程式層被阻擋，而不是只靠 prompt。
3. task 是否只有在證據完整後才進入 done。
4. 重新驗證是否真的重新執行，而不是使用 cache。
5. 中斷後是否依副作用類型安全恢復。
6. 持久化狀態是否可 replay、可稽核、可偵測竄改。
7. secrets 是否跨 tool、MCP、audit、report 一致遮蔽。
8. agent team 是否有穩定 schema、工具合約與 CI lint。

建議 hufu 新增一個第一級執行模式：

```text
strict-verification
```

此模式的核心語意：

```text
Fail closed.
Evidence before success.
No stale cache.
Side-effect-aware recovery.
Typed state.
Deterministic audit trail.
```

---

## 2. 文件範圍

## 2.1 本文件涵蓋

- hufu execution profile
- required skill/resource loading
- policy engine
- tool/MCP authorization
- task cache 與 capability freshness
- recovery semantics
- artifact 與 evidence model
- acceptance gate
- workspace scope
- secret handling
- terminal lifecycle
- context compilation
- typed task result
- event store
- Coordinator 模組化
- report 與 observability
- team schema、lint 與建議 agent team
- Go package、interface、migration 與 tests

## 2.2 本文件不涵蓋

- 外部 CLI 的選單順序
- 特定基礎設施產品的 schema
- 外部 deployment tool 的 bug
- 特定 runbook 的驗證命令
- 外部錄影工具的檔案格式
- 某個專案的 VM topology
- 某個專案應採用的 workaround

上述內容應存在各自專案的 runbook／skill／adapter 中，而不是寫進 hufu core 設計。

---

## 3. 設計目標

### G1：禁止行為由 hufu 程式層阻擋

不能只依賴：

```text
system prompt
agent prompt
skill prose
worker self-report
```

必須有 typed policy decision。

### G2：task 完成與證據完整綁定

```text
agent says done ≠ task done
verification passes ≠ evidence complete
evidence complete + acceptance passes = task/run done
```

### G3：重新驗證必須產生新執行

- 不讀舊 task result
- 不讀不適用的 capability cache
- 不將歷史 memory 當作目前環境事實
- 必須有新的 tool calls、artifacts 與 timestamps

### G4：中斷恢復不重複未知副作用

任何外部 mutation 都必須先 reconcile，再決定：

```text
complete
retry
compensate
manual intervention
```

### G5：所有重要狀態可 replay

至少可從 durable log 重建：

- run
- tasks
- attempts
- tool calls
- verification
- artifacts
- policy decisions
- acceptance
- terminal sessions

### G6：strict mode 不允許模糊成功

未知、缺證據、policy error、acceptance error 應是：

```text
blocked
evidence_error
not_accepted
needs_human
```

不能只是 warning。

---

## 4. 非目標

- 不把所有外部工具內建進 hufu。
- 不讓 Coordinator hardcode 每種工作流。
- 不以更多 prompt 規則取代 policy。
- 不要求一般低風險任務全部使用 strict mode。
- 不一次重寫所有 session、memory、scheduler 與 TUI。
- 不以 Go `plugin` 作為唯一 extension 機制。
- 不把 Markdown report 當作唯一真相來源。

---

## 5. hufu 現有優勢

## 5.1 `TaskDef` 已接近 workflow DSL

現有欄位已能表達：

- agent
- goal
- constraints
- dependency
- pipeline
- verification
- retry
- on-failure
- escalation
- adversarial verification

因此不需替換 scheduler，只需擴充 execution semantics。

## 5.2 DAG scheduler 是良好基礎

現有 scheduler 能處理：

- dependency readiness
- concurrency limit
- cycle detection
- failed dependency
- reset wave
- duplicate/in-flight task

下一步應加入：

- resource claims
- side-effect class
- cache policy
- recovery policy
- evidence gate

## 5.3 task journal 已示範 append-only recovery

task journal 已包含：

- append-only JSONL
- torn line skip
- tombstone
- compaction

這些模式應提升為通用 event store，而非只用於 task result cache。

## 5.4 verification 已存在

hufu 已能在 worker 回報後執行 objective verification。Evidence Service 可在此基礎上擴充，不必另建完全獨立的 workflow。

## 5.5 team／agent 設定已有清楚入口

Markdown agent definition 與 YAML team config 適合持續演進成 versioned schema、lint 與 reusable team package。

---

## 6. 問題總表

> 「對應工作卡」指向 `docs/hufu-future-improvement-roadmap.md` Part XII；schema／詞彙／實作狀態以工作卡為準。

| ID | 優先級 | hufu 問題 | 後果 | 對應工作卡 |
|---|---:|---|---|---|
| HF-PROFILE-001 | P0 | 缺少 strict execution profile | 多個嚴格語意散落於 flag、prompt 與 team config | HF-PR-113 |
| HF-SKILL-001 | P0 | 必要 skill/resource 不是啟動硬閘門 | agent 可能未完整載入規則就執行 | HF-PR-005 |
| HF-POLICY-001 | P0 | hook error 可能 fail-open | policy checker 故障時危險操作仍放行 | HF-PR-109 |
| HF-POLICY-002 | P0 | 缺少 tool/MCP/command/file authorization | 無法在 hufu 層證明操作路徑合法 | HF-PR-109、HF-PR-106 |
| HF-EVIDENCE-001 | P0 | task done 未綁定 evidence complete | 缺 artifact 仍可能成功 | HF-PR-108 |
| HF-CACHE-001 | P0 | task cache 缺少明確 use/refresh/bypass | 重新驗證可能重用舊輸出 | HF-PR-003 |
| HF-CAP-001 | P0 | capability cache 缺少 TTL/generation | 環境改變後可能沿用舊 probe | HF-PR-004 |
| HF-RECOVERY-001 | P0 | resume 未完整區分副作用 | 🟡 IMPLEMENTED (PENDING REVIEW) | HF-PR-107 |
| HF-ACCEPT-001 | P0 | acceptance error 未必阻擋成功 | strict run 可能以 warning 結束 | HF-PR-006 |
| HF-WORKSPACE-001 | P0 | control/subject workspace 無第一級模型 | cleanup 可能刪除控制面資料 | HF-PR-110 |
| HF-SECRET-001 | P0 | secrets 無跨 subsystem 統一遮蔽 | audit、report、history 可能洩密 | HF-PR-111 |
| HF-STATE-001 | P1 | session/journal/audit/report 多份狀態 | 難以 deterministic replay | HF-PR-104 |
| HF-TERM-001 | P1 | terminal session 不是 runtime resource | 長駐 TUI/child lifecycle 難恢復 | HF-PR-112 |
| HF-CTX-001 | P1 | constraints/context 缺少 typed compiler | strict rules 可能在壓縮或注入中遺失 | HF-PR-102 |
| HF-RESULT-001 | P1 | worker output 主要為自由文字 | verification、dependency 與 report 難結構化 | HF-PR-105 |
| HF-ARCH-001 | P1 | Coordinator 責任過多 | 🟡 IMPLEMENTED (PENDING REVIEW) | 6 大核心子服務介面解耦已實作，roadmap §17 |
| HF-REPORT-001 | P1 | report 內嵌截斷文字多、artifact refs 少 | 完整證據無法稽核 | HF-PR-108 |
| HF-TEAM-001 | P1 | team schema 無版本與 lint | prompt/tool drift 無法在 CI 發現 | HF-PR-007、HF-PR-204 |
| HF-OBS-001 | P2 | 缺少 replay、trace、context/evidence inspector | 難診斷 agent 行為 | （roadmap Part VII；HF-PR-205 部分涵蓋） |
| HF-EVAL-001 | P2 | 缺少固定 agent workflow eval | kernel 變更可能造成行為退化 | （roadmap Part VII §24，未編號 backlog） |

---

# Part I：Strict Execution Profile

## 7. 新增 `strict-verification` profile

目前嚴格工作流的需求可能散落在：

- `--new`
- `--no-journal`
- `--unattended`
- `--force-mcp`
- team prompt
- skill
- hooks
- acceptance command
- 手動操作習慣

建議集中為 profile。

```go
type ExecutionProfileName string

const (
    ProfileDefault            ExecutionProfileName = "default"
    ProfileUnattended         ExecutionProfileName = "unattended"
    ProfileStrictVerification ExecutionProfileName = "strict-verification"
)
```

```go
type ExecutionProfile struct {
    Name ExecutionProfileName

    StrictPolicy               bool
    PolicyFailureMode          PolicyFailureMode
    AcceptanceMode             AcceptanceMode

    RequireLockedResources     bool
    RequireEvidenceManifest    bool
    RequireClosedTerminals     bool
    RequireWorkspaceIsolation  bool

    DefaultCachePolicy         CachePolicy
    DefaultRecoveryPolicy      RecoveryPolicy

    DisableHistoricalTaskReuse bool
    DisableHistoricalMemory    bool
    DisableSemanticDedup       bool

    FailOnUnknownState         bool
}
```

Strict profile 預設：

```go
ExecutionProfile{
    Name:                       ProfileStrictVerification,
    StrictPolicy:               true,
    PolicyFailureMode:          PolicyFailClosed,
    AcceptanceMode:             AcceptanceBlocking,
    RequireLockedResources:     true,
    RequireEvidenceManifest:    true,
    RequireClosedTerminals:     true,
    RequireWorkspaceIsolation:  true,
    DefaultCachePolicy:         CacheBypass,
    DefaultRecoveryPolicy:      RecoveryReconcile,
    DisableHistoricalTaskReuse: true,
    DisableHistoricalMemory:    true,
    DisableSemanticDedup:       true,
    FailOnUnknownState:         true,
}
```

CLI：

```text
hufu --profile strict-verification ...
```

或 team：

```yaml
execution-profile: strict-verification
```

### 驗收條件

- profile resolution 可在 dry-run 顯示所有有效語意。
- strict profile 不得被低優先級 flag 靜默降級。
- report 必須記錄 resolved profile。
- profile schema 有版本。

---

# Part II：Required Resources

## 8. 必要 skill/resource lock

## 8.1 問題

「skill 可被找到」與「skill 已完整載入且整個 run 使用同一版本」是不同概念。

Strict workflow 需要：

- 明確路徑
- 必須存在
- 完整讀取
- hash 鎖定
- 注入指定 agents
- 執行中不可靜默切版

## 8.2 資料模型

```go
type RequiredResourceKind string

const (
    ResourceSkill        RequiredResourceKind = "skill"
    ResourcePrompt       RequiredResourceKind = "prompt"
    ResourceProjectRules RequiredResourceKind = "project_rules"
    ResourceSchema       RequiredResourceKind = "schema"
)

type RequiredResourceSpec struct {
    Name       string
    Kind       RequiredResourceKind
    Path       string
    SHA256     string
    InjectInto []string
    Required   bool
}
```

Lock record：

```go
type LockedResource struct {
    Name          string
    Kind          RequiredResourceKind
    CanonicalPath string
    SHA256        string
    ByteSize      int64
    LoadedAt      time.Time
    SnapshotRef   ArtifactRef
}
```

## 8.3 啟動流程

```text
resolve path
→ reject symlink/path ambiguity if policy requires
→ read complete content
→ compute hash
→ compare expected hash
→ snapshot to control artifact store
→ inject locked snapshot
→ write resource_locked event
```

## 8.4 Skill discovery 改善

一般 discovery 可加入：

- project root `.agents/skills`
- ancestor `.agents/skills`
- explicit `--skill-path`
- team-local skills
- global skills

優先序必須 deterministic，且同名 collision 要報錯或有明確 override 規則。

## 8.5 驗收條件

- required resource 缺失時，在第一個 mutation tool 前失敗。
- 同名 skill collision 不可靜默取第一個。
- resource 執行中變更不影響已鎖定 snapshot。
- final report 顯示 resource hash。

---

# Part III：Fail-Closed Policy Engine

## 9. 將 policy 從 prompt 提升到核心

## 9.1 Policy 不應只是 hook convention

Policy 應控制：

- 哪個 agent 可呼叫哪個 tool
- MCP server/tool 是否可用
- command executable/arguments
- working directory
- read/write paths
- task phase
- side-effect class
- secret references
- terminal creation
- task transition
- finish/acceptance

## 9.2 介面

```go
type PolicyDecisionCode string

const (
    DecisionAllow PolicyDecisionCode = "allow"
    DecisionDeny  PolicyDecisionCode = "deny"
)

type PolicyDecision struct {
    Code        PolicyDecisionCode
    RuleID      string
    Reason      string
    Obligations []PolicyObligation
}

type PolicyEngine interface {
    AuthorizeTask(ctx context.Context, req TaskAuthorizationRequest) (PolicyDecision, error)
    AuthorizeToolCall(ctx context.Context, req ToolAuthorizationRequest) (PolicyDecision, error)
    AuthorizeMCPCall(ctx context.Context, req MCPAuthorizationRequest) (PolicyDecision, error)
    AuthorizeTransition(ctx context.Context, req TaskTransitionRequest) (PolicyDecision, error)
    AuthorizeFinish(ctx context.Context, req FinishAuthorizationRequest) (PolicyDecision, error)
}
```

## 9.3 Fail-closed

```go
type PolicyFailureMode string

const (
    PolicyFailOpen   PolicyFailureMode = "open"
    PolicyFailClosed PolicyFailureMode = "closed"
)
```

Strict mode：

```text
policy error        → deny
policy timeout      → deny
unknown tool        → deny
unknown command     → deny
unknown write path  → deny
invalid response    → deny
```

## 9.4 Hook 改善

保留 shell hooks，但加入：

```yaml
hooks:
  before_tool_call:
    command: ./policy-check
    timeout: 5s
    failure-mode: closed
```

Hook response 應 versioned：

```json
{
  "api_version": "hufu.io/v1alpha1",
  "decision": "deny",
  "rule_id": "no-direct-exec",
  "reason": "..."
}
```

## 9.5 Tool/MCP middleware

不能因 transport 不同而繞過 policy。

```text
built-in tool
MCP tool
custom command tool
terminal start
extension tool
```

都必須通過同一 authorization pipeline。

## 9.6 Command policy

避免只做 prefix string match。至少正規化：

- executable
- argv
- shell
- cwd
- env names
- nested shell invocation
- redirects
- referenced paths

Strict mode 對無法解析的複雜 shell command 可預設阻擋，要求改用 typed command tool。

## 9.7 驗收條件

- hook exit non-zero 時 strict tool call 被阻擋。
- MCP 包裝同一禁止命令也被阻擋。
- policy decision 寫入 durable event。
- policy denial 不觸發一般 retry loop。
- policy unavailable 時 task 進入 `policy_blocked`。

---

# Part IV：Task Execution Semantics

## 10. 擴充 `TaskDef`

建議將執行語意集中在 `Execution`：

```go
type TaskDef struct {
    Agent       string   `json:"agent"`
    Goal        string   `json:"goal"`
    Constraints string   `json:"constraints,omitempty"`

    DependsOn   []int    `json:"depends_on,omitempty"`
    Pipeline    bool     `json:"pipeline,omitempty"`

    Verify      string   `json:"verify,omitempty"`
    VerifyMode  string   `json:"verify_mode,omitempty"`
    Requires    []string `json:"requires,omitempty"`

    MaxRetries  int      `json:"max_retries,omitempty"`
    OnFailure   *int     `json:"on_failure,omitempty"`
    Escalate    bool     `json:"escalate,omitempty"`

    Execution   TaskExecutionPolicy   `json:"execution,omitempty"`
    Evidence    []EvidenceRequirement `json:"evidence,omitempty"`
    Resources   []ResourceClaim       `json:"resources,omitempty"`
}
```

```go
type TaskExecutionPolicy struct {
    CachePolicy       CachePolicy
    RecoveryPolicy    RecoveryPolicy
    SideEffect        SideEffectClass

    AllowedTools      []string
    AllowedCommands   []string
    DeniedCommands    []string

    ReadOnlyPaths     []string
    WritablePaths     []string

    FreshCapabilities bool
    RequiresHuman     bool
    StrictResult      bool
}
```

## 10.1 Resource claims

Dependency 不能完整表達共享資源衝突。

```go
type ResourceClaimMode string

const (
    ResourceRead      ResourceClaimMode = "read"
    ResourceWrite     ResourceClaimMode = "write"
    ResourceExclusive ResourceClaimMode = "exclusive"
)

type ResourceClaim struct {
    Resource string
    Mode     ResourceClaimMode
}
```

Scheduler 應同時檢查：

```text
dependency ready
concurrency slot
resource claim compatible
policy allow
capability fresh
```

## 10.2 Task status

建議新增：

```text
pending
planned
policy_checking
policy_blocked
in_progress
waiting_external
verifying
evidence_verifying
done
error
blocked
needs_human
interrupted
reconciling
skipped
```

Strict task 不得直接：

```text
in_progress -> done
```

---

# Part V：Evidence 與 Artifact

## 11. Artifact Store

## 11.1 問題

stdout、stderr、tool result、report summary 可能被截斷，無法作完整證據。

## 11.2 介面

```go
type ArtifactRef struct {
    ID          string
    Kind        string
    Path        string
    SHA256      string
    ByteSize    int64
    MediaType   string

    RunID       string
    TaskID      string
    Attempt     int
    Agent       string
    ToolCallID  string
    CreatedAt   time.Time
}

type ArtifactStore interface {
    Put(ctx context.Context, req PutArtifactRequest) (ArtifactRef, error)
    Verify(ctx context.Context, ref ArtifactRef) error
    Open(ctx context.Context, id string) (io.ReadCloser, error)
    ListByTask(ctx context.Context, taskID string) ([]ArtifactRef, error)
}
```

Artifact kinds：

```text
stdout
stderr
raw_tool_result
verification_output
report
manifest
transcript
recording
diff
test_report
coverage
generated_file
snapshot
```

## 11.3 Immutable storage

Strict mode：

- artifact 建立後不可覆寫
- 修改後建立新 artifact/version
- report 只引用 artifact ID/hash
- artifact hash mismatch 阻擋 acceptance

---

## 12. Evidence Service

## 12.1 Requirement

```go
type EvidenceRequirement struct {
    ID        string
    Kind      string
    Required  bool
    Validator string
    Inputs    []string
    Expected  map[string]any
}
```

## 12.2 Result

```go
type EvidenceResult struct {
    RequirementID string
    Status        string
    Validator     string
    ArtifactRefs  []ArtifactRef
    Assertions    []AssertionResult
    CheckedAt     time.Time
}
```

## 12.3 Manifest

```go
type EvidenceManifest struct {
    SchemaVersion int
    RunID         string
    TaskID        string
    Attempt       int

    PolicyDecisionRefs []string
    ArtifactRefs       []ArtifactRef
    EvidenceResults    []EvidenceResult

    PreviousHash string
    ManifestHash string
    Status       string
}
```

## 12.4 Task transition

```text
worker completed
→ objective verification
→ artifact persistence
→ evidence validators
→ policy transition check
→ done
```

任一 required evidence：

```text
missing
invalid
hash mismatch
unknown
```

都不能完成 task。

## 12.5 驗收條件

- 刪除 required artifact 後 acceptance 失敗。
- 修改 artifact 一個 byte 後 hash validator 失敗。
- task report 顯示 manifest reference。
- evidence validator error 在 strict mode fail-closed。
- final run manifest 可獨立驗證。

---

# Part VI：Cache 與 Freshness

## 13. 明確 Cache Policy

```go
type CachePolicy string

const (
    CacheUse     CachePolicy = "use"
    CacheRefresh CachePolicy = "refresh"
    CacheBypass  CachePolicy = "bypass"
)
```

| Policy | Read | Execute | Write |
|---|---:|---:|---:|
| use | 是 | miss 才執行 | 是 |
| refresh | 否 | 一定執行 | 是 |
| bypass | 否 | 一定執行 | 否 |

Strict verification 預設：

```text
mutation task     = bypass
live verification = bypass
idempotency task  = bypass
analysis-only     = refresh 或 use
```

## 13.1 Cache identity

若允許 cache，key 至少包含：

```text
agent identity/version
goal + constraints
verification specification
source revision
workspace generation
required resource hashes
tool registry version
dependency result hashes
policy version
```

## 13.2 禁止 semantic reuse 的情境

- 使用者要求重新執行
- current-state verification
- benchmark
- security check
- environment mutation
- idempotency
- evidence collection

## 13.3 Report

每個 task 顯示：

```text
cache policy
cache lookup attempted
cache hit
cache source run/task
freshness checks
```

---

## 14. Capability Cache

## 14.1 問題

Capability 可能因前一個 task 而改變：

- executable 安裝/刪除
- remote host rebuild
- credential rotation
- network state
- writable path
- service availability

## 14.2 資料模型

```go
type CapabilityFreshness string

const (
    CapabilitySession   CapabilityFreshness = "session"
    CapabilityGeneration CapabilityFreshness = "generation"
    CapabilityImmediate CapabilityFreshness = "immediate"
)

type CapabilityRequirement struct {
    Name      string
    Probe     string
    Timeout   int64
    Scope     string
    Freshness CapabilityFreshness
    TTL       time.Duration
}
```

## 14.3 Invalidation

Task 完成後依 side effect：

```go
type CapabilityInvalidation struct {
    Scopes []string
    Names  []string
}
```

Environment mutation 至少 bump generation。

## 14.4 驗收條件

- generation 改變後舊 probe 不命中。
- immediate probe 每次執行。
- report 顯示 checked_at 與 generation。
- probe output 保存在 artifact store。

---

# Part VII：Recovery

## 15. Side-effect Class

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

## 15.1 Recovery Policy

```go
type RecoveryPolicy string

const (
    RecoveryRetry     RecoveryPolicy = "retry"
    RecoveryReconcile RecoveryPolicy = "reconcile"
    RecoveryManual    RecoveryPolicy = "manual"
    RecoveryNever     RecoveryPolicy = "never"
)
```

## 15.2 Recovery flow

```text
load unfinished operation
→ identify side-effect class
→ inspect durable tool/task events
→ execute read-only reconcile if configured
→ classify state:
   not_started
   complete
   partial
   unknown
→ choose:
   mark done
   retry
   compensate
   needs human
```

## 15.3 Tool recovery metadata

```go
type ToolRecoverySpec struct {
    RetrySafe       bool
    IdempotencyKey  string
    ReconcileTool   string
    CompensateTool  string
}
```

## 15.4 Retry 與 attempt

每次 attempt 必須有獨立：

- attempt ID
- tool calls
- artifacts
- evidence
- model
- failure classification

不可將多次 attempt 的 output 混成同一筆不可追溯結果。

## 15.5 驗收條件

- 模擬外部 mutation 成功後 crash，resume 不直接盲目重跑。
- `RecoveryManual` task 一律進入 needs_human。
- reconcile result 有 artifact/evidence。
- recovery decision 寫入 event store。

---

# Part VIII：Acceptance

## 16. Blocking Acceptance

```go
type AcceptanceMode string

const (
    AcceptanceAdvisory AcceptanceMode = "advisory"
    AcceptanceBlocking AcceptanceMode = "blocking"
)
```

Strict profile 使用 blocking。

Final gate：

```text
all mandatory tasks terminal
no policy_blocked
no evidence_error
no unresolved failed task
required resources still valid
all artifacts verify
all terminal sessions closed
run manifest valid
acceptance validator pass
```

## 16.1 Finish semantics

建議 run terminal states：

```text
succeeded
failed
blocked
not_accepted
completed_with_exceptions
aborted
```

Strict mode 預設禁止：

```text
completed_with_exceptions
```

除非使用者以明確 override policy 核准。

## 16.2 Exit code

CLI 應對應：

```text
0 succeeded
2 failed
3 blocked
4 not_accepted
5 needs_human
130 aborted
```

實際數值可調整，但語意要穩定並文件化。

---

# Part IX：Workspace Scope

## 17. Control Workspace 與 Subject Workspace

hufu 應區分：

```go
type WorkspaceScope struct {
    ControlRoot string
    SubjectRoot string
    ProjectRoot string
}
```

### ControlRoot

保存：

- session
- event log
- journal
- audit
- artifacts
- manifests
- report
- locked resources

### SubjectRoot

worker 真正操作的目標 workspace。

## 17.1 驗證

```go
func ValidateWorkspaceScope(scope WorkspaceScope) error
```

檢查：

- absolute/canonical path
- symlink
- control/subject ancestor relation
- root/home/protected directory
- policy allowed path
- task writable path

## 17.2 Tool context

每個 tool call 明確取得：

```text
ControlRoot
SubjectRoot
ProjectRoot
AllowedReadPaths
AllowedWritePaths
```

不可使用模糊的單一 `workspace` 同時代表控制面與操作面。

## 17.3 驗收條件

- subject 包含 control 時啟動失敗。
- control path 不能被一般 worker write/delete。
- report 顯示 canonical roots。
- symlink escape 測試通過。

---

# Part X：Secret Handling

## 18. Secret Registry

```go
type SecretRef struct {
    Name   string
    Source string
}

type SecretResolver interface {
    Resolve(ctx context.Context, ref SecretRef) ([]byte, error)
}

type Redactor interface {
    RedactText(text string) string
    RedactJSON(v any) any
}
```

## 18.1 原則

- model 只接收 secret reference，不接收 value。
- value 在 tool execution boundary 才解析。
- tool args 在 audit 前 redaction。
- stdout/stderr/tool result 在 persistence 前 redaction。
- artifact 可標示 sensitivity。
- secret scan 為 final acceptance requirement。
- redactor error 在 strict mode fail-closed。

## 18.2 Sensitive artifact

```go
type ArtifactSensitivity string

const (
    ArtifactPublic     ArtifactSensitivity = "public"
    ArtifactInternal   ArtifactSensitivity = "internal"
    ArtifactSensitive  ArtifactSensitivity = "sensitive"
)
```

Report 不直接 link sensitive artifact，必須經授權取得。

## 18.3 驗收條件

測試 secret 經過：

```text
tool input
MCP input
environment
stdout
stderr
audit
task result
report
session history
```

所有持久化內容 exact-value scan 為零。

---

# Part XI：Terminal Runtime

## 19. Terminal Session Manager

長駐 child process、互動 TUI 與 streaming command 不應只是一般 tool response。

```go
type TerminalSession struct {
    ID          string
    RunID       string
    OwnerTaskID string
    Agent       string

    Command     []string
    WorkingDir  string
    StartedAt   time.Time
    LastReadAt  time.Time

    Running     bool
    ExitCode    *int

    OutputRefs  []ArtifactRef
}
```

```go
type TerminalManager interface {
    Start(ctx context.Context, req TerminalStartRequest) (*TerminalSession, error)
    Write(ctx context.Context, id string, input TerminalInput) error
    Read(ctx context.Context, id string) (TerminalReadResult, error)
    Close(ctx context.Context, id string) error
    List(ctx context.Context, runID string) ([]TerminalSession, error)
    Reconcile(ctx context.Context, id string) (TerminalSession, error)
}
```

## 19.1 Lifecycle rules

- session 有單一 owner task。
- task 結束前 session 必須 closed 或 child exited。
- model timeout 與 child timeout 分離。
- abort 不應自動重跑 child。
- session event durable。
- leaked terminal 阻擋 strict finish。

## 19.2 驗收條件

- process restart 後能知道 terminal 是 running/unknown/exited。
- 非 owner task 不能寫入 session。
- terminal leak 在 final gate 被偵測。
- output 保存為 artifact，而非只保留最後片段。

---

# Part XII：Durable Event Store

## 20. 統一事件模型

建議以 append-only event log 作為 durable source of truth。

```go
type RunEvent struct {
    SchemaVersion int             `json:"schema_version"`
    ID            string          `json:"id"`
    Sequence      uint64          `json:"sequence"`

    RunID         string          `json:"run_id"`
    SessionID     string          `json:"session_id"`
    BranchID      string          `json:"branch_id,omitempty"`
    TaskID        string          `json:"task_id,omitempty"`
    Attempt       int             `json:"attempt,omitempty"`

    Actor         string          `json:"actor"`
    Type          string          `json:"type"`
    Timestamp     time.Time       `json:"timestamp"`
    Payload       json.RawMessage `json:"payload"`

    PreviousHash  string          `json:"previous_hash,omitempty"`
    Hash          string          `json:"hash,omitempty"`
}
```

## 20.1 事件類型

```text
run_started
profile_resolved
resource_locked
task_created
task_authorized
task_started
tool_authorized
tool_call_started
tool_call_finished
terminal_started
terminal_closed
verification_started
verification_finished
artifact_created
evidence_validated
task_completed
task_failed
task_interrupted
recovery_decided
acceptance_finished
run_finished
```

## 20.2 Projection

```text
event log
├── session state
├── task/todo state
├── audit view
├── report
├── TUI
├── recovery state
└── metrics
```

## 20.3 Migration

1. dual-write legacy files + event log
2. reducers
3. replay comparison tests
4. event log 成為 source of truth
5. legacy files 成為 projection/export

## 20.4 Hash chain

Strict mode可開啟 event hash chain，偵測刪除、插入與修改。

---

# Part XIII：Context 與 Result

## 21. ContextCompiler

Strict constraints 不應只是一大段 prompt 字串。

```go
type ContextItem struct {
    ID           string
    Kind         string
    Content      string
    Source       string
    Priority     int
    Required     bool
    TokenCount   int
    Provenance   []string
    DedupKey     string
}
```

Context pipeline：

```text
collect
→ normalize
→ scope
→ deduplicate
→ rank
→ token budget
→ validate required items
→ emit
```

Strict required context：

```text
current goal
hard constraints
locked resources
resolved policy summary
task execution policy
verification/evidence requirements
dependency results
approved exceptions
```

## 21.1 Required context validator

在每次 LLM turn 前檢查：

- required item 是否存在
- content hash 是否符合 lock
- 是否因 compaction 被移除
- 是否超出模型 context
- 是否有互相衝突的 constraints

若 required item 無法放入 context：

```text
不要靜默裁切
→ compact 其他低優先內容
→ 仍無法容納則 blocked
```

---

## 22. Typed TaskResult

```go
type TaskResult struct {
    TaskID        string
    Attempt       int
    Agent         string
    Status        TaskStatus
    Summary       string

    ArtifactRefs  []ArtifactRef
    EvidenceRefs  []string

    FilesRead     []FileRef
    FilesModified []FileRef
    Commands      []CommandResult
    Verification  []VerificationResult

    Decisions     []Decision
    Findings      []Finding
    Risks         []Risk
    OpenQuestions []string

    RetryHint     string
    RawOutputRef  *ArtifactRef
}
```

## 22.1 提交方式

新增 coordinator/worker tool：

```text
submit_result
```

Strict task 必須提交 typed result，不能只依 final text parser。

一般 task 可使用 compatibility parser，但標示：

```text
source = parsed_free_text
confidence < 1
```

## 22.2 Dependency injection

下游 task 接收：

- typed summary
- artifact refs
- evidence refs
- decisions/findings
- 不直接注入所有 raw output

可降低 context 與誤解。

---

# Part XIV：Coordinator 模組化

## 23. 問題

Coordinator 同時持有：

- providers
- agents
- task scheduler
- cache
- journal
- session
- memory
- skills
- sidecar/judge/guard
- hooks
- budgets
- TUI status
- acceptance
- permissions

這會造成：

- 修改一處影響多個 subsystem
- 難以單元測試
- 難以嵌入 SDK
- 難以替換 storage/policy
- strict mode 容易形成更多 if/flag

## 23.1 目標 façade

```go
type Coordinator struct {
    planner    Planner
    workflow   WorkflowEngine
    context    ContextCompiler
    agents     AgentPool
    policies   PolicyEngine
    sessions   SessionStore
    events     EventStore
    cache      TaskCache
    recovery   RecoveryManager
    evidence   EvidenceService
    artifacts  ArtifactStore
    terminals  TerminalManager
    memory     MemoryService
    observer   EventSink
}
```

Coordinator 本身只負責：

```text
receive request
resolve plan
coordinate services
return result
```

## 23.2 漸進抽取順序

1. `TaskCache`
2. `CapabilityService`
3. `EvidenceService`
4. `PolicyEngine`
5. `ContextCompiler`
6. `RecoveryManager`
7. `EventStore/SessionStore`
8. `WorkflowEngine`
9. Coordinator 變 façade

每一步保留 legacy adapter。

---

# Part XV：Report 與 Observability

## 24. Report 應引用 artifact

Report task section：

```text
Task ID
Agent
Status
Attempt
Side-effect class
Cache policy/result
Recovery policy/result
Policy decisions
Verification summary
Evidence manifest
Artifact references
Timing/tokens/model
```

不要把完整 output 截斷後直接當唯一證據。

## 24.1 Run manifest

```go
type RunManifest struct {
    SchemaVersion int
    RunID         string
    Profile       string

    LockedResources []LockedResource
    Tasks           []TaskManifest
    Artifacts       []ArtifactRef
    Acceptance      AcceptanceResult

    EventHeadHash   string
    ManifestHash    string
}
```

新增：

```text
hufu verify-run <manifest>
```

可在不啟動 agent 的情況下驗證 hash、references 與 required evidence。

## 24.2 Trace IDs

所有事件與 artifact 帶：

```text
run_id
session_id
task_id
attempt
agent
model
tool_call_id
parent_span_id
sequence
```

---

# Part XVI：Team Schema 與 Lint

## 25. Versioned team schema

```yaml
apiVersion: hufu.io/v1alpha1
kind: AgentTeam
metadata:
  name: strict-verification
spec:
  ...
```

## 25.1 `hufu team lint`

檢查：

- unknown tool
- unknown skill
- duplicate agent
- missing coordinator
- prompt 參照不存在 tool
- invalid timeout
- unsafe concurrency
- invalid model
- required skill missing
- strict agent 缺少 typed result
- side-effect task 使用 retry policy
- resource claim conflict
- acceptance missing
- unsupported schema field

## 25.2 Prompt/tool drift

Bundled team CI 應掃描 prompt 中的 tool 名稱，和實際 registry 比對。

Prompt 不應成為無測試的 API consumer。

## 25.3 Team contract tests

每個 bundled strict team 至少測：

```text
required resource failure
policy denial
blocked task
verification failure
evidence failure
recovery branch
acceptance failure
finish gate
```

---

# Part XVII：建議 Agent Team

## 26. Team 名稱

```text
strict-verification
```

這是通用 hufu team，不綁定任何外部產品。

## 26.1 Agents

| Agent | 權限類型 | 職責 |
|---|---|---|
| coordinator | 無直接 mutation | 建立序列化 workflow、控制 stop/finish |
| requirements-analyst | read-only | 讀取目標、constraints、skills、schemas，建立 execution contract |
| operator | policy-scoped mutation | 執行唯一被授權的外部操作 |
| evidence-verifier | read-only | 驗證 artifacts、hash、assertions、task evidence |
| bug-investigator | read-only | 建立假設、核對執行記錄與程式碼 |
| single-change-fixer | narrow write | 一次只執行一個已核准變更 |
| final-auditor | read-only | 檢查 run manifest、exceptions、acceptance |

## 26.2 Coordinator 規則

- 必要 resources 未 lock，不得 mutation。
- mutation task 預設 `max-concurrent=1`。
- 一個 task 只授權一個清楚 scope。
- evidence verifier 與 operator 分離。
- 不讓執行者自行驗證所有證據。
- bug investigation 完成前不得啟動 fixer。
- strict task 使用 cache bypass。
- unknown state 進 needs_human。
- mandatory evidence 缺失不得 finish。

## 26.3 Workflow

```text
lock requirements/resources
→ preflight
→ build typed plan
→ policy validation
→ execute one scoped task
→ objective verification
→ evidence verification
→ next task
→ anomaly investigation
→ approved single-variable fix
→ re-verification
→ final audit
→ blocking acceptance
→ finish
```

## 26.4 Team config 目標示例

以下是未來 schema 方向，需 parser 支援後才可直接使用：

```yaml
apiVersion: hufu.io/v1alpha1
kind: AgentTeam

metadata:
  name: strict-verification

spec:
  execution-profile: strict-verification

  max-concurrent: 1
  max-rounds: 100

  required-resources:
    - kind: skill
      name: verification-policy
      path: .agents/skills/verification-policy/SKILL.md
      required: true

  policy:
    mode: fail-closed

  cache:
    default: bypass

  recovery:
    default: reconcile

  evidence:
    required: true
    hash-chain: true

  acceptance:
    mode: blocking

  agents:
    - coordinator
    - requirements-analyst
    - operator
    - evidence-verifier
    - bug-investigator
    - single-change-fixer
    - final-auditor
```

---

# Part XVIII：建議 Package Layout

## 27. 新增 packages

```text
internal/
├── profile/
│   ├── profile.go
│   └── resolver.go
├── policy/
│   ├── engine.go
│   ├── tool.go
│   ├── command.go
│   ├── filesystem.go
│   └── decision.go
├── evidence/
│   ├── requirement.go
│   ├── validator.go
│   ├── manifest.go
│   └── service.go
├── artifact/
│   ├── store.go
│   ├── hash.go
│   └── sensitivity.go
├── recovery/
│   ├── policy.go
│   ├── manager.go
│   └── reconcile.go
├── eventstore/
│   ├── event.go
│   ├── jsonl.go
│   ├── hashchain.go
│   └── replay.go
├── terminal/
│   ├── manager.go
│   ├── session.go
│   └── recovery.go
├── contextcompiler/
│   ├── item.go
│   ├── compiler.go
│   └── validator.go
├── secret/
│   ├── registry.go
│   ├── resolver.go
│   └── redactor.go
└── teamschema/
    ├── schema.go
    ├── lint.go
    └── contract_test.go
```

## 27.1 現有 package 的調整方向

```text
internal/team/coordinator.go
    → façade，減少直接持有 subsystem 狀態

internal/team/coordinator_task_run.go
    → 使用 TaskExecutionPolicy / RecoveryManager / EvidenceService

internal/team/coordinator_taskcache.go
    → 抽為 TaskCache interface

internal/team/capability.go
    → 抽為 CapabilityService

internal/team/task_journal.go
    → 逐步併入 EventStore

internal/hooks/
    → 支援 failure-mode 與 versioned response

internal/audit/
    → 改為 event projection，整合 redaction

internal/tools/
    → 全部接 policy/secret/artifact middleware

cmd/hufu/report.go
    → 從 RunManifest/EventStore 建立 report
```

---

# Part XIX：實作順序：HF-PR 映射表

> 本 Part 不是獨立 backlog。**實作狀態、工作量、依賴、驗證指令與指派指令以 `docs/hufu-future-improvement-roadmap.md` Part XII 工作卡為準**；本文件各節為對應卡的詳細設計與驗收案例。

| 內容 | 對應工作卡 | 設計細節 |
|---|---|---|
| ExecutionProfile（strict-verification profile） | **HF-PR-113** | §7 |
| Required resource/skill lock | **HF-PR-005** | §8 |
| Hook fail-closed | **HF-PR-109** | §9.4 |
| In-process PolicyEngine | **HF-PR-109** | §9.2、§9.3 |
| Tool/MCP middleware 與 command policy | **HF-PR-109** | §9.5、§9.6 |
| TaskExecutionPolicy（`TaskDef.Execution`） | **HF-PR-003、HF-PR-105、HF-PR-107** | §10 |
| Resource claims | **HF-PR-106** | §10.1 |
| Artifact store | **HF-PR-108** | §11 |
| Evidence requirement／manifest | **HF-PR-108** | §12 |
| CachePolicy | **HF-PR-003** | §13 |
| Capability freshness | **HF-PR-004** | §14 |
| Side-effect class／recovery policy | **HF-PR-107** | §15 |
| Blocking acceptance | **HF-PR-006** | §16 |
| Control/subject workspace scope | **HF-PR-110** | §17 |
| Secret registry／redaction | **HF-PR-111** | §18 |
| Terminal session manager | **HF-PR-112** | §19 |
| Unified event store／reducers／dual-write | **HF-PR-104**（已實作待 Review） | §20 |
| ContextCompiler | **HF-PR-102** | §21 |
| Typed TaskResult／`submit_result` | **HF-PR-105** | §22 |
| Coordinator façade | （無獨立卡，隨各卡抽取演進，roadmap §17） | §23 |
| Report artifact refs／run manifest | **HF-PR-108** | §24 |
| Team schema／lint | **HF-PR-007、HF-PR-204** | §25 |
| strict-verification team 範本 | （roadmap §32 衍生；無獨立卡） | §26 |

建議落地順序（與 roadmap Part XVI 對齊）：

1. 第一批（roadmap Phase A P0 批次）：HF-PR-005、HF-PR-003、HF-PR-004、HF-PR-006、HF-PR-007、HF-PR-107、HF-PR-110（HF-PR-002 已實作待 Review）。
2. 第二批：HF-PR-109、HF-PR-108（完成後 strict workflow 核心閘門齊備）。
3. 第三批：HF-PR-111、HF-PR-113、HF-PR-105、HF-PR-106、HF-PR-102、HF-PR-112（HF-PR-104 已實作待 Review；HF-PR-112 依賴其 lifecycle event）。

---

# Part XX：測試策略

## 34. Unit Tests

### Profile

- override precedence
- strict defaults
- invalid downgrade

### Resource Lock

- missing path
- hash mismatch
- collision
- symlink
- mid-run change

### Policy

- allow/deny
- timeout
- invalid hook response
- MCP bypass
- unknown command
- path escape

### Cache

- use
- refresh
- bypass
- source revision change
- policy version change

### Capability

- TTL
- generation
- immediate freshness
- invalidation

### Evidence

- missing artifact
- hash mismatch
- validator failure
- manifest chain

### Secret

- input/result/audit/report redaction
- scan hit
- resolver error

### Recovery

- safe retry
- reconcile complete
- reconcile partial
- manual
- unknown state

### Team Lint

- unknown tool
- missing skill
- prompt/tool mismatch
- unsafe recovery
- no acceptance

---

## 35. Integration Tests

### Scenario A：Strict success

```text
required resources lock
→ policy allow
→ task execute
→ verification
→ evidence
→ blocking acceptance
→ success
```

### Scenario B：Policy failure

```text
policy backend error
→ policy_blocked
→ no tool process starts
```

### Scenario C：Cache bypass

```text
same task run twice
→ two new executions
→ distinct artifacts
→ cache hit=false
```

### Scenario D：Evidence missing

```text
worker success
→ verification success
→ required artifact missing
→ evidence_error
→ finish blocked
```

### Scenario E：Interrupted mutation

```text
external side effect
→ process crash
→ resume
→ reconcile
→ no blind retry
```

### Scenario F：Secret propagation

```text
secret ref
→ tool
→ stdout/stderr
→ report
→ exact-value scan zero
```

---

## 36. Fault Injection

在以下邊界強制 crash：

```text
policy allow 後、tool start 前
tool start 後、result 前
artifact write 後、hash 前
verification後、manifest前
task done event 後、snapshot前
terminal child exit 前後
acceptance前後
```

每個 fault 必須有 expected recovery state。

---

## 37. Golden Agent Eval

建立固定 scripted provider 回覆：

- requirements analyst
- operator
- evidence verifier
- bug investigator
- final auditor

評估：

```text
是否遵守 task scope
是否呼叫禁止 tool
是否缺 evidence
是否誤用 cache
是否提早 finish
是否在 unknown state 亂猜
```

---

# Part XXI：成功指標

## 38. Reliability

```text
strict task false-success = 0
required evidence completeness = 100%
policy fail-open incidents = 0
stale cache reuse incidents = 0
blind mutation retry incidents = 0
```

## 39. Recovery

```text
unfinished operation classification coverage
reconcile success rate
manual escalation accuracy
duplicate side-effect rate
```

## 40. Auditability

```text
task → tool call → artifact → evidence → acceptance 可完整追溯
event sequence gap = 0
artifact hash verification = 100%
secret scan pass = 100%
```

## 41. Developer Experience

```text
新增 policy 不需修改 Coordinator
新增 validator 不需修改 scheduler
team lint 在啟動前發現錯誤
fault scenario 可 deterministic replay
```

---

# Part XXII：最小可行版本

若要以最少 Go 改動先獲得最大可靠性，建議順序：

```text
1. strict-verification profile
2. required resource/skill lock
3. CachePolicy bypass
4. capability generation invalidation
5. blocking acceptance
6. hook fail-closed
7. ArtifactStore
8. EvidenceManifest
9. control/subject workspace
10. side-effect/recovery policy
```

這十項完成後，hufu 已可從「主要靠 prompt 遵守」提升為「核心可執行的 strict workflow」。

---

# Part XXIII：最終驗收標準

hufu strict workflow 必須能用自身資料回答：

1. 本 run 使用哪個 execution profile？
2. 哪些 resources 被鎖定？hash 是什麼？
3. 每個 task 的 cache policy 是什麼？
4. 每個 mutation 的 side-effect/recovery policy 是什麼？
5. 每個 tool/MCP call 的 policy decision 是什麼？
6. 哪些 artifacts 由哪個 task/tool call 產生？
7. required evidence 是否全部通過？
8. 是否有 artifact hash mismatch？
9. 是否曾有 policy error 或未知操作？
10. 中斷後執行了哪個 reconcile？
11. 是否有任何 terminal session 未關閉？
12. acceptance 是否為 blocking pass？
13. final manifest 是否可離線驗證？
14. secrets 是否出現在任何 persistent artifact？
15. event log 是否可重建最終 task/run 狀態？

若其中任一項在 strict mode 無法確定，run 不應標記成功。

---

## 42. 建議優先檢視的 hufu 程式區域

```text
internal/team/coordinator.go
internal/team/coordinator_run.go
internal/team/coordinator_execute.go
internal/team/coordinator_task_run.go
internal/team/coordinator_taskcache.go
internal/team/dag_scheduler.go
internal/team/coordinator_session.go
internal/team/task_journal.go
internal/team/coordinator_memory.go
internal/team/coordinator_skills.go
internal/team/coordinator_tools.go
internal/team/coordinator_tools_delegate.go
internal/team/capability.go
internal/team/parse.go

internal/agent/agent.go
internal/agent/capability.go

internal/hooks/hooks.go
internal/hooks/shell.go

internal/audit/audit.go

internal/tools/bash.go
internal/tools/bash_policy.go
internal/tools/tools.go
internal/tools/ask_user.go

cmd/hufu/team_loader.go
cmd/hufu/team_runner.go
cmd/hufu/report.go

.agent-teams/operator/
```

---

## 43. 結論

hufu 已經有多代理工作流最困難的一半：task orchestration、verification、retry、journal 與 team model。

下一階段應專注把以下語意從 prompt／習慣提升為 Go 核心能力：

```text
required resources
fail-closed policy
explicit cache
side-effect-aware recovery
artifact/evidence gate
blocking acceptance
durable event log
typed task result
```

完成後，hufu 的定位將不只是「能協調多個 LLM agent」，而是：

> **能以可驗證、可恢復、可稽核方式執行高風險多代理工作流的 Go runtime。**
