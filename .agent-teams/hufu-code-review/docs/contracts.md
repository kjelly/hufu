# Review handoff and acceptance contracts

The team reviews immutable worksets, critiques findings by source review,
synthesizes all results, and audits the final inventory before acceptance.
All review-specific adapters and schemas are owned by this team. The runtime
supports side-effect-free verification actions through its generic phase gate.

Reviewer schemas describe `requires_critique` directly: every warning/error
finding requires `true`; info findings may also request independent critique.
This domain rule stays with the team, including the report's embedded review
schema. Repair must preserve findings and severity. Write invariant assessments
only in `record.invariant_assessments`; the runtime projects the outer envelope.

The reviewer role is reusable across the primary and distinct supplemental
worksets. Its successful primary review cannot lock out the bounded supplement
round. Workset receipts still reject re-expansion of the same workset; retry
or recovery targets its existing children. Documentation reviewer and synthesizer
retain their one-shot role policy. Submit the supplement inventory and prepare
contracts in one batch; their static edge supplies the execution order.

Review identity schemas admit both the bounded primary keys (`unit-0000` through
`unit-0031`) and the single supplement round (`supplement-0000` through
`supplement-0007`). Use the current frozen item key, including for supplemental
results. Artifact directory names and the source review's original key do not
identify the new result. The native handoff still requires exact equality with
the current binding; admitting both key families does not permit substitution.
The review schemas bind `record.workset_key`, `review_revision`, and `lens`
through generic `workset_binding` runtime inputs. Workers and collectors omit
these fields, including identical copies. Runtime verifies the immutable
manifest and expansion receipt before assembling the canonical review, and
revalidates that same source after persistence or replay. The model supplies
findings, gaps and assessments; it does not reconstruct fixed identity data.

Each source-review contract declares an `equals_projection` assertion between
outer `findings` and `structured_payload.record.findings`, selecting `summary`,
`detail`, and `severity`. Counts, order, and shared fields are checked at
submission, before a result can be accepted. Empty record findings require empty
outer findings; coverage limitations remain in gaps rather than extra outer
info findings. The native inventory and final audit independently recheck this
relationship, so a worker rejection can be corrected on the same occurrence
without weakening the whole-run gates.

## Typed source gaps and one frozen supplement round

All source-review schemas (including documentation escalation and every nested
critic/report copy) require gap `description`, `missing_evidence`, `kind`,
`requested_evidence`, and `limitations`. Kind is exactly `unread`, `truncated`,
`missing_dependency`, or `semantic_uncertainty`. Requests are exact
repository-relative `{path,ranges}` entries: ordered half-open source-file
byte `{start,end}` spans; `ranges: []` requests the entire file. These are
distinct from runtime `required_reads` artifact byte coordinates. An unknown
path is a typed `unavailable_path` limitation, never guessed from prose.
Other limitation kinds are `out_of_scope`, `version_mismatch`,
`budget_exhausted`, and `invalid_request`; arrays are explicitly empty when
not applicable. Semantic uncertainty can remain after complete observation.

Before acceptance, ordinary workers must satisfy runtime-owned `required_reads`
using successful built-in view response spans, including a successful read of
an empty required artifact. Search scanning/matches do not attest full reading.
Machine-readable missing spans are repaired in the same unaccepted task;
collectors cannot satisfy another worker's read obligation.

Accepted nonsemantic gaps require `inventory-review-supplement-requests`
(`inventory_review_supplement_requests`, capability
`inventory-review-handoffs`), followed by `prepare-review-supplement`
(`prepare_review_supplement`, capability `prepare-review-supplement`) and the
optional `review-supplement-workset` fan-out using `review-v2.json`. Dispatch
the fan-out only when `item_count > 0`; empty manifests remain audit outputs.
The native inventory derives original task/gap/hash identities from accepted
records and emits receipt-bound runtime outputs. The producer consumes those
outputs and the original hash-verified frozen manifest, never model-copied
receipts, live checkout files, or historical storage mutation.
Runtime-output receipt digests cover the complete normalized map serialized
as two-space-indented JSON, matching `CanonicalizeRuntimeOutputs`; accepted
structured-payload digests use compact JSON. These representations are distinct.
The supplemental verifier preserves numbers and recomputes the original receipt
digest without reapplying redaction. Its embedded integration fixture obtains
receipts from the real runtime canonicalizer rather than the verifier itself.

Exactly one round is permitted, at most eight supplemental items and within
the unchanged 32-review/64-evidence-source capacity. Original worksets are not
redispatched. The `reviewer` role is reusable for the distinct supplemental
workset; immutable expansion receipts reject redispatch of either workset.
The `critic` role is likewise reusable for documentation escalation and
independent critiques of distinct sources. `documentation-reviewer` and
`synthesizer` retain role-level one-shot restrictions. Role reuse does not
authorize another supplement round or replay of accepted tasks.
Each supplement binds the original task, gap, hash, revision,
frozen manifest identity and requested paths. The native producer validates
paths, ordered ranges, scope grants, source identity and byte limits before
reading frozen bytes. Unavailable, excluded, stale, invalid or over-budget
requests retain typed limitations. No missing source is evidence of absence.
When different gaps materialize identical content, the producer declares that
content ID once and reuses its canonical reference in every item's inputs and
path bindings. Gap identities, outcomes and required reads remain separate.
Only path aliases with identical content metadata can be reused; conflicting
metadata fails publication. The runtime's ambiguous-declaration guard stays in
force, including current run, task and attempt checks.

