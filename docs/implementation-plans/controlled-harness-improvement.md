# hufu 受控 Harness 配對實驗與有界改善實作計畫

> Status: active
> Authority: reference
> Verified-Commit: `af4875f1720d2315cbaa2f2e866d11cb78ab1882`
> Supersedes: —
> Superseded-By: —
> Execution-Status: PR 1–3 已完成；PR 4–5 依第 12 節繼續交付。
> 日期：2026-10-20。
> 程式碼查核基準：`af4875f1720d2315cbaa2f2e866d11cb78ab1882`。
> 範圍決定：使用者已選擇「本次只做可離線驗收的功能，真實評測與隔離部署另案處理」。
> 依使用者最新目標移入正式計畫目錄，逐階段實作與提交。
> 下列新型別、設定格式、事件與 CLI 為待實作契約，不代表目前已有這些功能。

## 實作進度

- [x] Baseline：完整測試、build、vet、lint、文件及隱藏 team 測試。
- [x] PR 1：契約、materializer 與 plan CLI。
- [x] PR 2：耐久 controller 與總預算。
- [x] PR 3：手動 probe 與 runtime 閉環。
- [ ] PR 4：有界 Propose–Probe–Compose。
- [ ] PR 5：bundle、lesson 與完整驗收。

### Baseline 驗證紀錄

2026-10-20 在 `b83b787d` 執行；此提交只保存本計畫與文件索引，
Go 原始碼與 `af4875f1720d2315cbaa2f2e866d11cb78ab1882` 相同。
下列命令全部 exit 0；測試沿用 Go 正常 cache，並非全部強制重跑。

| 命令 | 結果 |
|---|---|
| `TMPDIR=/tmp/hy GOTMPDIR=/tmp/hy go test ./...` | 通過；`internal/team` 243.774s |
| `TMPDIR=/tmp/hy GOTMPDIR=/tmp/hy go test ./.agent-teams/hufu-code-review/reviewgate ./.agent-teams/hufu-code-review/reviewprep ./.agent-teams/web-research/tests` | 三組通過 |
| `go build -o /tmp/hufu-harness-baseline ./cmd/hufu` | 通過 |
| `go vet ./...` | 通過 |
| `GOLANGCI_LINT_CACHE=/tmp/hufu-harness-lint golangci-lint run` | 0 issues |
| `bin/check-docs` | 78 active、158 total、440 relative links、37 Go anchors |
| `git diff --check` | 通過 |

### PR 1 驗證紀錄

2026-10-20，在 baseline 提交 `2edff617` 上完成：

- `internal/improve/paired`：嚴格設定／fixture／program 契約、preview 與 committed plan
  的型別區分、整數不失真的 canonical digest、純配對計分。
- `internal/improver`：受限檔案封存、authored／effective 雙層比較、Markdown／flat／
  versioned inline／shorthand／skill body、template 展開後預檢、局部 schema closure
  bundling、policy 欄位分類與 schema coverage fixture。
- `hufu improve probe plan --config <file> [--json] [--out <new-file>]`：
  不讀 host provider 設定、不建立 workspace、不建立 provider；輸出明示 runtime preflight pending。
- 完整範例位於 `internal/improver/testdata/extraction/`；fixture 仍不代表真實品質結果。

驗證全部通過：focused tests、`TMPDIR=/tmp/hy GOTMPDIR=/tmp/hy go test ./...`
（最終 `internal/team` 267.900s）、`go vet ./...`、
`go build -o /tmp/hufu-paired-phase1 ./cmd/hufu`、
`GOLANGCI_LINT_CACHE=/tmp/hufu-harness-lint golangci-lint run`（0 issues）、
`bin/check-docs`、`git diff --check`。
實際 binary smoke test 以完整範例輸出合法 JSON，stderr 為空；
`--out` 建立預覽檔，明示指定的 workspace 沒有被建立。
T01–T07、T20 的相關離線邊界已有測試；runtime execution、journal、resume 與 export
仍由後續階段交付。

### PR 2 驗證紀錄

2026-10-20，在 `45bfcddd` 上完成 `internal/improver` 的耐久控制器：

- private content-addressed object store；artifact 先同步及 exclusive publish，
  journal 才引用；readonly opening、digest／size／permission／symlink 檢查。
- `StoreJournal` 沿用 canonical SQLite event store；`ReplayLedger` 純重播，
  `LoadCommittedPlan` 固定 branch／run／session／plan，global-head CAS 衝突後重讀重算。
- 初始 baseline＋第一候選完整 envelope、逐 stage 分配、所有 attempts 花費及
  probe 次數、未知用量保留、超額記錄、有限 retry、不可挪用 required 預算。
- durable UTC deadline＋process 內 monotonic deadline；clock rollback／cancel 停止 admission。
  真實 child identity binding 與單次 Start 授權先於 provider；`StageExecutor` 返回前須 drain。
- crash reconciliation、已完成 child 匯入一次、lost acknowledgement、late／conflicting
  completion、缺失 evidence、未引用物件均有故障測試。已發布判定遇到衝突也會 blocked。

最終版本驗證全部 exit 0：

| 命令 | 結果 |
|---|---|
| `go test ./internal/improver ./internal/improve/paired -count=1` | 通過 |
| `TMPDIR=/tmp/hy GOTMPDIR=/tmp/hy go test -race ./internal/improver -run 'TestController\|TestObjects\|TestLedger\|TestCommitted' -count=1` | 通過，17.126s；包含兩個 OS process 的同 slot admission |
| `TMPDIR=/tmp/hy GOTMPDIR=/tmp/hy go test ./...` | 通過；CLI 122.743s、team 214.244s |
| `go vet ./...` | 通過 |
| `go build -o /tmp/hufu-paired-phase2 ./cmd/hufu` | 通過 |
| `GOLANGCI_LINT_CACHE=/tmp/hufu-harness-lint golangci-lint run` | 0 issues |
| `bin/check-docs`、`git diff --cached --check` | 通過 |

T08–T14 使用 fake executor 驗收，沒有真實 provider、部署或品質評測。
本階段不增加 run／resume／show CLI；canonical runtime adapter 與各顯示投影屬 PR 3。
一般 `cmd/hufu`／`internal/team` 的執行語意沒有改動。

### PR 3 驗證紀錄

2026-10-20，在 `d6728c9e` 上完成手動 probe 閉環：

- immutable runtime factory、明確的 `structured_only` overlay 與 policy preflight；
  每個 arm／attempt 使用獨立的 team copy、child runtime 及 scripted cursor。
- 通用 `RunTask`、pre-provider invocation binding 與 construction-time transport
  injection；既有 coordinator、task verification 與 whole-run acceptance 語意保留。
- 共用 socket-free scripted transport，eval harness 沿用相同實作；trusted transport
  的 explicit zero usage 與 missing usage 分開保存，普通 SDK 預設零仍保持未知。
- canonical child event log／receipt／artifact 匯入、post-drain completion marker、
  frozen closure、readonly reconciliation 與報告重建；不依賴來源 team 或 child 目錄。
- `probe run/resume/show`、JSON／plain／既有 TUI；真實 PTY 測完成、resize、取消、
  terminal restoration。fixture 始終保留 `simulation_only`。
- 操作文件：[Controlled harness probes](../reference/controlled-harness-probes.md)。

驗證全部 exit 0：

