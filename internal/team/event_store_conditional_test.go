package team

import (
	"errors"
	"sync"
	"testing"
)

func TestEventStoreConditionalAppendAcrossHandles(t *testing.T) {
	workspace := t.TempDir()
	first, err := NewEventStore(workspace, "run-fence", "session-fence")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = first.Close() })
	second, err := NewEventStore(workspace, "run-fence", "session-fence")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = second.Close() })
	event := RunEvent{Type: "fence-test", Actor: "runtime", Payload: []byte(`{}`)}
	head, err := first.AppendPersistedAtHead(t.Context(), event, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := second.AppendPersistedAtHead(t.Context(), event, ""); !errors.Is(err, ErrEventHeadChanged) {
		t.Fatalf("stale handle appended: %v", err)
	}
	// Both independent writers observed the same head. Exactly one may win.
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for _, writer := range []*EventStore{first, second} {
		wg.Go(func() {
			<-start
			_, err := writer.AppendPersistedAtHead(t.Context(), event, head.ID)
			results <- err
		})
	}
	close(start)
	wg.Wait()
	close(results)
	winners, stale := 0, 0
	for err := range results {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, ErrEventHeadChanged):
			stale++
		default:
			t.Fatal(err)
		}
	}
	if winners != 1 || stale != 1 {
		t.Fatalf("winners=%d stale=%d", winners, stale)
	}
	if _, err := first.AppendPersisted(event); err != nil {
		t.Fatal(err)
	}
	events, err := first.ReadEvents()
	if err != nil || len(events) != 3 {
		t.Fatalf("events=%d error=%v", len(events), err)
	}
	if err := first.VerifyHashChain(); err != nil {
		t.Fatal(err)
	}
}
