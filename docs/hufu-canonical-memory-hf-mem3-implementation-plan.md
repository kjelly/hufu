# Hufu Canonical Memory HF-MEM3 實作計畫

> 基準版本：`bc8aab6217a70d49a9172e18c66160dd828ba311`
>
> 文件性質：依目前 Hufu 程式碼整理的可執行實作計畫；整合 `s1.md` 與 `spec-i.md`，並以現況重新排序。
>
> 核心目標：讓 `workspace/context.sqlite` 成為 shared／private、session／persistent knowledge 的唯一 canonical source of truth；Event Store 繼續保存 execution truth，Markdown 與向量資料庫只作 projection／index。

## 1. 執行摘要

目前 Hufu 的 private worker memory 已完成大部分基礎建設：

- `ContextItem` 已有 `BranchID`、`LifecycleCandidate/Confirmed/Rejected`、`SupersededBy`、`ExpiresAt`、evidence 與 provenance。
- `scopeAuthorize()` 已統一 exact／ancestors／subtree 可見性；空 `AgentID` 不再是 private child 的 wildcard。
- `WorkerMemoryService` 已提供 `Recall`、`SaveSessionMemory`、`SaveCandidate`、`Confirm`、`RejectRun`。
- normal DAG worker 與 direct-agent 已接上 private recall／ingestion。
- `CompletionGate` 已用 run outcome、acceptance、sealed evidence manifest、required tasks、risk 與 terminal leak 決定 candidate promotion。
- `hufu context` 已有 `query`、`list`、`inspect`、`explain`、`repair`、`rebuild`，可作後續維護入口。

下一階段不重做上述能力，而是修正 shared memory 仍存在的 canonical 分叉：

```text
private worker memory
    └── context.sqlite                         canonical

shared persistent memory
    ├── logs/reflexion_candidates.jsonl       truth #1
    ├── logs/reflexion_confirmed.jsonl        truth #2
    └── ltm-TEAM.md                           truth #3 / prompt input
```

同時修復下列契約缺口：

1. `memory_save`／`memory_query` 公開 schema 與 canonical runtime 實際處理欄位不一致。
2. `stm_write` 將所有內容固定寫成 `ContextProgress`，`replace` 又沒有真正 replace。
3. STM／LTM projection 共用同一批 shared items，沒有依 session lifetime 隔離。
4. `AutoExtractCanonicalLTM` 會把 generic `ContextProgress` 轉成永久 pattern candidate。
5. 高分 tool pattern 仍會直接 `skill.PromoteDraft()`，繞過人工批准。
6. normal DAG worker 已使用 canonical compiled prompt，但 direct-agent 仍以 legacy prompt 為 model-visible input；coordinator 仍從 Markdown／chromem 組裝 historical memory。

本計畫先完成 canonical correctness，再切換 prompt read path，最後才做 procedural learning 與 legacy cleanup。

## 2. 已驗證的現況與 canonical owner

| 能力 | 現況 | Canonical owner | 本計畫處置 |
| --- | --- | --- | --- |
| Context data model | `internal/context/model.go` 已有 scope、lifecycle、evidence、supersession | `internal/context` | 沿用，不新增 `MemoryTier` DB column |
| Scope authorization | SQLite query、FTS、worker recall 已有明確 visibility | `internal/context` | 保留並補 mutation authorization tests |
| Private worker lifecycle | `WorkerMemoryService` 已完成 candidate／confirm／reject | `internal/team/worker_memory.go` | 不重寫；抽出可共用的 lifecycle policy |
| Shared LTM candidate | 寫入 reflexion JSONL | `internal/team/coordinator_reflexion.go` | 改寫為 canonical ContextItem |
| Shared STM | `appendCanonicalContext()` 寫 SQLite，但 kind／replace 語意不足 | `internal/team/context_shadow.go` | 改由 typed reducer 與 shared memory service 寫入 |
| Completion acceptance | `CompletionGate` 是唯一 run certification policy | `internal/team/completion_gate.go` | shared／private candidate 都由它 promotion／rejection |
| Projection | `RebuildProjection()` 對同一 items 同時 render STM/LTM | `internal/context/projection.go` | 依 scope lifetime 分查詢、分投影 |
| Memory tools | wrapper 沿用 legacy schema但忽略部分參數 | `internal/team/coordinator_tools_memory.go` | 先達成 literal parity，再 deprecated alias |
| Prompt context | DAG worker canonical compile；direct/coordinator 尚有 legacy source | `internal/team/context_compiler.go` 與 coordinator call sites | 分階段切為 canonical retrieval |
| Procedural learning | 高分 pattern 可直接 promote skill | `internal/team/coordinator_skill_patterns.go` | 永遠只自動產生 proposal/draft |
| User maintenance CLI | 已有 `hufu context` | `cmd/hufu/contextcmd.go` | 擴充現有 namespace，不建立 `hufu memory` |
| Vector memory | `internal/memory/MemoryStore` 仍保存另一份 records | `internal/memory` | 最終降為可重建 index，再移除 duplicate model |

