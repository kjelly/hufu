# Managed workspace migration

> Status: active
> Authority: guide
> Verified-Commit: 2026-09-17
> Supersedes: —
> Superseded-By: —

Hufu now separates the project agents operate on from the durable state used
to coordinate them. `SubjectRoot` is the source repository. `ControlRoot` is a
registry-managed directory under the platform state root. With no workspace
flags, real execution resolves or creates the managed control root for the
discovered project and selected team.

## Existing workspaces

Hufu does not silently adopt or move legacy `<project>/workspace/<team>` data.
If an active managed workspace does not exist and a legacy source is present,
execution stops with the exact migration command:

```sh
hufu workspace migrate --team <team>
```

Migration copies the source into staging, takes an online SQLite snapshot,
verifies canonical state, and atomically publishes the managed workspace. It
does not rename, delete, or rewrite the legacy source. If more than one source
candidate exists, select its direct parent explicitly:

```sh
hufu workspace migrate --team <team> --legacy-root /exact/legacy/workspace
```

Preview the resulting registry state with read-only commands:

```sh
hufu workspace list --all
hufu workspace show
hufu workspace path --team <team>
hufu workspace doctor
```

## File-based runtime state

Hufu keeps a workspace's event journal and runtime state in `runtime.sqlite`.
A workspace written by an older Hufu, with `session.json` and
`logs/event_store.jsonl` instead, is refused until it is converted:

```sh
hufu workspace convert-storage --team <team> --dry-run
hufu workspace convert-storage --team <team> --yes
```

Conversion imports the event log with full hash-chain validation and the
session checkpoint, session tree, compaction state, conversation history,
terminal sessions, reliability report, task journal, and decision index as
they are. It verifies everything in a staging database before replacing
anything, holds the workspace lock so it never runs beside a run, and moves
the original files into `legacy-runtime-files/` rather than deleting them.
`--dry-run` imports and verifies without changing the workspace, including
checking all backup destinations and their parent directories for conflicts.
An existing database is never treated as a completed import on its own: recovery
requires a conversion record with matching source and database content hashes.
An empty, corrupt, unrelated, or subsequently modified database is refused
without moving the originals.

Failures before database publication leave the workspace unchanged. If archiving
is interrupted after publication, the error reports that `runtime.sqlite` is
installed and archiving is incomplete. Resolve the reported filesystem error
and rerun the same command (or preview with `--dry-run`). Recovery verifies every
imported source at its original or backup path, then moves only the remaining
originals and writes the completion note. Do not run the workspace until this
finishes: runtime writes change the imported state and prevent automatic
recovery. Recovery also handles interruption after the last file move but before
the completion note is written. Existing backup files are never overwritten.

`--workspace` remains an exact, unmanaged compatibility override;
`--workspace-root` remains a root to which the team name is appended once;
`--temp` remains unregistered and is cleaned up after execution.

## Removal and recovery

Delete is recoverable: it validates the registry and ownership marker, takes
the execution lock, and renames the control root into managed trash.

```sh
hufu workspace delete --team <team> --yes
hufu workspace list --all
hufu workspace restore <trash-id> --yes
```

Permanent deletion is deliberately separate:

```sh
hufu workspace purge <trash-id> --yes
```

Garbage collection is a dry-run unless both `--apply` and `--yes` are present:

```sh
hufu workspace gc
hufu workspace gc --apply --yes --trash-older-than 720h
```

When a lifecycle operation is interrupted, run `hufu workspace doctor` first,
then `hufu workspace doctor --repair`. Repair only applies deterministic,
marker-verified crash recovery; it never adopts an unknown directory.

## Shell navigation

`shell-init` prints static helper functions and never edits shell profiles.
Choose the command appropriate for the current shell:

```sh
eval "$(hufu workspace shell-init bash)"
eval "$(hufu workspace shell-init zsh)"
hufu workspace shell-init fish | source
$code = hufu workspace shell-init powershell | Out-String; Invoke-Expression $code
```

The generated `hcd` function changes to the active control root. `hproj`
changes to the registered subject root. Resolver failure prevents `cd`.