| 命令 | 結果 |
|---|---|
| `TMPDIR=/tmp/hy GOTMPDIR=/tmp/hy go test ./...` | CLI 245.490s、eval harness 115.047s、improver 41.326s、team 348.134s；其他套件通過 |
| `TMPDIR=/tmp/hy GOTMPDIR=/tmp/hy go test -race ./internal/improver ./internal/agent ./internal/team ./internal/scripted -run 'TestProbe\|TestExperiment\|TestInvocationBinding\|TestCoordinatorRunTask\|TestProviderUsagePresence\|TestCostManagerExplicit\|TestScripted\|TestBoundary' -count=1` | 通過；improver 150.790s |
| `TMPDIR=/tmp/hy GOTMPDIR=/tmp/hy go test ./internal/improver -run 'TestProbeClassification\|TestExperimentOffline' -count=1` | 最後新增的 verifier 分類與偽造報告測試通過，2.748s |
| `go vet ./...` | 通過 |
| `go build -o /tmp/hufu-paired-phase3 ./cmd/hufu` | 通過 |
| `GOLANGCI_LINT_CACHE=/tmp/hufu-harness-lint golangci-lint run` | 最終版本 0 issues |
| `bin/check-docs`、`git diff --check` | 通過 |

覆蓋 T15–T19、T21 的 runtime／tool／usage／報告邊界；沒有呼叫真實模型。
沒有完整 usage 或可核對完成證據的 provider failure 維持 unknown／blocked，
不以錯誤文字猜測是否可重試。PR 4 繼續交付 bounded proposal scheduling，
PR 5 交付 Markdown review bundle、exact patch 與 lesson。

## 1. 交付目標與完成邊界

交付一套可比較 baseline／candidate harness 的執行與證據管線，再加入一輪有界的 Propose–Probe–Compose。Harness 在此只指 worker 的提示內容及預先載入的純文字 skill。工具能力、模型、輸出契約與驗證條件固定。

| 階段 | Coding agent 必須交付 | 效益 |
|---|---|---|
| P0 | 嚴格配對資料契約、耐久控制器、總預算、手動候選 runner、查詢及報告 | 修改前後有相同輸入與可核對證據；避免漏算失敗、重跑與成本 |
| P1 | 最多兩個 typed proposals、一個 composition、重新驗證、exact patch 與候選經驗匯出 | 減少準備修改的人工工作，保留失敗與退步證據 |
| 驗收 | 真實 coordinator＋scripted provider 的離線整合測試、故障注入、CLI 與文件 | 無模型帳號、付費呼叫、外部 sandbox 或人工評分也能完成開發 |

最小可用交付為 PR 1–3。PR 4–5 完成自動提案與匯出。離線測試證明執行契約正確，不證明真實模型變好。

首版只支援 `structured_only`：一個已指定 worker 接收固定文字輸入，透過 `submit_result` 交付結構化結果；沒有 worker shell、檔案、網路或委派工具。適用於分類、資料抽取、受 schema 約束的回答等任務。一般 coding team、瀏覽研究及基礎設施操作不在首版支援面。

本次工作清單不包含：真實模型實驗的執行與品質結論、模型／帳號／GPU 採購或設定、業務資料及人工標註、hidden holdout 研究、sandbox／container／VM／egress 部署、任意 shell verifier、外部 coding CLI、MCP、活躍記憶學習、多 round、平行 probes、美元預算、延遲優化目標、正式 promotion／批准／部署。這些均不是合併或完成條件；不為它們保留待辦。

`llm` 模式的程式接線仍屬交付：由使用者日後明示執行，使用既有 provider 配置與 runtime admission。所有開發驗收使用 `fixture` 模式或注入的測試替身，不要求呼叫真實模型。

## 2. 現況、重用點與模組責任

| 現有實作 | 本計畫的使用方式 |
|---|---|
| `internal/improve/experiment.go`：TeamSnapshot、BenchmarkFixture、EvaluateExperiment | 重用快照讀取與 v1 報告；新資料另用 versioned paired envelope，補逐 case 綁定 |
| `cmd/hufu/improve_experiment.go`：compare 的人工 acceptance flags | 原有指令保留；新 runner 只認 canonical typed result 與 verifier evidence |
| `internal/team/team_composition.go`、`team_inline.go` | 使用現有 normalized agents 與 source index，支援 Markdown 與 inline instructions |
| `internal/team/execution_contract.go`、`tool_sequence_policy.go` | 固定 `tool_sequence: [submit_result]`，要求 grounded typed result |
| `internal/team/verification.go`、`internal/agent/agent.go` | 使用 `task_result_assert` 與既有 assertion validator，不執行 shell |
| `internal/team/event_store_conditional.go` | `AppendAtHead` 的跨 process CAS 是新控制器 admission 權威 |
| `internal/team/budget_manager.go`、`cost_manager.go` | 重用 child 的 token admission／usage 與既有 cost 記錄；新增 parent allocation，不改造計價器 |
| `internal/evalharness` | 重用 real coordinator＋scripted provider 的測試模式；抽共用 seam 時保留既有 suites |
| `internal/improve/handoff_store.go` | JSON＋CAS 仍是既有 handoff 權威；本次不遷移、不自動建立 handoff |
| `internal/promotion/apply.go` | 既有新 skill／追加 policy 的權限不擴張；本次僅匯出 review bundle |

現有 local／isolated-copy world 都宣告 `SupportsNativeSandbox=false`、`SupportsNetworkControl=false`。首版透過封閉工具與不啟動外部 backend 限制能力，不宣稱提供 OS sandbox；需要額外能力的設定在 provider 建立前拒絕。

建議新檔案及依賴方向：

- `internal/improve/probe_manifest.go`、`paired_experiment.go`：資料契約、canonical encoding、純比較及報告；不呼叫模型。
- `internal/improver/`：controller、journal reducer、allocation、materializer、probe adapter、proposal／composition、review export。
- `cmd/hufu/improve_probe.go`、`improve_refine.go`：參數解析、注入 immutable runtime factory。
- `internal/team/`：只補通用的受約束單 task invocation、pre-provider identity binding、可讀 evidence export seam；不理解 experiment 名稱、fixture case 或特定 team。
- `internal/evalharness/`／`evals/`：共用 scripted transport 與版本化測試 fixtures；正式 runtime 不依賴 evalharness。

禁止 `internal/team → internal/improve` 反向 import。若抽出共用 fake transport，使用無 evaluator／team 依賴的 transport support package，由 evalharness、fixture CLI adapter 與 improver tests 引用；fixture adapter 由入口注入，不讓 improve／team 匯入 evaluator。既有 improve metadata-only 邊界維持；prompt、proposal 內容與結果只存受控 artifact，不進 telemetry 正文。

## 3. 固定 Runtime 契約

1. 一般執行、舊 compare、plan、show 不增加模型呼叫。只有新 run／refine 入口啟動工作。
2. 模型僅回傳 typed proposals／results；不能改權限、預算、驗證、固定模型或批准自己。
3. Todo、task receipt、verification、session 仍透過現有 canonical transition APIs 寫入。
4. Canonical redaction 後的 bytes／hash 不再改寫；匯出不重新產生 child receipt。
5. Idempotency key 包含 branch、experiment、stage、generation。CAS／lease 負責跨 process 防重。
6. 未知 usage、遺失證據、identity 衝突或未停止 worker 都保持未知／blocked，不補 0 或 pass。
7. Provider work 走現有 runtime admission、budget、lifecycle；不新增直接 HTTP 或 CLI 模型捷徑。
8. 不修改 JUDGE／CHALLENGE／REVISE 的現有語意。首版 selection 是 deterministic，不引入模型裁判。
9. 關閉流程必須先停止 admission、drain worker／callback，再關 coordinator／store，最後釋放 workspace lease。
10. 新的可觀察狀態同步到 reducer、JSON、Markdown、CLI status 與通用 TUI reporter；不新增專用 TUI 頁面。

