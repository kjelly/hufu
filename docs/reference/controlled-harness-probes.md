# Controlled harness probes

> Status: active
> Authority: reference
> Verified-Commit: `720cf1e1`
> Supersedes: —
> Superseded-By: —

`hufu improve probe` compares a baseline worker with one authored candidate under
the same inputs, model settings, result schema, verifier, tools and budgets.
The initial execution profile is `structured_only`: a worker can only call
`submit_result`. It supports classification, extraction and other structured
responses. It does not provide an OS sandbox for shell or coding tasks.

The [implementation plan](../implementation-plans/controlled-harness-improvement.md)
defines the configuration and fixture formats. The runnable offline example is
[`internal/improver/testdata/extraction/`](../../internal/improver/testdata/extraction/experiment.yaml).
The manual runner and bounded refinement are available. Bundle export is the
final stage of that plan.

## Run the offline example

Build once and use the same binary for execution and resume:

```bash
go build -o /tmp/hufu-probe ./cmd/hufu
/tmp/hufu-probe improve probe plan \
  --config internal/improver/testdata/extraction/experiment.yaml --json
/tmp/hufu-probe improve probe run \
  --config internal/improver/testdata/extraction/experiment.yaml \
  --workspace /tmp/hufu-probe-workspace --json
/tmp/hufu-probe improve probe show \
  --experiment extraction_pair --workspace /tmp/hufu-probe-workspace --json
/tmp/hufu-probe improve probe resume \
  --experiment extraction_pair --workspace /tmp/hufu-probe-workspace --json
```

Use a new workspace path for this example. Its parent must exist. Control
directories are private (`0700`), and persisted objects are private (`0600`).
An existing experiment ID is never overwritten; use `resume` to continue it.

`plan` reads only the configured files. It creates no workspace, provider or
coordinator, and reports that runtime preflight is pending. `--out <new-file>`
optionally saves the preview without overwriting an existing file.

`run` freezes the configuration, team files, candidate, schemas, case inputs and
scripted responses. It preflights the effective policies and generation settings
before any provider invocation. `fixture` execution uses an in-process,
socket-free scripted transport; it needs no model server or credentials.
The example records a baseline failure and candidate success, but the resulting
decision is always `simulation_only`. `simulation_decision` describes the
simulated comparison. These values do not establish real model quality.

`llm` configurations use the existing host provider settings and admission path.
They require an explicit positive worker context window. Model, generation,
budget and template overrides on the command line are rejected: those values
belong in the frozen experiment configuration. Real model evaluations are
outside the offline development acceptance scope.

## What is compared

Only the configured worker instruction body or an explicitly allowed pure-text
skill body may differ. The materializer rejects changes to tools, frontmatter
policy, models, generation settings, schemas, verifiers or other protected
surfaces. Each arm and attempt gets a fresh private child runtime and provider
cursor, with memory and auxiliary model work disabled.

Fixture assertions address the authored payload, for example
`/structured_payload/category`. The adapter maps this to the canonical runtime
payload envelope at `/structured_payload/value/category`. Fixture verifier mode
`blocking` maps to the existing task verifier's `success` mode.

A passing observation requires a canonical submitted payload, its bound schema
and receipt, and a successful objective task verification. A worker's prose
does not establish success. The child's whole-run result remains subject to
normal acceptance semantics: without whole-run acceptance it can be
`unverified` even when the individually verified task passes this comparison.

Provider usage comes from canonical cost events. Missing usage stays unknown
and retains its reservation. Explicit zero usage is accepted only when the
trusted transport recorded its presence; an SDK's default zero does not prove
zero usage. Fixture token counts are scripted, not measurements of a real
model. Reports include failed attempts and distinguish executor totals from
the complete pipeline budget.

## Recovery and evidence

The parent journal records admission, actual child identities and dispatch
authorization before the first provider call. Completion is importable only
after the child runtime and callbacks have drained. A completion marker binds
the imported canonical evidence to its exact stage, generation and lease.

`resume` checks the frozen binary identity and policy. It reconciles admitted
work before starting new stages, without resetting deadlines, attempts or token
reservations. Missing completion evidence, unknown usage or conflicting
identities block further work. Absence of a marker never proves that a provider
call was not sent. A completed experiment resumes without dispatching a provider.

