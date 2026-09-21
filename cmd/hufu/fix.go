package main

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/sidecar"
	"github.com/kjelly/hufu/internal/team"
	"github.com/kjelly/hufu/internal/tools"
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
			analysis, err := func() (analysis string, runErr error) {
				defer func() {
					runErr = errors.Join(runErr, tc.Close())
				}()
				return runFixAnalysis(ctx, tc, fixQuestion, seg.Content, data)
			}()
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
			d.Reliability = fmt.Sprintf("outcome=%s stop_reason=%s criteria_passed=%d tasks_by_criterion=%v failures_by_class=%v failures_by_phase=%v diagnostic_tasks_since_progress=%d repeated_failure_fingerprints=%d recovery_strategy_changes=%d protocol_repairs=%d/%d protocol_repair_failures=%v replays_avoided=%d retry_attempts_avoided=%v worker_success_rejected=%d weak_verifiers=%d preflight_failures=%d non_asserting_rejected=%d verifications_overturned=%d typed_verifier_adoption=%d/%d(%.2f) tasks_without_objective_verifier=%d timeout_recovered=%d cancelled_excluded=%d time_since_progress_seconds=%d tokens_since_progress=%d turns_since_progress=%d tasks_since_progress=%d no_progress_limits=tokens:%d/turns:%d/tasks:%d", js.RunResult.Outcome, js.RunResult.StopReason, metrics.AcceptanceCriteriaPassed, metrics.TasksByCriterion, metrics.FailuresByClass, metrics.FailuresByPhase, metrics.DiagnosticTasksSinceProgress, metrics.RepeatedFailureFingerprints, metrics.RecoveryStrategyChanges, metrics.ProtocolRepairsSucceeded, metrics.ProtocolRepairsAttempted, metrics.ProtocolRepairFailuresByReason, metrics.ExecutionReplaysAvoided, metrics.RetryAttemptsAvoidedByDisposition, metrics.WorkerSuccessRejected, metrics.WeakVerifierWarnings, metrics.PreflightFailuresCaught, metrics.NonAssertingVerifiersRejected, metrics.VerificationsOverturned, metrics.TypedVerifiers, metrics.TasksWithVerifier, metrics.TypedVerifierAdoptionRate, metrics.TasksDoneWithoutObjectiveVerifier, metrics.TimeoutTasksRecovered, metrics.CancelledTasksExcludedFromRetries, metrics.TimeSinceCriterionProgressSeconds, metrics.TokensSinceCriterionProgress, metrics.TurnsSinceCriterionProgress, metrics.TasksSinceCriterionProgress, metrics.MaxTokensWithoutProgress, metrics.MaxTurnsWithoutProgress, metrics.MaxTasksWithoutProgress)
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
			slices.SortFunc(taskEntries, func(a, b os.DirEntry) int {
				return cmp.Compare(b.Name(), a.Name())
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
	if tc == nil || tc.coordinator == nil || tc.session == nil {
		return "", fmt.Errorf("fix analysis requires a coordinator")
	}
	if err := tc.coordinator.PrepareContextPreflightContext(ctx); err != nil {
		return "", fmt.Errorf("fix analysis provider boundary unavailable: %w", err)
	}
	defer tc.coordinator.CloseContextPreflight()

	s := tc.coordinator.Sidecar()
	if s == nil {
		return runFixAnalysisDirect(ctx, question, taskDesc, data, tc.session.Config.Name, tc.session.Config.SidecarModel)
	}

	prompt, err := buildFixPrompt(question, taskDesc, data)
	if err != nil {
		return "", fmt.Errorf("build fix analysis prompt: %w", err)
	}
	sidecarCtx, cancel := context.WithTimeout(tc.coordinator.ContextPreflight(), 90*time.Second)
	defer cancel()

	result, err := s.ExecuteProfile(sidecar.WithPurpose(sidecarCtx, "fix_analysis"), prompt, sidecar.CompactorProfile)
	if err != nil {
		return "", fmt.Errorf("sidecar analysis failed: %w", err)
	}
	return result, nil
}

func runFixAnalysisDirect(ctx context.Context, question string, taskDesc string, data *fixData, teamName, sidecarModel string) (string, error) {
	// This command may be reached before a team coordinator (and therefore its
	// durable context boundary) exists. Never start an un-attributed subprocess
	// model call from that state. The deterministic result remains useful to an
	// operator and makes the missing model boundary explicit.
	_ = ctx
	_ = teamName
	_ = sidecarModel
	var findings []string
	findings = append(findings, "## Deterministic Fix Analysis")
	findings = append(findings, "No context-attributed sidecar is available, so no model was called.")
	if strings.TrimSpace(question) != "" {
		findings = append(findings, "- Investigate the reported issue against the persisted execution receipts and objective verification results.")
	}
	if strings.TrimSpace(taskDesc) != "" {
		findings = append(findings, "- Compare the task contract with the latest task journal and context-manifest summaries.")
	}
	if data != nil && strings.TrimSpace(data.Reliability) != "" {
		findings = append(findings, "- Prioritize the recorded reliability counters, failure classes, and verification outcomes before changing agent prompts.")
	}
	findings = append(findings, "- Re-run with a loaded team sidecar to obtain a bounded, manifest-backed model analysis.")
	return strings.Join(findings, "\n"), nil
}

func buildFixPrompt(question, taskDesc string, data *fixData) (string, error) {
	const prefix = "You are analyzing an agent team execution to find root causes and suggest specific improvements.\n\n" +
		"Analyze the data below and provide:\n" +
		"1. Root cause of the reported problem\n" +
		"2. Specific suggestions for changes: rewrite specific sections of coordinator.md, agent .md files, or team.yaml\n" +
		"3. Priority-ranked action items (🔴 critical, 🟡 important, 🟢 nice-to-have)\n\n"
	const suffix = "\nProvide your analysis now. Be specific — quote exact sections that need changes."

	sections := []sidecar.PromptSection{
		{Header: "## Problem to Investigate\n", Content: question, Footer: "\n\n", Weight: 4, MaxRunes: 4000},
		{Header: "## Original Task\n", Content: taskDesc, Footer: "\n\n", Weight: 3, MaxRunes: 4000},
	}
	if data == nil {
		return sidecar.BuildBoundedPrompt(prefix, sections, suffix, sidecar.CompactorProfile.InputRuneLimit())
	}
	sections = appendFixPromptSection(sections, "## Team Configuration (team.yaml)\n```yaml\n", data.TeamYAML, "\n```\n\n", 2, 6000)
	sections = appendFixPromptSection(sections, "## Coordinator Instructions (coordinator.md)\n```markdown\n", data.CoordinatorMD, "\n```\n\n", 2, 3000)
	for _, name := range slices.Sorted(maps.Keys(data.AgentMDs)) {
		sections = appendFixPromptSection(sections, fmt.Sprintf("## Agent Definition: %s.md\n```markdown\n", name), data.AgentMDs[name], "\n```\n\n", 2, 1500)
	}
	sections = appendFixPromptSection(sections, "## Short-Term Memory (stm.md)\n```\n", data.STM, "\n```\n\n", 2, 6000)
	sections = appendFixPromptSection(sections, "## Long-Term Memory (ltm.md)\n```\n", data.LTM, "\n```\n\n", 1, 4000)
	sections = appendFixPromptSection(sections, "## Session History (session.json)\n```\n", data.SessionJSON, "\n```\n\n", 3, 8000)
	sections = appendFixPromptSection(sections, "## Reliability Metrics\n```\n", data.Reliability, "\n```\n\n", 2, 3000)
	sections = appendFixPromptSection(sections, "## Execution Log (execution_trace.log)\n```\n", data.SessionLog, "\n```\n\n", 3, 8000)
	sections = appendFixPromptSection(sections, "## Session Document (chat_history.md)\n```markdown\n", data.SessionMD, "\n```\n\n", 2, 4000)
	for _, agentName := range slices.Sorted(maps.Keys(data.TaskHistory)) {
		sections = appendFixPromptSection(sections, fmt.Sprintf("## Worker Task History: %s\n```\n", agentName), data.TaskHistory[agentName], "\n```\n\n", 2, 5000)
	}

	return sidecar.BuildBoundedPrompt(prefix, sections, suffix, sidecar.CompactorProfile.InputRuneLimit())
}

func appendFixPromptSection(sections []sidecar.PromptSection, header, content, footer string, weight, maxRunes int) []sidecar.PromptSection {
	if strings.TrimSpace(content) == "" {
		return sections
	}
	return append(sections, sidecar.PromptSection{
		Header: header, Content: content, Footer: footer, Weight: weight, MaxRunes: maxRunes,
	})
}

func limitStr(s string, maxChars int) string {
	r := []rune(s)
	if len(r) <= maxChars {
		return s
	}
	return string(r[:maxChars]) + "\n... [truncated]"
}
