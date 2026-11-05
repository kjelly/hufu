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
| WP 1: configuration | Complete | Normalization, canonical/legacy config, merge and alias tests; production Responses remains gated |
| WP 2: transport and fixtures | Complete | Direct/in-process/subprocess paths, base-path preservation, SSE guard and scripted Responses fixtures |
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

## WP 1: configuration

Added `api-protocol`, preserving the empty legacy value and omitting the new
field from legacy JSON/YAML serialization. Canonical backend assembly checks
backend kind, rejects misplaced `protocol`/`api-protocol`, and normalizes the
provider configuration. Provider alias equivalence includes the normalized
protocol and preserves the explicit-versus-legacy distinction.

The initial merge test exposed a specification assumption: CLI backend
assembly replaces complete entries, while the legacy config file loader and
`MergeProviderConfigs` inherit individual non-empty fields. Both existing
behaviors are preserved; the specification now describes them separately.
Explicit `chat-completions` resets an inherited Responses setting in field
merges. Production provider-manager construction rejects Responses with
`ErrResponsesNotEnabled` until the later integration gates are complete.

- API-protocol/backend targeted tests in config, agent and CLI: passed, exit 0.
- Complete config and agent package suites: passed, exit 0.
- `mise run lint`: passed, exit 0, 0 issues.
- Baseline full-suite evidence predates these source changes; full runtime
  validation after integration remains required.
- Shared cache allocated 27,049,580 KiB (about 575 MiB above the exact
  post-baseline reading). Task scratch is approximately 1.3 MiB. Project export
  paths are inventoried privately for provenance-aware cleanup of superseded
  phase outputs; shared dependencies and configured cache locations are kept.

## WP 2: transport and deterministic fixtures

Provider construction and boundary rebuild now share an immutable explicit
protocol resolution. Legacy exported constructors remain Chat-only by default;
the production manager's Responses enablement gate is still closed. The
scripted provider supports Responses text/tool events and known/unknown usage
through its existing in-memory ownership boundary. Chat event-index rewriting
is restricted to POST requests ending in `/chat/completions`.

- Targeted agent/scripted tests (including pinned SDK tests): passed, exit 0.
- Complete agent, scripted and providerproxy suites: passed, exit 0.
- `mise run lint`: passed, exit 0, 0 issues.
- Direct, in-process and real subprocess proxy requests were tested for both
  Generate and Stream, with legacy/explicit Chat and explicit Responses. A
  prefixed upstream base path and stripped wire model were asserted. A local
  fake server was used; no live provider was contacted.
- Tool call/result history round-trip and Responses boundary malformed/oversize
  rejection were verified. A Chat-shaped SSE payload on `/responses` remains
  byte-for-byte unchanged, so the path guard is exercised independently of
  event content.
- Export-path inventories identified 19 superseded phase-1 project archives.
  After confirming no validation consumers remained, their exact paths were
  removed, reclaiming 134,431,922 bytes. Shared dependencies were preserved.
  Post-cleanup shared cache: 27,359,304 KiB; scratch: approximately 1.3 MiB.

## WP 3: admission, terminal safety and usage accounting

Explicit Responses requests now validate supported content and options before
admission or invocation commit. The conservative UTF-8 estimate includes tool
schemas, object-generation tools and Responses options. Explicit credentials
also fence the SDK's environment-key fallback.

- Complete agent, sidecar, scripted and providerproxy race suites: passed,
  exit 0. The relevant team provider/admission/context/profile suite passed,
  exit 0 (52.153 seconds).
- All four Fantasy entry points reject incompatible settings and media with
  zero HTTP calls, commits or reservations. Captured requests cover CJK,
  emoji, long tool schemas and structured output.
- Usage tests distinguish absent, invalid and explicit-zero counters; cached
  input and reasoning output are normalized once. Failed terminal responses
  retain observed usage without quota replay. Cancellation and consumer-stop
  tests verify a single settlement and release.
- The pinned SDK can emit function calls before the terminal event. Responses
  streaming now buffers those calls, with a byte limit, until a completed
  terminal response is verified. Incomplete/failed/EOF fixtures never deliver
  incomplete tool calls. One-byte CRLF and multiline SSE tests cover both
  Stream and StreamObject.
- No real provider endpoint or credentials were used. Live OpenAI verification
  remains outside this implementation's acceptance criteria, pending separate
  user authorization.
- `mise run lint`: passed, 0 issues. Shared cache allocated 28,828,580 KiB;
  task scratch approximately 1.4 MiB. Another checkout concurrently uses the
  shared cache, so total growth is not attributable to this task alone.
- Removed 18 exact superseded phase-2 archives, reclaiming 134,156,298 bytes;
  ownership was checked against the source-root marker, preserving shared dependencies.
