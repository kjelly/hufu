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
   payload hashes, finding/gap counts, workset keys, lenses, each source's
   accepted artifact reads, and the required critic source IDs. Dispatch
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
   It includes each source's `gap_count` and the ordered `required_gap_refs`
   list of `(source_task_id, gap_index)` pairs. Synthesis must return exactly
   one resolution per pair. Only accepted review `record.gaps` entries count;
   prose, open questions and copies nested in critic records do not add gaps.
   Synthesis fails closed before exploration if any required source lacks an
   accepted critic dependency. The final audit independently re-derives the
   inventory, checks its current action receipt and snapshot, and compares
   every source hash and required ID before accepting the report.
5. Dispatch synthesis and `verify-review-report` in the same batch. The audit
   declares `depends-on-contracts: [synthesize-review]` and
   `on-failure-contract: synthesize-review` in team.yaml. The runtime resolves
   these names to batch dependencies and durable occurrence IDs. The
   coordinator supplies neither numeric edges nor a pipeline flag; an
   incomplete or duplicated pair is rejected before task creation. The
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
comparison later, when reviewers can no longer be redispatched. The final report audit is a static
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

The synthesizer reserves 65,536 output tokens for its semantic report fields. Report-v2 declares scope and test_verification through x-hufu-runtime-inputs, and input_review runtime-owned: the model omits all three fields, and generic runtime bindings copy complete receipt-verified native outputs and accepted source records and hashes before canonical validation. This avoids generating a large duplicate inventory over the provider stream. The stored inventory, render template, source coverage and final audit remain complete. Both normal synthesis and result-only repair receive the admitted submission shape.

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
`schemas/review-v1.json` evidence sources. The runtime supplies source task IDs,
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
runtime `view`, prefer its issued `ev_` keys in those arrays and `files_read.path`.
Do not copy keys from another task or attempt. The runtime expands them to
verified full artifact IDs before normal validation, hashing and persistence.
Forwarded source records remain canonical and are never rewritten with local
keys. Failed or omitted batch items issue no key; a key does not attest full
coverage or semantic support. `view.artifact_ref` still takes the full issued ID.

### Executed tests and failed tests

The review requires snapshot-pinned test execution with a nonnegative exit code,
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
