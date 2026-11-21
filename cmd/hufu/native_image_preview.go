package main

import (
	"context"
	"fmt"
	"io"
	"strings"

	"github.com/kjelly/hufu/internal/team"
)

func executeNativeImageDryRun(ctx context.Context, writer io.Writer, segments []team.PromptSegment, loadedTeams map[string]*teamContext) error {
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
		return writeNativeImageDryRun(writer, selected, preview)
	}
	return fmt.Errorf("image preview requires one nonempty coordinator or explicit worker task")
}
