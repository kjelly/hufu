package team

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type RunCancellationReason string

const (
	RunCancellationGracefulTimeout RunCancellationReason = "graceful_wrap_up_timeout_exceeded"
	RunCancellationOperatorForce   RunCancellationReason = "operator_force_quit"
)

// RecordRunCancellation persists why the runtime cancelled in-flight work.
// The event is written before context cancellation so post-run inspection does
// not have to infer the trigger from task errors or wall-clock proximity.
func (c *Coordinator) RecordRunCancellation(reason RunCancellationReason, source string, gracefulTimeout time.Duration) error {
	if c == nil || !c.hasDurableEventJournal() {
		return nil
	}
	reasonCode := strings.TrimSpace(string(reason))
	source = strings.TrimSpace(source)
	if reasonCode == "" || source == "" || gracefulTimeout < 0 {
		return fmt.Errorf("record run cancellation: invalid reason, source, or graceful timeout")
	}

	c.executionEventsMu.RLock()
	runID := strings.TrimSpace(c.executionRunID)
	c.executionEventsMu.RUnlock()
	if runID == "" {
		return nil
	}
	c.terminalLifecycleMu.Lock()
	active := c.terminalLifecycleRunID == runID && c.terminalLifecycleState != terminalLifecycleCommitted
	c.terminalLifecycleMu.Unlock()
	if !active {
		return nil
	}

	branchID := c.activeBranchID()
	payload := RunCancellationRequestedPayload{
		RunID: runID, BranchID: branchID, RequestedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Status: "cancellation_requested", ReasonCode: reasonCode, Source: source,
		GracefulTimeoutMillis: gracefulTimeout.Milliseconds(),
	}
	rawPayload, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal run cancellation cause: %w", err)
	}
	_, err = c.EventJournal().Append(context.Background(), RunEvent{
		Type: string(EventRunCancellationRequested), Actor: "runtime", RunID: runID, BranchID: branchID,
		IdempotencyKey: "run_cancellation_requested:" + runID + ":" + reasonCode, Payload: rawPayload,
	})
	if err != nil {
		c.dualWriteFailures.Add(1)
		return fmt.Errorf("persist run cancellation cause: %w", err)
	}
	return nil
}
