// Package improve turns durable execution telemetry into a deterministic,
// shareable improvement report. It intentionally never sends workspace data to
// an LLM and never includes prompt, output, or tool-argument content.
package improve

import (
	"bufio"
	"crypto/sha256"
	"encoding/json"
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

type executionRun struct {
	ID     string
	Team   string
	Events []team.ExecutionEvent
	Start  time.Time
	End    time.Time
}

type taskSummary struct {
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
	events, err := readEvents(workspace)
	if err != nil {
		return "", err
	}
	if len(events) == 0 {
		return "", ErrNoExecutionData
	}
	teamName, runs := selectRecentRuns(events, "", 1)
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
	events, err := readEvents(workspace)
	if err != nil {
		return nil, err
	}
	if len(events) == 0 {
		return nil, ErrNoExecutionData
	}

	teamName, runs := selectRecentRuns(events, teamName, runCount)
	if len(runs) == 0 {
		return nil, ErrNoExecutionData
	}
	def, err := readTeamDefinition(teamDir)
	if err != nil {
		return nil, err
	}
	if def.Name == "" {
		def.Name = teamName
	}
	selected := flattenRuns(runs)
	metrics := collectMetrics(workspace, teamName, selected)
	runIDs := make([]string, 0, len(runs))
	trend := make([]TrendPoint, 0, len(runs))
	teamRevisions := uniqueTeamRevisions(selected)
	for _, run := range runs {
		runMetrics := collectMetrics(workspace, teamName, run.Events)
		revision := latestTeamRevision(run.Events)
		runIDs = append(runIDs, run.ID)
		trend = append(trend, TrendPoint{
			RunID: run.ID, StartedAt: run.Start.Format(time.RFC3339), EndedAt: run.End.Format(time.RFC3339), TeamRevision: revision, Metrics: runMetrics,
		})
	}
	if len(teamRevisions) == 0 {
		if revision := definitionRevision(teamDir); revision != "" {
			teamRevisions = []string{revision}
		}
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
		Groups:        collectGroupedMetrics(selected),
		Findings:      analyze(def, metrics, provenance),
	}
	return report, nil
}

func readEvents(workspace string) ([]team.ExecutionEvent, error) {
	f, err := os.Open(filepath.Join(workspace, eventsPath))
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

func selectRecentRuns(events []team.ExecutionEvent, teamName string, runCount int) (string, []executionRun) {
	runsByID := make(map[string]*executionRun)
	for _, event := range events {
		if event.RunID == "" || event.Team == "" {
			continue
		}
		run := runsByID[event.RunID]
		if run == nil {
			run = &executionRun{ID: event.RunID, Team: event.Team}
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
	runs := make([]executionRun, 0, len(runsByID))
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
	selected := make([]executionRun, 0, runCount)
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

func flattenRuns(runs []executionRun) []team.ExecutionEvent {
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

func collectMetrics(workspace, teamName string, events []team.ExecutionEvent) Metrics {
	metrics := collectExecutionMetrics(events)
	start, _ := time.Parse(time.RFC3339, metrics.StartedAt)
	end, _ := time.Parse(time.RFC3339, metrics.EndedAt)
	collectAuditMetrics(filepath.Join(workspace, "logs", "audit"), teamName, start, end, &metrics)
	collectMemoryMetrics(workspace, events, &metrics)
	return metrics
}

func collectMemoryMetrics(workspace string, executionEvents []team.ExecutionEvent, metrics *Metrics) {
	store, err := team.OpenEventStore(workspace)
	if err != nil {
		return
	}
	defer func() { _ = store.Close() }()
	events, err := store.ReadEvents()
	if err != nil {
		return
	}
	retrievals := map[string]bool{}
	appliedTasks := map[string]bool{}
	verified, harmful, stale, memoryTokens := 0, 0, 0, 0
	for _, event := range events {
		if event.Type != "memory_retrieved" && event.Type != "memory_usage_recorded" && event.Type != "memory_outcome_recorded" {
			continue
		}
		var payload map[string]any
		if json.Unmarshal(event.Payload, &payload) != nil {
			continue
		}
		if id, _ := payload["retrieval_id"].(string); id != "" {
			retrievals[id] = true
		}
		switch event.Type {
		case "memory_retrieved":
			metrics.MemoryExposureCount++
			if reason, _ := payload["reason_code"].(string); reason == "stale_environment" {
				stale++
			}
			if count, ok := payload["token_count"].(float64); ok && count > 0 {
				memoryTokens += int(count)
			}
		case "memory_usage_recorded":
			if payload["disposition"] == "applied" {
				metrics.MemoryAppliedCount++
				appliedTasks[event.RunID+"\x00"+event.TaskID] = true
			}
		case "memory_outcome_recorded":
			signal, _ := payload["signal"].(string)
			if signal == "verification_passed" {
				verified++
			}
			if direction, _ := payload["direction"].(string); direction == "negative" {
				harmful++
			}
			if signal == "stale_environment" {
				stale++
			}
		}
	}
	metrics.MemoryRetrievalCount = len(retrievals)
	if metrics.MemoryExposureCount > 0 {
		metrics.MemoryAttributionCoverage = float64(metrics.MemoryAppliedCount) / float64(metrics.MemoryExposureCount)
		metrics.MemoryStaleRetrievalRate = float64(stale) / float64(metrics.MemoryExposureCount)
	}
	if metrics.MemoryAppliedCount > 0 {
		metrics.MemoryVerifiedAssistRate = float64(verified) / float64(metrics.MemoryAppliedCount)
		metrics.MemoryHarmfulUseRate = float64(harmful) / float64(metrics.MemoryAppliedCount)
	}
	totalInputTokens := 0
	for _, event := range executionEvents {
		if event.Usage.InputTokens > 0 {
			totalInputTokens += event.Usage.InputTokens
		}
	}
	if totalInputTokens > 0 {
		metrics.MemoryTokenOverhead = float64(memoryTokens) / float64(totalInputTokens)
	}
	retriedAssisted, retriedUnassisted, assistedTasks, unassistedTasks := 0, 0, map[string]bool{}, map[string]bool{}
	for _, event := range executionEvents {
		key := event.RunID + "\x00" + event.TaskID
		if event.Attempt <= 1 || event.TaskID == "" {
			continue
		}
		if appliedTasks[key] {
			if !assistedTasks[key] {
				retriedAssisted++
				assistedTasks[key] = true
			}
		} else if !unassistedTasks[key] {
			retriedUnassisted++
			unassistedTasks[key] = true
		}
	}
	if len(appliedTasks) > 0 {
		metrics.MemoryAssistedRetryRate = float64(retriedAssisted) / float64(len(appliedTasks))
	}
	unassistedTotal := metrics.TotalTasks - len(appliedTasks)
	if unassistedTotal > 0 {
		metrics.MemoryUnassistedRetryRate = float64(retriedUnassisted) / float64(unassistedTotal)
	}
}

func collectExecutionMetrics(events []team.ExecutionEvent) Metrics {
	metrics := Metrics{TokensByAgent: map[string]int{}, ToolCallsByAgent: map[string]int{}, ToolErrorsByAgent: map[string]int{}}
	runIDs := make(map[string]struct{})
	tasks := summarizeTasks(events)
	start, end := eventWindow(events)
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

func eventWindow(events []team.ExecutionEvent) (time.Time, time.Time) {
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

func summarizeTasks(events []team.ExecutionEvent) []taskSummary {
	tasks := make(map[string]*taskSummary)
	for _, event := range events {
		if event.TaskID == "" {
			continue
		}
		key := event.RunID + "\x00" + event.TaskID
		task := tasks[key]
		if task == nil {
			task = &taskSummary{RunID: event.RunID, TaskID: event.TaskID}
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
			task.Skills = uniqueStrings(event.Skills)
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
	result := make([]taskSummary, 0, len(tasks))
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

func uniqueStrings(values []string) []string {
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

func collectAuditMetrics(dir, teamName string, start, end time.Time, metrics *Metrics) {
	files, err := filepath.Glob(filepath.Join(dir, "audit-*.jsonl"))
	if err != nil {
		return
	}
	for _, filename := range files {
		f, err := os.Open(filename)
		if err != nil {
			continue
		}
		scanner := bufio.NewScanner(f)
		for scanner.Scan() {
			var event auditEvent
			if json.Unmarshal(scanner.Bytes(), &event) != nil || event.Team != teamName {
				continue
			}
			ts, err := time.Parse(time.RFC3339Nano, event.Timestamp)
			if err != nil || ts.Before(start) || ts.After(end) {
				continue
			}
			switch event.Event {
			case "tool_call":
				metrics.ToolCallsByAgent[event.Agent]++
				metrics.ToolCalls++
			case "tool_error":
				metrics.ToolErrorsByAgent[event.Agent]++
				metrics.ToolErrors++
			}
		}
		_ = f.Close()
	}
}

func collectGroupedMetrics(events []team.ExecutionEvent) GroupedMetrics {
	tasks := summarizeTasks(events)
	byAgent := make(map[string]*GroupMetric)
	byTaskType := make(map[string]*GroupMetric)
	byModel := make(map[string]*GroupMetric)
	bySkill := make(map[string]*GroupMetric)
	for _, task := range tasks {
		addGroupMetric(byAgent, fallbackGroup(task.Agent, "unspecified"), task)
		addGroupMetric(byTaskType, fallbackGroup(task.TaskType, "legacy/unspecified"), task)
		addGroupMetric(byModel, fallbackGroup(task.Model, "unspecified"), task)
		if len(task.Skills) == 0 {
			addGroupMetric(bySkill, "none", task)
			continue
		}
		for _, skill := range task.Skills {
			addGroupMetric(bySkill, skill, task)
		}
	}
	return GroupedMetrics{
		ByAgent: groupMetricSlice(byAgent), ByTaskType: groupMetricSlice(byTaskType), ByModel: groupMetricSlice(byModel), BySkill: groupMetricSlice(bySkill),
	}
}

func fallbackGroup(value, fallback string) string {
	if strings.TrimSpace(value) == "" {
		return fallback
	}
	return value
}

func addGroupMetric(groups map[string]*GroupMetric, key string, task taskSummary) {
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

func groupMetricSlice(groups map[string]*GroupMetric) []GroupMetric {
	result := make([]GroupMetric, 0, len(groups))
	for _, group := range groups {
		result = append(result, *group)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].Key < result[j].Key })
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
