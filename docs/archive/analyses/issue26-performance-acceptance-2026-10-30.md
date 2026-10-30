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

**At report creation, the three Actions jobs are running.** No completed
three-sample full-suite cell is yet accepted. Collect artifacts from the linked
run and replace this status with actual results before considering closure.
Artifacts retain failures for 14 days; archive their original ZIPs and digests
before expiry. A passing job alone is insufficient: review case inventory and
skip reasons first. In particular, the three revisions do not have an identical
raw test inventory. A same-case filtered capture or complete assertion-preserving
workload reconciliation is still required for a speedup claim.

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

The [downloadable raw evidence package](https://chatgpt.com/library?file_id=libfile_3478037263e88191b4b571f490701c1b&account_id=personal)
preserves this local capture and the five original correctness artifact ZIPs
audited below, with per-file SHA-256 and byte sizes. Private Go build caches are
excluded. It does not contain the still-running hosted capture.

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
