# Hufu/Pilot revalidation incident report

日期：2026-08-08
範圍：`./run.sh` 啟動的 `hufu-pilot-integration` team
結論：**未完成 minimal-poc revalidation；依使用者指定的「重試超過 10 次即停止」規則停止。**

## 1. 執行結論

這次不是 Pilot 執行檔故障。已執行的 Pilot 命令都沒有出現 executable、non-zero command 或 driver assertion error；失敗點在 Hufu 的協調與 evidence protocol。

最後一輪沒有開始任何 state-changing checkpoint。state probe 成功，但 contract reader 被 Hufu/team contract 判定為 evidence-integrity failure，協調器因此沒有派送 §3.1、§3.2 或 §3.3。

依硬停止條件停止後，goal 狀態為 `blocked`。沒有宣稱 revalidation 成功。

## 2. 執行量與停止原因

目前 workspace 中共有 103 個 `hufu-pilot-integration-run-*` 目錄，包含歷史執行與本次事故窗口。最後一個事故窗口是：

| 時間區間（UTC） | run 數 | 主要結果 |
|---|---:|---|
| 2026-08-08 09:05–10:34 | 11 | cancelled / partial / blocked |

因此已確定超過使用者允許的 10 次重試上限。最後一輪由外部硬停止中止；它的 execution event 是 `outcome=cancelled`，協調器在中止前已準備提交「contract-reader terminal BLOCKED」摘要。

## 3. 正常完成過的部分

### 3.1 Hufu 本身

每次通用 Hufu 修正後都重新驗證過：

- `go test ./...`：PASS
- `golangci-lint run`：`0 issues`
- `go build -o hufu ./cmd/hufu`：PASS
- `./hufu version`：`hufu dev`, Go `go1.26.5`

### 3.2 Read-only state probe

在多輪中，六個 state-probe 命令都成功：

1. `pwd`
2. `go version`
3. topology 檔案存在檢查
4. `git check-ignore --quiet tmp`
5. `pilot vm-target list`
6. `pilot vm-target topology status --topology docs/topologies/minimal-poc-topology.yaml`

最後一輪的 transcript 顯示全部 exit 0；三台 VM (`client-vm`, `freeipa-server`, `nexus`) 都是 running，且 topology wiring 存在。

### 3.3 §3.1 candidate freeze

在 2026-08-08 10:25–10:30 的 run 中，§3.1 完整成功，七個 bash call 全部 exit 0：

- HEAD：`5c981ad7a279c6f4046a0f7d474cc6187b80804c`
- tree：`b0cd0c5b2c36e3a5143f0d057e92b31c84fe7fea`
- Pilot：`/home/ubuntu/bin/pilot`
- Pilot SHA-256：`1bb781da0a8c5c91c9af059ad8ec310991c73bce951be00e95a240e3b6e2f988`
- Trec：`/home/ubuntu/bin/trec`
- Trec SHA-256：`f618765829f50ab47f0cad41d738de57ae7ac189991bff1e1eeca2774ea02035`

第 7 個 `sha256sum` 使用了同一 worker 第 5/6 個 `which` 的絕對路徑，證明動態 argv 修正後可以正常工作。

### 3.4 §3.2 topology up/status

在較早的成功 checkpoint 中，以下兩個 Pilot 命令都 exit 0：

- `./pilot vm-target topology up --topology docs/topologies/minimal-poc-topology.yaml`
- `./pilot vm-target topology status --topology docs/topologies/minimal-poc-topology.yaml`

`up` 走既有 VM 的 idempotent path，這符合 task contract；不是 Pilot 錯誤。

## 4. 失敗時間線與根因

### 4.1 §3.3 超長 task 造成 worker context switch

證據 run：`workspace/hufu-pilot-integration-run-20260808T100538Z/`

§3.3 是十個 slot 的 closed workflow：`bash, write, bash, bash, bash, write, bash, bash, bash, submit_result`。原本把完整 DSL、secret、role index、TREC 互動規則全部塞進超長 coordinator prompt。

結果 worker 在尚未執行任何 §3.3 slot 前，切回舊的 §3.1 脈絡並提交「沒有執行」的結果。Hufu 正確將它視為 protocol/execution failure，沒有讓 Pilot 繼續跑。

