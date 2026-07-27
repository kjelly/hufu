package team

import (
	"context"
	"fmt"
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
		OnAttach: c.pauseTerminalTask,
		OnDetach: c.resumeTerminalTask,
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
	if paused := c.terminalPauses[taskID]; paused != nil {
		cancel()
	}
}

func (c *Coordinator) unregisterTerminalRound(taskID string) {
	c.terminalControlMu.Lock()
	delete(c.terminalRoundCancels, taskID)
	c.terminalControlMu.Unlock()
}

func (c *Coordinator) pauseTerminalTask(session TerminalSession) {
	if session.OwnerTaskID == "" {
		return
	}
	c.initTerminalControl()
	c.terminalControlMu.Lock()
	if _, exists := c.terminalPauses[session.OwnerTaskID]; exists {
		c.terminalControlMu.Unlock()
		return
	}
	pause := &terminalTaskPause{resume: make(chan struct{})}
	pause.cancel = c.terminalRoundCancels[session.OwnerTaskID]
	c.terminalPauses[session.OwnerTaskID] = pause
	c.terminalControlMu.Unlock()
	if pause.cancel != nil {
		pause.cancel()
	}
	if err := c.taskTracker.TodoList().TryUpdateStatusAndOutput(session.OwnerTaskID, TaskPaused, "waiting for human terminal handoff", ""); err == nil {
		c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
	}
	c.report(c.newEvent("terminal_taken_over").withTodoID(session.OwnerTaskID).withMessage("human attached to PTY; model round paused"))
}

func (c *Coordinator) resumeTerminalTask(session TerminalSession) {
	if session.OwnerTaskID == "" {
		return
	}
	c.terminalControlMu.Lock()
	pause := c.terminalPauses[session.OwnerTaskID]
	if pause != nil {
		delete(c.terminalPauses, session.OwnerTaskID)
		close(pause.resume)
	}
	c.terminalControlMu.Unlock()
	if pause == nil {
		return
	}
	if err := c.taskTracker.TodoList().TryUpdateStatusAndOutput(session.OwnerTaskID, TaskInProgress, "human terminal handoff returned", ""); err == nil {
		c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
	}
	c.report(c.newEvent("terminal_released").withTodoID(session.OwnerTaskID).withMessage("human released PTY; resuming model round"))
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
