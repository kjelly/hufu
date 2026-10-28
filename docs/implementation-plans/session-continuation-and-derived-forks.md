# Hufu Runtime：安全接續、歷史結果交付與新任務 Fork 實作規格

> Status: active
> Authority: reference
> Verified-Commit: 4e13abfb91a1328ccc1c58d0fd4a7861b17d6d43
> Supersedes: —
> Superseded-By: —

> **Implementation status**：WP0–WP2 已提交；WP6 本階段完成驗證；WP3–WP5 與目標結束時的完整整合／race gate 尚未完成。階段證據見第 12–16 節。
> **Repository**：`kjelly/hufu`
> **Reviewed baseline**：`4e13abfb91a1328ccc1c58d0fd4a7861b17d6d43`
> **Revised**：2026-10-28
> **Evidence**：原始查核為靜態閱讀；其後 WP0–WP2 已執行本地 fake-provider 測試，命令與限制見第 12–14 節。未執行真實模型。
> **Execution boundary**：所有開發與驗收使用本地 fixture、fake provider、暫存 managed workspace。使用者已授權將原 `docs/tmp/spec.md` 移入本正式計畫，並逐階段驗證及提交。

## 0. 執行範圍與工作方式

本規格只保留 coding agent 可完成的程式、測試與文件工作。不包含正式環境操作、使用者歷史資料遷移、真實 provider 呼叫、部署、發布或 PR 合併；以上不是完成條件。

1. 先讀根目錄與受影響路徑的 `AGENTS.md`，檢查 `git status --short`、`git rev-parse HEAD` 與 baseline 差異，保留他人變更。
2. 閱讀 [execution runtime](../architecture/execution-runtime.md)、[workspace versioning](../architecture/workspace-versioning.md)、[historical results](../guides/historical-results.md)、[result collection recovery](../implementation-plans/failed-result-collection-recovery.md)、[lineage retrieval draft](../architecture/hybrid-retrieval-lineage-scope.md)。修改 runtime 或 Go 時使用適用的 repository skills。
3. WP0 先重現 R-01／R-02。HEAD 若已修正，記錄 `already_fixed` 與測試證據；不能重現時記錄阻擋點並保留行為測試，不為推測重構。
4. 每個 WP 形成可獨立審查的變更單位；本規格不要求建立、發布或合併遠端 PR。核心狀態修改與重量級驗證串行執行。
5. 重用 EventStore、SessionTree、WorkspaceSessionOps、現有 recovery assessor、Historical Results、ContextCompiler 和 ContextInjectionManifest；不另建 canonical history 或恢復引擎。
6. 以下為本輪確定的功能契約。型別名稱可依 repo 慣例調整；授權、持久化與驗收語意不可省略。只有 fixture 證據與實際測試通過才能宣稱完成。

## 1. 本輪交付與效益

| 交付 | 效益 | 邊界 |
|---|---|---|
| Version-aware Resume | 防止 Session 與實際檔案屬於不同 Branch | 修正入口整合，保留現有 recovery policy |
| Canonical historical reads | 防止讀到 sibling／fork 後來源或把 Markdown 當權威 | 沿用 branch-aware result projection |
| 指定 Worker 的歷史 Result 匯入 | 可追溯地重用舊成果，減少手動複製與重複研究 | 同 workspace、同 team、可見 lineage，明確接收者 |
| Derived new-task Fork | 保留失敗現場，建立全新任務而不重跑舊工作 | 僅 Linux managed + required + 安全 settled source |
| Recovery 診斷整合 | 清楚呈現可以採取的恢復動作 | 不新增恢復決策權威 |
| Ancestor memory pre-limit filtering | 大量資料時仍能召回合法祖先記憶 | 保留最終授權檢查與排序契約 |

本輪不實作並行 Fork 工作樹、per-attempt snapshots、跨 team／獨立 Session root 匯入、原始 transcript／artifact 全文匯入、metadata-only derived workflow、optional 歷史匯入或自動 relevance ranking。這些不是待辦、RFC 或 release gate。

## 2. 現況與缺口

| ID | 靜態依據 | 本輪處理 |
|---|---|---|
| R-01 | `cmd/hufu/resumecmd.go` 在 team setup 前呼叫 `ActivateResumeSource`；`internal/team/session_resume_source.go` 直接呼叫 metadata checkout，未經 WorkspaceSessionOps | WP0 重現跨分支 Session/files mismatch，WP1 統一版本感知入口 |
| R-02 | `coordinator_tool_teaminfo.go` 的 history 與 agent-result fallback 呼叫 `workspace.go:taskHistoryEntries`，按 team/agent 掃描 Markdown，沒有 branch cutoff 參數 | WP2 改為 canonical scoped reader |
| R-03 | `internal/inspect/results.go` 已有 ResultID、SourceEventID、SourceHash、TypedResult、branch-aware visibility；尚無指定 Worker 的歷史交付契約 | WP3 實作 explicit grant 與 context delivery |
| R-04 | `workflow_cycle.go:beginFollowUpWorkflow` 拒絕 FAILED；普通 fork 繼承原執行狀態 | WP4 增加 child-only 新任務邊界 |
| R-05 | `resume --run` help 已明確寫 membership constraint；不是時間旅行 | WP1 保留 help 語意，只補範例與測試 |
| R-06 | fork JSON 已有 snapshot ID、tree hash、materialized、metadata-only；event fork 使用可達的已確認 snapshot | WP1 補 snapshot event/capture 來源，不新增快照機制 |
| R-07 | `worker_memory.go` restricted ancestor 使用 100000 overfetch 再過濾；vector 路徑已有 pre-limit allowed-ID 授權 | WP6 修 exact／FTS 候選擠壓；屬召回完整性風險，不宣稱已有資料外洩 |

