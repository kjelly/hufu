# Hufu LTM Promotion：可實作計畫

## 1. 目標與範圍

建立一套人工控制的知識升級流程，把 Hufu 已確認且有重複成功證據的長期記憶，轉成可審查的：

- team coordinator policy
- agent policy
- reusable skill

核心不變量：

1. 分析階段不得修改 agent team Markdown 或正式 skill。
2. proposal 未經明確人工批准，不得套用。
3. 沒有合格來源時，必須輸出 `No suitable LTM entries found for promotion.`，exit code 為 0。
4. 原始 LTM/context item 永遠保留為 evidence；promotion 不得覆寫或 supersede 來源。
5. proposal 與套用結果必須可稽核、可重試，且不得因重試重複追加內容。

### 1.1 MVP 不做的事

- 不從單次失敗自動建立規則。
- 不自動批准、不在 unattended mode 代替人類批准。
- 不把 `context-ltm.md` 當資料來源；它只是可重建的 debug projection。
- 不自動修改 `team.yaml` 的 workflow、tools、capabilities 或 security 欄位。
- 不宣稱能從現有資料算出不存在的 `UsageCount` / `SuccessRate`；MVP 使用既有 experience aggregate。
- 不做跨 project 或跨 team promotion。
- 不直接修改 runtime contract。需要強制執行的安全規則仍應由 `team.yaml` 的 workflow/policy/capability/verification contract 人工建模。

## 2. 依目前程式碼做出的設計決策

### 2.1 權威 LTM 來源

權威來源是：

```text
<workspace>/context.sqlite
  context_items
  experience_aggregates
```

分析器只讀符合 scope 的 `ContextItem` 與同一 `policy_version` 的 `ExperienceAggregate`。舊的 `internal/memory.MemoryRecord` 必須先經既有 `hufu context migrate-memory` 匯入；Markdown LTM 不另建 parser。

### 2.2 CLI 放在既有 context 命令下

程式目前沒有 `hufu memory` command，但已有完整的 `hufu context` lifecycle、evidence、query 與 outcome tooling，因此新增：

```text
hufu context promotion analyze
hufu context promotion list
hufu context promotion show <proposal-id>
hufu context promotion edit <proposal-id> --draft-file <path>
hufu context promotion approve <proposal-id>
hufu context promotion reject <proposal-id> --reason <text>
hufu context promotion apply <proposal-id>
```

所有子命令共同接受：

```text
--workspace <path>       # context.sqlite / event_store.jsonl 所在 workspace
--project <id>           # 必填，canonical context project scope
--team <name>            # 必填，同時用於 context scope 與 TeamRegistry resolve
--team-search-path <csv> # 可選；預設沿用 team.DefaultSearchPaths()
--policy-version <id>    # 預設 memory-policy-v1
--json                    # 穩定的機器可讀輸出
```

`analyze` 額外接受：

```text
--type skill|team-policy|agent-policy  # 可選 filter
--agent <name>                         # agent-policy 時必填或由來源 scope 唯一決定
--model <model>                        # 產生 semantic draft；未指定則沿用 team sidecar/model 設定
--dry-run                              # 只列出 eligible source，不建立 proposal
```

`analyze` 不提供互動式自動批准；只有使用者明確執行 `approve <proposal-id>` 才算批准。不得提供 analyze-time `--yes`、`--auto-approve` 或 unattended safe default。

### 2.3 實際 target

目前 runtime 不載入 `team.md`，所以不能把它當 team policy target。MVP 定義：

| Promotion type | 實際 target | Runtime 意義 |
| --- | --- | --- |
| `skill` | `<team-dir>/skills/<name>/SKILL.md` | 成為該 team 可發現的正式 skill |
| `team_policy` | team 內 role 為 `coordinator` / `orchestrator` 的 agent `.md` | 約束 coordinator 的決策與 delegation |
| `agent_policy` | team 內指定 worker 的 `.md` | 只約束該 agent |

