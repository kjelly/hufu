# SQLite analytics ingestion batch review

Captured on 2026-09-14 on Linux amd64, Intel Core i7-6700K CPU @ 4.00GHz.

All loaders use the SQLite `MAX_VARIABLE_NUMBER` compile option, falling back to 999 when absent. Batch rows are capped at `maxVariables / columnCount`, and execution skills use an independent inserter because their column count and cardinality differ.

## Execution ingestion

Command:

```text
go test ./internal/improve -run '^$' -bench '^BenchmarkSQLiteAnalyticsExecutionBatchSizes$' -benchmem -benchtime=1x -count=5
```

The 100,000-event matrix evaluated batch sizes 1, 8, 16, 32, 64, 128, and 256. Batch 32 cleared the large-profile execution-load threshold:

```text
                                   │ batch=1 │ batch=32 │
sec/op                                5.969      4.998     -16.27% (p=0.008 n=5)
B/op                               544.2 MiB  480.2 MiB   -11.75% (p=0.008 n=5)
allocs/op                           11.202 M    7.567 M    -32.45% (p=0.008 n=5)
```

Batch 8 improved the median by about 9.6% and batch 16 by about 14.6%, so neither cleared the 15% execution-load threshold. Batch 64 had a 6.51s median, batch 128 a 9.79s median, and batch 256 a 16.00s median, so those candidates were also rejected.

The candidate that cleared the large-profile gate failed the tiny-profile safeguard. A separate ten-sample run compared batch 1 and batch 32 on 100 events:

```text
batch=1  median 6.18ms
batch=32 median 7.62ms (+23%)
```

The regression was substantially above the allowed tiny-dataset tolerance. Because no fixed candidate passed both the large and tiny gates, production retains candidate 1 (the single-row prepared-statement path).

## Audit and memory ingestion

The independent five-sample matrices used 5,000 audit rows and 10,000 validated memory events. Batch 32 had the best median among candidates without an allocation regression:

```text
audit:  batch=1 50.97ms -> batch=32 38.48ms
memory: batch=1 403.89ms -> batch=32 342.19ms
```

These component results do not override the merge gate's tiny-profile safeguard. Production therefore keeps candidate 1 for execution, execution skills, audit, and memory. The bounded multi-row implementation and benchmarks remain available for reevaluation; every configured candidate is independently capped by its column count and the discovered SQLite variable limit.
