package promotion

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/utils"
)

type EligibilityRepository interface {
	Query(context.Context, contextstore.RepositoryQuery) ([]contextstore.ContextItem, error)
	ExperienceAggregate(context.Context, string, string) (contextstore.ExperienceAggregate, error)
}

type EligibilityOptions struct {
	ProjectID, TeamID, PolicyVersion, AgentID string
	Type                                      Type
}

func EligibleSources(ctx context.Context, repo EligibilityRepository, opts EligibilityOptions, policy agent.MemoryLearningPolicy) ([]EligibleSource, []Diagnostic, error) {
	items, err := repo.Query(ctx, contextstore.RepositoryQuery{Scope: contextstore.Scope{ProjectID: opts.ProjectID, TeamID: opts.TeamID}, Visibility: contextstore.VisibilitySubtree, Limit: 100000})
	if err != nil {
		return nil, nil, err
	}
	now := time.Now().UTC()
	var result []EligibleSource
	var diagnostics []Diagnostic
	for _, item := range items {
		if item.Scope.ProjectID != opts.ProjectID || item.Scope.TeamID != opts.TeamID || item.Lifecycle != contextstore.LifecycleConfirmed || item.SupersededBy != "" {
			continue
		}
		if item.Scope.SessionID != "" || item.Scope.BranchID != "" || item.Scope.TaskID != "" || item.Scope.AttemptID != "" {
			continue
		}
		if item.ExpiresAt != nil && !item.ExpiresAt.After(now) {
			continue
		}
		if item.Metadata == nil || (item.Metadata["memory_lifetime"] != "persistent" && item.Metadata["memory_tier"] != "persistent") {
			continue
		}
		if utils.RedactSecrets(item.Content) != item.Content || strings.Contains(item.Content, "[REDACTED]") || strings.Contains(item.Content, "<REDACTED:") {
			diagnostics = append(diagnostics, Diagnostic{SourceID: item.ID, Reason: "secret_like_content"})
			continue
		}
		types := allowedPromotionTypes(item)
		if opts.AgentID != "" && item.Scope.AgentID != opts.AgentID {
			continue
		}
		if opts.Type != "" {
			if !containsType(types, opts.Type) {
				continue
			}
			types = []Type{opts.Type}
		}
		if len(types) == 0 {
			continue
		}
		agg, e := repo.ExperienceAggregate(ctx, item.ID, opts.PolicyVersion)
		if errors.Is(e, sql.ErrNoRows) {
			continue
		}
		if e != nil {
			return nil, diagnostics, e
		}
		if agg.VerifiedSupportCount < policy.MinConfirmedSupport || agg.IndependentTaskCount < policy.MinIndependentTasks {
			continue
		}
		harm := float64(agg.CausalFailureCount) / float64(max(agg.AppliedCount, 1))
		if harm > policy.MaxHarmRate {
			continue
		}
		result = append(result, EligibleSource{Item: item, Aggregate: agg, AllowedTypes: types})
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Item.ID < result[j].Item.ID })
	return result, diagnostics, nil
}

func allowedPromotionTypes(item contextstore.ContextItem) []Type {
	if item.Scope.AgentID != "" {
		return []Type{TypeAgentPolicy}
	}
	switch item.Kind {
	case contextstore.ContextPattern:
		return []Type{TypeSkill}
	case contextstore.ContextDecision, contextstore.ContextArchitecture:
		return []Type{TypeTeamPolicy}
	case contextstore.ContextConvention, contextstore.ContextInstruction:
		return []Type{TypeSkill, TypeTeamPolicy}
	default:
		return nil
	}
}
func containsType(types []Type, want Type) bool {
	for _, v := range types {
		if v == want {
			return true
		}
	}
	return false
}
