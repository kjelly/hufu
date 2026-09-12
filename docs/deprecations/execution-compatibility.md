# Execution Compatibility Sunset

> Status: draft
> Authority: normative
> Verified-Commit: `2026-09-12`
> Supersedes: —
> Superseded-By: —
> Scope: durable execution identity only
> Canonical architecture: [execution runtime](../architecture/execution-runtime.md)
> Warning-Introduced-In: pending PR-4 release

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

Before the removal release, users will be able to run:

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
