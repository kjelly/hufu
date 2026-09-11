# Team Contract Lint / Prompt-Tool Drift 實作規格

> Status: implemented and archived
> Priority: P1
> Baseline: `f57e4d3d5ffb993317ef6c34938f50067b5a51b3`
> Dependency: Versioned AgentTeam Schema（已於此 baseline 完成）

## 0. Implementation status (archived: implementation landed)

PR-0 through PR-4 below were implemented in commits `9ae0729`, `f37cbb5`,
`43ddd5f`, `1a3f549`, and `5bd0195`. This document is retained as design and
implementation history; the current code, tests, and CLI help are authoritative.

## 1. 目標與邊界

建立三個互不偷渡語意的命令：

```text
team validate → runtime/schema validity；維持既有成功條件
team explain  → resolved effective contract；維持既有輸出責任
team lint     → authoring drift、可疑 mismatch、可供 CI 使用的 findings
```

`team lint`：

- 預設不修改 runtime semantics，也不讓 `team validate` 自動執行全部 lint rule。
- 不呼叫 LLM、不存取網路、不建立 workspace、不啟動 local/remote MCP server，也不執行 verifier/reconcile command。
- 可以讀取 team directory、project `.agents/skills/`、global `~/.agents/skills/` 與 `hufu.yaml` profile。
- 只判定 authored contract 與可離線解析的 effective static contract；需要 live provider/MCP metadata 的答案必須是 `unknown`。

非目標：

- 不以 NLP/LLM 猜測自然語言意圖。
- 不證明 shell command、MCP endpoint 或外部資源在執行時一定存在。
- 不自動修檔。
- 第一版不支援 manifest 內永久 waiver；只提供 invocation-scoped CLI waiver。

## 2. 診斷模型與既有型別相容性

`internal/team.ContractFinding` 仍是 runtime validator 的 canonical semantic diagnostic。不得為同一問題建立第二組 code。

`TeamLintFinding` 是 source-located CLI projection；既有 `ContractFinding` 必須透過單一 adapter 轉換，新 lint rule 也必須使用同一份 code registry 與 severity constants。

```go
type TeamLintFinding struct {
    Code           string `json:"code"`
    Severity       string `json:"severity"` // error|warning|info
    File           string `json:"file,omitempty"`
    Line           int    `json:"line,omitempty"`   // 1-based；未知為 0
    Column         int    `json:"column,omitempty"` // 1-based；未知為 0
    LocationStatus string `json:"location_status"`  // exact|rendered|field_only
    Agent          string `json:"agent,omitempty"`
    FieldPath      string `json:"field_path,omitempty"`
    Resolution     string `json:"resolution,omitempty"`
    Message        string `json:"message"`
    Suggestion     string `json:"suggestion,omitempty"`
    Ignored        bool   `json:"ignored,omitempty"`
}
```

相容規則：

- `ContractFinding.Field` 原樣投影至 `FieldPath`。
- `ContractFinding.Hint` 原樣投影至 `Suggestion`。
- 既有 code 原樣保留，例如 `legacy_fanout_deprecated`；不得另發同義的 `legacy_fanout_source`。
- 新 code 在單一 constants 區宣告並附 predicate 註解。
- `File` 一律為相對 team directory 的 slash-separated path；不得輸出絕對路徑。
- findings 在套用 waiver 前先完成 deterministic sort。
- 同一 source span、field 與 semantic predicate 若已由既有 validator 產生 finding，新 rule 不得再產生同義 finding；adapter 優先保留既有 code。

`Resolution` 可用值：

```text
not_found | draft_only | out_of_scope | available | unknown
```

正常的 `available` 通常不產生 finding；該值保留給 explain/debug 測試與未來輸出。

## 3. Lint pipeline 與錯誤分類

不得單純 `CompileTeam(...)` 失敗後立即返回，否則 duplicate agent、multiple coordinator 與 unsupported schema 永遠無法成為 structured finding。

