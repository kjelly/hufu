//go:build linux || darwin
// +build linux darwin

package tools

// wait_for polls a shell command inside a single tool call until it succeeds
// or a deadline passes. Waiting via the model (sleep + re-check loops) burned
// ~39% of a real run's LLM round-trips, each re-sending the full history;
// this collapses the whole wait into one call. Path consent and command
// guards run once on entry — polls must never re-prompt the user.

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"time"

	"charm.land/fantasy"

	"github.com/anomalyco/hufu/internal/audit"
)

const (
	defaultWaitForInterval = 5 * time.Second
	minWaitForInterval     = 1 * time.Second
	defaultWaitForTimeout  = 120 * time.Second
)

type waitForArgs struct {
	Command         string  `json:"command"`
	IntervalSeconds float64 `json:"interval_seconds,omitempty"`
	TimeoutSeconds  float64 `json:"timeout_seconds,omitempty"`
	SuccessPattern  string  `json:"success_pattern,omitempty"`
	Sudo            bool    `json:"sudo,omitempty"`
}

func NewWaitForTool(opts ...ToolOption) fantasy.AgentTool {
	cfg := ApplyOptions(opts)
	cfg.ToolName = "wait_for"
	return &coreTool{
		info: fantasy.ToolInfo{
			Name:        "wait_for",
			Description: "Poll a shell command until it succeeds, in a single tool call. Use this instead of sleep + re-checking whenever you are waiting for a state change (VM boot, service ready, async job completion, file to appear). The command is re-run every interval_seconds until it exits 0 (and, if success_pattern is set, its combined stdout/stderr matches the regex) or timeout_seconds elapses. On timeout the error includes the last output and exit code so you can see the state it was stuck in. Do not include a 'sudo' prefix in the command — set sudo:true instead.",
			Parameters: map[string]any{
				"command": map[string]any{
					"type":        "string",
					"description": "Shell command to poll (runs as bash -c). Should be a quick, read-only status check.",
				},
				"interval_seconds": map[string]any{
					"type":        "number",
					"description": "Seconds between polls (optional, default 5, minimum 1)",
				},
				"timeout_seconds": map[string]any{
					"type":        "number",
					"description": "Total seconds to keep polling before giving up (optional, default 120, max 600)",
				},
				"success_pattern": map[string]any{
					"type":        "string",
					"description": "Optional Go regex the command's combined stdout/stderr must match, in addition to exit 0 (e.g. 'exited.*true', 'state:\\s*running')",
				},
				"sudo": map[string]any{
					"type":        "boolean",
					"description": "Run the command with root privileges via sudo (optional, default false)",
				},
			},
			Required: []string{"command"},
		},
		handler: func(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
			return executeWaitFor(ctx, call, cfg)
		},
	}
}

