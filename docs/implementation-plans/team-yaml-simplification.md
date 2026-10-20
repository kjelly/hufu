# Hufu Team YAML Simplification — 可直接實作規格

> Status: active
> Authority: reference
> Verified-Commit: 2026-10-20
> Supersedes: docs/tmp/spec.md（本機草案）
> Superseded-By: —
>
> **文件 ID**：HF-TEAM-AUTHORING-UX-2026-10-20
> **類型**：Implementation Plan / Coding Agent Spec
> **狀態**：Ready for implementation（規格已收斂；新功能尚未實作）
> **基準**：`kjelly/hufu` 此工作目錄於 2026-10-20 的程式碼；實作前確認相關符號仍存在。
> **交付順序**：PR-0 → PR-1 → PR-2。每階段均須可獨立編譯、測試與使用。
> **文件位置**：`docs/implementation-plans/team-yaml-simplification.md`；依使用者指示移入正式文件並逐階段提交。

## 0. 目標與執行範圍

本計畫只保留 coding agent 可以在 repository 內完成並以自動化測試驗收的工作：

1. 修正缺少 authored coordinator 時的 lint 誤判，及對應的文件錯誤。
2. 新增 team-level `preset`，以一份 `team.yaml` 定義標準團隊；同階段提供 `team create --single-file`。
3. 新增 `agents` scalar shorthand 與 inline **worker**；與 Team Preset、Markdown 只支援**不同名成員組合**。
4. 同步 compile、validate、lint、explain、list、show、team add、模板、migration 與測試，使新語法從建立到檢查均可使用。

效益是減少樣板檔案與重複設定，保留既有 Markdown 進階能力。本功能不承諾提升模型品質、降低 token 用量或加快 worker 執行。

### 實作邊界

- 沿用 `decodeTeamManifestYAML`、`loadTeamWithMode`、`CompileTeam` / `InspectTeam`、既有 Team / Agent Preset Registry，不建立第二條執行管線。
- 不修改 coordinator / worker dispatch、任務 admission、持久化、resume、模型 target precedence、workflow / contracts、tool authorization 的權威與執行語意。
- 未使用新欄位／旗標的合法團隊與 CLI，保留既有行為；PR-0 的 `missing_coordinator` 診斷修正，以及下述 list 對不可解析 manifest 的報錯，是明列的例外。
- 不從 instructions 或任務自然語言推導 Preset、角色、工具授權或執行順序。
- 編譯、列舉與靜態檢查不啟動模型、MCP server、shell、verifier 或 acceptance，不建立 runtime workspace。
- 本計畫不包含部署、發布、遠端服務設定、真實模型帳號開通、使用者訪談或人工驗收；自動化測試使用 fixture / fake provider。

### 明確排除的功能

以下不是待辦，不得在本次實作：

- Preset 成員的同名 field patch、隱式繼承，及由 `.md` 同名取代 Preset 成員。
- Inline coordinator、自訂 inline `role`，及變更 legacy coordinator 名稱 heuristic。
- `--single-file` 覆寫既有目錄，或替所有既有 `--force` 建立流程重新設計 rollback。
- 將全部 lint error 與 runtime validity 強行統一、批次調整既有 lint 嚴重度或改寫 ignore 契約。
- 自動遷移現有團隊、compact export、新 workflow DSL、強制 reviewer gate、自然語言自動選擇高權限 Preset。

## 1. 已核對的基準與修改位置

