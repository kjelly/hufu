package team

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// StreamValidatedRunEvents reads the durable event log without constructing an
// EventStore. It never creates, appends to, repairs, rescans into, or caches
// the canonical event store. Each validated event is passed to visit in file
// order. A callback may receive a valid prefix before a later chain error, so
// callers persisting streamed rows must make that persistence transactional.
//
// A missing event_store.jsonl is treated as an empty stream. Malformed JSON,
// a broken hash chain, I/O errors, and callback errors are returned.
func StreamValidatedRunEvents(ctx context.Context, workspace string, visit func(RunEvent) error) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if visit == nil {
		return fmt.Errorf("stream event store: nil visitor")
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	path := filepath.Join(workspace, logsDir, eventStoreFile)
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("open event store: %w", err)
	}
	defer func() { _ = f.Close() }()

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var sequence int
	var previousID, previousHash string
	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return err
		}
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var event RunEvent
		if err := json.Unmarshal(line, &event); err != nil {
			return fmt.Errorf("decode event %d: %w", sequence+1, err)
		}
		if sequence == 0 {
			if event.PreviousID != "" || event.PreviousHash != "" {
				return fmt.Errorf("first event (%s) must have empty previous_id and previous_hash", event.ID)
			}
		} else if event.PreviousID != previousID || event.PreviousHash != previousHash {
			return fmt.Errorf("event %d (%s) does not continue hash chain", sequence, event.ID)
		}
		expected := ComputeEventHash(event.PreviousHash, event.ID, event.Type, event.Timestamp, event.Payload)
		if event.Hash != expected {
			return fmt.Errorf("event %d (%s) hash invalid", sequence, event.ID)
		}

		if err := visit(event); err != nil {
			return fmt.Errorf("visit event %d (%s): %w", sequence+1, event.ID, err)
		}
		sequence++
		previousID, previousHash = event.ID, event.Hash
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan event store: %w", err)
	}
	return nil
}
