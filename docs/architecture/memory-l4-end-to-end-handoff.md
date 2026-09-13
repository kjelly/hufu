# Memory L4 End-to-End Handoff 實作規格

> Status: implemented
> Priority: P2
> Baseline: 3e5054b
> Verified-Commit: 3f94103
> Scope: 串接既有 memory / consolidation / promotion / improve artifacts；禁止 autonomous production mutation
> Authority: implementation contract for the handoff layer

本文件只定義 handoff layer：保存跨 subsystem 的 typed references、revision
bindings 與 review lifecycle。它不重新設計 memory、context promotion 或
experiment subsystem。

若本文件與既有 canonical subsystem contract 衝突，優先順序是：

1. internal/context 的 schema、repository 與測試。
2. internal/promotion 的 service、validator 與測試。
3. internal/improve 的 artifact schema、loader 與測試。
4. 本文件的 handoff projection 與 CLI 行為。

## 1. 已存在與需補上的 primitive

可直接重用：

| Capability | Canonical implementation | Handoff 用途 |
|---|---|---|
| Experience aggregate | internal/context.ExperienceAggregate | source eligibility 與 revision binding |
| Consolidation proposal | internal/context.ConsolidationProposal | context candidate 的 proposal ref |
| Promotion proposal | internal/promotion.Proposal / internal/context.PromotionProposal | skill / policy proposal ref |
| Memory policy snapshot | internal/improve.MemoryPolicySnapshot | policy candidate 與 revision binding |
| Benchmark fixture | internal/improve.BenchmarkFixture | immutable benchmark binding |
| Team snapshot | internal/improve.TeamSnapshot | skill candidate 的 immutable team overlay |
| Experiment report | internal/improve.ExperimentReport | deterministic evaluation result |
| Adoption / monitoring | internal/improve.Adoption、MonitoringReport | rollout evidence |
| Memory policy rollback | improve.RollbackMemoryPolicy | previous policy / rollback evidence |

本工作已補上的 durable / typed primitive：

- `MemoryPolicyOptimizationProposal` 有 ID、revision、loader 與 durable store。
- Skill promotion draft 可產生 isolated candidate team snapshot 與 review patch。
- `ExperimentReport` 的 arm/input 可綁定 baseline/candidate memory-policy snapshot refs。
- Runtime memory manifest 與 `memory_retrieved` / `memory_usage_recorded` events
  保存 content-free 的 context item ID、ContentHash 與 policy version；`Report`
  只從指定 run 的 runtime events 產生 `AppliedContextRefs` 與
  `MemoryPolicyVersions`。
- Experiment、adoption 與 monitoring loaders 會重算 deterministic outcome、
  snapshot content/patch digest 與 cross-artifact bindings，拒絕修改過的 JSON。
- Handoff monitoring 另將 baseline metrics 綁回同一 handoff 的 immutable
  experiment；rollback suggestion 必須指向 skill adoption、baseline memory
  policy 或 baseline team snapshot 所能驗證的 rollback target。

以上 primitive 仍只保存 metadata、refs 與 immutable snapshots；不會自行 apply、activate
或 rollback 正式環境。

## 2. 目標與非目標

目標流程：

~~~text
eligible source -> canonical proposal -> immutable candidate
-> immutable benchmark binding -> deterministic experiment result
-> eligible_for_review -> explicit human approval
-> canonical adoption/apply evidence -> monitoring / rollback recommendation
~~~

handoff 是 metadata artifact，不是 execution truth，也不取代 context item、
experience aggregate、promotion proposal、memory policy snapshot、team
snapshot、benchmark、experiment report、EventStore、session checkpoint、
artifact store、verification 或 acceptance。

禁止：

- agent 自動 merge PR、activate policy 或 apply promotion；
- prepare、evaluate 或 approve 修改正式 team、正式 skill 或 active policy；
- side-effect task online exploration；
- 單一 outcome 宣稱因果關係；
- single run 直接產生 production skill；
- handoff 複製 source content、prompt、agent output、tool arguments 或 secrets。

