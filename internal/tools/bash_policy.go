//go:build linux || darwin
// +build linux darwin

package tools

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"charm.land/fantasy"
)

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

var redirectRe = regexp.MustCompile(`(.+?)\s*(>|>>)\s*(\S+)\s*$`)

// checkReadOnlyBashCommand enforces the side_effect:none execution contract
// for the intentionally broad bash tool. It deliberately accepts a small,
// inspection-oriented command set rather than attempting to recognize every
// possible mutation in shell syntax. Any syntax that can introduce a hidden
// command, redirect output, or run an unknown executable is denied.
func checkReadOnlyBashCommand(command string) error {
	trimmed := strings.TrimSpace(command)
	if trimmed == "" {
		return fmt.Errorf("read-only bash policy: command is required")
	}
	if hasUnsafeReadOnlyShellSyntax(trimmed) {
		return fmt.Errorf("read-only bash policy denied shell expansion, control syntax, or output redirection")
	}

	segments, ok := splitReadOnlyBashSegments(trimmed)
	if !ok {
		return fmt.Errorf("read-only bash policy denied malformed shell syntax")
	}
	if len(segments) == 0 {
		return fmt.Errorf("read-only bash policy denied an empty command")
	}
	for _, segment := range segments {
		if err := checkReadOnlyBashSegment(strings.TrimSpace(segment)); err != nil {
			return err
		}
	}
	return nil
}

// IsReadOnlyBashCommand reports whether command is limited to the same narrow,
// inspection-only grammar enforced for side_effect:none workers. It is an
// observation helper: callers may use it to prove that an already-completed
// attempt did not mutate state; it does not grant a command any permission.
//
// A leading `cd <dir> &&` is safe for this purpose because it changes only the
// shell subprocess's current directory. The remainder must still pass the
// strict read-only grammar.
func IsReadOnlyBashCommand(command string) bool {
	if checkReadOnlyBashCommand(command) == nil {
		return true
	}
	_, rest, ok := extractLeadingCD(command)
	return ok && checkReadOnlyBashCommand(rest) == nil
}

func checkReadOnlyBashSegment(segment string) error {
	fields := strings.Fields(segment)
	if len(fields) == 0 {
		return fmt.Errorf("read-only bash policy denied an empty command segment")
	}
	name := fields[0]
	if name == "git" {
		if !readOnlyGitCommand(fields[1:]) {
			return fmt.Errorf("read-only bash policy denied git command %q", segment)
		}
		return nil
	}
	if name == "go" {
		if len(fields) < 2 || !map[string]bool{"test": true, "vet": true, "list": true, "env": true, "version": true, "doc": true}[fields[1]] {
			return fmt.Errorf("read-only bash policy denied go command %q", segment)
		}
		return nil
	}
	if name == "golangci-lint" {
		if len(fields) < 2 || fields[1] != "run" || containsField(fields[2:], "--fix") {
			return fmt.Errorf("read-only bash policy denied golangci-lint command %q", segment)
		}
		return nil
	}
	if !map[string]bool{
		"rg": true, "grep": true, "sed": true, "cat": true, "head": true,
		"tail": true, "find": true, "ls": true, "pwd": true, "wc": true,
		"sort": true, "uniq": true, "cut": true, "tr": true, "basename": true,
		"dirname": true, "stat": true, "file": true, "sha256sum": true,
		"md5sum": true, "diff": true, "echo": true,
	}[name] {
		return fmt.Errorf("read-only bash policy denied command %q", name)
	}
	if (name == "sed" && containsField(fields[1:], "-i")) ||
		(name == "find" && containsAnyField(fields[1:], "-delete", "-exec", "-execdir", "-ok", "-okdir", "-fprint", "-fls")) {
		return fmt.Errorf("read-only bash policy denied potentially mutating command %q", segment)
	}
	return nil
}

// hasUnsafeReadOnlyShellSyntax rejects shell syntax that could alter command
// execution. Quoted grep/regex patterns are treated as data, so a literal
// parenthesis or semicolon inside single or double quotes does not trigger a
// false positive.
func hasUnsafeReadOnlyShellSyntax(command string) bool {
	var quote byte
	for i := 0; i < len(command); i++ {
		r := command[i]
		if r == '\\' && quote != '\'' {
			// A backslash quotes the following byte for the purpose of this
			// small lexical gate. The subsequent byte is never command syntax.
			i++
			continue
		}
		if quote != 0 {
			if r == quote {
				quote = 0
			}
			continue
		}
		switch r {
		case '\'', '"':
			quote = r
		case '\n', ';', '`', '$', '<', '>', '(', ')', '{', '}':
			return true
		}
	}
	return quote != 0
}

