# Hufu Agent Team 品質改善：Coding Agent 實作計畫

> Status: active
> Authority: reference
> Verified-Commit: `2c188afe7c7078db43b84514cbaf305e4ffa9cf9`
> Supersedes: —
> Superseded-By: —
> **狀態**：`READY_FOR_IMPLEMENTATION`；表示範圍與驗收已定義，不表示功能已完成或通過實機驗證。
> **更新日期**：2026-10-16
> **核對基準**：`2c188afe7c7078db43b84514cbaf305e4ffa9cf9`
> **文件位置**：`docs/implementation-plans/agent-team-quality.md`；依使用者新指示自 docs/tmp/spec.md 搬入正式計畫目錄並提交。
> **交付邊界**：repository 內的 schemas、worker 指示、deterministic tests、指南與固定測試資料；不操作真實服務、模型或歷史 workspace。

## 0. 已確定的範圍與執行規則

本計畫只保留 coding agent 可在本地 checkout 與自建臨時目錄完成的工程工作。使用者已選擇：

- 本輪移除真實 LLM／embedding 成效評估與上線，日後另案處理。
- 歷史回顧只交付指南、模板與固定測試資料，不讀取真實執行資料，不執行核准或套用流程。

原先的定性 belief PoC、診斷迴圈建設、AT-02B／AT-03 跨任務與外部 backend 的新證據／revision 綁定，已移出本輪工作清單。本輪不新增相關 API、模式、adapter 或後續研究待辦。

### 0.1 不可破壞的既有契約

1. 依 repository `AGENTS.md` 執行；修改 Go 時使用 Modern Go Guidelines 技能，修改 runtime 契約時使用對應 runtime 技能。
2. `hufu-dev` 保持 single implementation writer；specialists、verifier 保持唯讀。保留原有工具權限、side-effect、recovery、blocking acceptance 與原始需求傳遞規則。
3. `hufu-coding` 保持初始五階段批次、Codex leaf worker 與既有 `semantic_rejection` 修復流程；本輪不修改其 team、worker 或 schema。
4. `runtime.sqlite`、`context.sqlite` 與既有 artifact store 的權威邊界不變；不新增資料庫、不遷移 workspace、不重寫歷史事件。
5. 結構合法不等於來源已查證、命令已執行或驗證通過。Schema 只約束 payload；完成判定仍由 canonical status、現有驗證與 acceptance 決定。
6. 不以自然語言關鍵字／正規表示式猜測任務意圖或驗證結論；fixtures 使用明確 typed inputs 與預先宣告的預期結果。
7. 不以更新 golden、降低驗證、刪除測試或擴大權限消除失敗。已覆蓋的案例引用既有測試，不新增重複測試。
8. 使用者已授權先提交本計畫、確認 baseline，再於每一階段實作及驗證完成後 commit 一次。不 push、merge 或部署。每次提交須包含該階段的驗證紀錄。

### 0.2 測試與環境規則

- 所有 runtime fixture 使用 `t.TempDir()`／等價臨時目錄與合成資料；測試若會讀全域設定、HOME 或 workspace registry，先重導向到臨時位置。
- fake model 與 fake encoder 可以使用本地 `httptest`；不需要 Ollama、API key、瀏覽器、真實網站或外部 provider。Loopback 測試仍可能需要環境允許建立監聽 socket。
- 未提供額外環境時，不呼叫真實模型、不下載模型、不讀現有私人 memory／run，也不把真實 workspace 當測試資料。
- 工具或沙箱限制導致檢查無法執行時，保留指令、exit code 與原因，標示未通過／未驗證；不得跳過後聲稱完成。
- 變更 schema 或 context-artifacts 設定時，只在自建 fixture 中測新舊 snapshot；不更動使用者現有 session。
- 不假設 PATH 上的 `hufu` 與 checkout 同版；驗證命令一律使用由當前 checkout 建置的臨時 binary。

## 1. 已核對的能力與限制

