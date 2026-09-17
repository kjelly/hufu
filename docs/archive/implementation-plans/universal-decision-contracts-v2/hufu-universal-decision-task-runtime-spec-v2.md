# Hufu Universal Decision Task Runtime — 精確契約版 v2

> Status: Implementation-ready contract；不是已完成實作報告。
> 日期：2026-09-17。查核基準：`c4899849bb46c2f2acb1316ba74e2f0f681ff884`。
> 完整取代：`hufu-universal-decision-task-runtime-spec.md`。前版的架構目標保留，未定義的行為及下列衝突由本版明文替換。
> 交付對象：coding agent。本文只保留能以 repository 內程式碼、fixtures、fake backends 與自動化測試完成及驗收的工作。
> **核心原則：Decision-aware runtime 不應定義「哪個 team 能做決定」；它應定義「任何 team 在執行決策型任務時，必須滿足什麼決策契約」。**

## 0. 範圍、來源與規範層級

Team、任務意圖與決策嚴謹度是三個正交維度。`hufu decide --team X` MUST 使用 X 的 loader、workspace、權限、模型解析、supporting tasks（支援任務：蒐集或驗證決策資料），最後由既有 DecisionEngine 形成決策。不得替換成 strategic-decision/default team，不新增第二個 scheduler、event store 或 TaskKindDecision。

本版六份契約依序為：①邏輯執行身分與事件；②共同收尾狀態機；③runtime-owned primary occurrence；④證據 schema 與裁切；⑤V2 literals 與角色綁定；⑥CLI、公開 DTO、遮罩與 exit codes。第七章提供跨契約驗收與實作順序。

**規範用語：** MUST／MUST NOT 是驗收硬條件；SHOULD 只用於不影響正確性的實作選擇。本文的新增型別、事件與旗標是implementation-ready contract，不能當成目前binary已支援。本文列出的具體預算與裁切上限是固定產品預設值，不是效能實測結果。

**來源分層：** 前版文件提供「任意 team + decision intent」需求；[S01]–[S13] 是本次讀取的程式碼基準；本版新增狀態、schema、預設值與錯誤規則是固定實作決定。只做過所列檔案的靜態查核，不宣稱功能已實作。不得將 README 歷史說明優先於實際 writer/reducer 程式。

**可自動完成邊界：** coding agent 不必取得產品決策、人工核准、真實 provider 帳號、production workspace 或外部安全稽核才能完成本規格。Provider／backend 整合只需實作 capability declaration、fail-closed preflight、fake backend 與 deterministic fixtures；沒有在程式中可驗證 capability 的真實 backend 一律回 `decision_backend_capability_unverified`，不屬於本次「再人工證明後放行」的工作。所有 required acceptance 必須能由本機 test command 判定。

### 0.1 必須明文取代的前版假設

| 前版 | 本版決定 |
|---|---|
| `run_id + question digest` 足以識別重啟 | 分開 logical_run_id 與每次 invocation 的 execution_run_id；問題內容不是唯一鍵。 |
| 固定 `__hufu_primary_decision__` | ID 納入 logical run + branch；固定字串只可作 UI 標籤。 |
| 只在 `finish()` 加 primary | 所有 terminal entry points 共用入口；成功準備階段才允許新模型工作。 |
| primary 一旦存在即可重用 | 驗證 requirement、generation、support revision、evidence、bindings；stale 必須失效。 |
| 成功時 exactly one primary，未區分修訂 | 一個 logical run 只有一個 primary slot；可有多個歷史 generations，但完成時只有一個有效 binding。 |
| 只收 completed task、以四個互斥等級表達證據 | 來源、完整性、驗證與知識狀態分開；保留失敗 assertion、反證與失效事件。 |
| 原始 request 必須逐字持久化 | 非敏感內容逐字保留；secret 先替換為安全參照／遮罩，再固定 canonical request；不能為逐字保存而落盤明文憑證。 |
| `decide --decision-profile` 改成 primary 語意 | 所有 commands 的同名 flag 保持原本 per-task 語意；primary 使用 `--primary-decision-profile`／`--rigor`。 |
| `--route auto` 就是 auto team selection | route 與 team selection 分開；不得拿 execution route 冒充團隊選擇。 |
| fallback 可在任何失敗後啟用 | 只在預先允許的「沒有匹配候選」情況啟用；不得繞過授權、policy 或 runtime 錯誤。 |
| V2 只有概念參數 | §5 與附件列出完整、無繼承、無省略的三份 catalog literals。 |
| 同模型三次呼叫等同三個獨立專家 | 僅保證 context isolation；實際 agent/model/provider diversity 與統計獨立性分開報告。 |

### 0.2 已有邊界，禁止重建

目前 `FinalizeRun` 已經是共用 terminal path，具有 candidate election、單一 `run_finished` writer、bounded cleanup context 與持久化確認後才更新 projection 的規則。[S01][S02] `execution_run_id` 本來用來區分共用 workspace 的 successive invocations；canonical journal 是 `logs/event_store.jsonl`，不是 `execution-events.jsonl` shadow。[S03][S04]

必須沿用 `DecisionEngine.Run/Resume`、DecisionAdmission、ArtifactStore、EventJournal、CompletionGate、TaskKindOutcome 與原本的 task recovery。[S05][S06] 新功能是同一 runtime 的 contract extension，不是另一套工作引擎。

### 0.3 整體流程

```text
CLI/API request → resolve same TeamSession → persist logical requirement
    → supporting tasks / optional auxiliary decisions
    → common success-preparation gate
    → immutable evidence + role plan → primary admission → existing DecisionEngine
    → persist record → bind primary → combined acceptance → final manifest
    → existing single-owner run_finished commit → output projections
```

DecisionRecord 表示「做出了何種決定」，不授權直接執行被選中的方案。`defer`、`abandon`、`request_information` 可以是有效決策；必須與「無法滿足決策契約而 blocked」區分。團隊既有強制 acceptance/workflow 不得默默移除；不相容時明確 blocked。

### 0.4 明確移除的工作

下列事項不在implementation backlog，不得建立placeholder、manual gate或「日後人工確認」task：真實世界決策正確率、forecast校準研究、不同模型的統計獨立性證明、production部署、liveprovider認證、外部安全稽核、使用者研究、成本／延遲benchmark及既有workspace資料migration。Runtime只需誠實輸出已觀察的agent/model/providerdiversity、阻擋未宣告capability的backend，並通過本規格的deterministic tests。未來若要加入上述項目，必須另立規格，不阻塞本版完成。

---

# 1. Logical decision run identity、events、reducers 與 crash matrix

## 1.1 身分模型

| 欄位 | 精確定義與範圍 |
|---|---|
| `logical_run_id` | `ldr_` + 16 bytes crypto-random 的 32 位 lowercase hex。接受一個新的決策請求時只生成一次；entropy failure 直接報錯，禁止 timestamp fallback。 |
| `execution_run_id` | 沿用現有 invocation ID。程序重啟／重新進入 Run() 可產生新值；不得拿它重建 logical identity。 |
| `branch_id` | 初始 requirement 所屬 branch。logical run 永不跨 branch 重綁。 |
| `team_id` | `team_` + `H("hufu/team-identity/v1", {name: strings.TrimSpace(session.Config.Name)})`；空名稱在opened前拒絕。授權一律另外核對`team_definition_digest`與`authority_snapshot_ref`，不得只以此ID放行。 |
| `requirement_digest` | 對安全 canonical request、team、primary profile/bundle、允許的 evidence imports、budget/authority envelope 的 hash。不是 request dedup key。 |
| `primary_task_id` | `__hufu_pd_` + `H("hufu/primary-task/v1", {branch_id, logical_run_id})` 的完整 64 位 hex。 |
| `generation` | 初始為 1；只有 durable invalidation 後才可遞增。相同輸入的 transport retry 不遞增。 |
| `decision_id` | 每 generation 固定：`pd_` + `H("hufu/primary-decision/v1", {logical_run_id, branch_id, generation})`。 |
| `invocation_attempt` | 同一 role/ordinal/round 的實際 provider 呼叫次數，從 1 開始，與 generation、task retry 分開。 |
| `owner_epoch` | workspace writer lock 內，依 verified journal 的既有最大值 + 1；防止舊 owner 回寫。 |

`team_definition_digest` MUST直接沿用目前`teamDefinitionRevision(session.Dir)`的lowercase SHA-256結果；函式可移到共用package，但輸入檔案集合、排序及byteframing不得改。Decisionintent preflight先逐一讀取該函式會納入的檔案，任一讀取錯誤或空digest都失敗，不能沿用現有「略過不可讀檔」的telemetry容錯。`H(domain, object)`的byte-level定義在§1.3。ID不取決於prompt、模型輸出、PID、時間或目前workingdirectory。

**同一句問題呼叫兩次是兩個 logical runs。** 只有顯式 `--resume-decision <logical_run_id>` 恢復既有 run。不得依「workspace 中最後一份 record」或相同 prompt hash 自動選 run。

新 request 若撞上未確認的 terminal commit、仍有 unresolved side effects 或活躍 writer，先遵守原安全 admission；`--new`、新 logical ID 都不能繞過。歷史上已中止／suspended 的其他 logical runs 不會自動成為本次 evidence。

## 1.2 固定 runtime schema（新增，非目前 API）

```go
type LogicalDecisionRun struct {
    SchemaVersion       int                   `json:"schema_version"` // 1
    Kind                string                `json:"kind"` // logical_decision_run
    LogicalRunID        string                `json:"logical_run_id"`
    BranchID            string                `json:"branch_id"`
    TeamID              string                `json:"team_id"`
    TeamDefinitionDigest string               `json:"team_definition_digest"`
    RequirementRef      ArtifactRef           `json:"requirement_ref"`
    RequirementDigest   string                `json:"requirement_digest"`
    ProfileBundleRef    ArtifactRef           `json:"profile_bundle_ref"`
    ExecutionRunIDs     []string              `json:"execution_run_ids"` // attach order
    OwnerEpoch          uint64                `json:"owner_epoch"`
    Phase               string                `json:"phase"`
    CurrentGeneration   uint32                `json:"current_generation"`
    ActivePrimary       *PrimaryBindingV1     `json:"active_primary"`
    LastAppliedEventID  string                `json:"last_applied_event_id"`
    LastAppliedHash     string                `json:"last_applied_hash"`
    Usage               LogicalDecisionUsage  `json:"usage"`
}

type PrimaryBindingV1 struct {
    SchemaVersion       int         `json:"schema_version"` // 1
    LogicalRunID         string      `json:"logical_run_id"`
    BranchID             string      `json:"branch_id"`
    TaskID               string      `json:"task_id"`
    Generation           uint32      `json:"generation"`
    DecisionID           string      `json:"decision_id"`
    RequirementDigest    string      `json:"requirement_digest"`
    SupportRevisionDigest string     `json:"support_revision_digest"`
    AdmissionRef         ArtifactRef `json:"admission_ref"`
    BaseEvidenceRef      ArtifactRef `json:"base_evidence_ref"`
    RolePlanRef          ArtifactRef `json:"role_plan_ref"`
    RecordRef            ArtifactRef `json:"record_ref"`
    SealedEvidenceHash   string      `json:"sealed_evidence_hash"`
}
```

`LogicalDecisionUsage` 的完整欄位為 `tokens_used:uint64`、`reserved_tokens:uint64`、`active_duration_ms:uint64`、`generations_started:uint32`、`invocation_settlements_ref:ArtifactRef|null`。Logical schema 在 `logical-run.schema.json`；settlement ref 指向既有 receipts 的唯一索引 projection。它是既有 budget/receipts 的 projection，**不是第二個累計帳本**。重新 attach 不歸零 tokens、unknown reservations 或已用 decision active time；程序不執行的間隔不算 active time，使用 durable invocation intervals/receipts 計算，unknown interval 保守計入當次上限。

Requirement artifact 的 strict wire contract 是 `runtime-contracts.schema.json#/$defs/DecisionRequirementV1`。必填欄位：`schema_version=1`、`kind=decision_requirement`、`logical_run_id`、`branch_id`、`team_id`、`team_definition_digest`、`original_request_safe`、`request_was_redacted`、`request_contract_ref`、`primary_profile_ref`、`profile_bundle_digest`、`authority_snapshot_ref`、`allowed_import_refs[]`、`effective_limits`、`intent=decision`。所有 refs 帶內容 digest；不得只存 workspace path。

`authority_snapshot_ref` 必須解析成 `runtime-contracts.schema.json#/$defs/DecisionAuthoritySnapshotV1`。它只列通過既有policygate的targets，不保存credentialvalue；targets依`target_id`升序且不可重複，每個`capabilities`依UTF-8bytes升序且不可重複。`tool_isolation=unknown|unrestricted`或`session_isolation=false`的target仍可供一般supportingworker使用，但不能綁到要求tool-less isolation的decisionrole。Resume以snapshot固定identity，同時以現行policy重驗deny；snapshot不能授權現況已拒絕的target。

RequestContract 仍要求 objective 與至少一個 success criterion；前版的「只有 OriginalRequest 必填」不直接 bypass 現有 validator。[S07] Runtime 注入的是可驗證的 process criterion：指定 requirement 下的有效 primary record 已持久化，而不是自動認證決策正確。Team/user criteria 是額外 AND 條件。

## 1.3 Canonical bytes、digest 與版本

新 contract metadata 使用 `hufu-json-c14n@v1`：UTF-8；object keys 依 UTF-8 bytes lexicographic 排序；array 順序具有語意；不加空白或換行；字串只 escape JSON 必要的 `"`、`\\`、U+0000..001F（控制字元採 `\b\t\n\f\r` 或 lowercase `\u00xx`）；不 escape `<>&`、非 ASCII、U+2028/2029；不接受未配對 surrogate。數值只允許 JSON-safe 整數 `0..2^53-1` 或 schema 明列的 signed integer。一般 JSON fact 以 `content` 字串承載，不在 metadata 中轉浮點。

`H(d, x) = lowercase_hex(SHA256(UTF8(d) || 0x00 || CanonicalJSON(x)))`。被計算物件不可含自己的 digest。ArtifactStore 的 `sha256` 仍是**實際儲存 bytes** 的 hash，不能拿帶 domain 的 H 冒充；兩者欄位名稱與用途必須區分。