## 4. 首版輸入契約

### 4.1 Authored config 與 resolved plan

新增 `ProbeConfigV1` 與 `ExperimentPlanV1`；前者含使用者路徑，後者只含已凍結 artifact refs／digests 與明確身份。使用嚴格 YAML decoder：拒絕未知欄位、重複 keys、alias cycle、null 必填欄位、錯型及整數溢位。所有 enum 為 lowercase／snake_case。

以下是手動模式的完整 config 形狀；相對路徑以 config 所在目錄為準。PR 1 必須新增對應完整 fixture 檔組並通過 parser；此例的檔案與 backend 是待建立的示例，不能把 placeholder 當真實 provider。

```yaml
schema_version: 1
experiment_id: extraction_pair
mode: manual
execution_mode: fixture
profile: structured_only
baseline_dir: ./team
candidate_dirs:
  - id: candidate_1
    path: ./candidate
fixture: ./cases.yaml
worker: extractor
vars: {}
allowed_surfaces:
  - kind: inline_instructions
    agent: extractor
models:
  executor: {backend: fixture, model: scripted}
fixture_program: ./responses.json
replicates: 1
limits:
  per_stage_tokens: 12000
  per_stage_seconds: 60
  pipeline_tokens: 60000
  pipeline_seconds: 300
  max_probe_runs: 4
  stage_retries: 1
objective:
  kind: quality_gain
  min_pass_gain: 1
```

規則：

| 欄位 | 型別、預設與限制 |
|---|---|
| `schema_version` | 必填整數，僅 1 |
| `experiment_id`、candidate `id` | 必填；`^[a-z][a-z0-9_]{0,63}$`；保留 baseline、procedure、domain、composition，不得作手動候選 ID |
| `mode` | 必填 `manual` 或 `refine` |
| `execution_mode` | 必填 `fixture` 或 `llm`；無隱含 live fallback |
| `profile` | 必填，僅 `structured_only` |
| `baseline_dir`、`fixture`、`worker` | 必填；worker 必須精確解析為既有 worker，不能是 coordinator／Helper |
| `candidate_dirs` | manual 必填且恰一項；refine 必須省略，由固定 slots 生成 |
| `vars` | string→string map，省略為空；不讀 process env 補 template 變數 |
| `allowed_surfaces` | 必填非空且不重複；kind 僅 `markdown_body`、`inline_instructions`、`skill_body`，見第 5 節 |
| `models` | 每項為 `{backend, model}` 非空字串；manual 僅 executor；refine 必須另列 proposer、composer、reflector。每角色一個 target，禁 fallback／escalation／extra-model |
| `fixture_program` | fixture 必填；llm 禁止。所有角色共用凍結 program、每 stage 建獨立 cursor |
| `replicates` | 整數 1–20，預設 1；case 數 1–100；超出拒絕 |
| `limits` | token／seconds／max_probe_runs 全為正整數、必填；stage_retries 僅 0 或 1，預設 0；無 `0=unlimited` |
| `objective` | 必填 typed union；`quality_gain` 要正整數 min_pass_gain；`token_reduction` 要 1–99 的整數 min_token_reduction_percent；另一種欄位必須省略 |
| `refinement` | refine 必填，manual 禁止；`procedure_prompt`、`domain_prompt`、`composer_prompt`、`reflector_prompt` 為凍結純文字檔路徑；不可含可執行模板。流程為 1 round／2 proposals／1 composition，不能覆寫數量 |

`pipeline_tokens >= per_stage_tokens`，`pipeline_seconds >= per_stage_seconds`。初次 admission 前需保留 baseline＋至少一候選全部 required slots 的額度；不滿足就拒絕，不開始半套比較。`max_probe_runs` 計 executor attempts（含重試）；proposal／compose／reflect 受 stage 次數與 pipeline 預算另行約束。

Surface 的完整 typed union：前兩種只有 `{kind, agent}`，skill_body 只有 `{kind, path}`；不接受其他欄位。Agent 使用 loader 的 canonical identity，path 為 baseline team root 下的 slash-separated 相對路徑。Plan 依 kind＋canonical agent／path 排序配置 `surface_001` 等穩定 surface_id；proposal 只能引用這些 ID。所有 limits 使用 int64，運算先檢查乘加溢位；所有 pipeline tokens／seconds／probe 次數必須足以容納最低 required slots，不能只驗正數。

Resolved plan 另外含：source revision 與 binary build digest、config digest、project/team/parent branch、logical slots、scope roots、runtime policy projection version、baseline closure、case refs、typed verifier、models 的實際 generation 設定、stage 上限與資料策略。非必要 role 不啟動。離線 plan 的輸出是 `PlanPreviewV1`，尚未取得的 branch／runtime identity 不填假值，也不冒充 committed ExperimentPlanV1。Unknown target 可標為尚未完成執行 preflight；run 在第一個 provider call 前解析並凍結，無法解析就拒絕。禁止把未解析 plan 顯示為可執行。未提供 source revision 的 local build 以實際 executable SHA-256 作 build identity，不能用共同的 unknown 字串通過 resume。

### 4.2 Benchmark fixture

新增 `ProbeFixtureV1`；不擴張舊 BenchmarkFixture v1 的隱含語意。形狀如下：

```yaml
schema_version: 1
id: extraction_cases
result_schema: ./result.schema.json
cases:
  - id: case_a
    input: ./inputs/case_a.txt
    critical: false
    verify:
      type: task_result_assert
      mode: blocking
      task-result-assertions:
        - pointer: /structured_payload/category
          op: equals
          value: allowed
```

每 case 必須有唯一合法 ID、非空 UTF-8 input 及非空 verify assertions。`critical` 預設 false。結果 schema 使用現有 Draft 2020-12 result-contract compiler，所有 `$ref` 必須解析為 fixture closure 內的檔案；禁網路 schema ref。固定字彙使用 enum。首版 assertion ops 僅 `exists`、`non_empty`、`equals`、`min_items`、`contains_scalar`，其型別與資源限制重用現有 validator。只允許指向 `/structured_payload` 子樹，不評分 summary 自述。

此新 fixture 契約的 `mode: blocking` 在 adapter 明確映射為既有 task verifier 的
`Mode: success`；現有 verifier 不接受 `blocking`，不能原字串直傳，也不修改既有 mode enum。
Fixture 的 `/structured_payload` 指向模型提交的值；canonical `TaskResult`
另外保存 schema identity 與 digest，因此 adapter 必須將這個 prefix 映射為
`/structured_payload/value`，保留後續 JSON Pointer escapes，且不改寫凍結 fixture。
跨檔案的普通 `$ref` 先封存、轉為單一文件的 fragment refs，再交既有 compiler；
跨 resource 的 dynamic refs／dynamic anchors 首版拒絕，不改寫其 scope 語意。

輸入、schema 與 evaluator 檔案都在第一次 stage admission 前凍結。首版所有 case 都是 required development cases，沒有 optional case、hidden holdout 或連線取得資料。驗證條件是公開的任務要求，可以隨固定 task contract 提供；不宣稱 blind grading。禁止 shell command、任意 renderer、程式碼判分或模型評分。

每個資料檔最多 1 MiB，case input／單份提示內容最多 64 KiB；closure 最多 1000 個 regular files、合計 32 MiB。超過拒絕，不能默默截斷。PR 1 用常數集中管理限制並測試邊界。

### 4.3 Fixture program 與完整離線示例

