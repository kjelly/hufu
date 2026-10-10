# Review handoff and acceptance contracts

The team reviews immutable worksets, critiques findings by source review,
synthesizes all results, and audits the final inventory before acceptance.
All review-specific adapters and schemas are owned by this team. The runtime
supports side-effect-free verification actions through its generic phase gate.

Each source-review contract declares an `equals_projection` assertion between
outer `findings` and `structured_payload.record.findings`, selecting `summary`,
`detail`, and `severity`. Counts, order, and shared fields are checked at
submission, before a result can be accepted. Empty record findings require empty
outer findings; coverage limitations remain in gaps rather than extra outer
info findings. The native inventory and final audit independently recheck this
relationship, so a worker rejection can be corrected on the same occurrence
without weakening the whole-run gates.

1. Dispatch the workset producer and snapshot-pinned Go verifier together.
2. Expand all three review routes, including their no-op children.
3. Run `inventory-review-handoffs` after all review children complete. This
   native action validates their accepted records and emits exact source IDs,
   payload hashes, finding counts, and the required critic source IDs. Dispatch
   one critic per source ID in this runtime-owned checklist, not per agent name.
   Documentation escalation is a `review` record even when its worker is
   named `critic`; flagged findings still require a separate `critic-review`.
   Dispatch one critic per source review that has a finding marked
   `requires_critique`. That critic receives exactly one `evidence_from` and
   evaluates every finding index in that source. Error/warning findings always
   require critique; the reviewer also marks security concerns and material
   disagreements. Missing evidence yields `blocked` or an explicit `unverified`
   verdict, never a substituted finding.
4. The synthesizer receives every review and accepted critic. It copies their
   records and payload hashes exactly, preserves original gaps, and reads
   sibling artifacts to resolve local evidence limitations. Scope and executed
   test receipts are forwarded from the PREPARE runtime outputs as JSON by
   static `fact_refs` with the `runtime_output` selector. The runtime injects
   these values into the synthesizer's constraints independently of the
   coordinator's prose, after checking the current-run receipt, output hash,
   and frozen input snapshot. No guessed metadata path or manual retyping is
   needed.
   The native review handoff inventory is forwarded through the same mechanism.
   Synthesis fails closed before exploration if any required source lacks an
   accepted critic dependency. The final audit independently re-derives the
   inventory, checks its current action receipt and snapshot, and compares
   every source hash and required ID before accepting the report.
5. `verify-review-report` audits the current session checkpoint. It checks
   current run/task receipts, native-action snapshot identities, payload hashes, workset bindings, typed
   findings and invariant claims, per-finding critic coverage, the complete
   report inventory, observed cross-workset evidence, scope and test receipts,
   and truthful coverage status. Its `passed` output is blocking acceptance.
6. `finish` renders the report schema's fixed template from the validated
   payload, instead of allowing the coordinator to replace it with free prose.

The workflow uses PREPARE → VERIFY. PREPARE actions bind the frozen review
scope, produce the immutable manifests, and execute snapshot-pinned tests.
VERIFY keeps repository invariant assessments on the primary children, then
critiques and synthesizes accepted results. The final report audit is a static
`side_effect: none` VERIFY action, bound to that same scope snapshot. Its typed
runtime output is objectively checked for `passed: true` before the action can
be marked done; blocking acceptance cannot pass until this contract succeeds.
The three workset contracts form the required VERIFY batch. Critic, synthesis,
and audit are optional follow-up contracts for phase scheduling, so they can
consume that batch's results in later calls. They are mandatory for outcome
acceptance when applicable: the report audit requires synthesis, all sources,
and each flagged critique, and acceptance requires the audit's passed output.
Mutating actions remain forbidden in VERIFY. There is no catalog fallback.

Materialized PREPARE and audit actions must carry the current run-input snapshot.
Ordinary LLM reviews, critics, and synthesis do not carry native-action input
metadata. Their accepted handoffs require a current run/task/attempt/producer
receipt with a model execution ID and transcript, a recorded successful
objective verification, and a valid payload hash. A conflicting snapshot is
rejected if present; missing native-action metadata is never fabricated on an
LLM handoff. Source workset identities and report scope/test attestations remain
independently checked.

`completed_with_gaps` remains acceptable when the limitations are visible.
Original gaps remain in the final report even after another workset supplies
the missing evidence. The overall coverage label stays `completed_with_gaps`
if any original gap or unverified critic decision exists. Code findings and
review completion are separate: a complete review can report errors.