`inventory-review-handoffs` inventories both original and supplemental accepted
reviews. Their findings all require the usual independent critic coverage.
The final report runtime-binds `supplement_outcomes` from that inventory,
including every typed producer limitation; the model must omit this field.
Each outcome's `limitations` is one array of `{kind, detail}` objects, matching
the native producer and inventory. Empty and nonempty limitation arrays pass
the same report binding; nested arrays are invalid. Up to 17 limitations retain
the original 16 request limitations plus a materialization limitation.

Result collection uses `collector-response-channel: function`. The model
returns the complete claims object in the sole `result` argument of the
`return_result` response function; `structured_payload` remains a field inside
those claims. The function has no executable handler and exposes no work tools.
The function channel avoids relying on plain-text JSON after repeated incomplete
text responses. It does not guarantee provider conformance or repair JSON:
malformed, mixed, or extra envelope fields remain rejected. Canonical validation,
source receipts, runtime-owned fields, and the two-turn format budget still apply.
Historical malformed responses lacked raw HTTP captures; their upstream origin
remains unresolved.
Review source schemas also declare `x-hufu-workset-paths` for finding locations.
The runtime offers path keys from the receipt-verified frozen assignment and
expands `path_key:original_source_line` to the exact stored path. A copied
misspelling is rejected during source submission rather than the later inventory.
In repository snapshot mode, the primary review schema also checks each cited
line against the assigned `review_evidence_inventory` before accepting the
result. The snapshot audit independently checks assigned paths and line bounds.
Accepted path-only repair requires an explicit frozen path that covers the
original line and preserves all findings, severities, gaps, judgments, receipts
and spent model turns.
Both critique schema versions use `x-hufu-submission-fields` to expose only
`status`, `summary`, and `structured_payload` to the model and collector.
Every judgment, full rationale, and evidence reference belongs in its keyed
decision row; the runtime binds the complete accepted source review. Optional
outer copies such as `details`, `findings`, `evidence`, and `risks` are rejected,
including null or empty values. This enforces the prompt's single location for
critique claims without dropping source findings, inherited gaps, or decisions.
In new sessions, the critic uses `result-transport: tool` for finding critique
and documentation escalation. It submits directly through `submit_result`
after reading the required evidence and settling the applicable decisions.
The active critique schema opts into `runtime_complete` at `/audit_complete`.
Runtime binds that marker, the verified source and fixed rows, and saves a
completed critique after schema, receipt and observation checks succeed.
The final report and native audit still determine workflow acceptance.
The audit re-derives request identity, outcome membership, observed resolver
evidence and supplemental source/critic coverage. A supplemental resolver must
bind the exact original task and gap. Original accepted records, findings and
severities remain immutable; every original gap stays visible even if resolved.
Coverage attests bytes returned, not understanding or semantic correctness.
Each outcome preserves its flat `limitations` array of `{kind, detail}` objects,
including nonempty restrictions for requests that could not be materialized.


1. Dispatch the workset producer and snapshot-pinned Go verifier together.
2. Expand all three review routes, including their no-op children.
3. Run `inventory-review-handoffs` after all review children complete. This
   native action validates their accepted records and emits exact source IDs,
   payload hashes, finding/gap counts, workset keys, lenses, each source's
   accepted artifact reads, and the required critic source IDs. The
   `critic-review` contract expands the receipt-verified list in order and
   persists one occurrence and its source hash per ID. Its one evidence
   dependency is the source review. Critiques cover every finding of that
   source and retain unverified decisions and their supporting evidence.
4. Synthesis receives every accepted review and its required accepted critic.
   The runtime builds and verifies this dependency list, then copies the
   records and payload hashes. The synthesizer preserves original gaps and
   uses sibling artifacts to resolve local evidence limitations.
   The runtime binds the scope and test evidence from PREPARE receipts
   and the completion, counts and coverage from `derive-review-report`.
   The native review handoff inventory is forwarded through the same mechanism.
   It includes each source's `gap_count` and the ordered `required_gap_refs`
   list of `(source_task_id, gap_index)` pairs. Synthesis must return exactly
   one resolution per pair. Only accepted review `record.gaps` entries count;
   prose, open questions and copies nested in critic records do not add gaps.
   Synthesis fails closed before exploration if any required source lacks an
   accepted critic dependency. The final audit independently re-derives the
   inventory, checks its current action receipt and snapshot, and compares
   every source hash and required ID before accepting the report.