team 中只有一個 coordinator/orchestrator 時，`team_policy` 才能自動解析 target；零個或多個時 fail closed，要求使用者先修正 team 定義。team policy 不等於所有 worker 的強制安全 contract，CLI 輸出與文件必須明示此限制。

## 3. 使用者流程

```text
confirmed persistent context + experience aggregate
                         |
                         v
                  analyze / dry-run
                         |
              no source +------+ eligible source
                 |                    |
                 v                    v
        明確回覆無項目       persist proposed proposal
                                      |
                         show / edit / review
                                      |
                         +------------+-----------+
                         |                        |
                       reject                  approve
                         |                        |
                         v                        v
                      rejected              approved
                                                  |
                                                  v
                                          apply + preflight
                                                  |
                                      +-----------+----------+
                                      |                      |
                                  stale/error              applied
```

建議操作範例：

```bash
hufu context promotion analyze \
  --workspace workspace --project my-project --team hufu-dev

hufu context promotion show promo-abc123 \
  --workspace workspace --project my-project --team hufu-dev

hufu context promotion approve promo-abc123 \
  --workspace workspace --project my-project --team hufu-dev

hufu context promotion apply promo-abc123 \
  --workspace workspace --project my-project --team hufu-dev
```

## 4. Domain model 與狀態機

新增 `internal/promotion` service package，避免把 artifact 產生邏輯塞進 legacy `internal/memory`。為避免 `context <-> promotion` import cycle，持久化 record types 與 repository methods 放在 `internal/context/promotion.go`；`internal/promotion` 只依賴該 repository contract。

```go
type Type string

const (
    TypeSkill       Type = "skill"
    TypeTeamPolicy  Type = "team_policy"
    TypeAgentPolicy Type = "agent_policy"
)

type Status string

const (
    StatusProposed Status = "proposed"
    StatusApproved Status = "approved"
    StatusRejected Status = "rejected"
    StatusApplied  Status = "applied"
    StatusStale    Status = "stale"
)

type SourceSnapshot struct {
    ContextItemID    string
    ContentHash      string
    AggregateRevision int64
}

type Proposal struct {
    ID              string
    ProjectID       string
    TeamID          string
    Type            Type
    AgentID         string
    TargetPath      string // 相對 team dir；不得持久化任意 absolute path
    TargetBaseHash  string // target 不存在時為空
    Draft           string
    DraftHash       string
    PolicyVersion   string
    Sources         []SourceSnapshot
    Metrics         Metrics
    Status          Status
    RejectionReason string
    CreatedAt       time.Time
    UpdatedAt       time.Time
    AppliedAt       *time.Time
}
```

合法 transition：

```text
proposed -> proposed   # edit 產生新 draft revision
proposed -> approved
proposed -> rejected
approved -> applied
proposed|approved -> stale
```

`apply` 的暫時性錯誤不改 status，維持 `approved` 供安全重試；source revision 或 target hash 改變則設為 `stale`，必須重新 analyze，不能強制套用。

## 5. Eligibility 與分類

### 5.1 共同硬門檻

來源必須同時符合：

- `LifecycleConfirmed`
- `SupersededBy == ""`
- 未過期，且 metadata 表示 persistent lifetime/tier
- `Scope.ProjectID == --project`
- `Scope.TeamID == --team`
- 來源內容通過 `utils.RedactSecrets` 等值檢查；疑似 secret 的來源直接排除並記錄 content-free diagnostic
- 找得到指定 `policy_version` 的 `ExperienceAggregate`
- `VerifiedSupportCount >= MemoryLearningPolicy.MinConfirmedSupport`
- `IndependentTaskCount >= MemoryLearningPolicy.MinIndependentTasks`
- `CausalFailureCount / max(AppliedCount, 1) <= MemoryLearningPolicy.MaxHarmRate`

預設 policy 的門檻來自 `agent.DefaultMemoryLearningPolicy()`，不要在 promotion package 複製另一套常數。

### 5.2 Type 的 deterministic 邊界

先用 scope 與 kind 限縮，再交給 semantic generator：

