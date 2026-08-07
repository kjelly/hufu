package team

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"charm.land/fantasy"
)

type submitResultTool struct {
	coordinator *Coordinator
	todoID      string
}

func (t *submitResultTool) ProviderOptions() fantasy.ProviderOptions {
	return fantasy.ProviderOptions{}
}
func (t *submitResultTool) SetProviderOptions(opts fantasy.ProviderOptions) {}

func (t *submitResultTool) Info() fantasy.ToolInfo {
	return fantasy.ToolInfo{
		Name:        "submit_result",
		Description: "Submit the terminal structured result for your assigned task. Call exactly once when finished. status=success or completed_with_gaps marks the task done; completed_with_gaps means the assigned work is complete but it discovered a target-system limitation. Use partial, failed, or blocked when the assigned work itself was not completed.",
		Parameters: map[string]any{
			"status": map[string]any{
				"type": "string", "enum": []string{TaskResultStatusSuccess, TaskResultStatusCompletedWithGaps, TaskResultStatusPartial, TaskResultStatusFailed, TaskResultStatusBlocked},
				"description": "Terminal outcome of the assigned goal",
			},
			"summary": map[string]any{
				"type":        "string",
				"description": "High-level summary of the task result and accomplishments.",
			},
			"artifacts": map[string]any{
				"type":        "array",
				"description": "List of regular files already created inside the team workspace. Hufu snapshots each file when this result is submitted; do not declare planned or external files.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path":        map[string]any{"type": "string"},
						"description": map[string]any{"type": "string"},
						"type":        map[string]any{"type": "string"},
					},
					"required": []string{"path"},
				},
			},
			"raw_output_ref": map[string]any{
				"type":        "object",
				"description": "Runner-owned complete tool transcript. For output_mode=verbatim hufu fills and verifies this field automatically; workers must not invent it.",
				"properties": map[string]any{
					"path":        map[string]any{"type": "string"},
					"description": map[string]any{"type": "string"},
					"type":        map[string]any{"type": "string"},
					"sha256":      map[string]any{"type": "string"},
					"bytes":       map[string]any{"type": "integer"},
				},
			},
			"files_read": map[string]any{
				"type":        "array",
				"description": "List of files read during the task.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path":    map[string]any{"type": "string"},
						"purpose": map[string]any{"type": "string"},
					},
					"required": []string{"path"},
				},
			},
			"files_modified": map[string]any{
				"type":        "array",
				"description": "List of files modified or created during the task.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"path":    map[string]any{"type": "string"},
						"purpose": map[string]any{"type": "string"},
					},
					"required": []string{"path"},
				},
			},
			"decisions": map[string]any{
				"type":        "array",
				"description": "Key technical decisions made during execution.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"topic":  map[string]any{"type": "string"},
						"choice": map[string]any{"type": "string"},
						"reason": map[string]any{"type": "string"},
					},
					"required": []string{"topic", "choice"},
				},
			},
			"findings": map[string]any{
				"type":        "array",
				"description": "Key findings or insights discovered.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"category": map[string]any{"type": "string"},
						"summary":  map[string]any{"type": "string"},
						"detail":   map[string]any{"type": "string"},
					},
					"required": []string{"summary"},
				},
			},
			"risks": map[string]any{
				"type":        "array",
				"description": "Identified risks or concerns.",
				"items": map[string]any{
					"type": "object",
					"properties": map[string]any{
						"description": map[string]any{"type": "string"},
						"impact":      map[string]any{"type": "string"},
						"mitigation":  map[string]any{"type": "string"},
					},
					"required": []string{"description"},
				},
			},
			"open_questions": map[string]any{
				"type":        "array",
				"description": "Unresolved questions or follow-ups.",
				"items":       map[string]any{"type": "string"},
			},
			"retry_hint": map[string]any{
				"type":        "string",
				"description": "Feedback or hint if this result is part of a retry or indicates partial failure.",
			},
			"confidence": map[string]any{
				"type":        "number",
				"description": "Self-assessed confidence level (0.0 to 1.0).",
			},
		},
		Required: []string{"status", "summary"},
	}
}