5. Dispatch derivation, synthesis and `verify-review-report` in the same
   batch. Derivation and the final audit use the same coverage rules. The audit
   declares `depends-on-contracts: [synthesize-review]` and
   `on-failure-contract: synthesize-review` in team.yaml. The runtime resolves
   these names to batch dependencies and durable occurrence IDs. The
   coordinator supplies neither numeric edges nor a pipeline flag; an
   incomplete or duplicated group is rejected before task creation. The
   declarations and retry policy are pinned for resume.
   The report schema's `x-hufu-evidence-references` declaration rejects a
   wrong resolving task/artifact pairing during submission, with the accepted
   readers in feedback. Occurrence commit also checks the submitting task's
   reads. Source records and hashes remain pinned on repair and replay.
   `verify-review-report` audits the current session checkpoint. It checks
   current run/task receipts, native-action snapshot identities, payload hashes, workset bindings, typed
   findings and invariant claims, per-finding critic coverage, the complete
   report inventory, observed cross-workset evidence, scope and test receipts,
   and truthful coverage status. A report rejection returns `passed: false`
   and a diagnostic; the objective result assertion fails with class
   `verification`. The declared DAG edge repairs synthesis once, supplies
   Remediation Context, and reruns the dependent audit. Provider outages do
   not authorize resetting synthesis. Prior attempts and receipts remain in
   the event journal. A second rejection terminates with failed acceptance.
   Its `passed` output is blocking acceptance.
6. `finish` renders the report schema's fixed template from the validated
   payload, instead of allowing the coordinator to replace it with free prose.
   When the workflow is FAILED, `finish` with `acknowledge_failed_tasks: true`
   uses the canonical partial termination path, preserving failures and
   acceptance rather than rendering an unaudited report. Reconciliation is
   available only with an existing verified replacement of the same frozen
   contract; unrelated synthesis cannot replace an audit.

The workflow uses PREPARE → VERIFY. PREPARE actions bind the frozen review
scope, produce the immutable manifests, and execute snapshot-pinned tests.
VERIFY keeps repository invariant assessments on the primary children, then
critiques and synthesizes accepted results. Each primary review's
`verify-spec` checks that `record.invariant_assessments` repeats the attested
claims (`invariant_id`, `status`, `summary`, and optional `finding_index` /
`missing_evidence`, in ascending ID order) while
`submit_result` can still be corrected; the handoff inventory repeats the full
comparison later, after the original workset's accepted tasks are immutable.
The final report audit is a static
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

A critic with an `unverified` verdict must use `completed_with_gaps`. When all
verdicts are settled, `success` is valid; a conservative `completed_with_gaps`
is also valid if the bound source review has nonempty `record.gaps`. The
critic identifies those inherited limitations in its summary, while the
original gap inventory and its report resolutions remain source-owned. The
audit preserves that limited status, still requires observed evidence for
settled verdicts, and rejects partial status without an unverified verdict or
a source gap. A `success` envelope must never hide an `unverified` verdict.

The audit checks typed identity and coverage, not the semantic truth of an
LLM's analysis or whether an observed code fragment proves a safety property.
Security/disagreement classification still belongs to the reviewer. No
natural-language goal is parsed by the adapter.

