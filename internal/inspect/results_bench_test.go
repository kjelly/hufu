package inspect

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func BenchmarkHistoricalResultProjection(b *testing.B) {
	for _, count := range []int{1000, 10_000, 100_000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			events := make([]IndexedEvent, 0, count*2+1)
			events = append(events, IndexedEvent{Ordinal: 1, Event: team.RunEvent{ID: "run-start", RunID: "run-1", SessionID: "session-1", Type: "run_started", Payload: []byte(`{"team":"review"}`)}})
			for i := range count {
				id := fmt.Sprint(i)
				created, err := json.Marshal(map[string]any{"id": id, "status": "pending", "agent": "helper", "desc": "fixture task"})
				if err != nil {
					b.Fatal(err)
				}
				completed, err := json.Marshal(map[string]any{"id": id, "status": "done", "agent": "helper", "output": "saved result", "typed_result": map[string]any{"summary": "saved result", "details": "Unicode 結果"}})
				if err != nil {
					b.Fatal(err)
				}
				events = append(events, IndexedEvent{Ordinal: int64(i*2 + 2), Event: team.RunEvent{ID: "created-" + id, RunID: "run-1", SessionID: "session-1", TaskID: id, Type: "task_created", Payload: created}})
				events = append(events, IndexedEvent{Ordinal: int64(i*2 + 3), Event: team.RunEvent{ID: "completed-" + id, RunID: "run-1", SessionID: "session-1", TaskID: id, Type: "task_completed", Timestamp: "2026-10-13T10:00:00Z", Payload: completed}})
			}
			b.ReportAllocs()
			for b.Loop() {
				rows, _, err := projectHistoricalResults(context.Background(), events, "main", HistoricalResultScope{Team: "review"})
				if err != nil || len(rows) != count {
					b.Fatalf("projection failed: %d %v", len(rows), err)
				}
			}
		})
	}
}
