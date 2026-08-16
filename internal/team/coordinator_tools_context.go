package team

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"charm.land/fantasy"

	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/tools"
	"github.com/kjelly/hufu/internal/utils"
)

const contextToolOutputMaxRunes = 6000

type contextQueryTool struct {
	coordToolBase
	coordinator *Coordinator
}

func (t *contextQueryTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{Name: "context_query", Description: "Query bounded, policy-authorized canonical context for the current task.", Parameters: map[string]any{
		"query": map[string]any{"type": "string"},
	}, Required: []string{"query"}}
}

func (t *contextQueryTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var args struct {
		Query string `json:"query"`
	}
	if json.Unmarshal([]byte(call.Input), &args) != nil || strings.TrimSpace(args.Query) == "" {
		return fantasy.NewTextErrorResponse("query is required"), nil
	}
	request := t.coordinator.contextToolRequest(ctx, args.Query, "", nil)
	request.ActionType = "context_query:" + call.ID
	request.AssignRequestID()
	route, err := t.coordinator.contextRouter().Route(ctx, request)
	if err != nil {
		return fantasy.NewTextErrorResponse("context query failed: " + utils.RedactSecrets(err.Error())), nil
	}
	compiled, err := compileRoutedContextForTool(ctx, t.coordinator, request, route)
	if err != nil {
		return fantasy.NewTextErrorResponse("context query compile failed: " + utils.RedactSecrets(err.Error())), nil
	}
	manifest := BuildContextInjectionManifest(request, compiled, route.Decisions, request.AgentName, time.Now().UTC())
	if err := t.coordinator.persistContextManifest(&manifest); err != nil {
		return fantasy.ToolResponse{}, fmt.Errorf("persist context query manifest: %w", err)
	}
	return fantasy.NewTextResponse(utils.TruncateRunes(utils.RedactSecrets(compiled.Prompt), contextToolOutputMaxRunes)), nil
}

type contextGetTool struct {
	coordToolBase
	coordinator *Coordinator
}

func (t *contextGetTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{Name: "context_get", Description: "Get one policy-visible canonical context item by opaque ID.", Parameters: map[string]any{
		"id": map[string]any{"type": "string"},
	}, Required: []string{"id"}}
}

func (t *contextGetTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var args struct {
		ID string `json:"id"`
	}
	if json.Unmarshal([]byte(call.Input), &args) != nil || strings.TrimSpace(args.ID) == "" {
		return fantasy.NewTextErrorResponse("id is required"), nil
	}
	if t.coordinator == nil || t.coordinator.contextRepo == nil {
		return fantasy.NewTextErrorResponse("canonical context unavailable"), nil
	}
	wantID := strings.TrimPrefix(strings.TrimSpace(args.ID), "context:")
	request := t.coordinator.contextToolRequest(ctx, "context item "+wantID, "", nil)
	request.ActionType = "context_get:" + call.ID
	request.AssignRequestID()
	item, reason, err := t.coordinator.GetAuthorizedContextItem(ctx, request, wantID)
	if err != nil {
		if reason == "" {
			reason = ContextOmittedLifecycle
		}
		if persistErr := t.coordinator.persistContextToolDecision(request, wantID, reason); persistErr != nil {
			return fantasy.NewTextErrorResponse("context authorization unavailable"), nil
		}
		return fantasy.NewTextErrorResponse("context item is not visible: " + string(reason)), nil
	}
	compiled := CompiledContext{Prompt: utils.TruncateRunes(utils.RedactSecrets(item.Content), contextToolOutputMaxRunes), IncludedItems: canonicalCompilerItems([]contextstore.ContextItem{item}, PriorityRelevantLTM, "context_get", false, item.Lifecycle == contextstore.LifecycleCandidate)}
	manifest := BuildContextInjectionManifest(request, compiled, []ContextRouteDecision{{ContextItemID: item.ID, Included: true, Reason: reason}}, request.AgentName, time.Now().UTC())
	if err := t.coordinator.persistContextManifest(&manifest); err != nil {
		return fantasy.ToolResponse{}, fmt.Errorf("persist context get manifest: %w", err)
	}
	if err := t.coordinator.recordContextToolConsulted(&manifest, item.ID); err != nil {
		return fantasy.ToolResponse{}, err
	}
	return fantasy.NewTextResponse(compiled.Prompt), nil
}

