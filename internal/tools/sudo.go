package tools

import (
	"context"
	"fmt"
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
			Description: "Execute a bash command with root privileges via sudo. Use only when elevated access is genuinely required. The command runs as: sudo bash -c \"<command>\".",
			Parameters: map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "The command to run with sudo privileges (do not include 'sudo' prefix — the tool adds it)",
				},
				"timeout": map[string]any{
					"type":        "number",
					"description": "Timeout in seconds (optional, default 120s, max 600s)",
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
	if bannedCmdRe.MatchString(args.Command) {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("command '%s' is not allowed", args.Command)), nil
	}

	effCfg := cfgWithMergedPaths(cfg, ctx)

	if err := checkBashPathConsent(args.Command, effCfg); err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}

	timeout := defaultSudoTimeout
	if args.Timeout > 0 {
		timeout = time.Duration(args.Timeout) * time.Second
		if timeout > maxBashTimeout {
			timeout = maxBashTimeout
		}
	}

	return runShellCommand(ctx, timeout, effCfg.WorkDir, effCfg.NetworkBlock, "sudo", "bash", "-c", args.Command)
}
