# Runtime 有限語意判斷統一決策實作計畫

> Status: active
> Authority: reference
> Verified-Commit: `134ad9546fb7efde7d69328167b73f04e2ddfbc8`
> Supersedes: —
> Superseded-By: —

> 狀態：已實作並通過離線驗收；固定集合、統一入口與共用設定均已交付。
> 日期：2026-10-21。
> 核對基準：`kjelly/hufu`，`279e762e02d8037ee12624385a4c1379acbaa279`；保留既有工作樹修改，未重做已完成能力。
> 驗收範圍：coding agent 可獨立完成的程式、配置、文件及離線測試。不要求真實 provider、憑證、人工標註或實跑品質比較。
> 使用者已確認：對 `hufu-code-review` 的 typed-finding critic 啟用；`no-net` 不禁止模型或 System One 的推論連線，仍禁止工具／子程序的網路能力。

## 1. 交付目標與排除範圍

讓 runtime 固定來源與 finding 清單、發出 row key、取得有來源綁定的有限選項決策，最後組裝並驗證 canonical payload。Worker 只負責閱讀證據、提出語意判斷所需的工具觀察，以及提交 rationale、evidence、missing_evidence 等非權威說明。

新增通用 opt-in schema extension，沿用 `decision_primitive`、catalog、事件 journal、receipt、結果驗證及 recovery；不新增資料庫、scheduler 或另一套驗收系統。領域選項及映射留在 team schema／catalog。

本次實際啟用 `.agent-teams/hufu-code-review/` 的 `critic-review` 契約。文件來源審查、no-op review、synthesizer 的語意流程不接入新決策模式；它們只做必要的契約相容性驗證。

以下移出本計畫，不能成為實作待辦或完成障礙：真實模型比較、準確率與成本改善承諾、人工判讀、模型選型實驗、部署／操作現有 run、批次遷移舊 workspace、決策自動修正或學習、決策上下文的模型摘要、通用 decision DAG、同一槽位反覆推論選答案。不要為這些工作建立佔位介面。

## 2. 變更前盤點與重用入口

本表記錄核對基準的狀態；實際完成範圍與相容性見第 9、12 節。

| 入口 | 已有能力及本次注意事項 |
|---|---|
| [result_contract_inputs.go](../../internal/team/result_contract_inputs.go) | 驗證 accepted evidence 的 task ID、hash 及 value bindings；目前是比對模型提交值，不是自動注入整份 input_review |
| [result_contract_collections.go](../../internal/team/result_contract_collections.go) | 固定 membership、順序、索引、含來源 hash 的 row key，以及 canonical array 組裝；目前沒有 catalog 決策結果注入 |
| [result_collection_context.go](../../internal/team/result_collection_context.go) | Worker manifest 與提交 schema projection；新增 runtime-owned 欄位必須同步排除 |
| [result_contract_validate.go](../../internal/team/result_contract_validate.go) | 本地／外部結果入口、canonical validation、evidence 處理與 payload hash；不能只修改 submit_result 的單一路徑 |
| [decision_primitive_tool.go](../../internal/team/decision_primitive_tool.go) | 授權、開始／結算事件、scope、receipt、replay、call limit；目前 context 由 Worker 提供，budget 檢查早於 replay |
| [catalog](../../internal/decisionrt/catalog/catalog.go) | immutable catalog、agent grants、scalar inputs、backend、版本與 publication validation |
| [validate.go](../../internal/decisionrt/validate.go) | 目前最多 21 個選項、單一 context string 4096 bytes、總 canonical context 64 KiB；本次不放寬這些限制 |
| [critique-v1.json](../../.agent-teams/hufu-code-review/schemas/critique-v1.json) | 尚未接入 fixed collection；有 index／數量及 evidence 條件，目前要求至少一個 finding；沒有完整 verdict／severity 組合限制 |
| [team.yaml](../../.agent-teams/hufu-code-review/team.yaml) | `critic-review` 使用一份 accepted evidence；team `no-net: true`，critic 同時服務 typed-finding 與文件審查 |

開始實作時重新記錄 HEAD、工作樹及適用 AGENTS.md。保留既有 protocol repair、wrap-up、binding 修正；新功能不得重做其恢復機制。閱讀 execution-runtime、decision-primitive、agent-format 的相關權威章節。

## 3. 已決定的設計

### 3.1 一個決策群組產生 verdict 與 severity

不分別執行兩次獨立推論。新增通用 decision group：一次 catalog `kind: choice` 的結果，透過 team 宣告的固定映射注入一個或多個 string enum 欄位。

`hufu-code-review` 使用 `assessment` 群組，輸出 `verdict` 與 `severity`。catalog 使用四個 verdict 與三個 severity 的 12 個組合，例如 `confirmed_error`、`downgraded_warning`、`unverified_info`。目前 schema 允許的組合全部保留，不自行發明「rejected 必須 info」或原始 severity 與 downgraded 之間的新規則。各選項的語意說明由 team catalog 提供，沿用 critic 現有審查要求。

映射只是 trusted configuration 的機械展開，不是 runtime 對 prose 的解讀。所有選項及 group ID 使用小寫 snake_case。完整結果仍須通過既有跨欄位與 reviewgate 驗證；enum 合法不代表判斷正確。

### 3.2 Schema 配置形狀

在既有 `x-hufu-fixed-collections` 每個宣告新增 optional `decision_groups`。以下為形狀示例，不是可單獨載入的完整 schema：

```json
{
  "items_pointer": "/record/decisions",
  "source_contracts": ["schemas/review-v1.json"],
  "source_scope": "evidence_from",
  "source_pointer": "/record/findings",
  "index_field": "finding_index",
  "choice_fields": ["verdict", "severity"],
  "decision_groups": [{
    "id": "assessment",
    "catalog_entry": "critique_assessment",
    "outputs": ["verdict", "severity"],
    "value_map": {
      "confirmed_error": {"verdict": "confirmed", "severity": "error"}
    },
    "context": {
      "mode": "observed_artifact_reads",
      "max_bytes": 49152
    },
    "on_abstained": "blocked",
    "on_error": "blocked"
  }]
}
```

實際配置的 `value_map` 必須包含 catalog 的全部 12 個選項。第一版固定只支援 `observed_artifact_reads`、`blocked`；這些固定詞彙使用 enum，禁止加入尚未實作的 fallback 選項。

Load／task admission 時，推論之前必須拒絕：

- 未知欄位、重複 group、重疊輸出、輸出不是 required string enum choice field、覆蓋既有 fixed field。
- catalog 不存在、不是 choice、選項無法與 value_map 一對一對應、映射值不符合目標 enum。
- 選中的 catalog entry 配有 `block-on`；本模式的結果組裝與該 worker 中止機制不能混用。
- context 輸入宣告不符合 3.4 的固定形狀、大小超限、actor 沒有 grant、工具／phase／unattended／force-mcp 政策禁止使用。
- 執行 backend 無法提供本模式的 durable artifact-read observation 或 runtime-owned 決策組裝。必須在執行前明確拒絕，不退回由 Worker 重抄決策。

編譯器保留 canonical schema，另產生 Worker submission projection；後者移除 fixed fields 與 group outputs，避免 required／conditional schema 要求 Worker 提交 runtime-owned 值。最終條件只在組裝後的 canonical payload 上判定。

### 3.3 固定列與來源注入

固定列沿用現有 SourceContracts、EvidenceFrom、source hash 與 row key。不得新增從 goal 字串挑來源的程式。

在 `x-hufu-evidence-inputs` 增加 optional `runtime_owned: true`。啟用時 runtime 依 durable EvidenceFrom 的順序，從 accepted payload 產生 ItemsPointer 的 entries，填入 TaskIDPointer、HashPointer 與 ValueBindings 指定欄位；不改寫 CompletePointer。為免猜測，第一版只支援每份 evidence 一個 entry、現有可明確組裝的 object paths；路徑衝突與未能由這些 bindings 供應的 required entry 欄位在編譯時拒絕。

Worker 必須省略整個 runtime-owned ItemsPointer；即使提供完全相同的值也拒絕。採用 false／省略的舊契約仍維持原有比對行為。來源的契約、accepted status 與 payload hash 在注入前和接受結果前都必須驗證。

