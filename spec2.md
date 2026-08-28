# Hufu Context Router 收斂實作計畫

## 1. 文件目的與完成判定

本文件把 `spec1.md` 的「完整交付」拆成可安全合併、可驗證的剩餘工作。它不是對目前程式碼已經完全符合 `spec1.md` 的宣告。

本計畫涵蓋本文列出的全部缺口與 execution path。不得以範圍排除或延期措辭移除其中任一項。文中的 deterministic fallback 是每個路徑都必須完成、持久化並測試的正式行為，不是縮減範圍的理由。

目前已具備的基礎包括：typed `ContextRequest`、request-aware router、typed compiler、general `ContextInjectionManifest`、model-execution identity、activation projection、attempt-scoped retry request、worker/direct/nested/extra-model 的主要路徑、progressive skill disclosure，以及 tool-failure 的下一 turn recovery annotation。

本計畫完成時，以下條件必須同時成立：

1. 每個實際模型呼叫都有 purpose-specific request、受限 compiled prompt，以及在呼叫前持久化的 manifest；無模型 fallback 有可辨識的 fallback manifest。
2. `context_query`、`context_get` 和 tool-failure recovery 維持目前 execution 的 phase、trigger、role、scope、attempt 與 model-execution identity；不得用泛用 trigger 破壞 activation eligibility。
3. coordinator runtime、CLI preflight/fix/promotion、skill-learning 與 compaction 等模型呼叫，要麼接入相同 attribution boundary，要麼有明確且測試過的 deterministic no-model 例外。
4. 任何 router/compiler/manifest persistence 失敗都發生在模型或 side effect 前；guard 保持 fail closed，recovery 不重播可能已完成的 side effect。
5. manifest、receipt、session、event replay、branch checkout、journal、JSON、report 與 TUI 對同一 invocation 的 identity 和 aggregate 一致，且不保存 prompt/context content、raw tool input/output 或秘密。
6. `go test ./...`、`go vet ./...`、`golangci-lint run` 全部成功。

本文件的「模型呼叫」包含 `fantasy.Agent.Generate`、`sidecar.Sidecar` 的 `generate/Execute/ExecuteProfile/Summarize/Compact`，以及等價的 provider、subprocess 或 MCP-backed generation 呼叫；不能只搜尋 coordinator worker。

## 2. 現況與已知缺口

### 2.1 已完成且必須維持的契約

| 項目 | 現有 owner | 必須保持的結果 |
| --- | --- | --- |
| request validation、fingerprint、redaction | `internal/team/context_request.go` | request identity 可重現，failure evidence 不序列化 |
| activation eligibility | `internal/team/context_router.go`、`internal/context/activation.go` | lifecycle/scope/expiry 先於 ranking；VERIFY 不吃 generic history |
| worker retry context | `internal/team/coordinator_task_run.go` | 每次 attempt route/compile/persist；不覆寫前 attempt |
| general manifest 與 receipt | `internal/team/context_manifest.go`、`execution_receipt.go` | 呼叫前 checkpoint，content-free identity 與 replay 穩定 |
| core sidecar adapter | `internal/team/auxiliary_context.go`、`coordinator_skills.go` | coordinator 建立的 sidecar 都走 prompt preparer |
| skill disclosure | `internal/team/coordinator_skills.go`、`tool_policy_gate.go` | mandatory load 在 task-work tool 前被 runtime 強制 |
| tool-failure annotation | `internal/team/coordinator_tools_context.go` | failed tool result 才建立 bounded recovery context |

### 2.2 尚未達成的可證明缺口

