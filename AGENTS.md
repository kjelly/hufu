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

## Package Structure

| Package | Path | Purpose |
|---------|------|---------|
| `main` | `cmd/hufu/` | CLI entry, cobra flags, segment execution, status display, TUI |
| `agent` | `internal/agent/` | Agent definitions, provider manager (multi-provider), agent creation, tool selection |
| `tools` | `internal/tools/` | Built-in agent tools: bash, sudo, ssh, view, write, edit, multiedit, grep, glob, ls, download, fetch, agentic_fetch, lua, golang, ask_user, random, math |
| `team` | `internal/team/` | Team loading/parsing, coordinator, session persistence, workspace I/O, discovery, prompt parsing, todo tracking, sidecar integration |
| `skill` | `internal/skill/` | Skill discovery, parsing, filtering, workspace copying |
| `mcp` | `internal/mcp/` | MCP server management, local & remote transports |
| `sidecar` | `internal/sidecar/` | Lightweight auxiliary LLM for skill matching, guard review, plan review |
| `tui` | `internal/tui/` | Bubble Tea TUI: task tracking, detail views, search, clipboard, ask_user |
| `readline` | `internal/readline/` | `github.com/ergochat/readline` wrapper for interactive prompts |
| `config` | `internal/config/` | YAML config loader (`~/.config/hufu/hufu.yaml`, `./hufu.yaml`) |
| `memory` | `internal/memory/` | Long-term memory (RAG) with chromem-go vector store |
| `hooks` | `internal/hooks/` | Hook registry for tool lifecycle events |
| `notify` | `internal/notify/` | External notification system (webhooks) |
| `audit` | `internal/audit/` | Tool call audit logging |
| `utils`      | `internal/utils/`      | Shared utility functions                                                            |
| `yamlutil`   | `internal/yamlutil/`   | YAML flatten/parse utilities for config interpolation                               |

### Key Types

- **`TeamRegistry`** (`discovery.go`) — Discovers and resolves teams by name from search paths
- **`PromptSegment`** (`prompt.go`) — One unit of execution: `switch_team`, `invoke_agent`, or `text`
- **`team.TeamSession`** — Loaded team state: config, agents map, MCP servers, skills, workspace
- **`team.Coordinator`** — Orchestrator: delegates via `agent`; provides `finish`/`load_skill`; manages sidecar, guard, auto-skills, dry-run, plan-first
- **`team.TeamContext`** — Container holding session + coordinator + sessionData for one team
- **`skill.SkillDef`** — Parsed from `SKILL.md`; name, description, content, summary
- **`sidecar.Sidecar`** — Auxiliary LLM agent for skill matching and guard review
- **`tui.Model`** — Bubble Tea TUI state machine
- **`agent.ProviderManager`** — Manages multiple LLM providers simultaneously

## Architecture

### Data Flow

```
User Prompt
    │
    ▼
main.runTeam()
    │
    ├─► team.NewTeamRegistry(searchPaths)
    ├─► TeamRegistry.Discover() ── Scans .agent-teams/ dirs; a directory is a team if it has team.yml/team.yaml **or** at least one *.md file (the directory basename becomes the team name when no team.yml is present)
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

### Coordinator Features

| Feature | Description |
|---------|-------------|
| **Multi-Provider** | `ProviderManager` routes models across multiple providers (Ollama, OpenAI, etc.) |
| **Sidecar** | Lightweight LLM for skill matching (`sidecarModel`) and guard review (`guardModel`) |
| **Guard System** | Rule-based output review per agent (`guard` field in .md); triggers `guardModel` sidecar on violation. **Fails closed** — if the reviewer errors/times out, the tool call is denied, not allowed |
| **Deliverable Verification** | Per-task `verify` shell command (in the `agent` tool); runs after the agent reports success and before the task is marked done. A non-zero exit fails the task and triggers a retry — an objective, non-LLM check that the artifact actually exists |
| **Auto-Skills** | Sidecar-driven skill matching via `--auto-skills` or `auto-skills: true` in team.yml |
| **Plan-First** | Agents must submit plans before execution if `--plan` or `plan: true` |
| **Dry Run** | Preview-only execution (`--dry-run`) — no LLM calls |
| **TUI Mode** | Real-time Bubble Tea dashboard via `--tui` |
| **Step Confirm** | Pause before each batch via `--steps` |
| **Report Gen** | Markdown report after execution via `--report` |
| **Template Vars** | `--var` / `--var-file` / `vars` in team.yml for agent prompts |
| **Default Team** | Built-in ad-hoc team (coordinator + Helper) via `--default` — no `.agent-teams/` required |

## CLI Flags

| Flag | Short | Default | Description |
|------|-------|---------|-------------|
| `--provider-url` | — | "" (hufu.yaml or `http://localhost:11434/v1`) | Ollama or OpenAI-compatible API base URL |
| `--provider-api-key` | — | "" | Provider API key |
| `--verbose` | `-v` | `false` | Show full agent text output in real-time |
| `--workspace` | `-w` | `""` (`<cwd>/workspace`) | Workspace directory path |
| `--new` | `-n` | `false` | Archive old session and start fresh |
| `--temp` | `-t` | `false` | Use a temporary directory as workspace |
| `--steps` | `-s` | `false` | Pause for user confirmation before each batch of worker tasks |
| `--agent-team` | — | `""` | Agent team name to load |
| `--agent-team-search-path` | — | `""` | Team search paths (comma-separated), defaults to `.agent-teams/,~/.agent-teams/` |
| `--memory` | — | `false` | Enable long-term memory (RAG vector search) |
| `--memory-model` | — | `""` | Embedding model for memory (default: `qwen3-embedding:4b`) |
| `--archive-memory` | — | `false` | Archive session summary to memory and exit |
| `--show-history` | — | `false` | Show previous session history on resume |
| `--dry-run` | — | `false` | LLM-free preview of skill matching and available agents (does not call the model, does not execute agents) |
| `--tui` | — | `false` | Show a Bubble Tea TUI for real-time task tracking |
| `--rbash` | — | `false` | Use restricted bash (rbash) for the bash tool |
| `--no-net` | — | `false` | Block all network access for agent subprocesses |
| `--force-mcp` | — | `false` | Force MCP mode: disable built-in execution/network tools (bash, sudo, ssh, golang, lua, download, fetch, agentic_fetch), require MCP servers |
| `--direnv` | — | `false` | Load `.envrc` / `.env` environment for the bash tool |
| `--think` | — | `false` | Show coordinator decision reasoning |
| `--plan` | — | `false` | Force plan-first mode: agents must submit plans before executing |
| `--auto-skills` | — | `false` | Enable automatic skill detection via sidecar / LLM matching |
| `--report` | — | `false` | Generate a full execution report as a markdown file |
| `--default` | — | `false` | Use the built-in default team (coordinator + Helper); no `.agent-teams/` directory required (mutually exclusive with `--agent-team`). Discovers global skills from `~/.agents/skills/` and respects `--skill` forced skills. |
| `--helper-tools` | — | `""` | Comma-separated extra tools to enable for the default Helper worker when `--default` is set (e.g. `bash` or `bash,sudo,ssh`). Whitespace around each entry is trimmed; empty entries are dropped. Empty = Helper's baseline read-only toolset. |
| `--model` | — | `""` | Override default model for the active team (e.g. `ollama/qwen3:8b`); highest priority — overrides agent .md, team.yaml, and hufu.yaml |
| `--temperature` | — | `""` | Override sampling temperature (e.g. `0.2`) |
| `--max-tokens` | — | `""` | Override max output tokens (e.g. `4096`) |
| `--top-p` | — | `""` | Override top-p value (e.g. `0.9`) |
| `--top-k` | — | `""` | Override top-k value (e.g. `40`) |
| `--sidecar-model` | — | `""` | Override sidecar model used for skill matching (e.g. `ollama/qwen3:1b`); falls back to `--model` when not set |
| `--guard-model` | — | `""` | Override guard model used for output review (e.g. `ollama/qwen3:8b`); falls back to `--model` when not set |
| `--timeout` | — | `0` | Override agent/coordinator timeout in seconds (e.g. `1800` for 30 min). `0` = use team/agent default. Highest priority — overrides agent `.md` and `team.yaml`. |
| `--fix` | — | `""` | Analyze previous execution data and suggest improvements |
| `--skill` | — | `nil` | Force-load specific skills (repeatable) |
| `--var` | — | `nil` | Set template variable `key=value` (repeatable) |
| `--var-file` | — | `nil` | Read template variables from a file (repeatable) |
| `--unattended` | — | `false` | No-human mode: `ask_user` returns safe defaults instead of blocking on stdin, `--steps`/`--tui` are auto-disabled, and only allowlisted tools may run (deny-by-default even without a TTY) |
| `--max-duration` | — | `0` | Budget: max total wall-clock seconds before forcing wrap-up (`0` = unlimited) |
| `--max-total-tokens` | — | `0` | Budget: max cumulative LLM tokens before forcing wrap-up (`0` = unlimited) |

