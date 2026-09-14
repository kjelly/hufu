# Typed Run Inputs、Review Scope Resolver 與 Task-output Assertion 改善計畫

> Status: implemented — WP-00 through WP-08 complete; archived 2026-09-14
> Priority: P0（近期止血）+ P1（通用根治）
> Baseline: `10bc6e7053e1c06cbacc3189c6a12225ddd4708a`
> Risk: high — 變更 team manifest、run admission、ActionProvider、task result、event replay、acceptance 與 report contract
> Normative: 本文件是「使用者要求的 scope 不得被靜默忽略」的實作契約；若後續 ADR 與本文件衝突，必須先更新本文件或以 ADR 明確取代

## 1. 決策摘要

本系列採兩階段交付：

1. **近期止血（P0）**
   - 將 `.agent-teams/hufu-code-review/team.yaml` 的 review 預設從
     `max_commits: 1` 恢復為 `10`。
   - 以現有 `--var` 相容機制提供顯式且可覆寫的
     `review.scope.max_commits`，讓 action payload 與 coordinator 提示使用同一個值。
   - 修正 team manifest 內 `spec.vars` 無法作為同檔模板預設值的 bootstrap 缺口；未解析模板在 unattended 模式也必須於 preflight hard-fail。
   - `reviewprep` 回傳實際選取範圍與 scope 狀態，報告不得再以模型自行產生的標題代表實際範圍。
   - 此階段明確標示為 compatibility bridge；它不解析自然語言，所以「prompt 寫 7、未傳 `--var`」仍只能被揭露，不能完整自動對齊。
2. **通用根治（P1）**
   - Hufu core 實作 domain-neutral **Typed Run Inputs**。
   - team 宣告並擁有 deterministic **input resolver**；Git 範圍語意留在
     `hufu-code-review/reviewprep`，不寫入 Hufu core。
   - runtime 以 frozen input snapshot 將解析值綁入 static Action payload，並將 snapshot/payload digest 寫入 occurrence、receipt、event 與 evidence。
   - 新增 generic `task_output_assert`，由 acceptance 比對 producer 的 canonical runtime output、resolved input 與實際 scope。
   - report/JSON 只從 canonical run snapshot、assertion result 與 attested output 顯示 scope；模型 prose 不具有完成證明力。

完成後的唯一合法成功路徑：

```text
user prompt / --input
  -> team-owned deterministic resolver
  -> schema-validated frozen RunInputSnapshot
  -> runtime materializes static Action payload
  -> team-owned producer resolves repository range
  -> runtime-owned task output + action receipt
  -> task_output_assert(scope satisfied + input binding)
  -> workset_complete(all generated items verified)
  -> RunResult.GoalSatisfied=true
  -> report renders canonical input and attestation
```

任何一段缺失、衝突、歧義、超限、過期或無法重播時，run 都不得宣告 completed。

## 2. 根本原因與現況證據

### 2.1 `max_commits: 1` 沒有 runtime 必要性

目前 `.agent-teams/hufu-code-review/team.yaml` 的 static action payload 將
`max_commits` 固定為 `1`。此值：

- 不是 provider concurrency 限制；`max-concurrent` 已獨立控制 worker concurrency；
- 不是 workset item 數量限制；單一大 commit 仍可能被 diff bytes/lines/path caps 切成多個 item；
- 不是 `reviewprep` 演算法限制；現有測試已涵蓋 `MaxCommits=2` 與 merge first-parent history；
- 不是 Codex 或 Ollama context-window 要求；worker 只收到 bounded workset item，不直接收到完整十個 commit；
- 是歷史上從 `10` 改成 `1` 後被後續 schema migration 原樣保留的 team policy。

Git 歷史顯示：

- `35af248` 首次加入範圍上限時使用 `max_commits: 10`，目的是避免單靠
  `since` 產生無界範圍；
- `8c03de8` 將 `since` 改為 epoch 並保留 `10`，使 `max_commits` 成為主要 review window；
- `5660b55` 將 `10` 改為 `1`，沒有相應設計說明或 regression test；
- `391c101` 只做 manifest migration，原樣保留 `1`。

因此 `1` 應保留為明確的 per-commit/CI profile，而不應是互動式或 ad-hoc review 的隱含預設。

### 2.2 真正缺陷是 goal-to-scope binding gap

目前資料流如下：

```text
Prompt: "Review the last 10 commits"
  -> coordinator dispatches goal "produce workset"
  -> CompileTaskGoalContracts selects repository-owned static task
  -> static Action.Payload overwrites/discards coordinator execution fields
  -> reviewprep receives max_commits=1
  -> manifest truthfully records commit_count=1
  -> workset_complete verifies generated 1/1 item(s)
  -> acceptance passes
  -> RunResult.GoalSatisfied=true
  -> model prose can still write "Last 10 Commits"
```

Static contract authority本身是正確的安全邊界：coordinator 不應能任意選 provider、action type
或 payload。問題在於 runtime 沒有一條受信任路徑，把使用者要求轉換成 static contract
允許的 typed value，也沒有 acceptance 檢查「實際輸出是否符合解析後要求」。

具體缺口：

- `internal/team/coordinator.go` 的 `TaskDef.Action` 是 configuration-only；
- `internal/team/contract_compile.go` 會以 static `contract.Action` 覆蓋 coordinator 值；
- `internal/team/action_provider.go` 的 command adapter 只收到 Action JSON，不收到 canonical user intent；
- `internal/team/workset_verification.go` 只證明 manifest 所列 item 已完成，未證明 manifest scope 符合要求；
- `internal/team/run_result.go` 在 acceptance passed 且無 unresolved task 時設定
  `GoalSatisfied=true`；
- `cmd/hufu/report.go` 沒有 canonical run-input/scope section，final model prose 因而容易被誤讀為證明。

正確分類為：

- primary：**goal-to-scope binding gap**；
- secondary：**acceptance scope-attestation gap**；
- presentation：**report provenance gap**。

只把 `1` 改回 `10` 能修復目前案例的預設行為，但不能防止未來 prompt 要求 `3`、`20`、特定 SHA range
或日期時再次被忽略。

## 3. 目標、非目標與成功不變量

### 3.1 目標

