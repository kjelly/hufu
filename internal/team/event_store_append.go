package team

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/kjelly/hufu/internal/runtimestore"
)

// ErrEventBatchConflict rejects partial or differently bound replay of an atomic batch.
var ErrEventBatchConflict = errors.New("event batch conflicts with durable events")

// AppendPersistedBatchAtHead commits an ordered event batch and its indexes in
// one transaction. Every multi-event member needs a distinct idempotency key.
func (es *EventStore) AppendPersistedBatchAtHead(ctx context.Context, events []RunEvent, headID string) ([]RunEvent, error) {
	return es.appendPersistedEvents(ctx, events, &headID)
}

func (es *EventStore) appendPersistedEvents(ctx context.Context, requested []RunEvent, expectedHead *string) ([]RunEvent, error) {
	if len(requested) == 0 {
		return nil, fmt.Errorf("event batch is empty")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := es.acquire(ctx); err != nil {
		return nil, err
	}
	defer es.release()

	if es.closed {
		return nil, fmt.Errorf("event store closed")
	}
	if es.readOnly {
		return nil, fmt.Errorf("event store is read-only")
	}
	if es.degraded || !es.stateValid {
		if err := es.reload(); err != nil {
			return nil, fmt.Errorf("recover degraded event store: %w", err)
		}
	}
	if es.commitHook == nil {
		err := fmt.Errorf("event store commit hook is unavailable")
		es.invalidateState(err)
		return nil, err
	}
	// An open connection keeps writing to a removed or replaced file, where
	// no later reader would see the event. Refuse; the next append reopens.
	if err := es.store.CheckCurrent(); err != nil {
		es.invalidateState(err)
		return nil, err
	}
	conn, err := es.store.DB().Conn(ctx)
	if err != nil {
		return nil, fmt.Errorf("acquire event store connection: %w", err)
	}
	defer func() { _ = conn.Close() }()
	// The transaction deliberately ignores ctx: database/sql rolls back a
	// transaction whose context ends, which would turn an admitted append
	// into a silent partial outcome.
	dbctx := context.WithoutCancel(ctx)
	tx, err := conn.BeginTx(dbctx, nil)
	if err != nil {
		if runtimestore.IsBusy(err) {
			return nil, fmt.Errorf("%w: %v", ErrEventStoreWriterUnavailable, err)
		}
		return nil, fmt.Errorf("begin event append: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	failClosed := func(err error) ([]RunEvent, error) {
		es.invalidateState(err)
		return nil, err
	}

	// The tail load and publication are inside the write transaction so the
	// idempotency index and chain head include every committed writer event.
	if err := es.syncCommittedEvents(dbctx, tx); err != nil {
		return failClosed(fmt.Errorf("load events committed by other writers: %w", err))
	}
	if expectedHead != nil && es.lastEventID != *expectedHead {
		return nil, ErrEventHeadChanged
	}

	if prior, found, err := es.replayedEventBatch(dbctx, tx, requested); err != nil || found {
		return prior, err
	}
	prepared, err := es.prepareEventBatch(requested)
	if err != nil {
		return nil, err
	}
	for i, event := range prepared {
		if err := insertEvent(dbctx, tx, es.sequence+i+1, event); err != nil {
			return failClosed(fmt.Errorf("write run event: %w", err))
		}
		if event.Type == "run_finished" {
			history := append(slices.Clone(es.cachedEvents), prepared[:i]...)
			if err := persistRunReport(dbctx, tx, history, event); err != nil {
				return failClosed(fmt.Errorf("write run report: %w", err))
			}
		}
	}
	committed = true
	if err := tx.Commit(); err != nil {
		return failClosed(fmt.Errorf("commit run event: %w", err))
	}
	// Return the pool's only connection before the hook runs.
	_ = conn.Close()

	for _, event := range prepared {
		es.sequence++
		es.lastEventID, es.lastHash = event.ID, event.Hash
		if event.IdempotencyKey != "" {
			identity := newEventIdempotencyIdentity(event.BranchID, event.IdempotencyKey)
			es.idempotencyKeys[identity] = cloneRunEvent(event)
		}
		es.cachedEvents = append(es.cachedEvents, cloneRunEvent(event))
	}
	if err := es.commitHook(); err != nil {
		return failClosed(fmt.Errorf("sync run event: %w", err))
	}
	return prepared, nil
}

// Staging changes only the chain cursor under the store semaphore. No cache or
// index is published until the complete database transaction commits.
func (es *EventStore) prepareEventBatch(requested []RunEvent) ([]RunEvent, error) {
	head, hash := es.lastEventID, es.lastHash
	defer func() { es.lastEventID, es.lastHash = head, hash }()
	prepared := make([]RunEvent, 0, len(requested))
	keys := make(map[eventIdempotencyIdentity]bool)
	for _, input := range requested {
		event, err := es.prepareEvent(input)
		if err != nil {
			return nil, err
		}
		key := newEventIdempotencyIdentity(event.BranchID, event.IdempotencyKey)
		if len(requested) > 1 && (event.IdempotencyKey == "" || keys[key]) {
			return nil, ErrEventBatchConflict
		}
		keys[key] = true
		prepared = append(prepared, event)
		es.lastEventID, es.lastHash = event.ID, event.Hash
	}
	return prepared, nil
}

func (es *EventStore) replayedEventBatch(ctx context.Context, q runtimestore.Queryer, requested []RunEvent) ([]RunEvent, bool, error) {
	var prior []RunEvent
	for _, event := range requested {
		branch := event.BranchID
		if branch == "" {
			branch = es.branchID
		}
		identity := newEventIdempotencyIdentity(branch, event.IdempotencyKey)
		durable, exists := es.idempotencyKeys[identity]
		if event.IdempotencyKey == "" || !exists {
			continue
		}
		if err := es.validateHistoricalGrantReplay(ctx, q, durable, event); err != nil {
			return nil, false, err
		}
		if event.Type == string(EventDerivedForkCreated) || durable.Type == string(EventDerivedForkCreated) {
			candidate, err := es.prepareEvent(event)
			if err != nil {
				return nil, false, err
			}
			if !equivalentDerivedForkEvents(candidate, durable) {
				return nil, false, ErrDerivedForkIntent
			}
			if err := validateDerivedForkStoredBranch(ctx, q, durable); err != nil {
				return nil, false, err
			}
		}
		if isDecisionCorrectnessEvent(event.Type) || isDecisionCorrectnessEvent(durable.Type) {
			equivalent, err := decisionIdempotencyEquivalent(durable, event)
			if err != nil {
				return nil, false, fmt.Errorf("compare decision idempotency payload: %w", err)
			}
			if !equivalent {
				return nil, false, fmt.Errorf("%w: branch %q key %q", ErrDecisionIdempotencyConflict, identity.branchID, identity.key)
			}
		}
		if len(requested) > 1 {
			candidate, err := es.prepareEvent(event)
			if err != nil {
				return nil, false, err
			}
			if !sameBatchEventContent(candidate, durable) || (len(prior) > 0 && durable.PreviousID != prior[len(prior)-1].ID) {
				return nil, false, ErrEventBatchConflict
			}
		}
		prior = append(prior, cloneRunEvent(durable))
	}
	if len(prior) != 0 && len(prior) != len(requested) {
		return nil, false, ErrEventBatchConflict
	}
	return prior, len(prior) != 0, nil
}

func sameBatchEventContent(a, b RunEvent) bool {
	return a.SchemaVersion == b.SchemaVersion && a.Type == b.Type && a.Actor == b.Actor &&
		a.RunID == b.RunID && a.SessionID == b.SessionID && a.BranchID == b.BranchID &&
		a.TaskID == b.TaskID && a.Attempt == b.Attempt && a.IdempotencyKey == b.IdempotencyKey && bytes.Equal(a.Payload, b.Payload)
}
