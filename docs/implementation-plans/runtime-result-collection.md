# 減少 submit_result 依賴：Runtime 主動收集結果實作計畫

> Status: active
> Authority: reference
> Verified-Commit: `c8c52d30a1932caddc3ad0d7ecc0ad0ad15882cb`
> Supersedes: —
> Superseded-By: —

> **用途**：交給 coding agent 依序實作；本文件本身不授權操作真實服務或更動部署。
> **Baseline**：`kjelly/hufu` @ `c8c52d30a1932caddc3ad0d7ecc0ad0ad15882cb`（2026-10-27 本地核對）。
> **狀態**：PR-1、PR-2、PR-3 已完成；PR-4 的公開設定與新預設尚未啟用。
> **產品決策**：使用者已選擇「相容的新任務預設使用 runtime 主動收集結果」，接受可能增加一次模型呼叫及受限格式修復。
> **文件位置**：依使用者後續指示，自 `docs/tmp/spec.md` 移入版本控制；本計畫為 reference，實作時同步正式架構與參考文件。

## 1. 目標與保證範圍

讓 runtime 負責發起結果收集，移除一般 local worker 必須自行呼叫 `submit_result` 才能交付工作的要求。

完成後，相容的新任務不向工作模型暴露 `submit_result`，也不以「沒呼叫此工具」判定失敗。工作執行留下的完整證據先持久化；結果收集失敗時只能修復或恢復結果階段，不得重播已完成或副作用不確定的工作。既有工具傳輸僅保留為明確的相容選項。

這項保證不代表任何模型回覆都能通過驗收。資料不足、非法 JSON、契約不成立、驗證失敗、預算耗盡或服務故障仍須如實呈現。禁止透過改名錯誤、無限等待、把 prose 直接轉為成功，來宣稱解決了問題。

### 可驗收的產品結果

- runtime 模式正常完成與格式修復的整個流程，`submit_result` 呼叫數均為 **0**。
- worker 不必選對結果工具、取得其工具權限，或在工作步數用完前留下額外一步呼叫它。
- 工作模型只交付文字報告時，由 runtime 發起無工具的結果收集；有效結果仍經完整契約、證據與驗證才能完成。
- 同一份封存工作證據遇到結果收集失敗、進程中斷或重啟，不重跑工作工具；受限修復及恢復能沿用原證據。
- legacy tool 任務、外部 agent proposal 與 runtime action 的既有完成和驗收語意不退步。

### 範圍

只實作 transport-neutral local result admission、無工具的結果收集、durable finalization checkpoint、必要的政策／恢復／顯示整合與測試。

移除原計畫中的 dependency 欄位投影、Task Signal／message bus、全面 lifecycle 重寫，以及 provider native structured output。這些不是本次交付事項，也不是後續必做 PR。實作不依賴真實模型測試、外部供應商改介面、付費帳號、上游套件合併或人工部署；一律以 scripted model、fake backend 及本地 HTTP fixture 驗收。

## 2. 現有程式事實與修改入口

實作前先比較 baseline 與當前 checkout 的相關差異；重用已存在的能力。下列連結指向本地檔案，函式位置以實作時版本為準。

| 入口 | 現有能力／本次責任 |
|---|---|
| [coordinator_tools_result.go](../../internal/team/coordinator_tools_result.go) | `SubmitResultInput`、`runFull`、schema／receipt／artifact／occurrence commit；抽出不依賴工具的 local admission |
| [task_protocol_tools.go](../../internal/team/task_protocol_tools.go)、[tool_policy_gate.go](../../internal/team/tool_policy_gate.go) | 協定工具可見性與中央授權；runtime 模式須同時移除可見性及執行授權 |
| [task_result_sink.go](../../internal/team/task_result_sink.go)、[task_occurrence.go](../../internal/team/task_occurrence.go) | 提交與完成已有責任分界；保留 reservation、duplicate、rollback 與 lease ordering |
| [coordinator_task_run.go](../../internal/team/coordinator_task_run.go)、[subagent_hufu.go](../../internal/team/subagent_hufu.go) | local worker、工具迴圈、repair、verifier 與 workspace apply；接入新的收集階段 |
| [result_contract_validate.go](../../internal/team/result_contract_validate.go)、[result_submission_schema.go](../../internal/team/result_submission_schema.go) | 完整 pinned schema、本地驗證與 `x-hufu-submission-fields`；沿用，不另造結果資料契約 |
| [result_candidate.go](../../internal/team/result_candidate.go)、[result_candidate_commit.go](../../internal/team/result_candidate_commit.go) | rejected/prepared candidate 已有 CAS 與無 provider 恢復；保留其名稱、限制及 legacy 語意 |
| [protocol_repair_attempts.go](../../internal/team/protocol_repair_attempts.go)、[operator_result_repair.go](../../internal/team/operator_result_repair.go) | durable repair 次數與 operator grant；不得以新收集流程重置或疊加無限重試 |
| [execution_policy_snapshot.go](../../internal/team/execution_policy_snapshot.go)、[execution_receipt.go](../../internal/team/execution_receipt.go) | transport 與收集版本封存、原始 execution receipt、收集 provenance 與恢復進度 |
| [provider_request.go](../../internal/agent/provider_request.go)、[context_purpose.go](../../internal/team/context_purpose.go)、[model_profile_runtime.go](../../internal/team/model_profile_runtime.go) | 現有 provider admission、模型／上下文／用量追蹤；新模型呼叫必須經相同邊界 |
| [subagent_external_result.go](../../internal/team/subagent_external_result.go) | external `WorkerResultProposal` 已可不使用 `submit_result`；維持既有獨立 adapter |

