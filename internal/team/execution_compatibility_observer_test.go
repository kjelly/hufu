package team

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/executioncompat"
)

func TestExecutionCompatibilityObserverPayloadContainsOnlyActionableCounts(t *testing.T) {
	report := &executioncompat.InspectionReport{Findings: []executioncompat.Finding{
		{
			TaskID: "task-secret", RunID: "run-secret", SourceEventID: "event-secret",
			Classification: executioncompat.ClassificationMigratable,
			Features:       []executioncompat.Feature{executioncompat.FeatureLocalAlias, executioncompat.FeatureLegacyReceiptProvider},
		},
		{
			TaskID: "already-migrated", Classification: executioncompat.ClassificationMigrated,
			Features: []executioncompat.Feature{executioncompat.FeatureLegacyResumeMigration},
		},
	}}
	observer := NewExecutionCompatibilityObserver(report)
	if !observer.HasActionableState() {
		t.Fatal("observer did not retain actionable compatibility state")
	}
	payload := observer.Payload()
	if payload.SchemaVersion != executionCompatibilityObservationSchemaVersion {
		t.Fatalf("schema version = %d", payload.SchemaVersion)
	}
	if len(payload.Counts) != 2 || payload.Counts[executioncompat.FeatureLocalAlias] != 1 || payload.Counts[executioncompat.FeatureLegacyReceiptProvider] != 1 {
		t.Fatalf("payload counts = %#v", payload.Counts)
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"task-secret", "run-secret", "event-secret", "already-migrated"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("metadata payload leaked source identifier %q: %s", forbidden, encoded)
		}
	}
}

func TestExecutionCompatibilityObserverFlushesOncePerRunAndBranch(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-observed", "session-observed")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	store.SetBranchID("main")
	coord := &Coordinator{
		session:        &TeamSession{Workspace: workspace},
		eventStore:     store,
		eventJournal:   eventStoreJournal{store: store},
		executionRunID: "run-observed",
	}
	coord.SetExecutionCompatibilityObserver(NewExecutionCompatibilityObserver(&executioncompat.InspectionReport{Findings: []executioncompat.Finding{{
		Classification: executioncompat.ClassificationAmbiguous,
		Features:       []executioncompat.Feature{executioncompat.FeatureLocalAlias},
	}}}))
	coord.flushExecutionCompatibilityObservation(context.Background())
	coord.flushExecutionCompatibilityObservation(context.Background())

	events, err := store.ReadEvents()
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("observation event count = %d, want 1", len(events))
	}
	event := events[0]
	if event.Type != string(EventExecutionCompatibilityObserved) || event.RunID != "run-observed" || event.BranchID != "main" {
		t.Fatalf("observation event = %#v", event)
	}
	if event.IdempotencyKey != "execution-compatibility-observed:v1:run-observed:main" {
		t.Fatalf("idempotency key = %q", event.IdempotencyKey)
	}
	var payload ExecutionCompatibilityObservedPayload
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if err := validateExecutionCompatibilityObservedPayload(payload); err != nil {
		t.Fatal(err)
	}
}

func TestExecutionCompatibilityObserverAppendFailureIsTelemetryGap(t *testing.T) {
	workspace := t.TempDir()
	store, err := NewEventStore(workspace, "run-gap", "session-gap")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	store.SetBranchID("main")
	configureEventStoreSyncFailureForEventType(t, store, string(EventExecutionCompatibilityObserved), 1, errors.New("injected observation failure"))
	coord := &Coordinator{
		session:        &TeamSession{Workspace: workspace},
		eventStore:     store,
		eventJournal:   eventStoreJournal{store: store},
		executionRunID: "run-gap",
	}
	coord.SetExecutionCompatibilityObserver(NewExecutionCompatibilityObserver(&executioncompat.InspectionReport{Findings: []executioncompat.Finding{{
		Classification: executioncompat.ClassificationUnmigratable,
		Features:       []executioncompat.Feature{executioncompat.FeatureLegacyProviderBinding},
	}}}))
	coord.flushExecutionCompatibilityObservation(context.Background())
	if coord.DualWriteFailures() != 1 {
		t.Fatalf("dual-write gaps = %d, want 1", coord.DualWriteFailures())
	}
}
