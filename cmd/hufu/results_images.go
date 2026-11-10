package main

import (
	"encoding/json/v2"
	"fmt"
	"slices"

	inspectpkg "github.com/kjelly/hufu/internal/inspect"
	"github.com/kjelly/hufu/internal/team"
	"github.com/spf13/cobra"
)

type resultImagesEnvelope struct {
	SchemaVersion int                `json:"schema_version"`
	ResultID      string             `json:"result_id"`
	RunID         string             `json:"run_id"`
	TaskID        string             `json:"task_id"`
	BranchID      string             `json:"branch_id"`
	SourceEventID string             `json:"source_event_id"`
	SourceHash    string             `json:"source_hash"`
	Images        []team.ResultImage `json:"images"`
	ExportedTo    string             `json:"exported_to,omitempty"`
}

func presentResultImages(command *cobra.Command, options *resultsCLIOptions, deps resultsDeps, row inspectpkg.HistoricalResult) error {
	if row.TaskID == "" {
		return resultsExit(2, fmt.Errorf("images requires a saved task result"))
	}
	if options.exportPath != "" && options.imageID == "" {
		return resultsExit(2, fmt.Errorf("--export requires --artifact with an exact image ID"))
	}
	scope := team.ResultImageScope{BranchID: row.SourceBranchID, RunID: row.RunID, TaskID: row.TaskID, EventID: row.SourceEventID, EventHash: row.SourceHash}
	list := deps.images
	if list == nil {
		list = team.ListResultImages
	}
	images, err := list(command.Context(), row.Workspace, scope)
	if err != nil {
		return resultsExit(2, err)
	}
	if options.imageID != "" {
		index := slices.IndexFunc(images, func(image team.ResultImage) bool { return image.Image.ID == options.imageID })
		if index < 0 {
			return resultsExit(2, fmt.Errorf("image is not published by the selected result"))
		}
		images = images[index : index+1]
	}
	envelope := resultImagesEnvelope{SchemaVersion: 1, ResultID: row.ResultID, RunID: row.RunID, TaskID: row.TaskID, BranchID: row.SourceBranchID, SourceEventID: row.SourceEventID, SourceHash: row.SourceHash, Images: images}
	if options.exportPath != "" {
		export := deps.exportImage
		if export == nil {
			export = team.ExportResultImage
		}
		if err := export(command.Context(), row.Workspace, scope, options.imageID, options.exportPath); err != nil {
			return resultsExit(2, err)
		}
		envelope.ExportedTo = options.exportPath
	}
	if options.output == "json" {
		encoded, err := json.Marshal(envelope)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(command.OutOrStdout(), string(encoded))
		return err
	}
	for _, image := range images {
		ref := image.Image
		if _, err := fmt.Fprintf(command.OutOrStdout(), "%s  %s  %dx%d  %d bytes  occurrences=%d\n", ref.ID, ref.MediaType, ref.Width, ref.Height, ref.ByteSize, image.Occurrences); err != nil {
			return err
		}
	}
	if envelope.ExportedTo != "" {
		_, err = fmt.Fprintf(command.OutOrStdout(), "Exported to %s\n", safeResultsText(envelope.ExportedTo))
	} else if len(images) == 0 {
		_, err = fmt.Fprintln(command.OutOrStdout(), "No published tool images in this saved result.")
	} else {
		_, err = fmt.Fprintf(command.OutOrStdout(), "Export an original image: hufu results images %s --artifact <image-id> --export <new-path>\n", row.ResultID)
	}
	return err
}
