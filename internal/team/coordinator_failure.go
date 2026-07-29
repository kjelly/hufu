package team

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/anomalyco/hufu/internal/tools"
	"github.com/anomalyco/hufu/internal/utils"
)

func detectFailureSource(err error) string {
	switch {
	case err == nil:
		return FailureSourceError
	case isTaskTimeout(err):
		return FailureSourceTaskTimeout
	case errors.Is(err, context.Canceled):
		if tools.IsInteractiveAbortRequested() {
			return FailureSourceSigint
		}
		return FailureSourceContextCanceled
	default:
		return FailureSourceError
	}
}

func (c *Coordinator) rememberFailureContext(agentName, taskDesc, todoID, detail string) {
	if c == nil || detail == "" {
		return
	}
	c.lastFailureMu.Lock()
	c.lastFailureAgent = agentName
	c.lastFailureTask = taskDesc
	c.lastFailureTodoID = todoID
	c.lastFailureDetail = detail
	c.lastFailureMu.Unlock()
}

func (c *Coordinator) getLastFailureContext() (agentName, taskDesc, todoID, detail string) {
	if c == nil {
		return "", "", "", ""
	}
	c.lastFailureMu.RLock()
	defer c.lastFailureMu.RUnlock()
	return c.lastFailureAgent, c.lastFailureTask, c.lastFailureTodoID, c.lastFailureDetail
}

// FailureDetail returns the structured failure detail string used across task,
// coordinator, and CLI failure paths.
func (c *Coordinator) FailureDetail(err error, source string) string {
	if source == "" {
		source = detectFailureSource(err)
	}
	parts := []string{fmt.Sprintf("source=%s", source)}
	if status := c.GetCurrentStatus(); status != "" && status != "idle" {
		parts = append(parts, "current="+status)
	}
	if tool := c.GetCurrentTool(); tool != "" {
		parts = append(parts, "last_tool="+tool)
	}
	if err != nil {
		parts = append(parts, "error="+utils.TruncateString(err.Error(), 500))
	}
	return strings.Join(parts, " | ")
}

// PersistFailure writes the structured failure detail to the active task's
// todo/status/workspace records and remembers it for later CLI-level reporting.
// It is safe to call even when some metadata is unavailable.
func (c *Coordinator) PersistFailure(agentName, taskDesc, todoID, detail string) {
	if c == nil || detail == "" {
		return
	}
	c.rememberFailureContext(agentName, taskDesc, todoID, detail)

	if todoID != "" {
		status := TaskError
		if isPermissionBlockedFailureDetail(detail) {
			status = TaskBlocked
		}
		c.taskTracker.TodoList().UpdateStatus(todoID, status, detail)
		c.reconcileTaskStatusProjection()
		if c.reportStatus != nil {
			c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
		}
	}

	if c.session != nil && c.session.Workspace != "" && agentName != "" && taskDesc != "" {
		taskTS := time.Now().Format("20060102-150405")
		_ = writeTaskFileWithDetail(c.session.Workspace, c.session.Config.Name, agentName, taskTS, "error", taskDesc, "", detail)
		c.recordTaskFailure(agentName, taskDesc, detail)
	}
}

// GetLastFailureContext returns the most recently persisted structured failure
// metadata, if any.
func (c *Coordinator) GetLastFailureContext() (agentName, taskDesc, todoID, detail string) {
	return c.getLastFailureContext()
}
