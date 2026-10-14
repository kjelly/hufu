package team

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/kjelly/hufu/internal/eventchain"
	"github.com/kjelly/hufu/internal/runtimestore"
)

// eventLogLineLimit bounds one exported event line, matching the retired
// file log's scanner limit.
const eventLogLineLimit = 4 * 1024 * 1024

// ExportEventLog writes workspace's validated journal to w as JSONL, one event
// per line in journal order: the format the retired event_store.jsonl used and
// audit bundles still carry. A workspace without runtime.sqlite exports
// nothing. A chain error stops the export after the valid prefix.
func ExportEventLog(ctx context.Context, workspace string, w io.Writer) error {
	return StreamValidatedRunEvents(ctx, workspace, func(event RunEvent) error {
		data, err := json.Marshal(event)
		if err != nil {
			return fmt.Errorf("marshal event: %w", err)
		}
		if _, err := w.Write(append(data, '\n')); err != nil {
			return fmt.Errorf("write event: %w", err)
		}
		return nil
	})
}

// ErrEventLogInvalid reports an imported event log whose chain, sequence, or
// decision idempotency does not validate.
var ErrEventLogInvalid = errors.New("event log is invalid")

// ImportEventLog loads a JSONL event log into workspace's empty journal in one
// transaction, creating runtime.sqlite when needed. Every event is validated
// as the journal's readers validate it, then written verbatim: ids,
// timestamps, hashes, and payload bytes are kept exactly as read. A journal
// that already has events, malformed JSON, and a log that fails validation
// (ErrEventLogInvalid) import nothing. It returns the number of events
// imported.
func ImportEventLog(ctx context.Context, workspace string, r io.Reader) (int, error) {
	store, err := runtimestore.Open(ctx, workspace)
	if err != nil {
		return 0, fmt.Errorf("import event log: %w", err)
	}
	defer func() { _ = store.Close() }()
	state := eventStoreState{idempotencyKeys: make(map[eventIdempotencyIdentity]RunEvent)}
	err = store.WithTx(ctx, func(tx *sql.Tx) error {
		var existing int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM events`).Scan(&existing); err != nil {
			return fmt.Errorf("read journal size: %w", err)
		}
		if existing > 0 {
			return fmt.Errorf("journal already has %d events", existing)
		}
		var verifier eventchain.Verifier
		scanner := bufio.NewScanner(r)
		scanner.Buffer(make([]byte, 0, 64*1024), eventLogLineLimit)
		for scanner.Scan() {
			line := bytes.TrimSpace(scanner.Bytes())
			if len(line) == 0 {
				continue
			}
			var event RunEvent
			if err := json.Unmarshal(line, &event); err != nil {
				return fmt.Errorf("decode event %d: %w", state.sequence+1, err)
			}
			if err := acceptLoadedEvent(&state, &verifier, event); err != nil {
				return fmt.Errorf("%w: %w", ErrEventLogInvalid, err)
			}
			if err := insertEvent(ctx, tx, state.sequence, event); err != nil {
				return err
			}
		}
		if err := scanner.Err(); err != nil {
			return fmt.Errorf("read event log: %w", err)
		}
		return nil
	})
	if err != nil {
		return 0, fmt.Errorf("import event log: %w", err)
	}
	return state.sequence, nil
}

// ReadUnvalidatedRunEvents returns runID's events in journal order without
// validating the chain, so diagnostics can still find a run in a damaged
// journal. It also counts events that carry no run id. A workspace without
// runtime.sqlite returns an error wrapping os.ErrNotExist.
func ReadUnvalidatedRunEvents(ctx context.Context, workspace, runID string) (events []RunEvent, unscoped int, err error) {
	store, err := runtimestore.OpenReadOnly(ctx, workspace)
	if err != nil {
		return nil, 0, fmt.Errorf("open event store read-only: %w", err)
	}
	defer func() { _ = store.Close() }()
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE run_id = ''`).Scan(&unscoped); err != nil {
		return nil, 0, fmt.Errorf("count unscoped events: %w", err)
	}
	rows, err := store.DB().QueryContext(ctx, `SELECT `+eventColumns+` FROM events WHERE run_id = ? ORDER BY sequence`, runID)
	if err != nil {
		return nil, 0, fmt.Errorf("read run events: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		_, event, err := scanEventRow(rows)
		if err != nil {
			return nil, 0, fmt.Errorf("decode run event: %w", err)
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("read run events: %w", err)
	}
	return events, unscoped, nil
}

// EventJournalExists reports whether workspace has an event journal, and
// fails for a workspace that still uses the retired file-based log.
func EventJournalExists(ctx context.Context, workspace string) (bool, error) {
	store, err := runtimestore.OpenReadOnly(ctx, workspace)
	if errors.Is(err, runtimestore.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("open event store read-only: %w", err)
	}
	return true, store.Close()
}
