package main

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/team"
	"github.com/kjelly/hufu/internal/utils"
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

// hasReportFindings reports whether any task carries a structured Finding.
// The boolean "findings are present" line in Review Outcome tells the reader
// something exists without saying what; the per-task detail lived only in
// session.json/JSONL, never in report.md itself.
func hasReportFindings(todos []*team.TodoItem) bool {
	for _, t := range todos {
		if t != nil && t.TypedResult != nil && len(t.TypedResult.Findings) > 0 {
			return true
		}
	}
	return false
}

// generateReport creates a markdown execution report for every loaded team.
// It is the explicit --report behavior.
func generateReport(loadedTeams map[string]*teamContext, combinedResult string) {
	generateReports(loadedTeams, combinedResult, func(*teamContext) bool { return true })
}

// generateRequestedReports preserves --report's all-team behavior while
// allowing a team to opt into reports for its own runs with auto-report: true.
func generateRequestedReports(loadedTeams map[string]*teamContext, combinedResult string) {
	if opts.reportMode {
		generateReport(loadedTeams, combinedResult)
		return
	}
	generateReports(loadedTeams, combinedResult, func(tc *teamContext) bool {
		return tc != nil && tc.session != nil && tc.session.Config.AutoReport
	})
}

// generateReports writes reports only for contexts accepted by include.
func generateReports(loadedTeams map[string]*teamContext, combinedResult string, include func(*teamContext) bool) {
	for teamName, tc := range loadedTeams {
		if tc == nil || !include(tc) {
			continue
		}
		if tc.coordinator != nil && !tc.coordinator.TerminalLifecycleConfirmed() {
			fmt.Fprintf(os.Stderr, "%s Report deferred for team %q: canonical run_finished is not confirmed\n", errStyle.Render("⚠"), teamName)
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
	Todos                 []*team.TodoItem
	STM                   string
	Skills                []team.SkillUsageEntry
	SessionData           *team.SessionData
	TaskHistory           map[string]string
	CurrentRunDiagnostics map[string]string
	StartedAt             time.Time
	SkillPatterns         []SkillPatternReport
	ContextUsageSection   string
	ResolvedProfile       team.ExecutionProfile
	RunResult             *team.RunResult
	SourceRunID           string
	EvidenceIdentity      string
	TerminalSessions      []team.TerminalSession
	WorkerMemory          team.WorkerMemoryReport
	MemoryLearning        team.MemoryLearningReport
	DeprecatedMemory      []team.DeprecatedMemoryToolUsage
	ContextRouting        team.ContextManifestSummary
	RuntimeWorksets       *team.RuntimeWorksetProjection
	RuntimeWorksetError   string
	CanonicalRunError     string
	HistoricalTodoCount   int
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
			return "no_objective_verifier"
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
		TaskHistory:           make(map[string]string),
		CurrentRunDiagnostics: make(map[string]string),
		StartedAt:             time.Now(),
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
		if sessions, err := tc.coordinator.TerminalSessions(context.Background()); err == nil {
			d.TerminalSessions = sessions
		}
		if workerMemory, err := tc.coordinator.WorkerMemoryReport(context.Background()); err == nil {
			d.WorkerMemory = workerMemory
		}
		d.MemoryLearning = tc.coordinator.MemoryLearningReport()
		d.DeprecatedMemory = tc.coordinator.DeprecatedMemoryToolReport()
		d.ContextRouting = tc.coordinator.ContextManifestReport()
	}
	if tc.session != nil {
		canonical, err := team.LoadCanonicalRunFinishedSnapshot(tc.session.Workspace, "")
		if err != nil {
			d.CanonicalRunError = err.Error()
		} else {
			d.RunResult = canonical
		}
	}
	if d.RunResult != nil {
		d.SourceRunID = d.RunResult.RunID
		if d.RunResult.EvidenceManifest != nil {
			if d.SourceRunID == "" {
				d.SourceRunID = d.RunResult.EvidenceManifest.RunID
			}
			d.EvidenceIdentity = d.RunResult.EvidenceManifest.ManifestHash
		}
	}
	if d.SourceRunID == "" {
		d.SourceRunID = "run-unavailable"
	}
	if d.EvidenceIdentity == "" {
		d.EvidenceIdentity = "unavailable"
	}
	if tc.session != nil {
		if d.RunResult == nil {
			// A workset pointer without a confirmed run_finished snapshot is only
			// an uncommitted filesystem projection and must not enter the report.
			d.RuntimeWorksetError = "canonical run_finished snapshot is unavailable"
		} else if projection, err := team.LoadRuntimeWorksetProjection(tc.session.Workspace, d.RunResult); err != nil {
			d.RuntimeWorksetError = err.Error()
		} else {
			d.RuntimeWorksets = projection
		}
		verifiedManifest, verified := verifiedEvidenceManifest(tc.session.Workspace, d.RunResult)
		if verified {
			d.Todos, d.HistoricalTodoCount = latestRunTodos(d.Todos, verifiedManifest)
		}
		d.STM = team.LoadSTM(tc.session.Workspace)

		if d.ResolvedProfile.DisableHistoricalTaskReuse {
			if verified {
				d.CurrentRunDiagnostics = currentRunReportDiagnostics(d.Todos, verifiedManifest)
			}
		} else {
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
	}

	return d
}

func verifiedEvidenceManifest(workspace string, result *team.RunResult) (*team.EvidenceManifest, bool) {
	if result == nil || result.EvidenceManifest == nil || result.EvidenceManifest.RunID == "" {
		return nil, false
	}
	store, err := team.NewFileArtifactStore(workspace, workspace)
	if err != nil || result.EvidenceManifest.Verify(context.Background(), store) != nil {
		return nil, false
	}
	return result.EvidenceManifest, true
}

// currentRunReportDiagnostics is built only from canonical task receipts and
// the sealed evidence manifest. It intentionally never opens workspace/tasks
// markdown, whose filenames and contents are not run-scoped.
func currentRunReportDiagnostics(todos []*team.TodoItem, manifest *team.EvidenceManifest) map[string]string {
	diagnostics := make(map[string]string)
	if manifest == nil || manifest.RunID == "" {
		return diagnostics
	}
	runID := manifest.RunID
	for _, item := range todos {
		if item == nil {
			continue
		}
		binding, ok := manifest.VerifiedTaskBinding(item.ID)
		if !ok || binding.RunID != runID {
			continue
		}
		// Extra-model candidates can share one Todo attempt. Completion order
		// does not prove which candidate produced the merged/judged output, so
		// do not publish a transcript or winner claim without an explicit
		// output-to-receipt binding.
		if itemHasAmbiguousCurrentRunReceipts(item, runID) {
			continue
		}
		var b strings.Builder
		fmt.Fprintf(&b, "- run_id: `%s`\n- task_id: `%s`\n- attempt: `%d`\n- model_execution_id: `%s`\n- producer: `%s`\n",
			reportSafeMetadata(binding.RunID, 120), reportSafeMetadata(binding.TaskID, 120), binding.Attempt, reportSafeMetadata(binding.ModelExecutionID, 160), reportSafeMetadata(binding.ProducerID, 120))
		fmt.Fprintf(&b, "- transcript_ref: `%s`\n- artifact_membership: `%d`\n- artifact_verification: `verified`\n", reportSafeMetadata(binding.TranscriptRef, 240), len(binding.ArtifactIDs))
		diagnostics[item.Agent] += fmt.Sprintf("### Task %s\n%s\n", reportSafeMetadata(item.ID, 120), b.String())
	}
	return diagnostics
}

func reportSafeMetadata(value string, max int) string {
	value = utils.RedactSecrets(value)
	value = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, value)
	value = strings.ReplaceAll(value, "`", "'")
	return limitStr(strings.TrimSpace(value), max)
}

// latestRunTodos excludes only tasks whose durable receipts positively bind
// them to another run. Tasks without a receipt remain visible: guessing they
// are historical would hide an unfinished/recovered task from operators.
func latestRunTodos(items []*team.TodoItem, manifest *team.EvidenceManifest) ([]*team.TodoItem, int) {
	if manifest == nil || manifest.RunID == "" {
		return items, 0
	}
	current := make([]*team.TodoItem, 0, len(items))
	historical := 0
	for _, item := range items {
		if item == nil {
			continue
		}
		if itemHasReceiptsFromAnotherRun(item, manifest.RunID) && !itemHasCurrentRunReceipt(item, manifest.RunID) {
			historical++
			continue
		}
		current = append(current, item)
	}
	return current, historical
}

func itemHasCurrentRunReceipt(item *team.TodoItem, runID string) bool {
	if item == nil || runID == "" {
		return false
	}
	for _, receipt := range item.ExecutionReceipts {
		if receipt.RunID == runID {
			return true
		}
	}
	return item.ExecutionReceipt != nil && item.ExecutionReceipt.RunID == runID
}

func itemHasReceiptsFromAnotherRun(item *team.TodoItem, runID string) bool {
	if item == nil {
		return false
	}
	for _, receipt := range item.ExecutionReceipts {
		if receipt.RunID != "" && receipt.RunID != runID {
			return true
		}
	}
	return item.ExecutionReceipt != nil && item.ExecutionReceipt.RunID != "" && item.ExecutionReceipt.RunID != runID
}

func itemHasAmbiguousCurrentRunReceipts(item *team.TodoItem, runID string) bool {
	if item == nil || runID == "" {
		return false
	}
	seen := make(map[string]bool)
	count := 0
	for _, receipt := range item.ExecutionReceipts {
		if receipt.RunID != runID || receipt.ExitCode != nil && *receipt.ExitCode != 0 {
			continue
		}
		identity := receipt.ModelExecutionID + "\x00" + receipt.TranscriptRef
		if !seen[identity] {
			seen[identity] = true
			count++
		}
	}
	if item.ExecutionReceipt != nil && item.ExecutionReceipt.RunID == runID && (item.ExecutionReceipt.ExitCode == nil || *item.ExecutionReceipt.ExitCode == 0) {
		identity := item.ExecutionReceipt.ModelExecutionID + "\x00" + item.ExecutionReceipt.TranscriptRef
		if !seen[identity] {
			count++
		}
	}
	return count > 1
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

//nolint:gocyclo // report rendering intentionally covers all execution projections.
func buildReportMD(data *reportData, teamName string, finalResult string) string {
	var b strings.Builder

	fmt.Fprintf(&b, "# Execution Report — %s\n\n", teamName)
	fmt.Fprintf(&b, "**Generated:** %s\n\n", time.Now().Format(time.RFC3339))

	duration := time.Since(data.StartedAt).Round(time.Second)
	fmt.Fprintf(&b, "**Duration:** %s\n\n", duration)
	snapshotState := "unavailable"
	if data.RunResult != nil {
		snapshotState = "confirmed"
	}
	b.WriteString("## Run Snapshot\n\n")
	fmt.Fprintf(&b, "- **Source run_id:** `%s`\n", reportSafeMetadata(data.SourceRunID, 160))
	fmt.Fprintf(&b, "- **Snapshot:** `%s` (canonical run_finished reducer snapshot)\n", snapshotState)
	fmt.Fprintf(&b, "- **Evidence identity:** `%s`\n\n", reportSafeMetadata(data.EvidenceIdentity, 160))
	if data.CanonicalRunError != "" {
		fmt.Fprintf(&b, "> ⚠️ Canonical run snapshot was not accepted: %s\n\n", reportSafeMetadata(data.CanonicalRunError, 240))
	}
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
		if len(data.RunResult.Worksets) > 0 {
			b.WriteString("\n### Workset Groups\n\n")
			b.WriteString("| Workset | Source artifact | Expected | Completed | Verified | Failed | State |\n")
			b.WriteString("|---|---|---:|---:|---:|---:|---|\n")
			for _, workset := range data.RunResult.Worksets {
				fmt.Fprintf(&b, "| `%s` | `%s` | %d | %d | %d | %d | `%s` |\n", reportSafeMetadata(workset.WorksetID, 120), reportSafeMetadata(workset.SourceArtifactID, 120), workset.Expected, workset.Completed, workset.Verified, workset.Failed, reportSafeMetadata(workset.State, 40))
			}
		}
		if data.RuntimeWorksets != nil {
			b.WriteString("\n### Runtime Workset Artifacts\n\n")
			fmt.Fprintf(&b, "- **Projection run_id:** `%s`\n", reportSafeMetadata(data.RuntimeWorksets.RunID, 160))
			fmt.Fprintf(&b, "- **Verified action manifests:** %d\n", len(data.RuntimeWorksets.Pointers))
			for _, pointer := range data.RuntimeWorksets.Pointers {
				fmt.Fprintf(&b, "- `%s` — sha256 `%s`\n", reportSafeMetadata(pointer.ManifestArtifactID, 160), reportSafeMetadata(pointer.ManifestSHA256, 160))
			}
		} else if data.RuntimeWorksetError != "" {
			fmt.Fprintf(&b, "\n> ⚠️ Runtime workset projection was not accepted for run `%s`: %s\n", reportSafeMetadata(data.SourceRunID, 160), reportSafeMetadata(data.RuntimeWorksetError, 240))
		}
		metrics := data.RunResult.Metrics
		b.WriteString("\n### Reliability Metrics\n\n")
		fmt.Fprintf(&b, "- **Acceptance criteria passed:** %d\n", metrics.AcceptanceCriteriaPassed)
		fmt.Fprintf(&b, "- **Protocol repairs:** %d attempted, %d succeeded\n", metrics.ProtocolRepairsAttempted, metrics.ProtocolRepairsSucceeded)
		fmt.Fprintf(&b, "- **Policy-denied tool calls:** %d (safe fresh attempts: %d; schema repairs: %d; budget wrap-ups: %d)\n", metrics.PolicyDeniedToolCalls, metrics.SafeFreshAttempts, metrics.SchemaRepairDenials, metrics.StepBudgetWrapUps)
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
		if data.ResolvedProfile.AcceptanceMode == team.AcceptanceAdvisory {
			b.WriteString("- **Acceptance Notice:** Acceptance is advisory; it does not prove findings are fixed.\n")
		}
		fmt.Fprintf(&b, "- **Default Cache Policy:** `%s`\n", data.ResolvedProfile.DefaultCachePolicy)
		fmt.Fprintf(&b, "- **Default Recovery Policy:** `%s`\n", data.ResolvedProfile.DefaultRecoveryPolicy)
		fmt.Fprintf(&b, "- **Disable Historical Memory:** %t\n", data.ResolvedProfile.DisableHistoricalMemory)
		fmt.Fprintf(&b, "- **Disable Task Cache:** %t\n\n---\n\n", data.ResolvedProfile.DisableTaskCache)
	}

	if data.RunResult != nil && data.RunResult.CompletedReview {
		b.WriteString("## Review Outcome\n\n")
		if data.RunResult.FindingsPresent {
			b.WriteString("Review artifact completed; findings are present and have not been represented as fixed.\n\n")
		} else {
			b.WriteString("Review artifact completed; no objective implementation verification was implied.\n\n")
		}
	}

	if data.ContextUsageSection != "" {
		b.WriteString(data.ContextUsageSection)
	}
	writeWorkerMemoryReport(&b, data.WorkerMemory)
	writeMemoryLearningReport(&b, data.MemoryLearning)
	writeDeprecatedMemoryToolReport(&b, data.DeprecatedMemory)
	writeContextRoutingReport(&b, data.ContextRouting)

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
	if hasReportFindings(data.Todos) {
		b.WriteString("## Review Findings\n\n")
		for _, t := range data.Todos {
			if t == nil || t.TypedResult == nil || len(t.TypedResult.Findings) == 0 {
				continue
			}
			fmt.Fprintf(&b, "### %s (%s)\n\n", t.ID, t.Agent)
			for _, f := range t.TypedResult.Findings {
				label := ""
				if category := strings.TrimSpace(f.Category); category != "" {
					label = fmt.Sprintf("**[%s]** ", category)
				}
				if strings.TrimSpace(f.Detail) != "" {
					fmt.Fprintf(&b, "- %s%s — %s\n", label, f.Summary, f.Detail)
				} else {
					fmt.Fprintf(&b, "- %s%s\n", label, f.Summary)
				}
			}
			b.WriteString("\n")
		}
		b.WriteString("---\n\n")
	}
	if data.HistoricalTodoCount > 0 {
		fmt.Fprintf(&b, "## Historical Runs\n\n%d task projection(s) with receipts from earlier runs were excluded from this latest-run task table.\n\n---\n\n", data.HistoricalTodoCount)
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
		if data.RunResult != nil && data.RunResult.CompletedReview {
			b.WriteString("## Appendix: Session Context (STM)\n\n")
		} else {
			b.WriteString("## Session Context (STM)\n\n")
		}
		b.WriteString(data.STM)
		b.WriteString("\n\n---\n\n")
	}

	// A fresh session must not reintroduce prior-run diagnostics through the
	// report appendix. The current run is represented by its task summary and
	// sealed final result; task transcripts on disk are not run-scoped.
	if len(data.TaskHistory) > 0 && !data.ResolvedProfile.DisableHistoricalTaskReuse {
		if data.RunResult != nil && data.RunResult.CompletedReview {
			b.WriteString("## Appendix: Agent Task Transcripts\n\n")
		} else {
			b.WriteString("## Agent Task Details\n\n")
		}
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
	if len(data.CurrentRunDiagnostics) > 0 {
		b.WriteString("## Current-Run Evidence Diagnostics\n\n")
		agents := make([]string, 0, len(data.CurrentRunDiagnostics))
		for agentName := range data.CurrentRunDiagnostics {
			agents = append(agents, agentName)
		}
		sort.Strings(agents)
		for _, agentName := range agents {
			fmt.Fprintf(&b, "### %s\n\n%s\n", reportSafeMetadata(agentName, 120), data.CurrentRunDiagnostics[agentName])
		}
	}

	return utils.RedactSecrets(b.String())
}

func writeMemoryLearningReport(b *strings.Builder, report team.MemoryLearningReport) {
	if report.Mode == "" {
		return
	}
	b.WriteString("## Outcome-driven Memory\n\n")
	fmt.Fprintf(b, "- **Mode:** `%s`\n", report.Mode)
	fmt.Fprintf(b, "- **Policy version:** `%s`\n", report.PolicyVersion)
	fmt.Fprintf(b, "- **Retrievals / exposures:** %d / %d\n", report.RetrievalCount, report.ExposureCount)
	fmt.Fprintf(b, "- **Applied / outcomes:** %d / %d\n", report.AppliedCount, report.OutcomeCount)
	fmt.Fprintf(b, "- **Pending reducer repairs:** %d\n\n---\n\n", report.PendingRepairGaps)
}

func writeContextRoutingReport(b *strings.Builder, summary team.ContextManifestSummary) {
	if summary.Requests == 0 {
		return
	}
	b.WriteString("## Context Routing\n\n")
	fmt.Fprintf(b, "- **Requests:** %d\n- **Model calls:** %d\n- **Deterministic fallbacks:** %d\n- **Included items:** %d (%d tokens)\n- **Omitted items:** %d (%d tokens)\n", summary.Requests, summary.ModelCalls, summary.Fallbacks, summary.Included, summary.IncludedTokens, summary.Omitted, summary.OmittedTokens)
	if len(summary.OmitReasons) > 0 {
		b.WriteString("- **Omission reasons:**\n")
		keys := make([]string, 0, len(summary.OmitReasons))
		for reason := range summary.OmitReasons {
			keys = append(keys, reason)
		}
		sort.Strings(keys)
		for _, reason := range keys {
			fmt.Fprintf(b, "  - `%s`: %d\n", reason, summary.OmitReasons[reason])
		}
	}
	if len(summary.Purposes) > 0 {
		b.WriteString("- **Purposes:**\n")
		keys := make([]string, 0, len(summary.Purposes))
		for purpose := range summary.Purposes {
			keys = append(keys, purpose)
		}
		sort.Strings(keys)
		for _, purpose := range keys {
			fmt.Fprintf(b, "  - `%s`: %d\n", purpose, summary.Purposes[purpose])
		}
	}
	b.WriteString("\n---\n\n")
}

func writeDeprecatedMemoryToolReport(b *strings.Builder, report []team.DeprecatedMemoryToolUsage) {
	if len(report) == 0 {
		return
	}
	b.WriteString("## Deprecated Memory Compatibility Usage\n\n")
	b.WriteString("| Tool | Calls | Success | Fail closed | Denied |\n| --- | ---: | ---: | ---: | ---: |\n")
	for _, usage := range report {
		fmt.Fprintf(b, "| `%s` | %d | %d | %d | %d |\n", usage.Tool, usage.Calls, usage.Success, usage.FailClosed, usage.Denied)
	}
	b.WriteString("\nOnly content-free lifecycle counts are reported.\n\n---\n\n")
}

func writeWorkerMemoryReport(b *strings.Builder, report team.WorkerMemoryReport) {
	if report.Total == 0 {
		return
	}
	b.WriteString("## Worker Memory\n\n")
	fmt.Fprintf(b, "- **Items:** %d (session: %d, persistent: %d)\n", report.Total, report.Session, report.Persistent)
	fmt.Fprintf(b, "- **Lifecycle:** %d confirmed, %d candidate, %d rejected\n", report.Confirmed, report.Candidate, report.Rejected)
	b.WriteString("- **Item IDs:** ")
	for i, id := range report.ItemIDs {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(b, "`%s`", id)
	}
	b.WriteString("\n\nPrivate worker-memory content is intentionally omitted from execution reports.\n\n---\n\n")
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
