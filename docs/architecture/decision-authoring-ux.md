# Hufu Decision Authoring UX 簡化實作規格

> Status: Implemented
> Authority: normative
> Verified-Commit: 1d370fe
> Verified-Date: 2026-09-17
> Supersedes: —
> Superseded-By: —
> Audience: Hufu maintainer / coding agent
> Source baseline: 33a634b2d4f0743323e600d05a05a4733f162171
> Scope: team manifest authoring、normalization、request contract ownership、inspection、migration、bundled team、tests、documentation

## 1. 實作決策

本規格只簡化 decision-aware runtime 的設定介面，不新增另一套 decision execution path。

完成後，普通 team 可以寫：

~~~yaml
decision:
  profile: standard
~~~

需要固定 request contract 時可以寫：

~~~yaml
request:
  objective: Keep the service reachable during migration.
  success-criteria:
    - id: reachable
      statement: Service remains reachable after migration.

decision:
  profile: standard
~~~

需要自訂 policy 時仍使用明確的 local profile：

~~~yaml
decision:
  profile: custom
  profiles:
    custom:
      policy:
        independent-judgments: 4
        context-isolation: sealed
        aggregation:
          method: median-score
        challenge:
          enabled: true
          count: 2
        revision:
          enabled: true
~~~

三種形式最後都必須進入現有的：

~~~text
authoring normalization
  -> materialized profile
  -> durable DecisionAdmission
  -> DecisionEngine
  -> DecisionRecord / event journal / decision index
~~~

不得新增平行 scheduler、journal、artifact store 或簡化版 DecisionEngine。

## 2. 明確排除的工作

以下項目不在本規格中，不得由實作者順便加入：

- hufu decide 新命令；
- single-decision run scope；
- --model 到 judge、sidecar 或 coordinator 的新 fallback；
- agent backend 與 direct LLM backend 之間的新轉接層；
- 新的 public DecisionRecord result envelope；
- coordinator 可寫入 decision profile、options、facts、base rates 或 provenance；
- preset overlay 或 profile inheritance；
- 新 aggregation algorithm；
- capability taxonomy redesign；
- pricing 或精準成本估算；
- outcome-learning calibration；
- 新 persistence backend；
- migrator 寫回檔案。

這些工作會改變 execution/backend/public API 契約，不能作為 authoring UX 簡化的附帶修改。

## 3. 不可破壞的 runtime invariants

實作必須保留：

1. profile precedence：

   ~~~text
   CLI/request override > task profile > team default > off
   ~~~

2. unknown profile fail closed；
3. preset 不得授權 agent 或 tool；
4. coordinator payload 不得取得 decision-only fields；
5. enabled decision 必須有 durable event journal；
6. enabled decision 必須有完整 request contract；
7. admission 必須 snapshot normalized policy、profile identity、policy digest 與 request contract identity；
8. resume 必須使用原 admission，不重新讀取 mutable authoring config；
9. shorthand 不得新增 provider call；
10. general hufu run 的 model resolution 與 per-task decision semantics 不變；
11. secret redaction、workspace scope、atomic persistence 與 branch projection 不變。

## 4. Canonical authoring schema

### 4.1 Decision profile

Canonical：

~~~yaml
decision:
  profile: standard
~~~

decision.profile 接受：

~~~text
off
light
standard
high-stakes
<team-local-profile>
builtin/<name>@<version>
~~~

不支援 scalar shorthand：

~~~yaml
# invalid
decision: standard
~~~

### 4.2 Routing hints

Canonical：

~~~yaml
decision:
  profile: standard
  routing:
    hints:
      - when-goal-contains: storage
        preferred-capabilities:
          - architecture
~~~

### 4.3 Request contract

Canonical：

~~~yaml
request:
  objective: Keep the bridge reachable while changing it.
  success-criteria:
    - id: reachable
      statement: The bridge answers after the change.
  constraints:
    - id: no-downtime
      statement: No externally visible downtime.
  assumptions:
    - id: service-accepts
      statement: Target service accepts the change.
      critical: true