## 3. 目標架構

```text
                              ┌────────────────────┐
                              │ Event Store        │
                              │ execution truth    │
                              └─────────┬──────────┘
                                        │
                          TaskResult / Verification / Receipt
                                        │
                  ┌─────────────────────┴─────────────────────┐
                  │                                           │
                  ▼                                           ▼
       Shared Working Memory                       Knowledge Extractor
       project/team/session                        evidence-aware candidates
                  │                                           │
                  └─────────────────────┬─────────────────────┘
                                        ▼
                             context.sqlite
                             canonical knowledge
                              │      │      │
                  ┌───────────┘      │      └────────────┐
                  ▼                  ▼                   ▼
             STM projection     LTM projection     retrieval indexes
             session scope      persistent scope    FTS / vector
                  │                  │                   │
                  └──────────────────┴─────────┬─────────┘
                                              ▼
                                      ContextCompiler
                                              │
                                              ▼
                                           Agent

confirmed persistent knowledge
              │
              ▼
       Experience Miner
              │
      Skill / Team proposal
              │
              ▼
       explicit human approval
```

邊界如下：

- Event Store、Todo、typed `TaskResult`、receipts、verification 與 acceptance 是執行事實。
- Context Store 是經選擇、可提供給 model 的知識；不得反向決定任務已完成或 verification 已通過。
- Markdown 是相容 projection，不可再讀回 SQLite 當成 canonical data。
- FTS／vector 是可重建 index；index failure 不得刪除 canonical item。
- Agent 可以提出 persistent knowledge candidate，但不能直接建立 confirmed persistent memory。
- Skill／Agent Team 是 procedural instruction；任何自動修改都必須經人工批准。

## 4. Data model 與 invariant

### 4.1 不新增 MemoryTier 欄位

不採用 `working/episodic/semantic` DB column。現有模型已可由 scope 表示 lifetime：

| 類型 | Scope | Lifecycle |
| --- | --- | --- |
| Shared session | `project/team/session` | confirmed 或 candidate |
| Shared persistent | `project/team`，`SessionID=""` | candidate → confirmed/rejected |
| Worker session | `project/team/session/branch/agent` | confirmed 或 candidate |
| Worker persistent | `project/team/agent` | candidate → confirmed/rejected |

`ContextKind` 表示內容語意；`ContextLifecycle` 表示是否可召回；`SupersededBy` 表示已被取代；`ExpiresAt` 表示有效期限。這四個概念不得混用。

現有 `metadata["memory_tier"]` 暫時保留作相容顯示與 audit，但 correctness 必須由 scope derive：

```go
func DeriveMemoryLifetime(item ContextItem) (session bool, persistent bool, err error)
```

一致性規則：

- persistent item 不得有 `SessionID` 或 `BranchID`。
- private item 必須有 `AgentID`；shared item 的 `AgentID/TaskID/AttemptID` 必須為空。
- session private item 必須同時有 `SessionID` 與 `BranchID`。
- candidate／rejected／superseded／expired item 不得進入一般 prompt retrieval 或 confirmed projection。
- source session、run、task、manifest hash 放在 `Source`、`Evidence` 或 metadata，不放進 persistent visibility scope。

### 4.2 Canonical mutation 必須 transactional

目前 `Append()` 與 `MarkSuperseded()` 是兩個 transaction。新增 repository-level operation：

```go
type AppendOptions struct {
    Supersedes []string
}

AppendWithOptions(ctx context.Context, item ContextItem, opts AppendOptions) error
```

同一 transaction 內完成：

1. 驗證新 item 與被取代 item 的 project/team、shared/private identity 相容。
2. append／dedupe 新 item。
3. 更新舊 item `superseded_by`。
4. 寫 `context_edges`。
5. 對 append 與 supersede 都寫 `context_events`。
6. commit 後才觸發 projection/index refresh。

禁止先寫新 item、失敗後留下未 supersede 的雙 truth。

### 4.3 Lifecycle mutation 必須 fail closed

`UpdateLifecycle(ids, lifecycle)` 保持 repository primitive；runtime caller 在呼叫前必須：

- 以 trusted run／scope 查出 candidate IDs；model 不可直接指定其他 scope IDs。
- 驗證 accepted `EvidenceManifest.RunID` 與 `ManifestHash`。
- 確認 candidate 的 run/task evidence 與 manifest 一致。
- promotion 任一必要 item 失敗時，不得把 run 留在 accepted 狀態。
- rejection 失敗要留下 observable error／pending repair，不可靜默忽略。

## 5. 執行工作包

### HF-MEM3-000 — Baseline 與 invariant regression tests

**Priority：P0；所有 production change 的前置工作。**

目標：把現況缺口變成 failing tests，避免 migration 期間修一條路、破壞另一條路。

修改範圍：

