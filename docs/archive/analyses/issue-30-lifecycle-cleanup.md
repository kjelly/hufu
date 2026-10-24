# Lifecycle-synchronized test cleanup (issue #30)

> Status: historical measurement
> Authority: analysis
> Baseline: `193799fadd59bd4497fc26fd2eb8d48a147d2a46`
> Issue: <https://github.com/kjelly/hufu/issues/30>

## Scope and lifecycle audit

The baseline has 87 `time.Sleep` call sites in Go tests under `cmd/` and
`internal/`. This change removes 22 fixed 100ms waits: 21 cleanup callbacks
and one pre-finalization wait. The removed sites are:

| File in `internal/team/` | Removed sites | Completion condition |
| --- | ---: | --- |
| `protocol_repair_test.go` | 12 | Coordinator.Close |
| `coordinator_terminal_test.go` | 3 | Coordinator.Close |
| `attempt_workspace_runtime_test.go` | 1 | Coordinator.Close |
| `memory_manifest_query_test.go` | 1 | Coordinator.Close, then fixture-owned repository.Close |
| `reliability_plan_test.go` | 1 | Coordinator.Close |
| `wp05_failure_classes_test.go` | 1 | Coordinator.Close |
| `wp08_retry_disposition_test.go` | 1 | Coordinator.Close |
| `terminal_broker_test.go` | 1 | broker.Close, then terminal manager.Close |
| `runtime_workflow_e2e_test.go` | 1 | execution finalizer drains writers; Coordinator.Close at cleanup |

`persistReflexionLessonAsync` registers synchronously with `asyncTasksWg.Go`
before returning to its caller. Its body does not enqueue another detached
writer. Despite its name, `autoWriteSTMASync` already writes synchronously.
Every modified fixture finishes public invocations before cleanup begins.
The shared cleanup helper is registered after workspace/injected repository
cleanup, so LIFO execution drains workers before closing stores and removing
workspaces. The isolated-attempt fixture now closes its initial owned context
repository before intentionally disabling context, and lets Coordinator.Close
close its event store after draining writers.

The terminal broker has its own lifecycle. Its accept loop stays registered
until every admitted handler is registered; shutdown stops admission, closes
accepted connections to interrupt socket I/O, and waits for handlers, including
deferred lease persistence. It does not borrow the coordinator WaitGroup.
Existing tests close client connections, broker, and terminal manager in that
order. TerminalSessionManager.Close owns process containment and finalization.

The remaining baseline waits are preserved by purpose:

- Condition polling: terminal exit/output, process liveness, semantic index
  readiness, lock/cancellation settlement, provider admission and RPC readiness.
- Time contracts and test inputs: deadlines, TTL/order spacing, cancellation,
  mock-provider latency, and the PTY output-drain timeout.
- Child readiness: the four fixed waits in `terminal_cleanup_test.go` allow
  shell signal traps to be installed before cancellation. They are readiness,
  not post-execution teardown, and are outside this cleanup change.
- Opt-in live smoke pacing remains unchanged.

Two virtual-time fixture tests cover immediate cleanup and a writer that takes
one second, beyond the old 100ms grace period, with explicit writer → store →
workspace ordering. Broker regressions cover idle socket interruption and
concurrent Close callers waiting for blocked lease persistence. No timeout,
process-group cancellation, or PTY drain assertion is removed or weakened.

## Measurement method

Measurements run sequentially on the same Linux x86_64 runner (kernel 6.18.55,
AMD EPYC 9V74, 5 visible CPUs), Go 1.27.2, `GOMAXPROCS=2`, `CGO_ENABLED=1`,
`GOTOOLCHAIN=local`, `GOFLAGS=-mod=readonly`, inherited Go caches, and unset
`GOTMPDIR`. No shared cache is cleared. These are warm build-cache/fresh test
executions (`-count=1`), not cold-cache or result-cache measurements.

The same anchored name filter selects 158 existing top-level tests from the nine
modified test files plus all callers of the three affected shared fixture
constructors. New regressions run separately and do not change that comparison.
The command on both revisions is:

```sh
mise exec -- go test -json -p 2 -short -count=1 -timeout 20m \
  -run "$selection" ./internal/team
```

Raw JSONL, exact selected names, monotonic command timings, and separate
instrumentation used to count fixed-wait executions are kept under
`work/issue30/`. They are intentionally excluded from version control.

