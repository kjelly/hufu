package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/kjelly/hufu/internal/media"
	"github.com/kjelly/hufu/internal/team"
)

// Resolve the final segment scope before binding any retained run inputs.
func bindRunSegmentInputs(ctx context.Context, segments []team.PromptSegment) (context.Context, error) {
	if len(opts.imagePaths) > 0 {
		if err := validateNativeImageSegments(segments); err != nil {
			return nil, err
		}
	}
	return bindHistoricalRunIntake(ctx, segments)
}

func validateNativeImageRunFlags(options runOptions) error {
	if len(options.imagePaths) == 0 {
		return nil
	}
	if len(options.imagePaths) > media.MaxCount {
		return media.Reject(media.LimitExceeded, "too many --image values")
	}
	if options.autoTeam || (!options.defaultTeam && strings.TrimSpace(options.agentTeamName) == "") {
		return fmt.Errorf("--image requires an explicit --team/--agent-team or --default; --auto-team is unsupported")
	}
	if options.routeMode == "fast" || options.intent == "decision" || options.resumeDecision != "" || options.archiveMemory || len(options.inheritResults) > 0 {
		return fmt.Errorf("--image requires a direct worker execution, without fast routing, decision resume or memory archival")
	}
	for _, path := range options.imagePaths {
		if strings.TrimSpace(path) == "" {
			return fmt.Errorf("--image requires a nonempty local file path")
		}
	}
	return nil
}

func validateNativeImageSegments(segments []team.PromptSegment) error {
	selectedTeam := ""
	executions := 0
	for _, segment := range segments {
		if segment.Type == team.SegmentSwitchTeam {
			if selectedTeam != "" && selectedTeam != segment.Name {
				return fmt.Errorf("--image supports exactly one team")
			}
			selectedTeam = segment.Name
		}
		if strings.TrimSpace(segment.Content) == "" {
			continue
		}
		if segment.Type != team.SegmentInvokeAgent || strings.TrimSpace(segment.Name) == "" || segment.IsPiped {
			return fmt.Errorf("--image requires one explicit @worker with a nonempty task")
		}
		executions++
	}
	if executions != 1 {
		return fmt.Errorf("--image supports exactly one nonempty @worker execution")
	}
	return nil
}
