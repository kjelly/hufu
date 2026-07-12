package improve

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/anomalyco/hufu/internal/team"
)

func TestAnalyzeUsesLatestRunAndProducesDeterministicFindings(t *testing.T) {
	workspace := t.TempDir()
	teamDir := filepath.Join(t.TempDir(), "dev")
	if err := os.MkdirAll(filepath.Join(workspace, "logs", "audit"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(teamDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(teamDir, "team.yaml"), []byte("name: dev\nmax-rounds: 10\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	agent := "---\nname: developer\ntools: bash,view\n---\nFix bugs.\n"
	if err := os.WriteFile(filepath.Join(teamDir, "developer.md"), []byte(agent), 0o644); err != nil {
		t.Fatal(err)
	}

	events := []team.ExecutionEvent{
		{Version: 1, Timestamp: "2026-07-12T10:00:00Z", RunID: "old", Team: "dev", TaskID: "1", Agent: "developer", Attempt: 1, Status: "done"},
		{Version: 1, Timestamp: "2026-07-12T11:00:00Z", RunID: "latest", Team: "dev", TaskID: "2", Agent: "developer", Attempt: 1, Status: "in_progress"},
		{Version: 1, Timestamp: "2026-07-12T11:00:03Z", RunID: "latest", Team: "dev", TaskID: "2", Agent: "developer", Attempt: 1, Status: "error", Usage: team.ExecutionUsage{TotalTokens: 30}},
		{Version: 1, Timestamp: "2026-07-12T11:00:04Z", RunID: "latest", Team: "dev", TaskID: "2", Agent: "developer", Attempt: 2, Status: "in_progress"},
		{Version: 1, Timestamp: "2026-07-12T11:00:08Z", RunID: "latest", Team: "dev", TaskID: "2", Agent: "developer", Attempt: 2, Status: "done", Usage: team.ExecutionUsage{TotalTokens: 50}},
	}
	var lines []string
	for _, event := range events {
		data, err := json.Marshal(event)
		if err != nil {
			t.Fatal(err)
		}
		lines = append(lines, string(data))
	}
	if err := os.MkdirAll(filepath.Join(workspace, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, eventsPath), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	audit := `{"timestamp":"2026-07-12T11:00:02Z","team":"dev","agent":"developer","event":"tool_error","input":"secret"}` + "\n"
	if err := os.WriteFile(filepath.Join(workspace, "logs", "audit", "audit-2026-07-12.jsonl"), []byte(audit), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := Analyze(workspace, "dev", teamDir)
	if err != nil {
		t.Fatal(err)
	}
	if report.Metrics.RunID != "latest" || report.Metrics.TotalTasks != 1 || report.Metrics.TotalAttempts != 2 || report.Metrics.RetriedTasks != 1 {
		t.Fatalf("unexpected metrics: %+v", report.Metrics)
	}
	if report.Metrics.TokensByAgent["developer"] != 80 {
		t.Fatalf("tokens = %d, want 80", report.Metrics.TokensByAgent["developer"])
	}
	if report.Metrics.ToolErrorsByAgent["developer"] != 1 {
		t.Fatalf("tool errors = %d, want 1", report.Metrics.ToolErrorsByAgent["developer"])
	}
	markdown := Markdown(report)
	if strings.Contains(markdown, "secret") {
		t.Fatal("report must not include audit input")
	}
	if !strings.Contains(markdown, "retry_rate") {
		t.Fatal("expected retry finding")
	}
}

func TestLatestTeamWithoutEvents(t *testing.T) {
	_, err := LatestTeam(t.TempDir())
	if err != ErrNoExecutionData {
		t.Fatalf("error = %v, want %v", err, ErrNoExecutionData)
	}
}
