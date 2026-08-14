package context

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type PromotionType string

const (
	PromotionTypeSkill       PromotionType = "skill"
	PromotionTypeTeamPolicy  PromotionType = "team_policy"
	PromotionTypeAgentPolicy PromotionType = "agent_policy"
)

type PromotionStatus string

const (
	PromotionStatusProposed PromotionStatus = "proposed"
	PromotionStatusApproved PromotionStatus = "approved"
	PromotionStatusRejected PromotionStatus = "rejected"
	PromotionStatusApplied  PromotionStatus = "applied"
	PromotionStatusStale    PromotionStatus = "stale"
)

type PromotionSourceSnapshot struct {
	ContextItemID     string `json:"context_item_id"`
	ContentHash       string `json:"content_hash"`
	AggregateRevision int64  `json:"aggregate_revision"`
}

type PromotionMetrics struct {
	UtilityLowerBound       float64 `json:"utility_lower_bound"`
	AppliedCount            int     `json:"applied_count"`
	RejectedCount           int     `json:"rejected_count"`
	VerifiedSupportCount    int     `json:"verified_support_count"`
	CausalFailureCount      int     `json:"causal_failure_count"`
	IndependentTaskCount    int     `json:"independent_task_count"`
	IndependentProjectCount int     `json:"independent_project_count"`
	AggregateRevision       int64   `json:"aggregate_revision"`
}

type PromotionProposal struct {
	ID              string                    `json:"id"`
	ProjectID       string                    `json:"project_id"`
	TeamID          string                    `json:"team_id"`
	Type            PromotionType             `json:"type"`
	AgentID         string                    `json:"agent_id,omitempty"`
	TargetPath      string                    `json:"target_path"`
	TargetBaseHash  string                    `json:"target_base_hash"`
	Draft           string                    `json:"draft,omitempty"`
	DraftHash       string                    `json:"draft_hash"`
	PolicyVersion   string                    `json:"policy_version"`
	Sources         []PromotionSourceSnapshot `json:"sources"`
	Metrics         PromotionMetrics          `json:"metrics"`
	Status          PromotionStatus           `json:"status"`
	RejectionReason string                    `json:"rejection_reason,omitempty"`
	CreatedAt       time.Time                 `json:"created_at"`
	UpdatedAt       time.Time                 `json:"updated_at"`
	AppliedAt       *time.Time                `json:"applied_at,omitempty"`
}

type PromotionOutboxEvent struct {
	IdempotencyKey string          `json:"idempotency_key"`
	EventType      string          `json:"event_type"`
	Payload        json.RawMessage `json:"payload"`
	CreatedAt      time.Time       `json:"created_at"`
}

func HashPromotionContent(content string) string {
	sum := sha256.Sum256([]byte(content))
	return hex.EncodeToString(sum[:])
}

// PromotionProposalID excludes DraftHash so an operator edit preserves identity.
func PromotionProposalID(p PromotionProposal) string {
	sources := append([]PromotionSourceSnapshot(nil), p.Sources...)
	sort.Slice(sources, func(i, j int) bool { return sources[i].ContextItemID < sources[j].ContextItemID })
	var b strings.Builder
	fmt.Fprintf(&b, "%s\x00%s\x00%s\x00%s\x00%s\x00%s\x00", p.Type, p.ProjectID, p.TeamID, p.AgentID, p.TargetPath, p.TargetBaseHash)
	for _, source := range sources {
		fmt.Fprintf(&b, "%s\x00%s\x00%d\x00", source.ContextItemID, source.ContentHash, source.AggregateRevision)
	}
	b.WriteString(p.PolicyVersion)
	sum := sha256.Sum256([]byte(b.String()))
	return "promo-" + hex.EncodeToString(sum[:12])
}

func validatePromotionTransition(from, to PromotionStatus) bool {
	if from == PromotionStatusProposed && (to == PromotionStatusApproved || to == PromotionStatusRejected || to == PromotionStatusStale) {
		return true
	}
	return from == PromotionStatusApproved && (to == PromotionStatusApplied || to == PromotionStatusStale)
}

