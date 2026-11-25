package team

import (
	"context"
	"encoding/json/v2"
	"errors"
	"sync/atomic"

	"charm.land/fantasy"
)

// Client metadata is authored by the runtime and survives the tool execution
// boundary. Never infer retry eligibility from a model's prose or error text.
type resultSubmissionFailure struct {
	Version       int                         `json:"version"`
	Reason        RepairFailureReason         `json:"submission_failure"`
	Normalization ResultNormalizationEvidence `json:"interrupted_normalization,omitzero"`
}

type normalizedResultCommitError struct {
	cause    error
	evidence ResultNormalizationEvidence
}

func (e *normalizedResultCommitError) Error() string { return e.cause.Error() }
func (e *normalizedResultCommitError) Unwrap() error { return e.cause }

func submissionRejectionResponse(err error) fantasy.ToolResponse {
	reason := localResultRejectionReason(err)
	if interrupted, ok := errors.AsType[*normalizedResultCommitError](err); ok {
		response := fantasy.NewTextErrorResponse(err.Error())
		response.StopTurn = true
		return fantasy.WithResponseMetadata(response, resultSubmissionFailure{
			Version: 1, Reason: reason, Normalization: interrupted.evidence,
		})
	}
	return submissionFailureResponse(reason, err.Error())
}

type resultSubmissionTurnKey struct{}
type originalSubmitResultInputKey struct{}

const ignoredSiblingSubmission RepairFailureReason = "ignored_sibling_submission"
const submissionNeedsArtifactRead RepairFailureReason = "submission_needs_artifact_read"

func claimResultSubmissionTurn(ctx context.Context) bool {
	claimed, _ := ctx.Value(resultSubmissionTurnKey{}).(*atomic.Bool)
	return claimed == nil || claimed.CompareAndSwap(false, true)
}

func tagSubmissionFailure(response fantasy.ToolResponse, reason RepairFailureReason) fantasy.ToolResponse {
	if !response.IsError || response.Metadata != "" {
		return response
	}
	response.StopTurn = true
	return fantasy.WithResponseMetadata(response, resultSubmissionFailure{Version: 1, Reason: reason})
}

func submissionFailureResponse(reason RepairFailureReason, message string) fantasy.ToolResponse {
	return tagSubmissionFailure(fantasy.NewTextErrorResponse(message), reason)
}

func sourceSubmissionFailure(err error) fantasy.ToolResponse {
	if _, missing := errors.AsType[*artifactReadCoverageIncomplete](err); missing {
		return fantasy.WithResponseMetadata(fantasy.NewTextErrorResponse(err.Error()), resultSubmissionFailure{
			Version: 1, Reason: submissionNeedsArtifactRead,
		})
	}
	return submissionFailureResponse(resultSourceReadFailureReason(err), err.Error())
}

func submissionFailureMetadata(raw string) RepairFailureReason {
	return decodeSubmissionFailure(raw).Reason
}

func decodeSubmissionFailure(raw string) resultSubmissionFailure {
	var metadata resultSubmissionFailure
	if json.Unmarshal([]byte(raw), &metadata) != nil || metadata.Version != 1 {
		return resultSubmissionFailure{}
	}
	switch metadata.Reason {
	case RepairFailureInvalidSchema, RepairFailureSource, RepairFailureContract, RepairFailureCommit, RepairFailureAction, ignoredSiblingSubmission, submissionNeedsArtifactRead:
		return metadata
	default:
		return resultSubmissionFailure{}
	}
}

func submissionFailureFromSteps(steps []fantasy.StepResult) RepairFailureReason {
	return submissionFailureDetails(steps).Reason
}

func submissionFailureDetails(steps []fantasy.StepResult) resultSubmissionFailure {
	var failure resultSubmissionFailure
	for _, step := range steps {
		for _, result := range step.Content.ToolResults() {
			if result.ToolName != submitResultToolName {
				continue
			}
			if _, isError := fantasy.AsToolResultOutputType[fantasy.ToolResultOutputContentError](result.Result); !isError {
				continue
			}
			current := decodeSubmissionFailure(result.ClientMetadata)
			switch current.Reason {
			case RepairFailureSource, RepairFailureContract, RepairFailureCommit, RepairFailureAction:
				// A later format error cannot erase an earlier non-format rejection.
				return current
			case RepairFailureInvalidSchema:
				failure = current
			}
		}
	}
	return failure
}

func stopsResultSubmission(reason RepairFailureReason) bool {
	return reason != "" && reason != RepairFailureInvalidSchema
}

func isTerminalResultSubmissionFailure(reason RepairFailureReason) bool {
	switch reason {
	case RepairFailureSource, RepairFailureAction, RepairFailureCommit, RepairFailureContract:
		return true
	default:
		return false
	}
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
