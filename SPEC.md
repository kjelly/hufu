# SPEC.md - hufu Specification (Go)

## 1. Project Overview

**hufu** is a Go CLI tool that orchestrates teams of LLM agents (via Ollama) to collaboratively accomplish tasks. Teams are discovered by name from configured search paths, and a single prompt can switch between multiple teams or invoke specific agents directly.

- **Module**: `github.com/anomalyco/hufu`
- **Go version**: 1.26.2
- **CLI framework**: `github.com/spf13/cobra`
- **LLM framework**: `charm.land/fantasy` (Charm's agent/LLM abstraction)
- **MCP client**: `github.com/mark3labs/mcp-go`

## 2. Build & Run

```bash
go build ./cmd/hufu          # Build binary
go run ./cmd/hufu [prompt]  # Run directly
go vet ./...                            # Lint
go test ./...                           # Run tests
```

## 3. Go Dependencies

### Direct Imports

| Package | Version | Purpose |
|---------|---------|---------|
| `charm.land/fantasy` | v0.17.2 | Agent/LLM abstraction framework |
| `github.com/aymanbagabas/go-udiff` | v0.4.1 | Unified diff generation |
| `github.com/charmbracelet/bubbles` | v1.0.0 | Bubble Tea component library |
| `github.com/charmbracelet/bubbletea` | v1.3.10 | TUI framework |
| `github.com/charmbracelet/lipgloss` | v1.1.0 | Terminal styling |
| `github.com/ergochat/readline` | v0.1.3 | Interactive prompt input |
| `github.com/mark3labs/mcp-go` | v0.48.0 | MCP client |
| `github.com/philippgille/chromem-go` | v0.7.0 | In-process vector database |
| `github.com/spf13/cobra` | v1.10.2 | CLI framework |
| `github.com/traefik/yaegi` | v0.16.1 | Go interpreter |
| `github.com/yuin/gopher-lua` | v1.1.2 | Lua scripting support |

### Indirect Dependencies

| Package | Version | Purpose |
|---------|---------|---------|
| `github.com/JohannesKaufmann/html-to-markdown` | v1.6.0 | HTML to Markdown conversion |
| `github.com/PuerkitoBio/goquery` | v1.9.2 | HTML parsing |
| `github.com/atotto/clipboard` | v0.1.4 | Clipboard integration |
| `github.com/aymanbagabas/go-osc52/v2` | v2.0.1 | OSC 52 clipboard protocol |
| `golang.org/x/net` | v0.52.0 | HTTP networking |

## 4. CLI Usage

```
hufu [prompt]
  --provider-url                       Ollama API base URL (default: from hufu.yaml or http://localhost:11434/v1)
  --provider-api-key                   Provider API key
  -v, --verbose                        Show full agent text output in real-time
  -w, --workspace                      Workspace directory (default: <cwd>/workspace)
  -n, --new                            Archive old session and start fresh
  -t, --temp                           Use a temporary directory as workspace
  -s, --steps                          Pause for user confirmation before each batch of worker tasks
  --agent-team                         Directly specify team name (no @ needed in prompt)
  --agent-team-search-path             Comma-separated search paths (default: .agent-teams/,~/.agent-teams/)
  --memory                             Enable long-term memory (RAG vector search)
  --memory-model                       Embedding model (default: qwen3-embedding:4b)
  --archive-memory                     Archive session summary to memory and exit
  --show-history                       Show previous session history on resume
  --dry-run                            Preview skill matching and task delegation without executing agents
  --tui                                Show a Bubble Tea TUI for real-time task tracking
  --rbash                              Use restricted bash (rbash) for the bash tool
  --no-net                             Block all network access for agent subprocesses
  --direnv                             Load .envrc/.env environment for the bash tool
  --think                              Show coordinator decision reasoning
  --plan                               Force plan-first mode: agents must submit plans before executing
  --auto-skills                        Enable automatic skill detection via sidecar/LLM matching
  --report                             Generate a full execution report as a markdown file
  --fix string                         Analyze previous execution data and suggest improvements
  --var key=value                      Set template variable (repeatable)
  --var-file string                    Read template variables from a file (repeatable)
  --skill string                       Force-load specific skills (repeatable)
```

## 5. Tool Specifications

### 5.1 Tool List (Current)

| Tool | File | Description |
|------|------|-------------|
| `bash` | `bash.go` | Execute shell commands in workDir |
| `sudo` | `sudo.go` | Execute commands with root privileges |
| `ssh` | `ssh.go` | Execute commands on remote hosts via SSH |
| `view` | `view.go` | Read file with line numbers, supports offset/limit |
| `write` | `write.go` | Write content to file, returns diff on update |
| `edit` | `edit.go` | Single find/replace in a file |
| `multiedit` | `multiedit.go` | Multiple find/replace in one atomic write |
| `grep` | `grep.go` | Search files using ripgrep or fallback |
| `glob` | `glob.go` | Search files by glob pattern |
| `ls` | `ls.go` | List directory as indented tree |
| `lua` | `lua.go` | Execute Lua scripts in a sandbox |
| `golang` | `golang.go` | Execute Go code via yaegi |
| `ask_user` | `ask_user.go` | Request user input via stdin |
| `download` | `download.go` | Download file from URL to local path |
| `fetch` | `fetch.go` | Fetch URL content (text/markdown/html) |
| `agentic_fetch` | `agentic_fetch.go` | Fetch URL with analysis prompt |
| `random` | `random.go` | Generate random numbers / UUIDs |
| `math` | `math.go` | Mathematical expression evaluation |

### 5.2 Deleted Tools

| Tool | Previous File | Reason |
|------|--------------|--------|
| `read` | `read.go` | Replaced by `view` tool |
| `find` | `find.go` | Replaced by `glob` tool |

### 5.3 Tool Parameters

#### bash

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `command` | string | yes | Shell command to execute |
| `timeout` | number | no | Timeout in seconds (default 120, max 600) |

**Security:** Blocks dangerous commands (curl, wget, sudo, apt, etc.)

#### view (replaces read)

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `file_path` | string | yes | Path to file (relative or absolute) |
| `offset` | number | no | Line number to start from (0-based, default 0) |
| `limit` | number | no | Number of lines to read (default 2000) |

**Output Format:**
```
<file_path>
  1 line content
  2 line content
</file_path>

[showing lines X-Y of Z total. Use offset=N to continue reading]
```

**Constraints:**
- Max file size: 100KB
- Max line length: 2000 chars
- Returns error for directories

#### write

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `file_path` | string | yes | Path to file |
| `content` | string | yes | Content to write |
| `path` | string | no | Deprecated: use file_path |

**Behavior:**
- Creates parent directories if needed
- Returns unified diff if file already exists
- Returns "already contains exact content" if no changes
- Uses `go-udiff` for diff generation

#### edit

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `file_path` | string | yes | Path to file |
| `old_string` | string | yes | Text to replace |
| `new_string` | string | yes | Replacement text |
| `replace_all` | boolean | no | Replace all occurrences (default false) |

**Features:**
- Fuzzy matching for better replacements
- CRLF normalization
- Multiple matches returns error

#### multiedit

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `file_path` | string | yes | Path to file |
| `edits` | array | yes | Array of edit operations |

**Edit Operation Structure:**
| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `old_string` | string | yes | Text to replace |
| `new_string` | string | yes | Replacement text |
| `replace_all` | boolean | no | Replace all occurrences |

**Features:**
- Partial success: applied edits kept, failed edits reported
- Supports file creation (when old_string is empty)
- Returns unified diff of changes

#### grep

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `pattern` | string | yes | Regex pattern to search |
| `path` | string | no | Path to search (default: workDir) |
| `include` | string | no | File pattern filter (preferred) |
| `glob` | string | no | File pattern filter (deprecated, use include) |
| `context` | number | no | Lines of context before/after |
| `ignore_case` | boolean | no | Case-insensitive search |
| `literal` | boolean | no | Treat pattern as literal text |
| `limit` | number | no | Max results (default 100) |

**Features:**
- Uses ripgrep with `-C` for context
- Falls back to grep/egrep if ripgrep unavailable
- `include` takes precedence over `glob`

#### glob

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `pattern` | string | yes | Glob pattern (e.g. `*.ts`, `**/*.json`) |
| `path` | string | no | Directory to search (default: current) |

**Features:**
- Uses ripgrep (rg) if available
- Falls back to Go implementation
- Respects .gitignore
- Limited to 100 results

#### ls

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `path` | string | no | Directory to list (default: current) |
| `ignore` | array | no | Glob patterns to ignore (e.g. `['node_modules', '*.log']`) |
| `depth` | number | no | Maximum depth to traverse (default: unlimited) |

**Output Format:**
```
- directory/
  - subdir/
    - file.txt
    - another.md
```

**Constraints:**
- Max entries: 1000
- Shows tree structure with proper nesting

#### download

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `url` | string | yes | URL to download from |
| `file_path` | string | yes | Local path to save |
| `timeout` | number | no | Timeout in seconds (default 300, max 600) |

**Features:**
- Supports HTTP and HTTPS
- Creates parent directories
- Sets User-Agent: hufu/1.0

#### fetch

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `url` | string | yes | URL to fetch |
| `format` | string | no | Format: 'text', 'markdown', 'html' (default: markdown) |
| `timeout` | number | no | Timeout in seconds (default 30, max 120) |

**Features:**
- Converts HTML to Markdown using html-to-markdown
- Extracts text from HTML
- Truncates to 100KB

#### agentic_fetch

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `url` | string | yes | URL to fetch |
| `prompt` | string | yes | Instruction for analysis |

**Features:**
- Fetches content and appends user prompt for analysis
- Returns markdown-converted HTML content

#### lua

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `code` | string | yes | Lua code to execute |
| `timeout` | number | no | Timeout in seconds (default 120, max 600) |

**Features:**
- Sandbox environment
- Supports string, math, table, coroutine, io standard libraries
- `os` library restricted: only `os.clock`, `os.time`, `os.date`, `os.difftime`
- `io.popen` and `debug` libraries disabled

#### golang

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `code` | string | yes | Go code to execute |
| `timeout` | number | no | Timeout in seconds (default 120, max 600) |

**Features:**
- Uses yaegi interpreter
- Code must include `package` declaration
- Full Go standard library support (requires explicit import)

#### ask_user

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `question` | string | yes | Question to ask |
| `type` | string | yes | Type: `single_choice`, `multiple_choice`, `free_text`, `mixed` |
| `options` | array | no | Options for choice types |

**Features:**
- Uses `tools.StdinMu` to serialize stdin reads
- Supports parallel option

#### random

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `type` | string | yes | `uuid`, `int`, `float` |
| `min` | number | no | Minimum value (int/float) |
| `max` | number | no | Maximum value (int/float) |

#### math

| Parameter | Type | Required | Description |
|-----------|------|----------|-------------|
| `expression` | string | yes | Mathematical expression to evaluate |

#### Coordinator-only Tools

| Tool | Description |
|------|-------------|
| `load_skill` | Load skill content by name |
| `finish` | Signal completion with final answer |
| `agent` | Delegate tasks to workers |

## 6. Team Configuration Format

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
worker-context-size: 4000      # Max tokens for worker context window

# === Workspace ===
workspace: workspace             # Workspace directory (default: "workspace")

# === Model Settings ===
model: ollama/qwen3:8b           # Default model name
temperature: "0.7"             # Temperature value
max-tokens: "4096"               # Maximum output tokens
top-p: "0.9"                     # Top P value
top-k: "40"                      # Top K value
provider-url: http://localhost:11434/v1
provider-api-key: ""             # API key override

# === Provider Pool (multi-provider support) ===
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
sidecar-model: qwen3:1b         # Lightweight model for skill matching
guard-model: qwen3:8b           # Model for guard/review tasks

# === Skills ===
skills: code-review,git-commit    # Skills to include
skills-exclude: debug             # Skills to exclude

# === MCP Servers ===
mcp-servers:
  filesystem:
    type: local
    command: ["npx", "-y", "@modelcontextprotocol/server-filesystem", "/path"]
  remote-api:
    type: remote
    url: "https://mcp-server.example.com/api"
    allowedTools: ["search", "query"]

# === Security ===
allowed-paths: ["/home/user/projects", "/tmp"]
restricted-path: "/etc"
no-net: false                     # Block network access

# === Template Variables ===
vars:
  project_name: "hufu"
  author: "anomalyco"

# === Notifications ===
notify:
  type: webhook
  url: "https://hooks.example.com/agent"
```

## 7. Agent .md Frontmatter

```markdown
---
name: developer
description: Implementation specialist
role: worker               # worker or coordinator
tools: view,write,edit,multiedit,bash,grep,glob,ls
skills: code-review        # Comma-separated or YAML list
guard:                     # Guard rules (YAML list)
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
provider-api-key: ""       # API key override
allowed-paths: ["src/", "tests/"]
restricted-path: "/etc"
no-net: false
```

| Field | Required | Default | Description |
|-------|----------|---------|-------------|
| `name` | yes | — | Agent name, used for `@<name>` invocation |
| `description` | no | — | Agent description |
| `role` | no | `worker` | Role (`worker` or `coordinator`) |
| `tools` | no | — | Available tools list (string or YAML list) |
| `skills` | no | — | Skills to load (string or YAML list) |
| `guard` | no | — | Guard rules (YAML list) |
| `model` | no | Team default | LLM model to use |
| `temperature` | no | Team default | Temperature value |
| `max-tokens` | no | Team default | Maximum output tokens |
| `top-p` | no | Team default | Top P value |
| `top-k` | no | Team default | Top K value |
| `timeout` | no | Team default | Timeout in seconds |
| `max-retries` | no | `-1` (use team default) | Maximum retries |
| `max-steps` | no | Team default | Maximum execution steps |
| `provider-url` | no | Team default | Provider URL override |
| `provider-api-key` | no | Team default | API key override |
| `allowed-paths` | no | Team default | Allowed file system paths |
| `restricted-path` | no | Team default | Restricted file system path |
| `no-net` | no | Team default | Block network access |

## 8. Key Types

### AgentDef (agent.go)

```go
type AgentDef struct {
    Name           string
    FileAlias      string
    Description    string
    Tools          string   // Comma-separated tool names
    Role           string
    System         string   // Full system prompt
    Capabilities   string
    Skills         string   // Comma-separated skill names
    Guard          []string
    Timeout        int64
    MaxRetries     int
    MaxSteps       int
    AllowedPaths   []string
    RestrictedPath string
    NoNet          bool
    ProviderURL    string
    ProviderAPIKey string
    Generation     GenerationParams
}

// Alias: AgentGenParams in 2024, renamed to GenerationParams
type GenerationParams struct {
    Model       string
    Temperature string
    MaxTokens   string
    TopP        string
    TopK        string
}
```

### TeamConfig (parse.go → agent.go)

```go
type TeamConfig struct {
    Name          string
    Description   string
    MaxRounds     int
    MaxSteps      int
    WorkspaceDir  string
    Timeout       int64
    MaxRetries    int
    Generation    GenerationParams
    Skills        string      // Comma-separated skill names
    SkillsExclude string
    ProviderURL    string
    ProviderAPIKey string
    Providers     map[string]config.ProviderConfig  // Multi-provider pool
    ModelList     []config.ModelEntry               // Custom model list
    SidecarModel  string           // Lightweight model for skill matching
    GuardModel    string           // Model for guard/review tasks
    MaxConcurrent int              // Max concurrent worker tasks
    Notify        notify.NotifyConfig
    AllowedPaths   []string
    RestrictedPath string
    NoNet            bool
    Vars             map[string]interface{}
    WorkerContextSize int        // Max worker context tokens
    ToolsAllowed   []string       // Explicit tool allowlist
}
```

### ProviderManager (agent.go)

```go
type OllamaProvider struct {
    provider fantasy.Provider  // charm.land/fantasy provider
    baseURL  string
    apiKey   string
    name     string            // Provider name (e.g. "ollama", "openai")
}

// ProviderManager manages multiple providers (added 2024 Q4)
type ProviderManager struct {
    providers []ProviderEntry  // Registered provider pool
    fallback  ProviderEntry    // Default fallback provider
}
```

## 9. Skill Usage Tracking

### Overview

Tracks which skills are loaded/used during a session, including which agents accessed them.

### Components

#### StatusEvent Extension

| Field | Type | Description |
|-------|------|-------------|
| `Type` | string | New value: `"skill_used"` |
| `SkillName` | string | Name of the skill being used |

#### Coordinator (`internal/team/coordinator.go`)

| Field/Method | Type | Description |
|--------------|------|-------------|
| `skillUsage` | `map[string]*SkillUsageEntry` | Tracks skill usage per session |
| `SkillUsageEntry` | struct | `{Name, Count, Agents map[string]bool}` |
| `recordSkillUsage(name, agent string)` | method | Records skill usage and emits event |
| `SkillUsage()` | method | Returns copy of all skill usage entries |
| `extractSkillFromToolCall(toolName, input string)` | method | Detects skill file access via view/read tools |

**Detection Logic:**
- Monitors `view` and `read` tool calls
- Extracts file path from JSON arguments (`file_path` or `path` field)
- Identifies skill files: paths containing `shared/skills/` with `.md` extension
- Matches against known skills by name (case-insensitive)
- Also records when coordinator uses `load_skill` tool

#### CLI Display (`cmd/hufu/display.go`)

| Struct | Description |
|--------|-------------|
| `skillDisplay` | Displays skills panel in CLI |
| `skillEntry` | `{name, count, agents []string}` |

**Display Format:**
```
─── SKILLS ───
  ✓ code-reviewer     ×2  researcher, writer
  ✓ git-commit        ×1  developer
```

### Integration Points

1. **setupStatusReporter**: Handles `"skill_used"` events via `skillDisp.record()`
2. **setStatusFlusher**: Passes `skillDisp` for dirty-refresh on idle
3. **executeSegments**: Creates `skillDisplay` for all three execution paths

## 10. Sidecar System

### Overview

The sidecar is a lightweight LLM agent used for auxiliary tasks that should not consume the main model's context window.

### Use Cases

1. **Skill Matching**: Match user prompts to relevant skills without main model calls
2. **Guard Review**: Review agent outputs against guard rules
3. **Plan Review**: Autonomous plan review for multi-step tasks

### Configuration

```yaml
# In team.yaml:
sidecar-model: qwen3:1b      # Lightweight model for skill matching
guard-model: qwen3:8b        # Model for guard/review tasks
```

### API

```go
type Sidecar struct {
    provider fantasy.Provider
    model    string
}

func NewSidecar(ctx context.Context, provider fantasy.Provider, model string) (*Sidecar, error)
func (s *Sidecar) MatchSkills(ctx context.Context, prompt string, skills []SkillSummary) ([]string, error)
func (s *Sidecar) GuardReview(ctx context.Context, output string, rules []string) (bool, []string, error)
```

## 11. TUI (Bubble Tea)

### Overview

Optional Bubble Tea TUI for real-time task tracking:

```bash
hufu --tui "Refactor the auth module"
```

### Architecture

```
main.go runTUIMode()
    │
    ▼
tuipkg.New("prompt", TeamInfo) ──► tea.NewProgram(Model)
    │
    ├─► Model.Update(msg) ── Pure state transition (Model, Cmd)
    ├─► Model.View() ── Render current state to string
    └─► Status reporter goroutine ── sends tea.Msg via p.Send()
```

### Model State Fields (`internal/tui/tui.go`)

**CRITICAL**: All fields are read by `View()` and modified by `Update()`. Adding new bool overlay flags requires updating the `View()` priority order.

| Field | Type | Purpose |
|-------|------|---------|
| `prompt` | `string` | User's original prompt displayed in top widget |
| `tasks` | `[]*team.TodoItem` | All tasks from coordinator TODO list |
| `logs` | `map[string][]string` | todoID → rendered log lines (task output buffer) |
| `coordItem` | `*team.TodoItem` | Coordinator pseudo-task item |
| `col` | `int` | Focused column index (0=pending, 1=planned, 2=in_progress, 3=done, 4=skipped, 5=error) |
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
| `finished` | `bool` | Set when FinishedMsg received |
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

### All tea.Msg Types

#### Public Messages (sent via p.Send from goroutines)

| Message | Sender | Purpose |
|---------|--------|---------|
| `TasksUpdatedMsg{Items}` | Coordinator → TUI | Update task column data |
| `TaskLogMsg{TodoID, Line}` | Status reporter → TUI | Append log line to task detail |
| `CoordItemMsg{Item}` | Status reporter → TUI | Create/update coordinator pseudo-task |
| `CoordStatusMsg{Status}` | Status reporter → TUI | Update coordinator task status |
| `FinishedMsg{}` | Coordinator → TUI | All work complete |
| `StatusBarMsg{Text}` | Status reporter → TUI | Update 1-line status bar |
| `ResultMsg{Text}` | Coordinator → TUI | Display final result |
| `TeamInfoMsg{Info}` | main.go → TUI | Load team metadata |
| `WrapUpMsg{}` | Ctrl+C handler → TUI | Wrap-up request |
| `AskUserCancelMsg{}` | Cleanup → TUI | Cancel ask_user dialog |

#### Internal Messages

| Message | Purpose |
|---------|---------|
| `AskUserMsg{Question, Type, Options, AllowAny, ReplyCh}` | Trigger ask_user modal |
| `detailRefreshMsg{}` | Debounced viewport re-render |
| `copySuccessMsg{Lines}` | OSC52 clipboard copy confirmation |

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

### StatusEvent Types Handled by TUI Reporter

`makeTUIReporter` in `cmd/hufu/display.go` translates `StatusEvent` → `tea.Msg`:

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

### Wrap-Up Mechanism

```
First Ctrl+C in TUI
    │
    ▼
handleCtrlC() → wrapUpRequested = true
                → send to WrapUpCh
    │
    ▼
runWithTUI goroutine → c.SetWrapUp() on coordinator
    │
    ▼
Coordinator → StatusEvent{Type:"wrap_up_phase"}
    │
    ▼
TUI reporter → TasksUpdatedMsg + status bar text
    │
    ▼
FinishedMsg + wrapUpRequested + !inAskUser → tea.Quit
```

**Second Ctrl+C** → context cancel → immediate exit.

### Thinking Tracker

- `thinkingTickInterval = 5s`
- Per-todoID background goroutine
- Started on `start`, stopped on `step`/`tool_call`/`tool_result`/`done`/`error`/`cache_hit`
- Sends `StatusBarMsg` with elapsed LLM wait time

### Detail Refresh Debounce

- `TaskLogMsg` for current `detailID` schedules `detailRefreshMsg` after **80ms**
- `detailRefreshScheduled` flag prevents duplicate schedules
- Prevents viewport thrashing during rapid log updates

### Activity Log

- Circular buffer: `recentLogs` max **500** entries
- Rendered between status bar and columns (max **6** lines)
- Full-screen overlay via `a` key

### Clipboard (VISUAL Mode)

- Uses **OSC52** escape sequence: `\x1b]52;c;base64\x1b\\`
- Copies selected text to system clipboard
- Works in most modern terminals

### Mouse Handling

- Left-click on item → open detail view (enables mouse)
- Wheel → scroll cursor position
- Entering detail enables mouse
- Exiting detail disables mouse UNLESS `mouseManuallyEnabled`
- Toggle with `m` key

### Edge Cases & Safety

1. **Prompt widget height**: Dynamic word-wrap to max 5 lines; affects `colBodyHeight()`
2. **`vpHeight()`**: Accounts for variable header lines (injected/loaded skills add extra)
3. **`scrollCursorIntoView()`**: Computes per-item rendered line heights; keeps cursor with ~1/3 page context above
4. **`wrapLine()`**: ANSI-aware text wrapping preserving escape sequences
5. **FinishedMsg transitions**: Pending→Skipped, InProgress→Done, Paused→Done (safety net)
6. **Column 2**: Includes BOTH `TaskInProgress` and `TaskPaused`
7. **Subtasks in detail**: Items with `ParentID == detailID` rendered under "─── Subtasks ───"
8. **Result box**: Max 8 lines; remaining indicated with `... (N more lines)`
9. **Status bar**: `ansi.Strip()` ensures exactly 1 line
10. **Window resize**: `clampScroll()` validates all column scroll offsets
11. **Line writer buffering**: Non-TUI mode buffers writes during ask_user; flushes on done
12. **CoordTodoID**: Special constant `__coord__`; rendered with `(coordinator)` label

### Style Definitions

| Style | Definition |
|-------|------------|
| `promptStyle` | Bold, foreground 13 (magenta) |
| `promptBoxStyle` | Rounded border, border fg 13, padding 0,1 |
| `headerStyle` | Bold, foreground 14 (cyan) |
| `agentStyle` | Bold, foreground 12 (blue) |
| `skillStyle` | Faint, foreground 6 (teal) |
| `selectedFg` | Bold, foreground 15 (white) |
| `selectedBg` | Background 237 (dark grey) |
| `pendingIcon` | Foreground 8 (grey) |
| `progressIcon` | Foreground 11 (yellow) |
| `pausedIcon` | Foreground 6 (teal) |
| `doneIcon` | Foreground 2 (green) |
| `errorIcon` | Foreground 9 (red) |
| `wrapUpStyle` | Bold, foreground 11 |
| `visualStyle` | Background 236, foreground 15 |
| `confirmBoxStyle` | Rounded border, border fg 9 (red), padding 1,3 |
| `resultBoxStyle` | Rounded border, border fg 2 (green), padding 0,1 |
| `teamStyle` | Bold, foreground 13 |

### Testing

All TUI logic is tested via state machine testing (`Update()` as pure function):

```go
func TestUpdate_Navigation(t *testing.T) {
    m := New("test prompt", TeamInfo{TeamName: "test-team"})
    m.width = 100; m.height = 40
    m.tasks = []*team.TodoItem{{ID: "1", Status: team.TaskPending, Desc: "Task 1"}}
    m2, _ := m.Update(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune("j")})
    if m2.(Model).row != 1 { t.Errorf("Expected row 1, got %d", m2.(Model).row) }
}
```

Key test files:
- `internal/tui/tui_test.go` — Core TUI model tests
- `internal/tui/selection_test.go` — Selection behavior
- `internal/tui/scroll_test.go` — Scrolling behavior
- `internal/tui/speckit_navigation_test.go` — Navigation

### Agent Safety Rules

1. **Never change `View()` priority order** without updating all bool checks consistently
2. **Always handle new `tea.Msg` types** in both `Update()` and any reporter translation
3. **Preserve `Update()` purity** — no I/O, no global state mutations
4. **Test new key bindings** with `tea.KeyMsg` in tests before claiming completion
5. **ANSI-aware wrapping** — use existing `wrapLine()` for any new text rendering
6. **Viewport lifecycle** — always check `vpReady` before `vp.SetContent()`
7. **Mouse state consistency** — `mouseEnabled` and `mouseManuallyEnabled` must stay in sync
8. **Detail debounce** — any new log-sending code must respect `detailRefreshScheduled`

## 12. Report Generation

### Overview

```bash
hufu --report "Refactor the module"
```

Generates a comprehensive markdown report with:
- Task delegation summary
- Agent execution logs
- Tool usage statistics
- Skill usage tracking
- Performance metrics (wall clock, token usage)

### Report Format

```markdown
# Execution Report: {task}

