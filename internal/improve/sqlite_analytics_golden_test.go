package improve

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/kjelly/hufu/internal/team"
)

func TestAnalyzeRecentSemanticGolden(t *testing.T) {
	workspace := t.TempDir()
	teamDir := filepath.Join(t.TempDir(), "dev")
	if err := os.MkdirAll(teamDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(teamDir, "team.yaml"), []byte("name: dev\nmax-rounds: 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(teamDir, "developer.md"), []byte("---\nname: developer\ndescription: Implements changes\ntools: view,bash\nguard: [require-tests]\n---\nImplement carefully.\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	writeExecutionEvents(t, workspace, semanticGoldenExecutionEvents())
	writeAuditJSONL(t, filepath.Join(workspace, "logs", "audit"), "audit-golden.jsonl", []string{
		`{"timestamp":"2026-07-12T10:00:00Z","team":"dev","agent":"developer","event":"tool_call"}`,
		`{"timestamp":"2026-07-12T10:00:01Z","team":"dev","agent":"developer","event":"tool_error"}`,
		`{"timestamp":"2026-07-12T10:00:01Z","team":"ops","agent":"operator","event":"tool_call"}`,
		`not-json`,
	})
	writeMemoryEventStore(t, workspace, []team.RunEvent{
		{ID: "selected-retrieval", RunID: "dev-latest", Type: memoryRetrievedEvent, Actor: "runtime", Timestamp: "2026-07-12T11:00:00Z", Payload: []byte(`{"retrieval_id":"selected","context_item_id":"context-selected","content_hash":"hash-selected","policy_version":"policy-v2","token_count":5}`)},
		{ID: "selected-usage", RunID: "dev-latest", TaskID: "task-latest", Type: memoryUsageRecordedEvent, Actor: "runtime", Timestamp: "2026-07-12T11:00:01Z", Payload: []byte(`{"retrieval_id":"selected","context_item_id":"context-selected","content_hash":"hash-selected","policy_version":"policy-v2","disposition":"applied"}`)},
		{ID: "selected-outcome", RunID: "dev-latest", Type: memoryOutcomeRecordedEvent, Actor: "runtime", Timestamp: "2026-07-12T11:00:02Z", Payload: []byte(`{"retrieval_id":"selected","signal":"verification_passed","direction":"positive"}`)},
		{ID: "global-retrieval", RunID: "unrelated", Type: memoryRetrievedEvent, Actor: "runtime", Timestamp: "2026-07-13T00:00:00Z", Payload: []byte(`{"retrieval_id":"global","reason_code":"stale_environment","token_count":7}`)},
	})

	var diagnostics AnalyticsDiagnostics
	report, err := analyzeRecent(t.Context(), workspace, "", teamDir, 100, &diagnostics)
	if err != nil {
		t.Fatal(err)
	}
	if diagnostics.ExecutionLinesRead != int64(len(semanticGoldenExecutionEvents())) || diagnostics.SelectedRuns != 4 || diagnostics.ProjectedTasks != 4 {
		t.Fatalf("diagnostics = %+v", diagnostics)
	}
	projection := newSemanticGoldenProjection(report)
	got, err := json.MarshalIndent(projection, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	got = append(got, '\n')
	want, err := os.ReadFile(filepath.Join("testdata", "sqlite_analytics_semantic.golden.json"))
	if err != nil {
		t.Fatalf("read semantic golden: %v\n--- generated ---\n%s", err, got)
	}
	if string(got) != string(want) {
		t.Fatalf("semantic report changed; update the golden only for an independently approved semantic change\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

type semanticGoldenTrend struct {
	RunID        string `json:"run_id"`
	StartedAt    string `json:"started_at"`
	EndedAt      string `json:"ended_at"`
	TeamRevision string `json:"team_revision,omitempty"`
	TotalTasks   int    `json:"total_tasks"`
	Done         int    `json:"done"`
	Error        int    `json:"error"`
	Planned      int    `json:"planned"`
	RetriedTasks int    `json:"retried_tasks"`
	TotalTokens  int    `json:"total_tokens"`
}

type semanticGoldenProjection struct {
	Team                 string                `json:"team"`
	Workspace            string                `json:"workspace"`
	GeneratedAt          string                `json:"generated_at"`
	RunIDs               []string              `json:"run_ids"`
	TeamRevisions        []string              `json:"team_revisions"`
	MemoryPolicyVersions []string              `json:"memory_policy_versions"`
	AppliedContextRefs   []ArtifactRef         `json:"applied_context_refs"`
	Metrics              Metrics               `json:"metrics"`
	Trend                []semanticGoldenTrend `json:"trend"`
	Agents               []string              `json:"agents"`
	TaskTypes            []string              `json:"task_types"`
	Models               []string              `json:"models"`
	Skills               []string              `json:"skills"`
	FindingRules         []string              `json:"finding_rules"`
}

func newSemanticGoldenProjection(report *Report) semanticGoldenProjection {
	projection := semanticGoldenProjection{
		Team:                 report.Team,
		Workspace:            "<workspace>",
		GeneratedAt:          "<generated-at>",
		RunIDs:               report.RunIDs,
		TeamRevisions:        report.TeamRevisions,
		MemoryPolicyVersions: report.MemoryPolicyVersions,
		AppliedContextRefs:   report.AppliedContextRefs,
		Metrics:              report.Metrics,
	}
	for _, point := range report.Trend {
		projection.Trend = append(projection.Trend, semanticGoldenTrend{
			RunID: point.RunID, StartedAt: point.StartedAt, EndedAt: point.EndedAt, TeamRevision: point.TeamRevision,
			TotalTasks: point.Metrics.TotalTasks, Done: point.Metrics.Done, Error: point.Metrics.Error,
			Planned: point.Metrics.Planned, RetriedTasks: point.Metrics.RetriedTasks, TotalTokens: point.Metrics.TotalTokens,
		})
	}
	projection.Agents = groupMetricKeys(report.Groups.ByAgent)
	projection.TaskTypes = groupMetricKeys(report.Groups.ByTaskType)
	projection.Models = groupMetricKeys(report.Groups.ByModel)
	projection.Skills = groupMetricKeys(report.Groups.BySkill)
	for _, finding := range report.Findings {
		projection.FindingRules = append(projection.FindingRules, finding.SourceRule)
	}
	return projection
}

func groupMetricKeys(groups []GroupMetric) []string {
	keys := make([]string, len(groups))
	for i, group := range groups {
		keys[i] = group.Key
	}
	return keys
}

func semanticGoldenExecutionEvents() []team.ExecutionEvent {
	return []team.ExecutionEvent{
		{Timestamp: "2026-07-12T09:00:00Z", RunID: "ops-run", Team: "ops", TaskID: "ops-task", Agent: "operator", Attempt: 1, Status: "done", Usage: team.ExecutionUsage{TotalTokens: 2}},
		// Version zero and an invalid timestamp represent a replayed legacy row.
		{Timestamp: "not-a-time", RunID: "dev-legacy", Team: "dev", TaskID: "legacy-task", Agent: "legacy", Attempt: 1, Status: "planned", Usage: team.ExecutionUsage{TotalTokens: 1}},
		{Version: 2, Timestamp: "2026-07-12T10:00:00Z", RunID: "dev-same-a", Team: "dev", TaskID: "task-a", Agent: "old-agent", Attempt: 1, Status: "in_progress", Model: "old-model", TaskType: "old-type", Skills: []string{"old-skill"}, TeamRevision: "revision-a", Usage: team.ExecutionUsage{InputTokens: 10, TotalTokens: 10}},
		{Version: 2, Timestamp: "2026-07-12T10:00:01Z", RunID: "dev-same-a", Team: "dev", TaskID: "task-a", Agent: "developer", Attempt: 2, Status: "done", Model: "model-b", TaskType: "verification", Skills: []string{"new-skill", "shared"}, TeamRevision: "revision-a", Usage: team.ExecutionUsage{OutputTokens: 6, TotalTokens: 6}},
		{Version: 2, Timestamp: "2026-07-12T10:00:00Z", RunID: "dev-same-b", Team: "dev", TaskID: "task-b", Agent: "reviewer", Attempt: 1, Status: "in_progress", Model: "model-a", TaskType: "review", Skills: []string{"review"}, TeamRevision: "revision-b", Usage: team.ExecutionUsage{InputTokens: 4, TotalTokens: 4}},
		{Version: 2, Timestamp: "2026-07-12T10:00:01Z", RunID: "dev-same-b", Team: "dev", TaskID: "task-b", Agent: "reviewer", Attempt: 1, Status: "error", Model: "model-a", TaskType: "review", Skills: []string{}, TeamRevision: "revision-b", Usage: team.ExecutionUsage{OutputTokens: 3, TotalTokens: 3}},
		{Version: 2, Timestamp: "2026-07-12T11:00:00Z", RunID: "dev-latest", Team: "dev", TaskID: "task-latest", Agent: "developer", Attempt: 1, Status: "in_progress", Model: "model-c", TaskType: "agent", Skills: []string{"memory"}, TeamRevision: "revision-c", Usage: team.ExecutionUsage{InputTokens: 20, TotalTokens: 20}},
		{Version: 2, Timestamp: "2026-07-12T11:00:02Z", RunID: "dev-latest", Team: "dev", TaskID: "task-latest", Agent: "developer", Attempt: 1, Status: "done", Model: "model-c", TaskType: "agent", Skills: []string{"memory"}, TeamRevision: "revision-c", Usage: team.ExecutionUsage{OutputTokens: 5, TotalTokens: 5}},
		{Version: 2, Timestamp: "2026-07-12T12:00:00Z", RunID: "", Team: "dev", TaskID: "empty-run", Status: "done"},
		{Version: 2, Timestamp: "2026-07-12T12:00:00Z", RunID: "empty-team-run", Team: "", TaskID: "empty-team", Status: "done"},
	}
}
