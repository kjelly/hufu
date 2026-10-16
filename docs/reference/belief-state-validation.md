# Bayesian belief-state implementation validation

> Status: active
> Authority: reference
> Verified-Commit: `3e358d12d8c78e6b669b4ca680b168a2829088cb`
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
| D: advisory policy | Complete | Frozen policies/catalog eligibility, durable generations, transactional head fence and historical inspection; checks below. |

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
| `go test -race -timeout 40m ./internal/belief/... ./internal/team/... ./internal/inspect/...` | 0 | Complete aggregate suite passed: belief 1.288s, team 1912.733s, preset 1.053s, inspect 119.184s. The final stateless append-capacity guard has its separate passing targeted follow-up. |

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
this fix. The full aggregate race suite passed with a 40-minute package
limit; the earlier timed-out runs remain recorded as failures.

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
The aggregate 40-minute race run subsequently exited 0 without a data-race
report. Its full-package scope plus the final guard/CLI follow-ups complete
the planned local gates. The timeout allowance skipped no test or assertion.

## Requirements and artifact audit

The implemented scope is the user's synthetic-model A–C delivery and D's
advisory recommendations. Active dispatch, calibrated domain likelihoods and
human usability measurements are outside that accepted scope. The
[implemented plan](../archive/implementation-plans/belief-evidence-implementation-plan.md)
is archived under the documentation lifecycle; the current contract is the
[runtime reference](belief-state.md).

| Requirement | Current evidence |
| --- | --- |
| Move and version the integrated plan; preserve phase commits | Initial documentation commits `de84101e` and `ee06ef23`; A `c48d2fe5`, B `31b4880b`, C `dbf86066`, D `cc551630`. The original baseline-to-`ee06ef23` diff contains documentation only. |
| Finite schema, exact update, independence and deterministic identity | [Pure model/core](../../internal/belief/projector.go), [wire schema](../../internal/belief/model.schema.json), and [Go function: TestKnownPosteriorAndDuplicateReplay](../../internal/belief/projector_test.go). Other core cases cover zero support, ties, limits, shared-source/group refusal, wire errors, input immutability and fuzzing. Core model/projector files are unchanged since A. |
| Existing journal/CAS only; authentic source, scope, deduplication and retraction | [Journal adapter](../../internal/team/belief_journal.go), [event reducer](../../internal/team/belief_events.go), [Go function: TestBeliefRetractionTombstoneAndPrior](../../internal/team/belief_journal_test.go), and [Go function: TestBeliefEventScopeCannotBeRebound](../../internal/team/belief_journal_test.go). No belief database or replacement scheduler was introduced. |
| Crash/uncertain commit and branch isolation | [Go function: TestBeliefUnknownCommitUsesSameAdmission](../../internal/team/belief_journal_test.go), [Go function: TestBeliefRuntimeRepairsForkGapOnlyOnSelectedBranch](../../internal/team/belief_runtime_test.go), and policy branch/cursor tests. Sources after a fork remain invisible; repair writes no ancestor epoch. |
| Runtime state and expendable cache; no repeat of completed execution | [Common runtime projection](../../internal/team/coordinator_belief.go), [Go function: TestBeliefRuntimeActionAndExpendableCache](../../internal/team/belief_runtime_test.go), real worker verification and normal session/checkpoint restart tests. Durable source/admission repair preserves the single action marker. |
| Locked models/policies, version drift and unavailable artifacts | [Team compilation](../../internal/team/belief_config.go), [Go function: TestBeliefRuntimeMissingArtifactNeverReloadsTeamModel](../../internal/team/belief_runtime_test.go), model/policy resume drift and missing/restored artifact history tests. Unknown probabilities remain null. |
| Read-only scoped inspection, equivalent text/JSON, no raw evidence | [Inspector](../../internal/inspect/beliefs.go), [Go function: TestBeliefInspectReconstructsWithoutWritingOrExposingRawEvidence](../../internal/inspect/beliefs_test.go), policy inspection and CLI SQLite fixtures. Tests compare bytes/modes/timestamps and reject private command/output leakage. |
| Advisory generation and authority, with no extra dispatch or budget reset | [Policy journal](../../internal/team/belief_suggestion_journal.go), [Go function: TestBeliefPolicyRuntimeAdvisoryAndRestart](../../internal/team/belief_policy_runtime_test.go), catalog/grant tests, duplicate-source actor regression and capacity boundaries. Active mode is rejected. |
| Reject late results in the write transaction | [Go function: TestEventStoreConditionalAppendAcrossHandles](../../internal/team/event_store_conditional_test.go), late-write injection, concurrent writers and uncertain advisory commit tests. The global head fence includes invisible sibling writes. |
| Neutral core and safe configuration | Core changes add no concrete-team-name branch; the four changed JSON/schema files have zero IP/credential-pattern hits. Policy files cannot supply grants or shell commands. |
| Automated gates | Complete normal tests, targeted and full planned-package race, vet, lint and document checks passed as recorded above. The full normal run covers all four package groups named in the plan. |