| 現況 | 本輪使用方式 |
|---|---|
| `hufu-dev` 已有 `memory-learning: observe`、四個授權 worker 與 `go test/vet/lint` blocking acceptance；尚無 worker result-contract | AT-02A 加上最小結果 schema，保留其餘行為 |
| Semantic retrieval 已支援 off／shadow／active／auto 與 fake encoder 測試 | AT-01 只驗證契約；不修改 bundled team 的檢索模式 |
| `context-artifacts` 已存在，預設關閉，有大小、授權及來源限制 | AT-04 在臨時 team／attempt 中驗證；不預設啟用於 bundled team |
| `evidence_from` 在批次建立前要求來源已是當次 run 的成功完成任務 | 不對尚未完成的同批次上游加入此欄位；不把 depends_on 當作 evidence_from |
| `x-hufu-evidence-inputs/references` 綁定 evidence_from；references 另檢查來源及提交者 files_read | 本輪沿用既有測試，不為 hufu-dev／hufu-coding 新增擴充接線 |
| `task_result_assert` 的提交驗證只對成功狀態套用 assertions | `status: failed/partial/blocked` 不會單因 PASS assertion 被拒；required schema 驗證仍須滿足 |
| `hufu result report` 已讀取 immutable report snapshot，缺快照時明示 unavailable | AT-05 重用 CLI，交付合成資料示例；不新增歷史查詢 runtime |
| web-research 已有 semantic active、required-workers-accept-gaps 與來源降級；code-review 已有 snapshot/evidence/report audit | AT-06 只補缺少的 regression，不重新實作 |

`hufu team explain` 可展示部分 compiled configuration，但不輸出完整 execution-policy snapshot、semantic/context policy 或全部 task 契約。盤點須合併原始設定、CompileTeam 結果與既有 snapshot 測試，不以 explain 單獨證明完整配置。

## 2. 工作順序與依賴

保留原有工作編號，方便對照；未列出的編號不屬於本輪。

| 順序／編號 | 可交付成果 | 依賴 | 預期工程效益 |
|---|---|---|---|
| 1／AT-00 | 靜態基線、既有測試對照與權限差異清單 | 無 | 避免重做既有能力與引入未察覺的配置漂移 |
| 2／AT-02A | hufu-dev 的三份 schema、四個 worker 接線及契約測試 | AT-00 | 減少交接欄位遺漏，保留負面結果與缺口 |
| 3／AT-04 | context-artifacts 邊界回歸及團隊使用說明 | AT-00；整合案例使用 AT-02A | 確認可保存與不可保存的輸出範圍，避免錯誤的完整性承諾 |
| 4／AT-05 | 回顧指南、候選模板與固定資料示例 | AT-00 | 讓後續人工回顧可追溯，無需新增 runtime |
| 5／AT-01 | 語意檢索離線契約覆蓋／缺口補測 | AT-00 | 保護選取、授權、fallback 與 snapshot 語意 |
| 6／AT-06 | 研究／審查既有資料品質契約的缺口補測 | AT-00 | 防止缺口、來源失配或未完成工作被展示成已驗證 |
| 7／AT-09 | fake-model 整合、驗證彙整與 fixture 回滾測試 | 前述所有保留項目 | 證明本輪組合可在無外部服務下運作 |

AT-01／AT-06 若測試已完整覆蓋，交付測試名稱、執行結果與對照即可，不要求為產生 diff 而新增程式。每項改動先執行 focused tests；AT-09 的整合案例也應隨 AT-02A／04 開發，不留到最後才發現契約衝突。

本輪完成不代表中文召回率、實際 agent 成功率、成本或延遲已改善；這些數值不是本輪驗收門檻。

## 3. AT-00：靜態基線與覆蓋盤點

### 3.1 工作

- [ ] 記錄 HEAD、既有 dirty paths、Go 與 golangci-lint 版本。保留使用者的無關修改。
- [ ] 從 checkout 建置臨時 CLI，對五個 bundled teams 執行 validate、lint、explain。
- [ ] 讀取 hufu-dev 的 team.yaml、四個 worker 與 maintainer 指示，記錄 tools、side-effect、recovery、delegation、acceptance、result-contract 的 before 值。
- [ ] 對 AT-01／02A／04／05／06 的驗收案例建立「案例 → 既有測試 → 缺少覆蓋」對照，優先補實際缺口。
- [ ] 先執行所列既有測試，將既存失敗與本次變更造成的失敗分開記錄。

