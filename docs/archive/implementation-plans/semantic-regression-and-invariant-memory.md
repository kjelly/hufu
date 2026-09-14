# Repository Invariant Catalog + Semantic Regression 實作計畫

> Status: implemented and archived
> Priority: P0
> Baseline: `623964ab12b067a9614615fdbf2d46eaceaa8660`
> Completed: 2026-09-14 (PR-1..5: `288ae4b`, `7b1838b`, `1009234`, `0cffb45`, `10bc6e7`)
> Risk: high — 變更 typed-result、context、completion 與 audit contract
> Normative: 本文件是 [AI 時代仍需記憶](../analyses/ai-era-memory-runtime-principles.md)
> 第一版的唯一實作契約

## 1. 決策摘要

第一版只實作一條可重播、fail-closed 的窄路徑：

```text
trusted invariants.yaml
  -> TeamSession.InvariantCatalog
  -> ContextRouter
  -> persisted ContextInjectionManifest
  -> model InvariantAssessmentClaim
  -> runtime InvariantAssessment attestation
  -> report-only projection OR CompletionGate
  -> auditverify independently replays and verifies the attestation
```

關鍵邊界：

1. `invariants.yaml` 是 invariant 的 canonical source。它與 `team.yaml` 位於同一個
   team trust boundary；`memory_save`、worker finding、prompt 文字都不能新增或修改
   可阻擋 completion 的 invariant。
2. Model 只能提交 `InvariantAssessmentClaim`。只有 runtime 能根據該 task 實際收到的
   context manifest 建立 `InvariantAssessment`；後者是 completion/audit 唯一消費的
   semantic-regression 事實。
3. 靜態 task contract 明確指定 `invariant-verification: report|gate`。`report` 描述
   被審查對象但不影響 run completion；`gate` 才是 completion policy。
4. 修復 violation 必須重新執行同一個 verifier Todo occurrence。completion 與 audit
   只讀 terminal projection 中目前的 `TypedResult`；舊 attempt 留在 event lineage 作為
   歷史，不再代表 unresolved state。
5. coordinator 與 auditverify 各自讀自己的 authoritative projection，但共用
   `internal/team` 的 pure evaluator。獨立驗證是「重新由 events 推導」，不是複製兩份
   policy 程式碼。

## 2. 範圍與非目標

### 2.1 本系列交付

- repository-owned invariant manifest 與嚴格 validation；
- `ContextInvariant` canonical kind；
- verifier task 的單一路徑 context routing 與 attribution manifest；
- finding severity；
- model claim 到 runtime attestation 的 trust-boundary conversion；
- report/gate 兩種不混淆的完成語義；
- event/session/task-journal replay；
- auditverify 的 mandatory semantic-regression dimension；
- CLI JSON、report、inspect 與 review-team integration。

### 2.2 不在第一版

- LLM 自動產生、promotion 或修改 invariant；
- `memory_save(category=invariant)`；
- SQLite context migration 或把 repository catalog 寫入 `context.sqlite`；
- AST/CFG/形式驗證器、自動 component inference；
- 數值化 Internal Model Coverage；
- arbitrary glob/regex applicability；
- multi-model/judge/skeptic semantic-verifier task；
- `--default` team 的 invariant catalog。

## 3. Canonical invariant manifest

### 3.1 路徑與檔案格式

若存在，`LoadTeam` 讀取 `<team-dir>/invariants.yaml`。檔案必須是 regular file，不能是
symlink，大小不得超過 256 KiB。不存在表示該 team 沒有 invariant catalog。
此檔不做 template interpolation、環境變數展開或 include；bytes 經嚴格 YAML decode 後即為
唯一輸入，避免 CLI vars 在不同 run 改變 policy。

```yaml
schema-version: 1
invariants:
  - id: sqlite-canonical-memory
    statement: "context.sqlite is canonical; context-stm.md and context-ltm.md are projections only"
    severity: error
    applies-to:
      - internal/context/
      - internal/team/shared_memory.go
  - id: tui-update-pure
    statement: "internal/tui Model.Update performs no I/O or global mutation"
    severity: warning
    applies-to:
      - internal/tui/
```

Canonical Go model（新增 `internal/team/invariant.go`）：

```go
const InvariantManifestSchemaVersion = 1

type InvariantSeverity string

const (
    InvariantSeverityError   InvariantSeverity = "error"
    InvariantSeverityWarning InvariantSeverity = "warning"
    InvariantSeverityInfo    InvariantSeverity = "info"
)

type InvariantDefinition struct {
    ID        string            `json:"id" yaml:"id"`
    Statement string            `json:"statement" yaml:"statement"`
    Severity  InvariantSeverity `json:"severity" yaml:"severity"`
    AppliesTo []string          `json:"applies_to" yaml:"applies-to"`
}

type InvariantManifest struct {
    SchemaVersion int                   `json:"schema_version" yaml:"schema-version"`
    Invariants    []InvariantDefinition `json:"invariants" yaml:"invariants"`
}
```

`TeamSession` 新增 `InvariantCatalog []InvariantDefinition`。catalog 是 immutable load-time
state；clone coordinator/extra execution 不得分享可變 slice backing array。

### 3.2 Validation

Runtime load 與 `hufu team validate` 使用同一個 validator：

- YAML decoder 使用 `KnownFields(true)`，只接受一個 document；unknown keys、duplicate
  mapping keys 與 trailing document 都是 hard error；
- `schema-version` 必須等於 1；
- 最多 128 筆 invariant；
- `id` 必須符合 `^[a-z][a-z0-9_-]{0,63}$`，且在 team 內唯一；
- `statement` trim、CRLF 正規化並經 `utils.RedactSecrets` 後 1–2048 runes；整份
  normalized statement 總量最多 64 KiB；後續 hash、注入與 attestation 一律使用同一份
  normalized/redacted 內容；