The adapter reads the session checkpoint (the `session` document in
`runtime.sqlite`, through Hufu's read-only `actionstate` package) only from the
root selected by the runtime's `HUFU_WORKSPACE`, run ID and action invocation
ID. It requires the exact
`runtime/runs/<run>/actions/<invocation>` layout and a matching in-progress
gate occurrence. A staging-layout change fails closed. It does not search for
another session, modify reviewed files, execute shell, or run additional tests.

A rejected audit must remain a failed/partial run. An already accepted source
or report must not be silently rewritten; use a fresh invocation to correct
the review when the accepted evidence itself needs replacement. Changing the
schemas or adapter changes the frozen execution policy. Start a fresh session
for the inventory and report contracts introduced here. For an older invocation,
point `hufu resume --agent-team-search-path` at the `.agent-teams` directory of
its original revision so the original team, schemas, and adapter match the
frozen policy. For an intentional contract upgrade, preview supported controlled
contract recovery before resuming.

Result submission uses the bound contract's model-facing fields. The report
collector omits `status` and `summary` for completed work. Stored top-level findings contain only runtime
fields (`category`, `summary`, `detail`, `severity`). Rich review locations,
opaque evidence references, and critique requirements live exclusively in
`structured_payload.record.findings`. Rejected submissions must repair the
reported JSON path while retaining the complete deliverable. Invariant
assessments carry the exact injected ID and a summary within the runtime's
1000-character bound. These boundaries also apply during result-only repair.
Evidence IDs in findings, critic decisions, and gap resolutions are schema-bound
to exact `sha256-<64 lowercase hex>` references. Descriptions, source paths,
and offsets belong in separate detail/rationale/explanation fields. The same
bare IDs appear in the runtime-owned `files_read` projection. Omit that field
from model submissions; put semantic explanations in the record's detail fields.
The record's workset_key is the manifest item key (`unit-0000` style), distinct
from the runtime Workset ID. Each fan-out goal supplies the exact required
item key, reviewed revision, and lens. The schema rejects Workset IDs in the
item-key field, and the final audit requires equality with the durable binding.

Request `critic-review` once. The runtime verifies the inventory receipt and
expands one task per required source in inventory order. It binds each task to
its source and resumes the remaining expansion from the persisted occurrences.
Before a primary review is accepted, its ordered invariant
copies must match the attested ID, status, summary, finding index, and missing
evidence. The projection explicitly defaults an omitted index to null and an
omitted/empty evidence list to `[]`, matching the inventory gate. An invented
zero index or discarded missing evidence is rejected during submission, while
the original review occurrence can still be corrected.

A non-fan-out workflow contract cannot appear twice in one
batch. The synthesis contract declares `max-evidence-sources: 64`, covering the
producer's 32-review budget plus one accepted critique per review. Its full
source inventory must not be reduced to the default eight-source handoff.
The runtime joins the inventory's accepted primary reviews and their required
accepted critics into one synthesis dependency list. A single accepted critic
covers all findings in its source, including unflagged ones.
The critique schema declares `x-hufu-evidence-inputs.runtime_bind: true`:
the runtime copies the source ID, accepted payload hash, and exact source record
into `record.input_review`. The critic omits this field. A fixed collection
binds `record.decisions` to the accepted source's `/record/findings`. The model
submits only decision fields under runtime-issued keys; runtime supplies finding
indices, ascending order and the final array. Keys are bound to source identity
and payload hash, so a replacement source cannot reuse previous decisions.
The report schema uses the same runtime binding for `/input_review`: synthesis
omits the complete source inventory from its submission. Runtime copies every
accepted review and critique dependency, including each critique's nested source
record. Runtime supplies `audit_complete`, each source and critique record kind, and the
report's derived coverage, status, and counts. Synthesis supplies semantic gap-resolution
decisions, supporting evidence, and concrete limitations.
The critique and report schemas use the existing 32-finding bound to require
exactly that many decisions, in ascending finding-index order. Missing,
duplicate, reordered, and out-of-range decisions fail at result submission;
the independent report audit still rejects multiple accepted critics for one
source. Runtime retries repair the same occurrence rather than authorizing
another accepted source audit.

The final report's main sections identify issues by the original finding
summary and location and display their detail, independent verdict, rationale,
missing evidence and next step. Task IDs and full payload/artifact hashes remain
in the audit appendix, alongside the original-review to independent-critic
links. The renderer indexes findings by the schema-validated ordered decision
position, so a JSON integer spelling such as `0.0` cannot lose its title binding.
Presentation does not mutate source records, rehash payloads or turn unverified
claims into confirmed defects. Existing saved response bodies are not rewritten.

Synthesis uses low reasoning effort and a 12-step budget because source review
and critique already own semantic investigation. It groups gaps, reads sibling
records, and uses at most two evidence-gathering rounds (four focused views per
round, 8192 bytes per view). It then records the semantic gap resolutions and reserves remaining steps for
result repairs. Unresolved bounded probes produce
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

The synthesizer reserves 65,536 output tokens for its semantic report fields.
Report-v3 binds scope, test verification, coverage, completion and audit
bookkeeping through `x-hufu-runtime-inputs`. It binds `input_review` from
accepted result records and hashes. The runtime also sets the successful
status and count summary from the native completion output. Both submission
and result-only repair omit these fields and validate the assembled report.

A bounded cross-workset gap resolution cites one artifact from one other
accepted review. The schema rejects combined-artifact claims during submission,
before the report can become an accepted handoff. The final audit still requires
that exact artifact in both the resolving review's and synthesizer's accepted
`files_read`. If answering the whole gap needs multiple artifacts or reviews,
the synthesizer retains the gap and explains the partial evidence. It preserves
all original findings, gaps, and critic decisions; retention does not fail an
otherwise complete review inventory.

## Objective progress and budgets

Nine required acceptance criteria reuse the existing blocking verifications:
review scope, test scope, executed Go tests, documentation references, each of
the three complete verified worksets, the native handoff inventory, and the
final report audit. YAML anchors keep the preflight checks and milestone
verifiers identical. Every static task declares `kind: outcome`, an objective
result verifier, and its `advances` links. First-time receipt-backed criterion
passes reset the runtime no-progress counters; repeated passes do not.

A review child contributes to its route's aggregate workset criterion. An
incomplete group does not count as progress. The workset contracts keep their
zero scheduler criterion-replay allowance, so completing another child cannot
replay accepted siblings while the group is still incomplete; normal worker
attempt recovery remains controlled by the team and agent retry policies.

The handoff inventory advances only after all three worksets pass. Critics and
synthesis contribute to the final report-audit criterion, which stays unpassed
until the native audit checks the complete inventory and every required
critique. A successful PREPARE or partial review cannot satisfy final acceptance.

The producer caps the total at 32 review children, each of which can require one
sequential critic. An interval can therefore contain 32 worker attempts at the
500,000-token per-attempt guard. The no-progress token ceiling is 18,000,000,
allowing 16,000,000 critic tokens, 500,000 synthesis tokens, and 1,500,000
tokens of coordinator overhead.
The task ceiling is 40, covering 32 critics plus synthesis and the audit. The
coordinator-turn ceiling remains 8, hard enforcement stays enabled, and the
24-hour wall-clock ceiling still bounds the whole run. `max-total-tokens: 0`
remains the existing whole-run policy; it does not disable these guards.

Changed task contracts and acceptance criteria require a fresh `--new` run;
resuming an old frozen policy does not retrofit these milestones into its
completed receipts.

## Workset source and input budgets

The producer first groups diff hunks by lens, then refines those groups using
the immutable source bundle and per-item reference report. This second pass
applies to primary, documentation, and documentation-escalation routes. It
splits primary paths first, then diff hunks when only one path remains. Item
keys, paths, route annotations, reference candidate bindings, and artifact
hashes are generated from the final partitions. The existing total diff and
32-item limits are checked again before publishing any manifests; additional
splits never silently narrow the requested scope.

The team's producer action explicitly sets `max_source_snapshot_bytes: 393216`
(384 KiB) and `max_review_input_tokens: 200000`. The latter sums the planning
estimate of each assigned diff, source snapshot, and reference report,
including their headers. The estimate is one token per two UTF-8 bytes,
rounded up per artifact; it is not provider-reported usage or a guarantee of
the model's tokenizer. This leaves headroom under the unchanged 500,000-token
attempt guard for instructions, reasoning, result submission, rereads, and
repairs. Each manifest item records `source_snapshot_bytes` and
`estimated_review_input_tokens` for comparison with the actual artifacts.

Go caller/test candidates are scoped to the owning directory and packages
that import it directly or transitively in the frozen snapshot. Import aliases,
dot imports, nested modules and vendored identities are resolved from source
and the nearest `go.mod`, without running `go list` or fetching dependencies.
A shared identifier such as `Context` in an unrelated package is not a caller
reference. Missing, unreadable or malformed module ownership keeps conservative
name matching rather than silently asserting absence. This remains a candidate
heuristic, not a type-checked call graph: interface injection, reflection and
dynamic calls can require the existing supplemental-evidence workflow. The
source bundle states this limitation; unlisted paths never prove absence.

The evidence inventory uses compact JSON with the same schema and complete
per-source fields. Budget-omitted related text keeps its path, role, hash, full
byte size, required scopes and omission flags in that inventory; its section
metadata is not duplicated into the source bundle. Embedded content still has
exact bundle/source offsets, and binary/empty/unavailable markers remain
explicit. This bounds redundant framing without dropping selected identities,
raising budgets or weakening required artifact reads.

Primary text files are complete; they are never truncated to satisfy these
budgets. Related caller/test evidence has a separate shared 64 KiB content
allowance, in deterministic priority order, plus section metadata. Every
truncated related section retains its path, role, full byte count and hash,
and explicit `content_truncated: true` and `omitted_bytes` fields. These are
visible evidence limitations: reviewers must report relevant gaps and cannot
infer absent behavior from an omitted suffix. Small callers and focused tests
still appear in full. Binary, missing, and symlink observations retain their
existing explicit status markers.

Related Go paths are a syntax-based dependency inventory, not a complete call
graph. Same-package references use declared symbols; cross-package references
must import the frozen module/package and select its symbols through the
actual package name, an explicit alias, or a dot import. Nested modules retain
their own import identity. Method dependencies use receiver type identities,
so unrelated `Error` or `Unwrap` methods and unrelated packages' `Query` types
do not inflate every workset. Interface dispatch and indirect calls can still
require supplemental evidence; an unlisted caller never proves absence.

The native supplement-request wire format places the accepted gap fields
directly on each request, alongside its source task/hash, gap index, revision
and frozen-source bindings. It does not add a `gap` wrapper: nonempty requested
byte ranges then fit the runtime's unchanged eight-level output depth limit.
Consumer and final audit still compare the complete gap and source grant with
the immutable accepted record; flattening never removes ranges or limitations.
Inventory validates the accepted gap's structure and bounds, preserving even
unsupported evidence requests verbatim. The materializer checks exact file
paths and byte ranges against frozen source before reading requested content;
directory, escape, or invalid-range requests yield `invalid_request` outcomes
without worksets. They remain visible source gaps and typed limitations in the
report. Inventory never guesses a replacement filename, normalizes a request
into authority, or turns such a limitation into acceptance of the code.
Native output receipt verification uses the runtime's indented canonical JSON
representation and preserves numeric lexemes with `UseNumber`. Accepted model
payload hashes continue to use compact canonical JSON. These are distinct
contracts: adapters must not substitute one hash format for the other. Offline
supplement fixtures obtain output receipts from `CanonicalizeRuntimeOutputs`
and reject changed output values as well as substituted source payloads.

If a single primary file and indivisible diff hunk still cannot fit,
PREPARE returns `workset_item_too_large` with observed sizes, configured limits,
and remediation before publishing worksets. Split that source file or choose
an explicit input budget with sufficient attempt headroom. Increasing worker
retries does not repair an oversized evidence unit. The producer also emits a
structured `workset_item_too_large` diagnosis on this failure. It reports
primary/related content bytes, source metadata bytes, dependency path count,
fixed per-item source bytes, inventory bytes, and observed/configured input
planning units (one estimated token per two bytes, rounded per artifact).
When fixed overhead or the inventory alone exceeds a limit, the explanation
identifies why splitting primary source cannot solve it. These measurements
are provider evidence, not recovery permission; the runtime retains and
presents them without interpreting review vocabulary. Adapter and payload changes
require a fresh invocation; historical accepted artifacts remain immutable.

## Documentation reference contract

PREPARE requires explicit Markdown link targets to resolve (retaining the
existing forward-reference rule), and checks canonical workspace resource
syntax. Inline code formatting alone asserts no current existence requirement.
For an absent inline repository path or qualified Go symbol with no later
resolution, the producer records `reference_candidates` with an ID, type,
document, target, excerpt, and zero-based added-line/code-span indices (not
source line numbers). `checked_paths`/`checked_symbols` count existence
observations, not semantic approvals. `passed: true` means the deterministic
checks completed without explicit-link/syntax errors; it does not discharge
semantic review. The existing archive exclusion and forward-reference
observations remain unchanged.

Every workset binds an ordered `reference_candidate_ids` JSON array, selected
by its touched documents. Different mentions of the same target stay distinct,
even when identical text appears in different contexts or twice on one line.
Each item receives a separate verification artifact with only its candidate
entries and forward references, including explicit empty arrays. The aggregate
PREPARE report is not an assigned child input. The item report supplies
observations and the immutable source supplies context. Its deterministic
counters retain review-wide scope and are labelled `review_scope`. Each
item is bounded to 128 candidates; excess fails explicitly without truncating
coverage. No-op and non-documentation items bind an empty array.

The existing reviewer model returns schema-validated `reference_assessments`:
`required`, `historical`, `deleted`, `example`, or `uncertain`. Each assessment
cites read artifacts and explains the context. `required` must link to a finding
that requires critique. `uncertain` must link to a gap and retain
`completed_with_gaps`. The other kinds require null finding/gap indices.
Missing, invalid, or ambiguous model output never defaults to an exemption.
The handoff inventory and final report audit compare ordered IDs against the
immutable binding and check evidence and finding/gap indices. Synthesis and
critique retain the full source record and hash. These checks establish
coverage and consistency; they do not prove the model's semantic judgment.

For example, the cleanup record in `docs/reference/belief-state-validation.md`
says a proposal was removed from `docs/tmp` and nothing there is tracked. Its
missing path becomes a candidate for `deleted`, rather than failing PREPARE.
A separate instruction requiring that same path still needs its own assessment
and finding. Deterministic adapters must not recognize English/Chinese deletion
keywords or infer meaning from `.gitignore`. Explicit links remain existence
requirements even when surrounding prose describes history; use inline code
for non-navigable historical references.

The three review fan-out goals interpolate `reference_candidate_ids` explicitly,
so the worker sees the exact ordered list, including `[]`. The review schema
uses the generic `x-hufu-workset-inputs` extension to bind
`/record/reference_assessments` entries' `/reference_id` to that frozen array.
Local submission and external canonicalization reject count/order/membership
mismatches before publication, allowing correction on the same task. Commit,
accepted-dependency checks, replay/completion, and report checks repeat the
binding validation. Native handoff inventory remains an independent final gate.

## Runtime-owned gap collection

The report schema declares the generic `x-hufu-fixed-collections` contract for
`/gap_resolutions`. Membership comes only from `/record/gaps` in accepted
`schemas/review-v2.json` evidence sources. The runtime supplies source task IDs,
zero-based gap indices and array order. Critic records and prose cannot create
additional gaps. The handoff inventory and final native audit remain independent
checks against all admitted review occurrences.

The synthesizer receives a runtime manifest mapping opaque, source-hash-bound
keys to fixed gap identities. It submits `gap_resolutions` as an object keyed by
those keys. Each value contains only `status`, `resolving_task_id`, `evidence`
and `explanation`. The finite semantic choice is `retained` or
`resolved_cross_workset`; the model must inspect the evidence to choose. There
is no automatic choice on malformed, missing or ambiguous output.

The runtime rejects extra/missing/duplicate keys, identity overrides and invalid
choices before publication, allowing correction on the same attempt. It then
assembles and validates the canonical report array and pins source hashes in
runtime-owned payload metadata. Existing cross-workset artifact ownership/read
checks run against that array. Replay, dependency delivery and final rendering
reject stale provenance, even if replacement sources have the same gap count.
The final report format and independent gap audit continue to consume arrays.

For the 51-gap regression, a proposed 52nd decision is rejected at submission;
the runtime always generates exactly the 51 original identities. This change
uses the existing worker model's structured enum decisions and grants no new
tools or network access. Changed schema hashes require a fresh `--new` run.

### Runtime evidence keys

Active source-review, typed-critique and report schemas opt into
`x-hufu-evidence-keys` for worker-authored evidence arrays. After a successful
runtime `view`, prefer its issued `ev_` keys in those arrays. Omit outer `files_read`; the runtime projects successful native observations.
Do not copy keys from another task or attempt. The runtime expands them to
verified full artifact IDs before normal validation, hashing and persistence.
Forwarded source records remain canonical and are never rewritten with local
keys. Failed or omitted batch items issue no key; a key does not attest full
coverage or semantic support. `view.artifact_ref` still takes the full issued ID.

### Executed tests and failed tests

The review requires snapshot-pinned test execution with exit code 0 or 1,
no timeout, no truncated output, and a sealed evidence artifact. A failing test
is evidence about the reviewed revision and does not prevent independent code
review. The runtime binds the entire receipt into the final report, including passed=false,
exit code and diagnostic. The audit rejects any substituted receipt; the schema
rejects inconsistent passed/exit-code claims and missing failure diagnostics.
The report renders failures explicitly. Launch errors, interruption, timeout and
truncated evidence still block the workflow.

### Bounded critique navigation and failure

`critic-review` tasks use literal artifact search and targeted excerpts, at
most six view calls / 12 KiB returned text per task. The static contract, not
the source's documentation lens, selects this mode. Search results provide
absolute offsets; only shown excerpts count as observed semantic evidence.
The 48 KiB helper context still includes the finding and every successful
observed read; it is never silently truncated. Before returning a new read,
the runtime checks the complete encoded context for every bound decision row.
A read that would overflow is withheld entirely with
`decision_read_budget_exhausted`, issues no new evidence key, and leaves prior
observations usable. Stop reading and ask the helper to assess that evidence.
Missing evidence remains visible
through unverified decisions. A failed or interrupted required helper requires
a minimal blocked result with no structured_payload; an incomplete decision
collection cannot claim a successful audit.

## Complete code review

Describe the review scope in ordinary language. Interactive users do not need
`--input`, JSON, internal scope kinds, or a history-mode setting.

```bash
mise exec -- go run -p 2 ./cmd/hufu --team hufu-code-review --new --report \
  '完整審查目前專案的所有程式碼，包含未修改檔案與既有問題。'
```

The team resolves this request to the complete latest committed tree. To pin a
version, name it in the prompt: `完整審查 commit abc123 的全部程式碼，包含既有問題。`
That audits all code at the named commit, not just changes introduced by it.
Uncommitted content is not part of a complete code audit yet; the existing
uncommitted-changes review remains available.

For a large audit, the report identifies the reviewed commit and available
batches. Request the next one naturally, for example:
`完整審查 commit abc123 的全部程式碼，第 2 批，包含既有問題。`
Use the commit from the previous report so a moving branch does not change
what is being reviewed. Cross-run aggregation is not automatic.

### Internal scope contract and automation

Natural-language resolution produces a schema-validated `review.scope` object.
`kind=repository_snapshot` inventories a complete committed tree rather than a
Git diff. `head` accepts a commit or `HEAD`; `history=first_parent` is an internal
schema-compatibility value and does not trigger history traversal in this mode.
These names are team-authoring details, not required operator vocabulary.

Explicit typed input remains available for automation that needs fixed
parameters; ordinary interactive review should use the natural-language
commands above. Explicit input and a scope expression in the prompt must agree.

The producer resolves `HEAD` once, archives that commit, and inventories every
tracked tree entry with its mode, byte count, line count and SHA-256. The content
identity hashes ordered path/mode/content-hash/size tuples, including excluded
entries; dispositions and batch selection do not change that identity. Gitlinks
are recorded by object identity and never traversed. Symlinks are recorded by
link target and never followed. Dependency trees (`vendor`, `node_modules`,
`third_party`), generated Go sources, binary files, and ordinary documentation
outside the code audit are explicitly excluded with reasons. Team prompts and
AGENTS.md remain eligible configuration. Other tracked text (source, tests,
configuration, scripts) is included by default. Large primary files are unfinished
coverage, never silently truncated or classified as excluded.

The verifier now depends on `produce-workset` in the same PREPARE batch. For a
snapshot audit it reads the current accepted producer through the read-only
ActionState interface, checks its run and frozen-input identity, and archives
that exact resolved commit. It recomputes the content hash before execution;
source identity, revision, and hash are preserved in the full verification
artifact and receipt. Workset bindings and source artifact headers carry the
same identity. The native inventory and final audit reject differing reviewer
or verifier identities. No runtime/core team-name special case is involved.

The complete file inventory remains in the `repository_snapshot_inventory`
artifact and workset manifests. Receipt-bound `scope.repository_snapshot`
outputs contain identity, batch metadata, `file_count`, and an `inventory`
reference (artifact ID, producer-relative path, SHA-256 and byte size), without
the file list. Coverage gates resolve that reference only beneath the verified
producer workspace, including an admitted recovery origin, and verify exact
bytes, identity, counts and the content inventory hash before computing coverage.
Missing, replaced or inconsistent inventories, or inventories exceeding the
gate's bounded 32 MiB read, fail closed. Legacy accepted
outputs with inline inventories remain readable. The 900 KiB action-response
limit is unchanged; larger inventories are never silently truncated.

Worksets contain package/directory file lists, assigned primary source ranges
and bounded related caller/test context. Snapshot mode uses a single primary
review route. The other routes emit explicit noops. Existing issues within the
assigned primary ranges are valid findings at the original `repository/path:line`.
The native gate checks each finding against those ranges. Diff review retains
its changed-scope rules. Snapshot reference reports have zero deterministic
link/path checks, so semantic review of the supplied source remains mandatory.

### Bounded batches and truthful coverage

Snapshot mode removes the 256 changed-path selection limit. It uses actual
source bytes, the configured per-item source/token budgets, package boundaries,
and the smaller of the configured workset limit and 32 total slots. Two slots
are reserved for the mandatory no-op routes, leaving at most 30 primary
worksets. The legacy observed-budget field `changed_paths` counts inventoried paths in
snapshot mode; coverage uses the explicit inventory dispositions instead.
This preserves the 64-source synthesis limit when each review needs
a critic, and the existing no-progress budgets. It does not widen runtime caps.

The producer budgets the source bundle, evidence inventory, file list and
reference report together for each workset. It splits worksets to fit that
budget, then assigns the resulting worksets to numbered batches. Oversized
single lines can be split at UTF-8 boundaries and retain their original line
number. The total batch count comes from the final workset count. The default
is batch 1. Automation can set the typed `batch` value explicitly. Use the same
resolved commit and team budget for each batch. An out-of-range batch fails
during preparation.

The receipt-bound `repository_snapshot_inventory` records the ordered plan of
primary byte and line ranges and the selected batch's exact source artifact
hashes. Models receive compact scope context and runtime-owned workset bindings.
`inventory-review-handoffs` publishes a
`repository_snapshot_coverage` artifact with the expected and completed paths
and every excluded or unfinished path and reason. Its receipt binds
`snapshot_coverage` with counts and the artifact ID, digest and byte size.
The report uses that summary and artifact reference. Final audit recomputes
coverage from accepted sources and checks the complete artifact against it.
Every assigned workset requires an accepted review that observed the exact
source artifact bound by the plan. A file counts as completed only when all its
planned worksets were reviewed in the selected batch without evidence gaps. A
missing assigned review blocks the gate. Evidence gaps and ranges assigned to
other batches keep the file unfinished. The coverage artifact includes the
plan digest and the one-based global numbers of gap-free reviewed worksets.
These fields let operators compare reports that share a snapshot and plan.
Each report records the work performed by its own run.

Any unfinished coverage forces report coverage and terminal task status to
`completed_with_gaps`. Findings alone do not. Failed target tests also remain
visible review evidence under the existing outcome contract. Successful audit
acceptance means evidence, scope and reporting were checked; it does not mean
the program is correct. Zero findings does not guarantee zero defects.

## Workset evidence boundaries and cross-workset reconciliation

Source bundles explicitly declare `coverage_scope: workset_only`. Their path
list and diff membership are not a repository inventory. An explicit
`status: absent_at_review_revision` or a deterministic reference check covering
that path can establish absence; an unlisted path or an unobserved byte range
cannot. Required-current-reference interpretation remains model-owned.
Reviewers record concrete gaps rather than turn omitted callers, files or
validators into findings. File-byte hashes and canonical payload hashes must
be compared under the same representation before claiming corruption.

Each critic still audits exactly one accepted source, with the same bounded
artifact access and runtime-owned decision receipts. The version 2
`critique_assessment` question requires `unverified` when the claimed impact
needs unavailable evidence, even if a local omission is observable. It does
not authorize consulting another source or the live checkout.

The synthesizer uses its authorized sibling records and snapshots to resolve
source gaps, prioritizing evidence-file availability, callers/validators,
CLI/config declarations, hash representation and independent acceptance.
The existing one-artifact/one-resolver rule and bounded view rounds remain.
A resolution preserves all original source records and settled critic
verdicts; contradictory sibling evidence must be explained, not used to
silently rewrite a verdict. This remains semantic model work, not a new
native assertion that code is correct. Historical invocations are unchanged;
team policy changes require a fresh invocation under ordinary resume rules.

Review result keys accept the producer's exact bounded vocabulary: `unit-0000`
through `unit-0031` for primary items and `supplement-0000` through
`supplement-0007` for incremental items. Critic and report schemas embed the
same review definition. Workset binding still requires the assigned item key;
an allowed key from another item cannot substitute for that identity. Historical
schema drift uses ordinary controlled contract recovery and result-only repair,
preserving accepted sources and the completed worker transcript.

Supplemental results use `schemas/supplement-review-v2.json`. The runtime owns
`record.workset_key`, `record.review_revision` and `record.lens`, binding them
from the assigned immutable workset. Frozen goal metadata describes the review
scope; it is not a request to submit these fields. The collector submits only
semantic claims and omits the runtime-owned identity fields. Critic and report
source selectors accept this schema alongside primary/documentation sources.

### Runtime-owned evidence selections

Active primary, documentation, supplemental, critic and report contracts enable
`x-hufu-evidence-keys.runtime_owned_files_read`. Workers and collectors omit
outer `files_read`; native successful observations own that projection. New
citations select issued short keys from the collector's observed evidence enum.
Runtime resolves exact full IDs and checks observation membership before acceptance.
Source records copied by runtime retain their original references verbatim.

Ordinary submit_result and the collector share the same provenance checks and
repair classification. An invalid model citation may use bounded result-only format repair
after independent source, receipt and binding checks pass. Preserve all findings,
severities and gaps; do not replay work or silently guess a corrected hash. A
corrupt source, forged receipt, stale scope or repeated unrepaired response remains
blocking. The inventory and final audit retain their exact evidence checks.
