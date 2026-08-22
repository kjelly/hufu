//go:build linux || darwin
// +build linux darwin

package tools

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"charm.land/fantasy"
)

const defaultSudoTimeout = 120 * time.Second

func NewSudoTool(opts ...ToolOption) fantasy.AgentTool {
	cfg := ApplyOptions(opts)
	cfg.ToolName = "sudo"
	return &coreTool{
		info: fantasy.ToolInfo{
			Name:        "sudo",
			Description: "Execute a bash command with root privileges via sudo. Use only when elevated access is genuinely required. The command runs as: sudo bash -c \"<command>\". Environment variables (HOME, PATH, etc.) are inherited from the parent process — do not use 'env -i' to reset them.",
			Parameters: map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "The command to run with sudo privileges (do not include 'sudo' prefix — the tool adds it)",
				},
				"timeout": map[string]any{
					"type":        "number",
					"description": "Timeout in seconds (optional, default 120s, max 600s)",
				},
				"working_directory": map[string]any{
					"type":        "string",
					"description": "Working directory for the command (optional, defaults to the project directory). Only set this if you need to run the command in a different directory.",
				},
			},
			Required: []string{"command"},
		},
		handler: func(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return executeSudo(ctx, call, cfg)
		},
	}
}

func executeSudo(ctx context.Context, call fantasy.ToolCall, cfg ToolConfig) (fantasy.ToolResponse, error) {
	var args bashArgs
	if err := parseArgs(call.Input, &args); err != nil {
		return fantasy.NewTextErrorResponse("command parameter is required"), nil
	}
	if args.Command == "" {
		return fantasy.NewTextErrorResponse("command parameter is required"), nil
	}

	effCfg := cfgWithMergedPaths(cfg, ctx)

	if len(effCfg.AllowedWritePaths) > 0 {
		return fantasy.NewTextErrorResponse("sudo tool is disabled in this runtime workflow to enforce write isolation — use structured write/edit tools instead"), nil
	}

	if args.WorkDir == "" {
		if dir, rest, ok := extractLeadingCD(args.Command); ok {
			args.WorkDir = dir
			args.Command = rest
		}
	} else if dir, rest, ok := extractLeadingCD(args.Command); ok && sameDir(dir, args.WorkDir) {
		// See the identical comment in executeBash: only strip a redundant
		// leading cd when it targets the same directory as the explicit
		// working_directory already set.
		args.Command = rest
	}

	if args.WorkDir != "" {
		abs, err := filepath.Abs(args.WorkDir)
		if err != nil {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid working_directory: %v", err)), nil
		}
		abs = filepath.Clean(abs)
		if err := enforceArtifactPathPolicy(canonicalPathForAuthorization(abs), effCfg); err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
		info, err := os.Stat(abs)
		if err != nil || !info.IsDir() {
			return fantasy.NewTextErrorResponse("working_directory does not exist or is not a directory"), nil
		}
		if !isPathAllowed(abs, effCfg.AllowedPaths) {
			if effCfg.PathConsent != nil {
				result, suggestion, err := effCfg.PathConsent.AskConsent(abs, "workdir", cfg.ToolName, args.WorkDir)
				if err != nil {
					return fantasy.NewTextErrorResponse(fmt.Sprintf("working_directory outside allowed paths and consent failed: %v", err)), nil
				}
				switch result {
				case ConsentDenied:
					if suggestion != "" {
						return fantasy.NewTextErrorResponse(fmt.Sprintf("working_directory is outside allowed paths. User suggested '%s'; retry with that directory instead.", suggestion)), nil
					}
					return fantasy.NewTextErrorResponse("working_directory is outside allowed paths — access denied by user"), nil
				}
			} else {
				return fantasy.NewTextErrorResponse("working_directory is outside allowed paths"), nil
			}
		}
		effCfg.WorkDir = abs
	}

	if cdBlockRe.MatchString(args.Command) {
		return fantasy.NewTextErrorResponse("'cd' is not allowed — use the working_directory parameter to set the working directory instead"), nil
	}

	if bannedCmdRe.MatchString(args.Command) {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("command '%s' is not allowed", args.Command)), nil
	}

	consentStart := time.Now()
	if err := checkBashPathConsent(ctx, args.Command, effCfg); err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	warnSlowConsent("sudo", consentStart)

	timeout := defaultSudoTimeout
	if args.Timeout > 0 {
		timeout = time.Duration(args.Timeout) * time.Second
		if timeout > maxBashTimeout {
			timeout = maxBashTimeout
		}
	}

	return runShellCommand(ctx, timeout, effCfg.WorkDir, effCfg.NetworkBlock, "sudo", []string{"bash", "-c", args.Command}, nil)
}
