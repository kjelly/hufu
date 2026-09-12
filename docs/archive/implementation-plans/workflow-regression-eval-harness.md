# Workflow Regression Eval Harness 實作計畫

> Status: implemented and archived
> Priority: P2（對快速演進的 hufu 屬高價值）
> Baseline: `ae47210a01b80cba96fe9095351ed4d4f4223a14`

## 0. Implementation status (archived: implementation landed)

Runner、assertion DSL、10 個 critical semantic cases、1 個 core lifecycle
baseline 與 blocking CI gate 均已完成。2026-09-12 的 completion audit 亦補齊
原先漏掉的 terminal lifecycle、task verification、BackendBinding、禁止未授權
fallback、event cardinality/fields、EvidenceManifest/CAS hash、artifact membership、
`auditverify` 重播與 improve benchmark revision。主要實作 commit：`d7406dd`、
`f8515ae`、`bdc2726`、`643bc51`、`11c2f07`、`c8c849c`、`0714055`、
`c55fbd2`、`5cc6ba9`、`f19b12f`、`f4947eb`、`2808f6f`、`aa0ce10`、
`e0036fe`、`89bc526`、`d4bd8c6`；non-blocking
CI 起點為 `47d25b7`，本次封存同時將 `eval-core` 納入 `ci-success.needs`。
此文件保留為設計與實作歷史；目前程式碼、tests 與 CLI help 為準。

## 1. 目標

建立 offline deterministic workflow regression harness：

```bash
hufu eval run ./evals/runtime-core
```

目標不是測模型智商，而是捕捉 runtime semantic regression：

```text
routing
task lifecycle
verification
retry
recovery
acceptance
events
artifacts
outcome
```

## 2. 重用既有元件

不要重建，且每項已核對對應的既有型別/函式（確保開工時不需要重新探勘）：

- improve benchmark fixture — `internal/improve/experiment.go`: `BenchmarkFixture`/`CreateBenchmark`/`LoadBenchmark`。
- baseline/candidate snapshot — 同檔 `TeamSnapshot`/`CreateBaselineSnapshot`/`LoadCandidateSnapshot`/`EvaluateExperiment`。
- experiment compare — 同檔 `EvaluateExperiment`/`WriteExperimentReport`。
- RunResult — `internal/team/run_result.go`。
- EvidenceManifest（含 hash 驗證）— `internal/team/evidence_store.go`；`(*EvidenceManifest).Verify` 的呼叫範例見 `internal/team/canonical_run_snapshot.go`。
- audit verify — `internal/auditverify`（`verifier.go`/`event_verify.go`）。
- team loader — `internal/team/parse.go` 的 `LoadTeam`。
- **deterministic judge**（真的可直接重用）— `JudgeRunner` 介面（`internal/team/decision_engine.go:42`）＋兩種既有 scripted 實作：`recordingRunner`（in-process，`internal/team/decision_engine_test.go`，8+ 個 test 檔重用）與 `fakeJudge`（HTTP-level，依 stage prompt 分類回應的 `net/http.Handler`，`internal/team/decision_fake_judge_test.go`，經 `classifyDecisionStagePrompt` 辨識 options/judge/challenge/premortem/revision/reference/finalization 七個 stage）。
- **fake provider（修正）**——原文寫「fake provider ... infrastructure」暗示有現成共用元件可套，查證後不成立：`Coordinator.workerAgentOverride`/`repairAgentOverride`（`internal/team/coordinator.go:777-780`，明確標註為 "deterministic integration-test seam"）是 unexported 欄位，40+ 個 test 各自手刻自己的 `fantasy.Agent` 假實作，從未收斂成一個可重用元件，且因為是 unexported，`internal/evalharness`（外部套件）本來就無法直接設定它們。本計畫改採「HTTP-level fake provider」路線重用既有可行模式：`fixedAnswerProvider`（`internal/team/decision_reference_capability_runner_test.go`，回傳 OpenAI-相容 SSE `chat.completion.chunk` 的 `net/http.Handler`，掛在 `httptest`-style server 上，由 `agent.TeamConfig`/`team.NewCoordinator(...)` 的 provider URL 參數指過去）才是真正可跨套件重用、且已有多處驗證過的手法。詳見第4節。

### 2.1 與既有 HF-AR-006 ReliabilityEvalSuite 的關係

`internal/team/reliability_eval.go`（HF-AR-006，來自已 archive 的 `docs/archive/implementation-plans/autonomous-self-correction.md`）已經是一個 fault-injection replay harness：`ReliabilityScenario` → `FaultInjection` → 直接餵給 diagnosis policy → 斷言 `ExpectedDisposition`（`RetryDisposition`）。它完全不經過 prompt/model call，只測 retry/recovery 決策層本身。

