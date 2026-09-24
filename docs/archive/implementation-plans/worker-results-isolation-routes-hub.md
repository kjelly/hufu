# hufu Structured Subagent / Workspace Isolation / Execution Routes / Agent Hub 實作計畫（v2）

> Status: in implementation — archived 2026-09-24 from local scratch space (`docs/tmp/spec.md`) before implementation began
> Authority: reference
> Target: `kjelly/hufu`
> Baseline: `eaa77f1`（2026-09-24，本機 main）。v1 草稿的 baseline `5e6decb` 已落後 74 個 commit，其中包含 workspace versioning。
> Branch: `feat/worker-results-isolation-routes`
> Date: 2026-09-24（v1 草稿：2026-09-21）
> Audience: coding agent / maintainer
> Priority: P0 + P1
> Decisions: D1–D4 已由維護者確認（§0.1）；D5–D14 為依既有 invariant 採用的預設（§0.2）
> Baseline test status（`eaa77f1`，2026-09-24）：`go build ./...`、`go vet ./...`、`go test -count=1 ./...`（44 個 package 全數 ok）、`golangci-lint run ./...`（0 issues）、`bin/check-docs` 全部通過

本文件中所有 `file:line` 引用都已在 `eaa77f1` 核實。實作時行號可能漂移，請以函式名稱與型別名稱為準。

## Implementation record

每個階段各自一個 commit（commit message 內文標有對應的 HF-OMP 編號）。

| Phase | 狀態 | 與本文件的偏差 |
|---|---|---|
| HF-OMP-000 | done | 無。escalate 系列行為已由 `escalation_test.go` 覆蓋，未另外新增重複的測試。 |
| HF-OMP-001 | done | ① `RequiresGroundedResult`：`15dbf47`（2026-08-27）已刻意改為 unified repair protocol（grounded task 也走 result-only repair，但必須自己呼叫 `submit_result`），過時的是 `execution_contract.go` 的欄位註解，而不是程式碼。因此改為：修正註解，並讓 free-text promotion（`promoteValidatedReadOnlyHandoff`）對 grounded task 一律不升級；不改 repair 流程，也不影響既有的 `TestProtocolRepair_GroundedResultRejectsRepairAndRetriesInstead`。② execution-events shadow（`execution_events.go`）仍從 Todo 的 target 推導 backend：在 fallback 出現之前，attempt target 一定等於 Todo target，所以移到 HF-OMP-008 一起處理。③ `dispatch_attempt` 除了計畫列出的兩處之外，也加在其他真正開始單次 attempt 的地方：structured action、sidecar、direct agent、fan-out parent、delegated sub-task。④ receipt 的 `Usage` 使用 `usageWithProgressTokens`，與 legacy log 的數值一致。 |
| HF-OMP-002 | done | ① §4.9 的 receipt metadata 欄位與 `TaskResult.StructuredPayload`、`ResultPayload`、`ResultValidationState` 移到 HF-OMP-003，和實際填值的程式碼一起加入，避免出現沒有人寫入的 durable 欄位。② `hufu team validate` / `hufu team explain` 的顯示移到 HF-OMP-003：本階段的過渡期 gate 會讓帶 contract 的 team 無法載入，explain 也就無從顯示。③ 額外的防護：agent frontmatter 的 YAML decode 原本在失敗時會靜默退回簡化解析器，這會把 `result-contract` 丟掉；現在只要 frontmatter 宣告了 `result-contract` 且 YAML 解析失敗，就 fail closed。`ResultContractSpec` 也改為嚴格 decode，拼錯的鍵（例如 `require_structured`）會直接報錯。④ compile 時的屬性名稱檢查使用新 export 的 `utils.IsRedactedJSONKey`，語意與 event redaction 相同，會放行 `max_tokens` 這類 numeric telemetry key。⑤ D10 的 load-time 判定：角色的 pin，以及以 capability registry 解析出符合角色 `RequiredCapabilities` 的 worker；adaptive capability 只會在 required 之上再加條件，所以用 required 判定已經涵蓋所有可能的候選。run-time 另外在 `resolvePinnedCandidate` 與 `resolveDecisionRoleBindingPlan` 做防禦性檢查。 |
| HF-OMP-003 | done | ① 補上 HF-OMP-002 漏掉的 resume 路徑：`taskDefFromTodoItem` 現在會帶上 `ResultContract`。② require-structured 的 completion 由 ingress 保證：`submit_result` handler 與 external canonicalizer 都會拒絕缺少或不合法的 payload，free-text promotion 也已關閉，所以沒有另外加一道 completion 時的檢查。③ receipt 的 result contract 結果在兩處套用：worker 回合結束後的第一次寫入，以及 result-only repair 之後的寫入；failure 計數用累加的方式，repair 不會覆蓋前一次的次數。④ `structured_result_validation_failures` 只計算本地 `submit_result` 的拒絕次數；external canonicalizer 是 stateless 的，它的拒絕會以 protocol failure 的形式呈現。⑤ event store 在 redaction 之後會 compact，而 redaction 的 re-marshal 保持 key 排序，所以持久化的 payload bytes 與 canonical bytes 完全相同（有測試覆蓋）。⑥ `hufu team validate` / `explain` 的顯示在本階段完成（從 002 移過來）。 |
| HF-OMP-004 | done | ① tools 端集中在新檔 `internal/tools/execution_root.go`：`cfgWithMergedPaths` 設定 `WorkDir` / `ExecutionRoot` / `DeniedWriteRoots`；glob、grep、ls、view、fetch、agentic_fetch 的 closure 改用 `executionWorkDir(ctx, cfg)`；write 路徑的判定放在既有 resolver 之後（resolver 會先解開 symlink），並以「最深、實際存在的祖先」解析 symlink；shell 的 `PWD` 與 `GIT_CEILING_DIRECTORIES` 在 `runShellCommand` 與 restricted 版本中依 ctx 設定，涵蓋 bash、direnv、sudo 等所有 shell 路徑。② `create_skill` 會直接寫入 project 的 `skills/`，也一併重新綁定，讓 skill 成為 attempt delta 的一部分。③ AGENTS.md 的內容由 `workerCtxOnce` 快取，isolated 副本在複製當下與 canonical 相同，所以歸類為 canonical；只有 prompt 的「Project root (CWD)」行重新綁定。④ `executeTask` 裡 verify 指令的「Runs from」提示是在 attempt loop 之前、每個 task 只建一次，而每個 attempt 的 world 路徑都不同，所以留到 HF-OMP-006，改為對 isolated task 使用通用說法。⑤ 寫入尚不存在的巢狀目錄會在既有的 write resolver 失敗，這是既有行為，與本階段無關。⑥ 新發現、交給 HF-OMP-006 處理：structured execution step（`coordinator_structured_execution.go:129`）與 action task 會在 `c.projectDir` 執行，所以帶 `Execution.Steps` 或 `Action` 的 task 必須在 isolated admission 被拒絕。<br>**`c.projectDir` 分類**（45 處）：<br>• attempt-bound（本階段重新綁定）：worker prompt 的 CWD 行（`coordinator_agents.go`）；verification 的 cwd（`verifyTaskDeliverableWithSpecAndResult` / `verifyTaskDeliverableWithMode`，fingerprint 用 `canonicalVerificationFingerprint` 維持 canonical）。<br>• canonical（不變）：AGENTS.md 快取、coordinator 自己的 prompt（`coordinator_prompt.go`）、policy snapshot 與 codex world、required-resource lock、resource scope root（`coordinator_task_execution_envelope.go`）、artifact store 與 skill artifact、capability probe、cache 與 context-request fingerprint、context shadow、acceptance 層級的 verification（criteria、criterion checkpoint、failure event、task output assertion、workset）、coordinator 自己的 verify tool、phase workflow 與 run input 的 repository root、execution events 的 repository root、未被讀取的 `AgentConfig.WorkDir`。<br>• 由 D8 或 ⑥ 排除、不需要綁定：declared/custom tool runner、structured execution、action environment。 |
| HF-OMP-005 | done | ① `ExecutionWorldSpec` 新增 `OccurrenceAttempt`，寫入 `owner.json`。② `Snapshot()` 為了符合 `ExecutionWorld` 介面，回傳 world root 的 effect snapshot；isolated integration 改用新的 `Delta()`，它是 kind-aware 的，並且會套用 ignore 規則。③ apply 的暫存檔沿用 versionstore 的前綴（export 為 `versionstore.MaterializeTempPrefix`），所以 crash 留下的暫存檔不會被 discovery 列為專案檔，也不會被複製進新的 world。④ symlink 的 target 檢查是 lexical 的：只接受解析後仍在 root 內的相對 target，絕對路徑的 target 一律拒絕（即使它指向 root 內）。⑤ walk 模式（非 git 專案）不偵測巢狀 repo，這與 versionstore 的語意相同；walk 模式的 `.hufuignore` 從 source root 讀取，與複製當下的副本內容相同。⑥ apply 在寫入前會整批檢查前置條件；寫入時若 world 檔案在 delta 取得之後又被修改，會以 `workspace_apply_incomplete` 失敗，確保未經驗證的內容不會進入專案。 |
| HF-OMP-006a | done | HF-OMP-006 拆成三個 commit（006a 設定/admission/resource scope、006b 接線與套用、006c recovery/metrics/文件），每個 commit 都讓 repo 保持一致。006a：① `worker-workspace`（agent frontmatter 與 team 預設）採嚴格 decode，拼錯的鍵會直接報錯，不會靜默退回 shared；frontmatter 的 YAML fallback 也會對它 fail closed。② admission 在 target 與 topology 解析之後凍結 `WorkerWorkspacePolicy`；D8 的排除條件分兩層檢查：team load 時查 agent 設定（extra-models、external backend、`MCPTools`、phase workflow、禁用 tool、`tools: all`），task admission 時再查實際解析出來的 tool surface（dynamic MCP target、禁用 tool）以及 structured step / action / fan-out / 非 managed scope。③ resource scope：isolated snapshot 為 version 2，帶 `Isolation: isolated-copy` 與 whole-root read claim；非 isolated snapshot 維持 version 1，digest 不變。④ 過渡期 gate：設定 isolated 的 team 在 006b 之前無法載入。 |
| HF-OMP-006b | done | ① attempt 接線：`executeTask` 每個 dispatch attempt 在 provider context 解析之後建立新的 world（`prepareIsolatedAttempt`），並把 execution root 綁到 attempt ctx；terminal resume 重跑同一個 attempt 編號時沿用同一個 world。attempt 失敗時（分類之後）丟棄 world 並追加 `attempt_workspace_discarded{reason=failure class}`；任何提早 return 都由函式層級的 defer 丟棄。verification 在 attempt root 中執行。② 套用：`integrateIsolatedAttempt` 位於 verification、adversarial verify、terminal evidence 都通過之後，`TaskDone` 之前；`TaskDone` commit 成功後才刪除 world。套用一開始就改用 `context.WithoutCancel`，避免取消造成半套用。delta 為空時直接 release，不寫 apply 事件。衝突 → `workspace_conflict`（`RetryWorker`），world 刪除；寫入失敗會重跑一次，仍失敗或 applied 事件寫不進去 → `workspace_apply_incomplete`，task 以 `NeedsHuman` 被 block，world 保留。③ 為了讓 world 能跨 process 使用，Prepare 會在 world 目錄寫入 `baseline.json`（kind-aware manifest，載入時比對 digest，並與 prepared 事件的 `baseline_digest` 比對）；套用前寫入 `delta.json`，供 006c 的 crash recovery 使用。④ protocol repair：inline repair 成功後走一般成功路徑的 hook；resume 時由 `resumeProtocolIncompleteTask` 在最前面處理 isolated task：目前 occurrence 最新的 world 仍是 prepared 且可載入 → 綁定後 repair，`finishProtocolRepair` 在 `TaskDone` 前套用；world 不存在或無法載入 → 經由 `prepareInterruptedTaskForResume` 重設後重新 dispatch（不從 `RepairProvenance` 完成）；最新 world 處於 applying/applied → block，交給 006c 的 recovery。resume 路徑沒有 worker retry loop，所以套用衝突時直接重新 dispatch。⑤ `TaskDone` 呼叫點盤點：一般成功路徑與 `finishProtocolRepair` 已接上 hook；extra-model fan-out（兩處）、structured steps、runtime action 在 admission 被拒絕；sidecar 不會是 isolated；direct agent 的 inline 路徑改為讓 isolated task 走 `executeTask`；`request_agent` 的 sub-agent 是 inline 執行、沒有 world，因此拒絕 isolated agent（`workspace_isolation_unsupported`，新增的限制）；DAG cache hit、operator 的 targeted reconcile、`reconcile` recovery policy 的完成確認，都是以 canonical 狀態為依據，不讀 world。⑥ world 狀態一律由全域事件推導（`attemptWorldRecordsFromEvents`），006c 的跨 branch reconcile 也需要它；因此不加 `TodoItem.AttemptWorld` projection，避免多一份需要與 session、checkpoint、shadow 保持一致、卻沒有讀取者的狀態。⑦ 移除過渡期 gate。⑧ 測試：dirty Git working tree（未 commit 的修改、untracked 檔案、`.gitignore` 忽略的輸出、`.git` 不複製）、完成前 canonical 不變、verification 在 attempt root 執行、apply_started 帶齊完成所需的證據、並行修改不同檔案、並行修改同一檔案（衝突後在新 base 上重試，結果等於某個序列順序）、失敗的 attempt 不影響 project 也不影響下一個 attempt、下游 task 看得到上游的變更、inline repair 後套用、resume 時 world 存在與遺失兩種情況、direct agent 路徑、`request_agent` 拒絕；以上並以 `-race` 執行。crash recovery（§5.14 第 8、9、18 項）、metrics 與文件在 006c。 |
| HF-OMP-006c | done | **P0-B 完成。** ① `reconcileAttemptWorlds` 接在四個公開入口：`Run`（`initTaskJournal` 之後、`ResumeInterruptedTasks` 之前）、`RunDirectAgent`（admission 檢查之後、dispatch 之前）、`ContinueWithPrompt`（run input 解析之後）、targeted retry/reconcile（`initTaskJournal` 之後）；不受 execution profile 影響。無法列出 world 目錄或讀不到事件時，整個 invocation fail closed。② 在 §5.11 之外多加的安全條件：這個 process 自己正在使用的 world 一律跳過（process-wide registry；`ContinueWithPrompt` 可能在同一個 process 還有 attempt 執行時進入）；owner 的 source root 不是本 project 的 world 不處理；沒有 prepared 事件、但目錄中有 `delta.json` 的 world 保留（可能已經開始套用）。③ applying 或 applied 的 world，若它的 task 不在這個 session 的 todo list 中（例如 fresh profile 沒有還原 todo），或 task 狀態已經無法轉成 done，只警告並保留。④ recovery 追加的 applied 事件帶 `recovered: true`；`finalizeAppliedAttempt` 的 `TaskDone` transition 帶 `recovered_from_attempt_world`，依序還原 typed result、verification、receipt 後 commit，並跳過 worker memory ingestion、STM、reflexion。⑤ `RunMetrics` 的三個欄位由本 run 的事件計算（與 `retry_suppressions` 相同作法），restart 之後仍然正確。⑥ 依 §11，world 無法準備時 task 以 `FailureEnvironment`、`needs_human` 被 block，不會退回 shared。⑦ 文件：`execution-runtime.md` 新增「Isolated worker workspaces」一節（名詞區分、admission、套用、並行、事件、recovery、v1 限制與 `external_drift` 的互動）；`workspace-versioning.md` 說明套用是 run 期間的一般寫入、attempt world 不是 snapshot；`agent-format.md` 新增 `worker-workspace`；`AGENTS.md` 功能表新增一列。⑧ 測試：套用中途 crash（canonical 已寫一半）、套用完成但 applied 事件未寫入、applied 之後 `TaskDone` 之前 crash，三者都不重跑 worker、只有一個 applied 事件、task 完成且 world 刪除；無法證明的套用 → task blocked、world 保留、衝突的 path 不被覆寫；孤兒 world 表格（attempt 途中 crash 的 world 與沒有事件的 world 刪除；protocol repair 的 world、使用中的 world、沒有 owner 的目錄、其他 branch 的 world 保留）與 orphan 計數；衝突後的 metrics；經由 `RunDirectAgent` 驗證入口會先 reconcile；Prepare 失敗 → blocked。以上並以 `-race` 執行。 |
| HF-OMP-007 | done | ① §6.1 的四個欄位合併成一個凍結的 struct：`ExecutionRoute *ExecutionRouteBinding{name, digest, candidates, fallback_on}`（`TaskDef` 為 `json:"-"`；`TodoItem` 與 `task_created` 為 `execution_route`，omitempty），經過與 `ResultContract` 相同的所有 carrier（TodoSpec 五處、reducer、shadow、journal、occurrence projection、`taskDefFromTodoItem`）；`Candidates[0]` 一定等於 `ExecutionTarget`，`ExecutionTopology` 仍是 `[primary]`。② 綁定發生在 `NewCoordinator` 內、建立 policy snapshot 之前（snapshot 是在 constructor 中建立的，而不是 `team_setup.go` 的 `ValidateModelCapabilities` 附近）；`team_setup.go` 在 `LoadConfig` 之後把 hufu.yaml 的 route 放到 `session.ExecutionRouteConfigs`。檢查分三層：`LoadTeam` 檢查名稱格式、agent 同時設定 route 與 `model`（`execution_route_conflict`）、coordinator 設定 route；`bindExecutionRoutes` 檢查 candidate、backend kind、`fallback-on`、互斥組合，最後才是過渡期 gate `execution_route_fallback_unsupported`；capability 檢查放在 `ValidateModelCapabilities`（profile warm 之後），`modelsInUse` 也會 warm 所有 candidate。③ 對 task 層級的 model：綁定多 candidate route 的 agent，coordinator 指定其他 model 或 `escalate: true` → `execution_route_conflict`，coordinator 不能繞過 route；單一 candidate 的 route 與 agent 自己的 `model` 等價，指定其他 model 時只 admit 一般 target、不帶 route。綁定 route 的 agent 一律跳過 model-list 的複雜度選擇（含 `selectCapabilityAwareModel`）。④ team 的預設 route 也會綁定內建的 helper worker（任何沒有自己 model 或 route 的 worker）。⑤ `ProviderFailureClass` 與允許 fallback 的清單先在本階段定義（`fallback-on` 驗證需要），`ClassifyProviderError` 在 008。⑥ hufu.yaml 的 route 採嚴格 decode；拼錯的鍵會讓整份 hufu.yaml 解析失敗（既有行為：警告並忽略該檔），引用該 route 的 team 接著以 route「not defined」失敗，不會靜默改用其他 model。⑦ policy snapshot 新增 omitempty 的 `execution_routes`（{owner, route, digest}，依 owner 排序），model inputs 納入所有 candidate；沒有 route 的 team snapshot 不變（HF-OMP-000 golden 通過）。⑧ 測試：compile 的 table（排序與 canonical 化、digest 確定、數量、未帶 backend、未知 backend、agent backend、重複、缺少或無效的 `fallback-on`）；binding 優先順序與互斥（extra-models、escalate-on-retry、decision role、gate、單一 candidate 允許的組合）；task admission（primary、escalate、其他 model）；複雜度選擇跳過；coordinator payload 拒絕；capability（已知不相容報錯、unknown 警告）；完整 team load 後 `task_created`、replay、resume 的 task 定義都帶 route；direct agent 路徑；`LoadTeam` 拒絕的設定；hufu.yaml route 修改後 resume 以 policy drift fail closed；CLI `-m` 與 `--worker-model` 清除 route；hufu.yaml 的 merge 與嚴格 decode。 |

