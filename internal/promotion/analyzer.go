package promotion

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/team"
)

type Repository interface {
	EligibilityRepository
	Get(context.Context, string) (contextstore.ContextItem, error)
	CreatePromotion(context.Context, Proposal, contextstore.PromotionOutboxEvent) (Proposal, bool, error)
	GetPromotion(context.Context, string, string, string) (Proposal, error)
	ListPromotions(context.Context, string, string) ([]Proposal, error)
	UpdatePromotionDraft(context.Context, string, string, string, string, string, contextstore.PromotionOutboxEvent) (Proposal, error)
	TransitionPromotion(context.Context, string, string, string, Status, string, contextstore.PromotionOutboxEvent) (Proposal, error)
	EnqueuePromotionEvent(context.Context, contextstore.PromotionOutboxEvent) error
}

type AnalyzeOptions struct {
	ProjectID, TeamID, PolicyVersion, AgentID, TeamDir string
	Type                                               Type
	DryRun                                             bool
}
type AnalyzeResult struct {
	Eligible    []EligibleSource `json:"eligible"`
	Diagnostics []Diagnostic     `json:"diagnostics,omitempty"`
	Proposals   []Proposal       `json:"proposals"`
}

type Analyzer struct {
	Repo      Repository
	Generator DraftGenerator
	Policy    agent.MemoryLearningPolicy
}

func (a *Analyzer) Analyze(ctx context.Context, opts AnalyzeOptions) (AnalyzeResult, error) {
	if opts.ProjectID == "" || opts.TeamID == "" {
		return AnalyzeResult{}, fmt.Errorf("project and team are required")
	}
	if opts.PolicyVersion == "" {
		opts.PolicyVersion = agent.DefaultMemoryLearningPolicy().PolicyVersion
	}
	if opts.Type == TypeAgentPolicy && opts.AgentID == "" {
		return AnalyzeResult{}, fmt.Errorf("--agent is required for agent-policy analysis")
	}
	policy := a.Policy
	if policy.PolicyVersion == "" {
		policy = agent.DefaultMemoryLearningPolicy()
	}
	eligible, diagnostics, err := EligibleSources(ctx, a.Repo, EligibilityOptions{ProjectID: opts.ProjectID, TeamID: opts.TeamID, PolicyVersion: opts.PolicyVersion, AgentID: opts.AgentID, Type: opts.Type}, policy)
	if err != nil {
		return AnalyzeResult{}, err
	}
	result := AnalyzeResult{Eligible: eligible, Diagnostics: diagnostics}
	if opts.DryRun || len(eligible) == 0 {
		return result, nil
	}
	targets, err := discoverTargets(opts.TeamDir)
	if err != nil {
		return result, err
	}
	for _, source := range eligible {
		metrics := metricsFromAggregate(source.Aggregate)
		draft, err := a.Generator.Generate(ctx, DraftRequest{SourceID: source.Item.ID, Kind: string(source.Item.Kind), Content: source.Item.Content, AgentID: source.Item.Scope.AgentID, AllowedTypes: source.AllowedTypes, Metrics: metrics})
		if err != nil {
			return result, fmt.Errorf("generate proposal for %s: %w", source.Item.ID, err)
		}
		if !containsType(source.AllowedTypes, draft.Type) {
			return result, fmt.Errorf("generator selected disallowed type %q for %s", draft.Type, source.Item.ID)
		}
		targetPath, err := targets.resolve(draft, source.Item)
		if err != nil {
			return result, err
		}
		if err = ValidateDraft(draft.Type, draft.Draft, draft.SkillName, draft.Steps); err != nil {
			return result, fmt.Errorf("validate proposal for %s: %w", source.Item.ID, err)
		}
		baseHash, err := targetBaseHash(opts.TeamDir, targetPath)
		if err != nil {
			return result, err
		}
		if draft.Type == TypeSkill && baseHash != "" {
			return result, fmt.Errorf("skill target %s already exists; promotion never overwrites skills", targetPath)
		}
		p := Proposal{ProjectID: opts.ProjectID, TeamID: opts.TeamID, Type: draft.Type, AgentID: draft.AgentID, TargetPath: targetPath, TargetBaseHash: baseHash, Draft: draft.Draft, DraftHash: contextstore.HashPromotionContent(draft.Draft), PolicyVersion: opts.PolicyVersion, Sources: []SourceSnapshot{{ContextItemID: source.Item.ID, ContentHash: source.Item.ContentHash, AggregateRevision: source.Aggregate.Revision}}, Metrics: metrics, Status: StatusProposed}
		if p.Type == TypeAgentPolicy {
			p.AgentID = source.Item.Scope.AgentID
		}
		p.ID = contextstore.PromotionProposalID(p)
		event := lifecycleEvent("memory_promotion_proposed", p, "create", "")
		stored, _, err := a.Repo.CreatePromotion(ctx, p, event)
		if err != nil {
			return result, err
		}
		result.Proposals = append(result.Proposals, stored)
	}
	return result, nil
}

