package context

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

type derivedConsolidation struct {
	proposalID  string
	status      string
	candidateID string
}

// demoteDerivedConsolidationsTx runs inside the transaction that stops
// invalidated items from being current knowledge. Every proposed or approved
// consolidation that uses one of them as a source becomes stale, and an
// approved candidate returns to candidate lifecycle so it leaves every
// confirmed-only read path in the same commit. Demoted candidates are
// processed in turn, so a consolidation of consolidations is demoted too.
func demoteDerivedConsolidationsTx(ctx context.Context, tx execQueryer, invalidated []string, reason ConsolidationReason) error {
	type pending struct {
		id     string
		reason ConsolidationReason
	}
	queue := make([]pending, 0, len(invalidated))
	for _, id := range invalidated {
		queue = append(queue, pending{id: id, reason: reason})
	}
	visited := map[string]bool{}
	for len(queue) > 0 {
		next := queue[0]
		queue = queue[1:]
		if visited[next.id] {
			continue
		}
		visited[next.id] = true
		derived, scope, err := derivedConsolidationsQ(ctx, tx, next.id)
		if err != nil {
			return err
		}
		for _, d := range derived {
			if err := updateConsolidationStatusTx(ctx, tx, d.proposalID, []string{ConsolidationStatusProposed, ConsolidationStatusApproved}, ConsolidationStatusStale, string(next.reason), nil); err != nil {
				return err
			}
			payload := map[string]string{"proposal_id": d.proposalID, "invalidated_source_id": next.id, "reason": string(next.reason)}
			eventScope := scope
			candidate, err := getItemQ(ctx, tx, d.candidateID)
			switch {
			case errors.Is(err, sql.ErrNoRows):
				payload["candidate_link_mismatch"] = "true"
			case err != nil:
				return fmt.Errorf("load consolidation candidate %q: %w", d.candidateID, err)
			case candidate.Source.Type != SourceTypeConsolidationProposal || candidate.Source.Ref != d.proposalID:
				eventScope = candidate.Scope
				payload["candidate_link_mismatch"] = "true"
			default:
				eventScope = candidate.Scope
				if d.status == ConsolidationStatusApproved && candidate.Lifecycle == LifecycleConfirmed {
					if _, err := tx.ExecContext(ctx, "UPDATE context_items SET lifecycle=?,updated_at=? WHERE id=?", string(LifecycleCandidate), time.Now().UnixMilli(), candidate.ID); err != nil {
						return fmt.Errorf("demote consolidation candidate %q: %w", candidate.ID, err)
					}
					if err := insertEvent(ctx, tx, "lifecycle", candidate.ID, candidate.Scope, map[string]string{"lifecycle": string(LifecycleCandidate), "reason": "consolidation_stale"}); err != nil {
						return err
					}
					queue = append(queue, pending{id: candidate.ID, reason: ReasonSourceNotConfirmed})
				}
			}
			if err := insertEvent(ctx, tx, "consolidation_stale", d.candidateID, eventScope, payload); err != nil {
				return err
			}
		}
	}
	return nil
}

// derivedConsolidationsQ lists the proposed or approved consolidations that
// use itemID as a source, reading the proposal's own source set rather than
// edges, which legacy writes may have left incomplete. It returns the
// invalidated item's scope for event provenance.
func derivedConsolidationsQ(ctx context.Context, q queryer, itemID string) ([]derivedConsolidation, Scope, error) {
	item, err := getItemQ(ctx, q, itemID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, Scope{}, nil
	}
	if err != nil {
		return nil, Scope{}, fmt.Errorf("load invalidated item %q: %w", itemID, err)
	}
	rows, err := q.QueryContext(ctx, `SELECT p.id, p.status, p.candidate_context_item_id FROM consolidation_proposals p, json_each(p.source_ids_json) s WHERE p.project_id=? AND p.status IN ('proposed','approved') AND s.value=? ORDER BY p.created_at, p.id`, item.Scope.ProjectID, itemID)
	if err != nil {
		return nil, Scope{}, fmt.Errorf("find consolidations derived from %q: %w", itemID, err)
	}
	defer func() { _ = rows.Close() }()
	var derived []derivedConsolidation
	for rows.Next() {
		var d derivedConsolidation
		if err := rows.Scan(&d.proposalID, &d.status, &d.candidateID); err != nil {
			return nil, Scope{}, fmt.Errorf("scan derived consolidation: %w", err)
		}
		derived = append(derived, d)
	}
	return derived, item.Scope, rows.Err()
}
