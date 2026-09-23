# 自動技能模式發現與視覺化

> Status: active
> Authority: reference
> Verified-Commit: 2026-09-13
> Supersedes: —
> Superseded-By: —

## 功能概述

hufu 會在 coordinator 執行期間記錄工具呼叫，偵測重複的工具序列，並在每次模式評估後寫出 metadata-only projection。符合條件的模式可以產生技能草稿，但草稿在人工 promotion 前不會進入 LLM-facing skill pool。

模式偵測、草稿與 projection 是三種不同資料：

- detector state 是 coordinator process 內的暫存統計。
- 技能草稿是待人工審查的 `SKILL.md`。
- `patterns.json` 是供 `hufu skill graph` 使用的非 canonical、只含安全聚合 metadata 的最新評估投影。

## 技能搜尋路徑與優先順序

具名 team 會依下列順序搜尋技能：

1. `<team-dir>/skills/<skill-name>/SKILL.md`
2. `<cwd>/.agents/skills/<skill-name>/SKILL.md`
3. `~/.agents/skills/<skill-name>/SKILL.md`

技能名稱不分大小寫；同名技能只保留第一個找到的定義，因此 team-local 定義會覆蓋 project 與 global 定義，project 定義會覆蓋 global 定義。`skills/drafts/` 不會進入預設的 LLM-facing skill pool；只有明確 promotion 到正式技能目錄後才參與上述優先順序。

## 偵測流程

```text
worker tool call
  -> SkillPatternDetector.RecordToolCall
  -> sliding windows (預設 3–10 steps)
  -> frequency gate (預設至少 5 次)
  -> parameter normalization
  -> sidecar semantic grouping and parameter qualification
  -> quality/candidate cap
  -> draft review flow
  -> atomic replace <workspace>/skills/patterns.json
```

### 工具序列與參數

`internal/skill/discovery.go` 依 agent 記錄工具呼叫，並建立連續工具序列。相同序列可包含多個 agent 的 attribution；candidate identity 來自原始序列 hash，不受 count、timestamp 或顯示名稱影響。

detector 內部會保留 normalized parameters 供草稿生成，但 projection 不保存原始 argument。`patterns.json` 只記錄 `none`、`file`、`url`、`number`、`hash`、`string`、`other` 等 allowlisted parameter classes。

### Sidecar 語意分組

provider execution boundary 啟動且 sidecar 可用時，coordinator 會將 sidecar 接到 detector，以批次語意分析協助合併相近 candidate，並評估參數能否泛化。candidate qualification 需要 sidecar；sidecar 不可用時本次評估不會產生 candidate。若個別 sidecar 分析 timeout 或失敗，該分析會依 detector 的 fail-closed 規則退回、降分或排除 candidate，不保證保留 deterministic 工具序列結果。

已持久化的 projection 與 `hufu skill graph` 不需要 sidecar：graph command 只讀取最新成功寫入的 `patterns.json`，不會呼叫 provider 或重新評估 candidate。

## 草稿生命週期

自動產生的草稿位於：

```text
<team-dir>/skills/drafts/<name>/SKILL.md
```

內建 default team 的 `<team-dir>` 等於 workspace。具名 team 則是已解析的 team directory；lifecycle CLI 可用 `--team <name>` 經由和執行期相同的 `TeamRegistry` 搜尋規則定位。未指定 `--team` 時保留 workspace-based 行為；必要時仍可用 `--workspace <team-dir>` 明確指定目錄。

高信心 candidate 可自動寫成草稿，其餘 candidate 先經使用者選擇。兩種路徑都只建立 draft，不會自動 publication。`DiscoverSkills(..., false)` 是提供給模型的預設 discovery 路徑，會排除 `skills/drafts/`；只有明確 promotion 後，技能才進入正式目錄。

常用命令：

```bash
# 列出正式技能與草稿；草稿會標記為 [draft]
hufu skill list
hufu skill list --drafts-only
hufu skill list --team my-team --drafts-only

# 顯示草稿的路徑與內容；不會開 editor，也不會 promotion
hufu skill review draft-view-edit-bash
hufu skill review draft-view-edit-bash --team my-team

# 人工審查後，將 skills/drafts/<name> 移到 skills/<name>
hufu skill promote draft-view-edit-bash
hufu skill promote draft-view-edit-bash --team my-team

# 預覽或執行草稿清理
hufu skill clean --older-than 30d --unused
hufu skill clean --older-than 30d --unused --apply --yes
hufu skill clean --team my-team --older-than 30d --unused
```

`promote` 會把 `draft-` 前綴從目錄名稱去掉，並同步改寫 frontmatter 的 `name:`（runtime 以 frontmatter name 作為技能名稱，所以 `draft-foo` promote 後會以 `foo` 載入）。promote 前以 `ValidateSkillDraft` 驗證改寫後的內容：缺少 `description` 或 body 的草稿會被拒絕，草稿保持原狀。

