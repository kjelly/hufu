# Hufu 通用 Workset 與結構化證據修正計畫

> 狀態：提案
> 範圍：批次工作展開、artifact 身分、typed result 驗證、整組完成判定、事件與報表投影
> Consumer 遷移計畫：[hufu-code-review-migration-plan.md](hufu-code-review-migration-plan.md)
> 核心原則：Hufu runtime 不得認識 team 名稱、特定 VCS、特定工作類型、consumer item 命名、呈現格式、consumer 路徑或 consumer 輸出欄位。

## 1. 目的

建立一組可供任何批次型 agent team 使用的通用 runtime 機制，讓 team 不必以 Bash 重新解析 task journal、tool transcript 或自由文字來回答下列問題：

1. producer 本輪實際產生了哪些工作項目；
2. fan-out 是否完整且沒有漏項、重複或使用過期 manifest；
3. 每個 child task 是否觀察了宣告的輸入；
4. worker 的 typed result 是否符合 consumer 宣告的結構要求；
5. 整組工作是否全部 terminal、verified，且可安全進入 acceptance；
6. retry、resume 或 protocol repair 是否仍綁定同一 run、attempt 與輸入世代。

相同機制至少必須能支援多個互不相關的 consumer，例如：

- 測試矩陣：producer 列出 package/shard，worker 執行每一列，最後要求全部 verified；
- 安全掃描：producer 列出掃描目標與 rule set，worker 回傳 typed findings；
- 文件或資料處理：producer 列出 input artifact，worker 逐項轉換或分析；
- 多環境檢查：producer 列出 environment/resource，worker 逐項執行 read-only probe。

若一項 runtime 設計只能用單一 consumer 的詞彙描述，該設計不得進入 Hufu core。

## 2. 問題與通用 root cause

### 2.1 Fan-out 只有資料展開，沒有 durable group identity

目前 `FanOutSpec` 從 workspace TSV 讀取資料並展開 child tasks，但展開結果沒有一個 canonical、可持久化的 parent/group receipt。Runtime 因此無法直接證明：

- source 是否由本輪已驗證的 producer 產生；
- source digest、row count 與 item keys 是什麼；
- 哪些 child task 對應哪些 source rows；
- resume 後的 child set 是否與原本完全相同；
- acceptance 時是否每個 source item 都有且只有一個有效 terminal result。

Consumer 只好另外建立 marker、checksum 與 coverage script。所有 data-driven fan-out 都會遇到這個問題。

### 2.2 Verification 無法直接斷言 submitted `TaskResult`

現有 `tool_call_assert` 能檢查本 attempt 的 tool receipts，但 consumer 若要要求 `TaskResult.FilesRead`、`Findings`、`Details` 或其他 typed 欄位，只能解析序列化 transcript 或 Markdown。這會將 JSON escaping、輸出格式和 provider 措辭變成完成條件。

Runtime 應驗證 canonical `TaskResult`，而不是再次解析它曾經如何出現在 transcript 中。

### 2.3 Artifact path 與 artifact identity 混用

目前 fan-out source 是 workspace-relative path。路徑本身不能證明 producer、run、attempt、digest 或 freshness。`ArtifactRef` 已具備 identity 與 SHA256 欄位，但 fan-out 尚未以 artifact reference 作為主要資料來源。

### 2.4 Aggregate completion 被外包給 shell

`require-no-unresolved-tasks` 只能證明目前已建立的 tasks 都 terminal，無法單獨證明「manifest 中的每一項都正確建立成 task」。缺少 expansion receipt 時，漏展開一列仍可能看起來沒有 unresolved task。

### 2.5 Presentation contract 被誤當 execution contract

Markdown heading、特定字詞或報告段落是 presentation concern，不應決定 task 是否完成。Runtime 應驗證 typed fields、artifacts、receipts 與 task state；最終 Markdown 由 coordinator/rendering layer產生。

## 3. 設計原則

