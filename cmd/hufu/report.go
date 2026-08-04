package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/anomalyco/hufu/internal/team"
	"github.com/anomalyco/hufu/internal/utils"
)

var taskStatusIcons = map[team.TaskStatus]string{
	team.TaskDone:      "●",
	team.TaskError:     "✗",
	team.TaskBlocked:   "⚠",
	team.TaskSkipped:   "—",
	team.TaskPending:   "○",
	team.TaskPlanned:   "◎",
	team.TaskVerifying: "◔",
}

// generateReport creates a markdown execution report for each loaded team
// and saves it to the team's workspace directory.
func generateReport(loadedTeams map[string]*teamContext, combinedResult string) {
	for teamName, tc := range loadedTeams {
		if tc == nil {
			continue
		}

		data := gatherReportData(tc, teamName)
		content := buildReportMD(data, teamName, combinedResult)

		path := filepath.Join(tc.session.Workspace, "report.md")
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "%s Failed to write report for team %q: %v\n",
				errStyle.Render("✗"), teamName, err)
			continue
		}

		fmt.Fprintf(os.Stderr, "%s Report saved: %s\n",
			doneStyle.Render("✓"),
			path,
		)
	}
}

type reportData struct {
	Todos               []*team.TodoItem
	STM                 string
	Skills              []team.SkillUsageEntry
	SessionData         *team.SessionData
	TaskHistory         map[string]string
	StartedAt           time.Time
	SkillPatterns       []SkillPatternReport
	ContextUsageSection string
	ResolvedProfile     team.ExecutionProfile
	RunResult           *team.RunResult
	TerminalSessions    []team.TerminalSession
}

// SkillPatternReport holds detected skill pattern info for reports
type SkillPatternReport struct {
	Name  string
	Tools []string
	Count int
	Desc  string
	Saved bool
}

func formatVerificationSummary(item *team.TodoItem) string {
	if item == nil {
		return ""
	}
	cmd := strings.TrimSpace(item.Verify)
	if item.VerifyResult == nil {
		if cmd == "" {
			return ""
		}
		return "pending: " + limitStr(cmd, 120)
	}
	if cmd == "" {
		cmd = item.VerifyResult.Command
	}
	status := "ok"
	if item.VerifyResult.ExitCode != 0 {
		status = fmt.Sprintf("exit %d", item.VerifyResult.ExitCode)
	}
	if item.VerifyResult.TimedOut {
		status += ", timed out"
	}
	return fmt.Sprintf("%s: %s (%s)", status, limitStr(cmd, 120), item.VerifyResult.Duration.Round(time.Millisecond))
}

func gatherReportData(tc *teamContext, teamName string) *reportData {
	d := &reportData{
		TaskHistory: make(map[string]string),
		StartedAt:   time.Now(),
	}

	if tc.sessionData != nil {
		if t, err := time.Parse(time.RFC3339, tc.sessionData.CreatedAt); err == nil {
			d.StartedAt = t
		}
		d.SessionData = tc.sessionData
	}

	if tc.coordinator != nil {
		d.Todos = tc.coordinator.TaskTracker().TodoList().Items()
		d.Skills = tc.coordinator.SkillUsage()
		d.SkillPatterns = gatherSkillPatterns(tc.coordinator)
		d.ContextUsageSection = tc.coordinator.RenderContextUsageSection()
		d.ResolvedProfile = tc.coordinator.ExecutionProfile()
		d.RunResult = tc.coordinator.LastRunResult()
		if sessions, err := tc.coordinator.TerminalSessions(context.Background()); err == nil {
			d.TerminalSessions = sessions
		}
	}
	if d.RunResult == nil && d.SessionData != nil {
		d.RunResult = d.SessionData.RunResult
	}

	if tc.session != nil {
		d.STM = team.LoadSTM(tc.session.Workspace)

		tasksDir := filepath.Join(tc.session.Workspace, "tasks", teamName)
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
				var mdEntries []os.DirEntry
				for _, te := range taskEntries {
					if strings.HasSuffix(te.Name(), ".md") {
						mdEntries = append(mdEntries, te)
					}
				}
				sort.Slice(mdEntries, func(i, j int) bool {
					return mdEntries[i].Name() > mdEntries[j].Name()
				})
				var b strings.Builder
				count := 0
				for _, te := range mdEntries {
					if count >= 10 {
						break
					}
					data, err := os.ReadFile(filepath.Join(agentDir, te.Name()))
					if err != nil {
						continue
					}
					fmt.Fprintf(&b, "### %s\n```\n%s\n```\n\n",
						strings.TrimSuffix(te.Name(), ".md"),
						limitStr(string(data), 1500))
					count++
				}
				if count > 0 {
					d.TaskHistory[entry.Name()] = b.String()
				}
			}
		}
	}

	return d
}

