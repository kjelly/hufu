package team

import (
	"fmt"
	"strconv"
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
	return LintVerifierWithMode(spec, legacyCommand, "")
}

// LintVerifierWithMode is like LintVerifier, but accepts an explicit legacyMode.
func LintVerifierWithMode(spec VerificationSpec, legacyCommand, legacyMode string) []ContractFinding {
	normalized := NormalizeVerificationSpec(spec, legacyCommand, legacyMode)
	if normalized.Type == VerifyTaskResultAssert {
		if err := validateVerificationSpec(normalized); err != nil {
			return []ContractFinding{{
				Severity: FindingSeverityError,
				Code:     FindingVerifierInvalid,
				Field:    "verify_spec",
				Message:  err.Error(),
				Hint:     "use RFC 6901 pointers and one supported task_result_assert operator per assertion",
			}}
		}
		return nil
	}

	// Observation verifiers are exempt: they are never a success gate.
	if normalized.Mode == "observation" {
		return nil
	}

	// Typed non-command verifiers (file_exists, file_absent, json_assert,
	// task_result_assert) are structurally asserting; they do not need
	// shell-pipeline analysis.
	if normalized.Type != VerifyCommandExit {
		return nil
	}

	command := strings.TrimSpace(normalized.Command)
	if command == "" {
		return nil
	}

	return lintShellCommand(command)
}

// LintTaskDef statically analyzes a TaskDef's verification spec and legacy command
// for §4.3 anti-patterns.
func LintTaskDef(task TaskDef) []ContractFinding {
	var specArg VerificationSpec
	if task.VerifySpec != nil {
		specArg = *task.VerifySpec
	}
	return LintVerifierWithMode(specArg, task.Verify, task.VerifyMode)
}

// LintTeamContracts statically analyzes team-level acceptance criteria and contracts.
func LintTeamContracts(session *TeamSession) []ContractFinding {
	if session == nil {
		return nil
	}
	var findings []ContractFinding
	if mode, err := ParseGoalMode(session.Config.GoalMode); err == nil && mode == GoalModeOutcome && stringSliceContainsFold(session.Config.ToolsDenied, "finish") {
		findings = append(findings, ContractFinding{
			Severity: FindingSeverityError,
			Code:     FindingCompletionToolDenied,
			Field:    "tools.denied",
			Message:  "outcome mode requires the finish tool so the coordinator can evaluate acceptance",
		})
	}

	accSpec := session.Config.AcceptanceSpec
	if accSpec != nil {
		// 1. Lint every command in accSpec.Commands
		for index, cmd := range accSpec.Commands {
			cmd = strings.TrimSpace(cmd)
			if cmd != "" {
				spec := VerificationSpec{
					Type:    VerifyCommandExit,
					Command: cmd,
				}
				findings = append(findings, scopeContractFindings(fmt.Sprintf("acceptance.commands[%d]", index), LintVerifierWithMode(spec, cmd, ""))...)
			}
		}

		// 2. Lint every VerificationSpec in accSpec.Verifications
		for index, v := range accSpec.Verifications {
			findings = append(findings, scopeContractFindings(fmt.Sprintf("acceptance.verifications[%d]", index), LintVerifierWithMode(v, v.Command, v.Mode))...)
		}

		// 3. Lint every AcceptanceCriterion in accSpec.Criteria
		for index, crit := range accSpec.Criteria {
			findings = append(findings, scopeContractFindings(fmt.Sprintf("acceptance.criteria[%d]", index), LintVerifierWithMode(crit.Verify, crit.Verify.Command, crit.Verify.Mode))...)
		}
	}

	// 4. Preserve legacy Acceptance lint when not already represented in accSpec.Commands
	legacyCmd := strings.TrimSpace(session.Config.Acceptance)
	if legacyCmd != "" {
		alreadyLinted := false
		if accSpec != nil {
			for _, cmd := range accSpec.Commands {
				if strings.TrimSpace(cmd) == legacyCmd {
					alreadyLinted = true
					break
				}
			}
		}
		if !alreadyLinted {
			spec := VerificationSpec{
				Type:    VerifyCommandExit,
				Command: legacyCmd,
			}
			findings = append(findings, scopeContractFindings("acceptance", LintVerifierWithMode(spec, legacyCmd, ""))...)
		}
	}

	for index, task := range session.ContractTasks {
		findings = append(findings, scopeContractFindings(fmt.Sprintf("tasks[%d]", index), LintTaskDef(task))...)
	}
	findings = append(findings, ValidateTeamTaskContracts(session)...)
	findings = append(findings, ValidateTeamPolicyContracts(session)...)

	return findings
}

func stringSliceContainsFold(values []string, want string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), want) {
			return true
		}
	}
	return false
}

