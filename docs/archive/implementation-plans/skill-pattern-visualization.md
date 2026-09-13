# Skill Pattern Visualization 實作規格

> Status: implemented and archived
> Priority: P3
> Baseline: `2fc04dd269aecfb9082ffa85fb5b6972ae5d1869`
> Existing base: detector + sidecar semantic merge + report + draft/promote lifecycle
> Completed: 2026-09-13

## 0. Implementation status

本規格已依三個可獨立驗證的階段完成：

| 階段 | Commit | 結果 |
| --- | --- | --- |
| PR-1 identity、attribution 與 snapshot | `8369656` | detector metadata/deep-copy、safe snapshot schema 與 round-boundary atomic projection |
| PR-2 graph projection 與 text/json CLI | `22da85c` | deterministic filter/aggregation、overflow protection、text/JSON 與 empty-state contract |
| PR-3 Mermaid 與 reference cleanup | `dcd5532` | raw Mermaid/escaping、三格式 CLI、stale projection regression fix 與 active reference 更新 |

最終驗證通過本文件第 12 節全部命令。此文件保留為設計與實作歷史；目前行為以 code、tests、CLI help 與 `docs/reference/skill-discovery.md` 為準。

## 1. 目標

新增一個不呼叫 LLM 的唯讀 CLI，顯示最近一次完成 skill-pattern candidate evaluation 的結果：

```bash
hufu skill graph
hufu skill graph --format text
hufu skill graph --format json
hufu skill graph --format mermaid
hufu skill graph --agent coder
hufu skill graph --min-frequency 10
```

預設格式為 `text`。CLI 只讀 workspace 中的 metadata-only projection，不 parse `report.md`、task transcript、event log 或 `SKILL.md`。

本功能補完 detected pattern visualization；不改變 detection 的門檻、quality filtering、semantic merge、candidate cap 或 draft approval policy。

## 2. 非目標

- 不新增 web UI、Graphviz 或其他外部 runtime dependency。
- 不重跑 detection，也不從歷史 tool calls 重建 pattern。
- 不跨 run 累積或合併 pattern；只保存最近一次完成的 evaluation。
- 不保存或顯示 raw tool arguments、tool output、task description、LLM prompt/response 或 `SpecificElements`。
- 不改變 skill draft 的 promotion/clean lifecycle。
- 不讓 projection 成為 detection、draft generation 或其他 runtime decision 的輸入。

## 3. 現況與必要的相容性修正

現有 `SkillPatternDetector` 是記憶體內物件；獨立執行的 `hufu skill graph` 無法取得 active coordinator。因此 persisted projection 是本功能的必要元件，不是 optional follow-up。

為產生正確且穩定的 projection，允許下列 metadata-only 修正：

1. `FindCandidates()` deep-copy `ToolSequence` 時必須保留 `Hash`。
2. `ToolSequence` 增加 `AgentCounts map[string]int`：
   - 新 sequence 初始化為目前 agent count `1`。
   - 已存在的相同 sequence 每次增加 total `Count` 時，同步增加對應 agent count。
   - semantic merge 時合併各 agent counts。
   - `GetSequencesByAgent(agent)` 改為依 `AgentCounts` membership 從 `sequences` 篩選，不再依賴目前無法表示 shared sequence 的 `sequenceByAgent` index。
3. `PatternCandidate` 增加 `SourcePatternIDs []string`：
   - 未 merge 的 candidate 含原始 `ToolSequence.Hash`。
   - semantic merge 後取所有來源 ID 的去重、遞增排序聯集。
4. `GetAllSequences()` 與 `GetSequencesByAgent()` 回傳 deep copy，不可在解鎖後暴露 detector 內部 pointer、slice 或 map。

以上只增加 attribution/identity metadata，不得改變 sequence hash、frequency count、sidecar prompt、篩選結果或排序優先級。

## 4. 資料流與持久化時機

```text
Coordinator round boundary
  -> SkillPatternDetector.FindCandidates(ctx) exactly once
  -> apply existing per-session candidate cap
  -> generate selected drafts from the same candidate slice
  -> build metadata-only SkillPatternSnapshot
  -> atomic replace workspace/skills/patterns.json

hufu skill graph
  -> load and validate workspace/skills/patterns.json
  -> apply agent/min-frequency filters
  -> aggregate nodes and edges
  -> render text/json/mermaid
```

### 4.1 Coordinator integration