| 現況 | 實作依據 |
|---|---|
| `team.yaml` 可省略，`.md` 可以沒有 frontmatter | `internal/team/parse.go` 的 `parseTeamYMLWithAuthoring`、`parseAgentContent` |
| flat 與 `hufu.io/v1alpha1/spec` 共用欄位 | `internal/team/team_manifest.go` 的 `teamManifestSpecFields`、`decodeTeamManifestYAML` |
| Team Preset 已有 `coding-single`、`coding-reviewed`、`research`、`safe-ops` | `internal/team/preset/team_preset.go` 的 `LookupTeam`、`Render` |
| Agent Preset 已有 `readonly`、`coding`、`review`、`research`、`writer`、`ops` | `internal/team/preset/preset.go` 的 `Lookup` |
| Preset grants 與 explicit allowlist 聯集，再套 deny | `internal/team/parse.go` 的 `parseAgentContent` |
| 缺少 authored coordinator 時 runtime 有 fallback，lint 卻回報 error | `internal/team/coordinator_prompt.go:GetOrchestratorDef`、`parse.go:FindingMissingCoordinator` |
| 無明確 coordinator 時，runtime 仍依名稱含 `coordinat` / `orchestr` 選取 | `GetOrchestratorDef` 的第二段搜尋；本次保留舊行為 |
| 空字串工具清單在 selector 中表示全部可用工具 | `internal/agent/agent.go:SelectToolNames`；新語法須防止意外產生空字串 |
| Helper 在載入時注入，且包含寫檔工具 | `loadTeamWithMode`；不得把簡短設定或 `readonly` 成員描述成整個團隊唯讀 |
| Team discovery 已接受只有 `team.yaml` 的目錄 | `internal/team/discovery.go:hasTeamFile`；補整合測試即可，不重寫 discovery |
| `hufu list` 目前只掃描 `.md` | `cmd/hufu/list.go:collectListedTeam`、`printTeam`；必須支援新成員來源 |
| explain 的 provenance 重新讀取 `.md` | `internal/team/effective_spec.go:newEffectiveTeamSpec`；新成員來源須由編譯結果提供 |
| 既有相容性 snapshot 沒有 prompt、model、skills、Agent timeout | `internal/team/compat_snapshot.go:AgentSnapshot`；不能單靠它證明等價 |
| 既有 create helper 對 preexisting 目錄失敗時不復原被覆寫內容 | `cmd/hufu/team_create_core.go:writeTeamFilesWithValidation`；新旗標拒絕既有目錄 |

其他必讀：`internal/team/team_lint*.go`、`team_policy_lint.go`、`team_manifest_migrate.go`、`cmd/hufu/teamcmd.go`、`teamcreatecmd.go`、`teamaddcmd.go`、`teamexplaincmd.go`、`teamshowcmd.go`、`teamlintcmd.go`。

## 2. 唯一支援的組合規則

### 2.1 Team-level `preset`

```yaml
preset: coding-reviewed
```

在記憶體中得到 `developer`、`reviewer`，再沿用 Helper 注入與 coordinator 選取；不寫出暫存或生成的 `.md`。

- `preset` 必須是非空 YAML string，經 Registry 既有大小寫／空白正規化後，以 `LookupTeam` 查詢。
- 未知名稱報錯並列出 Team Preset 名稱；Agent-only Preset（例如 `coding`）不得當作 Team Preset。
- 展開的 name、role、description、System、tools、side effect、generation fallback、限制及 Helper 行為，須與該 Team Preset 的 `Render()` 產物走原管線等價。
- 只展開現有 Registry 定義，不自動加入 workflow、acceptance、unattended、學習、額外 sandbox 或更高權限。
- 保留舊的 `team.yaml` 可省略行為。新增欄位是 additive extension，不更換 apiVersion。

### 2.2 Inline worker

```yaml
agents:
  architect: readonly
  coder:
    preset: coding
    instructions: |
      實作功能、執行測試，提供修改檔案與測試結果。
  reviewer:
    preset: review
    instructions: 檢查 correctness、regression 與測試缺口。
```

`architect: readonly` 等價 `{preset: readonly}`。每個 inline worker 都是完整的新宣告，不繼承其他成員。

| 可接受欄位 | 型別與規則 |
|---|---|
| `preset` | **必填**非空 string，僅接受 `preset.Lookup` 的 Agent Preset。Scalar shorthand 提供此欄位。 |
| `description` | String；省略時用 id，明示空白字串拒絕。 |
| `instructions` | String；省略時為 `Perform the <id> role for this team.`；明示只有空白拒絕。正規化後對應 `AgentDef.System`，與 Markdown body 相同地 trim 首尾空白。 |
| `model`、`reasoning-effort` | String；省略／空字串依現有 Agent generation 規則處理，不改 CLI / profile / team / global precedence。 |
| `tools` | 非空 string、string list、或只有 `allowed` / `denied` 的 mapping；兩個子欄位接受相同的 string / list。空 list 可表示不額外加 grant／deny，但不撤銷 Preset。明示空字串、空白名稱、未知子欄位、錯誤型別拒絕。新 inline 不接受 `all` wildcard，工具須具名。 |
| `skills`、`allowed-paths` | String 或 string list，沿用既有 splitting、path expansion、skill 解析與下游校驗；不接受 null、非字串項目。 |
| `timeout`、`max-steps` | 非負 integer，須在既有欄位型別範圍內；省略或 0 保留原 parser 的繼承／預設語意。 |
| `max-retries` | 非負 integer；省略仍產生現有的 unset sentinel，明示 0 表示不重試。 |
| `no-net` | Boolean；省略為既有預設。`false` 不得撤銷 team/global 的網路限制。 |

