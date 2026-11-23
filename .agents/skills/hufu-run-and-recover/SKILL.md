---
name: hufu-run-and-recover
description: Execute or take over a Hufu task through final acceptance. Use when the user asks to run and recover a Hufu command or task on its original session lineage. Diagnose blocked work, repair confirmed defects in the owning runtime or team adapter within scope, and verify recovery.
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
| Read-only native Go action failed because of a confirmed adapter defect | Fix the owning adapter, then assess `resume --repair-action <task-id> --dry-run` for the exact failed run and branch. Bind publication to the admitted plan hash and preflight the child. |
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

## 4. Fix confirmed defects in the owning layer

An execute-and-recover request covers narrow source fixes necessary to continue
the authorized task, including defects in its team-owned action adapters.
Honor explicit read-only or no-edit instructions and obtain authorization for
changes outside the requested scope.

Follow the [fix evidence workflow](../../../docs/reference/fix-evidence.md).
Reproduce the failing boundary offline. Fix team-owned code under its team;
for a runtime defect, follow [harden-hufu-runtime](../harden-hufu-runtime/SKILL.md)
and use a team-independent regression. Preserve frozen inputs, accepted
artifacts, receipts, side-effect authority, and verification requirements.
Run the relevant validation gates and repeat recovery preflight for the
original target with the repaired source. Follow the documented
[native Go action recovery](../../../docs/architecture/execution-runtime.md)
for adapter replacements, including the plan hash and any required metadata-only
selection. Verify the fix by running the admitted continuation and checking its
acceptance.

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
