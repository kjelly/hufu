# Test performance baseline

> Status: active
> Authority: reference
> Verified-Commit: `13c126aec5022472fd29ce9c01613f49023c779d`
> Supersedes: —
> Superseded-By: —

This is the measurement entry point for [issue #26](https://github.com/kjelly/hufu/issues/26).
It establishes evidence for #27–#32; adding the harness does not complete those
optimizations or establish a speedup. Existing correctness CI gates remain required.

## Capture

Use Go **1.27.2**, Python 3.9+ on Linux/macOS, a C compiler for race, and the same
runner, checkout, environment and flags for both revisions. Ensure loopback
sockets, Unix sockets and PTYs are available. Do not run measurements concurrently
with lint, builds, other measurements or unrelated CPU-intensive work.

```bash
mkdir -p work
python3 scripts/test-baseline.py --output work/baseline-normal --cache-state all --profile
python3 scripts/test-baseline.py --output work/baseline-race --mode race --cache-state all --profile
python3 scripts/test-baseline.py --output work/baseline-cgo-free --mode cgo-free --cache-state all --full
```

Each command defaults to **three repetitions** of the full core suite plus
`./.agent-teams/hufu-dev/diagnostic-pilot/...`. Normal and race use `-short`;
CGO-free above deliberately omits it to match the release gate. For a focused
team race measurement, add `--package ./internal/team`. Repeating `--package`
replaces the default package set; `--run` also changes the measured cases and
must be identical on both revisions. A focused run is not full-suite evidence.

The opt-in **Test performance baseline** GitHub Actions workflow runs these
three configurations on separate Ubuntu runners and uploads artifacts for 14
days, including on failure. Dispatch it on each comparison ref; rerun enough
independent jobs if runner variance dominates. Runner CPU models/quotas may
vary even with the same `ubuntu-latest` label: inspect environment metadata.
It does not skip, split or weaken any required CI check.

## Cache states and outputs

For each repetition the tool creates a private build cache, then runs:

1. `cold`: empty Go build cache and `-count=1`.
2. `warm`: the same build cache with `-count=1`, so tests execute afresh.
3. `result-seed`: no `-count`, to populate successful test results.
4. `result`: the same command again, to measure normal developer cache reuse.

`--cache-state cold`, `warm` (default), or `result` stops after that state in
each repetition; preceding states still run to seed the requested state.
`all` records all three measured groups. Seed observations are kept separately.
Module downloads, filesystem/page caches and external processes are **not**
cold. Private caches are removed after each repetition; the tool never clears
shared `GOCACHE`, changes `GOTMPDIR` or overrides `GOMAXPROCS`. The runtime probe
and optional profile use the inherited cache outside the timed observations.

`environment.json` records commit/dirty state, OS, CPU, logical CPUs, Linux CPU
quota, runner type, configured/effective GOMAXPROCS, CGO, Go toolchain, GOFLAGS,
GOTMPDIR, package selection and flags. GOFLAGS must be empty or `-mod=readonly`
so hidden test filters/cache options cannot invalidate the comparison. Do not
enable live-provider environment flags for a deterministic baseline.

Each invocation writes raw `*.jsonl`, `*.stderr` and a `*.json` observation.
The observation includes monotonic whole-command wall time, child-process user
and system CPU time, exit status, package elapsed times, slowest test events,
pass/fail/skip event counts, skipped tests, cached package names and a hash of
the observed package/test names. Parent/subtest counts are event counts, not
independent top-level cases. `summary.json` reports median/min/max wall time
and per-package elapsed time with sample counts for each cache state. Raw logs
retain every assertion, skip reason and subtest outcome for coverage review.

**Never sum parent and subtest elapsed values, or package elapsed values to
estimate wall time.** Packages and tests overlap. Cached test elapsed values
can be replayed from the original run: use whole-command wall time and
`cached_packages` to interpret developer-cache experience. Do not treat cache
replay as fresh test execution.

The tool stops at the first unsuccessful command, returns nonzero, and keeps
the partial report marked unsuccessful. A partial group with fewer than three
samples is not a baseline. Investigate failures on the unchanged revision;
never retry silently, shorten timeouts, skip a failure or relax an assertion
to produce a faster number. Review skipped cases even when exit status is zero.

## Profiles and interpretation

`--profile` runs a **separate** team invocation after the timing repetitions,
with the selected short/race/filter flags, `-count=1`, CPU and allocation
profiles, and a retained test binary. Its instrumented time is not part of the
baseline medians. `team.cpu.top.txt` and `team.mem.top.txt` are generated with
`go tool pprof -top` (memory defaults to allocation space).

```bash
go tool pprof -http=localhost:8081 work/baseline-normal/team.test work/baseline-normal/team.cpu
go tool pprof -alloc_space work/baseline-normal/team.test work/baseline-normal/team.mem
# If wall time is dominated by waits/I/O, capture trace separately:
go test -count=1 -short -timeout 20m -trace=work/team.trace ./internal/team
go tool trace work/team.trace
```

CPU profiles do not account for all wall time, child-process work or I/O waits.
Review subprocess build counts and teardown waits before changing Coordinator
or SQLite fixtures (#31). Raw JSONL, binaries, traces and private caches stay
under ignored `work/`; commit only a concise report with reproducible commands.

## Coverage contracts that must survive optimization

This inventory identifies representative contract owners, not permission to
delete other cases. The default full-suite commands above discover the full
package sets; the focused entries below are diagnostic entry points only.

| Layer / contract | Representative tests | Diagnostic entry point |
| --- | --- | --- |
| Pure logic, parsing and permissions | `internal/yamlutil`, `internal/tools/permission_test.go`, `internal/tools/session_permission_test.go` | `go test -count=1 ./internal/yamlutil ./internal/tools` |
| Process/PTY, signal handling, terminal restore | `cmd/hufu/pty_e2e_test.go`, `cmd/hufu/signal_injector_test.go`, `internal/team/terminal_cleanup_test.go`, `internal/team/terminal_lifecycle_test.go` | `go test -count=1 ./cmd/hufu ./internal/team -run 'TestE2E_|TestTerminalCleanup|TestTerminalLifecycle'` |
| SQLite migration, durable storage and private sidecars | `internal/context/migrations_test.go`, `internal/runtimestore/permissions_unix_test.go`, `internal/team/event_store_sqlite_test.go` | `go test -count=1 ./internal/context ./internal/runtimestore ./internal/team` |
| Crash/resume and durable result repair | `internal/team/resume_test.go`, `internal/team/wp02_resume_test.go`, `internal/workspace/versionstore` | `go test -count=1 ./internal/team ./internal/workspace/versionstore` |
| Cross-process leases, locking and terminal custody | `internal/team/terminal_broker_test.go`, `internal/context/sqlite_txlock_test.go`, `internal/workspace/lifecycle_test.go` | `go test -count=1 ./internal/team ./internal/context ./internal/workspace` |
| Data races | All core and diagnostic-pilot packages under `-race -short` | Same packages as normal capture, `--mode race` |
| Release constraint | All core and diagnostic-pilot packages with CGO disabled, without race | `--mode cgo-free --full` (actual tests, not compile-only) |
| Live integration (explicit opt-in) | `internal/team/codex_smoke_test.go` | `HUFU_CODEX_SMOKE=1 go test -count=1 ./internal/team -run CodexSmoke` with an installed, authenticated Codex |

Live smoke is separate evidence and is intentionally not enabled by the
baseline workflow. Unix/PTY cases retain their platform build conditions;
short-mode skips are recorded rather than counted as covered contracts.

## Reporting and follow-up

The [2026-10-22 capture attempt](../archive/analyses/test-baseline-2026-10-22.md)
records a failing original-revision run and the resulting baseline gaps.
The [2026-10-24 fixture comparison](../archive/analyses/coordinator-sqlite-fixtures-2026-10-24.md)
records the profile-backed scope, focused before/after measurements and retained
contract coverage for issue #31. It does not establish a passing full-suite baseline.

For each revision report commands, source commit, case-set changes, environment,
sample count, median/range wall time, package timings, slowest tests, skips,
failures/flake observations and CPU seconds. State whether a group was cold,
warm/fresh or result-cached; keep race and CGO-free results separate. Child CPU
time is a local process cost signal, not billing or total runner CPU utilization.
Record CI job duration/runner cost separately when deciding on sharding.

Proceed in the issue's order: shared CLI build helper (#27), identical duplicate
cases (#28), PTY readiness/assertions (#29), lifecycle-driven teardown (#30),
profile-backed fixture changes (#31), then short layering and pure-logic
parallelism (#32). Every deletion needs an assertion-preservation map. Package
splitting/sharding remains a decision after updated timings and flake evidence.
