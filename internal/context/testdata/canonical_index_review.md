# Canonical SQLite index discovery — 2026-09-14

This is the WP-9 discovery record. It authorizes **no schema change**. The
reproducible harness is `TestCanonicalIndexDiscovery` and
`BenchmarkCanonicalIndexDiscovery`; candidate indexes exist only in each
temporary test database. Released migrations 1–8 are unchanged.

## Fixture and method

- predecessor: `bd3cdac`
- host: linux/amd64, Intel i7-6700K
- fixture: 10,000 context and FTS rows in one project; 100 candidate rows and
  100 distinct `run_id` values; 1,000 outcome, experience, and promotion rows;
  1,000 policy versions with one active version
- read timing: `go test -bench ... -benchtime=10x -count=5`, compared with
  `golang.org/x/perf/cmd/benchstat` (n=5)
- write timing: five 20-item context append transactions, rolled back after
  exercising context, FTS, event, and activation-index writes
- size: `(page_count - freelist_count) * page_size`, measured before the write
  samples; all selectivity values below are fixture values

The five-sample benchstat run satisfies the plan's minimum sample count. At
n=5, benchstat can report rank-test p-values but not a finite 95% confidence
interval; the harness remains available for longer follow-up runs.

## Review table

Plans are abbreviated to their material access path. Latencies are benchstat
medians; `~` means benchstat found no significant difference.

| Query / caller | Cardinality and selectivity | Current index and plan | Candidate and plan after | Read before → after | Write / size effect | Decision |
| --- | --- | --- | --- | --- | --- | --- |
| canonical `Query`, project + lifecycle + top 20 | 10K; 99% lifecycle match; returns 0.2% | `idx_context_created`; project search + temp order | no dedicated candidate; origin-run candidate is covering by accident but still sorts | 11.856 ms baseline; 4.534 ms with unrelated origin index (-61.75%) | origin candidate below | retain; do not pay for an index whose leading semantic purpose is another caller |
| FTS search | 10K; returns top 20 | FTS5 virtual index `M4` | none | 201.9 µs | none | retain FTS5 |
| candidate duplicate lookup | one exact content hash | `idx_context_hash(project_id,content_hash)` | wide expression dedupe index; all equality terms used | 137.22 → 87.70 µs (-36.09%) | context append 9.174 → 9.440 ms (+2.90%); +12.34% database bytes | reject: large storage cost for a sub-millisecond lookup |
| append dedupe | one exact content hash | `idx_context_hash` | same wide expression index | 100.53 → 97.14 µs (`~`) | same as above | reject: no repeatable hot-query benefit |
| reducer dedupe | one exact hash plus run/task/attempt JSON | `idx_context_hash` then residual JSON | wide expression dedupe index; typed scope terms used, JSON remains residual | 123.2 → 155.1 µs (`~`) | same as above | reject |
| run-scoped candidate selection | 10K; one run is 1%, candidates are 1%; returns one row | `idx_context_created`; project scan + temp order | `(project_id,json_extract(run_id),lifecycle,priority,created_at,id)` covering search | 18.686 ms → 240.4 µs (-98.71%) | context append showed no regression; +7.42% database bytes; reducer dedupe 123.2 → 296.3 µs (+140.62%) | reject: material storage growth and a measured regression in another canonical lookup; Phase 4 already reduced the 100K caller to 96.53 ms without it |
| context outcome lookup | 1K; one exact six-dimension key | covering `idx_context_outcome_dimensions` | none | 63.99 µs | none | retain |
| experience aggregate list | 1K; one policy returns all rows | primary key `(context_item_id,policy_version)` scan | `(policy_version,context_item_id)` covering search | 539.2 → 603.8 µs (`~`) | +0.50% database bytes | reject: no improvement |
| promotion proposal list | 1K; one project/team returns all rows | `idx_promotion_scope_status`; temp order | `(project_id,team_id,created_at DESC,id)` covering search | 1.600 → 1.097 ms (-31.42%) | +0.69% database bytes; context append structurally unaffected | reject: only 0.503 ms absolute gain on an operator/review path, before source hydration |
| promotion status list | 1K proposed rows | `idx_promotion_scope_status`; only final ID order is temporary | promotion-order candidate loses status prefix | 1.503 → 1.439 ms (`~`) | same as above | retain existing status index |
| active memory-policy lookup | 1K; one active row (0.1%) | table scan + temp order | `(status,adopted_at DESC)` | 287.35 → 61.95 µs (-78.44%) | +0.37% database bytes; context append structurally unaffected | reject: 0.225 ms absolute gain on a cold policy-load path does not justify another maintained index |

## Decision

No candidate meets the total-runtime gate. Existing hash, outcome, FTS, scope,
promotion-status, and primary-key indexes remain. No index is added or removed,
and no migration, checksum, backup path, or open behavior changes.
