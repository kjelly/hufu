# Hufu Context Router 未完成工作規格

## 1. 文件目的

本文件定義目前 Context Router 實作仍需完成的功能、runtime 契約、持久化要求與驗收條件。完成本文件要求後，Hufu 必須能依據 agent、phase、attempt、trigger、failure evidence 與 environment，為每一次模型呼叫建立最小充分、可解釋、可重播且不洩漏敏感內容的 context。

現有程式已具備以下基礎，實作時應直接沿用：

- typed context compiler 與 required/optional budget semantics；
- `ContextRequest`、deterministic query/fingerprint 基礎；
- canonical context repository 與 hybrid retrieval；
- candidate/inject K 與 learning mode ranking；
- activation metadata eligibility 基礎；
- worker retry 的 per-attempt route/compile；
- general context manifest、receipt/session/event/JSON/report 的部分接線；
- skill summary/full fallback 基礎；
- typed dependency result projection。

實作不得建立第二套 memory repository，不得繞過既有 tool policy、execution receipt、objective verification、recovery disposition、anti-thrashing、event store 或 session checkpoint。

---

## 2. 完成交付定義

以下條件必須同時成立：

1. coordinator、DAG worker、direct-agent、nested `request_agent`、extra-model worker、retry、repair 與所有 auxiliary LLM 呼叫都建立 purpose-specific `ContextRequest`。
2. 每一次實際模型呼叫都必須先經 context routing、compiler budget、secret redaction 與 context manifest persistence。
3. 每一次 deterministic fallback 都必須留下可區分的 no-model/fallback manifest decision。
4. task/session/event/receipt/journal/branch replay/JSON/report/TUI 對 manifest 的投影一致。
5. VERIFY phase 只取得驗證所需的 typed evidence，不注入 raw transcript 或無條件 generic history。
6. assigned、forced、auto-selected skill 都遵守 progressive disclosure，且 mandatory skill load 由 runtime 強制。
7. tool failure 能在下一個 model turn 取得 bounded recovery context，並提供 policy-gated JIT context tools。
8. activation policy 具有 typed schema/index、outcome attribution、candidate policy、shadow comparison、adopt 與 rollback。
9. 所有 execution-path coverage matrix 項目都有通過的自動化測試。
10. `go test ./...`、`go vet ./...`、`golangci-lint run` 全部成功且無錯誤。

---

## 3. ContextRequest 契約補全

### 3.1 Trigger 集合

擴充 `internal/team/context_request.go`：

```go
const (
    ContextTriggerCoordinatorStart ContextTrigger = "coordinator_start"
    ContextTriggerContinuation     ContextTrigger = "continuation"
    ContextTriggerTaskDispatch     ContextTrigger = "task_dispatch"
    ContextTriggerRetry            ContextTrigger = "retry"
    ContextTriggerToolFailure      ContextTrigger = "tool_failure"
    ContextTriggerSidecarTask      ContextTrigger = "sidecar_task"
    ContextTriggerSkillMatch       ContextTrigger = "skill_match"
    ContextTriggerGuardReview      ContextTrigger = "guard_review"
    ContextTriggerPlanReview       ContextTrigger = "plan_review"
    ContextTriggerJudge            ContextTrigger = "judge"
    ContextTriggerSkeptic          ContextTrigger = "skeptic"
    ContextTriggerRepair           ContextTrigger = "repair"
)
```

`Validate()` 必須依 trigger 驗證必要欄位：

| Trigger | 必要欄位 |
| --- | --- |
| coordinator start / continuation | run ID、goal、agent role、phase |
| task dispatch | run ID、task ID、attempt、goal、agent identity、phase |
| retry | task dispatch 欄位、`Failure`、attempt > 1 |
| tool failure | task ID、attempt、tool name、error class、tool input hash |
| skill match | run ID、goal、agent role |
| guard review | agent、tool name、tool input hash、guard component |
| plan review | task ID、plan/revision identity、verification criteria |
| judge | task ID、candidate identities、selection contract |
| skeptic | task ID、verification contract、candidate identity |
| repair | task ID、approved failure evidence、recovery disposition |

