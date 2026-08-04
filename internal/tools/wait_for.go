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
	maxWaitForTimeout      = 30 * time.Minute
	// unfalsifiablePollLimit is how many consecutive silent polls are allowed
	// when the wait also requires a success_pattern. A pattern is matched
	// against the command's output, so a command that prints nothing can never
	// satisfy it — continuing to poll only burns the timeout. A real run spent
	// four consecutive 30-minute waits this way, 110 minutes in total, because
	// the polled path did not exist and its error was being discarded.
	unfalsifiablePollLimit = 3
)

// stderrSuppressionRe matches the shell idioms that discard a polled command's
// error output. Inside a poller they are actively harmful: a wrong path or a
// misspelled flag stops being an error and becomes a condition that is simply
// never met, so the wait runs to its full timeout with nothing to show. Every
// silent wait in the observed 110-minute stall carried one of these.
var stderrSuppressionRe = regexp.MustCompile(`\s*2\s*>\s*(?:/dev/null|&\s*-)`)

// exposePollErrors removes stderr suppression so a broken poll command reports
// why it is broken. It reports whether anything was rewritten so the caller can
// say so rather than silently changing what the agent asked for.
func exposePollErrors(command string) (string, bool) {
	rewritten := stderrSuppressionRe.ReplaceAllString(command, "")
	return rewritten, rewritten != command
}

type waitForArgs struct {
	Command         string  `json:"command"`
	IntervalSeconds float64 `json:"interval_seconds,omitempty"`
	TimeoutSeconds  float64 `json:"timeout_seconds,omitempty"`
	SuccessPattern  string  `json:"success_pattern,omitempty"`
	Until           string  `json:"until,omitempty"`
	Sudo            bool    `json:"sudo,omitempty"`
	TerminalID      string  `json:"terminal_id,omitempty"`
}