---

## 0. 決策紀錄與 v2 修訂摘要

### 0.1 Maintainer 已確認的決策（D1–D4）

| ID | 決策 | 章節 |
|---|---|---|
| D1 | P0-B 使用 Hufu 自有的 **copy world**：新增一個 `ExecutionWorld` 實作 `isolated-copy`，把 subject root 的 managed 檔案（包含尚未 commit 的修改）複製到 hufu 擁有的 attempt root。**不使用 git worktree、不寫入使用者的 `.git`**。 | §5 |
| D2 | isolated 變更在 verification 通過後由 runtime **自動套用**（`integrate: on-verified`）。套用時逐一比對每個 path 的 baseline 狀態作為前置條件；只要有衝突就 fail closed（什麼都不寫），並以新的 base 重試。v1 **不**實作 proposal / 手動 apply 模式。 | §5.8 |
| D3 | fallback 候選存放在**新欄位** `ExecutionCandidates`，不沿用 `ExecutionTopology`（它的語意是 extra-model 平行 fan-out）。同一 agent 若同時設定多 candidate route 與 `escalate` / `escalate-on-retry` / `extra-models`，一律 fail closed。 | §6 |
| D4 | route 定義放在 **hufu.yaml**（`execution-routes:`）；team.yaml 與 agent frontmatter 只能寫 `execution-route: <name>`。引用的名稱不存在時，team 載入 fail closed。 | §6.2 |

### 0.2 依既有 invariant 採用的預設（D5–D14）

這些預設是 v2 根據既有程式碼與 normative 文件推導出來的。coding agent **不得**自行改變；若實作時發現必須偏離，停止並回報 maintainer。

| ID | 預設 | 依據 |
|---|---|---|
| D5 | result contract 只能由 agent frontmatter 或 static contract task 宣告。coordinator 的 agent tool payload 不能設定或覆寫 result contract。 | 沿用 `taskToolGrants` 的信任模型（`internal/team/tool_deny.go:188`）；coordinator 可以寫 `execution` 物件（`internal/team/coordinator.go:2016`），所以 result contract 不能放在 `ExecutionContract` 裡。 |
| D6 | result contract 與 route 定義都納入 execution policy snapshot 的 `ConfigurationHash`。resume 時只要有變更就 fail closed，要換成新設定必須 `--new`。**未使用這些新功能的 team，其 `ConfigurationHash` 必須與改動前完全相同**（以 HF-OMP-000 第 4 項的正規化 snapshot golden 驗證）。 | 與 `--worker-model` 相同的既有語意（`TestWorkerModelChangeOnResumeFailsClosedOnPolicyDrift`；`execution_policy_snapshot.go:724,752`）。 |
| D7 | `WorkspaceMode` 只有 `shared`（預設）與 `isolated`。刪除 v1 的 `read_only`（等同既有的 `side_effect: none`，`internal/tools/types.go:72-96`）與 `preferred`。 | 避免重複的 policy surface。 |
| D8 | isolated 模式在 v1 只支援 native LLM worker。以下情況一律 admission fail closed：external agent backend（codex 等）、extra-models、phase workflow、帶 MCP 或自訂 command tool 的 agent、`sudo`、`scp`、`terminal*`、`lua`、`golang` tool、非 managed workspace、有 submodule 或巢狀 repo 的 subject root。 | 這些路徑的 filesystem 效果無法安全地重新綁定到 attempt root：<br>• terminal 固定從 `c.projectDir` 啟動（`coordinator_tools_terminal.go:166-168`）；<br>• `lua` 與 `golang` 會呼叫 process-global 的 `os.Chdir`（`lua.go:99-100`、`golang.go:88-89`），不同 root 的 worker 並行時會產生 race；<br>• `scp` 不驗證本地路徑，而且沒有設定 `cmd.Dir`，會在 process 的 cwd 執行（`scp.go:89-240`），相對路徑的寫入會落在真正的 project root。 |
| D9 | fallback candidate 在 v1 只能是 `BackendKindLLM` backend（`internal/execution/kind.go:9`）；agent backend 不能當 candidate。 | 確保同一個 occurrence 的 tool envelope 不變（`execution-runtime.md` invariant 7）。 |
| D10 | 可能被 decision runtime 綁定為角色的 agent，v1 不得宣告多 candidate 的 route 或 result contract。角色是在 runtime 依 capability 綁定的（`decision_diversity.go:23-50`、`decision_judge_capability_runner.go:63`、`decision_challenge_capability_runner.go:89`），所以「decision-role agent」定義為：宣告的 capability 符合 team decision 設定中任一角色 `RequiredCapabilities` 的 agent。team load 時檢查這個定義；runtime 在綁定角色時再做一次防禦性檢查，違反就 fail closed。 | 保留 JUDGE/CHALLENGE/REVISE 的既有語意與 distinct-model/provider 下限（非目標 10）。 |
| D11 | Agent Hub 不是 `Coordinator` 的方法。它是對 canonical events 與 `TodoItem` 的純函式，放在 `internal/inspect`。 | `hufu status` 在另一個 process 執行，讀的是 `session.json`（`cmd/hufu/statuscmd.go:73,97`），呼叫不到 live coordinator。 |
| D12 | Agent Hub 的 activity 只從 lifecycle 事件推導。v1 不新增 token 或 tool 進度事件；沒有可靠來源時顯示 `unknown`。 | 現有的 `currentSnapshot.Stage` 是整個 coordinator 共用的單一值（`coordinator_status.go:35-44`），平行 worker 會互相覆蓋，不能當成 per-attempt 資料。 |
| D13 | extra-models 在 v1 與三項新功能（result contract、isolation、多 candidate route）都互斥。它既有的「多個 leaf 共用 subject root 並行寫入」行為（`coordinator_extra_models.go:80-104,295-316`）不在本計畫修正範圍，只在 Phase 0 用 characterization test 記錄下來。 | 範圍控制。 |
| D14 | `-m` 或 `--worker-model` 覆寫會讓該 agent 變成單一 candidate（route 綁定失效），不會隱含 fallback。 | v1 草稿 §6.9 的意圖；現有 `-m` 不會清掉 `ExtraModels`，此行為保持不變。 |

### 0.3 v1 草稿的前提錯誤與 v2 的處理

| v1 宣稱 | 實際情況（`eaa77f1`） | v2 處理 |
|---|---|---|
| `ExecutionContract` 已有 output schema 概念 | 沒有 result 或 output schema。唯一的 schema 是 structured step 的 `ExecutionStepOutput.Schema` 字串（`execution_contract.go:63,78`） | result contract 是全新的欄位，放在 `ExecutionContract` 之外（D5） |
| 需要評估是否新增 JSON Schema 相依 | `santhosh-tekuri/jsonschema/v6` 已是直接相依，並以 Draft 2020-12 使用中（`internal/agent/decision_profile_bundles.go:298-318`） | 直接重用 |
| free-text fallback 的值是 `parsed_free_text` / `promoted_free_text` | 正式流程中 `parsed_free_text` 永遠會被覆寫；實際的最終值是 `recovered_protocol`（`coordinator_task_run.go:1658,1699`）與 `promoted_free_text`（`:2215`） | 規則改以「是否經過 validated submission」判定 |
| team.yaml 的 `tasks:` 是 map | 是 list（`internal/team/parse.go:111`） | 範例已修正 |
| 外部 provider 與 local 走相同的 validator | 外部結果直接進 `storeSubmittedTaskResult`（`coordinator_task_run.go:1155-1163`），跳過 `validateWorkerClaims` 等檢查 | payload validator 必須在兩條路徑各自呼叫；其他既存差異不在本計畫範圍 |
| receipt 只存 metadata | `ExecutionReceipt.SubmittedResult` 內嵌完整的 `TaskResult`（`execution_receipt.go:190`），而且 resume 時會從 `RepairProvenance.SubmittedResult` 完成 task（`coordinator_task_run.go:2834-2837`） | payload 與其他 result 欄位一樣完整保存在 receipt；另外新增 metadata 欄位方便查詢（§4.9）。剝除 payload 會讓 resume 遺失資料，所以不剝除 |
| 需要新增 `WorkspaceProvider` 介面 | 已有 `ExecutionWorld`（`Prepare/Snapshot/Release`，`execution_world.go:15-22`），而且被明定為 workspace/effect 邊界（`execution-runtime.md:43-45`） | isolation 做成 `ExecutionWorld` 的實作 |
| `WorkspaceLease`、`WorkspaceDelta` 可以用來命名新型別 | 兩者都已存在：`TeamSession.WorkspaceLease`（team 層級的鎖，`parse.go:30-32`）與 `WorkspaceDelta`（`workspace_snapshot.go:54`） | 新型別改名（§2.5） |
| worker A/B 同時改 `foo.go` 會互踩 | 有副作用的 task 會拿到 whole-root exclusive claim（`coordinator_resource_scope.go:223-245`），DAG 排程下本來就不會並行。真正沒涵蓋的是 extra-model fan-out | isolation 的價值是「讓寫入者可以並行」與「失敗的 attempt 可以丟棄」，而不是正確性 |
| git worktree | 違反 `workspace-versioning.md:13-15`（不做 Git 整合、不碰 `.git`），也違反 AGENTS.md 的規定（隔離機制要有明確架構決策，`AGENTS.md:109-111`） | D1：改用 copy world |
| `ExecutionTopology` 是可以存有序 fallback 的 topology | 它是欄位不是型別，語意是 **fan-out**：topology 長度大於 1 就平行執行所有 leaf（`status.go:282-290`；`services.go:405-410`） | D3：新增 `ExecutionCandidates` |
| `ModelTopology` 是型別 | 是 `[]string` 欄位（`status.go:285`） | 不使用 |
| 可命名為 `ExecutionRoute` | `ExecutionRoute` / `ExecutionRouter` 已存在於 `cmd/hufu/router.go:13-30`（fast/team 路徑），`--route` flag 也已存在（`root.go:148`） | 改名為 `ExecutionRouteDefinition`；YAML 鍵維持 `execution-routes` / `execution-route` |
| 沒有既有的 fallback | 已有 `escalate`（task 欄位，`coordinator.go:161-163`）、`escalate-on-retry`（`team_manifest.go:159`）、`nextStrongerModel`（`coordinator_escalation.go:18-59`）、protocol capability fallback（`coordinator_task_run.go:1941-1953`），以及 ProviderManager 在 prefix 未知時靜默退回 local（`agent.go:1575-1616`） | D3 互斥 |
| fallback 類別名稱可直接使用 | v1 列出的類別中，只有 `semantic_rejection`（`TaskFailureClass`）與 `provider_unavailable`（僅 codex）存在；repo 裡沒有任何 429 偵測 | §6.5 新增 `ProviderFailureClass` 分類 |
| `-m` 就是單一 target | `-m` 不會清掉 `ExtraModels` | D14 只規範 route 的行為 |
| receipt 可以記錄 fallback 後的 target | receipt 沒有 target 欄位，`Backend` 由 Todo 覆寫（`status.go:1652-1654`；`execution_events.go:723-725`） | 在 HF-OMP-001 修正 |
| 可以新增 `Coordinator.WorkerSnapshots()` | status 在另一個 process 執行 | D11 |
| epaper / no-spinner 要新做 | 已完成（`internal/tui/theme.go:28-58`；`cmd/hufu/display.go:1939-1941`；`--display-preset` 與 `--no-spinner`） | 只補回歸測試 |
| usage 可以直接顯示 | `ExecutionUsage` 只寫進舊的 per-attempt JSONL（`execution_events.go:778`），canonical receipt 沒有 | 在 HF-OMP-001 把 usage 加進 receipt |
| 副作用類別叫 `credential` | 實際值是 `credential_mutation`（`recovery.go:13-18`） | 已修正 |

### 0.4 已排除在本計畫之外的工作（需要 maintainer 決定，不交給 coding agent）

- 在 built-in teams（`.agent-teams/*`）或個人的 hufu.yaml 啟用 result contract、isolation 或 route（v1 草稿 §12 v2 adoption）。
- workspace versioning 的 attempt-level checkpoint（PR-10）。
- 修正 extra-model fan-out 共用 subject root 的既存行為（D13）。
- 移除或 deprecate `escalate` / `escalate-on-retry`。

---

## 1. 目標

| Priority | Capability | Goal |
|---|---|---|
| P0 | Structured result contract（結構化結果合約） | worker 以符合 task/agent 專屬 JSON Schema 的 payload 回傳結果，由 runtime 在 trusted boundary 驗證並雜湊；coordinator 與下游 task 不必從自由文字重新解析。 |
| P0 | Isolated attempt world（隔離的 attempt 工作區） | 寫入型 worker 在 hufu 擁有的副本中執行，彼此可以並行；失敗的 attempt 可以整個丟棄；驗證通過的變更以有前置條件的原子方式套回 canonical project。 |
| P1 | Execution routes + deterministic fallback（具名路由與確定性備援） | agent 引用 hufu.yaml 中具名的有序 candidate 清單；provider 層失敗時依凍結的清單與 side-effect 安全規則切換到下一個 candidate，每次切換都會留下 canonical event 與 receipt。 |
| P1 | Worker attempt hub（worker attempt 觀測） | CLI、TUI、report 從 canonical events 投影出同一份 per-attempt 檢視：target、workspace、result validation、fallback、usage、failure。 |

核心流程：

```text
agent / static contract / hufu.yaml route
        │ team load：compile + validate（fail closed）
        ▼
task admission（canonicalizeTaskOccurrence）
        ├── ResultContractRef
        ├── WorkerWorkspacePolicy
        └── ExecutionTarget + ExecutionCandidates
        │ freeze → task_created（policy snapshot hash 包含 contracts/routes）
        ▼
dispatch attempt
        ├── IsolatedCopyExecutionWorld.Prepare（isolated 時）
        ├── tools 綁定到 execution root
        └── candidate[i] 的 LLM 呼叫
        │
        ▼
submit_result → structured payload 驗證 → verification
        │
        ▼
isolated：apply delta（前置條件檢查）→ TaskDone
        │
        ▼
canonical events → inspect.ProjectWorkerAttempts → status / TUI / report
```

---

## 2. 已驗證的 baseline（`eaa77f1`）

### 2.1 Structured result

- `TaskResult`（`task_result.go:260-328`）包含 v1 列出的 17 個欄位，另有 `TaskID`、`Attempt`、`Agent`、`RuntimeOutputs`（`map[string]any`）與 `RuntimeOutputsHash`、`ReceiptIDs`、`Source` 等欄位。`Outputs`（`map[string]StructuredOutputValue`）是 runtime-owned，submit_result 會拒絕它（`coordinator_tools_result.go:407,436`）。`Facts map[string]any` 是 worker 可寫、完全未驗證的開放物件（`:368`）。
- `isSubmittedResultSource`（`task_result.go:340`）判斷哪些 Source 算是真正的 structured handoff。Source 的實際值：`submitted`、external proposal（`subagent_external_result.go:115`）、`runtime`、`recovered_protocol`、`promoted_free_text`。
- `RequiresResult` 由 `coordinator_execute.go:181-185` 強制開啟（除非 team 設定 `allow-free-text-results`）。只有通過 `validateCompletedTaskResult`（`task_result.go:389`）的 submitted 或 external 結果才能滿足它。`promoted_free_text` 在「task 無副作用、attempt 唯讀、`validateTaskOutput` 通過」時也能滿足（`coordinator_task_run.go:1583-1596`）。
- protocol repair 有兩種機制，兩者都不會重新執行 tool：
  - per-call argument repair：`protocolRepairWrapper`（`coordinator_tools_repair.go:68-131`）。
  - result-only repair（`coordinator_task_run.go:1405-1692`）：乾淨的 context、只允許 `submit_result`、MaxSteps 1。
- `submit_result` 的 schema 由 `submitResultToolInfo`（`coordinator_tools_result.go:155-397`）**依每個 task 動態產生**，來源是 `taskResultSubmissionContractForTask`（`task_result_contract.go:29`）。decode 是 strict（`:104`），未知欄位直接拒絕，不會被剝除（`:404-409,436-441`）。runtime-owned 欄位會被覆寫（`:498-504`）。
- `validateToolArguments`（`coordinator_tools_repair.go:234-352`，一個手寫的 schema 子集驗證器）只由 `protocolRepairWrapper` 呼叫（`:73`），而這個 wrapper 只套用在 coordinator 自己的 tool 上（`tool_policy_gate.go:571-575`，`todoID == CoordTodoID`）。**worker 的 `submit_result` 不會經過它**，所以 worker 端唯一的 schema 信任邊界就是 handler 內的驗證。
- coordinator 的 task payload 由 `decodeModelTaskDefs`（`coordinator_tools.go:188-213`）以寬鬆的 `json.Unmarshal` decode；標記為 `json:"-"` 的欄位會被**靜默丟棄**，不會報錯。runtime-owned 的鍵必須像 `workset_binding` / `workset_receipt` 那樣明確列出並拒絕（`:202`）。
- Codex 的 proposal schema 是靜態的 OpenAI strict schema：`codexWorkerResultProposalSchema()`（`codex_result_schema.go:30`）不接受 task 參數，所有 property 都必填，`additionalProperties` 為 false。
- `ExternalResultCanonicalizer.Canonicalize` 在 `subagent_external_result.go:140-268`。
- `EffectiveTaskContract{ID,Revision,Hash,...}`（`contract_compile.go:17-37`）；`effectiveContractHash` 會雜湊整個 static contract。
- 一個 durable 欄位從頭到尾接線的前例：`ResourceScopeSnapshot`（commit `0b09899`），它修改了 `status.go`、`coordinator_eventstore.go`、`event_reducers.go`、`task_occurrence_projection.go`、`task_creation_admission.go`、`task_journal.go`、`projection_shadow.go`。
- 路徑限制的前例：`required-resources` 的路徑限制（`team/resource_lock.go:60-108`）。
- **既存 bug**：`RequiresGroundedResult` 只有外部路徑會讀（`subagent_external_result.go:166`），本地的 promotion 與 result-only repair 路徑沒有檢查，違反 `execution_contract.go:105-110` 註解的承諾。目前有兩個 team 宣告了它：`.agent-teams/hufu-coding`、`.agent-teams/hufu-code-review`。

### 2.2 Workspace / effect boundary

