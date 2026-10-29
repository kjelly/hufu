# Hufu Runtime：安全接續、歷史結果交付與新任務 Fork 實作規格

> Status: active
> Authority: reference
> Verified-Commit: 4e13abfb91a1328ccc1c58d0fd4a7861b17d6d43
> Supersedes: —
> Superseded-By: —

> **Implementation status**：WP0–WP2 與 WP6 已提交。WP3 已完成 grant 儲存、任務綁定、required context、canonical run CLI 與使用報告的階段實作及驗證。WP4 已完成 native fork、child 投影與第一個新請求的階段實作及驗證；WP5 與目標結束時的完整整合／race／baseline compatibility matrix 尚未完成。階段證據見第 12–24 節。
> **Repository**：`kjelly/hufu`
> **Reviewed baseline**：`4e13abfb91a1328ccc1c58d0fd4a7861b17d6d43`
> **Revised**：2026-10-29
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
- 以 baseline binary 操作新版 fixture 驗證拒絕；覆蓋 run/resume/session fork/checkout 與唯讀 results，所有支援布局均須在模型與任務執行前拒絕，不能新增事件或改動專案內容。已初始化 version-store 的工作區另須驗證完整檔案零變動。
- 未初始化 version-store 的布局須明確記錄舊 binary 可能先建立空版本資料庫與鎖檔的限制；不能把已初始化 fixture 的零變動結果推論至此布局。此限制不允許啟動 provider／Worker 或改寫 runtime events。新版再驗證可正常讀寫。

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
| old schema read-only、新 schema baseline binary | 新版讀舊資料不 migration；舊版在模型／任務前拒絕，檔案變動依第 6.3 節的布局別保證驗證 | 3/4 |
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

## 17. WP3 儲存層基礎階段

Migration 5 新增 branch/cycle → canonical grant event 的索引，沒有另存結果內容。
低階 helper 驗證來源 event type、branch、cycle/run、ID 與 hash，保留第一次 admission，
相同來源 replay 為冪等、不同來源拒絕替換。事件與索引的原子寫入須由後續 EventStore
整合提供；目前尚未開放 CLI grant、recipient 授權或 Worker context delivery。

[格式相容性與本機實測](../reference/runtime-grant-storage.md) 記錄新版唯讀支援
schema 3/4、舊 verifier 拒絕 schema 5、rollback 與來源驗證測試。
真正 baseline binary 的初始化 managed fixture 實測通過；未初始化 version-store 的
布局有舊 fork 建立空版本資料庫／lock 的限制。使用者於 2026-10-29 同意第 6.3 節的
分層保證：所有支援布局在模型與任務執行前拒絕；已初始化布局另外保證檔案零變動。
相容性決定已解決；儲存層仍不代表 WP3 的 grant／recipient／delivery 已完成。

本階段 working-tree 完整 `mise exec -- go test -p 2 ./... -timeout 30m` 通過：
CLI 149.681s、runtimestore 3.634s、team 653.881s；其餘 packages 通過或無測試。
這包含保留的無關本地修改，不稱為 immutable 新 HEAD 全測證據。
定向 `-race` 覆蓋 HistoricalGrant、PreReportReadOnly、unknown-schema rejection、
RunReportPreCapture，通過：runtimestore 5.390s、team 1.374s。
`mise run vet`、`mise run lint` 均通過，lint 0 issues；`bin/check-docs`、
`git diff --check` 通過。此儲存層可獨立提交；WP3 其餘功能與最終
integration／完整受影響 race gate 仍未完成。

## 18. WP3 grant envelope 與事件原子寫入

Schema-versioned envelope 只保存 consumer scope、recipient 與完整 ResultID、
來源身份、分開的 source／typed payload／presentation SHA-256，以及 presentation
byte count；不另存結果本文。Occurrence attempt 與實際 dispatch attempt 分別保存。
Envelope 驗證穩定排序與唯一性、16 組上限及合計 1 MiB 的宣告大小限制。

EventStore 的 append 與 event-log import 沿用既有交易，同時寫入 canonical event
及 migration 5 的索引；索引失敗整筆回滾。相同 admission key 必須符合原 envelope
且原索引仍完整；不同 key 也不能替換同一 branch/cycle 的原授權。Journal 中的
第一筆 grant event 是 first-admission 權威，索引遺失不允許改由後一筆取代。
讀取器亦拒絕被改指向後一筆有效 event 的索引。

