# Hufu 通用任務可靠性機制設計

> 狀態：提案
> 範圍：任務契約、契約鑑別力、失敗分類、重試決策、協定復原、deadline、可觀測性
> 非範圍：任何特定 CLI、DSL、雲端服務、檔名或工作流程的專用 adapter

## 1. 目的

Hufu 應能區分下列三件事，並為它們採取不同的處置：

1. worker 的工作確實沒有完成；
2. 任務的執行或驗證契約不可成立；
3. 工作已完成，但回報協定、程序 deadline 或 session 生命周期中斷。

若把三者都表示成「task failed，交給同一個 worker retry」，會產生重複副作用、
token thrashing、誤導性的錯誤訊息，以及已完成工作被視為失敗。本文件定義可跨專案
使用的機制，不從命令、prompt 或外部工具名稱猜測專案語意。

## 2. 設計原則

1. **契約先於執行。** 可在 dispatch 前判定的問題不得消耗 worker attempt。
2. **證據先於敘述。** transcript、工具 exit code、artifact 與 verifier result 是一級資料；
   模型文字只是 claim。
3. **失敗類別決定恢復策略。** retry 是策略，不是所有錯誤的預設動作。
4. **不可重放的副作用必須保守。** protocol 或 timeout 不得自動重做 external / infrastructure /
   credential mutation。
5. **環境一致性是契約的一部分。** worker、verifier 和 acceptance command 必須記錄其工作目錄、
   shell、PATH/profile 與 timeout。
6. **保留舊格式。** 現有字串 `verify` 可轉換為 typed contract；新機制不可要求一次遷移所有 team。
7. **診斷不等於自動修正。** hufu 可以提出安全建議或阻止 dispatch；除非使用者明確設定，
   不應偷偷改寫 command、PATH 或驗收條件。
8. **契約必須有鑑別力。** 一個結構合法但在任何情況下都不會失敗的 verifier，等於沒有 verifier。
   預檢除了驗格式，必須驗「這個契約在交付物不存在或錯誤時會不會失敗」（§4.3）。

## 3. 任務生命週期

```text
task definition
  -> contract preflight
  -> dispatch worker
  -> collect execution evidence
  -> recover result protocol (if needed)
  -> verify deliverable
  -> classify outcome
  -> retry / reconcile / replan / block
```

每一階段都應輸出結構化事件。`done` 只代表 task contract 通過；run-level goal completion
仍由 acceptance/outcome evaluator 決定。

## 4. 通用任務契約預檢

### 4.1 ContractPreflight

在建立 TodoItem 或 dispatch worker 前執行 `ValidateTaskContract`。此檢查不得執行有副作用的命令，
只檢查可確定的結構與環境條件。

```go
type ContractPreflightResult struct {
    Valid       bool
    Findings    []ContractFinding
    Environment ExecutionEnvironment
}

type ContractFinding struct {
    Severity string // error, warning, info
    Code     string // verifier_invalid, executable_unresolved, deadline_conflict ...
    Field    string // verify, verify_spec, execution, timeout
    Message  string
    Hint     string
}
```

預檢應包含：

- typed verification spec 的 schema、mode 與 assertion 合法性；
- command verifier 的 shell 語法基本檢查（可選，且不得宣稱能完整驗證 shell 語意）；
- command verifier 的斷言性，即「它有沒有可能失敗」（§4.3）；
- 相對 path 是否可依既定 work directory 解析；
- command **每一個 pipeline stage** 首 token 的解析結果：PATH 可找到、project-local
  executable 存在、或無法解析。只檢查整條命令的第一個 token 不足夠：`a | b | c` 之中
  任一 stage 無法解析，都會產生一個「語法上成功、語意上無意義」的 exit code；
- worker/verification 所使用的 shell、工作目錄與 security profile；
- parent deadline 是否小於已明確宣告的 child timeout 加上 result-finalization grace；
- outcome-mode run 的 acceptance 契約是否為空（§4.4）；
- `requires_result`、`requires_verification` 與 side-effect/recovery policy 的相容性。

`executable_unresolved` 應是 error，避免派出明知必失敗的工作。若工作目錄下存在同名 executable，
可提供「改用顯式相對路徑」提示，但不得自動把 bare command 改為 `./command`。

**為何 executable 解析只能在 preflight 做。** 一旦命令以 `2>&1 | …` 形式執行，shell 的
`not found` 訊息會被折進 pipe 並被下游 stage 消化（例如被 `grep -c` 計成 0），
執行後的 stdout/stderr 不再留下任何環境失敗痕跡，只剩一個普通的非零 exit code。
這表示 §5.1 要求的「先檢查 command 是否因 shell 找不到而失敗」在事後**不可能可靠實施**；
唯一可靠的位置是 dispatch 前的 token 解析。

### 4.2 優先使用 typed verifier

字串 shell command 保持支援，但以下情況應有資料型別表示：

- path 是否存在/不存在；
- JSON scalar assertion；
- command exit expectation；
- 純 observation（僅收集證據，不能作為成功依據）。

