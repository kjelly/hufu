# Bayesian belief-state implementation validation

> Status: active
> Authority: reference
> Verified-Commit: `cc5516300df2b0ee4316b398d8bdd07a0560fe51`
> Supersedes: —
> Superseded-By: —
> Baseline: `c03b785788fbe9b1f636e8b8baca745b6345114e`
> Date: 2026-10-15

## Baseline

No Go source was changed while the baseline commands were running. The
intervening `de84101e` commit only versions the implementation plan.

Environment: Linux amd64, Go 1.27.2, golangci-lint 2.14.0 (built with Go 1.27.0).

| Command | Exit | Result |
| --- | --- | --- |
| `go test ./...` in restricted executor | 1 | Existing httptest listeners denied by the sandbox (`socket: operation not permitted`). |
| `TMPDIR=/tmp/hufu-beliefs-validation/tmp go test ./...` with local sockets allowed | 1 | Six existing terminal tests exceeded Unix socket path limits because Go 1.27 `testing.T.TempDir` uses `GOTMPDIR`. |
| `GOTMPDIR=/tmp/hb TMPDIR=/tmp/hb go test ./...` with local sockets allowed | 0 | Entire baseline passed; internal/team took 244.892s. |
| `go vet ./...` | 0 | Passed. |
| `golangci-lint run` | 0 | 0 issues. |

The six path-related failures were TestTerminalBrokerAttachStreamsInputAndOutput,
TestTerminalBrokerRejectsConcurrentAttach,
TestTerminalAttachmentClientTransfersInputAndDetaches,
TestTerminalBrokerTransferRequiresExplicitOperatorHandoff,
TestTerminalBrokerDisconnectKeepsTaskPaused, and
TestTerminalTools_LazilyEnablePTYOnStart. No source fix or test skip was needed.
Use a short **GOTMPDIR as well as TMPDIR** for later full and race runs.

## Delivery status

| Stage | State | Evidence |
| --- | --- | --- |
| A: pure finite Bayesian core | Complete | Schema, exact update, bounded validation, deterministic digests; checks below. |
| B: durable binding and replay | Complete | SQLite journal, immutable model binding, canonical receipt admission, tombstones and lineage replay; checks below. |
| C: runtime integration and inspect | Complete | Opt-in team compilation, execution-policy pinning, canonical runtime observations, restart/branch repair, text/JSON inspection; checks below. |
| D: advisory policy | Implemented and stage-validated; aggregate race pending | Frozen policies/catalog eligibility, durable generations, transactional head fence and historical inspection; checks below. |

The user selected explicitly synthetic models for initial runtime integration
and advisory-only policy. Production calibration and active candidate dispatch
are outside this implementation's acceptance scope.

## Stage A

`internal/belief` uses only the standard library in production. Model JSON is
strictly decoded and cross-field validated; the schema fixture is also checked
with the repository's existing Draft 2020-12 validator. Synthetic values are
confined to testdata and remain explicitly marked in State.

| Command (short GOTMPDIR/TMPDIR) | Exit | Evidence |
| --- | --- | --- |
| `go test ./internal/belief/...` | 0 | Known posterior, 100 duplicate replays, independence, null/zero, retraction by recomputation, quantization, identity, resource and wire validation. |
| `go test -race ./internal/belief/...` | 0 | Passed. |
| `go test ./internal/belief -run '^$' -fuzz '^FuzzProjection$' -fuzztime=5s` | 0 | 54,733 executions; no failure. |
| `go test ./internal/belief -run '^$' -bench '^BenchmarkProjection$' -benchmem` | 0 | 52,583 ns/op, 6,648 B/op, 44 allocs/op; 2 hypotheses, 1 observation. |
| `golangci-lint run` | 0 | 0 issues; executor permits writes to the standard lint cache. |

Benchmark hardware: Intel Core i7-6700K @ 4.00GHz, linux/amd64, Go 1.27.2,
6 benchmark workers as reported by Go. Timing is a measurement, not an SLO.
The initial lint run found a boolean style warning; the implementation was
corrected and the complete lint gate passed on rerun.

A delivers computation only. Runtime integration and advisory policy
consumption are recorded under C and D below.

## Stage B

`BeliefJournal` verifies the existing journal and branch lineage, pins models
in the existing immutable artifact store, and writes append-only model/epoch/
observation/retraction/closure events. Admission derives pass/fail from the
source receipt, checks its occurrence and revision, and preserves the first
source reference when another event repeats the same receipt. Different
invocations in one correlation group produce an unresolved distribution.

| Command (short GOTMPDIR/TMPDIR) | Exit | Evidence |
| --- | --- | --- |
| `go test ./internal/team -run '^TestBelief'` | 0 | Durable replay, 100 duplicate admissions, receipt aliases, prior reconstruction after tombstone, missing model, forged outcome/source/scope, revision isolation, closed epochs, fork cursors, crash gap and uncertain append. |
| `go test -race ./internal/team -run '^TestBelief'` | 0 | Passed, including final session/attempt/task/branch binding checks. |
| `golangci-lint run` | 0 | Final complete gate: 0 issues. |

