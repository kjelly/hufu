# Team authoring baseline and staged validation

> Status: active
> Authority: reference
> Verified-Commit: `84ad9b6a77a865de7cf7b3f59135e5a4a95383fa`
> Supersedes: —
> Superseded-By: —

This record accompanies the [team YAML implementation plan](../implementation-plans/team-yaml-simplification.md).
The baseline has no implementation edits: `84ad9b6a` adds only the plan and its documentation index entry.

## Baseline (2026-10-20)

Environment: Linux, Go `go1.27.2`; use a short temporary directory (`TMPDIR=/tmp/hy GOTMPDIR=/tmp/hy`)
for the repository's Unix socket tests. No live model or external account is required.

| Command | Result |
|---|---|
| `go build -o /tmp/hufu-yaml-baseline ./cmd/hufu` | PASS |
| `TMPDIR=/tmp/hy go test ./...` (restricted sandbox) | FAIL: local fixture socket creation denied (`operation not permitted`) |
| `TMPDIR=/tmp/hy GOTMPDIR=/tmp/hy go test ./...` (local fixture sockets permitted) | PASS, exit 0 (`cmd/hufu`: 123.074s; `internal/team`: 259.495s) |
| `go vet ./...` | PASS |
| `golangci-lint run` | PASS, exit 0, `0 issues`; cache persistence warnings in restricted sandbox |
| `GOLANGCI_LINT_CACHE=/tmp/hufu-yaml-lint-cache golangci-lint run` | PASS, exit 0, `0 issues`, no cache warnings |
| `bin/check-docs` | PASS (76 active files, 156 total files, 431 relative links, 37 Go function anchors) |

A fresh compiled CLI confirms the PR-0 regression with a temporary worker-only
team (`worker.md` uses `preset: readonly`): `team validate` exits 0, while
`team lint --format json` exits 1, reports `complete: false` and one
`missing_coordinator` finding with severity `error`. The runtime has a
coordinator fallback; no model was called for this reproduction.

The restricted-sandbox failures are environmental: the failing tests attempted to
open loopback HTTP fixture listeners. The retry permits those listeners and sets
both temporary-directory variables. Neither tests nor runtime code were changed
to work around the sandbox. A concurrent lint retry was rejected by the linter
runner lock; it was rerun sequentially after the original lint finished.

The same baseline CLI also confirms a separate, preserved lint policy:
`team create ops --preset safe-ops` succeeds, but lint reports
`side_effect_retry_without_reconcile` for the operator's external writes. PR-0
only changes the coordinator finding; it does not suppress this safety error.
Compact/expanded preset parity tests must compare that existing behavior rather
than require every preset to pass all lint policies.

## Stage records

Stage results are added only after the commands finish and the corresponding
implementation gates pass. Baseline failures, if any, must remain visible here
and must not be described as newly introduced failures.

### PR-0 — Coordinator fallback lint

The stage changes only the missing-authored-coordinator finding to `info` and
keeps inspection complete. It preserves runtime role/name matching, built-in
fallback, multiple-coordinator errors, other lint policies, JSON v1 and ignore
semantics. README English/Chinese, the agent reference, and CLI help now describe
optional manifests/frontmatter and filename inference accurately.

Regression coverage includes worker-only and legacy-name selection, compiled
Team Preset scaffolds, CLI validate/lint process exit codes, error/warning/info
thresholds, `--ignore`, and complete JSON output. `safe-ops` retains its existing
external-write recovery error.

| Check | Result |
|---|---|
| `go build -o /tmp/hufu-yaml-pr0 ./cmd/hufu` | PASS |
| Targeted coordinator/preset/CLI process regressions | PASS |
| `TMPDIR=/tmp/hy GOTMPDIR=/tmp/hy go test ./internal/team/... ./cmd/hufu/...` | PASS (`internal/team`: 198.732s; `cmd/hufu`: 86.169s) |
| `TMPDIR=/tmp/hy GOTMPDIR=/tmp/hy go test ./...` | PASS, exit 0 (`internal/team`: 198.155s; unchanged packages reused valid cached results) |
| `go vet ./...` | PASS |
| `GOLANGCI_LINT_CACHE=/tmp/hufu-yaml-lint-cache golangci-lint run` | PASS, `0 issues` |
| `bin/check-docs` and `git diff --check` | PASS |

PR-0 was committed as `a56bbf57`. Later stage results follow below.

### PR-1 — Single-file Team Presets

Parent implementation baseline: `a56bbf57`. Team-level `preset` is now accepted
in flat and v1alpha1 manifests and compiled in memory using the same Agent
normalizer as Markdown. Same-name or alias collisions fail closed; distinct
Markdown members, contracts, delegation and existing model/tool policy work
through the shared loader. Provenance is process-local and excluded from policy
hashes. CLI list/show/explain/add and migration understand the new source.

`team create --single-file` creates only team.yaml, defaults to coding-single,
and refuses existing directories and the declared incompatible flags (including
explicit false/empty flag values). Exclusive creation and non-recursive,
identity-checked cleanup protect existing or concurrently replaced files.
Legacy create output and runtime authorization are unchanged. README English /
Chinese, references and the Team Builder skill document the supported scope.

Evidence: `TestTeamCompositionPresetParity` compares every AgentDef field, the
compatibility projection, roster variables, coordinator selection and lint
codes/severities across all four presets and both schema shapes. The
fake-provider test executes a compiled worker through RunDirectAgent with a
typed submission and verifies policy hash equivalence. Other composition tests
cover strict types, unknown presets, all collision forms, discovery, prompt
splitting, contract/delegation binding, no generated Markdown and raw-template
migration. `TestTeamSingleFile*` covers the real CLI journey, output formats,
invalid flag combinations, JSON failure without partial output,
rollback on write/validation failures, concurrent creation, symlinks and foreign
directory replacement. Existing bundled teams still list successfully.

| Check | Result |
|---|---|
| `go build -o /tmp/hufu-yaml-pr1 ./cmd/hufu` | PASS |
| Focused composition and CLI regressions (`-count=1`) | PASS (team: 1.303s; CLI: 9.294s) |
| `TMPDIR=/tmp/hy GOTMPDIR=/tmp/hy go test ./internal/team/... ./cmd/hufu/...` | PASS (team: 262.039s; CLI: 128.084s) |
| `TMPDIR=/tmp/hy GOTMPDIR=/tmp/hy go test ./...` | PASS (team: 214.194s; evalharness: 61.602s; other valid cached results reused where applicable) |
| `go vet ./...` | PASS |
| `GOLANGCI_LINT_CACHE=/tmp/hufu-yaml-lint-cache golangci-lint run` | PASS, `0 issues` after removing one leading blank line |
| `bin/check-docs` and `git diff --check` | PASS |

Migration deliberately retains the existing nil/empty normalization for task
slices. Its regression checks the complete serialized contracts and compiled
contract identity with the existing migration verifier, alongside full AgentDef
equality. No runtime contract was weakened for this test. The safe-ops recovery
lint error remains identical across compact and expanded forms.

PR-2 (inline workers and distinct-name three-source composition) remains outstanding.
