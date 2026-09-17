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