// ResolveTeamContractExecutables reports unresolved executables in every
// command-based task and acceptance verifier. workDir must be the runtime
// project directory used to execute verification commands, rather than the
// directory containing team.yaml. It calls the WP-04 resolver without
// executing any command.
func ResolveTeamContractExecutables(session *TeamSession, workDir string) []ContractFinding {
	if session == nil {
		return nil
	}
	var findings []ContractFinding
	appendCommand := func(scope, command string) {
		command = strings.TrimSpace(command)
		if command != "" {
			findings = append(findings, scopeContractFindings(scope, ResolveCommandExecutables(command, workDir))...)
		}
	}
	if spec := session.Config.AcceptanceSpec; spec != nil {
		for index, command := range spec.Commands {
			appendCommand(fmt.Sprintf("acceptance.commands[%d]", index), command)
		}
		for index, verification := range spec.Verifications {
			appendCommand(fmt.Sprintf("acceptance.verifications[%d]", index), commandForExecutableResolution(verification, verification.Command, verification.Mode))
		}
		for index, criterion := range spec.Criteria {
			appendCommand(fmt.Sprintf("acceptance.criteria[%d]", index), commandForExecutableResolution(criterion.Verify, criterion.Verify.Command, criterion.Verify.Mode))
		}
	}
	legacyAcceptance := strings.TrimSpace(session.Config.Acceptance)
	if legacyAcceptance != "" && (session.Config.AcceptanceSpec == nil || !containsCommand(session.Config.AcceptanceSpec.Commands, legacyAcceptance)) {
		appendCommand("acceptance", legacyAcceptance)
	}
	for index, task := range session.ContractTasks {
		var spec VerificationSpec
		if task.VerifySpec != nil {
			spec = *task.VerifySpec
		}
		appendCommand(fmt.Sprintf("tasks[%d]", index), commandForExecutableResolution(spec, task.Verify, task.VerifyMode))
	}
	return findings
}

func commandForExecutableResolution(spec VerificationSpec, legacyCommand, legacyMode string) string {
	normalized := NormalizeVerificationSpec(spec, legacyCommand, legacyMode)
	if normalized.Type != VerifyCommandExit {
		return ""
	}
	return normalized.Command
}

func containsCommand(commands []string, target string) bool {
	for _, command := range commands {
		if strings.TrimSpace(command) == target {
			return true
		}
	}
	return false
}