新增 `ProbeProgramV1`，形狀為 `{schema_version: 1, scripts: [...]}`。每 script 是 `{stage, case_id?, variant_slot_id?, attempt, steps}`：stage enum 為 `executor / procedure / domain / compose / reflect`；executor 必填 case_id／variant_slot_id，其他 stage 禁止這兩欄；attempt 為 1 或 2。每個 selector 唯一，replicate 共享相同 script 定義但各自取得 fresh cursor。不存在 script 時回 `fixture_program_missing`，不能回退真實模型。

Steps 重用現有 ProviderStep 的 content／tool_call／match／forbidden 形狀，另加明確 `usage: {input_tokens, output_tokens}`，兩數非負且加總不溢位。`usage: null` 僅用於測試 unknown usage，不能當成零。不得省略 usage。現有 fake provider 沒有完整 usage 回報，PR 3 必須補 transport seam 同時支援 streaming／non-streaming usage；保留舊 eval suites 的相容預設，新 paired program 必須明示 usage。

以下 `responses.json` 足以驅動第 4.1、4.2 節的單 case 範例；它刻意令 baseline 失敗、candidate 通過，僅驗證比較分支：

```json
{
  "schema_version": 1,
  "scripts": [
    {
      "stage": "executor", "case_id": "case_a", "variant_slot_id": "baseline", "attempt": 1,
      "steps": [{
        "tool_call": {"name": "submit_result", "arguments": "{\"status\":\"failed\",\"summary\":\"required classification was not produced\",\"structured_payload\":{\"category\":\"denied\"}}"},
        "usage": {"input_tokens": 100, "output_tokens": 20}
      }]
    },
    {
      "stage": "executor", "case_id": "case_a", "variant_slot_id": "candidate_1", "attempt": 1,
      "steps": [{
        "tool_call": {"name": "submit_result", "arguments": "{\"status\":\"success\",\"summary\":\"fixture result\",\"structured_payload\":{\"category\":\"allowed\"}}"},
        "usage": {"input_tokens": 100, "output_tokens": 20}
      }]
    }
  ]
}
```

`result.schema.json` 內容為：

```json
{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "type": "object",
  "properties": {"category": {"type": "string", "enum": ["allowed", "denied"]}},
  "required": ["category"],
  "additionalProperties": false
}
```

`$schema` 是本機 compiler 的 dialect identifier，不是允許下載網路資源。`inputs/case_a.txt` 為 `Return the category allowed.`。`team/team.yaml` 與 `candidate/team.yaml` 使用以下基底；candidate 只將 instructions 換成 `Return the requested category in structured_payload.category.`：

```yaml
name: extraction_fixture
agents:
  extractor:
    preset: readonly
    tools:
      allowed: [submit_result]
      denied: [view, grep, glob, ls]
    instructions: Return a category in the required structured payload.
```

Profile 在新的 bounded invocation 啟用 memory-off／closed sequence／typed verification，會將這些限制記入固定 policy；不是把一般 team 執行冒充成原樣比較。明示衝突設定仍拒絕。PR 1 建立完整示例檔組，PR 3 驗收結果須為 `simulation_only`、simulation_decision=`eligible_for_review`、兩個 scored slots、synthetic executor tokens 各 120。Retry 示範另用具備 attempt 2 script 的 fixture，不能把此範例沒有 retry script 當成功能缺陷。

Baseline 用 schema-valid、明示 `failed` 的 typed result 表達 functional failure。
若回傳不符合 assertion 的 `success`，既有 runtime 會在 submit admission 拒絕並要求修正；
測試該分支須提供完整的有界 repair script，不能把 script 耗盡產生的 transport error
當作已驗證的 functional failure。未提供整體 acceptance 時，child run 保留 canonical
`unverified`／`acceptance_not_configured`；case pass 只取已完成 task 的 schema、receipt
及 verifier evidence，不把整次 run 改寫成 completed。

## 5. 候選可變面與 materialization

### 5.1 明確修改規則

| Surface | 可修改 | 固定或拒絕 |
|---|---|---|
| `markdown_body`，指定 agent | 該 worker 已存在 `.md` 的 body | Frontmatter bytes、名稱／角色／alias、檔案位置固定 |
| `inline_instructions`，指定 agent | flat `agents.<id>.instructions` 或 versioned `spec.agents.<id>.instructions` | 其餘 typed YAML 值固定；允許 scalar shorthand 展開成保留同 preset 的 mapping，新增 instructions；禁止修改共享 anchor 影響其他欄位 |
| `skill_body`，指定 `path` | 已存在且明確列入該 worker skills 的 `SKILL.md` body | Frontmatter、skill ID／trigger、引用依賴、tools、scripts 固定；禁新增 skill、auto discovery 或動態載入 |

Tool guidance 必須寫在上述 body／instructions，沒有獨立可改 tool schema。TeamPreset 生成而無 authored instructions 的 worker 首版不可作可變目標；不得偷偷建立同名 Markdown worker 覆蓋 preset。載入其他不變的 preset workers 可以，但不 dispatch。

候選驗證分兩層：先比較 authored tree，確認只變更明列 surface；再用相同 vars 與既有 loader normalize，確認除指定 System／skill body 外的全部 effective settings 相等。新載入欄位沒有分類時拒絕。YAML 序列保持原順序、map 用 keys 比較；不得以字串替換或 regex 模擬 YAML 語意。

新提示文字不接受 template delimiters；baseline 原有 templates 以凍結 vars 解析一次，保留 source 與 rendered hashes。技能只取凍結的純文字內容，由 runtime 在受約束 task 啟動前提供；不讓 worker 使用 load_skill 或 view。若技能依賴執行 script、讀未封存檔案或動態 skill matching，preflight 拒絕；不能從自然語言猜測並執行指令。

允許提示中討論命令文字，但文字不產生執行權限。禁止的是可執行配置、script／hook／plugin 檔案變動及工具 admission；不使用關鍵字表判定「提示是否危險」。

### 5.2 Closure 與路徑

Closure 包含：team 所有定義與本地 result schemas、選定 worker 的凍結 skill 內容、fixture inputs／verifiers、fixture program（僅 fixture mode）、明示 vars、refinement 提示與 adapter 版本。其餘檔案要列入 inventory 並保持不變，不靠 patch hash 代表完整內容。

所有輸入必須為 regular files；拒絕 symlink、特殊檔、逃出根目錄的路徑、網路 include、循環引用、未列入 inventory 的依賴。新檔案引用以各自 owning manifest 目錄解析。materialize 時複製並驗證輸入 hashes；輸入在讀取間改變則拒絕。不讀或複製既有 workspace、`.git`、憑證、global skills 或使用者 context store。

每 arm 有 experiment-owned team copy、subject root 與獨立 control workspace。正式 checkout 不作整合目標。memory／semantic retrieval／learning／promotion 固定 off，context store 新建空白；不導入原先設想的 SQLite 記憶複製與跨 scope 重映射。

## 6. 資料權威、digests 與防誤用

### 6.1 權威與 artifact storage

父 workspace `runtime.sqlite` 的 `improvement_*` events 是新 controller 唯一權威；child workspace 的既有 journal 是 task／receipt／verification／usage 權威。projection 可丟棄重建。既有 HandoffStore 與 context.sqlite 的權威不變，不建立跨 store transaction。

新增資料放在 `<workspace>/improvement/paired/<experiment_id>/`。內含 content-addressed `objects/<sha256>`、child workspaces 與可重建 reports。目錄 0700、檔案 0600；同 digest 已存在時驗證相同 bytes，不覆寫。原始內容不寫 event payload。ArtifactRefV1 為 `{kind, digest, size_bytes}`，digest 為 64 位小寫 SHA-256；kind 由讀取端明確限制，不能把 ref 當任意 filesystem path。

