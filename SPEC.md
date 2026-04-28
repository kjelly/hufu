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
| `github.com/charmbracelet/lipgloss` | v0.12.0 | Terminal styling |
| `github.com/ergochat/readline` | v0.1.3 | Interactive prompt input |
| `github.com/mark3labs/mcp-go` | latest | MCP client |
| `github.com/spf13/cobra` | v1.10.2 | CLI framework |
| `github.com/yuin/gopher-lua` | v1.1.2 | Lua scripting support |
| `github.com/aymanbagabas/go-osc52/v2` | v2.0.1 | Clipboard integration |

### Indirect Dependencies (New)

| Package | Version | Purpose |
|---------|---------|---------|
| `github.com/JohannesKaufmann/html-to-markdown` | v1.6.0 | HTML to Markdown conversion |
| `github.com/PuerkitoBio/goquery` | v1.9.2 | HTML parsing |
| `github.com/andybalholm/cascadia` | v1.3.2 | CSS selectors for goquery |
| `golang.org/x/net` | v0.52.0 | HTTP networking |

## 4. CLI Usage

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

## 5. Tool Specifications

### 5.1 Tool List (Current)

| Tool | File | Description |
|------|------|-------------|
| `bash` | `bash.go` | Execute shell commands in workDir |
| `view` | `view.go` | Read file with line numbers, supports offset/limit |
| `write` | `write.go` | Write content to file, returns diff on update |
| `edit` | `edit.go` | Single find/replace in a file |
| `multiedit` | `multiedit.go` | Multiple find/replace in one atomic write |
| `grep` | `grep.go` | Search files using ripgrep or fallback |
| `glob` | `glob.go` | Search files by glob pattern |
| `ls` | `ls.go` | List directory as indented tree |
| `lua` | `lua.go` | Execute Lua scripts |
| `golang` | `golang.go` | Execute Go commands |
| `ask_user` | `ask_user.go` | Request user input via stdin |
| `download` | `download.go` | Download file from URL to local path |
| `fetch` | `fetch.go` | Fetch URL content (text/markdown/html) |
| `agentic_fetch` | `agentic_fetch.go` | Fetch URL with analysis prompt |

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

#### Coordinator-only Tools

| Tool | Description |
|------|-------------|
| `load_skill` | Load skill content by name |
| `finish` | Signal completion with final answer |
| `agent` | Delegate tasks to workers |

## 6. Untracked Files (New)

The following new tool files were added but not yet tracked in git:

| File | Status | Description |
|------|--------|-------------|
| `internal/tools/view.go` | Untracked | File viewer with line numbers |
| `internal/tools/multiedit.go` | Untracked | Multi-edit tool |
| `internal/tools/glob.go` | Untracked | Glob pattern file search |
| `internal/tools/download.go` | Untracked | URL download tool |
| `internal/tools/fetch.go` | Untracked | URL content fetcher |
| `internal/tools/agentic_fetch.go` | Untracked | Fetch with analysis prompt |

## 7. Deleted Files

| File | Status | Description |
|------|--------|-------------|
| `internal/tools/read.go` | Deleted | Replaced by view.go |
| `internal/tools/find.go` | Deleted | Replaced by glob.go |

## 8. .gitignore Changes

Added `.claude` to ignore Claude-specific files.

```
+.claude
```

## 9. Tool Selection Changes

In `internal/agent/agent.go`, `SelectTools()` no longer maps `glob` to `find`:

```go
// Before: if n == "glob" { n = "find" }
// After: Direct mapping, glob uses NewGlobTool directly
```