1. **延伸既有 abstraction，不建立平行系統。** 使用 `TaskResult`、`ArtifactRef`、`VerificationSpec`、`FanOutSpec`、`ExecutionReceipt`、event store 與 acceptance criteria。
2. **Artifact identity 優先於 path。** Runtime 以 opaque artifact ID、digest、run/task/attempt identity 判定來源；path 只由 workspace adapter 解決。
3. **結構化證據優先於 transcript。** Transcript 保留作診斷，不作 required acceptance 的主要 parser input。
4. **展開一次、持久化一次。** Workset 展開後建立 immutable receipt；resume 重播 receipt，不重新讀取可能已變更的 source。
5. **全部或完全不展開。** Manifest schema、item key、引用或 template 綁定任何一項無效時，不建立部分 child tasks。
6. **驗證失敗不可被 prose 覆蓋。** Worker 的成功文字不能推翻 failed verification 或 incomplete group。
7. **副作用與 recovery 保持原語意。** 新機制不得讓 protocol repair 重播 external/infra/credential mutation。
8. **Core 只理解通用資料契約。** Git range、diff、review lens、Markdown 欄位及 consumer 命令全部留在 team adapter/config。

## 4. 目標模型

### 4.1 Artifact-backed workset

新增 provider-neutral workset envelope。第一版使用 JSON；TSV 保留為相容輸入，但進入 runtime 後正規化成同一模型。

```go
type WorksetManifest struct {
    SchemaVersion int           `json:"schema_version"`
    Items         []WorksetItem `json:"items"`
}

type WorksetItem struct {
    Key      string            `json:"key"`
    Bindings map[string]string `json:"bindings,omitempty"`
    Inputs   []ArtifactRef     `json:"inputs,omitempty"`
}
```

限制：

- `Key` 在同一 manifest 內唯一、非空且有長度上限；
- `Bindings` 只能是 bounded scalar strings，不允許任意巢狀物件；
- `Inputs` 必須是已登錄、workspace-scoped、屬於允許 producer/run 的 artifacts；
- manifest 本身必須以 `ArtifactRef` 傳入，runtime 驗證 SHA256 與 freshness；
- item 數量與總 decoded bytes 使用現有 fan-out safety limits 或更嚴格上限。

### 4.2 Expansion receipt

每次展開產生 immutable receipt：

```go
type WorksetExpansionReceipt struct {
    RunID            string            `json:"run_id"`
    ParentTaskID     string            `json:"parent_task_id"`
    SourceArtifactID string            `json:"source_artifact_id"`
    SourceSHA256     string            `json:"source_sha256"`
    ItemCount        int               `json:"item_count"`
    ItemKeysSHA256   string            `json:"item_keys_sha256"`
    Children         map[string]string `json:"children"` // item key -> task ID
}
```

Receipt 建立後：

- source artifact 改變時不得靜默重展開；
- resume 必須從 receipt 重建 child mapping；
- retry 只建立新的 child attempt，不建立第二個 child identity；
- event store、session、task journal、JSON output 與 report 都能投影 group 狀態；
- raw manifest content 不寫入 event payload，只保存 artifact ID、digest、count 與 bounded key digest。

### 4.3 `TaskResult` assertion verifier

在 `VerificationSpec` 新增 `task_result_assert`，直接對已驗證 schema 的 canonical `TaskResult` 執行 bounded assertions。

```go
type TaskResultAssertion struct {
    Pointer string `json:"pointer" yaml:"pointer"`
    Op      string `json:"op" yaml:"op"`
    Value   any    `json:"value,omitempty" yaml:"value,omitempty"`
}
```

第一版只支援可明確 fail-closed 的操作：

- `exists`
- `non_empty`
- `equals`
- `min_items`
- `contains_scalar`

`Pointer` 使用 RFC 6901 JSON Pointer，目標是 marshal 後的 `TaskResult`。不支援 regex、任意 scripting、Markdown parsing 或 consumer-defined executable。

