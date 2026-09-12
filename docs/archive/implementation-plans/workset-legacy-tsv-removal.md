# Legacy TSV Workset Removal 實作計畫

> Status: archived
> Authority: reference
> Verified-Commit: `902096a`（PR-1～PR-3 evidence；discovery fix `2535d8b`）
> Superseded-By: [workset-path-fanout deprecation record](../../deprecations/workset-path-fanout.md)
> Priority: P1
> Historical-Baseline: `58676a6bf0f01f28216c8537592bf6ba34f94a75`
> Canonical authority: [workset](../../architecture/workset.md)
> Scope: legacy workspace-relative TSV `FanOutSpec.source`

本文件是 PR-1～PR-3 的歷史實作計畫；這些階段已完成並落地。尚未開放的 PR-4
（breaking removal）by release gate 由正式 deprecation record 持續追蹤。本文件
不得作為重新實作既有機制或提前移除 legacy reader 的請求。

## 實作結果（2026-09-12）

- Stage 0（discovery 修正，非原計畫項目，實作中發現）：
  `legacy_fanout_deprecated` 原本只認 `.tsv` 副檔名，與 runtime 判斷（非
  `.json` 即 legacy）不一致，已修正並補回歸測試
  （`TestTeamLintLegacyFanoutDetectsNonTSVExtension`）。commit `2535d8b`。
- PR-1（shadow comparison helper + consumer-shaped fixture）：已完成，見
  `internal/team/workset_legacy_compare_test.go`
  （`TestCompareLegacyAndArtifactWorksetOrdering`、
  `TestCompareLegacyAndArtifactWorksetKeys`、
  `TestCompareLegacyAndArtifactWorksetTemplateSubstitution`、
  `TestCompareLegacyAndArtifactWorksetChildCount`、
  `TestLegacyAndArtifactWorksetConsumerShapedFixtureEquivalent`）。
  commit `134b3d3`。
- PR-2（bundled teams 零 legacy 使用防護）：已完成，見
  `TestBundledTeamsHaveNoLegacyFanOutFinding`
  (`internal/team/team_lint_test.go`)。commit `96764ca`。
- PR-3（`--reject-legacy-fanout` opt-in 升級 warning→error）：已完成，見
  `TeamLintOptions.RejectLegacyFanOut`、`hufu team lint --reject-legacy-fanout`
  (`internal/team/team_lint.go`、`cmd/hufu/teamlintcmd.go`)。commit `902096a`。
- PR-4（breaking removal）：未開始，且現在不能開始——見下方「Removal gate」。

## 0. 現況核實（避免重做已完成的部分）

在寫這份計畫前已對照現有程式碼，確認以下項目**已經實作完成**，PR 不應重做：

- Canonical artifact-backed 流程：`internal/team/fan_out.go`
  (`usesStructuredWorkset`) 判斷 legacy／structured 的依據是
  `FanOut.SourceArtifact` 非空，或 `Source` 副檔名為 `.json`——不是單純「有沒有
  `source-artifact` 欄位」。
- `internal/team/workset_fanout.go`：producer artifact 解析、CAS 完整性驗證、
  run/task/attempt/agent occurrence 驗證、`WorksetExpansionReceipt` 建立與
  duplicate-key 偵測，均已存在且有測試覆蓋
  （`wp02_workset_test.go`、`wp04_workset_complete_test.go`、
  `workset_artifact_resolution_test.go`、`workset_security_test.go`）。
- `legacy_fanout_deprecated` finding 已存在
  （`internal/team/contract_finding.go`），`hufu team lint --format json`
  已可列出它。
- Generic migration 兩個必要 fixture（transform / probe）已完成：
  `internal/team/wp06_generic_fixture_test.go`
  （`TestWP06GenericWorksetFixtures/transform`、`/probe`，deprecation record
  §Required migration evidence 已引用這兩個測試）。
- Legacy／artifact 的 ordering + template substitution 對等性已有測試：
  `internal/team/wp07_legacy_fanout_test.go`
  （`TestLegacyAndArtifactFanOutPreserveOrderingAndTemplateSubstitution`）。
- 目前唯一使用 `fan_out` 的 bundled team 是
  `.agent-teams/hufu-code-review/team.yaml`，且已使用 canonical
  `source-artifact`，沒有任何 bundled team 使用 legacy `source` TSV。

