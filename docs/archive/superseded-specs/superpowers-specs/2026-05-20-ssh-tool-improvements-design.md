# SSH Tool Improvements Design Specification

**Date**: 2026-05-20
**Author**: hufu development team
**Status**: Draft (pending review)

---

## Executive Summary

This spec proposes a comprehensive set of improvements to the SSH tool in hufu, focusing on session management, security integration, error handling, and file transfer capabilities. The improvements maintain backward compatibility while adding significant functionality for power users.

---

## Current State Analysis

### Existing Features
- Basic SSH command execution via `ssh` CLI wrapper
- Parameters: `host`, `command`, `port`, `identity_file`, `timeout`
- BatchMode authentication (no password prompts)
- StrictHostKeyChecking=accept-new
- Configurable timeout (default 30s, max 600s)
- Session hints in response text

### Identified Limitations

| Category | Issue | Impact |
|----------|-------|--------|
| **Session Management** | No state tracking | Agents cannot see active sessions |
| **Security** | `--force-mcp` not enforced | Inconsistent with tool permission model |
| **Error Handling** | Generic error messages | Poor debugging experience |
| **File Transfer** | No SCP/SFTP support | Requires external tools |
| **SSH Config** | Ignores ~/.ssh/config | Manual parameter specification |
| **Connection Reuse** | New connection per command | Slow for multi-command workflows |
| **Audit** | No logging | No compliance trail |
| **Advanced Features** | No ProxyJump, ControlMaster | Limited enterprise scenarios |

---

## Design Goals

1. **Consistency**: Align SSH tool with `--force-mcp` and tool permission model
2. **Visibility**: Make SSH sessions observable in TUI and coordinator state
3. **Usability**: Improve error messages and diagnostics
4. **Extensibility**: Add file transfer without breaking existing workflows
5. **Performance**: Support connection reuse for repeated commands
6. **Security**: Maintain audit trail and respect security boundaries

---

## Proposed Architecture

### 1. SSH Session Manager

**New Component**: `internal/tools/ssh_session.go`

```go
type SSHSessionManager struct {
    mu       sync.RWMutex
    sessions map[string]*SSHSession  // keyed by task ID
}

func (m *SSHSessionManager) Create(taskID, host string) *SSHSession
func (m *SSHSessionManager) Get(taskID string) *SSHSession
func (m *SSHSessionManager) List() []*SSHSession
func (m *SSHSessionManager) Close(taskID string)
```

**Context Integration**:
- Store manager in context via `SSHSessionKey`
- Coordinator creates manager at session start
- Each task can have one active SSH session

**TUI Integration**:
- Display active SSH sessions in team info panel
- Show host, user, port, connection time

---

### 2. Force-MCP Integration

**Change**: Add `--force-mcp` check at start of `executeSSH()`

```go
func executeSSH(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
    if fm, ok := ctx.Value(AgentForceMCPKey).(bool); ok && fm {
        return fantasy.NewTextErrorResponse(
            "ssh tool is blocked by --force-mcp. " +
            "Use an MCP server for SSH operations instead."
        ), nil
    }
    // ... rest of existing code
}
```

**Rationale**: SSH is in `ForceMCPBlockedTools` but the tool itself never checked. This aligns behavior with the documented security model.

---

### 3. Enhanced Error Diagnostics

**New Function**: `diagnoseSSHErrors(exitCode int, stderr string) string`

```go
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
    case exitCode == 124: // timeout
        return "SSH connection timed out. Consider:\n" +
               "- Increasing timeout parameter\n" +
               "- Checking network latency\n" +
               "- Verifying host availability"
    default:
        return stderr
    }
}
```

**Response Format**:
```
[SSH Error: Authentication Failed]

SSH authentication failed. Check:
- Identity file permissions (chmod 600)
- SSH agent forwarding (ssh-add -l)
- User@host format

Original error: Permission denied (publickey,password).
```

---

### 4. SCP File Transfer Support

**New Tool**: `scp` (separate from `ssh`)

```go
type scpArgs struct {
    Source       string `json:"source"`        // Local or remote path
    Destination  string `json:"destination"`   // Local or remote path
    Host         string `json:"host,omitempty"` // Remote host (for remote paths)
    Port         int    `json:"port,omitempty"`
    IdentityFile string `json:"identity_file,omitempty"`
    Timeout      float64 `json:"timeout,omitempty"`
    Recursive    bool   `json:"recursive,omitempty"`
}
```

