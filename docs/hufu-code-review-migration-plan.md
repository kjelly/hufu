# `hufu-code-review` 遷移與精簡計畫

> 狀態：提案
> 類型：consumer-specific migration plan
> Runtime 前置計畫：[hufu-generic-workset-evidence-plan.md](hufu-generic-workset-evidence-plan.md)
> 邊界：本文件可以描述 Git、diff、review roles、finding severity 與 report 格式；這些內容不得進入 Hufu core。

## 1. 目的

將 `hufu-code-review` 從以多個 agent prompt、Bash marker、task transcript parsing 維持 coverage 的設計，遷移到 Hufu 的通用 artifact-backed workset、typed-result assertion 與 aggregate completion contract。

完成後應：

- 保留完整、可追溯的 changed-hunk coverage；
- 降低 model calls、token 用量、protocol repair 與 wall time；
- 不再以 Bash 解析 task journal、NDJSON、JSON escaping 或 Markdown headings；
- 將 deterministic Git inventory 放在 team-owned adapter；
- 將 runtime 完成判定交給通用 receipts/verifiers；
- 將 review 品質標準與輸出格式留在 `.agent-teams/hufu-code-review/`。

本計畫不得在 `cmd/hufu/`、`internal/team/` 或 `internal/tools/` 增加 team 名稱、Git range、`batch-*`、review heading、severity 或 provider 文字特例。

## 2. 現況基線

目前 team 共有七個 agents：

- coordinator；
- inventory；
- general Go reviewer；
- runtime-integrity reviewer；
- boundary/TUI reviewer；
- security/tool reviewer；
- coverage verifier。

另有三支 Bash：

- `prepare-manifest.sh`：325 行；
- `verify-coverage.sh`：161 行；
- `mark-batch.sh`：20 行。

目前 agent/team 定義與 scripts 共約 1,397 行。最近一次保存的 run baseline：

- duration：1h23m35s；
- outcome：`partial`；
- stop reason：`budget_exceeded`；
- model calls：125；
- task execution：15；
- protocol failures：25；
- protocol repairs：6；
- tokens：2,448,072；
- acceptance 未到達可完成狀態。

遷移後須以同類範圍重新量測；不得只比較成功案例或縮小 review scope。

## 3. Consumer root causes

### 3.1 重複 reviewer fan-out

General reviewer 已檢查每個 batch，coordinator 又依 path 派遣 specialist。相同 diff/context 因此可能被讀取二至四次，且每次都有獨立的 `submit_result` protocol failure surface。

### 3.2 Prompt 重複 runtime policy

四個 reviewer prompt 重複：

- 只能讀五個 evidence files；
- 必須依固定順序 view；
- 不可輸出 prose；
- 必須立即 submit；
- 必須包含固定 Markdown sections；
- 必須重複 range、paths、ledger 與 `untruncated` 字樣。

這些要求同時存在於 coordinator prompt、worker prompts、static verifier 與 coverage script，造成多個互相漂移的事實來源。

### 3.3 Producer 產生大量低精度 context

現有 producer：

- 對每個 changed file 固定取前 180 行，而不是 hunk 周邊；
- 只從新增 `func` 行抽 caller symbol；
- 掃描 changed directory 下所有 tests，再取固定前 220 行；
- 將 source/caller/test 各自截斷到 400 KB。

Reviewer 被要求無條件讀取這些 context，即使與 hunk 無關；large context 又增加 truncation 與漏交 result 的機率。

### 3.4 Coverage verifier 驗證 presentation 而非 canonical result

`verify-coverage.sh` 解析 task-output NDJSON，匹配 escaped JSON、Markdown heading、特定字詞與 tool output rendering。這對 provider formatting、serializer 與 report presentation 都過度敏感。

### 3.5 Coverage 執行重複

Coverage-verifier agent 執行一次 script，blocking acceptance 又執行一次相同 script。若 coordinator 在 budget exhausted 前沒有呼叫 finish，兩層都無法挽救不完整 run。

### 3.6 Dead marker helper

