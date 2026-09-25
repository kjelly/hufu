# Hufu Team Action Catalog 實作規格(v2)

> Status: draft(Ready for implementation;實作中,完成後移到 `docs/archive/implementation-plans/`)
> Authority: normative(實作計畫;完成後 canonical 文件為 `docs/reference/action-providers.md`)
> Verified-Commit: `42ffd53`
> Superseded-By: —
> Target: `kjelly/hufu` main
> Baseline: `cb2ee2b`(本文所有 `file:line` 以此 commit 為準;行號會漂移,以函式名稱為主)
> Date: 2026-09-24
> Revised: 2026-09-25(採納外部審查 4 點:provider 身分固定 D19、提案者可達性 D20、workspace_write 必須明確 recovery D21、
> WP-6 過時指令與 §5.1 範例路徑)
> Supersedes: 同檔案 v1「Team Action Catalog + Gated Action Invocation」(worker 同步 invoke 設計,已否決,見 §28)
> Scope: team 設定、ActionProvider 執行路徑、coordinator 派工、worker protocol tools、durable Todo、事件、觀測、文件

---

## 0. 文件狀態與閱讀方式

- 本文件是實作計畫(2026-09-25 從 gitignored 的 `docs/tmp/` 移入)。**程式碼註解不得引用本檔**;對外引用一律指向
  WP-10 更新後的 `docs/reference/action-providers.md`(`docs/README.md` 的 Document header 段要求註解引用 canonical 路徑)。
  全部 WP 完成後,本檔移到 `docs/archive/implementation-plans/team-action-catalog.md`。
- 本文件只保留 **coding agent 可以直接完成** 的工作。需要產品決策的項目已由使用者在 §1 決定;
  仍待決策的議題列在 §27(延後),不屬於本規格的交付範圍。
- 實作順序:§24 的 WP 必須依序完成(WP-0 可獨立先做)。每個 WP 一個 commit,
  commit 前 `go build ./... && go vet ./... && go test ./...` 全綠。
- 檔案大小規則(CLAUDE.md:< 800 行):新邏輯一律放在新檔案。對已超過 800 行的檔案
  (`coordinator_task_run.go` 5590、`status.go` 1997、`agent/agent.go` 1936、`parse.go` 1839、
  `coordinator_eventstore.go` 1707、`coordinator_session.go` 1349、`cmd/hufu/report.go` 1346、
  `event_reducers.go` 1297、`coordinator_tools.go` 1182、`runtime_workflow.go` 971、`services.go` 920、
  `execution_events.go` 814)只允許加入「呼叫新檔案函式」或「結構欄位」等級的最小掛勾,
  每個檔案淨增加不超過約 25 行。
- 錯誤一律以 `fmt.Errorf("doing X: %w", err)` 包裝;測試一律 table-driven。
- **實作 baseline(2026-09-25,branch `feat/team-action-catalog` 起點 `42ffd53`)**:`go build ./...`、`go vet ./...` 通過;
  `go test ./...` 只有 `TestCoordinatorFinalizeTaskTerminalResourcesClosesLeakAfterAcceptedTerminalResult` 在第一次全套件
  執行時失敗一次(terminal cleanup 在負載下偶發),單獨 `-count=10`、`-race -count=40` 與之後的全套件重跑都通過,
  視為既存 flake,與本功能無關。WP-3 全套件執行時 `TestCoordinatorTransferTerminalRejectsUnsafeRequests` 也偶發失敗一次
  (同為 terminal session 測試),單獨 `-race -count=30` 與全套件重跑都通過。

### 0.1 v1 → v2 主要變更

| 項目 | v1 | v2 |
|---|---|---|
| 誰觸發執行 | worker 呼叫 `team_action_invoke`,在 tool handler 內同步執行 child | **coordinator** 在既有 `agent` 工具的 task 內帶 `catalog_action`,走既有 ExecuteTasks/scheduler/admission/resume |
| worker 工具 | list / get / propose / invoke | list / get / propose(**無 invoke**) |
| side effect | 全部類別 | **只允許 `none`、`workspace_write`** |
| decision runtime | 依 catalog `decision-profile` 閘控 | catalog task 固定 `decision_profile: off`;若有效 profile 非 off 則派工前拒絕 |
| team 類型 | 未說明(實際只有 workflow team 能跑 action) | 動態 team 與 workflow team 都支援 |
| proposal 連結 | invoke 時帶 `proposal_ids` | runtime 依 `(action, entry hash, arguments hash)` 自動連結,不需 ID |
| budget | `max-per-run`(run ID 每次 resume 都換) | `max-invocations`,以 session 內 durable Todo 計數 |
| 冪等鍵 | 沿用 `HUFU_ACTION_INVOCATION_ID`(實為每 attempt 變動) | 新增 durable `HUFU_CATALOG_INVOCATION_ID` |
| catalog drift | 新 finding `action_catalog_drift` | catalog hash 納入 `ExecutionPolicySnapshot`,沿用既有 drift fail-closed |
| 派工被拒 | 未定義(一般 tool error 會讓 coordinator run 直接失敗) | 專用可恢復回應 `TEAM ACTION DISPATCH REJECTED:`,不進 policy repair(D14) |
| 既存問題 | 未處理 | §23 列 35 項;其中 10 項(E-01~E-06、E-16、E-18、E-21、E-22)在 WP-0/WP-2/WP-9 修正(E-02、E-22、E-25、E-26、E-27、E-29、E-30、E-31、E-32、E-33、E-34 已完成) |

---

## 1. 決策紀錄

### 1.1 使用者決策(2026-09-24)

- **D1 執行者:coordinator 派工。** Worker 只能 discover/propose;catalog action 由 coordinator
  以 `catalog_action` 派成一般 action task。
- **D2 v1 side-effect 範圍:`none` + `workspace_write`。** `external_write`、`infra_mutation`、
  `credential_mutation`、`unknown` 在載入時拒絕(留給 v2,見 §27)。`side-effect` 是 maintainer 對受信任 provider
  的**宣告**,不是檔案系統沙箱(見 D21)。
- **D3 team 類型:動態 team 與 workflow team 都支援。**
- **D4 既存 decision no-go 未強制(E-07):只記錄,另案處理。** v1 catalog task 不依賴 decision runtime。

### 1.2 本規格採用的技術預設(coding agent 照做,不需再詢問)

- **D5** catalog task 的 `DecisionProfile` 固定 `"off"`。派工時以
  `resolution, err := ResolveDecisionProfile(c.decisionConfig(), c.DecisionProfileOverride(), task)`
  (`decision_config.go:44`)解析;`err != nil` 或 `resolution.Enabled()`(例如 CLI `--decision-profile`
  覆寫成 team 已定義或 `builtin/…` 的非 off profile)→ 拒絕 `team_action_decision_profile_unsupported`,
  **不建立 Todo、不啟動 provider**。理由:非 off profile 需要 decision-options/option-proposal、
  request contract 與 judge(`decision_dispatch.go:113-189`),catalog v1 沒有這些欄位。
- **D6** Proposal 自動連結:派工時 runtime 找出所有 `action_id`、`entry_hash`、`arguments_hash`
  皆相同的 proposal,依事件順序取前 32 筆寫入 binding。`require-proposal: true` 只有在其中至少一筆
  `assessment == "recommended"` 時才成立。
- **D7(計數範圍為使用者決策,2026-09-24)** 次數上限 `max-invocations` 以「目前 session 可見的 durable TodoItem」
  計數(含 resume 前的 run);**只要 Todo 被 admit 就計入**,包含 provider 從未啟動的情況(`--steps` 確認被拒、
  capability preflight 擋下、resume 時被 manual recovery block)。被拒絕的派工(沒有建立 Todo)不計入。
  `--new` 開新 session 會重置。預設 1,範圍 1..64。
- **D8** catalog hash 納入 `ExecutionPolicySnapshot`(`omitempty`)。catalog 變更後 resume 走既有
  policy snapshot drift fail-closed(需 `--new`)。
- **D9** 每個 catalog Todo 在 admission 前取得 durable `CatalogInvocationID`(`tai_…`),存在 TodoItem,
  resume 時不變;以新環境變數 `HUFU_CATALOG_INVOCATION_ID` 傳給 provider。既有
  `HUFU_ACTION_INVOCATION_ID`(每 attempt 變動)語意不變。
- **D10** catalog entry 以單一 `agent` 欄位宣告執行身分(action task 的 `TaskDef.Agent`;不啟動模型)。
  Coordinator 派工時 `agent` 必須等於該值。
- **D11** Worker 工具不顯示 `capability`、`type`、`agent`、provider 任何資訊;coordinator 工具額外顯示
  `agent`(派工需要)與次數/proposal 狀態,也不顯示 provider。CLI(operator 面)顯示 capability/type,
  但不顯示 provider command/source/dir。
- **D12** 參數 schema 沿用 `RunInputSchema` 方言(kebab-case YAML;JSON 投影為 snake_case),不引入第二套
  JSON Schema。catalog 路徑額外加嚴:拒絕重複 key、input schema 禁用 `number` 型別(只允許 integer)、
  每個 object 節點必須 `additional-properties: false`。
- **D13** Workflow team 內 catalog task 不支援 `depends_on`(workflow 模式的 coordinator schema 本來就壓縮成
  agent/goal/constraints);workflow 模式下帶 `depends_on` 的 catalog task 以 `team_action_task_field_forbidden` 拒絕。
  動態 team 支援同批次 `depends_on`。
