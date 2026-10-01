# Action Providers

> Status: active
> Authority: reference
> Verified-Commit: `2797cbd`
> Supersedes: —
> Superseded-By: —

Action providers are team-owned adapters for structured runtime actions: a
command, an embedded Go package, or one tool of a declared MCP server. A team
declares a provider under `action-providers` and binds a static task to its
capability. Hufu passes the action as JSON, preserves the normal action
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

`command` is argv, not shell text. `dir` and `timeout` are optional; `timeout`
is expressed in seconds. Existing command providers remain supported.

`dir` and any relative path in `command` are resolved against the Hufu
process working directory, not the team directory. When `dir` is empty the
adapter runs in that working directory. Use absolute paths when a team can be
run from more than one directory.

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

### MCP provider

An `mcp` provider calls one tool of an MCP server that the team declares under
`mcp-servers`:

```yaml
mcp-servers:
  diagnostics:
    type: local
    command: [/opt/incident-team/bin/diagnostics-mcp]
    allowedTools: [collect_debug]

action-providers:
  diagnostics:
    runtime: mcp
    server: diagnostics
    tool: collect_debug
    timeout: 120
```

`server` must name a declared server exactly, and that server must be `local`
or `remote`. `tool` must pass the server's `allowedTools` and
`excludedTools`. `command`, `dir`, `source`, and `mode` must not be set, and
the command and Go providers reject `server` and `tool`. `hufu team validate`,
`hufu team lint`, and `--dry-run` check all of this without starting the
server.

Only `team.yaml` selects the server and tool; a model, proposal, catalog
argument, or payload cannot change them. The action `type` stays Hufu's action
identity and is not sent. The action `payload` must be one JSON object without
duplicate keys, at most 64 KiB. It is sent as the tool's arguments and must
satisfy the tool's input schema, which Hufu validates as full JSON Schema. The
schema may declare its own draft, but no external reference is loaded. The
server receives only these arguments: none of the `HUFU_*` values in
[Runtime environment](#runtime-environment) reach it.

An MCP action is read-only. A static task that uses an MCP provider must
declare `side_effect: none` explicitly, and a catalog entry must declare
`side-effect: none`; an MCP provider cannot back a run-input resolver. Like
every side-effect class, `none` is the maintainer's declaration, not a
sandbox: confirm that the bound tool does not change anything.

**Startup.** Every run binds each MCP provider to its tool before any model or
action call. The run does not start when the server failed to load, the server
does not expose the tool, or the tool's input schema does not compile. An
unrelated MCP server failure stays the ordinary warning.

**Worker isolation.** A bound tool leaves every worker tool surface, including
workers that inherit all tools and `use_dynamic_tool`. A worker that declares
the bound `server__tool` in `tools:` is a load error. Other tools of the same
server are unaffected. Workers see an MCP-backed catalog entry only through
`team_action_list` and `team_action_get`, which name no server or tool.

**Authorization.** Before the call is sent, Hufu authorizes exactly that
`server:tool` on behalf of the task's agent through its MCP authorization
policy, and records the decision as a `policy_decision` event.

**Timeout.** `timeout` (seconds) bounds the call together with the task's
deadline. With `timeout: 0` and no task deadline, the MCP default of 30
seconds applies. This differs from the command provider, where zero means no
limit.

**Result.** When the tool result has `structuredContent`, it must be a JSON
object and becomes the action's outputs. Otherwise the result must have
exactly one text block, which becomes `outputs.result`; the text is not parsed
as JSON. A tool error (`isError`), any other block shape, and more than 1 MiB of
raw content fail the action. The runtime output limits and a catalog output
schema then apply as for any provider; for a text result, the output schema
declares `result` as a string. An MCP result never declares artifacts.

**Identity and resume.** The provider name is `mcp:<server>/<tool>`. Its
identity adds the server, the tool, and a hash of the server configuration:
type, command, URL, allowed and excluded tools, `noOAuth`, and environment
variable names with digests of their values. The execution policy snapshot pins
that identity and the tool descriptor bound at startup, and each runtime action
receipt records `provider_descriptor_sha256`. A resume fails closed after the
server configuration or the tool's descriptor (name, description, or input
schema) changes; start a new session with `--new`. Hufu lists a server's tools
once, when the server loads, so a change on the server during a run is caught
at the next start, not mid-run.

