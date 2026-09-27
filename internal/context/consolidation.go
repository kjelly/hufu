package context

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// SourceTypeConsolidationProposal marks the candidate a consolidation proposal
// owns. Only the consolidation repository methods may create, confirm,
// reject, or rewrite items with this source type.
const SourceTypeConsolidationProposal = "consolidation_proposal"

const (
	ConsolidationStatusProposed = "proposed"
	ConsolidationStatusApproved = "approved"
	ConsolidationStatusRejected = "rejected"
	// ConsolidationStatusFailed is a legacy value that is read but never written.
	ConsolidationStatusFailed = "failed"
	// ConsolidationStatusStale marks a proposal whose source stopped being
	// current knowledge after it was proposed or approved.
	ConsolidationStatusStale = "stale"
)

const (
	EvidenceTypeOperatorApproval  = "operator_approval"
	EvidenceTypeOperatorRejection = "operator_rejection"
)

var (
	// ErrConsolidationNotFound covers both a missing proposal and one outside
	// the requested project so the error does not reveal existence.
	ErrConsolidationNotFound = errors.New("consolidation proposal not found")
	// ErrConsolidationNotPending means the proposal status does not allow the
	// requested review transition.
	ErrConsolidationNotPending = errors.New("consolidation proposal is not pending")
	// ErrConsolidationProposalExists means the same proposal identity already
	// exists in a terminal status.
	ErrConsolidationProposalExists = errors.New("consolidation proposal already exists")
	// ErrConsolidationPending means another proposal for the same source set
	// is still waiting for review.
	ErrConsolidationPending = errors.New("consolidation proposal is already pending for these sources")
	// ErrConsolidationCandidateDuplicate means the consolidated text already
	// exists as a context item in the same scope and kind.
	ErrConsolidationCandidateDuplicate = errors.New("consolidation candidate duplicates an existing context item")
	// ErrConsolidationSourceInvalid means at least one source is not current,
	// eligible knowledge. The concrete error is *ConsolidationSourceError.
	ErrConsolidationSourceInvalid = errors.New("consolidation source validation failed")
	// ErrConsolidationInconsistent means the proposal and its candidate no
	// longer point at each other; hufu context doctor --consolidation reports
	// the details.
	ErrConsolidationInconsistent = errors.New("consolidation proposal is inconsistent; run hufu context doctor --consolidation")
)

type ConsolidationProposal struct {
	ID                     string            `json:"id"`
	ProjectID              string            `json:"project_id"`
	TeamID                 string            `json:"team_id,omitempty"`
	CandidateContextItemID string            `json:"candidate_context_item_id"`
	SourceIDs              []string          `json:"source_ids"`
	SourceRevisions        map[string]string `json:"source_revisions"`
	AggregateRevisions     map[string]int64  `json:"aggregate_revisions"`
	Status                 string            `json:"status"`
	Reason                 string            `json:"reason,omitempty"`
	CreatedAt              time.Time         `json:"created_at"`
	ReviewedAt             *time.Time        `json:"reviewed_at,omitempty"`
}

const consolidationColumns = `id,project_id,team_id,candidate_context_item_id,source_ids_json,source_revisions_json,aggregate_revisions_json,status,reason,created_at,reviewed_at`

// errConsolidationDecode marks a proposal row whose JSON columns cannot be
// decoded; doctor reports it instead of failing the whole report.
var errConsolidationDecode = errors.New("decode consolidation proposal")

func scanConsolidationProposal(row interface{ Scan(...any) error }) (ConsolidationProposal, error) {
	var proposal ConsolidationProposal
	var teamID, reason sql.NullString
	var sources, revisions, aggregates string
	var created int64
	var reviewed sql.NullInt64
	if err := row.Scan(&proposal.ID, &proposal.ProjectID, &teamID, &proposal.CandidateContextItemID, &sources, &revisions, &aggregates, &proposal.Status, &reason, &created, &reviewed); err != nil {
		return proposal, err
	}
	proposal.TeamID, proposal.Reason, proposal.CreatedAt = teamID.String, reason.String, time.UnixMilli(created).UTC()
	if reviewed.Valid {
		value := time.UnixMilli(reviewed.Int64).UTC()
		proposal.ReviewedAt = &value
	}
	if json.Unmarshal([]byte(sources), &proposal.SourceIDs) != nil || json.Unmarshal([]byte(revisions), &proposal.SourceRevisions) != nil || json.Unmarshal([]byte(aggregates), &proposal.AggregateRevisions) != nil {
		return proposal, fmt.Errorf("%w %q", errConsolidationDecode, proposal.ID)
	}
	return proposal, nil
}

