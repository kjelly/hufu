# strategic-decision

This bundled team is a configuration example for Hufu's decision runtime. It
uses canonical `decision.profile`, `decision.primary-profile`,
`decision.routing.hints`, and the top-level `request` block in `team.yaml`.

The local `light`, `standard`, and `high-stakes` profiles retain the auxiliary
V1 task-review behavior. `primary-standard` separately aliases the immutable
`builtin/standard@v2` bundle used by `hufu decide`. A command-line
`--primary-decision-profile` or `--rigor` value overrides that team default;
`--decision-profile` continues to affect auxiliary task review only.

The example deliberately has no model placeholder. Configure provider and
model selection through normal Hufu settings before executing it.

Provider-free inspection commands:

```text
hufu team validate .agent-teams/strategic-decision
hufu team explain .agent-teams/strategic-decision
hufu decision profile list --team .agent-teams/strategic-decision
hufu decision profile show standard --team .agent-teams/strategic-decision
hufu decision plan --profile standard --team .agent-teams/strategic-decision
hufu decide --team strategic-decision "Should we adopt option B?"
```

These commands compile without model calls or workspace writes. Legacy
`decision.default-profile`, `decision.routing-hints`, and
`decision.request-contract` remain accepted for compatibility, but new files
should use the canonical fields. To preview a migration:

```text
hufu team migrate --dry-run --canonical-authoring .agent-teams/strategic-decision
```

The migrator is dry-run only and never edits this directory. This team is an
example, not a special route: decision intent can use any valid team.