- 使用者可用自然語言、`--input` 或 input file 指定 team-defined run input；
- 相同 logical input 經 canonicalization 後得到穩定 digest；
- input 在第一個 model/task/action call 前解析、驗證、凍結並持久化；
- static task contract 繼續擁有 capability、action type、payload shape 與允許的 binding target；
- coordinator/model 不得新增、覆寫或偽造 run input；
- team resolver 擁有 domain semantics，Hufu core 只管理 schema、precedence、provenance 與 lifecycle；
- producer output 能被 generic acceptance assertion 消費；
- resume、event replay、branch checkout、report 與 audit 對同一份 input/output identity 達成一致；
- 範圍太大時明確回報 `scope_too_large`，不得為了資源限制偷偷縮小範圍；
- `GoalSatisfied=true` 必須同時證明「要求的 scope 已滿足」與「該 scope 的 workset 已完成」。

### 3.2 非目標

- 不在 Hufu core 加入 `commit`、`git range`、`since` 或 `diff` 專用欄位；
- 不允許 coordinator 直接傳任意 Action payload；
- 不讓 LLM resolver 成為 blocking input 的唯一來源；
- 不把既有 `--var` 永久升格成 execution input；它仍是字串模板相容層；
- 第一版不把 Typed Run Inputs 當 credential/secret 管理機制；
- 不以自動 truncate、sampling 或「best effort」讓超大 scope 看似成功；
- 不改變沒有宣告 inputs 的既有 team 行為。

### 3.3 必須維持的不變量

1. **Static authority**：team contract 決定哪些 input 可綁入哪些 JSON pointer；model 無權指定。
2. **Resolve before execute**：input 未 frozen 前不能開始 coordinator LLM、worker、producer或 fan-out。
3. **No silent conflict**：prompt-derived candidate 與 explicit input 不一致時 hard-fail，不做靜默 precedence。
4. **No silent truncation**：requested scope 與 resource budgets 是不同概念；budget 超限不改寫 scope。
5. **Occurrence binding**：task occurrence、action invocation、output assertion 必須引用同一 input snapshot。
6. **Event ownership**：session/report 是 event-derived projection，不是另一份 canonical truth。
7. **Resume freeze**：中斷後沿用原 snapshot，不重新解析 prompt 或目前 repository state 來改寫需求。
8. **Fail closed**：缺少 resolver、schema mismatch、ambiguous source task、stale output 或 digest mismatch
   都阻擋完成。
9. **Domain neutrality**：core 只理解 JSON value、schema、resolver contract、binding、assertion與 digest。
10. **Bounded evidence**：input、resolver output、task output 與 diagnostics 都有明確大小/數量上限。

## 4. 推薦預設值與 scope/budget 分離

### 4.1 Review scope 推薦值

`hufu-code-review` 的一般預設：

```yaml
review.scope:
  kind: last_n
  count: 10
  history: first_parent
  head: HEAD
```

運行模式建議：

| Profile | `count` | 用途 |
|---|---:|---|
| ad-hoc / interactive default | 10 | 符合「近期變更 review」的一般預期 |
| per-commit CI | 1 | 每個 commit 都有獨立 job，明確選用而非隱含預設 |
| release / audit | explicit range | 使用 base/head 或 tag range，不以任意 N 代表 release scope |

`10` 是合理 nominal default，不是安全上限。近期基線的 last 10 first-parent commits 約為
88 paths、370,886 patch bytes、8,136 patch lines，現有 per-item caps 可切批，但沒有控制整體
fan-out 與 token 成本。

### 4.2 Team-owned resource budgets

另新增 review-team 專用 budgets；建議初始值：

```yaml
review.budget:
  max_total_diff_bytes: 1048576
  max_total_diff_lines: 20000
  max_changed_paths: 256
  max_workset_items: 64
  max_diff_bytes_per_item: 24000
  max_diff_lines_per_item: 600
  max_paths_per_item: 16
```

這些不是 Hufu core schema，而是 `reviewprep` input/output semantics。超限行為固定為：

```text
scope_too_large
requested scope: <canonical summary>
observed totals: <bytes/lines/paths/items>
configured caps: <...>
remediation: narrow --input review.scope, raise an explicit budget, or split the run
```

禁止：只取前 N paths、只產生前 64 items、降低 commit count、或仍回傳 `satisfied=true`。

## 5. P0：近期止血方案

P0 目標是在不等待完整 Typed Run Inputs 的情況下恢復正確常用預設，提供 deterministic
override，並讓未解析設定 fail closed。P0 不得被宣稱為自然語言 scope 的完整解法。

### 5.1 Team manifest 變更

在 v1alpha1 `spec.vars` 加入 team-owned default：

```yaml
spec:
  vars:
    review:
      scope:
        max_commits: 10
```

static Action payload 改為引用該值：

```yaml
payload: >-
  {"since":"1970-01-01","max_commits":{@ printf "%q" .review.scope.max_commits @},"max_diff_bytes":24000,"max_diff_lines":600,"max_paths":16}
```

同一變數可由現有 CLI 明確覆寫：

```bash
go run ./cmd/hufu \
  --agent-team hufu-code-review \
  --var review.scope.max_commits=10 \
  --report \
  --new \
  "Review the last 10 commits. Ignore uncommitted working-tree changes."
```

P0 precedence：

```text
team spec.vars default < hufu.yaml vars < --var-file < --var
```

`printf "%q"` 讓相容層值只能成為 JSON string，不能插入額外 JSON fields。`reviewprep` 以
strict wire DTO（unknown fields 與 trailing JSON hard-fail）接收該 string，再用 `strconv.Atoi`
轉成 internal integer 並驗證 `1..100`。字串模板只是 transport；invalid、float、negative、
zero、overflow 或 JSON injection 全部 hard-fail。既有直接呼叫 `Prepare(Config{MaxCommits:int})`
的 Go API 與 unit tests 不需改成字串。

### 5.2 修正 manifest vars bootstrap

現況 `readTeamManifestSource` 在 strict decode 前只使用呼叫端傳入的 vars 做 template；因此
同檔 `spec.vars` 無法可靠地替同一份 manifest 的 `tasks.action.payload` 提供 default。

P0 新增 bounded two-pass bootstrap：

1. 讀取 raw team manifest；
2. 只用最小 envelope-aware struct 擷取 legacy `vars` 或 v1alpha1 `spec.vars`；
3. vars 僅允許 YAML scalar/巢狀 mapping，透過既有 `yamlutil.FlattenYAML` 轉成字串 map；
4. 拒絕 vars key/value 內的 `{@`/`@}`，禁止 recursive template；
5. 以 team vars 為低優先，合併呼叫端 config/CLI vars；
6. 對完整 raw bytes 執行一次 `applyTemplate`；
7. 使用既有 strict/version-aware decoder 解碼；
8. render 後仍含任何 `{@ ... @}` 時 hard-fail，不能延後到 ActionProvider 才出錯。