這可減少 pipeline 的 exit-code masking，也讓 verifier 可以精確地回報「檔案不存在」而非模糊的
`exit status 1`。Shell verifier 仍適合跨多個條件的驗收，但其 stdout/stderr 必須保留。

typed verifier 只有在**成為預設路徑**時才有效。若 typed spec 僅存在於資料模型，而 coordinator
的 task schema 與 prompt 仍只提供字串 `verify`，實際採用率會是 0，所有存在性與 JSON 斷言仍會
退回 shell verifier。因此「提供 typed 選項」屬於契約設計而非遷移工作，見 §10 P0。

### 4.3 Verifier 斷言性檢查

格式合法的 verifier 仍可能在結構上不可能失敗。預檢必須拒絕這類契約，
finding code `verifier_not_asserting`，severity `error`。

至少要辨識下列反模式：

| 反模式 | 例 | 問題 |
|---|---|---|
| 尾端吞掉 exit code | `<assert> \|\| echo FAIL`、`<assert> \|\| true`、`…; exit 0` | 恆為 exit 0 |
| 最後一個 stage 是純輸出器 | `… \| <printer>`、`echo <status>`、`cat <result>` | 印出失敗仍 exit 0 |
| assertion stage 的錯誤被丟棄 | `<assert> 2>/dev/null` 且無後續判斷 | 失敗訊息與 exit code 同時消失 |
| 計數但不比較 | `… \| grep -c X` 作為唯一判斷 | 0 與 N 都可能代表成功，語意完全由 polarity 決定（§5.1） |

規則：

- 判定只做語法層，不執行命令，也不猜測業務語意；無法判定時輸出 `warning` 而非 `error`。
- 「印出狀態讓人判讀」的需求應改用 typed `json_assert`（§4.2），而不是放寬本檢查。
- observation mode 的 verifier 豁免本檢查，因為它本來就不能作為成功依據。
- 本檢查必須是靜態、可單元測試的純函式，並能透過 CLI（`hufu doctor` 類命令）單獨執行，
  讓使用者在寫 team YAML 時就得到回饋。

### 4.4 Acceptance 契約非空

outcome-mode 的 run 若 acceptance 契約為空（例如 `{}`），run-level 完成判定永遠無法成立，
所有 run 只能收斂到 `partial`。預檢必須把空契約視為 error（`acceptance_vacuous`），
而不是讓 run 以 `acceptance_state=not_configured` 靜默進行。

契約修改的 audit 事件必須能區分「未設定」與「設定為空」；`old_spec={} → new_spec={}`
這種無資訊的紀錄不可接受。

## 5. 結構化失敗分類

所有 failure 都應有 machine-readable class、phase 與 retry disposition。文字錯誤只作為 evidence。

```go
type FailureClass string

const (
    FailureContract          FailureClass = "contract"
    FailureEnvironment       FailureClass = "environment"
    FailureExecution         FailureClass = "execution"
    FailureVerification      FailureClass = "verification"
    FailureProtocol          FailureClass = "protocol"
    FailureTimeout           FailureClass = "timeout"
    FailurePolicy            FailureClass = "policy"
    FailureCancelled         FailureClass = "cancelled"
)

type RetryDisposition string

const (
    RetryNone      RetryDisposition = "none"
    RetryWorker    RetryDisposition = "retry_worker"
    ReconcileOnly  RetryDisposition = "reconcile_only"
    ReplanRequired RetryDisposition = "replan_required"
    NeedsHuman     RetryDisposition = "needs_human"
)
```

建議對應如下：

| 失敗類別 | 例子 | 預設處置 |
|---|---|---|
| `contract` | malformed verifier、無效驗證 mode、deadline conflict | `replan_required` |
| `environment` | executable unresolved、PATH/shell/workdir 不一致 | `replan_required` 或 `needs_human` |
| `execution` | 工作命令確實失敗、產物無法建立 | `retry_worker`（受 retry budget 限制） |
| `verification` | 產物存在但不符合 assertion | `retry_worker`；若 verifier 本身失效則改為 `replan_required` |
| `protocol` | 漏交 submit_result | `reconcile_only`，不可重做 worker tools |
| `timeout` | agent 或 child process deadline | 依 side effect 決定 `reconcile_only` 或一次受控 retry |
| `policy` | guard / permission / no-net 拒絕 | `replan_required` 或 `needs_human` |
| `cancelled` | 使用者取消、全域 budget | `none` |

### 5.1 避免 heuristic 誤診

不可只憑 `grep -c` 的 `stdout=0` 與 exit 1 推論 verify polarity。判斷前必須先檢查：

1. process 是否因 shell 找不到 command 而失敗；
2. stderr 是否包含 executable/shell/environment failure；
3. pipeline 是否遮蔽 upstream exit code；
4. typed verifier 是否可避免 shell pipeline。

只有在 command 已確定成功執行、且 verifier 的 assertion 邏輯明確矛盾時，才能標為
`verifier_wrong_polarity`。此規則完全通用，且能防止錯誤 hint 讓 coordinator 修錯方向。

補充兩項約束：

