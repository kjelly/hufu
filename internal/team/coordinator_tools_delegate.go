package team

// Agent-to-agent delegation: the request_agent tool, delegation-chain depth
// limits, and sidecar-assisted agent selection.

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/sidecar"
	"github.com/kjelly/hufu/internal/utils"
)

const maxDelegationDepth = 5

// delegationChain returns the ancestor chain of agent names that led to the
// current request_agent call. The chain is propagated through the context
// (see delegationChainKey) rather than the coordinator's mutable snapshot,
// since the snapshot only ever holds the single currently-running agent's
// flat name and gets overwritten on every nested agent run.
func delegationChain(ctx context.Context, callerName string) []string {
	if raw, ok := ctx.Value(delegationChainKey{}).(string); ok && raw != "" {
		return strings.Split(raw, "/")
	}
	if callerName == "" {
		return nil
	}
	return []string{callerName}
}

// checkDelegationLimits rejects a delegation to selected if it would exceed
// the maximum chain depth or would re-introduce an agent already present in
// the chain (a delegation cycle).
func checkDelegationLimits(chain []string, selected string) error {
	if len(chain) >= maxDelegationDepth {
		return fmt.Errorf("maximum delegation depth (%d) reached to prevent infinite recursion", maxDelegationDepth)
	}
	for _, a := range chain {
		if strings.EqualFold(a, selected) {
			return fmt.Errorf("circular delegation detected: agent '%s' is already in the delegation chain (%s)", selected, strings.Join(chain, "/"))
		}
	}
	return nil
}

type requestAgentTool struct {
	coordToolBase
	coordinator *Coordinator
}

func (t *requestAgentTool) Info() fantasy.ToolInfo {
	// Built fresh on every call (Info takes no ctx/caller identity, so this
	// can't exclude "self" — a real run still hit two self-delegation
	// attempts even with the hint below). The enum is the concrete fix: it
	// stops the model from inventing agent names that were never valid (a
	// real run once tried delegating to "exec", which does not exist).
	agentDesc := "Name of the specific agent to assign this task to. If omitted, the best available agent is selected automatically. You cannot delegate to yourself (the agent making this call) — the coordinator rejects that as a delegation cycle."
	agentParam := map[string]any{
		"type":        "string",
		"description": agentDesc,
	}
	if workers := t.coordinator.uniqueWorkerDefs(); len(workers) > 0 {
		names := make([]string, 0, len(workers))
		for _, w := range workers {
			names = append(names, w.Name)
		}
		agentParam["enum"] = names
	}
	return fantasy.ToolInfo{
		Name:        "request_agent",
		Description: "Request the coordinator to delegate a task to another agent. Describe what needs to be done (goal) and any constraints. The coordinator will select the best agent and return the result. You are paused until the result is ready.",
		Parameters: map[string]any{
			"goal": map[string]any{
				"type":        "string",
				"description": "The goal of the task — what should be achieved",
			},
			"constraints": map[string]any{
				"type":        "string",
				"description": "Non-obvious restrictions the sub-agent must respect",
			},
			"agent": agentParam,
		},
		Required: []string{"goal"},
	}
}