- `ExecutionWorld`（`execution_world.go:15-22`）。`ExecutionWorldSpec` 已帶有 `RunID/TaskID/Attempt/Root/CWD/WritableRoots/ReadOnlyRoots/Environment`（`:38-65`）；`PreparedExecutionWorld`（`:69-97`）以未 export 的欄位保存 process-local 狀態。
- `LocalExecutionWorld`（`execution_world_local.go`）：直接在既有 working tree 中執行；以 per-root lease 序列化（`:29-53`）；Prepare 時取 baseline snapshot。目前只有 Codex 在用（`subagent_codex.go:673-678`）；native worker 完全不經過 `ExecutionWorld`。
- `WorkspaceSnapshotter` 與 `WorkspaceDelta{Added,Modified,Deleted}`（`workspace_snapshot.go:54-69`）。它只用 SHA、bytes、perm 判斷 Modified，**看不出 file 與 symlink 之間的型別變化**（見 `docs/archive/implementation-plans/workspace-versioning.md:1648`）。非 Git 的 walk 模式還會以目錄名稱排除 bookkeeping 目錄。
- versionstore 的 inclusion policy（`internal/workspace/versionstore/inclusion.go`）才是正確的 managed-path 規則：只排除 `.git`、被排除的 subtree、以及 Git/.hufuignore 忽略的路徑；submodule 與巢狀 repo 視為 unmanaged。它目前沒有 export。`internal/team` 已經 import versionstore（`workspace_version_*.go`）。
- native tool 的 root：每個 coordinator 只在一處綁定，`agent.BuildAllAgentTools(projectDir=session.Scope.SubjectRoot, …)`（`coordinator.go:1361-1368`）。每個 tool 在建構時就擷取 `cfg.WorkDir`；per-attempt 的覆寫則經由 context key 在 `cfgWithMergedPaths`（`internal/tools/tools.go:494-536`）合併，安裝點在 `coordinator_task_run.go:1031-1045`。目前**沒有** WorkDir 的 context key。
- `internal/team` 中有 56 處使用 `c.projectDir`。重要的幾處：
  - prompt 的 project root 行（`coordinator_agents.go:364`）與讀取 AGENTS.md（`:223`）；
  - verify 指令的 cwd（`coordinator_task_run.go:540`）；
  - required-resource 雜湊（`coordinator_execute.go:158`）；
  - policy snapshot（`execution_policy_snapshot.go:293`）。
- 寫入路徑的判定：`AllowedWritePaths` 為空時，`resolveAndValidateWritePathWithConsent` 會退回使用 `AllowedPaths`（`tools.go:1213-1219`），而 `AllowedPaths` 包含 `session.Workspace`（ControlRoot）與 team 目錄（`cmd/hufu/team_setup.go:519-549`）。反過來，只要 `AllowedWritePaths` 非空，bash 就會被整個停用（`bash.go:96-100`，這是 phase workflow 的寫入隔離）。
- 其他使用 canonical root 的地方：
  - terminal tool 從 `t.coordinator.projectDir` 啟動（`coordinator_tools_terminal.go:166-168`）；
  - `lua` 與 `golang` 用 process-global 的 `os.Chdir(cfg.WorkDir)`（`lua.go:99-100`、`golang.go:88-89`）；
  - verification 的實際 cwd 來自 `verificationWorkDir()`（`coordinator_task_run.go:4746`，不接受 task 參數），而這個 workDir 也會被雜湊進 verification fingerprint（`verification.go:170-176`），用來偵測重複的失敗；`coordinator_task_run.go:540` 只是在 prompt 裡描述「Runs from」。
- 並行：預設 8 個（`cmd/hufu/team_setup.go:241-244`；`dag_scheduler.go:87-90`）。副作用不是 none 的 task 會拿到 `workspace:path:.` 的 exclusive claim；none 則是 read claim（`coordinator_resource_scope.go:223-245`）。`validateTaskResourceScopeSnapshot` 會強制檢查這一點（`:163-172`）。
- 唯讀執行：`AgentReadOnlyExecutionKey`（`coordinator_task_run.go:1034-1035`；`tools/types.go:72-96`）。
- 副作用類別：`none`、`workspace_write`、`external_write`、`infra_mutation`、`credential_mutation`、`unknown`（`recovery.go:13-18`）。不可重播的類別：external_write、infra_mutation、credential_mutation、unknown（`:21-28`）。
- `WorkspaceScope`（`workspace_scope.go:11-18`）：managed scope 的 ControlRoot 一定在 SubjectRoot 之外（`:43`）。
- 沒有任何 Go 程式碼執行 `git worktree`。

### 2.3 Execution target / routing

- `ExecutionSelector` 與 `ExecutionTarget{Backend,Model}` 在 `internal/execution/target.go:23,31`；`ExecutionRegistry` 在 `internal/team/execution_registry.go:14`，它會拒絕未知的 backend。
- target 的 admission 在 `canonicalizeTaskOccurrence`（`decision_admission.go:279-311`）完成：解析 target、topology，以及 `validateExtraModelExecutionTopology`。
- `resolveAgentModel`（`coordinator_agents.go:393-411`）的優先順序：override > coordinator role model > `def.Generation.Model` > `WorkerModel` > `Generation.Model`。`-m` 與 `--worker-model` 會寫入 `def.Generation.Model`（`cmd/hufu/model_overrides.go:38-41,145-149`）。
- `executionPolicyModelInputs`（`execution_policy_snapshot.go:161-217`）會雜湊所有可選的 model；snapshot 版本是 4（`:24`）。
- `TaskFailureClass`（`run_result.go:878-904`）：contract、environment、execution、protocol、verification、policy、timeout、cancelled、semantic_rejection。`RetryDisposition`（`disposition.go:13-24`）。
- fantasy 的 retry 已經關閉（`WithMaxRetries(0)`，`agent.go:1680`）。`fantasy.ProviderError` 有 `StatusCode`（fantasy v0.41.1 `errors.go:34-40`）。
- `ModelProfileRuntime.ResolveAdmission`（`model_profile_runtime.go:197-242`）；`ValidateModelCapabilities`（`model_capability_validation.go:51-78`，呼叫點 `cmd/hufu/team_setup.go:310`）；`selectCapabilityAwareModel`（`:172-192`）會在已知不相容時靜默替換 model。
- 預算：`BudgetManager`（`budget_manager.go`）；每個 attempt 的 `MaxTokensPerAttempt` 預設 500000（`agent.go:786,821`）。
- receipt 的 identity 是 `(RunID, TaskID, Attempt, ModelExecutionID)`（`status.go:1667-1674`）。
- 有兩個 attempt 計數器：
  - dispatch 內的 `attempt` 每次 dispatch 從 1 開始（`coordinator_task_run.go:658`），寫進 `receipt.Attempt`；
  - 事件 payload 的 `"attempt"` 是 `Retries+1`（`coordinator_eventstore.go:1514`）。
- runtime 身分：`submitResultRuntimeIdentity{RunID, TaskID, Attempt, Agent, OccurrenceRevision, DispatchID}`（`coordinator.go:2754-2756`）。**`DispatchID` 不是 durable 的**：它是 process-local 的計數器 `dispatch-%d`（`task_occurrence.go:222-224`），每次 process 重啟都會從頭開始；而且 `CommitTaskTransition` 在 append 之前會先清空它（`coordinator_eventstore.go:808,990,1082`），所以事件中的 `dispatch_id`（`:1462`）永遠是空字串。不能用它建立任何需要從事件重建的 identity。
- 本地 LLM backend 的 attempt 由 `HufuLocalSubagentProvider.RunAttempt` 執行（`coordinator.go:2531`），它會拒絕與凍結身分不符的 attempt：
  - `request.ModelID` 必須等於凍結 target 的 model（`subagent_hufu.go:73-79`）；
  - `request.Agent` 必須與 canonical agent 完全相同（`workerAgentResolutionAssertionMatches` 用 `reflect.DeepEqual`，`subagent_provider.go:177-179`），所以只改了 `Generation.Model` 的 clone 也會被拒絕；
  - attempt 帶的 target 必須等於 `ResolvedExecutionTarget`（`subagent_hufu.go:112-118` 附近）。
  另外，durable retry 每次都會把 model 重設回凍結的 model（`coordinator_task_run.go:784-789`，註解明寫「a changed model needs a new occurrence」）。
- config：`internal/config/config.go`，`ModelList []ModelEntry yaml:"model-list"`（`:146`），檔案中的值會整份取代（`:284`）。
- team.yaml 已占用的鍵：`workspace`（字串）、`routing-policy`、`model-list`、`escalate-on-retry`、`tasks` 等（`team_manifest.go:120-215`）。agent frontmatter 的欄位在 `parse.go:53-84`。

### 2.4 Observability

- `hufu status` 只有 `-w/--workspace`、`--team`、`--json` 三個 flag（`statuscmd.go:48-52`）。
- `hufu inspect task --run --attempt` 已經輸出 `AttemptData`（`internal/inspect/run.go:38-48`）。
- per-agent projection：`AgentStatus`（`status_projection.go:19-31`）。
- task status（`status.go:140-151`）：pending、in_progress、verifying、done、error、blocked、skipped、planned、paused、protocol_incomplete。cancelled 不是 status，而是 `TaskError` 加 `task_cancelled` 事件。
- 相關的 canonical 事件（`event_types.go`）：task_created/planned/started/verifying/completed/failed/blocked/protocol_incomplete/cancelled、recovery_decision、policy_decision、backend_session_bound、workspace_snapshot_committed。retry 時會發出 `task_started`（`coordinator_task_run.go:755-763`）。receipt 只在 attempt 結束時寫入（`:1224-1230`）。
- 舊的 `execution-events.jsonl` 不是 status 或 TUI 的依賴（`execution_event_exporter.go:12-15`）。
- redaction：`withToolResult` 會遮蔽（`status.go:84-90`），但 **`withTool` 不會遮蔽 `ToolArgs`**（`status.go:78-82`），而 TUI 會顯示 args 預覽（`display.go:1757-1760`）。canonical 事件在成功轉換時會存原始 `output`（`coordinator_eventstore.go:1546-1549`）；receipt 會存 repair prompt（`execution_receipt.go:51`）。
- TUI 以 `TasksUpdatedMsg` 推送資料，並非同步刷新 `InspectOverview` 成 `OperatorSnapshotMsg`（`display.go:1286-1316`）。
- report 是 `--report` flag（`cmd/hufu/report.go`）。既有指標：`RunStats`（`run_result.go:823-831`）與 `RunMetrics`（`:692`）。

### 2.5 保留名稱（不得重用或遮蔽）

| 已存在 | 位置 | v2 新型別 / 鍵 |
|---|---|---|
| `WorkspaceDelta` | `internal/team/workspace_snapshot.go:54` | `AttemptWorkspaceDelta` |
| `WorkspaceLease`、`commandWorkspaceLease` | `parse.go:30-32`、`cmd/hufu/workspace_resolver.go:24` | 不使用 lease 一詞；改稱 attempt world |
| `internal/workspace`（registry/lifecycle package） | — | 新程式碼放在 `internal/team`；versionstore 只 export discovery |
| `ExecutionRoute`、`ExecutionRouter`、`--route` | `cmd/hufu/router.go:13-30`、`root.go:148` | `ExecutionRouteDefinition`、`execution-routes`、`execution-route` |
| `ExecutionTopology`、`ModelTopology`（fan-out 語意） | `status.go:282-290` | `ExecutionCandidates` |
| team.yaml `workspace`（字串） | `team_manifest.go:126` | `worker-workspace` |
| team.yaml `routing-policy`、capability registry labels | `team_manifest.go` | 不使用 `require-capabilities`；capability 需求沿用 agent 的 `requires` |
| `canonicalWorkerAttemptContext` | `subagent_provider.go:113` | `WorkerAttemptView` |
| `RuntimeWorkspace`（`<control>/runtime`） | `execution_context.go:56` | attempt world 放在 `<ControlRoot>/attempt-worlds/` |

### 2.6 超過 800 行的檔案

CLAUDE.md 規定單檔不超過 800 行。以下檔案已經超過：

| 檔案 | 行數 |
|---|---|
| `coordinator_task_run.go` | 5429 |
| `display.go` | 2266 |
| `status.go` | 1960 |
| `event_reducers.go` | 1282 |
| `execution_policy_snapshot.go` | 840 |
| `coordinator_extra_models.go` | 831 |
| `coordinator_execute.go` | 829 |
| `execution_events.go` | 809 |

**規則：** 新邏輯一律寫在新檔案。上述檔案只允許加入最小的 hook 呼叫與欄位；本計畫不做拆檔重構。

### 2.7 Baseline 健康度

在 `eaa77f1` 上，`go build ./...`、`go vet ./...`、`go test ./...` 全部通過（44 個 package，0 FAIL）。

---

## 3. Architecture invariants

### I-1 Canonical state ownership

```text
EventStore / TodoItem / ExecutionReceipt
        ↓
   projections（純函式）
        ↓
CLI / TUI / report / Worker hub
```

Worker hub、status JSON、TUI 都不能成為 truth source。

### I-2 Admission freezes authority

dispatch 前必須凍結以下項目，並寫入 `task_created`：

- `ResultContractRef`
- `WorkerWorkspacePolicy`
- `ExecutionTarget`，以及（若有 route）`ExecutionCandidates`、`FallbackOn`、route 名稱與 digest
- 既有的 `ExecutionContract`、resource scope 與 tool envelope

worker runtime 不得重新讀取 team config 或 hufu.yaml 來決定 authority。

### I-3 Model identity 與 backend authority 分離

沿用 `ExecutionTarget = {backend, model}`。route candidate 必須寫成帶 backend 的完整 selector，並經過 `execution.ParseExecutionSelector` 解析；禁止在 runtime 以 `strings.Split(model, "/")` 重新推導 backend。

### I-4 No hidden fallback

- fallback 只能選擇凍結的 `ExecutionCandidates` 中的下一個 entry。
- 每次 fallback 都必須有 canonical 事件 `execution_fallback_decided`，並產生新的 attempt receipt。
- ProviderManager 不得靜默換 backend 或 model。

### I-5 Side effects constrain retry/fallback

已經產生不可撤銷的副作用時，不得因為 model failure 自動重播（§6.6）。isolated world 只能圍住 `workspace_write`；`external_write`、`infra_mutation`、`credential_mutation`、`unknown` 不因 isolation 就變成可以重試。

### I-6 Structured payload does not grant authority

worker 提交的 JSON 只是 result data。worker 不能透過 payload 或 submit_result 的其他欄位改寫：

- contract identity 或 hash
- execution target
- workspace root
- side-effect class
- verification policy
- authorization
- receipts
- runtime-owned attestation

### I-7 Isolation integrates only through a declared policy

isolated 變更只能透過 `integrate: on-verified` 這個明確設定的 policy 套回 canonical root，而且必須通過逐 path 的前置條件檢查。runtime 不做三方合併，也不做 Git merge。

### I-8 Name hygiene

不得重用 §2.5 所列的既有名稱來表示不同語意。

### I-9 Extra-models untouched

extra-model fan-out 的既有行為不變。它與本計畫的新功能組合時一律 fail closed（D13）。

---

## 4. P0-A — Structured Result Contract

### 4.1 資料模型

新檔案 `internal/team/result_contract.go`：

```go
// ResultContractSpec is the authoring shape (agent frontmatter / static contract task).
type ResultContractSpec struct {
    Schema            string `yaml:"schema"`             // team-directory-relative path
    RequireStructured bool   `yaml:"require-structured"`
}

// ResultContractRef is the durable identity frozen into a task occurrence.
type ResultContractRef struct {
    ID                string `json:"id"`            // team-dir-relative schema path, forward slashes
    SchemaSHA256      string `json:"schema_sha256"` // sha256 of canonical schema bytes
    RequireStructured bool   `json:"require_structured,omitempty"`
}

// CompiledResultContract lives only in the loaded TeamSession; never persisted.
type CompiledResultContract struct {
    Ref             ResultContractRef
    CanonicalSchema []byte
    schema          *jsonschema.Schema
}

// ResultPayload is runtime-owned; the worker supplies only the value.
type ResultPayload struct {
    Contract ResultContractRef `json:"contract"`
    Value    json.RawMessage   `json:"value"`  // canonical JSON
    SHA256   string            `json:"sha256"` // sha256 of Value
}

type ResultValidationState string

const (
    ResultValidationValid       ResultValidationState = "valid"
    ResultValidationInvalid     ResultValidationState = "invalid"
    ResultValidationAbsent      ResultValidationState = "absent"
    ResultValidationNotRequired ResultValidationState = "not_required"
)
```

`TaskResult` 新增欄位：

```go
StructuredPayload *ResultPayload `json:"structured_payload,omitempty"`
```

這個欄位是 runtime-owned，只能由 §4.5 的 validator 寫入。

`TaskDef` 新增 `ResultContract *ResultContractRef` 欄位，並標記 `json:"-"`，讓 coordinator payload 無法設定它。`TodoItem` 與 `task_created` payload 新增 `result_contract,omitempty`，依照 `ResourceScopeSnapshot` 的前例（§2.1）端到端接線。

### 4.2 Authoring

在 agent frontmatter 宣告（`parse.go:53-84` 的 `agentFrontmatter` 新增 `ResultContract *ResultContractSpec yaml:"result-contract"`）：

```yaml
---
name: reviewer
side_effect: none
result-contract:
  schema: schemas/code-review-v1.json
  require-structured: true
---
```

在 static contract task 宣告（team.yaml 的 `tasks:` 是 **list**）：

```yaml
tasks:
  - id: final-review
    agent: reviewer
    result-contract:
      schema: schemas/final-review-v1.json
      require-structured: true
```

優先順序：

1. static contract task：以 `ContractID`、agent 名稱與 `ContractHash` 比對，比對方式與 `boundedWorkflowBashCommand`（`tool_deny.go:222-226`）相同。
2. agent frontmatter。
3. 無 contract。

static contract 的 `result-contract` 必須納入 `effectiveContractHash` 的輸入（`contract_compile.go`）。

以下情況**永遠不綁定** result contract：coordinator/orchestrator agent、sidecar task（`TodoItem.Sidecar`）、decision runtime 的內部 stage。

coordinator 的 agent tool schema（`coordinator.go:2016` 的 `execution` 物件）**不得**新增任何 result 相關欄位。因為 `decodeModelTaskDefs` 會靜默丟棄 `json:"-"` 欄位而不報錯（§2.1），所以必須在它的 runtime-owned 拒絕清單（`coordinator_tools.go:202`）中明確加入：

- task 層級的 `result_contract`、`result-contract`、`structured_payload`；
- `execution` 物件內的 `result`、`result_contract`、`result-contract`。

比對時不分大小寫，比照 `workset_binding` 的寫法。必須有測試證明：coordinator payload 帶這些鍵時，delegation 會以錯誤拒絕。

### 4.3 Compile（team load，fail closed）

新檔案 `internal/team/result_contract_compile.go`。每一項錯誤訊息都必須指出 agent 或 contract 名稱與檔案路徑。

1. 路徑相對於 team 目錄。拒絕絕對路徑與 `..`；`filepath.EvalSymlinks` 之後必須仍在 team 目錄內。
2. 檔案大小不超過 256 KiB。
3. 以 `UseNumber` 做 JSON decode。最上層必須是 JSON 物件（schema）。
4. `$ref` 只允許同一文件內的 fragment（`#...`）。任何帶 scheme 的 `$ref`/`$id` 或相對檔案路徑都拒絕。santhosh-tekuri compiler 必須設定一個對所有 URL 都回錯的 loader（不做網路存取、也不載入檔案）。
5. 以 `jsonschema.Draft2020` 編譯，寫法比照 `decision_profile_bundles.go:298-318`。
6. canonical bytes 是 decode 後的值再以 `encoding/json` 重新 marshal 的結果（map key 會排序），`SchemaSHA256` 是這些 bytes 的 SHA-256。
7. **redaction 相容性**：event payload 在 append 時會經過 `redactJSONPreservingRuntimeOutputs`（`event_store.go:391`），而 `redactJSONValue`（`internal/utils/redact.go:492`）會依照 key 名稱遮蔽值（`secretKeyNameRe`，`redact.go:59`：password、passwd、secret、token、credential、api key、access key、private key）。因此 schema 中任何 property 名稱（包括 `properties`、`patternProperties`、`$defs` 之下的名稱）只要符合 `secretKeyNameRe`，就在 team load 時以 `result_contract_invalid` 拒絕，錯誤訊息要列出該名稱並建議改名（例如 `token_count` 改為 `usage_count`）。
8. 編譯結果存進 `TeamSession.ResultContracts map[string]*CompiledResultContract`（以 contract ID 為 key），不持久化。
9. 以下組合 fail closed：
   - 同一 agent 同時有 `result-contract` 與 `extra-models`（D13）；
   - decision-role agent 宣告 result contract（D10）；
   - 外部 agent backend 的 contract schema canonical bytes 超過 32 KiB（§4.6）。
