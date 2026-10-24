# Coordinator and SQLite fixture measurements — 2026-10-24

Execution evidence for [issue #31](https://github.com/kjelly/hufu/issues/31).
The general capture procedure remains in the
[test performance reference](../../reference/test-performance-baseline.md).

## Scope and environment

The comparison starts at `3caa3170029bd6f52d423de13cca7550882ad91b`.
Both revisions ran sequentially in the same managed Linux amd64 container
(kernel 6.18.55, glibc 2.41), on an Intel Xeon Platinum 8573C, with five visible
logical CPUs and a four-CPU cgroup quota. Mise selected Go 1.27.2 and
GOMAXPROCS=2; builds used `-p 2`, GOTOOLCHAIN=local and GOFLAGS=-mod=readonly.
CGO_ENABLED=1 for normal/race; GOTMPDIR was unset. Existing module/build/lint
caches were preserved. Loopback and Unix sockets were available.

These are **warm build cache / fresh test execution** measurements (`-count=1`),
not cold builds, result-cache measurements, CI runner cost, or a full-repository
speedup. Each mode had an excluded warm-up followed by three measured commands;
normal and race were measured separately, without competing validation jobs.
The case set was frozen before editing: all 55 top-level tests in
`memory_ranking_test.go`, `memory_outcome_test.go`, `semantic_runtime_test.go`,
`profile_test.go` and `services_test.go`, under `internal/team`. Both revisions
produced the same 61 parent/subtest run events, zero skips and case-name SHA-256
`9f55f12c8dc7d85c136d7ef5f154e1da01167f5d4e1e67991c3267a0b9ab6a77`.
New fixture contract tests are validated separately, outside this comparison.

## Profile-backed choice

An unchanged full-team CPU/allocation profile took 227.406 seconds of package
wall time and sampled 230.06 CPU seconds. It failed the five descendant-process
contracts listed below, so it is diagnostic evidence, not a successful full-suite
baseline. An earlier restricted-socket attempt aborted before producing usable
profiles and is excluded.

The full profile attributes 8.89 cumulative CPU seconds (3.86%) to context SQLite
migration. The larger costs are redaction (89.56 seconds under
`utils.redactSecretsText`) and session checkpoint/serialization. SQLite is not
the dominant team bottleneck; this change deliberately converts only three
service fixtures, without changing production redaction or persistence.

Separate profiles of the frozen, passing subset identified the avoidable
constructor work more clearly:

| Cumulative profile observation | Before | After |
| --- | ---: | ---: |
| `newCoordinator` CPU | 1.09 s | 0.65 s |
| context `SQLiteRepository.migrate` CPU | 0.51 s | 0.15 s |
| Total sampled allocation space | 461.40 MB | 264.75 MB |
| Model catalog load allocation space | 389.00 MB | 206.57 MB |

CPU/allocation profiles are single diagnostic samples, not timing repetitions.
Cumulative rows overlap and must not be added. They do not measure all I/O waits
or child-process work. Avoiding unnecessary model catalog parsing and context
schema creation is supported by these samples; no production injection seam is
needed.

## Timing comparison

Seconds, median (minimum–maximum), three samples per row.

| Mode / revision | Whole command wall | Package elapsed | Child CPU median |
| --- | ---: | ---: | ---: |
| normal / before | 5.859 (5.804–5.931) | 3.136 (3.125–3.270) | 5.528 |
| normal / after | 4.858 (4.795–5.455) | 2.346 (2.334–2.382) | 4.174 |
| race / before | 37.396 (36.473–43.906) | 32.901 (31.826–38.944) | 37.144 |
| race / after | 21.877 (20.217–21.879) | 16.926 (16.422–17.546) | 21.195 |

The measured whole-command median decreased by 17.1% normally and 41.5% under
race. These percentages apply only to this frozen subset; the race before-range
also shows runner variance. Child CPU values cover the local command's process
tree, not total runner utilization or billing.

Reproduce the case set and flags from the repository root on each revision:

```bash
fixture_cases=$(python3 - <<'PY'
import re
from pathlib import Path
files = ['memory_ranking_test.go', 'memory_outcome_test.go',
         'semantic_runtime_test.go', 'profile_test.go', 'services_test.go']
names = sorted({name for file in files for name in re.findall(
    r'^func (Test\w+)\(', Path('internal/team', file).read_text(), re.M)})
print('^(' + '|'.join(names) + ')$')
PY
)
# Warm each mode first, then capture three independent invocations.
/usr/bin/time -p mise exec -- go test -p 2 -short -count=1 -timeout 20m \
  -json ./internal/team -run "$fixture_cases"
/usr/bin/time -p mise exec -- go test -p 2 -race -short -count=1 -timeout 20m \
  -json ./internal/team -run "$fixture_cases"
# Separate diagnostic run; exclude its time from the three observations.
mise exec -- go test -p 2 -short -count=1 -timeout 20m ./internal/team \
  -run "$fixture_cases" -cpuprofile=work/team.cpu -memprofile=work/team.mem \
  -o work/team.test
mise exec -- go tool pprof -top -cum work/team.test work/team.cpu
mise exec -- go tool pprof -top -cum work/team.test work/team.mem
```

Raw command observations, JSONL, profiles and binaries were retained under
ignored `work/issue31/`, not committed. The observations used monotonic
whole-command wall time and child user+system CPU time; package elapsed time is
the terminal package event. Parent/subtest elapsed values are never summed.

## Contract preservation

| Case class | Change and retained contract |
| --- | --- |
| Pure path/profile logic | The seven workspace-isolation cases and acceptance-mode precedence call the same methods with only their session/project/profile fields; every assertion remains. |
| Service override dispatch | Uses the injected mocks directly; the constructor defaults and composition-root tests still cover real wiring. |
| Memory ranking, outcome attribution, semantic services | Each opens a private real SQLite adapter from an empty migrated image. Seeds and assertions remain per case; semantic refreshers close before their repository, and outcome event stores close first. |
| Constructor, history recovery, acceptance, terminals | Retain the real constructor; the remaining ten constructor fixtures in profile/services now register `Close` before assertions. |
| Migration, checksum, empty DB, reopen, durability, resume, cross-process locking | Existing direct-open tests remain unchanged. There is no cache of mutable databases, no migration bypass and no safety-pragma change. |

The template is created once per process with the production `OpenSQLite`.
Closing its final connection checkpoints the WAL; the helper requires both WAL
and SHM to be absent before reading the main file, then removes the source
directory. Only immutable bytes remain shared. Every destination is created
exclusively with mode 0600 and reopened through production `OpenSQLite`, which
still validates checksums and applies WAL/busy-timeout/foreign-key settings.
The helper refuses to overwrite any existing database.

New contract tests compare fresh and copied adapters for FTS, scope isolation,
persistence across a normal reopen, cleanup and file-descriptor ownership.
Eight parallel copies write the same item ID with different data and verify
isolation; the shared image hash must remain unchanged.

## Validation and limits

- Frozen 55-test comparison: three passing normal and three passing race
  invocations on each revision, with 61 identical run events and zero skips.
- `mise exec -- go test -p 2 -count=1 -timeout 20m ./internal/context`,
  repeated with `-race` and separately with `CGO_ENABLED=0`: all standard
  regression checks passed (247 top-level entries, 432 run events per mode).
  The three existing opt-in `TestCanonicalConcurrencyDiscovery`,
  `TestCanonicalIndexDiscovery` and `TestCanonicalMaintenanceDiscovery` reviews
  require `HUFU_SQLITE_DISCOVERY=1` and remained skipped in every mode.
- Related team selection: 185 top-level tests / 338 run events from 26 files,
  passed with `-race -short -count=1` and separately with `CGO_ENABLED=0`
  without `-short`; zero skips in either. This includes the three helper
  consumers, fixture safety contracts, profile/services and constructor ownership
  tests, including concurrent/idempotent close and async draining.
- `mise run lint`: passed, **0 issues**.
- `git diff --check` and `bin/check-docs`: passed.

The unchanged full-team profile failed:

- `TestProcessSupervisorKillsDescendants`
- `TestTerminalSession_ProcessGroupKillDescendants`
- `TestPTYCloseWaitsForFinalizationAndPublishesClosedOnlyAfterRelease`
- `TestPTYExitCleansDescendantHoldingSlaveBeforeRelease`
- `TestTerminalPipeNaturalExitContainsDescendantsBeforeReap`

The post-change full-team run used the same short/count/profile flags and
recorded 201.607 seconds of package elapsed time. It failed exactly the same
five tests, with no additional failing test; the new fixture contracts passed.
Both full runs are single failing diagnostic observations, not an accepted
three-sample full-team speedup comparison.

These process-reaping failures are reported without skipping, retrying, weakening
assertions or changing timeouts. They prevent claiming a passing full-team
baseline in this container. Issue #26 remains responsible for the broader
cold/warm/result-cache and full-suite/race baseline, and redaction/serialization
remain potential follow-up bottlenecks outside this fixture change.
