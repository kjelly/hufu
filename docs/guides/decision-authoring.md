# Decision-aware runtime: authoring guide

> Status: active
> Authority: guide
> Verified-Commit: `1e9f1e4`
> Supersedes: —
> Superseded-By: —

This is the operator-facing companion to
[the decision-aware runtime specification](../architecture/decision-runtime.md).
The spec says what the runtime guarantees; this guide says what you write to
get those guarantees, and what happens when you write nothing.

Everything here is configuration. None of it can be set by a coordinator's
task payload: the fields below are `json:"-"` or frontmatter-only precisely so
that the model under scrutiny cannot lower the rigor it is being judged under.

---

## 1. Doing nothing

A team with no `decision:` block behaves exactly as it did before this feature
existed. Every task resolves to the reserved `off` profile, no judge is
dispatched, no decision event is written, and both runtime tool hooks are
no-ops.

This is a tested guarantee, not an intention:
`TestDecisionV1OffProfileIsInert`, `TestParseTeamYMLWithoutDecisionBlock`,
`TestDisciplineHooksAreNoOpsWhenUnarmed`.

You opt in per task, or set a team default. There is no migration step for an
existing team.

---

## 2. Declaring profiles

Profiles live under `decision.profiles` in `team.yaml`. A profile is a rigor
level, not a topic: name them for how much scrutiny they buy.

Every `team.yaml` snippet in this guide is shown at its field's own level: a
legacy flat team puts `decision:` at the top of team.yaml, exactly as shown
below; a `hufu.io/v1alpha1` team puts the same block under `spec:` instead,
since v1alpha1 keeps identical field names inside an envelope — see
[team schema versioning](../architecture/team-schema-versioning.md).

```yaml
decision:
  default-profile: off          # the reserved name; tasks opt in individually
  request-contract:
    enabled: true
    objective: keep the bridge reachable while changing it
    success-criteria:
      - id: reachable
        statement: the bridge answers after the change
    assumptions:
      - id: service-accepts
        statement: the target service accepts the change
        critical: true
  profiles:
    standard:
      independent-judgments: 3
      context-isolation: strict
      score-scale: 0-10
      criteria:
        - id: impact
          statement: how much this moves the objective
          weight: 1
          direction: higher-is-better   # or lower-is-better
      aggregation:
        method: mean-score
      challenge:
        enabled: true
        count: 1
        trigger:
          dispersion-above: 0.5         # challenge only when judges disagree
      revision:
        enabled: true
      finalization:
        mode: aggregate                 # or coordinator, or judge + judge-id
      discipline:
        alternatives:
          require-no-action-option: true
          min-options: 2
        stop:
          checkpoint-every: 2
          require-kill-criteria: true
          kill-criteria:
            - id: tool-calls
              kind: tool_calls
              threshold: 20
            - id: assumption
              kind: assumption_invalid
        commit:
          require-verification: true
          require-reconcile: true
        replan:
          on-critical-assumption-contradicted: replan
```

A complete three-profile example (light, standard, high-stakes) is kept as an
executable fixture in `internal/team/decision_v1_fixture_test.go`. It is
loaded through the real team loader in tests, so it cannot drift away from
what the parser accepts.

**Unknown enum values are rejected at load time**, with the reason code in the
message. A profile name a task references but the team does not define fails
the run before any work starts.

---

## 3. Declaring a decision task

```yaml
tasks:
  - agent: deployer
    goal: change br0 without dropping the tunnel
    decision-profile: high-stakes
    decision-options:
      - id: execute
        kind: execute
        title: Change the bridge in place
      - id: reduce
        kind: reduce_scope
        title: Change one port first
      - id: defer
        kind: defer                     # the no-go alternative
        title: Do nothing for now
      - id: gather
        kind: request_information
        title: Measure the tunnel first
    decision-assumptions:
      - id: service-accepts
        statement: the target service accepts the change
        critical: true
        evidence-refs:
          - id: evidence-1
    decision-base-rates:
      - reference-class: bridge changes
        metric: success
        sample-size: 12
        source:
          id: base-rate-1
```

Option kinds: `execute`, `reduce_scope`, `negotiate`, `request_information`,
`defer`, `abandon`, `custom`. `defer` and `abandon` are the no-go kinds — a
profile with `require-no-action-option: true` blocks before judgment unless
one is present.

If a task declares no options, the profile must enable `option-proposal`, or
the task is a configuration error. The runtime still injects the required
alternatives afterwards, so enabling proposal does not weaken the no-go gate.

---

## 4. Evidence artifacts