- `agent_policy`：來源 `Scope.AgentID` 非空，且能唯一對應 team agent。
- `team_policy`：來源不得有 AgentID；kind 只接受 `instruction`、`decision`、`convention`、`architecture`。
- `skill`：kind 只接受 `pattern`、`convention`、`instruction`，且 semantic 結果必須包含至少兩個可驗證步驟。
- `progress`、`summary`、`open_question`、raw `tool_call`、raw `tool_result` 一律不 promotion。

若同一來源可成為多種類型，semantic generator 必須回傳排序後的建議；MVP 只為最高順位建立 proposal，避免一次 evidence 產生互相重疊的規則。

### 5.3 不再使用虛構 composite confidence

不實作原規格的：

```text
frequency * 0.3 + success_rate * 0.3 + reuse_value * 0.2 + stability * 0.2
```

因為目前沒有可靠的 `reuse_value` 與 `stability` 資料。輸出直接揭露可追溯的既有 metrics：

- utility lower bound
- applied / rejected count
- verified support count
- causal failure count
- independent task/project count
- aggregate revision

semantic model 只能分類與起草，不能提高 evidence 強度或繞過硬門檻。

## 6. Proposal 產生與驗證

### 6.1 兩階段 analyzer

`internal/promotion/analyzer.go`：

1. 從 canonical repository 查詢 exact project/team scope。
2. 讀 experience aggregates，執行硬門檻。
3. 依 kind/scope 建立 eligible source set。
4. 以 source ID 排序，確保 deterministic output。
5. 把 redacted source、metrics 與可選 target agent 送給 `DraftGenerator`。
6. 嚴格解析 generator 的 JSON schema。
7. 驗證 type、target、skill name、steps 與內容，再持久化 proposal。

`DraftGenerator` 使用 interface，單元測試用 fake；CLI adapter 可用現有 `agent.ProviderManager` + `sidecar.Sidecar.ExecuteProfile`。模型無法使用、timeout、JSON 不合法或內容含 secret 時，該 proposal 不落庫，command 回傳非零錯誤。不得用不完整的 deterministic prose 悄悄降級成正式 proposal。

### 6.2 Draft 格式

Skill proposal 的 `Draft` 是完整 `SKILL.md`，至少包含合法 frontmatter：

```yaml
---
name: protobuf-change
description: Run the verified protobuf change workflow.
---
```

Policy proposal 的 `Draft` 只包含要加入 agent body 的 Markdown，不包含或重寫 YAML frontmatter。套用時使用 idempotent marker：

```markdown
<!-- hufu-promotion:promo-abc123:start -->
## Promoted policy: protobuf generation

...
<!-- hufu-promotion:promo-abc123:end -->
```

`edit --draft-file` 只更新 proposal draft 與 `DraftHash`，status 必須仍為 `proposed`。讀入後先做 secret redaction check 與 type-specific validation。

## 7. Persistence、evidence 與 audit

### 7.1 SQLite schema

在 `internal/context` 的既有 migration 機制新增：

```text
promotion_proposals
  id, project_id, team_id, type, agent_id, target_path,
  target_base_hash, draft, draft_hash, policy_version,
  status, metrics_json, rejection_reason,
  created_at, updated_at, applied_at

promotion_sources
  proposal_id, context_item_id, content_hash, aggregate_revision
  PRIMARY KEY (proposal_id, context_item_id)

promotion_event_outbox
  idempotency_key, event_type, payload_json, created_at, delivered_at
```

repository 提供 create/get/list/update-status/update-draft，所有狀態更新使用 transaction。proposal ID 由 type、scope、target、target base hash、排序後 source IDs/source hashes 與 policy version 產生 deterministic digest；`DraftHash` 不納入 ID，否則 edit 會破壞 identity。重跑 analyze 若命中既有 proposal，回傳原 proposal，不覆寫人工 edit 或 lifecycle status。

### 7.2 Source evidence

建立 proposal 時加入 `derived_from` 關係或等價 source rows，但不得：

