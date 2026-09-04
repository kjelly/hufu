package team

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/tools"
)

type PlanEntry struct {
	TodoID string
	// PlanRevisionID marks a durable plan-revision review. Such entries use
	// the same reviewer agent as plan-first execution, but approval only
	// records the review decision; it must never execute a task as a side
	// effect of reviewing a revision.
	PlanRevisionID string
	ReviewReason   string
	Agent          string
	Goal           string
	PlanText       string
	Status         string // "submitted", "approved", "modified", "rejected"
	ReviewCount    int
	Task           TaskDef
}

const planReviewerMaxReviews = 3

const planReviewerSystemPrompt = `You are a Plan Reviewer. Review agent-submitted execution plans against the original user requirement and the submitting agent's capabilities.

Input:
- SUBMITTING AGENT: The agent that submitted the plan (name, role, description, available tools, skills)
- USER REQUIREMENT: The original task goal
- COMPLETED TASKS: Previously completed tasks with their results (to detect actual duplication)
- PLAN: The agent's proposed execution plan

Rules:
1. APPROVE: The plan is clear, addresses the USER REQUIREMENT, and is not a true duplicate of completed work. Approval is the DEFAULT — only reject when there is clear evidence of a problem.
2. MATCH: Verify the plan is executable by the SUBMITTING AGENT. The agent can ONLY use the tools listed in its profile. Reject plans that require tools the agent does not have.
3. REJECT only when: the plan repeats the EXACT SAME work that was already completed (same deliverable, same file, same outcome) OR requires tools the agent does not have. Do NOT reject because tasks share a category. Provide a SPECIFIC reason.

Creating files, writing documents, generating code — these are legitimate execution plans. APPROVE them.

You MUST call one of:
- approve_plan(todo_id) → execute the plan immediately
- reject_plan(todo_id, reason) → agent re-plans with your feedback

No other response format. Do NOT do the work yourself — only approve or reject.`

// planReviewer implements an autonomous plan review agent using a sidecar model.
// It is NOT user-configurable — it only activates when forcePlanFirst is set.
type planReviewer struct {
	coordinator                    *Coordinator
	modelID                        string
	agent                          fantasy.Agent
	requestDescriptor              contextWindowRequestDescriptor
	providerBoundInvocationContext providerBoundInvocationContext
	initialized                    bool
	todoID                         string
}

func (c *Coordinator) getPlanReviewer(ctx context.Context, todoID string) (*planReviewer, error) {
	ctx = withoutCoordinatorRequestPreflight(ctx)
	modelID := c.planReviewerModel
	ctx, invocation, err := c.resolveProviderBoundInvocationContext(ctx, modelID, nil)
	if err != nil {
		return nil, fmt.Errorf("resolve plan reviewer provider context: %w", err)
	}
	pr := &planReviewer{
		coordinator:                    c,
		modelID:                        modelID,
		todoID:                         todoID,
		providerBoundInvocationContext: invocation,
	}
	reviewerTools := []fantasy.AgentTool{
		&reviewerApprovePlanTool{coordinator: c, todoID: todoID},
		&reviewerRejectPlanTool{coordinator: c, todoID: todoID},
	}
	ag, err := c.createGatedAgent(ctx, c.providerManager.GetProvider(modelID), agent.AgentConfig{
		Def: &agent.AgentDef{
			Name:       "plan-reviewer",
			System:     planReviewerSystemPrompt,
			Role:       "plan_reviewer",
			Generation: agent.GenerationParams{Model: modelID},
		},
		TeamConfig:        &c.session.Config,
		WorkDir:           c.projectDir,
		MaxSteps:          1,
		InvocationModelID: modelID,
		AdmissionContext:  invocation.AdmissionContext,
	}, reviewerTools)
	if err != nil {
		return nil, err
	}
	pr.requestDescriptor = c.newContextWindowRequestDescriptorWithContext(ctx, modelID, &agent.AgentDef{Name: "plan-reviewer", Role: "plan_reviewer", System: planReviewerSystemPrompt, Generation: agent.GenerationParams{Model: modelID}}, reviewerTools, "plan-reviewer", "plan-reviewer")
	pr.agent = ag
	pr.initialized = true
	return pr, nil
}