## 3. Canonical schema

### 3.1 Typed refs

所有跨 artifact 關係都必須使用 typed ref，不得使用無 kind 的 string。

~~~go
type ArtifactRef struct {
    Kind     string // JSON: kind; artifact type
    ID       string // JSON: id
    Revision string // JSON: revision hash or canonical revision
}

type HandoffScope struct {
    ProjectID     string
    TeamID        string
    AgentID       string
    PolicyVersion string
}

type SourceBinding struct {
    Ref               ArtifactRef
    ContentHash       string
    AggregateRevision int64
    ProjectID         string
    TeamID            string
}

type BenchmarkBinding struct {
    Ref      ArtifactRef // kind=benchmark_fixture
    Name     string
    Category string
    Cases    int
}
~~~

Revision mapping：

| Ref kind | Revision |
|---|---|
| context_item | ContextItem.ContentHash |
| experience_aggregate | ExperienceAggregate.Revision 的 decimal string |
| consolidation_proposal | proposal identity + source revision digest |
| promotion_proposal | PromotionProposal.DraftHash，並保存 source bindings |
| memory_policy_proposal | proposal revision hash |
| memory_policy_snapshot | MemoryPolicySnapshot.RevisionHash |
| team_snapshot | DefinitionRevision，並檢查 ContentRevision |
| benchmark_fixture | improve.BenchmarkRevision(fixture) |
| experiment_report | immutable report digest |
| improve_adoption | adoption artifact digest |
| monitoring_report | monitoring artifact digest |
| context_consolidation_approval | ID 是 consolidation proposal ID；Revision 是 confirmed candidate ContentHash |

### 3.2 Handoff record

~~~go
const HandoffSchemaVersion = 1

type ImprovementHandoff struct {
    Version        int
    ID             string
    Kind           HandoffKind
    Scope          HandoffScope
    Sources        []SourceBinding
    Proposal       ArtifactRef
    Candidate      *ArtifactRef
    Benchmark      *BenchmarkBinding
    Experiment     *ArtifactRef
    Adoption       *ArtifactRef
    Monitoring     []ArtifactRef
    Status         HandoffStatus
    Evaluation     EvaluationState
    StatusReason   string
    Revision       int64
    CreatedAt      time.Time
    UpdatedAt      time.Time
}

type HandoffKind string
const (
    HandoffMemoryPolicy  HandoffKind = "memory_policy"
    HandoffConsolidation HandoffKind = "context_consolidation"
    HandoffSkill         HandoffKind = "skill"
)

type EvaluationState struct {
    Decision string // eligible_for_review|retry|reject
    Status   string // passed|failed|inconclusive
    Report   *ArtifactRef
}
~~~

The actual Go declaration may split the types across files, but the JSON shape,
required fields and meanings are normative.

JSON field names are snake_case equivalents of the Go fields, for example:
version, id, kind, scope, sources, proposal, candidate, benchmark, experiment,
adoption, monitoring, status, evaluation, status_reason, revision, created_at
and updated_at. The store must reject unknown schema versions.

Normative invariants：

- Version is 1；ID 是 immutable identity 的 deterministic digest 且
  artifact-safe；新 record 必須從 proposed revision 1 開始。
- Kind 決定合法的 proposal、candidate 與 adoption ref kinds。
- ProjectID、TeamID 必填，且必須匹配所有 scoped artifact。
- Sources 依 (Ref.Kind, Ref.ID) 排序且不得重複。
- 不保存 source content；ContentHash 與 aggregate revision 只是 metadata。
- Proposal、Candidate、Benchmark、Experiment、Adoption、Monitoring 都是
  opaque refs，canonical artifact 不由 handoff 複製。
- 建立後 immutable bindings 不得修改；lifecycle update 只能修改 Status、
  StatusReason、Evaluation、Adoption、Monitoring、Revision、UpdatedAt。
- StatusReason 只能使用與目前 status 及 kind 相符的固定 reason code，不接受
  caller prose；既有 schema v1 固定字串仍可讀取。
