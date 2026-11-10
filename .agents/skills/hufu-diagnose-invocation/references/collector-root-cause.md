# Collector root-cause tracing

Use for failed result collection, invalid structured submissions, and repeated
format repair. The goal is to explain how the invalid value arose, not merely
to restate why the validator rejected it. Keep live inference and runtime
mutation within the invocation skill's authorization boundary; historical
diagnosis does not authorize a paid provider replay or a live workflow retry.

## Bind the evidence to the failed stage

Identify the exact task, work attempt/model execution, collector invocation,
turn and artifact references from public inspect output or the scoped debug
export. Distinguish these stages:

1. Worker work/report and original transcript.
2. Frozen collector request: system instruction, compiled prompt, claims
   schema, model/generation configuration and previous-response feedback.
3. Provider request after adapter compilation, including the effective API
   protocol and response channel (text, JSON schema, or function arguments).
4. Provider response before SDK parsing, then SDK content/function arguments.
5. Collector response as persisted, framing/argument normalization, validation,
   and any runtime binding/projection before canonical acceptance.

Follow only exported evidence references. A bundle may encode a referenced
request/response inside a JSON blob with base64 `data`; decode that declared
encoding before comparing JSON types. Preserve original exported bytes and
their identity. Do not read the corresponding live CAS, database or checkpoint
when an export omits a reference.

Check whether an exported artifact's bytes still match the persisted reference
digest before treating it as the original request. A redacted export can change
the representation. For a matching frozen request, also validate the JSON
evidence blocks inside the compiled prompt, not just the outer request object.
Text-based secret redaction after JSON serialization can damage escaped quotes
in nested reports or JSONL transcripts. Trace that boundary through the shared
redactor and context compiler. This establishes a runtime request defect when
reproduced; it does not alone prove why an upstream service returned an error.
Compare successful requests for the same defect before claiming causality.

For a slow ongoing collector, locate its persisted state in the scoped export
when the public task view omits checkpoints. `in_flight` alone establishes no
saved response; `received` has durable response bytes, and `prepared` has claims
stored for ordinary admission. Trace later invocations if present, otherwise
inspect local source binding, decision-receipt validation and commit before
attributing that wait to the provider. A work attempt's exit code zero or a UI
"waiting for LLM" message does not locate this boundary. Repeated journal reads
or CPU activity supplement the checkpoint evidence; they do not prove a deadlock
or identify a hotspot without profiling. Do not resend the model request or
restart completed work merely because post-response validation is slow.
`committed` records canonical acceptance; task verification and goal acceptance
remain separate checks.

For a collection commit reported as stale, check whether a committed event was
already appended before the failure. Compare its typed result with the result
inside the referenced accepted artifact, after verifying the artifact and event
hashes. Event-store redaction can change a secret-looking fact object after the
accepted blob was written. That is a canonicalization mismatch, not proof of a
competing writer. Align redaction before acceptance and hashing; never disable
the stale-check guard or edit the historical checkpoint to make it pass.

Label each artifact's stage. A frozen application request is not proof of the
actual wire request; an empty work-tools list does not prove a data response
function was absent. A saved collector response is not necessarily raw HTTP
response data. Identify missing stages explicitly instead of filling them from
current source or model prose. Summaries and zero counters for other validation
stages do not overrule an exact failed collection receipt.

For provider failures, compare the error summary's model with the checkpoint's
execution target and frozen request model. A coordinator model inherited through
context can misattribute a collector error; the summary alone does not prove
which model received the request. Preserve the provider's status and response
separately: a streaming `Internal Server Error` without a recorded HTTP status
does not establish HTTP 500. If no collector response was saved, do not diagnose
a schema rejection or invent the provider's internal cause. Check the declared
turn-budget semantics before calling unused capacity a retry defect: a turn
reserved for format repair need not authorize retry after a provider failure.

## Compare one failure and its repair end to end

Start with a representative failed turn and its immediate repair turn. Check
another affected task/role when necessary to establish a shared failure rather
than assuming every terminal task has the same cause.

- Compare the rejected JSON pointer and all reported type/required-field issues
  against the exact model-facing schema. Distinguish outer envelope fields,
  domain payload fields, keyed collections and runtime-owned projections.
  Show concrete values and types, not only the first diagnostic string.
- Compare system instructions, runtime-generated guidance, team instructions
  actually included, and examples at those paths. Trace each contradiction to
  its owner. In particular, an instruction about one keyed collection must not
  be assumed to prohibit arrays elsewhere. Source wording is causal evidence
  only if it reached this invocation.
- Determine which schema and response mode reached the provider, if recorded.
  A forced function selects a response channel; it does not prove strict schema
  decoding was enforced by the endpoint. Do not infer support from the model
  name, a capability advertisement, or a successful HTTP response alone.
- Check that the repair request actually contains the rejected response,
  accurate path-specific feedback and applicable schema. Determine whether
  compilation, truncation, example generation or transport removed or changed
  any of them. Separate required fields from optional fields that could have
  been omitted without losing a semantic claim.
- Compare the returned repair bytes, not a model's statement that it fixed
  them. Distinguish identical submissions from different submissions repeating
  the same invalid types, budget exhaustion, and a later anti-thrashing stop.
- Compare empty and nonempty values at outer and nested paths. For example,
  outer `{}` and nested `[]` in the same response do not establish a blanket
  conversion of every empty array. They also do not exclude a path-specific
  adapter transformation. Find the earliest recorded representation that
  contains the wrong type.