10. `hufu team validate` 與 `hufu team explain` 必須顯示每個 agent 綁定的 contract ID、hash 與 `require-structured`。

### 4.4 Durable identity 與 drift

- `task_created` 與 `TodoItem` 只存 `ResultContractRef`，不存完整 schema。
- `ConfigurationHash` 加入 `result_contracts` 區段：以 agent 或 contract 名稱排序的 `{owner, id, schema_sha256, require_structured}` 清單。**清單為空時不加入**，讓既有 team 的 hash 不變（D6）。
- resume 時，occurrence 的 ref 必須與目前載入的 contract 相同，否則該 task 以 `result_contract_drift` 被 block。policy snapshot drift 通常會先擋下；這是防禦性的第二道檢查。

### 4.5 Validation boundary（local worker）

1. `SubmitResultInput`（`coordinator_tools_result.go:29-51`）新增 `StructuredPayload json.RawMessage json:"structured_payload,omitempty"`。
2. `submitResultToolInfo` 在 task 綁定 contract 時新增 `structured_payload` property：
   - 若 canonical schema 只用到 `dynamic_tool_schema.go` 允許清單（`:18`）中的關鍵字，就直接嵌入 schema；
   - 否則只提供 `{"type": <schema 最上層的 type>, "description": "Must satisfy result contract <id> (sha256 <prefix>)"}`。
   - `require-structured` 為 true 時加入 `required`。
   - 允許清單的目的是**provider 的 tool schema 相容性**（各家 provider 對 tool schema 關鍵字的支援程度不一），不是驗證。worker 的 `submit_result` 不經過 `validateToolArguments`（§2.1），所以 **`validateStructuredResultPayload` 是唯一的信任邊界**。必須有測試證明：含有不在清單內關鍵字的 schema 會以開放 property 暴露，而且仍然會被 `validateStructuredResultPayload` 驗證。
3. handler 在既有的 strict decode 與 runtime-owned 欄位覆寫之後執行：
   1. task 沒有 contract 卻提供 payload → tool error：`structured_payload is not accepted for this task`。
   2. 有 contract 且提供 payload → 呼叫 `validateStructuredResultPayload(compiled, raw)`，依序：
      - canonical bytes 不超過 256 KiB；
      - 以 `UseNumber` decode；
      - schema 驗證；
      - canonicalize（與 §4.3 第 6 點相同的規則）；
      - **redaction-stable 檢查**：對 canonical bytes 執行與 event append 相同的 redaction，結果必須與原本的 bytes 完全相同。不相同（例如值裡含有 `Authorization: Bearer ...`、private key 區塊，或 `API_KEY=...`）→ `structured_payload_invalid`，原因是「payload contains secret-like content」。這樣可以保證持久化後的值與 `SHA256` 一致，而且不會把 secret 寫進 event store。**不得**把 `structured_payload` 加進 redaction 的例外（它是 worker 產生的資料，不同於 runtime-owned 的 RuntimeOutputs）；
      - 計算 SHA-256；
      - 由 runtime 填入 `Contract`，組成 `ResultPayload`。
   3. 驗證失敗 → 回傳 tool error，內容最多 20 條錯誤，每條不超過 200 字元，並附上 JSON pointer 位置。worker 可以在同一個 attempt 內修正。validation failure metric 加一。
   4. `require-structured` 為 true 但沒有 payload → tool error：`structured_payload is required by result contract <id>`。
4. local、external（§4.6）、result-only repair 三條路徑必須共用**同一個** `validateStructuredResultPayload`。

### 4.6 External provider（codex）

- 在 `codexWorkerResultProposalSchema()` 新增 property `structured_payload_json: {"type": ["string", "null"]}`，並列為 required（strict mode 要求所有 property 必填）。strict decode 的 `WorkerResultProposal` struct（`subagent_external_result.go:26`）也要新增對應欄位 `StructuredPayloadJSON *string json:"structured_payload_json"`。
- 這個 schema 變更會影響**所有** codex task：沒有 contract 的 task 必須送 `null`；送出非 null 值時，以「task 不接受 structured payload」拒絕。必須有測試涵蓋沒有 contract 的 codex task。
- 若 task 綁定 contract，codex prompt 會加入 contract ID 與 canonical schema（不超過 32 KiB，超過的組合在 team load 已被拒絕），並要求把 payload 以 JSON 字串填入該欄位。
- `ExternalResultCanonicalizer.Canonicalize` 會把字串 decode 後交給同一個 validator。驗證失敗時，proposal 以 protocol failure 拒絕，與其他 canonicalization 失敗同一類別。
- 外部路徑跳過 `validateWorkerClaims` 等其他檢查是既存差異，不在本計畫修正範圍，但必須在 PR 描述中記錄。

### 4.7 Completion 規則

- `require-structured: true`：
  - completion 需要 `StructuredPayload != nil`，而且它必須來自經過驗證的 submission（submitted 或 external proposal）。
  - `promoted_free_text` 與 `recovered_protocol` 永遠不能滿足，這類 task 的 free-text promotion 路徑（`coordinator_task_run.go:1583-1596`）必須跳過。
  - attempt 結束時仍沒有有效 payload → `protocol_incomplete` → 走既有的 result-only repair。repair 只能呼叫 `submit_result`，因此不會重新執行任何副作用。
  - team 的 `allow-free-text-results: true` 不會覆蓋這條規則。
- 有 contract 但 `require-structured: false`：payload 可以不提供；有提供就必須有效。沒有 payload 的最終結果仍可接受，`ResultValidation` 為 `absent`。
- 沒有 contract：行為完全不變。
- schema 驗證有效不代表 verified，verification 照常執行。
- `partial`、`failed`、`blocked` 狀態的結果仍會 durable 保存 payload，但照既有的 status 語意，不會完成 task。

### 4.8 可見性

- `TaskResult.FormatForContext`（`task_result.go:403`）與 `coordinatorTaskOutput`（`:471`）要附加一個 `structured_payload` 區塊，內容是 contract ID、SHA-256 與 canonical JSON。上限 16 KiB；超過時截斷並加上明確標記，註明完整值保存在 durable 的 typed_result。
- `task_result_assert` 可以使用 `/structured_payload/value/...` 這類 pointer。`taskResultAssertionRootField` 回傳 `structured_payload` 時，必須把它列入 RequiredFields（需要測試）。

### 4.9 Receipt

在 `ExecutionReceipt`（`execution_receipt.go:145-207`）新增：

```go
ResultContractID     string                `json:"result_contract_id,omitempty"`
ResultContractSHA256 string                `json:"result_contract_sha256,omitempty"`
ResultPayloadSHA256  string                `json:"result_payload_sha256,omitempty"`
ResultValidation     ResultValidationState `json:"result_validation,omitempty"`
```

`SubmittedResult`（`:190`）與 `RepairProvenance` 內的 `TaskResult` 複本**完整保留** `StructuredPayload`，不剝除 `Value`。理由：

- resume 時會直接從 `RepairProvenance.SubmittedResult` 完成 task（`coordinator_task_run.go:2834-2837`），剝除 payload 會讓 `require-structured` 的 task 在 resume 後遺失 payload；
- receipt 本來就完整保存 `Details`、`Findings` 等 result 欄位，payload 的敏感程度和它們相同。

上面四個 metadata 欄位是為了讓查詢與 projection 不必解析整個 `TaskResult`。

### 4.10 Compatibility

| 設定 | 行為 |
|---|---|
| 無 `result-contract` | 現有行為不變；`ConfigurationHash` 不變 |
| 有 contract，`require-structured: false` | payload 可選；有提供就必須有效 |
| 有 contract，`require-structured: true` | 必須有 schema-valid 的 payload |

### 4.11 Required tests

以下每一項都用 table-driven 測試。LLM 一律不用 mock，沿用既有的 fake model 或測試 harness 慣例。

1. 有效的 payload 被接受，canonical hash 是確定的（key 順序不同但內容相同的 JSON 會得到相同的 hash）。
2. 缺少 required 欄位、`additionalProperties` 違規、enum 或 type 不符，都會被拒絕，錯誤訊息有上限且帶 pointer。
3. malformed JSON 不會被 promotion 成 success。
4. `require-structured` 加上 free-text 或 `recovered_protocol` → `protocol_incomplete`；result-only repair 提交有效 payload 後可以完成。
5. repair 期間只允許 `submit_result`，不會執行任何其他 tool。
6. codex proposal 的 `structured_payload_json` 與 local 路徑走同一個 validator，無效時被拒絕。
7. worker 無法提交 `contract` 或 `sha256` 欄位（strict decode 會拒絕）。
8. coordinator payload 帶 `result_contract` 或 `execution.result` 會被拒絕。
9. schema 檔案在 admission 之後被修改 → resume 時 policy drift fail closed；`--new` 則使用新 schema。
10. resume 使用 durable ref；ref 與載入的 contract 不符 → `result_contract_drift`。
11. schema-valid 的結果仍然必須通過 verification。
12. partial/failed/blocked 的 typed result 會保存 payload，但不會完成 task。
13. result-only repair 在 checkpoint 之後 crash，resume 從 `RepairProvenance.SubmittedResult` 完成 task 時，payload 與 `ResultValidation` 都保留，而且 `require-structured` 的 task 會被正確判定為滿足。
14. 沒有 contract 的 fixture team，其正規化後的 policy snapshot JSON 等於 HF-OMP-000 的 golden（§8 HF-OMP-000 第 4 項）。
15. event、reducer、session 與 shadow projection 的一致性（沿用 `CompareCanonicalProjection`）。
16. 遠端 `$ref`、路徑逃逸、symlink 逃逸、超過大小上限的 schema，都會在 team load 被拒絕。
17. 不在允許清單內的 schema 關鍵字 → property 以開放形式暴露，但仍在 trusted boundary 驗證。
18. `extra-models` 與 `result-contract` 同時設定 → team load 失敗。
19. schema 的 property 名稱含 `token`、`secret` 等字樣 → team load 失敗，錯誤訊息指出名稱。
20. payload 的值含有 bearer token 或 private key 區塊 → `structured_payload_invalid`；有效的 payload 寫入 event store 再讀回之後，`Value` 與 `SHA256` 仍然一致。

### 4.12 Files

新增：

- `internal/team/result_contract.go`
- `internal/team/result_contract_compile.go`
- `internal/team/result_contract_validate.go`
- 對應的 `_test.go`

修改（大檔案只加 hook）：

- `parse.go`（frontmatter、static task）
- `coordinator_tools.go`（`decodeModelTaskDefs` 的拒絕清單；`:1014` 附近手動組裝的 TodoSpec）
- `coordinator_run.go`（direct agent 路徑手動組裝的 TodoSpec，`:442-456`）
- `coordinator_tools_delegate.go`（手動組裝的 TodoSpec）
- `contract_compile.go`
- `task_result.go`
- `task_result_contract.go`
- `coordinator_tools_result.go`
- `codex_result_schema.go`
- `subagent_external_result.go`
- `coordinator_task_run.go`（hook）
- `execution_receipt.go`
- `status.go`（`TodoItem`）
- `coordinator_eventstore.go`
- `event_reducers.go`
- `task_occurrence_projection.go`
- `task_creation_admission.go`
- `task_journal.go`
- `projection_shadow.go`
- `execution_policy_snapshot.go`（hook）
- team lint 與 explain

---

## 5. P0-B — Isolated Attempt World

### 5.1 定位

新增 `IsolatedCopyExecutionWorld`，名稱 `isolated-copy`，實作既有的 `ExecutionWorld` 介面，放在新檔案 `internal/team/execution_world_isolated.go`。

- 它**不**呼叫 versionstore 的 `Capture` 或 `Materialize`，也不產生 versionstore snapshot。它只使用 versionstore export 出來的 managed-path discovery（§5.5）。
- 它不碰使用者的 `.git`。Git 只用於唯讀的 candidate discovery 與 ignore 判定，這和既有的 snapshotter 與 versionstore 一樣。
- `SupportsNativeSandbox` 回報 false。它不是安全沙箱（非目標 11）。

### 5.2 Authoring

在 agent frontmatter 或 team.yaml（作為 team 預設）設定：

```yaml
worker-workspace:
  mode: isolated          # shared（預設）| isolated
  integrate: on-verified  # mode=isolated 時必填，v1 唯一合法的值
```

- 優先順序：agent frontmatter > team 預設 > `shared`。static contract task 與 coordinator payload 都不能設定（v1）。`decodeModelTaskDefs` 的拒絕清單（`coordinator_tools.go:202`）要加入 `worker_workspace` 與 `worker-workspace`。
- 編譯後的 `WorkerWorkspacePolicy{Mode, Integrate, EffectiveMode}` 在 task admission 時凍結，寫入 `task_created` 與 `TodoItem`，只有設定的 mode 為 isolated 時才寫入。`EffectiveMode` 是依 §5.3 第 2 點解析副作用之後實際採用的模式（`isolated`，或 `side_effect: none` 時的 `shared`）。
- 未知的鍵或值 → team load 失敗（KnownFields strict）。

### 5.3 Admission 規則

team load 時能檢查的項目在 team load 檢查；其餘在 task admission 檢查。每一項都以 `workspace_isolation_unsupported` 加上具體原因 fail closed。

1. `WorkspaceScope.Managed` 必須為 true，而且 ControlRoot 與 SubjectRoot 互不包含。
2. 有效的副作用由 `resolveTaskRecovery`（`recovery.go:230-256`）決定，優先順序是：task 層級的 `side_effect`（coordinator 可以設定）> agent 的 `side_effect` > `InferSideEffectClass(def.Tools)`（`recovery.go:107-136`：有 bash/write/edit 等 tool 就是 `workspace_write`；有 `ssh` 就是 `external_write`；`sudo` 或 `tools: all` 就是 `infra_mutation`）。依解析結果處理：
   - `workspace_write` → 使用 isolated world；
   - `none` → 以 shared 模式唯讀執行（isolation 對它沒有意義，不報錯，但 policy 的 `EffectiveMode` 記為 `shared`）；
   - `external_write`、`infra_mutation`、`credential_mutation`、`unknown` → 拒絕（I-5）。
3. agent 的 backend 是 external agent backend（`BackendKindAgent`）→ 拒絕。
4. agent 設定了 `extra-models` → 拒絕（D13）。
5. phase workflow 啟用中（`runtime_workflow.go` 的 `RuntimeWorkspace` 寫入邊界）→ 拒絕。
6. 解析後的 tool 包含以下任一項 → 拒絕：
   - MCP 或 dynamic target（`ResolvedWorkerTools.DynamicTargets` 非空）；
   - agent command tool；
   - `sudo`；
   - `scp`（本地路徑未經驗證，並在 process 的 cwd 執行）；
   - `terminal`、`terminal_start`、`terminal_write`、`terminal_wait`、`terminal_close`、`terminal_reconcile`（固定從 canonical root 啟動）；
   - `lua`、`golang`（會呼叫 process-global 的 `os.Chdir`）。
7. 在 Prepare 時才能判定的佈局問題：subject root 含有 submodule 或巢狀 repo（`DiscoverManagedPaths` 回傳的 `Nested` 非空）→ 以 `workspace_isolation_unavailable` 失敗（§5.5）。

多 candidate route 與 isolation 可以組合，而且這正是 isolation 能讓 fallback 變安全的情境（§6.6）。

### 5.4 Attempt world 的配置與擁有權

```text
<ControlRoot>/attempt-worlds/<world-id>/
    owner.json   # 最先寫入：{format:1, world_id, run_id, task_id, occurrence_attempt, attempt, source_root, created_at}
    root/        # worker 看到的 project root（ExecutionRoot）
```

- `world-id` 是 runtime 在 Prepare 時以 `crypto/rand` 產生的 nonce：`aw-` 加上 16 個 hex 字元。它同時寫入 `owner.json` 與 `attempt_workspace_prepared` 事件，recovery 以 `world_id` 把目錄與事件對應起來。worker 無法指定或影響它。不使用 `DispatchID` 來衍生（它不是 durable 的，§2.3）。
- 碰到 ID 重複的目錄（機率極低）時，重新產生一個 nonce，不覆寫既有目錄。
- cleanup 只能刪除 `<ControlRoot>/attempt-worlds/` 之下、`owner.json` 可解析且 `world_id` 與目錄名稱相符的目錄。其他目錄只記錄警告，永遠不刪。

### 5.5 Prepare（複製）

1. **Discovery**：在 `internal/workspace/versionstore` export 一個純函式包裝 `discoverCandidates`，不新增其他 inclusion 規則：

   ```go
   type DiscoveryOptions struct {
       ExcludeSubtrees   []string
       RequireHufuignore bool
   }

   type ManagedPathSet struct {
       GitMode  bool
       Paths    []string // sorted, root-relative, forward slashes
       Excluded []string // exactly opts.ExcludeSubtrees, normalized
       Nested   []string // submodule / nested repository prefixes discovered under root
   }

   func DiscoverManagedPaths(ctx context.Context, root string, opts DiscoveryOptions) (ManagedPathSet, error)
   ```

   內部的 `candidateSet.unmanaged` 在一開始就會放入 `excludeSubtrees`（`inclusion.go:119,196`），所以 export 時必須把呼叫端指定的排除 subtree（`Excluded`）和 discovery 發現的 submodule 與巢狀 repo（`Nested`）分開。只有 `Nested` 非空 → `workspace_isolation_unavailable`（nested repository/submodule）。
2. 以 process-local 的 canonical integration lock（§5.9）的**共享**模式包住整個複製過程。
3. 上限：
   - 檔案數不超過 `workspaceSnapshotMaxFiles`（200000）；
   - 總 bytes 不超過新常數 `isolatedWorldMaxBytes = 2 GiB`。
   超過 → `workspace_isolation_unavailable`。
4. 先寫 `owner.json`，再建立 `root/`。
5. 對每個 path，來源與目的地都以 `os.Root` 開啟，**不跟隨 symlink**：
   - 一般檔案：複製 bytes 與 perm bits，邊複製邊計算 SHA-256。
   - symlink：只有在 link target 解析後仍位於 source root 內時，才複製 link 文字；逃逸的 symlink → `workspace_isolation_unavailable`。
   - 其他型別（FIFO、device、socket）→ `workspace_isolation_unavailable`，並指出 path。