## Summary
- Team: {team-name}
- Agents Used: {count}
- Rounds: {count}
- Wall Clock: {duration}

## Task Breakdown
| Agent | Task | Status | Duration |
|-------|------|--------|----------|
| ... | ... | ... | ... |

## Tool Usage
| Tool | Count |
|------|-------|
| bash | 42 |
| write | 12 |

## Skill Usage
| Skill | Count |
|-------|-------|
| code-review | 3 |
```

## 13. Dry Run Mode

```bash
hufu --dry-run --agent-team my-team "Refactor the module"
```

Preview-only mode that:
- Loads skills and agents
- Matches skills against prompt
- Shows which agents would be used
- Displays planned task delegation
- Does NOT execute any LLM calls

## 14. Plan-First Mode

```bash
hufu --plan "Implement a feature"
```

Agents must submit a plan before executing. The coordinator reviews the plan before allowing task execution.

## 15. Session Management

### Wrap-up & Interrupt

- **Ctrl+C**: First press = graceful wrap-up (finish current work, call `finish`); Second press = force exit (exit 130)
- **Ctrl+Z**: Inject additional prompt via readline while running
- **SIGUSR1**: Inject additional prompt (alternative to Ctrl+Z)

### Session Files

```
workspace/
├── session.json              # Structured session data
├── session.md                # Human-readable session log
├── session_history.json      # Raw message history
├── stm.md                    # Short-term memory
├── tasks/
│   └── {team-name}/
│       └── {agent-name}/
│           └── {timestamp}.md
├── shared/
│   └── skills/               # Copied SKILL.md files
├── status/                   # Agent status files
├── history/                  # Archived session files
└── logs/                     # All system/debug logs
    ├── audit/                # Tool audit log
    ├── llm/                  # LLM request/response logs
    └── stm/                  # STM round checkpoints
