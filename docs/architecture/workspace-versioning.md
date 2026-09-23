# Workspace versioning

> Status: active
> Authority: normative
> Verified-Commit: 2026-09-23
> Supersedes: —
> Superseded-By: —

Workspace versioning makes `hufu session fork` and `hufu session checkout`
switch the **project files** (the subject root) together with the session
projection. Snapshots of the managed files are stored in a content-addressed
object store (CAS) and a SQLite snapshot DAG under the project's state
directory. It is not a Git integration: Git is used only to discover
candidate files, and Git's HEAD, branch, index, and `.git` bytes are never
touched.

The implementation plan, decision log, and implementation record are in
[the archived plan](../archive/implementation-plans/workspace-versioning.md).

## Modes

`workspace-versioning.mode` lives in the global hufu config
(`~/.config/hufu/hufu.yaml`, merged with `./hufu.yaml`), because it is a
property of the subject root shared by every team of a project.

| Mode | Behavior |
|---|---|
| `off` (default) | Exactly the pre-versioning behavior. |
| `observe` | Runs record snapshots and commit events; `session fork` / `checkout` stay metadata-only. Checkpoints take the project lock briefly and are skipped, never blocking, when it is busy. |
| `required` | Fork, checkout, and restore switch files. One coordinator per project may run at a time. Non-Git subject roots need a `.hufuignore`. |

Versioning is only enabled on linux; other platforms compile but treat every
mode as off. Only managed workspaces (registered with `hufu workspace`) are
versioned; unmanaged `-w` control roots stay metadata-only.

Once a subject root has run in `required` mode its **mode floor** is
recorded; configuring a lower mode (including `off`) is refused until
`hufu workspace version downgrade <mode>` lowers the floor explicitly.

## Scope

Only regular files and symlinks inside the subject root that pass the
inclusion policy are versioned. Not versioned: files written outside the
subject root (for example through `--allow-path`), ignored and unmanaged
paths, `.git` and Git submodule contents, empty directories, and external
side effects (network calls, services, databases).

### Inclusion policy

Exactly three kinds of path are excluded:

1. any path segment named `.git`;
2. the control-root subtree when a control root lies inside the subject root
   (a control root equal to the subject root cannot be versioned);
3. Git-ignored paths (in a Git work tree) or `.hufuignore` paths (otherwise).

Git submodules and nested repositories are unmanaged. Project directories
named like hufu bookkeeping (`logs/`, `history/`, `tasks/`, `shared/`,
`status/`) are versioned normally.

`.hufuignore` syntax: one pattern per line; blank lines and `#` comments are
ignored; a leading `/` (or a `/` anywhere except at the end) anchors the
pattern at the subject root, otherwise it matches a basename at any depth; a
trailing `/` matches directories only; globs use `path.Match`. Negation `!`
and `**` are rejected with the line number. An empty `.hufuignore` is a valid
explicit "exclude nothing".

## Storage

```text
<Project.StateDir>/workspace-versions/
├── workspace_versions.db      # 0600, snapshot DAG, heads, operations, subject state
├── lock / lock.owner          # project lock and its diagnostic owner record
├── objects/{blob,tree,link}/sha256/<2>/<62>   # 0600, immutable
└── tmp/
```

The schema is documented in
[the workspace versions SQLite schema](../reference/workspace-versions-sqlite-schema.md).
Objects are hashed while written, fsynced, and hard-linked into place, so a
published object is never overwritten ([Go function: putObject](../../internal/workspace/versionstore/cas.go)).
Trees use a canonical binary encoding (format v1) whose hash is pinned by a
golden test ([Go function: EncodeTree](../../internal/workspace/versionstore/codec.go)).

## Snapshot lifecycle and publication

A snapshot is `pending` after capture, `published` once canonical, `orphaned`
if it never became canonical, and `pruned` when GC reclaimed its objects
(the row and its lineage stay). Identity fields are immutable and only those
transitions are allowed; SQLite triggers enforce both, and branch heads may
only reference published snapshots of the same branch.

Publication is event-first: CAS objects are durable, the pending row exists,
`workspace_snapshot_committed` is appended to the branch's event log (payload:
IDs, hashes, counts only), and only then does
[Go function: PublishSnapshot](../../internal/workspace/versionstore/heads.go)
publish the row and advance the head with an optimistic generation.