### Unattended Operation

For running with no human watching (cron, queue worker, CI):

- **`--unattended`** is the master switch. It makes `ask_user` non-blocking (choice → first option; free-text → an error telling the agent to proceed on its own), disables `--steps`/`--tui`, and lets the allowlist run without a TTY while still denying non-allowlisted tools.
- **Budgets** (`--max-duration`, `--max-total-tokens`, or team.yaml `max-duration` / `max-total-tokens`) are the circuit-breaker: when exceeded, `ExecuteTasks` forces wrap-up and refuses new tasks, emitting a notifiable `budget_exceeded` event. Token usage is aggregated from each agent run's `fantasy.StepResult.Usage`.
- **Notifications** fire on `done` / `error` / `wrap_up` / `budget_exceeded` / `needs_human` via the `notify` config (OSC and/or `command`). `needs_human` fires when an agent calls `ask_user` in unattended mode so an operator can follow up out-of-band.
- **Acceptance** (`acceptance:` in team.yaml) is an objective whole-run gate run at `finish`. In interactive mode a non-zero exit appends a failure note to the result and emits an `error` notification. In **unattended mode** it drives a self-healing loop: up to 2 retries (the coordinator is told to fix the failures and call `finish` again, tracked by `selfHealingAttempts`); if still failing, it runs **rollback** (`rollback:` command, or a default `git reset --hard && git clean -fd` when the project is a git repo) and reports the outcome.
- **Mid-task crash-resume (re-attaching in-flight workers):** every task status change checkpoints the todo list to `session.json` (`TodoList.onChange → saveCheckpoint`, including full task `Output`). On the next non-`--new` run the CLI `LoadSession`s it and `SetSessionData` restores the tasks and pre-populates the result cache from completed ones. At the start of `Run()`, `ResumeInterruptedTasks` re-drives every task left in a non-terminal state (`in_progress` / `paused` / `planned` / `pending`) on its **original todo ID**, in ascending-ID order so dependencies run first; `done`/`skipped`/`error` tasks are left as-is (completed work is reused, not redone). It is a no-op on a fresh run or with `--new`.
- **Triggers (by design, external):** scheduling is delegated to the host (system cron / systemd timer / queue) invoking `hufu` per run; session state persists under `workspace/` (`session.json`, `stm.md`, `ltm.md`).

### Prompt Syntax

- `@<team-name> <task>` — Switch to a team and delegate the task
- `@<agent-name> <task>` — Invoke a specific agent directly in the current team
- Plain text — Passed to the current team's coordinator
- Multiple teams in one prompt: `@team-a research @team-b write @team-a summarize`

## TUI System

The TUI is built on the **Bubble Tea** framework. `Model.Update(msg)` is a **pure function** `(Model, Msg) -> (Model, Cmd)`. No I/O or global mutations in `Update()`.

### Model Fields (`internal/tui/tui.go`)

**Never remove or rename fields without updating `View()` and `Update()` together.**

