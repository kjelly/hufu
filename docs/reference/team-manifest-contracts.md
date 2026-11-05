# Team manifest contracts

> Status: active
> Authority: reference
> Verified-Commit: —（本次工作樹，尚未提交）
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

For requirements that vary with each invocation, enable `request-acceptance:
true` or pass `--request-acceptance`. This supplements static contracts with a
frozen, model-resolved request and an evidence-bound completion review. See
[per-request runtime acceptance](request-acceptance.md).

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
自訂 Preset 成員時，使用不含 team-level preset 的 inline 或 Markdown 完整 roster。新欄位會隨 `team migrate --dry-run` 保留，migration 不展開模板。
`team explain` 顯示展開後的來源與權限，`team show` 顯示 authored YAML，`hufu list` 可列出虛擬成員；不可解析的 manifest 會讓 list 回報錯誤，而非輸出不完整 JSON。

`coding-reviewed` 不表示強制派工順序或 reviewer gate；需要客觀完成條件仍使用既有 workflow / contracts / verification。
Helper、side effect、網路與 path policy 沿用現有 runtime。`review` 含 bash，Preset 名稱不是唯讀 sandbox；`safe-ops` 原有缺少 reconcile 的 lint error 仍保留。
Flat `agents`／版本化 `spec.agents` 可宣告以 Agent Preset 為基礎的 inline worker，例如 `agents: {inspector: readonly}`。
Team Preset、inline 與 Markdown 只允許不同名組合；同名不提供 patch 或 replacement。
Static contracts、delegation、action catalog 與 team worker defaults 可引用這些 worker；per-agent 進階契約及 coordinator 仍以 Markdown 宣告。
完整欄位、工具限制與模板時序見 [inline worker 規格](agent-format.md#inline-worker)。

## Native image policy (M0)

`hufu.yaml` 的 `media` 是 operator 授權；flat team manifest 的 `media` 或版本化 manifest 的 `spec.media` 只能加嚴。未設定 operator 授權時，team 不能自行啟用。此階段提供私有圖片入庫、canonical turn／task binding 與配額；含圖任務會明確 blocked，尚未提供 CLI `--image` 或模型圖片傳輸。

```yaml
media:
  enabled: true
  image:
    max-count: 2
  allowed-targets:
    - backend: fixture_backend
      provider-key: fixture_provider
      model: fixture_vision
      image-token-upper-bound: 8192
```

這是離線 fixture 配置，實際 target 必須存在於 frozen execution policy；不是可直接使用的 provider。完整 limits 與 ceiling 見[圖片實作規格](../implementation-plans/native-image-runtime.md#72-明示目標授權不猜測-localremote)。限制及 token upper bound 必須是 YAML 整數，不接受會被截斷的小數。未知欄位、wildcard、重複 target、超出 ceiling 的值均拒絕。operator 各配置層與 team 取交集；停用不可被後續層重新啟用，target 陣列省略代表繼承，明示 `[]` 代表拒絕所有目標。每張圖片 token upper bound 取較大值，不能以縮小估算放寬容量。

授權凍結 backend／provider／model、實際 backend identity hash 與 limits。圖片內容留在同一 ArtifactStore 的 private namespace；source paths 不寫入 binding。重複圖片保留 part 順序，但 branch storage 配額按 unique blobs 計算；fork 繼承祖先的 admitted 使用量，重新開始 invocation 不會重設配額。含圖任務停用自動結果 cache、歷史結果自動重用與 inflight 合併。

Migration：未啟用 media 的舊文字 policy hash 與 artifact layout 不變；新增欄位為 optional，不改寫歷史 events／receipts。`--no-journal` 只停用 task-result journal，仍須具備 durable event journal 與 private CAS 才能接收圖片。引用式 conversation、完整恢復與獨立 audit 仍屬後續 M0 階段，不能由這份 storage／binding 支援推定已完成。

## Fixed batch contract graphs

Static contracts can own a fixed batch graph using contract IDs:

```yaml
contracts:
  - id: draft
    agent: writer
    phase: verify
    side_effect: none
    recovery: retry
  - id: check
    agent: checker
    phase: verify
    depends-on-contracts: [draft]
    on-failure-contract: draft
    on-failure-classes: [verification]
    max_retries: 1
```

With static contract binding enabled, dispatch both contracts in one `agent`
call using only their `agent`, `contract_id`, goal, and needed evidence inputs.
The runtime resolves the named edges to batch positions and persists the
resulting task IDs. Every connected group must appear exactly once in the same
batch, including the producer when only the checker declares edges. Model
`depends_on`, `on_failure`, and `pipeline` values are replaced for this group.
Batch order does not change which contracts the edges reference.

Targets must exist, remain in the same phase, and have no fan-out. Dependencies
must be unique and acyclic. A failure target must be the task itself or a
transitive dependency. Invalid declarations fail preflight; incomplete or
duplicate groups fail before creating tasks. Existing failure-class checks,
retry limits, remediation context, and recovery behavior still apply. Contract
hashes and the execution policy snapshot pin the declarations and retry policy;
changing them requires a new run rather than resuming an older policy.

## Dependency result view: configuration foundation

`dependency-result-view` is static contract policy, available only with static
contract binding and same-batch dependencies. The configuration and persistence
foundation is installed; selected execution currently fails explicitly before
provider startup until the delivery integration is installed. It does not fall
back to full. See the [implementation plan](../implementation-plans/dependency-result-projection-and-contract-regressions.md).

```yaml
contracts:
  - id: produce
    agent: producer
  - id: consume
    agent: worker
    depends-on-contracts: [produce]
    dependency-result-view:
      mode: selected
      fields: [summary, findings, artifacts, files_modified, verification]
```

Omission and `mode: full` use the existing formatter, policy version 5 and
contract revision 1 without changing their bytes or hashes. Full must not
include `fields`. Selected requires nonempty, unique fields from the five shown
above; mode and field names are lowercase. Unknown keys and null policies fail
loading. Field order is canonicalized in the order shown.

Selected is rejected with required evidence, `evidence_from`, result contracts,
native actions, sidecar execution, plan-first or closed tool sequences. Dynamic
`agent` task input cannot set this field. Selected contracts use revision 2 and
policy version 6, pinning the normalized policy in the configuration hash and
immutable occurrence admission. Task events, checkpoints, replay, clones and
manifest migration preserve it. Existing sessions cannot acquire selected
policy through resume or configuration edits. Binaries supporting only policy
versions through 5 must reject version 6; there is no policy down-migration.
This version distinction is separate from the [runtime-store schema upgrade](runtime-grant-storage.md).

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