本計畫的新 harness 是更高一層：從 prompt 出發，跑完整 team run，斷言 `RunResult`/events/artifacts/outcome。兩者範圍不同、不衝突，**但第11節「首批必測 cases」有幾項在概念上與 HF-AR-006 的既有 scenario 重疊**（見該節註記）。新 harness 對這些重疊 case 的目的是「確認同一行為在完整 pipeline 中可被觀察到」（integration-level cross-check），不是重新推導 policy 正確性——policy 正確性已由 HF-AR-006 覆蓋，不要在 evals/ 裡重造一份等價的 fault-injection 測試。

## 3. Fixture

```yaml
version: 1
name: retry-recovery-smoke
team: hufu-dev
mode: deterministic
cases:
  - id: verifier-failure-retry
    prompt: "..."
    provider-fixture: verifier-retry.json
    expect:
      run-outcome: completed
      acceptance: passed
      task-count: 1
      events:
        required: [task_created, verification_completed]
```

Prompt可存在 Git fixture，但不得進一般 telemetry/report raw payload。

## 4. Deterministic provider driver

**設計基礎改為 HTTP-level fake provider，不是 Coordinator 內部 override。** 原因見第2節：`workerAgentOverride`/`repairAgentOverride` 是 `internal/team` 的 unexported 欄位，`internal/evalharness` 作為外部套件無法設定它們；而 HTTP fake（`fixedAnswerProvider`/`fakeJudge` 手法）只需要把 `agent.TeamConfig` 的 provider URL 指向本地 `httptest.Server`，對外部套件完全可行，且已有多處先例證明可行。這代表 **v1 不需要改動 `internal/team` 任何一行**——只要 `internal/evalharness` 在本地起一個 HTTP handler 並把它的 URL 傳給既有的 `team.NewCoordinator(...)`/`agent.TeamConfig` 建構參數即可。

```go
// scriptedProvider (internal/evalharness/provider_fake.go) serves scripted
// OpenAI-compatible chat completions over HTTP, following internal/team's
// fixedAnswerProvider/fakeJudge pattern. One instance backs every model
// call -- worker, judge, and coordinator alike -- for a single case; a case
// gets its own fresh instance (no cross-case Reset).
type ProviderStep struct {
    Match    *StepMatch    // optional: substring match; nil = arrival order
    Content  string        // plain assistant text (finish_reason: stop)
    ToolCall *ToolCallStep // a tool call (finish_reason: tool_calls); exactly one of Content/ToolCall is set
}

type ToolCallStep struct {
    Name      string // e.g. "agent", "submit_result", "finish"
    Arguments string // the exact JSON argument-object text, embedded verbatim
}
```

blocking CI只用 scripted fake responses（`scriptedProvider` 背後的 `httptest.Server`）。
live Ollama/Codex可 opt-in（把同一個 driver 換成真實 provider URL），但不當 acceptance dependency。

### 4.1 Provider-fixture schema（PR-1 實作定案，取代原草稿）

`provider-fixture: single-task-unverified.json`（第3節 YAML 的 `provider-fixture` 欄位）指向的 JSON 檔案結構——原草稿的 `{"response": "..."}` 太簡化，實作後改為 `content`/`tool_call` 兩種 step，因為協調者的委派/完成動作本質上就是 tool call，不是純文字：

```json
{
  "steps": [
    { "tool_call": {"name": "agent", "arguments": "{\"tasks\":[{\"agent\":\"worker\",\"goal\":\"...\"}]}"} },
    { "tool_call": {"name": "submit_result", "arguments": "{\"status\":\"success\",\"summary\":\"...\"}"} },
    { "tool_call": {"name": "finish", "arguments": "{\"response\":\"...\"}"} },
    { "content": "(run ends here)" }
  ]
}
```

**關鍵、非顯而易見的協定細節**（PR-1 實測驗證，直接抄會踩坑）：

- 協調者的委派工具叫 `agent`（不是 `create_task`/`assign_task`），schema 是 `{"tasks":[{"agent":"<worker>","goal":"<text>"}]}`，且 `additionalProperties:false`——只能給 `agent`/`goal` 兩個欄位。
- worker 的 `submit_result` 必須同時給 `status`（enum，`"success"` 可用）與 `summary`；只給 `status` 會被 schema 拒絕。
- 協調者呼叫 `finish`（`{"response":"..."}`）之後，**runtime 仍會再打一次 model 請求**（底層 fantasy agent step loop 直到模型回傳「無 tool call」的一輪才會自然結束；`finishCalled` 旗標只影響外層是否強制 wrap-up，不會讓當前 step loop 提前停止）——所以每個 case 的 provider-fixture 最後都要多一個純文字 `content` step 讓 loop 收尾，否則最後一輪會撞到「no unconsumed step」。
- 要讓協調者/worker 解析出 `ExecutionBackend`，team 內每個 agent 的 frontmatter 都必須顯式給 `model:`（team.yaml 沒有 team 層級預設 model 欄位）；沒給的話會在 `resolve coordinator execution backend` 階段直接失敗。

