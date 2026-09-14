package inspect

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/kjelly/hufu/internal/agent"
	contextstore "github.com/kjelly/hufu/internal/context"
	operatorpkg "github.com/kjelly/hufu/internal/operator"
)

// InspectLearning returns the presentation-safe learning projection for one
// canonical shared scope. requestedMode comes from the team definition; it is
// kept separate from the adopted effective policy so fallback is visible.
func InspectLearning(ctx context.Context, workspace, projectID, teamID, requestedMode string) operatorpkg.LearningView {
	view := operatorpkg.LearningView{Status: "unknown", RequestedMode: "unknown", EffectiveMode: "unknown"}
	if validLearningMode(agent.MemoryLearningMode(requestedMode)) {
		view.RequestedMode = requestedMode
	}
	repo, err := contextstore.OpenSQLiteReadOnly(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			view.Status = "available"
			view.EffectiveMode = string(agent.MemoryLearningOff)
			view.PolicyVersion = agent.DefaultMemoryLearningPolicy().PolicyVersion
			view.EmptyState = "no_recall_data"
			setZeroLearningCounters(&view)
			if view.RequestedMode != "unknown" && view.RequestedMode != view.EffectiveMode {
				view.UnavailableReason = "requested_mode_not_effective"
			}
			return view
		}
		view.Status = "unavailable"
		view.UnavailableReason = "context_store_unavailable"
		return view
	}
	defer func() { _ = repo.Close() }()

	record, err := repo.ActiveMemoryPolicyVersion(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		view.Status = "available"
		view.EffectiveMode = string(agent.MemoryLearningOff)
		view.PolicyVersion = agent.DefaultMemoryLearningPolicy().PolicyVersion
	} else if err != nil {
		view.UnavailableReason = "learning_policy_query_failed"
		return view
	} else {
		var snapshot struct {
			Learning agent.MemoryLearningPolicy `json:"learning"`
		}
		if err = json.Unmarshal(record.Snapshot, &snapshot); err != nil || !validLearningMode(snapshot.Learning.Mode) {
			view.UnavailableReason = "learning_policy_invalid"
			return view
		}
		view.Status = "available"
		view.EffectiveMode = string(snapshot.Learning.Mode)
		view.PolicyVersion = record.PolicyVersion
	}
	if view.RequestedMode != "unknown" && view.RequestedMode != view.EffectiveMode {
		view.UnavailableReason = "requested_mode_not_effective"
	}

	aggregates, err := repo.ListExperienceAggregatesForScope(ctx, view.PolicyVersion, contextstore.Scope{ProjectID: projectID, TeamID: teamID})
	if err != nil {
		view.Status = "unknown"
		view.UnavailableReason = "learning_aggregate_query_failed"
		return clearLearningCounters(view)
	}
	var exposures, consulted, applied, rejected, verified, failures int64
	for _, aggregate := range aggregates {
		exposures += int64(aggregate.ExposureCount)
		consulted += int64(aggregate.ConsultedCount)
		applied += int64(aggregate.AppliedCount)
		rejected += int64(aggregate.RejectedCount)
		verified += int64(aggregate.VerifiedSupportCount)
		failures += int64(aggregate.CausalFailureCount)
	}
	view.Exposures = new(exposures)
	view.Consulted = new(consulted)
	view.Applied = new(applied)
	view.Rejected = new(rejected)
	view.VerifiedSupport = new(verified)
	view.CausalFailures = new(failures)

	proposals, err := repo.ListPromotions(ctx, projectID, teamID)
	if err != nil {
		view.Status = "unknown"
		view.UnavailableReason = "promotion_query_failed"
		view.EligiblePromotions = nil
		view.ProposedPromotions = nil
		view.ApprovedPromotions = nil
		view.AppliedPromotions = nil
		return view
	}
	var proposed, approved, published int64
	for _, proposal := range proposals {
		switch proposal.Status {
		case contextstore.PromotionStatusProposed:
			proposed++
		case contextstore.PromotionStatusApproved:
			approved++
		case contextstore.PromotionStatusApplied:
			published++
		}
	}
	view.ProposedPromotions = new(proposed)
	view.ApprovedPromotions = new(approved)
	view.AppliedPromotions = new(published)
	// Eligibility is only known after the explicit analyze operation. Existing
	// proposal rows are lifecycle state, not a substitute eligibility count.
	view.EligiblePromotions = nil
	if exposures == 0 && len(proposals) == 0 {
		view.EmptyState = "no_recall_data"
	} else if exposures > 0 && applied == 0 {
		view.EmptyState = "exposed_not_applied"
	} else if applied > 0 && verified == 0 {
		view.EmptyState = "applied_without_objective_support"
	}
	return view
}

func setZeroLearningCounters(view *operatorpkg.LearningView) {
	zero := int64(0)
	view.Exposures = new(zero)
	view.Consulted = new(zero)
	view.Applied = new(zero)
	view.Rejected = new(zero)
	view.VerifiedSupport = new(zero)
	view.CausalFailures = new(zero)
	view.ProposedPromotions = new(zero)
	view.ApprovedPromotions = new(zero)
	view.AppliedPromotions = new(zero)
}

func learningView(ctx context.Context, workspace, projectID, teamID string) operatorpkg.LearningView {
	return InspectLearning(ctx, workspace, projectID, teamID, "")
}

func clearLearningCounters(view operatorpkg.LearningView) operatorpkg.LearningView {
	view.Exposures = nil
	view.Consulted = nil
	view.Applied = nil
	view.Rejected = nil
	view.VerifiedSupport = nil
	view.CausalFailures = nil
	view.EligiblePromotions = nil
	view.ProposedPromotions = nil
	view.ApprovedPromotions = nil
	view.AppliedPromotions = nil
	return view
}

func validLearningMode(mode agent.MemoryLearningMode) bool {
	switch mode {
	case agent.MemoryLearningOff, agent.MemoryLearningObserve, agent.MemoryLearningShadow, agent.MemoryLearningActive:
		return true
	default:
		return false
	}
}
