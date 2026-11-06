package team

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"slices"

	"github.com/kjelly/hufu/internal/media"
)

// ResultImageScope selects one immutable terminal result in a verified branch.
// This is an operator API. It does not grant model-facing artifact access.
type ResultImageScope struct {
	BranchID  string
	RunID     string
	TaskID    string
	EventID   string
	EventHash string
}

type ResultImageSource struct {
	PublicationHash  string `json:"publication_hash"`
	RunID            string `json:"run_id"`
	ToolCallID       string `json:"tool_call_id"`
	ToolInvocationID string `json:"tool_invocation_id"`
	ToolName         string `json:"tool_name"`
	ModelExecutionID string `json:"model_execution_id,omitempty"`
}

// ResultImage describes original image content; repeated appearances retain
// their occurrence count and independently verified producing invocations.
type ResultImage struct {
	Image       media.Ref           `json:"image"`
	Occurrences int                 `json:"occurrences"`
	Sources     []ResultImageSource `json:"sources"`
}

// TaskResultImages is a metadata projection for reports and UI. It grants no
// byte access; exports still require the immutable result scope above.
func TaskResultImages(item *TodoItem) []media.Ref {
	if item == nil {
		return nil
	}
	var refs []media.Ref
	seen := make(map[string]bool)
	include := func(result *TaskResult) {
		if result != nil {
			refs = projectionImageRefs(result.RuntimeMedia, refs, seen)
		}
	}
	include(item.TypedResult)
	for _, receipt := range item.ExecutionReceipts {
		include(receipt.AcceptedResult)
	}
	if item.ExecutionReceipt != nil {
		include(item.ExecutionReceipt.AcceptedResult)
	}
	return refs
}

func projectionImageRefs(projection *RuntimeMediaProjection, refs []media.Ref, seen map[string]bool) []media.Ref {
	if projection == nil {
		return refs
	}
	for _, source := range projection.Sources {
		for _, part := range source.Parts {
			if part.Type != "image" || seen[part.Image.Ref.ID] {
				continue
			}
			seen[part.Image.Ref.ID] = true
			refs = append(refs, part.Image.Ref)
		}
	}
	return refs
}

// Decode only the new image fields. Legacy result/receipt duration formats
// remain owned by their existing codecs and do not affect a metadata view.
func resultEventImageRefs(event RunEvent) ([]media.Ref, error) {
	type resultMetadata struct {
		Projection *RuntimeMediaProjection `json:"runtime_media"`
	}
	type receiptMetadata struct {
		Accepted *resultMetadata `json:"accepted_result"`
	}
	var payload struct {
		Result   resultMetadata    `json:"typed_result"`
		Receipt  receiptMetadata   `json:"execution_receipt"`
		Receipts []receiptMetadata `json:"execution_receipts"`
	}
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return nil, err
	}
	seen := make(map[string]bool)
	refs := projectionImageRefs(payload.Result.Projection, nil, seen)
	for _, receipt := range append(payload.Receipts, payload.Receipt) {
		if receipt.Accepted != nil {
			refs = projectionImageRefs(receipt.Accepted.Projection, refs, seen)
		}
	}
	return refs, nil
}

func reportImageResultIDs(events []RunEvent, runID string) (map[string]string, error) {
	imageTasks := make(map[string]bool)
	latest := make(map[string]string)
	for _, event := range events {
		if event.RunID != runID || event.TaskID == "" {
			continue
		}
		if event.Type == string(EventToolMediaPublished) {
			if _, err := decodeToolMediaPublication(event); err != nil {
				return nil, err
			}
			imageTasks[event.TaskID] = true
		}
		if !IsTerminalEvent(event.Type) {
			continue
		}
		refs, err := resultEventImageRefs(event)
		if err != nil {
			return nil, err
		}
		if len(refs) != 0 {
			imageTasks[event.TaskID] = true
		}
		latest[event.TaskID] = HistoricalResultID(event)
	}
	var ids map[string]string
	for taskID := range imageTasks {
		if id := latest[taskID]; id != "" {
			if ids == nil {
				ids = make(map[string]string)
			}
			ids[taskID] = id
		}
	}
	return ids, nil
}

