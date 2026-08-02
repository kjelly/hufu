package team

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

// ResolveStageExecutables inspects each pipeline stage in stages and attempts
// to resolve the first executable token via PATH or project-local lookup.
//
// Returns a slice of ContractFinding for any stage whose executable cannot be
// resolved. If a bare executable name is not found in PATH but exists as a
// file under workDir, an executable_unresolved finding is produced with a hint
// suggesting an explicit relative path (e.g. ./tool).
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §4.1, WP-04
func ResolveStageExecutables(stages []string, workDir string) []ContractFinding {
	var findings []ContractFinding

	for _, stage := range stages {
		stageSeen := make(map[string]bool)
		tokens := extractStageExecutableTokens(stage)
		for _, tok := range tokens {
			if tok == "" || stageSeen[tok] {
				continue
			}
			stageSeen[tok] = true

			if isShellBuiltin(tok) {
				continue
			}

			if finding, unresolved := resolveSingleExecutable(tok, workDir); unresolved {
				findings = append(findings, finding)
			}
		}
	}

	return findings
}

// ResolveCommandExecutables splits command into pipeline stages and resolves
// the executable tokens across all stages.
func ResolveCommandExecutables(command, workDir string) []ContractFinding {
	stages := SplitPipelineStages(command)
	return ResolveStageExecutables(stages, workDir)
}

// formatUnresolvedExecutableFindings turns resolver findings into bounded
// verifier evidence while preserving actionable explicit-path hints.
func formatUnresolvedExecutableFindings(findings []ContractFinding) string {
	var parts []string
	for _, finding := range findings {
		part := finding.Message
		if finding.Hint != "" {
			part += "; hint: " + finding.Hint
		}
		if part != "" {
			parts = append(parts, part)
		}
	}
	if len(parts) == 0 {
		return "verify executable unresolved: one or more pipeline stages could not be resolved"
	}
	return "verify executable unresolved: " + strings.Join(parts, " | ")
}

// resolveSingleExecutable checks if tok can be resolved.
// Returns (finding, true) if unresolved, or (ContractFinding{}, false) if resolved.
func resolveSingleExecutable(tok, workDir string) (ContractFinding, bool) {
	if strings.Contains(tok, "/") {
		// Path with directory component: absolute or explicit relative.
		targetPath := tok
		if !filepath.IsAbs(tok) {
			if workDir != "" {
				targetPath = filepath.Join(workDir, tok)
			}
		}
		info, err := os.Stat(targetPath)
		if err != nil || info.IsDir() || !isExecutableFile(info) {
			msg := fmt.Sprintf("executable %q not found at path %q", tok, targetPath)
			hint := fmt.Sprintf("Verify that file %q exists and is accessible.", targetPath)
			if filepath.IsAbs(tok) {
				msg = fmt.Sprintf("executable %q not found or not accessible at absolute path", tok)
			}
			if err == nil && !info.IsDir() && !isExecutableFile(info) {
				msg = fmt.Sprintf("file %q at path %q is not executable (permission denied)", tok, targetPath)
				hint = fmt.Sprintf("Grant execute permission (chmod +x %s) or verify file permissions.", targetPath)
			}
			return ContractFinding{
				Severity: FindingSeverityError,
				Code:     FindingExecutableUnresolved,
				Field:    "execution",
				Message:  msg,
				Hint:     hint,
			}, true
		}
		return ContractFinding{}, false
	}

	// Bare command name: look up in PATH first.
	if _, err := exec.LookPath(tok); err == nil {
		return ContractFinding{}, false
	}

	// Not in PATH: check if project-local file exists under workDir.
	if workDir != "" {
		localPath := filepath.Join(workDir, tok)
		if info, err := os.Stat(localPath); err == nil && !info.IsDir() {
			hint := fmt.Sprintf("A project-local executable was found at %s. Consider using an explicit relative path (e.g. ./%s) instead.", localPath, tok)
			if !isExecutableFile(info) {
				hint = fmt.Sprintf("A project-local file was found at %s, but it is not executable. Grant execute permission (chmod +x %s) and consider using an explicit relative path (e.g. ./%s).", localPath, localPath, tok)
			}
			return ContractFinding{
				Severity: FindingSeverityError,
				Code:     FindingExecutableUnresolved,
				Field:    "execution",
				Message:  fmt.Sprintf("executable %q not found in PATH", tok),
				Hint:     hint,
			}, true
		}
	}

	return ContractFinding{
		Severity: FindingSeverityError,
		Code:     FindingExecutableUnresolved,
		Field:    "execution",
		Message:  fmt.Sprintf("executable %q not found in PATH", tok),
		Hint:     fmt.Sprintf("Ensure %q is installed in PATH or specify an explicit relative path.", tok),
	}, true
}

