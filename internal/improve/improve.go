// Package improve turns durable execution telemetry into a deterministic,
// shareable improvement report. It intentionally never sends workspace data to
// an LLM and never includes prompt, output, or tool-argument content.
package improve

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/anomalyco/hufu/internal/team"

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
	RunID             string         `json:"run_id"`
	StartedAt         string         `json:"started_at"`
	EndedAt           string         `json:"ended_at"`
	TotalTasks        int            `json:"total_tasks"`
	Done              int            `json:"done"`
	Error             int            `json:"error"`
	Planned           int            `json:"planned"`
	TotalAttempts     int            `json:"total_attempts"`
	RetriedTasks      int            `json:"retried_tasks"`
	TokensByAgent     map[string]int `json:"tokens_by_agent"`
	ToolCallsByAgent  map[string]int `json:"tool_calls_by_agent"`
	ToolErrorsByAgent map[string]int `json:"tool_errors_by_agent"`
}

type Finding struct {
	Layer      string `json:"layer"`
	Target     string `json:"target"`
	Severity   string `json:"severity"`
	Category   string `json:"category"`
	Metric     string `json:"metric"`
	Value      string `json:"value"`
	Suggestion string `json:"suggestion"`
	SourceRule string `json:"source_rule"`
	Evidence   string `json:"evidence"`
	Confidence string `json:"confidence"`
}

type Report struct {
	Team        string    `json:"team"`
	Workspace   string    `json:"workspace"`
	GeneratedAt string    `json:"generated_at"`
	Source      string    `json:"source"`
	Metrics     Metrics   `json:"metrics"`
	Findings    []Finding `json:"findings"`
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
	return events[len(events)-1].Team, nil
}

// Analyze produces a deterministic report for the newest run in workspace.
// teamDir is read directly and does not load a TeamSession or create folders.
func Analyze(workspace, teamName, teamDir string) (*Report, error) {
	events, err := readEvents(workspace)
	if err != nil {
		return nil, err
	}
	if len(events) == 0 {
		return nil, ErrNoExecutionData
	}

	runID := events[len(events)-1].RunID
	selected := make([]team.ExecutionEvent, 0)
	for _, event := range events {
		if event.RunID == runID {
			selected = append(selected, event)
		}
	}
	if len(selected) == 0 {
		return nil, ErrNoExecutionData
	}
	if teamName == "" {
		teamName = selected[len(selected)-1].Team
	}
	def, err := readTeamDefinition(teamDir)
	if err != nil {
		return nil, err
	}
	if def.Name == "" {
		def.Name = teamName
	}
	metrics := collectMetrics(workspace, teamName, selected)
	report := &Report{
		Team:        teamName,
		Workspace:   workspace,
		GeneratedAt: time.Now().UTC().Format(time.RFC3339),
		Source:      "hufu improve",
		Metrics:     metrics,
		Findings:    analyze(def, metrics),
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
	metrics := Metrics{RunID: events[0].RunID, TokensByAgent: map[string]int{}, ToolCallsByAgent: map[string]int{}, ToolErrorsByAgent: map[string]int{}}
	tasks := map[string]string{}
	attempts := map[string]int{}
	terminal := map[string]string{}
	start, _ := time.Parse(time.RFC3339Nano, events[0].Timestamp)
	end := start
	for _, event := range events {
		if event.TaskID == "" {
			continue
		}
		tasks[event.TaskID] = event.Agent
		if event.Attempt > attempts[event.TaskID] {
			attempts[event.TaskID] = event.Attempt
		}
		if event.Status == "done" || event.Status == "error" || event.Status == "planned" {
			terminal[event.TaskID] = event.Status
		}
		metrics.TokensByAgent[event.Agent] += event.Usage.TotalTokens
		if event.Status == "in_progress" {
			metrics.TotalAttempts++
		}
		if ts, err := time.Parse(time.RFC3339Nano, event.Timestamp); err == nil {
			if start.IsZero() || ts.Before(start) {
				start = ts
			}
			if ts.After(end) {
				end = ts
			}
		}
	}
	metrics.StartedAt, metrics.EndedAt = start.Format(time.RFC3339), end.Format(time.RFC3339)
	metrics.TotalTasks = len(tasks)
	for taskID := range tasks {
		if attempts[taskID] > 1 {
			metrics.RetriedTasks++
		}
		switch terminal[taskID] {
		case "done":
			metrics.Done++
		case "error":
			metrics.Error++
		case "planned":
			metrics.Planned++
		}
	}
	collectAuditMetrics(filepath.Join(workspace, "logs", "audit"), teamName, start, end, &metrics)
	return metrics
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
			case "tool_error":
				metrics.ToolErrorsByAgent[event.Agent]++
			}
		}
		_ = f.Close()
	}
}

func analyze(def TeamDefinition, metrics Metrics) []Finding {
	findings := make([]Finding, 0)
	for _, agent := range def.Agents {
		if len([]rune(agent.Body)) < 200 {
			findings = append(findings, Finding{"agent", agent.Name, "warning", "prompt", "prompt_length", fmt.Sprintf("%d chars", len([]rune(agent.Body))), "Expand the agent instructions with scope, expected deliverables, and completion criteria.", "agent_prompt_short", "Prompt body is shorter than 200 characters.", "high"})
		}
		if hasSensitiveTool(agent.Tools) && len(agent.Guard) == 0 {
			findings = append(findings, Finding{"agent", agent.Name, "suggestion", "guard", "sensitive_tools_without_guard", strings.Join(agent.Tools, ", "), "Add guards only for concrete risks this agent must avoid; do not add a blanket guard without an enforceable policy.", "guard_missing", "Agent exposes bash, sudo, or ssh without agent-specific guard rules.", "medium"})
		}
		if metrics.ToolErrorsByAgent[agent.Name] >= 3 {
			findings = append(findings, Finding{"agent", agent.Name, "warning", "tools", "tool_errors", fmt.Sprintf("%d", metrics.ToolErrorsByAgent[agent.Name]), "Inspect the failing tool configuration and prompt the agent to use the supported tool and argument shape.", "tool_error_high", "At least three audited tool errors occurred in this run.", "high"})
		}
	}
	if metrics.TotalTasks > 0 && metrics.RetriedTasks > 0 {
		rate := float64(metrics.RetriedTasks) / float64(metrics.TotalTasks)
		severity := "warning"
		if rate >= 0.5 {
			severity = "critical"
		}
		findings = append(findings, Finding{"team", def.Name, severity, "prompt", "retried_tasks", fmt.Sprintf("%d/%d (%.0f%%)", metrics.RetriedTasks, metrics.TotalTasks, rate*100), "Review the failed task goals and verification criteria; make task inputs and expected deliverables more specific before increasing retry budgets.", "retry_rate", "Attempt-level execution events show one or more tasks required a retry.", "high"})
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