### 3.2 Determinism 與敏感資料

- `RequestID` 必須由 canonical、已正規化且不含 secret 明文的欄位產生。
- `Fingerprint()` 必須包含 trigger、phase、attempt、agent role、environment、failure class 與 model-call purpose。
- extra-model request identity 必須包含穩定的 model execution identity，避免相同 task/agent/attempt 的 manifest 相互覆寫。
- `RetrievalQuery()` 只可包含 redacted、bounded evidence；不得包含 raw tool input、完整 tool output、完整 transcript 或 credential。
- manifest 只保存 request/query hash，不保存完整 query。

### 3.3 測試

- 每種 trigger 的 valid/invalid table test；
- 相同 canonical request 產生相同 fingerprint；
- attempt、trigger、failure class、environment、model execution identity 改變時 fingerprint 必須改變；
- secret、raw tool input 與 raw transcript 不得出現在 request JSON、manifest、event 或 report。

---

## 4. Execution path parity

### 4.1 共用 builder 與 compiler

所有 worker 類路徑必須使用 `buildWorkerContextInput(...)` 與同一 compiler contract：

- normal DAG worker；
- direct-agent；
- nested `request_agent`；
- extra-model isolated worker；
- plan-only worker；
- approved-plan execution；
- retry attempt；
- crash-resumed task。

不得在 caller 中重新把 goal、constraints、plan、verification、runtime context 或 skill content 拼成 giant prompt。

### 4.2 Extra-model identity 與投影

每個 extra-model execution 必須擁有唯一且穩定的：

- request ID；
- manifest fingerprint；
- receipt identity；
- model/provider identity；
- event idempotency key。

多個 model 共用 todo ID 時，manifest 與 receipt 必須以 `(run_id, task_id, attempt, model_execution_id)` 區分，不得以 request ID 或 attempt 單獨覆寫其他 model 的資料。

isolated coordinator 產生的 manifest、receipt、usage 與 failure evidence 必須合併回 canonical parent event lineage，並能由 session reload、event replay 與 branch checkout 還原。

### 4.3 驗收

- 對同一 `TaskDef`，DAG/direct/nested/extra-model 的 normative fragments 必須一致；
- goal、constraints、approved plan、instructions、verification 與 runtime context 各只出現一次；
- plan-only prompt 不得含 execute/result instructions；
- approved-plan execution 必須將 plan 標為 required normative item；
- 同時執行三個 extra models 時，必須留下三份獨立 manifest 與 receipt；
- crash-resume 不得覆寫已完成 attempt 或重新播放已完成 side effect。

---

## 5. Context Router eligibility 完整化

### 5.1 Activation helper

在 canonical context owner 提供共用 parser/validator，支援：

```text
activation.phases
activation.triggers
activation.roles
activation.capabilities
activation.tools
activation.error_classes
activation.environment
```

規則：

- 單一欄位內採 OR；多個非空欄位採 AND；
- phase 使用正式 `Phase` 常數；其他 token trim 後轉小寫；
- environment 明確不一致時 hard omit；
- malformed activation metadata 必須在 model call 前 fail closed；
- lifecycle、scope、authority、superseded、validity 與 expiry gate 必須先於 retrieval/ranking；
- failed-run candidate、其他 worker private memory、expired 或 superseded item 不得重新可見；
- applicability 必須是 deterministic eligibility，不能由 relevance threshold 推導。

### 5.2 Decision reasons

所有 candidate 必須產生唯一且 deterministic 的 inclusion/omission reason。Router decisions 與 compiler 的 dedup/budget omissions 必須合併，不能遺失原始 eligibility reason。

### 5.3 測試矩陣

