package team

// Plan lifecycle tools: submit_plan (worker side) and the reviewer's
// approve/modify/reject tools.

import (
	"context"
	"encoding/json"
	"fmt"

	"charm.land/fantasy"
)

type submitPlanTool struct {
	coordinator *Coordinator
	todoID      string
}

func (t *submitPlanTool) ProviderOptions() fantasy.ProviderOptions        { return fantasy.ProviderOptions{} }
func (t *submitPlanTool) SetProviderOptions(opts fantasy.ProviderOptions) {}

func (t *submitPlanTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        "submit_plan",
		Description: "Submit your task execution plan for coordinator review. The plan should be a numbered list of concrete steps with brief descriptions. Do NOT include any execution results — only the plan. After submitting, wait for the coordinator to approve, modify, or reject your plan before executing.",
		Parameters: map[string]any{
			"plan": map[string]any{
				"type":        "string",
				"description": "The task execution plan as a numbered list of steps with descriptions.",
			},
		},
		Required: []string{"plan"},
	}
}

func (t *submitPlanTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var args struct {
		Plan string `json:"plan"`
	}
	if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if args.Plan == "" {
		return fantasy.NewTextErrorResponse("plan is required"), nil
	}
	t.coordinator.pendingPlansMu.Lock()
	existing := t.coordinator.pendingPlans[t.todoID]
	if existing != nil {
		existing.PlanText = args.Plan
		existing.Status = "submitted"
	} else {
		t.coordinator.pendingPlans[t.todoID] = &PlanEntry{
			TodoID:   t.todoID,
			PlanText: args.Plan,
			Status:   "submitted",
		}
	}
	t.coordinator.pendingPlansMu.Unlock()
	if t.coordinator.forcePlanFirst {
		return fantasy.NewTextResponse("Plan submitted. Awaiting review."), nil
	}
	return fantasy.NewTextResponse("Plan submitted. Await coordinator review."), nil
}

type approvePlanTool struct {
	coordinator *Coordinator
}

func (t *approvePlanTool) ProviderOptions() fantasy.ProviderOptions        { return fantasy.ProviderOptions{} }
func (t *approvePlanTool) SetProviderOptions(opts fantasy.ProviderOptions) {}

func (t *approvePlanTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        "approve_plan",
		Description: "Approve a submitted task plan and execute it immediately. The plan must have been submitted by an agent via submit_plan. The agent that submitted the plan will execute the approved plan.",
		Parameters: map[string]any{
			"todo_id": map[string]any{
				"type":        "string",
				"description": "The todo ID of the submitted plan to approve.",
			},
		},
		Required: []string{"todo_id"},
	}
}

func (t *approvePlanTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var args struct {
		TodoID string `json:"todo_id"`
	}
	if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if args.TodoID == "" {
		return fantasy.NewTextErrorResponse("todo_id is required"), nil
	}
	t.coordinator.pendingPlansMu.Lock()
	entry, ok := t.coordinator.pendingPlans[args.TodoID]
	if !ok {
		t.coordinator.pendingPlansMu.Unlock()
		return fantasy.NewTextErrorResponse("plan not found for todo_id: " + args.TodoID), nil
	}
	if entry.Status != "submitted" {
		t.coordinator.pendingPlansMu.Unlock()
		return fantasy.NewTextErrorResponse(fmt.Sprintf("plan already %s", entry.Status)), nil
	}
	entry.Status = "approved"
	var approvedTask TaskDef
	if entry.Task.Agent != "" {
		approvedTask = cloneTaskDef(entry.Task)
	} else {
		approvedTask = TaskDef{
			Agent: entry.Agent,
			Goal:  entry.Goal,
		}
	}
	approvedTask.PlanFirst = true
	approvedTask.PlanID = entry.TodoID
	todoID := entry.TodoID
	t.coordinator.pendingPlansMu.Unlock()

	t.coordinator.taskTracker.TodoList().UpdateStatus(todoID, TaskPlanned, "")
	t.coordinator.report(t.coordinator.newEvent("todos_updated").withTodos(t.coordinator.taskTracker.TodoList().Items()))

	result, err := t.coordinator.ExecuteTasks(ctx, []TaskDef{approvedTask})
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	return fantasy.NewTextResponse(fmt.Sprintf("Plan approved and executed.\n\n%s", result)), nil
}

type modifyPlanTool struct {
	coordinator *Coordinator
}

func (t *modifyPlanTool) ProviderOptions() fantasy.ProviderOptions        { return fantasy.ProviderOptions{} }
func (t *modifyPlanTool) SetProviderOptions(opts fantasy.ProviderOptions) {}

