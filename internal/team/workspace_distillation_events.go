package team

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"maps"
	"slices"

	"github.com/kjelly/hufu/internal/eventchain"
	"github.com/kjelly/hufu/internal/taskdistill"
)

// Validate the complete journal with the ordinary loader's acceptance rules,
// without retaining historical payloads. The digest covers every persisted
// field, including metadata outside the event hash contract. A second streaming
// pass per branch uses the canonical task reducer without loading the journal.
func readRetiredDistillationEvents(ctx context.Context, db *sql.DB) (map[string][]*TodoItem, string, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+eventColumns+` FROM events ORDER BY sequence`)
	if err != nil {
		return nil, "", err
	}
	defer func() { _ = rows.Close() }()
	state := eventStoreState{idempotencyKeys: make(map[eventIdempotencyIdentity]RunEvent)}
	var verifier eventchain.Verifier
	hash := sha256.New()
	branches := map[string]bool{}
	for rows.Next() {
		sequence, event, err := scanEventRow(rows)
		if err != nil {
			return nil, "", err
		}
		if sequence != state.sequence+1 {
			return nil, "", fmt.Errorf("event journal is not contiguous: sequence %d after %d", sequence, state.sequence)
		}
		if err = acceptLoadedEvent(&state, &verifier, event); err != nil {
			return nil, "", err
		}
		// Only these event families compare payloads for duplicate keys. All
		// other families need identity/type only, including a later collision
		// with a correctness event (which rejects a different type).
		if event.IdempotencyKey != "" && !isDecisionCorrectnessEvent(event.Type) && event.Type != string(EventHistoricalResultsGranted) && event.Type != string(EventDerivedForkCreated) {
			compact := event
			compact.Payload = nil
			state.idempotencyKeys[newEventIdempotencyIdentity(event.BranchID, event.IdempotencyKey)] = compact
		}
		clear(state.events)
		state.events = state.events[:0]
		raw, err := taskdistill.MarshalSnapshot(event)
		if err != nil {
			return nil, "", err
		}
		_, _ = hash.Write(raw)
		_, _ = hash.Write([]byte{'\n'})
		branches[event.BranchID] = true
	}
	if err = rows.Err(); err != nil {
		return nil, "", err
	}
	if err = rows.Close(); err != nil {
		return nil, "", err
	}
	groups := map[string][]*TodoItem{}
	for _, branch := range slices.Sorted(maps.Keys(branches)) {
		rows, err := db.QueryContext(ctx, `SELECT `+eventColumns+` FROM events WHERE branch_id = ? ORDER BY sequence`, branch)
		if err != nil {
			return nil, "", err
		}
		var readErr error
		tasks := reduceTodoEventStream(func(yield func(RunEvent) bool) {
			for rows.Next() {
				_, event, err := scanEventRow(rows)
				if err != nil {
					readErr = err
					return
				}
				if !yield(event) {
					return
				}
			}
			readErr = rows.Err()
		})
		closeErr := rows.Close()
		if err = errors.Join(readErr, closeErr); err != nil {
			return nil, "", err
		}
		groups[branch] = tasks.tasks
	}
	return groups, hex.EncodeToString(hash.Sum(nil)), nil
}