func (pr *planReviewer) review(ctx context.Context, planText string) (string, bool, error, error) {
	ctx = withoutCoordinatorRequestPreflight(ctx)
	ctx = withProviderBoundInvocationContext(ctx, pr.providerBoundInvocationContext)
	ctx = withContextWindowRequestDescriptor(ctx, pr.requestDescriptor)
	c := pr.coordinator
	c.pendingPlansMu.Lock()
	entry := c.pendingPlans[pr.todoID]
	if entry == nil {
		c.pendingPlansMu.Unlock()
		return "", true, nil, nil
	}
	goal := entry.Goal
	agentName := entry.Agent
	entry.ReviewCount++
	forceApprove := entry.ReviewCount > planReviewerMaxReviews
	c.pendingPlansMu.Unlock()

	if forceApprove {
		jsonResp, ok := tools.TryAskUserTUI(ctx,
			fmt.Sprintf("Plan rejected %d times:\n\nAgent: %s\nGoal: %s\n\nPlan:\n%s\n\nManually approve and execute this plan?",
				entry.ReviewCount, agentName, goal, planText),
			"confirm",
			[]tools.AskUserTUIOption{
				{Label: "Approve and execute", Value: "approve"},
				{Label: "Reject and cancel task", Value: "reject"},
			}, false)

		userApproved := false
		if ok && jsonResp != "" {
			var resp struct {
				Answers []string `json:"answers"`
			}
			if json.Unmarshal([]byte(jsonResp), &resp) == nil && len(resp.Answers) > 0 {
				userApproved = resp.Answers[0] == "approve"
			}
		}

		if userApproved {
			approved := c.autoApprovePlan(ctx, pr.todoID)
			c.pendingPlansMu.Lock()
			actualErr := c.approvedErrors[pr.todoID]
			delete(c.approvedOutputs, pr.todoID)
			delete(c.approvedErrors, pr.todoID)
			c.pendingPlansMu.Unlock()
			return approved, true, actualErr, nil
		}

		if err := c.rejectPlanOnExistingTodo(ctx, pr.todoID, "rejected by user after 3+ plan reviews"); err != nil {
			return "", true, nil, err
		}
		c.pendingPlansMu.Lock()
		delete(c.pendingPlans, pr.todoID)
		c.pendingPlansMu.Unlock()
		c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
		msg := fmt.Sprintf("Task skipped: user manually rejected the plan for agent %q after %d review cycles. Do not retry this task.", agentName, entry.ReviewCount)
		return msg, true, nil, nil
	}

	taskStatus := c.buildTaskStatusContext()
	agentDef, _, agentResolveErr := c.AgentPool().ResolveAgentName(agentName)
	var agentInfo string
	if agentResolveErr != nil || agentDef == nil {
		agentInfo = fmt.Sprintf("Name: %s\n(could not resolve agent definition: %v)", agentName, agentResolveErr)
	} else {
		agentInfo = fmt.Sprintf("Name: %s\nRole: %s\nDescription: %s\nTools: %s\nSkills: %s",
			agentDef.Name, agentDef.Role, agentDef.Description, agentDef.Tools, agentDef.Skills)
	}
	prompt := fmt.Sprintf("## SUBMITTING AGENT\n\n%s\n\n## USER REQUIREMENT\n\n%s\n\n## TASK STATUS\n\n%s\n\n## PLAN\n\n%s", agentInfo, goal, taskStatus, planText)

	c.report(c.newEvent("step").withMessage("plan reviewer evaluating plan").withTodoID(pr.todoID))
	// Plan review has no separate execution receipt. A review invoked from a
	// worker context may inherit the worker's receipt marker, so force direct
	// no-progress token accounting for this auxiliary LLM stream.
	ctx = context.WithValue(ctx, llmUsageReceiptExpectedKey{}, false)
	preparedPrompt, err := c.prepareAuxiliaryPrompt(ctx, "plan_reviewer", prompt)
	if err != nil {
		return "", false, nil, err
	}
	result, _, err := c.runAgentWithStatusAndHistory(ctx, pr.agent, "plan-reviewer", preparedPrompt, nil, &taskTiming{}, fantasy.StepCountIs(1))
	if err != nil {
		return "", false, nil, err
	}

	// Check if the plan was approved and executed during review.
	// autoApprovePlan deletes the pendingPlans entry and stores the real task
	// output in approvedOutputs. Return that output so the coordinator sees the
	// actual result rather than just the plan-reviewer's summary text.
	c.pendingPlansMu.Lock()
	entry = c.pendingPlans[pr.todoID]
	actualOutput := c.approvedOutputs[pr.todoID]
	actualErr := c.approvedErrors[pr.todoID]
	delete(c.approvedOutputs, pr.todoID)
	delete(c.approvedErrors, pr.todoID)
	c.pendingPlansMu.Unlock()
	if actualErr != nil {
		return actualOutput, false, actualErr, nil
	}
	if entry == nil {
		if actualOutput != "" {
			return actualOutput, true, nil, nil
		}
		return result, true, nil, nil
	}

	return result, false, nil, nil
}

