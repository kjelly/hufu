# Extract knowledge before cleaning old tasks

> Status: active
> Authority: guide
> Verified-Commit: — (working tree; pending commit)
> Supersedes: —
> Superseded-By: —

`hufu workspace distill` asks the configured team sidecar model to propose
durable knowledge and retention decisions. It supports explicit task IDs in
the current managed workspace, retired workspaces in managed trash, or both
in one proposal. Duplicate knowledge across the selected sources can become
one memory with multiple source references. There are no special team names
or hard-coded task-type rules.

Run these commands from the registered project directory. First preview:

```sh
hufu workspace distill --team my-team --task 12,13 \
  --policy 'Keep verified procedures and project decisions; discard transient reports.'
```

To review knowledge from protected or incomplete sources without granting any
cleanup, add `--extract-only`. The saved plan retains the original protection
flags and snapshot hashes; every source is unconditionally retained, even when
the model proposes discarding it. Selected knowledge still requires operator
review and application. `review` and `refine` inherit this restriction. This mode
does not complete, recover or approve the original task, and cannot be combined
with `--probe-batch` or added when applying an existing plan.

The JSON output contains a saved plan ID, the redacted frozen sources, the
proposed knowledge, and one decision per source: `retain`, `distill`,
`discard`, or the runtime-derived `extract_only`. The last action keeps the
source while preserving knowledge approved from its independent fragments;
models cannot submit it directly. Inspect the content before applying the exact saved proposal:

```sh
hufu workspace distill --team my-team --apply PLAN_ID --yes
```

Application makes no additional model calls. It confirms the reviewed
knowledge in the destination's canonical `context.sqlite` before cleanup.
Knowledge stores source IDs and snapshot hashes; the saved proposal retains
the redacted source text, policy, model identity and reasons. Saving knowledge
is atomic and retries preserve its identity. A model failure, invalid response,
changed source or knowledge storage failure prevents cleanup. A partial purge
failure leaves the saved knowledge intact; fix the reported lifecycle problem
with `workspace doctor` and retry the same plan.

## Refine a proposal before applying it

If a completed preview still contains transient reports, repeated advice or
historical evidence gaps, refine its **saved plan ID** before confirming it:

```sh
hufu workspace distill refine PLAN_ID --team my-team \
  --model ollama/my-model --max-model-calls 256 \
  --policy 'Keep reusable rules and verified failure patterns. Merge equivalent claims. Remove one-off statistics and obsolete reports; do not promote suspicions into verified defects.'
```

This creates a new saved plan with a new ID; it never edits the parent plan,
confirms memories or deletes sources. The model defaults to the parent model.
The call budget includes filtering, contract repair and merging. Refinement
sends the parent's proposed knowledge and its source references, rather than
resending the original task logs. It does not independently verify claims
against today's code. Filter and merge batches are bounded; semantic duplicates
in different final merge groups can still require another reviewed refinement.

Review the new plan without model calls:

```sh
hufu workspace distill show NEW_PLAN_ID --team my-team
hufu workspace distill show NEW_PLAN_ID --team my-team --full
```

The default review contains `draft`, source snapshot hashes, audit workspace
paths and `refinement`. `--full` also exports complete frozen sources and the
application inventory. `refinement.parent_plan_id` identifies the immutable
parent. `refinement.evidence.batches` records one filtering judgment and reason
per parent fact. `refinement.evidence.knowledge` connects merged text to those
parent facts: `fact_0` means the parent's `draft.knowledge[0]`, using zero-based
indices. `retain` in a filtering batch preserves the original fact text;
`discard` omits it from the child plan. Final `draft.knowledge[].source_ids`
are rebuilt by the runtime from the cited parent facts, never copied from
model-authored original source IDs. Follow `parent_plan_id` with `show` to
inspect earlier claims or `show --full` to inspect their frozen evidence.

Apply only the exact child plan you reviewed:

```sh
hufu workspace distill --team my-team --apply NEW_PLAN_ID --yes
```

Review and apply revalidate parent identity, source inventory, filtering
judgments, selected-fact coverage and the reconstructed child draft. Apply also
rechecks live source fingerprints. A missing parent, changed source, forged
reference or incomplete merge fails closed. When refinement omits any parent
fact from a source, that source becomes `extract_only` if knowledge remains,
or `retain` otherwise; omission never authorizes deletion. Existing retained
or protected sources remain retained. A source with no omitted facts retains
the parent's cleanup disposition. Filtering decisions describe knowledge
selection, not a new authorization to delete the original source.

