---
name: hufu-run-and-recover
description: Execute or take over a Hufu task, monitor it through final acceptance, and recover failures on the original session lineage. Use when the user asks Codex to run a Hufu command or task to completion and carry out necessary recovery. Apply authorized, team-neutral runtime fixes when a confirmed defect blocks recovery.
---

# Run and recover a Hufu task

Carry the authorized task through verified Hufu acceptance or establish why
the original task cannot safely continue. Use the same workflow within a
Codex `/goal` or when invoked directly.

## 1. Establish the target and launch once

- Accept a Hufu command, a task description intended for Hufu, or an existing
  invocation or run ID. For an existing target, take over monitoring and
  recovery using its saved execution identity. For a new task description,
  resolve the team and workspace using the requested scope and project
  configuration. Ask only if material ambiguity remains.
- Preserve the user's project, arguments, model, inputs, budgets, and network
  scope. Honor a read-only or dry-run request. Follow the requested Hufu
  command and the repository's build conventions. Do not add `--new` on your
  own.
- Monitor the owned process from launch and capture its CLI output and exit
  status. Record the Hufu executable/version, invocation and run IDs, team,
  workspace, session and branch as they become available. Keep the original
  identity and associate each later recovery invocation with it. Keep any
  scratch logs private and credentials out of the launch record.
- Use [the diagnostic skill](../hufu-diagnose-invocation/SKILL.md) to resolve
  identities through public Hufu interfaces. Project-wide latest-run discovery
  is useful for an explicit takeover of the latest run. A launch with delayed
  identity output does not authorize adopting a different run or launching
  again. If launch fails before durable start, retry launch only if evidence
  shows the intended run never started and the launch left no side effects
  that could be repeated.

## 2. Monitor to a canonical terminal outcome

Use scoped `hufu status` and `hufu inspect run` output for the recorded target.
If the job is still running, continue monitoring it and use the scoped trace
when progress is uncertain. A quiet terminal, a wait message, or a report on
disk does not establish failure or acceptance. Confirm the original execution
has stopped before recovery, since a watchdog report can precede process exit.
If an authorized graceful stop is needed, use the documented procedure. A
progress request calls for an update and then continued monitoring unless the
user changes or stops the task.

For an outcome task, verify the persisted terminal event has
`outcome: completed`, `goal_satisfied: true`, and no unresolved tasks. Check
required acceptance and the requested deliverable separately from child task
counts. A command exit, completed workers, a source fix, and a passing recovery
preview are each intermediate evidence. If acceptance is unavailable, report
the verification gap without changing the user's requested mode or criteria.

## 3. Recover the original work

Use [the diagnostic skill](../hufu-diagnose-invocation/SKILL.md) to establish
the failure cause and run the installed CLI's read-only recovery preview
against the exact branch, run and task when applicable. Check the preview's
disposition, failed checks and what it will retain or execute. Choose the
narrowest eligible documented operation based on the failed boundary.

| Failed boundary | Recovery candidate |
| --- | --- |
| Interrupted or retryable work | Ordinary `resume` when admitted. Preserve accepted results. Check side-effect receipts before any work replay. |
| Work saved, result not accepted | Prefer an eligible result-only repair so worker tools are not run again. |
| Workers complete, final acceptance interrupted | Preserve accepted results. Use `--retry-acceptance` only if its exact-source preview admits it. Otherwise assess whether an eligible checkpoint before final acceptance can be resumed with zero child tasks to replay. |
| Frozen result contracts require an upgrade | Use controlled contract recovery only when eligible. Bind publication to the preview's exact plan hash and preflight the child before executing it. |

An admitted recovery may produce a new invocation/run ID or use a child
branch proven to descend from the original checkpoint. Confirm the authorized
retained, invalidated, and rerunnable work against the preview. For checkpoint
recovery, inspect the exact source event and `hufu session fork --help` before
publication. Check whether the fork will restore project files or requires
explicit metadata-only selection. Compare retained work on the child with the
source, then preflight the exact child before running it. An interrupted
acceptance check can qualify for recovery; a substantive negative verdict
requires addressing its findings. After the task starts, never use `--new`,
a fresh session, direct storage edits, or policy overrides to bypass recovery.

Run the matching recovery within the user's existing authorization. This
skill carries out authorized recovery and monitors the result. Obtain any
missing authorization for a new operation, external side effect, disclosure,
or cost outside the original scope. A preview does not grant that authority.
If the same failure recurs, reassess its cause and the recovery evidence
before another attempt. Repeat only when new evidence, a validated fix, or a
changed supported recovery disposition justifies it.

## 4. Fix confirmed runtime defects when authorized

For a demonstrated runtime defect, follow
[harden-hufu-runtime](../harden-hufu-runtime/SKILL.md) and the repository's
[fix evidence workflow](../../../docs/reference/fix-evidence.md). Reproduce
the observed failure and validate a team-independent fix at the affected
boundary. Keep team-specific behavior with the team, and preserve frozen
inputs, policy, accepted artifacts, receipts, and verification requirements.
Apply the repository's relevant validation gates. Then repeat recovery
preflight for the original target using the repaired executable before any
live recovery. Verify the fix on the live task by taking the admitted
continuation of the original lineage. Use an offline fixture when reproducing
the launch command for regression coverage. A team- or provider-specific cause
alone does not justify changing Hufu core.

## 5. Finish or report the blocker

After recovery, verify the terminal outcome for the exact resumed run and its
link to the original task. Report the original and final execution IDs,
acceptance, retained and rerun work, and any deliverable or remaining coverage
gap. Distinguish offline validation from observed live acceptance.

Stop when there is no supported safe recovery within scope. Report the exact
failed check or missing proof, the preserved results, and the prerequisite
for continuing. This includes uncertain external side effects, unavailable
checkpoints, unsupported policy drift, exhausted authority or budget, and a
recurring failure with no evidence-based next attempt.