解碼要求：

- `agents` 必須是 mapping；`agents: {}` 合法，明示 null 拒絕。每個 value 僅接受非空 string 或 mapping。
- String 欄位須檢查 YAML tag／node kind，拒絕 number、boolean 被 decoder 隱式轉成 string；所有欄位拒絕明示 null。
- 未知欄位一律拒絕，包括 `name`、`role`、`system`、`prompt`、`permissions`、`provider`、`execute-after`。
- 進階 Agent 欄位（`requires`、`result-contract`、`worker-workspace`、`execution-route`、`memory`、`mcp-tools`、`side_effect` 等）仍使用 `.md`，不在本次擴充 inline schema。
- `<id>` 使用 `^[a-zA-Z][a-zA-Z0-9_-]*$`；Registry identity key 沿用既有 `normalizedName`，顯示名稱保留 id。Inline 不另產生第二個 alias。
- 保留 `helper`。此外，新 inline id 在小寫化後不得含 `coordinat` 或 `orchestr`，包括 `coordinator`：以靜態錯誤建議改名或使用 `coordinator.md`。這只是新語法的名稱限制，不改舊 `.md` heuristic。
- Inline role 一律為 `worker`。需要自訂 coordinator 時新增 `.md`；沒有時採現有選取規則。若混合的 legacy `.md` 命中名稱 heuristic，仍保留原行為，不宣稱一定使用內建 fallback。

### 2.3 不同名成員組合

```yaml
preset: coding-reviewed
agents:
  architect:
    preset: readonly
    instructions: 先分析設計與風險。
```

可以另有 `security-reviewer.md` 或 `coordinator.md`；最終 roster 是 Team Preset、inline、Markdown 的不重複聯集，再注入既有 Helper。

**所有不同來源的同名情況均拒絕，沒有 precedence、patch 或 replacement。** 衝突判定同時包含 `.md` filename alias 與 explicit `name`，採既有大小寫不敏感 identity 規則；同一 Markdown definition 的 name / alias 指向自己不算重複。

| 輸入 | 結果 |
|---|---|
| Preset `reviewer` + inline `reviewer` | 拒絕，即使 inline 完整指定了 preset |
| Preset `reviewer` + `reviewer.md` | 拒絕，不繼承也不覆寫 |
| Preset `reviewer` + `custom.md` 的 `name: reviewer` | 拒絕，包含 alias collision |
| Inline `architect` + `ARCHITECT.md` | 拒絕 |
| Inline YAML 重複 key／大小寫正規化後重複 | 拒絕，不採最後一個值 |
| 三種來源使用不同 identity | 接受；排序與 map 插入順序不影響結果 |
| 兩個明確 Markdown coordinator | 維持既有拒絕／fatal finding |

想自訂 Preset 成員時，改用不含 team-level `preset` 的完整 `agents` 或既有 `.md` 定義；不要在錯誤訊息引導使用未支援的覆寫語法。此規則在 PR-1 首次允許 Preset + Markdown 時就必須完成，不能留待後續 PR。

### 2.4 權限與模板

**權限：** 共用既有 Agent normalizer，Preset grants 與 explicit allowlist 聯集，展開 implied tools，再套 deny。新 inline 正規化後若 tools 為空字串，必須拒絕（含 deny 掉全部工具的情況），不得交給把空清單視為全開的 selector，也不在本次修改全域 selector。Preset 展開同樣檢查此不變量，防止 Registry 未來產生空 grant。Runtime 的 always-included tools、team/global deny、capability、approval、network/path policy 保持原權威；不宣稱零工具或絕對唯讀。`review` 含 bash，`side_effect: none` 不是 OS sandbox。