The audit checks typed identity and coverage, not the semantic truth of an
LLM's analysis or whether an observed code fragment proves a safety property.
Security/disagreement classification still belongs to the reviewer. No
natural-language goal is parsed by the adapter.

The adapter reads `session.json` only from the root selected by the runtime's
`HUFU_WORKSPACE`, run ID and action invocation ID. It requires the exact
`runtime/runs/<run>/actions/<invocation>` layout and a matching in-progress
gate occurrence. A staging-layout change fails closed. It does not search for
another session, modify reviewed files, execute shell, or run additional tests.

A rejected audit must remain a failed/partial run. An already accepted source
or report must not be silently rewritten; use a fresh invocation to correct
the review when the accepted evidence itself needs replacement. Changing the
schemas or adapter changes the frozen execution policy, so use `--new`.

Result submission uses a complete `submit_result` envelope (`status`, `summary`,
and the task's required fields). Its top-level findings contain only runtime
fields (`category`, `summary`, `detail`, `severity`). Rich review locations,
opaque evidence references, and critique requirements live exclusively in
`structured_payload.record.findings`. Rejected submissions must repair the
reported JSON path while retaining the complete deliverable. Invariant
assessments carry the exact injected ID and a summary within the runtime's
1000-character bound. These boundaries also apply during result-only repair.
Evidence IDs in findings, critic decisions, and gap resolutions are schema-bound
to exact `sha256-<64 lowercase hex>` references. Descriptions, source paths,
and offsets belong in separate detail/rationale/explanation fields. The same
bare IDs must appear in files_read paths; purpose text belongs in its own field.
The record's workset_key is the manifest item key (`unit-0000` style), distinct
from the runtime Workset ID. Each fan-out goal supplies the exact required
item key, reviewed revision, and lens. The schema rejects Workset IDs in the
item-key field, and the final audit requires equality with the durable binding.

Dispatch critiques sequentially, with one `critic-review` task and one source
per `agent` call. A non-fan-out workflow contract cannot appear twice in one
batch. The synthesis contract declares `max-evidence-sources: 64`, covering the
producer's 32-review budget plus one accepted critique per review. Its full
source inventory must not be reduced to the default eight-source handoff.
Select unique source review IDs, never individual finding IDs. A single
accepted critic covers all findings in its source, including unflagged ones.
The critique and report schemas use the existing 32-finding bound to require
exactly that many decisions, in ascending finding-index order. Missing,
duplicate, reordered, and out-of-range decisions fail at result submission;
the independent report audit still rejects multiple accepted critics for one
source. Runtime retries repair the same occurrence rather than authorizing
another accepted source audit.

Synthesis uses low reasoning effort and a 12-step budget because source review
and critique already own semantic investigation. It groups gaps, reads sibling
records, and uses at most two evidence-gathering rounds (four focused views per
round, 8192 bytes per view). It then submits the complete unchanged inventory,
reserving remaining steps for result repairs. Unresolved bounded probes produce
truthful `retained` resolutions, never whole-run absence claims. This limits
repeated investigation and repeated inventory reasoning without increasing
the runtime's per-attempt token limit or weakening report acceptance.

Validation:

```bash
hufu team validate --team hufu-code-review
hufu list hufu-code-review
hufu --agent-team hufu-code-review --dry-run --new 'review 最近10個feat的 git commit'
go test ./.agent-teams/hufu-code-review/reviewgate -count=1
go test ./.agent-teams/hufu-code-review/reviewprep -count=1
golangci-lint run ./.agent-teams/hufu-code-review/reviewgate/...
golangci-lint run ./.agent-teams/hufu-code-review/reviewprep/...
golangci-lint run
```

Result schemas must fit the runtime full-schema prompt budget (32 KiB canonical JSON). Ordered critic finding indices use one shared prefixItems rule; the per-count conditions only enforce exact coverage lengths. The team schema tests reject missing, duplicate, reordered, and out-of-range decisions and prevent schema growth from silently removing the full model-visible contract.

The synthesizer reserves 65,536 output tokens for the full inventory envelope, which includes verbatim dependency records. Both normal synthesis and result-only repair receive the admitted schema; diagnostics and planning must not consume the inventory output allowance.

A bounded cross-workset gap resolution cites one artifact from one other
accepted review. The schema rejects combined-artifact claims during submission,
before the report can become an accepted handoff. The final audit still requires
that exact artifact in both the resolving review's and synthesizer's accepted
`files_read`. If answering the whole gap needs multiple artifacts or reviews,
the synthesizer retains the gap and explains the partial evidence. It preserves
all original findings, gaps, and critic decisions; retention does not fail an
otherwise complete review inventory.