沒有 `match` 的 step 依到達順序消費（對應 worker/repair 呼叫）；有 `match` 的 step 依 request body 內容比對（對應 judge/decision stage，沿用 `classifyDecisionStagePrompt` 的 marker 比對方式，見 `internal/team/decision_fake_judge_test.go`）。兩種比對可以混用同一個 fixture，driver 依序嘗試「有 match 的先比對，沒比對到才退回到到達順序佇列」。

### 4.2 範圍界線（v1 不做）

`workerAgentOverride`/`repairAgentOverride` 能做到 HTTP fake 做不到的事（例如精確控制單一 tool-call 序列、模擬畸形/非 JSON 回覆）。v1 的 10 個 critical cases 不需要這些能力，所以不在 `internal/team` 加任何 exported hook。若之後某個 suite 真的需要，屆時再評估是否值得新增一個 exported 建構選項——不要為了「以防萬一」現在就加。

### 4.3 Provider hard-abort boundary（PR-1 踩坑記錄）

`team.NewCoordinator` 建出的 Coordinator 在第一次呼叫模型前，一律會呼叫 `os.Executable()` 並把自己 re-exec 成一個 provider hard-abort boundary 子行程（`internal/team/provider_boundary.go`）。production 情境下 `os.Executable()` 解析到 `hufu` 本體，`cmd/hufu/main.go` 已經知道處理 `providerproxy.ChildArg`；但在 `go test` 底下 `os.Executable()` 解析到的是測試二進位檔本身，不知道怎麼變成 proxy child，會直接失敗成「provider proxy exited before readiness」。

修法：`internal/evalharness/main_test.go` 加一個 `TestMain`，在呼叫 `m.Run()` 之前攔截 `os.Args`，比對到 `providerproxy.ChildArg` 就呼叫 `providerproxy.RunChild(os.Stdin, os.Stdout)` 並結束——完全比照 `internal/team/main_test.go` 攔截 fake-Codex-app-server re-exec 的手法。這只影響 `go test`；`hufu eval run` 本身用真正的 `hufu` binary 執行時不需要這個攔截。

## 5. Assertion dimensions

### Run
```text
outcome
stop reason
acceptance
terminal lifecycle
```

### Task
```text
count
agent binding
attempts
status
verification
failure class
```

### Execution
```text
ExecutionTarget
BackendBinding
no unauthorized fallback
```

### Evidence
```text
manifest exists/hash
required evidence
artifact refs
```

### Event
只比 critical type/order/cardinality/fields，不 golden 整個 timestamp/id JSON。

實作 DSL 使用 `required`、`order`、`counts` 與 `matches`；`matches` 以事件
type + 1-based occurrence 選取，再以 dotted path 比對明列欄位。Task/evidence
關聯一律用建立順序的 `task-index`，不把 opaque Todo ID 寫死進 fixture。
`no-unauthorized-fallback` 會 fail-closed 比對 immutable `ExecutionTarget`、
mutable `BackendBinding` 與每個 `ExecutionReceipt.Backend`。

Evidence assertions 直接呼叫 `EvidenceManifest.Verify` 與 workspace 的
`FileArtifactStore`；`audit-verdict` 再透過 `auditverify.VerifyWorkspaceRun`
獨立驗證 event hash chain、terminal projection、receipt provenance、evidence、
acceptance 與 completion justification，沒有重寫第二套判定器。

## 6. Canonical result

```go
type EvalCaseResult struct {
    CaseID string
    Passed bool
    RunOutcome string
    Findings []EvalFinding
    Metrics EvalMetrics
}
```

Finding必須列 expected/actual/dimension。

## 7. CLI

```bash
hufu eval list
hufu eval run ./evals/core-lifecycle
hufu eval run ./evals/core-lifecycle --case off-profile
hufu eval run ... --format json
```

第一版不做一鍵 update golden。

## 8. Suites

```text
evals/core-lifecycle/
evals/retry-recovery/
evals/execution-target/
evals/decision-runtime/
evals/memory-learning/
evals/workset/
evals/strict-verification/
```

## 9. 檔案