- 第 1、2 項在 `2>&1 | …` 的情況下事後不可靠（見 §4.1），因此 polarity 判定必須以 preflight
  的 token 解析結果為前提；preflight 未通過或未執行時，不得產生 polarity hint。
- polarity 誤判的代價不只是分類錯誤。若該 hint 同時被當成「重試無法修復」的依據而終止重試，
  一次誤判就會把錯誤結論固化成 run 的最終狀態。因此「終止重試」必須由 disposition 決定，
  不得由 hint 文字決定。

### 5.2 證據優先序

同一次驗證可能同時存在多種訊號。判定成功與失敗的優先序固定為：

1. typed assertion 結果（`file_exists`、`file_absent`、`json_assert`）；
2. verifier 自述的失敗（stdout/stderr 中結構化的失敗欄位）；
3. process exit code。

也就是說，**exit 0 但 verifier 輸出自述失敗時必須推翻 exit 0**，並以 `verification` 失敗處理，
不得降級為 warning。理由：exit code 由 wrapper 決定，自述狀態由被驗證的系統決定，
後者才是交付物的事實。

實施方式必須是結構化的，不是輸出形狀比對：

- verifier 若能輸出 JSON，契約應改用 `json_assert` 對狀態欄位斷言；
- 只有在無法取得結構化輸出時才退回文字訊號，且此時僅能產生 `weak_verifier` warning，
  不得作為 pass 的依據；
- 以 regex 比對特定工具的輸出形狀（例如某個 runner 的 `failed <n>` 字樣）是過渡手段，
  必須明確標記為最後防線，不可成為主要判定路徑，也不可用來取代 §4.3。

### 5.3 Cancelled 的處置

`cancelled` 必須與 `execution` 嚴格分離，並區分來源：

- 使用者中斷（SIGINT / 互動取消）；
- parent context cancel（上層 deadline 或 budget 造成）。

兩者的 disposition 都是 `none`，且**不得計入 retry、failure-class 統計或 anti-thrashing
fingerprint**。把取消算成執行失敗，會讓斷路器與指標同時失真，並讓下一次 run 誤以為存在一個
反覆失敗的任務。

## 6. Retry、reconcile 與 replan

### 6.1 Retry policy input

retry 決策至少依賴：

```go
type RecoveryDecisionInput struct {
    FailureClass       FailureClass
    SideEffect         SideEffectClass
    RecoveryPolicy     RecoveryPolicy
    Attempt            int
    MaxRetries         int
    EvidenceComplete   bool
    FailureFingerprint string
    PreviousFingerprint string
}
```

規則：

- 相同 `FailureFingerprint` 不得無限制重試；第一次相同重複失敗後，升級為 `replan_required`
  （計數的 scope 與強制力見 §6.2）。
- `contract`、`environment`、`policy` 預設不重新執行 worker。
- `protocol` 只允許 result-only repair；不得重放工具。
- `external_write`、`infra_mutation`、`credential_mutation` 遇 timeout 時先 reconcile。
- 僅 `none` 或可安全重放的 `workspace_write`，才可在明確 policy 下重試 execution。
- retry prompt 必須包含 class、證據、上次 command/exit、以及明確可改變的欄位；不能只說「再試一次」。

### 6.2 Fingerprint 的作用範圍與強制力

fingerprint 必須同時在三個 scope 下計數，缺任一層都會讓斷路器失效：

| Scope | key | 用途 |
|---|---|---|
| task | (task_id, digest) | 同一任務反覆以同樣方式失敗 |
| systemic | (component, operation, class, digest) | 不同任務、同一系統性缺陷 |
| run | digest | run 層級的總體重複度 |

只在 task scope 計數是常見錯誤：一個系統性缺陷（例如所有 worker 都漏交 result）會分散到 N 個
不同任務上，每個任務的 occurrences 都停在 1，於是 `repeated` 只是不斷發出警告，`limited`
永遠不會成立。規則：

- systemic scope 的同一 digest 跨越 M 個不同任務（預設 M=3）即視為 systemic，處置升級為
  `replan_required`；若 class 為 `protocol`、`environment` 或 `contract`，直接升級為 `needs_human`。
- 「升級為 replan_required」必須有可觀察的行為：停止對該 scope 派工、要求新的 recovery
  hypothesis，或要求人工授權。只寫入一個 warning 事件不算實施本節。
- **所有 enforcement 上限必須有預設值。** 以「未設定即為 0、0 表示不限制」實作的上限，
  等於預設關閉整層斷路器：計數器持續累積、warning 持續發出，但永遠不會有任何處置。
  預設必須強制，warning-only 必須是明確 opt-in。
- **對外顯示的計數必須與判定所用的計數同源。** 若判定使用 run-global 計數，事件與 UI 卻顯示
  per-task 計數，forensics 會看到「occurrences=1」而誤以為沒有重複，掩蓋真正的系統性失敗。
- fingerprint 的 normalized error 只用於 digest 穩定性；持久化時必須同時保留未正規化的原文，
  否則 forensics 會失去 task id、path 與 exit code 等關鍵欄位。