這個 helper 必須由 `parseTeamYML`、`loadTeamContractTasks`、`loadTeamMCPServers`、
`ResolveTeamTemplateVars`、`FindMissingVars` 與 team lint 共用，避免不同入口得到不同 payload。

### 5.3 P0 producer scope output

`reviewprep` manifest schema 升至 v2，加入明確 scope：

```json
{
  "schema_version": 2,
  "scope": {
    "requested": {
      "kind": "last_n",
      "count": 10,
      "history": "first_parent",
      "head": "HEAD"
    },
    "resolved": {
      "base": "<sha>",
      "head": "<sha>",
      "selected_commit_count": 10,
      "available_commit_count": 10,
      "history_exhausted": false,
      "repository_shallow": false
    },
    "satisfied": true,
    "input_digest": "sha256:<canonical requested scope>"
  }
}
```

ActionResult `outputs` 至少回傳：

- `scope`：與 manifest 一致的 object；
- `manifest_path`；
- `changed_files`；
- `item_count`；
- `total_diff_bytes`、`total_diff_lines`。

語義：

- 一般 repository 中 selected count 必須等於 requested count；
- repository 確實只有少於 N 個 first-parent commits 時，可在
  `history_exhausted=true` 且 repository 非 shallow 時視為 satisfied；
- shallow repository 無法證明完整歷史，預設 `history_incomplete` hard-fail；
- empty scope 預設 `scope_empty` hard-fail，除非將來另有顯式 `allow_empty` input；
- resource cap 超限回傳 error，不寫 partial manifest。

### 5.4 P0 coordinator 與 report 約束

coordinator prompt 加入已解析的相容層值，但清楚標記它只是 runtime config：

```text
Authoritative configured scope: last {@ .review.scope.max_commits @}
first-parent commits ending at HEAD. Natural-language scope text cannot override
this value in the compatibility phase. Never claim another range was reviewed.
```

報告至少顯示 producer 的 runtime-owned outputs，不得從 `finalResult` 推導 scope。若 report
尚未完成 generic output rendering，P0 可先顯示：

```text
Configured review scope: last 10 first-parent commits
Resolved range: <base>..<head>
Selected commits: 10
Scope source: compatibility_var
Scope assertion: producer_attested (not yet core-bound)
```

`producer_attested` 不等於 P1 的 runtime assertion；報告必須如此標示，避免誇大保證。

### 5.5 P0 已知限制與退出條件

P0 尚無 deterministic prompt resolver。若 prompt 寫「last 7」但使用 default 10：

- runtime 會明確顯示實際 review 10；
- coordinator 不得聲稱 review 7；
- 但 runtime 尚無法自動判定 prompt/config conflict。

因此 P0 只能在下列條件下算完成：

- 原始案例的 default 與明確 `--var ...=10` 都實際產生 10-commit scope；
- override 3 實際產生 3-commit scope；
- 缺值、非法值、未解析 placeholder 都在任何 model/provider execution 前失敗；
- report 顯示實際 range/count，不再使用模型標題作為 scope 證明；
- 文件與錯誤訊息明示：非預設 scope 必須使用 `--var`，直到 P1 上線。

P1 完成後，`review.scope.max_commits` template var 標記 deprecated；經至少一個 release 的警告期後移除 execution semantics。

## 6. P1：Typed Run Inputs canonical model

### 6.1 Manifest authoring schema

在 `spec.inputs` 宣告 domain-neutral input definitions：

```yaml
spec:
  inputs:
    review.scope:
      type: object
      required: true
      default:
        kind: last_n
        count: 10
        history: first_parent
        head: HEAD
      properties:
        kind:
          type: string
          enum: [last_n, revision_range, since]
        count:
          type: integer
          minimum: 1
          maximum: 100
        history:
          type: string
          enum: [first_parent]
        head:
          type: string
          min-length: 1
          max-length: 160
        base:
          type: string
          max-length: 160
        since:
          type: string
          max-length: 80
      additional-properties: false
```

第一版支援：

- `string`、`integer`、`number`、`boolean`、`array`、`object`；
- `required`、`default`、`enum`、numeric min/max；
- string min/max length；
- array min/max items 與 item schema；
- object properties、required-properties、`additional-properties:false`；
- canonical JSON encoding。

第一版限制：

- input definitions 最多 64 個；
- 單一 canonical value 最多 64 KiB，整個 snapshot 最多 256 KiB；
- nesting depth 最多 8，array items 最多 256，object properties 最多 128；
- input name 符合 `^[a-z][a-z0-9_.-]{0,127}$`；
- 禁止 `$ref`、remote schema、regex、custom code 與 environment expansion；
- 不支援 secret input；宣告 `sensitive/secret` 必須 lint error，另行設計 credential channel。

### 6.2 Canonical Go types

新增 `internal/team/run_input.go`：

```go
type RunInputDefinition struct {
    Name     string
    Schema   RunInputSchema
    Required bool
    Default  json.RawMessage
    Resolver *RunInputResolverSpec
}

type RunInputSource string

const (
    RunInputSourceCLI      RunInputSource = "cli"
    RunInputSourceFile     RunInputSource = "file"
    RunInputSourceResolver RunInputSource = "resolver"
    RunInputSourceDefault  RunInputSource = "default"
)

type ResolvedRunInput struct {
    Name            string          `json:"name"`
    Type            string          `json:"type"`
    CanonicalValue  json.RawMessage `json:"value"`
    ValueHash       string          `json:"value_hash"`
    Source          RunInputSource  `json:"source"`
    ResolverID      string          `json:"resolver_id,omitempty"`
    ResolverVersion string          `json:"resolver_version,omitempty"`
    Evidence        []InputEvidence `json:"evidence,omitempty"`
}

type RunInputSnapshot struct {
    Version      int                `json:"version"`
    ID           string             `json:"id"`
    RunID        string             `json:"run_id"`
    InvocationID string             `json:"invocation_id"`
    Team         string             `json:"team"`
    SchemaHash   string             `json:"schema_hash"`
    Inputs       []ResolvedRunInput `json:"inputs"`
    SnapshotHash string             `json:"snapshot_hash"`
}
```

