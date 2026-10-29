# hufu Dependency Result Projection 與結果邊界回歸計畫

> Status: active
> Authority: reference
> Verified-Commit: a3ae5756
> Supersedes: —
> Superseded-By: —

> **狀態**：PR-1 設定與持久化完成，必要驗證通過；selected 執行仍明確拒絕，PR-2／PR-3 尚未實作。
> **查核基準**：`a3ae5756`（2026-10-28）；以已提交原始碼確認既有能力，另核對目前工作樹。
> **文件性質**：正式實作計畫；功能完成以前，不取代既有 normative 架構文件。
> **範圍**：靜態依賴結果投影、三條既有結果路徑的契約回歸、已證實缺口的最小修正。
> **執行方式**：僅修改程式、測試及文件，使用離線 fixtures；不需要真實模型、憑證或外部服務。

## 1. 現況、效益與範圍決策

原稿以 `c8c52d30` 為基準，將結果收集與 admission 分離列為新功能。
目前已有以下能力，不能按原稿重複建造：

| 目前事實 | 原始碼／正式文件 | 對本計畫的影響 |
|---|---|---|
| 本機結果模式只接受 `runtime`、`tool`；相容的新 session 預設 `runtime`，舊 snapshot 缺欄位代表 `tool` | [transport policy](../../internal/team/result_transport.go)、[設定參考](../reference/agent-format.md#結果傳輸result-transport) | 保留現有預設、設定名稱、優先序及 frozen policy |
| Runtime collector 保存工作，再以無工具的模型呼叫收集結果；不依賴 provider native JSON Schema | [collector invocation](../../internal/team/result_collector_invocation.go)、[collector commit](../../internal/team/result_collection_commit.go) | 已能解除 Worker 必須呼叫 `submit_result` 的限制 |
| Tool 與 collector 已共用 `localResultAdmission`；admission 不執行工具、不標記任務完成 | [result admission](../../internal/team/result_admission.go)、[tool adapter](../../internal/team/coordinator_tools_result.go) | 不再建立第二套 Admission Service |
| External backend 使用不可信 `WorkerResultProposal`、獨立 workspace delta 查核，再進入 runtime 結果儲存／完成流程 | [external canonicalizer](../../internal/team/subagent_external_result.go)、[provider contract](../../internal/team/subagent_provider.go) | 保留 external adapter，僅對真正共通的驗證做回歸 |
| `FormatDependencyResults` 展開 typed handoff，`dependency_results` 為必要 context | [context compiler](../../internal/team/context_compiler.go) | 可新增 opt-in 精簡視圖，預設 formatter 保持原樣 |
| `requires-evidence` 查核完整來源、實際 context 及 durable delivery manifest | [evidence delivery](../../internal/team/task_evidence_delivery.go) | 第一版不得對需要此證明的任務裁切 |
| 靜態任務使用 `contracts:`；`tasks:` 是舊讀取別名 | [manifest](../../internal/team/team_manifest.go)、[contract compiler](../../internal/team/contract_compile.go) | 新範例只使用 `contracts:` |

### 1.1 留下的工程工作及效益

1. **依賴結果投影**：為不需完整證據的下游 Worker，只傳契約選定的欄位。
   直接效益是縮小 dependency context、減少無關內容；用同一 fixture 的 bytes 與 tokens
   驗證縮減，不預先承諾模型品質、延遲或費用改善百分比。
2. **三路徑契約回歸**：比較 tool、runtime collector、external proposal 在共同能力上的
   admission／完成判定，保護收據、repair、resume 和副作用不重播。
   效益是及早發現語意分歧，避免以搬動程式碼代替可觀察改善。
3. **有測試證據才修正邊界**：只有回歸測試證實重複驗證或完成 gate 分歧時，才做最小抽取。
   若既有行為已一致，交付測試及文件即可，不強制重寫 lifecycle。

### 1.2 已移除的工作

以下僅記錄範圍決策，不是待辦或後續實作承諾：

| 移除項目 | 原因 |
|---|---|
| 新增 `result-transport: native | auto`、provider native schema adapter | 原稿未指定 backend／模型及可驗證能力，且 runtime collector 已滿足解除強制 submit 工具的需求；沒有足夠依據再增加協定與 fallback 組合 |
| 重新打造通用 `ResultAdmissionService`、新結果 Store／AcceptedResult 持久狀態 | 已有 admission、occurrence transaction、canonical result 及完成 gate；新增平行權威會增加相容與復原風險 |
| Typed Task Signal、`report_signal`、收件人 ACL、cursor／ack 與新訊號事件 | 尚無具體未完成任務協作案例；不能以「若有需要」的未決條件作為可直接實作的工作 |
| 真實 Ollama／外部 agent 混合執行、上線、量測專案、人工審核排程 | 需要使用者提供環境、憑證、費用及操作授權；離線 fixtures 足以驗收本計畫的 deterministic contract |
| 完整 message bus、跨團隊共享、field-level evidence proof、任意 JSONPath | 超出已界定問題，且會擴大授權與證據模型 |

## 2. 不可改變的約束

- 模型只提供 claims。Run／Branch／Task／Attempt、occurrence revision、lease、原生收據、
  HMAC、runtime outputs、hash 和 workspace identity 由 runtime 決定。
- `submit_result` 成功、collector JSON 合法、工具 exit 0 都不能單獨代表 `TaskDone`。
  必須通過現有 verifier、evidence、workspace apply、terminal cleanup 及 completion gate。
- 保留 `submitted`、`collected`、`external_provider_proposal`、`runtime` 等既有 Source 語意；
  本計畫不新增傳輸、不更名已存值。
- `partial`、`failed`、`blocked` 可為合法已接收結果，但不能當成成功；
  `completed_with_gaps` 依原契約與 acceptance 判斷。
- 同批依賴使用 `depends_on`；跨批證據使用 `evidence_from`。不得自動搜尋所有歷史結果。
- `FactRefs`、artifact CAS、授權 scope、runtime-owned input binding 及來源圖沿用現有機制。
- 投影只改傳給 Worker 的 presentation，不修改已接受結果、CAS、收據、payload hash 或授權。
- Result-only repair、fallback、resume 必須沿用原證據；未知副作用先 reconcile，不重播工作。
- 保留 occurrence reservation／commit／rollback、branch-scoped idempotency 及 result-only retry。
- 不加入具體團隊名稱分支，不修改無關團隊設定，不建立另一套 recovery engine。

## 3. PR-1：依賴投影設定、編譯與持久化

### 3.1 固定對外設定

新增靜態 contract 欄位 `dependency-result-view`，不新增 team／agent 全域預設或動態工具參數。
下例是將加入 parser tests 的 **contract 片段**；整個團隊的 agents、workflow 與 delegation
沿用測試 fixture 的有效設定，這段不能單獨作為完整 team.yaml：

```yaml
contracts:
  - id: implement
    agent: coder
  - id: review
    agent: reviewer
    depends-on-contracts: [implement]
    dependency-result-view:
      mode: selected
      fields: [summary, findings, artifacts, files_modified, verification]
```

| 欄位 | V1 規則 |
|---|---|
| 未宣告 | 等價 `full`，不得改變舊 prompt、contract hash、policy bytes 或 manifest fingerprint |
| `mode` | 必填，小寫 enum：`full`、`selected`；拒絕空值、其他大小寫及未知值 |
| `full` | 不接受 `fields`；正規化成未設定，沿用原 formatter |
| `selected.fields` | 必填非空陣列；enum 僅 `summary`、`findings`、`artifacts`、`files_modified`、`verification` |
| 重複欄位／未知鍵 | 拒絕，不靜默去重或忽略 |
| 欄位順序 | 編譯成固定順序：summary → findings → artifacts → files_modified → verification；作者順序不影響 hash |

所有 selected 結果一律保留來源 task ID、agent、status header，並標示為「選定欄位視圖，
不代表完整上游證據」。未選欄位不能因模型文字、budget 或 retry 被自動加入。
選到的欄位保留完整資料與既有授權限制；空欄位可不渲染，不生成虛構值。
Artifact 僅顯示現有 opaque reference 與允許的 metadata，不展開檔案或增加讀取權限。

### 3.2 V1 適用範圍與拒絕規則

Selected 僅用於已成功綁定靜態 contract、具有同批依賴的普通 Worker task。
以下一律拒絕 selected，使用者要明確改為 full，不能偷偷 fallback：

- `Execution.RequiresEvidence`，或任何 `evidence_from`。
- 下游已綁定 `result-contract`，包括 evidence/runtime-owned／fixed collection bindings。
- 上游實際結果含 `StructuredPayload`；V1 不做欄位級證據證明。
- native action、sidecar、plan-first、closed tool sequence，或沒有 canonical typed source。
- 無法還原完整靜態 contract 身分、沒有同批依賴，或來源不在既有 branch／workset 授權範圍。

能從靜態設定判斷者在 lint／contract preflight 拒絕；必須等上游結果才知道者在下游
attempt context 編譯前拒絕。兩者都要發生在**該下游 Worker 的 provider 呼叫之前**。
不得為了提前檢查而禁止上游正常執行，也不得在下游已派發後才回報設定衝突。
Full 模式的 `evidence_from`、result-contract、requires-evidence 行為全部沿用原規則。

### 3.3 程式落點與資料生命週期

1. 在 `internal/team/` 定義 `DependencyResultViewSpec`。TaskDef 的作者欄位只讀 YAML，
   使用 `json:"-"`；動態 `agent` tool 不公開此欄位，未知參數維持拒絕。
2. 接入 manifest strict decode、`ValidateTeamTaskContracts`、初始與具名 contract 編譯路徑。
   必須完成靜態 contract 綁定才可取得 selected；未綁定不得忽略設定後繼續執行。
3. 將正規化 policy 放入 `EffectiveTaskContract`、contract hash 及 execution policy snapshot。
   只在 selected 存在時加資料；full 的舊序列化及 hash 不變。
4. 透過既有 task admission 封存在 `TodoItem`，同步 task-created event、reducer、checkpoint、
   taskDefFromTodoItem 與 clone。模型不能指定或覆寫這些 runtime-bound 欄位。
5. Selected 任務不得先 dispatch 再補 policy。Resume 使用持久化值，設定漂移依既有規則拒絕。
   Fanout／額外模型與 retry 必須取同一份 frozen policy；不支援的路徑在 preflight 拒絕。

### 3.4 相容性決策

- 保留所有既有受支援的 snapshot／contract 讀取版本與重建算法。
- 僅含 full 的 session 保留原 policy／effective contract 版本與序列化。含 selected 的新
  session 使用下一個 policy snapshot 版本，selected effective contract 也使用新 revision。
  既有 session 以原版本重建比較，不因新增欄位漂移；不得在原 session 中加 selected。
- Selected 使用的新 snapshot version 必須使舊 binary 在執行／寫入之前拒絕新 session，不能忽略 selected。
  新程式必須讀取舊 session 為 full，不憑現行設定補入歷史 policy。
- 不新增 SQLite table 或第二套 Store。新增值使用既有 canonical snapshot／event／checkpoint。
- PR 交付舊版 fixtures 與版本拒絕測試，列明新 session 的降版限制；不得宣稱可無條件降版。

**PR-1 驗收**：設定、hash、版本與 replay 測試通過；selected 已可封存，但在 PR-2 完成前，
啟動 selected 執行必須明確拒絕，不能先以 full 假裝成功。未設定者行為不變。

## 4. PR-2：投影渲染、實際交付與復原

### 4.1 資料與 hash 的定義

- `dependencyResultsForTask` 仍選取來源，保留既有 scope/workset 限制；
  `validateDependencyPayloads` 驗證完整 canonical source，不接收投影後的假 TaskResult。
- 新純函式從已授權的完整 handoff 建立獨立 view；不就地修改 `TodoItem.TypedResult`、
  source slices／maps 或其中的 artifact refs。Full 直接呼叫現有 `FormatDependencyResults`。
- WorkerContextInput 分開傳 canonical dependency results 與 rendered view／provenance；
  不用清空 TaskResult 欄位的方式冒充已接受結果。
- Selected 以現有 source order 渲染；欄位固定排序；沿用 redaction 與 `fenceUntrusted`。
  不重新 redaction 已雜湊的 canonical receipt／runtime outputs。

Selected manifest 的新增 attribution 必須由 runtime 計算，至少包含：

| 資料 | 定義 |
|---|---|
| 來源 identity | 已核准來源的 branch、task、attempt、occurrence revision；run 身分沿用現有可驗證 binding，不由模型補填 |
| `full_view_sha256` | 對投影前、已套用授權及 workset 限制的單一來源 full handoff UTF-8 bytes 計算 SHA-256；不是原始 event 或 payload hash |
| `policy_sha256` | 正規化 selected policy 的 deterministic JSON bytes SHA-256 |
| `ContentHash` | 實際完整 dependency_results context item 的 SHA-256，沿用既有欄位 |

不以 path、CAS locator 或 redacted payload 的重算值冒充既有 canonical digest。
需要 structured payload 的來源已在 V1 排除，既有 full evidence hash 規則保持原樣。

### 4.2 接線與失敗處理

1. 在 `CompileWorkerContext` 組合 `dependency_results` 前產生 selected view，沿用
   Required item、budget admission 與 dedup 身分；不能把省略欄位解讀為來源不存在。
2. Selected item 也必須完整交付，不得壓縮或截斷已選欄位。超過 context budget 使用既有
   RequiredContextBudgetError／失敗分類，在 provider 前停止；不新增自行重試迴圈。
3. 擴充既有 ContextInjectionManifest 的 selected attribution 與相應版本驗證；
   full 沿用既有 manifest 版本、序列化及 fingerprint。
4. Retry／fanout 的每個實際 request 都記錄來源及 view hash；只把 `ModelCalled` 的
   authoritative request 算成交付，不能用 preview 或未派發 manifest 代替。
5. Resume／replay 從 frozen policy 與原授權來源重建核對。來源 revision、policy、
   view hash 缺失或不符即拒絕該任務執行／完成；不尋找「最新結果」替換。
6. Result-only repair 沿用原已封存工作與投影；不要為了補完整欄位重跑 Worker。
   `team_info`、coordinator 既有 handoff、CAS 完整結果及 required evidence checks 不做裁切。
7. Inspector／JSON／report 顯示 projection 模式及必要 provenance；維持既有 task 狀態，
   不由 presentation layer 推定成功。正式設定參考與架構文件一併更新。

### 4.3 必要驗收

- [ ] 未設定／full：prompt bytes、source order、hash、manifest、依賴 gate 與既有結果查詢不變。
- [ ] Selected：只含 header 與選定欄位；來源、CAS、artifact 授權、payload／receipt bytes 不變。
- [ ] Parser：未知欄位、空選擇、重複、非法 mode、動態覆寫及不相容任務均拒絕。
- [ ] 完整 evidence 場景仍成功；相同場景宣告 selected 時，在下游 provider 前拒絕。
- [ ] 多上游不同完成順序、retry、fanout，來源與 policy 相同則投影 digest 相同。
- [ ] Cross-branch、過期 revision、篡改 checkpoint／manifest／policy 均拒絕；不擴大 source scope。
- [ ] Required budget 不足不派發；不截斷、不改派更大 context 的未授權模型。
- [ ] Resume 與 result-only repair 保留原 receipt、view 及 task identity，不重播 mutating tools。
- [ ] 固定 fixture 含大量未選 details／risks，selected 的 bytes 及相同 tokenizer 的 token 數
  均小於 full；記錄數值。此測試只證明 context 縮減，不以模型品質作驗收。

## 5. PR-3：既有結果路徑的契約一致性

### 5.1 比較範圍

使用 `newTestCoordinator`／`scriptWorker`、本機 fake HTTP provider、fake external backend
與 `t.TempDir()`，不啟動真實 Ollama 或外部 agent 程序，不讀使用者 workspace/database。
比較 tool、collector、external 的**共同可表達資料與 native evidence**。
不強迫 external 支援 local 專屬欄位，不把不同 transport 的 Source／合法事件差異判成 bug。

| 場景 | 驗收 |
|---|---|
| 合法 success、completed_with_gaps | 各自經 admission；只有原有 verifier/evidence/apply gates 通過才完成 |
| 合法 partial／failed／blocked | 保留 typed handoff，維持各路徑既有失敗與 recovery 語意 |
| 非法 schema／runtime-owned 欄位／假造 artifact 或 receipt | 在對應 trust boundary 拒絕，無成功投影 |
| 舊 session resume、prepared candidate、prepared collection commit | 保留原 receipt／hash，無額外工作執行 |
| Stale lease、併發 double submit、duplicate result-only retry | 沿用既有 idempotency；每 occurrence 至多一個 accepted result，不重複 materialize |
| CAS／event I/O 失敗、materialize 後 commit 前中斷、commit 後 Done 前中斷 | 原證據可復原或保守阻擋；無雙重 apply 或未知副作用重播 |
| Plan-first、closed sequence、direct-agent、nested worker、extra model | 支援者保留契約；不支援者於既有 preflight 拒絕，不能繞過工具授權 |
| runtime-owned action result | 保留可信獨立入口，共用完成 gates；不被改走模型 submission DTO |

比較 canonical 狀態、驗證、收據關聯及 durable replay；runtime 生成 ID／時間不同可在
測試比較層明確映射，但不得修改儲存值、重算偽造 receipt 或忽略真正 payload 差異。

### 5.2 修正界線

先補能證實分歧的行為測試，再做最小修正。沿用 `localResultAdmission`、
`validateStructuredResultPayload`、ExternalResultCanonicalizer 與 occurrence transaction。
不能把 external filesystem／delta 驗證移除後，直接信任它提供的 TaskResult。

若沒有測試證實的分歧，PR-3 只交付契約測試及責任文件；不新增 service interface、
持久狀態、failure enum 或 lifecycle controller。需要修正時沿用既有 failure classes，
並同步相關 reducer、checkpoint、journal、inspect、JSON、report 及適用的 TUI projection。

**PR-3 驗收**：上述矩陣有明確測試對應；原有 gate 與 recovery fixtures 通過，
並能說明每項程式變更修正哪個已觀察行為。

## 6. 執行順序與驗證

順序固定為 **PR-1 → PR-2 → PR-3**，每階段完成必要驗證再往下。
目前工作樹已有其他修改，實作者先記錄 `git status --short`，不得覆寫或吸收無關變更。
使用者已授權將文件移入正式目錄、確認 baseline、實作與逐階段 commit；不推送或執行真實 provider 驗證。

每階段由 coding agent 完成：

1. 對照實際 HEAD 與本文查核基準，閱讀 [AGENTS.md](../../AGENTS.md) 及相關 runtime skills。
   若能力已存在則重用；若 normative 文件衝突，先修正計畫，不偷偷改產品語意。
2. 補相應 behavior-named table-driven tests；Go 使用 mise toolchain、現有 coordinator fixture，
   每個 test 清理自己的資料／程序；source 與 test 檔案遵守長度限制。
3. 先跑確實包含新增測試的 targeted tests，再依序跑以下 gates；不能把空匹配視為通過。
   並行 lease／manifest／event 變更加跑涵蓋該行為的 targeted race。

```bash
# TMPDIR 設為本次唯一的私有 /tmp/codex-task.XXXXXX；保留 GOTMPDIR 與共享 caches。
# 依 AGENTS.md 先查 /tmp、Go cache volumes、可用 RAM；與 live team runs 分開。
GOMAXPROCS=2 mise exec -- go test -p 2 ./... -timeout 30m
mise run vet
mise run lint
# Race 使用明確受影響 package／test，GOMAXPROCS=2、-p 2，與上列命令循序執行。
```

4. 故障注入使用既有 fixture／注入點，不需要 kill 真實工作程序或操作歷史使用者資料。
5. 回報變更、已通過與失敗的命令、baseline 既有失敗、剩餘風險與版本限制。
   有失敗不得宣稱完整通過；未跑真實 provider 必須明記，不能由 fixture 推論供應商相容。

同步正式文件：
[execution runtime](../architecture/execution-runtime.md)、
[agent format](../reference/agent-format.md)、
[strict verification](../architecture/strict-verification.md)、
[workset](../architecture/workset.md)。
本文已依使用者後續授權移入 `docs/implementation-plans/`；`docs/tmp/` 不加入版本控制。

## 7. 完成定義

只有全部符合才宣告本計畫完成：靜態 selected view 可供合格任務實際使用；full 舊行為相容；
必要證據不能被裁切；投影政策與交付可持久化及復原核驗；三條既有結果路徑具有契約回歸；
需要的程式 gates 全部通過。沒有 native transport、Task Signal、真實環境操作或人工決策
藏在驗收條件中。實作者不需要替使用者選擇 provider、取得憑證或安排外部操作。

## 8. Baseline 實測紀錄

正式文件提交 `825ff686` 的完整隔離測試已執行，結果及 CLI fixture 問題見
[baseline 報告](../reference/runtime-continuation-baseline.md)。當時 PR-1–PR-3
尚未實作；後續實作紀錄見下節。baseline 失敗不當成產品功能已驗收，也不以工作樹測試代替 immutable baseline。

## 9. PR-1 實作與驗證（2026-10-29）

- 嚴格 YAML policy、固定欄位順序、full 正規化為未設定；未知鍵、重複／空欄位與動態覆寫均拒絕。
- Initial／named 靜態 contract 同步 policy 與 revision 2；full 保持 revision 1。
  任務建立、occurrence digest、event reducer、checkpoint、clone 與 journal 保留 policy。
- Selected 使用 policy snapshot 6 與完整 SHA-256 policy digest；full 仍使用版本 5，
  舊版 3/4/5 讀取算法保留，舊 session 加入 selected 會在 admission 拒絕。
- Provider startup 前先封存 selected policy，再明確拒絕執行；PR-2 尚未完成，不以 full
  替代 selected，也不把此階段當成已交付投影功能。
- 實際 `b02b302b` 隔離 source 的舊 helper 量出 full contract 與 occurrence digest，
  以固定 fixture 驗證新程式未改 hash；沒有改 baseline tracked source 或更新 golden 迴避漂移。
- 定向契約／舊 golden／task digest／manifest alias 測試通過（team 0.381s）；
  加入 migration 案例後，全部 TestDependencyResultView 定向測試亦通過（0.103s）。
  隨後將兩項 unavailable 斷言改為 errors.Is 檢查既有 sentinel。2026-10-29 恢復驗證後，
  投影／occurrence／policy 定向測試通過：team 0.920s、improver 0.026s。
- 初次驗證曾因快取磁碟不足而停止；使用者於 2026-10-29 授權清理可重建的專案快取與
  停用暫存，保留相依套件與快取設定，容量阻礙已解除。後續按新的階段快取預算執行。
- 恢復後第一輪完整測試發現 improver 的明確 policy schema 分類缺少新增欄位；
  已將 DependencyResultViewsHash 分類為固定政策，recursive fixture 僅增加該欄位與分類。
  不放寬未知欄位或既有 version-5 strict comparison 邊界。team 全量通過（660.199s）；
  CLI／improver 的已知失敗須由修正後的完整重跑確認，不宣稱此輪全套通過。
- 修正後完整 `mise exec -- go test -p 2 ./... -timeout 30m` 已通過：CLI 146.639s、
  improver 153.815s；team 沿用上述相同原始碼的成功測試快取，其餘 packages 通過或無測試。
  此為保留既有無關修改的 working-tree 驗證，不稱為 immutable 新 HEAD 測試。
- 上述原始碼的定向 race 通過：team 42.901s、improver 1.096s；`mise run vet` 通過。
  `mise run lint` 指出 policy factory 複雜度 44 超過門檻 40；已將新增的 selected hash／
  版本驗證原樣抽成 helper。抽取後定向回歸通過：team 4.522s、improver 0.027s。
- 抽取後的最終完整 `go test -p 2 ./... -timeout 30m` 通過：CLI 145.112s、
  improver 159.500s、team 666.902s；其餘 packages 通過或無測試。
  定向 race 覆蓋 selected、occurrence digest、execution policy 與 schema 分類，
  通過：team 55.199s、improver 1.104s。`mise run vet`、`mise run lint` 通過，lint 0 issues。
- 驗證前後 2,556 個 Go／工具設定檔的 SHA-256 一致。保留目前 normal／race compiler
  exports 後，清理本輪 104 個過期專案封存檔與索引（約 698 MiB）；本輪仍需保留的
  專案封存檔約 701 MiB，未刪相依套件或現行 exports。
- 文件與 diff 檢查通過；本階段可提交。PR-2／PR-3、另一份計畫的 WP3–WP5 與
  最終整合驗收仍未完成，不能把 PR-1 當成可實際執行 selected 的交付。