驗證時機：worker 提交 schema-valid `TaskResult` 後、task 轉為 done 前。`partial`、`failed`、`blocked` 仍依既有 recovery semantics 處理，不因 assertions 而偽裝成 schema error。

### 4.4 Work-item binding

`FanOutSpec` 擴充 artifact source 與 bindings template；舊 `source` path 保留相容性。

```go
type FanOutSpec struct {
    Source         string `json:"source,omitempty"`
    SourceArtifact FactRef `json:"source_artifact,omitempty"`
    GoalTemplate   string `json:"goal_template"`
}
```

每個 child task 保存自己的 immutable `WorksetBinding`，供下列位置使用：

- goal/constraints template；
- static verification assertion values；
- report/debug projection；
- resume identity check。

Template 只替換完整 scalar value，不把 bindings 拼入 shell command，也不進行遞迴 template expansion。

### 4.5 Group completion verifier

新增 `workset_complete` verification type，直接讀 canonical expansion receipt 與 child states：

```yaml
type: workset_complete
source-task: prepare
require-all-terminal: true
require-all-verified: true
accepted-statuses: [success, completed_with_gaps]
```

成功條件：

1. receipt 的 source artifact identity 與 digest 仍有效；
2. receipt item count 等於 child mapping count；
3. 每個 item key 恰好對應一個 child task；
4. 每個 child terminal 且符合 required verification；
5. 沒有額外、未綁定到 receipt 的 child 被算入成功；
6. cancelled、partial、failed、blocked 或 verification failure 不得通過。

`completed_with_gaps` 是否可接受由 team contract 明確宣告；runtime 不推測其 domain 意義。

### 4.6 Action provider artifact ingestion

沿用現有 `ActionProvider` 與 command adapter，不新增 Git 或 review tool。定義通用 provider result envelope：

```go
type ActionResult struct {
    Outputs   map[string]any `json:"outputs,omitempty"`
    Artifacts []ArtifactRef  `json:"artifacts,omitempty"`
}
```

Command adapter 回傳的 artifact path 必須由 runtime：

- 驗證位於允許 workspace；
- 開檔計算 digest/bytes，而非信任 adapter 自報；
- 綁定 run/task/attempt/provider identity；
- 發出 artifact/receipt events；
- 將 opaque artifact reference 傳給後續 fan-out。

Consumer-specific producer 因此可以是 Go、Rust、Python、MCP 或其他 adapter；Hufu core 不需要理解其輸入格式或命令語意。

## 5. 端到端流程

```text
static producer task/action
        │
        ▼
ActionProvider ──► ActionResult + registered manifest artifact
        │
        ▼
validate WorksetManifest atomically
        │
        ▼
persist WorksetExpansionReceipt
        │
        ▼
create child tasks with immutable item bindings
        │
        ├── worker tool receipts
        ├── submitted TaskResult
        ├── tool_call_assert / task_result_assert
        └── normal retry/recovery
        │
        ▼
workset_complete verifier
        │
        ▼
acceptance / RunOutcomeEvaluator
```

Coordinator 只決定何時委派與如何綜合結果，不負責重打 paths、計數 rows、建立 marker 或解析 transcript。

## 6. 工作包

### WP-0：Characterization 與通用測試 fixture

**目的**：在修改前固定現有 fan-out、typed verification、retry、resume 與 report 行為。

**主要檔案**：

- `internal/team/fan_out_test.go`
- `internal/team/fact_refs_test.go`
- `internal/team/verification_test.go`
- `internal/team/verification_integration_test.go`
- `internal/team/coordinator_terminal_test.go`
- `cmd/hufu/report_test.go`

**測試 fixture**：使用 `alpha/beta/gamma` work items、臨時 workspace 與 fake provider；不得出現 team 名稱、Git SHA、`batch-XXXX`、review heading 或真實 consumer path。

**完成條件**：測試能重現漏列、重複 key、stale source、child verification failure、budget cancellation 與 resume。

