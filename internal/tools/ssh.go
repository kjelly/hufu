//go:build linux || darwin
// +build linux darwin

package tools

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"
)

const defaultSSHTimeout = 30 * time.Second

type sshArgs struct {
	Host            string  `json:"host"`
	User            string  `json:"user,omitempty"`
	Command         string  `json:"command,omitempty"`
	Port            int     `json:"port,omitempty"`
	IdentityFile    string  `json:"identity_file,omitempty"`
	Timeout         float64 `json:"timeout,omitempty"`
	ConnectionReuse bool    `json:"connection_reuse,omitempty"`
	ControlPath     string  `json:"control_path,omitempty"`
}

func NewSshTool(opts ...ToolOption) fantasy.AgentTool {
	return &coreTool{
		info: fantasy.ToolInfo{
			Name:        "ssh",
			Description: "Execute a command on a remote host via SSH. CRITICAL: Use the EXACT host identifier provided by the user. If user specified 'offline-test-gpu', use 'offline-test-gpu' — DO NOT resolve to IP (10.1.24.229). SSH config settings (IdentityFile, User, Port) are tied to hostnames, not IPs. Only use IP if user explicitly provided IP.",
			Parameters: map[string]any{
				"host": map[string]any{
					"type":        "string",
					"description": "CRITICAL: Use EXACT host identifier from user. If user said 'offline-test-gpu', use 'offline-test-gpu' — NOT the resolved IP. SSH config requires hostnames. Only use IP if user explicitly provided IP address.",
				},
				"user": map[string]any{
					"type":        "string",
					"description": "SSH username (optional). Can be specified here or as user@host (e.g., 'admin@server'). Explicit user parameter takes precedence. If not specified, uses SSH config or system default.",
				},
				"command": map[string]any{
					"type":        "string",
					"description": "Command to execute on the remote host (optional; omit to test connectivity)",
				},
				"port": map[string]any{
					"type":        "number",
					"description": "SSH port (optional, 0-65535). Explicit port overrides SSH config. If not specified, uses SSH config value only (no default).",
				},
				"identity_file": map[string]any{
					"type":        "string",
					"description": "Path to SSH private key file (optional). Explicit identity file overrides SSH config.",
				},
				"timeout": map[string]any{
					"type":        "number",
					"description": "Timeout in seconds (optional, default 30s, max 600s)",
				},
				"connection_reuse": map[string]any{
					"type":        "boolean",
					"description": "Enable SSH connection reuse (ControlMaster). Subsequent connections to same host will be faster.",
				},
				"control_path": map[string]any{
					"type":        "string",
					"description": "Custom ControlPath for connection reuse (default: /tmp/hufu-ssh-%r@%h:%p)",
				},
			},
			Required: []string{"host"},
		},
		handler: func(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return executeSSH(ctx, call)
		},
	}
}

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

