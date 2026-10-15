# Replayable Bayesian belief state

> Status: reference; initial finite-model implementation
> Authority: reference (runtime code and tests are authoritative)

Beliefs are a rebuildable component alongside execution state. They describe
probabilities under a locked model and its admitted evidence. They do not
establish task success, grant tools, change verification or acceptance, or
authorize a side effect.

## Team configuration

The owning team supplies a model JSON file. Paths are relative to the team
directory; traversal, escaping symlinks, non-regular files and oversized models
are rejected before model execution.

```yaml
belief:
  model-file: models/diagnostic.json
  experiment: comparison
  # Optional; omitting this keeps belief calculation without policy advice.
  policy-file: policies/diagnostic.json
```

The selected experiment must declare `source: verification` and exactly the
`pass` and `fail` outcomes. Other model experiments are not automatically fed
the same verification result. Omitting `belief` disables the feature and
preserves existing execution-policy hashes and task behavior. Unsupported
settings, including `mode: active`, are rejected.

Model schema: [`internal/belief/model.schema.json`](../../internal/belief/model.schema.json).
The model declares a version, mutually exclusive and exhaustive hypotheses,
prior source, applicability, likelihoods and an independence basis. Probability
units are integers on the fixed scale 1,000,000,000; priors and each hypothesis's
complete outcome probabilities must sum to that scale.

[`synthetic-model.json`](../../internal/belief/testdata/synthetic-model.json)
contains deliberately synthetic engine-test values. For its `pass` outcome,
prior `(0.4, 0.6)` becomes Q9 `(142857143, 857142857)`. It is not a calibrated
program/environment diagnosis. Production hypotheses and numbers belong to the
team's versioned model and must identify their assumptions and source.

## Observation and revision boundaries

Runtime admission reads a durable canonical task terminal event. A verifier
result inside an execution receipt is preferred. When a success path instead
records the verifier result on the terminal event, it is bound only to that
event's explicit current receipt, with matching run, task and occurrence
attempt. The same rule can supply the current receipt's frozen run-input hash
from the event. Historical receipts never inherit another execution's result
or input revision. Exact aliases of the current receipt are deduplicated.

`pass` requires verifier exit code zero, without timeout or overturn. Other
recorded verifier outcomes are `fail`. Worker prose, a success claim, judge
opinion or tool-output text does not become an observation. No verifier result
means no observation. A verifier with missing receipt or revision identity is
unavailable and produces a content-free bookkeeping diagnostic.

This first adapter uses one completed receipt per epoch. Its subject revision
is the admitted run-input snapshot hash, and its environment revision hashes
the recorded verifier contract, working directory and execution target. It is
not a fingerprint of the entire source tree or live operating system. Tasks
without a durable input revision cannot silently receive one from today's
configuration. Teams using the adapter need frozen typed invocation inputs;
for synthetic exercises these can use an explicit fixed fixture revision, for
example:

```yaml
inputs:
  revision:
    type: string
    default: synthetic_fixture_v1
```

This fixture value is a declared synthetic scope, not evidence that source
files or the environment are unchanged. Later receipts open new epochs and do
not inherit earlier posteriors. The generic journal API can admit multiple
observations in an epoch, but different invocations are not independent by
default: multiple observations in the same correlation group, or sharing a
source, yield `dependence_unresolved` with a null distribution.

## Persistence and recovery

Models and optional bound policies live in the existing immutable artifact store; model bindings, epochs,
admissions, retractions and closure are events in `runtime.sqlite`. Runtime
model and adapter identity are pinned in the existing execution-policy
snapshot. Changing a model or selected experiment fails resume with policy
drift; use a new session for a changed execution policy.

## Advisory policies

An optional `belief.policy-file` enables deterministic suggestions after runtime
observations. It is strict JSON, confined to the team directory, with a 64 KiB
limit. The supported schema is
[`policy.schema.json`](../../internal/belief/policy.schema.json), with this
synthetic example:

```json
{
  "schema_version": 1,
  "version": "synthetic-v1",
  "algorithm": "max_posterior_v1",
  "mode": "advisory",
  "choices": [
    {"hypothesis_id": "h1", "candidate_id": "check-a"},
    {"hypothesis_id": "h2", "candidate_id": "check-b"}
  ]
}
```

