// Package improve turns durable execution telemetry into a deterministic,
// shareable improvement report. It intentionally never sends workspace data to
// an LLM and never includes prompt, output, or tool-argument content.
package improve

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/team"

	"gopkg.in/yaml.v3"
)

const eventsPath = "logs/execution-events.jsonl"

var ErrNoExecutionData = errors.New("no execution event data")

type AgentDefinition struct {
	Name        string
	Description string
	Tools       []string
	Guard       []string
	Body        string
}

type TeamDefinition struct {
	Name      string
	MaxRounds int
	Agents    []AgentDefinition
}

type Metrics struct {
	RunID                     string         `json:"run_id"`
	RunCount                  int            `json:"run_count"`
	StartedAt                 string         `json:"started_at"`
	EndedAt                   string         `json:"ended_at"`
	TotalTasks                int            `json:"total_tasks"`
	Done                      int            `json:"done"`
	Error                     int            `json:"error"`
	Planned                   int            `json:"planned"`
	TotalAttempts             int            `json:"total_attempts"`
	RetriedTasks              int            `json:"retried_tasks"`
	TotalTokens               int            `json:"total_tokens"`
	ToolCalls                 int            `json:"tool_calls"`
	ToolErrors                int            `json:"tool_errors"`
	TokensByAgent             map[string]int `json:"tokens_by_agent"`
	ToolCallsByAgent          map[string]int `json:"tool_calls_by_agent"`
	ToolErrorsByAgent         map[string]int `json:"tool_errors_by_agent"`
	MemoryRetrievalCount      int            `json:"memory_retrieval_count"`
	MemoryExposureCount       int            `json:"memory_exposure_count"`
	MemoryAppliedCount        int            `json:"memory_applied_count"`
	MemoryAttributionCoverage float64        `json:"memory_attribution_coverage"`
	MemoryVerifiedAssistRate  float64        `json:"memory_verified_assist_rate"`
	MemoryHarmfulUseRate      float64        `json:"memory_harmful_use_rate"`
	MemoryStaleRetrievalRate  float64        `json:"memory_stale_retrieval_rate"`
	MemoryTokenOverhead       float64        `json:"memory_token_overhead"`
	MemoryAssistedRetryRate   float64        `json:"memory_assisted_retry_rate"`
	MemoryUnassistedRetryRate float64        `json:"memory_unassisted_retry_rate"`
}

type Finding struct {
	Layer         string   `json:"layer"`
	Target        string   `json:"target"`
	Severity      string   `json:"severity"`
	Category      string   `json:"category"`
	Metric        string   `json:"metric"`
	Value         string   `json:"value"`
	Suggestion    string   `json:"suggestion"`
	SourceRule    string   `json:"source_rule"`
	Evidence      string   `json:"evidence"`
	Confidence    string   `json:"confidence"`
	RunIDs        []string `json:"run_ids,omitempty"`
	TeamRevisions []string `json:"team_revisions,omitempty"`
}

// TrendPoint is a single run in chronological order. It contains only durable
// execution metadata, never task descriptions, output, tool arguments, or
// tool results.
type TrendPoint struct {
	RunID        string  `json:"run_id"`
	StartedAt    string  `json:"started_at"`
	EndedAt      string  `json:"ended_at"`
	TeamRevision string  `json:"team_revision,omitempty"`
	Metrics      Metrics `json:"metrics"`
}

// GroupMetric summarizes one independent telemetry dimension. Skill groups
// overlap by design: a task associated with two skills appears in both groups.
type GroupMetric struct {
	Key           string `json:"key"`
	TotalTasks    int    `json:"total_tasks"`
	Done          int    `json:"done"`
	Error         int    `json:"error"`
	Planned       int    `json:"planned"`
	TotalAttempts int    `json:"total_attempts"`
	RetriedTasks  int    `json:"retried_tasks"`
	TotalTokens   int    `json:"total_tokens"`
}

type GroupedMetrics struct {
	ByAgent    []GroupMetric `json:"by_agent"`
	ByTaskType []GroupMetric `json:"by_task_type"`
	ByModel    []GroupMetric `json:"by_model"`
	BySkill    []GroupMetric `json:"by_skill"`
}

type Report struct {
	Team          string         `json:"team"`
	Workspace     string         `json:"workspace"`
	GeneratedAt   string         `json:"generated_at"`
	Source        string         `json:"source"`
	RunIDs        []string       `json:"run_ids"`
	TeamRevisions []string       `json:"team_revisions,omitempty"`
	Metrics       Metrics        `json:"metrics"`
	Trend         []TrendPoint   `json:"trend"`
	Groups        GroupedMetrics `json:"groups"`
	Findings      []Finding      `json:"findings"`
}

