package improve

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/audit"
	"github.com/kjelly/hufu/internal/team"
)

type analyticsBenchmarkProfile struct {
	name   string
	events int
	runs   int
}

var analyticsBenchmarkProfiles = []analyticsBenchmarkProfile{
	{name: "tiny", events: 100, runs: 5},
	{name: "small", events: 10_000, runs: 100},
	{name: "medium", events: 100_000, runs: 1_000},
	{name: "large", events: 1_000_000, runs: 10_000},
}

func writeAnalyticsBenchmarkFixture(tb testing.TB, profile analyticsBenchmarkProfile) (string, string) {
	tb.Helper()
	workspace := tb.TempDir()
	teamDir := filepath.Join(tb.TempDir(), "dev")
	if err := os.MkdirAll(teamDir, 0o755); err != nil {
		tb.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(teamDir, "team.yaml"), []byte("name: dev\nmax-rounds: 10\n"), 0o644); err != nil {
		tb.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(teamDir, "developer.md"), []byte("---\nname: developer\ntools: view,bash\n---\nImplement and verify.\n"), 0o644); err != nil {
		tb.Fatal(err)
	}
	writeBenchmarkExecutionEvents(tb, workspace, profile)
	writeBenchmarkAuditEvents(tb, workspace, profile)
	writeBenchmarkMemoryEvents(tb, workspace, profile)
	return workspace, teamDir
}

func writeBenchmarkExecutionEvents(tb testing.TB, workspace string, profile analyticsBenchmarkProfile) {
	tb.Helper()
	base := time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC)
	eventsPerRun := max(1, profile.events/profile.runs)
	rows := make([]team.ExecutionEvent, 0, profile.events)
	for i := range profile.events {
		run := min(profile.runs-1, i/eventsPerRun)
		withinRun := i % eventsPerRun
		task := withinRun / 2
		terminal := withinRun%2 == 1
		status := "in_progress"
		if terminal {
			status = "done"
			if task%17 == 0 {
				status = "error"
			}
		}
		attempt := 1
		if task%11 == 0 {
			attempt = 2
		}
		agent := fmt.Sprintf("agent-%d", task%4)
		model := fmt.Sprintf("model-%d", task%3)
		taskType := []string{"agent", "analysis", "verification"}[task%3]
		if !terminal && task%13 == 0 {
			agent, model, taskType = "initial-agent", "initial-model", "initial-type"
		}
		row := team.ExecutionEvent{
			Version:      2,
			Timestamp:    base.Add(time.Duration(run)*time.Minute + time.Duration(withinRun/2)*time.Millisecond).Format(time.RFC3339Nano),
			RunID:        fmt.Sprintf("run-%06d", run),
			Team:         "dev",
			TaskID:       fmt.Sprintf("task-%06d-%04d", run, task),
			Agent:        agent,
			Attempt:      attempt,
			Status:       status,
			Model:        model,
			TaskType:     taskType,
			Skills:       []string{fmt.Sprintf("skill-%d", task%5), fmt.Sprintf("shared-%d", task%2)},
			TeamRevision: fmt.Sprintf("revision-%d", run%7),
		}
		if terminal {
			// The runtime records an attempt's usage on its terminal row.
			row.Usage = team.ExecutionUsage{
				InputTokens:  5 + task%20,
				OutputTokens: 3 + task%10,
				TotalTokens:  8 + task%30,
			}
		}
		rows = append(rows, row)
	}
	appendEventChain(tb, workspace, executionFixtureEvents(tb, rows))
}

func writeBenchmarkAuditEvents(tb testing.TB, workspace string, profile analyticsBenchmarkProfile) {
	tb.Helper()
	dir := filepath.Join(workspace, "logs", "audit")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		tb.Fatal(err)
	}
	file, err := os.Create(filepath.Join(dir, "audit-benchmark.jsonl"))
	if err != nil {
		tb.Fatal(err)
	}
	writer := bufio.NewWriter(file)
	count := max(3, profile.events/20)
	base := time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC)
	for i := range count {
		eventType := audit.EventToolCall
		if i%19 == 0 {
			eventType = audit.EventToolError
		}
		event := audit.ToolAction{
			Timestamp: base.Add(time.Duration(i) * time.Millisecond).Format(time.RFC3339Nano),
			Team:      "dev",
			Agent:     fmt.Sprintf("agent-%d", i%4),
			Tool:      fmt.Sprintf("tool-%d", i%6),
			Event:     eventType,
		}
		data, marshalErr := json.Marshal(event)
		if marshalErr != nil {
			tb.Fatal(marshalErr)
		}
		if _, err = writer.Write(append(data, '\n')); err != nil {
			tb.Fatal(err)
		}
	}
	if _, err = writer.WriteString("not-json\n"); err != nil {
		tb.Fatal(err)
	}
	if err = writer.Flush(); err != nil {
		tb.Fatal(err)
	}
	if err = file.Close(); err != nil {
		tb.Fatal(err)
	}
}

