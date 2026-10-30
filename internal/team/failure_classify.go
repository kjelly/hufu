package team

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/kjelly/hufu/internal/tools"
)

// FailureClassificationInput carries the structured signals used to classify a
// task failure. Unknown live errors default to execution; their wording never
// selects recovery policy. Text fallback is limited to legacy persisted details.
//
// Refs: docs/archive/implementation-plans/generic-task-reliability.md §5, §5.3, WP-05
type FailureClassificationInput struct {
	// Err is the failure error. When nil, the class defaults to
	// FailureExecution (a nil error reaching the classifier is itself a bug,
	// but we fail safe rather than panic).
	Err error
	// ExitCode is the exit code of the process whose failure is being
	// classified, when available. The sentinel -1 means "unknown / not a
	// process exit" (e.g. an agent stream error). Combined with ExitCodeSource,
	// this is the structured signal WP-05 requires: a non-zero verify-command
	// exit code is objective evidence of a verification failure (§5, §5.2)
	// and classifies as FailureVerify regardless of the error message text.
	ExitCode int
	// ExitCodeSource identifies which process produced ExitCode. "verify"
	// means the deliverable verification command; "worker" means the worker
	// agent's own process. Empty means unspecified, in which case ExitCode is
	// not used to override the text path (the evidence is ambiguous).
	ExitCodeSource string
	// ResolveFindings are the pre-dispatch contract findings produced by
	// executable resolution (WP-04) and verifier lint (WP-01). An error
	// severity FindingExecutableUnresolved finding maps to FailureEnvironment;
	// other error findings map to FailureContract.
	ResolveFindings []ContractFinding
	// ContextErr is the parent context's error (if any) at classification
	// time. A context.Canceled caused by an interactive SIGINT maps to
	// FailureCancelled (§5.3); a parent-deadline cancellation stays
	// FailureTimeout.
	ContextErr error
}

// failureClassOverrideError carries an explicit failure class through wrapped
// errors. This is used when the human-readable error necessarily mentions the
// original failure mechanism (for example, a protocol repair that uncovered a
// non-final progress result) but recovery must use the reclassified class.
// Text matching is intentionally not used for this boundary.
type failureClassOverrideError struct {
	class TaskFailureClass
	err   error
}

// toolLoopError is the structured boundary emitted when an agent repeats the
// same rejected tool call. The error deliberately excludes tool arguments:
// model-controlled input must not influence failure classification or leak
// into persisted failure summaries. submit_result loops are protocol failures;
// all other tool loops are execution failures.
type toolLoopError struct {
	agentName        string
	toolName         string
	rejectedAttempts int
	successfulCalls  int
	class            TaskFailureClass
}

func (e *toolLoopError) Error() string {
	if e == nil {
		return ""
	}
	if e.successfulCalls > 0 {
		return fmt.Sprintf("agent %s is stuck in a loop repeating the same successful tool call: %s after %d completed call(s)", e.agentName, e.toolName, e.successfulCalls)
	}
	return fmt.Sprintf("agent %s is stuck in a loop executing the same failing command: %s after %d rejected attempt(s)", e.agentName, e.toolName, e.rejectedAttempts)
}

func (e *toolLoopError) FailureClassOverride() TaskFailureClass {
	if e == nil || e.class == "" {
		return FailureExecution
	}
	return e.class
}

func newToolLoopError(agentName, toolName string, rejectedAttempts int) error {
	class := FailureExecution
	if toolName == submitResultToolName {
		class = FailureProtocol
	}
	return &toolLoopError{
		agentName:        agentName,
		toolName:         toolName,
		rejectedAttempts: rejectedAttempts,
		class:            class,
	}
}

func newSuccessfulToolLoopError(agentName, toolName string, successfulCalls int) error {
	return &toolLoopError{
		agentName:       agentName,
		toolName:        toolName,
		successfulCalls: successfulCalls,
		class:           FailureExecution,
	}
}

func isSubmitResultProtocolLoop(err error) bool {
	loopErr, ok := errors.AsType[*toolLoopError](err)
	return ok && loopErr != nil && loopErr.toolName == submitResultToolName && loopErr.FailureClassOverride() == FailureProtocol
}

func (e *failureClassOverrideError) Error() string {
	if e == nil || e.err == nil {
		return ""
	}
	return e.err.Error()
}