回歸測試先重現兩項問題再修正：遮罩流程原本會合併重複 JSON member，現於遮罩前
嚴格解碼、遮罩後再驗證 envelope；索引遺失原本允許新 key／另一筆有效 event
取代原 admission，現寫入與讀取均查核 journal 的第一筆來源。測試另外涵蓋
不確定 commit acknowledgment 後精確 replay、branch/cycle 隔離、匯出／匯入保留
canonical bytes，以及整筆 import 遇衝突時零 event／零 index 殘留。

本階段仍未開放 `--inherit-result`。Recipient role／scope／lineage 授權、實際
presentation 重建與大小驗證、task binding、required context delivery 和 recovery
仍屬 WP3 後續工作，不能以 envelope 測試代替。

最終 working-tree 完整 `mise exec -- go test -p 2 ./... -timeout 30m` 通過：
CLI 149.299s、improver 160.415s、team 712.802s；runtimestore 等未再變更來源的
packages 重用已通過結果。這包含保留的其他本地修改，不稱為 immutable commit
snapshot 的全測證據。定向 `-race` 覆蓋 Historical、EventStore、EventLog、
Import／Decode／ExportEventLog 及 WorkflowCycle，通過：team 15.382s，
runtimestore 重用已通過結果。

`mise run lint` 通過、0 issues；`mise run vet`、`bin/check-docs`、
`git diff --check` 通過。Lint 曾指出 envelope 驗證函式複雜度過高；拆分單筆來源
與重播驗證後，對最終相同的 2,547 個 Go／設定檔重新完成全部必要檢查。
`event_store.go` 維持 799 行，新增測試檔低於 800 行。

本階段依 provenance 清除過時 project archives、被中止的精確 Go work／fixture
目錄與已退出的測試 engine 目錄，合計約 2.26 GiB。當前 normal／race project
exports 約 687 MiB；沒有新增快取 ELF 殘留。私有 scratch 約 149 MiB，保留單一
約 126 MiB baseline binary 供後續最終相容性測試。共享依賴與工具鏈快取維持原設定。

## 19. WP3 canonical 來源與接收者準備

唯讀 preflight 使用既有 inspect reader 的 managed control workspace／team／branch
範圍，要求完整 ResultID、有效已接受且完成的 typed result。接收者沿用 agent resolver，
別名正規化後去重與排序，拒絕 coordinator／orchestrator、wildcard、未知或歧義身份；
限制 16 組與所有完整 presentation 合計 1 MiB UTF-8 bytes。停用 historical task
reuse 時拒絕，單獨停用 historical memory 不擴張成拒絕 explicit result reference。

Reader 分開回傳原儲存 `typed_result` bytes 的 SHA-256 與完整 redacted JSON。
未知欄位、大於 2^53 的精確整數、原納秒 duration 與完整診斷不因 DTO 轉換遺失。
僅 canonical 頂層 runtime outputs 保留已綁定收據的值；任意巢狀 model facts
自稱 `source: runtime` 不能繞過遮罩。Inspect 的 typed DTO 亦保留精確 JSON number。
投影與 presentation 編碼保留原 raw JSON 跳脫字元，不將 `\u003c` 自動改寫為 `<`
而破壞 structured payload SHA-256；回歸測試驗證完成兩層編碼後仍符合原收據。

Presentation 包含 Summary、完整 typed JSON、source citation 與 validity；資料以
untrusted fence 包裝，明示不提供新任務完成證據或 artifact 讀取權限。不匯入 raw
Output、對話或 artifact bytes。來源雜湊、payload 雜湊、presentation 雜湊與
policy fingerprint 分開保存；native outputs／structured payload 的收據雜湊須完整。
重新建立 frozen envelope 時任一來源、有效性、policy 或內容變動均拒絕，不更新原 hash。

此 API 僅建立內容不外露的唯讀準備結果，不持久化 admission、不建立 task、也不呼叫
模型。CLI 入口限制、cycle activation、task binding、required context delivery、
dispatch proof 與 recovery 仍待後續實作；`--inherit-result` 尚未開放。

