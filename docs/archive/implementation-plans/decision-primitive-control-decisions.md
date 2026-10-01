# DecisionPrimitive control decisions implementation plan

> Status: implemented — archived 2026-10-01; implemented on branch feat/decisionrt-team-points (see Implementation record)
> Authority: reference (implementation record; current behavior is defined by [DecisionPrimitive](../../architecture/decision-primitive.md) §59 and [Runtime control decisions](../../reference/control-decisions.md))
> Verified-Commit: 2026-10-01
> Supersedes: —
> Superseded-By: [DecisionPrimitive](../../architecture/decision-primitive.md) §59

## 1. Context

DecisionPrimitive has three backends (`rule`, `sidecar`, `systemone`). It has two
callers today:

- the standalone `hufu decisionrt` CLI (§40–§57 of [DecisionPrimitive](../../architecture/decision-primitive.md));
- the agent-facing `decision_primitive` tool from §58 (commit `6cc76da`). Team
  maintainers declare a catalog; an agent calls it and decides itself what to do
  with the answer.

Neither caller lets hufu's own runtime use a decision model. The runtime still
makes several bounded control decisions by asking the sidecar for a JSON object
and parsing it leniently. Those calls have no usable probability, so
"confidence" is either self-reported text or absent.

This plan adds a third caller: **control decisions**. These are fixed,
hufu-owned decision points inside the team runtime. They can ask `systemone` the
same bounded question the sidecar is asked today, and they read the answer under
Go-owned policy. This is "route B" of the 2026-10-01 evaluation. Route A is the
§58 agent tool, which already exists.

## 2. Goals and non-goals

Goals:

1. Let four existing runtime decisions use `systemone`: agent matcher,
   unattended `ask_user` selector, path reviewer, and guard reviewer.
2. Offer a `shadow` mode first. In shadow mode systemone runs next to the existing
   path and hufu records whether the two agree, without changing behavior. This
   is the only way to get accuracy evidence for nimble on hufu's own questions;
   none exists today.
3. Offer an opt-in `active` mode in which the systemone answer and its raw
   probability decide the outcome, and low confidence takes a documented safe
   outcome.
4. Make `off` the default everywhere. A run with no control-decision settings is
   byte-for-byte unchanged: no new request, event, prompt, or snapshot field.

Non-goals:

- No change to DecisionEngine, its judges, finalization, or `decision:` semantics
  (§22 still holds).
- No change to recovery disposition, the read-only bash grammar, deterministic
  guard rules, or verification. They stay deterministic.
- No SimilarTask integration. It can have up to 100 candidates, which exceeds the
  DecisionPrimitive limit of 21 options, so it needs a separate pre-filter design.
- No skill-matcher integration (multi-label), and no run-input resolver.
- No `sidecar` backend for control decisions. The sidecar already makes these
  decisions on the existing path, and the decisionrt sidecar adapter reports
  `ConfidenceNone`, which cannot satisfy a confidence threshold.
- No calibration. Confidence stays `raw`.

## 3. Decisions

- **D1 — fixed points, hufu-owned questions.** The question text, option
  mapping, context keys, and safe outcome of each point are Go constants
  versioned by spec ID (`hufu.<point>`, version `v1`). Team config can only choose
  a mode, a threshold, and the transport. It cannot change what is asked.
- **D2 — systemone only.** The backend is always `systemone`, so config has no
  `backend` key.
- **D3 — off | shadow | active.** This follows the precedent in
  [memory learning](../../architecture/memory-learning.md) §6. There is no `observe` mode, because
  every non-off mode has to call the model.
- **D4 — outages degrade to the existing path.** In active mode, a technical
  failure (backend failure, timeout, invalid output, unavailable backend) runs
  the existing path, exactly as `off` would, and records `applied: legacy`. Only
  a *successful* abstention (low confidence) takes the point's safe outcome.
  Active mode can therefore never be less available than `off`.
- **D5 — shadow runs concurrently.** Shadow starts the systemone attempt, runs
  the existing path, waits for the systemone result (bounded by its attempt
  timeout), records both, and returns the existing result. The added latency is
  `max(0, systemone − existing)`, at most the attempt timeout.
