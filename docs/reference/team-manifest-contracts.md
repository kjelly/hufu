# Team manifest contracts

> Status: active
> Authority: reference
> Verified-Commit: 2026-10-20
> Supersedes: —
> Superseded-By: —

`contracts:` declares static execution constraints. `workflow:` optionally controls
runtime phase order and completion. Neither is a list of work already dispatched:

```text
workflow (optional phase lifecycle)
  contracts (authored constraints, with stable IDs)
    task occurrence (created for this run)
      attempt / result / receipt
```

Contracts also work without a workflow. The runtime binds the same
`EffectiveTaskContract` regardless of the YAML collection's spelling; hashes,
revisions, task persistence and workflow behavior are unchanged. Coordinator
dispatch continues to use the existing binding rules, including exact
`contract_id` for task-goal binding. `when-goal-contains` remains unsupported.


## 單檔 Team Preset

標準團隊可只有 `.agent-teams/dev/team.yaml`：

```yaml
preset: coding-reviewed
```

也可在 `hufu.io/v1alpha1` manifest 的 `spec.preset` 宣告；版本化格式仍需 `metadata.name`。
可用 Team Preset 為 `coding-single`、`coding-reviewed`、`research`、`safe-ops`，與 Agent frontmatter 的 `preset` 是不同 namespace。
使用 `hufu team create dev --preset coding-reviewed --single-file` 可產生單檔；`--model` 為選填，沒有 `--preset` 時使用 `coding-single`。
此模式不覆寫既有目錄，不與 `--force`、`--wizard`、`--expanded`、`--from`、明示的 `--category` 共用；省略新旗標時維持原輸出。

Preset 成員與不同名 Markdown 成員可共存。跨來源相同 name／filename alias（含大小寫折疊）一律拒絕，不提供 patch、繼承或 replacement。
自訂 Preset 成員時，使用不含 team-level preset 的原有 Markdown 定義。新欄位會隨 `team migrate` 保留，migration 不展開模板。
`team explain` 顯示展開後的來源與權限，`team show` 顯示 authored YAML，`hufu list` 可列出虛擬成員；不可解析的 manifest 會讓 list 回報錯誤，而非輸出不完整 JSON。

`coding-reviewed` 不表示強制派工順序或 reviewer gate；需要客觀完成條件仍使用既有 workflow / contracts / verification。
Helper、side effect、網路與 path policy 沿用現有 runtime。`review` 含 bash，Preset 名稱不是唯讀 sandbox；`safe-ops` 原有缺少 reconcile 的 lint error 仍保留。
此階段尚未支援 `agents` inline 語法。

## Authoring

This fragment shows placement; the team's agents and execution requirements must
also be declared:

```yaml
apiVersion: hufu.io/v1alpha1
kind: AgentTeam
metadata:
  name: example
spec:
  delegation:
    bind-task-goal-contracts: true
  contracts:
    - id: inspect
      agent: reviewer
      side_effect: none
```

| Format | Accepted collection |
| --- | --- |
| `hufu.io/v1alpha1` | `spec.contracts` (recommended), `spec.tasks` (deprecated) |
| Legacy flat | `contracts` (recommended), `tasks` or `advanced.tasks` (deprecated) |

`advanced.contracts` and versioned `spec.advanced` are not supported. Declaring
more than one accepted collection fails at load time, even if values are `[]` or
`null`. This intentionally tightens the old flat loader, which could accept an
empty root `tasks` alongside `advanced.tasks`. A single `tasks: null` still means
no static contracts. YAML merge mappings and aliases participate in presence
checks; normal YAML precedence applies to an override of the *same* key.

## Lint and preview migration

```bash
hufu team lint path/to/team --format json
hufu team lint path/to/team --reject-legacy-tasks
hufu team migrate path/to/team --dry-run --canonical-contracts
```

The deprecated keys produce one `legacy_tasks_key_deprecated` warning with a
source path and line. Existing finding codes stay stable, while their field paths
follow the authored collection. `--reject-legacy-tasks` escalates only this warning
to an error. Default `--fail-on error` accepts warnings; `--fail-on warning` CI
will fail on the new warning and should migrate or explicitly waive it.

Migration writes YAML only to stdout, leaving source bytes and mtime unchanged.
Review the preview before adopting it. Ordinary migration continues to emit
`spec.tasks` for older binaries; `--canonical-contracts` explicitly opts into
`spec.contracts`. It is independent of `--canonical-authoring`; both may be used
together. Empty collections may be omitted from serialized output.

The migrator reads unrendered source. Quoted string templates such as
`'{@ .scope @}'` remain templates. Typed or structural templates that cannot be
decoded as raw YAML are rejected for manual review, rather than rendered using a
specific run's values. Re-serialization does not preserve comments, anchor
spelling or layout. Output is re-decoded and compared using the complete authored
contract representation and compiler identities; the partial compatibility
snapshot alone is insufficient to prove equivalence.

Upgrade the binary **before** adopting `contracts:`. Older strict decoders reject
that spelling. To roll back the binary, first revert the collection key to the
equivalent `tasks:` form; no database migration is needed. Internal Go task types,
coordinator task JSON and persisted events retain their established names.