The workspace state of an event E is the last `workspace_snapshot_committed`
at or before E in E's branch lineage. Without one (a legacy lineage), a
workspace-aware fork fails closed; `--metadata-only` forks only the session.

## Session operations

- **fork** of the active branch reuses its snapshot tree (no bytes copied).
  A fork of an earlier branch or event first saves the active branch's live
  files, then materializes the snapshot at the fork point.
- **checkout** saves the live files of the current branch, materializes the
  target branch's head, and only then rebuilds `session.json` and switches
  the active branch. Branches without a head need `--metadata-only`.
- **restore** appends a restore node on the active branch whose tree is the
  chosen snapshot's tree; history is never rewritten.

Materialization happens through `os.Root`, writes before deletes, replaces
or deletes only paths the current snapshot manages, fails with a collision
instead of overwriting an unmanaged path or following a symlink, removes
directories that became empty, and verifies the result
([Go function: Materialize](../../internal/workspace/versionstore/materialize.go)).

## Runtime checkpoints

v1 checkpoints only at quiescent run boundaries, never per attempt, because
native workers write the subject root concurrently:

- **admission** (every public invocation, right after `checkRunAdmission`):
  settle pending snapshots, refuse to run over incomplete operations or a
  recovery marker, and make the branch head equal the live files — a
  `baseline` for a new branch, `external_drift` for edits made between runs
  (by the user or by another team of the project);
- **run checkpoint** (after every worker stopped, before `run_finished`):
  record the run's files as `run_checkpoint`. A failure adds a run warning
  and a `checkpoint_deferred` marker but never blocks `run_finished`; an
  interrupted run is only marked deferred.

When a Codex attempt changes paths outside its writable roots, a
`recovery_required` marker is recorded, the run checkpoint is skipped, and
required-mode admission fails until `hufu workspace version adopt` accepts the
files or `hufu workspace version restore <head>` undoes them.

## Locks

| Lock | Holder | Purpose |
|---|---|---|
| team lock (`<state>/locks/<workspace_id>.lock`) | every managed run; workspace-aware session and workspace version commands | same-team exclusion |
| project lock (`workspace-versions/lock`) | required-mode coordinators for their whole lifetime; every mutating version command | cross-team exclusion |

Both are non-blocking and always taken team first. A conflicting operation
fails with "workspace version operation conflicts with active execution" and
names the recorded holder.

## Recovery

Recovery runs before any workspace operation (required-mode coordinator
startup and every mutating session or workspace version command). A pending
snapshot with a durable commit event is published; one without is orphaned.
Recovery never appends or replays events. Interrupted checkouts and restores
are completed forward; a fork is completed forward once its child's commit
event is durable, and otherwise its eventless inactive child branch is
removed.

## Maintenance

| Command | Behavior |
|---|---|
| `hufu workspace version status` | Mode, floor, active head, live drift, markers, incomplete operations, snapshot and CAS counts, and commit metrics projected from events. |
| `hufu workspace version doctor [--repair]` | Store integrity, commit events, head-to-lineage consistency, legacy branches, live drift, markers, stale temp files and lock owners. `--repair` only settles pending snapshots and operations, resets heads to the event lineage, and removes stale temp files. |
| `hufu workspace version gc [--apply]` | Dry run by default. Roots are all heads, label-pinned snapshots, incomplete operations, the materialized projection, pending snapshots within the grace period, and each branch's `retention.keep-recent-snapshots` most recent snapshots. Other published snapshots become `pruned`; unreferenced objects and temp files older than `retention.orphan-grace` are deleted. |
| `hufu workspace version downgrade <mode>` | Explicitly lower the mode floor. |

GC and doctor never run `VACUUM`, `PRAGMA optimize`, or checkpoints
([SQLite maintenance policy](sqlite-maintenance-policy.md)).

## Invariants

1. Published snapshots never change identity; heads only reference published
   snapshots of their branch.
2. A head advances only after its commit event is durable.
3. Every object's content hashes to its name.
4. A historical fork never uses a snapshot from after the fork point, and a
   missing, pruned, or corrupt snapshot never falls back to the live files.
5. Materialization deletes only paths the current snapshot manages and
   never escapes the subject root.
6. The control-root subtree is never captured, written, or deleted.
7. Automatic recovery never appends events.
8. In required mode only one coordinator runs per project, and file-changing
   version operations only run under the project lock.
