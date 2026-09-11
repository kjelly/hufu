package team

// Dry-run planning: predicting delegations without executing them.

import (
	"context"
	"strings"

	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/execution"
)

type DryRunAgentInfo struct {
	Name            string
	Role            string
	Model           string
	ExecutionTarget string
	Tools           []string
	Skills          []string
}

type DryRunSkillInfo struct {
	Name        string
	Description string
}

type DryRunResult struct {
	UserPrompt         string
	TeamName           string
	Model              string
	SidecarModel       string
	WorkerTarget       string
	CoordinatorTarget  string
	SidecarTarget      string
	GuardTarget        string
	JudgeTarget        string
	PlanReviewerTarget string
	ResolvedProfile    ExecutionProfile
	Agents             []DryRunAgentInfo
	AllSkills          []DryRunSkillInfo
	MatchedSkillNames  []string
	OrchestratorPrompt string
	FirstRoundTasks    []TaskDef
	ContractFindings   []ContractFinding
	Error              string
}

func (c *Coordinator) DryRun(ctx context.Context, userPrompt string) (*DryRunResult, error) {
	orchDef := c.GetOrchestratorDef()

	result := &DryRunResult{
		UserPrompt:      userPrompt,
		ResolvedProfile: c.ExecutionProfile(),
	}
	if c.session != nil && c.session.Config.Name != "" {
		result.TeamName = c.session.Config.Name
	}
	if c.session != nil {
		result.WorkerTarget = canonicalDryRunTarget(c.session.Config.WorkerModel, c.session.Config.DefaultLLMBackend)
		result.CoordinatorTarget = canonicalDryRunTarget(c.session.Config.CoordinatorModel, c.session.Config.DefaultLLMBackend)
		result.SidecarTarget = canonicalDryRunTarget(c.session.Config.SidecarModel, c.session.Config.DefaultLLMBackend)
		result.GuardTarget = canonicalDryRunTarget(c.session.Config.GuardModel, c.session.Config.DefaultLLMBackend)
		result.JudgeTarget = canonicalDryRunTarget(c.session.Config.JudgeModel, c.session.Config.DefaultLLMBackend)
		result.PlanReviewerTarget = canonicalDryRunTarget(c.session.Config.PlanReviewerModel, c.session.Config.DefaultLLMBackend)
	}
	if orchDef != nil {
		result.Model = c.resolveAgentModel(orchDef, "")
	}

	if c.session != nil {
		result.ContractFindings = LintTeamContracts(c.session)
		if c.session.Config.SidecarModel != "" {
			result.SidecarModel = c.session.Config.SidecarModel
		}
		if result.SidecarModel == "" {
			if resolved := config.LoadConfig(); resolved != nil {
				result.SidecarModel = resolved.ResolveSidecarModel(c.session.Config.SidecarModel)
			}
		}
	}

	// Skill matching: keyword-only, no LLM, no sidecar.
	allSkills := c.getSkills()
	matchedSet := map[string]bool{}
	for _, sk := range allSkills {
		if strings.Contains(strings.ToLower(userPrompt), strings.ToLower(sk.Name)) || SkillMatchesPrompt(sk, userPrompt) {
			matchedSet[strings.ToLower(sk.Name)] = true
		}
		result.AllSkills = append(result.AllSkills, DryRunSkillInfo{
			Name:        sk.Name,
			Description: sk.Description,
		})
	}
	for _, sk := range allSkills {
		if matchedSet[strings.ToLower(sk.Name)] {
			result.MatchedSkillNames = append(result.MatchedSkillNames, sk.Name)
		}
	}

	// Agent listing: derived from session config, not from an LLM.
	// Dedupe by def.Name so any agent registered under multiple map keys
	// (e.g. legacy aliases) still appears only once in the dry-run output.
	if c.session != nil {
		seenAgents := map[string]bool{}
		for _, def := range c.session.Agents {
			if def == nil {
				continue
			}
			if seenAgents[def.Name] {
				continue
			}
			seenAgents[def.Name] = true
			role := def.Role
			if role == "" {
				role = "worker"
			}
			model := c.resolveAgentModel(def, "")
			var tools []string
			if def.Tools != "" {
				tools = strings.Split(def.Tools, ",")
				for i, t := range tools {
					tools[i] = strings.TrimSpace(t)
				}
			}
			var skills []string
			if def.Skills != "" {
				skills = strings.Split(def.Skills, ",")
				for i, s := range skills {
					skills[i] = strings.TrimSpace(s)
				}
			}
			if role == "coordinator" || role == "orchestrator" {
				tools = []string{"agent", "finish", "load_skill", "save_skill", "ask_user"}
			}
			result.Agents = append(result.Agents, DryRunAgentInfo{
				Name:            def.Name,
				Role:            role,
				Model:           model,
				ExecutionTarget: canonicalDryRunTarget(model, c.session.Config.DefaultLLMBackend),
				Tools:           tools,
				Skills:          skills,
			})
		}
	}

	c.report(c.newEvent("done").withAgent("coordinator").withMessage("dry-run complete (no LLM calls)").withTodoID(CoordTodoID))

	return result, nil
}

