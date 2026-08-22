package team

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/kjelly/hufu/internal/utils"
)

const eventStoreFile = "event_store.jsonl"

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

// EventStore manages durable append-only event logging with hash chain verification.
type EventStore struct {
	mu              sync.Mutex
	f               *os.File
	path            string
	runID           string
	sessionID       string
	branchID        string
	lastEventID     string
	lastHash        string
	sequence        int
	degraded        bool
	stateValid      bool
	stateErr        error
	closed          bool
	scanCount       int
	cacheHitCount   int
	cachedEvents    []RunEvent
	syncFile        func() error
	idempotencyKeys map[string]RunEvent
}

// SetBranchID binds the store to a session branch: subsequent events appended
// without an explicit BranchID are stamped with it. Empty means no stamping
// (events then fall back to the implicit main lineage).
func (es *EventStore) SetBranchID(branchID string) {
	es.mu.Lock()
	defer es.mu.Unlock()
	es.branchID = branchID
}

// ComputeEventHash computes a SHA-256 hash over an event's prevHash, ID, type, timestamp, and payload.
func ComputeEventHash(prevHash, id, eventType, timestamp string, payload json.RawMessage) string {
	h := sha256.New()
	_, _ = h.Write([]byte(prevHash))
	_, _ = h.Write([]byte(id))
	_, _ = h.Write([]byte(eventType))
	_, _ = h.Write([]byte(timestamp))
	_, _ = h.Write(payload)
	return hex.EncodeToString(h.Sum(nil))
}

// NewEventStore opens or creates the event_store.jsonl file in workspace/logs.
func NewEventStore(workspace, runID, sessionID string) (*EventStore, error) {
	if workspace == "" {
		return nil, fmt.Errorf("new event store: empty workspace")
	}
	path := filepath.Join(workspace, logsDir, eventStoreFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("new event store mkdir: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("new event store open: %w", err)
	}

	es := &EventStore{
		f:               f,
		path:            path,
		runID:           runID,
		sessionID:       sessionID,
		syncFile:        f.Sync,
		idempotencyKeys: make(map[string]RunEvent),
	}

	if err := es.rescan(); err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("new event store rescan: %w", err)
	}

	return es, nil
}

// OpenEventStore opens an existing EventStore without modifying runID or sessionID defaults.
func OpenEventStore(workspace string) (*EventStore, error) {
	return NewEventStore(workspace, "", "")
}

func generateEventID() string {
	buf := make([]byte, 8)
	_, _ = rand.Read(buf)
	return fmt.Sprintf("evt-%s-%s", time.Now().UTC().Format("20060102150405"), hex.EncodeToString(buf))
}