`FormatDependencyResults` 已有欄位渲染，workset 亦有 artifact 授權投影；本次不修改它們。`coordinatorTaskResultSink` 已不負責任務驗收；本次不建立第二套 lifecycle controller。

## 3. 已定稿的傳輸與相容政策

### 3.1 靜態設定

新增 team 與 agent frontmatter 設定 `result-transport`，只接受小寫 enum `runtime`、`tool`。優先順序：agent 明確設定 > team 明確設定 > 下表預設。未知值直接拒絕；不新增 CLI flag，也不讓動態 `agent` tool JSON 提升或切換此政策。

```yaml
# 新 session 的一般 local worker 可省略；預設就是 runtime。
result-transport: runtime
```

| 任務／來源 | 未明確設定時 | 明確設定的處理 |
|---|---|---|
| 新 session 的一般 local LLM worker | `runtime` | 可明確選擇 `tool` |
| 靜態 closed tool sequence 或其他機器契約要求 `submit_result` | `tool` | 明確 `runtime` 與該契約衝突時，在任何模型／工作工具執行前拒絕 |
| 舊 session／舊政策 snapshot 沒有 transport 欄位 | 保留原工具協定 | 不因新版預設自動改成 runtime；改政策須新 session |
| External agent backend | 保留 `WorkerResultProposal` | 此設定僅作用於 local LLM candidate；不改 external wire protocol |
| Runtime-owned action／structured execution | 保留現有可信結果入口 | 不增加模型收集呼叫，不要求結果工具 |
| Sidecar、coordinator、plan-only 回合 | 保留各自現有協定 | 不套用 worker 收集流程 |

不能掃描任務描述、prompt 關鍵字或團隊名稱猜測模式。判斷只依 backend kind 與靜態 typed contract；一般工具清單提到某工具也不能取代 completion contract。

### 3.2 封存與路由

- 在 task admission、任何副作用之前，封存有效 transport、collector schema revision、收集目標與 budget。這些資料需進入 execution policy／occurrence 的 hash 與 drift 驗證。
- 收集使用**實際執行工作的已核准 local target**，不自行換成 sidecar、更強模型或其他 endpoint。路由先安全 fallback 到另一候選時，收集仍綁定實際候選。
- mixed local／external route 為每個候選封存自己的結果協定。工作開始後不得因結果格式失敗換 backend 重跑工作。
- 舊 snapshot 缺欄位時以相容解碼還原 tool 語意並保留原 hash／serialization 規則，不拿新版 runtime 預設重算歷史政策；顯式改設定仍遵守既有 drift rejection。補上真實舊版 fixture，不能只測新版資料刪掉一個欄位。
- `plan-first` 仍先通過既有 `submit_plan`／審批；runtime 收集僅接在獲准的 execute 階段。無法相容的組合須 preflight 拒絕，不得靜默改成 tool。
- direct-agent、subagent 與 extra-model leaf 均使用相同政策解析；每個 leaf 的收集只能讀取該 leaf 的證據與 workspace。

## 4. 兩階段流程與信任邊界

```text
authorized worker tools（沒有 submit_result）
  → 工作回合返回
  → 封存 transcript／receipt／work report／scope／stop reason
  → durable result-collection checkpoint
  → runtime 發起無工具模型呼叫，取得不可信 JSON claims
  → strict decode + local result admission
  → existing verification / workspace apply / terminal gates
  → TaskDone 或既有失敗／blocked 恢復流程
```

### 4.1 工作階段

1. 保留 `RequiresResult` 的 typed-result 要求，但將它與「必須暴露 `submit_result`」分開；不得用 `RequiresResult = false` 逃避驗證。
2. runtime 模式不注入結果工具，不把它放入 logical allowlist、gateway 動態目標或 result-only 工具集合；worker 偽造呼叫時由中央 gate 拒絕，不能執行 handler。
3. 更新中央 worker 完成指示：工作完成後交付報告，結果由 runtime 收集。工具模式仍保留其正確工具指示。不要用 deterministic prose parser 修改團隊提示。
4. `submit_plan`、工作工具、phase、unattended、guard、artifact scope 與 closed sequence 等授權維持有效；新結果模式不擴權。
5. 工作的錯誤、取消、tool denial、terminal failure 與 step-budget stop reason 以結構化資料保留。模型報告或 collector 不得抹除這些事實；依既有 completion gate 判定，不將步數耗盡一律當成缺結果工具。
6. 若 runtime 模式的 worker 因殘留指示或幻覺送出 `submit_result`，中央 gate 仍拒絕執行，記錄沒有副作用的報告協定拒絕；以固定原因 `deprecated_result_transport` 結束該工作回合並進入封存／收集，不只因這一次過時報告嘗試就把任務判為失敗。其參數只可作 fenced untrusted data，不直接提交。這個分類只適用已封存 runtime 模式的此報告工具，不豁免其他 denied tools、原有執行失敗或驗收要求。
7. 純文字／分析任務可能沒有工作工具；更新 local adapter 的空工具檢查，使已獲准的 runtime 任務可正常執行無工具工作回合，不能為了通過檢查補上結果工具或其他權限。