6. 記錄 baseline manifest：`attemptManifestEntry{Path, Kind(file|symlink), SHA256, Size, Mode}`。SHA-256 是實際複製的 bytes 的雜湊（symlink 則是 link 文字的雜湊）。這是 kind-aware 的新結構，**不修改** `WorkspaceFileState` 或它的 digest 語意。
7. 被忽略的檔案（Git ignore 或 .hufuignore）不會被複製，例如 `node_modules/` 或 `.env`。這是刻意的設計（避免把 credential 複製出去），必須寫進文件。
8. 回傳 `PreparedExecutionWorld{Root: <world>/root, CWD: <mapped cwd>, WritableRoots: [<world>/root]}`，其餘狀態放在新增的未 export 欄位 `isolated *isolatedWorldState` 中。
9. 追加 canonical 事件 `attempt_workspace_prepared`（§5.11）。

### 5.6 Execution-root 重新綁定（HF-OMP-004）

1. 新增 context key `tools.AgentExecutionRootKey`（值為 `AgentExecutionRoot{Root string; DeniedWriteRoots []string}`）。`ToolConfig` 新增 `ExecutionRoot string` 與 `DeniedWriteRoots []string`。`cfgWithMergedPaths`（`tools.go:494-536`）在 key 存在時：
   - `WorkDir` 設為 execution root，`ExecutionRoot` 設為同一個路徑；
   - `DeniedWriteRoots` 設為 `[canonical subject root, <ControlRoot>/attempt-worlds]`；
   - **`AllowedWritePaths` 保持不變，不得為了隔離而把它設成非空**，否則 bash 會被整個停用（§2.2）；
   - 讀取用的 `AllowedPaths` 設為 execution root 加上原本設定的 allowed paths。canonical root 的**讀取**照舊允許（team 目錄、skill 等檔案可能位於 canonical root 內，讀取不會造成傷害）。

   `resolveAndValidateWritePathWithConsent`（`tools.go:1213-1219`）的新規則（僅在 `ExecutionRoot` 非空時生效）：
   1. 路徑在 `ExecutionRoot` 內 → 允許（即使 `ExecutionRoot` 本身位於某個 `DeniedWriteRoots` 之下）。
   2. 路徑在任一 `DeniedWriteRoots` 內 → 拒絕。這樣可以同時擋住寫入 canonical root 與寫入其他 attempt 的 world。
   3. 其他情況 → 沿用既有的判定（`AllowedWritePaths`，為空時退回 `AllowedPaths`）。control root 內一般的寫入（例如 artifact）因此維持現狀。
2. 每個在建構時擷取 `cfg.WorkDir` 的 tool，都要改用 `cfgWithMergedPaths(cfg, ctx).WorkDir`。以下檔案要逐一處理，無法重新綁定的必須在 PR 描述說明理由：
   - `bash.go`、`bash_exec.go`、`bash_policy.go`
   - `common.go`、`glob.go`、`grep.go`、`ls.go`、`view.go`
   - `scoped_file_access_unix.go`、`wait_for.go`
   - `fetch.go`、`agentic_fetch.go`（下載目的地）
   - `create_skill.go`（寫入 skill 目錄，預期不需綁定）
   - `stub_windows.go`、`types_common.go`
   - `sudo.go`、`scp.go`、`lua.go`、`golang.go` 與 terminal 系列 tool 已由 D8 在 admission 排除，這個 PR 不需要重新綁定它們，但必須有測試證明它們在 isolated attempt 中不會被解析出來。
   - write、edit、multiedit 的 scoped 路徑（`scoped_file_access_unix.go:24-80`、`write.go:81-88`）不經過 `resolveAndValidateWritePathWithConsent`，只有在設定了 bounded workset scope（`TaskPathScope`）時才會使用。isolated task 不推導 bounded scope（§5.9），所以要加一個 fail-closed 的斷言：`ExecutionRoot` 與 `TaskPathScope` 同時存在時，tool 直接回錯。
3. bash 在 isolated attempt 中：cwd 設為 execution root，環境變數加入 `PWD=<root>` 與 `GIT_CEILING_DIRECTORIES=<dirname(root)>`，避免 git 往上找到外層的 repo。
4. coordinator 端新增 helper `func (c *Coordinator) attemptProjectRoot(task TaskDef) string`：isolated attempt 回傳 execution root，其他情況回傳 `c.projectDir`。PR 必須把 56 處 `c.projectDir` 逐一分類為 canonical 或 attempt-bound，分類表放在 PR 描述中。
   - attempt-bound（至少包含）：
     - prompt 的 project root 行與 AGENTS.md 讀取（`coordinator_agents.go:223,364`），以及 prompt 中「Runs from」的描述（`coordinator_task_run.go:540`）；
     - verify 指令的實際 cwd：`verificationWorkDir()`（`coordinator_task_run.go:4746`）目前不接受 task 參數，要改成由呼叫端把 attempt root 傳入 `verifyTaskDeliverableWithSpecAndResult`（`:4651`），讓 `:4693` 與 `:4735` 的使用點都拿到 attempt root；
     - worker claim 的檔案檢查（`FilesRead`/`FilesModified`）；
     - transcript 的檔案 ref。
   - canonical（至少包含）：
     - policy snapshot（`execution_policy_snapshot.go:293`）；
     - required-resource 雜湊（`coordinator_execute.go:158`）；
     - resource claim 名稱；
     - workspace versioning；
     - **verification fingerprint 的 workDir**（`ComputeVerificationFingerprintFull`，`verification.go:170`）必須維持使用 canonical root。否則每個 attempt 的 root 都不同，fingerprint 每次都會變，重複失敗的偵測就會失效。
5. prompt 在 isolated attempt 中要說明：project root 是一份隔離副本；使用者的 git 歷史無法使用；被忽略的檔案沒有被複製。
6. 這個 PR 沒有使用者可見的變化：沒有 isolated mode 時，key 永遠不會被安裝。

### 5.7 Delta

- `Snapshot(prepared)` 用與 baseline 相同的 kind-aware manifest 掃描 execution root。規則：
  - 任何 path 片段含 `.git` 一律排除；
  - 對不在 baseline 中的新 path：
    - source 是 Git 模式 → 在 **source** 上執行 `git -C <SubjectRoot> check-ignore`（唯讀）判定，重用 versionstore 的 `gitIgnored`（`capture_delta.go:271`），並將它 export；
    - source 是 walk 模式 → 使用 baseline 時從副本載入的 .hufuignore；
  - 被忽略的 path 不會進入 delta，會隨 world 一起丟棄。
- `AttemptWorkspaceDelta` 放在新檔案 `internal/team/attempt_workspace_delta.go`：

  ```go
  type AttemptPathChange struct {
      Path   string                `json:"path"`
      Op     string                `json:"op"` // add | modify | delete
      Before *attemptManifestEntry `json:"before,omitempty"`
      After  *attemptManifestEntry `json:"after,omitempty"`
  }

  type AttemptWorkspaceDelta struct {
      WorldID        string              `json:"world_id"`
      BaselineDigest string              `json:"baseline_digest"`
      FinalDigest    string              `json:"final_digest"`
      Changes        []AttemptPathChange `json:"changes"` // sorted by path
      Digest         string              `json:"digest"`  // sha256 of canonical JSON of Changes
  }
  ```

  file 與 symlink 之間的型別變化以 `modify` 表示（Kind 不同）。
- delta 驗證：以下任一情況 → `workspace_delta_rejected`（`FailurePolicy`）：
  - 路徑不是 clean 的相對路徑；
  - 路徑含 `.git`；
  - symlink 的 After target 逃出 root；
  - `Changes` 超過 50000 筆。

### 5.8 套用（`integrate: on-verified`）

**時機：** verification 與 invariant gate 都通過之後、`TaskDone` commit 之前，呼叫 hook `c.integrateIsolatedAttempt(ctx, attempt, result, verification)`。以下兩條會 commit `TaskDone` 的路徑都必須呼叫：

1. 一般的成功路徑（`coordinator_task_run.go`）；
2. protocol repair 的完成路徑 `finishProtocolRepair`（`coordinator_task_run.go:3088`），包括 resume 時經由 `resumeProtocolIncompleteTask`（`:2788`）走到它的情況。

只要還有任何 commit `TaskDone` 的路徑沒有呼叫這個 hook，isolated task 就可能在變更被丟棄的情況下被標成 done。HF-OMP-006 必須列出所有會 commit `TaskDone` 的呼叫點，並逐一確認。

delta 為空時跳過套用，直接 release world。

**protocol_incomplete 的 isolated task：** world 會保留到 result-only repair 結束，repair 成功後依上述第 2 條套用。resume 時若 world 已經不存在，這個 attempt 不可能再套用，因此不得從 `RepairProvenance` 完成 task，而是改為重新 dispatch（task reset，新的 world）。

**步驟**（實作在新檔案 `internal/team/attempt_workspace_apply.go`）：

1. 驗證 delta（§5.7）。
2. 追加 `attempt_workspace_apply_started{world_id, delta_digest, change_count, typed_result, verify_result, execution_receipt, coordinator_output}`：
   - `typed_result`：已通過驗證、準備用來完成 task 的 `TaskResult`；
   - `verify_result`：對應的 verification 結果；
   - `execution_receipt`：包含已更新的 `HandoffState`。成功路徑對 `HandoffState` 的更新只存在記憶體中（`coordinator_task_run.go:1807-1811`），所以必須在這裡明確保存；
   - `coordinator_output`：成功路徑要交給 coordinator 的輸出。verbatim-transcript 的 task，這個輸出由 `finalizeTaskResultOccurrence`（`:1740-1757`）產生，無法從 `typed_result` 重建。
   這些欄位讓 crash 之後可以直接完成 task，不必重新執行 worker（§5.11）。這種在 payload 中帶 `typed_result` 與 `verify_result` 的寫法與既有的 task 事件一致。payload 在 append 時會被 redaction，與一般的 `TaskDone` 事件相同；`structured_payload` 因為是 redaction-stable（§4.5），不受影響。
3. 取得 canonical integration lock 的**獨占**模式。
4. **先整批檢查前置條件，再寫入任何東西。** 對每個 change，以 `Lstat` 加雜湊取得 canonical 的目前狀態：
   - 狀態等於 `Before`（add 則為「不存在」）→ 需要寫入；
   - 狀態等於 `After` → 已經套用過，跳過（re-entrant）；
   - 其他 → 衝突。
   只要有任何衝突：釋放 lock，追加 `attempt_workspace_apply_conflicted{world_id, conflicted_paths（最多 50 個）}`，task 以 `workspace_conflict` 失敗，canonical 完全沒有被寫入。
5. 寫入 add 與 modify：
   - 以 `os.Root` 從 world root 讀取，寫到 canonical 的暫存檔再 rename，全程不跟隨 symlink；
   - 需要時建立父目錄；
   - 保留 perm bits；
   - symlink 用暫存 link 加 rename。
6. 所有寫入成功後才執行 delete；只移除因為 delete 而變空的父目錄，比照 `versionstore.Materialize` 的規則。
7. 重新驗證每個 path 都等於 `After`。
8. 釋放 lock，追加 `attempt_workspace_applied{world_id, delta_digest, files_written, files_deleted}`。
9. 呼叫端 commit `TaskDone` **之後**，才 release world（刪除目錄）。在 applied 與 `TaskDone` 之間 crash 時，world 目錄仍然存在，由 §5.11 的 reconcile 完成 task 後再刪除。

**失敗處理：**

- 寫入途中發生 I/O 錯誤 → 立即重跑一次 re-entrant 套用；仍然失敗 → task 以 `workspace_apply_incomplete` 被 block（`needs_human`），world 目錄保留供檢查。
- 衝突 → 新增 `TaskFailureClass` `workspace_conflict`，`RetryDisposition` 為 `retry_worker`。重試次數受既有的 max-retries 限制；新的 attempt 會從目前的 canonical root 建立新的 world。

因為套用發生在 `TaskDone` 之前，DAG 中的下游 task 一定看得到這些變更。

### 5.9 Resource scope 與並行

- `TaskResourceScopeSnapshot` 新增 `Isolation string json:"isolation,omitempty"`，值為 `isolated-copy`。
  - isolated task 的 whole-root claim 是 `workspace:path:.` 的 **read** 模式。
  - `taskResourceScopeDigest` 會雜湊 `Version`（`coordinator_resource_scope.go:87`），而 validator 要求版本完全相符（`:112`）。因此**不得**全面升級版本常數：
    - 非 isolated 的 task 繼續寫 version 1，digest 完全不變；
    - 只有 `Isolation` 非空的 snapshot 寫 version 2，並把 `Isolation` 納入 digest；
    - validator 接受 {1, 2}，而且 version 1 的 snapshot 不得帶有 `Isolation`。
  - `validateTaskResourceScopeSnapshot` 只在 `Isolation == "isolated-copy"` 時，接受 `workspace_write` 搭配 read 模式的 whole-root claim。
  - isolated task 不走 bounded workset scope 的推導。
- 產生的並行關係：
  - isolated task 之間可以並行；isolated task 與唯讀 task 也可以並行；
  - shared 模式的寫入者（包括 Codex）持有 exclusive `.`，因此與 isolated task 互斥，不會重疊。
- `delegation_policy.go:482-487` 的批次衝突判斷必須正確處理 isolated task（它是 `workspace_write`，但 claim 是 read），需要測試。
- canonical integration lock：一個以 canonical subject root 為 key 的 process-local `sync.RWMutex` registry，寫法比照 `localExecutionWorldLeases`（`execution_world_local.go:24-53`）。Prepare 的複製持有共享鎖，Apply 持有獨占鎖。
- v1 的已知限制（必須寫進文件）：
  - 並行中的唯讀 task 可能觀察到套用到一半的多檔變更；
  - 同一個 subject root 上的其他 hufu process（例如另一個 team）只靠逐 path 的前置條件保護，沒有跨 process 協調，這和目前的 shared 模式相同。

### 5.10 Failure、retry 與 fallback 的交互

- 在套用之前，attempt 因任何原因失敗（包括 timeout 與 cancel）→ world 被丟棄，追加 `attempt_workspace_discarded{reason}`，canonical 完全不受影響。`workspace_write` 的效果被完整圍住，既有的 retry 預設不變。
- 在 P1-A 中，isolated 且尚未套用的 attempt，即使已經執行過會變更狀態的 tool，仍然可以 fallback（§6.6）。
- crash 發生在 attempt 途中 → 留下孤兒 world → 由啟動時的 reconcile 處理（§5.11）。

### 5.11 Durable 事件與 crash recovery

在 `event_types.go` 新增以下事件：

| 事件 | payload |
|---|---|
| `attempt_workspace_prepared` | world_id, task_id, occurrence_attempt, attempt, baseline_digest, file_count, bytes |
| `attempt_workspace_apply_started` | world_id, delta_digest, change_count, typed_result, verify_result, execution_receipt, coordinator_output |
| `attempt_workspace_apply_conflicted` | world_id, conflicted_paths |
| `attempt_workspace_applied` | world_id, delta_digest, files_written, files_deleted |
| `attempt_workspace_discarded` | world_id, reason |
| `attempt_workspace_orphan_removed` | world_id, last_state |

reducer 要在 `TodoItem` 新增 `AttemptWorld *AttemptWorldProjection{WorldID, State, DeltaDigest}`，State 的值為 prepared、applying、applied、discarded、conflicted，並保持 session、checkpoint 與 shadow projection 的一致性。

**啟動時的 reconcile**（只在 managed scope 執行）：

- **執行位置**：新增 `c.reconcileAttemptWorlds(ctx)`，在**每一個會 dispatch worker 的公開入口**呼叫：`initTaskJournal` 完成之後、任何 dispatch 之前，而且**不受 execution profile 影響**。至少包括：
  - `Run`（目前 `ResumeInterruptedTasks` 只在這裡被呼叫，`coordinator_run.go:2282`）；
  - direct agent 路徑（`coordinator_run.go:366-400`）；
  - `RetryTask` 與 targeted reconcile（`targeted_recovery.go:61-106`）；
  - `ContinueWithPrompt`（`coordinator_run.go:2367`）。

  不能只放在 `ResumeInterruptedTasks` 裡：它在 fresh-session 與 fresh-verification 的 profile 下會直接 return（`coordinator_session.go:918-920`），這些 profile 也不會還原 Todo list（`:631`）。reconcile 必須在 `ResumeInterruptedTasks` 決定要重新執行、block、skip 或 repair 之前完成，否則重新執行會在已經套用過的變更上重複工作。
- **事件範圍**：world 的狀態要從 session 的**全域**事件判定，不能用 branch 過濾後的投影。`--new` 會把事件過濾到新的 branch，如果用過濾後的事件，舊 branch 的 applying world 會被誤判為「沒有任何事件」而被刪除，project 就會停在套用一半的狀態，而且沒有任何警告。
- **不屬於目前 branch 的 world**：只發出明顯的警告（列出 run、task 與 world 路徑，並提示可以 resume 原本的 session 來完成它），然後保留目錄。不修改 canonical root，也不刪除目錄。
- **安全底線**：最後一個全域狀態是 applying，或是 applied 但 task 尚未 done 的 world，在任何情況下都不得刪除。
- **與 workspace versioning 的互動**：`admitWorkspaceVersion` 比 reconcile 早執行，如果 project 停在套用一半的狀態，它會把這個狀態記成 `external_drift` snapshot（`workspace_version_runtime.go:204`）。這和既有的 interrupted run 行為一致（`:92-95`），只需要寫進文件，不需要修改。
- 新增 `finalizeAppliedAttempt(item, applyStarted)`。成功路徑的收尾是 inline 的程式碼，加上一些 local closure（`coordinator_task_run.go:1806-1855`），沒有可以直接呼叫的共用函式；而 `commitTaskTransitionFromCurrent`（`coordinator_eventstore.go:1375`）不接受 result 參數，會從記憶體中的 `TodoItem` 組 payload（`:1591-1601`）。因此 finalize 要**比照 `finishProtocolRepair` 的收尾**（`coordinator_task_run.go:3125-3131`），依序：
  1. `storeSubmittedTaskResult(typed_result)`（resume 時呼叫是安全的，見 `:2817,2836`）；
  2. `SetVerificationResult(verify_result)`；
  3. `SetExecutionReceipt(execution_receipt)`；
  4. 以 `coordinator_output` commit `TaskDone`；
  5. `recordTerminalTypedTaskResult`；
  6. `reconcileTaskStatusProjection`；
  7. `reEvaluateAffectedCriteria`。
  **不重新執行 worker，也不重新 verify**（verification 在套用前已經通過）。成功路徑中其他的附帶步驟（worker memory ingestion、STM、reflexion）在 finalize 時一律跳過，這一點要寫在程式註解與文件中。

處理流程：

1. 列出 `<ControlRoot>/attempt-worlds/*/owner.json`。無效的目錄只記錄警告，不刪除。
2. 不屬於目前 branch 的 world，依上面的規則只警告並保留，不進入下面的處理。
3. 屬於目前 branch 的 world，依它最新的全域事件處理：
   - **applying**（有 apply_started，但沒有 applied 或 conflicted）：
     - world root 存在 → 重跑 re-entrant Apply；成功 → 追加 applied，然後 `finalizeAppliedAttempt`；
     - 有衝突或狀態不符，或 world root 已不存在 → task 以 `workspace_apply_incomplete` 被 block，world 目錄保留。
   - **applied**，但 task 還不是 done（在 applied 之後、`TaskDone` 之前 crash）→ `finalizeAppliedAttempt`，不會重複套用；之後刪除目錄。
   - **prepared**：
     - task 狀態是 `protocol_incomplete`，而且這是它最新的 world → 保留，供 result-only repair 使用（§5.8）；
     - 其他情況 → 丟棄，追加 `orphan_removed`。
   - **discarded**、**conflicted**，或 applied 且 task 已經 done → 刪除目錄，追加 `orphan_removed`。
   - **沒有任何事件**（全域事件中完全找不到這個 world_id，也就是在 prepared 事件寫入前就 crash）→ 刪除目錄。
