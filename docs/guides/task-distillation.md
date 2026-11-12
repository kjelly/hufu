# Extract knowledge before cleaning old tasks

> Status: active
> Authority: guide
> Verified-Commit: 46a8793e
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

The JSON output contains a saved plan ID, the redacted frozen sources, the
proposed knowledge, and one decision per source: `retain`, `distill`, or
`discard`. Inspect the content before applying the exact saved proposal:

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
Older versions that did not save these checkpoints cannot recover completed
responses through this mechanism. No preview or checkpoint deletes sources.

## Clean a whole retired workspace

Move the old workspace into recoverable trash with the existing lifecycle:

```sh
hufu workspace delete --team my-team --yes
hufu workspace list --all
```

Create/use a fresh active workspace for the same project and team through a
normal Hufu run. Then select the old trash ID, optionally alongside current
task IDs, to extract and merge their knowledge:

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
Provider token-limit stops are rejected even when the returned JSON parses.
Unexpected EOF or oversized output triggers bounded source subdivision, up to
three levels, without accepting or repairing the partial JSON. Individual text
fragments under 2,048 bytes are not split further. Consolidation may split groups
of facts but keeps each selected fact intact. Failed calls and subdivision calls
all count against `--max-model-calls`; exhausting the budget saves no applicable
proposal and leaves sources intact. Completed sub-batches remain reusable.
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
only ordered replacement texts, aiming for 1,000 Unicode characters per text
under the unchanged 2,000-character hard limit. It may split a long claim into
self-contained facts while preserving its conditions and limitations. The
runtime binds the original citations and validates the complete draft before
checkpointing. A `null` replacement abstains and refuses the proposal while
preserving its sources. This repair can also follow an ordinary correction that leaves
an otherwise valid overlong fact; it is never repeated, and shares the same
total model-call budget. Invalid stored checkpoints are never repaired.

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
budget; application makes no further calls. A policy is limited
to 16,000 bytes; the final knowledge-and-decisions payload is limited to 1 MiB and 1,000 knowledge
items. Exceeding any budget fails closed and retains the sources. These limits
apply to the canonical model projection, rather than the runtime file size.
Existing memory database inspection retains its separate 8 MiB / 1,000-record
budget. Extraction reads canonical task results and memory, not arbitrary
files or debug transcripts. After upgrading from the earlier snapshot format,
create a fresh preview before applying. Plans and extracted knowledge remain in the
destination workspace; deleting that destination also deletes them.
