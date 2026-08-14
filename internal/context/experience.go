package context

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"sort"
	"time"
)

type ExperienceAggregate struct {
	ContextItemID           string    `json:"context_item_id"`
	PolicyVersion           string    `json:"policy_version"`
	PositiveWeight          float64   `json:"positive_weight"`
	NegativeWeight          float64   `json:"negative_weight"`
	ExposureCount           int       `json:"exposure_count"`
	ConsultedCount          int       `json:"consulted_count"`
	AppliedCount            int       `json:"applied_count"`
	RejectedCount           int       `json:"rejected_count"`
	VerifiedSupportCount    int       `json:"verified_support_count"`
	CausalFailureCount      int       `json:"causal_failure_count"`
	IndependentTaskCount    int       `json:"independent_task_count"`
	IndependentProjectCount int       `json:"independent_project_count"`
	UtilityLowerBound       float64   `json:"utility_lower_bound"`
	LastObservedAt          time.Time `json:"last_observed_at"`
	Revision                int64     `json:"revision"`
}

type ExperienceObservation struct {
	IdempotencyKey       string
	ContextItemID        string
	PolicyVersion        string
	ProjectID            string
	TaskID               string
	ObservedAt           time.Time
	ExposureDelta        int
	ConsultedDelta       int
	AppliedDelta         int
	RejectedDelta        int
	VerifiedSupportDelta int
	CausalFailureDelta   int
	PositiveWeight       float64
	NegativeWeight       float64
	PriorAlpha           float64
	PriorBeta            float64
	UtilityPercentile    float64
}

type ExperienceRepository interface {
	ApplyExperienceObservation(context.Context, ExperienceObservation) (bool, error)
	ExperienceAggregate(context.Context, string, string) (ExperienceAggregate, error)
	ListExperienceAggregates(context.Context, string) ([]ExperienceAggregate, error)
	RebuildExperienceAggregates(context.Context, []ExperienceObservation) error
}

func (r *SQLiteRepository) ExperienceProcessedCount(ctx context.Context) (int, error) {
	var count int
	err := r.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM experience_processed_events").Scan(&count)
	return count, err
}

func (r *SQLiteRepository) ApplyExperienceObservation(ctx context.Context, observation ExperienceObservation) (bool, error) {
	if observation.IdempotencyKey == "" || observation.ContextItemID == "" || observation.PolicyVersion == "" {
		return false, errors.New("experience observation requires idempotency key, context item, and policy version")
	}
	observation = normalizeExperienceObservation(observation)
	processed := false
	err := r.withBusyRetry(ctx, func() error {
		tx, err := r.db.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer func() { _ = tx.Rollback() }()
		processed, err = applyExperienceObservationTx(ctx, tx, observation, "experience_aggregates", "experience_processed_events", "experience_observation_sources")
		if err != nil {
			return err
		}
		if err = tx.Commit(); err != nil {
			return err
		}
		return nil
	})
	return processed, err
}

func normalizeExperienceObservation(observation ExperienceObservation) ExperienceObservation {
	if observation.PriorAlpha <= 0 {
		observation.PriorAlpha = 1
	}
	if observation.PriorBeta <= 0 {
		observation.PriorBeta = 1
	}
	if observation.UtilityPercentile <= 0 || observation.UtilityPercentile >= 1 {
		observation.UtilityPercentile = 0.10
	}
	if observation.ObservedAt.IsZero() {
		observation.ObservedAt = time.Now().UTC()
	}
	return observation
}

