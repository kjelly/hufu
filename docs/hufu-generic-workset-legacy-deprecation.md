# Generic Workset Legacy Deprecation Record

> Status: deprecated, compatibility retained
> Scope: path-based TSV fan-out only
> Related plan: [hufu-generic-workset-evidence-plan.md](hufu-generic-workset-evidence-plan.md)

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
- stale-source, duplicate-key, partial-child, retry, cancel and resume
  semantics are no weaker under the artifact-backed reader;
- the artifact-backed path produces a durable expansion receipt and a
  blocking `workset_complete` result;
- no marker file, transcript parser, or rendered report text participates in
  acceptance.

The consumer migration contract records the first two generic fixture checks:
`TestWP06GenericWorksetFixtures/transform` and
`TestWP06GenericWorksetFixtures/probe`. The legacy compatibility assertion is
`TestLegacyTSVFanOutIsDeprecatedButStillCompatible`.

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