~~~

Top-level request 不含 enabled。規則固定如下：

- key 不存在：request contract disabled；
- key 存在且為 mapping：request contract enabled；
- request: null：load-time error；
- request: {}：因缺 objective 與 success criteria 而 load-time error；
- objective 與 success criteria 使用現有 validation rules；
- request contract 內容不得由 LLM 補寫或推論。

## 5. Legacy compatibility

以下 legacy YAML 在本次變更後仍必須可載入：

~~~yaml
decision:
  default-profile: standard
  request-contract:
    enabled: true
    objective: Make a safe decision.
    success-criteria:
      - id: safe
        statement: Material risks are represented.
  routing-hints:
    - when-goal-contains: storage
      preferred-capabilities:
        - architecture
  profiles:
    standard:
      preset: builtin/standard@v1
~~~

Compatibility 只存在於 authoring decoder/normalizer。Runtime 不得同時維護兩份 authority。

### 5.1 Conflict rules

下列組合一律 load-time error，即使值相同：

~~~text
decision.profile + decision.default-profile
decision.routing.hints + decision.routing-hints
top-level request + decision.request-contract
~~~

Stable diagnostics：

~~~text
decision_authoring_conflict: decision.profile conflicts with deprecated decision.default-profile
decision_authoring_conflict: decision.routing.hints conflicts with deprecated decision.routing-hints
request_authoring_conflict: request conflicts with deprecated decision.request-contract
~~~

Legacy decision.request-contract 的行為保持不變：

- enabled: true 時必須通過完整 validation；
- absent、null、{} 或 zero value 仍表示 disabled；
- explicit enabled: false 不得被轉成 enabled；
- disabled 但含其他欄位仍維持 disabled，不得猜測作者意圖。

## 6. Authoring/runtime boundary

### 6.1 Canonical owner

新增 internal/team/decision_authoring.go。

Authoring types 放在 internal/team，因為 YAML manifest parsing 屬於 team package；runtime policy types 繼續放在 internal/agent。

規定型別：

~~~go
type DecisionRoutingAuthoringConfig struct {
    Hints []agent.RoutingHint `yaml:"hints,omitempty"`

    hintsSet bool
}

type DecisionAuthoringConfig struct {
    Profile  string                                `yaml:"profile,omitempty"`
    Profiles map[string]agent.DecisionProfileSpec `yaml:"profiles,omitempty"`
    Routing  DecisionRoutingAuthoringConfig        `yaml:"routing,omitempty"`

    // Legacy authoring only.
    DefaultProfile  string                       `yaml:"default-profile,omitempty"`
    RequestContract *agent.RequestContractConfig `yaml:"request-contract,omitempty"`
    RoutingHints    []agent.RoutingHint          `yaml:"routing-hints,omitempty"`

    profileSet        bool
    defaultProfileSet bool
    routingHintsSet   bool
    legacyContractSet bool
}

type RequestAuthoringConfig struct {
    Objective       string                            `yaml:"objective"`
    SuccessCriteria []agent.RequestSuccessCriterion   `yaml:"success-criteria"`
    Constraints     []agent.RequestConstraint         `yaml:"constraints,omitempty"`
    Assumptions     []agent.RequestContractAssumption `yaml:"assumptions,omitempty"`
}
~~~

DecisionAuthoringConfig.UnmarshalYAML 必須：

- 接受 mapping only；
- 使用明確 allowlist 拒絕未知 key；
- 記錄 canonical/legacy key 是否實際出現；
- 使用 agent.DecisionProfileSpec.UnmarshalYAML，不另寫寬鬆 decoder；
- 不 materialize policy、不呼叫 provider。