重構 `checkSkillPatternsAndSave` 為接收已評估的 candidates，不可再次呼叫 `FindCandidates()`：

```go
func (c *Coordinator) checkSkillPatternsAndSave(
    candidates []skill.PatternCandidate,
) []skill.SavedPatternDraft
```

```go
type SavedPatternDraft struct {
    PatternID string
    Name      string
    Path      string // runtime notification only; never persisted
}
```

`checkSkillPatterns()` 的順序固定為：

1. 呼叫 `FindCandidates(ctx)` 一次。
2. 套用既有 `maxDrafts` cap。
3. 若有新 candidates，沿用既有確認流程產生 drafts。
4. 建立 snapshot，將本次 `SavedPatternDraft` 回填為 `draft_name`。generator 回傳 path 只可用 `filepath.Base(filepath.Dir(path))` 取出 name；projection 不保存 team directory 或絕對路徑。
5. 對本次未重新產生 draft 的相同 pattern ID，可沿用前一份 snapshot 的 `draft_name`。它表示曾產生過的歷史關聯，不宣稱 draft 目前仍存在或尚未 promote。
6. 無論 candidates 是否為空，都 atomic replace projection，避免上一次 evaluation 的候選被誤認為本次結果。
7. projection 寫入失敗只記錄 sanitized warning，不得讓 coordinator run 失敗；projection 不是 canonical runtime state。
8. 完成 projection 後才沿用既有 notification/reporting 流程。

`run_id` 使用 `c.contextRunID()`；projection 代表最近一次成功寫入的 evaluation，不代表整個 run 已完成。若某個新 run 尚未抵達 round boundary，舊 projection 保留，CLI 會明確顯示其來源 `run_id` 與 `generated_at`。

### 4.2 Projection 路徑與寫入

固定路徑：

```text
<workspace>/skills/patterns.json
```

- 使用 `team.AtomicWriteFile(path, data, 0o600)`。
- parent directory 沿用 `AtomicWriteFile` 的建立行為。
- 每次完整覆寫，不 append。
- JSON 最後保留一個 newline。
- projection 不得被讀回 detector 或 candidate evaluation。

## 5. Pattern identity

### 5.1 Source pattern ID

沿用目前 `ToolSequence.Hash` 的完整 SHA-256 hex。它由 tool names 與 normalized params 計算，但 projection 不保存 normalized params 本身。

### 5.2 Candidate pattern ID

先將 `SourcePatternIDs` 去重並以 Go bytewise lexical order 排序：

```text
candidate_id = "pat_" + hex(sha256(strings.Join(source_pattern_ids, "\x00")))
```

即使只有一個 source ID，也使用相同算法。不得使用 suggested name、LLM-generated name、map iteration order、agent、frequency 或 timestamp 產生 ID。

### 5.3 Semantic group ID

只有 semantic merge 確實合併兩個以上 source patterns 時才設定：

```text
semantic_group_id = "sg_" + hex(sha256(strings.Join(source_pattern_ids, "\x00")))
```

未 merge 時為空字串並由 JSON `omitempty` 省略。不得保存 sidecar 回傳的臨時整數 cluster ID，因為它不具跨 evaluation 穩定性。

## 6. Snapshot schema

```go
const SkillPatternSnapshotVersion = 1

type SkillPatternSnapshot struct {
    SchemaVersion int                   `json:"schema_version"`
    RunID         string                `json:"run_id"`
    TeamName      string                `json:"team_name"`
    GeneratedAt   time.Time             `json:"generated_at"`
    Patterns      []SkillPatternSummary `json:"patterns"`
}

type SkillPatternSummary struct {
    ID               string              `json:"id"`
    Tools            []string            `json:"tools"`
    ParameterClasses [][]string          `json:"parameter_classes"`
    Count            int64               `json:"count"`
    Agents           []SkillPatternAgent `json:"agents"`
    SemanticGroupID  string              `json:"semantic_group_id,omitempty"`
    DraftName        string              `json:"draft_name,omitempty"`
    FirstSeen        time.Time           `json:"first_seen"`
    LastSeen         time.Time           `json:"last_seen"`
}

type SkillPatternAgent struct {
    Name  string `json:"name"`
    Count int64  `json:"count"`
}
```

### 6.1 Schema invariants