已做的通用修正：把 §3.3 變成 team-owned 的精簡 closed task contract，明確列出十個 slot、停止條件、禁止探索與 secret reference；不再依賴超長模型上下文維持 workflow。

### 4.2 §3.1 動態 argv 被 scalar input gate 錯誤拒絕

證據 run：`workspace/hufu-pilot-integration-run-20260808T101939Z/`

worker 正確完成前六個 bash call，但第七個：

```text
sha256sum /home/ubuntu/bin/pilot /home/ubuntu/bin/trec
```

被 Hufu 回覆：

```text
closed tool sequence input violation at position 7 of 8; do not call it
```

原因是 coordinator 同時設定了 `tool_input_value_sequence`，並把 `<pilot-path> <trec-path>` 當成靜態 placeholder。這和「第 7 個 argv 必須由同一 worker 第 5/6 個 stdout 動態複製」互相衝突：Hufu 在 worker 執行前已凍結值，所以合法的 resolved path 永遠不會等於 placeholder。

已做的通用修正：

- 動態 argv checkpoint 不再設定 `tool_input_field` 或 `tool_input_value_sequence`。
- 保留 closed `tool_sequence`，由 worker-side copy rule 驗證來源與順序。
- §3.1 隨後成功完成七個 call，證明這個修正有效。

### 4.3 `team_info(task_result)` 的 result lookup / evidence publication 問題

證據 run：`workspace/hufu-pilot-integration-run-20260808T102554Z/`

§3.1 runner 已顯示 done，且 transcript/task file 已產生；audit worker 立即呼叫：

```text
team_info(action=task_result, agent=poc-step-runner, ...)
```

卻收到：

```text
Agent "poc-step-runner" has task records but none are completed yet.
```

之後 coordinator 用 `ls`/`view` 又能看到同一份 done task file。最初把它判定成
durable Markdown 發布晚於 completion event 的 race；2026-08-08 事故後再比對 source
與保存下來的 task file，確認這個判定**不完整**，至少有三個可獨立觸發的問題：

1. `task_result` 以 `agent + task_contains` 的人類文字做識別。保存下來的 task description
   含 literal `\"`，JSON decode 後的 selector 則是 `"`；目前
   `taskDescriptionMatches` 只做 `strings.Contains`，所以 task 已完成也可能回「none are
   completed」。這一輪不能只憑該訊息證明 publication race。
2. 後續不帶 selector 的 `task_result` 已成功找到 §3.1 task，卻只回 durable Markdown
   裡的模型摘要，沒有 `VERBATIM TRANSCRIPT CAPTURED` manifest。這證明即使 publication
   完成，auditor 仍拿不到權威 raw evidence。
3. `handleTaskResult` 現在先讀 durable Markdown，只有找不到 done file 時才走 in-memory
   `TypedResult` fallback；因此有檔案但內容不是 verbatim manifest 時，反而遮蔽了帶
   `RawOutputRef` 的 typed result。

已做的通用修正：在 Hufu `team_info(task_result)` 加入 in-memory fallback：若 TodoList
已是 `done` 且有 typed result/raw transcript reference，即使 task Markdown 尚未發布，
也直接返回該結果與 manifest。並新增 regression test；完整測試與 lint 均通過。

**修正狀態：部分完成。** 目前 test 只直接測 `inMemoryCompletedTaskResult` helper；尚未
覆蓋 `handleTaskResult` 的 durable-file-first 分支、selector escaping、或 completion 與
transcript sealing 的整條 integration path。這個 fallback 可保留作相容層，但不能當作
最終一致性模型。

### 4.4 最後一輪的 reader contract mismatch

證據 run：`workspace/hufu-pilot-integration-run-20260808T103448Z/`

目前 team-owned contract（[team.yaml](.agent-teams/hufu-pilot-integration/team.yaml) 與 [poc-contract-reader.md](.agent-teams/hufu-pilot-integration/poc-contract-reader.md)）要求 reader：

- 不讀 source
- 不呼叫工具
- 只做一次 `submit_result`

但 coordinator prompt [coordinator.md:71](.agent-teams/hufu-pilot-integration/coordinator.md:71)–[coordinator.md:103](.agent-teams/hufu-pilot-integration/coordinator.md:103) 仍要求 reader：