- PREPARE、AUDIT、EXECUTE、VERIFY phase；
- dispatch、retry、tool failure、guard、judge、skeptic 與 repair trigger；
- role/capability/tool/error/environment match 與 mismatch；
- current-run candidate、failed previous-run candidate、expired、superseded；
- repository unavailable、malformed activation、ranking degradation；
- direct-agent、nested、extra-model 與 retry integration。

---

## 6. Retry、repair 與 crash-resume 完整化

### 6.1 Per-attempt context

每個 attempt 必須在 model call 前依序完成：

1. 建立 request；
2. route canonical context；
3. recall worker-private context；
4. 建立 bounded/redacted failure delta；
5. compile；
6. persist general manifest；
7. persist memory-learning manifest（learning enabled 時）；
8. 建立並持久化 matching execution receipt boundary；
9. 啟動模型。

retry context 必須包含：

- prior failure class；
- bounded/redacted evidence；
- transcript/artifact opaque reference；
- verifier command、exit code與 evidence reference；
- last tool name 與 bounded result summary；
- mutable fields；
- approved recovery disposition。

### 6.2 Recovery invariants

- cancellation 不建立 recovery retrieval；
- protocol-incomplete result repair 不重新建立 worker或重新執行 tool；
- potentially completed side effect 不得自動 replay；
- reflection/repair 只能取得 recovery machinery 核准的 evidence；
- router/compiler/persistence failure 必須發生在 model/side effect 前；
- prior receipt、verification evidence、manifest identity 不得由新 attempt 改寫。

### 6.3 驗收

- attempt 1/2 的 request、retrieval、manifest 與 receipt identity 不同；
- retry-only memory 在 attempt 2 可見且 attempt 1 不可見；
- objective verify failure 的 command/exit/evidence 保留在原 attempt；
- crash-resume 能重建 task context lineage且不重播已完成 action；
- cancellation、timeout、budget、permission denial、protocol incomplete 與 verification failure 都有不同的 failure attribution。

---

## 7. General ContextInjectionManifest 完整化

### 7.1 Canonical manifest

`ContextInjectionManifest` 必須涵蓋每一次模型呼叫與 deterministic fallback，並保存：

- schema version；
- request ID/hash；
- run/task/attempt；
- agent、role、model execution identity；
- phase、trigger、purpose；
- included/omitted items；
- reason、tokens、compressed、base/final score；
- content-free fingerprint 與 timestamp。

不得保存 prompt、context content、raw query、raw tool input、raw output、credential 或完整 failure evidence。

### 7.2 Persistence 與 replay

manifest 必須同步投影到：

- `TodoItem` / session checkpoint；
- coordinator session manifests；
- matching `ExecutionReceipt`；
- event store 與 reducer；
- task journal；
- session reload；
- session branch fork/checkout/time-travel；
- projection shadow；
- JSON output；
- markdown report；
- TUI status/detail log。

event replay 與 branch checkout 後，manifest fingerprint、request identity、item order 與 reason aggregate 必須保持一致。

### 7.3 TUI 接線

新增 context routing status event 與對應的 TUI message，內容至少包含：

- request/attempt identity；
- included/omitted count；
- included token total；
- omission reason aggregate；
- fallback/no-model 狀態。

必須同步修改：

- coordinator status emission；
- `makeTUIReporter` translation；
- `Model.Update()`；
- message tests；
- detail log rendering。

使用既有 status/detail log，不新增 overlay，不改變 `View()` priority order，`Update()` 保持純函式。

### 7.4 Memory manifest projection

`MemoryInjectionManifest` 必須由 general manifest 的 included canonical-memory subset 衍生，不得重新執行 selection。item order、token count、score 與 retrieval identity 必須一致。

### 7.5 驗收

- learning mode `off` 仍有 general manifest，且沒有 learning outcome event；
- task、coordinator、direct、nested、extra-model、retry 與 auxiliary calls 都可在 replay 後還原；
- TUI、JSON、report 的 request/item/token/reason aggregate 一致；
- persistence failure 阻止模型啟動；
- 輸出與 durable sinks 不洩漏 context content 或 secret。

