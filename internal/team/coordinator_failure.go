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

// rememberDiagnosticHint keeps the bounded reflection candidate with the task
// so a later failure packet can include the actual sidecar/local hypothesis,
// rather than losing it after it was used only in the retry prompt.
func (c *Coordinator) rememberDiagnosticHint(todoID, hint string) {
	if c == nil || c.taskTracker == nil || strings.TrimSpace(hint) == "" {
		return
	}
	_ = c.taskTracker.TodoList().AppendDiagnosticHint(todoID, redactRetryText(hint, 500))
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
	c.persistFailure(agentName, taskDesc, todoID, detail, RetryNone, "", nil)
}

// PersistFailureWithDisposition records the recovery action selected for a
// failed attempt. The legacy PersistFailure wrapper remains available for
// callers that do not have a recovery decision yet.
func (c *Coordinator) PersistFailureWithDisposition(agentName, taskDesc, todoID, detail string, disposition RetryDisposition) {
	c.persistFailure(agentName, taskDesc, todoID, detail, disposition, "", nil)
}

// PersistFailureWithClass preserves the structured class selected by the
// retry decision. The detail string remains human-readable evidence; it is
// never used to override this class when building fingerprints or events.
func (c *Coordinator) PersistFailureWithClass(agentName, taskDesc, todoID, detail string, disposition RetryDisposition, class TaskFailureClass) {
	c.persistFailure(agentName, taskDesc, todoID, detail, disposition, class, nil)
}

// PersistFailureWithClassAndStatusAndOutput records structured failure
// evidence and atomically attaches bounded worker output before the terminal
// status checkpoint. It is used when the output is the primary evidence for a
// protocol failure.
func (c *Coordinator) PersistFailureWithClassAndStatusAndOutput(agentName, taskDesc, todoID, detail string, disposition RetryDisposition, class TaskFailureClass, status TaskStatus, output string) {
	c.persistFailureWithOutput(agentName, taskDesc, todoID, detail, disposition, class, &status, output)
}

// PersistFailureWithClassAndStatus records structured failure evidence while
// preserving a caller-selected terminal status. Finalization uses this when
// an incomplete task must remain TaskError, and recovery/capability paths use
// it when a failure must remain TaskBlocked.
func (c *Coordinator) PersistFailureWithClassAndStatus(agentName, taskDesc, todoID, detail string, disposition RetryDisposition, class TaskFailureClass, status TaskStatus) {
	c.persistFailureWithOutput(agentName, taskDesc, todoID, detail, disposition, class, &status, "")
}

func (c *Coordinator) persistFailure(agentName, taskDesc, todoID, detail string, disposition RetryDisposition, class TaskFailureClass, forcedStatus *TaskStatus) {
	c.persistFailureWithOutput(agentName, taskDesc, todoID, detail, disposition, class, forcedStatus, "")
}