The residual local scratch proposal was compared with the integrated plan and
removed from `docs/tmp`; that directory contains no remaining proposal files
and no path under it is tracked. Its source SHA256 was
`6919048af8033d9af02d505586ff8c17c04b953ce792ecda59d2c810c0eff714`.
The unmodified source backup is retained with the local validation artifacts,
outside the documentation authority chain. No production guidance links to
scratch files.

The final document audit passed `bin/check-docs`: all active document headers,
relative links and named Go function anchors are checked. The PR's initial
docs failure was reproduced locally as a missing `Verified-Commit` header;
commit `f6b3971d` added the required lifecycle metadata. The completed plan is
now archived and linked from the documentation map. Final changes after the
validated production commit are documentation only.

## PR review follow-up (2026-10-16)

Reviewed revision: `9fb5f30562bc72badc8ad97d46baab44ba3ff134`.
The [initial assessment](https://github.com/kjelly/hufu/pull/18#issuecomment-6193029078)
and [merge blocker](https://github.com/kjelly/hufu/pull/18#issuecomment-6204983011)
were checked against that revision and the current callers.

| Review point | Assessment and change |
| --- | --- |
| Top-level verification/input hash and nanosecond durations | Already fixed in C; the existing real worker/action and inspector regressions remain applicable. |
| Nil verification panic in private receipt helpers | No reachable normal caller was found: callers check `VerifyResult` or use `beliefSourceReceipt`, which rejects receipts without verification. No fabricated identity or fallback probability was added. |
| Bookkeeping under `taskProjectionMu` | Confirmed. Durable append and live publication retain their shared lock; the captured durable event is passed to synchronous belief bookkeeping after unlocking. The separate belief lock still serializes publications and journal-head fencing still rejects stale advisory writes. Non-verification transitions skip the belief path. |
| Repeated historical suggestion prefixes | Confirmed. A shared ordered reducer validates events once and snapshots only the recommendation's epoch. Historical actor, cursor, generation, retraction, branch and unavailable-artifact checks are retained. No persistent cache was added. |
| Source ordering with equal receipt keys | Added event ID and hash tie breakers. A two-experiment, same-receipt integration test repeatedly checks the entire projection. |
| Current-branch completed admission replay | Added a per-refresh settled-receipt index. Local admission is settled only after closure; inherited admissions retain ancestor ownership. The partial-admission test repairs the missing close before testing the zero-I/O fast path. |
| Multiplication after unresolved dependency | Skips further high-precision multiplication while still validating every remaining observation and retaining its identity. With current caps, the claimed intermediate-bit overflow is not reachable (`256 * 30 + 30 < 16384`); the unnecessary work is real. |
| Public state digest helper | Optional API suggestion, not a current correctness defect. Callers obtain state digests from `belief.Project`; no external manual state rehash path exists. No unused public API was introduced. |

The [Go function: TestBeliefBookkeepingReleasesTaskProjectionLock](../../internal/team/belief_concurrency_test.go)
regression uses a channel-controlled journal read rather than a latency threshold. While the first terminal caller waits on belief I/O, its task
is already published and another task can append and publish both progress and
completion. After release, both epochs and their first advisory generations are
verified, and live task state agrees with event replay. The caller still waits
for its own synchronous bookkeeping; this is not an asynchronous worker design.

The history fixture writes actual hash-chained events and immutable artifacts.
The [Go function: TestBeliefHistoryReplayWorkGrowsLinearly](../../internal/team/belief_history_test.go)
growth test verifies recommendations at 8, 32 and 128 historical decisions,
including exactly one model resolution per read and one policy resolution per
recommendation. The benchmark measures 16, 64 and 256 historical recommendations;
it does not measure model quality or end-to-end production task latency.

Benchmark command on both revisions:

```bash
go test ./internal/team -run '^$' -bench '^BenchmarkBeliefHistoryReplay$' -benchtime=3x -count=1 -timeout=5m
```

The pre-fix measurements use a `git archive` of the reviewed revision with the
same benchmark fixture added; production code is untouched. Both runs use Go
1.27.2, Linux/amd64, six logical Go CPUs and short `/tmp/hb` temporary roots.
Other validation was active on the host, so elapsed times are indicative samples,
not a production latency guarantee. Memory is decimal MB per replay.

| Historical recommendations | Before ms/op | After ms/op | Before MB/op | After MB/op | Before allocations/op | After allocations/op |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| 16 | 223.17 | 6.88 | 5.220 | 0.790 | 30,010 | 4,301 |
| 64 | 2,051.32 | 37.67 | 68.224 | 3.055 | 377,432 | 16,420 |
| 256 | 18,788.99 | 312.12 | 1,032.635 | 11.881 | 5,663,344 | 64,510 |

Both benchmark commands exited 0. The deterministic growth regression guards
artifact-resolution work, while the benchmark exposes timing and allocation
costs. Full journal integrity verification still occurs on runtime refresh;
this change removes repeated historical-prefix reconstruction and shared task
lock contention, rather than claiming constant-time runtime bookkeeping.

Follow-up checks use the installed Go 1.27.2 binary/`GOROOT`, `GOTOOLCHAIN=local`,
`GOTMPDIR=/tmp/hb` and `TMPDIR=/tmp/hb`. Local test sockets are permitted.

| Check | Exit | Evidence |
| --- | ---: | --- |
| Isolated reviewed revision: `go test ./internal/team -run '^TestBeliefBookkeepingReleasesTaskProjectionLock$' -count=1 -timeout=1m` | 1 | Expected regression: `belief I/O preceded live publication`; the same channel-controlled test passes with the fix. |
| Initial `go test ./...` | 1 | Changed team package passed (333.466s), CLI passed (309.912s), inspector passed (8.662s). Unchanged `internal/tools` failed `TestBashHandsCompleteOutputToOffloader/normal`: captured 139,277 bytes instead of 168,907. |
| Isolated reviewed revision: `go test ./internal/tools -run '^TestBashHandsCompleteOutputToOffloader$' -count=20 -timeout=3m` | 0 | The shell-output mismatch did not reproduce in these 20 targeted repetitions (1.200s); no root cause or fix is claimed. No tools source or test was changed. |
| `go test -race ./internal/belief ./internal/team ./internal/inspect -run 'TestBelief\|TestUnresolved\|TestActionProgress.*Transition\|TestActionProgressWaits\|TestEventStoreConditional' -count=1 -timeout=5m` | 0 | Belief 1.790s, team 32.404s, inspector 2.036s; no race report. |
| `go vet ./...` | 0 | Complete static analysis of the final source. |
| `golangci-lint run --allow-parallel-runners` | 0 | Final source and tests: 0 issues; the supported runner-lock option disables no checker. |
| `bin/check-docs` | 0 | Lifecycle headers, relative links and named Go function anchors validated. |
| Final `go test ./...` rerun | 0 | Complete suite passed: team 211.006s, tools 50.452s, CLI 91.354s. The initial shell-output mismatch remains recorded above and is not claimed fixed. |

The review fixes are committed in `3e358d12d8c78e6b669b4ca680b168a2829088cb`. The final document
changes do not modify the validated code. The PR remains draft for re-review;
no merge or new active policy behavior is enabled.
