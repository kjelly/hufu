package team

import (
	"context"
	"errors"
	"fmt"

	"github.com/kjelly/hufu/internal/eventchain"
	"github.com/kjelly/hufu/internal/runtimestore"
)

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
	if ctx == nil {
		ctx = context.Background()
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

	rows, err := store.DB().QueryContext(ctx, `SELECT `+eventColumns+` FROM events ORDER BY sequence`)
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
