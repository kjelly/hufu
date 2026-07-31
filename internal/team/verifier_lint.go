package team

import (
	"strings"
)

// LintVerifier statically analyses a verification specification for §4.3
// anti-patterns that make the verifier structurally unable to fail.
//
// Both the typed spec and the legacy shell command string are accepted so that
// mixed (migration-era) definitions can be linted in a single call.
//
// Rules:
//   - observation mode verifiers are exempt (they are never success criteria).
//   - Syntax-only analysis: no commands are executed; no filesystem I/O is performed.
//   - When the form is structurally known to be non-asserting, severity is "error".
//   - When the analysis is genuinely undecidable, severity is "warning".
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §4.3
func LintVerifier(spec VerificationSpec, legacyCommand string) []ContractFinding {
	normalized := NormalizeVerificationSpec(spec, legacyCommand, "")

	// Observation verifiers are exempt: they are never a success gate.
	if normalized.Mode == "observation" {
		return nil
	}

	// Typed non-command verifiers (file_exists, file_absent, json_assert) are
	// structurally asserting; they do not need shell-pipeline analysis.
	if normalized.Type != VerifyCommandExit {
		return nil
	}

	command := strings.TrimSpace(normalized.Command)
	if command == "" {
		return nil
	}

	return lintShellCommand(command)
}

// lintShellCommand analyses a raw shell command for §4.3 anti-patterns.
// It operates on the textual representation of the command; no execution occurs.
func lintShellCommand(command string) []ContractFinding {
	var findings []ContractFinding

	findings = append(findings, lintTailSwallowsExitCode(command)...)
	findings = append(findings, lintLastStageIsPrinter(command)...)
	findings = append(findings, lintErrorDiscarded(command)...)
	findings = append(findings, lintCountWithoutCompare(command)...)

	return findings
}

// lintTailSwallowsExitCode detects patterns where the verifier always exits 0
// regardless of assertion outcome:
//   - `<assert> || true`           — rhs is the bare word "true"
//   - `<assert> || echo ...`       — rhs is a terminal echo (no further pipe)
//   - `...; exit 0`                — explicit forced exit code
//
// When the rhs of `||` is a pipeline (e.g. `echo fail | grep -q no-match`),
// the analysis is undecidable at the syntax level, so a warning is emitted
// instead of an error.
//
// Refs: §4.3 anti-pattern table, row 1.
func lintTailSwallowsExitCode(command string) []ContractFinding {
	var findings []ContractFinding

	// Split on semicolons to detect trailing `; exit 0`.
	semicolonParts := splitRespectingQuotes(command, ';')
	lastSemi := strings.TrimSpace(semicolonParts[len(semicolonParts)-1])
	if lastSemi == "exit 0" || lastSemi == "exit\t0" {
		findings = append(findings, ContractFinding{
			Severity: FindingSeverityError,
			Code:     FindingVerifierNotAsserting,
			Field:    "verify",
			Message:  "verifier ends with `; exit 0` which forces exit code 0 regardless of assertion outcome",
			Hint:     "Remove `; exit 0` or use a typed verifier (file_exists, json_assert) that does not rely on shell exit codes.",
		})
	}

	// Detect `|| <rhs>` at the end of the command.
	orIdx := lastOrOperatorIndex(command)
	if orIdx >= 0 {
		// Extract the rhs of the last `||`, e.g. everything after `||`.
		rhs := strings.TrimSpace(command[orIdx+2:])
		rhsLower := strings.ToLower(rhs)

		// `|| true` — structurally always exits 0: error.
		if rhsLower == "true" {
			findings = append(findings, ContractFinding{
				Severity: FindingSeverityError,
				Code:     FindingVerifierNotAsserting,
				Field:    "verify",
				Message:  "verifier uses `|| true` which swallows all failures and always exits 0",
				Hint:     "Remove the `|| true` fallback so assertion failures propagate as non-zero exit codes.",
			})
			return findings
		}

		// `|| echo <text>` where echo is the sole terminal command (no further
		// pipe): structurally exits 0 — error.
		// But `|| echo ... | ...` continues with a pipeline stage, making the
		// final exit code undecidable statically — warning.
		if rhsLower == "echo" || strings.HasPrefix(rhsLower, "echo ") {
			// Check whether the rhs contains a further pipe (not `||`).
			rhsStages := SplitPipelineStages(rhs)
			if len(rhsStages) == 1 {
				// Terminal echo — always exits 0: error.
				findings = append(findings, ContractFinding{
					Severity: FindingSeverityError,
					Code:     FindingVerifierNotAsserting,
					Field:    "verify",
					Message:  "verifier uses `|| echo ...` as a terminal fallback which always exits 0, swallowing the assertion failure",
					Hint:     "Remove the `|| echo` fallback so assertion failures propagate as non-zero exit codes.",
				})
			} else {
				// RHS continues into a pipeline — undecidable statically: warning.
				findings = append(findings, ContractFinding{
					Severity: FindingSeverityWarning,
					Code:     FindingVerifierNotAsserting,
					Field:    "verify",
					Message:  "verifier uses `|| echo ... | ...`; static analysis cannot determine whether the pipeline always exits 0",
					Hint:     "Ensure the final pipeline stage is an asserting command, or use a typed verifier.",
				})
			}
		}
	}

	return findings
}

