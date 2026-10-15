package team

import (
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/tools"
)

// attemptToolDispositions is scoped to exactly one worker attempt. Tool
// handlers append only pre-execution policy decisions, which lets recovery
// prove a retry will not replay an external side effect.
type attemptToolDispositions struct {
	mu    sync.Mutex
	items []ToolExecutionDisposition
}

func (d *attemptToolDispositions) add(item ToolExecutionDisposition) {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.items = append(d.items, item)
	d.mu.Unlock()
}

func (d *attemptToolDispositions) snapshot() []ToolExecutionDisposition {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]ToolExecutionDisposition(nil), d.items...)
}

func newToolDispositionReporter(dispositions *attemptToolDispositions, sideEffect SideEffectClass, runID, todoID string, attempt int) tools.ToolExecutionDispositionReporter {
	return tools.ToolExecutionDispositionReporter(func(disposition tools.ToolExecutionDisposition) {
		retrySafety := RetrySafetyReconcileOnly
		if disposition.Kind == string(ToolExecutionPolicyDenied) && !disposition.Executed && sideEffect == SideEffectNone {
			retrySafety = RetrySafetySafeFreshAttempt
		}
		dispositions.add(ToolExecutionDisposition{
			Kind: ToolExecutionKind(disposition.Kind), Executed: disposition.Executed,
			SideEffect: sideEffect, RetrySafety: retrySafety, ReasonCode: disposition.ReasonCode,
			ToolName: disposition.ToolName, ToolCallID: disposition.ToolCallID, TodoID: todoID,
			RunID: runID, Attempt: attempt,
		})
	})
}

// onlyPolicyDeniedToolCalls reports whether every recorded tool call was
// denied before execution. It intentionally requires IDs for every call, so
// missing telemetry remains fail-closed.
func (d *attemptToolDispositions) onlyPolicyDeniedToolCalls(steps []fantasy.StepResult) bool {
	items := d.snapshot()
	if len(items) == 0 {
		return false
	}
	denied := make(map[string]bool, len(items))
	for _, item := range items {
		if item.Kind == ToolExecutionPolicyDenied && !item.Executed && item.RetrySafety == RetrySafetySafeFreshAttempt && item.ToolCallID != "" {
			denied[item.ToolCallID] = true
		}
	}
	sawCall := false
	for _, step := range steps {
		for _, call := range step.Content.ToolCalls() {
			sawCall = true
			if !denied[call.ToolCallID] {
				return false
			}
		}
	}
	return sawCall
}

// buildPolicyDeniedRetryContext is intentionally LLM-free and contains only
// stable policy facts. It is used for the one clean retry allowed after every
// tool call was denied before execution.
func buildPolicyDeniedRetryContext(dispositions []ToolExecutionDisposition) string {
	var lines []string
	for _, disposition := range dispositions {
		if disposition.Kind != ToolExecutionPolicyDenied || disposition.Executed {
			continue
		}
		line := strings.TrimSpace(disposition.ToolName)
		if reason := strings.TrimSpace(disposition.ReasonCode); reason != "" {
			line += " (" + reason + ")"
		}
		if line != "" {
			lines = append(lines, line)
		}
	}
	return "\n\n## Deterministic Policy Repair\nNo prior tool call executed. The policy denied: " + strings.Join(lines, ", ") + ".\nIf bash is needed, make exactly one read-only inspection command. Merging stderr with `2>&1` or discarding it with `2>/dev/null` is allowed; do not use `>`, `>>`, `tee`, a temporary output file, variable expansion, command substitution, or subshells. Do not repeat the denied syntax or tool. Complete the assigned task, then call submit_result exactly once with a truthful status; if you cannot continue, submit `partial` or `blocked`. This is the only fresh retry for this policy denial.\n"
}

