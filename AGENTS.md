# AGENTS.md

## Project Overview

**agent-team-cli** is a Go CLI tool that orchestrates teams of LLM agents (via Ollama) to collaboratively accomplish tasks. Teams are discovered by name from configured search paths, and a single prompt can switch between multiple teams or invoke specific agents directly.

- **Module**: `github.com/anomalyco/agent-team-cli`
- **Go version**: 1.26.2
- **CLI framework**: cobra
- **LLM framework**: `charm.land/fantasy` (Charm's agent/LLM abstraction)
- **MCP client**: `github.com/mark3labs/mcp-go`

## Build & Run

```bash
go build ./cmd/agent-team-cli          # Build binary
go run ./cmd/agent-team-cli [prompt]  # Run directly
go vet ./...                            # Lint
go test ./...                           # Run tests
```

### CLI Usage

```
agent-team-cli [prompt]
  --ollama-url string              Ollama API URL (default "http://localhost:11434/v1")
  -v, --verbose                   Show full agent text output in real-time
  -w, --workspace                 Workspace directory (default: <cwd>/workspace)
  -n, --new                       Archive old session and start fresh
  --agent-team                     Directly specify team name (no @ needed in prompt)
  --agent-team-search-path         Comma-separated search paths (default: .agent-teams/,~/.agent-teams/)
```

### Prompt Syntax

- `@<team-name> <task>` — Switch to a team and delegate the task to its coordinator
- `@<agent-name> <task>` — Invoke a specific agent directly in the current team
- Plain text — Passed to the current team's coordinator

Multiple teams can be used in one prompt:
```
@team-a do research @team-b write docs @team-a summarize
```

### Interactive Mode

When no prompt is provided and stdin is empty:
1. If no team can be inferred, shows team selection menu
2. Then prompts for the task description

## Architecture

### Data Flow

```
User Prompt
    │
    ▼
main.runTeam()
    │
    ├─► team.NewTeamRegistry(searchPaths)
    ├─► TeamRegistry.Discover() ── Scans .agent-teams/ dirs for team.yml/team.yaml
    │
    ▼
ParsePromptWithLazyAgents() ── Identifies @team-name references
    │
    ├─► TeamRegistry.Resolve(name) ── Maps team name to absolute directory path
    │
    ▼
loadTeamByName() ── LoadTeam() + NewCoordinator() per team
    │
    ▼
executeSegments() ── Processes PromptSegments in order
    │
    ├─► SegmentSwitchTeam ── coordinator.Run() (switch active team)
    ├─► SegmentInvokeAgent ── coordinator.RunDirectAgent() (direct invoke)
    └─► SegmentText ── coordinator.Run() (pass to coordinator)
    │
    ▼
Results joined and printed to stdout
```

### Package Structure

| Package | Path | Purpose |
|---------|------|---------|
| `main` | `cmd/agent-team-cli/` | CLI entry, cobra flags, segment execution, status display |
| `agent` | `internal/agent/` | Agent definitions, Ollama provider, agent creation, tool selection |
| `tools` | `internal/tools/` | Built-in agent tools: bash, read, write, edit, grep, find, ls, ask_user |
| `team` | `internal/team/` | Team loading/parsing, coordinator, session persistence, workspace I/O, discovery, prompt parsing |
| `skill` | `internal/skill/` | Skill discovery, parsing, filtering, workspace copying |
| `mcp` | `internal/mcp/` | MCP server management, local & remote transports |

### Key Types

- **`TeamRegistry`** (`discovery.go`) — Discovers and resolves teams by name from search paths
- **`PromptSegment`** (`prompt.go`) — One unit of execution: `switch_team`, `invoke_agent`, or `text`
- **`team.TeamSession`** — Loaded team state: config, agents map, MCP servers, skills, workspace
- **`team.Coordinator`** — Orchestrator: delegates via `run_agents`, provides `finish`/`load_skill`
- **`team.TeamContext`** — Container holding session + coordinator + sessionData for one team
- **`skill.SkillDef`** — Parsed from `SKILL.md`; name, description, content, summary

## Team Discovery

### Search Paths

Default: `./.agent-teams/` and `~/.agent-teams/`

Custom paths via `--agent-teams-search-path` (comma-separated). Paths starting with `~` are expanded to the user's home directory.

### Team Directory Structure

```
.agent-teams/
├── delegate/
│   ├── team.yaml
│   ├── coordinator.md
│   ├── researcher.md
│   ├── writer.md
│   └── .agents/
│       └── skills/
│           └── code-review/
│               └── SKILL.md
├── tether/
│   └── ...
```

A directory is a valid team if it contains `team.yml` or `team.yaml`.

## Prompt Parsing

### Segment Types

```go
const (
    SegmentSwitchTeam  = "switch_team"   // Switch to a team
    SegmentInvokeAgent = "invoke_agent"  // Invoke a specific agent
    SegmentText        = "text"          // Pass to current team's coordinator
)
```

### Parsing Functions

| Function | Purpose |
|----------|---------|
| `HasAtName(s)` | Check if string contains `@name` references |
| `ParsePromptWithLazyAgents(prompt, registry, defaultTeam)` | Initial parse: identifies `@team-name`; falls back to `--agent-team` flag |
| `ParsePrompt(prompt, registry, currentTeam, currentAgents)` | Full parse: handles both team switches and agent invokes |
| `SplitSegmentByAgents(segment, registry, currentAgents)` | Splits a switch_team segment containing `@agent-name` calls |
| `extractUntilNextAt(rest)` | Extracts task content until the next `@name` |

### @-name Pattern

Regex: `@([\w][\w-]*)` — matches `@team`, `@team-name`, `@agent1`, but NOT `@-leading`.

**Important**: `@example.com` (email-like) also matches due to the broad regex. The parser disambiguates by checking if the name is a known team or agent.

### Parse Flow

```
"@delegate research this @tether also check that"
    │
    ▼
ParsePromptWithLazyAgents() — sees @delegate is a team
    │
    ▼
[SegmentSwitchTeam{name="delegate", content="research this @tether also check that"}]
    │
    ▼
SplitSegmentByAgents() — sees @tether is another team
    │
    ▼
[SegmentSwitchTeam{name="delegate", content="research this "},
 SegmentSwitchTeam{name="tether", content="also check that"}]
```

## Multi-Team Execution

### teamContext

```go
type teamContext struct {
    session     *team.TeamSession
    coordinator *team.Coordinator
    sessionData *team.SessionData
}
```

Each team has its own `teamContext` with independent session, workspace, and coordinator instance.

### executeSegments

Processes segments sequentially. On team switch:
1. Saves previous team's session (`SaveSessionMD`)
2. Activates new team context
3. Runs the task (either full `Run()` or `RunDirectAgent()`)

### Direct Agent Invocation

When a segment's `@name` matches an agent in the current team (not a team name):
1. `coordinator.RunDirectAgent(agentName, task)` is called directly
2. If the team has a coordinator, the result is passed to the coordinator for synthesis
3. If no coordinator, the result is output directly

### Idle Warning

30-second idle timer resets on each status event. After 30s of no activity, prints an idle warning to stderr.

## Task Tracking (TodoList)

`TaskTracker` contains a `TodoList` for structured task progress tracking:

```go
type TodoItem struct {
    ID     string      // Auto-incrementing: "1", "2", ...
    Agent  string      // Agent name
    Desc   string      // Task description
    Status TaskStatus  // TaskPending / TaskInProgress / TaskDone / TaskError
    Detail string      // Error detail (when Status == TaskError)
}

type TodoList struct {
    mu   sync.Mutex
    items []*TodoItem
    next  int  // Next ID counter
}
```

| Method | Description |
|--------|-------------|
| `AddBatch([]{Agent, Desc})` | Batch-add tasks; returns `[]*TodoItem` with auto-assigned IDs |
| `UpdateStatus(id, status, detail)` | Update status of a specific task by ID |
| `Items()` | Returns a thread-safe copy of all items |
| `Clear()` | Clears all items and resets the ID counter |

**Status flow:**
```
AddBatch() → TaskPending → UpdateStatus(TaskInProgress) → TaskDone
                                          ↓
                                     TaskError
```

**Coordinator integration:**
- `ExecuteTasks()` calls `TodoList.AddBatch()` to create TODO items for each delegated task
- `executeTask()` and `RunDirectAgent()` call `UpdateStatus()` at each lifecycle stage
- Every TODO update fires `StatusEvent{Type: "todos_updated", Todos: ...}`
- CLI renders the TODO panel on each `todos_updated` event

**CLI display format:**
```
─── TODO ───
  ◑ 1. researcher find bugs
  ○ 2. writer write docs
  ● 3. checker verify tests
  ✗ 4. researcher attempt 1 failed: ...
```

## Team Configuration Format

### team.yml

Simple flat YAML (custom line-by-line parser):

```yaml
name: my-team
description: "Description"
max-rounds: 10
timeout: 300
max-retries: 2
model: ollama/qwen3:8b
workspace: workspace
skills: code-review,git-commit
skills-exclude: debug
```

### Agent .md files

Markdown with YAML frontmatter (`---` delimited):

```markdown
---
name: developer
description: Implementation specialist
role: worker
tools: read,write,edit,bash,grep,find,ls
skills: code-review
---
Your system prompt here.
```

### Skills Directory Structure

```
{team-dir}/.agents/skills/{skill-name}/SKILL.md
~/.agents/skills/{skill-name}/SKILL.md
```

## Workspace Layout

Each team gets its own workspace directory (named `workspace` relative to the team directory, or as overridden by `--workspace`).

```
workspace/
├── inbox/           # Per-agent task assignments
├── outbox/          # Per-agent results
├── shared/
│   └── skills/     # Copied SKILL.md files
├── status/          # Agent status files
├── history/         # Archived session files
├── session.json     # Structured session data
└── session.md      # Human-readable session log
```

## Session Management

- Each team maintains its own session data independently
- On team switch: previous team's session is saved (`SaveSessionMD`)
- On interrupt (Ctrl+C): current team's session is saved
- `--new` archives and starts a fresh session per active team

## Tools Reference

### Coordinator Tools

- **`run_agents`** — Delegate tasks to workers
- **`load_skill`** — Load skill content by name
- **`finish`** — Signal completion with final answer
- **`ask_user`** — Request user input

### Worker Tools

- `bash`, `read`, `write`, `edit`, `grep`, `find`, `ls`, `ask_user`
- Skill summaries auto-injected into task prompts for workers with `skills` field

## Key Gotchas & Non-Obvious Patterns

1. **CLI no longer takes team directory as positional arg** — Usage changed from `agent-team-cli <team-dir> [prompt]` to `agent-team-cli [prompt]`. Teams are discovered by name from search paths.

2. **Team vs agent disambiguation** — `@name` is first checked against known teams (via `registry.HasTeam()`), then against the current team's agent list. Unknown names produce specific error messages listing available options.

3. **Prompt parsing is lazy-then-eager** — `ParsePromptWithLazyAgents` only identifies the team switch; agent invokes within that team are resolved later by `SplitSegmentByAgents`.

4. **`@example.com` false positive** — The `@name` regex is broad. The parser disambiguates, but `HasAtName()` will return true for email-like strings.

5. **No parallel team loading upfront** — `loadTeamByName` is called in a loop after `ParsePromptWithLazyAgents`, but only for teams that will actually be used (lazy loading).

6. **Team switch saves previous session** — When moving from one team to another, `SaveSessionMD` is called before activating the new team. This ensures each team's session is persisted.

7. **Direct agent invocation can synthesize** — If the team has a coordinator, `RunDirectAgent` result is passed to the coordinator with a synthesis prompt, not returned raw.

8. **`BuildOrchestratorPrompt` uses string.Builder** — Formerly a `fmt.Sprintf` template; now uses `strings.Builder` for better performance with large agent/skill lists.

9. **CLI returns `exit(130)` on SIGINT** — Interrupt handling checks `ctx.Err() == context.Canceled` and calls `os.Exit(130)` for proper shell integration.

10. **`prompt_test.go` requires discovered teams** — Tests in `TestParsePromptWithLazyAgents` and `TestSplitSegmentByAgents` use `newTestRegistry()` which needs the `.agent-teams/` directory to exist with valid teams. Tests will skip/fail if no teams are found.

11. **`idleWarningTimer`** — New struct in main.go, tracks idle time and prints a warning to stderr after 30s of no activity. Resets on every status event.

12. **Workspace flag is shared** — `--workspace` sets the workspace for all teams; each team's session uses this combined workspace path.

13. **`teamStyle`** — New lipgloss style (color 13/magenta) added for displaying team names in CLI output, distinct from `agentStyle` (cyan).

14. **go.mod indirect deps** — `charm.land/fantasy`, `github.com/charmbracelet/lipgloss`, `github.com/spf13/cobra`, `github.com/mark3labs/mcp-go` are marked `// indirect` but directly imported. `go mod tidy` would fix this.

15. **Prompt injection via Ctrl+Z / SIGUSR1** — While a coordinator is running, users can press Ctrl+Z (SIGTSTP) or send SIGUSR1 to inject an additional prompt. The prompt is enqueued and processed after the current coordinator round completes. The coordinator receives a continuation message instructing it to adjust tasks. New prompts do NOT interrupt or cancel running agents.

16. **ContinueWithPrompt preserves conversation history** — `Coordinator.ContinueWithPrompt()` reuses the `conversationHistory` field (accumulated `fantasy.Message` from previous rounds) so the coordinator has full context when processing an injected prompt. This is different from `Run()` which starts fresh.

17. **Stdin mutex** — `tools.StdinMu` is a shared `sync.Mutex` that serializes reads from stdin. Both `ask_user` (agent tool) and `promptInjector.promptAndEnqueue()` (Ctrl+Z handler) lock this mutex before reading, preventing garbled input.

18. **promptInjector** — Buffered channel (size 16) that receives prompts from SIGTSTP/SIGUSR1 signal handlers. `poll()` is non-blocking; `runWithInjection()` checks after each coordinator round.

15. **`TaskInfo` vs `TodoItem`** — `TaskTracker` maintains both the old `TaskInfo` (used by `Start`/`Done`/`Error`) and the new `TodoList`. Both are used in parallel. `TodoItem` fields are `ID`/`Agent`/`Desc` (lowercase), while the old `TaskInfo` uses `Agent`/`Task`. The TODO display uses `t.ID` prefix (e.g., `1.`) to identify each item.
