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

// dispatchSegmentContent executes a segment's text content via the fast path
// (single-agent direct dispatch with auto-escalation) or the team path
// (coordinator DAG), based on the resolved route decision. It manages the
// activeCoordinator pointer around the execution entry points so TUI/SIGINT
// tracking and prompt injection remain consistent across both paths.
func dispatchSegmentContent(ctx context.Context, tc *teamContext, content string, route RouteDecision, injector *promptInjector, activeCoord *activeCoordinator) (string, error) {
	if shouldUseFastPath(route, tc.coordinator) {
		primary := tc.coordinator.PrimaryWorkerName()
		d := fastPathDispatch{
			runDirect: func(ctx context.Context, agentName, task string) (*team.DirectAgentResult, error) {
				if injector.IsWrapUpRequested() {
					tc.coordinator.SetWrapUp()
				}
				activeCoord.Store(tc.coordinator)
				res, err := tc.coordinator.RunDirectAgent(ctx, agentName, task)
				activeCoord.Store(nil)
				return res, err
			},
			canEscalate: NewExecutionRouter(nil, nil).CanEscalateToTeam,
		}
		o := runFastPath(ctx, primary, content, route, d)
		if o.err != nil {
			return "", o.err
		}
		if o.attempted && !o.escalated {
			return o.output, nil
		}
		// Not attempted (no single worker) or escalated -> fall through to team path.
		if o.escalated {
			stderrLog("%s Falling back to team path (coordinator DAG).\n", dimStyle.Render("·"))
		}
	}
	activeCoord.Store(tc.coordinator)
	if injector.IsWrapUpRequested() {
		tc.coordinator.SetWrapUp()
	}
	result, err := tc.coordinator.Run(ctx, content)
	activeCoord.Store(nil)
	return result, err
}

// runDirectReplanThroughCoordinator promotes an explicit @agent invocation to
// the coordinator when the direct worker reaches the first no-progress
// threshold. Direct execution has no continuation loop of its own, so the
// original request must enter the normal coordinator path rather than being
// rendered as a standalone direct-agent error.
func runDirectReplanThroughCoordinator(ctx context.Context, tc *teamContext, content string, injector *promptInjector, activeCoord *activeCoordinator, runCoordinator func(context.Context, string) (string, error)) (string, error) {
	prompt := fmt.Sprintf("The direct-agent attempt reached the no-progress replan threshold. Continue this request through the team coordinator, reuse the completed direct-agent work where appropriate, and finish with the best final answer.\n\nOriginal request:\n%s", content)
	if activeCoord != nil {
		activeCoord.Store(tc.coordinator)
		defer activeCoord.Store(nil)
	}
	result, err := runCoordinator(ctx, prompt)
	if err != nil {
		return result, err
	}
	return runWithInjection(ctx, tc, result, injector)
}

type segmentDirectAgentRunner func(context.Context, *team.Coordinator, string, string) (*team.DirectAgentResult, error)
type segmentCoordinatorRunner func(context.Context, *team.Coordinator, string) (string, error)

// handleDirectAgentReplanResult processes a direct-agent result that surfaced
// ReplanRequired, promoting the execution to the coordinator. It returns the
// updated results slice, the replanned output, and any error.
func handleDirectAgentReplanResult(
	ctx context.Context,
	tc *teamContext,
	segName, segContent, currentTeamName string,
	results []string,
	injector *promptInjector,
	activeCoord *activeCoordinator,
	disp2 *coordDisplay,
	totalSegments, segIndex int,
	loadedTeams map[string]*teamContext,
	runCoordinator func(context.Context, *team.Coordinator, string) (string, error),
) ([]string, string, error) {
	stderrLog("\n%s %s reached the no-progress threshold; escalating to coordinator replan.\n", errStyle.Render("⚠"), agentStyle.Render(segName))
	replanned, replanErr := runDirectReplanThroughCoordinator(ctx, tc, segContent, injector, activeCoord, func(ctx context.Context, prompt string) (string, error) {
		return runCoordinator(ctx, tc.coordinator, prompt)
	})
	if replanErr != nil {
		return results, "", fmt.Errorf("direct agent @%s replan failed: %w", segName, replanErr)
	}
	disp2.finalizeTasks()
	results = append(results, fmt.Sprintf("## Agent: @%s (team: %s)\n%s", segName, currentTeamName, replanned))
	if segIndex == totalSegments-1 {
		if current := loadedTeams[currentTeamName]; current != nil && current.session != nil {
			_ = team.SaveSessionMD(current.session.Workspace, team.GenerateSessionMD(current.sessionData, current.session.Config.Name))
		}
	}
	return results, replanned, nil
}