既有 `CanonicalDecisionPolicy` / `PolicyDigest`／DecisionEvidencePacket hash **不改演算法**；新增 `bundle_digest` 與 `base_evidence_ref.sha256` 不取代原 digest。V1 fixture 的既有 digest 必須 byte-identical。

### 1.3.1 新 digest／ID registry（不得自行另選 domain 或欄位）

下表的 object key 使用表列順序不影響 canonical bytes；陣列排序規則是 hash 輸入的一部分。`ArtifactRefIdentity` 固定為 `{id,sha256,media_type,size_bytes}`。所有 hex 是 64 位 lowercase。

| 名稱 | domain | canonical object |
|---|---|---|
| `team_id` hash 部分 | `hufu/team-identity/v1` | `{name}`，name為trim後非空team config name，保留大小寫及Unicodebytes。 |
| authority `policy_digest` | `hufu/decision-authority-policy/v1` | `{security_flags,targets}`，內容與 `DecisionAuthoritySnapshotV1` 相同；targets及capabilities先依上述規則排序。 |
| `requirement_digest` | `hufu/decision-requirement/v1` | 完整 `DecisionRequirementV1`；物件本身不含 digest。 |
| `primary_task_id` hash 部分 | `hufu/primary-task/v1` | `{branch_id,logical_run_id}`。 |
| `decision_id` hash 部分 | `hufu/primary-decision/v1` | `{branch_id,generation,logical_run_id}`。 |
| `lineage_digest` | `hufu/event-lineage/v1` | `{branch_id,event_hash,event_id}`，三值取 support cursor 指向的 verified event。 |
| `source_index_digest` | `hufu/decision-source-index/v1` | `{branch_id,logical_run_id,sources,task_occurrences}`；`sources` 是 eligible `ArtifactRefIdentity` 按 `(sha256,id,media_type,size_bytes)` 排序；`task_occurrences` 是 `{task_id,occurrence_revision,result_event_id}` 按三欄 bytewise 排序；兩陣列不得重複。 |
| `support_revision_digest` | `hufu/decision-support-revision/v1` | `{branch_id,lineage_digest,logical_run_id,source_index_digest,support_cursor_event_hash,support_cursor_event_id}`。 |
| `preparation_digest` | `hufu/decision-preparation/v1` | `{base_evidence_ref,bundle_digest,generation,logical_run_id,requirement_digest,role_plan_ref,support_revision_digest}`；refs 使用 `ArtifactRefIdentity`。 |
| `binding_id` | `hufu/decision-role-binding/v1` | `{agent_definition_digest,agent_id,execution_mode,execution_target_ref,generation,logical_run_id,model_identity,ordinal,provider_identity,role,role_instruction_ref}`；nullable 值必須保留為 JSON `null`。 |
| evidence `item_id` hash 部分 | `hufu/decision-evidence-item/v1` | `{collector_version,json_pointer,source_artifact_sha256,source_event_id,task_attempt,task_id,task_occurrence_revision}`；nullable 值保留。 |
| independence `group_id` hash 部分 | `hufu/decision-independence-group/v1` | `{root_identities}`；identity 去重後按 UTF-8 bytes 排序。 |

`base_evidence_ref.sha256` 就是 base evidence artifact 實際 stored bytes 的 SHA-256；不得再建立同義 `BaseEvidenceHash` 演算法。`sealed_evidence_hash` 沿用既有 DecisionEvidencePacket hash。附件 `canonical-test-vectors.json` 必須為team ID、authority policy、requirement、primary task、decision、lineage、source index、support revision、preparation、binding、item與group各保留至少一個向量；Go測試逐項重算。

## 1.4 Writer 與持久化順序

唯一 authority：verified active-branch `RunEvent` stream。[S04] `session.json`、Todo、DecisionIndex、status、execution-events shadow 都只能投影。Reducer 不呼叫模型、不讀 mutable team config、不查網路、不產生新的 ID。

所有相依 artifact MUST 經既有 content-addressed store 的 durable write/resolve，確認資料與必要 rename/fsync 邊界後，才能 append 引用它的事件。檔案与 event log 不宣稱是跨檔 transaction。Crash 後未被 event 引用的 artifact 是 orphan，不能由「最新檔案」推進狀態；GC 只能走既有安全策略。

每個 mutating invocation MUST 持有 workspace/branch 的跨程序 writer exclusion。現有 EventStore 的 in-process semaphore 不等於整個 logical operation 的跨程序 lock；優先整合現有 writer/resource lease，不足才在相同 storage package 增加本機鎖。持鎖範圍從 attach/preparation 到 invocation 結束；不以 TTL 偷走仍存活的 owner。每次成功 attach 遞增 owner_epoch；事件與 provider dispatch 在接受前核對 epoch。舊 goroutine 的 late response 可以留下有來源的診斷 artifact，但不能推進新 epoch 的 primary 或 terminal result。

Append error 不等於確定「沒有寫入」。發生 write/sync timeout、partial append 或 acknowledgment 遺失：停止 provider dispatch，進入 recovery-required，依 hash chain + branch + key + payload identity reconciliation。未知的 durable boundary 絕不可當成 no-op 重試。

## 1.5 精確 event contract

沿用 RunEvent envelope schema 2；下列**新增 payload schema_version=1**，snake_case 名稱。每個 payload 均必填 `schema_version`、`logical_run_id`、`branch_id`、`execution_run_id`、`owner_epoch`；generation-related event 另必填 `generation`。未知欄位／未知版本對這組 correctness events MUST 拒絕，不可當普通 telemetry 忽略。

`RunEvent.RunID` 是產生該事件的 physical execution；logical 身分放 payload。恢復前先按既有規則解決上一 invocation 的 pending `run_finished`，不能重寫其 RunID。[S02][S03]

| Event | 額外必填 payload（除共通欄位） | 前置條件／reducer 效果 |
|---|---|---|
| `decision_run_opened` | `team_id, team_definition_digest, requirement_ref, requirement_digest, profile_bundle_ref, authority_snapshot_ref, effective_limits` | 本 branch 的 logical ID 不存在；建立 SUPPORTING、generation=0、無 binding；同時完成首個 physical invocation membership，owner_epoch=1。 |
| `decision_run_attached` | `previous_execution_run_id, resume_from_event_id` | requirement 已存在，logical 未 CLOSED；writer epoch 增加；把 physical ID 加入允許 lineage。 |
| `primary_decision_prepared` | `task_id, decision_id, support_cursor, support_revision_digest, base_evidence_ref, role_plan_ref, preparation_digest` | supporting settled；保存精確 immutable preparation，不啟動 provider。 |
| `primary_decision_admitted` | `task_id, decision_id, occurrence_ref, admission_ref, preparation_digest` | 前一 prepared 有效；**同一 event** 對 primary reducer 與 task reducer 建立 occurrence/admission，無第二個 task_created authority。 |
| `decision_role_call_started` | `decision_id, role, ordinal, round, invocation_attempt, binding_id, invocation_id, input_digest, reservation_ref` | binding/輸入固定、budget reservation 成功；可 dispatch 的 durable 邊界。 |
| `decision_role_call_unconfirmed` | `invocation_id, reason_code, reservation_ref` | started 存在；結果未知，只保留 reservation，不能把未知記為不可更改的 settled。reconcile 後可補唯一 settled。 |
| `decision_role_call_settled` | `invocation_id, status, receipt_ref, output_ref`（失敗可 null）, `usage_state, tokens_used, reason_code` | started 存在；status=`accepted|rejected|failed`；只按唯一 receipt 計費。 |
| `primary_decision_blocked` | `task_id`（未 admitted 可 null）, `reason_code, support_revision_digest, repairable, missing_requirements[]` | 只表示該準備失敗；**不是** logical terminal。相同輸入指紋不重跑模型。 |
| `primary_decision_bound` | `binding`（§1.2 的完整值）, `record_validation_receipt_ref` | record、admission、packet、role plan 匹配；generation 未 invalidated；該 slot 無不同 active binding。 |
| `primary_decision_invalidated` | `task_id, decision_id, prior_binding_event_id`（可 null）, `trigger_event_ids[], reason_code, next_generation` | logical 未 CLOSED；引用新的實質 support/assumption evidence；next_generation=目前+1；清除 active binding，保留歷史。 |
| 既有 `run_finished` 的擴充 | `decision_run` 必須符合 `runtime-contracts.schema.json#/$defs/RunFinishedDecisionExtensionV1` | `disposition=closed` 只接受通過 §2 的 completed；否則 `suspended`。不再另寫 decision_run_completed 造成雙 terminal commit。 |

`receipt_ref`/`output_ref` 引用既有 execution receipt / engine stage artifact。這兩個 role-call events 只補 dispatch identity 與 receipt 關聯，不另保存一份 opinion，也不得同時在兩處增加 usage。若基準 implementation 已提供等價 started/settled event，實作者 MUST 以 schema adapter 統一成同一 logical event，不雙寫第二套計費／replay authority；PR-0 必須列出確切 mapping。

每個 `missing_requirements` 是 `{code, criterion_id:null|string, required_count:int, observed_count:int}`。`reason_code` 是穩定 enum，不以任意 error prose 驅動 reducer。新的未知 correctness event 使該 logical lineage 進入 recovery-required；不影響不相干的舊 non-decision branch。

## 1.6 Idempotency keys 與 reducer 規則

Key 格式（全部 prefix 固定 `udr:v1`）：

```text
open        udr:v1:<L>:opened
attach      udr:v1:<L>:attach:<execution_run_id>
prepare     udr:v1:<L>:g:<G>:prepared:<preparation_digest>
admit       udr:v1:<L>:g:<G>:admitted
call-start  udr:v1:<L>:g:<G>:<role>:<ordinal>:r:<round>:a:<attempt>:started
call-unknown udr:v1:<L>:g:<G>:<role>:<ordinal>:r:<round>:a:<attempt>:unconfirmed
call-settle udr:v1:<L>:g:<G>:<role>:<ordinal>:r:<round>:a:<attempt>:settled
blocked     udr:v1:<L>:g:<G>:blocked:<support_revision_digest>:<reason_code>
bound       udr:v1:<L>:g:<G>:bound
invalidate  udr:v1:<L>:g:<G>:invalidated
terminal    沿用 run_finished:<execution_run_id>
```

EventStore key scope 已包含 branch。[S04] 同 key + 同 **business payload** 返回原 durable event；同 key + 不同 business payload → `decision_idempotency_conflict`，不可 last-write-wins。transport `execution_run_id/owner_epoch` 不屬 business equality，可在 replay 命中原 event 後保持原 transport metadata；內容 refs、requirement、generation、result、usage 不能排除於 equality。新 append 必須通過目前 owner fencing。

Reducer 輸入為按 journal 順序驗證的 events；同事件 ID 重播不再 apply。Logical projection 只依 opened/attached 的 membership 收集 tasks，不是 `event.RunID == current execution_run_id`。Nested task/attempt 必須帶 admitted logical owner 關聯；不得以來源為「同 workspace」放行。

CLOSED 後只有索引重建與結果讀取；新問題或實質修訂必須新 logical run。CLOSED 中若歷史資料損壞，標記 audit/recovery failure，不修改原 success event。新版本 reader 可以讀舊事件；**不承諾舊 binary 安全讀寫新增 logical events**，有新 intent workspace 不支援直接 binary downgrade。

### 1.6.1 Reducer 精確轉移與錯誤規則

Logical projection 的 `phase` enum 固定為 `SUPPORTING|PREPARED|ADMITTED|BOUND|SUSPENDED|CLOSED|RECOVERY_REQUIRED`；這不是 §2 每次 preparation 的 in-process 階段。`opened` 本身就是第一次 attach，`execution_run_ids=[首 RunID]`、owner_epoch=1；後續 attach 必須 epoch=目前+1、previous_execution_run_id 等於最後一個 member。

| Event | 允許的 projection pre-state | Projection after-state |
|---|---|---|
| opened | logical 不存在 | SUPPORTING，generation=0 |
| attached | SUPPORTING/PREPARED/ADMITTED/BOUND/SUSPENDED | 保留目前 primary phase；若 SUSPENDED 依最後有效 prepared/admitted/bound 事件恢復；新增 physical membership |
| prepared | SUPPORTING；或同 key 的 PREPARED replay | 首次 generation=1；否則等於 invalidation 已宣告 next_generation；PREPARED |
| admitted | PREPARED | ADMITTED；task projection 原子建立；generation 不變 |
| call started | ADMITTED | ADMITTED；建立唯一 call slot 與 reservation |
| call unconfirmed | ADMITTED 且 started 存在 | ADMITTED；該 call 結果未知，仍無可用 opinion |
| call settled | ADMITTED 且 started 存在、未有不同 settlement | ADMITTED；只由 receipt 結算一次；accepted result 仍須 engine 解析驗證才可進 record |
| blocked | SUPPORTING/PREPARED/ADMITTED | logical 回 SUPPORTING 或保持待復原的 ADMITTED；狀態由有無 admission 決定，不由 error prose 猜測 |
| bound | ADMITTED；或同 key BOUND replay | BOUND；active_primary 指定且與 current_generation 相同 |
| invalidated | PREPARED/ADMITTED/BOUND | SUPPORTING；active_primary=null；current_generation=next_generation；舊事件不更改 |
| run_finished(disposition=suspended) | 未 CLOSED | SUSPENDED，保留當前 primary snapshot；不表示 record 無效 |
| run_finished(disposition=closed) | BOUND + valid completion proof | CLOSED；後續不能重新 attach 執行 |

`primary_decision_blocked` 不授權從 ADMITTED 退回同 generation 重做輸入；只有未開始 provider、且完全相同 prepared bundle 可重試本機動作。要恢復 supporting 並改資料，MUST 先 invalidated，下一 generation 才能 admission。MAX generations 在 prepared 之前檢查，未通過 evidence preflight 的 generation=0 不耗一次模型 generation。

```text
Reduce(logicalProjection, verifiedEvent):
  validate strict payload version/type and branch/logical membership
  if seen same event id: require identical event hash; return unchanged
  if same business idempotency key exists:
      require same normalized business payload; return existing projection
  check legal pre-state, owner fence, declared generation and all referenced identities
  reject orphan stage, skipped generation, second active primary, foreign-run references
  apply exactly the field transition in the table
  update last_applied_event_id/hash only after the whole transition succeeds
  return detached projection; do not publish partial updates or call external services
```