- 讀完整 runbook/driver skill
- 回報 numbered checkpoints
- 使用 22 次 `view` 加一次 `submit_result`

因此 worker 按 team-owned contract 做了 0 次 `view`，直接提交 acknowledgement；coordinator 再按舊 prompt 判定「22 次 view 缺失」及「結果與任務矛盾」，把 run 終止為 evidence-integrity failure。

這是目前尚未修完的真正問題：**同一個 worker 有兩份互相衝突的 contract，且 coordinator prompt 與 static binding 的權威順序沒有統一。**

## 5. 影響評估

### 對 Pilot

- 沒有觀察到 Pilot executable 問題。
- 沒有發生因 Pilot non-zero 而停止的 run。
- 最後一輪沒有進入 deployment、reconcile、inventory generate 或 TREC mutation。
- VM topology 仍維持先前已存在的 running 狀態；本次最後一輪沒有新增 mutation。

### 對 Hufu

- closed sequence enforcement 本身有價值，確實阻止了錯誤重試與越權工具呼叫。
- 但 task contract 的 static binding、coordinator prose、worker file 三者沒有單一 source of truth。
- durable Markdown 不能作為 typed completion/evidence 的權威資料源；in-memory fallback
  只修正「done file 尚未可讀」的窄分支，尚未解決 selector 與 manifest 遺失問題。

### 對驗證結果

- candidate freeze：有一份成功 evidence。
- topology state probe：成功。
- 完整 minimal-poc revalidation：**未完成、不可宣稱 PASS**。

## 6. 已修改內容

### Team contract

- [team.yaml](.agent-teams/hufu-pilot-integration/team.yaml)：加入 initial contract binding、closed sequence、reader/state probe static contract。
- [coordinator.md](.agent-teams/hufu-pilot-integration/coordinator.md)：加入 dynamic argv 不使用 scalar pinning 的通用規則，並縮短 §3.3 task contract。
- [poc-contract-reader.md](.agent-teams/hufu-pilot-integration/poc-contract-reader.md)：改為 context-only acknowledgement contract。

### Hufu runtime

- [coordinator_tool_teaminfo.go](/home/ubuntu/nfs/github/agent-team-cli/internal/team/coordinator_tool_teaminfo.go)：`team_info(task_result)` 的 in-memory completed-result fallback。
- [coordinator_tool_teaminfo_test.go](/home/ubuntu/nfs/github/agent-team-cli/internal/team/coordinator_tool_teaminfo_test.go)：task-file publication race regression test（檔案原本在 dirty worktree 中為未追蹤狀態，未移除其他既有內容）。

所有 Hufu source 修正都保持 provider/integration independent，沒有寫死 Pilot API 或 Pilot binary 行為。

## 7. 建議的後續修復順序（未執行）

由於已達重試上限，本報告不再執行新 run。若要日後恢復，應先：

1. 先把 reader 暫時固定為 team YAML 的 one-submit contract，刪除 coordinator 的
   22-view task contract；這與現有 `poc-contract-reader.md` 一致，且可在不讀 source 的
   情況下只承認 force-loaded contract。若工作流確實需要 source-derived checkpoint，
   應另建有 view contract 的 worker，不要讓同一 worker 同時代表 acknowledgement 與
   source reader。
2. 修正 `task_result` 的權威資料源與識別方式（§9.2），再恢復 evidence auditor；否則
   runner 成功後 auditor 仍可能因拿不到 manifest 而 BLOCKED。
3. 把 §3.1 的動態 argv 與 §3.3 的十步 workflow 移到 structured execution/dataflow
   contract（§9.3），不要再由模型記住前一步 stdout 或充當 workflow state machine。
4. 加入 team contract compile/validate gate（§9.1）；任何衝突在建立 TODO、啟動 worker、
   消耗 retry budget 前 fail closed。
5. 對下列 acceptance matrix 全部通過後，才用全新 run workspace 重跑。不要把本報告中
   成功的 §3.1/§3.2 結果當作新 tree 的完整 evidence。
6. 只有所有 checkpoint、audit、verify、rollback/idempotency 完成後，才能把 runbook
   verdict 標為 PASS。

## 8. 證據索引