需要把既有 loader 重構成共用 pipeline；runtime 與 lint 不得各自實作一份解析／驗證邏輯：

```text
read source documents
  → detect schema envelope
  → parse with runtime parser
  → collect recoverable authoring diagnostics
  → normalize / preset expansion
  → build partial or complete TeamSession
  → existing contract validators
  → offline effective resolvers
  → prompt/tool/skill lint
  → source-location projection
```

建議內部介面：

```go
type TeamCompileMode string

const (
    TeamCompileRuntime TeamCompileMode = "runtime"
    TeamCompileLint    TeamCompileMode = "lint"
)

type TeamInspection struct {
    Spec        *EffectiveTeamSpec // 無法安全 normalize 時可為 nil
    Session     *TeamSession       // 不完整時可為 nil
    Sources     *TeamSourceIndex
    Diagnostics []ContractFinding
    Complete    bool
}
```

`Complete=true` 表示所有適用的 lint phase 都已安全執行；因 semantic finding 而無法進入 effective phase 時為 `false`，但仍輸出已取得的 findings，且仍依 `--fail-on` 決定 exit。`Complete=false` 本身不另外改變 threshold。

實際名稱可調整，但必須符合：

- runtime mode 遇到 error-severity semantic diagnostic 時維持現在的 fail-closed 行為。
- lint mode 將 semantic authoring problem 轉為 finding，並在安全時繼續其他規則。
- source reader 只建立 AST/source index，不得自行決定 runtime validity。
- agent identity、coordinator、task contract 等 predicate 必須抽自現有 loader/validator，不能在 linter 複製。
- 如果前一階段不足以安全執行 effective rule，跳過該 rule；不得用猜測值繼續。

錯誤分類：

| 狀況 | 輸出 | Exit |
|---|---|---:|
| 可讀且 envelope 可解析，但 schema 不支援 | `unsupported_schema_version` finding | 依 `--fail-on`，預設 1 |
| duplicate agent、missing/multiple coordinator、未知 task agent、cycle | structured finding | 依 `--fail-on` |
| malformed YAML/frontmatter、template render failure、檔案不可讀 | load error；stderr 或 JSON error envelope | 2 |
| internal invariant、marshal failure | internal error；stderr 或 JSON error envelope | 2 |
| effective phase 因既有 error finding 無法繼續 | 保留已取得 findings；不誤報為 internal error | 依 `--fail-on` |

## 4. Source location

新增 `TeamSourceIndex`，以 `yaml.Node` 保存 team manifest field path 的 line/column，並保存每個 agent frontmatter/body 的檔案與行號偏移。

規則：

- 未模板化且可直接映射的 node/body span：`location_status=exact`。
- 模板化後仍能以相同 field path 對回 authored node：使用 authored line/column，`exact`。
- 只有 rendered document 可定位：回報 rendered line/column，`location_status=rendered`，message 必須說明行號基於 rendered template。
- template control flow 動態生成欄位而無法穩定反查：`Line=0`、`Column=0`、`location_status=field_only`，但 `File` 與 `FieldPath` 必須存在。
- prompt finding 必須指向實際 directive token，而不是 agent 檔案第一行。
- built-in Helper 等無 source file 的項目使用 `field_only`，不得捏造檔名或行號。

Source mapping 只負責定位；runtime parser 的 rendered value 仍是判定 authority。

## 5. Offline effective tool resolver（PR 前置條件）

現有 runtime `ToolResolver.ResolveTaskTools` 依賴 Coordinator、Todo、model 與 MCP manager，不能由 lint 直接建立或呼叫。先抽出 pure/offline resolution core，然後讓 runtime resolver 與 lint 共同使用。

概念介面：

```go
type ToolAvailability string

const (
    ToolAvailable ToolAvailability = "available"
    ToolDenied    ToolAvailability = "denied"
    ToolMissing   ToolAvailability = "missing"
    ToolUnknown   ToolAvailability = "unknown"
)

type StaticToolResolutionInput struct {
    Session          *TeamSession
    Agent            *agent.AgentDef
    Task             *TaskDef
    LifecycleMode    WorkerToolResolutionMode
    ExecutionProfile ExecutionProfile
    Policy           EffectiveTeamContractContext
}

type StaticToolResolution struct {
    Tools map[string]ToolAvailability
}
```