Interrupted resume、targeted retry/reconcile、result-only repair grants、completed workflow follow-up、canonical history query、required workspace checkout 已存在，均應重用。

## 3. 不變條件

- 執行生命週期以已驗證事件與既有受控交易為權威；不把 Markdown、cache 或模型文字當授權來源。
- 已接受 Result、Receipt、來源事件及原 Report 不重寫。新任務不能用舊 Task 的 `done` 抵充本次 acceptance。
- Child 只能讀 fork cutoff（含）之前的祖先事件及自己的合法事件；sibling、父分支後續事件、其他 root 不可見。
- 取得 team lease 後再取得 project lock；檔案 materialization 與 SQLite 交易透過既有 durable operation/recovery 串接，不假設兩者物理原子化。
- 同 branch Resume 保留既有 external-drift admission；跨 branch 不得把舊分支 live files 封存成目標分支 drift。
- 已完成 worker 不重跑；未知／部分外部副作用使用現有 reconcile／manual policy。新功能不能擴大 retry、repair grants 或 frozen target 權限。
- 歷史內容是 untrusted reference；不能變成 system instruction、工具授權或 fresh verification。Prompt fencing 與 redaction 不等於保證模型免疫注入，權限必須由 runtime 強制。
- 不使用自然語言關鍵字或正規表示式猜測讀寫權限、接收者或任務範圍。CLI 明確結構化選擇由 deterministic parser 驗證。
- 舊格式由新版讀取；新格式由舊版拒絕。授權與 projection 邊界不能依賴舊版忽略未知欄位。
- 安全修正 R-01／R-02 可以改變原先錯誤行為；其他未啟用新功能的合法操作保持語意相容。資料庫格式升級的 downgrade 限制依第 6 節揭露。

## 4. WP1：Resume activation 與快照來源

### 4.1 唯讀規劃與受鎖啟用

使用既有版本控制器抽取可共用入口；不得從 `team` import `cmd/hufu`。Plan 至少帶 workspace、source branch、run membership constraint、expected active branch/event head、mode、target snapshot ID/tree hash/capture event。

- Plan／result-repair dry-run 使用唯讀 store，不 migration、checkout、snapshot 或啟動 provider。
- Activate 在 team→project lock 內重新解析來源並驗證 head／generation；不信任先前 preview JSON。來源變更回報 `stale_activation_plan`，不自動換來源。
- required 跨分支先完成既有 recovery，再走 WorkspaceSessionOps checkout/materialization；成功後才載入可執行 Coordinator。project lock 必須交接給 run admission，不能以無鎖空窗暴露已切換狀態。
- required 缺快照、CAS integrity error、lock conflict 或未完成 operation 必須明確失敗或進入既有 forward recovery，不能 fallback live files。
- off／observe 保留原 metadata-only Resume 語意；不得宣稱檔案也已回復。
- 同 branch 不為了統一路徑重做 checkout 或 accepted worker；仍執行既有 admission/recovery。
- 保留 `--run` 的 membership 語意與既有 help，補「指定 event 應使用 session fork」範例。

已有錯誤型別可重用；新增 reason code 至少區分 stale plan、snapshot unavailable、project lock conflict、recovery required、source run not in lineage。JSON 擴充不得更改既有欄位語意。

### 4.2 快照揭露

沿用 fork／checkout JSON 既有欄位，補實際 snapshot capture event／reason，並在需要的 manifest/report 引用同一來源。Event E 的檔案來自 E 之前最近一次可達且已確認的 snapshot，不宣稱等於 E 瞬間的任意檔案狀態。metadata-only 時 snapshot 欄位留空，不能填 active live head 冒充來源。

## 5. WP2／WP3：歷史讀取與指定 Worker 交付

### 5.1 Canonical reader 與 package 邊界

`internal/inspect` 已 import `internal/team`。在 team 定義最小唯讀 interface/DTO，由 CLI 注入 inspect adapter；避免循環相依及第二套投影。

- `team_info task_history`、`task_result(agent)` fallback 均使用 consumer branch 的 canonical lineage；不再讀 Markdown fallback。
- `task_result(task_id)` 保留當前 active cycle typed lookup。歷史精確選取以完整 ResultID 為準，不接受模糊 prefix、任務描述或「最近一筆」替代。
- 只有 Markdown 的舊資料回報 `legacy_unverified`／無 canonical history；不供 Worker 當成果權威，不新增 legacy forensic command。
- 尊重既有 execution profile 對 historical task reuse 的限制；不得借新 adapter 繞過。Memory disable 保留本來對 memory 的語意，不自行擴張成禁止所有 explicit result reference。
- 歷史查詢仍可呈現 failed／invalidated 狀態並標註；WP3 匯入則只接受所選 lineage 下有效且已接受的 task result。失敗證據可由既有 operator inspect/results 查看，第一版不匯入 Worker。