// synthesizeDirectAgentResult synthesizes a direct agent result through the
// coordinator if an orchestrator is defined, otherwise returns the raw output.
func synthesizeDirectAgentResult(ctx context.Context, tc *teamContext, agentName, content, output string, injector *promptInjector, activeCoord *activeCoordinator) (string, error) {
	orchDef := tc.coordinator.GetOrchestratorDef()
	if orchDef == nil {
		return output, nil
	}
	synthesisPrompt := fmt.Sprintf("A user directly asked @%s to do the following task:\n\n%s\n\nHere is what %s produced:\n\n---\n%s\n---\n\nPlease synthesize this into a final, well-organized answer for the user.",
		agentName, content, agentName, output)
	activeCoord.Store(tc.coordinator)
	if injector.IsWrapUpRequested() {
		tc.coordinator.SetWrapUp()
	}
	synthResult, err := tc.coordinator.Run(ctx, synthesisPrompt)
	activeCoord.Store(nil)
	if err != nil {
		return "", err
	}
	synthResult, err = runWithInjection(ctx, tc, synthResult, injector)
	if err != nil {
		return "", err
	}
	return synthResult, nil
}

func executeSegments(ctx context.Context, segments []team.PromptSegment, registry *team.TeamRegistry, defaultProviderURL string, loadedTeams map[string]*teamContext, injector *promptInjector, activeCoord *activeCoordinator, pathConsent *tools.PathConsent, vars map[string]string, route RouteDecision) (string, error) {
	return executeSegmentsWithRunners(ctx, segments, registry, defaultProviderURL, loadedTeams, injector, activeCoord, pathConsent, vars, route,
		func(ctx context.Context, coordinator *team.Coordinator, agentName, task string) (*team.DirectAgentResult, error) {
			return coordinator.RunDirectAgent(ctx, agentName, task)
		},
		func(ctx context.Context, coordinator *team.Coordinator, prompt string) (string, error) {
			return coordinator.Run(ctx, prompt)
		})
}

func executeSegmentsWithRunners(ctx context.Context, segments []team.PromptSegment, registry *team.TeamRegistry, defaultProviderURL string, loadedTeams map[string]*teamContext, injector *promptInjector, activeCoord *activeCoordinator, pathConsent *tools.PathConsent, vars map[string]string, route RouteDecision, runDirect segmentDirectAgentRunner, runCoordinator segmentCoordinatorRunner) (string, error) {
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

			if route.Route == RouteFast {
				stderrLog("\n%s Fast path → team %s...\n\n", boldStyle.Render("→"), teamStyle.Render(teamName))
			} else {
				stderrLog("\n%s Starting team %s...\n\n", boldStyle.Render("→"), teamStyle.Render(teamName))
			}

			result, err := dispatchSegmentContent(ctx, tc, content, route, injector, activeCoord)
			disp.stopTimer()

			if err != nil {
				return handleSegmentError(ctx, tc, results, err, "team %q failed", teamName)
			}

			result, err = runWithInjection(ctx, tc, result, injector)
			if err != nil {
				return handleSegmentError(ctx, tc, results, err, "team %q continuation failed", teamName)
			}

			disp.finalizeTasks()
			if route.Route == RouteFast {
				stderrLog("\n%s Fast path complete.\n", doneStyle.Render("✓"))
			} else {
				stderrLog("\n%s Team %s coordination complete.\n", doneStyle.Render("✓"), teamStyle.Render(teamName))
			}
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
			directResult, err := runDirect(ctx, tc.coordinator, seg.Name, content)
			activeCoord.Store(nil)
			disp2.stopTimer()

			if err != nil {
				return handleSegmentError(ctx, tc, results, err, "direct agent @%s failed", seg.Name)
			}

			if directResult.ReplanRequired {
				var replanned string
				results, replanned, err = handleDirectAgentReplanResult(ctx, tc, seg.Name, content, currentTeamName, results, injector, activeCoord, disp2, len(segments), i, loadedTeams, runCoordinator)
				if err != nil {
					return "", err
				}
				prevResult = replanned
				continue
			}

			if directResult.Error != nil {
				stderrLog("\n%s %s failed: %s\n", errStyle.Render("✗"), agentStyle.Render(seg.Name), errStyle.Render(directResult.Error.Error()))
				results = append(results, fmt.Sprintf("## Agent: @%s\n**ERROR**: %s", seg.Name, directResult.Error))
				continue
			}

			stderrLog("\n%s %s completed, synthesizing...\n\n", doneStyle.Render("✓"), agentStyle.Render(seg.Name))

			synthResult, synthErr := synthesizeDirectAgentResult(ctx, tc, seg.Name, content, directResult.Output, injector, activeCoord)
			if synthErr != nil {
				return handleSegmentError(ctx, tc, results, synthErr, "synthesis for @%s failed", seg.Name)
			}
			disp2.finalizeTasks()
			results = append(results, fmt.Sprintf("## Agent: @%s (team: %s)\n%s", seg.Name, currentTeamName, synthResult))
			prevResult = synthResult

		case team.SegmentText:
			if currentTeamName == "" {
				return strings.Join(results, "\n\n"), fmt.Errorf("text segment with no active team — specify a team with --agent-team or @team-name first")
			}

			tc := loadedTeams[currentTeamName]

			disp3 := newCoordDisplay(tc)

			if route.Route == RouteFast {
				stderrLog("\n%s Fast path → team %s...\n\n", boldStyle.Render("→"), teamStyle.Render(currentTeamName))
			} else {
				stderrLog("\n%s Team %s processing...\n\n", boldStyle.Render("→"), teamStyle.Render(currentTeamName))
			}

			result, err := dispatchSegmentContent(ctx, tc, content, route, injector, activeCoord)
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
