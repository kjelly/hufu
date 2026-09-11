# Generic Required Resource Lock 實作計畫

> Status: proposed
> Priority: P0
> Baseline: `56b9abc3a5091a93697ae134578035853a2cd07e`
> Risk: high — pre-dispatch safety boundary

## 1. 目標

把目前 required skill 的局部保證提升為 runtime-owned generic resource lock：

```text
skill
prompt
project_rules
schema
```

在任何 worker/provider dispatch 前完成：

```text
resolve → canonicalize → read → hash → authorize → lock → persist → bind/inject
```

同一 run 不得因檔案被改動而靜默切版。

## 1.1 與既有設計文件的關係

`docs/architecture/strict-verification.md` §8（Status: draft，Authority:
reference，驗證於 `6ab9951`）已經有同一功能的目標 schema，對應 archived
roadmap 的 HF-PR-005（狀態仍是 OPEN；`internal/skill/`、`internal/team/`
目前都還沒有對應程式碼，確認是乾淨的待做工作）。本計畫的範圍比 §8 原本
描述的更廣（不只 skill，且加上 durable resume/replay），但**欄位與事件命名
統一沿用 §8**，不得另創一套命名——見 §4/§7 的標注。PR-4 完成後必須回頭比對
`docs/architecture/strict-verification.md` §8，確保文件跟最終 schema 一致。

## 2. 已存在能力（不得重做）

- `strict-verification` profile（`internal/team/profile.go:14`；既有的
  `RequireLockedResources bool` flag 語意見下方澄清）。
- `Coordinator.ValidateResourceLocks()`（`internal/team/coordinator.go:2732`）
  ——⚠️ 名字容易誤導：目前只做 workspace 目錄檢查（`EnsureWorkspaceDirs`）
  加 `CapabilityRequirement` 環境 probe（`internal/team/capability.go`），
  **不是**內容身分鎖，跟本計畫要建的機制是兩件事，只是恰好撞名。新的驗證
  入口必須用新名字（例如 `ValidateRequiredResourceLocks`），跟既有方法並存
  呼叫，不得假設可以直接在裡面塞新邏輯；PR-2 完成、行為穩定後再評估是否讓
  `ValidateResourceLocks()` 內部改叫新方法。
- required skill progressive disclosure / full fallback
  （`internal/team/coordinator_skills.go`）。
- `InjectedSkills` tracking（`internal/team/status.go:272`）。
- workspace separation（`ValidateWorkspaceSeparation`/
  `ValidateWorkspaceIsolationPaths`，`coordinator.go:2673`）。
- central tool authorization（`authorizeToolInvocation`，
  `internal/team/tool_policy_gate.go:582`）。
- canonical RunEvent / receipt / artifact infrastructure（`RunEvent` 於
  `internal/team/event_store.go:36`；`ArtifactStore`/`FileArtifactStore` 於
  `internal/team/evidence_store.go`；`ArtifactRef` 於
  `internal/team/task_result.go:18`）。

缺口是 generic lock object、durable binding、resume/replay identity。

現有 `RequireLockedResources`（`profile.go:50`）保持原意（gate workspace/
capability 檢查），不因本計畫改語意。若 team.yaml 宣告了 `required-resources`
（§5），不管 profile 是什麼都必須鎖定；是否要讓 `strict-verification`
額外強制「至少宣告一個 required resource」是產品決策，本計畫不預設，PR-2
只需保證「宣告了就一定鎖」。

## 3. 非目標

- remote registry / Internet fetch。
- runtime auto-update。
- agent 自己新增 required resource。
- resource lock 授予 tool/capability。
- 一次重寫 skill discovery。

## 4. Canonical model

欄位名沿用 `docs/architecture/strict-verification.md` §8.2（`Name`/`Path`），
不使用先前草稿的 `ID`/`Source`，避免同一功能出現兩套命名。

