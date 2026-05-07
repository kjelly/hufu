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

// bashPrivEscRe matches sudo or ssh used as commands at any point in a pipeline or
// subshell, so the plain bash tool cannot escalate privileges or open remote sessions.
// Recognised command-start positions: beginning of string, pipe (|), logical ops (&, ;),
// subshell open (, backtick (`), $( substitution, and newline.
var bashPrivEscRe = regexp.MustCompile("(?:^|[|;&(\n\x60]|\\$\\()\\s*(?:sudo|ssh)(?:\\s|$)")

var absPathInCmdRe = regexp.MustCompile(`(?:^|\s|=|>|<|")(/(?:[a-zA-Z0-9_.-]+/)+[a-zA-Z0-9_.-]*)(?:\s|"|$|;|&|\|)`)

var cdPathRe = regexp.MustCompile(`(?:^|\s|;|&|\||\n)cd\s+(?:'([^']+)'|"([^"]+)"|([^ \t\n;&|'"` + "`" + `]+))`)

var systemPathPrefixes = []string{"/usr/", "/bin/", "/sbin/", "/lib/", "/lib32/", "/lib64/", "/proc/", "/sys/", "/dev/", "/etc/alternatives/"}

type bashArgs struct {
	Command string  `json:"command"`
	Timeout float64 `json:"timeout,omitempty"`
}

func setNetNamespace(cmd *exec.Cmd) error {
	uid := os.Getuid()
	gid := os.Getgid()
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:   syscall.CLONE_NEWNET | syscall.CLONE_NEWUSER,
		UidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: uid, Size: 1},
		},
		GidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: gid, Size: 1},
		},
	}
	return nil
}

// runShellCommand runs name+args under a derived context with the given timeout,
// sets Dir and the SHELL env var, then collects stdout/stderr and builds a response.
// It is used by the bash and sudo tools.
func runShellCommand(ctx context.Context, timeout time.Duration, workDir string, networkBlock bool, name string, args ...string) (fantasy.ToolResponse, error) {
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
	bashPath, err := exec.LookPath("bash")
	if err != nil {
		bashPath = "/bin/bash"
	}
	cmd.Env = append(os.Environ(), "SHELL="+bashPath)

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
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else if cmdCtx.Err() == context.DeadlineExceeded {
			return fantasy.NewTextErrorResponse("command timed out"), nil
		}
	}
	return buildBashResponse(stdout.String(), stderr.String(), exitCode), nil
}

func NewBashTool(opts ...ToolOption) fantasy.AgentTool {
	cfg := ApplyOptions(opts)
	cfg.ToolName = "bash"
	desc := "Execute a bash command. Returns stdout and stderr. Output is truncated to the last 2000 lines or 50KB. Optionally provide a timeout in seconds."
	if cfg.RestrictedBash {
		desc = "Execute a bash command in restricted mode (rbash). 'cd' and output redirection ('>', '>>') are blocked. Commands must be in PATH; absolute paths are not allowed. The working directory is fixed. Optionally provide a timeout in seconds."
	}
	if cfg.NetworkBlock {
		desc = "Execute a bash command. Network access is blocked (--no-net). No network connections can be made. Returns stdout and stderr. Output is truncated to the last 2000 lines or 50KB. Optionally provide a timeout in seconds."
		if cfg.RestrictedBash {
			desc = "Execute a bash command in restricted mode (rbash). Network access is blocked (--no-net). 'cd' and output redirection ('>', '>>') are blocked. Commands must be in PATH; absolute paths are not allowed. The working directory is fixed. Optionally provide a timeout in seconds."
		}
	}
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
	args.Command = normalizeBashCommand(args.Command, effCfg.WorkspaceName)
	if bannedCmdRe.MatchString(args.Command) {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("command '%s' is not allowed", args.Command)), nil
	}
	if bashPrivEscRe.MatchString(args.Command) {
		return fantasy.NewTextErrorResponse("sudo and ssh are not available in the bash tool — use the sudo or ssh tool instead"), nil
	}

	if err := checkBashPathConsent(args.Command, effCfg); err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
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

	if restricted {
		restrictedPath := effCfg.RestrictedPath
		if rp, ok := ctx.Value(AgentRestrictedPathKey).(string); ok && rp != "" {
			restrictedPath = rp
		}
		return runShellCommandRestricted(ctx, timeout, effCfg.WorkDir, restrictedPath, effCfg.NetworkBlock, args.Command)
	}

	return runShellCommand(ctx, timeout, effCfg.WorkDir, effCfg.NetworkBlock, "bash", "-c", args.Command)
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
		if exitErr, ok := waitErr.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else if cmdCtx.Err() == context.DeadlineExceeded {
			return fantasy.NewTextErrorResponse("command timed out"), nil
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

func checkBashPathConsent(command string, cfg ToolConfig) error {
	pathsToCheck := extractPathsFromCommand(command, cfg.WorkDir)
	seen := make(map[string]bool)
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

		if cfg.PathConsent != nil {
			result, err := cfg.PathConsent.AskConsent(absPath, "access", cfg.ToolName, p)
			if err != nil {
				return fmt.Errorf("path '%s' is outside allowed paths and consent failed: %w", absPath, err)
			}
			switch result {
			case ConsentDenied:
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

	return paths
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
