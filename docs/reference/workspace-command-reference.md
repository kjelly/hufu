# Workspace command reference

> Status: active
> Authority: reference
> Verified-Commit: 2026-09-17
> Supersedes: —
> Superseded-By: —

`hufu workspace` manages project registration and durable team control roots.
Selectors accept a full project ID, a unique ID prefix of at least eight hex
characters, an exact alias, an exact subject path, or an unambiguous slug.
Omitting a selector resolves the discovered subject root of the current
directory.

| Command | Behavior |
|---|---|
| `register [path]` | Register a subject root without creating a team workspace. |
| `list [--all]` | List projects; `--all` includes trash and incomplete operations. |
| `show [selector]` | Show one project and its workspace rows. |
| `path [selector] [--team name]` | Print only the active canonical control root. |
| `subject-path [selector]` | Print only the canonical subject root. |
| `alias set/clear` | Manage a normalized project alias. |
| `rebind <selector> <new-root>` | Change only the subject path; workspace identities and control roots remain stable and the next execution requires `--new`. |
| `migrate [selector]` | Non-destructively import one legacy team or `--all-teams`. |
| `delete [selector]` | Move one team or `--all-teams` to managed trash. |
| `restore <trash-id>` | Restore to the exact original control root without overwrite. |
| `purge <trash-id> --yes` | Permanently remove one validated trash entry. |
| `gc` | Preview expired trash; `--apply --yes` performs purge. |
| `doctor [selector]` | Report typed consistency issues; `--repair` performs deterministic recovery. |
| `shell-init <shell>` | Print static navigation helpers for bash, zsh, fish, or PowerShell. |

Mutation commands accept `--output text|json`. JSON uses a stable envelope
with `schema_version`, `data`, and `warnings`. Once a filesystem lifecycle
operation has created its operation row, even an interrupted command emits an
`outcome` of `complete`, `partial`, or `failed`; partial and failed outcomes
also return non-zero. Preflight failures write no partial JSON.

`delete`, `restore`, and `purge` never operate on unmanaged paths, subject
roots, state-root ancestors, top-level symlinks, or marker mismatches. In a
non-interactive process, delete and restore require `--yes`; purge always
requires explicit `--yes`. GC defaults to a read-only preview and only purges
trash entries older than its parsed retention duration.
