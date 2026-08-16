package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/kjelly/hufu/internal/config"
	"github.com/kjelly/hufu/internal/readline"
	"github.com/kjelly/hufu/internal/team"
	"github.com/kjelly/hufu/internal/tools"
	tuipkg "github.com/kjelly/hufu/internal/tui"
)

// executeDryRun handles dry run path for the CLI.
func executeDryRun(ctx context.Context, segments []team.PromptSegment, prompt string, loadedTeams map[string]*teamContext) error {
	var dryRunTeamName string
	for _, seg := range segments {
		if seg.Type == team.SegmentSwitchTeam {
			dryRunTeamName = seg.Name
			break
		}
	}
	if dryRunTeamName == "" {
		dryRunTeamName = strings.ToLower(opts.agentTeamName)
	}
	if dryRunTeamName == "" {
		return fmt.Errorf("--dry-run requires a team (use --agent-team or @team-name in the prompt)")
	}
	tc, ok := loadedTeams[dryRunTeamName]
	if !ok {
		return fmt.Errorf("failed to load team %q for dry-run", dryRunTeamName)
	}
	dryRunPrompt := ""
	for _, seg := range segments {
		if seg.Type == team.SegmentSwitchTeam && seg.Name == dryRunTeamName {
			dryRunPrompt = seg.Content
		} else if seg.Type == team.SegmentText && dryRunPrompt == "" && dryRunTeamName != "" {
			dryRunPrompt = seg.Content
		}
	}
	if dryRunPrompt == "" {
		dryRunPrompt = prompt
	}
	if dryRunPrompt == "" {
		return fmt.Errorf("--dry-run requires a prompt")
	}

	fmt.Fprintf(os.Stderr, "\n%s Running dry-run for team %s...\n\n", boldStyle.Render("→"), teamStyle.Render(dryRunTeamName))

	dryDisp := newCoordDisplay(tc)
	result, err := tc.coordinator.DryRun(ctx, dryRunPrompt)
	dryDisp.stopTimer()

	if err != nil {
		return fmt.Errorf("dry-run failed: %w", err)
	}
	renderDryRun(result)
	generateRequestedReports(loadedTeams, "(dry-run — no tasks executed)")
	return nil
}

// loadTeamsForSegments loads and validates all the teams referenced in the switch team segments.
func loadTeamsForSegments(ctx context.Context, initialSegments []team.PromptSegment, registry *team.TeamRegistry, pathConsent *tools.PathConsent, pr *readline.PromptReader, vars map[string]string) (map[string]*teamContext, map[string]string, error) {
	loadedTeams := map[string]*teamContext{}
	for _, seg := range initialSegments {
		if seg.Type != team.SegmentSwitchTeam {
			continue
		}
		if _, ok := loadedTeams[seg.Name]; ok {
			continue
		}
		var tc *teamContext
		var err error
		if opts.defaultTeam && seg.Name == "default" {
			tc, err = loadDefaultTeam(ctx, opts.providerURL, opts.providerAPIKey, pathConsent, vars, opts.forcedSkills, opts.planMode, opts.autoSkills)
		} else {
			vars, err = promptForMissingTemplateVars(ctx, seg.Name, registry, pr, vars)
			if err != nil {
				return nil, vars, err
			}
			tc, err = loadTeamByName(ctx, seg.Name, registry, opts.providerURL, opts.providerAPIKey, pathConsent, vars, opts.forcedSkills, opts.planMode, opts.autoSkills)
		}
		if err != nil {
			return nil, vars, fmt.Errorf("failed to load team %q: %w\n  Verify the team exists in your search paths (run 'hufu list' or 'hufu doctor')", seg.Name, err)
		}
		if opts.stepsMode {
			tc.coordinator.SetStepConfirmFn(makeStepConfirmFn())
		}
		loadedTeams[seg.Name] = tc
	}
	return loadedTeams, vars, nil
}

func expandSegmentsWithAgents(initialSegments []team.PromptSegment, loadedTeams map[string]*teamContext, registry *team.TeamRegistry) ([]team.PromptSegment, error) {
	pipedSegments := team.SplitSegmentsByPipe(initialSegments)

	var segments []team.PromptSegment
	for _, seg := range pipedSegments {
		if seg.Type == team.SegmentSwitchTeam {
			tc := loadedTeams[seg.Name]
			if tc != nil && seg.Content != "" {
				subSegs, err := team.SplitSegmentByAgents(seg, registry, agentNamesFromSession(tc.session))
				if err != nil {
					return nil, err
				}
				if seg.IsPiped {
					// The piped result must land on the segment that actually
					// executes the @mention (team switch or agent invoke), not
					// on leading filler text split off before it.
					for i := len(subSegs) - 1; i >= 0; i-- {
						if subSegs[i].Content != "" {
							subSegs[i].IsPiped = true
							break
						}
					}
				}
				segments = append(segments, subSegs...)
			} else {
				segments = append(segments, seg)
			}
		} else {
			segments = append(segments, seg)
		}
	}
	return segments, nil
}

