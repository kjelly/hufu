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

	"github.com/anomalyco/hufu/internal/config"
	"github.com/anomalyco/hufu/internal/readline"
	"github.com/anomalyco/hufu/internal/team"
	"github.com/anomalyco/hufu/internal/tools"
	tuipkg "github.com/anomalyco/hufu/internal/tui"
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
		dryRunTeamName = strings.ToLower(agentTeamName)
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
	if reportMode {
		generateReport(loadedTeams, "(dry-run — no tasks executed)")
	}
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
		if defaultTeam && seg.Name == "default" {
			tc, err = loadDefaultTeam(ctx, providerURL, providerAPIKey, pathConsent, vars, forcedSkills, planMode, autoSkills)
		} else {
			vars, err = promptForMissingTemplateVars(ctx, seg.Name, registry, pr, vars)
			if err != nil {
				return nil, vars, err
			}
			tc, err = loadTeamByName(ctx, seg.Name, registry, providerURL, providerAPIKey, pathConsent, vars, forcedSkills, planMode, autoSkills)
		}
		if err != nil {
			return nil, vars, fmt.Errorf("failed to load team %q: %w\n  Verify the team exists in your search paths (run 'hufu list' or 'hufu doctor')", seg.Name, err)
		}
		if stepsMode {
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
func executeAndReport(ctx context.Context, cancel context.CancelFunc, prompt, originalPrompt string, segments []team.PromptSegment, registry *team.TeamRegistry, loadedTeams map[string]*teamContext, injector *promptInjector, activeCoord *activeCoordinator, pathConsent *tools.PathConsent, vars map[string]string) error {
	startedAt := time.Now()
	var result string
	var runErr error
	if tuiMode {
		var teamInfo tuipkg.TeamInfo
		teamInfo.AvailableTeams = registry.ListTeams()
		for _, tc := range loadedTeams {
			if tc != nil && tc.session != nil {
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
				teamInfo.MemoryEnabled = memoryEnabled && !tempWorkspace
				if teamInfo.MemoryEnabled {
					teamInfo.MemoryModel = config.ResolveEmbeddingModel(memoryModel)
				}
				teamInfo.SSHSessions = 0
				break
			}
		}
		result, runErr = runWithTUI(ctx, cancel, prompt, segments, registry, loadedTeams, injector, activeCoord, pathConsent, vars, teamInfo)
	} else {
		result, runErr = executeSegments(ctx, segments, registry, providerURL, loadedTeams, injector, activeCoord, pathConsent, vars)
	}
	if runErr != nil {
		return runErr
	}

	if reportMode {
		generateReport(loadedTeams, result)
	}

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
	if outputFormat == "json" {
		if err := printResultJSON(result, loadedTeams, allSkillUsage); err != nil {
			return err
		}
	} else {
		fmt.Println(result)
		if !quietMode {
			renderSkillSummary(allSkillUsage)
			if !noSummary {
				renderExecutionSummary(os.Stderr, loadedTeams, time.Since(startedAt))
			}
		}
	}

	if archiveMemory && !newSession {
		for _, tc := range loadedTeams {
			archiveCurrentSessionToMemory(ctx, tc)
		}
	}

	if tempWorkspace {
		absWS, _ := filepath.Abs(workspace)
		fmt.Fprintf(os.Stderr, "\n%s\n  Path: %s\n",
			boldStyle.Render("─── Temporary Workspace ───"),
			absWS)
	}

	if originalPrompt != "" {
		savePromptToHistory(ctx, originalPrompt, providerURL)
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
	_, _ = fmt.Fprint(w, formatExecutionSummary(summary, duration))
}

func formatExecutionSummary(summary executionSummary, duration time.Duration) string {
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
	return b.String()
}