```go
type RequiredResourceKind string

const (
    ResourceSkill        RequiredResourceKind = "skill"
    ResourcePrompt       RequiredResourceKind = "prompt"
    ResourceProjectRules RequiredResourceKind = "project_rules"
    ResourceSchema       RequiredResourceKind = "schema"
)

type RequiredResourceSpec struct {
    Name       string
    Kind       RequiredResourceKind
    Path       string
    SHA256     string
    InjectInto []string
    Required   bool
}

// LockedResource 是可持久化的 metadata-only 記錄（invariant 9：event/report
// 只記 metadata 不記 content，見 §6）。SnapshotRef 指向既有 ArtifactStore
// （internal/team/evidence_store.go）存的完整內容快照，型別是既有的
// internal/team.ArtifactRef（task_result.go:18）。InjectInto 特意從
// RequiredResourceSpec 複製一份到這裡——相對 strict-verification.md §8.2
// 的原始模型多出的欄位——這樣 bind/inject 步驟只需要 LockedResource 就能
// 決定注入目標，不必回頭 join 原始 spec 清單。
type LockedResource struct {
    Name          string
    Kind          RequiredResourceKind
    CanonicalPath string
    SHA256        string
    ByteSize      int64
    LoadedAt      time.Time
    SnapshotRef   ArtifactRef
    InjectInto    []string
}

// LoadedResource 只存在於單次 run 的記憶體中：never persisted、never放進
// event payload。它是「read」步驟實際讀到的內容，bind/inject 一律用
// Content 注入，不得在 inject 階段重新讀檔案——這是 invariant 5（lock 後
// source 改動不影響本 run）的落地方式，也是 §8 ContextItem 範例的資料來源。
type LoadedResource struct {
    LockedResource
    Content string
}
```

Authoring `Path`（team.yaml 裡填的字串）不是 durable identity；持久化使用
`CanonicalPath`/`SHA256`。

## 5. Team config

```yaml
required-resources:
  - name: team-rules
    kind: project_rules
    path: AGENTS.md
    sha256: optional-authoring-pin
    inject-into: [coordinator, coder]
    required: true
```

`required-resources` 是 `TeamConfig`（`internal/agent/agent.go`）新的頂層欄位
（`[]RequiredResourceSpec`），跟既有 `Preflight`/`Requirements`/`Delegation`
同一種放法——flat 頂層 typed slice，不套用巢狀 wrapper——也跟
`docs/architecture/strict-verification.md` §26.4 的目標 schema 一致。

runtime 不猜哪些檔案該鎖，只鎖 team.yaml 明確宣告的項目。

## 6. Runtime invariants

1. 第一個 provider/model call 前完成 lock。
2. declared SHA mismatch → admission fail。
3. required source missing → admission fail。
4. symlink/canonical path escape → fail。
5. lock 後 source 改動，不改本 run input。
6. resume 驗證 locked-set digest。
7. resource lock 證明 identity，不代表內容可信或取得授權。
8. required resource 不得因 token budget 被 silent truncate。
9. event/report 只記 metadata，不記 content。
10. 未配置 resource 的 legacy/default team 行為零變更。
11. `LockedResource` 不持有 content；bind/inject 只能用同一次 read 到的
    in-memory `LoadedResource.Content`，不可在 inject 階段重新讀檔案
    （防 TOCTOU，見 §4）。

## 7. Persistence

```go
type LockedResourceSet struct {
    SchemaVersion int
    Resources     []LockedResource
    Digest        string
}
```

Canonical ordered digest 欄位：

```text
kind,name,canonical_path,sha256,byte_size,inject_targets
```

digest 刻意不含 `LoadedAt`（時間本質不 deterministic，納入會讓每次 resume
都算出不同 digest）；`SnapshotRef` 預設也不納入，保持跟舊 schema 相容。

持久化：

- session checkpoint projection（`internal/team/session.go` 的
  `SessionData`／`SaveSession`／`LoadSession`——只改這一個檔案，不要用
  `session*.go` glob，`session_tree.go` 等其他 session 檔案跟本計畫無關）。
- `resource_locked` event（跟 `docs/architecture/strict-verification.md`
  §8.3 和 archived roadmap 已經引用的事件名一致，不用先前草稿的
  `resource_lock_created`）。
- final evidence/run manifest ref。

不要另建第二 JSONL truth source。

## 8. Context integration

### Skill
沿用目前 disclosure；來源改為 locked snapshot。

### Prompt / project rules / schema

注入既有的 `ContextItem`（`internal/team/context_compiler.go:50`）——這是已經
存在、有 408 處使用的型別，不是新型別，欄位是 `Content`/`Source`/
`Required`/`Authority`/`ConflictKey`/`DedupKey`（沒有 `SourceID`）。
`Content` 必須來自 §4 的 `LoadedResource.Content`（同一次 read 到的
in-memory 內容），不可重新讀檔案：

```go
ContextItem{
    ID:          "locked_resource:" + loaded.Name,
    Kind:        "locked_resource",
    Content:     loaded.Content,
    Source:      "resource_lock",
    Priority:    PriorityHardConstraints,
    Required:    true,
    Authority:   ContextAuthorityNormative,
    DedupKey:    hashContentKey(loaded.Content),
    ConflictKey: "locked_resource:" + loaded.Name,
}
```

