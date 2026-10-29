package team

import (
	"bytes"
	"cmp"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/kjelly/hufu/internal/eventchain"
	"github.com/kjelly/hufu/internal/runtimestore"
)

// ErrEventStoreWriterUnavailable indicates that another connection held the
// runtime store's write lock past the busy timeout.
var ErrEventStoreWriterUnavailable = errors.New("event store writer unavailable")

const (
	// eventStoreSchemaVersion is the current writer schema. Schema v1 remains
	// replayable, but all event-first production boundaries now write v2.
	eventStoreSchemaVersion       = 2
	eventStoreLegacySchemaVersion = 1
)

// RunEvent defines a single append-only structured event in the session timeline.
type RunEvent struct {
	SchemaVersion  int             `json:"schema_version"`
	ID             string          `json:"id"`
	PreviousID     string          `json:"previous_id,omitempty"`
	InvocationID   string          `json:"invocation_id,omitempty"`
	RunID          string          `json:"run_id"`
	SessionID      string          `json:"session_id"`
	BranchID       string          `json:"branch_id,omitempty"`
	TaskID         string          `json:"task_id,omitempty"`
	Attempt        int             `json:"attempt,omitempty"`
	Actor          string          `json:"actor"`
	Type           string          `json:"type"`
	Timestamp      string          `json:"timestamp"`
	IdempotencyKey string          `json:"idempotency_key,omitempty"`
	Payload        json.RawMessage `json:"payload,omitempty"`
	PreviousHash   string          `json:"previous_hash,omitempty"`
	Hash           string          `json:"hash,omitempty"`
}

type eventIdempotencyIdentity struct {
	branchID string
	key      string
}

func newEventIdempotencyIdentity(branchID, key string) eventIdempotencyIdentity {
	if branchID == "" {
		branchID = "main"
	}
	return eventIdempotencyIdentity{branchID: branchID, key: key}
}

// EventStore is the hash-chained event journal kept in the workspace's
// runtime.sqlite events table.
//
// Integrity model: every writer appends inside one IMMEDIATE transaction that
// first validates the rows other writers committed after this store's
// validated head, so the idempotency index and chain head include every
// committed event before a new one is chained. Opening a store and
// VerifyHashChain validate the whole journal. Appends only insert; triggers
// count every update or delete of a stored event, and an append revalidates
// everything when that count moved or the head row no longer matches the
// validated head event and its hash, so it never extends a damaged chain. The
// chain is self-contained: even a full validation cannot detect a rewrite
// that recomputes every hash and bypasses the triggers, which would need an
// anchor kept outside the database.
type EventStore struct {
	// mu is a one-token semaphore so emergency callers can cancel lock
	// acquisition. Once an append holds the database connection it runs to
	// completion.
	mu                chan struct{}
	store             *runtimestore.Store
	path              string
	runID             string
	invocationID      string
	sessionID         string
	branchID          string
	lastEventID       string
	lastHash          string
	sequence          int
	degraded          bool
	stateValid        bool
	stateErr          error
	closed            bool
	readOnly          bool
	scanCount         int
	cacheHitCount     int
	cachedEvents      []RunEvent
	occurrenceIndex   map[eventOccurrenceKey][]int
	occurrenceIndexed int
	idempotencyKeys   map[eventIdempotencyIdentity]RunEvent
	// rewrites is the journal's rewrite counter when it was validated.
	rewrites int64
	// commitHook runs after an append commits. Its failure leaves the
	// append's durability unknown, as an fsync failure did for the file log;
	// tests inject one to exercise recovery. A reload restores the no-op.
	commitHook func() error
}

func noCommitHook() error { return nil }

// SetBranchID binds the store to a session branch: subsequent events appended
// without an explicit BranchID are stamped with it. Empty means no stamping
// (events then fall back to the implicit main lineage).
func (es *EventStore) SetBranchID(branchID string) {
	es.lock()
	defer es.release()
	es.branchID = branchID
}

// BranchID returns the branch that unstamped appends are bound to.
func (es *EventStore) BranchID() string {
	es.lock()
	defer es.release()
	return es.branchID
}

// SetInvocationID binds subsequent events to their top-level CLI invocation.
// Historical writers that do not set it remain valid and omit the field.
func (es *EventStore) SetInvocationID(invocationID string) {
	es.lock()
	defer es.release()
	es.invocationID = strings.TrimSpace(invocationID)
}

// ComputeEventHash computes a SHA-256 hash over an event's prevHash, ID, type, timestamp, and payload.
func ComputeEventHash(prevHash, id, eventType, timestamp string, payload json.RawMessage) string {
	return eventchain.ComputeHash(prevHash, id, eventType, timestamp, payload)
}