### 4.2 先封存，再收集

增加 runtime-owned `ResultCollectionCheckpoint`，存放於既有 receipt／CAS／Event Store 體系。它表示工作回合已返回、結果尚待處理，**不表示工作成功或任務完成**。

至少封存：

- branch、原 run/task/attempt、occurrence revision、dispatch identity、實際 execution target、有效 transport 與政策 digest。
- 完整 sealed 工作 transcript、工作報告及原 receipt 的 opaque refs／hash；保留原時間、stop reason、工具 dispositions、預算使用與 workspace identity。
- frozen input snapshot、result contract、已接受 evidence sources 與 artifact scope 的身份／digest；沒有此類契約的任務使用明確的 absent 表示，不偽造 ID。
- collector schema revision、不可變收集輸入 manifest、collector model invocation IDs、已消耗回合與收集進度。
- 已收到的完整 collector response 或 prepared claims 的 CAS refs／hash；candidate 不得只存在 RAM。

使用新的 `task_result_collection_checkpointed` event 更新這份 attempt metadata；註冊 event schema、catalog、reducer、branch replay 及 inspect。CAS 寫入先於引用事件，事件成功後才發布 projection；append 失敗不得繼續呼叫 collector 或公開成功狀態。這不是第二份 accepted result store。

checkpoint 更新需有單調 revision 與 compare-and-swap，event idempotency key 包含 branch、task、原工作 attempt 與 collection revision／invocation identity。同一個 pending checkpoint 的兩個 resume 競爭時，只有一個能保留模型回合並派發請求；另一個讀取 durable 進度，不另花 budget 或執行 collector。保留既有 receipt 的 canonical bytes／hash，新增收集 provenance 不得重新 redaction 原始 runtime outputs。

收集進度固定為 `pending`、`in_flight`、`received`、`prepared`、`committed`、`blocked`，由 runtime 產生，不開放模型填寫。任務在收集階段維持既有非終態；耗盡收集機會或出現不可自動修復的收集錯誤時，依既有 failure disposition 進入 `TaskBlocked`／停止流程。工作本身失敗仍走原 execution recovery。不新增 Todo status。

不假設現有 `ResultCandidate` 可直接承載所有收集資料：它目前限於有 result contract 的任務，且上限為 256 KiB。新 checkpoint 應支援沒有 result contract 的任務；可用共用 CAS helper，但不可重新解釋舊 candidate 或無聲放寬其上限。收集 JSON response 上限 2 MiB；組裝 runtime-owned 欄位後的 canonical payload 仍遵守現有 2 MiB 限制。超限如實拒絕，不裁切成另一份有效結果。

### 4.3 Runtime 主動收集

第一版固定由 runtime 主動發起一次普通模型呼叫；不依賴 worker 自行輸出指定 JSON，不從工作報告擷取「看似結果」的片段，也不使用另一個改名的結果工具。

1. 在既有模型 invocation／provider admission 邊界新增 `result_collection` purpose。以普通 text generation 請求單一 JSON object，明確傳遞 model-authored submission schema、必要契約說明與工作證據。
2. 請求中的 `tools` 必須為空，不能藏入 `submit_result`、schema function tool、MCP、讀寫檔案或其他可執行工具。不得透過 `GenerateObject` 預設的 tool-mode fallback 偷渡工具呼叫。
3. 使用既有 provider wrapper、模型容量、token／時間 budget、用量／quota、取消、稽核、redaction、status 與 manifest 機制。不要直接建立未受控 HTTP client 或裸呼叫模型繞過這些邊界。
4. collector 輸入只來自當次封存工作與已授權來源。prose、工具輸出與上游文字均以 untrusted data fencing 包裝；不交付其他 task 或 branch 的內容。檔案與 artifact 由 runtime 依授權作確定性核驗，collector 沒有工具可自行探索。
5. 要求模型只供應 `SubmitResultInput` 對應的語意 claims，並沿用有效 `x-hufu-submission-fields`。模型不填寫 run/task/attempt、source、可信 hash、receipt 真實性、runtime outputs、runtime-owned structured 欄位或 workspace identity。
6. 完整原始回覆在既有 redaction 後持久化。嚴格解碼單一 JSON object：拒絕 duplicate key、unknown field、trailing value、code fence、非 JSON 文字及 oversized response；沒有 regexp、寬鬆 parser 或 free-text success fallback。
7. 模型輸出仍是不可信資料。本地 pinned schema、來源與收據驗證為權威；普通 JSON prompting 不等於 provider native structured output，也不保證 JSON 永遠有效。
8. 證據／schema／輸入超出可用 context 時 preflight 停止收集，保留 checkpoint 與原因。不得刪除必要證據、只用摘要取代完整 required evidence，或要求更高權限來完成收集。