func (c *Coordinator) autoApprovePlan(ctx context.Context, todoID string) string {
	c.pendingPlansMu.Lock()
	entry := c.pendingPlans[todoID]
	if entry == nil {
		c.pendingPlansMu.Unlock()
		return ""
	}
	entry.Status = "approved"
	agentName := entry.Agent
	goal := entry.Goal
	c.pendingPlansMu.Unlock()

	output, err := c.executeApprovedPlanOnExistingTodo(ctx, todoID, agentName, goal)
	if err != nil {
		c.pendingPlansMu.Lock()
		if c.approvedErrors == nil {
			c.approvedErrors = make(map[string]error)
		}
		c.approvedErrors[todoID] = err
		c.pendingPlansMu.Unlock()
		return fmt.Sprintf("Plan execution failed: %v", err)
	}
	return output
}

func (c *Coordinator) executeApprovedPlanOnExistingTodo(ctx context.Context, todoID, agentName, goal string) (string, error) {
	if err := c.commitTaskPlanLifecycle(ctx, todoID, true, todoID); err != nil {
		return "", err
	}
	if err := c.commitTaskTransitionFromCurrent(ctx, todoID, TaskPlanned, "", "", nil); err != nil {
		return "", err
	}
	c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
	c.report(c.newEvent("plan_approved").withAgent(agentName).withMessage("plan approved, starting execution").withTodoID(todoID))

	item := c.todoItemByID(todoID)
	if item == nil {
		return "", fmt.Errorf("approved plan todo %s disappeared", todoID)
	}
	approvedTask := taskDefFromTodoItem(item)
	approvedTask.PlanFirst = true
	approvedTask.PlanID = todoID
	output, err := c.executeTask(ctx, approvedTask, todoID)

	// Store the actual output so review() can return it to the coordinator,
	// then clean up the plan entry. This ensures the coordinator receives the
	// real task result rather than the plan-reviewer's summary text.
	c.pendingPlansMu.Lock()
	if c.approvedOutputs == nil {
		c.approvedOutputs = make(map[string]string)
	}
	if c.approvedErrors == nil {
		c.approvedErrors = make(map[string]error)
	}
	c.approvedOutputs[todoID] = output
	if err != nil {
		c.approvedErrors[todoID] = err
	}
	delete(c.pendingPlans, todoID)
	c.pendingPlansMu.Unlock()

	if err != nil {
		return output, err
	}
	// Populate the task cache so duplicate detection in subsequent rounds
	// finds the completed result instead of treating it as a new task.
	c.storeTaskCache(strings.ToLower(agentName), goal, output)
	return output, nil
}

