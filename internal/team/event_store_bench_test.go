package team

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The event-store benchmarks measure the costs that grow with history on a
// synthetic log shaped like a long-lived workspace: about 2 KiB payloads, half
// of them carrying an idempotency key. fsync is disabled on the appending
// stores, so results measure CPU and allocation rather than the storage
// device. Run them with:
//
//	go test ./internal/team -run '^$' -bench 'BenchmarkEventStore(Append|Open|ReadEvents)$' -benchmem
//
// The interleaved append case grows the log by two events per iteration, so
// run it alone with -benchtime=200x to keep the history near its nominal size:
//
//	go test ./internal/team -run '^$' -bench 'BenchmarkEventStoreAppend$/events/interleaved' -benchtime=200x -benchmem
var benchEventStoreSizes = []int{1_000, 10_000, 70_000}

// writeBenchEventLog writes a hash-chained log of count events into a fresh
// workspace and returns the workspace.
func writeBenchEventLog(b *testing.B, count int) string {
	b.Helper()
	workspace := b.TempDir()
	path := filepath.Join(workspace, logsDir, eventStoreFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		b.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		b.Fatal(err)
	}
	w := bufio.NewWriter(f)
	pad := strings.Repeat("x", 2048)
	var previousID, previousHash string
	for i := range count {
		payload, err := json.Marshal(map[string]any{"progress": pad, "n": i})
		if err != nil {
			b.Fatal(err)
		}
		event := RunEvent{
			SchemaVersion: eventStoreSchemaVersion, ID: fmt.Sprintf("evt-bench-%d", i), PreviousID: previousID,
			RunID: "run-bench", SessionID: "session-bench", Actor: "worker", Type: "task_progress",
			Timestamp: "2026-09-26T00:00:00Z", Payload: payload, PreviousHash: previousHash,
		}
		if i%2 == 0 {
			event.IdempotencyKey = fmt.Sprintf("bench-key-%d", i)
		}
		event.Hash = ComputeEventHash(event.PreviousHash, event.ID, event.Type, event.Timestamp, event.Payload)
		line, err := json.Marshal(event)
		if err != nil {
			b.Fatal(err)
		}
		_, _ = w.Write(line)
		_ = w.WriteByte('\n')
		previousID, previousHash = event.ID, event.Hash
	}
	if err := w.Flush(); err != nil {
		b.Fatal(err)
	}
	if err := f.Close(); err != nil {
		b.Fatal(err)
	}
	return workspace
}

func openBenchAppender(b *testing.B, workspace string) *EventStore {
	b.Helper()
	store, err := OpenEventStore(workspace)
	if err != nil {
		b.Fatal(err)
	}
	store.syncFile = func() error { return nil }
	b.Cleanup(func() { _ = store.Close() })
	return store
}

func benchAppendEvent(writer string, n int) RunEvent {
	return RunEvent{
		Type: "task_progress", Actor: "worker", IdempotencyKey: fmt.Sprintf("%s-%d", writer, n),
		Payload: []byte(`{"progress":"bench"}`),
	}
}

// BenchmarkEventStoreAppend times one append when only this store writes
// ("own") and when another writer appended one event first ("interleaved"),
// which makes the store validate and merge that tail before writing.
func BenchmarkEventStoreAppend(b *testing.B) {
	for _, count := range benchEventStoreSizes {
		b.Run(fmt.Sprintf("events=%d/writer=own", count), func(b *testing.B) {
			store := openBenchAppender(b, writeBenchEventLog(b, count))
			b.ReportAllocs()
			n := 0
			for b.Loop() {
				n++
				if _, err := store.AppendPersisted(benchAppendEvent("own", n)); err != nil {
					b.Fatal(err)
				}
			}
		})
		b.Run(fmt.Sprintf("events=%d/writer=interleaved", count), func(b *testing.B) {
			workspace := writeBenchEventLog(b, count)
			store := openBenchAppender(b, workspace)
			other := openBenchAppender(b, workspace)
			b.ReportAllocs()
			n := 0
			for b.Loop() {
				n++
				b.StopTimer()
				if _, err := other.AppendPersisted(benchAppendEvent("other", n)); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
				if _, err := store.AppendPersisted(benchAppendEvent("own", n)); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkEventStoreOpen times the strict full scan every new store performs.
func BenchmarkEventStoreOpen(b *testing.B) {
	for _, count := range benchEventStoreSizes {
		b.Run(fmt.Sprintf("events=%d", count), func(b *testing.B) {
			workspace := writeBenchEventLog(b, count)
			b.ReportAllocs()
			for b.Loop() {
				store, err := OpenEventStore(workspace)
				if err != nil {
					b.Fatal(err)
				}
				_ = store.Close()
			}
		})
	}
}

// BenchmarkEventStoreReadEvents times the defensive copy of the whole cache.
func BenchmarkEventStoreReadEvents(b *testing.B) {
	for _, count := range benchEventStoreSizes {
		b.Run(fmt.Sprintf("events=%d", count), func(b *testing.B) {
			store, err := OpenEventStore(writeBenchEventLog(b, count))
			if err != nil {
				b.Fatal(err)
			}
			defer func() { _ = store.Close() }()
			b.ReportAllocs()
			for b.Loop() {
				if _, err := store.ReadEvents(); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
