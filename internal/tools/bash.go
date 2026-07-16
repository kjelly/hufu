//go:build linux || darwin
// +build linux darwin

package tools

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"

	"charm.land/fantasy"
)

const defaultBashTimeout = 120 * time.Second
const maxBashTimeout = 600 * time.Second

var bannedCmdRe = regexp.MustCompile(`^(alias|bg|bind|builtin|caller|command|compgen|complete|compopt|coproc|dirs|disown|enable|fc|fg|hash|help|history|jobs|kill|logout|mapfile|popd|pushd|readonly|select|set|shopt|source|suspend|times|trap|type|typeset|ulimit|umask|unalias|wait)\s`)

var bashPrivEscRe = regexp.MustCompile("(?:^|[|;&(\n\x60]|\\$\\()\\s*(?:sudo|ssh)(?:\\s|$)")

// sudoPrefixRe and sshPrefixRe isolate which of the two bashPrivEscRe caught,
// so a bare "sudo ..." command can be auto-routed to the sudo tool while an
// ssh-involving command (including "ssh host 'sudo ...'", a remote escalation
// that must not be rerouted to local sudo) still gets the reject-with-hint.
var sudoPrefixRe = regexp.MustCompile("(?:^|[|;&(\n\x60]|\\$\\()\\s*sudo(?:\\s|$)")
var sshPrefixRe = regexp.MustCompile("(?:^|[|;&(\n\x60]|\\$\\()\\s*ssh(?:\\s|$)")

var absPathInCmdRe = regexp.MustCompile(`(?:^|\s|=|>|<|"|;)(/(?:[a-zA-Z0-9_.-]+/)*(?:[a-zA-Z0-9_.-]+))(?:\s|"|$|;|&|\|)`)

var envVarPathRe = regexp.MustCompile(`\b[A-Z_][A-Z0-9_]*=(/[a-zA-Z0-9_./-]+)`)

var cdPathRe = regexp.MustCompile(`(?:^|\s|;|&|\||\n)cd\s+(?:'([^']+)'|"([^"]+)"|([^ \t\n;&|'"` + "`" + `]+))`)

var cdBlockRe = regexp.MustCompile(`(?:^|[;&&|\|\||\(\s]+)\s*cd\s`)

// leadingCDRe matches the single most common shape models default to out of
// shell habit: "cd <dir> && <rest>" at the very start of the command, with
// no other cd anywhere else. Real runs hit this 5 times in one session
// (always this exact shape) and burned a round trip each time on a reject
// that just repeats what the tool description already says.
var leadingCDRe = regexp.MustCompile(`^\s*cd\s+(?:'([^']+)'|"([^"]+)"|(\S+))\s*&&\s*(.+)$`)

// extractLeadingCD splits a "cd <dir> && <rest>" command into its directory
// and remainder. It only fires for that exact leading shape — if a cd
// remains anywhere in rest (multiple directory changes, cd after other
// commands), it returns ok=false so the caller falls back to the normal
// reject, since that shape is more likely genuine multi-step shell logic a
// blind rewrite could misinterpret.
func extractLeadingCD(command string) (dir, rest string, ok bool) {
	m := leadingCDRe.FindStringSubmatch(command)
	if m == nil {
		return "", "", false
	}
	dir = m[1]
	if dir == "" {
		dir = m[2]
	}
	if dir == "" {
		dir = m[3]
	}
	rest = strings.TrimSpace(m[4])
	if rest == "" || cdBlockRe.MatchString(rest) {
		return "", "", false
	}
	return dir, rest, true
}

// sameDir reports whether a and b resolve to the same absolute directory.
// Used to tell a genuinely redundant "cd <dir> && ..." (same dir as an
// already-set working_directory) from a conflicting one, without resolving
// symlinks — this is just for spotting an obviously redundant prefix, not a
// security check.
func sameDir(a, b string) bool {
	absA, errA := filepath.Abs(a)
	absB, errB := filepath.Abs(b)
	if errA != nil || errB != nil {
		return false
	}
	return filepath.Clean(absA) == filepath.Clean(absB)
}