新 critic 契約啟用此模式，保留 `audit_complete: true`，由 runtime 注入 `/record/input_review`，並對 `/record/decisions` 接入固定列。零個 finding 的 accepted review 產生空 decisions，不呼叫模型；沒有 accepted source 則失敗，不能視為空集合。

### 3.4 決策上下文與證據

新模式的 Worker 呼叫只包含 `collection`、`row_key`、`group`，不接受 context、catalog name、task ID、hash、finding index、receipt 或預期答案。以互斥參數模式擴充既有 `decision_primitive`；舊 `{name, context}` 模式維持相容，但不能替本模式填入 settled 決策。對 bound task 的 group catalog entry，拒絕使用舊模式繞過綁定。

Runtime 組成 canonical context envelope：

1. 從 accepted source payload 解析的完整 finding，標示為來源主張，不把主張本身當證明。
2. 該 task occurrence 在首次呼叫此槽位前成功取得的 authorized `view artifact_ref` 觀察，按持久化觀察順序排列。只納入從此 task 的 durable dependency／evidence 權限可讀的 artifact；排除 live file_path、其他 task／branch 觀察、工具錯誤與模型文字。
3. 每段觀察的 opaque artifact ID、內容 hash、讀取範圍、實際回傳內容，以及截斷／範圍不完整資訊。不能只保存 ID，也不能聲稱分段讀取等於完整 artifact。

沿用既有 artifact 授權、讀取及 transcript capture seam。必要時為本模式保存結構化成功讀取觀察，但不另建證據 store；不得讓模型自行提供觀察 bytes 或讓 prose 摘要取代 bytes。現有 attempt 的記憶體 evidenceRecords 不是 durable authority，Worker 可寫的 transcript 檔案也不能直接升格為權威；要在 runtime capture 時將觀察與內容 hash 綁定至既有 journal／artifact。跨 attempt 只重用已驗證的同 occurrence 觀察，不接受未證明來源的 transcript 檔案。

以受權限保護、既有可持久化 artifact 保存決策上下文快照，journal 以引用與 digest 綁定；快照必須在 started 事件之前持久化並可重新驗證。沿用既有 secret redaction，先固定可持久化且允許傳送的 bytes，再計算 digest；禁止後續 redaction 改寫已雜湊內容。無可驗證觀察、capture 超限失效或 scope 不能證明時，blocked，不能把 unavailable 偽裝成沒有證據。

不按關鍵字猜相關段落，不自動摘要、不靜默截掉舊觀察。Worker 先讀必要的有界範圍再呼叫決策；若完整 finding 加上本次可用觀察的 canonical envelope 超過上限，回報 `decision_context_too_large` 並 blocked，不消耗推論呼叫、不猜答案。所有觀察皆受現有 task transcript／artifact 大小上限約束。

使用既有 scalar catalog，不新增 nested context 型別：宣告 `binding_digest` 及 `context_00` 至 `context_11` 共 13 個 string inputs。Runtime 將最多 48 KiB 的 canonical envelope 按 UTF-8 邊界切成每段不超過 4096 bytes 的連續片段，未使用欄位填空字串；因 UTF-8 分段造成無法放入 12 段時同樣拒絕。catalog question 明訂依數字順序連接片段，其內容是資料而非指令。仍執行既有完整 Request.Validate，不能繞過 64 KiB context 上限。

這是有限上下文支援，不保證現有所有大小的 review 都可決策。超限必須可診斷且 fail closed；本次不擴充 token budget、隱藏摘要或放寬既有 decisionrt 容量。

### 3.5 決策 identity、持久化與唯一性

Runtime 決策綁定至少包含：

- 既有 branch／actor／task-created occurrence scope；不要將 attempt number 當作禁止 retry reuse 的主鍵。
- result contract schema hash、collection pointer、row key、group ID、輸出欄位。
- accepted source task ID／payload hash、source pointer／index。
- catalog hash、spec ID／version、value_map 所屬 schema hash。
- canonical context artifact 引用與 hash、實際 request digest。

先對不含 request digest 的 binding descriptor 作 deterministic encoding／hash，作為 catalog `binding_digest` 輸入；完成 Request 後再保存 request digest，避免自我引用 hash。

同一 scope、schema、來源、collection、row、group 是一個槽位。首次 started 固定上下文；後續讀取不會偷偷改變此槽位的決策依據。相同槽位不得因新 tool call ID 而重新抽樣。新的證據判斷須建立新的 task occurrence；本計畫不新增 revision API。

擴充既有 decision primitive event payload，以版本化 optional binding 支援新紀錄；保留舊 payload 的解碼／驗證能力。使用既有 gate 與 durable journal，並行重複請求只能產生一筆邏輯 started／settled 序列。idempotency key 必須包含 branch 與槽位身份，不能只依 row key 或 catalog name。

settled 結果由已驗證的 journal 讀取，重新驗證 scope、binding、request digest、catalog publication 與 receipt；不能接受外部送入的 receipt。模型結果與輸出映射都是 journal 可追溯的引用，ResultPayload 的 provenance metadata 由 runtime 產生，不由 Worker 提交。

### 3.6 結果組裝與不變性

順序固定為：驗證 durable sources → 注入 evidence inputs → 查詢並驗證每個槽位的 settled 決策 → 展開輸出映射 → 合併 Worker 的非權威欄位 → 驗證全部 canonical schema／evidence／跨欄位規則 → 產生最終 canonical bytes 與 hash → 既有結果接受流程。

Worker 每個 row 仍提交 rationale、evidence、missing_evidence；不得提交 verdict、severity、finding_index 或 receipt。缺漏、重複與未知 row key 一律拒絕，不自動補充理由或 missing evidence。

既有 evidence 正規化／降級處理不得靜默改寫 decision-owned 欄位。如果處理結果會使它們與 settled mapping 不同，拒絕結果；不能用 schema 驗證後的另一個答案冒充原決策。合法 `unverified` 必須是 catalog 的 decided 選項且具有合法 missing_evidence；abstained 或錯誤不轉換成 unverified。

接受時重驗 source，防止推論期間來源改變。使用既有 occurrence lease／結果提交交易邊界處理競態。結果回讀／replay 必須驗證 provenance 與注入值一致；不能只檢查陣列長度或 payload 自身 hash。

### 3.7 失敗、預算及恢復

| 狀況 | 規則 |
|---|---|
| decided | 保存並重用 immutable 結果；不再推論 |
| abstained | 保存結果並 blocked；不填入業務答案，不自動重試 |
| timeout／provider error | 保存既有明確 error category 並 blocked；同一槽位不自動重試 |
| canceled | 保存可取得的結算事實，沿用 task cancellation，不偽造成功或改成普通 partial |
| source／context／catalog／schema 不符 | 拒絕使用，回報 binding／contract drift；不重新解讀同名 row |
| started 存在但無 settled | 視為未能確定結果的 interrupted 槽位並 blocked；不假設 provider 沒被呼叫，不自動重跑 |
| budget／call limit 耗盡 | 禁止新的 inference；仍允許只讀有效 settled 紀錄及結果組裝 |
| result validation failed | 修正非權威欄位；若原因需要新決策，既有 task blocked，不用 result-only repair 偷跑工具 |

只對新模式保證上述槽位語意；不要順便改掉 legacy `{name, context}` 的錯誤重試規則。沿用既有 budget、timeout、call limit、token accounting 與 recovery category；新穩定錯誤分類接入既有 recovery／anti-thrashing，不以任意錯誤字串作 fingerprint。

Retry／resume 在同一 branch、相同 occurrence 與完整 binding 下重用 settled；部分列完成時只在正常執行模式處理尚未 started 的列。result-only repair 完全沒有 decision_primitive 推論能力，只讀 journal。跨 branch（包含 fork）不套用舊槽位，即使來源 hash 相同；舊 branch 的既有結果保持可讀。

### 3.8 no-net 與模型連線