type agentFrontmatter struct {
	Name        string      `yaml:"name"`
	Description string      `yaml:"description"`
	Tools       interface{} `yaml:"tools"`
	Guard       []string    `yaml:"guard"`
}

type teamYAML struct {
	Name      string `yaml:"name"`
	MaxRounds int    `yaml:"max-rounds"`
}

type auditEvent struct {
	Timestamp string `json:"timestamp"`
	Team      string `json:"team"`
	Agent     string `json:"agent"`
	Event     string `json:"event"`
}

// LatestTeam returns the team attached to the newest durable execution event.
func LatestTeam(workspace string) (string, error) {
	ctx := context.Background()
	analytics, err := openSQLiteAnalyticsSession(ctx)
	if err != nil {
		return "", err
	}
	defer func() { _ = analytics.Close() }()
	if _, err := analytics.loadExecutionEvents(ctx, filepath.Join(workspace, eventsPath)); err != nil {
		return "", newAnalyticsError(AnalyticsStageLoadExecution, err)
	}
	teamName, runs, err := analytics.sqlSelectRecentRunSummaries(ctx, "", 1)
	if err != nil {
		return "", newAnalyticsError(AnalyticsStageSelectRuns, err)
	}
	if len(runs) == 0 {
		return "", ErrNoExecutionData
	}
	return teamName, nil
}

// Analyze produces a deterministic report for the newest run in workspace.
// It is retained for callers that expect the original one-run behaviour.
func Analyze(workspace, teamName, teamDir string) (*Report, error) {
	return AnalyzeRecent(workspace, teamName, teamDir, 1)
}

// AnalyzeRecent produces a deterministic report for the most recent runCount
// runs of one team. teamDir is read directly and does not load a TeamSession or
// create folders.
func AnalyzeRecent(workspace, teamName, teamDir string, runCount int) (*Report, error) {
	if runCount < 1 {
		return nil, fmt.Errorf("run count must be at least 1")
	}
	ctx := context.Background()
	analytics, err := openSQLiteAnalyticsSession(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = analytics.Close() }()
	if _, err := analytics.loadExecutionEvents(ctx, filepath.Join(workspace, eventsPath)); err != nil {
		return nil, newAnalyticsError(AnalyticsStageLoadExecution, err)
	}
	if _, err := analytics.loadAuditEvents(ctx, filepath.Join(workspace, "logs", "audit")); err != nil {
		return nil, newAnalyticsError(AnalyticsStageLoadAudit, err)
	}
	if _, err := analytics.loadMemoryEvents(ctx, workspace); err != nil {
		return nil, newAnalyticsError(AnalyticsStageLoadMemory, err)
	}
	if err := analytics.createIndexes(ctx); err != nil {
		return nil, newAnalyticsError(AnalyticsStageSchema, err)
	}

	teamName, selectedRuns, err := analytics.sqlSelectRecentRunSummaries(ctx, teamName, runCount)
	if err != nil {
		return nil, newAnalyticsError(AnalyticsStageSelectRuns, err)
	}
	if len(selectedRuns) == 0 {
		return nil, ErrNoExecutionData
	}

	def, err := readTeamDefinition(teamDir)
	if err != nil {
		return nil, err
	}
	if def.Name == "" {
		def.Name = teamName
	}
	runIDs := make([]string, len(selectedRuns))
	for i, run := range selectedRuns {
		runIDs[i] = run.RunID
	}
	metrics, err := analytics.sqlCollectExecutionMetrics(ctx, runIDs)
	if err != nil {
		return nil, newAnalyticsError(AnalyticsStageAggregateExecution, err)
	}
	start, _ := time.Parse(time.RFC3339, metrics.StartedAt)
	end, _ := time.Parse(time.RFC3339, metrics.EndedAt)
	if err := analytics.sqlCollectAuditMetrics(ctx, teamName, start, end, &metrics); err != nil {
		return nil, newAnalyticsError(AnalyticsStageAggregateExecution, err)
	}

	projectionByRun, err := analytics.sqlSelectedExecutionProjection(ctx, runIDs)
	if err != nil {
		return nil, newAnalyticsError(AnalyticsStageAggregateExecution, err)
	}
	selectedProjection := make([]team.ExecutionEvent, 0)
	for _, runID := range runIDs {
		selectedProjection = append(selectedProjection, projectionByRun[runID]...)
	}
	if err := analytics.sqlCollectMemoryMetrics(ctx, runIDs, &metrics); err != nil {
		return nil, newAnalyticsError(AnalyticsStageAggregateMemory, err)
	}

	trend := make([]TrendPoint, 0, len(selectedRuns))
	teamRevisions := uniqueTeamRevisions(selectedProjection)
	for _, run := range selectedRuns {
		runMetrics, err := analytics.sqlCollectExecutionMetrics(ctx, []string{run.RunID})
		if err != nil {
			return nil, newAnalyticsError(AnalyticsStageAggregateExecution, err)
		}
		runStart, _ := time.Parse(time.RFC3339, runMetrics.StartedAt)
		runEnd, _ := time.Parse(time.RFC3339, runMetrics.EndedAt)
		if err := analytics.sqlCollectAuditMetrics(ctx, teamName, runStart, runEnd, &runMetrics); err != nil {
			return nil, newAnalyticsError(AnalyticsStageAggregateExecution, err)
		}
		if err := analytics.sqlCollectMemoryMetrics(ctx, []string{run.RunID}, &runMetrics); err != nil {
			return nil, newAnalyticsError(AnalyticsStageAggregateMemory, err)
		}
		revision := latestTeamRevision(projectionByRun[run.RunID])
		trend = append(trend, TrendPoint{
			RunID: run.RunID, StartedAt: unixNSToTime(run.StartUnixNS).Format(time.RFC3339), EndedAt: unixNSToTime(run.EndUnixNS).Format(time.RFC3339), TeamRevision: revision, Metrics: runMetrics,
		})
	}
	if len(teamRevisions) == 0 {
		if revision := definitionRevision(teamDir); revision != "" {
			teamRevisions = []string{revision}
		}
	}
	groups, err := analytics.sqlCollectGroupedMetrics(ctx, runIDs)
	if err != nil {
		return nil, newAnalyticsError(AnalyticsStageAggregateGroups, err)
	}
	provenance := findingProvenance{runIDs: runIDs, teamRevisions: teamRevisions}
	report := &Report{
		Team:          teamName,
		Workspace:     workspace,
		GeneratedAt:   time.Now().UTC().Format(time.RFC3339),
		Source:        "hufu improve",
		RunIDs:        runIDs,
		TeamRevisions: teamRevisions,
		Metrics:       metrics,
		Trend:         trend,
		Groups:        groups,
		Findings:      analyze(def, metrics, provenance),
	}
	return report, nil
}