func canonicalDryRunTarget(raw, defaultBackend string) string {
	selector, err := execution.ParseExecutionSelector(raw)
	if err != nil || selector.Model == "" {
		return raw
	}
	backend := selector.Backend
	if backend == "" {
		backend = execution.CanonicalTargetBackendName(defaultBackend)
		if backend == "" {
			backend = execution.OllamaBackendName
		}
	}
	return (execution.ExecutionTarget{Backend: backend, Model: selector.Model}).String()
}

func cloneTaskDef(td TaskDef) TaskDef {
	clone := td
	clone.Execution = cloneExecutionContract(td.Execution)
	clone.ModelTopology = cloneModelTopology(td.ModelTopology)
	clone.Action = cloneActionPtr(td.Action)
	clone.FanOut = cloneFanOutSpec(td.FanOut)
	clone.DecisionFacts = cloneDecisionFacts(td.DecisionFacts)
	clone.DecisionArtifacts = append([]ArtifactRef(nil), td.DecisionArtifacts...)
	clone.DecisionBaseRates = cloneBaseRateEvidence(td.DecisionBaseRates)
	clone.DecisionAssumptions = cloneDecisionAssumptions(td.DecisionAssumptions)
	clone.DecisionProvenance = cloneEvidenceProvenance(td.DecisionProvenance)
	if td.ContextFiles != nil {
		clone.ContextFiles = make([]string, len(td.ContextFiles))
		copy(clone.ContextFiles, td.ContextFiles)
	}
	if td.DependsOn != nil {
		clone.DependsOn = make([]int, len(td.DependsOn))
		copy(clone.DependsOn, td.DependsOn)
	}
	if td.Requires != nil {
		clone.Requires = make([]string, len(td.Requires))
		copy(clone.Requires, td.Requires)
	}
	if td.ResourceClaims != nil {
		clone.ResourceClaims = append([]string(nil), td.ResourceClaims...)
	}
	if td.Resources != nil {
		clone.Resources = append([]ResourceClaim(nil), td.Resources...)
	}
	clone.VerifySpec = cloneVerificationSpecPtr(td.VerifySpec)
	clone.RecoveryHypothesis = cloneRecoveryHypothesis(td.RecoveryHypothesis)
	return clone
}

func cloneDecisionFacts(facts map[string]any) map[string]any {
	if facts == nil {
		return nil
	}
	clone := make(map[string]any, len(facts))
	for key, value := range facts {
		clone[key] = cloneDecisionValue(value)
	}
	return clone
}

func cloneDecisionValue(value any) any {
	switch value := value.(type) {
	case map[string]any:
		return cloneDecisionFacts(value)
	case []any:
		clone := make([]any, len(value))
		for i, item := range value {
			clone[i] = cloneDecisionValue(item)
		}
		return clone
	default:
		return value
	}
}

func cloneBaseRateEvidence(rates []BaseRateEvidence) []BaseRateEvidence {
	if rates == nil {
		return nil
	}
	clone := make([]BaseRateEvidence, len(rates))
	copy(clone, rates)
	for i := range clone {
		clone[i].Source = rates[i].Source
		clone[i].Limitations = append([]string(nil), rates[i].Limitations...)
	}
	return clone
}

func cloneDecisionAssumptions(assumptions []DecisionAssumption) []DecisionAssumption {
	if assumptions == nil {
		return nil
	}
	clone := make([]DecisionAssumption, len(assumptions))
	copy(clone, assumptions)
	for i := range clone {
		clone[i].EvidenceRefs = append([]ArtifactRef(nil), assumptions[i].EvidenceRefs...)
	}
	return clone
}

func cloneEvidenceProvenance(provenance []EvidenceProvenance) []EvidenceProvenance {
	if provenance == nil {
		return nil
	}
	clone := make([]EvidenceProvenance, len(provenance))
	copy(clone, provenance)
	for i := range clone {
		clone[i].ParentSourceIDs = append([]string(nil), provenance[i].ParentSourceIDs...)
		clone[i].DeclaredParentSourceIDs = append([]string(nil), provenance[i].DeclaredParentSourceIDs...)
	}
	return clone
}

func cloneActionPtr(action *Action) *Action {
	if action == nil {
		return nil
	}
	clone := *action
	return &clone
}