這裡的「驗證 refs」在 replay hydration 的同一 read-only validation boundary 執行：reducer 自己只比對已提供、已驗證的 artifact/receipt metadata，不能在 pure reducer 中依現況重新下載資料。資料缺失或不匹配是 `RECOVERY_REQUIRED`，不能省略該事件繼續減出「成功」。

`run_finished` extension 的 `primary_binding_event_id`／`primary_record_ref` 在 closed 時不可 null，在 suspended 時可 null；閉合 event 必須與其 record proof 有同一 logical ID、generation、requirement、branch。整個投影可從 journal + immutable refs重建，session cache 全刪後結果一致。

所有 non-completed physical invocations 的 logical disposition 都固定為 `suspended`，這是刻意設計，不存在隱含的 `abandoned` 狀態，也不做自動 GC。Resume 規則固定如下：`closed` 一律拒絕執行；`active` 或 `suspended` 可 attach；cancel/provider/transient failure 可續缺失步驟；authorization/backend capability 必須以現況重新通過；max generation、相同 evidence 缺口或 mandatory overflow 只重播原 blocked 診斷且 provider call=0；journal/hash/schema corruption 只能進 recovery reconcile，不能 dispatch。刪除 workspace 是唯一刪除 suspended logical run 的方式，本規格不新增清理命令。

## 1.7 Resume 與 generation

Resume 順序固定：取得 writer exclusion → verified branch replay → reconcile pending terminal → 找指定 logical → 固定 requirement/policy/role snapshots → 現行權限及 backend 能力重新驗證 → 只補缺失步驟。模型、profile catalog 或 agent 檔案變了，不是重新解析舊 decision 的理由；舊 snapshot 不能再安全執行就 blocked，不偷偷換模型。

相同已 frozen generation 的 response 遺失：優先由原 provider receipt / idempotency token 取回。backend 不支援結果查詢時，只在 §5 已明示 bounded tool-less replay、reservation 足夠時增加 invocation_attempt 重跑同一 binding；原 unknown attempt 的額度保留。**只保證一個被採纳的 durable result，不保證外部模型恰好被呼叫一次。** 第一個合法且成功 committed 的該 slot 結果是 authority；不能為挑較喜歡的答案重新取樣。

重新蒐集實質 evidence、assumption contradicted、authorized acceptance repair 改變 support revision → invalidated event → 下一 generation，新 decision ID。generation budget 是整個 logical run 共用上限，不因 process restart 歸零。最大 generation 到達後 blocked，不能用建立同題 auxiliary 迴避。

## 1.8 Crash matrix（每列必須有故障注入測試）

| Crash window | 可相信的 durable authority | Resume 動作 | 禁止行為 |
|---|---|---|---|
| requirement artifact 完成，opened 前 | 無 logical event | artifact 保留為 orphan；新 request 可重新開始 | 從檔名發現並假裝已接受 |
| opened 後、checkpoint 前 | opened | reducer 重建 logical；explicit resume | 重新生成 logical ID |
| attached 後、support task 途中 | attached + task events | 沿既有 task reconciliation；沿用 logical | 換 physical ID 就跳過副作用恢復 |
| base evidence/role plan 寫完、prepared 前 | support events | 可重算同 deterministic bytes | 使用未引用的任意「最新」evidence |
| prepared 後、admitted 前 | prepared | validate refs，補同 generation admission | 選另一套 live profile |
| admitted 後、模型前 | admitted / occurrence | 補 started reservation，原 decision ID | 派给一般 worker或第二 primary |
| call started 後、provider 是否執行未知 | started + reservation | receipt reconcile；有限 tool-less replay或 blocked | 自動退款、換 reviewer |
| response artifact 後、settled 前 | receipt/engine已commit事件，或只有 orphan | 可證明相同 invocation 就補 settlement；否則 unknown | 把未驗證 prose 算有效 opinion |
| 部分 judge settled、其餘未完成 | 各 slot receipt | 只補缺失 slots；原 opinions不重算 | 完整重跑已成功 judges |
| record persist 後、bound 前 | engine final record事件+ref | 重新 validate 並補 bound | 再問一次 final winner |
| bound 後、acceptance 前 | bound | 核對 support revision；沒有變更可重用；重跑 acceptance | 直接宣布 completed |
| acceptance 修復造成新 evidence | invalidated | 新 generation，同 logical slot | 沿用舊 primary |
| manifest artifact 後、run_finished 前 | bound + validation / manifest refs | 唯一 terminal owner補 commit | 先輸出 success |
| run_finished sync 已發生但 ack 遺失 | journal 中精確 key/payload | reconcile committed terminal，修 projections | 另寫「較新」成功終態 |
| append 缺損或 hash chain 不明 | 不明 | recovery-required，0 新模型呼叫 | 把 timeout 當未寫入 |
| run_finished 確認後、JSON stdout 前 | run_finished | replay renderer；不執行任務 | 生成第二份 primary |
| SIGKILL | 最後確認事件 | explicit resume 檢查所處 window | 宣稱一定有 graceful terminal event |

---

# 2. 所有 terminal entry points 的共同 state machine

## 2.1 一個入口、兩個階段，不建平行 terminal writer

新增 `PrepareDecisionForTerminal` 作為既有 FinalizeRun 的前段共用service。所有正常／異常paths都必須經同一`RequestRunTermination` coordinator boundary；這兩個名稱及責任是固定contract。它先判斷可否做primarypreparation，再把immutablecandidate交給既有`FinalizeRun` / `commitTerminalLifecycle`。現有candidateelection已發生後，不得退回supporting或再跑LLM。[S01][S02]

```go
type TerminalIntent struct {
    EntryPoint string // audited enum below
    Cause      string // success_requested | no_progress | budget | cancelled | ...
    WantsSuccess bool
    CanContinueSupporting bool // determined by runtime, never LLM payload
}
type TerminalPreparation struct {
    Action string // continue_work | commit_terminal | recover_only
    Candidate *RunResult
    PrimaryBinding *PrimaryBindingV1
    ReasonCodes []string
}
```

preparation 使用原 run cancellation/budget context；10 秒 detached `terminalFinalizationContext` **只能**用於清理、local persistence/reconciliation。不得在該 context 呼叫 judge、reference、coordinator、rollback script 或額外驗收命令。原本通用路径已有行为与此不同時，本規格只要求 decision-intent path改用明确的权限表；non-decision behavior不在本次偷改。

### 2.1.1 防止直接 caller 繞過

`FinalizeRun` 在 decision-intent 下 MUST 檢查符合 `runtime-contracts.schema.json#/$defs/TerminalPreparationProofV1` 的 runtime-only proof。宣稱 success 卻沒有 proof 的直接 caller不可在內部 detached context 補跑 primary；改回 non-success candidate，reason=`decision_terminal_preparation_missing`。`SetLastRunResult`、library compatibility seam、natural-return path 均不得先公開未確認的 success。

EntryPoint wire enum 固定：`finish_tool|coordinator_eof|direct_agent|fast_route|reused_work|resume_completion|partial_ack|worker_hard_stop|turn_limit|budget_stop|no_progress|acceptance_repair|provider_failure|watchdog|signal|emergency|panic|embedded|aggregate`。此 enum 只記呼叫來源，不給額外授權。

## 2.2 共同準備狀態機

| 狀態 | 可進入的下一狀態 | 允許的工作 |
|---|---|---|
| SUPPORTING | QUIESCING / STOPPING / RECOVERY_REQUIRED | team一般任務、既有 auxiliary decisions |
| QUIESCING | SNAPSHOTTING / SUPPORTING / STOPPING | 關閉新派工、等待已授權工作收斂；檢查 terminal resources；不得只看 primary 自己pending |
| SNAPSHOTTING | DECIDING / SUPPORTING / STOPPING / RECOVERY_REQUIRED | 收集與驗證 evidence；固定 roles、admission；無 LLM |
| DECIDING | PRIMARY_BOUND / SUPPORTING / STOPPING / RECOVERY_REQUIRED | 依 profile與已預約budget執行 DecisionEngine；rounds受限 |
| PRIMARY_BOUND | VALIDATING / SUPPORTING / STOPPING | 不重新判斷；支持資料改變必須先 invalidation |
| VALIDATING | READY_TO_COMMIT / SUPPORTING / STOPPING / RECOVERY_REQUIRED | process verification + team acceptance；可修復失敗只在 runtime仍可繼續時回SUPPORTING |
| STOPPING | READY_TO_COMMIT / RECOVERY_REQUIRED | 停模型、受限清理、持久化已知 partial evidence；不形成新決策 |
| READY_TO_COMMIT | CANDIDATE_ELECTED | 形成不可變 result與manifest；檢查終止原因後交既有terminal owner |
| CANDIDATE_ELECTED | COMMITTING / RECOVERY_REQUIRED | 原 `terminalLifecycleCandidateElected`；不能回SUPPORTING |
| COMMITTING | COMMITTED / RECOVERY_REQUIRED | 原writer的一次run_finished append |
| COMMITTED | COMMITTED | 只能投影／render；same-request re-entry返回同一結果 |
| RECOVERY_REQUIRED | REPLAY_ONLY | 不做新provider calls；verified replay後回到最後已確認狀態 |

準備鎖可短暫保護狀態選擇，**不得持 lifecycle mutex 等 LLM／I/O**。Concurrent finish、自然返回與 signal 共用同一 preparation owner。success-preparation 尚未 elect terminal candidate 時若收到 hard stop，立刻取消 in-flight provider context，所有未開始 roles 禁止 dispatch；再走 STOPPING。commit 已確認後到达 signal不能改寫既有終態。

## 2.3 Terminal entry-point matrix

下表是涵蓋契約；PR-0 必須以實際 call-site inventory 對應所有 `FinalizeRun`、`SetLastRunResult`、`run_finished`、direct、stream-return 與 emergency 呼叫，禁止只修 finishTool。

| Entry point | 基準掛接位置／類別 | 可新跑 primary？ | 缺 primary 時結果 |
|---|---|---:|---|
| `finish` tool success | `finishTool.Run` [S08] | 是，通過quiescence/budget前置條件 | 可修復則continue；否則blocked/7 |
| Coordinator自然結束／沒有finish的EOF | coordinator run loop | 是，僅視為success候選，不相信prose | 无有效契约則blocked/7 |
| Direct `@agent` 正常完成 | `finalizeDirectRun` [S09] | 是，direct輸出只能作reported evidence | 不可用direct shortcut跳過primary |
| fast/direct execution route | 同一run pipeline | 是，backend具備所需約束才行 | 不支援則preflight blocked，不換team |
| supporting全部來自reused results | completion path | 是，僅採本次admitted import/reuse provenance | 不因沒有新worker呼叫而跳過 |
| resume後primary已bound | resume/completion | 否，先驗證或續未完成stage | stale時invalidated；不得新IID掩蓋 |
| `acknowledge_failed_tasks=true` | finish tool | 否，不能以partial ack繞過required support | 保留partial/blocked，非成功 |
| unresolved-worker hard stop | `finalizeTerminalUnresolvedRun` 路徑 [S08] | 否 | non-success evidence-only |
| coordinator round/turn limit | run guard | 否，hard limit後沒有特權額度 | partial/7 |
| token／duration budget exhausted | budget guard | 否 | partial/7 |
| no-progress／anti-thrashing stop | no-progress path [S08] | 否 | partial/7；不再允許self-healing |
| acceptance failed、可bounded repair | acceptance handling [S08] | 不立即重跑；support實際改變才新generation | continue或partial/7 |
| provider錯誤／schema錯誤／watchdog | run error path | 否；已允許的same-stage retry在DECIDING內處理 | failed/1 或 stalled/124 |
| SIGINT／SIGTERM／context cancelled | cancellation | 否 | cancelled/130或143 |
| emergency finalize／第二次interrupt | `EmergencyFinalizeRun` [S02] | 否 | bounded commit／recovery-required |
| panic recovery | top-level deferred recovery | 否 | 只記診斷與已知失敗；無recovery handler不宣稱已持久化 |
| library/embedder呼叫 | 共用runtime API | 同上 | 不允許legacy direct setter偽造decision success |
| 多team聚合 | `AggregateRunResults` [S10] | 否 | 只合併已commit结果，不創造全域primary |

任意team ≠ 多個 team 各做一份 primary 再假裝同一個。此版單一決策run有一個 owner TeamSession；既有chain/multi-team模式不得以一個 `decide` 隱式展開。跨team supporting需既有顯式授權及 evidence-import契約；沒有這些契約則拒絕該組合，不縮限合法單team。

## 2.4 Quiescence、acceptance 與 manifest 順序

成功準備順序為：

1. 驗證既有workflow可結束；所有**required supporting work**已達可接受終態；reconcile/leak檢查涵蓋歷史invocation資源。只有primary目的的runtime occurrence從這一輪pending檢查排除。
2. Freeze supporting-set revision；攔截新task admission、未完成result更新；late事件使當前snapshot失效，不能悄悄混入。
3. Evidence collector可先回不足，不一定建立occurrence；材料齊備才commitprepared與admitted。
4. Existing DecisionEngine完成record，record持久化＋schema與hash驗證後才能bound。
5. 驗證mandatory process criterion，再run既有team acceptance。驗收的寫入性命令仍受原授權。Acceptance 前後各以相同 collector 重新建立 source index；若 `source_index_digest`、support cursor event/hash 或任一 selected source artifact hash 改變，當前 `support_revision_digest` 即失效，必須 append invalidated 並回 SUPPORTING。Acceptance receipt 本身只進 final manifest，不自動加入support evidence；只有 acceptance repair 透過既有 task/artifact/event API 發布的新資料才會進下一 generation。未出現在 canonical event／artifact index 的任意 workspace file mutation 不屬於 evidence，不可被 collector 或 process criterion採用。
6. 將primary record、binding event、base evidence、sealed packet、roleplan、process verification與team acceptance evidence放入**同一最終manifest**後seal。
7. CompletionGate以 AND 判斷，再交既有terminal commit。從record到manifest是單向參照；record不得引用包含自己的finalmanifest造成hash cycle。

