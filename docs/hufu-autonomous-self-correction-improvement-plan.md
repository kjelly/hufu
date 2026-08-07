# hufu 自主診斷、重規劃與自我修正改善計畫

## 1. 目的與成功定義

本計畫讓 hufu 從「任務失敗後附帶提示重試」演進為可稽核的自我修正工作流引擎：以證據診斷失敗、在受限範圍內重新設計計畫、執行可恢復的修復，並以獨立驗證決定最終結果。

系統不得宣稱能保證任意任務必定完成。可保證的安全語意是：任何執行都必須以一個誠實且可驗證的終態結束：

- `accepted`：所有必要任務、evidence 與 run-level acceptance 都通過。
- `partial`：已交付部分可證實的成果，但尚未達成完整 acceptance。
- `blocked`：缺少權限、外部資訊、可用能力或足夠證據，需人工介入。
- `rolled_back`：未能達成 acceptance，且已執行並記錄 rollback。
- `failed`：已耗盡可安全採取的修復策略，並保留完整診斷證據。

`accepted` 是唯一可對外表示「完成」的狀態。不得由 worker 的自然語言回覆、單一模型判斷或未經驗證的快取直接產生。

## 2. 現有基礎與主要缺口

hufu 已有 failure classification、retry disposition、reflexion hint、`ReplanRequired`、plan reviewer、verification、acceptance、anti-thrashing、side-effect-aware crash recovery 與 event store 基礎。這些能力尚未形成一個一致的控制迴路。

目前主要缺口：

1. 失敗資訊分散在 task、journal、receipt、verify 與 prompt，沒有單一的診斷合約。
2. `ReplanRequired` 是正確的停止訊號，但沒有版本化、可審查的重規劃物件與 DAG diff。
3. retry、reconcile、replan、self-healing 與 rollback 分散於不同路徑，難以統一套用安全、預算與可觀測性規則。
4. completion、task verification 與 run acceptance 的證據鏈尚未完全收斂為唯一 gate。
5. reflexion lesson 可以保留經驗，但尚未區分「候選推論」與「經 acceptance 證實的知識」。

本文件補足上述閉環，不取代 `docs/hufu-future-improvement-roadmap.md`。既有 Go 子系統工作卡與實作狀態仍以 roadmap 為單一真相來源。

## 3. 目標控制迴路

```text
observe evidence
      |
      v
diagnose failure -> block / reconcile when uncertainty or policy requires it
      |
      v
form repair hypothesis -> construct PlanRevision -> validate and review
      |
      v
checkpoint -> controlled execution -> task verification -> acceptance
      |                                                       |
      +------------------- failure evidence ------------------+
```

每次迴圈都必須有新的可觀察進展：新的證據、新的修復假設、新的計畫版本，或更嚴格的終態。缺乏進展時，anti-thrashing policy 必須升級為 `blocked`、`partial` 或 `failed`，而不是無限重試。

### 3.1 任務狀態機

在既有 task 狀態上增加或正式化下列轉換：

```text
planned -> executing -> verifying -> accepted
                    |               |
                    v               v
               diagnosing        re-evaluate criteria
                    |
                    +-> reconciling -> executing
                    +-> replanning  -> planned
                    +-> blocked / partial / rolled_back / failed
```

規則：

- `executing` 不得直接轉為成功終態。
- `verifying` 必須保存 verifier、命令或 typed spec、輸入、輸出、退出碼、時間與 artifact digest。
- 任何被 `replanning` 取代的 task 必須保留其失敗原因與 PlanRevision 關聯，不能被覆寫為一般 `done`。
- 需要人工權限、外部事實或高風險副作用時，診斷結果必須為 `blocked`，而非自動嘗試替代動作。

## 4. 不可突破的安全邊界

1. **Acceptance immutability**：run 開始後，agent 不得放寬 acceptance、移除 required artifact 或把 strict verification 降級。任何此類變更都需要使用者明確授權，並產生 audit event。
2. **Least-privilege repair**：repair 只能使用原 task 或 team policy 已授權的工具、路徑、網路與 side-effect class；不可因為失敗而擴權。
3. **Uncertain external state**：有外部寫入、infra mutation 或 credential mutation 的不確定中斷狀態，先 `reconcile`；無可靠 reconcile tool 時轉 `blocked`。
4. **Evidence over assertion**：模型建議、skeptic vote 與 reflexion 都是診斷訊號，不是完成證據。
5. **Budgeted convergence**：對每個 run、task、diagnosis、replan 與 repair 設定時間、token、attempt 與 side-effect 預算；預算耗盡後產生可續作的 `partial` 或 `blocked` 結果。

## 5. 新增資料模型

### 5.1 DiagnosticPacket（HF-AR-001）