func (r *SQLiteRepository) CreatePromotion(ctx context.Context, p PromotionProposal, event PromotionOutboxEvent) (PromotionProposal, bool, error) {
	if err := normalizePromotionProposal(&p); err != nil {
		return PromotionProposal{}, false, err
	}
	var created bool
	err := r.withBusyRetry(ctx, func() error {
		tx, err := r.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		metrics, err := json.Marshal(p.Metrics)
		if err != nil {
			return err
		}
		res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO promotion_proposals(id,project_id,team_id,type,agent_id,target_path,target_base_hash,draft,draft_hash,policy_version,status,metrics_json,rejection_reason,created_at,updated_at,applied_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,NULL)`, p.ID, p.ProjectID, p.TeamID, p.Type, nilIfEmpty(p.AgentID), p.TargetPath, p.TargetBaseHash, p.Draft, p.DraftHash, p.PolicyVersion, p.Status, string(metrics), p.RejectionReason, p.CreatedAt.UnixMilli(), p.UpdatedAt.UnixMilli())
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		created = n == 1
		if created {
			for _, source := range p.Sources {
				if _, err = tx.ExecContext(ctx, `INSERT INTO promotion_sources(proposal_id,context_item_id,content_hash,aggregate_revision) VALUES(?,?,?,?)`, p.ID, source.ContextItemID, source.ContentHash, source.AggregateRevision); err != nil {
					return err
				}
			}
			if err = insertPromotionOutbox(ctx, tx, event); err != nil {
				return err
			}
		}
		return tx.Commit()
	})
	if err != nil {
		return PromotionProposal{}, false, err
	}
	stored, err := r.GetPromotion(ctx, p.ID, p.ProjectID, p.TeamID)
	return stored, created, err
}

func normalizePromotionProposal(p *PromotionProposal) error {
	if p.ID == "" {
		p.ID = PromotionProposalID(*p)
	}
	if p.DraftHash == "" {
		p.DraftHash = HashPromotionContent(p.Draft)
	}
	if p.Status == "" {
		p.Status = PromotionStatusProposed
	}
	if p.ID == "" || p.ProjectID == "" || p.TeamID == "" || p.TargetPath == "" || p.PolicyVersion == "" || len(p.Sources) == 0 {
		return errors.New("promotion proposal requires identity, scope, target, policy version, and sources")
	}
	if p.Status != PromotionStatusProposed {
		return errors.New("new promotion proposal must have proposed status")
	}
	if p.Type != PromotionTypeSkill && p.Type != PromotionTypeTeamPolicy && p.Type != PromotionTypeAgentPolicy {
		return fmt.Errorf("invalid promotion type %q", p.Type)
	}
	if p.Type == PromotionTypeAgentPolicy && p.AgentID == "" {
		return errors.New("agent policy proposal requires an agent ID")
	}
	if p.Type != PromotionTypeAgentPolicy && p.AgentID != "" {
		return errors.New("only agent policy proposals may carry an agent ID")
	}
	cleanTarget := filepath.Clean(filepath.FromSlash(p.TargetPath))
	if filepath.IsAbs(cleanTarget) || cleanTarget == ".." || strings.HasPrefix(cleanTarget, ".."+string(filepath.Separator)) || filepath.ToSlash(cleanTarget) != p.TargetPath {
		return errors.New("promotion target must be a clean relative path")
	}
	if p.DraftHash != HashPromotionContent(p.Draft) || p.ID != PromotionProposalID(*p) {
		return errors.New("promotion proposal identity or draft hash mismatch")
	}
	if p.Type == PromotionTypeSkill {
		parts := strings.Split(p.TargetPath, "/")
		if len(parts) != 3 || parts[0] != "skills" || parts[2] != "SKILL.md" {
			return errors.New("skill promotion target must be skills/<name>/SKILL.md")
		}
	} else if strings.Contains(p.TargetPath, "/") || !strings.HasSuffix(strings.ToLower(p.TargetPath), ".md") {
		return errors.New("policy promotion target must be a root agent Markdown file")
	}
	for _, source := range p.Sources {
		if source.ContextItemID == "" || source.ContentHash == "" {
			return errors.New("promotion source snapshots require ID and content hash")
		}
	}
	now := time.Now().UTC()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	if p.UpdatedAt.IsZero() {
		p.UpdatedAt = p.CreatedAt
	}
	return nil
}

func (r *SQLiteRepository) GetPromotion(ctx context.Context, id, projectID, teamID string) (PromotionProposal, error) {
	row := r.db.QueryRowContext(ctx, `SELECT id,project_id,team_id,type,COALESCE(agent_id,''),target_path,target_base_hash,draft,draft_hash,policy_version,status,metrics_json,rejection_reason,created_at,updated_at,applied_at FROM promotion_proposals WHERE id=? AND project_id=? AND team_id=?`, id, projectID, teamID)
	p, err := scanPromotion(row)
	if err != nil {
		return p, err
	}
	p.Sources, err = r.promotionSources(ctx, p.ID)
	return p, err
}

func (r *SQLiteRepository) ListPromotions(ctx context.Context, projectID, teamID string) ([]PromotionProposal, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT id,project_id,team_id,type,COALESCE(agent_id,''),target_path,target_base_hash,draft,draft_hash,policy_version,status,metrics_json,rejection_reason,created_at,updated_at,applied_at FROM promotion_proposals WHERE project_id=? AND team_id=? ORDER BY created_at DESC,id`, projectID, teamID)
	if err != nil {
		return nil, err
	}
	var out []PromotionProposal
	for rows.Next() {
		p, e := scanPromotion(rows)
		if e != nil {
			_ = rows.Close()
			return nil, e
		}
		out = append(out, p)
	}
	if err = rows.Err(); err != nil {
		_ = rows.Close()
		return nil, err
	}
	if err = rows.Close(); err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Sources, err = r.promotionSources(ctx, out[i].ID)
		if err != nil {
			return nil, err
		}
	}
	return out, nil
}

