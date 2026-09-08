# hufu-coding

A reliability-oriented coding team: Hufu owns the control plane (task
admission, retry, failure classification, recovery, and final acceptance);
Codex is a bounded leaf implementation worker. See `../../spec.md` for the
full design rationale (gitignored; ask the operator if you need the original
document).

## Fixed batch shape

The coordinator submits exactly one batch, once, per run:

```text
0: sa        SA_ANALYZE
1: coder     CODER_IMPLEMENT       depends_on [0]
2: verifier  VERIFY_IMPLEMENTATION depends_on [0,1],     on_failure -> 1
3: reviewer  REVIEW_CODE           depends_on [0,1,2],   on_failure -> 1
4: final-sa  FINAL_SA_GATE         depends_on [0,1,2,3], on_failure -> 1
```

Each `depends_on` list is every earlier task, not just the immediately
preceding one: a worker's compiled prompt only receives an earlier task's
typed result when that task is listed directly in its own `depends_on` —
Hufu does not walk the dependency graph transitively
(`Coordinator.dependencyResultsForTask`, coordinator_task_run.go). Without
the full list, final-sa would only ever see reviewer's result, never SA's
contract or verifier's evidence.

Hufu's DAG scheduler (`internal/team/dag_scheduler.go`) owns everything past
that point: readiness, concurrency, retry, and the bounded coder-remediation
loop.

## How a semantic rejection reaches the coder

Verifier/reviewer/final-sa each submit an honest `status` for their own task —
there is no separate pass/fail fact field:

| Task | `status: success` means | `status: failed` means |
|---|---|---|
| verifier | every required check passed | a required check genuinely failed |
| reviewer | no confirmed must-fix finding | a confirmed must-fix finding exists |
| final-sa | the request is genuinely satisfied | the request is not (or not fully) satisfied |

`status: blocked`/`partial` means the worker itself could not finish
(infrastructure, environment, or an unresolvable ambiguity) — not a verdict on
the code either way. None of the three ever uses `completed_with_gaps`: that
status means the check/review/gate itself is done and its conclusion is
trustworthy, which an unfinished evaluation is not — using it for an evidence
gap would make "I couldn't tell" indistinguishable from "I checked and it's
fine," silently treating an unreliable review as clean instead of retrying it.

A `task_result_assert` verify-spec was deliberately **not** used to encode
this verdict, even though `hufu-code-review`'s reviewer contract uses exactly
that mechanism for a different purpose. `task_result_assert` enforces its
assertions at submit_result *admission* time
(`internal/team/task_result_contract.go`): a failing assertion rejects the
tool call itself, forcing the model to revise and resubmit. That is correct
for a protocol-completeness check (e.g. "files_read must be non-empty" — the
model can always add the missing evidence) but wrong for a semantic verdict —
a worker that honestly found a real, confirmed defect would never be able to
successfully submit that finding at all, since the assertion could never pass
for a true positive (a real gap this team's design went through before
settling on the `status`-based approach documented here).

Instead: `status: failed` is a fully valid, *admission-accepted* submission
(only `success`/`completed_with_gaps` are ever gated by a verify-spec at
all), and Hufu's own generic non-terminal-status handling
(`coordinator_task_run.go`'s `withFailureClassOverride`) always classifies it
`TaskFailureClass = execution` — the exact same class an app-server crash or
network timeout gets, since from Hufu's point of view both are simply "the
worker did not reach a done state." A fixed class alone cannot tell a
confirmed rejection apart from an infra failure, so `dagScheduler.handleEvent`
(`internal/team/dag_scheduler.go`, `isGenuineWorkerReportedFailure`) also
checks whether the terminal `TypedResult` is a genuine, complete
worker-authored submission with `status: failed` — if so, the on_failure edge
is authorized regardless of the raw failure class; if the task instead has no
stored result at all (a real protocol/infra abort) or reported
`blocked`/`partial`, it is not. Each of the three tasks' contract still sets
`on-failure-classes: [verification]` — a new, generic, backward-compatible
field on `TaskDef` (`internal/team/coordinator.go`) — as defense-in-depth for
a future `VerifyCommandExit`-style check that might genuinely produce that
class; it does no active work for the `status: failed` path today. This is
what makes "reviewer's app-server disconnected" and "reviewer couldn't read a
cited file" both behave completely differently from "reviewer found a real
bug," even though all three start as a task error.

A non-authorized class is not automatically retried in place, either.
`dagScheduler`'s gate (`selfHealEligibleFailureClass`) only self-heals
execution/timeout/protocol (and verification, if a task's allowlist ever
excludes it) — transient conditions worth one more attempt. Environment,
contract, policy, and cancelled fail closed instead (no retry, no reset,
left in whatever terminal state the task's own inner attempt loop already
persisted), matching `disposition.go`'s own `DecideRecovery` semantics for
these same classes: a missing tool, an invalid contract, a policy denial, or
a cancellation is not fixed by trying again, and retrying here would grant a
second, DAG-level retry cycle the inner loop already correctly refused.

When a semantic rejection does reset the coder, Hufu attaches a durable
`RemediationContext` (`internal/team/remediation_context.go`) built from the
source task's own canonical failure/result — source task id, agent, attempt,
failure class, status, summary, findings, verification evidence — to the
coder's next dispatch. The coder never has to be manually re-told what went
wrong by the coordinator's prose.

## Running it

```bash
hufu team validate --team hufu-coding
hufu list hufu-coding
hufu --agent-team hufu-coding --report \
  "Implement <requested change>. Find and fix the root cause, add regression
   tests, and finish only after verification and independent review pass."
```

## Codex single-agent enforcement

The `codex` subagent-provider command in `team.yaml` passes both
`-c agents.enabled=false` and `-c features.multi_agent_v2=false`, because
Codex's `features.multi_agent_v2` can otherwise take precedence over
`[agents].enabled`. This is asserted by a config-lint regression test
(fails `hufu team validate`-style checks if either flag is missing) and, when
credentials are available, by the opt-in `HUFU_CODEX_SMOKE=1` live suite,
which additionally proves the running Codex worker cannot expose a native
multi-agent tool. The deterministic (non-live) test suite cannot observe real
Codex CLI behavior, so it only proves Hufu's own configuration is correct —
see the completion report for the exact split.
