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
	PromptCacheReadTokens     int            `json:"prompt_cache_read_tokens"`
	PromptCacheCreationTokens int            `json:"prompt_cache_creation_tokens"`
	PromptCacheHitRate        float64        `json:"prompt_cache_hit_rate"`
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
	Team                 string         `json:"team"`
	Workspace            string         `json:"workspace"`
	GeneratedAt          string         `json:"generated_at"`
	Source               string         `json:"source"`
	RunIDs               []string       `json:"run_ids"`
	TeamRevisions        []string       `json:"team_revisions,omitempty"`
	MemoryPolicyVersions []string       `json:"memory_policy_versions,omitempty"`
	AppliedContextRefs   []ArtifactRef  `json:"applied_context_refs,omitempty"`
	Metrics              Metrics        `json:"metrics"`
	Trend                []TrendPoint   `json:"trend"`
	Groups               GroupedMetrics `json:"groups"`
	Findings             []Finding      `json:"findings"`
	// PromotedSkills is an association report for skills created by applied
	// LTM promotions, not a causal attribution.
	PromotedSkills []PromotedSkillUsage `json:"promoted_skills,omitempty"`
	// PromotedSkillsUnavailable is a reason code (query_failed) when the
	// context store exists but promoted skills could not be read.
	PromotedSkillsUnavailable string `json:"promoted_skills_unavailable,omitempty"`
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
	return analyzeRecent(context.Background(), workspace, teamName, teamDir, runCount, nil)
}