### 5.2 CLI 與接收者

新增 repeatable flag，只在明確選定單一 team 的 `run` 新任務／正常 completed follow-up 使用：

```bash
# Proposed: agent 名稱依載入 team 的既有 resolver 正規化
hufu run --team <team> --workspace <workspace> \
  --inherit-result researcher=<full-result-id> \
  --inherit-result reviewer=<another-full-result-id> \
  -- "根據指定歷史成果執行新的驗證"
```

本輪使用 `agent=result_id`，不保留無接收者的 `--inherit-result <id>` 語法。

- 由 operator 明確授權具名 Worker；未知 agent、coordinator/orchestrator、空值、多 team、獨立 root、`--new` 或普通 resume/retry/repair 攜帶新 grant，均在模型呼叫前拒絕。
- source 只限同 managed control workspace、同 team、consumer branch 可見 lineage。ResultID 不是授權憑證。
- 同一 `(agent, result_id)` 去重並穩定排序。每 invocation 最多 16 組 grant；去重後所有來源 presentation 合計最多 1 MiB UTF-8 bytes；超出明確拒絕。
- Grant 不允許 wildcard，不自動交給 Coordinator、其他 agent、nested subagent、judge、skeptic 或輔助模型。Coordinator 只收到不含來源內容的 routing metadata。
- 指定 Worker 在該 workflow cycle 內建立的每個 task 都繼承其 grant；不推測哪個 task 語意相關。這一行為寫入 flag help。未被派工的 recipient 記為 `not_dispatched`，不要求為它額外建立 task。
- Direct-agent、multi-team、chat 等新入口整合不列入本輪；若共用 flag 註冊造成可見，應明確拒絕不支援的入口，不得靜默忽略。

### 5.3 凍結、交付與重啟

新增 schema-versioned grant envelope，至少記錄：consumer workspace/team/branch/cycle、canonical recipient identity、完整 ResultID、source session/run/task/attempt/event/hash、typed payload hash、presentation hash、policy fingerprint。

1. 新 request admission 在任何模型呼叫前驗證 scope、來源完整性、有效性、profile 與 disclosure policy，凍結 cycle grant。若 required source 缺內容或不合法，整個新請求拒絕。
2. Task 建立時由 runtime 將對應 grant 綁定 consumer task ID；attempt 派送前再驗證 lineage、來源有效性、policy、內容 digest，寫入 task/attempt request manifest。模型不可重建或覆寫上述身份與 hash。
3. Presentation 固定包含 redacted Summary 與完整 redacted TypedResult JSON，加上 source citation/validity 標籤；不匯入原始 Output、對話、transcript 或 artifact bytes。redaction 不修改原 canonical bytes；source hash、payload hash 與 presentation hash 分開記錄。
4. TypedResult 內 artifact locator 僅是資料，不產生新的讀取權限。無 typed payload、來源缺失或 hash mismatch 明確拒絕，不從 Markdown／LLM 補造。
5. 所有匯入都是 required。每次該 Worker 的 provider request、重試與合法 context compaction 後都必須保留完整 presentation；可按 source identity 去重，不能暗中截斷。若合併完整 system/tools/task context 後超出實際模型預算，在該次 provider dispatch 前拒絕。不是等到執行後才補驗證。
6. Worker fallback／extra-model 只在原 task 身份及 frozen execution policy 已允許且每個 target 通過 disclosure/budget 檢查時收到相同 grant。Nested subagent 不繼承；不能用父 task 的 grant 取得新 recipient 權限。
7. Resume 原 cycle 時從 durable grant 與 task binding 恢復，不接受 CLI 增補；來源已失效或 presentation 無法一致重建時拒絕派送，不用更新內容代替。新的 completed follow-up 不自動繼承上一 cycle 的 grant。
8. Manifest 記錄 actual recipient/task/attempt/request、source IDs/hashes、included、reason、bytes/tokens；能區分「獲准」「實際送出」「尚未派工」。沿用既有 ContextCompiler 與 manifest，不另存一份可變權威。

## 6. WP4：Derived new-task Fork 與格式相容性

### 6.1 第一版支援矩陣

```bash
# Proposed: 只建立／切換 child，下一次 run 才開始新任務
hufu session fork <branch-or-event> --name investigation --new-task \
  --workspace <workspace>
```

- 僅 Linux managed workspace、`workspace-versioning.mode: required`、可驗證並 materialize 的來源 snapshot 支援。
- off／observe／unmanaged／非支援平台／`--metadata-only` 與 `--new-task` 組合，在建立 branch 前拒絕。一般 stateful fork 保留原行為。
- 來源可為 DONE、FAILED 或 interrupted，但不得有 active writer、unknown/partial external effect、pending terminal commit、isolated world applying/applied 未 settle、workspace recovery marker。依既有 typed state/receipt 判斷；無法證明安全就拒絕，不以任務描述猜測。
- 拒絕時不執行 recovery side effect；提供既有 inspect/reconcile 指引。一般 workspace startup recovery 仍依既有契約執行，不為 derived mode 新增自動外部 reconcile。

### 6.2 Durable child boundary