// lintLastStageIsPrinter detects when the final (or only) stage of a verifier
// command is a pure-output command (echo, cat, printf, tee, print).  These
// commands always exit 0, so any upstream failure is silently discarded.
//
// §4.3 explicitly lists `echo <status>` and `cat <result>` as anti-patterns
// regardless of whether they are standalone or the last stage of a pipeline.
//
// Refs: §4.3 anti-pattern table, row 2.
func lintLastStageIsPrinter(command string) []ContractFinding {
	stages := SplitPipelineStages(command)
	if len(stages) == 0 {
		return nil
	}

	last := strings.TrimSpace(stages[len(stages)-1])
	firstToken := firstShellToken(last)
	firstTokenLower := strings.ToLower(firstToken)

	// Well-known pure-output commands that always exit 0.
	purePrinters := map[string]bool{
		"echo":   true,
		"cat":    true,
		"printf": true,
		"tee":    true,
		"print":  true,
	}

	if !purePrinters[firstTokenLower] {
		return nil
	}

	// Standalone pure-printer (no upstream pipeline): also flagged, because a
	// verifier whose sole command is `echo` or `cat` can never signal failure.
	msg := "verifier command is a pure output command (" + firstToken + "), which always exits 0 regardless of assertion outcome"
	if len(stages) > 1 {
		msg = "last pipeline stage is a pure output command (" + firstToken + "), which always exits 0 and discards upstream failures"
	}
	return []ContractFinding{{
		Severity: FindingSeverityError,
		Code:     FindingVerifierNotAsserting,
		Field:    "verify",
		Message:  msg,
		Hint:     "Move the assertion to be the final stage, or use `json_assert` to check structured output instead.",
	}}
}

// lintErrorDiscarded detects when the last (or only) pipeline stage redirects
// stderr to /dev/null without any subsequent judgment.  This discards failure
// messages; when the assertion itself writes only to stderr (e.g. `test`, many
// shell builtins), the exit code may still propagate — but the diagnostic is
// lost.  §4.3 lists this as a structurally problematic form: error.
//
// Refs: §4.3 anti-pattern table, row 3.
func lintErrorDiscarded(command string) []ContractFinding {
	stages := SplitPipelineStages(command)
	if len(stages) == 0 {
		return nil
	}

	last := strings.TrimSpace(stages[len(stages)-1])
	if containsStderrDiscard(last) {
		return []ContractFinding{{
			Severity: FindingSeverityError,
			Code:     FindingVerifierNotAsserting,
			Field:    "verify",
			Message:  "last pipeline stage redirects stderr to /dev/null, discarding failure messages and potentially masking assertion failures",
			Hint:     "Remove `2>/dev/null` so assertion failures are visible, or capture stderr explicitly.",
		}}
	}

	return nil
}

// lintCountWithoutCompare detects pipelines where `grep -c` is the terminal
// stage.  `grep -c` exits 0 regardless of the count (0 or N); success vs
// failure polarity is entirely context-dependent and cannot be determined
// statically.  §4.3 lists this as a structurally ambiguous form: error.
//
// A standalone `grep -c file` (no upstream pipe) is also ambiguous for the
// same reason — count 0 and count N are both valid exits.
//
// Refs: §4.3 anti-pattern table, row 4.
func lintCountWithoutCompare(command string) []ContractFinding {
	stages := SplitPipelineStages(command)
	if len(stages) == 0 {
		return nil
	}

	last := strings.TrimSpace(stages[len(stages)-1])
	if !isGrepCountStage(last) {
		return nil
	}

	return []ContractFinding{{
		Severity: FindingSeverityError,
		Code:     FindingVerifierNotAsserting,
		Field:    "verify",
		Message:  "`grep -c` as the final/sole pipeline stage always exits 0; success vs failure polarity is ambiguous",
		Hint:     "Replace with `grep -q` (exits 1 when not found) or compare the count explicitly, e.g. `[ $(... | grep -c X) -gt 0 ]`.",
	}}
}

