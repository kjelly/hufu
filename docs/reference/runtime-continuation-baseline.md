# Runtime continuation implementation baseline

> Status: active
> Authority: reference
> Verified-Commit: 825ff686
> Supersedes: —
> Superseded-By: —

## Scope and isolation

On 2026-10-28, the committed `825ff686` tree was extracted with `git archive`
into the task's private temporary directory. This excluded the checkout's
unrelated local changes. All 3,263 tracked blobs were checked against the Git
blob hashes after the full test run; none changed. Real providers and user
databases were not used.

This supplements the earlier `4e13abfb` baseline recorded in the
[session continuation plan](../implementation-plans/session-continuation-and-derived-forks.md#12-wp0-實作紀錄).
The dependency projection work uses the same `825ff686` baseline.

## Full baseline result

From the repository root, with the configured Go caches and `GOTMPDIR` preserved:

```bash
TMPDIR=<task> GOMAXPROCS=2 mise exec -- go -C <task>/baseline-current test -p 2 ./... -timeout 30m
```

| Result | Evidence |
|---|---|
| Overall | Exit 1 |
| `cmd/hufu` | Failed after 173.035 seconds; only failing test was `TestResumeCommandActivatesVersionedBranchBeforeProvider` |
| Failure | `public resume: calls=0 err=<nil>` |
| `internal/team` | Passed in 629.654 seconds |
| `internal/context` | Passed in 76.045 seconds |
| Other packages | Passed or had no test files |

The tests used local fixture listeners, requiring execution outside the network
sandbox. This did not grant or perform real provider calls.

## Confirmed fixture defect

CLI tests reuse package-global Cobra subcommands. A preceding `resume --help`
leaves that command's lazily created help flag true. A later invocation can
return success after printing help, without running resume or its provider.

A new behavior test, `TestCLIInvocationsDoNotRetainHelp`, was added as an
untracked test harness to the baseline snapshot after its full run and tracked
blob verification. Against the unchanged baseline helper it failed with
`a prior help invocation suppressed resume validation` (exit 1, package time
0.045 seconds). This separately reproduces the shared-fixture defect.

The correction resets reused help flags in the CLI invocation fixture. The
working-tree targeted run passed both this regression and the original
versioned-resume integration test (`cmd/hufu`, 3.075 seconds). The subsequent
full working-tree suite also passed: `cmd/hufu` 158.332s, `internal/context`
72.495s, `internal/team` 656.382s, and all remaining packages passed or had no
tests. A failing immutable baseline remains a failing baseline; working-tree
results are separate evidence and include the preserved unrelated local edits.
