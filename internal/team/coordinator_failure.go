package team

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/anomalyco/hufu/internal/agent"
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
	var item *TodoItem
	if c.taskTracker != nil && todoID != "" {
		for _, candidate := range c.taskTracker.TodoList().Items() {
			if candidate != nil && candidate.ID == todoID {
				item = candidate
				break
			}
		}
	}
	class := classifyTaskFailure(errors.New(detail))
	criterion := c.failedCriterionForTask(item)
	fp := NewFailureFingerprint(criterion, agentName, failureOperation(item), class, detail)
	strategy := RecoveryStrategy("")
	c.metricsMu.Lock()
	priorStrategy := c.antiThrashing.LastStrategy[fp.Digest]
	if item != nil && item.RecoveryHypothesis != nil {
		strategy = item.RecoveryHypothesis.Strategy
	}
	repeated, limited := c.antiThrashing.record(item, fp, strategy, c.reliabilityConfig())
	if item != nil {
		_ = c.taskTracker.TodoList().AppendFailureFingerprint(todoID, fp)
		for _, storedItem := range c.taskTracker.TodoList().Items() {
			if storedItem.ID != todoID {
				continue
			}
			for _, stored := range storedItem.FailureFingerprints {
				if stored.Digest == fp.Digest {
					fp = stored
					break
				}
			}
			break
		}
	}
	c.metricsMu.Unlock()
	if repeated || limited {
		kind := "repeated_failure_fingerprint"
		if limited {
			kind = "anti_thrashing_limit_reached"
		}
		c.emitEvent(kind, "coordinator", todoID, map[string]interface{}{"fingerprint": fp, "repeated": repeated, "limited": limited, "warning": true})
		if limited && c.reliabilityConfig().HardEnforcement && item != nil {
			detail += " | anti-thrashing limit reached; strategy change or human review required"
		}
	}
	c.emitEvent("failure_fingerprint", "coordinator", todoID, map[string]interface{}{"fingerprint": fp, "count": c.failureFingerprintCount(fp.Digest), "repeated": repeated})
	hypothesisInvalid := false
	if repeated && item != nil && item.Kind == TaskKindRepair {
		hypothesisInvalid = item.RecoveryHypothesis == nil || item.RecoveryHypothesis.ValidateForTask(fp.CriterionID, true, priorStrategy, taskDefFromTodoItem(item)) != nil
		if hypothesisInvalid {
			c.emitEvent("recovery_hypothesis_missing", "coordinator", todoID, map[string]interface{}{"fingerprint": fp, "warning": true})
			c.metricsMu.Lock()
			c.antiThrashing.rememberRejectedStrategy(fp.Digest, strategy)
			if c.reliabilityConfig().HardEnforcement {
				c.antiThrashing.markBlockedScope(item, fp)
			}
			c.metricsMu.Unlock()
		}
	}

	if todoID != "" {
		status := TaskError
		if isPermissionBlockedFailureDetail(detail) {
			status = TaskBlocked
		}
		if (limited || hypothesisInvalid) && c.reliabilityConfig().HardEnforcement {
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
		var fingerprints []FailureFingerprint
		if item != nil {
			fingerprints = item.FailureFingerprints
		}
		c.recordTaskFailure(agentName, taskDesc, detail, fingerprints)
	}
}

func failureOperation(item *TodoItem) string {
	if item == nil {
		return ""
	}
	if strings.TrimSpace(item.LastOperation) != "" {
		return item.LastOperation
	}
	if item.VerifySpec != nil {
		if item.VerifySpec.Type != "" {
			return "verify:" + string(item.VerifySpec.Type)
		}
		if strings.TrimSpace(item.VerifySpec.Command) != "" {
			return "verify:" + item.VerifySpec.Command
		}
	}
	if strings.TrimSpace(item.Verify) != "" {
		return "verify:" + item.Verify
	}
	if strings.TrimSpace(item.ReconcileTool) != "" {
		return "reconcile:" + item.ReconcileTool
	}
	return "task:" + string(item.Kind)
}

func (c *Coordinator) failedCriterionForTask(item *TodoItem) string {
	if item == nil || len(item.Advances) == 0 {
		return ""
	}
	if c != nil && c.sessionData != nil {
		states := make(map[string]CriterionState, len(c.sessionData.CriterionResults))
		for _, result := range c.sessionData.CriterionResults {
			states[result.ID] = result.State
		}
		for _, id := range item.Advances {
			if state, ok := states[id]; ok && state != CriterionPassed {
				return id
			}
		}
	}
	return item.Advances[0]
}

func (c *Coordinator) failureFingerprintCount(digest string) int {
	c.metricsMu.RLock()
	defer c.metricsMu.RUnlock()
	return c.antiThrashing.Counts[digest]
}

func (c *Coordinator) antiThrashingHardBlocked() bool {
	if c == nil {
		return false
	}
	c.metricsMu.RLock()
	defer c.metricsMu.RUnlock()
	return c.antiThrashing.HardBlocked
}

func (c *Coordinator) antiThrashingBlocksTask(task TaskDef, item *TodoItem) bool {
	if c == nil {
		return false
	}
	c.metricsMu.RLock()
	defer c.metricsMu.RUnlock()
	return c.antiThrashing.blocksTask(task, item)
}

func (c *Coordinator) rebuildAntiThrashingState() {
	if c == nil || c.taskTracker == nil {
		return
	}
	limits := c.reliabilityConfig()
	items := c.taskTracker.TodoList().Items()
	c.metricsMu.Lock()
	c.antiThrashing.rebuild(items, limits)
	c.metricsMu.Unlock()
}

// recordDiagnosticCompletion applies the diagnostic circuit breaker to a
// successful diagnostic as well as to a failed one. Without this path a run
// could exhaust the diagnostic budget while every diagnostic task itself
// appeared successful.
func (c *Coordinator) recordDiagnosticCompletion(item *TodoItem) {
	if c == nil || item == nil || item.Kind != TaskKindDiagnostic || item.Status != TaskDone {
		return
	}
	limits := c.reliabilityConfig()
	c.metricsMu.Lock()
	newTask := c.antiThrashing.recordDiagnostic(item)
	limited := newTask && limits.MaxDiagnosticTasksWithoutProgress > 0 && c.antiThrashing.DiagnosticSinceProgress >= limits.MaxDiagnosticTasksWithoutProgress
	if limited {
		c.antiThrashing.Warnings++
		if limits.HardEnforcement {
			c.antiThrashing.markBlockedScope(item, FailureFingerprint{})
		}
	}
	c.metricsMu.Unlock()
	if limited {
		c.emitEvent("anti_thrashing_limit_reached", "coordinator", item.ID, map[string]interface{}{
			"limit":   "max-diagnostic-tasks-without-progress",
			"count":   c.Metrics().DiagnosticTasksSinceProgress,
			"warning": true,
		})
	}
}

func (c *Coordinator) reliabilityConfig() agent.ReliabilityConfig {
	if c != nil && c.session != nil {
		cfg := c.session.Config.Reliability
		cfg.HardEnforcement = cfg.HardEnforcement || c.ExecutionProfile().AntiThrashingEnforced
		return cfg
	}
	return agent.ReliabilityConfig{}
}

// GetLastFailureContext returns the most recently persisted structured failure
// metadata, if any.
func (c *Coordinator) GetLastFailureContext() (agentName, taskDesc, todoID, detail string) {
	return c.getLastFailureContext()
}