type experienceTx interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func applyExperienceObservationTx(ctx context.Context, tx experienceTx, observation ExperienceObservation, aggregatesTable, processedTable, sourcesTable string) (bool, error) {
	result, err := tx.ExecContext(ctx, "INSERT OR IGNORE INTO "+processedTable+"(idempotency_key,processed_at) VALUES(?,?)", observation.IdempotencyKey, observation.ObservedAt.UnixMilli())
	if err != nil {
		return false, err
	}
	rows, err := result.RowsAffected()
	if err != nil || rows == 0 {
		return false, err
	}
	projectID := observation.ProjectID
	var itemProjectID string
	if err = tx.QueryRowContext(ctx, "SELECT project_id FROM context_items WHERE id=?", observation.ContextItemID).Scan(&itemProjectID); err != nil {
		return false, fmt.Errorf("experience context item: %w", err)
	}
	if projectID == "" {
		projectID = itemProjectID
	}
	_, err = tx.ExecContext(ctx, "INSERT INTO "+aggregatesTable+`(context_item_id,policy_version,positive_weight,negative_weight,exposure_count,consulted_count,applied_count,rejected_count,verified_support_count,causal_failure_count,independent_task_count,independent_project_count,utility_lower_bound,last_observed_at,revision) VALUES(?,?,0,0,0,0,0,0,0,0,0,0,0,?,0) ON CONFLICT(context_item_id,policy_version) DO NOTHING`, observation.ContextItemID, observation.PolicyVersion, observation.ObservedAt.UnixMilli())
	if err != nil {
		return false, err
	}
	_, err = tx.ExecContext(ctx, "UPDATE "+aggregatesTable+` SET positive_weight=positive_weight+?, negative_weight=negative_weight+?, exposure_count=exposure_count+?, consulted_count=consulted_count+?, applied_count=applied_count+?, rejected_count=rejected_count+?, verified_support_count=verified_support_count+?, causal_failure_count=causal_failure_count+?, last_observed_at=MAX(last_observed_at,?), revision=revision+1 WHERE context_item_id=? AND policy_version=?`, observation.PositiveWeight, observation.NegativeWeight, observation.ExposureDelta, observation.ConsultedDelta, observation.AppliedDelta, observation.RejectedDelta, observation.VerifiedSupportDelta, observation.CausalFailureDelta, observation.ObservedAt.UnixMilli(), observation.ContextItemID, observation.PolicyVersion)
	if err != nil {
		return false, err
	}
	if observation.TaskID != "" && (observation.AppliedDelta > 0 || observation.PositiveWeight > 0 || observation.NegativeWeight > 0) {
		if _, err = tx.ExecContext(ctx, "INSERT OR IGNORE INTO "+sourcesTable+"(context_item_id,policy_version,task_id,project_id) VALUES(?,?,?,?)", observation.ContextItemID, observation.PolicyVersion, observation.TaskID, projectID); err != nil {
			return false, err
		}
	}
	var positive, negative float64
	if err = tx.QueryRowContext(ctx, "SELECT positive_weight,negative_weight FROM "+aggregatesTable+" WHERE context_item_id=? AND policy_version=?", observation.ContextItemID, observation.PolicyVersion).Scan(&positive, &negative); err != nil {
		return false, err
	}
	utility := BetaQuantile(observation.PriorAlpha+positive, observation.PriorBeta+negative, observation.UtilityPercentile)
	_, err = tx.ExecContext(ctx, "UPDATE "+aggregatesTable+" SET utility_lower_bound=?, independent_task_count=(SELECT COUNT(DISTINCT task_id) FROM "+sourcesTable+" WHERE context_item_id=? AND policy_version=?), independent_project_count=(SELECT COUNT(DISTINCT project_id) FROM "+sourcesTable+" WHERE context_item_id=? AND policy_version=?) WHERE context_item_id=? AND policy_version=?", utility, observation.ContextItemID, observation.PolicyVersion, observation.ContextItemID, observation.PolicyVersion, observation.ContextItemID, observation.PolicyVersion)
	return true, err
}

func (r *SQLiteRepository) ExperienceAggregate(ctx context.Context, itemID, policyVersion string) (ExperienceAggregate, error) {
	return scanExperienceAggregate(r.db.QueryRowContext(ctx, `SELECT context_item_id,policy_version,positive_weight,negative_weight,exposure_count,consulted_count,applied_count,rejected_count,verified_support_count,causal_failure_count,independent_task_count,independent_project_count,utility_lower_bound,last_observed_at,revision FROM experience_aggregates WHERE context_item_id=? AND policy_version=?`, itemID, policyVersion))
}

