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

Models live in the existing immutable artifact store; model bindings, epochs,
admissions, retractions and closure are events in `runtime.sqlite`. Runtime
model and adapter identity are pinned in the existing execution-policy
snapshot. Changing a model or selected experiment fails resume with policy
drift; use a new session for a changed execution policy.

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
artifacts retain available source references.

Inspection does not repair bookkeeping, create a missing workspace, migrate
storage, load team files, or execute historical operations. The useful result
is a reproducible distribution and an evidence trail that explains its inputs;
it is not a measured improvement in diagnostic accuracy.

Implementation stages and test evidence are recorded in
[`belief-state-validation.md`](belief-state-validation.md).
