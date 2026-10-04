package team

import (
	"fmt"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/agent"
)

var (
	benchmarkMemoryOutcomeWeight  float64
	benchmarkMemoryLearningReport MemoryLearningReport
)

// BenchmarkMemoryEventConsumersSparse measures the production consumers on a
// long-lived workspace whose event history is mostly unrelated to memory.
func BenchmarkMemoryEventConsumersSparse(b *testing.B) {
	for _, count := range []int{1_000, 10_000, 50_000} {
		b.Run(fmt.Sprintf("events=%d/credit", count), func(b *testing.B) {
			coordinator, item := memoryEventConsumerBenchmarkFixture(count)
			b.ReportAllocs()
			for b.Loop() {
				benchmarkMemoryOutcomeWeight = coordinator.memoryOutcomeWeightForSignal(item, "verification_passed", "positive")
			}
		})
		b.Run(fmt.Sprintf("events=%d/report", count), func(b *testing.B) {
			coordinator, _ := memoryEventConsumerBenchmarkFixture(count)
			b.ReportAllocs()
			for b.Loop() {
				benchmarkMemoryLearningReport = coordinator.MemoryLearningReport()
			}
		})
	}
}

func memoryEventConsumerBenchmarkFixture(count int) (*Coordinator, *TodoItem) {
	noise := []byte(fmt.Sprintf(`{"noise":"%s"}`, strings.Repeat("x", 512)))
	events := make([]RunEvent, count)
	for i := range events {
		events[i] = RunEvent{RunID: "run-a", TaskID: "task-1", Type: "task_progress", Payload: noise}
		if i%2 != 0 {
			events[i].RunID = "run-b"
		}
		switch {
		case i%100 == 0:
			events[i].Type = "memory_outcome_recorded"
			events[i].Payload = []byte(`{"retrieval_id":"retrieval-1","signal":"verification_passed","direction":"positive","effective_weight":0.25}`)
		case i%100 == 1:
			events[i].Type = "memory_retrieved"
			events[i].Payload = []byte(`{"retrieval_id":"retrieval-1"}`)
		case i%100 == 2:
			events[i].Type = "memory_usage_recorded"
			events[i].Payload = []byte(`{"disposition":"applied"}`)
		}
	}
	store := &EventStore{
		mu: make(chan struct{}, 1), path: "benchmark-event-store", stateValid: true, cachedEvents: events,
	}
	store.mu <- struct{}{}
	coordinator := &Coordinator{
		eventStore: store,
		session:    &TeamSession{Config: agent.TeamConfig{MemoryLearning: agent.DefaultMemoryLearningPolicy()}},
	}
	item := &TodoItem{ID: "task-1", MemoryManifests: []MemoryInjectionManifest{{RetrievalID: "retrieval-1"}}}
	return coordinator, item
}