After interruption, repeat the same `refine` command. Verified whole-fact
filter and merge checkpoints are reused under the same parent, policy, model,
destination and admitted generation settings. No partial plan is published on
failure. Changing the parent or guidance creates a different scope. Refinement
ancestry is bounded to eight levels; every parent plan must remain available.
Keep the parent and extraction audit workspaces for provenance and recovery.

After bounded JSON, schema or reference correction fails, filtering and merging
can subdivide groups of whole proposed facts under the existing depth and call
limits. Every child still passes the unchanged schema, privacy, stage and
coverage checks; a fact is never fragmented. An indivisible failure,
cancellation or exhausted limit still stops publication and preserves sources.

## Select the exact knowledge you reviewed

To accept only part of a proposal, create an operator-selected child plan:

```sh
hufu workspace distill review PLAN_ID --team my-team --keep-knowledge 0,4,7
hufu workspace distill show REVIEWED_PLAN_ID --team my-team
hufu workspace distill --team my-team --apply REVIEWED_PLAN_ID --yes
```

`--keep-knowledge` lists zero-based indices in the saved parent's
`draft.knowledge`. At least one index is required; negative, repeated and
out-of-range indices fail closed. The command makes no model calls and saves
exactly the selected original texts, with their existing source references,
as a separate plan. It does not edit the parent or confirm any knowledge.
Repeating the same selection produces the same saved plan ID.

The child records `refinement.operator_selection` and a complete disposition
inventory. Review and apply reconstruct the selection from the immutable
parent; changed text, substituted indices or incomplete evidence are rejected.
Unselected knowledge remains available in the parent and its frozen evidence.
Omitting a claim retains its original source under the same conservative
refinement rules. Applying a selected plan does not silently approve the
parent's other knowledge, and `extract_only` still keeps its source.

The preview's `--model` selects the maintenance sidecar model. The team's
execution routes still come from the effective `hufu.yaml`, using the same
configuration for static preflight and coordinator admission. Undefined or
invalid routes fail before inference; a model override does not replace worker
route policy.

Model admission and call evidence use an independent durable workspace under
`maintenance/task-distillation/` in the destination. The plan names that audit
workspace. Drafting cannot overwrite the active task session; failed drafts keep
their call evidence there for diagnosis.

List maintenance audit workspaces, including failed drafts, without creating a
workspace or making model calls:

```sh
hufu workspace distill list --team my-team
```

Use the listed audit workspace with `hufu status --workspace PATH --json`,
`hufu inspect`, or `hufu debug` to examine that maintenance invocation.

Validated extraction and merge batches are checkpointed in the destination.
After interruption or failure, repeat the same preview command: unchanged frozen
sources, destination scope, policy, model and admitted generation settings reuse
completed batches. Changed inputs create a separate checkpoint scope. Checkpoint
hits make no model calls; each explicit invocation grants its own call budget.
The saved plan's `audit_workspaces` retains the audit locations of reused batches.
Runtime aggregates that exceed a legacy checkpoint contract use separately
versioned records; accepted legacy records remain unchanged and reusable.
Older versions that did not save these checkpoints cannot recover completed
responses through this mechanism. No preview or checkpoint deletes sources.

## Clean a whole retired workspace

Move the old workspace into recoverable trash with the existing lifecycle:

```sh
hufu workspace delete --team my-team --yes
hufu workspace list --all
```

Create a fresh active workspace for the same project and team without starting
agents or requiring their tools. `ensure` preserves an existing active workspace
and never restores, overwrites or purges a retired source:

```sh
hufu workspace ensure --team my-team
```

Then select the old trash ID, optionally alongside current task IDs, to extract
and merge their knowledge. Maintenance validates the team configuration but
does not connect to worker MCP servers or execute action providers:

```sh
hufu workspace distill --team my-team --trash TRASH_ID --task 12,13
hufu workspace distill --team my-team --apply PLAN_ID --yes
```

An accepted `distill` or `discard` decision permanently purges that entire old
workspace, including its databases, logs and artifacts. The source is locked
and revalidated immediately before purge. A `retain` decision leaves it in
trash. Existing shared, confirmed persistent memories are knowledge inputs;
unconfirmed memories are not silently promoted. A retired source containing
current unresolved tasks, recovery state, or agent-private, pinned or time-bounded confirmed
memories is protected:
it cannot be purged by distillation. Private memory is never shown to the
model or widened into team-shared knowledge. Session, branch, task and attempt
scopes are provenance for explicit knowledge promotion: their confirmed
memories can be considered by the model, with applicability preserved and the
result reviewed before application. Failures from superseded historical runs
remain source evidence; they do not reopen tasks in a completed checkpoint.
Fork checkpoints from historical branches are also evidence; the complete
retired workspace and its branch graph are selected together.

