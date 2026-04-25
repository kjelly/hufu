# AGENTS.md

## Project Overview

**agent-team-cli** is a Go CLI tool that orchestrates teams of LLM agents (via Ollama) to collaboratively accomplish tasks. A coordinator agent delegates work to worker agents, which execute tasks in parallel using file and shell tools. MCP servers and skills can be attached as additional capabilities.

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

When no prompt is provided and stdin is empty, the CLI prompts interactively:

```
─── Enter Prompt ───
Please describe the task you want the team to perform:
> 
```

## Architecture

### Data Flow

```
User Prompt (CLI arg, stdin, or interactive)
    │
    ▼
main.go (CLI entry point)
    │
    ▼
team.LoadTeam() ── Parses team.yml + *.md agent definitions + skills
    │
    ├─► skill.DiscoverSkills() from .agents/skills/ and ~/.agents/skills/
    │
    ▼
team.NewCoordinator() ── Creates coordinator with Ollama provider, tools, MCP, skills
    │
    ├─► skill.CopySkillsToWorkspace() ── Copies SKILL.md files to workspace/shared/skills/
    │
    ▼
coordinator.Run() ── Orchestrator agent receives prompt, uses tools
    │
    ├─► run_agents ── Delegates tasks to workers (parallel execution)
    ├─► load_skill ── Loads skill content by name (coordinator only)
    ├─► finish ── Signals completion (coordinator only)
    │
    ├─► Agent 1 (worker) ── Executes with file/shell tools + skill injection
    ├─► Agent 2 (worker) ── Executes in parallel
    └─► Agent N ...
    │
    ▼
Coordinator calls finish ── Returns final answer to user
```

### Package Structure

| Package | Path | Purpose |
|---------|------|---------|
| `main` | `cmd/agent-team-cli/` | CLI entry, cobra flags, status display, session management |
| `agent` | `internal/agent/` | Agent definitions, Ollama provider, agent creation, tool selection |
| `tools` | `internal/tools/` | Built-in agent tools: bash, read, write, edit, grep, find, ls, ask_user |
| `team` | `internal/team/` | Team loading/parsing, coordinator orchestration, session persistence, workspace I/O |
| `mcp` | `internal/mcp/` | MCP server management, tool discovery & execution, local & remote transports |
| `skill` | `internal/skill/` | Skill discovery, parsing, filtering, workspace copying |

### Key Types

- **`agent.AgentDef`** — Parsed from `.md` frontmatter; defines name, role, tools, skills, model params, system prompt
- **`agent.TeamConfig`** — Parsed from `team.yml`; team-wide defaults including skills include/exclude filters
- **`team.TeamSession`** — Loaded team state: config, agents map, MCP servers, skills list, workspace path
- **`team.Coordinator`** — Orchestrator: creates agents, manages rounds, delegates via `run_agents`, provides `load_skill` and `finish` tools
- **`skill.SkillDef`** — Parsed from `SKILL.md`; name, description, allowed-tools, content, workspace path, summary
- **`team.SessionData`** — JSON session history persisted across runs

### Agent Roles

- **`coordinator`** — Gets `run_agents`, `finish`, `load_skill`, `ask_user`. Cannot be delegated to.
- **`worker`** (default role) — Gets specified tool set. Can have `skills` field that injects skill summaries into task prompts.

## Team Configuration Format

Teams are defined by a directory containing `team.yml` (or `team.yaml`), one `.md` file per agent, and optionally `.agents/skills/`.

### team.yml

Simple YAML (custom parser, **not** full YAML — line-by-line `key: value` only):

```yaml
name: my-team
description: "Description"
max-rounds: 10        # Default: 10
timeout: 300           # Default: 600, in seconds
max-retries: 2         # Default: 2
model: ollama/qwen3:8b  # Default model for all agents
workspace: workspace   # Default: "workspace", can be absolute path
skills: code-review,git-commit    # Include these skills
skills-exclude: debug              # Exclude these skills
```

Generation params (`temperature`, `max-tokens`, `top-p`, `top-k`) are also supported at team level.

### Agent .md files

Markdown with YAML frontmatter delimited by `---`:

```markdown
---
name: developer
description: Implementation specialist
model: ollama/qwen3:8b
max-tokens: 8192
temperature: 0.2
role: worker
tools: read,write,edit,bash,grep,find,ls
skills: code-review
---
Your system prompt goes here.
```

**The body after the second `---` becomes the system prompt.** Frontmatter fields override team-level defaults.

### Tool Names

Available built-in tools (comma-separated list, or `"all"`):

- `bash`, `read`, `write`, `edit`, `grep`, `find`, `ls`, `ask_user`
- `"glob"` is aliased to `"find"` in `agent.SelectTools()`

### MCP Configuration

MCP servers are declared in `team.yml` alongside team-level settings. MCP tool names are prefixed as `{server}__{tool}`.

### Skills Directory Structure

```
team-dir/.agents/skills/{skill-name}/SKILL.md
~/.agents/skills/{skill-name}/SKILL.md
```

Each skill `SKILL.md` has YAML frontmatter:

```markdown
---
name: code-review
description: Review code for bugs, security, and performance
allowed-tools: read,bash,grep,find
---
# Skill body here...
```