| Field | Type | Purpose |
|-------|------|---------|
| `prompt` | `string` | User's original prompt displayed in top widget |
| `tasks` | `[]*team.TodoItem` | All tasks from coordinator TODO list |
| `logs` | `map[string][]string` | todoID → rendered log lines (task output buffer) |
| `coordItem` | `*team.TodoItem` | Coordinator pseudo-task item |
| `col` | `int` | Focused column: 0=pending, 1=planned, 2=in_progress, 3=done, 4=skipped, 5=error |
| `row` | `int` | Cursor position within focused column |
| `scrollOff` | `[6]int` | Scroll offset per column |
| `inDetail` | `bool` | Detail log overlay active |
| `detailID` | `string` | ID of task in detail view |
| `vp` | `viewport.Model` | Bubble Tea viewport for scrollable views |
| `vpReady` | `bool` | Viewport initialized |
| `inMemory` | `bool` | Memory view overlay active |
| `memoryVP` | `viewport.Model` | Separate viewport for memory content |
| `memoryReady` | `bool` | Memory viewport initialized |
| `inConfirm` | `bool` | Quit confirmation dialog |
| `confirmChoice` | `int` | 0=No, 1=Yes, 2=Force |
| `width` / `height` | `int` | Terminal dimensions |
| `finished` | `bool` | Set when `FinishedMsg` received |
| `statusText` | `string` | Current status line text |
| `result` | `string` | Final coordinator answer |
| `inAskUser` | `bool` | ask_user dialog active |
| `ask` | `askState` | ask_user dialog state |
| `inPromptInput` | `bool` | Prompt injection dialog active |
| `promptInput` | `textinput.Model` | Text input for prompt injection |
| `PromptInjectCh` | `chan string` | Forwards injected prompts to coordinator |
| `inSearch` | `bool` | Search overlay active |
| `searchInput` | `textinput.Model` | Search text input |
| `searchQuery` | `string` | Last search query |
| `searchResults` | `[]*team.TodoItem` | Matching tasks |
| `searchIdx` | `int` | Current match index |
| `inInfo` | `bool` | Team info panel active |
| `teamInfo` | `TeamInfo` | Team metadata |
| `wrapUpRequested` | `bool` | First Ctrl+C pressed |
| `WrapUpCh` | `chan struct{}` | Ctrl+C → coordinator wrap-up |
| `ReportCh` | `chan struct{}` | `r` key → report generation |
| `mouseEnabled` | `bool` | Mouse tracking active |
| `mouseManuallyEnabled` | `bool` | User explicitly toggled mouse |
| `inActivityLog` | `bool` | Full-screen activity log |
| `recentLogs` | `[]string` | Circular buffer (max 500 entries) |
| `detailRefreshScheduled` | `bool` | Debounce flag for detail viewport |
| `inVisual` | `bool` | VISUAL mode in detail view |
| `cursorLine` | `int` | Current line in detail logs |
| `visualStart` / `visualEnd` | `int` | Selection range |

### View Priority Order (CRITICAL)

`Model.View()` checks overlays in this strict priority. Adding a new overlay bool MUST insert it in the correct position:

1. `inAskUser` — Modal dialog, centered
2. `inInfo` — Team info panel, centered
3. `inSearch` — Search textinput, centered
4. `inPromptInput` — Prompt injection textinput, centered
5. `inConfirm` — Quit confirmation (No/Yes/Force), centered
6. `inDetail` — Task log viewport + header + footer
7. `inActivityLog` — Full-screen recent logs viewport
8. `inMemory` — STM/LTM content viewport
9. Default — 6-column Kanban dashboard

### Key Bindings Reference

#### Global (Column Dashboard)

| Key | Action |
|-----|--------|
| `j` / `k` / `↓` / `↑` | Move cursor in column |
| `h` / `l` / `←` / `→` | Switch column |
| `tab` | Cycle column (0→5→0) |
| `g` | First item in column |
| `G` | Last item in column |
| `ctrl+d` | Half-page down |
| `ctrl+u` | Half-page up |
| `enter` | Open detail view |
| `/` | Open search |
| `n` / `N` | Next / previous search match |
| `i` | Open team info |
| `c` | Open prompt injection dialog |
| `a` | Toggle activity log |
| `m` | Toggle mouse |
| `M` | Open memory view |
| `q` | Quit (only when finished) |
| `r` | Generate report (only when finished) |
| `esc` | Quit confirmation (or clear search) |
| `ctrl+c` | Request wrap-up (1st) / quit (2nd) |

#### Detail View

| Key | Action |
|-----|--------|
| `esc` / `backspace` | Return to columns |
| `j` / `k` / `↓` / `↑` | Scroll cursor line |
| `g` / `G` | First / last log line |
| `v` | Enter VISUAL mode |
| `y` | Copy selection (VISUAL only) |
| `n` / `N` | Next/prev search match |
| `i` / `M` / `m` / `q` / `r` / `ctrl+c` | Same as global |

#### VISUAL Mode (inside Detail)

| Key | Action |
|-----|--------|
| `j` / `k` / `↓` / `↑` | Extend selection |
| `g` / `G` | Extend to top/bottom |
| `y` | Yank to clipboard (OSC52), exit VISUAL |
| `esc` / `v` | Cancel selection |

#### Activity Log

| Key | Action |
|-----|--------|
| `esc` / `q` / `a` / `enter` | Close |
| `j` / `k` / `↓` / `↑` | Scroll |
| `g` / `G` | Top / bottom |
| `space` | Page down |
| `b` | Page up |

#### Memory View

| Key | Action |
|-----|--------|
| `esc` / `backspace` | Return |
| `g` / `G` | Top / bottom |
| `q` / `ctrl+c` | Handle quit/wrap-up |
| *(others)* | Forwarded to memoryVP |

#### Team Info Panel

| Key | Action |
|-----|--------|
| `esc` / `q` / `i` / `enter` | Close |
| `ctrl+c` | Handle wrap-up |

#### Search Dialog

| Key | Action |
|-----|--------|
| `enter` | Execute search, jump to first match |
| `esc` / `ctrl+c` | Cancel |
| *(others)* | Forwarded to searchInput |

#### Prompt Input Dialog

| Key | Action |
|-----|--------|
| `enter` | Submit injection |
| `esc` / `ctrl+c` | Cancel |

#### Quit Confirmation

| Key | Action |
|-----|--------|
| `←`/`h` / `→`/`l` / `tab` | Cycle choice |
| `enter` | Submit choice |
| `esc` / `n` | Cancel (No) |
| `y` | Yes (wrap-up) |
| `f` | Force quit |

#### ask_user Dialog (`internal/tui/ask_user.go`)

| Key | Action |
|-----|--------|
| `enter` | Submit answer |
| `ctrl+c` | Cancel |
| `↑` / `k` | Move up |
| `↓` / `j` / `tab` | Move down |
| `space` | Toggle checkbox (multiple choice) |

### TUI Agent Safety Rules

1. **Never change `View()` priority order** without updating all bool checks consistently
2. **Always handle new `tea.Msg` types** in both `Update()` and any reporter translation (`makeTUIReporter`)
3. **Preserve `Update()` purity** — no I/O, no global state mutations
4. **Test new key bindings** with `tea.KeyMsg` in tests before claiming completion
5. **ANSI-aware wrapping** — use existing `wrapLine()` for any new text rendering
6. **Viewport lifecycle** — always check `vpReady` before `vp.SetContent()`
7. **Mouse state consistency** — `mouseEnabled` and `mouseManuallyEnabled` must stay in sync
8. **Detail debounce** — any new log-sending code must respect `detailRefreshScheduled`
9. **FinishedMsg transitions** — Pending→Skipped, InProgress→Done, Paused→Done (safety net)
10. **Window resize** — new fields must be handled in `tea.WindowSizeMsg` branch

### tea.Msg Types

#### Public (sent from goroutines via p.Send)

