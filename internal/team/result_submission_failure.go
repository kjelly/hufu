package team

import (
	"encoding/json/v2"
	"errors"

	"charm.land/fantasy"
)

// Client metadata is authored by the runtime and survives the tool execution
// boundary. Never infer retry eligibility from a model's prose or error text.
type resultSubmissionFailure struct {
	Version int                 `json:"version"`
	Reason  RepairFailureReason `json:"submission_failure"`
}

func tagSubmissionFailure(response fantasy.ToolResponse, reason RepairFailureReason) fantasy.ToolResponse {
	if !response.IsError || response.Metadata != "" {
		return response
	}
	return fantasy.WithResponseMetadata(response, resultSubmissionFailure{Version: 1, Reason: reason})
}

func submissionFailureResponse(reason RepairFailureReason, message string) fantasy.ToolResponse {
	return tagSubmissionFailure(fantasy.NewTextErrorResponse(message), reason)
}

func submissionFailureMetadata(raw string) RepairFailureReason {
	var metadata resultSubmissionFailure
	if json.Unmarshal([]byte(raw), &metadata) != nil || metadata.Version != 1 {
		return ""
	}
	switch metadata.Reason {
	case RepairFailureInvalidSchema, RepairFailureSource, RepairFailureContract, RepairFailureCommit:
		return metadata.Reason
	default:
		return ""
	}
}

func submissionFailureFromSteps(steps []fantasy.StepResult) RepairFailureReason {
	var reason RepairFailureReason
	for _, step := range steps {
		for _, result := range step.Content.ToolResults() {
			if result.ToolName != submitResultToolName {
				continue
			}
			if _, isError := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](result.Result); !isError {
				continue
			}
			current := submissionFailureMetadata(result.ClientMetadata)
			switch current {
			case RepairFailureSource, RepairFailureContract, RepairFailureCommit:
				// A later format error cannot erase an earlier non-format rejection.
				return current
			case RepairFailureInvalidSchema:
				reason = current
			}
		}
	}
	return reason
}

func (t *submitResultTool) payloadRejectionReason(input string) RepairFailureReason {
	issues, _, err := resultSubmissionIssues(input, t.candidateValidationInfo())
	if err == nil && len(issues) > 0 {
		return RepairFailureInvalidSchema
	}
	// Complete model-facing data failed canonical assembly/evidence validation.
	// Repeating a formatting turn cannot restore a missing authorized source.
	return RepairFailureSource
}

func resultPersistenceFailure(err error) fantasy.ToolResponse {
	reason := RepairFailureCommit
	if errors.Is(err, errTaskResultStale) {
		reason = RepairFailureSource
	}
	return submissionFailureResponse(reason, err.Error())
}