**模板：** 不改 manifest bootstrap 時序。Inline 與 team manifest 共用 `readTeamManifestSource` / `renderTeamManifest` 的一次渲染，只接受現有 `vars` 與 caller overrides 可提供的變數，之後不再套用 Agent template。

- Inline **不自動取得**後續 loader 才補入的 `TEAM_NAME`、`AGENT_COUNT`、`AGENT_NAMES`。需要時可明示提供同名 vars（仍遵守既有保留／覆寫規則），或改用 `.md`；文件範例不得假設這些變數已自動存在。
- 缺值、recursive delimiter、渲染後殘留 `{@` / `@}` 的情況，按既有 manifest 規則報錯；不新增 escape 或二次解析機制。
- Markdown 的模板時序與變數集合保持不變；不承諾與 inline 在未提供相同變數時等價。
- 等價測試使用兩種格式皆可用且值相同的變數；另測試 `.md` 的既有 `TEAM_NAME` 行為與 inline 缺值拒絕。
- Inline 的 `AGENT_COUNT` / `AGENT_NAMES` **輸出 roster 計算**仍須包含所有新增 worker 並依 definition 去重；不因此提前對 inline prompt 注入這些變數。

### 2.5 版本化格式

```yaml
apiVersion: hufu.io/v1alpha1
kind: AgentTeam
metadata:
  name: dev
spec:
  preset: coding-reviewed
  agents:
    architect: readonly
```

只在共用 `teamManifestSpecFields` 追加 typed `preset` / `agents`。保留 envelope 要求、flat 格式與 strict unknown-field rejection。舊 binary 不識別新欄位時拒絕是預期行為，不得靜默降級為其他 roster。

## 3. 實作設計與共用介面

1. 新增 `internal/team/team_composition.go`（檔名可調整），處理 typed declaration、Preset expansion、來源資料及 identity collision；接入既有 loader 註冊階段，早於 contract / delegation / action catalog 綁定、skill discovery 與 worker vars 計算。
2. 從 `parseAgentContent` 抽出不依賴檔案的正規化 helper，讓 `.md`、TeamPreset declaration、inline 均產生相同 `AgentDef`。保留 Markdown 現有 lenient decode / fallback；新 inline 的嚴格型別驗證在呼叫 helper 前完成，不順便收緊舊格式。
3. 不先生成 Markdown 字串再回灌 parser。使用 `TeamPreset.Agents` 的 typed 資料；以現有 `Render()` 產物作獨立等價測試基準，避免兩條展開邏輯漂移。
4. 自訂 `UnmarshalYAML` 時須在方法內明確驗證 allowed keys、scalar tags、重複 key、required preset；不可假設外層 `KnownFields(true)` 會檢查自訂 decoder 內部。沿用現有 manifest mapping 的 alias / merge / cycle 檢查，不建立較寬鬆的 YAML 路徑。Typed declaration 保留 authored field presence 與 scalar / mapping shape，序列化時不寫入 normalizer 產生的預設或 runtime-only 資料；migration 的 raw decode 不執行 Preset lookup 或模板渲染，lookup 留在 render 後的 composition normalization。
5. 可沿用 `loadTeamContractTasks` 的方式，以同一組 manifest read/decode helper 取得 composition；不為減少一次 YAML decode 重寫整條 loader。相同輸入及 vars 必須產生相同結果。
6. 候選 identity 全部檢查後才註冊；不得先覆寫 session map 再找衝突。重用既有 Helper、identity、coordinator 與 downstream validation。
7. 編譯結果攜帶 process-local authoring provenance；`TeamSourceIndex.Agents` 需涵蓋新 definition，供既有 tool / skill / prompt lint 掃描，不能只新增 YAML 行號卻漏掃 inline instructions。新來源的索引須與模板渲染後內容一致。
8. `EffectiveTeamSpec` 的 name / role / tools / side-effect 來源及 explain 使用此 provenance，不再把沒有 `.md` 的新成員標成 builtin。保留現有 `ValueSource` enum 與 JSON 欄位；以 `SourcePreset` / `SourceTeam` 搭配 `Detail` 表示 `team-preset:<name>/<id>`、`inline:<manifest>:<field>`。工具來自 Agent Preset 時保留該來源與 deny 資訊。
9. Provenance 不寫入 execution-policy hash，不改現有 policy snapshot schema；以實際相同的 `AgentDef` / policy 測試格式等價與 drift。若出現差異，先修正 normalization，不能移除既有 drift 欄位或放寬 resume。

