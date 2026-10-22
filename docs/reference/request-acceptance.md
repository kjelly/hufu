# Per-request runtime acceptance

> Status: active
> Authority: reference
> Verified-Commit: —（本次工作樹，尚未提交）
> Supersedes: —
> Superseded-By: —

`--request-acceptance` makes explicit requirements in each invocation part of
the runtime acceptance gate. It works with any team; domain-specific requirements
remain in the user's task rather than becoming permanent team rules.

```bash
hufu run --team my-team --request-acceptance -- \
  "Retrieve both source documents in full, compare their reasoning, and cite the passages."
```

The compatibility command also accepts the flag:

```bash
go run ./cmd/hufu --agent-team web-research \
  --model ollama/deepseek-v4.1-flash:cloud --request-acceptance \
  "取得兩份判決書全文，比較判決理由，並引用相關段落。"
```

A team can opt in for all its requests with `request-acceptance: true` in
`team.yaml` (or under `spec` in an `hufu.io/v1alpha1` manifest). A profile can
set the `request-acceptance` flag to `"true"`. Explicit CLI values override the
profile and team setting, including `--request-acceptance=false`.

The default is disabled. Enabling or disabling it changes the frozen execution
policy: use `--new` when changing this setting for an existing workspace, or
keep the original setting when resuming. Existing runs are not retroactively
certified.

## Requirements and execution

Before dispatching work, the sidecar model translates the request into a typed,
schema-validated contract containing at most 16 requirements. Each requirement
has a statement, an exact source quotation from the request, a required/optional
classification, a delivery contract, and one of two verification kinds:

| Kind | Evidence needed |
| --- | --- |
| `observed` | Actual tool or runtime-action output for acquisition, execution, tests, scope, or constraints |
| `response` | Delivered explanation or analysis, supported by task results or recorded response text |

Version 2 and newer delivery contracts record the requested `target` (including scope and
version), `extent` (`full`, `relevant_parts`, or `not_applicable`), `source`
(`primary` or `any`), and an explicit `allowed_substitutes` list. Every
substitute has a description, its own extent/source obligations, and a verbatim
request quotation authorizing that alternative. An empty list authorizes no
substitution. Related coverage, summaries, discovery links and attempted access
are not automatically equivalent to the requested material. Full acquisition
or primary sources are required only when grounded in the user's request;
ordinary opinion or explanation requests do not acquire invented obligations.

Only explicitly optional requests may become optional requirements. The runtime
rejects malformed, empty, ungrounded or unavailable extraction before workers
execute. It does not infer requirements with keyword matching, generate verifier
shell commands, or grant additional tools.

Analysis of specifically requested documents, datasets, code or other material
retains observed access to its actual relevant contents as a prerequisite,
separately from the final analysis. Asking a question about that material does
not by itself make the task response-only. Without an explicit requirement for
full acquisition or primary provenance, relevant parts and any suitable source
can suffice; related commentary still cannot replace the requested contents.

An invalid extraction receives one bounded pre-freeze repair call with
the original request, the rejected candidate, and deterministic feedback.
Quotes must still be verbatim substrings; the runtime never paraphrases or
replaces them itself. Only a fully validated candidate is frozen. Repeated
invalid output fails closed before dispatch; provider errors, cancellation,
and oversized output do not trigger this repair.

After extraction validation, a separate data-only model call audits the entire
contract against the original request, including coverage, optionality, target,
extent, source and permission for alternatives. Weakened, strengthened or
materially ambiguous candidates are rejected before freezing or dispatch. A
semantic rejection may use the same one-revision budget as extraction
validation: the extractor receives the original request, rejected candidate and
independent feedback, and the revised candidate must pass validation and a new
independent audit. An unchanged rejected candidate cannot be re-audited; a
second rejection fails closed. This never revises a frozen contract or retries
a completion verdict. An unavailable or malformed audit fails immediately. A
`request_acceptance_contract_reviewed` event records each valid verdict,
reason and candidate-contract digest. The audit cannot rewrite the contract.