```bash
git rev-parse HEAD
git status --short
go version
golangci-lint version

# 在同一 shell 使用此變數；目錄只存建置產物與驗證輸出。
assessment_tmp=$(mktemp -d /tmp/hufu-team-check.XXXXXX)
go build -o "$assessment_tmp/hufu" ./cmd/hufu
for t in hufu-dev hufu-coding hufu-code-review web-research strategic-decision; do
  "$assessment_tmp/hufu" team validate --team "$t"
  "$assessment_tmp/hufu" team lint --team "$t" --format json --fail-on error
  "$assessment_tmp/hufu" team explain --team "$t" --format json
done
```

逐條保留 exit code，前一項失敗時仍記錄其餘獨立檢查。既存 `legacy_execution_provider_field` warning 不要求本輪遷移；不得順手修改其他團隊。

### 3.2 驗收

五個團隊沒有新 static errors；before 配置與待改範圍清楚；每個後續案例都有既有測試或明確的補測落點。不呼叫 doctor/provider、不盤點或轉換真實 workspace。

## 4. AT-02A：hufu-dev 最小結構化結果

### 4.1 檔案所有權

新增：

- `.agent-teams/hufu-dev/schemas/analysis-v1.json`
- `.agent-teams/hufu-dev/schemas/implementation-v1.json`
- `.agent-teams/hufu-dev/schemas/verification-v1.json`
- `internal/team/hufu_dev_result_contract_test.go`；固定 payload 可放相鄰 `testdata/hufu-dev-results/`。

修改：

- hufu-dev 的 `runtime-engineer.md`、`integration-engineer.md`、`implementation-engineer.md`、`verifier.md`。
- `maintainer.md` 僅補讀取 structured_payload 與保留 gaps 的指示。
- `internal/team/testdata/bundled-teams-compat/hufu-dev/expected-effective.json` 僅在結果契約確實改變快照時更新，並逐欄說明差異。

本項不修改 team.yaml 的 dispatch／acceptance，不新增 static tasks、evidence extensions 或泛用 runtime 行為。

### 4.2 Schema 決策

三份 schema 均使用 Draft 2020-12、object root、`additionalProperties: false`；每個物件列出 required。每份自含 `$defs`，不引用另一個檔案或網路 schema。

| Schema／角色 | 必填欄位及型別 |
|---|---|
| analysis／runtime-engineer、integration-engineer | `scope: string`；`observations`、`risks`、`recommendations`、`gaps` 均為 string array |
| implementation／implementation-engineer | `changed_files`、`tests_added`、`limitations`、`gaps` 均為 string array；`checks: check[]` |
| verification／verifier | `checks: check[]`；`failures`、`gaps` 均為 string array |

固定限制：

- `scope` 為非空白字串，最多 2000 characters；array 的字串元素為非空白，最多 1000 characters。
- 所有 array 最多 50 個元素，允許空集合；未知事項用 gaps 描述，不能為滿足 minItems 虛構結果。
- check 必填 `command: string`、`outcome: enum [pass, fail, unverified]`、`exit_code: integer|null`、`note: string`；未知欄位拒絕。
- command 為非空白、最多 2000 characters；note 最多 2000 characters，允許空字串；exit_code 為 null 或非負整數。
- `pass` 要求 exit_code 為 0；`fail` 要求為正整數；`unverified` 可以是 null 或已觀察到的 exit code，且 note 必須非空白，說明為何無法得出驗證結論。以 schema 條件約束這些欄位，不解析 note 猜結論。
- 每份 schema 不超過 8 KiB，避免擠壓 worker context。保持既有 runtime payload 上限，不放完整 log、patch、秘密或大型證據內容。
- 不新增 payload-level 總 verdict／status，避免與 canonical `submit_result.status` 建立第二份權威。check.outcome 只描述單項觀察，不能當 receipt。

### 4.3 Worker 接線與狀態

在原 frontmatter 增加以下欄位，依角色改 schema 路徑，保留其餘設定：

```yaml
result-contract:
  schema: schemas/verification-v1.json
  require-structured: true
```

