# Duplicate regression tests: issue #28

## Assertion preservation

For [issue #28](https://github.com/kjelly/hufu/issues/28), the current bodies were
parsed with `go/parser` and compared with `go/scanner`, ignoring comments and
whitespace. Each of these five pairs had identical Go tokens before deletion:

| Removed test | Retained test |
| --- | --- |
| `TestTodoToolHandleUpdatePreventErrorToDoneDup` | `TestTodoToolHandleUpdatePreventErrorToDone` |
| `TestScopeMatrix_EmptyAgentIDSeesAgentChild_Query` | `TestScopeMatrixContract_EmptyAgentIDSharedOnly_Query` |
| `TestScopeMatrix_EmptyAgentIDSeesAgentChild_SearchExact` | `TestScopeMatrixContract_EmptyAgentIDSharedOnly_SearchExact` |
| `TestScopeMatrix_EmptyAgentIDSeesAgentChild_SearchLexical` | `TestScopeMatrixContract_EmptyAgentIDSharedOnly_SearchLexical` |
| `TestScopeMatrix_EmptyAgentIDSeesAgentChild_Vector` | `TestScopeMatrixContract_EmptyAgentIDSharedOnly_Vector` |

The retained todo case checks both the error response and that the task remains
in error after an attempted error-to-done transition. The four retained scope
contracts still reject agent-private results for an empty AgentID; Query also
checks shared records remain visible. Every other function body in both files,
including fixtures and helpers, is byte-for-byte unchanged. Only these five
functions and their attached comments were removed. No helper became unused.
The file header now describes the active isolation contract instead of claiming
that the tests skip and the wildcard remains open. A repository-wide search,
including hidden tracked paths, found no old-name references outside these files.

## Controlled timing

Before: `402d97e2b3619ccd84447a247fa9a91349ba548f`.
After: this change on `6ca6f6d668b804ad7ed19e6e4c184b73e2126a85`.
The intervening main commit changes only CLI test helpers and their report;
`internal/context` and `internal/team` are identical between the two bases.

Environment: Go 1.27.2, Linux amd64 6.18.55, AMD EPYC 9V74, five visible CPUs,
cgroup quota 400000/100000, GOMAXPROCS=5, CGO_ENABLED=1,
GOFLAGS=-mod=readonly, GOTOOLCHAIN=local, GOTMPDIR unset.
Shared GOCACHE and GOMODCACHE were preserved. Each mode/tree ran one unmeasured
warm-up, followed by three fresh executions (`-count=1`) using warm build caches.
No lint, builds or other measurements ran concurrently. This is not a cold-build
or result-cache benchmark, nor a full-suite baseline for issue #26.

Run the same command before and after, adding `-race` for the separate race group:

```sh
go test -json -short -count=1 -timeout 20m \
  ./internal/context ./internal/team \
  -run '^(TestScopeMatrix.*|TestTodoToolHandleUpdatePreventErrorToDone.*)$'
```

The filter selects the same fixture families, but the top-level test set
intentionally shrinks from 24 to 19 (context 22 to 18, team 2 to 1). All selected
cases pass without skips. Whole-command wall time uses Python's monotonic clock;
package elapsed times come from Go JSON events and are not summed. Raw JSONL,
observations and comparison scripts remain under ignored `work/issue28/`.

Seconds, median (min–max):

| Mode / tree | Wall observations | Wall | Context package | Team package |
| --- | --- | --- | --- | --- |
| normal / before | 2.463, 2.341, 2.274 | 2.341 (2.274–2.463) | 0.302 (0.297–0.308) | 0.016 (0.016–0.019) |
| normal / after | 2.262, 2.399, 2.154 | 2.262 (2.154–2.399) | 0.247 (0.246–0.275) | 0.019 (0.019–0.020) |
| race / before | 9.324, 9.184, 9.215 | 9.215 (9.184–9.324) | 8.410 (8.202–8.414) | 1.067 (1.064–1.069) |
| race / after | 7.865, 7.954, 7.865 | 7.865 (7.865–7.954) | 6.987 (6.935–7.139) | 1.062 (1.056–1.089) |

Normal wall time has overlapping ranges: the small observed reduction is not
strong evidence of an end-to-end speedup. Race wall time is lower for this local
fixture set, but it is not a prediction of whole-suite or CI acceleration.
The todo case itself remains too small to show a useful timing benefit.

## Validation

The focused normal and race commands above pass on every before/after execution,
including all five retained contract tests. Broader checks use fresh executions:

```sh
go test -json -short -count=1 -timeout 20m ./internal/context ./internal/team
go test -json -race -short -count=1 -timeout 20m ./internal/context ./internal/team
golangci-lint run
bin/check-docs
git diff --check
```

The full normal short run passes `internal/context` (8.486s), but
`internal/team` fails (176.135s) with five descendant-cleanup failures:

- `TestProcessSupervisorKillsDescendants`
- `TestTerminalSession_ProcessGroupKillDescendants`
- `TestPTYCloseWaitsForFinalizationAndPublishesClosedOnlyAfterRelease`
- `TestPTYExitCleansDescendantHoldingSlaveBeforeRelease`
- `TestTerminalPipeNaturalExitContainsDescendantsBeforeReap`

All five also fail on an isolated `git archive` snapshot of the unchanged
`6ca6f6d6` base using the same Go environment, `-short -count=1 -timeout 5m`,
and an exact-name alternation selecting these five tests. This reproduces the
pre-existing failures; it does not establish their root cause. They overlap the
historical [issue #26 capture failures](test-baseline-2026-10-22.md).
No assertion, timeout, skip or production code was changed to hide them.
The full normal run has 8,861 pass, 14 skip and 5 fail test events, including
parents/subtests; these are not independent top-level case counts.

The full race run passes `internal/context` (174.238s). `internal/team` reports
the same five descendant-cleanup failures, then reaches the **whole-package
20-minute timeout** (1200.093s) while `TestWP10_SystemicCount_LiveMatchesReplay`
is running. Its terminal test events include 8,832 pass, 14 skip and 5 fail;
the remaining cases are not established as passed. There is no
`WARNING: DATA RACE` in the captured output, but this incomplete run is **not**
a passing full-package race check. The focused before/after race checks above
remain passing evidence for the changed contracts. The timeout itself was not
reproduced on the base; no claim is made about its cause or a full-suite speedup.

`golangci-lint run` passes with **0 issues**. `bin/check-docs` and
`git diff --check` also pass. Required full-suite correctness gaps remain
explicitly recorded rather than hidden by a skip, retry or weakened assertion.
