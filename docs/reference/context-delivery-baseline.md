# Context delivery baseline

> Status: active
> Authority: reference
> Verified-Commit: `7312757e0187d471f16347656e887207b9db96ef`
> Supersedes: —
> Superseded-By: —

This record captures the code baseline before the [context delivery plan](../implementation-plans/context-delivery.md). The documentation-only commit `f3ca1ba6` contains that plan; its Go sources are identical to the reviewed baseline.

Environment: Go 1.27.2, Linux/amd64. The integration checks use local fake providers and ephemeral workspaces, with local socket access enabled; no live model was used.

| Check | Result |
|---|---|
| `go test ./...` | Initial run failed in 6 terminal-broker tests because Unix socket paths exceeded the OS limit |
| `TMPDIR=/tmp GOTMPDIR=/tmp go test ./...` | Passed, exit 0, against the immutable `git archive f3ca1ba6` snapshot |
| `go vet ./...` | Passed, exit 0 |
| `golangci-lint run` | Passed, exit 0; 0 issues |
| `go run ./cmd/hufu eval run evals/context-routing --format json` | Passed, 1/1 cases, exit 0 |
| `go run ./cmd/hufu eval run evals/memory-learning --format json` | Passed, 3/3 cases, exit 0 |
| `go run ./cmd/hufu eval run evals/retry-recovery --format json` | Passed, 2/2 cases, exit 0 |
| `bin/check-docs` | Passed, exit 0 |

The context-routing suite currently checks CJK syntax safety, not CJK semantic recall or actual context delivery. PR-00 adds explicit provider-visible assertions and authored relevance oracles; passing this baseline does not establish the proposed query policy's benefit.

The environment initially set `GOTMPDIR` to a long cache path. Setting only `TMPDIR` did not change the Go test temporary directory. Setting both variables to `/tmp` passed the six original failing tests (2.229s) and then the complete suite (`cmd/hufu`: 141.456s; `internal/team`: 241.493s). No test was skipped or weakened to obtain this result. The obsolete retry with only `TMPDIR` was cancelled after the cause was verified.
