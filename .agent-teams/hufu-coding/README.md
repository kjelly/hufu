# hufu-coding

> Status: active
> Authority: team contract
> Verified-Commit: `6ab9951`
> Supersedes: the former standalone coding-team implementation spec
> Superseded-By: —

A reliability-oriented coding team: Hufu owns the control plane (task
admission, retry, failure classification, recovery, and final acceptance);
every worker's actual reasoning turn executes through the `codex`
subagent-provider. This README is the canonical contract and operator guide
for the team. The generic runtime boundary is documented in
[`docs/architecture/execution-runtime.md`](../../docs/architecture/execution-runtime.md);
the former standalone coding-team implementation spec is archived.

The original design scoped Codex to just the coder leaf — see "All five roles
are codex-bound" below for why every role uses it here instead.

Binding a worker to a subagent-provider changes only *which process executes
its model call* — Hufu's own control plane (task admission, the typed
`submit_result` protocol, DAG scheduling, retry/failure classification,
on-failure-classes, remediation context, final acceptance) is unchanged and
still fully owned by Hufu regardless of which agent below is codex-bound.

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

## SA never lets the coder start on an unconfirmed contract

The coder depends on SA (`depends_on: [0]`), and Hufu's own DAG readiness
rule requires every dependency to reach `TaskDone` before a task becomes
ready — SA has no `on_failure` edge of its own (nothing resets into it, and
it resets nothing else), so this dependency gate is the *only* mechanism
that matters for it, and it already does the right thing with no further
runtime change: SA reports `status: success` only when its implementation
contract is actionable, `status: partial` for a genuine blocking ambiguity,
`status: blocked` for a missing input/environment/human decision — never
`completed_with_gaps` (sa.md is explicit: for this task a "gap" is never one
safe to hand to the coder). A `partial`/`blocked` SA never reaches
`TaskDone`, so the coder simply never becomes ready; the run correctly ends
up blocked/partial instead of the coder implementing against a contract SA
itself was not confident in.

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
(`coordinator_task_run.go`'s `withFailureClassOverride`) always persists its
raw `TaskFailureClass` as `execution` — the exact same raw class an
app-server crash or network timeout gets, since from Hufu's point of view
both are simply "the worker did not reach a done state." A raw class alone
cannot tell a confirmed rejection apart from an infra failure, so
`dagScheduler.handleEvent` (`internal/team/dag_scheduler.go`) canonicalizes
the class it actually checks against the allowlist
(`effectiveFailureClassForTodo`): if the terminal `TypedResult` is a genuine,
complete worker-authored submission with `status: failed`
(`isGenuineWorkerReportedFailure`), the class it evaluates is
`TaskFailureClass = semantic_rejection`, a value Hufu itself assigns and
that is never bypassed by a separate OR-condition — the allowlist is one
single, uniform check against this canonicalized class. If the task instead
has no stored result at all (a real protocol/infra abort) or reported
`blocked`/`partial`, the raw class (`execution`, unchanged) is what gets
checked, and `on-failure-classes` correctly does not name it. Each of the
three tasks' contract sets `on-failure-classes: [verification,
semantic_rejection]` — `verification` is defense-in-depth for a future
`VerifyCommandExit`-style check that might genuinely produce that raw class;
`semantic_rejection` is what actually authorizes today's `status: failed`
path — omitting it here would make every genuine confirmed rejection fail
closed instead of resetting the coder. This is what makes "reviewer's
app-server disconnected" and "reviewer couldn't read a cited file" both
behave completely differently from "reviewer found a real bug," even though
all three start as a task error.

A non-authorized class is not automatically retried in place, either.
`dagScheduler`'s gate (`selfHealEligible`) reuses the task's own
already-persisted `RetryDisposition` (`disposition.go`'s `DecideRecovery`)
rather than re-deriving eligibility from `TaskFailureClass` in a second,
independently-maintained mapping. `RetryDisposition == RetryNone` is
necessary but not sufficient: DecideRecovery also returns `RetryNone` for a
resolved recovery policy that is explicitly `never` (or, via the same
`CanAutomaticallyReplay` check, `manual`/`reconcile`, or a structurally
non-replayable side effect) *regardless of remaining retry budget* — so
`selfHealEligible` additionally requires `CanAutomaticallyReplay` (the same
authority `resetTask` itself already gates on) before treating a `RetryNone`
as "budget exhausted on an otherwise-retryable class, safe for one more
DAG-level attempt". Cancellation is excluded directly by `FailureClass`.
Everything else `DecideRecovery` decided must not be replayed
(`ReplanRequired`, `ReconcileOnly`, `NeedsHuman`) fails closed here too: a
missing tool, an invalid contract, a policy denial, or a cancellation is not
fixed by trying again, and retrying here would grant a second, DAG-level
retry cycle the task's own inner attempt loop already correctly refused.

When a semantic rejection does reset the coder, Hufu attaches a durable
`RemediationContext` (`internal/team/remediation_context.go`) built from the
source task's own canonical failure/result — source task id, agent, attempt,
failure class, status, summary, findings, verification evidence — to the
coder's next dispatch. The coder never has to be manually re-told what went
wrong by the coordinator's prose.

## All five roles are codex-bound

`sa`, `coder`, `verifier`, `reviewer`, and `final-sa` each carry
`subagent-provider: codex` in their frontmatter (operator-directed change;
the original design bound only `coder`, keeping Codex a bounded leaf
implementation worker). This does not weaken read-only enforcement for the
four non-coder roles: `subagent_codex.go` derives each dispatch's Codex
sandbox mode (`read-only` vs `workspace-write`) generically from the task's
own `side_effect` (`ExecutionWorldSpec`/`WritableRoots`), the same mechanism
that already gated `coder`'s `workspace_write` access — so `sa`/`verifier`/
`reviewer`/`final-sa`'s `side_effect: none` contracts still force a read-only
Codex sandbox exactly as they did against Hufu's native model before this
change. A codex-bound worker also carries no `tools:` frontmatter (unlike a
Hufu-native worker): a codex provider sets `SupportsHufuTools: false` and
always uses Codex's own built-in tools, so Hufu's `tools:` allowlist has no
effect once a role is codex-bound — the sandbox mode is the only gate that
matters for these roles now (`TestHufuCodingAllRolesBoundToCodex`,
`TestHufuCodingReadOnlyRolesSideEffectNoneGatesCodexSandbox`).

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
