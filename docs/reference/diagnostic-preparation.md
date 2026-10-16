# Diagnostic preparation, offline shadow and replay

> Status: active
> Authority: reference
> Verified-Commit: `51c0d8bd31316056150a292ece512aff1339fb17` (implementation base; changes validated in the working tree)
> Supersedes: —
> Superseded-By: —

Stages 0–1 of the [bounded diagnostic proposal](../architecture/bounded-diagnostic-belief-loop.md)
provide pure calculation, strict typed contracts and an offline synthetic pilot.
Stage 2 adds journaled **offline fixture** episodes, replay and read-only
inspection. Runtime diagnostic dispatch remains unavailable. Existing
receipt-per-epoch belief and advisory policy behavior is unchanged.

## Prepare a team's contracts

An omitted `diagnostic-loop` block preserves the existing execution-policy
JSON and configuration hash. A present block requires explicit preparation:

```yaml
diagnostic-loop:
  mode: off
  actor: worker
  model-file: models/diagnostic.json
  policy-file: policies/diagnostic.json
  adapter-file: adapters/diagnostic.json
```

The actor must be a loaded agent, with discover and propose grants for every
candidate in the frozen action catalog. Candidates must have `side-effect: none`.
These checks establish preparation eligibility only; they grant no invocation,
dispatch, tool, phase, unattended or artifact access. `shadow`,
`bounded_read_only`, unknown modes and a missing mode are rejected before any
model or action call.

Resources must be bounded regular files under the team directory; symlinks,
absolute paths and traversal are rejected. The existing strict belief model
schema is reused. New schemas are:

- [Ranking policy](../../internal/belief/diagnostic-policy.schema.json): exact
  expected Gini gain per declared positive cost, or declared fixed order.
- [Measurement adapter](../../internal/belief/diagnostic-adapter.schema.json):
  versioned verification experiments with exactly `pass` and `fail`.
- [Evidence envelope](../../internal/belief/diagnostic-evidence.schema.json):
  expected source mapping, subject/source/environment revisions and measurement
  digest, at most 16 KiB.

The prepared model, policy, adapter, actor and catalog are fingerprinted in the
existing execution policy snapshot. A changed binding rejects resume through
the existing policy-drift gate. Current files cannot rewrite an admitted
snapshot. This prepared runtime binding does not open episodes. Offline fixture
episodes have their own frozen artifact binding; a digest alone never proves a
runtime journal source authentic.

`RankDiagnostic` uses the projector's unrounded weights from the original prior
and deduplicated observations. Rational scores are compared exactly, with
catalog ID as the tie breaker. The fixed-order policy preserves candidate
order. A threshold requires a new observation and compares the exact posterior,
so rounding the Q9 display cannot cause an early stop. Threshold takes precedence
over the round limit. Resource limits are declarative preparation contracts, not a
reservation ledger. Future execution must enforce the stricter runtime limits.

`CheckDiagnosticEvidence` checks an expected typed mapping and revisions; it
does not read a journal, verify receipt provenance, load artifacts, or authorize
execution. Unknown, timeout, cancelled and invalid outcomes are rejected rather
than converted to `fail`. The existing projector still handles duplicate IDs,
identity conflicts, shared sources/correlation groups and retraction by
recomputing from the original prior.

## Run the offline pilot

Team-owned resources and the pure provider live under
[diagnostic-pilot](../../.agent-teams/hufu-dev/diagnostic-pilot/README.md).
Run from the repository root:

```bash
go run ./.agent-teams/hufu-dev/diagnostic-pilot --evaluate
go test ./.agent-teams/hufu-dev/diagnostic-pilot
```

The evaluation enumerates independent seeds in `[0,99]`, weighted 5:3:2 across
the three synthetic latent states, for 100,000 paired cases. It computes eight
deterministic full traces (four bundles × two strategies), then expands corpus
rows by reference. It does not pretend to perform 400,000 distinct provider
executions. `--evaluate --traces` writes the 100,000 reference rows as JSONL to
stdout; `--evaluate` returns their corpus hash, the eight full traces, provenance
and contract hashes, and aggregate counts. Shell redirection is optional; the
provider itself writes no files.

For this model, both strategies expand to 200,000 actions and 200,000 cost units.
Both stop at `threshold_reached` for 44,000 cases and `round_limit` for 56,000.
The source-fail/environment-pass Q9 posterior is:

```text
environment_fault:   6,818,182
other_fault:        72,727,273
source_fault:      920,454,545
```

All values and latent labels are synthetic engine-test data. No result proves
real diagnosis accuracy, calibration or savings. The harness oracle never
enters the provider request or bundle. Out-of-universe registrations are refused;
two failing probes remain a valid noisy measurement pair, not proof of two
latent faults.

## Persist an offline shadow episode

Use a dedicated, empty workspace; the fixture harness refuses a workspace with
non-fixture runtime events. It accepts only the four embedded fixture names.

```bash
go run ./.agent-teams/hufu-dev/diagnostic-pilot \
  --shadow-workspace /tmp/hufu-diagnostic-shadow --fixture fail-pass
go run ./cmd/hufu inspect --workspace /tmp/hufu-diagnostic-shadow \
  beliefs run-diagnostic-fixture
go run ./cmd/hufu inspect --workspace /tmp/hufu-diagnostic-shadow \
  --output json beliefs run-diagnostic-fixture --task fixture-owner
```

This path computes the embedded comparisons directly. It does not call an
ActionProvider, model, subprocess verifier or network service. The normal
team's `diagnostic-loop.mode` still accepts only `off`; the fixture command does
not enable runtime `shadow` or `bounded_read_only`.

The [diagnostic journal](../../internal/team/diagnostic_journal.go) writes three
schema-v1 event types in the existing hash-chained `runtime.sqlite` journal:

- `diagnostic_episode_opened` pins a content-addressed `diagnostic_fixture`
  artifact containing the synthetic model, policy, epoch, all three revisions
  and typed fixture measurements. The artifact is durable before the opening
  event; an interrupted opening may leave an unreferenced artifact.
- `diagnostic_episode_shadowed` records a deterministic decision ID, generation,
  previous episode cursor/hash, exact scores, selected candidate or stop reason,
  and input belief digest. It conveys no execution authority.
- `diagnostic_episode_observed` consumes exactly the pending choice's frozen
  fixture measurement. These are synthetic observations, never runtime receipts,
  child tasks, dispatch intents or cross-task evidence admissions.

The offline state sequence is `ready → awaiting_fixture_observation → ready`,
ending with a shadow decision whose stop reason sets `stopped`. Each append
preflights the same reducer used by replay and uses global-head transactional
CAS. Stale cursors and competing appends are refused; recovery reads the durable
state before continuing. A lost acknowledgment cannot duplicate an observation.
The harness holds the existing branch writer lease. Forks may inspect the
inherited prefix but cannot advance an ancestor's episode, and later parent
events do not enter that prefix.

Policy attempt/cost caps bound the simulated fixture trace only; they do not
reserve or spend runtime quota. `fixture_budget_exhausted`,
`fixture_observation_limit` and `event_limit` stop before exceeding the frozen
offline caps. Action and episode timeouts remain declarative: this path has no
actions and does not implement a production deadline ledger.

[Replay](../../internal/team/diagnostic_events.go) recomputes each decision from
the frozen inputs and rejects mismatched scores, outcomes, cursors or scopes.
The fixture command's JSON and inspect's `data.episodes[]` share `replay_digest`.
The digest covers the complete verified projection, including recorded event
identities; independently created journals need not have the same digest.
Repeating the command after a terminal episode appends nothing. Changed embedded
inputs cannot rewrite an existing episode; use a new dedicated workspace.

Inspect is read-only: it does not create, migrate, repair or resume a workspace,
read current team files, or execute a provider/verifier. Missing or corrupt
artifacts produce `validation: unavailable`, `state: null` and no replay digest,
while preserving historical choices, observations and artifact references.
Historical choices then remain explicitly unverified. Unknown episode schemas
fail closed. There is no new database table or migration; use a binary supporting
these schema-v1 diagnostic events to inspect these offline workspaces.

## Deferred runtime work

Live evidence admission, runtime shadow, authorization, reservations, durable
dispatch intent/child-task mapping, executor and external-action crash recovery
remain stages 3–4. This delivery validates every **offline journal commit**
boundary, not the later dispatch crash matrix. In particular, it establishes no
cross-task receipt provenance or exactly-once external execution guarantee.
