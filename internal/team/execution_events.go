package team

// Durable, structured execution telemetry for post-run diagnostics. Events are
// deliberately metadata-only: task output, prompts, tool arguments, and error
// text are not persisted here so reports can be safely shared by default.

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
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
	// ProgressTokens is the growth-only token count for this attempt — new
	// context plus generated output, the same accounting attemptBudget
	// already uses (see attempt_budget.go) — for feeding the run-level
	// no-progress budget specifically. TotalTokens sums every step's full
	// resent-conversation usage and is correct for cost/receipt reporting,
	// but summed across steps it double- and triple-counts the same resent
	// history and grows roughly quadratically with an attempt's step count;
	// recordExecutionEvent prefers this field for recordReliabilityUsage so
	// one long, legitimate task cannot look identical to unproven thrash.
	// Zero means "not computed for this call site" and TotalTokens is used
	// as the fallback, matching the pre-existing behavior.
	ProgressTokens int `json:"progress_tokens,omitempty"`
}

// ExecutionEvent records one attempt lifecycle transition. TaskID and Attempt
// identify the unit of work; RunID separates successive invocations that share
// a workspace.
type ExecutionEvent struct {
	Version              int             `json:"version"`
	Timestamp            string          `json:"timestamp"`
	RunID                string          `json:"run_id"`
	Team                 string          `json:"team"`
	TaskID               string          `json:"task_id"`
	Agent                string          `json:"agent"`
	Attempt              int             `json:"attempt"`
	Status               string          `json:"status"`
	Model                string          `json:"model,omitempty"`
	TaskType             string          `json:"task_type,omitempty"`
	Skills               []string        `json:"skills,omitempty"`
	TeamRevision         string          `json:"team_revision,omitempty"`
	DurationMS           int64           `json:"duration_ms,omitempty"`
	Usage                ExecutionUsage  `json:"usage"`
	Outcome              RunOutcome      `json:"outcome,omitempty"`
	StopReason           StopReason      `json:"stop_reason,omitempty"`
	AcceptanceState      AcceptanceState `json:"acceptance_state,omitempty"`
	EvidenceManifestHash string          `json:"evidence_manifest_hash,omitempty"`
	RepairAttempts       int             `json:"repair_attempts,omitempty"`
	DecisionChain        []string        `json:"decision_chain,omitempty"`
	PlanRevision         string          `json:"plan_revision,omitempty"`
	RepairCost           RepairCost      `json:"repair_cost,omitempty"`
	TerminalReason       string          `json:"terminal_reason,omitempty"`
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
		total := step.Usage.TotalTokens
		if total == 0 {
			// Keep reliability accounting aligned with addStepTokens: providers
			// that omit usage still contribute a conservative message-size
			// estimate to the receipt-backed no-progress budget.
			estimatedChars := 0
			for _, message := range step.Messages {
				estimatedChars += messageTextSize(message)
			}
			total = int64(estimatedChars / 4)
		}
		usage.TotalTokens += int(total)
	}
	if usage.TotalTokens == 0 {
		usage.TotalTokens = usage.InputTokens + usage.OutputTokens
	}
	return usage
}

// usageWithProgressTokens augments usageFromSteps(steps) with a
// ProgressTokens snapshot from this attempt's live attemptBudget. TotalTokens
// sums every step's full resent-conversation usage, which is correct for
// cost/receipt reporting but, summed across an attempt's steps, charges the
// same resent history once per step; a long attempt's total then grows
// roughly with the square of its step count rather than with the content it
// actually added. attemptTokens already tracks growth-only usage for this
// exact attempt (see attempt_budget.go), so reusing its snapshot here — rather
// than re-deriving a second estimate from steps — keeps the no-progress
// budget's accounting identical to the per-attempt guard's, instead of a
// second, differently-biased implementation of the same idea. A nil
// attemptTokens (guard disabled, MaxTokensPerAttempt <= 0) or a zero
// snapshot leaves TotalTokens as the fallback via recordExecutionEvent.
func usageWithProgressTokens(steps []fantasy.StepResult, attemptTokens *attemptBudget) ExecutionUsage {
	usage := usageFromSteps(steps)
	if used, _ := attemptTokens.snapshot(); used > 0 {
		usage.ProgressTokens = int(used)
	}
	return usage
}