名稱可調整，但同一 resolution core 必須處理：

```text
agent/preset tool grants
+ implied aliases（例如 read→view、find→glob）
+ runtime-required protocol tools
+ task tool_sequence / task grants
+ team allow/deny
+ no-net / force-mcp
+ execution profile and side-effect policy
+ agent-declared MCP tools
+ team MCP declarations
= effective static tool set/status
```

限制：

- pure resolver 不建立 `fantasy.AgentTool`、不接觸 provider、不啟動 MCP、不執行 command。
- runtime `defaultToolResolver` 必須消費 pure resolver 的決策，再綁定 concrete handlers；不得保留另一份 allow/deny 判定。
- model capability 或 live MCP metadata 無法離線判定時回 `unknown`，不得假裝 `missing`。
- lint 必須有測試證明不會建立網路連線或 MCP subprocess。

### 5.1 Profile/CLI context

`team lint` 預設評估 manifest 已解析的 execution profile 與 team policy。

若使用 inherited `--profile` 或 `--execution-profile`：

- `runTeamLint` 必須呼叫既有 `applyProfile`。
- 只將與 static policy 有關的 resolved flags 投影到 `EffectiveTeamContractContext`，至少包含 `unattended`、`no-net`、`force-mcp`、`plan`、`execution-profile`。
- precedence 與 runtime 一致：explicit CLI > hufu.yaml profile > team manifest > built-in default。
- model/provider 的 live capability 不因此被探測；仍回 `unknown`。

`team lint` 必須另外註冊本命令可用的 `--unattended`、`--no-net`、`--force-mcp`、`--plan` policy flags，使 explicit CLI override 不依賴 root-only flag 的 Cobra 繼承細節；不得直接重用 package global flag storage。`--profile` 套用到同名 lint flag 後再建立 context。

## 6. MCP offline semantics

第一版一律 offline：

- agent frontmatter `mcp-tools` 的 key 是 declared local tool，名稱可確定。
- team MCP server 名稱與 `allowedTools`/`excludedTools` 可讀，但 `allowedTools` 只是 author intent，不是 live existence proof。
- reference 指向不存在的 MCP server：`mcp_tool_missing`，error。
- server 存在但沒有離線 metadata 可證明該 tool：`mcp_tool_unknown`，info，`resolution=unknown`。
- tool 被 `excludedTools`、team deny 或 force-mcp policy 明確阻擋：依情境輸出 denied finding，不得輸出 unknown。
- lint 不因 `mcp_tool_unknown` 單獨失敗，除非使用者設定 `--fail-on info`。

## 7. Skill resolution

skill resolver 必須使用與 runtime 相同的 search path 與 precedence，另外以 `DiscoverSkills(..., true)` 建立只供診斷使用的 draft index；draft 絕不可加入 runtime-visible pool。

需要區分：

```text
not_found   → 所有 search path 都不存在
draft_only  → 只在 drafts/ 找到
out_of_scope→ production skill 存在，但被 team include/exclude 或 agent scope 排除
available   → 該 agent 執行時可載入
```

第一版的 machine-readable required skill 來源限定為：

- team `skills` 明列的名稱；
- agent frontmatter `skills` 明列的名稱；
- `required-resources` 中 kind=`skill` 的名稱。

自然語言只有符合第 8 節 directive grammar 才算 prompt reference；一般提到「skill」或某個英文單字不算 requirement。

規則：

- machine-readable required skill 為 `not_found` 或 `draft_only`：`required_skill_missing`，error。
- prompt directive 指向 `not_found`/`draft_only`：`prompt_unknown_skill`，warning，保留實際 `Resolution`。
- production skill 存在但對 agent 為 `out_of_scope`：`skill_not_available_to_agent`，error。
- draft 不能滿足 production requirement。
- dependency expansion 與 `skills-exclude` 必須重用 `ExpandSkillDependenciesForSet`/runtime filter authority。

