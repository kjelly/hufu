# SQLite analytics index review

Captured on 2026-09-14 on Linux amd64, Intel Core i7-6700K CPU @ 4.00GHz.

## Consumers

| Index | Consumer after selected-run scoping | Decision |
|---|---|---|
| `idx_execution_team_run` | run-summary selection | retain |
| `idx_execution_task_attempt` | selected task projection | replace |
| `idx_execution_agent` | none; grouping uses `task_summary` | remove |
| `idx_execution_model` | none; grouping uses `task_summary` | remove |
| `idx_execution_task_type` | none; grouping uses `task_summary` | remove |
| `idx_skill_task` | none; projection joins the existing `(event_seq, skill)` primary key | remove |
| `idx_audit_team_time` | overall and per-run audit windows | retain |
| `idx_memory_type_run` | global memory summary, evidence, and retry classification | retain |
| `idx_execution_run_task_seq` | selected task projection and event-level token grouping | add |

`EXPLAIN QUERY PLAN` coverage asserts that the selected run/task/order query uses `idx_execution_run_task_seq`; it intentionally does not freeze complete planner output.

## Benchmark decision

Command:

```text
go test ./internal/improve -run '^$' -bench '^BenchmarkSQLiteAnalyticsIndexSets$' -benchmem -benchtime=1x -count=5
```

The benchmark executes load, index build, selected-run materialization, execution/revision/memory/trend aggregation, and grouped aggregation for 100,000 execution events and `runCount=10`.

Benchstat comparison of the five samples:

```text
                         │ legacy/current │ run-task/current │
                         │     sec/op     │      sec/op      │
SQLiteAnalyticsIndexSets      8.409             7.378          -12.26% (p=0.008 n=5)

B/op                         550.0 MiB          550.0 MiB       no significant difference
allocs/op                     11.37 M            11.37 M        no significant difference
```

The replacement exceeds the plan's 5% total-pipeline threshold.

Tiny-profile five-sample benchstat medians were 30.77 ms for the legacy set and 28.86 ms for the run/task set; the timing difference was not statistically significant (`p=0.841`), allocations improved 0.40%, and no regression above 5% was observed.