func (e *failureClassOverrideError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.err
}

func (e *failureClassOverrideError) FailureClassOverride() TaskFailureClass {
	if e == nil {
		return ""
	}
	return e.class
}

func withFailureClassOverride(err error, class TaskFailureClass) error {
	if err == nil {
		return nil
	}
	return &failureClassOverrideError{class: class, err: err}
}

// ExitCodeSource constants for FailureClassificationInput.ExitCodeSource.
const (
	ExitCodeSourceVerify = "verify"
	ExitCodeSourceWorker = "worker"
)

// ClassifyTaskFailureStructured classifies live failures from explicit signals.
// Unknown errors remain execution failures. Legacy persisted prose is handled
// separately by classifyTaskFailure.
func ClassifyTaskFailureStructured(in FailureClassificationInput) TaskFailureClass {
	// §5.3: cancelled must be detected and separated from execution before
	// any text matching, and must never be counted as a retry/fingerprint.
	if isCancelledFailure(in.Err, in.ContextErr) {
		return FailureCancelled
	}
	if in.Err == nil {
		return FailureExecution
	}
	var override interface{ FailureClassOverride() TaskFailureClass }
	if errors.As(in.Err, &override) {
		if class := override.FailureClassOverride(); class != "" {
			return class
		}
	}
	// Structured: timeout (deadline exceeded).
	if isTaskTimeout(in.Err) || errors.Is(in.Err, context.DeadlineExceeded) || errors.Is(in.ContextErr, context.DeadlineExceeded) {
		return FailureTimeout
	}
	// Structured: contract preflight findings (WP-01/WP-04). Error-severity
	// executable_unresolved → environment; other error findings → contract.
	if class, ok := classFromResolveFindings(in.ResolveFindings); ok {
		return class
	}
	// Structured: exit code (§5, §5.2). A non-zero verify-command exit code is
	// objective evidence the deliverable failed verification and classifies
	// as FailureVerify, taking precedence over the text fallback. This is the
	// evidence-first policy WP-05 requires: the verifier's own exit status
	// wins over any wrapper message text. ExitCode == 0 or unknown (-1), or
	// an unspecified source, does not override the text path (ambiguous).
	if class, ok := classFromExitCode(in.ExitCode, in.ExitCodeSource); ok {
		return class
	}
	// An unclassified live error is an execution failure. Its prose must not
	// select recovery policy. Historical records use classifyTaskFailure.
	return FailureExecution
}

// classFromExitCode maps a process exit code to a failure class when the
// evidence is unambiguous. A non-zero exit code from the verify command is a
// verification failure (§5, §5.2); a zero or unknown exit code, or an
// unspecified source, is not used to override the text path.
func classFromExitCode(exitCode int, source string) (TaskFailureClass, bool) {
	if source == ExitCodeSourceVerify && exitCode != 0 && exitCode != -1 {
		return FailureVerify, true
	}
	return "", false
}

// isCancelledFailure reports whether the failure represents a user- or
// context-initiated cancellation. A context.Canceled caused by an interactive
// SIGINT is a user cancel; a context.Canceled with no interactive-abort flag is
// a parent-context cancel. Both map to FailureCancelled (§5.3).
func isCancelledFailure(err, ctxErr error) bool {
	if errors.Is(err, context.Canceled) || errors.Is(ctxErr, context.Canceled) {
		return true
	}
	// Some agent errors wrap the cancellation as a string; detect the
	// interactive-abort marker so a wrapped "context canceled" still classifies
	// as cancelled when the user hit Ctrl+C.
	if tools.IsInteractiveAbortRequested() && (isCanceledText(err) || isCanceledText(ctxErr)) {
		return true
	}
	return false
}

func isCanceledText(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(strings.ToLower(err.Error()), "context canceled")
}

// classFromResolveFindings maps error-severity contract findings to a failure
// class. executable_unresolved → environment; any other error finding →
// contract. Warning/info findings do not override the structured path (they
// are reported but do not classify the failure).
func classFromResolveFindings(findings []ContractFinding) (TaskFailureClass, bool) {
	if len(findings) == 0 {
		return "", false
	}
	hasError := false
	for _, f := range findings {
		if f.Severity != FindingSeverityError {
			continue
		}
		hasError = true
		if f.Code == FindingExecutableUnresolved {
			return FailureEnvironment, true
		}
	}
	if hasError {
		return FailureContract, true
	}
	return "", false
}