func (t *modifyPlanTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        "modify_plan",
		Description: "Modify a submitted task plan and execute the modified version. Provide the corrected plan steps. The agent will execute the modified plan.",
		Parameters: map[string]any{
			"todo_id": map[string]any{
				"type":        "string",
				"description": "The todo ID of the submitted plan to modify.",
			},
			"plan": map[string]any{
				"type":        "string",
				"description": "The modified task execution plan as a numbered list of steps.",
			},
		},
		Required: []string{"todo_id", "plan"},
	}
}

func (t *modifyPlanTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var args struct {
		TodoID string `json:"todo_id"`
		Plan   string `json:"plan"`
	}
	if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if args.TodoID == "" || args.Plan == "" {
		return fantasy.NewTextErrorResponse("todo_id and plan are required"), nil
	}
	t.coordinator.pendingPlansMu.Lock()
	entry, ok := t.coordinator.pendingPlans[args.TodoID]
	if !ok {
		t.coordinator.pendingPlansMu.Unlock()
		return fantasy.NewTextErrorResponse("plan not found for todo_id: " + args.TodoID), nil
	}
	entry.Status = "modified"
	entry.PlanText = args.Plan
	var modifiedTask TaskDef
	if entry.Task.Agent != "" {
		modifiedTask = cloneTaskDef(entry.Task)
	} else {
		modifiedTask = TaskDef{
			Agent: entry.Agent,
			Goal:  entry.Goal,
		}
	}
	modifiedTask.PlanFirst = true
	modifiedTask.PlanID = entry.TodoID
	todoID := entry.TodoID
	t.coordinator.pendingPlansMu.Unlock()

	t.coordinator.report(t.coordinator.newEvent("step").withMessage(fmt.Sprintf("plan %s modified by coordinator", todoID)))

	result, err := t.coordinator.ExecuteTasks(ctx, []TaskDef{modifiedTask})
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	return fantasy.NewTextResponse(fmt.Sprintf("Plan modified and executed.\n\n%s", result)), nil
}

type rejectPlanTool struct {
	coordinator *Coordinator
}

func (t *rejectPlanTool) ProviderOptions() fantasy.ProviderOptions        { return fantasy.ProviderOptions{} }
func (t *rejectPlanTool) SetProviderOptions(opts fantasy.ProviderOptions) {}

func (t *rejectPlanTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        "reject_plan",
		Description: "Reject a submitted task plan and ask the agent to re-plan. Include the reason for rejection so the agent can improve their plan.",
		Parameters: map[string]any{
			"todo_id": map[string]any{
				"type":        "string",
				"description": "The todo ID of the submitted plan to reject.",
			},
			"reason": map[string]any{
				"type":        "string",
				"description": "Why the plan was rejected. The agent will see this and re-plan accordingly.",
			},
		},
		Required: []string{"todo_id", "reason"},
	}
}

func (t *rejectPlanTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var args struct {
		TodoID string `json:"todo_id"`
		Reason string `json:"reason"`
	}
	if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if args.TodoID == "" || args.Reason == "" {
		return fantasy.NewTextErrorResponse("todo_id and reason are required"), nil
	}
	t.coordinator.pendingPlansMu.Lock()
	entry, ok := t.coordinator.pendingPlans[args.TodoID]
	if !ok {
		t.coordinator.pendingPlansMu.Unlock()
		return fantasy.NewTextErrorResponse("plan not found for todo_id: " + args.TodoID), nil
	}
	entry.Status = "rejected"
	var revisedTask TaskDef
	if entry.Task.Agent != "" {
		revisedTask = cloneTaskDef(entry.Task)
	} else {
		revisedTask = TaskDef{
			Agent: entry.Agent,
		}
	}
	revisedTask.Goal = fmt.Sprintf("%s\n\n## Plan Rejected\nYour previous plan was rejected for the following reason:\n\n%s\n\nPlease re-plan and submit a new plan.", entry.Goal, args.Reason)
	revisedTask.PlanFirst = true
	revisedTask.PlanID = ""
	t.coordinator.pendingPlansMu.Unlock()

	t.coordinator.report(t.coordinator.newEvent("step").withMessage(fmt.Sprintf("plan %s rejected: %s", args.TodoID, args.Reason)))

	result, err := t.coordinator.ExecuteTasks(ctx, []TaskDef{revisedTask})
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	return fantasy.NewTextResponse(fmt.Sprintf("Plan rejected. Agent re-planned.\n\n%s", result)), nil
}