此方案不要求供應商新增功能或 coding agent 更改 Fantasy 上游。HTTP fixture 必須檢查實際 request 沒有工具，不能只依 method 名稱判斷。

collector timeout 與 output-token limit 沿用該已核准 target 的有效設定，且不得超過剩餘 task／invocation deadline 與總 budget；工作 context 已取消或逾時不能建立新 context 繼續呼叫。collector manifest 是獨立的派生 invocation，不能取代原 task dispatch manifest 的 required-evidence delivery 證明。沒有全量證據可交付時就停止，不能讓收集階段重新閱讀更寬的來源補足。

### 4.4 共用 local admission 與既有完成流程

- 從 `submitResultTool.runFull` 抽出 internal local admission，以 runtime-owned identity、原始工作證據與 decoded model claims 為輸入。`submit_result` 保留 DTO／工具錯誤映射；collector adapter 保留 strict JSON decode／收集錯誤映射。
- 共用部分沿用完整 schema、runtime input binding、evidence delivery、receipt claims、HMAC stripping、memory-use claims、assumption／invariant attestation、artifact materialization、occurrence reservation 與 commit 順序。不得因沒有 tool context 而跳過其中任一步；receipt／tool observations 綁原工作 attempt，collector invocation 另記 provenance。
- collector 的 `TaskResult.Source` 使用新值 `collected`，由 runtime 設定。擴充 `isSubmittedResultSource` 及所有硬編碼 `Source == "submitted"` 的 admission／repair／resume 判斷，按實際語意處理；不可把 `collected` 當成可信 action 的 `runtime`。
- local tool、`collected`、external proposal 仍保留各自必要的邊界驗證；external 已共用的 structured payload validator 繼續沿用。本次不強迫 external DTO 與 local DTO 合併，也不擴張其支援契約。
- canonical result 成功提交後，沿用既有 verifier、skeptic、terminal checks、isolated workspace apply、commit gate 與 acceptance；不得在 collector handler 設定 `TaskDone`。
- `partial`、`failed`、`blocked` 是有效的誠實結果，走 execution recovery；不做格式修復將它改成 success。`completed_with_gaps` 仍由原 acceptance 決定是否接受。
- 任務身份與 lease 只能由 runtime 開啟；status transition commit 後才取得新 lease。不同 collection 回合不建立新的工作 attempt，也不能用失效 lease 提交。
- checkpoint 的原工作身份不可變；operator grant／合法恢復若提升當前 occurrence revision，只能由 canonical transition 建立「原工作證據 → 新提交 lease」的受信綁定並持久化。不得直接把舊 revision 改成新值，也不能因合法恢復取得新 lease 就誤認原證據 stale；未經綁定的 revision 差異仍拒絕。

## 5. 有界修復、恢復與失敗分類

### 5.1 收集 budget

- 同一工作 attempt 自動最多 **2 個 collection 模型回合**：通常為 1 次收集，加 1 次僅限格式／schema 的修復；第 5.3 節的 crash 重發也消耗同一個剩餘名額，不能再多一次格式修復。工作模型 `max-steps` 不用預留 `submit_result` 步數；collector 用量仍計入整體 token／duration budget，不能超額借用。
- 每次模型呼叫前先持久化 `in_flight`、invocation identity 與已消耗回合數；若這一步失敗，不呼叫模型。重啟不得重置次數；無法確認收到回覆的 in-flight 回合也計為已消耗。
- 既有 provider transport retry 仍受原有上限與剩餘總預算控制，不得另建無限 HTTP retry。它不授權重跑工作，也不重置 collector 邏輯回合數。
- 新模式不能在兩個回合後再自動進 legacy protocol repair／worker retry／另一個 collector model。legacy tool 任務維持其原有修復額度；兩種模式不得疊加額度。
- 總 budget 用盡、使用者取消或 shutdown 時不發起額外模型呼叫；保留 pending／blocked checkpoint，依現有取消或 wrap-up 流程停止。

### 5.2 可修復與不可格式修復的情況

