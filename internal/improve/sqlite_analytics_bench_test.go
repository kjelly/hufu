package improve

// Legacy-vs-SQL benchmarks at the scales spec.md §17 requires (10k/100k/1M
// execution events), comparing ingestion + aggregation time and allocations
// between the pre-refactor Go implementation and the SQLite TEMP + SQL
// implementation that replaced it.
//
// WP-9 deleted the production legacy aggregation code (spec.md §15,
// "刪除 legacy aggregation"), so there is no longer a live legacy code path
// to benchmark against. The functions below (legacyBench*) are a frozen,
// benchmark-only copy of the pre-refactor implementation, taken verbatim
// from commit d66bbc2 (the WP-0 baseline, the last commit before any SQL
// migration work) — internal/improve/improve.go, readEvents through
// collectGroupedMetrics. They are dead code from production's point of
// view: nothing outside this file calls them, and they exist solely so
// spec.md §17.1's acceptance gates ("SQL total wall time <= legacy Nx" at
// 10k/100k) can still be checked against a real baseline instead of an
// unfalsifiable "trust the SQL path" claim.
//
// Run with: go test ./internal/improve/... -run '^$' -bench
// BenchmarkExecutionTelemetryLegacyVsSQL -benchtime=1x
// (the 1M scale is slow enough that -benchtime=1x, one iteration, is the
// practical way to see all three scales in one run).

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/team"
)

// ---- legacy baseline (frozen copy of pre-SQL-migration improve.go) ----

type legacyBenchExecutionRun struct {
	ID     string
	Team   string
	Events []team.ExecutionEvent
	Start  time.Time
	End    time.Time
}

type legacyBenchTaskSummary struct {
	RunID         string
	TaskID        string
	Agent         string
	Model         string
	TaskType      string
	Skills        []string
	Terminal      string
	Attempts      int
	TotalAttempts int
	TotalTokens   int
}

func legacyBenchReadEvents(path string) ([]team.ExecutionEvent, error) {
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read execution events: %w", err)
	}
	defer func() { _ = f.Close() }()
	var events []team.ExecutionEvent
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 4<<20)
	for scanner.Scan() {
		var event team.ExecutionEvent
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil || event.RunID == "" {
			continue
		}
		events = append(events, event)
	}
	if err := scanner.Err(); err != nil {
		return events, fmt.Errorf("read execution events: %w", err)
	}
	return events, nil
}

func legacyBenchSelectRecentRuns(events []team.ExecutionEvent, teamName string, runCount int) (string, []legacyBenchExecutionRun) {
	runsByID := make(map[string]*legacyBenchExecutionRun)
	for _, event := range events {
		if event.RunID == "" || event.Team == "" {
			continue
		}
		run := runsByID[event.RunID]
		if run == nil {
			run = &legacyBenchExecutionRun{ID: event.RunID, Team: event.Team}
			runsByID[event.RunID] = run
		}
		run.Events = append(run.Events, event)
		if timestamp, err := time.Parse(time.RFC3339Nano, event.Timestamp); err == nil {
			if run.Start.IsZero() || timestamp.Before(run.Start) {
				run.Start = timestamp
			}
			if timestamp.After(run.End) {
				run.End = timestamp
			}
		}
	}
	runs := make([]legacyBenchExecutionRun, 0, len(runsByID))
	for _, run := range runsByID {
		runs = append(runs, *run)
	}
	sort.Slice(runs, func(i, j int) bool {
		if runs[i].End.Equal(runs[j].End) {
			return runs[i].ID < runs[j].ID
		}
		return runs[i].End.Before(runs[j].End)
	})
	if teamName == "" && len(runs) > 0 {
		teamName = runs[len(runs)-1].Team
	}
	selected := make([]legacyBenchExecutionRun, 0, runCount)
	for _, run := range runs {
		if run.Team == teamName {
			selected = append(selected, run)
		}
	}
	if len(selected) > runCount {
		selected = selected[len(selected)-runCount:]
	}
	return teamName, selected
}

func legacyBenchFlattenRuns(runs []legacyBenchExecutionRun) []team.ExecutionEvent {
	count := 0
	for _, run := range runs {
		count += len(run.Events)
	}
	events := make([]team.ExecutionEvent, 0, count)
	for _, run := range runs {
		events = append(events, run.Events...)
	}
	return events
}

func legacyBenchEventWindow(events []team.ExecutionEvent) (time.Time, time.Time) {
	var start, end time.Time
	for _, event := range events {
		timestamp, err := time.Parse(time.RFC3339Nano, event.Timestamp)
		if err != nil {
			continue
		}
		if start.IsZero() || timestamp.Before(start) {
			start = timestamp
		}
		if timestamp.After(end) {
			end = timestamp
		}
	}
	return start, end
}