依使用者決定，`no-net` 針對工具與子程序的網路能力，不阻止模型推論。通用調整 decision primitive 的靜態 grant、工具組裝與執行期 policy gate：team／agent／CLI 的 no-net 不再單獨拒絕 sidecar 或 systemone inference；rule backend 也不因此被拒絕。

sidecar 沿用 provider boundary、credential、request admission 與 usage accounting。systemone 使用受信任 catalog 中凍結的 endpoint／model／credential binding 及既有 transport；模型不能從 tool 參數指定或覆寫。保留 transport 與 host 層網路政策，不為測試或執行申請額外網路能力。

仍強制檢查 explicit tool grant、catalog actor grant、denied-tools、phase、unattended 與 `force-mcp`；後者維持原有禁止行為。不能直接跳過整個 central policy gate。bash、fetch、download、MCP 及其他工具的 no-net 行為不變。禁止加入 team-name 判斷。

## 4. hufu-code-review 的具體接入

1. 新增 `schemas/critique-v2.json`，保留 v1。v2 保持 canonical record 形狀，啟用 3.2／3.3 的來源注入、固定列及 decision group；允許 accepted review 的零 findings。移除已由固定列機制保證的 1–32 項枚舉式數量／索引條件，保留 schema 的上限、欄位與 evidence 約束。
2. `critic-review` 改用 v2，仍只允許一個來源。不要將文件審查契約切到 v2，不要求 synthesizer 呼叫 decisionrt。
3. team 新增 `critique_assessment` catalog，`backend: sidecar`，使用現有 sidecar model 解析方式，不新增 provider、端點或憑證。`version: "1"`、`kind: choice`、12 個上述選項、13 個固定 string inputs、`agents: [critic]`、`max-calls: 2048`、`timeout: 60s`；不設 fallback、block-on 或 confidence gate。max-calls 是既有 catalog scope 的上限，不是每 row 的重試次數；測試需核對實際累計 scope。
4. critic 工具加入 `decision_primitive`，並同步現有 phase／unattended 授權配置。工具在 bound `critic-review` occurrence 才允許該 entry：文件審查 occurrence 即使共用 critic actor，也不能呼叫此 entry。對被 decision_groups 引用的 entry，通用規則是只能從相符的 bound task 槽位呼叫；普通 legacy entry 維持原授權方式。此限制由通用 contract binding 判定，不在 core 寫 critic 名稱。critic 目前 `max-steps: 20` 不足以順序處理 32 個 finding，改為明訂的 `120`；此為共用 actor 的容量調整，文件審查語意與契約不變。不新增隱藏步數或無限重試。
5. 更新 critic 提示：先讀授權 artifact，再以 manifest 的 collection／row／group 呼叫；取得全部 decided 結果後提交其餘欄位。詳細說明 blocked、上下文超限、禁止重抄來源／決策，以及只見到部分證據時的限制。
6. 更新 team reviewgate、contract inventory、source schema references、schema fixture 與文件，使其接受 v2 的 canonical 輸出；不得為了讓測試過關取消原本 evidence／coverage 檢查。v1 的既有 fixture 與解析相容性保留。
7. 配置與 schema hash 納入既有 execution policy snapshot。新功能以新 session 使用；不 hot-reload 執行中 run、不自動遷移舊 session。舊 run 如遇政策漂移，沿用現有拒絕／要求新 session 行為，不放寬 snapshot 檢查。

## 5. Coding agent 工作包與依賴

| 工作包 | 可獨立驗收的交付 | 依賴 |
|---|---|---|
| WP-01 | 通用 no-net 模型推論語意、靜態／動態 gate 一致性、文件及 fake backend 測試 | 無 |
| WP-02 | evidence runtime_owned、fixed collection decision_groups 編譯與 schema projection、load／admission 拒絕測試 | 無 |
| WP-03 | durable read observations、bounded context artifact、槽位 binding、既有 tool 新參數模式、versioned journal | WP-01、WP-02 |
| WP-04 | settled 查詢、canonical 組裝、provenance、failure／repair／resume、既有結果入口一致性 | WP-03 |
| WP-05 | hufu-code-review v2 啟用、team catalog／prompt／reviewgate／fixture，以及未啟用模式回歸 | WP-04 |
| WP-06 | 完整離線驗證、架構／參考文件、最終 diff 與完成紀錄 | WP-05 |

本地 worker 必須完整支援。直接執行、extra-model、外部 backend 等入口要麼使用同一授權／組裝能力，要麼在 admission 明確拒絕不支援的組合；不可執行後才發現缺 journal 或觀察。未啟用新模式的入口維持原行為。dry-run／靜態 team 檢查不建立 provider 連線。

既有 session／JSON／report 的 decision projection 補上可追溯但不含證據內容的槽位、事件與 binding digest 欄位，保留 error category；TUI 若消費這些 projection，要使用同一資料來源。不要為此新增專用 dashboard 或複製 reducer。

## 6. 必要離線測試

所有模型結果由 fake generator／backend 提供；測試不得依賴本機 Ollama、真實 System One 或 API key。網路相關測試用 fake transport，證明政策選擇，不使用外部端點。

| 層級 | 必測情境 |
|---|---|
| 編譯與 admission | 欄位／mapping 缺漏、未知 catalog、選項不合、輸出重疊、group 重複、不支援 backend、缺 grant、force-mcp、phase／unattended deny；全部在 inference 前拒絕 |
| no-net | sidecar／systemone 可在 trusted inference 配置下執行；team／agent／CLI 設定一致；Worker 不能注入 endpoint；工具網路仍被禁止；denied-tools 與 force-mcp 仍有效 |
| 來源組裝 | input_review 全由 runtime 注入；相同值覆寫亦拒絕；hash 不合、非 accepted 來源、未知 contract、沒有來源、合法空 findings |
| 列覆蓋 | 缺漏、重複 JSON key、未知 row、錯 collection／group、索引／決策欄位覆寫；同 finding 出現在不同 collection 不碰撞 |
| 上下文 | 完整 finding、授權 artifact read bytes／範圍、非 UTF-8／超限／缺 artifact／hash 損壞、分段重建一致、非權威文字不混入；注入文字僅作資料 |
| scope／receipt | 偽造或錯配 receipt、錯 actor／task／branch、相同 row count 但 source hash 改變、spec／catalog／mapping／context 改變、binding metadata 與輸出不一致 |
| 並行／持久化 | 同槽位並行只推論一次；快照寫入前後、started 後、settled 後與提交前 crash；孤立 context artifact 不被當完成決策 |
| failure／recovery | abstained、timeout、provider error、取消、call limit／budget；技術錯誤不轉 unverified；started 未 settled 不自動重跑 |
| replay／repair | 同 scope settled reuse、部分列完成、預算耗盡仍能組裝已完成結果、repair 不呼叫推論、fork 不重用舊槽位 |
| canonical validation | Worker rationale／evidence 缺漏、unverified 沒有 missing_evidence、非 unverified 無 evidence、非法 enum／映射值、evidence normalization 想改 owned 欄位、hash 只對最終合法值產生 |
| 相容性 | legacy decision tool、舊 event payload、未啟用 schema、v1 canonical 結果、文件審查與 synthesizer 原有語意、外部入口明確拒絕不支援模式 |
| team 接入 | 分層驗證：通用 fixed-row fake provider／合成 artifact 覆蓋 0、1、32 findings 的決策、replay 與 canonical 組裝；真實 team 載入測試驗證 v2 schema、空集合來源注入與覆寫拒絕；隱藏 reviewgate／reviewprep 測試驗證交付契約。無須跑真實審查 |

注入文字測試只能驗證 transport／權限／資料邊界，不能宣稱 fake model 測試證明真實模型不受 prompt injection 影響。

## 7. 完成條件與驗證命令

- WP-01 至 WP-06 均交付，hufu-code-review 配置已切換到 v2，無待決的 schema／失敗策略／授權設計。
- 最終 diff 確認 `cmd/hufu`／`internal` 沒有 team 名稱、verdict 詞彙或 team 專屬網路例外。
- 更新 `docs/architecture/decision-primitive.md`、`docs/architecture/execution-runtime.md` 與結果契約參考文件，明確記錄 no-net 的模型連線語意、context 上限、blocked 行為、v1 相容性及新 session 要求。
- 每個工作包執行相關 focused tests；完成後逐一執行以下命令，全部成功才完成：