### 6.3 Reconcile 的通用語意

reconcile 不是特定工具的 probe，而是「用已宣告的 read-only evidence 判定目前目標狀態」。

- 若 objective verifier 已通過：標記 task `reconciled`，保留原 failure history。
- 若 evidence 不足：標記 `blocked` 或 `replan_required`，不可假設未完成也不可重跑副作用。
- 若 policy 有安全的 reconcile command：它必須與原 task 一起持久化，且能在 crash-resume 中重新執行。

## 7. 結果協定與證據保留

`submit_result` 是 metadata protocol，不應否定已保存的執行證據。

當 `requires_result=true` 且 worker 漏交 result：

1. 原子保存 transcript、tool calls、artifacts、exit codes、開始/結束時間與 side-effect class；
2. 將 task 標為 `protocol_incomplete`；
3. 執行最多一次、僅開放 `submit_result` 的 repair；
4. repair 成功後執行原 verifier；
5. repair 仍失敗時，以 evidence 建立 `recovered_protocol` provisional result，信心應明確標示；
6. 對不可安全重放的 task，轉入 reconcile/replan，而不是重派 worker。

repair 失敗必須帶子原因，且子原因決定下一步處置：

| 子原因 | 意義 | 處置 |
|---|---|---|
| `no_tool_call` | repair turn 沒有呼叫 `submit_result` | 保留 provisional result，`reconcile_only` |
| `invalid_schema` | 呼叫了但不符合 result schema | 允許第二次、僅修正 schema 的 repair |
| `progress_not_final` | 交出的是進度更新而非最終結果 | **重新分類為 `execution`** |

`progress_not_final` 最容易被誤分類：它在協定層看起來是 protocol 問題，但在證據層代表工作
尚未完成。若繼續當 protocol 重試，會同時污染 protocol repair 成功率與 retry 統計，
並讓真正的執行缺口被掩蓋。

資料模型應分開記錄 `execution_succeeded`、`result_protocol_complete`、`verification_passed`，
避免單一 status 欄位遺失關鍵事實。

## 8. Deadline 階層與長任務

單一 agent deadline 不足以表達長任務。hufu 應區分：

```text
run budget
  └─ coordinator turn deadline
      └─ task deadline
          ├─ worker/tool execution deadline
          ├─ verification deadline
          └─ result-finalization grace period
```

必要規則：

- task deadline 必須大於 worker execution 上限 + verification 上限 + grace；
- child command 有已知 timeout 時，contract preflight 應檢查 parent 是否能覆蓋；
- timeout 事件要記錄哪一層取消、是否有活躍 child、最後 activity 與可恢復 checkpoint；
- parent cancel 必須可靠地傳播到 child process；
- 遇到 timeout 時先持久化 receipt，讓下一次 run 能 reconcile，而非只留下 `context deadline exceeded`。

預設 grace 可採小而固定的值（例如 30–60 秒），並允許 team/CLI 覆寫。它不是延長無限執行，
而是為退出、寫 artifact、submit_result 和 verifier 排程保留空間。

### 8.1 無進展預算

時間軸的 deadline 擋不住「持續有活動但沒有進展」的 run。必須把**距上次客觀進展之後的消耗**
列為與時間並列的一層預算：

```text
no-progress budget
  ├─ tokens since last objective progress
  ├─ coordinator turns since last objective progress
  └─ tasks created since last objective progress
```

「客觀進展」指 criterion 由未通過轉為通過，或某個 objective verifier 由 fail 轉為 pass。

規則：

- 任一項超過上限即觸發 `replan_required`，再超過即停止 run 並產出 `partial` 結果與可續跑狀態。
- 這些計數器**只能由客觀進展重置**，不得由 task `done`、agent 自述成功或新任務被建立重置；
  否則一個持續產生 done 的迴圈可以無限延長 run。
- 指標存在但沒有任何機制依它動作，等於沒有這一層預算。實作必須包含 enforcement 路徑，
  不能只有計數與報告。

## 9. 可觀測性與使用者介面

每個 task failure event 至少包含：

```json
{
  "task_id": "12",
  "phase": "verification",
  "failure_class": "environment",
  "retry_disposition": "replan_required",
  "command": "tool status",
  "work_dir": "/project",
  "shell": "sh",
  "exit_code": 127,
  "stdout": "",
  "stderr": "tool: not found",
  "fingerprint": "…",
  "hint": "A project-local executable may require an explicit relative path."
}
```

TUI、text output、JSON、report、event store 與 task journal 應從同一結構渲染。這讓人與
coordinator 都可判斷下一步是 retry、replan、reconcile 還是等待人類授權。

### 9.1 事件持久化契約

- **failure event 必須自足。** 只寫入任務定義（desc / verify / status）而不寫入 `failure_class`、
  `exit_code`、`stdout`、`stderr`、`hint` 的事件，無法用於任何診斷；事後只能從錯誤字串回推。
- **不得內嵌完整任務 prompt。** 任務描述以 id 引用。把數 KB 的 prompt 複製進每一個失敗事件，
  既讓 forensics 難讀，也讓 event store 體積與後續 context 成本失控。