// IsTerminalEvent returns true if the event type represents a terminal run or task outcome.
func IsTerminalEvent(eventType string) bool {
	switch eventType {
	case "run_finished", "task_completed", "task_failed", "task_blocked", "task_skipped", "task_protocol_incomplete":
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
	es.mu.Lock()
	defer es.mu.Unlock()

	if es.closed {
		return RunEvent{}, fmt.Errorf("event store closed")
	}
	if es.f == nil || es.degraded || !es.stateValid {
		if err := es.reopenAndRescan(); err != nil {
			return RunEvent{}, fmt.Errorf("recover degraded event store: %w", err)
		}
	}
	// A failed Sync leaves durability uncertain: the event may nevertheless be
	// visible after reopen. Treat a persisted idempotency key as success so a
	// retry cannot fork the logical event stream with a duplicate observation.
	if event.IdempotencyKey != "" {
		if durable, exists := es.idempotencyKeys[event.IdempotencyKey]; exists {
			// The event was already acknowledged as durable. Callers may safely
			// apply their idempotent projection using its original identity; no
			// second transition is written.
			return cloneRunEvent(durable), nil
		}
	}

	if IsTerminalEvent(event.Type) && IsEmptyPayload(event.Payload) {
		return RunEvent{}, fmt.Errorf("reject terminal event %q with empty payload", event.Type)
	}
	if len(bytes.TrimSpace(event.Payload)) > 0 {
		redacted, err := utils.RedactJSON(event.Payload)
		if err != nil {
			return RunEvent{}, fmt.Errorf("redact event payload: %w", err)
		}
		// The outer event marshal compacts RawMessage values. Compact here too
		// so the hash is computed over exactly the bytes persisted on disk.
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
	if event.RunID == "" {
		if es.runID == "" {
			// A recovered empty store has no inherited identity. Give the first
			// current-schema event a durable recovery run identity instead of
			// silently weakening schema validation.
			es.runID = fmt.Sprintf("run-recovery-%d", time.Now().UTC().UnixNano())
		}
		event.RunID = es.runID
	}
	if event.SessionID == "" {
		if es.sessionID == "" {
			es.sessionID = filepath.Base(filepath.Dir(filepath.Dir(es.path)))
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

	data, err := json.Marshal(event)
	if err != nil {
		return RunEvent{}, fmt.Errorf("marshal run event: %w", err)
	}

	line := append(data, '\n')
	n, err := es.f.Write(line)
	if err != nil {
		es.invalidateState(fmt.Errorf("write run event: %w", err))
		return RunEvent{}, fmt.Errorf("write run event: %w", err)
	}
	if n != len(line) {
		es.invalidateState(io.ErrShortWrite)
		return RunEvent{}, fmt.Errorf("write run event: %w", io.ErrShortWrite)
	}

	if err := es.syncFile(); err != nil {
		es.invalidateState(fmt.Errorf("sync run event: %w", err))
		return RunEvent{}, fmt.Errorf("sync run event: %w", err)
	}

	es.sequence++
	es.lastEventID = event.ID
	es.lastHash = event.Hash
	if event.IdempotencyKey != "" {
		es.idempotencyKeys[event.IdempotencyKey] = cloneRunEvent(event)
	}
	es.cachedEvents = append(es.cachedEvents, cloneRunEvent(event))
	return event, nil
}

// reopenAndRescan closes the file whose durable state is uncertain, reopens
// it, and derives the chain head from bytes that are actually visible in the
// event log. It deliberately refuses a malformed or broken chain instead of
// appending from a guessed last hash.
func (es *EventStore) reopenAndRescan() error {
	if es.closed {
		return fmt.Errorf("event store closed")
	}
	f, err := os.OpenFile(es.path, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		es.invalidateState(err)
		return err
	}
	state, err := es.scanFile(f)
	if err != nil {
		_ = f.Close()
		es.invalidateState(err)
		return err
	}

	old := es.f
	es.publishState(state, f)
	if old != nil && old != f {
		_ = old.Close()
	}
	return nil
}

// rescan strictly validates the persisted JSONL and restores the chain head.
// ReadEvents remains the tolerant inspection API for compatibility; append
// recovery must be strict because silently skipping a line would fork the
// hash chain.
func (es *EventStore) rescan() error {
	state, err := es.scanFile(es.f)
	if err != nil {
		es.invalidateState(err)
		return err
	}
	es.publishState(state, es.f)
	return nil
}

type eventStoreState struct {
	lastEventID     string
	lastHash        string
	sequence        int
	runID           string
	sessionID       string
	events          []RunEvent
	idempotencyKeys map[string]RunEvent
}

// scanFile is the one strict durable scanner. It never mutates EventStore;
// callers publish its complete state only after the entire file validates.
func (es *EventStore) scanFile(f *os.File) (eventStoreState, error) {
	es.scanCount++
	state := eventStoreState{
		runID:           es.runID,
		sessionID:       es.sessionID,
		idempotencyKeys: make(map[string]RunEvent),
	}
	if f == nil {
		return eventStoreState{}, fmt.Errorf("event store file is unavailable")
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return eventStoreState{}, fmt.Errorf("seek event store: %w", err)
	}

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var event RunEvent
		if err := json.Unmarshal(line, &event); err != nil {
			return eventStoreState{}, fmt.Errorf("decode event %d: %w", state.sequence+1, err)
		}
		if state.sequence == 0 {
			if event.PreviousID != "" || event.PreviousHash != "" {
				return eventStoreState{}, fmt.Errorf("first event (%s) must have empty previous_id and previous_hash", event.ID)
			}
		} else if event.PreviousID != state.lastEventID || event.PreviousHash != state.lastHash {
			return eventStoreState{}, fmt.Errorf("event %d (%s) does not continue hash chain", state.sequence, event.ID)
		}
		expected := ComputeEventHash(event.PreviousHash, event.ID, event.Type, event.Timestamp, event.Payload)
		if event.Hash != expected {
			return eventStoreState{}, fmt.Errorf("event %d (%s) hash invalid", state.sequence, event.ID)
		}
		state.events = append(state.events, cloneRunEvent(event))
		state.lastEventID = event.ID
		state.lastHash = event.Hash
		if event.IdempotencyKey != "" {
			state.idempotencyKeys[event.IdempotencyKey] = cloneRunEvent(event)
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
		return eventStoreState{}, fmt.Errorf("scan event store: %w", err)
	}
	return state, nil
}

func (es *EventStore) publishState(state eventStoreState, f *os.File) {
	es.lastEventID = state.lastEventID
	es.lastHash = state.lastHash
	es.sequence = state.sequence
	es.runID = state.runID
	es.sessionID = state.sessionID
	es.cachedEvents = state.events
	es.idempotencyKeys = state.idempotencyKeys
	es.f = f
	if f != nil {
		es.syncFile = f.Sync
	}
	es.stateErr = nil
	es.stateValid = true
	es.degraded = false
}

// invalidateState removes every derived value that could permit a caller to
// continue from an unvalidated file. The configured run/session identity is
// retained as an append default, while chain/cache/idempotency state is not.
func (es *EventStore) invalidateState(err error) {
	if es.f != nil {
		_ = es.f.Close()
	}
	es.f = nil
	es.syncFile = nil
	es.lastEventID = ""
	es.lastHash = ""
	es.sequence = 0
	es.cachedEvents = nil
	es.idempotencyKeys = nil
	es.stateErr = err
	es.stateValid = false
	es.degraded = true
}

// ReadEvents returns a copy of the strictly validated event cache populated by
// rescan and maintained after each durable append. Callers may safely mutate
// returned payloads without changing the store's canonical records.
func (es *EventStore) ReadEvents() ([]RunEvent, error) {
	es.mu.Lock()
	defer es.mu.Unlock()
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

// VerifyHashChain validates that all events form an unbroken cryptographic hash chain.
func (es *EventStore) VerifyHashChain() error {
	es.mu.Lock()
	defer es.mu.Unlock()
	if es.closed {
		return fmt.Errorf("event store closed")
	}
	if err := es.reopenAndRescan(); err != nil {
		return fmt.Errorf("refresh event store for verification: %w", err)
	}
	return nil
}

// Close closes the underlying log file.
func (es *EventStore) Close() error {
	es.mu.Lock()
	defer es.mu.Unlock()
	if es.f != nil {
		err := es.f.Close()
		es.f = nil
		es.closed = true
		return err
	}
	es.closed = true
	return nil
}