| 情況 | 行為 |
|---|---|
| 完整收到回覆但缺結果、JSON 格式錯誤或 schema shape 不合法 | 有餘額時使用一次無工具修復；提供封存回覆與確定性診斷，不重跑工作 |
| 合法的 `partial`／`failed`／`blocked` | 接受誠實報告並依既有 execution recovery 判斷；不是格式錯誤 |
| 證據不足、來源 stale／跨 branch、receipt／契約／identity 不成立 | 停止收集與格式修復；保留原因並依既有 policy／identity／reconcile 規則處理 |
| CAS／Event Store／commit 故障 | 保留原始工作；回報 storage／commit 錯誤，不讓模型重新編造結果或重播工作 |
| 模型服務暫時失敗 | 停止本次收集並保留可恢復進度；不以格式修復換模型或啟動工作重試 |
| collector 嘗試呼叫工具 | 不執行任何工具；視為不合法結果，受同一修復 budget 約束 |

新增 runtime-authored reason code `result_missing` 表示收集未產生結果；其餘優先沿用 `invalid_schema`、`source_invalid`、`contract_rejected`、`commit_failed` 等現有分類，provider／budget／cancelled 沿既有 failure classification。runtime 模式不得生成 `no_tool_call`，也不得向使用者提示「請補呼叫 submit_result」。所有新增固定 vocabulary 都需 enum、序列化與 replay 測試。

### 5.3 Crash／resume 決策

1. 依 checkpoint 的 branch、原 run、工作 attempt、policy、schema、inputs、source digest、workspace identity 與 CAS bytes 驗證。任一不一致則 fail closed，不重新選最新結果或 workspace。
2. `pending`：在同一 frozen policy 與剩餘 budget 內只執行 collection。
3. `in_flight` 且沒有持久回覆：標為中斷的 collection invocation；若仍有預先封存的自動回合額度，可對同一不可變輸入重新收集。沒有餘額則 blocked，不重跑工作。
4. `received`：先對已持久化回覆做本地 decode／validation；有效時直接準備提交，不新增模型呼叫。無效時才按剩餘額度處理。
5. `prepared`：不呼叫 provider；重用既有 prepared candidate 的本地恢復原則，重新核驗原始證據後走共用 admission。只延伸必要 helper，不經工具 handler 假造工具呼叫。
6. `committed` 或已有 accepted result 而 finalization checkpoint 尚未更新：依既有 occurrence idempotency 辨識已提交結果，只接續 verifier／apply／terminal gate，不重新 materialize artifact、工作工具或模型回合。
7. `blocked`：普通 resume 不自動發起新工作或增加模型額度。需額外模型回合時，只接受既有明確 result-only operator grant 的一次有界增額（`additional_attempts: 2`），記錄 grant 與既有消耗次數；prepared bytes 的本地提交使用 `additional_attempts: 0`。只允許修復結果缺漏、格式或暫時 provider 問題；不能授權來源、身份或工作執行失敗的繞道。延伸現有 grant 驗證以支援 collection checkpoint／無 result contract 任務，仍綁定原 target，不開放 replacement model，並保留隔離 workspace 的既有安全限制；不另開 repair CLI 或默默放寬限制。
8. 若進程在工作回合返回、但 checkpoint 尚未成功封存之前中斷，仍依既有 unknown-effect reconciliation。不能聲稱新 checkpoint 能恢復尚未持久化的證據，亦不能自動重跑副作用不確定的工作。

verifier／apply 的重啟安全性沿用現有流程；本次新模型流程不提供額外執行授權。artifact materialization、result commit 與 terminal transition 各間隙均需 fault-injection 回歸。

## 6. PR 切分與 coding agent 工作事項

所有 PR 依序執行；以 repository fixtures 完成，不要求使用者提供模型、帳號、金鑰、實際任務或部署環境。

| PR | 可直接執行的工作 | 完成門檻 |
|---|---|---|
| **PR-1：抽出 local admission** | 拆出 tool-neutral claims admission；保留 local tool adapter、external adapter、既有 candidate 與 lifecycle；整理 transport／source 判斷 helper | 同樣的 legacy 輸入產生等價 canonical 結果、receipt、artifact 與失敗分類；無新預設行為 |
| **PR-2：收集 checkpoint 與恢復** | 增加 receipt metadata／CAS envelope／checkpoint event、reducer、budget reservation、prepared commit 與 result-only 恢復；處理無 result contract 任務 | 中斷、event failure、double-submit、跨 branch／stale、prepared／committed resume fixtures 通過；未啟用新預設 |
| **PR-3：無工具 collector** | 增加 `result_collection` purpose、schema-guided text request、strict decoder、一回合格式修復、共用 admission、runtime 工具可見性／授權與完成指示、所有執行入口整合 | 選用 runtime 的 local worker 及修復全程 0 次 `submit_result`；wire-level tools 為空；工作證據與 tool counters 不因收集重跑 |
| **PR-4：新任務預設與公開驗收** | 新增靜態 enum／有效政策封存、舊 snapshot 相容、中央 prompts／tool surface、CLI／JSON／report／inspect／TUI 顯示、正式文件與泛用範例；啟用第 3 節預設 | 新 session 預設實際走 runtime；舊 session、closed sequence、external 與 action paths 保持宣告協定；跨 PR 測試矩陣通過 |