func (r *SQLiteRepository) ListExperienceAggregates(ctx context.Context, policyVersion string) ([]ExperienceAggregate, error) {
	rows, err := r.db.QueryContext(ctx, `SELECT context_item_id,policy_version,positive_weight,negative_weight,exposure_count,consulted_count,applied_count,rejected_count,verified_support_count,causal_failure_count,independent_task_count,independent_project_count,utility_lower_bound,last_observed_at,revision FROM experience_aggregates WHERE policy_version=? ORDER BY context_item_id`, policyVersion)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []ExperienceAggregate
	for rows.Next() {
		item, scanErr := scanExperienceAggregate(rows)
		if scanErr != nil {
			return nil, scanErr
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func scanExperienceAggregate(row interface{ Scan(...any) error }) (ExperienceAggregate, error) {
	var item ExperienceAggregate
	var observed int64
	err := row.Scan(&item.ContextItemID, &item.PolicyVersion, &item.PositiveWeight, &item.NegativeWeight, &item.ExposureCount, &item.ConsultedCount, &item.AppliedCount, &item.RejectedCount, &item.VerifiedSupportCount, &item.CausalFailureCount, &item.IndependentTaskCount, &item.IndependentProjectCount, &item.UtilityLowerBound, &observed, &item.Revision)
	item.LastObservedAt = time.UnixMilli(observed).UTC()
	return item, err
}

func (r *SQLiteRepository) RebuildExperienceAggregates(ctx context.Context, observations []ExperienceObservation) error {
	sort.SliceStable(observations, func(i, j int) bool { return observations[i].IdempotencyKey < observations[j].IdempotencyKey })
	// Rebuild into temporary copies, then swap all projection rows in one
	// transaction. A failed replay leaves the live projection untouched.
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err = tx.ExecContext(ctx, `DROP TABLE IF EXISTS rebuild_aggregates; DROP TABLE IF EXISTS rebuild_processed; DROP TABLE IF EXISTS rebuild_sources; CREATE TEMP TABLE rebuild_aggregates AS SELECT * FROM experience_aggregates WHERE 0; CREATE UNIQUE INDEX rebuild_aggregates_pk ON rebuild_aggregates(context_item_id,policy_version); CREATE TEMP TABLE rebuild_processed AS SELECT * FROM experience_processed_events WHERE 0; CREATE UNIQUE INDEX rebuild_processed_pk ON rebuild_processed(idempotency_key); CREATE TEMP TABLE rebuild_sources AS SELECT * FROM experience_observation_sources WHERE 0; CREATE UNIQUE INDEX rebuild_sources_pk ON rebuild_sources(context_item_id,policy_version,task_id,project_id);`); err != nil {
		return err
	}
	for _, observation := range observations {
		observation = normalizeExperienceObservation(observation)
		if _, err = applyExperienceObservationTx(ctx, tx, observation, "rebuild_aggregates", "rebuild_processed", "rebuild_sources"); err != nil {
			return err
		}
	}
	if _, err = tx.ExecContext(ctx, `DELETE FROM experience_aggregates; INSERT INTO experience_aggregates SELECT * FROM rebuild_aggregates; DELETE FROM experience_processed_events; INSERT INTO experience_processed_events SELECT * FROM rebuild_processed; DELETE FROM experience_observation_sources; INSERT INTO experience_observation_sources SELECT * FROM rebuild_sources;`); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `DROP TABLE rebuild_aggregates; DROP TABLE rebuild_processed; DROP TABLE rebuild_sources;`); err != nil {
		return err
	}
	return tx.Commit()
}

// BetaQuantile returns the deterministic inverse regularized incomplete beta
// using bisection and a continued fraction. It is sufficient for the small,
// positive weighted counts used by memory-policy-v1.
func BetaQuantile(alpha, beta, percentile float64) float64 {
	if alpha <= 0 || beta <= 0 || percentile <= 0 {
		return 0
	}
	if percentile >= 1 {
		return 1
	}
	lo, hi := 0.0, 1.0
	for range 80 {
		mid := (lo + hi) / 2
		if regularizedBeta(mid, alpha, beta) < percentile {
			lo = mid
		} else {
			hi = mid
		}
	}
	return (lo + hi) / 2
}

func regularizedBeta(x, a, b float64) float64 {
	if x <= 0 {
		return 0
	}
	if x >= 1 {
		return 1
	}
	lga, _ := math.Lgamma(a)
	lgb, _ := math.Lgamma(b)
	lgab, _ := math.Lgamma(a + b)
	front := math.Exp(lgab - lga - lgb + a*math.Log(x) + b*math.Log1p(-x))
	if x < (a+1)/(a+b+2) {
		return front * betaFraction(x, a, b) / a
	}
	return 1 - front*betaFraction(1-x, b, a)/b
}

func betaFraction(x, a, b float64) float64 {
	const tiny = 1e-300
	c := 1.0
	d := 1 - (a+b)*x/(a+1)
	if math.Abs(d) < tiny {
		d = tiny
	}
	d = 1 / d
	h := d
	for m := 1; m <= 200; m++ {
		mf := float64(m)
		m2 := 2 * mf
		aa := mf * (b - mf) * x / ((a + m2 - 1) * (a + m2))
		d = 1 + aa*d
		if math.Abs(d) < tiny {
			d = tiny
		}
		c = 1 + aa/c
		if math.Abs(c) < tiny {
			c = tiny
		}
		d = 1 / d
		h *= d * c
		aa = -(a + mf) * (a + b + mf) * x / ((a + m2) * (a + m2 + 1))
		d = 1 + aa*d
		if math.Abs(d) < tiny {
			d = tiny
		}
		c = 1 + aa/c
		if math.Abs(c) < tiny {
			c = tiny
		}
		d = 1 / d
		delta := d * c
		h *= delta
		if math.Abs(delta-1) < 3e-14 {
			break
		}
	}
	return h
}

var _ ExperienceRepository = (*SQLiteRepository)(nil)
