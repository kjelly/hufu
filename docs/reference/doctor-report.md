# Doctor preflight report

> Status: active
> Authority: reference

`hufu doctor` runs provider, model, workspace, team-contract, event-integrity,
and recovery-readiness checks without starting an agent or performing recovery.
The default rendering writes human-readable diagnostics to stderr. Use
`hufu doctor --json` to write exactly one versioned JSON object to stdout,
including when a check fails and the command exits nonzero.

```json
{
  "schema_version": 1,
  "status": "degraded",
  "checks": [
    {
      "id": "recovery.unresolved",
      "status": "warning",
      "message": "1 unresolved high-risk task(s)",
      "count": 1
    }
  ]
}
```

The report status is `failed` if any check fails, `degraded` if no check fails
but at least one is `warning` or `unknown`, and `ready` otherwise. Only
`failed` returns a nonzero exit code. Checks are sorted by `id`, optional
`subject`, and message. Consumers should use `id`, `status`, and the optional
numeric `count`, not parse `message`.

| Check ID | Meaning |
| --- | --- |
| `provider.reachable` | Configured provider responds and reports model availability. |
| `models.configuration` | Model override configuration can be resolved; emitted on failure. |
| `models.resolved` | Role model configuration and provider-list comparison; `subject` names the role. |
| `workspace.writable` | Active managed workspace admits an exclusive, temporary write probe. |
| `teams.discovery` | Team search paths can be scanned. |
| `teams.contract` | Loaded team execution targets and verifier contracts are valid; `subject` identifies a team or finding. |
| `events.integrity` | The entire existing event chain validates, or no history exists. |
| `recovery.unresolved` | Active-branch high-risk tasks with `partial` or `unknown` recovery state. |

`recovery.unresolved.count` is present only when an existing checkpoint and
valid active-branch projection support a count, including zero. If both event
history and the checkpoint are absent, the check says `no prior session` and
has no count. If history exists but the checkpoint is absent, it is `unknown`
without a count. A malformed or unreadable checkpoint, invalid active branch
lineage, or corrupt event chain never becomes a zero count. The event check
fails on corruption; the recovery check remains unknown until the chain can
be trusted. Doctor never executes reconciliation or grants retry permission.

Reports do not include event payloads, task output, prompts, commands,
provider credentials, or raw provider URLs. The temporary workspace probe
does not overwrite the historical `.hufu-doctor-probe` filename or any other
pre-existing file.