When a bounded provider probe is authorized, compare the same minimal value
through text JSON, direct function arguments and a function's nested object
argument. A direct array becoming `{}` while nested/text arrays remain `[]`
locates the failure in the upstream function-response path when visible in the
raw HTTP response. It does not identify which upstream component transformed
it. Preserve the exact request schemas and raw responses; do not substitute a
successful nested probe for an end-to-end test of Hufu's adapter and validator.

Keep findings/claims intact during analysis. Do not turn `{}` into `[]`, drop
nonempty claims, lower severity, or reconstruct unavailable submissions to
make the evidence appear valid.

## Test the competing explanations

Use the smallest relevant source surface after the evidence identifies a
boundary. In this repository, collector construction is in
`internal/team/result_collector_invocation.go`; response adaptation is in
`internal/agent/structured_response.go`; collection admission and framing are
in `internal/team/result_collection_commit.go` and `result_collection_json.go`.
Follow the selected provider/SDK implementation only as needed. These are
navigation hints, not proof the historical run used today's implementation.

Check whether the affected path parses/re-encodes JSON or preserves argument
bytes, whether streaming differs from non-streaming, and whether fallback to
text changes validation. Compare the presented schema with admission rules,
including any runtime-owned bindings. Check whether a provider proxy rewrites
response bodies before attributing the defect to the model.

Prefer existing focused offline tests or local synthetic provider fixtures to
another full agent run. A useful fixture supplies known `[]` and `{}` bytes at
the affected paths through the actual adapter and collector, observes what was
sent/received, and verifies strict rejection plus repair feedback and claim
preservation. A test of JSON decoding alone does not exercise that boundary.
Follow repository resource rules and required checks for code/test changes.
Do not add a test that merely checks prompt wording.

State the limits of each test: passing current-code fixtures can rule out a
reproduced local transformation under those settings, but cannot prove what a
historical provider returned or that a new prompt fixes that model. If the
necessary wire evidence was never captured, report that gap and propose a
bounded isolated capture of the minimal request with redacted request/response
and effective schema/channel. Do not execute external inference without
authorization or call a new provider response a replay of historical evidence.

## Attribute only what the evidence supports

Report a short chain: original work -> collector request -> returned value ->
rejection -> failed repair -> terminal stop. Separate:

- **Immediate trigger:** the exact invalid value and violated rule.
- **Origin:** the first boundary demonstrably introducing it. Runtime/SDK
  transformation, contradictory team input, and provider/model generation are
  different explanations; name the supported one or leave the unresolved
  boundary explicit.
- **Contributors:** demonstrated conflicting guidance, missing feedback,
  unsupported response mode, or other conditions explaining unsuccessful
  correction. Distinguish those from plausible preventive improvements.
- **Recovery:** a separate limitation; inability to resume does not explain
  why the original collection failed.

For both runtime and team, report confirmed cause, contributor, unsupported by
available evidence, or unresolved with a named missing observation. When only
the persisted post-adapter response is available, prefer "collector returned
invalid claims; origin upstream of this observation remains unresolved" over
"the model ignored the schema". Correct validator behavior alone establishes
neither runtime innocence nor team responsibility.

Finish with the concrete next verification that would resolve any remaining
boundary and its owning layer. Do not promise that the skill can recover
uncaptured evidence or force a binary runtime-versus-team answer.

When typed projection rejects a value that the provider-facing schema permits
(for example an integer string), compare scalar normalization order with final
payload admission. Inspect both collection turns: repeating a schema-legal
representation can exhaust repair without a model defect. Verify local and
external paths use the original compiled contract before typed projection;
retain bounds, duplicate-key rejection, semantic judgments, and replay checks.


## Transient failures and workflow closure

For an SSE error, inspect the matching SDK's typed `TransientError` and
`ErrorType` handling, not only HTTP status or provider message text. Disabling
SDK retries requires a replacement at the admitted model-request boundary;
quota-only HTTP 429 recovery does not handle a transient SSE rejection inside
HTTP 200. Test bounded resends before any stream part is delivered, distinct
admission/settlement for every request, cancellation, and refusal after partial
text or tool data. A model-step resend must retain its existing tool history
and collector work receipt; it must never restart completed tools or reset
format-repair budgets. For a data-only collector, also test reasoning and partial
function arguments followed by a typed SSE rejection. Verify the trusted
structured-response boundary buffers the entire attempt before publishing it,
discards failed uncommitted fragments, and reuses the same bounded admitted
request recovery. A function name alone does not prove that replay is safe;
executable tools must be excluded and provider-executed events must fail closed.
Normal workers must still refuse replay after partial delivery. If the failed
run did not capture the raw prefix, distinguish that observability gap from the
offline reproduction. Do not erase systemic failure guards to mask an outage.

When the user authorized executing and fixing an original command, run that
command with the final repaired source after offline validation. Assess its
actual exit code, canonical outcome, acceptance/verification, and unresolved
tasks. A saved report, a final DONE line, passing unit tests, or source repair
alone does not complete that request. If the workflow fails again, inspect its
exact new failures before another change or run. Continue correcting confirmed
in-scope defects; if an external failure persists after bounded recovery,
report the remaining blocker and actual non-success outcome explicitly.
Historical provider probes still require their own authorization.