process criterion不能以「record檔案存在」代替：至少檢查requirement/branch/logical/generation/admission/roleplan/evidence hash、有效opinions數、必跑stages、aggregation、選项是否合法、staleness、binding唯一性。這是程序驗證，不是「決策正確率」或forecast calibration。

若team acceptance未設定：在**decision intent**下，runtime-owned process criterion本身是合法且明確的新 mandatory acceptance contract，因此可以completed；DTO仍保留 `team=not_configured`，不能改寫為team通過。既有team acceptance失敗、required workflow未滿足或unresolved tasks存在時仍不可completed。

## 2.5 Stop優先序與錯誤預算

同一preparation window內按固定優先序保留原因：persistence不確定 → confirmed watch­dog stall → explicit cancellation → run failure/security violation → budget/no-progress/limits → unresolved work → acceptance/decision-not-ready → success候選。這沿用目前canonical evaluator的stalled/cancelled/failure/budget先後；不要在CLI另做不同排序。[S10]

已耗盡budget或收到cancel時，即使有完整evidence，也不得為了「湊出primary」開一個新call。decision階段預算必須在supporting開始時由既有BudgetManager標示可保留額度／在call前reserve；不需要為cleanup解除原模型budget。reservation不足就清楚blocked/partial。

同一support revision缺相同evidence，第二次finish必須本機回同一診斷，不重复REFERENCE/proposal。允許supporting繼續只在runtime具備remainingbudget、未hardstop、repair次數未耗盡時。無進展的finish迴圈納入原anti-thrashing計數。

---

# 3. Primary runtime occurrence 的 ID、scheduler exclusion、resume 與 manifest

## 3.1 Occurrence schema及authority

primary使用既有TaskKindOutcome，增加**runtime-owned origin/purpose**，不要新增TaskKindDecision：

```go
type RuntimeOccurrenceMetaV1 struct {
    SchemaVersion int    `json:"schema_version"` // 1
    Origin        string `json:"origin"`         // runtime
    Purpose       string `json:"purpose"`        // primary_decision
    ExecutionOwner string `json:"execution_owner"` // decision_engine
    LogicalRunID  string `json:"logical_run_id"`
    BranchID      string `json:"branch_id"`
    Generation    uint32 `json:"generation"`
    DecisionID    string `json:"decision_id"`
}
```

完整 strict wire contract 是 `runtime-contracts.schema.json#/$defs/RuntimeOccurrenceV1`。`occurrence_ref` 必須解析到該 schema；runtime constructor 之外沒有合法 producer。

這是durable occurrence wire DTO，不是model-facing TaskDef schema。TaskDef配置／模型JSON不得直接設上述資料；runtime constructor填值，TaskOccurrenceProjection、TodoItem、admission digest、eventreducer、restore adapter必須完整round-trip。未具對應 admitted event 的reservedID，無論出自JSON、YAML或checkpoint，都不是合法runtime occurrence。

`primary_task_id`固定同logical slot；每generation有明確occurrence revision與自己的decision ID。相同generation的provider retry不調整task identity。新generation使用既有可審計的occurrence reset/revision mechanism，必須由invalidation觸發；不以手改TaskDone→Pending繞過CanTransition。

`Agent`不得填一個虛構的已授權worker。若歷史presentation需要字串，可顯示`runtime:primary-decision`，但runner selection由protectedexecution_owner控制；該標籤永不進agent candidate registry。也不能假造`ExecutionTarget=runtime`作為provider。實際backend、model、provider全部記在role bindings。

## 3.2 Admission與派工

`primary_decision_admitted`携帶 occurrence_ref、DecisionAdmission_ref，兩者在一個event被task與primaryreducer共同接受，來源是同一journal commit。任何task-created cache同步只是projection，不能成為第二份存在證據。

既有admission validation需要識別`execution_owner=decision_engine`：驗證完整role plan，而不是強迫primary本身有一個普通worker model。normal tasks仍沿用原ExecutionTarget要求，不接受由此擴大可略過的範圍。

Engine substage可以有statuses/receipts，但不是新DAG task，不持有parent Todo lifecycle，不觸發per-taskdecisionprofile。`DecisionRequirement`只授權同一primary形成，不讓stages遞迴進decisionengine。

## 3.3 必須統一採用的predicate

```go
IsPrimaryOccurrence(x) bool // 需valid runtime meta + matching durable admission
IsSchedulableWorker(x) bool // !IsPrimaryOccurrence && existing checks
IsBlockingSupportingWork(x) bool // !IsPrimaryOccurrence && existing unresolved logic
```

不能只靠IDprefix實作以上predicate。所有路徑的排除／納入如下：

| 子系統 | Primary occurrence規則 |
|---|---|
| dag ready queue、normal executeTask、worker fanout | 一律排除；誤派工回`decision_occurrence_wrong_owner` |
| generic ResumeInterruptedTasks | 排除primary，交共同terminal preparation恢復；不得兩邊resume |
| dependency expansion／on_failure back-edge／repair autodelegation | 不可把primary當可重派worker；primary→supporting依賴禁止形成cycle |
| reused-task journal／result memoization | 不可按goal text命中primary；只依logical/generation的durablebinding恢復 |
| worker escalation/model override | 不改已固定primarybinding；只能影響未admitted的新support task |
| workflow phase完成計數 | primary不是要求進入某個phase的worker；原workflow gates照常 |
| finish pending/failed support checks | 排除primary自身，避免等自己完成的deadlock；其失敗由decisiongate處理 |
| task列表、inspect、TUI | 納入、標`origin=runtime purpose=primary_decision`與generation |
| run工作量統計 | existing user-task stats維持；新欄位單列runtime_occurrences、primary_generations；模型usage仍全數入帳 |
| context memory／LTM promotion | 維持既有outcome verification；record存在不自動標記有用或verified outcome |
| final evidence manifest | completed時primary proof為mandatory；non-success列missing/blocked reason |
| CompletionGate | 不能因primary從unresolvedsupport排除就自動成功；必須另外checkprimarycontract |

## 3.4 Manifest契約

決策run final manifest新增typed requirement `primary_decision_valid`。其verification receipt必須符合 `runtime-contracts.schema.json#/$defs/PrimaryManifestProofV1`，並綁定 `logical_run_id, branch_id, requirement_digest, primary_task_id, generation, decision_id, admission_ref, record_ref, base_evidence_ref, sealed_evidence_hash, role_plan_ref, binding_event_id, support_revision_digest, validation_version`。

completed的manifest MUST exactlyone active validprimaryproof，與run_finished相同。歴史invalidated generations只能放history references；auxiliary records不得算primaryproof。

non-success manifest可有primary缺席，明列`state=not_satisfied`與stable reason；不能省略requirement，不能把failed primary藏在support exclusion後面。manifest可包含此前已形成但不再有效的record，并標invalidated；不得讓renderer把它當本次主答案。

## 3.5 Resume與不變性

Restore順序：logicalreducer → primaryadmission/occurrence → existing task rehydration → primary exclusion → rights revalidation → decisionpreparation。對已bound且supportrevision未變，使用原record；對call中斷，按§1.7續缺失slots；對corruptrecord/admission，recovery-required，不走normalworkerrepair。

Policy/role/evidence生成中的immutable artifact不可由checkpoint欄位覆寫。含primaryprefix但沒有admission的legacycheckpoint必須拒絕為可執行工作；loader不得猜其意圖或改寫checkpoint。

---

# 4. PrimaryDecisionEvidence 完整 schema、轉換及 bounding algorithm

## 4.1 Schema檔案與分層

`primary-evidence.schema.json` 是完整strict JSON schema；本章是其跨欄位與執行語意。附件中的schema使用`additionalProperties:false`；unknownnestedfields、重複JSONkeys、NaN/Infinity、無效UTF-8均reject。JSONschema不能表達的referentialintegrity由本章補足。

```text
PrimaryDecisionEvidenceV1
  schema_version=1, kind=primary_decision_evidence
  logical_run_id, branch_id, generation, requirement_digest
  support_cursor {event_id,event_hash,lineage_digest}
  support_revision_digest, source_index_digest
  collector_version=primary-collector@v1
  normalizer_version=hufu-json-c14n@v1
  redaction_policy_ref, limits_ref
  scope {execution_run_ids[],imported_evidence_refs[],excludes_primary_lineage=true}
  items[]                         // full EvidenceItemV1 below
  coverage {inventory_ref,inventory_count,selected_count,excluded_count,
            excluded_reason_counts,mandatory_selected_count,
            known_independent_groups,unresolved_provenance_count}
  budget {used_view_bytes,used_tokens,reserved_framing_tokens,count_method,
          tokenizer_id,tokenizer_revision,max_context_input_tokens}
  base_rate_item_ids[], assumption_item_ids[]
```

Reference一律含 `{id, sha256, media_type, size_bytes}`，來源path只在store内部解析。源artifactdigest與可提供給模型的redactedviewdigest分開。

## 4.2 EvidenceItemV1完整欄位

| 欄位 | 契約 |
|---|---|
| `item_id` | `ei_` + H(source event、task occurrence、pointer、source digest、collector version)；不可由coordinator命名覆蓋。 |
| `source.source_kind` | `assertion|receipt|fact|artifact_text|artifact_metadata|base_rate|assumption|advisory_summary` |
| `source.task` | task_id、occurrence_revision、attempt、producer_execution_run_id、result_event_id；operator input可null |
| `source.source_artifact` | 已resolve的immutable來源ref；純fact/result須先引用其durable result artifact，不引用任意prose |
| `source.json_pointer` | JSON值來源用pointer；text用空字串，由extraction紀錄line ranges |
| `source.source_event_id` | 宣告此資料存在的canonicalevent |
| `producer_agent_id/model_identity/provider_identity` | 有則填實際canonicalidentity；未知為null，不能靠LLM自報 |
| `derived_parent_item_ids[]` | 只收runtime可證明的來源link；modeldeclared lineage留advisory，不參與independence證明 |
| `provenance_authority` | `runtime_observed|operator_declared|model_reported` |
| `view_ref` | 儲存清理後rendering的ref；與content的bytes一致 |
| `content_format/content` | `text` 或 `json`；JSON內容以字串保存exactsafeJSON，避免binaryfloat重寫值；不得再任意摘要 |
| `verification.state` | `passed|failed|not_checked|stale|inconclusive` |
| `verification.scope` | `specific_assertion|integrity_only|none`；hash符合只是integrity，不等於論述正確 |
| `verification.assertion_refs[]` | 真正驗證此claim的既有receipt；不同claim不可借用同task成功標籤 |
| `mandatory_reasons[]` | §4.5明列的enum；空陣列代表optional，不代表不重要 |
| `epistemic_status` | `observed|reported|advisory`；不把verified視為全文件的互斥階級 |
| `independence` | group_id、known_root、counts_toward_minimum；不同URL/agent不是獨立性的充分條件 |
| `extraction` | algorithm、truncated、selected_line_ranges、source_bytes、view_bytes、removed_line_count |

`coverage.inventory_count = selected_count + excluded_count`；去重前所有候選都在immutableinventory列出，包括排除原因，不能只回一個數字。inventoryentry=`{item_id,source_ref,source_event_id,mandatory_reasons,selection,reason}`；selection為selected/excluded。mandatory candidate不得以budget或optional_oversize排除。

## 4.3 Scope與原始資料到item的轉換

Collector只讀已verifiedactivebranch、已attachphysicalinvocations中**屬此logicalrequest的admittedtasks**，以及requirement中顯式授權的imports。歷史STM/LTM若被引用，先有本次importref与原始provenance，不能用「同teammemory」直接混入。

| 現有資料 | 轉換 | 不能做的推論 |
|---|---|---|
| TaskResult typed facts | 每pointer形成factitem，值保留型別與單位 | submit_result即真／所有facts皆verified |
| Assertion-bearing verification | 每具體assertion形成item，綁operand/source與receipt；failed同樣保留 | exit0就證明整個報告／domain結論 |
| observed command/receipt | receiptitem，保留whatwasmeasured与scope | tool執行成功就是方案正確 |
| Published text artifact | resolvehash後生成安全view；reported或advisory依來源 | 檔案存在等於內容可信 |
| Binary/unsupported media | metadata-onlyitem，不能滿足內容驗證／base-rate需求 | 用檔名推斷內容，靜默OCR |
| BaseRateEvidence | 僅從已typedsubmitted欄位／artifactpointer載入；ref可回溯到source | 由LLM編造sample-size或分布值 |
| Assumption check | 只採既有允许sources的status事件；保存contradicted/stale | 從prose推測supported |
| Failed／cancelled task已有assertion | 有receipt就保留相應negativeevidence；task未成功不抹去資料 | task失敗因此所有結果都不可見 |
| Coordinator summary／terminal screen | 沒有durableclaim/receipt則不入canonicalevidence | finalanswer當作証據重餵judges |
| Prior primary record／其judges/revision | 一律不入下一primary的primary-sourcefacts；僅可作lineagehistory | 自己以前的答案變成獨立證據 |
| Auxiliary decision record | advisoryitem，父source lineage仍要追到root | 多一份decision多一組independent source |

同一fact被新attempt取代時，最新**已驗證適用的**值是current，旧值留下superseded/invalidation追踪；不是按wall-clock盲選最新。較晚的失敗驗證不得被較早success覆蓋。明確的supersession／reconcile事件才可解除舊failedassertion對currentstate的效力；歷史反證仍在inventory與limitations可查。

BaseRateEvidence payload精確包含 `reference_class:string, metric:string, sample_size:positive_integer, distribution:{mean,median,p10,p90}, source:ArtifactRef, limitations:string[]`。數值在evidence JSON字串內保留，adapter檢查finite、sample_size>0、p10<=median<=p90；不得將unknown補0。此schema來自既有DecisionTypes但本版補上轉換驗證。[S06]

## 4.4 Provenance與independence分組

先用runtime-known父子關係、相同contenthash與同一measurement/dataset identity建立union-find。未知origin全部歸入`ig_unknown`，`known_root=false`且不计入hardminimum。只有runtime-observed來源關聯或顯式可信operator declaration能建立已知root；modelreported關聯不能證明獨立。

同source衍生報告、同dataset不同analysis、多次同模型輸出不能增加knownindependentgroupcount。不同domain、URL、filename、taskID亦不能直接增加。若minimum無法由可信provenance證明，block，不得把「沒有發現相同」等同「已證明不同」。

