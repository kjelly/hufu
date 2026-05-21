# SSH Tool Usage Guide

## Overview

The SSH tool in hufu allows agents to execute commands on remote hosts via SSH. It supports key-based authentication, connection reuse, SSH config integration, and comprehensive error diagnostics.

## Basic Usage

```yaml
- tool: ssh
  args:
    host: user@example.com
    command: "uptime"
```

## Parameters

| Parameter | Type | Required | Default | Description |
|-----------|------|----------|---------|-------------|
| `host` | string | ✅ | — | Remote hostname. **CRITICAL**: Use exact hostname from user (e.g., `offline-test-gpu`), NOT resolved IP. SSH config requires hostnames. |
| `user` | string | ❌ | — | SSH username. Can also specify as `user@host`. Priority: explicit `user` > `user@host` > SSH config. |
| `command` | string | ❌ | — | Command to execute (omit to test connectivity) |
| `port` | number | ❌ | 22 | SSH port (0-65535). Explicit port overrides SSH config. |
| `identity_file` | string | ❌ | — | Path to SSH private key file. Explicit file overrides SSH config. |
| `timeout` | number | ❌ | 30 | Timeout in seconds (max 600s) |
| `connection_reuse` | boolean | ❌ | false | Enable SSH connection reuse (ControlMaster) |
| `control_path` | string | ❌ | /tmp/hufu-ssh-%r@%h:%p | Custom ControlPath for connection reuse |

### Parameter Priority

Parameters are resolved in this order (highest priority first):

1. **Explicit parameter** — Specified in tool call (e.g., `user: "admin"`)
2. **user@host format** — Username in host string (e.g., `admin@server.com`)
3. **SSH config** — From `~/.ssh/config`
4. **No default** — If not specified anywhere, SSH uses system default

Example:
```yaml
# Uses explicit user "root", port 2222 from config, identity from config
- tool: ssh
  args:
    host: server.example.com
    user: root
    port: 0  # 0 means "use config"
```

## Advanced Features

### Connection Reuse

Enable SSH connection reuse for faster subsequent connections to the same host:

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

### SSH Config Integration

The SSH tool automatically reads `~/.ssh/config` for:
- User
- Port
- IdentityFile
- ProxyJump
- HostName

## SSH Config Integration

The SSH tool automatically reads `~/.ssh/config` for:
- User
- Port
- IdentityFile
- ProxyJump
- HostName
- ForwardAgent

### Parameter Resolution Order

Parameters are resolved with this priority (highest first):

1. **Explicit parameter** — Specified in tool call
2. **user@host format** — Username embedded in host string
3. **SSH config** — From `~/.ssh/config`
4. **No default** — SSH uses system default if nothing specified

**Example**: If you specify `user: "root"` explicitly, it overrides both `admin@server.com` format and SSH config's `User admin` setting.

Example `~/.ssh/config`:
```
Host offline-test-gpu
    User kjelly
    Port 22
    IdentityFile ~/.ssh/id_ed25519

Host example.com
    User admin
    Port 2222
    IdentityFile ~/.ssh/id_ed25519

Host *.prod.example.com
    User deploy
    Port 22
```

### Usage Examples

```yaml
# Uses SSH config values for offline-test-gpu (User=kjelly, Port=22)
- tool: ssh
  args:
    host: offline-test-gpu
    command: "uptime"

# Explicit user overrides SSH config
- tool: ssh
  args:
    host: offline-test-gpu
    user: root  # Overrides SSH config's User=kjelly
    command: "uptime"

# Explicit port overrides SSH config
- tool: ssh
  args:
    host: offline-test-gpu
    port: 2222  # Overrides SSH config's Port=22
    command: "uptime"
```

## SCP File Transfer

### Upload (Local → Remote)

```yaml
- tool: scp
  args:
    source: /workspace/file.txt
    destination: /remote/path/
    host: user@example.com
```

### Download (Remote → Local)

```yaml
- tool: scp
  args:
    source: /remote/file.txt
    destination: /workspace/
    host: user@example.com
    direction: download
```

### Recursive Transfer

```yaml
- tool: scp
  args:
    source: /workspace/directory/
    destination: /remote/path/
    host: user@example.com
    recursive: true
```

### SCP Parameters

| Parameter | Type | Required | Default | Description |
|-----------|------|----------|---------|-------------|
| `source` | string | ✅ | — | Source file path |
| `destination` | string | ✅ | — | Destination path |
| `host` | string | ✅ | — | Remote host |
| `port` | number | ❌ | 22 | SSH port |
| `identity_file` | string | ❌ | — | SSH private key file |
| `timeout` | number | ❌ | 30 | Timeout in seconds |
| `recursive` | boolean | ❌ | false | Transfer directories recursively |
| `direction` | string | ❌ | auto | "upload" or "download" |

