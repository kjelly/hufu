package context

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

// consolidationTxTestHook lets tests fail a transaction after a named stage
// to prove nothing is persisted. It is always nil in production.
var consolidationTxTestHook func(stage string) error

func consolidationTxStage(stage string) error {
	if consolidationTxTestHook == nil {
		return nil
	}
	return consolidationTxTestHook(stage)
}

// ConsolidationCreateInput is one proposal to merge sources into a candidate.
type ConsolidationCreateInput struct {
	ConsolidationSourceSelection
	Text string
	// Origin is "operator" for operator text or "model" for a drafted text.
	Origin     string
	DraftModel string
	// Expected is the source state a drafted text was generated from. When
	// set, creation refuses sources whose content or aggregate revision moved
	// while the model was writing, instead of binding the text to evidence it
	// never saw. Operator text leaves it nil.
	Expected *ConsolidationSourceRevisions
}

// ConsolidationSourceRevisions records each source's content hash and
// experience aggregate revision (0 when it had none) by source ID.
type ConsolidationSourceRevisions struct {
	Content    map[string]string
	Aggregates map[string]int64
}

// ConsolidationReviewInput approves or rejects one proposal.
type ConsolidationReviewInput struct {
	ProposalID    string
	ProjectID     string
	PolicyVersion string
	// StaleAfter requires recent strong evidence for every source at
	// approval; zero skips the check.
	StaleAfter time.Duration
	Actor      string
	Reason     string
}

// consolidationIdentity derives the proposal and candidate IDs from the
// sorted source IDs and the normalized text, so whitespace-only differences
// map to the same proposal.
func consolidationIdentity(ids []string, text string) (proposalID, candidateID string) {
	sum := sha256.Sum256([]byte(strings.Join(ids, "\x00") + "\x00" + text))
	return "consolidation-" + hex.EncodeToString(sum[:10]), "ctx-consolidated-" + hex.EncodeToString(sum[10:20])
}

