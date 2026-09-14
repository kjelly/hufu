# Hufu SQLite Optimization Plan

> Status: Historical — implemented
> Target: `kjelly/hufu`
> Baseline reviewed: `main` at commit `436d13f` (2026-09-14); `internal/improve` and `internal/context` are unchanged from `0a68f3e1547fbb25c4ae78a2fd9f091934a13b11`
> Completed: 2026-09-14 at commit `1fd79a9`
> Primary goal: improve runtime scalability and `hufu improve` analytics performance **without introducing DuckDB or replacing SQLite as canonical storage**.

## Implementation authorization

This document authorizes implementation in the phased PR order in section 13.

- PR 1 through PR 4 are implementation-ready and may begin in order.
- PR 5 through PR 7 are benchmark experiments; retain their code changes only when their stated merge gates pass.
- PR 8A and PR 8B implement the specified context query work. PR 8C requires its discovery deliverable before code changes.
- WP-9, WP-10, and WP-11B are discovery-gated. Profiling/query-plan evidence must first produce the concrete decision record required by each work package; they do not authorize speculative production changes. WP-11A read-only storage diagnostics is implementation-ready.
- A later PR must not start merely because an earlier PR was opened. Its predecessor exit criteria must be recorded and passing on the same branch baseline.

## Implementation outcome

The phased implementation completed on 2026-09-14:

| Phase | Commit | Outcome |
| --- | --- | --- |
| 0 — baseline and parity | `0ba986e` | Added semantic goldens, selected-scope benchmark seams, query-count observability, and opt-in stress coverage. |
| 1 — selected scope | `5891b0d` | Moved recent-run selection into SQLite, introduced relational `selected_runs`, and limited task projection to selected runs. |
| 2 — set-based aggregation | `78a54f4` | Replaced per-run trend queries with fixed query families while preserving report semantics. |
| 3 — ingestion experiments | `7dcd068` | Retained reproducible batch/PRAGMA evidence; production kept single-row batching and existing PRAGMAs because the full gates did not pass. |
| 4 — typed context filtering | `bd3cdac` | Added typed repository predicates and ordered streaming, converted candidate settlement and true-bulk callers, and recorded the deferred lineage constraint. |
| 5 — canonical discovery and inspection | `1fd79a9` | Added read-only storage inspection and recorded WP-9, WP-10, and WP-11B discovery decisions. |

The discovery-gated outcomes are deliberately conservative:

- WP-8C remains deferred in `docs/architecture/hybrid-retrieval-lineage-scope.md`; the explicit worker-lineage 100K ceiling remains classified risk.
- WP-9 adds no migration: no candidate index met the total-runtime/read/write/size gate.
- WP-10 adds no production read handle: the prototype showed contention, but the specified per-operation p95 wait and write-invariance conjuncts were not both established.
- WP-11A is implemented as `hufu inspect storage`; WP-11B authorizes no maintenance command because persistent WAL/freelist pressure and interruption-safe recovery evidence were absent.

The final production `100000` inventory is classified rather than hidden:

- shared-memory confirm/reject, worker-memory confirm/reject, run-shared candidate lookup, and current-run prompt candidates are typed-filter paths with lifecycle/origin/source predicates pushed into SQLite;
- context history is the explicitly preserved authorized bulk snapshot from WP-8B;
- worker ancestor-lineage retrieval is the WP-8C deferred ranked-search risk;
- `autoExtractCanonicalLTM` is the explicitly deferred mixed confirmed/current-run candidate rule from WP-8A and retains its post-query semantic checks.

Every code phase passed `go test ./...`, `go vet ./...`, and `golangci-lint run`. SQLite remains canonical, no DuckDB dependency was added, and released migrations 1–8 remain immutable.

---

## 1. Executive decision

Hufu should continue to use SQLite as the canonical transactional store and as the embedded analytics engine for the current workload.

The optimization strategy is:

```text
Canonical runtime state
        │
        ▼
SQLite WAL
transactional / durable / authoritative
        │
        ├── context / memory / promotion / outcome
        │
        └── FTS5 / typed projections
                    │
                    ▼
            runtime retrieval
```

```text
execution-events.jsonl ─┐
audit-*.jsonl           ├── streaming / validated ingestion
event_store.jsonl       ┘
                         │
                         ▼
                 SQLite :memory:
                         │
                 selected_runs
                         │
              selected-scope projection
                         │
             set-based aggregation
                         │
                         ▼
                  improve Report
```

Do **not** introduce DuckDB in this plan.

The highest-value improvements are:

1. eliminate N+1 analytics queries;
2. push run selection and filtering into SQLite;
3. materialize task projections only for selected runs;
4. replace repeated `IN (?, ?, ...)` generation with a TEMP `selected_runs` relation;
5. benchmark-gate batch ingestion and SQLite PRAGMA tuning;
6. remove runtime `Limit: 100000` + Go-side filtering where SQL-side typed predicates can do the work;
7. only pursue concurrent SQLite read handles if profiling proves the current single-connection repository is a bottleneck.

---

# 2. Current architecture constraints

The implementation must preserve these properties.

## 2.1 SQLite remains canonical

`context.sqlite` remains authoritative for:

- context items;
- context edges;
- context events;
- experience aggregates;
- memory policy versions;
- consolidation proposals;
- promotion proposals;
- promotion outbox;
- typed context activation;
- context outcome observations.

Do not create another canonical database.

## 2.2 Analytics remains derived and disposable

The `internal/improve` analytics database is currently:

- `:memory:`;
- connection-scoped;
- recreated for every analysis;
- unable to mutate canonical storage.

Keep this property.

Do not add a persistent global analytics DB in this work.

## 2.3 Durable telemetry semantics must not change

Preserve:

- tolerant execution/audit JSONL parsing behavior;
- deterministic event order using `event_seq`;
- run selection ordering semantics;
- last-non-empty task field semantics;
- last-reported skill overwrite semantics;
- memory event-store hash-chain validation;
- current global-vs-selected memory metric semantics;
- current report JSON shape;
- current grouped metric fallback labels;
- current audit inclusive time-window behavior.

Performance work must not silently become semantic work.

### 2.3.1 Exact run-selection compatibility contract

- Ignore execution rows whose `run_id` or `team` is empty when forming runs.
- Resolve a run's team from its first qualifying row by `event_seq`.
- Compute `start_ns` / `end_ns` from parseable timestamps only.
- Sort runs oldest-to-newest by `end_ns ASC, run_id ASC`, with NULL `end_ns` before non-NULL values.
- When no team is requested, choose the team of the globally last run in that ordering, then choose the latest `runCount` runs belonging to that team.
- Return selected runs oldest-to-newest.
- A run whose later rows name another non-empty team still belongs to its first resolved team; its execution window and task metrics continue to include all qualifying rows for the selected `run_id`, matching current behavior.

### 2.3.2 Exact team-revision compatibility contract

Two different report fields intentionally have different semantics:

- `Report.TeamRevisions` is the lexically sorted distinct set of **every** non-empty `team_revision` observed in selected execution rows.
- `TrendPoint.TeamRevision` is the last non-empty `team_revision` for that run by `event_seq`.
- `definitionRevision(teamDir)` remains the fallback only when the overall distinct set is empty. It does not populate an otherwise-empty per-run trend revision.

The set-based redesign must compute both projections; latest-per-run values cannot be reused as the overall distinct set.

### 2.3.3 Exact memory-metric compatibility contract

The existing formulas are frozen for this optimization even where their scope is unusual:

- Global over the complete validated memory event stream: retrieval count, exposure count, applied count, stale count, verified count, harmful count, retrieved-memory token count, and distinct applied `(run_id, task_id)` count.
- Scoped to the requested run set: execution input-token sum and retried execution-task pairs.
- Assisted/unassisted retry numerators are computed from scoped retry pairs joined to the global applied-pair relation on `(run_id, task_id)`.
- `MemoryTokenOverhead = globalMemoryTokens / scopedInputTokens` when the denominator is positive.
- `MemoryAssistedRetryRate = scopedAssistedRetries / globalAppliedTaskCount` when the denominator is positive.
- `unassistedTotal = scopedTotalTasks - globalAppliedTaskCount`; compute `MemoryUnassistedRetryRate` only when `unassistedTotal > 0`, otherwise leave it zero.
- Every trend point repeats the same global scalar counters and global applied-task denominator, while using that run's input tokens, total tasks, and retry pairs.

Changing these formulas is a separate metric-spec/product change and is forbidden in this performance program.

### 2.3.4 Exact audit-loader compatibility contract