func uniqueTeamRevisions(events []team.ExecutionEvent) []string {
	seen := make(map[string]struct{})
	revisions := make([]string, 0)
	for _, event := range events {
		if event.TeamRevision == "" {
			continue
		}
		if _, ok := seen[event.TeamRevision]; ok {
			continue
		}
		seen[event.TeamRevision] = struct{}{}
		revisions = append(revisions, event.TeamRevision)
	}
	sort.Strings(revisions)
	return revisions
}

func latestTeamRevision(events []team.ExecutionEvent) string {
	for i := len(events) - 1; i >= 0; i-- {
		if events[i].TeamRevision != "" {
			return events[i].TeamRevision
		}
	}
	return ""
}

// definitionRevision matches the metadata-only revision written by new
// telemetry. It is used as a best-effort fallback for legacy event files.
func definitionRevision(teamDir string) string {
	entries, err := os.ReadDir(teamDir)
	if err != nil {
		return ""
	}
	files := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if name == "team.yaml" || name == "team.yml" || strings.HasSuffix(name, ".md") {
			files = append(files, name)
		}
	}
	if len(files) == 0 {
		return ""
	}
	sort.Strings(files)
	hash := sha256.New()
	for _, name := range files {
		data, err := os.ReadFile(filepath.Join(teamDir, name))
		if err != nil {
			continue
		}
		_, _ = hash.Write([]byte(name))
		_, _ = hash.Write([]byte{0})
		_, _ = hash.Write(data)
		_, _ = hash.Write([]byte{0})
	}
	return fmt.Sprintf("%x", hash.Sum(nil))
}