### WP-1：`task_result_assert`

**目的**：以 typed verifier 取代 transcript/Markdown parser。

**主要修改面**：

- `internal/agent/agent.go`：verification schema；
- `internal/team/verification.go`：normalize、validate、execute、fingerprint；
- `internal/team/coordinator_task_run.go`：將 canonical `TaskResult` 傳入 verification context；
- `internal/team/team_policy_lint.go`：靜態 lint；
- parser、JSON schema 與 contract hash tests。

**必要測試**：

- 每個 operator 的 success/failure；
- missing pointer、wrong type、oversized assertion；
- `partial/blocked` 不被改分類為 schema failure；
- protocol-only repair 的 result 仍遵守 `requires-grounded-result`；
- verifier fingerprint 對 assertion 順序穩定；
- acceptance context 若沒有 task result 必須 fail closed。

### WP-2：Artifact-backed fan-out 與 expansion receipt

**目的**：讓 workset source、row identity 與 child mapping 成為 durable runtime state。

**主要修改面**：

- `internal/team/coordinator.go`：`FanOutSpec`/binding schema；
- `internal/team/fan_out.go`：manifest normalization 與 atomic expansion；
- `internal/team/coordinator_execute.go`：task ID 分配與 receipt commit；
- `internal/team/event_payloads.go`、event reducers、session data；
- `internal/team/run_result.go`：group projection types。

**必要測試**：

- duplicate/empty item key、schema mismatch、超過 row/byte limit；
- artifact 不屬於本 run、digest mismatch、workspace escape；
- 建立任一 child 前發現錯誤時零 child commit；
- receipt commit 後 source 變更不影響 resume；
- resume 不重建不同 task IDs；
- task retry 保留 item key，且不重展開 group。

### WP-3：Action provider artifact ingestion

**目的**：讓 deterministic producer 不需要 LLM 把 path、digest 或 artifact metadata 重新寫進結果。

**主要修改面**：

- `internal/team/action_provider.go`；
- `internal/team/coordinator_structured_execution.go`；
- artifact/evidence store 與 execution receipt；
- provider preflight、redaction 與 workspace policy。

**必要測試**：

- valid JSON envelope 與多 artifact；
- invalid JSON、undeclared output、missing file；
- symlink/path escape、artifact replaced during hashing；
- adapter 自報 SHA 與 runtime digest 不同；
- stdout/stderr secret redaction；
- timeout/cancel 保留 failure receipt，且不登錄半成品 artifact。

### WP-4：`workset_complete` 與 observable projections

**目的**：用 canonical group state 取代 marker/checksum coverage script。

**主要修改面**：

- `internal/team/verification.go`；
- acceptance criteria 與 `RunOutcomeEvaluator`；
- event store/reducer、task journal、session checkpoint；
- `cmd/hufu/json_output.go`、`report.go`、`display.go`；
- TUI reporter若顯示 group progress，新增對應 `tea.Msg` 與純 `Update()` 測試。

**投影至少包含**：source artifact ID/digest、expected/completed/verified/failed counts、group state。不得包含完整 manifest 或未 redacted worker output。

**必要測試**：

- 0-item workset 的明確語意；
- all verified、one failed、one partial、one cancelled；
- child 數量與 receipt 不符；
- acceptance failure 不被 coordinator prose 覆蓋；
- JSON、text report、resume projection 一致；
- event replay 後 group state 與 live run 相同。

### WP-5：靜態 contract 與 policy preflight

**目的**：在任何 model/action 執行前拒絕不可能成立的配置。

**規則**：

- `source` 與 `source_artifact` 互斥；
- `workset_complete` 必須引用一個可產生 expansion receipt 的 task；
- result assertions 只能使用支援的 pointer/operator；
- required verification 的 child contract 必須實際綁到 fan-out task；
- action provider capability 必須存在且 producer side-effect/recovery 相容；
- unattended workset 必須有 budget 與非空 blocking acceptance；
- consumer template 不得將 binding 拼入 command 字串。