- **D6 — content-free events.** Each shadow or active invocation appends one
  durable `control_decision_observed` event. It records the point, mode, which
  outcome was applied, the systemone status, the value encoded as `"true"`,
  `"false"`, or a 0-based candidate index, raw confidence, error code, duration,
  the existing outcome in the same encoding, and agreement. It never contains
  the question, command, path, tool arguments, option labels, goals, endpoint, or
  credentials.
- **D7 — redaction before transport.** Every context string sent to systemone
  first goes through `utils.RedactSecrets`, the same redactor used for persisted
  content, which includes the coordinator's registered secrets. The sidecar path
  is already redacted by the context compiler.
- **D8 — policy snapshot pins active points only.** A hash of the transport,
  timeout, credential revision, and every *active* point with its threshold goes
  into `ExecutionPolicySnapshot.ControlDecisionHash` (omitempty). Shadow-only or
  off configuration leaves it empty, so existing snapshots and goldens do not
  change, and shadow drift never blocks a resume. Active drift fails closed, as
  route A does.
- **D9 — hufu.yaml and team.yaml.** The same `control-decisions:` block is
  accepted in both. Each field resolves team value, then hufu.yaml value, then
  default. Per-point fields resolve the same way. This lets a user turn on shadow
  for every team from `~/.config/hufu/hufu.yaml` while a team can still pin its
  own policy.
- **D10 — not under `--no-net` restrictions.** These are runtime model calls,
  like sidecar calls. They are not agent tools, so `--no-net` and `--force-mcp`
  do not disable them.

## 4. Configuration

```yaml
control-decisions:
  endpoint: http://192.168.11.117:11434/v1/systemone  # default http://127.0.0.1:11434/v1/systemone
  model: nimble              # required when any point is not off
  api-key-env: SYSTEMONE_KEY # optional; the variable must be set and non-empty
  timeout: 5s                # per attempt; default 5s, maximum 30s
  mode: shadow               # default for every point: off | shadow | active (default off)
  min-confidence: 0.8        # optional default for every point; else the point default
  points:
    path-reviewer: {mode: active, min-confidence: 0.95}
    guard-reviewer: {mode: off}
```

Validation runs at load and never makes a network call:

- Mode must be `off`, `shadow`, or `active`.
- Point names must be the four listed in §5.
- `min-confidence` must be in `[0,1]`.
- The timeout must be within `(0,30s]`.
- `model` and `endpoint` are validated by `systemone.New`.
- `api-key-env` follows the route A rules: it is a variable name, and the
  variable must be set and non-empty.
- Inline keys are not accepted.

## 5. Points

| Point | Kind | Today | Active: decided ≥ threshold | Active: abstained | Default threshold |
|---|---|---|---|---|---|
| `agent-matcher` | choice over workers (2–21) | sidecar `SelectAgent`, self-reported confidence ≥ 0.60 | selected worker | fail closed: "specify agent explicitly" | 0.60 |
| `ask-user` | choice over options (2–21), `single_choice` only | sidecar `ChooseAskUserResponse`; any failure picks the first option | selected option | notify needs-human and tell the agent to proceed on its own judgement (no first-option guess) | 0.60 |
| `path-reviewer` | boolean "real filesystem access?" | sidecar `ReviewPathAccess` | `false` drops the path; `true` keeps it | keep the path (consent still applies) | 0.90 |
| `guard-reviewer` | boolean "complies with every rule?" | sidecar `ReviewToolCall` | approve or deny | deny | 0.90 |

A point is *not applicable* to a call, and runs the existing path with no event,
when any of these holds:

- it has fewer than 2 or more than 21 candidates;
- the `ask_user` type is not `single_choice`;
- the guard rules exceed the 4096-byte context value limit.

Long free text is truncated at a UTF-8 boundary to fit the 4096-byte context
value limit: the goal, question, and command (whose sidecar limit is 3000 runes)
and the tool arguments (2000 runes). Option and worker descriptions are
truncated to 300 runes, matching the sidecar.

The existing auto-approve lexicon scorer in `ask_user` still runs before the
selector. Control decisions replace only the selector.

## 6. Layering