- §3.3 context-switch failure：`workspace/hufu-pilot-integration-run-20260808T100538Z/hufu-pilot-integration/logs/`
- §3.1 dynamic scalar-gate failure：`workspace/hufu-pilot-integration-run-20260808T101939Z/hufu-pilot-integration/logs/`
- §3.1 success but audit result lookup/evidence publication failure：`workspace/hufu-pilot-integration-run-20260808T102554Z/hufu-pilot-integration/logs/`
- 最後 reader contract mismatch：`workspace/hufu-pilot-integration-run-20260808T103448Z/hufu-pilot-integration/logs/`
- Hufu source tests/lint/build output：本次執行終端 transcript（Hufu repo `/home/ubuntu/nfs/github/agent-team-cli`）

## 9. Hufu 應用程式改善設計

以下是根據事故 artifacts 與 2026-08-08 當下 Hufu source 的改善方案；尚未實作或
integration-run 的項目一律視為 design proposal，不是已驗證修復。

### 9.1 P0：把 effective task contract 變成單一、不可變的 source of truth

目前 `bindInitialTaskContracts` 只把 coordinator 提交的 `Execution` / `OutputMode` 靜默
替換成 `team.yaml` 的值，卻保留原本 `Goal`；worker markdown/system prompt 又可描述另一套
行為。這正是 reader 同時收到 one-submit 與 22-view 語意的原因。

建議新增 compile 階段，先產生 immutable `EffectiveTaskContract`，至少包含：

- `contract_id`、schema revision、canonical hash、來源檔與 agent 名稱；
- execution kind、structured steps 或 closed tool sequence、input constraints、output mode、
  result/evidence contract、side-effect boundary；
- coordinator 只可提供 contract 宣告允許的 parameters，不可再提交另一份 execution contract；
- worker context、TODO、task journal、execution event、evidence manifest 全部記錄同一個
  contract ID/hash。

當 `bind-contracts: true` 時，以下情況應在任何 TODO/model call 之前拒絕，而不是 silent
override：initial agent 沒有 contract、同 agent 有多份 contract、coordinator 同時提交
不同 execution/output contract、contract 引用不存在、closed sequence 的工具未授權、
input sequence 長度不符、或 output/evidence mode 無法滿足 downstream dependency。

建議提供 `hufu team validate --team <name>`，並讓正常 team load 也強制跑同一個 validator。
validator 只檢查結構化欄位；不要嘗試用字串規則理解任意 prose。最重要的做法是停止在
coordinator/worker prose 重複機器契約，改成引用 contract ID，再由 Hufu 產生一段唯讀的
effective-contract 摘要給模型。

### 9.2 P0：以 task ID + typed result 取代 agent + 描述字串 + Markdown projection

`team_info(task_result)` 應接受 dispatch 時回傳的 stable `task_id`，auditor 以 task ID
查詢；`agent + task_contains` 只保留為相容介面。相容介面遇到零筆或多筆時應回傳候選
task ID，不要用模糊的「none are completed yet」混合「尚未完成」「selector 不符」與
「publication 未完成」三種狀態。

完成資料的權威順序應改成：

1. sealed typed `TaskResult` / event-store record；
2. task journal 的同一份 typed projection；
3. Markdown 只作人類閱讀 projection，不再是 API lookup source。

對 `output_mode=verbatim`，只有在 transcript 已 close、hash/bytes 已計算、
`RawOutputRef` 已寫入 typed result 後才可發布 terminal `done`。`task_result(task_id)` 不論
在 completion event 後立即呼叫、task Markdown 延遲/缺失、或 process restart 後呼叫，
都必須回同一個 manifest path/hash/bytes。若 sealing/persistence 失敗，task 應是明確的
`evidence_publication_failed`，不能先顯示 done。

現有 in-memory fallback 可留作 migration fallback，但 `handleTaskResult` 不應先以 Markdown
短路 typed result。Task description 的 `\"`、Unicode、換行與截斷也不應再影響身份識別。

### 9.3 P0：動態資料流用 typed references，不交給模型複製 stdout

§3.1 暫時取消 scalar pinning 是正確的止血，但「由同一 worker 把 call 5/6 stdout 複製到
call 7」仍只是 prompt rule，runtime 無法證明 provenance。Hufu 已有 structured execution
的 `ExecutionStep.Outputs` / `References` 模型，應把它完成為通用 dataflow：

