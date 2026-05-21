//go:build linux || darwin
// +build linux darwin

package tools

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"time"

	"charm.land/fantasy"
)

type scpArgs struct {
	Source          string  `json:"source"`
	Destination     string  `json:"destination"`
	Host            string  `json:"host,omitempty"`
	Port            int     `json:"port,omitempty"`
	IdentityFile    string  `json:"identity_file,omitempty"`
	Timeout         float64 `json:"timeout,omitempty"`
	Recursive       bool    `json:"recursive,omitempty"`
	Direction       string  `json:"direction,omitempty"`
	Interactive     bool    `json:"interactive,omitempty"`
	Password        string  `json:"password,omitempty"`
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
				"interactive": map[string]any{
					"type":        "boolean",
					"description": "Enable interactive mode for password prompts. When true, SCP will prompt for passwords and use ask_user to request input from the user. Default: false (BatchMode).",
				},
				"password": map[string]any{
					"type":        "string",
					"description": "Pre-provided password for SCP. ⚠️ SECURITY WARNING: Avoid using this in YAML files. Prefer interactive: true to let the agent prompt the user securely.",
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
	startTime := time.Now()

	if fm, ok := ctx.Value(AgentForceMCPKey).(bool); ok && fm {
		return fantasy.NewTextErrorResponse(
			"scp tool is blocked by --force-mcp. "+
				"Use an MCP server for file transfer instead.",
		), nil
	}

	var args scpArgs
	if err := parseArgs(call.Input, &args); err != nil {
		return fantasy.NewTextErrorResponse("source and destination are required"), nil
	}

	if args.Source == "" || args.Destination == "" {
		return fantasy.NewTextErrorResponse("source and destination are required"), nil
	}

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

	_, cleanHost := ExtractUserFromHost(args.Host)

	timeout := defaultSSHTimeout
	if args.Timeout > 0 {
		timeout = time.Duration(args.Timeout) * time.Second
		if timeout > maxBashTimeout {
			timeout = maxBashTimeout
		}
	}

	scpArgList := []string{}
	if args.Port > 0 {
		scpArgList = append(scpArgList, "-P", strconv.Itoa(args.Port))
	}
	if args.IdentityFile != "" {
		scpArgList = append(scpArgList, "-i", args.IdentityFile)
	}
	if args.Recursive {
		scpArgList = append(scpArgList, "-r")
	}

	// Only use BatchMode if not in interactive mode
	if !args.Interactive {
		scpArgList = append(scpArgList, "-o", "BatchMode=yes")
	}
	scpArgList = append(scpArgList, "-o", "StrictHostKeyChecking=accept-new")

	var src, dst string
	if args.Direction == "download" || (args.Host != "" && args.Source != "" && args.Destination != "") {
		if args.Direction == "download" {
			if args.Host == "" {
				return fantasy.NewTextErrorResponse("host is required for download"), nil
			}
			src = args.Host + ":" + args.Source
			dst = args.Destination
		} else {
			if args.Host == "" {
				return fantasy.NewTextErrorResponse("host is required for upload"), nil
			}
			src = args.Source
			dst = args.Host + ":" + args.Destination
		}
	} else {
		return fantasy.NewTextErrorResponse("host is required for scp operation"), nil
	}

	scpArgList = append(scpArgList, src, dst)

	sessionMgr := GetSSHSessionManager(ctx)
	if sessionMgr != nil {
		_, _ = sessionMgr.Create(cleanHost, finalUser, args.Port, "")
		defer sessionMgr.Close(finalUser, cleanHost, args.Port)
	}

	// Check if password is cached in session
	var cachedPassword string
	if sessionMgr != nil {
		if pwd, ok := sessionMgr.GetPassword(finalUser, cleanHost, args.Port); ok {
			cachedPassword = pwd
		}
	}

	// Use cached password if available
	if cachedPassword != "" && args.Password == "" {
		args.Password = cachedPassword
	}

	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var cmd *exec.Cmd
	if args.Password != "" {
		// Check if sshpass is installed
		if _, err := exec.LookPath("sshpass"); err != nil {
			return fantasy.NewTextErrorResponse(
				"sshpass is required for password authentication. Please install it.",
			), nil
		}
		// Use environment variable for security
		cmd = exec.CommandContext(cmdCtx, "sshpass", "-e", "scp")
		cmd.Env = append(os.Environ(), "SSHPASS="+args.Password)
		cmd.Args = append(cmd.Args, scpArgList...)
	} else {
		cmd = exec.CommandContext(cmdCtx, "scp", scpArgList...)
	}

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	waitErr := cmd.Run()
	exitCode := 0
	stderrStr := stderr.String()

	// Check if password prompt was detected and interactive mode is enabled
	if waitErr != nil && args.Interactive && detectPasswordPrompt(stderrStr) {
		// Check if sshpass is installed
		if _, err := exec.LookPath("sshpass"); err != nil {
			return fantasy.NewTextErrorResponse(
				"sshpass is required for interactive password authentication.",
			), nil
		}

		// Ask user for password
		password, err := askUserForPassword(ctx, args.Host, finalUser, "ssh")
		if err == nil && password != "" {
			// Retry SCP with password using sshpass environment variable
			cmdCtx2, cancel2 := context.WithTimeout(ctx, timeout)
			defer cancel2()

			sshpassCmd := exec.CommandContext(cmdCtx2, "sshpass", "-e", "scp")
			sshpassCmd.Env = append(os.Environ(), "SSHPASS="+password)
			sshpassCmd.Args = append(sshpassCmd.Args, scpArgList...)

			_, stderr2, exitCode2 := runCommand(sshpassCmd)
			stderrStr = stderr2
			exitCode = exitCode2
			waitErr = nil
			if exitCode != 0 {
				waitErr = fmt.Errorf("command failed with exit code %d", exitCode)
			}

			// Cache password in session for future use (5 minute expiry) only on success
			if exitCode == 0 && sessionMgr != nil {
				sessionMgr.SetPassword(finalUser, cleanHost, args.Port, password, 5*time.Minute)
			}
		}
	} else if waitErr == nil && args.Password != "" && sessionMgr != nil {
		// If command succeeded with a provided password, cache it
		sessionMgr.SetPassword(finalUser, cleanHost, args.Port, args.Password, 5*time.Minute)
	}

	if waitErr != nil {
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		}

		duration := time.Since(startTime)

		// Log to audit
		if auditor := GetAuditLogger(ctx); auditor != nil {
			agentName, _ := ctx.Value(AgentNameKey).(string)
			auditor.LogSSHConnection(
				agentName,
				args.Host,
				fmt.Sprintf("scp %s -> %s", src, dst),
				exitCode,
				duration.Milliseconds(),
			)
		}

		diagnosedMsg := diagnoseSSHErrors(exitCode, stderrStr)
		return fantasy.ToolResponse{
			Content: fmt.Sprintf(
				"[SCP Error]\n\n%s\n\nOriginal error: %s",
				diagnosedMsg,
				stderrStr,
			),
			IsError: true,
		}, nil
	}

	duration := time.Since(startTime)

	// Log to audit
	if auditor := GetAuditLogger(ctx); auditor != nil {
		agentName, _ := ctx.Value(AgentNameKey).(string)
		auditor.LogSSHConnection(
			agentName,
			args.Host,
			fmt.Sprintf("scp %s -> %s", src, dst),
			0,
			duration.Milliseconds(),
		)
	}

	return fantasy.ToolResponse{
		Content: fmt.Sprintf(
			"SCP transfer successful\nSource: %s\nDestination: %s",
			src, dst,
		),
	}, nil
}