- Fork 操作在既有 journal／交易內保存 child-only derivation intent、source branch/event、snapshot ID/capture event、schema version；crash 後與 branch 一起恢復，不能留下可被當成普通 stateful branch 的半成品。
- 尚未收到新 request 的 child 投影為 `awaiting_new_request`。拒絕 `resume` 舊工作；不得把繼承的 DONE 當成新任務已完成。
- 下一次同 team `run` 的非空 request 建立唯一 `derived_fork` cycle boundary。沿用 workflow_cycle_started 的版本化 envelope 或等價明確事件，但 replay 必須能辨識 child-only 意義。
- 第一次 request 的 input snapshot、acceptance、execution target 按新任務建立；舊 Task、retry state、phase results、continuation、result collection 和 workset 只保留為歷史，不進入 child 的可執行投影。原父結果／失敗狀態不修改。
- 不將父 branch 原始執行對話直接作新指令；沿用合法歷史檢索邊界，explicit full typed result delivery 只走 WP3。
- Task ID floor 以可見 lineage high-water 計算。Boundary 綁定 durable invocation identity；append 成功但 checkpoint 失敗時 replay 同一 cycle，不因重新啟動再消費 request。不同新 request 在未完成 cycle 上沿用既有拒絕／恢復規則，不把文字相同視為 idempotency key。
- child 第一次 cycle 之後恢復普通 follow-up 規則；FAILED parent 本身不能藉此清空工作狀態。
- session checkout/diff、results、inspect、compaction、memory、report 與 audit 都應正確識別新邊界；不得在重播時改寫原 report。

### 6.3 舊 binary 拒絕機制

基線 `internal/runtimestore/schema.go` 最高 migration 為 4；`store_test.go:TestOpenRejectsAnUnknownSchemaVersion` 已涵蓋 writable/read-only 對未來 schema 的拒絕。事件 reader 允許未知事件，**只增加 event 欄位或 event version 不足以阻止舊 binary**。

- 在首次引入 durable grant／derivation projection 的變更中新增下一個 runtime-store migration（以實作時 HEAD 的下一號為準），儲存必要的 projection/schema metadata，沿用 migration checksum verification。
- Schema migration 與 store 驗證必須先於新語意寫入；使用既有 writable-open migration 生命週期，不設第二個版本 registry，不刪除既有 migration rows。
- 新版 read-only reader 必須安全讀取原本支援的舊 schema，不因查詢偷偷 migration；新表缺少時視為無新功能記錄，而不是猜測 derived 狀態。
- **升級後曾以新版 writable-open migration 的整個 runtime.sqlite，舊 binary 均可能無法再讀寫，即使沒有使用新旗標。** 文件必須明確揭露，不聲稱只影響單一 child。
- 不實作 down-migration，不要求舊 binary 支援 export。診斷/export 使用新版既有唯讀介面；保留原舊格式 fixture 作為相容性證據。
- 以 baseline binary 操作新版 fixture 驗證拒絕；覆蓋 run/resume/session fork/checkout 與唯讀 results，確認拒絕前沒有事件、檔案或 provider 副作用。新版再驗證可正常讀寫。

這是本地格式與測試工作，不包含操作任何使用者資料庫或發布升級。

## 7. WP5：Recovery 診斷整合

- 沿用現有 recovery policy、ResultCollectionRecoveryAssessment、repair grants 和 acceptance 檢查；只做 typed presentation adapter，不另定 retry eligibility，也不新增通用恢復引擎。
- 在既有 inspect/status/JSON 合適位置呈現 source branch/run membership、snapshot/capture、recovery disposition、side-effect state、blocked reason、是否等待新 request、granted/delivered result IDs。資料不明以 unknown/unavailable 呈現，不編造零計數。
- 只有具備可重播來源的欄位才能輸出；不為診斷另追加重複決策事件，不重算歷史 report。
- `resume --repair-result --dry-run` 與 offline inspect/list/show 不啟動 provider、不 migration、不啟用 branch。
- **`results query` 保留既有 LLM 分析功能**：只讀來源，不啟動 worker 或修改 Session／歷史；允許既有 provider/proxy 呼叫。只有 `results query --preview` 保證零模型呼叫。測試不可把唯讀等同離線。
- 驗證既有 result-only repair 不重跑工具、不退款重複 grant、reconcile complete 仍驗 acceptance，未知／部分副作用仍拒絕盲目重試。

## 8. WP6：Ancestor memory 檢索完整性（獨立工作包）

WP6 只依賴 WP0 的基線，不依賴 WP3／WP4，也不阻擋 exact ResultID reader。

- 使用 immutable typed allowed-ID constraint，區分 unrestricted 與 restricted empty（後者必須零結果）；拒絕空 ID，按 opaque ID 去重，不改大小寫或 trim 身份。
- Exact 與 FTS 在排序／limit 前套用相同 constraint；沿用 draft 的參數化 JSON array + json_each join，避免 SQL 字串拼接與 SQLite bind variable 上限。
- Vector 保留已有 pre-limit 授權；HybridRetrieve 傳遞一致 constraint，在融合／hydration 與 Worker 輸出仍保留最終授權檢查。
- 在合法候選集合上保留既有 exact prefix、RRF、MMR、file boost、去重、tie-break 排序；移除 100000 workaround 前與不截斷的授權 oracle 比對。
- 固定 seed 測 1K、10K、100K、100001+ 候選；包含 fork cutoff 後候選擠壓、sibling、restricted empty、超過 bind-variable limit、stale vector ID 與 cancellation。
- 本地比較 baseline/new latency、allocations、候選數與相同 ordered IDs。預熱後串行重複取樣，記錄分布，不以單次 wall time 當功能斷言。1K/10K fixture 的 median latency 或 allocated bytes 若惡化超過 20%，先定位並修正再完成；100K+ 以完整性通過及資源預算為硬門檻，列實測成本。這是本地回歸門檻，不是 production SLO。
- 更新 lineage architecture 文件以描述已實作的結果與限制，不留下等待另行人工核准的 ADR／SLO 協議作為本工作完成條件。