```

### Memory Architecture

| Component | Description |
|-----------|-------------|
| **Vector Store** | `chromem-go` (in-process, no external DB) |
| **Embedding** | Ollama embeddings |
| **Storage** | `~/.local/share/hufu/memory/<projectHash>/` |
| **Default Model** | `qwen3-embedding:4b` |

## 16. Configuration File (hufu.yaml)

```yaml
provider-url: http://localhost:11434/v1
provider-api-key: ""                    # API key
embedding-model: qwen3-embedding:4b
max-concurrent: 8
vars:
  project_name: "default"
```

**Priority:** CLI flag > `hufu.yaml` > Defaults

## 17. Defaults Reference

### General Settings

| Setting | Default | Source |
|---------|---------|--------|
| Provider URL | `http://localhost:11434/v1` | `config.go` |
| Embedding Model | `qwen3-embedding:4b` | `config.go` |
| Max Concurrent | `8` | `main.go` |

### Agent Settings

| Setting | Default | Source |
|---------|---------|--------|
| Max Steps (workers) | `30` | `agent.go` |
| Max Steps (coordinators) | `20` | `agent.go` |
| Agent Default Role | `worker` | `parse.go` |

### Team Settings

| Setting | Default | Source |
|---------|---------|--------|
| Team Max Rounds | `10` | `parse.go` |
| Team Timeout | `600s` | `parse.go` |
| Team Max Retries | `2` | `parse.go` |

