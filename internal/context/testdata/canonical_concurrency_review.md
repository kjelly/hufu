# Canonical SQLite concurrency discovery — 2026-09-14

This is the WP-10 profiling record. It authorizes **no production read handle**.
`TestCanonicalConcurrencyDiscovery` builds a temporary 10K-row WAL database,
runs four readers plus one writer, and compares the current single handle with
test-only read pools of 1, 2, 4, and 8. Each scenario performs 100 canonical
queries and 25 appends; the whole matrix was repeated five times.

## Five-run medians

| Scenario | Read p95 | Write p95 | `database/sql` wait count | Aggregate wait duration | Busy retries |
| --- | ---: | ---: | ---: | ---: | ---: |
| current single handle | 106.64 ms | 102.82 ms | 122 | 4.925 s | 0 |
| separate read pool 1 | 101.84 ms | 3.16 ms | 98 | 3.557 s | 0 |
| separate read pool 2 | 64.48 ms | 2.15 ms | 95 | 1.683 s | 0 |
| separate read pool 4 | 59.82 ms | 3.94 ms | 0 | 0 | 0 |
| separate read pool 8 | 63.92 ms | 3.70 ms | 0 | 0 | 0 |

The pool-4 prototype improves read p95 by 43.9%; pool 8 is slower, supporting a
small bound if this design is revisited. Results and row counts stayed equal,
the single writer retained `foreign_keys=ON`, cancellation returned an error,
and no SQLite busy retry occurred.

## Gate decision

The current handle has meaningful aggregate pool waiting (about 40 ms per
reported wait at the median run), but `database/sql.DBStats` exposes only
aggregate wait duration/count—not per-operation p95 wait. The harness therefore
cannot prove the plan's first conjunct, “p95 repository wait >= 10 ms and >=20%
of operation latency,” separately from query execution. In addition, separating
the writer changes write p95 by far more than the gate's literal 5% invariance
bound, although the change is an improvement caused by removing in-process
serialization.

Because both conjuncts are not established exactly, WP-10 remains discovery
only. No second production handle, pool initialization, ownership change, or
connection-local foreign-key policy is introduced. A follow-up must first add
per-operation acquisition timing and then, only if the gate passes, write the
required design note covering snapshots, read-after-write, migration exclusion,
close ownership, connection initialization, and failure behavior.
