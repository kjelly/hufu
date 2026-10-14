# Legacy Chromem Memory Store Deprecation Record

> Status: deprecated, compatibility retained
> Authority: normative
> Verified-Commit: — (working-tree change; tests are authoritative)
> Supersedes: —
> Superseded-By: —
> Scope: legacy `internal/memory` chromem store, `hufu context migrate-memory`, `--memory`, `--memory-model`, `embedding-model`
> Canonical architecture: [context SQLite schema](../reference/context-sqlite-schema.md), [semantic retrieval](../reference/semantic-retrieval.md)
> Warning-Introduced-In: the first release after v2.1.0
> Earliest-Removal: blocked until one later published release has also carried the warning

## Decision

Canonical memory lives in `<workspace>/context.sqlite`, and the semantic lane
reads embedding generations stored in the same database. The chromem-based
long-term memory store under `~/.local/share/hufu/memory/<projectHash>/` is no
longer written or read by runs, and the chromem index that `hufu context
query` used is already removed.

What remains is deprecated:

- `hufu context migrate-memory` still imports a legacy store into canonical
  context. It is the only code that reads the chromem store and prints
  `Command "migrate-memory" is deprecated` when it runs.
- `--memory` (root and `hufu chat`) and `--memory-model` are accepted but have
  no effect; cobra prints a deprecation notice when they are used.
- The `embedding-model` setting in hufu.yaml has no effect; `hufu doctor`
  warns when it is set (`legacy_embedding_model_set`).

## Migration

Import any remaining legacy store before the removal release:

```bash
hufu context migrate-memory --workspace <workspace> --project <project> --legacy-project <old-project-path>
hufu context migrate-memory --workspace <workspace> --project <project> --legacy-project <old-project-path> --apply
```

The first command is a count/checksum dry run; `--apply` backs up the
destination database before writing. Afterwards, drop `--memory` and
`--memory-model` from scripts and `embedding-model` from hufu.yaml. Semantic
retrieval is configured only by the `semantic-retrieval` block.

## Removal gate

After one later published release has carried these warnings, removal is a
separate change that:

1. deletes `hufu context migrate-memory`, `internal/memory`, the
   `--memory`/`--memory-model` flags, the `embedding-model` setting and its
   doctor check;
2. removes `github.com/philippgille/chromem-go` from `go.mod`/`go.sum` and its
   entry in `NOTICE`;
3. announces the breaking removal in the release notes.
