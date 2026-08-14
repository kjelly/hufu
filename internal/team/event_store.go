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
const eventStoreSchemaVersion = 1

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
	syncFile        func() error
	idempotencyKeys map[string]struct{}
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
		idempotencyKeys: make(map[string]struct{}),
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

// Append appends a new RunEvent to the log with hash chaining.
func (es *EventStore) Append(event RunEvent) error {
	es.mu.Lock()
	defer es.mu.Unlock()

	if es.f == nil {
		return fmt.Errorf("event store closed")
	}
	if es.degraded {
		if err := es.reopenAndRescan(); err != nil {
			return fmt.Errorf("recover degraded event store: %w", err)
		}
	}
	// A failed Sync leaves durability uncertain: the event may nevertheless be
	// visible after reopen. Treat a persisted idempotency key as success so a
	// retry cannot fork the logical event stream with a duplicate observation.
	if event.IdempotencyKey != "" {
		if _, exists := es.idempotencyKeys[event.IdempotencyKey]; exists {
			return nil
		}
	}

	if IsTerminalEvent(event.Type) && IsEmptyPayload(event.Payload) {
		return fmt.Errorf("reject terminal event %q with empty payload", event.Type)
	}
	if len(bytes.TrimSpace(event.Payload)) > 0 {
		redacted, err := utils.RedactJSON(event.Payload)
		if err != nil {
			return fmt.Errorf("redact event payload: %w", err)
		}
		// The outer event marshal compacts RawMessage values. Compact here too
		// so the hash is computed over exactly the bytes persisted on disk.
		var compact bytes.Buffer
		if err := json.Compact(&compact, redacted); err != nil {
			return fmt.Errorf("compact redacted event payload: %w", err)
		}
		event.Payload = compact.Bytes()
	}

	es.sequence++
	if event.SchemaVersion == 0 {
		event.SchemaVersion = eventStoreSchemaVersion
	}
	if event.ID == "" {
		event.ID = generateEventID()
	}
	if event.RunID == "" {
		event.RunID = es.runID
	}
	if event.SessionID == "" {
		event.SessionID = es.sessionID
	}
	if event.BranchID == "" {
		event.BranchID = es.branchID
	}
	if event.Timestamp == "" {
		event.Timestamp = time.Now().UTC().Format(time.RFC3339Nano)
	}

	event.PreviousID = es.lastEventID
	event.PreviousHash = es.lastHash
	event.Hash = ComputeEventHash(event.PreviousHash, event.ID, event.Type, event.Timestamp, event.Payload)

	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("marshal run event: %w", err)
	}

	line := append(data, '\n')
	n, err := es.f.Write(line)
	if err != nil {
		es.degraded = true
		return fmt.Errorf("write run event: %w", err)
	}
	if n != len(line) {
		es.degraded = true
		return fmt.Errorf("write run event: %w", io.ErrShortWrite)
	}

	if err := es.syncFile(); err != nil {
		es.degraded = true
		return fmt.Errorf("sync run event: %w", err)
	}

	es.lastEventID = event.ID
	es.lastHash = event.Hash
	if event.IdempotencyKey != "" {
		es.idempotencyKeys[event.IdempotencyKey] = struct{}{}
	}
	return nil
}

// reopenAndRescan closes the file whose durable state is uncertain, reopens
// it, and derives the chain head from bytes that are actually visible in the
// event log. It deliberately refuses a malformed or broken chain instead of
// appending from a guessed last hash.
func (es *EventStore) reopenAndRescan() error {
	if es.f != nil {
		_ = es.f.Close()
	}
	f, err := os.OpenFile(es.path, os.O_APPEND|os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		es.f = nil
		return err
	}
	es.f = f
	es.syncFile = f.Sync
	if err := es.rescan(); err != nil {
		_ = f.Close()
		es.f = nil
		return err
	}
	es.degraded = false
	return nil
}

// rescan strictly validates the persisted JSONL and restores the chain head.
// ReadEvents remains the tolerant inspection API for compatibility; append
// recovery must be strict because silently skipping a line would fork the
// hash chain.
func (es *EventStore) rescan() error {
	f, err := os.Open(es.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	defer func() { _ = f.Close() }()

	es.lastEventID = ""
	es.lastHash = ""
	es.sequence = 0
	es.idempotencyKeys = make(map[string]struct{})
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var event RunEvent
		if err := json.Unmarshal(line, &event); err != nil {
			return fmt.Errorf("decode event %d: %w", es.sequence+1, err)
		}
		if event.PreviousID != es.lastEventID || event.PreviousHash != es.lastHash {
			return fmt.Errorf("event %d (%s) does not continue hash chain", es.sequence, event.ID)
		}
		expected := ComputeEventHash(event.PreviousHash, event.ID, event.Type, event.Timestamp, event.Payload)
		if event.Hash != expected {
			return fmt.Errorf("event %d (%s) hash invalid", es.sequence, event.ID)
		}
		es.lastEventID = event.ID
		es.lastHash = event.Hash
		if event.IdempotencyKey != "" {
			es.idempotencyKeys[event.IdempotencyKey] = struct{}{}
		}
		es.sequence++
		if es.runID == "" && event.RunID != "" {
			es.runID = event.RunID
		}
		if es.sessionID == "" && event.SessionID != "" {
			es.sessionID = event.SessionID
		}
	}
	return sc.Err()
}

// ReadEvents reads all valid events from the event store file.
func (es *EventStore) ReadEvents() ([]RunEvent, error) {
	es.mu.Lock()
	defer es.mu.Unlock()
	if es.path == "" {
		return nil, nil
	}
	f, err := os.Open(es.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer func() { _ = f.Close() }()

	var events []RunEvent
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var e RunEvent
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		events = append(events, e)
	}
	return events, sc.Err()
}

// VerifyHashChain validates that all events form an unbroken cryptographic hash chain.
func (es *EventStore) VerifyHashChain() error {
	events, err := es.ReadEvents()
	if err != nil {
		return fmt.Errorf("read events for verification: %w", err)
	}
	var prevHash string
	var prevID string
	for i, e := range events {
		if i > 0 {
			if e.PreviousID != prevID {
				return fmt.Errorf("event %d (%s) previous_id mismatch: got %s, want %s", i, e.ID, e.PreviousID, prevID)
			}
			if e.PreviousHash != prevHash {
				return fmt.Errorf("event %d (%s) previous_hash mismatch: got %s, want %s", i, e.ID, e.PreviousHash, prevHash)
			}
		}
		expectedHash := ComputeEventHash(e.PreviousHash, e.ID, e.Type, e.Timestamp, e.Payload)
		if e.Hash != expectedHash {
			return fmt.Errorf("event %d (%s) hash invalid: got %s, calculated %s", i, e.ID, e.Hash, expectedHash)
		}
		prevHash = e.Hash
		prevID = e.ID
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
		return err
	}
	return nil
}
