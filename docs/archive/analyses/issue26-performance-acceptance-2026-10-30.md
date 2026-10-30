# Issue #26 performance acceptance — 2026-10-30

**Acceptance is incomplete. Do not close #26 or publish an overall speedup.**
This report distinguishes completed observations, historical focused comparisons,
and the new full-suite capture. No missing observation is estimated.

## Sources and immutable comparison boundaries

| Role | Source SHA | Interpretation |
| --- | --- | --- |
| Original issue reference | `8f65a57cca7470aa773a28f7c70bfaa5c6b4f550` | Original workload; predates the harness. |
| All six implementations merged | `b3360c9a5804e83934204d8118c5c322e50a94e9` | #32 merge; includes intervening runtime changes. |
| Main inspected for this acceptance | `4e13abfb91a1328ccc1c58d0fd4a7861b17d6d43` | Includes additional runtime changes and #49 test consolidation. |
| Measurement controller | `58efa61e6891be82a3bf1ccf56d6b43028a3cd2f` | Original harness bytes; new opt-in workflow only. |

The original reference and #32 endpoint already differ in 250 files under
`cmd/`, `internal/`, `go.mod` and `go.sum` (14,669 inserted / 2,420 deleted lines;
the module files themselves do not change). These are whole-revision
observations, not a causal estimate of the six optimizations. Production changes
and added/removed tests must be reconciled separately. The later #49
consolidation is deliberately outside the six-change endpoint.

On initial inspection, the original **Test performance baseline** workflow had
zero runs (workflow-specific API and Actions UI). Existing passing correctness
runs cannot fill its 3 × 3 mode/cache acceptance matrix.

## New capture and reproducibility