- Monitoring refs 只能 append，不能取代或刪除；Experiment 與
  Evaluation.Report 必須是同一 ref。

## 4. Status machine

~~~text
proposed -> candidate_ready -> benchmark_bound -> evaluated
evaluated -> eligible_for_review
evaluated -> rejected                 # explicit operator rejection only
eligible_for_review -> approved
eligible_for_review -> rejected       # explicit operator rejection
approved -> adopted -> monitoring -> rollback_recommended
proposed|candidate_ready|benchmark_bound|evaluated|eligible_for_review|approved
  -> stale
~~~

規則：

1. candidate_ready 必須有 validated immutable candidate ref。
2. benchmark_bound 必須有 immutable benchmark ref 與 revision。
3. evaluated 必須有 immutable experiment/report ref；report 可為
   passed、failed 或 inconclusive。
4. 只有 Evaluation.Decision == eligible_for_review 可進入
   eligible_for_review。
5. eligible_for_review、approved、adopted 是三個不同狀態。
6. approved 只代表 handoff review acknowledgement，不會 apply、activate、
   merge、push 或修改 repository。
7. adopted 必須有 scope 與 candidate revision 相符的 canonical
   adoption/apply artifact。
8. temporary command failure 保持原狀態，可重試。
9. stale 是 terminal；不得自動 rebuild，須建立新的 handoff。
10. 每次 transition 都要完整驗證後 atomic persist。

現有狀態對應：

| Handoff | Canonical evidence |
|---|---|
| proposed | source proposal 可讀且 scope 正確 |
| candidate_ready | memory snapshot / context candidate / team snapshot |
| benchmark_bound | BenchmarkRef.Revision |
| evaluated | ExperimentReport.Status |
| eligible_for_review | ExperimentReport.Decision |
| approved | handoff-only explicit review |
| adopted | policy activation / context promotion apply / improve Adoption |
| monitoring | MonitoringReport |
| rollback_recommended | MonitoringReport 有 rollback suggestion |

## 5. Kind-specific contract

### 5.1 Memory policy

--from-memory-policy 的 ID 必須是 durable memory_policy_proposal，不是任意
MemoryPolicySnapshot ID。PR-1 新增：

~~~go
type MemoryPolicyOptimizationProposal struct {
    Version       int
    ID            string
    BasePolicy    ArtifactRef
    Candidate     ArtifactRef
    SourceMetrics Metrics
    Reason        string
    RevisionHash  string
    CreatedAt     time.Time
}
~~~

位置：

~~~text
workspace/improvement/memory-policies/proposals/<id>/proposal.json
~~~

candidate 仍是既有 immutable
workspace/improvement/memory-policies/<candidate-id>.json；proposal 不得
inline policy content。

進入 eligible_for_review 前必須通過：

- baseline/candidate snapshot revision validation；
- candidate 只改一個 policy category；
- baseline/candidate metrics 綁定同一 benchmark revision；
- harmful-use rate = 0；
- attribution coverage 不退步；
- completion、error、retry 不退步；
- token overhead 在既有 10% gate 內；
- stale retrieval rate 不上升。

candidate execution 必須使用 isolated context repository 或等價的
read-only policy override。prepare/evaluate 絕不得呼叫
ActivateMemoryPolicy。experiment evidence 必須保存 baseline 與 candidate
memory_policy_snapshot refs，且 arm 的 improve report 必須由 runtime event
證明只使用相符 policy version；只有 team snapshot ref 或 CLI 宣告不足以證明
policy experiment。

explicit adoption 才能走既有 policy activation path，並保留 previous
policy 供 rollback。handoff 只連結 activation/adoption evidence。

實作上的 adoption ref 為 `memory_policy_activation:<snapshot-id>:<revision>`；
handoff 只驗證既有 active snapshot 與 ref 相符，不在 adopt command 內執行 activation。
既有 `ApproveMemoryPolicyCandidate` canonical activation path 可接受兩種
review evidence：原本的 durable policy gates，或 exact candidate / benchmark /
experiment refs 均通過的 approved memory-policy handoff。兩者仍要求呼叫端明確
傳入 approval；handoff adopt 本身不會 activate。