- `internal/context/*_test.go`
- `internal/team/coordinator_tools_memory_test.go` 或新增 contract test
- `internal/team/reflexion_test.go`
- `internal/team/worker_memory_*_test.go`
- `cmd/hufu/contextcmd_test.go`

先建立：

```text
TestSharedLTMUpdateCreatesCanonicalCandidate
TestAcceptedRunConfirmsSharedCandidate
TestRejectedRunRejectsSharedCandidate
TestPersistentMemoryHasNoSessionScope
TestCandidateNeverAppearsInRuntimeRecall
TestSupersededNeverAppearsInRuntimeRecall

TestSTMProjectionNeverContainsPersistentMemory
TestLTMProjectionNeverContainsSessionMemory
TestPrivateMemoryNeverAppearsInSharedProjection

TestMemorySavePreservesCategory
TestMemorySavePreservesConfidence
TestMemorySavePreservesFilePaths
TestMemorySaveHonorsSupersedesAtomically
TestMemoryQueryHonorsCategory
TestMemoryQueryHonorsMinConfidence
TestMemoryQueryFilePathBoostIsObservable

TestSTMWriteDoesNotMisclassifyDecisionAsProgress
TestSTMWriteReplaceCannotClaimFalseReplacement
TestProgressNeverAutoPromotesToPersistentPattern
TestSkillPatternNeverPromotesWithoutApproval
```

驗收：tests 在尚未修正的 branch 上能精準失敗；failure message 指向 contract，不依賴 live provider、Pilot checkout 或外部服務。

### HF-MEM3-001 — Canonical Shared Memory Lifecycle

**Priority：P0；第一個 production PR。**

目標：讓 `ltm_update`、shared `memory_save`、shared reflexion、AutoExtractLTM 全部建立 `context.sqlite` candidate，不再把 JSONL 當 truth。

新增 team-level policy service，storage 仍由 `internal/context.Repository` 負責：

```go
type SharedMemoryService interface {
    Propose(ctx context.Context, req SharedMemoryProposal) (ContextItem, error)
    ConfirmRun(ctx context.Context, req SharedMemoryPromotion) ([]ContextItem, error)
    RejectRun(ctx context.Context, req SharedMemoryRejection) ([]ContextItem, error)
}
```

`SharedMemoryProposal` 的 scope、run ID、task ID、source 與 evidence 都由 trusted runtime 注入；model 只能提供 content、kind、confidence、file refs 與可授權的 supersedes IDs。

實作步驟：

1. 在 `internal/team` 新增 shared memory service，沿用 `ContextRepository`，不要把 shared semantics 塞進 private-centric `WorkerMemoryService`。
2. persistent shared candidate 使用 `Scope{ProjectID, TeamID}`，明確清空 session／branch／agent／task／attempt。
3. candidate 寫入 `LifecycleCandidate`，metadata 至少保存 `run_id`、`task_id`、`source`、`legacy_section`；evidence 保存 task 與後續 manifest ref。
4. `persistKnowledgeCandidate()` 改為呼叫 service；暫時保留同名 helper 以縮小 call-site diff。
5. `CompletionGate` accepted path 依序確認 private 與 shared candidates；任一 promotion failure 將 `RunResult` 降為 partial/evidence incomplete。
6. rejected／partial／failed／cancelled run 對同一 `run_id` candidates 執行 `LifecycleRejected`。
7. 新增 lifecycle events：`shared_memory_candidate_saved`、`shared_memory_confirmed`、`shared_memory_rejected`；payload 只含 ID、scope identity、run/task、kind、manifest hash，不含秘密內容。
8. 更新 task journal／session checkpoint／report／JSON／TUI status consumer（若這些 events 對外可見）；不可只更新 coordinator log。
9. `reflexion_candidates.jsonl` 與 `reflexion_confirmed.jsonl` 改為 migration compatibility reader，停止新寫入。

主要檔案：

- `internal/team/coordinator_reflexion.go`
- `internal/team/completion_gate.go`
- `internal/team/coordinator_tools_memory.go`
- `internal/team/coordinator_memory.go`
- `internal/context/repository.go`
- `internal/context/sqlite_repository.go`
- lifecycle event／projection consumers

驗收：accepted manifest 只會 confirm 同 run 且 evidence-bound 的 candidates；failed run 不會留下可召回的 shared persistent knowledge；刪除 JSONL 後仍可由 SQLite 完整查詢 lifecycle。

### HF-MEM3-002 — Projection 依 scope lifetime 隔離

**Priority：P0；依賴 MEM3-001。**

目標：STM 與 LTM 不再吃同一批 records。

Repository API 拆分：

```go
QuerySharedSessionProjection(ctx, project, team, session)
QuerySharedPersistentProjection(ctx, project, team)
```

查詢規則：

- STM：exact `project/team/session` shared scope，只取 confirmed、current、non-superseded、non-expired。
- LTM：exact `project/team` 且 `SessionID IS NULL` shared scope，只取 confirmed、current、non-superseded、non-expired。
- private items、candidates、rejected、superseded、expired 永遠不進 shared projection。

