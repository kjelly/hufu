# Shadow comparison report

The five fixtures in `internal/team/testdata/shadow_sessions.json` are executed
by `TestShadowSessionFixturesProduceComparableBundlesWithoutMutatingLegacyPrompt`.
Each fixture records the legacy prompt that remains the model input during the
shadow phase, then compiles a canonical coordinator or worker bundle and checks
its fingerprint and token budget.

| Fixture | Role | Legacy-only data exercised | Canonical-only comparison evidence | Critical coverage |
| --- | --- | --- | --- | --- |
| coordinator-planning | coordinator | planning goal and STM decision | budgeted bundle fingerprint/token count | architecture decision retained |
| worker-implementation | worker | source path and test command | budgeted bundle fingerprint/token count | path/tool anchor input |
| worker-error | worker | error code and failed command | budgeted bundle fingerprint/token count | error/retry information |
| coordinator-duplicate | coordinator | repeated STM/LTM requirement | canonical compiler deduplication path | duplicate ratio input |
| worker-anchor | worker | SHA and git command | canonical anchor comparison input | commit/tool evidence |

## Comparison rules

- The fixture's `legacy_prompt` is snapshotted before compilation and must be
  byte-identical afterwards. Shadow compilation only observes it; it cannot
  substitute a canonical prompt into the legacy model path.
- Canonical-only evidence is the generated `CompiledContext` fingerprint,
  selected token count, omitted-item set, and anchor comparison metadata in
  `ShadowContextTrace`.
- Critical identifiers (paths, error code, SHA, commands, and decisions) are
  deliberately distributed across fixtures so missing-anchor diagnostics have
  stable, representative inputs.

Run `go test ./internal/team -run TestShadowSessionFixturesProduceComparableBundlesWithoutMutatingLegacyPrompt -v`
to regenerate the comparison evidence from the current compiler.
