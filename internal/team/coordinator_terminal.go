package team

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// terminalTaskPause coordinates a model round interrupted for a human terminal
// takeover. The channel closes only after the user releases the PTY lease.
type terminalTaskPause struct {
	resume chan struct{}
	cancel context.CancelFunc
}

func (c *Coordinator) initTerminalControl() {
	c.terminalControlMu.Lock()
	defer c.terminalControlMu.Unlock()
	if c.terminalPauses == nil {
		c.terminalPauses = make(map[string]*terminalTaskPause)
	}
	if c.terminalRoundCancels == nil {
		c.terminalRoundCancels = make(map[string]context.CancelFunc)
	}
	if c.terminalRoundDone == nil {
		c.terminalRoundDone = make(map[string]chan struct{})
	}
}

func (c *Coordinator) isTerminalRoundActive(taskID string) bool {
	if c == nil || taskID == "" {
		return false
	}
	c.terminalControlMu.Lock()
	defer c.terminalControlMu.Unlock()
	_, active := c.terminalRoundCancels[taskID]
	return active
}

// TerminalSessions returns durable terminal lifecycle metadata for reporting.
// It intentionally exposes references and lifecycle state only, never output.
func (c *Coordinator) TerminalSessions(ctx context.Context) ([]TerminalSession, error) {
	if c == nil || c.terminalSessionMgr == nil {
		return nil, nil
	}
	return c.terminalSessionMgr.List(ctx, "")
}

// TransferTerminal performs the Phase D, operator-authorized task handoff.
// It is coordinator-only: the model-facing terminal tool deliberately has no
// transfer action. The original OwnerTaskID remains durable provenance while
// controller authority moves to the declared destination task.
func (c *Coordinator) TransferTerminal(ctx context.Context, req TerminalTransferRequest) (TerminalSession, error) {
	if c == nil || c.terminalSessionMgr == nil {
		return TerminalSession{}, errors.New("transfer terminal: terminal manager is unavailable")
	}
	if c.taskTracker == nil || c.taskTracker.TodoList() == nil {
		return TerminalSession{}, errors.New("transfer terminal: task tracker is unavailable")
	}
	if req.RunID == "" {
		req.RunID = c.executionRunID
	}
	if req.RunID == "" || (c.executionRunID != "" && req.RunID != c.executionRunID) {
		return TerminalSession{}, fmt.Errorf("transfer terminal: requested run %q is not the active run", req.RunID)
	}
	var source, destination *TodoItem
	for _, item := range c.taskTracker.TodoList().Items() {
		switch item.ID {
		case req.SourceTaskID:
			source = item
		case req.DestinationTaskID:
			destination = item
		}
	}
	if source == nil || destination == nil {
		return TerminalSession{}, errors.New("transfer terminal: source and destination tasks must exist in the active run")
	}
	if source.Status != TaskPaused {
		return TerminalSession{}, fmt.Errorf("transfer terminal: source task %q must be paused", source.ID)
	}
	if isTerminalTaskStatus(destination.Status) {
		return TerminalSession{}, fmt.Errorf("transfer terminal: destination task %q is terminal (%s)", destination.ID, destination.Status)
	}
	if c.isTerminalRoundActive(source.ID) || c.isTerminalRoundActive(destination.ID) {
		return TerminalSession{}, errors.New("transfer terminal: source and destination tasks must not have active model rounds")
	}
	return c.terminalSessionMgr.TransferTerminal(ctx, req)
}

func isTerminalTaskStatus(status TaskStatus) bool {
	return status == TaskDone || status == TaskSkipped || status == TaskError || status == TaskBlocked
}

// SetPTYTerminalEnabled enables the local PTY broker. It is safe to call from
// every pty:true terminal start; the first caller initializes the broker and
// later calls are no-ops. Unattended runs deliberately expose no attach socket.
func (c *Coordinator) SetPTYTerminalEnabled(enabled bool) error {
	if c == nil || !enabled {
		return nil
	}
	if c.session == nil || c.session.Workspace == "" || c.terminalSessionMgr == nil {
		return fmt.Errorf("PTY terminal broker is unavailable without a coordinator workspace")
	}
	if c.unattended {
		return nil
	}
	c.initTerminalControl()
	c.terminalControlMu.Lock()
	defer c.terminalControlMu.Unlock()
	if c.ptyTerminalEnabled {
		return nil
	}
	broker, err := StartTerminalBrokerWithHooks(c.session.Workspace, c.terminalSessionMgr, TerminalBrokerHooks{
		OnAttach:   c.pauseTerminalTask,
		OnDetach:   c.resumeTerminalTask,
		OnTransfer: c.TransferTerminal,
	})
	if err != nil {
		return fmt.Errorf("start PTY terminal broker: %w", err)
	}
	c.terminalBroker = broker
	c.ptyTerminalEnabled = true
	return nil
}