- **terminal 事件必須經 schema 驗證且 payload 非空。** `run_finished` 這類事件若以空 payload
  寫入，該次 run 的 outcome、stop reason 與 stats 將永久不可還原。寫入失敗必須顯性報錯，
  不可靜默降級。
- **schema 演進必須可讀。** 事件 envelope 已帶 `schema_version`；新增或改名 payload 欄位時，
  舊事件缺欄位要有明確的 reader 語意（缺失 ≠ 空值 ≠ false），避免統計把舊 run 當成 0。

## 10. 導入順序

### P0：讓契約有鑑別力、讓誤判停止

1. verifier 斷言性靜態檢查（§4.3）與 acceptance 非空檢查（§4.4）：在 dispatch 前拒絕不可能
   失敗的契約。這是唯一能阻止「假 done」的機制，優先於其他所有恢復機制。
2. 證據優先序（§5.2）：exit 0 但 verifier 自述失敗必須推翻 exit code。
3. 在 verifier 中優先辨識 command-not-found、shell/workdir errors，再評估 assertion heuristic；
   polarity hint 以 preflight 結果為前提（§5.1）。
4. 擴充 `VerificationResult` 與 task error，加入 `FailureClass`、phase、disposition，並讓
   recovery loop 尊重 disposition；`contract` / `environment` / `policy` / `cancelled` 不重派 worker。
5. 新增統一的 failure event schema、事件持久化契約與 CLI/JSON rendering（§9、§9.1）。
6. 讓 typed verifier 成為存在性與 JSON 斷言的預設路徑：coordinator 的 task schema 與 prompt
   必須先提供 typed 選項，legacy 字串只作為 fallback；採用率為 0 的 run 必須警告。

### P1：讓工作可恢復

1. 導入 contract preflight 與 environment snapshot（含 pipeline 每個 stage 的 token 解析）。
2. 對 protocol failure 保存 provisional result 與 execution receipt，並記錄 repair 子原因（§7）。
3. 導入 timeout hierarchy、grace period、無進展預算（§8.1）與 reconcile-first policy。
4. 補齊 fingerprint 的三層 scope 與預設強制（§6.2）。

### P2：逐步減少 shell verifier

1. 擴充 typed verifier 的安全、有限 assertion 集。
2. 將 CLI/team YAML legacy verify 翻譯到 typed contract。
3. 報告 typed verifier 採用率、weak verifier warnings 與 retry 效益。

## 11. 驗收測試矩陣

| 情境 | 預期結果 |
|---|---|
| bare executable 不在 PATH，workdir 有同名檔案 | `environment` + explicit-path hint；不派 worker |
| pipeline 中 upstream command not found | `environment`，不可誤判 assertion/polarity |
| malformed typed verifier | `contract`，task 不開始 |
| worker 成功寫檔但漏 submit_result | tool 不重跑；repair/recovered result 後 verify |
| 不可重放 task timeout | receipt 持久化，reconcile-first，不盲目 retry |
| read-only task timeout | 可受 budget 限制重試一次 |
| 相同 fingerprint 第二次失敗 | 停止 retry，要求 replan 或 human input |
| child timeout 等於 parent timeout | preflight 警告/error；不得在完成回報前被無理由取消 |
| observation verifier exit 0 | 保存 evidence，但不得滿足 required verification |
| verifier 尾端有 `\|\| echo` / `\|\| true` | `contract` + `verifier_not_asserting`；task 不開始 |
| verifier 最後一個 stage 只印出狀態 | `contract`；提示改用 `json_assert` |
| verifier exit 0 但輸出自述失敗 | 判為 `verification` 失敗，不得判 done |
| pipeline 中段（非首個 stage）command not found | `environment`；preflight 即攔下 |
| outcome-mode run 的 acceptance 契約為空 | `contract` + `acceptance_vacuous`；run 不開始 |
| 同一 digest 跨 3 個不同任務失敗 | systemic 升級；停止對該 scope 派工 |
| 使用者 SIGINT | `cancelled`；不計入 retry / fingerprint / 指標 |
| repair 交出進度更新而非最終結果 | 重新分類為 `execution`，不累積 protocol 失敗 |
| run 在無客觀進展下超過 token 預算 | 觸發 replan，再超過即停止並輸出 partial |
| `run_finished` 以空 payload 寫入 | 視為寫入錯誤並顯性報告，不可靜默接受 |

## 12. 成功指標

新增或延伸 run metrics：

- failures by class / phase；
- retry attempts avoided by disposition；
- protocol repairs succeeded without tool replay；
- protocol repair failures by sub-reason（§7）；
- timeout tasks recovered through reconciliation；
- preflight failures caught before dispatch；
- non-asserting verifiers rejected at preflight（§4.3）；
- verifications overturned by evidence precedence（exit 0 → fail，§5.2）；
- typed verifier adoption rate（每 run 的 typed / 全部帶 verifier 的任務數）；
- tasks accepted as done without any objective verifier；
- systemic fingerprints escalated（跨任務同 digest，§6.2）；
- repeated-failure fingerprints stopped；
- tokens / turns since last objective progress at run end（§8.1）；
- cancelled tasks excluded from retry statistics（§5.3）；
- verifier false-diagnosis rate（人工標記或 regression test 驗證）。

