package improve

// WP-0 (spec.md §15, "建立 baseline 與 parity contracts"): these tests lock the
// legacy Go aggregation behavior in this package *before* any of it is
// replaced by SQL (WP-1..WP-9). Each test documents one specific legacy
// semantic by asserting hand-derived expected values, not by comparing a
// function against itself. If a later SQL implementation diverges from one
// of these, that is a deliberate decision that needs sign-off, not an
// accidental regression.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/kjelly/hufu/internal/team"
)

// --- readEvents -------------------------------------------------------

func TestReadEvents_SkipsMalformedAndMissingRunID(t *testing.T) {
	workspace := t.TempDir()
	valid1, err := json.Marshal(team.ExecutionEvent{Version: 1, Timestamp: "2026-07-12T10:00:00Z", RunID: "r1", Team: "dev", TaskID: "1", Status: "done"})
	if err != nil {
		t.Fatal(err)
	}
	valid2, err := json.Marshal(team.ExecutionEvent{Version: 1, Timestamp: "2026-07-12T10:00:01Z", RunID: "r1", Team: "dev", TaskID: "2", Status: "done"})
	if err != nil {
		t.Fatal(err)
	}
	missingRunID, err := json.Marshal(team.ExecutionEvent{Version: 1, Timestamp: "2026-07-12T10:00:02Z", RunID: "", Team: "dev", TaskID: "3", Status: "done"})
	if err != nil {
		t.Fatal(err)
	}
	lines := []string{
		string(valid1),
		"{not valid json",
		string(missingRunID),
		string(valid2),
		"", // blank scanner lines must not be treated as malformed-and-fatal
	}
	if err := os.MkdirAll(filepath.Join(workspace, "logs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workspace, eventsPath), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	events, err := readEvents(workspace)
	if err != nil {
		t.Fatalf("readEvents: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("events = %d, want 2 (malformed line and missing run_id line must be skipped): %+v", len(events), events)
	}
	if events[0].TaskID != "1" || events[1].TaskID != "2" {
		t.Fatalf("unexpected surviving events: %+v", events)
	}
}

func TestReadEvents_MissingFileReturnsNilNoError(t *testing.T) {
	events, err := readEvents(t.TempDir())
	if err != nil {
		t.Fatalf("err = %v, want nil", err)
	}
	if events != nil {
		t.Fatalf("events = %+v, want nil", events)
	}
}

// --- selectRecentRuns ---------------------------------------------------

func TestSelectRecentRuns_TieBreaksByRunIDOnEqualEndTimestamp(t *testing.T) {
	// Two runs share the exact same End timestamp. Legacy sort.Slice is not
	// stable across equal keys unless the comparator itself breaks the tie,
	// so the comparator's `runs[i].ID < runs[j].ID` fallback is the only
	// thing making this deterministic. A SQL `ORDER BY run_end, run_id` (as
	// spec.md §9.1 recommends) reproduces this exactly; ORDER BY run_end
	// alone would not.
	events := []team.ExecutionEvent{
		{RunID: "run-b", Team: "dev", TaskID: "1", Timestamp: "2026-07-12T10:00:00Z"},
		{RunID: "run-a", Team: "dev", TaskID: "1", Timestamp: "2026-07-12T10:00:00Z"},
	}
	teamName, runs := selectRecentRuns(events, "dev", 5)
	if teamName != "dev" {
		t.Fatalf("teamName = %q, want dev", teamName)
	}
	if len(runs) != 2 || runs[0].ID != "run-a" || runs[1].ID != "run-b" {
		t.Fatalf("runs = %+v, want [run-a run-b] ordered by run_id on tied end timestamp", runs)
	}
}

func TestSelectRecentRuns_EmptyTeamNameDefaultsToLatestRunsTeam(t *testing.T) {
	events := []team.ExecutionEvent{
		{RunID: "r1", Team: "alpha", TaskID: "1", Timestamp: "2026-07-12T09:00:00Z"},
		{RunID: "r2", Team: "beta", TaskID: "1", Timestamp: "2026-07-12T10:00:00Z"},
	}
	teamName, runs := selectRecentRuns(events, "", 5)
	if teamName != "beta" {
		t.Fatalf("teamName = %q, want beta (team of the chronologically last run)", teamName)
	}
	if len(runs) != 1 || runs[0].ID != "r2" {
		t.Fatalf("runs = %+v, want only r2", runs)
	}
}

func TestSelectRecentRuns_SkipsEventsWithoutRunIDOrTeam(t *testing.T) {
	events := []team.ExecutionEvent{
		{RunID: "", Team: "dev", TaskID: "1", Timestamp: "2026-07-12T09:00:00Z"},
		{RunID: "r1", Team: "", TaskID: "1", Timestamp: "2026-07-12T09:00:00Z"},
		{RunID: "r2", Team: "dev", TaskID: "1", Timestamp: "2026-07-12T09:00:00Z"},
	}
	_, runs := selectRecentRuns(events, "dev", 5)
	if len(runs) != 1 || runs[0].ID != "r2" {
		t.Fatalf("runs = %+v, want only r2", runs)
	}
}

// --- summarizeTasks -------------------------------------------------------

func TestSummarizeTasks_LateMetadataAttributesToWholeTask(t *testing.T) {
	// Attempt 1 carries no agent/model/task_type. Attempt 2 (the retry)
	// finally reports them. The task-level summary must still attribute the
	// whole task to that metadata, not just the attempt that reported it.
	events := []team.ExecutionEvent{
		{RunID: "r1", Team: "dev", TaskID: "1", Attempt: 1, Status: "in_progress"},
		{RunID: "r1", Team: "dev", TaskID: "1", Attempt: 1, Status: "error"},
		{RunID: "r1", Team: "dev", TaskID: "1", Attempt: 2, Status: "in_progress", Agent: "developer", Model: "large", TaskType: "agent"},
		{RunID: "r1", Team: "dev", TaskID: "1", Attempt: 2, Status: "done"},
	}
	tasks := summarizeTasks(events)
	if len(tasks) != 1 {
		t.Fatalf("tasks = %+v, want 1", tasks)
	}
	got := tasks[0]
	if got.Agent != "developer" || got.Model != "large" || got.TaskType != "agent" {
		t.Fatalf("task metadata = %+v, want agent=developer model=large task_type=agent", got)
	}
	if got.Terminal != "done" || got.Attempts != 2 || got.TotalAttempts != 2 {
		t.Fatalf("task lifecycle = %+v", got)
	}
}

func TestSummarizeTasks_SkillOnlyOnRetryAttributedToTask(t *testing.T) {
	events := []team.ExecutionEvent{
		{RunID: "r1", Team: "dev", TaskID: "1", Attempt: 1, Status: "in_progress"},
		{RunID: "r1", Team: "dev", TaskID: "1", Attempt: 1, Status: "error"},
		{RunID: "r1", Team: "dev", TaskID: "1", Attempt: 2, Status: "in_progress", Skills: []string{"go"}},
		{RunID: "r1", Team: "dev", TaskID: "1", Attempt: 2, Status: "done", Skills: []string{"go"}},
	}
	tasks := summarizeTasks(events)
	if len(tasks) != 1 || len(tasks[0].Skills) != 1 || tasks[0].Skills[0] != "go" {
		t.Fatalf("tasks = %+v, want single task with skill [go]", tasks)
	}
}

func TestSummarizeTasks_SkillsFieldOverwritesRatherThanUnionsAcrossEvents(t *testing.T) {
	// IMPORTANT for WP-4: legacy `task.Skills = uniqueStrings(event.Skills)`
	// *replaces* the task's skill set every time an event carries a
	// non-empty Skills list; it does not merge/union it with skills seen on
	// earlier events for the same task. In every existing fixture the later
	// event's skill list happens to be a superset of the earlier one, so
	// this never surfaces — but it is a real, load-bearing distinction from
	// spec.md §6.2's proposed `task_skills` view, which is defined as
	// `SELECT DISTINCT run_id, task_id, skill` (a union across all events
	// for the task). If a disjoint skill list ever appears on a later
	// event, legacy drops the earlier skill entirely; the spec's DISTINCT
	// view would keep both. WP-4 must reproduce the *overwrite* semantic
	// below, not silently switch to union, unless that is an explicit,
	// signed-off semantic change (spec.md §14.3 forbids silent semantic
	// drift).
	events := []team.ExecutionEvent{
		{RunID: "r1", Team: "dev", TaskID: "1", Attempt: 1, Status: "in_progress", Skills: []string{"go"}},
		{RunID: "r1", Team: "dev", TaskID: "1", Attempt: 1, Status: "error", Skills: []string{"go"}},
		{RunID: "r1", Team: "dev", TaskID: "1", Attempt: 2, Status: "in_progress", Skills: []string{"review"}},
		{RunID: "r1", Team: "dev", TaskID: "1", Attempt: 2, Status: "done", Skills: []string{"review"}},
	}
	tasks := summarizeTasks(events)
	if len(tasks) != 1 {
		t.Fatalf("tasks = %+v, want 1", tasks)
	}
	if got := tasks[0].Skills; len(got) != 1 || got[0] != "review" {
		t.Fatalf("skills = %+v, want [review] only (overwrite, not union with earlier [go])", got)
	}
}

func TestSummarizeTasks_MultiSkillEventAppearsInBothSkillGroups(t *testing.T) {
	events := []team.ExecutionEvent{
		{RunID: "r1", Team: "dev", TaskID: "1", Attempt: 1, Status: "in_progress", Skills: []string{"go", "review"}},
		{RunID: "r1", Team: "dev", TaskID: "1", Attempt: 1, Status: "done", Skills: []string{"go", "review"}},
	}
	tasks := summarizeTasks(events)
	if len(tasks) != 1 || len(tasks[0].Skills) != 2 {
		t.Fatalf("tasks = %+v, want single task with 2 skills", tasks)
	}
}

// --- collectExecutionMetrics ----------------------------------------------

func TestCollectExecutionMetrics_RetriedTaskSumsTokensAcrossAllAttempts(t *testing.T) {
	events := []team.ExecutionEvent{
		{RunID: "r1", Team: "dev", TaskID: "1", Agent: "developer", Attempt: 1, Status: "in_progress"},
		{RunID: "r1", Team: "dev", TaskID: "1", Agent: "developer", Attempt: 1, Status: "error", Usage: team.ExecutionUsage{TotalTokens: 30}},
		{RunID: "r1", Team: "dev", TaskID: "1", Agent: "developer", Attempt: 2, Status: "in_progress"},
		{RunID: "r1", Team: "dev", TaskID: "1", Agent: "developer", Attempt: 2, Status: "done", Usage: team.ExecutionUsage{TotalTokens: 50}},
	}
	metrics := collectExecutionMetrics(events)
	if metrics.TotalTokens != 80 || metrics.TokensByAgent["developer"] != 80 {
		t.Fatalf("tokens = %d/%v, want 80", metrics.TotalTokens, metrics.TokensByAgent)
	}
	if metrics.TotalTasks != 1 || metrics.TotalAttempts != 2 || metrics.RetriedTasks != 1 || metrics.Done != 1 {
		t.Fatalf("unexpected metrics: %+v", metrics)
	}
	if metrics.RunCount != 1 || metrics.RunID != "r1" {
		t.Fatalf("run identity = %+v", metrics)
	}
}

func TestCollectExecutionMetrics_MissingAgentFallsBackToUnspecified(t *testing.T) {
	events := []team.ExecutionEvent{
		{RunID: "r1", Team: "dev", TaskID: "1", Attempt: 1, Status: "done", Usage: team.ExecutionUsage{TotalTokens: 7}},
	}
	metrics := collectExecutionMetrics(events)
	if metrics.TokensByAgent["unspecified"] != 7 {
		t.Fatalf("tokens by agent = %+v, want unspecified:7", metrics.TokensByAgent)
	}
}

func TestCollectExecutionMetrics_V1TelemetryWithoutNewerFieldsDoesNotPanic(t *testing.T) {
	// v1-era events omit Model/TaskType/Skills/TeamRevision/Phase/Provider.
	events := []team.ExecutionEvent{
		{Version: 1, Timestamp: "2026-07-12T10:00:00Z", RunID: "r1", Team: "dev", TaskID: "1", Agent: "developer", Attempt: 1, Status: "in_progress"},
		{Version: 1, Timestamp: "2026-07-12T10:00:01Z", RunID: "r1", Team: "dev", TaskID: "1", Agent: "developer", Attempt: 1, Status: "done", Usage: team.ExecutionUsage{TotalTokens: 3}},
	}
	metrics := collectExecutionMetrics(events)
	if metrics.TotalTasks != 1 || metrics.Done != 1 || metrics.TotalTokens != 3 {
		t.Fatalf("v1 metrics = %+v", metrics)
	}
	groups := collectGroupedMetrics(events)
	if g := groupByKey(groups.ByModel, "unspecified"); g == nil || g.TotalTasks != 1 {
		t.Fatalf("model group = %+v, want unspecified:1", groups.ByModel)
	}
	if g := groupByKey(groups.ByTaskType, "legacy/unspecified"); g == nil || g.TotalTasks != 1 {
		t.Fatalf("task type group = %+v, want legacy/unspecified:1", groups.ByTaskType)
	}
	if g := groupByKey(groups.BySkill, "none"); g == nil || g.TotalTasks != 1 {
		t.Fatalf("skill group = %+v, want none:1", groups.BySkill)
	}
}

func TestCollectExecutionMetrics_EventWindowIgnoresTaskIDButUsesAllParseableTimestamps(t *testing.T) {
	events := []team.ExecutionEvent{
		{RunID: "r1", Team: "dev", TaskID: "", Timestamp: "2026-07-12T09:00:00Z", Status: "run_started"},
		{RunID: "r1", Team: "dev", TaskID: "1", Timestamp: "2026-07-12T09:00:05Z", Status: "done"},
		{RunID: "r1", Team: "dev", TaskID: "", Timestamp: "not-a-timestamp", Status: "run_finished"},
	}
	metrics := collectExecutionMetrics(events)
	if metrics.StartedAt != "2026-07-12T09:00:00Z" || metrics.EndedAt != "2026-07-12T09:00:05Z" {
		t.Fatalf("window = %s..%s", metrics.StartedAt, metrics.EndedAt)
	}
}

// --- collectGroupedMetrics -------------------------------------------------

func TestCollectGroupedMetrics_MultiSkillOverlapAcrossTasks(t *testing.T) {
	events := []team.ExecutionEvent{
		{RunID: "r1", Team: "dev", TaskID: "1", Attempt: 1, Status: "done", Skills: []string{"go"}},
		{RunID: "r1", Team: "dev", TaskID: "2", Attempt: 1, Status: "done", Skills: []string{"go", "review"}},
	}
	groups := collectGroupedMetrics(events)
	if g := groupByKey(groups.BySkill, "go"); g == nil || g.TotalTasks != 2 {
		t.Fatalf("go skill group = %+v, want 2 tasks", g)
	}
	if g := groupByKey(groups.BySkill, "review"); g == nil || g.TotalTasks != 1 {
		t.Fatalf("review skill group = %+v, want 1 task", g)
	}
}

// --- collectAuditMetrics ----------------------------------------------------

func writeAuditFile(t *testing.T, workspace string, lines []string) {
	t.Helper()
	dir := filepath.Join(workspace, "logs", "audit")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "audit-2026-07-12.jsonl"), []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestCollectAuditMetrics_TimeWindowAndTeamScoping(t *testing.T) {
	workspace := t.TempDir()
	lines := []string{
		`{"timestamp":"2026-07-12T10:59:59Z","team":"dev","agent":"developer","event":"tool_call"}`, // before window
		`{"timestamp":"2026-07-12T11:00:00Z","team":"dev","agent":"developer","event":"tool_call"}`, // window start (inclusive)
		`{"timestamp":"2026-07-12T11:00:05Z","team":"other","agent":"developer","event":"tool_call"}`, // wrong team
		`{"timestamp":"2026-07-12T11:00:10Z","team":"dev","agent":"developer","event":"tool_error"}`, // window end (inclusive)
		`{"timestamp":"2026-07-12T11:00:11Z","team":"dev","agent":"developer","event":"tool_call"}`, // after window
	}
	writeAuditFile(t, workspace, lines)
	start, _ := time.Parse(time.RFC3339, "2026-07-12T11:00:00Z")
	end, _ := time.Parse(time.RFC3339, "2026-07-12T11:00:10Z")
	metrics := &Metrics{ToolCallsByAgent: map[string]int{}, ToolErrorsByAgent: map[string]int{}}
	collectAuditMetrics(filepath.Join(workspace, "logs", "audit"), "dev", start, end, metrics)
	if metrics.ToolCalls != 1 || metrics.ToolErrors != 1 {
		t.Fatalf("metrics = %+v, want 1 tool call and 1 tool error inside the inclusive window", metrics)
	}
	if metrics.ToolCallsByAgent["developer"] != 1 || metrics.ToolErrorsByAgent["developer"] != 1 {
		t.Fatalf("per-agent metrics = %+v", metrics)
	}
}

func TestCollectAuditMetrics_ArbitraryPayloadNeverReachesMetrics(t *testing.T) {
	workspace := t.TempDir()
	writeAuditFile(t, workspace, []string{
		`{"timestamp":"2026-07-12T11:00:00Z","team":"dev","agent":"developer","event":"tool_call","input":"top-secret-command","command":"rm -rf /"}`,
	})
	start, _ := time.Parse(time.RFC3339, "2026-07-12T10:00:00Z")
	end, _ := time.Parse(time.RFC3339, "2026-07-12T12:00:00Z")
	metrics := &Metrics{ToolCallsByAgent: map[string]int{}, ToolErrorsByAgent: map[string]int{}}
	collectAuditMetrics(filepath.Join(workspace, "logs", "audit"), "dev", start, end, metrics)
	if metrics.ToolCalls != 1 {
		t.Fatalf("tool calls = %d, want 1", metrics.ToolCalls)
	}
	// auditEvent only decodes timestamp/team/agent/event; unknown fields like
	// "input"/"command" are structurally impossible to surface here. This
	// guards the TEMP audit_events schema in spec.md §6.3/§20.1 against ever
	// growing a payload column.
}

func TestCollectAuditMetrics_MalformedLineSkipped(t *testing.T) {
	workspace := t.TempDir()
	writeAuditFile(t, workspace, []string{
		`{not valid json`,
		`{"timestamp":"2026-07-12T11:00:00Z","team":"dev","agent":"developer","event":"tool_call"}`,
	})
	start, _ := time.Parse(time.RFC3339, "2026-07-12T10:00:00Z")
	end, _ := time.Parse(time.RFC3339, "2026-07-12T12:00:00Z")
	metrics := &Metrics{ToolCallsByAgent: map[string]int{}, ToolErrorsByAgent: map[string]int{}}
	collectAuditMetrics(filepath.Join(workspace, "logs", "audit"), "dev", start, end, metrics)
	if metrics.ToolCalls != 1 {
		t.Fatalf("tool calls = %d, want 1 (malformed line must be skipped, not fatal)", metrics.ToolCalls)
	}
}
