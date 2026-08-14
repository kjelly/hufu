# Context management Phase 1 implementation report (superseded)

> Status: historical record. HF-MEM3 replaced Phase 1's shadow-write strategy
> with canonical SQLite read/write and prompt assembly. `context-stm.md` and
> `context-ltm.md` remain debug projections only; they are never runtime inputs.

## Scope

Phase 1 establishes a durable canonical context store without changing the
legacy prompt path. The implementation is intentionally shadow-compatible:
SQLite commits and side-by-side projections are observable, while existing
STM/LTM prompt assembly remains the runtime authority during this phase.

## Delivered components

| Area | Implementation | Acceptance evidence |
| --- | --- | --- |
| Domain model | `internal/context/model.go` defines context kind, authority, trust, priority, scope, provenance, evidence, validity, and embedding fields. | Repository persistence and retrieval tests exercise scoped `ContextItem` round trips. |
| SQLite repository | `SQLiteRepository` uses WAL, busy retry, migration checksums, backup-before-migration, transactional append/deduplication, edges, expiry, and revision events. | Migration idempotence, checksum rejection, backup, expiry, and revision tests. |
| Projection | `RebuildProjection` generates `context-stm.md` and `context-ltm.md` from canonical rows without overwriting legacy `stm.md`/`ltm-*.md`. | `TestSQLiteRepositoryRebuildProjectionLeavesLegacyMemoryFilesUntouched`. |
| Redaction | Canonical content and pending-write errors are redacted before persistence or event logging. | SQLite/projection and shadow-write redaction tests. |
| Shadow writes | Legacy tool paths can append a canonical shadow record; canonical-store failure is queued in redacted pending JSONL for idempotent repair. | Shadow append, failure, repair, and no-legacy-prompt-path tests. |

## Legacy prompt compatibility

The shadow compiler records only diagnostics (`context-shadow-traces.jsonl`):
fingerprints, token counts, selection counts, duplicate ratio, and missing
anchors. It deliberately excludes prompt content. `compileShadowCoordinator`
and `compileShadowWorker` call the canonical compiler only to record those
diagnostics; they do not mutate the legacy prompt string passed to the model.

Regression coverage includes deterministic shadow compilation and bounded
context selection. The five-session golden fixture regression
(`TestShadowSessionFixturesProduceComparableBundlesWithoutMutatingLegacyPrompt`)
snapshots every legacy prompt before canonical shadow compilation and asserts
that it remains byte-identical afterwards, while recording a canonical bundle
fingerprint and token-count comparison for each coordinator/worker case.
During Phase 1, a canonical-store open/write failure is
non-fatal: legacy tool and prompt paths continue, while pending writes remain
repairable.

## Verification commands

```bash
go test ./internal/context ./internal/team
go vet ./...
```

Schema detail is documented separately in
[`context-sqlite-schema.md`](context-sqlite-schema.md).