- Files are processed in `filepath.Glob` order.
- Unopenable audit files are skipped.
- Malformed JSON rows are skipped.
- Valid JSON with an invalid timestamp is loaded but cannot match a time window.
- A scanner error ends the affected file, preserves its successfully scanned prefix, and processing continues with later files; the transaction commits all retained rows.
- Audit windows are inclusive at both ends. One audit row may contribute to multiple overlapping run windows.

### 2.3.5 Exact repository-query compatibility contract

- With no explicit lifecycle predicate, `IncludeCandidates=false` means `lifecycle='confirmed'` and `IncludeCandidates=true` means no lifecycle filter at all.
- `IncludeExpired` controls only `expires_at`; `valid_from` and `valid_until` are always enforced. Preserve the current evaluation order: when expiry filtering is enabled, capture its `time.Now()` value first, then capture the validity-window `time.Now()` value.
- Ordinary query order is `priority DESC, created_at DESC, id ASC`.
- Existing scope/visibility authorization remains authoritative and must be compiled before optimization predicates.

## 2.4 Security boundaries must remain

Do not persist into analytics tables:

- prompts;
- outputs;
- arbitrary tool arguments;
- arbitrary audit payload;
- raw memory content.

Continue to project only fields needed by metrics.

---

# 3. Problems to solve

## P1. `AnalyzeRecent` reloads and indexes history on every invocation

Current flow:

```text
open :memory:
→ scan execution events
→ scan audit logs
→ validate/scan memory event store
→ insert rows
→ create indexes
→ select recent runs
→ aggregate
```

This is acceptable for small histories but becomes expensive as telemetry grows.

The first optimization should be **scope reduction**, not a database replacement.

---

## P2. Run selection materializes all run summaries into Go

Current run selection builds all run summaries, returns them to Go, filters by team, then keeps the last `runCount`.

Desired behavior:

```text
SQLite determines target team
→ SQLite selects only N recent runs
→ selected runs are materialized once
```

Do not allocate every run summary in Go when only N runs are required.

---

## P3. Dynamic `run_id IN (?, ?, ...)` is repeated across analytics

Current analytics repeatedly creates dynamic `IN` clauses.

Problems:

- repeated SQL construction;
- parameter-count growth;
- repeated binding;
- inconsistent query shapes;
- harder query-plan inspection;
- unnecessary plumbing between Go and SQLite.

Use a TEMP relational scope instead.

---

## P4. `task_summary` and `task_skills` are materialized for all loaded runs

The report only needs selected runs, but current task projection is generated over all task-bearing events.

For large histories this performs unnecessary:

- window functions;
- grouping;
- joins;
- TEMP writes.

Projection must be selected-run scoped.

---

## P5. Trend generation is N+1

Current code effectively performs, for each selected run:

```text
execution metrics query set
audit metrics query set
memory metrics query set
team revision lookup
```

For N runs, query count grows approximately with N.

Trend should be a set-based operation over all selected runs.

---

## P6. Several runtime paths request up to 100,000 context rows

Current repository API supports mainly:

- scope;
- visibility;
- kinds;
- lifecycle inclusion flags;
- limit.

Multiple callers use `Limit: 100000`, then perform additional filtering or extraction in Go.

Representative paths include:

```text
internal/team/shared_memory.go
internal/team/context_shadow.go
internal/team/worker_memory.go
internal/team/completion_gate.go
internal/team/coordinator_memory.go
cmd/hufu/context_consolidation_cmd.go
```

This creates avoidable:

- row decoding;
- JSON decoding;
- allocations;
- memory pressure;
- SQLite → Go data transfer.

Hot predicates should move into typed SQL filters.

---

## P7. Current performance benchmarks do not represent the selected-scope pipeline

Two benchmarks already exist:

- `BenchmarkAnalyzeRecentSQLiteAnalytics`, which covers the public pipeline but only with two execution events;
- `BenchmarkExecutionTelemetryLegacyVsSQL`, which covers 10K / 100K / 1M execution events but selects all generated runs and excludes audit, memory, report construction, `runCount` variation, and stage timings.

The second benchmark must be preserved as a historical legacy-vs-SQL comparison. The missing baseline is a realistic end-to-end `AnalyzeRecent` matrix that can show whether reducing selected scope improves total work.

---

# 4. Design principles

## 4.1 Prefer set-based SQL

Bad:

```go
for _, run := range runs {
    collectExecution(run)
    collectAudit(run)
    collectMemory(run)
}
```

Preferred:

```sql
SELECT
    run_id,
    ...
FROM ...
JOIN selected_runs USING (run_id)
GROUP BY run_id;
```

---

## 4.2 Filter before projection

Bad:

```text
all history
→ task projection for all runs
→ select 10 runs
```

Preferred:

```text
all history
→ identify 10 runs
→ project only those 10 runs
```

---

## 4.3 Use TEMP tables as relational parameters

Instead of:

```sql
WHERE run_id IN (?, ?, ?, ?, ...)
```

create:

```sql
CREATE TEMP TABLE selected_runs (
    run_id   TEXT PRIMARY KEY,
    ordinal  INTEGER NOT NULL UNIQUE CHECK (ordinal >= 0),
    team     TEXT NOT NULL,
    start_ns INTEGER,
    end_ns   INTEGER
) WITHOUT ROWID;
```

Use:

```sql
JOIN selected_runs sr ON sr.run_id = e.run_id
```

Benefits:

- stable SQL text;
- no SQLite variable-limit concern;
- reusable scope;
- deterministic ordering;
- easier `EXPLAIN QUERY PLAN`;
- simpler aggregation APIs.

---

## 4.4 Optimize only with evidence

Every performance change must have:

1. semantic regression tests;
2. benchmark before/after;
3. `EXPLAIN QUERY PLAN` evidence when changing indexes;
4. rollback path.

Do not add an index merely because a column appears in a WHERE clause.

---

# 5. Target architecture

```text
                        ┌─────────────────────────┐
                        │ execution-events.jsonl  │
                        └────────────┬────────────┘
                                     │
                             streaming ingestion
                                     │
                                     ▼
                         ┌───────────────────────┐
                         │ execution_events TEMP│
                         └───────────┬───────────┘
                                     │
                          SQL recent-run selection
                                     │
                                     ▼
                         ┌───────────────────────┐
                         │ selected_runs TEMP    │
                         │ ordinal, run_id       │
                         └───────────┬───────────┘
                                     │
          ┌──────────────────────────┼─────────────────────────┐
          │                          │                         │
          ▼                          ▼                         ▼
 task_summary TEMP          selected execution        selected run windows
 selected runs only          aggregates                / revision
          │                          │                         │
          ├──────────┬───────────────┴─────────────┬───────────┘
          │          │                             │
          ▼          ▼                             ▼
      overall      grouped                       trend
      metrics      metrics                  one set-based pass
          │          │                             │
          └──────────┴─────────────┬───────────────┘
                                   ▼
                                Report
```

Audit:

```text
audit JSONL
→ minimal TEMP audit projection
→ correlate against selected_runs window columns
→ GROUP BY selected run
```

Memory:

```text
validated event-store stream
→ minimal TEMP memory projection
→ global memory summary once
→ selected-run retry/token denominators set-based
```

---

# 6. Work packages

# WP-0 — Establish performance and semantic baselines

Priority: **P0**

## Goal

Do not optimize blind.

## Files

Extend the existing benchmark/test surfaces:

```text
internal/improve/improve_benchmark_test.go
internal/improve/sqlite_analytics_bench_test.go
internal/improve/sqlite_analytics_*_test.go
internal/improve/testdata/...
```

Add helper files if needed:

```text
internal/improve/benchmark_fixture_test.go
internal/improve/benchmark_metrics_test.go
```

## Required benchmark datasets

Reuse one deterministic fixture generator for the new end-to-end matrix. It must generate at least:

| Profile | Execution events | Runs | Purpose |
|---|---:|---:|---|
| tiny | 100 | 5 | fast regression |
| small | 10,000 | 100 | normal local history |
| medium | 100,000 | 1,000 | scaling boundary |
| large | 1,000,000 | 10,000 | stress / architecture validation |

Test at least:

```text
runCount = 1
runCount = 10
runCount = 100
```

Include realistic proportions of:

- task lifecycle events;
- retries;
- multiple agents;
- multiple models;
- task types;
- skill arrays;
- malformed JSONL lines;
- empty run IDs;
- audit tool calls/errors;
- memory retrieval/usage/outcome events.