// gatherSkillPatterns extracts detected skill patterns from coordinator
func gatherSkillPatterns(coordinator *team.Coordinator) []SkillPatternReport {
	detector := coordinator.SkillDetector()
	if detector == nil {
		return nil
	}
	candidates := detector.FindCandidates(context.Background())
	var reports []SkillPatternReport
	for _, cand := range candidates {
		reports = append(reports, SkillPatternReport{
			Name:  cand.SuggestedName,
			Tools: cand.Sequence.Tools,
			Count: cand.Sequence.Count,
			Desc:  cand.SuggestedDesc,
			Saved: true,
		})
	}
	return reports
}

func buildReportMD(data *reportData, teamName string, finalResult string) string {
	var b strings.Builder

	fmt.Fprintf(&b, "# Execution Report — %s\n\n", teamName)
	fmt.Fprintf(&b, "**Generated:** %s\n\n", time.Now().Format(time.RFC3339))

	duration := time.Since(data.StartedAt).Round(time.Second)
	fmt.Fprintf(&b, "**Duration:** %s\n\n", duration)
	if data.RunResult != nil {
		b.WriteString("## Run Outcome\n\n")
		fmt.Fprintf(&b, "- **Outcome:** `%s`\n", data.RunResult.Outcome)
		fmt.Fprintf(&b, "- **Goal satisfied:** `%t`\n", data.RunResult.GoalSatisfied)
		if data.RunResult.GoalMode != "" {
			fmt.Fprintf(&b, "- **Goal mode:** `%s`\n", data.RunResult.GoalMode)
		}
		if data.RunResult.StopReason != "" {
			fmt.Fprintf(&b, "- **Stop reason:** `%s`\n", data.RunResult.StopReason)
		}
		if telemetry := data.RunResult.Telemetry; telemetry != nil {
			fmt.Fprintf(&b, "- **Plan revision:** `%s`\n", telemetry.PlanRevision)
			fmt.Fprintf(&b, "- **Evidence manifest:** `%s`\n", telemetry.EvidenceManifest)
			fmt.Fprintf(&b, "- **Terminal reason:** `%s`\n", telemetry.TerminalReason)
			fmt.Fprintf(&b, "- **Decision chain:** `%v`\n", telemetry.DecisionChain)
			fmt.Fprintf(&b, "- **Repair cost:** %d attempts, %d tokens, %dms\n", telemetry.RepairCost.Attempts, telemetry.RepairCost.Tokens, telemetry.RepairCost.WallClockMS)
		}
		fmt.Fprintf(&b, "- **Tasks unresolved:** %d\n", data.RunResult.Stats.TasksUnresolved)
		if data.RunResult.Acceptance != nil {
			fmt.Fprintf(&b, "- **Acceptance:** `%s`\n", data.RunResult.Acceptance.EffectiveState())
		}
		metrics := data.RunResult.Metrics
		b.WriteString("\n### Reliability Metrics\n\n")
		fmt.Fprintf(&b, "- **Acceptance criteria passed:** %d\n", metrics.AcceptanceCriteriaPassed)
		fmt.Fprintf(&b, "- **Protocol repairs:** %d attempted, %d succeeded\n", metrics.ProtocolRepairsAttempted, metrics.ProtocolRepairsSucceeded)
		fmt.Fprintf(&b, "- **Worker success claims rejected by verification:** %d\n", metrics.WorkerSuccessRejected)
		fmt.Fprintf(&b, "- **Weak verifier warnings:** %d\n", metrics.WeakVerifierWarnings)
		fmt.Fprintf(&b, "- **Preflight failures caught:** %d (non-asserting verifiers: %d)\n", metrics.PreflightFailuresCaught, metrics.NonAssertingVerifiersRejected)
		fmt.Fprintf(&b, "- **Verifications overturned by evidence:** %d\n", metrics.VerificationsOverturned)
		fmt.Fprintf(&b, "- **Failures by class:** %v\n", metrics.FailuresByClass)
		fmt.Fprintf(&b, "- **Failures by phase:** %v\n", metrics.FailuresByPhase)
		fmt.Fprintf(&b, "- **Typed verifier adoption:** %d/%d (%.0f%%)\n", metrics.TypedVerifiers, metrics.TasksWithVerifier, metrics.TypedVerifierAdoptionRate*100)
		fmt.Fprintf(&b, "- **Tasks accepted without an objective verifier:** %d\n", metrics.TasksDoneWithoutObjectiveVerifier)
		fmt.Fprintf(&b, "- **Retry attempts avoided:** %v\n", metrics.RetryAttemptsAvoidedByDisposition)
		fmt.Fprintf(&b, "- **Protocol repair failures by reason:** %v\n", metrics.ProtocolRepairFailuresByReason)
		fmt.Fprintf(&b, "- **Timeout tasks recovered through reconciliation:** %d\n", metrics.TimeoutTasksRecovered)
		fmt.Fprintf(&b, "- **Cancelled tasks excluded from retry statistics:** %d\n", metrics.CancelledTasksExcludedFromRetries)
		if metrics.TasksWithVerifier > 0 && metrics.TypedVerifiers == 0 {
			b.WriteString("\n> ⚠️ No typed verifiers were used in this run. Prefer `verify_spec` for file/path and JSON assertions.\n")
		}
		fmt.Fprintf(&b, "- **Execution replays avoided:** %d\n", metrics.ExecutionReplaysAvoided)
		fmt.Fprintf(&b, "- **Diagnostic tasks since criterion progress:** %d\n", metrics.DiagnosticTasksSinceProgress)
		fmt.Fprintf(&b, "- **Repeated failure fingerprints:** %d\n", metrics.RepeatedFailureFingerprints)
		fmt.Fprintf(&b, "- **Recovery strategy changes:** %d\n", metrics.RecoveryStrategyChanges)
		fmt.Fprintf(&b, "- **Time since criterion progress:** %ds\n", metrics.TimeSinceCriterionProgressSeconds)
		fmt.Fprintf(&b, "- **Tokens since criterion progress:** %d (limit %d)\n", metrics.TokensSinceCriterionProgress, metrics.MaxTokensWithoutProgress)
		fmt.Fprintf(&b, "- **Turns since criterion progress:** %d (limit %d)\n", metrics.TurnsSinceCriterionProgress, metrics.MaxTurnsWithoutProgress)
		fmt.Fprintf(&b, "- **Tasks since criterion progress:** %d (limit %d)\n", metrics.TasksSinceCriterionProgress, metrics.MaxTasksWithoutProgress)
		if len(metrics.TasksByCriterion) > 0 {
			b.WriteString("- **Tasks by criterion:**\n")
			keys := make([]string, 0, len(metrics.TasksByCriterion))
			for id := range metrics.TasksByCriterion {
				keys = append(keys, id)
			}
			sort.Strings(keys)
			for _, id := range keys {
				fmt.Fprintf(&b, "  - `%s`: %d\n", id, metrics.TasksByCriterion[id])
			}
		}
		b.WriteString("\n---\n\n")
	}
	b.WriteString("---\n\n")

	if finalResult != "" {
		b.WriteString("## Final Result\n\n")
		b.WriteString(finalResult)
		b.WriteString("\n\n---\n\n")
	}

	if data.ResolvedProfile.Name != "" {
		b.WriteString("## Execution Profile\n\n")
		fmt.Fprintf(&b, "- **Profile Name:** `%s` (schema v%d)\n", data.ResolvedProfile.Name, data.ResolvedProfile.SchemaVersion)
		fmt.Fprintf(&b, "- **Strict Policy:** %t\n", data.ResolvedProfile.StrictPolicy)
		fmt.Fprintf(&b, "- **Policy Failure Mode:** `%s`\n", data.ResolvedProfile.PolicyFailureMode)
		fmt.Fprintf(&b, "- **Acceptance Mode:** `%s`\n", data.ResolvedProfile.AcceptanceMode)
		fmt.Fprintf(&b, "- **Default Cache Policy:** `%s`\n", data.ResolvedProfile.DefaultCachePolicy)
		fmt.Fprintf(&b, "- **Default Recovery Policy:** `%s`\n", data.ResolvedProfile.DefaultRecoveryPolicy)
		fmt.Fprintf(&b, "- **Disable Historical Memory:** %t\n", data.ResolvedProfile.DisableHistoricalMemory)
		fmt.Fprintf(&b, "- **Disable Task Cache:** %t\n\n---\n\n", data.ResolvedProfile.DisableTaskCache)
	}

	if data.ContextUsageSection != "" {
		b.WriteString(data.ContextUsageSection)
	}

	if len(data.Todos) > 0 {
		b.WriteString("## Task Summary\n\n")
		b.WriteString("| ID | Status | Agent | Description | Detail | Verify | Duration |\n")
		b.WriteString("|----|--------|-------|-------------|--------|--------|----------|\n")
		for _, t := range data.Todos {
			statusIcon := taskStatusIcons[t.Status]
			if statusIcon == "" {
				statusIcon = "◑"
			}
			detail := reportTaskFailureDetail(t)
			verify := formatVerificationSummary(t)
			var dur string
			if !t.EndedAt.IsZero() && !t.StartedAt.IsZero() {
				dur = t.EndedAt.Sub(t.StartedAt).Round(time.Second).String()
			}
			fmt.Fprintf(&b, "| %s | %s | %s | %s | %s | %s | %s |\n",
				t.ID, statusIcon, t.Agent, t.Desc, detail, verify, dur)
		}
		b.WriteString("\n---\n\n")
	}

	writeTerminalSessionCleanup(&b, data.TerminalSessions)

	if failures := team.FailureEventsFromTodos(data.Todos); len(failures) > 0 {
		b.WriteString("## Failure Events\n\n")
		for _, failure := range failures {
			b.WriteString(team.RenderFailureMarkdown(&failure))
			b.WriteString("\n\n")
		}
		b.WriteString("---\n\n")
	}

	hasVerificationEvidence := false
	for _, t := range data.Todos {
		if t != nil && t.VerifyResult != nil {
			hasVerificationEvidence = true
			break
		}
	}
	if hasVerificationEvidence {
		b.WriteString("## Verification Evidence\n\n")
		for _, t := range data.Todos {
			if t == nil || t.VerifyResult == nil {
				continue
			}
			fmt.Fprintf(&b, "### Task %s: %s\n\n", t.ID, t.Desc)
			fmt.Fprintf(&b, "- Command: `%s`\n", limitStr(strings.TrimSpace(t.VerifyResult.Command), 200))
			if workDir := strings.TrimSpace(t.VerifyResult.WorkDir); workDir != "" {
				fmt.Fprintf(&b, "- Working directory: `%s`\n", limitStr(workDir, 200))
			}
			fmt.Fprintf(&b, "- Exit code: %d\n", t.VerifyResult.ExitCode)
			fmt.Fprintf(&b, "- Duration: %s\n", t.VerifyResult.Duration.Round(time.Millisecond))
			fmt.Fprintf(&b, "- Timed out: %t\n\n", t.VerifyResult.TimedOut)
			if stdout := strings.TrimSpace(t.VerifyResult.Stdout); stdout != "" {
				b.WriteString("#### Stdout\n\n")
				b.WriteString("```text\n")
				b.WriteString(stdout)
				b.WriteString("\n```\n\n")
			}
			if stderr := strings.TrimSpace(t.VerifyResult.Stderr); stderr != "" {
				b.WriteString("#### Stderr\n\n")
				b.WriteString("```text\n")
				b.WriteString(stderr)
				b.WriteString("\n```\n\n")
			}
		}
		b.WriteString("\n---\n\n")
	}

	if len(data.Skills) > 0 {
		b.WriteString("## Skills Used\n\n")
		for _, s := range data.Skills {
			fmt.Fprintf(&b, "- **%s** (×%d) — %s\n",
				s.Name, s.Count, strings.Join(s.Agents, ", "))
		}
		b.WriteString("\n---\n\n")
	}

	if len(data.SkillPatterns) > 0 {
		b.WriteString("## Auto-Detected Skill Patterns\n\n")
		b.WriteString("The following repeating patterns were detected and saved as skill drafts:\n\n")
		for _, p := range data.SkillPatterns {
			status := "○"
			if p.Saved {
				status = "✓"
			}
			fmt.Fprintf(&b, "%s **%s** (×%d)\n", status, p.Name, p.Count)
			fmt.Fprintf(&b, "   Pattern: %s\n", strings.Join(p.Tools, " → "))
			fmt.Fprintf(&b, "   Description: %s\n\n", p.Desc)
		}
		b.WriteString("\n---\n\n")
	}

	if data.STM != "" {
		b.WriteString("## Session Context (STM)\n\n")
		b.WriteString(data.STM)
		b.WriteString("\n\n---\n\n")
	}

	if len(data.TaskHistory) > 0 {
		b.WriteString("## Agent Task Details\n\n")
		agentNames := make([]string, 0, len(data.TaskHistory))
		for name := range data.TaskHistory {
			agentNames = append(agentNames, name)
		}
		sort.Strings(agentNames)
		for _, name := range agentNames {
			fmt.Fprintf(&b, "### %s\n\n", name)
			b.WriteString(data.TaskHistory[name])
		}
	}

	return utils.RedactSecrets(b.String())
}

