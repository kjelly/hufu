package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/anomalyco/hufu/internal/team"
	"github.com/anomalyco/hufu/internal/tools"
)

func handleSegmentError(ctx context.Context, tc *teamContext, results []string, err error, kind string, args ...any) (string, error) {
	if ctx.Err() == context.Canceled {
		if tc != nil {
			agentName, taskDesc, todoID, detail := tc.coordinator.GetLastFailureContext()
			if detail == "" {
				source := team.FailureSourceContextCanceled
				if tools.IsInteractiveAbortRequested() {
					source = team.FailureSourceSigint
				}
				detail = tc.coordinator.FailureDetail(err, source)
				if agentName == "" {
					agentName = "coordinator"
				}
				if taskDesc == "" {
					taskDesc = fmt.Sprintf(kind, args...)
				}
				tc.coordinator.PersistFailure(agentName, taskDesc, todoID, detail)
			}
			_ = team.SaveSession(tc.session.Workspace, tc.sessionData)
		}
		stderrLog("\n%s Interrupted\n", errStyle.Render("⚠"))
		return "", errInterrupted{}
	}
	if tc != nil {
		agentName, taskDesc, todoID, detail := tc.coordinator.GetLastFailureContext()
		if detail == "" {
			detail = tc.coordinator.FailureDetail(err, team.SegmentFailureSource(kind))
			if agentName == "" {
				agentName = "coordinator"
			}
			if taskDesc == "" {
				taskDesc = fmt.Sprintf(kind, args...)
			}
			tc.coordinator.PersistFailure(agentName, taskDesc, todoID, detail)
		}
		_ = team.SaveSession(tc.session.Workspace, tc.sessionData)
		_ = team.SaveSessionMD(tc.session.Workspace, team.GenerateSessionMD(tc.sessionData, tc.session.Config.Name))
	}
	return strings.Join(results, "\n\n"), fmt.Errorf(kind+": %w", append(args, err)...)
}