var systemPathPrefixes = []string{"/usr/", "/bin/", "/sbin/", "/lib/", "/lib32/", "/lib64/", "/proc/", "/sys/", "/dev/", "/etc/alternatives/"}

type bashArgs struct {
	Command string  `json:"command"`
	Timeout float64 `json:"timeout,omitempty"`
	WorkDir string  `json:"working_directory,omitempty"`
}

// commandReapDelay bounds how long Wait blocks after a timeout kill before
// giving up on the process and closing its pipes. Without it a child the
// kill could not reach (a root-owned sudo, EPERM) blocks Wait forever — a
// real run leaked a tool call for over two hours this way.
const commandReapDelay = 10 * time.Second

// configureCommandReaping makes a context-timeout kill reach the whole
// process tree, not just the direct child. exec.CommandContext's default
// cancel SIGKILLs only the child: grandchildren (e.g. a guestfish appliance)
// survive holding the output pipes, and a root-owned sudo child cannot be
// signalled at all. Must run after setNetNamespace, which replaces
// SysProcAttr.
func configureCommandReaping(cmd *exec.Cmd) {
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setpgid = true
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
			// EPERM for root-owned children (sudo). Try the plain kill and
			// otherwise rely on WaitDelay to unblock Wait; the orphan keeps
			// running but the tool call returns.
			return cmd.Process.Kill()
		}
		return nil
	}
	cmd.WaitDelay = commandReapDelay
}

// runShellCommand runs name+args under a derived context with the given timeout,
// sets Dir and the SHELL env var, then collects stdout/stderr and builds a response.
// It is used by the bash and sudo tools.
func runShellCommand(ctx context.Context, timeout time.Duration, workDir string, networkBlock bool, name string, args []string, envReplacer func(env []string) []string) (fantasy.ToolResponse, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, name, args...)
	if workDir != "" {
		cmd.Dir = workDir
	}
	if networkBlock {
		if err := setNetNamespace(cmd); err != nil {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to set network namespace: %v", err)), nil
		}
	}
	configureCommandReaping(cmd)
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		bashPath = "/bin/bash"
	}
	env := os.Environ()
	env = append(env, "SHELL="+bashPath)
	if envReplacer != nil {
		env = envReplacer(env)
	}
	cmd.Env = env

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return fantasy.NewTextErrorResponse("failed to create stdout pipe"), nil
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return fantasy.NewTextErrorResponse("failed to create stderr pipe"), nil
	}
	if err := cmd.Start(); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to start command: %v", err)), nil
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
		// Check the deadline before the exit error: the timeout kill makes
		// Wait report a plain signal death (exit -1), which would otherwise
		// masquerade as the command's own result.
		if cmdCtx.Err() == context.DeadlineExceeded {
			return fantasy.NewTextErrorResponse(timeoutResponseMessage(timeout, stdout.String(), stderr.String())), nil
		}
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		}
	}
	return buildBashResponse(stdout.String(), stderr.String(), exitCode), nil
}