func (c *Coordinator) persistFailureWithOutput(agentName, taskDesc, todoID, detail string, disposition RetryDisposition, class TaskFailureClass, forcedStatus *TaskStatus, output string) {
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
	if class == "" {
		class = classifyTaskFailure(errors.New(detail))
	}
	var failureEvent *FailureEventPayload
	criterion := c.failedCriterionForTask(item)
	// §6.2: the systemic-scope operation must be STABLE across the failed
	// task and a future un-fingerprinted candidate. LastOperation (the
	// mutable last tool call) would diverge between a task that ran
	// (LastOperation set) and a candidate that has not (LastOperation
	// empty), so the systemic fingerprint uses stableOperation (derived
	// from the immutable verify/reconcile/kind config) instead of
	// failureOperation. The full failureOperation is still available in
	// the task/run fingerprints for non-systemic anti-thrashing. Refs:
	// docs/hufu-generic-task-reliability-mechanisms.md §6.2, WP-10
	fp := NewFailureFingerprint(criterion, agentName, stableOperation(item), class, detail)
	strategy := RecoveryStrategy("")
	// §5.3: cancelled failures must not be counted in retry, failure-class
	// statistics or the anti-thrashing fingerprint. Skip the fingerprint
	// recording and anti-thrashing state mutation; the task status update
	// and workspace error file below still run so the todo list and
	// forensics reflect the cancellation.
	// Refs: docs/hufu-generic-task-reliability-mechanisms.md §5.3, WP-05
	cancelled := IsCancelledClass(class)
	var repeated, limited, systemic bool
	var hypothesisInvalid bool
	priorStrategy := RecoveryStrategy("")
	if !cancelled {
		c.metricsMu.Lock()
		priorStrategy = c.antiThrashing.LastStrategy[fp.Digest]
		if item != nil && item.RecoveryHypothesis != nil {
			strategy = item.RecoveryHypothesis.Strategy
		}
		repeated, limited, systemic = c.antiThrashing.record(item, fp, strategy, c.reliabilityConfig())
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
			_ = c.emitEvent(kind, "coordinator", todoID, map[string]interface{}{"fingerprint": fp, "repeated": repeated, "limited": limited, "warning": true})
			if forcedStatus == nil && limited && c.reliabilityConfig().HardEnforcement && item != nil {
				detail += " | anti-thrashing limit reached; strategy change or human review required"
			}
		}
		_ = c.emitEvent("failure_fingerprint", "coordinator", todoID, map[string]interface{}{"fingerprint": fp, "count": c.failureFingerprintCount(fp.Digest), "repeated": repeated})
		if systemic {
			// §6.2 systemic scope escalation: a (component, operation, class,
			// digest) failure has now been observed across MaxSystemicFailureTasks
			// distinct tasks. The disposition is class-derived (protocol /
			// environment / contract → needs_human; any other → replan_required)
			// and dispatch to that scope is blocked. The visible, actionable
			// behavior is the event + the hard block + the status message.
			// Refs: docs/hufu-generic-task-reliability-mechanisms.md §6.2, WP-10
			disposition := SystemicDispositionForClass(class)
			_ = c.emitEvent("systemic_escalation", "coordinator", todoID, map[string]interface{}{
				"fingerprint":    fp,
				"scope":          systemicScopeKey(fp),
				"disposition":    disposition,
				"distinct_tasks": c.antiThrashing.systemicTaskCount(systemicScopeKey(fp)),
				"threshold":      c.reliabilityConfig().MaxSystemicFailureTasks,
				"class":          string(class),
				"warning":        true,
			})
			detail += " | systemic defect escalated: " + disposition + " (scope blocked)"
		}
		if repeated && item != nil && item.Kind == TaskKindRepair {
			hypothesisInvalid = item.RecoveryHypothesis == nil || item.RecoveryHypothesis.ValidateForTask(fp.CriterionID, true, priorStrategy, taskDefFromTodoItem(item)) != nil
			if hypothesisInvalid {
				_ = c.emitEvent("recovery_hypothesis_missing", "coordinator", todoID, map[string]interface{}{"fingerprint": fp, "warning": true})
				c.metricsMu.Lock()
				c.antiThrashing.rememberRejectedStrategy(fp.Digest, strategy)
				if c.reliabilityConfig().HardEnforcement {
					c.antiThrashing.markBlockedScope(item, fp)
				}
				c.metricsMu.Unlock()
			}
		}
	}

	if todoID != "" && c.taskTracker != nil && c.taskTracker.TodoList() != nil {
		if disposition == "" {
			disposition = RetryNone
		}
		disposition = c.recordDiagnosticPacket(item, class, disposition, detail, fp, repeated, systemic)
		failureEvent = c.failureEventForItem(item, class, disposition, detail, fp, todoID)
		failureOutput := utils.TruncateString(utils.RedactSecrets(output), 2000)
		_ = c.taskTracker.TodoList().SetFailureEventAndOutput(todoID, failureEvent, failureOutput)
		if c.reportStatus != nil {
			data := map[string]any{
				"failure_event": failureEvent,
			}
			if failureOutput != "" {
				data["failure_output"] = failureOutput
			}
			c.report(c.newEvent("failure").withAgent(agentName).withMessage(RenderFailureText(failureEvent)).withTodoID(todoID).withData(data))
		}
	}

	if todoID != "" {
		status := TaskError
		if forcedStatus != nil {
			status = *forcedStatus
		}
		if isPermissionBlockedFailureDetail(detail) {
			status = TaskBlocked
		}
		if disposition == NeedsHuman && forcedStatus == nil {
			status = TaskBlocked
		}
		if forcedStatus == nil && (limited || hypothesisInvalid || systemic) && c.reliabilityConfig().HardEnforcement {
			status = TaskBlocked
		}
		if cancelled {
			// §5.3: cancelled tasks surface as a dedicated event so the run
			// record distinguishes a user/context cancel from an execution
			// failure. The todo status stays TaskError (the task did not
			// complete) but the failure class and all statistics exclude it.
			c.emitEvent("task_cancelled", "coordinator", todoID, map[string]interface{}{"class": string(class), "agent": agentName})
		}
		c.taskTracker.TodoList().UpdateStatus(todoID, status, detail)
		c.reconcileTaskStatusProjection()
		if c.reportStatus != nil {
			c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
		}
	}

	if c.session != nil && c.session.Workspace != "" && agentName != "" && taskDesc != "" {
		taskTS := time.Now().Format("20060102-150405")
		failureOutput := utils.TruncateString(utils.RedactSecrets(output), 2000)
		if failureOutput == "" && item != nil {
			failureOutput = utils.TruncateString(utils.RedactSecrets(item.Output), 2000)
		}
		_ = writeTaskFileWithFailureEvent(c.session.Workspace, c.session.Config.Name, agentName, taskTS, "error", taskDesc, failureOutput, detail, failureEvent)
		var fingerprints []FailureFingerprint
		if item != nil {
			fingerprints = item.FailureFingerprints
		}
		c.recordTaskFailureWithEventAndOutput(agentName, taskDesc, detail, failureEvent, failureOutput, fingerprints)
	}
}

