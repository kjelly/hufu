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
| Observed workflow wall seconds, including queue | 1,104.418 | 1,188.503 |
| Observed total runner seconds, all attempts | 4,698.418 | 4,677.503 |
| Team package elapsed seconds | 941.5 | 997.757 |
| Team command wall seconds | 1,056.9 | 1,142.61 |
| Team cache KiB before → after | 60,076 → 888,376 | 60,076 → 888,392 |
| Team package timeout usage | 78.5% of 20m | 55.4% of 30m |

The baseline package/command/cache observations are from the issue's artifact
analysis; aggregate numbers were independently read from its ci-success log.
The after column is [PR #45 run 40104002522](https://github.com/kjelly/hufu/actions/runs/40104002522),
head `b062304c`, with all 12 jobs passing on attempt 1. Workflow wall time is
longer and total runner time is approximately unchanged; this single comparison
establishes deadline headroom, not a speedup. Sum both short and integration
costs when comparing case coverage and runner time.

Several consecutive **main push** runs after merge must pass without manual
reruns, with the team ratio below the 75% target. A successful PR workflow is
necessary review evidence but does not establish that acceptance condition.
Do not manufacture main pushes or close the observation gap with retries.


## Main push follow-up: retry fixture lifetime

After #45 merged, [main push 40107449483](https://github.com/kjelly/hufu/actions/runs/40107449483)
on `b10f57a9` failed on attempt 1. Team elapsed was 997.715s / 1800s (55.4%),
command wall 1143.14s, and build cache 60,076 → 888,484 KiB. The failure was a
race between `routeCriterionRetry` reading a target's `Retries` and
`TryApplyProjectedItem` copying the worker's final projection. It was attributed
to `TestDAGSchedulerRoutesSuccessfulNoProgressWithBoundedBudget`, but the two
racing goroutines originated in the preceding
`TestCriterionRetryRoutingHonorsMaxRetriesAfterBudgetReset` fixture.

The routing unit fixtures admit real task goroutines. They previously returned
without draining those tasks; one fixture also marked its target done and
attempted another route before the admitted worker's final projection finished.
The fix waits for that completion before simulating the next routing decision,
registers stop-and-drain cleanup even when an assertion exits early, and releases
the cancelled fixture's scheduler bookkeeping when receiving its final event.
Retry budgets, per-task retry assertions, worker execution and race detection
remain covered. Production scheduling and Todo projection code are unchanged.
The unmodified main reproduces the race in the focused 100-repeat run. The three
fixed fixtures pass 1,000 shuffled race repetitions each, with no skips.

The same failed main run reports `setup_go_cache_hit=false`. This is not evidence
of another HTTP 400: [cache scope](https://github.com/actions/cache#cache-scopes)
includes the branch, so a cache written by a PR is not a default-branch seed.
The pinned [setup-go action](https://github.com/actions/setup-go/blob/40f1582b2485089dde7abd97c1529aa768e1baff/action.yml)
saves only under `post-if: success()`; this run's team post step was skipped after
the test failure. Its integration job succeeded and saved the main race cache.
The next successful main team run can seed its own cache; a later main push
must verify a restore hit. No cache-hit or consecutive-main-success claim is
made from the focused fixture tests.