### 5.2 Context consolidation

input 是既有 consolidation_proposal。它已包含 source IDs、content hashes
與 aggregate revisions；handoff 只複製這些 metadata bindings。

prepare 必須驗證：

- source 是 current confirmed persistent context；
- source scope 與 kind 全部相同；
- content hash 與 aggregate revision 未變；
- candidate ContextItem 仍是 candidate 且指向 proposal；
- candidate 未 confirmed、rejected 或 superseded。

候選評估必須涵蓋 relevance、contradiction/authority safety、secret
redaction、token overhead、stale/harmful retrieval exclusion 及
completion/error non-regression。

candidate arm 必須在 runtime `memory_usage_recorded` event 中證明 exact
`context_item:<id>:<ContentHash>` 曾被 applied；只匹配 ID、只在 compare 時讀取
目前 candidate hash，或只提供非空 snapshot ID 都不算有效證據。baseline arm
不得綁定 context candidate。

現有 hufu context consolidation approve 是 canonical confirmation。
handoff 的 prepare/evaluate 不得確認 candidate；adopt 只連結已確認的
candidate 與 approval evidence。

### 5.3 Skill

input 是 type=skill 的既有 promotion_proposal。canonical target 是
team directory 內的 skills/<name>/SKILL.md；.agents/skills/... 不是本流程
的 target。

prepare 必須接受：

~~~text
--baseline-team <baseline-snapshot-id>
~~~

並只在 improvement workspace：

1. load/validate promotion proposal 與 source bindings；
2. load immutable baseline team snapshot；
3. 將 draft 套到 baseline copy 的 skills/<name>/SKILL.md；
4. 產生 non-empty review patch 與 patch hash；
5. 建立 immutable candidate TeamSnapshot；
6. 保存 proposal、source、baseline、candidate refs。

正式 team、production skill 與 Git repository 不得被修改。candidate snapshot
是在 improvement workspace 內建立的 immutable artifact；正式 target 已有該
skill 時直接拒絕，符合既有 promotion 不覆寫 skill 的規則。

evaluation 後由既有 improve experiment pr 與人工作業負責 rollout；
handoff adopt 只連結既有 Adoption artifact，不 merge PR、不 apply patch。
benchmark 由 `hufu improve experiment compare --benchmark ...` 建立 report binding，
再由 handoff evaluate 驗證，不在 prepare 階段提前綁定。

## 6. Persistence 與 consistency

使用：

~~~text
workspace/improvement/handoffs/<handoff-id>/handoff.json
~~~

directory 建立一次，不得重用。store 必須：

- 驗證 artifact ID、拒絕 path traversal；
- first write 固定 immutable core bindings；
- lifecycle update 使用 temp-file + rename atomic write；
- 每次成功 update 遞增 Revision；
- expected revision 不匹配時拒絕更新；
- 同一 handoff ID 的 write 先取得 process-wide mutex，再取得 OS advisory
  file lock，CAS 的 read/validate/write 在同一 critical section；
- schema/ref validation 失敗時拒絕 tampered JSON；
- lifecycle audit key 使用 handoff:<id>:<operation>:<input-revision>；
- 永不保存 source content 或 model draft content。

handoff JSON 是 handoff lifecycle 的 canonical record。既有 EventStore
只保存 metadata audit：handoff_created、handoff_prepared、
handoff_evaluated、handoff_approved、handoff_rejected、handoff_adopted、
handoff_stale、handoff_monitoring。不建立第二 EventStore 或 execution state machine。

跨 store 操作順序：

1. validate 所有 canonical refs；
2. atomic persist handoff；
3. append metadata-only audit event；
4. event append 失敗時回傳 error，但保留 handoff；
5. audit retry 只接受 handoff ID，鎖定後重讀 durable record，並使用相同
   idempotency key；caller 不得提供 event state，也不得重複 event。

audit failure 不回滾已完成 canonical operation，也不重跑 worker 或 benchmark。