每個groupID為按sortedrootidentity計算的H，與排序/mapiteration無關。circular來源引用→reject。此規則保障可追溯的來源分組，不宣稱統計獨立。

## 4.5 Mandatory集合

下列項目在封包中不可因較大而捨棄：

- safe原始request及typedinputs/constraints，reason=`request_input`。
- 已配置requiredcriterion直接綁定的assertions，reason=`required_criterion`。
- 本scope內所有尚未被合法supersession解除的failedassertions，reason=`failed_assertion`。
- criticalassumption之檢查／反證及materialinvalidation事件，reason=`critical_assumption|invalidation`。
- profile要求的base-rate evidence最小集合，reason=`outside_view_minimum`。
- 達到knownindependencefloor所需的每組最小有效代表，reason=`independence_minimum`。

Representative依§4.6固定排序挑選，不選「最支持方案」的項目。無法得到最小集合，或mandatory本身超過byte/token/item上限 → `decision_evidence_insufficient`／`decision_evidence_mandatory_overflow`，回傳缺口但不降低profile。

## 4.6 Deterministic bounding algorithm（不可用LLM判斷相關度）

在同一verifiedsupportcursor、policybundle、rolebindingplan与redaction snapshot下，輸出必須byte-identical：

```text
1. 由logicalmembership與顯式imports列出所有eligible source refs。
2. 限制掃描量：最多4096 candidates、32 MiB aggregate source bytes。
   超過即scan_limit_exceeded；禁止只看前N筆便聲稱完整。
3. 對來源做scope、digest、receipt、pointer、typed-value、redaction檢查。
   生成immutable source inventory；無法完整驗證的required來源直接block。
4. 依source關係消重／分組；補mandatory集合M。
5. 每項生成deterministic view：結構化資料完整保留；可選text才可head/tail。
6. 排序key：mandatory先，然後failed assertion、passed specific assertion、
   observed receipt、typed base rate、reported fact、artifact text、advisory summary、metadata；
   最後按(source_event_order, task_id bytes, attempt, json_pointer bytes, item_id bytes)升序。
   同priority不按option立場、model文字相關度或confidence選取。
7. 先裝入M；任何mandatory不fit就block。
8. 依排序掃optional：只有整項加入仍滿足item數、view bytes、完整stage prompt token限制時加入；
   不fit記錄reason，繼續下一項，不做first-overflow即停止。
9. 核對最終known groups/base rate floors；不得只驗裁切前集合。
10. 保存inventory/ref、selected items、omission統計、所有view refs，計算base evidence hash。
11. 建立每個plannedbinding的完整prompt做context admission；任何一個不fit即重做共同選取或block。
    所有JUDGE看到相同packet；不能只對較小context模型另裁切。
```

Text extraction `head-tail-lines@v1`：將CRLF正規化LF，按行切，保留UTF-8；只對optional text採用。若完整view超過單項byte上限B，最大片段分配為head=floor(3B/4)、tail=B-head；以整行加到各自quota，不重疊；中間加入固定ASCII`[OMITTED_LINES:N]`並計入B，超出時從較長段尾端逐行刪至fit。超過16KiB的单行不切字，optional改metadata-only；mandatory則block。记录1-based閉區間與removedlinecount。extraction metadata及marker都算viewbytes，不可偷偷刪除JSON欄位或把數值prefix當完整值。

所有token判斷基於**完整序列化prompt**（包含roleinstructions、request、options上限、schema与outputreserve），不僅content字數。優先使用固定version/tokenizer；未知tokenizer时採UTF-8bytes作保守budgetunits（`count_method=utf8_byte_upper_budget`），這是預算估計，不宣稱所有tokenizer的數學上界。最後仍須backend已知contextwindow與既有ContextCompiler通過；未知capacity→block。新snapshot保存計數方法與version，resume不換算法。

若REFERENCE/proposal在engine中新增已驗證的stageoutput，必須在**第一個JUDGE前**形成最終sealedDecisionEvidencePacket并對全體binding再次做同一context admission。BaseEvidenceHash與SealedEvidenceHash是不同的值，兩者都保存。JUDGE後不可補新source、更換options或改packet；必須下一generation。必要stageoutput讓封包超限只能block，不允許個別judge看不同截斷版本。

## 4.7 給既有DecisionEngine的adapter

`PrimaryDecisionEvidence`不取代DecisionEvidencePacket。adapter把：

- `fact`按`item_id + sourcepointer`形成namespace，避免同名key覆寫；values只取safecontent。不帶verified標籤的fact仍是reported。
- view refs加入Artifacts；只有validatedtypeditems可以加入BaseRates與Assumptions。
- trust來源轉成runtime-ownedProvenance；不能從taskpayload設定trustedProvenance。[S05]
- safeoriginalrequest＋process/domaincriteria形成既有RequestContract；options由trustedrequest或既有proposal/injection流程提供。

所有資料來源與limitedcoverage寫入packet的availablemetadata/context，而非塞入coordinator自由prose。adapter MUST 使用既有store evidence resolution，不手寫SHA。schema/units/pointer不匹配就error，不猜值。

---

# 5. V2 profile 完整 literals、role binding／fallback schema

## 5.1 Catalog與相容性

新增`builtin/light@v2`、`builtin/standard@v2`、`builtin/high-stakes@v2`；V1所有bytes、行為與解析priority不改。V2ref是**profile bundle version**，不是RunEvent/DecisionRecord schema version。

每個V2bundle完整包含 `policy`、`role-resolution`、`evidence`、`limits`。三個literals見本章後段與附件`light-v2.json`等；**不得使用extends、merge key、preset overlay或從V1動態補預設**。任何欄位缺失就catalogvalidation failure，未來改預設要新ref。

`policy`可投影到既有DecisionPolicy，但`judge-role/challenge-role/outside-view.role=null`表示由V2 RoleResolver提供runner，不代表無judge。不得把`role-resolution`硬塞進V1role欄位以繞過requiredcapabilitiesvalidator。Snapshot同時保留existingPolicyDigest與完整BundleDigest。

V2builtins的harddistinct-agent/model/providerfloors皆0；嚴謹度由judgment次數、隔離、反論、證據、revision與verification要求區分。這不宣稱high-stakes具有跨模型獨立性。Team/operator要求不同models/providers時是額外hardconstraint，必須驗證；不能因宣稱「任意team」而自動移除。

預設judgecontext沒有conversationhistory、已形成的coordinator答案或先前judgments；same-model多次隔離執行只能稱isolatedjudgments。這是對前版含混`independent`的明確語意補強。

## 5.2 RoleBindingPlanV1

完整schema在`role-binding-plan.schema.json`。所有bindings於第一個該roleprovider call前持久化；revision不是新routing，重用原judge的完整binding，包括model、backend、authorization與instructiondigest。

每個binding含：`binding_id,role,ordinal,execution_mode,agent_id,agent_definition_digest,execution_target_ref,model_identity,provider_identity,invocation_policy_digest,authorization_snapshot_ref,role_instruction_ref,context_policy,tool_policy,allowed_tool_ids,selection_reason,fallback_from,provenance=runtime_bound,revalidate_on_resume=true`。

`execution_mode=runtime_reviewer` → agent_id與agentdefinitiondigest為null；不建立偽worker來湊agent數。其credential、modelendpoint必須來自當前team/run已授權targets；fallback不新增權限、帳號或外部provider。`selection_reason`只有`capability_match|authorized_generalist|runtime_fallback`；fallback_from只有`no_authorized_candidate|no_capability_match`或null。

`tool_policy=none`須由 backend adapter 的 typed capability declaration 證明：`schema_version=1, tool_isolation=none_enforced, session_isolation=true, declaration_source=adapter`。僅刪除LLM tool schema不夠。內建 adapter 必須用 fake backend contract tests 證明 preflight 只接受上述值，且 dispatch request 不含tools；未實作宣告的外部codingbackend一律回 `decision_backend_capability_unverified`。本規格不要求 coding agent 連線真實provider或人工驗證provider行為。`--model codex/...`不構成能力證明。

### 5.2.1 額外硬條件的完整設定契約

`role-constraints.schema.json` 定義獨立 `DecisionRoleConstraintsV1`，欄位全部必填：

```text
schema_version=1, kind=decision_role_constraints
required_capabilities {proposal:[],reference:[],judge:[],challenge:[],premortem:[]}
preferred_capabilities {proposal:[],reference:[],judge:[],challenge:[],premortem:[]}
diversity {min_distinct_agents:0,min_distinct_models:0,min_distinct_providers:0}
fallback=inherit|forbid
candidate_limit: integer 1..64
```

此物件來自 trusted team/operator configuration，不能由 coordinator tool payload 提供。它不改 catalog literal：effective hard capabilities 取 union、各 diversity floor 取 max、fallback 只有繼承或禁止；不得下調 profile 要求。effective constraint digest 一起放 requirement/admission。Required/Preferred 空陣列表示沒有額外條件，**不是清除 bundle 值**。

Authoring 位置固定為 `decision.routing.constraints`。YAML 使用 `required-capabilities`、`preferred-capabilities`、`diversity.min-distinct-agents`／`min-distinct-models`／`min-distinct-providers`、`fallback`、`candidate-limit`；normalizer 明確映射到上列 snake_case wire keys。YAML 可省略欄位，wire DTO 不可省略：省略的 role capabilities 展開為空陣列，floor 為 0，fallback 為 `inherit`，candidate-limit 為 64，schema/kind 由 runtime 填入。未知 key、重複 key、null、負數及 team/operator 同層矛盾值一律 reject。設定的 operator 額外限制與 team 限制只可單調收緊，不能以 CLI convenience 覆蓋較強限制。

```yaml
decision:
  primary-profile: builtin/standard@v2
  routing:
    constraints:
      required-capabilities:
        judge: [security-review]
      diversity:
        min-distinct-providers: 2
      fallback: forbid
```

此例是額外硬條件，不是 ordinary team 使用 decision 的必要設定。`RoleBindingPlanV1.diversity` 只統計 JUDGE round 1 slots；proposal/reference/challenge 不增加這裡的 agent/model/provider 數。runtime reviewer 的 agent_id 為 null，因此算 unknown_agent_count，不虛構不同代理人。

本版 V2 不新增 pin 語法；unknown pin欄位 load-time reject。既有 V1 pinned binding 路徑照舊，不能 migrate 成 relaxed V2。§5.4的 pin failure規則適用相容路徑，不能用它推導V2已支援pin。

`context_policy` 是 binding 的隔離基準。JUDGE round1用sealed_packet；round2由固定 `revision_binding_rule`只加該judge自己的原opinion與全體一致的challenge摘要，沒有新binding，不加入其他judge的個別opinion／任意teamhistory。REFERENCE新增來源後必須在JUDGE前封存，同§4.6。

## 5.3 Deterministic resolution

1. 先確定team與run授權候選；deny、credentialdomain、network/data-egress、sandbox能力、currentACL先過濾。
2. 依requiredcapability硬條件篩選；只在配置明確為preferred時可降成一般候選。能力證據過期與selfdeclaredconfidence不可等同maintainertrusted。
3. 每role先依preferred-capabilitymatch分tier；tier内採現有CapabilityRegistry score。tie採canonicalagentID bytes，再canonicaltargetidentity，保證maporder不影響結果。
4. 使用有界 deterministic depth-first search（確定性深度優先搜尋），pool最多64個candidate、slots≤16。slot順序固定為proposal、reference、judge ordinals、challenge ordinals、premortem；每節點依本role的新agent/model/provider軟偏好排序，再用第3步rank與identity打破平手。只對 JUDGE round 1 套用本版全域 diversity floors。若剩餘候選不可能滿足hardfloors，剪枝；第一個完整可行assignment即結果，不宣稱全域最佳。
5. `decision-role-resolver@v2` 的搜尋上限固定100,000個candidate-assignment expansions（每嘗試指派一個candidate算一次，不以wall-clock計數）。耗盡上限仍無可行解回 `decision_role_search_budget_exhausted`，不可把搜尋未完成當成確定無解，也不可因此fallback。完整搜尋證明無解才回 `decision_profile_unsatisfied`。soft偏好不得增加授權或下調hardfloors。
6. 只在所有合格teamcandidate為空、fallback明確允許時，使用授權runtime reviewer。runtimefallback仍須滿足hardconstraints與完整backend capability。
7. 對每binding保存decision與permission/instruction snapshots；REFERENCE read-only，JUDGE/CHALLENGE/PREMORTEM/PROPOSAL tool-less。

REFERENCE可在sealed前使用已授權read-onlytools蒐集資料；runtimefallback沒有tools，只能整理既有supportevidence。它不得憑模型記憶造出artifact-backedbase-rate。`no specialist label`不等於無法決策，但「證據不足」仍可block。

## 5.4 Fallback failure matrix

| 情況 | 可選fallback？ | 規則 |
|---|---:|---|
| 沒有matchingpreferredcapability | 是 | 先authorizedgeneralist，再預先允許的runtime reviewer |
| explicitrequiredcapability沒有人符合 | 否 | hardcontractunsatisfied |
| 全部候選被authorization或egressdeny | 否 | 不得用sidecar繞過；`no_authorized_candidate`只指未宣告worker且已有明確授權runtime target，不包括明確deny |
| 角色固定pin失效 | 否 | 不改綁其他worker |
| model/provider unavailable after binding | 否 | same-bindingboundedretry或blocked |
| JSON opinion invalid | 否 | 原binding的boundedprotocolretry；不換「更願意答」的模型 |
| opinion不同意使用者／選no-go | 否 | 這不是故障，不能觸發fallback |
| timeout／unknown receipt | 否（不換binding） | §1.7 boundedtool-lessreplay規則 |
| resume時原permission撤銷 | 否 | 阻止繼續；舊snapshot不是繞過現行deny的授權 |
| modelcontext不夠 | 否（不私換模型） | 共同重新boundbeforeadmission或block；sealed後不可各自裁切 |

## 5.5 Model解析、成本與stagecount

Teamagentbinding沿用該agent已解析model／CLIworker override；runtime reviewer先用explicitjudge model、configuredjudge、configuredsidecar，再用明確worker/default target。每個候選均重新過現有backend/credentialauthorization；若caller明確指定judge不可用，不當「沒設定」跳到較弱target。一般`execute`的ResolveJudgeModel成本語意不改。