### 診斷規則

新 declaration 在 runtime compile／validate 拒絕，lint 也不得通過 `--fail-on error`。錯誤需含 owning manifest、`agents.<id>.<field>`（版本化格式為 `spec.agents...`）與可採取的建議；有 YAML node 時保留 line / column。跨來源衝突列出雙方來源，不依讀取順序選 winner。

- 在 lint inspection 中，能安全投影的語意錯誤以既有 `TeamLintFinding` 表達；新 code 固定為 `team_preset_unknown`、`inline_agent_invalid`、`inline_agent_tools_empty`，跨來源 identity collision 沿用 `duplicate_agent`。
- 結構／型別解碼失敗維持 `load_error` / exit 2；不能產生安全有效 roster 時 `Complete=false`，不得繼續派工。Fatal semantic finding 維持 exit 1。
- 使用既有 `exact` / `rendered` / `field_only`；無法定位時不可捏造精確行列。
- 新 finding code 加入既有 known-code registry 並測試 `--ignore` 的原投影／threshold 行為；ignore 不會改變 compile 的有效性，也不授權 runtime 執行。
- 不修改現有 lint JSON schema version、欄位型別、`--fail-on`、`--ignore`、unknown schema 與其他 exit-code 契約。

### CLI 整合

| 介面 | 必須交付的行為 |
|---|---|
| `team validate` / `team lint` / `team explain` / runtime | 經過同一個 composition 與 identity 檢查，候選 roster 一致。 |
| `hufu list`，text / table / JSON | 新語法團隊透過共用離線 inspection 取得已解析 roster，按 definition 去重並穩定排序，顯示 Preset / inline / authored Markdown 成員，不新增注入的 Helper 或 fallback coordinator。保留原 JSON 欄位名稱與型別；未使用新語法的合法團隊維持原輸出與 best-effort Agent 欄位讀取。先用共用 manifest reader / decoder 判定是否含新欄位；manifest 不可解析或新語法 inspection 不完整時，回傳非零並在 stderr 指向來源，不退回空 roster。這項錯誤處理也適用不可解析的 legacy manifest，須補 CLI 測試與 help 說明。JSON 輸出先收集成功再編碼，失敗時不輸出半份文件。 |
| `team show` | 保持 authored view，呈現實際 `preset` / `agents`，不展開成 `.md` 或當作使用者寫過的 Agent 檔。保留原 manifest 檔名及 versioned shape。 |
| `team add` | 寫入前用同一 composition identity 資料檢查欲新增名稱；與 Preset / inline 衝突即拒絕，`--force` 不能跨來源取代。不同名仍建立 `.md`。舊 `.md` 對 `.md` 的 force / rollback 語意不變。 |
| `team migrate` | 保留原 migration 語意；新增欄位在 flat → v1alpha1 round trip 不遺失 declaration、instructions 或模板，不自動改成 compact 格式。 |
| `@team` / 直接 `@worker` / `run --team` | 新成員與既有 session 成員同樣可解析、委派；以 fake provider / 無模型解析測試，不依賴真實模型。 |

## 4. 分階段工作與驗收

### PR-0 — Coordinator lint 與文件更正

**範圍只限已確認的不一致，不進行全面 validator 重構。**