func scanPromotion(row interface{ Scan(...any) error }) (PromotionProposal, error) {
	var p PromotionProposal
	var metrics string
	var created, updated int64
	var applied sql.NullInt64
	err := row.Scan(&p.ID, &p.ProjectID, &p.TeamID, &p.Type, &p.AgentID, &p.TargetPath, &p.TargetBaseHash, &p.Draft, &p.DraftHash, &p.PolicyVersion, &p.Status, &metrics, &p.RejectionReason, &created, &updated, &applied)
	if err != nil {
		return p, err
	}
	if err = json.Unmarshal([]byte(metrics), &p.Metrics); err != nil {
		return p, err
	}
	p.CreatedAt = time.UnixMilli(created).UTC()
	p.UpdatedAt = time.UnixMilli(updated).UTC()
	if applied.Valid {
		v := time.UnixMilli(applied.Int64).UTC()
		p.AppliedAt = &v
	}
	return p, nil
}

func (r *SQLiteRepository) promotionSources(ctx context.Context, id string) ([]PromotionSourceSnapshot, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT context_item_id,content_hash,aggregate_revision FROM promotion_sources WHERE proposal_id=? ORDER BY context_item_id`, id)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []PromotionSourceSnapshot
	for rows.Next() {
		var s PromotionSourceSnapshot
		if err = rows.Scan(&s.ContextItemID, &s.ContentHash, &s.AggregateRevision); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (r *SQLiteRepository) UpdatePromotionDraft(ctx context.Context, id, projectID, teamID, draft, draftHash string, event PromotionOutboxEvent) (PromotionProposal, error) {
	return r.mutatePromotion(ctx, id, projectID, teamID, event, func(tx *sql.Tx, p PromotionProposal) error {
		if p.Status != PromotionStatusProposed {
			return fmt.Errorf("proposal %s is %s; only proposed drafts can be edited", id, p.Status)
		}
		_, err := tx.ExecContext(ctx, `UPDATE promotion_proposals SET draft=?,draft_hash=?,updated_at=? WHERE id=?`, draft, draftHash, time.Now().UTC().UnixMilli(), id)
		return err
	})
}

func (r *SQLiteRepository) TransitionPromotion(ctx context.Context, id, projectID, teamID string, to PromotionStatus, reason string, event PromotionOutboxEvent) (PromotionProposal, error) {
	return r.mutatePromotion(ctx, id, projectID, teamID, event, func(tx *sql.Tx, p PromotionProposal) error {
		if !validatePromotionTransition(p.Status, to) {
			return fmt.Errorf("invalid promotion transition %s -> %s", p.Status, to)
		}
		now := time.Now().UTC().UnixMilli()
		var applied any
		if to == PromotionStatusApplied {
			applied = now
		}
		_, err := tx.ExecContext(ctx, `UPDATE promotion_proposals SET status=?,rejection_reason=?,updated_at=?,applied_at=? WHERE id=?`, to, reason, now, applied, id)
		return err
	})
}

func (r *SQLiteRepository) mutatePromotion(ctx context.Context, id, projectID, teamID string, event PromotionOutboxEvent, fn func(*sql.Tx, PromotionProposal) error) (PromotionProposal, error) {
	err := r.withBusyRetry(ctx, func() error {
		tx, err := r.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		p, err := scanPromotion(tx.QueryRowContext(ctx, `SELECT id,project_id,team_id,type,COALESCE(agent_id,''),target_path,target_base_hash,draft,draft_hash,policy_version,status,metrics_json,rejection_reason,created_at,updated_at,applied_at FROM promotion_proposals WHERE id=? AND project_id=? AND team_id=?`, id, projectID, teamID))
		if err != nil {
			return err
		}
		if err = fn(tx, p); err != nil {
			return err
		}
		if err = insertPromotionOutbox(ctx, tx, event); err != nil {
			return err
		}
		return tx.Commit()
	})
	if err != nil {
		return PromotionProposal{}, err
	}
	return r.GetPromotion(ctx, id, projectID, teamID)
}

func insertPromotionOutbox(ctx context.Context, tx *sql.Tx, event PromotionOutboxEvent) error {
	if event.IdempotencyKey == "" {
		return errors.New("promotion lifecycle event requires idempotency key")
	}
	if !json.Valid(event.Payload) {
		return errors.New("promotion lifecycle event payload must be valid JSON")
	}
	if event.CreatedAt.IsZero() {
		event.CreatedAt = time.Now().UTC()
	}
	_, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO promotion_event_outbox(idempotency_key,event_type,payload_json,created_at) VALUES(?,?,?,?)`, event.IdempotencyKey, event.EventType, string(event.Payload), event.CreatedAt.UnixMilli())
	return err
}

