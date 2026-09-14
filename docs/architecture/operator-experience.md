# hufu CLI／TUI 使用者體驗改善計畫

> Status: active
> Authority: normative
> Verified-Commit: `ba30643754d8d6cd85f91624a3f9bedf9d32457a`
> Supersedes: —
> Superseded-By: —
> Implementation-Ready: yes（依 §1.3 的 PR 邊界與 phase gates 漸進實作）
> 文件性質：已採納的增量 implementation contract；不是已實作功能清單
> 文件版本：1.0｜日期：2026-09-14
> 查核基準：`kjelly/hufu @ ba30643754d8d6cd85f91624a3f9bedf9d32457a`
> 對象：coding agent、reviewer、hufu 維護者
> 正式位置：`docs/architecture/operator-experience.md`；`docs/tmp/` 副本不是權威來源。

## 執行摘要

本計畫的產品驗收原則只有一條：

> **使用者不必理解 hufu 內部有多少 subsystem，也必須能回答：我現在在哪個狀態、發生了什麼、下一步應該做什麼。**

落地方式不是先把命令改名或增加 dashboard，而是先建立共同的 **Operator Snapshot（操作狀態投影）＋ deterministic Next Action（確定性的下一步建議）**。CLI、TUI、錯誤訊息與 review flow 都消費同一份可追溯資料；變更仍交回既有 runtime/service 執行與授權。

優先順序：

```text
相容性基線與使用者旅程
  → scope / state / evidence / next-action 契約
  → CLI 摘要、help、team check、輸出一致性
  → TUI 操作面板與電子紙支援
  → Learning / Promotion review
  → 使用性實測與漸進發布
```

本文件已查讀本次基準的 CLI registration、workspace resolver、model override、inspector、promotion 入口與 TUI model/style，以及 team/context/session 相關程式。基準提交的 focused packages 與 `go test ./...` 已於 2026-09-14 通過；未執行真人 usability test。下列 SLO、效益與驗收數字仍是 release targets，不是已達成的測量結果。實作從 §16 的 **PR-0 characterization fixtures** 開始，不需要再等待 package ownership 或 schema 決策。

---

## 目錄