- [ ] 缺少 authored coordinator 時，取消 `FindingMissingCoordinator` 的 error，改為 info；訊息準確說明使用既有 coordinator 選取／fallback，不能忽略 legacy 名稱 heuristic。
- [ ] Info 不使 `TeamInspection.Complete` 變 false；`lint --fail-on error` 成功，`--fail-on info` 可按既有契約失敗。
- [ ] 保留多個明確 coordinator、Helper collision、duplicate identity、unknown schema、invalid YAML 的原錯誤行為；其他 lint error、policy override、ignore、deprecated alias 嚴重度均不變。
- [ ] 更新相關 unit / CLI JSON / process exit tests；檢查 `team create --preset coding-reviewed` 原始產物不再被誤判缺少 coordinator。
- [ ] 修正 `README.md`、`README.tw.md`、`docs/reference/agent-format.md` 與 CLI help 中和 parser 不符的必填敘述；精確說明 filename inference、frontmatter / team.yaml 可省略。
- [ ] 不加入 `preset` / `agents` 新語法。

**完成條件**：worker-only、明確 coordinator 與既有 Preset scaffold 在 compile／validate 與預設 lint 門檻下對 coordinator 問題有一致結論；不承諾所有既有 lint 規則與 runtime validity 完全相同。

### PR-1 — 單檔 Team Preset 與建立／檢查體驗

- [ ] 新增 flat / v1alpha1 共用 typed `preset`、Registry expansion、process-local provenance。
- [ ] 抽出來源無關 Agent normalizer，先供 Markdown / Team Preset 共用；不要等 PR-2 才建立，避免 PR-1 另做一套不相容的展開。
- [ ] 同時完成 Preset + `.md` 的不同名組合與同名拒絕，包括 explicit name／alias；不要留下未定義的混合狀態。
- [ ] 接入 compile / validate / lint / explain / list / show / team add / migrate；保留 discovery，新增只有 manifest 的整合案例。
- [ ] 提供 opt-in `team create NAME --single-file`：預設 `coding-single`，可用 `--preset` 指定 Team Preset，`--model` 有值時寫入。產物只有 `team.yaml`。
- [ ] 本模式拒絕 Agent-only Preset、`--force`、`--wizard`、`--expanded`、`--from`，以及明示的 `--category`；即使某個互斥旗標明示為 false／空值，也依 `Flags().Changed` 拒絕。所有拒絕在寫檔前完成。原本未帶 `--single-file` 的流程不變。
- [ ] 不存在的 team 才能建立：使用 exclusive `os.Mkdir` 取得新目錄所有權；若 parent 不存在只建立 parent。目標已存在（含空目錄／symlink）即拒絕，不以先 Stat 再 MkdirAll 當作排他保證。
- [ ] 只對本次成功取得所有權的新目錄寫檔、compile 並執行 `ValidateEffectiveTeam` 的 error gate；寫入或驗證失敗時只清理本次建立的產物，再以非遞迴 remove 嘗試移除空目錄，保留原有 parent／其他 team 及並行新增的非本次檔案。測試注入寫入或驗證失敗，不新增供使用者觸發故障的旗標。
- [ ] 比較所有 Team Preset 的 compact / Render fixture：既有 snapshot 加第 5 節補充比較；確認 fallback、Helper、model precedence 與 policy hash。
- [ ] 同 PR 更新 README 中英、reference、`.agents/skills/team-builder/SKILL.md`、CLI help / examples，說明不同名限制及 `coding-reviewed` 不代表強制 review gate。
- [ ] 此階段 `agents` 尚不支援，維持 strict unknown-field rejection。

**完成條件**：`hufu team create dev --preset coding-reviewed --single-file` 生成一份 manifest；discover、compile、validate、lint、explain、list、show 與 fake-provider run 全鏈可用，所有非法組合不寫入既有目錄。

### PR-2 — Inline worker 與三種來源的不同名組合

- [ ] 加入本文件精確列出的 `agents` typed schema、scalar shorthand、strict decoder；不接受 inline role／進階欄位／同名 patch。
- [ ] Inline 重用 PR-1 的來源無關 normalizer，保留 Markdown 兼容解析，驗證空工具結果拒絕。
- [ ] 完成 Preset + inline + Markdown 的不同名組合；重複、保留名、heuristic 名稱等錯誤有穩定來源資訊。
- [ ] 註冊所有成員後再做 contracts、delegation、action catalog、worker routing、memory/result-contract/team workspace defaults 與 model route 的現有綁定及驗證；不得漏掉新成員或 alias 去重。
- [ ] 補 source index 與 prompt/tool/skill lint；inline instructions 的 finding 指回 YAML 欄位。
- [ ] 擴充 PR-1 的 list / show / add / migrate / direct worker 功能與 fixtures，確保三種來源都能檢查及列出。
- [ ] 實作第 2.4 節模板行為與缺值測試，不更動現有模板 bootstrap 或 coordinator selector。
- [ ] 更新文件與 Team Builder Skill，提供單檔 → 不同名混合 → 既有 Markdown contracts 的範例；有被排除欄位的需求時，明確使用 `.md`。