- `schema_version` 必須等於 `1`。
- `run_id`、`team_name`、pattern `id` 與 `tools` 不可為空。
- `count` 及每個 agent count 必須大於零，且 agent counts 總和必須等於 pattern `count`。
- agent names 不可重複，依 `Name` 遞增排序。
- `parameter_classes` 長度必須等於 `tools` 長度；每個內層 slice 去重並依下列固定順序輸出。
- `first_seen <= last_seen <= generated_at`。
- patterns 依 `ID` 遞增排序。
- 所有 slice 在 JSON 中輸出為 `[]`，不可為 `null`。
- `draft_name` 若存在，必須符合既有 skill-name validation；不得包含 path separator。

### 6.2 Parameter classes

不得保存現有 `ToolSequence.Params` 字串。每個 step 只保存下列 allowlist class：

```text
none
file
url
number
hash
string
other
```

從記憶體中的 normalized param 判斷：出現相應 placeholder 時加入 `file/url/number/hash/string`；輸入為空時為 `none`；存在無法分類的非空內容時加入 `other`。已辨識 class 可與 `other` 並存，但 `none` 不可與其他 class 並存。任何情況都不可把原內容寫入 projection。

### 6.3 Loading behavior

- 檔案不存在：不是錯誤，回傳 `available=false`。
- 空檔、invalid JSON、unknown schema version 或 invariant violation：回傳錯誤；CLI exit non-zero、stderr 顯示 sanitized error、stdout 保持空白。
- reader 不做 migration，也不猜測未知版本。
- `draft_name` 是 evaluation 時已產生過 draft 的歷史關聯；loader 不讀取或檢查 team directory。

## 7. Graph model 與聚合規則

```go
type SkillPatternGraph struct {
    SchemaVersion     int                   `json:"schema_version"`
    SnapshotAvailable bool                  `json:"snapshot_available"`
    RunID             string                `json:"run_id,omitempty"`
    TeamName          string                `json:"team_name,omitempty"`
    GeneratedAt       *time.Time            `json:"generated_at,omitempty"`
    Patterns          []SkillPatternSummary `json:"patterns"`
    Nodes             []SkillPatternNode    `json:"nodes"`
    Edges             []SkillPatternEdge    `json:"edges"`
}

type SkillPatternNode struct {
    ID         string   `json:"id"`
    Tool       string   `json:"tool"`
    Count      int64    `json:"count"`
    Agents     []string `json:"agents"`
    PatternIDs []string `json:"pattern_ids"`
}

type SkillPatternEdge struct {
    From       string   `json:"from"`
    To         string   `json:"to"`
    Count      int64    `json:"count"`
    Agents     []string `json:"agents"`
    PatternIDs []string `json:"pattern_ids"`
}
```

### 7.1 Node identity

一個 node 代表一個 case-sensitive tool name，跨 agent 與 pattern 共用：

```text
node_id = "tool_" + hex(sha256(tool_name))
```

使用完整 digest，不得把 tool name 直接當 Mermaid identifier。

### 7.2 Filter order

先篩選 pattern，再 aggregate graph：

1. 有 `--agent NAME` 時，保留 `Agents` 中 exact case-sensitive match 的 pattern。
2. 有 `--min-frequency N` 時，保留 `Count >= N` 的 pattern。
3. filters 採 AND。
4. agent 不存在或 filter 後為空是成功的 empty graph，不是 usage error。

`--agent` 不重新按該 agent 的 count 計算 edge/node 權重；保留的 pattern 仍以總 `Count` 加權。這避免將 aggregate candidate 誤表達為 per-agent candidate。

### 7.3 Counts

對每個保留 pattern，令 `C = pattern.Count`：

- 每個 tool occurrence 都讓對應 node `Count += C`；同一 pattern 重複出現同一 tool 時逐 occurrence 計算。
- 每個相鄰 `(tools[i], tools[i+1])` 都讓對應 edge `Count += C`。
- 相同 adjacent pair 在同一 pattern 出現多次時逐次計算。
- self-edge 合法，不得丟棄。
- node/edge 的 `Agents` 是所有 contributing patterns 中 agent name 的聯集。
- `PatternIDs` 是 contributing pattern ID 的去重聯集。

所有加法必須檢查 `int64` overflow；overflow 回傳錯誤，不輸出 partial graph。

### 7.4 Ordering

- patterns：`ID`。
- nodes：先 `Tool`，再 `ID`。
- edges：先 `From`，再 `To`。
- 每個 `Agents` 與 `PatternIDs`：Go bytewise lexical order。

