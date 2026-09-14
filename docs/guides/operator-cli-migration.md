# Operator CLI migration

> Status: active
> Authority: guide
> Verified-Commit: 2026-09-14
> Supersedes: —
> Superseded-By: —

The canonical operator commands are additive façades. Existing entry points
remain available; migrate scripts when exact scope and common output selection
are useful, not because the legacy spelling has been removed.

| Existing entry point | Canonical equivalent | Important distinction |
|---|---|---|
| `hufu @TEAM "TASK"` | `hufu run --team TEAM -- "TASK"` | `--` preserves prompts beginning with `-`; runtime effects stay equivalent |
| `hufu --agent-team TEAM "TASK"` | `hufu run --team TEAM -- "TASK"` | `--team` and `--agent-team` may agree, but conflicting values fail before action |
| `hufu list [TEAM]` | `hufu team list [TEAM]` | Both are read-only aliases |
| `hufu init TEAM` | `hufu team create TEAM` | Deterministic creation remains non-interactive; `--wizard` is explicit and TTY-only |
| `hufu status -w WORKSPACE` | `hufu session status -w WORKSPACE --team TEAM` | Canonical form binds exact run/branch scope |
| `hufu resume -w WORKSPACE` | `hufu session resume -w WORKSPACE --team TEAM --run RUN --branch BRANCH` | Resume is not a new session and does not imply `--new` |
| `hufu retry TASK -w WORKSPACE` | `hufu session retry -w WORKSPACE --team TEAM --run RUN --branch BRANCH --task TASK` | Mutation revalidates the active target and eligibility |
| `hufu reconcile TASK -w WORKSPACE` | `hufu session reconcile -w WORKSPACE --team TEAM --run RUN --branch BRANCH --task TASK` | Required for unknown external effects when policy permits; not an automatic retry |
| command-specific `--json` | supported command's `--output json` | Existing payload and exit semantics remain unchanged |

Do not mechanically replace an exact legacy `-w` with `--workspace-root`.
`--workspace` is the exact team workspace; `--workspace-root` is a root from
which canonical run selection joins a team name once. Multi-team prompts use
the existing explicit `@team` segments and a root, not one ambiguous exact
workspace shared by multiple teams.

Before changing automation, replay its help and dry/read-only path, compare
stdout JSON and exit codes, then migrate one entry point at a time. The
[operator command reference](../reference/operator-command-reference.md) is
generated from the same metadata used by `hufu examples`.
