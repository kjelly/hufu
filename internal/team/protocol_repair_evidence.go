package team

import (
	"fmt"
	"strings"

	"charm.land/fantasy"
	"github.com/kjelly/hufu/internal/utils"
)

const (
	maxRejectedSubmissionRunes = 12000
	maxRejectionReasonRunes    = 2000
)

// rejectedSubmissionOmitted replaces the rejected submission in the repair
// prompt copy that receipts persist. The model gets its own arguments back;
// runtime records keep model-controlled arguments out, as failure summaries
// do.
const rejectedSubmissionOmitted = "\n\n## Last rejected submit_result\n[The worker's rejected submit_result arguments were given to the repair turn and are omitted from this record; the task transcript keeps them.]\n"

// protocolRepairRejectedSubmission renders the worker's last submit_result
// call that the runtime rejected, or "" when there was none. The stream
// callbacks record it in evidence even when the attempt returns no steps, as
// when a submit_result loop aborts the stream; steps are the fallback.
//
// Result-only repair used to see only the worker's final text and the names
// of the tools it called. A worker whose only report was a rejected
// submit_result, typically with a final text of zero bytes, then had its
// findings discarded: the repair turn had nothing to restate and submitted a
// placeholder result with no findings, which the workset accepted.
func protocolRepairRejectedSubmission(evidence *toolCallEvidence, steps []fantasy.StepResult) string {
	input, rejection := "", ""
	if evidence != nil {
		input, rejection = evidence.rejectedSubmitInput, evidence.rejectedSubmitError
	}
	if strings.TrimSpace(input) == "" {
		input, rejection = lastRejectedSubmitResult(steps)
	}
	if strings.TrimSpace(input) == "" {
		return ""
	}
	return fmt.Sprintf("\n\n## Last rejected submit_result\nThe worker already reported this result and the runtime rejected it. Resubmit the same findings, verdict, and assessments with only the rejected part corrected. Do not drop findings, and do not replace them with a claim that evidence is unavailable.\n\n### Rejection\n%s\n\n### Submitted arguments\n%s\n",
		utils.TruncateRunes(utils.RedactSecrets(strings.TrimSpace(rejection)), maxRejectionReasonRunes),
		utils.TruncateRunes(utils.RedactSecrets(input), maxRejectedSubmissionRunes))
}

func lastRejectedSubmitResult(steps []fantasy.StepResult) (input, rejection string) {
	inputs := make(map[string]string)
	for _, step := range steps {
		for _, part := range step.Content {
			if call, ok := fantasy.AsContentType[fantasy.ToolCallContent](part); ok {
				if call.ToolName == submitResultToolName {
					inputs[call.ToolCallID] = call.Input
				}
				continue
			}
			result, ok := fantasy.AsContentType[fantasy.ToolResultContent](part)
			if !ok {
				continue
			}
			submitted, tracked := inputs[result.ToolCallID]
			if !tracked {
				continue
			}
			text, isError := toolResultOutputText(result.Result)
			if isError {
				input, rejection = submitted, text
			} else {
				// An accepted submission supersedes earlier rejections.
				input, rejection = "", ""
			}
		}
	}
	return input, rejection
}

func omitRejectedSubmission(prompt, rejectedSubmission string) string {
	if rejectedSubmission == "" {
		return prompt
	}
	return strings.Replace(prompt, rejectedSubmission, rejectedSubmissionOmitted, 1)
}