新增（v1 不改動 `internal/team`/`internal/improve`/`internal/auditverify` 任何既有檔案，純新增）：

```text
internal/evalharness/types.go
internal/evalharness/runner.go
internal/evalharness/provider_fake.go   # 第4節 scriptedProvider：httptest handler + 第4.1節 fixture schema 解析
internal/evalharness/assert.go
internal/evalharness/report.go
internal/evalharness/fixtures.go
internal/evalharness/runner_test.go
internal/evalharness/fixtures_test.go
internal/evalharness/main_test.go       # 第4.3節：go test 下攔截 provider hard-abort boundary re-exec
cmd/hufu/evalcmd.go
evals/core-lifecycle/team/{team.yaml,coordinator.md,worker.md}
evals/core-lifecycle/cases.yaml
evals/core-lifecycle/fixtures/single-task-unverified.json
```

## 10. PR

### PR-1 Runner + one core lifecycle case — done

`ae47210`（baseline）之後實作。證據：`internal/evalharness/`（第9節檔案清單前9項）、`evals/core-lifecycle/`、`cmd/hufu/evalcmd.go` + `cmd/hufu/root.go` 掛載 `eval` 子指令。`go build ./...`/`go vet ./...`/`golangci-lint run`/`go test ./internal/evalharness/... ./cmd/hufu/...` 全過；`hufu eval run ./evals/core-lifecycle` 與 `hufu eval list ./evals/core-lifecycle` 手動驗證過真的呼叫進 `team.Coordinator.Run` 並產出正確 `unverified` 結果（非 mock）。

### PR-2 Assertion DSL — done

新增 `ExpectSpec.Tasks`（依 TodoList 建立順序逐位比對 agent/status/failure-class，不比對 opaque TodoItem.ID——它本來就只是遞增序號，見 `types.go` `TaskExpect` 註解）與 `EventsExpect.Order`（子序列相對順序檢查，允許其他事件穿插，呼應第5節「Event」只比 critical type/order，不 golden 整個 JSON）。新增 `normalizeOpaqueID`（redact `RunID` 的 timestamp+random 尾碼，讓同一個 deterministic case 兩次執行的 JSON report 逐位元組相同）與可覆寫的 `defaultCaseTimeout`（`60s` 預設，per-case，由 `context.WithTimeout` 包住 `coordinator.Run`）。`evals/core-lifecycle/cases.yaml` 的既有 case 已擴充 `tasks:`/`events.order:` 斷言驗證真的生效。第14節六個 runner 測試全部補齊：`TestEvalFixtureStrictDecode`、`TestEvalEventAssertionOrdering`、`TestEvalNormalizesOpaqueIDs`、`TestEvalTimeout`（用一個永不回應的 handler 驗證 per-case timeout 真的會在 200ms 內截斷，不是靠等滿 60s）、`TestEvalNeverCallsNetworkDriverInOfflineMode`（斷言 fixture team 不得宣告自己的 `providers:`，因為那會繞過 harness 唯一認可的 httptest 出口）、`TestEvalResultJSONStable`。`go build`/`go vet`/`golangci-lint`/`go test ./internal/evalharness/... ./cmd/hufu/...` 全過。

### PR-3 Critical suites — done

新增 `evals/strict-verification/`（team.yaml 帶 `acceptance: {commands: ["false"]}`，其餘同 core-lifecycle 手法），完成第11節「acceptance failure → non-completed」：worker 正常 `submit_result`/task 變 done，但 `finish` 自動觸發的 acceptance 檢查跑 `false`（deterministic、離線、無網路）失敗，驗證得到 `run-outcome: partial`/`acceptance: failed`——已用真實 CLI（`hufu eval run ./evals/strict-verification`）與 `go test` 雙重驗證，非猜測。`runner_test.go` 改為 `TestRunAllEvalSuites`：自動探索 `evals/` 下每個 suite 目錄並執行，之後新增 suite 不需要每次手寫新 test function。

新增 `evals/decision-runtime/`，完成「off-profile compatibility」：team.yaml 設定非 off 的 team-level `decision.default-profile`（連帶一個滿足設定驗證最低要求的 `independent-judgments: 1` profile——光是空 `{}` 會被 `decision.profiles.X: independent-judgments must be >= 1` 擋掉，這點文件原本沒提到），case 用新增的 `decision-profile-override: "off"`（映射到 `CaseFixture.DecisionProfileOverride` → `coordinator.SetDecisionProfile(...)`，run-scoped、precedence 最高層——見 `internal/team/decision_dispatch.go:22-27`）蓋掉它，驗證 override 真的讓任務繞過 decision engine、行為與 core-lifecycle 基準完全一致。**已實測確認這不是假陽性**：拿掉 override 重跑同一個 case，行為確實不同（delegate 呼叫本身就因為 decision profile 要求 durable event journal/request contract 而提前出錯，4 步 script 只消耗 1 步）——證明這個 case 真的在測 override 的 precedence，不是自動通過。