- `severity` 只能是 `error|warning|info`；不得省略；
- `applies-to` 必須有 1–32 筆，每筆最多 256 bytes；
- applicability 只接受 `*`、repository-relative exact path，或以 `/` 結尾的 directory
  prefix；拒絕 absolute path、空 segment、`.`、`..`、backslash、NUL 與 glob 字元；
- normalize 後 de-duplicate 並排序，duplicate invariant ID 是 hard error；
- manifest 存在但為空是 hard error。

`*` 表示所有 verifier task。directory prefix 只做 slash-boundary prefix match；例如
`internal/team/` 匹配 `internal/team/x.go`，不匹配 `internal/teams.go`。

### 3.3 ContextItem materialization

Catalog 不寫入 SQLite。`ContextRouter` 在 route 當下把 definition materialize 成現有
`internal/context.ContextItem`：

```text
ID          = "invariant:" + normalizedTeamName + ":" + definition.ID
Kind        = ContextInvariant
Content     = canonicalInvariantContent(definition)
ContentHash = SHA256(Content)
Scope       = {ProjectID: c.projectDir, TeamID: c.session.Config.Name}
Authority   = AuthorityRepository
TrustLevel  = TrustTrusted
Priority    = PriorityCritical
MustKeep    = true
Lifecycle   = LifecycleConfirmed
Source      = {type:"team_invariant_manifest", path:"invariants.yaml", ref:definition.ID}
Tags        = normalized applies-to entries
Metadata    = {"invariant.id": ID, "invariant.severity": severity,
               "activation.phases":"VERIFY"}
CreatedAt / UpdatedAt = zero (catalog has no runtime freshness; ordering falls back to ID)
```

Scope 的 SessionID/BranchID/AgentID/TaskID/AttemptID 全部為空；repository invariant 不隨
workspace session 或 branch 改變，也不呼叫 `contextScope()` 取得 session-scoped 值。

`normalizedTeamName` 明確使用現有 package-local `normalizedName(session.Config.Name)`（trim +
lowercase）；`LoadTeam` 已為無 team manifest 的目錄填入 basename，若最後仍為空則 catalog
load hard-fail，不能生成缺少 team identity 的 context ID。

`canonicalInvariantContent` 格式固定，讓模型拿得到 logical ID、policy severity 與 statement，
並讓 pre-call manifest 與 post-call attestation 綁定同一個 content hash：

```text
Invariant ID: <definition.ID>
Severity: <definition.Severity>
Applies to:
- <sorted applies-to entry>
Statement:
<normalized statement>
```

compiler-local `ContextItem` 新增 `InvariantSeverity InvariantSeverity` 與
`InvariantContentHash string`。對 `ContextInvariant`，adapter 設
`DedupKey=record.ID+"\x00"+record.ContentHash`，分別保留 content hash 與 policy severity，
指定 `PriorityHardConstraints`、source=`repository_invariant`，並設為 normative、required、
不可壓縮。Dedup identity 必須包含 logical ID；兩個文字相同但 ID 不同的 invariant 不能互相
消除，也不能因一般 historical-memory ranking、dedup 或 compaction 遺失 identity。

`ContextInvariant ContextKind = "invariant"` 加在 `internal/context/model.go`。這個新增不
改 `memoryCategoryKind`；agent-authored memory 仍不允許 invariant category。

## 4. Static verifier task contract

`TaskDef` 新增 configuration-only 欄位：

```go
type InvariantVerificationMode string

const (
    InvariantVerificationReport InvariantVerificationMode = "report"
    InvariantVerificationGate   InvariantVerificationMode = "gate"
)

InvariantVerification InvariantVerificationMode `json:"-" yaml:"invariant-verification,omitempty"`
```

`TodoItem`/`TodoSpec` 新增可持久化的
`InvariantVerification InvariantVerificationMode`。contract compile/admission 將靜態值
freeze 到 occurrence，`taskDefFromTodoItem` 與 occurrence comparison 必須包含它。
coordinator 的 `agent` tool JSON 不得暴露這個欄位。

例：

```yaml
tasks:
  - id: review-workset
    agent: reviewer
    phase: verify
    invariant-verification: report
    execution:
      requires-result: true
      requires-grounded-result: true
```

Preflight hard errors：

- mode 不是空、`report`、`gate`；
- 設定 report/gate 但 catalog 不存在；
- workflow enabled 時 phase 不是 `verify`；
- `execution.requires-result` 不是 true；
- task 是 sidecar、summarize 或有 `model_topology`/extra-model execution；
- `gate` task 被標成 optional；
- static contract 無法 freeze 到 Todo occurrence。

未設定 mode 的既有 task 完全不需 assessment，行為不變。

Rollout safety：PR-1 到 PR-3 雖可解析/持久化 mode，但 production admission 與
`hufu team validate` 必須以固定 diagnostic `invariant_verification_feature_unavailable` 拒絕
任何 non-empty mode。PR-4 只有在 gate lifecycle、completion evaluator、audit dimension 與
cache bypass 全部接妥且測試通過後，才在同一 PR 移除此暫時 guard；不得發布「能接受 gate
設定但不執行 gate」的中間狀態。

## 5. Applicability 與 context routing

### 5.1 Touched paths

`WorksetItem` 與 `WorksetBinding` 新增：

```go
TouchedPaths []string `json:"touched_paths,omitempty" yaml:"touched-paths,omitempty"`
```