成功不是把所有 run 都重試到完成，而是讓每個未完成 run 都有正確、可重現、可行動的狀態，
並確保 hufu 不會因自身的契約或協定問題重做已造成副作用的工作。

## 13. 本文件規則的鑑識依據

§4.3、§4.4、§5.2、§5.3、§6.2、§7 的 repair 子原因、§8.1、§9.1 是對一份真實多-run 工作區
（8 次 run、674 筆 event、115 筆 task journal）鑑識後補上的。觀察到的通用現象：

- 81 個任務中僅 30 個帶 verifier、**0 個使用 typed spec**；那 30 個之中有 28 個在結構上
  不可能失敗（尾端 `|| echo`、或最後一個 stage 只印出狀態）。
- 已記錄的驗證結果中，6 筆為 `exit_code=0` 且 stdout 自述失敗、1 筆為 `exit_code=0` 且
  stdout 顯示計數為 0，全部被判為通過。文字型 weak-verifier 檢查在這批資料上命中 0 次。
- 一次 executable 未解析（shell 回報 `not found`）被誤判為 verifier polarity 錯誤，
  並據此終止重試，把錯誤結論固化為最終狀態。
- 同一個 protocol digest 出現 13 次，斷路器發出 10 次 `repeated` 警告，但 `limited` 從未成立：
  強制上限沒有預設值（未在 team YAML 明示時為 0，而 0 被解讀為不限制），整層 enforcement 為
  inert。同時事件顯示的 `occurrences` 取自 per-task 計數（恆為 1），與判定所用的 run-global
  計數不同源，使 forensics 誤以為沒有重複。
- 兩次 run 在零 criterion 進展的情況下分別消耗約 8.9M 與 7.7M token。
- protocol repair 成功率為 2/8 與 6/12，且失敗原因未分類。
- 4 筆 `run_finished` 以空 payload 寫入，該 4 次 run 的 outcome 無法還原。
- acceptance 契約以 `{} → {}` 被「修改」並接受，之後所有 run 的 acceptance 狀態均為
  not_configured，outcome 永遠是 partial。

這些現象都不依賴特定專案、CLI 或工作流程，因此對應規則寫在本文件，而非任何 adapter。

## 14. 工作包

每個工作包（WP）都設計為**一個 commit**：可獨立建置、獨立測試、獨立 review，且不依賴尚未合併的
後續 WP。純函式先行、接線在後，讓高風險的行為改變集中在少數幾個 commit。

### 14.1 每個 WP 的共同完成定義

- `go build ./...`、`go test ./...`、`go vet ./...` 全綠。
- 新增或修改的判定邏輯必須是 table-driven 測試；錯誤一律 `fmt.Errorf("doing X: %w", err)` 包裝。
- 單一檔案維持 < 800 行；超過就拆檔（見 `CLAUDE.md`）。
- 若 WP 改變既有 team YAML 的行為，必須提供過渡開關並在 commit message 的 body 說明。
- commit message 用表格中的標題，body 以 `Refs: docs/hufu-generic-task-reliability-mechanisms.md §X` 指回規則。

### 14.2 相依順序

```text
WP-01 ─┬─> WP-02 ─────────────────────────────┐
WP-03 ─┘                                      │
WP-04 ─┬─> WP-05 ─┬─> WP-06                   ├─> WP-17 ─> WP-18
       │          ├─> WP-08 <── WP-07         │
       │          ├─> WP-11                   │
       │          └─> WP-13 <── WP-07         │
WP-09 ─┴─> WP-10                              │
       └─> WP-12 ────────────────────────────-┘
WP-14、WP-15、WP-16 無相依，可任意插入
```

### 14.3 工作包清單

| WP | Commit 標題 | 規則 | 風險 |
|---|---|---|---|
| 01 | `feat(team): add ContractFinding and verifier assertiveness lint` | §4.3 | 低（純新增） |
| 02 | `feat(team): reject non-asserting verifiers before dispatch` | §4.3, §4.1 | **高**（可能拒絕既有 YAML） |
| 03 | `feat(team): overturn verifier exit 0 on self-reported failure` | §5.2 | 中 |
| 04 | `feat(team): resolve executables per pipeline stage` | §4.1 | 低（純新增） |
| 05 | `feat(team): add contract, environment and cancelled failure classes` | §5, §5.3 | 中 |
| 06 | `fix(team): gate polarity hint on executable resolution` | §5.1 | 中 |
| 07 | `feat(team): add RetryDisposition decision function` | §5, §6.1 | 低（純新增） |
| 08 | `refactor(team): route retry loop through RetryDisposition` | §6.1 | **高**（行為等價性） |
| 09 | `feat(team): default and enforce anti-thrashing limits` | §6.2 | **高**（預設值翻轉） |
| 10 | `feat(team): count systemic failure fingerprints across tasks` | §6.2 | 中 |
| 11 | `feat(team): record protocol repair failure sub-reasons` | §7 | 低 |
| 12 | `feat(team): enforce no-progress budget` | §8.1 | 中 |
| 13 | `feat(team): make failure events self-contained` | §9, §9.1 | 中（event replay 相容性） |
| 14 | `fix(team): reject terminal events with empty payload` | §9.1 | 低 |
| 15 | `feat(team): reject vacuous acceptance contracts` | §4.4 | 中 |
| 16 | `feat(team): prefer typed verification in task authoring` | §4.2, §10 P0-6 | 低 |
| 17 | `feat(team): report contract and verifier reliability metrics` | §12 | 低 |
| 18 | `feat(cli): lint task contracts in hufu doctor` | §4.3 | 低 |