實作步驟：

1. 移除 `QuerySharedProjection()` 對 `IncludeCandidates/IncludeSuperseded/IncludeExpired=true` 的 projection 用法；maintenance query 另保留明確 API。
2. `RebuildProjection()` 分別 query session／persistent records。
3. `RenderSTMMarkdown()` 與 `RenderLTMMarkdown()` 接受各自 items，不再靠 `legacy_section` 猜 lifetime。
4. canonical mode 下，由 SQLite 原子產生 `stm.md` 與 `ltm-TEAM.md`；`context-stm.md/context-ltm.md` 在 cutover 期間可保留作比對，內容必須與 legacy projection 等價。
5. projection failure 不回滾已 commit canonical data，但要寫 repair queue／observable event；repair 必須可重建 projection。
6. update `workspace_redact.go`、archive/report readers 與 projection snapshot tests。

主要檔案：

- `internal/context/repository.go`
- `internal/context/sqlite_repository.go`
- `internal/context/projection.go`
- `internal/team/context_shadow.go`
- `internal/team/stm.go`
- `internal/team/ltm.go`

驗收：任何 session item 都不出現在 LTM，任何 persistent item 都不出現在 STM；重建具冪等性，private／candidate leakage test 全部通過。

### HF-MEM3-003 — Memory Tool Contract Parity

**Priority：P0；可與 MEM3-002 平行，supersedes 最終依賴 MEM3-006。**

目標：公開給 model 的 tool schema 必須與 runtime literal 一致，不得靜默忽略欄位。

`memory_save` compatibility contract：

- `content`：required，trim 後不可空。
- `category`：映射為明確 `ContextKind`；未知值回傳 validation error，不默認吞掉。
- `confidence`：改用 `*float64` 分辨 omitted 與明確 `0`；範圍必須為 `[0,1]`。
- `supersedes`：只可取代 caller 可見、同 project/team 與相同 shared/private identity 的 records；使用 atomic API。
- `file_paths`：正規化為 workspace-relative evidence refs；拒絕 workspace escape，持久化前 secret redaction。
- `visibility`／`tier`：shared persistent 或 private session/persistent；runtime 從 caller context 決定 agent/run/task，model 不可指定 scope identity。

`memory_query` contract：

- 完整解析 `query`、`n`、`category`、`min_confidence`、`file_paths`。
- exact、lexical、vector retrieval 使用相同 scope/lifecycle/category/confidence filter。
- `file_paths` 是 ranking boost，不是隱性 hard filter；boost 必須出現在 retrieval trace/explain metadata。
- 回傳只含 confirmed、current、non-superseded records；maintenance candidates 必須走 `hufu context`，不可由 model 開啟。

需要擴充：

```go
type RepositoryQuery struct {
    ...
    Kinds         []ContextKind
    MinConfidence *float64
    FilePaths     []string
}

type SearchRequest struct {
    ...
    Kinds         []ContextKind
    MinConfidence *float64
    FilePaths     []string
}
```

若某欄位無法在所有 retrieval backend 保證一致，該欄位必須先從 tool `Info()` 移除並回傳 unsupported error；不可保留假 contract。

主要檔案：

- `internal/memory/tools.go`
- `internal/team/coordinator_tools_memory.go`
- `internal/context/model.go`
- `internal/context/retrieval.go`、SQLite／vector adapters
- tool schema parity tests

驗收：對每個公開參數都有 positive、invalid、authorization、persistence、retrieval test；schema 與 Run args 由同一 typed definition 生成或有 equality test 防止再次漂移。

### HF-MEM3-004 — TaskResult → Shared Working Memory Reducer

**Priority：P0。**

目標：以 authoritative typed result 自動產生 shared session memory，取代 coordinator 重新解讀 prose 後呼叫 `stm_write`。

新增：

```go
type WorkingMemoryReducer interface {
    ReduceTaskResult(ctx context.Context, input TaskResultMemoryInput) ([]ContextItem, error)
    RecordVerificationFailure(ctx context.Context, input VerificationFailureInput) error
}
```

映射：

| TaskResult / runtime state | ContextKind | Scope |
| --- | --- | --- |
| `Findings` | `ContextObservation` | shared session |
| `Decisions` | `ContextDecision` | shared session |
| `OpenQuestions` | `ContextOpenQuestion` | shared session |
| failed verification + diagnostic | `ContextError` | shared session |
| verified artifact/receipt ref | `ContextVerification` / `ContextArtifact` | shared session |
| generic summary/progress | `ContextProgress` | shared session；永不作 LTM extraction input |

規則：