```bash
go test ./...
go vet ./...
golangci-lint run
```

- 另外從 `.agent-teams/hufu-code-review/reviewgate` 執行 `go test ./...`，使用其上層 repository module，並記錄結果；Go 的根目錄 `./...` 不應被當成涵蓋隱藏目錄的保證。對並行槽位與 cancellation 的 targeted tests 執行 race detector。
- 用臨時目錄與 fake backend 完成 integration tests；不讀寫執行中的 workspace、不對外發送 prompt、不啟動新實跑 session。
- 最終交付紀錄列出修改、實際驗證命令／結果及支援限制；只能宣稱 binding／coverage／組裝／replay 的離線正確性，不宣稱真實審查品質或成本已改善。

實際交付與驗證證據記錄於第 12 節。啟用 team、不使用真實模型驗收，以及 no-net 允許受信任 inference 的邊界均已由使用者確認。


## 8. 擴充範圍與使用者已決定的政策

2026-10-21 使用者要求將所有 runtime 有限語意選擇統一至 decision primitive。第 1–7 節保留為第一階段的固定集合規格；其中「本次只接入 critic」由本節擴充，但不將非有限生成、抽取或確定性規則交給模型。

已確認的產品決策：

- Multi-model judge 保留 graft：一次 primitive 選 winner，再一次明確計費的生成呼叫產生 rationale／graft。生成結果不能改變 winner。
- Skill matching 每個 skill 一個 relevant／irrelevant 槽位，明列呼叫上限與預算；不組合 2^N 個選項，也不截掉候選。
- Skeptic 無有效判斷時 fail closed。Plan review 耗盡時不能自動通過；保留原本明確人工批准邊界。盤點確認原程式 `forceApprove` 實際是人工確認，不是無條件批准。
- Control `off` 走統一 primitive；未設定共用預設時仍使用 sidecar；`active` 使用配置 backend，技術錯誤不再追加 legacy 推論；`shadow` 在配置載入時拒絕，提示改 active。舊事件及結果保持可讀。
- Primary decision engine 的混合契約保留原生成呼叫供連續分數／文字，再對影響決策的有限欄位執行明列預算的 primitive。生成中的舊有限欄位不直接生效；決策綁定 sealed evidence 與該生成結果。
- 模型沒有校準信心時不得填入虛構信心值。保留 public confidence 欄位形狀時，明訂 unavailable 表示與內部決策接受訊號，不能再把缺信心誤判為 primitive 未決定。

## 9. 入口盤點與遷移清單

以下依實際輸入、輸出與消費行為分類，不依名稱。`有限` 的項目必須完成接入；`生成／抽取` 與 `確定性` 項目不進行另一輪推論。所有 F 項的 production 呼叫點已改用 primitive；第 12 節記錄最終離線驗收結果。X 項保留原有生成／抽取或確定性行為。

| ID／入口與呼叫路徑 | 分類／現況 | 權威輸入、合法輸出與用途 | 失敗／恢復及處置 |
|---|---|---|---|
| F01 `decision_primitive_tool.go` → catalog | 有限；已接入 | trusted catalog＋runtime actor；choice／bool／integer，僅提供判斷 | 重用既有工具，不加第二次推論；補新 binding 與 replay 缺口 |
| F02 `result_decision_runtime.go` → critic-review | 有限；已接入；已驗證 | accepted findings＋artifact reads；12 組 verdict／severity | blocked、來源失效拒絕、partial／settled 重用；runtime 注入 |
| F03 `cmd/hufu/autoteam.go:autoSelectTeam` → `Sidecar.MatchTeam` | 有限；primitive | accepted prompt＋完整 team summaries；候選 team 或無選擇 | 未決回 picker；單一候選仍確定性；已接入 request snapshot binding |
| F04 `cmd/hufu/router.go:Route` → `Sidecar.ClassifyRoute` | 有限＋確定性 override | prompt；fast／team；explicit flags／team 不呼叫模型 | 失敗保留 team 安全預設；已接入 request snapshot binding |
| F05 `control_decision_points.go:selectAgentForGoal` → `Sidecar.SelectAgent` | 有限；統一 primitive／control policy | goal＋完整 eligible worker 集合；一個 worker 或 abstain | 無選擇要求明確 agent；已移除 legacy 推論與隱藏 fallback |
| F06 `reviewGuardCall` → `Sidecar.ReviewToolCall` | 有限語意 guard；最終授權仍確定性 | actor/tool/args/rules；approved／denied；提供 guard 判斷 | 技術錯誤／棄權拒絕，不繞過 central policy；已接入 state snapshot binding |
| F07 `reviewPathAccess` → `Sidecar.ReviewPathAccess` | 有限語意 path 分類 | 完整 command＋path；file_access／not_file_access | 未決保留 path 交確定性 consent／path policy；已接入並通過離線驗收 |
| F08 `chooseAskUserResponse` → `Sidecar.ChooseAskUserResponse` | 有限選項回答；自由文字另屬生成 | runtime 問題＋options；單選或明確多選 | 無可靠答案回 ErrAskUserAbstained；移除格式錯誤的隱藏二次推論；封閉多選列舉完整子集；mixed／allowAny 已確認排除 |
| F09 `coordinator_skills.go` → `Sidecar.MatchSkills` | 有限多標籤；逐列 primitive | prompt＋完整 skill 清單；逐 skill relevant／irrelevant | 任一失敗不宣稱完整匹配；持久化已完成列，不新增隱藏重試；已接入並通過離線驗收 |
| F10 `coordinator_taskcache.go` → `Sidecar.SimilarTask` | 有限；primitive | new task＋已經確定性 scope 過濾的全部 candidates；match／none | 未決不命中 cache；不可把超容量集合截斷送模型；已接入 request snapshot binding |
| F11 `coordinator_judge.go:judgeAgentResults` | 有限 winner＋自由文字 graft | goal＋完整成功候選內容；選 winner 後生成補充 | 未決沿用 merge fallback；選項與 graft 分離，後者不可覆寫 winner；已接入並通過離線驗收 |
| F12 `coordinator_skeptic.go:adversarialVerify` | 有限；每 lens 一次 primitive vote | goal／constraints／output／verify claim＋lens；refute／not_refute | 取消／無有效票不能當確認；多數決仍 Go 規則；已接入 state snapshot binding |
| F13 `coordinator_plan.go:planReviewer.review`、`plan_revision.go:reviewPlanRevisionWithReviewer` | 有限 approve/reject；primitive 直接選擇 | pending plan／revision digest、goal、agent capabilities、task state | 先 deterministic feasibility，再 primitive；人工 override 保留；不得在模型選擇階段執行工作；已接入並通過離線驗收 |
| F14 `decision_runners.go:RunJudge`／capability judge | 混合：連續評分／理由生成＋preferred_option 有限選擇 | sealed packet、judge identity、生成結果；packet 內 option | 生成證據與有限結果分離，拒絕未決；沿用 engine admission/store；已接入並通過離線驗收 |
| F15 `RunChallenge`／capability challenge | 混合：countercase 生成＋target_option | sealed packet、challenge stage、生成內容；已存在 option | primitive 指定 target，保留連續 severity；已接入並通過離線驗收 |
| F16 `RunRevision`／capability revision | 混合：revised scores 生成＋changed | 原 opinion、sealed packet、生成結果；changed／unchanged | 有限結果綁定原始及生成快照；不准舊 JSON flag 直接控制 reducer；已接入並通過離線驗收 |
| F17 `RunCoordinatorFinalization`／`RunJudgeFinalization` | 有限 option＋理由；primitive 與既有 engine 持久化 | sealed packet＋aggregates/challenges/revisions；packet options | 既有 finalization validator 最後決定是否合法；不重建 engine／store；已接入並通過離線驗收 |
| F18 `cmd/hufu/context_conflicts_cmd.go` → maintenance generator | 有限 memory relation＋理由 | scoped memory pair、內容 hash、judge policy；contradicts／duplicate／refines／compatible | 未決不變成 compatible；沿用 context judgment 與事件 outbox；已接入並通過離線驗收 |
| X01 coordinator `agent`／`finish`、一般 worker tool arguments | 生成／抽取＋確定性授權 | 開放式 task DAG、工具參數與結果；不是固定候選清單 | 不將任意工具呼叫再交模型審一輪；原 schema／policy 保留 |
| X02 `run_input_semantic.go`、consolidation／promotion draft、skill discovery | 非有限結構化抽取／內容生成 | 可變長 domain object、文件或 skill 內容；enum 屬整份生成契約的資料欄位 | 保留現有 schema、人類審核與驗證，不因欄位有 enum 就追加推論 |
| X03 `Summarize`／`Compact`／`CompactStructured`、reflection、fix／explain | 自由文字生成或非有限抽取 | 原始內容／錯誤 → 摘要、理由、建議 | 排除；不把摘要當原始 decision evidence |
| X04 primary option proposal／reference evidence／premortem | 非有限生成／抽取 | 建立候選、證據、連續 likelihood／impact 與 failure modes | 排除整份生成；已固定候選後的有限選擇屬 F14–F17 |
| X05 recovery／disposition／retry safety／stop policy／budget／route fallback | 確定性規則 | typed failure、receipt、attempt/state、明訂政策 | 排除；模型 reflection 不能變更 retry 權限 |
| X06 catalog grants、tool／phase／unattended／path policy、hash／coverage、驗收 | 確定性安全與正確性邊界 | 受信任配置、來源、結果及命令 exit code | 排除；semantic decision 不能授權行為 |

