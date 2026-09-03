package team

import (
	"context"
	"encoding/json"

	"github.com/kjelly/hufu/internal/agent"
	"github.com/kjelly/hufu/internal/config"
)

// TaskComplexityProfile contains provider-neutral signals used to choose a
// relative model tier. Model names and provider families are deliberately not
// part of the profile: model-list order is the only strength contract.
type TaskComplexityProfile struct {
	ContextTokens        int
	StepCount            int
	MutationSteps        int
	RepairHistory        int
	DependencyCount      int
	RequiresVerification bool
}

// Score returns a stable relative complexity score. The weights favor
// mutation safety, multi-step dataflow, and prior repair over prompt length.
func (p TaskComplexityProfile) Score() int {
	score := 0
	switch {
	case p.ContextTokens >= 16000:
		score += 3
	case p.ContextTokens >= 4000:
		score += 2
	case p.ContextTokens >= 1000:
		score++
	}
	if p.StepCount >= 8 {
		score += 3
	} else if p.StepCount >= 4 {
		score += 2
	} else if p.StepCount >= 2 {
		score++
	}
	if p.MutationSteps > 0 {
		score += 2
	}
	if p.MutationSteps > 1 {
		score++
	}
	if p.RequiresVerification {
		score++
	}
	if p.DependencyCount > 1 {
		score++
	}
	if p.RepairHistory > 0 {
		score += 2 + p.RepairHistory
	}
	return score
}

// SelectModelForComplexity chooses an ID from a weakest-to-strongest model
// list without interpreting provider or model names. Empty means no selection
// can be made and the caller should use its normal agent/team default.
func SelectModelForComplexity(models []config.ModelEntry, profile TaskComplexityProfile) string {
	if len(models) == 0 {
		return ""
	}
	score := profile.Score()
	index := 0
	if score >= 7 {
		index = len(models) - 1
	} else if score >= 3 {
		index = len(models) / 2
	}
	return models[index].ID
}

func taskComplexityProfile(task TaskDef) TaskComplexityProfile {
	contextChars := len(task.Goal) + len(task.Constraints)
	for _, path := range task.ContextFiles {
		contextChars += len(path)
	}
	profile := TaskComplexityProfile{
		ContextTokens:        contextChars / 4,
		DependencyCount:      len(task.DependsOn),
		RequiresVerification: task.Verify != "" || task.VerifySpec != nil || task.Execution.RequiresVerification,
	}
	if len(task.Execution.Steps) > 0 {
		profile.StepCount = len(task.Execution.Steps)
		for _, step := range task.Execution.Steps {
			if step.Effect == ExecutionEffectMutate {
				profile.MutationSteps++
			}
		}
	} else {
		profile.StepCount = len(task.Execution.ToolSequence)
	}
	if profile.MutationSteps == 0 {
		switch task.SideEffect {
		case SideEffectWorkspaceWrite, SideEffectExternalWrite, SideEffectInfraMutation, SideEffectCredential:
			profile.MutationSteps = 1
		}
	}
	return profile
}

func (c *Coordinator) selectTaskModel(task TaskDef, defs ...*agent.AgentDef) string {
	if task.Model != "" || len(c.modelList) == 0 {
		return task.Model
	}
	profile := taskComplexityProfile(task)
	contextChars := 0
	if len(defs) > 0 && defs[0] != nil {
		contextChars += len(defs[0].System)
	}
	if c != nil && c.session != nil {
		for _, path := range task.ContextFiles {
			if content, err := readShared(c.session.Workspace, path); err == nil {
				contextChars += len(content)
			}
		}
		contextChars += len(c.loadProjectContext())
		if !c.historicalMemoryDisabled() {
			if bundle, canonical, err := c.canonicalContextBundle(context.Background()); err == nil && canonical {
				for _, item := range append(bundle.SharedSession, bundle.SharedPersistent...) {
					contextChars += len(item.Content)
				}
			} else {
				contextChars += len(LoadSTM(c.session.Workspace)) + len(LoadLTM(c.session.Workspace, c.session.Config.Name))
			}
		}
	}
	profile.ContextTokens += contextChars / 4
	var def *agent.AgentDef
	if len(defs) > 0 {
		def = defs[0]
	}
	return c.selectCapabilityAwareModel(task, def, SelectModelForComplexity(c.modelList, profile))
}

// selectStructuredStepModel chooses a relative capability tier for one
// checkpoint. Deterministic probes and validators intentionally do not inherit
// the complexity of the whole workflow, while producers, mutations, and repair
// attempts retain the broader dataflow/risk signals.
func (c *Coordinator) selectStructuredStepModel(task TaskDef, step ExecutionStep, repairAttempt int, defs ...*agent.AgentDef) string {
	if task.Model != "" || c == nil || len(c.modelList) == 0 {
		return task.Model
	}
	profile := taskComplexityProfile(task)
	switch step.Effect {
	case ExecutionEffectRead, ExecutionEffectValidate, ExecutionEffectVerify:
		profile.StepCount = 1
		profile.MutationSteps = 0
		profile.DependencyCount = len(step.DependsOn)
		profile.RequiresVerification = false
	case ExecutionEffectProduce:
		profile.MutationSteps = 0
		for _, output := range step.Outputs {
			if normalizedExecutionOutputKind(output.Kind) == ExecutionOutputArtifact {
				profile.RequiresVerification = true
				break
			}
		}
		if !profile.RequiresVerification {
			for _, candidate := range task.Execution.Steps {
				if candidate.Effect == ExecutionEffectValidate {
					profile.RequiresVerification = true
					break
				}
			}
		}
	}
	profile.ContextTokens = structuredStepInputSize(step) / 4
	if len(defs) > 0 && defs[0] != nil {
		profile.ContextTokens += len(defs[0].System) / 4
	}
	if repairAttempt > 0 {
		// A failed production/validation cycle is a strong generic signal that
		// the repair needs more reasoning capacity than the original attempt.
		profile.RepairHistory = repairAttempt + 2
	}
	return SelectModelForComplexity(c.modelList, profile)
}

func structuredStepInputSize(step ExecutionStep) int {
	data, err := json.Marshal(step.Input)
	if err != nil {
		return 0
	}
	return len(data)
}