DecisionRoutingAuthoringConfig.UnmarshalYAML 必須接受 mapping only、拒絕未知或
重複 key，並只在 hints key 實際出現時設定 hintsSet。decision.routing: {}
可接受且不算 canonical hints；decision.routing: null 必須拒絕。因此只有
decision.routing.hints 與 decision.routing-hints 都實際出現時才觸發 conflict。

RequestAuthoringConfig.UnmarshalYAML 必須接受 non-null mapping only、使用
KnownFields(true) 等價的 allowlist 拒絕未知或重複 key。是否出現仍由 manifest
decoder 的 requestSet 記錄；不得用 decoded zero value 推測 presence。

目前 private decodeDecisionProfileSpecYAML 的邏輯必須搬入
DecisionProfileSpec.UnmarshalYAML；既有 DecisionConfig compatibility decoder
也改呼叫該 method。這只是 decoder ownership 移動，不得改變 accepted/rejected
YAML shapes。

Manifest decoder 必須在 legacy-flat top-level 或 v1alpha1 spec mapping 上掃描
top-level request node，另外記錄 requestSet，才能區分 absent 與 null。相同 mapping
中的 duplicate request key 必須拒絕；request: null 必須在 manifest node scan 時、
normalization 前拒絕。

### 6.2 Runtime types

agent.DecisionConfig 最終只保留 decision behavior：

~~~go
type DecisionConfig struct {
    DefaultProfile string
    Profiles       map[string]DecisionPolicy
    ProfileSpecs   map[string]DecisionProfileSpec
    RoutingHints   []RoutingHint
}
~~~

agent.DecisionConfig.UnmarshalYAML / MarshalYAML 同步縮成 decision-only schema：
只處理 default-profile、profiles、routing-hints。Legacy request-contract 的接受與
轉換只屬於 internal/team 的 DecisionAuthoringConfig；manifest compatibility tests
必須經 decodeTeamManifestYAML / parseTeamYML，而不是直接把整段 manifest decode
到 agent.DecisionConfig。

新增到 agent.TeamConfig：

~~~go
RequestContract RequestContractConfig
~~~

RequestContract 的唯一 normalized runtime owner 是 TeamConfig.RequestContract。不得讓 runtime 在 TeamConfig.RequestContract 與 DecisionConfig.RequestContract 間 fallback。

所有目前讀取 c.decisionConfig().RequestContract 的 production paths 必須改為讀取同一個 coordinator helper，例如：

~~~go
func (c *Coordinator) requestContractConfig() agent.RequestContractConfig
~~~

必須追蹤並更新：

- new admission；
- existing admission resume；
- direct-agent execution；
- normal worker execution；
- runtime action/static task execution；
- extra-model leaf execution；
- request contract revision；
- dry-run、validate、explain；
- tests and fixtures。

### 6.3 Normalization result

新增 deterministic API：

~~~go
type DecisionAuthoringMetadata struct {
    ProfileSource       string
    RoutingSource       string
    RequestSource       string
    RequestedProfile    string
    ResolvedProfileRef  string
    UsedLegacyDefault   bool
    UsedLegacyHints     bool
    UsedLegacyContract  bool
    Deprecations        []string
}

func NormalizeDecisionAuthoring(
    decision DecisionAuthoringConfig,
    request RequestAuthoringConfig,
    requestSet bool,
    catalog agent.DecisionProfileCatalog,
) (agent.DecisionConfig, agent.RequestContractConfig, DecisionAuthoringMetadata, error)
~~~

Metadata 是 read-only provenance，不是 runtime authority。唯一存放位置為：

~~~go
type TeamSession struct {
    // existing fields...
    DecisionAuthoring DecisionAuthoringMetadata
}
~~~

Metadata 字串值固定如下；未出現時為空字串，Deprecations 依
default-profile、request-contract、routing-hints 的固定順序輸出：

| Field | Allowed values |
|---|---|
| ProfileSource | `decision.profile`, `decision.default-profile`, `""` |
| RoutingSource | `decision.routing.hints`, `decision.routing-hints`, `""` |
| RequestSource | `request`, `decision.request-contract`, `""` |
| ResolvedProfileRef | resolved policy 來自 built-in 時為 exact `builtin/<name>@<version>`；local inline/off/absent 為 `""` |