func failureOperation(item *TodoItem) string {
	if item == nil {
		return ""
	}
	if strings.TrimSpace(item.LastOperation) != "" {
		return item.LastOperation
	}
	return stableOperation(item)
}

// stableOperation returns the task's immutable operation identity derived
// from its verification/recovery/kind configuration, ignoring the mutable
// LastOperation (the last tool call name recorded during execution, e.g.
// "bash"/"write"). It is used for the systemic-scope fingerprint so that a
// failed task (whose LastOperation was populated during execution) and a
// future un-fingerprinted candidate (which has not run and therefore has
// no LastOperation) derive the SAME operation, letting the systemic
// prefix block match post-escalation dispatch (§6.2: 停止對該 scope 派工).
// Refs: docs/hufu-generic-task-reliability-mechanisms.md §6.2, WP-10
func stableOperation(item *TodoItem) string {
	if item == nil {
		return ""
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
		_ = c.emitEvent("anti_thrashing_limit_reached", "coordinator", item.ID, map[string]interface{}{
			"limit":   "max-diagnostic-tasks-without-progress",
			"count":   c.Metrics().DiagnosticTasksSinceProgress,
			"warning": true,
		})
	}
}

func (c *Coordinator) reliabilityConfig() agent.ReliabilityConfig {
	cfg := agent.DefaultReliabilityConfig()
	if c != nil && c.session != nil {
		sessCfg := c.session.Config.Reliability
		if sessCfg.MaxDiagnosticTasksWithoutProgress > 0 {
			cfg.MaxDiagnosticTasksWithoutProgress = sessCfg.MaxDiagnosticTasksWithoutProgress
		}
		if sessCfg.MaxSameFailureFingerprint > 0 {
			cfg.MaxSameFailureFingerprint = sessCfg.MaxSameFailureFingerprint
		}
		if sessCfg.MaxRepairsPerCriterion > 0 {
			cfg.MaxRepairsPerCriterion = sessCfg.MaxRepairsPerCriterion
		}
		if sessCfg.MaxSystemicFailureTasksSet {
			// Honor an explicit YAML zero (disables the feature) rather
			// than restoring the default. Refs:
			// docs/hufu-generic-task-reliability-mechanisms.md §6.2, WP-10
			cfg.MaxSystemicFailureTasks = sessCfg.MaxSystemicFailureTasks
		} else if sessCfg.MaxSystemicFailureTasks > 0 {
			cfg.MaxSystemicFailureTasks = sessCfg.MaxSystemicFailureTasks
		}
		// No-progress budget (§8.1, WP-12): honor explicit value (including
		// 0 to disable one counter); unset keeps the default.
		if sessCfg.MaxTokensWithoutProgressSet {
			cfg.MaxTokensWithoutProgress = sessCfg.MaxTokensWithoutProgress
		} else if sessCfg.MaxTokensWithoutProgress > 0 {
			cfg.MaxTokensWithoutProgress = sessCfg.MaxTokensWithoutProgress
		}
		if sessCfg.MaxTurnsWithoutProgressSet {
			cfg.MaxTurnsWithoutProgress = sessCfg.MaxTurnsWithoutProgress
		} else if sessCfg.MaxTurnsWithoutProgress > 0 {
			cfg.MaxTurnsWithoutProgress = sessCfg.MaxTurnsWithoutProgress
		}
		if sessCfg.MaxTasksWithoutProgressSet {
			cfg.MaxTasksWithoutProgress = sessCfg.MaxTasksWithoutProgress
		} else if sessCfg.MaxTasksWithoutProgress > 0 {
			cfg.MaxTasksWithoutProgress = sessCfg.MaxTasksWithoutProgress
		}
		if sessCfg.WarnOnly {
			cfg.WarnOnly = true
			cfg.HardEnforcement = false
		} else {
			cfg.HardEnforcement = cfg.HardEnforcement || sessCfg.HardEnforcement || c.ExecutionProfile().AntiThrashingEnforced
		}
		return cfg
	}
	return cfg
}

// GetLastFailureContext returns the most recently persisted structured failure
// metadata, if any.
func (c *Coordinator) GetLastFailureContext() (agentName, taskDesc, todoID, detail string) {
	return c.getLastFailureContext()
}

// stableOperationFromTask derives the same operation identity as
// stableOperation but from a TaskDef instead of a TodoItem. This is used
// in the retry loop where the TaskDef is available but the TodoItem may not
// reflect the current attempt's verify configuration.
func stableOperationFromTask(task TaskDef) string {
	if task.VerifySpec != nil {
		if task.VerifySpec.Type != "" {
			return "verify:" + string(task.VerifySpec.Type)
		}
		if strings.TrimSpace(task.VerifySpec.Command) != "" {
			return "verify:" + task.VerifySpec.Command
		}
	}
	if strings.TrimSpace(task.Verify) != "" {
		return "verify:" + task.Verify
	}
	if strings.TrimSpace(task.ReconcileTool) != "" {
		return "reconcile:" + task.ReconcileTool
	}
	return "task:" + string(task.Kind)
}
