---
name: team-builder
description: Use when the user asks to create, scaffold, design, or improve an agent team for Hufu — generating or updating team.yaml and agent .md files under .agent-teams/team-name. Design least-privilege roles, execution contracts, workflow phases, verification, recovery, and validation appropriate to the task's risk.
---

# Team Builder Skill

Design a runnable, contract-valid Hufu agent team under `.agent-teams/<team-name>/`.
Optimize for the task's observable outcome and failure boundaries, not the
largest possible collection of agents.

## Workflow

Use progressive disclosure: ask only what the task's risk actually requires.
A simple coding or research team should never need more than a name and a
one-sentence goal before files exist and validate.

```text
goal → risk classification → choose preset/topology → generate → compile → validate
```

1. **Get the goal.** If the user has not specified a team name or purpose,
   ask for those two things only: team name (lowercase, hyphenated) and a
   one-sentence goal. Do not ask about deliverables, budgets, rollback, or
   side effects yet — those questions are conditional on risk, not universal.
2. **Classify risk from the goal.** A team is **high-risk** if it involves any
   of: unattended/no-human execution, external state mutation (APIs, deploys,
   remote systems), infrastructure or credential changes, irreversible
   actions, or a strict acceptance requirement (a false "success" would be
   worse than a blocked run). Everything else — most coding, writing, and
   research tasks — is **low-risk**.
3. **Low-risk: choose a preset and generate immediately.** Prefer
   `hufu team create <name> --preset <preset>` over hand-authoring files —
   see [Presets](#presets) below for the six agent presets and four team
   presets. This is the same mechanism the CLI itself uses, so the result
   uses identical conventions (filename-inferred identity, `preset:`
   frontmatter, no default-valued `team.yaml` fields) to what a user would
   get by running the command directly. Only fall back to hand-authoring
   individual agent files when no preset's topology fits (e.g. a genuinely
   novel pipeline shape) — even then, prefer `preset:` frontmatter per agent
   over a raw `tools:` allowlist. Do not ask about model, deliverable
   format, or parallelism unless the user's one-sentence goal left the
   topology genuinely ambiguous.
