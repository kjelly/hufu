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
	Source       string  `json:"source"`
	Destination  string  `json:"destination"`
	Host         string  `json:"host,omitempty"`
	Port         int     `json:"port,omitempty"`
	IdentityFile string  `json:"identity_file,omitempty"`
	Timeout      float64 `json:"timeout,omitempty"`
	Recursive    bool    `json:"recursive,omitempty"`
	Direction    string  `json:"direction,omitempty"`
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

	timeout := defaultSSHTimeout
	if args.Timeout > 0 {
		timeout = time.Duration(args.Timeout) * time.Second
		if timeout > maxBashTimeout {
			timeout = maxBashTimeout
		}
	}

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