func (c *Coordinator) rejectPlanOnExistingTodo(ctx context.Context, todoID, reason string) error {
	if err := c.commitTaskPlanLifecycle(ctx, todoID, false, ""); err != nil {
		return err
	}
	return c.commitTaskTransitionFromCurrent(ctx, todoID, TaskSkipped, reason, "", nil)
}

type reviewerApprovePlanTool struct {
	coordinator *Coordinator
	todoID      string
}

func (t *reviewerApprovePlanTool) ProviderOptions() fantasy.ProviderOptions {
	return fantasy.ProviderOptions{}
}
func (t *reviewerApprovePlanTool) SetProviderOptions(opts fantasy.ProviderOptions) {}

func (t *reviewerApprovePlanTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        "approve_plan",
		Description: "Approve the plan and execute it immediately.",
		Parameters: map[string]any{
			"todo_id": map[string]any{"type": "string", "description": "The todo ID of the plan to approve."},
		},
		Required: []string{"todo_id"},
	}
}

func (t *reviewerApprovePlanTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	t.coordinator.pendingPlansMu.Lock()
	entry := t.coordinator.pendingPlans[t.todoID]
	if entry != nil && entry.PlanRevisionID != "" {
		entry.Status = "approved"
		t.coordinator.pendingPlansMu.Unlock()
		return fantasy.NewTextResponse("Plan revision approved."), nil
	}
	t.coordinator.pendingPlansMu.Unlock()
	result := t.coordinator.autoApprovePlan(ctx, t.todoID)
	return fantasy.NewTextResponse("Plan approved and executed.\n\n" + result), nil
}

type reviewerRejectPlanTool struct {
	coordinator *Coordinator
	todoID      string
}

func (t *reviewerRejectPlanTool) ProviderOptions() fantasy.ProviderOptions {
	return fantasy.ProviderOptions{}
}
func (t *reviewerRejectPlanTool) SetProviderOptions(opts fantasy.ProviderOptions) {}

func (t *reviewerRejectPlanTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        "reject_plan",
		Description: "Reject the plan with a reason. The agent will re-plan based on your feedback.",
		Parameters: map[string]any{
			"todo_id": map[string]any{"type": "string", "description": "The todo ID of the plan to reject."},
			"reason":  map[string]any{"type": "string", "description": "Why the plan was rejected."},
		},
		Required: []string{"todo_id", "reason"},
	}
}

func (t *reviewerRejectPlanTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var args struct {
		TodoID string `json:"todo_id"`
		Reason string `json:"reason"`
	}
	_ = json.Unmarshal([]byte(call.Input), &args)

	t.coordinator.pendingPlansMu.Lock()
	entry := t.coordinator.pendingPlans[t.todoID]
	if entry == nil {
		t.coordinator.pendingPlansMu.Unlock()
		return fantasy.NewTextErrorResponse("plan not found"), nil
	}
	entry.Status = "rejected"
	if entry.PlanRevisionID != "" {
		entry.ReviewReason = strings.TrimSpace(args.Reason)
		t.coordinator.pendingPlansMu.Unlock()
		return fantasy.NewTextResponse("Plan revision rejected: " + entry.ReviewReason), nil
	}
	t.coordinator.pendingPlansMu.Unlock()

	t.coordinator.report(t.coordinator.newEvent("step").withMessage(fmt.Sprintf("plan %s rejected: %s", t.todoID, args.Reason)).withTodoID(t.todoID))

	if err := t.coordinator.rejectPlanOnExistingTodo(ctx, t.todoID, fmt.Sprintf("plan rejected: %s", args.Reason)); err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	t.coordinator.pendingPlansMu.Lock()
	delete(t.coordinator.pendingPlans, t.todoID)
	t.coordinator.pendingPlansMu.Unlock()
	t.coordinator.report(t.coordinator.newEvent("todos_updated").withTodos(t.coordinator.taskTracker.TodoList().Items()))
	return fantasy.NewTextResponse("Plan rejected. Original task terminalized; submit a new plan if needed."), nil
}