func legacyBenchUniqueStrings(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func legacyBenchSummarizeTasks(events []team.ExecutionEvent) []legacyBenchTaskSummary {
	tasks := make(map[string]*legacyBenchTaskSummary)
	for _, event := range events {
		if event.TaskID == "" {
			continue
		}
		key := event.RunID + "\x00" + event.TaskID
		task := tasks[key]
		if task == nil {
			task = &legacyBenchTaskSummary{RunID: event.RunID, TaskID: event.TaskID}
			tasks[key] = task
		}
		if event.Agent != "" {
			task.Agent = event.Agent
		}
		if event.Model != "" {
			task.Model = event.Model
		}
		if event.TaskType != "" {
			task.TaskType = event.TaskType
		}
		if len(event.Skills) > 0 {
			task.Skills = legacyBenchUniqueStrings(event.Skills)
		}
		if event.Attempt > task.Attempts {
			task.Attempts = event.Attempt
		}
		if event.Status == "in_progress" {
			task.TotalAttempts++
		}
		if event.Status == "done" || event.Status == "error" || event.Status == "planned" {
			task.Terminal = event.Status
		}
		task.TotalTokens += event.Usage.TotalTokens
	}
	result := make([]legacyBenchTaskSummary, 0, len(tasks))
	for _, task := range tasks {
		result = append(result, *task)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].RunID == result[j].RunID {
			return result[i].TaskID < result[j].TaskID
		}
		return result[i].RunID < result[j].RunID
	})
	return result
}

func legacyBenchCollectExecutionMetrics(events []team.ExecutionEvent) Metrics {
	metrics := Metrics{TokensByAgent: map[string]int{}, ToolCallsByAgent: map[string]int{}, ToolErrorsByAgent: map[string]int{}}
	runIDs := make(map[string]struct{})
	tasks := legacyBenchSummarizeTasks(events)
	start, end := legacyBenchEventWindow(events)
	for _, event := range events {
		if event.RunID != "" {
			runIDs[event.RunID] = struct{}{}
		}
		if event.TaskID == "" {
			continue
		}
		agent := event.Agent
		if agent == "" {
			agent = "unspecified"
		}
		metrics.TokensByAgent[agent] += event.Usage.TotalTokens
		metrics.TotalTokens += event.Usage.TotalTokens
	}
	metrics.RunCount = len(runIDs)
	if metrics.RunCount == 1 {
		for runID := range runIDs {
			metrics.RunID = runID
		}
	}
	metrics.StartedAt, metrics.EndedAt = start.Format(time.RFC3339), end.Format(time.RFC3339)
	metrics.TotalTasks = len(tasks)
	for _, task := range tasks {
		metrics.TotalAttempts += task.TotalAttempts
		if task.Attempts > 1 {
			metrics.RetriedTasks++
		}
		switch task.Terminal {
		case "done":
			metrics.Done++
		case "error":
			metrics.Error++
		case "planned":
			metrics.Planned++
		}
	}
	return metrics
}

func legacyBenchFallbackGroup(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func legacyBenchAddGroupMetric(groups map[string]*GroupMetric, key string, task legacyBenchTaskSummary) {
	group := groups[key]
	if group == nil {
		group = &GroupMetric{Key: key}
		groups[key] = group
	}
	group.TotalTasks++
	group.TotalAttempts += task.TotalAttempts
	group.TotalTokens += task.TotalTokens
	if task.Attempts > 1 {
		group.RetriedTasks++
	}
	switch task.Terminal {
	case "done":
		group.Done++
	case "error":
		group.Error++
	case "planned":
		group.Planned++
	}
}

func legacyBenchGroupMetricSlice(groups map[string]*GroupMetric) []GroupMetric {
	result := make([]GroupMetric, 0, len(groups))
	for _, group := range groups {
		result = append(result, *group)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Key < result[j].Key })
	return result
}

func legacyBenchCollectGroupedMetrics(events []team.ExecutionEvent) GroupedMetrics {
	tasks := legacyBenchSummarizeTasks(events)
	byAgent := make(map[string]*GroupMetric)
	byTaskType := make(map[string]*GroupMetric)
	byModel := make(map[string]*GroupMetric)
	bySkill := make(map[string]*GroupMetric)
	for _, task := range tasks {
		legacyBenchAddGroupMetric(byAgent, legacyBenchFallbackGroup(task.Agent, "unspecified"), task)
		legacyBenchAddGroupMetric(byTaskType, legacyBenchFallbackGroup(task.TaskType, "legacy/unspecified"), task)
		legacyBenchAddGroupMetric(byModel, legacyBenchFallbackGroup(task.Model, "unspecified"), task)
		if len(task.Skills) == 0 {
			legacyBenchAddGroupMetric(bySkill, "none", task)
			continue
		}
		for _, skill := range task.Skills {
			legacyBenchAddGroupMetric(bySkill, skill, task)
		}
	}
	return GroupedMetrics{
		ByAgent:    legacyBenchGroupMetricSlice(byAgent),
		ByTaskType: legacyBenchGroupMetricSlice(byTaskType),
		ByModel:    legacyBenchGroupMetricSlice(byModel),
		BySkill:    legacyBenchGroupMetricSlice(bySkill),
	}
}

// ---- synthetic fixture generation ----

