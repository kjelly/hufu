package team

import (
	"fmt"

	"github.com/kjelly/hufu/internal/execution"
)

// modelFanoutBinding is installed only by the parent fanout dispatcher. DTOs,
// model strings and tool arguments cannot create or retarget this binding.
type modelFanoutBinding struct {
	parent *Coordinator
	taskID string
	index  int
	target execution.ExecutionTarget
}

func (c *Coordinator) isModelFanoutLeaf(taskID string) bool {
	return c != nil && c.modelFanout != nil && c.modelFanout.taskID == taskID
}

func (c *Coordinator) modelFanoutTarget(taskID string) (execution.ExecutionTarget, error) {
	if !c.isModelFanoutLeaf(taskID) {
		return execution.ExecutionTarget{}, fmt.Errorf("task %q has no admitted model fanout binding", taskID)
	}
	binding := c.modelFanout
	item := binding.parent.todoItemByID(taskID)
	if item == nil {
		return execution.ExecutionTarget{}, fmt.Errorf("model fanout parent occurrence %q is unavailable", taskID)
	}
	target, err := admittedExecutionTargetAt(taskDefFromTodoItem(item), binding.index)
	if err != nil {
		return execution.ExecutionTarget{}, err
	}
	if target != binding.target {
		return execution.ExecutionTarget{}, fmt.Errorf("model fanout admitted target changed for task %q", taskID)
	}
	return target, nil
}