## Error Diagnostics

The SSH tool provides enhanced error messages with troubleshooting guidance:

### Authentication Failed
```
[SSH Error: Authentication Failed]

SSH authentication failed. Check:
- Identity file permissions (chmod 600)
- SSH agent forwarding (ssh-add -l)
- User@host format

Original error: Permission denied (publickey,password).
```

### Connection Refused
```
[SSH Error: Connection Refused]

SSH connection refused. Check:
- SSH daemon running on remote (systemctl status sshd)
- Correct port number
- Firewall rules

Original error: ssh: connect to host example.com port 22: Connection refused
```

### Host Unreachable
```
[SSH Error: Host Unreachable]

Host unreachable. Check:
- Network connectivity
- Hostname/IP correctness
- DNS resolution

Original error: ssh: connect to host example.com port 22: No route to host
```

### Timeout
```
[SSH Error: Timeout]

SSH connection timed out. Consider:
- Increasing timeout parameter
- Checking network latency
- Verifying host availability

Original error: ssh connection timed out
```

## Force-MCP Mode

When `--force-mcp` is enabled, SSH and SCP tools are blocked. Use MCP servers instead:

```bash
hufu --force-mcp @my-team "ssh into remote"
# Error: ssh tool is blocked by --force-mcp. Use an MCP server for SSH operations instead.
```

## Session Tracking

Active SSH sessions are tracked and displayed in the TUI:

- **Status Bar**: Shows "SSH: N active" when sessions are present
- **Team Info Panel** (press `i`): Displays active SSH session count

**Important**: Sessions represent **currently-executing SSH commands**, not persistent connections.

Session lifecycle:
1. Created when `executeSSH()` starts
2. Tracked in SSHSessionManager during command execution
3. Closed automatically when command completes (via defer)

The session count shows how many SSH commands are running **concurrently**.
This is useful for:
- Monitoring parallel SSH operations
- Audit trail (which agents connected to which hosts)
- Debugging stuck or long-running commands

For persistent SSH connections, use the `connection_reuse` parameter (ControlMaster),
which keeps the underlying SSH socket open for subsequent commands.

Sessions are automatically created when SSH connections are established and closed when commands complete.

## Audit Logging

All SSH connections are logged to the audit log:

- **Location**: `workspace/logs/audit/audit-{date}.jsonl`
- **Fields**: timestamp, team, agent, host, command, exit_code, duration_ms

Example log entry:
```json
{
  "timestamp": "2026-05-20T10:30:00Z",
  "team": "my-team",
  "agent": "developer",
  "tool": "ssh",
  "action": "ssh_connection",
  "input": "host=user@example.com, command=uptime",
  "result": "exit_code=0, duration_ms=150"
}
```

## Security Considerations

1. **Batch Mode**: SSH tool uses `BatchMode=yes` — no password prompts
2. **Key-Based Auth**: Requires SSH keys or established agent
3. **Host Key Checking**: Uses `StrictHostKeyChecking=accept-new`
4. **Workspace Boundaries**: SCP transfers respect workspace limits
5. **Force-MCP**: Enterprise control via `--force-mcp` flag

## Examples

### Check Connectivity
```yaml
- tool: ssh
  args:
    host: user@example.com
```

### Run Command
```yaml
- tool: ssh
  args:
    host: user@example.com
    command: "docker ps"
```

### Multiple Commands (Connection Reuse)
```yaml
- tool: ssh
  args:
    host: user@example.com
    command: "uptime"
    connection_reuse: true

- tool: ssh
  args:
    host: user@example.com
    command: "df -h"
    connection_reuse: true
```

### Deploy Application
```yaml
# Upload files
- tool: scp
  args:
    source: /workspace/build/
    destination: /opt/app/
    host: user@example.com
    recursive: true

# Run deployment
- tool: ssh
  args:
    host: user@example.com
    command: "systemctl restart app"
```

## Troubleshooting

### "Permission denied (publickey)"
1. Check key permissions: `chmod 600 ~/.ssh/id_ed25519`
2. Verify key is loaded: `ssh-add -l`
3. Add key if needed: `ssh-add ~/.ssh/id_ed25519`

### "Connection refused"
1. Check SSH daemon: `systemctl status sshd` on remote
2. Verify port number (default 22)
3. Check firewall rules

### "Host key verification failed"
1. Remove old host key: `ssh-keygen -R hostname`
2. Reconnect to accept new key

### SCP Transfer Fails
1. Verify remote directory exists
2. Check write permissions
3. Use absolute paths