### ✅ 這個 harness 抓到一個真的 internal/team bug——2026-09-12 已修復並驗證

`evals/retry-recovery/`「verifier failure → retry」原計畫：worker `verify` 失敗一次觸發 retry，第二次 `submit_result` 補上正確結果後任務變 done。查證過程：

1. `verify_spec`（`task_result_assert`）不在 coordinator `agent` 工具的 model-facing schema 白名單（`portableProviderTaskFields`，`internal/team/coordinator_tools.go:125-141`，只有 legacy `verify`/`verify_mode`）——傳了會被 JSON schema 擋成 `"additional property"`，不會走到 Go 層的 `decodeModelTaskDefs`。改用合法的 legacy `verify: "exit 1"`。
2. 改用後，**第二次**（retry 之後）的 `submit_result` 一律失敗：`"invalid submit_result runtime identity: submit_result runtime identity is missing or invalid"`，`failure_class: protocol`，最終 `run-outcome: blocked`。追到根因：`submitResultRuntimeIdentityFromContext`（`internal/team/coordinator_tools_result.go:455`）有兩條路徑——主路徑靠 `ctx.Value(submitResultRuntimeIdentityKey{})`；當它不存在時走 fallback（用 `invocationMetadataFromContext` 組出一個 identity），但這個 fallback 組出來的 identity **永遠不會設定 `OccurrenceRevision`/`DispatchID`**，而 `validSubmitResultIdentity` 要求兩者都非零/非空——也就是說這條 fallback 分支結構上不可能成功，任何時候主路徑的 identity 缺失，fallback 保證同樣失敗。
3. 派 Explore agent 直接跑 `evals/retry-recovery`（此 harness 自己的 fixture）確認：這是可重現的真 bug，不是我的 harness 少做了什麼初始化——`NewCoordinator`+`FreezeExecutionPolicyAtStartup`+`SetStatusReporter`+`Run` 這套序列就是 production 也在用的路徑，且目前 codebase 裡沒有任何既有測試真的走過「全端 HTTP-scripted retry-then-resubmit」這條路（呼應 2026-09-12 稍早的查核：`internal/team/*_test.go` 沒有任何測試把 `httptest` fake 跟 `coord.Run` 接在一起），這正是本文件開頭說要蓋的那類「escaped unit tests 的 kernel semantic regression」。

**根因確認**（用戶要求後續追查）：在 `submitResultRuntimeIdentityFromContext` 與 `coordinator_task_run.go` 的 retryLoop 加臨時 debug print 逐步定位，精確鎖定成因：`retryLoop` 每個 iteration 一開始呼叫 `c.setCurrentTaskAttempt(todoID, attempt)` 開一個新 occurrence（實測確認此刻 `activeTaskResultOccurrence` 回傳 `ok=true`、identity 合法）；但緊接著，`attempt > 1` 分支呼叫 `c.commitTaskTransitionFromCurrent(...)` 把任務標記回 `TaskInProgress`，這會走到 `CommitTaskTransition`（`coordinator_eventstore.go:753`），而它**無條件**把 `OccurrenceRevision` 加一並呼叫 `c.revokeTaskOccurrence(taskID)`（`coordinator_eventstore.go:847`）——目的是讓「舊 projection 的過期 worker lease」失效，但因為呼叫順序是「先開新 occurrence、才標記 in-progress」，這個 revoke 誤殺的是**這個 attempt 自己剛開的新 occurrence**，不是真正過期的那個。等到 closure 在後面呼叫 `withSubmitResultRuntimeIdentity` 前再查一次 `activeTaskResultOccurrence`，就查到 `ok=false`，只好落到那個結構上必敗的 fallback。

**修法**：在 `commitTaskTransitionFromCurrent` 成功之後，`reconcileTaskStatusProjection()` 之前，多呼叫一次 `c.setCurrentTaskAttempt(todoID, attempt)` 把 occurrence 重新開好（`internal/team/coordinator_task_run.go`，新增12行，純加法、不改動任何既有邏輯順序或簽名）。已完整驗證安全性：
- `evals/retry-recovery` 現在跑出**原本設計要驗的正確行為**：worker 兩次都能成功 `submit_result`，`verify:"exit 1"` 每次都真的重跑並失敗，retry 用盡後正確分類成 `run-outcome: partial`/`tasks[0].status: error`/`failure-class: verification`（不再是假的 `blocked`/`protocol`）。
- `go test ./internal/team/... -short`：全過。
- `go test ./internal/team/... -race -short`：**4751 個測試全過**，涵蓋整個既有測試套件，包含 occurrence controller 的並行安全性。
- `go test ./... -short`：整個 repo 全過。
- `golangci-lint run ./internal/team/...`：無新增問題。

