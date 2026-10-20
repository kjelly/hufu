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

## Stage records

Stage results are added only after the commands finish and the corresponding
implementation gates pass. Baseline failures, if any, must remain visible here
and must not be described as newly introduced failures.