func writeBenchmarkMemoryEvents(tb testing.TB, workspace string, profile analyticsBenchmarkProfile) {
	tb.Helper()
	count := max(3, profile.events/1_000)
	base := time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC)
	events := make([]team.RunEvent, 0, count)
	for i := range count {
		run := i % profile.runs
		eventType := []string{memoryRetrievedEvent, memoryUsageRecordedEvent, memoryOutcomeRecordedEvent}[i%3]
		payload := json.RawMessage(fmt.Sprintf(`{"retrieval_id":"retrieval-%d","context_item_id":"context-%d","content_hash":"hash-%d","policy_version":"policy-v1","reason_code":"normal","token_count":4,"disposition":"applied","signal":"verification_passed","direction":"positive"}`, i/3, i/3, i/3))
		events = append(events, team.RunEvent{
			RunID:     fmt.Sprintf("run-%06d", run),
			SessionID: "benchmark",
			TaskID:    fmt.Sprintf("task-%06d-%04d", run, i%max(1, profile.events/profile.runs/2)),
			Actor:     "runtime",
			Type:      eventType,
			Timestamp: base.Add(time.Duration(run) * time.Minute).Format(time.RFC3339Nano),
			Payload:   payload,
		})
	}
	appendEventChain(tb, workspace, events)
}

func addAnalyticsDiagnostics(total *AnalyticsDiagnostics, current AnalyticsDiagnostics) {
	total.EventsRead = current.EventsRead
	total.ExecutionRows = current.ExecutionRows
	total.AuditLinesRead = current.AuditLinesRead
	total.AuditRows = current.AuditRows
	total.MemoryRows = current.MemoryRows
	total.SelectedRuns = current.SelectedRuns
	total.ProjectedTasks = current.ProjectedTasks
	total.Open += current.Open
	total.LoadEventStore += current.LoadEventStore
	total.LoadAudit += current.LoadAudit
	total.BuildIndexes += current.BuildIndexes
	total.SelectRuns += current.SelectRuns
	total.ProjectTasks += current.ProjectTasks
	total.AggregateExecution += current.AggregateExecution
	total.AggregateAudit += current.AggregateAudit
	total.AggregateMemory += current.AggregateMemory
	total.AggregateGroups += current.AggregateGroups
	total.AggregateTrend += current.AggregateTrend
	total.Total += current.Total
}

func reportAnalyticsDiagnostics(b *testing.B, diagnostics AnalyticsDiagnostics) {
	b.Helper()
	iterations := float64(max(1, b.N))
	b.ReportMetric(float64(diagnostics.Open.Nanoseconds())/iterations, "open-ns/op")
	b.ReportMetric(float64(diagnostics.LoadEventStore.Nanoseconds())/iterations, "load_event_store-ns/op")
	b.ReportMetric(float64(diagnostics.LoadAudit.Nanoseconds())/iterations, "load_audit-ns/op")
	b.ReportMetric(float64(diagnostics.BuildIndexes.Nanoseconds())/iterations, "create_indexes-ns/op")
	b.ReportMetric(float64(diagnostics.SelectRuns.Nanoseconds())/iterations, "select_runs-ns/op")
	b.ReportMetric(float64(diagnostics.ProjectTasks.Nanoseconds())/iterations, "task_projection-ns/op")
	b.ReportMetric(float64(diagnostics.AggregateExecution.Nanoseconds())/iterations, "aggregate_execution-ns/op")
	b.ReportMetric(float64(diagnostics.AggregateAudit.Nanoseconds())/iterations, "aggregate_audit-ns/op")
	b.ReportMetric(float64(diagnostics.AggregateMemory.Nanoseconds())/iterations, "aggregate_memory-ns/op")
	b.ReportMetric(float64(diagnostics.AggregateGroups.Nanoseconds())/iterations, "aggregate_groups-ns/op")
	b.ReportMetric(float64(diagnostics.AggregateTrend.Nanoseconds())/iterations, "aggregate_trend-ns/op")
	b.ReportMetric(float64(diagnostics.Total.Nanoseconds())/iterations, "diagnostic_total-ns/op")
	b.ReportMetric(float64(diagnostics.ExecutionRows), "execution_rows")
	b.ReportMetric(float64(diagnostics.SelectedRuns), "selected_runs")
	b.ReportMetric(float64(diagnostics.ProjectedTasks), "projected_tasks")
}