最終 working-tree 完整 `mise exec -- go test -p 2 ./... -timeout 30m` 通過：
CLI 155.142s、inspect 19.392s、improver 152.150s、team 703.718s；其餘 packages
通過、重用未變更來源的既有結果或無測試。這包含保留的其他本地修改，不稱為
immutable commit snapshot 的全測證據。定向 `-race` 涵蓋歷史查詢、來源準備、
接收者與 canonical reader：team 4.182s、inspect 3.304s、CLI 8.048s，全部通過。

`mise run lint` 通過、0 issues；`mise run vet`、`bin/check-docs`、
`git diff --check` 通過。最後審查以最小 probe 確認 JSON 預設編碼會改寫 raw
跳脫字元，再以回歸測試重現 structured payload SHA 損壞；修正投影與 presentation
兩層編碼後，對同一最終版本的 2,552 個 Go／設定檔重新完成全部必要檢查。

本階段清除可追溯的過時 project archives 與已退出測試目錄，合計約 1.94 GiB。
當前 normal／race project exports 約 806 MiB；沒有新增快取 ELF 殘留或未清除的
自有 Go work 目錄。私有 scratch 約 151 MiB，保留約 126 MiB 的單一 baseline
binary 供最終相容性驗證；依賴、工具鏈與既有 cache 路徑保持原設定。

## 20. WP3 cycle 授權投影與重建

Session checkpoint 僅保留 immutable grant event 的 branch／cycle／event／hash
reference，不複製來源內容或形成另一份授權。事件重播保留 resume 前的原 cycle 授權，
`workflow_cycle_started` 清除 active reference，歷史事件與索引仍保留。
同 cycle 重複授權、錯誤 cycle、task 建立後才出現授權，均拒絕重建並要求 recovery。

啟動、branch rebuild 與 active projection refresh 同時驗證可見 lineage、原始
transactional index、control workspace，以及授權前已存在的 canonical policy。
append 已完成但 checkpoint 尚未寫入時，可以從上述權威重建；同 cycle 的 checkpoint
若改寫授權 reference 則拒絕，不用 checkpoint 補造 index 或 grant event。

普通 fork 只保留分岔點前可見的 grant，繼續引用祖先 branch 的原 event／index；
不複製 child 授權。本階段只驗證 envelope 的持久身份，後續內容恢復仍須以 consumer
branch 重新驗證來源有效性與所有 frozen hashes。CLI admission、task binding、required
context／manifest delivery、dispatch proof 仍待後續實作；`--inherit-result` 尚未開放。

最終 working-tree 完整 `mise exec -- go test -p 2 ./... -timeout 30m` 通過：
CLI 147.857s、inspect 14.047s、improver 170.990s、team 721.747s；其餘 packages
通過、重用未變更來源的既有結果或無測試。此證據包含保留的其他本地修改，不稱為
immutable commit snapshot 的全測證據。定向 `-race` 覆蓋歷史結果、cycle、branch
重建與 canonical reader：team 20.785s、inspect 11.660s、CLI 8.804s，全部通過。

`mise run lint` 通過、0 issues；`mise run vet`、`bin/check-docs`、
`git diff --check` 通過。定向測試亦驗證 append-before-checkpoint 重建、snapshot
pointer 隔離、偽造 checkpoint、索引遺失、fork cutoff、scope／policy 不匹配及
policy 晚於 grant 的拒絕。驗證期間 2,584 個 Go／工具設定檔保持相同內容。

本階段清除可追溯的過時 project archives 及 48 個已退出的私有測試目錄，合計約
1.0 GiB。保留的當前 normal／race project exports 約 806 MiB；記錄的自有 Go work
目錄均已隨程序退出清除。私有 scratch 約 151 MiB，其中單一約 126 MiB baseline
binary 保留供最終相容性測試：`/tmp/codex-task.OVwpKj/baseline-hufu`。
依賴、工具鏈與 configured cache／GOTMPDIR 路徑保持原設定。

## 21. WP3 runtime 任務綁定