func (t *requestAgentTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var args struct {
		Goal        string `json:"goal"`
		Constraints string `json:"constraints"`
		Agent       string `json:"agent"`
	}
	if err := json.Unmarshal([]byte(call.Input), &args); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid arguments: %v", err)), nil
	}
	if args.Goal == "" {
		return fantasy.NewTextErrorResponse("goal is required"), nil
	}

	callerName := t.coordinator.getSnapshotField(func(s *currentSnapshot) string { return s.Agent })
	parentID := t.coordinator.getSnapshotField(func(s *currentSnapshot) string { return s.TodoID })

	taskDesc := args.Goal
	if args.Constraints != "" {
		taskDesc += "\nconstraints: " + args.Constraints
	}

	c := t.coordinator

	var selected string
	if args.Agent != "" {
		def, _, err := c.AgentPool().ResolveAgentName(args.Agent)
		if err != nil {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("unknown agent %q: %v", args.Agent, err)), nil
		}
		selected = def.Name
	} else {
		var err error
		selected, err = c.selectAgentForGoal(ctx, args.Goal)
		if err != nil {
			return fantasy.NewTextErrorResponse(fmt.Sprintf("could not select agent: %v", err)), nil
		}
	}

	chainAgents := delegationChain(ctx, callerName)
	if err := checkDelegationLimits(chainAgents, selected); err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}

	subLabel := strings.Join(append(chainAgents, selected), "/")
	agentKey := strings.ToLower(selected)
	if match := c.findExistingTodoDuplicate(ctx, strings.ToLower(subLabel), taskDesc, nil, "", ""); match != nil {
		msg := fmt.Sprintf("[SUPPRESSED DUPLICATE] %s\n\nExisting task %s is already handling this work (status: %s).", match.Reason, match.Item.ID, match.Item.Status)
		if failure := FailureDisplayText(match.Item); failure != "" {
			msg += "\n\nFailure:\n" + utils.TruncateString(failure, 1500)
		} else if detail := TaskDetailDisplayText(match.Item); detail != "" {
			msg += "\n\nDetail:\n" + utils.TruncateString(detail, 500)
		}
		return fantasy.NewTextResponse(msg), nil
	}
	if cachedOutput, cachedDesc, ok := c.lookupTaskCacheAllGenerationsWithVerify(ctx, agentKey, taskDesc, ""); ok {
		log.Printf("[INFO] request_agent cache hit: agent=%q, task=%q, matched=%q", selected, taskDesc, cachedDesc)
		return fantasy.NewTextResponse(fmt.Sprintf("[CACHED RESULT] Task: '%s'\n\n%s", truncateTaskDesc(cachedDesc), cachedOutput)), nil
	}

	todoItems, err := c.CommitTaskCreation(ctx, []TodoSpec{{Agent: subLabel, Desc: taskDesc, Model: "", Source: TaskSourceSubagent, ParentID: parentID}})
	if err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	// Sub-agent creation is another real task-creation boundary for the
	// no-progress budget; it is not covered by coordinator ExecuteTasks.
	c.recordNoProgressTasks(len(todoItems))
	subTodoID := todoItems[0].ID

	if err := c.commitTaskTransitionFromCurrent(ctx, subTodoID, TaskInProgress, "", "", nil); err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	if parentID != "" {
		if err := c.commitTaskTransitionFromCurrent(ctx, parentID, TaskPaused, "", "", nil); err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
	}
	c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))
	c.report(c.newEvent("start").withAgent(subLabel).withMessage(taskDesc).withTodoID(subTodoID))

	// Inject subTodoID so events from runAgentWithStatusAndHistory attribute to the right item.
	execCtx := context.WithValue(ctx, todoIDKey{}, subTodoID)
	execCtx = context.WithValue(execCtx, delegationChainKey{}, subLabel)
	output, err := c.ExecuteSubAgent(execCtx, selected, args.Goal, args.Constraints)
	if err != nil {
		c.PersistFailureWithClass(subLabel, taskDesc, subTodoID, c.FailureDetail(err, FailureSourceError), RetryNone, FailureExecution)
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}

	c.storeTaskCache(agentKey, taskDesc, output)

	if parentID != "" {
		if err := c.commitTaskTransitionFromCurrent(ctx, parentID, TaskInProgress, "", "", nil); err != nil {
			return fantasy.NewTextErrorResponse(err.Error()), nil
		}
	}
	if err := c.commitTaskTransitionFromCurrent(ctx, subTodoID, TaskDone, utils.TruncateRunes(output, summaryMaxRunes), output, nil); err != nil {
		return fantasy.NewTextErrorResponse(err.Error()), nil
	}
	c.report(c.newEvent("todos_updated").withTodos(c.taskTracker.TodoList().Items()))

	return fantasy.NewTextResponse(output), nil
}

func (c *Coordinator) selectAgentForGoal(ctx context.Context, goal string) (string, error) {
	s := c.AgentPool().Sidecar()
	workers := c.uniqueWorkerDefs()
	if len(workers) == 0 {
		return "", fmt.Errorf("no workers available")
	}
	if len(workers) == 1 {
		return workers[0].Name, nil
	}

	var workersList strings.Builder
	for _, w := range workers {
		fmt.Fprintf(&workersList, "- %s", w.Name)
		if w.Description != "" {
			fmt.Fprintf(&workersList, ": %s", w.Description)
		}
		if w.Tools != "" {
			fmt.Fprintf(&workersList, " (tools: %s)", w.Tools)
		}
		workersList.WriteString("\n")
	}

	if s != nil {
		prompt := fmt.Sprintf("Select the single best agent name for this task:\n\nGoal: %s\n\nAvailable agents:\n%s\nReturn ONLY the agent name.", goal, workersList.String())
		selection, err := s.Execute(sidecar.WithPurpose(ctx, "agent_matcher"), prompt)
		if err == nil {
			selection = strings.TrimSpace(selection)
			for _, w := range workers {
				if strings.EqualFold(w.Name, selection) {
					return w.Name, nil
				}
			}
		}
	}
	_ = c.recordAuxiliaryFallback(ctx, "agent_matcher", "deterministic_fallback")

	for _, w := range workers {
		if strings.Contains(strings.ToLower(w.Description), "helper") || strings.Contains(strings.ToLower(w.Name), "helper") {
			return w.Name, nil
		}
	}
	return workers[0].Name, nil
}

