# strategic-decision

This bundled team demonstrates Hufu's decision-authoring contract. It uses
canonical `decision.profile`, `decision.routing.hints`, and the top-level
`request` block in `team.yaml`; the loader materializes these into runtime
configuration before dispatch.

The local `light`, `standard`, and `high-stakes` names alias versioned built-in
profiles. The team owns worker authorization, capability evidence, routing
hints, and read-only tool policy. Configure a provider model through normal
Hufu settings before running a non-`off` decision profile.

Provider-free inspection commands:

```text
hufu team validate .agent-teams/strategic-decision
hufu team explain .agent-teams/strategic-decision
hufu decision profile list --team .agent-teams/strategic-decision
hufu decision profile show standard --team .agent-teams/strategic-decision
hufu decision plan --profile standard --team .agent-teams/strategic-decision
```

These commands compile without model calls or workspace writes. Legacy
`decision.default-profile`, `decision.routing-hints`, and
`decision.request-contract` remain accepted for compatibility, but new files
should use the canonical fields. To preview a migration:

```text
hufu team migrate --dry-run --canonical-authoring .agent-teams/strategic-decision
```

The migrator is dry-run only and never edits this directory.
