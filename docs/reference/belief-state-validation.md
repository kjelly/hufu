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
| B: durable binding and replay | Pending | — |
| C: runtime integration and inspect | Pending | — |
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

A delivers computation only. Event persistence, Coordinator state, runtime
observation admission, inspection, and policy consumption remain B–D work.