Runtime 在 occurrence／decision admission digest 前固定
`historical_result_binding`：consumer task ID、canonical recipient、原 grant event
reference，以及該 recipient 的完整 ResultID 清單。來源內容與 source／presentation
hash 仍由既有 grant event 保存，不複製另一份權威，也不增加 artifact 權限。

一般 execution envelope 與 Worker 自建 TODO 都在 admission 前綁定；持久化與
dispatch admission 再驗證 exact binding，不能在已授權 digest 後補寫或改綁。
按 typed `TaskSourceSubagent` 排除 nested subagent，另排除 runtime primary decision；
不按 `ParentID` 排除合法 workset／objective 子任務。Fanout 由原 task owner 驗證授權，
未派工的 recipient 不產生假 task。

Checkpoint、task cloning、canonical shadow、事件 payload 與 checked replay 均保留
binding；只有第一個 `task_created` 可建立它，且 grant 必須先於 task 可見。
Lifecycle snapshot 不得改寫，retry 沿用同一綁定。Model task ingress 明確拒絕 binding
欄位，runtime-only TodoSpec 欄位不序列化為輸入；nil binding 維持既有 digest bytes。

本階段完成 occurrence metadata 綁定。CLI admission／cycle activation、來源內容在
consumer branch 上重建、required context／manifest delivery、dispatch proof 與最終
整合仍待後續實作；`--inherit-result` 尚未開放。

第一輪完整測試發現 pre-cancelled workflow 在新的 metadata 檢查提早返回，停留於
PREPARE，未投影既有的 FAILED 狀態。修正 envelope 的純 metadata binding 準備使用
`context.WithoutCancel`；實際 tool／model／scheduler 仍使用原已取消 context。
取消回歸與綁定／舊 digest／派工定向測試通過（20.161s），並對修正後版本重跑必要檢查。

最終 working-tree 完整 `mise exec -- go test -p 2 ./... -timeout 30m` 通過：
CLI 152.357s、improver 160.493s、team 704.577s；inspect 及其他 packages 通過、
重用有效既有結果或無測試。此證據包含保留的其他本地修改，不稱為 immutable commit
snapshot 的全測證據。定向 `-race` 包含 historical binding、取消回歸、occurrence／
admission、舊 dependency view、TODO、branch／cycle、canonical reader：team
50.525s、inspect 3.215s、CLI 8.140s，全部通過。

`mise run lint` 通過、0 issues；`mise run vet`、`bin/check-docs`、
`git diff --check` 通過。最終驗證期間 2,587 個 Go／工具設定檔保持相同內容。
新增測試亦覆蓋完整 recipient/result 子集、metadata override、task_created 前置證據、
Worker TODO admission、envelope digest、fanout owner、primary exclusion、profile／
policy／index／checkpoint 拒絕與 runtime-only model ingress。

本階段清除可追溯的過時 project archives 與前後兩輪共 96 個已退出的私有測試目錄，
合計約 1.32 GiB。保留的當前 normal／race project exports 約 807 MiB；沒有新增快取
ELF，記錄的自有 Go work 目錄均已清除。私有 scratch 約 152 MiB，含最終相容性測試
所需的單一約 126 MiB baseline binary：`/tmp/codex-task.OVwpKj/baseline-hufu`。
依賴、工具鏈與 configured cache／GOTMPDIR 路徑保持原設定。

## 22. WP3 required historical context 與派送證據

在已授權的 occurrence 上，由既有 ContextCompiler 恢復完整 redacted historical
presentation。先驗證 consumer lineage、task_created binding、原 grant/index 與
frozen policy，再在 consumer branch 重讀來源並比對 source／payload／presentation
hash。一般 fork 保留 ancestor grant identity，但不能只用 ancestor 的有效性替代
child branch 的判斷。Missing、invalidated 或任何內容漂移均拒絕派送。

歷史項目是 required、不可壓縮的 reference data；compiler omission、摘要替代、
recipient／attempt 錯置與 attribution override 都拒絕。超過可用 context budget 時
不截斷。Native provider 每步與 continuation 重驗來源，將完整 presentation 列為
full-request admission 的 protected messages；合法 history compaction 保留它，
並修正同一 current prompt 同時受保護時重複附加的情況。External backend dispatch
也驗證 prompt／history 必須攜帶完整內容。