- 四個 worker 依上表提交 structured_payload；原有必要 summary、findings 等 runtime 欄位照既有 schema 提供。
- maintainer 使用接受的 payload 協助交接；若未啟用來源驗證擴充，不宣稱內容已有 artifact attestation 或完整 evidence delivery 保證。
- verifier 保持 canonical status：所有必要驗證完成且通過才用 `success`；確認缺陷用 `failed`；環境／證據問題用 `partial` 或 `blocked`，不得用 `completed_with_gaps` 放行未完成的驗證。
- implementation 的 checks 是自述，不取代 verifier 或最後的 test/vet/lint acceptance。
- 不新增要求所有結果都為 pass 的 schema；不新增 `verify-spec`。
- 保留真正失敗的提交與修復語意。現行 assertion 只對成功結果執行；required structured payload 的合法性檢查與此分開，失敗結果也須提供可表達空 checks／gaps 的合法 payload。

Schema 無法自行把 check.outcome 與 canonical status 做跨欄位判斷；本輪不新增這類 runtime policy。Worker 指示、fixture 狀態對照與既有獨立 acceptance 各自驗證其責任，不宣稱 schema 能防止所有虛假自述。

### 4.4 必須覆蓋的測試

使用真實 bundled schemas、既有 compile／submit_result／repair APIs 與 fake model，驗證：

| Case | 預期 |
|---|---|
| 四個角色的合法 payload | 編譯／提交成功，hash 對應 canonical payload |
| 缺 payload、未知欄位、錯誤型別／enum、超長欄位 | 拒絕，回傳可定位欄位的錯誤，不發布接受結果 |
| check pass + 非零、fail + 0/null、unverified + 空 note | Schema 拒絕 |
| 真實命令失敗的 fixture + status failed | 合法提交；不改寫成 success／protocol error |
| 全部未執行 + blocked；部分未執行 + partial | 合法提交，保留 unverified 與 gaps |
| 第一次錯誤 payload，repair 後更正 | 在既有 result-only repair 路徑完成，不重跑寫入命令 |
| payload 宣稱 checks pass，但 acceptance command 確定失敗 | Run 不被接受；worker 自述不得略過 acceptance |
| schema 漂移與 session/replay | 新舊 fixture 的契約不同時 resume 拒絕；保留 frozen schema 的舊事件仍按原契約處理 |
| tools／side-effect／recovery／delegation 比較 | 與 AT-00 相同，只有預定 result-contract 改變 |

先參考 `result_contract_validate_test.go`、`protocol_repair_evidence_test.go`、`bundled_team_compat_test.go`；只補未覆蓋的 bundled-schema 接線與整合案例。不要新建 JSON Schema parser 或 repair loop。

## 5. AT-04：Context Artifacts 邊界與使用說明

### 5.1 交付與範圍

- 修改／補充 `internal/team/context_artifact_service_test.go`、`context_artifact_capture_test.go`、`context_artifacts_policy_test.go` 中缺少的邊界案例。
- 在 `.agent-teams/hufu-dev/README.md` 新增使用說明；檔案若不存在則建立。說明設定、限制與 fixture 測試入口，不加入實機操作待辦。
- 測試用臨時 team copy 啟用 context-artifacts，保留 AT-02A 的 schema 與角色授權；不修改 bundled team.yaml 的預設值。
- 不修改 offloader／bash／artifact store 的行為；新測試若發現可重現核心缺陷，回報精確案例，不在本項擴張 runtime 修復範圍。

測試政策固定為現有值：MinBytes=51200、PreviewBytes=8192、MaxArtifactBytes=1048576、MaxReadBytes=32768、MaxArtifactsPerAttempt=32。

### 5.2 正確的現有行為

設 n 為完整、已遮罩、包含 framing 的工具結果 byte 數。成功發布還要求合法 UTF-8、已授權 view、支援的工具／執行路徑、durable journal 與剩餘配額。

| 輸入條件 | 預期 |
|---|---|
| n <= PreviewBytes | 不 offload；即使既有行長限制會截斷，也不保證完整保存 |
| PreviewBytes < n <= MinBytes，且既有輸出會截斷 | 可以 offload；不能斷言「低於 min 一律無 artifact」 |
| PreviewBytes < n <= MinBytes，既有輸出不會截斷 | 維持原輸出，不 offload |
| MinBytes < n <= MaxArtifactBytes | 條件滿足則 offload，經授權 view 可重建完整遮罩內容 |
| n > MaxArtifactBytes 或配額用完 | 回退既有有界輸出，不提供虛構的完整 artifact ref |
| 保存或 journal append 失敗 | 回退既有有界輸出，不暴露未接受的 reference，不重跑工具 |
| artifact 讀取越權／缺失／損壞 | 拒絕該讀取，不以其他路徑補造內容 |

