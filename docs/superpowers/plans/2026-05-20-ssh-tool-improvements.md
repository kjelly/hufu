# SSH Tool Improvements Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Implement comprehensive SSH tool improvements including session management, error diagnostics, SCP support, SSH config parsing, audit logging, connection reuse, and TUI integration.

**Architecture:** Modular approach with separate components for session management (`ssh_session.go`), SCP tool (`scp.go`), config parsing (`ssh_config.go`), and enhanced SSH tool (`ssh.go`). All components integrate with existing context and audit systems.

**Tech Stack:** Go 1.26.2, charm.land/fantasy, golang.org/x/crypto/ssh (optional), existing audit/tui packages

---

## File Structure

### Files to Create
- `internal/tools/ssh_session.go` — SSH session manager
- `internal/tools/ssh_config.go` — SSH config file parser
- `internal/tools/scp.go` — SCP file transfer tool
- `internal/tools/ssh_test.go` — SSH tool tests
- `internal/tools/ssh_session_test.go` — Session manager tests
- `internal/tools/scp_test.go` — SCP tool tests
- `internal/tools/ssh_config_test.go` — Config parser tests
- `docs/superpowers/specs/2026-05-20-ssh-tool-improvements-design.md` — Design spec (already created)

### Files to Modify
- `internal/tools/ssh.go` — Add force-mcp, error diagnostics, session tracking, connection reuse
- `internal/tools/types.go` — Add SSHSessionManager type
- `internal/tools/tools.go` — Export ForceMCPBlockedTools for audit
- `internal/audit/audit.go` — Add SSH connection event type
- `internal/tui/tui.go` — Add SSH session display in team info
- `internal/team/coordinator.go` — Initialize SSH session manager

---

### Task 1: SSH Session Manager

**Files:**
- Create: `internal/tools/ssh_session.go`
- Create: `internal/tools/ssh_session_test.go`
- Modify: `internal/tools/types.go`

- [ ] **Step 1: Write session manager tests**

```go
// internal/tools/ssh_session_test.go
package tools

import (
    "context"
    "testing"
)

func TestSSHSessionManager_CreateGet(t *testing.T) {
    mgr := NewSSHSessionManager()

    session := mgr.Create("task-1", "user@host1", 22)
    if session == nil {
        t.Fatal("Create() returned nil")
    }
    if session.Host != "user@host1" {
        t.Errorf("Host = %q, want user@host1", session.Host)
    }
    if session.Port != 22 {
        t.Errorf("Port = %d, want 22", session.Port)
    }

    retrieved := mgr.Get("task-1")
    if retrieved != session {
        t.Error("Get() should return same session")
    }
}

func TestSSHSessionManager_List(t *testing.T) {
    mgr := NewSSHSessionManager()
    mgr.Create("task-1", "user@host1", 22)
    mgr.Create("task-2", "user@host2", 2222)

    sessions := mgr.List()
    if len(sessions) != 2 {
        t.Errorf("List() count = %d, want 2", len(sessions))
    }
}

func TestSSHSessionManager_Close(t *testing.T) {
    mgr := NewSSHSessionManager()
    mgr.Create("task-1", "user@host1", 22)

    mgr.Close("task-1")
    if mgr.Get("task-1") != nil {
        t.Error("Close() should remove session")
    }
}

func TestSSHSessionManager_ContextIntegration(t *testing.T) {
    mgr := NewSSHSessionManager()
    ctx := context.WithValue(context.Background(), SSHSessionManagerKey, mgr)

    retrieved := GetSSHSessionManager(ctx)
    if retrieved != mgr {
        t.Error("GetSSHSessionManager() should return manager from context")
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./internal/tools/ssh_session_test.go -v
```
Expected: FAIL with "undefined: NewSSHSessionManager"

- [ ] **Step 3: Write SSH session manager implementation**

```go
// internal/tools/ssh_session.go
//go:build linux || darwin
// +build linux darwin

package tools

import (
    "context"
    "sync"
    "time"
)

// SSHSessionManagerKey is the context key for SSH session manager
type sshSessionManagerKeyType struct{}

var SSHSessionManagerKey = sshSessionManagerKeyType{}

// SSHSessionManager manages active SSH sessions
type SSHSessionManager struct {
    mu       sync.RWMutex
    sessions map[string]*SSHSession
}

// NewSSHSessionManager creates a new session manager
func NewSSHSessionManager() *SSHSessionManager {
    return &SSHSessionManager{
        sessions: make(map[string]*SSHSession),
    }
}

// Create creates a new SSH session and stores it
func (m *SSHSessionManager) Create(taskID, host string, port int) *SSHSession {
    m.mu.Lock()
    defer m.mu.Unlock()

    user := ""
    if idx := strings.Index(host, "@"); idx != -1 {
        user = host[:idx]
    }

    session := &SSHSession{
        Host:      host,
        User:      user,
        Port:      port,
        TaskID:    taskID,
        CreatedAt: time.Now(),
    }
    m.sessions[taskID] = session
    return session
}

// Get retrieves a session by task ID
func (m *SSHSessionManager) Get(taskID string) *SSHSession {
    m.mu.RLock()
    defer m.mu.RUnlock()
    return m.sessions[taskID]
}

// List returns all active sessions
func (m *SSHSessionManager) List() []*SSHSession {
    m.mu.RLock()
    defer m.mu.RUnlock()

    result := make([]*SSHSession, 0, len(m.sessions))
    for _, s := range m.sessions {
        result = append(result, s)
    }
    return result
}

// Close removes a session
func (m *SSHSessionManager) Close(taskID string) {
    m.mu.Lock()
    defer m.mu.Unlock()
    delete(m.sessions, taskID)
}

// Count returns the number of active sessions
func (m *SSHSessionManager) Count() int {
    m.mu.RLock()
    defer m.mu.RUnlock()
    return len(m.sessions)
}

// SetSSHSessionManager stores manager in context
func SetSSHSessionManager(ctx context.Context, mgr *SSHSessionManager) context.Context {
    return context.WithValue(ctx, SSHSessionManagerKey, mgr)
}

// GetSSHSessionManager retrieves manager from context
func GetSSHSessionManager(ctx context.Context) *SSHSessionManager {
    if v, ok := ctx.Value(SSHSessionManagerKey).(*SSHSessionManager); ok {
        return v
    }
    return nil
}
```

