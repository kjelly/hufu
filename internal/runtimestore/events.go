package runtimestore

import (
	"context"
	"errors"
	"fmt"

	"github.com/kjelly/hufu/internal/eventchain"
)

// VerifyEventChain opens workspace's runtime.sqlite read-only and checks that
// the events table is one contiguous, unbroken hash chain. A workspace
// without runtime.sqlite has nothing to verify. Packages below the team
// runtime use it; the event store itself also validates decision
// idempotency.
func VerifyEventChain(ctx context.Context, workspace string) error {
	store, err := OpenReadOnly(ctx, workspace)
	if errors.Is(err, ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { _ = store.Close() }()
	rows, err := store.db.QueryContext(ctx, `SELECT sequence, id, previous_id, type, timestamp, payload, previous_hash, hash FROM events ORDER BY sequence`)
	if err != nil {
		return fmt.Errorf("read events: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var verifier eventchain.Verifier
	for rows.Next() {
		var sequence int
		var entry eventchain.Entry
		var payload []byte
		if err := rows.Scan(&sequence, &entry.ID, &entry.PreviousID, &entry.Type, &entry.Timestamp, &payload, &entry.PreviousHash, &entry.Hash); err != nil {
			return fmt.Errorf("decode event %d: %w", verifier.Sequence()+1, err)
		}
		if sequence != verifier.Sequence()+1 {
			return fmt.Errorf("event journal is not contiguous: found sequence %d after %d", sequence, verifier.Sequence())
		}
		if len(payload) > 0 {
			entry.Payload = payload
		}
		if err := verifier.Verify(entry); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read events: %w", err)
	}
	return nil
}