建圖為 `O(total tool occurrences)`；deterministic sorting 為 `O((P+V+E) log(P+V+E))`。

## 8. CLI contract

### 8.1 Flags

```text
--format text|json|mermaid   default text; value case-sensitive
--agent NAME                 optional exact agent-name filter
--min-frequency N            optional; default 0; N < 0 is usage error
```

未知 format 是 usage error，列出三個合法值。command 使用 `cmd.OutOrStdout()` / `cmd.ErrOrStderr()`，不可直接使用 global stdout/stderr，確保測試可注入 writer。

### 8.2 Missing snapshot

exit code 為 0。

Text：

```text
No skill-pattern snapshot is available for this workspace.
```

JSON（單行或 indent 均可，但 key order 由 struct 固定，slice 必須為 `[]`）：

```json
{
  "schema_version": 1,
  "snapshot_available": false,
  "patterns": [],
  "nodes": [],
  "edges": []
}
```

Mermaid 輸出 raw Mermaid，不加 Markdown fence：

```text
graph LR
  %% No skill-pattern snapshot is available for this workspace.
```

### 8.3 Existing snapshot with zero/filter-empty patterns

這與 missing snapshot 不同：`snapshot_available=true`，保留 snapshot 的 `run_id/team_name/generated_at`，但 `patterns/nodes/edges` 為空陣列。Text 顯示 header 與 `Patterns: 0`；Mermaid 顯示來源註解與 `%% No patterns matched.`。

### 8.4 Text format

格式固定如下；時間使用 UTC RFC3339Nano，draft 不存在時顯示 `-`：

```text
Skill pattern graph
Run: run-123
Team: demo
Generated: 2026-09-13T10:00:00Z
Patterns: 1

Patterns
  pat_abcd ×5 agents=coder draft=draft-edit-test
    read -> edit -> bash
    parameter-classes: [file] [file] [string]
    semantic-group: sg_abcd
    first-seen: 2026-09-13T09:55:00Z
    last-seen: 2026-09-13T09:59:00Z

Edges
  read -> edit ×5
  edit -> bash ×5
```

沒有 edge 時仍輸出 `Edges` header，下一行為 `  (none)`。

### 8.5 JSON stability

JSON 使用 `json.Encoder` 編碼 `SkillPatternGraph` 並以 newline 結尾。相同 snapshot 與 flags 必須 byte-for-byte 相同；render 不可重新產生 timestamp，也不檢查 draft filesystem state。

### 8.6 Mermaid escaping

輸出第一行固定為 `graph LR`。node 使用 opaque ID：

```text
  tool_<sha256>["escaped tool label"]
  tool_<sha256> -->|×5| tool_<sha256>
```

Label escape 順序固定：

1. `&` -> `&amp;`
2. `"` -> `&quot;`
3. `<` -> `&lt;`
4. `>` -> `&gt;`
5. CR、LF、TAB -> single ASCII space

連續 whitespace 不需折疊。count 只由 validated `int64` 以 decimal 輸出。nodes 與 edges 使用第 7.4 節順序。

## 9. 檔案變更

新增：

```text
internal/skill/snapshot.go
internal/skill/snapshot_test.go
internal/skill/visualization.go
internal/skill/visualization_test.go
cmd/hufu/skill_graph.go
cmd/hufu/skill_graph_test.go
```

修改：

```text
internal/skill/discovery.go
internal/skill/discovery_test.go
internal/team/coordinator_skill_patterns.go
internal/team/coordinator_skill_patterns_test.go
cmd/hufu/skill.go
docs/reference/skill-discovery.md
```

`cmd/hufu/report.go` 不負責寫 projection；report generation 不是 snapshot 可用性的前提。

## 10. 實作順序

### PR-1：identity、attribution 與 snapshot

- 補齊 detector deep-copy、`AgentCounts`、`SourcePatternIDs`。
- 新增 snapshot schema、validation、load/encode 與 parameter classification。
- coordinator 對同一 candidate slice 完成 draft generation 與 atomic projection。
- 暫不新增 CLI。

### PR-2：graph projection 與 text/json CLI

- 實作 filter、aggregation、ordering、overflow protection。
- 新增 `hufu skill graph`、text/json 與 empty-state contract。

### PR-3：Mermaid 與 reference docs cleanup

