# SQLite analytics trend-query review

Phase 2 replaced the per-run execution, audit, and memory query loop with set-based collectors ordered by `selected_runs.ordinal`.

The statement-count regression test wraps the package-private analytics executor after run selection and task projection. It executes the full aggregation/report-query family for `runCount=1` and `runCount=100` and requires equal counts, without parsing SQL logs.

Observed statement counts were 21 for both scopes.

The 100,000-event / 1,000-run / `runCount=10` selected-scope benchmark after the change reported:

```text
7,288,985,219 ns/op total
40,803,777 ns/op aggregate_trend
577,173,168 B/op
11,369,990 allocs/op
```

The Phase 1 measurement on the same fixture was 12,926,079,127 ns/op total and 2,265,763,605 ns/op for `aggregate_trend`. These single-run measurements are supporting diagnostics; the five-sample index decision is recorded separately in `sqlite_index_review.md`.