目前 store 使用 `workspace/improvement/handoffs/<id>/handoff.json`，以
atomic create/update、跨 store/process lock 與 expected-revision CAS 保護 lifecycle
寫入；audit failure 會保留已寫入的 handoff。candidate_ready、benchmark_bound、
evaluated、eligible_for_review、approved、adopted、monitoring 與
rollback_recommended 都可透過 durable state 重送相同 idempotency key 的 audit，
不重做 state transition。

## 7. CLI contract

所有 handoff commands 接受：

~~~text
--workspace <path>          default: <cwd>/workspace
--project <id>              required
--team <name>               required
--agent-team-search-path <csv>  improve 的既有 team discovery flag
--policy-version <id>       memory/consolidation source checks 使用
--json                      stable metadata-only output
~~~

Commands：

~~~bash
hufu improve handoff create --from-memory-policy <proposal-id> \
  --workspace workspace --project p --team dev
hufu improve handoff create --from-consolidation <proposal-id> \
  --workspace workspace --project p --team dev
hufu improve handoff create --from-promotion <proposal-id> \
  --workspace workspace --project p --team dev

hufu improve handoff show <handoff-id> --workspace workspace \
  --project p --team dev

hufu improve handoff prepare <handoff-id> --baseline-team <baseline-id> \
  --workspace workspace --project p --team dev

# Memory policy preparation uses a policy snapshot as its baseline.
hufu improve handoff prepare <handoff-id> --baseline-policy <policy-id> \
  --workspace workspace --project p --team dev

# Consolidation preparation validates the existing candidate binding.
hufu improve handoff prepare <handoff-id> --workspace workspace \
  --project p --team dev

# compare 綁定 benchmark；consolidation 另以 --candidate-context 將 runtime
# applied context ref 寫入 experiment report。
hufu improve experiment compare <experiment-id> \
  --baseline <baseline-team-snapshot> --candidate <candidate-team-snapshot> \
  --benchmark <fixture> --baseline-report <report.json> \
  --candidate-report <report.json> --candidate-context <context-item-id> \
  --baseline-accepted --candidate-accepted --workspace workspace

hufu improve handoff evaluate <handoff-id> --experiment <experiment-id> \
  --workspace workspace --project p --team dev

hufu improve handoff approve <handoff-id> --expected-revision <n> \
  --workspace workspace --project p --team dev

hufu improve handoff reject <handoff-id> --expected-revision <n> \
  --workspace workspace --project p --team dev

hufu improve handoff adopt <handoff-id> --adoption-ref <kind:id:revision> \
  --expected-revision <n> --workspace workspace --project p --team dev

hufu improve handoff monitor <handoff-id> --monitoring-ref <kind:id:revision> \
  --expected-revision <n> --workspace workspace --project p --team dev
~~~

Rules：

- create 必須且只能有一個 --from-*，同一 proposal/source revisions
  重試時 idempotent。
- prepare 對 skill 必須有 --baseline-team；對 memory_policy 必須有
  --baseline-policy；consolidation 使用既有 candidate binding。
- evaluate 只接受 benchmark revision、candidate refs、team、project、
  policy refs 全部匹配的 existing ExperimentReport。
- approve/adopt/monitor 必須有 expected revision；stale write 不改 record。
- reject 必須有 expected revision，只允許從 evaluated 或
  eligible_for_review 明確進入 terminal rejected；不修改 canonical candidate。
- adopt/monitor 只驗證 supplied ref，絕不執行該 ref 的 operation。
- JSON 只輸出 metadata/refs，不輸出 draft 或 benchmark prompts。

目前三種 kind 都有 kind-specific prepare/evaluate binding：skill 綁定
candidate team snapshot，memory policy 綁定 baseline/candidate policy snapshots，
consolidation 綁定 candidate context snapshot。沒有匹配 refs 的 generic team
experiment 會被拒絕。

## 8. File plan

新增：

