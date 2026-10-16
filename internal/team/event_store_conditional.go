package team

import (
	"context"
	"errors"
)

// ErrEventHeadChanged means the caller's read was superseded. No event was
// appended; the caller must reread the canonical journal before recomputing.
var ErrEventHeadChanged = errors.New("event journal head changed")

// ConditionalEventJournal supports a transactional read/append fence without
// changing the ordinary EventJournal contract or bypassing injected adapters.
type ConditionalEventJournal interface {
	EventJournal
	AppendAtHead(context.Context, RunEvent, string) (RunEvent, error)
}

func (j eventStoreJournal) AppendAtHead(ctx context.Context, event RunEvent, headID string) (RunEvent, error) {
	if j.store == nil {
		return RunEvent{}, errors.New("event journal is unavailable")
	}
	return j.store.AppendPersistedAtHead(ctx, event, headID)
}

// AppendPersistedAtHead checks the global chain head inside the existing
// SQLite write transaction, after synchronizing other writers. This is also
// safe across separate EventStore handles and OS processes.
func (es *EventStore) AppendPersistedAtHead(ctx context.Context, event RunEvent, headID string) (RunEvent, error) {
	return es.appendPersistedContext(ctx, event, &headID)
}