---

## 8. Skill progressive disclosure 完整化

### 8.1 Disclosure levels

| Level | 內容 | 使用條件 |
| --- | --- | --- |
| 0 | name | skill index、manifest |
| 1 | name、summary、path | task dispatch |
| 2 | 依 phase、trigger、section heading 選出的 bounded relevant sections | JIT hint 或 bounded load |
| 3 | full content | 成功的 `load_skill` 或 deterministic fallback |

assigned、forced、auto-selected skill 與其 dependencies 必須使用同一條 resolution、ordering、filter 與 disclosure pipeline。

### 8.2 Runtime enforcement

當 manifest 宣告 mandatory skill load 時：

- runtime 必須在任何 task-work tool 前要求完成相應 `load_skill`；
- mandatory skills 必須依 deterministic dependency order 載入；
- closed tool sequence 必須預留並驗證 skill-load slots；
- `load_skill` 不在 resolved tool set 或 sequence 不允許時，compiler 注入 required full-content fragment；
- worker 未完成 mandatory load 就呼叫其他工作工具時，回傳可恢復的 policy error，不中止 model round；
- `InjectedSkills`、`LoadedSkills`、`skill_used` event 與 manifest disclosure level 必須一致；
- summary disclosure 不得記錄為已完整載入。

### 8.3 驗收

- 初始 prompt 在可 JIT load 時不含完整 skill content；
- worker 第一個 task-work tool 前必須載入所有 mandatory skills；
- dependency order deterministic；
- filtered/missing dependency 在 model call 前失敗；
- full fallback 保證完整 instructions 可見；
- DAG/direct/nested/extra-model 行為一致；
- manifest 記錄 disclosure level 且不保存 skill content。

---

## 9. VERIFY-specific context projection

### 9.1 Required sources

`PhaseVerify` request 必須優先且明確建立以下 typed fragments：

- task goal；
- acceptance/verification criteria；
- dependency typed results；
- artifact opaque references；
- files modified；
- execution receipt references；
- verifier command/exit/fingerprint；
- unresolved findings、risks 與 questions；
- runtime phase/capability contract。

### 9.2 Source restrictions

- raw shell output、raw verifier stdout/stderr與完整 transcript 不得進 prompt；
- 需要原始資料時只提供 authorized opaque artifact/transcript reference；
- progress chatter 不得成為 required context；
- generic STM/LTM 必須具有符合 VERIFY 的 activation eligibility 才可注入；
- EXECUTE-only memory 在 VERIFY 必須以 `phase_mismatch` omit；
- historical content 保持 historical authority，不得覆蓋 verification contract；
- 模型 prose 不得取代 objective verifier 或 phase state machine 的成功判定。

### 9.3 驗收

- VERIFY worker 看得到 criteria、artifacts、modified files、receipt 與 verification evidence；
- generic 或 EXECUTE-only history 不進 VERIFY prompt；
- raw transcript/output 不進 compiled prompt、manifest、report；
- required verification fragment overflow 在 model call 前 fail closed；
- PREPARE/AUDIT/EXECUTE/VERIFY integration tests 全部覆蓋。

---

## 10. Tool-failure JIT context

### 10.1 JIT tools

新增 model-visible tools：

- `context_query`：依目前 request/phase/trigger 查詢 bounded context index；
- `context_get`：以 authorized opaque context ID 取得 bounded內容。

工具必須通過：

- central tool policy gate；
- model-visible tool resolution；
- execution-time authorization；
- unattended allowlist；
- force-MCP restrictions；
- phase/capability grants；
- closed tool sequence preflight；
- context item scope/lifecycle/authority authorization。

不得允許模型用任意路徑、Todo ID 或未授權 reference 讀取 context。

### 10.2 Tool failure next-turn injection

central tool wrapper/stream 收到 failed tool result 後必須：

