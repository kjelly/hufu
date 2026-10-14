package team

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/kjelly/hufu/internal/eventchain"
	"github.com/kjelly/hufu/internal/runtimestore"
)

const eventColumns = `sequence, id, schema_version, previous_id, invocation_id, run_id, session_id,
branch_id, task_id, attempt, actor, type, timestamp, idempotency_key, payload, previous_hash, hash`

type eventStoreState struct {
	lastEventID     string
	lastHash        string
	sequence        int
	runID           string
	sessionID       string
	events          []RunEvent
	idempotencyKeys map[eventIdempotencyIdentity]RunEvent
	// publishedKeys is the already published index a tail load checks for
	// conflicts; it is read, never written. Nil for a full load.
	publishedKeys map[eventIdempotencyIdentity]RunEvent
}

// loadEvents strictly validates the rows after state.sequence, extending
// state and the chain verifier with every event it reads. Rows must be
// contiguous: a gap or reordering is a broken journal, never skipped.
func loadEvents(ctx context.Context, q runtimestore.Queryer, state *eventStoreState, verifier *eventchain.Verifier) error {
	rows, err := q.QueryContext(ctx, `SELECT `+eventColumns+` FROM events WHERE sequence > ? ORDER BY sequence`, state.sequence)
	if err != nil {
		return fmt.Errorf("read events: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		sequence, event, err := scanEventRow(rows)
		if err != nil {
			return fmt.Errorf("decode event %d: %w", state.sequence+1, err)
		}
		if sequence != state.sequence+1 {
			return fmt.Errorf("event journal is not contiguous: found sequence %d after %d", sequence, state.sequence)
		}
		if err := acceptLoadedEvent(state, verifier, event); err != nil {
			return err
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read events: %w", err)
	}
	return nil
}

// scanEventRow decodes one row selected with eventColumns.
func scanEventRow(rows *sql.Rows) (int, RunEvent, error) {
	var sequence int
	var event RunEvent
	var payload []byte
	if err := rows.Scan(&sequence, &event.ID, &event.SchemaVersion, &event.PreviousID, &event.InvocationID,
		&event.RunID, &event.SessionID, &event.BranchID, &event.TaskID, &event.Attempt, &event.Actor,
		&event.Type, &event.Timestamp, &event.IdempotencyKey, &payload, &event.PreviousHash, &event.Hash); err != nil {
		return 0, RunEvent{}, err
	}
	if len(payload) > 0 {
		event.Payload = payload
	}
	return sequence, event, nil
}

func eventChainEntry(event RunEvent) eventchain.Entry {
	return eventchain.Entry{
		ID: event.ID, PreviousID: event.PreviousID, Type: event.Type, Timestamp: event.Timestamp,
		Payload: event.Payload, PreviousHash: event.PreviousHash, Hash: event.Hash,
	}
}

// acceptLoadedEvent verifies event against the chain and folds it into state.
func acceptLoadedEvent(state *eventStoreState, verifier *eventchain.Verifier, event RunEvent) error {
	if err := verifier.Verify(eventChainEntry(event)); err != nil {
		return err
	}
	state.events = append(state.events, event)
	state.lastEventID = event.ID
	state.lastHash = event.Hash
	if event.IdempotencyKey != "" {
		identity := newEventIdempotencyIdentity(event.BranchID, event.IdempotencyKey)
		existing, exists := state.idempotencyKeys[identity]
		if !exists {
			existing, exists = state.publishedKeys[identity]
		}
		if exists && (isDecisionCorrectnessEvent(event.Type) || isDecisionCorrectnessEvent(existing.Type)) {
			equivalent, compareErr := decisionIdempotencyEquivalent(existing, event)
			if compareErr != nil {
				return fmt.Errorf("compare event %d decision idempotency payload: %w", state.sequence+1, compareErr)
			}
			if !equivalent {
				return fmt.Errorf("%w: branch %q key %q", ErrDecisionIdempotencyConflict, identity.branchID, identity.key)
			}
		}
		state.idempotencyKeys[identity] = event
	}
	state.sequence++
	if state.runID == "" && event.RunID != "" {
		state.runID = event.RunID
	}
	if state.sessionID == "" && event.SessionID != "" {
		state.sessionID = event.SessionID
	}
	return nil
}

// insertEvent writes event as the chain position sequence. The primary key
// rejects a position another writer already took.
func insertEvent(ctx context.Context, q runtimestore.Queryer, sequence int, event RunEvent) error {
	scope := ""
	if event.IdempotencyKey != "" {
		scope = newEventIdempotencyIdentity(event.BranchID, event.IdempotencyKey).branchID
	}
	var payload []byte
	if len(event.Payload) > 0 {
		payload = event.Payload
	}
	_, err := q.ExecContext(ctx, `INSERT INTO events (`+eventColumns+`, idempotency_scope)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sequence, event.ID, event.SchemaVersion, event.PreviousID, event.InvocationID, event.RunID, event.SessionID,
		event.BranchID, event.TaskID, event.Attempt, event.Actor, event.Type, event.Timestamp, event.IdempotencyKey,
		payload, event.PreviousHash, event.Hash, scope)
	if err != nil {
		return fmt.Errorf("insert event %d: %w", sequence, err)
	}
	return nil
}

// loadFullState reads and validates the whole journal.
func loadFullState(ctx context.Context, q runtimestore.Queryer, runID, sessionID string) (eventStoreState, error) {
	state := eventStoreState{runID: runID, sessionID: sessionID, idempotencyKeys: make(map[eventIdempotencyIdentity]RunEvent)}
	var verifier eventchain.Verifier
	if err := loadEvents(ctx, q, &state, &verifier); err != nil {
		return eventStoreState{}, err
	}
	return state, nil
}