盤點需同時檢查 CLI preflight sidecar 與正常 coordinator；前者已有 workspace／event lineage，直接重用，不能另外建立 selection DB。Capability-routed primary stages 的外部生成結果同樣屬不受信任輸入，不能繞過有限欄位的 primitive binding。

## 10. 共用實作與遷移工作包（已完成）

- 只抽取實際使用的 request／state binding。輸入由 owning module 建立，包含完整 canonical request/state、候選順序及內容、版本、actor／phase、model/catalog policy identity；worker 不接受權威 ID、catalog name 或 receipt。
- 三種 binding 都使用既有 decision primitive started／settled journal 與 artifact store。建立共用執行／查詢 helper，保留固定列驗證，不能讓一般請求冒充 row settlement。
- Runtime semantic catalog 由 owning module 的明確決策宣告建立，動態候選從 runtime 快照注入並 hash；catalog 權限不得只信任模型輸入的 point 名。
- context 分段與總容量沿用第一階段有界實作；候選多於 21（包含業務 none 選項）就明確未決。逐 skill 判斷的每一列有獨立槽位，明確計入呼叫上限。
- 已結算且有效的 binding 可直接讀取，先 replay 再判斷新 inference 預算。started 未 settled 一律禁止隱藏重試；輸入／候選／政策／狀態改變使用新 binding，不套舊結果。
- Reason／graft 等生成內容明確標記為非權威；保留 public result 形狀。使用者核准的額外生成／有限判斷呼叫須逐一計費並在測試斷言次數。
- Result-only repair 不得呼叫 semantic executor；移除已遷移入口的舊 parser 推論 fallback。舊 parser 可保留用於讀取 legacy 結果及相容性測試。
- WP-07：完成盤點核對、共用 request／state executor、CLI preflight 綁定與投影。
- WP-08：F03–F10 遷移與離線測試；control off/active/shadow 的新語意與舊事件相容性。
- WP-09：F11–F13 遷移，驗證 judge graft 不改 winner、skeptic fail closed、plan 選擇不直接執行。
- WP-10：F14–F18 混合契約拆分、capability 路徑及 context conflict 判斷。
- WP-11：完整 go test／vet／golangci-lint、隱藏 team tests、targeted race、清單逐項核對及完成紀錄。

本節工作包同樣只做離線驗證。完成標準擴充為：所有 F 項均接入或具有使用者明確核准的排除；X 項保留原行為；不得以 TODO 或未被呼叫的介面宣稱遷移完成。

## 11. 共用 decision-runtime 設定（納入實作）

一般文字生成仍使用原有 model／sidecar-model。決策傳輸使用獨立區塊：

```yaml
decision-runtime:
  backend: systemone
  systemone:
    model: ollama/clef:cloud
    endpoint: http://127.0.0.1:11434/v1/systemone
    timeout: 60s
    # api-key-env: SYSTEMONE_API_KEY
```

endpoint 必須實際支援 System One 協定；模型名稱不推導 API 協定。離線測試只驗證解析與 fake transport，不聲稱部署中的端點支援任何模型。

解析器位於 `internal/decisionrt/settings`。全域／專案 hufu.yaml 按欄位合併，再由 team 覆寫；entry 明訂推論欄位優先。缺少 System One model 在 runtime admission 報錯，不猜測模型，不切換 backend。team parser 允許尚未取得上層配置的 transport 欄位延後解析，但 schema／options／grants 仍立即驗證。`sidecar` 不繼承 System One model、endpoint、credential 或 timeout。既有 control-decisions 明訂欄位優先於共用預設；其模式與 threshold 仍由該入口政策處理。

未設定新區塊時，原有明訂 entry 不變；新省略 backend 的 catalog entry 及已遷移的內建語意判斷使用 sidecar。舊 active control decision 未明訂 backend 時仍保留 System One 預設。critic 的 catalog entry 不再明訂 backend，可繼承共用設定。

有效設定在推論前凍結：catalog hash 涵蓋 resolved model、endpoint、timeout、credential revision；execution policy 加入 `decision_runtime_hash`。診斷僅公開有效 backend、model、endpoint 與來源，不公開 credential 值。設定來源標籤不取代實際值的 hash。決策的 source snapshot、scope、request digest 與 replay event 一同保存在原有 journal／artifact store。

## 12. 已確認的範圍修正與離線驗證紀錄

- `ask_user` 的 mixed 或 allowAny=true 問題不屬於封閉選擇題，依使用者確認保留既有生成及格式修復路徑。不得在這條路徑再追加 primitive。
- 封閉多選使用完整非空子集加 none，不逐項增加推論；超過 21 個候選直接失敗。現行實作最多四個多選項目；單選最多二十個項目加 none。
- memory conflict 實際合法關係為 contradicts、duplicate、refines、compatible；undetermined 是失敗後的保留狀態，不可猜成 compatible。判斷政策更新為 conflict-judge-v2，舊判斷保留讀取但不可充當新政策快取。
- 內建 request/state 決策使用既有 primitive journal 的 v3 payload，記錄不可變 request artifact；固定列使用 v2 payload，legacy payload v1 保留驗證。三者沒有平行資料庫。
- Runtime 選擇的直接結果不提供模型自稱的 confidence／rationale；相關 public 欄位保留，另以 accepted 或明確 runtime 說明表達來源。
- primary judge／challenge／revision 額外 primitive 呼叫上限為每個入口每條 lineage 1000 次；每次仍計入既有 run token／duration 預算。skill matching 上限 512 次，routing 上限 100 次，其餘內建入口上限 1000 次。這些是有界上限，不是隱藏 retry 配額。
- WP-01 至 WP-11 均已交付。固定列、control、共用設定與所有 F 項已通過離線驗收；X 項依已確認邊界排除。


### 實作與驗證補充