| ID | 缺口 | 現有位置 | 影響 |
| --- | --- | --- | --- |
| GAP-1 | `context_query/get` 固定用 `ContextTriggerAuxiliary`，沒有繼承 task dispatch/retry/tool-failure trigger。 | `internal/team/coordinator_tools_context.go` | `activation.triggers=retry` 等規則在 JIT query/get 失效。 |
| GAP-2 | `context_get` 直接用 repository item 作 eligibility，沒有先套用 typed activation projection。 | `internal/team/coordinator_tools_context.go` | metadata 與 typed projection 在 mixed-version/rebuilt DB 下可能分歧。 |
| GAP-3 | team auto-selection 在 coordinator 產生前建立 sidecar，沒有 context boundary 或 manifest。 | `cmd/hufu/autoteam.go` | 實際模型呼叫無 attribution/replay。 |
| GAP-4 | `hufu fix` 的 direct `ollama run` 繞過 provider、router、redaction/manifest 與 budget accounting。 | `cmd/hufu/fix.go` | 有未追蹤的模型及可能含敏感 execution data 的 prompt。 |
| GAP-5 | promotion draft generator 建立獨立 sidecar，未接 prompt preparer/manifest。 | `cmd/hufu/context_promotion_cmd.go` | compaction/generation 無 invocation evidence。 |
| GAP-6 | `internal/skill/discovery.go` 的 pattern-analysis、cluster、naming sidecar 呼叫未由 coordinator boundary 納管。 | `internal/skill/discovery.go` | skill-learning 的模型路徑無統一 policy、persistence 或 fallback record。 |
| GAP-7 | 尚無靜態 chokepoint test，防止新模型呼叫繞過 context boundary。 | repository-wide | 未來容易回歸。 |
| GAP-8 | `spec1.md` 的完整 integration/replay/security test matrix 沒有逐項可追溯的證據。 | tests across `internal/team`、`cmd/hufu` | 現有 unit tests 不能證明所有 path parity。 |

除非 GAP-1 至 GAP-8 關閉，release note、commit message 與文件不得使用「spec1 完整完成」的說法。

## 3. 不可違反的設計邊界

1. canonical context 仍由 `internal/context` 擁有；不得新增第二個 memory/context repository。
2. phase、task、attempt、role、failure class、tool policy 與 receipts 是 `internal/team` runtime contract；generic repository 不可反向依賴 team。
3. prompt、raw query、raw transcript、raw tool input/output、credential 與 full skill content 不得進 manifest、event、journal、report、session 或 TUI status。
4. artifact/transcript 只能以已授權的 opaque reference 流通。不得以 file path、Todo ID 或任意 context ID 當成讀取授權。
5. context failure 不能變成 allow；guard reviewer failure、router failure、compiler failure、manifest persistence failure 均依呼叫類型 fail closed。
6. `before_tool_call` 不是同一輪 prompt injection 機制；JIT recovery 只能影響下一 model turn。
7. protocol repair、crash-resume 和 retry 必須沿用既有 `DecideRecovery`、receipt、verification、anti-thrashing 與 side-effect reconciliation；不得自行 replay action。

## 4. 目標架構

### 4.1 統一 invocation boundary

新增一個由 team runtime 擁有的 invocation boundary；核心建議放在 `internal/team/context_invocation.go`，而不是令 `internal/sidecar` 知道 session、phase 或 SQLite。

```go
type ContextInvocation struct {
    Request       ContextRequest
    Purpose       string
    ModelID       string
    ModelCalled   bool
    FallbackKind  string
    SourceClass   string // worker, coordinator, sidecar, cli_preflight, promotion, skill_learning
}

type ContextInvocationBoundary interface {
    Prepare(context.Context, ContextInvocation, WorkerContextInput) (CompiledContext, ContextInjectionManifest, error)
    RecordFallback(context.Context, ContextInvocation, string) error
}
```

`Prepare` 必須依序驗證 request、route、compile/redact、建立 manifest、持久化 manifest/selection observation，最後才回傳 prompt。呼叫端收到 error 時不得發動模型。它不是第二個 compiler；worker/coordinator/auxiliary 都繼續使用既有 compiler 和 `persistContextManifest`。

為了處理 coordinator 尚未建立的 CLI 呼叫，提供狹義 adapter：