- Evidence PR: [#53](https://github.com/kjelly/hufu/pull/53).
- Full-suite run: [41046691306](https://github.com/kjelly/hufu/actions/runs/41046691306).
- Controller: [.github/workflows/test-baseline-issue26.yml](https://github.com/kjelly/hufu/blob/58efa61e6891be82a3bf1ccf56d6b43028a3cd2f/.github/workflows/test-baseline-issue26.yml).
- Harness: [test-baseline.py](https://github.com/kjelly/hufu/blob/4e13abfb91a1328ccc1c58d0fd4a7861b17d6d43/scripts/test-baseline.py).

The existing workflow uses short normal/race and full CGO-free. To cross #32
without excluding integration, this opted-in run uses `--full` in **all modes**.
Normal, race and CGO-free run on distinct Ubuntu 24.04 jobs. Within each mode,
the three immutable source worktrees run sequentially on the **same runner**.
Go is pinned to 1.27.2 through mise 2026.10.23; GOMAXPROCS=2,
GOTOOLCHAIN=local, GOFLAGS=-mod=readonly, native CGO=1 (CGO-free overrides to 0).
The Go default package parallelism follows GOMAXPROCS; no hidden test filter or
extra build instrumentation is enabled. Runner hardware/quotas remain in each
`environment.json`; different modes must not be compared as identical hardware.

The harness is imported without changing its bytes; only its `ROOT` variable
points at the selected clean worktree. No historical Go source, tests, assertions,
module files or toolchain declarations are overlaid. `dirty=false` is required.
Module downloads occur before timing and their diagnostics are retained.
Each source/mode requests three repetitions of cold, warm/fresh, result-seed,
then result-cache execution. Seeds are retained but excluded from reported
three-sample groups. Cold means private **Go build cache**, not cold module,
filesystem or OS page caches. No shared cache is cleared or GOTMPDIR changed.
There are no automatic test retries. A failed command ends that source's
repetitions; measuring another source never replaces that failure.

Equivalent invocation, from the pinned mise controller checkout, for a clean
historical worktree at `/tmp/h26-before`:

```bash
git worktree add --detach /tmp/h26-before 8f65a57cca7470aa773a28f7c70bfaa5c6b4f550
mise exec -- python3 - <<'PY'
import importlib.util
from pathlib import Path
import subprocess
import sys
spec = importlib.util.spec_from_file_location('baseline', 'scripts/test-baseline.py')
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)
module.ROOT = Path('/tmp/h26-before')
subprocess.run(['go', 'mod', 'download'], cwd=module.ROOT, check=True)
sys.argv = ['test-baseline.py', '--output', 'work/before-normal',
            '--mode', 'normal', '--cache-state', 'all', '--repetitions', '3', '--full']
raise SystemExit(module.main())
PY
```

Repeat separately with `--mode race` / `cgo-free`, distinct output directories,
and each pinned source. The workflow records the harness SHA-256, source SHA,
mode, run ID/attempt and source-specific exit status in `capture.json`.
Full JSONL contains case identities, outcomes, skips and package/test timings;
observation JSON contains monotonic wall time and child user/system CPU seconds.
Do not add package or parent/subtest elapsed times to derive wall time.

## Terminal hosted capture — verified 2026-10-31 (Asia/Taipei)

**Capture ended; acceptance remains incomplete.** Run 41046691306 has one attempt, two successful jobs (normal and CGO-free) and a failed race job. No rerun was performed. All three original artifact ZIPs were downloaded and their published SHA-256 digests verified. The run finished at 2026-10-30 23:43:59 Asia/Taipei.

The **unaltered three GitHub artifact ZIPs**, source/capture metadata, all raw JSONL/stderr/module logs inside those ZIPs, observation/summary JSON, run/jobs API snapshots, a SHA-256 manifest and the analysis script/derived analysis.json have been preserved in a separate evidence package delivered to the requester. The earlier local/correctness evidence package is preserved separately. Public repository readers can retrieve the original artifacts from the [Actions run](https://github.com/kjelly/hufu/actions/runs/41046691306), using the IDs/digests below; original GitHub artifacts expire on 2026-11-13 UTC. No private delivery link is published here.

| Original artifact | ID / published SHA-256 |
| --- | --- |
| issue26-full-race-attempt-1 | 12989923186 / `271c51b9bb763daf1430c1bbeefb41474af6808670113994a87f2cb0cbbd7ef9` |
| issue26-full-normal-attempt-1 | 12984623919 / `8ce542170d165ab1fdb3c06a6cc02c24dc36ca3e73ca99fd41efc407ab3eb989` |
| issue26-full-cgo-free-attempt-1 | 12980386061 / `99d69a927f7ce9d4dcd7dc1bb9314803ba0fdfa98edf84683496729a089ce86b` |

### Three successful samples within each source/mode/cache group

Seconds: monotonic whole-command wall median (min–max); child user and system CPU medians are reported independently. Every row below contains exactly three successful observations. These describe each revision’s own workload; **they are not an equal-case before/after speedup or accepted full-contract coverage**. Cold/warm use -count=1. Result rows replay 63/65/67 packages respectively and still execute uncached packages. Seed timings remain in the raw evidence and are excluded from these medians.

| Mode | Source | Cache | Wall median (min–max), s | User / system CPU median, s | Cached packages in each sample |
| --- | --- | --- | ---: | ---: | --- |
| normal | before | cold | 502.224 (500.917–514.728) | 882.832 / 127.845 | 0,0,0 |
| normal | before | warm | 339.960 (337.943–341.079) | 457.491 / 80.836 | 0,0,0 |
| normal | before | result | 105.697 (105.438–105.891) | 103.661 / 29.477 | 63,63,63 |
| normal | after-32 | cold | 483.826 (482.138–484.388) | 851.273 / 124.032 | 0,0,0 |
| normal | after-32 | warm | 312.112 (309.662–312.987) | 420.369 / 79.109 | 0,0,0 |
| normal | after-32 | result | 50.183 (50.071–50.601) | 50.189 / 22.578 | 65,65,65 |
| normal | current-main | cold | 549.991 (545.625–555.742) | 929.079 / 120.739 | 0,0,0 |
| normal | current-main | warm | 370.030 (361.854–371.704) | 471.215 / 71.358 | 0,0,0 |
| normal | current-main | result | 58.492 (57.912–59.168) | 60.522 / 24.053 | 67,67,67 |
| cgo-free | before | cold | 334.056 (333.183–340.414) | 506.925 / 79.932 | 0,0,0 |
| cgo-free | before | warm | 229.580 (227.780–232.516) | 251.025 / 49.791 | 0,0,0 |
| cgo-free | before | result | 68.371 (67.233–68.609) | 60.679 / 20.159 | 63,63,63 |
| cgo-free | after-32 | cold | 316.782 (315.618–322.598) | 489.950 / 79.140 | 0,0,0 |
| cgo-free | after-32 | warm | 206.101 (204.144–215.906) | 222.529 / 47.619 | 0,0,0 |
| cgo-free | after-32 | result | 30.949 (30.775–31.204) | 28.489 / 15.412 | 65,65,65 |
| cgo-free | current-main | cold | 364.111 (355.280–364.356) | 524.992 / 76.034 | 0,0,0 |
| cgo-free | current-main | warm | 250.287 (245.904–252.664) | 249.152 / 44.335 | 0,0,0 |
| cgo-free | current-main | result | 35.676 (35.343–37.952) | 33.541 / 16.750 | 67,67,67 |

### Race failures and incomplete groups

No race group has three successful observations. Do not use the harness’s partial summary medians as acceptance results. Failed package commands can have zero terminal **test** fail events: the process exit status, package failure, panic and race diagnostics control success.

| Source | Captured successful samples: cold / warm / result | Failure retained |
| --- | --- | --- |
| before | 2 / 2 / 1 | `result-seed-2`: exit 1, wall 1805.924793s; team package failed after 1160.369s. `WARNING: DATA RACE` shows `TodoList.TryApplyProjectedItem` (`status.go:1473`) writing while `dagScheduler.routeCriterionRetry` (`dag_scheduler.go:569`) reads; the reader originated in `TestCriterionRetryRoutingHonorsMaxRetriesAfterBudgetReset` (`dag_test.go:342`). Attribution by the currently active JSON Test name alone would be wrong. |
| after-32 | 0 / 0 / 0 | `cold-1`: exit 1, wall 2145.762881s; team package timeout 20m, package elapsed 1200.242s. Active test: `TestWP6SavedSessionMemoryRecallsAcrossForkAtWorkerScope`; this does not prove that test alone caused the package deadline. |
| current-main | 0 / 0 / 0 | `cold-1`: exit 1, wall 2111.497284s; team package timeout 20m, package elapsed 1200.208s. Active test: `TestBranchSwitchCommitsAllOrNothing/fork`; no deadlock conclusion follows from that active frame alone. |

The capture stops each source at its first failed command. There are 81 measured/seed commands altogether: 78 exit zero and three fail. The normal/CGO-free modes each have 36 passing commands, including seeds and partial result-cache replays. This is an observed outcome count, not a statistically defensible flake probability; the historical race is a real safety finding, not a reason for a blind retry.

### Environment, inventory and coverage reconciliation

All source worktrees report dirty=false, Go 1.27.2, Linux 7.0.0-1017-azure / glibc 2.39, four logical CPUs, configured/effective GOMAXPROCS=2, GOTOOLCHAIN=local, GOFLAGS=-mod=readonly and unset GOTMPDIR. CGO=1 in normal/race and 0 in CGO-free. Normal/race jobs used AMD EPYC 7763; CGO-free used AMD EPYC 9V45. Each source within a mode stayed on that mode’s one runner. cpu.max was unavailable (`null`), so CPU quota is **unknown**, not unlimited or equal to logical CPU count. Cross-mode timing comparisons are confounded by CPU model.

| Successful mode workload | Top-level Test/Example/Fuzz terminal identities | Ordinary top-level skips | Normalized nested identities |
| --- | ---: | ---: | ---: |
| before (normal and CGO-free) | 7436 | 27 | 14422 |
| after-32 (normal and CGO-free) | 7590 | 27 | 14901 |
| current-main (normal and CGO-free) | 7676 | 26 | 15668 |

Top-level inventories and outcomes are stable across the 12 commands per successful source/mode. Raw nested case hashes vary because three fixtures embed ephemeral values in subtest names. Audit normalization is limited to loopback URL ports in `TestWarmModelProfilesNoNetAllowsLoopbackAndRejectsRemote`, a trailing 64-hex object hash in `TestReviewBundleIndependentReadOnlyRoundTrip/tamper_objects`, and the temporary-directory numeric suffix in `TestIngestActionProviderArtifactsRejectsMissingAndEscapedPaths/.../001/stable.json`. With only those substitutions, the complete `(package, test, terminal outcome)` set has one hash across all 12 commands in each source/mode. The raw names/outcomes are retained; none is deleted. Different revisions still have different sets and assertions.

Before→after-32: 7,407 shared top-level identities, 29 removed and 183 added. Before→current-main: 7,176 shared, 260 removed and 500 added. Full name lists are in analysis.json. The published #28 five-duplicate and #29 PTY replacement maps explain specific intentional changes; #32 moved cases execute without -short, but those maps do not establish assertion equivalence for every unrelated addition/removal or the later #49 consolidation. No same-case filtered full-suite measurement was collected.

All six passing source/mode suites still skip the four native grep-selection/policy Tests because ripgrep is unavailable. That required environment-dependent coverage is **unexecuted**, despite exit zero. Other skips retain their raw reasons: opt-in live Codex, SQLite discovery, operator performance, interactive readline, real tmux, sidecar-dependent skill cases and a subprocess-helper entry. Before/after-32 also skip `TestBaselineContract_PerWorkerRecallNotImplemented`; current-main removes that skipped placeholder. Therefore fewer skips cannot be called improved coverage without its replacement contract proof.

### Slow packages and tests

Warm/fresh package elapsed median in seconds (three successful samples); values overlap and must not be summed into command wall time.

| Mode/source | team | CLI | improver | tools | evalharness |
| --- | ---: | ---: | ---: | ---: | ---: |
| normal/before | 183.403 | 115.678 | 103.667 | 47.132 | 39.943 |
| normal/after-32 | 201.931 | 50.492 | 104.780 | 47.120 | 42.141 |
| normal/current-main | 229.894 | 59.283 | 99.819 | 47.210 | 42.725 |
| cgo-free/before | 124.795 | 73.683 | 57.553 | 46.455 | 25.349 |
| cgo-free/after-32 | 133.770 | 29.606 | 56.413 | 46.560 | 26.446 |
| cgo-free/current-main | 152.368 | 35.461 | 50.835 | 46.456 | outside top five |

The warm normal slow-test samples are led by `TestReviewBundleSyntheticLessonAndInvalidEvidence` (before 37.78–38.51s; after-32 34.64–34.93s; current-main 30.78–32.66s). CGO-free current-main instead includes `TestManagedMCPTemporaryDirectoriesCleanFailedInitializationAndDuplicate` at about 30.11s. Parent/subtest events overlap; these are diagnostic events, not additive cost contributions. Exact per-package distributions and slow events are preserved in analysis.json and raw observation JSON.

### All-attempt runner cost and capture critical path

The Actions API reports **one attempt** and three distinct completed jobs; all are included, including the failed race job. Job duration sum is **35,345 runner seconds** (9.818 runner-hours). Earliest job start to last completion is **16,149 seconds**; run creation to last job completion is **16,151 seconds**, including the initial queue gap. The race job is the capture critical path. This is the cost of the measurement experiment, not a before/after PR-CI cost improvement.

| Capture job | Runner seconds | Toolchain setup step | Raw artifact upload step |
| --- | ---: | ---: | ---: |
| full-suite (cgo-free) | 7696 | 4s | 5s |
| full-suite (race) | 16148 | 3s | 4s |
| full-suite (normal) | 11501 | 4s | 7s |

No Actions Go build-cache restore/save is configured; every cold repetition uses a private build cache. Toolchain and module downloads occur before timing. Their network bytes/cost and module download durations are not separately instrumented; cache transfer savings are therefore unmeasured. The setup/upload step spans above include their action overhead and are not pure transfer measurements. Child CPU seconds belong to individual go-test process trees, not total runner CPU utilization or billed CPU time.

### Terminal acceptance decision

Keep #26 **open** and PR #53 **draft**. Required missing evidence is: (1) three successful race samples for every source/cache group, with the historical data race and both deadline failures resolved through an explicitly reviewed measurement approach; (2) equal-case timing or full assertion-preserving workload reconciliation across all revisions; (3) actual execution of the four missing-ripgrep contracts; (4) comparable repeated correctness-CI critical-path/cache-transfer costs and sufficient flake observation. No Go source/test, timeout, assertion or measured historical commit was changed to manufacture these results.

Retain the decision to defer team package splitting and additional sharding. Team remains a large package, but splitting it cannot establish correctness or cure the observed historical race. Shards may duplicate setup, compilation, caches and shared CLI fixture builds; current evidence does not justify that architectural change. The full capture is now archived and its terminal report submitted; further polling is unnecessary.

## Completed local original-revision failure observation

An independent full normal capture ran once on unchanged `8f65a57c` using the
same original harness with `--full`; it stopped at the first unsuccessful cold
command. This is **failure evidence**, not one of three successful samples.

| Item | Observed value |
| --- | --- |
| Wall / exit | 397.686162 seconds / 1 |
| Child user / system CPU | 634.796635 / 95.138088 seconds |
| Environment | Linux 6.18.57, glibc 2.39, AMD EPYC 9V74, 9 visible CPUs, cgroup 800000/100000 |
| Go / GOMAXPROCS | 1.27.2 / configured and effective 2 |
| CGO / GOFLAGS / GOTMPDIR | 1 / empty / unset |
| Source dirty | false |
| Terminal test events | 14,303 pass; 93 fail; 26 skip (parents/subtests overlap) |
| Failed packages | audit, evalharness, tools, team |

Examples include `TestNewAuditLoggerInvalidDir` expecting a directory permission
error while this container runs as root, and terminal containment diagnostics
reporting missing `/proc/<pid>/stat` for spawned processes. Those are observed
diagnostics, not an established root cause for every failure. This managed
container is not the hosted runner; its measurements are not mixed with Actions.
No retry, timeout change, assertion relaxation or excluded failure was used.
Warm/result/race/CGO-free local groups remain unmeasured after this failed baseline.

The earlier separately delivered raw evidence package
preserves this local capture and the five original correctness artifact ZIPs
audited below, with per-file SHA-256 and byte sizes. Private Go build caches are
excluded. It does not contain the hosted capture; that complete evidence is archived separately above.

## Existing focused comparisons (not new full-suite samples)

All figures below are attributed to the linked original reports. They are not
added together or substituted for this acceptance's complete matrix.

| Issue | Focused wall median before → after, seconds | Scope / comparability |
| --- | --- | --- |
| [#27](cli-test-binary-2026-10-22.md) | normal 59.559 → 15.716 | Controlled helper restoration; 31 same top-level tests; nested builds 15 → 1; new helper regression tests separate. |
| [#28](duplicate-tests-2026-10-22.md) | normal 2.341 → 2.262; race 9.215 → 7.865 | Fixture families shrink 24 → 19 top-level tests; five exact duplicate assertion maps retained; not equal-case timing. |
| [#29](pty-tests-2026-10-22.md) | normal 9.306 → 6.040; race 11.235 → 7.855 | PTY cases reorganized and rendering matrix moved; replacement/fault-injection map documents assertions; not equal-case timing. |
| [#30](issue-30-lifecycle-cleanup.md) | normal 11.976 → 5.600 | Same 158 top-level tests / 207 executed names; 66 actual fixed 100ms waits removed; no full-suite/race acceleration estimate. |
| [#31](coordinator-sqlite-fixtures-2026-10-24.md) | normal 5.859 → 4.858; race 37.396 → 21.877 | Frozen 55 top-level tests, 61 same run events, no skips; diagnostic profiles separate. |
| [#32](test-layers-2026-10-24.md) | normal 13.505 → 14.117; race 19.763 → 20.640 | Frozen 45 top-level tests, 127 same terminal events; **no observed median speedup**. |

The #28 map retains error→done rejection plus Query/SearchExact/SearchLexical/
Vector empty-agent isolation. The #29 map retains real PTY readiness, signals,
alternate-screen entry/restore, post-startup resize, provider diagnostics and
HOME/CWD isolation. #31 retains real migrations, private SQLite copies,
checksum/durability/locking and cleanup contracts. These published maps are
available evidence; they do not reconcile every unrelated later test change.

## Correctness coverage and cost audit

Raw [CI 40394773615](https://github.com/kjelly/hufu/actions/runs/40394773615)
artifacts at `c8c52d30` were downloaded and parsed for this report:

| Artifact | ID | Command wall / child user+system seconds | Cache KiB before → after |
| --- | --- | --- | --- |
| short-team | 12687296479 | 907.24 / 836.02 | 897248 → 1086528 |
| short-improver | 12687446029 | 891.00 / 930.14 | 824632 → 939992 |
| short-cli | 12685238207 | 369.95 / 424.90 | 1661716 → 1992064 |
| short-other | 12686950928 | 386.62 / 552.11 | 992768 → 1226448 |
| integration | 12686845566 | 221.17 / 357.04 | 1839876 → 2343548 |

The five commands consumed 3,100.21 child CPU seconds in total. This excludes
setup, compilation outside each timed command, other gates, cache transfers and
runner idle time; it is not runner billing or total CPU utilization. Cache disk
growth is not a hit ratio or transfer duration.

The common inventory lists 7,557 short and 37 integration top-level Tests.
Across all four short shards: 7,531 inventory Tests pass, 26 inventory Tests
skip; another eight Example/Fuzz entries pass outside the Test-only inventory.
All 37 integration Tests pass with zero skips and no missing/extra inventory
members. The short runs also skip those integration Tests as intended. Every
inventory Test has a terminal result; **skip does not mean covered**.
The 26 ordinary skips comprise opt-in SQLite discovery, live Codex, operator
performance, interactive readline, real tmux, sidecar-only skill cases, a helper
process entry and four grep cases requiring missing ripgrep. The latter are an
explicit environment-dependent coverage gap, not evidence of successful native
grep-policy execution. Review them again in the full-suite artifacts.

Slow race packages in that single correctness run: improver 858.547s, team
857.409s, CLI 316.976s, evalharness 255.691s, context 145.169s. Package times
overlap and do not define the workflow critical path by addition.

| Hosted observation | All-attempt cost / critical-path evidence |
| --- | --- |
| [#32 merge CI 40099502902](https://github.com/kjelly/hufu/actions/runs/40099502902/job/120892396731) | Aggregate reporter: 1 attempt, 12 jobs; 4698.417964 runner seconds and 1104.417964 workflow wall seconds **so far**, with aggregate job still running. |
| [Current main CI 40697977032](https://github.com/kjelly/hufu/actions/runs/40697977032) | 1 attempt; all 12 jobs successful. Completed job durations sum to 4458 seconds; earliest job start to final completion spans 1284 seconds (queue before first start excluded). Team job 1274s, improver 1015s. |

The current main aggregate's own reporter independently records 4456.321865
runner seconds and 1285.321865 workflow seconds so far; its remaining cleanup
is excluded. These differently bounded observations must not be equated.
The original reference's [CI 39715792891](https://github.com/kjelly/hufu/actions/runs/39715792891)
failed its race and docs gates, so it is not a passing before comparison.
One workflow observation per source, different workloads, cache states and
possibly CPUs cannot establish a CI cost improvement or flake rate.

## Decision and remaining acceptance evidence

Do not split `internal/team` or add test-level sharding based on these data.
The existing four package groups already isolate team and improver; both can
dominate. Further sharding might reduce waits but duplicate CLI/fixture setup,
compilation and caches. A package split is an architectural change; profiles
already identify redaction/checkpoint work beyond SQLite setup. First obtain
passing comparable full-suite samples and classify failure/skip causes.

- [ ] Three successful samples for every source × mode × cache group, with
  raw artifacts, median/range wall and child CPU, package/slow-test summaries.
- [ ] Reconcile observed test identities, outcomes, skipped prerequisites and
  all changed assertions; full-suite time alone is not equal-case acceleration.
- [ ] Resolve immutable-reference failures without patching the measured
  historical source or silently retrying; preserve every failed attempt.
- [ ] Capture complete all-attempt runner cost, setup/cache transfer cost,
  comparable workflow critical paths and enough repetitions to assess flakes.
- [ ] Revisit sharding only after those measurements; do not mark #26 complete
  from this workflow's existence, focused improvements or correctness success.

## Controller validation

- Existing Python tooling/evidence parser suite: 16 tests pass.
- Workflow YAML, Bash commands, inline Python and documented Python syntax pass.
- `mise run lint`: passes, 0 issues (Go 1.27.2, golangci-lint 2.14.0).
- `bin/check-docs`: 87 active / 177 total files, 562 relative links and 38 Go
  function anchors valid; `git diff --check` passes.
- Go production and test files remain unchanged. Lint ran only after the local
  measurement stopped; no heavyweight local validation competed with timings.

## PR correctness CI follow-up

[CI 41047921497](https://github.com/kjelly/hufu/actions/runs/41047921497)
checks merge commit `5e2739a3b9c7dfc74d72c50b73d0d62cd4b604c5`, which merges
this PR into advanced main `2100b1b227f27ecc11790b49b8fe8915920e4f9a`.
That is distinct from the deliberately pinned measurement source `4e13abfb`.

- [Docs job](https://github.com/kjelly/hufu/actions/runs/41047921497/job/124037133348)
  rejects the newly added `docs/reference/runtime-string-contracts.md` for a
  missing lifecycle header. The follow-up adds its five required fields; the
  complete documentation check passes locally (88 active / 178 total files).
- [CGO-free job](https://github.com/kjelly/hufu/actions/runs/41047921497/job/124037133363)
  fails four code-review team fixtures: `TestHufuCodeReviewRuntimeOwnsAll51GapIdentities`,
  `TestHufuCodeReviewEmptyCritiqueOwnsSource`,
  `TestHufuCodeReviewCharacterizesPromptCannotRewriteStaticScope` and
  `TestHufuCodeReviewSynthesisReceivesCompactPrepareStatusWithoutCoordinatorConstraints`.
  Diagnostics require the newly introduced snapshot-coverage source occurrence,
  complete static workflow contract batch, and `scope_context` runtime output.
  Advanced main's own runtime-string migration note already records the same
  four failures on an independent unchanged snapshot.
- The team race job's raw `short-team` artifact (ID `12975035006`, SHA-256
  `6c688740490e1cfbb062d691b495cb8cc0b1b825d259c1c02a456d59f621b761`)
  confirms exactly the same four top-level failures and diagnostics. There is
  no need to infer the cause from its failed conclusion alone.
- A focused normal execution on the merged source reproduces all four failures:
  `mise exec -- go test -p 2 -count=1 -timeout 10m ./internal/team -run
  '^(TestHufuCodeReviewRuntimeOwnsAll51GapIdentities|TestHufuCodeReviewEmptyCritiqueOwnsSource|TestHufuCodeReviewCharacterizesPromptCannotRewriteStaticScope|TestHufuCodeReviewSynthesisReceivesCompactPrepareStatusWithoutCoordinatorConstraints)$'`.
  This is a diagnostic selection, not another performance observation or a
  replacement for the failed full-suite CI run.

The documentation fix does not establish that these correctness gates pass.
The user's acceptance scope excludes Go production/test changes; the three
affected Go fixture files have therefore not been edited. The benchmark capture
used its original immutable source SHAs, without reruns or altered
assertions; see the terminal hosted section above. This PR remains draft and #26 remains open.
