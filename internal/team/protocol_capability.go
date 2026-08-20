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
	return "\n\n## Deterministic Policy Repair\nNo prior tool call executed. The policy denied: " + strings.Join(lines, ", ") + ".\nIf bash is needed, make exactly one inspection command with no redirect or shell expansion. Do not use `2>&1`, `>`, `>>`, a temporary output file, command substitution, or control syntax. Do not repeat the denied syntax or tool. Complete the assigned task, then call submit_result exactly once with a truthful status; if you cannot continue, submit `partial` or `blocked`. This is the only fresh retry for this policy denial.\n"
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

// protocolAttemptWasReadOnly is deliberately conservative. It is used only
// after a worker omitted submit_result to decide whether a single clean model
// retry could replay the task without repeating a side effect. Unknown tools,
// missing calls, and malformed bash input are all unsafe.
func protocolAttemptWasReadOnly(steps []fantasy.StepResult) bool {
	sawCall := false
	for _, step := range steps {
		for _, call := range step.Content.ToolCalls() {
			sawCall = true
			if !isReadOnlyToolCall(call.ToolName, call.Input) {
				return false
			}
		}
	}
	return sawCall
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
