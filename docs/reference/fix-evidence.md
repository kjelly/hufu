# Fix evidence and acceptance

> Status: active
> Authority: reference
> Verified-Commit: — (working tree; pending commit)
> Supersedes: —
> Superseded-By: —

Use this workflow for bug fixes and behavior changes. Scale investigation to
the risk; documentation and routine maintenance do not require a fabricated
root cause or unnecessary tests.

## Before changing behavior

Define the trigger, expected outcome, target/version/environment and affected
boundary. Distinguish observed facts, confirmed causes, hypotheses and missing
evidence. Reproduce the failure offline when possible, retaining the necessary
redacted request, response and repair evidence through supported interfaces.
If evidence is missing, improve diagnostics and label the original cause
unresolved rather than guessing it.

Inspect related failure modes and consumers before choosing the fix. For a long
workflow, use scripted faults to cover relevant format/length errors, transport
failures, bounded repair, cancellation, checkpoint reuse and preservation of
completed work. Preserve judgments, receipts and source data; raising limits,
truncating claims or weakening a gate needs a justified design, not a green run.

## Validation and completion

Verify the original failure, relevant neighboring cases and affected integration
boundary before running broader required checks. Keep required gates and reuse
passed results for unchanged source/settings. An existing test may be the
regression if its before/after evidence demonstrates the defect.

Live execution stays within existing authorization for the named target,
operations and budget. A new permission is needed only outside that scope.
Offline success, one compliant model response and a passing probe demonstrate
their tested scope; none alone proves the whole target workflow completed.
When that workflow fails again after a claimed fix, revisit the evidence,
acceptance boundary and recovery design before another patch or full rerun.

Record the remaining limitations and a supported recovery path that retains
completed work. Do not use the user repeatedly rerunning the full workflow as
the primary test strategy. Diagnostic improvements can be delivered separately
while the original workflow remains unresolved.

## PR evidence contract

Every non-documentation PR includes exactly one `hufu-fix-evidence` fenced JSON
block, as in [.github/pull_request_template.md](../../.github/pull_request_template.md).
Use actual observations, commands and artifact/test references; do not invent
successful checks. Do not include credentials or raw private model responses.

| Field | Contract |
| --- | --- |
| `kind` | `bugfix`, `behavior`, or `maintenance` |
| `acceptance` | Concrete trigger and expected observable outcome |
| `cause_status` | `confirmed`, `hypothesis`, `unresolved`, or `not_applicable`; bug fixes cannot use `not_applicable` |
| `cause`, `cause_evidence` | Explanation and references; confirmed causes require evidence |
| `regressions` | For bug fixes/behavior changes, at least one repository test with `path`, `scenario`, `before`, and `after`; existing tests are allowed |
| `checks` | Required source-validation commands, each with `command`, `result` (`passed`, `failed`, `not_run`), and `evidence` |
| `workflow.level` | `offline`, `bounded_probe`, `full_target`, or `not_applicable` |
| `workflow.claim` | `partial`, `source_fix`, or `workflow_complete` |
| `workflow.scope` | Version/environment, workload and boundary actually tested |
| `workflow.evidence` | Artifact/output references; required except for `not_applicable` |
| `workflow.limitations` | Required unless full-target acceptance is claimed; explain why if no workflow applies |

`source_fix` and `workflow_complete` require passed source checks; a bug fix
also needs a confirmed cause. `workflow_complete` additionally requires
`full_target` evidence. An unresolved diagnosis or failed/deferred required
check uses `partial`. Deferred live validation belongs in workflow limitations;
it must not be described as executed or expand authorization.

Documentation-only exemption is deliberately narrow: root AGENTS/README/
CONTRIBUTING and Markdown/reStructuredText under `docs/`. Agent-team prompts,
schemas, scripts, workflows and skill changes are not automatically exempt.

## CI enforcement and limits

The existing `ci-success` gate validates evidence on PR code runs. A separate
`fix-evidence-current` check revalidates PR-description edits without rerunning
heavy Go checks. Both read the current description using a read-only PR token,
while keeping source selection bound to the event's base/head SHAs. A description
edited during a long CI run does not leave the aggregate checking stale prose;
changed source revisions require validation for that revision. Read failures
fail closed, without printing PR content or tokens.

Repository administrators should require **both** statuses in
branch protection/rulesets to block merges after a description edit invalidates
the evidence. Workflow files do not configure server-side branch protection.

The checker validates declared scope, test-file references and consistency. It
does not execute commands from the PR body, verify external artifacts, establish
root-cause truth or certify model reliability. Existing CI tests/lint and human
review of the evidence remain required. A green evidence check is not product
acceptance.