func analyzeRecent(ctx context.Context, workspace, teamName, teamDir string, runCount int, diagnostics *AnalyticsDiagnostics) (*Report, error) {
	startedTotal := time.Now()
	diagnostics.reset()
	defer diagnostics.record(diagnosticTotal, startedTotal)
	if runCount < 1 {
		return nil, fmt.Errorf("run count must be at least 1")
	}
	started := time.Now()
	analytics, err := openSQLiteAnalyticsSession(ctx)
	diagnostics.record(diagnosticOpen, started)
	if err != nil {
		return nil, err
	}
	analytics.diagnostics = diagnostics
	defer func() { _ = analytics.Close() }()
	started = time.Now()
	executionStats, err := analytics.loadExecutionEvents(ctx, filepath.Join(workspace, eventsPath))
	diagnostics.record(diagnosticLoadExecution, started)
	if err != nil {
		return nil, newAnalyticsError(AnalyticsStageLoadExecution, err)
	}
	if diagnostics != nil {
		diagnostics.ExecutionLinesRead = executionStats.LinesRead
		diagnostics.ExecutionRows = executionStats.RowsLoaded
	}
	started = time.Now()
	auditStats, err := analytics.loadAuditEvents(ctx, filepath.Join(workspace, "logs", "audit"))
	diagnostics.record(diagnosticLoadAudit, started)
	if err != nil {
		return nil, newAnalyticsError(AnalyticsStageLoadAudit, err)
	}
	if diagnostics != nil {
		diagnostics.AuditLinesRead = auditStats.LinesRead
		diagnostics.AuditRows = auditStats.RowsLoaded
	}
	started = time.Now()
	memoryStats, err := analytics.loadMemoryEvents(ctx, workspace)
	diagnostics.record(diagnosticLoadMemory, started)
	if err != nil {
		return nil, newAnalyticsError(AnalyticsStageLoadMemory, err)
	}
	if diagnostics != nil {
		diagnostics.MemoryRows = memoryStats.RowsLoaded
	}
	started = time.Now()
	if err := analytics.createIndexes(ctx); err != nil {
		diagnostics.record(diagnosticBuildIndexes, started)
		return nil, newAnalyticsError(AnalyticsStageSchema, err)
	}
	diagnostics.record(diagnosticBuildIndexes, started)

	started = time.Now()
	teamName, selectedRuns, err := analytics.sqlSelectRecentRunSummaries(ctx, teamName, runCount)
	diagnostics.record(diagnosticSelectRuns, started)
	if err != nil {
		return nil, newAnalyticsError(AnalyticsStageSelectRuns, err)
	}
	if diagnostics != nil {
		diagnostics.SelectedRuns = len(selectedRuns)
	}
	if len(selectedRuns) == 0 {
		return nil, ErrNoExecutionData
	}
	if err := analytics.materializeTaskViews(ctx); err != nil {
		return nil, newAnalyticsError(AnalyticsStageAggregateExecution, err)
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
	started = time.Now()
	metrics, err := analytics.sqlCollectExecutionMetrics(ctx)
	diagnostics.record(diagnosticAggregateExecution, started)
	if err != nil {
		return nil, newAnalyticsError(AnalyticsStageAggregateExecution, err)
	}
	start, _ := time.Parse(time.RFC3339, metrics.StartedAt)
	end, _ := time.Parse(time.RFC3339, metrics.EndedAt)
	started = time.Now()
	if err := analytics.sqlCollectAuditMetrics(ctx, teamName, start, end, &metrics); err != nil {
		diagnostics.record(diagnosticAggregateAudit, started)
		return nil, newAnalyticsError(AnalyticsStageAggregateExecution, err)
	}
	diagnostics.record(diagnosticAggregateAudit, started)

	started = time.Now()
	teamRevisions, revisionsByOrdinal, err := analytics.sqlSelectedRevisions(ctx)
	diagnostics.record(diagnosticAggregateExecution, started)
	if err != nil {
		return nil, newAnalyticsError(AnalyticsStageAggregateExecution, err)
	}
	started = time.Now()
	memoryMetrics, err := analytics.sqlCollectMemoryAnalytics(ctx)
	if err != nil {
		diagnostics.record(diagnosticAggregateMemory, started)
		return nil, newAnalyticsError(AnalyticsStageAggregateMemory, err)
	}
	if err := memoryMetrics.apply(&metrics, allSelectedRunOrdinals); err != nil {
		diagnostics.record(diagnosticAggregateMemory, started)
		return nil, newAnalyticsError(AnalyticsStageAggregateMemory, err)
	}
	memoryPolicyVersions, appliedContextRefs, err := analytics.sqlMemoryEvidence(ctx)
	diagnostics.record(diagnosticAggregateMemory, started)
	if err != nil {
		return nil, newAnalyticsError(AnalyticsStageAggregateMemory, err)
	}

	started = time.Now()
	trend, err := analytics.sqlCollectTrend(ctx, memoryMetrics, revisionsByOrdinal)
	if err != nil {
		diagnostics.record(diagnosticAggregateTrend, started)
		return nil, err
	}
	diagnostics.record(diagnosticAggregateTrend, started)
	if len(teamRevisions) == 0 {
		if revision := definitionRevision(teamDir); revision != "" {
			teamRevisions = []string{revision}
		}
	}
	started = time.Now()
	groups, err := analytics.sqlCollectGroupedMetrics(ctx)
	diagnostics.record(diagnosticAggregateGroups, started)
	if err != nil {
		return nil, newAnalyticsError(AnalyticsStageAggregateGroups, err)
	}
	promotedSkills, promotedSkillsUnavailable := collectPromotedSkills(ctx, analytics, workspace, teamName)
	provenance := findingProvenance{runIDs: runIDs, teamRevisions: teamRevisions}
	report := &Report{
		Team:                 teamName,
		Workspace:            workspace,
		GeneratedAt:          time.Now().UTC().Format(time.RFC3339),
		Source:               "hufu improve",
		RunIDs:               runIDs,
		TeamRevisions:        teamRevisions,
		MemoryPolicyVersions: memoryPolicyVersions,
		AppliedContextRefs:   appliedContextRefs,
		Metrics:              metrics,
		Trend:                trend,
		Groups:               groups,
		Findings:             analyze(def, metrics, provenance),

		PromotedSkills:            promotedSkills,
		PromotedSkillsUnavailable: promotedSkillsUnavailable,
	}
	return report, nil
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