**完成條件**：第 2.2 / 2.3 / 2.5 節案例通過 compile／validate／lint／explain／list；同變數條件下 inline 與對應 `.md` 的有效 AgentDef 等價；所有禁用語法在執行前拒絕。

## 5. 必要自動化測試矩陣

| ID | 階段 | Case 與預期 |
|---|---|---|
| T01 | PR-0 | 只有 worker.md：compile / validate 成功；lint 為 info、Complete=true；error/info threshold 各自正確。 |
| T02 | PR-0 | 明確 coordinator、兩個 coordinator、Helper / alias collision：維持原有效性與 fatal 診斷。 |
| T03 | PR-0 | unknown schema、invalid YAML、deprecated tasks alias、ignore、policy overrides：原 JSON / exit contracts 不變。 |
| T04 | PR-0 | 原始 create coding-reviewed scaffold：validate / lint error 門檻成功；舊檔名與 role 推導測試不變。 |
| T05 | PR-1 | 各 Team Preset compact / Render fixture：完整語意比較，flat / v1alpha1 等價；大小寫／空白 lookup 同 Registry。 |
| T06 | PR-1 | 未知／Agent-only／null／非 string Team Preset：strict error 或 fatal finding，不靜默 fallback。 |
| T07 | PR-1 | Preset + 不同名 .md 成功；同名、大小寫、explicit name / alias collision 均拒絕。 |
| T08 | PR-1 | manifest-only discovery、list 三種 output、show authored 欄位、explain provenance、@team 及 @worker。 |
| T09 | PR-1 | single-file 預設／指定 Team Preset、有無 model：只產生合法 team.yaml；未帶新旗標的舊輸出 golden 不變。 |
| T10 | PR-1 | single-file 互斥旗標、Agent-only Preset、既有目錄／空目錄／symlink、模擬 create race：寫入前拒絕，原檔 bytes 不變。 |
| T11 | PR-1 | 新目錄寫入／編譯／驗證失敗：只回收本次產物；既有 sibling / parent 保留。 |
| T12 | PR-1/2 | team add 遇 Preset／inline 同名（含 --force）拒絕且原檔不變；不同名成功；legacy .md force 行為不變。 |
| T13 | PR-2 | scalar / mapping worker、description / instructions defaults、明示 generation / timeout / max-retries: 0：符合規定。 |
| T14 | PR-2 | null、number-to-string、負數／overflow、unknown field、重複 key／case-fold key、缺 preset、空 instructions：拒絕並定位。 |
| T15 | PR-2 | role / coordinator / orchestrator-helper / helper、進階欄位：新 inline 拒絕；舊 .md 同名／heuristic 行為不變。 |
| T16 | PR-2 | Preset grant + explicit allow + deny、implied tools、network/path 限制：和同內容 .md 相同；wildcard / 空字串 / deny-all 空結果拒絕。 |
| T17 | PR-2 | 三來源不同名聯集與全部重名矩陣：固定結果、穩定排序、無 alias 重複計數。 |
| T18 | PR-2 | instructions / skill 相關 lint 確實執行且指回 YAML；explain 不把 inline 當 builtin；list 無漏列或半份 JSON。 |
| T19 | PR-2 | 共同 vars 的 inline / .md prompt 等價；缺值／recursive／殘留 delimiter 拒絕；inline 未提供 TEAM_NAME 失敗，明示提供成功；.md 原變數行為不變。 |
| T20 | PR-2 | contracts / delegation / action catalog / capability routing 引用新 worker 成功，引用不存在或不可達 worker 依原規則拒絕。 |
| T21 | PR-1/2 | Team / Agent / CLI / profile generation、worker-model / execution-route、team worker defaults：舊 precedence 不變。 |
| T22 | PR-1/2 | 在相同 team name、project root、provider / backend 設定與環境下，新舊格式使用相同有效配置的 policy snapshot 相容。工具／target 變更後，兩種格式的 resume 接受／拒絕結果須與既有 Markdown baseline 一致；原本已固定的政策欄位仍拒絕 drift，不新增 snapshot 欄位／版本，也不放寬比較。 |
| T23 | PR-1/2 | inspect / list / validate / lint / explain 無模型、MCP、shell、網路與 workspace 寫入；run 整合採 fake provider。 |
| T24 | PR-1/2 | migration round trip 保留新 declaration、instructions 與 literal template；改變 map 順序不改有效配置或 hash。 |
| T25 | 全部 | 無新欄位的既有長篇 manifests、bundled teams、Helper availability、workflow / contracts / verification：原 golden 與整合測試不退化。 |

