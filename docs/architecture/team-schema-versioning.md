# Versioned AgentTeam Schema 實作計畫

> Status: draft
> Authority: reference
> Verified-Commit: `deee6bd298069e49132f64da959ff754f92498d5`
> Supersedes: —
> Superseded-By: —
> Priority: P1

## 1. 目標

建立可版本化、可 migration 的 public AgentTeam manifest：

```yaml
apiVersion: hufu.io/v1alpha1
kind: AgentTeam
metadata:
  name: hufu-coding
spec:
  ...
```

同時保留現有 flat `team.yaml` 相容性。

## 2. 現況

已存在：

- strict YAML decode。
- `teamConfigYAML` / `agent.TeamConfig`。
- `LoadTeam` / registry。
- `EffectiveTeamSpec`。
- `hufu team validate` / `team explain`。
- bundled team tests。
- `internal/team/compat_snapshot.go`（`CompatSnapshot` / `BuildCompatSnapshot`）
  已經是一組 deterministic 的 team effective-semantics golden snapshot 機制，
  搭配 `internal/team/testdata/team-compat/*/team/team.yaml`（9 個 fixture
  team）與 `TestTeamCompatFixtures`。schema versioning 的 normalize-equivalence
  驗證應該延伸這個既有機制，不要另外重建一套。
- `agent.TeamConfig.RequiredResources`（yaml `required-resources`，
  `internal/agent/agent.go`）是 baseline 之後（`ffce546`..`deee6bd`）才落地
  的 Generic Required Resource Lock 欄位，已接進 context injection、
  pre-dispatch admission、skill disclosure。這是目前 `spec:` 已支援 contract
  的一部分，必須明確列入 v1alpha1 範圍（見 §7）與 normalize-equivalence 測試。

缺：

- explicit schema version。
- version dispatch。
- envelope。
- deterministic normalize/migration。
- unsupported future version behavior。

## 3. 原則

1. versioning 在 runtime config 前完成。
2. runtime 只吃 normalized config。
3. legacy flat manifest 視為 `legacy-flat-v0`。
4. unknown `apiVersion` fail closed。
5. migration pure/deterministic。
6. 不用 `map[string]any` 吞 unknown fields。
7. authoring schema version 與 runtime state schema分離。

## 4. Data model

```go
type TeamManifest struct {
    APIVersion string `yaml:"apiVersion"`
    Kind       string `yaml:"kind"`
    Metadata   TeamMetadata `yaml:"metadata"`
    Spec       TeamSpecV1Alpha1 `yaml:"spec"`
}

type TeamMetadata struct {
    Name string `yaml:"name"`
    Labels map[string]string `yaml:"labels,omitempty"`
    Annotations map[string]string `yaml:"annotations,omitempty"`
}

type NormalizedTeamConfig struct {
    Version string
    Config  agent.TeamConfig
}
```

第一版不要搬動整個 `agent.TeamConfig`。

## 5. Loader

```text
raw bytes
→ detect envelope
→ strict decoder for version
→ normalize
→ existing semantic validation
→ TeamConfig
```

不得先 decode current struct 再猜版本。

## 6. Version dispatch

第一版用明確 switch 即可：

```go
switch apiVersion {
case "":
    decodeLegacyFlat(...)
case "hufu.io/v1alpha1":
    decodeV1Alpha1(...)
default:
    return UnsupportedSchemaVersionError
}
```

第二個正式版本出現前，不必先做複雜 registry。

## 7. v1alpha1 範圍

`spec` 直接承接現有已支援 contract：

```text
generation
worker/coordinator model
providers/backends
workflow
policies
capabilities
verification
retry
decision
routing-policy
action-providers
required-resources (RequiredResourceSpec / RequiredResources)
memory-learning
tasks
...
```

`required-resources` 是 baseline commit 之後才新增的欄位（Generic Required
Resource Lock），必須明確列在範圍內，不能被 strict unknown-field 拒收，也不能
在 normalize-equivalence 測試矩陣中被漏掉（見 §12
`TestLegacyAndV1Alpha1NormalizeIdentically`）。

`advanced:` 替代命名空間（`teamConfigYAML` 的 `rawAdvancedSection` /
`mergeAdvancedNamespace`，目前是 `workflow`/`reliability`/`verification`/
`retry` 的拼寫別名）只保留在 `legacy-flat-v0`。v1alpha1 的 `spec:` 不支援
`advanced:` alias，一律使用正式欄位名；`spec:` 內出現 `advanced:` 視為
unknown field，依 strict decode 規則 fail closed。dry-run migrator（PR-3）
必須先把 legacy team 的 `advanced:` 值展開回正式欄位再輸出 v1alpha1 YAML，
確保原本使用 `advanced:` 別名的既有 team 遷移後語意不變（見 §12
`TestV1Alpha1RejectsAdvancedNamespace` / `TestTeamMigrateExpandsAdvancedNamespaceAlias`）。