func executeSegments(ctx context.Context, segments []team.PromptSegment, registry *team.TeamRegistry, defaultProviderURL string, loadedTeams map[string]*teamContext, injector *promptInjector, activeCoord *activeCoordinator, pathConsent *tools.PathConsent, vars map[string]string) (string, error) {
	var results []string
	currentTeamName := ""
	var prevResult string

	for i, seg := range segments {
		content := seg.Content
		if seg.IsPiped && prevResult != "" {
			content = content + "\n\n" + prevResult
		} else if strings.Contains(content, "{{PREV_RESULT}}") {
			content = strings.ReplaceAll(content, "{{PREV_RESULT}}", prevResult)
		}

		switch seg.Type {
		case team.SegmentSwitchTeam:
			teamName := seg.Name
			tc, ok := loadedTeams[teamName]
			if !ok {
				loaded, err := loadTeamByName(ctx, teamName, registry, opts.providerURL, opts.providerAPIKey, pathConsent, vars, opts.forcedSkills, opts.planMode, opts.autoSkills)
				if err != nil {
					return strings.Join(results, "\n\n"), fmt.Errorf("failed to load team %q: %w", teamName, err)
				}
				if opts.stepsMode {
					loaded.coordinator.SetStepConfirmFn(makeStepConfirmFn())
				}
				tc = loaded
				loadedTeams[teamName] = tc
			}

			if currentTeamName != "" && currentTeamName != teamName {
				prevTC := loadedTeams[currentTeamName]
				if prevTC != nil {
					_ = team.SaveSessionMD(prevTC.session.Workspace, team.GenerateSessionMD(prevTC.sessionData, prevTC.session.Config.Name))
				}
				stderrLog("\n%s Switching team: %s → %s\n\n", boldStyle.Render("⇒"), teamStyle.Render(currentTeamName), teamStyle.Render(teamName))
			}

			currentTeamName = teamName

			if opts.isChatTUI {
				disp := newCoordDisplay(tc)
				activeCoord.Store(tc.coordinator)
				result, err := runWithInjection(ctx, tc, "", injector)
				activeCoord.Store(nil)
				disp.stopTimer()
				disp.finalizeTasks()
				if err != nil {
					return handleSegmentError(ctx, tc, results, err, "chat session failed")
				}
				results = append(results, result)
				continue
			}

			if content == "" {
				continue
			}

			disp := newCoordDisplay(tc)

			stderrLog("\n%s Starting team %s...\n\n", boldStyle.Render("→"), teamStyle.Render(teamName))

			activeCoord.Store(tc.coordinator)
			if injector.IsWrapUpRequested() {
				tc.coordinator.SetWrapUp()
			}
			result, err := tc.coordinator.Run(ctx, content)
			activeCoord.Store(nil)
			disp.stopTimer()

			if err != nil {
				return handleSegmentError(ctx, tc, results, err, "team %q failed", teamName)
			}

			result, err = runWithInjection(ctx, tc, result, injector)
			if err != nil {
				return handleSegmentError(ctx, tc, results, err, "team %q continuation failed", teamName)
			}

			disp.finalizeTasks()
			stderrLog("\n%s Team %s coordination complete.\n", doneStyle.Render("✓"), teamStyle.Render(teamName))
			results = append(results, fmt.Sprintf("## Team: %s\n%s", teamName, result))
			prevResult = result

		case team.SegmentInvokeAgent:
			if currentTeamName == "" {
				return strings.Join(results, "\n\n"), fmt.Errorf("@%s — no active team. Specify a team with --agent-team or @team-name first", seg.Name)
			}

			tc := loadedTeams[currentTeamName]
			if tc == nil {
				return strings.Join(results, "\n\n"), fmt.Errorf("@%s — team %q not loaded", seg.Name, currentTeamName)
			}

			disp2 := newCoordDisplay(tc)

			stderrLog("\n%s Direct invocation: @%s (team: %s)\n\n", boldStyle.Render("→"), agentStyle.Render(seg.Name), teamStyle.Render(currentTeamName))

			activeCoord.Store(tc.coordinator)
			if injector.IsWrapUpRequested() {
				tc.coordinator.SetWrapUp()
			}
			directResult, err := tc.coordinator.RunDirectAgent(ctx, seg.Name, content)
			activeCoord.Store(nil)
			disp2.stopTimer()

			if err != nil {
				return handleSegmentError(ctx, tc, results, err, "direct agent @%s failed", seg.Name)
			}

			if directResult.Error != nil {
				stderrLog("\n%s %s failed: %s\n", errStyle.Render("✗"), agentStyle.Render(seg.Name), errStyle.Render(directResult.Error.Error()))
				results = append(results, fmt.Sprintf("## Agent: @%s\n**ERROR**: %s", seg.Name, directResult.Error))
				continue
			}

			stderrLog("\n%s %s completed, synthesizing...\n\n", doneStyle.Render("✓"), agentStyle.Render(seg.Name))

			orchDef := tc.coordinator.GetOrchestratorDef()
			if orchDef == nil {
				disp2.finalizeTasks()
				results = append(results, fmt.Sprintf("## Agent: @%s (team: %s)\n%s", seg.Name, currentTeamName, directResult.Output))
				prevResult = directResult.Output
			} else {
				synthesisPrompt := fmt.Sprintf("A user directly asked @%s to do the following task:\n\n%s\n\nHere is what %s produced:\n\n---\n%s\n---\n\nPlease synthesize this into a final, well-organized answer for the user.",
					seg.Name, content, seg.Name, directResult.Output)
				activeCoord.Store(tc.coordinator)
				if injector.IsWrapUpRequested() {
					tc.coordinator.SetWrapUp()
				}
				synthResult, err := tc.coordinator.Run(ctx, synthesisPrompt)
				activeCoord.Store(nil)
				if err != nil {
					return handleSegmentError(ctx, tc, results, err, "synthesis for @%s failed", seg.Name)
				}

				synthResult, err = runWithInjection(ctx, tc, synthResult, injector)
				if err != nil {
					return handleSegmentError(ctx, tc, results, err, "synthesis continuation for @%s failed", seg.Name)
				}

				disp2.finalizeTasks()
				results = append(results, fmt.Sprintf("## Agent: @%s (team: %s)\n%s", seg.Name, currentTeamName, synthResult))
				prevResult = synthResult
			}

		case team.SegmentText:
			if currentTeamName == "" {
				return strings.Join(results, "\n\n"), fmt.Errorf("text segment with no active team — specify a team with --agent-team or @team-name first")
			}

			tc := loadedTeams[currentTeamName]

			disp3 := newCoordDisplay(tc)

			stderrLog("\n%s Team %s processing...\n\n", boldStyle.Render("→"), teamStyle.Render(currentTeamName))

			activeCoord.Store(tc.coordinator)
			if injector.IsWrapUpRequested() {
				tc.coordinator.SetWrapUp()
			}
			result, err := tc.coordinator.Run(ctx, content)
			activeCoord.Store(nil)
			disp3.stopTimer()

			if err != nil {
				return handleSegmentError(ctx, tc, results, err, "team %q failed", currentTeamName)
			}

			result, err = runWithInjection(ctx, tc, result, injector)
			if err != nil {
				return handleSegmentError(ctx, tc, results, err, "team %q continuation failed", currentTeamName)
			}

			disp3.finalizeTasks()
			results = append(results, fmt.Sprintf("## Team: %s\n%s", currentTeamName, result))
			prevResult = result
		}

		if i == len(segments)-1 {
			if tc, ok := loadedTeams[currentTeamName]; ok {
				_ = team.SaveSessionMD(tc.session.Workspace, team.GenerateSessionMD(tc.sessionData, tc.session.Config.Name))
			}
		}
	}

	if len(results) == 0 {
		return "", nil
	}
	if len(results) == 1 {
		return results[0], nil
	}
	return strings.Join(results, "\n\n---\n\n"), nil
}