// executeAndReport handles the execution (either in TUI or CLI mode) and aggregates skill usage/reports.
func executeAndReport(ctx context.Context, cancel context.CancelFunc, prompt, originalPrompt string, segments []team.PromptSegment, registry *team.TeamRegistry, loadedTeams map[string]*teamContext, injector *promptInjector, activeCoord *activeCoordinator, pathConsent *tools.PathConsent, vars map[string]string, route RouteDecision) error {
	startedAt := time.Now()
	// Restored sessions retain terminal failures so they remain visible to the
	// operator, but those historical failures must not make a later successful
	// invocation exit non-zero. Record the unresolved tasks that predate this
	// execution and only report failures created (or re-created) below.
	priorUnresolved := make(map[string]map[string]time.Time, len(loadedTeams))
	priorResults := make(map[string]*team.RunResult, len(loadedTeams))
	for name, tc := range loadedTeams {
		if tc != nil && tc.coordinator != nil {
			priorUnresolved[name] = snapshotUnresolvedTasks(tc.coordinator.TaskTracker().TodoList().Items())
			priorResults[name] = tc.coordinator.LastRunResult()
		}
	}
	var result string
	var runErr error
	if opts.tuiMode {
		teamInfo := buildTeamInfoForTUI(registry, loadedTeams)
		result, runErr = runWithTUI(ctx, cancel, prompt, segments, registry, loadedTeams, injector, activeCoord, pathConsent, vars, teamInfo, route)
	} else {
		result, runErr = executeSegments(ctx, segments, registry, opts.providerURL, loadedTeams, injector, activeCoord, pathConsent, vars, route)
	}
	if runErr != nil {
		// Abort paths still have a canonical RunResult on each coordinator.
		// Machine-readable callers must receive that result even though the
		// process returns non-zero; otherwise an abort is indistinguishable from
		// missing output (and callers may incorrectly treat it as completed).
		generateRequestedReports(loadedTeams, result)
		if opts.outputFormat == "json" {
			if outputErr := printResultJSONWithPrior(result, loadedTeams, nil, priorUnresolved); outputErr != nil {
				return fmt.Errorf("%w (json output failed: %v)", runErr, outputErr)
			}
		}
		return team.WrapRunOutcomeError(runErr, canonicalNonSuccessfulRunResultWithPrior(loadedTeams, priorResults, priorUnresolved))
	}

	generateRequestedReports(loadedTeams, result)

	var allSkillUsage []team.SkillUsageEntry
	seenSkill := map[string]int{}
	for teamName, tc := range loadedTeams {
		for _, entry := range tc.coordinator.SkillUsage() {
			key := strings.ToLower(entry.Name)
			if idx, ok := seenSkill[key]; ok {
				allSkillUsage[idx].Count += entry.Count
				for _, a := range entry.Agents {
					prefixed := teamName + "/" + a
					if !slices.Contains(allSkillUsage[idx].Agents, prefixed) {
						allSkillUsage[idx].Agents = append(allSkillUsage[idx].Agents, prefixed)
					}
				}
			} else {
				seenSkill[key] = len(allSkillUsage)
				prefixed := make([]string, len(entry.Agents))
				for i, a := range entry.Agents {
					prefixed[i] = teamName + "/" + a
				}
				allSkillUsage = append(allSkillUsage, team.SkillUsageEntry{
					Name:   entry.Name,
					Count:  entry.Count,
					Agents: prefixed,
				})
			}
		}
	}
	if opts.outputFormat == "json" {
		if err := printResultJSONWithPrior(result, loadedTeams, allSkillUsage, priorUnresolved); err != nil {
			return err
		}
	} else {
		fmt.Println(result)
		if !opts.quietMode {
			renderSkillSummary(allSkillUsage)
			if !opts.noSummary {
				renderExecutionSummary(os.Stderr, loadedTeams, time.Since(startedAt))
			}
		}
	}

	if opts.archiveMemory && !opts.newSession {
		for _, tc := range loadedTeams {
			archiveCurrentSessionToMemory(ctx, tc)
		}
	}

	if opts.tempWorkspace {
		absWS, _ := filepath.Abs(opts.workspace)
		fmt.Fprintf(os.Stderr, "\n%s\n  Path: %s\n",
			boldStyle.Render("─── Temporary Workspace ───"),
			absWS)
	}

	if originalPrompt != "" {
		savePromptToHistory(ctx, originalPrompt, opts.providerURL)
	}

	var unresolvedErr error
	for name, tc := range loadedTeams {
		if tc == nil || tc.coordinator == nil {
			continue
		}
		if item := executionUnresolvedTask(tc.coordinator.TaskTracker().TodoList().Items(), priorUnresolved[name]); item != nil {
			unresolvedErr = formatUnresolvedTaskError(tc.teamName, item)
			break
		}
	}
	if outcome := canonicalNonSuccessfulRunResultWithPrior(loadedTeams, priorResults, priorUnresolved); outcome != nil {
		if unresolvedErr == nil {
			unresolvedErr = fmt.Errorf("run outcome is %s", outcome.Outcome)
		}
		return team.WrapRunOutcomeError(unresolvedErr, outcome)
	}
	if unresolvedErr != nil {
		return unresolvedErr
	}

	return nil
}