The first adapter's subject revision is the receipt's admitted run-input hash;
its environment revision hashes the recorded verifier contract and execution
target. These are deliberately narrow historical receipt scopes, not a live OS
or complete source-tree fingerprint. Replay only reads events and artifacts;
it never executes the stored verifier command or repairs a missing artifact
from current configuration. Runtime wiring and inspection are delivered by C.

## Stage C

Team loading compiles a bounded model through a confined filesystem root and
pins the selected model/experiment in the existing execution-policy snapshot.
After canonical task transitions, the Coordinator derives observations from
durable receipt verification or the terminal event's exact current-receipt
binding. Existing receipt bytes, execution status, budgets and authorization
remain under their original owners. Legacy event durations are explicitly
decoded as integer nanoseconds.

The disposable Coordinator projection rebuilds from historical model and
adapter bindings. `inspect beliefs` uses the existing globally verified branch
lineage, a read-only artifact store, and the same reducer. It reports execution
separately from model identity, synthetic flag, effective observations,
distributions, source references, retractions and historical diagnostics.

| Command (short GOTMPDIR/TMPDIR for tests) | Exit | Evidence |
| --- | --- | --- |
| `go test ./internal/team ./internal/inspect ./cmd/hufu -run '^TestBelief'` | 0 | Team resource boundaries, policy drift, real action and worker verification, missing model, crash repair, fork isolation, text/JSON and read-only inspection. |
| `go test ./internal/team -run '^TestBelief'` | 0 | Final restart/checkpoint and source-binding tests included. |
| `go test -race ./internal/team ./internal/inspect -run '^TestBelief'` | 0 | Final runtime/inspection state: team 14.275s; inspect 2.124s. |
| `go test ./...` with local sockets allowed | 0 | Complete suite passed; team 333.150s and tools 66.964s. |
| `go test ./cmd/hufu -run '^TestBelief'` | 0 | Follow-up after the final CLI-only missing-binding diagnostic rendering fix; no source execution. |
| `go vet ./...` | 0 | Final gate passed. |
| `golangci-lint run --allow-serial-runners` | 0 | Final complete gate: 0 issues. The option waits for the shared lint lock; no checker is disabled. |

An earlier full run failed the unchanged
`TestBashHandsCompleteOutputToOffloader/normal`: captured output was 131,085
bytes instead of 168,907. The original baseline was extracted with `git archive`
and its exact test passed 30 consecutive repeats. The unchanged shell runner
has a 250ms output-drain grace; the observation is consistent with scheduling
sensitivity but was not reproduced in isolation, so this record does not claim
a proven cause or a shell fix. The subsequent full suite passed. No test was
skipped, and no tools/process implementation was changed by this feature.

During development, the real action test exposed the legacy duration wire
format mismatch, and lint found two functions above the existing complexity
limit. The adapter was corrected and small validation/snapshot helpers were
extracted; final tests and lint passed. The restart test uses the normal frozen
input admission, session checkpoint, event-store startup and
`ResumeInterruptedTasks` APIs and proves a completed action is not repeated.

Usage and the deliberately narrow receipt/input/environment scope are described
in [`belief-state.md`](belief-state.md). No policy recommendation or active
candidate selection is delivered by A–C; D adds advisory records only.

## Stage D

An optional, confined `belief.policy-file` compiles a strict finite advisory
policy against existing catalog candidates with side-effect `none`. Model and
policy digests plus frozen discover/propose eligibility live in the existing
CAS and execution-policy snapshot. A source actor without the frozen grant
cannot select that candidate. The pure policy consumes the published Q9
distribution with deterministic ties; it cannot call a provider or tool.

Each historical suggestion binds the epoch, snapshot/state digests, policy
version/digest, source cursor/hash/actor, candidate/hypothesis or unavailable
reason, synthetic flag and epoch-local generation. The conditional journal
checks the global chain head inside SQLite's write transaction. Late results
are refused, uncertain commits reconcile to the existing generation, and
inherited epochs remain read-only on a child branch.

The runtime records suggestions after its existing observation refresh.
Checkpoint/resume and cache reconstruction preserve the recorded generation
without dispatching a candidate, repeating execution, changing task status or
resetting execution budgets. Inspector projections retain historical choices
when their artifacts are missing and distinguish validation unavailability
from a recorded unavailable result. Restoring an artifact never rewrites an
earlier availability fact.