- reducer 只在 canonical task transition 已保存 typed result 後執行。
- evidence 使用 artifact IDs、receipt IDs、verification refs；不可把任意 filesystem path 當 artifact ID。
- free-text recovered result confidence 較低，且沒有 objective verify 時保持 candidate／不 promotion。
- reducer write failure 不把成功 task 改成失敗，但必須寫 pending queue、event 與 repair evidence。
- crash-resume 重播同一 task transition 時必須 idempotent；dedupe identity 綁定 run/task/attempt/kind/content hash。

所有 execution path：

| Path | 要求 |
| --- | --- |
| normal DAG worker | task done + typed result + verification 後 reduce |
| direct-agent | 與 DAG 共用 helper，不另做 mapping |
| fast-path 升級到 team | 相同 execution identity，不能 double ingest |
| extra-model/judge | isolated candidates 不得直接污染 main shared memory；只 ingest 被採用且已驗證的 final result |
| sidecar/judge/skeptic | 不作 shared working-memory producer，除非產生 canonical task result |
| unattended | 同一 reducer，無互動 fallback |
| crash-resume | 已完成 task 不重跑 side effects，reducer 可安全補寫 |
| dry-run/plan-only | 不寫 knowledge item |

主要檔案：

- `internal/team/task_result.go`
- `internal/team/coordinator_task_run.go`
- `internal/team/coordinator_run.go`
- `internal/team/coordinator_extra_models.go`
- `internal/team/worker_memory.go`（抽取共用 ingest identity，不改 private policy）
- task transition／journal／resume tests

驗收：worker 不呼叫任何 memory tool，仍可由 typed result 得到正確 shared STM kinds；所有 path 產生一致結果且不重播已完成 side effect。

### HF-MEM3-005 — Tighten Knowledge Extraction

**Priority：P0；依賴 MEM3-001 與 MEM3-004。**

目標：只從具有 reusable knowledge 語意與 evidence 的 ContextItem 建立 persistent candidate。

允許來源：

- `ContextDecision`
- `ContextObservation`（finding）
- `ContextConvention`
- `ContextArchitecture`
- `ContextPattern`
- `ContextError`，但必須包含 resolution／mitigation 或 verified fix evidence

禁止來源：

- `ContextProgress`
- 未解 `ContextOpenQuestion`
- raw tool output／conversation prose
- rejected、expired、superseded、unverified candidate
- 只有「完成 xxx」而無可重用內容的 summary

實作步驟：

1. 把 `autoExtractCanonicalLTM()` 改為 typed extractor，不讀 legacy Markdown。
2. extractor 輸出 `KnowledgeCandidate`，包含 kind、content、confidence、source item IDs、evidence refs、future utility reason。
3. deterministic quality gate 先檢查 evidence、內容長度、generic progress pattern、secret redaction與 scope stability。
4. candidate 寫入 MEM3-001 shared service；不直接 confirmed。
5. 相同 source item／content 的 extraction 必須 idempotent。

驗收：`ContextProgress` 無論 run 是否 accepted 都不能出現在 persistent projection；有 verified decision/finding 的 accepted run 可產生 confirmed shared LTM。

### HF-MEM3-006 — Atomic supersession 與 save-time conflict detection

**Priority：P1；依賴 MEM3-001/003。**

目標：永久知識採 append-only revision，不原地覆寫；寫入前辨識 duplicate／extension／supersedes／conflict。

分類：

```text
NEW         → append candidate
DUPLICATE   → 不新增；記錄 recurrence／evidence edge
EXTENSION   → append candidate + related edge
SUPERSEDES  → atomic append + supersede old IDs
CONFLICT    → 保留 candidate，不 supersede；標記 needs_review
```

實作要求：

- 先用 exact hash，再用 lexical/vector 找同 scope confirmed memories。
- model-assisted comparison只能提出 classification；scope authorization、transaction 與 lifecycle 由 deterministic runtime 執行。
- conflict 不可自動選邊；一般 recall 仍使用既有 confirmed truth。
- recurrence、novelty、verification、confidence、future utility 可作 candidate score，但 score 不得直接修改 Skill／Team。
- `MarkSuperseded()` 保留 compatibility；新 runtime 一律使用 atomic append API。

驗收：transaction injection failure 不會留下半套 supersession；conflict candidate 不進 prompt；history 可由 edges 完整追蹤。

### HF-MEM3-007 — Deprecate `stm_write` / `ltm_update`

**Priority：P1；依賴 MEM3-003/004/005。**

目標：移除 model 對 Markdown-shaped mutation 的依賴。

階段：

1. compatibility：保留 tool name，但 description 標示 deprecated。
2. `stm_write`：只允許明確 typed `kind` append；舊 `mode=replace` 不再宣稱 replace。coordinator maintenance replacement 改走 canonical supersession API。
3. `ltm_update`：改為 `memory_propose` compatibility alias；只建立 persistent candidate。
4. prompt 移除「必須在 finish 前呼叫 stm_write」規則；finish 不再以 `lastStmWrite` 判斷 session knowledge 是否保存。
5. reducer 與 extraction soak period 通過後，從 worker/core tool surface 移除舊 tool；舊 team config 出現名稱時給明確 migration warning。
6. 最終只保留 model-facing `memory_query` 與可選的 `memory_propose`。