`evals/retry-recovery/cases.yaml` 的 comment 已從「known-bug pin」改寫成記錄根因與修復方式的說明；`verifier-failure-retry.json` 少了 2 個多餘的 filler content step（修復後流程少走兩輪）。

新增 `evals/execution-target/`，完成「ExecutionTarget frozen across retry」：`ExpectSpec` 加一個目的明確的新斷言維度 `execution-target-frozen: true`（不是通用 event-field-diff DSL——刻意選擇最小、專用的實作，因為目前只有這一個 case 需要跨 event 比對欄位值），檢查整個 run 期間每個帶有非空 `StatusEvent.ExecutionTarget` 的事件是否全部同值。Case 讓 worker 第一次 attempt 因 `verify:"exit 1"` 失敗觸發 retry，第二次 attempt 故意不呼叫任何 tool（觸發 `no_tool_call` protocol failure，快速走到穩定終態，避開 `verifier-failure-retry` 那個 known bug 的 identity 路徑），只驗證兩次 attempt 之間 ExecutionTarget 是否凍結——已實測確認橫跨 retry 邊界的 13 個事件（`verify_error` 之前與之後都有）全部回報同一個 `ollama/eval-harness-model`，不是只有 0 或 1 個事件的假陽性。

（附帶一提：跑這幾個 suite 時穩定看到一則無關的既有 log 警告 `execution-event shadow export parity: execution events length mismatch: legacy=6 exported=5`——`internal/team/execution_events.go:458`，非 fatal、非本計畫斷言範圍，記錄下來但不深追。）

新增 `evals/decision-runtime/challenge-revise-routing`，完成「decision challenge/revise routing」。查證過程一路踩坑（都已修正，非猜測）：

1. `DecisionOptions` 是 `json:"-"`——coordinator `agent` 工具的 live tool_call 本來就無法帶 options，這點原始查核已確認。改用 team.yaml 的 `decision.default-profile`（非 off）+ `option-proposal.enabled:true`，讓 decision engine 自己跑 options-proposal stage，不需要 model 提供 options。
2. 需要 `request-contract.enabled:true`，且底下 `objective`/`success-criteria` 都是必填（否則 config 驗證直接擋掉，文件原本沒提到）。
3. 需要 `RoleModels.Judge` 設成跟 worker 同一個 model（否則直接 `decision_budget_insufficient: ... needs a judge model and none is configured` 失敗）——已加進 `runner.go`，對其餘不啟用 decision 的 case 完全無害。
4. **修了 internal/agent 的第二個真 bug**：`DecisionPolicy.MaxTokens`（`internal/agent/decision_config.go:189`）沒有 `json` 標籤，encoding/json 退回用 Go 欄位名 `"MaxTokens"`；event-journal 的通用 redaction pass（`internal/utils/redact.go`）的 key 比對是大小寫不敏感抓 `token` 字樣（信不信任一個 field 的依據是查 `numericTelemetryKeys["max_tokens"]`——注意底線），`"maxtokens"`（去掉底線後小寫）對不上 `"max_tokens"`，導致這個完全無關的數值欄位被誤判成憑證、整個 redact 成字串，之後 `json.Unmarshal` 回 `int64` 就炸開。同一個欄位在姊妹 struct `StopPolicy`（`decision_config.go:557`）就有正確的 `json:"max_tokens,omitempty"`，證明這是單純漏標，不是設計如此。修法：補上缺的 json tag。這個 bug 會影響**任何**啟用非 off decision profile 且底層走真實 EventStore 的正式使用場景，不是 harness 特有——跟 submit_result 那個 bug 一樣，是「這個 harness 第一個真的走過這條路」才浮現。
5. `sidecar`（decision engine 用來呼叫 options/judge/challenge/revision 的 client）送的是 **non-streaming** request（body 沒有 `"stream":true`），跟 worker/coordinator 走的 streaming SSE 完全不同協定；用 SSE 回應會直接炸 `expected destination type of 'string' or '[]byte' for responses with content-type 'text/event-stream'`。**這是 harness 自己的能力缺口，不是 internal/team 的 bug**：`provider_fake.go` 加了 `requestWantsStream(body)`，依請求的 `"stream"` 欄位決定回 SSE 還是純 `application/json` 的 `chat.completion`。
6. `Revision` 即使 `enabled:true`，`policy.EffectiveMaxRounds() < 2` 時永遠不會跑（`decision_engine_stages.go:269`）——`max-rounds` 預設 0 換算成 1。要真的測到 revision 必須顯式設 `max-rounds:2`。
7. Revision 的 `revised_scores` 必須覆蓋 judge 評過的**每一個** option，漏一個會報 `option "X" was not scored`。