先以 temp file、fsync、原子 create／rename 持久化 artifact，再提交帶 ref 的事件。事件提交失敗留下未引用 artifact，不視為完成；首版不做自動 GC。原始輸入須為可保存的已清理內容；若既有 secret scanner 要改寫內容，preflight 拒絕並要求另提供清理後 fixture，不能改寫已凍結 bytes。模型輸出依現有 canonical redaction 持久化；evidence view 與 proposal 必須使用那些 canonical bytes。

### 6.2 三種 digest

所有新 hash 使用 schema version 1 的 canonical JSON bytes：UTF-8、object keys 遞迴字典序、無多餘空白、array 保留順序、整數禁止浮點替代、拒絕 NaN／Inf／重複 keys；不存在欄位與 null 不混同。Generation 等既有 float 欄位在新 projection 中用 `strconv.FormatFloat(value, 'g', -1, 64)` 的字串表示，負零正規化為 `"0"`；不改原 runtime snapshot 的序列化。Digest 加 domain prefix：`hufu.paired.<kind>.v1\n`（結尾是真正 newline）。PR 1 提供固定 golden vectors，不依賴 Go map iteration。

| Digest | 納入內容 | 排除內容 |
|---|---|---|
| `comparison_context_digest` | case input／公開 schema／verifier hashes、logical world、有效工具封閉契約、模型及 generation map、共同 memory-off policy、per-stage limits、runtime build、固定 team 配置、fixture program hash（適用時） | 可變提示／skill body、arm 的實體路徑、run IDs、時間戳 |
| `variant_digest` | 完整 authored inventory、rendered worker System、技能 bodies、vars binding、合法 overlay、preset expansion／runtime build version | child run ID、執行結果、物理 materialization 路徑 |
| `evidence_digest` | slot／attempt／runtime identities、canonical completion／verification／usage refs、輸出 artifact inventory | CLI rendering、可重建摘要、重新遮罩後的內容 |

另保留每個 arm 原始完整 ExecutionPolicySnapshot／ConfigurationHash，resume 仍使用 runtime 原有 drift check。比較 projection 不取代原 hash。

PR 1 必須建立 typed classification table：

- tool grants、roles、generation、result contracts、routes、capabilities、verification、network、memory、budget 都是 fixed；有 hash 包含物理 root 時需從受信任的 resolved typed input 重建 logical projection，不能直接刪 hash。
- `subject_root`、`team_root`、`control_root` 僅按 materializer 登記的精確 root 映射為各自 logical IDs；禁止通用 regex 忽略所有 path／hash。
- 指定 System／skill bodies 是 variant；未指定提示與 roster 都是 fixed。
- `AgentDef.Capabilities` 是 loader 從 System 推導的描述文字，與機器可驗證的
  capabilities／requirements／tool grants 不同。只在指定 System surface 上確認其
  等於既有推導函式輸出後列為 derived variant；固定 worker、禁止動態委派，
  不以這段文字變動取得能力。其餘 worker 的描述仍固定。
- run IDs、receipt IDs、時間戳是 evidence；仍保留於原始 policy 或 journal。
- build／policy schema 不支援、未知欄位、無法分類的 opaque policy hash都拒絕 strict comparison。

用反射／schema coverage 測試確保新增 runtime policy 欄位會觸發更新分類；另以行為測試證明相同 roots 映射不掩蓋 model／tool／verifier 變更。

### 6.3 Plan、slot 與 manifest

Plan 在第一個 provider call 前提交，列出完整 `variant_slot_id × case_id × replicate` 集合。manual 為 baseline＋candidate_1；refine 為 baseline、procedure、domain、composition。不存在的 optional variant 可跳過，但 variant 一旦開始驗證，其所有 cases 都是 required。

候選內容未知時只建立 logical slot。`improvement_variant_bound` 以 CAS 將 slot 綁定一次性的 `{snapshot_ref, variant_digest, origin}`。同 slot retry 不得換內容；失敗 proposal 的第一次合法結果才綁定，綁定後再修改需要新實驗。

`ProbeRunManifestV1` 必填：schema_version、experiment/parent_branch、slot_id、variant_slot_id、case_id、replicate、pair_key、attempt（1 或 2）、generation、child invocation/run/branch IDs、execution_mode、snapshot_ref、三種 digests、完整 execution_policy_ref、resolved_models、outcome、receipt_refs、verification_refs、usage_ref、usage_completeness。`retry_of`／`reused_from` 僅存在時填；不允許空 reference。

`pair_key` 是 experiment、comparison context、case、replicate 的 canonical hash。outcome enum 為 `passed / failed / timed_out / unknown / cancelled`。usage_completeness 為 `known / unknown`；fixture 的 synthetic usage 另由 execution_mode 標示，不能冒充真實 usage。planned／未啟動 slot 放 reducer projection，沒有 manifest、不預造 run IDs。

### 6.4 新報告與 legacy 邊界

`PairedExperimentReportV1` 使用獨立 `kind: paired_experiment_report`，含 plan_ref、所有 slot states／manifests、gates、decision、simulation_decision（僅 fixture）、coverage 與 budget summary。這不是舊 `ExperimentReport.Version=1` 的加欄位版本。

舊 v1 reports、compare flags、handoff、promotion 行為不變。新格式不能交給 legacy reader 或當作 `experiment_report` ref。新 reader 必須從指定 workspace journal 的 plan／variant／finished events 重建與核對，不能只相信匯入 JSON 的 origin 或 decision。

Canonical candidate identity 為 parent workspace identity＋branch＋experiment＋variant slot＋digest；origin 僅 `runtime_paired_probe`、`bounded_harness_improvement`，由事件決定。刪 origin／改檔名／移除 strict refs 不能讓同一受管 identity 通過新 reader。手動另建內容的 legacy operator 流程不在此防偽保證內；不得聲稱其取得 strict evidence。

本次不建立 promotion proposal 或 handoff，不以新 report 自動驅動 Apply。`eligible_for_review` 只表示新 paired 報告符合條件，沒有批准或套用權限。

## 7. Probe runtime 與隔離邊界

### 7.1 有效能力必須在 provider 建立前決定

Probe 使用通用單 task runtime entry，指定 worker／固定 typed contract，不執行 LLM coordinator planning。重用 ExecuteTasks 的編譯、授權、verification、receipt 路徑；若需要新的 public entry，由 team 提供不含 experiment 詞彙的 bounded invocation API。不得直接呼叫 worker provider 後自己製作成功 receipt。

每個 executor／proposer／composer／reflector task 均固定：inline execution、requires-result、requires-grounded-result、`tool_sequence: [submit_result]`、side_effect=`none`、無 worker artifact side effects、無 dynamic delegation。agent 空工具集不能被舊 selector 解釋成 wildcard；以封閉序列及中央 gate 執行限制，不修改原有空工具語意。Runtime 自身仍正常保存 typed results／receipts；不能將「無 worker artifact」誤解成不保存證據。

只用 `hufu-local` 的 LLM transport。拒絕 external backend、MCP、hooks、action catalog、runtime workflow、run-input model resolver、auto skills、guard sidecar、judge／skeptic、extra models、fallback／escalation、shell verify／acceptance／rollback、網路／檔案工具與主動記憶功能。固定配置存在不相容能力時回 `unsupported_profile`，不可默默刪掉並宣稱原 team 已被測試。