`reviewprep` 將現有 item `paths` 改成 `touched_paths` 並沿用 §3.2 的 relative-path
normalizer（不允許 `*`）。workset expansion 必須把 paths freeze 到 child TodoItem 的
binding；原始 workset artifact 的 `SourceSHA256` 綁定完整 manifest，既有 item-key digest
維持只 hash keys。event/session replay 必須保留這個欄位。

`newTaskContextRequest` 對 invariant-verifier task 將 binding 的 `TouchedPaths` 複製到
`ContextRequest.TouchedPaths`。沒有 workset binding 時保持空；空值採 fail-safe 行為，
視為所有 invariant 都 applicable，而不是零筆。

### 5.2 Selection

只有 `InvariantVerification` 為 report/gate 的 task 才選 invariant：

- `TouchedPaths` 空：catalog 全部 applicable；
- 非空：選 `applies-to: ["*"]` 或至少一個 exact/prefix 規則匹配的 definition；
- selection 依 invariant ID 排序，必須 deterministic；
- selected invariant 全部是 required context，不參與一般 memory ranking threshold 或
  top-k；
- `CanonicalContextBundle` 新增 `RepositoryInvariants []context.ContextItem`；router 將
  selected catalog items 放入此欄位，不混入 `SharedPersistent`；compiler adapter 使用
  source=`repository_invariant`；
- 仍由 `ContextRouter.Route` 加入 canonical bundle，再走既有
  `ContextCompiler`、token admission、manifest persistence；不得直接串 prompt；
- required invariants 超過 context budget 時，在 model call 前失敗，不得靜默省略。

`coordinatorContextRouter.Route` 必須先處理 `TeamSession.InvariantCatalog`，再判斷
`contextRepo` 是否存在；catalog 不依賴 SQLite，不能被現有 `contextRepo == nil` fast return
略過。ordinary task 或空 catalog 保留原 fast path。

每一筆 selected/omitted invariant 都產生 `ContextRouteDecision`。新增 reason：
`ContextOmittedNotApplicable = "not_applicable"`。

為避免未進 compiler 的 not-applicable item 被現有 manifest builder fallback 誤標成
`canonical_memory`，`ContextRouteDecision` 增加下列 optional、content-free attribution：

```go
Kind              string            `json:"kind,omitempty"`
Source            string            `json:"source,omitempty"`
ContentHash       string            `json:"content_hash,omitempty"`
InvariantSeverity InvariantSeverity `json:"invariant_severity,omitempty"`
```

Invariant decision 四欄必填；既有 memory decision 可保持空值。`BuildContextInjectionManifest`
對沒有對應 compiled item 的 decision 使用這四欄，不得再硬編碼 invariant 為
`canonical_memory`。included/omitted item 都因此保有相同 logical ID、kind、source、hash 與
severity，且納入 manifest fingerprint。

`ContextManifestItem` 新增：

```go
ContentHash       string            `json:"content_hash,omitempty"`
InvariantSeverity InvariantSeverity `json:"invariant_severity,omitempty"`
```

非 invariant item 兩欄都留空。
`BuildContextInjectionManifest` 只在 `Kind==invariant` 時，從 compiler item 的
`InvariantContentHash` 複製並驗證 64-char lowercase SHA-256 hash，並從已授權的 repository
invariant item 複製 policy severity；其他 context kind 不把既有 dedup key 寫入
ContentHash。manifest 仍是 content-free。
`ContextManifestSchemaVersion` 由 1 升到 2，新建 manifest 使用 v2，replay 仍接受 v1。
verifier task 只接受 v2 manifest，ordinary legacy task 不受影響。included invariant 必須帶
deterministic context ID、kind `invariant`、source、content hash、policy severity 與
fingerprint attribution。

## 6. Model claim 與 runtime attestation

### 6.1 Finding severity

現有 `Finding` 只新增一個 presentation 欄位：

```go
Severity string `json:"severity,omitempty"`
```

合法值重用 `FindingSeverityError/Warning/Info`。空字串視為 info，保留舊資料相容性；
非空未知值在 local submit 與 external proposal decode/canonicalize 時均拒絕。

Finding severity 不直接進 completion gate。是否阻擋由 invariant definition 的 severity
決定，避免模型自行升級 policy。

### 6.2 Public claim

新增 model-owned DTO：

```go
type InvariantAssessmentStatus string

const (
    InvariantPreserved InvariantAssessmentStatus = "preserved"
    InvariantViolated  InvariantAssessmentStatus = "violated"
    InvariantUnknown   InvariantAssessmentStatus = "unknown"
)

type InvariantAssessmentClaim struct {
    InvariantID    string                    `json:"invariant_id"`
    Status         InvariantAssessmentStatus `json:"status"`
    Summary        string                    `json:"summary"`
    FindingIndex   *int                      `json:"finding_index,omitempty"`
    MissingEvidence []string                 `json:"missing_evidence,omitempty"`
}
```

`SubmitResultInput` 與 `WorkerResultProposal` 新增
`InvariantAssessments *[]InvariantAssessmentClaim`。使用 pointer 是為了區分 ordinary task
的 absent/null 與 verifier task 明確提交的空 array。最多 128 筆；summary trim 後
1–1000 runes。Codex strict schema 因所有 property 必須 required，將欄位列為 required 且
type 為 `array|null`；非 verifier task 回傳 null。`missing_evidence` 最多 16 筆，每筆 trim 後
1–256 runes。

Local `submit_result` schema 永遠描述此欄位，但只有 verifier task 將它加入 ToolInfo 的
required list。External path 把 decoded claims 暫存在 `AttemptResult` 新增的 package-private
`invariantClaims *[]InvariantAssessmentClaim`；不得先塞進 canonical TaskResult 或持久化。
兩套 schema 的 claim item 都設 `additionalProperties:false`；Codex 版本將
`invariant_id/status/summary/finding_index/missing_evidence` 全列 required，`finding_index` 為
`integer|null` 且 minimum 0，`missing_evidence` 為 `array|null`。Codex finding 的新
`severity` property 同樣 required 且為 `error|warning|info|null`。