func formatUnresolvedTaskError(teamName string, item *team.TodoItem) error {
	if item == nil {
		return team.ErrTasksUnresolved
	}
	return fmt.Errorf("%w: team %s task %s (%s): %s", team.ErrTasksUnresolved, teamName, item.ID, item.Agent, team.FailureDisplayText(item))
}

// canonicalNonSuccessfulRunResult folds only results produced by this
// invocation and delegates outcome/exit-code selection to the team evaluator.
// Historical restored results are intentionally excluded so a later run is
// not made to fail by an earlier session branch.
func canonicalNonSuccessfulRunResult(loadedTeams map[string]*teamContext, priorResults map[string]*team.RunResult) *team.RunResult {
	return canonicalNonSuccessfulRunResultWithPrior(loadedTeams, priorResults, nil)
}

func canonicalNonSuccessfulRunResultWithPrior(loadedTeams map[string]*teamContext, priorResults map[string]*team.RunResult, priorUnresolved map[string]map[string]time.Time) *team.RunResult {
	var results []*team.RunResult
	var unresolved []team.TaskReference
	var stats team.RunStats
	for name, tc := range loadedTeams {
		if tc == nil || tc.coordinator == nil {
			continue
		}
		result := tc.coordinator.LastRunResult()
		if result == nil || result == priorResults[name] {
			continue
		}
		results = append(results, result)
		unresolved = append(unresolved, result.UnresolvedTasks...)
		if tracker := tc.coordinator.TaskTracker(); tracker != nil && tracker.TodoList() != nil {
			for _, item := range tracker.TodoList().Items() {
				if item == nil {
					continue
				}
				if item.Status == team.TaskError || item.Status == team.TaskBlocked {
					if prior := priorUnresolved[name]; prior != nil {
						if endedAt, ok := prior[item.ID]; ok && endedAt.Equal(item.EndedAt) {
							continue
						}
					}
				}
				unresolved = append(unresolved, team.UnresolvedTaskReferences([]*team.TodoItem{item})...)
			}
		}
		stats.TasksTotal += result.Stats.TasksTotal
		stats.TasksDone += result.Stats.TasksDone
		stats.TasksUnresolved += result.Stats.TasksUnresolved
		stats.AttemptsTotal += result.Stats.AttemptsTotal
		stats.AttemptsFailed += result.Stats.AttemptsFailed
	}
	if len(results) == 0 {
		return nil
	}
	evaluated := team.AggregateRunResults(results, unresolved, stats)
	if team.IsRunOutcomeSuccess(evaluated.Outcome) {
		return nil
	}
	return &evaluated
}

// snapshotUnresolvedTasks records terminal failures already present before an
// execution. EndedAt lets a retry of the same todo ID count as this run while
// an unchanged, restored failure remains historical state.
func snapshotUnresolvedTasks(items []*team.TodoItem) map[string]time.Time {
	prior := make(map[string]time.Time)
	for _, item := range items {
		if item != nil && (item.Status == team.TaskError || item.Status == team.TaskBlocked) {
			prior[item.ID] = item.EndedAt
		}
	}
	return prior
}