| Command (short GOTMPDIR/TMPDIR for tests) | Exit | Evidence |
| --- | --- | --- |
| `go test ./internal/belief ./internal/team ./internal/inspect ./cmd/hufu -run 'TestPolicy\|TestEventStoreConditional\|^TestBelief' -timeout 120s` | 0 | Final binding helper included: team 7.870s, inspect 0.973s, CLI 1.081s. Synthetic choice/tie/schema, authority, 100 retries, retraction, concurrent writers, late append, uncertain commit, forged history, missing/restored artifacts, branch/global-head isolation, real runtime restart/budgets and read-only/text inspection. |
| `go test ./...` with local sockets allowed | 0 | Final complete suite: team 336.143s, CLI 238.726s, tools 64.207s; no skips. |
| `go vet ./...` | 0 | Final complete gate passed. |
| `golangci-lint run --allow-parallel-runners` | 0 | Final publication gate: 0 issues. A held shared lock blocked serial runners; the supported parallel option leaves all checkers enabled. |
| `go test -race ./internal/belief/... ./internal/team/... ./internal/inspect/... -run 'TestPolicy\|TestEventStoreConditional\|^TestBelief\|^TestVerificationRepairReturnsToProducerOnce$' -count 1 -timeout 3m` | 0 | All new belief/policy/persistence tests and the synchronized repair test: team 42.873s; inspect 3.509s. |
| Original baseline archive: `go test -race ./internal/team -run '^TestVerificationRepairReturnsToProducerOnce$' -count 20 -timeout 3m` | 1 | Independently reproduced the same pre-existing live-item assertion race (0.829s). |
| `go test -race -timeout 20m ./internal/belief/... ./internal/team/... ./internal/inspect/...` | 1 | Found that baseline race, then reached the package timeout during `TestSessionDataConcurrentCheckpointNoRace`. |
| `go test ./...` after the baseline test synchronization | 0 | Complete follow-up suite, local sockets allowed; all tests passed. |
| `go test -race -timeout 40m ./internal/belief/... ./internal/team/... ./internal/inspect/...` | Running | Complete aggregate rerun. The final stateless append-capacity guard has a separate targeted follow-up. |

The first complete test run included two development fixtures with incomplete
metadata: a branch event lacked an actor and a catalog schema lacked explicit
`additional-properties: false`. They were corrected. That same run also failed
the unchanged `TestContextQuerySelectionRollsBackAllScopesOnFailure/shared_deadline`
with 3 calls instead of 2 (145.050324ms). The context query implementation and
test are unchanged from the passing original baseline. The final full suite
passed; this record does not claim a proven cause or a context-query fix.

The initial full race command reached Go's default 10-minute package timeout
while `TestSetAcceptance_RejectedEmptyUpdateAudited` had just started. The
captured log had no data-race report. This is not recorded as a passing race
gate. The 20-minute rerun continued further and detected a race in the unchanged
`TestVerificationRepairReturnsToProducerOnce`: its assertion read a live
`TodoItem.RemediationContext` while the canceled repair worker installed a new
projection into that same item. It later timed out during the concurrent
checkpoint stress test; it was not a passing race gate.

The exact repair test reproduced the same race in an isolated archive of the
original baseline. Commit `8c44625c` fixes only test synchronization: receive the
relaunched worker result before inspecting the aliased item. All existing
assertions remain. The targeted race suite, vet and complete lint passed after
this fix. The full aggregate race suite is running with a 40-minute package
limit; timing out does not count as passing.

Lint initially reported complexity 44 against the existing threshold 40 in
the extended belief reducer. The binding checks were extracted into a shared
validator, preserving the threshold and all enabled checkers; final lint and
targeted regression checks passed.

The final provenance regression also covers duplicate admission events that
reference an alias of the same receipt from another actor. Both math and
policy retain the first source, so a duplicate cannot change proposal
eligibility or create a generation. The writer checks run-history and epoch
capacity before appending; a valid append cannot cross the reader's replay
limit. Boundary tests cover the last accepted slot and refusal at either
limit. The final guard targeted race validation passed; the aggregate run began before
that stateless guard, so its targeted follow-up is reported separately.

Final frozen-code follow-ups:

- `go test -race ./internal/belief/... ./internal/team/... ./internal/inspect/... -run 'TestPolicy|TestEventStoreConditional|^TestBelief|^TestVerificationRepairReturnsToProducerOnce$' -count 1 -timeout 3m`: exit 0; team 35.197s, inspect 3.289s, belief 1.550s. Includes both source-alias and append-capacity regressions.
- `go test ./cmd/hufu -run '^TestBelief'`: exit 0 after the final CLI-test-only promoted-field literal cleanup.
- `go vet ./...`: exit 0 with the final production code. The CLI-only test formatting follow-up is checked separately.
- `golangci-lint run --allow-parallel-runners`: exit 0, 0 issues after that last test cleanup. The lock coordination option disables no checker and changes no threshold.

Publication-stage required gates passed on the final production code:
`go test ./...` exited 0 (team 381.005s), `go vet ./...` exited 0, and
`golangci-lint run --allow-parallel-runners` exited 0 with 0 issues. After
the CLI-test-only literal cleanup, `go test ./cmd/hufu -run '^TestBelief'`
and `go vet ./cmd/hufu` also exited 0. No tests were skipped. One intermediate
non-race run from before the final append guard exhausted its default
10-minute package timeout; the final frozen-code complete run passed.
The aggregate 40-minute race run remains in progress, so the PR stays draft
and no all-suite-race success is claimed yet.