func (c *Coordinator) PTYTerminalEnabled() bool {
	if c == nil {
		return false
	}
	c.terminalControlMu.Lock()
	defer c.terminalControlMu.Unlock()
	return c.ptyTerminalEnabled
}

func (c *Coordinator) registerTerminalRound(taskID string, cancel context.CancelFunc) {
	c.initTerminalControl()
	c.terminalControlMu.Lock()
	defer c.terminalControlMu.Unlock()
	c.terminalRoundCancels[taskID] = cancel
	c.terminalRoundDone[taskID] = make(chan struct{})
	if paused := c.terminalPauses[taskID]; paused != nil {
		cancel()
	}
}

func (c *Coordinator) unregisterTerminalRound(taskID string) {
	c.terminalControlMu.Lock()
	delete(c.terminalRoundCancels, taskID)
	if done := c.terminalRoundDone[taskID]; done != nil {
		delete(c.terminalRoundDone, taskID)
		close(done)
	}
	c.terminalControlMu.Unlock()
}

// finalizeTaskTerminalResources contains a terminal left behind by a task
// after its model round has stopped. It deliberately preserves taskErr: a
// successful cleanup proves containment, never successful task execution.
// The returned bool is true only when the resource still needs human
// intervention, which prevents an automatic retry from racing that session.
func (c *Coordinator) finalizeTaskTerminalResources(ctx context.Context, todoID string, taskErr error) (error, bool) {
	if c == nil || c.terminalSessionMgr == nil {
		return taskErr, false
	}
	if taskErr == nil {
		if err := c.terminalSessionMgr.RequireTaskClosed(todoID); err != nil {
			taskErr = err
		}
	}
	if taskErr == nil {
		return nil, false
	}
	reason := TerminalCleanupTaskFailed
	if errors.Is(taskErr, context.Canceled) || errors.Is(taskErr, context.DeadlineExceeded) {
		reason = TerminalCleanupTaskCancelled
	} else if strings.Contains(taskErr.Error(), "unclosed terminal session") {
		reason = TerminalCleanupTaskIncomplete
	}
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	results, cleanupErr := c.terminalSessionMgr.CleanupTaskTerminals(cleanupCtx, TerminalCleanupRequest{
		OwnerTaskID: todoID,
		Reason:      reason,
		GracePeriod: time.Second,
		ForceAfter:  5 * time.Second,
	})
	if cleanupErr != nil {
		return errors.Join(taskErr, fmt.Errorf("terminal cleanup: %w", cleanupErr)), true
	}
	for _, result := range results {
		if result.ManualAction {
			return errors.Join(taskErr, fmt.Errorf("terminal cleanup requires manual intervention for session %q", result.Session.ID)), true
		}
	}
	return taskErr, false
}

// cleanupRunTerminalResources stops model rounds, prevents new broker
// attachments, and contains every live terminal before run finalization.
func (c *Coordinator) cleanupRunTerminalResources(reason TerminalCleanupReason) error {
	if c == nil || c.terminalSessionMgr == nil {
		return nil
	}
	c.initTerminalControl()
	c.terminalControlMu.Lock()
	cancels := make([]context.CancelFunc, 0, len(c.terminalRoundCancels))
	roundDone := make([]<-chan struct{}, 0, len(c.terminalRoundDone))
	for _, cancel := range c.terminalRoundCancels {
		cancels = append(cancels, cancel)
	}
	for _, done := range c.terminalRoundDone {
		roundDone = append(roundDone, done)
	}
	broker := c.terminalBroker
	c.terminalBroker = nil
	c.ptyTerminalEnabled = false
	c.terminalControlMu.Unlock()
	for _, cancel := range cancels {
		cancel()
	}
	// Closing the listener first prevents a new lease from appearing between
	// cleanup selection and the manager's lease revocation.
	if broker != nil {
		_ = broker.Close()
	}
	roundShutdownTimeout := c.terminalRoundShutdownTimeout
	if roundShutdownTimeout <= 0 {
		roundShutdownTimeout = 15 * time.Second
	}
	cleanupCtx, cancel := context.WithTimeout(context.Background(), roundShutdownTimeout)
	defer cancel()
	roundTimedOut := false
	for _, done := range roundDone {
		select {
		case <-done:
		case <-cleanupCtx.Done():
			roundTimedOut = true
		}
		if roundTimedOut {
			break
		}
	}
	runID := c.executionRunID
	if runID == "" && c.taskTracker != nil && c.taskTracker.TodoList() != nil {
		runID = c.taskTracker.TodoList().RunID()
	}
	if roundTimedOut {
		// The owner has already been cancelled but failed to acknowledge it.
		// Block it before containment; the manager then atomically revokes
		// owner custody so a late tool call is denied while the child dies.
		c.blockTerminalCleanupTasks(runID, fmt.Errorf("terminal model round did not stop before shutdown deadline"))
		cleanupCtx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
	}
	var results []TerminalCleanupResult
	var err error
	if roundTimedOut {
		results, err = c.terminalSessionMgr.CleanupRunTerminalsAfterRoundTimeout(cleanupCtx, runID, reason)
	} else {
		results, err = c.terminalSessionMgr.CleanupRunTerminals(cleanupCtx, runID, reason)
	}
	if err != nil {
		cleanupErr := fmt.Errorf("cleanup run terminals: %w", err)
		c.blockTerminalCleanupTasks(runID, cleanupErr)
		return cleanupErr
	}
	for _, result := range results {
		if result.ManualAction {
			cleanupErr := fmt.Errorf("terminal cleanup requires manual intervention for session %q", result.Session.ID)
			c.blockTerminalCleanupTasks(runID, cleanupErr)
			return cleanupErr
		}
	}
	return nil
}