```go
type PreflightContextBoundary struct {
    Workspace string
    Scope     contextstore.Scope
    Repo      contextstore.Repository
    Events    team.EventStore // optional but required when replay is promised
}
```

此 adapter 必須只產生 `PhaseInit`、preflight purpose 的最小 request，並將 manifest 寫入與該 workspace 綁定的 session/event lineage。若 command 沒有安全可用的 workspace/repository/event store，該 command 必須使用 deterministic fallback 或顯式拒絕模型模式；不能悄悄呼叫模型。

### 4.2 Invocation purpose registry

新增封閉 purpose registry（例如 `internal/team/context_purpose.go`）。每個 purpose 定義：trigger、預設 phase、必要 fragment、允許來源、是否能用 fallback、fallback outcome、最大 input/output budget、是否需要 task identity。

| Purpose | Trigger | 必要來源 | 禁止來源 |
| --- | --- | --- | --- |
| `skill_matcher` | `skill_match` | goal、role、skill index summary | full skill、raw history |
| `guard_reviewer` / `path_reviewer` | `guard_review` | guard contract、agent、tool、redacted bounded args | STM/LTM、worker transcript |
| `plan_reviewer` | `plan_review` | goal、constraints、plan revision、criteria | unrelated chatter |
| `judge` | `judge` | selection contract、candidate IDs、bounded candidates | worker-private memory |
| `skeptic` | `skeptic` | goal、criteria、candidate、artifact/verify refs | raw transcript |
| `reflection` / `result_repair` | `repair` | approved failure evidence、mutable fields、disposition | unapproved evidence/replay instruction |
| `sidecar_task` | `sidecar_task` | typed task goal/constraints/contract | ambient session history |
| `compacter` | `sidecar_task` with purpose `compacter` | explicit bounded source plus compaction contract | ambient session history |
| `team_selection` | `coordinator_start` with purpose `team_selection` | user goal、team summary index | team/private context |
| `fix_analysis` | `sidecar_task` with purpose `fix_analysis` | redacted bounded diagnostic bundle | raw credentials/transcript |
| `promotion_draft` | `sidecar_task` with purpose `promotion_draft` | approved proposal/snapshot refs and draft contract | ambient runtime history |
| `skill_learning` | `sidecar_task` with purpose `skill_learning` | bounded aggregate/sequence contract | raw task transcript |

目的 registry 必須被 request builder、source allowlist、manifest renderer、fallback recorder 與 static audit 共用；不可讓各 caller 用字串自行猜測 trigger。

### 4.3 原始 invocation context 的傳遞

新增只存在於 Go `context.Context` 的 execution metadata：

```go
type contextInvocationKey struct{}
type InvocationMetadata struct {
    RunID, TaskID, AgentName, AgentRole, ModelExecutionID string
    Attempt int
    Phase Phase
    Trigger ContextTrigger
    Purpose string
}
```

worker dispatch、retry、direct/nested agent、extra-model、sidecar task 及 repair 必須在建立 model stream 前寫入 metadata。後續 `context_query/get`、tool-failure recovery、guard/judge 等可從此 metadata 建立子 request。子 request 可以有自己的 purpose/action ID，但 `Phase`、`Trigger`、scope、attempt、agent role 與 model execution identity 必須可追溯回 parent；若 purpose 需要不同 trigger，manifest 必須同時保存 `parent_trigger`（加入新的 content-free 欄位）而非遺失 parent。

## 5. 分段實作計畫

### CTX-12：建立 invocation inventory 與 anti-bypass gate

**目標：** 先確定沒有漏網模型入口，再遷移個別路徑。

修改：

- 新增 `internal/team/context_invocation_audit_test.go`。
- 必要時在 `internal/sidecar/sidecar.go` 補可識別 boundary ownership 的 interface，但不引入 team 依賴。
- 新增 repository manifest，例如 `internal/team/testdata/model_call_chokepoints.txt`，列出每個允許 model call 的 owner、purpose 與測試。