4. 經 reconcile 定案的 task（已 finalize 或已 block）不得再進入重新執行的清單。
5. 永遠不碰 `<ControlRoot>/attempt-worlds/` 以外的路徑。

### 5.12 Security

必須測試：

- source 中有逃逸的 symlink → Prepare 失敗；
- worker 在 world 中建立指向 root 外的 symlink → delta 被拒絕；
- `..` 路徑、絕對路徑；
- worker 在 world 中 `git init` 產生的 `.git` → 被排除，永不套用；
- nested repo 與 submodule → `workspace_isolation_unavailable`；
- FIFO 與 device → 失敗；
- cleanup 只刪除有 owner 標記的目錄；
- 被忽略的 `.env` 不會被複製；
- file tool 以絕對路徑寫入 canonical root → 被拒絕（讀取照舊允許）；
- file tool 寫入其他 attempt 的 world（`<ControlRoot>/attempt-worlds/<other>/`）→ 被拒絕；
- file tool 寫入自己的 execution root → 允許；寫入 control root 中一般的 artifact 位置 → 維持現狀；
- 設定 isolation 後，`AllowedWritePaths` 仍為空，bash 沒有被停用；
- `ExecutionRoot` 與 `TaskPathScope` 同時存在 → write/edit/multiedit 直接回錯（scoped 路徑的 fail-closed 斷言）；
- 帶 `scp`、`sudo`、`lua`、`golang` 或 terminal 系列 tool 的 agent 設定 isolated → admission 拒絕；
- bash 的 cwd 與 `GIT_CEILING_DIRECTORIES` 設定正確；
- world ID 不可由 worker 控制。

bash 以絕對路徑寫入 canonical root 無法被阻擋，這與目前「bash 沒有 path scope」的狀況相同（`execution-runtime.md:77-80`），必須寫進文件並列入非目標。

### 5.13 與 workspace versioning 的關係

- 套用就是 run 期間的一般寫入。run checkpoint 在 run 結束時會把它們一起捕捉，versioning 行為不變。
- attempt world 不是 versionstore snapshot。本計畫不實作 versioning 的 PR-10（attempt-level checkpoint）。
- 在 `docs/architecture/workspace-versioning.md` 補一段說明這兩點。

### 5.14 Required tests

1. 兩個並行的 isolated worker 修改**不同**檔案 → 兩者都成功套用；DAG 允許它們並行（claim 是 read）。
2. 兩個並行的 isolated worker 修改**同一個**檔案 → 一個套用成功，另一個得到 `workspace_conflict`，在新的 base 上重試後成功，最終內容與序列執行的結果一致。
3. worker 完成之前，canonical root 的每一個 byte 都沒有變化。
4. delta digest 是確定的。
5. 失敗的 attempt 的 world 不會汙染下一個 attempt。
6. dirty source（未 commit 的修改與 untracked 檔案）被正確複製與套用。
7. read、write、edit、bash、grep、glob、ls、view 都綁定到 execution root（table-driven，逐一走過 builtin tool）。
8. 套用途中 crash（在測試中注入）→ resume 時完成套用或 block，不會半套用後就結束。
9. 孤兒 world 被 reconcile；非 hufu 擁有的目錄不會被刪除。
10. `external_write` 等副作用加上 isolated → admission 拒絕。
11. D8 的每一項排除條件各有一個測試。
12. resource scope snapshot v1 可讀；沒有 isolation 的 task 的 digest 不變。
13. event、reducer、session、report 的一致性。
14. `go test -race`，涵蓋 integration lock 與並行套用。
15. 下游 task 看得到上游 isolated task 套用的變更。
16. 衝突時 canonical 完全沒有被寫入（整批檢查前置條件）。
17. isolated task 進入 `protocol_incomplete`，result-only repair 成功後會套用 world 的變更；resume 時 world 已不存在 → 重新 dispatch，不會從 `RepairProvenance` 完成 task。
18. 在 `applied` 之後、`TaskDone` 之前 crash → resume 時經由 `finalizeAppliedAttempt` 完成 task，不重新執行 worker，也不重複套用。
19. 所有會 commit `TaskDone` 的呼叫點都會呼叫 integrate hook（以測試逐一覆蓋 HF-OMP-006 列出的呼叫點）。
20. verification 在 attempt root 中執行，而 verification fingerprint 在不同 attempt 之間保持相同（使用 canonical root）。
21. isolated attempt 的 world ID 是 nonce，與 `DispatchID` 無關；process 重啟之後不會與舊的 world 衝突。

### 5.15 Files

新增：

- `internal/team/worker_workspace_policy.go`
- `internal/team/execution_world_isolated.go`
- `internal/team/attempt_workspace_manifest.go`
- `internal/team/attempt_workspace_delta.go`
- `internal/team/attempt_workspace_apply.go`
- `internal/team/attempt_workspace_recovery.go`
- `internal/team/attempt_workspace_integration_lock.go`
- `internal/workspace/versionstore/discovery.go`（export）
- 對應的 `_test.go`

修改：

- `internal/tools/*`（§5.6 清單）
- `coordinator_resource_scope.go`
- `coordinator_task_run.go`（integrate hook、`finishProtocolRepair`、`verifyTaskDeliverableWithSpecAndResult` 的 root 參數）
- `coordinator_run.go`、`targeted_recovery.go`、`coordinator_session.go`（在每個公開入口呼叫 `reconcileAttemptWorlds`，§5.11）
- `coordinator_tools.go`（拒絕清單）
- `coordinator_agents.go`
- `event_types.go`
- `event_reducers.go`
- `status.go`
- `coordinator_eventstore.go`
- `run_result.go`（`workspace_conflict`）
- `disposition.go`
- `parse.go`、`team_manifest.go`（`worker-workspace`）

---

## 6. P1-A — Execution Routes + Deterministic Fallback

### 6.1 資料模型

新檔案 `internal/team/execution_route.go`：

```go
type ExecutionRouteDefinition struct {
    Name       string
    Candidates []execution.ExecutionTarget // 1..4, canonical, backend-qualified
    FallbackOn []ProviderFailureClass       // required when len(Candidates) > 1
    Digest     string                       // sha256 over canonical JSON {name, candidates, fallback_on}
}
```

只有在 agent 綁定 route 時，`TaskDef`、`TodoItem` 與 `task_created` 才新增以下欄位（全部 omitempty）：

- `ExecutionRouteName string`
- `ExecutionRouteDigest string`
- `ExecutionCandidates []execution.ExecutionTarget`（`[0]` 必須等於 `ExecutionTarget`）
- `FallbackOn []ProviderFailureClass`

`ExecutionTarget` 仍然是 primary，也就是 candidates[0]。`ExecutionTopology` 仍然是 `[primary]`，fan-out 語意完全不變。

### 6.2 Authoring

在 hufu.yaml 定義 route。`internal/config/config.go` 新增：

```go
type ExecutionRouteConfig struct {
    Candidates []string `yaml:"candidates"`
    FallbackOn []string `yaml:"fallback-on"`
}

// in Config / file config:
ExecutionRoutes map[string]ExecutionRouteConfig `yaml:"execution-routes"`
```

merge 語意比照 `ModelList`（`config.go:284`）：檔案中的值會整份取代。

```yaml
# hufu.yaml
execution-routes:
  coding:
    candidates:
      - ollama/qwen3:32b
      - openai/gpt-5.6-luna
    fallback-on: [rate_limited, provider_unavailable, model_unavailable]
  review:
    candidates:
      - openai/gpt-5.6-terra
```

team.yaml 與 agent frontmatter 只能引用名稱：

```yaml
# team.yaml（worker 預設）
execution-route: coding
```

```yaml
# agent frontmatter
execution-route: review
```

route 名稱必須符合 `^[a-z][a-z0-9-]{0,62}$`。

### 6.3 綁定優先順序（每個 worker agent）

1. `--worker-model agent=target` → 單一 target，忽略 route（D14）。
2. `-m target` → 單一 target，忽略 route（D14）。`applyCLIRuntimeOverrides` 在寫入 `def.Generation.Model` 時，同時清除該 agent 的 route 綁定。
3. agent 的 `execution-route`。若 agent 同時設定 `model` → team load 錯誤 `execution_route_conflict`。
4. agent 的 `model`。
5. team 的 `execution-route`。
6. team 的 `worker-model` 或 `model`（既有邏輯）。

coordinator/orchestrator agent，以及 sidecar、guard、judge、plan-reviewer 使用的 model，在 v1 都不會綁定 route。

coordinator payload 不能設定 route：`decodeModelTaskDefs` 的拒絕清單（`coordinator_tools.go:202`）要加入 `execution_route`、`execution-route`、`execution_candidates`、`fallback_on`。

### 6.4 Compile 與 admission（team load，在 CLI 覆寫套用之後）

新檔案 `internal/team/execution_route_admission.go`，由 `cmd/hufu/team_setup.go` 在 `ValidateModelCapabilities` 附近呼叫。以下任一情況 → `execution_route_invalid`：

- 引用的 route 名稱不存在；
- candidate 不是帶 backend 的 selector，或無法以 `ParseExecutionSelector` 解析（backend 需 canonicalize，例如 `local` 會轉成 `ollama`）；
- `ExecutionRegistry.ResolveBackend` 失敗，或 backend kind 不是 `BackendKindLLM`（D9）；
- candidate 重複，或數量不在 1 到 4 之間；
- candidate 多於 1 個卻沒有 `fallback-on`，或 `fallback-on` 中有不在允許清單（§6.5）的值；
- 以 agent 的 `requires` 對每個 candidate 執行 `ValidateModelCapabilities` 的硬性檢查，結果為已知不相容。已知不相容一律報錯，不會靜默過濾；unknown 只發出警告，與 `model-metadata.md` 的語意一致。`selectCapabilityAwareModel` 不得套用在綁定 route 的 agent 上。

以下組合 → `execution_route_conflict`：

- agent 綁定多 candidate route，同時設定 `extra-models`；
- team 設定 `escalate-on-retry: true`，而任何 agent 綁定多 candidate route；
- decision-role agent 綁定多 candidate route（D10）。

coordinator 對綁定多 candidate route 的 agent 發出帶 `escalate: true` 的 delegation → 在 `canonicalizeTaskOccurrence` 回傳 `execution_route_conflict` 錯誤，由 agent tool 以 tool error 回給 coordinator，錯誤訊息要明確說明原因。`escalate` 的 tool 說明（`coordinator.go:2162`）也要補上「不適用於綁定 execution route 的 agent」。

task admission 在 `canonicalizeTaskOccurrence`（`decision_admission.go:279`）中，以 candidates[0] 解析 `ExecutionTarget`，並寫入 §6.1 的欄位。

### 6.5 Provider failure taxonomy

新檔案 `internal/team/provider_failure.go`：

```go
type ProviderFailureClass string

const (
    ProviderRateLimited      ProviderFailureClass = "rate_limited"            // HTTP 429
    ProviderUnavailable      ProviderFailureClass = "provider_unavailable"    // 500/502/503/504, conn refused/reset, DNS, TLS handshake
    ProviderModelUnavailable ProviderFailureClass = "model_unavailable"       // 404, or documented backend "model not found"
    ProviderTransportTimeout ProviderFailureClass = "transport_timeout"       // provider HTTP client / transport timeout (not task timeout)
    ProviderAuthFailed       ProviderFailureClass = "auth_failed"             // 401/403 — never fallback-eligible in v1
    ProviderContextExceeded  ProviderFailureClass = "context_length_exceeded" // never fallback-eligible in v1
    ProviderOther            ProviderFailureClass = "other"
)

func ClassifyProviderError(err error) ProviderFailureClass
```

- 分類只能經由 `errors.As(*fantasy.ProviderError)` 的 `StatusCode`、`net.Error`、`*net.OpError`、`context.DeadlineExceeded`（僅限 transport 層）判定。
- 唯一例外：ollama 與 openai-compatible 回傳「model not found」時的錯誤 body，允許以一張明確列出的比對表判定。實際的錯誤格式要用 `httptest` server 在測試中重現確認。
- `fallback-on` 只允許 `rate_limited`、`provider_unavailable`、`model_unavailable`、`transport_timeout`。
- v1 草稿的 denylist（semantic_rejection、verification_failed 等）屬於 `TaskFailureClass` 的結果，本來就不是 provider failure，不會成為 fallback 觸發條件。

### 6.6 Fallback 資格（依序檢查，全部成立才 fallback）

1. attempt 以 model 呼叫的錯誤結束，而且 `ClassifyProviderError` 的結果在 `FallbackOn` 中。verification、semantic、policy、task timeout、cancel 等 task 層的失敗永遠不符合資格。
2. 存在下一個 candidate（`index+1 < len`）。
3. side-effect 安全。判定依據是**本 attempt 實際執行過的 tool call 紀錄**（見下方），不是 fantasy 的 steps：
   - 有效副作用是 `none` → 符合；
   - isolated-copy world 且尚未套用 → 符合；
   - shared 模式的 `workspace_write` → 紀錄中沒有任何 call，或每一個 call 都通過 `isReadOnlyToolCall(name, input)`（`protocol_capability.go` 中 `protocolAttemptWasReadOnly` 使用的同一個封閉式判定，底層是 `tools.IsReadOnlyObservationTool`，`tools/types.go:100`，以及 `tools.IsReadOnlyBashCommand`，`bash_policy.go:225`）時才符合；
   - `external_write`、`infra_mutation`、`credential_mutation`、`unknown` → 只有在紀錄中完全沒有任何 call 時才符合；
   - 紀錄不存在或不完整 → 不符合（fail closed）。

   **為什麼不能直接用 steps**：step 出錯時，fantasy 的 `Stream` 會回傳 `nil, err`，並丟掉已經完成的 steps（fantasy v0.41.1 `agent.go:1036-1041`）；hufu 只回傳 `result.Steps`（`coordinator_task_run.go:4571-4577`）。所以遇到 429 或 503 時，steps 是空的，即使之前已經執行過 tool。另外，`protocolAttemptWasReadOnly` 在沒有任何 call 時回傳 false（它回傳的是 `sawCall`，`protocol_capability.go:118-129`），不能直接拿來判定「零個 call」。

   **為什麼不能用其他現有資料**：
   - `AgentReadOnlyExecutionKey` 的拒絕清單（`tools/types.go:88-90`）不包含 bash，用 bash 寫過檔案的 attempt 會被誤判為可以 fallback，違反 I-5；
   - `receipt.ToolInvocations` 只記錄 dynamic tool（`coordinator_task_run.go:1234`）。

   **新增的紀錄**：在 `tool_policy_gate.go` 新增一個 attempt-scoped 的 executed-call recorder（安裝方式比照 `withDynamicToolInvocationSink`，`coordinator_task_run.go:1026`）。gate 在授權通過之後、**呼叫底層 tool 之前**記錄 `{tool name, input}`，所以即使 tool 執行到一半失敗，也會被計入（保守）。目前 gate 只回報被拒絕的 call（`:89-353`）。這份紀錄只存在記憶體中、只屬於單一 attempt，不需要持久化，因為 fallback 的決定是在同一個 process 內做出的。
4. 預算：`BudgetManager` 的 wall-clock 與 token 都還有剩餘，而且 per-attempt token 上限允許新的 attempt。
5. 沒有收到 cancel 要求。

任一步驟不成立 → 走既有的 recovery 路徑，分類與行為都與現在相同。拒絕的原因要記錄在 attempt receipt 的 `FallbackDeniedReason`。

### 6.7 Fallback 的執行方式

新檔案 `internal/team/execution_fallback.go`，hook 放在 dispatch 內的 attempt loop（`coordinator_task_run.go:658`）。

- fallback 就是 loop 的下一輪，使用 `candidateIndex+1`。
  - 它**不佔用** max-retries；另有一個計數器 `fallbacksUsed`，上限是 `len(candidates)-1`。
  - `attempt` 仍然遞增，以維持 receipt identity 唯一。因為 loop 目前是 `for attempt := 1; attempt <= maxAttempts`（`coordinator_task_run.go:658`），max-retries 為 0 時 fallback 永遠跑不到，所以 loop 的上限必須改成 `maxAttempts + fallbacksUsed`。
  - `attempt > 1` 的分支目前會呼叫 `recordRetry`（`:722`）與 `reflectOnFailure`（`:863`），並組出 retry context。**fallback 這一輪要跳過這三件事**：新的 candidate 從頭開始，就像第一個 attempt 一樣；isolated 模式下舊的 world 也已經丟棄。
- 新的 attempt 開始之前：
  1. 追加 canonical 事件 `execution_fallback_decided{task_id, occurrence_attempt, from_attempt, from_target, to_target, candidate_index, failure_class}`。不使用 `dispatch_id`（它不是 durable 的，§2.3）。
  2. 以 `ModelProfileRuntime.ResolveAdmission` 重新解析 candidate 的 context 與 output-token profile。
  3. isolated 模式下，先丟棄舊 world，再建立新 world。
- **attempt target 的傳遞**。**不新增欄位**，直接把既有的 `AttemptRequest.ExecutionTarget` 當作「這個 attempt 的 target」。理由：`LLMExecutionBackend.RunAttempt` 會拿 `request.ExecutionTarget` 對 backend 做 `ValidateTarget`，並用它覆寫 `request.ModelID`（`execution_backend.go:149-160`），而 `HufuLocalSubagentProvider` 也是用 `request.ExecutionTarget` 建立 provider（`subagent_hufu.go:120-128`）。如果另外加一個欄位，實際執行的仍然會是 primary model（靜默錯誤），或是在別的 backend 上的 candidate 會在 `ValidateTarget` 失敗。HF-OMP-008 必須明確修改以下各處：
  1. coordinator 目前從 `task.ResolvedExecutionTarget` 填入 `ExecutionTarget` 與 `ModelID`（`coordinator_task_run.go:1107,1138,1143`）。fallback 那一輪改為使用 `candidates[idx]`，讓 backend 的選擇、backend semaphore 與 `ModelID` 都跟著 candidate 走。
  2. `resolvedModel`（`coordinator_task_run.go:835` 附近）在 fallback 那一輪也改由 candidate 計算。
  3. `HufuLocalSubagentProvider.RunAttempt` 的 model 檢查（`subagent_hufu.go:73-79`）與 target 檢查（`:121` 附近）改為：若 task 有 `ExecutionCandidates`，`request.ExecutionTarget` 必須是其中之一（完全相等）；若沒有，維持現有的完全相等檢查。
  4. **不得** clone AgentDef 來換 model：`CreateAgent` 會優先使用 `InvocationModelID`（`agent.go:1655`），所以不需要改 AgentDef；`workerAgentResolutionAssertionMatches`（`subagent_provider.go:177-179`）的 `reflect.DeepEqual` 維持不變，傳入的仍然是 canonical agent。
  5. `coordinator_task_run.go:784-789` 的「durable retry 重設回凍結 model」只對 route fallback 例外：當這一輪是 fallback 時，使用 `candidates[idx]`；其他情況（包括一般 retry）行為不變。