新增固定 signature：

~~~go
func parseTeamYMLWithAuthoring(
    teamDir string,
    vars map[string]string,
) (agent.TeamConfig, DecisionAuthoringMetadata, error)
~~~

它回傳 normalized TeamConfig 與 metadata；既有 parseTeamYML 保留為
compatibility wrapper並丟棄 metadata。
loadTeamWithMode 必須呼叫新函式並填入 TeamSession.DecisionAuthoring。
CompileTeam/team explain 從 RuntimeSession 讀取該 metadata；migrator 直接使用同一
normalizer 的回傳值。DecisionEngine 不得依賴 metadata。

## 7. Exact normalization algorithm

### 7.1 Profile source

1. canonical decision.profile 與 legacy decision.default-profile 同時出現：error；
2. canonical 出現：requested profile = canonical value；
3. 否則 legacy 出現：requested profile = legacy value；
4. 兩者皆未出現：default profile 留空，runtime 照既有規則 resolve 到 off；
5. 空白 authored value：error，不得當作 absent。

### 7.2 Ergonomic aliases

Alias resolution 只發生在 authoring normalization，不改 CLI/task override 的既有語意。

Resolution order：

~~~text
1. off
2. exact builtin/<name>@<version>
3. team-local profile with the requested name
4. ergonomic alias:
   light       -> builtin/light@v1
   standard    -> builtin/standard@v1
   high-stakes -> builtin/high-stakes@v1
5. unknown -> fail closed
~~~

為保持 requested name 與 legacy effective config 相同，ergonomic alias 必須 normalize 成 synthetic profile spec：

~~~go
ProfileSpecs["standard"] = DecisionProfileSpec{
    Preset: &DecisionProfileRef{Name: "builtin/standard@v1"},
}
DefaultProfile = "standard"
~~~

只有在沒有同名 local profile 時才可建立 synthetic spec。不得覆寫 local standard。

off 不建立 profile spec。Exact builtin ref 直接作為 DefaultProfile。

### 7.3 Profiles

- profile name 不得為空、off 或 builtin/...；
- DecisionProfileSpec 維持 exactly one of preset or policy；
- preset 必須是 exact versioned builtin ref；
- preset 不支援 overlay；
- inline policy 使用既有 validation、normalization 與 digest code；
- synthetic aliases 與 explicit legacy aliases 必須 materialize 成相同 policy、
  agent.DecisionProfileMetadata、digest 與 execution plan；DecisionAuthoringMetadata
  的 source/deprecation fields 按實際 authoring form 保留差異。

### 7.4 Routing

1. canonical hints key 與 legacy hints key 同時出現：error；空的 routing mapping 不算 conflict；
2. canonical hints normalize 到 DecisionConfig.RoutingHints；
3. legacy hints normalize 到同一 field；
4. preserve authored order；
5. 每個 hint 呼叫既有 RoutingHint.Validate()；
6. routing hints 不得增加 required capabilities 或 authority。

### 7.5 Request

1. top-level request 與 legacy nested contract 同時出現：error；
2. top-level request 轉為 Enabled: true 的 agent.RequestContractConfig；
3. legacy contract deep-copy 到 normalized TeamConfig.RequestContract；
4. absent forms 產生 zero/disabled config；
5. 呼叫現有 RequestContractConfig.Validate()；
6. 不改 BuildRequestContract 的 redaction、material hash、revision 或 persistence semantics。

## 8. Parser and manifest integration

teamManifestSpecFields 必須改為 authoring types：

~~~go
Decision DecisionAuthoringConfig `yaml:"decision,omitempty"`
Request  RequestAuthoringConfig  `yaml:"request,omitempty"`
~~~

實際實作可使用 pointer 或額外 presence bit，但必須符合第 4.3 節的 absent/null/mapping semantics。