工作：

1. 搜尋 `Generate(`、`Execute(`、`ExecuteProfile(`、`ollama run`、provider generation API 與任何代理 wrapper。
2. 將每個結果分類為：runtime boundary、preflight boundary、deterministic no-model、或待遷移。
3. static audit test 必須在新未登錄 chokepoint 出現時失敗；允許項目以小型、明確 allowlist 管理。
4. audit 也檢查 `sidecar.NewSidecar` 的使用者是否建立 boundary/preparer，或被標示為 deterministic-only test fixture。

驗收：

- 所有 production model chokepoint 在 inventory 有唯一 owner/purpose。
- 新增一個未登錄 fake generation call 會令 audit test 失敗。
- test fixture 和純 deterministic fallback 不被誤判。

### CTX-13：修正 JIT request 繼承與 item authorization

**目標：** GAP-1、GAP-2 關閉，讓 tool JIT 真正遵守目前 execution contract。

修改：

- `internal/team/coordinator_tools_context.go`
- `internal/team/context_request_runtime.go`
- `internal/team/context_router.go`
- `internal/team/context_manifest.go`
- `internal/team/context_request_test.go`
- 新增或擴充 `internal/team/coordinator_tools_context_test.go`

工作：

1. 將 `InvocationMetadata` 寫入 worker/direct/nested/extra-model stream contexts。
2. `contextToolRequest` 改為衍生 parent metadata；`context_query/get` 不再硬編碼 `ContextTriggerAuxiliary`。
3. 若 JIT purpose 需要子 trigger，新增 `ParentTrigger`、`ParentRequestID`、`ParentManifestFingerprint` 到 request runtime-only identity 與 manifest content-free projection；fingerprint 必須包含這些 canonical fields。
4. `context_get` 取得 item 後，必須先以 `activationItemFromRepository` 合併 typed activation projection，再呼叫 eligibility；不得只依舊 metadata。
5. 將 `context_get` 的 lookup 改為 router-owned authorized selection 或明確 `GetAuthorizedContextItem` helper，保證 scope/lifecycle/activation/VERIFY restriction 與 `context_query` 相同。
6. 對 rejected JIT access 回傳 recoverable tool error，不中止 model round；manifest/event 記錄 denied reason，但不洩漏 item content。

驗收：

- retry worker 的 `context_query/get` 可取用 `activation.triggers=retry` item；dispatch worker 不可取用。
- VERIFY JIT 不可取 generic 或 EXECUTE-only item。
- typed activation projection 和 metadata 不一致時，repository projection 是 canonical 來源。
- cross-run、cross-worker、expired、superseded、candidate-from-other-run 的 query/get 都被拒絕。
- query/get 的 manifest 與 parent invocation 可由 receipt/session/event replay 關聯。

### CTX-14：收斂 coordinator 內所有 auxiliary paths

**目標：** 將現有 `prepareAuxiliaryPrompt` 升級為 registry 驅動的 complete boundary，確認 sidecar task、guard、path review、plan review、judge、skeptic、reflection、result repair、final summary repair 和 compacter 都符合各自 allowlist。

修改：

- `internal/team/auxiliary_context.go`
- `internal/team/coordinator_skills.go`
- `internal/team/coordinator_task_run.go`
- `internal/team/coordinator_judge.go`
- `internal/team/coordinator_skeptic.go`
- `internal/team/coordinator_plan.go`
- `internal/team/context_compiler.go`
- 對應 auxiliary tests。

工作：

1. `prepareAuxiliaryPrompt` 接收 structured invocation input，不只接受已手組的 raw prompt。
2. 各 caller 改傳 typed contract fragments；raw prompt 僅能成為經 redaction/size bound 的 candidate/evidence fragment。
3. compiler 對 `DisableMemory` auxiliary call 仍建立 required purpose contract，並依 registry 拒絕未允許 source。
4. compacter 需提供明確 source items 和 compaction instruction；不應把既有 conversation/ambient context 混入。
5. sidecar unavailable、judge/skeptic deterministic fallback、guard failure 均使用 registry 定義的 `ModelCalled=false` manifest outcome。
6. auxiliary model usage 必須仍進既有 token/no-progress budget；不能以新增 boundary 逃避 budget。