### 14.4 各工作包內容

**WP-01 — verifier 斷言性 lint（純函式）**
- 新增 `internal/team/contract_finding.go`：`ContractFinding`、severity 與 code 常數（`verifier_not_asserting`、`executable_unresolved`、`acceptance_vacuous`…）。
- 新增 `internal/team/verifier_lint.go`：`LintVerifier(spec VerificationSpec, legacyCommand string) []ContractFinding`，切 pipeline stage 並辨識 §4.3 四類反模式；observation mode 直接回空。
- 測試：`verifier_lint_test.go`，每一類反模式與其正例各一組；含「無法判定 → warning」案例。
- 不接線，行為零改變。

**WP-02 — 在 dispatch 前拒絕不可能失敗的契約**
- `internal/team/execution_contract.go`：`ValidateExecutionContract` 擴充為對**所有** `ExecutionKind` 驗證 typed spec 與 lint 結果（目前只在 `interactive`/`external` 且 `requires_verification` 時驗），舊函式名保留為 wrapper。
- 呼叫點已存在：`coordinator_task_run.go:26`、`coordinator_execute.go:55` 與 `:360`。
- error findings → 以 `contract` class 記錄並**不派工**；warning → 發事件後照常派工。
- 過渡開關：`reliability.verifier-lint: error|warn|off`，預設 `error`。
- 驗收：§11 的「尾端 `|| echo`」、「最後 stage 只印出狀態」、「malformed typed verifier」三列；且 inline kind 的 malformed spec 也必須在派工前被攔下。

**WP-03 — 證據優先序**
- `internal/team/verification.go`：把目前散在 `else` 分支的 `CheckDefinitiveVerifierFailure` / `CheckWeakVerifierWarning` 收斂為 `applyEvidencePrecedence(res, err, spec)`，明確實作 §5.2 的三層優先序。
- `internal/team/status.go` 的 `VerificationResult` 加 `Overturned bool` 與 `OverturnReason string`。
- 一併收進工作區目前未 commit 的 `CheckDefinitiveVerifierFailure` 改動。
- 驗收：§11 的「exit 0 但輸出自述失敗」列；observation mode 不受影響。

**WP-04 — pipeline 每個 stage 的 executable 解析（純函式）**
- 新增 `internal/team/command_resolve.go`：`SplitPipelineStages(command) []string`、`ResolveStageExecutables(stages, workDir) []ContractFinding`，回報 PATH 命中、project-local 同名檔（附 explicit-path hint）、或 `executable_unresolved`。
- 不執行任何命令；不得自動改寫 command。
- 不接線。

**WP-05 — 新增 contract / environment / cancelled 失敗類別**
- `internal/team/run_result.go:598`：`TaskFailureClass` 補三個值。
- `internal/team/coordinator_task_run.go:772` 的 `classifyTaskFailure` 改為以結構化輸入為主（exit code、resolve findings、context 取消原因），文字比對降為 fallback。
- SIGINT 與 `context.Canceled` → `FailureCancelled`，並在 `metrics.go:104` 的 `recordRetry` 與 `coordinator_failure.go` 的 fingerprint 路徑排除（§5.3）。
- 驗收：§11 的「使用者 SIGINT」列。

**WP-06 — polarity hint 以解析結果為前提**
- `internal/team/verification.go:493-502`：polarity 分支只在「所有 stage token 均已解析」時才可產生；未解析 → `environment` + explicit-path hint。
- `isUnfixableVerifyFailure` 不再由 hint 文字觸發。
- 迴歸測試（本次鑑識的實際案例）：`<missing> show 2>&1 | grep -c running` 必須得到 `environment`、輸出不含 polarity 字樣、且不終止重試。
- 驗收：§11 的「pipeline 中段 command not found」列。

**WP-07 — RetryDisposition 決策函式（純函式）**
- 新增 `internal/team/disposition.go`：`RetryDisposition` 常數與 `DecideRecovery(RecoveryDecisionInput) (RetryDisposition, string)`，實作 §5 對應表與 §6.1 規則。
- 測試直接以 §11 矩陣的「預期結果」欄為案例來源。
- 不接線。