`bash` 既有截斷還包含 2000 lines／50 KiB／單行 2000 runes。不能將其他 preview 的 500-rune 限制當成 bash 單行限制。

上述 fallback 不會自動讓整個 task 失敗；本輪不新增此行為。文件須明示：若必要驗證內容已截斷而不可取得，worker 應回報 unverified／partial／blocked，不得聲稱已讀完整證據；這是指示，不能宣稱已新增 runtime 強制 gate。

### 5.3 測試與驗收

- [ ] 大小比較使用完整 capture，覆蓋 PreviewBytes／MinBytes／MaxArtifactBytes 的等於與相鄰邊界，避免 exit-code footer 使案例落錯區間。
- [ ] 覆蓋小於 preview 的長單行截斷，以及大於 preview、低於 min 的 legacy truncation offload。
- [ ] 合法 offload 內容由 production view 分段重建，與完整遮罩 capture 的 bytes／digest 一致；包含頭、中、尾標記。
- [ ] non-zero exit／IsError／exit footer 不因 offload 改變；工具僅執行一次。
- [ ] 無 view、closed tool sequence、asserted result-contains 與不支援的 source 不啟用 offload，不擴權。
- [ ] 配額、保存失敗、append 失敗、越權讀取、缺失與損壞各保留既有拒絕／fallback 語意。
- [ ] context-artifacts 設定漂移會被 snapshot 拒絕；不假設程序重啟必能重建 attempt-local refs，未有既有恢復證明時明示 unavailable。

`TestBashOutputOffloadEndToEnd`、`TestContextArtifactFailuresFallBackWithoutReference` 等已覆蓋的案例直接沿用。實際 Codex／Playwright 輸出不在本輪測試或相容性承諾內。

## 6. AT-05：歷史回顧指南、模板與合成示例

### 6.1 交付

新增 `docs/guides/agent-team-retrospective.md`，依永久文件格式填入 Status、Authority、Verified-Commit、Supersedes、Superseded-By。候選模板與已填寫示例直接附在同一指南中，不另建 helper、CLI、資料庫或自動 promotion 流程。

模板必含：

- 候選識別、`draft` 狀態與適用 team。
- run／task／source-event IDs、event hash、branch lineage、已知 source revision；未知值明示 unavailable。
- 觀察、重現步驟、反例、coverage gaps；分開標示 worker 自述與 canonical 驗證。
- 建議的 team／schema／skill 落點、預期效益、風險、fixture 驗證方式及回滾方法。
- 「未核准、未套用」標記；不偽造 approver、核准日期或真實成效。

### 6.2 指南及 fixture 要求

- 指南解釋既有 `results list/search/show` 與 `result report` 的用途、branch 範圍、缺快照時 unavailable，以及不可信報告內容只能當資料。
- CLI 語法以現有 command help／測試確認；示例輸出使用固定合成資料並醒目標記，不能假裝出自真實 run。
- 如示例需要匯出報告，從既有 results_report test helper 在臨時 SQLite 建立 run/report；不執行真實 results query 或掃描現有 registry。
- 正反例至少包含可追溯完整快照、舊 run 無快照、同 workspace 不同 branch、worker 自述無 receipt。以既有 `cmd/hufu/results_report_test.go`、`internal/team/run_report_test.go` 優先對照。
- 不解析歷史自然語言自動提出政策；不讀真實 workspace，不修改 context.sqlite、Skills 或 team policy。

### 6.3 驗收

模板無外部資料即可填寫；合成示例的 ID／hash／狀態彼此一致且可由 fixture 重建；缺失資訊不以目前配置／其他 run 補齊。測試已覆蓋時不新增同義案例。人工審查與套用不列為本輪完成條件。

## 7. AT-01：Semantic Retrieval 的離線契約驗證

不修改 hufu-dev 或其他 bundled team 的 semantic-retrieval 設定，不執行 model build/pull，也不承諾召回品質或 latency 改善。

### 7.1 工作與驗收

以 `semantic_route_test.go`、`semantic_wiring_test.go`、`semantic_runtime_test.go` 及 `internal/semantic/config_test.go` 的 fake encoder 為起點：

