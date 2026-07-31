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
				rawToken := firstShellToken(and)
				tok := cleanExecutableToken(rawToken)
				if tok != "" && !seen[tok] {
					seen[tok] = true
					tokens = append(tokens, tok)
				}
			}
		}
	}

	return tokens
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
	case "echo", "test", "[", "]", "exit", "cd", "set", "unset", "export",
		"alias", "unalias", "read", "eval", "trap", "shift", "true", "false",
		"return", "exec", "type", "pwd", "builtin", "command", "printf",
		"source", ".", ":", "local", "declare", "readonly", "typeset",
		"getopts", "ulimit", "umask", "wait", "kill", "jobs", "bg", "fg":
		return true
	default:
		return false
	}
}