所有 slices/maps 都需 defensive copy。hash 使用版本化 canonical JSON；`SnapshotHash` 計算時
排除自身欄位與時間戳。`ResolvedAt` 若需要可放 event envelope，不進 identity hash。

### 6.3 Input provenance 與 precedence

有效解析順序不是簡單的「高優先覆蓋低優先」；所有可觀察來源先被解析，再檢查衝突：

1. CLI `--input`；
2. `--input-file`；
3. team resolver 從該 invocation 的 prompt 產生 candidate；
4. team default。

規則：

- CLI 與 file 同 key 且不同：`input_explicit_conflict`；
- explicit 與 resolver candidate 相同：採 explicit source，保留 resolver evidence 作一致性證明；
- explicit 與 resolver candidate 不同：`input_prompt_conflict` hard-fail；
- resolver 無 match：使用 explicit，否則 default；
- resolver ambiguous：即使有 default 也 hard-fail；不能用 default 掩蓋明確但模糊的要求；
- 沒有任何值且 required：`input_missing`；
- 未被 schema 宣告的 explicit key：hard-fail；
- 重複提供相同 canonical value 可接受但保留所有 source locations。

此規則刻意不提供 `--input-wins` 靜默開關。若將來需要 override prompt，必須新增名稱明確且留下 audit event 的確認機制，例如 `--ack-input-conflict`，不在第一版。

### 6.4 CLI surface

新增：

```text
--input <name>=<json-or-schema-typed-scalar>   repeatable
--input-file <path>                            repeatable; JSON/YAML
```

範例：

```bash
--input 'review.scope={"kind":"last_n","count":10,"history":"first_parent","head":"HEAD"}'
```

parser 依已選 team schema 解碼：integer 不接受 `10.0`，boolean 不接受任意 truthy 字串；string
可使用 JSON quoted string。CLI help 必須顯示 team inputs 與 default，可透過
`hufu list hufu-code-review` 或未來 `hufu team explain` 檢視。

多 team prompt 使用 `team::input=value`：

```bash
--input 'hufu-code-review::review.scope={...}'
```

- 單一明確 team 可用 unqualified name；
- multi-team 或 `--auto-team` 在 route 尚未確定時，unqualified input 若不能唯一歸屬則 hard-fail；
- resolver 只收到該 team/segment 的 prompt，不讀其他 team segment，避免 cross-team contamination。

現有 `--var` 保持 prompt/config templating 用途；CLI help 加註「不提供 typed execution semantics」。

## 7. Team-owned deterministic resolver

### 7.1 Manifest declaration

resolver 與 input definition 綁定，但執行能力由 team action provider 提供：

```yaml
spec:
  inputs:
    review.scope:
      # schema/default omitted here
      resolver:
        id: review-scope-v1
        capability: resolve-review-scope
        type: resolve_review_scope
        source: invocation_prompt
        side_effect: none
        timeout: 10

  action-providers:
    resolve-review-scope:
      command: [go, run, ./.agent-teams/hufu-code-review/reviewprep]
      dir: .
      timeout: 30
```

Hufu core 不理解 `last_n`。它只知道 resolver 輸入/輸出 envelope、side-effect policy、schema
validation 與 provenance。

### 7.2 Resolver protocol

stdin：

```json
{
  "type": "resolve_run_input",
  "input_name": "review.scope",
  "prompt": "Review the last 10 commits...",
  "explicit_value": null,
  "schema_hash": "sha256:...",
  "resolver_id": "review-scope-v1"
}
```

stdout：

```json
{
  "status": "matched",
  "value": {
    "kind": "last_n",
    "count": 10,
    "history": "first_parent",
    "head": "HEAD"
  },
  "evidence": [
    {"source":"prompt","start":11,"end":26,"kind":"last_n_commits"}
  ],
  "resolver_version": "1"
}
```

`status` 僅接受 `matched|no_match|ambiguous|invalid`。diagnostic 必須 bounded/redacted；resolver
不能回傳 artifact、shell command 或可執行 plan。Hufu 對 `matched.value` 再做 schema validation，
不能因 resolver 是 team-owned 就跳過 core contract。

### 7.3 `hufu-code-review` resolver semantics

第一版只接受清楚、可測試的 grammar：

- `last N commit` / `last N commits`；
- `HEAD~N..HEAD`；
- `<base>..<head>` full/abbreviated SHA 或 tag/ref；
- `since YYYY-MM-DD`；
- 沒有 scope 語句時 `no_match`，使用 default 10。

多個不相容表達（例如「last 10 commits, but only HEAD~3..HEAD」）回 `ambiguous`。不要用 LLM
猜測。resolver 僅解析 intent；Git ref 存在性、shallow history 與實際 commit cardinality由 producer
在固定 repository snapshot 上驗證。

### 7.4 Admission 時序

每次 coordinator invocation：

1. route/select team；
2. 建立 run/invocation identity；
3. 建立並持久化 execution policy snapshot；其 configuration hash 包含 input schema、resolver
   declaration、command provider identity 與 action bindings；
4. 執行 side-effect-none resolver；
5. 合併所有 sources、檢查衝突、validate schema；
6. append `run_inputs_resolved` event；
7. reducer 建立 frozen `RunInputSnapshot`；
8. 才允許 coordinator model、task admission 或 ActionProvider execution。

resolver 是 provider call，因此 execution policy snapshot 必須先完成。若 resolver failure 屬永久輸入錯誤不 retry；
只有明確 classified transient provider failure 可依既有 transient policy retry，且 idempotency key 固定。

## 8. Static Action input binding

### 8.1 Manifest schema

不以文字模板拼接 JSON。static payload 保留完整 shape，bindings 只允許替換 team 明確授權的 pointer：

```yaml
tasks:
  - id: produce-workset
    action:
      capability: produce-workset
      type: prepare_review_workset
      payload: >-
        {"scope":{"kind":"last_n","count":10,"history":"first_parent","head":"HEAD"},"budget":{"max_total_diff_bytes":1048576,"max_total_diff_lines":20000,"max_changed_paths":256,"max_workset_items":64,"max_diff_bytes_per_item":24000,"max_diff_lines_per_item":600,"max_paths_per_item":16}}
      input-bindings:
        - input: review.scope
          target: /scope
```

`input-bindings` 是 configuration-only，和 `Action` 一樣不得出現在 coordinator tool schema。