// SplitPipelineStages splits a shell command on unquoted `|` characters,
// skipping `||` (logical-or) operators.  It returns the individual pipeline
// stages as raw strings (whitespace preserved).
//
// This is a best-effort static approximation; it does not parse full shell
// grammar.  Subshells ((...), $(...), `...`) are treated as opaque.
func SplitPipelineStages(command string) []string {
	return splitOnPipeNotOr(command)
}

// splitOnPipeNotOr splits on `|` that is not part of `||`.
func splitOnPipeNotOr(command string) []string {
	var stages []string
	var cur strings.Builder
	inSingle := false
	inDouble := false

	runes := []rune(command)
	for i := 0; i < len(runes); i++ {
		ch := runes[i]

		// Track quoting state.
		switch {
		case ch == '\'' && !inDouble:
			inSingle = !inSingle
		case ch == '"' && !inSingle:
			inDouble = !inDouble
		}

		if inSingle || inDouble {
			cur.WriteRune(ch)
			continue
		}

		// Detect `|` vs `||`.
		if ch == '|' {
			if i+1 < len(runes) && runes[i+1] == '|' {
				// `||` — logical or; keep as-is and skip both characters.
				cur.WriteRune('|')
				cur.WriteRune('|')
				i++
				continue
			}
			// Single `|` — pipeline split.
			stages = append(stages, cur.String())
			cur.Reset()
			continue
		}

		cur.WriteRune(ch)
	}

	if s := cur.String(); s != "" || len(stages) > 0 {
		stages = append(stages, cur.String())
	}
	return stages
}

// splitRespectingQuotes splits s on sep, honouring single- and double-quoted
// strings so that quoted separators are not treated as split points.
func splitRespectingQuotes(s string, sep rune) []string {
	var parts []string
	var cur strings.Builder
	inSingle := false
	inDouble := false

	for _, ch := range s {
		switch {
		case ch == '\'' && !inDouble:
			inSingle = !inSingle
		case ch == '"' && !inSingle:
			inDouble = !inDouble
		}
		if !inSingle && !inDouble && ch == sep {
			parts = append(parts, cur.String())
			cur.Reset()
			continue
		}
		cur.WriteRune(ch)
	}
	parts = append(parts, cur.String())
	return parts
}

// lastOrOperatorIndex returns the rune index of the last unquoted `||` in the
// command (pointing at the first `|`), or -1 if none is found.
func lastOrOperatorIndex(command string) int {
	runes := []rune(command)
	inSingle := false
	inDouble := false
	last := -1

	for i := 0; i < len(runes); i++ {
		ch := runes[i]
		switch {
		case ch == '\'' && !inDouble:
			inSingle = !inSingle
		case ch == '"' && !inSingle:
			inDouble = !inDouble
		}
		if !inSingle && !inDouble && ch == '|' && i+1 < len(runes) && runes[i+1] == '|' {
			last = i
			i++ // skip the second '|'
		}
	}

	return last
}

// firstShellToken returns the first whitespace-delimited token of a stage,
// stripping any leading environment-variable assignments (KEY=val ...).
func firstShellToken(stage string) string {
	fields := strings.Fields(stage)
	for _, f := range fields {
		// Skip env-var assignments like FOO=bar.
		if strings.Contains(f, "=") && !strings.HasPrefix(f, "-") {
			continue
		}
		return f
	}
	return ""
}

// containsStderrDiscard reports whether the stage text contains a stderr
// redirect to /dev/null (`2>/dev/null` or `2> /dev/null`).
func containsStderrDiscard(stage string) bool {
	s := strings.ReplaceAll(stage, "2> /dev/null", "2>/dev/null")
	return strings.Contains(s, "2>/dev/null")
}

// isGrepCountStage reports whether a pipeline stage is `grep -c ...` (with
// `-c` in any flag position).
func isGrepCountStage(stage string) bool {
	fields := strings.Fields(stage)
	if len(fields) == 0 {
		return false
	}
	if strings.ToLower(fields[0]) != "grep" {
		return false
	}
	for _, f := range fields[1:] {
		if strings.HasPrefix(f, "-") && strings.ContainsRune(f, 'c') {
			return true
		}
	}
	return false
}