func NewWaitForTool(opts ...ToolOption) fantasy.AgentTool {
	cfg := ApplyOptions(opts)
	cfg.ToolName = "wait_for"
	return &coreTool{
		info: fantasy.ToolInfo{
			Name:        "wait_for",
			Description: "Poll a shell command until its condition is met, in a single tool call. Use this instead of sleep + re-checking whenever you are waiting for a state change (VM boot, service ready, async job completion, file to appear). By default (until: success), the command is re-run until it exits 0. Set until: failure to wait until it exits non-zero, for example when waiting for a local process check such as 'ps -p 123 -o pid= > /dev/null 2>&1' to report that the process has exited. If success_pattern is set, combined stdout/stderr must also match the regex. timeout_seconds defaults to 120 and is capped at 1800. On timeout the error includes the last output and exit code so you can see the state it was stuck in. When the thing you are waiting for is a process started with terminal_start, the wait ends as soon as that process exits without the condition being met — the session id is picked up from the command automatically, or pass terminal_id. Do NOT discard errors with '2>/dev/null' — a wrong path would stop being an error and become a condition that is simply never met; the redirection is removed automatically and the error is reported instead. If success_pattern is set and the command prints nothing at all, the wait is abandoned early because the pattern cannot match, so make the check print the state you are matching on. Prefer sudo:true over a 'sudo' prefix in the command (a stray prefix is tolerated but sudo:true is the clean way to ask for root). A command containing 'ssh' is always rejected — use the ssh tool for remote checks instead.",
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
					"description": "Optional Go regex the command's combined stdout/stderr must match, in addition to the selected until exit condition (e.g. 'exited.*true', 'state:\\s*running')",
				},
				"until": map[string]any{
					"type":        "string",
					"enum":        []string{"success", "failure"},
					"description": "Exit condition that completes the wait: success (default) waits for exit 0; failure waits for a non-zero exit (e.g. a process no longer exists)",
				},
				"sudo": map[string]any{
					"type":        "boolean",
					"description": "Run the command with root privileges via sudo (optional, default false)",
				},
				"terminal_id": map[string]any{
					"type":        "string",
					"description": "Optional terminal session id (from terminal_start) whose process this wait depends on. The wait is abandoned as soon as that process exits without the condition being met, instead of polling a dead process until the timeout. A session id already mentioned in the command is detected automatically, so set this only when the command does not name it.",
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
	if args.Until != "" && args.Until != "success" && args.Until != "failure" {
		return fantasy.NewTextErrorResponse(`until must be "success" or "failure"`), nil
	}

	effCfg := cfgWithMergedPaths(cfg, ctx)

	if cdBlockRe.MatchString(args.Command) {
		// wait_for has no working_directory parameter, but the single most
		// common shape ("cd <dir> && <rest>") is harmless here: each poll
		// runs the whole string fresh in its own "bash -c" subshell, so the
		// cd never persists across polls or leaks into anything else. Only
		// that exact leading shape is let through — anything with cd
		// anywhere else still rejects below.
		if _, _, ok := extractLeadingCD(args.Command); !ok {
			return fantasy.NewTextErrorResponse("'cd' is not allowed in wait_for commands — use absolute paths instead"), nil
		}
	}
	if bannedCmdRe.MatchString(args.Command) {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("command '%s' is not allowed", args.Command)), nil
	}
	// A model that writes "sudo foo" as the command habitually does one of
	// two things wrong: forgets sudo:true, or sets sudo:true and *also*
	// leaves the literal "sudo " in the command text. Both used to be a
	// flat reject ("sudo and ssh prefixes are not allowed"), which a real
	// run hit even with sudo:true already set — one wasted round trip to
	// relearn what the tool description already said. Since sudo:true runs
	// the whole command through `sudo bash -c "<command>"`, a redundant
	// "sudo" inside it is a harmless no-op (sudo re-invoked as already-root
	// just proceeds) — so a bare sudo command with no ssh involved can
	// simply be treated as sudo:true instead of rejected. ssh still always
	// rejects: this tool has no way to safely parse or route it.
	needsSudo := args.Sudo
	if sshPrefixRe.MatchString(args.Command) {
		return fantasy.NewTextErrorResponse("ssh prefixes are not allowed in wait_for commands — use the ssh tool for remote checks"), nil
	}
	if sudoPrefixRe.MatchString(args.Command) {
		needsSudo = true
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
	timeoutCapped := false
	requestedTimeout := timeout
	if args.TimeoutSeconds > 0 {
		requestedTimeout = time.Duration(args.TimeoutSeconds * float64(time.Second))
		timeout = requestedTimeout
		if timeout > maxWaitForTimeout {
			timeout = maxWaitForTimeout
			timeoutCapped = true
		}
	}
	timeoutNotice := ""
	if timeoutCapped {
		timeoutNotice = fmt.Sprintf(" (requested timeout %s capped to %s)", requestedTimeout, timeout)
	}

	pollCommand, errorsExposed := exposePollErrors(args.Command)
	rewriteNotice := ""
	if errorsExposed {
		rewriteNotice = " [wait_for removed stderr suppression from the command so a broken check reports its error instead of polling silently]"
	}

	name := "bash"
	cmdArgs := []string{"-c", pollCommand}
	if needsSudo {
		// sudo (whether from sudo:true or inferred from a literal "sudo" in
		// the command) must not escalate past the agent's own toolset: an
		// agent whose allowlist lacks the sudo tool cannot gain root through
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
		cmdArgs = []string{"bash", "-c", pollCommand}
	}

	// Which terminal-started processes this wait depends on. An explicit
	// terminal_id wins; otherwise any session id the command already names is
	// used, because an agent tailing a session's log has stated the dependency
	// there just as clearly.
	var watchedTerminals []string
	if args.TerminalID != "" {
		watchedTerminals = terminalSessionIDsIn(args.TerminalID)
		if len(watchedTerminals) == 0 {
			return fantasy.NewTextErrorResponse("terminal_id must be a terminal session id returned by terminal_start (term- followed by 24 hex characters)"), nil
		}
	} else {
		watchedTerminals = terminalSessionIDsIn(args.Command)
	}
	livenessProbe := terminalLivenessFrom(ctx)

	agentName, _ := ctx.Value(AgentNameKey).(string)
	started := time.Now()
	deadline := started.Add(timeout)
	waitCtx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()

	attempt := 0
	lastOutput := "(no output)"
	lastExit := -1
	silentPolls := 0
	deadTerminalPolls := 0
	observedChange := false
	var firstObservation string
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
		// Audit the command as executed, not as requested: the rewrite above is
		// what actually ran, and an audit trail that disagrees with the shell is
		// worse than none.
		audit.LogWaitPoll(agentName, pollCommand, attempt, exitCode)

		// Track whether this wait is observing anything at all. A poll whose
		// output and exit code never move is not watching a state change, and
		// saying so turns a silent 30-minute timeout into a diagnosis.
		observation := fmt.Sprintf("%d\x00%s", exitCode, output)
		if attempt == 1 {
			firstObservation = observation
		} else if observation != firstObservation {
			observedChange = true
		}

		conditionMet := exitCode == 0
		if args.Until == "failure" {
			conditionMet = exitCode != 0
		}
		// A deadline-aborted command also returns non-zero. It is not evidence
		// that an until:failure condition was met.
		if waitCtx.Err() == nil && conditionMet && (pattern == nil || pattern.MatchString(output)) {
			tr := TruncateTail(lastOutput, defaultMaxLines, defaultMaxBytes)
			return fantasy.NewTextResponse(fmt.Sprintf("%s\n\n[wait_for: condition met after %d attempt(s), %s elapsed%s%s]",
				tr.Content, attempt, time.Since(started).Round(time.Second), timeoutNotice, rewriteNotice)), nil
		}

		// A success_pattern is matched against output, so a command that prints
		// nothing cannot ever satisfy it. Fail fast with the reason instead of
		// polling a condition that is unfalsifiable by construction.
		if pattern != nil {
			if output == "" {
				silentPolls++
			} else {
				silentPolls = 0
			}
			if silentPolls >= unfalsifiablePollLimit {
				return fantasy.NewTextErrorResponse(fmt.Sprintf(
					"wait_for gave up after %d attempt(s), %s elapsed: the command produced no output at all, so success_pattern %q can never match. "+
						"The check itself is almost certainly wrong — verify the path exists and the command prints what you expect before waiting on it. Last exit code %d.%s",
					attempt, time.Since(started).Round(time.Second), args.SuccessPattern, lastExit, rewriteNotice)), nil
			}
		}

		// The condition is not met, so ask whether the process this wait depends
		// on is even alive. Polling the log of a process that already exited can
		// never succeed, and that is where a real run lost 110 minutes.
		if dead := exitedWatchedTerminals(waitCtx, livenessProbe, watchedTerminals); len(dead) > 0 {
			// Output written just before exit can still be in flight, so a
			// process that died during this wait gets one more poll. One that was
			// already gone when the wait began has nothing left to flush.
			if deadTerminalPolls > 0 || allExitedBefore(dead, started) {
				return fantasy.NewTextErrorResponse(fmt.Sprintf(
					"wait_for gave up after %d attempt(s), %s elapsed: %s, and the condition was still not met. "+
						"Waiting cannot change that — call terminal_read on the session to see why it exited, fix that, and start it again. Last output (exit code %d):\n%s%s",
					attempt, time.Since(started).Round(time.Second), describeExitedTerminals(dead), lastExit,
					TruncateTail(lastOutput, defaultMaxLines, defaultMaxBytes).Content, rewriteNotice)), nil
			}
			deadTerminalPolls++
		} else {
			deadTerminalPolls = 0
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

	reason := fmt.Sprintf("wait_for timed out after %s%s", time.Since(started).Round(time.Second), timeoutNotice)
	if ctx.Err() != nil && time.Now().Before(deadline) {
		reason = "wait_for cancelled"
	}
	// A wait whose observation never moved was not watching a state change.
	// Naming that is the difference between "retry with a longer timeout" and
	// "the check is wrong" — the wrong reading cost a real run 110 minutes.
	stalled := ""
	if attempt > 1 && !observedChange {
		stalled = fmt.Sprintf(" The command returned the same exit code and the same output on all %d attempts, so this wait was not observing a changing state — check that the path and command are correct rather than waiting longer.", attempt)
	}
	tr := TruncateTail(lastOutput, defaultMaxLines, defaultMaxBytes)
	return fantasy.NewTextErrorResponse(fmt.Sprintf("%s (%d attempt(s), interval %s). Condition never met.%s%s Last output (exit code %d):\n%s",
		reason, attempt, interval, stalled, rewriteNotice, lastExit, tr.Content)), nil
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
	configureCommandReaping(cmd)
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