### 8.2 Materialization

新增 pure `MaterializeAction(staticAction, bindings, snapshot)`：

- strict decode static payload 為 JSON object；
- target 使用 RFC 6901 JSON Pointer；
- target 必須已存在，第一版不允許動態建立 shape；
- 每個 target 最多一個 binding；pointer overlap（`/scope` 與 `/scope/count`）hard-fail；
- input type 必須與 target schema/原值 type 相容；
- 替換後使用 canonical JSON encode 回 `Action.Payload`；
- 回傳 `MaterializedActionIdentity`：contract hash、snapshot hash、bound input hashes、payload hash；
- 不做 string interpolation、shell escaping 或 environment expansion。

static/effective contract hash 包含 input schema hash、resolver declaration、bindings 與未 materialize
payload；resolved value 不改寫 contract identity，而是進 occurrence/invocation identity。如此相同 team policy
在不同合法 scope 下仍是同一 contract，但 cache/receipt 不會混用。

### 8.3 Receipt 與 task occurrence binding

每個 action-backed task occurrence 新增：

```text
run_input_snapshot_id
run_input_snapshot_hash
materialized_action_payload_hash
bound_inputs[name -> value_hash]
```

同樣欄位寫入 `action_started`/`action_completed` payload、runtime action receipt、TaskResult runtime
metadata 與 EvidenceManifest binding。retry 沿用同一 snapshot 與 payload hash；若 live materialization 不同，
在 provider call 前以 `execution_input_drift` 阻擋。

cache key 必須納入 snapshot hash 與 materialized payload hash，避免 1-commit 結果命中 10-commit run。

## 9. Canonical task outputs

### 9.1 Runtime-owned output storage

目前 ActionProvider `ActionResult.Outputs` 被放入 `TaskResult.Facts`。P1 將 provider/runtime output 與
worker-authored facts 分開：

```go
type TaskResult struct {
    // existing fields...
    RuntimeOutputs map[string]any `json:"runtime_outputs,omitempty"`
}
```

規則：

- model-facing `submit_result` DTO 不包含 `runtime_outputs`；unknown field 仍拒絕；
- 只有 ActionProvider 與 structured runtime execution 可寫入；
- output 最多 128 keys、256 KiB canonical JSON、depth 8；
- key trim 後不可空，需 deterministic sort/hash；
- task completed event、task journal、session reducer、occurrence receipt 與 evidence manifest 都保存同一 digest；
- `Source="runtime"` 且存在有效 action invocation receipt 才能作 blocking assertion source。

遷移策略：新 run 寫 `RuntimeOutputs`。legacy session 的 action output若只存在 `Facts`，只在 action receipt
能證明 provider source時提供 read-only compatibility projection；它不能滿足新 manifest 的 blocking
`task_output_assert`，新 contract 的 interrupted legacy run 必須 `--new` 或明確 migration。

### 9.2 Review producer output

`produce-workset` 的 `RuntimeOutputs.scope` 至少包含：

```json
{
  "requested": {"kind":"last_n","count":10,"history":"first_parent","head":"HEAD"},
  "requested_input_hash": "sha256:...",
  "resolved": {
    "base":"...",
    "head":"...",
    "selected_commit_count":10,
    "history_exhausted":false,
    "repository_shallow":false
  },
  "observed_budget": {
    "total_diff_bytes":370886,
    "total_diff_lines":8136,
    "changed_paths":88,
    "workset_items":18
  },
  "satisfied": true
}
```

`requested_input_hash` 必須等於 runtime 傳入的 bound input hash；resolved base/head 必須與產出 diff
所使用 range 相同。producer 在所有 checks 完成前不發布 manifest；錯誤時 invocation workspace 可留作
diagnostic，但不能被 ingest 為成功 artifact。

## 10. Generic `task_output_assert`

### 10.1 Verification schema

在現有 `VerificationType` 新增 `task_output_assert`：

```yaml
acceptance:
  mode: blocking
  require-no-unresolved-tasks: true
  verifications:
    - type: task_output_assert
      source-task: produce-workset
      output: scope
      assertions:
        - pointer: /requested
          op: equals_input
          input: review.scope
        - pointer: /requested_input_hash
          op: equals_input_hash
          input: review.scope
        - pointer: /satisfied
          op: equals
          value: true
        - pointer: /resolved/selected_commit_count
          op: minimum
          value: 1
    - type: workset_complete
      source-task: review-workset
      require-all-terminal: true
      require-all-verified: true
      accepted-statuses: [success, completed_with_gaps]
```

新增型別：

```go
type TaskOutputAssertion struct {
    Pointer string `json:"pointer" yaml:"pointer"`
    Op      string `json:"op" yaml:"op"`
    Value   any    `json:"value,omitempty" yaml:"value,omitempty"`
    Input   string `json:"input,omitempty" yaml:"input,omitempty"`
}
```

第一版 ops：

- `exists`、`non_empty`、`equals`；
- `minimum`、`maximum`、`min_items`、`contains_scalar`；
- `equals_input`：actual canonical JSON 等於 frozen input value；
- `equals_input_hash`：actual string 等於 frozen input value hash。

### 10.2 Source resolution 與 trust checks

verifier 必須：

1. 以 logical task/contract ID 找到目前 branch、目前 run 的唯一 terminal occurrence；
2. 拒絕零個或多個 matches，不能任選最新一筆；
3. 要求 `TaskDone`、successful canonical TaskResult、`Source=runtime`；
4. 要求 result digest 與 completed task event/action receipt一致；
5. 要求 task occurrence 的 input snapshot hash 等於本 invocation frozen snapshot；
6. 從 `RuntimeOutputs[output]` 取得 root，使用 bounded RFC 6901 evaluator；
7. 將 expected/actual hash、pointer、op、source occurrence、snapshot ID 寫入 VerificationResult；
8. 任何 malformed pointer、type mismatch、missing output、stale receipt、digest mismatch都 fail closed。

不得 fallback 到 `TaskResult.Summary`、`Details`、artifact description、coordinator text 或 report prose。

### 10.3 Completion semantics

`hufu-code-review` 的 blocking acceptance 同時要求：

```text
scope_binding_assertion passed
AND producer reported scope.satisfied=true
AND workset_complete passed
AND no unresolved required tasks
```

缺少 `task_output_assert` 時，team lint 對「有 input-bound producer + workset_complete」組合報 P0 error，
避免 team 作者只證明處理完自己縮小後的 workset。