parseTeamYML 不得再用 DefaultProfile/Profiles 是否為空判斷是否複製 decision config。它必須無條件呼叫 authoring normalizer，再把 normalized decision/request 寫入 agent.TeamConfig。

Legacy flat manifest 與 hufu.io/v1alpha1 envelope 必須共用同一組 authoring field declarations與同一 normalizer。Strict KnownFields(true) 行為不得弱化。

## 9. Validation and inspection

### 9.1 hufu team validate

沿用現有 CompileTeam pipeline，不建立第二套 validator。新增：

- canonical/legacy conflicts；
- profile resolution；
- request completeness；
- routing hint validation；
- enabled decision 是否有 request contract；
- capability-routed profile 是否有候選；
- legacy fields 以 warning 顯示，不讓 normal run 重複輸出。

team validate 繼續只做現有 compile/contract validation，不新增 environment-dependent
execution-target 檢查；該責任仍由 hufu doctor、hufu team check 與 runtime preflight
持有。這一階段只把 decision/request checks 接到既有 CompileTeam /
ValidateEffectiveTeam pipeline。

Deprecation warning：

~~~text
WARN decision.default-profile is deprecated; use decision.profile
WARN decision.request-contract is deprecated; use request
WARN decision.routing-hints is deprecated; use decision.routing.hints
~~~

### 9.2 hufu team explain

在既有 output DTO 加入 decision projection：

~~~text
Decision authoring:
  profile source: decision.profile
  requested: standard
  resolved: builtin/standard@v1
  origin: builtin
  version: v1
  policy digest: sha256:...

Request contract:
  source: request
  enabled: true

Routing:
  source: decision.routing.hints
  hints: 1

Decision plan:
  proposal: true
  reference: true
  judges: 3
  aggregation: mean-score
  challenge: 1
  revision: true
  premortem: true
  forecast: true
  finalization: aggregate
~~~

Text、JSON、YAML formats 必須使用同一 projection builder。Plan 必須呼叫現有 CompileDecisionExecutionPlan，不可複製 stage inference logic。

既有 team explain top-level JSON/YAML fields 不改名。新增 decision field，
其值使用下述 decisionProfileView；另加 request 與 routing 子物件，分別只有
source/enabled 與 source/hint_count。沒有 decision/request/routing authoring 時仍輸出
decision disabled projection，不得從 output absence 猜測狀態。

### 9.3 Decision profile inspection

在現有 hufu decision command 下新增：

~~~bash
hufu decision profile list
hufu decision profile show standard
hufu decision profile show standard --team my-team
hufu decision plan --profile standard
hufu decision plan --profile standard --team my-team
~~~

規則：

- 無 --team 時只列 exact built-ins 與 ergonomic aliases；
- 有 --team 時額外載入 local profiles；
- --team 接受 discoverable team name，沿用現有 TeamRegistry search paths；
- unknown profile fail closed；
- commands 為 read-only；
- 不建立 workspace；
- 不呼叫 provider；
- plan 使用 CompileDecisionExecutionPlan；
- text 為預設輸出；JSON 沿用 decision command 現有 persistent --json flag；
- JSON output 使用 explicit schema_version: 1 DTO，不直接 marshal internal policy struct；
- 第一版不輸出美元成本，只輸出 stage fan-out 與 configured max-token bounds。

固定 JSON DTO：

~~~go
type decisionProfileIdentityView struct {
    RequestedName string `json:"requested_name" yaml:"requested_name"`
    ResolvedRef   string `json:"resolved_ref,omitempty" yaml:"resolved_ref,omitempty"`
    Origin        string `json:"origin,omitempty" yaml:"origin,omitempty"`
    Version       string `json:"version,omitempty" yaml:"version,omitempty"`
    PolicyDigest  string `json:"policy_digest,omitempty" yaml:"policy_digest,omitempty"`
}