Preserve `BenchmarkExecutionTelemetryLegacyVsSQL` unchanged as the historical execution-only comparison unless a fixture bug is proven. Add a separate benchmark named `BenchmarkAnalyzeRecentSelectedScope` for the matrix above. It must invoke the same private orchestration used by public `AnalyzeRecent`, including team-definition reads and report construction.

Fixture generation must occur before `b.ResetTimer`. The 1M profile is opt-in through `HUFU_BENCH_LARGE=1`; tiny, small, and medium remain directly runnable. Every sub-benchmark name must include profile, event count, run count, and `runCount` so before/after output is comparable.

## Report benchmark stages

Instrument stage durations without changing public `Report`.

Minimum internal timing stages:

```text
open
load_execution
load_audit
load_memory
create_indexes
select_runs
task_projection
aggregate_execution
aggregate_audit
aggregate_memory
aggregate_groups
aggregate_trend
total
```

Use one private orchestration seam rather than timing public stages from tests by copying production logic:

```go
func analyzeRecent(
    ctx context.Context,
    workspace, teamName, teamDir string,
    runCount int,
    diagnostics *AnalyticsDiagnostics,
) (*Report, error)
```

Public `AnalyzeRecent` calls this with `context.Background()` and `nil` diagnostics. Tests and benchmarks pass a non-nil diagnostics value. Stage timing must use a small internal helper and must not change error wrapping, report fields, or public API.

Task projection is currently lazy. In PR 1, measure it inside `materializeTaskViews` without changing when it runs. In PR 3, after selected scope exists, orchestration must explicitly materialize it after `selected_runs` is frozen and before aggregation; aggregation methods may retain defensive `ensureTaskViews` checks.

For Go benchmarks also record:

```text
ns/op
B/op
allocs/op
```

`B/op` is allocation volume, not peak heap or RSS. This plan gates allocation volume only. Do not claim a peak-memory improvement or regression unless a separate reproducible heap/RSS measurement is attached.

## Semantic baseline

Create golden fixtures covering:

- team omitted;
- multiple teams;
- unparsable timestamps;
- same timestamps;
- empty team;
- empty run ID;
- retry;
- last agent/model/task_type overwrite;
- last reported skills overwrite;
- overlapping audit windows;
- memory global summary behavior;
- selected-run memory evidence;
- legacy execution events.

Generate expected reports from the current implementation and freeze their deterministic projection. Compare structured reports after clearing `GeneratedAt` and replacing fixture-specific absolute `Workspace` / team-definition paths with stable placeholders. Do not regenerate goldens after an implementation change unless the diff is independently shown to be an intentional product-semantic change outside this optimization program.

Retain the existing focused regression tests as independent oracles; the new golden suite supplements them and must not replace them.

## Exit criteria

WP-0 is complete when:

- the existing legacy-vs-SQL benchmark still runs;
- the end-to-end 100 / 10K / 100K matrix exists for `runCount=1,10,100`;
- the end-to-end 1M benchmark can be run explicitly as a long benchmark;
- semantic golden tests pass;
- benchmark output separates load/index/query time.

Do not begin speculative SQLite tuning before this baseline exists.

---

# WP-1 — Replace Go-side recent-run materialization with SQL selection

Priority: **P0**

## Goal

Return only the selected run set from SQLite.

## Current problem

Current logic effectively:

```text
GROUP all runs
→ Scan all summaries to Go
→ determine team
→ filter team
→ truncate to N
```

## Required design

Add SQL that:

1. resolves each run's team from its first qualifying event;
2. calculates run start/end using parseable timestamps;
3. if `teamName == ""`, determines the current default team using existing ordering semantics;
4. selects only the latest `runCount` runs for that team;
5. preserves final oldest-to-newest report order.

Use the existing `qualifying`, `run_team`, and `run_window` definitions as the semantic source. Selection has two explicit steps:

1. If `teamName == ""`, resolve it from the globally newest run:

```sql
SELECT team
FROM run_summary
ORDER BY (end_ns IS NULL) ASC, end_ns DESC, run_id DESC
LIMIT 1;
```

2. Select the newest `runCount` rows for the resolved team, then restore oldest-to-newest order before assigning ordinals:

```sql
SELECT run_id, team, start_ns, end_ns
FROM (
    SELECT run_id, team, start_ns, end_ns
    FROM run_summary
    WHERE team = ?
    ORDER BY (end_ns IS NULL) ASC, end_ns DESC, run_id DESC
    LIMIT ?
)
ORDER BY (end_ns IS NOT NULL) ASC, end_ns ASC, run_id ASC;
```

The explicit NULL sort expressions are required; do not rely on implicit NULL ordering. SQL may combine the two steps into one CTE statement if tests prove identical behavior.

## Add TEMP selected-run scope

Create one table in the initial TEMP schema; do not create a second `selected_run_windows` table:

```sql
CREATE TEMP TABLE selected_runs (
    run_id   TEXT PRIMARY KEY,
    ordinal  INTEGER NOT NULL UNIQUE CHECK (ordinal >= 0),
    team     TEXT NOT NULL,
    start_ns INTEGER,
    end_ns   INTEGER
) WITHOUT ROWID;
```

`ordinal=0` is the oldest selected run. Populate all rows in one transaction. Do not expose a partially populated selected scope to later queries.

## Session lifecycle

Add explicit session state:

```go
selectedRunsReady bool
taskViewsReady    bool
```

Required transitions:

```text
create schema
→ load execution events
→ create any selection-required execution indexes
→ resolve team and populate selected_runs atomically
→ explicitly materialize selected-only task views
→ aggregate
```

PR 2 implements the transition through frozen `selected_runs` while existing aggregators may still extract ordered IDs for compatibility. PR 3 converts all scopes and adds the explicit selected-only projection transition.

- Selecting twice is an error, including attempts to change team or `runCount`.
- Set `selectedRunsReady=true` only after the selection transaction commits, including a successful zero-row selection; a zero-row result is still a completed one-shot selection.
- Loading execution events after selection is an error.
- Materializing task views before selection is an error.
- Loading audit or memory rows does not change selected scope and may occur before or after selection until WP-5 determines the fastest indexed sequence.
- An empty result is returned to orchestration as no selected rows; `AnalyzeRecent` continues to translate that to `ErrNoExecutionData`.

## API direction

Replace APIs centered on:

```go
[]string runIDs
```

for internal SQL execution with session-owned selected scope.

Public report construction can still extract `[]string`.

Example:

```go
type selectedRun struct {
    RunID   string
    Ordinal int
    Team    string
    StartNS sql.NullInt64
    EndNS   sql.NullInt64
}
```

`sqlSelectRecentRunSummaries` becomes the only function allowed to populate the table. Ordinary aggregators must read `selected_runs`; they must not accept an alternate `[]string` scope. A small `sqlSelectedRuns` reader may return the ordered slice needed to construct public `RunIDs` and `Trend`.

## Acceptance tests

Must prove exact parity for:

- missing team name;
- mixed teams;
- NULL timestamps;
- equal end timestamps;
- fewer than requested runs;
- deterministic `run_id` tie break;
- a run whose first and later qualifying rows name different teams;
- duplicate ordinal rejection;
- second selection rejection;
- execution ingestion after selection rejection;
- task projection before selection rejection.

---

# WP-2 — Scope task projection to selected runs

Priority: **P0**

## Goal

Never compute task state for runs excluded from the report.

## Change

Current:

```sql
FROM execution_events
WHERE task_id <> '' AND team <> ''
```

Target:

```sql
FROM execution_events e
JOIN selected_runs sr ON sr.run_id = e.run_id
WHERE e.task_id <> ''
  AND e.team <> ''
```

Apply selected scope to:

- aggregate CTE;
- last agent;
- last model;
- last task type;
- last terminal;
- last skill event.

## TEMP schema

Evaluate:

```sql
CREATE TEMP TABLE task_summary (...) WITHOUT ROWID;
CREATE TEMP TABLE task_skills (...) WITHOUT ROWID;
```

for composite-primary-key tables.

This change is **benchmark gated**.

Do not assume `WITHOUT ROWID` is faster; retain it only if medium/large benchmarks improve or storage/allocation cost decreases without query regression.

## Lifecycle change

Current session boolean:

```go
taskViewsReady bool
```

becomes logically:

```go
selectedRunsReady bool
taskViewsReady    bool
```

Invariant:

```text
load events
→ select runs
→ freeze selected_runs
→ materialize task views
```

Reject attempts to change selected scope after task projection.

## Acceptance criteria

- report output byte-equivalent except `GeneratedAt`;
- task projection row count equals only selected-run tasks;
- 100K history / `runCount=10` materially reduces task projection work.

---