## 9. 工作順序與驗收矩陣

| WP | 依賴 | 可審查交付 |
|---|---|---|
| WP0 | 無 | 現有入口／鎖／projection 對照，R-01/R-02 重現與既有 recovery 基線；測試按行為命名 |
| WP1 | WP0 | version-aware Resume、snapshot provenance、membership 文件／測試 |
| WP2 | WP0 | canonical historical adapter、移除 Worker Markdown fallback |
| WP3 | WP2 | schema compatibility gate、recipient grants、context delivery/replay |
| WP4 | WP1 + WP3 | required-only derived fork、child boundary、所有相關 projections |
| WP5 | WP1 + WP4 | 診斷 adapter 與 recovery 整合回歸 |
| WP6 | WP0 | pre-limit filtering、oracle tests、固定資料集 benchmark |

WP1／WP2 完成後才能修改 derived cycle。WP6 可獨立交付，但重量級驗證仍串行。

| 測試 | 必須觀察的結果 | WP |
|---|---|---|
| A/B 不同 snapshot，B active，resume A | branch/session/live files/materialized head 全為 A；不得把 B 記成 A drift | 0/1 |
| project lock conflict、stale plan、缺 snapshot、CAS corruption | activation/provider 前拒絕，或既有明確 recovery marker；不 silent fallback | 1 |
| materialize 中途及 session commit 前後 crash | 以 operation journal 唯一恢復；第二次操作不重複副作用 | 1 |
| same-branch resume、off/observe、repair preview | 保留原語意；preview 無 migration／provider／branch activation | 1/5 |
| sibling／fork 後 Markdown sentinel | team_info history/fallback 無未授權文字，current task lookup 正常 | 2 |
| 同 task ID 不同 run、legacy-only history | 完整 ResultID 精確匹配；legacy 不當 canonical result | 2/3 |
| 指定 Worker、其他 Worker、nested subagent | 只在被授權 recipient 的實際 request 出現來源；其他 request 無來源正文 | 3 |
| unknown recipient、cross-team/root/sibling、invalidated/hash mismatch | admission 前拒絕；零模型呼叫 | 3 |
| 16-grant/1-MiB 上限、模型 token overflow | 不截斷 required 內容；派送前拒絕；manifest/error 可診斷 | 3 |
| retry、fallback、compaction、restart、new follow-up | 同 cycle 凍結來源一致；未授權 target 不收到；新 cycle 不自動繼承 | 3 |
| 未派工 recipient、artifact locator、prompt injection | not_dispatched 可辨識；locator 不授權讀檔；工具政策不改 | 3 |
| old schema read-only、新 schema baseline binary | 新版讀舊資料不 migration；舊版拒絕新資料且無副作用 | 3/4 |
| FAILED/DONE/safe interrupted source derived fork | child 全新任務，父狀態/receipt/report 不變 | 4 |
| metadata-only、off/observe、unknown side effect、pending worlds | derived branch 建立前拒絕；一般 fork 行為不變 | 4 |
| awaiting request、boundary append 後 crash、第二個 request | 不恢復父任務、不重複 boundary、正常 cycle lifecycle | 4 |
| checkout parent/child、diff、results、memory、compaction | 每個 reader/projector 都尊重 boundary/cutoff，task IDs 不碰撞 | 4 |
| accepted result、repair grant 重送、reconcile failure | accepted worker 不重跑；grant 不疊加、不退款；驗收仍執行 | 5 |
| results query 與 query --preview | query 可用 fake LLM 且歷史唯讀；preview 零模型呼叫 | 5 |
| 100001+ 候選、empty restriction、跨 branch、vector stale ID | 合法結果與 oracle 相同；未授權永不輸出；排序與資源 gate 通過 | 6 |
| concurrent fork/checkout/resume 與 read-only query | writer 排他、reader 一致；沒有重複 worker 副作用 | 1/4/5 |

### 9.1 整合驗收

使用真實 CLI binary + 本地 fake provider + temporary managed workspace：建立 accepted A 和安全失敗 B；驗證 resume 不重跑 A；從明確 event derived fork；下一次 run 以 `worker=A-result-id` grant 交付 A；新的 task 產生自己的 typed result 與 acceptance；關閉／重開 store 後比較 branch、cycle、task、source hashes、manifest、snapshot、receipt、父 report。

Negative fixtures 必須覆蓋來源在 admission 後／dispatch 前失效、cutoff 不合法、budget overflow、materialize crash、terminal commit 不確定與兩進程競態。對不確定 commit 使用原有 durable idempotency/reconciliation，不以重新執行 worker 當恢復方法。

## 10. 驗證與資源規則