## 8. Prompt scanning grammar

第一版只掃描：

- 每個 authored agent `.md` 的 system prompt body；
- static task 的 `goal` 與 `constraints`。

不掃描 team description、README、skill body、runtime-generated prompt 或 fenced code block。

只接受下列 case-insensitive explicit directives；`NAME` 必須符合 `[A-Za-z][A-Za-z0-9_.:-]*`：

```text
use `NAME` tool
use the `NAME` tool
call `NAME` tool
invoke `NAME` tool
tool: NAME
tool: `NAME`

use `NAME` skill
use the `NAME` skill
load `NAME` skill
invoke `NAME` skill
skill: NAME
skill: `NAME`
```

`use|call|invoke|load` 與 `tool|skill` 中間只允許規格所示 whitespace/`the`；不要擴張為任意自然語言。單獨 code span（例如 `` `kubectl` is an example command ``）不得觸發。

Normalization 必須重用 runtime tool/skill name normalization 與 alias expansion。分類 precedence：

1. 明確 denied → denied finding。
2. registry 中不存在 → unknown/missing finding。
3. registry 已知但不在 agent effective grant → not-granted finding。
4. available → no finding。

不得新增 NLP、stemming、模糊比對或 LLM fallback。

## 9. Rule catalog

下表是第一版完整 predicate；未列出的推測不得自行加入。

### 9.1 Schema/lifecycle

| Code | Severity | Predicate |
|---|---|---|
| `unsupported_schema_version` | error | envelope YAML 可解析且 `apiVersion` 非空但不在 supported versions |
| `deprecated_field` | warning | authored YAML key 位於中央 deprecated-field registry；message 必須列 replacement |
| `legacy_execution_provider_field` | warning | team/agent 使用中央 registry 標記的 legacy provider selector，而 canonical backend/target field 未取代它 |
| `legacy_fanout_deprecated` | warning | 重用既有 path-based TSV fan-out predicate；不要另建同義 code |

中央 deprecated-field registry 必須明列 field path、schema versions、replacement 與移除版本；沒有 registry entry 就不發 finding。此 baseline 尚無已核准的 generic `deprecated_field` entry，因此第一版先保留 code/registry framework，但不得猜測或發出該 finding。

`legacy_execution_provider_field` 在此 baseline 的初始 registry 只有：

- legacy flat schema 的 `model`，或 v1alpha1 的 `spec.model`：當對應的 `worker-model`/`coordinator-model` 未明確設定時，建議改用 role-specific selector；一個 source field 只發一筆 finding。
- legacy flat schema 的 `providers`，或 v1alpha1 的 `spec.providers`：replacement 為 `backends`。
- legacy flat schema 的 `subagent-providers`，或 v1alpha1 的 `spec.subagent-providers`：replacement 為 `backends`。

`provider-url`、`provider-api-key`、agent-level `model` 與 `subagent-provider` 在此 baseline 仍有 runtime-supported 用途，第一版不得因名稱看似 legacy 就發 warning。

### 9.2 Tool

| Code | Severity | Predicate |
|---|---|---|
| `prompt_unknown_tool` | warning | explicit prompt directive 指向非 built-in、非 agent MCP、非 team MCP namespace 的名稱 |
| `prompt_denied_tool` | error | explicit prompt directive 指向被 effective policy 明確 deny 的 tool |
| `prompt_tool_not_granted` | warning | tool 已知且未被全域 deny，但不在該 agent effective grant |
| `declared_tool_missing` | error | agent/preset authored tool 名稱經 alias expansion 後不在任何離線 registry；`all` 除外 |
| `mcp_tool_missing` | error | MCP-qualified reference 的 server 未宣告，或離線 authoritative snapshot 明確證明 tool 不存在 |
| `mcp_tool_unknown` | info | server 已宣告，但 live catalog 未探測且離線資料無法證明 tool 是否存在 |