`show` is read-only: it does not create a missing workspace, migrate its
database, load credentials or start a runtime. It reconstructs the report from
the journal and verifies imported evidence, rather than trusting a saved
decision. Completed child directories can be removed without losing imported
evidence; the parent object store and journal must be retained.

Run manifests reference the exact imported child event log for policy,
receipts, verification and usage. These references preserve original child
identities and hashes. They are not reconstructed receipts.

## Output and cancellation

`--json` writes only the report to stdout; progress and diagnostics use stderr.
`run` and `resume` also support `--tui` on a terminal, using the existing task
dashboard. `--json` and `--tui` are mutually exclusive. Plain output contains no
terminal controls. Cancelling stops new admissions, drains active work and
records the resulting evidence before releasing the workspace lease.

| Operation result | Exit code |
|---|---|
| `eligible_for_review`, `no_change`, `simulation_only` | 0 |
| `rejected` | 1 |
| Invalid input, `blocked`, `inconclusive`, integrity error | 2 |
| Cancelled execution | 130 |
| Successful `show`, including a historical failed decision | 0 |

No command in this workflow applies changes to the source team, promotes
memory, approves adoption or deploys a candidate.

## Bounded refinement

```bash
/tmp/hufu-probe improve refine \
  --config internal/improver/testdata/refinement/experiment.yaml \
  --workspace /tmp/hufu-refinement-workspace --json
```

The [complete refinement fixture](../../internal/improver/testdata/refinement/experiment.yaml)
uses one round: baseline probes, a procedure proposal and its probes, a domain
proposal and its probes, one composition and its probes, deterministic candidate
selection, and optional reflection. Both proposers receive the same baseline
evidence. Domain proposals never receive procedure results. Each role uses its
own schema and bounded child runtime with only `submit_result`.

The fixture deliberately makes both individual candidates pass and their
composition fail. The selected candidate is `domain`, with four scored
executor slots and 830 scripted tokens across all eight stages. This remains
`simulation_only`; it illustrates control flow rather than model improvement.
Use `probe show` and `probe resume` with experiment ID `extraction_refinement`
to inspect or resume it. Source files are unnecessary after freezing.

A proposal payload has this shape:

```json
{
  "schema_version": 1,
  "hypothesis": "Explicit instructions address the observed failure.",
  "observed_failure_refs": ["probe_0001"],
  "changes": [{
    "surface_id": "surface_001",
    "before_hash": "<exact hash from the registered baseline surface>",
    "replacement_text": "Return the requested category exactly."
  }],
  "expected_behavior": "Satisfy the frozen result verifier.",
  "applicability": "The supplied classification task."
}
```

Reference strings are logical stage IDs within the experiment. The runtime
resolves them to committed canonical evidence; the model cannot supply paths.
Failure references must identify actual baseline failures. Changes require
unique registered surfaces, matching base hashes and bounded UTF-8 text;
templates, secrets and protected changes are rejected before probe admission.
There can be at most 64 changes in one proposal.

Composition uses a strict union:

- `{"schema_version":1,"action":"keep_baseline"}` creates no new candidate.
- `{"schema_version":1,"action":"use_candidate","candidate_slot":"domain"}`
  selects an existing `procedure` or `domain` candidate for reuse.
- `{"schema_version":1,"action":"compose","proposal":{...}}` supplies a complete
  proposal using the same shape above.

Every new digest is probed against all required pairs. An identical digest
with complete existing observations is reused without creating a new
composition binding or manufacturing receipts. The corresponding planned
probe stages become `skipped` with reason `candidate_reused`. `keep_baseline`
skips only the composition; the final deterministic comparison still considers
the other verified candidates.

An invalid, fully settled proposal can retry once when `stage_retries: 1` and
additional free budget permit it. Required probe reservations remain protected.
A bound candidate is immutable. Resume reconciles original role completion
markers before binding a candidate and never repeats an already completed
proposal to improve its score.

Optional domain, composition and reflection groups require sufficient budget
for their complete remaining work. Otherwise they become `skipped_budget`
without partial admission. Verified candidates remain available for selection.
Reflection receives the deterministic candidate preference and can return
`no_entry`; any lesson must reference the selected candidate and committed
baseline, candidate and verification evidence. The final report still checks
the complete pipeline budget after reflection. Nothing enters active memory
or the promotion lifecycle.