- 全部 Go/lint 使用 `mise`，`GOTOOLCHAIN=local`、`GOMAXPROCS=2`、Go `-p 2`；保留既有 GOTMPDIR/GOCACHE/GOMODCACHE。
- 一個 `mktemp -d /tmp/codex-task.XXXXXX` 私有 task 目錄；測試 subprocess 的 TMPDIR 指向它，跨呼叫記錄 cleanup owner。baseline binary／current binary 各至多一份，無消費者的 build 用 `/dev/null`。
- 開始及階段間檢查 `df -h /tmp`、`free -h`，以及 configured Go cache/temp 所在檔案系統空間。task retained scratch 預設 512 MiB，保留至少 1 GiB 且 25% /tmp、2 GiB available RAM；大型 fixture 分批建立並清除，不複製到磁碟逃避預算。
- baseline compatibility fixture/worktree 放 task 目錄，排除無關大型檔案；若單一必要 binary 超過預設預算，先確認總 reserve。不要覆蓋現有 checkout 或啟動真實模型。
- Socket/PTY fixtures 遵循 AGENTS.md 的短路徑 helper 與 cleanup；新測試不得留下資料或背景程序。
- 只在行為變更／失敗／未解疑慮時重跑相關測試；最終完整驗證仍須通過。100K+ fixture 與 race/lint 串行，不能以 skip 避開失敗。

實作時的 gate（從 repo root 執行，環境套用上述限制）：

```bash
mise exec -- go test -p 2 ./internal/team ./internal/inspect ./internal/context ./internal/runtimestore ./cmd/hufu
mise exec -- go test -timeout 30m -p 2 ./...
mise exec -- go test -timeout 60m -race -p 2 ./internal/team ./internal/inspect ./internal/context ./internal/runtimestore ./cmd/hufu
mise run lint
bin/check-docs
git diff --check
```

Focused tests 依實際行為名稱選取並確認有執行測試，不以 WP 編號命名。WP6 benchmark 與 baseline compatibility harness 由 coding agent 加入可重現命令，記錄 fixture seed、資料量、工具鏈、exit code、資源與結果。

本文件位於正式 implementation-plans，須通過 `bin/check-docs` 與 `git diff --check`；另外檢查工作依賴、CLI 契約與驗收一致性。

## 11. 完成條件

- WP0–WP6 各有具體變更／already_fixed 證據；R-01 未重現時須記錄阻擋路徑，不能宣稱已修 production incident。
- 第 9 節測試均有實際結果；不支援平台／模式須有拒絕測試，不以 non-applicability 跳過安全邊界。
- 原已接受結果、Receipt、父 report、frozen policy、repair grants 保持有效；沒有錯誤分支讀寫、重複 worker side effect 或授權擴張。
- 新旗標的語法、recipient 範圍、required-only derived 限制、schema 升級與不能降版的影響，寫入既有 reference/guide；維持 team-neutral runtime，檢查 `git diff -- cmd/hufu internal` 無特定 team 分支。
- lint、相關測試、完整測試、race、docs checks 通過；記錄命令、exit code 與限制。完成後移除 task-owned 暫存產物。
- 可先交付 WP0/1/2，再 WP3/4/5；WP6 獨立交付。只宣稱實際完成的工作包，不要求正式發布或 production canary。

所有保留事項都能以 repo 程式碼與本地 fixture 完成。目前沒有需要使用者提供憑證、正式資料或操作外部系統的前置工作。

## 12. WP0 實作紀錄

### 範圍與入口

- 原始 baseline：`4e13abfb91a1328ccc1c58d0fd4a7861b17d6d43`，使用 `git archive` 匯出 immutable snapshot；未把既有未提交的診斷／result-collection 修改混入 baseline。
- 普通 Resume：resolveExistingExecutionWorkspace → ReadResumeSource → ActivateResumeSource → loadTeamCommon → bindRunWorkspaceVersioning → Coordinator。activation 目前早於 static gate 與 project lock。
- required session checkout：openVersionedSession → team lock → project lock → workspaceOps/recovery → WorkspaceSessionOps.Checkout → materialization → canonical session rebuild。
- 明確 `--workspace` 的 resolver 先走 unmanaged adapter；WP1 必須辨識已註冊 control root 並保持 team/project lease 到 run 結束。
- task_history 與 task_result(agent) fallback 走 taskHistoryEntries；task_result(task_id) 走 current TodoList typed result。WP2 保留 current typed lookup，只更換未授權歷史來源。
- ordinary stateful fork 保留舊投影；completed follow-up 經 workflow_cycle_started；FAILED 不允許普通 follow-up。這些是 WP4 的相容基線。

### 特徵測試

- `TestResumeActivationMetadataLeavesMaterializedBranchUnchanged`：建立 required A/B snapshot，對 A 做 metadata activation，觀察 active branch=A、live files/materialized branch=B。WP1 落地時以正確行為測試替換。
- `TestTeamInfoHistoricalMarkdownIsNotBoundToBranch`：寫入不含 canonical branch binding 的 sentinel Markdown，觀察兩個 history/fallback action 可讀取它。WP2 落地時以 cutoff／sibling negative tests 替換。

### 驗證紀錄