// writeSyntheticExecutionEvents writes n JSONL execution events for a single
// team, spread across runs of tasksPerRun tasks each (two events per task:
// in_progress then a terminal status), with a small, realistic spread of
// agents/models/task types/skills so grouped-metrics cardinality resembles
// real telemetry rather than a single degenerate group.
func writeSyntheticExecutionEvents(b *testing.B, path string, n int) {
	b.Helper()
	f, err := os.Create(path)
	if err != nil {
		b.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	w := bufio.NewWriterSize(f, 1<<20)
	defer func() { _ = w.Flush() }()

	agents := []string{"agent-a", "agent-b", "agent-c"}
	models := []string{"small", "large"}
	taskTypes := []string{"coordinator", "worker"}
	skillSets := [][]string{{"go"}, {"go", "review"}, {"python"}, nil}
	statuses := []string{"done", "error"}
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

	const tasksPerRun = 200
	const eventsPerTask = 2
	for i := 0; i < n; i++ {
		taskOrdinal := i / eventsPerTask
		runIdx := taskOrdinal / tasksPerRun
		taskIdx := taskOrdinal % tasksPerRun
		status := "in_progress"
		if i%eventsPerTask == eventsPerTask-1 {
			status = statuses[taskIdx%len(statuses)]
		}
		event := team.ExecutionEvent{
			Version:   3,
			Timestamp: base.Add(time.Duration(i) * time.Millisecond).Format(time.RFC3339Nano),
			RunID:     fmt.Sprintf("run-%d", runIdx),
			Team:      "bench",
			TaskID:    fmt.Sprintf("task-%d-%d", runIdx, taskIdx),
			Agent:     agents[taskIdx%len(agents)],
			Attempt:   1,
			Status:    status,
			Model:     models[taskIdx%len(models)],
			TaskType:  taskTypes[taskIdx%len(taskTypes)],
			Skills:    skillSets[taskIdx%len(skillSets)],
			Usage:     team.ExecutionUsage{TotalTokens: 10 + taskIdx%7},
		}
		data, err := json.Marshal(event)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := w.Write(data); err != nil {
			b.Fatal(err)
		}
		if err := w.WriteByte('\n'); err != nil {
			b.Fatal(err)
		}
	}
}

// ---- benchmarks ----

// BenchmarkExecutionTelemetryLegacyVsSQL is spec.md §17's required
// comparison: for each of 10k/100k/1M synthetic execution events, ingest the
// JSONL fixture and compute execution + grouped metrics via the legacy Go
// path and via the SQLite TEMP + SQL path, reporting wall time and
// allocations for both (allocations stand in for peak-memory growth, per
// spec.md §17's "可用 benchmark allocation 作 proxy").
//
// The 1M scale is slow; run with -benchtime=1x to see all three scales
// without an extended benchmark run recalibrating iteration counts:
//
//	go test ./internal/improve/... -run '^$' \
//	    -bench BenchmarkExecutionTelemetryLegacyVsSQL -benchtime=1x -benchmem
func BenchmarkExecutionTelemetryLegacyVsSQL(b *testing.B) {
	for _, n := range []int{10_000, 100_000, 1_000_000} {
		dir := b.TempDir()
		path := filepath.Join(dir, "execution-events.jsonl")
		writeSyntheticExecutionEvents(b, path, n)

		b.Run(fmt.Sprintf("legacy/n=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				events, err := legacyBenchReadEvents(path)
				if err != nil {
					b.Fatal(err)
				}
				_, runs := legacyBenchSelectRecentRuns(events, "bench", len(events))
				selected := legacyBenchFlattenRuns(runs)
				metrics := legacyBenchCollectExecutionMetrics(selected)
				groups := legacyBenchCollectGroupedMetrics(selected)
				if metrics.TotalTasks == 0 || len(groups.ByAgent) == 0 {
					b.Fatal("empty result")
				}
			}
		})

		b.Run(fmt.Sprintf("sql/n=%d", n), func(b *testing.B) {
			b.ReportAllocs()
			ctx := context.Background()
			for b.Loop() {
				session, err := openSQLiteAnalyticsSession(ctx)
				if err != nil {
					b.Fatal(err)
				}
				if _, err := session.loadExecutionEvents(ctx, path); err != nil {
					b.Fatal(err)
				}
				if err := session.createIndexes(ctx); err != nil {
					b.Fatal(err)
				}
				teamName, runIDs, err := session.sqlSelectRecentRuns(ctx, "bench", n)
				if err != nil {
					b.Fatal(err)
				}
				metrics, err := session.sqlCollectExecutionMetrics(ctx, runIDs)
				if err != nil {
					b.Fatal(err)
				}
				groups, err := session.sqlCollectGroupedMetrics(ctx, runIDs)
				if err != nil {
					b.Fatal(err)
				}
				_ = session.Close()
				if teamName != "bench" || metrics.TotalTasks == 0 || len(groups.ByAgent) == 0 {
					b.Fatal("empty result")
				}
			}
		})
	}
}
