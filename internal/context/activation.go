package context

import (
	"context"
	"fmt"
	"time"
)

// ActivationRecord is the queryable projection of legacy activation.*
// metadata. Metadata remains readable for compatibility; this projection is
// maintained by migration triggers and is safe to index without changing a
// context item's canonical content identity.
type ActivationRecord struct {
	ContextItemID                                                           string
	Phases, Triggers, Roles, Capabilities, Tools, ErrorClasses, Environment string
}

func (r *SQLiteRepository) ActivationForItem(ctx context.Context, id string) (ActivationRecord, error) {
	var record ActivationRecord
	err := r.db.QueryRowContext(ctx, `SELECT context_item_id,phases,triggers,roles,capabilities,tools,error_classes,environment FROM context_activation WHERE context_item_id=?`, id).Scan(&record.ContextItemID, &record.Phases, &record.Triggers, &record.Roles, &record.Capabilities, &record.Tools, &record.ErrorClasses, &record.Environment)
	return record, err
}

type ContextOutcomeObservation struct {
	IdempotencyKey, ContextItemID, Phase, Trigger, AgentRole, Environment, Outcome, PolicyRevision string
	RequestID, ManifestFingerprint, RunID, TaskID, ModelExecutionID                                string
	VerificationOutcome, AcceptanceOutcome, JudgeOutcome, SkepticOutcome                           string
	Attempt                                                                                        int
	ObservedAt                                                                                     time.Time
}

func (r *SQLiteRepository) RecordContextOutcomeObservation(ctx context.Context, observation ContextOutcomeObservation) (bool, error) {
	if observation.IdempotencyKey == "" || observation.ContextItemID == "" || observation.Phase == "" || observation.Trigger == "" || observation.Outcome == "" {
		return false, fmt.Errorf("context outcome observation requires identity, dimensions, and outcome")
	}
	if observation.ObservedAt.IsZero() {
		observation.ObservedAt = time.Now().UTC()
	}
	result, err := r.db.ExecContext(ctx, `INSERT OR IGNORE INTO context_outcome_observations(idempotency_key,context_item_id,phase,trigger,agent_role,environment,outcome,policy_revision,observed_at,request_id,manifest_fingerprint,run_id,task_id,attempt,model_execution_id,verification_outcome,acceptance_outcome,judge_outcome,skeptic_outcome) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`, observation.IdempotencyKey, observation.ContextItemID, observation.Phase, observation.Trigger, observation.AgentRole, observation.Environment, observation.Outcome, observation.PolicyRevision, observation.ObservedAt.UnixMilli(), observation.RequestID, observation.ManifestFingerprint, observation.RunID, observation.TaskID, observation.Attempt, observation.ModelExecutionID, observation.VerificationOutcome, observation.AcceptanceOutcome, observation.JudgeOutcome, observation.SkepticOutcome)
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	return rows == 1, err
}

func (r *SQLiteRepository) ContextOutcomeCount(ctx context.Context, contextID, phase, trigger, role, environment, outcome string) (int, error) {
	var count int
	err := r.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM context_outcome_observations WHERE context_item_id=? AND phase=? AND trigger=? AND agent_role=? AND environment=? AND outcome=?`, contextID, phase, trigger, role, environment, outcome).Scan(&count)
	return count, err
}