func getConsolidationProposalQ(ctx context.Context, q queryer, id string) (ConsolidationProposal, error) {
	proposal, err := scanConsolidationProposal(q.QueryRowContext(ctx, "SELECT "+consolidationColumns+" FROM consolidation_proposals WHERE id=?", id))
	if errors.Is(err, errConsolidationDecode) {
		return ConsolidationProposal{}, err
	}
	return proposal, err
}

func (r *SQLiteRepository) GetConsolidationProposal(ctx context.Context, id string) (ConsolidationProposal, error) {
	return getConsolidationProposalQ(ctx, r.db, id)
}

// ListConsolidationProposals returns every proposal in a project, limited to
// one team unless allTeams is set, ordered by creation. Rows that cannot be
// decoded are returned with their ID and status and a non-nil decode error in
// the second result so read-only checks can report them.
func (r *SQLiteRepository) ListConsolidationProposals(ctx context.Context, projectID, teamID string, allTeams bool) ([]ConsolidationProposal, map[string]error, error) {
	query := "SELECT " + consolidationColumns + " FROM consolidation_proposals WHERE project_id=?"
	args := []any{projectID}
	if !allTeams {
		query += " AND COALESCE(team_id,'')=?"
		args = append(args, teamID)
	}
	rows, err := r.db.QueryContext(ctx, query+" ORDER BY created_at, id", args...)
	if err != nil {
		return nil, nil, fmt.Errorf("list consolidation proposals: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var proposals []ConsolidationProposal
	decodeErrors := map[string]error{}
	for rows.Next() {
		proposal, scanErr := scanConsolidationProposal(rows)
		if errors.Is(scanErr, errConsolidationDecode) {
			decodeErrors[proposal.ID] = scanErr
		} else if scanErr != nil {
			return nil, nil, fmt.Errorf("scan consolidation proposal: %w", scanErr)
		}
		proposals = append(proposals, proposal)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("list consolidation proposals: %w", err)
	}
	return proposals, decodeErrors, nil
}

// FindProposedConsolidation returns the earliest (created_at, id) proposal
// still in proposed status whose sorted source IDs equal sortedSourceIDs, so
// drafting the same sources again reuses the pending proposal.
func (r *SQLiteRepository) FindProposedConsolidation(ctx context.Context, projectID, teamID string, sortedSourceIDs []string) (ConsolidationProposal, bool, error) {
	return findProposedConsolidationQ(ctx, r.db, projectID, teamID, sortedSourceIDs, "")
}

// findProposedConsolidationQ is FindProposedConsolidation through q, skipping
// the proposal with ID exclude.
func findProposedConsolidationQ(ctx context.Context, q queryer, projectID, teamID string, sortedSourceIDs []string, exclude string) (ConsolidationProposal, bool, error) {
	var id string
	err := q.QueryRowContext(ctx, `SELECT id FROM consolidation_proposals WHERE project_id=? AND COALESCE(team_id,'')=? AND status='proposed' AND source_ids_json=? AND id<>? ORDER BY created_at, id LIMIT 1`, projectID, teamID, mustJSON(sortedSourceIDs), exclude).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return ConsolidationProposal{}, false, nil
	}
	if err != nil {
		return ConsolidationProposal{}, false, fmt.Errorf("find pending consolidation proposal: %w", err)
	}
	proposal, err := getConsolidationProposalQ(ctx, q, id)
	if err != nil {
		return ConsolidationProposal{}, false, fmt.Errorf("load pending consolidation proposal %s: %w", id, err)
	}
	return proposal, true, nil
}

// sortedUniqueIDs trims, deduplicates, and sorts ids.
func sortedUniqueIDs(ids []string) []string {
	seen := make(map[string]bool, len(ids))
	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if id = strings.TrimSpace(id); id != "" && !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}
