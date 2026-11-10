# Issue 26 performance acceptance experiment

> Status: draft
> Authority: reference
> Verified-Commit: `cd836428850103d5aaf83b944303d822db6f0a60` (correctness baseline; experiment results pending)
> Supersedes: —
> Superseded-By: —

This is the new experiment protocol for [#26](https://github.com/kjelly/hufu/issues/26),
not a rewrite of the [original failed capture](../archive/analyses/issue26-performance-acceptance-2026-10-30.md).
PR #53 now includes full acceptance, rather than evidence archival alone.

## Comparison identity and the known historical race

Compare original source `8f65a57cca7470aa773a28f7c70bfaa5c6b4f550` with the six-change
endpoint `b3360c9a5804e83934204d8118c5c322e50a94e9`. Each receives **only** the exact
`internal/team/dag_test.go` patch from already-merged fix
`300041b84e45e7c53549bd53c753b6b0ac6ad860` (#46): drain admitted retry workers before
reusing their Todo projection and during cleanup. It preserves retry assertions;
it does not disable race detection, exclude cases, or change production code.

Each is a separately committed, clean measurement variant with the original SHA
as its parent. Record its measured SHA/tree, original SHA, fix SHA and patch hash.
Fixed author/committer metadata makes variants reproducible across runners.
These are **not successful executions of the unchanged original revisions**.
Original commits, failure logs and the old incomplete matrix remain unchanged.

Whole revisions still differ in production and added tests. Their full-suite
timings are workload observations, not a causal estimate of the six changes.
Acceptance requires an explicit removed-test assertion map and complete test
outcome inventories; faster totals cannot replace those checks. The existing
six focused reports retain their own controlled-comparison boundaries.

## Pilot and co-located group correction

The first paired pilot, run `42743586308` at controller `9758ec40`, placed each
sample on a separate runner. Its first three CGO-free samples used EPYC 7763,
9V74 and 9V45 respectively. Although each before/after pair shared its runner,
these are not three repetitions in the same environment. Preserve that complete
pilot and its cost; do not pool different CPU models or select replacement
samples after inspecting their timings.

The acceptance experiment is therefore a separate `issue26-co-located-v2`
protocol: **one job per mode/cache group, all three samples on that same runner**.
Every group alternates before→after, after→before, before→after. Both variants
and every sample share the group's hardware, OS, toolchain, GOMAXPROCS and flags.
Groups on different hardware remain separate; cross-cache or cross-mode ratios
cannot be attributed solely to cache/CGO when their CPUs differ.

## Planned observations

The explicitly dispatched `Issue 26 full-suite evidence` workflow runs nine
jobs: three modes × three cache groups. Each job measures both variants three
times on **one** Ubuntu 24.04 runner; sample two reverses source order. No job retries
a failing sample. Other matrix samples are independent planned observations;
every failure remains part of the experiment and prevents acceptance.

Each source executes full normal, race or CGO-free commands, never `-short`.
Every sample has its own private build cache. Cold runs `-count=1` on an empty
cache. Warm first compiles test packages with `-run '^$' -count=1` and the CLI
with `go build -o /dev/null`, then measures the full `-count=1` execution.
Result-cache samples first execute a separately recorded full unfiltered seed
on their private cache, then measure the identical command's result replay.
Seeds are never counted as measured samples. Compile-only warm seeds are not
correctness evidence and do not suppress any case in the measured full run.
This avoids redundant full-suite warm-up executions while keeping each group
within the six-hour hosted deadline. All seed logs, commands and CPU/wall costs
are retained and source/sample/cache identities remain explicit.
All Go build/test commands use mise's Go 1.27.2,
GOTOOLCHAIN=local, GOMAXPROCS=2 and explicit package parallelism two.

The common package timeout is **30 minutes** on both sources and in every mode.
The previous 20-minute race deadline expired at the package boundary; current
full-suite validation also uses 30 minutes. This increases diagnostic headroom
without shortening a deadline, changing assertions or concealing the old failure.
Job deadlines remain bounded at six hours. Different flags mean this experiment
does not replace old observations or merge samples with them.

Require ripgrep before timing; check actual native grep outcomes rather than
assuming installation implies coverage. Record all other skip reasons and
platform/opt-in boundaries. Record OS, CPU, quota availability, toolchain, CGO,
flags, run/attempt/sample identity, source order and harness hash. Compare the
six observations' environment within each mode/cache group before pooling;
do not pool different groups or infer unlimited CPU from missing
quota metadata. Fresh and cached test outcomes remain distinct evidence.

Private build caches are intentional cold-cache measurement instruments on the
hosted runner's **disk**, removed after each source/repetition. The controller
rejects tmpfs/ramfs and local Codex captures; it never clears shared caches or
changes GOTMPDIR. Do not run this experiment locally by bypassing admission.

## Completion gate

- All nine co-located jobs and both variants succeed; every source/mode/cache group
  has three successful observations, with unmodified raw JSONL/stderr/metadata.
- Report median/range wall, child user/system CPU, package and slow-test timings;
  parent/subtest and package elapsed values must not be summed into wall time.
- Audit full case/outcome inventories, native grep execution, every removed
  top-level case's retained assertions, and required persistence/process/race
  contracts. Additions and remaining workload differences stay explicit.
- Record all-attempt experiment cost separately from correctness CI cost,
  setup/cache-transfer measurements and flake observations. Three passing samples
  are a small observation, not proof that the flake probability is zero.
- Preserve baseline and latest correctness CI, lint and documentation evidence.
  Decide on package splitting/sharding using current evidence, with the old
  October deferral identified as historical.

Raw artifacts are retained for 30 days. Completion requires a committed report
with run/job/artifact identifiers, digests, reproduction and limitations. A
running workflow or a passing parser is not acceptance; #26 stays open and
PR #53 stays draft until the full evidence gate is satisfied.