// classFromFailureDetailSource reads the stable source field in persisted
// failure headers. Diagnostic payloads and partial label matches are ignored.
func classFromFailureDetailSource(err error) (TaskFailureClass, bool) {
	if err == nil {
		return "", false
	}
	// Only runtime header fields are metadata. Never interpret source-like
	// text inside the human-readable error payload as a control signal.
	for field := range strings.SplitSeq(err.Error(), " | ") {
		field = strings.TrimSpace(field)
		if strings.HasPrefix(field, "current=") || strings.HasPrefix(field, "last_tool=") {
			continue
		}
		switch field {
		case "source=environment":
			return FailureEnvironment, true
		case "source=cancelled", "source=sigint", "source=context_canceled":
			return FailureCancelled, true
		case "source=contract":
			return FailureContract, true
		default:
			return "", false
		}
	}
	return "", false
}

// classifyTaskFailureByText is the legacy text-matching classifier, retained as
// the fallback path for errors that carry no structured metadata. It must not
// classify cancellations (those are intercepted before this path).
//
// Refs: docs/archive/implementation-plans/generic-task-reliability.md §5, WP-02, WP-05
func classifyTaskFailureByText(err error) TaskFailureClass {
	if err == nil {
		return FailureExecution
	}
	if isTaskTimeout(err) || errors.Is(err, context.DeadlineExceeded) {
		return FailureTimeout
	}
	text := err.Error()
	if _, payload, ok := strings.Cut(text, " | error="); ok {
		text = payload
	}
	msg := strings.ToLower(text)
	if strings.Contains(msg, "contract preflight failed") {
		return FailureContract
	}
	if strings.Contains(msg, "environment") || hasEnvironmentFailureSignal(err.Error()) {
		return FailureEnvironment
	}
	if strings.Contains(msg, "protocol") || strings.Contains(msg, "empty output") {
		return FailureProtocol
	}
	if strings.Contains(msg, "verification") || strings.Contains(msg, "deliverable") {
		return FailureVerify
	}
	if strings.Contains(msg, "policy") || strings.Contains(msg, "blocked") {
		return FailurePolicy
	}
	return FailureExecution
}

// classifyTaskFailure supports historical persisted details without typed
// classification. Live execution must use ClassifyTaskFailureStructured.
func classifyTaskFailure(err error) TaskFailureClass {
	class := ClassifyTaskFailureStructured(FailureClassificationInput{Err: err})
	if class != FailureExecution {
		return class
	}
	if override, ok := errors.AsType[interface {
		error
		FailureClassOverride() TaskFailureClass
	}](err); ok && override.FailureClassOverride() != "" {
		return class
	}
	if class, ok := classFromFailureDetailSource(err); ok {
		return class
	}
	return classifyTaskFailureByText(err)
}

// IsCancelledClass reports whether a failure class represents a cancellation
// (§5.3). Cancelled failures are excluded from retry, fingerprint and
// anti-thrashing statistics.
func IsCancelledClass(class TaskFailureClass) bool {
	return class == FailureCancelled
}

// verifyResultForTodo reads the most recent verification result for a todo
// item, returning nil when no verification has run or the result is absent.
// The caller is responsible for clearing it after classification so the next
// attempt does not read a stale result (§5, §5.1).
func verifyResultForTodo(c *Coordinator, todoID string) *VerificationResult {
	if c == nil || c.taskTracker == nil || todoID == "" {
		return nil
	}
	for _, item := range c.taskTracker.TodoList().Items() {
		if item != nil && item.ID == todoID && item.VerifyResult != nil {
			return item.VerifyResult
		}
	}
	return nil
}

// receiptIDsForTodo returns the execution receipt IDs bound to a todo's typed
// result, so a failed verification can carry authoritative receipt evidence.
func receiptIDsForTodo(c *Coordinator, todoID string) []string {
	if c == nil || c.taskTracker == nil || todoID == "" {
		return nil
	}
	for _, item := range c.taskTracker.TodoList().Items() {
		if item != nil && item.ID == todoID && item.TypedResult != nil && len(item.TypedResult.ReceiptIDs) > 0 {
			return append([]string(nil), item.TypedResult.ReceiptIDs...)
		}
	}
	return nil
}