Artifact evidence is resolved and integrity-checked before any judge runs. A
reference to an artifact nobody published is rejected, and a declared digest
that disagrees with the stored one is rejected.

That means evidence must exist in the workspace artifact store
(`<workspace>/logs/artifacts`) before the decision task is dispatched — a
producing task publishes it, and the consuming decision task cites its `id`.
Do not hand-write a `sha256`: cite the id and let the store supply the digest.

Base-rate sources follow the same rule. An outside-view requirement is
satisfied by artifact-backed evidence, not by a prose claim.

---

## 5. Assumptions

An assumption has exactly three status sources (spec §18.1):

1. a submitted task result that reports a check,
2. a declared verification, and
3. an explicit operator command.

Nothing else may move an assumption's status, and the history is append-only:
contradicting an assumption never edits the decision that rested on it. It
supersedes it, marks it stale, and — under
`on-critical-assumption-contradicted: replan` — drives a new lineage.

```bash
hufu decision assume <decision-id> <assumption-id> \
  --status contradicted --note "the service rejected the change"
```

Only `supported` and `contradicted` are reportable. `unknown` is the absence
of a check, and `stale` is the runtime's own conclusion that an earlier check
no longer applies — neither is something an operator asserts.

---

## 6. Verification and reconciliation

The commit gate decides whether a side-effecting task may start at all. Each
prerequisite is decidable from what you authored:

| prerequisite | satisfied by |
|---|---|
| `require-verification` | `verify:` command or `verify_spec:` |
| `require-evidence` | an assertion-bearing `verify_spec:` |
| `require-reconcile` | `recovery: reconcile` **and** `reconcile_tool:` |
| `require-observability` | `expected_state_change:` **and** `reconcile_tool:` |
| `require-rollback` | a `compensate-tool` for the invoked tool (below) |

The gate is evaluated at the true mutation boundary. A read-only observation
is never gated, and clearing the gate for one tool authorizes only that tool.

### Per-tool recovery

`require-rollback` is a property of the tool that mutates, not of the task, so
it is declared per tool in agent frontmatter:

```yaml
---
name: deployer
tools: bash,view,delete-bridge
side_effect: infra_mutation
recovery: reconcile
reconcile-tool: probe-bridge
tool-recovery:
  bash:
    compensate-tool: delete-bridge
    reconcile-tool: probe-bridge
    retry-safe: false
    idempotency-key: request_id
---
```

A task may override any field of an entry with `tool-recovery:` of its own.
Two rules are enforced before execution rather than after a mutation:

- a `compensate-tool` naming something the agent cannot invoke is rejected at
  arm time — a rollback path the worker cannot take is not a rollback path;
- a tool cannot compensate itself.

Static runtime actions are gated under `capability:type`, and a declaration
for the bare capability covers every operation it exposes. Structured
execution steps are gated when they declare `effect: mutate`.

### When a mutation's outcome is unknown

If an interrupted mutation cannot be classified, the runtime stops rather than
continuing over state nobody can account for. The checkpoint stops with
`reconcile_unknown_state`, and the repair controller refuses to retry: it
reconciles first, or blocks for a human. This applies to every non-replayable
side-effect class, `unknown` included.

---

## 7. Reading decisions back

```bash
hufu decision list                     # decisions with no recorded outcome (--all for every one)
hufu decision show <decision-id>       # the full record, including sealed evidence
hufu decision stats                    # activity projected from the event log and index
hufu decision resolve <decision-id> --outcome succeeded \
  --evidence <sha256> --lesson "..." --resolved-by "..."
hufu decision assume <decision-id> <assumption-id> --status contradicted --note "..."
```

`--outcome` is one of `succeeded`, `failed`, `mixed`, `superseded`,
`unresolved`. A decision can be resolved once, and resolving it never edits
the record: what was decided, and on what evidence, stays as it was formed.
`--resolved-by` is provenance, never justification.

`decision resolve` is **recording only**. The runtime does not feed outcomes
back into judge weighting or calibration, and will not until the volume and
audit gates in the plan's Stage 9 are met. Outcome quality is not decision
quality (spec §48.7).

---

## 8. Migration notes

- **Adding a `decision:` block changes nothing on its own.** Tasks keep their
  behavior until one names a profile or you set a non-`off` default.
- **Records are versioned.** A record written by an older session stays
  readable; unknown enum values fail validation rather than being ignored.
- **A durable task carries its decision admission.** Resuming a run never
  re-resolves mutable profile configuration for a task that was already
  admitted — the admission that authorized it is the one that governs it.
- **`--decision-profile` overrides for one run.** An undefined name fails
  before the run starts.