### Tool Timeouts & Limits

| Setting | Default | Source |
|---------|---------|--------|
| Bash Timeout | `120s` | `bash.go` |
| Max Bash Timeout | `600s` | `bash.go` |
| SSH Timeout | `30s` | `ssh.go` |
| Lua Timeout | `120s` | `lua.go` |
| Golang Timeout | `120s` | `golang.go` |
| Download Timeout | `300s` | `download.go` |
| Fetch Timeout | `30s` | `fetch.go` |
| MCP Timeout | `30s` | `manager.go` |

### Tool Output Limits

| Setting | Default | Source |
|---------|---------|--------|
| View Limit | `2000` lines | `view.go` |
| Grep Limit | `100` matches | `grep.go` |
| Glob Limit | `100` results | `glob.go` |
| LS Limit | `1000` items | `ls.go` |

---

## Changelog

### 2024 Q3 → Q4

- **Multi-provider support**: `providers` field in team.yaml, `ProviderManager` type
- **Sidecar system**: Skill matching, guard review, plan review auxiliary agents
- **TUI mode**: `--tui` flag with Bubble Tea real-time task tracking
- **Dry run mode**: `--dry-run` flag for preview-only execution
- **Plan-first mode**: `--plan` flag requiring agents to submit plans
- **Auto-skills**: `--auto-skills` flag for sidecar-driven skill detection
- **Report generation**: `--report` flag for markdown execution reports
- **Template variables**: `--var`, `--var-file` flags, `vars` field in team.yaml
- **New tools**: `random`, `math`
- **rbash / no-net / direnv**: `bash` tool security enhancements
- **Guard system**: `guard` field on agents for rule-based output review
- **Worker context size**: `worker-context-size` field for per-agent context limits
- **MCP integration**: `mcp-servers` field in team.yaml
- **ChromaDB replaced with chromem-go**: Removed external ChromaDB dependency