- **tool envelope**：同一個 occurrence 的 tool envelope 必須保持不變（`execution-runtime.md` invariant 7）。HF-OMP-008 開工前先確認 `TaskExecutionEnvelope` 的任何 digest（`coordinator_task_execution_envelope.go`）是否取決於 model。如果有，必須在 admission 時為每個 candidate 各凍結一份 envelope digest；如果這會改變既有 envelope 的格式，停止並回報 maintainer。
- DAG 層級的 retry（task reset 之後的新 dispatch）一律從 candidate 0 重新開始，確保行為確定。resume 之後重新 dispatch 也一樣。
- occurrence 凍結的 `ExecutionTarget` 永遠不改；每個 attempt 實際使用的 target 只記錄在 receipt。
- 綁定 route 的 task 必須跳過 `nextStrongerModel` 與 protocol capability fallback（`coordinator_task_run.go:1941-1953`），需要測試。

### 6.8 Durable metadata 與 receipt

在 HF-OMP-001 新增的 `ExecutionReceipt.ExecutionTarget` 之外，再新增：

```go
CandidateIndex       *int                     `json:"candidate_index,omitempty"`
FallbackFrom         *execution.ExecutionTarget `json:"fallback_from,omitempty"`
FallbackFailureClass ProviderFailureClass     `json:"fallback_failure_class,omitempty"`
FallbackDeniedReason string                   `json:"fallback_denied_reason,omitempty"`
```

receipt 裡不得保存任何 credential。

### 6.9 Policy snapshot 與 resume

- `executionPolicyModelInputs` 加入每個已綁定 route 的所有 candidate（model 加 backend）。
- `ConfigurationHash` 加入以名稱排序的 route digest 清單；清單為空時不加入，所以不需要升 snapshot 版本。以 HF-OMP-000 的正規化 snapshot golden（§8 HF-OMP-000 第 4 項）驗證既有 team 的 snapshot 沒有多出任何鍵。
- hufu.yaml 中的 route 在 admission 之後被修改 → resume 時 fail closed，要用 `--new`（D6）。

### 6.10 Normative 文件修訂（在 HF-OMP-008 完成）

`docs/architecture/execution-runtime.md`：

- canonical model 圖加入 `ExecutionCandidates`。
- invariant 2 改為：「Dispatch 只解析凍結的 target：occurrence 的 `ExecutionTarget`，或對綁定 route 的 occurrence，依確定性 fallback policy 從凍結的 `ExecutionCandidates` 中選出的 entry。」
- registry 那一句改為「prevents undeclared scheduler-side fallback」。
- 新增「Execution routes and fallback」一節，摘要 §6.5–6.7。

`docs/architecture/model-metadata.md`：寫明 route candidate 的 capability 檢查語意（已知不相容就拒絕；unknown 只警告）。

### 6.11 Decision runtime

依照 D10。其他路徑完全不變：decision runtime 先選出 agent 或 candidate，被選中的 agent 若綁定單一 candidate 的 route，就照一般 target 執行。JUDGE→CHALLENGE→REVISE 的 durable binding 不受影響（需要回歸測試）。

### 6.12 Required tests

1. route 解析成確定、有序的 candidates；`task_created` 帶有 route name、digest 與 candidates。
2. 未知的 route、未知的 backend、agent backend、重複的 candidate、缺少 `fallback-on` → team load 失敗。
3. 已知不相容的 capability → 失敗；unknown → 警告。
4. D3 與 D10 的每一種互斥組合各有一個測試；coordinator 對綁定 route 的 agent 發出 `escalate: true` → tool error。
5. `-m` 與 `--worker-model` → 單一 candidate，沒有 fallback（D14）。
6. HTTP 429、503、connection refused、404 model not found → fallback 到下一個 candidate，並有事件與 receipt（使用 `httptest` server，不 mock LLM）。
7. 401、context 超限、verification 失敗、semantic rejection → 不 fallback。
8. shared 模式的 `workspace_write`：attempt 只做過唯讀的 view/grep 與唯讀 bash → fallback；**用 bash 寫過檔案**（例如 `echo x > f`）→ 不 fallback；isolated 模式 → fallback，而且新的 attempt 使用新的 world。
9. `external_write` 已執行 tool → 不 fallback。
10. 沒有下一個 candidate → 走既有的 recovery。
11. 預算用盡 → 不 fallback。
12. crash 或 resume 之後重新 dispatch → 從 candidate 0 開始，使用同一份凍結的清單。
13. hufu.yaml 的 route 被修改 → resume 時 policy drift fail closed。
14. 沒有 route 的 fixture team，正規化後的 policy snapshot JSON 等於 HF-OMP-000 的 golden。
15. telemetry 記錄確切的 failure class。
16. JUDGE→CHALLENGE→REVISE 的 durable binding 不受影響。
17. direct agent、normal worker、resume 三條路徑的行為一致；extra-model 路徑不受影響。
18. 綁定 route 的 task 不會觸發 `nextStrongerModel` 或 protocol capability fallback。
19. fallback attempt 通過 `HufuLocalSubagentProvider.RunAttempt` 的 model、agent、target 三項檢查，而且**實際呼叫的 model 就是 candidate 的 model**（用 `httptest` provider 檢查收到的 model 名稱）；`request.ExecutionTarget` 不在 `ExecutionCandidates` 中 → 被拒絕；沒有 route 的 task，檢查行為與現在完全相同。
20. 一般的 durable retry 仍然會重設回凍結的 model（只有 fallback 例外）。
21. max-retries 為 0 時，fallback 仍然會執行；fallback 那一輪不會呼叫 `recordRetry` 與 `reflectOnFailure`，也不會帶 retry context。
22. 在 429 或 503 發生之前已經執行過寫入型 tool（包括 bash 寫檔）的 shared 模式 attempt → executed-call recorder 有記錄 → 不 fallback；recorder 不存在 → 不 fallback。
23. candidate 位於不同的 LLM backend（例如 `ollama/...` → `openai/...`）時，backend 的選擇與 semaphore 都跟著 candidate 走。

### 6.13 Files

新增：

- `internal/team/execution_route.go`
- `internal/team/execution_route_admission.go`
- `internal/team/provider_failure.go`
- `internal/team/execution_fallback.go`
- 對應的 `_test.go`

修改：

- `internal/config/config.go`
- `parse.go`、`team_manifest.go`
- `cmd/hufu/model_overrides.go`、`cmd/hufu/team_setup.go`
- `decision_admission.go`
- `status.go`
- `coordinator_eventstore.go`、`event_reducers.go`
- `task_occurrence_projection.go`
- `execution_receipt.go`
- `execution_policy_snapshot.go`
- `coordinator_task_run.go`（fallback hook、loop 上限、跳過 retry 步驟、凍結 model 重設的例外、`ExecutionTarget`/`ModelID`/`resolvedModel` 改用 candidate）
- `subagent_hufu.go`（model 與 target 檢查）
- `execution_backend.go`（確認 `ValidateTarget` 與 `ModelID` 覆寫在 candidate 上的行為；原則上不需要修改）
- `tool_policy_gate.go`（executed-call recorder）
- `coordinator.go`（`escalate` 的 tool 說明）
- `coordinator_tools.go`（拒絕清單）
- `coordinator_run.go`、`coordinator_tools_delegate.go`（手動組裝的 TodoSpec）
- `event_types.go`

---

## 7. P1-B — Worker Attempt Hub

### 7.1 定位

Worker attempt hub 是 canonical runtime state 的唯讀投影。它不是 scheduler、不是 registry、不是 task database、也不是第二個 event store。

### 7.2 Identity

- 一個 attempt 的邊界由開始它的 `task_started` 事件決定，但**只有帶 `dispatch_attempt` 欄位的 `task_started` 才算 attempt 的開始**。`task_started` 的事件類型是從 status 推導的（`coordinator_eventstore.go:1402`），所以任何狀態維持 in_progress 的重新 commit 也會發出它，例如 `coordinator_terminal.go:391`、`coordinator_tools_delegate.go:246` 的 parent 重新 commit，以及 `coordinator_eventstore.go:913`。如果把這些都算成 attempt 的開始，一個 attempt 會被拆成好幾個。HF-OMP-001 只在兩個真正開始 attempt 的地方（`coordinator_task_run.go:433` 與 `:761` 附近）寫入 `dispatch_attempt`。
- `AttemptKey` = `sha256(run_id \x00 task_id \x00 <該 task_started 事件的 event ID>)` 的前 16 個 hex 字元。event ID 是 durable 的（`event_store.go:39`），可以經由 `IndexedEvent.Event.ID` 取得（`internal/inspect/source.go:12-15`），所以 key 可以從事件確定性地重建。不使用 random UUID，也**不使用 `DispatchID`**（不是 durable 的，§2.3）。crash 之後重新 dispatch 會產生新的 `task_started` 事件，因此會得到不同的 key，不會和 crash 前的 attempt 合併。
- 兩個 attempt 計數器都要呈現，不做合併（§2.3）：`Attempt` 是 dispatch 內的計數（等於 `receipt.Attempt`），`OccurrenceAttempt` 是 `Retries+1`（等於事件中的 `attempt`）。
- HF-OMP-001 會讓 `task_started` 的 payload 帶 `dispatch_attempt`，並讓 receipt 帶 `OccurrenceAttempt`。receipt 以 `(run_id, task_id, occurrence_attempt, attempt)` 對應回開始它的 `task_started` 事件，不必解析 detail 文字（`"attempt N/M"`）。
- `RunID`（occurrence 所屬的 run）與 `InvocationRunID`（實際執行該 attempt 的那次 invocation，取自對應 `task_started` 事件的 `RunEvent.RunID`）要分開呈現。

### 7.3 `WorkerAttemptView`

新檔案 `internal/inspect/workers.go`：

```go
type WorkerAttemptView struct {
    AttemptKey        string `json:"attempt_key"`
    RunID             string `json:"run_id"`
    InvocationRunID   string `json:"invocation_run_id,omitempty"`
    TaskID            string `json:"task_id"`
    Agent             string `json:"agent"`
    StartedEventID    string `json:"started_event_id"`
    Attempt           int    `json:"attempt"`
    OccurrenceAttempt int    `json:"occurrence_attempt"`

    TaskStatus string `json:"task_status"`
    Activity   string `json:"activity"`

    ExecutionTarget string `json:"execution_target,omitempty"`
    CandidateIndex  *int   `json:"candidate_index,omitempty"`
    RouteName       string `json:"route_name,omitempty"`

    WorkspaceMode     string `json:"workspace_mode"` // shared | isolated
    AttemptWorldID    string `json:"attempt_world_id,omitempty"`
    AttemptWorldState string `json:"attempt_world_state,omitempty"`

    StartedAt      time.Time `json:"started_at,omitzero"`
    FinishedAt     time.Time `json:"finished_at,omitzero"`
    DurationMillis int64     `json:"duration_ms,omitempty"`

    Usage *team.ExecutionUsage `json:"usage,omitempty"` // nil = unknown

    ResultContractID string `json:"result_contract_id,omitempty"`
    ResultValidation string `json:"result_validation,omitempty"`

    FailureClass  string `json:"failure_class,omitempty"`
    RetryCount    int    `json:"retry_count"`
    FallbackCount int    `json:"fallback_count"`
}

// Pure: never writes the EventStore or session; now is injected.
func ProjectWorkerAttempts(todos []*team.TodoItem, events []IndexedEvent, now time.Time) []WorkerAttemptView
```

結果依 task ID 排序，同一 task 內再依 `(OccurrenceAttempt, Attempt)` 排序。

### 7.4 Activity（只從 lifecycle 推導，D12）

| 條件 | activity |
|---|---|
| status 為 pending 或 planned | `queued` |
| 該 attempt 最新的事件是 `execution_fallback_decided` | `fallback` |
| attempt world 處於 applying | `integrating` |
| status 為 protocol_incomplete | `result_repair` |
| status 為 verifying | `verifying` |
| status 為 in_progress 且還沒有 receipt | `running` |
| done、error、blocked、skipped、paused | `terminal` |
| 其他 | `unknown` |

v1 不提供 model_stream 或 tool 等更細的 activity。

### 7.5 資料來源

- task lifecycle 事件的 payload：`execution_receipt`、`execution_target`、`attempt`（`coordinator_eventstore.go:1444-1600`）。事件中的 `dispatch_id` 永遠是空字串，不得使用。
- `task_started`：決定 attempt 的邊界、`StartedEventID` 與 `StartedAt`；HF-OMP-001 之後帶有 `dispatch_attempt`。
- HF-OMP-001 加入 receipt 的 `ExecutionTarget` 與 `Usage`。
- P0-A 的 receipt 欄位、P0-B 的 attempt world 事件、P1-A 的 `execution_fallback_decided` 與 receipt 欄位。
- 執行中的 attempt 還沒有 receipt：usage 與 result validation 顯示 unknown；target 取自 fallback 事件或 primary。

### 7.6 CLI

在 `statuscmd.go` 新增 `--workers` 與 `--verbose`。

```bash
hufu status --workers
hufu status --workers --json      # 既有 JSON 輸出加上 "workers": [...]（只有帶 --workers 時才加）
hufu status --workers --verbose
```

輸出範例：

```text
TASK  AGENT     ATT  STATE        ACTIVITY      TARGET                  WORKSPACE  AGE
12    coder     1    in_progress  running       ollama/qwen3:32b        isolated   42s
13    reviewer  1    verifying    verifying     openai/gpt-5.6-terra    shared     18s
```

`--verbose` 另外顯示：route、candidate index、fallback 次數、usage、result contract 與 validation、failure class、world ID 與 state、receipt identity。

資料的載入沿用 `inspect.InspectOverview` 的事件讀取路徑（`statuscmd.go:97`），不得寫入任何 store。

### 7.7 TUI

- detail view（`internal/tui/detail_view.go:32-134`）新增 Attempts 區塊，顯示選取 task 的：attempt、target、workspace、activity、duration、tokens、fallback、最近一次失敗。
- 資料經由既有的 `OperatorDetailsMsg` 送達（`loadTUIOperatorDetails`，`cmd/hufu/display.go:1343`），使用 TUI 自己的型別（例如 `tui.OperatorAttemptDetail`），由 `cmd/hufu` 從 `inspect.WorkerAttemptView` 轉換而來。**不得**放進 `OperatorSnapshot`：
  - `internal/operator` 只 import `internal/utils`，而 `internal/inspect` 會 import `internal/operator`；讓 `OperatorSnapshot` 引用 `WorkerAttemptView` 會形成 import cycle；
  - `OperatorSnapshot` 的 `SnapshotID` 是內容雜湊（`internal/operator/snapshot.go:13-22`），不斷變化的 duration 會讓它每次刷新都改變。
- **不新增** ticker 或 polling loop。
- 既有的 epaper preset（`theme.go:28-58`）：新增的欄位必須用文字表達狀態，不依賴 spinner 或動畫。需要補回歸測試。

### 7.8 Privacy

- projection 不得包含以下內容：
  - 原始 `output`（`coordinator_eventstore.go:1546-1549`）；
  - repair prompt（`execution_receipt.go:51`）；
  - tool 的 args 與 result；
  - prompt 或 transcript 內容。
- failure 摘要一律經過 `RedactedFailureEvent`（`failure_event.go:45`）與 `utils.RedactSecrets`（`internal/utils/redact.go:335`）。
- 測試：在每一個被排除的欄位放入 secret fixture，確認 CLI JSON、TUI 與 report 的輸出都不含它。

### 7.9 Metrics 與 report

在 `RunMetrics`（`run_result.go:692`）新增以下欄位。`RunStats.attempts_total` 已經存在，不重複定義。**每個欄位由產生該資料的 PR 定義並累加**，HF-OMP-010 只負責投影與顯示：

| 欄位 | 由哪個 PR 定義並累加 |
|---|---|
| `structured_result_validation_failures` | HF-OMP-003 |
| `isolated_attempts_total`、`attempt_workspace_conflicts_total`、`attempt_worlds_orphan_removed` | HF-OMP-006 |
| `worker_fallbacks_total`、`worker_fallbacks_by_class` | HF-OMP-009 |

`--report`（`cmd/hufu/report.go`）新增 Workers 一節，資料來自同一個 projection。

### 7.10 Required tests

1. 由 canonical events 重建的結果是確定的，而且重建過程不修改 EventStore（比對事件前後的 hash）。
2. resume 保留原本 attempt 的關聯；新的 invocation 不會覆蓋先前的 receipt。
3. 重複的 idempotent 事件不會產生重複的 attempt。
4. fallback 會產生新的 AttemptKey，`FallbackCount` 正確。
5. attempt world 的狀態正確關聯。
6. result validation 的失敗看得見。
7. cancelled、blocked、error 的狀態正確。
8. CLI JSON、TUI 與 report 使用同一個 projection 函式。
9. secret 不會外洩（§7.8）。
10. 缺少資料來源時顯示 unknown，不做推測。
11. race 測試：多個並行 worker 加上 TUI 刷新。
12. epaper 與 no-spinner 的 render 回歸。
13. `now` 以注入方式提供，projection 是純函式（同樣的輸入得到同樣的輸出）。

### 7.11 Files

新增：

- `internal/inspect/workers.go`、`internal/inspect/workers_test.go`

修改：

- `cmd/hufu/statuscmd.go`
- `cmd/hufu/display.go`（`loadTUIOperatorDetails` 的轉換）
- `internal/tui/detail_view.go`，以及 `OperatorDetailsMsg` 所在的檔案（新增 TUI 自己的 attempt 型別）
- `run_result.go`
- `cmd/hufu/report.go`

不修改 `internal/operator`。

---

## 8. PR 分解

**每個 PR 共同的 exit criteria：**

- `go build ./...`、`go vet ./...`、`go test ./...` 全部通過；
- `golangci-lint run ./...` 通過（`.golangci.yml`，gocyclo 上限 40）；
- 動到並行邏輯的 PR 要加跑 `go test -race ./internal/team/... ./internal/tools/... ./internal/inspect/...`；
- 動到文件的 PR 要通過 `bin/check-docs`；
- 每個 PR 都必須讓 repository 維持可以 build，而且可以獨立審查。

### HF-OMP-000 — Characterization（只加測試，不改 production code）

先盤點已經存在的測試，避免重工。例如：`subagent_binding_test.go`、`task_occurrence_durability_test.go`、`worker_model_override_durability_test.go`、`rca20_task_occurrence_test.go`、`protocol_repair_test.go`、`execution_compatibility_migration_test.go`、`subagent_codex_test.go`。然後只補上缺少的部分：