func (r *SQLiteRepository) PendingPromotionEvents(ctx context.Context) ([]PromotionOutboxEvent, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT idempotency_key,event_type,payload_json,created_at FROM promotion_event_outbox WHERE delivered_at IS NULL ORDER BY created_at,idempotency_key`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []PromotionOutboxEvent
	for rows.Next() {
		var e PromotionOutboxEvent
		var payload string
		var created int64
		if err = rows.Scan(&e.IdempotencyKey, &e.EventType, &payload, &created); err != nil {
			return nil, err
		}
		e.Payload = json.RawMessage(payload)
		e.CreatedAt = time.UnixMilli(created).UTC()
		out = append(out, e)
	}
	return out, rows.Err()
}
func (r *SQLiteRepository) MarkPromotionEventDelivered(ctx context.Context, key string) error {
	_, err := r.db.ExecContext(ctx, `UPDATE promotion_event_outbox SET delivered_at=? WHERE idempotency_key=? AND delivered_at IS NULL`, time.Now().UTC().UnixMilli(), key)
	return err
}

func (r *SQLiteRepository) EnqueuePromotionEvent(ctx context.Context, event PromotionOutboxEvent) error {
	return r.withBusyRetry(ctx, func() error {
		tx, err := r.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		if err = insertPromotionOutbox(ctx, tx, event); err != nil {
			return err
		}
		return tx.Commit()
	})
}