| Revision | Package seconds, three runs | Median package seconds | Whole-command seconds, three runs |
| --- | --- | ---: | --- |
| Baseline | 9.858, 9.837, 9.734 | 9.837 | 12.103, 11.976, 11.935 |
| Fixed | 3.282, 3.306, 3.113 | 3.282 | 5.914, 5.600, 5.407 |

The selected package execution median decreased by 6.555s (66.6%); command
median decreased from 11.976s to 5.600s (53.2%). This is a focused comparison,
not a claim about full-suite speedup. Case-name sets were identical and every
sample passed. New regressions were excluded from this timing filter.

## Full-package baseline limitation

The initial unmodified full-team run used the same flags without `-run`.
It executed 4,392 top-level tests, took 177.035s package / 219.156s command
(including initial compilation), and failed these five existing assertions:

- `TestProcessSupervisorKillsDescendants`
- `TestTerminalSession_ProcessGroupKillDescendants`
- `TestPTYCloseWaitsForFinalizationAndPublishesClosedOnlyAfterRelease`
- `TestPTYExitCleansDescendantHoldingSlaveBeforeRelease`
- `TestTerminalPipeNaturalExitContainsDescendantsBeforeReap`

They report descendants or process groups still alive after containment. This
baseline is retained as a failure, not retried or treated as a speed measurement.
The focused filter is explicitly narrower and does not establish full-suite
correctness. None of these assertions or their cancellation/drain timing
contracts is changed by this PR.

A separate baseline-only `go test -overlay` run wrapped the 22 removed sites
with a marker while retaining each original 100ms Sleep. It passed with the
same filter and counted **66 actual fixed-wait executions** (6.6s imposed wait).
This instrumented run is excluded from the timing table. The fixed revision
executes none of those waits. The 207 executed names including subtests have
identical SHA-256 `62ca5e067dccbcc3f042fefbf3e9f4446e330432cf0de7bb2009528acaeb1269`
in all six uninstrumented comparison samples.

The two additional shared-fixture callers in `belief_runtime_test.go` and
`runtime_fault_matrix_test.go` also leave event-store teardown to the
coordinator. The latter explicitly drains writers before closing and reopening
the task journal for its persistence assertion. Both were already in the
158-test comparison through shared-helper call-site discovery.

To reconstruct the exact comparison filter from either revision, run this from
the repository root, then set `selection` to the generated file contents:

```python
from pathlib import Path
import re

files = {
    "attempt_workspace_runtime_test.go", "coordinator_terminal_test.go",
    "memory_manifest_query_test.go", "protocol_repair_test.go",
    "reliability_plan_test.go", "runtime_workflow_e2e_test.go",
    "terminal_broker_test.go", "wp05_failure_classes_test.go",
    "wp08_retry_disposition_test.go",
}
helpers = ("newIsolatedTeamFixture", "newWP05RetryTestCoordinator", "newWP08TestCoordinator")
names = set()
for path in Path("internal/team").glob("*_test.go"):
    for block in re.split(r"(?=^func )", path.read_text(), flags=re.M):
        match = re.match(r"func (Test\w+)\(t \*testing.T\)", block)
        if match and (path.name in files or any(h + "(" in block for h in helpers)):
            names.add(match[1])
Path("work/issue30").mkdir(parents=True, exist_ok=True)
Path("work/issue30/selection.txt").write_text("^(" + "|".join(sorted(names)) + ")$")
```

## Validation

- New fixture and broker lifecycle regressions: pass (4 top-level tests).
- Broader targeted race run: pass, `-race -count=5`, 166 top-level tests per
  iteration / 830 executions, 351.648s package / 483.068s command.
- CGO-free targeted run: pass, `CGO_ENABLED=0`, 166 top-level tests, 8.282s
  package / 76.115s command including its separate build.
- Fixed full-team run: 4,396 top-level tests (the original 4,392 plus four new
  regressions), 174.089s package / 209.684s command. Its failure set is exactly
  the same five process-containment assertions as the baseline; no new failing
  test is observed. This is not counted as a successful full-suite run or used
  to calculate a speedup.
- Required `mise run lint`: pass, golangci-lint 2.14.0 / Go 1.27.2,
  117.121s command time.
- Final shared-fixture call-site cases: pass, `-race -count=5`, 10 top-level
  executions, 5.302s package / 57.570s command including the rebuilt race binary.
  These directly cover event-store ownership and explicit task-journal draining.
- Final normal timing samples above were collected after all code adjustments;
  their exact case sets and zero-skip/zero-failure results were checked again.