**WP-08 — retry loop 改由 disposition 驅動**
- `internal/team/coordinator_task_run.go` 的 retry loop（約 292–770 行）：現有五個 early-break（`terminalBlocked`、`protocolFailure`、`CanAutomaticallyReplay`、`isUnfixableVerifyFailure`、`sameFailure`）改為 `DecideRecovery` 的輸入，由單一決策點決定 retry / break / block。
- 同 commit 前半先補這五條路徑的 characterization test，確保重構前後行為等價。
- 風險最高；若 loop 因此超過 800 行，一併拆出 `coordinator_task_retry.go`。

**WP-09 — anti-thrashing 預設值與強制**
- `internal/agent/agent.go:169`：新增 `DefaultReliabilityConfig()`；`internal/team/parse.go:604` 的 `cfg.Reliability = yc.Reliability` 改為套用預設後再覆寫。
- `HardEnforcement` 預設 true，新增 `warn-only` 明確 opt-in。
- `internal/team/anti_thrashing.go:194`：判定與對外顯示同源——事件與 UI 改用 run-global `Counts[digest]`，不再顯示 per-task `occurrences`。
- 驗收：未設定任何 YAML 上限時，同 digest 第二次失敗即 `limited`；warn-only 時只警告不封鎖。

**WP-10 — systemic scope 計數**
- `internal/team/anti_thrashing.go`：新增 `(component, operation, class, digest)` 的 systemic 計數與 `MaxSystemicFailureTasks`（預設 3），含 `rebuild` / resume 支援。
- 達標時依 class 升級：`protocol` / `environment` / `contract` → `needs_human`，其餘 → `replan_required`。
- 驗收：§11 的「同一 digest 跨 3 個不同任務失敗」列。

**WP-11 — repair 失敗子原因**
- `internal/team/coordinator_task_run.go:550-565`：`RepairProvenance` 加 `FailureReason`；依 §7 表格判定 `no_tool_call` / `invalid_schema` / `progress_not_final`。
- `progress_not_final` 改判 `FailureExecution`，不計入 protocol repair 統計。
- 測試補在 `internal/team/protocol_repair_test.go`。
- 驗收：§11 的「repair 交出進度更新」列。

**WP-12 — 無進展預算**
- `ReliabilityConfig` 加 `MaxTokensWithoutProgress` / `MaxTurnsWithoutProgress` / `MaxTasksWithoutProgress`（皆有預設）。
- `internal/team/coordinator_run.go`：在 task 結束與 turn 邊界檢查，先 `replan_required`、再停止並輸出 `partial` + continuation。
- 計數只由 criterion 進展重置，沿用既有的 `resetAfterCriterionProgress` 路徑；task `done` 不重置。
- 驗收：§11 的「無客觀進展下超過 token 預算」列。

**WP-13 — failure event 自足化**
- `internal/team/coordinator_failure.go`：`PersistFailure` / `FailureDetail` 改發 §9 的結構化 payload（class / phase / disposition / command / work_dir / shell / exit_code / stdout / stderr / fingerprint / hint）。
- `task_failed` payload 不再內嵌完整 `desc`，改為 task id + 截斷 summary。
- 相容性必做：`internal/team/event_reducers.go:309` 依 payload 重建 TodoItem，需新增 replay 測試確認舊格式與新格式都能還原（缺欄位 ≠ 空值）。

**WP-14 — 拒絕空 payload 的 terminal 事件**
- `internal/team/coordinator_eventstore.go:95` 的 `emitEvent` 與 run-finished 寫入路徑：terminal 事件 payload 為空即回錯並顯性報告，不得靜默寫入。
- 驗收：§11 的「`run_finished` 以空 payload 寫入」列。

**WP-15 — 拒絕空 acceptance 契約**
- `internal/team/criteria.go`、`set_acceptance` 工具與 `internal/team/parse.go`：outcome-mode 的空契約回 `acceptance_vacuous` error。
- acceptance audit 事件區分「未設定」與「設為空」。
- 驗收：§11 的「acceptance 契約為空」列。

**WP-16 — 讓 typed verification 成為預設書寫路徑**
- `internal/team/coordinator.go:969-998`：調整 `verify` 與 `verify_spec` 的 description，明示 `verify` 僅在無法用 typed spec 表達時使用；`internal/team/coordinator_prompt.go:140` 同步。
- 新增保守的 legacy→typed 翻譯，只處理可無歧義對應的形狀（如 `test -f X` → `file_exists`）。
- 驗收：翻譯不得在有歧義時發生；typed adoption 指標在測試中可觀察。

**WP-17 — 契約與 verifier 可靠性指標**
- `internal/team/run_result.go:544` 的 `RunMetrics` 加入 §12 新增項；`internal/team/metrics.go` 累計；`cmd/hufu/report.go:215` 與 `cmd/hufu/fix.go:81` 渲染。
- typed adoption 為 0 的 run 必須在 report 輸出警告。

**WP-18 — `hufu doctor` 契約 lint**
- `cmd/hufu/doctor.go`：對 team YAML 的 tasks 與 acceptance criteria 執行 WP-01 / WP-04 的純函式並輸出 findings，讓使用者在 run 之前就得到回饋（§4.3 最後一條規則）。