# WP-3 — Replace `IN (...)` scopes with `selected_runs` joins

Priority: **P1**

## Goal

Use one relational run scope everywhere.

## Remove/reduce

```go
runsInClause(...)
```

Do not build variable SQL for ordinary selected-run queries.

## Convert queries

Examples:

### Event window

From:

```sql
WHERE run_id IN (...)
```

to:

```sql
FROM execution_events e
JOIN selected_runs sr ON sr.run_id = e.run_id
```

### Tokens by agent

```sql
FROM execution_events e
JOIN selected_runs sr ON sr.run_id = e.run_id
WHERE e.task_id <> ''
  AND e.team <> ''
GROUP BY ...
```

### Task summary reads

```sql
FROM task_summary t
JOIN selected_runs sr ON sr.run_id = t.run_id
```

Task tables are already selected-only after WP-2, so the join may be unnecessary there. Prefer the simplest plan verified by `EXPLAIN QUERY PLAN`.

### Memory evidence

Join `memory_events` with `selected_runs`.

### Scope rule

After this work package, `runsInClause` must have no production callers and should be deleted. Test helpers may insert arbitrary run scopes directly into `selected_runs`; production aggregators always operate on the one frozen session scope.

## Benefits

- constant SQL shape;
- no run-count-dependent placeholder count;
- reusable query plans;
- cleaner APIs;
- easier plan assertions.

---

# WP-4 — Eliminate trend N+1 queries

Priority: **P0**

## Goal

Compute trend metrics for all selected runs in set-based queries.

## Current anti-pattern

Conceptually:

```go
for each run:
    execution metrics
    audit metrics
    memory metrics
```

Replace with:

```go
trend := analytics.sqlCollectTrend(ctx)
```

returning:

```go
[]TrendPoint // ordered by selected_runs.ordinal
```

The collector may use private maps while joining query families, but the final slice and all nested maps must be initialized deterministically. Preserve non-nil empty `TokensByAgent`, `ToolCallsByAgent`, and `ToolErrorsByAgent` maps.

## Execution trend

Aggregate all selected runs:

```sql
SELECT
    sr.ordinal,
    sr.run_id,
    COUNT(...),
    SUM(...),
    ...
FROM selected_runs sr
LEFT JOIN task_summary t ON t.run_id = sr.run_id
GROUP BY sr.ordinal, sr.run_id
ORDER BY sr.ordinal;
```

Use additional grouped CTEs where task and event-level semantics differ.

Do not merge token attribution semantics incorrectly:

- task metrics derive from `task_summary`;
- `TokensByAgent` remains per-event attribution.

For every selected run, execution trend must reproduce:

- `RunID` and `RunCount=1`;
- `StartedAt` / `EndedAt` from that run's nullable selected window, formatted exactly as today;
- task status, attempt, retry, and token fields using the same formulas as overall metrics;
- zero-task runs with initialized empty maps.

## Audit trend

Use the window columns on `selected_runs`.

A single audit event may match multiple overlapping run windows.

That is required if current repeated per-run window queries would count it for each overlapping run.

Example:

```sql
SELECT
    sr.run_id,
    COALESCE(a.agent, '') AS agent,
    COALESCE(SUM(CASE WHEN a.event = 'tool_call' THEN 1 ELSE 0 END), 0),
    COALESCE(SUM(CASE WHEN a.event = 'tool_error' THEN 1 ELSE 0 END), 0)
FROM selected_runs sr
LEFT JOIN audit_events a
  ON a.team = sr.team
 AND a.timestamp_unix_ns >= sr.start_ns
 AND a.timestamp_unix_ns <= sr.end_ns
GROUP BY sr.run_id, COALESCE(a.agent, '');
```

Aggregate overall audit metrics separately over the inclusive window from the earliest parseable selected timestamp to the latest parseable selected timestamp. If no selected timestamp is parseable, overall audit metrics are zero. Do not derive overall audit metrics by summing per-run values because overlapping windows would double-count rows.

Only insert an agent map entry when its corresponding count is positive; the synthetic empty-agent zero row from a no-match LEFT JOIN must not create a map entry. When a selected run has NULL `start_ns` or `end_ns`, its trend audit metrics remain zero. Preserve the existing scanner-error prefix-commit behavior from section 2.3.4.

## Memory trend

The current implementation intentionally computes several memory counters globally over the whole canonical memory event stream.

Do not change that semantic in this optimization.

Implement exactly:

1. compute global memory summary once;
2. compute the global distinct applied `(run_id, task_id)` relation and count once;
3. compute input-token denominators for overall selected scope and grouped by selected run;
4. compute scoped retry pairs and their assisted/unassisted classification for overall scope and grouped by selected run;
5. combine them using every formula in section 2.3.3.

Do **not** run the same global memory summary query N times.

## Team revision

Compute both required revision projections:

1. overall lexically sorted distinct non-empty revisions from every selected execution row;
2. latest non-empty revision by run using:

```sql
ROW_NUMBER() OVER (
    PARTITION BY run_id
    ORDER BY event_seq DESC
)
```

or equivalent max-event-seq join.

If the overall set is empty, orchestration retains the existing `definitionRevision(teamDir)` fallback. Per-run values do not use that fallback.

## Acceptance criteria

For `runCount=N`, the number of analytics SQL query families must remain O(1), not O(N).

Add a package-private query-executor wrapper used by the session (`QueryContext`, `QueryRowContext`, and `ExecContext`) so tests can count statements without parsing logs. The exact statement count may change with a justified refactor, but tests must compare `runCount=1` and `runCount=100` and assert the aggregation-stage query count is equal.

Add parity cases for:

- unrelated-run memory applied pairs affecting the frozen global denominator;
- overlapping audit windows without overall double counting;
- NULL run windows;
- a run containing multiple revisions, where overall and trend revision projections differ;
- zero-task and zero-token runs;
- non-nil empty maps in every trend point.

---

# WP-5 — Rationalize analytics indexes using actual query plans

Priority: **P1**

## Goal

Stop paying index-construction cost for indexes not used by the current query plan.

## Current index review

Review at least:

```text
idx_execution_team_run
idx_execution_task_attempt
idx_execution_agent
idx_execution_model
idx_execution_task_type
idx_skill_task
idx_audit_team_time
idx_memory_type_run
```

Several grouping operations now run against `task_summary`, not directly against execution-event agent/model/task-type columns.

Therefore the historical execution indexes may no longer be useful.

## Required process

For every index:

1. identify consuming SQL;
2. capture `EXPLAIN QUERY PLAN`;
3. benchmark medium dataset with index;
4. benchmark without index;
5. retain only when total pipeline time improves.

Do not optimize isolated query latency while increasing total `load + build index + query` cost.

## Candidate replacement index

Evaluate a run/task/order-oriented index, for example:

```sql
CREATE INDEX temp.idx_execution_run_task_seq
ON execution_events(run_id, task_id, event_seq);
```

This may better serve selected task projections than separate agent/model/task-type indexes.

Benchmark; do not assume.

## Test support

Add helpers such as:

```go
explainQueryPlan(t, conn, query, args...)
```

Tests should assert important planner properties only when stable enough.

Avoid brittle full-plan string matching.

Use at least five before/after samples and `benchstat`. Retain an index change only when it improves the 100K / `runCount=10` end-to-end pipeline by at least 5% or the combined index-build plus affected-query stages by at least 10%, without a repeatable tiny-profile or ingestion regression above 5%. If no change passes, record a retain-all/no-change decision and close WP-5 successfully.

---

# WP-6 — Batch analytics ingestion

Priority: **P1, benchmark gated**

## Goal

Reduce `database/sql` call overhead while keeping streaming memory behavior.

## Current property to preserve

Do not materialize complete event history into Go slices.

Go allocation growth should remain bounded by batch size rather than history size; this work package does not claim a measured peak-RSS bound.

## Strategy

Keep:

```text
Scanner
→ JSON decode
→ projection
```

but buffer a bounded batch of projected rows.

Evaluate fixed-size multi-row INSERT.

Example:

```sql
INSERT INTO execution_events (...)
VALUES
  (?, ?, ...),
  (?, ?, ...),
  ...
```

## Batch size

Add a private helper that reads `PRAGMA compile_options`, parses `MAX_VARIABLE_NUMBER=<n>`, and falls back conservatively to 999 when the option is absent. For a table with `columnCount` bound values per row, reject non-positive column counts and compute:

```text
maxRows = maxVariableNumber / columnCount
batchRows = min(configuredCandidate, maxRows)
```

Never emit an empty batch or a statement whose bound-value count exceeds the discovered/fallback limit. Batch size is an internal benchmark parameter, not a public setting.

