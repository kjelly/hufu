package auditverify

import (
	"context"
	"fmt"

	"github.com/kjelly/hufu/internal/team"
)

// ExplainResult is the complete, deterministic answer to "why was this run
// considered complete/failed/partial?" (spec.md §20). Rendering it as text is
// the CLI's job; this package only assembles the underlying facts, and does
// so without ever calling an LLM (spec.md §21).
type ExplainResult struct {
	Verification *AuditVerificationResult `json:"verification"`
	Witness      *DecisionWitness         `json:"witness"`

	// AttemptHistory lists every attempt for every task that has one, oldest
	// first, so a retried task's full history -- not just its final status --
	// is visible (spec.md §25).
	AttemptHistory []TaskAttemptHistory `json:"attempt_history,omitempty"`
}

// TaskAttemptHistory is one task's complete attempt history plus which
// attempt (if any) was selected as the evidence-bound winner.
type TaskAttemptHistory struct {
	TaskID   string          `json:"task_id"`
	Status   team.TaskStatus `json:"status"`
	Winning  ReceiptRef      `json:"winning,omitempty"`
	Attempts []TaskAttempt   `json:"attempts"`
}

// TaskAttempt is one execution attempt's forensic summary.
type TaskAttempt struct {
	Attempt          int    `json:"attempt"`
	ModelExecutionID string `json:"model_execution_id,omitempty"`
	ProducerID       string `json:"producer_id,omitempty"`
	ExitCode         *int   `json:"exit_code,omitempty"`
	VerifierCommand  string `json:"verifier_command,omitempty"`
	VerifierExitCode *int   `json:"verifier_exit_code,omitempty"`
	Winning          bool   `json:"winning"`
}

// ExplainRun assembles ExplainResult for a run: it runs the same verification
// phases VerifyWorkspaceRun does (one lineage scan, spec.md §46), then
// projects a DecisionWitness and per-task attempt history from the result.
// An error means explanation could not even be attempted; a non-error result
// is returned regardless of the verdict -- explaining why a run failed is as
// much this command's job as explaining why it passed.
func ExplainRun(ctx context.Context, workspace, runID string) (*ExplainResult, error) {
	verification, projection, err := runWorkspaceAudit(ctx, workspace, runID, VerifyOptions{})
	if err != nil {
		return nil, err
	}
	result := &ExplainResult{Verification: verification}
	if projection == nil || projection.runResult == nil {
		return result, nil
	}

	gate := GateWitness{
		Accepted:               verification.Completion.Status == AuditDimensionPass && projection.runResult.Outcome == team.RunOutcomeCompleted,
		AcceptanceState:        acceptanceState(projection.runResult),
		EvidenceManifestStatus: evidenceStatus(projection.runResult),
		RequiredTasksTotal:     len(projection.tasks),
	}
	for _, item := range projection.tasks {
		if item != nil && item.Status == team.TaskDone {
			gate.RequiredTasksDone++
		}
	}
	if !gate.Accepted && projection.runResult.Reason != "" {
		gate.Reasons = []string{projection.runResult.Reason}
	}

	witness, err := buildDecisionWitness(runID, projection.runResult, projection.tasks,
		projection.terminalEvent.ID, projection.terminalEvent.Hash, projection.requiredCriteria, gate)
	if err != nil {
		return nil, fmt.Errorf("build decision witness: %w", err)
	}
	result.Witness = witness

	for _, item := range projection.tasks {
		if item == nil || len(item.ExecutionReceipts) == 0 {
			continue
		}
		history := TaskAttemptHistory{TaskID: item.ID, Status: item.Status}
		winner := team.LatestSuccessfulExecutionReceipt(item, runID)
		for i := range item.ExecutionReceipts {
			receipt := &item.ExecutionReceipts[i]
			attempt := TaskAttempt{
				Attempt: receipt.Attempt, ModelExecutionID: receipt.ModelExecutionID,
				ProducerID: receipt.ProducerID, ExitCode: receipt.ExitCode,
			}
			if receipt.VerifyResult != nil {
				attempt.VerifierCommand = receipt.VerifyResult.Command
				exitCode := receipt.VerifyResult.ExitCode
				attempt.VerifierExitCode = &exitCode
			}
			if winner != nil && winner.Attempt == receipt.Attempt && winner.ModelExecutionID == receipt.ModelExecutionID {
				attempt.Winning = true
				history.Winning = ReceiptRef{
					RunID: receipt.RunID, TaskID: receipt.TaskID, Attempt: receipt.Attempt,
					ModelExecutionID: receipt.ModelExecutionID, ProducerID: receipt.ProducerID,
				}
			}
			history.Attempts = append(history.Attempts, attempt)
		}
		result.AttemptHistory = append(result.AttemptHistory, history)
	}

	return result, nil
}
