# hufu

> ⚠️ **Fork notice** — This repository (`kjelly/hufu`) is a personal fork of [`anomalyco/hufu`](https://github.com/anomalyco/hufu). Its Go module path is `github.com/kjelly/hufu`. Releases are published from this fork via GoReleaser.


> A Go CLI tool that orchestrates teams of LLM agents via Ollama to collaboratively accomplish tasks

`hufu` is a command-line tool written in Go that coordinates teams of multiple LLM agents (via Ollama), enabling them to work together on complex tasks through division of labor. Teams are discovered by name from configured search paths, and a single prompt can switch between multiple teams or invoke specific agents directly.

- **Module**: `github.com/kjelly/hufu`
- **Go language version**: 1.26.5
- **Go toolchain**: 1.26.6
- **CLI framework**: [cobra](https://github.com/spf13/cobra)
- **LLM framework**: [charm.land/fantasy](https://charm.land/fantasy)
- **MCP client**: [github.com/mark3labs/mcp-go](https://github.com/mark3labs/mcp-go)

---

## Table of Contents

- [Features](#features)
- [Installation & Build](#installation--build)
- [Quick Start](#quick-start)
- [CLI Flags Reference](#cli-flags-reference)
- [Prompt Syntax](#prompt-syntax)
- [Interactive Mode](#interactive-mode)
- [Team Configuration](#team-configuration)
- [Agent .md File Format](#agent-md-file-format)
- [MaxSteps Priority](#maxsteps-priority)
- [Team Directory Structure](#team-directory-structure)
- [Skills System](#skills-system)
- [Workspace Layout](#workspace-layout)
- [Memory System (RAG)](#memory-system-rag)
- [MCP Configuration](#mcp-configuration)
- [Multi-Provider Support](#multi-provider-support)
- [Sidecar System](#sidecar-system)
- [Guard System](#guard-system)
- [Worker Tools Reference](#worker-tools-reference)
- [Signal Handling](#signal-handling)
- [Loop Detection](#loop-detection)
- [Session Management](#session-management)
- [TUI Mode](#tui-mode)
- [Dry Run Mode](#dry-run-mode)
- [Plan-First Mode](#plan-first-mode)
- [Report Generation](#report-generation)
- [Idle Warning](#idle-warning)
- [Configuration File (hufu.yaml)](#configuration-file-hufuyaml)
- [Defaults Reference](#defaults-reference)

---

## Features

- 🤖 **Multi-Agent Collaboration** — Coordinator delegates tasks to workers, automatically orchestrating multi-round conversations
- 🔄 **Multi-Team Switching** — Switch between different teams or invoke specific agents in a single prompt
- 🧠 **Long-Term Memory (RAG)** — Vector search via chromem-go automatically injects relevant memories into system prompts
- 🛠️ **18 Worker Tools** — Covering bash, file operations, remote execution, code interpreters, random, math, and more
- 🔌 **MCP Integration** — Supports local and remote MCP servers to extend agent capabilities
- 📋 **Skills System** — Reusable skill definitions, shareable across teams, with automatic skill detection
- 🎯 **Sidecar System** — Lightweight auxiliary LLM for skill matching and guard review
- 🛡️ **Guard System** — Rule-based output review (e.g., require tests, no profanity)
- ⚡ **Signal Control** — Ctrl+C for graceful shutdown, Ctrl+Z to inject additional prompts
- 📺 **TUI Mode** — Real-time Bubble Tea terminal UI for task tracking
- 🖥️ **PTY Handoff (experimental)** — Opt-in interactive terminal sessions with exclusive local human takeover
- 🔍 **Dry Run Mode** — Preview execution plan without running agents
- 📝 **Plan-First Mode** — Require agents to submit plans before execution
- 📊 **Report Generation** — Full execution report as markdown
- 🌐 **SSH Tool** — Enhanced with session management, error diagnostics, SCP support, SSH config integration, connection reuse, and audit logging
- ⚖️ **Multi-Model Judge** — Pick the best of N worker results via `--judge-model`
- 🎭 **Skeptic** — Challenge a single result before acceptance with adversarial verification
- 📈 **Escalation on Retry** — Automatically escalate to a stronger model when retries are needed
- 🔄 **DAG Task Scheduling** — Declare `on_failure` loops and `verify` commands for non-LLM task verification
- 🪞 **Reflexion** — Structured failure hints inform retries with deterministic local fallback
- 📓 **Task Journal** — Durable per-task results persisted to `workspace/logs/task_journal.jsonl`

---

## Installation & Build

```bash
# Build binary
go build ./cmd/hufu

# Run directly
go run ./cmd/hufu [prompt]
```

---

## Quick Start

### 0. Try it in 3 commands (no config)

```bash
# 1. Check that everything is wired up
hufu doctor

# 2. Use the built-in default team (no .agent-teams/ directory required)
hufu --default --model ollama/qwen3:8b "say hello"

# 3. Scaffold your own team (creates .agent-teams/my-team/ with helper.md)
hufu init my-team --model ollama/qwen3:8b
```

Other useful commands:
```bash
hufu list              # show all discoverable teams and their agents
hufu list my-team      # show one team in detail
hufu chat --agent-team my-team  # interactive REPL with that team
hufu chat --default    # interactive REPL with the built-in team
```

### 1. Start Ollama

Ensure Ollama is running at the default address:

```bash
ollama serve
```

### 2. Create a Team

Create an `.agent-teams/` directory in your project root and add a team definition:

```bash
mkdir -p .agent-teams/my-team
```

Create `team.yaml` (optional — the directory name is used as the team name when the file is absent):

```yaml
name: my-team
description: "My development team"
model: ollama/qwen3:8b
temperature: "0.2"
max-tokens: "16384"
skills: code-review,git-commit
```

Create `coordinator.md`:

```markdown
---
name: coordinator
description: "Task coordinator"
role: coordinator
model: ollama/qwen3:8b
---
You are the team coordinator, responsible for delegating tasks to team members and synthesizing results.
```

Create a worker agent, e.g. `developer.md`:

```markdown
---
name: developer
description: "Implementation specialist"
role: worker
tools: view,write,edit,bash,grep,glob,ls
model: ollama/qwen3:8b
---
You are a senior developer skilled at writing high-quality code.
```

#### Machine-readable team requirements

Optional `requires` contracts let hufu reject contradictory teams before any
agent or workspace action. They are domain-neutral and are checked by team
loading, `hufu team validate`, `hufu doctor`, and the resolved runtime policy:

```yaml
# team.yaml
requires:
  environment: [CI_TOKEN]
  paths: [/srv/project]

# worker.md frontmatter
requires:
  tools: [bash]
  environment: [WORKER_TOKEN]
  paths: [/srv/project/artifacts]
  interactive: false
  network: true
  plan-first: true
```

The checker validates delegation references, allowed/denied tool conflicts,
required tool availability, `force-mcp`, `no-net`, unattended interaction,
plan-first, environment variables, and allowed paths. It does not infer
mandatory behavior from free-form prompts.

### 3. Run a Task

```bash
# Specify prompt directly
go run ./cmd/hufu "Refactor the error handling logic in the auth module"

# Specify a team
go run ./cmd/hufu --agent-team my-team "Refactor the auth module"

# Interactive mode (entered when no prompt is provided)
go run ./cmd/hufu
```

---

## CLI Flags Reference

| Flag | Short | Type | Default | Description |
|------|-------|------|---------|-------------|
| `--provider-url` | — | `string` | "" (hufu.yaml or `http://localhost:11434/v1`) | Ollama or OpenAI-compatible API base URL |
| `--provider-api-key` | — | `string` | "" | Provider API key |
| `--verbose` | `-v` | `bool` | `false` | Show full agent text output in real-time |
| `--workspace` | `-w` | `string` | `""` (`<cwd>/workspace`) | Workspace directory path |
| `--new` | `-n` | `bool` | `false` | Archive old session and start fresh |
| `--temp` | `-t` | `bool` | `false` | Use a temporary directory as workspace |
| `--steps` | `-s` | `bool` | `false` | Pause for user confirmation before each batch of worker tasks |
| `--agent-team` | — | `string` | `""` | Agent team name to load |
| `--agent-team-search-path` | — | `string` | `""` | Team search paths (comma-separated), defaults to `.agent-teams/,~/.agent-teams/` |
| `--memory` | — | `bool` | `false` | Enable long-term memory (RAG vector search) |
| `--memory-model` | — | `string` | `""` | Embedding model for memory (default: `ollama/nomic-embed-text:latest`) |
| `--archive-memory` | — | `bool` | `false` | Archive session summary to memory and exit |
| `--show-history` | — | `bool` | `false` | Show previous session history on resume |
| `--dry-run` | — | `bool` | `false` | LLM-free preview of skill matching and available agents (no model calls, no agent execution) |
| `--tui` | — | `bool` | `false` | Show a Bubble Tea TUI for real-time task tracking |
| `--enable-pty-terminal` | — | `bool` | `false` | Eagerly initialize experimental Linux/macOS PTY handoff; `pty:true` starts it automatically |
| `--rbash` | — | `bool` | `false` | Use restricted bash (rbash) for the bash tool |
| `--no-net` | — | `bool` | `false` | Block all network access for agent subprocesses |
| `--force-mcp` | — | `bool` | `false` | Force MCP mode: disable built-in execution/network tools (bash, sudo, ssh, golang, lua, download, fetch, agentic_fetch), require MCP servers |
| `--direnv` | — | `bool` | `false` | Load `.envrc` / `.env` environment for the bash tool |
| `--think` | — | `bool` | `false` | Show coordinator decision reasoning |
| `--plan` | — | `bool` | `false` | Force plan-first mode: agents must submit plans before executing |
| `--auto-skills` | — | `bool` | `false` | Enable automatic skill detection via sidecar / LLM matching |
| `--report` | — | `bool` | `false` | Generate a full execution report as a markdown file |
| `--default` | — | `bool` | `false` | Use the built-in default team (coordinator + Helper); no `.agent-teams/` directory required (mutually exclusive with `--agent-team`). Discovers project skills from `.agents/skills/`, global skills from `~/.agents/skills/`, and respects `--skill` forced skills. |
| `--helper-tools` | — | `string` | `""` | Comma-separated extra tools for the default Helper worker when `--default` is set (e.g. `bash` or `bash,sudo,ssh`). Whitespace trimmed; empty entries dropped. Empty = baseline read-only toolset. |
| `--auto-approve` | — | `bool` | `false` | Automatically choose clearly safe `ask_user` options; dangerous or ambiguous choices still prompt the user |
| `--model` | — | `string` | `""` | Override default model for the active team (highest priority) |
| `--context-window` | — | `int` | `0` | Explicit positive model context capacity in tokens for pre-provider admission; `0` uses provider metadata or the model registry |
| `--temperature` | — | `string` | `""` | Override sampling temperature |
| `--max-tokens` | — | `string` | `""` | Override max output tokens |
| `--top-p` | — | `string` | `""` | Override top-p value |
| `--top-k` | — | `string` | `""` | Override top-k value |
| `--sidecar-model` | — | `string` | `""` | Override sidecar model used for skill matching (falls back to `--model` when not set) |
| `--guard-model` | — | `string` | `""` | Override guard model used for output review (falls back to `--model` when not set) |
| `--judge-model` | — | `string` | `""` | Override judge model used for multi-model result selection (falls back to sidecar when not set) |
| `--plan-reviewer-model` | — | `string` | `""` | Override plan reviewer model (falls back to `--model` when not set) |
| `--timeout` | — | `int64` | `0` | Override agent/coordinator timeout in seconds (e.g. `1800` for 30 min). `0` = use team/agent default. |
| `--max-rounds` | — | `int` | `0` | Override team.yaml max-rounds (coordinator round limit) |
| `--max-concurrent` | — | `int` | `0` | Override team.yaml max-concurrent (parallel worker dispatch) |
| `--max-steps` | — | `int` | `0` | Override team.yaml max-steps (per-agent step budget) |
| `--no-journal` | — | `bool` | `false` | Disable task journal (per-task results persisted to workspace/logs/task_journal.jsonl) |
| `--unattended` | — | `bool` | `false` | No human present: ask_user returns safe defaults, --steps/--tui disabled |
| `--max-duration` | — | `int64` | `0` | Budget: max total wall-clock seconds before forcing wrap-up |
| `--max-total-tokens` | — | `int64` | `0` | Budget: max cumulative LLM tokens before forcing wrap-up |
| `--auto-team` | — | `bool` | `false` | Auto-select the best team for the prompt |
| `--profile` | — | `string` | `""` | Apply a named flag bundle from hufu.yaml `profiles:` |
| `--quiet` | `-q` | `bool` | `false` | Suppress status output; print only the final result to stdout |
| `--output` | — | `string` | `""` | Final-result format: `text` (default) or `json` |
| `--template` | — | `string` | `""` | Load prompt template by name |
| `--fix` | — | `string` | `""` | Analyze previous execution data and suggest improvements |
| `--skill` | — | `[]string` | `nil` | Force-load specific skills (repeatable) |
| `--var` | — | `[]string` | `nil` | Set template variable `key=value` (repeatable) |
| `--var-file` | — | `[]string` | `nil` | Read template variables from a file (repeatable) |

### Usage Examples

```bash
# Use a custom Ollama address
go run ./cmd/hufu --provider-url http://192.168.1.100:11434/v1 "Analyze the code"

# Enable verbose mode to observe agent operations
go run ./cmd/hufu -v "Refactor the module"

# Specify a workspace directory
go run ./cmd/hufu -w /path/to/project "Fix the bug"

# Start a new session
go run ./cmd/hufu -n "New task"

# Use a temporary workspace
go run ./cmd/hufu -t "Quick test"

# Specify team and search paths
go run ./cmd/hufu --agent-team dev-team --agent-team-search-path "./teams,~/teams" "Develop feature"

# Enable memory
go run ./cmd/hufu --memory "Task that needs memory"

# Specify embedding model
go run ./cmd/hufu --memory-model mxbai-embed-large "Analyze documents"

# Archive session memory and exit
go run ./cmd/hufu --archive-memory

# TUI mode
go run ./cmd/hufu --tui "Refactor the module"

# Dry-run preview
go run ./cmd/hufu --dry-run "Refactor the module"

# Plan-first mode
go run ./cmd/hufu --plan "Implement a feature"

# Generate report
go run ./cmd/hufu --report "Build something"

# Step-by-step confirmation
go run ./cmd/hufu -s "Refactor the module"
```

---

## Prompt Syntax

`hufu` supports special syntax in prompts to switch teams or invoke specific agents:

| Syntax | Description | Example |
|--------|-------------|---------|
| `@<team-name> <task>` | Switch to the specified team and delegate the task | `@research-team Investigate API design` |
| `@<agent-name> <task>` | Directly invoke a specific agent | `@developer Implement the login feature` |
| Plain text | Passed to the current team's coordinator | `Refactor the auth module` |

### Multi-Team Switching Example

You can switch between multiple teams in a single prompt:

```bash
go run ./cmd/hufu "@research-team Investigate API design @dev-team Implement the feature @research-team Verify results"
```

This will sequentially:
1. Switch to `research-team` for investigation
2. Switch to `dev-team` for implementation
3. Switch back to `research-team` to verify results

### Advanced Agentic Patterns

`hufu` is designed to support modern agentic workflows seamlessly without hardcoding complex orchestration logic. By combining tools like `create_skill` and Prompt Chaining (`|`), you can achieve advanced patterns:

#### 1. Hierarchical / Dynamic Delegation (Pattern 2)
Agents can dynamically delegate subtasks or spawn specialized helpers during execution.
Instead of the Coordinator manually planning everything upfront, you can prompt an agent to write its own sub-tools:
```bash
go run ./cmd/hufu "@coder You are building a complex feature. If you need specialized help, use the create_skill tool to write a bash script that invokes 'hufu @specialist' as a subtask, then execute it."
```
This enables the agent to recursively spawn CLI processes or create skills on the fly for deep hierarchical delegation.

#### 2. Multi-Agent Debate / Cross-Examination (Pattern 3)
For high-stakes tasks (like code reviews or architecture decisions), you can force two different agents to debate and refine a solution until consensus is reached.
Use the Prompt Chaining syntax (`|`) and `{{PREV_RESULT}}` to chain multiple agents into a debate loop:
```bash
go run ./cmd/hufu "@generator Propose a system architecture | @auditor Critique the architecture proposed by generator and point out flaws: {{PREV_RESULT}} | @generator Based on the critique: {{PREV_RESULT}}, revise the architecture"
```

---

## Common Use Cases

### 1. Code Review with Multi-Model Judge

When you have multiple models available, use the judge to pick the best code review result:

```yaml
# team.yaml
model-list:
  - name: qwen3:1b
    provider: ollama
  - name: qwen3:8b
    provider: ollama
judge-model: qwen3:1b  # Cheap model for judging
```

```bash
# Run code review with multiple models, judge picks the best
go run ./cmd/hufu --agent-team code-review "Review the authentication module"
```

### 2. High-Stakes Decisions with Skeptic

For critical decisions, use adversarial verification to challenge the result:

```bash
# Instruct the coordinator to enforce adversarial verification
go run ./cmd/hufu --agent-team arch-team \
  "Design the database schema. Ensure the result is rigorously verified by 3 skeptics."
```

### 3. Escalating Retry for Stubborn Tasks

Enable escalation when tasks need stronger models on retry:

```yaml
# team.yaml
escalate-on-retry: true
model-list:
  - name: qwen3:1b
    provider: ollama
  - name: qwen3:8b
    provider: ollama
  - name: qwen3:30b
    provider: ollama
```

### 4. Verify Deliverables with Shell Commands

Ensure tasks produce actual artifacts, not just claims:

```bash
# Provide clear instructions and verification criteria in your prompt
go run ./cmd/hufu --agent-team dev-team \
  "Implement the login feature. Set a verify command 'go test ./tests/login/...' to ensure it works."
```
### 5. Reflexion for Blind Retries

Even without a sidecar, retries get structured hints:

```bash
# Reflexion works without sidecar - deterministic error classification
go run ./cmd/hufu --agent-team dev-team "Implement complex feature"
# If it fails, reflexion classifies: timeout / missing file / permission error
```

### 6. Memory-Augmented Development

Persist learnings across sessions:

```bash
# Enable memory to search past sessions
go run ./cmd/hufu --memory "Refactor the API layer"

# Later sessions can query relevant memories
go run ./cmd/hufu --memory "How did we handle auth errors previously?"
```

### 7. Unattended Batch Processing

Run jobs with no human watching:

```bash
# Full unattended configuration
go run ./cmd/hufu \
  --unattended \
  --max-duration 3600 \
  --max-total-tokens 500000 \
  --no-journal \
  --profile batch \
  --agent-team pipeline \
  "Process all pending pull requests"
```

### 8. Plan-First for Complex Features

Require agents to submit plans before acting:

```bash
# Agents must propose plans first
go run ./cmd/hufu --plan "Implement a distributed caching layer"
```

### 9. Dry Run for Safe Exploration

Preview what would happen without executing:

```bash
# No LLM calls, no agent execution - just preview
go run ./cmd/hufu --dry-run --agent-team dev-team "Refactor the auth module"
```

### 10. Guardrails for Code Quality

Add output guardrails per agent:

```markdown
# researcher.md
---
name: researcher
guard:
  - require-tests
  - no-profanity
---
```

### 11. TUI for Real-Time Monitoring

Watch tasks progress in real-time:

```bash
# TUI mode shows task board, logs, skill usage
go run ./cmd/hufu --tui "Implement the new feature"
```

---

## Interactive Mode

When no prompt is provided and stdin is empty, `hufu` enters interactive mode:

1. **If no team can be inferred** — Displays a team selection menu for the user to choose
2. **After selecting a team** — Prompts the user to enter a task description

```bash
# Enter interactive mode
go run ./cmd/hufu

# Example output:
# ? Select a team:
#   > my-team
#     research-team
#     dev-team
#
# ? Enter task description:
```

---

## Team Configuration

The team configuration file defines the team's overall behavior and default parameters. Complete configuration reference:

```yaml
# === Required Fields ===
name: my-team                    # Team name (required)

# === Optional Fields ===
description: "My development team"  # Team description

# === Execution Control ===
max-rounds: 10                   # Maximum coordination rounds (default: 10)
max-steps: 30                    # Agent default max steps (default: 30)
timeout: 600                     # Timeout in seconds (default: 600)
max-retries: 2                   # Maximum retries (default: 2)
max-concurrent: 8                # Maximum concurrent worker tasks (default: 8)

# === Workspace ===
workspace: workspace             # Workspace directory (default: "workspace")

# === Model Settings ===
model: ollama/qwen3:8b           # Default model name
temperature: "0.2"               # Temperature value
max-tokens: "16384"               # Maximum output tokens
top-p: "0.9"                     # Top P value
top-k: "40"                      # Top K value

# === Provider ===
provider-url: http://localhost:11434/v1  # Provider URL override
provider-api-key: ""                      # Provider API key override

# === Multi-Provider Pool ===
providers:
  openai:
    url: https://api.openai.com/v1
    key: $OPENAI_API_KEY
    models: [gpt-4o, gpt-4-turbo]
    aliases:
      gpt-4: gpt-4o

# === Model List ===
model-list:
  - name: qwen3:8b
    provider: ollama
  - name: gpt-4o
    provider: openai

# === Sidecar / Guard / Judge Models ===
sidecar-model: qwen3:1b          # Lightweight model for skill matching
guard-model: qwen3:8b            # Model for guard / review tasks
judge-model: qwen3:1b            # Model for multi-model result selection (defaults to sidecar)
plan-reviewer-model: qwen3:8b   # Model for plan review tasks

# === Escalation ===
escalate-on-retry: false        # Escalate to next stronger model on retry (requires model-list)

# === Skills ===
skills: code-review,git-commit  # Skills to include
skills-exclude: debug            # Skills to exclude
auto-skills: false              # Enable automatic skill detection
auto-report: false              # Always write workspace/<team>/report.md after a team run

# === Outcome-driven Memory ===
memory-learning:
  mode: off                     # off | observe | shadow | active
  policy-version: memory-policy-v1
  prior-alpha: 1.0
  prior-beta: 1.0
  utility-percentile: 0.10
  max-credit-per-signal: 1.0
  min-confirmed-support: 2
  min-independent-tasks: 2
  max-harm-rate: 0.0

# === Security ===
allowed-paths: ["/home/user/projects", "/tmp"]  # Allowed file system paths
restricted-path: "/etc"                           # Restricted path
no-net: false                                     # Block network access
force-mcp: false                                  # Force MCP mode
auto-approve: false                               # Auto-select clearly safe ask_user options
shell: bash                                       # Default shell for MCP tools (searched from PATH)

# === MCP Tools (Agent-level) ===
# Defined in agent .md files, not team.yml
# See Agent Configuration section below

# === Template Variables ===
vars:
  project_name: "hufu"
  author: "anomalyco"

# === Notifications ===
notify:
  type: webhook
  url: "https://hooks.example.com/agent"

# === MCP Servers ===
mcp-servers:
  filesystem:
    type: local
    command: ["npx", "-y", "@modelcontextprotocol/server-filesystem", "/path"]
  remote-api:
    type: remote
    url: "https://mcp-server.example.com/api"
    allowedTools: ["search", "query"]
```

> **Note**: `temperature`, `max-tokens`, `top-p`, and `top-k` values are represented as strings in YAML.

Outcome-driven memory is opt-in. `observe` records attempt-scoped exposure and validated `TaskResult.memory_uses` without changing selection; `shadow` also records reinforced ranking traces; only `active` changes prompt selection. Inspect the content-free projection with `hufu context outcomes <id>`, `hufu context explain-memory <id> --project <id> --query <goal>`, `hufu context doctor --learning`, and rebuild it from the hash-chained event ledger with `hufu context rebuild --aggregates`.

Confirmed persistent context with repeated verified support can be promoted through an explicit human review flow:

```bash
hufu context promotion analyze --workspace workspace --project my-project --team my-team
hufu context promotion show promo-abc123 --workspace workspace --project my-project --team my-team --show-content
hufu context promotion approve promo-abc123 --workspace workspace --project my-project --team my-team
hufu context promotion apply promo-abc123 --workspace workspace --project my-project --team my-team
```

Analysis never changes team Markdown or installed skills, approval never applies a draft, and `apply` fails closed if its source evidence or target changed. Use `reject --reason ...` to close a proposal or `edit --draft-file ...` while it is still proposed. Team-policy promotion appends policy only to the team's single coordinator/orchestrator; it is not a team-wide runtime security contract. Proposed and rejected drafts are never loaded by the runtime.

See the [L3/L4 outcome-driven memory implementation specification](docs/hufu-outcome-driven-memory-hf-mem4-implementation-spec.md) for contracts, rollout gates, and acceptance criteria.
See the [LTM promotion specification](docs/hufu-ltm-promotion-spec.md) for the promotion lifecycle, safety boundaries, and acceptance matrix.

---

## Agent .md File Format

Each agent is defined by a Markdown file with YAML frontmatter and a system prompt as the body:

```markdown
---
name: developer
description: Implementation specialist
role: worker
tools: view,write,edit,multiedit,bash,grep,glob,ls
skills: code-review
guard:
  - require-tests
  - no-profanity
model: ollama/qwen3:8b
temperature: "0.2"
max-tokens: "16384"
top-p: "0.9"
top-k: "40"
timeout: 300
max-retries: 2
max-steps: 50
provider-url: http://localhost:11434/v1
---
You are a senior developer skilled at writing high-quality, maintainable code.
Please follow best practices and ensure proper error handling.
```

### Frontmatter Fields

| Field | Required | Default | Description |
|-------|----------|---------|-------------|
| `name` | ✅ | — | Agent name, used for `@<name>` invocation |
| `description` | ❌ | — | Agent description |
| `role` | ❌ | `worker` | Agent role (`worker` or `coordinator`) |
| `tools` | ❌ | — | Available tools list (comma-separated) |
| `skills` | ❌ | — | Skills to load (comma-separated) |
| `guard` | ❌ | — | Guard rules (YAML list) |
| `model` | ❌ | Team default | LLM model to use |
| `temperature` | ❌ | Team default | Temperature value |
| `max-tokens` | ❌ | Team default | Maximum output tokens |
| `top-p` | ❌ | Team default | Top P value |
| `top-k` | ❌ | Team default | Top K value |
| `timeout` | ❌ | Team default | Timeout in seconds |
| `max-retries` | ❌ | `-1` (use team default) | Maximum retries |
| `max-steps` | ❌ | Team default | Maximum execution steps |
| `provider-url` | ❌ | Team default | Provider URL override |
| `provider-api-key` | ❌ | Team default | API key override |
| `allowed-paths` | ❌ | Team default | Allowed file system paths |
| `restricted-path` | ❌ | Team default | Restricted file system path |
| `no-net` | ❌ | Team default | Block network access |
| `force-mcp` | ❌ | Team default | Force MCP mode (disable execution/network tools) |
| `shell` | ❌ | Team default | Default shell for agent's MCP tools (e.g., `bash`, `zsh`, `nu`, or full path) |
| `mcp-tools` | ❌ | — | Custom MCP tools (dict format: `{tool-name: {cmd, desc, inputs, shell, dir}}`) |
| `requires` | ❌ | — | Machine-readable prerequisites: tools, environment, paths, interactive, network, plan-first |

> **Important**: The default value for `max-retries` is `-1`, meaning the team default is used; if explicitly set, it overrides the team default.

### MCP Tools (`mcp-tools`)

Define custom MCP tools for an agent. Each tool executes shell commands with parameter substitution:

```yaml
mcp-tools:
  run-tests:
    cmd: go test ./...
    desc: Run Go tests
    inputs: [package]

  build:
    cmd: go build -o /tmp/app ./...
    desc: Build the application

  calc:
    cmd: print ($env.V1 + $env.V2)
    desc: Calculate sum using nushell
    inputs: [a, b]
    shell: nu
```

**Parameter Mapping:**
- **Named parameters**: `url` → `$URL` environment variable
- **Positional parameters**: 1st input → `$V1`, 2nd → `$V2`, etc.
- **Shell priority**: `tool.shell` > `agent.shell` > `team.shell` > `hufu.yaml shell` > `bash` (default)

**Supported shells:** `bash`, `sh`, `zsh`, `fish`, `nu` (nushell), or any shell in PATH.

---

## MaxSteps Priority

When `max-steps` is configured in multiple places, the priority from highest to lowest is:

| Priority | Source | Description |
|----------|--------|-------------|
| 1 | `AgentConfig.MaxSteps` | Explicit override in code |
| 2 | `AgentDef.MaxSteps` | Setting in the agent `.md` file |
| 3 | `TeamConfig.MaxSteps` | Setting in `team.yml` |
| 4 | `DefaultMaxSteps` | Default: 30 for workers, 20 for coordinators |

---

## Team Directory Structure

Team files are organized in subdirectories under the search paths. Default search paths are `.agent-teams/` and `~/.agent-teams/`:

```
.agent-teams/
├── my-team/
│   ├── team.yaml              # Team configuration
│   ├── coordinator.md         # Coordinator agent definition
│   ├── researcher.md          # Worker agent definition
│   ├── writer.md              # Worker agent definition
│   └── .agents/
│       └── skills/
│           └── code-review/
│               └── SKILL.md   # Skill definition
```

### Search Paths

Team search paths are controlled by `--agent-team-search-path`:

```bash
# Default search paths
# .agent-teams/
# ~/.agent-teams/

# Custom search paths
go run ./cmd/hufu --agent-team-search-path "./teams,~/teams" "Task"
```

---

## Skills System

Skills are reusable definitions that enable agents to follow specific workflows.

### SKILL.md Format

```markdown
---
name: code-review
description: Perform a systematic code review
allowed-tools: view,grep,glob,bash
---
# Code Review

## Steps

1. Use `glob` to find all relevant source files
2. Use `view` to read each file one by one
3. Use `grep` to search for potential issue patterns
4. Summarize findings and suggest improvements
```

### Skill Search Paths

Skills are searched in the following order:

1. `<teamDir>/skills/<skill-name>/SKILL.md` — Team-specific skills (changed from `<teamDir>/.agents/skills/`)
2. `<current-directory>/.agents/skills/<skill-name>/SKILL.md` — Project skills
3. `~/.agents/skills/<skill-name>/SKILL.md` — Global skills (unchanged)

### Using Skills in Teams

```yaml
# team.yml
skills: code-review,git-commit    # Include skills
skills-exclude: debug             # Exclude skills
shell: bash                       # Default shell for MCP tools
```

```markdown
---
name: developer
tools: view,write,edit,bash
skills: code-review               # Agent-level skill
shell: bash                       # Agent-level shell
mcp-tools:
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
```

> **Note**: `allowed-tools` defines a subset of tools the skill can use, which is intersected with the agent's own `tools`.

---

## Workspace Layout

`hufu` maintains a structured file system in the workspace directory for inter-agent communication and state tracking:

```
workspace/
├── tasks/               # Unified task files (merged inbox + outbox)
│   └── {team-name}/
│       └── {agent-name}/
│           └── {timestamp}.md
├── compaction_history.json  # Structured compaction history (13-section summaries)
├── shared/
│   └── skills/          # Copied SKILL.md files
├── status/              # Agent status files
├── history/             # Archived session files
├── logs/                # System and debug logs
│   └── task_journal.jsonl  # Per-task durable results (journal)
├── session.json         # Structured session data
├── chat_history.md      # Human-readable conversation transcript
└── execution_trace.log  # Detailed execution trace log
```

### Directory Descriptions

| Directory/File | Description |
|----------------|-------------|
| `tasks/{team-name}/{agent-name}/` | Per-agent task files (task description, status, results) |
| `shared/skills/` | SKILL.md files copied from skill definitions |
| `status/` | Agent status tracking files |
| `history/` | Archived historical session files |
| `logs/` | System and debug logs |
| `logs/task_journal.jsonl` | Per-task durable results (journal) |
| `compaction_history.json` | Structured compaction summary history |
| `session.json` | Structured session data (machine-readable) |
| `chat_history.md` | Human-readable conversation transcript |
| `execution_trace.log` | Detailed execution trace log (TUI mode only) |

---

## Memory System (RAG)

`hufu` includes a built-in long-term memory system using RAG (Retrieval-Augmented Generation), enabling agents to retain and query important information across sessions.

### Architecture

| Component | Description |
|-----------|-------------|
| **Vector Store** | chromem-go (in-process, file-based) |
| **Embedding** | Ollama embeddings |
| **Storage Location** | `~/.local/share/hufu/memory/<projectHash>/` |
| **Default Embedding Model** | `ollama/nomic-embed-text:latest` |

### How It Works

- **Automatic Querying**: Relevant memories are automatically injected into agent system prompts
- **Archiving**: Session summaries are stored to memory when a session ends

### Memory Tools

The default model-facing memory surface is read-only. Workers return findings,
decisions, open questions, verification, and artifacts through the typed
`TaskResult` contract; the runtime reduces that evidence into canonical session
context and proposes persistent candidates when eligible.

#### `memory_query`

Search long-term memory for knowledge relevant to the query.

```
memory_query(
  query: "API design decisions",               # required
  n: 5,                                        # optional, number of results (default 5, max 20)
  category: "architecture"                     # optional, category filter
)
```

### Configuration Priority

Memory-related configuration priority:

```
CLI flag > hufu.yaml > Defaults
```

### Usage Examples

```bash
# Enable memory (enabled by default)
go run ./cmd/hufu --memory "Analyze code architecture"

# Disable memory
go run ./cmd/hufu --memory=false "One-off task"

# Specify embedding model
go run ./cmd/hufu --memory-model mxbai-embed-large "Analyze documents"

# Archive session memory and exit
go run ./cmd/hufu --archive-memory
```

---

## MCP Configuration

`hufu` supports Model Context Protocol (MCP) servers, enabling agents to use external tools and resources. Add an `mcp-servers` section to `team.yml`:

```yaml
mcp-servers:
  filesystem:
    type: local
    command: ["npx", "-y", "@modelcontextprotocol/server-filesystem", "/path/to/dir"]
  remote-api:
    type: remote
    url: "https://mcp-server.example.com/api"
    allowedTools: ["search", "query"]
```

### MCPServerConfig Fields

| Field | Type | Description |
|-------|------|-------------|
| `type` | `string` | `"local"` or `"remote"` (auto-inferred from `command`/`url`) |
| `command` | `[]string` | Launch command for local server |
| `environment` | `map[string]string` | Environment variables |
| `url` | `string` | URL for remote server |
| `allowedTools` | `[]string` | Tool whitelist |
| `excludedTools` | `[]string` | Tool blacklist |
| `noOAuth` | `bool` | Disable OAuth |

### MCP Tool Naming

MCP tools in `hufu` use the following prefix format:

```
<serverName>__<toolName>
```

For example: `filesystem__read_file`, `remote-api__search`

### Security Restrictions

The following environment variables are blocked and cannot be set via `environment`:

- `LD_PRELOAD`
- `LD_LIBRARY_PATH`
- `DYLD_INSERT_LIBRARIES`
- `DYLD_LIBRARY_PATH`
- `__AFL_PRELOAD`

### Local MCP Server Example

```yaml
mcp-servers:
  filesystem:
    command: ["npx", "-y", "@modelcontextprotocol/server-filesystem", "/home/user/projects"]
    environment:
      NODE_ENV: production
  git:
    command: ["npx", "-y", "@modelcontextprotocol/server-git", "/home/user/repo"]
```

### Remote MCP Server Example

```yaml
mcp-servers:
  search-api:
    url: "https://search.example.com/mcp"
    allowedTools: ["search", "query", "suggest"]
  analytics:
    url: "https://analytics.example.com/mcp"
    excludedTools: ["admin_delete"]
```

---

## Multi-Provider Support

`hufu` supports multiple LLM providers simultaneously via the `providers` field in `team.yml`:

```yaml
providers:
  openai:
    url: https://api.openai.com/v1
    key: $OPENAI_API_KEY
    models: [gpt-4o, gpt-4-turbo]
    aliases:
      gpt-4: gpt-4o
  local:
    url: http://localhost:11434/v1
    key: ollama
    models: [qwen3:8b]
```

Agents can specify `provider-url` and `provider-api-key` individually in their `.md` frontmatter, or use the team's default.

---

## Sidecar System

The sidecar is a lightweight auxiliary LLM used for tasks that should not consume the main model's context window.

### Use Cases

- **Skill matching** — Match prompts to relevant skills
- **Guard review** — Review agent outputs against guard rules
- **Plan review** — Autonomous plan review before execution
- **Judge** — Pick the best of N worker results (defaults to sidecar model)
- **Skeptic** — Challenge a single result before acceptance

### Configuration

```yaml
sidecar-model: qwen3:1b   # Lightweight model for skill matching
guard-model: qwen3:8b     # Model for guard / review tasks
judge-model: qwen3:1b     # Model for multi-model result selection (defaults to sidecar)
```

---

## Guard System

The Guard system reviews agent outputs against configurable rules. Rules are defined per-agent via the `guard` field in `.md` frontmatter:

```yaml
guard:
  - require-tests
  - no-profanity
```

When a guard is triggered, the output is passed to the `guard-model` sidecar for review. If the review fails, the agent is asked to correct the output.

### Supported Rules

| Rule | Description |
|------|-------------|
| `require-tests` | Ensure test files are included in code changes |
| `no-profanity` | Block profanity |

You can implement custom rules by creating guard skill definitions in `skills/guard-<name>/SKILL.md` in your team directory.

### Skeptic (Adversarial Verification)

Beyond guard rules, you can request adversarial verification in your prompt. The Coordinator will instruct the skeptic to challenge the result before acceptance:

```bash
go run ./cmd/hufu --agent-team arch-team \
  "Implement the feature and use 2 skeptics to rigorously verify the design."
```

If the skeptic votes fail, the task is rejected and retried.

---

## Worker Tools Reference

`hufu` provides 18 worker tools, along with 4 always-included tools and 5 coordinator tools:

### 18 Worker Tools

| Tool | Description |
|------|-------------|
| `bash` | Execute bash commands (timeout: 120s, max 600s) |
| `sudo` | Execute commands with root privileges via sudo |
| `ssh` | Execute commands on remote hosts via SSH |
| `view` | Read file contents (with line numbers) |
| `write` | Write file contents, auto-creates directories |
| `edit` | Edit files by replacing exact text |
| `multiedit` | Atomically apply multiple edit operations |
| `grep` | Search file contents using regular expressions |
| `glob` | Search files using glob patterns |
| `ls` | List directory contents in a tree structure |
| `lua` | Execute Lua code in a sandbox |
| `golang` | Execute Go code via the yaegi interpreter |
| `javascript` | Deterministic structured-data computation in an isolated goja sandbox (no fs/network/process/timers) |
| `ask_user` | Ask the user a question (multiple choice / free text) |
| `download` | Download a file from a URL |
| `fetch` | Fetch URL content (text/markdown/html) |
| `agentic_fetch` | Fetch and analyze URL content |
| `random` | Generate random numbers / UUIDs |
| `math` | Evaluate mathematical expressions |

### Always-included Tools (available to all agents)

| Tool | Description |
|------|-------------|
| `agent` | Delegate tasks to other agents |
| `todo` | Manage task lists |
| `memory_query` | Search long-term memory |

### Coordinator Tools

| Tool | Description |
|------|-------------|
| `agent` | Delegate tasks to worker agents |
| `finish` | End the coordination process |
| `load_skill` | Load a skill definition |
| `save_skill` | Save a skill definition |
| `ask_user` | Ask the user a question |

### Specifying Tools in Agent .md

```markdown
---
name: developer
tools: view,write,edit,bash,grep,glob,ls
---
```

> **Note**: Always-included tools (`agent`, `todo`, `memory_query`) do not need
> to be specified in `tools`; they are automatically included. The deprecated
> `stm_write`, `ltm_update`, and `memory_save` compatibility aliases are
> canonical-only and default-disabled. A team or one agent must explicitly list
> an exact alias name to opt in; `tools: all`, an empty list, and template grants
> do not opt in. Persistent aliases create candidates only, never confirmed truth.

---

## Signal Handling

`hufu` supports the following signal operations:

| Signal | Key | Behavior |
|--------|-----|----------|
| `SIGINT` | `Ctrl+C` | First press: enter wrap-up mode (graceful shutdown); second press: force exit |
| `SIGTSTP` | `Ctrl+Z` | Inject an additional prompt via readline |
| `SIGUSR1` | — | Inject an additional prompt (alternative method) |

### Usage Scenarios

```bash
# Press Ctrl+C while running:
# First press → Agent attempts to complete current work and summarize results
# Second press → Force immediate exit

# Press Ctrl+Z while running:
# Enters prompt input, allowing injection of additional instructions
# e.g., "Please focus on performance optimization"

# Send SIGUSR1:
kill -USR1 <hufu-pid>
# Also injects an additional prompt
```

---

## Loop Detection

`hufu` includes a loop detection mechanism to prevent agents from repeatedly delegating the same task to the same agent:

- **Tracking**: Uses `agent:task` as the key to track delegated tasks
- **Detection Condition**: Triggers when the same task is delegated to the same agent multiple times
- **Warning Behavior**: Emits a `loop_warning` status event
- **Warning Content**: Includes the task description, agent name, and repetition count

---

## Session Management

`hufu` maintains independent session data for each team:

| Scenario | Behavior |
|----------|----------|
| Team switch | Saves the previous team's session (`SaveSessionMD`) |
| `Ctrl+C` exit | Saves the current session |
| `--new` flag | Archives the old session and starts a new one |
| `--temp` flag | Uses a temporary directory as workspace |

### Session Files

- `session.json` — Structured session data (machine-readable)
- `chat_history.md` — Human-readable conversation transcript
- `execution_trace.log` — Detailed execution trace log (TUI mode only)
- `history/` — Archived historical session files

---

## TUI Mode

Enable a real-time Bubble Tea terminal UI:

```bash
# TUI mode with task tracking
go run ./cmd/hufu --tui "Refactor the auth module"
```

The TUI displays:
- **Task list** — Pending, in progress, done, and error states with visual indicators
- **Tool calls / results** — Live agent tool execution logging
- **Skill usage** — Track which skills are loaded during the session
- **Wrap-up indicator** — Visual cue when the coordinator is finishing

> **Note**: `--tui` and `--steps` cannot be used together.

### Interactive PTY takeover (experimental)

The stateful `terminal` tool starts the local PTY broker automatically when an
agent explicitly requests `pty: true`. This does not change the ordinary
`bash` tool. `--enable-pty-terminal` remains available if you want the broker
created eagerly at startup.

```bash
hufu --tui --agent-team ops "run the interactive wizard"
# Or from another local terminal while hufu is still running:
hufu terminal attach <session-id> --workspace ./workspace
```

The TUI shows an attach hint when an agent starts a PTY; open that task's detail
view and press `t`. An attached user exclusively owns input, so the task and
active model round pause until `Ctrl-]` explicitly returns control. An
unexpected attach-client disconnect leaves the task paused; reconnect and
detach normally to resume. The local Unix socket is same-UID only, exists only
while its owning hufu process runs, and is unavailable in `--unattended` mode.

---

## Dry Run Mode

Preview execution plan without LLM calls:

```bash
# Preview skill matching and delegation
go run ./cmd/hufu --dry-run "Refactor the module"
```

Outputs (derived purely from team config, no LLM involved):
- Team name, model, sidecar model
- User prompt
- All available agents (name, role, model, tools, skills)
- All discovered skills
- Skills whose name/description keywords match the user prompt
- Note: the actual task delegation is **not** planned here; that requires the LLM. Dry-run only shows the agents that *could* be used.

---

## Plan-First Mode

Require agents to submit a plan before execution:

```bash
go run ./cmd/hufu --plan "Implement a feature"
```

---

## Multi-Model Judge

When a task can be executed by multiple models (via `model-list`), the judge picks the best result:

```bash
# Use a specific judge model
go run ./cmd/hufu --judge-model qwen3:1b "Implement a feature"
```

The judge resolves to the sidecar model by default to keep judging cheap. The main model is not used for judging to avoid doubling main-model cost.

---

## Skeptic (Adversarial Verification)

The skeptic challenges a single result before it is accepted. You can specify the need for adversarial verification directly in your prompt:

```bash
# Instruct the coordinator to delegate with adversarial verification
go run ./cmd/hufu --agent-team arch-team "Implement login. Request 2 skeptic votes for verification."
```

This runs 2 skeptic challenges; if any fail, the task is rejected.

---

## Escalation on Retry

When `escalate-on-retry` is enabled in team.yaml, a retry automatically escalates to the next stronger model in the `model-list`:

```yaml
escalate-on-retry: true
model-list:
  - name: qwen3:1b
    provider: ollama
  - name: qwen3:8b
    provider: ollama
```

---

## DAG Task Scheduling

DAG task scheduling is dynamically driven by your prompts. You don't need to write configuration files; simply instruct the Coordinator to set up dependencies, pipelines, or verification steps when delegating tasks. The Coordinator will automatically map these instructions into a parallel execution graph.

For example, to enforce a specific sequence and objective verification:

```bash
# Instruct the coordinator to set up a dependency chain and a verify command
go run ./cmd/hufu --agent-team dev-team \
  "First ask the researcher to gather auth best practices. Once done, ask the coder to implement the login feature. For the coder, set a verify command 'go test ./tests/login/...' to ensure it works."
```

### Verify Command

The `verify` instruction tells the Coordinator to attach a shell command check to a task. It runs after the agent reports success but before the task is marked done. A non-zero exit fails the task and triggers the retry path, preventing agents from falsely claiming completion.

---

## Reflexion (Failure Hints)

When a task fails, reflexion produces structured hints that inform retries:

- **With sidecar**: Uses the sidecar model to analyze the failure and suggest corrections
- **Without sidecar**: Falls back to deterministic local hints (timeout, missing file, permission, verification, step exhaustion, duplicate)

This ensures retries are never blind even without a sidecar configured.

---

## Task Journal

Per-task results are durably persisted to `workspace/logs/task_journal.jsonl`:

```bash
# Disable task journal
go run ./cmd/hufu --no-journal "Task"
```

Each line is a JSON object with task metadata and results, useful for audit trails and crash recovery.

---

## Report Generation

Generate a full markdown report after execution:

```bash
go run ./cmd/hufu --report "Refactor the module"
```

The report includes:
- Task delegation summary
- Agent execution logs
- Tool and skill usage statistics
- Performance metrics

---

## Idle Warning

`hufu` includes a built-in idle detection mechanism:

- **Timer**: 30-second idle timer, resets on each status event
- **Trigger**: Outputs an idle warning to stderr after 30 seconds of no activity
- **Purpose**: Alerts the user that an agent may be stuck or waiting for input

---

## Configuration File (hufu.yaml)

`hufu` supports a YAML configuration file, loaded from the following locations in priority order:

| Priority | Path | Description |
|----------|------|-------------|
| 1 | `~/.config/hufu/hufu.yaml` | Global configuration |
| 2 | `./hufu.yaml` | Project configuration |

### Configuration File Example

```yaml
provider-url: http://localhost:11434/v1
embedding-model: ollama/nomic-embed-text:latest
```

### Configuration Priority

Overall configuration priority:

```
CLI flag > hufu.yaml > Defaults
```

---

## Defaults Reference

Complete reference for all default values, including their source files:

### General Settings

| Setting | Default | Source |
|---------|---------|--------|
| Provider URL | `http://localhost:11434/v1` | `agent.go` |
| Embedding Model | `ollama/nomic-embed-text:latest` | `config.go` |

### Agent Settings

| Setting | Default | Source |
|---------|---------|--------|
| Max Steps (workers) | 30 | `agent.go` |
| Max Steps (coordinators) | 20 | `agent.go` |
| Agent Default Role | `worker` | `parse.go` |

### Team Settings

| Setting | Default | Source |
|---------|---------|--------|
| Team Max Rounds | 10 | `parse.go` |
| Team Timeout | 600s | `parse.go` |
| Team Max Retries | 2 | `parse.go` |

### Tool Timeouts & Limits

| Setting | Default | Source |
|---------|---------|--------|
| Bash Timeout | 120s | `bash.go` |
| Max Bash Timeout | 600s | `bash.go` |
| SSH Timeout | 30s | `ssh.go` |
| Lua Timeout | 120s | `lua.go` |
| Golang Timeout | 120s | `golang.go` |
| Download Timeout | 300s | `download.go` |
| Fetch Timeout | 30s | `fetch.go` |
| MCP Timeout | 30s | `manager.go` |

### Tool Output Limits

| Setting | Default | Source |
|---------|---------|--------|
| View Limit | 2000 lines | `view.go` |
| Grep Limit | 100 matches | `grep.go` |
| Glob Limit | 100 results | `glob.go` |
| LS Limit | 1000 items | `ls.go` |

---

## Complete Usage Examples

### Basic Task Execution

```bash
# Run a task with the default team
go run ./cmd/hufu "Refactor the error handling logic in the auth module"
```

### Specifying a Team with Verbose Mode

```bash
# Specify a team and observe agent operations
go run ./cmd/hufu --agent-team dev-team -v "Implement the user login feature"
```

### Multi-Team Collaboration

```bash
# Have the research team investigate first, then the dev team implement
go run ./cmd/hufu "@research-team Investigate the best authentication approach @dev-team Implement based on the research"
```

### Direct Agent Invocation

```bash
# Directly invoke the developer agent
go run ./cmd/hufu "@developer Fix the memory leak in login.go"
```

### Using the Memory System

```bash
# Enable memory and specify embedding model
go run ./cmd/hufu --memory --memory-model mxbai-embed-large "Analyze the project architecture"

# Archive session memory
go run ./cmd/hufu --archive-memory
```

### Using a Temporary Workspace

```bash
# Quick experiment without affecting the existing workspace
go run ./cmd/hufu -t "Test a new algorithm idea"
```

### Starting a New Session

```bash
# Archive the old session and start fresh
go run ./cmd/hufu -n "A completely new task"
```

### Custom Search Paths

```bash
# Specify multiple team search paths
go run ./cmd/hufu --agent-team-search-path "./teams,~/projects/teams,/opt/teams" "Task"
```

---

## License

This project is licensed under the Apache License 2.0 - see the [LICENSE](LICENSE) file for details.

---

## Related Links

- **Ollama**: [https://ollama.com](https://ollama.com)
- **Cobra**: [https://github.com/spf13/cobra](https://github.com/spf13/cobra)
- **ChromaDB**: [https://www.trychroma.com](https://www.trychroma.com)
- **MCP**: [https://modelcontextprotocol.io](https://modelcontextprotocol.io)
