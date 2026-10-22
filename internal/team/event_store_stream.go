package team

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/kjelly/hufu/internal/eventchain"
	"github.com/kjelly/hufu/internal/runtimestore"
)

// ErrReadLimit means a read-only inspection exhausted its resource budget.
// Callers must discard any partial projection.
var ErrReadLimit = errors.New("read-only inspection resource limit exceeded")

type EventReadLimits struct {
	MaxEvents       int
	MaxPayloadBytes int64
}

// StreamValidatedRunEvents reads the durable event journal without
// constructing an EventStore. It opens runtime.sqlite read-only, so it never
// creates, appends to, repairs, or caches the canonical journal. Each
// validated event is passed to visit in journal order. A callback may receive
// a valid prefix before a later chain error, so callers persisting streamed
// rows must make that persistence transactional.
//
// A workspace without runtime.sqlite is an empty stream. A broken hash chain,
// a non-contiguous journal, read errors, and callback errors are returned.
func StreamValidatedRunEvents(ctx context.Context, workspace string, visit func(RunEvent) error) error {
	return StreamValidatedRunEventsWithLimits(ctx, workspace, EventReadLimits{}, visit)
}

// StreamValidatedRunEventsWithLimits checks the journal's size before loading
// payloads. The preflight and actual read share one SQLite read transaction,
// so concurrent appends cannot invalidate admission. Zero limits are unlimited.
func StreamValidatedRunEventsWithLimits(ctx context.Context, workspace string, limits EventReadLimits, visit func(RunEvent) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if limits.MaxEvents < 0 || limits.MaxPayloadBytes < 0 {
		return fmt.Errorf("stream event store: negative read limit")
	}
	if visit == nil {
		return fmt.Errorf("stream event store: nil visitor")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	store, err := runtimestore.OpenReadOnly(ctx, workspace)
	if errors.Is(err, runtimestore.ErrNotFound) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("open event store: %w", err)
	}
	defer func() { _ = store.Close() }()

	var query runtimestore.Queryer = store.DB()
	if limits.MaxEvents > 0 || limits.MaxPayloadBytes > 0 {
		tx, err := store.DB().BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
		if err != nil {
			return fmt.Errorf("begin bounded event read: %w", err)
		}
		defer func() { _ = tx.Rollback() }()
		if err := preflightEventRead(ctx, tx, limits); err != nil {
			return err
		}
		query = tx
	}
	rows, err := query.QueryContext(ctx, `SELECT `+eventColumns+` FROM events ORDER BY sequence`)
	if err != nil {
		return fmt.Errorf("read events: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var chainVerifier eventchain.Verifier
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		sequence, event, err := scanEventRow(rows)
		if err != nil {
			return fmt.Errorf("decode event %d: %w", chainVerifier.Sequence()+1, err)
		}
		if sequence != chainVerifier.Sequence()+1 {
			return fmt.Errorf("event journal is not contiguous: found sequence %d after %d", sequence, chainVerifier.Sequence())
		}
		if err := chainVerifier.Verify(eventChainEntry(event)); err != nil {
			return err
		}
		if err := visit(event); err != nil {
			return fmt.Errorf("visit event %d (%s): %w", chainVerifier.Sequence(), event.ID, err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read events: %w", err)
	}
	return nil
}

func preflightEventRead(ctx context.Context, query runtimestore.Queryer, limits EventReadLimits) error {
	// Read lengths, not payload blobs. Iteration stops at the first excess row,
	// without allocating a row-count-sized collection or overflowing a sum.
	rows, err := query.QueryContext(ctx, `SELECT coalesce(length(CAST(payload AS BLOB)), 0) FROM events ORDER BY sequence`)
	if err != nil {
		return fmt.Errorf("measure event journal: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var count int
	remaining := limits.MaxPayloadBytes
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return err
		}
		if limits.MaxEvents > 0 && count >= limits.MaxEvents {
			return ErrReadLimit
		}
		count++
		var size int64
		if err := rows.Scan(&size); err != nil {
			return fmt.Errorf("measure event payload: %w", err)
		}
		if limits.MaxPayloadBytes > 0 {
			if size > remaining {
				return ErrReadLimit
			}
			remaining -= size
		}
	}
	return rows.Err()
}

// readValidatedEventJournal strictly validates workspace's whole journal,
// including decision idempotency, without creating or changing any file. A
// workspace without runtime.sqlite has no events.
func readValidatedEventJournal(ctx context.Context, workspace string) ([]RunEvent, error) {
	store, err := runtimestore.OpenReadOnly(ctx, workspace)
	if errors.Is(err, runtimestore.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("open event store read-only: %w", err)
	}
	defer func() { _ = store.Close() }()
	state, err := loadFullState(ctx, store.DB(), "", "")
	if err != nil {
		return nil, err
	}
	return state.events, nil
}