PR-3 的測試可經 internal typed policy seam 選用 runtime；不得把測試 seam 暴露成不受封存限制的 runtime 開關。PR-4 的正式 parser 與政策接好後，production 只能從 admitted static policy 選擇模式。

## 7. 必要測試與驗收證據

沿用 `newTestCoordinator`、`scriptWorker`、既有 provider fake 與短路徑 socket／PTY fixtures。新增行為測試以行為命名；不保留重複的臨時 characterization tests，也不新增超過 800 行的測試檔案。

| 場景 | 必須證明 |
|---|---|
| 新一般 local task，worker 僅回傳 prose | runtime 主動收集並驗證後完成；worker 與 collector 的 `submit_result` 呼叫數均為 0 |
| runtime worker 沒有工作工具／嘗試過時的 submit_result | 無工具任務可完成；過時工具 handler 執行數為 0，僅記錄拒絕並由 runtime 收集，不只因工具名稱導致失敗；其他 policy denial 不被豁免 |
| 工作完成時用完 worker tool-step budget | 不要求額外提交工具步數；collector 依剩餘整體預算執行，保留真實 stop reason 與 completion gates |
| 非法 JSON → 一次修復 → 有效結果 | 只增加收集回合；工作工具副作用 counter 恆為 1，原 receipt／transcript hash 不變 |
| 連續兩回合非法結果／provider unavailable／總 budget 用盡 | bounded stop 並保留工作與診斷；無 legacy repair 疊加、隱藏工具 fallback 或模型／工作重跑 |
| 偽造身份、source、hash、runtime-owned field、receipt 或 artifact | 共用 admission 拒絕；不能藉由更換傳輸繞過 local 原有驗證 |
| 缺 required evidence、來源不符、schema drift、超大內容 | preflight 或 admission fail closed；未獲授權來源不進 prompt，不裁切必要證據 |
| `partial`／`failed`／`blocked`／`completed_with_gaps` | 保留誠實狀態，驗收依原 contract；不以 collector 成功回傳取代 TaskDone gate |
| verifier failure／terminal failure／isolated apply conflict | 與舊路徑相同 recovery 分類，不因收集成功掩蓋失敗 |
| pending／in-flight／received／prepared／committed 各 crash point | 不重置 collection budget，不重播工作；收到有效 durable response 或 prepared bytes 時不需模型 |
| operator result-only grant、重複 grant、無 result contract、隔離 workspace | 只可依明確授權增額或本地提交；不可換 target、重用 grant 或藉新 checkpoint 放寬隔離限制 |
| artifact materialized → result commit、result commit → TaskDone 間故障 | 不重複 artifact 副作用或 workspace apply；occurrence 僅一份 accepted result |
| concurrent submit、event append failure、錯誤分支／revision／dispatch | projection 不先於 durable event；stale／duplicate 不造成第二份 accepted result |
| direct-agent、subagent、extra-model leaf、plan-first、unattended、dry-run | 各入口使用正確政策與權限；dry-run 不呼叫 worker／collector／工作工具 |
| legacy snapshot／closed sequence／顯式 tool／external proposal／runtime action | 原協定與既有成功／失敗語意保留；action 不新增 collector，external 不被強迫走 local tool |
| HTTP request 與 model-call chokepoint | runtime 工作請求不含 submit_result；collector request 的 tools 為空；模型呼叫有 manifest、完整 budget／usage／cancel admission |
| CLI／JSON／report／inspect／TUI 與 replay | 清楚區分執行與收集進度／錯誤；顯示層不合成完成狀態，不揭露 raw CAS 內容或秘密 |

至少用兩個泛用 workflow fixture 驗證 team-neutral 行為：`coder → reviewer → verifier` 與 `researcher → evidence consumer`。可用 fake external backend 混合 local worker；不能只測單一團隊名稱或把真實 Codex／Ollama 服務作必備 gate。

另以確定性 fixture 比較 tool 與 runtime 路徑：記錄 `submit_result` 次數、工作工具執行次數、collection／repair 回合、模擬 provider calls 與 fake usage。這是回歸證據，不是實際模型成本或可靠性量測；不得由此宣稱節省 token、零格式錯誤或適用所有供應商。

### 驗證命令與資源規範

每個修改程式碼的 PR，先跑實際涉及的 behavior tests，再依序執行以下命令；全部通過才能完成。針對 occurrence／event／collector race 補 targeted race tests。不能將空匹配 `-run` 視為通過。

```bash
mise run test
mise run vet
mise run lint
```

遵守 [AGENTS.md](../../AGENTS.md)：Go 指令一律經 mise、`GOMAXPROCS=2`、build/test/vet 使用 `-p 2`；重型驗證不並行，也不與 live team run 重疊。所有 disposable binaries／logs／fixtures 放同一 private `/tmp/codex-task.XXXXXX`，對 subprocess 設 `TMPDIR`，保留原 `GOTMPDIR` 與 caches，檢查各 volume 與記憶體餘裕，結束清理。Socket fixtures 使用既有短路徑 helper，不能靠改名或跳過測試避開問題。

