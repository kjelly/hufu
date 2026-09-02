package improve

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kjelly/hufu/internal/team"
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

func TestLatestTeamUsesNewestValidRun(t *testing.T) {
	workspace := t.TempDir()
	writeExecutionEvents(t, workspace, []team.ExecutionEvent{
		{Timestamp: "2026-07-12T10:00:00Z", RunID: "old", Team: "old-team", TaskID: "task", Status: "done"},
		{Timestamp: "2026-07-12T11:00:00Z", RunID: "new", Team: "new-team", TaskID: "task", Status: "done"},
	})
	teamName, err := LatestTeam(workspace)
	if err != nil {
		t.Fatal(err)
	}
	if teamName != "new-team" {
		t.Fatalf("team = %q, want new-team", teamName)
	}
}

func TestAnalyzeRecentAggregatesTeamRunsAndGroupsMetadata(t *testing.T) {
	workspace := t.TempDir()
	teamDir := filepath.Join(t.TempDir(), "dev")
	if err := os.MkdirAll(filepath.Join(workspace, "logs", "audit"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(teamDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(teamDir, "team.yaml"), []byte("name: dev\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(teamDir, "developer.md"), []byte("---\nname: developer\ntools: bash,view\n---\nFix bugs.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	events := []team.ExecutionEvent{
		{Version: 2, Timestamp: "2026-07-12T10:00:00Z", RunID: "dev-1", Team: "dev", TaskID: "1", Agent: "developer", Attempt: 1, Status: "in_progress", Model: "small", TaskType: "coordinator", Skills: []string{"go"}, TeamRevision: "rev-a"},
		{Version: 2, Timestamp: "2026-07-12T10:00:03Z", RunID: "dev-1", Team: "dev", TaskID: "1", Agent: "developer", Attempt: 1, Status: "done", Model: "small", TaskType: "coordinator", Skills: []string{"go"}, TeamRevision: "rev-a", Usage: team.ExecutionUsage{TotalTokens: 10}},
		{Version: 2, Timestamp: "2026-07-12T11:00:00Z", RunID: "dev-2", Team: "dev", TaskID: "1", Agent: "developer", Attempt: 1, Status: "in_progress", Model: "large", TaskType: "agent", TeamRevision: "rev-b"},
		{Version: 2, Timestamp: "2026-07-12T11:00:02Z", RunID: "dev-2", Team: "dev", TaskID: "1", Agent: "developer", Attempt: 1, Status: "error", Model: "large", TaskType: "agent", TeamRevision: "rev-b", Usage: team.ExecutionUsage{TotalTokens: 5}},
		{Version: 2, Timestamp: "2026-07-12T11:00:04Z", RunID: "dev-2", Team: "dev", TaskID: "1", Agent: "developer", Attempt: 2, Status: "in_progress", Model: "large", TaskType: "agent", Skills: []string{"go", "review"}, TeamRevision: "rev-b"},
		{Version: 2, Timestamp: "2026-07-12T11:00:06Z", RunID: "dev-2", Team: "dev", TaskID: "1", Agent: "developer", Attempt: 2, Status: "done", Model: "large", TaskType: "agent", Skills: []string{"go", "review"}, TeamRevision: "rev-b", Usage: team.ExecutionUsage{TotalTokens: 15}},
		{Version: 2, Timestamp: "2026-07-12T11:00:07Z", RunID: "dev-2", Team: "", TaskID: "ignored", Agent: "developer", Attempt: 1, Status: "done", Model: "secret-model", TeamRevision: "wrong-revision", Usage: team.ExecutionUsage{TotalTokens: 100}},
		{Version: 2, Timestamp: "2026-07-12T12:00:00Z", RunID: "other-1", Team: "other", TaskID: "1", Agent: "helper", Attempt: 1, Status: "done", Usage: team.ExecutionUsage{TotalTokens: 99}},
	}
	writeExecutionEvents(t, workspace, events)
	audit := strings.Join([]string{
		`{"timestamp":"2026-07-12T10:00:00Z","team":"dev","agent":"developer","event":"tool_call"}`,
		`{"timestamp":"2026-07-12T11:00:01Z","team":"dev","agent":"developer","event":"tool_error","input":"secret"}`,
		`{"timestamp":"2026-07-12T11:00:06Z","team":"dev","agent":"developer","event":"tool_call"}`,
		`{"timestamp":"2026-07-12T11:00:07Z","team":"other","agent":"helper","event":"tool_error"}`,
		`not-json`,
		`{"timestamp":"not-a-timestamp","team":"dev","agent":"developer","event":"tool_call"}`,
	}, "\n") + "\n"
	if err := os.WriteFile(filepath.Join(workspace, "logs", "audit", "audit-2026-07-12.jsonl"), []byte(audit), 0o644); err != nil {
		t.Fatal(err)
	}

	report, err := AnalyzeRecent(workspace, "dev", teamDir, 2)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(report.RunIDs, ","), "dev-1,dev-2"; got != want {
		t.Fatalf("run IDs = %q, want %q", got, want)
	}
	if report.Metrics.RunCount != 2 || report.Metrics.TotalTasks != 2 || report.Metrics.Done != 2 || report.Metrics.TotalAttempts != 3 || report.Metrics.RetriedTasks != 1 || report.Metrics.TotalTokens != 30 {
		t.Fatalf("unexpected aggregate metrics: %+v", report.Metrics)
	}
	if report.Metrics.ToolCalls != 2 || report.Metrics.ToolErrors != 1 || report.Metrics.ToolCallsByAgent["developer"] != 2 || report.Metrics.ToolErrorsByAgent["developer"] != 1 {
		t.Fatalf("unexpected aggregate audit metrics: %+v", report.Metrics)
	}
	if got, want := strings.Join(report.TeamRevisions, ","), "rev-a,rev-b"; got != want {
		t.Fatalf("team revisions = %q, want %q", got, want)
	}
	if group := groupByKey(report.Groups.BySkill, "go"); group == nil || group.TotalTasks != 2 {
		t.Fatalf("go skill group = %+v, want two tasks", group)
	}
	if group := groupByKey(report.Groups.BySkill, "review"); group == nil || group.TotalTasks != 1 {
		t.Fatalf("review skill group = %+v, want one task", group)
	}
	if group := groupByKey(report.Groups.ByTaskType, "agent"); group == nil || group.RetriedTasks != 1 {
		t.Fatalf("agent task type group = %+v, want one retry", group)
	}
	if len(report.Trend) != 2 || report.Trend[1].Metrics.TotalTokens != 20 || report.Trend[0].Metrics.ToolCalls != 1 || report.Trend[1].Metrics.ToolCalls != 1 || report.Trend[1].Metrics.ToolErrors != 1 || report.Trend[0].TeamRevision != "rev-a" || report.Trend[1].TeamRevision != "rev-b" {
		t.Fatalf("trend = %+v", report.Trend)
	}
	if len(report.Findings) == 0 || strings.Join(report.Findings[0].RunIDs, ",") != "dev-1,dev-2" {
		t.Fatalf("finding provenance = %+v", report.Findings)
	}
	markdown := Markdown(report)
	if strings.Contains(markdown, "secret") || !strings.Contains(markdown, "## Trend by Run") || !strings.Contains(markdown, "### By Skill") {
		t.Fatalf("unexpected markdown report:\n%s", markdown)
	}
}

func TestAnalyzeRecentIncludesSQLMemoryMetrics(t *testing.T) {
	workspace := t.TempDir()
	teamDir := filepath.Join(t.TempDir(), "dev")
	if err := os.MkdirAll(teamDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(teamDir, "team.yaml"), []byte("name: dev\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(teamDir, "developer.md"), []byte("---\nname: developer\n---\nFix bugs.\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	writeExecutionEvents(t, workspace, []team.ExecutionEvent{
		{Timestamp: "2026-07-12T10:00:00Z", RunID: "run-1", Team: "dev", TaskID: "task-1", Attempt: 1, Status: "done", Usage: team.ExecutionUsage{InputTokens: 10}},
	})
	writeMemoryEventStore(t, workspace, []team.RunEvent{
		{ID: "retrieval-1", RunID: "run-1", Type: memoryRetrievedEvent, Actor: "runtime", Payload: []byte(`{"retrieval_id":"r1","token_count":2.9}`)},
		{ID: "usage-1", RunID: "run-1", TaskID: "task-1", Type: memoryUsageRecordedEvent, Actor: "runtime", Payload: []byte(`{"disposition":"applied"}`)},
	})

	report, err := AnalyzeRecent(workspace, "dev", teamDir, 1)
	if err != nil {
		t.Fatal(err)
	}
	if report.Metrics.MemoryRetrievalCount != 1 || report.Metrics.MemoryExposureCount != 1 || report.Metrics.MemoryAppliedCount != 1 {
		t.Fatalf("memory counts = %+v", report.Metrics)
	}
	if report.Metrics.MemoryTokenOverhead != 0.2 || report.Metrics.MemoryAttributionCoverage != 1 {
		t.Fatalf("memory rates = %+v", report.Metrics)
	}
}

func writeExecutionEvents(t *testing.T, workspace string, events []team.ExecutionEvent) {
	t.Helper()
	lines := make([]string, 0, len(events))
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
}

func groupByKey(groups []GroupMetric, key string) *GroupMetric {
	for i := range groups {
		if groups[i].Key == key {
			return &groups[i]
		}
	}
	return nil
}
