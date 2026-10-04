# Hufu Recovery Evidence and Doctor Implementation Plan

> Status: Implemented
> Scope: Existing task recovery and `hufu doctor`
> Authority: Implementation plan; runtime behavior is defined by `docs/architecture/execution-runtime.md` and the code

## Outcome

Implement two bounded improvements using Hufu's existing runtime:

1. Make reconciliation results typed and record the evidence source used to classify an interrupted task. For external, infrastructure, credential, or unknown side effects, task output alone must not prove completion.
2. Give `hufu doctor` a stable JSON report and add read-only checks for the active workspace's event integrity and unresolved recovery tasks.

The first change prevents an ambiguous external mutation from being treated as complete merely because the worker emitted text. Both changes make recovery decisions easier to inspect without adding another operation store, task lifecycle, retry engine, or command family.

These changes can be implemented and tested entirely in this repository. They require no external account, deployed service, remote provider, or product choice.

## Existing contracts to preserve

- `internal/team/recovery.go` defines the four persisted recovery states: `not_started`, `complete`, `partial`, and `unknown`. Existing session data and events must remain readable.
- `internal/team/repair_controller.go` owns retry, reconcile, replan, and block decisions. Do not add a second decision engine or grant new automatic retry permission.
- `internal/team/execution_fallback.go` currently denies cross-target fallback after an executed side effect. This gate remains unchanged.
- `internal/team/targeted_recovery.go` provides operator-requested `ReconcileTask` and `RetryTask`; normal crash resume uses `internal/team/coordinator_session.go`. Both paths must consume the same reconciliation result.
- `internal/team/event_store.go` and `internal/team/event_store_stream.go` own durable event identity, branch-scoped idempotency, and hash-chain validation. Do not rewrite old logs.
- `internal/inspect/` already projects persisted recovery state, and `cmd/hufu/inspectcmd.go` already exposes read-only inspection. Extend these surfaces where needed.
- `cmd/hufu/doctor.go` already checks provider/model availability, workspace writability, team discovery, and team contracts. Preserve those checks and their current exit behavior unless the newly specified integrity check finds an unsafe workspace.
- `docs/architecture/execution-runtime.md` is the active runtime authority. Update it when implementation changes a normative recovery invariant.

## Change 1 — Typed reconciliation evidence

### Contract

Replace the internal string-only return from `reconcileInterruptedTask` with one typed result. Keep the existing JSON spelling of `TodoItem.RecoveryState` and the four state values for compatibility. Convert the typed resolution to that persisted string only at the existing projection boundary. Suggested shape:

```go
type RecoveryResolution string

const (
    ResolutionNotStarted RecoveryResolution = "not_started"
    ResolutionComplete   RecoveryResolution = "complete"
    ResolutionPartial    RecoveryResolution = "partial"
    ResolutionUnknown    RecoveryResolution = "unknown"
)

type ReconcileSource string

const (
    ReconcileSourceVerifySpec    ReconcileSource = "verify_spec"
    ReconcileSourceReconcileTool ReconcileSource = "reconcile_tool"
    ReconcileSourceVerify        ReconcileSource = "verify"
    ReconcileSourceTaskOutput    ReconcileSource = "task_output"
    ReconcileSourceNone          ReconcileSource = "none"
)

type ReconcileResult struct {
    Resolution RecoveryResolution
    Source     ReconcileSource
    ExitCode   *int // absent if no subprocess supplied a trustworthy exit code
}
```

The implementer may adjust type names to fit the package. Validate resolution and source combinations before persisting them. Do not put command output, tool arguments, environment values, credentials, or arbitrary error strings in the evidence record.

Classification rules:

| Evidence | Result |
| --- | --- |
| Declared `verify_spec`, `reconcile_tool`, or `verify` succeeds | `complete`; record the source and exit code when available |
| Declared check exits 1 | `not_started` |
| Declared check exits 2 | `partial` |
| Declared check times out, fails to start, has another exit code, or yields no valid result | `unknown` |
| No declared check; task has `external_write`, `infra_mutation`, `credential_mutation`, or `unknown` side effect | `unknown`, even if task output is non-empty |
| No declared check; low-risk task has non-empty task output | Preserve current `complete` behavior and identify `task_output` as the source |
| No declared check or usable output | `unknown` |

The exit-code interpretation above applies only to the existing authored check contract. It is not a claim that arbitrary shell commands are inherently read-only. No new cleanup or compensation action is introduced.

### Persistence and consumers

- Reuse the existing `recovery_decision` event and task checkpoint/transition machinery. Add stable, bounded evidence fields to the event payload where a reconciliation result is recorded; do not create a parallel event log or a new operation ID.
- Keep `RecoveryState` as the projected decision input. Persisting a source must not itself change task status or authorize replay. If event-first ordering or reducer changes are needed for a new durable field, implement and test them in the canonical event path.
- Update normal crash resume, targeted operator reconciliation, and any other caller of `reconcileInterruptedTask` together. Keep acceptance revalidation before a reconciled task becomes done.
- Expose the source and optional exit code through the existing `hufu inspect task` text and JSON views. Bind evidence to the selected session, branch lineage, task, and attempt; an older attempt's result must not describe the current attempt. Historical events without the new fields must be presented as source `unknown`; never infer `not_started` or `complete` from their absence.
- Preserve existing output and result contracts in CLI text, JSON output, reports, and TUI; update a consumer only if it displays this recovery evidence.
- Cross-target fallback must continue to require its existing no-side-effect proof. A `not_started` reconciliation does not silently bypass frozen execution routes, attempt budgets, task recovery policy, or targeted-retry validation.

