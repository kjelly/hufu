---
name: hufu-diagnose-invocation
description: Diagnose Hufu invocations or runs through public status, inspect, results, and debug interfaces. Use to assess progress, investigate failures or suspected stalls, attribute causes to Hufu runtime, agent team, or other layers, and recommend concrete improvements.
---

# Hufu Diagnose Invocation

Investigate the requested `inv-...` or `run-...` using Hufu's public diagnostic
interfaces. Treat their scoped, canonical output as the evidence boundary.

## Interface contract

- Start with `hufu status`, `hufu inspect`, and `hufu results`. Use JSON output
  for filtering, counting, and comparison; confirm flags with `--help` or
  full command help, such as `hufu help --all inspect run`. Diagnose with the
  same Hufu version used for the run when
  available; a binary on PATH may differ from the checkout being investigated.
- Obtain workspace, team, run, session, branch, and result identities from CLI
  output or the user's launch record. Do not reconstruct managed paths from
  project slugs or inspect process file descriptors to locate runtime storage.
- Do not query databases, parse checkpoints, or read internal audit/journal
  files directly as part of this workflow. Read-only access still couples the
  diagnosis to implementation details and can bypass scope and presentation
  guarantees. Do not scan the whole repository or `.git` for an invocation.
- If an interface lacks needed evidence, record the exact command, scope,
  version, and missing information. Try another documented diagnostic command
  or a debug export; report any remaining observability gap. Do not silently
  replace the interface with storage-specific queries or claim a missing
  feature without first checking the CLI.
- Diagnosis does not authorize retries, recovery, migration, session checkout,
  cancellation, or edits to runtime state. Recommend a concrete remedy from
  the evidence; perform it only within the user's authorized task.

The examples below assume variables have been populated from the user or CLI
output. Substitute the actual identities; do not infer IDs from task prose.

## 1. Resolve the target and scope

For a known team, inspect the current workspace:

```bash
hufu status --team "$team_name" --json --workers
```

This returns the current workspace/run/invocation identities and task counts.
It describes the active session, which may already differ from the requested
invocation. Never substitute the latest run for the user's target.

For an invocation, discover its saved results and their run/branch identities:

```bash
hufu results invocation "$invocation_id" --team "$team_name" --all-branches --output json --limit 50
```

If the team is unknown, omit `--team` for this discovery query, then narrow
subsequent queries to the matching team and exact workspace. If the user
provided an exact workspace, pass `--workspace "$workspace"` explicitly.
Follow `next_cursor` using `--cursor` with the same query filters; the first
page is not the whole invocation. Saved result rows can represent different
attempts or snapshots of a task: use canonical task identity and run summaries
rather than counting rows as tasks.

`results` exposes saved results, not every in-flight task. An empty result page
alone does not establish that a run never started or has stopped. Check current
status and the supplied launch output as well.

For historical runs, resolve their branch from the returned result metadata or
`hufu session list --workspace "$workspace" --json`. `inspect` defaults to the
active branch; `results` defaults to the active lineage. Historical `not found`
responses therefore require a scope check before concluding data is missing.
Use an exact `--branch` for `inspect`; use `--all-branches` only for deliberate
historical discovery with `results`. Do not switch branches to inspect one.

## 2. Assess progress and outcome

For the current session:

```bash
hufu status --workspace "$workspace" --json --workers --verbose
```

For a specific run and its event timeline:

```bash
hufu inspect run "$run_id" --workspace "$workspace" --branch "$branch_id" --format json
hufu inspect trace "$run_id" --workspace "$workspace" --branch "$branch_id" --format json
```

Compare successive snapshots with the same scope. Look for new events,
worker/attempt changes, task transitions, retry activity, and freshness metadata.
These are separate CLI reads, so account for their observation times while a
run is changing. Do not classify a normal inference or verifier wait as a
failure merely because the terminal printed a 30-second waiting message.

Use the run's canonical outcome, acceptance, stop reason, and unresolved task
summary together. A report being present or a worker saying "done" does not
prove acceptance. Missing or unavailable observations are not zero failures or
proof of a deadlock. Describe uncertainty when progress cannot be established.

If runtime observations leave an OS-liveness question, supplement them with
only the process or terminal already identified by the launch record:

```bash
ps -p "$pid" -o pid,ppid,comm,pcpu,etime
tmux capture-pane -pt "$pane" -S -50
```

Use tmux only when the relevant pane is known. Do not dump all process arguments,
environments, or unrelated panes. Process presence and CPU activity supplement
CLI evidence; neither proves task success or a deadlock by itself.

## 3. Inspect the relevant failure or result

Choose the task from status/trace evidence and inspect its persisted attempts:

```bash
hufu inspect task "$task_id" --run "$run_id" --workspace "$workspace" --branch "$branch_id" --format json
```