// isExecutableFile reports whether info is a regular file with execute permissions.
func isExecutableFile(info os.FileInfo) bool {
	if !info.Mode().IsRegular() {
		return false
	}
	if runtime.GOOS == "windows" {
		return true
	}
	return info.Mode().Perm()&0111 != 0
}

// extractStageExecutableTokens extracts all command executable tokens from a stage.
func extractStageExecutableTokens(stage string) []string {
	var tokens []string
	seen := make(map[string]bool)

	semiParts := splitRespectingQuotes(stage, ';')
	for _, semi := range semiParts {
		orParts := splitOrOperators(semi)
		for _, or := range orParts {
			andParts := splitAndOperators(or)
			for _, and := range andParts {
				for _, rawToken := range executableTokensAfterShellPrefixes(and) {
					tok := cleanExecutableToken(rawToken)
					if tok != "" && !seen[tok] {
						seen[tok] = true
						tokens = append(tokens, tok)
					}
				}
			}
		}
	}

	return tokens
}

// executableTokensAfterShellPrefixes returns the command token for a shell
// fragment after skipping leading assignments, redirections, and control
// prefixes. It is intentionally conservative: this is command-resolution
// preflight, not a full shell parser. In particular, `! missing`, `if test`,
// and `then echo` must continue past the prefix, while a `for`/`case` header
// has no command token of its own and is skipped until its body fragment.
func executableTokensAfterShellPrefixes(fragment string) []string {
	trimmed := strings.TrimSpace(fragment)
	lower := strings.ToLower(trimmed)
	if strings.HasPrefix(lower, "case ") || strings.HasPrefix(lower, "case\t") {
		// A case header has no executable of its own, but the first pattern
		// body may already be in this fragment when the command is written on
		// one line. Inspect the text after its pattern delimiter instead of
		// discarding the whole compound command.
		if body := caseBodyAfterPattern(trimmed); body != "" {
			return executableTokensAfterShellPrefixes(body)
		}
		return nil
	}

	fields := strings.Fields(fragment)
	for i, field := range fields {
		if i == 0 {
			// SplitPipelineStages/splitRespectingQuotes may leave a case
			// pattern fragment such as `x) missing-command` here. The
			// pattern is syntax, while the suffix is the command to resolve.
			if strings.ContainsRune(field, ')') {
				trimmedFragment := strings.TrimSpace(fragment)
				if body := caseBodyAfterPattern(trimmedFragment); body != "" {
					return executableTokensAfterShellPrefixes(body)
				}
			}
		}
		if i == 0 && isShellAssignment(field) {
			continue
		}
		if isShellRedirection(field) {
			continue
		}
		word := strings.ToLower(cleanExecutableToken(field))
		switch word {
		case "!", "if", "then", "else", "elif", "while", "until", "do", "done", "fi", "time":
			continue
		case "for", "case", "esac", "function", "select":
			return nil
		default:
			return []string{field}
		}
	}
	return nil
}

// caseBodyAfterPattern returns command text after the case-pattern delimiter.
// The delimiter search tracks shell quotes and command substitutions so a `)`
// inside `$(...)` (including one in a quoted case word) is not mistaken for
// the end of the case pattern.
func caseBodyAfterPattern(fragment string) string {
	idx := casePatternDelimiter(fragment)
	if idx < 0 {
		idx = casePatternFragmentDelimiter(fragment)
	}
	if idx < 0 || idx+1 >= len(fragment) {
		return ""
	}
	return strings.TrimSpace(fragment[idx+1:])
}

type shellScanFrame struct {
	quote      byte
	escaped    bool
	parenDepth int
}

// casePatternDelimiter finds the first case-pattern `)` after the shell's
// `in` keyword. This is deliberately a small lexical scanner rather than a
// full shell parser: it only needs to avoid treating quoted text and nested
// command substitutions as case syntax while preserving the existing
// conservative executable preflight.
func casePatternDelimiter(fragment string) int {
	return findCasePatternDelimiter(fragment, true)
}

// casePatternFragmentDelimiter finds a delimiter in a fragment that starts
// at a case pattern (for example `*) false`) after semicolon splitting has
// separated it from the original `case ... in` header.
func casePatternFragmentDelimiter(fragment string) int {
	return findCasePatternDelimiter(fragment, false)
}