1. `ExecutionTopology` / `ModelTopology` 的 fan-out 語意：長度大於 1 就 fan-out；`-m` 不會清掉 `ExtraModels`。
2. 會變更狀態的 extra-model leaf 共用 `projectDir`，記錄 D13 的現況。
3. shared 寫入者的 whole-root exclusive claim；`side_effect: none` 的 read claim。
4. 對一個沒有 contract 也沒有 route 的 fixture team，建立**正規化的 policy snapshot golden**。不能直接固定 `ConfigurationHash`：snapshot 一定包含預設的 codex world，而它會雜湊 `ProjectRootHash(c.projectDir)` 以及 PATH、CODEX_HOME 等環境值（`execution_policy_snapshot.go:134-145,334-341,483-487`），每次測試的暫存路徑都不同。做法：
   - 以 `t.Setenv` 固定 PATH、CODEX_HOME、HOME 等會被雜湊的環境變數；
   - 把 snapshot 序列化成 canonical JSON，並把與路徑有關的欄位替換成固定的佔位字串；
   - 將結果存成 testdata golden 檔。
   之後的 PR 在不使用新功能時，這份 golden 必須完全不變，也就是不得多出任何鍵（包括值為 null 的鍵）。
5. receipt 的 `Backend` 會被 Todo 覆寫（`status.go:1652-1654`），記錄現況，HF-OMP-001 會改掉。
6. 兩個 attempt 計數器（receipt 的計數每次 dispatch 重新開始；事件中的計數是 `Retries+1`）。
7. `RequiresGroundedResult`：外部路徑會強制檢查；本地的 promotion 與 repair 目前沒有，記錄現況，HF-OMP-001 會改掉。
8. `StatusEvent.ToolArgs` 沒有遮蔽，記錄現況，HF-OMP-001 會改掉。
9. `escalate`、`escalate-on-retry`、protocol capability fallback 的現有行為。
10. `ParseFreeTextResult` 的呼叫者都會覆寫 Source。

Exit：全部通過，而且沒有任何 production 程式碼變更。

### HF-OMP-001 — Prerequisite fixes

1. 讓本地的 promotion（`coordinator_task_run.go:1583-1596`）與 result-only repair（`:1405-1692`）也遵守 `RequiresGroundedResult`：依照 `execution_contract.go:105-110` 的註解，這兩條路徑的結果永遠不能作為 completion，task 改依 retry policy 重新執行。
   - 這是行為變更，會影響 `.agent-teams/hufu-coding` 與 `.agent-teams/hufu-code-review`，必須在 PR 描述中列出。
2. `StatusEvent.withTool`（`status.go:78`）改用 `utils.RedactSecrets` 遮蔽 `ToolArgs`，並把長度限制在 2 KiB。TUI 的 args 預覽改用遮蔽後的值。
3. 在 `ExecutionReceipt` 新增：
   - `ExecutionTarget execution.ExecutionTarget json:"execution_target,omitzero"`，記錄 attempt 實際使用的 target；
   - `Usage *ExecutionUsage json:"usage,omitempty"`。
   每個 native 與 external attempt 都要填入這兩個欄位。receipt 帶有 `ExecutionTarget` 時，`SetExecutionReceipt`（`status.go:1652-1654`）與 execution event shadow（`execution_events.go:723-725`）不得再覆寫 `Backend`。exporter 的一致性要一起更新；沒有這兩個欄位的舊 receipt 仍然可以讀取。
4. attempt identity 的 durable 化（§7.2 需要）：
   - `ExecutionReceipt` 新增 `OccurrenceAttempt int json:"occurrence_attempt,omitempty"`（`Retries+1`）；
   - `task_started` 事件的 payload 新增 `dispatch_attempt`（dispatch 內的 attempt 計數，也就是 `coordinator_task_run.go:658` 的 `attempt`）。**只有**真正開始一個 attempt 的兩個地方（`coordinator_task_run.go:433` 與 `:761` 附近）會寫入這個欄位；其他狀態維持 in_progress 的重新 commit（`coordinator_terminal.go:391`、`coordinator_tools_delegate.go:246`、`coordinator_eventstore.go:913`）不得寫入。需要有測試。
   reducer 與 shadow projection 要同步更新；沒有這些欄位的舊事件與舊 receipt 仍然可以讀取。
5. 把 HF-OMP-000 的第 5、7、8 項測試翻轉成新的預期。

### HF-OMP-002 — ResultContract 設定、compile 與 durable ref

內容為 §4.1–4.4 與 §4.9 的欄位定義，但**還不做** runtime 驗證。

- 這個 PR 期間，宣告了 `result-contract` 的 team 在載入時以 `result_contract_unsupported` 失敗，避免兩個 PR 之間出現沒有強制檢查的空窗。HF-OMP-003 會移除這個限制。
- **gate 的位置**：compile 與組合檢查（§4.3）要先完整執行，只有全部通過之後，team load 的最後一步才回傳 `result_contract_unsupported`。這樣錯誤的設定仍然會得到精確的錯誤訊息。
- 測試直接呼叫 compile、drift 與 hash 相關的函式（單元測試），不經過 team load 的 gate：§4.11 的第 10、14、16、18 項，以及 policy snapshot 在設定 contract 後會改變 hash 的測試。另外要有一個測試確認 gate 本身會擋下 team load。

### HF-OMP-003 — Structured payload 的 runtime 驗證

內容為 §4.5–4.8 與 §4.9 的行為、`RunMetrics.structured_result_validation_failures`（§7.9），並移除 `result_contract_unsupported`。

測試：§4.11 全部。**P0-A 完成。**

### HF-OMP-004 — Execution-root 重新綁定

內容為 §5.6。這個 PR 不會產生任何行為變化（沒有 isolated mode，key 永遠不會被安裝）。

測試：
- table-driven 地逐一走過 builtin tool，確認它們在注入的暫存 root 中運作；
- `DeniedWriteRoots` 會拒絕寫入 canonical root 與其他 world，但允許寫入 execution root；
- 安裝 execution root key 之後 bash 沒有被停用；bash 的 cwd 與 `GIT_CEILING_DIRECTORIES` 正確；
- `ExecutionRoot` 與 `TaskPathScope` 同時存在時，write/edit/multiedit 直接回錯；
- verification 的 cwd 使用 attempt root，fingerprint 仍然使用 canonical root；
- 也就是 §5.12 中與 tool 綁定有關的項目；
- `projectDir` 的分類表放在 PR 描述。

### HF-OMP-005 — Isolated copy world library

內容：

- versionstore 的 `DiscoverManagedPaths` 與 `gitIgnored` export；
- `IsolatedCopyExecutionWorld` 的 Prepare、Snapshot、Release；
- `AttemptWorkspaceDelta` 與 delta 驗證；
- re-entrant 的 Apply library；
- integration lock。

這個 PR **不接線**到 coordinator。

測試：
- §5.12 中與 world 本身有關的項目：source 與 world 中的 symlink 逃逸、`..` 與絕對路徑、`.git` 排除、nested repo 與 submodule、FIFO 與 device、只刪除有 owner 標記的目錄、被忽略的 `.env` 不被複製、world ID 是 nonce；
- §5.14 的第 3、4、6、16 項以 library 層級測試；
- `-race`。

§5.12 中與 admission 有關的項目（帶 `scp`、`sudo` 等 tool 的 agent 被拒絕）在 HF-OMP-006 測試；與 tool 綁定有關的項目在 HF-OMP-004 測試。

### HF-OMP-006 — Isolation 接線、套用與 recovery

內容：

- `worker-workspace` 設定與 admission（§5.2、§5.3）；
- resource scope v2（§5.9）；
- native attempt 路徑接線；
- on-verified 套用（§5.8）；
- `workspace_conflict` 類別；
- 事件與 reducer；
- `RunMetrics` 的 `isolated_attempts_total`、`attempt_workspace_conflicts_total`、`attempt_worlds_orphan_removed`（§7.9）；
- 啟動時的 reconcile（§5.11）；
- 文件（§13）。

測試：§5.14 全部，以及 §5.12 中與 admission 有關的項目。**P0-B 完成。**

### HF-OMP-007 — Execution route 的 compile 與綁定

內容：§6.1–6.4、§6.9，以及 durable 欄位。

- 這個 PR 只支援單一 candidate 的 route；多 candidate 的 route 在 team load 時以 `execution_route_fallback_unsupported` 失敗，HF-OMP-008 會移除這個限制。
- **gate 的位置**：§6.4 的所有 compile 與互斥檢查（包括只適用於多 candidate route 的 `fallback-on` 驗證與 `execution_route_conflict`）都要先執行，全部通過之後才回傳 `execution_route_fallback_unsupported`。
- 測試：§6.12 的第 1、3、5、13、14、17 項走完整的 team load。第 2、4 項（多 candidate 相關）直接呼叫 compile 與 admission 函式做單元測試。另外要有一個測試確認 gate 本身會擋下多 candidate route。

### HF-OMP-008 — Deterministic fallback engine

依賴：HF-OMP-006 與 HF-OMP-007。

內容：§6.5–6.8、§6.10，並移除 `execution_route_fallback_unsupported`。

測試：§6.12 全部。

### HF-OMP-009 — Routing telemetry 與回歸矩陣

內容：

- fallback 相關的 metrics：`worker_fallbacks_total`、`worker_fallbacks_by_class`（§7.9 中只有這兩項屬於這個 PR）；
- report 的 route/fallback 區段；
- execution event shadow 的一致性；
- §10 回歸矩陣中 routing 那一欄全部補齊。

**P1-A 完成。**

### HF-OMP-010 — Worker attempt projection

依賴：HF-OMP-003、HF-OMP-006、HF-OMP-009。

內容：§7.1–7.5、§7.8，以及把 §7.9 各 PR 已經定義的 metrics 投影到 projection 中（這個 PR 不新增 metric 欄位）。

測試：§7.10 的第 1–10、13 項。

### HF-OMP-011 — CLI / TUI hub

內容：§7.6、§7.7、report 的 Workers 區段、文件。

測試：§7.10 的第 8、11、12 項。**P1-B 完成。**

---

## 9. Dependency graph

```text
HF-OMP-000
    │
    ▼
HF-OMP-001
    │
    ├─────────────────┬──────────────────────┐
    ▼                 ▼                      ▼
002 Result compile   004 Root rebinding     007 Route compile
    │                 │                      │
    ▼                 ▼                      │
003 Result runtime   005 World library       │
    │                 │                      │
    │                 ▼                      │
    │              006 Isolation wiring ─────┤
    │                 │                      ▼
    │                 │                   008 Fallback
    │                 │                      │
    │                 │                      ▼
    │                 │                   009 Telemetry
    │                 │                      │
    └────────────┬────┴──────────────────────┘
                 ▼
          010 Attempt projection
                 │
                 ▼
          011 CLI / TUI
```

HF-OMP-001 完成之後，三條路線（002→003、004→005→006、007）可以由不同的 agent 平行開發。每個 PR 都要 rebase 到最新的 main，並通過 HF-OMP-000 的 characterization 測試。

---

## 10. Cross-cutting regression matrix

每個改變 execution 語意的 PR 都必須覆蓋下表中與它相關的欄。

| Path | Result contract | Isolation | Routes / fallback | Hub |
|---|---|---|---|---|
| normal team worker | required | required | required | required |
| direct agent（`coordinator_run.go:440`） | required | required | required | required |
| extra-model fan-out | 組合時拒絕；否則不變 | 組合時拒絕；否則不變 | 組合時拒絕；否則不變 | visible |
| sidecar task | 永不綁定 | n/a | 永不綁定 | visible |
| external agent backend（codex） | required（proposal 欄位） | admission 拒絕 | 不可作為 candidate | required |
| resume interrupted task | required（drift） | required（reconcile） | required（從 candidate 0 開始） | required |
| protocol repair | required（不重播） | 未套用前才可 | 不 fallback | required |
| JUDGE / CHALLENGE / REVISE | D10 拒絕；否則保留 | 依 policy | D10 拒絕；保留 binding | visible |
| unattended mode | required | required | required | required |
| phase workflow | required | admission 拒絕 | required | required |
| workset bounded scope | n/a | isolated 時不推導 bounded scope | n/a | visible |

---

## 11. Failure semantics 與 reason code

| Code | 發生時機 | 類別 / 處置 |
|---|---|---|
| `result_contract_invalid` | team load | 載入失敗 |
| `result_contract_unsupported` | team load（組合限制，或 002 的過渡期） | 載入失敗 |
| `result_contract_drift` | resume | task blocked |
| `structured_payload_invalid` | submit_result 的 tool error | attempt 繼續，worker 可在同一 attempt 修正 |
| `structured_payload_missing` | attempt 結束時 | `protocol_incomplete` → result-only repair |
| `workspace_isolation_unsupported` | team load 或 task admission | 載入失敗，或 delegation 被拒絕 |
| `workspace_isolation_unavailable` | Prepare | `FailureEnvironment`，`needs_human` |
| `workspace_delta_rejected` | delta 驗證 | `FailurePolicy`，沿用既有預設 |
| `workspace_conflict` | Apply 的前置條件檢查 | 新類別 `workspace_conflict`，`retry_worker` |
| `workspace_apply_incomplete` | Apply 發生 I/O 錯誤，或 recovery 失敗 | task blocked，`needs_human` |
| `execution_route_invalid` | team load | 載入失敗 |
| `execution_route_conflict` | team load，或 delegation 時 | 載入失敗，或 tool error |
| `execution_route_fallback_unsupported` | 007 的過渡期 | 載入失敗 |
| `execution_fallback_denied` | 記錄在 receipt 與事件 | 不是失敗類別；走既有 recovery |

isolated 模式要求 isolation，但 world 無法準備時，**不得**偷偷退回 shared 模式。

---

## 12. Migration / compatibility

- 所有新功能預設關閉：沒有 result contract、`worker-workspace` 為 `shared`、沒有 execution route、status 輸出不變。
- 未使用新功能的 team：
  - 正規化後的 policy snapshot 與 HF-OMP-000 的 golden 相同（因此 `ConfigurationHash` 的計算輸入不變）；
  - resource scope digest 不變；
  - durable 事件不含新欄位（全部 omitempty）。
- 舊 session 可以讀取：resource scope snapshot v1、沒有 `ExecutionTarget`/`Usage` 的舊 receipt、沒有新欄位的 `TodoItem`。
- 在 built-in team 或個人 hufu.yaml 啟用新功能，屬於 maintainer 的決定，不在本計畫範圍（§0.4）。

---

## 13. Documentation

每個 PR 只更新與自己相關的文件，並通過 `bin/check-docs`。

| 文件 | 更新內容 | PR |
|---|---|---|
| `docs/architecture/execution-runtime.md` | isolated-copy world、attempt-bound root、`ExecutionCandidates` 與 invariant 修訂、fallback | 006、008 |
| `docs/architecture/model-metadata.md` | route candidate 的 capability 檢查語意 | 008 |
| `docs/architecture/workspace-versioning.md` | 套用屬於 run 期間的寫入；attempt world 不是 snapshot | 006 |
| `docs/architecture/operator-experience.md` | `status --workers`、TUI Attempts | 011 |
| `docs/reference/agent-format.md` | `result-contract`、`worker-workspace`、`execution-route` | 003、006、007 |
| `docs/reference/operator-command-reference.md` | `hufu status --workers/--verbose` | 011 |
| `AGENTS.md` | 設定表與功能列表（result contract、isolation、routes） | 003、006、008 |
| README 的設定範例 | `execution-routes`（hufu.yaml） | 008 |

文件必須明確區分以下名詞，不得混用：

- control root（session workspace）
- subject root（source project root）
- attempt world 與 execution root
- runtime workspace（`<control>/runtime`）
- workspace version snapshot
- Git working tree

---

## 14. 明確 Non-goals

1. 不使用 git worktree，不寫入使用者的 `.git`。
2. 不引入第二個 orchestrator 或第二套 routing authority。
3. 不允許 subagent 繞過 coordinator 自行 spawn 受 hufu 控制的子代理。
4. worker hub 不當 scheduler，也不建立第二份 canonical worker DB。
5. 不做三方合併、不做 Git merge，也不提供 proposal 或手動 apply 模式（v1）。
6. 不把任意 free-text 解析結果當作 structured output。
7. 不讓 provider 決定 fallback；不在 runtime 重新解讀 model prefix。
8. 不因為 isolation 就假設外部副作用可以安全重試。
9. 不實作 container 或 VM 等級的安全隔離。bash 以絕對路徑寫入 canonical root 無法被阻擋。
10. 不改變 JUDGE/CHALLENGE/REVISE 已完成的 durable binding 語意。
11. 不修改 extra-model fan-out 的既有行為（D13）。
12. 不實作 workspace versioning 的 attempt-level checkpoint（PR-10）。
13. 不讓 Codex 或其他 agent backend 在 isolated world 中執行，也不讓它們成為 fallback candidate（v1）。
14. 不新增 token 或 tool 等級的進度事件（D12）。
15. 不 deprecate `escalate` / `escalate-on-retry`。

---

## 15. 完成定義

四項能力全部完成時，以下情境必須以 e2e 測試（不 mock LLM，使用既有的 fake model 或 `httptest` provider）成立：

```text
Coordinator 平行 dispatch 3 個 worker
    ├─ worker A（coder，isolated，route coding）→ world A
    ├─ worker B（coder，isolated，route coding）→ world B（與 A 改同一檔案）
    └─ worker C（reviewer，shared，side_effect none，result-contract）
                │
A 的 candidate 0 回 429 → execution_fallback_decided → candidate 1、新的 world
                │
A 通過驗證 → 套用成功；B 得到 workspace_conflict → 在新的 base 重試 → 套用成功
C 提交 schema-valid 的 payload → verification → done
                │
canonical EventStore → ProjectWorkerAttempts → status --workers / TUI / report
```

而且：

- 任何 UI 都可以從 canonical state 重建；
- worker 無法透過 result JSON 提升權限；
- 並行的寫入型 worker 不會互踩，衝突時 fail closed 並重試；
- fallback 不會重播不確定的副作用；
- route 與 fallback 的每個決定都可以稽核；
- crash 或 resume 不會改變凍結的 contract、candidates 與 workspace policy；
- 未 opt-in 的既有 team，行為與 `ConfigurationHash` 都不變。

---

## 16. Coding-agent execution instructions

每個 PR：

1. 先讀 `AGENTS.md`、`CLAUDE.md`，以及 §13 列出的相關 architecture 文件。
2. 先寫 characterization 或會失敗的測試，再寫實作。
3. 不跨 PR 預先加入後續 phase 的欄位或 schema。
4. 每個新增的 durable 欄位都要同步處理：event payload、reducer、session/checkpoint、receipt、report/JSON、resume、舊資料的讀取相容性、shadow projection。
5. 修改 coordinator lifecycle 時，要檢查 normal worker、direct agent、extra-model、external agent backend、sidecar、resume 等路徑（§10）。
6. 並行相關的程式碼必須跑 `go test -race`。
7. filesystem 相關功能必須包含 symlink 與路徑的攻擊性測試。
8. 不以 prompt 措辭取代確定性的 policy。
9. 不以 telemetry shadow log 作為正確性的依賴。
10. 新邏輯寫在新檔案；§2.6 列出的大檔案只加 hook（CLAUDE.md 的 800 行規則）。錯誤一律以 `fmt.Errorf("doing X: %w", err)` 包裝；測試採 table-driven。
11. 不得偏離 D1–D14。若既有的 normative architecture 與本文件衝突，或必須偏離某個決策，停止並回報 maintainer，不得繞過既有 invariant。
12. 每個 PR 的描述都要列出：與本文件的偏差、新增的 durable 欄位，以及受影響的 built-in team。
13. commit message 若含有 backtick，必須使用 single-quoted heredoc（`git commit -F- <<'EOF'`）。