Suggested initial benchmark matrix:

```text
1
32
64
128
256
```

For `execution_events` with many columns, smaller batches may be optimal.

Execution-event skills use a separate bounded batch because they have a different column count and cardinality.

Audit and memory loaders should be benchmarked independently.

## Error semantics

Batching must preserve:

- malformed-line skip counts;
- missing-run-id skip counts;
- transaction rollback behavior;
- event_seq determinism;
- source ordering;
- memory hash-chain validation behavior.

It must also preserve the audit loader's per-file scanner-error behavior from section 2.3.4. Memory rows must not be committed until the complete hash chain validates; a late validation failure rolls back every buffered memory row.

## Merge threshold

Only retain multi-row batching if on 100K events it produces at least one of:

- >= 15% execution-load time improvement; or
- >= 10% end-to-end improvement,

without:

- > 10% repeatable `B/op` allocation-volume regression;
- semantic changes;
- substantially worse tiny-dataset latency.

Use `benchstat` over at least five before/after samples. Record the selected batch size and retain the single-row prepared-statement implementation when no candidate passes.

---

# WP-7 — Benchmark-gated PRAGMA profile for ephemeral analytics

Priority: **P2**

## Goal

Exploit the fact that analytics DB is disposable and in-memory.

## Centralize configuration

Create one function:

```go
func configureAnalyticsSQLite(ctx context.Context, conn *sql.Conn) error
```

Do not scatter PRAGMAs across loaders.

## Candidates to benchmark

Evaluate, do not blindly enable:

```sql
PRAGMA temp_store = MEMORY;
PRAGMA synchronous = OFF;
PRAGMA journal_mode = MEMORY;
```

Potential cache sizing may also be benchmarked, but avoid hard-coding large memory reservations.

Because the DB is already `:memory:` and single-connection, some PRAGMAs may have no measurable benefit.

If benefit is negligible, do not add configuration complexity.

Benchmark the default and each candidate independently before benchmarking a combined profile. Retain a setting only if, at 100K events / `runCount=10`, it yields either at least 5% end-to-end improvement or 10% improvement in combined load/index stages, with no repeatable regression above 5% on the tiny profile and no allocation-volume regression above 5%. Use at least five samples and `benchstat`; otherwise keep SQLite defaults.

Add a connection-level test that reads back every retained PRAGMA immediately after `configureAnalyticsSQLite` and a persistence-isolation test proving canonical `context.sqlite` PRAGMAs and bytes are unchanged.

## Forbidden

Never apply these disposable-analytics settings to canonical `context.sqlite`.

---

# WP-8 — Push runtime context filtering into SQLite

Priority: **P0/P1**

## Goal

Eliminate `Limit: 100000` as a pseudo-query API without changing authorization, lifecycle, temporal, ordering, or branch-lineage semantics.

This work package is split because ordinary canonical queries, true bulk scans, and hybrid retrieval have different owners and safety constraints.

## WP-8A — Typed predicates for candidate settlement

Extend `RepositoryQuery` only with **typed, domain-specific predicates** that correspond to real hot paths.

Do not add:

```go
WhereSQL string
```

Do not expose arbitrary SQL.

Do not expose arbitrary caller-controlled column names.

Add exactly these fields in the first implementation:

```go
type RepositoryQuery struct {
    Scope             Scope
    Visibility        ScopeVisibility
    Kinds             []ContextKind
    IncludeSuperseded bool
    IncludeExpired    bool
    IncludeCandidates bool

    Lifecycles  []ContextLifecycle
    OriginRunID string
    SourceTypes []string

    Limit             int
}
```

Update both `Repository` and `ReadOnlyRepository` through the same private predicate compiler so the two read surfaces cannot drift. Existing test doubles must exercise the same validation contract.

Contract:

- `len(Lifecycles) == 0` preserves the existing `IncludeCandidates` behavior from section 2.3.5.
- A non-empty `Lifecycles` list and `IncludeCandidates=true` is an invalid query and returns an error before SQL execution.
- Reject unknown or empty lifecycle values. Dedupe valid values before generating bound placeholders.
- `OriginRunID == ""` means no origin-run predicate. Otherwise bind it to `json_extract(metadata_json, '$.run_id') = ?` without trimming or interpolating it.
- `len(SourceTypes) == 0` means no source predicate. Otherwise trim, drop empty values, dedupe, and bind them to `json_extract(source_json, '$.type') IN (...)`. If a non-empty input normalizes to an empty list, return an invalid-query error rather than broadening the query.
- All new values are SQL parameters. No caller value may become SQL text or an identifier.
- New predicates are appended after canonical scope/visibility, supersession, expiry, lifecycle, validity, and kind predicates. Existing result order and limit behavior remain unchanged.

Do not add `UpdatedAfter`, arbitrary metadata filters, visibility metadata, memory tier, or branch metadata in this PR. They lack one shared, proven query contract across the current consumers.

## Origin run filtering

Several runtime operations need run-produced context.

If `run_id` remains inside `metadata_json`, add a typed predicate compiled internally to:

```sql
json_extract(metadata_json, '$.run_id') = ?
```

Evaluate an expression index:

```sql
CREATE INDEX idx_context_origin_run
ON context_items(
    project_id,
    json_extract(metadata_json, '$.run_id'),
    lifecycle
);
```

Do not expose generic metadata-key filtering just because SQLite JSON1 supports it.

A future schema may promote hot metadata to typed columns, but this plan should begin with the lower-risk typed query + expression-index approach.

The expression index is not part of WP-8A by default. First benchmark candidate-settlement queries at representative 10K/100K context-row fixtures. If it passes WP-9's canonical write/read total-cost gate, add it as a new immutable migration; never edit an existing migration string.

## WP-8A caller conversion matrix

Convert only predicates already enforced by each caller's Go code:

| Caller purpose | SQL predicates to add | Go checks that remain |
|---|---|---|
| shared-memory confirm/reject | candidate lifecycle, origin run, `shared_memory_candidate` source | manifest evidence and scope validation |
| run-shared context candidate lookup | candidate lifecycle, origin run, `run_shared_context` source | none beyond ID collection |
| worker-memory confirm/reject | candidate lifecycle, origin run | visibility, memory tier, creation branch, passed-task evidence |
| current-run session prompt candidates | candidate lifecycle, origin run | prompt composition/deduplication |

For `coordinator_memory.autoExtractCanonicalLTM`, do not replace its mixed confirmed/candidate rules with one `OriginRunID` query. It intentionally accepts confirmed rows with no run ID as well as rows from the current run. Either issue two typed queries and preserve final ordering/deduplication explicitly, or leave this caller for a later measured change.

Every converted caller needs a fixture containing matching and non-matching lifecycle, run, source, visibility, tier, and branch values. Assert ordered `ContextItem` equality before and after filtering, not just selected IDs.

## WP-8B — Streaming true bulk operations

Some operations genuinely require every authorized row. Replace their arbitrary 100,000-row ceiling with a streaming method on both `Repository` and `ReadOnlyRepository` (or on one shared embedded read interface used by both), rather than keyset pagination:

```go
Iterate(
    ctx context.Context,
    q RepositoryQuery,
    visit func(ContextItem) error,
) error
```

Contract:

- `q.Limit` must be zero; a non-zero limit is an error.
- Reuse the same predicate compiler as `Query`.
- Execute one ordered SELECT using `priority DESC, created_at DESC, id ASC`, without `LIMIT`.
- Decode and visit one row at a time. The callback error stops iteration and is returned unchanged; row/scan errors are wrapped with operation context.
- The open `Rows` cursor supplies one SQLite read snapshot for the scan.
- The callback must not call the same repository or perform writes; document this non-reentrancy rule because the canonical handle currently has one connection.
- Honor context cancellation and always close rows.
- Do not add a second database handle or connection pool in WP-8B.

Initial conversions:

- `VectorStore.Rebuild`: remove the 100K truncation by materializing authorized confirmed items through `Iterate`, close the iterator, then perform embeddings and embedding-state updates exactly as today. This conversion fixes correctness but intentionally does not claim bounded application memory; bounded rebuilds require a later paging or separate-read-handle design. Never hold the SQLite cursor while calling the embedding provider.
- context consolidation dry-run: stream into its cluster builder; output ordering and cluster identity must remain stable.
- shared/session/persistent projection rebuilds: use an iterator-backed private implementation while retaining current public return types where compatibility requires them.
- promotion eligibility: extend its local `EligibilityRepository` read contract and test doubles with `Iterate`; stream and collect only rows that pass its cheap item-local eligibility checks, then perform `ExperienceAggregate` lookups after iteration closes.
- `context history` remains unchanged in WP-8B. Replacing its authorized bulk snapshot with repeated unrestricted `Get` calls would change scope/temporal behavior. A later change may add an authorized item-ID lookup plus cycle detection, but that API must be specified and tested first.

