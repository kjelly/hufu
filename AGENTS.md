# AGENTS.md

## Project Overview

**hufu** is a Go CLI tool that orchestrates teams of LLM agents (via Ollama) to collaboratively accomplish tasks. Teams are discovered by name from configured search paths, and a single prompt can switch between multiple teams or invoke specific agents directly.

- **Module**: `github.com/anomalyco/hufu`
- **Go version**: 1.26.2
- **CLI framework**: cobra
- **LLM framework**: `charm.land/fantasy` (Charm's agent/LLM abstraction)
- **MCP client**: `github.com/mark3labs/mcp-go`

## Build & Run

```bash
go build ./cmd/hufu          # Build binary
go run ./cmd/hufu [prompt]  # Run directly
go vet ./...                            # Lint
go test ./...                           # Run tests
```

### CLI Usage

```
hufu [prompt]
  --ollama-url string              Ollama API URL (default "http://localhost:11434/v1")
  -v, --verbose                   Show full agent text output in real-time
  -w, --workspace                 Workspace directory (default: <cwd>/workspace)
  -n, --new                       Archive old session and start fresh
  -t, --temp                      Use a temporary directory as workspace
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
| `main` | `cmd/hufu/` | CLI entry, cobra flags, segment execution, status display |
| `agent` | `internal/agent/` | Agent definitions, Ollama provider, agent creation, tool selection |
| `tools` | `internal/tools/` | Built-in agent tools: bash, view, write, edit, multiedit, grep, glob, ls, download, fetch, agentic_fetch, lua, golang, ask_user |
| `team` | `internal/team/` | Team loading/parsing, coordinator, session persistence, workspace I/O, discovery, prompt parsing |
| `skill` | `internal/skill/` | Skill discovery, parsing, filtering, workspace copying |
| `mcp` | `internal/mcp/` | MCP server management, local & remote transports |

### Key Types

- **`TeamRegistry`** (`discovery.go`) — Discovers and resolves teams by name from search paths
- **`PromptSegment`** (`prompt.go`) — One unit of execution: `switch_team`, `invoke_agent`, or `text`
- **`team.TeamSession`** — Loaded team state: config, agents map, MCP servers, skills, workspace
- **`team.Coordinator`** — Orchestrator: delegates via `agent`, provides `finish`/`load_skill`
- **`team.TeamContext`** — Container holding session + coordinator + sessionData for one team
- **`skill.SkillDef`** — Parsed from `SKILL.md`; name, description, content, summary

## Team Discovery

### Search Paths

Default: `./.agent-teams/` and `~/.agent-teams/`

Custom paths via `--agent-team-search-path` (comma-separated). Paths starting with `~` are expanded to the user's home directory.

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

### @-name Priority Rules

When `@name` matches both a team name and an agent name (via fuzzy match):

1. **No active team** (`currentTeam == ""`): Team name wins. `@reviewer` → switch to "reviewer" team.
2. **Inside a team** (`currentTeam != ""`): Agent name wins. `@reviewer` → invoke "Code Reviewer" agent.
3. **When `@name` is used as a team switch**, the same `@name` reference is stripped from the content and not re-interpreted as an agent name. `@reviewer check code` → team switch only (no agent invocation).

In `SplitSegmentByAgents`, team names are checked before agent names: if `@name` matches a known team, it becomes a team switch regardless of agent name matches.

### Parse Flow

```
"@delegate research this @tether also check that"
    │
    ▼
ParsePromptWithLazyAgents() — sees @delegate is a team, strips @delegate from content
    │
    ▼
[SegmentSwitchTeam{name="delegate", content="research this @tether also check that"}]
    │
    ▼
SplitSegmentByAgents() — sees @tether is another team (team name takes priority)
    │
    ▼
[SegmentSwitchTeam{name="delegate", content="research this "},
 SegmentSwitchTeam{name="tether", content="also check that"}]
```

```
"@reviewer check code"
    │
    ▼
ParsePromptWithLazyAgents() — sees @reviewer is a team, strips @reviewer from content
    │
    ▼
[SegmentSwitchTeam{name="reviewer", content="check code"}]
    │
    ▼
SplitSegmentByAgents() — no @name references in "check code"
    │
    ▼
[SegmentSwitchTeam{name="reviewer", content="check code"}]
```

```
"@reviewer @reviewer check code"
    │
    ▼
ParsePromptWithLazyAgents() — sees @reviewer is a team, strips first @reviewer
    │
    ▼
[SegmentSwitchTeam{name="reviewer", content="@reviewer check code"}]
    │
    ▼
SplitSegmentByAgents() — sees @reviewer in content, matches "Code Reviewer" agent (inside team, agent priority)
    │
    ▼
[SegmentSwitchTeam{name="reviewer", content=""},
 SegmentInvokeAgent{name="reviewer", content="check code"}]
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
2. Skill matching via sidecar is performed before the agent runs (`matchSkillsWithSidecar`)
3. If the team has a coordinator, the result is passed to the coordinator for synthesis
4. If no coordinator, the result is output directly

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
tools: view,write,edit,multiedit,bash,grep,glob,ls
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

## Prompt Injection via Signals

While a coordinator is running, users can inject additional prompts without interrupting running agents:

**Supported signals:** `SIGTSTP` (Ctrl+Z) and `SIGUSR1`

**Architecture:**
```go
type promptInjector struct {
    ch           chan string
    mu           sync.Mutex
    promptReader *readline.PromptReader  // optional; nil means fallback mode
}
```

- `enqueue()` — non-blocking write to channel; drops if full
- `poll()` — non-blocking read using `select default`
- `promptAndEnqueue()` — locks `StdinMu` before reading stdin; uses `readline.PromptReader` when available

**StdinMutex:** `tools.StdinMu` is a shared `sync.Mutex` that serializes stdin reads between `ask_user` tool and signal handlers.

**Flow:**
```
Signal (Ctrl+Z / SIGUSR1)
    │
    ▼
injector.promptAndEnqueue() — reads one line from stdin via readline
    │
    ▼
Buffered in channel (size 16)
    │
    ▼
After each coordinator round: runWithInjection() polls the channel
    │
    ▼
Coordinator.ContinueWithPrompt() processes pending prompts
```

**`ContinueWithPrompt()`** — preserves `conversationHistory` (accumulated `fantasy.Message`) so the coordinator has full context. Different from `Run()` which does not preserve history.

**`projectDir` change:** `Coordinator` now stores `projectDir` (from `os.Getwd()`) separately from `session.Workspace`. `WorkDir` for agents is set to `projectDir`, while `session.Workspace` is only used for session persistence.

## Readline Integration

`internal/readline` wraps `github.com/ergochat/readline` for interactive CLI input:

```go
type PromptReader struct {
    instance *ergoreadline.Instance
}

func NewPromptReader(historyFile string) (*PromptReader, error)
func (r *PromptReader) ReadLine(prompt string) (string, error)
func (r *PromptReader) Close() error
```

- **Prompt history**: enabled automatically, stored at `~/.hufu/prompt_history` (limit 1000 lines)
- **Signal prompts**: prompts injected via Ctrl+Z/SIGUSR1 also use readline, providing history navigation
- **Ctrl+C / Ctrl+D**: trigger `ErrInterrupt` and `io.EOF` respectively, causing `os.Exit(130)` or graceful exit
- **Fallback**: if readline init fails (e.g., non-terminal), degrades to `fmt.Scanln` with no history or readline features
- **History file**: `defaultHistoryPath()` returns `~/.hufu/prompt_history`; created on demand with `0o755`

## Tools Reference

### Coordinator Tools

- **`agent`** — Delegate tasks to workers
- **`load_skill`** — Load skill content by name
- **`finish`** — Signal completion with final answer
- **`ask_user`** — Request user input

### Worker Tools

- `bash`, `view`, `write`, `edit`, `multiedit`, `grep`, `glob`, `ls`, `download`, `fetch`, `agentic_fetch`, `lua`, `golang`, `ask_user`
- **`agent`** — Create a sub-agent to execute a specific task (always available, even if not listed in `tools:`)
- **`todo`** — Manage task list: create, update, and list TODO items (always available, even if not listed in `tools:`)
- Skill summaries auto-injected into task prompts for workers with `skills` field

## Key Gotchas & Non-Obvious Patterns

1. **CLI no longer takes team directory as positional arg** — Usage changed from `hufu <team-dir> [prompt]` to `hufu [prompt]`. Teams are discovered by name from search paths.

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

16. **readline fallback is silent** — When `NewPromptReader` returns an error, `pr` is set to `nil` and a warning is printed to stderr. All prompt functions check `pr != nil` and fall back to `fmt.Scanln`. No readline features (history, completion) are available in fallback mode.

17. **promptInjector carries PromptReader** — The `promptInjector` struct now embeds `*readline.PromptReader`. When nil, `promptAndEnqueue` returns immediately without reading. This ensures graceful degradation when readline is unavailable.

18. **setupPromptSignals returns cleanup func** — `setupPromptSignals` now returns a `func()` that stops signal handlers and closes channels. Called via `defer setupPromptSignals(injector)()` to ensure cleanup on function exit.

19. **Graceful shutdown with Ctrl+C** — First Ctrl+C sends SIGINT → `activeCoordinator.SetWrapUp()` → `SetWrapUp()` reports `StatusEvent{Type: "wrap_up"}` (CLI displays `─── WRAP UP ───`) → `ExecuteTasks` checks `IsWrapUp()` and refuses to delegate new tasks → Second Ctrl+C forces `cancel()` for immediate exit.

20. **Wrap-up mechanism** — `promptInjector.wrapUpCh` (buffered channel, size 1) + `wrapUpRequested atomic.Bool` flag. `injectWrapUp()` sets the flag and sends to channel (non-blocking). `IsWrapUpRequested()` atomically checks. `runWithInjection()` uses `select` to handle both normal prompts and wrap-up in one select statement.

21. **wrapUpPromptTemplate** — Hard-coded prompt that forces the coordinator to summarize immediately and call `finish`. No new tasks delegated, no `agent` calls.

22. **activeCoordinator pointer** — Passed to `executeSegments`, stored/cleared on each coordinator run call. Allows signal handler to call `SetWrapUp()` on the right coordinator instance even after `executeSegments` returns.

23. **StatusEvent builder pattern** — `c.newEvent(type)` returns a `StatusEvent` with `Type` and `TeamName` pre-filled. Chainable methods: `withAgent()`, `withMessage()`, `withStep()`, `withTool()`, `withToolResult()`, `withTodos()`. No more manual struct literal construction throughout coordinator code.

24. **Tool call/result output includes agent label** — `formatAgentLabel(event)` renders `"team-name/agent-name"` for cross-team visibility. Tool call output now includes the agent label and `›` separator, with args on a separate indented line. Previous format was all on one line.

25. **Context propagation for wrap-up** — `context.WithCancel(context.Background())` used instead of `signal.NotifyContext` directly, so `cancel()` can be called by the second Ctrl+C while SIGINT is handled separately via a dedicated channel.

26. **Coordinator.wrapUp atomic flag** — `wrapUp atomic.Int32` stores wrap-up state. `SetWrapUp()` stores 1, `IsWrapUp()` checks if 1. Different from the channel-based notification — the atomic flag persists the state across coordinator calls.

27. **runWithInjection uses select not polling** — `select { case <-injector.wrapUpCh: ... case prompt, ok := <-injector.ch: ... default: return }` instead of a polling loop. More efficient and responsive.

28. **executeSegments passes activeCoord to all run paths** — Both coordinator `Run()` and `RunDirectAgent()` paths set/clear `activeCoordinator` so wrap-up signal can target the right instance regardless of which code path is active.

## Skill Usage Tracking

The system tracks which skills are loaded/used during a session:

### Detection Methods

1. **Via `load_skill` tool** — Coordinator explicitly loads a skill
2. **Via `view`/`read` tool calls** — Detects when agents view files in `shared/skills/` directory

### Data Structures

| Component | Type | Purpose |
|-----------|------|---------|
| `SkillUsageEntry` | struct | `{Name, Count, Agents map[string]bool}` |
| `skillUsage` | `map[string]*SkillUsageEntry` | Per-coordinator usage tracking |
| `StatusEvent.SkillName` | string | Event field for `"skill_used"` events |

### CLI Display

The CLI renders skill usage in a panel:

```
─── SKILLS ───
  ✓ code-reviewer     ×2  researcher, writer
  ✓ git-commit        ×1  developer
```

Displayed via `skillDisplay` struct in `cmd/hufu/display.go`, updated on each `skill_used` event.

### Untracked Skill

| Path | Description |
|------|-------------|
| `.agents/skills/code-reviewer/SKILL.md` | Code review skill (supports local changes and remote PRs) |