Extraction and completion-review responses must contain one JSON value. A
single whole-response Markdown fence (unlabelled or labelled `json`) is also
accepted without changing its JSON content. Surrounding prose, extra JSON
documents, unknown or duplicate fields, and invalid contracts or evidence are
still rejected.

The resulting contract is stored as the runtime-owned typed input
`hufu.request_acceptance`. Its schema is pinned in the execution policy, and
the invocation's values are hashed and persisted in `run_inputs_resolved`.
Neither `--input` nor `--input-file` may replace it. Ordinary workers, direct
agents, and sidecar tasks receive the frozen contract. The contract supplements
the team's workflow, permissions, result contracts, and objective acceptance.
Delegation may split the work but cannot change a delivery obligation or add
alternatives. Workers continue to receive the original frozen contract even
when a coordinator's task description is less specific.

## Completion and evidence

At completion, an independent sidecar review checks both coverage of the
original request and fulfillment of every requirement. It also reviews the
proposed final answer; a worker's analysis cannot replace an omitted final
deliverable. Every check records one of `satisfied`, `missing`, or `blocked`, a
reason, and supporting source quotations where available.

Every version 2 or newer check also records a structured delivery assessment: target
`match` (`requested`, `allowed_substitute`, `none`, `unknown`), observed
`extent` (`full`, `relevant_parts`, `partial`, `unknown`, `not_applicable`),
observed `source` (`primary`, `secondary`, `unknown`, `not_applicable`) and
blocking `gaps`, with optional contextual `limitations`. Both note lists are
bounded to eight entries of at most 1,024 runes each. An `allowed_substitute` match must select an authorized
`substitute_index`; its own frozen extent/source obligations then apply.
Publication on a primary website does not make commentary the requested item.

The runtime checks these dimensions independently of the model's success
label. A `satisfied` verdict with unmet extent/source obligations, unknown or
unmatched material, unauthorized substitution or declared gaps is downgraded to
`missing`. These semantic shortfalls cannot be upgraded by protocol repair.
Useful provisional analysis is preserved, while an unmet required acquisition
still prevents a completed outcome. An absent or malformed assessment cannot
silently fall back to the older provenance-only gate.

Only unmet obligations of the specific frozen requirement belong in `gaps`.
Source redactions outside the requested passages, unavailable auxiliary
material, future outcomes and other contextual uncertainty belong in
`limitations` when they do not prevent the requested outcome. These caveats
remain visible but do not invent new acceptance obligations. Missing required
material cannot be waived by calling it a limitation: target, extent, source,
evidence binding and explicit negative verdicts still gate completion.
An extent of `not_applicable` imposes no completeness constraint; it does not
require an analysis to return that exact extent label.
For response-only requirements with extent `not_applicable` and source `any`,
the runtime canonicalizes an assessment's `any` policy marker to
`not_applicable`. This asserts no observed provenance. The marker remains
invalid for material observations or constrained provenance. Other protocol
errors identify the specific dimension and its accepted values for the bounded
repair, rather than only reporting an invalid assessment.

Version 3 independently checks material equivalence after the primary review.
It classifies the actual bound observations as `direct`, `faithful_copy`,
`authorized_substitute`, `indirect`, or `unknown`. A summary, announcement or
commentary about a requested document cannot substitute for the document,
even on its author's website. Source policy `any` permits a faithful copy;
it does not waive material identity. The check respects the frozen extent
and explicitly authorized alternatives, without inventing full-text or
primary-source obligations. It cannot upgrade a negative primary verdict.
An indirect/unknown result, malformed response or provider failure fails
closed without retrying this stage or replaying work. The result records
`material_review`, bound to the exact contract, candidate response, selected
proofs and delivery assessments. Offline audit requires this binding before
accepting a version 3 material success. Reports show the material verdict;
`inspect run` exposes only its form and adequacy, without private reasons.

The proposed final answer is an explicit `candidate-response` evidence source,
bound to its response digest. A satisfied `response` requirement must quote
that source; worker output alone cannot establish that the answer was delivered.
Offline audit rechecks the quote against the saved final answer. This source
cannot satisfy an `observed` requirement or establish an external action.