type decisionPlanView struct {
    Proposal        bool   `json:"proposal" yaml:"proposal"`
    Reference       bool   `json:"reference" yaml:"reference"`
    JudgeCount      int    `json:"judge_count" yaml:"judge_count"`
    Aggregation     string `json:"aggregation,omitempty" yaml:"aggregation,omitempty"`
    ChallengeCount  int    `json:"challenge_count" yaml:"challenge_count"`
    Revision        bool   `json:"revision" yaml:"revision"`
    Premortem       bool   `json:"premortem" yaml:"premortem"`
    Forecast        bool   `json:"forecast" yaml:"forecast"`
    Finalization    string `json:"finalization,omitempty" yaml:"finalization,omitempty"`
    PolicyMaxTokens int64  `json:"policy_max_tokens,omitempty" yaml:"policy_max_tokens,omitempty"`
    StopMaxTokens   int64  `json:"stop_max_tokens,omitempty" yaml:"stop_max_tokens,omitempty"`
}

type decisionProfileView struct {
    SchemaVersion int                         `json:"schema_version" yaml:"schema_version"`
    Kind          string                      `json:"kind" yaml:"kind"`
    Enabled       bool                        `json:"enabled" yaml:"enabled"`
    Identity      decisionProfileIdentityView `json:"identity" yaml:"identity"`
    Plan          *decisionPlanView            `json:"plan,omitempty" yaml:"plan,omitempty"`
}

type decisionProfileListItemView struct {
    Name       string `json:"name" yaml:"name"`
    Kind       string `json:"kind" yaml:"kind"` // off, builtin, alias, local
    ResolvesTo string `json:"resolves_to,omitempty" yaml:"resolves_to,omitempty"`
}

type decisionProfileListView struct {
    SchemaVersion int                           `json:"schema_version" yaml:"schema_version"`
    Kind          string                        `json:"kind" yaml:"kind"` // decision_profile_list
    Profiles      []decisionProfileListItemView `json:"profiles" yaml:"profiles"`
}
~~~

profile show 的 Kind 固定為 decision_profile；decision plan 的 Kind 固定為
decision_plan，但共用其餘 decisionProfileView fields。off 必須回傳 Enabled:false、
空 digest、無 plan。profile list 必須包含 off、exact built-ins 與 aliases；有 team 時
再加入 local profiles。同名 local profile 取代 alias row，不能輸出兩個 standard。
所有 list 依 name bytewise ascending 排序。

## 10. Bundled strategic-decision cleanup

從 .agent-teams/strategic-decision/team.yaml 移除 invalid model/judge-model placeholders，改用 canonical request、decision.profile 與 decision.routing.hints。

保留現有：

- capability registry；
- routing policy；
- tools/delegation authority；
- workflow ownership；
- agent definitions。

不得修改 general model fallback。Bundled team 沒有有效 model config 時仍應在 provider call 前以既有 actionable diagnostic 失敗。

文件中的 runnable example 必須明確提供或先設定：

- worker execution target；
- coordinator direct-LLM target；
- judge direct-LLM target，或可解析的 sidecar/judge global config。

不得宣稱 --model codex/... 能同時滿足 coordinator 或 judge。

## 11. Migrator and scaffolder regression

### 11.1 team migrate

擴充現有 dry-run-only command：

~~~bash
hufu team migrate --dry-run --canonical-authoring <team-directory>
~~~

沒有 --canonical-authoring 時，現有 schema-version migration 行為完全不變。

有 flag 時：

1. 使用同一 authoring decoder/normalizer；
2. default-profile 轉為 profile；
3. routing-hints 轉為 routing.hints；
4. enabled legacy request contract 轉為 top-level request 並移除 enabled；
5. exact builtin aliases可 elide；
6. reload migrated YAML；
7. 比較 normalized decision/request、materialized policy、policy digest 與 execution plan；
8. 任一不相等則 fail，不輸出 best-effort migration。