// timeoutResponseMessage reports a command timeout together with whatever the
// command printed before it was killed, so the model can see the state it was
// stuck in instead of a bare "timed out".
func timeoutResponseMessage(timeout time.Duration, stdout, stderr string) string {
	msg := fmt.Sprintf("command timed out after %s", timeout)
	combined := strings.TrimSpace(stdout)
	if s := strings.TrimSpace(stderr); s != "" {
		if combined != "" {
			combined += "\n"
		}
		combined += "STDERR:\n" + s
	}
	if combined == "" {
		return msg
	}
	tr := TruncateTail(combined, defaultMaxLines, defaultMaxBytes)
	return msg + ". Output before the kill:\n" + tr.Content
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

// normalizeBashCommand rewrites occurrences of /{workspaceName}[/...] in a shell
// command to ./{workspaceName}[/...] so they resolve correctly relative to workDir.
// Only rewrites paths that are at a word boundary (preceded by a shell separator or
// start of string) and followed by '/' or end-of-string or a shell separator, so
// paths like /usr/workspace/... are left untouched.
func normalizeBashCommand(command, workspaceName string) string {
	if workspaceName == "" || command == "" {
		return command
	}
	prefix := "/" + workspaceName
	dotPrefix := "./" + workspaceName

	var sb strings.Builder
	sb.Grow(len(command) + 8)

	i := 0
	for i < len(command) {
		idx := strings.Index(command[i:], prefix)
		if idx == -1 {
			sb.WriteString(command[i:])
			break
		}
		absIdx := i + idx

		validStart := absIdx == 0
		if !validStart {
			prev := command[absIdx-1]
			validStart = prev == ' ' || prev == '\t' || prev == '"' || prev == '\'' ||
				prev == '=' || prev == ';' || prev == '|' || prev == '&' ||
				prev == '<' || prev == '>' || prev == '(' || prev == '`'
		}

		afterIdx := absIdx + len(prefix)
		validEnd := afterIdx >= len(command)
		if !validEnd {
			next := command[afterIdx]
			validEnd = next == '/' || next == ' ' || next == '\t' || next == '"' ||
				next == '\'' || next == ';' || next == '|' || next == '&' ||
				next == '<' || next == '>' || next == ')' || next == '`'
		}

		sb.WriteString(command[i:absIdx])
		if validStart && validEnd {
			sb.WriteString(dotPrefix)
		} else {
			sb.WriteString(prefix)
		}
		i = afterIdx
	}
	return sb.String()
}

func executeBash(ctx context.Context, call fantasy.ToolCall, cfg ToolConfig) (fantasy.ToolResponse, error) {
	effCfg := cfgWithMergedPaths(cfg, ctx)

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

	if args.WorkDir != "" {
		abs, err := filepath.Abs(args.WorkDir)
		if err != nil {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid working_directory: %v", err)), nil
		}
		abs = filepath.Clean(abs)
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

	args.Command = normalizeBashCommand(args.Command, effCfg.WorkspaceName)
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
		if effCfg.WorkDir == "" {
			return fantasy.NewTextErrorResponse("working_directory is required in direnv mode"), nil
		}
		projectEnv, err := LoadProjectEnv(effCfg.WorkDir, true)
		if err != nil {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to load project env: %v", err)), nil
		}
		return runShellCommand(ctx, timeout, effCfg.WorkDir, effCfg.NetworkBlock, "bash", []string{"-c", args.Command}, func(env []string) []string {
			bashPath, _ := exec.LookPath("bash")
			if bashPath == "" {
				bashPath = "/bin/bash"
			}
			homeDir := os.Getenv("HOME")
			if homeDir == "" {
				homeDir = "/root"
			}
			usr := os.Getenv("USER")
			if usr == "" {
				usr = "unknown"
			}
			base := []string{"HOME=" + homeDir, "USER=" + usr}
			base = append(base, projectEnv...)
			base = append(base, "PATH="+os.Getenv("PATH"))
			base = append(base, "SHELL="+bashPath)
			return base
		})
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

func runShellCommandRestricted(ctx context.Context, timeout time.Duration, workDir string, restrictedPath string, networkBlock bool, command string) (fantasy.ToolResponse, error) {
	cmdCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cmdCtx, "bash", "-r", "-c", command)
	if workDir != "" {
		cmd.Dir = workDir
	}
	if networkBlock {
		if err := setNetNamespace(cmd); err != nil {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to set network namespace: %v", err)), nil
		}
	}
	configureCommandReaping(cmd)
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		bashPath = "/bin/bash"
	}
	env := os.Environ()
	filtered := make([]string, 0, len(env))
	for _, e := range env {
		if !strings.HasPrefix(e, "PATH=") {
			filtered = append(filtered, e)
		}
	}
	pathVal := restrictedPath
	if pathVal == "" {
		pathVal = os.Getenv("PATH")
	}
	filtered = append(filtered, "PATH="+pathVal)
	filtered = append(filtered, "SHELL="+bashPath)
	cmd.Env = filtered

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return fantasy.NewTextErrorResponse("failed to create stdout pipe"), nil
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return fantasy.NewTextErrorResponse("failed to create stderr pipe"), nil
	}
	if err := cmd.Start(); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to start command: %v", err)), nil
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
		if cmdCtx.Err() == context.DeadlineExceeded {
			return fantasy.NewTextErrorResponse(timeoutResponseMessage(timeout, stdout.String(), stderr.String())), nil
		}
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		}
	}
	return buildBashResponse(stdout.String(), stderr.String(), exitCode), nil
}