**工具輸出**：`hufu team validate`、`hufu doctor` 與 dry-run 應顯示相同 finding codes，不各自實作不同規則。

### WP-6：Consumer migration contract

只有 WP-1 至 WP-5 完成並以至少兩個通用 fixture 驗證後，consumer 才能遷移到新 contract。每個 consumer 的 domain producer、角色拓撲、輸出格式、驗收內容與 workaround 移除清單必須放在自己的遷移文件，不得回填到 runtime plan。

首個具體遷移計畫見 [hufu-code-review-migration-plan.md](hufu-code-review-migration-plan.md)。該文件不得成為 core implementation 的輸入 schema；它只是 generic API 的 consumer。

### WP-7：移除 legacy workaround 與相容層評估

通過 shadow/E2E 後才刪除 consumer workaround。舊 path-based TSV fan-out 至少保留一個 release cycle並標示 deprecated；不得在同一變更中無條件破壞既有 teams。

刪除前必須保存一組 migration fixture，證明舊/new fan-out 的 item ordering、template substitution 與 failure semantics 可比較。

## 7. 明確的 Core / Consumer 邊界

### Hufu core 可以知道

- workset、item key、scalar bindings；
- opaque artifact identity、digest、producer/run/task/attempt；
- child task mapping；
- typed result JSON pointers；
- tool receipts、verification result、group completion；
- retry、recovery、acceptance 與 projection。

### Hufu core 不得知道

- 任何 team/agent 名稱；
- 特定 VCS、revision、patch 或 domain record；
- consumer item 命名規則；
- consumer manifest、evidence 或 output 檔名；
- consumer severity taxonomy；
- 呈現格式或特定 provider 的文字；
- consumer workspace 的固定絕對/相對路徑；
- 如何找 caller、test、security boundary 或 TUI code。

Core tests 若需要上述概念，表示 abstraction 已洩漏，必須退回設計階段。

## 8. Failure semantics

| 情境 | 結果 |
|---|---|
| producer/action validation 失敗 | permanent contract/action failure；不建立 workset |
| manifest schema/digest 無效 | fail closed；零 child commit |
| child 漏交 result | 既有 protocol-only repair；不可重展開 workset |
| child result assertion 失敗 | verification failure；依 task recovery，而非 schema repair |
| child tool observation 不足 | verification failure；不得以 result prose 補足 |
| child partial/blocked | 保留 typed evidence；group incomplete/failed |
| retry | 同 child ID、增加 attempt；維持 item binding |
| crash-resume | 從 expansion receipt 重建，不重新讀 producer source |
| source artifact 被替換 | digest mismatch；group 不得完成 |
| budget exceeded/cancel | 尚未 terminal 的 child 保持明確狀態；acceptance 不通過 |
| report renderer 失敗 | projection/report failure；不得改寫 canonical group outcome |

## 9. 安全與資料治理

- Workset manifest 與 provider output 都受 workspace path policy、size limit及 redaction。
- Artifact hashing 必須避免 symlink race；沿用/擴充既有安全開檔方式，不先 `stat` 後無保護地重開。
- Event payload只保存 digest、counts、IDs與 bounded previews。
- `TaskResult` assertion 不允許 regex DoS、script execution 或任意 reflection method。
- Binding 不可被直接插入 shell；action payload必須保持結構化 JSON。
- Tool authorization仍經中央 policy gate；workset binding不能增加 worker tool grants。
- `completed_with_gaps` 不可默認等同 success，必須由 group contract列入 accepted statuses。

## 10. Rollout

1. `legacy`：既有 path/TSV fan-out 行為不變。
2. `shadow`：新 expansion receipt與group projection並行計算，但不影響 outcome；比較 item/task/result parity。
3. `enforce`：artifact-backed teams由新 verifier決定 acceptance。
4. `migrate`：遷移至少兩個獨立 consumer fixture，再開放實際 consumer 採用。
5. `deprecate`：記錄舊 path-based fan-out使用量；一個 release cycle後再決定移除時程。