// NewEventStore opens workspace's event journal, creating runtime.sqlite when
// it does not exist.
func NewEventStore(workspace, runID, sessionID string) (*EventStore, error) {
	if workspace == "" {
		return nil, fmt.Errorf("new event store: empty workspace")
	}
	store, err := runtimestore.Open(context.Background(), workspace)
	if err != nil {
		return nil, fmt.Errorf("new event store: %w", err)
	}
	es := &EventStore{
		mu:              make(chan struct{}, 1),
		store:           store,
		path:            store.Path(),
		runID:           runID,
		sessionID:       sessionID,
		idempotencyKeys: make(map[eventIdempotencyIdentity]RunEvent),
	}
	es.mu <- struct{}{}
	if err := es.reload(); err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("new event store load: %w", err)
	}
	return es, nil
}

// OpenEventStore opens an existing EventStore without modifying runID or sessionID defaults.
func OpenEventStore(workspace string) (*EventStore, error) {
	return NewEventStore(workspace, "", "")
}

// borrowEventStore returns open when it is workspace's live, valid event
// journal, and otherwise opens the journal for a caller that must call
// release. Opening validates the whole journal, which costs seconds on a
// long-lived workspace, so a caller that already holds the store should lend
// it.
func borrowEventStore(workspace string, open *EventStore) (store *EventStore, release func(), err error) {
	if open.servesWorkspace(workspace) {
		return open, func() {}, nil
	}
	store, err = OpenEventStore(workspace)
	if err != nil {
		return nil, nil, err
	}
	return store, func() { _ = store.Close() }, nil
}

// servesWorkspace reports whether es is workspace's event journal and can
// still answer reads from validated state.
func (es *EventStore) servesWorkspace(workspace string) bool {
	if es == nil || es.mu == nil || strings.TrimSpace(workspace) == "" {
		return false
	}
	path, err := filepath.Abs(runtimestore.Path(workspace))
	if err != nil {
		return false
	}
	es.lock()
	defer es.release()
	return !es.closed && es.stateValid && es.path == path
}

// OpenEventStoreReadOnly opens and validates an existing event journal
// without creating or changing any file. A workspace without runtime.sqlite
// returns an error wrapping os.ErrNotExist. Mutation methods on the returned
// store fail closed.
func OpenEventStoreReadOnly(workspace string) (*EventStore, error) {
	if strings.TrimSpace(workspace) == "" {
		return nil, fmt.Errorf("open read-only event store: empty workspace")
	}
	store, err := runtimestore.OpenReadOnly(context.Background(), workspace)
	if err != nil {
		return nil, fmt.Errorf("open read-only event store: %w", err)
	}
	es := &EventStore{
		mu:              make(chan struct{}, 1),
		store:           store,
		path:            store.Path(),
		readOnly:        true,
		idempotencyKeys: make(map[eventIdempotencyIdentity]RunEvent),
	}
	es.mu <- struct{}{}
	if err := es.reload(); err != nil {
		_ = store.Close()
		return nil, fmt.Errorf("open read-only event store: %w", err)
	}
	return es, nil
}

func generateEventID() string {
	buf := make([]byte, 8)
	_, _ = rand.Read(buf)
	return fmt.Sprintf("evt-%s-%s", time.Now().UTC().Format("20060102150405"), hex.EncodeToString(buf))
}

// IsTerminalEvent returns true if the event type represents a terminal run or task outcome.
func IsTerminalEvent(eventType string) bool {
	switch eventType {
	case "run_finished", "task_completed", "task_failed", "task_blocked", "task_skipped", "task_protocol_incomplete", "task_cancelled":
		return true
	default:
		return false
	}
}

// IsEmptyPayload checks if a JSON raw payload is nil, 0 bytes, whitespace, or a vacuous empty container ({}, [], null).
func IsEmptyPayload(payload json.RawMessage) bool {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 {
		return true
	}
	s := string(trimmed)
	if s == "null" || s == "{}" || s == "[]" {
		return true
	}
	var val interface{}
	if err := json.Unmarshal(trimmed, &val); err != nil {
		return true
	}
	if val == nil {
		return true
	}
	switch v := val.(type) {
	case map[string]interface{}:
		return len(v) == 0
	case []interface{}:
		return len(v) == 0
	default:
		return false
	}
}

