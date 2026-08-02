package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/anomalyco/hufu/internal/team"
	"github.com/anomalyco/hufu/internal/tools"
)

// runFixMode analyzes previous execution data and suggests improvements
// without running any agents. Returns nil on success.
func runFixMode(ctx context.Context, prompt string, fixQuestion string, registry *team.TeamRegistry, defaultProviderURL, defaultProviderAPIKey string, pathConsent *tools.PathConsent, vars map[string]string, forcedSkills []string, planMode bool, autoSkills bool) error {
	initialTeam := ""
	initialSegments, err := team.ParsePromptWithLazyAgents(prompt, registry, initialTeam)
	if err != nil {
		return fmt.Errorf("failed to parse prompt: %w", err)
	}

	for _, seg := range initialSegments {
		if seg.Type == team.SegmentSwitchTeam {
			tc, err := loadTeamByName(ctx, seg.Name, registry, defaultProviderURL, defaultProviderAPIKey, pathConsent, vars, forcedSkills, planMode, autoSkills)
			if err != nil {
				fmt.Printf("\n%s Cannot load team %q: %v\n", errStyle.Render("✗"), seg.Name, err)
				continue
			}

			fmt.Printf("\n%s %s\n\n",
				headerStyle.Render("─── Fix Analysis:"),
				teamStyle.Render("team "+seg.Name),
			)

			data := collectFixData(tc.session, seg.Content)
			analysis, err := runFixAnalysis(ctx, tc, fixQuestion, seg.Content, data)
			if err != nil {
				fmt.Printf("%s Analysis failed: %v\n", errStyle.Render("✗"), err)
				continue
			}
			fmt.Println(analysis)
		}
	}
	return nil
}

type fixData struct {
	SessionJSON   string
	SessionMD     string
	SessionLog    string
	STM           string
	LTM           string
	CoordinatorMD string
	AgentMDs      map[string]string
	TaskHistory   map[string]string
	TeamYAML      string
	Reliability   string
}

func collectFixData(session *team.TeamSession, taskDesc string) *fixData {
	d := &fixData{
		AgentMDs:    make(map[string]string),
		TaskHistory: make(map[string]string),
	}

	if js := team.LoadSession(session.Workspace); js != nil {
		var b strings.Builder
		for i, e := range js.Entries {
			if i >= 20 {
				break
			}
			fmt.Fprintf(&b, "[%s] %s: %s\n", e.Timestamp, e.Role, limitStr(e.Content, 500))
		}
		d.SessionJSON = b.String()
		if js.RunResult != nil {
			metrics := js.RunResult.Metrics
			d.Reliability = fmt.Sprintf("outcome=%s stop_reason=%s criteria_passed=%d tasks_by_criterion=%v diagnostic_tasks_since_progress=%d repeated_failure_fingerprints=%d recovery_strategy_changes=%d protocol_repairs=%d/%d replays_avoided=%d worker_success_rejected=%d weak_verifiers=%d time_since_progress_seconds=%d tokens_since_progress=%d turns_since_progress=%d tasks_since_progress=%d no_progress_limits=tokens:%d/turns:%d/tasks:%d", js.RunResult.Outcome, js.RunResult.StopReason, metrics.AcceptanceCriteriaPassed, metrics.TasksByCriterion, metrics.DiagnosticTasksSinceProgress, metrics.RepeatedFailureFingerprints, metrics.RecoveryStrategyChanges, metrics.ProtocolRepairsSucceeded, metrics.ProtocolRepairsAttempted, metrics.ExecutionReplaysAvoided, metrics.WorkerSuccessRejected, metrics.WeakVerifierWarnings, metrics.TimeSinceCriterionProgressSeconds, metrics.TokensSinceCriterionProgress, metrics.TurnsSinceCriterionProgress, metrics.TasksSinceCriterionProgress, metrics.MaxTokensWithoutProgress, metrics.MaxTurnsWithoutProgress, metrics.MaxTasksWithoutProgress)
		}
	}

	if md := team.LoadSessionMD(session.Workspace); md != "" {
		d.SessionMD = limitStr(md, 4000)
	}

	logPath := filepath.Join(session.Workspace, "execution_trace.log")
	if data, err := os.ReadFile(logPath); err == nil {
		d.SessionLog = limitStr(string(data), 8000)
	}

	if stm := team.LoadSTM(session.Workspace); stm != "" {
		d.STM = stm
	}

	if ltm := team.LoadLTM(session.Workspace, session.Config.Name); ltm != "" {
		d.LTM = limitStr(ltm, 4000)
	}

	coordPath := filepath.Join(session.Dir, "coordinator.md")
	if data, err := os.ReadFile(coordPath); err == nil {
		d.CoordinatorMD = string(data)
	}

	for name := range session.Agents {
		mdPath := filepath.Join(session.Dir, name+".md")
		if data, err := os.ReadFile(mdPath); err == nil {
			d.AgentMDs[name] = string(data)
		}
	}

	tasksDir := filepath.Join(session.Workspace, "tasks", session.Config.Name)
	entries, err := os.ReadDir(tasksDir)
	if err == nil {
		for _, entry := range entries {
			if !entry.IsDir() {
				continue
			}
			agentDir := filepath.Join(tasksDir, entry.Name())
			taskEntries, err := os.ReadDir(agentDir)
			if err != nil {
				continue
			}
			sort.Slice(taskEntries, func(i, j int) bool {
				return taskEntries[i].Name() > taskEntries[j].Name()
			})
			var b strings.Builder
			count := 0
			for _, te := range taskEntries {
				if count >= 5 {
					break
				}
				if !strings.HasSuffix(te.Name(), ".md") {
					continue
				}
				data, err := os.ReadFile(filepath.Join(agentDir, te.Name()))
				if err != nil {
					continue
				}
				fmt.Fprintf(&b, "--- %s ---\n", te.Name())
				b.WriteString(limitStr(string(data), 1000))
				b.WriteString("\n")
				count++
			}
			if count > 0 {
				d.TaskHistory[entry.Name()] = b.String()
			}
		}
	}

	yamlPath := filepath.Join(session.Dir, "team.yaml")
	if data, err := os.ReadFile(yamlPath); err == nil {
		d.TeamYAML = string(data)
	} else {
		yamlPath = filepath.Join(session.Dir, "team.yml")
		if data, err := os.ReadFile(yamlPath); err == nil {
			d.TeamYAML = string(data)
		}
	}

	return d
}