~~~text
internal/improve/handoff.go
internal/improve/handoff_store.go
internal/improve/handoff_lock_{unix,windows,other}.go
internal/improve/handoff_test.go
cmd/hufu/improve_handoff.go
cmd/hufu/improve_handoff_test.go
~~~

修改：

~~~text
internal/improve/memory_policy.go
internal/improve/experiment.go
internal/improve/automation.go
internal/improve/sqlite_analytics_{schema,memory,memory_loader}.go
internal/team/memory_learning.go
cmd/hufu/improve_experiment.go
~~~

不得新增第二 context database、EventStore、graph database 或 remote memory
service。

## 9. PR sequence 與 exit criteria

### PR-1 — Schema、store、proposal durability（已完成：`3fc1b95`）

- typed refs、schema validation、CAS transition；
- atomic handoff store 與 metadata-only audit；
- durable MemoryPolicyOptimizationProposal；
- 三種 kind 的 create/show；
- 不 materialize candidate、不改 production。

Exit：create/show、scope、tamper、stale、idempotency 測試通過，
golangci-lint run 通過。

### PR-2 — Skill candidate preparation（已完成：`3f71a55`）

- candidate team 只建立在 workspace/improvement；
- target 固定為 skills/<name>/SKILL.md；
- 建立 immutable candidate snapshot、patch、source bindings；
- 實作 skill prepare。

Exit：正式 team/skill hash 不變、candidate immutable、promotion validation
通過，且不呼叫 GitHub、merge 或 apply。

### PR-3 — Memory policy preparation 與 report binding（已完成：`c3a0410`）

- isolated candidate policy execution input；
- experiment evidence 加入 baseline/candidate policy refs；
- 實作 memory_policy prepare 與 validation；
- evaluate 絕不呼叫 ActivateMemoryPolicy。

Exit：candidate benchmark 不改 active policy，report 不可綁錯 policy
revision，rollback reference 保留，gates deterministic。

### PR-4 — Consolidation checks 與 lifecycle facade（已完成：`3e5054b`）

- consolidation source/candidate validation；
- fixed benchmark 與 evaluation evidence binding；
- 實作 evaluate、approve、adopt、monitor 的 ref-validating lifecycle；
- 保留既有 consolidation approve 與 promotion apply 作為 canonical mutation。

Exit：eligible_for_review 不會 confirm/apply；adoption 必須有 matching
canonical evidence；monitoring 只產生 rollback recommendation。Monitoring
report 以 `monitor-<timestamp>` ID durable 保存於
`workspace/improvement/monitoring/<adoption-id>/`，並透過
`monitoring_report:<id>:<revision>` ref 綁定。

### Corrective hardening（已完成：`d4e23f6`、`c7fc533`、`3f94103`）

- 修正三種 kind 的 prepare dispatch 與所有 command scope enforcement；
- candidate / benchmark / experiment / evaluation / adoption bindings immutable；
- canonical source、snapshot、benchmark、report、adoption 與 monitoring evidence
  每次 mutation command 前重驗，變更即轉 stale；
- 使用跨程序 lock 保護 CAS，並支援 audit-safe command retry；
- experiment / monitoring outcome 由 evidence deterministic 重算；
- rollback suggestion 可實際進入 rollback_recommended；
- runtime report 綁定 exact policy version 與 applied context ContentHash，而非
  接受使用者自行宣告；
- handoff ID、initial state、status reason code、create-time evidence 與 audit retry
  都由 durable identity 驗證，不接受 caller 組裝 lifecycle metadata；
- monitoring baseline metrics 與 rollback target 綁回同一 experiment/adoption，
  healthy report 也不能繞過 baseline 驗證；
- 補上帶 CAS 的 explicit reject facade，拒絕不會 confirm/apply candidate；
- 補齊 prepare、完整 consolidation lifecycle、canonical memory activation、
  tamper、scope、audit retry、monitoring baseline 與 cross-process concurrency tests。

## 10. Required tests