1. 分類 error、exit code 與 component；
2. 建立 `tool_failure` ContextRequest；
3. route failure-specific context；
4. redaction、dedup、budget；
5. persist manifest/event；
6. 將 bounded recovery annotation 放入下一個 model turn。

不得修改已完成的 model decision，不得把 `before_tool_call` hook 當成 prompt injection，不得隱藏 retry、切換替代工具、重播 side effect 或繞過 action receipt。

### 10.3 Evidence 與限制

- tool input 只保存 hash；
- tool output 只保存 bounded/redacted summary 或 opaque transcript ref；
- context tool output 必須有 token/rune upper bound；
- 每次 query/get 與 automatic next-turn injection 都需 audit/event/manifest；
- recoverable authorization error 必須回到模型作為 tool result，保留 attempt evidence。

### 10.4 驗收

- failed SSH/bash/MCP call 的下一 turn 能取得符合 tool/error activation 的 context；
- 成功 tool call 不觸發 failure context；
- unattended、force-MCP、closed sequence 的 allow/deny 測試；
- secret、raw input/output 不出現在 durable sinks；
- context tools 無法跨 scope、run、worker 或 artifact authorization boundary；
- tool failure 不造成 side effect replay。

---

## 11. Activation schema、outcome attribution 與 policy optimization

### 11.1 Typed schema 與索引

將 activation metadata 映射到 repository typed schema，至少包含：

- phases；
- triggers；
- roles；
- capabilities；
- tools；
- error classes；
- environment。

migration 必須：

- 冪等且可重跑；
- 保持既有 metadata rows 可讀；
- 不改變 context item identity/content hash；
- 建立 routing 所需索引；
- 支援 mixed-version repository；
- 提供 migration/reopen/rollback 測試。

### 11.2 Outcome observation

selection、use 與 outcome observation 的 key 至少包含：

```text
context_item_id × phase × trigger × agent_role × environment
```

每筆 observation 必須能連回：

- request/manifest；
- task/attempt/model execution；
- verification result；
- acceptance outcome；
- skeptic/guard/judge signal；
- failure attribution。

只有 manifest 中實際 included 的 context item 可取得 exposure/use attribution。

### 11.3 Policy lifecycle

optimizer 必須產生 immutable candidate policy，並依序執行：

1. observations aggregation；
2. candidate policy generation；
3. shadow comparison；
4. deterministic acceptance gate；
5. explicit adopt；
6. active policy application；
7. rollback 到上一 revision。

optimizer 不得直接覆寫 active policy。adopt 與 rollback 必須留下 event、policy revision、snapshot hash 與 verification evidence。

### 11.4 驗收

- phase/trigger/role/environment outcome attribution 正確；
- omitted item 不計為 exposure；
- failed run 與 unverified prose 不產生正向 adoption signal；
- shadow policy 不改變 prompt selection；
- active policy 只有通過 acceptance gate 後才影響 routing；
- adopt/rollback 可由 event replay 重建；
- legacy policy snapshot 與 mixed schema repository 可讀。

---

## 12. Auxiliary LLM 全面納管

### 12.1 必須納管的模型路徑

- sidecar task；
- skill matcher；
- guard tool-call reviewer；
- path reviewer；
- plan reviewer；
- extra-model judge；
- skeptic；
- reflection；
- protocol/result repair；
- context/project compacter 中實際呼叫模型的路徑。

### 12.2 Purpose-specific source allowlist

| Purpose | Required context | 禁止來源 |
| --- | --- | --- |
| skill match | goal、agent role、skill name/summary index | full skill content、raw history |
| guard review | guard rules、agent、tool name、bounded/redacted arguments | unrelated STM/LTM、worker transcript |
| plan review | goal、constraints、plan revision、acceptance criteria | unrelated task chatter |
| judge | selection contract、candidate identities、bounded candidate outputs | worker private memory、unrelated STM/LTM |
| skeptic | goal、criteria、candidate output、artifact/verification refs | raw transcript、unrelated history |
| reflection/repair | approved failure class、bounded evidence、mutable fields、recovery disposition | unapproved evidence、completed side-effect replay instructions |
| compacter | explicit bounded source items與 compaction contract | ambient session history |