- [ ] 相同合成 memory／scope／query：off 與 shadow selected IDs 一致；shadow 可以留下 trace，但不更改注入選取。
- [ ] auto 根據實際 retrieval query 的既有非 ASCII token 比例規則與 0.15 門檻路由；不按使用者語言標籤猜測，也不更改門檻。
- [ ] 純英文 lexical route 與 off 的 selected IDs 一致；private memory 只在授權 scope 可見。
- [ ] fake encoder 的 warming／busy／timeout／profile mismatch／projection incomplete 保留既有 lexical fallback 與 content-free reason。
- [ ] 不可用模型不觸發下載；no-net／遠端端點的既有拒絕契約維持。
- [ ] snapshot 語意依現有實作：off／shadow 不加入 selecting semantic hash；active／auto 才會固定該選取政策。不得要求所有模式切換一律產生 policy drift。

已由 `TestExecutionPolicySnapshotPinsOnlySelectingSemanticRetrieval` 等覆蓋的項目只記錄結果。缺少案例才補測；不為離線測試新增 encoder adapter、評分器或新 benchmark 系統。

## 8. AT-06：既有研究／審查契約的缺口補測

### 8.1 範圍

僅修改相鄰 test／fixture；保留 web-research 的 semantic active、required-workers-accept-gaps 與 code-review 的唯讀、no-net、fresh-session、VERIFY 隔離、snapshot-only source。不得擴張 coding acceptance 接受 gaps。

### 8.2 覆蓋要求

- [ ] completed_with_gaps 只有在已完成工作且 team 明示允許時滿足 required worker；partial／blocked／failed 不被當成功。
- [ ] 必要階段未執行時，其他成功結果不能補造完成狀態。
- [ ] 合成 tool transcript：成功取得內容、引文 mismatch、工具 error、無可用內容維持既有不同來源分類。
- [ ] 無 browser evidence 的 search snippet 不升格為已驗證引文；使用 fake tool transcript，不啟動 Playwright。
- [ ] 既有 evidence payload/hash／雙方 files_read／缺少 evidence_from 的拒絕案例保持成立；不為其他 team 加接線。
- [ ] immutable report 缺少所需證據時保留 coverage gap，不以 reviewer 數量或自然語言摘要推論無缺陷。

優先引用 `acceptance_required_workers_test.go`、`result_contract_evidence_test.go`、`result_contract_references_test.go`、`task_evidence_delivery_test.go` 與 reviewgate／reviewprep 既有測試。去重 URL 的語意規則、真實 claim accuracy、歷史比較成效不屬於本輪新功能；不新增對來源文字的 heuristic parser。

## 9. AT-09：無外部服務的整合與交付

### 9.1 整合案例

使用臨時 hufu-dev team copy、AT-02A 的實際 schema、fake workers／encoder 與臨時 event/artifact stores。測試走既有協調、提交、驗證與 persistence 路徑，不以孤立 schema.Validate 取代整合：

1. Specialist 接受合法 payload，implementer 提交合法修改描述，verifier 提交完整成功結果，客觀 acceptance 通過。
2. Verifier 確認 FAIL 可提交，run 不被接受；blocked／partial 不被提升為完成。此處不新增 hufu-dev 自動 redispatch 保證。
3. 錯誤 payload 被拒後 result-only repair 成功，原寫入動作不重播。
4. 相同案例分別關閉／啟用臨時 context-artifacts，僅切換一個因素；成功保存時可完整讀回，fallback 不虛構 reference，也不改原命令成功／失敗。
5. Session/checkpoint／event replay 保留當時 result-contract hash、payload hash 與 task status；不同契約的 resume 拒絕。
6. fixture 回滾：還原舊 team 定義，在新的臨時 session 編譯／執行；舊事件及 artifact 不覆寫、不刪除、不套用新 schema。

Fixtures 可以直接呼叫既有 Go API／fake model，無須呼叫真實 CLI 模型路徑。將新增整合案例放在 `internal/team/hufu_dev_result_contract_test.go` 或相鄰整合測試檔，不新增專用 launcher。

### 9.2 必要檢查

任何 schema、team 定義、測試或程式碼改動，完成前執行下列檢查，保留各指令 exit code；一項失敗不應遮蔽其他獨立檢查結果：