Every model hypothesis needs exactly one mapping. Each candidate must already
exist in the team's validated `action-catalog`, declare `side-effect: none`,
and have at least one actor in both its discover and propose lists. A policy
file cannot supply grants. For example, a candidate can use the existing
catalog contract:

```yaml
action-catalog:
  check-a:
    description: Inspect one bounded diagnostic result.
    capability: diagnostics  # An existing action-providers capability.
    type: check_a
    agent: worker
    side-effect: none
    input-schema:
      type: object
      additional-properties: false
    access:
      discover: [worker]
      propose: [worker]
```

The example policy also requires `check-b` to be declared. Runtime uses the
source receipt event's actor to check the frozen proposal eligibility. A
missing grant produces `candidate_not_authorized`; unknown distributions
produce `distribution_unavailable`. Neither produces a selected candidate.

`max_posterior_v1` selects the highest published Q9 mass, breaking a tie by
hypothesis ID. This demonstrates consuming the distribution; it does not
estimate information gain, cost or diagnostic accuracy. It never invokes the
candidate, proposes an action, schedules a task, calls a model, or consumes or
resets execution budgets. Active mode is rejected.

The execution-policy snapshot pins the bound policy digest, which includes
the model, policy and frozen catalog eligibility. Changing it fails resume.
Each `belief_policy_suggested` event saves the snapshot and state digests,
policy version/digest, candidate and hypothesis or an unavailable reason,
source cursor/hash/actor, synthetic flag and epoch-local `decision_generation`.
This sequence is separate from the active decision engine's generation.

Retries of one snapshot return the same durable generation. Effective belief
or source-actor changes advance it; closure and duplicate admission do not.
The writer validates the expected snapshot and generation, then fences the
global journal head inside the SQLite write transaction. A late computation
must reread and recompute. A maximum of 1,024 generations per epoch and 4,096
records per inspected run bounds replay; exceeding a limit reports unavailable
bookkeeping and does not alter completed execution.

Replay verifies each recommendation at its historical input cursor. Missing
model or policy artifacts preserve the recorded choice, with validation marked
unavailable; missing policy does not erase a reconstructable distribution.
Availability failures recorded by the runtime are historical facts: restoring
an artifact does not rewrite an earlier unavailable generation. Current team
files never substitute for a locked artifact.

## Reconstruction

The Coordinator maintains a defensive in-memory projection. Discarding that
cache does not change its digest: replay verifies the journal and selected
branch lineage, loads the historical model artifact, selects effective
observations, then recomputes from the original prior. Retractions are
append-only tombstones; late admissions cannot revive a withdrawn observation.
No rounded posterior is used as a new prior.

Startup repairs source/admission gaps using the historical adapter binding.
Repairs do not call a provider, rerun a verifier, repeat an action, reset a
budget, or change an already verified task's status. A child branch can repair
a visible source gap in its own new epoch; it does not write an ancestor's
epoch. Sources beyond a fork cursor are invisible to that child.

Missing or invalid model artifacts and unsupported model/reducer versions are
unavailable. Replay does not substitute the current team file. Historical
bookkeeping diagnostics remain visible even if later admission succeeds; they
are not execution failures or a claim that the latest repair failed.

## Read-only inspection

```bash
hufu inspect beliefs run-123 --workspace ./workspace
hufu inspect beliefs run-123 --workspace ./workspace --format json
hufu inspect beliefs run-123 --workspace ./workspace --task task-7 --branch main
```

`--session` restricts the exact session. The inspector first validates the full
event chain, then selects the requested branch lineage and run; sibling
branches are not searched implicitly. Task filtering does not expand scope.

Text and JSON expose execution status separately from belief state, with model
version/digest/synthetic flag, epoch scope, probability units, status, state
digest, effective experiment/outcome observations, source event/hash/receipt
references and retractions. They omit raw
worker output, verifier command/output and working directories. Unknown mass
is null/unavailable, never zero or a uniform distribution. Missing model
artifacts retain available source references. Policy bindings and historical
suggestions include their provenance and validation status in both formats.

Inspection does not repair bookkeeping, create a missing workspace, migrate
storage, load team files, or execute historical operations. The useful result
is a reproducible distribution and an evidence trail that explains its inputs;
it is not a measured improvement in diagnostic accuracy.

Implementation stages and test evidence are recorded in
[`belief-state-validation.md`](belief-state-validation.md).