初次 immutable baseline 全測因 sandbox 禁止 local listener，觸發 httptest socket 權限錯誤；確認程序身份後終止該輪（exit 143），不把它當成完整 baseline 結果。允許本地 fixture listener 後，以同一 immutable archive 重跑 `mise exec -- go -C <archive> test -p 2 ./...`。完整執行 exit 1；唯一失敗 package 為 `cmd/hufu`（141.080s），唯一失敗 case 為 `TestResumeRecoveryPreviewThenExecute/direct`：重新載入時 execution-policy snapshot drift。`internal/team` 通過（597.409s），其餘 package 均通過或無測試。未修改 baseline 以消除失敗。本工作樹特徵測試與 lint 結果另列。

- `mise exec -- go test -p 2 ./internal/team -run 'Test(ResumeActivationMetadataLeavesMaterializedBranchUnchanged|TeamInfoHistoricalMarkdownIsNotBoundToBranch)$' -count=1`：exit 0，4.377s，兩項特徵均重現。branch ID 依既有規則小寫正規化，fixture 使用 `b`。

由於 baseline 的 team suite 已接近預設 10 分鐘，後續完整 test 使用 `-timeout 30m`，race 使用 `-timeout 60m`；單項協定 deadline、fixture readiness 與失敗斷言維持不變。

- WP0 `mise run lint`：exit 0，0 issues；`bin/check-docs`：exit 0（88 active files／561 relative links）；`git diff --check`：exit 0。

## 13. WP1 實作紀錄

- `resume` 明確指定已註冊 control root 時，恢復 managed scope 與 team lease。靜態 startup gate 後取得 project lock，再做 version-aware activation；lock 交由原 team context 持有到 Close。required mode floor 寫入也移入 project lock。
- ResumeSource 凍結 workspace、active branch、event head ID/hash。activation 前重新驗證；required 跨 branch 走現有 WorkspaceSessionOps.Checkout，同 branch 保留 live checkpoint，off/observe 保留 metadata-only。
- materialization 後 crash 由原 operation journal forward complete；若 recovery 改變原 selection，回報 stale_activation_plan，重新選取後再接續，不重複 capture 或 worker。缺目標 snapshot 與取消均拒絕。
- fork/checkout 輸出區分 snapshot publication 與 capture origin；沿 fork parent 找原 capture，restore v1 缺 source identity 時明確 unavailable。未新增快照時機或歷史資料回填。
- WP0 Resume 特徵測試已替換成行為測試；歷史 Markdown 特徵仍保留至 WP2。
- 定向測試第二輪 exit 0：team 13.934s、CLI 29.619s，覆蓋新 activation、snapshot provenance、公開 fork/checkout、static gate、existing repair preview/execute。後續新增取消與 event-hash 檢查，驗證結果於下列補錄。

- WP1 工作樹完整 CLI：`mise exec -- go test -timeout 30m -p 2 ./cmd/hufu -count=1`，exit 0，130.781s；包含 baseline 原失敗的 recovery case。目前工作樹仍包含本任務以外的既有未提交修改，因此不將此結果稱為 immutable commit 全綠證據。
- 公開 CLI fake-provider E2E：`TestResumeCommandActivatesVersionedBranchBeforeProvider`，exit 0，2.405s。先實際產生 accepted worker／frozen inputs／receipt，再切到 B；resume A 的每次 provider request 均確認 canonical branch、live files、materialized head 為 A，未重跑原 worker。fixture 必須提供原始輸入、runtime collector JSON 與明確 acceptance，不弱化 admission gate。
- 定向 race：`mise exec -- go test -timeout 30m -race -p 2 ./internal/team ./cmd/hufu -run 'Test(VersionedResumeActivation|ManagedResumeBindingAndActivation|WorkspaceSnapshotProvenance|ResumeCommandActivatesVersionedBranchBeforeProvider|CLISessionWorkspaceVersioningEndToEnd|LoadTeamCommonStaticFailureRunsBeforeWorkspaceVersioning|ResumeRecoveryPreview)' -count=1`，exit 0；team 9.280s、CLI 104.385s，無 race 報告。
- `bin/check-docs`、`git diff --check` 通過；另驗證 baseline archive 3,247 個 tracked blobs 與原 SHA 完全相同。所有 WP1 驗證均使用本地 fixtures。全 repository test／完整受影響 package race 的最終 gate 保留到整體實作完成，不以本定向結果宣稱全計畫完成。

- 最終 activation plan：在 project lock 前唯讀記錄目標 snapshot ID/tree hash、公佈／capture event、branch-head generation 與 materialized subject identity；鎖內及 forward recovery 後重驗，不信任舊 preview。缺 snapshot 只記為不可用，不 fallback live files。已有跨 workspace 或 active branch/materialized branch 不一致而無可恢復 journal 時回報 recovery_required，不將別人的檔案 capture 成目前分支。metadata helper 已改為私有，只供非 required 的共同 activation controller 呼叫。
- 最終定向 race（在上述 plan／ownership 修正後）：沿用前述命令，另包含 `ResumeActivationPlan`、`ResumeActivationRefuses` 與原 canonical source lookup；exit 0，team 10.804s、CLI 96.707s。增加 readonly plan、相同 snapshot/event 但 generation 已變、舊版留下的 metadata/files mismatch，以及 foreign materialized workspace 的拒絕測試。