func (c *Coordinator) beginExecutionRun() func() {
	// A resumed coordinator may hold the previous run's result in memory. It
	// must never leak that completed outcome into this run's run_finished event
	// when the new run stops during acceptance or before finish.
	c.lastRunResultMu.Lock()
	c.lastRunResult = nil
	c.lastRunResultMu.Unlock()
	c.lastEvidenceManifestMu.Lock()
	c.lastEvidenceManifest = nil
	c.lastEvidenceManifestMu.Unlock()
	if c.sessionData != nil {
		c.sessionData.RunResult = nil
		if c.session != nil && c.session.Workspace != "" {
			_ = SaveSession(c.session.Workspace, c.sessionData)
		}
	}
	c.metricsMu.Lock()
	c.antiThrashing.reset()
	c.tokensSinceCriterionProgress = 0
	c.turnsSinceCriterionProgress = 0
	c.tasksSinceCriterionProgress = 0
	c.noProgressReplanTripped = false
	c.noProgressStopTripped = false
	c.reliabilityUsageByAttempt = make(map[string]int)
	c.metricsMu.Unlock()
	c.rebuildAntiThrashingState()
	runID := newExecutionRunID()
	teamRevision := ""
	if c.session != nil {
		teamRevision = teamDefinitionRevision(c.session.Dir)
	}

	c.executionEventsMu.Lock()
	c.executionRunID = runID
	c.executionTeamRevision = teamRevision
	if c.taskTracker != nil {
		c.taskTracker.TodoList().SetRunID(runID)
	}
	c.executionEventsMu.Unlock()

	workspace := ""
	if c.session != nil {
		workspace = c.session.Workspace
	}
	logger, err := newExecutionEventLogger(workspace)
	if err != nil {
		log.Printf("warning: create execution event logger: %v", err)
		return func() {}
	}

	c.executionEventsMu.Lock()
	previous := c.executionEvents
	c.executionEvents = logger
	c.executionEventsMu.Unlock()
	if previous != nil {
		previous.close()
	}

	c.initEventStore()
	teamName := ""
	if c.session != nil {
		teamName = c.session.Config.Name
	}
	c.emitEvent("run_started", "coordinator", "", map[string]interface{}{
		"team": teamName,
	})

	return func() {
		// Every terminal result, including no-progress and coordinator-fallback
		// exits, must carry the final evidence chain before run_finished is
		// published. The finish tool also performs this work earlier so policy
		// failures can be surfaced interactively; this deferred gate covers all
		// non-tool terminal paths.
		if result := c.LastRunResult(); result != nil {
			if err := c.finalizeEvidenceManifest(context.Background(), result.Acceptance); err != nil {
				log.Printf("warning: final evidence manifest before run_finished failed: %v", err)
			} else {
				c.lastEvidenceManifestMu.RLock()
				result.EvidenceManifest = c.lastEvidenceManifest
				c.lastEvidenceManifestMu.RUnlock()
				c.SetLastRunResult(result)
			}
		}
		if result := c.LastRunResult(); result != nil {
			if evalReport, err := c.PersistReliabilityEvaluation(result); err != nil {
				log.Printf("warning: persist reliability evaluation failed: %v", err)
				// Reliability-report read/write failures are terminal safety
				// failures too. Publishing a completed result without a valid
				// historical metrics report would make the run unverifiable.
				downgradeReliabilityResultForError(result, err)
				c.SetLastRunResult(result)
			} else {
				// Keep the quantitative production metrics visible in the durable
				// event-store terminal record as well as in the JSON report.
				_ = c.emitEvent("reliability_eval", "coordinator", "", map[string]interface{}{
					"metrics":                evalReport.Metrics,
					"production_observation": evalReport.ProductionObservation,
				})
			}
		}
		c.recordRunTelemetry(c.LastRunResult())
		payload := map[string]interface{}{}
		if result := c.LastRunResult(); result != nil {
			payload["outcome"] = result.Outcome
			payload["goal_satisfied"] = result.GoalSatisfied
			if result.Acceptance != nil {
				payload["acceptance_state"] = result.Acceptance.EffectiveState()
				payload["acceptance_passed"] = result.Acceptance.IsPassed()
				payload["acceptance"] = result.Acceptance
			}
			payload["stats"] = result.Stats
			payload["metrics"] = result.Metrics
			payload["telemetry"] = result.Telemetry
			if result.EvidenceManifest != nil {
				payload["evidence_manifest"] = result.EvidenceManifest
			}
		} else {
			payload["outcome"] = string(RunOutcomeFailed)
			payload["goal_satisfied"] = false
		}
		if err := c.emitEvent("run_finished", "coordinator", "", payload); err != nil {
			log.Printf("error: failed to write run_finished event: %v", err)
		}
		if c.eventStore != nil {
			_ = c.eventStore.Close()
			c.eventStore = nil
		}
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

func (c *Coordinator) recordRunTelemetry(result *RunResult) {
	if c == nil || result == nil {
		return
	}
	telemetry := c.buildRunTelemetry(result)
	result.Telemetry = &telemetry
	c.SetLastRunResult(result)
	c.executionEventsMu.RLock()
	logger, runID, revision := c.executionEvents, c.executionRunID, c.executionTeamRevision
	c.executionEventsMu.RUnlock()
	if logger == nil || runID == "" {
		return
	}
	state := AcceptanceNotConfigured
	if result.Acceptance != nil {
		state = result.Acceptance.EffectiveState()
	}
	hash := ""
	if result.EvidenceManifest != nil {
		hash = result.EvidenceManifest.ManifestHash
	}
	_ = logger.append(ExecutionEvent{
		Version: 3, Timestamp: time.Now().UTC().Format(time.RFC3339Nano), RunID: runID,
		Team: c.session.Config.Name, Status: "run_finished", TeamRevision: revision,
		Outcome: result.Outcome, StopReason: result.StopReason, AcceptanceState: state,
		EvidenceManifestHash: hash, RepairAttempts: telemetry.RepairCost.Attempts,
		DecisionChain: telemetry.DecisionChain, PlanRevision: telemetry.PlanRevision,
		RepairCost:     telemetry.RepairCost,
		TerminalReason: string(result.StopReason),
	})
}

func (c *Coordinator) buildRunTelemetry(result *RunResult) RunTelemetry {
	telemetry := RunTelemetry{TerminalReason: string(result.StopReason)}
	if result.EvidenceManifest != nil {
		telemetry.EvidenceManifest = result.EvidenceManifest.ManifestHash
	}
	repairs := result.Metrics.ProtocolRepairsAttempted
	for _, count := range result.Metrics.RepairAttemptsByCriterion {
		repairs += count
	}
	telemetry.RepairCost = RepairCost{
		Attempts:    repairs,
		Tokens:      result.Metrics.TokensSinceCriterionProgress,
		WallClockMS: result.Metrics.TimeSinceCriterionProgressSeconds * 1000,
	}
	if revisions := c.PlanRevisions(); len(revisions) > 0 {
		telemetry.PlanRevision = revisions[len(revisions)-1].ID
	}
	telemetry.DecisionChain = []string{
		"outcome:" + string(result.Outcome),
		"goal_satisfied:" + fmt.Sprint(result.GoalSatisfied),
		"acceptance:" + acceptanceState(result),
		"evidence:" + evidenceState(result),
		"terminal:" + string(result.StopReason),
	}
	for class, count := range result.Metrics.FailuresByClass {
		telemetry.DecisionChain = append(telemetry.DecisionChain, fmt.Sprintf("failure:%s=%d", class, count))
	}
	// Step exhaustion is reported alongside the failure classes it hides inside.
	// A chain showing failure:protocol=15 alone points at the model; the same
	// chain with budget_exhausted=9 points at the budget.
	if n := result.Metrics.StepBudgetExhaustions; n > 0 {
		telemetry.DecisionChain = append(telemetry.DecisionChain, fmt.Sprintf("budget_exhausted=%d", n))
	}
	sort.Strings(telemetry.DecisionChain[5:])
	return telemetry
}

func acceptanceState(result *RunResult) string {
	if result == nil || result.Acceptance == nil {
		return string(AcceptanceNotConfigured)
	}
	return string(result.Acceptance.EffectiveState())
}

func evidenceState(result *RunResult) string {
	if result != nil && result.EvidenceManifest != nil && result.EvidenceManifest.Status == "accepted" && result.EvidenceManifest.ManifestHash != "" {
		return "complete"
	}
	return "incomplete"
}

func (c *Coordinator) recordExecutionEvent(taskID, agent string, attempt int, status, model string, duration time.Duration, usage ExecutionUsage) {
	if progressTokens := usage.ProgressTokens; progressTokens > 0 {
		c.recordReliabilityUsage(taskID, attempt, progressTokens)
	} else if usage.TotalTokens > 0 {
		c.recordReliabilityUsage(taskID, attempt, usage.TotalTokens)
	}
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