// splitReadOnlyBashSegments permits only the ordinary inspection pipelines
// agents need: commands joined by |, &&, or ||. It recognizes separators only
// outside quotes and rejects a lone &, so background jobs cannot escape the
// read-only policy.
func splitReadOnlyBashSegments(command string) ([]string, bool) {
	var segments []string
	start := 0
	var quote byte
	for i := 0; i < len(command); i++ {
		ch := command[i]
		if ch == '\\' && quote != '\'' {
			i++
			continue
		}
		if quote != 0 {
			if ch == quote {
				quote = 0
			}
			continue
		}
		if ch == '\'' || ch == '"' {
			quote = ch
			continue
		}
		if ch != '|' && ch != '&' {
			continue
		}
		width := 1
		if ch == '&' {
			if i+1 >= len(command) || command[i+1] != '&' {
				return nil, false
			}
			width = 2
		} else if i+1 < len(command) && command[i+1] == '|' {
			width = 2
		}
		segment := strings.TrimSpace(command[start:i])
		if segment == "" {
			return nil, false
		}
		segments = append(segments, segment)
		i += width - 1
		start = i + 1
	}
	if quote != 0 {
		return nil, false
	}
	last := strings.TrimSpace(command[start:])
	if last == "" {
		return nil, false
	}
	return append(segments, last), true
}

func readOnlyGitCommand(args []string) bool {
	if len(args) == 0 {
		return false
	}
	idx := 0
	for idx < len(args) {
		switch args[idx] {
		case "--no-pager", "--paginate":
			idx++
		case "-C":
			idx += 2
		default:
			goto command
		}
	}
command:
	if idx >= len(args) {
		return false
	}
	subcommand := args[idx]
	allowed := map[string]bool{
		"status": true, "log": true, "show": true, "diff": true,
		"rev-parse": true, "merge-base": true, "ls-files": true,
		"ls-tree": true, "cat-file": true, "blame": true,
		"shortlog": true, "describe": true,
	}
	return allowed[subcommand]
}

func containsField(fields []string, want string) bool {
	for _, field := range fields {
		if field == want {
			return true
		}
	}
	return false
}

func containsAnyField(fields []string, wants ...string) bool {
	for _, want := range wants {
		if containsField(fields, want) {
			return true
		}
	}
	return false
}

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

	// Resolve safe, path-producing substitutions such as
	// `sha256sum "$(which trec)"`. This is static analysis only: arbitrary
	// substitutions must never be executed while checking path consent.
	paths = append(paths, extractLookupSubstitutionPaths(command)...)

	filtered := make([]string, 0, len(paths))
	for _, p := range paths {
		if !envPaths[p] {
			filtered = append(filtered, p)
		}
	}
	return filtered
}

// extractLookupSubstitutionPaths closes the consent bypass where an absolute
// path hidden behind $(which ...), $(command -v ...), or backticks was not
// visible to absPathInCmdRe.
func extractLookupSubstitutionPaths(command string) []string {
	var paths []string
	var scan func(string)
	scan = func(input string) {
		for i := 0; i < len(input); i++ {
			switch input[i] {
			case '\\':
				i++
			case '\'':
				i = skipSingleQuoted(input, i)
			case '`':
				end := findBacktickEnd(input, i+1)
				if end < 0 {
					continue
				}
				body := input[i+1 : end]
				if path, ok := resolveLookupSubstitution(body); ok {
					paths = append(paths, path)
				}
				scan(body)
				i = end
			case '$':
				if i+1 >= len(input) || input[i+1] != '(' {
					continue
				}
				end := findCommandSubstitutionEnd(input, i+2)
				if end < 0 {
					continue
				}
				body := input[i+2 : end]
				if path, ok := resolveLookupSubstitution(body); ok {
					paths = append(paths, path)
				}
				scan(body)
				i = end
			}
		}
	}
	scan(command)
	return paths
}

func skipSingleQuoted(input string, start int) int {
	for i := start + 1; i < len(input); i++ {
		if input[i] == '\'' {
			return i
		}
	}
	return len(input)
}

func findBacktickEnd(input string, start int) int {
	for i := start; i < len(input); i++ {
		if input[i] == '\\' {
			i++
			continue
		}
		if input[i] == '`' {
			return i
		}
	}
	return -1
}

