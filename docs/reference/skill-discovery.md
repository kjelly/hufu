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

## 偵測流程

```text
worker tool call
  -> SkillPatternDetector.RecordToolCall
  -> sliding windows (預設 3–10 steps)
  -> frequency gate (預設至少 5 次)
  -> parameter normalization
  -> optional sidecar semantic grouping
  -> quality/candidate cap
  -> draft review flow
  -> atomic replace <workspace>/skills/patterns.json
```

### 工具序列與參數

`internal/skill/discovery.go` 依 agent 記錄工具呼叫，並建立連續工具序列。相同序列可包含多個 agent 的 attribution；candidate identity 來自原始序列 hash，不受 count、timestamp 或顯示名稱影響。

detector 內部會保留 normalized parameters 供草稿生成，但 projection 不保存原始 argument。`patterns.json` 只記錄 `none`、`file`、`url`、`number`、`hash`、`string`、`other` 等 allowlisted parameter classes。

### Sidecar 語意分組

provider execution boundary 啟動且 sidecar 可用時，coordinator 會將 sidecar 接到 detector，以批次語意分析協助合併相近 candidate。sidecar 不可用、timeout 或分析失敗時，偵測會退回 deterministic 的工具序列結果；因此 sidecar 是增強功能，不是 projection 或 graph command 的讀取依賴。

## 草稿生命週期

自動產生的草稿位於：

```text
<team-dir>/skills/drafts/<name>/SKILL.md
```

內建 default team 的 `<team-dir>` 等於 workspace。具名 team 則是已解析的 team directory；若要用 lifecycle CLI 操作該目錄，可明確傳入 `--workspace <team-dir>`。

高信心 candidate 可自動寫成草稿，其餘 candidate 先經使用者選擇。兩種路徑都只建立 draft，不會自動 publication。`DiscoverSkills(..., false)` 是提供給模型的預設 discovery 路徑，會排除 `skills/drafts/`；只有明確 promotion 後，技能才進入正式目錄。

常用命令：

```bash
# 列出正式技能與草稿；草稿會標記為 [draft]
hufu skill list
hufu skill list --drafts-only

# 顯示草稿的路徑與內容；不會開 editor，也不會 promotion
hufu skill review draft-view-edit-bash

# 人工審查後，將 skills/drafts/<name> 移到 skills/<name>
hufu skill promote draft-view-edit-bash

# 預覽或執行草稿清理
hufu skill clean --older-than 30d --unused
hufu skill clean --older-than 30d --unused --apply --yes
```

如需修改草稿，直接用編輯器編輯對應的 `SKILL.md`，完成審查後再執行 `hufu skill promote`。

## 模式 projection

每次 round-boundary 模式評估都會以 atomic replace 更新：

```text
<workspace>/skills/patterns.json
```

即使評估結果為空，也會寫入空的 `patterns` 陣列，以免舊 candidate 被誤認為目前結果。projection 代表最近一次成功寫入的 evaluation，不代表整個 run 已完成；`run_id`、`team_name` 與 `generated_at` 會標示來源。

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