**Errors.** An MCP action failure starts with a stable code:

| Code | Meaning | Tool called |
| --- | --- | --- |
| `mcp_action_bind_failed` | The provider could not be bound at startup | No |
| `mcp_action_unbound` | The provider was never bound | No |
| `mcp_action_payload_invalid` | The payload is not one JSON object or fails the input schema | No |
| `mcp_action_identity_missing` | No runtime-owned task attempt | No |
| `mcp_action_authorization_missing` | No runtime action authorizer | No |
| `mcp_action_descriptor_mismatch` | The tool no longer matches the bound descriptor | No |
| `mcp_action_authorization_denied` | The authorization policy denied the call | No |
| `mcp_action_transport_failed` | The call failed or timed out | Possibly |
| `mcp_action_tool_error` | The tool reported an error; its text is redacted | Yes |
| `mcp_action_result_invalid` | The result does not meet the result rules above | Yes |

A static task binds an MCP provider like any other:

```yaml
tasks:
  - id: collect-debug
    agent: reviewer
    phase: prepare
    side_effect: none
    action:
      capability: diagnostics
      type: collect_debug
      payload: '{"service":"api"}'
```

Both examples are kept loadable:
[internal/team/testdata/docs-mcp-action-catalog](../../internal/team/testdata/docs-mcp-action-catalog/team.yaml)
holds a dynamic team with an MCP-backed catalog entry, and
[internal/team/testdata/docs-mcp-action-workflow](../../internal/team/testdata/docs-mcp-action-workflow/team.yaml)
a workflow team with the static task. `TestMCPActionProviderDocExamplesLoad`
loads and lints both. Update them together with this section.

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
  "resolver_version": "3"
}
```

Unknown fields, malformed JSON, oversized output, and non-zero adapter exits
are failures. Resolver providers should be deterministic and side-effect free.

An input may additionally declare `resolver.mode: semantic_json`. In that
mode Hufu first asks its provider-bound sidecar to translate the invocation
prompt into exactly one JSON value matching the input schema (or `null`). The
sidecar cannot return a command, action payload, path request, or prose. Hufu
strictly parses and schema-validates the value, then sends it to the declared
resolver provider as `candidate_value`. The provider must deterministically
validate team-owned cross-field rules and either return `matched` with the
candidate unchanged or return `invalid` with a diagnostic. It must not infer
meaning from the prompt or rewrite the candidate. Hufu allows one bounded
semantic repair using that diagnostic and revalidates the repaired value before
stamping resolver provenance and freezing the input snapshot. A second invalid
candidate fails closed. The deterministic resolver remains the fallback only
when semantic resolution is unavailable, returns `null`, or fails; invalid
semantic output is never silently replaced by a default.

A translation that fails, because the sidecar timed out, dropped its stream,
or returned something other than one JSON value, is attempted three times.
Each retry gets the declared `timeout` multiplied by the attempt number and a
larger output budget. When all three fail and neither an explicit value nor
the deterministic resolver supplies one, the input's default applies without
failing the run, but visibly: Hufu emits a warning naming the
`--input <name>=<json>` remedy, tells the coordinator that the default may not
be what the request asked for, and appends a `RUN INPUT DEFAULTED` notice with
the default value and the failure to the final answer. `--dry-run` never
invokes the semantic resolver. Command-shaped strings are rejected before provider
validation or snapshot/event persistence.

Team-owned vocabulary belongs in `resolver.semantic-guidance`. Hufu includes
this bounded repository-authored text in the semantic conversion prompt while
keeping the core runtime domain-neutral. For example, a review team can declare
that a named commit maps to its `last_n` variant while `working_tree` is reserved
for uncommitted changes. Guidance improves translation accuracy; the provider's
candidate validation remains the authoritative safety boundary.

Explicit `--input` or `--input-file` values do not bypass prompt resolution.
Hufu compares each explicit value with the resolver candidate after canonical
JSON validation: equal values retain the explicit source plus resolver
evidence, while different values fail with `input_prompt_conflict`. This is an
intentional fail-closed consistency check, not an override precedence rule. The
error shows both canonical values, redacted and bounded, so a misread prompt can
be told apart from an ambiguous one.

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
| `HUFU_ACTION_INVOCATION_ID` | Action invocation ID. It changes on every attempt, so it is not an idempotency key |

The adapter should use the supplied action workspace for transient outputs.
Declared artifacts are copied into Hufu's workspace and receive verified
provenance after execution.

## Binding a provider to a task

The capability is referenced by a static task contract in a workflow team.
The workflow owns phase dispatch, so the team must bind task goals to
contracts, list the capability as required, give every phase a static
contract with `contract_id`, and, when verification is required,
declare an objective check in the verify phase. An action outside the execute
phase must be `side_effect: none`:

```yaml
name: review-team
description: Prepares a review workset with an action provider, then reviews it
workflow:
  phases: [prepare, audit, execute, verify]
