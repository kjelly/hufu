package context

import (
	"context"
	"fmt"
	"os"
	"sort"
	"sync"
	"testing"
	"time"
)

// TestCanonicalConcurrencyDiscovery is the opt-in WP-10 profiling gate. The
// prototype handles exist only in this test and do not alter repository
// ownership or connection counts in production.
func TestCanonicalConcurrencyDiscovery(t *testing.T) {
	if os.Getenv("HUFU_SQLITE_DISCOVERY") != "1" {
		t.Skip("set HUFU_SQLITE_DISCOVERY=1 to run the concurrency review")
	}
	current := runCanonicalContentionScenario(t, 0)
	logContentionResult(t, "current_single_handle", current)
	for _, poolSize := range []int{1, 2, 4, 8} {
		prototype := runCanonicalContentionScenario(t, poolSize)
		logContentionResult(t, fmt.Sprintf("prototype_read_pool_%d", poolSize), prototype)
	}
}

type contentionResult struct {
	readP95, writeP95       time.Duration
	waitDuration            time.Duration
	waitCount, busyRetries  int64
	readOperations, workers int
	writeOperations         int
}

func runCanonicalContentionScenario(t *testing.T, readPool int) contentionResult {
	t.Helper()
	writer := openCanonicalIndexFixture(t, 10_000)
	defer writer.Close()
	reader := writer
	if readPool > 0 {
		opened, err := OpenSQLiteReadOnly(writer.path)
		if err != nil {
			t.Fatal(err)
		}
		reader = opened.(*SQLiteRepository)
		reader.db.SetMaxOpenConns(readPool)
		defer reader.Close()
	}
	var foreignKeys int
	if err := writer.db.QueryRowContext(t.Context(), "PRAGMA foreign_keys").Scan(&foreignKeys); err != nil || foreignKeys != 1 {
		t.Fatalf("writer foreign_keys=%d, err=%v", foreignKeys, err)
	}

	const readerWorkers = 4
	const operationsPerWorker = 25
	query := RepositoryQuery{
		Scope: Scope{ProjectID: "project"}, Visibility: VisibilitySubtree,
		Lifecycles: []ContextLifecycle{LifecycleConfirmed}, Limit: 20,
	}
	reads := make(chan time.Duration, readerWorkers*operationsPerWorker)
	writes := make(chan time.Duration, operationsPerWorker)
	errors := make(chan error, readerWorkers+1)
	start := make(chan struct{})
	statsBefore := reader.db.Stats()
	busyBefore := writer.busyRetries.Load() + reader.busyRetries.Load()
	var group sync.WaitGroup
	for worker := range readerWorkers {
		group.Add(1)
		go func(worker int) {
			defer group.Done()
			<-start
			for operation := range operationsPerWorker {
				started := time.Now()
				items, err := reader.Query(t.Context(), query)
				reads <- time.Since(started)
				if err != nil {
					errors <- err
					return
				}
				if len(items) != query.Limit {
					errors <- fmt.Errorf("reader %d operation %d returned %d items", worker, operation, len(items))
					return
				}
			}
		}(worker)
	}
	group.Add(1)
	go func() {
		defer group.Done()
		<-start
		for operation := range operationsPerWorker {
			id := fmt.Sprintf("contention-%d-%d", readPool, operation)
			started := time.Now()
			err := writer.Append(t.Context(), ContextItem{
				ID: id, Kind: ContextObservation, Content: "contention write " + id,
				Scope: Scope{ProjectID: "writer-project"}, Authority: AuthorityTool, TrustLevel: TrustInternal,
			})
			writes <- time.Since(started)
			if err != nil {
				errors <- err
				return
			}
		}
	}()
	close(start)
	group.Wait()
	close(reads)
	close(writes)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	statsAfter := reader.db.Stats()

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := reader.Query(canceled, query); err == nil {
		t.Fatal("canceled concurrent reader query succeeded")
	}
	return contentionResult{
		readP95: p95Durations(reads), writeP95: p95Durations(writes),
		waitDuration:    statsAfter.WaitDuration - statsBefore.WaitDuration,
		waitCount:       statsAfter.WaitCount - statsBefore.WaitCount,
		busyRetries:     writer.busyRetries.Load() + reader.busyRetries.Load() - busyBefore,
		readOperations:  readerWorkers * operationsPerWorker,
		writeOperations: operationsPerWorker,
		workers:         readerWorkers + 1,
	}
}

func p95Durations(values <-chan time.Duration) time.Duration {
	var durations []time.Duration
	for duration := range values {
		durations = append(durations, duration)
	}
	sort.Slice(durations, func(left, right int) bool { return durations[left] < durations[right] })
	index := (len(durations)*95 + 99) / 100
	if index > 0 {
		index--
	}
	return durations[index]
}

func logContentionResult(t *testing.T, name string, result contentionResult) {
	t.Helper()
	t.Logf("scenario=%s workers=%d reads=%d writes=%d read_p95=%s write_p95=%s wait_count=%d wait_duration=%s busy_retries=%d", name, result.workers, result.readOperations, result.writeOperations, result.readP95, result.writeP95, result.waitCount, result.waitDuration, result.busyRetries)
}