```text
hufu.yaml / team.yaml  control-decisions
                 ↓
internal/decisionrt/control   config, validation, fixed point specs, hash
                 ↓
existing backend factory → systemone adapter → DecisionPrimitive Runtime
                 ↑
internal/team   modes, shadow orchestration, durable events, snapshot, report
```

- `internal/decisionrt/control` must not import `internal/team`,
  `internal/agent`, `internal/sidecar`, or `cmd/hufu`. An architecture test
  enforces this.
- `internal/config` imports `control` only for the YAML type.

## 7. Work packages (one commit each)

1. **WP-1 docs.** This plan, and the amendment adding §59 to
   [DecisionPrimitive](../../architecture/decision-primitive.md).
2. **WP-2 control package.**
   - Contents: config types, validation, merge, point specs and request
     builders, typed outcomes, and hash.
   - Tests: table-driven validation, merge precedence, request shape per point,
     not-applicable limits, redaction hook, hash (empty unless a point is active,
     changes with threshold or credential), and the architecture boundary.
3. **WP-3 wiring.**
   - Config entry points: the hufu.yaml field and its merge, the team manifest
     field (strict decode, flat and v1alpha1), `agent.TeamConfig`, the
     `TeamSession` effective value and its clone, and the merge in team setup.
   - Coordinator: construction (shared with extra-model clones), secret
     registration, and the policy snapshot field with its validation.
4. **WP-4 runtime points.**
   - `control_decision_observed` event type, payload validation, and recorder.
   - Shadow and active orchestration for the four points.
   - `ask_user` abstention handling.
   - Tests:
     - every point in off, shadow, active-decided, active-abstained, and
       active-error, against an httptest System One server;
     - no request and no event when off;
     - no content in events;
     - concurrent shadow;
     - the snapshot drift test.
5. **WP-5 report.** `ControlDecisionSummary`, aggregated per point and mode:
   calls, decided, abstained, errors, compared, agreed, would-abstain under the
   threshold, mean confidence, and p50/p95 duration. Shown in `hufu report` and
   `--output json`.
6. **WP-6 docs.** The reference page, README and README.tw sections, the docs
   index, and a live shadow smoke test against nimble on the GPU host.
7. **WP-7 archive.** Move this plan to `docs/archive/implementation-plans/` with
   an implementation record.

## 8. Acceptance

- `go test ./...`, `go vet ./...`, and `golangci-lint run` are green, and
  `bin/check-docs` passes for every file this change touches.
- With no `control-decisions` block:
  - no new event is written;
  - no systemone request is sent;
  - policy snapshot JSON is unchanged (existing goldens pass untouched).
- Shadow never changes a returned outcome. A test asserts this for every point,
  for both a disagreeing and an agreeing model.
- Active-mode outages produce the existing outcome.
- Active-mode abstentions produce the safe outcome in §5.

## Implementation record

Implemented on feat/decisionrt-team-points, one commit per work package, in a
separate git worktree because another agent session was working in the main
checkout. Two pre-existing bugs found by the 2026-10-01 evaluation were fixed
first, at the user's request. The plan was a tracked draft in
docs/architecture/ during implementation and moved here once every work package
had landed.

| Commit | Work package | Content |
| --- | --- | --- |
| `c7b0194` | bug fix | A failed path review no longer drops the path from the bash consent check. Before this, a sidecar error made the path skip consent, even in unattended runs that auto-deny. |
| `9abf5b0` | bug fix | The top-level `provider-api-key` in hufu.yaml is merged (it was declared and read but never copied from either file). |
| `05c87ca` | WP-1 | This plan and DecisionPrimitive §59. |
| `079e405` | WP-2 | `internal/decisionrt/control`: config, merge, validation, the four point contracts, `Decide`, and the active-only hash. |
| `4726eb4` | WP-3 | hufu.yaml and team.yaml blocks, session and coordinator wiring, secret registration, and `ControlDecisionHash`. |
| `15fabdf` | WP-4 | Shadow and active orchestration for the four points, the `control_decision_observed` event, `ErrAskUserAbstained`, and the model-call audit extension. |
| `fdc1ff3` | WP-5 | `SummarizeControlDecisions`, the report section, `--output json`, and `hufu inspect control-decisions`. |
| `53b371a` | WP-6 | The reference page and links from §59, both READMEs, the agent-format reference, the operator command reference, and the docs index. |
| `3f5a1e3` | WP-6 fix | The inspect example registered in `canonicalExamples` and the operator command reference regenerated. |