### 等價證據的必要補強

保留現有 `CompatSnapshot` golden，但新增 test-only composition comparison，按唯一 definition 排序後另比較：

- name、FileAlias、role、description、System、Capabilities、skills、tools、side effect、recovery。
- generation 所有欄位、ProviderURL（fixture 使用非敏感佔位值）、ExtraModels、timeout、max-steps、max-retries。
- network/path、guard、requires、MCP declaration，以及 team 繼承的 memory、result-contract、worker-workspace、execution-route 等有效政策。
- worker 候選與 vars、coordinator 實際選取結果、Helper 行為，以及現有 execution-policy configuration hash。

測試可直接比較正規化的 AgentDef／現有 policy 投影，勿把所有 runtime session 狀態直接序列化，也不要為測試擴大 public CLI JSON、輸出 system prompt 或敏感資訊。來源位置與 provenance 是刻意不同的欄位，不列入執行等價 hash。

可重用：`internal/team/team_manifest_test.go`、`team_lint_test.go`、`agent_preset_test.go`、`agent_identity_inference_test.go`、`team_preset_integration_test.go`、`effective_spec_test.go`、`compat_snapshot_test.go`、`bundled_team_compat_test.go`、`execution_policy_snapshot_test.go`；`cmd/hufu/team_create_core_test.go`、`teamcreatecmd_test.go`、`teamcreatewizard_test.go`、`teamaddcmd_test.go`、`teamlintcmd_test.go`、`teamexplaincmd_test.go`、`teamshowcmd_test.go`、`usability_test.go`。新增 composition 與 list 的專門測試，避免只測 decoder 而漏掉 CLI。

## 6. 每階段必跑驗證與總完成條件

每個修改程式碼的 PR，實作對應測試後執行並成功完成：

```bash
go build ./cmd/hufu
go test ./internal/team/... ./cmd/hufu/...
go vet ./...
golangci-lint run
```

全計畫完成後再執行 `go test ./...`，並對臨時目錄中的生成案例跑 CLI process tests（validate / lint / explain / list / show / migration / fake-provider run）。每階段的 lint 都是硬性 gate，不延後到最後；若 Unix socket 暫存路徑過長，以短 `TMPDIR` 重跑並記錄環境差異，不把未通過當成功。

每階段交付記錄只需 coding agent 可產生的資訊：實際修改、測試命令與結果、規格驗收對照。測試失敗須修正後重跑；環境限制未排除前如實標示未完成，不聲稱驗收通過。不要求模型帳號、外部部署或操作員先執行工作。

總完成條件：

- [ ] PR-0、PR-1、PR-2 各自工作項與所屬測試通過。
- [ ] 一份 YAML 可建立標準 Preset 團隊或自訂 inline worker 團隊；不同名 Markdown 成員可共存。
- [ ] 同名來源、禁用欄位與危險空工具結果均在執行前拒絕。
- [ ] 建立、discover、list、show、explain、validate、lint、direct worker、migration 均支援已交付的新語法。
- [ ] 舊 authoring、runtime／resume／權限契約維持原語意，且有 snapshot 以外的 prompt / model / policy 證據。
- [ ] README 中英、reference、Team Builder Skill 與 CLI examples 描述相同的支援邊界。
- [ ] 全套測試、vet、lint 通過；沒有依賴人工決策、真實模型或遠端環境的未結事項。