func (c *Coordinator) contextToolRequest(ctx context.Context, goal string, trigger ContextTrigger, failure *ContextFailure) ContextRequest {
	if metadata, ok := invocationMetadataFromContext(ctx); ok {
		if trigger == "" {
			trigger = metadata.Trigger
		}
		if trigger == "" {
			trigger = ContextTriggerAuxiliary
		}
		r := ContextRequest{
			SchemaVersion:             ContextRequestSchemaVersion,
			RunID:                     metadata.RunID,
			TaskID:                    metadata.TaskID,
			Attempt:                   metadata.Attempt,
			Goal:                      utils.RedactSecrets(strings.TrimSpace(goal)),
			AgentName:                 metadata.AgentName,
			AgentRole:                 metadata.AgentRole,
			Phase:                     metadata.Phase,
			Trigger:                   trigger,
			Purpose:                   contextPurposeForTrigger(trigger),
			ModelExecutionID:          metadata.ModelExecutionID,
			EnvironmentFingerprint:    metadata.EnvironmentFingerprint,
			ParentTrigger:             metadata.Trigger,
			ParentRequestID:           metadata.ParentRequestID,
			ParentManifestFingerprint: metadata.ParentManifestFingerprint,
			Failure:                   failure,
		}
		if r.RunID == "" {
			r.RunID = c.contextRunID()
		}
		if r.Attempt < 1 {
			r.Attempt = 1
		}
		if r.AgentName == "" {
			r.AgentName, _ = ctx.Value(tools.AgentNameKey).(string)
		}
		if r.AgentRole == "" {
			r.AgentRole = "worker"
		}
		if r.Phase == "" {
			r.Phase = PhaseExecute
		}
		if r.TaskID == "" && trigger == ContextTriggerToolFailure {
			r.TaskID = "unbound-tool-call"
		}
		r.AssignRequestID()
		return r
	}
	todoID, _ := ctx.Value(todoIDKey{}).(string)
	attempt, _ := ctx.Value(executionAttemptKey{}).(int)
	if attempt < 1 {
		attempt = 1
	}
	agentName, _ := ctx.Value(tools.AgentNameKey).(string)
	phase := PhaseExecute
	if c.phaseWorkflow != nil && c.phaseWorkflow.Enabled() {
		phase = c.phaseWorkflow.State()
	}
	if trigger == "" {
		trigger = ContextTriggerAuxiliary
	}
	r := ContextRequest{SchemaVersion: ContextRequestSchemaVersion, RunID: c.executionRunID, TaskID: todoID, Attempt: attempt, Goal: utils.RedactSecrets(strings.TrimSpace(goal)), AgentName: agentName, AgentRole: "worker", Phase: phase, Trigger: trigger, Purpose: contextPurposeForTrigger(trigger), Failure: failure}
	if r.TaskID == "" && trigger == ContextTriggerToolFailure {
		// A tool result can be observed in a deliberately task-less stream
		// (for example a protocol probe). Preserve its bounded recovery
		// attribution as a coordinator-scope manifest instead of rejecting the
		// failure or inventing a Todo item.
		r.TaskID = "unbound-tool-call"
	}
	if r.RunID == "" {
		r.RunID = "run-unknown"
	}
	r.AssignRequestID()
	return r
}