新增 `evals/workset/workset-partial-blocks-completion`，完成「workset partial blocks completion」。查證過程：

1. `fan_out`（跟 `verify_spec`、`DecisionOptions` 一樣）**不在** coordinator `agent` 工具的 model-facing JSON schema 白名單內，live tool_call 帶了會被擋成 `"additional property"`——這點原始查核只驗了 Go 層 `decodeModelTaskDefs` 不擋，漏查了 JSON schema 驗證這一層，實測後才發現。改用 team.yaml 靜態 `tasks:` 清單 + `delegation.bind-task-goal-contracts:true`（`CompileTaskGoalContracts`，`internal/team/contract_compile.go:114`）：coordinator 活的委派只給 `{"agent":"worker","goal":"Process the workset."}`（不含 fan_out），runtime 依 `agent` 名稱 + `when-goal-contains` 子字串（或「這個 agent 只有一個 contract」的 fallback）自動把靜態 contract 的 `fan_out` 併上去——不需要 model 知道 fan_out 存在。
2. `fan_out.source` 是相對於 `session.Workspace` 的檔案路徑，必須真實存在——加了新的 harness 能力 `CaseFixture.WorkspaceFiles`（`runner.go` 的 `seedWorkspaceFiles`），在建 Coordinator 前把檔案寫進暫存 workspace。
3. `TaskDef.FanOut` 的 yaml tag 是 `fan_out`（底線），不是這個 repo 其他欄位慣用的連字號 `fan-out`——踩了一次才發現。
4. 兩個 fan-out 子任務用 `match.contains` 比對各自 goal text（而非 arrival order，因為兩個 child 的實際分派順序不受我方控制）時，第一版拿 `"handle a"`/`"handle b"` 當比對字串，結果被 coordinator 工具說明文字裡的樣板句「create a sub-agent to **handle a** specific sub-task」誤配到——換成不會跟樣板文字碰撞的獨特字串（`"handle alpha-item"`/`"handle beta-item"`）才穩定。
5. worker 送 `status:"failed"` 的 `submit_result`（不同於送 `"success"`）之後，还需要一輪不呼叫任何 tool 的收尾回應（跟 coordinator 呼叫 `finish` 之後一樣的模式），否則同一個 worker turn 會撞上「no unconsumed step」引發的連鎖 protocol repair。
6. worker 自己回報 `status:"failed"`，任務最終狀態是 **`blocked`**（不是 `error`），因此彙總後 `RunResult.Outcome` 是 `"blocked"`（`StopReason: external_blockage`），不是原先猜測的 `"partial"`——已實測確認，不是憑空假設。

`evals/strict-verification`（1）+ `evals/decision-runtime`（2）+
`evals/retry-recovery`（1）+ `evals/execution-target`（1）+
`evals/workset`（1）+ `evals/side-effect-recovery`（1）+
`evals/legacy-execution-replay`（1）+ `evals/memory-learning`（1）+
`evals/terminal-leak`（1）已完成，共 10/10 critical cases；另有
`evals/core-lifecycle` 的 1 個 baseline case，所以 CI 實際執行 11 cases。

### PR-4 CI gate — done

新增 `.github/workflows/ci.yml` 的 `eval-core` job：`go build -o /tmp/hufu ./cmd/hufu`，接著對 `evals/*/` 每個 suite 目錄各跑一次 `hufu eval run <suite> --format json`，各自寫成 `eval-reports/<suite>.json` 並用 `actions/upload-artifact`（`v4.6.2`）上傳成 artifact；`timeout-minutes: 5` 控制總時間。起初以 `47d25b7` 採 non-blocking rollout；10 個 critical cases 全部穩定通過後，已將 `eval-core` 加入 `ci-success.needs`，成為 blocking merge gate。

### Post-archive completion audit — done

- `aa0ce10`：補齊 Run/Task/Execution/Event assertion dimensions，並讓既有
  cases 實際執行 terminal lifecycle、verification、BackendBinding、fallback、
  cardinality 與 field assertions。
- `e0036fe`：補齊 EvidenceManifest hash、required evidence、artifact refs，並
  重用 `internal/auditverify` 對完成後 workspace 做 canonical audit。
