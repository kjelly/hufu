package main

import (
	"fmt"
	"strings"

	"github.com/kjelly/hufu/internal/team"
)

func writeImageResultReport(out *strings.Builder, data *reportData) {
	if data.ReportSource == nil || len(data.ReportSource.ImageResultIDs) == 0 {
		return
	}
	out.WriteString("## Published Images\n\n")
	for _, item := range data.Todos {
		if item == nil {
			continue
		}
		id := data.ReportSource.ImageResultIDs[item.ID]
		if id == "" {
			continue
		}
		fmt.Fprintf(out, "- Task `%s`: `%s`\n", reportSafeMetadata(item.ID, 120), team.HistoricalResultImagesCommand(id, data.ReportSource.Team))
		for _, image := range team.TaskResultImages(item) {
			fmt.Fprintf(out, "  - `%s` · %s · %dx%d · %d bytes\n", image.ID, image.MediaType, image.Width, image.Height, image.ByteSize)
		}
	}
	out.WriteString("\nExport an original image with `--artifact <image-id> --export <new-path>`. Existing files are never replaced.\n\n")
}