1. [文件閱讀與執行規則](#section-1)
2. [現況查核與必須修正的先前建議](#section-2)
3. [核心 UX 契約：State → Evidence → Next](#section-3)
4. [非目標與不可破壞的 invariant](#section-4)
5. [共用 Operator Snapshot](#section-5)
6. [Deterministic Next Action](#section-6)
7. [Workspace／Project／Session 的一致語意](#section-7)
8. [CLI 資訊架構與 Progressive Disclosure](#section-8)
9. [Onboarding：Team Check、Wizard 與 Model Resolution](#section-9)
10. [Output／Errors／Exit Codes](#section-10)
11. [TUI：從任務追蹤到操作面板](#section-11)
12. [Learning／Skill／Promotion UX](#section-12)
13. [電子紙、動態主題與可及性](#section-13)
14. [端到端使用者旅程與驗收輸出](#section-14)
15. [實作架構與程式修改邊界](#section-15)
16. [分階段 Implementation Backlog](#section-16)
17. [必要 Regression／Acceptance Test Matrix](#section-17)
18. [使用者體驗驗收：以行為衡量，不以畫面數量衡量](#section-18)
19. [Compatibility、發布、Rollback](#section-19)
20. [Coding Agent 的工作方式與每個 PR 的 Definition of Done](#section-20)
21. [可直接交給 Coding Agent 的起始指令](#section-21)
22. [來源、範圍與追溯](#section-22)

---

<a id="section-1"></a>

## 1. 文件閱讀與執行規則

### 1.1 三種狀態必須分清

| 標記 | 含義 |
|---|---|
| **現況** | 可由已查讀程式碼／文件支持；附 `[Sxx]` 來源 |
| **新增契約** | 本計畫要求實作的行為，不代表 hufu 現在已支援 |
| **實作核對** | 本文件已決定契約；實作者仍須以 characterization test 證明現有行為，發現差異時停止該 work item 並更新本規格或先修正基線 |

**本文所有 `hufu run`、`team check`、`inspect overview`、`session status/resume/retry/reconcile`、`promotion review`、`--theme`、`--display-preset` 等新介面，除明示現況者外，都是目標語法。** 不得把範例直接寫入「目前可用」手冊，直到對應 acceptance test 通過。

### 1.2 必須保留的既有方向

- Runtime 對 execution、verification、recovery、authorization 與 completion 的 ownership 不變。
- Canonical execution evidence 與 canonical context 不被 UI 狀態、Markdown 或 log 猜測取代。
- `-m` 保持 worker execution target override；coordinator、sidecar、guard、judge、plan reviewer 各自解析。
- 同一 session 可呈現不同 backend/model 的 task attempts；UI 不得為了簡化強制全 session 只用一種 backend。
- 無人值守、CLI scripting、SSH/tmux、小終端、電子紙均為主要情境，不是事後相容。
- 前文的命令收斂建議是產品方向，不是立即刪除舊入口的授權。

### 1.3 權威與變更規則

本文件對**新增的 operator UX surface**具有 normative authority；既有 runtime、
inspector、run outcome、execution target、memory promotion 契約仍由各自的 active
architecture 文件與 code/tests 管理。本文件只能新增 projection、adapter 與 presentation，
不能覆蓋既有 execution/recovery/authorization 語意。

實作者發現衝突時依下列規則處理：

1. 既有 surface 的 code/tests 與其 normative architecture 優先；不得以本文件改變舊行為。
2. 新 `overview`、新 `run` façade、Operator Snapshot 與 Next Action 由本文件定義。
3. 若新增 surface 必須改動既有 normative schema 或 runtime policy，先在同一 PR 更新該
   architecture 文件並取得 review；不可只修改實作。
4. 每個 work item 都是獨立交付邊界。未列在該 work item 的功能不因出現在後續章節而進入 scope。

### 1.4 可以立即開始的第一個 PR

第一個 PR 固定為 **HF-UX-001A：characterization baseline**，只新增 tests/fixtures 與
contract inventory，不改 production behavior。最低交付：

- root execution、resume、retry/reconcile 的 workspace path matrix；
- root JSON、inspect JSON、team lint JSON 與對應 exit-code goldens；
- help/completion/read-only command 的 no-create/no-provider assertions；
- 記錄本文件 Verified-Commit、Go/OS 與測試命令結果。

完成 HF-UX-001A 後先實作 HF-UX-001B；Gate P0 通過後，HF-UX-010A 與 HF-UX-011A
可並行。HF-UX-012A 必須等 HF-UX-010B 與 HF-UX-011B 完成。不需要再做 package
ownership、schema 名稱或 MVP 範圍選擇。真人 usability 與電子紙實機驗收是 release gate，
不阻擋前置工程 PR 合併。

---

<a id="section-2"></a>

## 2. 現況查核與必須修正的先前建議

### 2.1 已確認的現有接縫

| 現況 | 對 UX 的影響 | 實作接縫 |
|---|---|---|
| root 同時註冊大量頂層命令及執行、安全、模型、compaction、輸出 flags | 任務導向 discovery 不足；旗標資訊密度過高 | `cmd/hufu/root.go` [S01] |
| 已有 `examples` 與 `help-flags <group>` | 不必從零再建第二套 help metadata | `cmd/hufu/helpcmd.go` [S02] |
| 具名 team 的正常執行把 base workspace 接上 team name；maintenance/resume 類入口另有 exact workspace 語意 | 同一 `-w` 易造成不同理解 | `team_loader.go`、`recoverycmd.go` [S03] [S04] |
| `--model` 僅覆寫 worker，獨立 role flags 已存在 | 新 UX 應顯示 resolution，不重做 prefix 特判 | `model_overrides.go` [S05] |
| `inspect` 是 read-only façade，使用 `internal/inspect` | CLI/TUI 可共用既有 query 層 | `inspectcmd.go`、inspect guide [S06] [S07] |
| `inspect storage` 已能提供 SQLite/WAL 等 metadata，且明確不 migrate/optimize/rebuild | 不應再另建 workspace database inspector | inspect guide [S07] |
| TUI 已有 tasks、detail、result、memory、activity、search、quit confirmation、PTY 關聯 | 不應推倒重寫或重複建立同類畫面 | `internal/tui/tui.go` [S08] |
| TUI styles 存在 package-level 固定色碼，spinner/compact 也有預設開關 | 動態 palette 應移到 instance-scoped rendering | `internal/tui/tui.go` [S08] |
| `internal/tui/tui.go` 現為 3021 行，已超過 CLAUDE.md 建議的 800 行/檔案上限約 4 倍（package 內已有 `overlay.go`、`ask_user.go` 等分檔前例） | 新增 summary strip、theme resolver、snapshot rendering 前必須先分解此檔案，否則既有違規只會惡化 | `internal/tui/tui.go` [S08] |
| `team show/explain/validate/lint` 分工已存在 | `team check` 應是組合入口，不是新驗證引擎 | 前文查讀的對應 CLI [S09] |
| `context promotion` 已有 analyze/list/show/edit/approve/reject/apply | 增加 review façade，沿用 lifecycle | `context_promotion_cmd.go` [S10] |
| promotion 共用 `openPromotion` 會 `OpenSQLite` 並 `flushPromotionEvents` | 不可把現有 CLI handler 當絕對 read-only UI query 直接呼叫 | `context_promotion_cmd.go` [S10] |
| output 存在 `--output`、`--format`、`--json`；inspect 有自己的 versioned envelope / exit code | 必須分開「旗標一致」與「payload/exit contract 變更」 | [S01] [S06] [S07] [S10] |

### 2.2 對先前提案的收斂

| 先前方向 | 本計畫採用的安全版本 |
|---|---|
| 把 top-level commands 收到 8～10 個 | **先分組與漸進揭露**，新增 canonical aliases；不以命令數量作 KPI |
| `-w` 全面改成 exact workspace | 新 `run` 採 exact；**legacy root 路徑不靜默改義**，透過 shared resolver 的 legacy adapter 保留 |
| 統一 `--output text/json/yaml` | **text/json 優先**；格式按 command capabilities 支援；Mermaid 等 domain format 保留；不強迫所有結果提供 YAML |
| 統一 exit code 成 0～4 | **不直接改現有 codes**；先新增結構化 `error.kind`，需要改碼時另提 versioned migration |
| `team check` 一次全跑 | 預設 static；online checks 顯式 opt-in，所有 skip/unknown 都如實顯示 |
| TUI 呼叫 inspect/context/session CLI | 同程序依賴 service/query API；**不 parse stdout、不以 shell 呼叫自己當內部 API** |
| approve 完即可接 apply | approve 與 apply 仍是兩次不同操作，apply 前再次展示 diff、scope、風險及 stale preflight |
| 「Next」永遠是一條命令 | 可以是 wait、review、provide-input、inspect、none；**不得為了有下一步而虛構動作** |

這些調整屬本次設計判斷，目的是降低 semantic regression 與誤操作，而非宣稱原程式已有相同機制。

---

<a id="section-3"></a>

## 3. 核心 UX 契約：State → Evidence → Next

### 3.1 所有核心畫面都回答三個問題

| 問題 | 最低必要資訊 | 禁止的呈現 |
|---|---|---|
| **我現在在哪裡？** | team、project identity、exact workspace、session/run、active branch、task/attempt（如適用）、目前 activity | 只顯示 `Running...`；用 cwd 冒充 canonical project ID |
| **發生了什麼？** | 最新重要 durable transition、verification/acceptance 摘要、失敗分類、證據參考、資料新鮮度 | 以模型自述「完成」當成功；以缺少資料當通過 |
| **下一步做什麼？** | 一個 primary action、原因、執行者、風險、先決條件；沒有必要行動則明示 | 自動套用、越權 retry、把建議當授權、只顯示 stack trace |

正常執行時使用者只需看到一個**操作摘要區塊**；詳細資訊由 disclosure、`inspect` 或 detail view 展開。不能為了滿足三個問題而在每個 token 重複印整份摘要。

### 3.2 不把不同維度壓成單一綠／紅燈

新增 UX view model 必須分開：

```text
operation_status  這次 CLI 查詢／變更是否成功
activity_state    runtime 現在正在做什麼
run_outcome       runtime 所判定的完成結果
integrity         evidence chain / projection 是否可信
attention         使用者是否需要介入
freshness         已知資料截至哪個時間／revision
```

例如：

```text
operation_status = succeeded       # inspect 成功
activity_state   = finished
run_outcome      = failed          # 被查詢的 run 失敗
integrity        = valid
attention        = review_required
```

這不是矛盾。Inspector exit 0 不等於 run success；existing contract 已明確區分。[S07]

### 3.3 Activity vocabulary（顯示投影，不新增 runtime state machine）

| UX activity | 只能由何種資訊推導 | Primary action 類型 |
|---|---|---|
| `unconfigured` | 缺少必要 team/model 設定 | configure/check |
| `ready` | 指定範圍的 preflight 已完成 | run，或 none |
| `preflight` | 可觀察的 preflight 階段 | wait |
| `planning` | runtime planning/prepare phase | wait 或 review-plan |
| `executing` | 尚在執行的 tasks/attempts | wait；可展開 inspection |
| `verifying` | worker 完成但 verifier/acceptance 尚未完成 | wait，不顯示成功 |
| `waiting_input` | runtime 有 pending input request | provide-input |
| `waiting_approval` | runtime 有 pending approval gate | review，不預選 approve |
| `blocked` | 已存在 policy/recovery/dependency blocker | inspect/reconcile/configure，依權威 policy |
| `wrapping_up` | runtime 已接受收尾 request | wait；不立即顯示 cancelled |
| `interrupted` | durable checkpoint 顯示中斷；非僅 UI 離線 | inspect/reconcile/resume |
| `finished` | runtime terminal transition | review-result / none |
| `unknown` | 來源不足、損壞、讀取失敗或已過期 | inspect/select-target；禁止推測 retry |

#### Activity v1 precedence

`internal/operator.DeriveActivity(Facts)` 必須是 pure function，依下列順序取第一個符合者；
同一輸入不得因 map iteration、時間或 renderer 不同而改變結果：

1. scope ambiguous/conflict、event chain invalid、必要 projection unreadable → `unknown`；
2. 已驗證 `run_finished` 與其 `RunResult` → `finished`，即使 outcome 是 failed/cancelled/stalled；
3. durable pending input gate → `waiting_input`；
4. durable pending approval/review gate → `waiting_approval`；
5. durable wrap-up accepted、但尚無 `run_finished` → `wrapping_up`；
6. durable interruption/checkpoint，且尚無 terminal run → `interrupted`；
7. 任一 task 為 `blocked`、`error` 或 `protocol_incomplete`，且沒有可執行中的 dependency repair → `blocked`；
8. 任一 task 為 `verifying` → `verifying`；
9. 任一 task 為 `in_progress` → `executing`；
10. 任一 task 為 `planned` → `planning`；
11. 可觀察的 static/online preflight 正在執行 → `preflight`；
12. preflight 對其聲明的檢查範圍成功且尚未建立 run → `ready`；
13. 缺少已知必要設定 → `unconfigured`；
14. 其餘 → `unknown`。

`pending`、`paused`、`skipped`、`done` 單獨存在不足以推導 executing、interrupted 或
finished；必須結合 durable run/gate facts。未知的 `TaskStatus`、`RunOutcome`、gate type 或
reason code 原樣放入 `raw_*`，normalized activity 固定為 `unknown`。State mapping 不寫回
event store、session、context DB 或第二份 task state。

`attention` v1 只允許 `none|informational|action_available|review_required|human_required|unknown`。
`finished` 不代表 `attention=none`；例如 failed terminal run 對應 `review_required`。

### 3.4 CLI 摘要範例（目標文案）

```text
Team: hufu-dev     Workspace: /repo/workspace/hufu-dev
Run: run-42       Branch: main       Task: task-7 / attempt 2

State    BLOCKED — 執行結果待確認
What     worker 中斷；已有 execution receipt，外部副作用是否完成仍未知。
Evidence receipt: receipt-9  |  verification: pending
Next     先檢查 task-7 的 recovery evidence，再執行允許的 reconcile。
         不要直接 retry：可能重複寫入外部系統。

Data     durable through event e-314; live connection unavailable
```

Run ID、task ID、evidence references 都必須來自實際資料；範例 ID 僅用於文件與 fixtures。

---

<a id="section-4"></a>

## 4. 非目標與不可破壞的 invariant

**本計畫不做：**更換 CLI/TUI framework、新增 Web server/帳號系統、重寫 execution backend、重建 memory DB、讓 LLM 產生 recovery command、fine-tuning、自動發布 skill、為 UX 重排 runtime workflow。

| ID | 必須成立的 invariant |
|---|---|
| INV-01 | execution truth 仍來自既有 canonical events / receipt / completion gate；context truth 仍由 canonical repository 提供 |
| INV-02 | 查看、help、completion、theme 切換、next-action 查詢不執行 provider、tool、verifier、recovery 或 promotion |
| INV-03 | UI selection、prefill、Next Action、複製命令、generic auto-approve 都不是變更授權 |
| INV-04 | 每次 mutation 重新 resolve scope、驗證目前 state/revision、檢查 policy 與既有人工 gate |
| INV-05 | 不因「改善 UX」重跑已完成 side effect，或用 retry 修 learning projection |
| INV-06 | legacy CLI syntax、workspace resolution、JSON payload 與 exit code 有 characterization tests 保護 |
| INV-07 | 顯示層不從 raw logs 推定成功、信用或因果；LLM claim 必須標記為 claim |
| INV-08 | Runtime retrieval 不因 UI 能查看 descendants/private memory 而放寬 scope；administrative view 與 runtime ancestors-only visibility 分離 |
| INV-09 | draft 不進正式 skill pool；promotion approve/apply 與來源/target stale checks 不被縮減 |
| INV-10 | 主題、排序、展開折疊、spinner、refresh 只改 presentation，不改任何 runtime contract |
| INV-11 | 不能在 read-only view 開啟 writable DB 然後順便 migrate、flush outbox、repair 或建立 workspace |
| INV-12 | `--input` 的 typed execution semantics 不與 `--var` 的 template semantics 合併；同理三種 profile 不合併 |
| INV-13 | 不直接以後來的設定覆寫歷史 attempt 的 backend/model/policy 展示 |
| INV-14 | 公用 renderer 與 command builder 必須阻擋 terminal escape injection、shell injection 與機密洩漏 |

---

<a id="section-5"></a>

## 5. 共用 Operator Snapshot

### 5.1 已決定的 package ownership

```text
existing runtime / canonical event / context / promotion services
                        │
               read-only query adapters
                        │
                        ▼
                 OperatorSnapshot
                        │
           pure presentation/action derivation
                ┌───────┼──────────┐
                ▼       ▼          ▼
              CLI      TUI      JSON envelope
                        │
             explicit user command intent
                        │
                        ▼
        existing command service + policy + revalidation
```

Ownership 固定如下：

| Package | Owns | Must not own |
|---|---|---|
| `internal/operator` | v1 view types、enum validation、activity/attention/action pure functions、snapshot/revalidation hash | filesystem/DB/provider access、Cobra、runtime mutation |
| `internal/inspect` | canonical event/context/promotion read-only adapters、target selection、`overview` assembly | action execution、workspace creation、policy modification |
| `cmd/hufu` | flags、legacy/exact adapter、output、typed command intent、mutation 前 revalidation | state inference、stdout parsing、第二套 resolver |
| `internal/tui` | immutable snapshot rendering、focus/theme/refresh UI state | canonical outcome/recovery decision、直接 DB mutation |
| `internal/team` / `internal/promotion` | 既有 execution/recovery/promotion policy 與 mutation | presentation-specific state |

`internal/operator` 可以 import canonical value types，但不能 import `internal/inspect`、`cmd/hufu`
或 `internal/tui`。`internal/inspect` import `internal/operator` 並回傳 snapshot；CLI/TUI 消費同一型別。
禁止建立第二份 overview、action resolver 或 canonical store。

### 5.2 Operator Snapshot v1 資料契約

```go
const SchemaVersion = 1

type OperatorSnapshot struct {
    SchemaVersion    int                `json:"schema_version"`
    SnapshotID       string             `json:"snapshot_id"`
    Scope            ResolvedScope      `json:"scope"`
    Activity         ActivityView       `json:"activity"`
    Outcome          OutcomeView        `json:"outcome"`
    Integrity        IntegrityView      `json:"integrity"`
    Freshness        FreshnessView      `json:"freshness"`
    Attention        string             `json:"attention"`
    Blockers         []DiagnosticView   `json:"blockers"`
    LatestChanges    []ChangeView       `json:"latest_changes"`
    RoleTargets      []RoleTargetView   `json:"role_targets"`
    Learning         LearningView       `json:"learning"`
    PrimaryAction    *ActionSuggestion  `json:"primary_action"`
    SecondaryActions []ActionSuggestion `json:"secondary_actions"`
}

type ResolvedScope struct {
    RequestedPath      string `json:"requested_path"`
    RequestedSemantics string `json:"requested_semantics"` // legacy_base|exact|root|default
    WorkspaceExact     string `json:"workspace_exact"`
    WorkspaceRoot      string `json:"workspace_root"`
    ProjectDir         string `json:"project_dir"`
    ProjectID          string `json:"project_id"`
    TeamName           string `json:"team_name"`
    TeamDir            string `json:"team_dir"`
    SessionID          string `json:"session_id"`
    RunID              string `json:"run_id"`
    BranchID           string `json:"branch_id"`
    SelectionSource    string `json:"selection_source"` // explicit|active_binding|single_candidate|legacy
    BindingStatus      string `json:"binding_status"`   // verified|absent|ambiguous|conflict|unknown
}

type ActivityView struct {
    State          string   `json:"state"`
    RawTaskStates  []string `json:"raw_task_states"`
    RawReasonCodes []string `json:"raw_reason_codes"`
}

type OutcomeView struct {
    RunOutcome      string `json:"run_outcome"`
    GoalSatisfied   *bool  `json:"goal_satisfied"`
    StopReason      string `json:"stop_reason"`
    AcceptanceState string `json:"acceptance_state"`
    CompletionState string `json:"completion_state"`
}

type IntegrityView struct {
    Status      string   `json:"status"`       // valid|degraded|invalid|unknown
    EventChain  string   `json:"event_chain"` // verified|invalid|unavailable
    Projection  string   `json:"projection"`  // consistent|drift|unavailable
    ReasonCodes []string `json:"reason_codes"`
}

type FreshnessView struct {
    QueriedAt      string `json:"queried_at"`       // RFC3339Nano
    EventID        string `json:"event_id"`
    EventHash      string `json:"event_hash"`
    EventOrdinal   int64  `json:"event_ordinal"`    // validated global file order, not persisted sequence
    ContextRevision string `json:"context_revision"`
    LiveState      string `json:"live_state"`       // connected|disconnected|not_applicable|unknown
    StaleReason    string `json:"stale_reason"`
}

type DiagnosticView struct {
    Code     string `json:"code"`
    Severity string `json:"severity"` // info|warning|error
    Message  string `json:"message"`
    Ref      string `json:"ref"`
}

type ChangeView struct {
    EventID      string   `json:"event_id"`
    EventOrdinal int64    `json:"event_ordinal"`
    Kind         string   `json:"kind"`
    Status       string   `json:"status"`
    ReasonCode   string   `json:"reason_code"`
    Refs         []string `json:"refs"`
}

type RoleTargetView struct {
    Role         string `json:"role"`
    Requested    string `json:"requested"`
    Effective    string `json:"effective"`
    BackendKind  string `json:"backend_kind"`
    Source       string `json:"source"`
    Availability string `json:"availability"` // verified|unverified|unavailable|unknown
    ReasonCode   string `json:"reason_code"`
}

type LearningView struct {
    Status              string `json:"status"` // available|not_applicable|unavailable|unknown
    RequestedMode       string `json:"requested_mode"`
    EffectiveMode       string `json:"effective_mode"`
    PolicyVersion       string `json:"policy_version"`
    Exposures           *int64 `json:"exposures"`
    Applied             *int64 `json:"applied"`
    VerifiedSupport     *int64 `json:"verified_support"`
    EligiblePromotions  *int64 `json:"eligible_promotions"`
    UnavailableReason   string `json:"unavailable_reason"`
}
```

`*bool`／`*int64` 的 `null` 表示資料未知或未查詢；零表示已成功查詢且結果為零。所有 slice
必須輸出 `[]` 而非 `null`，並在 hash/render 前依本節定義的 key 排序。核心欄位不使用
`omitempty`，避免 consumer 無法區分 schema drift 與空值。Phase 1 即固定完整 v1 shape；
尚未接上的 learning/role adapter 輸出明確 `unavailable`，後續 phase 不改欄位語意。

| Field | 契約 |
|---|---|
| `Scope` | canonical IDs、exact path、selection provenance、team definition reference；缺少時用 unknown，不補造 ID |
| `Activity` | normalized display state ＋ raw runtime state/reason code |
| `Outcome` | existing runtime outcome / acceptance / verification；不自行重新裁決 |
| `Integrity` | valid/degraded/invalid/unknown，來源與診斷；禁止 bool 混淆「未檢查」 |
| `Freshness` | queried_at、source revision/event anchor、live connection、stale reason；snapshot 時間不當作 runtime heartbeat |
| `LatestChanges` | 預設最多 3 個重要 transition；使用通過 hash-chain 驗證的 global event file ordinal，不稱為 durable/persisted sequence |
| `RoleTargets` | requested selector、effective target、backend kind、role、source、verified/unverified availability；遮蔽 credential |
| `Learning` | requested/effective mode、policy version、可驗證 counters、eligibility summary 與 unavailable reason |
| `PrimaryAction` | 同一 scope/facts/policy 下固定；nil 合法，另有原因；不是 runnable authority |

### 5.3 一致性與查詢成本

1. Snapshot 要能說明各來源對應的 anchor；event 與 SQLite 無法取得一致 view 時標記 degraded，不偽裝 atomic consistency。
2. SQLite 查詢優先沿用既有 read-only repository／transaction；**不得把 active WAL database 當 immutable snapshot 開啟**來躲避一致性問題。
3. `SnapshotID` 算法固定為 `sha256:` + hex(SHA-256(`hufu-operator-snapshot-v1\x00` + canonical JSON))。Canonical JSON 使用專用無 map DTO；slice 依 `event_ordinal,event_id`、`role`、`code,ref` 排序，排除 `queried_at`、rendered message、live connection 與 actions，包含 scope binding、event ID/hash/ordinal、context revision、normalized activity/outcome/integrity、role targets、learning counters及 policy version。
4. 不得每個 token 全量 replay event log。使用既有 bounded query、增量 event anchor 或 memory cache；持久化 cache 若需要，必須是可拋棄 projection 且讀取不強制建立。
5. unknown/missing/permission denied 各自有 reason；不要把 `0 records` 與 `query failed` 視為同一狀態。
6. 總數可能增長的 DAG 不显示虛假完成百分比。可顯示「目前 3/7 個已知 tasks 完成；仍可新增」。預算百分比只有分母已知時才顯示。
7. 有限長度、lazy detail、取消查詢與 backpressure 必須設計在 adapter；不能讓昂貴 memory scan 阻塞 task 狀態畫面。

Integrity v1 mapping：event chain invalid或 required canonical projection drift → `invalid`；event
chain verified 且所有本次 requested required projections consistent → `valid`；event chain verified
但 optional projection unavailable/changed during read → `degraded`；沒有 event store、schema不支援
或無法判定 → `unknown`。`invalid`/`unknown` 都禁止 mutation suggestion；`degraded` 只有與缺失
projection無關的 read-only action可 available。

Freshness 不使用任意 elapsed-time timeout推斷 crash。`queried_at` 只是 query時間；event timestamp
只是最後 durable fact時間。`stale_reason` 只在 `projection_changed_during_read`、明確 owner
connection失聯、或 revalidation mismatch時設定。Passive view沒有 live channel時
`live_state=not_applicable`，不因而標 stale。`context_revision` 使用本 snapshot實際讀取之
aggregate/proposal revision的排序 hash；未查 context時留空，不使用 DB mtime或 WAL size代替。

### 5.4 新增查詢入口

```bash
# 新增：read-only overview，exact execution workspace
hufu inspect overview --workspace /repo/workspace/hufu-dev
hufu inspect overview --workspace /repo/workspace/hufu-dev --output json

# 指定歷史 run；不改 active branch/session
hufu inspect overview --workspace /repo/workspace/hufu-dev \
  --run run-42 --branch branch-main --output json
```

重用既有 inspect envelope，新增 `KindOverview = "overview"`，在 `data` 放
`OperatorSnapshot`；既有 kind、payload、exit code 與 field order byte-for-byte 保持不變。
`overview` 成功時使用 inspector schema version 1。Session status 是 overview 的面向使用者
façade，不是第二份 reducer。

未指定 `--branch` 時使用 `session_tree.json` 經驗證的 active branch；沒有 tree 時使用既有
implicit main lineage。未指定 `--run` 時，只能採用 active session/run binding，或 selected
lineage 中唯一的 run；兩者皆無或存在多個候選時回 not-found/ambiguous，不以 mtime、檔案順序
或「最新」猜測。`--run`/`--branch` 都只做選擇，不 checkout 或修改 active state。

---

<a id="section-6"></a>

## 6. Deterministic Next Action

### 6.1 Action suggestion contract

| Field | 要求 |
|---|---|
| `id` / `reason_code` | 穩定 machine key，與翻譯、rendered text 無關 |
| `kind` | `wait` / `inspect` / `configure` / `provide_input` / `review` / `mutate` / `none` |
| `actor` | user / runtime / external-owner；等待 runtime 時不要暗示使用者須下命令 |
| `target` | exact workspace + run/session/branch/task/attempt/proposal，僅含此動作所需的真實 IDs |
| `preconditions` | status、revision、policy readiness、未結案副作用等；每次執行重新檢查 |
| `risk` | read-only / local-write / external-effect / approval；表示真實效果，不因命令叫 inspect/reconcile 就推定安全 |
| `argv` | 可信 command registry 產生的參數陣列；不接受模型產生 shell 字串 |
| `cwd` | 明確工作目錄（若需要）；必要的 team search path 也要由 resolver 帶入 |
| `confirmation` | none / existing-runtime-gate / explicit-review；不得由 generic `--yes` 降級 |
| `source_refs` | recommendation 依據的 event/reason/receipt，不能只有 prose |
| `revalidation_key` | 綁定 relevant target revisions；不以 workspace 任意新 event 一律判失效 |
| `availability` | available / blocked / unknown；blocked reason 可展開查看 |

v1 型別固定如下；`Target` 與 `Preconditions` 不使用任意 map：

```go
type ActionSuggestion struct {
    ID              string               `json:"id"`
    Kind            string               `json:"kind"`
    Actor           string               `json:"actor"`
    ReasonCode      string               `json:"reason_code"`
    Availability    string               `json:"availability"`
    Risk            string               `json:"risk"`
    Target          ActionTarget         `json:"target"`
    Preconditions   ActionPreconditions  `json:"preconditions"`
    Argv            []string             `json:"argv"`
    CWD             string               `json:"cwd"`
    Confirmation    string               `json:"confirmation"`
    SourceRefs      []string             `json:"source_refs"`
    RevalidationKey string               `json:"revalidation_key"`
}

type ActionTarget struct {
    Workspace string `json:"workspace"`
    ProjectID string `json:"project_id"`
    TeamID    string `json:"team_id"`
    SessionID string `json:"session_id"`
    RunID     string `json:"run_id"`
    BranchID  string `json:"branch_id"`
    TaskID    string `json:"task_id"`
    Attempt   int    `json:"attempt"`
    ProposalID string `json:"proposal_id"`
}

type ActionPreconditions struct {
    BindingStatus       string `json:"binding_status"`
    ExpectedActivity    string `json:"expected_activity"`
    ExpectedTaskStatus  string `json:"expected_task_status"`
    ExpectedEventID     string `json:"expected_event_id"`
    ExpectedEventHash   string `json:"expected_event_hash"`
    ExpectedPolicy      string `json:"expected_policy"`
    ExpectedRevision    string `json:"expected_revision"`
    ExternalEffectState string `json:"external_effect_state"`
}
```

所有 slice 輸出 `[]`。`wait`/`none` 的 `argv` 為空；`blocked`/`unknown` availability 的
`argv` 也必須為空，避免 UI 將不可用建議執行。

### 6.2 決策順序

下列是 UX selection precedence；**不是新的 recovery policy**：

| 優先 | 可驗證條件 | Primary action |
|---|---|---|
| 1 | scope 不唯一／不符，或 canonical integrity invalid | 停止 mutation；選定 target 或查看 integrity diagnostics |
| 2 | 已知 external/infra/credential side effect 結果未知 | read-only evidence inspection；只有既有 recovery service 允許時才提供 reconcile |
| 3 | authorization / policy deny | 說明限制與合法設定檢查；不推薦 `--force`、關 gate 或放寬所有權限 |
| 4 | 有 pending approval / input gate | 導向該 gate；列出受影響 scope，不預先肯定 |
| 5 | projection 問題但 runtime outcome/effect 已確定 | 先診斷，再提供明確維護入口；不得 rerun worker |
| 6 | interrupted 且 recovery service 有可用操作 | 根據既有分類提供 resume/reconcile；retry 只在允許後展示 |
| 7 | runtime 確認仍在執行或驗證 | wait；次要動作可 inspect，不能因 no output 直接 retry |
| 8 | run terminal 且無 blocker | review result / none；「無須操作」是成功體驗 |
| 9 | 條件不足 | inspect/unknown；絕不為湊出建議建立寫入命令 |

多個 blockers 同時存在：顯示 highest-priority action 與「另有 N 個待處理」，最多展開兩個 secondary actions；不能把安全 blocker 隱藏在裝飾性摘要下。

#### v1 action registry

| Action ID | 條件 | kind/risk | argv contract | 首次可用工作 |
|---|---|---|---|---|
| `select-scope` | binding ambiguous/conflict | inspect/read-only | `[]`；由 TTY selector 或錯誤訊息處理 | HF-UX-011B |
| `inspect-integrity` | event/projection invalid 或 degraded | inspect/read-only | `hufu inspect replay RUN --workspace WS [--branch B]`；沒有唯一 RUN 時為 blocked | HF-UX-012A |
| `inspect-task-recovery` | external effect unknown | inspect/read-only | `hufu inspect task TASK --run RUN --workspace WS [--branch B] [--attempt N]` | HF-UX-020A |
| `provide-input` | pending input gate | provide_input/approval | owner TUI 使用 typed intent；CLI v1 無 argv | HF-UX-020A |
| `review-approval` | pending approval gate | review/approval | owner TUI 使用 typed intent；不產生 approve argv | HF-UX-020A |
| `wait-runtime` | planning/executing/verifying/wrapping_up | wait/read-only | `[]` | HF-UX-020A |
| `resume-session` | durable interrupted 且既有 resume policy允許 | mutate/local-write | `hufu session resume --workspace WS --team TEAM --run RUN --branch B`，只在 HF-UX-031B façade 存在後 available | HF-UX-031B |
| `reconcile-task` | recovery service 明確回報 reconcile eligible | mutate/external-effect | `hufu session reconcile --workspace WS --team TEAM --run RUN --branch B --task TASK --attempt N` | HF-UX-031B |
| `retry-task` | reconcile/receipt 已證明 retry safe 且 policy允許 | mutate/external-effect | 同上使用 `session retry`；未證明時不得出現 | HF-UX-031B |
| `review-result` | terminal run，且 outcome 需人工查看 | inspect/read-only | `hufu inspect run RUN --workspace WS [--branch B]` | HF-UX-020A |
| `none-required` | terminal completed 且無 blocker | none/read-only | `[]` | HF-UX-020A |

argv 的 flag 順序固定為 command path、`--workspace`、`--team`、`--run`、`--branch`、
`--task`、`--attempt`；空 selector 不輸出。Registry 只生成已註冊且已有 contract test 的命令。
在對應 façade 落地前，必須保留相同 ID/reason，但 `availability=blocked`、`argv=[]`。

### 6.3 指令安全與 freshness

- 顯示命令與執行 intent 來自同一 typed command builder；argv 才是 authoritative，pretty command 只是 shell-specific rendering。
- Bash、PowerShell 等 renderer 各自 escape；無可靠 renderer 的 shell 顯示 argv/說明，不提供誤導的「直接複製」。禁止 `sh -c` 拼接模型、路徑、task 名稱。
- 命令不得包含 API key、token、raw prompt、memory content。Target path 可在 local view 顯示；供分享的 diagnostics 要有 redaction。
- UI refresh 不執行 action。按 Enter 顯示 detail/preview，不能默認執行 primary mutation。
- 每次 mutation 重新讀取 target state，發現 task 已完成、branch 已換、proposal draft/source/target 已改，拒絕 stale action 並刷新畫面。
- Keyboard repetition、double click、重送 intent 不得造成重複操作。沿用服務既有 idempotency；沒有該保證時實作最小 command-side gate，不能宣稱 exactly-once external effect。
- 沒有 reconciliation capability 時明示需要人工確認，而不是虛構可運行的 probe。

`RevalidationKey` 固定為 `sha256:` + hex(SHA-256(`hufu-operator-action-v1\x00` + canonical
JSON))。輸入只含 `ActionTarget`、`ActionPreconditions`、action ID、risk、confirmation 與排序後
source refs；不含文案、queried_at、unrelated workspace head 或 pretty command。任何 mutation
handler 必須重新建立同一份 target-specific facts，比對 key，再呼叫既有 service；不一致回
`stale_action`，不得自動刷新後繼續執行。

`ExpectedRevision` 來源按 action 固定：session resume 使用 active branch ID + selected
checkpoint/session projection hash；task reconcile/retry 使用該 task最新 canonical event ID/hash、
attempt、receipt fingerprint、recovery state與 effective policy revision；promotion edit/approve/
apply 使用 proposal draft hash、source aggregate revisions、target base hash與proposal status。
任何必要來源缺少時 mutation action `availability=unknown` 且無 argv。Read-only/wait/none action
的 RevalidationKey可為空，執行時仍重新 resolve query scope。

### 6.4 可機器讀的範例（proposed）

```json
{
  "id": "inspect-task-recovery",
  "kind": "inspect",
  "actor": "user",
  "reason_code": "external_effect_unknown",
  "availability": "available",
  "risk": "read_only",
  "argv": [
    "hufu", "inspect", "task", "task-7",
    "--run", "run-42", "--branch", "branch-main",
    "--workspace", "/repo/workspace/hufu-dev"
  ],
  "confirmation": "none",
  "source_refs": ["receipt-9"],
  "revalidation_key": "example-target-revision"
}
```

範例只能讀取；下一個 reconcile/retry intent 必須經 scope-safe recovery adapter 提供。UI 不得只因兩個 task 恰好同名就使用最新 active task 代替指定 run 的 task。


---

<a id="section-7"></a>

## 7. Workspace／Project／Session 的一致語意

### 7.1 核心決策：新入口 exact，舊入口不改義

**新增契約：**新 `hufu run --workspace/-w` 永遠指向 exact execution workspace。需要依 team 自動展開時，用 `--workspace-root`。兩者互斥。

```bash
# 新介面：使用的就是 /repo/workspace/hufu-dev
hufu run --team hufu-dev --workspace /repo/workspace/hufu-dev -- "修正錯誤"

# 新介面：明確要求 root + team 展開
hufu run --team hufu-dev --workspace-root /repo/workspace -- "修正錯誤"

# 舊介面：維持舊 resolver，不可改成 exact
hufu --agent-team hufu-dev --workspace /repo/workspace "修正錯誤"
```

**不能只把 `run` 轉呼叫原 root handler 然後傳入 exact path。** 原 handler 會接上 team name，造成雙重巢狀路徑；新 façade 必須把已 resolve 的 exact scope 傳給現有 exact-workspace loading seam。[S03] [S04]

### 7.2 兩層 resolver 契約

不得建立一個同時猜測 path 與 durable identity 的 resolver。`internal/operator` 提供兩個明確
階段：

```go
type WorkspaceRequest struct {
    RequestedPath string
    Mode          string // legacy_base|exact|root|default
    TeamName      string
    ProjectDir    string // new run only; canonicalized runtime CWD supplied by caller
}

type WorkspaceResolution struct {
    RequestedPath      string
    RequestedSemantics string
    WorkspaceExact     string
    WorkspaceRoot      string
    ProjectDir         string
    TeamName           string
}

type BindingRequest struct {
    Workspace WorkspaceResolution
    ProjectID string
    TeamID    string
    SessionID string
    RunID     string
    BranchID  string
}
```

1. `ResolveWorkspacePath` 只處理 path mode：`legacy_base` 對 named team join 一次；`exact`
   不 join；`root` 要求非空 team 並 join 一次；`default` 使用現行 command 的 documented
   default。path 先 `Abs`，存在時 `EvalSymlinks`，不存在時保留 cleaned absolute path。
2. `internal/inspect.BindReadTarget` 只讀 event lineage/session tree/context metadata，把明確
   selectors 與 persisted binding 比對後產生 `ResolvedScope`。
3. `cmd/hufu` mutation adapter 在執行前再次呼叫同一 binding query，驗證
   `RevalidationKey`，再呼叫既有 service。它不得自行從 basename、mtime 或 slice 第一筆推導。

Legacy handlers可以先使用 adapter 維持原語意；新 reader 與 mutation 都使用
`WorkspaceResolution` + `ResolvedScope`，不能在每個 handler 另做 `filepath.Join` 或 basename
inference。

### 7.3 Selection precedence 與錯誤規則

| 情境 | 新介面行為 |
|---|---|
| `--workspace` 明確指定 | exact，不再自動加 team 子目錄 |
| `--workspace-root` + 單一 team | root 下導出 team workspace，明示 resolved path |
| 未指定 workspace，但 team 與 project root 可唯一確定 | `run` 使用 `<canonical-project-dir>/workspace/<team>`；read/recovery 不從 project tree 搜尋，要求 workspace 或使用該 command 已有且已 characterization 的 default |
| 多個候選 workspace/session | TTY 顯示 selector；non-TTY 回 ambiguous error，絕不選最新檔案 |
| 指定 path 不存在，操作是 inspect/resume/context read | 回 not-found；不建立空 workspace 或 DB |
| 指定 path 不存在，操作是 run | preflight 後才建立，先告知位置；失敗前不能留下誤導的 session |
| workspace canonical identity 與 `--team/--run/--project` 不符 | fail closed，不把參數當成覆寫既有 binding 的授權 |
| 只有 path basename 看起來等於 team | 最多是 candidate hint，不足以授權 mutation |
| session branch 與 Git branch 不同 | 顯示為不同欄位，不暗示 checkout session 會 checkout Git 或 rollback filesystem |
| symlink、相對路徑、worktree alias | 使用既有 canonicalization 規則；保留 requested spelling 供解釋 |
| multi-team prompt + exact workspace | 新入口拒絕讓不同 team 共用單一 exact path；要求 workspace-root 或已定義的明確 mapping |

新 run 的 ProjectID 仍由 runtime 現有 `canonicalPath(os.Getwd())` derivation 產生並寫入既有
canonical facts；overview/recovery 只能讀取已持久化 ProjectID 或比對使用者明確提供的
`--project`，不得以 inspector 當下 cwd、repo display name 或 workspace basename 重算。
selected run 的 canonical facts 沒有 ProjectID 時，run/task overview 可顯示 unknown；context/
promotion query需要 ProjectID 時回 `project_scope_required`。

### 7.4 Scope-aware 新 recovery 入口

```bash
# 新增 façade；--run/--branch/--attempt 綁定來源，並由 adapter 驗證
hufu session reconcile --workspace /repo/workspace/hufu-dev \
  --run run-42 --branch branch-main --task task-7 --attempt 2
```

既有 service 只能對 active task 操作時：adapter 必須確認指定 run/branch/attempt 就是該 active target；否則拒絕，不可自行 checkout 或改用另一 task。**不因新增 selector 就宣稱既有 service 支援 arbitrary historical mutation。**

Legacy `resume/retry/reconcile` 保持原語法，但 new UI 不生成缺少 scope 或會靠猜測 target 的 mutation command。所有 recovery 一樣通過現有 policy。

### 7.5 遷移不搬資料

本計畫預設**不 rename workspace、不搬 SQLite、不重寫 ProjectID、不合併 session**。新舊入口可以落在同一既有 exact workspace。CLI/手冊提供語法對照而不是資料搬遷腳本。

若另有真實資料 schema migration 需求，拆成獨立 ADR 與備份／dry-run／rollback，不綁進 UI release。UI 的「最近使用」或「當前選擇」只能是 local preference，不能成為新的 identity authority。

---

<a id="section-8"></a>

## 8. CLI 資訊架構與 Progressive Disclosure

### 8.1 第一階段不做大規模改名

預設 help 以使用者任務分組；沿用 Cobra command groups、自訂 help rendering 與既有 `help-flags`。[S02] [E01]

```text
執行任務           run     chat
建立與檢查團隊     team    doctor
查看與恢復進度     session inspect
管理經驗與技能     context skill
環境與偏好         config  models

進階工具：audit / decision / improve / eval / debug / terminal / migrate
全部命令：hufu help --all
常用例子：hufu examples
旗標分類：hufu help-flags <group>
```

進階工具不刪除、不從 completion 消失；常用 `models` 不需要為湊 namespace 數量硬塞進 config。`help --all` 是新增 help presentation，不藉由大批設定 `Hidden=true` 破壞 tooling discovery。

### 8.2 Canonical syntax 與 compatibility façade

| 目標入口 | Legacy 行為 |
|---|---|
| `hufu run --team NAME "TASK"` | root execution 與 `@team` shorthand 保留 |
| `hufu team create NAME` | `init` 保留 |
| `hufu team list [NAME]` | 現有 `list/ls/teams` 保留 |
| `hufu session status` | `status` 保留 |
| `hufu session resume` | `resume` 保留 |
| `hufu session retry/reconcile` | 舊頂層入口保留 |
| `hufu inspect overview` | 新增，不取代既有 run/task/evidence/context/trace/replay/storage |

先改文件與 onboarding 推薦語法；是否隱藏 legacy command 要在 compatibility evidence 與 release policy 完成後另行決定。**不承諾 1～2 個 release 後一定移除。**

新 `session` façades使用 `--workspace` exact semantics。`status` 接受 optional
`--team/--run/--branch`；`resume` 要求可唯一驗證的 team/run/branch；`retry/reconcile` 另要求
task，attempt在多 attempt時必填。明確 selector與 persisted active binding不符時回
`workspace_scope_conflict` 或 `historical_mutation_unsupported`，不 checkout、不改選其他 target。
`--team` 是新 façade名稱；legacy頂層 `--agent-team` 保留。兩者若同時出現套用 §8.4衝突規則。

新 commands 各自有 command factory 與 option struct，委派共同 application operation。不能把同一 `*cobra.Command` 掛到兩個 parent，也不能靠全域 `opts` 被上一個命令留下的值才能運作。

### 8.3 Flag 分組與統一說明

| Group | 核心內容 |
|---|---|
| core | team、worker model、coordinator model、workspace、workspace-root |
| execution | route、plan、timeout、limits、dry-run、typed input |
| safety | no-net、force-mcp、allow-path、unattended |
| learning | worker-memory / memory-learning 的設定入口解釋、memory embedding flag |
| output | output、event-format、quiet、verbose、report |
| display | tui、display-mode、theme、display-preset、no-color、no-spinner |
| advanced | compaction、debugging、template variables、provider-specific controls |

一份 command/flag metadata registry 驅動 help、examples、completion 與文件生成；禁止再手抄一份容易失真的 flags table。

所有 required flags、危險預設、scope 語意必須出現在相關 command 的一般 help。可以折疊不常用 knobs，不能隱藏安全資訊。

### 8.4 語法與 non-interactive contract

- `--team` 與 `--agent-team` 同時給相同值可接受；不同值報錯。與 `--default` 互斥。
- 新 `run --team A` 中若 prompt含可解析為另一 team B 的 `@B` switch，在任何 workspace/provider
  action前回 `team_selector_conflict`；同 team的 `@agent` direct invocation維持可用。Multi-team
  execution 不帶 `--team/--default`，使用現有 explicit `@team` segments，並只接受
  `--workspace-root` 或既有 default root；與 exact `--workspace` 同時使用時回 usage error。
- 支援 `--` 分隔 option 與以 `-` 開頭的 prompt；新 canonical examples 全部 flags 放在 prompt 前。
- `--input` 與 `--var`、`--profile` 與 `--execution-profile/--decision-profile` 在 help 中清楚區別。
- `--help` 不呼叫模型、provider、MCP，也不建立 workspace；命令缺參數時 non-TTY 不進 wizard。
- 補齊 Bash/Zsh/Fish/PowerShell/Nushell 中目前已支援的 completion 路徑；新增功能不足時保留既有行為，不宣稱所有 shell 都有同等 quoting 能力。

### 8.5 Completion 的資料限制

新增 completion：team、profile、workspace 下的 run/task/context/proposal IDs。只做有界、scope-authorized 的本地 metadata 查詢；不啟動 model server、不 flush outbox、不暗加 `--all-agents`。

Task completion 必須基於指定 run/branch；proposal completion 基於 project/team。候選描述不包含 raw prompt、memory、credentials。資料庫 absent/locked/unknown version 時回空候選與合適 directive，不在 completion 過程修復。

每次動態 completion 最多回100筆、以 canonical ID排序，query context deadline為150ms；超時、
missing/locked/unsupported store回空 slice + `ShellCompDirectiveNoFileComp`，不印 diagnostic到 stdout。
明確 flags互相衝突時才回 `ShellCompDirectiveError`。Task缺少 run/branch、proposal缺少 project/team
時回空候選，不跨 scope搜尋。Descriptions只允許 bounded status/type，不含 prompt/content/path。

---

<a id="section-9"></a>

## 9. Onboarding：Team Check、Wizard 與 Model Resolution

### 9.1 `hufu team check`

新增推薦入口：

```bash
hufu team check hufu-dev
hufu team check hufu-dev --online
hufu team check hufu-dev --output json
```

預設為 static / no-model / no-network / no-workspace-write；組合既有 compiler、validate、lint 與不帶副作用的 prerequisites checks。**不是把五個現有 CLI 依序 subprocess 執行。**

| 檢查層 | 預設 static | 顯式 probe |
|---|---|---|
| schema、references、tool policy | 執行 | 不變 |
| requested/effective model target、role compatibility | 本地可判斷者執行 | 不變 |
| required env 名稱、path 存在性／權限 metadata | 檢查，不輸出值 | 不變 |
| network reachability / remote model metadata | skipped | `--online`：沿用 provider introspection/model-list endpoint；每 provider 5s、整體15s timeout，不做 inference |
| external backend executable | 只用 `exec.LookPath`/file metadata，不啟程序 | 不變；v1不啟動 app-server或握手 |
| MCP startup / verifier shell | 不執行 | 不變；不在 team check v1 scope，使用現有 doctor或各自明確 operation |
| context/learning/storage | 沒有 workspace 時 not-applicable；已有資料只經 read-only query | 仍不能 migrate、checkpoint 或 rebuild |

每項狀態為 `passed / warning / failed / skipped / unknown / not_applicable`。總結：

```text
Static readiness: PASSED
Online readiness: NOT CHECKED
Learning readiness: NOT APPLICABLE — workspace 尚未建立
Next: hufu team check hufu-dev --online
```

不能顯示「Ready: YES」卻把 provider readiness 跳過。Static passed 代表範圍內檢查通過，不保證任務成功。

JSON v1 固定為：

```go
type TeamCheckDocument struct {
    SchemaVersion     int                 `json:"schema_version"` // 1
    Kind              string              `json:"kind"`           // team_check
    Team              string              `json:"team"`
    Operation         OperationView       `json:"operation"`
    StaticReadiness   string              `json:"static_readiness"`
    OnlineReadiness   string              `json:"online_readiness"`
    LearningReadiness string              `json:"learning_readiness"`
    Checks            []TeamCheckItem     `json:"checks"`
    NextAction        *ActionSuggestion   `json:"next_action"`
    Error             *OperatorErrorView  `json:"error"`
}

type TeamCheckItem struct {
    ID         string `json:"id"`
    Category   string `json:"category"`
    Status     string `json:"status"`
    Required   bool   `json:"required"`
    ReasonCode string `json:"reason_code"`
    Message    string `json:"message"`
}
```

Checks 依 `category,id` 排序且必為陣列。未要求 `--online` 時 online readiness 固定為
`not_checked`，對應 checks 為 `skipped`。Required static/online check 的 `failed` 或 `unknown`
使 readiness=`failed`；warning 不使 command 失敗。Exit 0 表示所有本次要求的 required checks
通過（可有 warnings），exit 1 表示 readiness failed，exit 2 表示 usage/command operation error。
JSON mode 在三種 exit 都輸出一份 document 到 stdout且 stderr 不重複錯誤；text mode 的錯誤只印
stderr。不得輸出 env value、credential、完整 remote response 或任意 verifier內容。

### 9.2 `team show/explain/check/validate/lint` 不互相取代

| 使用者想知道 | 對應入口 |
|---|---|
| 我寫了哪些設定？ | show |
| 最後生效的是什麼、從哪裡來？ | explain |
| 這個 team 可不可以開始、下一步是什麼？ | check |
| 契約是否成立，供 CI 使用？ | validate |
| 有哪些 authoring drift/品質問題？ | lint |

`team check` 預設先顯示 blockers 與下一步，詳細 provenance 透過 explain 展開。Lint finding 不得被合併成一條泛稱「配置錯誤」。

### 9.3 `team create --wizard`

只做顯式可選的 interactive authoring；普通 `team create` 保持 deterministic scripting。

流程：`目的/preset → worker backend/model → coordinator LLM → 權限/路徑 → verification → memory/learning → preview diff → static check → write`。

必須：

- 只問會改變結果的選項；其餘使用明示的 defaults，可返回前一步。
- 預設保守權限；不要預選 `tools: all`、全磁碟 allow、跳過 validation、active learning。
- 以現有 preset/compiler schema 產生設定；`observe` 是學習流程的建議起點，不自動開 active。
- Preset 名稱「review/readonly」不是 OS sandbox 保證；`bash` 可有寫入／網路副作用，必須據實顯示。
- 不覆寫現有 team；有衝突時 preview 具體檔案，不提供模糊「全部覆蓋」預設。
- Wizard 不自動執行剛寫好的 team、不呼叫模型判斷需求、不購買／下載模型。
- non-TTY 明確報 interactive required，附等價 non-interactive syntax。
- 完成後只給一個主要建議 `team check` 或 `run`；不塞一長串命令。

### 9.4 `-m` UX：顯示 role target，不重做 routing

現況已是 worker-only override。[S05] 新 UI 顯示：

```text
Role           Requested              Effective backend/model       Source
worker         codex/<model>          codex / <model>               CLI -m
coordinator    <configured>           <LLM backend> / <model>       team.yaml
sidecar        <configured/default>   <resolved target>             config/default
judge          <configured/default>   <resolved target>             existing fallback
```

規範：

1. selector parsing、backend kind、fallback、durable binding 全部由既有 execution registry/resolver 提供。
2. 不自行寫 `strings.HasPrefix("codex/")` 的 UI routing；model 名稱中包含 slash 也不能被 presentation 層再次切割。
3. unknown backend / role mismatch / backend unavailable 要分成不同 error kinds；absence of remote check 不表示 unavailable。
4. 使用 agent backend 作 worker 不得污染 coordinator 或 auxiliary roles；缺 coordinator LLM 時給出針對該欄位的修正。
5. 過去 attempt 顯示其持久化 target；目前 config 的 worker model 只標為「下一次可用設定」，不能覆寫歷史。
6. active attempt 不允許 UI 隨時改 model。新任務／新 attempt 的變更遵守既有 binding/replay 規則。
7. 不新增模型下載、server load/unload、LRU 或自動 fallback 到未授權 endpoint 的 UX 捷徑。

---

<a id="section-10"></a>

## 10. Output／Errors／Exit Codes

### 10.1 統一 flags，不破壞 payload

新增 `--output text|json` 到適合的 query/mutation commands，作為與 `--format json` / `--json` 等價的 renderer selection。

| 組合 | 新行為 |
|---|---|
| 只有 `--json` | 保留原 payload、exit 與語意 |
| 新 `--output json` | 與同 command 的舊 JSON rendering 等價；不自動更換 envelope |
| `--json --output json` | 接受等價設定 |
| `--json --output text` | 在任何 action 前報互斥選項衝突 |
| `--format mermaid` 的 skill graph | 保留；不可因全域只允許 text/json 而破壞 |
| command 不支援 YAML | 清楚列出 supported formats；不做看似 YAML 的文字包裝 |

新 `inspect overview` 使用 inspector envelope；其他新命令使用已確認的共用 envelope 或自己的 versioned kind。所有兼容性改動需 golden/schema test。

### 10.2 `inspect overview` v1 outcome contract

既有 inspector `Envelope` 加入 `KindOverview`，其 `data` 固定為下列型別；其他 inspector
kind 不新增欄位、不改 JSON：

```go
type OverviewData struct {
    Operation OperationView       `json:"operation"`
    Snapshot  *OperatorSnapshot   `json:"snapshot"`
    Error     *OperatorErrorView  `json:"error"`
    Warnings  []DiagnosticView    `json:"warnings"`
}

type OperationView struct {
    Status string `json:"status"` // succeeded|failed
}

type OperatorErrorView struct {
    Kind    string `json:"kind"`    // usage|scope|configuration|policy|integrity|runtime|stale|cancelled
    Code    string `json:"code"`
    Message string `json:"message"` // fixed template + redacted bounded metadata
}
```

成功：`operation.status=succeeded`、snapshot 非 null、error=null、process exit 0。被查詢 run 的
outcome 放在 `snapshot.outcome.run_outcome`，不影響 query success。失敗：
`operation.status=failed`、snapshot=null、error 非 null；`--output json` 仍在 stdout 輸出一個
合法 inspector envelope，stderr 不重複印錯誤，process exit 使用既有 inspector mapping：
integrity=1，其餘 domain/usage=2。Text mode 失敗只在 stderr 印一次 §10.4 template，stdout 為空。

JSON 的 `warnings` 必為陣列。Message 不作 automation key；consumer 使用 error kind/code。
新 `team check` 的 JSON contract見 §9.1；新 `run` 與 session aliases 保留其對應 legacy
operation 的既有 JSON shape，不能改成此 envelope。

### 10.3 stdout / stderr contract

- stdout：所選格式的主要結果。`--output json` 只能有一個合法 document，不混 spinner、ANSI、提示或 deprecation banner。
- stderr：人類 progress/diagnostics，遵守 quiet 與 display-mode；已有 `--event-format jsonl` contract 不改 channel 或 event schema。
- JSONL event stream 與 JSON final result 分開描述。不得宣稱 `--output json` 就讓兩個 file descriptors 都成為一個 JSON document。
- 新需要全結構化 stream 的需求先記錄既有使用方式，再另提 schema；不把人類訊息混進 machine event stream。
- Pipe / non-TTY 不顯示 wizard、spinner、cursor movement、alternate screen；不得往 stdout 探測終端背景。
- `--quiet` 抑制例行進度，不吞失敗原因；錯誤只報一次。`--verbose` 不是無條件輸出 secrets 的許可。

### 10.4 Error template

```text
Error [workspace_scope_conflict]
State: 操作未開始；沒有修改資料。
What: 指定 team 與 workspace 的已知 binding 不一致。
Target: /repo/workspace/hufu-dev
Next: 以 inspect overview 檢查該 workspace，或選擇正確的 exact path。
```

`沒有修改資料` 只在已證明操作未開始時顯示。部分副作用已發生時必須改成「已完成 X；Y 未知」，不能顯示籠統的 failed 然後推薦重試整段。

錯誤分類至少包含 usage、scope、configuration、provider、policy、integrity、runtime、stale、cancelled；同時保留既有細分 failure class 與原因。Provider 不可用不等於配置錯，policy deny 不等於 transient failure。

### 10.5 Exit code 遷移政策

**不在本計畫直接把全 CLI 改成新 0～4 對照。** Phase 0 建立現有 code table，包含 inspect/lint/run/recovery。

第一階段統一的是可機器讀 `error.kind`；既有 code 維持原樣。新 façade 與對應舊 command 的 outcome 相同時，保持相同 process exit。查詢成功但 run 失敗仍遵守 query 的 exit semantics。[S07]

需要新增與統一 numeric taxonomy 時，另附 compatibility ADR、shell/systemd/pueue/CI fixtures 與 opt-in migration。`SIGINT`、取消、broken pipe、timeout 必須保留既有信號／平台語意，不在 presentation 層泛化成 code 1。

---

<a id="section-11"></a>

## 11. TUI：從任務追蹤到操作面板

### 11.1 漸進改造而非重寫

現有 tasks columns、detail、memory/activity overlays、search、result、quit confirmation 與 PTY 關聯要保留。[S08] 優先加 summary strip 與 shared snapshot，再逐步整合 navigation。

預設顯示：

```text
┌ Team / Exact workspace / Run / Branch ──────────────────┐
│ State: VERIFYING       Evidence: valid       Age: 2 s   │
│ What: coder 已完成；等待 verifier 的客觀結果。           │
│ Next: 等待驗證，不需要操作。                             │
├─────────────────────────────────────────────────────────┤
│ Overview | Tasks | Evidence | Context | Learning         │
├───────────────────┬─────────────────────────────────────┤
│ 任務與 blockers   │ 目前選擇的 detail                    │
│ 保持穩定排序      │ status / receipt / verification      │
│ 顯示實際依賴     │ 展開 logs，模型文字標為 claim         │
├───────────────────┴─────────────────────────────────────┤
│ Enter 詳細  / 搜尋  Esc 返回  ? 按鍵說明                 │
└─────────────────────────────────────────────────────────┘
```

示意文字，不要求像素級照抄；資訊順序與語意是驗收點。不能為了多個 tabs 在窄螢幕壓縮成不可讀的九個欄位。

### 11.2 View contract

| View | 回答的問題 | 必要保護 |
|---|---|---|
| Overview | 在哪裡、是否需介入、下一步 | 不自行宣告 outcome；顯示 freshness |
| Tasks | 哪些 task 在跑、卡在哪個依賴 | DAG 是 runtime projection；不從 prose 猜 dependencies |
| Evidence | 哪些結果被客觀驗證 | 區分 receipt、verification、acceptance、claim |
| Context | 為什麼這筆內容進 prompt | item ID、source、scope、selection reason、budget；原文另行授權 |
| Learning | 有沒有收集、使用、有效提升或候選 | 觀察／採用／信用分離；不顯示虛構「智慧提升 %」 |
| Promotion detail | 草稿做什麼改變、可否套用 | 對應既有 lifecycle 與 explicit review gate |
| Logs/activity | 診斷需要的細節 | bounded、sanitized、明示可能敏感，不預設全文 |

### 11.3 互動行為

- `Enter` 進入 detail，不以 Enter 無條件執行 Next Action；`Esc` 返回上一個 view，保留焦點、scroll 與選擇。
- `/` 保持搜尋語意；新增 command palette 必須選擇不衝突按鍵，先做 keymap inventory，不直接覆蓋現有 `m/M/a`。[S11]
- 後台新 event 不可搶走使用者正在閱讀的 task/detail 焦點；改成未讀/狀態 badge。
- 重要 blockers 出現在摘要，但不強制彈 modal 打斷輸入，除非 runtime 真正進入 interaction gate。
- 首次點開危險操作顯示 preview／風險／scope，不自動 approve；重複按鍵不能跨越兩層 confirmation。
- spinner 只代表有可觀察的 activity，不代表 provider healthy；斷線明示 unknown/stale。
- 僅等待沒有新文字時，要顯示最近 durable progress 與 deadline/budget；不可杜撰 ETA。

### 11.4 Live owner、passive viewer、quit 必須分清

| TUI 模式 | 允許行為 |
|---|---|
| `run --tui` 的 owner view | 查詢、既有 prompt/approval/收尾操作；mutation 必須重驗 scope/policy |
| 日後新增 read-only watch/view | query、copy、filter、theme；不得偷偷啟動 coordinator 或 recovery |
| 已完成 run 的歷史 view | inspection；變更必須另外進入明確 operation，不自動切回 active run |

`q`、Ctrl+C、Esc 的語意在 footer/help 明示：離開 view、要求 graceful wrap-up、force termination 是三種不同操作。不能讓「關閉面板」意外殺 worker，也不能把同程序 owner view 關掉後宣稱任務一定背景繼續。只有真實 runtime 支援 detach 才提供 detach。

PTY takeover 保持既有 exclusive ownership；operator shortcuts 在 PTY input focus 時不能攔截並觸發 mutation。

### 11.5 適應終端限制

測試 120×40、80×24、60×20 與極小 viewport：大型可雙欄；80×24 使用單頁/detail；更小時縮成 State/What/Next 並保留可展開路徑。

CJK 寬字、combining characters、emoji/ASCII fallback、長 path 都以 terminal cell width 處理，不用 byte length。只以顏色區分 status 不合格。截斷 path 必須可展開／複製完整值。

---

<a id="section-12"></a>

## 12. Learning／Skill／Promotion UX

### 12.1 一個 learning view，沿用原 namespace

不新增平行 `memory`、`knowledge`、`learning` canonical store/CLI。UI 可聚合 `context`、`skill`、`improve` 的 read-only metrics 與 eligible actions；各 subsystem ownership 保持不變。

畫面至少分成：

```text
Worker memory      shared/private; session/persistent
Recall             selected/injected；缺資料時 unknown
Usage              applied / consulted / rejected
Outcome            verified support / causal failures / independent tasks
Learning policy    requested mode / effective mode / policy version
Candidates         eligible / proposed / approved / stale / applied
```

`--memory` 的 embedding/vector 設定、worker private memory、memory-learning、skill pattern detection 與 LTM promotion 不是一個 enable toggle。UI 必須顯示各自狀態與 prerequisite，不要用一顆「自我學習已開啟」掩蓋差異。

### 12.2 空狀態與不足條件

| 情境 | 顯示與下一步 |
|---|---|
| 新 workspace 沒有 memory | 「尚無可回憶資料」；正常執行適當任務，不報故障 |
| 有 exposures、無 applied | 「內容曾被注入，但無已回報採用」；檢查 attribution，不自動加信用 |
| 有 applied、無 objective support | 「已有使用，但驗證證據不足」；檢查 verification，不直接 promote |
| policy mode off | 呈現 off 的作用與明確配置入口；不自動切 observe |
| observe/shadow | 顯示是否只是收集／模擬排序，不能說已改變結果 |
| active requested 但 effective fallback | 顯示 requested/effective 差異與原因，不隱藏 degraded ranking |
| sidecar 不可用而未產生 skill candidates | 顯示 candidate qualification unavailable；不宣稱完全沒有重複操作 |
| eligible 為零 | 合法空結果，表示條件不足；不啟動昂貴 drafting 來填滿畫面 |
| statistics query 失敗 | unknown/error，不顯示 0 |

不新增一次按鈕直接由 observe 跳 active。可引導檢視 shadow/benchmark evidence；實際 policy transition 使用既有配置及實驗／review gate，不能由 UX 修改 reducer 或 thresholds。

### 12.3 `context promotion review` 新 façade

```bash
hufu context promotion review --workspace /repo/workspace/hufu-dev \
  --project <canonical-project-id> --team hufu-dev
```

一次選定 scope，從 canonical data 取得待 review proposals，不要求每一步重打三個 flags。能從 verified binding 唯一取得 project/team 時可預填，但仍顯示；無法取得時詢問或報錯，不能用 repo display name 填 ProjectID。

Review flow：

```text
read-only list
  → select proposal
  → authorized redacted source summary + evidence + target diff
  → edit / reject / approve / skip
  → approve 成功後顯示「尚未套用」
  → 另外選 Apply
  → re-read source/target/draft revision + existing preflight
  → explicit confirmation
  → existing Apply service
  → 結果、modified files、後續 validate/review 建議
```

安全規範：

- proposal list/default detail 不輸出 draft/source content；顯示內容是 explicit authorized action。
- `openPromotion` 目前含 writable open/outbox flush；新面板不得直接呼叫它來 refresh。[S10]
- HF-UX-052 在 `context.ReadOnlyRepository` 增加 `GetPromotion`、`ListPromotions` 兩個只讀
  method，並讓 `OpenSQLiteReadOnly` 的既有 query-only handle實作；promotion review list/show
  直接使用該 interface。不得把 writable `promotion.Service` 或 `openPromotion` 傳入 refresh。
  補送 outbox 保留在明確 write/repair lifecycle。
- proposal 在另一程序被 edit/apply/reject，review 畫面必須刷新並撤銷舊 action；不自動追套新 draft。
- 編輯器僅在使用者選 Edit 時啟動；editor invocation 使用可信配置與 argv，返回時重新驗證檔案、secret redaction、draft schema。
- generic `--auto-approve`、`--unattended` 或流程中的預設 Yes 不算人類 review。non-TTY 不进入 editor/review loop，不自動 approve。
- 不新增 batch approve/apply；首先確保單筆流程可追溯、可取消、可防重。
- `team-policy` 是現有 coordinator/orchestrator body target，不等於所有 worker 的 enforced policy；必須在 diff 頁明示。
- `skill review/promote` 的 draft publication 與 LTM promotion proposal 是不同 lifecycle；UI 共用閱讀方式，但不能把兩種 ID/資格門檻互換。
- 需要 rollback 時顯示具體 target change 與既有 Git/manual restore 流程；不宣稱存在尚未驗證的 automatic rollback command。


---

<a id="section-13"></a>

## 13. 電子紙、動態主題與可及性

### 13.1 Display preferences 與 runtime policy 分開

新增介面固定為：

```bash
hufu run --team hufu-dev --tui --theme light --display-preset epaper -- "檢查任務"
hufu run --team hufu-dev --display-mode plain --no-color -- "檢查任務"
```

| 設定 | 目標語意 |
|---|---|
| `--theme auto|light|dark|mono` | 色彩／對比 token 選擇；不更動 runtime |
| `--display-preset default|epaper` | refresh、動畫、裝飾密度與字型效果策略 |
| 既有 `--display-mode auto|terminal|plain` | terminal rendering transport；plain 無 cursor control |
| 既有 `--no-color` / `NO_COLOR` | 禁用 ANSI color，不能當成「禁用一切動畫」的替代 |
| 既有 `--no-spinner` / `NO_SPINNER` | spinner 行為；沿用現有預設和明確 override 的語意 |

主題與電子紙 preset 可獨立組合，避免把「白底」誤當作「低刷新」。Preferences 放 user config 的 presentation 區域；不寫入 team runtime policy、不造成 session binding 改變。

### 13.2 Theme implementation contract

- 以語意 token 表達 background/foreground/muted/focus/success/warning/error/border，不散布新固定色碼。
- TUI model / renderer 持有 instance-scoped palette；不能 hot-swap package global styles 造成 races 或其他 model 被改。
- 現有 CLI banner、表格、errors、TUI overlays、ask_user、result、search highlight 全部用相同 theme resolver；不只修改主畫面。
- runtime 中可從 display menu/command palette 切換 light/dark/mono/auto，**不重啟 agent**，不遺失 scroll、選擇、輸入、pending approval 或 PTY ownership。
- Auto detect 不可阻塞啟動；terminal 不支援或經 SSH/tmux 無法判斷時，使用可預期 fallback 並允許手動切換。任何 terminal probe 都只能送往合適的 TTY，不污染 machine stdout。
- 資訊不能只靠紅/綠、faint 或灰階差異；同時顯示文字 state、prefix/符號與 focus border。
- Custom RGB palette 的一般文字對比目標至少 4.5:1；terminal palette 因使用者配置不同不能假設達標，需有 mono/plain fallback。

### 13.3 `NO_COLOR` 與 display precedence

依 NO_COLOR 的公開約定，非空值應禁用預設 ANSI color；它不要求禁用 bold/underline，也不等於禁用 cursor control。[E02]

本計畫要求：machine/plain mode 永遠不輸出 terminal control；明確 `--no-color` 不能因 theme 切換被打開。當 app 支援 explicit color preference 時，必須依文件定義與測試其 CLI/config/env precedence；**切換 light/dark 只表示選 palette，不等同使用者允許忽略 NO_COLOR**。

v1 precedence 固定如下：

1. `display-mode=plain` 或 machine stdout：禁用 ANSI color、cursor control、alternate screen、
   spinner，theme/preset只影響不輸出的 semantic token。
2. 非空 `NO_COLOR` 或 `--no-color`：禁用 color；`--theme` 不得重新啟用。兩者同等效果。
3. 非空 `NO_SPINNER` 或 `--no-spinner`：禁用 spinner；epaper亦強制禁用，沒有正向 flag覆蓋。
4. Theme來源：explicit `--theme` > user config presentation.theme > `auto`。
5. Preset來源：explicit `--display-preset` > user config presentation.display-preset > `default`。
6. `auto` v1 不發 terminal query：TTY下只讀 `COLORFGBG` 的最後一個 numeric component，0–6
   選 dark、7–15選 light；缺少/invalid時使用 dark（現有 palette相容 fallback）。non-TTY/plain
   不做偵測。Runtime manual theme toggle只改 model palette，不寫 team/session/runtime policy。

### 13.4 電子紙 rendering policy

| 項目 | epaper 要求 |
|---|---|
| 常態 refresh | dirty/event-driven；背景更新合併，目標上限 1 次/秒 |
| idle | 沒有新資訊時 0 repaint；不用時計每秒刷新來表示「還活著」 |
| token stream | 不逐 token 重繪全頁；聚合到 detail/log buffer |
| animation | spinner、blink、pulse、漸變／閃爍移除 |
| user input | 按鍵、焦點與明確主題切換立即回饋，不受背景 1 Hz 節流阻塞 |
| blocking state | approval/input/error/terminal outcome 要及時 flush；重要變更不能被一般 refresh 合併遺失 |
| full redraw | 只在必要時，如 resize/theme switch；穩態採最少變動；遵守所用 framework 能力 |
| freshness | 顯示固定「資料截至」時間或 stale badge，不為秒數倒數持續刷新 |
| manual refresh | 提供明確按鍵；不重跑 provider 或 verification |

上述 1 Hz 是 v1 固定設定與驗收目標，不是現有已量測的能力。User input/重要 gate 可例外刷新，但不得以此理由讓普通 stream 繞過節流。

### 13.5 Keyboard、copy、terminal safety

完整 keyboard-only journey 必須可完成。非必要 modal 不搶焦點；退出 modal 回到原位置。原文/detail/copy 的 redaction 與 access check 一致，不可畫面遮蔽而 clipboard 洩漏。

所有不可信顯示文字去除危險 ANSI/OSC/DCS/control sequences；保留可讀換行與 tab 的安全形式。OSC52 copy 只能由明確 copy action 觸發，不能由 model output 或 proposal 內容觸發。

---

<a id="section-14"></a>

## 14. 端到端使用者旅程與驗收輸出

以下流程是目標 UX；安裝與 provider provisioning 不由 wizard 偷偷完成。

### Journey A：第一次使用，到第一個可驗證結果

```text
team create / optional wizard
 → preview definition
 → team check（static）
 → optional online check
 → run
 → State/What/Next 可見
 → objective verification / acceptance
 → final result + evidence + none/review-result
```

必要：不需理解 `context.sqlite`、EventStore、reducer 才能完成。第一次模型設定失敗要指出是哪個 role，而不是只說 model missing。

### Journey B：已有 workspace 的持續開發

```text
session status / inspect overview
 → 確認 exact workspace、run、branch
 → 依建議 resume 或新 task
 → 保留已完成任務與 durable bindings
```

新任務不等於 `--new`；fresh session 不等於刪 workspace；session branch 不等於 Git branch。這些差異必須出現在 relevant help 與 destructive confirmation。

### Journey C：Crash 後不重複副作用

```text
inspect overview
 → unknown external effect 被明示
 → inspect task/evidence
 → policy 可用才顯示 reconcile
 → reconcile 確認後才能提供合適 retry/resume
```

成功標準：使用者不必自行 grep 多份 JSONL 才知道先做什麼；UI 不給危險捷徑。

### Journey D：建立可治理的學習閉環

```text
memory policy observe
 → 看得見 exposures / applied / objective support
 → shadow evidence / benchmark
 → 維護者決定 active
 → eligible proposal
 → review diff / approve / separately apply
```

成功標準：能分辨「記住」、「用過」、「證實有效」、「已成正式 skill/policy」。不以動畫、綠色 badge 或自評分數替代證據。

### Journey E：電子紙與長時間 CLI

```text
啟動 light + epaper
 → 閱讀長任務 detail
 → 新 event 不搶焦點、不閃整屏
 → 隨時切 dark/mono
 → 保留輸入、task 狀態與待審批
```

成功標準：主題切換無 runtime 副作用；背景 token flood 不造成畫面高頻刷新。

### Journey F：自動化／CI／pueue

```text
non-TTY invocation
 → 明確 selectors
 → stable machine output
 → existing exit semantics
 → 無提示阻塞、無終端控制碼
```

遇到 scope ambiguity，明確失敗而不是互動等待。舊 script 與新 syntax 的 equivalent operation 產生相同 runtime effects。

---

<a id="section-15"></a>

## 15. 實作架構與程式修改邊界

### 15.1 Reuse-first ownership

| 責任 | 優先使用的現有位置 | 可新增的最小部分 |
|---|---|---|
| command/flag registration、help | `cmd/hufu/root.go`、`helpcmd.go` | metadata、command factories、thin aliases |
| team authoring | `teamcreatecmd.go`、`teamcmd.go`、`teamlintcmd.go`、`teamexplaincmd.go` | `team check` composition、optional wizard |
| workspace resolution | `team_loader.go`、`recoverycmd.go`、`resumecmd.go` | `internal/operator` path resolver + command-specific legacy/exact adapters |
| execution target | `model_overrides.go`、existing execution registry/resolution | structured explanation，不新增 resolver |
| query/overview | `internal/inspect`、`inspectcmd.go` | binding query + overview assembly；view types在 `internal/operator` |
| command intent/recovery | 現有 team recovery/policy services | exact-scope guard + action adapter |
| context/learning/promotion | `internal/context`、`internal/promotion`、相關 `context_*cmd.go` | read-only queries、interactive review façade |
| TUI | `internal/tui` | summary strip、navigation、palette、refresh policy |
| output/error | 既有 formatter/error/exit code paths，Phase 0 盤點 | format resolver、typed diagnostic presenter |
| documentation | `docs/README.md` 指定的正式 docs hierarchy | 使用者旅程指南、migration table、生成 help reference |

上述 ownership 已由 §5.1 決定。若實作發現 package cycle，先把 canonical input DTO 縮小，
不得把 storage query 搬進 `internal/operator` 或另建平行 package。

`internal/tui/tui.go` 目前已達 3021 行，超過 CLAUDE.md 的 800 行/檔案上限。Phase 4 開始前
必須先完成 HF-UX-039（檔案分解，零行為變更），新增的 summary strip、theme resolver、
snapshot rendering 一律落在分解後的新檔案，不得再對 `tui.go` 淨增行數。

### 15.2 Read/query 與 mutation 的硬分界

Query adapter 不依賴「建立 coordinator」來讀資料。不调用 `loadTeam...` 的 runtime startup path 只為取得畫面。Read-only interface 與 mutation interface 必須能用測試 double 明確檢查：讀取時任何 write/provider callback 被呼叫就失敗。

若現有 service 把 read 與 flush/migration 綁在一起，先抽開 query 接縫再接 UI；這是必要的小型 refactor，不可用 UI 的命令名稱假定安全。

### 15.3 TUI concurrency 與更新

- 新增 `OperatorSnapshotMsg{Generation uint64, Snapshot operator.OperatorSnapshot}`。Msg 建立前
  deep-copy 所有 slice/string；TUI 不保留 `TodoItem`、`RunResult` 或 repository object 指標。
- 每個 selected `workspace/run/branch` scope 的 generation 單調遞增。Update 只接受等於目前
  requested generation 的回覆；切換 scope 先增加 generation、cancel 舊 context，再啟動 query。
- Owner TUI 的 snapshot 只能在對應 canonical event append/reducer完成後發布；尚未落盤的 token/
  status text 可留在 logs/activity，但不得改變 activity/outcome/evidence summary。若 durable append
  失敗，顯示 `integrity=degraded`/`freshness.live_state=connected` 與固定 reason，不升格 live prose。
- Passive/history view 只呼叫 `internal/inspect`。Owner view 與 CLI 對相同 event anchor 呼叫同一
  `internal/operator` derivation；parity test 比 semantic fields，不要求 `queried_at` 相同。
- theme、filter、selection 是 UI state，runtime progress 是 query/event projection，彼此分離。
- 可以合併 repaint notifications，不可丟掉 canonical events 或終端 outcome。
- bounded queues/log buffers，overflow 顯示 dropped-view-lines metadata；需要全文時走原始授權 log 檢視，不重跑 agent。
- Fake clock 驅動 freshness、throttle、timeout 測試，避免 sleep-based flaky tests。

### 15.4 Application operation 與 flags provenance

CLI alias 與新 command 不能只複製一份 `RunE`。提取最小 typed operations 並保留 explicitly-set flags 的 provenance，特別是 `--model`、profile merges、false/zero overrides。

Command factories 每次建立獨立 options；測試同一 process 連續建立／執行 command 時不得互相污染。不要順手重構整個 runtime 或全專案所有 global variables。

---

<a id="section-16"></a>

## 16. 分階段 Implementation Backlog

所有階段採小 PR；每個 PR 必須有對應 acceptance tests。Phase gate 控制 stable rollout，不把
沒有直接依賴的 work item強制串行。下表的明示 dependency 才是開工條件；同 phase 不代表同 PR。

### Phase 0 — Characterization baseline

| 工作 ID | 工作 | 交付物 | 完成條件 |
|---|---|---|---|
| HF-UX-000 | **完成：**以 HEAD `ba306437...` 核對 baseline、docs authority、主要 package 接縫 | 本文件 §1、§2、§5、§7 | ownership/schema/scope 決策已凍結；後續差異走 §1.3 |
| HF-UX-001A | **完成：**新增 commands、flags、JSON、exit、scope、TTY side-effect characterization | [Phase 0 baseline](../reference/operator-phase0-baseline.md)、contract inventory、golden fixtures | root/chat/resume/recovery/context/promotion/inspect/skill/team 已覆蓋；production diff 為零 |
| HF-UX-001B | **完成：**建立 deterministic journey fixtures | `cmd/hufu/testdata/operator/journeys.json` | completed/failed/verifying/interrupted/external-effect-unknown/ambiguous scope 各有 fixture |
| HF-UX-002 | **完成：**建立 usability measurement script | `docs/reference/operator-usability-corpus.tsv`、`scripts/operator-usability-measure.sh` | 六條 journey 的三問與 safety gate 有事先定義答案，可產生 baseline/candidate 評分表 |

**Gate P0（2026-09-14 已通過）：**HF-UX-001A/001B 通過；Linux amd64、Go 1.26.6 的
`go test ./...`、`go vet ./...`、`golangci-lint run` 與 `bin/check-docs` 成功。既有 docs lifecycle
metadata 缺漏只補標頭、不改技術內容。後續仍不能把既有失敗當本計畫新增 regression，也不能
刪測試讓基線變綠。

### Phase 1 — Scope 與 Read-only Operator Snapshot

以下 work item 都以 Gate P0 通過為共同前提，再套用表內的直接依賴。

| 工作 ID | 直接依賴 | 工作 | 主要接縫 | 完成條件 |
|---|---|---|---|---|
| HF-UX-010A | — | **完成：**`internal/operator.ResolveWorkspacePath` + legacy/exact/root modes | §7.2、CLI workspace loaders | pure path matrix通過；尚不切換 production callers |
| HF-UX-010B | 010A | **完成：**root/recovery/inspect adapters 使用 resolver | CLI workspace loaders | legacy path與 goldens不變；新 exact/root cases通過；不搬資料 |
| HF-UX-011A | — | **完成：**v1 immutable types、validation、hash、activity/attention pure mapping | `internal/operator` | §3.3/§5.2 fixtures 全過；unknown fail closed |
| HF-UX-011B | 010A/011A | **完成：**exact target binding/provenance | `internal/inspect` | active/explicit/single/ambiguous matrix；project ID 不重算；collision不誤 join |
| HF-UX-012A | 010B/011B | **完成：**`inspect overview` read-only endpoint與JSON/text renderer | `inspectcmd.go` | §5.4/§10.2；missing DB不建立；零 provider/tool/write；舊 inspector goldens不變 |
| HF-UX-013 | 012A | **完成：**bounded latest changes、role/learning unavailable adapters | overview assembly | v1完整 shape；最多3 changes；未知 counters為null |

**Gate P1（2026-09-14 已通過）：**read-only no-side-effect suite、scope selection/collision matrix、
legacy workspace characterization 與 inspector JSON/text contract tests 全過；Linux amd64、Go 1.26.6
的 `go test ./...`、`go vet ./...`、`golangci-lint run` 成功。Phase 1 只發布新的 read-only
`inspect overview`；role/learning 明確回報 unavailable，尚未加入 mutation recommendation。

### Phase 2 — Next Action 與統一操作摘要

| 工作 ID | 直接依賴 | 工作 | 完成條件 |
|---|---|---|---|
| HF-UX-020A | 011A/012A | pure action selector + v1 registry | §6.2 actions；unknown effect 不推薦 retry；未發布 façade blocked/empty argv |
| HF-UX-020B | 011B | existing recovery/policy read-only eligibility adapter | policy deny 不給繞過操作；沒有 capability 不虛構 probe |
| HF-UX-021 | 020A/020B | typed argv builder、Bash/PowerShell renderer、target revalidation | §6.1/§6.3；metacharacter/secret/ANSI tests 全過 |
| HF-UX-022 | 012A/020A/021 | CLI State/What/Next summary 與 typed diagnostics | start、block、interrupt、terminal、partial cases 一致 |

**Gate P2：**所有安全情境有 deterministic recommendation；不能生成不存在的 command/flag。未發布的 mutation façade 不提供可執行建議。

### Phase 3 — CLI Discovery、Canonical Facades、Team Check

各 work item 依賴如下；help/output/team check 不必等待 recovery mutation façade。

| 工作 ID | 依賴 | 工作 | 完成條件 |
|---|---|---|---|
| HF-UX-030 | 001A | help grouping / progressive disclosure / shared metadata | `help --all` 可見完整命令；安全 flags 不失蹤 |
| HF-UX-031A | 010B | `run` exact/root façade、team list alias | 舊 root/new run equivalent effects；新 -w 不 double-join；factory 無污染 |
| HF-UX-031B | 011B/020B/021 | `session status/resume/retry/reconcile` exact-scope façades | 指定 run/branch/task/attempt；只操作 active compatible target；舊入口不變 |
| HF-UX-023 | 031B | scope-safe mutation intents | stale target、double submit、historical selection安全拒絕或 idempotent |
| HF-UX-032 | 001A | common `--output` selection、alias conflict guard | 舊 JSON/exit golden 不變；Mermaid 等 domain format正常 |
| HF-UX-033 | 001A/020A | `team check` static composition、opt-in online checks | §9.1 schema/exit；static 零 provider/MCP/verifier/write |
| HF-UX-034 | 011A | model role target explanation | -m worker-only；歷史 target 不被新 config改寫 |

**Gate P3：**onboarding、日常執行、crash recovery 三條 CLI journeys 可完成。這是第一個完整核心交付，不必等全部 TUI 功能。

### Phase 4 — TUI Summary、Theme 與低刷新

以下工作可與 Phase 3 無直接依賴的部分並行。HF-UX-039 是本 phase 其餘工作項的共同前置：
`internal/tui/tui.go` 現況 3021 行，任何在其上疊加的新程式碼都會讓 CLAUDE.md 的 800 行/檔案
違規更嚴重，必須先分解。

| 工作 ID | 直接依賴 | 工作 | 完成條件 |
|---|---|---|---|
| HF-UX-039 | 001A | `internal/tui` 檔案分解：拆分 `tui.go`（現況 3021 行）為多個 <800 行檔案，純搬移不改行為 | 既有 TUI unit/integration/race tests 全過且零行為差異；拆分後每個檔案 <800 行；之後的 HF-UX-040/041/044 不得再對 `tui.go` 淨增行數 |
| HF-UX-040 | 012A/020A/022/039 | summary strip + stable task/detail navigation | 與 CLI snapshot 一致；新 event 不搶焦點 |
| HF-UX-041 | 001A/039 | instance-scoped semantic theme | live light/dark/mono 切換不重啟、不改 runtime、不遺失輸入 |
| HF-UX-042 | 041 | epaper renderer policy / bounded redraw | idle 0 repaint；背景 <=1Hz；input/gate 及時處理 |
| HF-UX-043 | 020B/040 | owner/passive/quit/PTY boundary | 不把 close view 當 kill；不把 owner退出當虛構 detach |
| HF-UX-044 | 040/041 | compact layout、CJK、keyboard/copy/escape safety | 各 terminal sizes 與 terminal injection fixtures 全過 |

**Gate P4：**CLI/TUI parity、keyboard journeys、theme/race/epaper tests 全過。不得只提供 theme flag 而未替換 overlays 的固定色碼；HF-UX-039 未完成前，不得將新程式碼疊加進未分解的 `tui.go`。

### Phase 5 — Evidence／Context／Learning 與 Review

Read-only CLI adapters 不依賴 Phase 4；只有把它們接入 TUI view 時才依賴 HF-UX-040。

| 工作 ID | 依賴 | 工作 | 完成條件 |
|---|---|---|---|
| HF-UX-050 | 011B | Evidence/Context detail adapters | scope/privacy 與 inspect契約一致；raw claim不成 verified |
| HF-UX-051 | 011A/050 | Learning overview / empty states / effective mode | exposure/usage/outcome/promotion區分；unknown不變0 |
| HF-UX-052 | 001A | promotion read-only query 接縫 | list/refresh 不 flush outbox、不 migrate |
| HF-UX-053 | 021/052 | interactive promotion review | approve/apply分開；stale/double-submit/secret tests全過 |
| HF-UX-054 | 050/051 | skill drafts 與 improve evidence導覽 | 不混淆 lifecycle；只連既有可用能力 |
| HF-UX-055 | 040/050/051/052 | 將 Evidence/Context/Learning/Promotion detail 接入 TUI | passive refresh純讀；owner mutation仍走typed intent |

**Gate P5：**有證據的 learning journey 與安全 publication 全流程通過；不能以開一個 Learning tab 當完成。

### Phase 6 — Wizard、Completion、Documentation

各 work item 使用明示 dependency；wizard 使用成熟的 team check。

| 工作 ID | 依賴 | 工作 | 完成條件 |
|---|---|---|---|
| HF-UX-060 | 033 | optional wizard + preview/validate/write | non-TTY不掛起；不覆寫 team；不自動跑任務 |
| HF-UX-061 | 011B/052 | scope-aware ID completion | bounded read-only；不跨 private/branch；不 call network |
| HF-UX-062 | 030/031A/032/033 | journey guides / migration / generated command reference | examples可parse；只列已落地 syntax |
| HF-UX-063 | 022/031B/050 | user-facing troubleshooting 與恢復步驟 | 三問可由正常介面回答 |

**Gate P6：**文件、help、completion 共用 metadata；無未實作命令出現在已發布指南。

### Phase 7 — Usability Gate、發布與 Rollback

依賴：欲發布 feature 的前置 phases 完成。

| 工作 ID | 工作 | 完成條件 |
|---|---|---|
| HF-UX-070 | baseline vs candidate 真人 task tests | 依第 18 節產生 report，不用模型主觀評分代替 |
| HF-UX-071 | cross-platform / legacy automation / performance | supported platform matrix 明確，未測平台不得標已驗收 |
| HF-UX-072 | release notes、opt-in rollout、fallback 路徑 | 舊入口、plain renderer、舊 theme 或關閉新摘要的退回路徑可用 |
| HF-UX-073 | 最終 no-regression 與 source docs 更新 | 每個 requirement 有 test/evidence 或明列未交付；無假完成 |

**Gate P7：**關鍵 safety/compatibility gates 零失敗；其餘目標未達就調整 UI，不降低 verification 或 authorization 來換取較少步驟。


---

<a id="section-17"></a>

## 17. 必要 Regression／Acceptance Test Matrix

### 17.1 測試層級

`unit` 驗證純映射、resolver、recommendation；`contract/golden` 保護 CLI/schema/exit；`integration` 使用暫存 workspace、fake provider/tool/backend；`TUI` 使用 fake clock、keyboard messages、固定 terminal sizes；`usability` 由真實使用者做任務。

禁止使用付費 provider 的偶然回答作為 CI passing condition。真實 provider smoke test 可 opt-in，但不代替 deterministic tests。

### 17.2 最低測試集合

| Test ID | 情境 | 必須驗證 | 關聯工作 |
|---|---|---|---|
| UX-T01 | 無 workspace 的 help/completion | 不建目錄、不開 DB、不 network、不 provider | 000/030/061 |
| UX-T02 | static team check | compiler/lint 依契約執行；online/probe 為 skipped，不誤報 ready | 033 |
| UX-T03 | online check timeout | bounded timeout、unknown/failed 分類正確；不做 inference | 033 |
| UX-T04 | 新 run exact -w | 路徑不再次接上 team name | 010/031 |
| UX-T05 | legacy root -w | 與 baseline 路徑完全一致 | 001/010 |
| UX-T06 | workspace 與 workspace-root 同時指定 | action 前報互斥；無任何寫入 | 010 |
| UX-T07 | team/root/default/multi-team matrix | 每種語意明確；禁止兩個 team 共用不明 exact scope | 010/031 |
| UX-T08 | symlink/worktree/relative path | canonical identity 連續；requested spelling 可解釋 | 010/013 |
| UX-T09 | 同 task ID 出現在不同 run/branch | query 不誤 join；mutation 不選錯 target | 011/013/023 |
| UX-T10 | ProjectID 與 cwd/repo 名稱不同 | 使用真實 canonical ID；不硬編碼 hufu | 013/052 |
| UX-T11 | legacy JSON 與 exit fixtures | alias/new renderer 不改 payload 或 numeric code | 001/032 |
| UX-T12 | --json/--format/--output 衝突 | 相同接受、不同在 action 前失敗 | 032 |
| UX-T13 | skill graph Mermaid | 原 format 與內容契約不變 | 032 |
| UX-T14 | machine stdout | 單一合法 document，無 ANSI/spinner/banner | 022/032 |
| UX-T15 | stderr event-format=jsonl | 依既有 contract；人類補充訊息不混入 event stream | 022/032 |
| UX-T16 | inspect 成功、run failed/partial | operation success 與 run outcome 分開 | 011/012 |
| UX-T17 | worker 完成但 verification 未完成 | 顯示 verifying，不顯示成功 | 011/040 |
| UX-T18 | runtime 修復先前 failure 後 terminal success | 顯示 runtime 最終結論，不因歷史紅字永久判 failed | 011/040 |
| UX-T19 | 沒有 live heartbeat、舊 snapshot | 顯示 freshness unknown/stale；不自行判 crash | 011/022 |
| UX-T20 | external effect unknown | primary 是 inspection/reconcile eligibility；絕無直接 retry | 020/023 |
| UX-T21 | policy deny / approval pending | 不推薦繞過 flags；Enter 不批准 | 020/043 |
| UX-T22 | 已完成 task 的 stale Next Action | 重驗後拒絕 rerun，不產生第二次 side effect | 021/023 |
| UX-T23 | 學習 aggregate degraded | 建議診斷／明確重建；不重跑 worker/tool/acceptance | 020/051 |
| UX-T24 | 相同 facts/policy/revision | action ID/type/argv/order 完全相同 | 020/021 |
| UX-T25 | 只有 unrelated event 更新 | action 不因查詢時間改變任意失效；target 改變才失效 | 021/023 |
| UX-T26 | selector 含空白、引號、Unicode、shell metacharacters | argv 不注入 shell；unknown shell 不提供錯誤 copy | 021/044 |
| UX-T27 | output 含 ANSI/OSC52/control sequence | 不觸發 terminal escape/copy；可讀且有界 | 022/044 |
| UX-T28 | API key/secret 在 config/error/proposal | summary、JSON、clipboard、next action 不洩漏 | 021/044/053 |
| UX-T29 | -m 切 agent backend | 只影響 worker，coordinator/auxiliary 獨立 | 034 |
| UX-T30 | 同 session 不同 backend attempts | 顯示各自 durable target；舊 attempt 不被新設定覆寫 | 034/040 |
| UX-T31 | read-only promotion list/refresh | 不 writable open/migrate/flush outbox/create DB | 052 |
| UX-T32 | approve 後未 apply | UI 顯示 approved-not-applied，正式檔案未變 | 053 |
| UX-T33 | review 中 source/target/draft 變動 | 不能套用舊 approval；重新 preflight/refresh | 053 |
| UX-T34 | double key/click/resend | 不重複 approve/apply/retry；保留原 idempotency | 023/053 |
| UX-T35 | unattended 或 generic auto-approve | 不繞過 promotion human gate | 053 |
| UX-T36 | 無 eligible / DB query failure | empty 與 unknown 分別表示；不自動 draft | 051/052 |
| UX-T37 | exposed/consulted/applied/verified | 不混算 reward、learning level 或 success rate | 051 |
| UX-T38 | default context view/private memory | 未授權不顯示；completion/copy 不暗加 all-agents | 050/061 |
| UX-T39 | CLI/TUI 同一 snapshot | state/outcome/evidence/primary action 語意完全一致 | 022/040 |
| UX-T40 | theme light→dark→mono→auto | 不重啟 agent；輸入、focus、scroll、approval state 不變 | 041 |
| UX-T41 | NO_COLOR/non-TTY/plain | 無不應有的 ANSI/control；換 theme 不啟用被禁色彩 | 041/044 |
| UX-T42 | epaper idle/token flood/critical gate | idle 0 repaint；背景 <=1Hz；重要 gate 不丟失 | 042 |
| UX-T43 | resize/CJK/長 path/60×20 | State/What/Next 可讀；cell width 正確；detail 可展開 | 044 |
| UX-T44 | 新 event 在使用者閱覽時進來 | 不搶焦點、不自動切 task、不重設 scroll | 040/044 |
| UX-T45 | 舊 query 晚於新 scope query 回來 | 不覆寫目前 run/branch 畫面 | 011/040 |
| UX-T46 | owner/passive view 的 q/Ctrl+C/PTY focus | close、wrap-up、force、detach 不混淆；無未支援背景承諾 | 043 |
| UX-T47 | wizard cancel/non-TTY/file collision | 不覆寫、不執行、不阻塞腳本、不產生半套正式 team | 060 |
| UX-T48 | 多個 command factory 同程序連續建立執行 | 無 shared opts 污染、Changed flag provenance 不丟 | 031/032 |
| UX-T49 | read-only overview/storage 的 malformed/舊 schema | 不自動 migrate/repair；typed diagnostics | 011/012 |
| UX-T50 | large event/context corpus | 查詢／rendering 有界；不逐 token full replay/scan | 011/042/071 |
| UX-T51 | 新 façade 與 legacy equivalent scenario | provider/tool invocation、durable outcome、receipt/verification 一致 | 031/071 |
| UX-T52 | 未啟用新 UI 的 baseline | runtime behavior/output contract 無 regression | 072/073 |

### 17.3 Snapshot/Golden 測試的限制

Snapshot tests 不只比「截圖長得一樣」。每個 critical screen 另有 semantic assertion：state、scope、outcome、reason code、next-action risk/argv。

固定 rendering 時用 fake clock，不把真實 timestamp、random ID、endpoint secrets 固定進 golden。Accessibility case 不能只跑彩色寬螢幕。

### 17.4 Read-only/no-side-effect 測試方法

在 temporary HOME/CWD/workspace 下執行 read-only queries，使用 fake provider/MCP/tool/repair callbacks 斷言零呼叫；記錄 canonical records、events、outbox 與主要檔案前後差異。

至少覆蓋 absent DB、read-only filesystem、既有 WAL、schema version 不支援、DB locked、partial event file。不能用 `immutable` 模式跳過 active WAL consistency。若既有 SQLite read path 對 OS lock/shared memory 有平台差異，明確區分暫時協調行為與 domain writes，禁止新增 durable state、migration、outbox delivery 或隱性 checkpoint。

### 17.5 建議執行命令

以下在 hufu repo、按 repository `go.mod` 與 toolchain執行。基準提交已通過 focused tests 與
`go test ./...`；每個 code/test PR 仍須重新執行全部適用 gates：

```bash
go test ./cmd/hufu/... ./internal/inspect/... ./internal/tui/... ./internal/promotion/...
go test -race ./cmd/hufu/... ./internal/inspect/... ./internal/tui/...
go test ./...
go vet ./...
golangci-lint run
```

Lint、docs check 與跨平台 jobs 依 repo 既有 CI 執行。若新增 package，也必須納入 focused/race tests。真人測試與 terminal interoperability 另存 evidence，不因 `go test` 通過就標記已完成 usability 驗收。

### 17.6 支援平台與驗收層級

| Surface | Merge gate | Stable release gate |
|---|---|---|
| Linux amd64/arm64 | 現有 Ubuntu CI：lint、vet、race/short、CGO-free、build、docs、eval | native TUI/PTY/manual smoke；epaper若宣稱支援則實機或低刷新錄製 |
| Darwin amd64/arm64 | CGO-free cross-build與 test compile | 至少一個 native Darwin terminal smoke；未完成前新 TUI標 preview |
| Windows | 僅 Bash/PowerShell argv renderer pure unit tests | 非 `.goreleaser.yml` target，本計畫不宣稱 binary/TUI 支援 |
| Bash/Zsh/Fish/Nushell completion | Linux deterministic generation/parse fixtures | 在可取得的原生 shell smoke；未測 shell在 release note明列 |
| PowerShell command rendering | pure escaping/argv golden；不執行 shell | 僅作可讀顯示，不宣稱 Windows CLI 支援 |

跨平台 release gate 不阻擋純 Linux implementation PR；它只控制對應 feature/platform 是否由
preview 升為 stable。不得因缺少某平台環境而刪除平台無關的安全測試。

---

<a id="section-18"></a>

## 18. 使用者體驗驗收：以行為衡量，不以畫面數量衡量

### 18.1 必達安全條件

以下任何一項失敗，禁止發布為 stable：

| 指標 | Gate |
|---|---|
| 錯 workspace/run/branch 上的寫入 | 0 次 |
| unknown external effect 時引導直接重試 | 0 次 |
| 查看／theme／completion 觸發 mutation | 0 次 |
| 未經明確 review 自動 approve/apply | 0 次 |
| legacy script 的未宣告 JSON/exit/path breaking change | 0 個 |
| raw secret/terminal injection 經 UI 或 clipboard 洩漏 | 0 個 |
| CLI 與 TUI 對同一 snapshot 的 state/action 衝突 | 0 個 |

安全門檻不與省步驟、速度、成功率平均後抵銷。

### 18.2 真人 task test

建議至少 6 位工程使用者：3 位沒用過 hufu、3 位有既有 workflow。這是 formative usability evaluation，不宣稱能代表所有使用者。使用 baseline/candidate 同一 corpus，交錯順序降低學習效應；記錄樣本數、每人結果、錯誤類型與未完成原因。

每人涵蓋第 14 節 Journeys A～F 的代表任務。測試主持人在測量階段不講解 subsystem，不提示下一個 command；受測者靠正常 help/UI 完成。

| UX 指標 | 建議 acceptance target | 測量方式 |
|---|---|---|
| 三問辨識 | 至少 90% 的測試情境能在 10 秒內正確說出 state、主要原因、安全下一步 | 螢幕＋口述評分，事先定義標準答案 |
| 初次可驗證 run | 環境已 provision 下，中位數 <=10 分鐘；相較 baseline 不惡化 | 從 create/check 開始到 objective result，另報含 install/model loading 的 wall time |
| 找到故障下一步 | 中位數操作步數比 baseline 減少至少 30%，且安全錯誤為零 | 只數必要 actions/commands；不可刪 confirmation 假裝改善 |
| Workspace 選擇 | 所有 scope-conflict fixtures 中無誤寫；使用者能指出 exact path | 路徑測驗＋實際 operation receipt |
| Promotion comprehension | 所有受測者可區分 proposed、approved、applied | 明確問答＋實際 review journey |
| 電子紙舒適與控制 | 任務全程可用 keyboard 完成；無 input loss/持續閃爍/僅顏色訊號 | 實際顯示器或低刷新終端測試與 recording |
| Power-user 效率 | 常用 scripting/快捷操作不增加強制互動 | baseline command replay、non-TTY tests |

這些數字是產品目標；不能用「已建立測試」替代「測試已達標」。實測不足時把功能維持 preview，並保留 failed scenario。

### 18.3 技術效能目標

在 Phase 0 記錄 reference hardware、OS、terminal、Go 版本與 corpus 大小。建議至少 small/large 兩組 workspace，large 包含足以暴露全量掃描問題的 events/context items。

- Warm overview query 目標 p95 <=250 ms；cold query 單獨報告，不用 warm cache 掩蓋。
- 新 help/static query 不應有 provider startup latency；static check 應在可接受的本地配置時間內完成，超出時指出哪個 check 耗時。
- 一般 TUI 的已知重要 state change 目標在 500 ms 內反映；epaper 背景更新可到 1 秒，輸入與 gate 不受一般 throttle 影響。
- Idle epaper repaint = 0；不為 freshness 秒數持續佔 CPU。
- Queue/buffer 有上限，長任務不線性增加所有 rendered logs 的常駐記憶體。

尚未量測前不得宣稱上述效能。若硬體或大資料量使絕對目標不合理，提出 measurement report 與修訂理由，不能靜默放寬 gate。

### 18.4 UX telemetry 的隱私與邊界

測試期間用 explicit recording/metrics harness 收集 command count、操作耗時、錯誤分類與介面模式。不預設上傳 analytics；不收集原始 prompt、memory、tool args、key、完整 path 或文件內容。

Production 如需 opt-in UX metrics，單獨設計 consent、retention 與 redaction。**read-only help/inspect/completion 不得為了統計使用次數而悄悄寫入 workspace 或 flush events。**

---

<a id="section-19"></a>

## 19. Compatibility、發布、Rollback

### 19.1 相容性表

| Surface | 本計畫允許變更 | 禁止的隱性破壞 |
|---|---|---|
| legacy root execution | help/diagnostic 改善、文件改推薦新 syntax | 既有 -w 重新詮釋、忽略 @team、改 provider selection |
| 新 `run` | exact workspace + explicit root semantics | 內部再走 legacy join |
| `--json` / `--format` | 新增 equivalent output alias | 換 schema、換 exit code、污染 stdout |
| existing specialized queries | 加連到 overview 的引導 | 換成寫入式 facade 或暗改 scope |
| context/promotion lifecycle | 更好的 read/review UX | 自動 approve/apply、提高信用、修改 hard gates |
| TUI | theme、navigation、summary、低刷新 | 簡化結果造成 false success；q 意外執行 kill |
| runtime storage | 本計畫原則上不 migration | 新 canonical DB、搬 workspace、改 identity |
| team definitions | wizard 顯式產生／使用者 edit | UI 初始化或查看時自動改 team.yaml |

### 19.2 Rollout 順序

1. **Additive preview：**先發布 read-only overview、help 分組、可選新 aliases；舊入口完整保留。
2. **CLI default presentation：**通過 contract/usability gate 後，改推薦新 syntax 與操作摘要；machine contract 不變。
3. **TUI opt-in：**保留現有 TUI/plain 路徑，以 presentation preference 選擇新操作面板，不新增 runtime profile。
4. **Learning review opt-in：**read-only views 穩定後才開寫入操作按鈕；authorization 不因 opt-in 自動允許。
5. **Stable：**文件標示通過的功能與平台。Legacy deprecation 要另有公告與使用證據，不以本計畫日期自動移除。

### 19.3 Rollback

- 可關閉新摘要／console，回既有 CLI/TUI 或 plain rendering；底層 session/evidence/context 無需轉換。
- 新增 query/view 失敗時顯示 unavailable，不能為了「fallback」執行 provider 或偷偷修資料。
- Alias、output renderer 的 rollback 不得換回有不同 workspace 意義的新命令；已發布 syntax 應持續有安全 adapter。
- Promotion 曾經 apply 後，UI feature rollback 不等於撤銷 policy/skill。必須分開回復正式檔案並保留 audit history。
- 本計畫不搬資料，故不應有 UX-only SQLite rollback。其他 parallel runtime migrations 的回復由其原計畫管理。

---

<a id="section-20"></a>

## 20. Coding Agent 的工作方式與每個 PR 的 Definition of Done

### 20.1 執行約束

先讀 `AGENTS.md`、`docs/README.md`、相關 package 的現行約束、code/tests；本文件高於先前聊天中未驗證的建議，但不高於 existing runtime contract。

禁止：

- 先大規模刪除/搬移命令，再補相容性測試。
- 新增很多 wrappers，卻每個 wrapper 各自猜 workspace、query status 或生成 next action。
- 用 LLM 總結 raw logs 來「判斷現在的 state」。
- 把「看起來更簡單」當允許關閉安全 gate、修改 retry/escalation 或降低 evidence 門檻。
- 把已有 feature 或歷史 archived spec 再實作一次。
- 因 scope 複雜而默認取第一筆、最新一筆、全域 private context。
- 一個 PR 同時重構 Cobra lifecycle、execution runtime、memory store 與所有 UI。

### 20.2 建議分工

Phase 0 由一個 agent 建立 baseline/contract inventory。Schema 凍結後再分為 query/scope、CLI、TUI 三條工作線；`root.go`、全域 options、scope adapter 等高衝突檔案指定單一 owner。

Reviewer 優先審查 scope、authority、state inference、idempotency、output compatibility，再審查 rendering。Layout 美化不能掩蓋 semantic regression。

### 20.3 每個 PR 必須附

| 項目 | 必要內容 |
|---|---|
| 目的 | 改善哪一條 user journey、哪一個三問缺口 |
| Contract | 新增/不變/刻意改變的 syntax、scope、JSON、exit、side effects |
| Implementation | 重用的 service/query 接縫；為何不重建 abstraction |
| Tests | HF-UX / UX-T 對應、實際 command 與結果、失敗基線 |
| Evidence | plain/JSON/TUI snapshots；必要時測量 report |
| Safety | unknown effect、stale target、private context、read-only/no-write assertions |
| Rollback | 如何回舊 presentation，是否有使用者檔案變更 |
| 下一步 | 哪個 phase gate 已完成、哪個仍未滿足；不能把 TODO 當完成 |

### 20.4 Engineering PR DoD

每個 work item PR 只需滿足其 scope 與 prerequisite，不因尚未執行真人研究或其他平台 smoke
而阻擋合併：

- [ ] 對應 HF-UX/UX-T tests、`go test ./...`、`go vet ./...`、`golangci-lint run` 通過。
- [ ] 既有 JSON/exit/path goldens無未宣告變化；read-only assertion零 mutation/provider/tool。
- [ ] 新 schema/enum/argv符合 §3、§5、§6、§7、§9、§10；未知值 fail closed。
- [ ] 文件、help與實作狀態一致；未落地 surface不出現在一般使用指南。
- [ ] PR說明包含 §20.3 的 contract、evidence、rollback與下一個 gate。

### 20.5 Stable release DoD

- [ ] 核心畫面都有 State/What/Next，且有 source refs 與 freshness。
- [ ] CLI/TUI/JSON 讀同一個 snapshot，沒有第二套 execution truth。
- [ ] Next Action deterministic；unknown/policy-denied 不產生 unsafe mutation。
- [ ] 精確的 scope resolution、legacy -w 行為與同名 task collision tests 通過。
- [ ] 新 `run`、session façades、team check 的 operation 與 flags provenance 正確。
- [ ] 舊 JSON、exit code、specialized output formats 與 scripts 無未宣告破壞。
- [ ] Read-only views/help/completion 不 create/migrate/flush/repair。
- [ ] Theme 可動態切換，epaper 不逐 token 重繪；CJK/小終端/keyboard 可用。
- [ ] Learning metrics 不混淆 exposure、usage、credit；promotion 仍需明確兩階段審查／套用。
- [ ] 符合支援平台的 tests 已執行；未測部分明列，不冒稱通過。
- [ ] 真人使用性 report 已驗證三問與安全恢復旅程；未達標功能維持 preview。
- [ ] docs/help/examples/completion 一致；所有新命令已實作才列入正式手冊。

---

<a id="section-21"></a>

## 21. 可直接交給 Coding Agent 的起始指令

> 實作 `docs/architecture/operator-experience.md`。第一個 PR 只做 HF-UX-001A：新增 root/chat/resume/recovery/context/promotion/inspect/skill/team 的 workspace、JSON、exit、TTY/no-side-effect characterization tests 與 contract inventory，production behavior 必須零變更。完成後先做 HF-UX-001B；Gate P0 通過後，依 work-item dependency 進入 010A/011A。不要把整份 roadmap 放入單一 PR。
>
> 維持 legacy CLI、workspace、JSON 與 exit code contract。新增功能以共用 service/query façade 完成，不重建 runtime、memory store 或 recovery policy。每個階段先補 regression tests，再接 CLI/TUI。不得以 LLM prose 判定完成，不得在 read-only UI 中 flush/migrate，不得降低 promotion/verification/human gate。
>
> 每個 PR 的交付都回答：目前完成到哪個 phase、這次改變了什麼且有何證據、下一步要通過哪個 gate。遇到基準差異，更新 inventory 並採用現行 code/tests；不要以舊 spec 強迫 runtime 退回。

---

<a id="section-22"></a>

## 22. 來源、範圍與追溯

### 22.1 來源使用方式

原始碼優先於 README 與歷史計畫。下列 permalink 固定到本次查核的 `ba30643754d8d6cd85f91624a3f9bedf9d32457a`；除 `[S09]` 表示前文已查讀而本輪僅用作接縫索引外，核心現況已在本輪查讀相應內容。實作時以新的 HEAD 再核對，不把本文件視為永不過期的 CLI reference。

手冊 `hufu-operator-self-learning-handbook.md` 是本次對話的使用流程背景，基準為較早的 `0a68f3e...`；不是 source of truth，也不以它覆蓋本輪的新 source findings。

### 22.2 hufu 來源

[S01]: https://github.com/kjelly/hufu/blob/ba30643754d8d6cd85f91624a3f9bedf9d32457a/cmd/hufu/root.go
[S02]: https://github.com/kjelly/hufu/blob/ba30643754d8d6cd85f91624a3f9bedf9d32457a/cmd/hufu/helpcmd.go
[S03]: https://github.com/kjelly/hufu/blob/ba30643754d8d6cd85f91624a3f9bedf9d32457a/cmd/hufu/team_loader.go
[S04]: https://github.com/kjelly/hufu/blob/ba30643754d8d6cd85f91624a3f9bedf9d32457a/cmd/hufu/recoverycmd.go
[S05]: https://github.com/kjelly/hufu/blob/ba30643754d8d6cd85f91624a3f9bedf9d32457a/cmd/hufu/model_overrides.go
[S06]: https://github.com/kjelly/hufu/blob/ba30643754d8d6cd85f91624a3f9bedf9d32457a/cmd/hufu/inspectcmd.go
[S07]: https://github.com/kjelly/hufu/blob/ba30643754d8d6cd85f91624a3f9bedf9d32457a/docs/guides/inspect.md
[S08]: https://github.com/kjelly/hufu/blob/ba30643754d8d6cd85f91624a3f9bedf9d32457a/internal/tui/tui.go
[S09]: https://github.com/kjelly/hufu/tree/ba30643754d8d6cd85f91624a3f9bedf9d32457a/cmd/hufu
[S10]: https://github.com/kjelly/hufu/blob/ba30643754d8d6cd85f91624a3f9bedf9d32457a/cmd/hufu/context_promotion_cmd.go
[S11]: https://github.com/kjelly/hufu/blob/ba30643754d8d6cd85f91624a3f9bedf9d32457a/internal/tui/NAVIGATION_SPEC.md

| 引用 | 查核內容 |
|---|---|
| [S01] | root command registration、flags、--model/help/output 相關說明 |
| [S02] | 既有 examples、help-flags 分組與 rendering |
| [S03] | named team base workspace join、canonical path handling |
| [S04] | recovery command 的 workspace/team inference 與 output flags |
| [S05] | worker-only model override、auxiliary roles 的獨立性 |
| [S06] | inspector 依賴 internal/inspect、command factories、query selectors、format |
| [S07] | read-only guarantee、storage inspector、exit semantics、privacy |
| [S08] | TUI message/model fields、既有 overlays/PTY、固定 styles、spinner/compact |
| [S09] | 前文已查讀的 teamcreatecmd/teamcmd/teamshowcmd/teamexplaincmd/teamlintcmd；實作 Phase 0 須逐一核對現行版本 |
| [S10] | promotion lifecycle CLI、shared writable open 與 outbox flush 接縫 |
| [S11] | TUI navigation 設計中的既有按鍵；最終以 code/tests 核對，不能把 checklist 當已通過測試 |

### 22.3 外部 primary references

外部資料僅用於既有 framework 能力與 display convention，不取代 hufu code 或使用者需求。

[E01]: https://cobra.dev/docs/how-to-guides/working-with-commands/
[E02]: https://no-color.org/

| 引用 | 使用範圍 |
|---|---|
| [E01] | Cobra 原生 command grouping / help 的擴充接縫；不因此要求升級 framework |
| [E02] | NO_COLOR 的非空環境變數與 explicit preference 原則；不把 no-color 誤當 no-animation |

本文件的 state projection、action contract 與 phase sequencing 是已採納但尚待實作的新增契約；UX／performance targets 與使用性測試方案則是 release 驗收目標。兩者都不是原始碼中已存在的功能，也不是外部研究已證實 hufu 已達到的效益。