## 8. 正式文件、回復方式與交付

- 實作時同步 [execution-runtime.md](../architecture/execution-runtime.md)、[agent-format.md](../reference/agent-format.md)、[strict-verification.md](../architecture/strict-verification.md) 與必要的 config 範例；明確記錄新 session 預設、legacy 例外、額外模型呼叫、收集上限與 resume 語意。
- 依 [workset.md](../architecture/workset.md) 保留 workspace／artifact scope 與來源綁定；參照 [歷史結果協定事故](../postmortems/2026-08-04-worker-result-protocol-deadlock.md)，以回歸測試防止工具可見性、授權與指示分歧。
- 新 event／receipt／snapshot 欄位要列 revision、舊資料 absent 語意與 branch replay 測試。不要改寫歷史 event 或就地把舊 session 切換 transport。
- 回復使用方式：新 session 可明確 `result-transport: tool`；已開啟的 session 維持 frozen transport。舊二進位無法理解新 event 時需保留新版程式與原 workspace，不把 downgrade／改資料庫列成自動回復方式。
- 每個 PR 回報具體行為差異、檔案、fixture、通過／失敗命令、event／schema／receipt 差異與回復限制。不要把架構重構本身當成已減少結果工具呼叫的證據。

**Definition of Done**：新一般 local task 預設由 runtime 發起無工具的結果收集；正常與修復皆無 `submit_result` 呼叫；工作證據先於收集持久化；收集及恢復不重播工作；只有完整 admission、verification、apply 與 acceptance 通過才完成任務。相容工具協定的使用範圍有明確邊界，無真實服務或人工操作前置條件。

## 9. Baseline 驗證與實作進度

2026-10-27，在修改任何 Go 程式之前完成驗證。程式碼為本文件 baseline `c8c52d30`；執行完整測試時 HEAD 為只加入本計畫與文件索引的 `51148852`，Go 程式與 baseline 相同。

| Gate | 結果 |
|---|---|
| `mise run test` | exit 0；完整套件通過，部分沿用 Go test cache；`internal/team` 實跑 512.063s，`cmd/hufu` 112.157s |
| `mise run vet` | exit 0 |
| `mise run lint` | exit 0，0 issues |
| `bin/check-docs` | exit 0，生命週期標頭、相對連結與 Go function anchors 有效 |

所有 Go gates 使用 `GOMAXPROCS=2`、repository mise toolchain 與原設定 caches／`GOTMPDIR`；subprocess `TMPDIR` 使用本任務 private directory。第一次 sandbox 測試因禁止 local socket，導致 `httptest` 等 fixtures 失敗；完整重跑允許本地 HTTP／Unix socket fixtures 後全數通過。此環境失敗不列為程式 baseline 缺陷，後續需要 fixtures 的驗證須使用同等執行權限。

### PR-1：完成 local admission 抽取

- [Go function: admit](../../internal/team/result_admission.go) 接收已解碼 claims 與 adapter 從 durable task 取得的契約，以 typed rejection 回報結果，不依賴 Fantasy tool call／response。既有工具 adapter 繼續負責 schema guidance、解碼、相容 normalization 與錯誤映射。
- 保留原 schema／receipt／HMAC／invariant／artifact／occurrence validation 與 commit 順序；不新增配置、事件、receipt 欄位、result source 或公開 API，不改 default，也不接管 lifecycle。
- 原工具檔由 990 行縮至 744 行，移除已不需要的 complexity suppression；新 admission 核心 320 行。
- 新增工具／直接入口的 12 類案例對照：成功、gaps、partial／failed／blocked、缺摘要、非法狀態、缺 artifact、偽造 receipt、stale occurrence、materialization 後 assertion failure、sink failure。驗證 canonical handoff 等價、artifact 精確 hash／bytes、拒絕不發布結果或遺留 reservation，提交不直接完成任務。

2026-10-27 驗證結果：

| Gate | 結果 |
|---|---|
| `mise exec -- go test -p 2 ./internal/team -run 'Test(LocalResultAdmission\|SubmitResult\|SubmissionGate\|SubmissionFailure\|ResultCandidate\|PreparedCandidate\|OccurrenceTransition\|StaleSubmitResult)' -count=1` | exit 0，10.996s |
| `mise run test` | exit 0，完整套件通過；`internal/team` 540.875s |
| `mise run vet` | exit 0 |
| `mise run lint` | exit 0，0 issues |

新預設與 collector 尚未啟用，不能將本階段視為已達成零工具結果收集。

### PR-2：收集 checkpoint 與本地恢復

