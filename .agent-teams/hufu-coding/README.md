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
1: coder     CODER_IMPLEMENT      depends_on [0]
2: verifier  VERIFY_IMPLEMENTATION depends_on [1], on_failure -> 1
3: reviewer  REVIEW_CODE           depends_on [2], on_failure -> 1
4: final-sa  FINAL_SA_GATE         depends_on [3], on_failure -> 1
```

Hufu's DAG scheduler (`internal/team/dag_scheduler.go`) owns everything past
that point: readiness, concurrency, retry, and the bounded coder-remediation
loop.

## How a semantic rejection reaches the coder

Verifier/reviewer/final-sa each submit an honest `status` for their own task
(`success`/`completed_with_gaps` when they did their job, `blocked`/`partial`
when an infrastructure/environment problem stopped them) plus one explicit
boolean fact about what they found:

| Task | Fact | Meaning when `false` |
|---|---|---|
| verifier | `all_checks_passed` | a required check genuinely failed |
| reviewer | `must_fix_found` | inverted: `true` means a concrete defect exists |
| final-sa | `request_satisfied` | the requested behavior is not actually done |

Each task's static contract in `team.yaml` asserts that fact via
`verify-spec: task_result_assert`. A `false` (or `must_fix_found: true`)
result fails that task with `TaskFailureClass = verification`. Each of those
three tasks' contract also sets `on-failure-classes: [verification]` — a new,
generic, backward-compatible field on `TaskDef`
(`internal/team/coordinator.go`) consulted by `dagScheduler.handleEvent`
before it honors an `on_failure` edge. Only that class may reset the coder
wave; any other class (execution/timeout/protocol/environment/contract/
policy/cancelled — a genuine provider or infrastructure failure) instead
retries the same failing worker in place, never the coder. This is what
makes "reviewer's app-server disconnected" behave completely differently
from "reviewer found a real bug," even though both start as a task error.

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
