# Bayesian belief-state implementation validation

> Status: active implementation record
> Authority: reference
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
| D: advisory policy | Pending | — |

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

A delivers computation only. Runtime integration is delivered by C; advisory
policy consumption remains stage D work.

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
in [`belief-state.md`](belief-state.md). D remains unfinished and no policy
recommendation or active candidate selection is delivered by A–C.