每次 task failure 都建立不可變診斷包，並寫入 event store：

```go
type DiagnosticPacket struct {
    ID                string
    RunID             string
    TaskID            string
    Attempt           int
    PlanRevisionID    string
    FailureClass      TaskFailureClass
    Disposition       RetryDisposition
    Confidence        float64
    EvidenceRefs      []EvidenceRef
    VerifyResult      *VerificationResult
    CapabilityFinding []CapabilityFinding
    EnvironmentDigest string
    ArtifactDigests   []string
    SideEffect        SideEffectClass
    Recovery          RecoveryPolicy
    BudgetSnapshot    BudgetSnapshot
    Hypotheses        []RepairHypothesis
    CreatedAt         time.Time
}
```

資料來源包含既有 `FailureEvent`、execution receipt、verify result、tool transcript、capability probe、task result 與 artifact store。所有文字欄位寫入前必須經統一 secret redaction。

### 5.2 RepairHypothesis（HF-AR-002）

診斷引擎輸出可審查的假設，而不是純文字提示：

```go
type RepairHypothesis struct {
    ID             string
    Cause          string
    EvidenceRefs   []EvidenceRef
    ProposedAction RepairAction // retry, reconcile, replan, block, rollback
    ExpectedSignal string
    Risk           RiskLevel
    Confidence     float64
}
```

deterministic classifier 必須優先處理可判定情境，例如 verify exit、missing command、permission、timeout、policy denial、相同 failure fingerprint 與 recovery state。sidecar 可補充假設，但不得覆蓋 deterministic safety decision；衝突或低信心時採保守 disposition。

### 5.3 PlanRevision（HF-AR-003）

每次重規劃產生不可變版本：

```go
type PlanRevision struct {
    ID                    string
    ParentID              string
    DiagnosticPacketIDs   []string
    Goal                  string
    Constraints           string
    AcceptanceFingerprint string
    TaskDAG               []TaskDef
    DAGDiff               PlanDiff
    RepairHypothesisIDs   []string
    Review                PlanReviewResult
    Status                PlanRevisionStatus
}
```

validator 必須拒絕：cycle、遺失 dependency、未授權的工具／路徑、未定義 verify 的高風險 task、改變 acceptance fingerprint、超出剩餘預算，以及與 resource claim 衝突的併發計畫。

## 6. 分期工作計畫

### Phase 0：完成可靠性地基

先關閉或收斂既有 roadmap 中會破壞診斷可信度的殘餘：

- `HF-PR-104`：完成 event store F1–F3，並把事件重播設為所有後續狀態的驗收前提。
- `HF-PR-105`：接線 typed result 至下游 context、每次 retry 清除舊 result、補 strict enforcement 與 escalation 整合測試。
- `HF-PR-108`：建立 artifact/evidence store 與 final evidence manifest。
- `HF-PR-006`：收斂 blocking acceptance 的語意，使 acceptance failure 不可能得到 completed outcome。

驗收：從 event store 重播後，task status、attempt、verify、acceptance、artifact 與 failure disposition 必須與原 run 一致。

### Phase 1：可稽核診斷

新增 `HF-AR-001 DiagnosticPacket` 與 `HF-AR-002 DiagnosisPolicy`。

- 將每次失敗歸併為診斷包，替換只靠 error string 的 retry context。
- 將現有 local failure hint、sidecar reflection、capability findings 與 anti-thrashing 併入診斷決策。
- 定義 `retry`、`reconcile`、`replan`、`block`、`rollback` 的明確 decision table。
- 診斷輸出需有 evidence reference、confidence、risk 與預期驗證信號。

驗收：對 timeout、permission denied、verify fail、重複 failure、tool missing、crash after external write、sidecar unavailable 等情境，disposition 必須 deterministic 且可由事件重播。

### Phase 2：受約束的重規劃

新增 `HF-AR-003 PlanRevision`。

- coordinator 根據 DiagnosticPacket 產生修復假設與新的 task DAG。
- 以 deterministic validator 先檢查，再交給 plan reviewer 評估可行性、完整性與最小變更性。
- 保存 parent revision、DAG diff、被淘汰 task、使用的證據與審查決定。
- acceptance contract 在 revision 間必須保持相同 fingerprint；不相同即要求人類批准。

驗收：同一失敗不可產生等價的無限 replan；replan 後只能執行 diff 中允許的 task；任何 acceptance weakening 都會被拒絕並留下 audit event。

### Phase 3：受控修復與恢復

新增 `HF-AR-004 RepairController`，並完成既有安全工作卡：

- `HF-PR-106` resource locks。
- `HF-PR-109` policy engine。
- `HF-PR-110` control/subject workspace separation。
- `HF-PR-111` secret registry 與統一 redaction。