- 新增 deterministic Mermaid renderer 與 escaping。
- 更新 `docs/reference/skill-discovery.md`：
  - 移除 sidecar、report、promote 已完成項目的 stale TODO。
  - 修正 auto-generated draft path 為 `<team-dir>/skills/drafts/<name>/SKILL.md`。
  - 明確說明 draft promotion 前不在 LLM-facing skill pool。
  - 修正 `hufu skill review` 為顯示內容，不宣稱會開 editor 或自動 promote。
  - 新增 graph command、snapshot path、latest-evaluation 與 non-canonical 說明。
  - 驗證後更新 `Verified-Commit`。

每個 PR 都必須可 build、test、lint；PR-1 沒有 CLI 是允許的，但不得留下未使用的 exported API 或 lint failure。

## 11. 必要測試

### Detector metadata

```text
TestSkillPatternCandidatePreservesSourceHash
TestSkillPatternTracksIdenticalSequenceAcrossAgents
TestSkillPatternSemanticMergeUnionsSourceIDsAndAgents
TestSkillPatternSequenceAccessorsReturnDeepCopies
TestSkillPatternMetadataDoesNotChangeCandidateSelection
```

### Snapshot

```text
TestSkillPatternSnapshotRoundTrip
TestSkillPatternSnapshotDeterministicOrdering
TestSkillPatternSnapshotRejectsUnknownVersion
TestSkillPatternSnapshotRejectsInvalidInvariant
TestSkillPatternSnapshotClassifiesParamsWithoutContent
TestSkillPatternSnapshotContainsNoRawArguments
TestSkillPatternSnapshotStoresDraftNameWithoutPath
TestSkillPatternSnapshotEmptyEvaluationReplacesPreviousData
TestSkillPatternSnapshotAtomicWriteFailureDoesNotFailRun
TestSkillPatternSnapshotReusesDraftNameByPatternID
```

安全測試至少包含 unquoted token、URL query、hostname、absolute path、quoted secret 與 multiline command，並確認 bytes 中不存在原值。

### Graph/renderers

```text
TestSkillGraphDeterministicOrdering
TestSkillGraphAggregatesNodeCountsPerOccurrence
TestSkillGraphAggregatesRepeatedEdgeCounts
TestSkillGraphPreservesSelfEdges
TestSkillGraphFiltersByAgentBeforeAggregation
TestSkillGraphFiltersByMinimumFrequency
TestSkillGraphRejectsCountOverflow
TestSkillGraphJSONStable
TestSkillGraphMermaidEscapesNames
TestSkillGraphMissingSnapshotPerFormat
TestSkillGraphExistingEmptySnapshotPerFormat
TestSkillGraphUnknownFormatFails
TestSkillGraphMissingAgentReturnsEmptySuccess
TestSkillGraphRendersDraftName
```

### CLI integration

以 `t.TempDir()` workspace 寫入固定 snapshot，執行 Cobra command 並驗證 stdout/stderr、exit behavior 與三種 format。不得依賴 Ollama、network、TTY 或現有使用者 workspace。

## 12. Required validation

每個 code PR 都必須通過：

```bash
go test ./internal/skill/... ./internal/team/... ./cmd/hufu/...
go test -race ./internal/skill/...
go test ./...
go vet ./...
golangci-lint run
```

若 repository 的完整 test suite 有既存失敗，實作者仍須證明所有本次相關 package tests、race test 與 lint 成功，並附上完整 suite 的原始 failure；不得把本次引入的失敗標成既存問題。

## 13. Done criteria

- `hufu skill graph` 在完成一次 pattern evaluation 後，不需 provider/LLM 即可讀取最近 projection。
- 未執行 `--report` 也會產生 projection。
- command 不 parse human-readable report、SKILL.md、transcript 或 event log。
- detector candidate selection 與 baseline 相同；只增加 identity/attribution metadata 並移除重複 `FindCandidates()` 呼叫。
- text/json/mermaid 對相同輸入 deterministic。
- agent/frequency filter、counts、repeated edges 與 self-edges符合第 7 節。
- projection 與 CLI output 均不含 raw arguments/output/task descriptions/secrets。
- missing、empty、corrupt、unknown-version snapshot 行為符合第 6、8 節。
- stale reference docs 已修正。
- 所有第 12 節 validation gate 通過。

## 14. Coding-agent 指派

只實作本規格，不重寫 discovery。若發現必須改變 frequency、quality、semantic merge、candidate cap、draft approval 或 persistence canonicality，停止實作並先更新本規格，不可自行擴張範圍。