Claim validation：

- verifier task 必須提交 non-null array，並對每個實際 included invariant 恰好提交一筆，
  不能多、不能少、不能 duplicate；沒有 applicable invariant 時必須明確提交 `[]`；
- 非 verifier task 只能省略或提交 null；即使是空 array 也視為 protocol error；
- ID 必須對應同一 task、attempt、model execution 的 persisted context manifest 中
  `Included=true` 且 `Kind=invariant` 的 item；
- status 只能是 preserved/violated/unknown；
- violated 必須提供有效的 zero-based `finding_index`；index 針對 bounds/redaction 後即將
  持久化的 canonical `TaskResult.Findings`，該 finding severity 必須是 definition severity
  （error/warning/info）；
- preserved/unknown 不得提供 finding index；
- unknown 必須提供 1–16 筆 `missing_evidence`；preserved/violated 只能省略、null 或空 array；
- claim 或 manifest 驗證失敗走既有 protocol-repair/retry，不得建立 attestation，也不得
  transition 到 Done。

### 6.3 Runtime-owned attestation

`TaskResult` 新增 runtime-only canonical output：

```go
type InvariantAssessment struct {
    InvariantID      string                    `json:"invariant_id"`
    ContextItemID    string                    `json:"context_item_id"`
    InvariantContentHash string                `json:"invariant_content_hash"`
    Severity         InvariantSeverity         `json:"severity"`
    Status           InvariantAssessmentStatus `json:"status"`
    Summary          string                    `json:"summary"`
    FindingIndex     *int                      `json:"finding_index,omitempty"`
    MissingEvidence  []string                  `json:"missing_evidence,omitempty"`
}

type InvariantVerificationResult struct {
    ContextManifestFingerprint string                `json:"context_manifest_fingerprint"`
    Assessments                []InvariantAssessment `json:"assessments"`
}

InvariantVerification *InvariantVerificationResult `json:"invariant_verification,omitempty"`
```

Canonical `TaskResult.InvariantVerification` envelope 與其中的 attestation 型別不出現在任何
model-owned DTO；`SubmitResultInput`/`WorkerResultProposal` 的 `invariant_assessments` field 只能
解成 claim DTO，若塞入 hash、context item ID、severity 或 manifest fingerprint 會因 strict
decode 失敗。attestor
從 `TeamSession.InvariantCatalog`、persisted context manifest 與 claims 建立 snapshot；claim
使用 manifest 的 logical definition ID，attestor 以
`invariant:<normalized-team>:<definition-id>` 對應 `ContextItemID`。它要求 catalog canonical
content hash/severity 等於 manifest item 的 content hash/severity，按 logical invariant ID
排序，再寫入 canonical TaskResult。即使 applicable set 是空集合，仍必須建立 non-nil
envelope、保存該次 manifest fingerprint 並令 `Assessments` 為 non-nil `[]`；如此 completion
才能證明空結果屬於本次 model call。statement/applies-to 不複製進每個 task result，避免
fan-out 重複放大；離線 audit 使用 pre-call manifest 已封存的 hash/severity。

共用接點固定為：

```go
func (c *Coordinator) attestInvariantClaims(
    todoID string,
    attempt int,
    modelExecutionID string,
    claims *[]InvariantAssessmentClaim,
    result *TaskResult,
) error
```

函式只能從 durable Todo snapshot 取得 mode/context manifests；傳入的 attempt 與
modelExecutionID 只用來選出唯一 manifest，選不到或選到多筆都 fail closed。

執行接點：

1. local `submit_result`：runtime identity/receipt claim validation 後、
   `beginTaskResultSubmission` 前；
2. external provider：`ExternalResultCanonicalizer` 建立 canonical result，並把 proposal
   claims 留在 transient `AttemptResult.invariantClaims`（同樣是 pointer-to-slice）；
   `coordinator_task_run.go` 必須在
   `storeSubmittedTaskResult` 前呼叫同一個 attestor，失敗時把 attempt 分類為 protocol
   failure；
3. protocol-repair 使用原 attempt 已持久化的 context manifest；result-only/schema-only repair
   prompt 必須列出 manifest 要求的 exact invariant IDs，並只在 current catalog 的 canonical
   content hash/severity 與 manifest 相同時附上 bounded invariant content。若 catalog 已變更，
   禁止修補舊 claim，改讓該 attempt 以 protocol failure 結束並由一般 retry 建立 fresh
   context manifest；repair 仍只允許 `submit_result`，不得重新執行工作；
4. direct、sidecar、coordinator、ordinary worker 在 mode 空值時走 no-op；
5. crash-resume 從 TodoItem/context manifests 重建，不能讀 transient coordinator fields。

若 attestation 尚未成功，TaskResult 不得進 task cache、journal、working memory 或 Done
transition。

### 6.4 Execution-path matrix

| Path | Contract |
|---|---|
| hufu-local static verifier | local claim schema；attest before result transaction |
| Codex/external static verifier | transient claims；attest before `storeSubmittedTaskResult` |
| protocol/schema repair | reuse same attempt's persisted v2 context manifest；不得執行工作 |
| ordinary local/external worker | mode empty；claims 必須 absent/null；attestor no-op |
| direct-agent / fast path | 沒有 static occurrence contract，不能啟用 report/gate |
| coordinator/sidecar/judge/skeptic | 不接受 invariant assessment；維持現有 context policy |
| extra-model verifier | preflight hard error；第一版不允許 |
| runtime ActionProvider | 只可產生 workset/touched paths，不能產生 assessment/attestation |
| dry-run / team validate | 載入並驗證 manifest/static contract；不執行 workset route 或 model |
| unattended | 與 interactive 同一 gate；不能因無 TTY 降級為 report |
| crash-resume / branch checkout | 從 event-reduced Todo、manifest、TaskResult 重算；舊 run gate 依 §6.6 refresh |