不要趁機大改 field naming。

## 8. Validation order

```text
syntax/strict fields
→ apiVersion/kind
→ metadata
→ normalize
→ conflict/duplicate checks
→ semantic validation
→ model/backend resolution
→ runtime admission
```

錯誤需帶 file/schema version/field path/reason。

## 9. CLI

擴充：

```bash
hufu team validate
```

輸出 schema/source-format。

新增：

```bash
hufu team migrate <team-dir> --to hufu.io/v1alpha1 --dry-run
```

第一版只 dry-run，不直接覆寫。

## 10. 檔案

新增：

```text
internal/team/team_manifest.go
internal/team/team_manifest_legacy.go
internal/team/team_manifest_v1alpha1.go
internal/team/team_manifest_normalize.go
internal/team/team_manifest_test.go
cmd/hufu/teammigratecmd.go
```

修改：

```text
internal/team/parse.go
internal/agent/agent.go
cmd/hufu/teamcmd.go
cmd/hufu/team_loader.go
```

`internal/team/parse.go`（現時約 1753 行）與 `internal/agent/agent.go`
（現時約 1822 行）已經超過 CLAUDE.md 的 800 行/檔案上限。這兩個檔案的改動必須
限制在薄的 envelope-detection/dispatch hook（例如 `parseTeamYML` 開頭偵測
`apiVersion` 後轉呼叫 `team_manifest*.go` 內的邏輯），normalize/migrate/
version-dispatch 的實際邏輯全部放在 §10 新增的 `team_manifest*.go`。不得把
這些邏輯灌回 `parse.go`/`agent.go` 既有函式，也不得讓既有函式的 cyclomatic
complexity 提高——`.golangci.yml` 的 gocyclo 門檻是 40 且只降不升的 ratchet，
新程式碼不得新增 exclusion。

## 11. PR

### PR-1 Golden normalized fixtures
延伸既有 `internal/team/compat_snapshot.go`（`CompatSnapshot` /
`BuildCompatSnapshot`）與 `internal/team/testdata/team-compat/*` fixture，不要
另建第二套平行的 golden-fixture 機制。先確認 `CompatSnapshot` 是否已涵蓋
`RequiredResources` 等欄位，不足的話先補上，再 snapshot bundled teams
effective config，作為 legacy/v1alpha1 normalize 一致性的比對基準。

### PR-2 v1alpha1 parser
envelope + strict unknown field rejection + normalize。

### PR-3 dry-run migrator
stable YAML + round-trip。

### PR-4 canary bundled teams
先轉 `hufu-coding`、`strategic-decision`。

### PR-5 wider migration
legacy reader仍保留。實際遷移範圍共 20 個 legacy `team.yaml`/`team.yml`：
11 個 `.agent-teams/*/team.yaml`（bundled teams，PR-4 canary 的
`hufu-coding`、`strategic-decision` 已涵蓋在內）+ 9 個
`internal/team/testdata/team-compat/*/team/team.yaml`（PR-1 沿用的
golden-fixture team）。後者應該和 PR-1 的 fixture 更新一起處理，不要拆成
獨立的第三批次；PR-5 只需處理剩下的 9 個非 canary bundled team。

## 12. Tests

```text
TestLegacyFlatTeamStillLoads
TestV1Alpha1TeamLoads
TestUnknownAPIVersionFailsClosed
TestUnknownKindFails
TestV1Alpha1RejectsUnknownField
TestLegacyAndV1Alpha1NormalizeIdentically
TestManifestMetadataNameMismatchFails
TestAdvancedNamespaceConflictStillFails
TestV1Alpha1RejectsAdvancedNamespace
TestTeamMigrateExpandsAdvancedNamespaceAlias
TestBundledTeamsNormalize
TestTeamMigrateDryRunRoundTrips
TestTeamMigrateDoesNotChangeEffectiveSpec
```

## 13. Rollout

```text
read both
→ docs/examples prefer v1alpha1
→ generator writes v1alpha1
→ legacy deprecation
→ later removal
```

## 14. 非目標

- extension manifest。
- runtime event schema。
- AgentDef Markdown redesign。
- Kubernetes CRD dependency。
- 大規模欄位 rename。

## 15. Done

- manifest 明確 version。
- unknown version fail closed。
- legacy可讀。
- semantic-equivalent fixtures完全一致。
- coding agent可依 schema author team。

## 16. Coding-agent 指派

先做 normalized golden fixtures，再加入 `hufu.io/v1alpha1` envelope，最後 dry-run migrator。
不要把 schema versioning變成 TeamConfig大重構。
