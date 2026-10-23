# Issue #28 validation diagnostics — 2026-10-22

Follow-up to the [duplicate-test report](duplicate-tests-2026-10-22.md) and
[PR #35](https://github.com/kjelly/hufu/pull/35). This investigation changes
neither runtime/test source nor the CI timeout, assertions, or skip conditions.

## Comparison sources and runner

Main: `6ca6f6d668b804ad7ed19e6e4c184b73e2126a85`.
PR: `a608cc71d26cd2bb78631e01c14d4a7e1395b9e9`.
Both are isolated, unmodified `git archive` snapshots. The PR's later evidence
commit only adds/updates documentation. All runs use Go 1.27.2 on the same
Linux amd64 managed container, AMD EPYC 9V74, five visible CPUs, quota four
CPUs, GOMAXPROCS=5, CGO_ENABLED=1, GOFLAGS=-mod=readonly and GOTOOLCHAIN=local.
GOTMPDIR is unset; the shared build/module caches are preserved. Free space was
checked before validation. The full comparison runs sequentially; it is a
correctness comparison with one full execution per tree, not a three-sample
performance baseline or evidence for a speedup percentage.

## Five process-cleanup failures

The original container has `tail` as PID 1, rather than an init that reaps
orphaned children. A fresh execution on the PR reproduces all five failures.
Reading `/proc/<pid>/stat` for each failed child, or the member of its failed
process group, gives the following observations:

| Failed assertion | Observed PID | PGID | State | Parent PID | Signal 0 |
| --- | --- | --- | --- | --- | --- |
| `TestProcessSupervisorKillsDescendants` | 29476 | 29475 | Z | 1 | succeeds |
| `TestTerminalSession_ProcessGroupKillDescendants` | 29481 | 29479 | Z | 1 | succeeds |
| `TestPTYCloseWaitsForFinalizationAndPublishesClosedOnlyAfterRelease` | 29485 | 29483 | Z | 1 | succeeds |
| `TestPTYExitCleansDescendantHoldingSlaveBeforeRelease` | 29487 | 29486 | Z | 1 | succeeds |
| `TestTerminalPipeNaturalExitContainsDescendantsBeforeReap` | 29489 | 29488 | Z | 1 | succeeds |

`Z` means the process has exited and awaits reaping; it cannot execute or hold
terminal file descriptors. `kill(pid, 0)` and `kill(-pgid, 0)` can still succeed
for zombies. The tests' `processAlive`, `isPIDAlive` and
`terminalProcessGroupAlive` checks therefore report these zombies as alive.
The runtime's Linux `terminalProcessGroupContained` already intentionally
ignores zombies when proving execution/resource containment; its explicit
`cmd.Wait` remains responsible for its direct child. Hufu cannot normally
`waitpid` a grandchild adopted by an unrelated PID 1.

A controlled diagnostic wrapper sets Linux `PR_SET_CHILD_SUBREAPER`, starts the
unaltered test executable, and calls `waitpid(-1, WNOHANG)` to reap adopted
orphans. It never signals processes, modifies fixtures, or changes assertions.
Under this wrapper, all five original tests pass **three consecutive times on
both main and PR** (15/15 per tree). The wrapper records the adopted children
with wait status 9 (SIGKILL) in this controlled run. Without the wrapper, the
same five fail on both trees. This identifies missing orphan reaping in this
container as the cause of these five observed failures, rather than surviving
executable descendants. It is not proof that every possible cleanup path is
correct or that the ordinary container run passes.

The exact focused filter is:

```text
^(TestProcessSupervisorKillsDescendants|TestTerminalSession_ProcessGroupKillDescendants|TestPTYCloseWaitsForFinalizationAndPublishesClosedOnlyAfterRelease|TestPTYExitCleansDescendantHoldingSlaveBeforeRelease|TestTerminalPipeNaturalExitContainsDescendantsBeforeReap)$
```

Use `go test -json -short -count=1 -timeout 5m ./internal/team -run "$filter"`
for the ordinary container reproduction, and add `-exec /absolute/reaper.py`
with `-count=3` for the controlled check. Save the wrapper below as an
executable file (`chmod +x reaper.py`). Its complete mechanism is:

```python
#!/usr/bin/env python3
import ctypes, json, os, subprocess, sys, time
libc = ctypes.CDLL(None, use_errno=True)
if libc.prctl(36, 1, 0, 0, 0) != 0:  # Linux PR_SET_CHILD_SUBREAPER
    raise OSError(ctypes.get_errno(), "PR_SET_CHILD_SUBREAPER")
child = subprocess.Popen(sys.argv[1:])
status = None
reaped = []
while status is None:
    try:
        pid, code = os.waitpid(-1, os.WNOHANG)
    except ChildProcessError:
        break
    if not pid:
        time.sleep(.001)
        continue
    if pid == child.pid:
        status = code
    else:
        reaped.append({"pid": pid, "wait_status": code})
while True:
    try:
        pid, code = os.waitpid(-1, os.WNOHANG)
    except ChildProcessError:
        break
    if not pid:
        break
    reaped.append({"pid": pid, "wait_status": code})
print("DIAGNOSTIC_SUBREAPER " + json.dumps({"adopted": reaped}), file=sys.stderr)
sys.exit(os.waitstatus_to_exitcode(status) if status is not None else 125)
```

This is a Linux diagnostic harness, not a new runtime implementation. A durable
runner remedy is a proper init/subreaper (for example, a container's init option).
An alternative test-portability change must preserve proof of execution and
resource containment and must be reviewed separately; removing the assertions
or simply ignoring failed cleanup is not a remedy.

## Twenty-minute timeout

The original full race run's test deadline is package-wide. The team package
starts at about 09:27:39 UTC, and WP10 starts at **09:47:39.668761063 UTC**;
the timeout fires at about **09:47:39.986 UTC**. WP10 therefore had only about
0.3 seconds before the existing package deadline, not twenty minutes of its own.

Run separately with unchanged sources and assertions:

```sh
go test -json -race -short -count=3 -timeout 5m ./internal/team \
  -run '^TestWP10_SystemicCount_LiveMatchesReplay$'
```

All three executions pass on each tree. Per-test seconds: main **0.30, 0.29,
0.29**; PR **0.31, 0.30, 0.31**. This provides no evidence of a WP10 deadlock.
The earlier long run continuously completed tests; among its slower completed
top-level cases were `TestSQLiteRepositoryTenThousandItems` (61.54s),
`TestSessionDataConcurrentCheckpointNoRace` (59.96s),
`TestHufuDevWorkflowStructuredHandoffAndArtifacts` (53.01s),
`TestSerializedMutationsDoNotInheritFailure` (38.58s), and
`TestExplainPersistentMemoryMatchesRuntimeSelection` (37.13s). These elapsed
values are diagnostic examples and must not be summed to estimate wall time.

## Independent GitHub Actions evidence

The original PR source also completed the repository's ordinary CI successfully,
with its existing **20-minute** race deadline and without this diagnostic wrapper:

- [PR CI run 39758289160](https://github.com/kjelly/hufu/actions/runs/39758289160),
  head `a608cc71`, checks out merge `f2d8e608` against main `6ca6f6d6`.
  All seven required jobs plus the `ci-success` aggregate pass: race tests, build
  (including Darwin checks), vet, lint, docs, CGO-free actual build/tests, and
  eval-core.
- Its [race test job 119755773888](https://github.com/kjelly/hufu/actions/runs/39758289160/job/119755773888)
  runs `go test -race -short -timeout 20m ./... ./.agent-teams/hufu-dev/diagnostic-pilot/...`.
  Log results: `internal/context` **261.610s**, `internal/team` **870.072s**,
  both pass. The checkout and successful job steps were checked, not just a
  combined status endpoint (that endpoint has no legacy status entries).
- The [main CI run 39754005878](https://github.com/kjelly/hufu/actions/runs/39754005878)
  on exact base `6ca6f6d6` has a failing race job: context passes (378.877s),
  while improver (1200.165s) and team (1200.252s) hit the package timeout.
  Its other six required jobs pass. This is additional evidence that a
  20-minute timeout is not a regression introduced by removing five duplicates.

These Actions jobs use separate runners and whole-project package concurrency;
they are not the controlled local two-package comparison. Do not infer that
this tiny PR fixes general CI timing or that their elapsed difference is its
speedup. A future CI timeout/budget decision needs runner-specific measurements
and should be separated from this duplicate-test deletion. No CI gate was
weakened or rerun to manufacture the recorded PR success.


## Complete local controlled race comparison

Both immutable source snapshots complete successfully with the same command:

```sh
go test -json -race -short -count=1 -timeout 35m \
  -exec /absolute/reaper.py ./internal/context ./internal/team
```

The 35-minute deadline is only for these diagnostic executions; the repository's
20-minute CI setting is unchanged. The wrapper is the orphan reaper documented
above. The ordinary PID-1-tail environment's earlier failure and timeout records
remain valid historical observations; these are separate corrected-environment
runs, not a relabeling of those failures as passes.

Seconds, one complete fresh execution per tree:

| Tree | Context package | Team package | Whole command | Child user / system CPU | Pass / skip / fail events |
| --- | --- | --- | --- | --- | --- |
| base | 180.999 | 1221.154 | 1226.780 | 1408.760 / 70.670 | 8871 / 14 / 0 |
| pr | 193.602 | 1242.051 | 1310.164 | 1531.710 / 79.400 | 8866 / 14 / 0 |

Pass/skip counts include parents and subtests, not independent top-level cases.
Child CPU values aggregate subprocess consumption, not billing or runner
utilization. Neither run reports a data-race warning or timeout. Comparing terminal test
names and outcomes (with the single normalization below) finds exactly the
five intended removed tests and no added tests or changed outcomes among the **8880** retained test events.
Their common-outcome SHA-256 is `95068cd5e4df86f766514a3d68a17176c96df8b8775a5580a926c82c610184ec`.

One existing subtest uses its absolute `t.TempDir()` path as its name:
`TestIngestActionProviderArtifactsRejectsMissingAndEscapedPaths/.../stable.json`
in `internal/team/wp03_action_provider_test.go`. Its source runs
`t.Run(path, ...)` for the same `insideSession` fixture. Raw names differ only
in the generated temporary-directory number (1376457200 versus 2508033084).
Only that numeric suffix is normalized to `<RANDOM>` for the comparison;
all other path components and test names are preserved. There is exactly one
such normalization per run and no name collision. Both raw and normalized
differences are retained in the evidence archive. Both versions of this
absolute-path rejection subtest pass; it is neither an added nor a removed
scenario. Test source is unchanged.

The 14 skips are unchanged between snapshots. They remain skips, not claims of
coverage. The same-source file/blob checks also confirm the archived files were
not edited during validation. Raw JSONL, process snapshots, commands, CPU/wall
observations and the reaper are retained in the evidence archive under the
cloud workspace's ignored `work/issue28-diagnostics/evidence/` directory. This
executor has no mounted chat-download directory or file-export tool. The report
and reproduction procedure are available in the PR; large raw logs are not
committed to the repository.

This one-run correctness comparison completes the focused validation gap. It
is not a robust speedup measurement; package elapsed times must not be summed,
and the original three-sample focused fixture measurements remain the only
local repeated timing comparison for this PR.