func runFixAnalysis(ctx context.Context, tc *teamContext, question string, taskDesc string, data *fixData) (string, error) {
	s := tc.coordinator.Sidecar()
	if s == nil {
		return runFixAnalysisDirect(ctx, question, taskDesc, data, tc.session.Config.Name, tc.session.Config.SidecarModel)
	}

	prompt := buildFixPrompt(question, taskDesc, data)
	sidecarCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	result, err := s.Execute(sidecarCtx, prompt)
	if err != nil {
		return "", fmt.Errorf("sidecar analysis failed: %w", err)
	}
	return result, nil
}

func runFixAnalysisDirect(ctx context.Context, question string, taskDesc string, data *fixData, teamName, sidecarModel string) (string, error) {
	prompt := buildFixPrompt(question, taskDesc, data)
	ctx2, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	if sidecarModel == "" {
		sidecarModel = "qwen3:8b" // last-resort fallback when neither team config nor hufu.yaml specifies one
	}
	cmd := exec.CommandContext(ctx2, "ollama", "run", "--format", "```", sidecarModel, prompt)
	cmd.Env = append(os.Environ(), "OLLAMA_NUM_PARALLEL=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("ollama analysis failed: %w, output: %s", err, string(out))
	}
	return string(out), nil
}

func buildFixPrompt(question, taskDesc string, data *fixData) string {
	var b strings.Builder

	b.WriteString("You are analyzing an agent team execution to find root causes and suggest specific improvements.\n\n")
	b.WriteString("Analyze the data below and provide:\n")
	b.WriteString("1. Root cause of the reported problem\n")
	b.WriteString("2. Specific suggestions for changes: rewrite specific sections of coordinator.md, agent .md files, or team.yaml\n")
	b.WriteString("3. Priority-ranked action items (🔴 critical, 🟡 important, 🟢 nice-to-have)\n\n")

	b.WriteString("## Problem to Investigate\n")
	b.WriteString(question)
	b.WriteString("\n\n")

	if taskDesc != "" {
		b.WriteString("## Original Task\n")
		b.WriteString(taskDesc)
		b.WriteString("\n\n")
	}

	if data.TeamYAML != "" {
		b.WriteString("## Team Configuration (team.yaml)\n```yaml\n")
		b.WriteString(data.TeamYAML)
		b.WriteString("\n```\n\n")
	}

	if data.CoordinatorMD != "" {
		b.WriteString("## Coordinator Instructions (coordinator.md)\n```markdown\n")
		b.WriteString(limitStr(data.CoordinatorMD, 3000))
		b.WriteString("\n```\n\n")
	}

	if len(data.AgentMDs) > 0 {
		b.WriteString("## Agent Definitions\n\n")
		for name, md := range data.AgentMDs {
			fmt.Fprintf(&b, "### %s.md\n```markdown\n%s\n```\n\n", name, limitStr(md, 1500))
		}
	}

	if data.STM != "" {
		b.WriteString("## Short-Term Memory (stm.md)\n```\n")
		b.WriteString(data.STM)
		b.WriteString("\n```\n\n")
	}

	if data.LTM != "" {
		b.WriteString("## Long-Term Memory (ltm.md)\n```\n")
		b.WriteString(data.LTM)
		b.WriteString("\n```\n\n")
	}

	if data.SessionJSON != "" {
		b.WriteString("## Session History (session.json)\n```\n")
		b.WriteString(data.SessionJSON)
		b.WriteString("\n```\n\n")
	}

	if data.Reliability != "" {
		b.WriteString("## Reliability Metrics\n```\n")
		b.WriteString(data.Reliability)
		b.WriteString("\n```\n\n")
	}

	if data.SessionLog != "" {
		b.WriteString("## Execution Log (execution_trace.log)\n```\n")
		b.WriteString(data.SessionLog)
		b.WriteString("\n```\n\n")
	}

	if data.SessionMD != "" {
		b.WriteString("## Session Document (chat_history.md)\n```markdown\n")
		b.WriteString(data.SessionMD)
		b.WriteString("\n```\n\n")
	}

	if len(data.TaskHistory) > 0 {
		b.WriteString("## Worker Task History\n\n")
		for agentName, history := range data.TaskHistory {
			fmt.Fprintf(&b, "### %s\n```\n%s\n```\n\n", agentName, history)
		}
	}

	b.WriteString("\nProvide your analysis now. Be specific — quote exact sections that need changes.")
	return b.String()
}

func limitStr(s string, maxChars int) string {
	r := []rune(s)
	if len(r) <= maxChars {
		return s
	}
	return string(r[:maxChars]) + "\n... [truncated]"
}
