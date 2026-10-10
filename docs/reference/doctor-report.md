# Doctor preflight report

> Status: active
> Authority: reference
> Verified-Commit: 2026-10-10
> Supersedes: —
> Superseded-By: —

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

Provider failures include an optional `reason_code` and, for an HTTP error,
numeric `http_status`. These additive schema-version-1 fields carry only
runtime-owned diagnostics; neither the raw error nor the response body is
rendered. Text mode uses the same safe message, including HTTP status when
available.

| Provider reason code | Meaning |
| --- | --- |
| `invalid_request` | The provider request configuration is invalid. |
| `request_cancelled` | The request was cancelled. |
| `timeout` | The request exceeded its deadline or a network timeout. |
| `dns_failed` | The provider hostname could not be resolved. |
| `connection_refused` | The provider refused the connection. |
| `tls_failed` | TLS certificate verification failed. |
| `transport_failed` | Another transport failure prevented the request. |
| `http_error` | The provider returned a non-200 status; `http_status` is present. |
| `invalid_model_list` | The successful HTTP response could not be decoded as a model list. |

Each `models.resolved` check also includes `model_state`: `configured` means
there is a target at the inspected CLI/global configuration level; `deferred`
means the team or agent may select it later. A deferred check remains `pass`
because omission at this level is valid. It does not certify that an effective
model has been selected or that a model call will succeed. A configured target
is compared with the provider's model list only when that list is nonempty.

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
without a count. With a valid event chain, a malformed or unreadable checkpoint
or invalid active branch lineage makes `recovery.unresolved` `fail`, without a
count. If the event chain is corrupt, `events.integrity` is `fail` and
`recovery.unresolved` is `unknown`, without a count; checkpoint and lineage
checks do not proceed until the chain can be trusted. None of these cases is
reported as a zero count. Doctor never executes reconciliation or grants retry
permission.

Reports do not include event payloads, task output, prompts, commands,
provider credentials, or raw provider URLs. The temporary workspace probe
does not overwrite the historical `.hufu-doctor-probe` filename or any other
pre-existing file.
