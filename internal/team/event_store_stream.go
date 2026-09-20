package team

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/kjelly/hufu/internal/eventchain"
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
	var chainVerifier eventchain.Verifier
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
			return fmt.Errorf("decode event %d: %w", chainVerifier.Sequence()+1, err)
		}
		if err := chainVerifier.Verify(eventchain.Entry{
			ID: event.ID, PreviousID: event.PreviousID, Type: event.Type, Timestamp: event.Timestamp,
			Payload: event.Payload, PreviousHash: event.PreviousHash, Hash: event.Hash,
		}); err != nil {
			return err
		}

		if err := visit(event); err != nil {
			return fmt.Errorf("visit event %d (%s): %w", chainVerifier.Sequence(), event.ID, err)
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("scan event store: %w", err)
	}
	return nil
}
