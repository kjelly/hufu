# Team race timeout headroom (issue #42)

This report accompanies [issue #42](https://github.com/kjelly/hufu/issues/42),
on implementation base `b3360c9a5804e83934204d8118c5c322e50a94e9`.
The maintained [test-layer reference](../../reference/test-layers.md) contains
the complete six-case migration map and replacement execution entry points.

## Changes and limits

The team race short package deadline is 30m, with a 40m job limit. The other
three short groups retain 20m package deadlines with 30m job limits. Integration
retains 20m package deadlines and has a 40m job limit. The runner still uses
`-p 2`, `-count=1`, `-race`, and `-short` only for the short layer. Additional
flags already supported overriding the default deadline; a regression test
checks the final timeout alongside race/fresh-execution flags.

Six reviewed workflow/persistence cases move from short into the existing
required race integration job, which discovers first-statement whole-test short
guards. Their original source bytes are unchanged after removing only the six
new guard blocks. No volume, assertion, timeout or cleanup inside those cases
is reduced. The full native CGO-free job continues to execute all cases without
`-short`; independent build, vet and eval-core gate commands are unchanged.

Every race scope and integration writes `package-budget.json`, reports package
terminal elapsed/deadline ratios in its summary and emits a warning above 75%.
Failed/incomplete discovery and truncated evidence cannot be mistaken for a
zero-second successful package. The reporter keeps the original test exit
status, JSONL, command wall/user/system seconds and cache KiB measurements.

The pinned setup-go v5.0.0 used `@actions/cache ^3.2.1`; the updated v5.6.0
(`40f1582b2485089dde7abd97c1529aa768e1baff`) uses `@actions/cache ^4.0.3`.
This replaces the client predating the cache service migration. Race scopes
also use distinct build-cache paths, which participate in cache archive version
hashing. A non-race job or another scope can no longer save the first immutable
cache for that race scope. Go module-cache paths are unchanged. A first run is
expected to miss; cache-hit output and setup-go transfer logs must confirm
subsequent behavior. The client update alone does not establish that every
HTTP 400 was caused by the old client or that race compilation is fully cached.

This change does not optimize shared Coordinator, SQLite, checkpoint or redaction
production code, introduce package shards, or add fixture concurrency. Existing
[fixture profiling](coordinator-sqlite-fixtures-2026-10-24.md) attributes more
cost to redaction/checkpoint work than schema migration. That remains the next
profiling target if the team group stays on the critical path. Moving six cases
is a reduction in the short job's executed workload, not evidence of lower
total CPU or workflow cost.

## Source and local validation

The baseline inventory was generated from an immutable `git archive` of b3360c9a;
the updated inventory uses the working tree. Both use mise's pinned Go 1.27.2
and the same package universe. Before: 7,551 short + 31 integration = 7,582
top-level Tests. After: 7,546 short + 37 integration = 7,583. The only added
Test is the runner timeout regression. Exactly the six documented team cases
move to integration, with zero removed cases and zero overlapping membership.

Local evidence is retained under ignored `work/issue42/`. Shared build/module/lint
caches are preserved; commands execute sequentially with GOMAXPROCS=2 and `-p 2`.
The local container is not a GitHub-hosted runner. Compilation is included in
command wall time; the first race invocation may be cold for a test binary.
These runs establish coverage and behavior, not a same-cache speedup comparison.

## Hosted CI comparison and outstanding acceptance

The last successful main baseline is
[run 40099502902](https://github.com/kjelly/hufu/actions/runs/40099502902)
on b3360c9a. Its aggregate job reports one attempt, 12 job records, and one job
still running (the aggregate itself) at observation time. Its remaining cleanup
is excluded from the aggregate measurements.

| Metric | Before: main b3360c9a | After |
| --- | ---: | --- |
| Observed workflow wall seconds, including queue | 1,104.418 | Pending completed PR workflow |
| Observed total runner seconds, all attempts | 4,698.418 | Pending completed PR workflow |
| Team package elapsed seconds | 941.5 | Pending `short-team` artifact |
| Team command wall seconds | 1,056.9 | Pending `short-team` artifact |
| Team cache KiB before → after | 60,076 → 888,376 | Pending `short-team` artifact |
| Team package timeout usage | 78.5% of 20m | Reported against 30m in summary/artifact |

The baseline package/command/cache observations are from the issue's artifact
analysis; aggregate numbers were independently read from its ci-success log.
No post-change hosted speedup or cache hit is claimed before completed evidence
exists. Sum both short and integration costs when comparing case coverage and
runner time; relocation can increase repeated compilation or setup costs.

Several consecutive **main push** runs after merge must pass without manual
reruns, with the team ratio below the 75% target. A successful PR workflow is
necessary review evidence but does not establish that acceptance condition.
Do not manufacture main pushes or close the observation gap with retries.