// CreateConsolidationProposal validates the sources and persists the
// candidate, its derived_from edges, the proposal, and a content-free event
// in one transaction. created is false when the same proposal is already
// pending, in which case nothing is written.
func (r *SQLiteRepository) CreateConsolidationProposal(ctx context.Context, in ConsolidationCreateInput) (ConsolidationProposal, bool, error) {
	ids := sortedUniqueIDs(in.SourceIDs)
	if len(ids) < 2 {
		return ConsolidationProposal{}, false, errors.New("consolidation requires at least two source items")
	}
	if strings.TrimSpace(in.ProjectID) == "" {
		return ConsolidationProposal{}, false, errors.New("consolidation requires a project")
	}
	if in.Origin != "operator" && in.Origin != "model" {
		return ConsolidationProposal{}, false, fmt.Errorf("invalid consolidation origin %q", in.Origin)
	}
	if in.Origin == "model" && strings.TrimSpace(in.DraftModel) == "" {
		return ConsolidationProposal{}, false, errors.New("model-drafted consolidation requires the draft model")
	}
	trimmed := strings.ReplaceAll(strings.TrimSpace(in.Text), "\r\n", "\n")
	text := NormalizeContent(in.Text)
	if text == "" {
		return ConsolidationProposal{}, false, errors.New("consolidation text is empty")
	}
	if text != trimmed {
		return ConsolidationProposal{}, false, errors.New("consolidation text contains secret-like material")
	}
	proposalID, candidateID := consolidationIdentity(ids, text)
	var out ConsolidationProposal
	var created bool
	err := r.withBusyRetry(ctx, func() error {
		out, created = ConsolidationProposal{}, false
		tx, err := r.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		existing, err := getConsolidationProposalQ(ctx, tx, proposalID)
		switch {
		case err == nil:
			if existing.Status != ConsolidationStatusProposed {
				return fmt.Errorf("%w: %s is %s", ErrConsolidationProposalExists, existing.ID, existing.Status)
			}
			candidate, candidateErr := getItemQ(ctx, tx, existing.CandidateContextItemID)
			if candidateErr != nil || candidate.Source.Type != SourceTypeConsolidationProposal || candidate.Source.Ref != existing.ID {
				return fmt.Errorf("%w: proposal %s", ErrConsolidationInconsistent, existing.ID)
			}
			out = existing
			return nil
		case !errors.Is(err, sql.ErrNoRows):
			return fmt.Errorf("load consolidation proposal %s: %w", proposalID, err)
		}
		if pending, found, err := findProposedConsolidationQ(ctx, tx, in.ProjectID, in.TeamID, ids, proposalID); err != nil {
			return err
		} else if found {
			return fmt.Errorf("%w: %s", ErrConsolidationPending, pending.ID)
		}
		sourceCheck := consolidationSourceCheck{
			projectID: in.ProjectID, teamID: in.TeamID, ids: ids, check: consolidationCheckCreate,
			policyVersion: in.PolicyVersion, support: in.Support, now: time.Now(),
		}
		if in.Expected != nil {
			sourceCheck.frozen = in.Expected.Content
			sourceCheck.aggregates = in.Expected.Aggregates
			if sourceCheck.aggregates == nil {
				sourceCheck.aggregates = map[string]int64{}
			}
		}
		checked, err := r.checkConsolidationSourcesQ(ctx, tx, sourceCheck)
		if err != nil {
			return err
		}
		if len(checked.reasons) > 0 {
			return &ConsolidationSourceError{SourceReasons: checked.reasons}
		}
		first := checked.sources[0]
		candidate := ContextItem{
			ID: candidateID, Kind: first.Kind, Content: text, Scope: first.Scope, Authority: first.Authority,
			TrustLevel: TrustInternal, Priority: first.Priority, Confidence: minimumConfidence(checked.sources),
			Lifecycle: LifecycleCandidate, Source: SourceRef{Type: SourceTypeConsolidationProposal, Ref: proposalID},
			Metadata: map[string]string{"derived_from": strings.Join(ids, ","), "consolidation_proposal": proposalID, "proposal_origin": in.Origin},
		}
		if in.DraftModel != "" {
			candidate.Metadata["draft_model"] = in.DraftModel
		}
		if err = normalize(&candidate); err != nil {
			return err
		}
		var duplicate string
		err = tx.QueryRowContext(ctx, `SELECT id FROM context_items WHERE id=? OR (project_id=? AND kind=? AND content_hash=? AND COALESCE(team_id,'')=? AND COALESCE(session_id,'')=? AND COALESCE(branch_id,'')=? AND COALESCE(agent_id,'')=? AND COALESCE(task_id,'')=? AND COALESCE(attempt_id,'')=?) LIMIT 1`,
			candidate.ID, candidate.Scope.ProjectID, candidate.Kind, candidate.ContentHash, candidate.Scope.TeamID, candidate.Scope.SessionID, candidate.Scope.BranchID, candidate.Scope.AgentID, candidate.Scope.TaskID, candidate.Scope.AttemptID).Scan(&duplicate)
		if err == nil {
			return fmt.Errorf("%w: %s", ErrConsolidationCandidateDuplicate, duplicate)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("check consolidation candidate duplicate: %w", err)
		}
		if err = insertItemRowTx(ctx, tx, candidate); err != nil {
			return fmt.Errorf("insert consolidation candidate: %w", err)
		}
		if err = insertEvent(ctx, tx, "candidate_append", candidate.ID, candidate.Scope, map[string]string{"lifecycle": string(LifecycleCandidate)}); err != nil {
			return err
		}
		if err = consolidationTxStage("candidate"); err != nil {
			return err
		}
		now := time.Now().UTC()
		revisions := make(map[string]string, len(checked.sources))
		for _, source := range checked.sources {
			revisions[source.ID] = source.ContentHash
			if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO context_edges(from_id,relation,to_id,metadata_json,created_at) VALUES(?,?,?,?,?)", candidate.ID, "derived_from", source.ID, "{}", now.UnixMilli()); err != nil {
				return fmt.Errorf("insert derived_from edge: %w", err)
			}
		}
		if err = consolidationTxStage("edges"); err != nil {
			return err
		}
		proposal := ConsolidationProposal{
			ID: proposalID, ProjectID: in.ProjectID, TeamID: in.TeamID, CandidateContextItemID: candidate.ID,
			SourceIDs: ids, SourceRevisions: revisions, AggregateRevisions: checked.aggregateRevisions,
			Status: ConsolidationStatusProposed, CreatedAt: time.UnixMilli(now.UnixMilli()).UTC(),
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO consolidation_proposals(id,project_id,team_id,candidate_context_item_id,source_ids_json,source_revisions_json,aggregate_revisions_json,status,reason,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`,
			proposal.ID, proposal.ProjectID, nilIfEmpty(proposal.TeamID), proposal.CandidateContextItemID, mustJSON(proposal.SourceIDs), mustJSON(proposal.SourceRevisions), mustJSON(proposal.AggregateRevisions), proposal.Status, "", proposal.CreatedAt.UnixMilli()); err != nil {
			return fmt.Errorf("insert consolidation proposal: %w", err)
		}
		if err = consolidationTxStage("proposal"); err != nil {
			return err
		}
		if err = insertEvent(ctx, tx, "consolidation_proposed", candidate.ID, candidate.Scope, map[string]any{"proposal_id": proposal.ID, "source_ids": ids, "origin": in.Origin, "policy_version": in.PolicyVersion}); err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		out, created = proposal, true
		return nil
	})
	return out, created, err
}

// ApproveConsolidationProposal revalidates a pending proposal and confirms
// its candidate and the proposal in one transaction.
func (r *SQLiteRepository) ApproveConsolidationProposal(ctx context.Context, in ConsolidationReviewInput) (ConsolidationProposal, error) {
	var out ConsolidationProposal
	err := r.withBusyRetry(ctx, func() error {
		tx, err := r.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		proposal, err := loadConsolidationForReviewQ(ctx, tx, in)
		if err != nil {
			return err
		}
		if proposal.Status != ConsolidationStatusProposed {
			return fmt.Errorf("%w: %s is %s", ErrConsolidationNotPending, proposal.ID, proposal.Status)
		}
		freshness, err := r.evaluateConsolidationQ(ctx, tx, proposal, consolidationCheckApprove, in.PolicyVersion, in.StaleAfter, time.Now())
		if err != nil {
			return err
		}
		if err := freshness.Err(); err != nil {
			return err
		}
		candidate, err := getItemQ(ctx, tx, proposal.CandidateContextItemID)
		if err != nil {
			return fmt.Errorf("load consolidation candidate: %w", err)
		}
		binding := CandidateBinding{Evidence: EvidenceRef{Type: EvidenceTypeOperatorApproval, Ref: proposal.ID}, Metadata: map[string]string{"approved_by": in.Actor}}
		if err = confirmCandidateTx(ctx, tx, candidate, binding, map[string]string{}); err != nil {
			return err
		}
		if err = consolidationTxStage("approve_candidate"); err != nil {
			return err
		}
		reviewed := time.UnixMilli(time.Now().UnixMilli()).UTC()
		if err = updateConsolidationStatusTx(ctx, tx, proposal.ID, []string{ConsolidationStatusProposed}, ConsolidationStatusApproved, in.Reason, &reviewed); err != nil {
			return err
		}
		if err = insertEvent(ctx, tx, "consolidation_approved", candidate.ID, candidate.Scope, map[string]string{"proposal_id": proposal.ID, "actor": in.Actor}); err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		proposal.Status, proposal.Reason, proposal.ReviewedAt = ConsolidationStatusApproved, in.Reason, &reviewed
		out = proposal
		return nil
	})
	return out, err
}

// RejectConsolidationProposal rejects a pending or stale proposal and its
// candidate in one transaction, binding operator rejection evidence so the
// same content cannot be re-proposed into a candidate later.
func (r *SQLiteRepository) RejectConsolidationProposal(ctx context.Context, in ConsolidationReviewInput) (ConsolidationProposal, error) {
	if strings.TrimSpace(in.Reason) == "" {
		return ConsolidationProposal{}, errors.New("consolidation rejection requires a reason")
	}
	var out ConsolidationProposal
	err := r.withBusyRetry(ctx, func() error {
		tx, err := r.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		proposal, err := loadConsolidationForReviewQ(ctx, tx, in)
		if err != nil {
			return err
		}
		switch proposal.Status {
		case ConsolidationStatusProposed, ConsolidationStatusStale:
		case ConsolidationStatusApproved:
			return fmt.Errorf("%w: %s is approved; replace approved knowledge with hufu context supersede", ErrConsolidationNotPending, proposal.ID)
		default:
			return fmt.Errorf("%w: %s is %s", ErrConsolidationNotPending, proposal.ID, proposal.Status)
		}
		candidate, err := getItemQ(ctx, tx, proposal.CandidateContextItemID)
		if errors.Is(err, sql.ErrNoRows) || (err == nil && (candidate.Lifecycle != LifecycleCandidate || candidate.Source.Type != SourceTypeConsolidationProposal || candidate.Source.Ref != proposal.ID)) {
			return fmt.Errorf("%w: proposal %s", ErrConsolidationInconsistent, proposal.ID)
		}
		if err != nil {
			return fmt.Errorf("load consolidation candidate: %w", err)
		}
		evidence := append([]EvidenceRef(nil), candidate.Evidence...)
		evidence = append(evidence, EvidenceRef{Type: EvidenceTypeOperatorRejection, Ref: proposal.ID})
		metadata := candidate.Metadata
		if metadata == nil {
			metadata = map[string]string{}
		}
		metadata["rejection_reason"] = in.Reason
		now := time.Now().UnixMilli()
		if _, err = tx.ExecContext(ctx, "UPDATE context_items SET evidence_json=?,metadata_json=?,lifecycle=?,updated_at=? WHERE id=?", mustJSON(evidence), mustJSON(metadata), string(LifecycleRejected), now, candidate.ID); err != nil {
			return fmt.Errorf("reject consolidation candidate: %w", err)
		}
		if err = insertEvent(ctx, tx, "lifecycle", candidate.ID, candidate.Scope, map[string]string{"lifecycle": string(LifecycleRejected)}); err != nil {
			return err
		}
		reviewed := time.UnixMilli(now).UTC()
		if err = updateConsolidationStatusTx(ctx, tx, proposal.ID, []string{ConsolidationStatusProposed, ConsolidationStatusStale}, ConsolidationStatusRejected, in.Reason, &reviewed); err != nil {
			return err
		}
		if err = insertEvent(ctx, tx, "consolidation_rejected", candidate.ID, candidate.Scope, map[string]string{"proposal_id": proposal.ID, "actor": in.Actor}); err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		proposal.Status, proposal.Reason, proposal.ReviewedAt = ConsolidationStatusRejected, in.Reason, &reviewed
		out = proposal
		return nil
	})
	return out, err
}

func loadConsolidationForReviewQ(ctx context.Context, q queryer, in ConsolidationReviewInput) (ConsolidationProposal, error) {
	proposal, err := getConsolidationProposalQ(ctx, q, in.ProposalID)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && proposal.ProjectID != in.ProjectID) {
		return ConsolidationProposal{}, fmt.Errorf("%w: %q in project %q", ErrConsolidationNotFound, in.ProposalID, in.ProjectID)
	}
	if err != nil {
		return ConsolidationProposal{}, fmt.Errorf("load consolidation proposal %q: %w", in.ProposalID, err)
	}
	return proposal, nil
}

// updateConsolidationStatusTx moves a proposal from one of from to to and
// requires exactly one row to change.
func updateConsolidationStatusTx(ctx context.Context, tx *sql.Tx, id string, from []string, to, reason string, reviewed *time.Time) error {
	marks := strings.TrimSuffix(strings.Repeat("?,", len(from)), ",")
	args := []any{to, reason}
	query := "UPDATE consolidation_proposals SET status=?,reason=?"
	if reviewed != nil {
		query += ",reviewed_at=?"
		args = append(args, reviewed.UnixMilli())
	}
	args = append(args, id)
	for _, status := range from {
		args = append(args, status)
	}
	result, err := tx.ExecContext(ctx, query+" WHERE id=? AND status IN ("+marks+")", args...)
	if err != nil {
		return fmt.Errorf("update consolidation proposal %s: %w", id, err)
	}
	if rows, err := result.RowsAffected(); err != nil {
		return err
	} else if rows != 1 {
		return fmt.Errorf("%w: %s changed concurrently", ErrConsolidationNotPending, id)
	}
	return nil
}

func minimumConfidence(items []ContextItem) float64 {
	value := 1.0
	for _, item := range items {
		if item.Confidence < value {
			value = item.Confidence
		}
	}
	return value
}