驗收：

- 每個 coordinator-owned sidecar call 在 model call 前有 manifest，且該 manifest purpose/trigger/agent/model execution identity 正確。
- guard router/compiler/persistence failure 仍 deny；judge/skeptic fallback 可區分 no-model 與 model error。
- judge/guard/skeptic test 證明 unrelated STM/LTM、full skill、raw transcript 不進 prompt。
- 每個 fallback 只產生一份冪等 manifest，不因重試重複覆寫其他 invocation。

### CTX-15：遷移 CLI preflight、fix 與 promotion 模型路徑

**目標：** GAP-3、GAP-4、GAP-5 關閉。

修改：

- 新增 `cmd/hufu/context_preflight_boundary.go` 與測試。
- `cmd/hufu/autoteam.go`
- `cmd/hufu/fix.go`
- `cmd/hufu/context_promotion_cmd.go`
- 必要時 `internal/team/context_invocation.go` 的可重用 adapter。

工作：

1. auto-team 在選擇 team 前建立 preflight run identity、project/workspace scope 與 `team_selection` invocation。若沒有安全 workspace，回退至既有 keyword selection，並記錄 deterministic no-model outcome（若沒有 event store，至少輸出受測的非持久化 reason；不得叫模型）。
2. `hufu fix` 移除 direct `exec.Command(... "ollama", "run" ...)` generation 路徑。改用 provider manager + sidecar 以及 preflight/loaded-team boundary；若無法建立必要 context store，回傳可操作 error 或 deterministic local analysis，不得執行未追蹤模型。
3. promotion generator 使用 promotion workspace 的 SQLite repository/event store，透過 `promotion_draft` purpose 寫入 manifest；proposal content 採必要、redacted、bounded fragment。
4. 所有 CLI adapter 使用 explicit timeout、model identity、usage accounting 和 fallback classification。
5. CLI JSON/report 在相關 command 可顯示 invocation summary；不得回傳 manifest item content。

驗收：

- auto-team 的模型與 keyword fallback 都有可區分結果；model-selection persistence 可重播。
- `hufu fix` code path 不再含 `ollama run`；provider/manifest preflight failure 在模型前停止。
- promotion draft call 的 manifest 在 promotion event lineage 可重播，並與 proposal hash 關聯。
- secret fixture 不出現在 CLI error、manifest、event 或 report。

### CTX-16：遷移 skill-learning 及其他非 coordinator sidecar owners

**目標：** GAP-6 關閉，避免 `internal/skill` 自行持有未納管 LLM。

修改：

- `internal/skill/discovery.go`
- 新增小型 interface（例如 `SkillModelInvoker`）和 fake test implementation。
- team 建立 `SkillPatternDetector` 的 composition root。
- 相關 `internal/skill/*_test.go` 與 `internal/team/*skill*_test.go`。

工作：

1. `SkillPatternDetector` 不直接依賴裸 `*sidecar.Sidecar`；改依賴可由 coordinator/invocation boundary 包裝的 invoker。
2. pattern generalization、cluster、LLM naming 各有 `skill_learning` sub-purpose、bounded input schema、manifest/fallback outcome。
3. 只有在有 runtime context boundary 時才允許 LLM skill learning；離線或 unit caller 使用明確 deterministic heuristic。
4. 把 model result 視為 untrusted suggestion，維持既有 parse/validation 與人工/review gates；不得讓生成內容直接改 policy/skill。

驗收：

- 三種 skill-learning model call 都有 purpose-specific manifest，或 deterministic path 有 no-model record。
- malformed sidecar output、timeout、repository error 都不會生成/套用 skill。
- skill-learning prompt 不含 raw tool transcript 或 credential。

