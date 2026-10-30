package team

import (
	"context"
	"errors"
	"fmt"

	"charm.land/fantasy"
)

// Inline workers own their transcript even when invoked from a parent's tool
// context. Seal actual callbacks before persisting a receipt; never reconstruct
// tool evidence from a summary or borrow the parent's recorder.
func (c *Coordinator) runInlineWorkerTranscript(ctx context.Context, worker fantasy.Agent, name, prompt string, timing *taskTiming) (string, []fantasy.StepResult, *ArtifactRef, error) {
	todoID, _ := ctx.Value(todoIDKey{}).(string)
	if todoID == "" {
		return "", nil, nil, fmt.Errorf("inline worker transcript requires a task identity")
	}
	attempt := c.taskAttempt(todoID)
	transcript, err := newTaskTranscriptForAttempt(c.session.Workspace, todoID, coordinatorRuntimeRunID(c), attempt, name)
	if err != nil {
		return "", nil, nil, err
	}
	ctx = context.WithValue(ctx, taskTranscriptKey{}, transcript)
	output, steps, runErr := c.runAgentWithStatusAndHistory(ctx, worker, name, prompt, nil, timing)
	recordErr := transcript.RecordAssistantOutput(output)
	ref, sealErr := sealVerbatimTaskTranscript(transcript)
	closeErr := transcript.Close()
	return output, steps, ref, errors.Join(runErr, recordErr, sealErr, closeErr)
}

func (c *Coordinator) persistInlineWorkerReceipt(receipt *ExecutionReceipt, transcriptRef *ArtifactRef, steps []fantasy.StepResult, runErr error) error {
	if transcriptRef != nil {
		receipt.TranscriptRef = transcriptRef.ID
	}
	if runErr != nil {
		receipt.ExitCode = new(1)
	}
	c.applyResultContractReceipt(receipt.TaskID, c.GetTaskResult(receipt.TaskID), receipt)
	err := c.setAttemptReceipt(receipt.TaskID, receipt)
	c.recordAttemptUsage(receipt.TaskID, receipt.Attempt, usageFromSteps(steps))
	return err
}