The reviewer selects `source_id` and `quote_index` from numbered source
fragments. The runtime expands that selection into the exact original text,
then binds and audits it normally; the model does not need to transcribe
quotations. Unknown sources, out-of-range indices, conflicting quote text,
and attempts to use response fragments as observed evidence are rejected.
Literal quotations from older integrations remain strictly validated.

A review with faithful request coverage whose proof data fails validation receives at most one
protocol-only repair with the same contract, sources and proposed answer.
Exact quote and source-kind validation still apply. A repair cannot upgrade a
`missing` or `blocked` verdict. Negative coverage or a missing/blocked required
item is not retried. Provider failures do
not trigger this repair, and no worker or external action is replayed.

The runtime supplies sources from completed tasks bound to the same invocation
snapshot. It verifies the winning receipt and content-addressed transcript,
matches successful tool calls with their results, and checks quoted text against
the actual output. Runtime-action observations retain their action receipt
identity; successful runtime verifier receipts are also available as observations.
Tool failures and `submit_result` claims do not establish observed
success; response text can only establish response content.

A missing or blocked required item, incomplete request coverage, invalid
evidence, or unavailable reviewer fails acceptance. A passing request review
cannot override a failed static team check. Coordinator `finish` uses the
existing bounded repair loop (up to two turns, when the workflow permits more
work); afterward it ends with `partial` / `acceptance_failed`. A direct-agent
run returns its failed acceptance immediately. No rollback occurs without the
team's existing explicit rollback configuration.

Final text, reports, and TUI results include a per-requirement section. JSON
results contain the full `acceptance.request_acceptance` value, including
snapshot, final-response digest, checks, reasons, quotations and artifact refs.
Text reports also display expected and observed delivery dimensions, gaps and
contextual limitations. `inspect run` exposes content-free check statuses, delivery enums, gap and limitation counts
and evidence refs. Offline
audit revalidates the frozen contract, response digest and quoted source
bindings, including delivery consistency, without calling a model or replaying
actions. Historical version 1 and 2 contracts remain readable and auditable under
their original schemas; they are not upgraded or retroactively certified. The
version 3 schema changes the frozen execution policy, so starting a new session
requires `--new` when replacing a workspace using the earlier policy.

## Resume, preview, and limits

Interrupted runs and targeted `retry` / `reconcile` reuse the original contract.
They do not reinterpret the prompt or silently weaken requirements. Keep
`--request-acceptance` on those commands when the original invocation enabled
it via CLI. A new invocation after completed work resolves its own requirements.
`chat` resolves each new turn independently. `--dry-run` shows that extraction
is pending and makes no model calls.

The feature requires an available sidecar model. A normal successful invocation
adds one extraction, one contract audit and one completion review call.
Version 3 adds one independent material review when an observed material
requirement has a positive primary verdict; response-only and action-only
requirements do not add this call. Protocol repair can add one primary review.
An invalid initial extraction or semantically rejected candidate can add one
shared extraction-revision call; a distinct revised candidate adds a contract
audit. No worker or external action is replayed by this revision.
These are semantic model judgments with deterministic provenance checks, not
objective proof that a source is truthful or an argument is correct. Keep
objective team verification for requirements that can be mechanically checked.
Target identity, completeness and provenance classification remain untrusted
model judgments checked for consistency; these checks do not independently
prove that an entire external document was fetched. An incorrectly classified
source can still require human or domain-specific objective verification.

Structured calls reserve 32,768 output tokens, including model reasoning. A
provider-reported token-limit stop fails explicitly even if its partial text
looks like valid JSON. Syntax diagnostics include response byte length without
exposing the raw output. Output and per-check size limits still apply; this
reservation does not relax schema validation or add retries.

Auxiliary compilation preserves the complete review prompt, including its
final-response source and schema. The caller's input envelope and context
admission reject oversized requests instead of silently cutting off the tail.

Requests are limited to 16,000 Unicode characters. Review evidence is bounded
to 128 sources, 8,000 characters per source and an aggregate context budget;
transcripts over 16 MiB are refused. Truncated or absent material is unknown
and cannot prove fulfillment. Large or tool-inaccessible deliverables can
therefore fail acceptance even after a successful attempt; the final result
must state that limitation rather than report success.
