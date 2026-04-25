# AGENTS.md

## Project Overview

**agent-team-cli** is a Go CLI tool that orchestrates teams of LLM agents (via Ollama) to collaboratively accomplish tasks. A coordinator/orchestrator agent delegates work to worker agents, which execute tasks in parallel using file and shell tools. MCP (Model Context Protocol) servers can be attached as additional tool providers.

- **Module**: `github.com/anomalyco/agent-team-cli`
- **Go version**: 1.26.2
- **CLI framework**: cobra
- **LLM framework**: `charm.land/fantasy` (Charm's agent/LLM abstraction)
- **MCP client**: `github.com/mark3labs/mcp-go`

## Build & Run

```bash
go build ./cmd/agent-team-cli          # Build binary
go build -o agent-team-cli ./cmd/agent-team-cli  # Build with custom name
go run ./cmd/agent-team-cli <team-dir> [prompt]  # Run directly
go vet ./...                            # Lint
```

No Makefile, CI, or test files exist in this project.

### CLI Usage

```
agent-team-cli <team-dir> [prompt]     # Required: team directory; prompt as args, stdin, or interactive
  --ollama-url string   Ollama API URL (default "http://localhost:11434/v1")
  -v, --verbose         Show full agent text output in real-time
  -w, --workspace       Workspace directory (default: <cwd>/workspace)
  -n, --new             Archive old session and start fresh
```

## Architecture

### Data Flow

```
User Prompt
    │
    ▼
main.go (CLI entry point)
    │
    ▼
team.LoadTeam() ── Parses team.yml + *.md agent definitions from team directory
    │
    ▼
team.NewCoordinator() ── Creates coordinator with Ollama provider, tools, MCP
    │
    ▼
coordinator.Run() ── Orchestrator agent receives prompt, uses run_agents tool
    │
    ├─► Agent 1 (worker) ── Executes with file/shell tools, writes to workspace
    ├─► Agent 2 (worker) ── Executes in parallel
    └─► Agent N ...
    │
    ▼
Coordinator synthesizes results ── Returns final answer to user
```

### Package Structure

| Package | Path | Purpose |
|---------|------|---------|
| `main` | `cmd/agent-team-cli/` | CLI entry, cobra flags, status display, session management |
| `agent` | `internal/agent/` | Agent definitions, Ollama provider, agent creation, tool selection |
| `tools` | `internal/tools/` | Built-in agent tools: bash, read, write, edit, grep, find, ls, ask_user |
| `team` | `internal/team/` | Team loading/parsing, coordinator orchestration, session persistence, workspace I/O |
| `mcp` | `internal/mcp/` | MCP server management, tool discovery & execution, local & remote transports |

### Key Types

- **`agent.AgentDef`** — Parsed from `.md` frontmatter; defines name, role, tools, model params, system prompt
- **`agent.TeamConfig`** — Parsed from `team.yml`; team-wide defaults for model, timeout, retries
- **`team.TeamSession`** — Loaded team state: config, agents map, MCP servers, workspace path
- **`team.Coordinator`** — Orchestrator: creates agents, manages rounds, delegates via `run_agents` tool
- **`team.SessionData`** — JSON session history (user/assistant exchanges) persisted across runs

### Agent Roles

- **`coordinator`/`orchestrator`** — Gets `run_agents` tool + `ask_user`. Cannot be delegated to. Has dynamically built system prompt listing all workers.
- **`worker`** (default role) — Gets specified tool set. Cannot use `run_agents`.

## Team Configuration Format

Teams are defined by a directory containing `team.yml` (or `team.yaml`) and one `.md` file per agent.

### team.yml

Simple YAML (custom parser, **not** full YAML — uses line-by-line `key: value` parsing):

```yaml
name: my-team
description: "Description"
max-rounds: 10        # Default: 10
timeout: 300           # Default: 600, in seconds
max-retries: 2         # Default: 2
model: ollama/qwen3:8b  # Default model for all agents
workspace: workspace   # Default: "workspace", can be absolute path
```

Generation params (`temperature`, `max-tokens`, `top-p`, `top-k`) are also supported at team level as fallbacks.

### Agent .md files

Markdown with YAML frontmatter delimited by `---`:

```markdown
---
name: developer
description: Implementation specialist
model: ollama/qwen3:8b
max-tokens: 8192
temperature: 0.2
top-p: 0.9
top-k: 40
role: worker
tools: read,write,edit,bash,grep,find,ls
timeout: 300
max-retries: 3
---
Your system prompt goes here.
```

**The body after the second `---` becomes the system prompt.** Frontmatter fields override team-level defaults.

### Tool Names

Available built-in tools (specified in agent `tools` field as comma-separated list, or `"all"`):

- `bash`, `read`, `write`, `edit`, `grep`, `find`, `ls`, `ask_user`
- `"glob"` is aliased to `"find"` in `agent.SelectTools()`

### MCP Configuration

MCP servers are declared in `team.yml` alongside team-level settings but are currently parsed via a separate mechanism. MCP tool names are prefixed as `{server}__{tool}` to avoid collisions.

## Workspace Layout

The workspace directory is created at runtime with these subdirectories:

```
workspace/
├── inbox/        # Per-agent task assignments (inbox/{agent}/task-*.md)
├── outbox/       # Per-agent results (outbox/{agent}/result-*.md)
├── shared/       # Shared context files between agents
├── status/       # Agent status files (status/{agent}.yml)
├── history/      # Archived session markdown files
├── session.json  # Structured session data
└── session.md    # Human-readable session log
```

## Session Management

- `session.json` tracks exchanges (`SessionData` with timestamps, roles, content)
- `session.md` is a human-readable markdown log regenerated from session data
- On `--new`: existing session is archived to `history/YYYY-MM-DD-slug.md`, fresh session starts
- Without `--new`: previous session resumes, context summary injected into orchestrator prompt (last 20 exchanges, 500 chars each)
- Session data is saved on success, error, and interrupt (Ctrl+C)

## Key Gotchas & Non-Obvious Patterns

1. **YAML parser is minimal** — `parseSimpleYAML()` only handles flat `key: value` pairs. No nested structures, no lists, no flow syntax. MCP server config parsing uses `encoding/json` unmarshalling instead.

2. **Agent names are case-insensitive** — `LoadTeam()` lowercases agent names when building the map. Tool `run_agents` task routing also lowercases. But system prompt generation uses original-case names.

3. **`run_agents` is the only coordinator tool** — Coordinator cannot do implementation work itself. It gets only `run_agents` (custom tool built by `Coordinator.RunAgentsTool()`) and `ask_user`.

4. **Coordinator round tracking** — `MaxRounds` limits delegation rounds, not total LLM calls. Each `run_agents` call increments the round counter.

5. **Agent caching** — Agents are created once per name and cached in `Coordinator.agentCache`. If the same agent name appears in multiple `run_agents` calls, the same `fantasy.Agent` instance is reused (stateless from the team's perspective since each call sends a new prompt).

6. **Retry with conversation history** — On failure, `executeTask` retries with accumulated `conversationHistory` from previous steps, so the LLM sees its prior attempts. This is per-task within a single agent execution.

7. **Tool path resolution** — All file tools (`read`, `write`, `edit`, `grep`, `find`, `ls`) resolve relative paths against the workspace directory (`WorkDir`). This is configured via `tools.WithWorkDir()`.

8. **Edit tool fuzzy matching** — The edit tool has a fuzzy match fallback. If exact `old_text` isn't found, it normalizes whitespace and smart quotes before matching. Multiple matches of the same text cause an error (must provide more context).

9. **Bash command restrictions** — A regex bans shell builtins (`alias`, `bg`, `bind`, `builtin`, etc.) from being executed via the bash tool.

10. **Model ID stripping** — `OllamaProvider.LanguageModel()` strips the `ollama/` prefix from model IDs, so both `qwen3:8b` and `ollama/qwen3:8b` work in config.

11. **MCP server types** — `"local"` (default when `command` is present) spawns stdio MCP servers. `"remote"` (when `url` is present) connects via HTTP. Auto-detected if `type` is empty.

12. **Output truncation** — All tool outputs are truncated: 2000 lines / 50KB default. Grep limits to 100 matches. Find limits to 1000 results. Bash output uses head-truncation (keeps start), while read uses head-truncation with offset support.

13. **Session context injection** — When resuming a session, the last 20 exchanges are summarized and injected into the orchestrator's system prompt under a "Session Context" heading, each entry capped at 500 chars.

14. **`ask_user` tool** — Only available to the coordinator agent (explicitly filtered in `coordinator.Run()`). Supports `single_choice`, `multiple_choice`, `free_text`, and `mixed` question types.

15. **go.mod has indirect dependencies marked as direct** — Several key dependencies (fantasy, lipgloss, cobra, mcp-go) show `// indirect` but are directly imported. Running `go mod tidy` would fix this.