func executeSSH(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	startTime := time.Now()

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
	if args.Host == "" {
		return fantasy.NewTextErrorResponse("host parameter is required"), nil
	}

	// Detect if host looks like an IP address
	if looksLikeIP(args.Host) {
		// Check if this IP might have been resolved from a hostname
		// Warn agent that SSH config requires hostnames
		return fantasy.NewTextErrorResponse(
			fmt.Sprintf("Host is an IP address (%s). If the user provided a hostname (e.g., 'offline-test-gpu'), use that instead. SSH config settings (IdentityFile, User, Port) are tied to hostnames and will not work with IPs. Example: use 'ssh host=offline-test-gpu' not 'ssh host=10.1.24.229'.", args.Host),
		), nil
	}

	// Parse SSH config for fallback values
	sshConfig, _ := GetSSHConfig(args.Host)

	// Validate and apply parameters with priority:
	// Explicit parameter > user@host format > SSH config > no default
	
	// Port validation and resolution
	if args.Port < 0 || args.Port > 65535 {
		return fantasy.NewTextErrorResponse("port must be 0-65535"), nil
	}
	if args.IdentityFile != "" {
		if _, err := os.Stat(args.IdentityFile); os.IsNotExist(err) {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("identity file not found: %s", args.IdentityFile)), nil
		}
	}
	
	// Resolve port: explicit > SSH config
	if args.Port == 0 && sshConfig.Port != 0 {
		args.Port = sshConfig.Port
	}
	
	// Resolve identity file: explicit > SSH config
	if args.IdentityFile == "" && sshConfig.IdentityFile != "" {
		args.IdentityFile = sshConfig.IdentityFile
	}
	
	// Resolve user: explicit > user@host format > SSH config
	userFromHost, cleanHost := ExtractUserFromHost(args.Host)
	finalUser := args.User
	if finalUser == "" {
		finalUser = userFromHost
	}
	if finalUser == "" && sshConfig.User != "" {
		finalUser = sshConfig.User
	}
	
	// Build SSH host argument (user@host or just host)
	sshHost := cleanHost
	if finalUser != "" {
		sshHost = finalUser + "@" + cleanHost
	}

	timeout := defaultSSHTimeout
	if args.Timeout > 0 {
		timeout = time.Duration(args.Timeout) * time.Second
		if timeout > maxBashTimeout {
			timeout = maxBashTimeout
		}
	}

	// Build the SSH argument list without shell interpolation to avoid injection.
	sshArgList := []string{
		"-o", "BatchMode=yes",
		"-o", "StrictHostKeyChecking=accept-new",
		"-o", fmt.Sprintf("ConnectTimeout=%d", max(5, int(timeout.Seconds()/4))),
	}

	// Add connection reuse options
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

	if args.Port > 0 {
		sshArgList = append(sshArgList, "-p", strconv.Itoa(args.Port))
	}
	if args.IdentityFile != "" {
		sshArgList = append(sshArgList, "-i", args.IdentityFile)
	}
	sshArgList = append(sshArgList, sshHost)
	if args.Command != "" {
		sshArgList = append(sshArgList, args.Command)
	}

	sessionMgr := GetSSHSessionManager(ctx)
	if sessionMgr != nil {
		session, _ := sessionMgr.Create(cleanHost, finalUser, args.Port, "")
		if session != nil {
			defer func() {
				sessionMgr.Close(cleanHost)
			}()
		}
	}

	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, "ssh", sshArgList...)

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return fantasy.NewTextErrorResponse("failed to create stdout pipe"), nil
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return fantasy.NewTextErrorResponse("failed to create stderr pipe"), nil
	}
	if err := cmd.Start(); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to start ssh: %v", err)), nil
	}

	var wg sync.WaitGroup
	var stdout, stderr bytes.Buffer
	wg.Add(2)
	go func() { defer wg.Done(); io.Copy(&stdout, stdoutPipe) }()
	go func() { defer wg.Done(); io.Copy(&stderr, stderrPipe) }()

	waitErr := cmd.Wait()
	wg.Wait()

	duration := time.Since(startTime)

	exitCode := 0
	if waitErr != nil {
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else if cmdCtx.Err() == context.DeadlineExceeded {
			return fantasy.NewTextErrorResponse("ssh connection timed out"), nil
		}
	}

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
	} else {
		// Add SSH context hint for agent (only on success)
		response.Content += fmt.Sprintf(
			"\n\n[SSH Session Active] You have connected to %s. "+
				"To execute additional commands on this host, use the ssh tool again with the SAME host identifier (keep using '%s' as provided). "+
				"Do NOT embed 'ssh' in bash commands - use the ssh tool directly.",
			args.Host,
			args.Host,
		)
	}

	return response, nil
}

// looksLikeIP checks if a string looks like an IPv4 or IPv6 address
func looksLikeIP(s string) bool {
	// Simple IPv4 check: x.x.x.x where x is 1-3 digits
	if strings.Count(s, ".") == 3 {
		parts := strings.Split(s, ".")
		if len(parts) == 4 {
			for _, part := range parts {
				if len(part) == 0 || len(part) > 3 {
					return false
				}
				for _, c := range part {
					if c < '0' || c > '9' {
						return false
					}
				}
			}
			return true
		}
	}
	// Simple IPv6 check: contains colons
	if strings.Contains(s, ":") {
		return true
	}
	return false
}