```bash
go test ./... -count=1
go vet ./...
golangci-lint run
go build -o /dev/null ./cmd/hufu
bin/check-docs
git diff --check
```

- 重跑 AT-00 的五團隊 validate/lint/explain，使用本次 checkout 重新建置的 binary。
- 新增或修改 team runtime 整合測試時，對相關 package 執行 `go test -race ./internal/team`；依實際變更縮小其他 race 檢查，不把無關全量 race 當額外範圍。
- 若有改動隱藏目錄的 Go package，另外明確執行其 go test／go vet／golangci-lint；根目錄 `./...` 不包含 `.agent-teams/`。
- 跨平台與 CGO-free 驗證沿用現有 CI 工作，不新增 runner／憑證配置；只能報告已取得的結果，不能把尚未執行的 CI 算通過。
- 本計畫已移至正式 docs 目錄，須通過 `bin/check-docs`；另檢查 fences、工作依賴與無過期路徑。只有文件修訂時不要求 Go lint gate。

### 9.3 完成定義

- [ ] AT-00、AT-02A、AT-04、AT-05、AT-01、AT-06 均有成果或既有覆蓋證據，無未定義的必要設計／外部輸入。
- [ ] hufu-dev 四個 worker 綁定正確 schema，負面／不完整結果能合法提交，blocking acceptance 不變。
- [ ] AT-09 的 fake-model 整合案例通過，schema/hash/status 的保存及重播一致。
- [ ] 文件準確區分 typed payload、worker 自述與 canonical receipt；不聲稱已新增 evidence attestation 或 revision-bound PASS。
- [ ] Context-artifacts 的小輸出截斷、大小上限、配額與發布失敗限制均有案例或既有測試證據。
- [ ] Semantic retrieval 僅離線驗證，bundled mode 與真實服務不變；真實歷史未被讀取。
- [ ] test/vet/lint/build 與適用文件／race／隱藏 package 檢查通過，既存問題與未執行檢查如實列出。
- [ ] 交付差異檔案、before/after、測試指令與 exit code、base SHA／工作區 diff、fixture 回滾方式與剩餘限制。

## 10. Coding Agent 開始提示

> 依目前 `docs/implementation-plans/agent-team-quality.md`，先完成 AT-00，再按第 2 節順序處理保留項目。這次只做本地 repository 工程工作；用 fake models、固定 typed fixtures 與臨時 workspace，不呼叫真實 LLM／embedding／browser、不讀真實歷史、不遷移資料、不部署。保留 single-writer、原權限、blocking acceptance 與 canonical status。只新增未被現有測試覆蓋的案例，不修改泛用 runtime 行為、不加入 evidence／revision 擴充。每項完成後執行相應 focused checks，最後按 AT-09 驗證並交付工作區差異與結果。保持本計畫位於正式目錄；先提交計畫與 baseline，之後每完成一階段並驗證即 commit 一次，不 push/merge/部署。

階段狀態及驗證紀錄見 [實作進度](agent-team-quality-progress.md)。

## 11. 本地核對入口

以下連結描述現有能力，執行前重新核對 HEAD；計畫中尚未新增的路徑僅以 code 標示。

- [hufu-dev team](../../.agent-teams/hufu-dev/team.yaml)
- [hufu-coding 初始批次與失敗語意](../../.agent-teams/hufu-coding/team.yaml)
- [結果合約與 context-artifacts reference](../reference/agent-format.md)
- [Semantic retrieval reference](../reference/semantic-retrieval.md)
- [歷史結果指南](../guides/historical-results.md)
- [Result schema 編譯](../../internal/team/result_contract_compile.go)
- [Payload 驗證](../../internal/team/result_contract_validate.go)
- [Assertion admission](../../internal/team/task_result_contract.go)
- [Evidence source admission](../../internal/team/task_evidence_sources.go)
- [Context artifact service](../../internal/team/context_artifact_service.go)
- [Bash output framing](../../internal/tools/bash_output.go)
- [Bash truncation](../../internal/tools/truncate.go)
- [Semantic snapshot 測試](../../internal/team/semantic_wiring_test.go)
- [Bundled team compatibility](../../internal/team/bundled_team_compat_test.go)
- [歷史報告測試](../../cmd/hufu/results_report_test.go)