Skills are deduplicated by name (case-insensitive). Team-local skills take precedence over `~/.agents/skills/`.

## Workspace Layout

```
workspace/
├── inbox/        # Per-agent task assignments (inbox/{agent}/task-*.md)
├── outbox/       # Per-agent results (outbox/{agent}/result-*.md)
├── shared/
│   └── skills/  # Copied SKILL.md files (workspace/shared/skills/{name}.md)
├── status/       # Agent status files (status/{agent}.yml)
├── history/      # Archived session markdown files
├── session.json  # Structured session data
└── session.md    # Human-readable session log
```

## Session Management

- `session.json` tracks exchanges with timestamps and roles, persisted across runs
- `session.md` is a human-readable markdown log regenerated from session data
- On `--new`: existing session is archived to `history/YYYY-MM-DD-slug.md`, fresh session starts
- Without `--new`: previous session resumes, context summary injected into orchestrator prompt (last 20 exchanges, 500 chars each)
- Session data is saved on success, error, and interrupt (Ctrl+C)

## Key Gotchas & Non-Obvious Patterns

1. **YAML parser is minimal** — `parseSimpleYAML()` only handles flat `key: value` pairs. No nested structures, no lists, no flow syntax. MCP server config parsing uses `encoding/json` unmarshalling instead.

2. **Agent names are case-insensitive** — `LoadTeam()` lowercases agent names when building the map. Tool `run_agents` task routing also lowercases. But system prompt generation uses original-case names.

3. **Coordinator tools are limited** — Coordinator only gets `run_agents`, `finish`, `load_skill`, and `ask_user`. It cannot do implementation work itself.

4. **`finish` tool required** — Coordinator must call the `finish` tool instead of outputting text. The response is prefixed with `FINISHED:` which signals completion.

5. **`load_skill` for coordinator only** — This tool loads skill content into the coordinator's context. Workers receive skill summaries injected into their prompts, not this tool.

6. **Skill injection for workers** — When a worker has a `skills` field, the coordinator automatically prepends a "Relevant Skills" section to the task prompt, with the skill summary and workspace path to the skill file. Workers can `read` the full skill content from `workspace/shared/skills/{name}.md`.

7. **Skills copied to workspace on startup** — `CopySkillsToWorkspace()` is called in `coordinator.Run()` before the coordinator starts. All discovered skills are copied, not just included ones.

8. **Round tracking** — `MaxRounds` limits delegation rounds, not total LLM calls. Each `run_agents` call increments the round counter.

9. **Agent caching** — Agents are created once per name and cached in `Coordinator.agentCache`. Reused across multiple `run_agents` calls with fresh prompts.

10. **Retry with conversation history** — On failure, `executeTask` retries with accumulated `conversationHistory` from previous steps, so the LLM sees its prior attempts. Per-task within a single agent execution.

11. **Tool path resolution** — All file tools resolve relative paths against the workspace directory (`WorkDir`), configured via `tools.WithWorkDir()`.

12. **Edit tool fuzzy matching** — Exact match first; if not found, normalizes whitespace and smart quotes. Multiple matches cause error (need more context).

13. **Bash command restrictions** — Regex bans shell builtins (`alias`, `bg`, `bind`, `builtin`, `caller`, `command`, `compgen`, `complete`, `compopt`, `coproc`, `dirs`, `disown`, `enable`, `fc`, `fg`, `hash`, `help`, `history`, `jobs`, `kill`, `logout`, `mapfile`, `popd`, `pushd`, `readonly`, `select`, `set`, `shopt`, `source`, `suspend`, `times`, `trap`, `type`, `typeset`, `ulimit`, `umask`, `unalias`, `wait`).

14. **Model ID stripping** — `ollama/` prefix stripped by `OllamaProvider.LanguageModel()`, so both `qwen3:8b` and `ollama/qwen3:8b` work.

15. **MCP server types** — `"local"` (default when `command` is present) spawns stdio MCP servers. `"remote"` (when `url` is present) connects via HTTP.

16. **Output truncation** — Tool outputs truncated: 2000 lines / 50KB default. Grep limits to 100 matches. Find limits to 1000 results. Bash uses tail truncation; read uses head truncation with offset support.

17. **`ask_user` tool** — Only available to the coordinator agent (explicitly filtered in `coordinator.Run()`). Supports `single_choice`, `multiple_choice`, `free_text`, and `mixed` question types.

18. **go.mod has indirect dependencies marked as direct** — Key deps (fantasy, lipgloss, cobra, mcp-go) show `// indirect` but are directly imported. Running `go mod tidy` would fix this.

19. **Skill discovery is non-recursive** — `DiscoverSkills` reads one level of subdirectories only. Each skill needs its own `{name}/SKILL.md` structure.

20. **Skill dedup by lowercase name** — `seen[nameLower]` map ensures the same skill name from multiple directories doesn't duplicate. Team-local `.agents/skills/` is checked before `~/.agents/skills/`.

21. **Coordinator prompt dynamically includes skills table** — `BuildOrchestratorPrompt()` generates a markdown table of all available skills with descriptions, and includes instructions to use `load_skill` before delegating.

22. **Default orchestrator system prompt updated** — Now instructs to use `load_skill` for relevant tasks and include skill instructions in worker task descriptions.
