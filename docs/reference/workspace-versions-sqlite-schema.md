# Workspace versions SQLite schema

> Status: active
> Authority: reference
> Verified-Commit: 2026-09-23
> Supersedes: —
> Superseded-By: —

`<Project.StateDir>/workspace-versions/workspace_versions.db` holds the
workspace snapshot DAG ([workspace versioning](../architecture/workspace-versioning.md)).
It is shared by every team of a project and opened with
`busy_timeout(5000)`, `foreign_keys(1)`, `journal_mode(DELETE)`, and
`synchronous(FULL)`; read-only connections use `mode=ro` and `query_only`.
The file is created with mode `0600` under an owner-only (`0700`) directory.

## Migrations

Migrations are defined in `internal/workspace/versionstore/migrations.go`
and applied by `internal/sqlmigrate`: each applied migration is recorded in
`schema_migrations(version, name, applied_at, checksum)` with the SHA-256 of
its SQL. A renamed, unknown, or edited migration fails closed. Version 1
(`initial_workspace_versions`) creates the whole schema below.

## Tables

### `snapshots`

| Column | Meaning |
|---|---|
| `id` | `wsv_<32 hex>` |
| `workspace_id`, `branch_id` | The managed team workspace and session branch the node belongs to. |
| `parent_snapshot_id` | DAG parent (references `snapshots`). |
| `root_tree_hash` | SHA-256 of the canonical root tree object. |
| `manifest_digest` | SHA-256 of the flattened leaf manifest. |
| `file_count`, `logical_bytes` | Leaves and blob bytes. |
| `materializable` | `0` when the tree has a symlink escaping the subject root. |
| `reason` | `baseline`, `external_drift`, `run_checkpoint`, `fork`, `checkout_save`, `restore`, `manual`, or `adopt` (`attempt` is reserved). |
| `run_id`, `task_id`, `attempt`, `anchor_event_id` | Optional provenance. |
| `commit_event_id` | The durable `workspace_snapshot_committed` event (unique). |
| `state` | `pending`, `published`, `orphaned`, or `pruned`. |
| `created_at`, `published_at`, `pruned_at` | Unix milliseconds. |

Triggers reject any update of the identity columns and any state transition
other than `pending→published`, `pending→orphaned`, and `published→pruned`.

### `branch_heads`

`(workspace_id, branch_id)` → `snapshot_id` with an optimistic `generation`
(the first head is inserted with generation 1). Insert and update triggers
require the referenced snapshot to be published and to belong to the same
workspace and branch.

### `operations`

Recovery checkpoints of multi-step operations: `kind` (`capture`, `fork`,
`checkout`, `restore`, `materialize`, `adopt`, `downgrade`, `gc`), `state`
(`prepared`, `objects_written`, `event_committed`, `materializing`,
`materialized`, `projection_updated`, `completed`, `failed`),
`from_snapshot_id` (what the files were), `to_snapshot_id` (what they
become), `target_branch_id`, and `detail_code`.

### `subject_state`

One row per subject root, keyed by `subject_key` (SHA-256 of the canonical
root): `subject_root` (the persistent binding), `mode_floor`, the
`materialized_*` diagnostic projection, `recovery_required` /
`recovery_code` / `recovery_detail` (paths only, never content),
`checkpoint_deferred`, and an optimistic `generation`.

### `stat_cache`

`(subject_key, path)` → size, mtime, ctime, inode, mode, and blob hash for
files that had settled (unchanged for two seconds) at capture time. It is
only a hint: any metadata difference means the file is re-read, and deleting
the table only costs performance.