不得以 team 名稱切換 rollout；使用 schema/contract feature flag 或顯式 manifest版本。

## 11. 驗證矩陣

### Runtime unit/integration

```bash
go test ./internal/team/...
go test ./cmd/hufu/...
go test -race ./internal/team/...
go vet ./...
golangci-lint run
```

此外必須執行 `go test ./...`。上述命令需分開執行並保留失敗輸出。

### Contract validation

```bash
go run ./cmd/hufu team validate --team <fixture-team>
go run ./cmd/hufu list <fixture-team>
go run ./cmd/hufu --agent-team <fixture-team> --dry-run "process the declared workset"
```

### 通用 E2E fixtures

至少建立兩個不相關 fixture：

1. **transform fixture**：三個文字 artifacts，worker 逐項回傳 typed output；
2. **probe fixture**：三個 read-only targets，其中一個 verification failure。

驗證 normal、partial、duplicate key、stale artifact、retry、cancel、resume、budget exceeded 與 report replay。

### Consumer E2E contract

每個 consumer 的獨立遷移文件必須定義代表性 E2E，並至少檢查：

- producer 使用 structured adapter boundary，而非 transcript orchestration；
- manifest item count 等於 expansion receipt count；
- 每個 child 都有 item binding、typed result與 required verification；
- 不存在 marker files 或 transcript-parsing acceptance；
- acceptance 與 final report outcome 一致；
- presentation format 不參與 runtime completion gate。

## 12. 完成標準

全部條件同時成立才算完成：

- Hufu core 中沒有 consumer 名稱、Git/review/batch 特例或 Markdown parser；
- artifact-backed fan-out具有 durable expansion receipt與 resume parity；
- required result semantics由 `task_result_assert`驗證；
- aggregate completeness由 `workset_complete`驗證；
- action provider artifacts由 runtime計算 identity/digest並登錄；
- protocol repair、retry、cancel與resume不重展開 workset或重播不安全副作用；
- event/session/journal/JSON/report/TUI相關投影一致；
- 至少兩個獨立通用 fixtures通過，再開放 consumer遷移；
- `go test ./...`、`go vet ./...`、`golangci-lint run`全部成功；
- consumer E2E不再使用 transcript parser、review marker或 coverage-verifier agent；
- 相較遷移前，model calls、protocol repairs、tokens與 wall time都有記錄且無明顯退化。

## 13. 不採用的方案

### 將現有 coverage Bash 原樣改寫成 Go

不採用。它仍會解析 consumer transcript與 Markdown，只是換語言，沒有修正 canonical ownership。

### 在 `internal/team` 加入 review-aware verifier

不採用。這會違反 consumer邊界，且不能服務測試矩陣、資料處理或多環境 probe。

### 只依賴 `require-no-unresolved-tasks`

不採用。它不知道 source manifest是否完整展開，無法偵測漏列。

### 只檢查 output檔案存在

不採用。舊檔或前一 attempt產物可能被誤認為本輪成功。

### 讓 coordinator自行比較 row count或 paths

不採用。模型輸出不是 canonical evidence，且長清單重打本身就是既有失敗來源。

## 14. 實施順序與提交邊界

建議每個工作包獨立提交：

1. `test(team): characterize generic fan-out evidence gaps`
2. `feat(team): add typed task-result assertions`
3. `feat(team): persist artifact-backed workset expansion receipts`
4. `feat(team): ingest action-provider artifacts`
5. `feat(team): verify aggregate workset completion`
6. `feat(team): project workset state across reports and resume`
7. `docs(team): publish consumer migration contract`
8. `chore(team): remove legacy transcript workarounds after migration`

任何提交若同時加入 generic runtime mechanism與 consumer名稱判斷，應拆分並拒絕合併。