// GetAuthorizedContextItem applies the identical repository scope,
// lifecycle/expiry, typed activation projection, and request eligibility used
// by routed context. It deliberately does not accept a caller-supplied item:
// opaque IDs are lookup keys, never authorization tokens.
func (c *Coordinator) GetAuthorizedContextItem(ctx context.Context, request ContextRequest, id string) (contextstore.ContextItem, ContextDecisionReason, error) {
	if c == nil || c.contextRepo == nil || c.session == nil {
		return contextstore.ContextItem{}, ContextOmittedLifecycle, fmt.Errorf("canonical context unavailable")
	}
	items, err := c.contextRepo.Query(ctx, contextstore.RepositoryQuery{
		Scope:             c.contextScope(),
		Visibility:        contextstore.VisibilityAncestors,
		IncludeCandidates: true,
		Limit:             200,
	})
	if err != nil {
		return contextstore.ContextItem{}, ContextOmittedLifecycle, fmt.Errorf("query canonical context: %w", err)
	}
	for _, item := range items {
		if item.ID != id {
			continue
		}
		item, err = activationItemFromRepository(ctx, c.contextRepo, item)
		if err != nil {
			return contextstore.ContextItem{}, ContextOmittedLifecycle, fmt.Errorf("context activation projection: %w", err)
		}
		eligible, reason, eligibilityErr := EvaluateContextEligibility(item, request, request.RunID, time.Now().UTC())
		if eligibilityErr != nil {
			return contextstore.ContextItem{}, reason, eligibilityErr
		}
		if !eligible {
			return contextstore.ContextItem{}, reason, fmt.Errorf("context item is not eligible")
		}
		return item, reason, nil
	}
	return contextstore.ContextItem{}, ContextOmittedLifecycle, fmt.Errorf("context item not found")
}

func (c *Coordinator) persistContextToolDecision(request ContextRequest, itemID string, reason ContextDecisionReason) error {
	manifest := BuildContextInjectionManifest(request, CompiledContext{}, []ContextRouteDecision{{ContextItemID: itemID, Included: false, Reason: reason}}, request.AgentName, time.Now().UTC())
	manifest.ModelCalled = false
	manifest.Outcome = "authorization_denied"
	manifest.Fingerprint = contextManifestFingerprint(manifest)
	return c.persistContextManifest(&manifest)
}

func compileRoutedContextForTool(ctx context.Context, c *Coordinator, request ContextRequest, route ContextRoute) (CompiledContext, error) {
	input := WorkerContextInput{Request: request, Goal: request.Goal, CanonicalMemory: &route.Bundle, ModelContext: globalRegistry.GetSpec("")}
	return c.ContextCompiler().CompileWorkerContext(ctx, input)
}

func (c *Coordinator) prepareToolFailureRecovery(ctx context.Context, agentName, toolCallID, toolName, toolInput string) (string, error) {
	goal := "Recover from the failed tool call without replaying completed side effects."
	if snapshot := c.current.Load(); snapshot != nil && strings.TrimSpace(snapshot.Task) != "" {
		goal = snapshot.Task
	}
	failure := &ContextFailure{Class: FailureExecution, ErrorClass: "tool_error", ToolName: toolName, ToolInputHash: hashContentKey(utils.RedactSecrets(toolInput)), EvidenceRefs: []string{toolCallID}}
	request := c.contextToolRequest(ctx, goal, ContextTriggerToolFailure, failure)
	request.AgentName = agentName
	request.AssignRequestID()
	route, err := c.contextRouter().Route(ctx, request)
	if err != nil {
		return "", err
	}
	compiled, err := compileRoutedContextForTool(ctx, c, request, route)
	if err != nil {
		return "", err
	}
	manifest := BuildContextInjectionManifest(request, compiled, route.Decisions, agentName, time.Now().UTC())
	if err := c.persistContextManifest(&manifest); err != nil {
		return "", err
	}
	if strings.TrimSpace(compiled.Prompt) == "" {
		return "", nil
	}
	return "## Bounded tool-failure recovery context\n\n" + utils.TruncateRunes(utils.RedactSecrets(compiled.Prompt), contextToolOutputMaxRunes), nil
}