func terminalSessionReportGuidance(session team.TerminalSession) string {
	switch session.CleanupState {
	case team.TerminalCleanupCompleted:
		return "Automatically contained; safe to retry."
	case team.TerminalCleanupManual:
		return "Manual intervention required; reconcile before retry."
	}
	if session.State == team.TerminalSessionUnknown {
		return "Unknown after restart; reconcile before retry."
	}
	return "No active terminal cleanup required."
}

func writeTerminalSessionCleanup(b *strings.Builder, sessions []team.TerminalSession) {
	if len(sessions) == 0 {
		return
	}
	b.WriteString("## Terminal Session Cleanup\n\n")
	b.WriteString("| Session | Owner task | State | Cleanup | Custody | Guidance |\n")
	b.WriteString("|---------|------------|-------|---------|---------|----------|\n")
	for _, session := range sessions {
		fmt.Fprintf(b, "| %s | %s | %s | %s | %s | %s |\n", session.ID, session.OwnerTaskID, session.State, session.CleanupState, session.Custodian, terminalSessionReportGuidance(session))
	}
	b.WriteString("\nTerminal output is retained only in its artifact reference and is not embedded in this report.\n\n---\n\n")
}

func reportTaskFailureDetail(item *team.TodoItem) string {
	if item == nil {
		return ""
	}
	switch item.Status {
	case team.TaskError, team.TaskBlocked, team.TaskProtocolIncomplete:
		failure := team.FailureDisplayText(item)
		if failure != "" {
			return limitStr(strings.ReplaceAll(failure, "\n", "<br>"), 1500)
		}
	}
	return ""
}