- `which pilot` 與 `which trec` 各輸出 schema=`absolute_path` 的 fact；
- `sha256sum` 的 argv 只能由兩個 upstream fact reference resolve；
- receipt 記錄 resolved reference 的 producer step ID、output hash 與 consumer input；
- coordinator/worker 不能覆寫 resolved field，替換任一路徑都在 provider call 前被拒絕。

同樣地，§3.3 的十個 slot 應由 runtime 的 step DAG 執行 `produce → validate → mutate →
produce → validate → mutate → verify`；模型只負責產出受 schema 約束的 DSL 內容或在允許的
validation repair budget 內修正，不負責記住 slot counter、secret reference 與前一步輸出。
這能直接消除超長 prompt 導致的舊脈絡切換，也讓 mutation 前置條件可由 runtime 強制。

### 9.4 P1：讓 downstream evidence dependency 成為型別化依賴

auditor 不應從一段自然語言結果抽出 manifest path。Task contract 應能宣告：

- `depends_on_task_id`；
- `consumes: raw_transcript_manifest`；
- required producer status/contract hash；
- 可讀 evidence scope，以及禁止重跑 producer 的 replay policy。

Scheduler 只有在 producer terminal success 且 evidence sealed 後才啟動 auditor，並直接把
typed `ArtifactRef` 注入 auditor 的 `view` step。這同時移除 task selector、路徑猜測、
filesystem archaeology 與 publication timing 的耦合。

### 9.5 P1：改善錯誤分類與 retry budget

preflight contract conflict、selector mismatch、evidence publication failure、worker protocol
violation、provider/target command failure 應有不同 machine-readable code。尤其 config/compile
錯誤必須在 worker 啟動前發生，不應消耗使用者的 repair/retry 次數；同一 contract hash 的
deterministic failure 也不應靠重新問模型十次來修復。

每次 run 的 terminal summary 應列出：failure code、effective contract ID/hash、失敗 phase、
是否曾開始 mutation、最後 sealed evidence ID，以及 retry budget 被哪些 failure class 消耗。

### 9.6 驗收矩陣（實作完成前不可宣稱已解決）

| 測試 | 必須證明的行為 |
|---|---|
| static contract 與 dispatch contract 衝突 | worker/TODO 建立前 fail closed，錯誤列出 contract ID 與衝突欄位 |
| initial agent contract 缺失或重複 | team validation 失敗，不呼叫模型 |
| one-submit reader | effective contract、worker tools、實際 transcript 都只有一次 `submit_result` |
| task description 含 `\"`、Unicode、換行 | 以 task ID 仍精確取得同一結果；legacy selector 回明確 zero/ambiguous diagnosis |
| task Markdown 延遲、遺失或已有摘要 | `task_result(task_id)` 始終回 sealed typed result；verbatim task 始終含相同 manifest |
| completion/restart | `done` 前 transcript 已 sealed；restart 後 manifest hash/bytes 不變 |
| dynamic argv | consumer 只能使用 producer 的 typed path refs；手動替換值在執行前被拒絕 |
| downstream audit | scheduler 直接注入 producer `ArtifactRef`，不做字串解析或路徑探索 |
| §3.3 synthetic context-switch | 模型輸出舊 checkpoint 內容時 runtime 不執行越序 slot，且不開始 mutation |
| failure accounting | config/preflight failure 不消耗 execution retry；target non-zero 依 contract terminal |

最低 integration test 應走完整 `LoadTeam → ExecuteTasks → transcript seal → done event →
team_info(task_id) → process restart/replay → team_info(task_id)` 路徑，不能只直接呼叫 helper。
之後再跑一次不接 Pilot 的 synthetic provider test，最後才在新的 disposable Pilot topology
做 end-to-end revalidation。

## 最終判定

**BLOCKED / INCOMPLETE。** 主要阻塞是 Hufu/team contract 一致性，不是 Pilot。已完成的通用 Hufu 修正經過單元測試、lint 與 build 驗證，但 reader contract mismatch 尚未在重試上限內修復並重新驗證，因此不能把這次工作視為成功交付。