If a consumer truly requires a materialized slice, it may build one explicitly from `Iterate`; the removal of silent 100K truncation remains the correctness gain.

## WP-8C — Hybrid retrieval lineage scope (discovery gate)

`internal/team/worker_memory.go` uses `SearchRequest{Limit: 100000}` so ancestor-branch items can be filtered by an allowed-ID lineage set after exact/FTS/vector ranking. `RepositoryQuery` predicates and `Iterate` do not solve this path, and lowering the limit can let post-fork results crowd out valid ancestors.

Before changing this path, produce a focused design note that traces identical constraints through:

- `SearchExact`;
- `SearchLexical` / FTS5;
- `VectorSearcher.SearchVector` and canonical hydration;
- `HybridRetrieve` filtering, fusion, MMR, and final limit;
- active branch versus restricted ancestor branches.

The design must choose one authorization-preserving representation for the allowed lineage set, define behavior when the set exceeds SQLite's variable limit, and include exact/lexical/vector parity tests. Until that note is accepted, leave this `Limit: 100000` path unchanged and list it as an explicit remaining risk.

## WP-8 acceptance criteria

- WP-8A candidate-settlement paths no longer fetch unrelated lifecycles or runs.
- WP-8B true bulk paths have no silent 100K truncation and preserve one-snapshot ordering semantics.
- Old fixture result equals new fixture result for every converted caller, including order and decoded fields.
- Candidate-settlement benchmarks report rows returned/decoded, query latency, and allocation deltas; query-plan evidence is required before claiming reduced SQLite scanning.
- The worker-memory lineage limit is either resolved by an accepted WP-8C design and implementation or remains explicitly documented as deferred; it must not be silently removed.

---

# WP-9 — Canonical SQLite index review

Priority: **P1, discovery gated**

## Goal

Align indexes with current query shapes rather than accumulated historical assumptions.

## Existing strengths to preserve

Keep the existing principles:

- WAL;
- busy timeout;
- foreign keys;
- migration checksum validation;
- pre-migration backup;
- FTS5;
- deterministic migrations.

## Review queries

At minimum benchmark/query-plan:

- canonical `Query`;
- FTS search;
- candidate duplicate lookup;
- append deduplication;
- reducer deduplication;
- context outcome lookups;
- experience aggregates;
- promotion proposal list/status;
- memory policy lookup.

## Avoid redundant wide indexes

Current content hash is high-selectivity.

Do not create a large composite dedupe index unless benchmarks prove the existing:

```text
(project_id, content_hash)
```

index still leaves meaningful lookup cost.

Every canonical index adds write amplification.

The objective is **total runtime cost**, not maximum read index coverage.

## JSON expression indexes

Prefer expression indexes only for JSON fields that satisfy all:

1. high-frequency predicate;
2. stable semantic meaning;
3. no existing typed column;
4. measured scan cost;
5. expected selectivity.

## Required discovery deliverable

Before changing canonical schema, attach one reviewable table to the proposed PR or a checked-in performance note:

```text
query/caller
fixture cardinality and selectivity
current indexes
EXPLAIN QUERY PLAN before
candidate index
EXPLAIN QUERY PLAN after
read latency before/after (count >= 5, benchstat)
representative append/update latency before/after
database size before/after
decision: add / retain / remove / no change
```

No production index change is authorized until this deliverable exists. Any accepted index change must be an additive new migration; existing migration SQL and checksums remain immutable. Removing an existing index likewise requires a new migration. The PR must include open/migrate/backup tests against both a pre-change fixture and a newly created database.

Default decision is no schema change. A candidate normally needs at least a repeatable 15% improvement in its measured hot query or 10% in the measured end-to-end caller, with no more than 5% regression in representative canonical writes and no material database-size increase. Exceptions require a written rationale using absolute latency and workload frequency.

---

# WP-10 — Optional concurrent read path for canonical SQLite

Priority: **P2, only after profiling**

## Problem hypothesis

Current repository intentionally uses one open connection, which serializes this process's SQLite operations.

In a multi-worker agent runtime, concurrent context retrieval may eventually queue behind unrelated operations.

This is only a hypothesis until measured.

## First add observability

Measure:

```text
repository query latency
write transaction latency
busy retries
time waiting to acquire DB work
concurrent worker count
```

## If contention is real

Prefer architectural separation:

```text
write handle
  max connections = 1
  WAL writer
  foreign_keys ON
  all mutation paths

read handle
  query_only
  read-only
  small bounded pool
```

Suggested initial read pool benchmark:

```text
1
2
4
8
```

Do not use an unbounded pool.

## Important SQLite constraint

`PRAGMA foreign_keys` is connection-local.

If write connections ever become pooled, foreign-key enforcement must be configured on every connection.

This is one reason to keep a single canonical writer.

## Concurrency test

Create tests with:

- parallel readers;
- one writer;
- WAL mode;
- cross-process busy simulation where practical;
- cancellation;
- migration/open behavior.

## Skip condition

If current repository waits are insignificant relative to LLM/tool latency, do not implement this phase.

Complexity is not justified without measured contention.

## Authorization gate

The profiling PR may add test-only benchmarks and internal duration counters, but must not add a second production handle. A concurrent read implementation is authorized only when a four-reader/one-writer representative fixture shows both:

- p95 repository wait time is at least 10 ms and at least 20% of repository operation latency; and
- a bounded read-handle prototype improves p95 read latency by at least 25% without changing results, foreign-key enforcement, migration behavior, cancellation, or write latency by more than 5%.

If the gate passes, write a separate design note defining connection initialization, close ownership, snapshot expectations, read-after-write expectations, migration exclusion, and failure behavior before production code changes. WP-10 itself is not that implementation authorization.

---

# WP-11 — SQLite maintenance and observability

Priority: **P2**

## Goal

Make storage behavior inspectable rather than automatically aggressive.

## WP-11A — Read-only diagnostics

Add the concrete existing-inspection subcommand:

```text
hufu inspect storage [--workspace/-w PATH] [--format text|json]
```

It uses the existing inspect envelope/exit-code conventions and `OpenSQLiteReadOnly`. It must not create the database or parent directories, run migrations, enable WAL, checkpoint, optimize, vacuum, or rebuild projections. Report:

```text
page_count
freelist_count
page_size
journal_mode
wal_autocheckpoint
schema version
database file size
WAL file size
FTS row count
context row count
```

JSON field names are stable snake_case equivalents:

```text
page_count
freelist_count
page_size
journal_mode
wal_autocheckpoint
schema_version
database_bytes
wal_bytes
fts_rows
context_rows
```

Missing WAL files report `wal_bytes: 0`. A missing/unreadable `context.sqlite`, invalid schema query, or canceled context returns the existing inspect error envelope and a non-zero exit. No context content, metadata, prompts, or audit payload may be emitted. Do not make normal runtime output noisy.

Tests must prove read-only filesystem behavior using before/after file snapshots, text/JSON parity, missing WAL handling, missing database behavior, and cancellation.

## WP-11B — Maintenance policy (discovery gated)

Do not automatically run full:

```sql
VACUUM;
```

during normal execution.

Potential explicit maintenance operation may run:

```sql
PRAGMA optimize;
```

after validation and benchmark.

If WAL growth becomes measurable, define a deliberate checkpoint policy instead of checkpointing every write.

WP-11B does not authorize a maintenance command. First provide measurements, an explicit operator command contract, backup/recovery behavior, cancellation behavior, disk-space preflight, and tests showing that interruption cannot corrupt canonical storage. Until then, no runtime or inspect path may execute a write-capable maintenance PRAGMA.

---

# 7. Query-specific redesign

## 7.1 Recent-run selection

Target:

```sql
WITH qualifying AS (
    SELECT event_seq, run_id, team, timestamp_unix_ns
    FROM execution_events
    WHERE run_id <> ''
      AND team <> ''
),
run_team AS (
    SELECT run_id, team
    FROM (
        SELECT
            run_id,
            team,
            ROW_NUMBER() OVER (
                PARTITION BY run_id
                ORDER BY event_seq
            ) AS rn
        FROM qualifying
    )
    WHERE rn = 1
),
run_window AS (
    SELECT
        run_id,
        MIN(timestamp_unix_ns) AS start_ns,
        MAX(timestamp_unix_ns) AS end_ns
    FROM qualifying
    WHERE timestamp_unix_ns IS NOT NULL
    GROUP BY run_id
)
...
```