var redirectRe = regexp.MustCompile(`(.+?)\s*(>|>>)\s*(\S+)\s*$`)

func rewriteBashRedirects(command string) string {
	lines := strings.Split(command, "\n")
	for i, line := range lines {
		lines[i] = rewriteLineRedirects(line)
	}
	return strings.Join(lines, "\n")
}

func rewriteLineRedirects(line string) string {
	m := redirectRe.FindStringSubmatch(line)
	if m == nil {
		return line
	}
	cmdPart := strings.TrimSpace(m[1])
	redirect := m[2]
	filePath := m[3]
	if strings.Contains(cmdPart, "|") || strings.Contains(cmdPart, "&&") || strings.Contains(cmdPart, "||") {
		return line
	}
	if redirect == ">>" {
		return cmdPart + " | tee -a " + filePath
	}
	return cmdPart + " | tee " + filePath
}

func checkBashPathConsent(ctx context.Context, command string, cfg ToolConfig) error {
	pathsToCheck := extractPathsFromCommand(command, cfg.WorkDir)
	seen := make(map[string]bool)
	var candidatePaths []string
	for _, p := range pathsToCheck {
		if seen[p] {
			continue
		}
		seen[p] = true

		if isSystemPath(p) {
			continue
		}

		absPath, err := filepath.Abs(p)
		if err != nil {
			continue
		}
		absPath = filepath.Clean(absPath)

		if isPathAllowed(absPath, cfg.AllowedPaths) {
			continue
		}

		// Skip paths that don't exist on the filesystem.
		// Bash commands often reference non-filesystem paths (e.g. AWS SSM
		// parameter paths like /visionai/env/dev3) that match the absolute
		// path regex but aren't actual file access.
		if _, err := os.Stat(absPath); err != nil {
			continue
		}

		candidatePaths = append(candidatePaths, p)
	}

	// Sidecar path review: filter out non-filesystem paths (e.g. sed replacements, env var values)
	pathReviewer := cfg.PathReviewer
	if pr := GetPathReviewerFromContext(ctx); pr != nil {
		pathReviewer = pr
	}
	if pathReviewer != nil && len(candidatePaths) > 0 {
		var realPaths []string
		for _, p := range candidatePaths {
			isFileAccess, err := pathReviewer(ctx, command, p)
			if err == nil && isFileAccess {
				realPaths = append(realPaths, p)
			}
		}
		candidatePaths = realPaths
	}

	for _, p := range candidatePaths {
		absPath, err := filepath.Abs(p)
		if err != nil {
			continue
		}
		absPath = filepath.Clean(absPath)

		if cfg.PathConsent != nil {
			result, suggestion, err := cfg.PathConsent.AskConsent(absPath, "access", cfg.ToolName, command)
			if err != nil {
				return fmt.Errorf("path '%s' is outside allowed paths and consent failed: %w", absPath, err)
			}
			switch result {
			case ConsentDenied:
				if suggestion != "" {
					return fmt.Errorf("path '%s' is outside allowed paths; user suggested '%s', retry the command using that path instead", absPath, suggestion)
				}
				return fmt.Errorf("path '%s' is outside allowed paths — access denied by user", absPath)
			}
		} else {
			return fmt.Errorf("path '%s' is outside allowed paths", absPath)
		}
	}
	return nil
}

