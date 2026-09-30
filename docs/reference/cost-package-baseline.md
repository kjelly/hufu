# Cost governance and team-package baseline

> Status: active
> Authority: reference
> Verified-Commit: `bd42062e144750dca0d411e63bbe492524a14c5a`
> Supersedes: —
> Superseded-By: —

This note records the CP-000 characterization baseline for
`docs/architecture/cost-governance-and-portable-team-packages.md`. It describes
the runtime at the verified commit; later phases must update their own tests
when they intentionally change one of these boundaries.

## Environment

```text
Go: go1.26.6 linux/amd64
OS: Linux 7.0.0-31-generic x86_64 GNU/Linux
```

All baseline tests use fakes or local temporary files. They require no live
provider, external service, package registry, or Paperclip checkout.

## Characterization matrix

| Boundary | Existing or extended evidence |
| --- | --- |
| `ExecutionUsage` JSON, including cache read/write | `TestUsageFromStepsSumsCacheTokens`, `TestExecutionUsageJSONOmitsZeroCacheFields`, `TestExecutionUsagePromptTokens` |
| Concurrent token reservations | `TestTokenBudgetConcurrentStepReservationsBoundOvershoot` |
| Token-budget ownership across extra-model coordinators | `TestTokenBudgetSharedRootAdmissionAcrossExtraModels` |
| Admission and durable commit precede all four Fantasy transports | `TestBoundAdmissionContextCoversAllLanguageModelMethods` (extended in CP-000 with an ordered trace) |
| Stream slot release and cancellation | `TestAdmittedStreamHoldsProviderSlotUntilExhaustion`, `TestAdmittedStreamObjectHoldsProviderSlotUntilExhaustion`, `TestAdmittedStreamCancellationReleasesUniteratedSlot`, `TestAdmittedStreamEarlyBreakCancelsBeforeRelease` |
| EventStore interprocess append and idempotency | `TestEventStoreAppendSeesAnotherWritersIdempotencyKey`, `TestEventStoreAppendMergesAnotherWritersTail`, `TestEventStoreIdempotencyIsScopedToBranch` |
| EventStore corrupt tail, replacement, write and sync failures | `TestCoordinatorStartupMarksCorruptEventStoreForRecovery`, `TestEventStoreAppendRefusesReplacedLog`, `TestEventStoreAppendFailsSafelyWhenSyncFunctionIsAbsent`, `TestEventStoreSyncFailureIsObservable` |
| Execution-policy v5 compatibility and drift | `TestExecutionPolicySnapshotV5OmitsLegacyProviderAndReadsV4AndV3`, `TestExecutionPolicySnapshotBlocksResumeOnPersistedConfigurationDrift`, `TestOMPCharacterizePolicySnapshotGolden` |
| Read-only event inspection | `TestOpenEventStoreReadOnlyDoesNotCreateAndRejectsAppend`, `TestInspectStorageMissingDatabaseUsesIntegrityExitAndCreatesNothing`, `TestInspectCommandRunJSON` |
| Legacy and v1alpha1 normalization | `TestLegacyAndV1Alpha1NormalizeIdentically` |
| Top-level agent Markdown | `TestLoadTeam_NoYAMLDirName`, `TestLoadTeamExcludesREADMEFromAgentDiscovery` |
| Team-owned and external skill boundaries | `TestTeamLintSkillDependencyUsesRuntimeExpansion`, `TestLoadTeam_DiscoversProjectSkillsNotTeamAgentSkills` |
| Result-contract schemas | `TestLoadTeamBindsResultContracts`, `TestCompileResultContractSchemaRejectsUnsafeSchemas` |
| Trusted-static Go action source | `TestActionCatalogProviderIdentityPinsGoSourceButNotCommandScripts` |

## Generation call paths

The shared Fantasy admission wrapper covers coordinator generation, Hufu-local
workers, direct agents, extra-model Hufu-local leaves, sidecar roles, and
result-only protocol repair. `internal/team/testdata/model_call_chokepoints.txt`
is the existing audited inventory for those constructors and accessors.

The wrapper performs this order for each of `Generate`, `Stream`,
`GenerateObject`, and `StreamObject`:

```text
AdmitProviderRequest
    -> acquire provider slot
    -> CommitProviderInvocation
    -> provider transport
```

`AgentExecutionBackend.RunAttempt` is the characterized bypass. It delegates
to an external `SubagentProvider`, such as Codex app-server, and therefore does
not pass through the Fantasy model wrapper. CP-105 must integrate its cost
boundary before preparing an execution world or starting a child process.

## Package ownership boundary

An agent-team package may own only these structurally discoverable resources:

- exactly one team manifest;
- top-level agent Markdown and optional `README.md`;
- team-local `skills/<name>/` trees rooted by a valid `SKILL.md`;
- team-relative result-contract schemas;
- non-test Go source used by trusted-static Go action providers.

The following remain external requirements and are not package-owned:

- project and global skills;
- subject-project `required-resources`;
- command-provider executables and working directories;
- MCP server binaries;
- provider configuration, credentials, workspace state, logs, and context
  databases.

The source and staged package must compile to the same normalized `TeamConfig`.
This is a compatibility assertion, not permission to follow arbitrary prose or
filesystem references.

## Compatibility assumptions

- Cost fields and events are additive and absent when unused.
- Existing v4 execution-policy snapshots remain readable after the writer
  version changes.
- `ExecutionReceipt.Usage` remains task-attempt evidence; it is not the cost
  ledger.
- EventStore branch scoping and hash-chain verification remain canonical.
- Package metadata does not replace either supported team manifest schema.
- Package integrity hashes do not establish publisher authenticity.
