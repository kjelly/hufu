package team

import (
	"context"
	"fmt"
	"strings"

	"charm.land/fantasy"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/modelprofile"
)

func (c *Coordinator) filterWorkerToolsForModel(ctx context.Context, modelID string, candidate []fantasy.AgentTool, protocolRequired bool, sequence []string) ([]fantasy.AgentTool, error) {
	if c == nil || c.modelProfileRuntime == nil || strings.TrimSpace(modelID) == "" {
		return candidate, nil
	}
	profile, err := c.modelProfileRuntime.Profile(ctx, modelID, 0, 0)
	if err != nil || profile.SupportsTools != modelprofile.CapabilityNo {
		return candidate, nil
	}
	if protocolRequired {
		return nil, fmt.Errorf("model %q does not support tools required by the worker result protocol", modelID)
	}
	if len(sequence) > 0 {
		return nil, fmt.Errorf("model %q does not support execution tool_sequence %v", modelID, sequence)
	}
	return nil, nil
}

// ModelCapabilityValidation contains advisory diagnostics and hard contract
// failures found before a worker is invoked. Unknown evidence is deliberately
// advisory because provider metadata is best effort.
type ModelCapabilityValidation struct {
	Warnings []string
	Errors   []string
}

// ValidateModelCapabilities checks every model that can be selected for a
// worker, including model-list alternatives, retries, and agent extra models.
// It returns warnings separately so unreachable introspection and unknown
// metadata never become accidental authorization or startup failures.
func (c *Coordinator) ValidateModelCapabilities(ctx context.Context) ModelCapabilityValidation {
	var result ModelCapabilityValidation
	if c == nil || c.session == nil || c.modelProfileRuntime == nil {
		return result
	}

	for _, candidate := range c.modelCapabilityCandidates() {
		requirements := mergeModelRequirements(c.session.Config.Requirements.Model, candidate.def.Requirements.Model)
		if requirements == (agent.ModelRequirements{}) {
			continue
		}
		operatorContext := candidate.def.Generation.ContextWindow
		if operatorContext <= 0 {
			operatorContext = c.session.Config.Generation.ContextWindow
		}
		profile, err := c.modelProfileRuntime.Profile(ctx, candidate.model, operatorContext, c.resolveAgentMaxOutputTokens(candidate.def))
		if err != nil {
			result.Warnings = append(result.Warnings, fmt.Sprintf("model capability warning for agent %q model %q: introspection unavailable: %v", candidate.def.Name, candidate.model, err))
			continue
		}
		checkModelRequirement(&result, candidate.def.Name, candidate.model, profile, requirements)
	}
	return result
}

// ModelCapabilityValidationError is returned when a known model fact
// contradicts a declared requirement. Unknown facts remain warnings.
func (v ModelCapabilityValidation) Err() error {
	if len(v.Errors) == 0 {
		return nil
	}
	return fmt.Errorf("model capability validation failed:\n- %s", strings.Join(v.Errors, "\n- "))
}

type modelCapabilityCandidate struct {
	def   *agent.AgentDef
	model string
}

func (c *Coordinator) modelCapabilityCandidates() []modelCapabilityCandidate {
	seen := make(map[string]bool)
	var candidates []modelCapabilityCandidate
	add := func(def *agent.AgentDef, model string) {
		model = strings.TrimSpace(model)
		if def == nil || model == "" || strings.EqualFold(strings.TrimSpace(def.Role), "coordinator") {
			return
		}
		key := strings.ToLower(def.Name) + "\x00" + strings.ToLower(model)
		if seen[key] {
			return
		}
		seen[key] = true
		candidates = append(candidates, modelCapabilityCandidate{def: def, model: model})
	}

	for _, def := range c.session.Agents {
		if def == nil || strings.EqualFold(strings.TrimSpace(def.Role), "coordinator") {
			continue
		}
		add(def, def.Generation.Model)
		for _, extra := range def.ExtraModels {
			add(def, extra)
		}
		if def.Generation.Model == "" {
			add(def, c.session.Config.Generation.Model)
		}
		for _, entry := range c.modelList {
			add(def, entry.ID)
		}
	}
	return candidates
}