func readTeamDefinition(dir string) (TeamDefinition, error) {
	def := TeamDefinition{}
	for _, filename := range []string{"team.yaml", "team.yml"} {
		data, err := os.ReadFile(filepath.Join(dir, filename))
		if err != nil {
			continue
		}
		var cfg teamYAML
		if err := yaml.Unmarshal(data, &cfg); err != nil {
			return def, fmt.Errorf("parse %s: %w", filename, err)
		}
		def.Name, def.MaxRounds = cfg.Name, cfg.MaxRounds
		break
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return def, fmt.Errorf("read team definition: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".md") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, entry.Name()))
		if err != nil {
			continue
		}
		front, body := splitFrontmatter(string(data))
		var fm agentFrontmatter
		if front != "" && yaml.Unmarshal([]byte(front), &fm) != nil {
			continue
		}
		name := fm.Name
		if name == "" {
			name = strings.TrimSuffix(entry.Name(), ".md")
		}
		def.Agents = append(def.Agents, AgentDefinition{Name: name, Description: fm.Description, Tools: stringsFromYAML(fm.Tools), Guard: fm.Guard, Body: strings.TrimSpace(body)})
	}
	sort.Slice(def.Agents, func(i, j int) bool { return def.Agents[i].Name < def.Agents[j].Name })
	return def, nil
}

func splitFrontmatter(content string) (front, body string) {
	if !strings.HasPrefix(content, "---\n") {
		return "", content
	}
	rest := content[4:]
	if idx := strings.Index(rest, "\n---\n"); idx >= 0 {
		return rest[:idx], rest[idx+5:]
	}
	return "", content
}

func stringsFromYAML(raw interface{}) []string {
	switch value := raw.(type) {
	case string:
		return splitCSV(value)
	case []interface{}:
		out := make([]string, 0, len(value))
		for _, item := range value {
			if text, ok := item.(string); ok {
				out = append(out, strings.TrimSpace(text))
			}
		}
		return out
	case []string:
		return value
	default:
		return nil
	}
}

func splitCSV(value string) []string {
	var result []string
	for _, part := range strings.Split(value, ",") {
		if part = strings.TrimSpace(part); part != "" {
			result = append(result, part)
		}
	}
	return result
}

type findingProvenance struct {
	runIDs        []string
	teamRevisions []string
}

func (p findingProvenance) attach(finding Finding) Finding {
	finding.RunIDs = append([]string(nil), p.runIDs...)
	finding.TeamRevisions = append([]string(nil), p.teamRevisions...)
	parts := make([]string, 0, 3)
	if finding.Evidence != "" {
		parts = append(parts, finding.Evidence)
	}
	if len(p.runIDs) > 0 {
		parts = append(parts, "Runs: "+strings.Join(p.runIDs, ", "))
	}
	if len(p.teamRevisions) > 0 {
		parts = append(parts, "Team revisions: "+strings.Join(p.teamRevisions, ", "))
	}
	finding.Evidence = strings.Join(parts, " ")
	return finding
}

func analyze(def TeamDefinition, metrics Metrics, provenance findingProvenance) []Finding {
	findings := make([]Finding, 0)
	for _, agent := range def.Agents {
		if len([]rune(agent.Body)) < 200 {
			findings = append(findings, provenance.attach(Finding{Layer: "agent", Target: agent.Name, Severity: "warning", Category: "prompt", Metric: "prompt_length", Value: fmt.Sprintf("%d chars", len([]rune(agent.Body))), Suggestion: "Expand the agent instructions with scope, expected deliverables, and completion criteria.", SourceRule: "agent_prompt_short", Evidence: "Prompt body is shorter than 200 characters.", Confidence: "high"}))
		}
		if hasSensitiveTool(agent.Tools) && len(agent.Guard) == 0 {
			findings = append(findings, provenance.attach(Finding{Layer: "agent", Target: agent.Name, Severity: "suggestion", Category: "guard", Metric: "sensitive_tools_without_guard", Value: strings.Join(agent.Tools, ", "), Suggestion: "Add guards only for concrete risks this agent must avoid; do not add a blanket guard without an enforceable policy.", SourceRule: "guard_missing", Evidence: "Agent exposes bash, sudo, or ssh without agent-specific guard rules.", Confidence: "medium"}))
		}
		if metrics.ToolErrorsByAgent[agent.Name] >= 3 {
			findings = append(findings, provenance.attach(Finding{Layer: "agent", Target: agent.Name, Severity: "warning", Category: "tools", Metric: "tool_errors", Value: fmt.Sprintf("%d", metrics.ToolErrorsByAgent[agent.Name]), Suggestion: "Inspect the failing tool configuration and prompt the agent to use the supported tool and argument shape.", SourceRule: "tool_error_high", Evidence: "At least three audited tool errors occurred in the selected runs.", Confidence: "high"}))
		}
	}
	if metrics.TotalTasks > 0 && metrics.RetriedTasks > 0 {
		rate := float64(metrics.RetriedTasks) / float64(metrics.TotalTasks)
		severity := "warning"
		if rate >= 0.5 {
			severity = "critical"
		}
		findings = append(findings, provenance.attach(Finding{Layer: "team", Target: def.Name, Severity: severity, Category: "prompt", Metric: "retried_tasks", Value: fmt.Sprintf("%d/%d (%.0f%%)", metrics.RetriedTasks, metrics.TotalTasks, rate*100), Suggestion: "Review the failed task goals and verification criteria; make task inputs and expected deliverables more specific before increasing retry budgets.", SourceRule: "retry_rate", Evidence: "Attempt-level execution events show one or more tasks required a retry.", Confidence: "high"}))
	}
	return findings
}

func hasSensitiveTool(tools []string) bool {
	for _, tool := range tools {
		if tool == "bash" || tool == "sudo" || tool == "ssh" {
			return true
		}
	}
	return false
}