4. **High-risk: ask the targeted follow-ups, then design deliberately.** Only
   now collect what the risk category actually needs: expected deliverable
   and objective success criteria; inputs/outputs/dependencies and what can
   run in parallel; the specific side effect (external state, infrastructure,
   credentials); and, for unattended runs, the required budget, rollback, and
   operator-approval boundaries. Choose the smallest topology that can verify
   the outcome (see [Reliability-First Team Design](#reliability-first-team-design)
   below) and use a contract-driven workflow (see
   [Contract-Driven Workflow Configuration](#contract-driven-workflow-configuration)) —
   none of the four team presets attempt to encode this shape; hand-author
   `team.yaml` and each agent `.md` for it. Define agent contracts before
   writing prompts: for every worker specify one responsibility, allowed
   tools, input source, output format, success criterion, side-effect class,
   recovery behavior, and whether it may modify files. Define handoffs by
   typed result or declared artifact, never by an assumed shared
   conversation. Never overwrite an existing team directory without the
   user's approval.
5. **Run static contract validation.** Run `hufu team validate --team
   <team-name>` (or `hufu team explain --team <team-name>` to see resolved
   tools/side-effects with their source) and `hufu list <team-name>`. Fix all
   errors before a model call. Run `hufu doctor` when provider, verifier
   executable, and workspace preflight are in scope.
6. **Run a safe preview.** Use `hufu --agent-team <team-name> --dry-run
   "<representative prompt>"` to inspect the resolved team without model calls.
   Run a real smoke test only for a read-only team or with explicit permission
   for the target system.
7. **Report.** Show the file tree, task topology, risk/verification choices,
   validation results, and one safe "try it" command.

## Presets

Reach for these first — they are the CLI's own building blocks
(`hufu team create --preset <name>`, `hufu team add <team> <agent> --preset
<name>`), not a skill-specific shortcut, so skill-generated and
CLI-generated teams look identical.

**Agent presets** (single-worker `--preset`, or per-agent `preset:`
frontmatter): `readonly` (inspect/search only), `coding` (inspect + edit +
verify), `review` (inspect + run checks, no mutation), `research` (inspect +
fetch), `writer` (inspect + produce documents), `ops` (scoped host/remote
operations — never grants `sudo` by default).

**Team presets** (`hufu team create <name> --preset <name>`, one command,
no hand-authored files):

| Team preset | Topology | Use for |
|---|---|---|
| `coding-single` | one `coding` worker | "just do X", simple task |
| `coding-reviewed` | `coding` producer + `review` reviewer | "build a feature", "review my work" |
| `research` | `research` researcher + `writer` writer | "research then write", "analyze then report" |
| `safe-ops` | `ops` operator + `review` monitor | scoped operational tasks — still add explicit `unattended`/budget/acceptance config yourself for a genuinely unattended run; the preset does not assume it |

If the user's goal matches one of these shapes, generate with the command
and skip straight to validation — do not hand-write `team.yaml` or agent
`.md` files for a shape a preset already covers.

## Reliability-First Team Design

### Select the execution shape

| Task shape | Recommended design |
| --- | --- |
| Simple, reversible, one deliverable | Single worker with an objective `verify` when possible |
| Code or document production requiring review | Producer → critic → producer, with critic read-only |
| Independent investigation | Parallel read-only workers, then one synthesizer |
| Ordered implementation steps | Plan → execute → verify, with explicit dependency and done criteria |
| Unattended or high-cost external operation | Runtime-enforced Prepare → Audit → Execute → Verify workflow with acceptance gate |
| Infrastructure or credential mutation | Dedicated action/execute boundary, capability requirement, reconciliation and rollback plan; do not delegate unrestricted shell access to every worker |

### Design each handoff

For each agent-to-agent edge, answer all of the following before generating
files:

- What exact result, artifact, or decision is produced?
- Which later agent consumes it, and how is the producer identified?
- What objective verifier proves the handoff is usable?
- What happens when it is partial, blocked, stale, or invalid?
- Can retry replay a side effect? If so, require reconciliation or manual
  recovery instead of blind retry.

Treat a model's prose as a claim, not evidence. A `success` result must mean
the declared outcome and its required verification are both complete.

## Directory Layout

```
.agent-teams/<team-name>/
├── team.yaml          # Team-level config (optional but recommended)
├── coordinator.md     # Exactly one agent with role: coordinator
├── <worker-1>.md      # One file per worker agent
└── <worker-2>.md
```

**Discovery rules** (from `discovery.go`):
- A directory is a team if it contains `team.yml`/`team.yaml` **or** at least one `*.md` file.
- When `team.yaml` is absent, the directory basename becomes the team name.
- When `team.yaml` is present, its `name:` field takes precedence (if non-empty).
- Team names are matched case-insensitively.
- An agent file needs no frontmatter at all: `name`/`role` infer from the
  filename (see [Agent .md Reference](#agent-md-reference)), and a
  frontmatter-free file's whole body becomes its system prompt.

## team.yaml Reference

`team.yaml` is optional — even `name` is only needed when it must differ
from the directory name — and every field below has a built-in default
(`max-rounds: 10`, `timeout: 600`, `max-retries: 2`, `workspace: workspace`).
Do not write a field just to pin its default value; only write what
genuinely overrides it. For a low-risk team, `hufu team create --preset`
(see [Presets](#presets)) writes a minimal or empty `team.yaml`
automatically — prefer that over hand-authoring one.

```yaml
name: my-team
description: "One-sentence team purpose"
max-rounds: 10          # coordinator round limit
max-steps: 30           # per-agent step budget
timeout: 600            # per-agent timeout in seconds
max-retries: 2
max-concurrent: 8       # parallel worker dispatch
workspace: workspace
model: ollama/qwen3:8b  # team-wide default model
temperature: "0.2"
max-tokens: "4096"
sidecar-model: ollama/qwen3:1b   # for skill matching / guard review
guard-model: ollama/qwen3:8b     # for output review
judge-model: ollama/qwen3:8b     # for multi-model result selection
skills: code-review,git-commit   # comma-separated skill names
auto-skills: false
no-net: false           # block network for all agents
force-mcp: false        # disable built-in exec/net tools
shell: bash             # default shell for mcp-tools
unattended: false       # no-human mode
max-duration: 0         # budget: wall-clock seconds (0 = unlimited)
max-total-tokens: 0     # budget: cumulative LLM tokens (0 = unlimited)
acceptance: ""          # shell command; non-zero exit = run not accepted
rollback: ""            # unattended: command after self-healing fails
vars:                   # template variables for agent prompts
  project_name: "my-app"
notify:
  type: webhook
  url: "https://hooks.example.com/agent"
providers:              # multi-provider pool
  openai:
    url: https://api.openai.com/v1
    key: $OPENAI_API_KEY
    models: [gpt-4o, gpt-4-turbo]
    aliases:
      gpt-4: gpt-4o
mcp-servers:
  filesystem:
    type: local
    command: ["npx", "-y", "@modelcontextprotocol/server-filesystem", "/path"]
  remote-api:
    type: remote
    url: "https://mcp-server.example.com/api"
    allowedTools: ["search", "query"]
tools:                  # team-wide tool allowlist
  allowed:
    - view
    - edit
    - write
    - bash
    - grep
    - glob
    - ls
```

### Contract-Driven Workflow Configuration

Use these fields only when a workflow must be enforced by Hufu at runtime.
They are not a substitute for a clear worker prompt; they make phase order,
capabilities, retries, and acceptance independently checkable.

```yaml
# Reject missing runtime prerequisites before dispatch.
requires:
  environment: [REQUIRED_TOKEN]
  paths: [/srv/project]
  network: true

# Restrict who can be delegated and freeze safety-critical task shapes.
delegation:
  allowed-workers: [preparer, auditor, executor, verifier]
  bind-task-goal-contracts: true
  no-redispatch-after-success: [preparer, auditor, executor, verifier]
  forbid-context-files: true

# policies/capabilities stay at the top level (not wrapped by advanced:).
policies:
  require_phase_success: true
  allow_phase_skip: false
  max_retries: 0
  fail_fast: true
capabilities:
  required: [domain-action]

# Prefer the advanced: namespace for workflow/verification/retry/reliability
# (an authoring convention, not a runtime difference — see "Advanced Namespace"
# below). Hufu, not coordinator prose, owns phase progression.
advanced:
  workflow:
    phases: [prepare, audit, execute, verify]
  verification:
    required: true
  retry:
    transient:
      max_attempts: 0
    repair:
      max_attempts_per_failure_signature: 0
  reliability:
    verifier-lint: error
    hard-enforcement: true
    max-systemic-failure-tasks: 1

# Bind a provider-neutral capability to a team-owned adapter. Keep domain
# commands and schemas here, never in Hufu core.
action-providers:
  domain-action:
    command: [bash, .agent-teams/my-team/run-action.sh]
    dir: /srv/project
    timeout: 1800

goal-mode: outcome
acceptance:
  mode: blocking
  require-no-unresolved-tasks: true
  commands: [bash .agent-teams/my-team/acceptance.sh]
```

### Advanced Namespace

`workflow`, `tasks`, `reliability`, `verification`, and `retry` may be
written either at the top level (as in older teams and examples elsewhere
in this file) or grouped under `advanced:` as shown above — both produce
identical effective behavior; `advanced:` is purely an authoring
convention that keeps runtime-oriented fields visually separate from
everyday team config. Prefer `advanced:` when generating a new
contract-driven team. Never define the same field in both places — Hufu
rejects that as a conflict rather than silently choosing one.

For a runtime action, declare a static task contract that assigns it to the
`execute` phase and makes verification objective. For an attestation-only
phase, give its worker only `submit_result`; do not grant a shell merely
because a later phase needs one. Keep the action adapter as the sole mutation
owner.

Do not set `unattended: true` unless every tool is allowlisted, every required
human decision has a safe default, budgets are configured, and the acceptance
failure/rollback behavior is intentional. Never use a destructive default
rollback for a team whose target or workspace has not been explicitly scoped.

### Field Priority (highest wins)

1. CLI flags (`--model`, `--temperature`, ...)
2. Agent `.md` frontmatter
3. `team.yaml`
4. `hufu.yaml` global config

## Agent .md Reference

`name` and `role` are both inferable from the filename: `developer.md`
infers `name: developer, role: worker`; `coordinator.md` infers
`name: coordinator, role: coordinator`. Only write them explicitly when
they must differ from the filename. Prefer `preset:` over a hand-written
`tools:` list for a common shape — see [Presets](#presets) — and only fall
back to explicit `tools:`/`side_effect:` when no preset fits or a preset
needs a narrower grant (`tools: {denied: [...]}` always wins over what a
preset grants).

```markdown
---
description: Implementation specialist
preset: coding                     # readonly, coding, review, research, writer, ops
tools:                             # optional: narrow a preset's grant, or use standalone
  denied: [bash]
skills: code-review               # skills to load
guard:                            # guard rules (per-agent only)
  - require-tests
  - no-profanity
model: ollama/qwen3:8b            # overrides team default
temperature: "0.2"
max-tokens: "8192"
top-p: "0.9"
top-k: "40"
timeout: 1200                     # seconds; overrides team default
max-retries: 2
max-steps: 50
provider-url: http://localhost:11434/v1
allowed-paths: ["src/", "tests/"]
restricted-path: "/etc"
no-net: false
force-mcp: false
shell: bash
side_effect: workspace_write        # none, workspace_write, external_write, infra_mutation, credential_mutation
recovery: retry                     # retry, reconcile, manual, never
reconcile-tool: "test -f output"    # read-only status probe for interrupted work
mcp-tools:                        # custom MCP tools (dict format)
  run-tests:
    cmd: go test ./...
    desc: Run Go tests
    inputs: [package]
  build:
    cmd: go build -o /tmp/app ./...
    desc: Build the application
  lint:
    cmd: golangci-lint run
    desc: Run linter
    shell: bash
---
Your system prompt here. Describe the agent's persona, workflow, and output format.
```

### Frontmatter Quick Reference

| Field | Required | Default | Notes |
|-------|----------|---------|-------|
| `name` | no | inferred from filename | Unique within team; used for `@<name>` invocation |
| `description` | no | — | One-line role description |
| `role` | no | inferred from filename (`worker`, or `coordinator` for `coordinator.md`) | `worker` or `coordinator` |
| `preset` | no | — | `readonly`, `coding`, `review`, `research`, `writer`, or `ops` — expands to `tools`/`side_effect`; merges with an explicit `tools: allowed`, never with a `tools: denied` |
| `tools` | no | — | Comma string, YAML list, or `{allowed: [...], denied: [...]}`; `denied` always wins, including over a preset's grant |
| `skills` | no | — | Skill names to load |
| `guard` | no | — | Guard rule names (YAML list) |
| `model` | no | team default | LLM model ID |
| `timeout` | no | team default | Seconds |
| `max-retries` | no | team default | `-1` = inherit team default |
| `max-steps` | no | team default | Per-agent step budget |
| `side_effect` | no | tool-inferred | Choose the strongest possible effect: `none`, `workspace_write`, `external_write`, `infra_mutation`, or `credential_mutation` |
| `recovery` | no | class-derived | Use `retry` only for replay-safe work; use `reconcile`, `manual`, or `never` for external effects as appropriate |
| `reconcile-tool` | no | — | Read-only probe for an interrupted task: exit 0=complete, 1=not started, 2=partial, other=unknown |

## Tool Catalog

**Coordinator tools:** `agent` (delegate), `load_skill`, `save_skill`, `finish`, `ask_user`, `todo`, `memory_save`, `memory_query`

**Worker tools (18):** `bash`, `sudo`, `ssh`, `scp`, `view`, `write`, `edit`, `multiedit`, `grep`, `glob`, `ls`, `lua`, `golang`, `ask_user`, `download`, `fetch`, `agentic_fetch`, `random`, `math`

**Always-included:** `agent`, `todo`, `memory_save`, `memory_query`

**Tool aliases:** `read` → `view`, `find` → `glob`. Both names work in `tools:` lists; canonical names are `view` and `glob`.

**Security flags:**
- `--rbash` / `no-net: true` → restricted bash / no network
- `force-mcp: true` → disables bash, sudo, ssh, golang, lua, download, fetch, agentic_fetch
- `allowed-paths` / `restricted-path` → filesystem sandbox

## Template Catalog

The templates below show what each shape's files look like and remain the
reference for anything a team preset doesn't cover. For the shapes a preset
already covers (marked below), generate with `hufu team create --preset`
first per [Presets](#presets) rather than hand-authoring these files —
read the template to understand the shape, not to copy-paste it verbatim
over a working preset command.

### Template: General Development Team

Best for: feature development, bug fixes, full-stack work.
**Covered by the `coding-reviewed` team preset** (`hufu team create <name>
--preset coding-reviewed`) for the 2-worker producer/reviewer shape; add a
third `tester` agent by hand only if the task specifically needs one.

```
.agent-teams/dev-team/
├── team.yaml
├── coordinator.md    # role: coordinator — delegates, synthesizes
├── architect.md      # role: worker — designs structure, APIs
├── developer.md      # role: worker — writes production code
└── reviewer.md       # role: worker — audits quality, security
```

**team.yaml:**
```yaml
name: dev-team
description: "A full-stack development team for building features"
max-rounds: 10
timeout: 3300
```

**coordinator.md:**
```markdown
---
name: coordinator
description: Team coordinator — decides who to delegate to and synthesizes results
role: coordinator
tools: ask_user,read
temperature: 0.1
---
You are the orchestrator. Your job is to coordinate agents to accomplish the user's task.

## How to Coordinate
1. Analyze the user's request to identify which team members are needed
2. Delegate independent tasks in parallel using the `agent` tool
3. Synthesize results into a coherent answer for the user

Always use the `agent` tool to delegate work. Do NOT implement code yourself.
```

**architect.md:**
```markdown
---
name: architect
description: System architect — designs structure, APIs, data models
max-tokens: 4096
temperature: 0.3
role: worker
tools: read,write,edit,bash,grep,glob,ls
---
You are a system architect. Design clear, implementation-ready specifications.
Define interfaces, data models, and API contracts. Return your design as your response.
Be specific and pragmatic.
```

**developer.md:**
```markdown
---
name: developer
description: Implementation specialist — writes production code
max-tokens: 8192
temperature: 0.2
role: worker
tools: read,write,edit,bash,grep,glob,ls
---
You are an implementation specialist. Write clean, well-structured production code.
Return your implementation summary as your response.
```

**reviewer.md:**
```markdown
---
name: reviewer
description: Code reviewer — audits quality, security, correctness
max-tokens: 4096
temperature: 0.2
role: worker
tools: view,write,edit,bash,grep,glob,ls
---
You are a code reviewer. Audit code for quality, security, and correctness.
Categorize findings as: critical, warning, or suggestion.
```

### Template: Reviewer-Critic Pair

Best for: any task that benefits from adversarial review (writing, tests, docs).

Pattern: coordinator → producer → critic → (producer applies feedback) → finish.

```
.agent-teams/<name>/
├── team.yaml
├── coordinator.md
├── <producer>.md     # creates the artifact
└── <critic>.md       # reviews, does NOT modify — only reports
```

**Key principle:** The critic never writes the fix. The coordinator routes the critic's feedback back to the producer. This prevents the critic from silently "fixing" things the producer should learn from.

**team.yaml:**
```yaml
name: <name>
description: "<one-sentence purpose>"
max-rounds: 5
timeout: 1800
max-retries: 2
```

**coordinator.md:**
```markdown
---
name: coordinator
description: Coordinates the team, deciding what to produce and routing critic feedback.
role: coordinator
tools: ask_user
timeout: 600
---
You are the coordinator. Given a task:
1. Decide what needs to be produced.
2. Delegate to <producer> to draft it.
3. Delegate to <critic> to review it.
4. Apply feedback by re-delegating to <producer>.
5. Call finish with a summary.
```

**producer.md:**
```markdown
---
name: <producer>
description: <what it produces>
tools: view,write,edit,bash,grep,glob
role: worker
timeout: 1200
---
You are a <role>. Given a description of what to produce:
- Match existing project style (read similar files first).
- <domain-specific instructions>
- Return the path of file(s) you created or updated.
```

**critic.md:**
```markdown
---
name: <critic>
description: Reviews <artifact> for correctness, quality, and gaps. Suggests improvements.
tools: view,bash,grep
role: worker
timeout: 600
---
You are a strict critic. Review the work produced by <producer>:
- <domain-specific review criteria>
- Return a numbered list of specific suggestions, no more than 5.
- Do NOT modify files yourself — the producer will apply your feedback.
```

### Template: Plan-Execution Team

Best for: faithfully executing a user-supplied plan without deviation.

```
.agent-teams/operator/
├── team.yaml         # max-rounds: 30, max-steps: 50
├── coordinator.md    # never adds/skips/reinterprets steps
├── plan-parser.md    # extracts strict JSON sub-task list
├── executor.md       # executes exactly one step
└── verifier.md       # PASS / DEVIATION / BLOCKED
```

**Key principle:** The coordinator enforces plan adherence. The parser extracts exactly the steps the user wrote (no more, no less). The executor runs one step. The verifier checks for deviation.

**team.yaml:**
```yaml
name: <name>
description: "Strict plan-execution team — executes a user plan faithfully"
max-rounds: 30
max-steps: 50
timeout: 1800
temperature: "0.2"
max-tokens: "4096"
```

**coordinator.md:**
```markdown
---
name: coordinator
description: Strict plan executor — never deviates from a user-supplied plan
role: coordinator
tools: ask_user
temperature: 0.1
max-tokens: 2048
---
You are the coordinator. Your defining principle is plan adherence.

## Workflow
1. Receive the plan. If no plan is present, ask the user via ask_user.
2. Delegate to plan-parser for a strict JSON decomposition.
3. Build the task list — TodoItems MUST equal the plan steps.
4. Execute in dependency order. Dispatch executor agents in parallel for independent steps.
5. Verify every step with verifier. If DEVIATION, re-dispatch executor (max 2 retries).
6. Synthesize only when all steps verified. Call finish.

## Hard Rules
- Never add a step the user did not write.
- Never skip a step.
- Never reinterpret a step's intent — use ask_user if ambiguous.
- Never finish early.
```

**plan-parser.md:**
```markdown
---
name: plan-parser
description: Extracts a strict, dependency-annotated sub-task list from a user plan
role: worker
tools: read
temperature: 0.0
max-tokens: 2048
---
You are a plan parser. Return a JSON array. Each element:
{"id": "step-1", "description": "<user's text>", "depends_on": [], "done_criteria": "<observable condition>"}

## Hard Rules
- One JSON object per plan step the user wrote. Count them.
- Do not invent or merge steps.
- description is the user's own text.
- done_criteria is observable — "file X exists" not "X is built".

If input is not a plan, return: {"error": "no plan detected", "input_excerpt": "<first 200 chars>"}
```

**executor.md:**
```markdown
---
name: executor
description: Executes exactly one sub-task from a plan, never more
role: worker
tools: read,write,edit,bash,grep,glob,ls
temperature: 0.2
max-tokens: 4096
---
You are a single-step executor. Execute ONLY the assigned step.

- Do not start, finish, or modify any other step.
- Do not optimize the plan or perform "while I'm at it" work.
- Return: {"step_id": "<id>", "status": "DONE" | "BLOCKED: <reason>", "output": "<result>", "criteria_met": true|false}
- If you cannot complete the step, return "BLOCKED: <reason>". Do not guess.
```

**verifier.md:**
```markdown
---
name: verifier
description: Verifies that an executor's output matches its assigned plan step
role: worker
tools: read,bash,grep,glob,ls
temperature: 0.0
max-tokens: 1024
---
You are a plan-step verifier. Decide whether the executor:
(a) Executed the correct step (not a different step)
(b) Satisfied the done_criteria

Return exactly one of:
- PASS
- DEVIATION: <one-sentence reason>
- BLOCKED: <reason>

Do not perform any execution. Do not write files. Only report.
```

### Template: Runtime-Enforced Outcome Workflow

Best for: unattended operations, expensive or externally mutating workflows,
and tasks where a false-success result is worse than a blocked run.

```
.agent-teams/<name>/
├── team.yaml
├── coordinator.md         # delegates only within the declared contract
├── preparer.md            # attestation or read-only preparation boundary
├── auditor.md             # read-only contract/evidence checkpoint
├── executor.md            # runtime-owned action boundary
├── verifier.md            # objective verification checkpoint
├── run-action.sh          # action-provider adapter
└── acceptance.sh          # whole-run gate
```

Use this topology only when each phase has a distinct safety purpose. For
example, a preparer and auditor should be attestation-only workers if the
runtime action owns all mutation. Do not create nominal phases that merely
repeat the same shell command.

**Design rules:**

- Give prepare/audit/verify workers only the tools their phase requires. For an
  attestation boundary, define a static contract whose only tool sequence entry
  is `submit_result`.
- Bind the execute capability to an `action-providers` adapter. The adapter
  owns the mutation, writes durable receipts, and returns failure to Hufu.
- Define `requires`, workflow phases, `require_phase_success`, `fail_fast`,
  capabilities, blocking acceptance, and verifier linting in `team.yaml`.
- Set retries to zero unless the domain operation is explicitly idempotent and
  replay-safe. Add a read-only reconciliation path before permitting replay.
- Make `acceptance.sh` verify the delivered outcome rather than merely check
  that a process exited zero.

### Template: Pipeline Team

Best for: multi-stage processing where each stage feeds the next (research → analyze → write).
For the simpler 2-stage research → write case with no separate analysis
stage, use the **`research` team preset** instead of hand-authoring this.

```
.agent-teams/<name>/
├── team.yaml
├── coordinator.md      # routes output from stage N to stage N+1
├── stage-1.md          # e.g. researcher
├── stage-2.md          # e.g. analyzer
└── stage-3.md          # e.g. writer
```

**Key principle:** The coordinator explicitly passes context from one agent's results to the next in the task description. Workers do not talk to each other directly.

**coordinator.md:**
```markdown
---
name: coordinator
description: Coordinates the pipeline, routing output from each stage to the next.
role: coordinator
tools: ask_user
---
You are the coordinator of a pipeline team. Your job:
1. Receive the user's request.
2. Delegate to <stage-1> to gather/research.
3. Pass <stage-1> results to <stage-2> in the task description.
4. Pass <stage-2> results to <stage-3> in the task description.
5. Synthesize the final output and call finish.

When delegating, use the `agent` tool. Pass context from one agent's results
to the next agent explicitly in the task description.

IMPORTANT: Always call `finish` when done. Do not just output text.
```

### Template: Security Audit Team

Best for: vulnerability scanning, secret detection, code pattern review.

```
.agent-teams/security-audit/
├── team.yaml
├── coordinator.md      # defines scope, aggregates report
├── dep-scanner.md      # CVE / license checks
├── secret-scanner.md   # hardcoded secrets
└── code-reviewer.md    # unsafe code patterns
```

Workers run in parallel (independent scans). Coordinator aggregates into a prioritized report.

### Template: Single-Worker Team

Best for: simple tasks that still benefit from coordinator ask_user / finish flow.
**Covered by the `coding-single` team preset** — this is what
`hufu team create <name>` (and `hufu init <name>`) generates with no
`--preset` given. Use when the user just needs one capable agent; do not
hand-author this shape.

```
.agent-teams/<name>/
├── team.yaml
└── worker.md           # role: worker — general-purpose, preset: coding
```

## Choosing a Template

| User says... | Team preset (generate first) | Template (reference / fallback) |
|---|---|---|
| "build a feature", "implement X" | `coding-reviewed` | General Development Team |
| "review my work", "check quality" | `coding-reviewed` | Reviewer-Critic Pair |
| "execute this plan", "follow these steps" | — (no preset covers this) | Plan-Execution Team |
| "research then write", "analyze then report" | `research` | Pipeline Team |
| "security audit", "find vulnerabilities" | — (no preset covers this) | Security Audit Team |
| "run unattended", "deploy", "rebuild and verify", "must not falsely succeed" | `safe-ops` for the topology, then hand-add unattended/budget/acceptance config | Runtime-Enforced Outcome Workflow |
| "just do X", "simple task" | `coding-single` (the default) | Single-Worker Team |

A row with a preset is high-risk only if step 2's risk classification says
so (e.g. "deploy" and "rebuild and verify" are high-risk regardless of
topology) — generate with the preset, then layer on the risk-appropriate
config from step 4 rather than hand-authoring the whole team.

## Design Principles

1. **One coordinator only.** Exactly one agent with `role: coordinator`. It delegates via the `agent` tool and calls `finish`. It does NOT implement work itself.
2. **Coordinator needs `ask_user`.** Include `ask_user` in the coordinator's tools so it can clarify ambiguity. Workers may also have `ask_user` for their own questions.
3. **Workers are specialized.** Each worker has a narrow, well-defined responsibility. Give it a clear system prompt describing its persona, workflow, and expected output format.
4. **Tools follow least-privilege.** Give each agent only the tools it needs. A reviewer needs `view,grep` but not `write`. A writer needs `write,edit` but maybe not `bash`.
5. **Temperature reflects role.** Coordinators and reviewers: low temperature (0.0–0.2) for deterministic decisions. Creative writers: higher (0.7+).
6. **Timeouts reflect workload.** Heavy workers (developers running builds): 1200–3300s. Light workers (parsers, verifiers): 600s.
7. **System prompt is the agent's identity.** Write it in second person ("You are a..."). Describe what to do, how to do it, and what to return. Be specific about output format (JSON, markdown, file path).
8. **Critic ≠ Fixer.** In review patterns, the critic reports findings but does NOT modify files. The coordinator routes feedback back to the producer.
9. **Parallelize independent work.** The coordinator can dispatch multiple workers in parallel via the `agent` tool. Use this for independent scans, independent file edits, etc.
10. **Pass context explicitly.** Workers do not see each other's output unless the coordinator includes it in the task description. When delegating stage N+1, include stage N's results.
11. **Make output contracts observable.** Define what each worker returns and what proves it is usable. Use typed results, declared artifacts, and `verify`/acceptance checks; do not rely on a final prose claim or a report file appearing eventually.
12. **Separate mutation from review.** Reviewers, fact-checkers, and verifiers are read-only. Give mutation authority to the smallest practical set of workers or to a runtime action provider.
13. **Make retries replay-safe.** Set `side_effect` and `recovery` accurately. Do not automatically retry external, infrastructure, or credential mutations without a deliberate idempotency and reconciliation design.
14. **Use runtime contracts for hard boundaries.** For ordered phases, fixed tool sequences, no-redispatch rules, capabilities, or task references that must not depend on model memory, declare the corresponding team policy or task contract.
15. **Keep consumer detail out of Hufu core.** Put domain commands, input schemas, paths, and acceptance logic in the team/adapter; use capability names and generic contracts at the Hufu boundary.

## Common Mistakes

| Mistake | Fix |
|---|---|
| No coordinator agent | Add one `.md` with `role: coordinator` |
| Multiple coordinators | Only one coordinator per team; merge them |
| Coordinator implements code itself | Coordinator should only delegate via `agent` tool |
| Worker tools too broad | Remove tools the worker doesn't need (least privilege) |
| Filename can't be used to infer a name (spaces, leading digit, etc.) | Either rename the file to a valid identifier, or add an explicit `name:` in frontmatter |
| Hand-writing a `tools:` allowlist that matches a built-in preset | Use `preset: <name>` instead — see [Presets](#presets) |
| `team.yaml` name differs from directory | The `name:` field takes precedence; keep them consistent |
| Critic modifies files | Critic should only report; producer applies fixes |
| No `finish` call | Coordinator must call `finish` with the final answer |
| Timeout too short for builds | Set worker `timeout: 1800+` for build-heavy tasks |
| `tools` as YAML list when string is simpler | Both work; use comma string for short lists, YAML list for long |
| Forgetting `ask_user` on coordinator | Coordinator needs it to clarify ambiguity |
| Worker claims success with no objective evidence | Add a task `verify`, typed verification spec, or blocking acceptance gate |
| Reviewer or verifier can modify production state | Remove write/edit/bash mutation permissions; use a separate producer or runtime action |
| `tool-sequence` names a tool the worker does not have | Align the worker tool grant with the full sequence before dispatch; do not expect the model to work around it |
| Workflow phase is only described in coordinator prose | Declare `workflow`, `policies`, capabilities, and task phase/action contracts in `team.yaml` |
| Same worker is redispatched after a successful irreversible step | Use `no-redispatch-after-success` and a downstream verifier or consumer task |
| Retry replays an external change | Classify `side_effect`, select `recovery: reconcile`/`manual`, and supply a read-only `reconcile-tool` |
| Producer passes a filesystem path as proof | Use declared artifact/typed result handoff; do not treat Todo IDs, checkpoints, or arbitrary paths as artifact references |
| Unattended team waits for input or retries forever | Set `unattended`, explicit tool allowlists, budgets, no-progress/retry limits, acceptance, and a reviewed rollback policy |
| `force-mcp` or `no-net` conflicts with worker tools | Remove incompatible tools or configure the required MCP/provider capability before writing the team |

## Validation Steps

After creating or changing a team, validate from cheapest and safest to most
expensive. Fix static errors before any model call.

```bash
# 1. Validate static contracts: task references, delegation, tool policy,
#    requirements, workflow/action configuration, and verifier contracts.
hufu team validate --team <team-name>

# 2. Confirm the team is discovered and agent frontmatter parses.
hufu list <team-name>

# 3. Preflight provider, workspace, verifier executables, and discoverable teams.
hufu doctor

# 4. Preview the resolved team without model calls or task execution.
hufu --agent-team <team-name> --dry-run "<representative prompt>"

# 5. Run a real smoke test only if it is read-only. For any mutating target,
#    obtain explicit authorization and report the command, exit code, evidence,
#    acceptance result, and unresolved tasks.
hufu @<team-name> "say hello" -v
```

For a workflow team, test both a valid path and at least one rejected path:
missing requirement, unavailable capability, invalid tool sequence, failed
verification, and blocked/partial result. A team is not production-ready if it
can only demonstrate its happy path.

## Example: Creating a Custom Team

User: "Create a team called `refactor-bot` that refactors Go code. It should have an analyzer that finds code smells, a refacturer that fixes them, and a tester that runs tests after."

1. Pick template: **Reviewer-Critic Pair** (adapted to 3 workers).
2. Create directory: `.agent-teams/refactor-bot/`
3. Write `team.yaml`:
   ```yaml
   name: refactor-bot
   description: "Refactors Go code: analyzes smells, refactors, and verifies tests pass"
   max-rounds: 8
   timeout: 1800
   ```
4. Write `coordinator.md` (role: coordinator, tools: ask_user).
5. Write `analyzer.md` (role: worker, tools: view,bash,grep,glob — finds smells, returns JSON list).
6. Write `refacturer.md` (role: worker, tools: read,write,edit,bash,grep,glob — applies fixes).
7. Write `tester.md` (role: worker, tools: view,bash,grep — runs `go test ./...`, reports results).
8. Run `hufu list refactor-bot` to validate.
9. Report: `hufu @refactor-bot "refactor internal/team/coordinator.go"`.
