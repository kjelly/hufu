package team

import (
	"charm.land/fantasy"
)

// submitResultToolName is the name of the coordinator tool that workers call
// to submit a structured task result. Used as a constant so the repair
// failure classifier can identify submit_result calls in the repair turn's
// step history without hardcoding string literals at call sites.
const submitResultToolName = "submit_result"

// classifyRepairFailure determines the §7 sub-reason for a failed protocol
// repair attempt from the evidence produced by the repair turn.
//
// Inputs:
//   - steps: the StepResult slice returned by the repair agent run. When the
//     repair agent went through the real tool execution path, its steps
//     carry ToolCallContent and ToolResultContent parts that identify
//     whether submit_result was invoked and whether it returned an error.
//   - typedRes: the TaskResult stored via submit_result (nil when the tool was
//     never successfully invoked, e.g. invalid JSON, missing summary, or the
//     tool was not called at all).
//
// Returns:
//   - reason: one of RepairFailureNoToolCall, RepairFailureInvalidSchema, or
//     RepairFailureProgressNotFinal. Empty when the repair succeeded (no
//     failure to classify).
//   - reclassifyExecution: true when reason == RepairFailureProgressNotFinal,
//     signalling the caller must treat the task as an execution failure
//     (FailureExecution) rather than a protocol failure and must NOT count
//     it toward protocol repair statistics (§7).
//
// The function is a pure, side-effect-free classification of evidence; it
// does not inspect business semantics, only structural signals:
//  1. Was submit_result called at all? (steps or typedRes.Source == "submitted")
//  2. If called, did it return an error? (tool result is_error, or no stored result)
//  3. If it stored a result, is the status a terminal "success" or a
//     progress update (partial/failed/blocked)?
//
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §7
func classifyRepairFailure(steps []fantasy.StepResult, typedRes *TaskResult) (reason RepairFailureReason, reclassifyExecution bool) {
	// A successful repair produces a valid submitted result; nothing to classify.
	if typedRes != nil && typedRes.Source == "submitted" && validateSubmittedTaskResult(typedRes) == nil {
		return "", false
	}

	calledSubmitResult, toolReturnedError := scanRepairStepsForSubmitResult(steps)

	// Determine whether submit_result was invoked, combining the step scan
	// (authoritative in the production path) with the stored-result signal
	// (fallback for test seams that bypass tool execution).
	submitCalled := calledSubmitResult || (typedRes != nil && typedRes.Source == "submitted")

	if !submitCalled {
		// The repair turn did not invoke submit_result at all.
		return RepairFailureNoToolCall, false
	}

	// submit_result was invoked but did not yield a valid stored result.
	if typedRes == nil || typedRes.Source != "submitted" {
		// The tool ran but returned an error (invalid JSON, missing summary,
		// invalid status) so storeSubmittedTaskResult was never called.
		// When we have no step evidence of an error either, fall back to
		// invalid_schema as the safer classification (the tool was called but
		// did not produce an acceptable schema).
		_ = toolReturnedError // used only for forensics; classification is the same
		return RepairFailureInvalidSchema, false
	}

	// A result was stored. A non-success status means the worker reported a
	// progress update (partial/failed/blocked) rather than a final outcome.
	// Per §7 this is reclassified as an execution failure, not a protocol
	// failure, and must not count toward protocol repair statistics.
	switch typedRes.Status {
	case "partial", "failed", "blocked":
		return RepairFailureProgressNotFinal, true
	default:
		// Submitted with an unrecognised status — treat as a schema problem.
		return RepairFailureInvalidSchema, false
	}
}

// scanRepairStepsForSubmitResult walks the repair turn's step history looking
// for submit_result tool calls and their results.
//
// Returns:
//   - called: true if at least one ToolCallContent with ToolName ==
//     submitResultToolName was found.
//   - resultError: true if the corresponding ToolResultContent is an error
//     response (fantasy.ToolResultOutputContentError), indicating the tool
//     rejected the submitted arguments (invalid schema).
//
// When the repair agent is a test seam that returns empty steps, both return
// values are false and the caller falls back to the typedRes signal.
func scanRepairStepsForSubmitResult(steps []fantasy.StepResult) (called, resultError bool) {
	for _, step := range steps {
		for _, tc := range step.Content.ToolCalls() {
			if tc.ToolName == submitResultToolName {
				called = true
			}
		}
		for _, tr := range step.Content.ToolResults() {
			if tr.ToolName != submitResultToolName {
				continue
			}
			called = true
			if _, isErr := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](tr.Result); isErr {
				resultError = true
			}
		}
	}
	return called, resultError
}
