package improve

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

type memoryMetricsSnapshot struct {
	MemoryRetrievalCount      int
	MemoryExposureCount       int
	MemoryAppliedCount        int
	MemoryAttributionCoverage float64
	MemoryVerifiedAssistRate  float64
	MemoryHarmfulUseRate      float64
	MemoryStaleRetrievalRate  float64
	MemoryTokenOverhead       float64
	MemoryAssistedRetryRate   float64
	MemoryUnassistedRetryRate float64
}

func snapshotMemoryMetrics(metrics Metrics) memoryMetricsSnapshot {
	return memoryMetricsSnapshot{
		MemoryRetrievalCount:      metrics.MemoryRetrievalCount,
		MemoryExposureCount:       metrics.MemoryExposureCount,
		MemoryAppliedCount:        metrics.MemoryAppliedCount,
		MemoryAttributionCoverage: metrics.MemoryAttributionCoverage,
		MemoryVerifiedAssistRate:  metrics.MemoryVerifiedAssistRate,
		MemoryHarmfulUseRate:      metrics.MemoryHarmfulUseRate,
		MemoryStaleRetrievalRate:  metrics.MemoryStaleRetrievalRate,
		MemoryTokenOverhead:       metrics.MemoryTokenOverhead,
		MemoryAssistedRetryRate:   metrics.MemoryAssistedRetryRate,
		MemoryUnassistedRetryRate: metrics.MemoryUnassistedRetryRate,
	}
}

func TestSQLMemoryMetricsParityAllTenFields(t *testing.T) {
	workspace := t.TempDir()
	selectedExecution := []team.ExecutionEvent{
		{Timestamp: "2026-07-12T10:00:00Z", RunID: "selected", Team: "dev", TaskID: "", Usage: team.ExecutionUsage{InputTokens: 5}},
		{Timestamp: "2026-07-12T10:00:01Z", RunID: "selected", Team: "dev", TaskID: "task-a", Attempt: 1, Status: "in_progress", Usage: team.ExecutionUsage{InputTokens: 10}},
		{Timestamp: "2026-07-12T10:00:02Z", RunID: "selected", Team: "dev", TaskID: "task-a", Attempt: 2, Status: "in_progress", Usage: team.ExecutionUsage{InputTokens: 3}},
		{Timestamp: "2026-07-12T10:00:03Z", RunID: "selected", Team: "dev", TaskID: "task-b", Attempt: 1, Status: "in_progress"},
		{Timestamp: "2026-07-12T10:00:04Z", RunID: "selected", Team: "dev", TaskID: "task-b", Attempt: 2, Status: "in_progress"},
		{Timestamp: "2026-07-12T10:00:05Z", RunID: "selected", Team: "dev", TaskID: "task-c", Attempt: 2, Status: "in_progress"},
	}
	allExecution := append([]team.ExecutionEvent{}, selectedExecution...)
	allExecution = append(allExecution, team.ExecutionEvent{Timestamp: "2026-07-12T11:00:00Z", RunID: "unrelated", Team: "other", TaskID: "other-task", Attempt: 2, Usage: team.ExecutionUsage{InputTokens: 100}})
	writeExecutionEvents(t, workspace, allExecution)
	writeMemoryEventStore(t, workspace, []team.RunEvent{
		{ID: "run-start", RunID: "selected", Type: "run_started", Actor: "runtime", Payload: []byte(`{"secret":"discard"}`)},
		{ID: "retrieval-1", RunID: "selected", Type: memoryRetrievedEvent, Actor: "runtime", Payload: []byte(`{"retrieval_id":"r1","reason_code":"stale_environment","token_count":2.9}`)},
		{ID: "retrieval-2", RunID: "selected", Type: memoryRetrievedEvent, Actor: "runtime", Payload: []byte(`{"retrieval_id":"r1","token_count":0}`)},
		{ID: "retrieval-3", RunID: "selected", Type: memoryRetrievedEvent, Actor: "runtime", Payload: []byte(`{"retrieval_id":"r2","token_count":-3}`)},
		{ID: "retrieval-null", RunID: "selected", Type: memoryRetrievedEvent, Actor: "runtime", Payload: []byte(`null`)},
		{ID: "retrieval-global", RunID: "unrelated", Type: memoryRetrievedEvent, Actor: "runtime", Payload: []byte(`{"retrieval_id":"global","token_count":4}`)},
		{ID: "usage-a-1", RunID: "selected", Type: memoryUsageRecordedEvent, Actor: "runtime", TaskID: "task-a", Payload: []byte(`{"disposition":"applied"}`)},
		{ID: "usage-a-2", RunID: "selected", Type: memoryUsageRecordedEvent, Actor: "runtime", TaskID: "task-a", Payload: []byte(`{"disposition":"applied"}`)},
		{ID: "usage-unrelated", RunID: "unrelated", Type: memoryUsageRecordedEvent, Actor: "runtime", TaskID: "other-task", Payload: []byte(`{"disposition":"applied"}`)},
		{ID: "outcome-verified", RunID: "selected", Type: memoryOutcomeRecordedEvent, Actor: "runtime", Payload: []byte(`{"signal":"verification_passed","direction":"negative"}`)},
		{ID: "outcome-stale", RunID: "selected", Type: memoryOutcomeRecordedEvent, Actor: "runtime", Payload: []byte(`{"signal":"stale_environment"}`)},
		{ID: "bad-shape", RunID: "selected", Type: memoryRetrievedEvent, Actor: "runtime", Payload: []byte(`[]`)},
	})

	want := collectExecutionMetrics(selectedExecution)
	collectMemoryMetrics(workspace, selectedExecution, &want)

	session := newTestSession(t)
	if _, err := session.loadExecutionEvents(t.Context(), filepath.Join(workspace, eventsPath)); err != nil {
		t.Fatal(err)
	}
	if _, err := session.loadMemoryEvents(t.Context(), workspace); err != nil {
		t.Fatal(err)
	}
	got, err := session.sqlCollectExecutionMetrics(t.Context(), []string{"selected"})
	if err != nil {
		t.Fatal(err)
	}
	if err := session.sqlCollectMemoryMetrics(t.Context(), []string{"selected"}, &got); err != nil {
		t.Fatal(err)
	}
	wantSnapshot := memoryMetricsSnapshot{
		MemoryRetrievalCount: 3, MemoryExposureCount: 5, MemoryAppliedCount: 3,
		MemoryAttributionCoverage: 0.6, MemoryVerifiedAssistRate: 1.0 / 3.0,
		MemoryHarmfulUseRate: 1.0 / 3.0, MemoryStaleRetrievalRate: 0.4,
		MemoryTokenOverhead: 1.0 / 3.0, MemoryAssistedRetryRate: 0.5,
		MemoryUnassistedRetryRate: 2,
	}
	if gotSnapshot := snapshotMemoryMetrics(got); !reflect.DeepEqual(gotSnapshot, wantSnapshot) {
		t.Fatalf("memory metrics mismatch:\n  got  = %+v\n  want = %+v", gotSnapshot, wantSnapshot)
	}
	if legacySnapshot := snapshotMemoryMetrics(want); !reflect.DeepEqual(legacySnapshot, wantSnapshot) {
		t.Fatalf("legacy fixture metrics = %+v, want %+v", legacySnapshot, wantSnapshot)
	}
}
