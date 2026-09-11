# Hufu 強制步驟依賴模型呼叫之調查與機制優化方案

**日期：** 2026-08-27
**主題：** Hufu 運行時中「強制性但依賴模型主動發起 Tool Call」的步驟盤點、根本矛盾與機制優化方案
**狀態：** 分析與設計提案

---

## 1. 背景與核心問題

在 Hufu 的多 Agent 協作架構中，系統引入了**合約狀態機（Contract State Machine）、執行憑證（Execution Receipts）、工具序列策略（Sequence Policy）與驗證門禁（Verification Gates）**來確保任務的可靠性與可追溯性。

然而，目前許多關鍵的生命週期流轉與門禁判定，本質上是**「強制性的系統要求」**，卻**「依賴機率性的語言模型主動發起正確的 Tool Call」**（如 `submit_result`、`finish`、`submit_plan`、`reconcile_task` 等）。

當模型出現以下常見行為時，系統就會陷入協議中斷、額外 Repair 輪次開銷、甚至死鎖或空轉：
1. **純文字停止（Conversational Stop）**：模型完成所有操作後，僅以自然語言輸出「我已經完成工作」，而沒有發起協議工具呼叫（`finish_reason=stop`）。
2. **工具誤呼叫 / 順序顛倒（Sequence & Tool Miscall）**：在封閉序列或特定階段呼叫了非預期的工具（如在結果回報階段呼叫探索工具）。
3. **步數預算耗盡（Step Budget Exhaustion）**：模型在達到 `MaxSteps` 前夕才完成工作，但在發起 `submit_result` 前被截斷，被系統誤診為協議違規。
4. **參數漏填或格式錯誤（Schema & Argument Mismatch）**：例如在 `finish` 時漏傳 `acknowledge_failed_tasks: true`，或在 `submit_result` 中填寫了虛構的產出路徑。

---

## 2. 現有程式碼中「強制但依賴模型呼叫」的步驟盤點

### 2.1 Worker / Subagent 執行階段

