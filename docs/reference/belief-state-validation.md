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
| A: pure finite Bayesian core | In progress | Pending repository validation. |
| B: durable binding and replay | Pending | — |
| C: runtime integration and inspect | Pending | — |
| D: advisory policy | Pending | — |

The user selected explicitly synthetic models for initial runtime integration
and advisory-only policy. Production calibration and active candidate dispatch
are outside this implementation's acceptance scope.
