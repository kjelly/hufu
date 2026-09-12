# Execution Compatibility Sunset

> Status: active, warning introduced in v2.0.0
> Authority: normative
> Verified-Commit: `8fd1802`
> Supersedes: —
> Superseded-By: —
> Scope: durable execution identity only
> Canonical architecture: [execution runtime](../architecture/execution-runtime.md)
> Warning-Introduced-In: v2.0.0
> Warning-Release: [v2.0.0](https://github.com/kjelly/hufu/releases/tag/v2.0.0)
> Warning-Release-Workflow: [34679777188](https://github.com/kjelly/hufu/actions/runs/34679777188)
> Warning-Published-At: 2026-09-12
> Earliest-Removal: blocked until one later published release has also carried the warning

## Decision

`ExecutionTarget{backend, model}`, `ExecutionTopology`, `BackendBinding`,
receipt `backend`, and v4 execution-policy routes are the only durable
execution identity written by current Hufu versions.

The historical `local` backend alias and provider-era durable shadows remain
read-compatible during the sunset window:

- task/session/event `model`, `model_topology`, `subagent_provider`, and
  `provider_binding` identity fields;
- receipt `subagent_provider`;
- v3 policy-route `legacy_provider`;
- `provider_session_bound` event and `execution-events.jsonl` `provider`.

New writers must not emit these fields. Existing workspaces are not rewritten:
the later materializer appends a self-contained canonical migration event after
a read-only inspector has proved a unique mapping. Ambiguous identity fails
closed and cannot be guessed from current configuration or provider
availability.

## User migration contract

Before the removal release, users can run:

```bash
hufu migrate inspect-execution --workspace <path>
hufu migrate apply-execution --workspace <path> --apply
```

The inspector is read-only. The apply command is append-only, branch-aware,
and idempotent; it never rewrites EventStore history. A workspace with an
ambiguous or unmigratable subject receives no migration append.

## Removal gate

Removal occurs only in a separate semver-appropriate release after all of the
following are true:

1. a release has emitted the compatibility warning and a later published
   release has carried it for a full release window;
2. saved legacy fixtures inspect, materialize, and replay under canonical-only
   runtime readers;
3. production inventories have no migratable, ambiguous, or unmigratable
   task or policy subjects;
4. release notes include migration and rollback instructions.

`providers:`, `ProviderManager`, provider keys, transient adapter fields, and
the `SubagentProvider` protocol are not part of this deprecation. They have
different transport or configuration semantics and require a separate public
deprecation before removal.

## Release evidence and remaining owner handoff

The warning release was published as `v2.0.0`. Its successful release workflow
uploaded checksums plus Linux and macOS archives for amd64 and arm64. The
published release notes include both migration commands, the append-only
contract, and rollback by restoring a complete pre-migration workspace backup.

The release owner must supply the remaining evidence in this order:

1. Publish at least one later release carrying the same warning before
   calculating the earliest semver-appropriate removal release.
2. Run `inspect-execution` against maintainer-designated production
   workspaces and retain only aggregate task and policy classification
   counters.
3. Attach the warning tag, later release tag, release-notes reference, and
   sanitized zero-actionable inventory to the removal PR.

Release or production evidence cannot be replaced by a local build, an
untagged commit, fixture results, or edits to this record.