func (c *Coordinator) buildTaskStatusContext() string {
	items := c.taskTracker.TodoList().Items()
	if len(items) == 0 {
		return "No tasks have been delegated yet.\n"
	}

	var done, inProgress, pending, skipped, planned, errored, paused []string
	// Pre-allocate with small capacity to reduce allocations during append.
	// Actual size will grow naturally as needed.
	cap := 1
	if len(items) > 7 {
		cap = len(items) / 7
	}
	done = make([]string, 0, cap)
	inProgress = make([]string, 0, cap)
	pending = make([]string, 0, cap)
	skipped = make([]string, 0, cap)
	planned = make([]string, 0, cap)
	errored = make([]string, 0, cap)
	paused = make([]string, 0, cap)

	for _, item := range items {
		extra := ""
		if failure := FailureDisplayText(item); failure != "" {
			extra = ": " + failure
		} else if detail := TaskDetailDisplayText(item); detail != "" {
			extra = ": " + detail
		}
		// Flatten and cap each line: task outputs previously flowed into the
		// system prompt verbatim, growing it ~10KB after a long run and
		// injecting stray markdown headings into it.
		entry := "  - " + flattenStatusEntry(fmt.Sprintf("%s: %s%s", item.Agent, item.Desc, extra), 220)
		switch item.Status {
		case TaskDone:
			done = append(done, entry)
		case TaskInProgress:
			inProgress = append(inProgress, entry)
		case TaskVerifying:
			inProgress = append(inProgress, entry)
		case TaskPaused:
			paused = append(paused, entry)
		case TaskPending:
			pending = append(pending, entry)
		case TaskSkipped:
			skipped = append(skipped, entry)
		case TaskPlanned:
			planned = append(planned, entry)
		case TaskError, TaskBlocked, TaskProtocolIncomplete:
			errored = append(errored, entry)
		}
	}

	var b strings.Builder
	// Completed tasks first with warning
	if len(done) > 0 {
		fmt.Fprintf(&b, "⚠️ COMPLETED - DO NOT RE-DELEGATE (%d):\n%s\n", len(done), strings.Join(done, "\n"))
	} else {
		b.WriteString("⚠️ COMPLETED - DO NOT RE-DELEGATE (0)\n")
	}
	// Paused tasks (waiting for sub-agent)
	if len(paused) > 0 {
		fmt.Fprintf(&b, "⏸️ PAUSED - Waiting for sub-agent (%d):\n%s\n", len(paused), strings.Join(paused, "\n"))
	}
	if len(inProgress) > 0 {
		fmt.Fprintf(&b, "In Progress (%d):\n%s\n", len(inProgress), strings.Join(inProgress, "\n"))
	} else {
		b.WriteString("In Progress (0)\n")
	}
	if len(pending) > 0 {
		fmt.Fprintf(&b, "Pending (%d):\n%s\n", len(pending), strings.Join(pending, "\n"))
	}
	if len(skipped) > 0 {
		fmt.Fprintf(&b, "Skipped (%d):\n%s\n", len(skipped), strings.Join(skipped, "\n"))
	}
	if len(planned) > 0 {
		fmt.Fprintf(&b, "Planned (%d):\n%s\n", len(planned), strings.Join(planned, "\n"))
	}
	if len(errored) > 0 {
		fmt.Fprintf(&b, "Error (%d):\n%s\n", len(errored), strings.Join(errored, "\n"))
	}
	return b.String()
}

// flattenStatusEntry collapses whitespace (including newlines and markdown
// headings) into single spaces and truncates to maxRunes.
func flattenStatusEntry(s string, maxRunes int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > maxRunes {
		return string(r[:maxRunes]) + "…"
	}
	return s
}