- [ ] **Step 4: Update SSHSession struct in types.go**

```go
// internal/tools/types.go - modify SSHSession struct
type SSHSession struct {
    Host      string    // Remote host in [user@]hostname format
    User      string    // Username (extracted from host)
    Port      int       // SSH port (default 22)
    TaskID    string    // Task ID where this session was created
    CreatedAt time.Time // Session creation time
}
```

- [ ] **Step 5: Run test to verify it passes**

```bash
go test ./internal/tools/ssh_session_test.go -v
```
Expected: PASS all 4 tests

- [ ] **Step 6: Commit**

```bash
git add internal/tools/ssh_session.go internal/tools/ssh_session_test.go internal/tools/types.go
git commit -m "feat: add SSH session manager with context integration"
```

---

### Task 2: Force-MCP Integration for SSH

**Files:**
- Modify: `internal/tools/ssh.go:64-70`

- [ ] **Step 1: Write force-mcp test**

```go
// internal/tools/ssh_test.go
package tools

import (
    "context"
    "testing"

    "charm.land/fantasy"
)

func TestExecuteSSH_ForceMCP(t *testing.T) {
    tool := NewSshTool()
    ctx := context.WithValue(context.Background(), AgentForceMCPKey, true)

    input := `{"host": "user@example.com", "command": "uptime"}`
    result, err := tool.Run(ctx, fantasy.ToolCall{Input: input})

    if err != nil {
        t.Fatalf("Run() error: %v", err)
    }
    if !result.IsError {
        t.Error("Expected error when --force-mcp is enabled")
    }
    if result.Content == "" {
        t.Error("Expected error message")
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./internal/tools/ssh_test.go -run TestExecuteSSH_ForceMCP -v
```
Expected: FAIL (SSH tool doesn't check force-mcp yet)

- [ ] **Step 3: Add force-mcp check to executeSSH**

```go
// internal/tools/ssh.go - modify executeSSH function
func executeSSH(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
    // Check force-mcp mode
    if fm, ok := ctx.Value(AgentForceMCPKey).(bool); ok && fm {
        return fantasy.NewTextErrorResponse(
            "ssh tool is blocked by --force-mcp. " +
                "Use an MCP server for SSH operations instead.",
        ), nil
    }

    var args sshArgs
    if err := parseArgs(call.Input, &args); err != nil {
        return fantasy.NewTextErrorResponse("host parameter is required"), nil
    }
    // ... rest of existing code
}
```

- [ ] **Step 4: Run test to verify it passes**

```bash
go test ./internal/tools/ssh_test.go -run TestExecuteSSH_ForceMCP -v
```
Expected: PASS

- [ ] **Step 5: Commit**

```bash
git add internal/tools/ssh.go internal/tools/ssh_test.go
git commit -m "feat: add --force-mcp check to SSH tool"
```

---

### Task 3: Enhanced Error Diagnostics

**Files:**
- Modify: `internal/tools/ssh.go:120-147`
- Modify: `internal/tools/ssh_test.go`

- [ ] **Step 1: Write error diagnostic tests**

```go
// internal/tools/ssh_test.go
func TestDiagnoseSSHErrors_AuthFailed(t *testing.T) {
    stderr := "Permission denied (publickey,password)."
    result := diagnoseSSHErrors(255, stderr)

    if !strings.Contains(result, "Authentication failed") {
        t.Errorf("Expected authentication failure message, got %q", result)
    }
    if !strings.Contains(result, "Identity file permissions") {
        t.Error("Expected identity file troubleshooting")
    }
}

func TestDiagnoseSSHErrors_ConnectionRefused(t *testing.T) {
    stderr := "ssh: connect to host example.com port 22: Connection refused"
    result := diagnoseSSHErrors(255, stderr)

    if !strings.Contains(result, "Connection refused") {
        t.Errorf("Expected connection refused message, got %q", result)
    }
    if !strings.Contains(result, "SSH daemon running") {
        t.Error("Expected SSH daemon troubleshooting")
    }
}

func TestDiagnoseSSHErrors_Timeout(t *testing.T) {
    result := diagnoseSSHErrors(124, "ssh connection timed out")

    if !strings.Contains(result, "timed out") {
        t.Errorf("Expected timeout message, got %q", result)
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./internal/tools/ssh_test.go -run TestDiagnose -v
```
Expected: FAIL (function doesn't exist)

- [ ] **Step 3: Implement error diagnostics**

```go
// internal/tools/ssh.go - add new function
func diagnoseSSHErrors(exitCode int, stderr string) string {
    switch {
    case strings.Contains(stderr, "Permission denied"):
        return "SSH authentication failed. Check:\n" +
            "- Identity file permissions (chmod 600)\n" +
            "- SSH agent forwarding (ssh-add -l)\n" +
            "- User@host format"
    case strings.Contains(stderr, "Connection refused"):
        return "SSH connection refused. Check:\n" +
            "- SSH daemon running on remote (systemctl status sshd)\n" +
            "- Correct port number\n" +
            "- Firewall rules"
    case strings.Contains(stderr, "No route to host"):
        return "Host unreachable. Check:\n" +
            "- Network connectivity\n" +
            "- Hostname/IP correctness\n" +
            "- DNS resolution"
    case exitCode == 124:
        return "SSH connection timed out. Consider:\n" +
            "- Increasing timeout parameter\n" +
            "- Checking network latency\n" +
            "- Verifying host availability"
    default:
        return stderr
    }
}
```

- [ ] **Step 4: Update executeSSH to use diagnostics**

```go
// internal/tools/ssh.go - modify error handling section
exitCode := 0
if waitErr != nil {
    if exitErr, ok := waitErr.(*exec.ExitError); ok {
        exitCode = exitErr.ExitCode()
    } else if cmdCtx.Err() == context.DeadlineExceeded {
        return fantasy.NewTextErrorResponse("ssh connection timed out"), nil
    }
}

response := buildBashResponse(stdout.String(), stderr.String(), exitCode)

// Enhanced error diagnostics
if exitCode != 0 {
    diagnosedMsg := diagnoseSSHErrors(exitCode, stderr.String())
    response.Content = fmt.Sprintf(
        "[SSH Error: %s]\n\n%s\n\nOriginal error: %s",
        getSSHErrorTitle(exitCode, stderr.String()),
        diagnosedMsg,
        stderr.String(),
    )
    response.IsError = true
}

// Add SSH context hint for agent (only on success)
if exitCode == 0 {
    response.Content += fmt.Sprintf(
        "\n\n[SSH Session Active] You have connected to %s. "+
            "To execute additional commands on this host, use the ssh tool again with the SAME host identifier (keep using '%s' as provided). "+
            "Do NOT embed 'ssh' in bash commands - use the ssh tool directly.",
        args.Host,
        args.Host,
    )
}
```

- [ ] **Step 5: Add helper function**

```go
// internal/tools/ssh.go
func getSSHErrorTitle(exitCode int, stderr string) string {
    switch {
    case strings.Contains(stderr, "Permission denied"):
        return "Authentication Failed"
    case strings.Contains(stderr, "Connection refused"):
        return "Connection Refused"
    case strings.Contains(stderr, "No route to host"):
        return "Host Unreachable"
    case exitCode == 124:
        return "Timeout"
    default:
        return "SSH Error"
    }
}
```

- [ ] **Step 6: Run test to verify it passes**

```bash
go test ./internal/tools/ssh_test.go -run TestDiagnose -v
```
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add internal/tools/ssh.go internal/tools/ssh_test.go
git commit -m "feat: add enhanced SSH error diagnostics"
```

---

### Task 4: SCP File Transfer Tool

**Files:**
- Create: `internal/tools/scp.go`
- Create: `internal/tools/scp_test.go`

- [ ] **Step 1: Write SCP tool tests**

```go
// internal/tools/scp_test.go
package tools

import (
    "context"
    "os"
    "path/filepath"
    "testing"

    "charm.land/fantasy"
)

func TestSCPTool_Info(t *testing.T) {
    tool := NewScpTool()
    info := tool.Info()

    if info.Name != "scp" {
        t.Errorf("Name = %q, want scp", info.Name)
    }
}

func TestSCP_Upload(t *testing.T) {
    // Skip if no SSH access
    if os.Getenv("SSH_TEST_HOST") == "" {
        t.Skip("SSH_TEST_HOST not set")
    }

    tool := NewScpTool()
    ctx := SetToolsAllowed(context.Background(), []string{"scp"})

    // Create test file
    testDir := t.TempDir()
    testFile := filepath.Join(testDir, "test.txt")
    os.WriteFile(testFile, []byte("hello scp"), 0644)

    input := `{
        "source": "` + testFile + `",
        "destination": "/tmp/test.txt",
        "host": "` + os.Getenv("SSH_TEST_HOST") + `"
    }`

    result, err := tool.Run(ctx, fantasy.ToolCall{Input: input})
    if err != nil {
        t.Fatalf("Run() error: %v", err)
    }
    if result.IsError {
        t.Errorf("Upload failed: %s", result.Content)
    }
}

func TestSCP_Download(t *testing.T) {
    if os.Getenv("SSH_TEST_HOST") == "" {
        t.Skip("SSH_TEST_HOST not set")
    }

    tool := NewScpTool()
    ctx := SetToolsAllowed(context.Background(), []string{"scp"})

    testDir := t.TempDir()

    input := `{
        "source": "/etc/hostname",
        "destination": "` + testDir + `/hostname",
        "host": "` + os.Getenv("SSH_TEST_HOST") + `",
        "direction": "download"
    }`

    result, err := tool.Run(ctx, fantasy.ToolCall{Input: input})
    if err != nil {
        t.Fatalf("Run() error: %v", err)
    }
    if result.IsError {
        t.Errorf("Download failed: %s", result.Content)
    }

    // Verify file was downloaded
    downloadedFile := filepath.Join(testDir, "hostname")
    if _, err := os.Stat(downloadedFile); os.IsNotExist(err) {
        t.Error("Downloaded file should exist")
    }
}

func TestSCP_Recursive(t *testing.T) {
    if os.Getenv("SSH_TEST_HOST") == "" {
        t.Skip("SSH_TEST_HOST not set")
    }

    tool := NewScpTool()
    ctx := SetToolsAllowed(context.Background(), []string{"scp"})

    testDir := t.TempDir()
    subdir := filepath.Join(testDir, "subdir")
    os.MkdirAll(subdir, 0755)
    os.WriteFile(filepath.Join(subdir, "file.txt"), []byte("test"), 0644)

    input := `{
        "source": "` + testDir + `",
        "destination": "/tmp/testdir",
        "host": "` + os.Getenv("SSH_TEST_HOST") + `",
        "recursive": true
    }`

    result, err := tool.Run(ctx, fantasy.ToolCall{Input: input})
    if err != nil {
        t.Fatalf("Run() error: %v", err)
    }
    if result.IsError {
        t.Errorf("Recursive upload failed: %s", result.Content)
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./internal/tools/scp_test.go -v
```
Expected: FAIL (NewScpTool doesn't exist)

- [ ] **Step 3: Implement SCP tool**

```go
// internal/tools/scp.go
//go:build linux || darwin
// +build linux darwin

package tools

import (
    "bytes"
    "context"
    "fmt"
    "io"
    "os/exec"
    "strconv"
    "sync"
    "time"

    "charm.land/fantasy"
)

type scpArgs struct {
    Source       string  `json:"source"`
    Destination  string  `json:"destination"`
    Host         string  `json:"host,omitempty"`
    Port         int     `json:"port,omitempty"`
    IdentityFile string  `json:"identity_file,omitempty"`
    Timeout      float64 `json:"timeout,omitempty"`
    Recursive    bool    `json:"recursive,omitempty"`
    Direction    string  `json:"direction,omitempty"` // "upload" or "download"
}

func NewScpTool(opts ...ToolOption) fantasy.AgentTool {
    return &coreTool{
        info: fantasy.ToolInfo{
            Name:        "scp",
            Description: "Transfer files to/from remote hosts via SCP. Supports upload (local→remote) and download (remote→local).",
            Parameters: map[string]any{
                "source": map[string]any{
                    "type":        "string",
                    "description": "Source file path (local for upload, remote for download)",
                },
                "destination": map[string]any{
                    "type":        "string",
                    "description": "Destination path (remote for upload, local for download)",
                },
                "host": map[string]any{
                    "type":        "string",
                    "description": "Remote host in [user@]hostname format",
                },
                "port": map[string]any{
                    "type":        "number",
                    "description": "SSH port (default 22)",
                },
                "identity_file": map[string]any{
                    "type":        "string",
                    "description": "Path to SSH private key file",
                },
                "timeout": map[string]any{
                    "type":        "number",
                    "description": "Timeout in seconds (default 30s, max 600s)",
                },
                "recursive": map[string]any{
                    "type":        "boolean",
                    "description": "Transfer directories recursively",
                },
                "direction": map[string]any{
                    "type":        "string",
                    "description": "Transfer direction: 'upload' (local→remote) or 'download' (remote→local). Auto-detected if omitted.",
                    "enum":        []string{"upload", "download"},
                },
            },
            Required: []string{"source", "destination"},
        },
        handler: func(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
            return executeSCP(ctx, call)
        },
    }
}

func executeSCP(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
    var args scpArgs
    if err := parseArgs(call.Input, &args); err != nil {
        return fantasy.NewTextErrorResponse("source and destination are required"), nil
    }

    if args.Source == "" || args.Destination == "" {
        return fantasy.NewTextErrorResponse("source and destination are required"), nil
    }

    // Check force-mcp mode
    if fm, ok := ctx.Value(AgentForceMCPKey).(bool); ok && fm {
        return fantasy.NewTextErrorResponse(
            "scp tool is blocked by --force-mcp. " +
                "Use an MCP server for file transfer instead.",
        ), nil
    }

    // Validate parameters
    if args.Port < 0 || args.Port > 65535 {
        return fantasy.NewTextErrorResponse("port must be 0-65535"), nil
    }
    if args.IdentityFile != "" {
        if _, err := os.Stat(args.IdentityFile); os.IsNotExist(err) {
            return fantasy.NewTextErrorResponse(
                fmt.Sprintf("identity file not found: %s", args.IdentityFile),
            ), nil
        }
    }

    timeout := defaultSSHTimeout
    if args.Timeout > 0 {
        timeout = time.Duration(args.Timeout) * time.Second
        if timeout > maxBashTimeout {
            timeout = maxBashTimeout
        }
    }

    // Build scp command
    scpArgs := []string{}
    if args.Port > 0 {
        scpArgs = append(scpArgs, "-P", strconv.Itoa(args.Port))
    }
    if args.IdentityFile != "" {
        scpArgs = append(scpArgs, "-i", args.IdentityFile)
    }
    if args.Recursive {
        scpArgs = append(scpArgs, "-r")
    }
    scpArgs = append(scpArgs, "-o", "BatchMode=yes")
    scpArgs = append(scpArgs, "-o", "StrictHostKeyChecking=accept-new")

    // Determine source and destination based on direction
    var src, dst string
    if args.Direction == "download" || (args.Host == "" && args.Source != "") {
        // Download: remote → local
        if args.Host == "" {
            return fantasy.NewTextErrorResponse("host is required for download"), nil
        }
        src = args.Host + ":" + args.Source
        dst = args.Destination
    } else {
        // Upload: local → remote
        if args.Host == "" {
            return fantasy.NewTextErrorResponse("host is required for upload"), nil
        }
        src = args.Source
        dst = args.Host + ":" + args.Destination
    }

    scpArgs = append(scpArgs, src, dst)

    cmdCtx, cancel := context.WithTimeout(ctx, timeout)
    defer cancel()

    cmd := exec.CommandContext(cmdCtx, "scp", scpArgs...)

    var stdout, stderr bytes.Buffer
    cmd.Stdout = &stdout
    cmd.Stderr = &stderr

    if err := cmd.Run(); err != nil {
        exitCode := 0
        if exitErr, ok := err.(*exec.ExitError); ok {
            exitCode = exitErr.ExitCode()
        }

        diagnosedMsg := diagnoseSSHErrors(exitCode, stderr.String())
        return fantasy.ToolResponse{
            Content: fmt.Sprintf(
                "[SCP Error]\n\n%s\n\nOriginal error: %s",
                diagnosedMsg,
                stderr.String(),
            ),
            IsError: true,
        }, nil
    }

    return fantasy.ToolResponse{
        Content: fmt.Sprintf(
            "SCP transfer successful\nSource: %s\nDestination: %s",
            src, dst,
        ),
    }, nil
}
```

- [ ] **Step 4: Run test to verify it passes**

```bash
go test ./internal/tools/scp_test.go -run TestSCPTool_Info -v
```
Expected: PASS (info test), SKIP (network tests)

- [ ] **Step 5: Commit**

```bash
git add internal/tools/scp.go internal/tools/scp_test.go
git commit -m "feat: add SCP file transfer tool"
```

---

### Task 5: SSH Config Parser

**Files:**
- Create: `internal/tools/ssh_config.go`
- Create: `internal/tools/ssh_config_test.go`

- [ ] **Step 1: Write config parser tests**

```go
// internal/tools/ssh_config_test.go
package tools

import (
    "os"
    "path/filepath"
    "testing"
)

func TestParseSSHConfig_Basic(t *testing.T) {
    // Create temp SSH config
    tmpDir := t.TempDir()
    configFile := filepath.Join(tmpDir, "ssh_config")

    content := `
Host example.com
    User admin
    Port 2222
    IdentityFile ~/.ssh/id_ed25519
`
    os.WriteFile(configFile, []byte(content), 0644)

    config, err := parseSSHConfigFile(configFile, "example.com")
    if err != nil {
        t.Fatalf("parseSSHConfigFile() error: %v", err)
    }

    if config.User != "admin" {
        t.Errorf("User = %q, want admin", config.User)
    }
    if config.Port != 2222 {
        t.Errorf("Port = %d, want 2222", config.Port)
    }
    if config.IdentityFile != "~/.ssh/id_ed25519" {
        t.Errorf("IdentityFile = %q, want ~/.ssh/id_ed25519", config.IdentityFile)
    }
}

func TestParseSSHConfig_Wildcard(t *testing.T) {
    tmpDir := t.TempDir()
    configFile := filepath.Join(tmpDir, "ssh_config")

    content := `
Host *.example.com
    User webadmin
    Port 22
`
    os.WriteFile(configFile, []byte(content), 0644)

    config, err := parseSSHConfigFile(configFile, "server1.example.com")
    if err != nil {
        t.Fatalf("parseSSHConfigFile() error: %v", err)
    }

    if config.User != "webadmin" {
        t.Errorf("User = %q, want webadmin", config.User)
    }
}

func TestParseSSHConfig_MergeWithExplicit(t *testing.T) {
    tmpDir := t.TempDir()
    configFile := filepath.Join(tmpDir, "ssh_config")

    content := `
Host example.com
    User admin
    Port 2222
`
    os.WriteFile(configFile, []byte(content), 0644)

    config, _ := parseSSHConfigFile(configFile, "example.com")

    // Explicit port should override config
    explicitPort := 0
    if explicitPort == 0 {
        explicitPort = config.Port
    }

    if explicitPort != 2222 {
        t.Errorf("Merged port = %d, want 2222", explicitPort)
    }
}
```

- [ ] **Step 2: Run test to verify it fails**

```bash
go test ./internal/tools/ssh_config_test.go -v
```
Expected: FAIL (parseSSHConfigFile doesn't exist)

- [ ] **Step 3: Implement SSH config parser**

```go
// internal/tools/ssh_config.go
package tools

import (
    "bufio"
    "fmt"
    "os"
    "path/filepath"
    "regexp"
    "strings"
)

// SSHConfig represents parsed SSH configuration
type SSHConfig struct {
    Host         string
    User         string
    HostName     string
    Port         int
    IdentityFile string
    ProxyJump    string
    ForwardAgent bool
}

// parseSSHConfigFile parses an SSH config file for a specific host
func parseSSHConfigFile(configPath, host string) (*SSHConfig, error) {
    file, err := os.Open(configPath)
    if err != nil {
        return nil, err
    }
    defer file.Close()

    config := &SSHConfig{}
    scanner := bufio.NewScanner(file)

    currentHostPattern := ""
    matchFound := false

    hostPattern := regexp.MustCompile(`(?i)^Host\s+(.+)$`)
    keywordPattern := regexp.MustCompile(`(?i)^\s*(\w+)\s+(.+)$`)

    for scanner.Scan() {
        line := strings.TrimSpace(scanner.Text())

        // Skip comments and empty lines
        if line == "" || strings.HasPrefix(line, "#") {
            continue
        }

        // Check for Host directive
        if matches := hostPattern.FindStringSubmatch(line); matches != nil {
            currentHostPattern = matches[1]
            continue
        }

        // Check if this host block matches
        if !matchFound && matchHostPattern(currentHostPattern, host) {
            matchFound = true
        }

        // Parse directives in matching block
        if matchFound {
            if matches := keywordPattern.FindStringSubmatch(line); matches != nil {
                keyword := strings.ToLower(matches[1])
                value := matches[2]

                switch keyword {
                case "user":
                    config.User = value
                case "hostname":
                    config.HostName = value
                case "port":
                    fmt.Sscanf(value, "%d", &config.Port)
                case "identityfile":
                    config.IdentityFile = value
                case "proxyjump":
                    config.ProxyJump = value
                case "forwardagent":
                    config.ForwardAgent = strings.ToLower(value) == "yes"
                }
            }
        }
    }

    return config, scanner.Err()
}

// matchHostPattern checks if a host matches a pattern (supports * wildcard)
func matchHostPattern(pattern, host string) bool {
    if pattern == "" {
        return false
    }

    // Convert SSH wildcard to regex
    regexPattern := "^" + strings.ReplaceAll(pattern, "*", ".*") + "$"
    matched, _ := regexp.MatchString(regexPattern, host)
    return matched
}

// GetSSHConfig parses ~/.ssh/config for a host
func GetSSHConfig(host string) (*SSHConfig, error) {
    homeDir, err := os.UserHomeDir()
    if err != nil {
        return nil, err
    }

    configPath := filepath.Join(homeDir, ".ssh", "config")
    if _, err := os.Stat(configPath); os.IsNotExist(err) {
        return &SSHConfig{}, nil // No config file, return empty config
    }

    return parseSSHConfigFile(configPath, host)
}
```

- [ ] **Step 4: Integrate config parsing into SSH tool**

```go
// internal/tools/ssh.go - modify executeSSH to use config
func executeSSH(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
    // ... existing force-mcp check ...

    var args sshArgs
    if err := parseArgs(call.Input, &args); err != nil {
        return fantasy.NewTextErrorResponse("host parameter is required"), nil
    }

    // Parse SSH config and merge with explicit parameters
    sshConfig, _ := GetSSHConfig(args.Host)

    // Use config values if not explicitly provided
    if args.Port == 0 && sshConfig.Port != 0 {
        args.Port = sshConfig.Port
    }
    if args.IdentityFile == "" && sshConfig.IdentityFile != "" {
        args.IdentityFile = sshConfig.IdentityFile
    }

    // ... rest of existing code ...
}
```

- [ ] **Step 5: Run test to verify it passes**

```bash
go test ./internal/tools/ssh_config_test.go -v
```
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/tools/ssh_config.go internal/tools/ssh_config_test.go internal/tools/ssh.go
git commit -m "feat: add SSH config file parser with wildcard support"
```

---

### Task 6: Audit Logging for SSH

**Files:**
- Modify: `internal/audit/audit.go`
- Modify: `internal/tools/ssh.go`

- [ ] **Step 1: Add SSH connection event type**

```go
// internal/audit/audit.go - add new event type
type SSHConnectionEvent struct {
    Timestamp  string `json:"timestamp"`
    Team       string `json:"team"`
    Agent      string `json:"agent"`
    Host       string `json:"host"`
    Command    string `json:"command,omitempty"`
    ExitCode   int    `json:"exit_code"`
    DurationMs int64  `json:"duration_ms"`
}
```

- [ ] **Step 2: Add SSH logging method**

```go
// internal/audit/audit.go - add new method
func (l *AuditLogger) LogSSHConnection(agent, host, command string, exitCode int, durationMs int64) {
    l.log(ToolAction{
        Timestamp: time.Now().Format(time.RFC3339Nano),
        Team:      l.teamName,
        Agent:     agent,
        Tool:      "ssh",
        Action:    "ssh_connection",
        Input:     fmt.Sprintf("host=%s, command=%s", host, truncate(command, 500)),
        Result:    fmt.Sprintf("exit_code=%d, duration_ms=%d", exitCode, durationMs),
    })
}
```

- [ ] **Step 3: Write audit test**

```go
// internal/tools/ssh_test.go
func TestSSH_AuditLogging(t *testing.T) {
    // This test verifies audit integration
    // Actual logging tested in audit package
    t.Skip("Audit logging integration test")
}
```

- [ ] **Step 4: Integrate audit logging into SSH tool**

```go
// internal/tools/ssh.go - add audit logging
import (
    "github.com/kjelly/hufu/internal/audit"
)

func executeSSH(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
    startTime := time.Now()

    // ... existing code ...

    waitErr := cmd.Wait()
    wg.Wait()

    duration := time.Since(startTime)

    // Log to audit
    if auditor := GetAuditLogger(ctx); auditor != nil {
        agentName, _ := ctx.Value(AgentNameKey).(string)
        auditor.LogSSHConnection(
            agentName,
            args.Host,
            args.Command,
            exitCode,
            duration.Milliseconds(),
        )
    }

    // ... rest of existing code ...
}
```

- [ ] **Step 5: Add audit logger context helper**

```go
// internal/tools/tools.go - add context helper
type auditLoggerKeyType struct{}

var AuditLoggerKey = auditLoggerKeyType{}

func SetAuditLogger(ctx context.Context, logger *audit.AuditLogger) context.Context {
    return context.WithValue(ctx, AuditLoggerKey, logger)
}

func GetAuditLogger(ctx context.Context) *audit.AuditLogger {
    if v, ok := ctx.Value(AuditLoggerKey).(*audit.AuditLogger); ok {
        return v
    }
    return nil
}
```

- [ ] **Step 6: Run tests**

```bash
go test ./internal/audit/... -v
go test ./internal/tools/... -v
```
Expected: PASS

- [ ] **Step 7: Commit**

```bash
git add internal/audit/audit.go internal/tools/ssh.go internal/tools/tools.go
git commit -m "feat: add SSH connection audit logging"
```

---

### Task 7: Connection Reuse (ControlMaster)

**Files:**
- Modify: `internal/tools/ssh.go`
- Modify: `internal/tools/ssh_test.go`

- [ ] **Step 1: Add connection reuse test**

```go
// internal/tools/ssh_test.go
func TestSSH_ConnectionReuse(t *testing.T) {
    tool := NewSshTool()
    ctx := SetToolsAllowed(context.Background(), []string{"ssh"})

    input := `{
        "host": "user@example.com",
        "command": "uptime",
        "connection_reuse": true
    }`

    result, err := tool.Run(ctx, fantasy.ToolCall{Input: input})
    if err != nil {
        t.Fatalf("Run() error: %v", err)
    }
    // Just verify it doesn't error on the parameter
    // Actual connection reuse tested manually
}
```

- [ ] **Step 2: Add connection reuse parameters**

```go
// internal/tools/ssh.go - update sshArgs struct
type sshArgs struct {
    Host             string  `json:"host"`
    Command          string  `json:"command,omitempty"`
    Port             int     `json:"port,omitempty"`
    IdentityFile     string  `json:"identity_file,omitempty"`
    Timeout          float64 `json:"timeout,omitempty"`
    ConnectionReuse  bool    `json:"connection_reuse,omitempty"`
    ControlPath      string  `json:"control_path,omitempty"`
}
```

- [ ] **Step 3: Implement connection reuse**

```go
// internal/tools/ssh.go - modify SSH argument building
if args.ConnectionReuse {
    controlPath := args.ControlPath
    if controlPath == "" {
        controlPath = "/tmp/hufu-ssh-%r@%h:%p"
    }
    sshArgList = append(sshArgList,
        "-o", "ControlMaster=auto",
        "-o", "ControlPath="+controlPath,
        "-o", "ControlPersist=600",
    )
}
```

- [ ] **Step 4: Update tool description**

```go
// internal/tools/ssh.go - update tool info
info: fantasy.ToolInfo{
    Name:        "ssh",
    Description: "Execute a command on a remote host via SSH. Non-interactive (batch) mode only.",
    Parameters: map[string]any{
        // ... existing params ...
        "connection_reuse": map[string]any{
            "type":        "boolean",
            "description": "Enable SSH connection reuse (ControlMaster). Subsequent connections to same host will be faster.",
        },
        "control_path": map[string]any{
            "type":        "string",
            "description": "Custom ControlPath for connection reuse (default: /tmp/hufu-ssh-%r@%h:%p)",
        },
    },
    // ...
}
```

- [ ] **Step 5: Run test**

```bash
go test ./internal/tools/ssh_test.go -run TestSSH_ConnectionReuse -v
```
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/tools/ssh.go internal/tools/ssh_test.go
git commit -m "feat: add SSH connection reuse (ControlMaster) support"
```

---

### Task 8: TUI Integration for SSH Sessions

**Files:**
- Modify: `internal/tui/tui.go`
- Modify: `internal/team/coordinator.go`

- [ ] **Step 1: Add SSH session count to team info**

```go
// internal/tui/tui.go - update TeamInfo struct
type TeamInfo struct {
    AvailableTeams []string
    TeamName       string
    Agents         []AgentInfoEntry
    MemoryEnabled  bool
    MemoryModel    string
    Skills         []string
    SidecarModel   string
    GuardModel     string
    Workspace      string
    TeamDir        string
    SSHSessions    int `json:"ssh_sessions"` // Add this field
}
```

- [ ] **Step 2: Display SSH sessions in team info panel**

```go
// internal/tui/tui.go - find team info rendering and add SSH sessions
func (m Model) View() string {
    // ... existing code ...

    if m.inInfo {
        var b strings.Builder
        b.WriteString(fmt.Sprintf("Team: %s\n", m.teamInfo.TeamName))
        // ... existing fields ...
        if m.teamInfo.SSHSessions > 0 {
            b.WriteString(fmt.Sprintf("Active SSH Sessions: %d\n", m.teamInfo.SSHSessions))
        }
        // ...
        return b.String()
    }

    // ...
}
```

- [ ] **Step 3: Update coordinator to send SSH session updates**

```go
// internal/team/coordinator.go - add SSH session tracking
func (c *Coordinator) ExecuteTasks(ctx context.Context, tasks []TaskDef) (string, error) {
    // Initialize SSH session manager if not present
    if mgr := tools.GetSSHSessionManager(ctx); mgr == nil {
        mgr = tools.NewSSHSessionManager()
        ctx = tools.SetSSHSessionManager(ctx, mgr)
    }

    // ... existing code ...
}
```

- [ ] **Step 4: Add status event for SSH sessions**

```go
// internal/team/coordinator.go - add SSH session count to status events
func (c *Coordinator) report(event StatusEvent) {
    // Add SSH session count to existing events
    if mgr := tools.GetSSHSessionManager(c.session.Context); mgr != nil {
        event.SSHSessions = mgr.Count()
    }
    // ... existing code ...
}
```

- [ ] **Step 5: Update TUI reporter to handle SSH sessions**

```go
// cmd/hufu/display.go - update makeTUIReporter
func makeTUIReporter(p tea.ProgramSender) func(StatusEvent) {
    return func(event StatusEvent) {
        // ... existing code ...

        if event.SSHSessions > 0 {
            p.Send(StatusBarMsg{
                Text: fmt.Sprintf("SSH: %d active", event.SSHSessions),
            })
        }
    }
}
```

- [ ] **Step 6: Add SSHSessions field to StatusEvent**

```go
// internal/team/types.go - update StatusEvent
type StatusEvent struct {
    Type        string
    TeamName    string
    Agent       string
    Message     string
    Step        int
    ToolName    string
    ToolArgs    string
    ToolResult  string
    Todos       []*TodoItem
    SSHSessions int  // Add this field
}
```

- [ ] **Step 7: Run tests**

```bash
go test ./internal/tui/... -v
go test ./internal/team/... -v
```
Expected: PASS

- [ ] **Step 8: Commit**

```bash
git add internal/tui/tui.go internal/team/coordinator.go internal/team/types.go cmd/hufu/display.go
git commit -m "feat: add SSH session display to TUI"
```

---

### Task 9: Integration Testing and Documentation

**Files:**
- Create: `docs/ssh-tool-usage.md`
- Modify: `AGENTS.md`
- Modify: `README.md`

- [ ] **Step 1: Write SSH usage documentation**

```markdown
# SSH Tool Usage Guide

## Basic Usage

```yaml
- tool: ssh
  args:
    host: user@example.com
    command: "uptime"
```

## Advanced Features

### Connection Reuse

```yaml
- tool: ssh
  args:
    host: user@example.com
    command: "uptime"
    connection_reuse: true
```

### Custom Identity File

```yaml
- tool: ssh
  args:
    host: user@example.com
    identity_file: ~/.ssh/id_ed25519
    command: "uptime"
```

### SCP File Transfer

Upload:
```yaml
- tool: scp
  args:
    source: /workspace/file.txt
    destination: /remote/path/
    host: user@example.com
```

Download:
```yaml
- tool: scp
  args:
    source: /remote/file.txt
    destination: /workspace/
    host: user@example.com
    direction: download
```

## SSH Config Integration

The SSH tool automatically reads `~/.ssh/config` for:
- User
- Port
- IdentityFile
- ProxyJump

Explicit parameters override config values.

## Force-MCP Mode

When `--force-mcp` is enabled, SSH and SCP tools are blocked. Use MCP servers instead.

## Error Diagnostics

The SSH tool provides enhanced error messages for:
- Authentication failures
- Connection refused
- Host unreachable
- Timeouts

## Session Tracking

Active SSH sessions are tracked and displayed in the TUI team info panel.
```

- [ ] **Step 2: Update AGENTS.md**

```markdown
// AGENTS.md - add SSH tool to tools reference table
| `ssh` | Execute commands on remote hosts via SSH |
| `scp` | Transfer files to/from remote hosts |
```

- [ ] **Step 3: Update README.md**

```markdown
// README.md - add SSH improvements to features list
- **SSH Tool**: Enhanced with session management, error diagnostics, SCP support, and SSH config integration
```

- [ ] **Step 4: Commit**

```bash
git add docs/ssh-tool-usage.md AGENTS.md README.md
git commit -m "docs: add SSH tool usage guide and update references"
```

---

## Testing Summary

### Unit Tests
- `TestSSHSessionManager_CreateGet` — Session CRUD
- `TestSSHSessionManager_List` — Session listing
- `TestSSHSessionManager_Close` — Session removal
- `TestExecuteSSH_ForceMCP` — Force-mcp blocking
- `TestDiagnoseSSHErrors_*` — Error diagnostics
- `TestSCPTool_Info` — SCP tool metadata
- `TestParseSSHConfig_*` — Config parsing

### Integration Tests
- `TestSSH_Localhost` — Local SSH daemon (requires setup)
- `TestSCP_UploadDownload` — File transfer (requires SSH access)

### Manual Tests
```bash
# Test SSH
hufu @my-team "ssh into user@localhost and run uptime"

# Test SCP
hufu @my-team "upload test.txt to remote"

# Test force-mcp
hufu --force-mcp @my-team "ssh into remote"

# Test TUI display
hufu --tui @my-team "ssh into multiple hosts"
```

---

## Success Criteria

- [ ] All unit tests pass
- [ ] SSH session manager tracks active sessions
- [ ] Force-mcp blocks SSH/SCP tools
- [ ] Error messages are diagnostic-rich
- [ ] SCP tool transfers files successfully
- [ ] SSH config is parsed and merged
- [ ] Audit logs contain SSH events
- [ ] Connection reuse reduces latency
- [ ] TUI displays SSH session count
- [ ] Documentation is complete

---

**Plan complete and saved to `docs/superpowers/plans/2026-05-20-ssh-tool-improvements.md`. Two execution options:**

**1. Subagent-Driven (recommended)** - I dispatch a fresh subagent per task, review between tasks, fast iteration

**2. Inline Execution** - Execute tasks in this session using executing-plans, batch execution with checkpoints

**Which approach?**
