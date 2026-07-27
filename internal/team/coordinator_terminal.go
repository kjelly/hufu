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

// SetPTYTerminalEnabled enables the experimental local PTY broker. Unattended
// runs deliberately do not expose the user-attach socket.
func (c *Coordinator) SetPTYTerminalEnabled(enabled bool) error {
	if c == nil || !enabled {
		return nil
	}
	if c.unattended {
		return nil
	}
	c.initTerminalControl()
	c.terminalControlMu.Lock()
	if c.ptyTerminalEnabled {
		c.terminalControlMu.Unlock()
		return nil
	}
	c.terminalControlMu.Unlock()
	broker, err := StartTerminalBrokerWithHooks(c.session.Workspace, c.terminalSessionMgr, TerminalBrokerHooks{
		OnAttach: c.pauseTerminalTask,
		OnDetach: c.resumeTerminalTask,
	})
	if err != nil {
		return fmt.Errorf("start PTY terminal broker: %w", err)
	}
	c.terminalControlMu.Lock()
	c.terminalBroker = broker
	c.ptyTerminalEnabled = true
	c.terminalControlMu.Unlock()
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
