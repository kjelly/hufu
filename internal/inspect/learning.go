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
// canonical shared scope. requested is the team definition's policy; it is
// kept separate from the effective policy so fallback is visible. The
// effective policy follows the runtime: an adopted policy wins, and without
// one the requested policy applies through agent.UnadoptedMemoryLearningPolicy.
// A caller that does not know the team's policy passes the zero value.
func InspectLearning(ctx context.Context, workspace, projectID, teamID string, requested agent.MemoryLearningPolicy) operatorpkg.LearningView {
	view := operatorpkg.LearningView{Status: "unknown", RequestedMode: "unknown", EffectiveMode: "unknown"}
	if validLearningMode(requested.Mode) {
		view.RequestedMode = string(requested.Mode)
	}
	unadopted := unadoptedLearningPolicy(requested)
	repo, err := contextstore.OpenSQLiteReadOnly(filepath.Join(workspace, "context.sqlite"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			view.Status = "available"
			view.EffectiveMode = string(unadopted.Mode)
			view.PolicyVersion = unadopted.PolicyVersion
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
		view.EffectiveMode = string(unadopted.Mode)
		view.PolicyVersion = unadopted.PolicyVersion
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
	lineage, err := repo.ListExperienceLineage(ctx, projectID)
	if err != nil {
		view.Status = "unknown"
		view.UnavailableReason = "learning_lineage_query_failed"
		return clearLearningCounters(view)
	}
	// A promoted persistent record inherits its source session record's
	// evidence; counting the source as well would report that evidence twice.
	promoted := make(map[string]bool, len(lineage))
	for _, link := range lineage {
		promoted[link.SourceID] = true
	}
	var exposures, consulted, applied, rejected, verified, failures int64
	for _, aggregate := range aggregates {
		if promoted[aggregate.ContextItemID] {
			continue
		}
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
		view.RejectedPromotions = nil
		view.StalePromotions = nil
		view.AppliedEditedPromotions = nil
		view.AppliedEditUnknownPromotions = nil
		return view
	}
	var proposed, approved, published, rejectedProposals, stale, edited, editUnknown int64
	for _, proposal := range proposals {
		switch proposal.Status {
		case contextstore.PromotionStatusProposed:
			proposed++
		case contextstore.PromotionStatusApproved:
			approved++
		case contextstore.PromotionStatusApplied:
			published++
			if wasEdited, known := proposal.DraftEdited(); !known {
				editUnknown++
			} else if wasEdited {
				edited++
			}
		case contextstore.PromotionStatusRejected:
			rejectedProposals++
		case contextstore.PromotionStatusStale:
			stale++
		}
	}
	view.ProposedPromotions = new(proposed)
	view.ApprovedPromotions = new(approved)
	view.AppliedPromotions = new(published)
	view.RejectedPromotions = new(rejectedProposals)
	view.StalePromotions = new(stale)
	view.AppliedEditedPromotions = new(edited)
	view.AppliedEditUnknownPromotions = new(editUnknown)
	// Eligibility is only known after the explicit analyze operation. Existing
	// proposal rows are lifecycle state, not a substitute eligibility count.
	view.EligiblePromotions = nil
	openConflicts, conflictErr := repo.ListConflicts(ctx, contextstore.ConflictQuery{ProjectID: projectID, TeamID: teamID})
	switch {
	case conflictErr == nil:
		view.OpenConflicts = new(int64(len(openConflicts)))
	case errors.Is(conflictErr, contextstore.ErrConflictsUnavailable):
		// A store older than migration 11 has no conflict data: unknown, not zero.
		view.OpenConflicts = nil
	default:
		view.OpenConflicts = nil
		if view.UnavailableReason == "" {
			view.UnavailableReason = "conflict_query_failed"
		}
	}
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
	view.RejectedPromotions = new(zero)
	view.StalePromotions = new(zero)
	view.AppliedEditedPromotions = new(zero)
	view.AppliedEditUnknownPromotions = new(zero)
	view.OpenConflicts = new(zero)
}

func learningView(ctx context.Context, workspace, projectID, teamID string) operatorpkg.LearningView {
	return InspectLearning(ctx, workspace, projectID, teamID, agent.MemoryLearningPolicy{})
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
	view.RejectedPromotions = nil
	view.StalePromotions = nil
	view.AppliedEditedPromotions = nil
	view.AppliedEditUnknownPromotions = nil
	view.OpenConflicts = nil
	return view
}

// unadoptedLearningPolicy is the policy a run uses when none is adopted. An
// unknown request keeps the defaults. A request without a policy version,
// which a parsed team configuration never has, reads the default version.
func unadoptedLearningPolicy(requested agent.MemoryLearningPolicy) agent.MemoryLearningPolicy {
	if !validLearningMode(requested.Mode) {
		return agent.DefaultMemoryLearningPolicy()
	}
	policy, _ := agent.UnadoptedMemoryLearningPolicy(requested)
	if policy.PolicyVersion == "" {
		policy.PolicyVersion = agent.DefaultMemoryLearningPolicy().PolicyVersion
	}
	return policy
}

func validLearningMode(mode agent.MemoryLearningMode) bool {
	switch mode {
	case agent.MemoryLearningOff, agent.MemoryLearningObserve, agent.MemoryLearningShadow, agent.MemoryLearningActive:
		return true
	default:
		return false
	}
}