| Message | Fields | Purpose |
|---------|--------|---------|
| `TasksUpdatedMsg` | `Items []*team.TodoItem` | Update task column data |
| `TaskLogMsg` | `TodoID string, Line string` | Append log line to task detail |
| `CoordItemMsg` | `Item *team.TodoItem` | Create/update coordinator pseudo-task |
| `CoordStatusMsg` | `Status team.TaskStatus` | Update coordinator task status |
| `FinishedMsg` | (none) | All work complete |
| `StatusBarMsg` | `Text string` | Update 1-line status bar |
| `ResultMsg` | `Text string` | Display final result |
| `TeamInfoMsg` | `Info TeamInfo` | Load team metadata |
| `WrapUpMsg` | (none) | Wrap-up request |
| `AskUserCancelMsg` | (none) | Cancel ask_user dialog |

#### Internal

| Message | Fields | Purpose |
|---------|--------|---------|
| `AskUserMsg` | `Question, Type, Options, AllowAny, ReplyCh` | Trigger ask_user modal |
| `detailRefreshMsg` | (none) | Debounced viewport re-render |
| `copySuccessMsg` | `Lines int` | OSC52 clipboard copy confirmation |

### StatusEvent → tea.Msg Translation

`makeTUIReporter` in `cmd/hufu/display.go` translates coordinator `StatusEvent` to TUI messages:

| StatusEvent.Type | TUI Action |
|------------------|-----------|
| `todos_updated` | `TasksUpdatedMsg` |
| `plan_approved` | `StatusBarMsg` with checkmark |
| `wrap_up_phase` | `TasksUpdatedMsg` + wrap-up status |
| `start` | `TaskLogMsg` + `CoordItemMsg`/`CoordStatusMsg`, starts thinking ticker |
| `step` | Stop thinking ticker, `TaskLogMsg` |
| `tool_call` | Stop ticker, `TaskLogMsg`, update status bar |
| `tool_result` | `TaskLogMsg`, restart ticker |
| `cache_hit` | Stop ticker, cached log line |
| `text` | Buffered into `textBufs` map |
| `done` | Stop ticker, flush buffered text, done line |
| `error` | Stop ticker, flush text, error line |
| `think_*` | Various `TaskLogMsg` lines |

### Thinking Tracker

- `thinkingTickInterval = 5s`
- Per-todoID background goroutine
- Started on `start`, stopped on `step`/`tool_call`/`tool_result`/`done`/`error`/`cache_hit`
- Sends `StatusBarMsg` with elapsed LLM wait time

## Team Configuration Format

### team.yml

`team.yml` (or `team.yaml`) is **optional**. A directory is recognized as a team if it contains this file **or** at least one `*.md` agent definition. When the file is absent, the directory basename is used as the team name. When present, the file's `name:` field takes precedence (if non-empty). When the file is absent, all other team-level settings fall back to built-in defaults (`max-rounds: 10`, `workspace: workspace`, `timeout: 600`, `max-retries: 2`).

Complete configuration reference:

```yaml
# === Optional (recommended) Fields ===
name: my-team

# === Optional Fields ===
description: "My development team"

# === Execution Control ===
max-rounds: 10
max-steps: 30
timeout: 600
max-retries: 2
max-concurrent: 8

# === Workspace ===
workspace: workspace

# === Model Settings ===
model: ollama/qwen3:8b
temperature: "0.7"
max-tokens: "4096"
top-p: "0.9"
top-k: "40"

# === Provider ===
provider-url: http://localhost:11434/v1
provider-api-key: ""

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

# === Sidecar / Guard Models ===
sidecar-model: qwen3:1b
guard-model: qwen3:8b

# === Skills ===
skills: code-review,git-commit
skills-exclude: debug
auto-skills: false

# === Security ===
allowed-paths: ["/home/user/projects", "/tmp"]
restricted-path: "/etc"
no-net: false
force-mcp: false
shell: bash

# === Unattended Operation ===
unattended: false           # no human present: ask_user auto-answers, deny-by-default tools
max-duration: 0             # budget: max total wall-clock seconds (0 = unlimited)
max-total-tokens: 0         # budget: max cumulative LLM tokens (0 = unlimited)
acceptance: ""              # shell command run at finish; non-zero exit = run not accepted
rollback: ""                # unattended: command run after self-healing fails (default: git reset --hard && git clean -fd)

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

### Agent .md files

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
temperature: "0.7"
max-tokens: "4096"
top-p: "0.9"
top-k: "40"
timeout: 300
max-retries: 2
max-steps: 50
provider-url: http://localhost:11434/v1
provider-api-key: ""
allowed-paths: ["src/", "tests/"]
restricted-path: "/etc"
no-net: false
shell: bash
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
Your system prompt here.
```

### Frontmatter Fields (Agent .md)

| Field | Required | Default | Description |
|-------|----------|---------|-------------|
| `name` | ✅ | — | Agent name, used for `@<name>` invocation |
| `description` | ❌ | — | Agent description |
| `role` | ❌ | `worker` | Role (`worker` or `coordinator`) |
| `tools` | ❌ | — | Available tools (string or YAML list) |
| `skills` | ❌ | — | Skills to load (string or YAML list) |
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
| `shell` | ❌ | Team default | Default shell for agent's MCP tools (e.g., `bash`, `zsh`, `nu`, or full path like `/usr/bin/nu`) |
| `mcp-tools` | ❌ | — | Custom MCP tools (dict format: `{tool-name: {cmd, desc, inputs, shell, dir}}`) |

### Team Frontmatter Fields (team.yml)

| Field | Description |
|-------|-------------|
| `name` | Team name. File `team.yml`/`team.yaml` is optional; if absent or `name` is empty, the directory basename is used. |
| `description` | Team description |
| `max-rounds` | Maximum coordination rounds (default: 10) |
| `max-steps` | Agent default max steps (default: 30) |
| `timeout` | Timeout in seconds (default: 600) |
| `max-retries` | Maximum retries (default: 2) |
| `max-concurrent` | Maximum concurrent worker tasks (default: 8) |
| `workspace` | Workspace directory (default: "workspace") |
| `model` | Default model name |
| `temperature` | Temperature value |
| `max-tokens` | Maximum output tokens |
| `top-p` | Top P value |
| `top-k` | Top K value |
| `provider-url` | Provider URL override |
| `provider-api-key` | Provider API key override |
| `providers` | Multi-provider pool map |
| `model-list` | Custom model list |
| `sidecar-model` | Lightweight model for skill matching |
| `guard-model` | Model for guard / review tasks |
| `skills` | Comma-separated skill names |
| `skills-exclude` | Skills to exclude |
| `auto-skills` | Enable automatic skill detection |
| `mcp-servers` | MCP server configurations |
| `allowed-paths` | Allowed file system paths |
| `restricted-path` | Restricted file system path |
| `no-net` | Block network access |
| `force-mcp` | Force MCP mode: disable built-in execution/network tools |
| `shell` | Default shell for all agents in this team (searched from PATH, e.g., `bash`, `zsh`, `nu`) |
| `unattended` | Run with no human: `ask_user` auto-answers, `--steps`/`--tui` off, deny-by-default tools (CLI `--unattended` also sets this) |
| `max-duration` | Budget: max total wall-clock seconds before forced wrap-up (`0` = unlimited) |
| `max-total-tokens` | Budget: max cumulative LLM tokens before forced wrap-up (`0` = unlimited) |
| `acceptance` | Shell command run at `finish` as a whole-run gate; non-zero exit marks the run not-accepted |
| `rollback` | Unattended: command run after self-healing is exhausted on acceptance failure (default: `git reset --hard && git clean -fd` when a git repo) |
| `vars` | Template variables map |
| `notify` | Notification configuration |