涉及：

- `internal/agent/agent.go`
- `internal/team/coordinator_prompt.go`
- `internal/team/coordinator_tools_memory.go`
- `internal/team/coordinator_run.go`
- allow/deny/tool-policy tests
- default team 與 frontmatter compatibility tests

驗收：沒有 memory mutation tool 的 worker 仍能完成 working/persistent memory lifecycle；舊名稱不會靜默改變語意。

### HF-MEM3-008 — `hufu context` lifecycle maintenance

**Priority：P1；依賴 MEM3-001/006。**

目標：在既有 namespace 加入 knowledge lifecycle 維護，不建立第二套 CLI。

新增命令：

```text
hufu context show <id>
hufu context candidates
hufu context confirm <id...>
hufu context reject <id...>
hufu context supersede <old-id...> --with <new-id>
hufu context history <id>
hufu context consolidate [--dry-run]
```

安全規則：

- mutation 必須要求明確 workspace/project/team 與 item IDs。
- 預設不顯示 content；`--show-content` 仍須 redaction。
- private cross-agent 維護必須顯式 `--all-agents` 或 exact `--agent`；不得沿用 runtime ancestors query 擴大 mutation scope。
- confirm 要顯示 evidence／run binding；無 evidence candidate 預設拒絕，除非新增明確 maintenance override 並留下 audit event。
- JSON output 維持 schema version；新增欄位需向後相容或升版。

驗收：CLI 可查出每筆 item 的 lifecycle、scope-derived lifetime、supersession history、evidence 與 projection eligibility；所有 mutation 有 event/revision。

### HF-MEM3-009 — Canonical prompt read-path cutover

**Priority：P1；依賴 MEM3-001～005。**

目標：ContextCompiler 直接接收 canonical query results，不再以 `LoadSTM()`、`LoadLTM()` 或 chromem record 作 primary historical source。

目前需補齊的路徑：

| Path | 現況 | Cutover |
| --- | --- | --- |
| normal DAG worker | compiler model-visible，但 RawSTM/RawLTM 來自 Markdown | 改為 authorized canonical bundle |
| direct-agent | legacy suffix model-visible，compiler 只 shadow | 改為與 DAG 共用 compile helper |
| coordinator | compiler model-visible，但 historical source 仍由 Markdown/chromem 組裝 | 改為 canonical shared retrieval |
| extra-model | isolated coordinator 可能寫 isolated context | read main snapshot；只讓 winner 回寫 main lifecycle |
| resume/continuation | session checkpoint + legacy memory | canonical revision/scope 固定於 resume snapshot |
| fresh-verification | 已禁 historical memory | 保持 fail-closed，不因 cutover 重新注入 |

實作步驟：

1. 新增 `CanonicalContextBundle`，分 shared session、shared persistent、private worker、dependency results。
2. 授權與 retrieval 在 compiler 前完成；compiler只作 authority labeling、dedupe、ranking、conflict check、token budget。
3. canonical item ID、kind、authority、trust、lifecycle、evidence summary保留到 compiler trace。
4. 以現有 shadow trace 比較 legacy/canonical coverage、missing anchors、token usage 與 selected IDs。
5. 先在 tests／feature gate 下 dual compile，達到 parity 後切換 direct-agent、coordinator。
6. cutover 後 Markdown 僅供人類與舊版相容，不再成為 runtime truth。

驗收：同一 scope 的 DAG/direct/coordinator 對 shared memory 有一致 visibility；candidate/private leakage、normative conflict、token overflow 均 fail closed；shadow parity fixtures 全通過。

### HF-MEM3-010 — Procedural promotion gate

**Priority：P1；依賴穩定的 confirmed LTM。**

目標：從 confirmed experience 產生 Skill／Agent Team proposal，但永不自動發布。

立即修正：移除 `coordinator_skill_patterns.go` 中 `quality >= 0.95 && count >= 15` 直接呼叫 `skill.PromoteDraft()` 的分支。threshold 只能決定是否自動建立 draft/proposal。

Experience Miner 輸出：

```go
type ProceduralProposal struct {
    ID              string
    Target          string // skill | team
    SourceContextIDs []string
    EvidenceRefs    []EvidenceRef
    SuggestedPath   string
    Diff            string
    Status          string // draft | approved | rejected | applied
}
```

分類：

- Skill：可重複 SOP、debug procedure、tool sequence、deployment/verification/recovery 方法。
- Team Markdown：角色責任、team-wide policy、delegation strategy、model/tool governance、project convention。
- 只留 LTM：單一檔案位置、一次性事故、版本特定 bug、低 recurrence observation。

流程：

```text
confirmed memories
  → recurrence/evidence filter
  → proposal + diff
  → explicit user review
  → apply through skill/team writer
  → audit event + source edges
```