Preserve current ordering exactly.

Then insert selected rows into `selected_runs`.

---

## 7.2 Task summary

All CTEs must be scoped early:

```sql
FROM execution_events e
JOIN selected_runs sr ON sr.run_id = e.run_id
```

Do this inside each source CTE rather than filtering only at the final SELECT.

Reason:

```text
filter early
→ fewer window partitions
→ fewer rows sorted
→ fewer aggregate rows
```

---

## 7.3 Grouped metrics

Existing grouped metrics are already conceptually correct and set-based.

Do not rewrite them prematurely.

After selected-only task materialization, benchmark whether four grouping queries are significant.

Only then consider a `UNION ALL` query that emits:

```text
dimension
group_key
metrics...
```

Avoid sacrificing clarity for a negligible reduction in four local SQL calls.

---

## 7.4 Audit metrics

Load only the minimal existing audit projection.

After selected run windows are available, consider filtering ingestion to:

```text
selected team
overall min selected start
overall max selected end
```

This is safe only if it preserves both:

- overall report audit metrics;
- per-run trend metrics.

Because every per-run window is contained in the selected overall window, rows outside the global selected window cannot contribute to selected-run audit metrics.

This can significantly reduce TEMP audit rows for long histories.

### Important

This optimization changes loader behavior, not report semantics.

Add a regression test with audit rows:

- before selected runs;
- between selected runs;
- inside one run;
- inside overlapping run windows;
- after selected runs;
- other teams.

This is not part of PR 2 through PR 4. Those PRs continue to load all minimal audit rows. Only implement filtered insertion in a later benchmark PR if diagnostics show audit TEMP row/index cost is material and the change improves end-to-end 100K workload time by at least 10%. The loader must still scan every source line so malformed/scanner statistics and tolerant file behavior remain unchanged.

---

## 7.5 Memory metrics

Do not prune memory ingestion solely to selected runs in this plan.

Current public semantics intentionally use the complete canonical memory event store for several counters.

Changing that behavior would be a separate product/metric-spec decision.

Optimization should instead remove repeated queries over the same loaded memory rows.

---

# 8. SQLite schema refinements for TEMP analytics

The `selected_runs` schema is required:

```sql
CREATE TEMP TABLE selected_runs (
    run_id   TEXT PRIMARY KEY,
    ordinal  INTEGER NOT NULL UNIQUE CHECK (ordinal >= 0),
    team     TEXT NOT NULL,
    start_ns INTEGER,
    end_ns   INTEGER
) WITHOUT ROWID;
```

The following `WITHOUT ROWID` refinements remain benchmark candidates under WP-2:

```sql
CREATE TEMP TABLE task_summary (
    run_id         TEXT NOT NULL,
    task_id        TEXT NOT NULL,
    agent          TEXT NOT NULL DEFAULT '',
    model          TEXT NOT NULL DEFAULT '',
    task_type      TEXT NOT NULL DEFAULT '',
    terminal       TEXT NOT NULL DEFAULT '',
    attempts       INTEGER NOT NULL DEFAULT 0,
    total_attempts INTEGER NOT NULL DEFAULT 0,
    total_tokens   INTEGER NOT NULL DEFAULT 0,
    PRIMARY KEY (run_id, task_id)
) WITHOUT ROWID;
```

```sql
CREATE TEMP TABLE task_skills (
    run_id  TEXT NOT NULL,
    task_id TEXT NOT NULL,
    skill   TEXT NOT NULL,
    PRIMARY KEY (run_id, task_id, skill)
) WITHOUT ROWID;
```

`execution_events` should retain a deterministic integer `event_seq`.

Do not replace ordering with implicit rowid behavior.

---

# 9. Benchmark acceptance gates

All performance changes must pass semantic tests first.

## Required benchmark command

Capture before and after results from the same machine, Go toolchain, CPU-power state, fixture seed, and repository revision except for the change under test:

```bash
go test ./internal/improve \
  -run '^$' \
  -bench 'BenchmarkAnalyzeRecentSelectedScope' \
  -benchmem \
  -count 5 > before.txt

go test ./internal/improve \
  -run '^$' \
  -bench 'BenchmarkAnalyzeRecentSelectedScope' \
  -benchmem \
  -count 5 > after.txt

benchstat before.txt after.txt
```

Long dataset can use an environment switch:

```bash
HUFU_BENCH_LARGE=1
```

Do not make normal CI generate a 1M event fixture unless runtime is acceptable.

## Suggested merge gates

### P0 query architecture work

At 100K execution events / `runCount=10`:

Target:

```text
end-to-end <= 75% of baseline
```

or demonstrate a similarly material reduction in:

```text
task projection
trend aggregation
allocations
```

with no meaningful regression elsewhere.

For PR 2 through PR 4, functional/semantic correctness is mandatory even if the 25% end-to-end target is not reached. A PR below the target may merge only when it materially reduces the specifically targeted stage or allocation volume, introduces no repeatable regression above 5% in end-to-end 100K results, and documents why remaining full-history ingestion dominates total time.

### Ingestion optimization

At 100K events:

```text
load stage improvement >= 15%
```

or:

```text
total improvement >= 10%
```

### Small workload protection

At 100 events:

```text
no > 20% repeatable total latency regression
```

unless absolute regression is negligible and large-history gain is substantial.

### Semantic gate

Must be exact for all deterministic report fields.

`GeneratedAt` may differ.

Performance results are evidence only when the relevant `benchstat` comparison is statistically usable. If variance prevents a conclusion, increase `-count` or benchmark duration; do not label noise as an improvement.

---

# 10. Regression tests required

Add/retain tests for:

## Run selection

```text
default team
explicit team
NULL timestamps
equal timestamps
run-id tie break
mixed teams
empty team
empty run_id
first-event team remains authoritative when later rows differ
selection lifecycle misuse
```

## Task state

```text
last non-empty agent
last non-empty model
last non-empty task_type
last terminal state
max attempt
in_progress attempt count
token sum
skills overwrite, not union
```

## Audit

```text
inclusive start
inclusive end
outside range
other team
overlapping run windows
tool_call
tool_error
per-agent counts
scanner-error prefix retained and later files processed
overall window does not sum overlapping per-run windows
```

## Memory

```text
global memory summary remains global
selected run input-token denominator
applied task retry rate
unassisted retry rate
policy version evidence
applied context evidence
hash-chain validation failure
transaction rollback on validation failure
unrelated-run applied pair remains in global retry denominator
```

## Revisions

```text
overall includes every distinct selected revision
trend uses only last non-empty revision per run
definition fallback applies only to empty overall revision set
```

## Storage

```text
analytics never mutates context.sqlite
analytics never persists TEMP schema
canonical migration checksums remain immutable
canonical backup-before-migration remains valid
```

## Repository query pushdown

For every converted `Limit: 100000` caller:

```text
old fixture result == new fixture result
```

Use semantic equality, not only row counts.

Also cover:

```text
legacy IncludeCandidates behavior when Lifecycles is nil
IncludeCandidates plus Lifecycles conflict rejection
unknown lifecycle rejection
OriginRunID exact bound matching
SourceTypes normalization and empty-after-normalization rejection
ordinary Query order remains priority DESC, created_at DESC, id ASC
Iterate order equals unlimited semantic Query fixture order
Iterate callback error, scan error, cancellation, and non-reentrancy documentation
```

---

# 11. Observability additions

Use this internal analytics diagnostics struct for the WP-0 orchestration seam:

```go
type AnalyticsDiagnostics struct {
    ExecutionLinesRead int64
    ExecutionRows      int64
    AuditLinesRead     int64
    AuditRows          int64
    MemoryRows         int64

    SelectedRuns       int
    ProjectedTasks     int

    LoadExecution      time.Duration
    LoadAudit          time.Duration
    LoadMemory         time.Duration
    BuildIndexes       time.Duration
    SelectRuns         time.Duration
    ProjectTasks       time.Duration
    AggregateExecution time.Duration
    AggregateAudit     time.Duration
    AggregateMemory    time.Duration
    AggregateGroups    time.Duration
    AggregateTrend     time.Duration
    Total              time.Duration
}
```

Do not add this to the stable public report unless explicitly required.

Use it for:

- benchmark output;
- debug diagnostics;
- regression investigation.

Diagnostics are observational only: timing collection must not change transaction boundaries, query ordering, error stages, or report values. Counts are assigned only after their corresponding stage succeeds, except loader line/skip counts that already have defined partial-progress semantics.