func (c *Coordinator) ExecuteSubAgent(ctx context.Context, name string, task string, constraints string) (string, error) {
	if c.IsWrapUp() {
		return "", fmt.Errorf("wrap-up in progress: cannot create sub-agent")
	}

	agentDef, _, err := c.AgentPool().ResolveAgentName(name)
	if err != nil {
		// No silent fallback to a generic worker: a fabricated sub-agent runs
		// with the caller's own permissions and cannot provide any capability
		// the caller lacks, so the request would fail in a confusing way later.
		return "", fmt.Errorf("cannot create sub-agent: %w", err)
	}

	// Derive the sub-agent allowlist from the exact tool slice it receives,
	// rather than inheriting the caller's permissions or re-selecting later.
	agentTools := c.selectWorkerTools(agentDef)
	ctx = c.withEffectiveToolsAllowed(ctx, agentDef, agentToolNames(agentTools))
	// Sub-agent streams do not produce a separate execution receipt. Make the
	// usage-accounting boundary explicit even when the parent worker context
	// carried the receipt marker.
	ctx = context.WithValue(ctx, llmUsageReceiptExpectedKey{}, false)

	subAgModelID := c.resolveAgentModel(agentDef, "")
	ag, err := c.createGatedAgent(ctx, c.providerManager.GetProvider(subAgModelID), agent.AgentConfig{
		Def:        agentDef,
		TeamConfig: &c.session.Config,
		WorkDir:    c.projectDir,
		MaxSteps:   c.stepBudget(agentDef, agent.DefaultMaxSteps),
	}, agentTools)
	if err != nil {
		return "", fmt.Errorf("failed to create sub-agent %q: %w", name, err)
	}

	todoID, _ := ctx.Value(todoIDKey{}).(string)
	granted := toolNameSet(agentToolNames(agentTools))
	taskDef := TaskDef{Agent: agentDef.Name, Goal: task, Constraints: constraints}
	request := c.newTaskContextRequest(taskDef, todoID, 1, ContextTriggerTaskDispatch, agentDef.Name, agentDef.Role, nil)
	instructions := "Complete the delegated goal and return a concise result to the requesting agent.\n" + c.sharedKnowledgeInstructions(granted)
	skills, skillErr := c.buildSkillContextItems(agentDef, agentDef.Name, task, todoID, granted)
	if skillErr != nil {
		return "", fmt.Errorf("sub-agent skill context preflight failed: %w", skillErr)
	}
	workerInput := buildWorkerContextInput(request, taskDef, agentDef, "", instructions, "", "", skills)
	workerInput.ModelContext = globalRegistry.GetSpec(c.resolveAgentModel(agentDef, "")).WithEffectiveMaxOutputTokens(c.resolveAgentMaxOutputTokens(agentDef))
	workerInput.MaxAuxChars = maxWorkerAuxContextChars
	workerInput.DisableMemory = c.historicalMemoryDisabled()
	var routeDecisions []ContextRouteDecision
	if !workerInput.DisableMemory {
		bundle, decisions, canonical, routeErr := c.canonicalContextBundleForRequest(ctx, request)
		if routeErr != nil {
			return "", fmt.Errorf("sub-agent context routing preflight failed: %w", routeErr)
		}
		if canonical {
			workerInput.CanonicalMemory = bundle
			routeDecisions = decisions
		} else {
			workerInput.RawSTM = LoadSTM(c.session.Workspace)
			workerInput.RawLTM = LoadLTM(c.session.Workspace, c.session.Config.Name)
			workerInput.MemoryStore = c.memoryStore
		}
	}
	workerInput.WorkerMemory = c.recallWorkerMemory(ctx, agentDef, request.RetrievalQuery())
	compiled, compileErr := c.ContextCompiler().CompileWorkerContext(ctx, workerInput)
	if compileErr != nil {
		return "", fmt.Errorf("sub-agent context preflight failed: %w", compileErr)
	}
	if strings.TrimSpace(compiled.Prompt) == "" {
		return "", fmt.Errorf("sub-agent context preflight produced an empty prompt")
	}
	manifest := BuildContextInjectionManifest(request, compiled, routeDecisions, agentDef.Name, time.Now().UTC())
	if err := c.persistContextManifest(&manifest); err != nil {
		return "", fmt.Errorf("sub-agent context manifest preflight failed: %w", err)
	}
	prompt := compiled.Prompt

	timing := &taskTiming{}
	timing.reset()

	output, _, err := c.runAgentWithStatusAndHistory(ctx, ag, name, prompt, nil, timing)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(output) == "" {
		return "", fmt.Errorf("sub-agent %q finished without a final message; the requester received no result", name)
	}
	return output, nil
}