首版每次只啟動一個 stage。worker、coordinator planning 與 implicit tools 都不能提供繞路能力；中央 gate 拒絕不在序列的呼叫，測試需包含偽造 agent／bash／view／memory_query 呼叫。Typed result schema 與 task_result_assert 由 runtime 保護，模型不能替換它們。

模型傳輸與 task egress 是兩件事：llm 模式只透過現有 provider transport 通訊，worker 沒有 network tool。fixture 模式只能使用 runner 建立的 scripted local endpoint／in-process transport；拒絕任意 URL 或 live credentials。`fixture` 的 backend selector 是入口注入的測試 transport 身份，不寫入一般 provider registry 作可連線服務。

### 7.2 驗證與身份提交

Runtime 先分配真實 child invocation/run/branch IDs，呼叫通用 pre-provider binding callback；callback 將 `improvement_stage_bound` 提交父 journal，成功後才允許任何 provider call。binding 失敗即關閉 child，保留可追溯 reservation。`SetInvocationID` 不等於指定 run ID。

這個 seam 必須位於 run-input resolver、sidecar、context embedding、worker 或其他可能付費呼叫之前；即使 profile 已禁止那些功能，也用測試防止新增路徑偷跑。Runtime factory 使用 immutable options；不修改 Cobra globals、process env 或 cwd。

`passed` 要求 canonical worker status 成功、grounded result 合法、task_result_assert blocking 驗證通過及 receipt／usage 完整。每個 fixture schema／assertions 經既有編譯 API 綁定 durable task，不偽造 verifier exit code。驗證失敗保留原始結果與 receipt；不啟動自我修復來改 rubric 或挑最好答案。

## 8. 配對、評分與決策

同 pair 的 input、fixed policy、model/generation、per-stage limits 必須相同。case/replicate multiset 精確比對，拒絕缺失、重複、外來 IDs；不能以 tasks 總數替代 coverage。一個 baseline replicate 可供多候選共用，報告明示 correlation／reuse，成本只計一次。

每個 case/replicate 的客觀分數僅 0 或 1；不做語意猜測或統計顯著性宣稱。

| 結果 | 計分與處理 |
|---|---|
| Canonical result＋所有 assertions 通過 | 1 |
| 有可靠終態，模型未履約或 assertions 失敗 | 0；成本保留，不因失敗移出分母 |
| 達 task deadline，已確認停止且 usage 完整 | 0；timed_out |
| Verifier outage、已存在 artifact 遺失／hash drift、usage 未知 | 不評分；unknown／inconclusive |
| Child 是否停止或是否送出 provider request 不明 | blocked，先 reconciliation |
| 預算不足、未 dispatch | not_started／skipped_budget；不偽造結果 |

Coverage 同時列 planned、terminal、scored、functional_failed、unknown、not_started。Baseline task 失敗不等於 baseline evidence 無效。所有 critical pairs 的 candidate score 必須為 1；所有 noncritical pairs 也不得從 baseline 1 退成 0。完整 pair coverage 與可核對成本是任何改善判定前提。

- `quality_gain`：candidate pass 總數至少比 baseline 多 min_pass_gain，且無逐 pair 回歸。
- `token_reduction`：逐 pair 品質不退步，baseline attributable tokens 必須大於 0，總節省百分比達 min_token_reduction_percent；用整數交叉相乘比較，避免浮點誤差。
- Objective 不得事後切換。候選排序：quality_gain 依 pass 數高者、再 executor tokens 少者；token_reduction 依 executor tokens 少者、再 pass 數高者；仍相同則按凍結 variant slot 順序。Baseline 與 candidate 全部目標相同選 baseline。
- Executor tokens 包含該 evidence 所有已發生成本，不只成功最後一次。另列整個 pipeline 的 proposal、compose、reflection 與失敗探索成本，不把 executor 節省宣稱成總成本節省。

Gate 固定為 pairing_integrity、baseline_evidence_valid、evidence_integrity、protected_surface、required_coverage、quality_non_regression、objective_gain、resource_limits、resume_consistency。每 gate 輸出 expected／observed／`passed | failed | unknown`。

Decision enum：`eligible_for_review / no_change / rejected / inconclusive / blocked / simulation_only`。優先序為未排除的在途／恢復問題 blocked，再證據缺失 inconclusive，再對完整候選評 gate。至少一個合格候選才 eligible；有效比較全部無目標增益為 no_change；只有越界／回歸候選為 rejected。Optional variant 失敗不抹除已驗證候選，但所有已 admission stage 必須停止且完成可核對結算。

Fixture 成功完成整個流程時一律 `simulation_only`，原本應得到的 eligible/no_change/rejected 記於 simulation_decision；blocked／inconclusive 仍如實回報。Fixture 報告與 bundle 明示 synthetic，永遠不能成為真實品質或採用資格證據。

## 9. 總預算、事件與 crash recovery

Parent ledger 在 stage admission 前保留完整 per-stage token 上限；child 用現有 token admission enforcing。預留整個 candidate required slots 的 envelope，再從中分配單 stage reservation，不能重複加總。optional composition 不挪用 required 驗證預算。

Stage retry 最多一次且只限已證明停止、可核對花費的 transient transport failure 或尚未綁定的 invalid proposal。客觀失敗／deadline failure 不重跑挑 winner。child task fresh retries 設 0；現有 bounded result-only protocol repair可保留，但每次模型呼叫仍在同 stage budget，repair 不得執行其他工具。所有 exhausted attempts 都保留。

第一個 stage 前將 started_at／deadline 持久化；resume 不重置總時限、總花費、attempt 或 max_probe_runs。單 process elapsed 用 monotonic clock；跨 process 用持久化 UTC deadline，若 now 早於最新 durable clock observation 回 blocked。provider usage 未知保留 reservation、停止新 stage。不能承諾外部 provider 物理 exactly-once 或絕不超額；超額如實記錄並使 resource_limits 失敗。

| 事件 | 必填 payload 核心 |
|---|---|
| `improvement_run_created` | plan_ref、parent branch、deadline、schema_version |
| `improvement_envelope_reserved` | group；完整 required slots 的 token／probe envelope 由 frozen plan 推導 |
| `improvement_variant_bound` | slot、snapshot_ref、variant_digest、canonical origin |
| `improvement_stage_admitted` | stage_id（對應 plan 的 stage／slot）、generation、owner、planned child workspace identity、reservation |
| `improvement_stage_bound` | 真實 child invocation/run/branch IDs |
| `improvement_stage_started` | ownership／dispatch reference |
| `improvement_stage_completed` | canonical refs、evidence_digest、usage settlement、outcome |
| `improvement_stage_transition` | failed／blocked／cancelled／skipped_budget、stable reason_code |
| `improvement_admission_stopped` | cancel_requested／deadline_exceeded／clock_rollback；停止後續 admission，仍可 reconcile／settle |
| `improvement_run_finished` | report_ref、decision；budget summary 由 reducer 重建，報告不是 ledger 權威 |

Stage states 為 planned → admitted → bound → running → completed，另有 failed／blocked／cancelled／skipped_budget。completed 表示證據收集完成，不代表品質通過。reducer 拒絕非法 transition、身份衝突、過期 generation；同內容重送冪等。AppendAtHead 衝突後重讀重算，不只使用 mutex。Payload 自有 version；只有新增 SQL 結構才 append migration，不改舊 migration checksum 或全域 policy hash 語意。

