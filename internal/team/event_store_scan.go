package team

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"maps"
	"os"

	"github.com/kjelly/hufu/internal/eventchain"
)

type eventStoreState struct {
	lastEventID     string
	lastHash        string
	sequence        int
	runID           string
	sessionID       string
	events          []RunEvent
	idempotencyKeys map[eventIdempotencyIdentity]RunEvent
	// size is the byte length of the log prefix this state was derived from.
	size int64
}

// scanFile is the one strict durable scanner. It never mutates EventStore;
// callers publish its complete state only after the entire file validates.
func (es *EventStore) scanFile(f *os.File) (eventStoreState, error) {
	es.scanCount++
	state := eventStoreState{
		runID:           es.runID,
		sessionID:       es.sessionID,
		idempotencyKeys: make(map[eventIdempotencyIdentity]RunEvent),
	}
	if f == nil {
		return eventStoreState{}, fmt.Errorf("event store file is unavailable")
	}
	var verifier eventchain.Verifier
	if err := scanEventLog(f, &state, &verifier); err != nil {
		return eventStoreState{}, err
	}
	return state, nil
}

// appendedState returns the published state extended with the events other
// writers appended after the validated prefix, and whether there were any.
// The log only grows while it is open, so the prefix needs no second scan;
// a log shorter than the prefix was rewritten and is scanned in full. Like
// scanFile it never mutates EventStore, so a rejected tail publishes nothing.
func (es *EventStore) appendedState(f *os.File) (eventStoreState, bool, error) {
	if f == nil {
		return eventStoreState{}, false, fmt.Errorf("event store file is unavailable")
	}
	info, err := f.Stat()
	if err != nil {
		return eventStoreState{}, false, fmt.Errorf("stat event store: %w", err)
	}
	switch {
	case info.Size() == es.validatedSize:
		return eventStoreState{}, false, nil
	case info.Size() < es.validatedSize:
		state, err := es.scanFile(f)
		return state, err == nil, err
	}
	state := eventStoreState{
		lastEventID: es.lastEventID,
		lastHash:    es.lastHash,
		sequence:    es.sequence,
		runID:       es.runID,
		sessionID:   es.sessionID,
		// The full slice expression makes the first append copy, leaving the
		// published cache untouched until the tail validates.
		events:          es.cachedEvents[:len(es.cachedEvents):len(es.cachedEvents)],
		idempotencyKeys: maps.Clone(es.idempotencyKeys),
		size:            es.validatedSize,
	}
	if state.idempotencyKeys == nil {
		state.idempotencyKeys = make(map[eventIdempotencyIdentity]RunEvent)
	}
	verifier := eventchain.ResumeVerifier(state.sequence, state.lastEventID, state.lastHash)
	if err := scanEventLog(f, &state, &verifier); err != nil {
		return eventStoreState{}, false, err
	}
	return state, true, nil
}

// publishAppendedState publishes appendedState when other writers appended.
func (es *EventStore) publishAppendedState(f *os.File) error {
	state, appended, err := es.appendedState(f)
	if err != nil {
		return err
	}
	if appended {
		es.publishState(state, f, false)
	}
	return nil
}

// scanEventLog strictly validates the log from state.size to its current end,
// extending state and the chain verifier with every event it reads.
func scanEventLog(f *os.File, state *eventStoreState, chainVerifier *eventchain.Verifier) error {
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat event store: %w", err)
	}
	end := info.Size()
	sc := bufio.NewScanner(io.NewSectionReader(f, state.size, end-state.size))
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var event RunEvent
		if err := json.Unmarshal(line, &event); err != nil {
			return fmt.Errorf("decode event %d: %w", state.sequence+1, err)
		}
		if err := chainVerifier.Verify(eventchain.Entry{
			ID: event.ID, PreviousID: event.PreviousID, Type: event.Type, Timestamp: event.Timestamp,
			Payload: event.Payload, PreviousHash: event.PreviousHash, Hash: event.Hash,
		}); err != nil {
			return err
		}
		state.events = append(state.events, cloneRunEvent(event))
		state.lastEventID = event.ID
		state.lastHash = event.Hash
		if event.IdempotencyKey != "" {
			identity := newEventIdempotencyIdentity(event.BranchID, event.IdempotencyKey)
			if existing, exists := state.idempotencyKeys[identity]; exists && (isDecisionCorrectnessEvent(event.Type) || isDecisionCorrectnessEvent(existing.Type)) {
				equivalent, compareErr := decisionIdempotencyEquivalent(existing, event)
				if compareErr != nil {
					return fmt.Errorf("compare event %d decision idempotency payload: %w", state.sequence, compareErr)
				}
				if !equivalent {
					return fmt.Errorf("%w: branch %q key %q", ErrDecisionIdempotencyConflict, identity.branchID, identity.key)
				}
			}
			state.idempotencyKeys[identity] = cloneRunEvent(event)
		}
		state.sequence++
		if state.runID == "" && event.RunID != "" {
			state.runID = event.RunID
		}
		if state.sessionID == "" && event.SessionID != "" {
			state.sessionID = event.SessionID
		}
	}
	if err := sc.Err(); err != nil {
		return fmt.Errorf("scan event store: %w", err)
	}
	state.size = end
	return nil
}
