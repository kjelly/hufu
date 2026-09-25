package team

// Dry-run planning: predicting delegations without executing them.

import (
	"context"
	"fmt"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
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
	UserPrompt           string
	TeamName             string
	Model                string
	WorkerTarget         string
	CoordinatorTarget    string
	SidecarTarget        string
	GuardTarget          string
	JudgeTarget          string
	PlanReviewerTarget   string
	ResolvedProfile      ExecutionProfile
	Agents               []DryRunAgentInfo
	AllSkills            []DryRunSkillInfo
	MatchedSkillNames    []string
	OrchestratorPrompt   string
	FirstRoundTasks      []TaskDef
	ContractFindings     []ContractFinding
	ResolvedRunInputs    *RunInputSnapshot
	Error                string
	SubjectRoot          string
	WorkspaceWouldCreate bool
	// WorkerRoute and WorkerRouteCandidates describe the team execution
	// route, the worker default when the team sets one.
	WorkerRoute           string
	WorkerRouteCandidates []string
}

// DryRunCoordinatorParams is the resolved configuration a preview needs to
// freeze the same execution policy a real run would admit.
type DryRunCoordinatorParams struct {
	Session               *TeamSession
	Profile               ExecutionProfile
	DefaultProviderURL    string
	DefaultProviderAPIKey string
	ModelList             []config.ModelEntry
	RoleModels            RoleModels
	MaxConcurrent         int
	NoNet                 bool
}

// NewDryRunCoordinator creates the read-only coordinator projection used by
// CLI previews. It builds the in-memory provider and execution registries and
// freezes the execution policy, because run input resolvers execute under that
// policy. It intentionally does not open provider transports, SQLite,
// journals, terminals, audit logs, or workspace directories.
func NewDryRunCoordinator(params DryRunCoordinatorParams) (*Coordinator, error) {
	session := params.Session
	if session == nil {
		return nil, fmt.Errorf("dry-run coordinator requires a team session")
	}
	pm, err := agent.NewProviderManager(params.DefaultProviderURL, params.DefaultProviderAPIKey, session.Config.Providers)
	if err != nil {
		return nil, fmt.Errorf("failed to create provider manager: %w", err)
	}
	c := &Coordinator{
		providerManager:   pm,
		session:           session,
		skills:            session.Skills,
		taskTracker:       NewTaskTracker(),
		reportStatus:      func(StatusEvent) {},
		projectDir:        session.Scope.SubjectRoot,
		modelList:         params.ModelList,
		sidecarModel:      params.RoleModels.Sidecar,
		guardModel:        params.RoleModels.Guard,
		judgeModel:        params.RoleModels.Judge,
		planReviewerModel: params.RoleModels.PlanReviewer,
		maxConcurrent:     params.MaxConcurrent,
		noNet:             params.NoNet,
	}
	c.executionRegistry = newExecutionRegistryFor(c)
	c.SetExecutionProfile(params.Profile)
	// Routes are bound before the policy snapshot, which pins them.
	if err := c.bindExecutionRoutes(); err != nil {
		return nil, err
	}
	executionPolicy, err := newExecutionPolicyState(c)
	if err != nil {
		return nil, fmt.Errorf("resolve execution policy snapshot: %w", err)
	}
	c.executionPolicy = executionPolicy
	return c, nil
}

func (c *Coordinator) DryRun(ctx context.Context, userPrompt string) (*DryRunResult, error) {
	if err := c.validateDryRunExecutionPolicy(); err != nil {
		return nil, err
	}
	resolvedInputs, err := c.previewRunInputs(ctx, userPrompt)
	if err != nil {
		return nil, err
	}
	orchDef := c.GetOrchestratorDef()

	result := &DryRunResult{
		UserPrompt:        userPrompt,
		ResolvedProfile:   c.ExecutionProfile(),
		ResolvedRunInputs: resolvedInputs,
	}
	if c.session != nil && c.session.Config.Name != "" {
		result.TeamName = c.session.Config.Name
	}
	if c.session != nil {
		result.SubjectRoot = c.session.Scope.SubjectRoot
		result.WorkspaceWouldCreate = c.session.Scope.Managed && c.session.Scope.ProjectID == ""
		backend := c.session.Config.DefaultLLMBackend
		result.WorkerTarget = canonicalDryRunTarget(c.session.Config.WorkerModel, backend)
		if route := c.session.ExecutionRoutes[c.session.Config.ExecutionRoute]; c.session.Config.ExecutionRoute != "" && route != nil {
			result.WorkerRoute = route.Name
			for _, candidate := range route.Candidates {
				result.WorkerRouteCandidates = append(result.WorkerRouteCandidates, candidate.String())
			}
		}
		result.CoordinatorTarget = canonicalDryRunTarget(c.session.Config.CoordinatorModel, backend)
		// Auxiliary roles show the resolved role models (team > hufu.yaml,
		// guard/judge falling back to sidecar) a real run would use. A
		// coordinator built without them shows the raw team configuration.
		result.SidecarTarget = canonicalDryRunTarget(firstNonEmpty(c.sidecarModel, c.session.Config.SidecarModel), backend)
		result.GuardTarget = canonicalDryRunTarget(firstNonEmpty(c.guardModel, c.session.Config.GuardModel), backend)
		result.JudgeTarget = canonicalDryRunTarget(firstNonEmpty(c.judgeModel, c.session.Config.JudgeModel), backend)
		result.PlanReviewerTarget = canonicalDryRunTarget(firstNonEmpty(c.planReviewerModel, c.session.Config.PlanReviewerModel), backend)
	}
	if orchDef != nil {
		result.Model = c.resolveAgentModel(orchDef, "")
	}

	if c.session != nil {
		result.ContractFindings = LintTeamContracts(c.session)
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
				tools = []string{"agent", "finish", "load_skill", "ask_user"}
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

func (c *Coordinator) validateDryRunExecutionPolicy() error {
	if c != nil && c.session != nil {
		hasResolver := false
		for _, definition := range c.session.RunInputDefinitions {
			hasResolver = hasResolver || definition.Resolver != nil
		}
		if !hasResolver {
			return nil
		}
	}
	if c == nil || c.executionPolicy == nil || c.executionPolicy.snapshot == nil {
		return fmt.Errorf("dry-run requires a frozen execution policy")
	}
	live, err := newExecutionPolicyState(c)
	if err != nil {
		return err
	}
	if live.snapshot.ConfigurationHash != c.executionPolicy.snapshot.ConfigurationHash {
		return fmt.Errorf("dry-run execution policy drift: frozen=%s live=%s", c.executionPolicy.snapshot.ConfigurationHash, live.snapshot.ConfigurationHash)
	}
	return nil
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
	clone.ActionInputBindings = append([]ActionInputBinding(nil), td.ActionInputBindings...)
	clone.BoundInputs = cloneStringMap(td.BoundInputs)
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
	clone.CatalogAction = td.CatalogAction.clone()
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
	clone := cloneActionValue(*action)
	return &clone
}
