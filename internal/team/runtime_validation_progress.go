package team

import (
	"fmt"
	"sync/atomic"
)

var runtimeValidationSequence atomic.Uint64

// Validation progress is ephemeral status, never a journal mutation. Report
// completed checks only: a timer heartbeat would conceal a genuinely stuck
// validator and a journal append would invalidate its own source-head fence.
type runtimeValidationProgress struct {
	coordinator *Coordinator
	id, scope   string
	taskID      string
}

func (c *Coordinator) beginRuntimeValidation(scope, taskID string) *runtimeValidationProgress {
	p := &runtimeValidationProgress{coordinator: c, id: fmt.Sprint(runtimeValidationSequence.Add(1)), scope: scope, taskID: taskID}
	p.report("started", "")
	return p
}

func (p *runtimeValidationProgress) report(phase, check string) {
	message := "Runtime validation: " + p.scope
	if check != "" {
		message += " / " + check
	}
	p.coordinator.report(p.coordinator.newEvent("runtime_validation").withTodoID(p.taskID).withMessage(message).withData(map[string]any{
		"validation_id": p.id, "scope": p.scope, "phase": phase, "check": check,
	}))
}

func (p *runtimeValidationProgress) checked(check string) { p.report("progress", check) }
func (p *runtimeValidationProgress) close()               { p.report("completed", "") }