`mark-batch.sh` 沒有呼叫點；marker 已由 `verify-coverage.sh` 建立。它應在 migration cleanup 中刪除。

## 4. 目標 team 拓撲

```text
.agent-teams/hufu-code-review/
├── team.yaml
├── coordinator.md
├── reviewer.md
├── critic.md                    # 可選；只複核高風險 findings
└── reviewprep/
    ├── main.go                  # team-owned ActionProvider adapter
    └── main_test.go
```

Runtime-owned producer action 取代 `inventory` LLM worker；若第一階段尚不能完全移除 worker，`inventory.md` 只能作為短期 shim，並使用 closed `[action, submit_result]` contract，不能保留任意 Bash。

### 角色契約

#### Coordinator

唯一責任：

1. 觸發 deterministic producer；
2. 對 manifest 執行一次 artifact-backed fan-out；
3. 綜合 typed findings；
4. 僅在高風險 finding 或 reviewer disagreement 時派 critic；
5. 呼叫 finish。

Coordinator 不讀 batches TSV、不重打 SHA/path、不計數 rows、不建立 marker、不執行 shell。

#### Reviewer

一個 reviewer 處理所有 work items。每個 manifest item提供 `lens` binding：

- `general`
- `runtime-integrity`
- `boundary-tui`
- `security-tool`

Lens 只影響 review checklist，不改變 tools、result schema 或 verification semantics。Reviewer 永遠 read-only。

#### Critic（可選）

只讀取已完成 reviewer 的 typed finding與其 evidence refs。符合任一條件才派遣：

- `[BLOCKER]`；
- security finding；
- finding 與另一 reviewer/result disagreement；
- coordinator無法判定是否有明確既有 contract 被違反。

Critic 不重新 review 所有 clean batches，也不修改檔案。

## 5. Team-owned producer adapter

### 5.1 所有權

Producer 實作位於 `.agent-teams/hufu-code-review/reviewprep/`。它可以理解：

- Git revision/window；
- changed paths；
- unified diff與hunks；
- Hufu package→lens routing；
- review batch limits。

Hufu core 只把它當成一個 configured `ActionProvider`。

### 5.2 輸入

透過 structured Action payload傳入，不從自由文字猜測：

```json
{
  "repository": ".",
  "since": "2.days.ago",
  "max_diff_bytes": 24000,
  "max_diff_lines": 600,
  "max_paths": 16
}
```

`since` 的來源優先順序：明確 CLI/team var > team default。自然語言若無法無歧義解析，coordinator 應 ask user或採記錄在 final report 的固定 default；不得在 core加入日期文字 parser。

### 5.3 輸出

Adapter 回傳通用 `ActionResult`，登錄：

- workset manifest artifact；
- 每個 item 的 bounded diff artifact；
- 必要時 paths artifact。

Workset bindings可以包含：

```json
{
  "item_id": "unit-0004",
  "lens": "security-tool",
  "range_start": "...",
  "range_end": "..."
}
```

Diff/path 必須用 artifact refs傳遞，不把絕對 workspace path當 proof。

### 5.4 不再預先生成的資料

第一版移除：

- `source-context.txt`；
- `caller-context.txt`；
- `test-context.txt`。

Reviewer 先讀 bounded diff，再依 hunk與finding候選使用 `view`/`grep`讀精確 source、caller與focused tests。Clean batch不需要為每個檔案製造假的完整 evidence ledger；severity finding仍必須具備完整證據鏈。

### 5.5 Adapter tests

至少覆蓋：

- empty range；
- one/multiple commits；
- deleted/renamed/binary files；
- oversized single hunk；
- hunk boundary split不漏行；
- deterministic item ordering；
- duplicate paths；
- invalid limits；
- dirty worktree不污染 commit-range diff；
- source revision在產生中改變時fail closed；
- package-to-lens routing。

## 6. `team.yaml` 遷移

### 6.1 保留

- `execution-profile: fresh-session`；
- `no-net: true`；
- `unattended: true`，但補齊明確 budgets；
- read-only reviewer tool allowlist；
- blocking acceptance；
- replay-safe read-only reviewer retry。