// blockTerminalCleanupTasks makes failed shutdown containment visible in the
// canonical task state. A later finalizeRemainingTasks call intentionally
// leaves TaskBlocked untouched, so the operator evidence is not overwritten.
func (c *Coordinator) blockTerminalCleanupTasks(runID string, cleanupErr error) {
	if c == nil || c.taskTracker == nil || c.taskTracker.TodoList() == nil || c.terminalSessionMgr == nil || cleanupErr == nil {
		return
	}
	sessions, err := c.terminalSessionMgr.List(context.Background(), runID)
	if err != nil {
		return
	}
	for _, session := range sessions {
		controllerTaskID := terminalControllerTaskID(session)
		if controllerTaskID == "" || (session.CleanupState != TerminalCleanupManual && session.State != TerminalSessionRunning && session.State != TerminalSessionUnknown && !session.Running) {
			continue
		}
		for _, item := range c.taskTracker.TodoList().Items() {
			if item.ID != controllerTaskID || item.Status == TaskDone || item.Status == TaskSkipped || item.Status == TaskBlocked {
				continue
			}
			detail := c.FailureDetail(cleanupErr, "terminal_cleanup")
			c.PersistFailureWithClassAndStatus(item.Agent, item.Desc, item.ID, detail, NeedsHuman, FailureExecution, TaskBlocked)
			break
		}
	}
}

func (c *Coordinator) pauseTerminalTask(session TerminalSession) {
	controllerTaskID := terminalControllerTaskID(session)
	if controllerTaskID == "" {
		return
	}
	c.initTerminalControl()
	c.terminalControlMu.Lock()
	if _, exists := c.terminalPauses[controllerTaskID]; exists {
		c.terminalControlMu.Unlock()
		return
	}
	pause := &terminalTaskPause{resume: make(chan struct{})}
	pause.cancel = c.terminalRoundCancels[controllerTaskID]
	c.terminalPauses[controllerTaskID] = pause
	c.terminalControlMu.Unlock()
	if pause.cancel != nil {
		pause.cancel()
	}
	if err := c.taskTracker.TodoList().TryUpdateStatusAndOutput(controllerTaskID, TaskPaused, "waiting for human terminal handoff", ""); err == nil {
		c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
	}
	c.report(c.newEvent("terminal_taken_over").withTodoID(controllerTaskID).withMessage("human attached to PTY; model round paused"))
}

func (c *Coordinator) resumeTerminalTask(session TerminalSession) {
	controllerTaskID := terminalControllerTaskID(session)
	if controllerTaskID == "" {
		return
	}
	c.terminalControlMu.Lock()
	pause := c.terminalPauses[controllerTaskID]
	if pause != nil {
		delete(c.terminalPauses, controllerTaskID)
		close(pause.resume)
	}
	c.terminalControlMu.Unlock()
	if pause == nil {
		return
	}
	if err := c.taskTracker.TodoList().TryUpdateStatusAndOutput(controllerTaskID, TaskInProgress, "human terminal handoff returned", ""); err == nil {
		c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
	}
	c.report(c.newEvent("terminal_released").withTodoID(controllerTaskID).withMessage("human released PTY; resuming model round"))
}

func (c *Coordinator) waitForTerminalResume(ctx context.Context, taskID string) bool {
	c.terminalControlMu.Lock()
	pause := c.terminalPauses[taskID]
	c.terminalControlMu.Unlock()
	if pause == nil {
		return false
	}
	select {
	case <-pause.resume:
		return true
	case <-ctx.Done():
		return false
	}
}
