package team

import (
	"context"
	"fmt"

	"charm.land/fantasy"
)

// Bind protocol handlers only after the static surface and collisions have
// been checked. Candidate validation precedes any repair provider execution.
func (c *Coordinator) appendTaskProtocolTools(ctx context.Context, concrete []fantasy.AgentTool, taskID string, static StaticToolResolution) ([]fantasy.AgentTool, error) {
	concrete = removeToolNames(concrete, submitResultToolName, "submit_plan")
	if static.ResultRequired {
		submit, err := c.newResultSubmissionTool(ctx, taskID, static.ResultOnly)
		if err != nil {
			return nil, err
		}
		concrete = append(concrete, submit)
	}
	if static.PlanRequired {
		concrete = append(concrete, &submitPlanTool{coordinator: c, todoID: taskID})
	}
	return concrete, nil
}

func (c *Coordinator) newResultSubmissionTool(ctx context.Context, taskID string, resultOnly bool) (*submitResultTool, error) {
	submit := &submitResultTool{coordinator: c, todoID: taskID}
	if !resultOnly {
		return submit, nil
	}
	item := c.todoItemByID(taskID)
	if item == nil || item.ResultCandidate == nil {
		return submit, nil
	}
	submit.candidate = cloneResultCandidate(item.ResultCandidate)
	if _, err := c.loadResultCandidate(ctx, item, submit.candidate); err != nil {
		return nil, fmt.Errorf("prepare result candidate: %w", err)
	}
	return submit, nil
}