超過 context budget → explicit admission error，不可 drop required input。
這個 fail-closed 行為不用新寫：`ValidateRequiredItems`
（`context_compiler.go:250`）與 budget 階段（`context_compiler.go:362-413`）
已經對所有 `Required: true` 的 `ContextItem` 做這件事——把 locked resource
標成 `Required: true` 就會自動吃到既有保護，不需要另一套 budget-gate 邏輯。

## 9. 檔案範圍

新增（已核對：這 5 個檔名目前都不存在，是乾淨的新檔案，不會撞到既有無關
檔案）：

```text
internal/team/resource_lock.go
internal/team/resource_lock_digest.go
internal/team/resource_lock_replay.go
internal/team/resource_lock_test.go
internal/team/resource_lock_replay_test.go
```

修改候選（附目前行數；⚠️ 已超過 CLAUDE.md 800 行/檔上限）：

```text
internal/team/profile.go             242 行
internal/team/coordinator.go        2780 行 ⚠️
internal/team/coordinator_run.go    2383 行 ⚠️
internal/team/coordinator_execute.go 744 行
internal/team/coordinator_skills.go  878 行 ⚠️
internal/team/context_compiler.go    895 行 ⚠️
internal/team/session.go             306 行（只改這一個檔案，不要用
                                      session*.go glob——session_tree.go
                                      (941行)/session_md.go 等其他 13 個
                                      session*.go 檔案跟本計畫無關）
internal/team/event_types.go          81 行
internal/team/evidence_manifest.go   439 行
internal/team/parse.go              1746 行 ⚠️
internal/agent/agent.go             1817 行 ⚠️
cmd/hufu/team_setup.go               655 行
cmd/hufu/report.go                   983 行 ⚠️
```

### 9.1 File size caveat

標 ⚠️ 的 7 個檔案已經超過 800 行上限。跟先前
`spec-md-execution-target-refactor-review` 那次重構踩過同一個問題，處理
原則沿用同一個先例：**不要求先拆檔**，直接在既有檔案上加最小、可審查的
diff；`coordinator.go`/`coordinator_run.go` 這種已經遠超上限的檔案，本計畫
要加的應該只是呼叫 `resource_lock*.go` 裡邏輯的短函式，不是把邏輯本身寫進
這些大檔案。若某個 PR 對這些檔案的 diff 超過約 150 行，先檢討是不是該搬進
`resource_lock*.go`，而不是預設可以繼續往大檔案裡加。

## 10. PR 拆分

### PR-1 Schema + pure resolver
- types/parser/validation。
- canonical path + symlink scope。
- digest pure functions。

### PR-2 Persistence + replay
- locked-set event/checkpoint。
- conflict detection。
- legacy run 沒有 lock 時相容。

### PR-3 Context integration
- skill/prompt/rules/schema 使用 immutable snapshot。
- required input budget gate。

### PR-4 Report/audit/docs
- metadata-only observability。
- 回頭核對 `docs/architecture/strict-verification.md` §8：確認
  `RequiredResourceSpec`/`LockedResource`/`resource_locked` 事件命名跟最終
  實作完全一致（本文件已經對齊，這一步是防止 PR-1~3 過程中又岔開）。

## 11. Tests

```text
TestRequiredResourceMissingFailsAdmission
TestRequiredResourceDigestMismatchFailsAdmission
TestRequiredResourceSymlinkEscapeFails
TestRequiredResourceLocksBeforeProviderStart
TestLockedResourceMutationDoesNotChangeRunInput
TestLockedResourceSetSurvivesResume
TestLockedResourceReplayRejectsConflict
TestRequiredResourceCannotBeDroppedByTokenBudget
TestRequiredSkillUsesLockedSnapshot
TestDefaultProfileWithoutResourcesIsCompatible
TestResourceLockEventContainsNoContent
TestLockedResourceContentNeverReReadAtInjectTime
TestLockedResourceSetDigestExcludesLoadedAt
```

## 12. Migration

舊 team 無 `required-resources` → 完全相容。
不要自動把所有 `skills:` 轉 required resource；只有 explicit required declaration 建 lock。

## 13. Done

可宣稱：

```text
required resource identity is runtime-owned and immutable per run
```

而不是只有「檔案存在」。

## 14. Coding-agent 指派

先做 pure schema/resolver + digest，再接 durable event/session replay，最後接 ContextCompiler/required skill。
不得擴權、不得新增第二 truth store、不得在 required resource 超 budget 時 silent truncate。