- 修改 source lifecycle
- supersede source
- 把 proposal Draft 注入 runtime context
- 在預設 list/show 輸出 source content

只有 `show --show-content` 才顯示經 redaction 的 draft 與 source 摘要。

### 7.3 Audit event

不要新增 `workspace/logs/memory_promotion.jsonl` 作第二個 truth source。沿用 `<workspace>/event_store.jsonl` 的 hash-chain event store，新增 content-free events：

```text
memory_promotion_proposed
memory_promotion_edited
memory_promotion_approved
memory_promotion_rejected
memory_promotion_applied
memory_promotion_stale
memory_promotion_apply_failed
```

payload 只記 schema version、proposal ID、source IDs、draft/target hash、target 相對路徑、policy version 與 actor；不得記 raw memory、draft 全文、provider key 或 command output。每次 lifecycle transaction 同時新增 deterministic outbox row；CLI commit 後把事件送進 event store。送出失敗時 command 回傳非零且保留 outbox，下一次任何 promotion command 先安全重送，避免 SQLite 與 audit log 之間出現永久缺口。

## 8. Apply contract

`internal/promotion/apply.go` 的 preflight 順序固定：

1. proposal status 必須為 `approved`。
2. 重新解析 `TeamRegistry`，target team 必須存在且與 proposal team 相符。
3. target relative path 經 clean、symlink resolution 後仍在 team dir 內。
4. 每個 source 仍為 current confirmed persistent context。
5. 每個 source `ContentHash` 與 aggregate `Revision` 必須等於 snapshot。
6. proposal `DraftHash` 必須與內容一致，且內容不含 secret-like material。
7. target 現況 hash 必須等於 `TargetBaseHash`；原本不存在者現在仍須不存在。
8. type-specific parser 必須接受套用後內容。

寫入規則：

- 使用 `team.AtomicWriteFile` 或抽出共用 atomic writer；不得直接覆寫。
- policy 只在 agent body 尾端加入 promotion marker，不改 frontmatter、tools、role、model 或 guard。
- skill 只能建立新的 `<team-dir>/skills/<name>/SKILL.md`，不得覆蓋既有 skill。
- 寫入後重新 parse：透過新增的 `team.ValidateAgentFile` 確認 agent name/role/frontmatter identity 不變；skill 透過匯出的 validation helper 驗證，並可被 `skill.DiscoverSkills` 找到。
- 驗證成功後才把 proposal 設為 `applied` 並 emit event。
- 同一已 applied proposal 再次 apply 回報 already applied，exit code 0，不重複寫入。

若檔案寫入成功但 SQLite status 更新失敗，下一次 apply 以 promotion marker 或相同 target hash 判定已寫入，補寫 status/event，不再追加第二次。

## 9. 實作工作包

### WP-1：資料模型與 repository

修改／新增：

- `internal/context/promotion.go`
- `internal/context/sqlite_repository.go`
- `internal/context/migrations_test.go`
- promotion repository tests

交付：migration、CRUD、state transition validation、deterministic ID、source snapshot、transaction 與 restart persistence。

### WP-2：唯讀 eligibility analyzer

修改／新增：

- `internal/promotion/analyzer.go`
- `internal/promotion/eligibility.go`
- analyzer tests with fake repositories

交付：canonical query、scope isolation、policy threshold、stable ordering，以及沒有合格項目的明確結果。此 WP 不呼叫模型、不寫 proposal、不碰 team files。

### WP-3：semantic classification 與 draft

修改／新增：

- `internal/promotion/generator.go`
- `internal/promotion/validate.go`
- `cmd/hufu/context_promotion_cmd.go`
- strict JSON fixtures/tests

交付：`DraftGenerator` interface、sidecar adapter、schema validation、secret rejection、proposal persistence、`analyze/list/show/edit`。

### WP-4：人工 lifecycle

修改／新增：

- `cmd/hufu/context_promotion_cmd.go`
- `internal/promotion/service.go`
- CLI tests