// Append appends a new RunEvent to the log with hash chaining. It preserves
// the original API for callers that do not need the assigned durable identity.
func (es *EventStore) Append(event RunEvent) error {
	// Preserve the pre-EventJournal public API as a compatibility writer. New
	// runtime paths use AppendPersisted (via EventJournal) and therefore get
	// the stricter current schema by default.
	if event.SchemaVersion == 0 {
		event.SchemaVersion = eventStoreLegacySchemaVersion
	}
	_, err := es.AppendPersisted(event)
	return err
}

// AppendPersisted appends an event and returns the exact redacted, stamped,
// hash-chained record that reached the durability boundary. It is the commit
// primitive used by EventJournal and event-first projections.
func (es *EventStore) AppendPersisted(event RunEvent) (RunEvent, error) {
	return es.AppendPersistedContext(context.Background(), event)
}

// AppendPersistedBoundedContext bounds the caller's wait even when the event
// store has already admitted an append whose transaction cannot be cancelled.
// The append remains owned by EventStore until the commit returns; a caller
// timeout is durability-unknown, never success. Close acquires the same
// semaphore and therefore waits for an owned append before closing the
// database rather than leaking an append worker against a closed handle.
func (es *EventStore) AppendPersistedBoundedContext(ctx context.Context, event RunEvent) (RunEvent, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return RunEvent{}, err
	}
	if ctx.Done() == nil {
		return es.AppendPersistedContext(ctx, event)
	}
	type appendResult struct {
		event RunEvent
		err   error
	}
	resultCh := make(chan appendResult, 1)
	var worker sync.WaitGroup
	worker.Add(1)
	go func() {
		defer worker.Done()
		persisted, err := es.AppendPersistedContext(context.Background(), event)
		resultCh <- appendResult{event: persisted, err: err}
	}()
	select {
	case result := <-resultCh:
		worker.Wait()
		return result.event, result.err
	case <-ctx.Done():
		// The worker is deliberately not detached from EventStore ownership:
		// its result channel is buffered and Close waits on the store semaphore.
		// The caller must enter recovery and must not publish projections.
		return RunEvent{}, fmt.Errorf("event durability unknown: %w", ctx.Err())
	}
}

// AppendPersistedContext is the cancellable append boundary. Context
// cancellation applies to admission and to waiting for the database
// connection. Once the append holds the connection it runs to completion;
// callers must treat a deadline that expires in that window as
// durability-unknown and reconcile by idempotency key on restart.
func (es *EventStore) AppendPersistedContext(ctx context.Context, event RunEvent) (RunEvent, error) {
	return es.appendPersistedContext(ctx, event, nil)
}

func (es *EventStore) appendPersistedContext(ctx context.Context, event RunEvent, expectedHead *string) (RunEvent, error) {
	events, err := es.appendPersistedEvents(ctx, []RunEvent{event}, expectedHead)
	if err != nil {
		return RunEvent{}, err
	}
	return events[0], nil
}

// prepareEvent redacts, stamps, validates, and chains event onto the
// validated head.
func (es *EventStore) prepareEvent(event RunEvent) (RunEvent, error) {
	if event.Type == string(EventDerivedForkCreated) {
		if _, err := decodeDerivedForkCreatedPayload(event.Payload); err != nil {
			return RunEvent{}, err
		}
	}
	if event.Type == string(EventHistoricalResultsGranted) {
		// The legacy redactor decodes into maps. Reject duplicate members and
		// unknown fields before that transformation can erase their ambiguity.
		if _, err := decodeHistoricalResultGrantPayload(event.Payload); err != nil {
			return RunEvent{}, err
		}
	}
	if IsTerminalEvent(event.Type) && IsEmptyPayload(event.Payload) {
		return RunEvent{}, fmt.Errorf("reject terminal event %q with empty payload", event.Type)
	}
	if len(bytes.TrimSpace(event.Payload)) > 0 {
		redacted, err := redactJSONPreservingRuntimeOutputs(event.Payload)
		if err != nil {
			return RunEvent{}, fmt.Errorf("redact event payload: %w", err)
		}
		// Exports marshal RawMessage values compacted. Compact here too so the
		// hash is computed over exactly the bytes every reader sees.
		var compact bytes.Buffer
		if err := json.Compact(&compact, redacted); err != nil {
			return RunEvent{}, fmt.Errorf("compact redacted event payload: %w", err)
		}
		event.Payload = compact.Bytes()
	}

	if event.SchemaVersion == 0 {
		event.SchemaVersion = eventStoreSchemaVersion
	}
	if event.ID == "" {
		event.ID = generateEventID()
	}
	if event.RunID == "" && event.Type != string(EventDerivedForkCreated) {
		if es.runID == "" {
			// A recovered empty store has no inherited identity. Give the first
			// current-schema event a durable recovery run identity instead of
			// silently weakening schema validation.
			es.runID = fmt.Sprintf("run-recovery-%d", time.Now().UTC().UnixNano())
		}
		event.RunID = es.runID
	}
	event.InvocationID = cmp.Or(event.InvocationID, es.invocationID)
	if event.SessionID == "" {
		if es.sessionID == "" {
			es.sessionID = filepath.Base(filepath.Dir(es.path))
		}
		event.SessionID = es.sessionID
	}
	if event.BranchID == "" {
		event.BranchID = es.branchID
	}
	if event.Timestamp == "" {
		event.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	}
	if err := ValidateEventPayload(event); err != nil {
		return RunEvent{}, fmt.Errorf("validate run event: %w", err)
	}

	event.PreviousID = es.lastEventID
	event.PreviousHash = es.lastHash
	event.Hash = ComputeEventHash(event.PreviousHash, event.ID, event.Type, event.Timestamp, event.Payload)
	return event, nil
}