Alias elision 只允許 light、standard、high-stakes 對應其 exact builtin v1 preset，不使用近似 policy comparison。

特殊情況：

- disabled legacy request contract 且無其他內容：可移除；
- disabled legacy request contract 但含其他內容：保留 legacy form並輸出 warning；
- authoring decision/request 欄位含未解析 template marker：canonical-authoring migration fail closed；
- command 永遠不寫入來源檔。

### 11.2 Scaffolder non-regression

hufu init、hufu team create、hufu team generate 目前不會建立 decision config，
因此不修改其輸出。只新增 regression tests，確認它們不會因本功能而預設輸出
decision 或 request。Canonical decision examples 由第 10 節 bundled team 與
第 12 節 Phase 5 的文件清單持有。

## 12. Implementation phases

每個 phase 完成後都必須保持 repository tests passing。

### Phase 0 — Regression lock

只加 tests/fixtures：

- built-in profile digests；
- strategic-decision current decision policy、routing、authority 與 execution plan
  （排除將在 Phase 5 移除的 model placeholder 與 authoring source metadata）；
- legacy default profile/preset aliases/request contract/routing hints；
- admission/resume identity；
- profile precedence。

Acceptance：無 production behavior change。

### Phase 1 — Authoring types、aliases、routing

實作 strict decoder、conflicts、decision.profile、ergonomic aliases、decision.routing.hints、metadata 與無條件 normalization。

Phase 1 同時包含 alias support；不得把 profile: standard acceptance 放在 alias 尚未實作的中間狀態。

Acceptance：canonical standard 與 legacy explicit standard preset alias 產生相同
materialized policy、agent.DecisionProfileMetadata、digest 與 plan；兩者的
DecisionAuthoringMetadata 正確記錄不同 source/deprecation provenance。

### Phase 2 — Top-level request ownership

實作 top-level request presence、TeamConfig.RequestContract、coordinator helper、所有 execution/admission/resume paths 與 legacy nested normalization。

Acceptance：給定相同 raw request、revision 與 clock，old/new authoring 產生 deep-equal normalized config、相同 RequestContract material hash 與相同 admission contract identity。

### Phase 3 — Validate、explain、profile inspection

實作第 9 節。所有 commands read-only、provider-free。

Acceptance：text/JSON/YAML projections 來自同一 builder；JSON golden 固定 schema version；plan 與 CompileDecisionExecutionPlan deep equal。

### Phase 4 — Migrator and scaffolder regression

實作第 11.1 節，並加入第 11.2 節的 non-regression tests。

Acceptance：legacy fixture canonical migration/reload 後 semantic equality；第二次 migration byte-stable；來源檔未被修改。

### Phase 5 — Bundled team and docs

實作第 10 節與文件更新：

~~~text
.agent-teams/strategic-decision/README.md
.agent-teams/strategic-decision/coordinator.md
docs/guides/decision-authoring.md
docs/architecture/decision-runtime.md
docs/architecture/operator-experience.md
docs/README.md
README.md
~~~

Acceptance：bundled team validates；examples 通過 command-construction/smoke tests；文件正確描述 capability-routed REFERENCE/JUDGE/CHALLENGE。

## 13. Required test matrix

| Area | Required test |
|---|---|
| absent | no decision/request blocks -> decision off, request disabled |
| canonical | profile standard -> builtin standard synthetic alias |
| local | local standard wins over ergonomic alias |
| exact | exact builtin standard resolves |
| failure | unknown/blank profile fails closed |
| conflict | profile + default-profile fails even when equal |
| conflict | canonical + legacy routing fails |
| conflict | top-level + nested request fails |
| legacy | old default/request/routing remain valid |
| request | absent disabled; mapping enabled; null invalid; empty invalid |
| request | disabled legacy contract remains disabled |
| authority | preset cannot grant tools or agents |
| policy | old/new policy digest and plan equal |
| admission | old/new policy/request identities equal |
| resume | existing admission survives config change |
| paths | normal/direct/static/runtime-action/extra-model paths use TeamConfig request owner |
| explain | text/json/yaml share one projection |
| inspection | no provider call and no workspace creation |
| migration | dry-run only, semantic equality, idempotent output |
| templates | unresolved authoring template fails canonical migration |
| redaction | diagnostics/projections do not expose recognizable secrets |
| bundled | strategic-decision validates without placeholders |
| docs | canonical CLI examples are smoke tested |

