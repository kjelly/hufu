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

const defaultSSHTimeout = 30 * time.Second

type sshArgs struct {
	Host         string  `json:"host"`
	Command      string  `json:"command,omitempty"`
	Port         int     `json:"port,omitempty"`
	IdentityFile string  `json:"identity_file,omitempty"`
	Timeout      float64 `json:"timeout,omitempty"`
}

func NewSshTool(opts ...ToolOption) fantasy.AgentTool {
	return &coreTool{
		info: fantasy.ToolInfo{
			Name:        "ssh",
			Description: "Execute a command on a remote host via SSH. Non-interactive (batch) mode only — no password prompts. Requires key-based authentication or an already-established agent. TIP: When a hostname is provided (e.g., 'offline-test-gpu'), use it as-is rather than resolving to IP. SSH config settings (IdentityFile, User, etc.) are typically tied to hostnames.",
			Parameters: map[string]any{
				"host": map[string]any{
					"type":        "string",
					"description": "Remote host in [user@]hostname format. If a hostname is provided (e.g., 'offline-test-gpu'), use it as-is. If an IP is provided, use it directly. Do not resolve hostname to IP yourself - use whichever form the user specified.",
				},
				"command": map[string]any{
					"type":        "string",
					"description": "Command to execute on the remote host (optional; omit to test connectivity)",
				},
				"port": map[string]any{
					"type":        "number",
					"description": "SSH port (optional, default 22)",
				},
				"identity_file": map[string]any{
					"type":        "string",
					"description": "Path to SSH private key file (optional)",
				},
				"timeout": map[string]any{
					"type":        "number",
					"description": "Timeout in seconds (optional, default 30s, max 600s)",
				},
			},
			Required: []string{"host"},
		},
		handler: func(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return executeSSH(ctx, call)
		},
	}
}

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
	if args.Host == "" {
		return fantasy.NewTextErrorResponse("host parameter is required"), nil
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
	if args.Port > 0 {
		sshArgList = append(sshArgList, "-p", strconv.Itoa(args.Port))
	}
	if args.IdentityFile != "" {
		sshArgList = append(sshArgList, "-i", args.IdentityFile)
	}
	sshArgList = append(sshArgList, args.Host)
	if args.Command != "" {
		sshArgList = append(sshArgList, args.Command)
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

	exitCode := 0
	if waitErr != nil {
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else if cmdCtx.Err() == context.DeadlineExceeded {
			return fantasy.NewTextErrorResponse("ssh connection timed out"), nil
		}
	}

	response := buildBashResponse(stdout.String(), stderr.String(), exitCode)
	
	// Add SSH context hint for agent
	if exitCode == 0 {
		// Append SSH context hint to the response content
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