## Workspace Layout

```
workspace/
├── session.json              # Structured session data
├── chat_history.md           # Human-readable conversation transcript
├── session_history.json      # Raw conversation message history
├── execution_trace.log       # Detailed execution trace log (TUI mode only)
├── stm.md                    # Short-term memory (active session)
├── tasks/                    # Per-task records
│   └── {team-name}/
│       └── {agent-name}/
│           └── {timestamp}.md
├── shared/                   # Files shared between agents
│   └── skills/               # Copied SKILL.md files
├── status/                   # Agent status files (working/done/error)
├── history/                  # Archived session files
└── logs/                     # All system/debug logs
    ├── audit/                # Tool audit log (audit-{date}.jsonl)
    ├── llm/                  # LLM request/response logs
    │   └── {team-name}/
    │       └── {agent-name}/
    │           └── llm.log
    └── stm/                  # STM round checkpoints (stm_rN.md)
```

## Session Management

- Each team maintains its own session data independently
- On team switch: previous team's session is saved (`SaveSessionMD`)
- On interrupt (Ctrl+C): current team's session is saved
- `--new` archives and starts a fresh session per active team

## Task Tracking (TodoList)

```go
type TodoItem struct {
    ID     string      // Auto-incrementing: "1", "2", ...
    Agent  string      // Agent name
    Desc   string      // Task description
    Status TaskStatus  // TaskPending / TaskInProgress / TaskDone / TaskError
    Detail string      // Error detail (when Status == TaskError)
}

type TodoList struct {
    mu    sync.Mutex
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

## Readline Integration

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
- **`save_skill`** — Save skill definition
- **`finish`** — Signal completion with final answer
- **`ask_user`** — Request user input

### Worker Tools (18 total)

- `bash` — Execute shell commands (timeout: 120s, max 600s)
- `sudo` — Execute commands with root privileges
- `ssh` — Execute commands on remote hosts via SSH
- `scp` — Transfer files to/from remote hosts
- `view` — Read file contents (with line numbers)
- `write` — Write file contents, auto-creates directories
- `edit` — Edit files by replacing exact text
- `multiedit` — Atomically apply multiple edit operations
- `grep` — Search file contents using regular expressions
- `glob` — Search files using glob patterns
- `ls` — List directory contents in a tree structure
- `lua` — Execute Lua code in a sandbox
- `golang` — Execute Go code via the yaegi interpreter
- `ask_user` — Ask the user a question (multiple choice / free text)
- `download` — Download a file from a URL
- `fetch` — Fetch URL content (text/markdown/html)
- `agentic_fetch` — Fetch and analyze URL content
- `random` — Generate random numbers / UUIDs
- `math` — Evaluate mathematical expressions

### Always-included Tools

- **`agent`** — Create a sub-agent to execute a specific task (always available)
- **`todo`** — Manage task list (always available)
- **`memory_save`** — Save knowledge to long-term memory
- **`memory_query`** — Search long-term memory

## Core Mandates for Agents

1. **Always Write Unit Tests:** Whenever you modify existing code or implement a new feature, you **MUST** write or update the corresponding unit tests to verify your changes. A task is considered incomplete without passing tests.
2. **Prioritize Readability:** Code should be idiomatic, clear, and well-documented.
3. **Follow Workspace Standards:** Adhere strictly to the established architectural patterns and conventions defined in `AGENTS.md` and related documentation.

## TUI Testing Guidelines

The TUI is built using the Bubble Tea framework, which emphasizes a pure functional approach to state management. To maintain high code quality and prevent regressions, agents should follow these testing patterns:

### 1. State Machine Testing (Pure Function)

Since the `Update(msg)` function is a pure state transition `(Model, Msg) -> (Model, Cmd)`, we can test TUI logic without a terminal environment.

**Pattern:**
1. Initialize a `Model` with `New()`.
2. Call `m.Update(msg)` with a specific `tea.Msg` (e.g., `tea.KeyMsg`).
3. Assert that the resulting `Model` state (fields like `row`, `col`, `inDetail`) matches expectations.

**Example:**
```go
func TestUpdate_Navigation(t *testing.T) {
    // 1. Setup
    m := New("test prompt", TeamInfo{TeamName: "test-team"})
    m.width = 100
    m.height = 40
    m.tasks = []*team.TodoItem{
        {ID: "1", Status: team.TaskPending, Desc: "Task 1", Agent: "A1"},
        {ID: "2", Status: team.TaskPending, Desc: "Task 2", Agent: "A1"},
    }

    // 2. Action: Simulate pressing 'j' (Down)
    m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})

    // 3. Expectation: Row index should increment
    if m2.(Model).row != 1 {
        t.Errorf("Expected row 1 after 'j', got %d", m2.(Model).row)
    }
}
```

### 2. View Rendering Verification

To ensure the UI displays critical information, test the output of the `View()` function.

**Pattern:**
1. Setup a `Model` with specific data.
2. Call `m.View()`.
3. Use `strings.Contains()` to verify that important labels, descriptions, or status indicators are present in the rendered string.
4. Use `lipgloss.Width()` for visible width checks instead of `len()`, as `View()` output contains ANSI sequences.

### 3. Specification-Driven Testing (Speckit)

Follow the **Speckit x OpenCode** workflow defined in `internal/tui/OPENCODE_INTEGRATION.md`:
1. Define behavior in `*_SPEC.md` (Behavioral Checklist).
2. Create technical plans in `.opencode/plans/*.md`.
3. Derive test cases directly from the Checklist to ensure 100% requirement coverage.

## Security & Tooling Standards

### 1. Agent Tool Trust (Implicit Allowlist)

**Specification:** The `tools` field in an agent's `.md` definition file is considered a **trusted configuration**.
- Any tool listed in the agent's Markdown file is automatically added to that agent's **explicit allowlist**.
- Tools in this implicit allowlist **bypass** the global `team.yaml` restrictions and will not prompt the user for permission.
- This allows for fine-grained, agent-specific capabilities while maintaining global safety defaults.

### 2. Sandbox Hardening (Golang & Lua)

- **Golang**: Dangerous packages like `os/exec`, `net`, `syscall`, and `unsafe` are blocked in the `yaegi` interpreter. Standard file I/O via `os` is permitted within the workspace.
- **Lua**: Native `os.execute` and `io.popen` are restricted to prevent unauthorized shell command execution.

### 3. Bash Tool Security

- `--rbash` flag enables restricted bash mode
- `--no-net` blocks network access for agent subprocesses
- `--force-mcp` disables built-in execution/network tools (bash, sudo, ssh, golang, lua, download, fetch, agentic_fetch), forcing use of MCP servers
- `--direnv` loads `.envrc`/`.env` environment files
- Dangerous commands (curl, wget, sudo, apt, etc.) are blocked by default

### 4. No Hardcoded Sensitive Information in Tracked Files

**Specification:** Tracked configuration files must never contain environment-specific identifiers. Only generic, universal values are allowed.

- **Applies to:** `hufu.yaml`, `~/.config/hufu/hufu.yaml` (local-only), `team.yaml`, agent `.md` frontmatter, Go source defaults in `internal/config/`, example configs, CI workflows, documentation.
- **Forbidden categories:**
  - **IPs:** Private IPv4 (`10.x`, `172.16-31.x`, `192.168.x`), public IPs
  - **API keys / tokens in config values:** `sk-xxx`, raw tokens; env-var refs like `$OPENAI_API_KEY` in config are allowed
  - **Embedded secrets:** `password=xxx`, `secret=xxx` in connection strings; env-var refs are allowed
  - **Internal hostnames:** e.g. `mycompany.okta.com`, `vpn-gateway.local`, `dc01.corp`
  - **Custom ports on specific hosts:** e.g. `:5432`, `:6379`, `:11434` when combined with a non-generic host
  - **Non-standard cloud endpoints:** e.g. `mycompany.cognitiveservices.azure.com`; standard public endpoints like `api.openai.com` are allowed
- **Allowed values:**
  - `localhost`, `127.0.0.1`, `0.0.0.0`
  - Default ports with generic hosts (e.g., `ollama:11434`, `api.openai.com`, `github.com`)
  - Env-var placeholders in config: `$OLLAMA_HOST`, `${PROVIDER_URL}`, `$DB_PASS`
  - GitHub Actions `secrets.GITHUB_TOKEN` syntax in CI workflows
- **Even commented-out values are forbidden** — git history preserves them forever. If a temporary override is needed, put it in a gitignored `hufu.yaml` override or an env var.
- **Verification before commit (config files only):**

  ```bash
  # Check for IPs
  git grep -nE '([0-9]{1,3}\.){3}[0-9]{1,3}' -- '*.yaml' '*.yml' '*.toml' '*.json' '*.env*'
  # Check for common secret patterns (excluding env-var references)
  git grep -nEi 'sk-[0-9a-zA-Z]{20,}|password\s*=\s*["'\''][^$][^'\''"]*|secret\s*=\s*["'\''][^$]' -- '*.yaml' '*.yml' '*.toml' '*.json' '*.env*'
  ```

  Both must return zero hits in config files. Test fixtures (`*_test.go`) and educational examples in tool description strings are exempt — they reference IPs as test data or anti-pattern examples, not as live configuration.

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

11. **`idleWarningTimer`** — Tracks idle time and prints a warning to stderr after 30s of no activity. Resets on every status event.

12. **Workspace flag is shared** — `--workspace` sets the workspace for all teams; each team's session uses this combined workspace path.

13. **`teamStyle`** — New lipgloss style (color 13/magenta) added for displaying team names in CLI output, distinct from `agentStyle` (cyan).

14. **go.mod indirect deps** — `charm.land/fantasy`, `github.com/charmbracelet/lipgloss`, `github.com/spf13/cobra`, `github.com/mark3labs/mcp-go` are marked `// indirect` but directly imported. `go mod tidy` would fix this.

15. **Prompt injection via Ctrl+Z / SIGUSR1** — While a coordinator is running, users can press Ctrl+Z (SIGTSTP) or send SIGUSR1 to inject an additional prompt. The prompt is enqueued and processed after the current coordinator round completes. The coordinator receives a continuation message instructing it to adjust tasks. New prompts do NOT interrupt or cancel running agents.

16. **ContinueWithPrompt preserves conversation history** — `Coordinator.ContinueWithPrompt()` reuses the `conversationHistory` field (accumulated `fantasy.Message` from previous rounds) so the coordinator has full context when processing an injected prompt. This is different from `Run()` which starts fresh.

17. **Stdin mutex** — `tools.StdinMu` is a shared `sync.Mutex` that serializes reads from stdin. Both `ask_user` (agent tool) and `promptInjector.promptAndEnqueue()` (Ctrl+Z handler) lock this mutex before reading, preventing garbled input.

18. **promptInjector** — Buffered channel (size 16) that receives prompts from SIGTSTP/SIGUSR1 signal handlers. `poll()` is non-blocking; `runWithInjection()` checks after each coordinator round.

19. **promptInjector carries PromptReader** — The `promptInjector` struct now embeds `*readline.PromptReader`. When nil, `promptAndEnqueue` returns immediately without reading. This ensures graceful degradation when readline is unavailable.

20. **setupPromptSignals returns cleanup func** — `setupPromptSignals` now returns a `func()` that stops signal handlers and closes channels. Called via `defer setupPromptSignals(injector)()` to ensure cleanup on function exit.

21. **Graceful shutdown with Ctrl+C** — First Ctrl+C sends SIGINT → `activeCoordinator.SetWrapUp()` → `SetWrapUp()` reports `StatusEvent{Type: "wrap_up"}` (CLI displays `─── WRAP UP ───`) → `ExecuteTasks` checks `IsWrapUp()` and refuses to delegate new tasks → Second Ctrl+C forces `cancel()` for immediate exit. On 2nd Ctrl+C the handler prints `Coordinator.GetCurrentStatus()` (e.g. `model agent=helper model=ollama/qwen3:8b step=3 (12s elapsed)`) so the user can see which stage is stuck. If main does not return within 8 s, an `os.Exit(130)` watchdog fires after a best-effort `SaveSession` + `SaveSessionMD` so the user never has to SIGKILL with no output.

22. **Wrap-up mechanism** — `promptInjector.wrapUpCh` (buffered channel, size 1) + `wrapUpRequested atomic.Bool` flag. `injectWrapUp()` sets the flag and sends to channel (non-blocking). `IsWrapUpRequested()` atomically checks. `runWithInjection()` uses `select` to handle both normal prompts and wrap-up in one select statement.

23. **StatusEvent builder pattern** — `c.newEvent(type)` returns a `StatusEvent` with `Type` and `TeamName` pre-filled. Chainable methods: `withAgent()`, `withMessage()`, `withStep()`, `withTool()`, `withToolResult()`, `withTodos()`. No more manual struct literal construction throughout coordinator code.

24. **Tool call/result output includes agent label** — `formatAgentLabel(event)` renders `"team-name/agent-name"` for cross-team visibility. Tool call output now includes the agent label and `›` separator, with args on a separate indented line. Previous format was all on one line.

25. **Context propagation for wrap-up** — `context.WithCancel(context.Background())` used instead of `signal.NotifyContext` directly, so `cancel()` can be called by the second Ctrl+C while SIGINT is handled separately via a dedicated channel.

26. **Coordinator.wrapUp atomic flag** — `wrapUp atomic.Int32` stores wrap-up state. `SetWrapUp()` stores 1, `IsWrapUp()` checks if 1. Different from the channel-based notification — the atomic flag persists the state across coordinator calls.

27. **runWithInjection uses select not polling** — `select { case <-injector.wrapUpCh: ... case prompt, ok := <-injector.ch: ... default: return }` instead of a polling loop. More efficient and responsive.

28. **Coordinator.GetCurrentStatus() for interrupt diagnostics** — `Coordinator` tracks the current stage (`model` / `tool` / `wrapping_up` / `idle`), current step number, current tool name, and current model ID. `GetCurrentStatus()` returns a human-readable snapshot like `model agent=coordinator model=ollama/qwen3:8b step=3 (12s elapsed)`. Used by the SIGINT handler to tell the user exactly where the program is stuck when they hit Ctrl+C.

28. **executeSegments passes activeCoord to all run paths** — Both coordinator `Run()` and `RunDirectAgent()` paths set/clear `activeCoordinator` so wrap-up signal can target the right instance regardless of which code path is active.

29. **`TodoItem` fields are lowercase** — `TodoItem` uses `ID`, `Agent`, `Desc` (exported), while the legacy `TaskInfo` uses `Agent`/`Task`. Be careful not to confuse them.

30. **Sidecar model defaults to empty** — If `sidecarModel` is empty, sidecar features (skill matching, guard review) are silently disabled. No errors.

31. **Auto-skills uses keyword fallback** — If sidecar skill matching fails (network error, model unavailable), it falls back to keyword matching against skill names and descriptions.

32. **Guard rules are per-agent only** — There is no global guard configuration. Rules are defined in each agent's `.md` frontmatter under `guard:`. Guard review **fails closed**: if the `guardModel` reviewer errors or times out, the tool call is denied (returns a `Guard review unavailable` error), so an unreviewed call never slips through.

33. **Multi-provider aliases** — The `aliases` field in provider configs allows mapping short names (e.g., `gpt-4`) to full model names (e.g., `gpt-4o`).

42. **Per-task deliverable verification (`verify`)** — Each task in the `agent` tool accepts an optional `verify` shell command (`TaskDef.Verify`). After the worker reports success, `executeTask` runs it in the project dir (team/agent shell, falling back to `sh`); a non-zero exit converts the task to a failure and triggers the normal retry path (`coordinator.go`, success branch). This is an objective, non-LLM guard against agents that claim completion without producing the artifact. The extra-models path (`executeTaskWithExtraModels` → `executeSingleAgentWithModel`) threads `verify` into each model's `TaskDef`, so every model is verified (and retried) independently; unverified outputs surface as `*Error*` entries in `mergeAgentResults`.

43. **Repeated-failure early stop** — In `executeTask`'s retry loop, if an attempt fails with the same error as the previous attempt (`sameFailure`, ignoring the `attempt N failed:` prefix), retries stop early instead of burning the full `max-retries` budget on an identical failing action.

44. **Failure reflection always produces a hint** — `reflectOnFailure` uses the sidecar when available, but now falls back to `localFailureHint` (deterministic error classification: timeout / missing file / permission / verification / step exhaustion / duplicate) so retries are never blind even with no sidecar configured.

45. **`team_info` `task_result` action** — Workers can call `team_info` with `action: task_result, agent: <name>` to read the full `## Result` of another agent's most recently completed task (up to ~8 KB), not just the short STM summary — reduces duplicate work across agents.

46. **STM role filter relaxed** — `filterSTMSectionsByRole` now shows `# 決策` (decisions) and `# 待解決` (open questions) to **all** roles, and findings to writers/coders, so cross-cutting knowledge is never hidden by role. Empty/coordinator/orchestrator roles still see everything.

47. **Bounded worker context** — The auxiliary context blocks appended to a worker prompt (prior-agent STM, concurrent tasks, LTM) are assembled under a combined `maxWorkerAuxContextChars` (5000) budget via `assembleContextWithinBudget`, dropping lowest-priority blocks (LTM) first. `appendHistory` truncation preserves the first `conversationHeadKeep` (4) messages — which carry the original goal — instead of dropping the head when sidecar compaction is unavailable.

48. **Unattended is a context value, not just a flag** — `Coordinator.unattended` is propagated into worker/direct/orchestrator task contexts via `tools.UnattendedKey`. `ask_user` and `CheckToolPermission` branch on `tools.IsUnattended(ctx)`. Critically, unattended **skips the step-1 blanket deny** in `CheckToolPermission` (which otherwise denies everything when stdin is not a TTY) and falls through to the allowlist — without this, an unattended/no-TTY run could not use any tool.

49. **ask_user never blocks unattended** — In unattended or non-interactive (`!isInteractiveEnvironment()`) mode, `ask_user` returns `unattendedAskUserResponse` (first option for choices; an error for free-text) instead of reading stdin, and fires `tools.NotifyNeedsHuman`. This matters because `AskUserAwareDeadline` excludes ask_user time from the agent timeout — a stdin read would otherwise hang forever.

50. **Budget circuit-breaker** — `ExecuteTasks` checks `budgetExceeded()` right after the max-rounds check: if `maxWallClock` (from `--max-duration`/team `max-duration`) or `tokenBudget` (`--max-total-tokens`/team `max-total-tokens`) is exceeded, it forces wrap-up and emits a one-shot `budget_exceeded` event (`budgetTripped` guards against repeats). Tokens are summed from `fantasy.StepResult.Usage.TotalTokens` in `runAgentWithStatusAndHistory` (the single agent-run chokepoint).

51. **Notify event vocabulary extended** — `notify` `defaultEvents` now also includes `budget_exceeded` and `needs_human`, with dedicated `formatEvent` cases. The coordinator emits `budget_exceeded`; `needs_human` is pushed from the ask_user tool via the `SetOnNeedsHuman` hook (wired in `cmd/hufu` to the active notifier).

52. **Checkpoint on every status change** — `TodoList.onChange` is wired to `Coordinator.saveCheckpoint` by `SetSessionData`, and fires from `AddBatch`/`UpdateStatus(AndOutput)` and after each tool result. Each checkpoint writes the full todo list (including task `Output`) to `session.json`. This is what makes mid-task crash-resume possible — the on-disk state is at most one status-transition stale.

53. **Mid-task crash-resume** — `Coordinator.ResumeInterruptedTasks` (called at the top of `Run()`) re-drives tasks restored in a non-terminal state (`isInterruptedStatus`: in_progress/paused/planned/pending) via `executeTask` on their **original todo ID**, ordered by `todoIDLess` (ascending numeric ID) so dependencies run first. Completed tasks are skipped and their outputs reused (cache pre-populated in `SetSessionData`). `error` tasks are NOT auto-retried across restarts (they already exhausted retries). Selection/reset is factored into `resetInterruptedTasks` for testing without a provider. No-op on fresh runs / `--new`.

54. **Self-healing acceptance + rollback (unattended)** — On acceptance failure in unattended mode, `finishTool.Run` retries up to `selfHealingAttempts` (2), returning an error response that tells the coordinator to fix and re-`finish`. After exhaustion it calls `runRollback` (team `rollback:` command, or default `git reset --hard && git clean -fd` when `.git` exists). Interactive mode keeps the original non-blocking "append failure note" behavior.

55. **`runAgentWithStatusAndHistory` token aggregation is nil-safe** — `ag.Stream` returns `(*AgentResult, error)` and yields a `nil` result on error; `addStepTokens` is only called when `result != nil` (guarding both the success and error paths) so a failed/aborted stream — including a loop-detection abort — never nil-derefs.

56. **Tool-loop abort** — `runAgentWithStatusAndHistory` tracks the last tool call and a `consecutiveErrCount`; if the same `(toolName,input)` is invoked again after ≥2 consecutive failing results, `OnToolCall` returns an error ("stuck in a loop executing the same failing command") that aborts the stream. Prevents an agent from burning steps re-running an identical failing command.

## Model Configuration Priority

When multiple sources of model configuration are present, the effective value follows this priority order (highest first):

1. **CLI flags** (`--model`, `--temperature`, `--max-tokens`, `--top-p`, `--top-k`, `--sidecar-model`, `--guard-model`) — apply last, override everything below.
2. **Agent `.md` frontmatter** (`model:`, etc.) — per-agent override.
3. **Team `team.yaml`** (`model:`, `sidecar-model:`, `guard-model:`) — per-team default.
4. **`hufu.yaml`** global config — last-resort fallback.

CLI flags are only applied when their values are non-empty. The `--default` team (built-in) starts with empty `Generation`, so the CLI flags are the *only* way to set a model for it without editing `hufu.yaml`.

**Sidecar / guard model fallback:** if `--sidecar-model` or `--guard-model` is not specified, the value of `--model` is used. Explicit `--sidecar-model` / `--guard-model` always win. This lets a user set a single `--model` and have all three roles (main, sidecar, guard) use it.

34. **TUI `--steps` flag combination** — `--tui` and `--steps` cannot be used together because step confirmation requires terminal access that conflicts with the Bubble Tea altscreen.

35. **Dry run requires team** — `--dry-run` must be combined with `--agent-team` or an `@team-name` in the prompt. It preview-only and produces no LLM calls.

36. **Plan-first agents** — When `--plan` is enabled, agents must submit a plan before executing. The coordinator reviews the plan before allowing task execution.

37. **Report generation** — `--report` generates a markdown file with task delegation summary, tool usage statistics, skill usage tracking, and performance metrics (wall clock, token usage).

38. **Template variables** — Variables set via `--var`, `--var-file`, or `vars` in team.yml are interpolated into agent system prompts using `text/template`. Later values override earlier ones.

39. **Chromem-go replaces ChromaDB** — The memory system previously used an external ChromaDB dependency. It now uses `github.com/philippgille/chromem-go` (in-process, no external service required).

40. **TUI `View()` priority is hardcoded** — The strict 9-layer overlay priority in `View()` is not derived from any data structure. Adding a new overlay mode requires manually inserting the bool check in the correct position.

41. **`--force-mcp` disables 8 built-in tools** — When enabled, blocks bash, sudo, ssh, golang, lua, download, fetch, agentic_fetch. Agents must use MCP servers for these operations. Supports 3-level resolution: CLI flag OR global config OR team config, plus per-agent override via `force-mcp: true` in agent .md frontmatter.

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

```
─── SKILLS ───
  ✓ code-reviewer     ×2  researcher, writer
  ✓ git-commit        ×1  developer
```

Displayed via `skillDisplay` struct in `cmd/hufu/display.go`, updated on each `skill_used` event.

### Untracked Skill

| Path | Description |
|------|-------------|
| `.agents/skills/code-reviewer/SKILL.md` | Code review skill (supports local changes and remote PRs) — Global skill at project root (unchanged) |

## Ask-User Timeout Exclusion

When an agent invokes the `ask_user` tool, the time spent waiting for the user to respond **does not count** against the agent's LLM timeout. This prevents long user thinking time from causing premature timeout cancellation.

**Mechanism:** `tools.AskUserAwareDeadline(ctx)` wraps a `context.Context` with a deadline so that while `tools.IsAskUserActive()` is true, the wrapped context reports **no deadline, no done channel, and no error**. When ask_user finishes, the original deadline is restored.

**Wrapped sites** (in `internal/team/coordinator.go`):
- Worker `taskCtx` (line ~4138) — wraps `agentTimeout` so the user response time does not consume the agent's budget.
- Direct `taskCtx` (line ~5608) — same for direct agent invocations.
- Coordinator `orchCtx` (line ~5716) — same for the coordinator's own task ctx.

**Implementation:** `internal/tools/ask_user.go` defines `askUserDeadlineCtx` (private) and the public wrapper `AskUserAwareDeadline`. The wrapper delegates `Value()` to the underlying context so all `context.WithValue` keys remain accessible.

**Race-safety:** `IsAskUserActive()` reads an `atomic.Int32`; `Deadline()` is called at safe points (HTTP client, etc.). The toggle between hidden and visible deadline is observed consistently by all goroutines because `IsAskUserActive` is atomic.