func handoffStateForMissingResult(budgetExhausted bool, dispositions *attemptToolDispositions, steps []fantasy.StepResult, ctxErr error) ResultHandoffState {
	if ctxErr != nil {
		return ResultHandoffCancelled
	}
	if budgetExhausted {
		return ResultHandoffBudgetExhausted
	}
	if dispositions.onlyPolicyDeniedToolCalls(steps) {
		return ResultHandoffMissingAfterSafeDenial
	}
	return ResultHandoffMissingAfterPossibleEffect
}

// readOnlyToolCallFunc classifies one recorded tool call. Recovery gates take
// it explicitly: the static taxonomy is isReadOnlyToolCall, and a Coordinator
// extends it with maintainer-declared MCP observation tools.
type readOnlyToolCallFunc func(toolName, input string) bool

// protocolAttemptWasReadOnly is deliberately conservative. It is used only
// after a worker omitted submit_result to decide whether a single clean model
// retry could replay the task without repeating a side effect. Unknown tools,
// missing calls, and malformed bash input are all unsafe.
func protocolAttemptWasReadOnly(steps []fantasy.StepResult, readOnly readOnlyToolCallFunc) bool {
	sawCall := false
	for _, step := range steps {
		for _, call := range step.Content.ToolCalls() {
			sawCall = true
			if !readOnly(call.ToolName, call.Input) {
				return false
			}
		}
	}
	return sawCall
}

// attemptChangedNothing reports whether every tool call an attempt recorded
// was read-only or refused before it ran. submit_result only reports the
// outcome. An attempt with no recorded tool call, such as one run by an
// external provider, may have changed state out of view, so it does not count.
func attemptChangedNothing(steps []fantasy.StepResult, dispositions *attemptToolDispositions, readOnly readOnlyToolCallFunc) bool {
	refused := dispositions.refusedCallIDs()
	sawCall := false
	for _, step := range steps {
		for _, call := range step.Content.ToolCalls() {
			sawCall = true
			if call.ToolName != submitResultToolName && !readOnly(call.ToolName, call.Input) && !refused[call.ToolCallID] {
				return false
			}
		}
	}
	return sawCall
}

// refusedCallIDs returns the tool calls that a gate refused and that never
// executed.
func (d *attemptToolDispositions) refusedCallIDs() map[string]bool {
	if d == nil {
		return nil
	}
	refused := make(map[string]bool)
	executed := make(map[string]bool)
	for _, item := range d.snapshot() {
		if item.ToolCallID == "" {
			continue
		}
		if item.Executed {
			executed[item.ToolCallID] = true
		} else {
			refused[item.ToolCallID] = true
		}
	}
	for id := range executed {
		delete(refused, id)
	}
	return refused
}

// stalledWithoutToolCall reports whether a turn's last step made no tool
// call. Fantasy's own continuation loop correctly stops as soon as a step
// requests no tools -- it has no way to know a caller's task still requires
// either more tool calls or a submit_result; that is Hufu's protocol, not
// Fantasy's. Callers must gate this on the task actually requiring a result
// (task.Execution.RequiresResult): for any other caller, ending a turn on
// plain text with no further tool call is a normal, complete response, not a
// stall, and must never be nudged.
func stalledWithoutToolCall(steps []fantasy.StepResult) bool {
	if len(steps) == 0 {
		return false
	}
	last := steps[len(steps)-1]
	return len(last.Content.ToolCalls()) == 0
}

// stalledAtOutputLimit reports whether a turn's last step ended because the
// model reached its output token limit. Reasoning tokens count toward that
// limit, so a model deliberating over a large submission can exhaust it with
// neither text nor a tool call. The provider cut the step off; the model did
// not choose to stop, and the recovery prompt must say so: a generic "you
// stopped" prompt invites the model to redo the same deliberation and be cut
// off again.
func stalledAtOutputLimit(steps []fantasy.StepResult) bool {
	return len(steps) > 0 && steps[len(steps)-1].FinishReason == fantasy.FinishReasonLength
}

