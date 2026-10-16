# Offline diagnostic pilot

These team-owned resources implement stages 0–1 and the offline fixture portion
of stage 2 of the bounded diagnostic proposal. They are not wired into the
team's normal execution or action catalog.

From the repository root:

```bash
go run ./.agent-teams/hufu-dev/diagnostic-pilot --evaluate
go run ./.agent-teams/hufu-dev/diagnostic-pilot --evaluate --traces
go run ./.agent-teams/hufu-dev/diagnostic-pilot --shadow-workspace /tmp/hufu-diagnostic-shadow --fixture fail-pass
go run ./cmd/hufu inspect --workspace /tmp/hufu-diagnostic-shadow --output json beliefs run-diagnostic-fixture
go test ./.agent-teams/hufu-dev/diagnostic-pilot
golangci-lint run ./.agent-teams/hufu-dev/diagnostic-pilot/...
```

Go's `./...` skips hidden directories, so the explicit test and lint commands
are required alongside the repository gates.

`--shadow-workspace` requires a dedicated empty or existing offline fixture
workspace. `--fixture` accepts `pass-pass` (default), `pass-fail`, `fail-pass` or
`fail-fail`; it cannot be combined with `--evaluate`. Shadow computes embedded
comparisons directly, with zero provider/model/verifier calls, and records
choices and observations in the canonical journal. Repeating a stopped fixture
appends nothing. Inspect reconstructs from frozen artifacts and reports the same
`replay_digest`, without reading the team or repairing missing data. These
fixture records do not claim runtime receipt provenance or authorize actions.

The provider implements the existing `team.ActionProvider` interface. Its
standalone stdin/stdout mode accepts the existing action envelope with
`capability: diagnostic_fixture`, `type: check_source|check_environment`, and a
JSON string `payload` satisfying [input.schema.json](input.schema.json).
`fixture_ref` must be a registered canonical bundle digest, available in the
evaluation report. An unknown digest or path is rejected; there is no path
resolution, shell, plugin loading, network or host-credential access. Bundles
are compiled into the provider through `embed.FS`, never loaded from user paths.

[fixture.schema.json](fixture.schema.json) bounds the immutable bundle to eight
integer vectors, two version strings and a failure eligibility reference.
[measurement.schema.json](measurement.schema.json) describes the typed
comparison. The objective verifier recomputes the selected comparison and
digest; a source comparison failure is still a successful collection action.
Timeout/cancellation hooks are test-only contexts; normal probes do no sleeping.

[provenance.json](provenance.json) records `fixture_design_v1`, independent seed
grids and the 5:3:2 prior. The oracle belongs only to the evaluation harness.
Registrations are trusted fixture authorship, not a way to identify hidden
real-world causes. A bundle claiming another scope is refused. The noisy model
permits all four probe outcome pairs within the single-fault synthetic universe.

The report includes eight cached traces and a deterministic 100,000-row paired
corpus hash. Both strategies cost two units per case and yield the same final
posterior; this pilot makes no accuracy or savings claim. `--traces` emits raw
corpus reference rows; resolve their trace IDs through the ordinary report.

See [the preparation reference](../../../../docs/reference/diagnostic-preparation.md)
for configuration, current boundaries and deferred runtime work.