func ListResultImages(ctx context.Context, workspace string, scope ResultImageScope) ([]ResultImage, error) {
	if workspace == "" || scope.BranchID == "" || scope.RunID == "" || scope.TaskID == "" || scope.EventID == "" || scope.EventHash == "" {
		return nil, media.Reject(media.AccessDenied, "complete image result scope is required")
	}
	events, err := readValidatedEventJournal(ctx, workspace)
	if err != nil {
		return nil, err
	}
	tree, err := LoadSessionTreeContext(ctx, workspace)
	if err != nil {
		return nil, err
	}
	if _, exists := tree.Branches[scope.BranchID]; !exists {
		return nil, media.Reject(media.AccessDenied, "image result branch is unavailable")
	}
	lineage, err := ProjectValidatedEventsForBranch(events, tree, scope.BranchID)
	if err != nil {
		return nil, err
	}
	index := slices.IndexFunc(lineage, func(e RunEvent) bool { return e.ID == scope.EventID })
	if index < 0 {
		return nil, media.Reject(media.AccessDenied, "image result is outside the selected lineage")
	}
	terminal := lineage[index]
	if !IsTerminalEvent(terminal.Type) || terminal.TaskID != scope.TaskID || terminal.RunID != scope.RunID || terminal.Hash != scope.EventHash {
		return nil, media.Reject(media.AccessDenied, "image result identity differs from the selected terminal event")
	}
	lineage = lineage[:index+1]
	if err := VerifyNativeMediaLineage(ctx, workspace, lineage); err != nil {
		return nil, err
	}
	retained, err := retainedResultMediaPublications(terminal, lineage[:index])
	if err != nil {
		return nil, err
	}
	images := []ResultImage{}
	byID := make(map[string]int)
	for _, event := range lineage {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if event.Type != string(EventToolMediaPublished) || event.TaskID != scope.TaskID {
			continue
		}
		producer, err := decodeToolMediaPublication(event)
		if err != nil {
			return nil, err
		}
		if event.RunID != scope.RunID && !retained[producer.Hash] {
			continue
		}
		source := ResultImageSource{PublicationHash: producer.Hash, RunID: event.RunID, ToolCallID: producer.ToolCallID,
			ToolInvocationID: producer.ToolInvocationID, ToolName: producer.ToolName, ModelExecutionID: producer.ModelFanout.ModelExecutionID}
		for _, occurrence := range producer.Snapshot.Occurrences {
			var ref media.Ref
			for _, candidate := range producer.Snapshot.Refs {
				if candidate.ID == occurrence.MediaID {
					ref = candidate
					break
				}
			}
			if ref.ID == "" {
				return nil, media.Reject(media.IntegrityMismatch, "image occurrence has no canonical content")
			}
			position, exists := byID[ref.ID]
			if !exists {
				position = len(images)
				byID[ref.ID] = position
				images = append(images, ResultImage{Image: ref})
			} else if images[position].Image != ref {
				return nil, media.Reject(media.IntegrityMismatch, "image result has conflicting content metadata")
			}
			images[position].Occurrences++
			if !slices.Contains(images[position].Sources, source) {
				images[position].Sources = append(images[position].Sources, source)
			}
		}
	}
	return images, nil
}

func retainedResultMediaPublications(event RunEvent, prior []RunEvent) (map[string]bool, error) {
	var payload struct {
		Result   *TaskResult        `json:"typed_result"`
		Receipt  *ExecutionReceipt  `json:"execution_receipt"`
		Receipts []ExecutionReceipt `json:"execution_receipts"`
	}
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		return nil, err
	}
	retained := make(map[string]bool)
	receipts := append([]ExecutionReceipt(nil), payload.Receipts...)
	if payload.Receipt != nil {
		receipts = append(receipts, *payload.Receipt)
	}
	include := func(result *TaskResult, receipt *ExecutionReceipt) error {
		if result == nil || result.RuntimeMedia == nil {
			return nil
		}
		if err := verifyRuntimeMediaResult(result, receipt, prior); err != nil {
			return err
		}
		for _, source := range result.RuntimeMedia.Sources {
			retained[source.PublicationHash] = true
		}
		return nil
	}
	if payload.Result != nil && payload.Result.RuntimeMedia != nil {
		var receipt *ExecutionReceipt
		for i := range receipts {
			if receipts[i].RuntimeMediaHash == payload.Result.RuntimeMedia.Hash {
				receipt = &receipts[i]
			}
		}
		if err := include(payload.Result, receipt); err != nil {
			return nil, err
		}
	}
	for i := range receipts {
		if err := include(receipts[i].AcceptedResult, &receipts[i]); err != nil {
			return nil, err
		}
	}
	return retained, nil
}

// ExportResultImage verifies result scope and original bytes before creating
// an explicit new destination. Existing paths and symlinks are never replaced.
func ExportResultImage(ctx context.Context, workspace string, scope ResultImageScope, id, destination string) error {
	images, err := ListResultImages(ctx, workspace, scope)
	if err != nil {
		return err
	}
	index := slices.IndexFunc(images, func(image ResultImage) bool { return image.Image.ID == id })
	if index < 0 {
		return media.Reject(media.AccessDenied, "image is not published by the selected result")
	}
	store, err := OpenFileArtifactStoreReadOnly(workspace, workspace)
	if err != nil {
		return err
	}
	reader, err := store.Open(ctx, id)
	if err != nil {
		return err
	}
	defer func() { _ = reader.Close() }()
	if err := writeExclusiveImage(ctx, destination, reader); err != nil {
		return fmt.Errorf("export image: %w", err)
	}
	return nil
}