一個generation的最多logicalstages（不含每call retry）：light=proposal1+judge2；standard=proposal1+reference1+judge3+challenge1+revision3+premortem1；high=proposal1+reference1+judge5+challenge2+revision5+premortem1。已提供合法options可省proposal；forecast由既有opinion/finalrecord欄位承載，不額外增加forecastmodelcall。aggregate/finalization=aggregate為deterministic，不另問LLM選winner。具體stage順序沿用engine的有版本flow，不能只依文字架構圖重排。[S05]

每stage實際providercalls≤2；整體tokens、時間、generation再受limits限制。缺validjudge不得低於`min-independent-judgments`。不得為讓流程完成而動態減judges；未用額度只依既有可靠usage結算釋放。

### 5.6 三份完整V2 literals

下列使用可機械讀取JSON，與附件catalog相同；YAMLauthoring的preset只引用其exactref。`policy.discipline.commit`保留原taskside-effect語意，但primaryformation本身不執行被選方案；它的internalartifactwrites不等於業務變更權限。

#### builtin/light@v2

```json
{
  "schema_version": 2,
  "kind": "decision_profile_bundle",
  "ref": "builtin/light@v2",
  "policy": {
    "independent-judgments": 2,
    "min-independent-judgments": 2,
    "context-isolation": "strict",
    "score-scale": "0-10",
    "outside-view": {
      "required": false,
      "reference-evidence": false,
      "role": null
    },
    "criteria": [],
    "judge-role": null,
    "aggregation": {
      "method": "mean-score"
    },
    "challenge": {
      "enabled": false,
      "count": 0,
      "trigger": null
    },
    "challenge-role": null,
    "revision": {
      "enabled": false
    },
    "premortem": {
      "enabled": false,
      "required-before-commit": false
    },
    "forecast": {
      "required": false
    },
    "finalization": {
      "mode": "aggregate",
      "judge-id": ""
    },
    "option-proposal": {
      "enabled": true,
      "max-options": 5
    },
    "discipline": {
      "alternatives": {
        "require-no-action-option": true,
        "require-information-option": false,
        "min-options": 2
      },
      "stop": {
        "max-attempts": 2,
        "max-tool-calls": 0,
        "max-tokens": 0,
        "max-duration": "",
        "checkpoint-every": 1,
        "require-kill-criteria": false,
        "kill-criteria": []
      },
      "commit": {
        "required-for-side-effects": [],
        "require-rollback": false,
        "require-reconcile": false,
        "require-observability": false,
        "require-verification": false,
        "require-evidence": false
      },
      "replan": {
        "on-critical-assumption-contradicted": "continue",
        "on-material-evidence-changed": "continue",
        "on-repeated-failure": "continue"
      },
      "evidence": {
        "required-independent-groups": 1,
        "reject-circular-citation": true,
        "warn-shared-origin": true
      },
      "routing": {
        "capability-aware": false,
        "allow-pinned-binding": false
      }
    },
    "max-rounds": 1,
    "max-tokens": 80000,
    "budget-degradation": "forbidden"
  },
  "role-resolution": {
    "version": "decision-role-resolver@v2",
    "roles": {
      "proposal": {
        "count": 1,
        "preferred-capabilities": [],
        "required-capabilities": [],
        "candidate-order": [
          "capability-match",
          "authorized-generalist",
          "runtime-reviewer"
        ],
        "fallback": "if-no-match",
        "allow-repeated-definition": true,
        "tool-policy": "none"
      },
      "reference": {
        "count": 0,
        "preferred-capabilities": [
          "evidence-research"
        ],
        "required-capabilities": [],
        "candidate-order": [
          "capability-match",
          "authorized-generalist",
          "runtime-reviewer"
        ],
        "fallback": "if-no-match",
        "allow-repeated-definition": true,
        "tool-policy": "authorized-read-only"
      },
      "judge": {
        "count": 2,
        "preferred-capabilities": [
          "decision-analysis"
        ],
        "required-capabilities": [],
        "candidate-order": [
          "capability-match",
          "authorized-generalist",
          "runtime-reviewer"
        ],
        "fallback": "if-no-match",
        "allow-repeated-definition": true,
        "tool-policy": "none"
      },
      "challenge": {
        "count": 0,
        "preferred-capabilities": [
          "adversarial-analysis"
        ],
        "required-capabilities": [],
        "candidate-order": [
          "capability-match",
          "authorized-generalist",
          "runtime-reviewer"
        ],
        "fallback": "if-no-match",
        "allow-repeated-definition": true,
        "tool-policy": "none"
      },
      "premortem": {
        "count": 0,
        "preferred-capabilities": [
          "risk-analysis"
        ],
        "required-capabilities": [],
        "candidate-order": [
          "capability-match",
          "authorized-generalist",
          "runtime-reviewer"
        ],
        "fallback": "if-no-match",
        "allow-repeated-definition": true,
        "tool-policy": "none"
      }
    },
    "revision": "reuse-original-judge-binding",
    "finalization": "deterministic-aggregate",
    "diversity": {
      "scope": "judge-round-1",
      "min-distinct-agents": 0,
      "min-distinct-models": 0,
      "min-distinct-providers": 0,
      "prefer-distinct-agents": false,
      "prefer-distinct-models": true,
      "prefer-distinct-providers": true
    },
    "fallback-on-invocation-error": false,
    "allow-new-credentials": false,
    "allow-cross-team": false
  },
  "evidence": {
    "collector": "primary-collector@v1",
    "max-candidates": 4096,
    "max-scan-bytes": 33554432,
    "max-selected-items": 64,
    "max-item-view-bytes": 4096,
    "max-total-view-bytes": 16384,
    "max-evidence-input-tokens": 8192,
    "framing-reserve-tokens": 4096,
    "per-call-max-output-tokens": 4096,
    "unknown-context-window": "block",
    "text-extraction": "head-tail-lines@v1",
    "structured-truncation": "forbidden",
    "mandatory-overflow": "block",
    "max-json-depth": 16,
    "max-json-nodes": 8192,
    "max-line-bytes": 16384,
    "min-known-independent-groups": 1,
    "require-artifact-backed-base-rate": false,
    "include-failed-assertions": true,
    "unknown-provenance-counts-as-independent": false
  },
  "limits": {
    "total-decision-tokens": 80000,
    "active-decision-duration-ms": 300000,
    "max-generations": 2,
    "max-stage-invocation-attempts": 2,
    "unknown-usage-reservation": "retain-full",
    "transport-retry": "same-binding-only",
    "unknown-result-replay": "bounded-tool-less-only",
    "cleanup-timeout-ms": 10000
  }
}
```

#### builtin/standard@v2

```json
{
  "schema_version": 2,
  "kind": "decision_profile_bundle",
  "ref": "builtin/standard@v2",
  "policy": {
    "independent-judgments": 3,
    "min-independent-judgments": 3,
    "context-isolation": "strict",
    "score-scale": "0-10",
    "outside-view": {
      "required": true,
      "reference-evidence": true,
      "role": null
    },
    "criteria": [],
    "judge-role": null,
    "aggregation": {
      "method": "mean-score"
    },
    "challenge": {
      "enabled": true,
      "count": 1,
      "trigger": null
    },
    "challenge-role": null,
    "revision": {
      "enabled": true
    },
    "premortem": {
      "enabled": true,
      "required-before-commit": false
    },
    "forecast": {
      "required": true
    },
    "finalization": {
      "mode": "aggregate",
      "judge-id": ""
    },
    "option-proposal": {
      "enabled": true,
      "max-options": 5
    },
    "discipline": {
      "alternatives": {
        "require-no-action-option": true,
        "require-information-option": true,
        "min-options": 3
      },
      "stop": {
        "max-attempts": 3,
        "max-tool-calls": 0,
        "max-tokens": 0,
        "max-duration": "",
        "checkpoint-every": 1,
        "require-kill-criteria": true,
        "kill-criteria": [
          {
            "id": "critical-assumption",
            "kind": "assumption_invalid",
            "threshold": 0,
            "description": "A critical assumption is contradicted."
          }
        ]
      },
      "commit": {
        "required-for-side-effects": [],
        "require-rollback": false,
        "require-reconcile": false,
        "require-observability": false,
        "require-verification": true,
        "require-evidence": true
      },
      "replan": {
        "on-critical-assumption-contradicted": "replan",
        "on-material-evidence-changed": "replan",
        "on-repeated-failure": "replan"
      },
      "evidence": {
        "required-independent-groups": 2,
        "reject-circular-citation": true,
        "warn-shared-origin": true
      },
      "routing": {
        "capability-aware": false,
        "allow-pinned-binding": false
      }
    },
    "max-rounds": 2,
    "max-tokens": 240000,
    "budget-degradation": "forbidden"
  },
  "role-resolution": {
    "version": "decision-role-resolver@v2",
    "roles": {
      "proposal": {
        "count": 1,
        "preferred-capabilities": [],
        "required-capabilities": [],
        "candidate-order": [
          "capability-match",
          "authorized-generalist",
          "runtime-reviewer"
        ],
        "fallback": "if-no-match",
        "allow-repeated-definition": true,
        "tool-policy": "none"
      },
      "reference": {
        "count": 1,
        "preferred-capabilities": [
          "evidence-research"
        ],
        "required-capabilities": [],
        "candidate-order": [
          "capability-match",
          "authorized-generalist",
          "runtime-reviewer"
        ],
        "fallback": "if-no-match",
        "allow-repeated-definition": true,
        "tool-policy": "authorized-read-only"
      },
      "judge": {
        "count": 3,
        "preferred-capabilities": [
          "decision-analysis"
        ],
        "required-capabilities": [],
        "candidate-order": [
          "capability-match",
          "authorized-generalist",
          "runtime-reviewer"
        ],
        "fallback": "if-no-match",
        "allow-repeated-definition": true,
        "tool-policy": "none"
      },
      "challenge": {
        "count": 1,
        "preferred-capabilities": [
          "adversarial-analysis"
        ],
        "required-capabilities": [],
        "candidate-order": [
          "capability-match",
          "authorized-generalist",
          "runtime-reviewer"
        ],
        "fallback": "if-no-match",
        "allow-repeated-definition": true,
        "tool-policy": "none"
      },
      "premortem": {
        "count": 1,
        "preferred-capabilities": [
          "risk-analysis"
        ],
        "required-capabilities": [],
        "candidate-order": [
          "capability-match",
          "authorized-generalist",
          "runtime-reviewer"
        ],
        "fallback": "if-no-match",
        "allow-repeated-definition": true,
        "tool-policy": "none"
      }
    },
    "revision": "reuse-original-judge-binding",
    "finalization": "deterministic-aggregate",
    "diversity": {
      "scope": "judge-round-1",
      "min-distinct-agents": 0,
      "min-distinct-models": 0,
      "min-distinct-providers": 0,
      "prefer-distinct-agents": true,
      "prefer-distinct-models": true,
      "prefer-distinct-providers": true
    },
    "fallback-on-invocation-error": false,
    "allow-new-credentials": false,
    "allow-cross-team": false
  },
  "evidence": {
    "collector": "primary-collector@v1",
    "max-candidates": 4096,
    "max-scan-bytes": 33554432,
    "max-selected-items": 128,
    "max-item-view-bytes": 8192,
    "max-total-view-bytes": 32768,
    "max-evidence-input-tokens": 16384,
    "framing-reserve-tokens": 4096,
    "per-call-max-output-tokens": 4096,
    "unknown-context-window": "block",
    "text-extraction": "head-tail-lines@v1",
    "structured-truncation": "forbidden",
    "mandatory-overflow": "block",
    "max-json-depth": 16,
    "max-json-nodes": 8192,
    "max-line-bytes": 16384,
    "min-known-independent-groups": 2,
    "require-artifact-backed-base-rate": true,
    "include-failed-assertions": true,
    "unknown-provenance-counts-as-independent": false
  },
  "limits": {
    "total-decision-tokens": 240000,
    "active-decision-duration-ms": 600000,
    "max-generations": 3,
    "max-stage-invocation-attempts": 2,
    "unknown-usage-reservation": "retain-full",
    "transport-retry": "same-binding-only",
    "unknown-result-replay": "bounded-tool-less-only",
    "cleanup-timeout-ms": 10000
  }
}
```

#### builtin/high-stakes@v2