**Usage Patterns**:
- Upload: `source=/local/file`, `destination=/remote/path`, `host=user@remote`
- Download: `source=/remote/file`, `destination=/local/path`, `host=user@remote`, `remote=true`

**Workspace Integration**:
- Downloads go to `workspace/shared/ssh-downloads/`
- Uploads from `workspace/shared/` only (security boundary)

---

### 5. SSH Config Parsing

**New Function**: `parseSSHConfig(host string) (*SSHConfig, error)`

```go
type SSHConfig struct {
    User          string
    HostName      string
    Port          int
    IdentityFile  string
    ProxyJump     string
    ForwardAgent  bool
}
```

**Implementation**:
- Parse `~/.ssh/config` using regex patterns
- Extract Host, User, HostName, Port, IdentityFile, ProxyJump
- Merge with explicit parameters (explicit wins)

**Usage**:
```go
config, _ := parseSSHConfig(args.Host)
if args.Port == 0 {
    args.Port = config.Port  // Use config default
}
```

---

### 6. Connection Reuse (ControlMaster)

**New Parameters**:
- `connection_reuse: bool` (default: false)
- `control_path: string` (default: `/tmp/hufu-ssh-%r@%h:%p`)

**SSH Arguments**:
```go
if args.ConnectionReuse {
    sshArgList = append(sshArgList,
        "-o", "ControlMaster=auto",
        "-o", "ControlPath="+args.ControlPath,
        "-o", "ControlPersist=600",
    )
}
```

**Benefits**:
- Subsequent connections are instant
- Useful for multi-command workflows
- Optional (opt-in to avoid socket cleanup issues)

---

### 7. Audit Logging

**Integration**: Use existing `internal/audit/` package

```go
audit.Log(ctx, audit.SSHConnectionEvent{
    Host:      args.Host,
    Command:   args.Command,
    Timestamp: time.Now(),
    ExitCode:  exitCode,
    Duration:  duration,
})
```

**Log Location**: `workspace/logs/audit/ssh-{date}.jsonl`

**Fields**:
- `task_id`: Coordinator task ID
- `agent_name`: Agent that made the call
- `host`: Remote host
- `command`: Executed command
- `exit_code`: SSH exit code
- `duration_ms`: Connection duration
- `timestamp`: ISO 8601 timestamp

---

### 8. Parameter Validation

**New Validation Rules**:

```go
func validateSSHArgs(args sshArgs) error {
    if args.Host == "" {
        return fmt.Errorf("host is required")
    }
    if args.Port < 0 || args.Port > 65535 {
        return fmt.Errorf("port must be 0-65535 (0=default)")
    }
    if args.IdentityFile != "" {
        if _, err := os.Stat(args.IdentityFile); os.IsNotExist(err) {
            return fmt.Errorf("identity file not found: %s", args.IdentityFile)
        }
    }
    if args.Timeout < 0 {
        return fmt.Errorf("timeout cannot be negative")
    }
    return nil
}
```

---

## Implementation Plan

### Phase 1: Core Infrastructure (Priority: High)

1. **SSH Session Manager** (`ssh_session.go`)
   - Create manager struct with mutex-protected map
   - Implement Create/Get/List/Close methods
   - Add context helpers

2. **Force-MCP Integration**
   - Add check at start of `executeSSH()`
   - Test with `--force-mcp` flag

3. **Enhanced Error Diagnostics**
   - Implement `diagnoseSSHErrors()`
   - Update response formatting
   - Add test cases for common errors

### Phase 2: Extended Functionality (Priority: Medium)

4. **SCP Tool**
   - Create `scp.go` with upload/download support
   - Integrate with workspace boundaries
   - Add parameter validation

5. **SSH Config Parsing**
   - Implement config file parser
   - Merge with explicit parameters
   - Handle edge cases (wildcards, Match blocks)

6. **Audit Logging**
   - Integrate with existing audit system
   - Define SSH event schema
   - Add log rotation

### Phase 3: Advanced Features (Priority: Low)

7. **Connection Reuse**
   - Add ControlMaster support
   - Implement socket cleanup on shutdown
   - Document caveats