沿用既有 manifest/event authority，歷史項目使用 schema 5，與 selected dependency
view／query attribution 組合。記錄 task、recipient、attempt、request、model execution、
source hashes、完整 bytes、tokens、included/reason。舊 schema 1–4 不得夾帶新欄位；
沒有 historical item 的舊格式與 fingerprint 保持原行為。報告摘要只含 metadata。

Prepared 與實際 runtime dispatch 分開；完成與 result-only repair 必須驗證原始
canonical dispatched manifest，checkpoint 自稱已呼叫不足以成立。這是 runtime
呼叫邊界的證據，不宣稱遠端 server acknowledgment。Native action／declared tool
steps 不捏造 model-call 證據。Auxiliary model 不自動注入歷史本文。

端到端 runtime collector 測試發現原 `work_input` 會攜帶 worker 的完整 compiled
prompt。已在 collector 的輸入投影排除 runtime 注入的完整 historical blocks；原始
work input、transcript、receipt 與 manifest 仍完整保存。Worker 自己產出的 report／
observations 仍沿用既有 evidence 語意，不因此授予 source 存取權限。Tool transport
與 runtime collection 均經正常 acceptance path；replay 比對原 binding／manifests。

本階段不開放 CLI flag。新 request admission／cycle activation、resume CLI 整合、
unused recipient 的 not_dispatched 呈現、fallback／extra-model 完整入口驗收與最終
CLI／舊 binary compatibility matrix 仍屬後續 WP3／WP5 整合工作。

定向測試 `Test(HistoricalResult|DependencyResultView|ContextWindowManager)` 通過
（25.886s）；完整 lint／test／race／vet 與資源清理結果於本節末記錄。

第一輪完整測試發現無 grant 任務的 metadata 查詢提早回傳 cancellation，改變了既有
retry loop 的取消投影。僅在沒有 task binding 的負向 metadata 檢查使用
`context.WithoutCancel`；實際 source-content、worker 與 provider 呼叫仍保留取消。
另確認 collection replay 的兩筆 manifest 內容完全相同、只有未受契約保證的陣列順序
不同；測試改按 request identity 排序後比對完整 canonical bytes，而非依賴 map 迭代
順序。取消／collection 定向回歸連續 5 次通過（19.788s），修正後重跑必要 gates。

最終 working-tree 完整測試通過：CLI 138.096s、improver 155.774s、team 695.302s；
inspect 與其他 packages 通過、重用有效結果或無測試。此證據包含保留的其他本地
修改，不稱為 immutable commit snapshot 的全測證據。修正後 `mise run lint` 通過，
0 issues；最終驗證期間固定同一組 Go／工具設定內容。

定向 `-race` 通過：team 221.745s、inspect 5.718s、CLI 15.579s。範圍包含
historical grants／delivery、context window／query／manifest、result collection／
repair、invariant、取消回歸、occurrence／admission、dependency view、TODO、
branch／cycle 與 canonical reader。

`mise run vet`、`bin/check-docs`、`git diff --check` 通過。最終固定的 2,595 個
Go／工具設定檔內容保持一致。清除可追溯的過期 project cache outputs 與兩輪共 96 個
已退出的私有測試目錄，合計 2.01 GiB。保留的 current normal／race project
exports 約 809 MiB，低於 2 GiB task build-artifact 預算；沒有新增 cache
ELF，記錄的自有 Go work 目錄均已清除。私有 scratch 約
152 MiB，含最終 compatibility audit 所需的單一約 126 MiB baseline binary：
`/tmp/codex-task.OVwpKj/baseline-hufu`。既有依賴、工具鏈及 configured caches／
GOTMPDIR 保留原設定。

## 23. WP3 CLI admission、原子 cycle 與授權使用報告

Canonical `run` 的 execute intent 開放 repeatable
`--inherit-result worker=<full-result-id>`。要求明確指定單一 managed team，使用既有
agent alias resolver；自動選隊、direct-agent／piped chain、fast route、new／temp、
dry-run、decision 與 recovery augmentation 均在 model startup 前拒絕。
普通 prompt 中的 literal shell pipe 沿用原 parser 語意。

