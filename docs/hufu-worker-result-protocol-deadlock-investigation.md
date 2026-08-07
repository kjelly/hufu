# Worker Result-Protocol Deadlock — Investigation Report

**Date:** 2026-08-04
**Subject run:** `run-20260804T033148.792707555Z-2261deb800cd`
**Evidence:** `<workspace>/default/` (logs, task journal, LLM transcripts)
**Invocation:** `hufu --auto-approve --unattended --default --route team --helper-tools bash,terminal <goal> --model ollama/minimax-m2.7:cloud --timeout 7600 --new`

> Line references below describe the code **as it was during the subject run**, before the
> remediation at the end of this document. They will not match current line numbers.

## Summary

Every worker task in this run failed its result protocol on the first pass. Six tasks
were rescued only by the protocol-repair turn; the six that needed more than 30 tool
calls could not complete at all. The cause is not the target project or its runbook —
it is three independent hufu defects that compose into a deadlock:

1. `submit_result` is mandatory for every worker task, is exposed to the worker's model,
   and is **absent from the worker's runtime allowlist**. The stream authorization gate
   therefore aborts the attempt at the exact moment the worker tries to report.
2. The worker prompt instructs the worker to call `stm_write` (also denied) and **never
   mentions `submit_result`** at all.
3. Step-budget exhaustion is misclassified as a protocol failure, and the retry hint
   tells a worker that was merely truncated to "change your approach". `--max-steps`,
   the only operator knob, is silently ignored on every agent path.

## Observed outcome

`workspace/default/logs/execution-events.jsonl`, final record:

```
outcome: partial | stop_reason: unresolved_tasks
decision_chain: outcome:partial, goal_satisfied:false, acceptance:not_configured,
                evidence:incomplete, terminal:unresolved_tasks,
                failure:execution=15, failure:protocol=15, failure:verification=4
repair_cost: 10 attempts / 255,357 tokens
```

12 tasks over 95 minutes. The split is clean:

| Task class | Task IDs | Result |
|---|---|---|
| Finished work within 30 steps | 1, 2, 3, 7, 10, 12 | `done` — but all six only via protocol repair |
| Needed more than 30 steps | 4, 5, 6, 8, 9, 11 | 2 attempts each, all `error` |

`workspace/default/logs/llm/default/helper/llm.log` contains **no `submit_result` call
made by a worker in its own execution turn.** Every one of them is immediately preceded
by the repair prompt `"...you did not submit a structured result via submit_result as
required. Call submit_result now..."`.

## Root cause 1 — mandatory protocol tool is not runtime-authorized

Three individually reasonable decisions that contradict each other:

| Location | Behaviour |
|---|---|
| `internal/team/coordinator_execute.go:124` | `Execution.RequiresResult = true` unconditionally, for every non-sidecar task |
| `internal/team/coordinator_task_run.go:2219` | Appends the `submit_result` tool to the tool set handed to the model |
| `internal/team/coordinator_task_run.go:1507` | `withEffectiveToolsAllowed` builds the allowlist from `Config.ToolsAllowed + def.Tools + def.MCPTools` only — `submit_result` is not among them |
| `internal/team/coordinator_task_run.go:1665` | The `OnToolCall` gate returns an `error` on denial, which **aborts the whole stream**, and does so *before* `transcript.RecordToolCall`, so the call leaves no evidence |

`agent.SelectTools` additionally forces in nine tools via `alwaysIncludeTools`
(`internal/agent/agent.go:534`) regardless of `def.Tools`, and
`withEffectiveToolsAllowed` has no knowledge of them.

Measured against this run's configuration (`--helper-tools bash,terminal`), 9 of the 28
tools the worker's model can see are denied by the runtime allowlist:

```
load_skill  ltm_update  memory_query  memory_save  request_agent
stm_write   submit_result  team_info   todo
```

Task 9 was destroyed directly by this. Its journal record shows the denial as the
task's terminal error with zero output, and `last_tool=bash` — the `submit_result`
call was never even recorded, confirming the gate aborts before recording:

```
source=error | last_tool=bash |
error=tool authorization denied for "submit_result": tool "submit_result" is not authorized
```