```json
{
  "schema_version": 2,
  "kind": "decision_profile_bundle",
  "ref": "builtin/high-stakes@v2",
  "policy": {
    "independent-judgments": 5,
    "min-independent-judgments": 5,
    "context-isolation": "sealed",
    "score-scale": "0-10",
    "outside-view": {
      "required": true,
      "reference-evidence": true,
      "role": null
    },
    "criteria": [],
    "judge-role": null,
    "aggregation": {
      "method": "mean-score"
    },
    "challenge": {
      "enabled": true,
      "count": 2,
      "trigger": null
    },
    "challenge-role": null,
    "revision": {
      "enabled": true
    },
    "premortem": {
      "enabled": true,
      "required-before-commit": true
    },
    "forecast": {
      "required": true
    },
    "finalization": {
      "mode": "aggregate",
      "judge-id": ""
    },
    "option-proposal": {
      "enabled": true,
      "max-options": 5
    },
    "discipline": {
      "alternatives": {
        "require-no-action-option": true,
        "require-information-option": true,
        "min-options": 3
      },
      "stop": {
        "max-attempts": 3,
        "max-tool-calls": 0,
        "max-tokens": 0,
        "max-duration": "",
        "checkpoint-every": 1,
        "require-kill-criteria": true,
        "kill-criteria": [
          {
            "id": "critical-assumption",
            "kind": "assumption_invalid",
            "threshold": 0,
            "description": "A critical assumption is contradicted."
          }
        ]
      },
      "commit": {
        "required-for-side-effects": [],
        "require-rollback": false,
        "require-reconcile": true,
        "require-observability": true,
        "require-verification": true,
        "require-evidence": true
      },
      "replan": {
        "on-critical-assumption-contradicted": "replan",
        "on-material-evidence-changed": "replan",
        "on-repeated-failure": "stop"
      },
      "evidence": {
        "required-independent-groups": 2,
        "reject-circular-citation": true,
        "warn-shared-origin": true
      },
      "routing": {
        "capability-aware": false,
        "allow-pinned-binding": false
      }
    },
    "max-rounds": 2,
    "max-tokens": 480000,
    "budget-degradation": "forbidden"
  },
  "role-resolution": {
    "version": "decision-role-resolver@v2",
    "roles": {
      "proposal": {
        "count": 1,
        "preferred-capabilities": [],
        "required-capabilities": [],
        "candidate-order": [
          "capability-match",
          "authorized-generalist",
          "runtime-reviewer"
        ],
        "fallback": "if-no-match",
        "allow-repeated-definition": true,
        "tool-policy": "none"
      },
      "reference": {
        "count": 1,
        "preferred-capabilities": [
          "evidence-research"
        ],
        "required-capabilities": [],
        "candidate-order": [
          "capability-match",
          "authorized-generalist",
          "runtime-reviewer"
        ],
        "fallback": "if-no-match",
        "allow-repeated-definition": true,
        "tool-policy": "authorized-read-only"
      },
      "judge": {
        "count": 5,
        "preferred-capabilities": [
          "decision-analysis"
        ],
        "required-capabilities": [],
        "candidate-order": [
          "capability-match",
          "authorized-generalist",
          "runtime-reviewer"
        ],
        "fallback": "if-no-match",
        "allow-repeated-definition": true,
        "tool-policy": "none"
      },
      "challenge": {
        "count": 2,
        "preferred-capabilities": [
          "adversarial-analysis"
        ],
        "required-capabilities": [],
        "candidate-order": [
          "capability-match",
          "authorized-generalist",
          "runtime-reviewer"
        ],
        "fallback": "if-no-match",
        "allow-repeated-definition": true,
        "tool-policy": "none"
      },
      "premortem": {
        "count": 1,
        "preferred-capabilities": [
          "risk-analysis"
        ],
        "required-capabilities": [],
        "candidate-order": [
          "capability-match",
          "authorized-generalist",
          "runtime-reviewer"
        ],
        "fallback": "if-no-match",
        "allow-repeated-definition": true,
        "tool-policy": "none"
      }
    },
    "revision": "reuse-original-judge-binding",
    "finalization": "deterministic-aggregate",
    "diversity": {
      "scope": "judge-round-1",
      "min-distinct-agents": 0,
      "min-distinct-models": 0,
      "min-distinct-providers": 0,
      "prefer-distinct-agents": true,
      "prefer-distinct-models": true,
      "prefer-distinct-providers": true
    },
    "fallback-on-invocation-error": false,
    "allow-new-credentials": false,
    "allow-cross-team": false
  },
  "evidence": {
    "collector": "primary-collector@v1",
    "max-candidates": 4096,
    "max-scan-bytes": 33554432,
    "max-selected-items": 256,
    "max-item-view-bytes": 8192,
    "max-total-view-bytes": 65536,
    "max-evidence-input-tokens": 24576,
    "framing-reserve-tokens": 4096,
    "per-call-max-output-tokens": 4096,
    "unknown-context-window": "block",
    "text-extraction": "head-tail-lines@v1",
    "structured-truncation": "forbidden",
    "mandatory-overflow": "block",
    "max-json-depth": 16,
    "max-json-nodes": 8192,
    "max-line-bytes": 16384,
    "min-known-independent-groups": 2,
    "require-artifact-backed-base-rate": true,
    "include-failed-assertions": true,
    "unknown-provenance-counts-as-independent": false
  },
  "limits": {
    "total-decision-tokens": 480000,
    "active-decision-duration-ms": 1200000,
    "max-generations": 3,
    "max-stage-invocation-attempts": 2,
    "unknown-usage-reservation": "retain-full",
    "transport-retry": "same-binding-only",
    "unknown-result-replay": "bounded-tool-less-only",
    "cleanup-timeout-ms": 10000
  }
}
```


---

# 6. CLI flag matrix、versioned output DTO、redaction 與 exit-code matrix

## 6.1 統一語意與defaults

```bash
# 任意既有team，只改任務意圖
hufu decide --team hufu-dev --primary-decision-profile builtin/standard@v2 "是否採用方案B？"

# 完全等價的canonical path
hufu run --intent decision --team hufu-dev --primary-decision-profile builtin/standard@v2 "是否採用方案B？"

# 新增命令對light/standard/high的易用別名
hufu decide --team research --rigor high "是否採用方案B？"

# 精確恢復，不重傳問題或變更policy
hufu decide --workspace ./workspace/hufu-dev --resume-decision ldr_0123456789abcdef0123456789abcdef
```

`decide`只把缺省intent设为decision，並共享command-localoptions→RunRequest resolver；不修改全域opts造成不同command互相污染。相同的flag在`run`与`decide`語意必須一致。

Primaryprofile precedence：explicit `--primary-decision-profile`／`--rigor` → team `decision.primary-profile` → `builtin/standard@v2`。exactref不經alias；team-local名字先於相同名字的ergonomicalias，`--rigor`直接映射固定v2ref不被localname截走。`off`在decisionintent不合法。

既有`--decision-profile`始終是**supporting/auxiliary per-task override**，包括在decidecommand；primary不讀它。未提供時保留team原本taskdefault，不能為避免成本而默默把既有guardprofile改off。`--decision-profile off`可由operator顯式停用auxiliarydecision，不會停掉primary。

## 6.2 Flag matrix（MUST逐列測試）

| Flag/組合 | `run` intent=execute | `run` intent=decision / `decide` | conflict / resume規則 |
|---|---|---|---|
| `--intent` | 缺省execute | decide固定decision；顯式execute拒絕 | resume不能改intent |
| `--team X`／`--agent-team X` | 既有teamresolver | 同一resolver、同一X | aliases值不同error；與default/auto-team互斥 |
| `--default` | 既有defaultteam | 同一defaultteam，不注入新workers | 顯式team互斥 |
| 無teamselector | 既有discovery/interaction | 原teamdiscovery；不能按intent選strategicteam | unattended/JSON無唯一預設時error，不彈互動 |
| `--auto-team`（sharedfacade新增支援） | 使用既有auto-team selector | 同domainselector；intent不加決策team偏置 | 與explicitteam/default互斥；dry-run只能本機已知deterministicselection |
| `--route auto|fast|team` | 保留execution-route語意 | 同義；fastbackend也須通過primarygate | 它不是teamselector；不支援decisionconstraints就blocked |
| `--primary-decision-profile P` | error | 指定primaryprofile | 與rigor互斥；resume禁止 |
| `--rigor light|standard|high` | error | 固定v2presetmacro | 不支援第四個alias；resume禁止 |
| `--decision-profile P` | 既有per-taskoverride | 仍是per-taskoverride，primary不受影響 | 與primaryprofile可共存但explain必須列兩個scope；resume禁止改 |
| `-m/--model` | 既有worker-onlyoverride | 同義，僅在reviewer未指定時可作fallback候選 | 不覆寫explicitrolemodel；resume禁止改 |
| coordinator/sidecar/judge/guard/plan-reviewer model flags | 既有role語意 | 保留；與realbackendcapabilities一起驗證 | 別因decide而把所有role強設-m；resume禁止改 |
| generation flags（`--temperature / --top-p / --top-k / --reasoning-effort / --max-tokens / --context-window`） | 原CLI最高優先序 | 相同語意；在admission前固定；不得突破profile每calloutput/context上限 | resume禁止改；未知/不支援值明確error |
| `--skill / --auto-skills / --plan / --report` | 原工作流程 | supporting可使用；reviewer只載入已snapshot且不破壞隔離的role context | dry-run不得因auto-skills呼叫LLM；resume不得改semanticflags |
| `--workspace` / `--workspace-root` | 既有exact/root語意 | 同義，teamjoin只一次 | 互斥；resume用exact或可唯一resolve的原workspace |
| `--resume-decision L`（新增） | error | 無positionalquestion；從journal恢復L | 不能與new/temp/newprompt/profile/model/input變更共用 |
| `--new`／fresh-sessionexecutionprofile | 原有行為 | 新logicalrequest，仍不能繞過unresolvedsideeffects | 與resume互斥 |
| `--max-total-tokens / --max-duration` | 原有budget | runbudget與decisionlimits取嚴格可用值，不改catalog | resume不重設用量；變更limits拒絕，另開request |
| `--no-journal` | 原有語意 | preflight拒絕（即使目前只關taskjournal也不接受此組合） | 未啟動provider前error |
| `--no-net / --force-mcp` | 原安全規則 | 全support/reviewbackend都適用 | resume只能額外收緊，導致舊binding不符則blocked |
| `--allow-path / --helper-tools` | 原授權規則 | 不擴大decisionsrole工具；helper-tools僅defaultteam | resume禁止擴權 |
| `--profile / --execution-profile` | 原bundle/securityprofile | 不與decisionprofile混為一談 | 展開後再檢查衝突；resume使用已固定值 |
| `--goal-mode exploratory` | 原有行為 | 不解除primaryprocesscriterion | 不能用exploratory把missingprimary變success |
| `--input / --input-file / --var / --var-file` | 原語意 | typedinput可進requirement；var仍非typedsecuritycontract | resume禁止用它們改frozenrequest |
| `--output text|json` | 原output | 都從同一committedDTO投影 | resume可改presentation，不改businessstate |
| `--event-format jsonl` + outputtext | 原eventstream | stdout只印typedJSONLevents含一個finalresult | stderr diagnostics；不混最終純文字 |
| `--event-format jsonl` + outputjson | 原command若有既定規則則相容 | stdout全JSONL，終筆為result envelope；不另印裸JSON | help須明示framing |
| `--quiet / --no-summary / --no-spinner` | 原顯示規則 | 不移除finalrecord與missingrequirement狀態 | 錯誤仍nonzero |
| `--theme / --display-preset / --no-color` | 原顯示規則 | 同一renderer動態theme，支援mono/epaper | JSON/JSONL永無ANSI |
| `--dry-run` | 原行為 | 只本機preview DTO，providercall=0，無logicalopened或workspace mutation | auto-team需LLM时error，禁止假裝已選；exit0只代表preview成功 |
| `--tui / --steps / --unattended` | 原相容規則 | TUI輸出同primaryprojection；unattended不發新confirmation | JSON stdout与TUI互斥；不能靠TUI替terminalwriter |
| 多`@team`segment / chain | 既有execute功能 | 此版拒絕單一decide多owner擴展，須显式分開run | 不代表限制單一任意team |

Unknownflags、conflictingaliases、profile未知、schema不合法是usage/configerror2；backend權限／能力實際不足是blocked7，避免將安全denial當成可忽略syntax。

## 6.3 Versioned output DTO

完整strictschema：`decision-output.schema.json`。本文規範renderer不序列化整個internalDecisionRecord；使用以下固定wrapper與有限recordview：

```text
DecisionOutputV1
  schema_version=1
  kind=decision_run_result | decision_run_preview
  logical_run_id?, execution_run_id?, branch_id?, team? {id,definition_digest}
  intent=decision
  terminal_persisted: bool
  logical_disposition=not_created|active|suspended|closed
  outcome=preview|completed|unverified|partial|blocked|failed|cancelled|stalled|recovery_required
  exit_code, goal_satisfied
  profile? {requested,resolved_ref,origin,version,policy_digest,bundle_digest}
  primary {state,task_id?,generation,decision_id?,binding_event_id?,record_ref?,
           base_evidence_ref?,sealed_evidence_hash?,view?}
  acceptance {decision_process,team,combined}
  coverage?, budget {used_tokens,reserved_tokens,used_duration_ms}
  reasons[], warnings[]          // {code,message,retryable,field_path?}
  redaction {applied,policy_version,record_view_truncated}
  continuation? {resume_logical_run_id,requires_operator}
```

所有question mark欄位都**顯式null**，array缺值用`[]`，不是省略。null表示無該資料，不是zero。`primary.state`只可missing/in_progress/blocked/bound/invalidated；只有bound可有canonicalview，record_ref不是mutablepath。

recordview完整欄位：`record_schema_version,selected_option_id,options[],forecast_ppm,forecast_is_calibrated=false,rationale[],dissent[],critical_assumptions[],missing_information[],premortem_findings[],falsification_conditions[],limitations[]`。三個commentarrayitem=`{subject_id,text,evidence_item_ids[]}`。Options含id/kind/title/origin，selected_option_id必須指其中一個。

Forecast由原record的有效0..1數值轉成millionths，使用decimalround-half-even；unknown為null，不補0。它是模型預測而非驗證結果，也不混用confidence。typedrecord本身若不存在forecast或必要資訊，依profilevalidate而不是renderer編造。

`outcome=completed`的cross-field invariant：terminal_persisted=true、logical_disposition=closed、goal_satisfied=true、exit_code=0、primary.state=bound、有recordref與bindingevent、acceptance.combined=passed、decision_process=passed。任何一項不符合必須拒絕輸出success。

preflighterror沒有logicalrun：相關IDs/team/profile可null；terminal_persisted=false。recovery-required結果是**暫態診斷**，不標記成已持久化RunResult；它不能覆寫journal的businessrecord。

JSONL framing：每行`{schema_version:1,type:status|result,sequence:int,data:...}`；sequence為本次processpresentation順序，不可作durableevent順序。type=result的data為本DTO且最多一筆。內部streamtext/toolsrawoutputs不直接列為公開status。stderr也必須遮罩。

## 6.4 Redaction契約與hash順序

現有redactor是process-localboundary，對secret-lookingkeys有明確numericallowlist；新`used_tokens`等若不補typedallowlist可能被改成字串，破壞replay。[S11]

固定pipeline：source resolve/integritycheck → typeddecode → sourcepolicy/secretrefs → redact**內容欄位** → validateDTO → canonicalize → computeviewdigest → persistview → appendmetadataref。原sourceartifact不就地改；hashes永遠對應實際storedbytes。舊canonicalrecord的完整hash與可分享recordview的hash不同時要明示，不可宣稱redactedview就是原recordbytes。