### 9.3 Skill

| Code | Severity | Predicate |
|---|---|---|
| `prompt_unknown_skill` | warning | explicit prompt directive 的 skill 為 `not_found` 或 `draft_only` |
| `required_skill_missing` | error | machine-readable required skill 為 `not_found` 或 `draft_only` |
| `skill_not_available_to_agent` | error | production skill 存在但對引用 agent 為 `out_of_scope` |

### 9.4 Topology

| Code | Severity | Predicate |
|---|---|---|
| `duplicate_agent` | error | 兩個不同 agent file 的 normalized file alias/name identity collision；同一 agent 自己的 alias+name 不算重複 |
| `missing_coordinator` | error | authored agents 中沒有 role=`coordinator`（built-in Helper 不算） |
| `multiple_coordinators` | error | authored agents 中超過一個 role=`coordinator`；取代語意未定義的 `multiple_unqualified_coordinators` |
| `unknown_task_agent` | error | static task 的 normalized agent 不在 loaded authored workers/built-in Helper registry |
| `dependency_cycle` | error | static tasks 的 `depends_on` 形成 cycle；重用 runtime cycle predicate |

### 9.5 Runtime semantics

| Code | Severity | Predicate |
|---|---|---|
| `side_effect_retry_without_reconcile` | error | agent default 或 static task 的 effective side effect 是 `external_write`/`infra_mutation`/`credential_mutation`/`unknown`，effective recovery 是 `retry`，且沒有 reconcile-tool 或 blocking verifier |
| `strict_task_without_typed_result` | warning | effective execution profile `StrictPolicy=true` 或 task `requires_verification=true`，但 authored static task 未宣告 `requires_result=true`；runtime normalization 仍維持原行為 |
| `acceptance_missing_for_unattended` | error | effective unattended=true 且沒有 non-advisory acceptance command/spec checks |
| `verifier_missing` | error | 非 structured task 設定 `requires_verification=true`，但沒有 asserting `verify`/`verify-spec`；直接投影既有 validator code |
| `execution_steps_verifier_missing` | error | structured task 設定 `requires_verification=true`，但沒有 structured verify step；直接投影既有 validator code |
| `resource_claim_conflict` | error | 兩個 static task 的 resource claims 依 runtime `claimsConflict` 衝突，且彼此沒有 direct/transitive dependency ordering |
| `timeout_impossible` | warning | 有 verifier 的 static task，其 effective `verify-timeout` 大於或等於該 agent effective task timeout，使完整 verify window 不可能在 shared parent deadline 內取得 |

`verification_contract_incomplete` 是原提案的 rule-group 名稱，不是新的 emitted code；上述兩個既有 code 是公開輸出。相同原則適用於既有 `initial_contract_agent_unknown`、`goal_contract_agent_unknown` 與 `action_recovery_incompatible`：adapter 保留既有 code，general rule 只補足既有 validator 未涵蓋的 case，不得在同一 field 重複輸出 `unknown_task_agent` 或 `side_effect_retry_without_reconcile`。

若 runtime 已有相同 predicate/code，直接重用既有 code；上表 code 僅在沒有既有 canonical code 時新增。任何 code rename 必須在第一個公開 release 前完成，發布後不得無遷移直接變更。

## 10. CLI

```bash
hufu team lint <team-directory>
hufu team lint --team hufu-coding
hufu team lint --format text
hufu team lint --format json
hufu team lint --fail-on warning
hufu team lint --ignore prompt_unknown_tool@reviewer.md:42
hufu team lint --profile safe --team hufu-coding
```

參數：

- positional directory 與 `--team` 互斥，重用 `resolveTeamDirArg`。
- `--format`: `text|json`，預設 `text`。
- `--fail-on`: `error|warning|info|none`，預設 `error`。
- `--ignore`: repeatable selector；套用後 finding 保留在 JSON，但設為 `ignored:true`，且不參與 threshold。