第二階段的 `Controller`／`StoreJournal` 直接使用上述 canonical event store；
`ReplayLedger` 是純投影，`LoadCommittedPlan` 僅從指定 branch 的 creation event 載入。
`ObjectStore` 以 private directory descriptor 限制路徑，先同步 bytes、exclusive publish
再引用，任何讀取都核對 digest／size／permission。未引用物件不計費、不形成 evidence。
`StageExecutor` 是 runtime adapter 的接線介面：必須在 provider 前提交實際 identity，
並在返回前完成取消及 drain；此階段只有 fake executor 驗收，不代表真實 runtime 已接線。
dispatch 的 `Start` 授權只能取得一次；lost acknowledgement 必須 reconciliation。
已發布報告之後出現 conflicting completion，保留原 report ref 供稽核，但目前 decision
改為 blocked；不得繼續採用舊 report 的成功判定。

| Crash／取消點 | 恢復行為 |
|---|---|
| admission 前 | 可依 frozen plan admission |
| admission／binding 後、dispatch 狀態未知 | 查 child journal／ownership；缺 started event 不能證明尚未呼叫 provider |
| child 完成、父尚未匯入 | 驗證 refs／hashes，匯入並結算一次，不重跑 |
| child 仍在執行 | 等待／經既有取消回收；不開第二個同 slot child |
| child 已停止且成本未知 | blocked，保留 reservation；不以無檔案副作用推論未付費 |
| 純 projection／匯出中斷 | 從 committed refs 重建，不呼叫模型 |
| late／duplicate completion | 同 generation 同 bytes 冪等；過期拒絕，衝突 blocked |
| 取消 | 停止 admission、drain、結算已知成本，再釋放 lease；未完成回收不可標成已取消 |

父與子 store 不共享 transaction。Replay 是純函式，不會啟動 provider、補寫副作用或自行 dispatch；resume 明確執行 reconcile 後才可 admission。無法確認的狀態維持 blocked，不需要 coding agent 在驗收時人工操作外部服務才能完成測試。

## 10. 有界 Propose–Probe–Compose 與經驗匯出

Refine 固定順序：baseline → procedure proposal／probe → domain proposal／probe → compose／probe → deterministic select → reflect → export。兩 proposal 都只看同一 baseline evidence；domain 不偷看 procedure winner。每個模型角色都是同一 structured_only runtime 的 bounded child，沒有工具存取或自主檔案修改。

`ChangeProposalV1` 必填 schema_version、hypothesis、observed_failure_refs、changes、expected_behavior、applicability；changes 是非空的 `{surface_id, before_hash, replacement_text}` array。surface_id 只能選 plan 登記值，每項只出現一次。由 deterministic materializer 核對 before_hash、大小及 allowed surface 後套入 experiment-owned copy。模型輸出不是 patch 執行命令。

Compose 回傳 typed union：`keep_baseline`、`use_candidate`（已綁定 slot ID）、`compose`（同 ChangeProposal changes）。不能新增未計畫 slot。新的 variant_digest 必須完整重跑 required pairs；與已驗證候選 digest 完全相同且 fixed context 相同才能引用原 evidence。來源候選各自通過不使組合自動通過。

N cases、R replicates 的完整 executor slots 最多 `4 × N × R`，另加 2 proposal＋1 compose＋1 reflect stages；stage_retries=1 時最大 stage attempts 為其兩倍，仍受 max_probe_runs／pipeline 限制。每個 stage 啟動前計算完整驗證 envelope；不足以測完新 candidate／composition時跳過並保留已驗證候選。Reflection 是 optional；無預算直接 no_entry，已 admission 後仍須結算。

Evidence view 只含指定 case input、canonical結果、公開 verifier 結果與允許的候選 diff；所有內容帶來源 ref、限制大小並標為不可信資料。輸出只用 typed schema 解讀。首版無 hidden expected answer 的宣稱，也不將 proposer 自評當分數。

`ContrastLessonV1` 是 review bundle 內的候選文字，欄位為 cue、change_ref、baseline_observation_refs、candidate_observation_refs、verification_refs、counterevidence_refs、applicability、limitations。Reflect 回 `lesson` 或 `no_entry`；所有 refs 必須是本實驗已提交證據。候選文字不提升 memory confidence，不進 context lifecycle；fixture 的 lesson 永遠標 synthetic。無證據或 invalid reflection 保留診斷並輸出 no_entry。

## 11. CLI、報告與 review bundle

以下為新 CLI，既有 `improve experiment snapshot / candidate / compare` 維持原樣：

```bash
hufu improve probe plan --config experiment.yaml --json
hufu improve probe run --config experiment.yaml --workspace ./probe-workspace
hufu improve probe resume --experiment extraction_pair --workspace ./probe-workspace
hufu improve probe show --experiment extraction_pair --workspace ./probe-workspace --json
hufu improve probe export --experiment extraction_pair --workspace ./probe-workspace --out ./review-bundle
hufu improve refine --config refinement.yaml --workspace ./probe-workspace
```

plan 不建 coordinator、不讀 credential、不探測 provider、不建立 workspace；預設只輸出 stdout，顯示 source refs／resolved metadata 並清楚標出未完成 runtime preflight。`--out` 才原子寫入新 plan file，已存在就拒絕。run／refine 凍結 config 與 closure；同 experiment_id 已存在則要求 resume，不覆寫。resume 只用已凍結 artifact，不接受換 config／model／候選；source build 或 policy drift 拒絕。

show／export 不呼叫模型，不遷移來源 DB，不建立缺失的 journal。`--json` stdout 只有 JSON，diagnostic 到 stderr；plain 與 TUI 使用同 reducer，無硬編碼 ANSI／背景色。成功完成 eligible/no_change/simulation_only 的退出碼 0，rejected 為 1，無效 config／inconclusive／blocked 為 2；取消為 130。show 成功讀出歷史失敗報告仍 exit 0，讀取或 integrity 失敗 exit 2。

報告至少含：版本／build／fixture／variant IDs、effective models、execution_mode、逐 pair score／tokens／wall-clock、完整 coverage、各 gate、decision／reason_code、所有原始 attempts、baseline reuse、executor attributable tokens、pipeline spent/reserved/unknown、尚未回收 stage。時間只報觀察值；沒有延遲優化或美元節省結論。

Review bundle 包含 manifest、exact unified patch、baseline／candidate inventories、source base hashes、typed changes、strict report ref／digest、canonical evidence 物件及可選 lesson。export 先遍歷 ref closure並驗證來源，再原子建立新目錄；同目錄存在即拒絕，不能只匯出指向會被刪除的臨時 workspace 路徑。不得夾帶 credential、原始未清理 trace 或設定檔 secret。

產生 patch 後在全新 baseline copy 進行 round-trip，重新載入、核對 authored bytes／variant_digest等於已測 candidate。失敗就不提供可採用 bundle。這個檢查只作用於暫存 copy，不套用正式 team。Bundle 明示 `adoption_authorized: false`；fixture 加 `synthetic: true`。Bundle 方便人工審查；人員批准、送 PR、正式 apply 與部署均不列入 coding agent 的 Done 條件。

## 12. 分階段實作與驗收

