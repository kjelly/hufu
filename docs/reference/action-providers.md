# Action Providers

Action providers are team-owned adapters for structured runtime actions. A
team declares a provider under `action-providers` and binds a static task to
its capability. Hufu passes the action as JSON, preserves the normal action
lifecycle, and records provider identity in lifecycle events and receipts.

The provider owns domain semantics. Hufu does not interpret Git, deployment,
cloud, database, or other domain-specific commands.

## Provider types

### Command provider

The legacy command form starts an external executable with an argv array. It
receives one JSON request on stdin and must write one JSON value to stdout:

```yaml
action-providers:
  prepare-workset:
    command: [bash, ./actions/prepare-workset.sh]
    dir: .
    timeout: 300
```

`command` is argv, not shell text. `dir` and `timeout` are optional. Existing
command providers remain supported.

### Embedded Go provider

Use the embedded Go runtime for a maintainer-authored static Go package:

```yaml
action-providers:
  prepare-workset:
    runtime: golang
    source: ./actions/prepare-workset
    mode: trusted-static
    timeout: 300
```

`source` is resolved relative to the team directory, not the process working
directory. `runtime: golang` and `mode: trusted-static` are required; `command`
and `dir` must not be set for this provider type.

The source directory must contain a package named `main` with exactly one
exported entrypoint:

```go
func Run(ctx context.Context, in io.Reader, out io.Writer) error
```

The package must be self-contained with respect to the embedded interpreter;
standard-library imports are supported. It must not declare a `main` function.
The runtime validates the source path, rejects symlinks, hashes the non-test Go
files, and verifies the hash again before execution. A source change therefore
fails closed instead of silently running a different adapter.

Example layout:

```text
.agent-teams/review-team/
├── team.yaml
├── coordinator.md
├── reviewer.md
└── actions/
    └── prepare-workset/
        ├── main.go
        └── main_test.go
```

`main_test.go` is useful for native tests but is not loaded by the runtime.
The Go action is trusted team code: it runs with the Hufu process user's
authority and may deliberately use `os`, `os/exec`, Git, and file I/O. Those
capabilities remain inside the pre-written adapter; Hufu core does not expose
a Git capability or turn this provider into a coding-agent tool.

## Request and response contract

An action provider receives an `Action` envelope:

```json
{
  "capability": "prepare-workset",
  "type": "prepare",
  "payload": "{\"scope\":{\"kind\":\"last_n\",\"count\":10}}"
}
```

The adapter writes one JSON value. A structured result normally uses this
shape:

```json
{
  "outputs": {
    "manifest_path": "workset/workset-manifest.json"
  },
  "artifacts": []
}
```

Artifacts are re-identified and verified by Hufu after the provider returns;
an adapter cannot smuggle a trusted artifact reference through stdout.

For a run-input resolver, Hufu sends a `resolve_run_input` envelope and
requires the strict resolver response contract:

```json
{
  "status": "matched",
  "value": {"kind":"last_n","count":10},
  "evidence": [{"source":"prompt","start":7,"end":24,"kind":"last_commits"}],
  "resolver_version": "1"
}
```

Unknown fields, malformed JSON, oversized output, and non-zero adapter exits
are failures. Resolver providers should be deterministic and side-effect free.

## Runtime environment

Hufu adds invocation identity to the adapter environment when available:

| Variable | Meaning |
| --- | --- |
| `HUFU_WORKSPACE` | Invocation-scoped action workspace |
| `HUFU_REPOSITORY` | Resolved repository root |
| `HUFU_TEAM` | Team name |
| `HUFU_RUN_ID` | Coordinator run ID |
| `HUFU_TASK_ID` | Todo/task ID |
| `HUFU_ATTEMPT` | Current task attempt |
| `HUFU_ACTION_INVOCATION_ID` | Action invocation ID |

The adapter should use the supplied action workspace for transient outputs.
Declared artifacts are copied into Hufu's workspace and receive verified
provenance after execution.

## Binding a provider to a task

The capability is referenced by a static task contract. The task should assign
the action to the appropriate runtime phase and define objective verification:

```yaml
workflow:
  phases: [prepare, audit, execute, verify]

action-providers:
  prepare-workset:
    runtime: golang
    source: ./actions/prepare-workset
    mode: trusted-static
    timeout: 300

tasks:
  - id: prepare-workset
    agent: reviewer
    phase: prepare
    action:
      capability: prepare-workset
      type: prepare
      payload: '{"scope":{"kind":"last_n","count":10}}'
```

Keep mutation in the adapter. Prepare, audit, and verify agents should receive
only the tools required for their attestation or verification work. Configure
retries only when replay is safe and idempotent; otherwise use reconciliation
or explicit recovery.

## Validation

Validate the team before invoking a model or running the action:

```bash
hufu team validate --team review-team
hufu list review-team
hufu doctor
hufu --agent-team review-team --dry-run "prepare the review workset"
```

For a Go adapter, also run its native tests and the repository validation
gates:

```bash
go test ./.agent-teams/review-team/actions/prepare-workset
go test ./...
go vet ./...
golangci-lint run
```

The dry run does not execute the action. A real smoke test should be read-only,
or have explicit authorization for the adapter's target system.
