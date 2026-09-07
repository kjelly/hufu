package team

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
)

// Real capability routing for the REFERENCE stage (spec.md v2 §15; spec2.md
// PR-2; plan.md Stage 8 follow-up).
//
// Every other decision stage still calls the team's judge-model sidecar
// unconditionally (internal/team/decision_runners.go) — that is unchanged by
// this file. Only when a profile sets outside-view.role does this runner
// resolve an authorized, capability-matched concrete worker and invoke it
// directly, with its tools narrowed to read-only, instead of the sidecar.
// The prompt and response contract are identical to the sidecar path
// (referenceEvidencePrompt / decodeReferenceEvidence), so evidence-shape
// validation does not change depending on who produced it.

// referenceRoleReadOnlyToolNames is the fixed intersection ceiling for the
// reference role (spec.md v2 §6 RoleConstraints.ReadOnly; §22 "effective
// tools = base-agent tools ∩ ... ∩ role constraints"). A resolved worker's
// own tools are narrowed to this set regardless of what it declares in its
// own frontmatter — role routing only ever removes capability, never adds it.
var referenceRoleReadOnlyToolNames = map[string]bool{
	"view": true, "read": true, "grep": true, "glob": true, "ls": true, "find": true,
}

// runReferenceEvidenceViaCapabilityRouting implements the capability-routed
// half of coordinatorDecisionRunners.RunReferenceEvidence.
func (r *coordinatorDecisionRunners) runReferenceEvidenceViaCapabilityRouting(ctx context.Context, req ReferenceEvidenceRequest) (ReferenceEvidenceDraft, error) {
	c := r.coordinator
	if c == nil || c.session == nil {
		return ReferenceEvidenceDraft{}, fmt.Errorf("reference role routing requires an active session")
	}
	role := req.RoutingRole

	candidates, err := c.ResolveCapabilityCandidates(ctx, CapabilityQuery{
		Required:  role.RequiredCapabilities,
		Preferred: role.PreferredCapabilities,
	})
	if err != nil {
		return ReferenceEvidenceDraft{}, fmt.Errorf("reference role routing: %w", err)
	}
	var chosen string
	for _, candidate := range candidates {
		if candidate.Score > 0 {
			chosen = candidate.AgentID
			break
		}
	}
	if chosen == "" {
		return ReferenceEvidenceDraft{}, fmt.Errorf(
			"reference role routing: no authorized worker satisfies required capabilities %v", role.RequiredCapabilities)
	}
	def := c.session.Agents[chosen]
	if def == nil {
		return ReferenceEvidenceDraft{}, fmt.Errorf("reference role routing: resolved worker %q is not a configured agent", chosen)
	}

	requestBytes, err := json.Marshal(req)
	if err != nil {
		return ReferenceEvidenceDraft{}, fmt.Errorf("reference evidence request: %w", err)
	}
	prompt := referenceEvidencePrompt(string(requestBytes))

	response, modelID, err := c.invokeReferenceRoleAgent(ctx, def, prompt)
	if err != nil {
		return ReferenceEvidenceDraft{}, fmt.Errorf("reference role invocation of %q: %w", chosen, err)
	}
	c.report(c.newEvent("routing_decision").withMessage(fmt.Sprintf(
		"reference role bound to %q (model %q) for decision %s: required=%v preferred=%v",
		chosen, modelID, req.DecisionID, role.RequiredCapabilities, role.PreferredCapabilities)))

	var decoded ReferenceEvidenceDraft
	if err := decodeReferenceEvidence(response, &decoded); err != nil {
		return ReferenceEvidenceDraft{}, fmt.Errorf("reference evidence: %w", err)
	}
	if err := decoded.Validate(); err != nil {
		return ReferenceEvidenceDraft{}, fmt.Errorf("reference evidence: %w", err)
	}
	return decoded, nil
}

// invokeReferenceRoleAgent runs one bounded, non-TODO invocation of a
// resolved concrete agent for the REFERENCE role, allowing a few read-only
// tool-calling steps before its final answer (research may need them).
func (c *Coordinator) invokeReferenceRoleAgent(ctx context.Context, def *agent.AgentDef, prompt string) (string, string, error) {
	tools := referenceRoleTools(c.selectWorkerToolsForTask(def, TaskDef{Agent: def.Name}))
	maxSteps := def.MaxSteps
	if maxSteps <= 0 {
		maxSteps = referenceRoleDefaultMaxSteps
	}
	return c.invokeCapabilityRoutedAgent(ctx, def, prompt, tools, maxSteps)
}

// referenceRoleDefaultMaxSteps bounds a reference-role invocation when the
// resolved agent declares no MaxSteps of its own. It is deliberately several
// steps, not one: unlike the plan-reviewer's single-turn call, research may
// need a few read-only tool calls before its final answer.
const referenceRoleDefaultMaxSteps = 6

// invokeCapabilityRoutedAgent runs one bounded, non-TODO invocation of a
// resolved concrete agent, the same primitive coordinator_plan.go's
// plan-reviewer and coordinator_run.go's orchestrator calls already use for a
// bounded agent call outside the task/TODO lifecycle. It returns the model ID
// actually invoked so callers can record it for tracing. Shared by every
// capability-routed decision stage (REFERENCE, JUDGE, ...); each caller
// supplies its own tool ceiling and step bound.
func (c *Coordinator) invokeCapabilityRoutedAgent(ctx context.Context, def *agent.AgentDef, prompt string, tools []fantasy.AgentTool, maxSteps int, extraStop ...fantasy.StopCondition) (string, string, error) {
	modelID := strings.TrimSpace(def.Generation.Model)
	if modelID == "" {
		modelID = strings.TrimSpace(c.session.Config.Generation.Model)
	}
	if modelID == "" {
		return "", "", fmt.Errorf("resolved agent %q has no configured model", def.Name)
	}

	ctx, invocation, err := c.resolveProviderBoundInvocationContext(ctx, modelID, def)
	if err != nil {
		return "", "", fmt.Errorf("resolve provider context: %w", err)
	}
	provider := c.providerManager.GetProvider(modelID)
	ag, err := c.createGatedAgent(ctx, provider, agent.AgentConfig{
		Def:               def,
		TeamConfig:        &c.session.Config,
		WorkDir:           c.projectDir,
		MaxSteps:          maxSteps,
		InvocationModelID: modelID,
		AdmissionContext:  invocation.AdmissionContext,
	}, tools)
	if err != nil {
		return "", "", fmt.Errorf("build agent: %w", err)
	}
	timing := &taskTiming{}
	timing.reset()
	output, _, err := c.runAgentWithStatusAndHistory(ctx, ag, def.Name, prompt, nil, timing, extraStop...)
	if err != nil {
		return "", "", err
	}
	return output, modelID, nil
}

// referenceRoleTools narrows a resolved worker's own tools to the reference
// role's read-only ceiling (spec.md v2 §22). This is subtraction, not trust:
// a candidate configured with write/bash/agent tools still executes the
// reference role with only its read-only tools available.
func referenceRoleTools(tools []fantasy.AgentTool) []fantasy.AgentTool {
	out := make([]fantasy.AgentTool, 0, len(tools))
	for _, tool := range tools {
		if tool == nil {
			continue
		}
		if referenceRoleReadOnlyToolNames[normalizedToolName(tool.Info().Name)] {
			out = append(out, tool)
		}
	}
	return out
}
