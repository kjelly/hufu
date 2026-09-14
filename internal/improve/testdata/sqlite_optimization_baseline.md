# SQLite analytics optimization baseline

Captured on 2026-09-14 before the selected-scope optimization phases.

Command:

```text
go test ./internal/improve -run '^$' -bench 'Benchmark(AnalyzeRecentSQLiteAnalytics|ExecutionTelemetryLegacyVsSQL)$' -benchmem -benchtime=1x -count=1
```

Environment: Linux amd64, Intel Core i7-6700K CPU @ 4.00GHz, Go package `github.com/kjelly/hufu/internal/improve`.

| Benchmark | ns/op | B/op | allocs/op |
|---|---:|---:|---:|
| AnalyzeRecentSQLiteAnalytics | 13,363,034 | 175,792 | 1,390 |
| ExecutionTelemetryLegacy n=10,000 | 259,744,629 | 80,195,400 | 555,538 |
| ExecutionTelemetrySQL n=10,000 | 886,744,041 | 51,792,432 | 993,247 |
| ExecutionTelemetryLegacy n=100,000 | 2,358,262,484 | 837,959,336 | 5,553,626 |
| ExecutionTelemetrySQL n=100,000 | 9,728,397,692 | 518,616,808 | 9,931,051 |
| ExecutionTelemetryLegacy n=1,000,000 | 23,081,789,916 | 8,302,171,992 | 55,536,207 |
| ExecutionTelemetrySQL n=1,000,000 | 100,953,525,651 | 5,224,704,368 | 99,311,508 |

`B/op` is allocation volume. These measurements do not establish peak heap or RSS behavior.