- 在既有 receipt／CAS／Event Store 增加有版本的收集 metadata，保存完整原工作 receipt、報告、transcript 身份、政策、不可變 occurrence contract、輸入、來源與 workspace 綁定；不要求任務具有 result contract。
- 以事件先行、單調 revision 與 idempotency payload 比對更新進度；模型回合在派發前持久化保留，兩個競爭者只有一個取得回合。中斷重發仍計入原本的兩回合上限，同一個執行中的 invocation 不可重發。
- 收到的原始回覆、prepared claims 與已提交 canonical result 都有獨立 CAS 綁定。prepared claims 經共用 admission 本地提交，不建立 provider 或假造工具呼叫；提交事件成功前不發布結果，也不設定 TaskDone。
- Reducer 還原原提交 lease，prepared fixture 從事件重建全新的 Todo projection 後直接提交；晚到的工作 receipt 不會清除收集進度或 operator grant 歷史。新增 inspect 的收集狀態、回合上限與固定失敗原因欄位，不輸出原始內容。
- 延伸既有 result-only grant，支援無 result contract 的 collection failure，綁定原 target 與新的提交 lease；不允許 replacement model，保留隔離 workspace 的既有禁止條件。來源或契約拒絕不能藉 grant 繞過。
- 本階段建立供 collector 使用的設施，尚未接入 worker／一般 resume 調度，也未啟用新預設；PR-3 與 PR-4 仍待完成。

2026-10-27 驗證結果：

| Gate | 結果 |
|---|---|
| targeted checkpoint／admission／inspect tests | exit 0；`internal/team` 1.834s、`internal/inspect` 0.026s |
| targeted race tests | exit 0；涵蓋競爭 reservation、事件重建後的 prepared commit、crash budget 與 operator grant |
| `mise run test` | exit 0；最終完整套件通過，`internal/team` 533.805s、`cmd/hufu` 109.330s |
| `mise run vet` | exit 0 |
| `mise run lint` | exit 0，0 issues；已修正一次 whitespace finding |
| `bin/check-docs` | exit 0 |


### PR-3：無工具 collector 與各入口恢復（完成）

- 新增 `result_collection` invocation purpose，以實際工作 target 的普通文字生成取得 strict JSON；使用原 worker 的 generation／timeout、backend semaphore、provider admission、成本、token 與取消邊界。collector 的 HTTP request 沒有工具；模型幻覺工具呼叫在 handler 前拒絕，僅使用原來的格式修復額度。
- 透過封存的 internal typed policy seam 選用 runtime，分開「需要 typed result」與「授權 submit_result」。新來源為 `collected`；共用 admission、驗證、terminal 與 workspace apply 仍決定完成。公開 YAML 與新 session 預設留到 PR-4。
- 一般 worker、direct-agent、subagent 與 fanout leaf 接入同一套收集／恢復。工作回合返回後的 transcript／CAS／事件故障走停止與結果恢復，不回到 worker retry；過時的 submit_result 嘗試保留拒絕證據，handler 執行數為零。
- received／prepared／committed 優先使用 durable bytes。fanout 以凍結 topology slot 恢復該 leaf，保留其 workspace 直到 parent 完成；隔離 world 依原始世界身份重開，缺失時停止，不能改選新 world 或重跑工作。
- fanout leaf 的 artifact 從原工作區讀取並存入 parent CAS；測試在 leaf 清除後重新開啟已接受的 artifact 並比對完整內容。新增 policy 欄位明列為 improver 固定政策，避免自動探測改變結果傳輸契約。
- verbatim 結果在 collection commit 前附上原生 transcript；重複封存沿用 CAS canonical metadata，完成流程不再改變時間戳或重複 signed evidence。收集用量保存 cache counters；工作統計在 collector 前固定，避免進度 token 重複記帳。
- 取消或 collection event append 故障可保留較早的 checkpoint，由原生 failure receipt 支援既有 result-only continuation；storage continuation 的 grant 增額為零，沿用剩餘原額度。已 committed 的 grant 保留 accepted 狀態，只續行完成檢查。來源／契約拒絕及隔離 workspace 的既有 grant 限制不放寬。
- 本地 HTTP／scripted fixtures 覆蓋無工作工具、正常／一次修復／耗盡、collector 幻覺工具、過時報告工具、真實 bash 副作用計數、封存 writer 故障、direct／subagent／fanout／isolated／verbatim、durable response／prepared／accepted 恢復、operator grant 與取消／token budget。plan-first、unattended、dry-run 及公開新預設的整合驗收仍屬 PR-4。

2026-10-27 驗證結果：

| Gate | 結果 |
|---|---|
| focused collector／checkpoint／transport／transcript／provider admission tests | exit 0，最後修正後 19.599s；涵蓋 fanout artifact 保存，先前已通過 model-call inventory 與 gated constructor checks |
| `mise run test` | exit 0；最後修正後完整套件通過，`internal/team` 529.525s、`cmd/hufu` 113.734s、`internal/improver` 154.931s |
| targeted race tests | exit 0；`internal/team` 57.794s、`internal/inspect` 1.091s |
| `mise run vet` | exit 0 |
| `mise run lint` | exit 0，0 issues；以 protocol collision helper 分解複雜度，未放寬 lint 規則 |
| `bin/check-docs` | exit 0；86 active files、540 relative links、38 Go anchors |