本功能不新增 tool grant、shell/network 例外或 side-effect recovery 路徑。

### 6.5 Gate verdict 的 task lifecycle

新增 pure helper：

```go
func HasBlockingInvariantAssessment(
    mode InvariantVerificationMode,
    verification *InvariantVerificationResult,
) bool
```

它只讀 runtime-attested `TaskResult.InvariantVerification`。在 local/external 共用的
post-attempt success path，完成
attestation 與 `validateCompletedTaskResult` 後、objective `VerifySpec` 與 `TaskDone` transition
前套用：

- report mode 不因 invariant assessment 改寫 task outcome；若 worker 的 task status 本身是
  partial/failed/blocked，仍走既有失敗語義；若 review 已完成並提交 success，
  violation/unknown 只留在 finding、assessment 與報告；
- gate mode 的 error-severity violated/unknown 是完整、有效、但語意不接受的 handoff；
  runtime 將 canonical `TypedResult` 同時放進 failure receipt 與 canonical task error transition，
  確保 event-reduced `TodoItem.TypedResult` 保留 attestation；不得只留 process-local map，也不得
  進 Done 或 task cache；
- 這個 error 的 raw persisted class 沿用 `execution`，但
  `effectiveFailureClassForTodo` 新增 `isRuntimeAttestedInvariantRejection` 判斷，將它 canonicalize
  成既有 `FailureSemanticRejection`；不得靠解析 error string；
- 既有 per-task retry、`on-failure-classes: [semantic_rejection]`、ancestor reset、failure-signature
  anti-thrashing 與 max-retries 負責修復；重跑 verifier 使用新的 attempt/context manifest，成功
  preserved 後才可 Done；
- warning/info assessment 不改 task outcome；
- `no-redispatch-after-success` 不阻擋此流程，因 gate rejection 從未成為 successful terminal
  execution；需以 regression test 固定此行為。

`EvaluateSemanticRegression` 仍是 run completion 與 offline audit 的 defense-in-depth：若任何
路徑錯誤地留下 Done+blocking assessment、未完成 gate task，或只有舊 run 的 result，最終
completion 必須 fail closed。

### 6.6 新 invocation 與 crash-resume

`executionRunID` 每次 top-level invocation 都會更新，而現有 resume 會保留 Done task。為免
「拒絕舊 attestation、卻又永遠不重跑 verifier」的死結，新增：

```go
func (c *Coordinator) RefreshStaleGateVerifiers(ctx context.Context) (int, error)
```

它在 `beginExecutionRun`/event-store/task-journal 初始化後、`ResumeInterruptedTasks` 前執行：

1. 只處理受信任 static occurrence 中 mode=gate 的 Todo；report/ordinary task 不動；
2. Pending/Planned/InProgress/Paused 交給既有 resume，不先重設；
3. 對 Done/Error/Blocked/Skipped task，若 current verification envelope 找不到 manifest、或該
   manifest RunID 不等於新的 `executionRunID`，以新增的 canonical reset reason
   `stale_gate_verification` 將同一 Todo ID 重設為 Pending；
4. reset 清除 current TypedResult、failure/resolution/receipt 與 process-local result latch，但
   保留歷史 ContextManifests/event lineage；不得把 task 標成新 failure，也不得消耗
   `Retries`/max-retries；新 attempt 由新 RunID 提供 occurrence identity；
5. refresh 產生 durable task transition/checkpoint，失敗則 public invocation fail closed；
6. 隨後一次 `ResumeInterruptedTasks` 依既有 dependency/ID order 重跑 pending verifier，且發生
   在 coordinator 可提交新 delegation 前；reset 後 current projection 不再是 successful
   terminal item，因此 `no-redispatch-after-success` 不得拒絕這個 runtime-owned refresh；
7. 同一 invocation 內的 gate rejection 不呼叫 refresh，而走 §6.5 的一般 retry/on-failure。

如此 crash 後未完成工作與 stale completed gate 都能在同一 startup recovery wave 處理；
completion/audit 仍只接受 current executionRunID 的 envelope。

## 7. Resolution 與 completion semantics

### 7.1 Pure evaluator

新增 pure function，放在 `internal/team/invariant_completion.go`：

```go
type SemanticRegressionDecision struct {
    Configured    bool
    Clear         bool
    BlockingCount int
    Reasons       []string
}

func EvaluateSemanticRegression(runID string, tasks []*TodoItem) SemanticRegressionDecision

type InvariantVerificationValidation struct {
    Valid       bool
    Code        string
    Assessments []InvariantAssessment
}

func ValidateInvariantVerificationResult(
    todo *TodoItem,
    runID string,
) InvariantVerificationValidation
```

validation 回傳的是依 invariant ID 排序的 deep copy；成功時 `Valid=true`、`Code=""`，失敗
時 `Valid=false`、`Assessments=nil`。validation 不檢查 Todo status，因為有效的 gate rejection
預期會是 Error；它的 Code 只能是
`typed_result_missing|envelope_missing|manifest_missing|manifest_ambiguous|manifest_version|`
`run_mismatch|identity_mismatch|assessment_set_mismatch|attestation_invalid`，不能把任意 error
文本帶進 completion/audit output。

規則：

1. 只掃 terminal projection 中 `InvariantVerification == gate` 的 current TodoItem；
2. 沒有 gate task：`Configured=false, Clear=true`；
   有 gate task 但 runID 為空：`Configured=true, Clear=false`；