RepairController 負責統一協調 retry、model escalation、reconcile、replan、self-healing 與 rollback。每一個動作前都需 checkpoint，並套用剩餘預算與 side-effect policy。

驗收：infra mutation 在 crash-resume 時不會被盲目重播；control workspace 不會被 repair 清除；機密不會出現在 diagnostic packet、report 或 prompt；resource-conflicting task 不會並行。

### Phase 4：唯一完成閘門與知識治理

新增 `HF-AR-005 CompletionGate`。

- `accepted` 僅能由 CompletionGate 產生。
- Gate 檢查 acceptance、required evidence、artifact digest、所有 required task 的 terminal state、未解決風險與 terminal leak。
- reflexion／LTM entry 先以 candidate 儲存；只有 acceptance 通過且來源 evidence 完整時才提升為 confirmed lesson。

驗收：故意偽造 worker 成功文字、舊 cache hit、缺 artifact、失敗 verify 或未完成 acceptance 時，都不得輸出 `accepted`。

### Phase 5：可量化驗證與漸進上線

新增 `HF-AR-006 Reliability Eval Suite`，並擴充 `HF-OBS-001`。

- 建立可重播 scenario corpus 與 fault injection：provider timeout、sidecar 異常、verify wrong polarity、部分外部寫入、重複 failure、損毀 checkpoint、錯誤 acceptance、secret leakage。
- 在 report 與 telemetry 中輸出決策鏈、plan revision、evidence manifest、修復成本與終態原因。
- 採 shadow mode → warn-only → strict opt-in → strict default 的 rollout；嚴格模式初期僅適用具備 acceptance contract 的 team。

驗收指標：

- false-completion rate：未通過 acceptance 卻被標為 accepted 的比例，目標為 0。
- evidence coverage：accepted run 中有完整 evidence manifest 的比例，目標為 100%。
- diagnostic determinism：相同 evidence replay 產生相同 disposition 的比例，目標為 100%。
- repair convergence：在預算內由 replan/reconcile 成功達成 acceptance 的比例。
- unsafe replay rate：有外部副作用的未知狀態被自動重跑的比例，目標為 0。

## 7. 決策表

| 診斷結果 | 預設動作 | 自動化條件 |
|---|---|---|
| 暫時性 provider／網路 timeout | retry | 有剩餘預算、無外部未知副作用、failure fingerprint 未重複。 |
| verify failure，且 deliverable 可由 worker 修改 | replan 或 retry | 診斷包能指出可變 artifact，且 verify 合約未被修改。 |
| verify wrong polarity／不可由 worker 修改的 contract | replan | 必須由 coordinator 重建 task；不可重複 dispatch 同一 worker task。 |
| 外部寫入後中斷 | reconcile | 必須有成功的 reconcile evidence；否則 block。 |
| 權限、policy 或缺憑證 | blocked | 不得透過替代工具繞過 policy。 |
| 相同 failure fingerprint 跨 task 擴散 | systemic replan 或 blocked | 停止該 scope 的新 dispatch，先檢查共同根因。 |
| acceptance 失敗 | self-healing/replan | 僅在安全預算內；unattended 耗盡後依已授權 rollback policy 處理。 |

## 8. 測試策略

每個 Phase 必須提供：

1. 單元測試：schema、decision table、validator、redaction、state transition。
2. 事件重播測試：從 event store 重新建構 task、diagnostic packet 與 plan revision。
3. fault-injection 測試：在 tool call、verify、checkpoint、replan review 與 acceptance 前後中斷。
4. 整合測試：模擬 worker 報成功但 evidence 不足、replan 成功、replan 被拒絕、external side effect 需 reconcile 等流程。
5. 性質測試：任何事件序列均不可產生沒有完整 evidence 的 `accepted`；相同輸入不可因模型文字差異而改變安全 disposition。

所有 Go 程式變更完成前必須執行：

```bash
go test ./...
go vet ./...
golangci-lint run
```

## 9. 建議實作順序

1. `HF-PR-104` 殘餘與 `HF-PR-105` 殘餘。
2. `HF-PR-108` evidence store。
3. `HF-AR-001` DiagnosticPacket。
4. `HF-AR-002` DiagnosisPolicy。
5. `HF-AR-003` PlanRevision。
6. `HF-PR-106`、`109`、`110`、`111` 與 `HF-AR-004` RepairController。
7. `HF-AR-005` CompletionGate。
8. `HF-AR-006` evaluation、replay 與 rollout。

這個順序先建立「知道事實」與「保存證據」的能力，再讓系統重規劃與修復；避免在沒有可信狀態、證據與安全邊界時，把更多自治權交給 LLM。
