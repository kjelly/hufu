//go:build linux || darwin
// +build linux darwin

package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/fantasy"
)

type bashArgs struct {
	Command string  `json:"command"`
	Timeout float64 `json:"timeout,omitempty"`
	WorkDir string  `json:"working_directory,omitempty"`
}

func NewBashTool(opts ...ToolOption) fantasy.AgentTool {
	cfg := ApplyOptions(opts)
	cfg.ToolName = "bash"
	var desc string
	if cfg.Direnv {
		desc = "Execute a bash command with .envrc/.env environment loaded from the working directory (--direnv). Loads environment from .env (key=value) or via direnv (full shell .envrc support). 'cd' is not allowed — use working_directory to change directories. Returns stdout and stderr. Optionally provide a timeout in seconds."
	} else if cfg.RestrictedBash {
		desc = "Execute a bash command in restricted mode (rbash). 'cd' and output redirection ('>', '>>') are blocked — use working_directory to change directories. Commands must be in PATH; absolute paths are not allowed. The working directory is fixed. Returns stdout and stderr. Optionally provide a timeout in seconds."
	} else if cfg.NetworkBlock {
		desc = "Execute a bash command. Network access is blocked (--no-net). No network connections can be made. 'cd' is not allowed — use working_directory to change directories. Returns stdout and stderr. Output is truncated to the last 2000 lines or 50KB. Optionally provide a timeout in seconds."
	} else {
		desc = "Execute a bash command. 'cd' is not allowed — use working_directory to change directories. Returns stdout and stderr. Output is truncated to the last 2000 lines or 50KB. Optionally provide a timeout in seconds."
	}
	if cfg.RestrictedBash && cfg.Direnv {
		desc = "Execute a bash command with .envrc/.env environment loaded in restricted mode (rbash). Loads environment from .env (key=value) or via direnv (full shell .envrc support). 'cd' and output redirection ('>', '>>') are blocked — use working_directory to change directories. Commands must be in PATH; absolute paths are not allowed. Returns stdout and stderr. Optionally provide a timeout in seconds."
	} else if cfg.NetworkBlock && cfg.Direnv {
		desc = "Execute a bash command with .envrc/.env environment loaded (--direnv). Network access is blocked (--no-net). Loads environment from .env (key=value) or via direnv (full shell .envrc support). 'cd' is not allowed — use working_directory to change directories. Returns stdout and stderr. Optionally provide a timeout in seconds."
	} else if cfg.NetworkBlock && cfg.RestrictedBash {
		desc = "Execute a bash command in restricted mode (rbash). Network access is blocked (--no-net). 'cd' and output redirection ('>', '>>') are blocked — use working_directory to change directories. Commands must be in PATH; absolute paths are not allowed. The working directory is fixed. Returns stdout and stderr. Optionally provide a timeout in seconds."
	}
	desc += " Environment variables (HOME, PATH, etc.) are inherited from the parent process — do not use 'env -i' to reset them."
	return &coreTool{
		info: fantasy.ToolInfo{
			Name:        "bash",
			Description: desc,
			Parameters: map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "Bash command to execute",
				},
				"timeout": map[string]any{
					"type":        "number",
					"description": "Timeout in seconds (optional, default 120s, max 600s)",
				},
				"working_directory": map[string]any{
					"type":        "string",
					"description": "Working directory for the command (optional, defaults to the project directory). Only set this if you need to run the command in a different directory. In direnv mode this parameter is required.",
				},
			},
			Required: []string{"command"},
		},
		handler: func(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return executeBash(ctx, call, cfg)
		},
	}
}

