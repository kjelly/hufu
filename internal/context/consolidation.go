package context

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
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

type ConsolidationRepository interface {
	SaveConsolidationProposal(context.Context, ConsolidationProposal) error
	GetConsolidationProposal(context.Context, string) (ConsolidationProposal, error)
	UpdateConsolidationProposal(context.Context, string, string, string) error
}

func (r *SQLiteRepository) SaveConsolidationProposal(ctx context.Context, proposal ConsolidationProposal) error {
	if proposal.ID == "" || proposal.ProjectID == "" || proposal.CandidateContextItemID == "" || len(proposal.SourceIDs) < 2 {
		return errors.New("consolidation proposal requires id, project, candidate, and at least two sources")
	}
	if proposal.Status == "" {
		proposal.Status = "proposed"
	}
	if proposal.CreatedAt.IsZero() {
		proposal.CreatedAt = time.Now().UTC()
	}
	_, err := r.db.ExecContext(ctx, `INSERT INTO consolidation_proposals(id,project_id,team_id,candidate_context_item_id,source_ids_json,source_revisions_json,aggregate_revisions_json,status,reason,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, proposal.ID, proposal.ProjectID, nilIfEmpty(proposal.TeamID), proposal.CandidateContextItemID, mustJSON(proposal.SourceIDs), mustJSON(proposal.SourceRevisions), mustJSON(proposal.AggregateRevisions), proposal.Status, proposal.Reason, proposal.CreatedAt.UnixMilli())
	return err
}

func (r *SQLiteRepository) GetConsolidationProposal(ctx context.Context, id string) (ConsolidationProposal, error) {
	var proposal ConsolidationProposal
	var teamID, reason sql.NullString
	var sources, revisions, aggregates string
	var created int64
	var reviewed sql.NullInt64
	err := r.db.QueryRowContext(ctx, `SELECT id,project_id,team_id,candidate_context_item_id,source_ids_json,source_revisions_json,aggregate_revisions_json,status,reason,created_at,reviewed_at FROM consolidation_proposals WHERE id=?`, id).Scan(&proposal.ID, &proposal.ProjectID, &teamID, &proposal.CandidateContextItemID, &sources, &revisions, &aggregates, &proposal.Status, &reason, &created, &reviewed)
	if err != nil {
		return proposal, err
	}
	proposal.TeamID, proposal.Reason, proposal.CreatedAt = teamID.String, reason.String, time.UnixMilli(created).UTC()
	if reviewed.Valid {
		value := time.UnixMilli(reviewed.Int64).UTC()
		proposal.ReviewedAt = &value
	}
	if json.Unmarshal([]byte(sources), &proposal.SourceIDs) != nil || json.Unmarshal([]byte(revisions), &proposal.SourceRevisions) != nil || json.Unmarshal([]byte(aggregates), &proposal.AggregateRevisions) != nil {
		return ConsolidationProposal{}, fmt.Errorf("decode consolidation proposal %q", id)
	}
	return proposal, nil
}

func (r *SQLiteRepository) UpdateConsolidationProposal(ctx context.Context, id, status, reason string) error {
	if status != "approved" && status != "rejected" && status != "failed" {
		return fmt.Errorf("invalid consolidation proposal status %q", status)
	}
	result, err := r.db.ExecContext(ctx, `UPDATE consolidation_proposals SET status=?,reason=?,reviewed_at=? WHERE id=? AND status='proposed'`, status, reason, time.Now().UTC().UnixMilli(), id)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err == nil && rows != 1 {
		return fmt.Errorf("consolidation proposal %q is missing or already reviewed", id)
	}
	return err
}

var _ ConsolidationRepository = (*SQLiteRepository)(nil)