| 強制步驟 / 依賴工具 | 原始碼位置 | 強制機制與要求 | 模型漏呼叫 / 誤呼叫的後果 |
|---|---|---|---|
| **`submit_result`**<br>(提交結構化成果) | [`internal/team/coordinator_task_run.go#L880-L955`](file:///home/ubuntu/nfs/github/agent-team-cli/internal/team/coordinator_task_run.go#L880-L955)<br>[`internal/team/coordinator_tools_result.go`](file:///home/ubuntu/nfs/github/agent-team-cli/internal/team/coordinator_tools_result.go) | 所有非 sidecar 任務預設 `RequiresResult = true`。Worker 在完成操作後必須呼叫 `submit_result` 回傳狀態（`success`, `partial`, `failed`, `blocked` 等）、`summary` 與 `artifacts`。 | 若輸出純文字結束，任務被標記為 `protocol_incomplete` / `FailureProtocol`，觸發 1-step 的 Protocol Repair Turn。若 Repair 仍未呼叫則任務失敗。 |
| **`submit_plan`**<br>(提交執行計畫) | [`internal/team/coordinator_tools_plan.go#L14-L63`](file:///home/ubuntu/nfs/github/agent-team-cli/internal/team/coordinator_tools_plan.go#L14-L63)<br>[`internal/team/coordinator_plan.go`](file:///home/ubuntu/nfs/github/agent-team-cli/internal/team/coordinator_plan.go) | 在 `--plan` 或 `plan_first: true` 模式下，Worker 在第一步必須呼叫 `submit_plan` 提交步驟清單，嚴禁直接執行動作工具。 | 若模型忽略規範直接動手執行修改或僅給純文字，計畫審批機制失效或任務狀態卡在未審批。 |
| **Closed Tool Sequence**<br>(封閉工具序列) | [`internal/team/tool_sequence_policy.go#L88-L153`](file:///home/ubuntu/nfs/github/agent-team-cli/internal/team/tool_sequence_policy.go#L88-L153) | 任務被綁定 `taskToolSequence` 時，必須嚴格依序呼叫指定的工具與匹配的輸入參數（如 slot 1: `bash`, slot 2: `submit_result`）。 | 呼叫非預期工具會立即觸發 sequence violation 被拒絕；工具執行失敗後唯一允許的呼叫是提早回報失敗的 `submit_result`（honest early terminal）。 |
| **Artifact 落地宣告** | [`internal/team/coordinator_tools_result.go#L629-L671`](file:///home/ubuntu/nfs/github/agent-team-cli/internal/team/coordinator_tools_result.go#L629-L671) | Worker 在 `submit_result` 宣告的 `artifacts` 必須為磁碟上真實存在的 regular file。 | 若模型填寫了不存在的檔案或外部路徑，`materializeSubmittedArtifacts` 拒絕並 rollback 該次提交。 |

### 2.2 Coordinator 編排階段

| 強制步驟 / 依賴工具 | 原始碼位置 | 強制機制與要求 | 模型漏呼叫 / 誤呼叫的後果 |
|---|---|---|---|
| **`agent` 派工**<br>(Initial Batch 順序) | [`internal/team/coordinator_tools.go#L25-L182`](file:///home/ubuntu/nfs/github/agent-team-cli/internal/team/coordinator_tools.go#L25-L182)<br>[`internal/team/delegation_policy.go`](file:///home/ubuntu/nfs/github/agent-team-cli/internal/team/delegation_policy.go) | 在 `initial_pending` 階段，Coordinator 必須嚴格按照 `initial_batch` 設定的 Worker 名稱與順序派發任務。 | 派發順序錯誤或跳過 worker 會觸發 `delegationPolicyViolation`；若只輸出文字不派發則團隊停滯。 |
| **`finish`**<br>(宣告結案與交付) | [`internal/team/coordinator_tools.go#L252-L479`](file:///home/ubuntu/nfs/github/agent-team-cli/internal/team/coordinator_tools.go#L252-L479)<br>[`internal/team/coordinator_run.go#L1105-L1159`](file:///home/ubuntu/nfs/github/agent-team-cli/internal/team/coordinator_run.go#L1105-L1159) | Coordinator 結束協調時必須主動呼叫 `finish(response=...)`，不得僅在對話中輸出文字。 | 若 Coordinator 誤以為結束而輸出文字，系統會進入 wrap-up 催促；若無自動兜底條件會持續空轉直到 round 上限。 |
| **`acknowledge_failed_tasks`** | [`internal/team/coordinator_tools.go#L297-L304`](file:///home/ubuntu/nfs/github/agent-team-cli/internal/team/coordinator_tools.go#L297-L304) | 若團隊有 Worker 任務失敗或被阻塞，Coordinator 呼叫 `finish` 時必須明確傳入 `acknowledge_failed_tasks: true`。 | 若未傳入，`finish` 門禁直接阻擋並回報錯誤，要求重新處理或確認。 |
| **`approve_plan` / `modify_plan`** | [`internal/team/coordinator_tools_plan.go#L65-L264`](file:///home/ubuntu/nfs/github/agent-team-cli/internal/team/coordinator_tools_plan.go#L65-L264) | Worker 提交計畫後，Coordinator 必須主動呼叫審批工具才能解鎖 Worker 的實際執行。 | 若 Coordinator 忽視 pending plan 重新呼叫 `agent` 派工，會造成任務衝突與懸掛。 |
| **`reconcile_task`** | [`internal/team/coordinator_tools.go#L896-L966`](file:///home/ubuntu/nfs/github/agent-team-cli/internal/team/coordinator_tools.go#L896-L966) | 在 `RequireEvidenceManifest` / `StrictPolicy` 下，前期失敗的任務若已被後續任務修復，必須主動呼叫 `reconcile_task` 標記為 `superseded` 或 `reconciled`。 | 未經協調的失敗任務會永久阻擋 Strict Finish 門禁。 |

### 2.3 資源與連線清理

| 強制步驟 / 依賴工具 | 原始碼位置 | 強制機制與要求 | 模型漏呼叫 / 誤呼叫的後果 |
|---|---|---|---|
| **Terminal 關閉** | [`internal/team/coordinator_tools.go#L306-L313`](file:///home/ubuntu/nfs/github/agent-team-cli/internal/team/coordinator_tools.go#L306-L313) | 啟用 `RequireClosedTerminals` 時，呼叫 `finish` 前所有開啟的 Terminal Session 必須被關閉。 | 若殘留未關閉的終端，`finish` 被阻擋（`RequireNoLeaks` 違規）。 |

---

## 3. 機制優化方案

要徹底解決「確定性合約狀態機」與「機率性語言模型」之間的衝突，核心思路是：**將控制權與狀態判定收斂至 Runtime，減少非必要的模型主動性依賴**。

```text
┌─────────────────────────────────────────────────────────────────────────────┐
│                            優化後架構分層原則                                │
├─────────────────────────────────────────────────────────────────────────────┤
│ 1. 確定性晉升 (Deterministic Synthesis)  ── 系統客觀事實 > 模型 Tool Call   │
│ 2. 傳輸層約束 (Constrained Decoding)    ── 從 API / Grammar 消除格式錯誤    │
│ 3. 預算與生命週期解耦 (Lifecycle Decoupling) ── 執行步數與交付報告分離      │
│ 4. 智慧協調與容錯 (Smart Auto-Reconcile)  ── 降低 Coordinator 決策阻力      │
└─────────────────────────────────────────────────────────────────────────────┘
```

---

### 優化維度一：確定性自動晉升（客觀事實優先）

#### 1.1 驗證器驅動的自動 Done 晉升（Verifier-First State Transition）
- **現狀**：Worker 跑完指令且非 LLM 的 `verify`（客觀 Shell 檢查）通過（Exit 0），但 Worker 結尾忘了呼叫 `submit_result`，系統仍將其判定為 `protocol_incomplete`。
- **優化**：
  - **客觀證據高於回報協議**：當任務有綁定 `verify` 指令或 `ArtifactExpectation`，且客觀檢驗全數通過時，Runtime 直接將 Task 標記為 `TaskDone`。
  - Runtime 自動合成 `TaskResult{Status: "success", Summary: Truncate(output), Source: "verified_synthesis"}`，無需啟動額外的 Protocol Repair Turn。

#### 1.2 純文字輸出的自動封裝（Automatic Result Synthesis）
- **現狀**：Worker 正常完成工作（`finish_reason=stop` 且無 tool error），但以純文字作結，系統花費額外 1 輪 LLM Repair Turn 催促呼叫 `submit_result`。
- **優化**：
  - 若 Worker 執行過程中已產生有效動作（如調用過寫入或執行工具），且未遭遇錯誤，Runtime 直接以 Rule-based 方式抓取最後一段文字封裝為 `TaskResult`，狀態設為 `success`。
  - 僅在 Worker 既無輸出又無動作時，才判定為異常。

#### 1.3 Coordinator 的靜默結案（Auto-Finish on Quiescence）
- **現狀**：Coordinator 在所有任務完成後輸出總結文字卻未呼叫 `finish`，系統進入 wrap-up 重複催促。
- **優化**：
  - 擴展 `canDeterministicallyFinishCompletedTasks`：當 TODO List 內所有任務均已處於終態（Done / Reconciled / Skipped），若 Coordinator 本輪輸出非空文字且未呼叫任何工具，Runtime 自動視為 `finish(response=text)`，直接發起 Acceptance 驗證與結案，徹底消除空轉。

---

### 優化維度二：傳輸層硬約束（Grammar & Tool Choice）

#### 2.1 強制工具呼叫模式（`tool_choice: "required"`）
- **現狀**：在 Protocol Repair Turn 或 Plan-First 第一輪，系統依賴 Prompt 文案叮嚀模型「不要輸出文字，只能呼叫某工具」，模型仍有機率輸出文字。
- **優化**：
  - 在單一職責輪次中，於 API 請求層明確設定：
    ```json
    "tool_choice": {"type": "function", "function": {"name": "submit_result"}}
    ```
  - 從 Provider 傳輸層直接關閉自然語言輸出通道，強制模型只能填寫該工具參數。

#### 2.2 結構化輸出（Structured Outputs / JSON Schema Decoding）
- **現狀**：模型在 `submit_result` 中可能傳入型態錯誤或額外欄位，觸發 `validateToolArguments` 錯誤並進入參數修復迴圈。
- **優化**：
  - 啟用 Provider 原生支援的 `response_format: json_schema` 或 llama.cpp BNF Grammar，在 Token 採樣層保證產生的參數 100% 符合 JSON Schema，從根本杜絕語法與型態錯誤。

---

### 優化維度三：預算與資源生命週期解耦

#### 3.1 執行步數與交付步數分離（Dedicated Finalization Grace Turn）
- **現狀**：Worker 共用 `MaxSteps`（預設 30 步）。若第 29 步完成工作，第 30 步因步數耗盡被中斷，系統誤判為 `protocol incomplete` 並給予錯誤的重試提示（「Change your approach」）。
- **優化**：
  - **步數分離**：`MaxSteps` 僅計算具有實質副作用的 Action Tools（如 `bash`, `write`, `edit`）。
  - **豁免交付輪**：當執行步數達到上限或模型主動停止動作時，系統無條件額外提供一輪不受步數計費限制的 **Finalization Grace Turn**，專門用於回報成果與結構化數據。
  - **診斷修正**：若真的超時截斷，Receipt 應明確記錄 `BudgetExhausted`，Retry Prompt 提示「從上次中斷處繼續」，而非「改變做法」。

#### 3.2 資源由 Runtime 自動託管（RAII Pattern）
- **現狀**：`RequireClosedTerminals` 要求模型在 `finish` 前手動逐一關閉 Terminal Session。
- **優化**：
  - 資源生命週期綁定 Context：Terminal Session、暫存檔與 Lock 應由 Runtime 在 Task 或 Run 結束時自動執行 `defer Close()` 釋放，模型無需耗費輪次手動清理。

---

### 優化維度四：降低 Coordinator 決策摩擦

#### 4.1 `acknowledge_failed_tasks` 改為宣告式呈現
- **現狀**：有失敗任務時呼叫 `finish` 若未傳 `acknowledge_failed_tasks: true` 會被報錯阻擋。
- **優化**：
  - 不再因為未傳布林值而阻擋結案。只要 Coordinator 呼叫了 `finish`，Runtime 自動在最終 Deliverable 中附加未完成任務的診斷資訊與 Warning，避免因一個 flag 多消耗一輪 LLM。

#### 4.2 任務修復關係的自動推導（Implicit Task Reconciliation）
- **現狀**：Task 1 失敗後，Task 2 成功修復，Coordinator 必須單獨呼叫 `reconcile_task` 才能通過 Strict 門禁。
- **優化**：
  - 若 Task 2 的目標或依賴宣告包含修復 Task 1，且 Task 2 通過 Deliverable 驗證，Runtime 自動建立關聯將 Task 1 標記為 `superseded`，免除手動協調呼叫。

#### 4.3 Prompt 與 Tool Allowlist 的靜態同構編譯
- **現狀**：Prompt 要求模型「發現重要資訊請用 `stm_write` 寫入」，但該 Worker 的 runtime allowlist 並未開放 `stm_write`，導致模型遵照指令卻被攔截。
- **優化**：
  - 採用嚴格的同構編譯（Single Source of Truth）：System Prompt 的 instructions 必須由**當前 Agent 實際被允許的 Effective Tools** 動態生成，未被授權的工具與行為絕對不出現在 Prompt 中。

---

## 4. 效益與預期成果

| 指標 / 場景 | 現狀表現 | 優化後預期 |
|---|---|---|
| **純文字結束處理** | 觸發 Repair Agent，增加 1 輪 LLM 延遲與 Token 開銷；失敗則整單重試 | 透過 Verifier 或文字自動封裝直接標記 Done，0 額外延遲 |
| **步數耗盡截斷** | 誤判為 Protocol Failure，觸發錯誤的重試方針與 thrashing | 獨立 Finalization 輪次 + 正確的續作提示，大幅提升成功率 |
| **Coordinator 空轉** | 漏呼叫 `finish` 或漏帶 `acknowledge` 導致多消耗 rounds | 自動收斂與宣告式警告，徹底杜絕空轉到超時 |
| **Schema 與參數錯誤** | 透過自然語言反覆修復（1~2 次 redirects） | 傳輸層 Grammar / Tool Choice 強制鎖定，100% 格式正確 |