### CTX-17：完成 outcome policy lifecycle 與 projection 收斂

**目標：** 把 general manifest 的實際 included subset，可靠地連到 selection/use/verification/acceptance/judge/skeptic outcome；確認 optimizer 只用可證實 exposure。

修改：

- `internal/context/activation.go`
- `internal/context/sqlite_repository.go`（只新增 migration）
- `internal/team/context_manifest.go`
- `internal/team/memory_outcome.go`
- `internal/improve/memory_policy.go`
- `cmd/hufu/context_learning_cmd.go`
- migration/reopen/replay/adopt/rollback tests。

工作：

1. 定義 observation lifecycle：`selected`、`tool_consulted`（context_get 成功）、`verification_assessed`、`acceptance_assessed`、`judge_signal`、`skeptic_signal`、`failure_attributed`。
2. 每筆 observation 必須以 `(request_id, manifest_fingerprint, run_id, task_id, attempt, model_execution_id, context_item_id, event_kind)` 冪等；omitted item 永不取得 exposure/use 正向訊號。
3. verification/acceptance 訊號僅由 objective lifecycle owner 產生，不從 model prose 推論。
4. candidate policy 不可直接改 active policy；保持 immutable snapshot → shadow comparison → deterministic acceptance → explicit adopt → evented rollback。
5. migration 要可重跑、保持 legacy metadata/old snapshots 可讀、保留 content hash；對已有 DB 先備份的機制不能被破壞。

驗收：

- phase/trigger/role/environment 與 execution linkage 可查詢且 replay 一致。
- failed/unverified run 無正向 adoption signal；omitted memory 無 exposure signal。
- shadow 不影響 prompt ordering；未 adopt candidate 不影響 active routing。
- adopt/rollback/reopen/mixed-version tests 均可重建相同 revision/hash。

### CTX-18：完整 projection、recovery 與 coverage gate

**目標：** GAP-8 關閉，將完成條件變成自動化證據。

修改：

- `internal/team/context_manifest_test.go` 與新的 integration tests。
- `cmd/hufu/json_output_test.go`、`report_test.go`、`sessioncmd_test.go`。
- `cmd/hufu/display.go`、`internal/tui/*_test.go`（僅在需補 message/renderer contract 時）。
- 新增 `docs/context-router-coverage.md` 或由 testdata 生成 matrix；不要以人工宣告代替測試。

工作：

1. 為每一 invocation path 測試「manifest persistence 成功後才發模型」；模擬 SaveSession/event/repository failure。
2. 為 worker retry、tool failure、protocol repair、timeout、cancel、budget、permission denial、verify failure、crash-resume 建立 receipt/manifest non-overwrite tests。
3. 驗證 session reload、event reducer replay、branch fork/checkout、task journal、projection shadow、JSON、report、TUI detail/status 的 summary identity 一致。
4. TUI 只顯示 content-free request/attempt/purpose/count/token/reason/fallback；`Update()` 保持純函式，既有 View priority 不變。
5. 對 raw secret、tool input/output、full skill、transcript 作 sink matrix test：session、event、receipt、journal、JSON、report、TUI status、error。

驗收：

- 下列 matrix 每格都有自動測試，並可追溯到 manifest/receipt identity。

| Path | dispatch/retry | manifest before call | replay | source isolation | fallback |
| --- | --- | --- | --- | --- | --- |
| coordinator/start/continue | required | required | required | coordinator allowlist | required |
| DAG/direct/nested | required | required | required | worker contract | required |
| extra-model/judge | required | required | required | model execution separation | required |
| workflow PREPARE/AUDIT/EXECUTE/VERIFY | required | required | required | phase eligibility | required |
| tool failure/query/get | required | required | required | parent trigger + authorization | required |
| guard/path/plan/skill/skeptic/reflection/repair | required | required | required | purpose allowlist | required |
| sidecar task/compacter | required | required | required | explicit sources | required |
| auto-team/fix/promotion/skill-learning | required | required | required where workspace exists | preflight allowlist | required |