func findCasePatternDelimiter(fragment string, requireIn bool) int {
	frames := []shellScanFrame{{}}
	foundIn := false

	for i := 0; i < len(fragment); i++ {
		frame := &frames[len(frames)-1]
		ch := fragment[i]

		if frame.escaped {
			frame.escaped = false
			continue
		}
		if frame.quote == '\'' {
			if ch == '\'' {
				frame.quote = 0
			}
			continue
		}
		if ch == '\\' {
			frame.escaped = true
			continue
		}

		if frame.quote == '"' {
			if ch == '"' {
				frame.quote = 0
				continue
			}
			if ch == '$' && i+1 < len(fragment) && fragment[i+1] == '(' {
				frames = append(frames, shellScanFrame{parenDepth: 1})
				i++
			}
			continue
		}

		if ch == '\'' {
			frame.quote = '\''
			continue
		}
		if ch == '"' {
			frame.quote = '"'
			continue
		}
		if ch == '$' && i+1 < len(fragment) && fragment[i+1] == '(' {
			frames = append(frames, shellScanFrame{parenDepth: 1})
			i++
			continue
		}

		if len(frames) > 1 {
			if ch == '(' {
				frame.parenDepth++
				continue
			}
			if ch == ')' {
				frame.parenDepth--
				if frame.parenDepth == 0 {
					frames = frames[:len(frames)-1]
				}
			}
			continue
		}

		if !foundIn && shellWordAt(fragment, i, "in") {
			foundIn = true
			i++
			continue
		}
		if ch == ')' && (foundIn || !requireIn) {
			return i
		}
	}

	return -1
}

func shellWordAt(s string, offset int, word string) bool {
	if offset < 0 || offset+len(word) > len(s) || s[offset:offset+len(word)] != word {
		return false
	}
	return (offset == 0 || !isShellWordChar(s[offset-1])) &&
		(offset+len(word) == len(s) || !isShellWordChar(s[offset+len(word)]))
}

func isShellWordChar(ch byte) bool {
	return (ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') ||
		(ch >= '0' && ch <= '9') || ch == '_'
}

func isShellAssignment(field string) bool {
	if field == "" || strings.HasPrefix(field, "-") {
		return false
	}
	idx := strings.IndexByte(field, '=')
	return idx > 0 && !strings.ContainsAny(field[:idx], "/\\")
}

func isShellRedirection(field string) bool {
	return strings.HasPrefix(field, "<") || strings.HasPrefix(field, ">") ||
		strings.HasPrefix(field, "&>") || strings.HasPrefix(field, "<&") ||
		strings.HasPrefix(field, ">&")
}

// splitOrOperators splits s on unquoted `||` operators.
func splitOrOperators(s string) []string {
	var parts []string
	var cur strings.Builder
	inSingle := false
	inDouble := false
	runes := []rune(s)

	for i := 0; i < len(runes); i++ {
		ch := runes[i]
		switch {
		case ch == '\'' && !inDouble:
			inSingle = !inSingle
		case ch == '"' && !inSingle:
			inDouble = !inDouble
		}

		if !inSingle && !inDouble && ch == '|' && i+1 < len(runes) && runes[i+1] == '|' {
			parts = append(parts, cur.String())
			cur.Reset()
			i++ // skip second '|'
			continue
		}
		cur.WriteRune(ch)
	}
	parts = append(parts, cur.String())
	return parts
}

// cleanExecutableToken strips outer quotes and leading/trailing punctuation if present.
func cleanExecutableToken(tok string) string {
	tok = strings.TrimSpace(tok)
	if (strings.HasPrefix(tok, "'") && strings.HasSuffix(tok, "'")) ||
		(strings.HasPrefix(tok, "\"") && strings.HasSuffix(tok, "\"")) {
		if len(tok) >= 2 {
			tok = tok[1 : len(tok)-1]
		}
	}
	return strings.TrimSpace(tok)
}

// isShellBuiltin reports whether tok is a standard shell builtin or keyword.
func isShellBuiltin(tok string) bool {
	switch strings.ToLower(tok) {
	case "echo", "test", "[", "]", "[[", "]]", "exit", "cd", "set", "unset", "export",
		"alias", "unalias", "read", "eval", "trap", "shift", "true", "false",
		"return", "exec", "type", "pwd", "builtin", "command", "printf",
		"source", ".", ":", "local", "declare", "readonly", "typeset",
		"getopts", "ulimit", "umask", "wait", "kill", "jobs", "bg", "fg":
		return true
	default:
		return false
	}
}
