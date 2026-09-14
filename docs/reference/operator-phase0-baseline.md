# Operator experience Phase 0 baseline

> Status: active
> Authority: reference
> Verified-Commit: `3affc92`
> Supersedes: —
> Superseded-By: —

This record freezes the CLI contracts that the operator-experience work must
preserve. The machine-readable inventory and journey inputs live under
`cmd/hufu/testdata/operator/`; tests load them directly.

## Contract evidence

| Requirement | Evidence |
|---|---|
| Legacy root workspace remains base-plus-team | `TestOperatorPhase0RootWorkspaceRemainsLegacyBase` |
| Resume/retry/reconcile exact and inferred workspace matrix | `TestOperatorPhase0WorkspaceContract` |
| Root JSON v1 | `root_completed.golden.json`, `TestOperatorPhase0RootJSONGolden` |
| Inspector JSON v1 | `inspect_run_partial.golden.json`, `TestOperatorPhase0InspectJSONGolden` |
| Team lint JSON v1 | `teamlint_empty.golden.json`, `TestTeamLintJSONStable` |
| Process exit behavior | `TestCLIProcessExitContract`, `TestTeamLintProcessExitContract`, inspector exit tests |
| Help and completion have no runtime side effects | `TestOperatorPhase0HelpAndCompletionCreateNothing` |
| Missing inspector storage is not created | `TestInspectStorageMissingDatabaseUsesIntegrityExitAndCreatesNothing` |
| Deterministic operator journeys | `journeys.json`, `TestOperatorPhase0JourneyFixtures` |
| Surface inventory | `contract_inventory.json`, `TestOperatorPhase0ContractInventory` |
| Usability corpus and scorecard | `operator-usability-corpus.tsv`, `scripts/operator-usability-measure.sh` |

## Baseline environment and gates

- Source contract: `docs/architecture/operator-experience.md` at commit
  `3affc92` (itself verified against `ba30643754d8d6cd85f91624a3f9bedf9d32457a`).
- Validation host: Linux 7.0.0-30-generic, amd64; Go 1.26.6.
- Passed on 2026-09-14: `go test ./cmd/hufu -count=1`, `go test ./...`,
  `go vet ./...`, `golangci-lint run` (0 issues), and `bin/check-docs`.
- The lint process reported sandbox cache-write warnings only; analysis
  completed successfully with zero findings.
- Platform evidence is recorded with each test run. Phase 0 does not claim
  Darwin, Windows, real-provider, PTY, epaper, or human-usability acceptance.

Generate a blank measurement report with:

```bash
scripts/operator-usability-measure.sh baseline /tmp/hufu-operator-baseline.md
```

The answer key is for the facilitator. Participants must use normal CLI/TUI
surfaces without being shown the expected State, What, or Next values.