### 6.2 移除

- `coverage-verifier` worker；
- 四份重複 reviewer static task contracts；
- shell command acceptance；
- marker/coverage required artifacts；
- 為大量無 verifier tasks提高到 500 的 round/step/no-progress limits。

### 6.3 新增

- configured action provider：`produce-workset`；
- artifact-backed fan-out contract；
- reviewer `requires-result`與`requires-grounded-result`；
- `tool_call_assert`：要求讀取 manifest/diff artifacts並取得成功 result；
- `task_result_assert`：要求 summary、typed findings/files_read或明確 coverage gaps；
- `workset_complete` blocking acceptance；
- 明確 `max-duration`、`max-total-tokens`與較小的 retry limit。

實際 YAML schema以 generic runtime plan最終實作為準；本文件不得促使 runtime為 consumer欄位加入特例。

## 7. Reviewer result contract

Runtime驗證 typed `TaskResult`，不驗證 Markdown headings。

### Required typed fields

- `status`：`success`或`completed_with_gaps`；
- `summary`：非空；
- `files_read`：包含實際觀察的 diff與後續source/test paths；
- `findings`：可以為空；每個 finding使用現有通用 category/summary/detail；
- `risks`/`open_questions`：可選，用於未達severity門檻的項目；
- `details`：可選的完整review handoff，供coordinator synthesis，不參與core格式判定。

### Consumer severity rules

保留在 `reviewer.md`：

- BLOCKER/WARNING需要 changed `file:line`、reachable failure scenario、相關source/caller與focused test evidence；
- evidence不足時放入coverage gap/open question，不升級為finding；
- missing optional test本身不是finding；
- pre-existing/out-of-range問題不列為本輪finding；
- clean item允許零findings，不強迫產生NOTE。

### Coverage gap semantics

- required diff artifact不可讀：`blocked`；
- diff完整但相關caller/test不存在：`completed_with_gaps`；
- task因budget未讀完diff：`partial`，不得以repair補成clean success；
- protocol-only repair不得聲稱重新觀察了repair turn無法讀取的evidence。

## 8. Prompt 精簡

### Coordinator prompt目標

由約300行降到60–100行，只保留：

- review-only身份；
- producer → fan-out → optional critic → finish流程；
- finding deduplication與final report內容；
- coverage-limited不得呈現PASS。

移除所有已由runtime contract強制的tool order、artifact path、row expansion JSON範例與marker說明。

### Reviewer prompt目標

由四份高度重複prompt合併為一份，約100–150行：

- lens-specific checklist；
- finding evidence標準；
- typed result使用方式；
- read-only與scope限制。

不重複runtime已提供的`submit_result` schema、retry policy、workset identity或tool receipt規則。

## 9. 移除清單

Generic runtime contracts啟用並通過shadow/E2E後，刪除：

- `.agent-teams/hufu-code-review/mark-batch.sh`；
- `.agent-teams/hufu-code-review/verify-coverage.sh`；
- `.agent-teams/hufu-code-review/prepare-manifest.sh`；
- `.agent-teams/hufu-code-review/coverage-verifier.md`；
- `.agent-teams/hufu-code-review/runtime-integrity-reviewer.md`；
- `.agent-teams/hufu-code-review/boundary-tui-reviewer.md`；
- `.agent-teams/hufu-code-review/security-tool-reviewer.md`；
- 舊 `go-reviewer.md`，由新 `reviewer.md` 取代；
- coordinator內的fan-out JSON教學、marker與transcript protocol；
- `team.yaml`中的shell acceptance與四份duplicate verify specs。

若保留 specialist prompts做為skills/checklists，它們不得再作為可dispatch agents；應合併成team-owned reference或reviewer prompt sections。

## 10. 遷移階段

### Phase A：Baseline與adapter characterization

- 保存現有代表性run metrics；
- 對現有manifest/diff輸出建立golden fixtures；
- 先完成team-owned adapter tests，不改runtime outcome。