capabilities:
  required: [prepare-workset]
verification:
  required: true
delegation:
  bind-task-goal-contracts: true

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
    side_effect: none
    action:
      capability: prepare-workset
      type: prepare
      payload: '{"scope":{"kind":"last_n","count":10}}'

  - id: audit-workset
    agent: reviewer
    phase: audit
    side_effect: none

  - id: review-workset
    agent: reviewer
    phase: execute
    side_effect: none

  - id: verify-review
    agent: verifier
    phase: verify
    side_effect: none
    verify-spec:
      type: command_exit
      command: test -s review.md
```

This example is kept loadable: the same team, with minimal `reviewer.md`,
`verifier.md`, and Go adapter, lives in
[internal/team/testdata/docs-action-provider-example](../../internal/team/testdata/docs-action-provider-example/team.yaml)
and `TestActionProvidersDocExampleLoads` loads it. Update both together.

Keep mutation in the adapter. Prepare, audit, and verify agents should receive
only the tools required for their attestation or verification work. Configure
retries only when replay is safe and idempotent; otherwise use reconciliation
or explicit recovery.

## Action catalog

An action catalog lets a team offer predefined actions to its model roles
without letting a model define them. Workers can inspect the actions they are
allowed to see and record typed recommendations; only the coordinator
dispatches one, and the runtime compiles it from the frozen catalog into an
ordinary action task. A model chooses which approved action to run and with
what arguments. It never chooses the provider, capability, action type, side
effect, or recovery, and it cannot bypass the normal task lifecycle.

### Declaring entries

Each entry names an `action-providers` capability, the provider's action type,
the worker the action runs as, and a typed argument schema:

```yaml
name: incident-team
description: Diagnoses a service incident and runs predefined diagnostics on request
action-providers:
  diagnostics:
    command: [/opt/incident-team/actions/diagnostics.sh]
    timeout: 120

action-catalog:
  collect-debug-bundle:
    description: Collect bounded runtime diagnostics for one service.
    capability: diagnostics
    type: collect_debug_bundle
    agent: runtime-engineer
    side-effect: none
    input-schema:
      type: object
      properties:
        service:
          type: string
          min-length: 1
          max-length: 128
        include-thread-dump:
          type: boolean
      required-properties: [service]
      additional-properties: false
    output-schema:
      type: object
      properties:
        summary:
          type: string
      required-properties: [summary]
    access:
      discover: [runtime-engineer, network-engineer]
      propose: [runtime-engineer, network-engineer]
    invocation:
      require-proposal: true
      allow-unattended: true
      max-invocations: 2

  rotate-service-logs:
    description: Rotate one service's local logs into the run workspace.
    capability: diagnostics
    type: rotate_service_logs
    agent: runtime-engineer
    side-effect: workspace_write
    recovery: manual
    input-schema:
      type: object
      properties:
        service:
          type: string
      required-properties: [service]
      additional-properties: false
    access:
      discover: [runtime-engineer]