| PR | 實作工作 | Done 條件 |
|---|---|---|
| 1：契約與 materializer | config／fixture／plan／manifest／report 型別、strict decoder、surface validation、closure、canonical digests、純比較、plan CLI | 完整範例能離線解析；Markdown／flat inline／versioned inline／shorthand 行為明確；T01–T07、T20；無模型或 workspace 初始化 |
| 2：耐久 controller | event payloads／pure reducer、CAS、stage／candidate envelope、deadline、identity registry、resume reconciliation interface | 注入 fake executor 測 T08–T14；跨 process 防重；replay 零 dispatch；不需要 provider |
| 3：手動 probe 閉環 | immutable runtime factory、通用單 task／identity callback、structured_only gate、canonical evidence adapter、run/resume/show、CLI／JSON／TUI projections | real coordinator＋scripted provider 完成 baseline／candidate；T15–T19、T21；fixture 決策不取得 review eligibility |
| 4：有界自動流程 | typed proposer／composer、stage scheduling、proposal retry、Compose 重測、deterministic selection | T22–T25；fake provider 能驅動所有分支；非法修改在 probe admission 前拒絕 |
| 5：可審核匯出與收尾 | exact patch、self-contained bundle、optional lesson、文件、完整範例與回歸 suites | T26–T28；bundle round-trip 完整；所有必需檢查通過；無人工採用／真實品質結果待辦 |

開始每個 PR 前讀目前 AGENTS.md 與相關權威文件，記錄 HEAD 差異；重用新增等效功能。不得為特定 team 在核心加分支。只實作該 PR 範圍；每階段更新本計畫進度及實際測試，不把未執行寫成通過。

### 12.1 可離線執行的測試矩陣

| ID | 情境 | 必須成立 |
|---|---|---|
| T01 | 相同固定條件，不同 arm roots、不同合法 prompt | comparison context 相同、variant 不同、原 policy hashes 各自保留 |
| T02 | 換 input／model／generation／tool／schema／verifier／budget | strict pairing 拒絕；新增未分類 policy 欄位使 coverage test 失敗 |
| T03 | Markdown、兩種 YAML、shorthand、alias／merge、多 worker | 僅指定 instructions／body 可改；變更共享 anchor 或其他 effective fields 拒絕 |
| T04 | preset-only worker、frontmatter、scripts、外部 skill、symlink／path traversal | provider 前拒絕，沒有正式檔案變動 |
| T05 | 同 patch 不同技能／vars／preset build；template 改權限 | variant／fixed digest 正確變化；不能沿用 evidence |
| T06 | 缺 case、重複／外來 replicate、baseline 共用 | 精確 multiset；coverage 不縮分母；baseline 只計費一次 |
| T07 | baseline task 失敗後 candidate 修好；baseline evidence 缺失 | 前者可改善，後者 inconclusive；舊 v1 baseline_control 原義不變 |
| T08 | 兩 process 同時 admission、CAS head 衝突 | 只有一個有效 reservation／generation；重讀後判斷 |
| T09 | admission／bound／dispatch 各點 crash | 第一個 provider call 前有真實 binding；dispatch 不明 blocked |
| T10 | child 完成但父未結算、重複／late／衝突 receipt | 匯入一次、不重跑；過期不覆蓋、衝突 blocked |
| T11 | provider timeout／未知 usage／回報超額 | 保留 reservation、停止新 stage；unknown 不填零 |
| T12 | resume 超 deadline／clock 倒退／重用結果 | 不重置時間或預算；原成本保留，新增花費可為零 |
| T13 | 取消時 callback／worker 在途 | drain 後關資源，沒有後續 admission 或提前釋放 |
| T14 | event commit 失敗、artifact 未引用、純 replay | 未提交不算完成；replay 不呼叫 executor |
| T15 | 嘗試 bash／view／agent／memory_query／MCP／external backend | effective tool list 與 central gate 都拒絕；零外部程序／網路 task call |
| T16 | grounded submission／schema invalid／assertion fail／verifier outage | 正確區分 pass、functional failure、unknown；canonical receipt 可核對 |
| T17 | 兩 arm 的 control／provider session／fixture cursors | 完全獨立；memory off；不讀正式 context 或舊 session |
| T18 | 增加 pre-run resolver／sidecar 呼叫，binding callback失敗 | 所有模型 admission 均被 fence；沒有未綁定的呼叫 |
| T19 | fixture program 成功、偽造 origin／改報告 decision | 仍 simulation_only；新 reader 由 journal 驗證，不信任 JSON |
| T20 | 無效 enum／未知欄位／null／巨大值／溢位；plan／show | 明確 field diagnostic；唯讀指令不啟動 provider、寫 DB 或取得 credential |
| T21 | 舊 reports／一般 run／舊 session resume | 無新增模型成本、無 policy hash 或既有 acceptance 語意改變 |
| T22 | proposal 越界／無效 schema後重試、slot 綁定後換內容 | bounded retry；typed validation；綁定後不可替換 |
| T23 | A、B 各自通過、Compose 回歸；Compose 等於 A | 新 digest 重測並拒絕回歸；相同 digest 才可 reuse |
| T24 | 預算不足完成 composition；reflection 無預算 | composition 不 admission；保留已驗證候選；reflection no_entry |
| T25 | quality tie／token gain／逐 pair 回歸／改 objective | 凍結規則與 tie-break 生效；平均分不能掩蓋回歸 |
| T26 | bundle patch round-trip／YAML 序列化差異／base drift | 輸出重建出相同已測內容與 digest；不符拒絕 export |
| T27 | 缺 lesson evidence／fixture lesson／惡意 output指令 | no_entry 或 synthetic；不改 memory／promotion、不執行指令 |
| T28 | export 中斷／目錄已存在／evidence 遺失；JSON／Markdown／status | 原子且不覆寫；失效證據拒絕；所有投影來自同 reducer |

Scripted provider 只回應凍結 program，可依事先定義的請求匹配條件測試行為；不得由實驗實際分數回填 fake 答案。測試可刻意設定 candidate pass／fail以驗證分支，但報告始終標示 synthetic。LLM transport 的成功、拒絕、usage missing、cancel 等使用 scripted transport contract tests；不需連真實服務。

### 12.2 必需驗證與停止點

修改程式碼後執行相關 focused tests，然後：

```bash
go build ./cmd/hufu
go test ./...
go vet ./...
golangci-lint run
bin/check-docs
```

對新 journal／reservation／lease 邊界執行 targeted `go test -race`。新增隱藏 team 目錄的 Go 測試時要另列命令，不能假設 `go test ./...` 涵蓋。Socket 暫存路徑限制使用短 TMPDIR 重跑並記錄實際結果。只修改本計畫不需要 Go lint。

本計畫的完成證據是：五個階段程式、離線整合與 failure tests、CLI、bundle及相符文件。執行前發現不支援的環境／模型配置屬 preflight 可測的拒絕案例，不是等待外部部署才能結案的工作。真實品質、成本回收、人工採用與部署均不作成功宣稱。

## 13. 本機查核來源

- `internal/improve/experiment.go`、`handoff.go`、`handoff_store.go`、`improve.go`
- `cmd/hufu/improve_experiment.go`、`team_setup.go`
- `internal/team/team_inline.go`、`team_composition.go`、`execution_contract.go`、`tool_sequence_policy.go`
- `internal/team/coordinator_run.go`、`coordinator_execute.go`、`verification.go`、`execution_policy_snapshot.go`
- `internal/team/execution_world_local.go`、`execution_world_isolated.go`、`event_store_conditional.go`
- `internal/team/budget_manager.go`、`cost_manager.go`、`internal/promotion/apply.go`
- `internal/evalharness/types.go`、`runner.go`、`internal/agent/agent.go`
- `AGENTS.md`、`docs/architecture/execution-runtime.md`、`docs/architecture/decision-runtime.md`
- `docs/reference/agent-format.md`、`docs/reference/team-manifest-contracts.md`、`docs/reference/improvement-artifact-schemas.md`

Harness² 的 Propose–Probe–Compose 只作設計背景，不是本計畫的收益保證或驗收依賴。此版移除研究結果、外部部署及人工採用工作，全部交付項均可由 coding agent 在 repository 內實作與離線驗收。