- 最終 `mise exec -- go vet -p 2 ./...`：exit 0；`bin/check-docs`：exit 0。最終 `mise run lint`：exit 0，0 issues。WP1 可提交；WP2–WP6 與最終完整 gate 仍待完成。

## 14. WP2 實作紀錄

- 在 team 定義 runtime-owned HistoricalTaskScope／HistoricalTaskReader／detached result DTO，由 CLI 注入 inspect.LoadHistoricalResults adapter，沒有 team→inspect import cycle 或第二個結果索引。
- Scope 取 canonical control root、正規化 team ID 與綁定 event journal 的 branch；不使用失敗時 fallback main 的動態 tree helper。Extra-model clone 沿用同一 reader 與 parent control root。
- team_info history／agent fallback 不再掃描 Markdown；保留 current task typed lookup，新增完整 result_id，拒絕模糊 prefix、衝突 selector 與 model-supplied workspace/branch。歷史輸出以 fenced JSON 標為 reference，invalidated 只能明確 ID 閱讀，不能進 accepted agent fallback。
- Source projector 額外輸出 source_branch_id；保留既有 branches 的 visibility 語意。沒有重寫 canonical bytes、receipt 或舊 Markdown。
- WP0 Markdown characterization 已改為拒絕未驗證 Markdown 的行為測試。第一輪 targeted tests exit 0：team 1.122s、inspect 0.996s、CLI 12.303s；涵蓋 fork cutoff、sibling／other-team／Markdown sentinels、精確 ID、歧義、current typed lookup、profile gate 及 clone scope。後續補充 scope override、fencing、team casing 與 restored selector 測試，最終結果另列。

- 最終定向 race：`mise exec -- go test -timeout 30m -race -p 2 ./internal/team ./internal/inspect ./cmd/hufu -run 'Test(TeamInfo|TaskResultByID|InMemoryCompleted|CompletedTaskResult|CloneCoordinatorRetainsCanonicalHistoricalScope|CanonicalHistoricalTaskReader|HistoricalResult)' -count=1`，exit 0；team 15.247s、inspect 13.656s、CLI 40.313s。測試亦覆蓋 scope override 拒絕、team casing、restored-result profile、完整 Result ID 與 untrusted fence。

- WP2 `mise exec -- go vet -p 2 ./...`、`bin/check-docs`、`git diff --check`：exit 0；`mise run lint`：exit 0，0 issues。本階段可提交；WP3–WP6 與最終完整 gate 尚待完成。

## 15. 新 baseline 與 CLI fixture 隔離

`825ff686` 的隔離完整測試結果與 3,263 個 tracked blobs 完整性查核見
[baseline 報告](../reference/runtime-continuation-baseline.md)。唯一失敗是全域 Cobra
help 旗標污染 CLI fixture；原版本已用獨立回歸測試重現，working-tree 的 fixture
修正與原 versioned-resume 測試均已通過 targeted／完整 CLI 及定向 race 驗證。
本工作樹完整測試、vet 與 lint 均已通過（lint 0 issues）；immutable baseline 的失敗紀錄不改寫。
WP6 是第 8 節允許的獨立工作包，將以獨立階段提交；WP3–WP5 未完成。

## 16. WP6 實作與驗證

- 重用 `SearchRequest.AllowedItemIDs`，保留 nil／restricted-empty、opaque ID 的大小寫與空白；拒絕空 ID，不新增第二種 constraint 或 history store。
- Exact／FTS／canonical iterator 以一個 JSON array parameter 在 limit 前授權。明確 ID 的 canonical 查詢使用既有 PK，FTS 在 outer cursor 先過濾 ID，仍保留所有 scope／lifecycle／validity／kind／confidence 檢查。
- Hybrid 為每個 source 複製凍結清單，融合前再檢查，並沿用可取消的 MMR。Worker 保留最終授權 guard，restricted ancestor 的候選上限由完整合法集合大小決定；active branch 的既有 allowance 不變。
- 100,010 筆全部 matching 的 Worker fixture 與完整授權 oracle 順序一致；另驗證 40,000-ID set、空集合、跨 scope、來源 mutation、stale vector、cancellation 與兩種 fusion。沒有透過 skip／改名避開大型案例。
- [效能報告](../reference/ancestor-memory-retrieval-evaluation.md) 與 [完整 samples／source hashes](../reference/ancestor-memory-retrieval-evaluation.json) 保留 16 組比較。全部 ordered IDs 相同，1K／10K 的延遲與配置量 median gate 全數通過；100K vector canonical hydration 100,000 → 4 筆。大型 FTS 的成本變化與合法集合極大時既有 MMR 限制如實揭露。
- 最終完整 working-tree `go test -p 2 ./... -timeout 30m` exit 0：CLI 158.332s、context 72.495s、team 656.382s；其他 packages 通過或無測試。這包含保留的無關本地修改，不冒充 immutable 新 HEAD 的證據。
- 最終定向 race exit 0：context 308.485s、team 1018.747s、CLI 12.875s；命令／實際涵蓋的行為名稱見機器報告。`mise run vet`、`mise run lint` exit 0，lint 0 issues；cache 寫入的 sandbox 警告未停用分析或改動 cache 設定。
- `bin/check-docs` 與 `git diff --check` 通過。本階段獨立提交；WP3–WP5、另一份 dependency projection 計畫，以及全部功能完成後的整合與完整受影響 race 仍未完成。