scope mismatch 分類為 `FailureVerify`/`StopReasonAcceptanceFailed`，不是可讓 coordinator 改寫 input 的 repair
機會。resolver/schema conflict 則為 pre-execution contract/input failure。unattended self-healing 不得自動縮小 scope；
rollback 與 read-only review 無關。

## 11. Persistence、replay、branch 與 audit

### 11.1 Event model

新增 event type：

```text
run_inputs_resolved
```

payload 為完整 bounded `RunInputSnapshot`。event 必須有 stable idempotency key：

```text
run-inputs:<run_id>:<invocation_id>:<snapshot_hash>
```

`ValidateEventPayload` strict 驗證 version、identity、hash、source enum、schema/value limits。event reducer 是
SessionData input projection 的唯一 writer；session checkpoint 不能獨立創造 snapshot。

Action lifecycle payload升版，加入 snapshot/payload hashes。舊 event 仍可讀，但不能被新 blocking assertion
誤認為具有 input binding。

### 11.2 Session projection

`SessionData` 新增：

```go
RunInputSnapshots []RunInputSnapshot `json:"run_input_snapshots,omitempty"`
ActiveRunInputSnapshotID string       `json:"active_run_input_snapshot_id,omitempty"`
```

使用 slice 而非單一欄位，因為 chat turn、多 team segment、同 team 重入與 branch lineage 可能在一個 session
內有多個 invocation。每個 Todo occurrence 直接保存 snapshot ID/hash，不依賴 mutable active pointer。

`CompareCanonicalProjection`、checkout reconstruction 與 event-store repair 都比較 input snapshots。event journal
有 snapshot 而 session.json 缺少時可重建；兩者內容衝突時 fail closed。

### 11.3 Resume 與 branch semantics

- interrupted occurrence：沿用 occurrence 綁定 snapshot，不重新執行 resolver；
- resume 時再次傳入 explicit input：canonical value 必須與 frozen snapshot 相同，否則
  `resume_input_conflict` 並提示 `--new`；
- `--new`：建立新 run/invocation/snapshot；
- fork/checkout：snapshot 隨 event lineage，不能引用另一 branch 不可見的 snapshot；
- retry：同 occurrence、同 snapshot；
- repair task：若屬同 invocation仍用同 snapshot；不能因 repair prompt 改變 scope。

### 11.4 Audit verification

`internal/auditverify` 新增 mandatory dimension `run_input_binding`：

- 從 events 重建 schema/snapshot identity；
- 驗證每個 input-bound action occurrence 的 snapshot hash、bound input hash與 payload hash；
- 驗證 task output digest 與 action completed receipt；
- 重新執行 pure `task_output_assert` evaluator；
- 驗證 RunResult 的 acceptance/GoalSatisfied 與重播結果一致；
- 缺少 event、unknown version、legacy unbound output或 hash mismatch 時 audit failed，不降級為 warning。

## 12. Report、JSON 與 operator UX

### 12.1 Markdown report

`cmd/hufu/report.go` 在 Run Snapshot 後新增 generic section：

```markdown
## Resolved Run Inputs

| Input | Source | Canonical value | Value hash | Resolver |
|---|---|---|---|---|
| `review.scope` | `resolver` | `{"count":10,...}` | `sha256:...` | `review-scope-v1@1` |

## Input-bound Assertions

| Criterion | Source task | Output | Assertion | State |
|---|---|---|---|---|
| `scope-binding` | `produce-workset` | `scope` | requested equals `review.scope`; satisfied=true | `passed` |
```

值使用 canonical bounded rendering與既有 secret redaction；v1 雖拒絕 secret inputs，仍需 defensive redaction。
模型產生的 review heading 可保留為 deliverable presentation，但其數字若與 canonical input 不同，report
加入 warning 且不能改變 canonical section。

### 12.2 JSON output

`--output json` 的每 team result 新增：

```json
{
  "run_inputs": {"snapshot_id":"...","snapshot_hash":"...","inputs":[...]},
  "input_bound_assertions": [...]
}
```

JSON exit status 仍取 canonical `RunResult.ExitCode`。resolver/input failure 必須輸出 machine-readable code，不能只有 stderr prose。

### 12.3 Dry-run、chat 與 prompt injection

- `--dry-run` 可執行明確宣告 `side_effect:none` 的 deterministic input resolver，顯示 resolved inputs、
  planned bindings與 payload hash；不執行 producer/fan-out/model。resolver 被拒絕或無法執行時 dry-run非成功。
- `chat` 每個 user turn 建立新 invocation snapshot；chat startup 的 explicit inputs是候選來源，仍需與每 turn
  prompt resolver 比對。
- mid-run prompt injection 不得改變 frozen snapshot；包含新 scope 的 injection queue 到下一 invocation，或以
  `input_frozen` 拒絕。
- direct-agent path 若 team 宣告 required inputs，也必須先 resolve；但未宣告 action binding的 direct task不會自動取得 execution authority。
- `--auto-team` 先完成 team selection，再依該 team schema 解析 qualified inputs。

## 13. Failure taxonomy 與 retry policy

| Code | Stage | Failure class | Retry | Completion |
|---|---|---|---|---|
| `input_schema_invalid` | team load/lint | contract | no | blocked before run |
| `input_unknown` / `input_missing` | admission | contract | no | failed/unverified |
| `input_explicit_conflict` | admission | policy | no | failed |
| `input_prompt_conflict` | resolver merge | policy | no | failed |
| `input_ambiguous` | resolver | contract | no | needs human / failed unattended |
| `input_resolver_failed` | resolver provider | environment/execution | transient only | failed if exhausted |
| `execution_input_drift` | action admission | policy | no | blocked |
| `history_incomplete` | producer | environment | no automatic narrowing | partial/failed |
| `scope_empty` | producer | verification | no | partial/failed |
| `scope_too_large` | producer | policy | no automatic narrowing | needs explicit input/budget |
| `task_output_assert_failed` | acceptance | verification | only rerun same producer if policy allows and input unchanged | not satisfied |

互動模式對 ambiguous/missing input 可使用既有 `ask_user` UX，但回答必須轉為 explicit typed value並寫入 snapshot
provenance。unattended 模式禁止自動選第一個 scope；直接 machine-readable failure。

## 14. Work packages 與合併順序

### WP-00 — Characterization fixtures（P0，無行為變更）

交付：