8. **TUI Integration**
   - Display active sessions in team info
   - Add session count to status bar

---

## Testing Strategy

### Unit Tests

| Test | Purpose |
|------|---------|
| `TestSSHSessionManager_CreateGet` | Session CRUD operations |
| `TestDiagnoseSSHErrors_AuthFailed` | Error message formatting |
| `TestParseSSHConfig_Basic` | Config file parsing |
| `TestValidateSSHArgs_InvalidPort` | Parameter validation |
| `TestExecuteSSH_ForceMCP` | Force-mcp block enforcement |

### Integration Tests

| Test | Purpose |
|------|---------|
| `TestSSH_Localhost` | Connect to localhost (requires test SSH daemon) |
| `TestSCP_UploadDownload` | File transfer round-trip |
| `TestSSH_ConfigMerge` | Config + explicit params |

### Manual Testing

```bash
# Test SSH to local container
docker run -d -p 2222:22 linuxserver/openssh-server

# Test with hufu
hufu @my-team "use ssh tool to connect to user@localhost:2222"

# Test SCP
hufu @my-team "upload test.txt to remote host"

# Test force-mcp
hufu --force-mcp @my-team "ssh into remote"
# Should fail with clear error
```

---

## Security Considerations

### Threat Model

| Threat | Mitigation |
|--------|------------|
| Credential theft | No password storage, key-based only |
| Command injection | No shell interpolation (already implemented) |
| Man-in-the-middle | StrictHostKeyChecking=accept-new |
| Unauthorized access | `--force-mcp` block, audit logging |
| Data exfiltration | Workspace boundaries for SCP |

### Compliance

- All SSH connections logged to audit trail
- Workspace boundaries prevent arbitrary file access
- `--force-mcp` provides enterprise control

---

## Backward Compatibility

### Breaking Changes
**None** — all new features are opt-in or additive.

### Deprecated Features
**None** — existing behavior preserved.

### Migration Path
Not applicable — no breaking changes.

---

## Success Metrics

| Metric | Target | Measurement |
|--------|--------|-------------|
| Error resolution time | -50% | User feedback, issue tracking |
| Multi-command workflow speed | +300% | Benchmark with/without ControlMaster |
| Session visibility | 100% | TUI shows all active sessions |
| Security compliance | 100% | `--force-mcp` enforced |

---

## Open Questions

1. **Should SCP be a separate tool or a mode of SSH?**
   - Decision: Separate tool (`scp`) for clarity and TUI display

2. **Should connection reuse be default or opt-in?**
   - Decision: Opt-in (default: false) to avoid socket cleanup issues

3. **Should SSH config parsing be default or opt-in?**
   - Decision: Default (transparent, matches user expectations)

4. **Should audit logging be configurable?**
   - Decision: Always-on (security requirement), but log location configurable

---

## Appendix: Example Usage

### Basic SSH (unchanged)
```yaml
- tool: ssh
  args:
    host: user@server.example.com
    command: "uptime"
```

### SSH with Session Tracking
```yaml
- tool: ssh
  args:
    host: user@server.example.com
    command: "uptime"
# Session automatically tracked in context
```

### SCP Upload
```yaml
- tool: scp
  args:
    source: /workspace/shared/deploy.tar.gz
    destination: /opt/app/
    host: user@server.example.com
    recursive: true
```

### SSH with Connection Reuse
```yaml
- tool: ssh
  args:
    host: user@server.example.com
    command: "uptime"
    connection_reuse: true
# Subsequent commands reuse connection
```

### SSH with Force-MCP (blocked)
```bash
hufu --force-mcp @my-team "ssh into production"
# Error: ssh tool is blocked by --force-mcp
```

---

## References

- SSH Tool: `internal/tools/ssh.go`
- Session Context Key: `internal/tools/types.go:SSHSessionKey`
- Force-MCP: `internal/tools/tools.go:ForceMCPBlockedTools`
- Audit System: `internal/audit/`
- TUI Model: `internal/tui/tui.go`

---

## Review Checklist

- [ ] Technical feasibility verified
- [ ] Security implications reviewed
- [ ] Backward compatibility confirmed
- [ ] Test coverage planned
- [ ] Documentation updated
- [ ] User approval obtained

---

**Next Step**: Invoke `writing-plans` skill to create detailed implementation plan.