func executeBash(ctx context.Context, call fantasy.ToolCall, cfg ToolConfig) (fantasy.ToolResponse, error) {
	effCfg := cfgWithMergedPaths(cfg, ctx)

	if len(effCfg.AllowedWritePaths) > 0 {
		return fantasy.NewTextErrorResponse("bash tool is disabled in this runtime workflow to enforce write isolation — use structured write/edit tools instead"), nil
	}

	var args bashArgs
	if err := parseArgs(call.Input, &args); err != nil {
		return fantasy.NewTextErrorResponse("command parameter is required"), nil
	}
	if args.Command == "" {
		return fantasy.NewTextErrorResponse("command parameter is required"), nil
	}

	if args.WorkDir == "" {
		if dir, rest, ok := extractLeadingCD(args.Command); ok {
			args.WorkDir = dir
			args.Command = rest
		}
	} else if dir, rest, ok := extractLeadingCD(args.Command); ok && sameDir(dir, args.WorkDir) {
		// A model that already set working_directory sometimes also
		// habitually prefixes the command with a redundant "cd <same dir>
		// && ...". Only strip it when the cd target matches the explicit
		// working_directory — if they differ, that may be a genuine (if
		// unusual) request for two different directories, so the reject
		// below still applies rather than silently picking one.
		args.Command = rest
	}

	args.Command = normalizeBashCommand(args.Command, effCfg.WorkspaceName)
	if readOnly, _ := ctx.Value(AgentReadOnlyExecutionKey).(bool); readOnly {
		// Enforce the task's capability contract before validating the requested
		// directory. A denied mutating command must not be reported as a benign
		// path error merely because it also supplied an unapproved workdir.
		if err := checkReadOnlyBashCommand(args.Command); err != nil {
			ReportToolExecutionDisposition(ctx, ToolExecutionDisposition{
				Kind:       "policy_denied",
				ReasonCode: ReadOnlyBashDenialReason(args.Command),
				ToolName:   "bash",
				ToolCallID: call.ID,
				Executed:   false,
			})
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
	}

	if args.WorkDir != "" {
		abs, errResp := resolveBashWorkDir(args.WorkDir, effCfg, cfg.ToolName)
		if errResp != nil {
			return *errResp, nil
		}
		effCfg.WorkDir = abs
	}

	if bannedCmdRe.MatchString(args.Command) {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("command '%s' is not allowed", args.Command)), nil
	}

	// A model that already has sudo permission reflexively typing "sudo foo"
	// into the bash tool used to just get rejected with a hint to retry via
	// the sudo tool — and a real run hit that same rejection 11 times in one
	// session because the hint didn't reliably change its next tool choice.
	// Since sudo bash -c "<command>" (what the sudo tool itself runs) executes
	// the whole line as root regardless of where "sudo" appears in it, a bare
	// sudo command with no ssh involved can be routed there directly instead
	// of bouncing the round trip back to the model. Restricted/direnv bash
	// have their own execution paths below and are not covered by this reroute.
	forwardToSudo := false
	if bashPrivEscRe.MatchString(args.Command) {
		hasSSH := sshPrefixRe.MatchString(args.Command)
		if !hasSSH && sudoPrefixRe.MatchString(args.Command) && !effCfg.RestrictedBash && !effCfg.Direnv {
			if ok, _, _ := CheckToolPermission(ctx, "sudo"); ok {
				forwardToSudo = true
			}
		}
		if !forwardToSudo {
			msg := "sudo and ssh are not available in the bash tool"
			var alts []string
			if ok, _, _ := CheckToolPermission(ctx, "sudo"); ok {
				alts = append(alts, "sudo")
			}
			if ok, _, _ := CheckToolPermission(ctx, "ssh"); ok {
				alts = append(alts, "ssh")
			}
			if len(alts) > 0 {
				msg += " — use the " + strings.Join(alts, " or ") + " tool instead"
			} else {
				msg += ", and no sudo/ssh tool is enabled for this agent. Do not retry with sudo; report the exact command for the user to run manually, or ask the user to add 'sudo' to the agent's tools in team.yaml"
			}
			return fantasy.NewTextErrorResponse(msg), nil
		}
	}

	consentStart := time.Now()
	if err := checkBashPathConsent(ctx, args.Command, effCfg); err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	warnSlowConsent("bash", consentStart)

	if cdBlockRe.MatchString(args.Command) {
		return fantasy.NewTextErrorResponse("'cd' is not allowed — use the working_directory parameter to set the working directory instead"), nil
	}

	restricted := effCfg.RestrictedBash
	if restricted {
		args.Command = rewriteBashRedirects(args.Command)
	}

	timeout := defaultBashTimeout
	if args.Timeout > 0 {
		timeout = time.Duration(args.Timeout) * time.Second
		if timeout > maxBashTimeout {
			timeout = maxBashTimeout
		}
	}

	if effCfg.Direnv {
		return runBashDirenv(ctx, timeout, effCfg, args.Command)
	}

	if restricted {
		restrictedPath := effCfg.RestrictedPath
		if rp, ok := ctx.Value(AgentRestrictedPathKey).(string); ok && rp != "" {
			restrictedPath = rp
		}
		return runShellCommandRestricted(ctx, timeout, effCfg.WorkDir, restrictedPath, effCfg.NetworkBlock, args.Command)
	}

	if forwardToSudo {
		resp, err := runShellCommand(ctx, timeout, effCfg.WorkDir, effCfg.NetworkBlock, "sudo", []string{"bash", "-c", args.Command}, nil)
		if err == nil {
			resp.Content = "[bash: command required root privileges — automatically routed through the sudo tool]\n" + resp.Content
		}
		return resp, err
	}

	return runShellCommand(ctx, timeout, effCfg.WorkDir, effCfg.NetworkBlock, "bash", []string{"-c", args.Command}, nil)
}
