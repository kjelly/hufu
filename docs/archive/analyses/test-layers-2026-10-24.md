# Short/integration layering and pure team tests (issue #32)

This report accompanies [issue #32](https://github.com/kjelly/hufu/issues/32).
The initial source baseline was `3caa3170029bd6f52d423de13cca7550882ad91b`,
after shared CLI binary, PTY readiness, and lifecycle teardown changes. During
implementation main merged the fixture optimization in #31; the final PR is
rebased on `276c8cd5b7e4397127023fc9e1f2350bedba2278`. Earlier whole-suite
diagnostics below remain attributed to 3caa3170 and are not presented as a
full regression on the later base. The maintained
[entry points and complete 31-case migration map](../../reference/test-layers.md)
list every moved test. No test assertion, timeout or production behavior is
changed; 14 pure team Test functions gain `t.Parallel()`.

## Environment and comparison limits

Go 1.27.2, Linux amd64 6.18.55, glibc 2.41, Intel Xeon Platinum 8573C,
5 visible CPUs, cgroup quota 400000/100000, GOMAXPROCS=2, `-p 2`.
CGO_ENABLED=1 for normal/race; GOFLAGS=`-mod=readonly`, GOTOOLCHAIN=`local`,
GOTMPDIR unset. Commands run through mise; the preinstalled Go backend supplies
the same pinned golangci-lint 2.14.0. The inherited shared module/build/lint
caches are preserved. This is a managed container, not a GitHub Actions runner.
Local TCP/Unix sockets and PTYs are permitted for the reported executions.

An initial restricted-sandbox attempt could not open local HTTP fixture sockets
and was stopped. It is excluded from comparisons. The first unrestricted
whole-suite attempt overlaps cleanup of that attempt briefly and is diagnostic
failure evidence, not an accepted performance baseline.

Before timings use an immutable `git archive` of the selected baseline under
ignored `work/issue32/base`; after timings use the working tree. The earlier
3caa3170 archive is retained as `work/issue32/base-3caa`. Each source/mode has
one warmup excluded from the three fresh-execution samples. Snapshot paths
require separate compile/link cache entries, so a first warmup is not a cold
cache comparison. Measurements execute sequentially with no simultaneous
heavy validation. No cold-build, result-cache or speedup claim is made.
Raw JSONL, per-command wall/user/system seconds, and environment metadata remain
under ignored `work/issue32/`.

## Earlier whole-suite failures on 3caa3170

The first network-permitted command was:

```bash
mise exec -- go test -p 2 -json -count=1 -timeout 20m -short ./... ./.agent-teams/hufu-dev/diagnostic-pilot/...
```

It exited 1 after 399.015 seconds. Package elapsed times include team 274.820s
(fail), improver 127.521s, CLI 64.774s, evalharness 52.944s (fail). These are
not additive and do not establish an accepted critical-path speedup baseline.
The runtime and existing test bodies were unchanged; only the independent
layer-runner tooling was added at invocation time.

The failures predate the guards and parallel markers:

- `internal/evalharness`: context delivery/query lifecycle regressions and
  `TestRunAllEvalSuites`, including verbatim-dependency and worker-shell-tools.
- `internal/workspace/versionstore`: `TestMaterializeRecoversFromPartialWrite`.
- `internal/team`: `TestProcessSupervisorKillsDescendants`,
  `TestTerminalSession_ProcessGroupKillDescendants`,
  `TestPTYCloseWaitsForFinalizationAndPublishesClosedOnlyAfterRelease`,
  `TestPTYExitCleansDescendantHoldingSlaveBeforeRelease`, and
  `TestTerminalPipeNaturalExitContainsDescendantsBeforeReap`.

The descendant observations are consistent with the environment-sensitive
failures recorded in the [earlier baseline attempt](test-baseline-2026-10-22.md);
this report does not establish their root cause. They remain failures. None of
these cases is newly skipped to obtain passing timings.

## Comparable focused measurements after #31

The final equal-case comparison starts at `276c8cd5` after the #31 merge.
A fresh warmup and three observations per source/mode were collected after
updating the branch; no earlier timing is attributed to this source.

The selected set is exactly the 31 integration cases in the migration map plus
the 14 parallelized tests in prompt, decision_diversity and decision_explain.
Both sources execute all 45 with no short flag. This is a focused equal-case
comparison, not a whole-suite benchmark or proof of a CI critical-path benefit.

For each source, first warm up and then repeat three times, separately in normal
and race mode, with the exact anchored union of those 45 names as `FOCUSED`:

```bash
mise exec -- go test -p 2 -json -count=1 -timeout 20m -run "$FOCUSED" \
  ./cmd/hufu ./internal/team ./internal/context ./internal/decisionrt \
  ./internal/decisionrt/backend/systemone ./internal/decisionrt/control ./internal/modelcatalog
# Repeat the same command with -race before -run for race observations.
```

Each sample has 45 top-level passes, 127 pass events including subtests,
zero failures/skips, and the same observed case-set hash across both sources
and both modes. Warm build cache / fresh test execution, seconds:

| Source/mode | Three command wall observations | Median wall | Median child user+system CPU |
| --- | --- | ---: | ---: |
| baseline/normal | 13.607, 13.505, 13.399 | 13.505 | 23.536 |
| baseline/race | 19.205, 22.107, 19.763 | 19.763 | 33.216 |
| updated/normal | 14.117, 13.132, 17.289 | 14.117 | 23.979 |
| updated/race | 20.640, 22.247, 20.155 | 20.640 | 34.896 |

The normal/race medians do not show a speedup. These observations do not
establish a repeatable performance benefit for
the equal-case set. The purpose of this first batch is valid coverage layering
and safe parallelization, not a claimed whole-suite acceleration.

The actual new integration entry under race passes all 31 moved top-level cases
with zero skips; integration membership is generated from their guards. The 14
new parallel tests plus the four existing parallel team model-selection tests
also pass `-race -shuffle=on -count=20`: 360 top-level executions, no failures
or skips. This exercises all 18 top-level parallel Tests together. The three new #31
fixture contracts, including eight nested parallel database copies, are also
checked separately under race on the final base.
The layer runner's own four Go unit tests also pass under race.


## Earlier regression evidence before incorporating #31

All 15 modified existing test files reproduce the baseline bytes exactly after
removing only the 31 new guard blocks and 14 parallel markers. No assertion,
fixture, cleanup, timeout or import in those files is deleted or rewritten.

The updated full short command exits 1 after 394.686s, with **exactly the same
nine failing top-level tests** as the original diagnostic run. There are no new
failing top-level tests. All 31 moved cases appear as short skips and pass in the
separate integration invocation. Parent/subtest terminal events are 14,795 pass,
13 fail, 56 skip; these are not independent top-level test counts. The original
run has only three runner-tool unit tests versus four in the final tree, and
short intentionally omits integration children, so aggregate event counts must
not be treated as an equal-case performance comparison. Team elapsed is
267.057s; it remains the largest package. No whole-suite improvement is claimed.

Local race validation is focused (the selected 45, the complete current parallel
team batch, and the layer runner). The required CI still covers race across both
complete offline layers and executes the **full** native CGO-free suite. Local
CGO-free validation below is focused and does not substitute for that full gate.

## Final-base revalidation

The final base includes the three additional fixture contract Tests from #31.
Its [fixture report](coordinator-sqlite-fixtures-2026-10-24.md) records a
201.607s post-change full-team diagnostic with the same five descendant-process
failures. Its profiles identify redaction/checkpoint work as larger than schema
migration. That is attributed project evidence, not a new passing full-suite
measurement from this change. It reinforces the decision to keep this batch
small and avoid service extraction or new test shards.

Final-base checks:

- The final integration runner passes all 31 moved top-level Tests / 52 terminal
  events, zero skips, in each of three race invocations. Command wall times are
  32.086s, 22.821s, 23.451s; these include discovery/runner overhead,
  not just test package elapsed time. No comparison with the discarded
  per-package implementation or CI acceleration claim is made.
- The layer runner's four Go unit Tests pass under race, including exact-name
  matching, platform-active/external source discovery, invalid guard rejection,
  combined package execution and collision fallback.
- The 18 top-level parallel team Tests pass 20 shuffled race repetitions:
  360 top-level / 1,700 parent-and-subtest pass events, zero skips. The three
  merged fixture contracts also pass separately under race (16 pass events),
  including their nested parallel copy isolation checks.
- With CGO_ENABLED=0 and no short flag, all 52 selected original, runner and
  fixture top-level Tests pass (156 terminal pass events, zero skips). This is
  actual execution, not a compile-only check.
- The final AST inventory matches `go test -list '^Test'` exactly: 7,582 current
  platform-active Test functions. Every one of the 7,578 original functions on
  276c8cd5 remains present; the only additions are the four runner unit Tests.
  Short owns 7,551 and integration owns 31, with no overlap or removed case.
  Fuzz seeds and examples stay in unfiltered short/full package execution.
- `mise run lint` passes on the final base: **0 issues**. All six Python tooling
  unit tests, workflow YAML validation, `git diff --check` and `bin/check-docs`
  pass as well.

The final-base focused comparison above is independent of the earlier
full-short observations. Full required CI remains the final integration gate.

## CI critical path, total runner time, and cache cost

The four existing race short package groups remain. One required integration
runner executes the guarded cases under race, without a short flag or duplicate
full unit execution. `ci-success` depends on the integration job as well as the
original groups. The native full CGO-free test job, independent build, vet and
eval-core checks remain intact. No top-level test shards are introduced.

The aggregate gate additionally runs `scripts/ci-test-cost.py` with read-only
Actions access. It paginates every run attempt and counts each job ID once,
reporting workflow wall time (including queue/rerun gaps) and total runner
seconds observed so far. This avoids treating only the latest successful
rerun records as total cost. The aggregate's remaining runtime/cleanup is
explicitly excluded. Six Python tooling unit tests pass, including concurrent
job duration accounting, duplicate IDs, pagination and multiple attempts.

Every short group and the integration job upload active Test inventories,
raw test JSONL, wall/user/system seconds, and before/after Go build-cache KiB.
The extra integration runner restores a separate cache and repeats compilation
and linking for its selected packages. Cache disk size is not a hit ratio or
restore/save cost. setup-go logs record transfers; job start/end timestamps
supply runner durations.

Historical hosted-run context is available from
[CI 40079485734](https://github.com/kjelly/hufu/actions/runs/40079485734) on
PR #39 head `9a00c5dba3a66fdeb8978af90d0f922be584397b` (not this comparison's
exact source SHA). All latest job conclusions are success. Decoded log spans
show:

| Historical observation | Seconds |
| --- | ---: |
| Longest latest job: team race, including setup/cleanup log span | 899.178 |
| Sum of the 11 latest job log spans | 3,732.454 |
| Earliest job log through aggregate completion | 5,356.287 |

The team log starts well after the other test jobs: this run includes a rerun
and idle/queue gaps. These are approximate log spans, not exact billed runner
durations; the sum excludes earlier failed attempts, and the observed workflow
span is not an uninterrupted critical path. They cannot establish a speedup.
All nine setup-go cache restores in these latest logs report HTTP 400 and
cache-not-found; the lint cache restore also misses. Lint reports a 222ms save,
but Go cache transfer sizes/times are not established. The additional integration
runner must therefore be budgeted for possible cold compilation as well.

There is no completed same-runner before/after GitHub Actions measurement at
local implementation time. This change's effect on CI critical-path wall time,
total runner seconds, and cache transfer cost is **unmeasured**, not zero or improved.
To compare completed workflows, report workflow elapsed time and the sum of all
job durations, including the additional runner and unchanged gates, along with
cache restore/save duration and size. Moving work alone does not save CPU.
The local original run still identifies team as the largest package; this small
batch does not justify extracting services or introducing test shards.
