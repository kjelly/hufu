package main

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/kjelly/hufu/internal/team"
)

type historicalRunIntakeKey struct{}
type historicalRunIntake struct {
	segments []team.PromptSegment
	registry *team.TeamRegistry
	prompt   string
	prepared *team.PreparedHistoricalResultRequest
}

func resolveHistoricalRunOptions(command *cobra.Command, resolved *runOptions) error {
	if len(resolved.inheritResults) == 0 {
		return nil
	}
	if command.Name() != "run" || !resolved.canonicalRun || resolved.intent != "execute" ||
		strings.TrimSpace(resolved.agentTeamName) == "" || resolved.defaultTeam || resolved.autoTeam ||
		resolved.newSession || resolved.tempWorkspace || resolved.dryRun || resolved.resumeDecision != "" {
		return fmt.Errorf("--inherit-result requires a new execute request with one explicit --team; new, temp, dry-run and recovery entry points cannot add grants")
	}
	switch resolved.routeMode {
	case "", "auto", "team":
		resolved.routeMode = "team"
	case "fast":
		return fmt.Errorf("--inherit-result requires the coordinator route; --route fast is unsupported")
	default:
		return fmt.Errorf("--route must be auto, fast, or team")
	}
	return nil
}

func historicalRunContext(ctx context.Context, segments []team.PromptSegment, registry *team.TeamRegistry) (context.Context, error) {
	if len(opts.inheritResults) == 0 {
		return ctx, nil
	}
	if len(segments) != 1 || segments[0].Type != team.SegmentSwitchTeam || segments[0].Name != opts.agentTeamName ||
		segments[0].IsPiped || strings.TrimSpace(segments[0].Content) == "" {
		return nil, fmt.Errorf("--inherit-result requires one new request for the explicitly selected team")
	}
	return context.WithValue(ctx, historicalRunIntakeKey{}, &historicalRunIntake{segments: slices.Clone(segments), registry: registry}), nil
}

// This runs after policy freeze and before provider introspection. Agent
// mentions are resolved using the existing parser, never guessed from prose.
func prepareHistoricalRunIntake(ctx context.Context, coordinator *team.Coordinator, session *team.TeamSession) error {
	intake, _ := ctx.Value(historicalRunIntakeKey{}).(*historicalRunIntake)
	if intake == nil {
		if len(opts.inheritResults) != 0 {
			return fmt.Errorf("historical result intake is unavailable for this entry point")
		}
		return nil
	}
	if intake.prepared != nil {
		return fmt.Errorf("historical result intake cannot load another team")
	}
	expanded, err := expandSegmentsWithAgents(intake.segments, map[string]*teamContext{
		opts.agentTeamName: {session: session},
	}, intake.registry)
	if err != nil {
		return err
	}
	if len(expanded) != 1 || expanded[0].Type != team.SegmentSwitchTeam || expanded[0].IsPiped ||
		expanded[0].Name != opts.agentTeamName || strings.TrimSpace(expanded[0].Content) == "" {
		return fmt.Errorf("--inherit-result does not support direct-agent or piped requests")
	}
	intake.prompt = expanded[0].Content
	intake.prepared, err = coordinator.PrepareHistoricalResultRequest(ctx, intake.prompt, opts.inheritResults)
	return err
}

func bindHistoricalRunIntake(ctx context.Context, segments []team.PromptSegment) (context.Context, error) {
	intake, _ := ctx.Value(historicalRunIntakeKey{}).(*historicalRunIntake)
	if intake == nil {
		return ctx, nil
	}
	if intake.prepared == nil || len(segments) != 1 || segments[0].Type != team.SegmentSwitchTeam ||
		segments[0].Content != intake.prompt || segments[0].IsPiped {
		return nil, fmt.Errorf("historical result request changed after preflight")
	}
	return team.WithHistoricalResultRequest(ctx, intake.prepared), nil
}