func extractPathsFromCommand(command, workDir string) []string {
	var paths []string

	for _, match := range cdPathRe.FindAllStringSubmatch(command, -1) {
		for i := 1; i < len(match); i++ {
			if match[i] != "" {
				paths = append(paths, match[i])
			}
		}
	}

	for _, match := range absPathInCmdRe.FindAllStringSubmatch(command, -1) {
		if len(match) > 1 && match[1] != "" {
			paths = append(paths, match[1])
		}
	}

	envPaths := make(map[string]bool)
	for _, match := range envVarPathRe.FindAllStringSubmatch(command, -1) {
		if len(match) > 1 && match[1] != "" {
			envPaths[match[1]] = true
		}
	}

	filtered := make([]string, 0, len(paths))
	for _, p := range paths {
		if !envPaths[p] {
			filtered = append(filtered, p)
		}
	}
	return filtered
}

func isSystemPath(path string) bool {
	clean := filepath.Clean(path)
	for _, prefix := range systemPathPrefixes {
		if strings.HasPrefix(clean, prefix) {
			return true
		}
	}
	return false
}

func buildBashResponse(stdout, stderr string, exitCode int) fantasy.ToolResponse {
	var result strings.Builder
	if stdout != "" {
		result.WriteString(stdout)
	}
	if stderr != "" {
		if result.Len() > 0 {
			result.WriteString("\n")
		}
		result.WriteString("STDERR:\n")
		result.WriteString(stderr)
	}
	if exitCode != 0 {
		if result.Len() > 0 {
			result.WriteString("\n")
		}
		fmt.Fprintf(&result, "Exit code: %d", exitCode)
	}

	output := result.String()
	if output == "" {
		output = "(no output)"
	}

	tr := TruncateTail(output, defaultMaxLines, defaultMaxBytes)

	if exitCode != 0 {
		return fantasy.NewTextErrorResponse(tr.Content)
	}
	return fantasy.NewTextResponse(tr.Content)
}

func executeBashStreaming(ctx context.Context, call fantasy.ToolCall, cmd *exec.Cmd, outputCallback func(toolCallID, toolName, chunk string, isStderr bool)) (fantasy.ToolResponse, error) {
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return fantasy.NewTextErrorResponse("failed to create stdout pipe"), nil
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return fantasy.NewTextErrorResponse("failed to create stderr pipe"), nil
	}

	if err := cmd.Start(); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("failed to start command: %v", err)), nil
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	var stdoutChunks, stderrChunks []string

	streamOutput := func(reader io.Reader, isStderr bool) {
		defer wg.Done()
		scanner := bufio.NewScanner(reader)
		buf := make([]byte, 0, 64*1024)
		scanner.Buffer(buf, 1024*1024)
		for scanner.Scan() {
			chunk := scanner.Text()
			if outputCallback != nil {
				outputCallback(call.ID, "bash", chunk, isStderr)
			}
			mu.Lock()
			if isStderr {
				stderrChunks = append(stderrChunks, chunk)
			} else {
				stdoutChunks = append(stdoutChunks, chunk)
			}
			mu.Unlock()
		}
	}

	wg.Add(2)
	go streamOutput(stdoutPipe, false)
	go streamOutput(stderrPipe, true)

	err = cmd.Wait()
	wg.Wait()

	exitCode := 0
	if err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else if ctx.Err() == context.DeadlineExceeded {
			return fantasy.NewTextErrorResponse("command timed out"), nil
		}
	}

	return buildBashResponse(strings.Join(stdoutChunks, "\n"), strings.Join(stderrChunks, "\n"), exitCode), nil
}