- `runtime_semantic_decision.go` 共用 request/state executor，直接使用既有 catalog、artifact store、started/settled/reused journal。原始 request 全量綁定；sidecar 的 primitive prompt 不經摘要或 context compiler 重寫。
- Plan reviewer 不再建立具選擇工具的模型 agent；primitive 選擇後才走既有 policy-gated plan action。審查期間 plan revision、內容或狀態改變，拒絕套用舊選擇。
- Primary finalization 的有限選擇也走 primitive；與 aggregate 不同時，另作明確計費的 explanation 生成，結果標示非權威，不能覆寫選項。既有 finalization 持久化仍負責 stage 恢復。
- 診斷投影包含 started、settled、reused、來源 snapshot hash、backend/model、設定來源與錯誤碼；reuse 必須引用相同 branch/task/actor/binding 的 settled event。
- Memory conflict 的 v2 快取 identity 納入有效模型及 execution policy hash，避免同一對內容在模型／端點／憑證版本變更後命中不相容舊判斷。v1 仍可讀。
- 容量限制採明確錯誤：request 最多 48 KiB；sidecar backend 另外限制完整 prompt 為 65536 runes。不以截斷、摘要或刪減候選逃避容量限制。
- Primitive 的 sidecar adapter 使用專用 `DecisionProfile`，與 backend 同為 65536 runes 並保留 512 output tokens 上限；一般分類／文字生成的 profile 不受影響。實際模型 context admission 仍可拒絕無法容納的請求。
- Judge graft 與 divergent finalization explanation 的額外生成也拒絕 result-only repair，避免已結算選項的 replay 意外觸發新的生成。
- Primary judge／challenge／revision 會先查詢相同請求的 primitive start snapshot，恢復已綁定的非權威生成內容，再由原 executor 驗證 settlement。started 未 settled 或已結算失敗均不能先重跑生成；primitive 失敗也不進入舊 judge 的自動重問迴圈。
- Primitive 的 provider 診斷使用獨立 `decision_primitive` purpose，不冒用 parent coordinator 的 context manifest；權威輸入由原始 request artifact／binding hash 追溯。
- 最終驗收全部通過；結果與環境條件如下。沒有呼叫真實 provider，也不以準確率或成本改善作為完成宣稱。

- 最終 synthesizer 的生成流程不遷移；只將新 session 的結果 schema 升為 `report-v2.json`，接受 v2 critic 的空 finding 集合並保留逐項 coverage 約束。`report-v1.json` 與 `critique-v1.json` 保留供舊契約讀取。

### 2026-10-21 最終離線驗收

以下命令全部 exit 0。測試與工具使用 `GOMAXPROCS=2`、本次私有 `TMPDIR`；大型驗證逐一執行。

| Gate | 實際執行範圍 | 結果 |
|---|---|---|
| 完整 Go 測試 | `go test -p 1 -timeout 30m -exec <test-exec> ./...` | 通過；CLI 114 秒、runtime 224 秒 |
| 隱藏 team | `go test -p 1 -timeout 30m -exec <test-exec> ./.agent-teams/hufu-code-review/... ./.agent-teams/hufu-dev/... ./.agent-teams/web-research/...` | 全部通過 |
| Targeted race | `go test -p 1 -race -exec <test-exec> ./internal/team -run 'Test(RuntimeDecision\|BoundDecision\|PrimaryPrimitive)' -timeout 10m` | 通過；20 秒 |
| 靜態分析 | `go vet -p 2 ./...` | 通過 |
| Lint | `golangci-lint run --concurrency 2` | 通過；0 issues |
| Diff | `git diff --check`；核對 core 無 team-name 政策分支 | 通過 |

`<test-exec>` 是使用者核准的暫存 wrapper：只對測試程序清空 `GOTMPDIR`，讓 Go 1.27 `t.TempDir()` 使用私有 `TMPDIR`；測試內呼叫 `go` 時恢復原編譯 `GOTMPDIR`。Go build／module／lint cache 均保留原設定，未建立第二套 cache。測試暫存受 512 MiB 預算及記憶體／空間保留量監控。

早期驗證在使用率 97% 的 Btrfs Go 暫存磁碟上發生 SQLite、CLI、fixture 的時間限制失敗；這些失敗不列為通過證據。改用核准的測試暫存方式後，以上完整命令全部重跑通過，未放寬產品或測試 timeout。套件總時限設為 30 分鐘以容納完整大型套件。

真實 team 的空 critique 測試另外確認 runtime 注入 accepted source、空 coverage 合法，以及 worker 抄回相同來源值仍被拒絕。CLI 二進位由既有 helper 統一為每 suite 編譯一次，所有消費者結束後清理；PTY 與持久化 fixture 的暫存亦具 cleanup。

完成範圍不包含真實模型品質評估、部署、修改全域使用者設定或操作執行中的 session。新契約在新 session 生效，舊 v1 結果保留讀取；mixed／允許自由回答維持原生成路徑。

### 後續實跑驗證修正（2026-10-21）

使用者另行授權透過 `mise exec -- go run` 實跑最近十個 commit 的審查，
不將模型準確率或成本比較加入驗收。實跑發現並修正：

- 專案明訂 `control-decisions.mode: active`，避免繼承已退役的全域 shadow
  配置；不修改全域或其他專案設定。
- 公開診斷在 session 啟動、尚未有 checkpoint binding 時，選擇當前 branch
  唯一的 `run_started`；多個真實啟動仍明確報歧義，不選取祖先 run。
- snapshot Go verifier 保留既有編譯 `GOTMPDIR` 與 `GOMAXPROCS`。本次測試
  程序使用經核准的短路徑私有 TMPDIR；不使用 symlink 工作目錄或新 cache。
- schema-only repair 失敗時，FailureEvent 與後續 retry context 顯示最後一筆
  驗證拒絕原因，避免將歷史 view 錯誤當成本次失敗。診斷仍限長、遮蔽秘密，
  不改變 failure fingerprint、重試次數或工具執行。
- 經使用者確認，原始結果契約明訂 nullable 且不允許 string 的位置，接受
  精確字串 `"null"` 正規化為 JSON null。enum、const、字串及 hash 保持原值；
  不推測條件分支或組合 schema。全部既有驗證通過後才雜湊及儲存 canonical
  payload；固定列所有權與來源綁定不變。原生 null 與此輸入得到相同 hash。
- 取消時的「由目前狀態提交」在同一投影鎖內讀取及提交，消除 worker 與
  emergency finalization 的讀取／提交競態；明訂 expected status 的呼叫仍
  拒絕 stale transition，未放寬狀態機。

先前中斷或失敗的 run 不列為成功證據；完整離線 gate 與新 session 的實跑
結果將分別記錄，不能以啟動成功或存在 ResultID 代替完成與驗收成功。

本輪最終程式版本的已完成驗證（2026-10-21 14:03 UTC）：

| Gate | 結果 |
|---|---|
| `mise exec -- go test -json -p 2 -exec <test-exec> ./...` | exit 0；64 個有測試套件通過，runtime 約 223 秒；新增 null、提交所有權、並行取消與 repair 診斷案例通過 |
| `mise exec -- go vet -p 2 ./...`，另含 reviewprep／reviewgate | exit 0 |
| `mise run lint --timeout 15m ./...`，另含 reviewprep／reviewgate／diagnostic-pilot | exit 0；0 issues |
| `git diff --check` | 通過 |

尚未完成本輪 targeted race、隱藏 team 最終重驗與修正後的新 session 實跑。
Go cache 所在磁碟已接近滿載、只剩約 0.1 GiB；依 AGENTS 保留既有 cache
及編譯 GOTMPDIR 的要求，不自行清理共用快取或搬移到 tmpfs。已請使用者
釋放至少 2 GiB／擴充容量，或明確授權清理共用 build cache，之後續跑。
這是環境容量限制，不列為上述程式測試失敗，也不宣稱實跑已成功。

使用者隨後明確授權清除 Go cache；已透過 mise 執行 `go clean -cache`
並 exit 0，磁碟可用空間回升至約 48 GiB。保留 module cache、編譯
GOTMPDIR、lint cache 與其他專案檔案；容量阻礙已解除，接續剩餘驗證。

提交邊界另沿用 `decodeUniqueJSON`：在 legacy DTO shape normalization
合併物件之前，以及 canonical payload 驗證時，拒絕重複 key／多份 JSON。
避免 null 正規化把歧義輸入變成可接受結果。合法 legacy 提交、已儲存的
canonical 結果與 hash 格式保持相容；重複 key 的歧義提交現在明確拒絕。

本輪最終程式版本（含上述唯一 key 邊界）的離線驗收已全部通過：
`mise exec -- go test -p 2 -exec <test-exec> ./...`、四個隱藏 team 套件、
`mise exec -- go vet -p 2 ./...`（另含 reviewprep／reviewgate），以及
targeted race（nullable、重複 key、提交所有權、repair 診斷、並行取消、
run binding）。最新原始碼的完整 lint 亦 exit 0、0 issues；之後未再改動
Go 原始碼。未呼叫真實 provider。新 session 的實跑另行記錄，不以離線
通過代替端到端驗收。