type teamTargets struct {
	coordinators []string
	agents       map[string]string
}

func discoverTargets(teamDir string) (teamTargets, error) {
	vars, err := team.ResolveTeamTemplateVars(teamDir, nil)
	if err != nil {
		return teamTargets{}, err
	}
	entries, err := os.ReadDir(teamDir)
	if err != nil {
		return teamTargets{}, err
	}
	t := teamTargets{agents: map[string]string{}}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(strings.ToLower(e.Name()), ".md") {
			continue
		}
		if e.Type()&os.ModeSymlink != 0 {
			return t, fmt.Errorf("agent target %s must not be a symlink", e.Name())
		}
		path := filepath.Join(teamDir, e.Name())
		def, err := team.ValidateAgentFileWithVars(path, vars)
		if err != nil {
			return t, err
		}
		if def == nil {
			continue
		}
		rel := filepath.ToSlash(e.Name())
		for _, key := range []string{strings.ToLower(def.Name), strings.ToLower(strings.TrimSuffix(e.Name(), filepath.Ext(e.Name())))} {
			if old, ok := t.agents[key]; ok && old != rel {
				return t, fmt.Errorf("agent %q resolves to multiple files", key)
			}
			t.agents[key] = rel
		}
		if def.Role == "coordinator" || def.Role == "orchestrator" {
			t.coordinators = append(t.coordinators, rel)
		}
	}
	sort.Strings(t.coordinators)
	return t, nil
}
func (t teamTargets) resolve(d DraftResult, item contextstore.ContextItem) (string, error) {
	switch d.Type {
	case TypeSkill:
		if d.AgentID != "" {
			return "", fmt.Errorf("skill proposal must not target an agent")
		}
		def, err := skillNameFromDraft(d)
		if err != nil {
			return "", err
		}
		return TargetPathForSkill(def), nil
	case TypeTeamPolicy:
		if d.AgentID != "" {
			return "", fmt.Errorf("team policy proposal must not target an agent")
		}
		if item.Scope.AgentID != "" {
			return "", fmt.Errorf("private agent memory cannot produce team policy")
		}
		if len(t.coordinators) != 1 {
			return "", fmt.Errorf("team policy requires exactly one coordinator/orchestrator, found %d", len(t.coordinators))
		}
		return t.coordinators[0], nil
	case TypeAgentPolicy:
		id := item.Scope.AgentID
		if d.AgentID != "" && !strings.EqualFold(d.AgentID, id) {
			return "", fmt.Errorf("generator agent %q does not match source agent %q", d.AgentID, id)
		}
		path, ok := t.agents[strings.ToLower(id)]
		if !ok {
			return "", fmt.Errorf("source agent %q does not resolve to a team agent", id)
		}
		return path, nil
	default:
		return "", fmt.Errorf("unknown promotion type %q", d.Type)
	}
}
func skillNameFromDraft(d DraftResult) (string, error) {
	if d.SkillName != "" {
		return d.SkillName, nil
	}
	return "", fmt.Errorf("skill proposal requires skill_name")
}
func targetBaseHash(teamDir, rel string) (string, error) {
	path := filepath.Join(teamDir, filepath.FromSlash(rel))
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	return contextstore.HashPromotionContent(string(b)), nil
}
func metricsFromAggregate(a contextstore.ExperienceAggregate) Metrics {
	return Metrics{UtilityLowerBound: a.UtilityLowerBound, AppliedCount: a.AppliedCount, RejectedCount: a.RejectedCount, VerifiedSupportCount: a.VerifiedSupportCount, CausalFailureCount: a.CausalFailureCount, IndependentTaskCount: a.IndependentTaskCount, IndependentProjectCount: a.IndependentProjectCount, AggregateRevision: a.Revision}
}

type auditPayload struct {
	SchemaVersion int      `json:"schema_version"`
	ProposalID    string   `json:"proposal_id"`
	SourceIDs     []string `json:"source_ids"`
	DraftHash     string   `json:"draft_hash"`
	TargetHash    string   `json:"target_hash"`
	TargetPath    string   `json:"target_path"`
	PolicyVersion string   `json:"policy_version"`
	Actor         string   `json:"actor"`
}

func lifecycleEvent(typ string, p Proposal, revision, actor string) contextstore.PromotionOutboxEvent {
	if actor == "" {
		actor = "operator"
	}
	ids := make([]string, len(p.Sources))
	for i, s := range p.Sources {
		ids[i] = s.ContextItemID
	}
	sort.Strings(ids)
	payload, _ := json.Marshal(auditPayload{SchemaVersion: 1, ProposalID: p.ID, SourceIDs: ids, DraftHash: p.DraftHash, TargetHash: p.TargetBaseHash, TargetPath: p.TargetPath, PolicyVersion: p.PolicyVersion, Actor: actor})
	return contextstore.PromotionOutboxEvent{IdempotencyKey: p.ID + ":" + typ + ":" + revision, EventType: typ, Payload: payload}
}