func (es *EventStore) acquire(ctx context.Context) error {
	if es == nil || es.mu == nil {
		return fmt.Errorf("event store lock is unavailable")
	}
	select {
	case <-es.mu:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (es *EventStore) release() {
	if es != nil && es.mu != nil {
		es.mu <- struct{}{}
	}
}

// reload strictly validates the whole journal and publishes it. It refuses
// a broken chain instead of appending from a guessed last hash.
func (es *EventStore) reload() error {
	if es.closed {
		return fmt.Errorf("event store closed")
	}
	if err := es.reopenIfReplaced(); err != nil {
		es.invalidateState(err)
		return err
	}
	es.scanCount++
	state, err := loadFullState(context.Background(), es.store.DB(), es.runID, es.sessionID)
	if err != nil {
		es.invalidateState(err)
		return err
	}
	es.publishState(state)
	return nil
}

// reopenIfReplaced swaps a writable store whose file was removed or replaced
// for one on the file the path names now.
func (es *EventStore) reopenIfReplaced() error {
	if es.store == nil {
		return fmt.Errorf("event store database is unavailable")
	}
	if es.readOnly || es.store.CheckCurrent() == nil {
		return nil
	}
	// Close first: SQLite removes the stale connection's sidecars by name.
	closeErr := es.store.Close()
	es.store = nil
	store, err := runtimestore.Open(context.Background(), filepath.Dir(es.path))
	if err != nil {
		return errors.Join(fmt.Errorf("reopen replaced event store: %w", err), closeErr)
	}
	es.store = store
	return nil
}

// syncCommittedEvents publishes the events other writers committed after the
// validated head. The tail is loaded into private state and merged only once
// it validates, so a rejected tail publishes nothing. A rewrite of the
// validated prefix — seen through the rewrite counter, or as a head row that
// no longer matches what was validated — revalidates everything, so a
// damaged chain is refused rather than extended.
func (es *EventStore) syncCommittedEvents(ctx context.Context, q runtimestore.Queryer) error {
	rewritten, err := es.prefixRewritten(ctx, q)
	if err != nil {
		return err
	}
	if rewritten {
		es.scanCount++
		state, err := loadFullState(ctx, q, es.runID, es.sessionID)
		if err != nil {
			return err
		}
		hook := es.commitHook
		es.publishState(state)
		es.commitHook = hook
		return nil
	}
	tail := eventStoreState{
		lastEventID:     es.lastEventID,
		lastHash:        es.lastHash,
		sequence:        es.sequence,
		runID:           es.runID,
		sessionID:       es.sessionID,
		idempotencyKeys: make(map[eventIdempotencyIdentity]RunEvent),
		publishedKeys:   es.idempotencyKeys,
	}
	verifier := eventchain.ResumeVerifier(tail.sequence, tail.lastEventID, tail.lastHash)
	if err := loadEvents(ctx, q, &tail, &verifier); err != nil {
		return err
	}
	es.cachedEvents = append(es.cachedEvents, tail.events...)
	if es.idempotencyKeys == nil {
		es.idempotencyKeys = make(map[eventIdempotencyIdentity]RunEvent, len(tail.idempotencyKeys))
	}
	maps.Copy(es.idempotencyKeys, tail.idempotencyKeys)
	es.lastEventID = tail.lastEventID
	es.lastHash = tail.lastHash
	es.sequence = tail.sequence
	es.runID = tail.runID
	es.sessionID = tail.sessionID
	return nil
}

// prefixRewritten reports whether the journal changed other than by appends
// since it was validated: the rewrite counter moved, or the head row differs
// from the validated head event or no longer matches its own hash.
func (es *EventStore) prefixRewritten(ctx context.Context, q runtimestore.Queryer) (bool, error) {
	rewrites, err := readJournalRewrites(ctx, q)
	if err != nil {
		return false, err
	}
	if rewrites != es.rewrites {
		return true, nil
	}
	if es.sequence == 0 {
		return false, nil
	}
	rows, err := q.QueryContext(ctx, `SELECT `+eventColumns+` FROM events WHERE sequence = ?`, es.sequence)
	if err != nil {
		return false, fmt.Errorf("read validated head: %w", err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		return true, rows.Err()
	}
	_, head, err := scanEventRow(rows)
	if err != nil {
		return false, fmt.Errorf("decode validated head: %w", err)
	}
	validated := es.cachedEvents[len(es.cachedEvents)-1]
	if !sameStoredEvent(head, validated) ||
		head.Hash != ComputeEventHash(head.PreviousHash, head.ID, head.Type, head.Timestamp, head.Payload) {
		return true, nil
	}
	return false, rows.Err()
}

// sameStoredEvent compares two events field by field; an empty payload
// equals a nil one, as storage cannot tell them apart.
func sameStoredEvent(a, b RunEvent) bool {
	if !bytes.Equal(a.Payload, b.Payload) {
		return false
	}
	a.Payload, b.Payload = nil, nil
	return reflect.DeepEqual(a, b)
}

func (es *EventStore) publishState(state eventStoreState) {
	es.lastEventID = state.lastEventID
	es.lastHash = state.lastHash
	es.sequence = state.sequence
	es.runID = state.runID
	es.sessionID = state.sessionID
	es.cachedEvents = state.events
	es.occurrenceIndex = nil
	es.occurrenceIndexed = 0
	es.idempotencyKeys = state.idempotencyKeys
	es.rewrites = state.rewrites
	es.commitHook = noCommitHook
	es.stateErr = nil
	es.stateValid = true
	es.degraded = false
}

// invalidateState removes every derived value that could permit a caller to
// continue from unvalidated rows. The configured run/session identity is
// retained as an append default, while chain/cache/idempotency state is not.
func (es *EventStore) invalidateState(err error) {
	es.commitHook = nil
	es.lastEventID = ""
	es.lastHash = ""
	es.sequence = 0
	es.cachedEvents = nil
	es.occurrenceIndex = nil
	es.occurrenceIndexed = 0
	es.idempotencyKeys = nil
	es.stateErr = err
	es.stateValid = false
	es.degraded = true
}

// ReadEvents returns a copy of the strictly validated event cache populated by
// rescan and maintained after each durable append. Callers may safely mutate
// returned payloads without changing the store's canonical records.
func (es *EventStore) ReadEvents() ([]RunEvent, error) {
	es.lock()
	defer es.release()
	if es.path == "" {
		return nil, nil
	}
	if !es.stateValid {
		if es.stateErr != nil {
			return nil, fmt.Errorf("event store state invalid: %w", es.stateErr)
		}
		return nil, fmt.Errorf("event store state invalid")
	}
	es.cacheHitCount++
	return cloneRunEvents(es.cachedEvents), nil
}

func cloneRunEvent(event RunEvent) RunEvent {
	clone := event
	if event.Payload != nil {
		clone.Payload = make(json.RawMessage, len(event.Payload))
		copy(clone.Payload, event.Payload)
	}
	return clone
}

func cloneRunEvents(events []RunEvent) []RunEvent {
	if events == nil {
		return nil
	}
	clones := make([]RunEvent, len(events))
	for i, event := range events {
		clones[i] = cloneRunEvent(event)
	}
	return clones
}

// VerifyHashChain revalidates the whole journal as an unbroken hash chain.
func (es *EventStore) VerifyHashChain() error {
	es.lock()
	defer es.release()
	if es.closed {
		return fmt.Errorf("event store closed")
	}
	if err := es.reload(); err != nil {
		return fmt.Errorf("refresh event store for verification: %w", err)
	}
	return nil
}

// Close releases the database. Validated state stays readable.
func (es *EventStore) Close() error {
	es.lock()
	defer es.release()
	es.closed = true
	if es.store == nil {
		return nil
	}
	err := es.store.Close()
	es.store = nil
	if err != nil {
		return fmt.Errorf("close event store: %w", err)
	}
	return nil
}

func (es *EventStore) lock() {
	if es == nil || es.mu == nil {
		return
	}
	<-es.mu
}