const (
	stalledContinuationPrompt     = "You stopped without calling a tool or submit_result. If you are not finished, make your next tool call now. If you have gathered enough evidence, call submit_result now. Do not just describe what you plan to do next."
	outputLimitContinuationPrompt = "Your previous response reached the output token limit before it made a tool call, so nothing was submitted; reasoning counts toward that limit. Do not repeat or re-verify your earlier reasoning. If you have gathered enough evidence, call submit_result now with a compact payload: keep reasoning short, state each finding once, shorten narrative fields, and omit optional fields so the complete call fits in one response. If you are not finished, make your next tool call now."
	// protocolRepairOutputLimitNote is appended to a result-only repair whose
	// worker was cut off, since the repair model has the same output limit.
	protocolRepairOutputLimitNote = "\n\n## Output limit\nThe worker's last response reached the output token limit before it called submit_result; reasoning counts toward that limit. Keep reasoning brief: do not re-verify the evidence or deliberate over optional fields. Submit a compact result that states each material finding once, shortens narrative fields, and omits optional outer fields, so the complete submit_result call fits in one response.\n"
)

// stallContinuationPrompt is the same-turn continuation message for a worker
// that ended its turn without a tool call on a task that requires one.
func stallContinuationPrompt(steps []fantasy.StepResult) string {
	if stalledAtOutputLimit(steps) {
		return outputLimitContinuationPrompt
	}
	return stalledContinuationPrompt
}

// protocolRepairInstructions tells a result-only repair what to do. Only a
// repair that has a rejected submission to correct is asked to correct one;
// otherwise the model hunts for a submission that never existed.
func protocolRepairInstructions(hasRejectedSubmission bool) string {
	instructions := "The worker has not supplied an accepted submit_result. Call submit_result exactly once using only the bounded evidence above and the bound result contract. Include a concise summary."
	if hasRejectedSubmission {
		instructions += " Correct the rejected fields while preserving the existing evidence and valid parts of the submission."
	}
	return instructions + " Do NOT execute work, call any other tools, or emit a prose final response.\n"
}

// isReadOnlyToolCall is the narrow capability classification shared by retry
// and coordinator recovery. Unknown tools and malformed bash calls are never
// treated as safe: callers must preserve their existing fail-closed behavior.
func isReadOnlyToolCall(toolName, input string) bool {
	name := strings.TrimSpace(toolName)
	if name == "bash" {
		var args struct {
			Command string `json:"command"`
		}
		return json.Unmarshal([]byte(input), &args) == nil && tools.IsReadOnlyBashCommand(args.Command)
	}
	return tools.IsReadOnlyObservationTool(name)
}

// protocolRepairEvidenceSummary avoids carrying old fantasy tool messages into
// a result-only repair turn. Those messages can bias a tool-calling model
// toward tools that are intentionally absent from the repair agent.
func protocolRepairEvidenceSummary(steps []fantasy.StepResult, transcript *ArtifactRef) string {
	var names []string
	for _, step := range steps {
		for _, call := range step.Content.ToolCalls() {
			if name := strings.TrimSpace(call.ToolName); name != "" {
				names = append(names, name)
			}
		}
	}
	if len(names) == 0 && transcript == nil {
		return "\n\n## Execution Evidence\nNo worker tool calls were recorded. Do not infer that work completed; submit a truthful blocked or partial result.\n"
	}
	var b strings.Builder
	b.WriteString("\n\n## Execution Evidence\n")
	if len(names) > 0 {
		b.WriteString("The prior worker used these tools: ")
		b.WriteString(strings.Join(names, ", "))
		b.WriteString(". Their raw calls are sealed evidence, not tools available in this repair turn.\n")
	}
	if transcript != nil {
		fmt.Fprintf(&b, "Transcript evidence reference: %s.\n", transcript.ID)
	}
	b.WriteString("Only submit_result is available now.\n")
	return b.String()
}