## 14. Expected files and impact tracing

Primary files：

~~~text
internal/team/decision_authoring.go
internal/team/decision_authoring_test.go
internal/team/team_manifest.go
internal/team/parse.go
internal/team/decision_config.go
internal/team/decision_dispatch.go
internal/team/decision_admission.go
internal/team/request_contract.go
internal/agent/decision_config.go
internal/agent/decision_profiles.go
internal/agent/decision_profiles_yaml.go
internal/agent/agent.go
cmd/hufu/teamexplaincmd.go
cmd/hufu/decisioncmd.go
cmd/hufu/teammigratecmd.go
internal/team/team_manifest_migrate.go
.agent-teams/strategic-decision/team.yaml
~~~

實作者必須用 rg 找出所有 DecisionConfig.RequestContract、decisionConfig().RequestContract 與 manifest Decision consumers，不得只修改此清單。

## 15. Coding-agent execution rules

1. 先完成 Phase 0 tests，再改 production parser。
2. 每個 phase 保持可獨立 review。
3. 使用現有 catalog、normalization、digest、plan compiler、event journal、artifact store 與 admission APIs。
4. 不以 LLM 做 parsing、normalization、migration 或 validation。
5. static config errors 必須在 provider call 與 workspace mutation前失敗。
6. 不新增 silent fallback。
7. 不在 runtime 中依 display profile name 分支 execution semantics。
8. 不改 built-in profile policy bytes；digest golden 如改變即視為 regression。
9. deprecation metadata 不得成為 execution authority。
10. 不把 task-result journal 與 decision event journal混為一談。
11. JSON CLI additions 使用 explicit schema version DTO。
12. presentation output套用現有 secret redaction。
13. normal/direct/static/runtime-action/extra-model/resume 任一路徑仍讀 legacy request owner，Phase 2 不算完成。
14. migration 無法證明 semantic equality時 fail closed。

## 16. Validation gate

每個修改 Go source/tests 的 phase 完成時分別執行：

~~~bash
go test ./...
go vet ./...
golangci-lint run
~~~

最終另外執行：

~~~bash
go build ./cmd/hufu
go run ./cmd/hufu team validate .agent-teams/strategic-decision
go run ./cmd/hufu team explain .agent-teams/strategic-decision
go run ./cmd/hufu decision profile show standard
go run ./cmd/hufu decision plan --profile standard
go run ./cmd/hufu team migrate --dry-run --canonical-authoring .agent-teams/strategic-decision
~~~

不得使用 live provider 作為 acceptance 條件；所有新增功能都必須能以 unit/integration fixtures 完成驗證。

## 17. Definition of Done

完成時必須同時成立：

- decision.profile: standard 可直接使用；
- local profile precedence 不變；
- exact builtin refs 仍可使用；
- top-level request 有明確 presence/enable semantics；
- legacy authoring仍可載入；
- old/new authoring materialize 成相同 policy、digest、plan 與 request identity；
- runtime 只有一個 request contract owner；
- admission/resume semantics 不變；
- general model resolution 不變；
- validate/explain/profile inspection deterministic、read-only、provider-free；
- migrator dry-run、fail-closed、semantic-preserving；
- existing scaffolders 不會預設啟用 decision/request；
- bundled team 不含 invalid placeholders；
- 所有 tests、vet、lint、build 與 smoke commands成功。

成功條件是縮小 authoring surface，同時完全保留既有 DecisionEngine、authority、persistence 與 recovery semantics。