Threshold：

```text
error   → 只有未忽略 error 阻擋
warning → 未忽略 warning/error 阻擋
info    → 任一未忽略 finding 阻擋
none    → findings 永不造成 exit 1
```

Exit：

```text
0 no finding reaches threshold
1 threshold reached
2 CLI usage、invalid selector、load/internal error
```

必須新增實作 `ProcessExitCode() int` 的 typed CLI error；一般 `RunE` error 在現有 `main.go` 只會得到 exit 1，不能拿來表示 exit 2。

### 10.1 Waiver selector

```text
CODE                 # 此 invocation 中所有同 code findings；刻意 broad
CODE@FILE             # 限定 team-relative file
CODE@FILE:LINE        # 限定精確 source line
```

- `FILE` 必須是 clean team-relative slash path，不接受 absolute path 或 `..`。
- code 不在 registry、line 非正整數或 selector malformed：exit 2。
- ignored finding 在 text 顯示 `[ignored]`，在 JSON 保留 `ignored:true`。
- 第一版不提供 `--no-lint`。

### 10.2 JSON contract

成功完成 lint pipeline 時輸出：

```json
{
  "schema_version": 1,
  "team": "hufu-coding",
  "complete": true,
  "findings": [],
  "summary": {
    "error": 0,
    "warning": 0,
    "info": 0,
    "ignored": 0
  }
}
```

Load/internal error 且 `--format json` 時輸出單一 JSON document 至 stdout：

```json
{
  "schema_version": 1,
  "team": "",
  "complete": false,
  "error": {
    "kind": "load_error",
    "message": "..."
  }
}
```

JSON 規則：

- stdout 只包含一個 JSON document；human diagnostics 不得混入。
- paths 使用 team-relative slash form。
- findings 依 `file,line,column,code,agent,field_path,message` 升冪排序。
- summary 計數包含 ignored finding 的原 severity；`ignored` 另外計數。
- 新增 optional field 可以更新 schema 文件；刪除、改名或改型別必須升 `schema_version`。

## 11. 檔案配置

新增：

```text
internal/team/team_lint.go              # orchestration、rule registry、sort
internal/team/team_lint_source.go       # yaml.Node/body source index
internal/team/team_lint_prompt.go       # 第 8 節 grammar
internal/team/team_lint_tools.go        # offline tool findings
internal/team/team_lint_skills.go       # skill status findings
internal/team/team_lint_semantics.go    # topology/runtime-semantic findings
internal/team/team_lint_test.go
cmd/hufu/teamlintcmd.go
cmd/hufu/teamlintcmd_test.go
```

修改／重用：

```text
internal/team/effective_spec.go
internal/team/contract_finding.go
internal/team/team_policy_lint.go
internal/team/services.go               # 抽 pure tool resolution core
internal/team/parse.go                  # shared runtime/lint compile pipeline
internal/team/team_manifest.go
internal/skill/*
cmd/hufu/teamcmd.go
cmd/hufu/profile.go
```

實際拆檔可依 package cohesion 微調，但不得把所有規則塞進 CLI package。

## 12. PR 順序

### PR-0：共用基礎

- pure/offline tool resolution core；runtime resolver 改為消費它。
- shared compile/inspection pipeline 與 source index。
- 必須證明 runtime tool surface/authorization semantics 沒有改變。

### PR-1：Finding projection + CLI + existing deterministic findings

- adapter、JSON v1、sorting、threshold、typed exit code。
- 投影既有 contract findings。
- schema/topology findings。

### PR-2：Prompt/tool/skill drift

- exact directive scanner。
- offline MCP states。
- skill four-state resolution。
- source-located prompt findings。

### PR-3：Runtime semantic rules + bundled team CI

- 第 9.5 節 predicates。
- bundled teams 無 blocking findings。
- 如有真實 drift，修 team definition；不得以 broad waiver 隱藏。

### PR-4：Waiver 與公開 JSON stability gate