**Regression window.** The fail-closed allowlist was introduced in `23d698a`
(2026-07-26, "fix(team): propagate direct agent tool allowlist"); the unconditional
result requirement in `fb39e76` (2026-07-29, "feat(team): add protocol repair and
ExecutionReceipt for incomplete task runs"). The two were never reconciled.

**Why no test caught it.** `TestBuildOrchestratorToolsAreRuntimeAllowed`
(`internal/team/coordinator_tools_allowed_test.go`) guards exactly this invariant — for
the coordinator. There was no worker equivalent.

## Root cause 2 — the worker prompt contradicts the worker's grants

Dumped from the actual request in `helper/llm.log`:

```
## Instructions

You are a domain expert. Determine your own implementation approach based on the goal above.

- Key knowledge from previous agents is provided below. ...
- When you discover something important (API shape, file location, decision, error),
  write it to `stm.md` immediately via `stm_write` — do not wait until the end.
```

- `stm_write` (`coordinator_task_run.go:179`) is denied → four attempts died obeying
  their own instructions.
- The prompt **never mentions `submit_result`**, despite it being mandatory. It says
  "your final response", so the model writes a prose summary and stops — six
  `protocol incomplete` failures with `finish_reason=stop`.
- The plan-first branch (`coordinator_task_run.go:166`) says "Call finish when done",
  but `finish` is coordinator-only; workers never have it.

## Root cause 3 — step-budget exhaustion misdiagnosed, and no working knob

Nine of the fifteen `protocol incomplete` failures are `DefaultMaxSteps = 30`
exhaustion. `REQUEST step=29` timestamps in `helper/llm.log` map 1:1 onto the journal's
error timestamps:

```
03:45:11 step=29 → 03:45:39 err task=4    04:32:49 step=29 → 04:33:24 err task=8
03:49:13 step=29 → 03:49:20 err task=4    04:46:44 step=29 → 04:47:08 err task=8
03:52:14 step=29 → 03:52:19 err task=5    04:55:57 step=29 → 04:56:07 err task=11
04:01:08 step=29 → 04:01:38 err task=5    05:00:54 step=29 → 05:01:27 err task=11
04:17:49 step=29 → 04:18:00 err task=6
```

`coordinator_task_run.go:598` only tests `typedRes == nil`. It cannot distinguish "the
model ignored the protocol" from "the model ran out of steps", so both become
`FailureProtocol`.

The consequence is worse than a mislabel. The retry hint recorded in
`logs/task_journal.jsonl` is:

> The previous attempt failed with: ... protocol incomplete: missing required result.
> **Change your approach rather than repeating the same actions.**

Telling a worker that was merely truncated mid-work to change approach is actively
harmful, and drives the thrashing that ends the run.

**The operator has no escape hatch.** `internal/agent/agent.go:513` reads
`maxSteps := cfg.MaxSteps` first and only falls back to
`resolveMaxSteps(def.MaxSteps, teamCfg.MaxSteps)` when that is zero. Every call site
hardcodes a non-zero value:

| Call site | Hardcoded |
|---|---|
| `coordinator_task_run.go:2176` (plan agent) | `agent.DefaultMaxSteps` |
| `coordinator_task_run.go:2228` (worker) | `agent.DefaultMaxSteps` |
| `coordinator_tools_delegate.go:255` (sub-agent) | `agent.DefaultMaxSteps` |
| `coordinator_agents.go:79` | `agent.DefaultMaxSteps` |
| `coordinator_run.go:333` (coordinator) | `agent.DefaultCoordinatorMaxSteps` |

So `resolveMaxSteps` is never reached, and both `--max-steps` and team.yaml
`max-steps:` are dead.

`docs/hufu-generic-task-reliability-mechanisms.md` §8 already requires a
"result-finalization grace period" reserved for "退出、寫 artifact、submit_result 和
verifier 排程". It is implemented only on the time axis, never on the step axis.

## Root cause 4 — the repair turn has no evidence

The protocol repair agent runs with `MaxSteps: 1` and only `submit_result`
(`coordinator_task_run.go:627,636`), and its prompt carries only `task.Goal` plus the
final prose — **not the attempt's conversation history or transcript**
(`runAgentWithStatusAndHistory(..., prompt, nil, ...)`).

When the work genuinely did not finish (root cause 3), the repair can only guess, and
honestly reports `partial`/`failed`. Per §7 that is `progress_not_final` and is
correctly reclassified to an execution failure — but the retry then hits the same
30-step wall. That loop is the 10 repair attempts and 255,357 tokens.

Note that `rescueFinalSummary` (`coordinator_task_run.go:1361`) already reconstructs
history from `steps` for the empty-output case, but explicitly instructs "Do NOT call
any tools", so it can never satisfy the protocol either.

## Root cause 5 — two divergent authorization gates

hufu authorized agent tool calls in two places that disagreed:

- `internal/tools/tools.go:570` (the tool adapter) honours session-level permissions
  and surfaces a denial as a **recoverable tool error** the model can adapt to.
- the stream gate in `OnToolCall` consulted only the allowlist and returned an
  **error**, which fantasy propagates out of the step (`agent.go:1425`), aborting the
  whole model round.

So one boundary let the model recover from a denial and the other destroyed the attempt
— and the stricter one ran first, before `transcript.RecordToolCall`, so the denied call
left no evidence either. This is what made root cause 1 fatal rather than merely noisy:
one ungranted call discarded every tool call the worker had already completed.

The adapter also only covers tools built by `tools.AllTools`. The coordinator's own
tools — `submit_result` among them — are raw `fantasy.AgentTool` implementations with no
adapter, so for exactly the tool that mattered the aborting gate was the *only* gate.

Note that `--auto-approve` is not implicated: `tools.IsAutoApprove` governs `ask_user`
auto-selection, not tool grants, and must not become a grant mechanism.

## Remediation

Implemented (see `git log`):

- **P0-1** — Derive the runtime allowlist from the tools actually handed to the agent.
  `agent.EffectiveToolNames` returns what `SelectTools` selected (so `alwaysIncludeTools`
  is covered by construction), and `withEffectiveToolsAllowed` unions it with the
  protocol tools (`submit_result`, `submit_plan`) and global MCP tool names.
  Guarded by `TestWorkerExposedToolsAreRuntimeAllowed`: for every agent definition,
  exposed ⊆ allowed.

- **P0-2** — Generate the worker's protocol instructions from the contract and the
  effective tool set instead of hardcoded prose. A `## Result Protocol` block naming
  `submit_result` is emitted whenever `RequiresResult` is set; `stm_write` is mentioned
  only when actually granted; the bogus "Call finish" instruction is gone.

- **P0-3** — Honour the step budget: the worker call sites resolve it through
  `Coordinator.stepBudget` instead of hardcoding `AgentConfig.MaxSteps`, making
  `--max-steps` and `max-steps:` effective, and the `--default` team raises the worker
  budget to 120 while orchestration turns keep their own. Result finalization gets a turn
  outside the budget, and that turn now carries the attempt's conversation history so it
  reports from evidence rather than guessing. Exhaustion is recorded structurally on the
  receipt so the retry hint says "continue where you stopped" rather than "change your
  approach".

- **P1** — Authorization moved out of `OnToolCall` into `policyGatedTool`
  (`internal/team/tool_policy_gate.go`), so a denial reaches the model as a tool error it
  can adapt to instead of aborting the attempt, and it applies uniformly to the
  coordinator's own tools. The gate honours session-level permissions, matching the tool
  adapter; only a policy error such as a cancelled context still aborts. Every agent in
  the package is now built through `Coordinator.createGatedAgent`, which is what makes
  the boundary complete — `TestAgentsAreCreatedThroughTheGatedConstructor` fails the
  build if any construction bypasses it.

- **P2** — `Coordinator.validateToolGrants` runs in `NewCoordinator` and refuses to
  start when an agent would be shown a tool its allowlist does not cover, so a future
  drift surfaces before the first model call rather than as a mid-run task failure.
  `RunMetrics.StepBudgetExhaustions` counts truncated attempts from the receipts, and
  `buildRunTelemetry` emits `budget_exhausted=N` in the decision chain so
  `failure:protocol` stops hiding them.

### Verification

`TestWorkerExposedToolsAreRuntimeAllowed` was confirmed to reproduce the production
failure: with the P0-1 union removed it reports exactly the nine exposed-but-denied
tools observed in the subject run, `submit_result` among them. Likewise
`TestAgentsAreCreatedThroughTheGatedConstructor` was confirmed to fail when a single
call site is reverted to `agent.CreateAgent`.

Remaining follow-ups, not required by any observed failure:

- The stream gate's per-call recording now happens for denied calls too. Nothing consumes
  that as a signal yet; a repeated-denial counter would make a mis-declared team obvious
  from the run report.
- `rescueFinalSummary` still instructs "Do NOT call any tools" and so cannot satisfy the
  result protocol on its own. It is now redundant with the finalization turn for
  `RequiresResult` tasks and could be folded into it.