- **D14(使用者決策,2026-09-24)** 派工編譯錯誤(含 §12.1 catalog decode 錯誤)使用**專用的可恢復回應**
  「`TEAM ACTION DISPATCH REJECTED:`」,**不**走 delegation policy repair:不設定 repair pending、不消耗 repair
  預算、不觸發「已完成 worker 不得重派」鎖定(`delegation_policy.go:65-90`)。**2026-09-24 更新**:`dfc083f` 之後 coordinator
  工具的 error response 預設可恢復,D14 回應就是一般 error response,不需要 call-ID 放行機制(§12.2 #4)。coordinator 收到後可以繼續用
  `team_action_get`、`team_info`、派 worker 補 proposal,或放棄此 action。每個 invocation 最多 3 次;第 4 次起改走
  既有 `rejectDelegationPolicy` 路徑。詳細機制見 §12.2。背景:coordinator 工具的一般 error response 會被
  `policyGatedTool` 轉成 `errCoordinatorToolFailure` 使 run 直接失敗(`tool_policy_gate.go:372-403`,
  `TestPolicyGateCoordinatorDispatchErrorResponseIsTerminal`);policy repair 期間 coordinator 只能呼叫
  `agent`/`finish`(`tool_policy_gate.go:233-238`)。整批拒絕,不部分建立。
- **D15** 設定 `access.discover` / `access.propose` 或作為 entry `agent` 的角色不得宣告 extra-models;
  entry `agent` 若是 isolated worker workspace,entry 只能是 `side-effect: none`。
- **D16** team 若沒有 durable event journal,catalog 派工與 propose 一律拒絕 `team_action_journal_required`。
- **D17** Proposal ID 與 CatalogInvocationID 使用的 branch 身分取自 `c.eventStore.BranchID()`(`event_store.go:104`),
  以 helper `c.durableBranchID()` 包裝;**不要**用 `activeBranchID()`(每次重讀 session tree,失敗時靜默回 `"main"`,
  `worker_memory.go:1079-1087`)。branch ID 為空視同沒有 journal(D16)。
- **D18** Catalog task 不附加 result contract、execution route:在 `canonicalizeTaskOccurrence`
  (`decision_admission.go:279-323`)中,`task.CatalogAction != nil` 時跳過 `admittedResultContract`
  (`result_contract.go:58-73`)與 `admittedExecutionRoute`,兩者保持 nil。
- **D19(2026-09-25)Provider 身分固定。** 每個 entry 帶 `ProviderIdentityHash`(該 entry capability 的 provider 設定
  身分),並納入 `entry.Hash`,因此 provider 設定或 golang 原始碼變更後 resume 走 D8 的 drift fail-closed。計算方式抽自
  既有 `executionRunInputPolicyHash`(`run_input.go:605-642`)的 `providerIdentity`:runtime、source、mode、command、
  dir、timeout,加上 `ProviderRegistry.ProviderName(capability)`——golang runtime 的名稱是 `golang:<source digest>`
  (載入時 `golangruntime.Prepare` 計算,每次執行 `RunChild` 再驗證,`internal/golangruntime/runtime.go:164`)。
  **限制**:command runtime 只能固定 argv/dir/timeout,無法固定 argv 所呼叫的腳本或程式內容(`sh -c` 可執行任何東西);
  這是信任邊界,需要內容身分的 action 應使用 golang runtime。文件(WP-10)需寫明。
- **D20(2026-09-25)提案者必須可達。** `require-proposal: true` 的 entry 至少要有一個「有效提案者」,否則載入失敗
  `action_catalog_proposer_unreachable`(§6)。有效提案者 = 列在 `access.propose`,且:在 `reachableWorkers` 內
  (`team_policy_lint.go:578-612`);team `tools-denied` 沒有拒絕 `team_action_propose`(§9.2 #1 會靜默省略);
  不使用外部 agent backend(`agentUsesExternalBackend`,`result_contract_compile.go:321-328`;Codex 等 backend
  `SupportsHufuTools: false`,`subagent_codex.go:409-416`)。`--worker-model` 等 CLI 覆寫在載入之後才生效,所以
  run setup 以已解析的 execution target 再檢查一次(§6「Run setup 重檢」)。
- **D21(使用者決策,2026-09-25)`workspace_write` entry 必須明確設定 `recovery`。** `side-effect: none` 維持預設
  `retry`;`workspace_write` 缺 `recovery` → 載入失敗 `action_catalog_recovery_required`。理由:provider 沒有沙箱——
  `cmd.Dir` 是設定的 `dir`(空白時為 hufu process cwd),環境完整繼承 hufu 的 `os.Environ()` 並加上
  `HUFU_REPOSITORY` 等變數(`action_provider.go:363`、`510-535`);`side-effect` 只是對受信任 provider 的宣告,
  hufu 無法驗證 provider 是否冪等,而 `TaskDone` 前 crash 會重跑已成功的 provider(§18、E-12)。不採用「workspace_write
  預設 manual」:會與 static task 的 `DefaultRecoveryPolicy`(`recovery.go:140-157`)不一致,且讓未注意的 maintainer
  在 resume 時才發現被擋。

---

## 2. 目標與非目標

### 2.1 目標

1. Team maintainer 可在 `team.yaml` 宣告 action catalog(ID → 既有 ActionProvider capability/type、
   typed 參數 schema、side effect、recovery、存取與派工政策)。
2. Worker 可查詢自己被授權看見的 action contract,並留下 durable、typed 的 recommendation(proposal)。
3. Coordinator 可查詢 catalog 與 proposal,並以 catalog ID + 參數派出 action task;
   runtime 從 static catalog 編譯出 `TaskDef.Action`,model 無法指定 capability、type、side effect、recovery。
4. 派出的 action task 完整重用既有 admission、scheduler、resource 序列化、recovery、resume、receipt、事件。
5. 動態 team(沒有 `workflow.phases`)也能執行 catalog action。
6. inspect/report 可回答:誰建議、coordinator 派了什麼、用哪個 catalog entry hash、provider 是否啟動、結果在哪。
7. 修正查核時發現、與本功能同區域的既存問題(WP-0,§23)。

### 2.2 非目標(v1 不做)

- Worker 觸發執行(任何 `team_action_invoke` 形式)、CLI 直接 invoke。
- `external_write` / `infra_mutation` / `credential_mutation` / `unknown` 類 catalog action。
- 讓 catalog task 走 decision runtime / commit gate 前提。
- 為每個 action 建立一個 tool;自動把 `action-providers` 轉成 catalog;agent 動態建立或修改 catalog。
- 多 proposal 自動彙整、投票、依 confidence 自動派工。
- Worker 看見其他 worker 的 proposal。
- Workflow team 的 catalog task `depends_on`、跨回合 dependency。
- `hufu list <team>` 摘要、catalog tags / cost class。
- 修正 §23 中標記「記錄」的既存問題。

---

## 3. 已查核的現況(實作前提)

以下事實已在 `cb2ee2b` 對照原始碼確認,實作必須以此為前提。

### 3.1 Action 執行

| 事實 | 位置 |
|---|---|
| `executeTask` 遇到 `task.Action != nil` 時先 `prepareTaskDecision`(非 leaf)再進 `executeRuntimeAction`,不啟動 worker 模型 | `coordinator_task_run.go:343-355` |
| `executeActionValueForTask` 要求 `w.Enabled()`,且只允許 EXECUTE 或「PREPARE + side_effect none」 | `runtime_workflow.go:134-145` |
| `newRuntimeWorkflow` 在沒有 phases 時提早 return,`registry`、`workspace.Root`、`team` 都未設定 | `runtime_workflow.go:50-54` |
| `c.phaseWorkflow` 在 production 永遠非 nil(`newCoordinator` 無條件建立) | `coordinator.go:1490-1500` |
| `allocateRuntimeActionWorkspace`、`emitRuntimeActionEvent` 都以 `Enabled()` 閘控 | `coordinator_task_run.go:2378`、`2688` |
| `session.ProviderRegistry` 對所有 team 都會建立(與 workflow 無關) | `parse.go:1432-1449` |
| 結論:**動態 team 目前完全無法執行 action**(task 以 TaskError 結束,action_failed 事件也被跳過) | — |
| Static action 的 in-run retry `permitActionRetry` 不看 side effect / recovery | `runtime_workflow.go:213-253`、`dag_scheduler.go:392-401` |
| `HUFU_ACTION_INVOCATION_ID` = `structured-action-<todo>-<nanos>-<seq>`,每 attempt 都變 | `coordinator_task_run.go:2394-2397`、`action_provider.go:532` |
| Commit gate 在 provider 前執行(`commitGateActionDenial`);預設只保護 external_write/infra/credential/unknown | `coordinator_task_run.go:2437-2445`、`commit_gate.go:23-30` |
| Provider 輸出經 `CanonicalizeRuntimeOutputs`(≤128 key、256 KiB、depth 8、會 redaction);目前沒有 output schema 驗證 | `runtime_outputs.go:12-66`、`coordinator_task_run.go:2508` |
| Artifact ingestion 與 workset projection 發生在 canonicalize **之前** | `coordinator_task_run.go:2490-2515` |

### 3.2 Coordinator 派工

| 事實 | 位置 |
|---|---|
| `decodeModelTaskDefs` 只明確拒絕 `modelTaskRuntimeOwnedFields`;其他未知 key(含 `action`、`phase`、`decision_profile`)被靜默丟棄;JSON key 比對大小寫不敏感 | `coordinator_tools.go:192-257` |
| Coordinator JSON 可設定 `id`、`contract_id/hash/revision`、`side_effect`、`recovery`、`max_retries`、`on_failure`、`verify` | `coordinator.go:86-253` |
| `agent` 工具 schema:`required: [agent, goal]`、`additionalProperties: false`;workflow 模式壓縮成 agent/goal/constraints;initial batch 另有分支 | `coordinator_tools.go:30-91` |
| `agent` enum 來自 `workerNameList()`(workflow 模式為當前 phase workers;否則受 allowed-workers 限制) | `coordinator_agents.go:181-210` |
| `CompileTaskGoalContracts` 可**只憑 agent 名稱**把 static contract 綁到 task,並覆寫 Action/Phase/SideEffect | `contract_compile.go:117-179`(140-142) |
| `validateTasks` 要求 task 綁定當前 phase 的 static contract、每 contract 每 phase 只派一次、需一次派齊必要 contract | `runtime_workflow.go:456-496` |
| `observe` 只追蹤 static contract;未綁 contract 的 task 失敗不會讓 phase 失敗 | `runtime_workflow.go:498-577`(518) |
| `CheckDuplicate` 以 agent+goal+constraints+verify 判重;對失敗過的同 task 會抑制重派 | `coordinator_taskcache.go:489-495`、`699-713`、`749-857` |
| `canonicalizeTaskOccurrence` 只填空欄位(非空 SideEffect/Recovery 保留) | `decision_admission.go:279-323`、`recovery.go:230-256` |
| `decisionOccurrenceInputDigest` 的匿名 struct 大多數欄位**沒有 json tag**(以 Go 欄位名序列化);只有最新的 `DynamicToolAuthorization`、`ResourceScopeSnapshot` 帶 `json:"…,omitempty"`。新增欄位必須照後者加 tag,否則所有既有 digest 改變 | `decision_admission.go:132-201`(171-172) |
| **2026-09-24 已變更**(`dfc083f`):coordinator 工具的 error response 回給模型;只有 Go error(含 `markCoordinatorFatal` 標記的 fatal、repair exhausted、schema repair 失敗)或連續超過 3 次錯誤才轉成 `errCoordinatorToolFailure`。gate 是唯一的終止邊界(fantasy 忽略 `OnToolResult` 回傳值);`8221414` 之後 gate 在每一步生效 | `coordinator_tool_errors.go`、`tool_policy_gate.go`、`coordinator_preflight.go` |
| Repair pending 期間 coordinator 只能呼叫 `agent`/`finish`,其他工具被拒且再耗一次預算;repair 次數 > 0 後,已完成的 model worker 永遠不能重派 | `tool_policy_gate.go:233-238`、`delegation_policy.go:65-90` |
| `compareTaskDefWithTodoOccurrence` 以 `reflect.DeepEqual` 比較 TaskDef 與由 TodoItem 重建的 TaskDef | `task_occurrence_projection.go:217-250` |
| Resume 重新執行**同一個 Todo ID**,TodoItem 欄位經 `cloneTodoItem` 保留 | `coordinator_session.go:1240-1286`、`status.go:1288-1388` |
| `executionRunID` 在每次公開入口(含 resume)都換新 | `execution_events.go:333-343` |
| ExecuteTasks 回傳錯誤若不是經 `rejectDelegationPolicy`,會變成 coordinator tool failure 使 run 失敗 | `coordinator_execute.go:161-170` 註解 |

### 3.3 Worker 工具

| 事實 | 位置 |
|---|---|
| Protocol 工具在 `ResolveStaticWorkerTools` 選完一般工具後附加,不受 agent `tools:` 限制 | `static_tool_resolution.go:107-120` |
| 具體 handler 在 `ResolveTaskTools` 以 `&submitResultTool{todoID: req.TodoID}` 形式注入 | `services.go:779-785` |
| `readOnlyToolMutation` 把未知工具一律視為 mutation → side_effect none 的 worker 會被拒 | `tool_policy_gate.go:421-432`(執行於 223-232) |
| `tools.IsReadOnlyObservationTool` 已收錄 team 套件工具 `team_info`、`context_query` | `internal/tools/types.go:100-107` |
| 未實作 `DescribeWorkspaceScope` 的工具會讓 bounded read scope 任務 resume 失敗 `resource_scope_unreproducible` | `coordinator_resource_scope.go:307-329`、`444-451` |
| Bound workset 任務只允許帶 `boundArtifactPolicyTool()` marker 的非內建工具 | `tool_policy_gate.go:129-160` |
| Codex 等外部 backend 不支援 Hufu 工具(`SupportsHufuTools: false`),與 submit_result 相同 | `subagent_codex.go:409-416` |
| Event append 會先 redaction 再驗證/雜湊;`emitEvent` 在沒有 eventStore 時靜默成功 | `event_store.go:390-433`、`coordinator_eventstore.go:665-696` |
| Idempotent append 以 (branch, key) 去重,回傳既有事件且**不比對 payload**(decision 事件例外);回傳值沒有「是否命中既有事件」的訊號 | `event_store.go:364-385` |
| `initEventStore` 在每次公開入口(含 resume)執行,是重建 in-memory 事件投影的掛勾 | `coordinator_eventstore.go:102-176` |

---

## 4. 架構總覽

```text
team.yaml
  action-providers:  (不變,實作綁定)
  action-catalog:    (新,static authority)
        │ load: normalize + validate + hash
        ▼
TeamSession.ActionCatalog (frozen snapshot)  ──hash──►  ExecutionPolicySnapshot.ActionCatalogHash
        │
        ├── worker protocol tools (依 access 暴露)
        │     team_action_list / team_action_get   (read-only)
        │     team_action_propose ──► event: team_action_proposed ──► proposal index(session-scoped)
        │
        └── coordinator tools
              team_action_list / team_action_get   (read-only;含 proposal 與次數狀態)
              agent{tasks:[{agent, goal, catalog_action:{id, arguments}}]}
                    │ compileCatalogActionTasks (純函式 + 讀 index/TodoList)
                    ▼
              TaskDef{Action, SideEffect, Recovery, Phase, DecisionProfile:"off", CatalogAction binding}
                    │ ExecuteTasks(既有流程;少數 stage 對 catalog task 跳過)
                    ▼
              durable Todo (task_created 帶 catalog_action) → scheduler → executeTask
                    ▼
              executeRuntimeAction → ActionProvider(動態 team 經 ActionsEnabled 解耦)
                    ▼
              output schema 驗證 → receipt / action_* 事件(帶 catalog 欄位) → TaskDone
```

核心不變量:

> **LLM 可以選擇「要用哪一個已核准的 deterministic action、帶什麼參數」,
> 但不能定義 action、執行機制、side-effect 分類或 recovery 語意,也不能繞過既有 task lifecycle。**

---

## 5. 設定模型:`action-catalog`

### 5.1 YAML

```yaml
action-providers:          # 不變
  diagnostics:
    command: [/opt/team-actions/diagnostics.sh]   # 絕對路徑:相對路徑以 hufu process cwd 為準,不是 team 目錄(E-23)
    timeout: 120

action-catalog:
  collect-debug-bundle:                  # action ID
    description: Collect bounded runtime diagnostics for one service.
    capability: diagnostics              # 必須是 action-providers 的 key
    type: collect_debug_bundle           # 傳給 provider 的 Action.Type
    agent: runtime-engineer              # 執行身分;action task 的 TaskDef.Agent
    side-effect: none                    # none | workspace_write
    recovery: retry                      # retry | manual | never;none 可省略(預設 retry),workspace_write 必填(D21)
    input-schema:                        # RunInputSchema 方言
      type: object
      properties:
        service:
          type: string
          min-length: 1
          max-length: 128
        include-thread-dump:
          type: boolean
      required-properties: [service]
      additional-properties: false
    output-schema:                       # 選填;驗證 ActionResult.outputs
      type: object
      properties:
        summary:
          type: string
      required-properties: [summary]
    access:
      discover: [runtime-engineer, network-engineer, critic]
      propose: [runtime-engineer, network-engineer]
    invocation:
      require-proposal: true             # 預設 false
      allow-unattended: true             # 預設 false
      max-invocations: 4                 # 預設 1,範圍 1..64
```

- Catalog 內的 key 一律 kebab-case。注意 static task 用的是 `side_effect`(底線),catalog 用 `side-effect`;
  文件(WP-10)需明示。
- Command provider 的 `command` 與 `dir` 都不以 team 目錄為基準:`cmd.Dir = p.dir`(`action_provider.go:363`),
  `dir` 空白時 child 在 hufu process cwd 執行,`dir` 本身也相對於 cwd(E-23,記錄、本規格不改解析規則)。範例與
  WP-10 文件一律使用絕對路徑或 inline command,並明寫此規則。
- `action-catalog` 是 `spec` 下的 additive optional 欄位,v1alpha1 與 legacy 格式共用
  `teamManifestSpecFields`(`team_manifest.go:120-209`,strict `KnownFields(true)`)。在該 struct 加
  `ActionCatalog map[string]yaml.Node \`yaml:"action-catalog,omitempty"\``——型別用 `yaml.Node`,讓外層 strict
  decode 只檢查頂層 key,entry 內容錯誤留給 catalog loader 變成 finding(而不是 parse 硬錯誤);`omitempty`
  讓 `hufu team migrate` 重新編碼時不輸出空值(`team_manifest_migrate.go:234-244`)並能 round-trip。
- `parseTeamYML` 回傳 `agent.TeamConfig`(`parse.go:775`),無法攜帶 team package 型別。比照
  `LoadRunInputDefinitions`(`parse.go:1328-1345`,於 1406 呼叫)新增 team-package loader
  `loadActionCatalog(absDir string, vars map[string]string) (map[string]actionCatalogEntryYAML, []ContractFinding, error)`:
  重新讀取 manifest、取出 `action-catalog` 節點,逐 entry 以 strict decode(`KnownFields(true)`)解成
  `actionCatalogEntryYAML`;單一 entry 的 decode 失敗轉成 `action_catalog_entry_invalid` finding,不中斷其他 entry。
  只有 YAML 本身無法解析才回 error。

### 5.2 欄位規則

| 欄位 | 必填 | 規則 |
|---|---|---|
| ID(map key) | 是 | `^[a-z][a-z0-9-]{0,63}$`(不得含 `.`,否則 lint 路徑對應錯誤 `team_lint_source.go:140-174`) |
| `description` | 是 | trim 後 1..2048 bytes |
| `capability` | 是 | normalize 同 `normalizeCapability`;必須有已設定的 provider |
| `type` | 是 | trim 後 1..128 bytes |
| `agent` | 是 | 解析到 team 內的 worker(非 coordinator、非內建 `helper`);受 allowed-workers 限制時必須可達 |
| `side-effect` | 是 | `none` 或 `workspace_write` |
| `recovery` | 視 side-effect | `retry`、`manual`、`never`。`none`:選填,預設 `retry`;`workspace_write`:**必填**(D21) |
| `input-schema` | 是 | 見 §8.1 |
| `output-schema` | 否 | 見 §8.2 |
| `access.discover` | 否 | worker 名稱清單;空 = 沒有 worker 可見 |
| `access.propose` | 否 | 必須是 `discover` 的子集 |
| `invocation.require-proposal` | 否 | bool,預設 false;true 時至少要有一個有效提案者(D20) |
| `invocation.allow-unattended` | 否 | bool,預設 false |
| `invocation.max-invocations` | 否 | 整數 1..64,預設 1 |

Catalog 最多 128 個 entry。

---

## 6. 載入時驗證與 finding codes

所有 catalog 問題都以 `ContractFinding`(`contract_finding.go:5-17`)回報:error finding 讓 `LoadTeam`
失敗(lint mode 除外,`parse.go:1595-1600`);`hufu team lint` 列出全部。新 code 必須加入
`contract_finding.go` 常數與 `teamLintKnownCodes`(`team_lint_ignore.go:21-66`),否則 `--ignore` 會拒絕。

| Code | Severity | 條件 |
|---|---|---|
| `action_catalog_id_invalid` | error | ID 不符 §5.2 |
| `action_catalog_entry_invalid` | error | description/type/agent 缺漏或超長、未知 key、entry 數 > 128 |
| `action_provider_missing`(**沿用**) | error | capability 沒有 provider(沿用 `validateActionTaskContract` 的判斷,`team_policy_lint.go:312-329`) |
| `action_catalog_side_effect_unsupported` | error | side-effect 不是 none/workspace_write |
| `action_catalog_recovery_invalid` | error | recovery 不是 retry/manual/never |
| `action_catalog_recovery_required` | error | `side-effect: workspace_write` 但未設定 `recovery`(D21;YAML 未出現或 trim 後為空都算未設定) |
| `action_catalog_input_schema_invalid` | error | 違反 §8.1 |
| `action_catalog_output_schema_invalid` | error | 違反 §8.2 |
| `action_catalog_agent_unknown` | error | `agent`、`discover`、`propose` 中有名稱解析不到 worker |
| `action_catalog_agent_unreachable` | error | entry `agent` 是 coordinator 角色、`helper`,或不在 allowed-workers(沿用 `reachableWorkers`,`team_policy_lint.go:578-612`) |
| `action_catalog_agent_unsupported` | error | entry `agent` 或任何 discover/propose 角色宣告 extra-models;entry `agent` 為 isolated worker workspace 且 side-effect 非 none(`worker_workspace_policy.go:125-140`) |
| `action_catalog_access_invalid` | error | propose ⊄ discover、名單重複(以解析後 AgentDef 比較,不以字串) |
| `action_catalog_proposer_unreachable` | error | `require-proposal: true` 且沒有有效提案者(D20):`access.propose` 為空,或其中每個角色都不在 `reachableWorkers`、被 team `tools-denied` 拒絕 `team_action_propose`,或 `agentUsesExternalBackend` 為 true。訊息逐一列出每個提案者被排除的原因 |
| `action_catalog_invocation_invalid` | error | max-invocations 超出 1..64 |
| `action_catalog_phase_unreachable` | error | workflow team:`none` entry 但 phases 無 EXECUTE 也無 PREPARE;`workspace_write` entry 但無 EXECUTE |
| `action_catalog_tool_sequence_unsupported` | error | 任何 static contract / agent 預設 execution 的 `tool_sequence` 列出 `team_action_list/get/propose` |

驗證分兩段:

1. **結構正規化**(`normalizeActionCatalog`,新檔 `action_catalog.go`):接收 `loadActionCatalog` 的結果,
   正規化 schema(`normalizeRunInputSchema`,`run_input.go:219-276`)、檢查 ID/欄位/限制(含
   `action_catalog_recovery_required`:只依 entry 本身判斷,所以屬結構段;`actionCatalogEntryYAML.Recovery` 以 string
   保留「未設定」,正規化後 `none` 的空值才補成 `retry`)。產生 findings,不回硬錯誤。
   在 `loadTeamWithMode` 中於 agents(`parse.go:1517-1587`)與 registry(1432-1449)建立之後執行;結果存到
   `session.ActionCatalog`,結構 findings 暫存在 session 的未匯出欄位。
2. **語意驗證**(`validateActionCatalog(session)`,新檔 `action_catalog_validate.go`):先輸出暫存的結構 findings,
   再做 agent 解析、provider、access、提案者可達性(D20)、workflow phase、isolation、extra-models、tool_sequence。在
   `ValidateTeamPolicyContracts`(`team_policy_lint.go:29-69`,約第 59 行附近)呼叫。

結構正規化通過的 entry 才進入 snapshot。`validateActionCatalog` 必須是**純函式**(同一 session 每次呼叫回傳相同
findings、不修改 session):`ValidateTeamPolicyContracts` 有三個呼叫點(`parse.go:1595`、`effective_spec.go:297`、
`verifier_lint.go:137`)。語意驗證失敗的 entry 由 `loadTeamWithMode` 在 lint mode 下、驗證完成後從 snapshot 移除
(並重算 hash);非 lint mode 下任何 error finding 都讓 `LoadTeam` 失敗。

**Run setup 重檢(D20)**:載入時只看得到 agent `subagent-provider` / team `subagent-provider-default`;`--worker-model`
等覆寫與 model target 本身(例如 `codex/…`)要到 coordinator 建立後才解析。新增
`(c *Coordinator) validateActionCatalogProposers() error`(新檔 `action_catalog_validate.go`),在
`cmd/hufu/team_setup.go` 的 `FreezeExecutionPolicyAtStartup()`(約 321 行)成功之後、provider profile warm-up 之前呼叫:
對每個 `require-proposal: true` 的 entry,以與派工相同的規則解析每個提案者的 execution target
(`ResolveTaskModel(def, TaskDef{})` + 與 `canonicalizeTaskOccurrence` 相同的 legacy provider 回退 →
`c.resolveCanonicalTaskTarget`,`decision_admission.go:279-299`),再以 `c.ExecutionRegistry().ResolveBackend(target.Backend)`
判斷 kind(precedent `services.go:294-297`、`worker_workspace_policy.go:134`);kind 為 `execution.BackendKindAgent`
者不是有效提案者。若沒有有效提案者,run 在任何 provider 呼叫前失敗,錯誤以 `action_catalog_proposer_unreachable:` 開頭。
載入段與 run setup 段共用同一個「排除原因」判斷函式,只有 backend 判斷的輸入不同。沒有 catalog 或沒有
`require-proposal: true` 的 entry 時,run setup 重檢不做任何事(S12)。

---

## 7. Catalog snapshot、hash 與 drift

### 7.1 型別(新檔 `internal/team/action_catalog.go`)

```go
type ActionCatalogEntry struct {
    ID              string          `json:"id"`
    Description     string          `json:"description"`
    Capability      string          `json:"capability"`
    Type            string          `json:"type"`
    Agent           string          `json:"agent"`            // canonical AgentDef name
    SideEffect      SideEffectClass `json:"side_effect"`
    Recovery        RecoveryPolicy  `json:"recovery"`
    InputSchema     RunInputSchema  `json:"input_schema"`
    OutputSchema    *RunInputSchema `json:"output_schema,omitempty"`
    Discover        []string        `json:"discover"`          // canonical, sorted, deduped
    Propose         []string        `json:"propose"`
    RequireProposal bool            `json:"require_proposal"`
    AllowUnattended bool            `json:"allow_unattended"`
    MaxInvocations  int             `json:"max_invocations"`
    // ProviderIdentityHash pins the capability's provider configuration (D19).
    // It is part of Hash but never shown in worker/coordinator tool output.
    ProviderIdentityHash string     `json:"provider_identity_hash"`
    Hash            string          `json:"-"`                 // sha256:<hex>
}

type ActionCatalogSnapshot struct {
    Version int                  `json:"version"` // 1
    Entries []ActionCatalogEntry `json:"entries"` // sorted by ID
    Hash    string               `json:"-"`
}

func (s *ActionCatalogSnapshot) Lookup(id string) (ActionCatalogEntry, bool)
func (s *ActionCatalogSnapshot) clone() *ActionCatalogSnapshot // deep copy(含 schema,用 cloneRunInputSchema)
```

- `entry.ProviderIdentityHash = actionProviderIdentityHash(session, entry.Capability)`:把
  `executionRunInputPolicyHash`(`run_input.go:605-642`)內的 `providerIdentity` 型別與組裝抽成同檔共用 helper
  `actionProviderIdentity(session, capability) (providerIdentity, bool)`,本函式回傳
  `runInputHash(json.Marshal(identity))`(沒有 provider 時為空字串,由 `action_provider_missing` 報錯)。重構後 `executionRunInputPolicyHash` 的輸出 bytes 必須不變(既有 session 的
  `RunInputPolicyHash` 不得 drift)。需在 registry 建立後計算(`ProviderName` 讀 registry;golang runtime 的
  `golang:<digest>` 來自 `golangruntime.Prepare`)。
- `entry.Hash = runInputHash(json.Marshal(entry))`(`run_input.go:1045`,格式 `sha256:<64 hex>`;含 `ProviderIdentityHash`)。
- `snapshot.Hash = runInputHash(json.Marshal([]struct{ID, Hash}{…sorted}))`。
- 限制常數集中在 `action_catalog.go`:`maxActionCatalogEntries=128`、`maxActionDescriptionBytes=2048`、
  `maxActionSchemaBytes=64<<10`、`maxActionListResults=50`、`maxActionQueryBytes=128`、
  `maxProposalEvidenceRefs=32`、`maxProposalRationaleRunes=4000`、`maxProposalExpectedOutcomeRunes=1000`、
  `maxProposalsPerSession=256`、`maxLinkedProposals=32`、`maxActionInvocationsLimit=64`。

### 7.2 Session 與 clone

- `TeamSession`(`parse.go:25-66`)新增 `ActionCatalog *ActionCatalogSnapshot`(無 catalog 時為 nil)。
- `cloneSession()`(`session.go:266`)必須 `clone.ActionCatalog = orig.ActionCatalog.clone()`。
- 不要把 catalog 放進 `Config` 的 map(clone 時共用)。

### 7.3 Policy snapshot 與 drift

- `ExecutionPolicySnapshot`(`execution_policy_snapshot.go:34-50`)新增
  `ActionCatalogHash string \`json:"action_catalog_hash,omitempty"\``,只在
  `version == executionPolicySnapshotVersion` 區塊(約 266-269,與 ResultContracts/ExecutionRoutes 同處)設定;
  沒有 catalog 時為空字串。
- 既有 golden `testdata/execution-policy/no-contracts-no-routes.golden.json` 與
  `TestOMPCharacterizePolicySnapshotGolden`(`omp_baseline_characterization_test.go:116-157`)必須不變。
- 同一 run 內 list/get/propose/派工都讀 `session.ActionCatalog`(載入時凍結),不重讀 team.yaml。
- 「catalog 變更」包含 entry 引用的 provider 設定或 golang 原始碼變更(D19,經 `ProviderIdentityHash` → `entry.Hash`
  → `snapshot.Hash`);command runtime 所呼叫的腳本內容變更**不會**被偵測(D19 限制)。
- catalog 變更後 resume:既有 `ensureExecutionPolicySnapshot` 回報 drift(`execution_policy_snapshot.go:732-800`),
  任務被 block;`--new` 可繞過(新 root branch)。測試模式照 `TestChangedExecutionRouteFailsResumeClosed`
  (`execution_route_test.go:496-524`)。

---

## 8. 型別化參數與輸出規則

新檔 `internal/team/action_catalog_value.go`,提供:

```go
// canonicalizeCatalogArguments validates raw model JSON against an input schema and
// returns canonical bytes plus their sha256 hash.
func canonicalizeCatalogArguments(schema RunInputSchema, raw []byte) (json.RawMessage, string, error)

// validateCatalogOutputs validates canonical runtime outputs (json.Number values).
func validateCatalogOutputs(schema RunInputSchema, outputs map[string]any) error
```

### 8.1 Input schema(載入時)

- 頂層 `type: object`。
- 每個 `type: object` 節點(含巢狀)都必須明確 `additional-properties: false`。
- 不得出現 `type: number`(只允許 string/integer/boolean/object/array 與 enum)。理由:RunInputSchema 保留數字原文,
  `1` 與 `1.0` 雜湊不同,會讓 proposal 與派工的 arguments hash 對不上。
- 所有 property 名稱不得命中 `utils.IsRedactedJSONKey`(`redact.go:497-503`),訊息同
  `result_contract_compile.go:175-190`:「would be redacted in durable events; rename it」。
- 正規化後的 schema JSON ≤ 64 KiB。
- 不接受 `sensitive:` / `secret:`(`normalizeRunInputSchema` 已拒絕)。

### 8.2 Output schema(載入時)

- 頂層 `type: object`;property 名稱同樣不得命中 `IsRedactedJSONKey`
  (`CanonicalizeRuntimeOutputs` 會 redaction,否則驗證結果不穩定)。
- 允許 `number`。`additional-properties` 未設定時沿用 RunInputSchema 語意(允許)。
- 正規化後 ≤ 64 KiB。

### 8.3 參數正規化(執行時)

`canonicalizeCatalogArguments` 依序:

1. 必須剛好一個 JSON 值且為 object;拒絕重複 key(重用同 package 的 `decodeUniqueJSON`,
   `primary_decision_evidence_adapter.go:345`)。
2. 呼叫既有 `validateAndCanonicalizeRunInput(schema, raw)`(`run_input.go:394`),沿用 depth 8、
   128 properties、256 array items、64 KiB 限制。
3. integer 值以 `strconv.ParseInt` → `FormatInt` 重新輸出,確保 canonical。
4. `redactionStableJSON(canonical)`(`result_contract_validate.go:97-108`)必須為 true,否則錯誤
   `team_action_arguments_not_redaction_stable`。
5. 回傳 `canonical` 與 `runInputHash(canonical)`。

### 8.4 輸出驗證(執行時)

`validateCatalogOutputs` 在 canonicalize 後的 `runtimeOutputs` 上執行(值為 `json.Number`),
重用 `validateRunInputValue`(`run_input.go:443+`)。

---

## 9. Worker 工具

新檔 `internal/team/team_action_tools.go`。三個 Hufu-owned protocol 工具,struct 在解析時綁定
`coordinator`、`todoID`、`agent`(precedent `services.go:781`)。

### 9.1 暴露規則

- `team_action_list`、`team_action_get`:agent 名列至少一個 entry 的 `access.discover`。
- `team_action_propose`:agent 名列至少一個 entry 的 `access.propose`。
- 以下情況不暴露:result-only 模式、sidecar task、extra-model leaf context
  (`ctx.Value(leafExecutionKey{}) != nil`)、protocol repair context、direct agent
  (`createDirectAgent`)。
- 在 Codex 等外部 backend 上,工具即使被解析也不會送到 backend(與 submit_result 相同),runtime 不需特別處理;
  `require-proposal: true` 卻因此沒有提案者的設定由 D20 在載入與 run setup 時拒絕。

### 9.2 解析與授權掛勾

1. `ResolveStaticWorkerTools`(`static_tool_resolution.go:107-120` 之後):新 helper 從
   `input.Session.ActionCatalog` 判斷暴露;被 team `tools-denied` 拒絕時**只省略**,不報錯(會因此讓
   `require-proposal` entry 沒有提案者的情況由 D20 的 `action_catalog_proposer_unreachable` 在載入時擋下)。
   Lint 以零值 Task 呼叫此函式(`team_lint_tools.go:29-32`),helper 必須只依 session + agent 判斷。
2. `ResolveTaskTools`(`services.go:771-785`):
   - **僅在 team 有 catalog 時**:`removeToolNames` 加入三個名稱(防止 MCP/自訂工具冒名),三個名稱的 collision
     檢查改為無條件。沒有 catalog 的 team 行為不變(S12)。
   - `static.Names` 含該名稱時才建構 `&teamActionListTool{…}` 等,要求 `req.TodoID != ""`。Todo 存在性以
     `taskToolResolutionTodo(c, req)`(`services.go:794`)判斷——envelope 建立時 Todo 尚未寫入 TodoList,
     只有 `req.ProspectiveTodo`(`coordinator_task_execution_envelope.go:288-297`);**不可**直接查 TodoList。
     執行期的身分檢查留在 `Run`(§9.4)。
3. 授權分類:
   - `team_action_list`、`team_action_get` 加入 `tools.IsReadOnlyObservationTool`(`internal/tools/types.go:102`),
     一次涵蓋 readOnlyToolMutation、commitGateDenial、retry 安全、fallback、coordinator recovery。
   - `team_action_propose`:在 `readOnlyToolMutation`(`tool_policy_gate.go:428`)依名稱豁免(比照 submit_result/
     submit_plan/finish)。`commitGateDenial`(`decision_discipline.go:249-293`)目前**沒有**任何 protocol 豁免
     (submit_result 也會經過 `EvaluateCommitGate`),需在 `isReadOnlyToolCall` 判斷(254-256)旁**新增**
     `|| toolName == teamActionProposeToolName` 的豁免,並加測試:armed discipline + guarded side effect 的 worker
     呼叫 propose 不被 commit gate 擋。**不要**加入 `isReadOnlyToolCall`(它也影響 retry/fallback 判斷)。
   - 三個工具都實作 `boundArtifactPolicyTool()` marker(`tool_policy_gate.go:133-134`)與
     `DescribeWorkspaceScope() tools.ToolWorkspaceScopeDescriptor { return tools.ToolWorkspaceScopeDescriptor{} }`
     (precedent `coordinator_tools_result.go:127-129`)。`Run` 不得讀檔(catalog 已在記憶體)。
4. Lint:三個名稱加入 `offlineProtocolToolNames`(`team_lint_tools.go:18`)與 `declaredWorkerTools`
   隱含 protocol 名單(`team_policy_lint.go:614-624`)。
5. 可選:`dynamic_tool_authorization.go:477` protocol 排序加入三個名稱(只影響順序,digest 依名稱排序)。

### 9.3 `Info()` 決定性

`Info()` 只能依 static catalog 與 agent 決定(會進 `ProviderSurfaceDigest`/`LogicalToolsetDigest`,
hufu-local 會重新驗證,`subagent_hufu.go:47`)。**不可**讀 Todo、run、proposal 狀態
(不要照抄 `submitResultTool.Info()` 讀 Todo 的做法)。

### 9.4 呼叫者身分檢查(三個工具共用)

`Run` 開頭:

1. `ctx.Value(todoIDKey{}) == t.todoID`;
2. `strings.EqualFold(ctx.Value(tools.AgentNameKey), t.agent)`;
3. 不在 leaf / protocol repair context;
4. 僅 propose:`submitResultRuntimeIdentityFromContext(ctx, c, t.todoID)`(`task_occurrence.go:587-606`)
   取得身分,且 `c.activeTaskResultOccurrence(t.todoID)` 存在並與之相同(precedent `task_occurrence.go:317-329`)。

任一失敗 → tool error `team_action_caller_invalid`。

### 9.5 `team_action_list`

輸入:`{"query": "diag"}`(選填,≤128 bytes,不分大小寫比對 id 與 description 的子字串)。

輸出(只含 caller 可 discover 的 entry,依 ID 排序,最多 50 筆):

```json
{"actions":[{"id":"collect-debug-bundle","description":"…","side_effect":"none","proposal_allowed":true}],"truncated":false}
```

### 9.6 `team_action_get`

輸入:`{"action":"collect-debug-bundle"}`;schema 的 `action` 為該 agent 可 discover 的 ID enum。

輸出:

```json
{"id":"…","description":"…","side_effect":"none","recovery":"retry",
 "input_schema":{…RunInputSchema JSON 投影…},"output_schema":{…},
 "proposal_allowed":true,"require_proposal":true,
 "schema_dialect":"hufu-run-input-schema/v1"}
```

不存在或不可 discover 一律回 `team_action_not_discoverable`(不洩漏存在與否)。
不得輸出 capability、type、agent、provider 任何欄位(含 `provider_identity_hash`)。

### 9.7 `team_action_propose`

輸入 schema:

```json
{"action":"<enum: caller 可 propose 的 ID>","arguments":{},
 "assessment":"recommended|candidate|defer|reject",
 "rationale":"string (1..4000 runes)","expected_outcome":"string (≤1000 runes, optional)",
 "evidence_refs":["<artifact id>", "... ≤32"]}
```

處理順序:

1. §9.4 身分檢查;durable journal 必須存在(`c.hasDurableEventJournal()`),否則 `team_action_journal_required`。
2. entry 存在且 caller 在 `access.propose`,否則 `team_action_proposal_forbidden`。
3. `canonicalizeCatalogArguments` → 失敗 `team_action_arguments_invalid` / `team_action_arguments_not_redaction_stable`。
4. `rationale`、`expected_outcome` 先 `utils.RedactSecrets` 再 `utils.TruncateRunes`;rationale 不可為空。
5. evidence refs:每個 ID 以 `c.authorizedArtifactRef(ctx, id)`(`artifact_access.go:188-256`)驗證:
   `ok`、`ref.ID == id`、`ref.SHA256 != ""`、`producer != ""`、`ref.TaskID == "" || ref.TaskID == producer`。
   失敗 `team_action_evidence_invalid`。只接受 artifact ID,拒絕任何路徑形式字串。
6. session proposal 數 ≥ 256 → `team_action_proposal_limit_exceeded`。
7. 依 §10 建立 payload 並同步 append;失敗則回 tool error,不得回報成功。

成功輸出:

```json
{"proposal_id":"tap_…","action":"collect-debug-bundle","assessment":"recommended",
 "arguments_hash":"sha256:…","status":"recorded","duplicate":false}
```

Propose **不**建立任何 task、不授權任何執行。

---

## 10. Proposal durable 模型

新檔 `internal/team/team_action_proposal.go`。

### 10.1 事件

- 新常數 `EventTeamActionProposed EventType = "team_action_proposed"`(`event_types.go` 約 104 行後),
  加入 `IsKnownEventType`(112-140)。
- 新 typed payload 與 `ValidateEventPayload` case(`event_payloads.go:85-145`),採 strict decode
  (`DisallowUnknownFields` + trailing value 檢查,precedent `EventRunInputsResolved` 111-129),要求 `event.TaskID` 非空。

```go
type TeamActionProposedPayload struct {
    SchemaVersion      int                     `json:"schema_version"` // 1
    Status             string                  `json:"status"`         // "proposed"(讓 inspect trace 有狀態)
    ProposalID         string                  `json:"proposal_id"`
    ActionID           string                  `json:"action_id"`
    EntryHash          string                  `json:"entry_hash"`
    CatalogHash        string                  `json:"catalog_hash"`
    Agent              string                  `json:"agent"`
    OccurrenceRevision int                     `json:"occurrence_revision"`
    Attempt            int                     `json:"attempt"`
    Arguments          json.RawMessage         `json:"arguments"`
    ArgumentsHash      string                  `json:"arguments_hash"`
    Assessment         string                  `json:"assessment"`
    Rationale          string                  `json:"rationale"`
    ExpectedOutcome    string                  `json:"expected_outcome,omitempty"`
    EvidenceRefs       []TeamActionEvidenceRef `json:"evidence_refs,omitempty"`
}

type TeamActionEvidenceRef struct {
    ID             string `json:"id"`
    SHA256         string `json:"sha256"`
    ProducerTaskID string `json:"producer_task_id"`
}
```

Payload 的每個 JSON 欄位名稱都不得命中 `utils.IsRedactedJSONKey`(`redact.go:59` 的 regex 也涵蓋 `passwd`、
`apikey`/`api-key` 等變體);以測試反射列舉所有 tag 驗證。

### 10.2 Identity 與 append

- Idempotency key:`team-action-proposal:v1:<todoID>:<occurrenceRevision>:<attempt>:<actionID>:<argumentsHash>`
  (含 attempt,讓同一 occurrence 的 retry 可以用不同 rationale 重新提案而不衝突)。
- `ProposalID = "tap_" + hex(sha256(branchID + "\n" + key))[:24]`,`branchID = c.durableBranchID()`(D17)。
  **不得**使用 `executionRunID`。
- **先查 index**:index 以 idempotency key 為索引。key 已存在 → 比較既有 proposal 與本次內容
  (assessment、rationale、expected_outcome、evidence 的 canonical hash);相同 → 成功並 `duplicate: true`,
  不 append;不同 → `team_action_proposal_conflict`。(`Append` 的回傳值無法分辨「新寫入」或「去重命中」,
  所以不能靠比對回傳 payload 判斷。)
- key 不存在才 append:**不要用 `emitEvent`**(沒有 eventStore 時會靜默成功)。照
  `recordResourceClaimsResolved`(`coordinator_resource_scope.go:556-564`):
  `c.EventJournal().Append(context.WithoutCancel(ctx), RunEvent{Type, Actor: agent, TaskID: todoID,
  Attempt, IdempotencyKey: key, Payload})`,錯誤要回傳;成功後以回傳的 durable record 更新 index。
  「查 index → append → 更新 index」整段在 index mutex 內完成,避免並行的 worker 以相同 key 重複寫入。

### 10.3 Session-scoped index

- Coordinator 新增 `actionProposals`(mutex 保護;依事件 ordinal 排序的 slice + by-ID map)。
- 在 `initEventStore`(`coordinator_eventstore.go:152-159`,telemetry hydration 旁、pending-terminal
  early return 之前)先清空,再以 `FilterEventsForBranch(events, tree, activeBranch)`
  (`session_tree.go:497-503`)過濾後重建。initEventStore 在每次公開入口(含 resume)都執行。
- index 同時維護 idempotency key → proposal 的 map(§10.2 去重用);重建時取自 `RunEvent.IdempotencyKey`。
- 查詢 API:`matchingProposals(actionID, entryHash, argumentsHash) []TeamActionProposal`、
  `proposalsForAction(actionID) []TeamActionProposal`(新到舊)、`proposalCount() int`。
- `--new`(新 root branch)自然看不到舊 session 的 proposal。

---

## 11. Coordinator 工具與派工介面

### 11.1 Coordinator 唯讀工具

在 `buildOrchestratorToolsFor`(`coordinator_run.go:1192-1231`)中,team 有 catalog 時加入
coordinator 版 `team_action_list` / `team_action_get`(一般 slice 1212-1219 與 forcePlanFirst slice 1195-1199),
名稱加入 `coordinatorCoreToolNames`(1161-1175)。**不要**加入 `c.coreTools`(`coordinator.go:1579-1595`,
那是 worker 工具池)。可加入 `coordinatorInitialReadOnlyTools`(1283-1289)。

Coordinator 版 list 輸出每個 entry:`id`、`description`、`agent`、`side_effect`、`recovery`、
`require_proposal`、`allow_unattended`、`max_invocations`、`invocations_used`、
`dispatchable_now`(bool)與 `blocked_reason`(`phase`、`unattended`、`budget`、`decision_profile`、`journal`、
`initial_batch`;與 §12.3 共用同一判斷函式)、
`proposal_counts`({recommended, candidate, defer, reject})。

Coordinator 版 get 額外輸出 input/output schema 與該 action 最新 50 筆 proposal:
`proposal_id`、`agent`、`task_id`、`assessment`、`arguments`(canonical)、`arguments_hash`、
`entry_hash`、`rationale`、`expected_outcome`、`evidence_refs`。
Coordinator 版 list/get 同樣不輸出 `provider_identity_hash`(D11)。

`Info()` 同樣必須決定性(只依 catalog)。

### 11.2 Prompt

`BuildOrchestratorPrompt`(`coordinator_prompt.go:76`)在約 107 行後新增 `c.appendActionCatalogPrompt(&b)`
(pattern:`appendRuntimeWorkflowPrompt` 45-56),只在有 catalog 時輸出固定短文,不 dump catalog:

```text
This team defines predefined runtime actions. Use team_action_list and team_action_get to inspect them
and the workers' proposals. To run one, add a task to the agent tool with the entry's agent, a short goal,
and catalog_action {"id": ..., "arguments": {...}}. Arguments must match the entry's input schema.
Workers can only recommend actions; they cannot run them.
```

Worker prompt 不修改;指引寫在工具 description。

### 11.3 `agent` 工具 schema

在 `runAgentsTool.Info`(`coordinator_tools.go:30-91`)**workflow 壓縮之後**(約 43 行後)、且
`!c.initialDelegationPending()` 時加入(不要改 `buildAgentTaskProperties` 的簽名,測試依賴之):

```json
"catalog_action": {
  "type": "object",
  "properties": {
    "id": {"type": "string", "enum": ["<dispatchable entry IDs>"]},
    "arguments": {"type": "object", "description": "Arguments matching the action's input schema (see team_action_get)."}
  },
  "required": ["id"],
  "additionalProperties": false
}
```

- 不使用 oneOf/anyOf/pattern/const/not/dependentRequired/prefixItems(portability 測試禁止)。
- **Phase-eligible entries** 定義:動態 team 為全部 entry;workflow team 在 EXECUTE 為全部 entry、PREPARE 為
  side-effect none 的 entry、其他 phase 為空。只依 phase,**不**依 budget、proposal、unattended 狀態,
  讓 `Info()` 在同一 phase 內穩定。
- `id` 的 enum = phase-eligible entry ID(排序);超過 32 個時省略 `enum`,description 改為「see team_action_list」,
  由 runtime 驗證(控制 schema 大小,local grammar 相容)。
- phase-eligible 為空時(例如 AUDIT/VERIFY,或 PREPARE 只有 workspace_write entry)整個省略 `catalog_action` property。
- Workflow 模式下 `agent` enum 與 phase-eligible entry 的 `agent` 取聯集(排序、去重)。
- 動態 team 的 entry `agent` 已由載入驗證保證在 `workerNameList()` 內。
- 已知限制:fantasy 在同一個 stream 內不重新整理 tool schema(`coordinator_run.go` 約 1245),所以 initial batch
  pending 時隱藏的 `catalog_action` 在該 stream 內都不會出現;下一個 stream 才出現。文件(WP-10)需說明。
- 沒有 catalog 時 schema 必須與 baseline 完全相同;workflow 模式既有 schema 大小測試(< 12000 bytes)照舊,
  另加一個 32-entry catalog 的 schema 大小測試(< 16000 bytes)。

---

## 12. 派工編譯規則

新檔 `internal/team/team_action_dispatch.go`。

### 12.1 Decode

`decodeModelTaskDefs`(`coordinator_tools.go:222-257`)在既有 runtime-owned 欄位與 `execution` 檢查之後
(235-249;這些錯誤以及 goal 檢查 152-156 維持既有的 "invalid arguments"/terminal 行為)、`json.Unmarshal` 之前:
以 `strings.EqualFold` 尋找 `catalog_action` key;若存在:

- 以 token scan(比照 `decodeUniqueJSON`)檢查 task object 與 `catalog_action` object,拒絕完全相同或僅大小寫
  不同的重複 key(`map` decode 與 Go 的大小寫不敏感比對都會靜默吃掉後者)。
- task object 的 key(EqualFold 比較)只能是 `agent`、`goal`、`constraints`、`catalog_action`,
  動態 team 另加 `depends_on`(workflow team 不允許,D13);其他任何 key(含 legacy 別名 `task`、
  `strict_result`、`strict-result`)→ 錯誤 `team_action_task_field_forbidden`。
- `catalog_action` strict decode 成 `{ID string; Arguments json.RawMessage}`(DisallowUnknownFields);
  `null`、非 object 的 arguments → `team_action_arguments_invalid`。缺 arguments 視為 `{}`。
- decode 需要知道是否為 workflow 模式:把 `decodeModelTaskDefs` 的 catalog 分支實作成可接收
  `workflowMode bool` 的內部函式,既有簽名保留給其他呼叫者。
- 設定 `task.CatalogInvocation = &CatalogInvocation{ID, Arguments}`(新 TaskDef 欄位
  `json:"-" yaml:"-"`,暫態)。
- 上述錯誤以新型別 `*teamActionDispatchError{Index int; Code string; Detail string}` 回傳
  (`Error()` 格式 `"<code>: tasks[<index>]: <detail>"`),讓 Run 能分辨它與一般 decode 錯誤。

沒有 `catalog_action` 的 task 行為完全不變(`TestDecodeModelTaskDefsRetainsRuntimeContractFieldsOutsideProviderSchema`
必須照舊通過;既有一般 decode 錯誤仍是 "invalid arguments" 的既有行為)。

### 12.2 編譯位置與錯誤回報

`runAgentsTool.Run`(`coordinator_tools.go:144`):

1. `decodeModelTaskDefs` 回傳 `*teamActionDispatchError` 時,走 catalog 拒絕路徑(下述),不回 "invalid arguments"。
2. 在 goal 檢查之後、`validateDelegatedTaskCapabilities`(157)之前:若任何 task 帶 `CatalogInvocation`,先呼叫
   `c.AdmitExecutionPolicy()`(讓 resume 時的 catalog drift 以既有 drift 錯誤呈現,而非 catalog 拒絕;錯誤處理同
   ExecuteTasks 回傳錯誤時的既有路徑),再呼叫 `c.compileCatalogActionTasks(ctx, tasks)`,錯誤同樣是
   `*teamActionDispatchError`。
3. **Catalog 拒絕路徑**(D14,新檔 `team_action_dispatch.go`):
   - 以下情況**不**使用 D14 回應,直接走既有 policy repair(`c.rejectDelegationPolicy(...)` + 與
     `coordinator_tools.go:162-170` 相同的 violation 處理;把該段抽成 helper 供兩處共用):
     (a) `c.coordinatorPolicyRepairPending.Load()`(repair 期間只能 agent/finish,D14 的指引會自相矛盾);
     (b) `c.IsWrapUp() && !c.acceptanceRecovery.Load()`(wrap-up 拒絕任何新派工,`coordinator_execute.go:161-170`);
     (c) 本 invocation 的 D14 拒絕次數已達 3。
   - 其餘情況:Coordinator 新增 `teamActionRejections atomic.Int32`(每次公開 invocation——run 或 resume——共用一個計數,
     不分 action;在 `coordinatorPolicyRepairsAttempt.Store(0)` 同處 `execution_events.go:299` 歸零)。
     `teamActionRejections.Add(1)`;回
     `fantasy.NewTextErrorResponse(teamActionDispatchRejectedPrefix + " " + dispatchErr.Error() + "\n" + guidance)`,`err == nil`。
   - guidance 為固定文字:「No task was created. Do not retry the rejected call unchanged. Use team_action_get to check
     the input schema and proposals, dispatch workers to gather evidence or record a proposal, or continue without this
     action.」(同一 (tool, input) 在兩次錯誤後第三次呼叫會被既有 loop detector 以 `toolLoopError` 終止,
     `coordinator_task_run.go:4240-4244`、`4304-4310`。)
   - 常數 `teamActionDispatchRejectedPrefix = "TEAM ACTION DISPATCH REJECTED:"`。
   - `initialDelegationPending()` 時的拒絕(§12.3 #2)發生在 initial delegation 的一次性 CAS
     (`delegation_policy.go:22-24`)之前,**不消耗** initial delegation attempt;這與其他在 ExecuteTasks 之前被拒的
     錯誤一致,刻意保留。
4. **放行不需要額外機制**(2026-09-24 `dfc083f` 之後):coordinator 工具的 error response(`err == nil`)預設就回給模型
   (`coordinatorToolFailureResult`,`coordinator_tool_errors.go`),只有 Go error、tool 標記的 fatal、或連續超過
   `maxConsecutiveCoordinatorToolErrors`(3)次錯誤才終止。D14 拒絕回應是一般 error response,因此天然可恢復;
   **不要**為它回傳 Go error,也不要用 `markCoordinatorFatal`。它會計入連續錯誤 streak,所以 guidance 要求模型換一種呼叫。
   `OnToolResult` 不是終止邊界(fantasy v0.41.1 忽略它的回傳值),不需修改。
5. 拒絕**不**設定 `coordinatorPolicyRepairPending`、不增加 `coordinatorPolicyRepairsAttempt`,所以不影響
   repair-pending 的工具限制與已完成 worker 的重派鎖定。

`ExecuteTasks` 開頭(`coordinator_execute.go:91` 後)加防禦檢查:任何 task 的 `CatalogInvocation != nil`
→ `c.rejectDelegationPolicy("catalog_action was not compiled")`。

測試(以 `createGatedAgent`/`policyGatedTool` 包裝的 coordinator `agent` 工具,經 fantasy stream):
- 拒絕後 run 不終止,coordinator 下一回合可呼叫 `team_action_get` 與 `agent`(派已完成過的 worker 也不被鎖定);
- 回應含前綴與拒絕碼;沒有 Todo、沒有 `task_created`;
- 三次拒絕(每次 input 不同,避開 loop detector)後,第 4 次進入 policy repair(`COORDINATOR POLICY REPAIR REQUIRED`);
- repair pending 或 wrap-up 中的 catalog 拒絕直接走 policy repair;
- 連續 D14 拒絕與其他 coordinator 工具錯誤合計超過 3 次時,run 依 `dfc083f` 的 streak 規則終止。

### 12.3 編譯步驟(每個帶 `CatalogInvocation` 的 task,依序)

| # | 檢查 | 拒絕碼 |
|---|---|---|
| 1 | team 有 catalog | `team_action_catalog_absent` |
| 2 | `!c.initialDelegationPending()` | `team_action_initial_batch_pending` |
| 3 | `c.hasDurableEventJournal()` | `team_action_journal_required` |
| 4 | entry 存在(完全比對 ID) | `team_action_unknown` |
| 5 | `AgentPool().ResolveAgentName(task.Agent)` 解析後的 canonical 名稱 == entry.Agent | `team_action_agent_mismatch` |
| 6 | workflow enabled 時:state 為 EXECUTE,或 PREPARE 且 entry side-effect none;設 `task.Phase = state` | `team_action_phase_forbidden` |
| 7 | `c.IsUnattended() && !entry.AllowUnattended` | `team_action_unattended_denied` |
| 8 | `canonicalizeCatalogArguments(entry.InputSchema, arguments)` | `team_action_arguments_invalid` / `team_action_arguments_not_redaction_stable` |
| 9 | 同批次不得有兩個相同 (ID, arguments hash) | `team_action_duplicate_in_batch` |
| 10 | 連結 proposal(D6);`RequireProposal` 時至少一筆 recommended | `team_action_proposal_required` |
| 11 | `used + 同批次前面同 ID 數 + 1 <= MaxInvocations`,used = `c.taskTracker.TodoList().Items()` 中 `CatalogAction.ActionID == id` 且非 `IsPrimaryOccurrence` 的數量 | `team_action_invocation_budget_exceeded` |
| 12 | `DecisionProfile = "off"` 後以 `ResolveDecisionProfile(...)` 解析,enabled 則拒絕(D5) | `team_action_decision_profile_unsupported` |

### 12.4 編譯結果

產生**全新**的 TaskDef(不沿用 decode 出的其他欄位):

```go
TaskDef{
    Agent:           entry.Agent,
    Goal:            req.Goal,          // operator/debug 顯示,不影響 action identity
    Constraints:     req.Constraints,
    DependsOn:       req.DependsOn,     // workflow 模式 schema 不提供(D13)
    Phase:           phase,             // 動態 team 為 ""
    Action:          &Action{Capability: entry.Capability, Type: entry.Type, Payload: string(canonical)},
    SideEffect:      entry.SideEffect,
    Recovery:        entry.Recovery,
    MaxRetries:      0,
    DecisionProfile: "off",
    CatalogAction: &CatalogActionBinding{
        ActionID: entry.ID, EntryHash: entry.Hash, CatalogHash: snapshot.Hash,
        ArgumentsHash: argsHash, ProposalIDs: linked, // InvocationID 於 §13.3 補上
    },
    CatalogInvocation: nil,
}
```

---

## 13. ExecuteTasks 整合

### 13.1 對 catalog task 跳過或調整的 stage(判斷式 `task.CatalogAction != nil`)

| Stage | 位置 | 處理 |
|---|---|---|
| `CompileTaskGoalContracts` | `contract_compile.go:123` 迴圈 | 跳過 catalog task(兩次 bind 都經過此函式,`coordinator_execute.go:97`、`124`) |
| `CompileInitialTaskContracts` | `contract_compile.go:47-102`(65) | **必須**跳過:`BindInitialTaskContracts && !RequireExactInitialBatch` 的動態 team 在 `initialDelegationPending()` 為 false 時仍會綁定(`delegation_policy.go:352-354`、`404-455`),§12.3 #2 擋不到 |
| `phaseWorkflow.validateTasks` | `runtime_workflow.go:456-496` | 見 §16.2 |
| `validateTaskGoalInvariants`、`validateCapabilityRouting` | `delegation_policy.go:59-62`、`capability_registry.go:355` | 跳過 catalog task |
| policy repair 重派檢查、`no-redispatch-after-success` | `delegation_policy.go:65-113` | 跳過 `task.Action != nil` 的 task |
| allowed-workers | `delegation_policy.go:25-42` | 不變(載入驗證已保證可達) |
| `normalizeOutcomeTaskKinds`、`validateTaskCriterionLinks` | `criteria.go:379`、`311` | 跳過 catalog task |
| `forcePlanFirst` | `coordinator_execute.go:243-249` | 跳過 catalog task |
| `CheckDuplicate` 與失敗 task 抑制 | `coordinator_taskcache.go:690-713`、`749-857`;`coordinator_execute.go:294`、`566-576` | 雙向跳過:(a) 本批的 catalog task 不參與判重;(b) 既有 Todo 中 `CatalogAction != nil` 的 item 也不得拿來比對後續的一般 task——`findExistingTodoDuplicate`(690-712)與四個 pass(含約 836 的 semantic pass)都要略過(比照 InvariantVerification 757/774/801/815)。重複由 §12.3 #9 與 #11 控制 |
| 其他 stage | — | 不變(fan-out、fact_refs、materializeTaskActions、RequiresResult 預設、資源鎖、`serializeConflictingMutationTasks`、admission、`CommitTaskCreationResolved` 皆可直接通過) |

### 13.2 TodoSpec

`coordinator_execute.go:341-401` 的 TodoSpec literal 加入 `CatalogAction: t.CatalogAction.clone()`。

### 13.3 CatalogInvocationID

在 `ReserveIDs` 與 DependsOn/OnFailure ID 解析之後(`coordinator_execute.go:441-458`)、envelope 與
admission 之前,對每個 catalog task:

```text
binding = clone(binding)   // binding 視為 immutable;cloneTaskDef(coordinator_dryrun.go:265-298)必須 deep copy 新指標欄位(§14 #16)
binding.InvocationID = "tai_" + hex(sha256("hufu.team-action-invocation.v1\n" + c.durableBranchID() + "\n" + todoID + "\n" + entryHash + "\n" + argumentsHash))[:32]
```

TodoSpec literal(341-401)在 `ReserveIDs`(415)之前建立,所以此處必須同時更新 `tasks[i]` 與 `todoBatch[i]`
的 binding,確保 admission digest(508-527)與 `task_created` 看到相同的 InvocationID。

### 13.4 Coordinator 看到的結果

不新增格式。`formatTaskResults`(`coordinator_taskcache.go:859-904`)照舊輸出
`## Agent / Status / Todo ID / output`;action 輸出為 `actionResultDisplay`
(`coordinator_task_run.go:2563-2571`)。完整 typed result 可用 `team_info task_result` 讀取。

---

## 14. Durable binding plumbing

新檔 `internal/team/team_action_binding.go`:

```go
type CatalogActionBinding struct {
    ActionID      string   `json:"action_id"`
    EntryHash     string   `json:"entry_hash"`
    CatalogHash   string   `json:"catalog_hash"`
    ArgumentsHash string   `json:"arguments_hash"`
    InvocationID  string   `json:"invocation_id"`
    ProposalIDs   []string `json:"proposal_ids,omitempty"`
}
func (b *CatalogActionBinding) clone() *CatalogActionBinding
```

`ProposalIDs` 為空時一律存成 `nil`(compiler 與 `clone()` 都要正規化):`compareTaskDefWithTodoOccurrence` 用
`reflect.DeepEqual`,空 slice 與 nil 不相等會讓 scheduler 比對失敗(`task_occurrence_projection.go:245`)。

照 `ExecutionRoute *ExecutionRouteBinding` 的最新 precedent,每個結構只加一個欄位:

| # | 位置 | 變更 |
|---|---|---|
| 1 | `TaskDef`(`coordinator.go:99-109`) | `CatalogAction *CatalogActionBinding \`json:"-" yaml:"-"\``、`CatalogInvocation *CatalogInvocation \`json:"-" yaml:"-"\`` |
| 2 | `TodoSpec`(`status.go:443-523`) | `CatalogAction *CatalogActionBinding` |
| 3 | `TodoItem`(`status.go:251-393`) | `CatalogAction *CatalogActionBinding \`json:"catalog_action,omitempty"\``(經 MarshalJSON 自動進 session.json) |
| 4 | `todoItemFromSpec`(`status.go:564-634`) | clone 複製 |
| 5 | `cloneTodoItem`(`status.go:1288-1388`) | **必要**,clone 複製 |
| 6 | `restoreTodoOccurrenceContract`(`status.go:1485-1546`) | clone 複製 |
| 7 | `canonicalTaskShadow` / `toCanonicalTaskShadow`(`projection_shadow.go:311-397`、`417-501`) | `omitempty` 欄位 |
| 8 | task_created payload(`coordinator_eventstore.go:1444-1619`,約 1597) | `if item.CatalogAction != nil { payload["catalog_action"] = clone }`(legacy 事件 byte 不變) |
| 9 | Reducer(`event_reducers.go` payload struct 606-702、初始化 734-817、merge 約 1050) | decode + 條件合併 |
| 10 | `TaskOccurrenceProjection`(`task_occurrence_projection.go:21-96`、120-153、172-210) | 欄位 + 兩個建構函式 |
| 11 | `decisionOccurrenceInputDigest`(`decision_admission.go:132-201`) | `CatalogAction *CatalogActionBinding \`json:"catalog_action,omitempty"\``(**必須**有 tag;precedent 是同 struct 的 `DynamicToolAuthorization`、`ResourceScopeSnapshot`,171-172。ExecutionRoute 不在此 digest) |
| 12 | `taskDefFromTodoItem`(`coordinator_session.go:1300-1324`) | clone 複製 |
| 13 | `compareTaskDefWithTodoOccurrence`(`task_occurrence_projection.go:217-250`) | 不需改(欄位 round-trip;`CatalogInvocation` 編譯後為 nil) |
| 14 | `ActionEnvironment`(`action_provider.go:325-333`) | `CatalogInvocationID string`;env 組裝(510-533)非空時加 `HUFU_CATALOG_INVOCATION_ID` |
| 15 | `executeRuntimeAction` 設定 ActionEnvironment(`coordinator_task_run.go:2466-2469`) | 傳入 `task.CatalogAction.InvocationID` |
| 16 | `cloneTaskDef`(`coordinator_dryrun.go:265-298`) | deep copy `CatalogAction`(`.clone()`);`CatalogInvocation` 也 deep copy |
| 17 | `journalRecord`(`task_journal.go:58`、`179`,"result" op) | 比照 ExecutionRoute 攜帶 `CatalogAction` |

### 14.1 Provider 啟動前的完整性檢查

`executeRuntimeAction` 在 `validateMaterializedActionIdentity`(`action_materialization.go:155`)之後、provider 之前,
若 `task.CatalogAction != nil`:

1. `session.ActionCatalog.Lookup(ActionID)` 不存在或 `entry.Hash != binding.EntryHash` → `ActionValidationError`
   (Cause 以 `team_action_catalog_drift:` 開頭)。正常情況下 policy snapshot drift 會先擋下,此處是防禦。
   因為 `entry.Hash` 含 `ProviderIdentityHash`(D19),provider 設定變更也由此檢查涵蓋。
2. `runInputHash([]byte(task.Action.Payload)) != task.CatalogAction.ArgumentsHash` → 回傳 `ActionValidationError{Capability, Cause: fmt.Errorf("team_action_arguments_drift: …")}`
(`ActionValidationError` 只有 `Capability`、`Cause` 兩個欄位,`action_provider.go:552-555`;代碼放在 Cause 文字開頭),
provider 不啟動、不 retry。
此檢查放在新檔函式,`coordinator_task_run.go` 只加一行呼叫。

---

## 15. Action 執行路徑:與 phase workflow 解耦

### 15.1 `ActionsEnabled`

`runtime_workflow.go`:

- struct 新增 `actionsEnabled bool`;新增 `func (w *runtimeWorkflow) ActionsEnabled() bool { return w != nil && (w.enabled || w.actionsEnabled) }`。
- `newRuntimeWorkflow` 的 early-return 分支(52-54):若 `session.ActionCatalog != nil && len(Entries) > 0`,
  設 `actionsEnabled = true`,並設定 `team`、`repositoryRoot`(同 enabled 分支的 fallback 規則)、
  `workspace = RuntimeWorkspace{Root: <session.Workspace>/runtime}`(`ensureRuntimeWorkspace`)、
  `registry = session.ProviderRegistry`、`retryPolicy = session.Config.Retry`。**不設** `enabled`。
- 新增不依賴 `Enabled()` 的 accessor `runtimeWorkspace() RuntimeWorkspace`;**不要**修改 `executionContext()`
  (它在 disabled 時回空值,且被 phase 邏輯依賴)。

### 15.2 需改成 `ActionsEnabled()` 的位置

| 位置 | 變更 |
|---|---|
| `executeActionValueForTask`(`runtime_workflow.go:135`) | 閘控改 `ActionsEnabled()`;phase gate(142-145)只在 `w.enabled` 時套用 |
| `permitActionRetry`(`runtime_workflow.go:214`) | 改 `ActionsEnabled()`(WP-0.1 的 replay 檢查保留) |
| `allocateRuntimeActionWorkspace`(`coordinator_task_run.go:2378`、`2387`) | 改 `ActionsEnabled()` 與 `runtimeWorkspace()` |
| `emitRuntimeActionEvent`(`coordinator_task_run.go:2688`、receipt 寫入 2755) | 改 `ActionsEnabled()` 與 `runtimeWorkspace()`;workflow disabled 時 payload `Phase` 為 `""` |
| `actionExecutionError`(`runtime_workflow.go:255`) | workflow disabled 時 phase 用 `""` |
| `executeRuntimeAction`,緊接 `validateMaterializedActionIdentity`(`coordinator_task_run.go:2428`)之後 | 新增:`!c.phaseWorkflow.Enabled() && task.CatalogAction == nil` → `return "", fmt.Errorf("action invocation requires an enabled runtime workflow")`(與今天 `allocateRuntimeActionWorkspace` 2378-2379 的訊息相同),且**不**呼叫 `emitRuntimeActionEvent`(今天此路徑不發事件)。確保解耦只開放給 catalog task |

### 15.3 必須維持 phase-only(不得改成 ActionsEnabled)

worker tool-call 的 action_* 事件(`coordinator_task_run.go:4212-4224`、`4336-4361`)、
`permitRepairRetry` / `repairRetryLimit`(另見 WP-0.4)、`publishRuntimeWorksetProjection`、
`validateTasks`/`observe`/`fail`/`cancel`/`requireFinished`/`snapshot`/`restore`、coordinator schema 壓縮、
direct-agent 停用(`coordinator_run.go:385`)、`runtimeAllowedPaths`/`runtimeAllowedWritePaths`、
`taskMutableRoot`、context snapshot 與 tool gating(`tool_deny.go` 各處)。
結果:沒有 catalog 的動態 team 行為完全不變;有 catalog 的動態 team 只有 catalog action task 會使用 action runtime。

### 15.4 修正誤導註解

`parse.go:1191-1194` 註解宣稱「providers are available to … runtime action tasks in every supported team shape」,
改為如實描述:action 需要 workflow,或 team 有 action catalog。

---

## 16. Workflow team 規則

### 16.1 允許的 phase

- EXECUTE:所有 catalog entry。
- PREPARE:只有 `side-effect: none` 的 entry。
- 其他 phase、INIT、DONE、FAILED:拒絕(§12.3 #6)。
- 最後一個 static EXECUTE contract 成功後 `observe` 會立即推進到 VERIFY,之後就不能再派 catalog action。
  catalog action 只能在 static contract 之前或同批派出。這是既有 phase 語意,文件(WP-10)需說明。

### 16.2 `validateTasks` catalog 分支(`runtime_workflow.go:456-496`)

- 整批層級檢查(462-467:INIT/DONE/FAILED)保留。
- 對 `task.CatalogAction != nil` 的 task:`task.Phase` 必須等於 `w.state`;state 必須為 EXECUTE,或 PREPARE 且
  `SideEffect == none`;然後 `continue`,跳過 phase agent(476)、static contract(479)、dispatch-once(485-488)檢查。
- 「必須派齊 static contract」迴圈(490-494)只在本批含至少一個非 catalog task 時執行。
- 既有 `TestRuntimeWorkflowRequiresEveryStaticContractAndRestoresCheckpoint`(`runtime_workflow_test.go:137-166`)必須照舊通過。

### 16.3 Phase 簿記

`observe` 不需修改:catalog task 的 `ContractID` 為空,不在 `phaseContracts` 內(518 行會略過)。
catalog task 失敗不會讓 phase 失敗,但它仍是失敗 Todo:`finish` 需要 `acknowledge_failed_tasks`、
`require-no-unresolved-tasks` acceptance 會視為未解決;依賴它的 static contract 會被 `markStranded` block。
這些都是既有 DAG 語意,不需新增例外。

---

## 17. 輸出驗證、receipt 與 lifecycle 事件

### 17.1 輸出 schema 驗證

WP-0.3 已把 `CanonicalizeRuntimeOutputs` 提前到 `decodeActionResult` 之後;緊接在 canonicalize 之後:若
`task.CatalogAction != nil` 且 entry(§14.1 已驗證過 hash)有 output schema,呼叫 `validateCatalogOutputs`,失敗 →
`ActionValidationError`(Cause 以 `team_action_output_invalid:` 開頭)。
`ActionValidationError` 讓 `permitActionRetry` 回 false、`actionExecutionError` 分類為不可重試
(`runtime_workflow.go:220-223`、`262-266`)。Provider 已執行,不重試是正確的。

### 17.2 Receipt 與事件欄位(全部 additive、omitempty)

| 結構 | 位置 | 新欄位 |
|---|---|---|
| `runtimeActionReceipt` | `coordinator_task_run.go:2666-2685`(填值 2705-2716) | `CatalogActionID`、`CatalogEntryHash`、`ArgumentsHash`、`CatalogInvocationID`、`ProposalIDs`、`SideEffect`;Version 維持 2 |
| `LifecycleEventPayload` | `execution_events.go:151-159`(填值 `coordinator_task_run.go:2739-2750`) | `CatalogActionID`、`CatalogEntryHash`、`ArgumentsHash`、`CatalogInvocationID` |
| `ExecutionReceipt` | `execution_receipt.go:147-237`(`ActionInvocationID` 旁);填值 `coordinator_terminal_receipt.go:60-71`;clone `status.go:1758` | `CatalogActionID`、`CatalogEntryHash`、`ArgumentsHash`、`CatalogInvocationID` |

填值邏輯放在新檔 helper(例如 `catalogReceiptFields(task)`),大檔案只加呼叫。
不要動 `execution_event_exporter.go`:它是 legacy parity shadow,新增映射會破壞 parity(215-218)。

---

## 18. Recovery、resume 與 crash 語意

Catalog task 就是普通 action task,沿用既有 recovery:

| Crash 點 | Resume 行為 |
|---|---|
| `task_created` 之前 | 沒有 Todo、provider 未啟動;coordinator 可再派(只有已 admit 的 Todo 計入 max-invocations) |
| `task_created` 後、未開始(Pending) | `ResumeInterruptedTasks`:`retry` → 執行同一 Todo;`manual` → blocked needs_human;`never` → skipped(既有語意) |
| `action_started` 後、無 terminal | `retry`(none 的預設;workspace_write 須明確設定,D21)→ 以**相同** `HUFU_CATALOG_INVOCATION_ID` 重跑同一 Todo;`manual` → blocked;`never` → skipped |
| TypedResult/ExecutionReceipt 已存、`TaskDone` 前 | 視為中斷 → `retry` 會重跑(既有缺口 E-12);provider 必須以 invocation ID 冪等 |
| `TaskDone` 已提交 | 不會重跑;結果經 `team_info task_result` 取得 |
| coordinator 回應遺失 | coordinator 重新規劃;已完成 Todo 可見;再派會計入 max-invocations |

- `workspace_write` 在結構上可 replay(`IsTaskReplayable`),但 catalog entry 必須明確選擇 recovery(D21):
  選 `retry` 等於 maintainer 聲明「provider 以 `HUFU_CATALOG_INVOCATION_ID` 冪等,可安全重跑」,hufu 不驗證這項聲明;
  無法保證冪等的 entry 應設 `manual`(resume 時 blocked needs_human)或 `never`。`side-effect: none` 預設 `retry`。
- 信任邊界(D2、D21):provider 以 hufu 的 cwd 與完整環境變數執行,沒有檔案系統或網路沙箱;`side-effect`
  分類只決定 hufu 的排程、replay 與 commit gate 行為,不限制 provider 實際能做什麼。WP-10 文件需寫明此點,以及
  provider 必須以 `HUFU_CATALOG_INVOCATION_ID` 實作冪等才可設 `retry`。
- 動態 team 沒有 workflow retry 設定(`parse.go:1156-1162` 只在有 phases 時複製 policies/retry),
  catalog task `MaxRetries = 0`,所以動態 team 不做 in-run transient retry。

---

## 19. 觀測:inspect、report、CLI

### 19.1 `hufu inspect task --run <run> <task-id>`

- `TaskData`(`internal/inspect/run.go:50-68`)新增 `SideEffect string \`json:"side_effect,omitempty"\`` 與
  `CatalogAction *CatalogActionData \`json:"catalog_action,omitempty"\``(ActionID、EntryHash、ArgumentsHash、
  InvocationID、ProposalIDs)。在 `projectTaskWithEvents`(278-367)由 replay 出的 item 填入。
- Text 輸出(`cmd/hufu/inspectcmd.go:408-429`,約 413 行後)在有 catalog 資料時加:

```text
Catalog action: collect-debug-bundle  entry=sha256:…  args=sha256:…
Invocation: tai_…  proposals: tap_…, tap_…
```

- 預設不輸出 arguments 原文(arguments 在 Action.Payload,需 debug bundle 才看得到)。
- Proposal 透過 `hufu inspect trace` 的 `team_action_proposed` 事件查看(payload 帶 `status: proposed`)。

### 19.2 `hufu report`

新檔 `cmd/hufu/report_actions.go`,`writeCatalogActionReport`,在 `report.go` 約 969 行(`writeExecutionRouteReport`
旁,precedent `report_routes.go`)呼叫。只在有 catalog task 時輸出:

```text
## Catalog Actions
| Todo | Action | Status | Arguments hash | Proposals |
```

Arguments 欄只顯示 hash,不顯示原文(§20)。

同時修正 E-18:Task Summary 的 Provider 欄對 action task 顯示 ActionProvider capability,而非 worker subagent provider
(`report.go:212-230`、`944-961`)。

### 19.3 CLI

新檔 `cmd/hufu/teamactioncmd.go`,於 `init()` 註冊到 `hufu team`(precedent `teamprofilecmd.go:21-28`):

```bash
hufu team action list [team-directory] [--team <name>] [--output text|json] [--json]
hufu team action show <action-id> [team-directory] [--team <name>] [--output text|json] [--json]
```

- 參數順序與 team 解析比照 `hufu team profile`(`teamprofilecmd.go:21-48`,`show <profile> [team-directory]`),
  以 `CompileTeam(...)`(`effective_spec.go:222`)載入,catalog 從 `.RuntimeSession().ActionCatalog` 讀取。
- 輸出格式旗標比照 `hufu team check`:主旗標 `--output text|json`、別名 `--json`,經 `resolveOutputAlias`
  (`cmd/hufu/output_alias.go:19-37`,用法見 `teamcheckcmd.go:93-99`)。
- 只讀,不啟動 coordinator/provider。
- 顯示 id、description、capability、type、agent、side-effect、recovery、schemas、access、invocation、entry hash;
  **不顯示** provider command/source/dir/runtime。
- 不提供 invoke 子命令。

---

## 20. Redaction 與隱私邊界

- Catalog 參數必須 redaction-stable(§8.3),因為 `task_created` 與 session.json 都會 redaction,
  且沒有未 redaction 的儲存區。這保證 payload hash 在 resume 後一致。
- Schema property 名稱不得命中 `IsRedactedJSONKey`(§8.1、§8.2)。
- Proposal rationale/expected_outcome 先 redaction 再截斷。
- 任何 model-facing 輸出(worker/coordinator 工具、prompt)不得包含 provider command、source、dir、runtime、digest、
  `provider_identity_hash`。
- Inspect 預設不輸出 arguments 原文。

---

## 21. 安全不變量(每條都要有測試)

| ID | 不變量 |
|---|---|
| S1 | Coordinator 在 catalog task 內無法設定 capability/type/side_effect/recovery/decision_profile/phase/id/contract_*/max_retries/verify(allowlist 拒絕,含大小寫變體) |
| S2 | Worker 沒有任何派工面:`agent` 工具仍為 coordinator-only(`coordinatorOnlyWorkerTools`,`tool_deny.go:25-35`);propose 不建立 task |
| S3 | 未知 action ID 被拒,不建立 Todo、provider Execute 次數 = 0 |
| S4 | Entry 的 capability 必須有 provider(載入失敗) |
| S5 | Worker 與 coordinator 工具輸出不含 provider command/source/dir/runtime |
| S6 | Proposal 不是授權:沒有 require-proposal 時不需 proposal;有 require-proposal 時只有 recommended 才成立;proposal ID 不能被 model 用來指定授權 |
| S7 | 其他 session(`--new`)的 proposal 不會被連結 |
| S8 | Catalog 變更後 resume fail closed(policy snapshot drift) |
| S9 | side-effect 非 none/workspace_write 的 entry 載入失敗 |
| S10 | Action.Payload 與 ArgumentsHash 不符時 provider 不啟動 |
| S11 | side_effect none 的 worker 可以呼叫 list/get/propose(不被當 mutation 拒絕),且這些工具不寫 workspace |
| S12 | 沒有 catalog 的 team:agent 工具 schema、worker/coordinator 工具面、policy snapshot hash、事件、digest 與 baseline 完全相同 |
| S13 | 同一 session 內超過 max-invocations 的派工被拒;resume 後計數仍保留 |
| S14 | Unattended 且 `allow-unattended: false` 時派工被拒 |
| S15 | Catalog 派工拒絕不終止 run、不進 policy repair(前 3 次);拒絕回應是一般 error response,不回傳 Go error |
| S16 | 動態 team 中只有 catalog task 能執行 action;static contract action 行為不變 |
| S17 | Entry 引用的 provider 設定(runtime/source/mode/command/dir/timeout)或 golang 原始碼變更後,resume fail closed(D19) |
| S18 | `workspace_write` entry 未設定 `recovery` 時載入失敗;`none` entry 未設定時為 `retry`(D21) |
| S19 | `require-proposal: true` 且沒有有效提案者時載入失敗;`--worker-model` 覆寫讓所有提案者改走外部 agent backend 時,run 在任何 provider 呼叫前失敗(D20) |

---

## 22. 相容性

- 沒有 `action-catalog`:行為必須完全不變(S12)。既有 compat fixtures(`testdata/team-compat/*`)與
  execution-policy golden 不得變更。
- 不自動把 `action-providers` 轉成 catalog。
- 既有 static task → ActionProvider 路徑不變(除了 WP-0.1 的 replay 修正與 WP-0.3 的順序調整)。
- **刻意的行為變更**(皆為 WP-0 修正既存問題或觀測改善,不受 S12 約束,但各自需要回歸測試):
  - WP-0.1:不可 replay 的 static action 不再做 in-run retry。
  - WP-0.2:`inspect trace` 對 action_* 事件顯示狀態。
  - WP-0.3:outputs canonicalize 失敗時不再 ingest artifact / 寫 workset projection。
  - WP-0.4:**所有動態 team** 的 `on_failure` DAG 回跳開始生效(目前被靜默擋下,hufu-coding 受影響最大)。
  - WP-0.6:policy repair 預算耗盡時產生 LLM-free 部分摘要,而非 hard fail。
  - WP-9(E-18):`hufu report` 對 action task 的 Provider 欄改顯示 ActionProvider。
- `team_action_*` 名稱的保留(`removeToolNames` 與無條件 collision 檢查)只在 team 有 catalog 時啟用,
  沒有 catalog 的 team 若有同名 MCP 工具,行為不變。
- `decisionOccurrenceInputDigest`、task_created payload、`canonicalTaskShadow` 的新增欄位都必須 omitempty /
  條件輸出,既有 session resume 不得出現 digest mismatch。

---

## 23. 既存問題清單

查核 `cb2ee2b` 時發現的問題。「WP-0.x」表示本規格修正;「記錄」表示只記錄、不在本規格修正。

| ID | 問題 | 證據 | 處理 |
|---|---|---|---|
| E-01 | Static action 的 in-run retry 不看 side effect / recovery:provider 錯誤或 timeout 時會對不可 replay 的 action 用掉 retry 預算,隨後 `resetTask` 把它 block,phase 結果被記成 TASK_BLOCKED 而非 PROVIDER_FAILURE | `runtime_workflow.go:213-253`、`dag_scheduler.go:392-401`、`585-590` | **WP-0.1** |
| E-02 | 動態 team 的 `on_failure` DAG 回跳迴圈在 production 永遠被擋:`c.phaseWorkflow` 永遠非 nil,而 `permitRepairRetry` 在 workflow disabled 時回 false(訊息「repair retry … blocked by failure-signature limit」)。hufu-coding team 依賴此迴圈;測試因使用 nil phaseWorkflow 而沒抓到 | `runtime_workflow.go:307`、`dag_scheduler.go:446-449`、`coordinator.go:1490-1500`、`.agent-teams/hufu-coding/team.yaml:41-120`、`hufu_coding_workflow_test.go:47` | **已修正**:WP-0.4,commit `7065b7f` |
| E-03 | `hufu inspect trace` 對 action_* 事件狀態顯示空白:讀 `status`/`outcome`,事件寫的是 `action_status` | `internal/inspect/trace.go:152-171`、`execution_events.go:126`、`154` | **WP-0.2** |
| E-04 | Artifact ingestion 與 workset projection 發生在輸出 canonicalize 之前;canonicalize 失敗時留下孤兒 `current-workset.json`,使整個 run 的 workspace pointer 被捨棄 | `coordinator_task_run.go:2490-2515`、`runtime_workset_projection.go:24-54`、`150-196` | **WP-0.3** |
| E-05 | `docs/reference/action-providers.md` 綁定範例照抄會載入失敗(缺 `side_effect: none`、`when-goal-contains`、`capabilities.required`、`delegation.bind-task-goal-contracts: true`);`docs/README.md` 未連結此文件;未說明 `HUFU_ACTION_INVOCATION_ID` 每 attempt 都變,不能當冪等鍵 | 文件 167-186、`runtime_workflow.go:865-946`、`docs/README.md:79-101` | **WP-0.5** |
| E-06 | `docs/architecture/decision-runtime.md` 只描述工具呼叫的 commit gate,未記載 static action / structured step 在 provider 前的 `commitGateActionDenial` | `decision-runtime.md:1760`、`decision_discipline.go:295-344`、`coordinator_task_run.go:2437-2445` | **WP-0.5** |
| E-07 | Task decision 的最終選項只回報不強制:選到 defer/abandon(no-go)時 task 照樣執行 | `decision_dispatch.go:264-266`(無 `IsNoGo()` 檢查) | 記錄(D4,另案) |
| E-08 | `request_agent` 不在任何 model 工具面(worker 被拒,也不在 `coordinatorCoreToolNames`,`coordinator_run.go:1161-1175`),實際上是死碼;且:pause parent 會 revoke parent 的 submit_result lease;child 失敗時 parent 停在 Paused(Paused→Done 非法);caller 身分取自會被覆寫的全域 snapshot | `tool_deny.go:25-35`、`coordinator_tools_delegate.go:108-109`、`234-248`、`coordinator_eventstore.go:866`、`status.go:215-216` | 記錄 |
| E-09 | `decodeModelTaskDefs` 靜默丟棄 `action`/`phase`/`decision_profile` 等 key;coordinator 可在一般 task 設 `side_effect`/`recovery`/`id`/`contract_*` | `coordinator_tools.go:192-257`、`coordinator.go:86-253`、`decision_types_test.go:20-35` | 記錄(catalog task 以 allowlist 自保) |
| E-10 | `RunInputSchema` 不拒重複 JSON key(後者覆蓋)、`additional-properties` 未設時允許、數字保留原文 | `run_input.go:433-583` | 記錄(catalog 路徑自行加嚴,§8) |
| E-11 | action_* lifecycle 事件不在 typed event catalog;EXECUTE 期間每個 worker 工具呼叫也發同名事件;`emitRuntimeActionEvent` 丟棄 append 錯誤 | `event_types.go:8-140`、`coordinator_task_run.go:2739`、`4212-4224` | 記錄 |
| E-12 | `runtimeActionReceipt` 在 `TaskDone` 之後以非 atomic `os.WriteFile` 寫入,resume 從不讀取;TypedResult/ExecutionReceipt 已存但 TaskDone 前 crash 會被重跑 | `coordinator_task_run.go:2540-2560`、`2754-2772` | 記錄 |
| E-13 | Command provider stdout 沒有上限(golang provider 為 1 MiB) | `action_provider.go:366-368`、`394-400` | 記錄 |
| E-14 | Unattended 不閘控 static action;`credential_mutation` 的拒絕只在 ExecutionWorld.Prepare,ActionProvider 不受影響 | `runtime_workflow.go:134-176`、`execution_world.go:105-115` | 記錄(catalog v1 另有 §12.3 閘控與 D2 限制) |
| E-15 | `ValidateTaskDecisionProfiles` 只在 run setup 執行,`team validate`/`lint` 不檢查;`team validate` 遇第一個錯誤就停;provider 設定錯誤是硬錯誤而非 finding | `decision_config.go:139-158`、`cmd/hufu/team_setup.go:77-83`、`cmd/hufu/teamcmd.go:105-109`、`action_provider.go:231-283` | 記錄 |
| E-16 | `parse.go` 註解宣稱 provider 在任何 team 形態都可供 action task 使用,實際需要 workflow | `parse.go:1191-1194` | **WP-2** 修正註解 |
| E-17 | 推論(未執行驗證):`todo` 工具建立的 pending item 在 resume 時會被執行,因 `getInterruptedTasks` 不過濾 Source | `coordinator_session.go:868-904` | 記錄 |
| E-18 | `hufu report` Task Summary 的 Provider 欄對 action task 顯示 worker subagent provider | `cmd/hufu/report.go:212-230`、`944-961` | **已修正**:WP-9(顯示 `action:<capability>`) |
| E-19 | 動態 team 的 `policies`、`capabilities`、`verification`、`retry` 設定在 parse 時被丟棄(只在有 phases 時複製) | `parse.go:1156-1162` | 記錄 |
| E-20 | Workflow disabled 時 `snapshot()` 回傳 nil retry state,動態 team 的 retry 狀態不會進 checkpoint | `runtime_workflow.go:802-805`、`coordinator_session.go:794-795` | 記錄 |
| E-21 | 動態 team 的 action task 目前一定失敗,且失敗事件被跳過 | §3.1 | **WP-2** |
| E-22 | Delegation policy repair 預算耗盡時 run 會 hard fail,而不是預期的 LLM-free 部分摘要:`runAgentsTool.Run` 回 `errCoordinatorPolicyRepairExhausted`,但 `policyGatedTool` 只在 `err == nil` 時放行,否則重新包成只含 `errCoordinatorToolFailure` 的錯誤(原錯誤型別遺失);fantasy 對本地工具一律忽略 `OnToolResult` 回傳值(只有 `Run` 回傳的 Go error 會結束 stream);`attemptWrapUpRecovery` 的 exhausted 檢查因此落空,改走 tool-failure 分支。目前沒有端到端測試覆蓋 | `coordinator_tools.go:165-168`、`tool_policy_gate.go:377`、`403`、`fantasy@v0.41.1/agent.go:826-834`、`coordinator_task_run.go:4375-4376`、`coordinator_run.go:1701`、`1726` | **已修正**(gate 層):WP-0.6,commit `426b4a8`。端到端目前不經過 gate(E-25),修正前後 run 都能優雅收尾;此修正是 E-25 修好後的必要前提 |
| E-23 | Command provider 的 `dir` 不以 team 目錄為基準解析,而是相對於 process cwd(與 golang provider 的 `source` 不一致) | `action_provider.go:258`、`parse.go:1198` | 記錄 |
| E-24 | Repair pending 期間,coordinator 呼叫不允許的工具在預算耗盡時只得到 exhausted prompt(`err == nil`);`OnToolResult` 的 exhausted 回傳被 fantasy 忽略,`coordinatorPolicyRepairPending` 又被清除,所以 stream 在耗盡後繼續且不再受工具限制 | `tool_policy_gate.go:233-238`、`coordinator_policy_repair.go:70-72`、`coordinator_task_run.go:4375-4376`、`tool_policy_gate_test.go` 約 618-627 | 記錄(修正會改變既有測試預期,另案) |
| E-25 | **Coordinator 從第 2 步起繞過 policy gate 與 protocol repair**:`coordinatorRequestPreflight.prepare` 在 `stepNumber > 0` 且請求放得下時回傳 `fullTools` 以「還原完整工具集」,但 `fullTools` 是 `buildOrchestratorToolsFor` 產生的**未包裝**工具(`newCoordinatorRequestPreflightWithAdmission(..., orchTools, ...)` 在 `createGatedAgent` 包裝之前建立);PrepareStep 以 `result.Tools = preflightTools` 覆寫,fantasy 其後沿用。結果:只有第 1 步的 coordinator 工具呼叫經過 `policyGatedTool`(`authorizeToolInvocation`、repair-pending 限制、initial-tool 順序、coordinator error response 的 terminal 邊界、commit gate)與 `protocolRepairWrapper`(參數 schema 驗證)。2026-09-24 以 eval `policy-repair-exhaustion` 的 stack trace 證實:第 2~4 次 `agent` 呼叫由 fantasy 直接呼叫 `runAgentsTool.Run`。**實機證據(kvmforge-verify,2026-09-24)**:套上實驗修正(preflight 綁定 gated 工具)後,coordinator 在 step 3 派工時 `verify` 寫成 `... | grep -c`,contract 檢查回 `verifier_not_asserting`(ExecuteTasks 的一般錯誤),被轉成 `coordinator direct tool failure`,run 在建立任何 VM 前終止;同一 prompt 用目前 binary 則連續兩次被 contract 檢查拒絕後自行修正並繼續。另外本機只有 1 個既有 eval(terminal-leak)和新 eval(policy-repair-exhaustion)會因修正而改變行為 | `coordinator_preflight.go:180-188`(`if stepNumber > 0 { return fullSystem, fullTools, true, nil }`)、`coordinator_task_run.go:4059-4062`、`4156-4161`、`coordinator_run.go:1351`(preflight 建構)vs `1360`(`createGatedAgent`) | **已修正**:`dfc083f`(coordinator 工具錯誤預設可恢復 + fatal 標記 + 連續錯誤上限 + `agent` 工具未宣告欄位交給 decode + `on_failure` 進 schema)、`8221414`(preflight 綁定 gated 工具)。使用者決策:預設可恢復、未宣告欄位交給 decode |
| E-26 | **`--plan` 模式的 plan reviewer 永遠無法核准計畫**:plan reviewer 經 `createGatedAgent` 建立(`coordinator_plan.go:91`),但串流 context 沒有把 reviewer 自己的工具(`approve_plan`/`modify_plan`/`reject_plan`)設進 `tools.AgentToolsAllowedKey`,沿用了呼叫端的 allowlist,gate 以「tool "approve_plan" is not authorized」拒絕並轉成 coordinator direct tool failure;每個 plan-first 任務都以「plan reviewer failed」結束。2026-09-24 kvmforge-verify 對照組 run(目前 binary,`--plan`)實測:兩個 deployer 任務都因此失敗,run 以 `tasks unresolved: 2` 結束 | `coordinator_plan.go:91`、`182-192`;`tool_policy_gate.go:588-626`(`authorizeToolInvocation`) | **已修正**:commit `c5f3dee`(reviewer 串流改用自己的工具 allowlist;eval harness 新增 `plan-mode`,新增 eval `plan-first-review`) |
| E-27 | **Plan-first worker 規劃階段送出 `submit_plan` 後仍繼續執行,在計畫審查前就動手**:規劃階段的 worker 持有完整工具面,但 stream 在 `submit_plan` 後沒有停止條件;模型以文字結束後,「requires submit_result」同輪 continuation 又要求它「make your next tool call now」。2026-09-25 kvmforge `--plan` 實機 run:deployer(task 2)在審查前執行 `sudo virsh qemu-agent-command`,verifier(task 3)在規劃階段跑完整個 Phase 4 驗證直到 step budget 耗盡(原先只記錄為多耗一次模型呼叫,實際是審查前副作用) | `internal/team/coordinator_task_run.go`(continuation gate、worker-override 路徑)、`subagent_hufu.go:168`、`coordinator_tools_plan.go` | **已修正**:commit `1bfdf9f`(`planSubmissionStop` 讓規劃 stream 在計畫送出後立即停止,兩條 worker 執行路徑都套用;continuation 跳過計畫待審的 worker;`plan-first-review` eval 移除多餘 continuation 步驟,移除修正時 eval 必失敗) |
| E-28 | Eval harness 只把「outcome 形成前」的 Run 錯誤當 finding;outcome 已記錄後 Run 仍回錯誤(例如 finish 後 scripted provider 沒有步驟可回應)不會讓 case 失敗,容易寫出藏著錯誤卻通過的 fixture | `internal/evalharness/runner.go`(run-error 斷言);`996da5e` 修正了本次新增 fixture 的此問題 | 記錄 |
| E-29 | **Worker context routing preflight 因 FTS5 語法錯誤讓任務在啟動前失敗**:`ftsQuery` 只保留 ASCII 英數字與底線,且不跳脫 FTS5 運算子。全中文(或只有標點)的查詢變成空字串 → `MATCH ''` → `fts5: syntax error near ""`;只含 `AND`/`OR`/`NOT`/`NEAR` 或以運算子結尾的查詢也是語法錯誤;夾在詞中間的 `NOT` 會被當成排除運算子而漏掉結果。2026-09-24 kvmforge-verify 實機 run(`c5f3dee` binary)第一個 deployer 任務(goal 全中文)因此失敗,其餘 3 個任務被依賴阻擋 | `internal/context/sqlite_repository.go:1284`(`SearchLexical`)、`1354-1359`(`ftsQuery`);`coordinator_task_run.go:941`;重現測試 `internal/context/lexical_query_test.go` | **已修正**:commit `400ff60`(空查詢直接回傳無結果;略過大寫 FTS5 運算子並為每個詞加引號;新增 eval `context-routing`) |
| E-30 | `execution-event shadow export parity: execution events length mismatch` 警告(legacy 與 exported 事件數不同)。2026-09-24 確認為**既存**:`cb2ee2b` 的 eval suite 已出現同樣警告(legacy=6 exported=5 等),實機 run 也有;不限於 routing preflight 失敗路徑(先前的推測不成立)。2026-09-25 查明根因(21 個 eval 中 7 個出現;只影響 stderr 警告與 `dualWriteFailures` 計數,後者 production 無人讀取,不影響 outcome/exit code):(1) canonical event 的 payload 是 TodoItem 快照,任務內重試時 `attempt`/`retries` 仍是 1/0,重試的 `task_started`/`task_failed` 被 `projectedExecutionEvents` 以 (task,status,attempt) 去重吃掉,legacy 則記 attempt 2;(2) 去重不分先後,`--plan` 核准後重新進入 `in_progress` 也被吃掉;(3) LLM worker 與 sidecar 路徑轉入 `verifying` 時不寫 legacy 事件(只有 action 路徑 `coordinator_task_run.go:2522` 會寫),有 verify 的任務 exported 多一筆 `verifying` | `internal/team/execution_event_exporter.go`、`coordinator_task_run.go` | **已修正**:commit `42ffd53`(projection 改用 attempt 起始 `task_started` 已帶的 `dispatch_attempt`,只合併與當前狀態相同的重複轉移;worker/sidecar 路徑補寫 legacy `verifying`;eval harness 對任何 dual-write failure 一律判 FAIL,未修前 7 個 case 會 FAIL) |
| E-31 | **非 workset 的 worker 一律無法使用 shell 類工具**:每個 worker attempt 都帶 artifact scope,非 workset 任務被設 `DenyUnsupportedDeclaredTools`;`artifactScopeToolDenial` 在該分支先以 `artifactScopeUnsupportedTool` 拒絕 `bash`/`sudo`/`ssh`/`wait_for`/`lua`/`terminal*`/`scp`/`create_skill`,與緊接的註解「unbound workers keep their ordinary built-in capabilities; only external/MCP adapters are denied」矛盾。2026-08-31 `3af4421` 引入;`tool_policy_gate_lua_test.go` 斷言 unbound lua 被拒(意圖是保護 artifact store 路徑)。影響:delegate、dev-team、doc-writer、hufu-dev 等宣告 bash 的 worker;kvmforge-verify 兩次實機 run 的 deployer 所有 bash 都被拒(對照組 15 次、修正後 run 同樣),任務以 blocked / loop 結束 | `tool_policy_gate.go:136-150`(`artifactScopeToolDenial`)、`104-111`(`artifactScopeUnsupportedTool`)、`coordinator_task_run.go:1124-1131`、`coordinator_declared_tool_runner.go:77`、`coordinator_run.go:107` | **已修正**:commit `1fd31c9`(使用者決策:agent `tools:` 明確宣告的 shell 類工具在 unbound 任務放行;all-tools 繼承者仍拒絕;workset 任務維持 fail-closed;外部 adapter 仍拒絕;新增 eval `worker-shell-tools`)。已接受的取捨:宣告的 shell 工具可觸及 artifact store 路徑(artifact 仍以 hash 驗證) |
| E-32 | **任務有多筆 context manifest 時 resume 被誤判為 projection mismatch 而 fail closed**:live checkpoint 以寫入順序附加 manifest(`status.go:1560-1568`),事件重放的 `mergeContextInjectionManifests` 依 (attempt, request ID) 排序(`context_manifest.go:288-293`,request ID 是雜湊所以看似亂序),而 parity 比對用的 `normalizeContextManifests`(`projection_shadow.go:301-310`)不排序。集合相同、順序不同即判 mismatch → `recovery required: event-store projection mismatch`。工具失敗會不斷新增 `tool_failure` manifest,因此很容易觸發。2026-09-25 以 kvmforge 實機 workspace(task 1 有 38 筆 manifest)驗證:在 `normalizeContextManifests` 以相同規則排序後 `CompareCanonicalProjection` 通過,不排序則失敗 | `internal/team/projection_shadow.go:301-310`、`context_manifest.go:269-295`、`status.go:1560-1568`、`coordinator_eventstore.go:352-400` | **已修正**:commit `e101870`(canonical shadow 以與重放相同的規則排序,抽成 `sortContextInjectionManifests`;transition 事件 idempotency key 也不再受順序影響) |
| E-33 | **`output_mode: verbatim` 任務的下游任務一律在 artifact scope preflight 失敗**:verbatim 模式把 task transcript 附到 typed result(`RawOutputRef` 與 `outputs.raw_transcript`,`attachVerbatimTaskResult`),但 transcript 寫入 artifact store 時 `PutArtifactRequest` 沒有 `Agent`(`task_transcript.go:293-301`);下游非 workset 任務的 `validateCurrentProducerArtifactOccurrence` 要求 ref.Agent 等於上游 agent(`workset_fanout.go:271-273`)→「belongs to agent "", want current agent "deployer"」。2026-09-25 kvmforge 實機 run:coordinator 以 verbatim 派出任務 1(成功),依賴它的任務 2、3 開始前即失敗。transcript 自 07-27(`ac39202`)、agent 檢查自 08-22(`f8f2b18`),既存 | `internal/team/task_transcript.go:276-305`、`336-370`;`workset_fanout.go:247-275`;`artifact_access.go:305-320` | **已修正**:commit `22f82d3`(task-run 與 terminal-receipt 路徑把產出 agent 傳進 transcript,manifest 記錄 `Agent`;unit test + `evals/verbatim-dependency` 端到端覆蓋,移除修正時 eval 必失敗) |
| E-34 | **`--plan` 下依賴任務在上游失敗後仍被審查並執行**:規劃階段 DAG 把「已規劃」當完成,依賴者隨即規劃;scheduler 結束後 `ExecuteTasks` 依輸入順序逐一審查、執行計畫,不檢查依賴結果。2026-09-25 kvmforge 實機 run:task 2(guest agent 檢查)BLOCKED 後,依賴它的 task 3(verifier)仍被核准並執行 | `internal/team/coordinator_execute.go`(原內嵌審查迴圈)、`coordinator_plan.go` | **已修正**:commit `6911ee4`(`reviewSubmittedPlans` 依依賴順序審查,依賴未完成的計畫不執行,與 scheduler 不啟動失敗任務的依賴者一致;eval `plan-first-dependency` 涵蓋失敗與成功兩種情況) |
| E-35 | Static action task(`TaskDef.Action`,非 run input resolver)的 provider 設定與 golang 原始碼不在 `ExecutionPolicySnapshot`:只有 run input resolver 用到的 capability 經 `RunInputPolicyHash` 固定;改了 provider command 或 Go 原始碼後 resume 不會 drift,會以同一個 Todo 執行不同程式 | `execution_policy_snapshot.go:34-50`、`run_input.go:605-642`、`action_materialization.go:120-150` | 記錄(catalog action 由 D19 固定;static action 另案) |

---

## 24. 工作項目

每個 WP:一個 commit;commit message 用 single-quoted heredoc(避免 backtick 被 shell 執行);
完成時 `go build ./... && go vet ./... && go test ./...` 全綠,並在本節對應 WP 標記完成與 commit hash。

### WP-0 既存問題修正(可獨立,先做)

**WP-0.1 `permitActionRetry` 尊重 replay 安全(E-01)** — ✅ 已完成,commit `3e37b3b`

- `runtime_workflow.go` `permitActionRetry`:在 `task.Action == nil` 檢查之後、任何 `retryState` 寫入之前加
  `if !CanAutomaticallyReplay(task) { return false }`(`execution_contract.go:714-724`)。
- 測試(table-driven,`runtime_workflow_test.go`):external_write、unknown、recovery manual/reconcile/never、
  `AllowsReplay=false` 都回 false 且 retry state 不變;none + retry 仍回 true。
- 新增 scheduler 層測試,覆蓋 `dag_scheduler.go:392-401` 分支(目前無測試)。
- 既有 `TestRuntimeWorkflowRetriesProviderFailureBySignatureAndRestoresIt`、
  `TestRuntimeWorkflowRetryPoliciesKeepSignaturesAndPermanentFailuresDistinct` 必須照舊通過。

**WP-0.2 `inspect trace` 顯示 action 狀態(E-03)** — ✅ 已完成,commit `05a2d3a`

- `internal/inspect/trace.go` `eventStatusAndReason`:metadata struct 加 `ActionStatus string \`json:"action_status"\``;
  status 與 outcome 都空時使用 `boundedCode(metadata.ActionStatus)`。
- 測試:`internal/inspect/trace_test.go`(`TestEventStatusAndReasonProjectsRunCancellationCause` 旁)加
  `team.LifecycleEventPayload{ActionStatus: "failure"}` case。

**WP-0.3 輸出 canonicalize 提前(E-04)** — ✅ 已完成,commit `85b00fe`(helper 在 `runtime_action_outputs.go`,含 `failRuntimeAction`)

- `executeRuntimeAction`:把 `CanonicalizeRuntimeOutputs` 區塊(約 2508-2515)移到 `decodeActionResult`(約 2482-2489)
  之後、artifact ingestion(2490)之前;`output = actionResultDisplay(...)`(2507)保留在 ingestion 之後。
  邏輯搬到新檔 helper,大檔只保留呼叫。
- 測試:provider 回傳無法 canonicalize 的 outputs(例如 129 個 key)且帶一個 artifact → task 失敗、
  沒有 artifact 被 ingest、沒有 `current-workset.json` 被寫入。

**WP-0.4 動態 team 的 `on_failure` 迴圈(E-02)** — ✅ 已完成,commit `7065b7f`

- `permitRepairRetry`(`runtime_workflow.go:307`):workflow **disabled** 時,除 `ActionValidationError` 外一律允許
  (次數上限已由 scheduler 的 `s.retries[idx] >= maxRetries` 與 `repairRetryLimit` 的 task.MaxRetries 保證);
  enabled workflow 的行為不變。**不要**改成 `ActionsEnabled()`。
- 已確認:verification / semantic rejection 失敗時 `res.err` 非 nil(`coordinator_task_run.go:1848`、
  `task_result.go:393-403` → `executeTask` 回傳 `failErr` 2257-2261 → `dag_scheduler.go:856`);
  `res.err == nil` 的 task 會被標 Done、不會走 on_failure。所以 disabled 分支不需處理 nil。
- 測試:以 `newCoordinator`(非 `&Coordinator{}`,確保 phaseWorkflow 非 nil 但 disabled)建立 hufu-coding 形狀的
  動態 team,worker task 帶 `on_failure` 與 `on-failure-classes: [verification, semantic_rejection]`,
  驗證失敗後 `on_failure` 目標被重設,最多 `max_retries` 次。另加 enabled workflow 的回歸測試確認 FailFast/limit 行為不變。

**WP-0.5 文件修正(E-05、E-06)** — ✅ 已完成,commit `c09b3b7`(另補 command provider `dir` 相對 process cwd 的說明,E-23)

- `docs/reference/action-providers.md`:
  - 「Binding a provider to a task」範例改成可載入的完整範例(含 `side_effect: none`、`when-goal-contains`、
    `capabilities.required`、`delegation.bind-task-goal-contracts: true`,以及合法的 phases/verify 設定)。
  - 同一份 team 放到 `internal/team/testdata/docs-action-provider-example/`(含最小 agent `.md`),新增
    `TestActionProvidersDocExampleLoads` 確認 `LoadTeam` 成功;文件註明範例與該 testdata 同步。
  - 環境變數表註明 `HUFU_ACTION_INVOCATION_ID` 每個 attempt 都不同,不可當冪等鍵。
  - 更新 `Verified-Commit`。
- `docs/README.md`:在 Guides and references 加入 `reference/action-providers.md` 連結。
- `docs/architecture/decision-runtime.md` §30 附近:加一段說明 static action task 與 structured step 在 provider
  啟動前經 `commitGateActionDenial` 閘控(gate 名稱為 `capability:type`)。

**WP-0.6 Policy repair 耗盡時優雅結束(E-22)** — ✅ 已完成,commit `426b4a8`(branch `fix/repair-exhaustion-and-dynamic-on-failure`)

- `policyGatedTool.Run` 的 coordinator 錯誤分支(`tool_policy_gate.go:372-403`):`errors.Is(err, errCoordinatorPolicyRepairExhausted)`
  時回傳 `fmt.Errorf("%w: %w", errCoordinatorToolFailure, err)`(同時保留兩個 sentinel),讓
  `attemptWrapUpRecovery` 在 `coordinator_run.go:1701` 的 exhausted 檢查先命中並呼叫 `finalizeCoordinatorPolicyRepairRun()`。
- 測試:端到端(經 `createGatedAgent` + fantasy stream)連續三次 delegation policy 違規,斷言 run outcome 是
  LLM-free 部分摘要(`finalizeCoordinatorPolicyRepairRun` 的結果),而不是 coordinator tool failure。只檢查
  `IsWrapUp()` 不足以抓到此 bug。測試 run 中不可有失敗的 Todo:`terminalUnresolvedRun()`(`coordinator_run.go:1693-1699`)
  比 exhausted 檢查更早,會改走 `finalizeTerminalUnresolvedRun`。`TestPolicyGateCoordinatorDispatchErrorResponseIsTerminal`
  照舊通過。
- 不在範圍:repair pending 期間呼叫不允許的工具在第 3 次得到 exhausted prompt 後 stream 仍繼續(E-24,記錄)。

### WP-1 Catalog 設定、驗證、snapshot、CLI — ✅ 已完成,commit `8ceb97c`

實作紀錄:load-time schema 規則放在 `action_catalog_schema.go`,provider 身分 helper 放在 `action_provider_identity.go`
(`executionRunInputPolicyHash` 改用它,輸出 bytes 不變)。workflow 必須以 prepare 開始(`normalizeWorkflowPhases`),
所以 `none` entry 永遠有可派工 phase,`action_catalog_phase_unreachable` 實際只會對「workspace_write 但無 EXECUTE」觸發。
重名(兩種拼法指向同一 agent)與 propose ⊄ discover 在結構段檢查。Run setup 重檢為 exported
`ValidateActionCatalogProposers`,由 `cmd/hufu/team_setup.go` 的 `freezeStartupExecutionPolicy` 呼叫。


- 新檔 `action_catalog.go`(型別、正規化、hash、限制常數、clone)、`action_catalog_validate.go`(§6 findings)、
  `action_catalog_value.go`(§8)。
- `team_manifest.go` `teamManifestSpecFields` 加 `ActionCatalog map[string]yaml.Node \`yaml:"action-catalog,omitempty"\``
  (§5.1);entry 的 YAML 型別 `actionCatalogEntryYAML` 定義在 `action_catalog.go`,schema 欄位用 `runInputSchemaYAML`。
- `parse.go`:新增 `loadActionCatalog`(§5.1,precedent `LoadRunInputDefinitions`);在 `loadTeamWithMode` 的 agents 與
  registry 建好後呼叫並正規化,設定 `session.ActionCatalog`;語意驗證接進 `ValidateTeamPolicyContracts`(§6)。
- `session.go` `cloneSession` deep clone。
- `execution_policy_snapshot.go` `ActionCatalogHash`(§7.3)。
- `run_input.go`:抽出 `actionProviderIdentity` helper(§7.1,D19);`action_catalog.go` 以它計算 `ProviderIdentityHash`。
- 提案者可達性(D20):語意驗證的 `action_catalog_proposer_unreachable`,以及 run setup 重檢
  `validateActionCatalogProposers`(§6)與 `cmd/hufu/team_setup.go` 的一行呼叫。
- `contract_finding.go` 新 code;`team_lint_ignore.go` `teamLintKnownCodes`。
- CLI `cmd/hufu/teamactioncmd.go`(§19.3)。
- 測試:
  - 每個 finding code 至少一個 case(table-driven,`action_catalog_validate_test.go`)。
  - 合法 entry 的正規化結果與 hash 決定性(兩次載入 hash 相同;改任一欄位 hash 改變)。
  - Provider 身分(D19,table-driven):改 command、dir、timeout、runtime 或 golang source 內容 → `ProviderIdentityHash`
    與 `entry.Hash` 改變;只改 command 所呼叫的腳本內容 → hash 不變(記錄限制)。重構前後
    `executionRunInputPolicyHash` 對既有 run input fixture 的輸出相同。
  - Recovery(D21):`workspace_write` 缺 `recovery` → `action_catalog_recovery_required`;`workspace_write` + 明確
    `retry`/`manual`/`never` 通過;`none` 缺 `recovery` → 正規化為 `retry`。
  - 提案者可達性(D20,table-driven):`require-proposal: true` 搭配空 propose、全部被 `tools-denied`、全部不在
    allowed-workers、全部 `subagent-provider: codex` → `action_catalog_proposer_unreachable`;至少一個有效提案者 → 通過;
    `require-proposal: false` 時不檢查。Run setup 重檢:`--worker-model` 把唯一提案者改到 agent backend → setup 失敗,
    且沒有任何 provider 呼叫。
  - `cloneSession` 隔離(修改 clone 不影響原本)。
  - 無 catalog 時 policy snapshot golden 不變;有 catalog 時 hash 變更造成 resume drift(照 `execution_route_test.go:496-524`),
    包含只改 provider command 的情況(S17)。
  - `hufu team lint` 列出多個 catalog finding;`--ignore` 接受新 code。
  - CLI text/json 輸出(不含 provider command/source)。
  - `hufu team migrate` 對含 catalog 的 team round-trip 不遺失、不含空值。

### WP-2 動態 team 的 action runtime(§15) — ✅ 已完成,commit `a2a4748`

實作紀錄:`ActionsEnabled`、`runtimeWorkspace`、`enableCatalogActions` 與 `runtimeActionEventPhase` 放在新檔
`runtime_workflow_actions.go`。有 event store 時 `executeTask` 會從 durable Todo 重建 TaskDef(`CatalogAction` 要到 WP-5 才進
TodoItem),所以 WP-2 測試直接呼叫 `executeRuntimeAction`(precedent `wp03_action_provider_test.go`)。


- `runtime_workflow.go`:`actionsEnabled`、`ActionsEnabled()`、`runtimeWorkspace()`、early-return 分支設定。
- §15.2 所有位置;§15.4 註解。
- 先建立新檔 `team_action_binding.go` 的 `CatalogActionBinding` 型別與 `clone()`,以及 `TaskDef.CatalogAction` 欄位
  (`json:"-" yaml:"-"`,§14 #1 的一半);其餘 durable plumbing 留給 WP-5。
- 測試:
  - 動態 team(有 catalog)直接以帶 `CatalogAction` binding 的 TaskDef(`Action` 已設)執行 `executeTask`,
    provider 被呼叫一次,receipt 與 `action_started`/`action_completed` 事件存在、`Phase == ""`
    (pattern `wp03_action_provider_test.go:219`,用 `recordingActionProvider`,`runtime_workflow_test.go:168-224`)。
    WP-2 階段 binding 由測試直接建構(WP-5/6 才有 durable plumbing 與 compiler)。
  - 同一個動態 team 中,**沒有** `CatalogAction` 的 action TaskDef 仍以既有錯誤「action invocation requires an
    enabled runtime workflow」失敗、且不發 action_* 事件(§15.2 表格最後一列),static contract action 在動態 team 的行為不變(S16)。
  - 動態 team 沒有 catalog:仍然無法執行 action(行為不變)。
  - Workflow team 既有 action 測試全數照舊。
  - 動態 team 無 catalog 時,worker tool-call 仍不發 action_* 事件。

### WP-3 唯讀工具(§9.1-9.6、§11.1-11.2) — ✅ 已完成,commit `9e7e5eb`

實作紀錄:worker 工具在 `team_action_tools.go`,coordinator 工具與 prompt 在 `team_action_coordinator_tools.go`。
Direct agent 以 `RunDirectAgent` 開頭設定的 context 標記排除;extra-model leaf 與缺 Todo 時不建構 handler。
Coordinator 版 `dispatchable_now`/`blocked_reason` 留給 WP-6(與 §12.3 共用判斷函式),此階段不輸出。Lint 只在 team 有
catalog 時把三個名稱加入 known registry;`declaredWorkerTools` 未改(它只服務 `requires.tools`,且沒有 session 可判斷 catalog)。
可選的 `dynamic_tool_authorization.go` 排序未做。


- 新檔 `team_action_tools.go`:worker 版 list/get、coordinator 版 list/get。此階段 coordinator 版的
  proposal 欄位回空陣列、`invocations_used` 回 0;WP-4 接上 proposal,WP-6 接上次數計算。
- §9.2 所有掛勾;§11.1 coordinator 工具;§11.2 prompt。
- 測試:
  - 暴露矩陣(table-driven):discover/propose/無權限 × 一般/result-only/sidecar/leaf/repair。
  - side_effect none 的 worker 呼叫 list/get 不被 `readOnlyToolMutation` 拒絕(S11)。
  - bound workset 任務與 bounded read scope 任務可使用且 resume 不出現 `resource_scope_unreproducible`。
  - 輸出不含 capability/type/agent/provider(worker 版);coordinator 版不含 provider(S5)。
  - `Info()` 對同一 agent 兩次解析完全相同(digest 穩定)。
  - 無 catalog 時 worker/coordinator 工具面與 baseline 相同(S12);`TestBuildOrchestratorToolsAreRuntimeAllowed` 通過。
  - Lint:`tools:` 或 prompt 提到這些名稱不報 `declared_tool_missing`/`prompt_unknown_tool`。

### WP-4 Proposal(§9.7、§10) — ✅ 已完成,commit `b686eb9`

實作紀錄:全部在新檔 `team_action_proposal.go`(`EventTeamActionProposed` 常數放 `event_types.go`)。Append 失敗回
`team_action_proposal_append_failed`,另有 `team_action_request_invalid`(未知欄位、assessment 或空 rationale)。
`durableBranchID` 在沒有 `eventStore` 時回空字串(D17),所以只有自訂 journal 的 coordinator 會得到 `team_action_journal_required`。


- 新檔 `team_action_proposal.go`:propose 工具、payload、append、index、重建;`c.durableBranchID()` helper(D17)在此 WP 建立
  (WP-6 的 invocation ID 也使用它)。
- `event_types.go`、`event_payloads.go`;`initEventStore` 掛勾;coordinator 版 list/get 接上 proposal。
- 測試:
  - 授權 / 未授權 / 參數錯誤 / 非 redaction-stable / evidence ID 無效 / 路徑形式 evidence / 上限。
  - 相同內容重送 → `duplicate: true` 同 ID;同 key 不同內容 → `team_action_proposal_conflict`。
  - 沒有 durable journal → `team_action_journal_required`,且沒有回報成功。
  - Resume(重新 `initEventStore`)後 index 相同;`--new` 後 index 為空(S7)。
  - 多個 worker 對同一 action 留不同 assessment,coordinator get 依新到舊列出。
  - 事件 payload strict 驗證拒絕未知欄位。

### WP-5 Durable binding plumbing(§14) — ✅ 已完成,commit `8e7098e`

實作紀錄:§14.1 的檢查為 `validateCatalogActionIntegrity`(`team_action_binding.go`),放在動態 team 檢查之後、workspace 配置之前,
另外比對 action 的 capability/type 是否等於 entry。有 binding 但 team 沒有 catalog 時回 `team_action_catalog_drift`。
`task_journal.go` 的 journal 只寫不讀,所以只在 result record 攜帶 binding。


- §14 表格 #2-#17 全部,以及 §14 的 `ProposalIDs` nil 正規化與 §14.1 檢查。#1 的 `CatalogAction` 欄位與 binding 型別
  已在 WP-2 建立;`CatalogInvocation` 欄位與型別、以及它在 `cloneTaskDef` 的 deep copy 屬於 WP-6。
- 測試:
  - TodoSpec → TodoItem → task_created → replay → `taskDefFromTodoItem` → `compareTaskDefWithTodoOccurrence` round-trip。
  - `cloneTodoItem` 與 checkpoint(session.json)round-trip。
  - 沒有 CatalogAction 的 task:task_created payload bytes、`decisionOccurrenceInputDigest`、`canonicalTaskShadow`
    與 baseline 相同(以既有 session fixture 或直接比對 hash)。
  - `HUFU_CATALOG_INVOCATION_ID` 只在有值時出現在 provider env。
  - §14.1 payload drift → `ActionValidationError`,provider Execute 次數 = 0(S10)。
  - §14.1 `entry.Hash` 與 binding 不符(或 entry 不存在)→ `team_action_catalog_drift`,provider Execute 次數 = 0。
  - `ProposalIDs` 為空 slice 的 binding 經 round-trip 後 `compareTaskDefWithTodoOccurrence` 仍通過。

### WP-6 派工編譯(§11.3、§12、§13) — ✅ 已完成,commit `7f6ff96`

實作紀錄:全部新邏輯在 `team_action_dispatch.go`(含共用的 `policyViolationResponse`)。另加拒絕碼 `team_action_task_invalid`
(允許欄位型別錯誤、重複 key、`catalog_action` 內未知欄位)。§12.3 的 entry 層判斷(#2/#3/#6/#7/#11/#12)是
`catalogDispatchBlockedReason`,coordinator list/get 的 `dispatchable_now`/`blocked_reason` 使用它;compiler 依 §12.3 順序逐項
檢查同樣的條件。Workflow team 的 `validateTasks` 分支與 agent enum 聯集留給 WP-7。§12.2 的 stream 層行為以
`gatePolicyTools` 包裝的 `agent` 工具測試(錯誤回應可恢復、第 4 次轉 policy repair);連續錯誤 streak 終止沿用 `dfc083f` 的既有測試。


- 新檔 `team_action_dispatch.go`:`CatalogInvocation` 型別與 TaskDef 欄位(含 `cloneTaskDef` deep copy)、
  `teamActionDispatchError`、decode helper、`compileCatalogActionTasks`、拒絕回應與拒絕計數 `teamActionRejections`(§12.2)、
  invocation ID 產生、budget 計數、proposal 連結。
- `coordinator_tools.go`:schema(§11.3)、decode(§12.1)、Run 掛勾(§12.2)。
- `execution_events.go:299` 旁歸零 `teamActionRejections`。**不修改** `tool_policy_gate.go`:拒絕回應是一般 error response,
  `dfc083f` 之後天然可恢復,不需要 call-ID 放行機制(D14、§12.2 #4)。
- `decision_admission.go` `canonicalizeTaskOccurrence`:catalog task 跳過 result contract 與 execution route(D18)。
- `coordinator_execute.go`:防禦檢查、TodoSpec、§13.3。
- §13.1 所有 stage 跳過。
- Coordinator 版 list/get 接上 `invocations_used`、`dispatchable_now`、`blocked_reason`(與 §12.3 #2/#3/#6/#7/#11/#12 共用同一判斷函式,對應 §11.1 的六種 reason)。
- 測試:
  - §12.3 每個拒絕碼一個 case(table-driven),且拒絕時沒有 Todo、沒有 task_created、provider Execute = 0(S3)。
  - §12.2 的拒絕回應測試全部(run 不終止、可接著呼叫 `team_action_get` 與重派已完成 worker、第 4 次轉 policy repair、
    repair pending 或 wrap-up 中直接走 policy repair、與其他 coordinator 工具錯誤合計超過 3 次時依 streak 規則終止)。
  - Allowlist:`side_effect`、`Side_Effect`、`id`、`contract_id`、`max_retries`、`verify`、`task`、完全重複與大小寫重複 key
    都被拒(S1);workflow 模式的 `depends_on` 被拒(D13)。
  - Entry agent 有預設 result contract 或 execution route 時,catalog Todo 的 `ResultContract`、`ExecutionRoute` 仍為 nil(D18)。
  - 既有 catalog Todo 不會讓之後同 agent、同 goal 的一般 task 被 `CheckDuplicate` 抑制(§13.1)。
  - 成功派工:TaskDef 欄位完全來自 catalog;`Action.Payload` 為 canonical;`DecisionProfile == "off"`;
    ProposalIDs 依事件順序、最多 32 筆。
  - `require-proposal`:只有 candidate/defer/reject → 拒絕;有 recommended → 成功(S6)。
  - Budget:同 session 第 N+1 次被拒;模擬 resume(重建 TodoList)後仍計數(S13)。
  - Unattended 拒絕(S14);`--decision-profile standard` 覆寫 → `team_action_decision_profile_unsupported`。
  - 同批次 catalog task → 同批次 worker task `depends_on` 該 index,worker 的 dependency results 含 action 輸出。
  - 綁定 goal contract 的 agent 當 entry agent 時,catalog task 不被 static contract 接管。
  - 同 goal 不同參數的兩次派工不被 `CheckDuplicate` 判重;同參數第二次受 budget 控制。
  - CatalogInvocationID 在 resume 前後相同。
  - Schema 測試:`coordinator_tools_test.go:191/251/284/303/323/333`、`capability_test.go:190` 更新或擴充;
    workflow 模式 schema 仍 < 12000 bytes;32-entry catalog < 16000 bytes;33 個以上省略 enum;無 catalog 時 schema 與
    baseline 相同;initial batch pending 時沒有 `catalog_action`。

### WP-7 Workflow team(§16、§11.3 enum 聯集) — ✅ 已完成,commit `39afa35`

實作紀錄:`validateCatalogTaskLocked`、`catalogWorkflowAgents`、`unionAgentEnum` 在 `runtime_workflow_actions.go`;
`runtime_workflow.go` 只加呼叫與「本批含一般 task 才要求派齊 static contract」的旗標。


- `runtime_workflow.go` `validateTasks` catalog 分支(邏輯放新檔,大檔只加呼叫);`Info()` agent enum 聯集;
  `action_catalog_phase_unreachable` 已在 WP-1。
- 測試:
  - EXECUTE:none 與 workspace_write 都可派;PREPARE:只有 none;AUDIT/VERIFY:拒絕。
  - 只含 catalog task 的批次在 EXECUTE 不被「必須派齊 static contract」拒絕;混合批次仍檢查 static contract。
  - 同批兩個不同參數的同 ID catalog task 不觸發 dispatch-once。
  - Catalog task 失敗不讓 phase 失敗;phase 仍在 static contract 成功後推進。
  - `TestRuntimeWorkflowRequiresEveryStaticContractAndRestoresCheckpoint` 照舊通過。

### WP-8 輸出驗證與 receipt(§17) — ✅ 已完成,commit `191b168`

實作紀錄:三個結構共用內嵌的 `CatalogRuntimeFields`(全部 omitempty);runtime receipt 另內嵌 `catalogActionReceiptFields`
(加 `ProposalIDs`、`SideEffect`,只對 catalog task 填值)。輸出驗證在 `canonicalizeRuntimeActionOutputs` 內,所以仍在 artifact
ingestion 之前。Execution receipt 從 Todo 的 binding 取值。


- 新檔 helper;`executeRuntimeAction` 只加呼叫。
- 測試:
  - Output schema 不符 → task 失敗、不 retry(`permitActionRetry` false)、不標 completed、receipt 記錄失敗。
  - Receipt/LifecycleEventPayload/ExecutionReceipt 的 catalog 欄位正確;非 catalog action 的 receipt JSON 與 baseline 相同。

### WP-9 觀測(§19.1、§19.2、E-18) — ✅ 已完成,commit `ac89dd6`

實作紀錄:report 區段標題沿用同檔其他區段的 `###` 層級;E-18 的 Provider 欄對 action task 顯示 `action:<capability>`。


- `internal/inspect/run.go`、`cmd/hufu/inspectcmd.go`、新檔 `cmd/hufu/report_actions.go`、`report.go` 呼叫與 Provider 欄修正。
- `--steps` 確認提示(`cmd/hufu/run.go:334-349`,目前只顯示 agent 與 goal):catalog task 另顯示
  `catalog action <id> args=<hash 前 12 碼>`。
- 更新 `docs/architecture/unified-observability-inspector.md` §6.2(308-316)與 `docs/guides/inspect.md` 對應段落。
- 測試:`internal/inspect/run_test.go:106` pattern、`cmd/hufu/inspectcmd_test.go:199` pattern;report 在無 catalog task 時輸出不變。

### WP-10 文件 — ✅ 已完成,commit `388ece0`

實作紀錄:範例 team 在 `internal/team/testdata/docs-action-catalog-dynamic`、`docs-action-catalog-workflow`,
由 `TestActionCatalogDocExamplesLoad` 載入;文件內的 YAML 與 testdata 相同。


- `docs/reference/action-providers.md` 新增「Action catalog」章節:設定(§5)、限制、驗證碼、worker/coordinator 流程、
  workflow phase 規則(§16.1)、recovery 與冪等(§18,`HUFU_CATALOG_INVOCATION_ID`)、`side-effect` vs `side_effect` 命名差異、
  CLI、完整範例(動態 team 與 workflow team 各一)。範例 team 放入 testdata 並有載入測試。
- `internal/team/action_provider.go:22-23`、`coordinator.go:99-101`、`runtime_workflow.go:125-129` 的註解改為如實描述
  catalog 派工(仍強調 coordinator JSON 不能直接設定 `Action`),並引用 `docs/reference/action-providers.md`。
- 根目錄 `README.md` action-providers 段落(約 228-256)加一句 catalog 與連結。
- `.agents/skills/team-builder/SKILL.md`(action-providers 段落約 262、701)加入 catalog 的撰寫指引與連結。
- `AGENTS.md` 的 team.yaml key 表(約 709)加入 `action-catalog`。
- 文件需說明的限制:§11.3 的 schema 不在同一 stream 內刷新;§16.1 catalog 只能在 static EXECUTE contract 完成前派出;
  D14 拒絕回應每個 invocation 最多 3 次;D7 已 admit 即計數;`--new` 重置 proposal 與次數。
- 文件需說明的信任邊界與設定規則:
  - `side-effect` 是對受信任 provider 的宣告,不是沙箱;provider 以 hufu 的 cwd 與完整環境變數執行(D2、D21、§18)。
  - `workspace_write` entry 必須明確寫 `recovery`;選 `retry` 代表 provider 以 `HUFU_CATALOG_INVOCATION_ID` 冪等(D21)。
  - Provider 設定與 golang 原始碼變更會讓 resume fail closed(需 `--new`);command runtime 所呼叫的腳本內容不被固定,
    需要內容身分時改用 golang runtime(D19)。
  - Command provider 的 `command`/`dir` 相對於 hufu process cwd,不是 team 目錄;範例一律用絕對路徑或 inline command(§5.1、E-23)。
  - `require-proposal: true` 需要至少一個有效提案者:不被 `tools-denied` 拒絕、在 allowed-workers 內、不使用 Codex 等外部
    agent backend(D20)。

### WP-11 E2E 與 eval — ✅ 已完成

實作紀錄:Suite A 另加 `quick-bundle` entry(不需 proposal、max 1)給 budget case;Suite B 的 workflow 為
`[prepare, execute, verify]` + `allow_phase_skip`。EXECUTE 期間 tool call 也發 action_* 事件(E-11),所以 Suite B 以第 2 個
`action_completed` 的 `catalog_action_id` 斷言 catalog action。Resume 整合測試在 `team_action_resume_test.go`(同一 coordinator 以
durable events 重建 TodoList 後呼叫 `ResumeInterruptedTasks`,涵蓋 pending/started × retry/manual)。eval 計數測試改為 19 suites / 26 cases。


Eval harness 每個 suite 只有一個 team(`internal/evalharness/types.go:12-17`、`runner.go:90`),所以分成兩個 suite,
結構複製 `evals/core-lifecycle/`。Provider 一律用 inline command,避免 `dir` 相對於 cwd 的問題(E-23)。

**Suite A `evals/team-action-catalog/`(動態 team)**,兩個 capability:
`diag-ok` = `[/bin/sh, -c, 'cat >/dev/null; printf "%s" "{\"outputs\":{\"summary\":\"ok\"}}"']`;
`diag-bad` = 同樣形式但輸出 `{"outputs":{"unexpected":1}}`(違反 output schema)。兩個 catalog entry 分別綁定它們。

1. `dynamic-readonly-dispatch`:worker `team_action_get` → `team_action_propose`(recommended)→ coordinator
   `team_action_get` → `agent` 派 catalog task → provider 成功 → finish。斷言 durable events:
   `team_action_proposed`、`payload.catalog_action.action_id` 正確的 `task_created`、`action_completed`。
2. `require-proposal-missing`:coordinator 直接派 → 拒絕回應。斷言方式:fixture 在 coordinator 下一回合放一個
   `{"match":{"contains":"team_action_proposal_required"}}` 的 step(未被消耗的 step 會讓 case 失敗,`runner.go:215-216`),
   並斷言 `task-count: 0`、durable `task_created` 數為 0。
3. `invocation-budget-exceeded`:`max-invocations: 1`,第二次派工以同樣的 `match.contains` 方式斷言
   `team_action_invocation_budget_exceeded`。
4. `output-schema-invalid`:派 `diag-bad` entry → task 失敗、未 retry(只有一個 `action_failed`)。

**Suite B `evals/team-action-catalog-workflow/`(workflow team,phases 含 EXECUTE)**:

5. `workflow-execute-dispatch`:在 EXECUTE 派 catalog task 成功,phase 照 static contract 推進。

一個 case 可以同時腳本化 worker 與 coordinator 的模型回合(共用 step queue + `match.contains`,
`internal/evalharness/types.go:219-249`,harness 以 maxConcurrent 1 執行);durable event 的 payload 欄位可斷言
(`assert.go:257-279`)。Status event 的 message 目前無法斷言(`assert.go:282-296`),所以拒絕碼一律用上述 step 比對。
- Go 整合測試(`internal/team`):
  - 在 `task_created` 後、provider 前中斷 → resume 以同一 Todo 與同一 `HUFU_CATALOG_INVOCATION_ID` 執行(recovery retry)。
  - 同情境 `recovery: manual` → blocked needs_human、provider 未執行。
  - Catalog 變更後 resume → drift fail-closed(S8);只改 entry 引用的 provider command → 同樣 fail-closed(S17)。
  - `workspace_write` + `recovery: retry` 的 entry 在 `action_started` 後中斷 → resume 以同一 `HUFU_CATALOG_INVOCATION_ID`
    重跑;同情境 `recovery: manual` → blocked needs_human、provider 未再執行。
- CI `eval-core` 會自動執行新 suite(`.github/workflows/ci.yml:167-195`),`TestRunAllEvalSuites`
  (`internal/evalharness/runner_test.go:63-101`)也會。

---

## 25. 測試矩陣(彙總)

| 區域 | 必測 |
|---|---|
| 載入 | §6 每個 code;hash 決定性;provider 身分 hash(D19);workspace_write recovery 必填(D21);提案者可達性與 run setup 重檢(D20);clone 隔離;migrate round-trip;無 catalog 不變 |
| 參數 | 重複 key、number 型別、additional-properties、redaction-unstable、integer canonical、64 KiB 上限 |
| Worker 工具 | 暴露矩陣;read-only 分類;bound workset / bounded scope;Info 決定性;不洩漏 provider |
| Proposal | 授權、驗證、evidence、去重與衝突、journal 必要、resume 重建、`--new` 隔離 |
| 派工 | §12.3 每個拒絕碼;拒絕回應可恢復且不進 repair;allowlist(含大小寫、workflow depends_on);欄位來源;result contract/route 不附加;proposal 連結;budget 與 resume;decision 覆寫;depends_on |
| ExecuteTasks | goal contract 不接管;CheckDuplicate 不誤判;digest/payload/shadow 對非 catalog task 不變 |
| 執行 | 動態 team 執行;payload drift;輸出 schema;catalog drift;receipt 欄位;env 變數 |
| Workflow | phase 允許矩陣;validateTasks 分支;observe 不受影響 |
| Recovery | Pending/started 中斷 × retry/manual;workspace_write 明確 retry/manual;invocation ID 穩定;policy drift(含 provider 設定變更) |
| 觀測 | inspect task/trace;report 區段與 Provider 欄 |
| WP-0 | E-01~E-06、E-22 各自的回歸測試 |

---

## 26. Definition of Done

- [ ] WP-0 ~ WP-11 全部完成,§24 各 WP 標記 commit hash。
- [ ] §21 S1~S19 每條都有對應測試並通過。
- [ ] 沒有 `action-catalog` 的 team:schema、工具面、policy snapshot hash、事件、digest、compat fixtures 與 baseline 相同。
- [ ] 動態 team 與 workflow team 都能端到端執行 catalog action(eval case 1、5)。
- [ ] Model 無法設定 capability/type/side_effect/recovery/decision_profile/phase。
- [ ] Proposal durable、typed、resume 後可重建。
- [ ] Catalog 變更後 resume fail closed。
- [ ] `go build ./...`、`go vet ./...`、`go test ./...`、`go test -race ./internal/team/...`、`golangci-lint run` 通過。
- [ ] `evals/team-action-catalog`(4 個 case)與 `evals/team-action-catalog-workflow`(1 個 case)通過。
- [ ] 新增檔案皆 < 800 行;既有超大檔案淨增加符合 §0 限制。
- [ ] `docs/reference/action-providers.md` 為 catalog 的 canonical 文件,程式碼註解只引用它。

---

## 27. 延後項目(v2+,需另行產品決策)

| 項目 | 前置條件 / 待決策 |
|---|---|
| `external_write` / `infra_mutation` / `credential_mutation` catalog action | 先解決 E-07(no-go 強制);catalog 需能宣告 commit gate 前提(verify-spec、reconcile-tool、expected-state-change、compensate);decision profile 需要 decision-options 來源 |
| Worker 觸發執行(invoke) | 需解決 v1 查核列出的巢狀執行問題(§28.A) |
| Workflow team 的 catalog `depends_on` 與跨回合 dependency | workflow schema 壓縮與 static contract 模型的調整 |
| Worker 看見其他 worker 的 proposal | 資訊隔離政策 |
| Proposal 自動彙整 / information-gain 排序 | 需 decision runtime 支援 |
| Catalog tags、cost class、`hufu list` 摘要 | 產品需求 |
| MCP-backed ActionProvider | 仍須經 ActionProvider adapter |
| §23 標記「記錄」的既存問題 | 各自另案 |

---

## 28. 否決的替代方案

### A. Worker 在 tool handler 內同步執行 child action(v1 設計)

否決。查核發現(`cb2ee2b`):

- 唯一先例 `request_agent` 對 worker 不可用且有 lease revoke / Paused 卡死問題(E-08)。
- 巢狀 `executeTask` 會繼承 parent context:envelope digest 不符被 block;extra-model leaf 旗標會**跳過
  `prepareTaskDecision`**;parent 的 attempt deadline 會截斷 child。
- Child 繞過 scheduler(resource claim、semaphore、mutation 序列化);走 scheduler 則可能因 parent 佔著 slot 而死鎖。
- Resume 先重跑 parent worker(會再 invoke),child 另外被當孤兒 resume;route fallback 清空對話歷史;沒有 durable 去重。
- Isolated-copy parent 的 child provider 會寫到 canonical project,繞過 attempt world。
- 失敗的 child 會讓 run outcome 變成未解決而 hard stop。

Coordinator 派工完全重用既有 lifecycle,上述問題都不存在。

### B. 每個 action 一個 tool

否決:tool surface 隨 catalog 線性成長、digest 變大、schema 重複。

### C. 自動把所有 ActionProvider 暴露給 model

否決:provider configured ≠ model authorized。

### D. 讓 model 直接送 raw `Action` / capability / side effect

否決:違反 configuration-owned 邊界(S1)。

### E. 以 LLM confidence 或 proposal 數量授權

否決:proposal 只是 evidence;授權只來自 static catalog 與 invocation policy。

### F. catalog 使用 `decision-profile` 閘控 mutation

v1 否決:task decision 不強制 no-go(E-07),commit gate 需要 catalog 沒有的前提欄位,非 off profile 需要
decision-options 與 judge;見 §27。