## 6. 實作順序與 commit 邊界

1. **PR/commit A — CTX-12**：inventory 與 static anti-bypass test。先建立可檢查範圍。
2. **PR/commit B — CTX-13**：JIT inheritance 與 `context_get` authorization。此項優先，因為現有 activation semantics 可能被繞過。
3. **PR/commit C — CTX-14**：coordinator-owned auxiliary registry/boundary 與 allowlists。
4. **PR/commit D — CTX-15**：auto-team、fix、promotion 的 CLI/preflight boundary，並移除 direct Ollama generation。
5. **PR/commit E — CTX-16**：skill-learning invoker composition。
6. **PR/commit F — CTX-17**：outcome lifecycle、migration、policy adoption/rollback evidence。
7. **PR/commit G — CTX-18**：matrix integration tests、projections、security sink tests、coverage documentation。

每個 commit 只能宣告已完成其自身 slice，並在 commit/PR 描述列出：新增模型 paths、request/manifest schema version、fallback 行為、persistence/replay 影響、已執行 validation、以及未關閉的 GAP IDs。

## 7. 測試與驗證規格

### 7.1 必要單元測試

- ContextRequest/manifest fingerprint 在 parent invocation、purpose、attempt、trigger、model execution 改變時分離。
- activation metadata/typed projection 的 parse、mixed-version、mismatch、expiry/lifecycle/scope。
- purpose registry 的 required/forbidden sources、redaction、size bound。
- JIT query/get authorization、closed sequence、unattended、force-MCP、cross-scope denial。
- fallback idempotency、content-free durable sinks。
- policy observation/adopt/rollback/idempotency。

### 7.2 必要 integration 測試

- 真正含兩個 model turn 的 failed tool case：第一 turn 得到 error tool result，第二 turn 收到 recovery annotation，無 side-effect replay。
- 同一 task 三個 extra models：三個 distinct request/manifest/receipt identity，replay 後不互相覆寫。
- guard review router/persistence failure 仍拒絕工具。
- cancel/protocol repair/crash-resume 不生成重複 worker/action；completed receipt/manifest 不變。
- preflight CLI model path 的 workspace/repository 不可用時不發模型。

### 7.3 必要指令

每一個修改 Go code 或 tests 的 slice 結束時，按順序執行：

```bash
gofmt -w <changed-go-files>
go test ./...
go vet ./...
golangci-lint run
git diff --check
```

不得以 cached unit test、僅 compiler test 或模型文字回應取代上述 integration/replay/security 證據。

## 8. 最終 release checklist

- [ ] GAP-1 至 GAP-8 全部關閉並各有測試連結。
- [ ] model-call inventory 沒有未登錄 production chokepoint。
- [ ] 所有 invocation 在 model call 前具 request、compiled prompt、manifest 與 persistence boundary。
- [ ] deterministic fallback 有 distinct no-model manifest；model failure 不偽裝成 fallback。
- [ ] request/manifest/persistence 及所有 display sinks 均無秘密或 context content leak。
- [ ] phase/trigger/role/environment eligibility 在 worker、JIT、auxiliary 和 preflight path 一致。
- [ ] task/receipt/session/event/journal/branch replay/JSON/report/TUI 的 identity/aggregate 一致。
- [ ] acceptance、verification、judge、skeptic、failure outcome 只由正確 authority 寫入。
- [ ] `go test ./...`、`go vet ./...`、`golangci-lint run` 與 `git diff --check` 成功。

只有所有 checkbox 完成後，才可將 `spec1.md` 的完整交付條件標記為已完成；屆時應把 `spec.md`、`spec1.md` 與本文件整理至 `docs/`，保留設計史與驗收證據，而非將根目錄的暫存規格當成未註記的 release 文件。
