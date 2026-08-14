package context

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

type MemoryPolicyVersionRecord struct {
	PolicyVersion string
	Snapshot      []byte
	RevisionHash  string
	Status        string
	CreatedAt     time.Time
	AdoptedAt     *time.Time
}

func (r *SQLiteRepository) SaveMemoryPolicyVersion(ctx context.Context, policyVersion string, snapshot []byte, revisionHash, status string, createdAt time.Time) error {
	if policyVersion == "" || len(snapshot) == 0 || revisionHash == "" || status == "" {
		return fmt.Errorf("memory policy version requires id, snapshot, revision hash, and status")
	}
	if createdAt.IsZero() {
		createdAt = time.Now().UTC()
	}
	_, err := r.db.ExecContext(ctx, `INSERT INTO memory_policy_versions(policy_version,snapshot_json,revision_hash,status,created_at) VALUES(?,?,?,?,?) ON CONFLICT(policy_version) DO UPDATE SET status=excluded.status WHERE memory_policy_versions.revision_hash=excluded.revision_hash`, policyVersion, string(snapshot), revisionHash, status, createdAt.UnixMilli())
	if err != nil {
		return err
	}
	var storedHash string
	if err := r.db.QueryRowContext(ctx, "SELECT revision_hash FROM memory_policy_versions WHERE policy_version=?", policyVersion).Scan(&storedHash); err != nil {
		return err
	}
	if storedHash != revisionHash {
		return fmt.Errorf("memory policy version %q already exists with a different revision", policyVersion)
	}
	return nil
}

func (r *SQLiteRepository) SetMemoryPolicyStatus(ctx context.Context, policyVersion, status string, adopted bool) error {
	if policyVersion == "" || status == "" {
		return fmt.Errorf("memory policy version and status are required")
	}
	var adoptedAt any
	if adopted {
		adoptedAt = time.Now().UTC().UnixMilli()
	}
	result, err := r.db.ExecContext(ctx, `UPDATE memory_policy_versions SET status=?,adopted_at=? WHERE policy_version=?`, status, adoptedAt, policyVersion)
	if err != nil {
		return err
	}
	rows, err := result.RowsAffected()
	if err == nil && rows != 1 {
		return fmt.Errorf("memory policy version %q is not recorded", policyVersion)
	}
	return err
}

func (r *SQLiteRepository) ActivateMemoryPolicy(ctx context.Context, activeID, previousID, previousStatus string) error {
	if activeID == "" || previousID == "" || previousStatus == "" || activeID == previousID {
		return fmt.Errorf("active and previous memory policy versions must be distinct and status is required")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC().UnixMilli()
	activeResult, err := tx.ExecContext(ctx, `UPDATE memory_policy_versions SET status='active',adopted_at=? WHERE policy_version=?`, now, activeID)
	if err != nil {
		return err
	}
	previousResult, err := tx.ExecContext(ctx, `UPDATE memory_policy_versions SET status=?,adopted_at=NULL WHERE policy_version=?`, previousStatus, previousID)
	if err != nil {
		return err
	}
	activeRows, activeErr := activeResult.RowsAffected()
	previousRows, previousErr := previousResult.RowsAffected()
	if activeErr != nil || previousErr != nil || activeRows != 1 || previousRows != 1 {
		return fmt.Errorf("memory policy activation references missing versions")
	}
	return tx.Commit()
}

func (r *SQLiteRepository) ActiveMemoryPolicyVersion(ctx context.Context) (MemoryPolicyVersionRecord, error) {
	return scanMemoryPolicyVersion(r.db.QueryRowContext(ctx, `SELECT policy_version,snapshot_json,revision_hash,status,created_at,adopted_at FROM memory_policy_versions WHERE status='active' ORDER BY adopted_at DESC LIMIT 1`))
}

// MemoryPolicyVersion returns the immutable snapshot of a specific policy
// version. It is used by explain-memory to resolve an explicitly requested
// --policy-version to that version's own runtime parameters instead of the
// active policy's (spec §7 HF-MEM4-005).
func (r *SQLiteRepository) MemoryPolicyVersion(ctx context.Context, policyVersion string) (MemoryPolicyVersionRecord, error) {
	return scanMemoryPolicyVersion(r.db.QueryRowContext(ctx, `SELECT policy_version,snapshot_json,revision_hash,status,created_at,adopted_at FROM memory_policy_versions WHERE policy_version=?`, policyVersion))
}

func scanMemoryPolicyVersion(row *sql.Row) (MemoryPolicyVersionRecord, error) {
	var record MemoryPolicyVersionRecord
	var snapshot string
	var created int64
	var adopted sql.NullInt64
	err := row.Scan(&record.PolicyVersion, &snapshot, &record.RevisionHash, &record.Status, &created, &adopted)
	if err != nil {
		return record, err
	}
	record.Snapshot = []byte(snapshot)
	record.CreatedAt = time.UnixMilli(created).UTC()
	if adopted.Valid {
		value := time.UnixMilli(adopted.Int64).UTC()
		record.AdoptedAt = &value
	}
	return record, nil
}