func (t *submitResultTool) Run(ctx context.Context, call fantasy.ToolCall) (fantasy.ToolResponse, error) {
	var res TaskResult
	if err := json.Unmarshal([]byte(call.Input), &res); err != nil {
		return fantasy.NewTextErrorResponse(fmt.Sprintf("invalid submit_result arguments: %v", err)), nil
	}
	if res.Summary == "" {
		return fantasy.NewTextErrorResponse("summary is required"), nil
	}
	switch strings.ToLower(strings.TrimSpace(res.Status)) {
	case TaskResultStatusSuccess, TaskResultStatusCompletedWithGaps, TaskResultStatusPartial, TaskResultStatusFailed, TaskResultStatusBlocked:
		res.Status = strings.ToLower(strings.TrimSpace(res.Status))
	default:
		return fantasy.NewTextErrorResponse("status must be success, completed_with_gaps, partial, failed, or blocked"), nil
	}

	res.TaskID = t.todoID
	res.Source = "submitted"
	if res.Confidence == 0 {
		res.Confidence = 1.0
	}
	// Only a success claim can be mechanically contradicted by terminal
	// evidence — completed_with_gaps reports a target limitation rather than a
	// failed task, while partial/failed/blocked already say the task itself is
	// not done, so
	// there is nothing here for terminal evidence to override. Rejecting the
	// claim in the tool response (rather than only at round-end) lets the
	// model see the contradiction immediately and reconsider within the same
	// round instead of finding out only after it believed it had succeeded.
	if res.Status == "success" && t.coordinator != nil {
		if err := t.coordinator.terminalTaskFailure(ctx, t.todoID); err != nil {
			return fantasy.NewTextErrorResponse("success rejected: " + err.Error()), nil
		}
	}
	// Security: strip model-injected HMAC signatures from submit_result tool input
	for i := range res.Evidence {
		res.Evidence[i].SystemHMAC = ""
	}

	if t.coordinator != nil {
		if err := t.materializeSubmittedArtifacts(ctx, &res); err != nil {
			return fantasy.NewTextErrorResponse("invalid artifact declaration: " + err.Error()), nil
		}
		t.coordinator.storeSubmittedTaskResult(t.todoID, &res)
	}
	return fantasy.NewTextResponse("Task result submitted successfully."), nil
}

// materializeSubmittedArtifacts makes a worker's artifact claim durable before
// accepting its terminal result. Previously a model could declare a missing
// path, receive a successful submit_result response, and leave the evidence
// manifest to fail only at run finalization. Artifacts are workspace-contained
// evidence, so snapshot them here or reject the result while the worker can
// still correct its claim.
func (t *submitResultTool) materializeSubmittedArtifacts(ctx context.Context, res *TaskResult) error {
	if res == nil || len(res.Artifacts) == 0 {
		return nil
	}
	if t.coordinator == nil || t.coordinator.session == nil || strings.TrimSpace(t.coordinator.session.Workspace) == "" {
		return fmt.Errorf("artifact submission requires a workspace")
	}
	workspace := t.coordinator.session.Workspace
	store, err := NewFileArtifactStore(workspace, workspace)
	if err != nil {
		return err
	}
	runID := t.coordinator.executionRunID
	if runID == "" {
		runID = "run-unknown"
	}
	// Validate the complete declaration before snapshotting any files, so a bad
	// later artifact cannot leave an accepted result with a partial artifact set.
	for i, artifact := range res.Artifacts {
		if artifact.ID != "" {
			if err := store.Verify(ctx, artifact); err != nil {
				return fmt.Errorf("artifacts[%d] %q: %w", i, artifact.Path, err)
			}
			continue
		}
		if strings.TrimSpace(artifact.Path) == "" {
			return fmt.Errorf("artifacts[%d] path is required", i)
		}
		path, err := resolveArtifactSourcePath(workspace, artifact.Path)
		if err != nil {
			return fmt.Errorf("artifacts[%d] %q: %w", i, artifact.Path, err)
		}
		info, err := os.Stat(path)
		if err != nil {
			return fmt.Errorf("artifacts[%d] %q: %w", i, artifact.Path, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("artifacts[%d] %q must be a regular file", i, artifact.Path)
		}
	}
	for i, artifact := range res.Artifacts {
		if artifact.ID != "" {
			continue
		}
		ref, err := store.Put(ctx, PutArtifactRequest{
			Kind:        artifact.Kind,
			Path:        artifact.Path,
			Description: artifact.Description,
			MediaType:   artifact.MediaType,
			SourcePath:  artifact.Path,
			RunID:       runID,
			TaskID:      t.todoID,
			Attempt:     1,
			Agent:       res.Agent,
		})
		if err != nil {
			return fmt.Errorf("artifacts[%d] %q: %w", i, artifact.Path, err)
		}
		res.Artifacts[i] = ref
	}
	return nil
}