## Clean selected tasks in the current workspace

Only completed tasks without live dependencies can leave the active task
projection. Nonterminal tasks, workflow/contract-bound tasks and results
referenced by another task or branch are protected. The runtime validates
these restrictions independently of the model's judgment.

Task removal events, checkpoint changes, active branch state and optional
result-cache removal commit in one SQLite transaction. Immutable event history,
typed results, receipts, artifacts and historical branch snapshots remain
available for evidence and time travel. This mode cleans active task state;
it does **not** erase historical evidence or promise to shrink `runtime.sqlite`.
Purge the complete retired workspace to reclaim that storage after extraction.

## Limits

The first version accepts explicit selectors; it does not guess scope from task
prose, run on startup, or schedule automatic deletion. The policy is interpreted
by the model through a schema-validated proposal. Missing, ambiguous or invalid
model output fails closed, with no keyword-based fallback.

The runtime streams and verifies the complete retired event journal; the
runtime database's total size is not limited to 8 MiB. Canonical task replay
uses the same reducer as ordinary recovery, without loading the journal into
memory. Fingerprints cover every event, including historical evidence omitted
from the model projection, so changed or corrupted sources still block cleanup.

Small inputs use one model call. Larger inputs are split into ordered UTF-8
fragments without truncation, with at most 64,000 bytes of JSON data and 16,000
bytes of output per call. A retained fragment retains its complete source.
The 1,000-knowledge-item schema limit applies to each model reply. A canonical
aggregate can keep more independently validated facts, bounded by the unchanged
1 MiB serialized draft limit; text length, privacy and provenance checks still
apply to every item. It never drops selected facts to fit a per-call item cap.
When some fragments are retained and others approve knowledge, the runtime
keeps the complete source and preserves the approved facts as `extract_only`.
Applying that plan saves the reviewed knowledge without deleting that source.
Protected sources remain `retain` and cannot contribute knowledge.
Provider token-limit stops are rejected even when the returned JSON parses.
Unexpected EOF or oversized output triggers bounded source subdivision, up to
three levels, without accepting or repairing the partial JSON. Individual text
fragments under 2,048 bytes are not split further. Consolidation may split groups
of facts but keeps each selected fact intact. Failed calls and subdivision calls
all count against `--max-model-calls`; exhausting the budget saves no applicable
proposal and leaves sources intact. Completed sub-batches remain reusable.
During consolidation, a source-reference violation that persists after the
ordinary correction also partitions the group within those same depth and call
bounds. Each child must pass all reference and selected-fact preservation gates;
rejected replies are never checkpoints. A failed indivisible fact or exhausted
subdivision still fails closed. This recovery does not subdivide extraction
reference failures, schema/privacy violations, cancellation or budget failures.
On resume, preflight discounts only complete checkpoints validated against the
same frozen request, policy and source bindings. The minimum budget counts
unfinished extraction batches plus a consolidation reserve; actual repairs and
merges still share the total call limit. Split hints do not count as completed.
The model-facing schema restricts references to the exact outer source IDs of
the current batch, including subdivision suffixes; IDs embedded in evidence
are not selectors. Unknown, repeated or missing decision IDs and unknown
knowledge references receive at most one corrective model call per request,
with the same frozen evidence and explicit validation feedback. The runtime
never guesses ID mappings or accepts the rejected response. This correction
counts against the same call budget. Valid checkpoints from before this prompt
strengthening remain reusable after full source-reference validation.
Whitespace-only decision rationales receive the same bounded correction and
are diagnosed separately from unknown or repeated source IDs. Each request has
one ordinary correction opportunity, even if the next response has a different
contract violation. Overlong knowledge has one dedicated text-only repair:
after every other schema, protection, citation and privacy rule passes, the
runtime freezes decisions, source IDs and unaffected facts. The model returns
only ordered replacement texts: each target has one string, or a nonempty
array of strings when splitting a fact. Both forms preserve the exact returned
text and the same target binding, aiming for 1,000 Unicode characters per text
under the unchanged 2,000-character hard limit. It may split a long claim into
self-contained facts while preserving its conditions and limitations. The
runtime binds the original citations and validates the complete draft before
checkpointing. A `null` replacement abstains and refuses the proposal while
preserving its sources. This repair can also follow an ordinary correction that leaves
an otherwise valid overlong fact. If a replacement violates JSON or its schema,
one further text-only correction receives the precise diagnostic and the
original frozen targets. Split parts must stay inside their target entry.
Rejected replacement text never becomes evidence. Invalid corrections,
semantic violations and abstentions fail closed. All
calls share the same total model-call budget. Invalid stored checkpoints are never repaired.