## 1. 目標

`FanOutSpec.Source` 指向 workspace-relative 純 TSV（沒有 `SourceArtifact`、副檔
名也不是 `.json`）的 legacy 路徑，走的是 `internal/team/fan_out.go` 裡的
`expandFanOutTask`（小寫、非 receiver 方法）。這條路徑刻意保持最小：**不建立
`WorksetBinding`、不產生 `WorksetExpansionReceipt`、沒有 workset ID、沒有
duplicate-key 或 stale-source 檢查**。這正是它被 deprecate 的原因，不是需要補齊
到跟 structured 路徑對稱的地方。

本計畫目標：完成 `docs/deprecations/workset-path-fanout.md` 所列 removal gate
所需的剩餘 migration evidence，最終讓 `hufu` 只接受 `source-artifact`。

## 2. Canonical invariants（已實作，僅列出供交叉核對）

```text
producer task
artifact ID/digest
workset ID
stable item keys
child mapping（duplicate-key 偵測）
expansion receipt
whole-group verification（workset_complete）
```

實作位置：`internal/team/workset.go`、`workset_fanout.go`、
`workset_verification.go`。Path 不作 identity——這點已成立。

## 3. Discovery

`legacy_fanout_deprecated` finding 與 `hufu team lint --format json` 已存在
且已驗證可用。`hufu team scan-legacy --feature workset-path` 判定為非必要：
目前沒有任何已知 consumer（bundled 或其他）觸發這個 finding，等真的出現需要跨
多個 team 目錄批次掃描的情境時再加。

## 4. Migration fixtures

已完成：

```text
generic transform（wp06_generic_fixture_test.go::transform）
generic probe（wp06_generic_fixture_test.go::probe）
ordering + template substitution equivalence（wp07_legacy_fanout_test.go）
consumer-shaped synthetic fixture（workset_legacy_compare_test.go）
```

仍待：real consumer-owned fixture——目前沒有真實 consumer 使用 legacy TSV；
若日後出現真實 consumer 再補上並取代 synthetic 版本。

## 5. Shadow comparison helper（範圍已依現況修正；已實作）

```go
func compareLegacyAndArtifactWorkset(
    t *testing.T, workspace string, legacyTask, artifactTask TaskDef,
) worksetComparison
```

（`internal/team/workset_legacy_compare_test.go`）

**只比較 legacy 路徑實際擁有的行為維度**：

- item ordering
- unique keys（作為輸出比較，而非 legacy 端的「偵測」——legacy 本來就不偵測
  duplicate key，重複列會被當成兩個獨立 child）
- scalar template substitution
- child count

**不要求**對以下維度做 legacy／artifact 對等測試，因為 legacy 路徑結構上就沒有
這些語意（`expandFanOutTask` 回傳的 `TaskDef` 沒有 `WorksetBinding`）：

- duplicate-key rejection（legacy 沒有偵測，只有 artifact-backed 有——這是
  移除 legacy 的理由，不是要補的對等項）
- stale-source rejection（legacy 沒有 receipt 可比對新鮮度）
- workset-scoped resume/retry/cancel 穩定性（legacy 沒有 workset ID 可綁定）
- expansion receipt 存在性

一般 task 層級的 retry/resume/cancel（跟是不是 workset 無關，展開後兩種路徑產
生的都是普通 `TaskDef`）已有既存 characterization test 覆蓋
（`wp02_resume_test.go`、`wp08_retry_disposition_test.go`）。

## 6. Migration helper（optional，未做，低優先）

```bash
hufu team migrate-workset <team-dir> --dry-run
```

只在真的出現需要遷移的 team 時才做；目前零已知 legacy consumer，做這個工具沒有
使用對象。只對 deterministic static source 產 skeleton；runtime 不得自動用
path 猜 artifact producer identity。

## 7. Removal gate

Removal gate 以 `docs/deprecations/workset-path-fanout.md` 的「Removal gate」
章節為唯一權威來源，本文件不重複列出以避免兩份文件日後失準（這正是
execution-compatibility sunset 計畫曾經發生過的問題）。PR-4 開始前必須重讀
該文件確認條件是否全部成立——截至本文件封存時，這個 deprecation 連
compatibility warning 都還沒有隨任何 release 發布過，release-gate 不成立。

