package runtimestore

import (
	"context"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
)

const historicalGrantEventType = "historical_results_granted"

var (
	// ErrHistoricalGrantSource means a reference does not name the exact
	// durable grant event on the requested consumer branch and cycle.
	ErrHistoricalGrantSource = errors.New("historical grant source event is missing or mismatched")
	// ErrHistoricalGrantConflict refuses to replace an admitted cycle grant.
	ErrHistoricalGrantConflict = errors.New("historical grant cycle already has a different source")
)

// HistoricalGrantCycleRef indexes an immutable event, not another copy of its
// grants or result payloads. The event store owns canonical validation and must
// write this reference inside the same transaction as the source event.
type HistoricalGrantCycleRef struct {
	BranchID  string `json:"branch_id"`
	CycleID   string `json:"cycle_id"`
	EventID   string `json:"event_id"`
	EventHash string `json:"event_hash"`
}

func (r HistoricalGrantCycleRef) validate() error {
	if r.BranchID == "" || r.CycleID == "" || r.EventID == "" || len(r.EventHash) != 64 {
		return ErrHistoricalGrantSource
	}
	if hash, err := hex.DecodeString(r.EventHash); err != nil || len(hash) != 32 {
		return ErrHistoricalGrantSource
	}
	return nil
}

// PutHistoricalGrantCycleRef preserves first admission. An exact replay is
// idempotent; another event for the same branch/cycle is a conflict. Source
// identity is checked even on retries, so a stale index cannot confer access.
func PutHistoricalGrantCycleRef(ctx context.Context, q Queryer, ref HistoricalGrantCycleRef) error {
	if err := ref.validate(); err != nil {
		return err
	}
	var matches int
	if err := q.QueryRowContext(ctx, `SELECT COUNT(*) FROM events
WHERE id=? AND hash=? AND branch_id=? AND run_id=? AND type=?`,
		ref.EventID, ref.EventHash, ref.BranchID, ref.CycleID, historicalGrantEventType).Scan(&matches); err != nil {
		return fmt.Errorf("verify historical grant event: %w", err)
	}
	if matches != 1 {
		return ErrHistoricalGrantSource
	}
	firstID, firstHash, err := firstHistoricalGrantEvent(ctx, q, ref.BranchID, ref.CycleID)
	if err != nil {
		return err
	}
	if firstID != ref.EventID || firstHash != ref.EventHash {
		return ErrHistoricalGrantConflict
	}
	if _, err := q.ExecContext(ctx, `INSERT INTO historical_grant_cycles(branch_id,cycle_id,source_event_id,source_hash)
VALUES(?,?,?,?) ON CONFLICT(branch_id,cycle_id) DO NOTHING`, ref.BranchID, ref.CycleID, ref.EventID, ref.EventHash); err != nil {
		return fmt.Errorf("index historical grant cycle: %w", err)
	}
	stored, found, err := HistoricalGrantCycle(ctx, q, ref.BranchID, ref.CycleID)
	if err != nil {
		return err
	}
	if !found || stored != ref {
		return ErrHistoricalGrantConflict
	}
	return nil
}

// HistoricalGrantCycle returns only a reference to the canonical grant event.
// Old supported schemas have no references and are read without migration.
// New schemas with a missing table/event or mismatched index fail closed.
func HistoricalGrantCycle(ctx context.Context, q Queryer, branch, cycle string) (HistoricalGrantCycleRef, bool, error) {
	var version int
	if err := q.QueryRowContext(ctx, `SELECT COALESCE(MAX(version),0) FROM schema_migrations`).Scan(&version); err != nil {
		return HistoricalGrantCycleRef{}, false, fmt.Errorf("read historical grant schema: %w", err)
	}
	if version == 3 || version == 4 {
		return HistoricalGrantCycleRef{}, false, nil
	}
	var ref HistoricalGrantCycleRef
	err := q.QueryRowContext(ctx, `SELECT g.branch_id,g.cycle_id,g.source_event_id,g.source_hash
FROM historical_grant_cycles g
WHERE g.branch_id=? AND g.cycle_id=?`, branch, cycle).Scan(&ref.BranchID, &ref.CycleID, &ref.EventID, &ref.EventHash)
	if errors.Is(err, sql.ErrNoRows) {
		return HistoricalGrantCycleRef{}, false, nil
	}
	if err != nil {
		return HistoricalGrantCycleRef{}, false, fmt.Errorf("read historical grant cycle: %w", err)
	}
	if err := ref.validate(); err != nil {
		return HistoricalGrantCycleRef{}, false, err
	}
	firstID, firstHash, err := firstHistoricalGrantEvent(ctx, q, ref.BranchID, ref.CycleID)
	if err != nil {
		return HistoricalGrantCycleRef{}, false, err
	}
	if firstID != ref.EventID || firstHash != ref.EventHash {
		return HistoricalGrantCycleRef{}, false, ErrHistoricalGrantSource
	}
	return ref, true, nil
}

// The journal determines first admission even if its index was lost or
// changed. A later event with a valid hash cannot replace that authority.
func firstHistoricalGrantEvent(ctx context.Context, q Queryer, branch, cycle string) (id, hash string, err error) {
	err = q.QueryRowContext(ctx, `SELECT id,hash FROM events
WHERE branch_id=? AND run_id=? AND type=? ORDER BY sequence LIMIT 1`, branch, cycle, historicalGrantEventType).Scan(&id, &hash)
	if errors.Is(err, sql.ErrNoRows) {
		return "", "", ErrHistoricalGrantSource
	}
	if err != nil {
		return "", "", fmt.Errorf("read first historical grant event: %w", err)
	}
	return id, hash, nil
}