// findCommandSubstitutionEnd returns the closing parenthesis for a $()
// beginning at start, tracking quotes, escapes, and nested $() pairs.
func findCommandSubstitutionEnd(input string, start int) int {
	depth := 1
	var quote byte
	for i := start; i < len(input); i++ {
		if quote == '\'' {
			if input[i] == '\'' {
				quote = 0
			}
			continue
		}
		if quote == '"' {
			if input[i] == '\\' {
				i++
			} else if input[i] == '"' {
				quote = 0
			} else if input[i] == '$' && i+1 < len(input) && input[i+1] == '(' {
				depth++
				i++
			}
			continue
		}

		switch input[i] {
		case '\\':
			i++
		case '\'':
			quote = '\''
		case '"':
			quote = '"'
		case '$':
			if i+1 < len(input) && input[i+1] == '(' {
				depth++
				i++
			}
		case ')':
			depth--
			if depth == 0 {
				return i
			}
		}
	}
	return -1
}

// resolveLookupSubstitution resolves only commands whose purpose is to
// return an executable path. It refuses dynamic arguments and arbitrary shell
// code so consent analysis never runs model-provided commands.
func resolveLookupSubstitution(body string) (string, bool) {
	fields := strings.Fields(strings.TrimSpace(body))
	if len(fields) == 0 {
		return "", false
	}

	pathValue := os.Getenv("PATH")
	idx := 0
	for idx < len(fields) && isShellAssignment(fields[idx]) {
		assignment := fields[idx]
		if strings.HasPrefix(assignment, "PATH=") {
			pathValue = os.ExpandEnv(strings.TrimPrefix(assignment, "PATH="))
		}
		idx++
	}
	if idx >= len(fields) {
		return "", false
	}

	lookup := fields[idx]
	idx++
	var name string
	switch lookup {
	case "which":
		name = lookupArgument(fields[idx:])
	case "command":
		if idx >= len(fields) || (fields[idx] != "-v" && fields[idx] != "--verbose") {
			return "", false
		}
		name = lookupArgument(fields[idx+1:])
	case "type":
		if idx >= len(fields) || (fields[idx] != "-P" && fields[idx] != "-p") {
			return "", false
		}
		name = lookupArgument(fields[idx+1:])
	default:
		return "", false
	}
	if name == "" || strings.ContainsAny(name, "$`()") {
		return "", false
	}

	if filepath.IsAbs(name) {
		if _, err := os.Stat(name); err == nil {
			return name, true
		}
		return "", false
	}
	path, err := lookPathWithValue(name, pathValue)
	if err != nil {
		return "", false
	}
	return path, true
}

func lookupArgument(fields []string) string {
	var name string
	for _, field := range fields {
		if strings.HasPrefix(field, "-") {
			continue
		}
		if name != "" {
			return ""
		}
		name = strings.Trim(field, "\"'")
	}
	return name
}

func lookPathWithValue(name, pathValue string) (string, error) {
	if pathValue == os.Getenv("PATH") {
		return exec.LookPath(name)
	}
	for _, dir := range strings.Split(pathValue, string(os.PathListSeparator)) {
		if dir == "" {
			dir = "."
		}
		candidate := filepath.Join(dir, name)
		info, err := os.Stat(candidate)
		if err == nil && !info.IsDir() && info.Mode()&0111 != 0 {
			return candidate, nil
		}
	}
	return "", exec.ErrNotFound
}

func isShellAssignment(field string) bool {
	idx := strings.IndexByte(field, '=')
	if idx <= 0 {
		return false
	}
	for i, r := range field[:idx] {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (i == 0 || r < '0' || r > '9') && r != '_' {
			return false
		}
	}
	return true
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

// resolveBashWorkDir validates a requested working_directory against the
// allowed paths, asking for path consent when configured. It returns the
// cleaned absolute directory, or a non-nil error response to send back to
// the model.
func resolveBashWorkDir(workDir string, cfg ToolConfig, toolName string) (string, *fantasy.ToolResponse) {
	errResp := func(msg string) (string, *fantasy.ToolResponse) {
		r := fantasy.NewTextErrorResponse(msg)
		return "", &r
	}

	abs, err := filepath.Abs(workDir)
	if err != nil {
		return errResp(fmt.Sprintf("invalid working_directory: %v", err))
	}
	abs = filepath.Clean(abs)
	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		return errResp("working_directory does not exist or is not a directory")
	}
	if !isPathAllowed(abs, cfg.AllowedPaths) {
		if cfg.PathConsent == nil {
			return errResp("working_directory is outside allowed paths")
		}
		result, suggestion, err := cfg.PathConsent.AskConsent(abs, "workdir", toolName, workDir)
		if err != nil {
			return errResp(fmt.Sprintf("working_directory outside allowed paths and consent failed: %v", err))
		}
		if result == ConsentDenied {
			if suggestion != "" {
				return errResp(fmt.Sprintf("working_directory is outside allowed paths. User suggested '%s'; retry with that directory instead.", suggestion))
			}
			return errResp("working_directory is outside allowed paths — access denied by user")
		}
	}
	return abs, nil
}
