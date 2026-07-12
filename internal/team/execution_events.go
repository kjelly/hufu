package team

// Durable, structured execution telemetry for post-run diagnostics. Events are
// deliberately metadata-only: task output, prompts, tool arguments, and error
// text are not persisted here so reports can be safely shared by default.

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"charm.land/fantasy"
)

const executionEventsFile = "execution-events.jsonl"

// ExecutionUsage is the provider-reported LLM usage for a single attempt.
// A zero value means the provider did not report usage.
type ExecutionUsage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
	TotalTokens  int `json:"total_tokens"`
}

// ExecutionEvent records one attempt lifecycle transition. TaskID and Attempt
// identify the unit of work; RunID separates successive invocations that share
// a workspace.
type ExecutionEvent struct {
	Version      int            `json:"version"`
	Timestamp    string         `json:"timestamp"`
	RunID        string         `json:"run_id"`
	Team         string         `json:"team"`
	TaskID       string         `json:"task_id"`
	Agent        string         `json:"agent"`
	Attempt      int            `json:"attempt"`
	Status       string         `json:"status"`
	Model        string         `json:"model,omitempty"`
	TaskType     string         `json:"task_type,omitempty"`
	Skills       []string       `json:"skills,omitempty"`
	TeamRevision string         `json:"team_revision,omitempty"`
	DurationMS   int64          `json:"duration_ms,omitempty"`
	Usage        ExecutionUsage `json:"usage"`
}

type executionEventLogger struct {
	mu sync.Mutex
	f  *os.File
}

func newExecutionEventLogger(workspace string) (*executionEventLogger, error) {
	path := filepath.Join(workspace, logsDir, executionEventsFile)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, fmt.Errorf("create execution event directory: %w", err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open execution events: %w", err)
	}
	return &executionEventLogger{f: f}, nil
}

func (l *executionEventLogger) append(event ExecutionEvent) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f == nil {
		return nil
	}
	_, err = l.f.Write(append(data, '\n'))
	return err
}

func (l *executionEventLogger) close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.f != nil {
		_ = l.f.Close()
		l.f = nil
	}
}

func newExecutionRunID() string {
	buf := make([]byte, 6)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("run-%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("run-%s-%s", time.Now().UTC().Format("20060102T150405.000000000Z"), hex.EncodeToString(buf))
}

func usageFromSteps(steps []fantasy.StepResult) ExecutionUsage {
	var usage ExecutionUsage
	for _, step := range steps {
		usage.InputTokens += int(step.Usage.InputTokens)
		usage.OutputTokens += int(step.Usage.OutputTokens)
		usage.TotalTokens += int(step.Usage.TotalTokens)
	}
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.InputTokens + usage.OutputTokens
	}
	return usage
}

func (c *Coordinator) beginExecutionRun() func() {
	logger, err := newExecutionEventLogger(c.session.Workspace)
	if err != nil {
		return func() {}
	}
	runID := newExecutionRunID()
	teamRevision := teamDefinitionRevision(c.session.Dir)
	c.executionEventsMu.Lock()
	previous := c.executionEvents
	c.executionEvents = logger
	c.executionRunID = runID
	c.executionTeamRevision = teamRevision
	c.executionEventsMu.Unlock()
	if previous != nil {
		previous.close()
	}
	return func() {
		c.executionEventsMu.Lock()
		if c.executionEvents == logger {
			c.executionEvents = nil
			c.executionRunID = ""
			c.executionTeamRevision = ""
		}
		c.executionEventsMu.Unlock()
		logger.close()
	}
}

func (c *Coordinator) recordExecutionEvent(taskID, agent string, attempt int, status, model string, duration time.Duration, usage ExecutionUsage) {
	c.executionEventsMu.RLock()
	logger, runID, teamRevision := c.executionEvents, c.executionRunID, c.executionTeamRevision
	c.executionEventsMu.RUnlock()
	if logger == nil || runID == "" || taskID == "" || attempt < 1 {
		return
	}
	taskType, skills := c.taskTracker.TodoList().ExecutionMetadata(taskID)
	_ = logger.append(ExecutionEvent{
		Version:      2,
		Timestamp:    time.Now().UTC().Format(time.RFC3339Nano),
		RunID:        runID,
		Team:         c.session.Config.Name,
		TaskID:       taskID,
		Agent:        agent,
		Attempt:      attempt,
		Status:       status,
		Model:        model,
		TaskType:     taskType,
		Skills:       skills,
		TeamRevision: teamRevision,
		DurationMS:   duration.Milliseconds(),
		Usage:        usage,
	})
}

// teamDefinitionRevision hashes only team configuration and agent definition
// files. It provides a stable, metadata-only revision for telemetry even when
// the team directory is not inside a Git worktree.
func teamDefinitionRevision(teamDir string) string {
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
	if len(files) == 0 {
		return ""
	}
	return hex.EncodeToString(hash.Sum(nil))
}