- `89bc526`：將 suite prompts 投影成既有 `improve.BenchmarkFixture`，報告攜帶
  canonical benchmark revision；`hufu eval list` 無參數預設探索 `./evals`；
  JSON artifact 排除 wall-clock duration，保持 deterministic。
- `d4bd8c6`：offline 從測試慣例升級成 fail-closed runtime contract：拒絕
  fixture 自訂 provider/backend/Codex selector，worker tool policy 強制
  `no-net`，workspace seed 拒絕 path escape，provider fixtures 嚴格驗證
  單一 response shape、JSON object arguments 與 trailing documents。

## 11. 首批必測 cases

```text
off-profile compatibility
verifier failure → retry                                [HF-AR-006 overlap: FaultVerifyWrongPolarity/FaultRepeatedFailure — cross-check only, 見2.1]
side-effect unknown → reconcile, no blind retry          [HF-AR-006 overlap: FaultPartialExternalWrite — cross-check only, 見2.1]
acceptance failure → non-completed                       [HF-AR-006 overlap: FaultIncorrectAcceptance — cross-check only, 見2.1]
ExecutionTarget frozen across retry
legacy execution replay
decision challenge/revise routing
retrieved-only memory gets zero credit
workset partial blocks completion
terminal leak blocks strict completion
```

標註 `[HF-AR-006 overlap]` 的三項：case 的斷言重點是「這個行為在完整 run 中可觀察到」（RunResult/events 層級），不要重寫 `internal/team/reliability_eval.go` 已經驗證過的 policy 邏輯本身。

### 11.1 最後4個 case 的實作收尾

**side-effect unknown → reconcile, no blind retry**：`f19b12f` 新增 reconcile-only team contract 與完整 run case，斷言 unknown side effect 進入 `needs_reconcile`，不發生 blind retry，最終 failure/recovery projection 與事件順序一致。

**legacy execution replay**：`f4947eb` 讓 harness 依 production 路徑載入 `session.json` 與 prior-run event lineage，再由 `ResumeInterruptedTasks` 執行 migration/replay；同時修正 decision admission compatibility，只允許由已驗證 `execution_target_migrated` lineage 授權的 legacy identity 變化，篡改 goal 仍被拒絕。

**retrieved-only memory gets zero credit**：`2808f6f` 新增 explicit active-policy/context seed 與 run 後 SQLite aggregate assertion。Case 同時要求 durable `memory_retrieved`、`exposure_count >= 1`，並精確斷言 consulted/applied/positive/negative 都是 0，證明「被注入」不等於「被採用或得到 credit」。

**terminal leak blocks strict completion**：`5cc6ba9` 新增 strict terminal leak case 與 stop-reason assertion，確認 task 即使成功，只要 terminal custody 未清理，run 仍以 `terminal_leak`/non-completed 結束，不會由 fallback 合成成功完成。

## 12. Flake policy

- clock可 injection。
- random有固定 seed。
- opaque IDs normalize。
- network禁止。
- nondeterministic fixture不得 blocking。

## 13. CI

新增 `eval-core`，要求：

```text
offline
deterministic
per-case timeout
JSON artifact
總時間控制在數分鐘內
```

## 14. Tests

除 suite本身，runner需：

```text
TestEvalFixtureStrictDecode
TestEvalEventAssertionOrdering
TestEvalNormalizesOpaqueIDs
TestEvalTimeout
TestEvalNeverCallsNetworkDriverInOfflineMode
TestEvalResultJSONStable
```

## 15. Done

kernel semantic regression可以在 unit tests之外被固定 workflow suite捕捉，
且 failure report直接指出 case/dimension/expected/actual。

## 16. Coding-agent 指派

建立 offline deterministic eval harness：

1. 開工前務必先讀 `internal/team/reliability_eval.go`（HF-AR-006）與本文件2.1節，確認新 harness 與既有 fault-injection replay 的分工，不要重造重疊的 policy 測試。
2. Provider driver 走 HTTP fake 路線（第4節），沿用 `internal/team/decision_reference_capability_runner_test.go` 的 `fixedAnswerProvider` 與 `internal/team/decision_fake_judge_test.go` 的 `fakeJudge`/`classifyDecisionStagePrompt` 手法；**不要**嘗試從 `internal/evalharness` 存取 `Coordinator.workerAgentOverride`/`repairAgentOverride`（unexported，且 v1 不需要，見4.2節）。
3. 其餘重用 team loader（`LoadTeam`）/RunResult/EvidenceManifest/auditverify/improve benchmark fixture（第2節列的型別與檔案）。
4. 先做 10 個 critical semantic cases（第11節，含3項 HF-AR-006 cross-check）；不要把 live-model benchmark 放進 blocking CI。
