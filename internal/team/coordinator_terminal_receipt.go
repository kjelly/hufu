package team

import (
	"fmt"
	"strings"
	"time"
)

// persistSuccessfulCoordinatorTaskReceipt seals the coordinator-owned
// execution transcript and persists the task receipt before the caller can
// commit TaskDone. Runtime actions and structured coordinator tasks do not
// have a model stream to create this receipt for them, but they still need the
// same current-run evidence binding as ordinary worker tasks.
func (c *Coordinator) persistSuccessfulCoordinatorTaskReceipt(todoID, producer string, attempt int, startedAt time.Time, output string) error {
	if c == nil || c.taskTracker == nil || c.taskTracker.TodoList() == nil || c.session == nil {
		return fmt.Errorf("coordinator task receipt requires task state")
	}
	if strings.TrimSpace(todoID) == "" || attempt <= 0 {
		return fmt.Errorf("coordinator task receipt requires task identity and positive attempt")
	}
	runID := coordinatorRuntimeRunID(c)
	if strings.TrimSpace(producer) == "" {
		producer = "coordinator"
	}
	transcript, err := newTaskTranscriptForAttempt(c.session.Workspace, todoID, runID, attempt)
	if err != nil {
		return fmt.Errorf("create coordinator task transcript: %w", err)
	}
	closeTranscript := func() error {
		if closeErr := transcript.Close(); closeErr != nil {
			return fmt.Errorf("close coordinator task transcript: %w", closeErr)
		}
		return nil
	}
	if err := transcript.RecordAssistantOutput(output); err != nil {
		_ = closeTranscript()
		return fmt.Errorf("record coordinator task transcript: %w", err)
	}
	transcriptRef, err := transcript.Manifest()
	if err != nil {
		_ = closeTranscript()
		return fmt.Errorf("seal coordinator task transcript: %w", err)
	}
	if err := closeTranscript(); err != nil {
		return err
	}
	zero := 0
	finishedAt := time.Now().UTC()
	receipt := &ExecutionReceipt{
		RunID:            runID,
		TaskID:           todoID,
		Attempt:          attempt,
		ModelExecutionID: contextModelExecutionID(todoID, producer, "coordinator-runtime"),
		StartedAt:        startedAt.UTC(),
		FinishedAt:       finishedAt,
		ExitCode:         &zero,
		ProducerID:       producer,
		TranscriptRef:    transcriptRef.ID,
	}
	if strings.TrimSpace(receipt.ModelExecutionID) == "" || strings.TrimSpace(receipt.TranscriptRef) == "" {
		return fmt.Errorf("coordinator task receipt is missing execution identity or transcript reference")
	}
	if err := c.taskTracker.TodoList().SetExecutionReceipt(todoID, receipt); err != nil {
		return fmt.Errorf("persist coordinator task receipt: %w", err)
	}
	return nil
}