| 資料 | 持久化／輸出規則 |
|---|---|
| 原始request | 先轉安全refs或`[REDACTED]`；同一safeinput逐字固定；不得為原文承諾保存password |
| credential值／Bearer／privatekey／URLuserinfo/querysecret | 不進events、evidenceviews、DTO、stderr/TUI；path與hostname按既有sensitivitypolicy |
| executiontarget | 只記credential-freeidentity和受控credentialreference，不記secretvalue |
| typedmetadata ints/bools/enums | 依schema-aware精確path保留型別；不可globally放行含token的key |
| evidencecontent JSON | 內層JSON亦需typedredaction；要解析的數值被遮罩後變不合法則unknown/block，不能轉0 |
| mandatory數據或criterion被遮罩到不可用 | `decision_sensitive_input_unusable`；要求secretreference或安全summary，不以降低evidencegate應付 |
| debug／verbose／JSONL | 仍同樣遮罩；不得設`--raw` bypass |
| 新learnedsecret命中已frozenpacket | 不重算原hash假裝沒變；建立新safeview並invalidategeneration，或block；record仍保持歷史完整性 |
| cryptographichash／opaqueID | schemaformat驗證；redactor若誤改導致格式變化即error，不把被污染的hash當有效ref |

新eventpayload與outputDTO在經既有RedactJSON後必須仍過同schema，且數值計量維持type/value。最低測試keys涵蓋used_tokens、tokens_used、reserved_tokens、max_tokens、total-decision-tokens、max-evidence-input-tokens、per-call-max-output-tokens、used_duration_ms；array/object下內容仍逐欄遮罩。不要為某個key把整個object免檢。

## 6.5 Exit-code matrix

保留目前run evaluator／main的既有canonicalcodes：completed0、ordinaryfailure1、incomplete/blocked7、stalled124、cancel130；SIGTERM可用143。[S10][S12] 新schema/usageerror以typedProcessExitCode=2接入，不改其他commands原有契約。

| 狀況 | DTO outcome | code | terminal_persisted／允許新模型 |
|---|---|---:|---|
| 有效primary+combinedacceptance+confirmedrun_finished | completed | 0 | true／否 |
| 合法no-go/defer/request_information選項且上述成立 | completed | 0 | true；不表示已實作選中方案 |
| successfullocaldry-run | preview | 0 | false，logicalID=null，provider=0 |
| flagconflict／unknownenum／invalidschema／unknownprofile | failed | 2 | false；provider=0 |
| explicitteam不存在／resumeID不在branch | failed | 2 | false；不可偷選default |
| evidence不足、harddiversity／backendisolation不足、authorizationdeny | blocked | 7 | 已建立logical則持久化non-success；不silentfallback |
| requiredsupportfailed、acceptancefailed、budget/no-progress耗盡 | partial或blocked按existingevaluator | 7 | 持久化已知partial；0新decisioncalls |
| 未配置teamacceptance、但primaryprocesscheck過 | completed（僅decisionintent） | 0 | team仍not_configured；combinedpassed |
| generalexecute無acceptance | 維持unverified | 7 | 本spec不改 |
| providerfatal／artifactwrite明確failure | failed | 1 | 能安全commit則true，否則recoveryrequired |
| journalappend/sync結果未知、hashchain破損、conflictingidempotency | recovery_required（ephemeral） | 7 | false；只允許reconcile |
| watchdogstall | stalled | 124 | 檢查commit結果；不再呼叫模型 |
| SIGINT | cancelled | 130 | cleanupattempt；不保證SIGKILL情况下有event |
| SIGTERM | cancelled | 143 | 同上 |
| stdoutpipe/write失敗 | 不重寫businessoutcome | 1（processdeliveryerror） | journal若成功仍保持completed；不可重跑模型補輸出 |

當cancel/stall與persistuncertain同時出現，DTO保留recovery_required+原因列表；processcode優先保留confirmedwatchdog124／observedsignal130或143；非signaluncertain=7。這是輸送/耐久性狀態，不用它覆寫既有businesscandidate。只有commit確認後才可發出successstatus。

---

# 7. 實作門檻、測試與PR切分

## 7.1 六份契約各自的最低驗收

| 契約 | 必須先有的測試 |
|---|---|
| 身分/事件 | identicalprompt兩個logicalID；resume跨physicalIDs同logical；branch隔離；duplicatekey相同payloadno-op／不同payloadfail；§1.8每個crashwindow |
| terminalstate machine | 所有entry-point table routes；自然EOF/direct/cachehit不能漏primary；signal/budget/providerfatal不開LLM；concurrentfinish只有一個owner；existingrun_finished保持singlewriter |
| occurrence | reservedIDforgery；scheduler&genericresume排除；normalTaskKindOutcome不被誤排；primarypending不deadlock；manifestmandatoryproof；historygeneration非active |
| evidence | failedassertion強制保留；unknownprovenance不湊groups；largeJSON不截斷；mandatoryoverflowblock；排序/fingerprint獨立於maporder；所有judges同sealedhash |
| V2/roles | 三份完整literalvalidator；V1digest不變；無capabilitylabels時authorizedfallback；deny/requiredcapability/pinfailure不fallback；revisionreuse；以fake backend驗證context/tool isolation request與fail-closed preflight |
| CLI/DTO | flag表逐列；同名flag同義；dry-run0calls；DTOpositive/negative；numericredactionroundtrip；each exit；text/json/jsonl/TUI從同一committedprojection |

## 7.2 合併前跨契約場景

**T01** 任意team先跑3個supportingtasks，只有primary進DecisionEngine；exactlyonebinding，teamidentity不變。

**T02** defaultteam同模型產生3個isolatedslots；distinctmodel=1、runtimefallbackagent_id=null；不得聲稱3個agents。

**T03** 有已配置per-taskstandardguard的team，用decide後guard仍存在；primary不替supporting覆寫profile。

**T04** 五個successassertions外加一個failedmandatoryassertion，tightbudget時不得把faileditem先丟掉；fit則保留，不fit則blocked。

**T05** primaryrecord已bound後acceptancerepair改資料，旧bindinginvalidated，新generation新decisionID；最後只activeone。

**T06** record+manifest寫完、run_finishedsyncack遺失，重啟只reconcile+render；judgescalls不增加。

**T07** judge-2完成後crash，重啟只補其他slots；若providerreceipt未知，按reservation/replaycontract記實際額外cost，不聲稱exactlyoncecall。

**T08** resume時teamfile改名／模型改變／權限撤銷：身份policy依snapshot，現行deny照樣block；不重新routing掩蓋。

**T09** 第二次SIGINT在reference途中：取消generation、0後續call、唯一terminalcommit，沒有背景LLM繼續跑。

**T10** 沒有teamacceptance但primaryproof完整可以decisioncompleted；同條件execute仍unverified。teamacceptancefailed不能因primary存在變completed。

**T11** 以userJSON/YAML或legacycheckpoint注入`origin=runtime`／reservedID，全部被拒；runtimegeneratedoccurrence可以eventreplay恢復。

**T12** `--route auto`不被當成自動選team；explicitX永远使用X；dry-runteamselection需provider時不call。

**T13** 遮罩後viewhash與sourcehash分離、所有numericmetadata可反序列化；secret不出現在event/DTO/stderr。

**T14** non-successmanifest仍列primaryrequirement未滿足；typedprocesscheck不把forecast當observedoutcome。

## 7.3 PR順序（有依賴，禁止先做CLI空殼就宣稱完成）

1. **PR-0：baseline＋call-site inventory。** 固定V1digests、terminal-entry mapping、EventStore/receipts對應；加入本規格已固定interface的編譯期shape與現況invariant tests，不改runtime行為且不得提交預期失敗測試。
2. **PR-1：logicalidentity、payloadschema、reducers、writerfencing、crashfixtures。** 沒有這層不能開始primaryproviderdispatch。
3. **PR-2：protectedprimaryoccurrence與scheduler/replay/manifestpredicate。** nondecisionregression保持。
4. **PR-3：evidenceschema、sourceadapter、redaction、deterministicbounding。** 完成同一snapshotbit-identical與negativeevidence測試。
5. **PR-4：immutableV2bundles、roleplan、fallback與budgetadmission。** context隔離必須可證明，不能靠prompt承諾。
6. **PR-5：commonterminalpreparation＋所有entrypoints。** 完整crash/signal/concurrentrace測試；既有commitowner不分叉。
7. **PR-6：CLIsharedresolver、DTO、exit/redaction/framing、inspect。** 核對每flagmatrix。
8. **PR-7：docs/config compatibility與bundledteamcleanup。** 移除modelplaceholders、解釋primary/auxscope；strategicdecision只是範例。只測新舊config解析相容，不做既有workspace資料migration。

固定commit的已知baseline只有一項：`TestBundledTeamsNormalize/strategic-decision`因`.agent-teams/strategic-decision/team.yaml`的新description與compatgolden舊description不一致而失敗，其餘normalized內容相同。PR-0第一個commit MUST以source team definition為authority，執行`UPDATE_TEAM_COMPAT_GOLDEN=1 go test ./internal/team -run '^TestBundledTeamsNormalize$' -count=1`，只接受description欄位的goldendiff，接著重跑完整package tests。不得把team definition改回舊敘述來迎合golden。

schema分離、`request:`頂層authoring mapping、`decision.routing.hints`、profile/planinspect等前版簡化方向保留，但不得把legacy`default-profile`自動改成`primary-profile`，兩者語意不同。新的shortform只在primaryscope建立v2alias；舊local`standard`仍原樣解析。不改寫既有workspace或team檔案。

## 7.4 驗證命令與證據要求

Coding agent必須在repo實際執行既有Go/lint/docgates與新增targetedtests，記錄command與exitcode。不要用本文件附的schemaexamples冒充integrationtests。最低包括：

```bash
# 在本contract bundle目錄執行：
go run validate_contract_schemas.go
python3 validate_contract_examples.py

# 在repository root執行：
go test ./internal/agent ./internal/team ./cmd/hufu
go test -race ./internal/team ./cmd/hufu
# 另執行 repository 當前 AGENTS.md 指定的 lint / docs gate。
```

文件提供的`validate_contract_examples.py`只驗strictschemas、literals、canonicaldigests與少量跨欄位sampleinvariants；它**不**證明scheduler、providerisolation、fsync、signal或crashrecovery已正確。實作起點是本bundle所有schema、fixtures、digests與checksums通過；正式合併門檻是repository tests、race tests、lint及本節命令全部exit 0。沒有額外的人工review、live-provider或production驗證前置工作。

---

# 8. 來源與變更追蹤

本版程式码來源均固定到commit `c4899849bb46c2f2acb1316ba74e2f0f681ff884`。以下連結提供實作者定位；只將對應讀取範圍的現況當作已查核，不能推論未讀到的caller已正確。

| 來源 | 固定版本位置 | 支持的現況 |
|---|---|---|
| [S01] | [`internal/team/run_finalizer.go`](https://github.com/kjelly/hufu/blob/c4899849bb46c2f2acb1316ba74e2f0f681ff884/internal/team/run_finalizer.go#L1-L330) | FinalizeRun、terminal lifecycle、detached cleanup、event-first projection |
| [S02] | [`internal/team/run_finalizer.go`](https://github.com/kjelly/hufu/blob/c4899849bb46c2f2acb1316ba74e2f0f681ff884/internal/team/run_finalizer.go#L330-L615) | single terminal writer、idempotency key、pending commit、emergency finalize |
| [S03] | [`internal/team/execution_events.go`](https://github.com/kjelly/hufu/blob/c4899849bb46c2f2acb1316ba74e2f0f681ff884/internal/team/execution_events.go#L1-L230) | physical invocation identity、lifecycle payload、execution telemetry shadow |
| [S04] | [`internal/team/event_store.go`](https://github.com/kjelly/hufu/blob/c4899849bb46c2f2acb1316ba74e2f0f681ff884/internal/team/event_store.go#L1-L185) | event_store.jsonl、RunEvent schema 2、branch-scoped idempotency |
| [S05] | [`internal/team/decision_engine.go`](https://github.com/kjelly/hufu/blob/c4899849bb46c2f2acb1316ba74e2f0f681ff884/internal/team/decision_engine.go#L1-L220) | DecisionEngine、runners、request/packet/provenance ownership |
| [S06] | [`internal/team/decision_types.go`](https://github.com/kjelly/hufu/blob/c4899849bb46c2f2acb1316ba74e2f0f681ff884/internal/team/decision_types.go#L1-L270) | Record schema 3、typed assumptions/base rates/opinions、runtime/model provenance |
| [S07] | [`internal/agent/decision_config.go`](https://github.com/kjelly/hufu/blob/c4899849bb46c2f2acb1316ba74e2f0f681ff884/internal/agent/decision_config.go#L365-L735) | role validation、policy field shapes、RequestContract validation |
| [S08] | [`internal/team/coordinator_tools.go`](https://github.com/kjelly/hufu/blob/c4899849bb46c2f2acb1316ba74e2f0f681ff884/internal/team/coordinator_tools.go#L220-L530) | finishTool、pending/failed checks、acceptance repair、terminal hard stop |
| [S09] | [`internal/team/coordinator_run.go`](https://github.com/kjelly/hufu/blob/c4899849bb46c2f2acb1316ba74e2f0f681ff884/internal/team/coordinator_run.go#L1-L140) | direct-agent dispatch context；finalizeDirectRun 函式亦由同檔 code search 定位 |
| [S10] | [`internal/team/run_result.go`](https://github.com/kjelly/hufu/blob/c4899849bb46c2f2acb1316ba74e2f0f681ff884/internal/team/run_result.go#L350-L670) | EvaluateRunOutcome、exit codes、multi-team aggregate |
| [S11] | [`internal/utils/redact.go`](https://github.com/kjelly/hufu/blob/c4899849bb46c2f2acb1316ba74e2f0f681ff884/internal/utils/redact.go#L1-L220) | secret redaction boundary、typed numeric metadata exceptions |
| [S12] | [`cmd/hufu/main.go`](https://github.com/kjelly/hufu/blob/c4899849bb46c2f2acb1316ba74e2f0f681ff884/cmd/hufu/main.go#L1-L210) | ProcessExitCode 傳遞與 main exit mapping |
| [S13] | [`internal/team/coordinator_eventstore.go`](https://github.com/kjelly/hufu/blob/c4899849bb46c2f2acb1316ba74e2f0f681ff884/internal/team/coordinator_eventstore.go#L1-L240) | startup event-first restoration、pending terminal commit reconciliation |


**交付界限：** 本bundle只修改規格與附屬schema/examples，沒有實作Go runtime。所有新fields、events、V2presets與CLIflags均已固定到可開始實作，但在對應PR合併前不得宣稱目前binary支援。