3. 對每個 gate task 呼叫 pure `ValidateInvariantVerificationResult(todo, runID)`：要求
   TypedResult 與 non-nil verification envelope、由 envelope fingerprint 唯一找到 v2 context
   manifest、manifest 的 run/task/attempt/model identity 正確，且 assessment 恰好覆蓋該
   manifest 中全部 included invariant；空集合也必須是已綁 fingerprint 的明確 `[]`。任何
   structural/identity 錯誤產生一個 blocker 並停止評估該 task；上一個 run 的
   cached/completed verifier result 不得替本次 run 放行；
4. valid set 中 severity=error 且 status=violated 或 unknown：每筆各產生一個 blocker；若
   沒有 semantic blocker，task 仍必須為 Done，否則產生一個 structural blocker；gate
   semantic rejection 預期為非 Done，但不再額外重複計數；
5. warning/info 永不阻擋，但保留在報告；
6. report-mode task 完全不參與 Clear；
7. reasons 依 task ID、invariant ID 排序。semantic reason 格式固定為
   `task <task-id> invariant <id> is <status>: <summary>`；structural reason 固定為
   `task <task-id> invariant verification invalid: <bounded-code>`；除上述 validation codes 外，
   evaluator 唯一可新增的 code 是 `task_not_done`；
8. evaluator 計算全部 blockers 到 `BlockingCount`，但 `Reasons` 最多 50 筆、每筆 summary
   截為 256 runes；超過時保留排序後前 49 筆，第 50 筆固定為
   `and <N> additional invariant blockers`；
9. 不掃 raw `Finding`，不讀 coordinator 暫存值，也不查 live context.sqlite。

同一 verifier Todo 重跑後，current `TypedResult` 取代舊值；舊 attempt 仍在 event store，
但不再 unresolved。不同 gate tasks 的 violated assessment 彼此不能互相覆蓋；每一個都要
在自己的 current result 變成 preserved 才解除。

### 7.2 CompletionGate

`CompletionGateInput` 新增：

```go
SemanticRegression SemanticRegressionDecision
```

`applyCompletionGate` 呼叫 `EvaluateSemanticRegression(c.executionRunID, items)`。只有
`Configured && !Clear` 才逐項 reject。沿用現有 downgrade：completed claim 轉 partial、
`GoalSatisfied=false`、exit 7；不改 failed/cancelled/partial/unverified 的既有 outcome。

這個規則不依賴 `GoalMode`。適用性完全由受信任的靜態 task contract 決定：

- code-review/report task 發現 error invariant violation，review run 仍可 completed；
- coding/gate verifier 發現同一 violation，run 不得 completed；
- 沒有 manifest 或 contract 的舊 team 零行為改變。

## 8. Persistence、replay 與 projection

不新增 event type 或 SQLite migration。以下欄位都透過既有 task transition payload 的
`TypedResult`/TodoItem JSON 持久化：

- `TodoItem.InvariantVerification`；
- `WorksetBinding.TouchedPaths`；
- `Finding.Severity`；
- runtime-owned `TaskResult.InvariantVerification` envelope；
- existing `ContextInjectionManifest` attribution。

必須更新所有 clone/boundary：

- `cloneTaskResult` 深拷貝 verification envelope、assessment/`MissingEvidence` slices 與
  `FindingIndex` pointer；
- Todo/workset/context manifest clone；
- event reducer、session checkpoint、task journal 與 branch checkout tests；
- retry/remediation context、`FormatForContext`、worker STM 與 `team_info task_result`；
- external proposal bounding/redaction；
- report/gate verifier task 一律 bypass task-cache lookup、similarity duplicate suppression 與
  cache store；semantic verdict 必須綁定本次 run 的 pre-call manifest，不能重用 output-only
  cache entry。

舊 JSON 缺少新欄位時 mode 空、severity info、assessment 空；不得推測或回填 violation。

## 9. auditverify

### 9.1 Dimension contract

`AuditVerificationResult` 新增 mandatory dimension：

```go
SemanticRegression AuditDimensionResult `json:"semantic_regression"`
```

`AuditSchemaVersion` 由 1 升到 2，`mandatoryDimensions()` 加入
SemanticRegression。`CodeInvariantViolated = "AUDIT-INVARIANT-VIOLATED"` 與
`CodeInvariantAttestationInvalid = "AUDIT-INVARIANT-ATTESTATION-INVALID"`、
`CodeInvariantWitnessMismatch = "AUDIT-INVARIANT-WITNESS-MISMATCH"`。

正常 lineage path 在 Acceptance 後、Completion 前：

1. 從 `team.ReduceToSessionData` 得到 terminal tasks；
2. 呼叫 `team.EvaluateSemanticRegression(runID, session.Tasks)`；
3. 呼叫同一個 `ValidateInvariantVerificationResult` 重算 envelope 指向的 context manifest
   fingerprint，將 persisted attestation 的 content hash/severity 比對 manifest item，再驗證
   included invariant ID、run/task/attempt/model identity；
4. 沒有 gate contract => dimension Pass，reason=`semantic regression gate not configured`；
5. clear => Pass；violation/unknown/invalid => Fail 並加入 audit finding；
6. `DeriveCompletionAudit` 新增 `SemanticRegressionClear bool`，completed claim 必須為 true。

Audit 不讀 live `invariants.yaml` 或 `context.sqlite`。event hash chain、model call 前持久化的
content-free context manifest 與 TaskResult 內的 immutable assessment identity 是離線驗證
資料。

### 9.2 Early-return matrix

每個 audit result constructor/early return 都必須顯式設定 SemanticRegression，包括
`runWorkspaceAudit` 的 chain failure、`runLineageAudit`、bundle import failure 與
`failResultf`：