## 8. 檔案

新增（已完成）：

```text
internal/team/workset_legacy_compare_test.go   # 第 5 節 helper 與其測試
```

延伸（非重寫）既有檔案：

```text
internal/team/team_policy_lint.go   # discovery 修正
internal/team/team_lint.go          # --reject-legacy-fanout
cmd/hufu/teamlintcmd.go             # --reject-legacy-fanout flag
internal/team/team_lint_test.go     # PR-2/PR-3 tests
```

`cmd/hufu/team_workset_migrate.go`、`internal/team/workset_migration_test.go`
只在第 6 節 migration helper 真的要做時才新增。

最後 removal PR 才修改（breaking，尚未動）：

```text
internal/team/coordinator.go
internal/team/fan_out.go
internal/team/team_policy_lint.go
```

## 9. PR

### PR-1 Shadow comparison helper + synthetic consumer fixture（已完成）
範圍：第 5 節 helper（只涵蓋 legacy 實際具備的維度）+ 第 4 節 synthetic
consumer-shaped fixture。

### PR-2 確認 bundled teams 零 legacy 使用（已完成，gate check，非遷移）
目前唯一使用 `fan_out` 的 bundled team 已用 `source-artifact`。這個 PR 的實際
工作是加一個「bundled teams 不得使用 legacy fan_out source」的 lint 斷言，
防止未來有人新增 legacy 用法。

### PR-3 Newly-authored legacy config hard warning/error policy（已完成）
新建立的 team 若使用 legacy `source`，lint 可選擇由 warning 升級為
error（`--reject-legacy-fanout`）。

### PR-4 Remove reader（breaking，release-gated；依第 7 節條件；未開始）

## 10. Tests

已實作（第 5 節 helper 對應）：

```text
TestCompareLegacyAndArtifactWorksetOrdering
TestCompareLegacyAndArtifactWorksetKeys
TestCompareLegacyAndArtifactWorksetTemplateSubstitution
TestCompareLegacyAndArtifactWorksetChildCount
TestLegacyAndArtifactWorksetConsumerShapedFixtureEquivalent
```

已存在、未重寫（PR review 對照用）：

```text
TestWP06GenericWorksetFixtures/transform
TestWP06GenericWorksetFixtures/probe
TestLegacyTSVFanOutIsDeprecatedButStillCompatible
TestLegacyAndArtifactFanOutPreserveOrderingAndTemplateSubstitution
TestWorksetCompleteVerificationRejectsFailureExtraChildAndStaleSource
TestStructuredFanOutRejectsStaleOccurrencesButAcceptsRepeatedContent
TestWorksetReceiptBindsEveryChildAndReplays
```

PR-2 已實作：

```text
TestBundledTeamsHaveNoLegacyFanOutFinding
```

PR-3 已實作：

```text
TestTeamLintRejectLegacyFanOutEscalatesSeverity
TestTeamLintRejectLegacyFanOutProcessExitContract
```

Removal PR（尚未做）：

```text
TestLegacyFanOutSourceIsRejected
```

## 11. Fail-closed migration

遇到：

```text
dynamic path
runtime-generated TSV without producer artifact
ambiguous key field
consumer-specific parser semantics
```

只能報 manual migration required，不得由 runtime 自動猜 artifact producer
identity。

## 12. Done

- parser 不再接受 legacy path source（或回明確 unsupported）——PR-4 尚未做。
- bundled teams/fixtures 全 artifact-backed（現況已是如此，PR-2 只是加防護）。
- breaking release notes 含 migration——PR-4 尚未做。
- canonical workset replay/verification 完整（現況已完整，非本計畫產出）。

## 13. Coding-agent 指派（歷史記錄）

先做 PR-1（shadow comparison helper，只涵蓋 legacy 實際擁有的行為維度）；
PR-2 是防護性 lint 斷言，不是遷移；PR-3 才是把 legacy 從 warning 升級為
error 選項；PR-4 是獨立、release-gated 的 breaking removal，開始前必須重新
核對 `docs/deprecations/workset-path-fanout.md` 的 removal gate 是否全部成立。
不得在 runtime 自動由 path 推導 artifact identity。
