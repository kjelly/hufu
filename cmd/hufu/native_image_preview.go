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
		}
		if strings.TrimSpace(segment.Content) == "" {
			continue
		}
		tc := loadedTeams[selected]
		if tc == nil || tc.coordinator == nil {
			return fmt.Errorf("image preview requires one loaded team")
		}
		var preview team.NativeImagePreview
		var err error
		if segment.Type == team.SegmentInvokeAgent {
			preview, err = tc.coordinator.PreviewNativeImageTask(ctx, segment.Name, segment.Content, team.MediaTurnRequest{Images: opts.imagePaths})
		} else {
			preview, err = tc.coordinator.PreviewCoordinatorImageInput(ctx, segment.Content, team.MediaTurnRequest{Images: opts.imagePaths})
		}
		if err != nil {
			return err
		}
		if preview.MetadataOnly {
			stderrLog("Image preview: %d image(s), %d bytes, coordinator %s receives metadata only. Worker selection and vision capability remain unverified; no input has been admitted.\n", preview.ImageCount, preview.TotalBytes, preview.Target.String())
		} else {
			stderrLog("Image preview: %d image(s), %d bytes, target %s. Local validation only; model vision capability is unverified and no input has been admitted.\n", preview.ImageCount, preview.TotalBytes, preview.Target.String())
		}
		return nil
	}
	return fmt.Errorf("image preview requires one nonempty coordinator or explicit worker task")
}