| 情況 | SemanticRegression |
|---|---|
| event chain broken / terminal conflict | skipped：integrity unavailable |
| terminal event missing | skipped：no terminal projection |
| terminal reduction mismatch | fail：canonical projection invalid |
| normal legacy run, no gate task | pass：not configured |
| attestation malformed | fail |

### 9.3 Witness 與輸出

`GateWitness` 新增 `SemanticRegressionConfigured bool`、`SemanticRegressionClear bool`、
`SemanticRegressionBlockingCount int` 及 bounded/sorted
`SemanticRegressionReasons []string`；`WitnessSchemaVersion` 升到 2。Bundle schema 不升版，
因為沒有新增 archive role，events/run-result/witness 已包含全部證據。

`verifyWitnessLinkage` 除既有 hash/event-head/evidence linkage 外，v2 必須逐欄比對 replay
重算的 semantic decision；不一致以 `AUDIT-INVARIANT-WITNESS-MISMATCH` 令 Integrity fail。
Import 仍接受 v1 witness，但只有 replay projection 沒有 gate task 時才能解讀為 feature
not configured；若 events 已有 gate contract 而 witness 仍是 v1，同樣視為 witness mismatch，
不能用 schema downgrade 隱藏 gate decision。

同步更新：

- `cmd/hufu/auditcmd.go` text/JSON；
- `cmd/hufu/report.go` audit 與 task assessment sections；
- `internal/inspect/evidence.go` 的 semantic regression status；
- audit integration fixture map、witness hash tests、bundle round-trip tests。

## 10. Review team integration

新增 `.agent-teams/hufu-code-review/invariants.yaml`，第一筆使用已存在的 runtime contract：

```yaml
schema-version: 1
invariants:
  - id: sqlite-canonical-memory
    statement: "context.sqlite is canonical; context-stm.md and context-ltm.md are disposable projections and never runtime inputs"
    severity: error
    applies-to:
      - internal/context/
      - internal/team/shared_memory.go
      - internal/team/worker_memory.go
```

`review-workset` 設 `invariant-verification: report`，不得設 gate。reviewer prompt 要求逐筆
回報 injected invariant 的 assessment；critic prompt 只驗證 coordinator 指派的 finding，
不自行建立或解除 gate assessment。

此整合證明「發現被審查程式違反 invariant」與「review 工作成功完成」可以同時成立。

## 11. 完整檔案範圍

實作前允許依實際 symbol 位置微調，但不得省略對應 consumer。

新增：

```text
internal/team/invariant.go
internal/team/invariant_manifest.go
internal/team/invariant_attestation.go
internal/team/invariant_completion.go
internal/team/invariant_manifest_test.go
internal/team/invariant_attestation_test.go
internal/team/invariant_completion_test.go
.agent-teams/hufu-code-review/invariants.yaml
```

修改 model/config/admission：

```text
internal/context/model.go
internal/team/parse.go
internal/team/coordinator.go
internal/team/status.go
internal/team/contract_compile.go
internal/team/task_occurrence_projection.go
internal/team/team_lint.go
internal/team/coordinator_extra_models.go
```

修改 context/workset：

```text
internal/team/workset.go
internal/team/workset_fanout.go
internal/team/context_request.go
internal/team/context_request_runtime.go
internal/team/context_router.go
internal/team/context_compiler.go
internal/team/context_manifest.go
.agent-teams/hufu-code-review/reviewprep/main.go
```

修改 result/trust boundaries：

```text
internal/team/task_result.go
internal/team/task_occurrence.go
internal/team/coordinator_tools_result.go
internal/team/subagent_external_result.go
internal/team/subagent_provider.go
internal/team/codex_result_schema.go
internal/team/subagent_codex.go
internal/team/coordinator_task_run.go
internal/team/coordinator_run.go
internal/team/coordinator_session.go
internal/team/remediation_context.go
internal/team/worker_memory.go
internal/team/coordinator_tool_teaminfo.go
internal/team/coordinator_taskcache.go
internal/team/delegation_policy.go
internal/team/dag_scheduler.go
internal/team/services.go
```

修改 completion/audit/projections：

```text
internal/team/completion_gate.go
internal/team/run_finalizer.go
internal/auditverify/model.go
internal/auditverify/verifier.go
internal/auditverify/witness.go
internal/auditverify/import.go
internal/inspect/evidence.go
cmd/hufu/auditcmd.go
cmd/hufu/report.go
cmd/hufu/json_output.go
```

修改 team：

```text
.agent-teams/hufu-code-review/team.yaml
.agent-teams/hufu-code-review/reviewer.md
.agent-teams/hufu-code-review/critic.md
```

沒有新增 StatusEvent/tea.Msg 或 overlay，因此 TUI 不需新增 message；但 JSON/report 所含
Todo typed-result snapshot 必須有 regression tests。

## 12. PR 順序

### PR-1 — Catalog + static contract（無 completion 行為）

- manifest model/loader/validator；
- ContextInvariant；
- TaskDef/TodoItem contract freeze 與 preflight；
- non-empty mode 暫時以 `invariant_verification_feature_unavailable` hard-disable；
- team validate diagnostics；
- legacy parse/replay tests。

### PR-2 — Routing + workset attribution（仍無 completion 行為）

- touched paths freeze；
- ContextRouter deterministic applicability；
- compiler required-budget behavior；
- persisted context manifest and replay tests。

### PR-3 — Claim + attestation boundary（仍無 completion 行為）

- finding severity；
- local/external/Codex schemas；
- common attestor wired before every result persistence path；
- clone/redaction/retry/remediation/cache tests。

### PR-4 — Completion + audit

