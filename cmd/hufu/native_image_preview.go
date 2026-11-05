package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/kjelly/hufu/internal/team"
)

func executeNativeImageDryRun(ctx context.Context, segments []team.PromptSegment, loadedTeams map[string]*teamContext) error {
	if err := validateNativeImageSegments(segments); err != nil {
		return err
	}
	selected := strings.ToLower(opts.agentTeamName)
	for _, segment := range segments {
		if segment.Type == team.SegmentSwitchTeam {
			selected = segment.Name
			continue
		}
		if segment.Type != team.SegmentInvokeAgent || strings.TrimSpace(segment.Content) == "" {
			continue
		}
		tc := loadedTeams[selected]
		if tc == nil || tc.coordinator == nil {
			return fmt.Errorf("image preview requires one loaded team")
		}
		preview, err := tc.coordinator.PreviewNativeImageTask(ctx, segment.Name, segment.Content, team.MediaTurnRequest{Images: opts.imagePaths})
		if err != nil {
			return err
		}
		stderrLog("Image preview: %d image(s), %d bytes, target %s. Local validation only; model vision capability is unverified and no input has been admitted.\n", preview.ImageCount, preview.TotalBytes, preview.Target.String())
		return nil
	}
	return fmt.Errorf("image preview requires one explicit worker task")
}
