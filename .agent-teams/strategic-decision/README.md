# strategic-decision — implementation notes vs. spec.md

This team implements the directory layout `spec.md` §6 asked for
(`team.yaml`, `coordinator.md`, `reference.md`, `juror.md`, `challenger.md`),
adapted to what Hufu's real decision-aware runtime (`internal/team/decision_*.go`,
`internal/agent/decision_config.go`) actually parses and executes today.

**2026-09-07 update:** `spec.md` was rewritten to v2 (72 sections) and
`spec2.md` was added, both reframing `reference`/`juror`/`challenger` as
capability-routed logical roles rather than fixed agent identities. Per
spec2.md's own explicit sequencing (PR-2, then PR-3, then PR-4), this team
now genuinely capability-routes `REFERENCE` (PR-2), `JUDGE` (PR-3), and
`CHALLENGE` (PR-4), with `REVISE` correctly reusing `JUDGE`'s binding rather
than re-resolving — see Gap 1 below, `reference-specialist.md` (a second
real REFERENCE candidate), and `judge-role`/`challenge-role` in `team.yaml`
(which also make `juror.md`/`challenger.md` real candidates). This is
spec2.md's **complete MVP scope**: `PREMORTEM`/`FINALIZE` remain
sidecar-only by spec2.md's own design (§9/§10), not an oversight or
remaining follow-up.

**Further same-day update:** after finishing spec2.md's 4 PRs, this team
also picked up two of the (lower-risk) extras spec.md v2 describes beyond
spec2.md's own plan — see the `routing-policy.scoring.weights` and
`decision.routing-hints` blocks in `team.yaml`.

**Same-day update again (durable AgentBinding, metrics, explainability):**
`DecisionOpinion`/`DecisionChallenge`/`DecisionRevision`/
`ReferenceEvidenceDraft` now carry the resolved `AgentID` (the runner sets
it after invoking the candidate; empty on the legacy sidecar path), so a
routing decision survives past the run instead of only reaching an
ephemeral TUI status line. `DecisionMetrics` gained
`CapabilityRoutedBindingCount`, and `hufu decision explain <decision-id>`
now shows, per stage, whether capability routing resolved a concrete
worker and which one. Capability freshness/invalidation turned out to
already be implemented (`CapabilityRegistry.recordsFor` already honors
`declared-at`/`stale-after` and `scoreAgent` already skips stale records)
— just never demonstrated in this team's config, not a code gap.

**Same-day update, final pass (DiversityPolicy, pinned binding, mid-run
capability invalidation, adaptive challenger routing):** all four of the
previously-listed "still unbuilt" items are now shipped. `judge-role`/
`challenge-role` gained `min-distinct-models`/`min-distinct-providers`
(hard floors), `prefer-distinct-models`/`prefer-distinct-providers` (soft
preferences that reorder ranking without excluding a qualified candidate),
and `allow-repeated-agent-definition`; every role gained `pin.agent`/
`pin.reason` (gated by a new `discipline.routing.allow-pinned-binding`,
checked at `hufu team validate` time, not at run time); `challenge-role`
gained `adaptive-capabilities` (maps a configured decision criterion to
extra preferred capabilities, applied when JUDGE round 1's opinions
disagreed most on that criterion); and REVISE now reuses the original
opinion's durable `AgentID` directly instead of re-resolving, closing a
latent bug where a capability change between JUDGE and REVISE could
previously move REVISE to a silently different agent. Capability-group
distinctness remains explicitly out of scope — no such taxonomy exists in
this codebase, and inventing one is a product decision, not a wiring gap.
This team demonstrates `prefer-distinct-models` (a documented no-op today,
since every worker here shares one team-wide model) but not pinned binding
or adaptive-capabilities — see `plan.md`'s matching update for why. See
`plan.md`'s Status section for the full file/function/test breakdown.

Validated with:

```
go build ./cmd/hufu
./hufu team validate .agent-teams/strategic-decision   # -> "contracts valid"
./hufu team explain   .agent-teams/strategic-decision   # tools/side-effect wiring confirmed
```

## Which gaps below are actually "not implemented yet"