`--team` 適用於 `list`、`review`、`promote` 與 `clean`，並使用 `--agent-team-search-path`（若有設定）或預設 team search paths。具名 team 的 `clean --unused` 會從和執行期一致的 `<base-workspace>/<team-name>/.skill-usage.json` 判斷使用狀態；`--workspace` 在此代表 base workspace。`graph` 仍以 execution workspace 的 projection 為準，使用 `--workspace` 選擇來源。

如需修改草稿，直接用編輯器編輯對應的 `SKILL.md`，完成審查後再執行 `hufu skill promote`。

## 模式 projection

每次 coordinator round-boundary，以及每次已建立 task 的 direct-agent invocation 結束時，模式評估都會以 atomic replace 更新：

```text
<workspace>/skills/patterns.json
```

即使評估結果為空，也會寫入空的 `patterns` 陣列，以免舊 candidate 被誤認為目前結果。若 invocation context 已取消，evaluation 會停止且不覆寫上一份已完成 projection。projection 代表最近一次成功寫入的 evaluation，不代表整個 run 已完成；`run_id`、`team_name` 與 `generated_at` 會標示來源。

projection 不是 canonical execution state，也不會被 detector 讀回。它不包含 raw tool arguments、task descriptions、model output、transcript 或 draft filesystem path。`draft_name` 只保留模式與曾產生草稿的歷史關聯；loader 不會檢查草稿目前是否仍存在。

檔案不存在代表尚無 snapshot，不是錯誤。空檔、invalid JSON、不支援的 schema version 或 invariant violation 會讓 graph command 以非零狀態失敗，不會猜測或 migration 未知格式。

## `hufu skill graph`

graph command 只讀取 `patterns.json`；它不呼叫 provider/LLM、不重新執行 detector，也不 parse report、event log、transcript 或 `SKILL.md`。

```bash
hufu skill graph
hufu skill graph --format text
hufu skill graph --format json
hufu skill graph --format mermaid
hufu skill graph --agent coder
hufu skill graph --agent coder --min-frequency 10
```

flags：

- `--format text|json|mermaid`：case-sensitive，預設 `text`。
- `--agent NAME`：以 exact case-sensitive agent name 篩選。
- `--min-frequency N`：保留總 count 大於等於 `N` 的 pattern；`N < 0` 是錯誤。

filter 先作用於 patterns，再建立 graph，且多個 filter 採 AND。agent 不存在或無 pattern 符合時仍成功，並保留 `snapshot_available=true` 與 snapshot metadata。指定 agent 不會把 count 改算成該 agent 的局部 count；保留 pattern 仍以總 count 加權。

### 聚合語意

- node 代表 case-sensitive tool name；ID 是 `tool_` 加完整 SHA-256 digest。
- pattern 每出現一次 tool，node count 就加上該 pattern 的 count；重複 occurrence 會重複計算。
- 每個相鄰 tool pair 形成 edge；重複 edge 與 self-edge 都會保留並累加。
- node/edge 會列出 contributing agents 與 pattern IDs 的去重、排序聯集。
- 所有 `int64` 加法都會檢查 overflow，失敗時不輸出 partial graph。

輸出順序固定為 pattern ID、node tool/ID、edge from/to；JSON slice 永遠輸出為 `[]` 而不是 `null`。相同 snapshot 與 flags 會產生 byte-for-byte 相同 JSON。Mermaid 是未包 Markdown fence 的 raw source，tool label 會 escape `&`、`"`、`<`、`>`，並把 CR/LF/TAB 換成 ASCII space。

缺少 snapshot 時，text 顯示：

```text
No skill-pattern snapshot is available for this workspace.
```

Mermaid 則輸出：

```text
graph LR
  %% No skill-pattern snapshot is available for this workspace.
```

## Report 的關係

`--report` 可在 execution report 中附帶當次 coordinator 記憶體內的模式摘要，但 report 不是 graph data source。未使用 `--report` 仍會產生 `patterns.json`；graph command 的可用性不依賴 report generation。

## 實作位置

- `internal/skill/discovery.go`：工具序列、attribution、candidate 與 optional semantic merge。
- `internal/skill/generator.go`：從 candidate 建立 draft `SKILL.md`。
- `internal/skill/snapshot.go`：安全 projection schema、validation、encode/load。
- `internal/skill/visualization.go`：filter、graph aggregation、text/Mermaid rendering。
- `internal/team/coordinator_skill_patterns.go`：round-boundary evaluation、draft flow 與 atomic projection。
- `cmd/hufu/skill_graph.go`：text、JSON、Mermaid CLI。

## 驗證

```bash
go test ./internal/skill/... ./internal/team/... ./cmd/hufu/...
go test -race ./internal/skill/...
go test ./...
go vet ./...
golangci-lint run
```

主要限制是 sliding-window 偵測成本會隨工具呼叫數增加，semantic grouping 的品質取決於 sidecar；兩者都不改變 projection 的 non-canonical、metadata-only 邊界。
