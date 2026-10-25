# Test layers

> Status: active
> Authority: reference
> Verified-Commit: `300041b84e45e7c53549bd53c753b6b0ac6ad860` (implementation base)
> Supersedes: undocumented short-mode membership
> Superseded-By: —

The entry points below implement [issue #32](https://github.com/kjelly/hufu/issues/32).
Run them from the repository root with the pinned mise toolchain. The package
universe includes `./...` and `./.agent-teams/hufu-dev/diagnostic-pilot/...`.
Every active top-level `Test` function is in short or integration; existing opt-in and
platform restrictions still apply. This is an incremental classification:
short still includes unreviewed persistence/process tests. It is not yet a
promise that every short test is a pure unit test.

| Entry | Local command | Required CI coverage |
| --- | --- | --- |
| Short, including pure unit tests | `mise run test-short` | Four existing race package groups: cli, team, improver, other |
| Reviewed process/PTY, persistence, architecture/generator cases | `mise run test-integration` | Separate `integration` job, with race and no short flag |
| Complete offline regression | `mise exec -- go run ./scripts/testlayers -layer full` | Unchanged `cgo-free` job executes all tests with `CGO_ENABLED=0` and no short flag |
| Active test inventory | `mise run test-inventory` | Every short/integration invocation validates the same discovered sources |
| Live Codex smoke | `HUFU_CODEX_SMOKE=1 mise exec -- go test -p 2 -count=1 ./internal/team -run CodexSmoke` | Explicit operator opt-in, outside deterministic CI; requires installed/authenticated Codex |

`mise run test` retains its existing full core-suite behavior. The new full
entry additionally includes diagnostic-pilot. Live smoke keeps its existing
opt-in guard; moving it cannot accelerate default CI because it was already
skipped there. All entry points retain normal Go build tags. Example tests and
fuzz seed corpora remain in the unfiltered short/full package executions. The
inventory lists top-level `Test` functions; it does not filter examples or fuzz
seeds out of execution. The inventory shows the tests active for the current GOOS/GOARCH, including external test
packages, rather than inventing coverage for inactive platform files.

## Membership and coverage

Use a standard, **first-statement, whole-test** guard for a reviewed integration
case, with no setup preceding it:

```go
func TestProcessContract(t *testing.T) {
    if testing.Short() {
        t.Skip("integration: mise run test-integration")
    }
    // The complete original fixture and assertions follow.
}
```

The runner uses `go list -json` and Go's AST parser. Every canonical short guard
automatically contributes the test to its package's exact integration filter;
there is no separate name manifest to forget. A helper-level, inverted, late,
or nested short guard fails discovery. This keeps each canonical guard coupled to
its replacement race CI execution. Tests without a short guard belong to short by
default, including tests in newly added packages. No existing assertions are
removed by this classification.

The integration runner uses an exact anchored union of test names and no
`-short`; subtests execute normally. Packages share one Go invocation with the
existing `-p 2` budget when their names cannot select any short test in another
participating package. If names collide, it falls back to per-package exact
filters. This avoids a second full unit execution, repeated Go invocations in
the common case, and unbounded database concurrency.
Both offline runners use `-p 2`, `-count=1`, and a default 20-minute package
timeout. The team and improver short CI groups override this to 30 minutes, with
40-minute job limits including setup, compilation and evidence upload. The cli
and other groups retain 20-minute package deadlines with 30-minute job limits; integration retains
20-minute package deadlines with a 40-minute job limit across its packages.
Additional Go test flags follow `--` (the final `-timeout` value wins), for example:

```bash
mise exec -- go run ./scripts/testlayers -layer integration -- -race
mise exec -- go run ./scripts/testlayers -layer short -scope team -- -race -timeout 30m
mise exec -- go run ./scripts/testlayers -list > work/test-inventory.json
```

The four short package scopes preserve the existing partition: `cmd/hufu`,
`internal/team`, `internal/improver`, and all other packages. This change adds
no top-level test shards. `ci-success` requires both all four short groups and
the integration job, as well as lint, vet, build, docs, cgo-free and eval-core.
Race is preserved across both layers. CGO-free still executes the original
full release acceptance suite; compile-only checks do not replace it.

## First migration: all 31 moved top-level cases

| Source | Cases now guarded by short |
| --- | --- |
| `cmd/hufu/pty_e2e_test.go` | `TestE2E_VersionOutput`, `TestE2E_HelpOutput`, `TestE2E_DoctorCommand`, `TestE2E_ListCommand`, `TestE2E_TUILifecycle`, `TestE2E_NonInteractiveVersion`, `TestE2E_IsolatedEnvironment` |
| `cmd/hufu/chat_ui_e2e_test.go` | `TestE2E_ChatUI_TurnsCancelAndExit` |
| `cmd/hufu/improve_probe_pty_test.go` | `TestProbeBinaryTUICompletionAndTerminalRestore`, `TestProbeBinaryTUICancelDrainsBeforeExit` |
| `cmd/hufu/results_picker_pty_test.go` | `TestResultsPickerBinaryPTYSelectionCancelAndRestore` |
| `cmd/hufu/results_presentation_pty_test.go` | `TestResultsPresentationBinaryTTYAndPipe` |
| `internal/team/terminal_broker_test.go` | `TestTerminalBrokerAttachStreamsInputAndOutput`, `TestTerminalBrokerRejectsConcurrentAttach`, `TestTerminalAttachmentClientTransfersInputAndDetaches`, `TestTerminalBrokerTransferRequiresExplicitOperatorHandoff`, `TestTerminalBrokerDisconnectKeepsTaskPaused` |
| `internal/team/terminal_cleanup_test.go` | `TestTerminalCleanupLegacyDefaultsAndOwnershipGuard`, `TestTerminalCleanupGracefulCustodyTransferAndLeaseRevocation`, `TestTerminalCleanupEscalatesAndIsIdempotent`, `TestTerminalCleanupForcedStatePersistenceFailureStopsEscalation`, `TestTerminalCleanupCustodyBlocksOwnerAndConcurrentCleanup`, `TestTerminalCleanupActiveRoundAndIdentityMismatchAreFailClosed` |
| `internal/context/migrations_test.go` | `TestMigrateIsIdempotentAcrossReopen`, `TestOpenSQLiteRejectsTamperedMigrationChecksum`, `TestMigrateBacksUpExistingStoreBeforeApplyingNewMigration`, `TestMigrateSkipsBackupForBrandNewStore` |
| `internal/decisionrt/architecture_test.go` | `TestCoreDependencyBoundary` |
| `internal/decisionrt/backend/systemone/architecture_test.go` | `TestDependencyBoundary` |
| `internal/decisionrt/control/control_test.go` | `TestControlDependencyBoundary` |
| `internal/modelcatalog/catalog_test.go` | `TestGeneratorReproducesCommittedEmbeddedSnapshot` |

The broker errno predicate stays in short. Signal/terminal restore, pipe versus
PTY transport, custody/lease transfer, SQLite checksum/backup/reopen, dependency
boundaries and generator reproducibility retain their original assertions.
Other process, SQLite, crash/resume and locking cases remain in short until
reviewed; no broad file-name heuristic reclassifies them.

## Team timeout follow-up: six reviewed cases (issue #42)

These six cases were the >=10-second tail in the main `short-team` artifact
on `b3360c9a`. Each now has the whole-test guard above; the required integration
job discovers and executes it under race without `-short`. All original fixtures,
iteration counts and assertions remain intact. Full CGO-free execution also
continues to include them.

| Source | Case moved to integration | Fixture/contract retained |
| --- | --- | --- |
| `internal/team/hufu_dev_workflow_integration_test.go` | `TestHufuDevWorkflowStructuredHandoffAndArtifacts` | Three-worker handoff, real shell output/artifact offload, verification, acceptance and durable replay across six outcomes |
| `internal/team/memory_explain_test.go` | `TestExplainPersistentMemoryMatchesRuntimeSelection` | SQLite-backed adopted policy and runtime/explain ranking parity across four learning modes and five queries |
| `internal/team/session_data_race_test.go` | `TestSessionDataConcurrentCheckpointNoRace` | Four concurrent writers, manifest/checkpoint persistence and final coherent session state; still executed with the race detector |
| `internal/team/resource_order_test.go` | `TestSerializedMutationsDoNotInheritFailure` | Coordinator scheduler, serialized workspace writes, failure/dependency propagation and event-journal replay |
| `internal/team/compaction_state_test.go` | `TestRealPredecessorCompactionsUseIncrementalLineageValidation` | 64 persisted predecessor generations and one lineage range query per generation |
| `internal/team/team_action_dispatch_test.go` | `TestCompileCatalogActionTasksRejections` | Full Coordinator/event-store fixtures for 14 dispatch rejection scenarios; no provider may execute |

This brings integration membership to 37 cases on this implementation base.
The migration moves work between jobs; it does not claim to reduce total CPU.
Small pure parsing, scheduler predicates and rejection tests remain in short.
See the [timeout follow-up evidence](../archive/analyses/team-race-timeout-2026-10-24.md)
for validation, cache changes and the outstanding main-push stability check.

## First parallel batch

Only the seven top-level tests in `internal/team/prompt_test.go`, four in
`decision_diversity_test.go`, and three in `decision_explain_test.go` gain
`t.Parallel()`. Their fixtures are local maps/slices/structs, and the functions
parse mentions, rank bindings, count diversity, or format explanation records.
The synthetic registries never discover/load a team. They read the working
directory only when resolving their synthetic paths and do not change it.
These tests do not mutate environment, global opts, audit/logger configuration,
secret registries, or shared external processes. No Coordinator or database
fixture is newly parallelized. Existing subtest assertions stay intact.

Validate this batch in the complete team suite under race, plus repeated
shuffled focused runs, before expanding it. A passing focused run alone does
not establish isolation from other parallel tests in the package.

## Measurement and CI cost

Use the [baseline procedure](test-performance-baseline.md) for equal-runner,
equal-flag, three-sample comparisons. After layering, `-short` intentionally
selects a different execution set: compare `short + integration` coverage as
well as the full suite, not just the shorter job. See the
[initial migration report](../archive/analyses/test-layers-2026-10-24.md).

The aggregate `ci-success` gate uses read-only Actions access to report observed
workflow wall time and total runner seconds across **all** attempts, with
pagination and duplicate job-ID protection. It includes queued/rerun gaps in
workflow wall time and reports the current aggregate job only through the
observation time; its later cleanup is not yet measurable. The tooling unit
tests run in the required other short group.

Each race short group and integration job uploads raw test JSONL, command wall
and child user/system seconds, and Go build-cache size before/after testing.
Artifacts last 14 days. The additional integration runner restores its own Go
cache and compiles/links its selected test packages, even when those packages
also run in short. Cache size is a storage observation, not a cache-hit ratio,
transfer duration, or CPU saving. setup-go logs supply cache restore/save timing.

Each race job also publishes `package-budget.json` and a summary table of terminal
Go package `Elapsed` divided by that job's actual package timeout. Test/subtest
elapsed values and command wall time are not added into this ratio. Ratios above
75% emit a workflow warning; missing terminal results or truncated JSONL are
reported as unavailable/incomplete, including failed discovery. A warning does
not replace the existing test exit status or mask failures.

CI pins setup-go v5.6.0, which bundles the cache v4 client instead of v5.0.0's
cache v3 client. Each short race scope and integration uses a distinct `GOCACHE`
path: cache archive paths participate in the cache version, preventing a normal
build job or another scope from populating the same immutable race cache.
Module-cache paths stay unchanged. The summary/artifact records setup-go's
`cache-hit`; a hit does not prove every compilation action was cached. The first
run with the new client/paths may miss, and transfer results must be checked in
the logs before claiming the historical HTTP 400 problem is resolved.

For a completed CI comparison report both critical path (workflow wall time,
including setup/queue where relevant) and total runner seconds (sum of all job
start-to-finish durations), plus cache restore/save times and sizes. A shorter
short job does not establish a shorter workflow or lower total compute. The
additional runner may increase total cost; no CPU reduction is assumed. If team
remains the critical path, profile its remaining fixtures before considering
package extraction or test shards.
