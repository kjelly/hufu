# SQLite maintenance policy

> Status: draft
> Authority: normative
> Verified-Commit: 2026-09-14
> Supersedes: —
> Superseded-By: —

## Decision status

This is the WP-11B discovery record. It does not authorize or expose a
maintenance command. Normal runtime and every `hufu inspect` path must not run
`PRAGMA optimize`, a checkpoint, or `VACUUM` as maintenance.

## Measurements

`TestCanonicalMaintenanceDiscovery` created a 10,000-row canonical/FTS fixture
five times without running maintenance SQL. Every run produced the same facts:

| State | Page count | Freelist | Page size | Database | WAL | Journal | Autocheckpoint |
| --- | ---: | ---: | ---: | ---: | ---: | --- | ---: |
| writer active | 1,604 | 0 | 4,096 B | 6,569,984 B | 6,727,992 B | WAL | 1,000 pages |
| all writers closed | 1,604 | 0 | 4,096 B | 6,569,984 B | 0 B | WAL | 1,000 pages |

The representative WAL is transient and disappears on orderly close; the
fixture has no freelist pressure. This is not evidence for automatic or
operator-triggered maintenance.

## Proposed operator contract (not implemented)

If field evidence later passes the gate, a separate design may propose an
explicit command shaped like:

```text
hufu context maintain --workspace PATH --operation optimize|checkpoint|vacuum --apply
```

The contract would require one named operation, an explicit `--apply`, no agent
or provider execution, an exclusive canonical-store lease, a preflight report,
and machine-readable before/after storage diagnostics. It must reject active
runs and unknown schema versions. `inspect storage` remains strictly read-only
and cannot become an alias for this operation.

Before any implementation is authorized, that design must define:

- a verified backup containing the database and consistent WAL state before a
  write-capable operation, plus checksum and restore instructions;
- recovery that preserves the failed database and restores to a new path before
  atomic replacement, never deleting the only copy;
- context cancellation before acquisition and between SQLite operations;
  interruption of a single SQLite statement relies on SQLite atomicity and must
  still be verified by integrity/open/replay checks;
- free-space preflight using operation-specific worst cases—at least database +
  WAL + backup for optimize/checkpoint, and at least two database images + WAL +
  backup for vacuum/rewrite—with a safety margin;
- crash/kill tests at every externally visible phase, concurrent-reader/writer
  rejection tests, disk-full simulation, cancellation, backup corruption,
  restore verification, FTS parity, migration checksum validation, and repeated
  idempotent recovery.

## Gate result

The measured fixture shows neither persistent WAL growth nor reclaimable pages,
and the required interruption/recovery evidence does not yet exist. WP-11B is
closed as “not authorized”; no maintenance implementation or write-capable
inspect behavior is included.
