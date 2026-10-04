package context

import (
	"context"
	"fmt"
	"time"
)

// ConsolidationOrphan is a consolidation-owned candidate that no proposal
// points back to.
type ConsolidationOrphan struct {
	ItemID string              `json:"item_id"`
	Reason ConsolidationReason `json:"reason"`
}

// ConsolidationDoctorReport is the content-free result of checking every
// consolidation proposal in a scope.
type ConsolidationDoctorReport struct {
	Counts    map[ConsolidationFreshnessState]int `json:"counts"`
	Proposals []ConsolidationFreshness            `json:"proposals"`
	Orphans   []ConsolidationOrphan               `json:"orphans"`
}

// Healthy reports whether every proposal is fresh and nothing is orphaned.
func (r ConsolidationDoctorReport) Healthy() bool {
	return len(r.Orphans) == 0 && r.Counts[ConsolidationFresh] == len(r.Proposals)
}

// ConsolidationDoctor evaluates every proposal in a project (one team unless
// allTeams) and lists orphaned candidates. It only reads: it never repairs,
// regenerates, or changes a lifecycle.
func (r *SQLiteRepository) ConsolidationDoctor(ctx context.Context, projectID, teamID string, allTeams bool, policyVersion string, staleAfter time.Duration) (ConsolidationDoctorReport, error) {
	report := ConsolidationDoctorReport{
		Counts:    map[ConsolidationFreshnessState]int{ConsolidationFresh: 0, ConsolidationStale: 0, ConsolidationBlocked: 0, ConsolidationInvalid: 0},
		Proposals: []ConsolidationFreshness{},
		Orphans:   []ConsolidationOrphan{},
	}
	proposals, decodeErrors, err := r.ListConsolidationProposals(ctx, projectID, teamID, allTeams)
	if err != nil {
		return report, err
	}
	now := time.Now()
	linked := map[string]bool{}
	for _, proposal := range proposals {
		var freshness ConsolidationFreshness
		if decodeErrors[proposal.ID] != nil {
			freshness = ConsolidationFreshness{ProposalID: proposal.ID, Status: proposal.Status, CandidateID: proposal.CandidateContextItemID, State: ConsolidationInvalid, Reasons: []ConsolidationReason{ReasonProposalDecodeFailed}}
		} else if freshness, err = r.evaluateConsolidationQ(ctx, r.db, proposal, consolidationCheckInspect, policyVersion, staleAfter, now); err != nil {
			return report, err
		}
		report.Proposals = append(report.Proposals, freshness)
		report.Counts[freshness.State]++
		linked[proposal.CandidateContextItemID] = true
	}
	query := "SELECT id FROM context_items WHERE project_id=? AND json_extract(source_json,'$.type')=?"
	args := []any{projectID, SourceTypeConsolidationProposal}
	if !allTeams {
		query += " AND COALESCE(team_id,'')=?"
		args = append(args, teamID)
	}
	rows, err := r.db.QueryContext(ctx, query+" ORDER BY id", args...)
	if err != nil {
		return report, fmt.Errorf("list consolidation candidates: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return report, fmt.Errorf("scan consolidation candidate: %w", err)
		}
		// A candidate some proposal references is checked through that
		// proposal (a wrong Source.Ref is candidate_link_mismatch there).
		if !linked[id] {
			report.Orphans = append(report.Orphans, ConsolidationOrphan{ItemID: id, Reason: ReasonOrphanCandidate})
		}
	}
	return report, rows.Err()
}
