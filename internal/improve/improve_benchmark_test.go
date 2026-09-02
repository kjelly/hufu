package improve

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func BenchmarkAnalyzeRecentSQLiteAnalytics(b *testing.B) {
	workspace := b.TempDir()
	teamDir := filepath.Join(b.TempDir(), "dev")
	if err := os.MkdirAll(teamDir, 0o755); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(teamDir, "team.yaml"), []byte("name: dev\nmax-rounds: 10\n"), 0o644); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(teamDir, "developer.md"), []byte("---\nname: developer\ntools: view\n---\nFix bugs.\n"), 0o644); err != nil {
		b.Fatal(err)
	}
	events := []team.ExecutionEvent{
		{Timestamp: "2026-07-12T10:00:00Z", RunID: "run-1", Team: "dev", TaskID: "task-1", Agent: "developer", Attempt: 1, Status: "in_progress", Model: "model", TaskType: "agent", Usage: team.ExecutionUsage{InputTokens: 10}},
		{Timestamp: "2026-07-12T10:00:01Z", RunID: "run-1", Team: "dev", TaskID: "task-1", Agent: "developer", Attempt: 1, Status: "done", Model: "model", TaskType: "agent", Usage: team.ExecutionUsage{TotalTokens: 20}},
	}
	lines := make([]byte, 0, 256)
	for _, event := range events {
		data, err := json.Marshal(event)
		if err != nil {
			b.Fatal(err)
		}
		lines = append(lines, data...)
		lines = append(lines, '\n')
	}
	if err := os.MkdirAll(filepath.Join(workspace, "logs"), 0o755); err != nil {
		b.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, eventsPath), lines, 0o644); err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for b.Loop() {
		if _, err := AnalyzeRecent(workspace, "dev", teamDir, 1); err != nil {
			b.Fatal(err)
		}
	}
}