先建立 runtime 私有、單次使用的 request preflight，再由 `Run` 在 invocation 前
重驗；只有完成的 predecessor workflow 可以開新 cycle。既有 canonical policy
允許的 unused backend environment 相容差異不會改寫 grant 綁定的原始 policy hash，
也不會授權該 backend。CLI semantic warmup 延至 request admission 後，防止無效
recipient／source 在拒絕前啟動 embedding。Resume 只恢復原始 grants；下一個沒有
明確授權的 completed follow-up 清除舊 grants。

Cycle boundary 與 grant/index 共用 EventStore 的單一 transaction；在重新讀取來源
前擷取 journal head，conditional append 拒絕並行 head 變動。SQL failure 不留下
半個 cycle；uncertain commit 重播完整事件與原 index，不另造授權。單筆 append
沿用同一 transaction engine，保留原 idempotency 與 degraded recovery。

Coordinator 只收到 worker／ResultID routing metadata。JSON context routing 與
Markdown report 按 canonical manifest 顯示 `not_dispatched`、`prepared`、
`dispatched`；unused recipient 不產生假 task，checkpoint 自稱 dispatch 不成立。

實際 CLI 測試找到並修正兩個 replay 整合缺口：歷史結果的 per-task projection
必須保留 task_created 之前的原 grant authority；terminal input origin validation
必須以既有 JSON duration encoding 解讀 acceptance evidence。Fallback 的 dispatch
attempt 另由 active runtime occurrence 決定，不用 DAG retry counter 取代。

第二次請求的 CLI 測試亦暴露既有 shared-memory reducer 的 ID 碰撞迴圈：原先只取
timestamp ASCII hex 的前八碼，相同內容第三次以不同 execution identity 儲存時
無法產生新 ID。改用隨機碰撞 suffix，原 execution identity 的去重規則維持不變；
同一 regression 測試覆蓋四個 execution identities 與全部重播的 idempotency。

實際 binary 測試涵蓋單模型、rate-limit fallback 與 extra-model fanout，逐一驗證
完整本文與大整數送達、coordinator／未授權 verifier 不收到本文、unused observer
顯示 `not_dispatched`。每個案例另執行第二次普通 follow-up，驗證不沿用舊 grant，
原 accepted source event/hash 保持不變。拒絕 unknown recipient、coordinator、
invisible source、direct-agent 與 piped chain 時，連 provider metadata／embedding
啟動都沒有新增 HTTP 呼叫。Fanout 沿用既有 runtime merge 語意，由 submitted
verifier 驗收，不把 merge 冒充 worker-submitted result。

第一輪 lint 指出三個流程的複雜度超標，已拆分入口準備與歷史投影更新函式；沒有
新增 suppression 或放寬門檻。修正後 `mise run lint` 通過，0 issues。

最終 working-tree 完整測試通過：CLI 170.874s、context 61.316s、improver
155.025s、inspect 11.069s、team 709.207s；其他 packages 通過、重用有效結果或
無測試。此證據包含保留的其他本地修改，不稱為 immutable commit snapshot 的全測
證據。定向 `-race` 通過：team 254.816s、inspect 6.888s、context 1.907s、
CLI 57.514s，包含 historical grants／delivery、atomic event store、semantic
startup、workflow cycle、terminal input origin、context reducer、occurrence／
admission、collection／repair、query／manifest 與 dependency view 等受影響路徑。

`mise run vet`、`bin/check-docs`、`git diff --check` 通過。最終驗證期間固定的
2,605 個 Go／工具設定檔內容一致。清除可追溯的過期 project outputs 與 48 個已
退出的私有測試目錄，合計約 2.92 GiB。CLI 迭代期間 current 與 obsolete project
outputs 曾累積約 2.32 GiB，正式 gates 前已辨識並清理；最終保留的 current
normal／race project exports 約 829 MiB，低於 2 GiB 預算。沒有新增 cache
ELF，記錄的三個自有 Go work 目錄均已清除。

私有 scratch 約 154 MiB，包含最終 compatibility audit 所需的單一約 126 MiB
baseline binary：`/tmp/codex-task.OVwpKj/baseline-hufu`。既有依賴、工具鏈與
configured caches／GOTMPDIR 保留原設定。Derived new-task fork activation、
完整 operator diagnostics 與最終 baseline compatibility matrix 仍屬後續階段。

