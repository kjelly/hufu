package promotion

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
	"github.com/kjelly/hufu/internal/utils"
)

type EligibilityRepository interface {
	Iterate(context.Context, contextstore.RepositoryQuery, func(contextstore.ContextItem) error) error
	ExperienceAggregate(context.Context, string, string) (contextstore.ExperienceAggregate, error)
	// OpenConflictsForItems gates promotion: a source with an unresolved
	// memory conflict is never promoted.
	OpenConflictsForItems(context.Context, string, string, []string) (map[string][]string, error)
}

// DiagnosticUnresolvedConflict marks a source excluded because it has an
// open memory conflict (hufu context conflicts).
const DiagnosticUnresolvedConflict = "unresolved_conflict"

type EligibilityOptions struct {
	ProjectID, TeamID, PolicyVersion, AgentID string
	Type                                      Type
}

func EligibleSources(ctx context.Context, repo EligibilityRepository, opts EligibilityOptions, policy agent.MemoryLearningPolicy) ([]EligibleSource, []Diagnostic, error) {
	now := time.Now().UTC()
	var diagnostics []Diagnostic
	type eligibleItem struct {
		item  contextstore.ContextItem
		types []Type
	}
	var candidates []eligibleItem
	err := repo.Iterate(ctx, contextstore.RepositoryQuery{Scope: contextstore.Scope{ProjectID: opts.ProjectID, TeamID: opts.TeamID}, Visibility: contextstore.VisibilitySubtree}, func(item contextstore.ContextItem) error {
		if item.Scope.ProjectID != opts.ProjectID || item.Scope.TeamID != opts.TeamID || !contextstore.IsCurrentPersistentKnowledge(item, now) {
			return nil
		}
		if utils.RedactSecrets(item.Content) != item.Content || strings.Contains(item.Content, "[REDACTED]") || strings.Contains(item.Content, "<REDACTED:") {
			diagnostics = append(diagnostics, Diagnostic{SourceID: item.ID, Reason: "secret_like_content"})
			return nil
		}
		types := allowedPromotionTypes(item)
		if opts.AgentID != "" && item.Scope.AgentID != opts.AgentID {
			return nil
		}
		if opts.Type != "" {
			if !containsType(types, opts.Type) {
				return nil
			}
			types = []Type{opts.Type}
		}
		if len(types) == 0 {
			return nil
		}
		candidates = append(candidates, eligibleItem{item: item, types: types})
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	candidateIDs := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		candidateIDs = append(candidateIDs, candidate.item.ID)
	}
	conflicts, err := repo.OpenConflictsForItems(ctx, opts.ProjectID, opts.TeamID, candidateIDs)
	if err != nil {
		return nil, diagnostics, fmt.Errorf("check memory conflicts for promotion sources: %w", err)
	}

	var result []EligibleSource
	for _, candidate := range candidates {
		item, types := candidate.item, candidate.types
		if len(conflicts[item.ID]) > 0 {
			diagnostics = append(diagnostics, Diagnostic{SourceID: item.ID, Reason: DiagnosticUnresolvedConflict})
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
