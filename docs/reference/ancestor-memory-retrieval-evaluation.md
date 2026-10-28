# Ancestor memory retrieval evaluation

> Status: active
> Authority: reference
> Verified-Commit: 825ff686
> Supersedes: —
> Superseded-By: —

## Scope and result

These 2026-10-28 synthetic measurements compare committed baseline `825ff686`
with the source hashes in the [machine-readable report](ancestor-memory-retrieval-evaluation.json).
All 16 cases returned the same ordered IDs in both revisions. All eight
1K/10K latency and allocated-byte median gates passed the maximum 1.2 ratio.
The [lineage scope architecture](../architecture/hybrid-retrieval-lineage-scope.md)
defines the authorization and compatibility boundaries.

The 100,010-record worker behavior test separately checks exhaustive authorized
oracle equivalence when more than 100,000 post-fork records would crowd out
old legal memories. Benchmarks are not used as a substitute for that test.
Full repository, race, vet, and lint gates are recorded separately below.

## Method

- Linux amd64, Intel Core i7-6700K; mise Go 1.27.2, GOMAXPROCS=2, Go -p 2.
- Fixed sequential IDs; worker normalization uses frozen Unix time 200000 so
  insertion speed cannot change tie-breaking. No real provider or user data.
- Five warm serial samples per case. Small fixtures use 100 operations/sample;
  100K+ fixtures use five. Go serializes benchmark packages despite build -p 2.
- Source fixture allows four IDs, returning four matches. Worker fixture allows
  eight of sixteen matches, returning eight. Other rows are background noise.
- Both revisions use the same benchmark harness. Its SQLite driver wrapper
  counts actual canonical content rows read through the driver; embedding and
  metadata projection rows are excluded. Worker source counts include exact
  and lexical responses, so one item may contribute twice.
- Temporary baseline harness failures (embedding batch limit and missing
  read-only transaction forwarding) were corrected before using source-lane
  results. The successful worker measurements from the initial combined run
  were retained; that command's overall failure is not represented as a pass.

From the repository root, with existing caches/GOTMPDIR preserved and one private
`TMPDIR`, run each command sequentially. For baseline, insert
`-C <task>/baseline-current` after `go`, using an archive of `825ff686` with the
two benchmark test harness files added. No tracked baseline source is changed.

```bash
GOMAXPROCS=2 mise exec -- go test -p 2 ./internal/context ./internal/team -run '^$' -bench 'Benchmark(LineageRetrieval|WorkerAncestorRecall)/rows=(1000|10000)$' -benchtime=100x -count=1 -timeout 30m
GOMAXPROCS=2 mise exec -- go test -p 2 ./internal/context ./internal/team -run '^$' -bench 'Benchmark(LineageRetrieval|WorkerAncestorRecall)/rows=(100000|100002)$' -benchtime=5x -count=1 -timeout 30m
```

Raw sample values, sample iteration counts, ordered IDs and source hashes are
preserved in the JSON report. Baseline large source lanes passed in 145.423s;
baseline large worker data comes from its passing 121.179s package run. Final
small and large working-tree package runs both exited successfully.

## Median cost

Time is milliseconds/op; allocation is KiB/op. Each cell is baseline → new.
Rows are canonical driver rows for source lanes and returned source rows for
worker recall, as defined above.

| Corpus rows | Lane | Time ms | Allocation KiB | Rows |
|---|---|---|---|---|
| 1,000 | exact | 1.305 → 0.475 | 18.0 → 18.0 | 4 → 4 |
| 1,000 | fts | 0.763 → 0.606 | 23.4 → 23.4 | 4 → 4 |
| 1,000 | vector | 12.767 → 0.482 | 2055.7 → 28.2 | 1000 → 4 |
| 1,000 | worker | 3.437 → 1.812 | 298.1 → 147.4 | 32 → 16 |
| 10,000 | exact | 12.710 → 0.555 | 18.0 → 18.0 | 4 → 4 |
| 10,000 | fts | 0.814 → 0.489 | 23.4 → 23.4 | 4 → 4 |
| 10,000 | vector | 138.113 → 0.517 | 20422.3 → 28.2 | 10000 → 4 |
| 10,000 | worker | 14.011 → 1.654 | 297.2 → 146.9 | 32 → 16 |
| 100,000 | exact | 128.663 → 0.853 | 18.0 → 18.0 | 4 → 4 |
| 100,000 | fts | 0.735 → 1.400 | 23.3 → 23.4 | 4 → 4 |
| 100,000 | vector | 1348.896 → 0.542 | 203947.5 → 28.2 | 100000 → 4 |
| 100,000 | worker | 142.721 → 2.412 | 294.1 → 147.1 | 32 → 16 |
| 100,002 | exact | 140.284 → 0.434 | 18.0 → 17.9 | 4 → 4 |
| 100,002 | fts | 0.890 → 0.635 | 23.4 → 23.4 | 4 → 4 |
| 100,002 | vector | 1351.053 → 0.547 | 203952.8 → 28.2 | 100002 → 4 |
| 100,002 | worker | 165.250 → 1.686 | 294.1 → 145.9 | 32 → 16 |

The 100K FTS case recorded a higher median (0.735 → 1.400ms), while the
100,002-row FTS case was lower (0.890 → 0.635ms). Those five-operation samples
are disclosed as costs, without claiming a uniform FTS speedup. The plan's
20% regression gate applies to 1K/10K fixtures; large fixtures must pass
completeness and resource checks.

## Root fix and limits

The initial JSON predicate could leave exact search scanning the project
scope. A controlled 100-operation probe changed only the primary-key hint:
10K exact search fell from 12.570ms to 0.368ms. Explicit-ID canonical queries
now use the existing primary-key index. Restricted FTS reuses the fixed join
path and filters the outer FTS ID before canonical lookup. Neither change
removes scope, validity, lifecycle, kind or confidence checks.

Initial five-operation small samples showed a regression. After diagnosing
and fixing the query paths, both baseline and new small fixtures were measured
with 100 operations/sample; the gate uses that matched protocol, including
all cases rather than selecting favorable samples.

The full allowed set is retrieved before worker ranking. Large authorized
matching sets can still be expensive in existing MMR; these measurements use
small authorized sets and do not establish its worst-case scalability.
The active branch's existing 100,000 allowance remains unchanged.

All jobs were serialized with other heavyweight validation. Recorded checks
showed over 6GiB free in /tmp and over 9GiB available RAM; the private task
scratch retained about 110MiB. Configured Go caches and GOTMPDIR were preserved.
This is a local synthetic regression gate, not a production SLO.

## Repository validation

The final tuned-source full working-tree suite passed: `cmd/hufu` 158.332s,
`internal/context` 72.495s, `internal/team` 656.382s; all other packages passed
or had no tests. Targeted race passed (`context` 308.485s, `team` 1018.747s,
`cmd/hufu` 12.875s). `mise run vet` and mandatory `mise run lint` passed
(exit 0; lint reported 0 issues). The sandbox prevented lint cache persistence,
which produced non-fatal warnings without disabling analysis or changing caches. The working tree also contains unrelated
pre-existing edits, excluded from phase staging. The immutable baseline result
is recorded in the [baseline report](runtime-continuation-baseline.md).