- 固定目前 `max_commits=1` 的 characterization test；
- 測試 natural-language 10 不會改寫 static payload，證明現有 bug；
- 建立 first-parent 1/3/10、merge、shallow、history exhausted、dirty worktree fixtures；
- 保存 current report 缺少 canonical scope 的 fixture。

目的：先讓 bug 可重現，避免只測 `reviewprep` unit 而漏掉 coordinator/acceptance/report 整合。

### WP-01 — P0 default、compatibility var 與 fail-closed templating

交付：

- manifest vars two-pass bootstrap；
- unresolved placeholder preflight error；
- `review.scope.max_commits=10` default 與 Action payload binding；
- coordinator prompt 使用同一變數；
- reviewprep integer bounds；
- team validate/lint regression tests。

此 PR 不加入自然語言 resolver，release note 必須說明 limitation。

### WP-02 — P0 scope output、total budgets 與 honest report

交付：

- reviewprep manifest v2 scope/observed budget；
- total caps 與 `scope_too_large`；
- no partial manifest publication；
- generic bounded Action runtime output report section或最小 canonical producer output section；
- 原始 command 的 E2E：實際 selected count=10。

### WP-03 — Typed input schema、CLI 與 frozen snapshot

交付：

- manifest types/strict parser/lint；
- `--input`、`--input-file`、multi-team qualification；
- canonical value/schema/snapshot hashing；
- `run_inputs_resolved` event/reducer/session projection；
- no-input teams zero behavior change；
- admission ordering與 execution policy configuration hash integration。

### WP-04 — Team-owned resolver

交付：

- generic resolver envelope與 command adapter path；
- conflict/ambiguity rules；
- `review-scope-v1` deterministic parser；
- dry-run/chat/multi-team/resume semantics；
- no model/provider task call before successful resolution。

### WP-05 — Static Action materialization與 receipt binding

交付：

- `input-bindings` manifest schema/lint；
- pure pointer replacement/canonical payload；
- occurrence/action event/receipt/evidence hashes；
- retry/cache/resume drift checks；
- coordinator tool schema negative tests。

### WP-06 — RuntimeOutputs 與 `task_output_assert`

交付：

- runtime-only TaskResult outputs；
- strict assertion evaluator與 source occurrence resolution；
- acceptance criterion integration；
- event/task journal/session clone/replay；
- lint rule要求 input-bound workset producer具有 blocking scope assertion。

### WP-07 — Review team migration

交付：

- `hufu-code-review` 從 compatibility var 遷移至 `spec.inputs`；
- resolver、action binding、scope assertion、workset_complete wiring；
- CI one-commit profile 使用 explicit input；
- 移除 coordinator prompt 中「自然語言不能 override」的過渡警告，改顯示 canonical resolved input；
- compatibility `--var review.scope.max_commits` 發出 deprecation warning。

### WP-08 — Report、JSON、audit 與 compatibility cleanup

交付：

- canonical input/assertion report與 JSON；
- auditverify independent replay dimension；
- branch/session compatibility inspector；
- metrics與 release migration guide；
- 一個 release 後移除 execution-specific template var path，保留普通 `--var` 功能。

依賴順序固定：

```text
WP-00 -> WP-01 -> WP-02
              \
               -> WP-03 -> WP-04 -> WP-05 -> WP-06 -> WP-07 -> WP-08
```

WP-03 至 WP-06 不應以一個巨型 PR 合併；每個 PR 都需保持 event replay與 no-input team tests綠燈。

## 15. 預計修改檔案

### P0

- `.agent-teams/hufu-code-review/team.yaml`
- `.agent-teams/hufu-code-review/coordinator.md`
- `.agent-teams/hufu-code-review/reviewprep/main.go`
- `.agent-teams/hufu-code-review/reviewprep/main_test.go`
- `.agent-teams/hufu-code-review/reviewprep/testdata/**`
- `internal/team/team_manifest.go`
- `internal/team/parse.go`
- `internal/team/vars.go`
- 對應 parser/lint/report tests

### Typed inputs / resolver / action binding

- `internal/agent/agent.go`：manifest-facing definitions；
- `internal/team/team_manifest.go`、`parse.go`、`team_policy_lint.go`、`contract_finding.go`；
- 新增 `internal/team/run_input.go`、`run_input_resolver.go`、`action_materialize.go`；
- `internal/team/action_provider.go`、`runtime_workflow.go`、`contract_compile.go`；
- `internal/team/coordinator*.go`、`task_occurrence.go`、cache/receipt/evidence files；
- `cmd/hufu/options.go`、`root.go`、`run.go`、`team_runner.go`、`segments.go`、`chat.go`、`resumecmd.go`；
- dry-run/list/explain paths。

### Assertion / persistence / presentation

- `internal/team/task_result.go`；
- verification dispatch/validation files與新增 `task_output_verification.go`；
- `internal/team/event_types.go`、`event_payloads.go`、`event_reducers.go`、`session.go`、
  `coordinator_eventstore.go`；
- `internal/team/run_result.go`、evidence manifest與 task journal projection；
- `internal/auditverify/**`；
- `cmd/hufu/report.go`、JSON output與 TUI status translation（若顯示 resolution event）。

實作前需重新 `git status --short`；目前 working tree 已有與本計畫無關的
`internal/team/coordinator_task_run.go`、event store、execution policy snapshot與 invariant E2E 修改，
不得覆寫、reset 或混入本系列 commit。

## 16. 測試矩陣

### 16.1 P0 tests

- team default 無 CLI var：payload 為 integer 10；
- `--var review.scope.max_commits=3`：payload 為 integer 3；
- legacy flat與 v1alpha1 `spec.vars` 都可 bootstrap；
- config vars < var-file < CLI var；
- recursive/unresolved placeholder hard-fail；
- unattended 不跳過 missing execution var；
- invalid `0/-1/10.0/text/JSON fragment/101` hard-fail；
- first-parent last 10 selected；dirty working tree 不進 scope；
- total bytes/lines/paths/items 任一超限都不產生成功 manifest；
- report selected count/range 來自 runtime output。

### 16.2 Typed input unit/property tests

- schema normalization、limits、unknown fields、duplicate names；
- canonical JSON map ordering與 hash stability；
- source precedence and all conflict pairs；
- explicit/default/resolver values type validation；
- malformed/deep/oversized JSON fuzz tests；
- multi-team qualified input parsing；
- clone functions無 backing array/map alias；
- secret-like values經 redaction，不出現在 diagnostics。

