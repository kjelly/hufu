# Hufu artifact-backed worksets

> Status: active
> Authority: normative
> Verified-Commit: `6ab9951`
> Supersedes: `archive/implementation-plans/generic-workset-evidence.md`
> Superseded-By: —

A workset is a provider-neutral, artifact-backed fan-out contract. The
runtime owns identity, freshness, expansion, replay, and whole-group
verification; a consumer owns the meaning of each item.

## Canonical flow

```text
producer/action
    → immutable manifest artifact + digest
    → WorksetExpansionReceipt
    → one immutable WorksetBinding per child task
    → typed TaskResult / artifact verification
    → workset_complete acceptance projection
```

`WorksetManifest` is bounded and normalized. Its raw content stays in the
artifact store. The task/event boundary carries only bounded metadata and
opaque artifact references. A path is not a workset identity.

`WorksetExpansionReceipt` binds the workset ID, producer task, source artifact
ID/digest, item count, stable item-key digest, and child mapping. Replay keeps
the first equivalent receipt and fails on a conflicting receipt. Each child
retains the same immutable source identity through `WorksetBinding`.

## Verification rules

`workset_complete` is a runtime verifier, not a parser for transcripts or
rendered reports. It checks the canonical group state for the declared source
task and can require every item to be terminal, verified, and in an accepted
status set. A partial expansion, stale source, duplicate key, missing child,
or unverifiable result blocks acceptance.

The generic runtime does not know team names, VCS concepts, review headings,
or consumer-specific item fields. Consumer adapters publish the manifest and
declare their own typed-result expectations.

## Compatibility

`FanOutSpec.source` (workspace-relative TSV) remains readable for one release
cycle and emits `legacy_fanout_deprecated`. New teams must use
`source-artifact`; see the [deprecation record](../deprecations/workset-path-fanout.md)
for the removal gate.

Implementation anchors are [`workset.go`](../../internal/team/workset.go),
[`workset_verification.go`](../../internal/team/workset_verification.go), and
the contract checks in [`team_policy_lint.go`](../../internal/team/team_policy_lint.go).