// buildTeamInfoForTUI constructs TeamInfo from loaded teams for the TUI.
func buildTeamInfoForTUI(registry *team.TeamRegistry, loadedTeams map[string]*teamContext) tuipkg.TeamInfo {
	var teamInfo tuipkg.TeamInfo
	teamInfo.AvailableTeams = registry.ListTeams()
	for _, tc := range loadedTeams {
		if tc == nil || tc.session == nil {
			continue
		}
		teamInfo.TeamName = tc.session.Config.Name
		teamInfo.Workspace = tc.session.Workspace
		teamInfo.TeamDir = tc.session.Dir
		teamInfo.DefaultModel = tc.session.Config.Generation.Model
		for _, ag := range sortedAgents(tc.session.Agents) {
			model := ag.Generation.Model
			if model == "" {
				model = tc.session.Config.Generation.Model
			}
			teamInfo.Agents = append(teamInfo.Agents, tuipkg.AgentInfoEntry{
				Name:  ag.Name,
				Role:  ag.Role,
				Model: model,
			})
		}
		for _, s := range tc.session.Skills {
			teamInfo.Skills = append(teamInfo.Skills, s.Name)
		}
		if sc := tc.session.Config.SidecarModel; sc != "" {
			teamInfo.SidecarModel = sc
		}
		if gm := tc.session.Config.GuardModel; gm != "" {
			teamInfo.GuardModel = gm
		}
		teamInfo.MemoryEnabled = opts.memoryEnabled && !opts.tempWorkspace
		if teamInfo.MemoryEnabled {
			teamInfo.MemoryModel = config.ResolveEmbeddingModel(opts.memoryModel)
		}
		teamInfo.SSHSessions = 0
		teamInfo.PTYEnabled = opts.enablePTYTerminal
		teamInfo.HufuBinary, _ = os.Executable()
		break
	}
	return teamInfo
}

func executionUnresolvedTask(items []*team.TodoItem, prior map[string]time.Time) *team.TodoItem {
	for _, item := range items {
		if item == nil || (item.Status != team.TaskError && item.Status != team.TaskBlocked) {
			continue
		}
		if endedAt, existedBefore := prior[item.ID]; !existedBefore || !item.EndedAt.Equal(endedAt) {
			return item
		}
	}
	return nil
}

type executionSummary struct {
	teams      []string
	workspaces []string
	total      int
	done       int
	errored    int
	skipped    int
	pending    int
}

func summarizeExecution(loadedTeams map[string]*teamContext) executionSummary {
	summary := executionSummary{}
	for name, tc := range loadedTeams {
		if tc == nil || tc.coordinator == nil {
			continue
		}
		summary.teams = append(summary.teams, name)
		if tc.session != nil && tc.session.Workspace != "" {
			summary.workspaces = append(summary.workspaces, tc.session.Workspace)
		}
		for _, item := range tc.coordinator.TaskTracker().TodoList().Items() {
			summary.total++
			switch item.Status {
			case team.TaskDone:
				summary.done++
			case team.TaskError, team.TaskBlocked:
				summary.errored++
			case team.TaskSkipped:
				summary.skipped++
			default:
				summary.pending++
			}
		}
	}
	sort.Strings(summary.teams)
	sort.Strings(summary.workspaces)
	return summary
}

func renderExecutionSummary(w io.Writer, loadedTeams map[string]*teamContext, duration time.Duration) {
	summary := summarizeExecution(loadedTeams)
	if summary.total == 0 && len(summary.teams) == 0 {
		return
	}
	var names []string
	for name := range loadedTeams {
		names = append(names, name)
	}
	sort.Strings(names)

	var resList []*team.RunResult
	for _, name := range names {
		tc := loadedTeams[name]
		if tc != nil && tc.coordinator != nil {
			if r := tc.coordinator.LastRunResult(); r != nil {
				resList = append(resList, r)
			}
		}
	}
	_, _ = fmt.Fprint(w, formatExecutionSummary(summary, duration, resList))
}

func formatExecutionSummary(summary executionSummary, duration time.Duration, runResults []*team.RunResult) string {
	var b strings.Builder
	b.WriteString("\n")
	b.WriteString(headerStyle.Render("─── Summary ───"))
	b.WriteString("\n")
	if len(summary.teams) > 0 {
		fmt.Fprintf(&b, "  Team:      %s\n", strings.Join(summary.teams, ", "))
	}
	fmt.Fprintf(&b, "  Tasks:     %d done · %d error · %d skipped · %d pending (%d total)\n", summary.done, summary.errored, summary.skipped, summary.pending, summary.total)
	fmt.Fprintf(&b, "  Duration:  %s\n", duration.Round(time.Second))
	if len(summary.workspaces) > 0 {
		fmt.Fprintf(&b, "  Workspace: %s\n", strings.Join(summary.workspaces, ", "))
	}
	if len(runResults) > 0 {
		canonical := team.AggregateRunResults(runResults, nil, team.RunStats{})
		fmt.Fprintf(&b, "  Outcome:   %s (satisfied: %t, mode: %s, stop reason: %s)\n",
			canonical.Outcome, canonical.GoalSatisfied, canonical.GoalMode, canonical.StopReason)
		fmt.Fprintf(&b, "  Status:    %s\n", team.FormatCanonicalStatus(&canonical))
	}
	return b.String()
}