```

| Field | Rule |
| --- | --- |
| ID (map key) | `^[a-z][a-z0-9-]{0,63}$` |
| `description` | 1–2048 bytes |
| `capability` | a configured `action-providers` capability |
| `type` | passed to the provider as the action type, 1–128 bytes |
| `agent` | the worker the task runs as; not the coordinator or `helper`, and inside `delegation.allowed-workers` when set |
| `side-effect` | `none` or `workspace_write` |
| `recovery` | `retry`, `manual`, or `never`; optional for `none` (defaults to `retry`), required for `workspace_write` |
| `input-schema` | the run-input schema dialect; the top level is an object, every object sets `additional-properties: false`, and `number` is not allowed (use `integer`) |
| `output-schema` | optional; validates the provider's `outputs` |
| `access.discover` / `access.propose` | workers that may inspect / recommend the entry; propose must be a subset of discover |
| `invocation.require-proposal` | dispatch needs a `recommended` proposal for the same arguments |
| `invocation.allow-unattended` | allow dispatch in unattended runs |
| `invocation.max-invocations` | 1–64 admitted tasks per session, default 1 |

Catalog keys are kebab-case: an entry uses `side-effect`, while a static task
contract uses `side_effect`. Property names in either schema must not be keys
that durable-event redaction rewrites. A team may declare at most 128 entries.

The same team is kept loadable in
[internal/team/testdata/docs-action-catalog-dynamic](../../internal/team/testdata/docs-action-catalog-dynamic/team.yaml);
`TestActionCatalogDocExamplesLoad` loads it.

### Validation

`hufu team lint` reports every catalog problem, and any error stops `LoadTeam`:

| Code | Meaning |
| --- | --- |
| `action_catalog_id_invalid` | the entry ID is not a valid action ID |
| `action_catalog_entry_invalid` | a required field is missing or too long, an unknown key, or more than 128 entries |
| `action_provider_missing` | the capability has no configured provider |
| `action_catalog_side_effect_unsupported` | the side effect is not `none` or `workspace_write` |
| `action_catalog_recovery_invalid` | recovery is not `retry`, `manual`, or `never` |
| `action_catalog_recovery_required` | a `workspace_write` entry does not set recovery |
| `action_catalog_input_schema_invalid` / `action_catalog_output_schema_invalid` | a schema breaks the rules above |
| `action_catalog_agent_unknown` | the agent or an access role is not a team agent |
| `action_catalog_agent_unreachable` | the agent is the coordinator, `helper`, or outside `allowed-workers` |
| `action_catalog_agent_unsupported` | a role declares `extra-models`, or an isolated-workspace agent runs a `workspace_write` entry |
| `action_catalog_access_invalid` | a duplicate role, or a proposer that cannot discover the entry |
| `action_catalog_proposer_unreachable` | `require-proposal` is set but no proposer can record a proposal |
| `action_catalog_invocation_invalid` | `max-invocations` is outside 1–64 |
| `action_catalog_phase_unreachable` | a workflow team has no phase that may run the entry |
| `action_catalog_tool_sequence_unsupported` | a closed `tool-sequence` lists a catalog tool |

A proposer counts only if the coordinator can dispatch it, team `tools-denied`
does not remove `team_action_propose`, and it does not run on an external agent
backend such as Codex, which does not receive Hufu tools. Startup repeats this
check after `--worker-model` and model selectors are resolved and fails before
any provider call when no proposer remains.

### Workers, proposals, and dispatch

A worker listed in `access.discover` gets `team_action_list` and
`team_action_get`, which show the entries it may see and their input schemas
but never the capability, type, agent, or provider. A worker listed in
`access.propose` also gets `team_action_propose`. A proposal records the action,
canonical arguments, an assessment (`recommended`, `candidate`, `defer`, or
`reject`), a rationale, an optional expected outcome, and artifact IDs the
worker may already read. It is a durable `team_action_proposed` event; it
creates no task and authorizes nothing by itself. Workers never see each
other's proposals.

The coordinator gets its own `team_action_list` / `team_action_get`, which add
the executing agent, invocations used, whether each action is dispatchable now
(with a blocked reason), and the latest proposals. It dispatches by adding a
task to the `agent` tool:

```json
{"agent": "runtime-engineer", "goal": "collect a debug bundle for api",
 "catalog_action": {"id": "collect-debug-bundle", "arguments": {"service": "api"}}}