`/home/ubuntu/nfs/github/agent-team-cli/plan.md` ("Decision-Aware Runtime
Completion Plan") is the authoritative tracker for this runtime, and its
Status section says Stages 0–7 (all of V1) are **complete and committed**.
Only two stages are open, and both are deliberately gated behind a human
decision, not an oversight:

- **Stage 8 — capability-aware routing**, gated on "no trusted capability
  evidence source exists ... and the capability/authorization relationship
  has not been explicitly approved."
- **Stage 9 — outcome learning/calibration**, gated on 30 resolved decisions
  with 20 verified outcomes (currently 0/0).

Cross-checking against the gaps below: **only Gap 3 (capability-aware
routing) was genuinely pending, plan.md-tracked work — and it has since been
implemented, including wiring into the real worker-delegation dispatch path
(see Gap 3 below and `plan.md`'s Status section).** The
"on-capability-invalidated" half of Gap 7 is **now partially resolved**:
REVISE (`internal/team/decision_challenge_capability_runner.go`'s
`revisionCandidate`) now re-checks its original binding's authorization and
required-capability satisfaction before reusing it, and fails closed rather
than silently invoking a different agent when either check no longer
passes — this is the one place in the engine that currently has a
"capability no longer available" signal to act on. There is still no
generic `on-capability-invalidated` replan trigger (spec.md §19's third
bullet) that would fire for JUDGE/CHALLENGE mid-round, since those
invocations happen close enough together that no realistic wall-clock gap
exists between resolving a binding and invoking it — only the JUDGE→REVISE
gap (with CHALLENGE and aggregation in between) is a real window in this
codebase's execution model, and that is exactly what's now handled.
Everything else in this document —
Gap 2 (decision options/facts staying `json:"-"`) above all — is not missing;
it is a **deliberate, completed architectural choice**, stated directly in
plan.md's Invariants: "Reuse the existing scheduler ... Do not create a
second DAG or a parallel task lifecycle" and "Decision-only task
configuration remains `json:"-"` so a coordinator cannot lower rigor, inject
options, or alter evidence policy through its payload." Gaps 4, 5, 6, and 8
are likewise not plan.md gaps — they are spec.md's own proposed
YAML/frontmatter drifting from the schema the finished runtime actually
parses (field names, required keys it never mentioned, strict-vs-lenient
parsing), which I corrected directly in this directory's files rather than
waiting on any pending stage.

### `spec1.md` turned up: it is the missing companion doc (this section describes spec.md's original v1; spec.md is now v2 and asks for real capability routing — see Gap 1 above and the 2026-09-07 update note)

`spec1.md` ("Hufu Strategic Decision Discipline Runtime Specification") is,
almost certainly, the `hufu-strategic-decision-runtime-spec.md` that spec.md's
header names as its companion. It maps the same R1–R10 principles onto
Hufu primitives (`RequestContract`, `PremortemPolicy`, `StopPolicy`,
`AlternativesPolicy`, `CommitGatePolicy`, `ReplanPolicy`, sealed-evidence
isolation, challenger — spec1.md §4, §7–§15), and its §17 target `team.yaml`
is where spec.md's proposed YAML actually came from — including its bugs:
spec1.md §17 itself still writes `aggregation: mean-score` as a bare scalar,
`on-evidence-packet-changed`, `on-capability-invalidated`, and
`discipline.routing.capability-aware` under every profile, none of which
match the schema that got built (`internal/agent/decision_config.go`). So
spec1.md's own example never matched the final implementation either — it
predates it. Reading it doesn't change any of the schema-mismatch fixes
already applied to this directory's files.

More importantly, **spec1.md never asks for reference/juror/challenger to be
separate delegated agent files.** §15 talks about "N independent jurors" and
"challenger" only as internal Decision Engine concepts, and §12 explicitly
places capability/routing concerns in "AgentPool / routing service", outside
the Decision Engine. The whole "isolated agent dispatched through normal
delegation" topology in spec.md §3–§4 (v1) was spec.md's own packaging choice
when it turned this runtime spec into an agent-team layout — spec1.md itself
was consistent with what was originally built (one internal Decision Engine
state machine driving judge/reference/challenge/premortem stages). So **Gap 1
was never a real gap against spec1.md** — only against spec.md v1's own
extrapolation, at the time this section was written. spec1.md's remaining
request was narrower than spec.md v1 implied: capability-aware routing (§12,
§25 "Phase 3 — Capability routing") — exactly plan.md's Stage 8. Having the
document in the repo did not by itself implement `CapabilityResolver` /
`CapabilityRecord` / `RoutingPolicy` — that required someone to explicitly
open the gate (a human decision, not just code existing, per plan.md) and
then write it: done 2026-09-07, see Gap 3 below. spec.md was later rewritten
to v2 and now does ask for exactly the topology this section says spec1.md
never required — see Gap 1 above for what's actually been built against v2.

**Before running this team, you must set `judge-model` in `team.yaml` to a
model actually configured in your environment.** Nothing in the DecisionEngine
works without it (see Gap 5).

## What genuinely works as specified

- Isolation, sealed evidence, deterministic aggregation, challenge,
  premortem, forecast, no-go/information alternative enforcement, evidence
  independence grouping, stop/commit/replan discipline — all real,
  runtime-enforced, config-driven behavior (`internal/agent/decision_config.go`).
- `option-proposal.enabled: true` (added by me, not in spec.md — see Gap 2)
  makes the runtime auto-generate options from the task's Goal and then
  **deterministically inject** the required no-go/defer/reduce-scope/
  request-information alternative regardless of what the proposer returns
  (`internal/team/decision_options.go`) — this is a real, load-bearing
  mechanism for spec.md §17's "No-Go Rules".
- `budget-degradation: forbidden` (the runtime's own default) really does
  fail closed instead of silently running fewer judges than configured
  (spec.md §21).

## Where the runtime cannot satisfy spec.md as written

**Gap 1 — reference/juror/challenger are not dispatched as agents at all —
RESOLVED 2026-09-07 for every role spec2.md defines as capability-routed.**
Spec.md's whole topology (§3–§4; v2 §4/§15/§17/§19) assumes the coordinator
delegates isolated invocations of `reference.md` / `juror.md` /
`challenger.md` through the normal worker/task delegation system, resolved
by capability. Originally the `DecisionEngine` never did this at all: every
stage was a direct call from `internal/team/decision_runners.go` to one
shared, tool-less **judge-model sidecar**. That is now only true for
`PREMORTEM`/`FINALIZE` — and per spec2.md's own design (§9/§10), it stays
true for those two permanently; they were never meant to be capability-routed
roles in the first place.

**`REFERENCE` is genuinely capability-routed** (spec2.md PR-2): when a
profile sets `outside-view.role.required-capabilities` (see this team's
`standard`/`high-stakes` profiles), `internal/team/decision_reference_capability_runner.go`
resolves the highest-scoring already-authorized candidate from
`delegation.allowed-workers` — using the same `CapabilityRegistry` this team
already declared via `capability-registry` — and invokes it directly via the
normal agent-runtime primitive (`Coordinator.createGatedAgent` +
`runAgentWithStatusAndHistory`, the same one `coordinator_plan.go`'s
plan-reviewer uses for a bounded, non-TODO call), with its tools narrowed to
read-only. Proven in
`TestReferenceRoleCapabilityRouting_InvokesResolvedAgentNotLegacyJudge`.

**`JUDGE` (juror) is also genuinely capability-routed** (spec2.md PR-3):
`internal/team/decision_judge_capability_runner.go` resolves, independently
per judge, the identical deterministic ranked candidate list and picks the
judge's own rank (round-robin: judge-1 gets the top-ranked qualified
candidate, judge-2 the second, ...) — this team's `standard`/`high-stakes`
profiles set `judge-role.required-capabilities: [decision-analysis]` with
`min-distinct-agents: 3`, so all three of `reference`/`reference-specialist`/
`juror` genuinely execute as distinct judges (`high-stakes`'s 5 judges wrap
back over the same 3-candidate pool once it's exhausted). Unlike
`REFERENCE`, a judge-role invocation gets **zero** tools regardless of what
the resolved agent declares — parity with the sidecar's own guarantee, since
a judge must reason only from the sealed evidence packet or the "same
evidence" comparability between judges breaks. Proven in
`TestJudgeRoleCapabilityRouting_RoutesEachJudgeToADistinctAgent` — this is
spec2.md's own acceptance bar for this stage: each judge's provider call is
attributed to its own resolved candidate's model, and the legacy judge-model
sidecar records zero calls.

**`CHALLENGE` is also genuinely capability-routed** (spec2.md PR-4):
`internal/team/decision_challenge_capability_runner.go` follows the exact
same shape as `JUDGE`'s runner — this team's `standard`/`high-stakes`
profiles set `challenge-role.required-capabilities: [adversarial-analysis]`
(`min-distinct-agents: 2` for `high-stakes`'s 2 challengers), ranked
`challenger` (0.7) > `reference-specialist` (0.6) > `juror` (0.5). Proven in
`TestChallengeRoleCapabilityRouting_RoutesEachChallengerToADistinctAgent`.

**`REVISE` reuses `JUDGE`'s binding rather than re-resolving** (spec2.md
§8): there is no separate "revision-role" — `RevisionRequest.RoutingRole`
is `agent.JudgeRolePolicy` (the *same* config `JUDGE` used), and resolution
runs through the *same* function `JUDGE`'s runner calls, keyed by the same
`JudgeID`. Because that resolution is a pure function of `(role, ordinal)`
and neither changes between rounds, a revision for `judge-2` always
re-derives the exact candidate `judge-2` got in round 1 — no binding
persistence needed. Proven concretely (not just inferred from shared code)
in `TestRevisionCapabilityRouting_ReusesOriginalJudgeBinding`: it dispatches
a routed `judge-2`, then a routed revision for the same `judge-2`, and
asserts both land on the identical resolved candidate's provider.

Every team/profile that does not set `outside-view.role`/`judge-role`/
`challenge-role` is completely unaffected —
`TestReferenceEvidence_WithoutRoutingRoleStaysOnLegacySidecar`,
`TestJudgeRoleCapabilityRouting_WithoutRoutingRoleStaysOnLegacySidecar`,
`TestChallengeRoleCapabilityRouting_WithoutRoutingRoleStaysOnLegacySidecar`,
and `TestRevisionCapabilityRouting_WithoutRoutingRoleStaysOnLegacySidecar`
prove every legacy path stays byte-for-byte unchanged.

**Permanently sidecar-only, by spec2.md's own design, not a gap**:
`PREMORTEM` and `FINALIZE` (§9 "AGGREGATE 完全不要 routing", §10 "FINALIZE 第
一版也不用 capability routing" — spec2.md's final role list names exactly
three routed roles plus REVISE-reuses-JUDGE, nothing else). `reference.md`/
`reference-specialist.md`/`juror.md`/`challenger.md` are all genuinely
reachable through this path (several of them for more than one role, since
nothing stops one worker from qualifying for multiple). A coordinator can
still delegate a plain task to any of these four names as an ordinary
worker outside the decision pipeline, same as before.

**Gap 2 — decision inputs are configuration-only, not request-time-authored.**
`TaskDef.DecisionOptions` / `DecisionFacts` / `DecisionArtifacts` /
`DecisionBaseRates` / `DecisionProvenance` are all `json:"-"` in
`internal/team/coordinator.go` — deliberately unreachable from any tool call
an LLM coordinator makes, so the model under scrutiny can never author its
own evidence or alternative set. Likewise `decision.request-contract.objective`
/ `success-criteria` are fixed once in `team.yaml`, not something the
coordinator writes per request. Spec.md §4.1 describes the coordinator
dynamically producing "Question → Objective → Success Criteria → Options"
per request; in reality only the task's free-text `Goal` is request-time
content. I mitigated the options half with `option-proposal.enabled: true`
(see above); the objective/success-criteria half has no mitigation — I wrote
a deliberately generic request contract in `team.yaml` that has to cover
every question this team is ever asked.

**Gap 3 — capability-aware routing — RESOLVED 2026-09-07.**
This was the one gap that was genuine, plan.md-tracked, deliberately-deferred
work (Stage 8), not just spec.md drift, and it is now implemented and wired
into the real dispatch path: see `internal/agent/decision_config.go`
(`RoutingPolicy`, `DisciplinePolicy.Routing`), `internal/agent/agent.go`
(`DeclaredCapability`, `TeamConfig.CapabilityRegistry`,
`CapabilityRoutingRule`), and `internal/team/capability_registry.go`
(`CapabilityRegistry`, `CapabilityResolver`,
`Coordinator.ResolveCapabilityCandidates`,
`Coordinator.validateCapabilityRouting`), tested in
`internal/team/capability_registry_test.go`. `discipline.routing.capability-aware:
true` now parses; separately, `delegation.capability-routing` rules (a
goal-substring selector, same shape as `delegation.task-goal-invariants`,
plus a required capability) are enforced by
`validateCapabilityRouting` inside `validateDelegationPolicy` — a task
delegated to a worker that can't show the required capability, while another
already-authorized worker can, is rejected before any TODO is created.

It is deliberately **not** turned on in this team's profiles/delegation
below, and turning on `discipline.routing.capability-aware` specifically
would still not do anything here: that flag concerns the DecisionEngine's
own judge/reference/challenge stages, which (Gap 1, above) stay one
tool-less judge-model sidecar call each, not a routed candidate pool — this
team's reference/juror/challenger are each one fixed role, never several
candidates to choose from. `delegation.capability-routing` is the part of
Stage 8 that's actually usable by a team, but it applies to *ordinary*
worker delegation (e.g. "should this go to `implementation-engineer` or a
future specialist?"), which this single-role decision team doesn't have
either. See `plan.md`'s Status section for the exact scope of what Stage 8
does and does not cover (in particular: the `verified` trust tier stays
unreachable until Stage 9 supplies real outcome data, and the DecisionEngine
itself still isn't routed).

**Gap 4 — agent-frontmatter YAML is not strict.** `team.yaml` is parsed with
`yaml.NewDecoder(...).KnownFields(true)` (`internal/team/parse.go:832-834`),
so an unsupported key there is a hard load error — good, matches spec.md's
own stated principle. But `coordinator.md` / `reference.md` / `juror.md` /
`challenger.md` frontmatter is parsed with plain `yaml.Unmarshal`
(`internal/team/parse.go:586`), which silently drops unrecognized keys.
Two consequences directly hit spec.md's own example files:
- spec.md wrote `side-effect: none` (hyphen); the real field is `side_effect`
  (underscore, `internal/team/parse.go:65`). The hyphenated spelling
  silently no-ops — the agent's side effect stays unset instead of `none`. I
  fixed this in all three worker files (confirmed via `team explain`, which
  now reports `side effect: none` for each).
- spec.md's `delegation: disabled` is not a recognized frontmatter field at
  all (no such field exists on `agentFrontmatter`). It is inert in all three
  files; I kept it only as a human-readable note. The actual reason these
  workers can't delegate is structural: their `tools:` list omits `agent`.

**Gap 5 — `judge-model` is mandatory and absent from spec.md.** Every stage
runs through `Coordinator.AgentPool().JudgeSidecar()`
(`internal/team/coordinator_skills.go:719-739`), which returns `nil` when
`judge-model` is unset. Without it, `formTaskDecision` fails closed with
"needs a judge model and none is configured"
(`internal/team/decision_dispatch.go:178`). Spec.md's proposed `team.yaml`
never sets this. I added a placeholder (`judge-model:
CHANGE_ME_TO_A_CONFIGURED_MODEL_ID`) that you must replace.

**Gap 6 — `decision.request-contract.enabled: true` is mandatory and absent
from spec.md.** `prepareTaskDecision` refuses to form any decision unless
this is set (`internal/team/decision_dispatch.go:106-108`). I added the
generic contract described in Gap 2.

**Gap 7 — replan trigger names don't match.** Spec.md's `replan:` block uses
`on-evidence-packet-changed` and `on-capability-invalidated`. The real
`ReplanPolicy` (`internal/agent/decision_config.go:302-306`) only has
`on-critical-assumption-contradicted`, `on-material-evidence-changed`, and
`on-repeated-failure`. I renamed the first and dropped the second — there is
currently no config-level way to react to "capability no longer available"
(spec.md §19's third bullet).

**Gap 8 — `aggregation: mean-score` must be a nested object.** Spec.md wrote
`aggregation: mean-score` as a bare scalar; `AggregationPolicy` is a struct
(`{method: mean-score}`, `internal/agent/decision_config.go:166-168`). Same
for `outside-view.worker: reference` and `challenge.worker: challenger` —
neither `OutsideViewPolicy` nor `ChallengePolicy` has a `worker` field (there
is nothing to bind to a worker; see Gap 1). The real flag that turns on the
runtime's outside-view stage is `outside-view.reference-evidence: true`.

## No entry point for an ad-hoc question

`hufu decision` (the CLI subcommand) only supports `list` / `show` /
`resolve` / `assume` — post-hoc, cross-run addressing of decisions already
formed. There is no `hufu decision run "<question>"`. A decision is only
formed as a side effect of the coordinator delegating a task while a
non-`off` profile is in effect. In practice, once `judge-model` is set, the
way to actually use this team is:

```
hufu --team .agent-teams/strategic-decision "Should we adopt X or Y for Z?"
```

`decision.default-profile: standard` in `team.yaml` means every task the
coordinator delegates during that run is decided under the `standard`
profile automatically — there is no need for (and no way to use) a
`--decision-profile` flag per question with this team's current config.