- selector parser、ignored projection。
- golden JSON fixtures 與 backward-compatibility test。

若 PR-3 需要 legitimate false-positive waiver，應先合併 PR-4 的 selector 部分或對調 PR-3/PR-4；不得在 CI 暫時加入 `--no-lint`。

## 13. Tests

最低測試集：

```text
TestTeamLintUnsupportedSchemaIsFinding
TestTeamLintMalformedYAMLIsExit2
TestTeamLintDuplicateAgentIsStructuredFinding
TestTeamLintMissingCoordinator
TestTeamLintMultipleCoordinators
TestTeamLintUnknownTaskAgent
TestTeamLintDependencyCycle

TestTeamLintUnknownTool
TestTeamLintDeniedTool
TestTeamLintToolNotGranted
TestTeamLintKnownToolNoFinding
TestTeamLintDeclaredToolMissing
TestTeamLintToolAliasUsesRuntimeNormalization
TestTeamLintMCPMissingServer
TestTeamLintMCPMetadataUnknown
TestTeamLintDoesNotStartMCPOrNetwork

TestTeamLintUnknownSkill
TestTeamLintDraftSkillIsNotProductionRequirement
TestTeamLintSkillOutOfScope
TestTeamLintSkillDependencyUsesRuntimeExpansion

TestTeamLintSideEffectRetryNeedsRecovery
TestTeamLintStrictTaskWithoutTypedResult
TestTeamLintAcceptanceMissingForUnattended
TestTeamLintVerifierMissingReusesExistingCode
TestTeamLintStructuredVerifierMissingReusesExistingCode
TestTeamLintResourceClaimConflict
TestTeamLintTimeoutImpossible
TestTeamLintLegacyFanoutReusesExistingCode

TestPromptExampleDoesNotTriggerFalseUnknownTool
TestPromptFencedCodeDoesNotTrigger
TestPromptDirectiveSourceLine
TestTemplatedFieldLocationFallback

TestTeamLintJSONStable
TestTeamLintJSONWritesOneDocument
TestTeamLintFindingsSorted
TestTeamLintFailThreshold
TestTeamLintExit2UsesTypedError
TestTeamLintIgnoreByCode
TestTeamLintIgnoreByFileAndLine
TestTeamLintRejectsUnknownIgnoreCode
TestTeamLintProfileMatchesRuntimePolicyProjection
TestBundledTeamsHaveNoLintErrors
```

PR-0 額外 parity tests：對代表性的 preset、alias、team deny、force-mcp、no-net、task tool sequence、result/plan lifecycle mode，pure resolver 的 names/status 必須與 runtime concrete `ResolvedWorkerTools.Names` 一致；live-only metadata 差異只允許落在明文定義的 `unknown`。

## 14. Done

- `team validate` 既有成功條件與 runtime fail-closed 行為未變。
- lint 不呼叫 LLM、network、provider、MCP process 或 verifier command。
- runtime 與 lint 共用 tool/skill resolution authority，不複製 allow/deny 邏輯。
- load-time semantic authoring problems 能形成 structured findings；真正 load/internal failure 為 exit 2。
- findings code、severity、JSON schema、排序與 source-location fallback 穩定。
- prompt scanner 僅接受第 8 節 deterministic grammar。
- MCP unknown 不誤報 missing；draft skill 不滿足 production requirement。
- `--fail-on`、typed exit code 與精確 waiver selector 有 process-level tests。
- bundled teams 無未忽略的 error findings。
- `go test ./...`、`go vet ./...`、`golangci-lint run` 全部成功。

## 15. Coding-agent 指派摘要

先完成 PR-0 的 shared inspection pipeline 與 pure tool resolver，再新增 `hufu team lint`。不要改 `team validate` 的成功條件，不要從 linter 建立 Coordinator/MCP manager，不要複製 runtime 授權邏輯。Prompt 掃描只能實作第 8 節的 deterministic directives；規格未定義的自然語言形式一律不猜。