Examine the reported failure class, reason, retry disposition, verification,
recovery, and attempt identities before deciding whether the cause is a
provider error, invalid submission, semantic rejection, or failed verification.
Do not infer failure classification or recovery permission from model prose.

Read the exact saved result or its complete structured payload when needed:

```bash
hufu results show "$result_id" --workspace "$workspace" --team "$team_name" --all-branches --output json
hufu results show "$result_id" --workspace "$workspace" --team "$team_name" --all-branches --payload
hufu results report "$result_id" --workspace "$workspace" --team "$team_name" --all-branches
```

Use JSON tools to compare these exported payloads against the owning team's
contract, such as exact source IDs, collection membership, or coverage gaps.
A missing payload must remain unavailable; do not reconstruct it from a
summary. For evidence provenance or replay consistency, use the relevant
public projection:

```bash
hufu inspect evidence "$run_id" --workspace "$workspace" --branch "$branch_id" --format json
hufu inspect replay "$run_id" --workspace "$workspace" --branch "$branch_id" --format json
```

When these interfaces cannot explain the failure, export a debug bundle:

```bash
hufu debug "$run_id" --workspace "$workspace"
```

The command writes a diagnostic archive in the current directory and refuses
to overwrite one. Use a scratch directory when an export is needed. Read the
export's manifest to discover included evidence; do not assume fixed archive
member names or reach back into live storage for omitted content. Treat export
errors and omissions explicitly, keep its redaction intact, and do not upload
or share the bundle without authorization.

## 4. Attribute the cause

| Layer | Evidence to establish |
| --- | --- |
| Hufu runtime | A reproducible scheduling, contract enforcement, persistence, recovery, or projection defect, tied to the relevant task/attempt and event evidence. |
| Agent team | A team schema, prompt, workflow, action adapter, or verification rule that caused the observed rejection or prevented valid recovery. |
| Target repository | A concrete defect in the reviewed code, tests, or documentation, supported by the target revision and actual verification evidence. |
| Provider/environment | An observed timeout, unavailable service/model, missing credential or executable, or permission failure. Do not mislabel it as a target-code defect. |

Read the smallest relevant source/config/test surface after locating the
failure through the interface. Source inspection explains behavior; it does
not replace the run's canonical evidence. Keep confirmed causes, hypotheses,
and missing evidence distinct. A strict gate rejecting an invalid result is
not by itself a runtime bug; successful execution does not prove the reviewed
repository has no defects.

For every failure diagnosis, explicitly assess **Hufu runtime** and **agent
team** responsibility. Do not stop at an error message or a classification
such as `protocol`, `invalid_schema`, or `needs_human`: these describe the
observed failure, not which component caused it. For each layer, state whether
it is a confirmed cause, a contributing factor, unsupported by the available
evidence, or unresolved with specific missing evidence. Attribute other causes
to the layers above when appropriate; do not force a runtime/team binary choice.

Separate the immediate trigger from factors that made repair fail or exhausted
its budget. For a result-contract failure, compare the actual submission,
model-facing schema and instructions, validation diagnostic, and repair
responses when available. Distinguish a model violating a valid contract from
contradictory team guidance or a runtime defect in schema presentation,
validation, or recovery. Multiple layers may contribute; explain the evidence
for each rather than assigning all responsibility to the layer that rejected
the result. Record unavailable artifacts as limitations.

## 5. Recommend improvements

For each confirmed cause or contributing factor, identify the owning layer,
the concrete change, why it addresses the observed failure, and how to verify
it. Tie recommendations to relevant source/configuration locations when
available. Keep team-specific prompts, schemas, and workflow fixes in the agent
team; recommend runtime changes for reusable runtime behavior. Label proposals
that still need evidence as conditional rather than proven fixes.

Distinguish immediate recovery of the affected run from prevention of future
failures. State what work can be retained and what must be repaired only when
supported by canonical evidence or a documented read-only recovery assessment.
Check the installed CLI's supported flags and the original version/configuration
before recommending recovery commands; identify policy drift or other unmet
prerequisites. More retries, a larger budget, or a model change alone does not
explain or fix a contract defect. Preserve the authorization boundary above;
recommendations do not authorize implementation or recovery execution.

## 6. Report the finding

Scale the report to the question. Include:

- Current or terminal status, target identities and scope, and observation time.
- The decisive task/attempt transitions, immediate cause, and contributing
  factors, with an explicit assessment of Hufu runtime and agent team
  responsibility and any unresolved attribution.
- Reproducible CLI commands and their relevant output, with code/file links
  where they establish the cause. Use normal Markdown file links.
- Concrete improvements with their owning layer and validation criteria,
  separate from run-recovery steps and prerequisites. State what was actually
  tested or assessed versus what remains a recommendation.
- Any missing evidence or interface limitation that limits the diagnosis or
  the proposed remedy.

If the user asked only whether progress is normal, answer that first. Do not
turn a status check into a full audit or claim an observability defect merely
because one command presents only a summary.