Baseline at `6cc76da`: `go test ./...` passed (EXIT 0). `bin/check-docs`
already failed because docs/reference/performance-gate.md has no lifecycle
header. This change leaves that file alone; with it skipped, every other header
and link passes.

After implementation:

- `go vet ./...` passes and `golangci-lint run ./...` reports 0 issues.
- The first full `go test ./...` passed 49 of 50 packages. cmd/hufu failed
  `TestGeneratedOperatorCommandReferenceIsCurrent`, because 53b371a hand-edited
  the generated operator command reference. `3f5a1e3` registers the example in
  `canonicalExamples` and regenerates the page, and cmd/hufu passes again.
- `bin/check-docs` passes with performance-gate.md skipped.

### Live smoke test

Two `hufu` runs drove a one-worker scratch team (bash and one natural-language
guard rule) against Ollama 0.35.0 with nimble on a LAN GPU host. The existing
path used the configured sidecar model. The smoke test is not an acceptance
dependency, and fifteen observations are not accuracy evidence.

| Run | Point | Calls | Result |
| --- | --- | --- | --- |
| shadow | guard-reviewer | 5 | 5/5 agreed with the sidecar; mean raw confidence 0.94; one answer (0.863) was below the 0.90 threshold; nimble 1.0–1.2 s, sidecar 1.1–1.8 s |
| shadow | path-reviewer | 2 | 2/2 agreed; 0.991; nimble 1.1 s, sidecar 1.1–6.0 s |
| active | guard-reviewer | 5 | `rm` of the scratch file denied twice (false, 0.999–1.0) with "the decision model judged that this call violates a guard rule"; reads approved (0.94–0.95); the file survived |
| active | path-reviewer | 3 | every path kept (true, 0.98–0.994), so unattended consent denied them as before; `echo <path> \| sed …` was also judged a file access (0.98), the conservative direction |

`hufu report` and `hufu inspect control-decisions --workspace <dir>` showed the
same aggregates. Both runs exited 7 (`acceptance_not_configured`), as expected
for a team without acceptance.

### Deviations from the plan

- **Per-file and merged validation are split.** `Config.Validate` checks one
  block (modes, points, thresholds, timeout range, variable name) when
  hufu.yaml or team.yaml is loaded. `control.New` validates the merged block
  completely (model, endpoint, credential) when the coordinator is built,
  because the model can come from another file.
- **The threshold is applied by the control layer.** It is not
  `AcceptancePolicy.MinConfidence`, because a low-confidence runtime abstention
  drops the confidence, and shadow records need the raw value of an answer that
  active mode would reject.
- **The agent matcher sorts workers by name** on the non-off path.
  `uniqueWorkerDefs` follows map order, so without sorting the request and its
  option indexes would not be reproducible. The off path is unchanged.
- **Recording is best effort.** A journal failure only warns. Unlike route A's
  settlement, an observation never decides whether an answer is published.
- **`hufu inspect control-decisions` was added** next to the report and JSON
  projections. It aggregates every run in the branch lineage, which is the
  evidence needed before switching a point to active.
- **The model-call audit now scans internal/decisionrt.** It registers every
  `systemone.New(` call site and the systemone transport, so the team runtime
  cannot construct the transport directly.
- **`renderInspectText` lost its task case to a helper.** The case moved to
  `renderInspectTaskText` to stay under the gocyclo limit, and
  `InspectQuery.Validate` gained a helper for the same reason. Both are pure
  moves.

### Not done

- SimilarTask, the skill matcher, the run-input resolver, and the text
  heuristics in failure classification remain non-goals (see Section 2).
- Systemone calls have no concurrency limit, provider admission, or cost
  accounting; only the attempt timeout bounds them. Queueing on a single GPU
  shows up as latency or `timeout` errors in the summary.
- Every shadow or active call appends one event; the path reviewer can run
  several times per bash command.
- Aggregation is per workspace lineage; there is no cross-workspace view.
- No point has accuracy evidence beyond the smoke test above, and confidence
  remains raw.