Distillation requests `reasoning_effort=none` so its bounded output budget can
serve the JSON answer rather than the compactor's requested reasoning. Provider
support varies; a token-limit stop is still rejected even when no answer text
was returned. Changing generation settings starts a separate checkpoint scope.

Other model-produced JSON, schema and consistency violations use that same
ordinary correction opportunity, rather than a separate retry policy for each
error message. Every replacement is checked against the complete contract,
including protected sources, secret/redacted content and selected-fact
preservation during consolidation. Errors identify a stable code and field
path without printing the rejected text. Invalid stored checkpoints remain
errors; they are not repaired by another model call.
When a rejected candidate is valid JSON and fits the bounded prompt, its
JSON-aware redacted contents accompany the correction diagnostic. Malformed
or oversized candidates are omitted, never truncated. Candidates are untrusted
repair context, never checkpoints or evidence. A citation to a retained or
discarded source still fails validation; the runtime does not promote a
retention decision to `distill` to make knowledge acceptable. Citation errors
also report the conflicting decision action without publishing candidate text.
Legacy merge candidates that discarded selected facts were never acceptable
merge results. They remain unchanged and are bypassed through a separate merge
contract key; valid legacy merge checkpoints are still reusable.

Identical knowledge text is coalesced only after every original item's content
and references have passed validation. The runtime unions all citations and
preserves the text bytes, including indentation. Different claims or formatting
remain distinct; semantic merging belongs to the model and reviewed proposal.

To verify a failing extraction batch with a small, explicit model-call budget:

```sh
hufu workspace distill --team my-team --trash TRASH_ID --probe-batch 5 \
  --max-model-calls 12 --model PROVIDER/MODEL --policy 'The original policy'
```

This mode checks only the selected 1-based batch, including its bounded
subdivision/correction. It writes verified extraction checkpoints that a normal
preview can reuse with identical source/policy/model settings. Its JSON kind is
`distillation_probe`: it has no applicable plan ID, confirms no knowledge and
cannot delete sources. `--max-model-calls` bounds only the selected probe's
physical calls; the full source inventory retains its ordinary size limits.
It cannot be combined with `--apply` or `--yes`. A successful probe verifies
that batch, not the complete extraction or final consolidation.

A premature EOF observed at Hufu's provider HTTP boundary permits one resend
of the identical frozen draft request after a one-second, cancellable wait.
The partial response is rejected and the source is not subdivided because of
a transport failure. Admission, checkpoint and unmarked generator errors do
not enable this resend. An accompanying partial text result also prevents it.
Both the resend and provider-internal recovery consume the same
`--max-model-calls` HTTP-send budget; metadata requests outside model invocations
are excluded. Exhausting that budget sends no additional model request. Valid
checkpoints remain reusable with unchanged source and generation settings.
Selected knowledge is deduplicated and then merged in bounded model batches;
merging cannot discard an already selected fact. Exact duplicates across merge
batches share one memory and union their provenance; semantic merging happens
within each merge batch. Source identities are bound by the runtime, never
reconstructed by the model. Progress goes to stderr; stdout remains plan JSON. Large sources can require
many calls: check the reported extraction batch count and allow for model
latency and provider charges. Cancelling a preview never deletes its sources.

The complete extraction is bounded to 64 MiB of source text. By default, extraction
and merging together may use at most 256 model calls; oversized requests are
refused **before** provider admission. Use `--max-model-calls N` on the preview
to explicitly raise that budget, up to 4,096 calls. The saved plan records the
budget; application makes no further calls. A policy is limited to 16,000 bytes.
Each model reply allows at most 1,000 knowledge items; the final canonical
knowledge-and-decisions payload is limited to 1 MiB. Exceeding any budget fails
closed and retains the sources. These limits
apply to the canonical model projection, rather than the runtime file size.
Existing memory database inspection retains its separate 8 MiB / 1,000-record
budget. Extraction reads canonical task results and memory, not arbitrary
files or debug transcripts. After upgrading from the earlier snapshot format,
create a fresh preview before applying. Plans and extracted knowledge remain in the
destination workspace; deleting that destination also deletes them.