func executeWaitFor(ctx context.Context, call fantasy.ToolCall, cfg ToolConfig) (fantasy.ToolResponse, error) {
	var args waitForArgs
	if err := parseArgs(call.Input, &args); err != nil {
		return fantasy.NewTextErrorResponse("command parameter is required"), nil
	}
	if args.Command == "" {
		return fantasy.NewTextErrorResponse("command parameter is required"), nil
	}

	effCfg := cfgWithMergedPaths(cfg, ctx)

	if cdBlockRe.MatchString(args.Command) {
		return fantasy.NewTextErrorResponse("'cd' is not allowed in wait_for commands — use absolute paths instead"), nil
	}
	if bannedCmdRe.MatchString(args.Command) {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("command '%s' is not allowed", args.Command)), nil
	}
	if bashPrivEscRe.MatchString(args.Command) {
		return fantasy.NewTextErrorResponse("sudo and ssh prefixes are not allowed in wait_for commands — set sudo:true to run the poll with root privileges, or use the ssh tool for remote checks"), nil
	}

	var pattern *regexp.Regexp
	if args.SuccessPattern != "" {
		re, err := regexp.Compile(args.SuccessPattern)
		if err != nil {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid success_pattern: %v", err)), nil
		}
		pattern = re
	}

	// Consent is checked once for the whole wait: re-prompting on every poll
	// would turn an unattended wait into an interactive one.
	consentStart := time.Now()
	if err := checkBashPathConsent(ctx, args.Command, effCfg); err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	warnSlowConsent("wait_for", consentStart)

	interval := defaultWaitForInterval
	if args.IntervalSeconds > 0 {
		interval = time.Duration(args.IntervalSeconds * float64(time.Second))
		if interval < minWaitForInterval {
			interval = minWaitForInterval
		}
	}
	timeout := defaultWaitForTimeout
	if args.TimeoutSeconds > 0 {
		timeout = time.Duration(args.TimeoutSeconds * float64(time.Second))
		if timeout > maxBashTimeout {
			timeout = maxBashTimeout
		}
	}

	name := "bash"
	cmdArgs := []string{"-c", args.Command}
	if args.Sudo {
		// sudo:true must not escalate past the agent's own toolset: an agent
		// whose allowlist lacks the sudo tool cannot gain root through
		// wait_for either.
		if allowed, ok := ctx.Value(AgentToolsAllowedKey).([]string); ok {
			hasSudo := false
			for _, t := range allowed {
				if t == "sudo" {
					hasSudo = true
					break
				}
			}
			if !hasSudo {
				return fantasy.NewTextErrorResponse("sudo:true requires the sudo tool in this agent's tools allowlist"), nil
			}
		}
		name = "sudo"
		cmdArgs = []string{"bash", "-c", args.Command}
	}

	agentName, _ := ctx.Value(AgentNameKey).(string)
	started := time.Now()
	deadline := started.Add(timeout)
	waitCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	attempt := 0
	lastOutput := "(no output)"
	lastExit := -1
	for {
		attempt++
		output, exitCode, err := runPollCommand(waitCtx, name, cmdArgs, effCfg.WorkDir, effCfg.NetworkBlock)
		if err != nil {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("wait_for: %v", err)), nil
		}
		if output != "" {
			lastOutput = output
		}
		lastExit = exitCode
		audit.LogWaitPoll(agentName, args.Command, attempt, exitCode)

		if exitCode == 0 && (pattern == nil || pattern.MatchString(output)) {
			tr := TruncateTail(lastOutput, defaultMaxLines, defaultMaxBytes)
			return fantasy.NewTextResponse(fmt.Sprintf("%s\n\n[wait_for: condition met after %d attempt(s), %s elapsed]",
				tr.Content, attempt, time.Since(started).Round(time.Second))), nil
		}

		// Stop when the parent context is gone or there is no budget left for
		// another interval + poll.
		if ctx.Err() != nil || time.Until(deadline) <= interval {
			break
		}
		select {
		case <-waitCtx.Done():
		case <-time.After(interval):
			continue
		}
		break
	}

	reason := fmt.Sprintf("wait_for timed out after %s", time.Since(started).Round(time.Second))
	if ctx.Err() != nil && time.Now().Before(deadline) {
		reason = "wait_for cancelled"
	}
	tr := TruncateTail(lastOutput, defaultMaxLines, defaultMaxBytes)
	return fantasy.NewTextErrorResponse(fmt.Sprintf("%s (%d attempt(s), interval %s). Condition never met. Last output (exit code %d):\n%s",
		reason, attempt, interval, lastExit, tr.Content)), nil
}

// runPollCommand runs one poll attempt, returning combined output and the
// exit code. A context deadline mid-command is not an error: the caller
// treats it as one more failed attempt and reports the timeout itself.
func runPollCommand(ctx context.Context, name string, args []string, workDir string, networkBlock bool) (string, int, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if workDir != "" {
		cmd.Dir = workDir
	}
	if networkBlock {
		if err := setNetNamespace(cmd); err != nil {
			return "", -1, fmt.Errorf("setting network namespace: %w", err)
		}
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	runErr := cmd.Run()
	output := stdout.String()
	if stderr.Len() > 0 {
		if output != "" {
			output += "\n"
		}
		output += "STDERR:\n" + stderr.String()
	}
	exitCode := 0
	if runErr != nil {
		var exitErr *exec.ExitError
		switch {
		case errors.As(runErr, &exitErr):
			exitCode = exitErr.ExitCode()
		case ctx.Err() != nil:
			exitCode = -1
		default:
			return "", -1, fmt.Errorf("running poll command: %w", runErr)
		}
	}
	return output, exitCode, nil
}