func scopeContractFindings(scope string, findings []ContractFinding) []ContractFinding {
	for index := range findings {
		if findings[index].Field == "" {
			findings[index].Field = scope
		} else {
			findings[index].Field = scope + "." + findings[index].Field
		}
	}
	return findings
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
//   - `<assert> || <printer>`      — rhs ends with a pure printer (echo, cat, etc.)
//   - `...; exit 0`                — explicit forced exit code
//
// Refs: §4.3 anti-pattern table, row 1.
func lintTailSwallowsExitCode(command string) []ContractFinding {
	var findings []ContractFinding

	// Split on semicolons to detect trailing `; exit 0`.
	semicolonParts := splitRespectingQuotes(command, ';')
	lastSemi := strings.TrimSpace(semicolonParts[len(semicolonParts)-1])
	lastSemiFields := strings.Fields(lastSemi)
	if len(lastSemiFields) == 2 && strings.ToLower(lastSemiFields[0]) == "exit" && lastSemiFields[1] == "0" {
		findings = append(findings, ContractFinding{
			Severity: FindingSeverityError,
			Code:     FindingVerifierNotAsserting,
			Field:    "verify",
			Message:  "verifier ends with `; exit 0` which forces exit code 0 regardless of assertion outcome",
			Hint:     "Remove `; exit 0` or use a typed verifier (file_exists, json_assert) that does not rely on shell exit codes.",
		})
	}

	// Detect `|| <rhs>` in the last semicolon segment.
	orIdx := lastOrOperatorIndex(lastSemi)
	if orIdx >= 0 {
		rhs := strings.TrimSpace(lastSemi[orIdx+2:])
		andParts := splitAndOperators(rhs)
		firstAndPart := strings.TrimSpace(andParts[0])

		rhsStages := SplitPipelineStages(firstAndPart)
		if len(rhsStages) == 0 {
			return findings
		}
		lastRHSStage := strings.TrimSpace(rhsStages[len(rhsStages)-1])
		firstToken := strings.ToLower(firstShellToken(lastRHSStage))

		isFallbackPrinter := isPurePrinter(firstToken) || firstToken == "true"
		if !isFallbackPrinter {
			// The command right after || is not a fallback printer or true (e.g. || false, || grep).
			// When the assertion fails, || false returns non-zero so && true is skipped.
			return findings
		}

		if len(andParts) > 1 {
			severity, isFinding := evaluateAndChain(andParts[1:])
			if !isFinding {
				return findings
			}
			lastAndPart := strings.TrimSpace(andParts[len(andParts)-1])
			if severity == FindingSeverityError {
				findings = append(findings, ContractFinding{
					Severity: FindingSeverityError,
					Code:     FindingVerifierNotAsserting,
					Field:    "verify",
					Message:  "verifier uses `|| ... && " + lastAndPart + "` which forces exit code 0, swallowing assertion failure",
					Hint:     "Remove `&& " + lastAndPart + "` or ensure the fallback control list propagates assertion failures as non-zero exit codes.",
				})
			} else {
				findings = append(findings, ContractFinding{
					Severity: FindingSeverityWarning,
					Code:     FindingVerifierNotAsserting,
					Field:    "verify",
					Message:  "verifier uses `|| ... && ...` control list; static analysis cannot determine if final status asserts failure",
					Hint:     "Ensure the command returns a non-zero exit code on assertion failure.",
				})
			}
			return findings
		}

		if firstToken == "true" {
			findings = append(findings, ContractFinding{
				Severity: FindingSeverityError,
				Code:     FindingVerifierNotAsserting,
				Field:    "verify",
				Message:  "verifier uses `|| true` which swallows all failures and always exits 0",
				Hint:     "Remove the `|| true` fallback so assertion failures propagate as non-zero exit codes.",
			})
		} else if isPurePrinter(firstToken) {
			findings = append(findings, ContractFinding{
				Severity: FindingSeverityError,
				Code:     FindingVerifierNotAsserting,
				Field:    "verify",
				Message:  "verifier uses `|| " + firstToken + " ...` as a terminal fallback which always exits 0, swallowing the assertion failure",
				Hint:     "Remove the `|| " + firstToken + "` fallback so assertion failures propagate as non-zero exit codes.",
			})
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
	andParts := splitAndOperators(last)
	if len(andParts) > 1 {
		firstPart := strings.TrimSpace(andParts[0])
		firstToken := firstShellToken(firstPart)
		if isPurePrinter(firstToken) {
			severity, isFinding := evaluateAndChain(andParts[1:])
			if !isFinding {
				return nil
			}
			lastAndPart := strings.TrimSpace(andParts[len(andParts)-1])
			if severity == FindingSeverityError {
				return []ContractFinding{{
					Severity: FindingSeverityError,
					Code:     FindingVerifierNotAsserting,
					Field:    "verify",
					Message:  "last pipeline stage is a pure output command (" + firstToken + ") followed by `&& " + lastAndPart + "` which forces exit code 0",
					Hint:     "Ensure the verifier pipeline returns a non-zero exit code on assertion failure.",
				}}
			}
			return []ContractFinding{{
				Severity: FindingSeverityWarning,
				Code:     FindingVerifierNotAsserting,
				Field:    "verify",
				Message:  "last pipeline stage is a pure output command (" + firstToken + ") followed by `&& ...`; static analysis cannot determine if final status asserts failure",
				Hint:     "Ensure the verifier pipeline returns a non-zero exit code on assertion failure.",
			}}
		}
		return nil
	}

	firstToken := firstShellToken(last)
	if !isPurePrinter(firstToken) {
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

// splitAndOperators splits s on unquoted `&&` operators.
func splitAndOperators(s string) []string {
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

		if !inSingle && !inDouble && ch == '&' && i+1 < len(runes) && runes[i+1] == '&' {
			parts = append(parts, cur.String())
			cur.Reset()
			i++ // skip second '&'
			continue
		}
		cur.WriteRune(ch)
	}
	parts = append(parts, cur.String())
	return parts
}

// isAssertingFailureCommand reports whether cmd is known to produce a non-zero exit code.
func isAssertingFailureCommand(cmd string) bool {
	cmd = strings.TrimSpace(cmd)
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return false
	}
	token := strings.ToLower(firstShellToken(cmd))
	if token == "false" {
		return true
	}
	if token == "exit" && len(fields) >= 2 {
		code, err := strconv.Atoi(fields[1])
		if err == nil && code != 0 {
			return true
		}
	}
	return false
}

// isAlwaysSuccessCommand reports whether cmd is known to produce a 0 (success) exit code.
func isAlwaysSuccessCommand(cmd string) bool {
	cmd = strings.TrimSpace(cmd)
	fields := strings.Fields(cmd)
	if len(fields) == 0 {
		return false
	}
	token := strings.ToLower(firstShellToken(cmd))
	if token == "true" {
		return true
	}
	if token == "exit" && len(fields) >= 2 {
		code, err := strconv.Atoi(fields[1])
		if err == nil && code == 0 {
			return true
		}
	}
	if isPurePrinter(token) {
		return true
	}
	return false
}

// evaluateAndChain evaluates the sequence of commands in an AND-list following
// a fallback or printer. It returns the finding severity if the chain does not
// assert failure, or false if the chain contains a command that guarantees
// failure (thus asserting failure).
func evaluateAndChain(andParts []string) (string, bool) {
	hasUnknown := false
	for _, part := range andParts {
		cmd := strings.TrimSpace(part)
		if isAssertingFailureCommand(cmd) {
			// A known non-zero command (e.g. false, exit 1) terminates the && chain with non-zero status.
			// The verifier asserts failure. Clean!
			return "", false
		}
		if !isAlwaysSuccessCommand(cmd) {
			hasUnknown = true
		}
	}
	if hasUnknown {
		return FindingSeverityWarning, true
	}
	return FindingSeverityError, true
}

// isPurePrinter reports whether token is a known pure-output command.
func isPurePrinter(token string) bool {
	switch strings.ToLower(token) {
	case "echo", "cat", "printf", "tee", "print":
		return true
	default:
		return false
	}
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