// artifactIDsForTodo returns the opaque artifact IDs bound to a todo's typed
// result, so a failed verification of a produced artifact still carries the
// authoritative artifact evidence ref. Only opaque IDs are preserved; paths
// are never used as artifact evidence.
func artifactIDsForTodo(c *Coordinator, todoID string) []string {
	if c == nil || c.taskTracker == nil || todoID == "" {
		return nil
	}
	for _, item := range c.taskTracker.TodoList().Items() {
		if item != nil && item.ID == todoID && item.TypedResult != nil && len(item.TypedResult.Artifacts) > 0 {
			ids := make([]string, 0, len(item.TypedResult.Artifacts))
			for _, artifact := range item.TypedResult.Artifacts {
				if strings.TrimSpace(artifact.ID) != "" {
					ids = append(ids, artifact.ID)
				}
			}
			if len(ids) > 0 {
				return ids
			}
		}
	}
	return nil
}

// exitCodeFromVerifyResult extracts the exit code from a verification result,
// returning -1 (unknown) when the result is nil.
func exitCodeFromVerifyResult(vr *VerificationResult) int {
	if vr == nil {
		return -1
	}
	return vr.ExitCode
}

// environmentFindingsFromVerifyResult inspects a verification result for
// environment-failure evidence (command not found, executable unresolved) and
// returns a ContractFinding with FindingExecutableUnresolved severity when
// detected. This ensures environment evidence takes precedence over the exit
// code in the structured classifier (§5.1): a verify command that fails
// because the executable is missing is an environment failure, not a
// verification failure, regardless of its exit code.
func environmentFindingsFromVerifyResult(vr *VerificationResult) []ContractFinding {
	if vr == nil {
		return nil
	}
	if hasEnvironmentFailureSignal(vr.Stderr) || hasEnvironmentFailureSignal(vr.Stdout) {
		return []ContractFinding{{
			Severity: FindingSeverityError,
			Code:     FindingExecutableUnresolved,
			Field:    "verify",
			Message:  "verify command reported an environment failure (command not found or executable unresolved)",
		}}
	}
	return nil
}

// hasEnvironmentFailureSignal reports whether the given output text contains
// an unambiguous shell/executable-resolution diagnostic. Per §5.1, this must
// distinguish "the shell could not find the command" from "a tool reported an
// absent input/artifact" — the latter is an ordinary verification failure
// (FailureVerify), not an environment failure. Only canonical
// executable-resolution diagnostics are matched; generic phrases like
// "not found" or "no such file or directory" are deliberately excluded
// because they appear in ordinary verifier output (e.g.
// "grep: report.json: No such file or directory" when a deliverable is
// missing).
func hasEnvironmentFailureSignal(output string) bool {
	if output == "" {
		return false
	}
	lower := strings.ToLower(output)
	// "command not found" is the exact diagnostic bash/dash/zsh print when
	// an executable is not in PATH. It does not appear in ordinary
	// missing-artifact verifier output.
	if strings.Contains(lower, "command not found") {
		return true
	}
	// "executable file not found" is Go's / the OS's exec diagnostic for a
	// missing executable. Distinct from a tool reporting an absent file.
	if strings.Contains(lower, "executable file not found") {
		return true
	}
	// "executable unresolved" is hufu's own finding code phrasing.
	if strings.Contains(lower, "executable unresolved") {
		return true
	}
	return false
}

// attachVerifyResultToReceipt attaches a verification result to the
// ExecutionReceipt for the given run ID and attempt number, so the
// verification evidence (command, exit code, stdout, stderr) is retained
// per-attempt for forensics even after the todo-wide VerifyResult slot is
// cleared for the next retry (§5, §9 evidence retention). The receipt is
// identified by (runID, taskID, attempt) — matching on run ID is required so
// a crash-resumed run does not overwrite a prior run's receipt that shares
// the attempt number. If no receipt exists for that (runID, attempt) the call
// is a no-op.
func (c *Coordinator) attachVerifyResultToReceipt(runID, todoID string, attempt int, vr *VerificationResult) {
	if c == nil || c.taskTracker == nil || runID == "" || todoID == "" || vr == nil || attempt < 1 {
		return
	}
	c.taskTracker.TodoList().UpdateReceiptVerifyResult(runID, todoID, attempt, vr)
}