驗收：unattended、TUI、plain CLI 都不會在沒有明確批准時修改 `.agents/skills`、`.agent-teams` 或 agent Markdown；批准後可追溯到 source context IDs。

### HF-MEM3-011 — Legacy `MemoryStore` 降級為 index

**Priority：P2；依賴 prompt cutover。**

目標：消除 `MemoryRecord` 與 `ContextItem` 兩套 domain model。

步驟：

1. 停止 `memory_save` 直接寫 `MemoryStore`。
2. vector index document key 使用 canonical ContextItem ID；metadata只作搜尋索引，不保存獨立 lifecycle truth。
3. `hufu context rebuild` 從 SQLite confirmed/current items 重建 FTS 與 vector index。
4. `history.go`、archive memory、`memory.AutoQuery` 改為 canonical repository adapter。
5. 建 migration importer：讀舊 `MemoryRecord`，轉 ContextItem，保存 original ID/source/status/file paths；重跑 idempotent。
6. dual-read shadow 比較完成後刪除 runtime `MemoryRecord` writes；最後移除 chromem canonical APIs。

驗收：刪除 vector index 後可由 SQLite 完整重建；index 不可 promotion/reject/supersede canonical knowledge；舊 store migration 有 backup、dry-run 與 count/hash report。

### HF-MEM3-012 — Legacy artifact cleanup 與文件同步

**Priority：P2。**

目標：完成 cutover 後移除第二套 truth 與過期 proposal 文件。

清理：

- 停止並最終移除 `reflexion_candidates.jsonl`／`reflexion_confirmed.jsonl` runtime code。
- 移除 direct LTM writes 與 Markdown read-back import；legacy importer 保留一個明確版本週期。
- 移除 `context-stm.md/context-ltm.md` 或明確標記 debug projection；只保留一套正式 projection 命名。
- 更新 `docs/hufu-per-worker-memory-implementation-plan.md`，將 WP-1～WP-5 標成 DONE/PARTIAL/OPEN，不再維持「整份提案」狀態。
- 更新 AGENTS.md、CLI help、team config reference、migration guide。

驗收：新的 workspace 不建立 legacy JSONL；runtime 不從 legacy Markdown／MemoryRecord 決定 knowledge truth；upgrade workspace 能被一次性 migration 完整處理。

## 6. 交付順序與 PR 切分

建議依下列順序交付，每個 PR 必須可獨立回滾且保持 repository 可建置：

```text
MEM3-000 baseline tests
        │
        ▼
MEM3-001 canonical shared lifecycle
        │
        ├──────────────┐
        ▼              ▼
MEM3-002 projection   MEM3-003 tool parity
        │              │
        └──────┬───────┘
               ▼
MEM3-004 typed working-memory reducer
               │
               ▼
MEM3-005 knowledge extraction
               │
        ┌──────┴───────┐
        ▼              ▼
MEM3-006 conflict     MEM3-007 tool deprecation
        │              │
        ▼              └──────┐
MEM3-008 context CLI          │
        │                     │
        └──────────┬──────────┘
                   ▼
MEM3-009 prompt cutover
                   │
          ┌────────┴────────┐
          ▼                 ▼
MEM3-010 procedural gate  MEM3-011 index migration
          └────────┬────────┘
                   ▼
MEM3-012 cleanup/docs
```

推薦 PR：

1. PR-1：MEM3-000 + MEM3-001
2. PR-2：MEM3-002
3. PR-3：MEM3-003（supersedes 先拒絕或 feature-gate，待 atomic API）
4. PR-4：MEM3-004 + MEM3-005
5. PR-5：MEM3-006 + MEM3-008 mutation subset
6. PR-6：MEM3-007
7. PR-7：MEM3-009 direct-agent／coordinator cutover
8. PR-8：MEM3-010
9. PR-9：MEM3-011 + MEM3-012

不得把「shared lifecycle、projection split、prompt cutover、MemoryStore removal」塞進同一個不可回滾的大 PR。

## 7. Migration strategy

### Phase A — Characterize and freeze

- 加入 MEM3-000 tests。
- 釘住 schema migration checksum；既有 migration SQL 不可修改，只能新增版本。
- 記錄目前 SQLite／JSONL／Markdown item counts、hash 與 scope 分布。

### Phase B — Canonical shared writes

- 所有新 shared candidate 只寫 SQLite。
- legacy JSONL 暫時唯讀；建立 importer 對舊 candidates/confirmed records 補寫 canonical items。
- importer 必須 dry-run、idempotent、redacted，並在 migration 前確認 backup。

### Phase C — Projection isolation

- SQLite 產生 session STM 與 persistent LTM。
- dual projection 比較內容 coverage，禁止 private/candidate leakage。
- projection mismatch 先 fail CI／emit diagnostic，不回退去寫第二套 truth。

### Phase D — Canonical reads