~~~text
TestHandoffSchemaRejectsUntypedOrMissingRefs
TestHandoffCreateIsIdempotent
TestHandoffCannotCrossProjectOrTeamScope
TestHandoffRejectsTamperedArtifactRevision
TestHandoffCASRejectsStaleExpectedRevision
TestHandoffTransitionMatrix
TestHandoffCannotAdoptWithoutEvaluation
TestHandoffEligibleForReviewIsNotApprovalOrAdoption
TestHandoffAuditEventIsIdempotent
TestHandoffRetainsRecordWhenAuditAppendFails

TestMemoryOptimizerProposalIsDurableAndRevisionBound
TestMemoryPolicyHandoffBindsBenchmarkRevision
TestMemoryPolicyCandidateDoesNotActivateProduction
TestMemoryPolicyHandoffRejectsMultipleChangedCategories
TestMemoryPolicyHandoffRetainsRollbackPolicy

TestSkillHandoffUsesCanonicalSkillsTarget
TestSkillHandoffCandidateSnapshotIsImmutable
TestSkillHandoffRejectsExistingSkillTarget

TestConsolidationHandoffPreservesSourceHashesAndAggregateRevisions
TestConsolidationHandoffRejectsChangedSource
TestConsolidationCandidateIsNotConfirmedByPrepare
TestConsolidationHandoffRejectsContradictoryOrWidenedScope

TestHandoffRejectsWrongBenchmarkRevision
TestHandoffRejectsWrongExperimentCandidate
TestHandoffRejectsMismatchedAdoptionRef
TestHandoffMonitoringNeverExecutesRollback
TestHandoffMonitoringRejectsUnboundHealthyBaseline
TestHandoffRejectRequiresExplicitReviewedState
TestSideEffectTaskNeverEnablesMemoryExploration
TestHandoffArtifactsAndEventsContainNoContentOrSecrets
~~~

Tests 使用 local temp workspace 與 deterministic fixtures；live Ollama、external
API、GitHub、real PR 或 infrastructure mutation 不得是必要條件。

上列 contract 由 package-level schema/store tests 與 CLI lifecycle tests 共同
覆蓋；cross-process CAS 另以 subprocess 測試，並以 race detector 驗證
in-process concurrent transitions。

每個 code PR 必須執行：

~~~bash
go test ./...
go vet ./...
golangci-lint run
~~~

## 11. Failure 與 stale semantics

以下任一變更使 handoff stale：

- source context content hash、lifecycle、scope 或 aggregate revision；
- promotion draft hash 或 target base hash；
- memory policy proposal/candidate revision；
- team baseline definition/content revision；
- benchmark fixture revision；
- experiment report 或 candidate policy/team revision；
- adoption 或 monitoring artifact digest。

以下可重試且不改 status：

- temporary filesystem error；
- temporary SQLite busy/lock error；
- audit event append failure；
- operation 尚未開始時缺少後續 artifact。

以下對目前 handoff 是 terminal，須建立新的 handoff：

- source/candidate hash mismatch；
- cross-project/team/agent scope mismatch；
- invalid transition；
- benchmark revision mismatch；
- candidate artifact missing/malformed；
- adoption evidence 指向不同 candidate。

stale path 不得自動 rebuild、重跑 worker、apply promotion、activate policy
或 reset repository。

## 12. Definition of done

完成 handoff implementation 必須同時滿足：

1. 每個 handoff 都有 typed、revision-bound、scope-checked opaque refs。
2. Skill candidate 在任何 production apply 前已 materialize 並 benchmark。
3. Memory policy candidate evaluation 不改 active policy。
4. Consolidation candidate 保留 source provenance，且在既有 explicit
   approval 前維持未確認。
5. eligible_for_review、approved、adopted 是不同 durable states。
6. retry、process restart、audit delivery 不重複 state 或 event。
7. 每個 production adoption 都保留 previous revision 或 rollback ref。
8. handoff command 不會 autonomous merge、apply、activate 或 rollback。
9. 完整測試矩陣與 go test、go vet、golangci-lint 全部通過。

這代表可 review、可追溯的 L4 handoff，不代表 autonomous deployment 或
autonomous policy evolution。