### Phase B：Shadow workset

- 同一次producer輸出同時供legacy TSV與generic workset讀取；
- 比較item count、item key、diff digest與ordering；
- shadow mismatch時legacy仍主導，但run標記不可遺失diagnostic。

### Phase C：Generic verifier enforcement

- child tasks改用artifact-backed fan-out；
- 啟用`task_result_assert`；
- `workset_complete`改為blocking；
- 停止建立新的review marker。

### Phase D：Topology精簡

- 合併reviewers；
- 啟用on-demand critic；
- 縮短prompts與budgets；
- 移除inventory/coverage LLM tasks。

### Phase E：Cleanup

- 刪除三支Bash與舊agents；
- 刪除legacy workspace marker consumption；
- 確認fresh run不再建立`coverage/reviewed/*.ok`；
- 保留必要migration note，不保留dead compatibility branch。

## 11. 驗證

### Team adapter與static validation

```bash
go test ./.agent-teams/hufu-code-review/reviewprep/...
go run ./cmd/hufu team validate --team hufu-code-review
go run ./cmd/hufu list hufu-code-review
go run ./cmd/hufu --agent-team hufu-code-review --dry-run "review recent commits"
```

### Runtime gate

任何Go source/test修改均須執行：

```bash
go test ./...
go vet ./...
golangci-lint run
```

Fan-out、task state或resume變更另執行：

```bash
go test -race ./internal/team/...
```

### Consumer E2E cases

1. empty commit window；
2. single small change；
3. multiple work items；
4. oversized hunk；
5. deleted/binary file；
6. one reviewer blocked；
7. one reviewer `completed_with_gaps`；
8. one task verification failure與retry；
9. budget exceeded；
10. cancel與resume；
11. stale/replaced manifest artifact；
12. confirmed security finding觸發critic。

每個case檢查run outcome、goal satisfied、acceptance state、expansion receipt counts、task statuses、report與process exit code一致。

## 12. 成功指標

### 正確性

- manifest每個item恰好對應一個child task；
- acceptance只能在所有required children verified後通過；
- stale artifact、partial、blocked、cancel或budget exceeded不能呈現PASS；
- final report finding可追溯至typed result與tool/artifact evidence；
- crash-resume不重算range、不重展開不同children。

### 精簡度

- 0支解析transcript/Markdown的script；
- 0個coverage marker；
- 0個coverage-verifier LLM task；
- 主要topology最多 coordinator + reviewer + optional critic；
- complex Bash降為0；
- team/config/prompt總行數顯著低於現有1,397行。

### 效率

以相近changed file/hunk規模比較：

- model calls至少降低50%；
- protocol failure/repair降低到0或有明確provider-independent原因；
- token用量至少降低50%；
- wall time至少降低40%；
- review coverage不低於baseline，且不以縮小scope取得改善。

## 13. 回退策略

- Generic runtime feature以schema/contract開關rollout，不以team名稱判斷。
- Shadow階段保留legacy reader，但不雙重派遣reviewers。
- 若new verifier與legacy結果不一致，run標記`partial`/diagnostic，不能自動選較寬鬆結果。
- Artifact-backed enforcement啟用後，不回退到接受path existence或prose claim。
- Consumer rollback只切回上一版team config/adapter；不得回滾或繞過runtime安全gate。

## 14. 完成標準

全部條件同時成立才算完成：

- [通用 runtime 計畫](hufu-generic-workset-evidence-plan.md)的WP-1至WP-5已完成；
- 至少兩個非review通用fixtures已證明abstraction名稱無關；
- producer adapter具完整tests且沒有複雜Bash；
- team只使用generic artifact/workset/result/group contracts；
- 舊三支Bash、coverage-verifier與固定specialist fan-out已移除；
- static validation、unit、race、vet、lint全部成功；
- 代表性read-only E2E完成且report品質人工檢查通過；
- metrics達標或差異有可驗證、非workaround的原因；
- Hufu core diff中搜尋不到team名稱、consumer檔名、severity或Markdown heading特例。
