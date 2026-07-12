---
name: team-builder
description: Use when the user asks to create, scaffold, or design an agent team for hufu — generating the team.yaml and agent .md files under .agent-teams/<name>/.
---

# Team Builder Skill

Scaffold a runnable hufu agent team under `.agent-teams/<team-name>/` with a `team.yaml` and one or more agent `.md` files.

## Workflow

1. **Clarify intent.** If the user has not specified a team name or purpose, ask. Collect:
   - Team name (lowercase, hyphenated)
   - One-sentence goal
   - Which agents/roles are needed (coordinator + workers)
   - Default model (e.g. `ollama/qwen3:8b`)
2. **Pick a template** from the catalog below that closest matches the user's goal. Adapt it; do not copy verbatim if the user's domain differs.
3. **Write the files.** Create `team.yaml` first, then each agent `.md`. Never overwrite existing files — if the team directory already exists, ask the user before proceeding.
4. **Validate.** Run `hufu list <team-name>` to confirm the team is discovered and all agents parse. Run `hufu doctor` to verify the model is reachable.
5. **Report.** Print the file tree and a one-line "try it" command: `hufu @<team-name> "<example task>"`.

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

## team.yaml Reference

Only `name` is recommended; everything else has built-in defaults (`max-rounds: 10`, `timeout: 600`, `max-retries: 2`, `workspace: workspace`).

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

### Field Priority (highest wins)

1. CLI flags (`--model`, `--temperature`, ...)
2. Agent `.md` frontmatter
3. `team.yaml`
4. `hufu.yaml` global config

## Agent .md Reference

```markdown
---
name: developer
description: Implementation specialist
role: worker                      # "worker" or "coordinator"
tools: view,write,edit,bash,grep,glob,ls   # string or YAML list
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
| `name` | yes | — | Unique within team; used for `@<name>` invocation |
| `description` | no | — | One-line role description |
| `role` | no | `worker` | `worker` or `coordinator` |
| `tools` | no | — | Comma string or YAML list |
| `skills` | no | — | Skill names to load |
| `guard` | no | — | Guard rule names (YAML list) |
| `model` | no | team default | LLM model ID |
| `timeout` | no | team default | Seconds |
| `max-retries` | no | team default | `-1` = inherit team default |
| `max-steps` | no | team default | Per-agent step budget |

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

### Template: General Development Team

Best for: feature development, bug fixes, full-stack work.

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

### Template: Pipeline Team

Best for: multi-stage processing where each stage feeds the next (research → analyze → write).

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

```
.agent-teams/<name>/
├── team.yaml
└── helper.md           # role: worker — general-purpose
```

This is what `hufu init <name>` generates. Use when the user just needs one capable agent.

## Choosing a Template

| User says... | Template |
|---|---|
| "build a feature", "implement X" | General Development Team |
| "review my work", "check quality" | Reviewer-Critic Pair |
| "execute this plan", "follow these steps" | Plan-Execution Team |
| "research then write", "analyze then report" | Pipeline Team |
| "security audit", "find vulnerabilities" | Security Audit Team |
| "just do X", "simple task" | Single-Worker Team |

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

## Common Mistakes

| Mistake | Fix |
|---|---|
| No coordinator agent | Add one `.md` with `role: coordinator` |
| Multiple coordinators | Only one coordinator per team; merge them |
| Coordinator implements code itself | Coordinator should only delegate via `agent` tool |
| Worker tools too broad | Remove tools the worker doesn't need (least privilege) |
| `name` field missing in frontmatter | `name` is required — it's used for `@<name>` invocation |
| Agent names with spaces/uppercase | Use lowercase-hyphenated names |
| `team.yaml` name differs from directory | The `name:` field takes precedence; keep them consistent |
| Critic modifies files | Critic should only report; producer applies fixes |
| No `finish` call | Coordinator must call `finish` with the final answer |
| Timeout too short for builds | Set worker `timeout: 1800+` for build-heavy tasks |
| `tools` as YAML list when string is simpler | Both work; use comma string for short lists, YAML list for long |
| Forgetting `ask_user` on coordinator | Coordinator needs it to clarify ambiguity |

## Validation Steps

After creating a team, verify it works:

```bash
# 1. Check the team is discovered and agents parse
hufu list <team-name>

# 2. Preflight: model reachable, workspace writable
hufu doctor

# 3. Smoke test with a simple prompt
hufu @<team-name> "say hello"

# 4. Run with verbose to see agent output
hufu @<team-name> "do a small task" -v
```

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
