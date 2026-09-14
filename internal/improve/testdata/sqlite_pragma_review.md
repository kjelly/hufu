# SQLite analytics PRAGMA review

Captured on 2026-09-14 on Linux amd64, Intel Core i7-6700K CPU @ 4.00GHz.

The 100,000-event / `runCount=10` pipeline benchmark evaluated SQLite defaults and each disposable-database candidate independently, followed by the combined profile:

```text
default         median 6.845s
temp_store=MEMORY      6.575s
synchronous=OFF        6.773s
journal_mode=MEMORY    6.763s
combined               6.616s
```

Five-sample benchstat for the best individual candidate (`temp_store=MEMORY`) reported -3.95% (`p=0.008`) with no significant B/op or allocation change. That does not meet the plan's 5% end-to-end threshold. No candidate was retained, so `configureAnalyticsSQLite` intentionally preserves SQLite defaults.

The benchmark remains available as `BenchmarkSQLiteAnalyticsPragmaProfiles`. Tests assert the empty retained profile and prove that opening/configuring the disposable analytics database does not change canonical `context.sqlite` bytes.