舊版驗證 run `run-20261021T123043.250083247Z-c2e3606bdc2a` 已因這次
提交邊界修正而中斷（exit 130）。它通過 snapshot Go verifier，但未完成
typed critic、最終報告及驗收，不作為新決策機制端到端成功證據。


### 第二次實跑的失敗與修正（2026-10-21）

`run-20261021T153012.803810241Z-570ed98857e4` 的準備及 16 個來源審查
最終顯示 18/18 done，但公開 inspect 顯示 outcome=unverified，尚未進入
inventory／typed critic／synthesis／audit，也未產生已驗收報告。此 run
不是端到端成功證據。

公開 task 11 診斷及 trace 確認：無效 submit_result 經結果修復仍失敗後，
anti-thrashing 把該 task 持久化為 blocked；worker retry loop 卻繼續使用
先前的 RetryWorker 提案，再次推論並覆蓋為 done。wrap-up 安全鎖仍有效，
因而下一個 inventory 委派被拒絕並耗盡 coordinator policy repair budget。
修正 retry loop，遵守持久化後的 blocked 狀態，不重啟該 occurrence。
不提高預算、不解除 wrap-up，也不把無效提交改為成功。

另修正兩個投影缺口：startup binding 使用既有 single_candidate 分類，
避免 operator snapshot 拒絕未知 selection source；每次 coordinator
request 在 context admission／compaction 前，更新目前 workflow phase
的 Available Agents 與 Runtime Workflow 提示。其他自訂提示保留，
授權仍由既有工具 gate 決定，不額外推論，也不擴大可用 worker。

新增離線回歸覆蓋持久化阻擋後不重試、正常允許的 retry 仍有效、workflow
phase 切換與壓縮後的 roster，以及 startup selection 的 snapshot 驗證。
本段修正後的完整驗證與新 session 實跑結果待下方補記。


既有離線 fixture 亦同步修正：retry-recovery eval 不再提供 replan 後
hard-block 之後的第二次 replacement 提交，並斷言第一個 task 兩次
attempt、replacement blocked 且一次 attempt；移除原本落到 coordinator
回合的多餘 submit_result step。證據分類／保留的兩個 WP05 測試明訂
fingerprint limit=5，並行隔離的 WP08 測試明訂 limit=3，以測試其各自
目的；不改正式 team 或 runtime 的預設上限。離線 eval lifecycle suites
與這些針對性回歸已通過。


2026-10-22 最終修正版已完成離線驗收：完整 `go test -p 2`（含四個
隱藏 team 套件）、targeted race、`go vet -p 2 ./...`（另含 reviewprep／
reviewgate）、`golangci-lint run`（含變更相關隱藏套件，0 issues）及
`git diff --check` 全部 exit 0。命令均由 mise 選定工具鏈；test executable
沿用已核准的短 TMPDIR／清空測試程序 GOTMPDIR，編譯程序的 GOTMPDIR
與既有 caches 不變。最終 race 的 team／inspect 分別為 3.497s／1.131s。

Phase 提示更新同時涵蓋首次 request：若 preflight 建立後 phase 已變，
第一步也明確覆寫已過期提示。Workflow 的固定開頭不再列初始 worker，
避免簡介與後續名單矛盾；只重新產生目前 phase 的兩個 runtime 提示區段，
不重建工具、不觸發摘要推論，也不改自訂指令。非 workflow 提示保持相容。

接續以使用者指定的相同 prompt／CLI 參數啟動新 session 實跑。實跑期間
不修改專案檔案；實際 run、decision primitive、報告與驗收結果於結束後補記。

### 2026-10-22：第八次實跑與限定相容修正

Run `run-20261022T004031.361307536Z-7af9d91c5c61` 已驗證原生 workset
產生與快照 Go verifier（15 個 package）通過，階段切換的 worker roster
也正常更新。來源審查尚未完成：task 9 的 `open_questions` 物件被 schema
拒絕，修復耗盡後正確停在 blocked，未違規啟動第二次 worker attempt。
為避免繼續消耗推論，已手動停止；此次沒有通過最終 acceptance、report
或 typed-finding primitive 的端到端驗證，不能視為成功。

使用者已明確允許新增唯一相容形式：只在 `open_questions` 接受精確的
單一 `$text` key 且值為非空字串，立即轉為既有 canonical 字串；額外 key、
空白、null、非字串、重複 key 與其他欄位仍拒絕。工具 schema 與 typed
解析同步更新，保留授權、來源、跨欄位驗證、修復上限與 anti-thrashing。
舊 session 不套用新契約；後續使用新 session 重跑原指令。

限定 `$text` 相容修正的離線驗收已完成：直接及 policy-gated 提交正反例、
`go test ./...`、四組隱藏 team 測試、`go vet`、`golangci-lint`（0 issues）
與提交／protocol repair／anti-thrashing targeted race tests 均通過。
第一次完整測試因 sandbox 禁止 httptest 本機 socket 而失敗；在允許本機
socket 的環境重跑全套成功，未略過測試或放寬程式政策。

### 2026-10-22：第九次實跑結果（未通過端到端驗收）

已用原指定 `mise exec -- go run ./cmd/hufu --agent-team hufu-code-review
--model ollama/minimax-m3.1-flash:cloud --auto-approve --report --new` 與原 prompt
建立新 session。Invocation 為 `inv-20261022T013403.351778739Z-67bccc379c30`，
run 為 `run-20261022T013416.692116292Z-75cfd0504c04`，branch 為
`session-1792632848505797824`。

- 原生 workset 完成，快照 verifier 的 15 個 package 全數通過，exit code 0、
  未逾時且結果 accepted。來源審查階段持續正常派發。
- Task 5 提交字串 `"0"` 索引，且 `uncertain` 的 finding_index 應為 null；
  schema 正確拒絕，既有第二次 worker attempt 最終完成。沒有新增型別猜補。
- Task 9 先因 invariant summary 超過 1000 字元遭拒，結果修復後仍提交
  不足 64 位十六進位字元的 evidence hash。修復耗盡後 canonical 狀態為
  blocked，只有一次 worker attempt，沒有繞過 anti-thrashing 或補造 hash。
- 由於必要來源已 blocked，已停止此 run。Go run wrapper exit 1，應用程式
  因手動取消 exit 130。停止後公開 inspect 的 outcome/stop_reason 均為
  cancelled，18 個 task 中 done 9、unresolved 9；其中取消狀態不可誤認為
  原始故障。公開 completion audit 的 pass 不是整體任務成功。
- 尚未完成 typed-finding primitive、synthesis、最終 report audit 或最終報告；
  不宣稱這輪實跑驗收成功，也不據此宣稱模型準確率或成本改善。

這次 `$text` 相容修正及全部離線 gates 已完成。後續模型對照需使用者選擇，
不得為強迫通過而猜測 evidence、放寬 schema、提高重試上限或靜默換模型。

### 短 evidence key：減少模型重抄來源 ID

依使用者確認新增通用 opt-in result-contract 宣告
`x-hufu-evidence-keys.pointers`，明確選取 worker 可引用短 key 的字串位置。
同時接受 `files_read` 的既有字串／path 形式。Core 不判斷 team 名稱，
hufu-code-review 只在目前使用的 review-v1、critique-v2 與 report-v2 schema
及角色提示詞接入；既有來源快照與 legacy schema 保留完整 ID 格式。

共用既有 artifact observation、hash-chained journal、CAS、schema 驗證與
結果持久化，不建立第二套 alias database。每次成功的 trusted built-in
view 只產生一次觀察；同一觀察可同時支持 evidence key 與 decision context。
`artifact_evidence_observed` 沿用同一觀察 payload／驗證流程，legacy
`decision_artifact_observed` 仍可讀。Built-in view 以受信任 metadata 記錄
實際成功且輸出可見的 artifact，避免從模型輸入或 artifact-controlled XML
猜測 batch 成功項目。