交付：`approve`、`reject`、scope authorization、明確 approval command、stale transition、JSON/text output。

### WP-5：原子 apply

修改／新增：

- `internal/promotion/apply.go`
- `internal/skill` 匯出的 draft validation helper（重用既有 parser，不複製 parser）
- `internal/team` 匯出的 agent-file validation helper（重用 `parseAgentFile`）
- `cmd/hufu/context_promotion_cmd.go`
- filesystem integration tests

交付：target resolution、hash/revision preflight、atomic create/append、post-parse validation、crash-retry idempotency。

### WP-6：events、文件與 completion

修改／新增：

- `internal/team/execution_events.go` 或既有 event registry/reducer 的對應位置
- `cmd/hufu/completion.go`
- CLI help / README / AGENTS.md command reference（若 feature 完成）
- event redaction/hash-chain tests

交付：完整 content-free audit events、shell completion 與操作文件。

## 10. 測試矩陣

### Eligibility

- 單次 failure 或沒有 aggregate：0 proposal。
- confirmed + 足夠 verified support + independent tasks：eligible。
- candidate/rejected/superseded/expired/session-only：排除。
- causal harm 超過 policy：排除。
- project/team/agent scope 不符：不可見。
- private agent memory 不得生成 team policy。

### Proposal lifecycle

- analyze 重跑不建立 duplicate。
- invalid model JSON、unknown type、空 draft、secret：fail closed。
- edit 更新 draft hash，不能 edit approved/rejected/applied proposal。
- approve/reject 只接受 proposed。
- reject 不改 LTM、不改 Markdown、不建立 skill。
- process restart 後 proposal/status 仍存在。

### Apply

- 未 approve 不可寫檔。
- source hash 或 aggregate revision 改變時變 stale。
- target 被人工修改後不可套用。
- path traversal 與 symlink escape 被拒絕。
- existing skill 不被覆蓋。
- policy apply 保留所有 frontmatter。
- post-write parse 失敗時 proposal 不標 applied。
- 模擬「檔案已寫、status 未更新」後重試，不重複追加。
- event payload 與 CLI default output 不洩漏 source/draft secret。

### CLI acceptance

```text
Case 1: 無合格 LTM
exit 0；文字明確包含 No suitable LTM entries found for promotion.

Case 2: analyze
只新增 proposed proposal；team Markdown 與 skills 內容完全不變。

Case 3: reject
proposal=rejected；source context 未改；target 未改。

Case 4: approve without apply
proposal=approved；target 仍未改。

Case 5: approve + apply
只修改 proposal 指定 target；post-parse 成功；proposal=applied；audit event 存在。

Case 6: stale evidence
apply 不寫 target；proposal=stale；訊息要求重新 analyze。
```

## 11. 驗證指令與完成定義

每個修改 Go source/test 的 WP 都必須依序執行：

```bash
go test ./...
go vet ./...
golangci-lint run
```

feature 完成需同時滿足：

- 上述三個 command 全部成功。
- CLI text 與 JSON contract 測試通過。
- SQLite migration 可從既有 database 升級且可重開。
- proposal、source evidence、target write、event audit 能在 integration test 串成完整流程。
- 分析、批准、套用是三個可分離步驟；任一步 crash 都不會自動改寫第二次。
- 沒有任何 runtime 路徑讀取 proposed/rejected promotion draft。

## 12. 後續 L3/L4 extension

MVP 穩定後再做：

1. 把 auto-skill pattern 與 LTM promotion 共用同一個 `Proposal` / approve / apply pipeline。
2. 以 applied proposal ID 回寫後續 skill/policy usage outcome，但仍透過 event → aggregate projection，不直接累加可漂移的 counter。
3. 根據 verified outcomes 提出 deprecate/supersede proposal；仍需人工批准。
4. 若要真正的 team-wide stable policy，另立規格新增 runtime 載入的 team policy contract，並同時覆蓋 coordinator、worker、direct-agent、resume、dry-run 與輸出 projection；不得把只寫入但不載入的 `team.md` 當完成。