### 16.3 Resolver tests

- no scope -> no_match -> default 10；
- last 1/3/10 commits；singular/plural/case/spacing；
- SHA range、HEAD~N..HEAD、since date；
- two conflicting expressions -> ambiguous；
- explicit equal prompt -> pass；explicit different prompt -> fail before model call；
- resolver invalid JSON/unknown status/schema-invalid value/timeout/oversize；
- prompt segments不跨 team；
- unattended ambiguous不呼叫 ask_user或 coordinator model。

### 16.4 Action binding tests

- pointer replacement與 canonical payload golden；
- missing input/target、overlap、type mismatch、duplicate binding；
- coordinator提交不同 payload仍被 static contract取代；
- coordinator tool schema不能看見 input-binding/action fields；
- action receipt包含 snapshot/payload hash；
- retry hash不變；changed input無 cache hit；
- resume live drift在 provider call前被阻擋。

### 16.5 Task output assertion tests

- 所有 ops pass/fail/type mismatch；
- source task missing/duplicate/non-terminal/wrong run/wrong branch；
- worker-authored Facts/Summary不能滿足 assertion；
- action output digest、receipt、snapshot任一 mismatch fail；
- `equals_input` 與 `equals_input_hash`；
- 10-commit input + producer output 1 -> acceptance failed；
- 10-commit input + producer output 10 + incomplete child -> workset failed；
- scope pass + workset 100% -> completed；
- legacy unbound result不能滿足新 blocking criterion。

### 16.6 Persistence與 E2E

- crash after input event、before action；
- crash after action output、before acceptance；
- replay builds byte-equivalent snapshot/assertion result；
- fork/checkout branch visibility；
- chat two turns with different scopes have two invocation snapshots；
- prompt injection cannot mutate active snapshot；
- original user command實際 selected count=10，report canonical section=10；
- prompt says 10 + explicit 1：`input_prompt_conflict`，exit non-zero，report不得顯示 success；
- report final prose故意寫錯 count，canonical section與 audit仍顯示真值並警告；
- auditverify detects tampered snapshot/output/action receipt/run_finished。

## 17. Validation gates

每個修改 Go/code 的 WP 必須依 repository 規則執行：

```bash
go test ./.agent-teams/hufu-code-review/reviewprep
go test ./internal/team/...
go test ./internal/auditverify/...
go test ./cmd/hufu/...
go test ./...
go vet ./...
golangci-lint run
```

另執行：

```bash
go run ./cmd/hufu team validate .agent-teams/hufu-code-review
go run ./cmd/hufu --dry-run --agent-team hufu-code-review \
  "Review the last 10 commits. Ignore uncommitted working-tree changes."
```

最終真實 E2E 可使用使用者指定 models，但測試成功條件不看模型敘述，而看：

- resolved input snapshot count=10；
- action payload bound count=10；
- producer selected count=10；
- scope assertion passed；
- workset expected/completed/verified相等；
- canonical RunResult completed/GoalSatisfied=true；
- report與 audit identity一致。

不得以網路/model不可用作為跳過 deterministic unit/integration/lint gates 的理由。

## 18. Rollout、相容性與 rollback

### 18.1 Feature rollout

- Typed Inputs 只對含 `spec.inputs` 的 team 啟用；未宣告 teams不寫空 snapshot、不增加 resolver call；
- P1 初期可用 `typed-run-inputs: shadow` team flag，計算 snapshot/binding/assertion但不改 completion；
- `hufu-code-review` 先在 shadow 比較 compatibility var與 resolver output，要求連續測試無 mismatch；
- 再切 `enforce`，同時啟用 blocking `task_output_assert`；
- enforce後才移除 compatibility var作為 execution source。

### 18.2 Metrics

新增 bounded counters，不記 prompt/value內容：

- input resolution source counts；
- no-match/ambiguous/conflict/schema failure counts；
- resolver latency/failure；
- action materialization drift；
- task output assertion pass/fail；
- scope-too-large與 observed cap bucket；
- report prose/canonical scope mismatch count。

### 18.3 Rollback

- P0 rollback只回復 team default/template/report bridge；不刪除 user workspace artifacts；
- P1 shadow可關閉 enforcement，但已寫 events保持可讀，不能重寫 event journal；
- enforce run若已建立 input-bound tasks，降版 runtime不得 resume；compatibility inspector必須提示升版或
  `--new`，不得忽略 snapshot；
- 不使用 `git reset --hard`、`git clean` 或刪除 session作 migration手段。

## 19. 完成定義

本計畫只有同時滿足以下條件才算根治完成：

1. 原始 command 的 canonical scope 是 last 10 first-parent commits，而非 1；
2. prompt 指定其他合法 scope 時，resolver輸出與 action實際 payload一致；
3. prompt 與 explicit input衝突會在任何 model/task/action前失敗；
4. producer不得因 budgets縮小 scope；超限回明確錯誤；
5. `task_output_assert` 證明 producer requested output等於 frozen input、input hash一致且
   `satisfied=true`；
6. `workset_complete` 另外證明該完整 workset已 terminal/verified；
7. 缺少任一證明時 `RunResult.GoalSatisfied=false` 且 exit non-zero；
8. resume/replay/fork/cache不能跨 snapshot誤用結果；
9. report與 JSON顯示 canonical scope/provenance/assertion，不以 model prose為依據；
10. auditverify能從 events獨立重播並偵測 tampering；
11. 未宣告 Typed Inputs 的既有 team行為與測試維持不變；
12. `go test ./...`、`go vet ./...`、`golangci-lint run` 全部成功。

## 20. 最終推薦

- **預設值：10**，適合一般 ad-hoc code review；`1` 改為明確 CI profile。
- **近期操作介面：** `--var review.scope.max_commits=N`，搭配 team default 10與 honest runtime report；
  這是短期 bridge，不是最終安全契約。
- **長期操作介面：** `--input review.scope=...` + team-owned deterministic resolver。
- **完成證明：** `task_output_assert(scope binding/satisfied)` 與 `workset_complete` 兩者都必須通過。
- **資源控制：** total budgets獨立管理，超限 fail closed，不修改 requested scope。

這個切法保留 Hufu 的通用性：core 實作的是 typed values、resolver lifecycle、JSON binding、output assertion
與可重播證據；「commit 是什麼、last N 如何計算、first-parent 如何處理」始終由 review team 擁有。