Key 綁定 branch、run、task、attempt、model execution、task scope、schema
hash、artifact ID 與內容 hash。提交時由 durable observations 重建對照，
再次檢查授權及 CAS 完整性，再執行原有 canonical 驗證與 hash。未知／未讀／
跨 scope key、來源改變與碰撞 fail closed。只處理 schema 宣告的位置，
不改寫 rationale、摘要或來源識別欄位，不猜補模型抄錯的 hash。

離線驗收涵蓋直接與 gated 提交、canonical value/hash 一致、未啟用相容性、
未知／未讀／撤銷／跨 task-attempt-branch-model-run、schema drift、來源內容
變更、partial batch 與被輸出上限排除的項目、journal reopen、result-only
repair 不推論，以及與固定列／decision primitive 的共同接入。完成後依序
執行全套與隱藏 team 測試、targeted race、vet、lint 及 team 靜態驗證。

後續真實模型實跑如再次因模型提交錯誤而無法完成，使用者已授權保留失敗
紀錄，再以 `ollama/minimax-m3:cloud` 開新 session 對照。此為操作層測試
策略，不新增 runtime 隱藏重試或自動 backend／model fallback。

短 evidence key 的離線實作驗收已完成：全部 targeted cases、`go test ./...`、
四組隱藏 team 測試、targeted race（team 23.629s／tools 1.244s）、`go vet`
及完整 `golangci-lint`（0 issues）皆通過。並行讀取保留四份觀察但只發出
一個穩定 artifact key；journal reopen 後可結果修復重用，沒有新推論。
已驗證 keyed fixed collection 與 decision primitive 共用一次推論並組裝完整
ID；大型整數與指數數字表達、null、既有 file-ref shorthand 保持不變。
未知 key 的修復診斷只顯示有界的已發出對照表，超限會明確省略整張表，
不以部分表冒充完整權威資料。讀取事件保存可驗證的 evidence_keys 投影；
來源與觀察 CAS 損壞、偽造投影及混合 filesystem batch 均無法取得有效 key。

上述為離線可靠性與相容性驗證；不據此宣稱真實模型的端到端成功率或成本
已改善，先前 run 的失敗／取消紀錄也不因這次修正而改為成功。

CLI 靜態驗證亦完成：`team validate`、`team explain`、`list` 與指定模型的
`--dry-run` 全部成功。`team lint` 為 0 error、1 個既有
`subagent-providers` legacy warning。dry-run 使用獨立暫存 workspace，沒有
執行 worker 或呼叫真實 provider，未變更既有執行中 session 的契約。

### minimax-m3 實跑發現的 repair 授權缺口

2026-10-22 的 `run-20261022T031544.555062546Z-eee572e1707b` 使用
`ollama/minimax-m3:cloud` 審查最近 10 個 commit。固定快照的 15 個 Go package
驗證通過，第一個模型審查已接受；另一個 reviewer 將空索引提交為 `""`，
並曾將 open_questions 編成物件，均被既有契約拒絕。進入 result-only repair
後，正確引用先前 key 仍得到 `evidence_key_source_unauthorized`：repair 從
parent context 建立，遺漏原 attempt 的 ArtifactAccessScope。為修正此 runtime
缺口而取消該 run；其 canonical outcome 仍為 cancelled，不能記為成功。

修正將原 worker metadata 與 artifact scope 的複本帶入即時 repair；恢復路徑
從既有 receipt／context manifest 恢復，repair 次數不再替換原 worker attempt。
不重建更寬的授權集合、不新增 observation／工具呼叫或 repair 額度，原有
來源驗證與 schema 不變。實際 executeTask 與 checkpoint-resume 回歸測試核對
scope／metadata 與 durable snapshot 不被 repair context 修改。Team reviewer
另明示 JSON null、整數索引與 open_questions 字串陣列範例；空字串仍不接受。

此次完整回歸另揭露 artifact store 的既有發布競態：O_EXCL 建立最終路徑後才
寫入，並行讀取者可能比較到未完成內容。資料與 metadata 改用現有
`fsutil.AtomicCreateFile` 完整寫入、同步後再原子發布；既有內容與 metadata
衝突仍拒絕，不覆寫共享 CAS。並行 store publication 與 evidence-key read
壓力測試連續 30 次通過，並保留原始完整測試的失敗紀錄。

上述 repair／CAS 修正後，完整 `go test ./...`、四個隱藏 team 測試、targeted
race（repair／resume／key／CAS）、`go vet` 與 `golangci-lint` 全部通過；lint
為 0 issues。Team validate 通過，team lint 維持 0 error／1 legacy warning。
此為程式修正的離線驗證，不代表接續的真實模型 run 已完成。

### 修正後的 minimax-m3 實跑結果（未完成）

第二次使用 `run --team hufu-code-review --model ollama/minimax-m3:cloud
--auto-approve --report --new`，獨立 workspace 為
`/tmp/94b1/m3-fixed-workspace`，run 為
`run-20261022T035930.786849536Z-e820882a8d71`。同一最近 10-commit scope 的
15 個 Go package 驗證通過（exit 0、未逾時）；3 個模型來源審查被接受，
連同 2 個 native 任務共完成 5 項。尚未進入完整 critic／最終報告驗收，
不能宣稱端到端成功。

Task 5 原始提交的 finding_index 為空字串而被拒絕。兩次 result-only repair
分別漏外層 status、將 gap_index 交為字串，最終為 protocol／invalid_schema。
公開診斷未保存第二次 repair 的完整原始引數，因此不宣稱該字串一定為
`"0"`。修正後沒有再次出現 evidence_key_source_unauthorized；之後 Task 5
新的 worker attempt 沒有新讀取，卻引用前一 attempt 的 key，因而正確得到
unknown_or_out_of_scope。Task 7／8 的讀取仍有合法、可驗證的 key observation。

已要求停止派發並取消剩餘執行，CLI exit 130，canonical outcome／stop_reason
均為 cancelled；completion audit 的 pass 僅代表此停止狀態如實結算，不代表
審查完成。保留兩次 run 的公開 inspect／status、console log 與 debug bundle。
下一步待使用者決定是否接受 schema-aware 的精確整數字串正規化；尚未實作
該政策，也不擴大空字串、float、enum、hash 或任意文字的接受範圍。

### 已確認的受限整數相容規則

使用者已同意：schema 明訂整數欄位中的 `"0"` 轉成 JSON 整數 `0`。沿用
既有原始編譯契約的 scalar normalization，只有明訂 integer、排除 string
且沒有 enum／const 的欄位或陣列元素才適用。精確合法的十進位整數按原始
數值解析，不經 float64；不猜測 schema 分支，不補空字串、浮點或缺漏值，
不改寫 hash／文字，仍通過完整 schema、跨欄位、來源、receipt 與固定欄位
驗證後才接受及計算 canonical hash。既有結果與已保存 payload 不會重寫。

另已讀取使用者提供的保存報告：
`run-20261021T162510.409391320Z-22a954673f89` 於 2026-10-21 17:38:33 UTC
完成最近 3 個 commit 審查，報告記錄 completed、acceptance passed、0 unresolved、
19/19 typed verifier adoption，耗時 1h13m19s，worker 使用 minimax-m3.1-flash。
這是模型在其他 runtime／team／scope 組合成功的證據，不能以目前 10-commit
實跑失敗推論模型一概不能完成。保存報告不會重新驗證當時的 artifact 內容；
要定位版本回歸仍須比較實際 runtime／契約／輸入，不只比較模型名稱。

受限整數正規化的針對性測試及完整驗證已通過：根目錄所有 Go tests、四個
隱藏 team、targeted race、vet、lint（0 issues）。測試證明一般提交與
result-only repair 接受等值整數字串，保留大整數精度及 canonical hash，
空字串／浮點／指數／前導零／enum／const／超界／重複 key 仍拒絕。工具的
provider-facing 整數相容宣告與 pattern 預檢同步更新；既有的 3 次 worker
拒絕上限與 repair 次數回歸均通過，沒有因接受整數字串而增加推論次數。
原始 compiled schema 與 canonical payload 契約未放寬。