- gate verdict 接入 common post-attempt lifecycle，並映射既有 `semantic_rejection` 修復流程；
- startup stale-gate refresh 與 crash-resume ordering；
- shared pure evaluator；
- CompletionGate wiring；
- audit dimension/schema/witness；
- 所有 consumer 與 cache bypass 驗證完成後移除 temporary feature-unavailable guard；
- CLI/report/inspect projections；
- crash-resume、branch replay、bundle round-trip tests。

### PR-5 — hufu-code-review report-mode E2E

- example invariant；
- review-workset report contract；
- prompts；
- fake-provider E2E proving violation reported while review run completes。

PR 必須依序合併；PR-3 未完成前不得平行實作 PR-4，避免 completion 消費尚未 attested 的
model claims。

## 13. 必要測試矩陣

### Manifest/config

- absent manifest leaves legacy behavior unchanged；
- valid load/materialization is deterministic, including a nil context repository；
- duplicate ID、future schema、symlink、oversize、bad path/severity rejected；
- report/gate contract without catalog rejected；
- dynamic coordinator JSON cannot set invariant-verification；
- static contract hash and occurrence comparison include invariant-verification；
- optional gate and multi-model verifier rejected。
- PR-1..3 builds reject non-empty mode as feature unavailable；PR-4 build enables it only after all
  gate consumers are wired。

### Routing

- exact、directory-prefix、global applicability；
- empty touched paths selects all；
- unrelated invariant records not-applicable decision with invariant kind/hash/severity attribution；
- required invariant cannot be dropped by ranking/top-k；
- distinct invariant IDs with identical statement text both survive compiler dedup and require claims；
- required invariant context overflow fails before provider call；
- workset touched paths survive expansion, event replay and branch checkout；
- fan-out children inherit the parent static invariant-verification mode；
- context manifest v1 replay、v2 content hash 與 fingerprints are stable；

### Result boundaries

- legacy empty Finding severity => info；unknown severity rejected；
- local and external schemas accept bounded claims；Codex nullable field round-trip；
- external claims remain transient until coordinator attestation；
- zero applicable invariants still produce a non-nil verification envelope bound to the current
  manifest with a non-nil empty assessment array；
- unknown/not-injected/duplicate/missing invariant claim rejected；
- violated claim requires valid finding index and matching severity；
- unknown requires bounded structured missing evidence；preserved/violated reject non-empty missing
  evidence；
- model-supplied runtime attestation field rejected by strict schema；
- local、Codex external、protocol repair all attest before persistence；
- protocol repair receives the exact manifest invariant IDs；catalog/hash drift forces a fresh
  execution attempt instead of repairing against changed policy；
- failed attestation never reaches cache/journal/Done；
- a successful report-mode handoff with blocking assessment remains Done；an independently
  partial/failed/blocked report keeps existing failure semantics；gate-mode blocking assessment
  persists typed failure evidence, remains non-Done and canonicalizes to `semantic_rejection`；
- gate semantic rejection follows existing retry/on-failure/anti-thrashing limits；
- no-redispatch-after-success does not suppress a rejected gate verifier retry；
- startup refresh reruns terminal gate verifiers from a prior executionRunID without consuming retry
  budget, while preserving their historical manifests/events；
- report/gate verifier always bypasses exact/semantic task cache and cross-run pinned results；
- invariant verifier cannot use free-text-result promotion；
- clone is mutation-safe and `go test -race` focused tests pass。

### Completion/lifecycle

- report violation does not downgrade completed review；
- gate error violation and error unknown downgrade completed to partial；
- warning/info never block；
- raw Finding without attestation never blocks；
- a malformed Done+blocking gate result is rejected by the final completion evaluator；
- rerunning same verifier Todo with preserved current result resolves prior violation；
- violation from a different current gate Todo remains blocking；
- crash-resume refresh orders stale gate reruns after dependencies and before new coordinator
  delegation；refresh transition failure aborts the invocation；
- legacy event replay remains clear/not configured。

### Audit/projections

- audit recomputes from replayed tasks, not coordinator decision；
- tampered content hash/severity/manifest fingerprint fails；
- prior-run verifier result cannot satisfy the audited run；
- every early-return branch sets semantic dimension；
- completed claim requires semantic clear；
- v1 witness/import remains accepted only when replay has no gate task；v1 plus gate events fails as
  witness mismatch；
- v2 witness semantic fields must equal the replayed decision；
- v2 witness seal/verify and audit bundle round trip；
- CLI text/JSON、report、inspect include the new dimension；
- fake-provider E2E covers clean, report violation, gate violation and repaired gate。

## 14. Validation gate

每個 code PR 都必須個別執行並通過：

```bash
go test ./...
go vet ./...
golangci-lint run
```

涉及 clone/concurrent result paths 的 PR-3、PR-4 另外執行相關 package 的 `go test -race`。
E2E 使用 fake provider/fixtures，不依賴 live LLM、外部 checkout 或 network。

## 15. 完成定義

只有同時滿足以下條件才可宣稱完成：

1. 只有受信任 static contract 能啟用 semantic gate；
2. model 無法自行建立 invariant、attestation 或 gate applicability；
3. 每個 gate decision 可由 terminal event lineage 離線重算；
4. report finding 不會使 review run 失敗；
5. gate violation/unknown 無法被 finding omission、重啟、cache、branch checkout 或舊
   projection 繞過；
6. legacy team/run/audit bundle 維持相容；
7. 全部 validation gate 通過。

可對外描述為：

```text
Repository-declared invariants are injected through Hufu's canonical context
pipeline. Models submit bounded assessment claims; Hufu attests those claims
against the exact injected context. Only explicitly configured gate tasks can
block completion, and auditverify can reproduce that decision from events.
```