### 12.3 Failure semantics

- guard reviewer failure、router failure、compiler failure與 manifest persistence failure維持 fail closed；
- judge/skeptic 的既有 fallback policy 必須明確記錄於 fallback manifest；
- deterministic fallback 必須可與「模型有呼叫但 context 為空」區分；
- repair 不得擴張工具權限、phase capability 或 artifact access；
- auxiliary token usage 必須計入既有 no-progress/token budget。

### 12.4 Chokepoint audit

建立自動化測試或 static audit，列舉 repository 中所有：

- `runAgentWithStatusAndHistory`；
- sidecar `Execute` / `ExecuteProfile`；
- `MatchSkills`；
- `ReviewToolCall` / `ReviewPathAccess`；
- compacter/model generate entry point；
- 等價的 provider model call。

每個 chokepoint 必須能對應到 request、compiled prompt、manifest 與 persistence boundary，或有明確的 deterministic no-model test。

### 12.5 驗收

- 每一 auxiliary model call 都有 purpose-specific request/manifest；
- judge/skeptic/guard allowlist tests 阻止 unrelated STM/LTM 與 raw transcript；
- no-sidecar fallback 產生 fallback manifest；
- guard failure保持 deny；
- auxiliary manifests 可由 event/session/branch replay 還原；
- JSON/report/TUI 可區分 purpose、called/fallback、included/omitted counts。

---

## 13. Required test matrix

### 13.1 Unit

- 所有 ContextRequest trigger validation/query/fingerprint；
- activation parse/match/schema migration；
- lifecycle/scope/expiry/failed-run visibility；
- compiler fragment authority/conflict/dedup/budget；
- general manifest identity、reason merge、secret redaction；
- skill disclosure levels、dependency order、mandatory load enforcement；
- verify-specific source allowlist；
- JIT context tool authorization與 bounds；
- policy observation、candidate、adopt、rollback。

### 13.2 Coordinator integration

- normal DAG worker；
- direct-agent；
- nested `request_agent`；
- concurrent extra-model workers與 judge；
- plan-only與 approved-plan execution；
- PREPARE、AUDIT、EXECUTE、VERIFY；
- retry after tool failure；
- retry after objective verify failure；
- cancelled/timeout/budget/permission-denied attempt；
- protocol-incomplete result repair；
- crash-resume與 non-replay；
- unattended、force-MCP、closed tool sequence；
- repository unavailable/degraded；
- sidecar/skill match/guard/plan review/judge/skeptic/reflection/repair。

### 13.3 Persistence與 projection

- Todo/session checkpoint；
- execution receipt；
- event reducer replay；
- branch fork/checkout/time-travel；
- task journal；
- projection shadow；
- JSON output；
- markdown report；
- TUI reporter/Update/detail log；
- general manifest memory subset與 outcome attribution。

---

## 14. Validation gate

所有實作完成後必須執行：

```bash
gofmt -w <changed-go-files>
go test ./...
go vet ./...
golangci-lint run
```

目前已知必須修正的 lint 問題：

- `CompileWorkerContext` cyclomatic complexity 超過門檻，需拆分 typed fragment/source collection helpers；
- `internal/team/context_router.go` 必須通過 gofmt；
- `internal/team/coordinator_task_run.go` 的初始 `retrievalQuery` assignment 無效，需移除或改成實際使用的單一賦值。

完成報告必須列出：

- execution-path coverage matrix；
- request/manifest schema versions；
- persistence/replay 變更；
- required/optional omission 行為；
- tool policy 與 recovery invariants；
- 實際執行的 validation commands 與結果；
- 所有測試矩陣項目的證據位置。

當所有章節的驗收條件與 validation gate 全部通過時，本規格即告完成。