```

A catalog task may set only `agent`, `goal`, `constraints`, `catalog_action`,
and, in a team without phases, `depends_on`. The runtime checks the catalog,
initial batch, durable journal, entry, agent, phase, unattended policy,
arguments, duplicates in the batch, required proposal, invocation budget, and
decision profile, in that order. A rejected dispatch creates nothing and
returns a `TEAM ACTION DISPATCH REJECTED:` error the coordinator can recover
from: it can inspect the entry, ask a worker for evidence or a proposal, or
continue without the action. After three such rejections in one invocation,
or during policy repair or wrap-up, a rejection takes the ordinary delegation
policy repair path.

The invocation budget counts every admitted catalog task in the session,
including one whose provider never started. `--new` starts a session with no
proposals and a fresh budget.

### Workflow teams

In a workflow team a catalog action may run in EXECUTE, and a `side-effect:
none` action may also run in PREPARE. The last static EXECUTE contract moves
the workflow to VERIFY as soon as it succeeds, so dispatch a catalog action
before, or together with, that contract. A catalog task does not count as a
phase contract, and a failed catalog task does not fail the phase. Catalog
tasks in a workflow team cannot use `depends_on`.

```yaml
name: release-team
description: Prepares, applies, and verifies a release with a predefined preflight check
workflow:
  phases: [prepare, audit, execute, verify]
verification:
  required: true
delegation:
  bind-task-goal-contracts: true

action-providers:
  release-checks:
    command: [/opt/release-team/actions/release-checks.sh]
    timeout: 300

action-catalog:
  check-release-window:
    description: Report whether the release window is open for one environment.
    capability: release-checks
    type: check_release_window
    agent: executor
    side-effect: none
    input-schema:
      type: object
      properties:
        environment:
          type: string
          enum: [staging, production]
      required-properties: [environment]
      additional-properties: false
    access:
      discover: [preparer, executor]
      propose: [preparer]

tasks:
  - id: prepare-release
    agent: preparer
    phase: prepare
    side_effect: none
  - id: audit-release
    agent: auditor
    phase: audit
    side_effect: none
  - id: apply-release
    agent: executor
    phase: execute
    side_effect: workspace_write
    recovery: manual
  - id: verify-release
    agent: verifier
    phase: verify
    side_effect: none
    verify-spec:
      type: command_exit
      command: test -s release-notes.md
```

This team is kept loadable in
[internal/team/testdata/docs-action-catalog-workflow](../../internal/team/testdata/docs-action-catalog-workflow/team.yaml).

### Recovery, idempotency, and trust

`side-effect` is the maintainer's declaration about a trusted provider, not a
sandbox. The provider runs with the Hufu process's working directory and full
environment; the class only decides how Hufu schedules, replays, and gates the
task. A `workspace_write` entry therefore has to state its recovery:

- `retry` declares that the provider is idempotent by
  `HUFU_CATALOG_INVOCATION_ID`. An interrupted task is re-run on resume with the
  same ID, including a task whose provider finished but whose completion was
  not yet recorded.
- `manual` blocks an interrupted task for a human on resume.
- `never` skips it.

`HUFU_CATALOG_INVOCATION_ID` is stable across attempts and resumes of one
catalog task; `HUFU_ACTION_INVOCATION_ID` still changes on every attempt.

The catalog hash, which includes each entry's provider configuration (runtime,
source, mode, command, dir, timeout, the Go source digest, and for an MCP
provider its server, tool, and server configuration hash), is part of the
execution policy snapshot. Changing the catalog or its providers makes a resume
fail closed; start a new session with `--new`. A command provider's identity is
its argv, dir, and timeout, not the content of the script it runs: use the
embedded Go runtime when the adapter's content must be pinned.

When an entry declares an output schema, outputs that do not match fail the
task without a retry, because the provider has already run.

### Inspecting a catalog

```bash
hufu team action list [team-directory] [--team <name>] [--output text|json]
hufu team action show <action-id> [team-directory] [--team <name>] [--output text|json]
```

`list` summarizes each entry's capability, type, agent, side effect,
recovery, and invocation policy; its JSON output also carries access and the
entry hash. `show` prints one entry in full, adding its description, access,
entry hash, and input and output schemas. Neither shows the provider's
command, source, or directory. `hufu inspect task` shows a catalog task's
action, entry and arguments hashes, invocation ID, and linked proposals;
`hufu report` lists catalog tasks with their arguments hash only.

### Limits

- The `agent` tool schema is refreshed only when a new model stream starts, so
  `catalog_action` hidden while the initial batch is pending appears on the
  next stream.
- When more than 32 entries can run in the current phase, the schema omits the
  ID enum; the runtime still validates the ID.
- A session holds at most 256 proposals.

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
