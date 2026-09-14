# Operator experience release readiness

> Status: active
> Authority: reference
> Verified-Commit: 2026-09-14
> Supersedes: —
> Superseded-By: —

This report records the Phase 7 release evidence for the operator-experience
preview. It deliberately separates engineering evidence from human usability
evidence. The preview is not eligible for a stable label until the human gate
in [Operator usability evaluation](operator-usability-evaluation.md) passes.

## Release decision

| Gate | State | Evidence |
|---|---|---|
| Safety and compatibility automation | passed on the validation host | full Go tests, vet, lint, docs checks, deterministic operator contracts |
| Warm overview target | passed on the validation host | 26-event p95 1.55 ms; 1001-event p95 51.19 ms, 30 warm samples each |
| Supported-platform build matrix | partial | native Linux amd64 plus CGO-free Linux arm64 and Darwin cross-builds |
| Native terminal matrix | partial | Bash, Fish, and Nushell parse smoke only |
| Human task test | **not run** | 0 participants; no baseline/candidate comparison |
| Overall release status | **preview** | stable promotion is blocked by the human and native-platform gaps |

`passed` above means the named deterministic gate passed; it does not imply
that the whole Phase 7 stable-release gate passed.

## Validation environment and performance

- Date: 2026-09-14.
- Host: Linux `7.0.0-30-generic`, amd64, Intel i7-6700K, 6 vCPUs.
- Toolchain: Go 1.26.6, `CGO_ENABLED=1` for native tests.
- Harness: `HUFU_OPERATOR_PERF=1 go test ./internal/inspect -run TestOperatorOverviewPerformance -count=1 -v`.
- Corpus: one `run_started` event and either 25 or 1000 `task_created`
  events. Each warm result is p95 from 30 queries against a temporary event
  store; cold latency is reported separately.

| Corpus | Cold | Warm p95 | Target |
|---|---:|---:|---:|
| 26 events | 2.54 ms | 1.55 ms | <=250 ms |
| 1001 events | 47.81 ms | 51.19 ms | <=250 ms |

These measurements describe this host and corpus only. The harness is opt-in
and does not encode machine-dependent latency as a portable unit-test claim.

## Platform and terminal matrix

| Target/surface | Evidence | Acceptance state |
|---|---|---|
| Linux amd64 | native tests, vet, lint, docs, performance; Bash/Fish/Nushell parse smoke | engineering accepted; human usability pending |
| Linux arm64 | `CGO_ENABLED=0` cross-build | build-only; native TUI/PTY/manual smoke unverified |
| Darwin amd64 | `CGO_ENABLED=0` cross-build | build-only; native terminal smoke unverified |
| Darwin arm64 | `CGO_ENABLED=0` cross-build | build-only; native terminal smoke unverified |
| Windows | not a `.goreleaser.yml` target | unsupported; PowerShell argv rendering tests are not a Windows binary claim |
| Bash completion | generated script accepted by `bash -n` | native parse accepted |
| Fish completion | generated script accepted by `fish -n` | native parse accepted |
| Nushell completion | generated static declarations sourced by `nu --no-config-file` | native parse accepted; selector-aware dynamic IDs are not shipped |
| Zsh completion | Cobra generator and deterministic Go tests only | native parser unavailable; unverified |
| PowerShell completion | Cobra generator and deterministic Go tests only | native shell unavailable; unverified |

## Requirement and regression traceability

| Requirement group | Deterministic evidence | Delivery state |
|---|---|---|
| HF-UX-001–013 / UX-T01–10, 16–19, 45, 49 | Phase 0 goldens; scope, read-only inspector, snapshot and malformed-store tests | shipped in preview |
| HF-UX-020–023 / UX-T20–28 | deterministic action registry, typed argv, freshness, policy and sanitization tests | shipped in preview |
| HF-UX-030–034 / UX-T04–15, 29–30, 48, 51 | command-factory, legacy workspace/JSON/exit, model-role, facade equivalence and team-check tests | shipped in preview |
| HF-UX-039–044 / UX-T17–18, 21, 26–30, 39–46, 50 | TUI summary/theme/epaper/layout/navigation/PTY unit, integration and focused race tests | preview; native Darwin and arm64 smoke pending |
| HF-UX-050–055 / UX-T28, 31–39 | read-only context/learning queries and explicit promotion review/apply tests | shipped in preview |
| HF-UX-060–063 / UX-T01, 38, 47–48 | wizard safety, bounded scoped completion, generated reference and journey parse tests | shipped in preview; dynamic Nushell IDs unshipped |
| HF-UX-070 | six-person baseline/candidate report | **unshipped; 0 participants** |
| HF-UX-071 | this performance/platform report and cross-build/shell evidence | partial; unverified platforms are explicitly excluded |
| HF-UX-072 / UX-T52 | `--no-summary` contract plus documented legacy/plain/theme fallback | shipped in preview |
| HF-UX-073 | this source/evidence map and final repository gates | engineering portion complete; stable gate pending HF-UX-070 |

The authoritative test names and commands remain in the Phase gate entries of
the architecture document and in the repository tests. Unknown or untested
states above are not interpreted as successes.

## Remaining stable-release work

1. Run the baseline and candidate corpus with at least three new and three
   existing Hufu users; retain failed scenarios and aggregate the exact gates.
2. Repeat native TUI/PTY smoke on every platform that release notes intend to
   call stable, including at least one Darwin terminal.
3. Exercise an actual low-refresh display or record an equivalent controlled
   low-refresh terminal session before claiming e-paper comfort.
4. Promote only the surfaces whose evidence passes; leave all others preview
   or explicitly unsupported.