- 用 shadow trace 比較 canonical vs legacy selected IDs、missing anchors、token usage。
- normal DAG、direct-agent、coordinator、resume、unattended 分批切換。
- feature gate 只控制 read source；write source 不得回退成 JSONL/Markdown。

### Phase E — Retire legacy

- 移除 agent `stm_write/ltm_update`。
- MemoryStore 降為 index。
- 移除 JSONL 與 Markdown read-back。
- 保留明確版本的 migration/repair command，之後再刪。

## 8. Failure semantics 與 recovery

| Failure | 行為 |
| --- | --- |
| canonical candidate append failure | tool/reducer 回報 error；寫 redacted pending queue；不得假稱已保存 |
| lifecycle promotion failure | CompletionGate 將 run 降為 partial/evidence incomplete；不得留下 accepted prose |
| projection write failure | canonical commit 保留；emit event + repair；prompt cutover 後不影響 source truth |
| vector/FTS failure | 使用 SQLite fallback；標記 embedding state；可 rebuild |
| conflict detection unavailable | candidate 保留 `needs_review`；不自動 supersede |
| crash during append+supersede | transaction rollback；舊 confirmed truth 繼續有效 |
| crash after canonical commit before projection | revision/event 可偵測；startup/repair 重建 projection |
| resume 重複 ingestion | deterministic identity 去重；不重跑 external side effect |
| secret found in content/error | persist 前 redaction；audit/report/pending queue 只保存 redacted form |

## 9. Validation matrix

每個 runtime PR 至少執行：

```bash
go test ./...
go vet ./...
golangci-lint run
```

測試層次：

1. **Model/schema**：scope lifetime、lifecycle defaults、migration checksum、metadata compatibility。
2. **Repository**：exact/ancestors/subtree、candidate exclusion、atomic supersession、revision/events、expiry、redaction。
3. **Service**：shared/private propose/confirm/reject、manifest binding、idempotency、authorization。
4. **Tool contract**：Info schema、JSON parsing、all parameters、invalid values、scope spoofing、unsupported behavior。
5. **Execution integration**：DAG、direct-agent、fast-path upgrade、extra-model winner、unattended、resume、dry-run。
6. **Projection**：STM/LTM isolation、private leakage、snapshot、atomic write、repair。
7. **Observability**：event store、session/task journal、CLI JSON/text、report、TUI status、notifications。
8. **Prompt**：canonical compiler authority、conflict、token budget、candidate exclusion、legacy parity trace。
9. **Recovery**：store outage、projection outage、index outage、crash between lifecycle phases、pending replay。
10. **Security**：workspace escape、artifact ref confusion、secret redaction、cross-agent mutation、unattended policy。

不得以 live Ollama、外部 API、Pilot checkout 或真實 infra mutation 作 unit/integration test 的必要條件。

## 10. Definition of Done

HF-MEM3 完成時必須同時滿足：

- `context.sqlite` 是 shared/private、session/persistent knowledge 的唯一 source of truth。
- Event Store 與 typed runtime state 仍是 execution truth，memory 不能偽造完成／verification。
- accepted evidence 才能 promotion persistent candidate；failed run knowledge 不可進 prompt。
- STM 只含 shared session confirmed memory；LTM 只含 shared persistent confirmed memory。
- private/candidate/rejected/superseded/expired items 不會洩漏到一般 shared prompt。
- `memory_save/query` 的每個公開參數都被實作、驗證、持久化／查詢，或從 schema 移除。
- `ContextProgress` 不會自動提升為 persistent pattern。
- normal DAG、direct-agent、coordinator、extra-model、unattended、resume 使用一致的授權與 lifecycle contract。
- `stm_write`／`ltm_update` 不再是完成流程必要 tool，最終從 Agent tool surface 移除。
- Skill／Agent Team 只會由 proposal + explicit human approval 修改。
- vector/FTS 可由 SQLite rebuild，不保存獨立 lifecycle truth。
- legacy JSONL 與 direct Markdown write/read-back 已停止。
- `go test ./...`、`go vet ./...`、`golangci-lint run` 全部成功，且沒有已知 contract regression。

## 11. 明確非目標

- 不保存或 replay 完整 worker conversation。
- 不讓 memory 取代 task dependency、artifact store、receipt、verification 或 acceptance。
- 不建立新的 remote memory service、multi-user ACL 或 encryption system。
- 不讓 model 指定任意 AgentID／BranchID／RunID 寫入其他 scope。
- 不把 Hufu core 寫成 Pilot 或其他 consumer-specific workflow。
- 不用一個浮動 score 直接決定 Skill promotion 或 external mutation。
- 不在 canonical correctness 完成前先做 LTM → Skill 自動學習。

最優先的實作路徑是：

```text
MEM3-000 → MEM3-001 → MEM3-002/003 → MEM3-004 → MEM3-005
```

先把 shared candidate、projection、tool contract 與 typed reducer 收斂到 canonical store；在此之前不要啟動 procedural promotion 或移除 migration fallback。