---

# 12. Implementation order

Recommended sequence:

```text
Phase 0
  WP-0 benchmarks + semantic golden tests

Phase 1
  WP-1 SQL run selection
  WP-2 selected-only task projection
  WP-3 selected_runs relation

Phase 2
  WP-4 set-based trend
  WP-5 index rationalization

Phase 3
  WP-6 batch ingestion
  WP-7 ephemeral PRAGMA benchmark

Phase 4
  WP-8A typed candidate-settlement predicates
  WP-8B streaming true-bulk operations
  WP-8C hybrid-retrieval lineage design (discovery before implementation)

Phase 5
  WP-11A read-only SQLite observability
  WP-9 canonical index review (discovery gate)
  WP-10 optional concurrent read path (discovery gate)
  WP-11B maintenance policy (discovery gate)
```

Each phase should be independently mergeable.

Do not combine all phases into one large PR.

---

# 13. Suggested PR decomposition

## PR 1 — Benchmark and semantic lock

```text
test(improve): add scalable sqlite analytics benchmarks
test(improve): freeze analytics semantic fixtures
```

No behavior changes.

---

## PR 2 — Relational selected-run scope

```text
perf(improve): select recent runs entirely in sqlite
perf(improve): materialize selected_runs temp relation
```

Remove all-run Go materialization where possible.

---

## PR 3 — Selected-only task projection and relational scope conversion

```text
perf(improve): scope task projections to selected runs
perf(improve): replace dynamic run scopes with selected_runs joins
```

Complete WP-2 and WP-3, preserve task semantics, and delete production `runsInClause` usage before PR 4 begins.

---

## PR 4 — Set-based trend aggregation

```text
perf(improve): aggregate trend metrics set-wise
```

Remove per-run repeated metric calls.

---

## PR 5 — Index plan cleanup

```text
perf(improve): rationalize temp analytics indexes
```

Must include benchmark and query-plan evidence in PR description.

---

## PR 6 — Ingestion optimization

```text
perf(improve): batch sqlite telemetry ingestion
```

Only merge if benchmark gate passes.

---

## PR 7 — Ephemeral PRAGMA experiment

```text
perf(improve): evaluate ephemeral sqlite pragma profile
```

Only retain settings that pass the same semantic suite and benchmark gates. A no-change result is a successful experiment outcome.

---

## PR 8A — Typed canonical query pushdown

```text
perf(context): push typed runtime filters into sqlite
```

Implement only WP-8A and its caller matrix.

---

## PR 8B — Streaming canonical bulk reads

```text
perf(context): replace capped bulk queries with streaming iteration
```

Implement the WP-8B iterator contract and initial conversions. Do not include a second connection handle.

---

## PR 8C — Hybrid lineage retrieval

Create only after the WP-8C design note is accepted. The design and production implementation should be separate review units.

---

## PR 9 — Canonical index changes

Create only if WP-9 discovery authorizes a concrete migration. One PR should contain one coherent index decision set with its evidence.

---

## PR 10 — Optional concurrency

Create only if WP-10 profiling and its separate design note both pass:

```text
perf(context): add bounded concurrent sqlite read path
```

---

## PR 11A — Read-only storage inspection

```text
feat(inspect): report canonical sqlite storage diagnostics
```

Implement only the read-only contract. Maintenance is not part of this PR.

---

## PR 11B — Optional maintenance

Create only after the WP-11B discovery/design gate. No automatic runtime maintenance is allowed.

---

# 14. Coding-agent instructions

The coding agent must follow these rules.

## Before modifying code

1. run existing relevant tests;
2. for PR 1, run and retain the existing benchmark results before editing, then record the new selected-scope benchmark baseline after adding the benchmark-only seam;
3. for PR 2 and later, run the new selected-scope benchmark on the unchanged predecessor commit before editing and record the results;
4. inspect `EXPLAIN QUERY PLAN` before modifying indexes.

## During implementation

- preserve public APIs unless a change is justified;
- prefer private/internal refactors;
- preserve deterministic ordering explicitly;
- use bound parameters;
- never interpolate external values as SQL identifiers;
- keep allowed dimension names as closed internal allow-lists;
- do not load raw audit payload into SQLite;
- do not bypass canonical memory hash validation;
- do not introduce a global analytics DB;
- do not introduce DuckDB;
- do not change report semantics to make optimization easier.
- when `Repository` / `ReadOnlyRepository` changes, update every concrete implementation, wrapper, local narrowed interface, and test double in the same PR; use compile failures as a floor, not as the complete impact trace;
- for candidate lifecycle queries, trace normal coordinator completion, worker/shared-memory confirmation, rejection, failure cleanup, unattended execution, and crash-resume paths so SQL pushdown cannot bypass evidence or recovery gates;
- never perform repository mutation, provider calls, or other re-entrant work while an `Iterate` cursor is open.

## After each work package

Run the focused tests while iterating. Before declaring any Go/code PR complete, run each required repository gate separately:

```bash
go test ./...
go vet ./...
golangci-lint run
```

All must complete successfully with no known failures. Documentation-only changes do not require the Go lint gate.

Run benchmark comparison and document:

```text
before
after
delta
alloc delta
dataset
runCount
benchstat confidence/result
query-plan evidence when indexes change
```

---

# 15. Explicit non-goals

This plan does **not** include:

- DuckDB;
- PostgreSQL;
- external analytics services;
- replacing JSONL canonical telemetry;
- changing canonical event-store integrity;
- persistent analytics cache;
- changing memory metric definitions;
- changing report schema;
- changing vector store technology;
- replacing FTS5;
- auto-VACUUM on normal runtime;
- aggressive SQLite tuning without benchmark proof.

---

# 16. Future work intentionally deferred

These may become worthwhile only after the work above.

## Incremental analytics cache

A persistent derived SQLite analytics cache could avoid re-ingesting all historical JSONL.

However it introduces:

- cache invalidation;
- source offset tracking;
- telemetry truncation/rotation handling;
- hash-chain/cache consistency;
- migration/versioning;
- recovery behavior.

Do not implement until the optimized ephemeral pipeline is benchmarked.

## Typed promotion of hot metadata

If JSON expression indexes remain important over time, promote stable metadata such as origin run identity into typed columns through a schema migration.

Do not prematurely normalize all metadata.

## FTS5 storage redesign

External-content/contentless FTS may reduce duplication, but migration and synchronization risk are not justified until DB size proves problematic.

---

# 17. Definition of done

This optimization program is complete when all of the following are true:

- [x] SQLite remains the only canonical relational database.
- [x] `AnalyzeRecent` no longer loads all run summaries into Go merely to choose N runs.
- [x] selected run IDs are represented relationally in TEMP SQLite.
- [x] task projections are built only for selected runs.
- [x] trend aggregation is O(1) query families relative to run count.
- [x] every TEMP index has a recorded retain/remove/replace decision based on query-plan + benchmark evidence.
- [x] analytics ingestion has a measured optimization decision, not an assumption.
- [x] every production `Limit: 100000` path is classified as typed-filter, true-bulk, or explicitly deferred lineage retrieval.
- [x] WP-8A hot candidate-settlement filtering is pushed into typed SQLite predicates.
- [x] WP-8B conversions have no silent 100K truncation.
- [x] the end-to-end selected-scope 100/10K/100K benchmark matrix exists.
- [x] the end-to-end 1M selected-scope stress benchmark exists as an opt-in benchmark.
- [x] semantic golden tests prove report parity.
- [x] canonical SQLite migration and integrity guarantees remain intact.
- [x] no DuckDB dependency is added.
- [x] no raw sensitive telemetry is newly persisted.
- [x] WP-9, WP-10, and WP-11B each have a recorded implement/skip decision; a justified skip satisfies their completion gate.

---

# 18. Expected impact

Most likely impact order:

| Change | Expected value | Risk |
|---|---:|---:|
| selected-only task projection | Very high | Low |
| set-based trend aggregation | Very high | Medium |
| SQL-side run selection | High | Low |
| `selected_runs` TEMP relation | High | Low |
| remove unused analytics indexes | Medium–High | Low |
| typed context SQL pushdown | High | Medium |
| batch INSERT | Medium | Medium |
| analytics PRAGMA tuning | Low–Medium | Low |
| concurrent canonical read pool | Conditional | High |
| persistent analytics cache | Potentially high | High, deferred |

The first optimization target is therefore not “make SQLite faster”.

It is:

> **make Hufu ask SQLite to do less unnecessary work, then let SQLite execute the remaining work set-wise.**
