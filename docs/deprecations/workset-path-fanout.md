# Generic Workset Legacy Deprecation Record

> Status: deprecated, compatibility retained
> Authority: normative
> Verified-Commit: `902096a`
> Supersedes: `archive/implementation-plans/generic-workset-evidence.md`,
> `archive/implementation-plans/workset-legacy-tsv-removal.md` (PR-1–PR-3)
> Superseded-By: —
> Scope: path-based TSV fan-out only
> Canonical architecture: [workset](../architecture/workset.md)

## Decision

`FanOutSpec.source` remains supported as a workspace-relative TSV input for
one release cycle, but static contract lint now emits
`legacy_fanout_deprecated` at warning severity. New teams must publish a
manifest through an action provider and use `source-artifact` so the runtime
can retain producer, run, digest and expansion-receipt identity.

This is deliberately a compatibility window. Existing teams are not broken by
the deprecation warning, and the runtime does not infer a replacement artifact
from a path.

## Required migration evidence before removal

The legacy reader and artifact-backed reader must be compared using the saved
generic fixtures and a consumer-owned migration fixture. The comparison must
prove, for the same input generation:

- item ordering and unique keys are identical;
- scalar template substitution is identical;
- child count is identical.

The legacy reader (`expandFanOutTask` in `internal/team/fan_out.go`) never
creates a `WorksetBinding` or `WorksetExpansionReceipt`, so it has no
stale-source, duplicate-key, partial-child, or workset-scoped
retry/cancel/resume semantics to compare against: the artifact-backed reader
providing all of these, and the legacy reader providing none, is the reason
for this deprecation, not a gap to close with a symmetric equivalence test.
General task-level retry/resume/cancel is unaffected by fan-out kind and is
covered independently of this deprecation.

The consumer migration contract records the first two generic fixture checks:
`TestWP06GenericWorksetFixtures/transform` and
`TestWP06GenericWorksetFixtures/probe`. The legacy compatibility assertion is
`TestLegacyTSVFanOutIsDeprecatedButStillCompatible`. The ordering, key,
template-substitution, and child-count comparison above, plus a
consumer-shaped fixture standing in for the still-nonexistent real legacy
consumer, are `internal/team/workset_legacy_compare_test.go`
(`TestCompareLegacyAndArtifactWorksetOrdering`,
`TestCompareLegacyAndArtifactWorksetKeys`,
`TestCompareLegacyAndArtifactWorksetTemplateSubstitution`,
`TestCompareLegacyAndArtifactWorksetChildCount`,
`TestLegacyAndArtifactWorksetConsumerShapedFixtureEquivalent`).
`TestBundledTeamsHaveNoLegacyFanOutFinding`
(`internal/team/team_lint_test.go`) is a standing regression gate: today's
only bundled `fan_out` consumer (`.agent-teams/hufu-code-review`) already
uses `source-artifact`, so removal-gate item 1 below is satisfied for every
team this repository can discover — it does not by itself certify
user-authored teams outside this repository. `hufu team lint
--reject-legacy-fanout` (`internal/team/team_lint.go`,
`cmd/hufu/teamlintcmd.go`) lets a newly authored team, or CI on new
contributions, hard-fail on a legacy source ahead of the breaking removal.

## Removal gate

After one release cycle, removal requires all of the following in a separate
change:

1. every discovered team using `source` has migrated or has an explicit
   compatibility waiver;
2. consumer shadow/E2E evidence is retained and reviewed;
3. the migration fixture remains runnable in CI;
4. release notes announce the breaking removal;
5. the removal does not add consumer names or path parsing to Hufu core.

Until these conditions are met, only the warning and migration documentation
are active. The compatibility implementation remains intentionally small and
isolated in the generic fan-out adapter.
