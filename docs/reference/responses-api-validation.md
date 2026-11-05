# Responses API implementation evidence

> Status: in progress
> Authority: reference
> Baseline: `9420c2f58118cb5647e582ace02cc840452cdfea`
> Specification: [Responses API integration](../architecture/responses-api.md)
> Date: 2026-11-05

The documentation-only commit `8d4b46d2` promotes the specification; its Go
sources match the baseline. Implementation proceeds through the specification's
work packages, with a separate commit for each completed phase. This record is
updated with observed results, not expected outcomes.

## Environment and resource ownership

Linux/amd64, mise-managed Go 1.27.2 and golangci-lint 2.14.0,
`GOTOOLCHAIN=local`, `GOMAXPROCS=2`, build/test `-p 2`.
The existing `GOCACHE=/home/ubuntu/.cache/go-build`,
`GOTMPDIR=/home/ubuntu/.cache/go-build/tmp`, and
`GOMODCACHE=/home/ubuntu/gohome/pkg/mod` are preserved. Tests use a private task
`TMPDIR`; there are no live model requests or provider credentials involved.

Before baseline validation: `/tmp` had 7.3 GiB free out of 8 GiB;
available RAM was 11 GiB; the cache/module volume had 57 GiB free.
Allocated cache usage was approximately 26 GiB and module usage 17 GiB.
These are shared pre-existing caches, not task-owned artifacts.

## Baseline

An initial restricted-sandbox `mise run test` encountered
`httptest: failed to listen on a port: ... socket: operation not permitted`
in `cmd/hufu` and `internal/evalharness`. That attempt was cancelled after
confirming the environmental restriction. Its exact child processes were stopped;
the abandoned Go work directory (102 MiB) and CLI fixture binary (128 MiB) were
removed. No source or test was changed to avoid the restriction.

The baseline is rerun with local test listener permission and the same Go
sources, paths, caches and task settings. This permission enables local fixture
servers; it does not authorize live API verification.

| Command | Observed result |
|---|---|
| `mise run test` | Passed, exit 0; CLI 183.179s, improver 160.834s, team 831.510s |
| `mise run vet` | Passed, exit 0 |
| `mise run lint` | Passed, exit 0; 0 issues |
| `mise exec -- go build -p 2 -o /dev/null ./cmd/hufu` | Passed, exit 0 |

After baseline validation the shared build cache allocated 26,460,436 KiB
(approximately 25.2 GiB). The initial rounded 26 GiB reading does not establish
exact task growth. Scratch retained approximately 1.3 MiB of bounded evidence
and fixture leftovers for cleanup; no baseline executable is retained. The
abandoned work directory and superseded CLI binary reclaimed about 230 MiB.

## Implementation phases

| Phase | Status | Evidence |
|---|---|---|
| WP 0: pinned SDK wire contract | Complete | `internal/agent/responses_sdk_test.go`; offline HTTP transport, no live requests |
| WP 1: configuration | Not started | — |
| WP 2: transport and fixtures | Not started | — |
| WP 3: options, admission and settlement | Not started | — |
| WP 4: durable binding and resume | Not started | — |
| WP 5: offline integration | Not started | — |
| WP 6: diagnostics and user documentation | Not started | — |

Real endpoint verification is outside these acceptance gates. The user will
separately authorize it later; no offline result proves live provider support.

## WP 0: pinned SDK wire contract

Five test groups cover explicit protocol selection, all four language-model
entry points (including tool-based structured output), reasoning options keys
and the pinned model-classification branches, default `store:false`,
provider-error HTTP counts, and skipped plain/encrypted reasoning history.
The tests assert the SDK limitations instead of declaring them implemented.
The transport is an in-memory `RoundTripper`, so it cannot contact a real
provider or depend on local listener permissions.

- `mise exec -- go test -p 2 -timeout 5m ./internal/agent -run '^TestResponsesSDK' -count=1`: passed, exit 0.
- `mise exec -- go test -p 2 -timeout 20m ./internal/agent`: passed, exit 0.
- `mise run lint`: passed, exit 0, 0 issues.
- Other packages and production Go sources are unchanged; their completed
  baseline results remain applicable. Full runtime validation is repeated
  after production integration.
- Shared cache after this phase: approximately 26,490,040 KiB, around
  29 MiB above the exact post-baseline reading. No task binaries are retained.