### Acceptance tests

1. A crashed high-risk task with non-empty output and no declared check remains `unknown` and is not marked done or automatically retried.
2. Authored checks returning 0, 1, 2, another code, a launch error, and a timeout produce the states above. No raw output or secret appears in durable evidence.
3. Both resume and `hufu reconcile --task` record the same state/source semantics. A `complete` result still passes the existing acceptance revalidation before `TaskDone`.
4. Old event logs and sessions without source fields replay successfully and show an unknown source. Branch isolation and event hash-chain validation remain intact.
5. Existing low-risk retries and execution-route fallback tests retain their behavior; no new automatic cross-target retry is permitted.

Implement this as one reviewable recovery change before starting the doctor change. Likely owner files: `internal/team/recovery.go`, `coordinator_session.go`, `targeted_recovery.go`, the existing event/reducer code if required, and `internal/inspect/` projections and tests. Trace normal worker, direct-agent, unattended, and crash-resume entry points before editing.

## Change 2 — Structured doctor report

### CLI contract

Add `hufu doctor --json`. JSON goes only to stdout; diagnostic text goes only to stderr in default mode. An invocation returns exactly one JSON object, including on a failed preflight:

```json
{
  "schema_version": 1,
  "status": "ready",
  "checks": [
    {"id": "provider.reachable", "status": "pass", "message": "provider reachable"}
  ]
}
```

Use fixed lowercase `id` and `status` values. `status` is `ready`, `degraded`, or `failed`; a check status is `pass`, `warning`, `fail`, or `unknown`. Any `fail` makes the report `failed`; otherwise any `warning` or `unknown` makes it `degraded`; otherwise it is `ready`. Sort checks by stable ID and subject so JSON is deterministic. Optional `subject` may identify a configured team or model. Messages are human-readable, redacted, and not a machine decision key. Do not include event payloads, prompts, provider credentials, raw URLs with embedded credentials, or filesystem contents.

Keep exit code 0 for the current non-failing doctor result, including warnings; keep a nonzero exit for current failures. Do not introduce exit code 2. Event-store corruption in an existing active workspace is a new failure because the runtime cannot trust its history. When JSON is selected, return the JSON report before propagating the command's nonzero exit so automation still receives the diagnostic object.

### Collection rules

- Extract the existing doctor checks into one typed collector used by both text and JSON renderers. Text output should remain recognizable; JSON and text must agree on pass, warning, and failure findings.
- For an existing active workspace, validate the complete event chain through the read-only event-stream API. A missing event log means no recorded history; it is not corruption. Buffer findings until full validation succeeds; do not summarize a valid prefix if a later record fails validation.
- Read the active branch lineage and existing session/task projection without starting a coordinator. Report the count of non-terminal or blocked tasks whose high-risk side effect has `partial` or `unknown` recovery state. Mark that check as `warning`; do not infer that a task is safe to retry or run reconciliation from doctor. Never combine tasks from sibling branches.
- If both the session checkpoint and event history are absent, report that there is no prior session without a numeric unresolved-task count. If history exists but the checkpoint is absent, report an `unknown` recovery check. A malformed or unreadable checkpoint, or an invalid branch lineage, yields `fail`. None of these cases is reported as zero unresolved tasks.
- Existing provider/model checks may make documented read-only network requests. Doctor must not start agents, execute recovery, run compensation, or provision an execution environment.
- The current workspace writability probe must use an exclusive unique temporary filename and remove only the file it created. Preserve the existing workspace-creation behavior for compatibility, but never overwrite a pre-existing `.hufu-doctor-probe` or any other user file.

### Acceptance tests

1. Text and JSON are derived from the same findings for ready, warning, provider failure, and team-contract failure cases. JSON parses on success and failure, with no extra stdout bytes.
2. A valid event log passes; a broken hash chain fails; a missing log is reported as empty history. No event store or session file is created by the integrity check.
3. An unresolved high-risk task is visible as a warning; a missing or unreadable session never appears as a count of zero.
4. An existing `.hufu-doctor-probe` remains unchanged. Temporary probe cleanup does not delete another file, including after a probe error.
5. Legacy doctor invocations preserve exit codes. Secrets and raw event payloads do not appear in JSON or text diagnostics.

Likely owner files: `cmd/hufu/doctor.go`, its command/flag registration, doctor tests, and the existing read-only team/inspect APIs. Add a new package only if the current package boundaries cannot keep collection and rendering separate.

## Implementation order and validation

1. Implement Change 1 with focused tests and update `docs/architecture/execution-runtime.md` for the high-risk output rule and evidence contract.
2. Implement Change 2 against the resulting persisted recovery projection. Keep the JSON schema versioned and the text renderer compatible.
3. For each code change, run the required gates separately: `go test ./...`, `go vet ./...`, and `golangci-lint run`. Fix failures before considering the change complete.
4. Review `git diff -- cmd/hufu internal` for any team-specific policy, duplicate recovery machinery, or unrelated changes. Preserve pre-existing user edits in the working tree.

This plan is complete when both changes pass their acceptance tests and the validation gates, and ordinary local runs retain their existing target selection, tool authorization, verification, and retry boundaries.