## 24. WP4 derived new-task fork

新增 `session fork --new-task`，僅允許 Linux managed required-mode、具備已驗證且可 materialize 的來源快照。命令先以唯讀 store 檢查來源、鎖、pending terminal commit、workspace recovery、isolated apply 與 typed side-effect state，再開 writer；拒絕來源時不自動執行 recovery。已結束、失敗及安全中斷的工作可成為來源；等待第一個新請求的 child 不能再當成另一個 new-task 來源。

Migration 6 在 branch row 保存不可替換的 derivation metadata，並限制每個 child 只有一筆 native 初始化事件。來源 event／snapshot／capture 的身份與雜湊及 task-ID floor 都固定保存。child snapshot 與 `derived_fork_created` 於同一事件交易發布，沿用既有 fork 恢復：無事件的未完成 child 可移除；已發布者向前完成，不另造執行 run。checked lineage 同時驗證 branch 意圖、原子發布、來源前綴及 high-water，拒絕只選到發布第一筆事件的 cutoff。

child 投影為 `awaiting_new_request`，清除父任務、對話、compaction、inputs、驗收、重試與舊 grants；父 events、results、receipts 及 reports 留在原位置。一般 fork 的後代仍保留原 pending 身份。相同 team 的第一個非空 canonical execute `run` 以新的 invocation 身份建立 version-1 `derived_fork` cycle，重新分配高於來源 floor 的 task ID。可在同一原子交易納入明確的歷史結果 grants；resume、direct-agent、chat、dry-run、decision、空請求及停用 workflow 的設定不能啟用 child。

第一個 derived cycle 不自動交付父回覆或未分隔的 legacy memory；後續 cycle 恢復一般對話規則。實際 binary fixture 使用成功結果 A、失敗工作 B、required-mode snapshot 與新任務 child，驗證指定 worker 收到完整 A、其他角色沒有本文、child 重新產生 typed result、下一個 cycle 沒有前一次 grant，且父事件前綴、失敗狀態及原始 SQL report snapshots 不變。

測試另揭露並修正三項整合問題：historical reader 的第一輪只有標頭，現在僅選取 scope，待預算內載入內容後才執行完整 payload-dependent lineage 驗證；舊 native report 的 verification duration 依 nanosecond JSON 格式讀取；多模型收據以共用 identity／時間順序寫入及 replay，canonical comparison 對舊 checkpoint 的副本排序，保留完整證據且仍拒絕收據內容變動。新增 CLI fixture 也會還原全域 options，避免 resume 的路徑捕捉污染後續測試。

定向回歸已通過：team 18.943s、CLI 68.272s，包含四種實際 binary 路徑及五種 recovery preview／execute 案例。沒有修改 recovery eligibility 或放寬 policy drift 檢查。完整工作樹測試已通過：CLI 167.000s、team 737.749s、inspect 20.036s、improver 151.452s；context／runtimestore 等未改動輸入重用已通過的快取結果。定向 `-race` 通過：team 91.531s、inspect 13.429s、CLI 65.672s；context 沒有符合本階段 selector 的測試，不將它列為 race 覆蓋。`mise run lint` 為 0 issues，`mise run vet`、`bin/check-docs` 及 `git diff --check` 通過。2,622 個 Go／工具輸入在 gates 期間未變動。這是含保留本地修改的 working-tree 證據，不稱為 immutable commit 的全測。WP5、最終五個受影響 packages 的完整 race 及 baseline binary compatibility matrix 仍未完成。

本階段可核對的清理累計回收 3,437,486,080 bytes（約 3.20 GiB），包含 96 個已退出的私有 fixture 目錄；這是過程中的累計回收，不等於 shared cache 的淨縮減量。目前 normal／race 專案編譯輸出保留 872,824,832 bytes（約 832 MiB），task scratch 約 156 MiB。已記錄的三個 Go work 目錄均不存在，configured Go temporary root 也沒有殘留 compiler workdir。唯一刻意保留的大型暫存檔是約 126 MiB 的 baseline binary，供目標結束時的舊版相容性矩陣使用；既有 dependency/toolchain caches 未清空。