func mergeModelRequirements(team, worker agent.ModelRequirements) agent.ModelRequirements {
	return agent.ModelRequirements{
		Tools:       team.Tools || worker.Tools,
		Attachments: team.Attachments || worker.Attachments,
		Reasoning:   team.Reasoning || worker.Reasoning,
		Temperature: team.Temperature || worker.Temperature,
		MinContext:  max(team.MinContext, worker.MinContext),
	}
}

func checkModelRequirement(result *ModelCapabilityValidation, agentName, model string, profile modelprofile.ModelProfile, requirements agent.ModelRequirements) {
	check := func(name string, required bool, value modelprofile.CapabilityState, source modelprofile.ResolvedValue[modelprofile.CapabilityState]) {
		if !required {
			return
		}
		if value == "" {
			value = modelprofile.CapabilityUnknown
		}
		switch value {
		case modelprofile.CapabilityNo:
			result.Errors = append(result.Errors, fmt.Sprintf("agent %q model %q does not support required %s capability (source: %s)", agentName, model, name, source.Source))
		case modelprofile.CapabilityUnknown:
			result.Warnings = append(result.Warnings, fmt.Sprintf("model capability warning for agent %q model %q: %s capability is unknown", agentName, model, name))
		}
	}
	check("tools", requirements.Tools, profile.SupportsTools, profile.Sources.Capabilities.Tools)
	check("attachments", requirements.Attachments, profile.SupportsAttachments, profile.Sources.Capabilities.Attachments)
	check("reasoning", requirements.Reasoning, profile.SupportsReasoning, profile.Sources.Capabilities.Reasoning)
	check("temperature", requirements.Temperature, profile.SupportsTemperature, profile.Sources.Capabilities.Temperature)
	if requirements.MinContext <= 0 {
		return
	}
	if profile.EffectiveContext <= 0 {
		result.Warnings = append(result.Warnings, fmt.Sprintf("model capability warning for agent %q model %q: effective context is unknown (required at least %d)", agentName, model, requirements.MinContext))
	} else if profile.EffectiveContext < requirements.MinContext {
		result.Errors = append(result.Errors, fmt.Sprintf("agent %q model %q has effective context %d, below required minimum %d (source: %s)", agentName, model, profile.EffectiveContext, requirements.MinContext, profile.Sources.EffectiveContext.Source))
	}
}

// selectCapabilityAwareModel retains the complexity-selected model whenever
// it is compatible. If it is known incompatible, it chooses the first
// statically configured alternative that satisfies the requirement. Unknown
// alternatives remain eligible and therefore preserve warning-tolerant
// behavior when introspection is unavailable.
func (c *Coordinator) selectCapabilityAwareModel(task TaskDef, def *agent.AgentDef, selected string) string {
	if c == nil || def == nil || selected == "" {
		return selected
	}
	if c.session == nil {
		return selected
	}
	requirements := mergeModelRequirements(c.session.Config.Requirements.Model, def.Requirements.Model)
	if requirements == (agent.ModelRequirements{}) {
		return selected
	}
	if c.modelSatisfiesRequirements(context.Background(), selected, requirements) {
		return selected
	}
	for _, entry := range c.modelList {
		if c.modelSatisfiesRequirements(context.Background(), entry.ID, requirements) {
			return entry.ID
		}
	}
	return selected
}

func (c *Coordinator) modelSatisfiesRequirements(ctx context.Context, modelID string, requirements agent.ModelRequirements) bool {
	if c == nil || c.modelProfileRuntime == nil || strings.TrimSpace(modelID) == "" {
		return true
	}
	profile, err := c.modelProfileRuntime.Profile(ctx, modelID, 0, 0)
	if err != nil {
		return true
	}
	if requirements.Tools && profile.SupportsTools == modelprofile.CapabilityNo ||
		requirements.Attachments && profile.SupportsAttachments == modelprofile.CapabilityNo ||
		requirements.Reasoning && profile.SupportsReasoning == modelprofile.CapabilityNo ||
		requirements.Temperature && profile.SupportsTemperature == modelprofile.CapabilityNo {
		return false
	}
	return requirements.MinContext <= 0 || profile.EffectiveContext <= 0 || profile.EffectiveContext >= requirements.MinContext
}
